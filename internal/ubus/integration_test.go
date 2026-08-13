//go:build integration

package ubus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// Integration tests against a real OpenWrt device. The mock is faithful, but
// "it passed against the mock" is exactly the claim this project has learned
// not to trust — run these before believing the transport works.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt \
//	OONFEE_TEST_PASS=... go test -tags=integration ./internal/ubus/ -v
//
// Read-only: nothing here writes UCI or touches a service.

func realClient(t *testing.T, https bool) *Client {
	t.Helper()
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	c := New(Options{Host: host, HTTPS: https, Timeout: 20 * time.Second})
	if err := c.Login(context.Background(), user, pass); err != nil {
		t.Fatalf("login to %s: %v", host, err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestIntegrationDenialChannels(t *testing.T) {
	ctx := context.Background()
	c := realClient(t, false)

	// Granted method, ungranted TARGET -> proxied, object refuses -> status 6.
	// Permanent: the session is fine, so re-authenticating cannot help.
	err := c.Call(ctx, "uci", "get", map[string]any{"config": "rpcd"}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != StatusPermissionDenied {
		t.Fatalf("reading an ungranted uci config should be status 6, got %v", err)
	}
	if !IsPermanent(err) {
		t.Error("status 6 must be permanent")
	}

	// Ungranted OBJECT+METHOD -> rpcd refuses to proxy at all -> -32002, and
	// after one re-login it is a permanent capability gap.
	err = c.Call(ctx, "rc", "init", map[string]any{"name": "dropbear", "action": "status"}, nil)
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("an ungranted object should yield *DeniedError, got %T: %v", err, err)
	}
	if !de.Retried {
		t.Error("a genuine ACL gap should be reported only after the single re-login")
	}
	if !IsPermanent(err) {
		t.Error("a denial that survives re-login must be permanent")
	}
}

func TestIntegrationFreshSessionSeesCommittedState(t *testing.T) {
	ctx := context.Background()
	c := realClient(t, false)
	other, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("FreshSession: %v", err)
	}
	defer other.Destroy(ctx)
	// Not always independent: while a rollback is armed anywhere on the device,
	// rpcd hands every login the applying token. The invariant is that
	// Shared() reports this accurately — a caller must never be told a session
	// is independent when it is not.
	if other.Session() == c.Session() {
		if !other.Shared() {
			t.Fatal("got the parent's token but Shared() is false; callers " +
				"would destroy the applying session believing it was theirs")
		}
		t.Log("a rollback is armed on this device, so the session is shared " +
			"(expected; run integration tests with -p 1 to avoid the overlap)")
	} else if other.Shared() {
		t.Fatal("Shared() is true but the token differs")
	}
	var out struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := other.Call(ctx, "uci", "get", map[string]any{"config": "network"}, &out); err != nil {
		t.Fatalf("fresh session read: %v", err)
	}
	if len(out.Values) == 0 {
		t.Fatal("expected network config from the fresh session")
	}
}

// Batching is the cheap win the budget depends on; prove the real uhttpd build
// accepts an array body and answers in order.
func TestIntegrationBatch(t *testing.T) {
	c := realClient(t, false)
	calls := []Invocation{
		{Object: "system", Method: "board"},
		{Object: "system", Method: "info"},
		{Object: "network.interface", Method: "dump"},
		{Object: "luci-rpc", Method: "getHostHints"},
	}
	res, err := c.Batch(context.Background(), calls)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != len(calls) {
		t.Fatalf("want %d results, got %d", len(calls), len(res))
	}
	for i, r := range res {
		if r.Err != nil {
			t.Errorf("call %d (%s.%s): %v", i, calls[i].Object, calls[i].Method, r.Err)
		}
	}
	var board struct {
		Model   string `json:"model"`
		Release struct {
			Description string `json:"description"`
		} `json:"release"`
	}
	if err := res[0].Decode(&board); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	t.Logf("device: %s / %s", board.Model, board.Release.Description)
}

// The device serves a self-signed cert, so TOFU pinning is the trust model.
// A second client given the recorded fingerprint must connect; a wrong one must
// be refused before any request is sent.
func TestIntegrationTLSPinning(t *testing.T) {
	ctx := context.Background()
	c := realClient(t, true)
	pin := c.PinnedCert()
	if len(pin) != 64 {
		t.Fatalf("expected a 64-char sha256 fingerprint, got %q", pin)
	}
	t.Logf("pinned cert: %s", pin)

	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")

	good := New(Options{Host: host, HTTPS: true, PinnedCert: pin, Timeout: 20 * time.Second})
	if err := good.Login(ctx, user, pass); err != nil {
		t.Fatalf("correct pin should connect: %v", err)
	}
	good.Close()

	wrong := New(Options{Host: host, HTTPS: true,
		PinnedCert: "00000000000000000000000000000000000000000000000000000000deadbeef",
		Timeout:    20 * time.Second})
	if err := wrong.Login(ctx, user, pass); err == nil {
		t.Fatal("a mismatched pin must refuse the connection")
	}
	wrong.Close()
}
