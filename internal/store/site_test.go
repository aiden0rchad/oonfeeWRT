package store

import (
	"context"
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
