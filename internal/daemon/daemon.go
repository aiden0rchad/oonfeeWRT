// Package daemon is where the Phase 0 packages meet: it opens the store and the
// controller secret keyring, serves HTTP, and shuts down without abandoning a device
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

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
)

// driverName is the SQLite driver registered by the blank import above. store
// takes it as an argument so it does not force a driver on its dependents.
const driverName = "sqlite"

type clientPresenceCache struct {
	sources  map[string]collector.ClientPresence
	lastSeen collector.ClientPresence
}

// Daemon is a running controller.
type Daemon struct {
	Config Config
	Log    *slog.Logger
	Store  *store.DB
	Keys   *secrets.Keeper

	// resolver is consulted once at the start of inspect/adoption. Keeping the
	// dependency here makes the workflow's address pin testable without changing
	// global DNS state.
	resolver hostResolver

	// adoptMu serialises the pre-touch inventory checks with the adoption
	// transaction. Without it two simultaneous gateway requests can both see
	// an empty slot, bootstrap two routers, and only discover the conflict after
	// credentials and ACLs have been installed.
	adoptMu sync.Mutex

	// nbrMu guards lastNeighbourRun: the most recent 802.11k cycle, kept so the
	// screen can report what happened without making it happen.
	nbrMu            sync.Mutex
	lastNeighbourRun *neighbourRun

	// applies tracks in-flight applies so shutdown can wait for them.
	applies applyBarrier
	// deviceOps prevents an apply and un-adopt from mutating the same router and
	// ownership ledger at once. It is keyed per device, so unrelated routers do
	// not block each other.
	deviceOps deviceOperationGate

	// reprobes serialises capability probes per device and rate-limits the
	// automatic ones. A probe is a burst of calls against a budget that allows
	// one per minute, so two at once is worse than twice as expensive: they
	// interleave on one rpcd.
	reprobes reprobeGate

	// Samples is the in-RAM telemetry ring. It exists from Open so that a
	// device polled before the maintainer starts is still recorded.
	Samples *telemetry.Store
	// telemetryLifecycle keeps a flush's drain+database write atomic with
	// device deletion+sample purge. Device IDs are reusable, so neither
	// operation may cross the other's boundary.
	telemetryLifecycle sync.Mutex

	// api is built in routes(), which Open calls; it is kept so the maintenance
	// tick can sweep expired sessions.
	api *api.Server

	mu        sync.Mutex
	collector *collector.Collector
	// lastClients is the newest associated-station count per device. A nil
	// value means the device was asked and could not answer, which is not the
	// same as having no entry.
	lastClients map[int64]*int
	// lastStations is which clients the last poll saw associated, per device,
	// keyed by lower-case MAC. A nil entry is "asked and could not find out",
	// distinct from having no entry at all.
	lastStations map[int64]collector.LiveStationSet
	// lastPresence keeps authoritative client reachability timestamps. Host
	// hints and DHCP leases are deliberately absent: they are inventory and can
	// remain after a client leaves.
	lastPresence map[int64]*clientPresenceCache

	// sinkMu guards the poll sink's identity-keyed carry-forward state. These
	// values are deliberately process-local, but device IDs are reusable, so
	// deletion must invalidate them at the same boundary as the inventory row.
	sinkMu         sync.Mutex
	sinkUp         map[int64]bool
	sinkKnown      map[int64]bool
	sinkFirmware   map[int64]string
	logIngest      *logIngestor
	topologyIngest *topologyIngestor
	cascadeIngest  *cascadeGrouper

	// meshes retains the poll-derived half of every backhaul reading. In RAM
	// because it describes right now: a stale interface list read back from
	// disk would answer "is this up" with something that was once true.
	meshes meshStore

	maint     *telemetry.Maintainer
	maintDone chan struct{}
	maintStop context.CancelFunc

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
	// A drain shorter than an apply can possibly take is a config that turns
	// every full-window apply into the engine's one alarming outcome. Warned
	// rather than refused: a deliberately tiny drain is how the shutdown path
	// itself gets tested, and refusing it would break testing the thing this
	// warning is about.
	if min := applyengine.MinApplyBudget(); cfg.ApplyDrain < min {
		log.Warn("apply drain is shorter than an apply can take; applies that "+
			"need the full rollback window will report \"unknown\" with the "+
			"change still on the device",
			"apply_drain", cfg.ApplyDrain, "minimum", min)
	}
	// 0700: the directory holds the database and the keyring, and nothing else
	// on the host has any business reading either.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create data directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode unchanged. A data directory
	// commonly starts life as a normal 0755 folder created by an operator or
	// service manager; accepting that mode exposes database backups and the
	// keyring filename even though newly created directories are private.
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: secure data directory: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", cfg.Listen, err)
	}

	d := &Daemon{Config: cfg, Log: log, ln: ln, resolver: net.DefaultResolver,
		Samples: telemetry.New(telemetry.Options{})}
	if err := d.openKeyring(cfg); err != nil {
		ln.Close()
		return nil, err
	}
	db, err := store.Open(ctx, driverName, cfg.DBPath(), d.Keys)
	if err != nil {
		d.Keys.Close()
		ln.Close()
		return nil, err
	}
	// Recovery is daemon lifecycle policy, not a side effect of opening the
	// database. Read-only diagnostic tools also open this store; letting one of
	// them mark a live Apply interrupted would turn observation into mutation.
	if err := db.RecoverApplyOperations(ctx, time.Now().Unix()); err != nil {
		db.Close()
		d.Keys.Close()
		ln.Close()
		return nil, fmt.Errorf("daemon: recover apply operations: %w", err)
	}
	if _, err := db.RecoverRadioScans(ctx, time.Now().UnixMilli()); err != nil {
		db.Close()
		d.Keys.Close()
		ln.Close()
		return nil, fmt.Errorf("daemon: recover radio scans: %w", err)
	}
	// SQLite creates its database, WAL, and shared-memory files using the process
	// umask. The 0700 directory is the hard boundary while they are created; make
	// each file private too so copied backups and diagnostics retain safe modes.
	for i, path := range []string{cfg.DBPath(), cfg.DBPath() + "-wal", cfg.DBPath() + "-shm"} {
		err := os.Chmod(path, 0o600)
		if err == nil || i > 0 && errors.Is(err, os.ErrNotExist) {
			continue
		}
		db.Close()
		d.Keys.Close()
		ln.Close()
		return nil, fmt.Errorf("daemon: secure database file %s: %w", path, err)
	}
	d.Store = db

	d.http = &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
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
		if info, dbErr := os.Stat(cfg.DBPath()); dbErr == nil && info.Size() > 0 {
			return fmt.Errorf("daemon: keyring %s is missing but database %s already exists; restore the matching keyring backup or move the database aside before starting a new controller",
				path, cfg.DBPath())
		} else if dbErr != nil && !errors.Is(dbErr, os.ErrNotExist) {
			return fmt.Errorf("daemon: stat database before keyring creation: %w", dbErr)
		}
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
		return unlockHint(cfg, path, err)
	}
	d.Keys = keeper
	if created {
		d.Log.Warn("created a new controller secret keyring; if this system already had "+
			"a controller database, the data directory is not the one you think it is",
			"path", path)
	} else {
		d.Log.Info("controller secret keyring opened", "path", path)
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

	d.mu.Lock()
	apiSrv := d.api
	d.mu.Unlock()
	// Close admission before asking net/http to drain. Shutdown can time out and
	// Close can cancel a handler queued on the site lock; neither is enough on
	// its own because an Apply deliberately detaches from request cancellation.
	if apiSrv != nil {
		apiSrv.CloseAdmission()
	}

	// 1. Stop taking work.
	hctx, cancel := context.WithTimeout(context.Background(), d.Config.ShutdownGrace)
	defer cancel()
	if err := d.http.Shutdown(hctx); err != nil {
		errs = append(errs, fmt.Errorf("daemon: HTTP did not drain: %w", err))
		d.http.Close()
	}

	// 2. Drop live clients, then stop polling. This order matters: closing a
	//    connection releases the focus it holds, and releasing focus after the
	//    collector has stopped would touch a poller that is already gone.
	if apiSrv != nil && apiSrv.Hub != nil {
		apiSrv.Hub.Close()
	}
	if c := d.collectorRef(); c != nil {
		c.Stop()
	}
	if err := d.stopCascadeEvents(hctx); err != nil {
		errs = append(errs, fmt.Errorf("daemon: flush grouped topology events: %w", err))
	}

	// 3. Flush telemetry. After the collector has stopped, so nothing is still
	//    arriving, and before the database closes, so there is somewhere to put
	//    it. Skipping this loses up to five minutes of every series on every
	//    restart — a visible notch in every graph of a fleet that gets updated.
	d.stopMaintainer()

	// 4. Wait for in-flight applies. An apply past APPLY has a rollback armed on
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

	// Track the complete accepted API handler, not only its router state machine.
	// The final receipt is written after TrackApply returns; closing SQLite in
	// that gap loses the only durable answer to an ambiguous POST. Queued site
	// mutations have already been woken by CloseAdmission and return no-write.
	apiDrained := true
	if apiSrv != nil {
		if n := apiSrv.ActiveRequests(); n > 0 {
			d.Log.Warn("waiting for accepted API requests before closing the database",
				"count", n, "budget", d.Config.ApplyDrain)
			if !apiSrv.WaitForDrain(d.Config.ApplyDrain) {
				apiDrained = false
				errs = append(errs, fmt.Errorf("daemon: %d accepted API request(s) still running after %s; database left open rather than closing underneath them",
					apiSrv.ActiveRequests(), d.Config.ApplyDrain))
			}
		}
	}

	// 5. Checkpoint and close the database, then zero the keys. Keys last: a
	//    checkpoint that needed to read a credential would otherwise fail on a
	//    closed keeper.
	if apiDrained && d.Store != nil {
		if err := d.Store.Checkpoint(context.Background()); err != nil {
			errs = append(errs, err)
		}
		if err := d.Store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if apiDrained && d.Keys != nil {
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
	cascadeCtx, cancelCascade := context.WithTimeout(context.Background(), 5*time.Second)
	if err := d.stopCascadeEvents(cascadeCtx); err != nil {
		errs = append(errs, fmt.Errorf("daemon: flush grouped topology events: %w", err))
	}
	cancelCascade()
	d.stopMaintainer()
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
