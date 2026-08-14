//go:build integration

package applyengine

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Drives the real device through a complete apply cycle, including a
// deliberately unconfirmed one, so the rollback guarantee is exercised against
// the actual timer rather than the mock's.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt \
//	OONFEE_TEST_PASS=... go test -tags=integration ./internal/applyengine/ -v
//
// Writes ONLY to the oonfeewrt_probe scratch config, which no service reads.

func realClient(t *testing.T) *ubus.Client {
	t.Helper()
	host, user, pass := os.Getenv("OONFEE_TEST_HOST"),
		os.Getenv("OONFEE_TEST_USER"), os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER")
	}
	c := ubus.New(ubus.Options{Host: host, Timeout: 30 * time.Second})
	if err := c.Login(context.Background(), user, pass); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func realSeed(t *testing.T, c *ubus.Client, marker string) {
	t.Helper()
	ctx := context.Background()
	_ = c.Call(ctx, "uci", "revert", map[string]any{"config": "oonfeewrt_probe"}, nil)
	_ = c.Call(ctx, "uci", "delete",
		map[string]any{"config": "oonfeewrt_probe", "section": "probe"}, nil)
	_ = c.Call(ctx, "uci", "commit", map[string]any{"config": "oonfeewrt_probe"}, nil)
	if err := c.Call(ctx, "uci", "add", map[string]any{
		"config": "oonfeewrt_probe", "type": "probe", "name": "probe",
		"values": map[string]string{"marker": marker}}, nil); err != nil {
		// The scratch config lives in the ACL's `oonfeewrt-probe` group, which
		// adoption deliberately does NOT grant: production scope is production
		// scope, and widening it so a test can run would be exactly the wrong
		// trade. Grant it to the test login by hand instead.
		if ubus.IsPermanent(err) {
			t.Skipf("the login is not granted the scratch config this test "+
				"writes to (%v).\n"+
				"These tests write only to oonfeewrt_probe, which no service "+
				"reads. To enable them on a test device:\n"+
				"  ssh root@<device> \"uci add_list rpcd.oonfeewrt.read=oonfeewrt-probe; "+
				"uci add_list rpcd.oonfeewrt.write=oonfeewrt-probe; uci commit rpcd\"",
				err)
		}
		t.Fatalf("seed add: %v", err)
	}
	if err := c.Call(ctx, "uci", "commit",
		map[string]any{"config": "oonfeewrt_probe"}, nil); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

func realMarker(t *testing.T, c *ubus.Client) string {
	t.Helper()
	var out struct {
		Value string `json:"value"`
	}
	if err := c.Call(context.Background(), "uci", "get", map[string]any{
		"config": "oonfeewrt_probe", "section": "probe", "option": "marker"}, &out); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return out.Value
}

func realEngine() *Engine {
	e := New()
	e.ConfirmInterval = time.Second
	e.RevertGrace = 8 * time.Second
	return e
}

func realPlan(marker string) Plan {
	return Plan{
		Timeout: 15 * time.Second,
		Ops: []Op{{Kind: OpSet, Config: "oonfeewrt_probe", Section: "probe",
			Values: map[string]string{"marker": marker}}},
	}
}

func TestIntegrationHealthyApplyIsConfirmed(t *testing.T) {
	ctx := context.Background()
	c := realClient(t)
	realSeed(t, c, "BASE")

	res, err := realEngine().Apply(ctx, c, realPlan("CONFIRMED"),
		func(hctx context.Context, verify *ubus.Client) error {
			// Inside the window rpcd hands out the applying token, so health
			// must read RUNTIME state, never uci.get. This is what a real
			// health probe looks like.
			var ifaces struct {
				Interface []struct {
					Interface string `json:"interface"`
					Up        bool   `json:"up"`
				} `json:"interface"`
			}
			if err := verify.Call(hctx, "network.interface", "dump", nil, &ifaces); err != nil {
				return err
			}
			for _, i := range ifaces.Interface {
				if i.Interface == "lan" && i.Up {
					return nil
				}
			}
			return errors.New("lan is not up")
		})
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
	if got := realMarker(t, fresh); got != "CONFIRMED" {
		t.Fatalf("confirmed change must persist on the device, got %q", got)
	}
	t.Logf("outcome: %s (%s)", res.Outcome, res.Reason)
}

// The guarantee the whole design rests on, against the device's own timer:
// health fails, we never confirm, the device restores itself, and a fresh
// session sees the restoration.
func TestIntegrationUnhealthyApplyRevertsOnTheDevice(t *testing.T) {
	ctx := context.Background()
	c := realClient(t)
	realSeed(t, c, "SAFE")

	boom := errors.New("simulated: expected SSID missing")
	start := time.Now()
	res, err := realEngine().Apply(ctx, c, realPlan("SHOULD_VANISH"),
		func(context.Context, *ubus.Client) error { return boom })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	t.Logf("outcome after %s: %s", time.Since(start).Round(time.Second), res)

	if res.Outcome != Reverted {
		t.Fatalf("want Reverted, got %s (stranded=%v)", res, res.Stranded)
	}
	if !errors.Is(res.HealthErr, boom) {
		t.Errorf("health error should be carried through, got %v", res.HealthErr)
	}
	fresh, err := c.FreshSession(ctx)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	defer fresh.Destroy(ctx)
	if got := realMarker(t, fresh); got != "SAFE" {
		t.Fatalf("device should have reverted to SAFE, got %q", got)
	}

	// And the trap: the APPLYING session still reads the failed value, which is
	// why verification must never use it.
	if got := realMarker(t, c); got != "SHOULD_VANISH" {
		t.Logf("note: applying session read %q (expected the stale SHOULD_VANISH)", got)
	}
}

func TestIntegrationPreflightSeesNoForeignEdits(t *testing.T) {
	ctx := context.Background()
	c := realClient(t)
	dirty, err := ForeignDirtyConfigs(ctx, c)
	if err != nil {
		t.Fatalf("ForeignDirtyConfigs (needs file.list on /tmp/.uci): %v", err)
	}
	t.Logf("configs with foreign uncommitted edits: %v", dirty)
}
