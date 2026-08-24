package reconcile

import (
	"context"
	"encoding/json"
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
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
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
	if err := c.Call(context.Background(), "__test", "reset", nil, nil); err != nil {
		t.Fatalf("reset mock: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func openStore(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	keeper, err := secrets.Create(filepath.Join(dir, secrets.FileName),
		[]byte("reconcile-test-key"), secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatalf("secrets.Create: %v", err)
	}
	db, err := store.Open(context.Background(), "sqlite",
		filepath.Join(dir, "t.db"), keeper)
	if err != nil {
		keeper.Close()
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		keeper.Close()
	})
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

func TestOwnershipLedgerFailureAfterAppliedAndEmptyPlansIsTruthful(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	s := siteWLAN(dev.ID, 10)

	p, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatal(err)
	}
	if p.Empty() || p.Blocked() {
		t.Fatalf("fixture plan empty=%v blocked=%v", p.Empty(), p.Blocked())
	}
	if _, err := db.SQL().ExecContext(ctx, `CREATE TRIGGER reject_owned_insert
		BEFORE INSERT ON owned_sections BEGIN
			SELECT RAISE(ABORT, 'synthetic ownership ledger failure');
		END`); err != nil {
		t.Fatal(err)
	}

	assertFailure := func(label string, res applyengine.Result, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "synthetic ownership ledger failure") {
			t.Fatalf("%s error = %v", label, err)
		}
		if res.Outcome != applyengine.Applied ||
			!strings.Contains(res.Reason, "device outcome was applied") ||
			!strings.Contains(res.Reason, "ownership recording failed") ||
			!strings.Contains(res.Reason, "synthetic ownership ledger failure") {
			t.Fatalf("%s result = %+v", label, res)
		}
	}

	res, err := r.Apply(ctx, c, dev.ID, p, nil)
	assertFailure("confirmed apply", res, err)

	// The ledger failed after confirmation, not before the device write. A
	// fresh plan sees the requested config already present and has no device
	// operations, while the controller still has no ownership claim for it.
	p2, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Empty() {
		t.Fatalf("router config was not applied before the ledger failure: %+v", p2.Plan.Ops)
	}
	if owned, err := db.OwnedSections(ctx, dev.ID); err != nil || len(owned) != 0 {
		t.Fatalf("ownership claims = %+v, err=%v", owned, err)
	}

	res, err = r.Apply(ctx, c, dev.ID, p2, nil)
	assertFailure("empty apply", res, err)

	events, err := db.DeviceEvents(ctx, dev.ID, "config.apply", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("audit events = %+v, want both ledger failures", events)
	}
	for _, event := range events {
		detail := string(fmt.Appendf(nil, "%s", event.Detail))
		if event.Severity != "error" || !strings.Contains(detail, `"outcome":"applied"`) ||
			!strings.Contains(detail, "synthetic ownership ledger failure") {
			t.Errorf("untruthful ledger-failure audit: %+v", event)
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
		"config": "wireless", "section": "oowrt_wlan14_radio1",
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

func TestApplyOutcomeAuditIsBoundedAndSurvivesCallerCancellation(t *testing.T) {
	r, db := newReconciler(t)
	dev := device(t, db)
	huge := strings.Repeat("&", 70<<10)
	omissions := make([]render.Omission, 64)
	for i := range omissions {
		omissions[i] = render.Omission{
			WLAN: strings.Repeat("<", 1024), Reason: huge,
			Kind: render.OmissionKind(strings.Repeat(">", 512)),
		}
	}
	if blob, err := json.Marshal(omissions); err != nil {
		t.Fatal(err)
	} else if len(blob) <= 64<<10 {
		t.Fatalf("fixture is only %d bytes; it does not exercise the event cap", len(blob))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := applyengine.Result{
		Outcome: applyengine.Applied, Reason: huge, HealthErr: errors.New(huge),
	}
	if err := r.logOutcome(ctx, dev.ID,
		&DevicePlan{Report: render.Report{Omissions: omissions}}, res, errors.New(huge)); err != nil {
		t.Fatalf("bounded detached audit: %v", err)
	}
	events, err := db.DeviceEvents(context.Background(), dev.ID, "config.apply", 1)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit events=%+v err=%v", events, err)
	}
	raw, ok := events[0].Detail.(json.RawMessage)
	if !ok {
		t.Fatalf("audit detail type = %T", events[0].Detail)
	}
	if len(raw) >= 64<<10 {
		t.Fatalf("bounded audit detail is %d bytes", len(raw))
	}
	var detail struct {
		Omissions            []render.Omission `json:"omissions"`
		OmissionsTotal       int               `json:"omissions_total"`
		OmissionsTruncated   bool              `json:"omissions_truncated"`
		ReasonTruncated      bool              `json:"reason_truncated"`
		ErrorTruncated       bool              `json:"error_truncated"`
		HealthErrorTruncated bool              `json:"health_error_truncated"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Omissions) != applyAuditMaxOmissions ||
		detail.OmissionsTotal != len(omissions) || !detail.OmissionsTruncated ||
		!detail.ReasonTruncated || !detail.ErrorTruncated || !detail.HealthErrorTruncated {
		t.Fatalf("bounded audit detail = %+v", detail)
	}
}

func TestApplyFailsLoudlyWhenOutcomeAuditCannotBeRecorded(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	p, err := r.PlanDevice(ctx, c, siteWLAN(dev.ID, 112), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatal(err)
	}
	if p.Empty() || p.Blocked() {
		t.Fatalf("fixture plan empty=%v blocked=%v", p.Empty(), p.Blocked())
	}
	for range 16 {
		p.Report.Omissions = append(p.Report.Omissions, render.Omission{
			WLAN: "large-audit-fixture", Reason: strings.Repeat("&", 1024),
		})
	}
	if _, err := db.SQL().ExecContext(ctx, `CREATE TRIGGER reject_apply_audit
BEFORE INSERT ON events WHEN NEW.event = 'config.apply' BEGIN
  SELECT RAISE(ABORT, 'synthetic apply audit failure');
END`); err != nil {
		t.Fatal(err)
	}

	res, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "synthetic apply audit failure") {
		t.Fatalf("apply error = %v", err)
	}
	if res.Outcome != applyengine.Applied ||
		!strings.Contains(res.Reason, "device outcome was applied") ||
		!strings.Contains(res.Reason, "apply-outcome audit recording failed") {
		t.Fatalf("apply result = %+v", res)
	}
	if owned, err := db.OwnedSections(ctx, dev.ID); err != nil || len(owned) == 0 {
		t.Fatalf("confirmed router outcome was lost: owned=%+v err=%v", owned, err)
	}
	if events, err := db.DeviceEvents(ctx, dev.ID, "config.apply", 1); err != nil || len(events) != 0 {
		t.Fatalf("rejected audit unexpectedly persisted: events=%+v err=%v", events, err)
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

func TestNoOpPlanStillRunsRuntimeHealth(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	runtimeErr := errors.New("managed BSS is absent")
	ran := false
	res, err := r.Apply(ctx, c, dev.ID, &DevicePlan{Device: model.Device{ID: dev.ID}},
		func(context.Context, *ubus.Client) error {
			ran = true
			return runtimeErr
		})
	if !ran || !errors.Is(err, runtimeErr) || !errors.Is(res.HealthErr, runtimeErr) || res.Outcome != "" {
		t.Fatalf("no-op runtime health: ran=%v result=%+v err=%v", ran, res, err)
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
		"ssid":       "fixture-probe-5g",
		"disabled":   "0",
		"maclist":    []any{"aa:bb:cc:dd:ee:ff", "11:22:33:44:55:66"},
		"absent":     nil,
	})
	want := map[string]string{
		".anonymous": "false",
		".index":     "1",
		".type":      "wifi-iface",
		".name":      "default_radio0",
		"ssid":       "fixture-probe-5g",
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
	//
	// One key is ADDED — render.ListsKey, recording which options arrived as
	// UCI lists. Space-joining destroys that, and it is the difference between
	// a config netifd honours and one it silently ignores.
	if len(got) != len(want)+1 {
		t.Errorf("flatten produced %d keys from %d; keys were dropped", len(got), len(want))
	}
	if got[render.ListsKey] != "maclist" {
		t.Errorf("%s = %q, want the one list option", render.ListsKey, got[render.ListsKey])
	}
}

// A section with no list options records that it has none — it does not go
// silent.
//
// Absent is the third state, "nobody recorded this". If a list-free section
// fell into it, every section holding the malformed `option ports 'a b'` form
// would look unknown, which is precisely the case the marker exists to catch.
func TestFlattenRecordsTheAbsenceOfListsToo(t *testing.T) {
	got := flatten(map[string]any{"ssid": "x"})
	raw, ok := got[render.ListsKey]
	if !ok {
		t.Fatal("a section with no lists recorded nothing, so its options are " +
			"indistinguishable from options nobody looked at")
	}
	if raw != "" {
		t.Errorf("%s = %q, want empty", render.ListsKey, raw)
	}
	if isList, known := render.StoredAsList(got, "ssid"); isList || !known {
		t.Errorf("StoredAsList(ssid) = %v,%v; want false,true", isList, known)
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

// An ownership claim must not outlive the section it describes.
//
// The apply PRUNES: Doc.Prune removes every section carrying our marker that
// the render no longer produces. Recording only the additions therefore left a
// claim behind for everything ever pruned — observed on the lab C6, which
// claimed a mesh and an uplink section long after both were deleted from the
// site model and removed from the device.
//
// Harmless to the apply path, and not harmless to the operator: the un-adopt
// panel lists what it is about to revert, and listing sections that are not
// there is the kind of wrong detail that makes someone doubt the rest of it.
func TestAPrunedSectionLosesItsOwnershipClaim(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	// Apply a site with one WLAN, then confirm both radios are claimed.
	full := site(dev.ID)
	p, err := r.PlanDevice(ctx, c, full, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("nothing was claimed after the first apply")
	}

	// Now render a site with NO WLANs at all. Everything previously written is
	// pruned from the device.
	empty := full
	empty.WLANs = nil
	p2, err := r.PlanDevice(ctx, c, empty, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice (empty): %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p2, nil); err != nil {
		t.Fatalf("Apply (empty): %v", err)
	}

	after, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		var names []string
		for _, o := range after {
			names = append(names, o.Config+"."+o.Section)
		}
		t.Errorf("every section was pruned from the device, yet %d claim(s) "+
			"survived: %v", len(after), names)
	}
}

// Drift means the DEVICE changed, and must not be reported for our own edit.
//
// detectDrift compared the freshly rendered desired state against the device,
// which differs for two entirely different reasons and reported both the same
// way. So editing a WLAN and pressing Preview made every device announce
// "Someone edited config we own on this device" — naming a culprit for a change
// the reader had made in that screen seconds earlier — and adding "we applied
// X" for a value never applied to anything. Observed for real while turning PMF
// back on: two devices, four accusations, all false.
//
// The recorded hash is what tells the two apart, and it was already being
// stored at every confirmed apply.
func TestDriftIsNotReportedForOurOwnPendingEdit(t *testing.T) {
	sec := render.Section{
		Config: "wireless", Type: "wifi-iface", Name: "oowrt_wlan1_radio0",
		Values: map[string]string{
			render.OwnershipTag: "1",
			"ssid":              "fixture-roam",
			"ieee80211w":        "1", // what the operator has just asked for
		},
	}
	doc := render.Doc{Sections: []render.Section{sec}}
	existing := render.Existing{Configs: map[string]map[string]map[string]string{
		"wireless": {"oowrt_wlan1_radio0": {
			render.OwnershipTag: "1",
			"ssid":              "fixture-roam",
			"ieee80211w":        "0", // what is actually on the device
		}},
	}}

	// The model has moved since the last apply: the stored hash is not this
	// section's hash. That difference is ours, and the plan reports it as a
	// change to make — saying it again as drift is one edit wearing two names.
	ourEdit := map[string]string{"wireless.oowrt_wlan1_radio0": "a-hash-from-before-the-edit"}
	if d := detectDrift(doc, existing, ourEdit); len(d) != 0 {
		t.Errorf("our own pending edit was reported as someone editing the device: %v", d)
	}

	// The model is exactly what we applied, so the same difference now really
	// is the device having been changed under us. That must still be reported.
	unchanged := map[string]string{"wireless.oowrt_wlan1_radio0": sec.Hash()}
	d := detectDrift(doc, existing, unchanged)
	if len(d) != 1 {
		t.Fatalf("a device edited behind our back must still be reported: %v", d)
	}
	if d[0].Option != "ieee80211w" || d[0].Ours != "1" || d[0].Theirs != "0" {
		t.Errorf("drift does not name the option and both values: %+v", d[0])
	}

	// A section with no record at all — never applied — keeps the old
	// behaviour rather than silently vanishing from the report.
	if d := detectDrift(doc, existing, map[string]string{}); len(d) != 1 {
		t.Errorf("an unrecorded section should still be compared: %v", d)
	}
}

// The wiring: PlanDevice has to actually consult the recorded hashes.
//
// detectDrift's own test covers the rule, and a mutation that stopped
// PlanDevice loading the hashes at all broke nothing — the same shape of gap
// this repository keeps finding, where a correct function is wired to nothing.
// This drives the real path: apply, then change the SITE MODEL rather than the
// device, and require that the operator's own pending edit is not reported back
// to them as somebody having edited the router.
func TestPlanDeviceDoesNotCallOurOwnEditDrift(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	s := siteWLAN(dev.ID, 21)

	p, err := r.PlanDevice(ctx, c, s, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The operator changes the WLAN in the controller. Nothing touches the
	// device — this is exactly pressing Save and then Preview.
	//
	// The SSID rather than PMF, deliberately. A disabled PMF renders no
	// ieee80211w key at all, so the device has none after the apply and the
	// comparison skips the option on both sides — which made the first version
	// of this test pass whether or not the wiring existed. The SSID is written
	// on every apply, so it is present on the device and genuinely differs.
	edited := s
	edited.WLANs = append([]model.WLAN(nil), s.WLANs...)
	edited.WLANs[0].SSID = "Home21-renamed-in-the-controller"

	p2, err := r.PlanDevice(ctx, c, edited, model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice after the edit: %v", err)
	}
	if len(p2.Drift) != 0 {
		t.Errorf("the operator's own unapplied edit was reported as somebody "+
			"editing the device: %v", p2.Drift)
	}
	// And it must still be reported as work to do, or suppressing the drift
	// would have hidden the change instead of relabelling it.
	if p2.Empty() {
		t.Error("the edit produced no plan, so nothing would ever apply it")
	}
}

// forceOp guarantees the plan is non-empty, and says so if it cannot.
//
// The mock ubus server is shared across this package and keeps what earlier
// applies wrote, so a device may already match the site model. Apply returns
// early on an empty plan without touching the ownership record — which would
// let an ownership test report success having exercised none of it.
func forceOp(t *testing.T, p *DevicePlan) {
	t.Helper()
	p.Plan.Ops = append(p.Plan.Ops, applyengine.Op{
		Kind: applyengine.OpSet, Config: "wireless", Type: "wifi-iface",
		Name: "oowrt_wlan1_radio0", Section: "oowrt_wlan1_radio0",
		Values: map[string]string{"ssid": "forced-by-test"},
	})
	if p.Empty() {
		t.Fatal("plan is still empty; Apply would return before recording ownership")
	}
}

// An apply must not forget a claim on a section it deliberately did not touch.
//
// ReplaceOwned replaces rather than merges, on the premise that an apply prunes
// every owned section absent from the document. render's Retain and Blind broke
// that premise on purpose — a device whose radios could not be read keeps its
// sections instead of having them deleted — so the record has to keep saying we
// own them.
//
// The cost of getting this wrong is not bookkeeping. daemon.ownedSections reads
// this exact table to decide what un-adopt reverts, so a forgotten claim leaves
// oonfeeWRT's config on a device the operator was told had been cleaned.
func TestApplyKeepsClaimsOnSectionsItCouldNotDecideAbout(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	// A section we own on the device that this render knows nothing about,
	// recorded as ours by some earlier apply.
	const orphan = "oowrt_up990_radio9"
	if err := db.ReplaceOwned(ctx, dev.ID, []store.OwnedSection{
		{DeviceID: dev.ID, Config: "wireless", Section: orphan,
			RenderedHash: "old-hash", AppliedAt: 1000},
	}); err != nil {
		t.Fatal(err)
	}

	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	// Stand in for what render does on a device it cannot see: the section is
	// ours on the device, absent from the document, and explicitly not pruned.
	p.Existing.Configs["wireless"][orphan] = map[string]string{
		".type": "wifi-iface", render.OwnershipTag: "1", "mode": "sta",
	}
	p.Doc.Retain = append(p.Doc.Retain,
		render.SectionRef{Config: "wireless", Name: orphan})
	p.Plan.Ops = append(p.Plan.Ops, p.Doc.Prune(p.Existing)...)
	forceOp(t, p)

	for _, op := range p.Plan.Ops {
		if op.Kind == applyengine.OpDelete && op.Section == orphan {
			t.Fatal("the retained section was staged for deletion")
		}
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
	var kept *store.OwnedSection
	for i := range owned {
		if owned[i].Section == orphan {
			kept = &owned[i]
		}
	}
	if kept == nil {
		t.Fatal("the claim was dropped for a section still on the device and " +
			"still carrying our marker: un-adopt reverts exactly this table, so " +
			"that config can never be removed again")
	}
	// Carried forward unchanged. We did not re-apply it, and dating a change
	// that never happened hands detectDrift a hash nothing ever wrote.
	if kept.RenderedHash != "old-hash" || kept.AppliedAt != 1000 {
		t.Errorf("the carried-forward claim was restamped: %+v", *kept)
	}
	// And the sections we DID render are still claimed on their own account.
	if len(owned) != 3 {
		t.Errorf("want the two rendered sections plus the retained one, got %d", len(owned))
	}
}

// A claim is carried forward only for a section that is still OURS on the
// device.
//
// The record is what un-adopt deletes, so a claim kept for a section a human
// has since taken over is the controller deleting someone else's config on the
// way out — the one thing the ownership marker exists to prevent. Same for a
// section that is simply gone: claiming it makes the un-adopt panel promise to
// revert something that is not there.
func TestApplyDoesNotCarryForwardClaimsThatAreNoLongerOurs(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	// Names no other test in this package writes: the mock ubus server is
	// shared across the package and keeps what earlier applies put there, so a
	// reused name arrives already on the device and inverts the fixture.
	const taken = "oowrt_up991_radio9" // a human replaced this one
	const gone = "oowrt_mesh992_radio9"
	if err := db.ReplaceOwned(ctx, dev.ID, []store.OwnedSection{
		{DeviceID: dev.ID, Config: "wireless", Section: taken, RenderedHash: "h", AppliedAt: 1},
		{DeviceID: dev.ID, Config: "wireless", Section: gone, RenderedHash: "h", AppliedAt: 1},
	}); err != nil {
		t.Fatal(err)
	}

	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	// On the device, but no longer carrying our marker.
	p.Existing.Configs["wireless"][taken] = map[string]string{
		".type": "wifi-iface", "mode": "sta",
	}
	// `gone` is absent from Existing entirely.
	p.Doc.Blind = append(p.Doc.Blind, "wireless")
	p.Plan.Ops = append(p.Plan.Ops, p.Doc.Prune(p.Existing)...)
	forceOp(t, p)

	if _, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range owned {
		if o.Section == taken {
			t.Error("kept a claim on a section a human now owns; un-adopt " +
				"reverts this table, so the controller would delete their config")
		}
		if o.Section == gone {
			t.Error("kept a claim on a section that is not on the device; the " +
				"un-adopt panel would promise to revert something absent")
		}
	}
}

// A rendered section must be claimed once, not twice.
func TestApplyClaimsEachSectionOnce(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	forceOp(t, p)
	if _, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Re-plan against the device we just wrote, with the whole config blind, so
	// every rendered section is also a candidate for carrying forward.
	p2, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	p2.Doc.Blind = append(p2.Doc.Blind, "wireless")
	forceOp(t, p2)
	if _, err := r.Apply(ctx, c, dev.ID, p2,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, o := range owned {
		seen[o.Config+"."+o.Section]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s claimed %d times", k, n)
		}
	}
}

// driftOn is a section we own on the device, recorded as applied exactly as
// rendered — the state in which any difference really is somebody else's edit.
func driftFixture(current map[string]string) (render.Doc, render.Existing, map[string]string) {
	sec := render.Section{
		Config: "network", Type: "bridge-vlan", Name: "oowrt_bv20",
		Values: map[string]string{"device": "br-lan", "vlan": "20",
			render.OwnershipTag: "1"},
		Lists: map[string][]string{"ports": {"lan1:t", "lan2:t"}},
	}
	doc := render.Doc{Sections: []render.Section{sec}}
	existing := render.NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_bv20": current},
	})
	return doc, existing, map[string]string{"network.oowrt_bv20": sec.Hash()}
}

// A human editing a LIST option must be reported, not silently reverted.
//
// The section hash still matches what we applied, so this is correctly not our
// own pending edit — and drift then read only s.Values, so nothing was
// reported while plan.matches quietly staged a set to put it back. This
// package's opening claim is that drift is surfaced and never silently
// corrected; for list options it was silently corrected, and those are the
// options whose malformed form took the LAN down.
func TestDriftSeesEditsToListOptions(t *testing.T) {
	doc, existing, applied := driftFixture(map[string]string{
		"device": "br-lan", "vlan": "20", render.OwnershipTag: "1",
		"ports":         "lan1:t", // a human removed lan2 in LuCI
		render.ListsKey: "ports",
	})
	got := detectDrift(doc, existing, applied)
	if len(got) != 1 {
		t.Fatalf("want the port change reported as drift, got %v", got)
	}
	if got[0].Option != "ports" || got[0].Theirs != "lan1:t" {
		t.Errorf("drift = %+v", got[0])
	}
}

// An option a human DELETED from a section we own is an edit too.
func TestDriftSeesADeletedOption(t *testing.T) {
	doc, existing, applied := driftFixture(map[string]string{
		// "vlan" is gone.
		"device": "br-lan", render.OwnershipTag: "1",
		"ports": "lan1:t lan2:t", render.ListsKey: "ports",
	})
	got := detectDrift(doc, existing, applied)
	if len(got) != 1 || got[0].Option != "vlan" {
		t.Fatalf("want the deleted option reported, got %v", got)
	}
}

// But only when we have a record of applying it. Without one, an option
// missing from the device is indistinguishable from an option this version of
// the renderer has only just started writing — and accusing an operator of
// deleting something we never applied is the false-drift this whole mechanism
// was built to stop.
func TestDriftDoesNotInventADeletionWithoutARecord(t *testing.T) {
	doc, existing, _ := driftFixture(map[string]string{
		"device": "br-lan", render.OwnershipTag: "1",
		"ports": "lan1:t lan2:t", render.ListsKey: "ports",
	})
	if got := detectDrift(doc, existing, map[string]string{}); len(got) != 0 {
		t.Errorf("accused someone of deleting an option we never recorded "+
			"applying: %v", got)
	}
}

// A device holding exactly what we applied is not drifting. Otherwise every
// poll accuses somebody.
func TestDriftIsSilentWhenTheDeviceMatches(t *testing.T) {
	doc, existing, applied := driftFixture(map[string]string{
		"device": "br-lan", "vlan": "20", render.OwnershipTag: "1",
		"ports": "lan1:t lan2:t", render.ListsKey: "ports",
	})
	if got := detectDrift(doc, existing, applied); len(got) != 0 {
		t.Errorf("reported drift against an unchanged device: %v", got)
	}
}

// Our own pending edit is still not drift: the hash moved, so the plan already
// lists it as a change to make and naming it twice makes one edit look like
// two problems.
func TestOurOwnPendingEditIsNotDrift(t *testing.T) {
	doc, existing, _ := driftFixture(map[string]string{
		"device": "br-lan", "vlan": "20", render.OwnershipTag: "1",
		"ports": "lan1:t lan2:t", render.ListsKey: "ports",
	})
	stale := map[string]string{"network.oowrt_bv20": "a-hash-from-before-the-edit"}
	if got := detectDrift(doc, existing, stale); len(got) != 0 {
		t.Errorf("reported our own pending edit as somebody else's: %v", got)
	}
}

// A device that already matches must still have its ownership recorded.
//
// Re-adopt a device whose config an earlier un-adopt could not remove — the
// reference fleet's exact state after a routing failure — and the render
// matches what is already there, so the plan is empty and Apply returned
// before recording anything.
//
// Ownership is what un-adopt reverts (daemon.ownedSections), so the controller
// was left demonstrably owning config it could not clean up: the sections
// carry its marker on the device and appear nowhere in its record.
func TestAnEmptyPlanStillRecordsWhatIsOnTheDevice(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	// Apply once so the device carries our sections, then forget them, which
	// is what a fresh adoption over an uncleaned device looks like.
	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if _, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := db.ReplaceOwned(ctx, dev.ID, nil); err != nil {
		t.Fatal(err)
	}

	// Now the plan really is empty, and the sections really are ours.
	p2, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	if !p2.Empty() {
		t.Fatalf("expected nothing to apply, got %d ops", len(p2.Plan.Ops))
	}
	res, err := r.Apply(ctx, c, dev.ID, p2,
		func(context.Context, *ubus.Client) error { return nil })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Outcome != applyengine.Applied {
		t.Fatalf("outcome = %s", res.Outcome)
	}

	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) == 0 {
		t.Fatal("the controller owns sections on the device and has no record " +
			"of them, so un-adopt cannot remove its own config")
	}
	for _, o := range owned {
		if o.RenderedHash == "" || o.AppliedAt == 0 {
			t.Errorf("incomplete claim: %+v", o)
		}
	}
}

// And it must not claim a section the device does not have, which would
// promise an un-adopt that reverts something absent.
func TestAnEmptyPlanClaimsNothingTheDeviceLacks(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	p, err := r.PlanDevice(ctx, c, site(dev.ID), model.Device{ID: dev.ID}, caps())
	if err != nil {
		t.Fatalf("PlanDevice: %v", err)
	}
	// A section that renders but is NOT ours on the device.
	for _, s := range p.Doc.Sections {
		p.Existing.Configs[s.Config][s.Name] = map[string]string{".type": "wifi-iface"}
	}
	p.Plan.Ops = nil
	if _, err := r.Apply(ctx, c, dev.ID, p,
		func(context.Context, *ubus.Client) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 0 {
		t.Errorf("claimed sections the device holds without our marker: %+v", owned)
	}
}
