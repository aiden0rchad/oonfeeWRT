package applyengine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestOptionPatchLeavesExistingSectionUnowned(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE_PATCH")

	p := Plan{Timeout: 2 * time.Second, Ops: []Op{{
		Kind: OpSet, Config: "oonfeewrt_probe", Section: "probe",
		Values: map[string]string{"marker": "PATCHED"}, Patch: true,
	}}}
	if _, err := fastEngine().Apply(ctx, c, p,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply patch: %v", err)
	}
	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Destroy(ctx)
	var out struct {
		Values map[string]string `json:"values"`
	}
	if err := fresh.Call(ctx, "uci", "get", map[string]any{
		"config": "oonfeewrt_probe", "section": "probe"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Values["marker"] != "PATCHED" || out.Values[OwnershipTag] != "" {
		t.Fatalf("option patch changed ownership: %v", out.Values)
	}
}

func TestInvalidPatchShapesAreRejectedBeforeApply(t *testing.T) {
	tests := []Op{
		{Kind: OpAdd, Config: "oonfeewrt_probe", Type: "probe", Name: "probe", Patch: true},
		{Kind: OpDelete, Config: "oonfeewrt_probe", Section: "probe", Option: "marker", Patch: true},
		{Kind: OpSet, Config: "oonfeewrt_probe", Section: "probe", Values: map[string]string{OwnershipTag: "1"}, Patch: true},
		{Kind: OpSet, Config: "oonfeewrt_probe", Section: "probe", Lists: map[string][]string{"value": {"x"}}, Patch: true},
	}
	for i, op := range tests {
		c := dial(t)
		if _, err := fastEngine().Apply(context.Background(), c,
			Plan{Timeout: 2 * time.Second, Ops: []Op{op}}, nil); err == nil {
			t.Errorf("case %d: invalid patch was accepted", i)
		}
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

// A clean revert of a section we ALREADY OWN must report Reverted.
//
// This is the shape render actually produces and the existing tests never did:
// an OpSet carrying the WHOLE section, most of whose keys are unchanged by the
// apply — the ownership tag above all, which a section we already own still
// carries after a perfect revert.
//
// planStillApplied returned "still applied" on the first key that read back
// equal, so it matched on the tag and reported Unknown + Stranded: the audit
// event went out at error severity telling an operator to hand-reverse a device
// whose config was already correct. Every apply after the first to an owned
// section hit it — a re-key, an SSID rename, a channel change, a mesh edit.
//
// The cost is not the noise. It is that the engine's one alarming signal stops
// meaning anything, so a genuinely stranded change looks like all the others.
func TestARevertOfAnOwnedSectionIsNotStranded(t *testing.T) {
	ctx := context.Background()
	c := dial(t)

	// Seed a section that already carries the ownership tag, as one written by
	// a previous confirmed apply would.
	_ = c.Call(ctx, "uci", "add", map[string]any{
		"config": "oonfeewrt_probe2", "type": "probe", "name": "probe",
		"values": map[string]string{"marker": "BASE", OwnershipTag: "1"}}, nil)
	if err := c.Call(ctx, "uci", "commit",
		map[string]any{"config": "oonfeewrt_probe2"}, nil); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	boom := errors.New("ssid missing from the air")
	res, err := fastEngine().Apply(ctx, c, Plan{
		Timeout: 2 * time.Second,
		Ops: []Op{{
			Kind: OpSet, Config: "oonfeewrt_probe2", Section: "probe",
			// The whole section, exactly as render emits it: the changed key
			// AND the tag that cannot change.
			Values: map[string]string{"marker": "DOOMED", OwnershipTag: "1"},
		}},
	}, func(context.Context, *ubus.Client) error { return boom })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if res.Outcome != Reverted {
		t.Errorf("a clean revert reported %s — reason: %s", res.Outcome, res.Reason)
	}
	if res.Stranded {
		t.Errorf("a device that reverted perfectly was reported stranded, which "+
			"tells an operator to go and hand-reverse correct config: %s", res.Reason)
	}

	// And the device really did revert.
	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	defer fresh.Destroy(ctx)
	if got := marker(t, fresh, "oonfeewrt_probe2"); got != "BASE" {
		t.Errorf("the device holds %q; the test's premise (a clean revert) does "+
			"not hold, so its verdict assertions prove nothing", got)
	}
}

// The confirm-failure path must finish inside the caller's deadline.
//
// Both waits used to be anchored at "now": confirmPoll started its window after
// the health check returned, and awaitRevert then waited a SECOND full window
// after the poll had already spent the first. With the shipped constants that
// is 90s + 105s against a 180s apply deadline — so the context expired inside
// the wait, the fresh-session verification never ran, and a device that had
// reverted cleanly was reported Unknown and Stranded.
//
// Scaled down here, with the same shape: a deadline of twice the plan timeout,
// which the old anchoring overran and the arm-anchored one fits inside.
func TestConfirmFailureStillReachesVerificationInsideTheDeadline(t *testing.T) {
	c := dial(t)
	seed(t, c, "oonfeewrt_probe", "BASE3")

	e := fastEngine()
	// Make confirm impossible: the window is spent polling, exactly as it is
	// when rpcd restarts inside it.
	e.ConfirmInterval = 50 * time.Millisecond

	timeout := 1 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeout+e.RevertGrace)
	defer cancel()

	p := plan("oonfeewrt_probe", "DOOMED3")
	p.Timeout = timeout

	// Health passes, so the engine goes on to confirm — and confirm is what
	// fails, because the applying session is destroyed underneath it.
	res, err := e.Apply(ctx, c, p, func(hctx context.Context, _ *ubus.Client) error {
		c.Destroy(hctx) // the session that alone could confirm
		return nil
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome == Unknown && strings.Contains(res.Reason, "cancelled before verification") {
		t.Fatalf("the deadline expired before the revert could be verified: %s", res.Reason)
	}
	t.Logf("outcome=%s stranded=%v reason=%s", res.Outcome, res.Stranded, res.Reason)
}

func TestRollbackBudgetUsesExactRollbackAndRevertFloor(t *testing.T) {
	e := New()
	if got, want := MinApplyBudget(), 105*time.Second; got != want {
		t.Fatalf("default minimum apply budget = %s, want %s", got, want)
	}

	deadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	required := DefaultTimeout + DefaultRevertGrace
	if err := e.requireRollbackBudget(ctx, deadline.Add(-required), DefaultTimeout); err != nil {
		t.Fatalf("the exact rollback + revert floor must be accepted: %v", err)
	}
	if err := e.requireRollbackBudget(ctx, deadline.Add(-required+time.Nanosecond), DefaultTimeout); err == nil {
		t.Fatal("one nanosecond below the rollback + revert floor must be refused")
	}
	if err := e.requireRollbackBudget(context.Background(), deadline, DefaultTimeout); err != nil {
		t.Fatalf("a context without a deadline has no finite budget to reject: %v", err)
	}
}

// The fleet admission check happens before connecting and planning, but those
// steps plus staging can consume its safety margin. Model that passage with the
// injected clock: the operation starts with exactly enough time, then reaches
// the post-verification arm point one second late. The live marker must remain
// untouched and the staged delta must be gone.
func TestApplyRefusesWhenStagingConsumesRollbackBudget(t *testing.T) {
	c := dial(t)
	seed(t, c, "oonfeewrt_probe2", "BASE")

	e := fastEngine()
	p := plan("oonfeewrt_probe2", "MUST_NOT_LAND")
	required := p.Timeout + e.RevertGrace
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	started := deadline.Add(-required)
	nowCalls := 0
	e.Now = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return started
		}
		return started.Add(time.Second)
	}
	healthCalled := false
	res, err := e.Apply(ctx, c, p, func(context.Context, *ubus.Client) error {
		healthCalled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "insufficient rollback budget after staging") {
		t.Fatalf("Apply error = %v, want post-staging rollback-budget refusal", err)
	}
	if !strings.Contains(err.Error(), "no live configuration was written") {
		t.Fatalf("refusal must state the write boundary truthfully: %v", err)
	}
	if res.Outcome != "" {
		t.Fatalf("pre-write refusal must not claim a router outcome, got %q", res.Outcome)
	}
	if healthCalled {
		t.Fatal("health ran even though uci.apply was never armed")
	}
	if got := marker(t, c, "oonfeewrt_probe2"); got != "BASE" {
		t.Fatalf("budget refusal changed live config: marker = %q", got)
	}
	var changes struct {
		Changes map[string][]any `json:"changes"`
	}
	if err := c.Call(context.Background(), "uci", "changes", struct{}{}, &changes); err != nil {
		t.Fatalf("uci.changes: %v", err)
	}
	if len(changes.Changes) != 0 {
		t.Fatalf("budget refusal left staged changes behind: %v", changes.Changes)
	}
}
