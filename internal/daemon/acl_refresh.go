package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/aiden0rchad/oonfeewrt/deploy"
	"github.com/aiden0rchad/oonfeewrt/internal/adoption"
	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const aclRefreshTimeout = 90 * time.Second

// RefreshACL upgrades only the controller's scoped rpcd ACL on an adopted
// device. It preserves the controller login, UCI configuration, ownership
// ledger, and inventory identity, then proves a fresh controller session and
// re-probes through the new scope before reporting success.
func (d *Daemon) RefreshACL(ctx context.Context, req api.RefreshACLRequest) (*api.RefreshACLResult, error) {
	ctx, cancel := context.WithTimeout(ctx, aclRefreshTimeout)
	defer cancel()
	release, err := d.deviceOps.acquire(ctx, req.DeviceID)
	if err != nil {
		return nil, err
	}
	defer release()

	dev, err := d.Store.DeviceByID(ctx, req.DeviceID)
	if err != nil {
		return nil, err
	}
	if !dev.Adopted() {
		return nil, fmt.Errorf("daemon: %s is not adopted, so its controller ACL cannot be refreshed", dev.Name)
	}
	collector := d.collectorRef()
	if collector != nil {
		defer collector.Quiesce(dev.ID)()
	}

	// Prove the stored controller identity before offering the administrator
	// credential to SSH or writing anything.
	controller, err := d.Connect(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("daemon: verify the existing controller login before ACL refresh: %w", err)
	}
	mac, err := deviceMAC(ctx, controller)
	controller.Close()
	if err != nil {
		return nil, fmt.Errorf("daemon: verify device identity before ACL refresh: %w", err)
	}
	if mac != dev.MAC {
		return nil, fmt.Errorf("daemon: ACL refresh endpoint identity is %s, expected %s", mac, dev.MAC)
	}

	endpoint, err := d.resolveWorkflowEndpoint(ctx, dev.Host)
	if err != nil {
		return nil, err
	}
	boot, err := adoption.DialSSH(ctx, adoption.SSHOptions{
		Host: endpoint.sshAddress(), Username: req.Username, Password: req.Password,
		PrivateKey: []byte(req.PrivateKey), HostKeyFP: dev.HostKeyFP,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, sshBootstrapFailure(err)
	}
	defer boot.Close()
	if dev.HostKeyFP == "" {
		if fp := boot.Fingerprint(); fp != "" {
			if err := d.Store.SetHostKeyFP(ctx, dev.ID, fp); err != nil {
				return nil, fmt.Errorf("daemon: pin SSH identity before ACL refresh: %w", err)
			}
		}
	}
	if err := boot.InstallACL(ctx, adoption.DefaultACLPath, deploy.ACL); err != nil {
		return nil, fmt.Errorf("daemon: refresh controller ACL: %w", err)
	}
	if collector != nil {
		collector.RefreshAccess(dev.ID)
	}

	controller, err = d.Connect(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("daemon: ACL was updated, but a fresh controller login could not be verified: %w", err)
	}
	defer controller.Close()
	mac, err = deviceMAC(ctx, controller)
	if err != nil || mac != dev.MAC {
		return nil, fmt.Errorf("daemon: ACL was updated, but controller identity verification failed")
	}
	caps, err := capability.Probe(ctx, controller)
	if err != nil {
		return nil, fmt.Errorf("daemon: ACL was updated and the controller login works, but capability verification failed: %w", err)
	}
	if err := d.Store.SetCapabilities(ctx, dev.ID, caps, string(caps.Class)); err != nil {
		return nil, fmt.Errorf("daemon: ACL was updated and verified, but the capability record could not be stored: %w", err)
	}
	if err := d.Store.SetFirmware(ctx, dev.ID, caps.Board.Release); err != nil {
		d.Log.Warn("could not record firmware after ACL refresh", "device", dev.MAC, "err", err)
	}
	if fresh, err := d.Store.DeviceByID(ctx, dev.ID); err == nil {
		d.Track(fresh)
	} else {
		d.Log.Warn("could not refresh collector target after ACL refresh", "device", dev.MAC, "err", err)
	}

	out := &api.RefreshACLResult{
		DeviceID: dev.ID, Name: dev.Name, ACLUpdated: true, ControllerVerified: true,
	}
	for feature, state := range caps.Features {
		if state.Buildable() {
			out.Features = append(out.Features, string(feature))
		}
	}
	for _, feature := range caps.Unobservable() {
		out.Unobservable = append(out.Unobservable, string(feature))
	}
	sortStrings(out.Features)
	sortStrings(out.Unobservable)
	id := dev.ID
	_ = d.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "audit", Severity: "info", Event: "device.acl_refreshed",
		Detail: map[string]any{"mac": dev.MAC, "controller_verified": true},
	})
	return out, nil
}
