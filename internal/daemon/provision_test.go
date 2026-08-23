package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func boundApplyRequest(t testing.TB, d *Daemon, ctx context.Context,
	req api.ApplyRequest) api.ApplyRequest {
	t.Helper()
	preview, err := d.Preview(ctx)
	if err != nil {
		t.Fatalf("preview before apply: %v", err)
	}
	req.PreviewToken = preview.PreviewToken
	return req
}

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

func TestPreviewBlocksCorruptGatewayFunctionsBeforeDeviceContact(t *testing.T) {
	dev := &store.Device{
		ID: 9, Name: "corrupt-gateway", Host: "127.0.0.1", Port: 1,
		Role: "gateway", Functions: []string{},
		FunctionError: "stored device functions are invalid",
	}
	p := (&Daemon{}).previewDevice(context.Background(), model.Site{}, dev)
	if p.Error == "" {
		t.Fatal("corrupt function set reached planning")
	}
	if p.Role != "gateway" || p.Functions == nil || len(p.Functions) != 0 {
		t.Fatalf("preview widened/relabeled corrupt row: role=%q functions=%v", p.Role, p.Functions)
	}
	if len(p.Changes) != 0 {
		t.Fatalf("corrupt function set produced changes: %+v", p.Changes)
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

func TestConvergedApplyRepairsOwnedLedger(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	d := openDaemon(t)
	dev := seedAP(t, d, "02:00:00:00:0c:01", "ap-one", addr, capability.Present)

	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	caps.Radios = []capability.Radio{{
		Device: "wlan1", Phy: "phy1", Frequency: 2437,
		Hardware: "Generic MAC80211",
	}}
	if err := d.Store.SetCapabilities(ctx, dev.ID, caps, string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	network := &model.Network{
		Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Zone: "lan", Enabled: true,
	}
	if err := d.Store.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	group := &model.APGroup{Name: "all", DeviceIDs: []int64{dev.ID}}
	if err := d.Store.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	wlan := &model.WLAN{
		SSID: "owned-ledger-test", NetworkID: network.ID, GroupID: group.ID,
		Bands: []model.Band{model.Band2G}, Security: model.Security{
			Mode: model.SecPSK2, Key: "test-passphrase",
		}, Enabled: true,
	}
	if err := d.Store.SaveWLAN(ctx, wlan); err != nil {
		t.Fatal(err)
	}
	site, err := d.Store.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Put the desired section on the mock once, bypassing daemon.applyDevice's
	// runtime health gate. The second call below is the path under test: the
	// device is already converged, but its durable ownership ledger is missing.
	c, err := d.Connect(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	r := reconcile.New(d.Store)
	plan, err := r.PlanDevice(ctx, c, site,
		model.Device{ID: dev.ID, Name: dev.Name, Role: model.RoleAP}, caps)
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	if plan.Empty() || plan.Blocked() {
		c.Close()
		t.Fatalf("fixture did not produce an applicable plan: empty=%v blocked=%v",
			plan.Empty(), plan.Blocked())
	}
	if _, err := r.Apply(ctx, c, dev.ID, plan, nil); err != nil {
		c.Close()
		t.Fatal(err)
	}
	c.Close()
	if _, err := d.Store.SQL().ExecContext(ctx,
		`DELETE FROM owned_sections WHERE device_id=?`, dev.ID); err != nil {
		t.Fatal(err)
	}

	dev, err = d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := d.applyDevice(ctx, site, dev, false)
	if got.Outcome != string(applyengine.Applied) || got.Changes != 0 {
		t.Fatalf("converged apply = %+v, want applied with zero device changes", got)
	}
	owned, err := d.Store.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) == 0 {
		t.Fatal("converged apply left the ownership ledger empty; un-adopt would not clean up the controller's section")
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
	if !strings.Contains(err.Error(), "this change removed") {
		t.Errorf("the failure does not say what is wrong: %v", err)
	}
	// And it must not overstate what it checked: this reads hostapd, not the
	// air, and §0 is fourteen hours of those two disagreeing.
	if !strings.Contains(err.Error(), "not the air") {
		t.Errorf("the failure claims more than it verified: %v", err)
	}
}

// A RENAME makes the same claim as a delete: after this applies, the old name
// is not on the air.
//
// Checking only delete ops left a renamed WLAN able to go on broadcasting its
// old SSID while the controller recorded the new one — §0's failure with an
// extra step, and the harder one to notice because the new SSID does appear.
func TestHealthCheckVerifiesARenamedSSIDStopped(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	c := ubus.New(ubus.Options{Host: addr})
	if err := c.Login(ctx, "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer c.Close()

	onAir := readSSIDs(ctx, c)
	var old string
	for s := range onAir {
		old = s
		break
	}
	if old == "" {
		t.Skip("the mock is broadcasting nothing to rename")
	}

	// Rename the section's SSID rather than deleting the section. The new name
	// is not on the air either, but the gate must fail on the OLD one still
	// being there — that is the half a delete-only check missed.
	plan := &reconcile.DevicePlan{
		Existing: render.NewExisting(map[string]map[string]map[string]string{
			"wireless": {"default_radio0": {".type": "wifi-iface", "ssid": old}},
		}),
		Plan: applyengine.Plan{Ops: []applyengine.Op{{
			Kind: applyengine.OpSet, Config: "wireless", Section: "default_radio0",
			Values: map[string]string{"ssid": "renamed-to-this"},
		}}},
	}

	err := healthCheck(plan)(ctx, c)
	if err == nil {
		t.Fatalf("the gate passed a rename while %q is still being broadcast", old)
	}
	if !strings.Contains(err.Error(), "this change removed") {
		t.Errorf("the failure blames the wrong thing: %v", err)
	}
}

func TestFleetApplyStopsAfterAppliedDeviceOwnershipLedgerFailure(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	d.Config.ApplyDrain = applyengine.MinApplyBudget() + 5*time.Second
	first := seedAP(t, d, "02:00:00:00:0d:01", "ap-one", startMock(t), capability.Present)
	second := seedAP(t, d, "02:00:00:00:0d:02", "ap-two", startMock(t), capability.Present)

	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	caps.Radios = []capability.Radio{{
		Device: "wlan1", Phy: "phy1", Frequency: 2437,
		Hardware: "Generic MAC80211",
	}}
	for _, dev := range []*store.Device{first, second} {
		if err := d.Store.SetCapabilities(ctx, dev.ID, caps, string(capability.ClassA)); err != nil {
			t.Fatal(err)
		}
		// Keep the mock's runtime OpenWrt SSID for the health check, but remove
		// its foreign UCI section so the controller can create its own section
		// with that SSID without an ownership conflict.
		c, err := d.Connect(ctx, dev)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Call(ctx, "uci", "delete", map[string]any{
			"config": "wireless", "section": "default_radio0",
		}, nil); err != nil {
			c.Close()
			t.Fatal(err)
		}
		if err := c.Call(ctx, "uci", "commit", map[string]any{"config": "wireless"}, nil); err != nil {
			c.Close()
			t.Fatal(err)
		}
		c.Close()
	}

	network := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	group := &model.APGroup{Name: "all", DeviceIDs: []int64{first.ID, second.ID}}
	if err := d.Store.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	wlan := &model.WLAN{
		SSID: "OpenWrt", NetworkID: network.ID, GroupID: group.ID,
		Bands: []model.Band{model.Band2G}, Security: model.Security{
			Mode: model.SecPSK2, Key: "test-passphrase",
		}, Enabled: true,
	}
	if err := d.Store.SaveWLAN(ctx, wlan); err != nil {
		t.Fatal(err)
	}

	preview, err := d.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Devices) != 2 || len(preview.Devices[0].Changes) == 0 ||
		len(preview.Devices[1].Changes) == 0 {
		t.Fatalf("fixture preview = %+v", preview.Devices)
	}
	if _, err := d.Store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER reject_first_ownership
		BEFORE INSERT ON owned_sections WHEN NEW.device_id = %d BEGIN
			SELECT RAISE(ABORT, 'synthetic fleet ownership ledger failure');
		END`, first.ID)); err != nil {
		t.Fatal(err)
	}

	result, err := d.ApplySite(ctx, api.ApplyRequest{PreviewToken: preview.PreviewToken})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || !result.Aborted || result.AbortedAfter != first.Name {
		t.Fatalf("fleet result = %+v", result)
	}
	got := result.Devices[0]
	if got.DeviceID != first.ID || got.Outcome != "error" || got.Changes == 0 ||
		!strings.Contains(got.Reason, "device outcome was applied") ||
		!strings.Contains(got.Reason, "ownership recording failed") ||
		!strings.Contains(got.Reason, "synthetic fleet ownership ledger failure") {
		t.Fatalf("first device result = %+v", got)
	}

	ownedOnDevice := func(dev *store.Device) int {
		t.Helper()
		c, err := d.Connect(ctx, dev)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var out struct {
			Values map[string]map[string]any `json:"values"`
		}
		if err := c.Call(ctx, "uci", "get", map[string]any{"config": "wireless"}, &out); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, values := range out.Values {
			if fmt.Sprint(values[render.OwnershipTag]) == "1" {
				n++
			}
		}
		return n
	}
	if n := ownedOnDevice(first); n == 0 {
		t.Fatal("the first router was not configured before its ownership ledger failed")
	}
	if n := ownedOnDevice(second); n != 0 {
		t.Fatalf("later router has %d controller sections; fleet apply did not abort", n)
	}
	if owned, err := d.Store.OwnedSections(ctx, first.ID); err != nil || len(owned) != 0 {
		t.Fatalf("failed ledger contains %+v, err=%v", owned, err)
	}

	events, err := d.Store.DeviceEvents(ctx, first.ID, "config.apply", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Severity != "error" ||
		!strings.Contains(string(fmt.Appendf(nil, "%s", events[0].Detail)),
			"synthetic fleet ownership ledger failure") {
		t.Fatalf("first-device audit = %+v", events)
	}
	secondEvents, err := d.Store.DeviceEvents(ctx, second.ID, "config.apply", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondEvents) != 0 {
		t.Fatalf("later device was applied/audited after abort: %+v", secondEvents)
	}
}
