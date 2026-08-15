//go:build integration

package daemon

import (
	"context"
	"encoding/json"
	"os"
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
