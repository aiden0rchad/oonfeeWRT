//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/meshlink"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Mesh backhaul health against the two real devices.
//
// This is as far as this feature can be verified on the hardware available,
// and the limit is worth stating rather than discovering: mesh is Present only
// on the Archer C6 and gated Absent on the WRT3200ACM (§5q), so a two-node mesh
// cannot be built here and NO peered state can ever be reached. What this does
// verify is everything up to that point, and it verifies the two ends that
// matter most:
//
//   - the WRT reaches `not-buildable` carrying the driver's own sentence,
//     which is the §5q gate working from the health side rather than the
//     render side;
//
//   - the C6 reaches a live interface state through the collector, so the
//     "costs no device request" claim is exercised against real polls rather
//     than asserted.
//
//     OONFEE_MESH_HEALTH=1 OONFEE_SEED_DIR=$PWD/.run OONFEE_SEED_PASSFILE=/path \
//     OONFEE_AP1=192.168.1.1 OONFEE_AP2=192.168.1.2 \
//     OONFEE_ADMIN_USER=root OONFEE_ADMIN_PASS= \
//     go test -tags=integration ./internal/daemon/ -run TestIntegrationMeshHealth -v
//
// It APPLIES a mesh to whichever device can carry one, and removes it again.
func TestIntegrationMeshHealth(t *testing.T) {
	if os.Getenv("OONFEE_MESH_HEALTH") != "1" {
		t.Skip("set OONFEE_MESH_HEALTH=1 to run the mesh health test")
	}
	ap1, ap2 := os.Getenv("OONFEE_AP1"), os.Getenv("OONFEE_AP2")
	if ap1 == "" || ap2 == "" {
		t.Skip("set OONFEE_AP1 and OONFEE_AP2")
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
	// Not `defer`: t.Cleanup runs after deferred calls, so a deferred Close
	// shuts the database before the cleanup that removes the test mesh can use
	// it. Registered first so it runs LAST.
	t.Cleanup(func() { _ = d.Close() })

	var ids []int64
	for _, host := range []string{ap1, ap2} {
		id, err := adoptOrReuse(ctx, t, d, host)
		if err != nil {
			t.Fatalf("adopt %s: %v", host, err)
		}
		ids = append(ids, id)
	}
	seedRoamingSite(ctx, t, d, ids, os.Getenv("OONFEE_WLAN_SSID"),
		os.Getenv("OONFEE_WLAN_KEY"))

	site, err := d.Store.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var grp model.APGroup
	for _, g := range site.Groups {
		if g.Name == "all-aps" {
			grp = g
		}
	}
	mesh := &model.Mesh{
		MeshID: "oonfee-health-mesh", NetworkID: site.Networks[0].ID,
		GroupID: grp.ID, Band: model.Band5G, Key: "mesh-health-check-8842",
		Enabled: true,
	}
	for _, m := range site.Meshes {
		if m.MeshID == mesh.MeshID {
			mesh.ID = m.ID
		}
	}
	if err := d.Store.SaveMesh(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Leave the fleet as it was found. The apply that removes the section
		// is the same path §5n covers against the mock; here it runs for real.
		if err := d.Store.DeleteMesh(context.Background(), mesh.ID); err != nil {
			t.Logf("could not delete the test mesh: %v", err)
			return
		}
		if _, err := d.ApplySite(context.Background(), api.ApplyRequest{}); err != nil {
			t.Logf("could not prune the test mesh from the devices: %v", err)
		}
	})

	// The collector starts BEFORE the apply, deliberately.
	//
	// It is what makes this a test of the fix rather than of a lucky ordering.
	// The interface list rides a 15-minute cadence, so a poll taken before the
	// mesh exists caches "this section has no interface" — the §5q signature —
	// and every reading for the next quarter of an hour reports a critical
	// fault that has already resolved. ApplySite invalidates that cache; with
	// the collector started afterwards, nothing would be invalidated and the
	// test would pass on a cache that was empty anyway.
	if err := d.StartCollector(ctx, collector.Options{
		Baseline: 3 * time.Second, Log: quietLogger(),
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second) // a poll, so there is a stale list to invalidate

	// The preview is where the §5q gate speaks first.
	prev, err := d.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range prev.Devices {
		for _, om := range row.Omitted {
			t.Logf("preview %s omitted: %s", row.Name, om)
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

	// A mesh takes a few seconds to come up, and an apply returning is not the
	// same as a radio being ready — §5r learned that by asserting too early.
	time.Sleep(15 * time.Second)

	links, err := d.MeshHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 {
		t.Fatalf("want one link per device in the group, got %d", len(links))
	}

	var sawNotBuildable, sawLive bool
	for _, l := range links {
		t.Logf("%s on device %d (%s): %s [%s] — %s",
			l.Name, l.DeviceID, l.Iface, l.State, l.Tone, l.Reason)

		if l.Reason == "" {
			t.Errorf("%s has no reason", l.State)
		}
		switch l.State {
		case meshlink.StateNotBuildable:
			sawNotBuildable = true
			// The driver's own sentence, shared with the apply preview.
			if !contains2(l.Reason, "will not run") {
				t.Errorf("not-buildable did not carry the driver reason: %q", l.Reason)
			}
			if l.Actionable() {
				t.Error("hardware that cannot do mesh is not an outage to fix")
			}
		case meshlink.StateNoPeers, meshlink.StatePeersNotCounted:
			// The expected landing state on the one device that CAN carry it:
			// applied, interface up, and nobody to peer with because the other
			// device in the group cannot run a mesh at all.
			sawLive = true
			if l.Iface == "" {
				t.Error("a live mesh reported no interface name")
			}
		case meshlink.StateInterfaceAbsent:
			t.Errorf("the mesh applied and its interface never appeared: %s", l.Reason)
		}
	}
	if !sawNotBuildable {
		t.Error("no device reported not-buildable; the §5q gate is the whole " +
			"reason this fleet can be used to test the ladder at all")
	}
	if !sawLive {
		t.Error("the mesh-capable device never reached a live state")
	}
}

func contains2(s, want string) bool {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
