package applyengine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Runs against tools/mock_ubus.py, which reproduces the measured device
// semantics: per-session tokens, session-bound confirm, and a rollback that
// restores committed state while leaving the applying session's delta in place.

var mockAddr string

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mockAddr = fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("python3", filepath.Join(root, "tools", "mock_ubus.py"),
		"--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := waitReady(mockAddr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "mock not ready:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	os.Exit(code)
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("go.mod not found")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func dial(t *testing.T) *ubus.Client {
	t.Helper()
	c := ubus.New(ubus.Options{Host: mockAddr})
	if err := c.Login(context.Background(), "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// seed puts a known committed baseline in the scratch config.
func seed(t *testing.T, c *ubus.Client, config, marker string) {
	t.Helper()
	ctx := context.Background()
	_ = c.Call(ctx, "uci", "add", map[string]any{
		"config": config, "type": "probe", "name": "probe",
		"values": map[string]string{"marker": marker}}, nil)
	if err := c.Call(ctx, "uci", "commit", map[string]any{"config": config}, nil); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

func fastEngine() *Engine {
	e := New()
	e.ConfirmInterval = 200 * time.Millisecond
	e.RevertGrace = 1500 * time.Millisecond
	return e
}

func plan(config, marker string) Plan {
	return Plan{
		Timeout: 2 * time.Second,
		Ops: []Op{{
			Kind: OpSet, Config: config, Section: "probe",
			Values: map[string]string{"marker": marker},
		}},
	}
}

// Happy path: health passes, confirm lands, change is permanent.
func TestApplyConfirmsWhenHealthy(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE")

	res, err := fastEngine().Apply(ctx, c, plan("oonfeewrt_probe", "GOOD"),
		func(context.Context, *ubus.Client) error { return nil })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != Applied {
		t.Fatalf("want Applied, got %s", res)
	}
	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	defer fresh.Destroy(ctx)
	if got := marker(t, fresh, "oonfeewrt_probe"); got != "GOOD" {
		t.Fatalf("confirmed change should persist, got %q", got)
	}
}

// The ordering that makes failure cheap: when health fails we simply never
// confirm, and the device restores itself. No reversal, no extra apply.
func TestHealthFailureCostsNothingAndReverts(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE2")

	boom := errors.New("ssid missing from the air")
	res, err := fastEngine().Apply(ctx, c, plan("oonfeewrt_probe", "DOOMED"),
		func(context.Context, *ubus.Client) error { return boom })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != Reverted {
		t.Fatalf("want Reverted, got %s", res)
	}
	if !errors.Is(res.HealthErr, boom) {
		t.Fatalf("health error should be reported, got %v", res.HealthErr)
	}
	if res.Stranded {
		t.Error("a clean revert is not stranded")
	}
	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	defer fresh.Destroy(ctx)
	if got := marker(t, fresh, "oonfeewrt_probe"); got != "BASE2" {
		t.Fatalf("device should have reverted to BASE2, got %q", got)
	}
}

// Inside the confirmation window an independent session does not exist: rpcd
// hands every new login the applying token. Health therefore runs on the
// applying session by design, which is why its contract forbids uci.get.
func TestHealthRunsOnTheApplyingSessionInsideTheWindow(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE3")

	var seen string
	var sharedDuringWindow bool
	_, err := fastEngine().Apply(ctx, c, plan("oonfeewrt_probe", "X"),
		func(hctx context.Context, verify *ubus.Client) error {
			seen = verify.Session()
			// And prove the reason: asking for a fresh one yields the same
			// token, flagged shared so Destroy cannot cut it.
			other, err := verify.FreshSession(hctx)
			if err != nil {
				return err
			}
			sharedDuringWindow = other.Shared() && other.Session() == verify.Session()
			other.Destroy(hctx) // must be a no-op on a shared session
			return nil
		})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if seen == "" {
		t.Fatal("health check never ran")
	}
	if seen != c.Session() {
		t.Fatalf("health should receive the applying session (no other exists "+
			"inside the window); got %s want %s", seen, c.Session())
	}
	if !sharedDuringWindow {
		t.Fatal("FreshSession inside an armed window must return the applying " +
			"token, marked Shared()")
	}
}

// The bug this cost us once: destroying the session obtained inside the window
// destroys the APPLYING session, so confirm then fails and the change reverts.
// Destroy must refuse on a shared session.
func TestDestroyingASharedSessionDoesNotBreakConfirm(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE6")

	res, err := fastEngine().Apply(ctx, c, plan("oonfeewrt_probe", "SURVIVES"),
		func(hctx context.Context, verify *ubus.Client) error {
			other, err := verify.FreshSession(hctx)
			if err != nil {
				return err
			}
			other.Destroy(hctx) // would previously have killed the applier
			return nil
		})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != Applied {
		t.Fatalf("confirm should still have landed; got %s", res)
	}
}

// PREFLIGHT must refuse when someone is mid-edit in LuCI or over SSH.
func TestPreflightRefusesForeignDirtyConfigs(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE4")

	// The mock exposes /tmp/.uci through file.list; make one config dirty.
	if err := c.Call(ctx, "__test", "set_dirty",
		map[string]any{"configs": []string{"wireless"}}, nil); err != nil {
		t.Skipf("mock does not support dirty simulation: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Call(ctx, "__test", "set_dirty", map[string]any{"configs": []string{}}, nil)
	})

	_, err := fastEngine().Apply(ctx, c, plan("oonfeewrt_probe", "NOPE"), nil)
	var de *DirtyError
	if !errors.As(err, &de) {
		t.Fatalf("want *DirtyError, got %T: %v", err, err)
	}
	if len(de.Configs) == 0 || de.Configs[0] != "wireless" {
		t.Fatalf("should name the dirty config, got %v", de.Configs)
	}
}

// A network change can move the address we manage the device through, so it
// needs an explicit acknowledgement.
func TestPreflightRequiresTraversalAck(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	p := Plan{Timeout: 2 * time.Second, Ops: []Op{{
		Kind: OpSet, Config: "network", Section: "lan",
		Values: map[string]string{"ipaddr": "10.0.0.1"}}}}

	if _, err := fastEngine().Apply(ctx, c, p, nil); err == nil {
		t.Fatal("a network change without AcknowledgeTraversal must be refused")
	}
}

func TestEmptyPlanRejected(t *testing.T) {
	c := dial(t)
	if _, err := fastEngine().Apply(context.Background(), c, Plan{}, nil); err == nil {
		t.Fatal("an empty plan must be rejected")
	}
}

// Every section we write carries the ownership tag, so the reconciler can tell
// its own sections from a human's.
func TestStagedSectionsCarryOwnershipTag(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE5")

	if _, err := fastEngine().Apply(ctx, c, plan("oonfeewrt_probe", "TAGGED"),
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	defer fresh.Destroy(ctx)

	var out struct {
		Values map[string]string `json:"values"`
	}
	if err := fresh.Call(ctx, "uci", "get", map[string]any{
		"config": "oonfeewrt_probe", "section": "probe"}, &out); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if out.Values[OwnershipTag] != "1" {
		t.Fatalf("written section must carry %s=1, got %v", OwnershipTag, out.Values)
	}
}

func marker(t *testing.T, c *ubus.Client, config string) string {
	t.Helper()
	var out struct {
		Value string `json:"value"`
	}
	if err := c.Call(context.Background(), "uci", "get", map[string]any{
		"config": config, "section": "probe", "option": "marker"}, &out); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return out.Value
}
