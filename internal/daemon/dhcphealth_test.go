package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

type fakeDHCPRuntime struct {
	responses map[string]any
	errs      map[string]error
	calls     []string
}

func (f *fakeDHCPRuntime) Call(_ context.Context, object, method string, _ any, out any) error {
	key := object + "." + method
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	blob, err := json.Marshal(f.responses[key])
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, out)
}

func ownedDHCPPlan(op applyengine.Op) *reconcile.DevicePlan {
	return &reconcile.DevicePlan{
		Doc: render.Doc{Sections: []render.Section{
			{Config: "network", Type: "interface", Name: "oowrt_net_iot", Values: map[string]string{
				"ipaddr": "10.0.20.1", "netmask": "255.255.255.0", render.OwnershipTag: "1",
			}},
			{Config: "dhcp", Type: "dhcp", Name: "oowrt_dhcp_iot", Values: map[string]string{
				"interface": "oowrt_net_iot", "start": "20", "limit": "80",
				"leasetime": "12h", render.OwnershipTag: "1",
			}},
		}},
		Plan: applyengine.Plan{Ops: []applyengine.Op{op}},
	}
}

func healthyDHCPRuntime(rangeLine string) map[string]any {
	return map[string]any{
		"network.interface.dump": map[string]any{"interface": []any{map[string]any{
			"interface": "oowrt_net_iot", "up": true,
			"ipv4-address": []any{map[string]any{"address": "10.0.20.1", "mask": 24}},
		}}},
		"service.list": map[string]any{"dnsmasq": map[string]any{"instances": map[string]any{
			"cfg01411c": map[string]any{
				"running": true, "pid": 321,
				"command": []string{"/usr/sbin/dnsmasq", "-C", "/var/etc/dnsmasq.conf.cfg01411c", "-k"},
			},
		}}},
		"file.read": map[string]any{"data": rangeLine + "\n"},
	}
}

func TestDHCPRuntimePlanOnlyCoversOwnedPoolChanges(t *testing.T) {
	plan := ownedDHCPPlan(applyengine.Op{
		Kind: applyengine.OpSet, Config: "wireless", Section: "oowrt_wlan1_radio0",
	})
	if got := dhcpRuntimePlanFor(plan); got != nil {
		t.Fatalf("WLAN-only plan unexpectedly requires DHCP runtime access: %+v", got)
	}

	plan.Plan.Ops[0] = applyengine.Op{
		Kind: applyengine.OpSet, Config: "network", Section: "oowrt_net_iot",
	}
	got := dhcpRuntimePlanFor(plan)
	if got == nil || len(got.enabled) != 1 || len(got.absent) != 0 {
		t.Fatalf("owned network change did not require its DHCP proof: %+v", got)
	}
}

func TestDHCPRuntimePlanCoversTouchedWLANPoolDependency(t *testing.T) {
	plan := ownedDHCPPlan(applyengine.Op{
		Kind: applyengine.OpAdd, Config: "wireless", Section: "oowrt_wlan1_radio0",
	})
	plan.Doc.Sections = append(plan.Doc.Sections, render.Section{
		Config: "wireless", Type: "wifi-iface", Name: "oowrt_wlan1_radio0",
		Values: map[string]string{
			"network": "oowrt_net_iot", render.OwnershipTag: "1",
		},
	})
	got := dhcpRuntimePlanFor(plan)
	if got == nil || len(got.enabled) != 1 || got.enabled[0].iface != "oowrt_net_iot" {
		t.Fatalf("touched WLAN did not require its live DHCP pool: %+v", got)
	}
}

func TestOwnedInterfaceRuntimeCoversTouchedInterfaceAndWLANReference(t *testing.T) {
	plan := &reconcile.DevicePlan{
		Doc: render.Doc{Sections: []render.Section{
			{Config: "network", Type: "interface", Name: "oowrt_net_iot", Values: map[string]string{
				"proto": "none", render.OwnershipTag: "1",
			}},
			{Config: "wireless", Type: "wifi-iface", Name: "oowrt_wlan1_radio0", Values: map[string]string{
				"mode": "ap", "network": "oowrt_net_iot", render.OwnershipTag: "1",
			}},
		}},
		Plan: applyengine.Plan{Ops: []applyengine.Op{{
			Kind: applyengine.OpAdd, Config: "wireless", Section: "oowrt_wlan1_radio0",
		}}},
	}
	if got := ownedInterfacesForPlan(plan); len(got) != 1 || got[0] != "oowrt_net_iot" {
		t.Fatalf("WLAN network runtime requirement = %v", got)
	}

	// The interface itself is also a runtime claim even before a WLAN uses it.
	plan.Doc.Sections = plan.Doc.Sections[:1]
	plan.Plan.Ops[0] = applyengine.Op{
		Kind: applyengine.OpAdd, Config: "network", Section: "oowrt_net_iot",
	}
	if got := ownedInterfacesForPlan(plan); len(got) != 1 || got[0] != "oowrt_net_iot" {
		t.Fatalf("touched network runtime requirement = %v", got)
	}
}

func TestOwnedInterfaceRuntimeRejectsBroadcastingButIsolatedNetwork(t *testing.T) {
	required := []string{"oowrt_net_iot"}
	for _, tc := range []struct {
		name       string
		interfaces []any
		want       string
	}{
		{"up", []any{map[string]any{"interface": "oowrt_net_iot", "up": true}}, ""},
		{"down", []any{map[string]any{"interface": "oowrt_net_iot", "up": false}}, "down"},
		{"absent", []any{map[string]any{"interface": "lan", "up": true}}, "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDHCPRuntime{responses: map[string]any{
				"network.interface.dump": map[string]any{"interface": tc.interfaces},
			}, errs: map[string]error{}}
			err := checkOwnedInterfacesOnce(context.Background(), fake, required)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("runtime check = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEnabledDHCPRequiresInterfaceProcessAndExactRuntimeRange(t *testing.T) {
	plan := ownedDHCPPlan(applyengine.Op{
		Kind: applyengine.OpSet, Config: "dhcp", Section: "oowrt_dhcp_iot",
	})
	check := dhcpRuntimePlanFor(plan)
	if check == nil || len(check.enabled) != 1 {
		t.Fatalf("runtime plan = %+v", check)
	}
	want := "dhcp-range=set:oowrt_net_iot,10.0.20.20,10.0.20.99,255.255.255.0,12h"
	if check.enabled[0].rangeLine != want {
		t.Fatalf("runtime range = %q, want %q", check.enabled[0].rangeLine, want)
	}

	fake := &fakeDHCPRuntime{responses: healthyDHCPRuntime(want), errs: map[string]error{}}
	if err := checkDHCPRuntimeOnce(context.Background(), fake, check); err != nil {
		t.Fatal(err)
	}

	// A running dnsmasq is not proof that this pool loaded. Neither the desired
	// UCI section nor staged state is consulted as a fallback.
	fake = &fakeDHCPRuntime{responses: healthyDHCPRuntime(
		"dhcp-range=set:oowrt_net_iot,10.0.20.30,10.0.20.99,255.255.255.0,12h"),
		errs: map[string]error{}}
	err := checkDHCPRuntimeOnce(context.Background(), fake, check)
	if err == nil || !strings.Contains(err.Error(), "exact controller-owned range") {
		t.Fatalf("wrong live range passed: %v", err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "uci.") {
			t.Fatalf("runtime proof fell back to staged UCI state: %v", fake.calls)
		}
	}
}

func TestEnabledDHCPRequiresExactLiveInterfaceAddress(t *testing.T) {
	plan := ownedDHCPPlan(applyengine.Op{
		Kind: applyengine.OpSet, Config: "network", Section: "oowrt_net_iot",
	})
	check := dhcpRuntimePlanFor(plan)
	responses := healthyDHCPRuntime(check.enabled[0].rangeLine)
	responses["network.interface.dump"] = map[string]any{"interface": []any{map[string]any{
		"interface": "oowrt_net_iot", "up": true,
		"ipv4-address": []any{map[string]any{"address": "10.0.20.2", "mask": 24}},
	}}}
	err := checkDHCPRuntimeOnce(context.Background(), &fakeDHCPRuntime{
		responses: responses, errs: map[string]error{},
	}, check)
	if err == nil || !strings.Contains(err.Error(), "expected address 10.0.20.1/24") {
		t.Fatalf("wrong live interface address passed: %v", err)
	}
}

func TestDisabledDHCPProvesOnlyControllerPoolAbsent(t *testing.T) {
	plan := &reconcile.DevicePlan{
		Existing: render.NewExisting(map[string]map[string]map[string]string{
			"dhcp": {"oowrt_dhcp_iot": {
				".type": "dhcp", "interface": "oowrt_net_iot", render.OwnershipTag: "1",
			}},
		}),
		Plan: applyengine.Plan{Ops: []applyengine.Op{{
			Kind: applyengine.OpDelete, Config: "dhcp", Section: "oowrt_dhcp_iot",
		}}},
	}
	check := dhcpRuntimePlanFor(plan)
	if check == nil || len(check.enabled) != 0 || len(check.absent) != 1 {
		t.Fatalf("disabled runtime plan = %+v", check)
	}
	foreign := healthyDHCPRuntime("dhcp-range=set:foreign_lan,192.168.1.20,192.168.1.50,255.255.255.0,12h")
	delete(foreign, "network.interface.dump")
	if err := checkDHCPRuntimeOnce(context.Background(), &fakeDHCPRuntime{
		responses: foreign, errs: map[string]error{},
	}, check); err != nil {
		t.Fatalf("foreign DHCP was mistaken for our removed pool: %v", err)
	}

	lingering := healthyDHCPRuntime("dhcp-range=set:oowrt_net_iot,10.0.20.20,10.0.20.99,255.255.255.0,12h")
	delete(lingering, "network.interface.dump")
	err := checkDHCPRuntimeOnce(context.Background(), &fakeDHCPRuntime{
		responses: lingering, errs: map[string]error{},
	}, check)
	if err == nil || !strings.Contains(err.Error(), "cannot be proved inactive") {
		t.Fatalf("lingering controller range passed: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "no dhcp") {
		t.Fatalf("disabled diagnostic overclaims foreign DHCP absence: %v", err)
	}
}

func TestDHCPRuntimeACLGapFailsClosedWithReadoptAction(t *testing.T) {
	plan := ownedDHCPPlan(applyengine.Op{
		Kind: applyengine.OpSet, Config: "dhcp", Section: "oowrt_dhcp_iot",
	})
	check := dhcpRuntimePlanFor(plan)
	fake := &fakeDHCPRuntime{
		responses: healthyDHCPRuntime(check.enabled[0].rangeLine),
		errs: map[string]error{"service.list": &ubus.DeniedError{
			Object: "service", Method: "list",
		}},
	}
	err := checkDHCPRuntimeOnce(context.Background(), fake, check)
	if err == nil || !strings.Contains(err.Error(), "un-adopt this device") ||
		!strings.Contains(err.Error(), "removes controller-owned device configuration") ||
		!strings.Contains(err.Error(), "service path /var/etc/dnsmasq.conf.*") ||
		!strings.Contains(err.Error(), "canonical /tmp/etc/dnsmasq.conf.* target") {
		t.Fatalf("ACL gap is not safely actionable: %v", err)
	}
	if !dhcpObservationPermanent(err) {
		t.Fatal("known ACL gap would be retried through the rollback window")
	}
}

func TestDNSMasqRuntimePathIsNarrowlyBounded(t *testing.T) {
	for _, tc := range []struct {
		command []string
		ok      bool
	}{
		{[]string{"dnsmasq", "-C", "/var/etc/dnsmasq.conf.cfg01411c"}, true},
		{[]string{"dnsmasq", "-C/var/etc/dnsmasq.conf.cfg01411c"}, true},
		{[]string{"dnsmasq", "-C", "/etc/config/dhcp"}, false},
		{[]string{"dnsmasq", "-C", "/var/etc/dnsmasq.conf../etc/shadow"}, false},
		{[]string{"dnsmasq", "-C", "/var/etc/dnsmasq.conf.cfg/a"}, false},
	} {
		_, ok := dnsmasqConfigPath(tc.command)
		if ok != tc.ok {
			t.Errorf("dnsmasqConfigPath(%q) ok=%v, want %v", tc.command, ok, tc.ok)
		}
	}
}

func TestDHCPRuntimeHealthAgainstAppliedMockState(t *testing.T) {
	ctx := context.Background()
	c := ubus.New(ubus.Options{Host: startMock(t)})
	if err := c.Login(ctx, "root", "good"); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	network := render.Section{
		Config: "network", Type: "interface", Name: "oowrt_net_iot",
		Values: map[string]string{
			"proto": "static", "device": "br-lan.20", "ipaddr": "10.0.20.1",
			"netmask": "255.255.255.0", render.OwnershipTag: "1",
		},
	}
	dhcp := render.Section{
		Config: "dhcp", Type: "dhcp", Name: "oowrt_dhcp_iot",
		Values: map[string]string{
			"interface": "oowrt_net_iot", "start": "20", "limit": "80",
			"leasetime": "12h", render.OwnershipTag: "1",
		},
	}
	plan := &reconcile.DevicePlan{
		Doc: render.Doc{Sections: []render.Section{network, dhcp}},
		Plan: applyengine.Plan{
			AcknowledgeTraversal: true,
			Ops: []applyengine.Op{
				{Kind: applyengine.OpAdd, Config: network.Config, Type: network.Type,
					Name: network.Name, Section: network.Name, Values: network.Values},
				{Kind: applyengine.OpAdd, Config: dhcp.Config, Type: dhcp.Type,
					Name: dhcp.Name, Section: dhcp.Name, Values: dhcp.Values},
			},
		},
	}
	result, err := applyengine.New().Apply(ctx, c, plan.Plan, healthCheck(plan))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != applyengine.Applied {
		t.Fatalf("apply = %s", result.String())
	}
}

var _ dhcpRuntimeCaller = (*fakeDHCPRuntime)(nil)
