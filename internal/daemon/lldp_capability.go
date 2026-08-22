package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/adoption"
	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const lldpCapabilityTimeout = 3 * time.Minute

func (d *Daemon) LLDPCapability(ctx context.Context, req api.LLDPCapabilityRequest) (*api.LLDPCapabilityResult, error) {
	ctx, cancel := context.WithTimeout(ctx, lldpCapabilityTimeout)
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
		return nil, fmt.Errorf("daemon: %s is not adopted", dev.Name)
	}
	collector := d.collectorRef()
	if collector != nil {
		defer collector.Quiesce(dev.ID)()
	}

	boot, err := d.openCapabilitySSH(ctx, dev, req)
	if err != nil {
		return nil, err
	}
	defer boot.Close()
	current, err := boot.PackageState(ctx)
	if err != nil {
		return nil, err
	}
	install, err := d.Store.CapabilityInstall(ctx, dev.ID, "lldp")
	if err != nil {
		return nil, err
	}

	switch req.Action {
	case "diagnose":
		diagnostics, err := boot.LLDPDiagnostics(ctx)
		if err != nil {
			return nil, err
		}
		id := dev.ID
		_ = d.Store.LogEvent(ctx, store.Event{DeviceID: &id, Category: "audit", Severity: "info",
			Event: "device.capability_diagnosed", Detail: map[string]any{"capability": "lldp"}})
		return lldpDiagnosticResult(dev, current, install, diagnostics), nil
	case "plan_configure", "configure":
		if install == nil || (install.State != "installed" && install.State != "error") ||
			len(install.Services) != 1 || install.Services[0].Name != "lldpd" {
			return nil, fmt.Errorf("daemon: LLDP must be installed with a valid rollback record before configuring interfaces")
		}
		config, err := boot.LLDPConfigPlanState(ctx)
		if err != nil {
			return nil, err
		}
		service := install.Services[0]
		if service.ConfigBaseline != "" {
			if _, err := lldpConfigRollbackNeeded(config.Export, service); err != nil {
				return nil, err
			}
		}
		plan := lldpConfigPlan(config.WiredBridgeMembers)
		planHash := lldpConfigPlanHash(config.Export, config.WiredBridgeMembers)
		if req.Action == "plan_configure" {
			return &api.LLDPCapabilityResult{DeviceID: dev.ID, Name: dev.Name, State: "configure_planned",
				PackageManager: current.Manager, RequestedPackages: []string{"lldpd"}, AddedPackages: install.AddedPackages,
				Plan: plan, PlanHash: planHash, ConfigurationState: "planned",
				ConfiguredInterfaces: config.WiredBridgeMembers}, nil
		}
		if req.PlanHash != planHash {
			return nil, fmt.Errorf("daemon: the LLDP interface plan changed; review it again before applying")
		}
		if service.ConfigBaseline == "" {
			service.ConfigBaseline = config.Export
		}
		expected, err := rewriteLLDPInterfaces(config.Export, config.WiredBridgeMembers)
		if err != nil {
			return nil, err
		}
		service.ConfigApplied = expected
		service.ConfiguredInterfaces = append([]string(nil), config.WiredBridgeMembers...)
		if err := d.Store.UpdateCapabilityServices(ctx, dev.ID, "lldp", []store.CapabilityServiceState{service}); err != nil {
			return nil, err
		}
		if err := boot.ConfigureLLDP(ctx, config.Export, config.WiredBridgeMembers); err != nil {
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		after, err := boot.LLDPConfigState(ctx)
		if err != nil {
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if after.Export != expected {
			err = fmt.Errorf("daemon: LLDP configuration read-back differed from the authorized interface plan")
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if !containsAll(after.RuntimeInterfaces, config.WiredBridgeMembers) {
			err = fmt.Errorf("configured LLDP interfaces were not active after restart")
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if install.State == "error" {
			if err := d.Store.CompleteCapabilityConfiguration(ctx, dev.ID, "lldp"); err != nil {
				return nil, err
			}
		}
		if collector != nil {
			collector.RefreshAccess(dev.ID)
			d.rediscoverLLDPFleet(ctx, collector)
		}
		id := dev.ID
		_ = d.Store.LogEvent(ctx, store.Event{DeviceID: &id, Category: "audit", Severity: "info",
			Event: "device.capability_configured", Detail: map[string]any{
				"capability": "lldp", "interfaces": service.ConfiguredInterfaces,
			}})
		enabled, running := current.LLDPEnabled, current.LLDPRunning
		return &api.LLDPCapabilityResult{DeviceID: dev.ID, Name: dev.Name, State: "installed",
			PackageManager: current.Manager, RequestedPackages: []string{"lldpd"}, AddedPackages: install.AddedPackages,
			ServiceEnabled: &enabled, ServiceRunning: &running, ConfigurationState: "configured",
			ConfiguredInterfaces: service.ConfiguredInterfaces}, nil
	case "plan_install":
		if install != nil && (install.State != "error" || install.InstalledAt != nil) {
			return nil, fmt.Errorf("daemon: LLDP capability state is already %s", install.State)
		}
		plan, err := boot.LLDPPlan(ctx, current.Manager, nil)
		if err != nil {
			return nil, err
		}
		return lldpPlanResult(dev.ID, dev.Name, "install_planned", current, plan, false), nil
	case "install":
		if install != nil && (install.State != "error" || install.InstalledAt != nil) {
			return nil, fmt.Errorf("daemon: LLDP capability state is already %s", install.State)
		}
		plan, err := boot.LLDPPlan(ctx, current.Manager, nil)
		if err != nil {
			return nil, err
		}
		if req.PlanHash != lldpPlanHash("install", current.Manager, plan) {
			return nil, fmt.Errorf("daemon: the LLDP package plan changed; review it again before installing")
		}
		baseline := append([]string(nil), current.Installed...)
		services := []store.CapabilityServiceState{{
			Name: "lldpd", WasEnabled: current.LLDPEnabled, WasRunning: current.LLDPRunning,
		}}
		if install != nil {
			if install.PackageManager != current.Manager || !slices.Equal(install.RequestedPackages, []string{"lldpd"}) ||
				len(install.Services) != 1 || install.Services[0].Name != "lldpd" {
				return nil, fmt.Errorf("daemon: LLDP install retry has an invalid rollback record")
			}
			baseline = append([]string(nil), install.BaselinePackages...)
			services = append([]store.CapabilityServiceState(nil), install.Services...)
		}
		ownedPackages, err := lldpInstallOwnedPackages(current, install, plan)
		if err != nil {
			return nil, err
		}
		if err := d.Store.BeginCapabilityInstall(ctx, dev.ID, "lldp", current.Manager,
			[]string{"lldpd"}, baseline, services); err != nil {
			return nil, err
		}
		if err := d.Store.UpdateCapabilityAddedPackages(ctx, dev.ID, "lldp", ownedPackages); err != nil {
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if err := boot.InstallLLDP(ctx, current.Manager); err != nil {
			d.recordLLDPAddedPackages(ctx, boot, dev.ID, ownedPackages)
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		after, err := boot.PackageState(ctx)
		var added []string
		if err == nil {
			added = packageIntersection(ownedPackages, after.Installed)
			if updateErr := d.Store.UpdateCapabilityAddedPackages(ctx, dev.ID, "lldp", added); updateErr != nil {
				err = updateErr
			}
		}
		if err == nil {
			unexpected := packageDifference(packageDifference(after.Installed, baseline), ownedPackages)
			if len(unexpected) > 0 {
				err = fmt.Errorf("daemon: LLDP installation added packages absent from the authorized package-manager plan: %s", strings.Join(unexpected, ", "))
			}
		}
		if err != nil || !slices.Contains(after.Installed, "lldpd") || !after.LLDPEnabled || !after.LLDPRunning {
			if err == nil {
				err = fmt.Errorf("LLDP package or service verification failed")
			}
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if err := d.Store.CompleteCapabilityInstall(ctx, dev.ID, "lldp", added, services); err != nil {
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if collector != nil {
			collector.RefreshAccess(dev.ID)
			d.rediscoverLLDPFleet(ctx, collector)
		}
		id := dev.ID
		_ = d.Store.LogEvent(ctx, store.Event{DeviceID: &id, Category: "audit", Severity: "info",
			Event: "device.capability_installed", Detail: map[string]any{
				"capability": "lldp", "package_manager": current.Manager, "added_packages": added,
			}})
		enabled, running := after.LLDPEnabled, after.LLDPRunning
		return &api.LLDPCapabilityResult{DeviceID: dev.ID, Name: dev.Name, State: "installed",
			PackageManager: current.Manager, RequestedPackages: []string{"lldpd"}, AddedPackages: added,
			ServiceEnabled: &enabled, ServiceRunning: &running}, nil
	case "plan_remove", "remove":
		if install == nil {
			return nil, fmt.Errorf("daemon: the controller has no LLDP capability rollback record for %s", dev.Name)
		}
		if len(install.Services) != 1 || install.Services[0].Name != "lldpd" {
			return nil, fmt.Errorf("daemon: LLDP rollback record has no valid service baseline")
		}
		plan, removePackages, config, err := lldpRemovalPlan(ctx, boot, current, install)
		if err != nil {
			return nil, err
		}
		if req.Action == "plan_remove" {
			return lldpPlanResult(dev.ID, dev.Name, "remove_planned", current, plan, true), nil
		}
		if req.PlanHash != lldpPlanHash("remove", current.Manager, plan) {
			return nil, fmt.Errorf("daemon: the LLDP removal plan changed; review it again before removing")
		}
		if err := d.Store.BeginCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
			return nil, err
		}
		service := install.Services[0]
		if service.ConfigBaseline != "" && config != nil {
			restore, err := lldpConfigRollbackNeeded(config.Export, service)
			if err != nil {
				_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
				return nil, err
			}
			if restore {
				if err := boot.RestoreLLDPConfig(ctx, config.Export, service.ConfigBaseline); err != nil {
					_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
					return nil, err
				}
			}
			service.ConfigBaseline, service.ConfigApplied, service.ConfiguredInterfaces = "", "", nil
			if err := d.Store.UpdateCapabilityServices(ctx, dev.ID, "lldp", []store.CapabilityServiceState{service}); err != nil {
				return nil, err
			}
		}
		if slices.Contains(current.Installed, "lldpd") {
			if len(install.Services) != 1 || install.Services[0].Name != "lldpd" {
				err := fmt.Errorf("daemon: LLDP rollback record has no valid service baseline")
				_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
				return nil, err
			}
			baseline := install.Services[0]
			if err := boot.RemoveLLDP(ctx, current.Manager, removePackages,
				baseline.WasEnabled, baseline.WasRunning); err != nil {
				_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
				return nil, err
			}
		} else if len(removePackages) > 0 {
			if err := boot.RemoveLLDP(ctx, current.Manager, removePackages, false, false); err != nil {
				_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
				return nil, err
			}
		}
		after, err := boot.PackageState(ctx)
		if err == nil && after.Manager != current.Manager {
			err = fmt.Errorf("daemon: package manager changed during LLDP rollback")
		}
		if err == nil {
			err = verifyLLDPRemoval(after, install, removePackages)
		}
		if err != nil {
			_ = d.Store.MarkCapabilityInstallError(ctx, dev.ID, "lldp", err.Error())
			return nil, err
		}
		if err := d.Store.CompleteCapabilityRemoval(ctx, dev.ID, "lldp"); err != nil {
			return nil, err
		}
		if collector != nil {
			collector.RefreshAccess(dev.ID)
			d.rediscoverLLDPFleet(ctx, collector)
		}
		id := dev.ID
		_ = d.Store.LogEvent(ctx, store.Event{DeviceID: &id, Category: "audit", Severity: "info",
			Event: "device.capability_removed", Detail: map[string]any{
				"capability": "lldp", "removed_packages": removePackages,
				"package_count": len(after.Installed), "service_enabled": after.LLDPEnabled,
				"service_running": after.LLDPRunning,
			}})
		return &api.LLDPCapabilityResult{DeviceID: dev.ID, Name: dev.Name, State: "not_installed",
			RequestedPackages: []string{"lldpd"}, AddedPackages: []string{}}, nil
	default:
		return nil, fmt.Errorf("daemon: unsupported LLDP capability action %q", req.Action)
	}
}

func lldpDiagnosticResult(dev *store.Device, current adoption.PackageState, install *store.CapabilityInstall, diagnostics string) *api.LLDPCapabilityResult {
	enabled, running := current.LLDPEnabled, current.LLDPRunning
	out := &api.LLDPCapabilityResult{DeviceID: dev.ID, Name: dev.Name, State: "not_installed",
		PackageManager: current.Manager, RequestedPackages: []string{"lldpd"}, AddedPackages: []string{},
		ServiceEnabled: &enabled, ServiceRunning: &running, Diagnostics: diagnostics}
	if install == nil {
		return out
	}
	out.State = install.State
	out.RequestedPackages = install.RequestedPackages
	out.AddedPackages = install.AddedPackages
	out.Detail = install.Detail
	if len(install.Services) == 1 {
		service := install.Services[0]
		out.ConfiguredInterfaces = service.ConfiguredInterfaces
		switch {
		case install.State == "error" && service.ConfigBaseline != "":
			out.ConfigurationState = "incomplete"
		case service.ConfigApplied != "":
			out.ConfigurationState = "configured"
		case service.ConfigBaseline != "":
			out.ConfigurationState = "incomplete"
		default:
			out.ConfigurationState = "package_default"
		}
	}
	return out
}

func (d *Daemon) recordLLDPAddedPackages(ctx context.Context, boot *adoption.SSHBootstrap, deviceID int64, owned []string) {
	after, err := boot.PackageState(ctx)
	if err != nil {
		return
	}
	_ = d.Store.UpdateCapabilityAddedPackages(ctx, deviceID, "lldp", packageIntersection(owned, after.Installed))
}

func verifyLLDPRemoval(after adoption.PackageState, install *store.CapabilityInstall, removed []string) error {
	for _, name := range install.BaselinePackages {
		if !slices.Contains(after.Installed, name) {
			return fmt.Errorf("daemon: LLDP rollback did not preserve baseline package %s", name)
		}
	}
	for _, name := range removed {
		if slices.Contains(after.Installed, name) {
			return fmt.Errorf("daemon: LLDP rollback did not remove package %s", name)
		}
	}
	service := install.Services[0]
	expectInstalled := slices.Contains(install.BaselinePackages, "lldpd")
	installed := slices.Contains(after.Installed, "lldpd")
	if installed != expectInstalled {
		return fmt.Errorf("daemon: LLDP rollback did not restore the recorded package baseline")
	}
	if installed && (after.LLDPEnabled != service.WasEnabled || after.LLDPRunning != service.WasRunning) {
		return fmt.Errorf("daemon: LLDP rollback did not restore the recorded service baseline")
	}
	if !installed && (after.LLDPEnabled || after.LLDPRunning) {
		return fmt.Errorf("daemon: removed LLDP service remains enabled or running")
	}
	return nil
}

type lldpRediscoverer interface {
	Rediscover(int64)
}

func (d *Daemon) rediscoverLLDPFleet(ctx context.Context, collector lldpRediscoverer) {
	devices, err := d.Store.Devices(ctx)
	if err != nil {
		d.Log.Warn("could not schedule fleet LLDP rediscovery", "err", err)
		return
	}
	rediscoverAdoptedLLDPPeers(devices, collector)
}

func rediscoverAdoptedLLDPPeers(devices []*store.Device, collector lldpRediscoverer) {
	for _, device := range devices {
		if device.Adopted() {
			collector.Rediscover(device.ID)
		}
	}
}

func (d *Daemon) openCapabilitySSH(ctx context.Context, dev *store.Device, req api.LLDPCapabilityRequest) (*adoption.SSHBootstrap, error) {
	controller, err := d.Connect(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("daemon: verify controller identity before LLDP capability action: %w", err)
	}
	mac, macErr := deviceMAC(ctx, controller)
	controller.Close()
	if macErr != nil || mac != dev.MAC {
		return nil, fmt.Errorf("daemon: device identity verification failed before LLDP capability action")
	}
	endpoint, err := d.resolveWorkflowEndpoint(ctx, dev.Host)
	if err != nil {
		return nil, err
	}
	boot, err := adoption.DialSSH(ctx, adoption.SSHOptions{
		Host: endpoint.sshAddress(), Username: req.Username, Password: req.Password,
		PrivateKey: []byte(req.PrivateKey), HostKeyFP: dev.HostKeyFP, Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, sshBootstrapFailure(err)
	}
	if dev.HostKeyFP == "" {
		if fp := boot.Fingerprint(); fp != "" {
			if err := d.Store.SetHostKeyFP(ctx, dev.ID, fp); err != nil {
				boot.Close()
				return nil, fmt.Errorf("daemon: pin SSH identity before LLDP capability action: %w", err)
			}
		}
	}
	return boot, nil
}

func lldpPlanResult(deviceID int64, name, state string, packages adoption.PackageState, plan string, remove bool) *api.LLDPCapabilityResult {
	action := "install"
	if remove {
		action = "remove"
	}
	enabled, running := packages.LLDPEnabled, packages.LLDPRunning
	return &api.LLDPCapabilityResult{DeviceID: deviceID, Name: name, State: state,
		PackageManager: packages.Manager, RequestedPackages: []string{"lldpd"}, AddedPackages: []string{},
		Plan: plan, PlanHash: lldpPlanHash(action, packages.Manager, plan),
		ServiceEnabled: &enabled, ServiceRunning: &running}
}

func lldpRemovalPlan(ctx context.Context, boot *adoption.SSHBootstrap, current adoption.PackageState, install *store.CapabilityInstall) (string, []string, *adoption.LLDPConfigState, error) {
	var config *adoption.LLDPConfigState
	service := install.Services[0]
	enabled, running := "disabled", "stopped"
	if service.WasEnabled {
		enabled = "enabled"
	}
	if service.WasRunning {
		running = "running"
	}
	prefix := fmt.Sprintf("Recorded pre-install lldpd service baseline: %s and %s.\n", enabled, running)
	if service.ConfigBaseline != "" && !slices.Contains(current.Installed, "lldpd") {
		return "", nil, nil, fmt.Errorf("daemon: managed LLDP configuration cannot be restored while lldpd is absent; retaining the rollback record")
	}
	if service.ConfigBaseline != "" {
		state, err := boot.LLDPConfigPlanState(ctx)
		if err != nil {
			return "", nil, nil, err
		}
		if _, err := lldpConfigRollbackNeeded(state.Export, service); err != nil {
			return "", nil, nil, err
		}
		config = &state
		prefix += "Restore the exact pre-configuration /etc/config/lldpd baseline and restart only lldpd.\n"
	}
	removePackages, err := lldpRemovalPackages(current, install)
	if err != nil {
		return "", nil, nil, err
	}
	if len(removePackages) > 0 {
		plan, err := boot.LLDPPlan(ctx, current.Manager, removePackages)
		if err != nil {
			return "", nil, nil, err
		}
		if err := validateLLDPRemovalPlan(plan, removePackages); err != nil {
			return "", nil, nil, err
		}
		return prefix + plan, removePackages, config, nil
	}
	if slices.Contains(current.Installed, "lldpd") {
		return prefix + "Keep pre-existing package lldpd; restore its pre-install enabled/running service state.", nil, config, nil
	}
	return prefix + "All controller-added LLDP packages are already absent; clear the controller rollback record only.", nil, config, nil
}

func lldpRemovalPackages(current adoption.PackageState, install *store.CapabilityInstall) ([]string, error) {
	if current.Manager != install.PackageManager {
		return nil, fmt.Errorf("daemon: package manager changed since the LLDP rollback baseline was recorded")
	}
	for _, name := range install.BaselinePackages {
		if !slices.Contains(current.Installed, name) {
			return nil, fmt.Errorf("daemon: baseline package %s changed outside the controller; refusing LLDP rollback", name)
		}
	}
	if install.InstalledAt == nil {
		owned := append([]string(nil), install.AddedPackages...)
		if !slices.Contains(install.BaselinePackages, "lldpd") && slices.Contains(current.Installed, "lldpd") &&
			!slices.Contains(owned, "lldpd") {
			owned = append(owned, "lldpd")
		}
		unknown := packageDifference(packageDifference(current.Installed, install.BaselinePackages), owned)
		if len(unknown) > 0 {
			return nil, fmt.Errorf("daemon: interrupted LLDP install left packages with unproven ownership (%s); refusing automatic rollback", strings.Join(unknown, ", "))
		}
		return packageIntersection(owned, current.Installed), nil
	}
	return packageIntersection(install.AddedPackages, current.Installed), nil
}

var lldpPlanPackageRow = regexp.MustCompile(`^(?:\([0-9]+/[0-9]+\) )?(Installing|Upgrading|Downgrading|Reinstalling|Replacing|Purging|Removing) ([A-Za-z0-9][A-Za-z0-9+_.-]*)(?: \(| on | from |$)`)
var lldpPlanOPKGRemoveRow = regexp.MustCompile(`^Removing package ([A-Za-z0-9][A-Za-z0-9+_.-]*)(?: from |$)`)

type lldpPackageMutation struct {
	verb string
	name string
}

func lldpPlanMutations(plan string) []lldpPackageMutation {
	var mutations []lldpPackageMutation
	for _, line := range strings.Split(plan, "\n") {
		line = strings.TrimSpace(line)
		if match := lldpPlanPackageRow.FindStringSubmatch(line); len(match) == 3 {
			mutations = append(mutations, lldpPackageMutation{verb: match[1], name: match[2]})
			continue
		}
		if match := lldpPlanOPKGRemoveRow.FindStringSubmatch(line); len(match) == 2 {
			mutations = append(mutations, lldpPackageMutation{verb: "Removing", name: match[1]})
		}
	}
	return mutations
}

func lldpInstallOwnedPackages(current adoption.PackageState, install *store.CapabilityInstall, plan string) ([]string, error) {
	baseline := current.Installed
	var owned []string
	if install != nil {
		baseline = install.BaselinePackages
		owned = append(owned, install.AddedPackages...)
		known := append([]string(nil), owned...)
		if !slices.Contains(baseline, "lldpd") {
			known = append(known, "lldpd")
		}
		unknown := packageDifference(packageDifference(current.Installed, baseline), known)
		if len(unknown) > 0 {
			return nil, fmt.Errorf("daemon: LLDP install retry found packages with unproven ownership (%s); refusing to continue", strings.Join(unknown, ", "))
		}
		if slices.Contains(current.Installed, "lldpd") && !slices.Contains(baseline, "lldpd") {
			owned = append(owned, "lldpd")
		}
	}
	for _, mutation := range lldpPlanMutations(plan) {
		if mutation.verb != "Installing" {
			return nil, fmt.Errorf("daemon: LLDP package plan would %s pre-existing package %s; refusing a non-reversible install", strings.ToLower(mutation.verb), mutation.name)
		}
		if !slices.Contains(baseline, mutation.name) {
			owned = append(owned, mutation.name)
		}
	}
	sort.Strings(owned)
	owned = slices.Compact(owned)
	if !slices.Contains(baseline, "lldpd") && !slices.Contains(owned, "lldpd") {
		return nil, fmt.Errorf("daemon: package-manager plan did not prove that lldpd would be installed; refusing router changes")
	}
	return owned, nil
}

func validateLLDPRemovalPlan(plan string, removePackages []string) error {
	mutations := lldpPlanMutations(plan)
	seen := make(map[string]bool, len(removePackages))
	for _, mutation := range mutations {
		if mutation.verb != "Removing" && mutation.verb != "Purging" {
			return fmt.Errorf("daemon: LLDP removal plan would %s package %s; refusing non-rollback changes", strings.ToLower(mutation.verb), mutation.name)
		}
		if !slices.Contains(removePackages, mutation.name) {
			return fmt.Errorf("daemon: LLDP removal plan would remove unrecorded package %s", mutation.name)
		}
		seen[mutation.name] = true
	}
	for _, name := range removePackages {
		if !seen[name] {
			return fmt.Errorf("daemon: package-manager removal plan did not prove removal of %s", name)
		}
	}
	return nil
}

var lldpInterfaceIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,31}$`)

func rewriteLLDPInterfaces(exported string, interfaces []string) (string, error) {
	if exported == "" || !strings.HasSuffix(exported, "\n") || len(interfaces) > 32 {
		return "", fmt.Errorf("daemon: invalid LLDP configuration export")
	}
	for _, name := range interfaces {
		if !lldpInterfaceIdentifier.MatchString(name) {
			return "", fmt.Errorf("daemon: invalid LLDP interface name %q", name)
		}
	}
	lines := strings.Split(exported, "\n")
	start, end := -1, len(lines)
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "config" {
			continue
		}
		if start >= 0 {
			end = index
			break
		}
		if len(fields) == 3 && fields[1] == "lldpd" && fields[2] == "'config'" {
			start = index
		}
	}
	if start < 0 {
		return "", fmt.Errorf("daemon: LLDP configuration export has no named lldpd.config section")
	}
	clean := make([]string, 0, len(lines)+len(interfaces))
	insert := -1
	for index, line := range lines {
		if index == end {
			insert = len(clean)
		}
		fields := strings.Fields(line)
		if index > start && index < end && len(fields) >= 2 && fields[0] == "list" && fields[1] == "interface" {
			if len(fields) != 3 || len(fields[2]) < 2 || fields[2][0] != '\'' || fields[2][len(fields[2])-1] != '\'' {
				return "", fmt.Errorf("daemon: invalid lldpd.config.interface export")
			}
			continue
		}
		clean = append(clean, line)
	}
	if insert < 0 {
		insert = len(clean)
	}
	for insert > start+1 && strings.TrimSpace(clean[insert-1]) == "" {
		insert--
	}
	added := make([]string, len(interfaces))
	for index, name := range interfaces {
		added[index] = "\tlist interface '" + name + "'"
	}
	clean = slices.Insert(clean, insert, added...)
	return strings.Join(clean, "\n"), nil
}

func lldpConfigRollbackNeeded(current string, service store.CapabilityServiceState) (bool, error) {
	if current == service.ConfigBaseline {
		return false, nil
	}
	if service.ConfigApplied != "" && current == service.ConfigApplied {
		return true, nil
	}
	return false, fmt.Errorf("daemon: the LLDP configuration changed outside the controller; refusing to overwrite it")
}

func lldpConfigPlan(interfaces []string) string {
	return "Replace only lldpd.config.interface with: " + strings.Join(interfaces, ", ") +
		". Commit only /etc/config/lldpd, restart only lldpd, and verify every listed runtime interface. " +
		"The controller stores the exact current UCI export for drift-checked rollback."
}

func lldpConfigPlanHash(current string, interfaces []string) string {
	sum := sha256.Sum256([]byte("configure\n" + current + "\n" + strings.Join(interfaces, "\n")))
	return hex.EncodeToString(sum[:])
}

func containsAll(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, value := range have {
		set[value] = true
	}
	for _, value := range want {
		if !set[value] {
			return false
		}
	}
	return true
}

func lldpPlanHash(action, manager, plan string) string {
	sum := sha256.Sum256([]byte(action + "\n" + manager + "\n" + strings.TrimSpace(plan)))
	return hex.EncodeToString(sum[:])
}

func packageDifference(after, before []string) []string {
	baseline := make(map[string]bool, len(before))
	for _, pkg := range before {
		baseline[pkg] = true
	}
	var added []string
	for _, pkg := range after {
		if !baseline[pkg] {
			added = append(added, pkg)
		}
	}
	sort.Strings(added)
	return added
}

func packageIntersection(left, right []string) []string {
	present := make(map[string]bool, len(right))
	for _, pkg := range right {
		present[pkg] = true
	}
	var common []string
	for _, pkg := range left {
		if present[pkg] {
			common = append(common, pkg)
		}
	}
	sort.Strings(common)
	return common
}
