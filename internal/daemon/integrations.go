package daemon

import (
	"context"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/integrations"
)

func (d *Daemon) ReadWireGuard(ctx context.Context, deviceID int64) (integrations.WireGuardResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	unavailable := func() integrations.WireGuardResult {
		return integrations.WireGuardUnavailable(deviceID,
			"WireGuard evidence is unavailable. Check router reachability, explicitly install the optional helper if wanted, and grant only its separate read ACL. No installation or permission change was attempted.")
	}
	release, err := d.deviceOps.acquire(ctx, deviceID)
	if err != nil {
		return unavailable(), nil
	}
	defer release()
	device, err := d.Store.DeviceByID(ctx, deviceID)
	if err != nil {
		return unavailable(), err
	}
	if !device.Adopted() {
		return unavailable(), nil
	}
	if collector := d.collectorRef(); collector != nil {
		defer collector.Quiesce(deviceID)()
	}
	client, err := d.Connect(ctx, device)
	if err != nil {
		return unavailable(), nil
	}
	defer client.Close()
	var raw integrations.WireGuardRaw
	if err := client.Call(ctx, "oonfeewrt-agent", "wireguard", nil, &raw); err != nil {
		return unavailable(), nil
	}
	return integrations.ParseWireGuard(deviceID, raw), nil
}
