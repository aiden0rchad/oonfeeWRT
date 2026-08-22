package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/adoption"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type recordingRediscoverer []int64

func (r *recordingRediscoverer) Rediscover(id int64) { *r = append(*r, id) }

func TestLLDPPlanBindingAndPackageDifferenceAreCanonical(t *testing.T) {
	if lldpPlanHash("install", "apk", " plan\n") != lldpPlanHash("install", "apk", "plan") {
		t.Fatal("cosmetic surrounding whitespace changed the plan binding")
	}
	if lldpPlanHash("remove", "apk", "plan") == lldpPlanHash("install", "apk", "plan") {
		t.Fatal("install plan could authorize removal")
	}
	got := packageDifference([]string{"lldpd", "libcap", "base-files"}, []string{"base-files"})
	if !slices.Equal(got, []string{"libcap", "lldpd"}) {
		t.Fatalf("difference=%v", got)
	}
}

func TestLLDPConfigPlanBindsCurrentExportAndExactInterfaces(t *testing.T) {
	interfaces := []string{"lan1", "lan3"}
	plan := lldpConfigPlan(interfaces)
	if !strings.Contains(plan, "lan1, lan3") || !strings.Contains(plan, "restart only lldpd") {
		t.Fatalf("plan=%q", plan)
	}
	base := lldpConfigPlanHash("baseline-a", interfaces)
	if base == lldpConfigPlanHash("baseline-b", interfaces) ||
		base == lldpConfigPlanHash("baseline-a", []string{"lan3"}) {
		t.Fatal("configuration hash did not bind baseline and exact interfaces")
	}
	if !containsAll([]string{"lan1", "lan2", "lan3"}, interfaces) || containsAll([]string{"lan1"}, interfaces) {
		t.Fatal("runtime interface verification is incorrect")
	}
}

func TestLLDPConfigRollbackIntentIsDurableBeforeReadback(t *testing.T) {
	baseline := "package lldpd\n\nconfig lldpd 'config'\n\toption description 'router'\n\tlist interface 'old0'\n\nconfig chassis 'extra'\n\toption foo 'bar'\n"
	want := "package lldpd\n\nconfig lldpd 'config'\n\toption description 'router'\n\tlist interface 'lan1'\n\tlist interface 'lan3'\n\nconfig chassis 'extra'\n\toption foo 'bar'\n"
	applied, err := rewriteLLDPInterfaces(baseline, []string{"lan1", "lan3"})
	if err != nil {
		t.Fatal(err)
	}
	if applied != want {
		t.Fatalf("applied export:\n%s\nwant:\n%s", applied, want)
	}
	service := store.CapabilityServiceState{ConfigBaseline: baseline, ConfigApplied: applied}
	if restore, err := lldpConfigRollbackNeeded(baseline, service); err != nil || restore {
		t.Fatalf("pre-commit baseline restore=%t err=%v", restore, err)
	}
	if restore, err := lldpConfigRollbackNeeded(applied, service); err != nil || !restore {
		t.Fatalf("committed config restore=%t err=%v", restore, err)
	}
	if _, err := lldpConfigRollbackNeeded(strings.Replace(applied, "router", "externally changed", 1), service); err == nil {
		t.Fatal("external LLDP drift was accepted for rollback")
	}
}

func TestLLDPDiagnosticResultRetainsDurableInstalledConfiguration(t *testing.T) {
	result := lldpDiagnosticResult(
		&store.Device{ID: 7, Name: "wrt"},
		adoption.PackageState{Manager: "apk", LLDPEnabled: true, LLDPRunning: true},
		&store.CapabilityInstall{
			State:             "installed",
			RequestedPackages: []string{"lldpd"},
			AddedPackages:     []string{"lldpd", "libcap"},
			Services: []store.CapabilityServiceState{{
				Name: "lldpd", ConfigBaseline: "before", ConfigApplied: "after",
				ConfiguredInterfaces: []string{"lan1", "lan3"},
			}},
		},
		"RUNTIME_INTERFACES\n{}",
	)
	if result.State != "installed" || result.ConfigurationState != "configured" ||
		!slices.Equal(result.AddedPackages, []string{"lldpd", "libcap"}) ||
		!slices.Equal(result.ConfiguredInterfaces, []string{"lan1", "lan3"}) {
		t.Fatalf("diagnostic erased durable install state: %+v", result)
	}
}

func TestPackageIntersectionKeepsOnlyControllerAddedPackages(t *testing.T) {
	got := packageIntersection([]string{"lldpd", "libevent", "libcap"}, []string{"base-files", "libcap", "lldpd"})
	want := []string{"libcap", "lldpd"}
	if !slices.Equal(got, want) {
		t.Fatalf("intersection=%v want %v", got, want)
	}
}

func TestLLDPInterruptedInstallRemovalNeverClaimsUnrecordedPackages(t *testing.T) {
	install := &store.CapabilityInstall{
		PackageManager:   "apk",
		BaselinePackages: []string{"base-files"},
		AddedPackages:    []string{"libcap"},
	}
	got, err := lldpRemovalPackages(adoption.PackageState{
		Manager: "apk", Installed: []string{"base-files", "libcap", "lldpd"},
	}, install)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"libcap", "lldpd"}) {
		t.Fatalf("interrupted removal packages=%v", got)
	}
	if _, err := lldpRemovalPackages(adoption.PackageState{
		Manager: "apk", Installed: []string{"base-files", "curl", "libcap", "libevent", "lldpd"},
	}, install); err == nil {
		t.Fatal("unrecorded packages were claimed as controller-owned")
	}

	installedAt := time.Unix(1, 0)
	install.InstalledAt = &installedAt
	got, err = lldpRemovalPackages(adoption.PackageState{
		Manager: "apk", Installed: []string{"base-files", "libcap", "libevent", "lldpd"},
	}, install)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"libcap"}) {
		t.Fatalf("completed removal claimed unrecorded packages=%v", got)
	}
	if _, err := lldpRemovalPackages(adoption.PackageState{Manager: "opkg", Installed: []string{"base-files"}}, install); err == nil {
		t.Fatal("package-manager drift was accepted")
	}
	if _, err := lldpRemovalPackages(adoption.PackageState{Manager: "apk"}, install); err == nil {
		t.Fatal("missing baseline package was accepted")
	}
}

func TestLLDPInstallOwnershipComesFromBoundSimulatorRows(t *testing.T) {
	for _, test := range []struct {
		manager string
		plan    string
	}{
		{"apk", "(1/3) Installing libcap (2.74-r0)\n(2/3) Installing lldpd (1.0.20-r0)\nOK: 8 MiB"},
		{"opkg", "Installing libcap (2.74-r0) to root...\nInstalling lldpd (1.0.20-r0) to root..."},
	} {
		t.Run(test.manager, func(t *testing.T) {
			got, err := lldpInstallOwnedPackages(adoption.PackageState{
				Manager: test.manager, Installed: []string{"base-files"},
			}, nil, test.plan)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, []string{"libcap", "lldpd"}) {
				t.Fatalf("owned packages=%v", got)
			}
		})
	}
	if _, err := lldpInstallOwnedPackages(adoption.PackageState{Manager: "apk", Installed: []string{"base-files"}}, nil,
		"Package manager reported no package changes."); err == nil {
		t.Fatal("unparseable plan was allowed to install lldpd")
	}
	for _, row := range []string{
		"(1/1) Upgrading libcap (2.73-r0 -> 2.74-r0)",
		"Downgrading libcap (2.74-r0 -> 2.73-r0)",
		"Reinstalling libcap (2.74-r0)",
		"Replacing libcap (2.73-r0 -> 2.74-r0)",
	} {
		plan := row + "\nInstalling lldpd (1.0.20-r0)"
		if _, err := lldpInstallOwnedPackages(adoption.PackageState{
			Manager: "apk", Installed: []string{"base-files", "libcap"},
		}, nil, plan); err == nil {
			t.Fatalf("baseline mutation was accepted: %q", row)
		}
	}
	install := &store.CapabilityInstall{BaselinePackages: []string{"base-files"}, AddedPackages: []string{"libcap"}}
	if _, err := lldpInstallOwnedPackages(adoption.PackageState{
		Manager: "apk", Installed: []string{"base-files", "curl", "libcap"},
	}, install, "Installing lldpd (1-r0)"); err == nil {
		t.Fatal("retry claimed an unrelated package installed after its baseline")
	}
}

func TestLLDPRemovalPlanCannotAutoPurgeUnrecordedPackages(t *testing.T) {
	for _, test := range []struct {
		name string
		plan string
	}{
		{"apk exact", "(1/2) Purging libcap (2.74-r0)\n(2/2) Purging lldpd (1.0.20-r0)"},
		{"opkg exact", "Removing package libcap from root...\nRemoving package lldpd from root..."},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLLDPRemovalPlan(test.plan, []string{"libcap", "lldpd"}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, plan := range map[string]string{
		"dependency auto-purge": "Purging libcap (2.74-r0)\nPurging libevent (2.1-r0)\nPurging lldpd (1.0.20-r0)",
		"baseline upgrade":      "Purging lldpd (1.0.20-r0)\nUpgrading base-files (1-r0 -> 2-r0)",
		"unproven target":       "Purging libcap (2.74-r0)",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateLLDPRemovalPlan(plan, []string{"libcap", "lldpd"}); err == nil {
				t.Fatal("unsafe removal plan was accepted")
			}
		})
	}
}

func TestLLDPRemovalPlanNamesExactRecordedServiceBaseline(t *testing.T) {
	for _, test := range []struct {
		name             string
		enabled, running bool
		want             string
	}{
		{"disabled and stopped", false, false, "disabled and stopped"},
		{"disabled and running", false, true, "disabled and running"},
		{"enabled and stopped", true, false, "enabled and stopped"},
		{"enabled and running", true, true, "enabled and running"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, _, _, err := lldpRemovalPlan(context.Background(), nil, adoption.PackageState{}, &store.CapabilityInstall{
				Services: []store.CapabilityServiceState{{Name: "lldpd", WasEnabled: test.enabled, WasRunning: test.running}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(plan, "Recorded pre-install lldpd service baseline: "+test.want+".\n") {
				t.Fatalf("plan=%q", plan)
			}
		})
	}
}

func TestVerifyLLDPRemovalRequiresExactPackageAndServiceBaseline(t *testing.T) {
	install := &store.CapabilityInstall{
		BaselinePackages: []string{"base-files", "lldpd"},
		Services:         []store.CapabilityServiceState{{Name: "lldpd", WasEnabled: true, WasRunning: false}},
	}
	if err := verifyLLDPRemoval(adoption.PackageState{
		Installed: []string{"base-files", "lldpd"}, LLDPEnabled: true,
	}, install, []string{"libcap"}); err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]adoption.PackageState{
		"controller-added package remains": {Installed: []string{"base-files", "lldpd", "libcap"}, LLDPEnabled: true},
		"pre-existing lldpd was removed":   {Installed: []string{"base-files"}},
		"service baseline differs":         {Installed: []string{"base-files", "lldpd"}, LLDPEnabled: false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyLLDPRemoval(state, install, []string{"libcap"}); err == nil {
				t.Fatal("invalid rollback state was accepted")
			}
		})
	}

	controllerAdded := &store.CapabilityInstall{
		BaselinePackages: []string{"base-files"},
		Services:         []store.CapabilityServiceState{{Name: "lldpd"}},
	}
	if err := verifyLLDPRemoval(adoption.PackageState{Installed: []string{"base-files"}}, controllerAdded,
		[]string{"libcap", "lldpd"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyLLDPRemoval(adoption.PackageState{Installed: []string{"base-files"}, LLDPRunning: true},
		controllerAdded, []string{"libcap", "lldpd"}); err == nil {
		t.Fatal("removed service still running was accepted")
	}
}

func TestLLDPConfigurationRediscoveryIncludesEveryAdoptedPeer(t *testing.T) {
	devices := []*store.Device{
		{ID: 1, AdoptedAt: new(int64)},
		{ID: 2, AdoptedAt: new(int64)},
		{ID: 3},
	}
	var got recordingRediscoverer
	rediscoverAdoptedLLDPPeers(devices, &got)
	if !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("rediscovered devices = %v, want [1 2]", got)
	}
}
