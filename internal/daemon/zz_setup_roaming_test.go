//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
)

// Sets up a real roaming WLAN across both APs, through the controller.
//
// Not an assertion — a setup helper, like TestSeedLiveInventory. It exists so
// the two-AP roaming test is reproducible rather than a sequence of uci
// commands someone has to remember.
//
//	OONFEE_ROAM=1 OONFEE_SEED_DIR=/path OONFEE_SEED_PASSFILE=/path/pass \
//	OONFEE_AP1=192.168.1.1 OONFEE_AP1_PASS=... \
//	OONFEE_AP2=192.168.1.2 OONFEE_AP2_PASS=... \
//	OONFEE_WLAN_SSID=... OONFEE_WLAN_KEY=... \
//	go test -tags=integration ./internal/daemon/ -run TestZZSetupRoaming -v
func TestZZSetupRoaming(t *testing.T) {
	if os.Getenv("OONFEE_ROAM") != "1" {
		t.Skip("set OONFEE_ROAM=1")
	}
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.DataDir = os.Getenv("OONFEE_SEED_DIR")
	cfg.Listen = "127.0.0.1:0"
	cfg.PassphraseFile = os.Getenv("OONFEE_SEED_PASSFILE")

	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Adopted, not hand-seeded.
	//
	// This used to write inventory rows directly, sealing the `oonfeewrt`
	// password the devices happened to have. Two things were wrong with that.
	// The MACs were literals — one the box's WAN-side address, one a radio's —
	// while adoption identifies a device by its LAN bridge, so a seeded row and
	// a real adoption of the same box coexisted as two devices, one physical AP
	// polled twice against a budget of one request a minute. And it assumed the
	// controller login already existed, which stops being true the moment
	// someone factory resets a router.
	//
	// adoptOrReuse handles all of it: adopt, or reuse and re-probe, or — when
	// the stored credential is refused by a device that is plainly alive —
	// force un-adopt and adopt again, which is what a reset leaves behind.
	var ids []int64
	for _, host := range []string{os.Getenv("OONFEE_AP1"), os.Getenv("OONFEE_AP2")} {
		id, err := adoptOrReuse(ctx, t, d, host)
		if err != nil {
			t.Fatalf("adopt %s: %v", host, err)
		}
		ids = append(ids, id)
	}

	seedRoamingSite(ctx, t, d, ids, os.Getenv("OONFEE_WLAN_SSID"),
		os.Getenv("OONFEE_WLAN_KEY"))

	prev, err := d.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range prev.Devices {
		t.Logf("preview %s: %d change(s) blocked=%v err=%q",
			row.Name, len(row.Changes), row.Blocked, row.Error)
		for _, om := range row.Omitted {
			t.Logf("   omitted: %s", om)
		}
	}
	res, err := d.ApplySite(ctx, api.ApplyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Devices {
		t.Logf("APPLY %s -> %s (%d changes) %s", r.Name, r.Outcome, r.Changes, r.Reason)
	}
	if res.Aborted {
		t.Fatalf("apply aborted after %s", res.AbortedAfter)
	}
}
