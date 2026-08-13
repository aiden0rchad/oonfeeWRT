package ubus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The tests run against tools/mock_ubus.py, which reproduces the device
// semantics measured in docs/IMPLEMENTATION.md §14 — per-session tokens,
// session-bound confirm, a rollback that restores /etc/config while leaving the
// applying session's delta in place, and -32002 for a dead session.
//
// Every assertion below is a measured device behaviour, not a preference. If
// one starts failing, suspect the change under test before the test.

const mockPassword = "good"

var mockAddr string

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot locate repo root:", err)
		os.Exit(1)
	}
	mock := filepath.Join(root, "tools", "mock_ubus.py")
	if _, err := os.Stat(mock); err != nil {
		fmt.Fprintln(os.Stderr, "mock_ubus.py not found:", err)
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no free port:", err)
		os.Exit(1)
	}
	mockAddr = fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command("python3", mock, "--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot start mock:", err)
		os.Exit(1)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	if err := waitReady(mockAddr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "mock never became ready:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	os.Exit(code)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("go.mod not found in any parent")
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
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func dial(t *testing.T) *Client {
	t.Helper()
	c := New(Options{Host: mockAddr})
	if err := c.Login(context.Background(), "root", mockPassword); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestLoginYieldsDistinctSessions(t *testing.T) {
	a, b := dial(t), dial(t)
	if a.Session() == nullSession || a.Session() == "" {
		t.Fatal("session token not stored after login")
	}
	if a.Session() == b.Session() {
		t.Fatal("two logins returned the same token; session-scoped behaviour " +
			"cannot be reproduced and the apply path cannot be trusted")
	}
}

// Status 6 means the session is valid and the target is refused. Retrying it is
// pure latency, so Call must surface it immediately as permanent.
func TestStatusErrorIsPermanentAndNotRetried(t *testing.T) {
	c := dial(t)
	before := c.Session()

	err := c.Call(context.Background(), "uci", "add", map[string]any{
		"config": "definitely_not_a_config", "type": "x", "name": "y",
	}, nil)
	if err == nil {
		t.Fatal("expected an error adding to a nonexistent config")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("want *StatusError, got %T: %v", err, err)
	}
	if se.Status != StatusNotFound {
		t.Fatalf("want NOT_FOUND (uci.add will not create a config file), got %s", se.Status)
	}
	if !IsPermanent(err) {
		t.Fatal("a target refusal must be permanent")
	}
	if c.Session() != before {
		t.Fatal("Call re-authenticated on a ubus status; only -32002 may do that")
	}
}

// -32002 is ambiguous: dead session OR ungranted method. Exactly one re-login
// disambiguates it, and a live session must transparently recover.
func TestDeadSessionTriggersExactlyOneRelogin(t *testing.T) {
	c := dial(t)
	first := c.Session()

	// Kill the session server-side; the token in hand is now invalid.
	if err := c.Call(context.Background(), "session", "destroy", struct{}{}, nil); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	var out struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := c.Call(context.Background(), "uci", "get",
		map[string]any{"config": "network"}, &out); err != nil {
		t.Fatalf("call after session death should have recovered, got %v", err)
	}
	if c.Session() == first {
		t.Fatal("expected a new session token after transparent re-login")
	}
	if len(out.Values) == 0 {
		t.Fatal("recovered call returned no payload")
	}
}

// A token refresh inside the confirmation window guarantees the device reverts,
// because confirm is bound to the session that applied. So the window must
// suppress the recovery that is otherwise correct.
func TestConfirmWindowSuppressesRelogin(t *testing.T) {
	c := dial(t)
	if err := c.Call(context.Background(), "session", "destroy", struct{}{}, nil); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	dead := c.Session()

	end := c.BeginConfirmWindow()
	defer end()

	err := c.Call(context.Background(), "uci", "get",
		map[string]any{"config": "network"}, nil)
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("want *DeniedError inside a confirm window, got %T: %v", err, err)
	}
	if de.Retried {
		t.Fatal("Retried must be false: no re-login may happen inside the window")
	}
	if c.Session() != dead {
		t.Fatal("session was refreshed inside the confirmation window; " +
			"that guarantees the applied change reverts")
	}
}

func TestFreshSessionIsIndependent(t *testing.T) {
	c := dial(t)
	other, err := c.FreshSession(context.Background())
	if err != nil {
		t.Fatalf("FreshSession: %v", err)
	}
	defer other.Close()
	if other.Session() == c.Session() {
		t.Fatal("FreshSession returned the same token")
	}
}

// The trap the apply engine exists to survive: after a rollback, the applying
// session still reads the value it failed to set, while a fresh session reads
// the reverted one. Verification that skips the fresh session reports every
// failed apply as a success.
func TestRollbackIsInvisibleToTheApplyingSession(t *testing.T) {
	ctx := context.Background()
	c := dial(t)

	mustCall(t, c, "uci", "add", map[string]any{
		"config": "oonfeewrt_probe", "type": "probe", "name": "probe",
		"values": map[string]string{"marker": "BASE"}})
	mustCall(t, c, "uci", "commit", map[string]any{"config": "oonfeewrt_probe"})

	// Stage and apply with a short rollback armed, then deliberately never
	// confirm.
	mustCall(t, c, "uci", "set", map[string]any{
		"config": "oonfeewrt_probe", "section": "probe",
		"values": map[string]string{"marker": "DOOMED"}})
	mustCall(t, c, "uci", "apply", map[string]any{"rollback": true, "timeout": 2})

	time.Sleep(4 * time.Second) // outlive the timer

	applying := readMarker(t, c)
	if applying != "DOOMED" {
		t.Fatalf("applying session should still read its failed value "+
			"(staged delta survives the revert), got %q", applying)
	}

	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("FreshSession: %v", err)
	}
	defer fresh.Destroy(ctx)
	if got := readMarker(t, fresh); got != "BASE" {
		t.Fatalf("fresh session must observe the revert, got %q", got)
	}
}

// Only the session that applied may confirm. A second session is refused AND
// the change still reverts, which is precisely what a reconnect-then-confirm
// controller gets wrong.
func TestConfirmIsBoundToTheApplyingSession(t *testing.T) {
	ctx := context.Background()
	applier := dial(t)
	other := dial(t)

	mustCall(t, applier, "uci", "add", map[string]any{
		"config": "oonfeewrt_probe2", "type": "probe", "name": "probe",
		"values": map[string]string{"marker": "BASE"}})
	mustCall(t, applier, "uci", "commit", map[string]any{"config": "oonfeewrt_probe2"})
	mustCall(t, applier, "uci", "set", map[string]any{
		"config": "oonfeewrt_probe2", "section": "probe",
		"values": map[string]string{"marker": "NEW"}})
	mustCall(t, applier, "uci", "apply", map[string]any{"rollback": true, "timeout": 30})

	err := other.Call(ctx, "uci", "confirm", struct{}{}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != StatusPermissionDenied {
		t.Fatalf("a foreign session's confirm must be refused with status 6, got %v", err)
	}
	if err := applier.Call(ctx, "uci", "confirm", struct{}{}, nil); err != nil {
		t.Fatalf("the applying session's confirm must succeed: %v", err)
	}
}

func TestBatchPreservesOrderAndReportsPerCallErrors(t *testing.T) {
	c := dial(t)
	res, err := c.Batch(context.Background(), []Invocation{
		{Object: "system", Method: "board"},
		{Object: "uci", Method: "add", Args: map[string]any{
			"config": "nope_not_here", "type": "x", "name": "y"}},
		{Object: "system", Method: "info"},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	if res[0].Err != nil || res[2].Err != nil {
		t.Fatalf("calls 0 and 2 should succeed: %v / %v", res[0].Err, res[2].Err)
	}
	if res[1].Err == nil {
		t.Fatal("call 1 targets a nonexistent config and must report an error")
	}
	var board struct {
		Model string `json:"model"`
	}
	if err := res[0].Decode(&board); err != nil || board.Model == "" {
		t.Fatalf("decode board: %v (model=%q)", err, board.Model)
	}
}

func mustCall(t *testing.T, c *Client, obj, method string, args any) {
	t.Helper()
	if err := c.Call(context.Background(), obj, method, args, nil); err != nil {
		t.Fatalf("%s.%s: %v", obj, method, err)
	}
}

func readMarker(t *testing.T, c *Client) string {
	t.Helper()
	var out struct {
		Value string `json:"value"`
	}
	err := c.Call(context.Background(), "uci", "get", map[string]any{
		"config": "oonfeewrt_probe", "section": "probe", "option": "marker"}, &out)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return out.Value
}
