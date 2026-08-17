package daemon

import (
	"context"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
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

// A change to network or firewall config needs an explicit acknowledgment.
//
// Those configs carry the path the controller reaches the device through. The
// rollback still protects the change — that is what applying with one armed is
// for — but an operator should be told they are editing the road before driving
// down it, rather than learning from a device that stopped answering.
func TestTraversalDetection(t *testing.T) {
	wireless := &reconcile.DevicePlan{Plan: applyengine.Plan{Ops: []applyengine.Op{
		{Kind: applyengine.OpAdd, Config: "wireless", Section: "oowrt_wlan1_radio0"},
	}}}
	if touchesTraversal(wireless) {
		t.Error("a wireless-only change was flagged as touching the management path")
	}

	for _, config := range []string{"network", "firewall"} {
		p := &reconcile.DevicePlan{Plan: applyengine.Plan{Ops: []applyengine.Op{
			{Kind: applyengine.OpAdd, Config: "wireless", Section: "oowrt_wlan1_radio0"},
			{Kind: applyengine.OpAdd, Config: config, Section: "oowrt_bv45"},
		}}}
		if !touchesTraversal(p) {
			t.Errorf("a change to %s was not flagged; it carries the path the "+
				"controller reaches the device through", config)
		}
	}

	// dhcp is not traversal: losing a lease server does not cut a controller
	// that reaches the device by its static address.
	dhcp := &reconcile.DevicePlan{Plan: applyengine.Plan{Ops: []applyengine.Op{
		{Kind: applyengine.OpAdd, Config: "dhcp", Section: "oowrt_dhcp_iot"},
	}}}
	if touchesTraversal(dhcp) {
		t.Error("a dhcp-only change was flagged; the controller reaches devices " +
			"by address, not by lease")
	}
}

// The health gate must verify a removal, not just an addition.
//
// It confirmed only that the SSIDs it WROTE had appeared, so an apply that
// deleted a WLAN passed on "the lan interface is up" and the controller
// recorded the network as gone. STATUS §0 is fourteen hours of exactly that: a
// device beaconing an SSID that was in no config anywhere, while uci, the
// hostapd conf, iwinfo, ubus and `iw dev info` all agreed it was not.
func TestStillOnCatchesAnSSIDThatDidNotStop(t *testing.T) {
	onAir := map[string]bool{"kept": true, "removed-but-still-up": true}

	if got := stillOn(onAir, map[string]bool{"removed-but-still-up": true}); len(got) != 1 {
		t.Errorf("an SSID the change removed is still on the air and was not "+
			"reported: %v", got)
	}
	// One that genuinely stopped is not reported.
	if got := stillOn(onAir, map[string]bool{"actually-gone": true}); len(got) != 0 {
		t.Errorf("reported an SSID that is not being broadcast: %v", got)
	}
	// And the two halves read the same snapshot, so a change that both adds and
	// removes is judged on one view of the air.
	if got := missingFrom(onAir, map[string]bool{"kept": true}); len(got) != 0 {
		t.Errorf("an SSID that IS on the air was reported missing: %v", got)
	}
}

// The gate must REJECT a removal the device did not honour, end to end.
//
// The unit test above covers stillOn in isolation, which would still pass if
// healthCheck never called it — the trap that has produced six empty tests this
// week. This builds a real plan with a delete op and runs the returned gate
// against a device that is still broadcasting the SSID.
func TestHealthCheckRejectsAnSSIDThatSurvivedItsDeletion(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	c := ubus.New(ubus.Options{Host: addr})
	if err := c.Login(ctx, "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer c.Close()

	// What the mock is actually broadcasting, so the test cannot drift from it.
	onAir := readSSIDs(ctx, c)
	var victim string
	for s := range onAir {
		victim = s
		break
	}
	if victim == "" {
		t.Skip("the mock is broadcasting nothing to remove")
	}

	// A plan that DELETES the section carrying it, and writes nothing.
	plan := &reconcile.DevicePlan{
		Existing: render.NewExisting(map[string]map[string]map[string]string{
			"wireless": {"default_radio0": {".type": "wifi-iface", "ssid": victim}},
		}),
		Plan: applyengine.Plan{Ops: []applyengine.Op{
			{Kind: applyengine.OpDelete, Config: "wireless", Section: "default_radio0"},
		}},
	}

	err := healthCheck(plan)(ctx, c)
	if err == nil {
		t.Fatalf("the gate passed a change that removed %q while the device is "+
			"still broadcasting it — the controller would record it as gone",
			victim)
	}
	if !strings.Contains(err.Error(), "still being broadcast") {
		t.Errorf("the failure does not say what is wrong: %v", err)
	}
}
