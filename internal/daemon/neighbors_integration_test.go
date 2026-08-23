//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Neighbour distribution across two real access points.
//
// This is the test the feature exists for, and it needs two devices — which is
// why nothing like it could be written until there were two. Everything below
// the daemon has cheaper coverage: the rules are unit-tested in
// internal/roaming, the wiring against the mock in neighbors_test.go. What only
// hardware can show is that two APs of different silicon, running different
// drivers, produce elements each other will accept.
//
//	OONFEE_NEIGHBOURS=1 \
//	OONFEE_AP1=192.168.1.1 OONFEE_AP2=192.168.1.2 \
//	OONFEE_ADMIN_USER=root OONFEE_ADMIN_PASS= \
//	OONFEE_WLAN_SSID=fixture-roam OONFEE_WLAN_KEY=... \
//	go test -tags=integration ./internal/daemon/ -run TestIntegrationNeighbours -v
//
// It ADOPTS both devices, which rewrites their rpcd login and their ACL file.
// That is deliberate: a widened ACL only reaches a device through adoption, so
// re-adopting is the actual upgrade path for this feature and the test should
// walk it rather than arrange the end state by hand.
func TestIntegrationNeighbours(t *testing.T) {
	if os.Getenv("OONFEE_NEIGHBOURS") != "1" {
		t.Skip("set OONFEE_NEIGHBOURS=1 to run the two-AP neighbour test")
	}
	ap1, ap2 := os.Getenv("OONFEE_AP1"), os.Getenv("OONFEE_AP2")
	ssid := os.Getenv("OONFEE_WLAN_SSID")
	if ap1 == "" || ap2 == "" || ssid == "" {
		t.Skip("set OONFEE_AP1, OONFEE_AP2 and OONFEE_WLAN_SSID")
	}
	ctx := context.Background()

	// A temp data directory by default, so the test leaves nothing behind. But
	// adoption rotates the device's rpcd password and seals it in the keyring,
	// and a throwaway keyring means the device is left with a credential nobody
	// holds. Pointing OONFEE_SEED_DIR at a real data directory makes this test
	// double as the setup helper: the same run that verifies the feature leaves
	// a controller that can be started against these devices.
	cfg := testConfig(t, "operator passphrase")
	if dir := os.Getenv("OONFEE_SEED_DIR"); dir != "" {
		cfg.DataDir = dir
		if pf := os.Getenv("OONFEE_SEED_PASSFILE"); pf != "" {
			cfg.PassphraseFile = pf
		}
		t.Logf("seeding the real data directory %s", dir)
	}

	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var ids []int64
	for _, host := range []string{ap1, ap2} {
		id, err := adoptOrReuse(ctx, t, d, host)
		if err != nil {
			t.Fatalf("adopt %s: %v", host, err)
		}
		ids = append(ids, id)
	}

	seedRoamingSite(ctx, t, d, ids, ssid, os.Getenv("OONFEE_WLAN_KEY"))

	first, err := d.DistributeNeighbours(ctx)
	if err != nil {
		t.Fatalf("DistributeNeighbours: %v", err)
	}
	for _, dev := range first.Devices {
		t.Logf("%s: updated=%d unchanged=%d err=%q skipped=%q",
			dev.Name, dev.Updated, dev.Unchanged, dev.Error, dev.Skipped)
		for _, b := range dev.BSSes {
			t.Logf("    %-10s %s %s -> %d neighbour(s) %s",
				b.Iface, b.BSSID, b.SSID, b.Neighbours, b.Failed)
		}
	}

	// Two APs, two bands each, all carrying one SSID: four BSSes, each of which
	// should know the other three.
	var bsses int
	for _, dev := range first.Devices {
		if dev.Error != "" {
			t.Errorf("%s: %s", dev.Name, dev.Error)
		}
		for _, b := range dev.BSSes {
			bsses++
			if b.Failed != "" {
				t.Errorf("%s/%s: %s", dev.Name, b.Iface, b.Failed)
				continue
			}
			if b.Neighbours != 3 {
				t.Errorf("%s/%s got %d neighbours, want 3 (the other two bands "+
					"of this SSID plus the other AP's matching band)",
					dev.Name, b.Iface, b.Neighbours)
			}
		}
	}
	if bsses != 4 {
		t.Fatalf("want 4 BSSes carrying %q, found %d — check both APs are "+
			"publishing it on both bands", ssid, bsses)
	}
	// Deliberately not "4 pushes". How many BSSes needed writing depends on
	// what they already held, and a device that kept a correct list across the
	// adoption is a success rather than a missed write. Asserting the push
	// count would make this test pass only on a fleet nobody had touched, which
	// is the one fleet that never exists.
	if first.Updated+first.Unchanged != 4 {
		t.Errorf("every BSS should be either written or already correct; "+
			"got %d updated + %d unchanged", first.Updated, first.Unchanged)
	}

	// The property that makes the cadence affordable: nothing has moved, so
	// nothing is written. hostapd hands its list back in its own order, so this
	// fails loudly if the comparison ever becomes order-sensitive.
	second, err := d.DistributeNeighbours(ctx)
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if second.Updated != 0 || second.Unchanged != 4 {
		t.Errorf("a converged fleet was rewritten: %d updated, %d unchanged",
			second.Updated, second.Unchanged)
	}
}

// adoptOrReuse makes this rerunnable against a data directory that already
// holds these devices.
//
// Adoption refuses a device it has already adopted, which is right for the API
// — silently re-adopting would rotate a working credential behind an
// operator's back. But a test that can only run against a fresh database is a
// test nobody runs twice, and re-probing an existing device is what actually
// refreshes the capability record after an ACL change.
func adoptOrReuse(ctx context.Context, t *testing.T, d *Daemon, host string) (int64, error) {
	t.Helper()

	res, err := d.Adopt(ctx, api.AdoptRequest{
		Host:     host,
		Username: os.Getenv("OONFEE_ADMIN_USER"),
		Password: os.Getenv("OONFEE_ADMIN_PASS"),
		Name:     "ap-" + strings.ReplaceAll(host, ".", "-"),
		Role:     string(model.RoleAP),
	})
	if err == nil {
		t.Logf("adopted %s: %s (%s) class=%s", host, res.Name, res.Model, res.Class)
		t.Logf("  neighbor-report: %s", featureState(res, "neighbor-report"))
		return res.DeviceID, nil
	}
	if !strings.Contains(err.Error(), "already adopted") {
		return 0, err
	}

	devices, lerr := d.Store.Devices(ctx)
	if lerr != nil {
		return 0, lerr
	}
	for _, dev := range devices {
		if dev.Host != host || !dev.Adopted() {
			continue
		}
		// Re-probe rather than trust the stored record: the ACL on the device
		// may have gained the rrm grants since it was last looked at, and the
		// capability record is what gates the distribution below.
		pr, perr := d.Reprobe(ctx, dev.ID)
		if perr == nil {
			t.Logf("reusing already-adopted %s: %s", host, dev.Name)
			t.Logf("  neighbor-report: %s", pr.Registry.State("neighbor-report"))
			return dev.ID, nil
		}

		// The stored credential does not work. On a device that answers at all,
		// the overwhelmingly likely cause is a factory reset — which removes
		// the rpcd login and the ACL file and leaves the controller holding a
		// record of a device that no longer knows it.
		//
		// The recovery is un-adopt then adopt, and un-adopt has to be FORCED:
		// the footprint it exists to remove is already gone, so phase 2 has
		// nothing to do and reports that it could not confirm a clean removal.
		t.Logf("%s: stored credential rejected (%v) — assuming a factory reset "+
			"and re-adopting", host, perr)
		if _, uerr := d.Unadopt(ctx, api.UnadoptRequest{
			DeviceID: dev.ID,
			Username: os.Getenv("OONFEE_ADMIN_USER"),
			Password: os.Getenv("OONFEE_ADMIN_PASS"),
			Force:    true,
		}); uerr != nil {
			return 0, fmt.Errorf("could not clear the stale record for %s: %w", host, uerr)
		}
		res, aerr := d.Adopt(ctx, api.AdoptRequest{
			Host:     host,
			Username: os.Getenv("OONFEE_ADMIN_USER"),
			Password: os.Getenv("OONFEE_ADMIN_PASS"),
			Name:     "ap-" + strings.ReplaceAll(host, ".", "-"),
			Role:     string(model.RoleAP),
		})
		if aerr != nil {
			return 0, fmt.Errorf("re-adopting %s after a reset: %w", host, aerr)
		}
		t.Logf("re-adopted %s: %s (%s) class=%s", host, res.Name, res.Model, res.Class)
		t.Logf("  neighbor-report: %s", featureState(res, "neighbor-report"))
		return res.DeviceID, nil
	}
	return 0, err
}

func featureState(res *api.AdoptResult, name string) string {
	for _, f := range res.Features {
		if strings.Contains(f, name) {
			return f
		}
	}
	for _, f := range res.Unobservable {
		if strings.Contains(f, name) {
			return f + " (not observable)"
		}
	}
	return "absent"
}

// seedRoamingSite puts one 802.11k WLAN on both bands across both APs, reusing
// whatever is already there.
//
// Reuse rather than create, because VLAN id and SSID are both unique: a second
// run against a seeded data directory would otherwise fail on a database
// constraint rather than on anything about the feature under test. Shared with
// TestZZSetupRoaming so the verifier and the setup helper cannot describe two
// different site models.
func seedRoamingSite(ctx context.Context, t *testing.T, d *Daemon,
	deviceIDs []int64, ssid, key string) {
	t.Helper()

	site, err := d.Store.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	net := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	for i := range site.Networks {
		if site.Networks[i].VLAN == net.VLAN {
			net = &site.Networks[i]
		}
	}
	if net.ID == 0 {
		if err := d.Store.SaveNetwork(ctx, net); err != nil {
			t.Fatal(err)
		}
	}
	grp := &model.APGroup{Name: "all-aps", DeviceIDs: deviceIDs}
	for i := range site.Groups {
		if site.Groups[i].Name == grp.Name {
			grp = &site.Groups[i]
			grp.DeviceIDs = deviceIDs
		}
	}
	if err := d.Store.SaveGroup(ctx, grp); err != nil {
		t.Fatal(err)
	}
	w := &model.WLAN{
		SSID: ssid, NetworkID: net.ID, GroupID: grp.ID, Enabled: true,
		Bands:    []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecPSK2, Key: key, PMF: model.PMFOptional},
		// FT on WPA2-PSK needs the compatibility acknowledgment: it breaks some
		// older clients, which is why the renderer refuses it silently otherwise.
		Roaming: model.Roaming{FT: true, FTOverDS: true, KV: true, FTWithPSK2: true},
	}
	for i := range site.WLANs {
		if site.WLANs[i].SSID == ssid {
			w.ID = site.WLANs[i].ID
		}
	}
	if err := d.Store.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
}
