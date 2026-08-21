package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const radioScanTimeout = 45 * time.Second
const radioScanPersistTimeout = 5 * time.Second

// ScanRadio runs exactly one acknowledged radio scan. The API owns the
// acknowledgement; this layer owns device/capability validation and durable
// start/finish records.
func (d *Daemon) ScanRadio(ctx context.Context, deviceID int64, radioKey string) (model.RadioScan, []model.RadioScanBSS, error) {
	ctx, cancel := context.WithTimeout(ctx, radioScanTimeout)
	defer cancel()

	releaseOperation, err := d.deviceOps.acquire(ctx, deviceID)
	if err != nil {
		return model.RadioScan{}, nil, err
	}
	defer releaseOperation()
	releasePoll := func() {}
	if collector := d.collectorRef(); collector != nil {
		releasePoll = collector.Quiesce(deviceID)
	}
	defer releasePoll()

	device, err := d.Store.DeviceByID(ctx, deviceID)
	if err != nil {
		return model.RadioScan{}, nil, err
	}
	if !device.Adopted() {
		return model.RadioScan{}, nil, fmt.Errorf("%w: device is not adopted", api.ErrRadioScanUnavailable)
	}
	var registry capability.Registry
	if err := json.Unmarshal([]byte(device.CapsJSON), &registry); err != nil ||
		registry.State(capability.FeatRadioScan) != capability.Present {
		return model.RadioScan{}, nil, fmt.Errorf("%w: iwinfo.scan access has not been proved; re-probe this device", api.ErrRadioScanUnavailable)
	}

	client, err := d.Connect(ctx, device)
	if err != nil {
		return model.RadioScan{}, nil, err
	}
	defer client.Close()

	var access struct {
		Access bool `json:"access"`
	}
	if err := client.Call(ctx, "session", "access", map[string]any{
		"ubus_rpc_session": client.Session(), "scope": "ubus",
		"object": "iwinfo", "function": "scan",
	}, &access); err != nil || !access.Access {
		return model.RadioScan{}, nil, fmt.Errorf("%w: current device session does not prove iwinfo.scan access", api.ErrRadioScanUnavailable)
	}

	var inventoryRaw json.RawMessage
	if err := client.Call(ctx, "luci-rpc", "getWirelessDevices", nil, &inventoryRaw); err != nil {
		return model.RadioScan{}, nil, fmt.Errorf("read stable radio inventory: %w", err)
	}
	inventory, err := radio.DecodeWirelessDevices(inventoryRaw)
	if err != nil {
		return model.RadioScan{}, nil, err
	}
	iface, found := scanInterface(inventory, radioKey)
	if !found {
		return model.RadioScan{}, nil, fmt.Errorf("%w: %s has no runtime interface", api.ErrRadioNotFound, radioKey)
	}

	scan := model.RadioScan{Radio: model.RadioKey{DeviceID: deviceID, Section: radioKey},
		StartedAt: d.nowMillis(), Status: model.RadioScanRunning,
		Detail: json.RawMessage(`{"source":"iwinfo.scan"}`)}
	if err := d.Store.CreateRadioScan(ctx, &scan); err != nil {
		return scan, nil, err
	}
	fail := func(reason string, cause error) (model.RadioScan, []model.RadioScanBSS, error) {
		finished := d.nowMillis()
		detail, _ := json.Marshal(map[string]string{"source": "iwinfo.scan", "reason": reason})
		finishErr := d.finishRadioScan(ctx, scan.ID, model.RadioScanFailed, finished, detail, nil)
		scan.Status, scan.FinishedAt, scan.Detail = model.RadioScanFailed, &finished, detail
		return scan, []model.RadioScanBSS{}, errors.Join(cause, finishErr)
	}

	var raw json.RawMessage
	if err := client.Call(ctx, "iwinfo", "scan", map[string]any{"device": iface}, &raw); err != nil {
		return fail("device rejected or could not complete the scan", err)
	}
	decoded, err := radio.DecodeScan(raw)
	if err != nil {
		return fail("device returned an invalid scan result", err)
	}
	if len(decoded) == 0 {
		return fail("the device returned no usable BSS rows; this does not prove the band is quiet",
			api.ErrRadioScanUnavailable)
	}
	rows := make([]model.RadioScanBSS, 0, len(decoded))
	for _, row := range decoded {
		rows = append(rows, model.RadioScanBSS{BSSID: row.BSSID, SSID: row.SSID,
			MHz: row.MHz, Channel: row.Channel, Signal: row.Signal, Width: row.Width})
	}
	finished := d.nowMillis()
	detail, _ := json.Marshal(map[string]any{"source": "iwinfo.scan", "interface": iface,
		"observations": len(rows)})
	if err := d.finishRadioScan(ctx, scan.ID, model.RadioScanCompleted, finished, detail, rows); err != nil {
		return scan, nil, err
	}
	scan.Status, scan.FinishedAt, scan.Detail = model.RadioScanCompleted, &finished, detail
	// Persistence is complete; do not hold the device operation or collector
	// pause while best-effort audit logging waits on SQLite.
	releasePoll()
	releaseOperation()
	_ = boundedDetached(ctx, radioScanPersistTimeout, func(logCtx context.Context) error {
		return d.Store.LogEvent(logCtx, store.Event{DeviceID: &deviceID,
			Category: "radio", Severity: "info", Event: "radio.scan_completed",
			Detail: map[string]any{"radio": radioKey, "observations": len(rows)}})
	})
	return scan, rows, nil
}

func (d *Daemon) finishRadioScan(parent context.Context, scanID int64, status model.RadioScanStatus,
	finished int64, detail json.RawMessage, rows []model.RadioScanBSS) error {
	return boundedDetached(parent, radioScanPersistTimeout, func(persistCtx context.Context) error {
		return d.Store.FinishRadioScan(persistCtx, scanID, status, finished, detail, rows)
	})
}

func boundedDetached(parent context.Context, timeout time.Duration, run func(context.Context) error) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	return run(persistCtx)
}

func scanInterface(inventory []radio.InventoryRadio, key string) (string, bool) {
	for _, item := range inventory {
		if item.Key != key {
			continue
		}
		interfaces := make([]radio.Interface, 0, len(item.Interfaces))
		for _, iface := range item.Interfaces {
			if iface.Name != "" {
				interfaces = append(interfaces, iface)
			}
		}
		sort.SliceStable(interfaces, func(i, j int) bool {
			return interfaces[i].Mode == "ap" && interfaces[j].Mode != "ap"
		})
		if len(interfaces) == 0 {
			return "", false
		}
		return interfaces[0].Name, true
	}
	return "", false
}

func (d *Daemon) nowMillis() int64 {
	return time.Now().UnixMilli()
}
