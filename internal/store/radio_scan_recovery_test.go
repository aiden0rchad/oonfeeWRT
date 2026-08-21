package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestRecoverRadioScansClosesOnlyUnfinishedRunsTruthfully(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	makeScan := func(section string, started int64, status model.RadioScanStatus) model.RadioScan {
		t.Helper()
		scan := model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: section},
			StartedAt: started, Status: status}
		if err := db.CreateRadioScan(ctx, &scan); err != nil {
			t.Fatal(err)
		}
		return scan
	}

	pending := makeScan("radio0", 200, model.RadioScanPending)
	running := makeScan("radio1", 300, model.RadioScanRunning)
	completed := makeScan("radio2", 100, model.RadioScanRunning)
	if err := db.FinishRadioScan(ctx, completed.ID, model.RadioScanCompleted, 150,
		json.RawMessage(`{"source":"iwinfo.scan"}`), nil); err != nil {
		t.Fatal(err)
	}

	n, err := db.RecoverRadioScans(ctx, 250)
	if err != nil || n != 2 {
		t.Fatalf("recovered=%d err=%v, want 2", n, err)
	}
	for _, want := range []struct {
		scan     model.RadioScan
		finished int64
	}{{pending, 250}, {running, 300}} {
		got, rows, err := db.RadioScanByID(ctx, want.scan.ID)
		if err != nil || got.Status != model.RadioScanFailed || got.FinishedAt == nil ||
			*got.FinishedAt != want.finished || len(rows) != 0 {
			t.Fatalf("recovered scan=%+v rows=%+v err=%v", got, rows, err)
		}
		var detail struct {
			Interrupted bool   `json:"interrupted"`
			Reason      string `json:"reason"`
		}
		if err := json.Unmarshal(got.Detail, &detail); err != nil || !detail.Interrupted || detail.Reason == "" {
			t.Fatalf("recovery detail=%s parsed=%+v err=%v", got.Detail, detail, err)
		}
	}
	got, _, err := db.RadioScanByID(ctx, completed.ID)
	if err != nil || got.Status != model.RadioScanCompleted || got.FinishedAt == nil || *got.FinishedAt != 150 {
		t.Fatalf("terminal scan changed during recovery: %+v err=%v", got, err)
	}
	if n, err := db.RecoverRadioScans(ctx, 400); err != nil || n != 0 {
		t.Fatalf("second recovery=%d err=%v, want idempotent zero", n, err)
	}
}
