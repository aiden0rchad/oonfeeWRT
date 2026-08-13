// Package daemon is where the Phase 0 packages meet: it opens the store and the
// credential keyring, serves HTTP, and shuts down without abandoning a device
// mid-apply.
//
// There is very little logic here on purpose. Everything this package does that
// is interesting is an ordering constraint — what must be open before what, and
// what must finish before the process exits — and ordering constraints are
// easier to get right when they are all in one file.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver, per decision D3

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// driverName is the SQLite driver registered by the blank import above. store
// takes it as an argument so it does not force a driver on its dependents.
const driverName = "sqlite"

// Daemon is a running controller.
type Daemon struct {
	Config Config
	Log    *slog.Logger
	Store  *store.DB
	Keys   *secrets.Keeper

	// applies tracks in-flight applies so shutdown can wait for them.
	applies applyBarrier

	mu        sync.Mutex
	collector *collector.Collector

	http *http.Server
	ln   net.Listener
}

// Open brings up everything the daemon owns, in dependency order, and returns a
// Daemon that is ready to Serve.
//
// The listener is bound here rather than in Serve so that "the port is already
// in use" is reported before the keyring is opened — failing after prompting an
// operator for a passphrase is a small rudeness that is entirely avoidable.
func Open(ctx context.Context, cfg Config, log *slog.Logger) (*Daemon, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	// 0700: the directory holds the database and the keyring, and nothing else
	// on the host has any business reading either.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create data directory: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", cfg.Listen, err)
	}

	d := &Daemon{Config: cfg, Log: log, ln: ln}
	if err := d.openKeyring(cfg); err != nil {
		ln.Close()
		return nil, err
	}
	db, err := store.Open(ctx, driverName, cfg.DBPath())
	if err != nil {
		d.Keys.Close()
		ln.Close()
		return nil, err
	}
	d.Store = db

	// SQLite creates the database 0644. Tighten it: the database holds more than
	// sealed credentials — WLAN pre-shared keys live in the site model in the
	// clear, because they have to be pushed to devices as plaintext UCI.
	//
	// The 0700 data directory is the real boundary (nobody else can traverse into
	// it), and this does not reliably cover the -wal and -shm files, which SQLite
	// creates later. It is worth doing anyway for the case that actually happens:
	// someone copies the database file out for a backup.
	if err := os.Chmod(cfg.DBPath(), 0o600); err != nil {
		log.Warn("could not tighten permissions on the database file",
			"path", cfg.DBPath(), "err", err)
	}

	d.http = &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return d, nil
}

// openKeyring acquires the passphrase and opens (or creates) the keyring.
//
// Whether this is a first run is decided by the file's absence and reported to
// the operator, because "created a new keyring" and "opened the existing one"
// look identical from the outside and mean opposite things: the first, on a
// system that already had devices, means the data directory is not the one they
// think it is.
func (d *Daemon) openKeyring(cfg Config) error {
	path := secrets.DefaultPath(cfg.DataDir)
	firstRun := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		firstRun = true
	} else if err != nil {
		return fmt.Errorf("daemon: stat keyring: %w", err)
	}

	pass, err := passphraseFor(cfg, firstRun, os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	defer clear(pass)

	keeper, created, err := secrets.OpenOrCreate(path, pass, secrets.DefaultParams())
	if err != nil {
		return err
	}
	d.Keys = keeper
	if created {
		d.Log.Warn("created a new credential keyring; if this system already had "+
			"adopted devices, the data directory is not the one you think it is",
			"path", path)
	} else {
		d.Log.Info("credential keyring opened", "path", path)
	}
	return nil
}

// Addr reports the bound address, which matters when Listen was ":0".
func (d *Daemon) Addr() string {
	if d.ln == nil {
		return ""
	}
	return d.ln.Addr().String()
}

// Serve runs until ctx is cancelled, then shuts down and returns.
//
// A nil return means a clean shutdown. An error means either the server failed
// or shutdown did not complete within its budget — and in the latter case the
// error names what was still running, because "shutdown timed out" on its own
// tells an operator nothing about whether a device was left mid-apply.
func (d *Daemon) Serve(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		d.Log.Info("listening", "addr", d.Addr())
		err := d.http.Serve(d.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		// The server stopped on its own; still run shutdown so the database is
		// checkpointed and the key material is zeroed.
		return errors.Join(err, d.shutdown())
	case <-ctx.Done():
		d.Log.Info("shutting down")
		err := d.shutdown()
		<-errc // Serve has returned by now; drain so the goroutine does not leak.
		return err
	}
}

// shutdown implements the §11 contract, in the order the contract requires.
func (d *Daemon) shutdown() error {
	var errs []error

	// 1. Stop taking work. Draining HTTP first means no new apply can start
	//    while we are waiting for the ones already running.
	hctx, cancel := context.WithTimeout(context.Background(), d.Config.ShutdownGrace)
	defer cancel()
	if err := d.http.Shutdown(hctx); err != nil {
		errs = append(errs, fmt.Errorf("daemon: HTTP did not drain: %w", err))
		d.http.Close()
	}

	// 2. Stop polling. Devices are left alone from here: an apply in flight is
	//    the only thing that should still be talking to one.
	if c := d.collectorRef(); c != nil {
		c.Stop()
	}

	// 3. Wait for in-flight applies. An apply past APPLY has a rollback armed on
	//    the device; that timer runs whether this process exists or not, so
	//    exiting here would leave a healthy change to revert with nobody left to
	//    confirm it. This is the one shutdown step allowed to take minutes.
	if n := d.applies.inFlight(); n > 0 {
		d.Log.Warn("waiting for in-flight applies before exit; a device has a "+
			"rollback timer armed", "count", n, "budget", d.Config.ApplyDrain)
		if !d.applies.wait(d.Config.ApplyDrain) {
			errs = append(errs, fmt.Errorf("daemon: %d apply(s) still running after "+
				"%s — a device may revert an unconfirmed change; check its config "+
				"before assuming the change landed",
				d.applies.inFlight(), d.Config.ApplyDrain))
		}
	}

	// 4. Checkpoint and close the database, then zero the keys. Keys last: a
	//    checkpoint that needed to read a credential would otherwise fail on a
	//    closed keeper.
	if d.Store != nil {
		if err := d.Store.Checkpoint(context.Background()); err != nil {
			errs = append(errs, err)
		}
		if err := d.Store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if d.Keys != nil {
		errs = append(errs, d.Keys.Close())
	}
	return errors.Join(errs...)
}

// Close releases everything without the graceful sequence. It exists for the
// error paths in Open and for tests; Serve's own shutdown is the real one.
func (d *Daemon) Close() error {
	var errs []error
	if c := d.collectorRef(); c != nil {
		c.Stop()
	}
	if d.http != nil {
		errs = append(errs, d.http.Close())
	}
	// http.Server only tracks listeners once Serve is running, so closing the
	// server is not enough for a Daemon that was opened and never served — which
	// is every failed startup. Close it directly; already-closed is not news.
	if d.ln != nil {
		if err := d.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if d.Store != nil {
		errs = append(errs, d.Store.Close())
	}
	if d.Keys != nil {
		errs = append(errs, d.Keys.Close())
	}
	return errors.Join(errs...)
}
