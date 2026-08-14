package daemon

import (
	"context"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The gateway applies last.
//
// The controller's traffic to every other device traverses it, so applying to
// the gateway first and breaking it would strand the rest of the queue
// mid-apply — rollback timers armed on devices nobody can now reach to confirm.
// IMPLEMENTATION §6 states the rule; this is the rule as code.
func TestApplyOrderPutsGatewaysLast(t *testing.T) {
	devs := []*store.Device{
		{ID: 1, Name: "gw", Role: "gateway"},
		{ID: 2, Name: "ap-a", Role: "ap"},
		{ID: 3, Name: "sw", Role: "switch"},
		{ID: 4, Name: "ap-b", Role: "ap"},
	}
	var got []string
	for _, d := range applyOrder(devs) {
		got = append(got, d.Name)
	}
	if got[len(got)-1] != "gw" {
		t.Errorf("order = %v; the gateway must be last", got)
	}
	// Stable within a role, so two previews of the same fleet compare cleanly.
	want := []string{"ap-a", "sw", "ap-b", "gw"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order = %v, want %v", got, want)
			break
		}
	}
}

func TestApplyOrderWithSeveralGateways(t *testing.T) {
	devs := []*store.Device{
		{ID: 1, Name: "gw-a", Role: "gateway"},
		{ID: 2, Name: "ap", Role: "ap"},
		{ID: 3, Name: "gw-b", Role: "gateway"},
	}
	var got []string
	for _, d := range applyOrder(devs) {
		got = append(got, d.Name)
	}
	if got[0] != "ap" {
		t.Errorf("order = %v; every gateway goes after the APs", got)
	}
}

// A device with no capability record must be refused, not rendered with assumed
// capabilities: render gates options on what the probe found, and treating
// "unknown" as "everything works" sends a device options its hostapd rejects.
func TestDeviceWithoutCapabilitiesIsRefused(t *testing.T) {
	for _, caps := range []string{"", "{}"} {
		if _, err := deviceCaps(&store.Device{Name: "ap", CapsJSON: caps}); err == nil {
			t.Errorf("CapsJSON=%q was accepted; rendering against assumed "+
				"capabilities is how a device gets options it cannot take", caps)
		}
	}
	if _, err := deviceCaps(&store.Device{Name: "ap", CapsJSON: "{not json"}); err == nil {
		t.Error("an unreadable capability record was accepted")
	}
}

// A model-level mistake is reported once, not once per device.
func TestPreviewReportsSiteErrorsWithoutTouchingDevices(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// A WLAN that asks for a secured mode and has no passphrase — what a
	// half-finished settings screen leaves behind. The "unknown network"
	// case cannot be produced through the store at all: the foreign key
	// refuses it, so that arm of Site.Validate is defence in depth rather
	// than a reachable state.
	if _, err := d.Store.SQL().ExecContext(ctx,
		`INSERT INTO networks (id, name, vlan, cidr) VALUES (1,'lan',1,'192.168.1.1/24')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.SQL().ExecContext(ctx,
		`INSERT INTO ap_groups (id, name) VALUES (1,'All')`); err != nil {
		t.Fatal(err)
	}
	w := &model.WLAN{SSID: "Home", NetworkID: 1, GroupID: 1,
		Bands:    []model.Band{model.Band2G},
		Security: model.Security{Mode: model.SecSAEMixed}, Enabled: true}
	if err := d.Store.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	blob, err := d.Keys.SealCredential("aa:bb:cc:dd:ee:01", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	// Port 1: nothing listens there, so if the preview tried to contact it the
	// test would take the dial timeout instead of returning immediately.
	if err := d.Store.UpsertDevice(ctx, &store.Device{
		MAC: "aa:bb:cc:dd:ee:01", Host: "127.0.0.1", Port: 1, Name: "ap",
		Scheme: "http", AdoptedAt: &at, CredEnc: blob, CapsJSON: `{"Class":"A"}`,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := d.Preview(ctx)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(res.SiteErrors) == 0 {
		t.Fatal("a secured WLAN with no passphrase produced no site error")
	}
	if len(res.Devices) != 0 {
		t.Errorf("devices were planned despite a site-level error: %+v; the same "+
			"error would then appear once per device", res.Devices)
	}
}

// An unreachable device becomes a row that says so, not a failure that hides
// the rest of the fleet.
func TestPreviewReportsAnUnreachableDeviceAsARow(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	blob, err := d.Keys.SealCredential("aa:bb:cc:dd:ee:02", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	if err := d.Store.UpsertDevice(ctx, &store.Device{
		MAC: "aa:bb:cc:dd:ee:02", Host: "127.0.0.1", Port: 1, Name: "gone",
		Scheme: "http", AdoptedAt: &at, CredEnc: blob, CapsJSON: `{"Class":"A"}`,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := d.Preview(ctx)
	if err != nil {
		t.Fatalf("Preview returned an error instead of reporting the device: %v", err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("devices = %+v, want one row", res.Devices)
	}
	if res.Devices[0].Error == "" {
		t.Error("an unreachable device reported no error on its row")
	}
}
