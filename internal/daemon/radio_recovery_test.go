package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestDaemonStartupClosesInterruptedRadioScan(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, "radio recovery passphrase")
	first, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	device := &store.Device{MAC: "60:38:e0:44:00:01", Host: "192.0.2.1", Name: "scan-ap"}
	if err := first.Store.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	scan := model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio0"},
		StartedAt: time.Now().UnixMilli(), Status: model.RadioScanRunning}
	if err := first.Store.CreateRadioScan(ctx, &scan); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	got, rows, err := second.Store.RadioScanByID(ctx, scan.ID)
	if err != nil || got.Status != model.RadioScanFailed || got.FinishedAt == nil || len(rows) != 0 {
		t.Fatalf("startup-recovered scan=%+v rows=%+v err=%v", got, rows, err)
	}
	var detail struct {
		Interrupted bool `json:"interrupted"`
	}
	if err := json.Unmarshal(got.Detail, &detail); err != nil || !detail.Interrupted {
		t.Fatalf("startup recovery detail=%s parsed=%+v err=%v", got.Detail, detail, err)
	}
}
