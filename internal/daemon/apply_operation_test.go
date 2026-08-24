package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const daemonApplyOperationID = "01962c09-7d62-7cd7-a1c2-450eba830892"

func operationApplyFixture(t *testing.T) (*Daemon, *store.Device, *api.PreviewResult) {
	t.Helper()
	addr := startMock(t)
	d := openDaemon(t)
	dev := seedAP(t, d, "02:00:00:00:20:01", "operation-ap", addr,
		capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), dev.ID,
		bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	// Reuse the mock's running SSID so the post-apply health gate can confirm
	// the update. Ownership makes replacing the stock section intentional.
	bindingSetOption(t, d, dev.ID, "wireless", "default_radio0", "oonfeewrt", "1")
	bindingSaveWLAN(t, d, []int64{dev.ID}, "OpenWrt", "operation-passphrase",
		model.PMFDisabled)
	preview := bindingPreview(t, d)
	if len(preview.Devices) != 1 || len(preview.Devices[0].Changes) == 0 {
		t.Fatalf("operation fixture has no pending change: %+v", preview.Devices)
	}
	d.Config.ApplyDrain = applyengine.MinApplyBudget() + 5*time.Second
	return d, dev, preview
}

func beginDaemonApplyOperation(t *testing.T, d *Daemon, operationID string) {
	t.Helper()
	ctx := context.Background()
	if _, created, err := d.Store.BeginApplyOperation(ctx, operationID,
		strings.Repeat("a", 64), 1, "operator", time.Now().Unix()); err != nil {
		t.Fatal(err)
	} else if !created {
		t.Fatal("operation already existed")
	}
	if err := d.Store.MarkApplyOperationRunning(ctx, operationID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
}

func TestApplyOperationPersistsDeviceBoundaryAndOutcome(t *testing.T) {
	d, _, preview := operationApplyFixture(t)
	beginDaemonApplyOperation(t, d, daemonApplyOperationID)

	result, err := d.ApplySite(context.Background(), api.ApplyRequest{
		OperationID: daemonApplyOperationID, PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Aborted || len(result.Devices) != 1 ||
		result.Devices[0].Outcome != "applied" ||
		result.Devices[0].RouterOutcome != "applied" {
		t.Fatalf("apply result = %+v", result)
	}
	op, err := d.Store.ApplyOperation(context.Background(), daemonApplyOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != store.ApplyOperationRunning ||
		op.WriteState != store.ApplyWriteStatePossible || len(op.Devices) != 1 {
		t.Fatalf("durable parent = %+v", op)
	}
	device := op.Devices[0]
	if device.State != store.ApplyOperationDeviceCompleted ||
		device.WriteState != store.ApplyWriteStatePossible ||
		device.RouterOutcome != "applied" || device.Outcome != "applied" ||
		device.Changes == 0 || device.StartedAt == nil || device.FinishedAt == nil {
		t.Fatalf("durable device = %+v", device)
	}
}

func TestApplyOperationBoundaryPersistenceFailurePreventsRouterWrite(t *testing.T) {
	d, dev, preview := operationApplyFixture(t)
	beginDaemonApplyOperation(t, d, daemonApplyOperationID)
	before := bindingConfigFingerprint(t, d, dev.ID)
	if _, err := d.Store.SQL().Exec(`
CREATE TRIGGER fail_apply_boundary
BEFORE UPDATE OF state ON apply_operation_devices
WHEN NEW.state = 'applying'
BEGIN
  SELECT RAISE(ABORT, 'test boundary storage failure');
END`); err != nil {
		t.Fatal(err)
	}

	result, err := d.ApplySite(context.Background(), api.ApplyRequest{
		OperationID: daemonApplyOperationID, PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Aborted || len(result.Devices) != 1 ||
		!strings.Contains(result.Devices[0].Reason, "write boundary") {
		t.Fatalf("boundary failure result = %+v", result)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after != before {
		t.Fatal("router changed after the durable write boundary failed")
	}
	op, err := d.Store.ApplyOperation(context.Background(), daemonApplyOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op.WriteState == store.ApplyWriteStatePossible || len(op.Devices) != 1 ||
		op.Devices[0].State != store.ApplyOperationDeviceFailed ||
		op.Devices[0].WriteState != store.ApplyWriteStateNone {
		t.Fatalf("boundary failure durability = %+v", op)
	}
}

func TestApplyOperationPreservesRouterOutcomeWhenOwnershipRecordingFails(t *testing.T) {
	d, dev, preview := operationApplyFixture(t)
	beginDaemonApplyOperation(t, d, daemonApplyOperationID)
	before := bindingConfigFingerprint(t, d, dev.ID)
	if _, err := d.Store.SQL().Exec(`
CREATE TRIGGER fail_owned_record
BEFORE INSERT ON owned_sections
BEGIN
  SELECT RAISE(ABORT, 'test ownership ledger failure');
END`); err != nil {
		t.Fatal(err)
	}

	result, err := d.ApplySite(context.Background(), api.ApplyRequest{
		OperationID: daemonApplyOperationID, PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Aborted || len(result.Devices) != 1 ||
		result.Devices[0].Outcome != "error" ||
		result.Devices[0].RouterOutcome != "applied" ||
		!strings.Contains(result.Devices[0].Reason, "ownership") {
		t.Fatalf("ledger failure result = %+v", result)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after == before {
		t.Fatal("fixture never reached a confirmed router write")
	}
	op, err := d.Store.ApplyOperation(context.Background(), daemonApplyOperationID)
	if err != nil {
		t.Fatal(err)
	}
	device := op.Devices[0]
	if op.WriteState != store.ApplyWriteStatePossible ||
		device.State != store.ApplyOperationDeviceFailed ||
		device.WriteState != store.ApplyWriteStatePossible ||
		device.RouterOutcome != "applied" || device.Outcome != "error" {
		t.Fatalf("ledger failure durability = %+v", op)
	}

	// The public result that the API journals contains the two truths and no
	// internal plan or credential material.
	blob, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"router_outcome":"applied"`) {
		t.Fatalf("public result lost router outcome: %s", blob)
	}
}

func TestApplyOperationPreservesRouterOutcomeWhenAuditRecordingFails(t *testing.T) {
	d, dev, preview := operationApplyFixture(t)
	beginDaemonApplyOperation(t, d, daemonApplyOperationID)
	before := bindingConfigFingerprint(t, d, dev.ID)
	if _, err := d.Store.SQL().Exec(`
CREATE TRIGGER fail_apply_audit
BEFORE INSERT ON events WHEN NEW.event = 'config.apply'
BEGIN
  SELECT RAISE(ABORT, 'test apply audit failure');
END`); err != nil {
		t.Fatal(err)
	}

	result, err := d.ApplySite(context.Background(), api.ApplyRequest{
		OperationID: daemonApplyOperationID, PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Aborted || len(result.Devices) != 1 ||
		result.Devices[0].Outcome != "error" ||
		result.Devices[0].RouterOutcome != "applied" ||
		!strings.Contains(result.Devices[0].Reason, "audit recording failed") {
		t.Fatalf("audit failure result = %+v", result)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after == before {
		t.Fatal("fixture never reached a confirmed router write")
	}
	op, err := d.Store.ApplyOperation(context.Background(), daemonApplyOperationID)
	if err != nil {
		t.Fatal(err)
	}
	device := op.Devices[0]
	if op.WriteState != store.ApplyWriteStatePossible ||
		device.State != store.ApplyOperationDeviceFailed ||
		device.WriteState != store.ApplyWriteStatePossible ||
		device.RouterOutcome != "applied" || device.Outcome != "error" {
		t.Fatalf("audit failure durability = %+v", op)
	}
	if events, err := d.Store.DeviceEvents(context.Background(), dev.ID,
		"config.apply", 1); err != nil || len(events) != 0 {
		t.Fatalf("rejected audit unexpectedly persisted: events=%+v err=%v", events, err)
	}
}

func TestDaemonStartupRecoversUnfinishedApplyOperations(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, "apply recovery passphrase")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passphrase, err := secrets.ReadPassphraseFile(cfg.PassphraseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(passphrase)
	keeper, err := secrets.Create(secrets.DefaultPath(cfg.DataDir), passphrase,
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, driverName, cfg.DBPath(), keeper)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"queued-on-restart", "running-on-restart"} {
		if _, created, err := db.BeginApplyOperation(ctx, id, "hash-"+id,
			1, "operator", 1); err != nil || !created {
			t.Fatalf("begin %s: created=%v err=%v", id, created, err)
		}
	}
	if err := db.MarkApplyOperationRunning(ctx, "running-on-restart", 2); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeApplyOperationDevices(ctx, "running-on-restart",
		[]store.ApplyOperationDevice{{
			DeviceID: 7, DeviceMAC: "aa:bb:cc:dd:ee:ff",
			DeviceName: "gateway", Changes: 1,
		}}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationDeviceApplying(ctx, "running-on-restart", 0, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	queued, err := d.Store.ApplyOperation(ctx, "queued-on-restart")
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != store.ApplyOperationUnknown ||
		queued.WriteState != store.ApplyWriteStateNone {
		t.Fatalf("daemon-recovered queued operation = %+v", queued)
	}
	running, err := d.Store.ApplyOperation(ctx, "running-on-restart")
	if err != nil {
		t.Fatal(err)
	}
	if running.State != store.ApplyOperationUnknown ||
		running.WriteState != store.ApplyWriteStatePossible ||
		len(running.Devices) != 1 ||
		running.Devices[0].State != store.ApplyOperationDeviceUnknown ||
		running.Devices[0].WriteState != store.ApplyWriteStatePossible {
		t.Fatalf("daemon-recovered running operation = %+v", running)
	}
}
