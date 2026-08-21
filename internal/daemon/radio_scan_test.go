package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestScanRadioUsesStableKeyAndPersistsTerminalResults(t *testing.T) {
	d := openDaemon(t)
	device := seedRadioScanDevice(t, d, startMock(t), capability.Present)

	scan, rows, err := d.ScanRadio(context.Background(), device.ID, "radio0")
	if err != nil {
		t.Fatal(err)
	}
	if scan.Status != model.RadioScanCompleted || scan.Radio.Section != "radio0" ||
		len(rows) != 1 || rows[0].SSID != "neighbour-5g" || rows[0].ScanID != scan.ID {
		t.Fatalf("scan=%+v rows=%+v", scan, rows)
	}
	stored, observations, err := d.Store.LatestRadioScan(context.Background(), scan.Radio)
	if err != nil || stored.Status != model.RadioScanCompleted || len(observations) != 1 {
		t.Fatalf("stored scan=%+v rows=%+v err=%v", stored, observations, err)
	}
}

func TestScanRadioRefusesUnprovedCapabilityAndRuntimePhyIdentity(t *testing.T) {
	d := openDaemon(t)
	unknown := seedRadioScanDevice(t, d, startMock(t), capability.Unknown)
	if _, _, err := d.ScanRadio(context.Background(), unknown.ID, "radio0"); !errors.Is(err, api.ErrRadioScanUnavailable) {
		t.Fatalf("unknown capability error = %v", err)
	}

	proved := seedRadioScanDevice(t, d, startMock(t), capability.Present)
	if _, _, err := d.ScanRadio(context.Background(), proved.ID, "phy0"); !errors.Is(err, api.ErrRadioNotFound) {
		t.Fatalf("runtime PHY identity was accepted: %v", err)
	}
	if _, _, err := d.Store.LatestRadioScan(context.Background(),
		model.RadioKey{DeviceID: proved.ID, Section: "phy0"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid target created a scan row: %v", err)
	}
}

func TestScanInterfaceSkipsConfiguredButAbsentRuntimeInterfaces(t *testing.T) {
	inventory := []radio.InventoryRadio{{Key: "radio0", Interfaces: []radio.Interface{
		{Name: "", Mode: "ap"}, {Name: "phy0-mesh0", Mode: "mesh"},
	}}}
	if got, ok := scanInterface(inventory, "radio0"); !ok || got != "phy0-mesh0" {
		t.Fatalf("scan interface=%q ok=%v, want nonblank runtime fallback", got, ok)
	}
	inventory[0].Interfaces = []radio.Interface{{Name: "", Mode: "ap"}}
	if got, ok := scanInterface(inventory, "radio0"); ok || got != "" {
		t.Fatalf("configured-but-absent interface became scan target %q", got)
	}
}

func TestFinishRadioScanSurvivesLostRequestContext(t *testing.T) {
	d := openDaemon(t)
	device := &store.Device{MAC: "60:38:e0:22:00:09", Host: "192.0.2.9", Name: "scan-ap"}
	if err := d.Store.UpsertDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	scan := model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio0"},
		StartedAt: 10, Status: model.RadioScanRunning}
	if err := d.Store.CreateRadioScan(context.Background(), &scan); err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.finishRadioScan(request, scan.ID, model.RadioScanCompleted, 20,
		json.RawMessage(`{"source":"iwinfo.scan"}`), nil); err != nil {
		t.Fatal(err)
	}
	got, _, err := d.Store.RadioScanByID(context.Background(), scan.ID)
	if err != nil || got.Status != model.RadioScanCompleted || got.FinishedAt == nil || *got.FinishedAt != 20 {
		t.Fatalf("lost request left scan unfinished: %+v err=%v", got, err)
	}
}

func TestRadioScanBestEffortWorkIsDetachedButBounded(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	live := false
	started := time.Now()
	err := boundedDetached(parent, 20*time.Millisecond, func(ctx context.Context) error {
		live = ctx.Err() == nil
		<-ctx.Done()
		return ctx.Err()
	})
	if !live || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("detached bounded work: live=%v err=%v elapsed=%s", live, err, time.Since(started))
	}
}

func seedRadioScanDevice(t *testing.T, d *Daemon, host string, state capability.State) *store.Device {
	t.Helper()
	mac := "60:38:e0:22:00:01"
	if rows, _ := d.Store.Devices(context.Background()); len(rows) > 0 {
		mac = "60:38:e0:22:00:02"
	}
	credential, err := d.Keys.SealCredential(mac, "root", "good")
	if err != nil {
		t.Fatal(err)
	}
	registry := capability.NewRegistry()
	registry.Set(capability.FeatRadioScan, state)
	caps, _ := json.Marshal(registry)
	adopted := int64(1)
	device := &store.Device{MAC: mac, Host: host, Name: "scan-ap", Role: "ap",
		Scheme: "http", AdoptedAt: &adopted, CredEnc: credential, CapsJSON: string(caps)}
	if err := d.Store.UpsertDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	return device
}
