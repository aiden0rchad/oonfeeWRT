package daemon

import (
	"context"
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// beginAdoption keeps the inventory check and the external bootstrap inside
// one serial slot. The caller must hold the returned release until its adopted
// row has been committed; otherwise two gateway requests could both pass this
// check and write controller footprints to two devices.
func (d *Daemon) beginAdoption(ctx context.Context, host string,
	functions model.DeviceFunctions) (release func(), err error) {
	d.adoptMu.Lock()
	ok := false
	defer func() {
		if !ok {
			d.adoptMu.Unlock()
		}
	}()

	others, err := d.Store.Devices(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not check the device inventory before adoption: %w", err)
	}
	for _, other := range others {
		if other.Host == host && other.Adopted() {
			return nil, fmt.Errorf("%s is already adopted as %q (%s). One "+
				"address is one device: un-adopt %q first, or correct the "+
				"address if two devices really are involved",
				host, other.Name, other.MAC, other.Name)
		}
		otherFunctions := deviceFunctions(other)
		otherMayRoute := otherFunctions.Routes() ||
			(other.Functions != nil && len(otherFunctions) == 0 &&
				model.RoleOf(other.Role) == model.RoleGateway)
		if functions.Routes() && other.Adopted() && otherMayRoute {
			return nil, fmt.Errorf("%q is already the managed gateway. oonfeeWRT "+
				"does not provide gateway HA and will not adopt a second device "+
				"that can serve DHCP and routing; remove the gateway function or "+
				"un-adopt %q first", other.Name, other.Name)
		}
	}
	ok = true
	return d.adoptMu.Unlock, nil
}
