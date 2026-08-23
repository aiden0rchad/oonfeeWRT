package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func seedSite(t *testing.T, db *DB) (netID, groupID int) {
	t.Helper()
	ctx := context.Background()
	n := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Zone: "lan", Enabled: true}
	if err := db.SaveNetwork(ctx, n); err != nil {
		t.Fatal(err)
	}
	g := &model.APGroup{Name: "All APs"}
	if err := db.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	return n.ID, g.ID
}

func TestMissingNetworksAndGroupsReturnNotFound(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"update network": func() error {
			return db.SaveNetwork(ctx, &model.Network{ID: 999, Name: "gone", VLAN: 9})
		},
		"delete network": func() error { return db.DeleteNetwork(ctx, 999) },
		"update group": func() error {
			return db.SaveGroup(ctx, &model.APGroup{ID: 999, Name: "gone"})
		},
		"delete group": func() error { return db.DeleteGroup(ctx, 999) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

// The site UUID seeds the mobility-domain derivation, so every AP computes the
// same 802.11r domain without coordination. If it changed, roaming would be
// silently re-keyed fleet-wide and fast transition would break until every
// device was re-applied.
func TestSiteUUIDIsStableAcrossReads(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	first, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.UUID == "" {
		t.Fatal("no site UUID was generated")
	}
	for i := 0; i < 3; i++ {
		again, err := db.Site(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if again.UUID != first.UUID {
			t.Fatalf("UUID changed between reads: %q then %q", first.UUID, again.UUID)
		}
	}
	// Renaming must not disturb it either.
	if err := db.SetSiteName(ctx, "Home"); err != nil {
		t.Fatal(err)
	}
	after, _ := db.Site(ctx)
	if after.UUID != first.UUID {
		t.Errorf("renaming the site changed its UUID: %q -> %q", first.UUID, after.UUID)
	}
	if after.Name != "Home" {
		t.Errorf("name = %q, want Home", after.Name)
	}
}

// Saving a WLAN without re-sending its key must not wipe the key.
//
// This is what lets a screen edit an SSID, a band or a roaming toggle without
// ever holding the passphrase. Get it wrong and every unrelated edit silently
// turns a secured network open at the next apply.
func TestSavingAWLANWithoutTheKeyKeepsIt(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)

	w := &model.WLAN{
		SSID: "Home", NetworkID: netID, GroupID: groupID,
		Bands:    []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecSAEMixed, Key: "correct-horse", PMF: model.PMFOptional},
		Enabled:  true,
	}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}

	// An edit that touches everything except the key.
	edit := *w
	edit.SSID = "Home Renamed"
	edit.Security.Key = ""
	if err := db.SaveWLAN(ctx, &edit); err != nil {
		t.Fatal(err)
	}

	got := wlanByID(t, db, w.ID)
	if got.Security.Key != "correct-horse" {
		t.Errorf("key = %q after an edit that omitted it, want it preserved — "+
			"otherwise every unrelated edit turns the network open", got.Security.Key)
	}
	if got.SSID != "Home Renamed" {
		t.Errorf("ssid = %q, want the edit to have landed", got.SSID)
	}
}

func TestChangingTheKeyStillWorks(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "Home", NetworkID: netID, GroupID: groupID,
		Bands:    []model.Band{model.Band2G},
		Security: model.Security{Mode: model.SecSAE, Key: "old"}, Enabled: true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.Security.Key = "new"
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := wlanByID(t, db, w.ID); got.Security.Key != "new" {
		t.Errorf("key = %q, want new", got.Security.Key)
	}
}

func TestBandsRoundTripAndDeduplicate(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "Home", NetworkID: netID, GroupID: groupID,
		// Duplicated and out of order, as a careless caller might send.
		Bands:    []model.Band{model.Band5G, model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecNone}, Enabled: true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	got := wlanByID(t, db, w.ID)
	if len(got.Bands) != 2 {
		t.Fatalf("bands = %v, want two distinct", got.Bands)
	}
	if got.Bands[0] != model.Band2G || got.Bands[1] != model.Band5G {
		t.Errorf("bands = %v, want a stable sorted order so a no-op save is a no-op", got.Bands)
	}
}

// A row whose JSON will not parse must fail the load rather than becoming a
// WLAN with default (open) security.
func TestUnreadableSecurityFailsTheLoad(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO wlans (id, ssid, network_id, group_id, bands, security_json, enabled)
		 VALUES (1,'Broken',?,?,'2g','{not json',1)`, netID, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Site(ctx); err == nil {
		t.Error("a WLAN with unreadable security loaded silently; defaulting it " +
			"would publish an open network under a name people trust")
	}
}

// Deleting a referenced network or group must say what is in the way.
func TestDeletingAReferencedNetworkIsRefusedWithAReason(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "Home", NetworkID: netID, GroupID: groupID,
		Bands: []model.Band{model.Band2G}, Security: model.Security{Mode: model.SecNone}}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNetwork(ctx, netID); err == nil {
		t.Error("deleted a network a WLAN still points at")
	}
	if err := db.DeleteGroup(ctx, groupID); err == nil {
		t.Error("deleted a group a WLAN still targets")
	}
	if err := db.DeleteWLAN(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNetwork(ctx, netID); err != nil {
		t.Errorf("still refused after the WLAN was removed: %v", err)
	}
}

// Group membership is replaced wholesale, and a device listed twice is one
// member — the renderer fans out per membership, so a duplicate would render a
// device's sections twice.
func TestGroupMembershipReplacesAndDeduplicates(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	// Devices have to exist: the membership table has a foreign key, which is
	// the thing stopping a group from referring to hardware that is gone.
	for _, mac := range []string{"aa:aa:aa:aa:aa:01", "aa:aa:aa:aa:aa:02"} {
		d := &Device{MAC: mac, Host: "10.0.0.1", Name: mac}
		if err := db.UpsertDevice(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	devs, _ := db.Devices(ctx)
	g := &model.APGroup{Name: "All", DeviceIDs: []int64{
		devs[0].ID, devs[1].ID, devs[0].ID, // a duplicate
	}}
	if err := db.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	site, _ := db.Site(ctx)
	if len(site.Groups) != 1 || len(site.Groups[0].DeviceIDs) != 2 {
		t.Fatalf("members = %v, want two distinct", site.Groups)
	}

	// Replacing membership removes what is no longer listed.
	g.DeviceIDs = []int64{devs[1].ID}
	if err := db.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	site, _ = db.Site(ctx)
	if len(site.Groups[0].DeviceIDs) != 1 || site.Groups[0].DeviceIDs[0] != devs[1].ID {
		t.Errorf("members = %v, want only device %d", site.Groups[0].DeviceIDs, devs[1].ID)
	}
}

// A loaded site must satisfy the model's own validation, or the renderer will
// produce confusing per-device failures instead of one clear site-level error.
func TestLoadedSiteValidates(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "Home", NetworkID: netID, GroupID: groupID,
		Bands:    []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecSAEMixed, Key: "hunter2hunter2"},
		Enabled:  true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if errs := site.Validate(); len(errs) > 0 {
		t.Errorf("a freshly saved site does not validate: %v", errs)
	}
}

func wlanByID(t *testing.T, db *DB, id int) model.WLAN {
	t.Helper()
	site, err := db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range site.WLANs {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("WLAN %d not found", id)
	return model.WLAN{}
}

// An empty passphrase on update preserves the stored one.
//
// The same rule SaveWLAN follows, and for a sharper reason here: the API never
// sends a mesh passphrase back out, so a client that read a mesh and wrote it
// back would silently convert an encrypted mesh into an OPEN one — joinable by
// anyone in radio range, with access to the network behind it.
func TestSaveMeshPreservesTheKeyWhenNoneIsSupplied(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	net := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := db.SaveNetwork(ctx, net); err != nil {
		t.Fatal(err)
	}
	grp := &model.APGroup{Name: "all"}
	if err := db.SaveGroup(ctx, grp); err != nil {
		t.Fatal(err)
	}

	m := &model.Mesh{MeshID: "backhaul", NetworkID: net.ID, GroupID: grp.ID,
		Band: model.Band5G, Key: "a-mesh-passphrase", Enabled: true}
	if err := db.SaveMesh(ctx, m); err != nil {
		t.Fatal(err)
	}
	if m.ID == 0 {
		t.Fatal("insert did not populate the ID")
	}

	// A write-back with no key: rename only.
	edit := &model.Mesh{ID: m.ID, MeshID: "backhaul-2", NetworkID: net.ID,
		GroupID: grp.ID, Band: model.Band5G, Enabled: true}
	if err := db.SaveMesh(ctx, edit); err != nil {
		t.Fatal(err)
	}
	got := meshByID(t, db, m.ID)
	if got.Key != "a-mesh-passphrase" {
		t.Errorf("key = %q after a keyless update; the mesh silently became open",
			got.Key)
	}
	if got.MeshID != "backhaul-2" {
		t.Errorf("mesh ID = %q, want the edit to land", got.MeshID)
	}
	if got.Open() {
		t.Error("Open() reports true for a mesh that still has a passphrase")
	}
}

// A mesh reaches only the devices in its group, like a WLAN.
func TestMeshesForFollowsGroupMembership(t *testing.T) {
	site := model.Site{
		Groups: []model.APGroup{{ID: 1, Name: "edge", DeviceIDs: []int64{7}}},
		Meshes: []model.Mesh{
			{ID: 1, MeshID: "backhaul", GroupID: 1, Band: model.Band5G, Enabled: true},
			{ID: 2, MeshID: "disabled", GroupID: 1, Band: model.Band5G},
			{ID: 3, MeshID: "elsewhere", GroupID: 2, Band: model.Band5G, Enabled: true},
		},
	}
	got := site.MeshesFor(7)
	if len(got) != 1 || got[0].MeshID != "backhaul" {
		t.Errorf("MeshesFor(7) = %+v, want only the enabled in-group mesh", got)
	}
	if len(site.MeshesFor(9)) != 0 {
		t.Error("a device in no group received a mesh")
	}
}

func meshByID(t *testing.T, db *DB, id int) model.Mesh {
	t.Helper()
	s, err := db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Meshes {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("mesh %d not found", id)
	return model.Mesh{}
}

// AllowUplink rides options_json rather than a column of its own.
//
// Worth pinning: a column was written and then removed, because WLANOptions is
// already persisted whole and a second home for the same fact is how two
// sources of one truth start disagreeing. This asserts the round trip AND the
// default, since every WLAN that existed before the flag has no such key in its
// stored JSON and must come back false — a network nobody asked to accept
// wireless bridges does not accept them.
func TestAllowUplinkRoundTripsAndDefaultsOff(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	netID, groupID := seedSite(t, db)

	plain := &model.WLAN{SSID: "plain", NetworkID: netID, GroupID: groupID,
		Bands: []model.Band{model.Band5G}, Enabled: true}
	if err := db.SaveWLAN(ctx, plain); err != nil {
		t.Fatal(err)
	}
	bridged := &model.WLAN{SSID: "bridged", NetworkID: netID, GroupID: groupID,
		Bands: []model.Band{model.Band5G}, Enabled: true,
		Options: model.WLANOptions{AllowUplink: true}}
	if err := db.SaveWLAN(ctx, bridged); err != nil {
		t.Fatal(err)
	}

	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range site.WLANs {
		switch w.SSID {
		case "plain":
			if w.Options.AllowUplink {
				t.Error("a WLAN nobody asked to accept bridges came back accepting them")
			}
		case "bridged":
			if !w.Options.AllowUplink {
				t.Error("AllowUplink did not survive the round trip")
			}
		}
	}
}

// One wireless uplink per device, enforced by the schema rather than by a
// check-then-insert that two concurrent writers could both pass.
//
// The constraint is not arbitrary tidiness: a device with two uplinks bridges
// the same network to itself twice, which is a layer-2 loop. The error has to
// say that, because "UNIQUE constraint failed: uplinks.device_id" tells an
// operator nothing they can act on.
func TestOneUplinkPerDeviceAndTheErrorSaysWhy(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	netID, groupID := seedSite(t, db)

	w := &model.WLAN{SSID: "roam", NetworkID: netID, GroupID: groupID,
		Bands: []model.Band{model.Band5G}, Enabled: true,
		Options: model.WLANOptions{AllowUplink: true}}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	// A real device row: uplinks reference devices, and that FK is doing real
	// work — an uplink naming a device that does not exist would render for
	// nobody and prune for nobody.
	at := int64(1)
	dev := &Device{MAC: "02:00:00:00:0e:01", Host: "192.168.1.9", Name: "no-cable",
		Scheme: "http", AdoptedAt: &at}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	first := &model.Uplink{DeviceID: dev.ID, WLANID: w.ID, Band: model.Band5G, Enabled: true}
	if err := db.SaveUplink(ctx, first); err != nil {
		t.Fatalf("first uplink: %v", err)
	}

	second := &model.Uplink{DeviceID: dev.ID, WLANID: w.ID, Band: model.Band2G, Enabled: true}
	err := db.SaveUplink(ctx, second)
	if err == nil {
		t.Fatal("a second uplink on one device was accepted; that bridges the " +
			"same network to itself twice")
	}
	if !strings.Contains(err.Error(), "loop") {
		t.Errorf("the refusal does not explain the hazard: %v", err)
	}

	// And it round-trips into the site model.
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Uplinks) != 1 || site.Uplinks[0].Band != model.Band5G {
		t.Fatalf("uplink did not survive the round trip: %+v", site.Uplinks)
	}
	if u, ok := site.UplinkFor(dev.ID); !ok || u.WLANID != w.ID {
		t.Errorf("UplinkFor did not find it: %+v %v", u, ok)
	}

	if err := db.DeleteUplink(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	site, _ = db.Site(ctx)
	if len(site.Uplinks) != 0 {
		t.Errorf("delete left %d uplinks", len(site.Uplinks))
	}
}

// A network created without a zone gets one of its own, not the device's.
//
// The default was "lan", and no screen ever set it, so every VLAN network the
// product could create asked for a second firewall zone named lan beside the
// device's own — which the renderer now refuses as config it does not own. The
// default path must not be the blocked one.
func TestNewNetworkGetsItsOwnFirewallZone(t *testing.T) {
	db := open(t)
	n := &model.Network{Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Enabled: true}
	if err := db.SaveNetwork(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if n.Zone == "lan" {
		t.Fatal("a new network defaulted into the device's own lan zone")
	}
	if n.Zone != "iot" {
		t.Errorf("zone = %q, want the network's own name", n.Zone)
	}
}

func TestNetworkDHCPSurvivesRoundTripAndOlderClientUpdate(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dhcp := model.DHCPConfig{Enabled: true, Start: 20, Limit: 80, LeaseTime: "30m"}
	n := &model.Network{Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24",
		DHCP: &dhcp, Enabled: true}
	if err := db.SaveNetwork(ctx, n); err != nil {
		t.Fatal(err)
	}

	// A pre-DHCP API client omits the new object on an unrelated edit. Nil means
	// preserve on update, not reset to the renderer's old constants.
	older := &model.Network{ID: n.ID, Name: "things", VLAN: 20,
		CIDR: "10.0.20.1/24", Zone: n.Zone, Enabled: true}
	if err := db.SaveNetwork(ctx, older); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Networks) != 1 || site.Networks[0].EffectiveDHCP() != dhcp {
		t.Fatalf("DHCP changed during an older-client update: %+v", site.Networks)
	}
}

func TestLegacyEmptyDHCPJSONLoadsHistoricalDefaults(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO networks (name, vlan, cidr, zone, dhcp_json, enabled)
		 VALUES ('legacy', 20, '10.0.20.1/24', 'legacy', ' { } ', 1)`); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := site.Networks[0].EffectiveDHCP(), model.DefaultDHCPConfig(); got != want {
		t.Fatalf("legacy DHCP = %+v, want %+v", got, want)
	}
	if !site.Networks[0].LegacyDHCPDefaults {
		t.Fatal("dhcp_json={} lost its legacy-default marker")
	}

	// An older client may rename this row without making a DHCP decision. The
	// marker must survive so an unrelated edit cannot bypass the apply block.
	legacy := site.Networks[0]
	legacy.Name = "renamed"
	legacy.DHCP = nil
	if err := db.SaveNetwork(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !site.Networks[0].LegacyDHCPDefaults {
		t.Fatal("older-client update cleared the legacy-default marker")
	}

	custom := model.DHCPConfig{Enabled: true, Start: 20, Limit: 80, LeaseTime: "30m"}
	legacy = site.Networks[0]
	legacy.DHCP = &custom
	if err := db.SaveNetwork(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if site.Networks[0].LegacyDHCPDefaults {
		t.Fatal("explicit DHCP choice did not clear the legacy-default marker")
	}
}
