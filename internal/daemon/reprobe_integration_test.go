//go:build integration

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// Re-probing a real device.
//
// Read-only: it uses the controller credential that already exists, so unlike
// the adoption test it writes nothing to the device and needs no extra opt-in.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt OONFEE_TEST_PASS=... \
//	go test -tags=integration ./internal/daemon/ -run TestIntegrationReprobe -v
//
// The assertion that matters is the second probe: probing the same unchanged
// device twice must report NO changes. A probe whose results are not stable
// across two runs would fill the event log with churn on every firmware
// upgrade, and — worse — the churn would look like the upgrade's doing.
func TestIntegrationReprobeIsStableAcrossRuns(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "60:38:e0:00:00:03"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	// First probe: no prior record, so everything is a first observation and
	// nothing is actionable. A device does not gain a radio by being looked at.
	first, err := d.Reprobe(ctx, dev.ID)
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	t.Logf("first probe: %s", first.Summary)
	t.Logf("  %d change(s), %d actionable", len(first.Changes), first.Actionable())
	for _, c := range first.Changes {
		if c.Effect != capability.EffectFirst {
			t.Errorf("against no prior record, %s reported %q, want %q",
				c.Name, c.Effect, capability.EffectFirst)
		}
	}
	if first.Actionable() != 0 {
		t.Errorf("the first probe reported %d actionable changes; there was "+
			"nothing to change from", first.Actionable())
	}
	if first.Registry.Board.Model == "" {
		t.Error("the probe returned no model")
	}

	// The record must have landed, or the next probe re-reports everything.
	stored, err := d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CapsJSON == "" || stored.CapsJSON == "{}" {
		t.Fatal("the probe did not store a capability record")
	}
	if stored.FWRelease != first.Registry.Board.Release {
		t.Errorf("firmware column = %q, probe read %q — a mismatch re-triggers "+
			"the automatic probe on every poll",
			stored.FWRelease, first.Registry.Board.Release)
	}

	// Second probe of an unchanged device: the important one.
	second, err := d.Reprobe(ctx, dev.ID)
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if !second.Unchanged {
		var blob, _ = json.Marshal(second.Changes)
		t.Errorf("probing an unchanged device reported %d change(s): %s\n"+
			"A probe that is not stable across runs turns every firmware "+
			"upgrade into a page of spurious capability churn",
			len(second.Changes), blob)
	}
	t.Logf("second probe: unchanged=%v", second.Unchanged)

	// And the diff detects a real difference when there is one. Constructed
	// rather than waiting for a firmware upgrade, but against the registry the
	// hardware actually returned, so the field-by-field comparison is exercised
	// on real shapes rather than on a hand-written fixture.
	if len(first.Registry.Radios) > 0 {
		fewer := *first.Registry
		fewer.Radios = fewer.Radios[:len(fewer.Radios)-1]
		changes := capability.Diff(first.Registry, &fewer)
		if len(changes) != 1 || changes[0].Effect != capability.EffectLost {
			t.Errorf("removing a radio from a real registry produced %v", changes)
		}
	}
}

// The whole chain, against the real device: a capability is lost, a WLAN stops
// rendering, and the preview says the two might be related.
//
// This is what the re-probe work is for. Detecting a loss and logging it is
// only half — an operator reads the preview, sees "device has no 6 GHz radio",
// and has no way to tell a band they picked wrongly from hardware that failed.
// The link is offered as a possibility, never asserted: the preview knows both
// facts and does not know they are the same fact.
func TestIntegrationPreviewExplainsItselfAfterACapabilityLoss(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "60:38:e0:00:00:04"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		Role: "ap", AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Reprobe(ctx, dev.ID); err != nil {
		t.Fatalf("probe: %v", err)
	}

	// A WLAN on a band this device does not have, so the preview omits it. The
	// WRT3200ACM is 2.4/5 GHz; 6 GHz is the reliable miss.
	group := &model.APGroup{Name: "all", DeviceIDs: []int64{dev.ID}}
	if err := d.Store.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	// VLAN 1 is the device's existing LAN, which the renderer leaves alone —
	// so this attaches the WLAN to the network without proposing any wired
	// change to a device that is not ours to reconfigure.
	network := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	wlan := &model.WLAN{
		SSID: "oonfee-6g-probe", GroupID: group.ID, NetworkID: network.ID,
		Enabled:  true,
		Bands:    []model.Band{model.Band6G},
		Security: model.Security{Mode: model.SecPSK2, Key: "a-passphrase-1234"},
	}
	if err := d.Store.SaveWLAN(ctx, wlan); err != nil {
		t.Fatal(err)
	}

	// No capability history yet: the omission stands on its own.
	res, err := d.Preview(ctx)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("devices = %+v, want one row", res.Devices)
	}
	row := res.Devices[0]
	if row.Error != "" {
		t.Fatalf("the device could not be planned: %s", row.Error)
	}
	if len(row.Omitted) == 0 {
		t.Fatalf("a 6 GHz WLAN on a 2.4/5 GHz device produced no omission: %+v", row)
	}
	t.Logf("omitted: %v", row.Omitted)
	if row.CapabilityCause != nil {
		t.Errorf("a cause was offered with no capability history: %+v",
			row.CapabilityCause)
	}

	// Now record a real loss, as a re-probe would.
	id := dev.ID
	if err := d.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "device", Severity: "warning",
		Event: EventCapabilitiesChanged,
		Detail: map[string]any{
			"actionable": 1,
			"changes": []map[string]any{{
				"effect": string(capability.EffectLost),
				"detail": "radio phy2 is gone. WLANs targeted at its band will " +
					"not render on this device",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	res, err = d.Preview(ctx)
	if err != nil {
		t.Fatalf("second preview: %v", err)
	}
	row = res.Devices[0]
	if row.CapabilityCause == nil {
		t.Fatal("the preview still could not explain the omission after a " +
			"capability loss was recorded")
	}
	if len(row.CapabilityCause.Changes) != 1 {
		t.Errorf("cause = %+v, want the one actionable change",
			row.CapabilityCause.Changes)
	}
	t.Logf("cause offered: %v", row.CapabilityCause.Changes)
}

// roleFit against a registry the hardware actually produced.
//
// The unit tests build registries by hand, which proves the branching and not
// the premise: that `len(Radios)` and FeatSurvey mean on a real device what the
// code assumes. This runs each role past what the reference AP genuinely
// reported.
func TestIntegrationRoleFitAgainstRealHardware(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	const mac = "60:38:e0:00:00:05"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		Role: string(model.RoleAP), AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	res, err := d.Reprobe(ctx, dev.ID)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	caps := res.Registry
	t.Logf("real registry: %d radio(s), survey=%s, ports bridge=%q wan=%q",
		len(caps.Radios), caps.State(capability.FeatSurvey),
		caps.Ports.Bridge, caps.Ports.WAN)

	// This device has radios, so an AP role fits and must produce no noise. A
	// warning on the ordinary case teaches operators to ignore warnings.
	if got := roleFit(model.RoleAP, caps); len(got) != 0 {
		t.Errorf("an access point with %d radios warned: %v", len(caps.Radios), got)
	}
	// Adopted as a switch it must say plainly that nothing will broadcast —
	// which is the whole reason the note exists.
	if got := roleFit(model.RoleSwitch, caps); len(got) != 1 {
		t.Errorf("a switch with %d radios produced %v, want one note",
			len(caps.Radios), got)
	}
	// The reprobe result carries it, so the device screen can show it without
	// a second call.
	if len(res.RoleFit) != 0 {
		t.Errorf("the AP-role probe carried role-fit warnings: %v", res.RoleFit)
	}
}

// A mesh backhaul, previewed against the real device.
//
// PREVIEW ONLY, deliberately. Applying an 802.11s interface to the reference
// device would write wireless config to a router that is the path everything
// else takes, and a mesh with one node is meaningless anyway — there is nothing
// to peer with. What is worth checking on hardware is that the plan the
// controller would send is built from what the device actually reported: its
// real radios, its real wpad build, its real existing config.
func TestIntegrationMeshPreviewsAgainstRealHardware(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	const mac = "60:38:e0:00:00:06"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		Role: string(model.RoleAP), AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	res, err := d.Reprobe(ctx, dev.ID)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := res.Registry.State(capability.FeatMesh); got != capability.Present {
		t.Skipf("this device reports mesh %s; the preview assertions below "+
			"assume a mesh-capable build", got)
	}

	group := &model.APGroup{Name: "all", DeviceIDs: []int64{dev.ID}}
	if err := d.Store.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	network := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	mesh := &model.Mesh{
		MeshID: "oonfee-backhaul", NetworkID: network.ID, GroupID: group.ID,
		Band: model.Band5G, Key: "a-mesh-passphrase", Enabled: true,
	}
	if err := d.Store.SaveMesh(ctx, mesh); err != nil {
		t.Fatal(err)
	}

	prev, err := d.Preview(ctx)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(prev.Devices) != 1 {
		t.Fatalf("devices = %+v", prev.Devices)
	}
	row := prev.Devices[0]
	if row.Error != "" {
		t.Fatalf("could not plan: %s", row.Error)
	}
	t.Logf("planned %d change(s); omitted %v", len(row.Changes), row.Omitted)

	var meshSection string
	for _, c := range row.Changes {
		if c.Config == "wireless" && strings.Contains(c.Section, "_mesh") {
			meshSection = c.Section
			// The passphrase must never appear in a preview — the same rule the
			// WLAN path follows, for a screen that ends up in screenshots.
			for _, o := range c.Options {
				if o == "key" {
					t.Error("the preview lists the mesh passphrase key by name")
				}
			}
			if !c.TouchesKey {
				t.Error("the preview does not flag that this change writes a key")
			}
		}
	}
	if meshSection == "" {
		t.Fatalf("no mesh section was planned against real hardware: %+v", row)
	}
	t.Logf("mesh section: %s", meshSection)

	// Nothing was written. The device must be untouched by a preview — that is
	// the entire contract of the pending-changes flow.
	if len(row.Drift) != 0 {
		t.Errorf("preview reported drift on an untouched device: %v", row.Drift)
	}
}
