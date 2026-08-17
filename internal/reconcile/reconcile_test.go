package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"

	_ "modernc.org/sqlite"
)

var mockAddr string

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mockAddr = fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("python3", filepath.Join(root, "tools", "mock_ubus.py"),
		"--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := waitReady(mockAddr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "mock not ready:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	os.Exit(code)
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("go.mod not found")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func dial(t *testing.T) *ubus.Client {
	t.Helper()
	c := ubus.New(ubus.Options{Host: mockAddr})
	if err := c.Login(context.Background(), "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite",
		filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func caps() *capability.Registry {
	r := capability.NewRegistry()
	r.Radios = []capability.Radio{
		{Device: "wlan0", Phy: "phy0", Frequency: 5180},
		{Device: "wlan1", Phy: "phy1", Frequency: 2412},
	}
	return r
}

// site builds a fixture whose AP group contains the given device. The store
// assigns device IDs, so the fixture has to follow rather than assume one.
func site(deviceID int64) model.Site { return siteWLAN(deviceID, 3) }

// siteWLAN varies the WLAN id so each test owns distinct section names. Every
// test in the package shares ONE mock process, so sections confirmed by an
// earlier test would otherwise be found by a later one and quietly change what
// it is testing.
func siteWLAN(deviceID int64, wlanID int) model.Site {
	s := baseSite(deviceID)
	s.WLANs[0].ID = wlanID
	// Distinct SSID too: a foreign section left behind by one test would
	// otherwise collide with another test's WLAN and block it.
	s.WLANs[0].SSID = fmt.Sprintf("Home%d", wlanID)
	return s
}

func baseSite(deviceID int64) model.Site {
	return model.Site{
		UUID:     "site-uuid-for-tests",
		Networks: []model.Network{{ID: 1, Name: "lan", VLAN: 1, Enabled: true}},
		Groups:   []model.APGroup{{ID: 1, DeviceIDs: []int64{deviceID}}},
		WLANs: []model.WLAN{{
			ID: 3, SSID: "Home", NetworkID: 1, GroupID: 1,
			Bands:    []model.Band{model.Band2G, model.Band5G},
			Security: model.Security{Mode: model.SecPSK2, Key: "s3cretpass"},
			Enabled:  true,
		}},
	}
}

func fastEngine() *applyengine.Engine {
	e := applyengine.New()
	e.ConfirmInterval = 150 * time.Millisecond
	e.RevertGrace = 800 * time.Millisecond
	return e
}

func newReconciler(t *testing.T) (*Reconciler, *store.DB) {
	db := openStore(t)
	r := New(db)
	r.Engine = fastEngine()
	return r, db
}

func device(t *testing.T, db *store.DB) *store.Device {
	t.Helper()
	d := &store.Device{MAC: "aa:bb:cc:00:00:07", Host: mockAddr, Name: "ap7"}
	if err := db.UpsertDevice(context.Background(), d); err != nil {
		t.Fatalf("device: %v", err)
	}
	return d
}

// The whole cycle: plan, apply, and only then claim ownership.
func TestApplyRecordsOwnershipOnlyAfterConfirmation(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if p.Blocked() {
		t.Fatalf("unexpected conflicts: %v", p.Report.Conflicts)
	}
	if p.Empty() {
		t.Fatal("a fresh device should need both wifi-ifaces")
	}

	// Nothing may be claimed before the apply lands.
	if owned, _ := db.OwnedSections(ctx, dev.ID); len(owned) != 0 {
		t.Fatalf("planning must not claim ownership, got %d", len(owned))
	}

	res, err := r.Apply(ctx, c, dev.ID, p, func(context.Context, *ubus.Client) error { return nil })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != applyengine.Applied {
		t.Fatalf("want Applied, got %s", res)
	}
	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatalf("OwnedSections: %v", err)
	}
	if len(owned) != 2 {
		t.Fatalf("both sections should be recorded as ours, got %d", len(owned))
	}
	for _, o := range owned {
		if o.RenderedHash == "" || o.AppliedAt == 0 {
			t.Errorf("ownership row is incomplete: %+v", o)
		}
	}
}

// A change the device reverts was never ours. Recording it would leave the
// reconciler believing it owns config that is not there.
func TestRevertedApplyRecordsNoOwnership(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, siteWLAN(dev.ID, 11), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	p.Plan.Timeout = 2 * time.Second

	res, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return errors.New("ssid not on air") })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != applyengine.Reverted {
		t.Fatalf("want Reverted, got %s", res)
	}
	if owned, _ := db.OwnedSections(ctx, dev.ID); len(owned) != 0 {
		t.Fatalf("a reverted change must claim nothing, got %d rows", len(owned))
	}
}

// Applying twice with no model change should be a no-op the second time —
// otherwise every poll would churn the device.
func TestSecondApplyIsANoOp(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	s := siteWLAN(dev.ID, 12)

	p, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p, nil); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	p2, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("second PlanDevice: %v", err)
	}
	if len(p2.Drift) != 0 {
		t.Errorf("nothing changed, so there should be no drift: %v", p2.Drift)
	}
	// The sections exist now, so the ops are updates rather than adds.
	for _, op := range p2.Plan.Ops {
		if op.Kind == applyengine.OpAdd {
			t.Errorf("re-planning an applied device should not re-add %s", op.Section)
		}
	}
}

// A human edited a section we own. Surface it; never silently correct it.
func TestDriftIsDetectedAndReportedNotCorrected(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	s := siteWLAN(dev.ID, 13)

	p, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Someone changes the SSID in LuCI on a section carrying our marker.
	if err := c.Call(ctx, "uci", "set", map[string]any{
		"config": "wireless", "section": "oowrt_wlan13_radio1",
		"values": map[string]string{"ssid": "HumanRenamedThis"}}, nil); err != nil {
		t.Fatalf("simulate human edit: %v", err)
	}
	if err := c.Call(ctx, "uci", "commit", map[string]any{"config": "wireless"}, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p2, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice after drift: %v", err)
	}
	if len(p2.Drift) == 0 {
		t.Fatal("an edit to a section we own must be detected as drift")
	}
	var found bool
	for _, d := range p2.Drift {
		if d.Option == "ssid" && d.Theirs == "HumanRenamedThis" && d.Ours == "Home13" {
			found = true
		}
	}
	if !found {
		t.Fatalf("drift should name the option and both values, got %v", p2.Drift)
	}
	// It is reported, not auto-fixed: the plan is still just the normal
	// reconciliation, and applying it is a decision the operator makes.
	if p2.Blocked() {
		t.Error("drift on our OWN section is not a conflict; it is ours to reconcile")
	}
}

// Drift must compare only the keys we manage. The device adds its own defaults
// and hostapd writes state back, and reporting those would train the operator
// to ignore drift entirely.
func TestDriftIgnoresKeysWeDoNotManage(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	s := siteWLAN(dev.ID, 14)

	p, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// A key we never set appears on the section — a device default, say.
	if err := c.Call(ctx, "uci", "set", map[string]any{
		"config": "wireless", "section": "oowrt_wlan13_radio1",
		"values": map[string]string{"some_device_default": "42"}}, nil); err != nil {
		t.Fatalf("add foreign key: %v", err)
	}
	if err := c.Call(ctx, "uci", "commit", map[string]any{"config": "wireless"}, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p2, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if len(p2.Drift) != 0 {
		t.Fatalf("a key we never set is not drift: %v", p2.Drift)
	}
}

// A conflict is a human owning something we would have to touch. Produce no
// operations at all — a partial apply around a conflict leaves half a WLAN.
func TestConflictBlocksTheDeviceEntirely(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	// A human's section already publishes our SSID on radio1.
	if err := c.Call(ctx, "uci", "set", map[string]any{
		"config": "wireless", "section": "default_radio1", "type": "wifi-iface",
		"values": map[string]string{"ssid": "Home15", "device": "radio1"}}, nil); err != nil {
		t.Fatalf("seed foreign section: %v", err)
	}
	if err := c.Call(ctx, "uci", "commit", map[string]any{"config": "wireless"}, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p, err := r.PlanDevice(ctx, c, siteWLAN(dev.ID, 15), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if !p.Blocked() {
		t.Fatal("a foreign section publishing our SSID must block the device")
	}
	if len(p.Plan.Ops) != 0 {
		t.Fatalf("a blocked device must produce NO operations, got %d", len(p.Plan.Ops))
	}
	if _, err := r.Apply(ctx, c, dev.ID, p, nil); err == nil {
		t.Fatal("Apply must refuse a blocked plan")
	}
}

// Every apply outcome must be accountable afterwards.
func TestEveryOutcomeIsAudited(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, siteWLAN(dev.ID, 11), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	p.Plan.Timeout = 2 * time.Second
	if _, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return errors.New("nope") }); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	events, err := db.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("an apply with no audit trail is unaccountable")
	}
	e := events[0]
	if e.Event != "config.apply" {
		t.Errorf("event = %q, want config.apply", e.Event)
	}
	if e.Severity != "warning" {
		t.Errorf("a reverted apply should be a warning, got %q", e.Severity)
	}
	if !strings.Contains(string(fmt.Appendf(nil, "%s", e.Detail)), "reverted") {
		t.Errorf("the detail should record the outcome: %v", e.Detail)
	}
}

// A device already matching the model needs no apply at all.
func TestNoOpPlanShortCircuits(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	empty := &DevicePlan{Device: model.Device{ID: dev.ID}}
	res, err := r.Apply(ctx, c, dev.ID, empty, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != applyengine.Applied {
		t.Fatalf("an empty plan is trivially applied, got %s", res)
	}
	if !strings.Contains(res.Reason, "nothing to do") {
		t.Errorf("the reason should say why: %q", res.Reason)
	}
}

// ReadExisting reads every config the renderer can write, and the WifiIfaces
// accessor narrows to AP interfaces.
//
// Reading exactly the managed set is what makes Prune safe: it removes owned
// sections the model no longer produces, so a config we render into but never
// read would leave orphans behind.
func TestReadExistingCoversEveryManagedConfig(t *testing.T) {
	ctx := context.Background()
	existing, err := ReadExisting(ctx, dial(t))
	if err != nil {
		t.Fatalf("ReadExisting: %v", err)
	}
	if _, ok := existing.Configs["wireless"]; !ok {
		t.Fatal("wireless was not read")
	}
	// Radios are wireless sections but not AP interfaces.
	if _, present := existing.In("wireless")["radio0"]; !present {
		t.Error("the raw wireless config should contain the radio sections")
	}
	for name, vals := range existing.WifiIfaces() {
		if vals[".type"] != "wifi-iface" {
			t.Errorf("%s is not a wifi-iface: %v", name, vals)
		}
	}
	if _, leaked := existing.WifiIfaces()["radio0"]; leaked {
		t.Error("WifiIfaces must not report a wifi-device as an AP interface")
	}
}

var _ = render.OwnershipTag // keep the render import meaningful if tests shrink

// A device's uci.get does not return only strings.
//
// Measured on OpenWrt 25.12.5: every UCI *option* is a string, but the section
// metadata is not — `.anonymous` is a JSON bool and `.index` a number. Decoding
// the payload straight into map[string]string failed the entire read with
// "cannot unmarshal bool", so every device reported as unplannable. The mock
// returned strings throughout, which is why it took a preview against real
// hardware to find.
func TestFlattenHandlesEveryTypeADeviceReturns(t *testing.T) {
	got := flatten(map[string]any{
		".anonymous": false,
		".index":     float64(1),
		".type":      "wifi-iface",
		".name":      "default_radio0",
		"ssid":       "oonfeewrt-probe-5g",
		"disabled":   "0",
		"maclist":    []any{"aa:bb:cc:dd:ee:ff", "11:22:33:44:55:66"},
		"absent":     nil,
	})
	want := map[string]string{
		".anonymous": "false",
		".index":     "1",
		".type":      "wifi-iface",
		".name":      "default_radio0",
		"ssid":       "oonfeewrt-probe-5g",
		"disabled":   "0",
		"maclist":    "aa:bb:cc:dd:ee:ff 11:22:33:44:55:66",
		"absent":     "",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("flatten[%q] = %q, want %q", k, got[k], w)
		}
	}
	// Nothing may be dropped: a key that vanished here would read downstream as
	// "the device does not have this option", which is a different claim.
	if len(got) != len(want) {
		t.Errorf("flatten produced %d keys from %d; keys were dropped", len(got), len(want))
	}
}

// A number must not come back with a spurious decimal point, or a comparison
// against the string we wrote would always report drift.
func TestFlattenFormatsNumbersWithoutADecimalPoint(t *testing.T) {
	got := flatten(map[string]any{"n": float64(20000)})
	if got["n"] != "20000" {
		t.Errorf("n = %q, want 20000 — a value like 20000.0 would never match "+
			"the string we applied and would report drift on every poll", got["n"])
	}
}

// meshCaps is caps() with 802.11s available, which the mesh renderer gates on.
func meshCaps() *capability.Registry {
	r := caps()
	r.Set(capability.FeatMesh, capability.Present)
	return r
}

// siteMesh is a site carrying one mesh and no WLAN, with ids distinct from the
// other tests in this package — they share one mock process, so a section an
// earlier test confirmed would otherwise be found by a later one.
func siteMesh(deviceID int64, meshID int) model.Site {
	s := baseSite(deviceID)
	s.WLANs = nil
	s.Meshes = []model.Mesh{{
		ID: meshID, MeshID: fmt.Sprintf("backhaul%d", meshID),
		NetworkID: 1, GroupID: 1, Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	}}
	return s
}

// A mesh applies and is recorded as ours, like any other section we write.
//
// The whole apply path had never carried one: mesh landed as model, renderer
// and storage, verified by preview. Preview reads; this is what writes.
func TestAMeshAppliesAndIsRecordedAsOurs(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, siteMesh(dev.ID, 41), model.Device{ID: dev.ID}, meshCaps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if p.Blocked() {
		t.Fatalf("unexpected conflicts: %v", p.Report.Conflicts)
	}
	if p.Empty() {
		t.Fatal("a mesh on a fresh device produced no plan")
	}

	res, err := r.Apply(ctx, c, dev.ID, p, func(context.Context, *ubus.Client) error { return nil })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != applyengine.Applied {
		t.Fatalf("want Applied, got %s", res)
	}
	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 {
		t.Fatalf("want one owned section, got %d: %+v", len(owned), owned)
	}
	if !strings.Contains(owned[0].Section, "_mesh41_") {
		t.Errorf("the owned section is %q, not the mesh", owned[0].Section)
	}
	if owned[0].Config != "wireless" {
		t.Errorf("mesh recorded under config %q", owned[0].Config)
	}
}

// Applying an unchanged mesh twice must do nothing the second time.
//
// A mesh section carries a passphrase, and a plan that never converges would
// rewrite it on every apply — churn on the device for no change, on the one
// kind of section where a rewrite briefly drops the interface.
func TestASecondMeshApplyIsANoOp(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	first, err := r.PlanDevice(ctx, c, siteMesh(dev.ID, 42), model.Device{ID: dev.ID}, meshCaps())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, first,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatal(err)
	}

	again, err := r.PlanDevice(ctx, c, siteMesh(dev.ID, 42), model.Device{ID: dev.ID}, meshCaps())
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Errorf("re-planning an unchanged mesh produced %d op(s): %+v",
			len(again.Plan.Ops), again.Plan.Ops)
	}
}

// Removing a mesh from the model removes it from the device.
//
// Prune is what makes a delete mean anything: the model no longer wants the
// section, we own it, so it goes. Without this a deleted mesh keeps running on
// every AP that carries it — a backhaul nobody can see in the controller.
func TestDeletingAMeshPrunesItFromTheDevice(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, siteMesh(dev.ID, 43), model.Device{ID: dev.ID}, meshCaps())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The mesh is gone from the site model. Everything else is unchanged.
	empty := baseSite(dev.ID)
	empty.WLANs = nil
	gone, err := r.PlanDevice(ctx, c, empty, model.Device{ID: dev.ID}, meshCaps())
	if err != nil {
		t.Fatal(err)
	}
	var removed bool
	for _, op := range gone.Plan.Ops {
		if op.Kind == applyengine.OpDelete && strings.Contains(op.Section, "_mesh43_") {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("deleting the mesh planned no removal: %+v", gone.Plan.Ops)
	}
}

// A WLAN planned onto a switched-off radio must be warned about, end to end
// against the fixture rather than against a hand-built Existing.
//
// This is the state that swallows a WLAN silently: the section is written
// correctly, the apply reports success, and nothing broadcasts — then the
// health check fails looking for an SSID that was never going to appear. The
// fixture could not express it until the mock learned to switch a radio off,
// which is the same reason the adoption bug survived so long.
func TestPlanWarnsWhenARadioIsSwitchedOff(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	if err := c.Call(ctx, "__test", "switch_off_radio",
		map[string]any{"radio": "radio0"}, nil); err != nil {
		t.Skipf("mock cannot switch a radio off: %v", err)
	}
	t.Cleanup(func() {
		// Process-wide fixture: put it back or every later test inherits a dead
		// radio.
		_ = c.Call(ctx, "uci", "delete", map[string]any{
			"config": "wireless", "section": "radio0", "option": "disabled"}, nil)
	})

	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}

	var warned bool
	for _, w := range p.Report.Warnings {
		if w.DefectID == "radio-disabled" {
			warned = true
			if !strings.Contains(w.Summary, "radio0") {
				t.Errorf("the warning does not name the radio: %q", w.Summary)
			}
			if w.Mitigation == "" {
				t.Error("no mitigation, so the operator is told a problem and not a fix")
			}
		}
	}
	if !warned {
		t.Errorf("planning a WLAN onto a switched-off radio produced no warning; "+
			"got %d warning(s): %+v", len(p.Report.Warnings), p.Report.Warnings)
	}

	// It must still be a plan, not a refusal: the controller says what will
	// happen and does what was asked.
	if p.Blocked() {
		t.Error("a switched-off radio blocked the plan; it is a warning, not a conflict")
	}
}
