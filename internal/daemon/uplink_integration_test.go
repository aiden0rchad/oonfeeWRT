//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// A wireless uplink on real hardware: one device reaching the network over the
// air as a 4-address bridge.
//
// # Read this before running it
//
// This configures a device to reach the network WIRELESSLY. If its driver
// refuses 4-address framing the way mwlwifi refuses mesh points (§5q), the
// station applies cleanly and never associates — and on a device with no cable
// that is an unreachable device.
//
// So it is written to be run with the joining device STILL CABLED, and it does
// not ask anyone to unplug anything. What it proves is that the station
// associates and the bridge forms; whether the device survives losing its cable
// is the operator's experiment, made safe by the fact that plugging the cable
// back in always works.
//
// A cabled device bridged into the network it is joining is a layer-2 loop, so
// this REFUSES TO RUN unless STP is on, on the joining device's bridge.
// OpenWrt ships bridges with STP off, and the symptom of getting this wrong is
// not an error — it is a broadcast storm across somebody's home network.
//
//	OONFEE_UPLINK=1 OONFEE_SEED_DIR=$PWD/.run OONFEE_SEED_PASSFILE=/path \
//	OONFEE_AP1=192.168.1.1 OONFEE_AP2=192.168.1.2 \
//	OONFEE_UPLINK_DEVICE=192.168.1.2 \
//	OONFEE_ADMIN_USER=root OONFEE_ADMIN_PASS= \
//	OONFEE_WLAN_SSID=oonfee-roam OONFEE_WLAN_KEY=... \
//	go test -tags=integration ./internal/daemon/ -run TestIntegrationUplink -v
func TestIntegrationUplink(t *testing.T) {
	if os.Getenv("OONFEE_UPLINK") != "1" {
		t.Skip("set OONFEE_UPLINK=1 to run the wireless uplink test")
	}
	ap1, ap2 := os.Getenv("OONFEE_AP1"), os.Getenv("OONFEE_AP2")
	joiner := os.Getenv("OONFEE_UPLINK_DEVICE")
	ssid := os.Getenv("OONFEE_WLAN_SSID")
	if ap1 == "" || ap2 == "" || joiner == "" || ssid == "" {
		t.Skip("set OONFEE_AP1, OONFEE_AP2, OONFEE_UPLINK_DEVICE and OONFEE_WLAN_SSID")
	}
	ctx := context.Background()

	cfg := testConfig(t, "operator passphrase")
	if dir := os.Getenv("OONFEE_SEED_DIR"); dir != "" {
		cfg.DataDir = dir
		if pf := os.Getenv("OONFEE_SEED_PASSFILE"); pf != "" {
			cfg.PassphraseFile = pf
		}
	}
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	var ids []int64
	var joinerID int64
	for _, host := range []string{ap1, ap2} {
		id, err := adoptOrReuse(ctx, t, d, host)
		if err != nil {
			t.Fatalf("adopt %s: %v", host, err)
		}
		ids = append(ids, id)
		if host == joiner {
			joinerID = id
		}
	}
	if joinerID == 0 {
		t.Fatalf("OONFEE_UPLINK_DEVICE %q is not one of the adopted devices", joiner)
	}

	// The device that will join must be able to. Checked against its own
	// capability record rather than assumed, and reported rather than skipped
	// silently — "the test did not run" and "the test passed" must not look the
	// same from the outside.
	dev, err := d.Store.DeviceByID(ctx, joinerID)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := deviceCaps(dev)
	if err != nil {
		t.Fatal(err)
	}
	if st := caps.State(capability.FeatWirelessUplink); st != capability.Present {
		t.Fatalf("%s reports wireless-uplink %s, so there is nothing to test here",
			joiner, st)
	}

	// STP, checked rather than assumed.
	//
	// The doc comment above promised this and an earlier draft did not do it,
	// which is §6's "a comment that states a guarantee is a claim" in a comment
	// written minutes before. A cabled device bridged into the network it is
	// joining is a layer-2 loop; OpenWrt ships bridges with STP off, and the
	// symptom of getting it wrong is not an error, it is a broadcast storm
	// across somebody's home network. So this refuses to proceed.
	stp, err := sshOut(joiner, "cat /sys/class/net/br-lan/bridge/stp_state")
	if err != nil || strings.TrimSpace(stp) != "1" {
		t.Fatalf("STP is not enabled on %s (%q): this test bridges a cabled "+
			"device into the network it is joining, which is a loop. Enable STP "+
			"on that bridge first, or run it on a device with no cable",
			joiner, strings.TrimSpace(stp))
	}
	t.Logf("STP is on for %s — the loop this test creates is protected", joiner)

	seedRoamingSite(ctx, t, d, ids, ssid, os.Getenv("OONFEE_WLAN_KEY"))

	// The AP half, through the store rather than by editing a row: a WLAN that
	// does not accept bridges is the half people forget, and the station would
	// associate as an ordinary client with everything behind it dark.
	site, err := d.Store.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wlan model.WLAN
	for _, w := range site.WLANs {
		if w.SSID == ssid {
			wlan = w
		}
	}
	if wlan.ID == 0 {
		t.Fatalf("no WLAN %q in the site model", ssid)
	}
	wlan.Options.AllowUplink = true
	wlan.Security.Key = os.Getenv("OONFEE_WLAN_KEY")
	if err := d.Store.SaveWLAN(ctx, &wlan); err != nil {
		t.Fatal(err)
	}

	up := &model.Uplink{DeviceID: joinerID, WLANID: wlan.ID,
		Band: model.Band5G, Enabled: true}
	for _, u := range site.Uplinks {
		if u.DeviceID == joinerID {
			up.ID = u.ID
		}
	}
	if errs := up.Validate(mustSite(t, d)); len(errs) > 0 {
		t.Fatalf("the uplink is not valid: %v", errs)
	}
	if err := d.Store.SaveUplink(ctx, up); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.Store.DeleteUplink(context.Background(), up.ID)
		w := wlan
		w.Options.AllowUplink = false
		w.Security.Key = os.Getenv("OONFEE_WLAN_KEY")
		_ = d.Store.SaveWLAN(context.Background(), &w)
		cleanupCtx := context.Background()
		if _, err := d.ApplySite(cleanupCtx, boundApplyRequest(t, d, cleanupCtx,
			api.ApplyRequest{AcknowledgeTraversal: true})); err != nil {
			t.Logf("could not prune the uplink: %v", err)
		}
	})

	// The traversal gate must fire. Removing a device's only route is the
	// hazard this whole feature carries, and a preview that does not say so is
	// worse than no preview.
	prev, err := d.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawLoopWarning, sawTraversal bool
	for _, row := range prev.Devices {
		if row.TouchesTraversal {
			sawTraversal = true
		}
		for _, om := range row.Omitted {
			t.Logf("preview %s: %s", row.Name, om)
			if strings.Contains(om, "layer-2 loop") {
				sawLoopWarning = true
			}
		}
	}
	if !sawLoopWarning {
		t.Error("the preview did not warn about the layer-2 loop, which is the " +
			"one hazard the controller cannot check for the operator")
	}
	if !sawTraversal {
		t.Error("writing a wireless uplink did not register as touching the " +
			"management path")
	}

	if err := d.StartCollector(ctx, collector.Options{
		Baseline: 3 * time.Second, Log: quietLogger(),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := d.ApplySite(ctx, api.ApplyRequest{
		PreviewToken: prev.PreviewToken, AcknowledgeTraversal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Devices {
		t.Logf("APPLY %s -> %s (%d changes) %s", r.Name, r.Outcome, r.Changes, r.Reason)
	}
	if res.Aborted {
		t.Fatalf("apply aborted after %s", res.AbortedAfter)
	}

	// Did it actually associate? A station that applies and never associates is
	// §5q's shape in a different mode, and it is the outcome this hardware
	// might genuinely produce — so it is asserted rather than assumed.
	deadline := time.Now().Add(60 * time.Second)
	var assoc string
	for time.Now().Before(deadline) {
		assoc = uplinkStatus(t, joiner)
		if strings.Contains(assoc, "ESSID: \"") && !strings.Contains(assoc, "unknown") {
			break
		}
		time.Sleep(4 * time.Second)
	}
	t.Logf("station on %s:\n%s", joiner, assoc)
	if !strings.Contains(assoc, ssid) {
		t.Errorf("the station never joined %q. On a device with no cable that "+
			"is an unreachable device, which is why this test runs cabled", ssid)
	}
}

func mustSite(t *testing.T, d *Daemon) model.Site {
	t.Helper()
	s, err := d.Store.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// uplinkStatus reads the joining device's station interface over SSH.
//
// SSH rather than ubus because the question is whether a 4-address station
// associated, and `iwinfo <iface> info` on a station interface is the source
// that answers it plainly. The controller does not need this; the test does.
func uplinkStatus(t *testing.T, host string) string {
	t.Helper()
	out, err := sshOut(host, "iwinfo 2>/dev/null | grep -A3 -E 'Mode: Client' || "+
		"iw dev | grep -B2 'type managed'")
	if err != nil {
		return fmt.Sprintf("(could not read: %v)", err)
	}
	return out
}

// sshOut runs one read-only command on a device and returns its output.
func sshOut(host, cmd string) (string, error) {
	c := exec.Command("ssh", "-o", "ConnectTimeout=6", "-o", "BatchMode=yes",
		"root@"+host, cmd)
	out, err := c.CombinedOutput()
	return string(out), err
}
