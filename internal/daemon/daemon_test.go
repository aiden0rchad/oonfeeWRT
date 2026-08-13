package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testConfig builds a daemon config with a passphrase file, which is the only
// unattended path — the interactive one needs a terminal.
func testConfig(t *testing.T, passphrase string) Config {
	t.Helper()
	dir := t.TempDir()
	pf := filepath.Join(dir, "passphrase")
	if err := os.WriteFile(pf, []byte(passphrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = "127.0.0.1:0"
	cfg.PassphraseFile = pf
	cfg.ShutdownGrace = 2 * time.Second
	cfg.ApplyDrain = 2 * time.Second
	return cfg
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		EnvDataDir:        "/srv/oonfee",
		EnvListen:         "127.0.0.1:9999",
		EnvPassphraseFile: "/run/secrets/pass",
	}
	cfg := DefaultConfig()
	if err := cfg.load(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DataDir != "/srv/oonfee" || cfg.Listen != "127.0.0.1:9999" ||
		cfg.PassphraseFile != "/run/secrets/pass" {
		t.Fatalf("environment not applied: %+v", cfg)
	}
	if got, want := cfg.DBPath(), filepath.Join("/srv/oonfee", DBFileName); got != want {
		t.Fatalf("DBPath = %q, want %q", got, want)
	}
}

func TestConfigDefaultsWhenEnvIsEmpty(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.load(func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DataDir != DefaultDataDir || cfg.Listen != DefaultListen {
		t.Fatalf("defaults not preserved: %+v", cfg)
	}
	if cfg.PassphraseFile != "" {
		t.Fatalf("PassphraseFile defaulted to %q; it must stay unset so the "+
			"interactive prompt is the default", cfg.PassphraseFile)
	}
}

// A passphrase in the environment is readable from /proc and shows up in
// `docker inspect`. Refusing it loudly is the whole point.
func TestConfigRejectsPassphraseInEnvironment(t *testing.T) {
	cfg := DefaultConfig()
	err := cfg.load(func(k string) (string, bool) {
		if k == EnvPassphraseRejected {
			return "hunter2", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("a passphrase in the environment was accepted")
	}
	if !strings.Contains(err.Error(), EnvPassphraseFile) {
		t.Errorf("the rejection should point at the supported alternative: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection echoed the secret it was rejecting: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"empty data dir", func(c *Config) { c.DataDir = "" }},
		{"relative data dir", func(c *Config) { c.DataDir = "data" }},
		{"empty listen", func(c *Config) { c.Listen = "" }},
		{"zero shutdown grace", func(c *Config) { c.ShutdownGrace = 0 }},
		{"zero apply drain", func(c *Config) { c.ApplyDrain = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
}

func TestOpenServeHealthzShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "pass"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()

	resp, err := waitForHealthz(d.Addr())
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("healthz body = %q, want %q", body, "ok")
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

func waitForHealthz(addr string) (*http.Response, error) {
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}

// The keyring is created on first start and opened on the next, and a wrong
// passphrase must fail rather than quietly initialise a second keyring beside
// the credentials it cannot read.
func TestKeyringLifecycleAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, "pass")

	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	blob, err := d.Keys.SealCredential("aa:bb:cc:dd:ee:ff", "oonfeewrt", "device-pw")
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(secrets.DefaultPath(cfg.DataDir)); err != nil {
		t.Fatalf("keyring was not created: %v", err)
	}

	d2, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	user, pass, err := d2.Keys.OpenCredential("aa:bb:cc:dd:ee:ff", blob)
	if err != nil {
		t.Fatalf("credential from the previous run does not open: %v", err)
	}
	if user != "oonfeewrt" || pass != "device-pw" {
		t.Fatalf("got %q/%q", user, pass)
	}
	d2.Close()

	wrong := cfg
	wrong.PassphraseFile = filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(wrong.PassphraseFile, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	d3, err := Open(ctx, wrong, quietLogger())
	if err == nil {
		d3.Close()
		t.Fatal("Open accepted the wrong passphrase")
	}
	if !errors.Is(err, secrets.ErrBadPassphrase) {
		t.Fatalf("got %v, want ErrBadPassphrase", err)
	}
}

// Open binds the listener before prompting for or reading the passphrase, so a
// port clash is reported without first asking an operator to type a secret.
func TestOpenReportsPortClashBeforeTouchingTheKeyring(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, "pass")

	first, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer first.Close()

	clash := cfg
	clash.Listen = first.Addr()
	clash.DataDir = filepath.Join(t.TempDir(), "other")
	d, err := Open(ctx, clash, quietLogger())
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded on an address already in use")
	}
	if _, statErr := os.Stat(secrets.DefaultPath(clash.DataDir)); statErr == nil {
		t.Fatal("a keyring was created despite the daemon failing to start")
	}
}

func TestNoPassphraseSourceIsRefused(t *testing.T) {
	cfg := testConfig(t, "pass")
	cfg.PassphraseFile = ""

	// os.Stdin under `go test` is not a terminal, which is exactly the
	// unattended case this must refuse.
	if _, err := passphraseFor(cfg, true, os.Stdin, io.Discard); !errors.Is(err, ErrNoPassphraseSource) {
		t.Fatalf("got %v, want ErrNoPassphraseSource", err)
	}
}

func TestConnectRefusesAnUnadoptedDevice(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "pass"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	dev := &store.Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.0.2.1", Name: "ap1"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if _, err := d.Connect(ctx, dev); err == nil {
		t.Fatal("Connect used a device that was never adopted")
	} else if !strings.Contains(err.Error(), "not adopted") {
		t.Fatalf("unhelpful error for an un-adopted device: %v", err)
	}
}

func TestHostPort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dev   store.Device
		https bool
		want  string
	}{
		{"default http port omitted", store.Device{Host: "192.168.1.1", Port: 80}, false, "192.168.1.1"},
		{"default https port omitted", store.Device{Host: "192.168.1.1", Port: 443}, true, "192.168.1.1"},
		{"non-default kept", store.Device{Host: "192.168.1.1", Port: 8080}, false, "192.168.1.1:8080"},
		{"zero treated as default", store.Device{Host: "192.168.1.1"}, false, "192.168.1.1"},
		{"https on 80 is not default", store.Device{Host: "192.168.1.1", Port: 80}, true, "192.168.1.1:80"},
		{"ipv6 bracketed", store.Device{Host: "fd00::1", Port: 8080}, false, "[fd00::1]:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostPort(&tc.dev, tc.https); got != tc.want {
				t.Fatalf("hostPort = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- apply barrier ----

func TestApplyBarrierWaitsAndTimesOut(t *testing.T) {
	var b applyBarrier
	if !b.wait(time.Millisecond) {
		t.Fatal("wait on an idle barrier did not return immediately")
	}

	end := b.begin()
	if got := b.inFlight(); got != 1 {
		t.Fatalf("inFlight = %d, want 1", got)
	}
	if b.wait(20 * time.Millisecond) {
		t.Fatal("wait reported idle while an apply was running")
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		end()
		end() // idempotent: deferred and called on an early return
	}()
	if !b.wait(5 * time.Second) {
		t.Fatal("wait did not observe the apply finishing")
	}
	if got := b.inFlight(); got != 0 {
		t.Fatalf("inFlight = %d after completion, want 0", got)
	}
}

func TestApplyBarrierConcurrent(t *testing.T) {
	var b applyBarrier
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		end := b.begin()
		go func() {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			end()
		}()
	}
	if !b.wait(5 * time.Second) {
		t.Fatal("wait timed out with 16 short applies")
	}
	wg.Wait()
	if got := b.inFlight(); got != 0 {
		t.Fatalf("inFlight = %d, want 0", got)
	}
}

// An apply must not be cancelled because the operator closed their browser tab
// or the daemon got SIGTERM: the rollback timer on the device keeps running
// either way, and cancelling only removes the party that could confirm.
func TestTrackApplyDetachesFromCallerCancellation(t *testing.T) {
	d := &Daemon{Config: DefaultConfig()}
	ctx, cancel := context.WithCancel(context.Background())
	type key struct{}
	ctx = context.WithValue(ctx, key{}, "carried")
	cancel()

	err := d.TrackApply(ctx, func(actx context.Context) error {
		if actx.Err() != nil {
			t.Errorf("apply context is already cancelled: %v", actx.Err())
		}
		if _, ok := actx.Deadline(); !ok {
			t.Error("apply context has no deadline; a wedged apply would hold shutdown open")
		}
		if v, _ := actx.Value(key{}).(string); v != "carried" {
			t.Error("apply context lost the caller's values")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("TrackApply: %v", err)
	}
}

// Shutdown must wait for applies rather than exiting while a device still has a
// rollback armed.
func TestShutdownWaitsForInFlightApply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := testConfig(t, "pass")
	cfg.ApplyDrain = 5 * time.Second
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	if _, err := waitForHealthz(d.Addr()); err != nil {
		t.Fatalf("healthz: %v", err)
	}

	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		_ = d.TrackApply(context.Background(), func(context.Context) error {
			close(finished)
			<-release
			return nil
		})
	}()
	<-finished // the apply is registered

	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned (%v) while an apply was still running", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the apply finished")
	}
}

// If an apply will not finish, shutdown proceeds but says so — and the message
// has to be actionable, because the device may revert a change nobody confirmed.
func TestShutdownReportsAnApplyThatOutlastsTheBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := testConfig(t, "pass")
	cfg.ApplyDrain = 200 * time.Millisecond
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	if _, err := waitForHealthz(d.Addr()); err != nil {
		t.Fatalf("healthz: %v", err)
	}

	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	go func() {
		_ = d.TrackApply(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	cancel()
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("shutdown reported success while an apply was abandoned")
		}
		if !strings.Contains(err.Error(), "revert") {
			t.Fatalf("the error does not say what is at risk: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never returned")
	}
}
