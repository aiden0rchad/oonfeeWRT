package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestApplyOperationLifecyclePreservesSafeResult(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	op, created, err := db.BeginApplyOperation(ctx, "apply-lifecycle",
		"hmac-request-a", 7, "operator", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !created || op.OperationID != "apply-lifecycle" ||
		op.RequestHash != "hmac-request-a" || op.ActorAdminID != 7 ||
		op.ActorUsername != "operator" || op.State != ApplyOperationQueued ||
		op.CreatedAt != 10 || op.StartedAt != nil || op.FinishedAt != nil ||
		op.ResultJSON != nil || op.Error != "" ||
		op.WriteState != ApplyWriteStateNone ||
		op.HTTPStatus != 0 {
		t.Fatalf("new operation = %+v, created=%v", op, created)
	}
	if err := db.MarkApplyOperationRunning(ctx, op.OperationID, 20); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeApplyOperationDevices(ctx, op.OperationID,
		[]ApplyOperationDevice{
			{DeviceID: 41, DeviceMAC: "aa:bb:cc:dd:ee:41", DeviceName: "ap", Changes: 3},
			{DeviceID: 42, DeviceMAC: "aa:bb:cc:dd:ee:42", DeviceName: "gateway"},
		}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationDeviceApplying(ctx, op.OperationID, 0, 21); err != nil {
		t.Fatal(err)
	}
	running, err := db.ApplyOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if running.State != ApplyOperationRunning || running.StartedAt == nil ||
		*running.StartedAt != 20 || running.WriteState != ApplyWriteStatePossible ||
		len(running.Devices) != 2 || running.Devices[0].Ordinal != 0 ||
		running.Devices[0].State != ApplyOperationDeviceApplying ||
		running.Devices[0].WriteState != ApplyWriteStatePossible ||
		running.Devices[0].StartedAt == nil || *running.Devices[0].StartedAt != 21 ||
		running.Devices[1].State != ApplyOperationDeviceQueued ||
		running.Devices[1].WriteState != ApplyWriteStateNone {
		t.Fatalf("running operation = %+v", running)
	}
	if err := db.FinishApplyOperation(ctx, op.OperationID, ApplyOperationCompleted,
		24, nil, "", ApplyWriteStatePossible, 200); !errors.Is(err, ErrApplyOperationState) {
		t.Fatalf("finish with applying child = %v, want ErrApplyOperationState", err)
	}
	stillRunning, err := db.ApplyOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.State != ApplyOperationRunning ||
		stillRunning.Devices[0].State != ApplyOperationDeviceApplying ||
		stillRunning.Devices[1].State != ApplyOperationDeviceQueued {
		t.Fatalf("failed parent finish was not atomic: %+v", stillRunning)
	}
	if err := db.FinishApplyOperationDevice(ctx, op.OperationID, 0,
		ApplyOperationDeviceCompleted, 25, "applied", "applied", 3,
		"health passed and confirm landed", ApplyWriteStatePossible); err != nil {
		t.Fatal(err)
	}
	result := []byte(`{"devices":[{"device_id":7,"outcome":"applied","reason":"health passed — confirmed"}],"aborted":false}`)
	if err := db.FinishApplyOperation(ctx, op.OperationID, ApplyOperationCompleted,
		30, result, "", ApplyWriteStatePossible, 200); err != nil {
		t.Fatal(err)
	}
	finished, err := db.ApplyOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != ApplyOperationCompleted || finished.FinishedAt == nil ||
		*finished.FinishedAt != 30 || !bytes.Equal(finished.ResultJSON, result) ||
		finished.Error != "" || finished.WriteState != ApplyWriteStatePossible ||
		finished.HTTPStatus != 200 || len(finished.Devices) != 2 ||
		finished.Devices[0].State != ApplyOperationDeviceCompleted ||
		finished.Devices[0].RouterOutcome != "applied" ||
		finished.Devices[0].Outcome != "applied" ||
		finished.Devices[0].Changes != 3 ||
		finished.Devices[1].State != ApplyOperationDeviceSkipped ||
		finished.Devices[1].WriteState != ApplyWriteStateNone ||
		finished.Devices[1].Outcome != "skipped" ||
		finished.Devices[1].FinishedAt == nil || *finished.Devices[1].FinishedAt != 30 {
		t.Fatalf("finished operation = %+v", finished)
	}
	if err := db.FinishApplyOperation(ctx, op.OperationID, ApplyOperationFailed,
		40, nil, "late overwrite", ApplyWriteStatePossible, 500); !errors.Is(err, ErrApplyOperationState) {
		t.Fatalf("terminal overwrite error = %v, want ErrApplyOperationState", err)
	}
	if _, err := db.ApplyOperation(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing operation error = %v, want ErrNotFound", err)
	}
}

func TestBeginApplyOperationReturnsExistingRequestHashForComparison(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, created, err := db.BeginApplyOperation(ctx, "apply-collision",
		"hash-a", 7, "first-operator", 1); err != nil || !created {
		t.Fatalf("first Begin = created %v, err %v", created, err)
	}
	op, created, err := db.BeginApplyOperation(ctx, "apply-collision",
		"hash-b", 8, "second-operator", 2)
	if err != nil {
		t.Fatal(err)
	}
	if created || op.RequestHash != "hash-a" || op.ActorAdminID != 7 ||
		op.ActorUsername != "first-operator" || op.CreatedAt != 1 {
		t.Fatalf("colliding Begin = %+v, created=%v; API cannot compare the original hash", op, created)
	}
}

func TestBeginApplyOperationHasOneConcurrentCreator(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	const callers = 24
	var created atomic.Int64
	errs := make(chan error, callers)
	var wg sync.WaitGroup

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, wasCreated, err := db.BeginApplyOperation(ctx,
				"apply-concurrent", "same-keyed-request-hash", 7, "operator", 100)
			if err == nil && (op.OperationID != "apply-concurrent" ||
				op.RequestHash != "same-keyed-request-hash") {
				err = errors.New("Begin returned the wrong operation")
			}
			if wasCreated {
				created.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := created.Load(); got != 1 {
		t.Fatalf("created count = %d, want exactly one", got)
	}
}

func TestOpeningStoreDoesNotRecoverAndExplicitRecoveryPreservesTruth(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "apply-recovery.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"queued", "running", "running-no-write", "completed"} {
		if _, created, err := db.BeginApplyOperation(ctx, id, "hash-"+id,
			7, "operator", 10); err != nil || !created {
			t.Fatalf("Begin %s = created %v, err %v", id, created, err)
		}
	}
	if err := db.MarkApplyOperationRunning(ctx, "running", 20); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationRunning(ctx, "running-no-write", 20); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeApplyOperationDevices(ctx, "running-no-write",
		[]ApplyOperationDevice{{
			DeviceID: 51, DeviceMAC: "aa:bb:cc:dd:ee:51",
			DeviceName: "waiting-before-write", Changes: 1,
		}}); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeApplyOperationDevices(ctx, "running", []ApplyOperationDevice{
		{DeviceID: 52, DeviceMAC: "aa:bb:cc:dd:ee:52", DeviceName: "applying-ap", Changes: 2},
		{DeviceID: 53, DeviceMAC: "aa:bb:cc:dd:ee:53", DeviceName: "waiting-ap", Changes: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationRunning(ctx, "completed", 20); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeApplyOperationDevices(ctx, "completed", []ApplyOperationDevice{{
		DeviceID: 54, DeviceMAC: "aa:bb:cc:dd:ee:54", DeviceName: "complete-ap", Changes: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationDeviceApplying(ctx, "running", 0, 21); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationDeviceApplying(ctx, "completed", 0, 21); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishApplyOperationDevice(ctx, "completed", 0,
		ApplyOperationDeviceCompleted, 29, "applied", "applied", 1,
		"confirmed", ApplyWriteStatePossible); err != nil {
		t.Fatal(err)
	}
	terminalResult := []byte(`{"devices":[],"aborted":false}`)
	if err := db.FinishApplyOperation(ctx, "completed", ApplyOperationCompleted,
		30, terminalResult, "", ApplyWriteStateNone, 200); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"queued", "running", "running-no-write"} {
		op, err := readOnly.ApplyOperation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := ApplyOperationQueued
		if id != "queued" {
			want = ApplyOperationRunning
		}
		if op.State != want || op.FinishedAt != nil {
			t.Fatalf("read-only open changed %s operation: %+v", id, op)
		}
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"queued", "running", "running-no-write"} {
		op, err := db.ApplyOperation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := ApplyOperationQueued
		if id != "queued" {
			want = ApplyOperationRunning
		}
		if op.State != want || op.FinishedAt != nil {
			t.Fatalf("generic writable open changed %s operation: %+v", id, op)
		}
	}
	const recoveredAt = int64(40)
	if err := db.RecoverApplyOperations(ctx, recoveredAt); err != nil {
		t.Fatal(err)
	}

	queued, err := db.ApplyOperation(ctx, "queued")
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != ApplyOperationUnknown || queued.StartedAt != nil ||
		queued.FinishedAt == nil || *queued.FinishedAt != recoveredAt ||
		queued.WriteState != ApplyWriteStateNone ||
		queued.HTTPStatus != 503 || !strings.Contains(queued.Error, "restarted") {
		t.Fatalf("recovered queued operation = %+v", queued)
	}
	if len(queued.Devices) != 0 {
		t.Fatalf("queued parent unexpectedly has devices: %+v", queued.Devices)
	}
	running, err := db.ApplyOperation(ctx, "running")
	if err != nil {
		t.Fatal(err)
	}
	if running.State != ApplyOperationUnknown || running.StartedAt == nil ||
		*running.StartedAt != 20 || running.FinishedAt == nil ||
		*running.FinishedAt != recoveredAt ||
		running.WriteState != ApplyWriteStatePossible || running.HTTPStatus != 503 ||
		!strings.Contains(running.Error, "outcome may be incomplete") {
		t.Fatalf("recovered running operation = %+v", running)
	}
	if len(running.Devices) != 2 ||
		running.Devices[0].State != ApplyOperationDeviceUnknown ||
		running.Devices[0].WriteState != ApplyWriteStatePossible ||
		running.Devices[0].RouterOutcome != "unknown" ||
		!strings.Contains(running.Devices[0].Reason, "write was in progress") ||
		running.Devices[1].State != ApplyOperationDeviceSkipped ||
		running.Devices[1].WriteState != ApplyWriteStateNone ||
		!strings.Contains(running.Devices[1].Reason, "before this device began") {
		t.Fatalf("recovered running devices = %+v", running.Devices)
	}
	runningNoWrite, err := db.ApplyOperation(ctx, "running-no-write")
	if err != nil {
		t.Fatal(err)
	}
	if runningNoWrite.State != ApplyOperationUnknown ||
		runningNoWrite.WriteState != ApplyWriteStateNone ||
		!strings.Contains(runningNoWrite.Error, "before any device write began") ||
		len(runningNoWrite.Devices) != 1 ||
		runningNoWrite.Devices[0].State != ApplyOperationDeviceSkipped ||
		runningNoWrite.Devices[0].WriteState != ApplyWriteStateNone {
		t.Fatalf("recovered pre-write running operation = %+v", runningNoWrite)
	}
	completed, err := db.ApplyOperation(ctx, "completed")
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != ApplyOperationCompleted || completed.FinishedAt == nil ||
		*completed.FinishedAt != 30 || completed.WriteState != ApplyWriteStatePossible ||
		completed.HTTPStatus != 200 || !bytes.Equal(completed.ResultJSON, terminalResult) {
		t.Fatalf("terminal operation changed during recovery: %+v", completed)
	}
	if len(completed.Devices) != 1 ||
		completed.Devices[0].State != ApplyOperationDeviceCompleted ||
		completed.Devices[0].FinishedAt == nil ||
		*completed.Devices[0].FinishedAt != 29 {
		t.Fatalf("terminal device changed during recovery: %+v", completed.Devices)
	}
}

func TestInterruptApplyOperationConservativelyClosesOneRun(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, created, err := db.BeginApplyOperation(ctx, "interrupted", "hash", 7,
		"operator", 10); err != nil || !created {
		t.Fatalf("Begin = created %v, err %v", created, err)
	}
	if err := db.MarkApplyOperationRunning(ctx, "interrupted", 11); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeApplyOperationDevices(ctx, "interrupted", []ApplyOperationDevice{
		{DeviceID: 1, DeviceMAC: "aa:bb:cc:dd:ee:01", DeviceName: "applying", Changes: 1},
		{DeviceID: 2, DeviceMAC: "aa:bb:cc:dd:ee:02", DeviceName: "queued", Changes: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkApplyOperationDeviceApplying(ctx, "interrupted", 0, 12); err != nil {
		t.Fatal(err)
	}
	if err := db.InterruptApplyOperation(ctx, "interrupted", 20,
		"terminal receipt could not be saved"); err != nil {
		t.Fatal(err)
	}
	op, err := db.ApplyOperation(ctx, "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if op.State != ApplyOperationUnknown || op.WriteState != ApplyWriteStatePossible ||
		op.HTTPStatus != 503 || op.FinishedAt == nil || *op.FinishedAt != 20 ||
		len(op.Devices) != 2 || op.Devices[0].State != ApplyOperationDeviceUnknown ||
		op.Devices[0].WriteState != ApplyWriteStatePossible ||
		op.Devices[1].State != ApplyOperationDeviceSkipped ||
		op.Devices[1].WriteState != ApplyWriteStateNone {
		t.Fatalf("interrupted operation = %+v", op)
	}
}

func TestApplyOperationSchemaMigratesV12AndCreatesDowngradeBoundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "apply-migration.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DROP TABLE apply_operation_devices`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DROP TABLE apply_operations`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM secret_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE schema_version SET version = 12`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("migrate v12 database: %v", err)
	}
	if _, created, err := db.BeginApplyOperation(ctx, "after-v12", "request-hash",
		7, "operator", 1); err != nil || !created {
		t.Fatalf("v13 table unusable after migration: created=%v err=%v", created, err)
	}
	var version int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE schema_version SET version = ?`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if newer, err := Open(ctx, driver, path, testProtector(t, path)); err == nil {
		newer.Close()
		t.Fatalf("a v%d binary accepted a newer schema", schemaVersion)
	} else if !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("newer schema error = %v", err)
	}
}
