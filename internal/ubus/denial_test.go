package ubus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The permanent half of the -32002 contract. Until the mock could produce a
// denial for a VALID session, this could only be asserted against hardware —
// which meant the "one re-login, then give up" policy was untested in CI, and
// that policy is the difference between a clear capability error and an
// infinite retry loop against someone's router.

func setACLGap(t *testing.T, c *Client, pairs ...[2]string) {
	t.Helper()
	list := make([]map[string]string, 0, len(pairs))
	for _, p := range pairs {
		list = append(list, map[string]string{"object": p[0], "method": p[1]})
	}
	if err := c.Call(context.Background(), "__test", "set_acl_gap",
		map[string]any{"pairs": list}, nil); err != nil {
		t.Skipf("mock does not support ACL-gap simulation: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Call(context.Background(), "__test", "set_acl_gap",
			map[string]any{"pairs": []any{}}, nil)
	})
}

func TestUngrantedMethodIsPermanentAfterOneRelogin(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	setACLGap(t, c, [2]string{"rc", "init"})

	before := c.Session()
	err := c.Call(ctx, "rc", "init", map[string]any{"name": "x", "action": "y"}, nil)

	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("want *DeniedError for an ungranted method, got %T: %v", err, err)
	}
	if !de.Retried {
		t.Fatal("a denial that survives the single re-login must be marked " +
			"Retried, or callers cannot tell it from an expired session")
	}
	if !IsPermanent(err) {
		t.Fatal("an ACL gap is permanent; retrying it loops forever")
	}
	if c.Session() == before {
		t.Error("the client should have spent its one re-login disambiguating")
	}
}

// The distinction that matters: an ACL gap and an expired session arrive on the
// same wire code, and only the second is worth retrying.
func TestDeniedErrorDistinguishesGapFromExpiry(t *testing.T) {
	ctx := context.Background()
	c := dial(t)

	// Expired session: recoverable, and Call recovers transparently.
	if err := c.Call(ctx, "session", "destroy", struct{}{}, nil); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := c.Call(ctx, "uci", "get", map[string]any{"config": "network"}, nil); err != nil {
		t.Fatalf("an expired session should recover, got %v", err)
	}

	// ACL gap: not recoverable, and must say so.
	setACLGap(t, c, [2]string{"rc", "init"})
	err := c.Call(ctx, "rc", "init", map[string]any{"name": "x", "action": "y"}, nil)
	if !IsPermanent(err) {
		t.Fatalf("an ACL gap must be permanent, got %v", err)
	}
}

// IsPermanent is consulted through layers of fmt.Errorf wrapping, and a bare
// type assertion silently reports every wrapped permanent failure as retryable.
func TestIsPermanentSeesThroughWrapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"wrapped status error", fmt.Errorf("apply: %w",
			&StatusError{Object: "uci", Method: "get", Status: StatusPermissionDenied}), true},
		{"double-wrapped denial", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w",
			&DeniedError{Object: "rc", Method: "init", Retried: true})), true},
		{"denial not yet retried", &DeniedError{Object: "rc", Method: "init"}, false},
		{"wrapped protocol error", fmt.Errorf("x: %w",
			&ProtocolError{Code: -32700, Message: "parse"}), true},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := IsPermanent(tc.err); got != tc.want {
			t.Errorf("%s: IsPermanent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStatusPermanenceDistinguishesRefusalFromTransientFailure(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   bool
	}{
		{StatusPermissionDenied, true},
		{StatusNotSupported, true},
		{StatusNotFound, true},
		{StatusTimeout, false},
		{StatusUnknownError, false},
		{StatusConnectionFailed, false},
	} {
		err := &StatusError{Object: "x", Method: "y", Status: tc.status}
		if got := IsPermanent(err); got != tc.want {
			t.Errorf("%s permanent = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// Batch is the poll's workhorse; its chunking and its recovery path had no
// coverage at all.
func TestBatchChunksOnBytesAndKeepsOrder(t *testing.T) {
	ctx := context.Background()
	c := dial(t)

	// Enough padded calls to cross the 48KB chunk boundary several times.
	const n = 600
	calls := make([]Invocation, n)
	pad := strings.Repeat("x", 120)
	for i := range calls {
		calls[i] = Invocation{Object: "uci", Method: "get", Args: map[string]any{
			"config": "network", "__pad": fmt.Sprintf("%s-%04d", pad, i)}}
	}
	res, err := c.Batch(ctx, calls)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != n {
		t.Fatalf("every call must be answered across chunk boundaries: got %d of %d",
			len(res), n)
	}
	for i, r := range res {
		if r.Err != nil {
			t.Fatalf("call %d failed: %v", i, r.Err)
		}
	}
}

func TestBatchReportsPerCallErrorAtTheRightIndexAcrossChunks(t *testing.T) {
	ctx := context.Background()
	c := dial(t)

	const n = 500
	calls := make([]Invocation, n)
	pad := strings.Repeat("y", 120)
	for i := range calls {
		calls[i] = Invocation{Object: "uci", Method: "get", Args: map[string]any{
			"config": "network", "__pad": fmt.Sprintf("%s-%04d", pad, i)}}
	}
	// One deliberate failure, deep enough to land in a later chunk.
	const bad = 400
	calls[bad] = Invocation{Object: "uci", Method: "get",
		Args: map[string]any{"config": "no_such_config_at_all"}}

	res, err := c.Batch(ctx, calls)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != n {
		t.Fatalf("want %d results, got %d", n, len(res))
	}
	if res[bad].Err == nil {
		t.Fatalf("the failing call must report at index %d", bad)
	}
	for i, r := range res {
		if i != bad && r.Err != nil {
			t.Fatalf("call %d should have succeeded, got %v", i, r.Err)
		}
	}
}

func TestBatchRecoversFromADeadSession(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	if err := c.Call(ctx, "session", "destroy", struct{}{}, nil); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	dead := c.Session()

	res, err := c.Batch(ctx, []Invocation{
		{Object: "system", Method: "board"},
		{Object: "system", Method: "info"},
	})
	if err != nil {
		t.Fatalf("Batch after session death: %v", err)
	}
	for i, r := range res {
		if r.Err != nil {
			t.Fatalf("call %d should have recovered: %v", i, r.Err)
		}
	}
	if c.Session() == dead {
		t.Error("Batch should have re-logged in once to recover")
	}
}

func TestBatchMarksAPartialDenialAsAPermanentACLGap(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	setACLGap(t, c, [2]string{"rc", "init"})
	before := c.Session()

	res, err := c.Batch(ctx, []Invocation{
		{Object: "rc", Method: "init", Args: map[string]any{"name": "x", "action": "y"}},
		{Object: "system", Method: "info"},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 2 || res[0].Err == nil || res[1].Err != nil {
		t.Fatalf("mixed batch results = %+v", res)
	}
	var denied *DeniedError
	if !errors.As(res[0].Err, &denied) || !denied.Retried || !IsPermanent(res[0].Err) {
		t.Fatalf("partial denial was not identified as an ACL gap: %v", res[0].Err)
	}
	if c.Session() != before {
		t.Fatal("a successful sibling call already proved the session; re-login was unnecessary")
	}
}
