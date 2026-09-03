package render

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func ipv6Policy(mode model.IPv6Mode, length int) *model.IPv6Config {
	return &model.IPv6Config{Mode: mode, AssignmentLength: length}
}

func managementIPv6Site(mode model.IPv6Mode) model.Site {
	return model.Site{UUID: "ipv6-site", Networks: []model.Network{{
		ID: 1, Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Zone: "lan",
		IPv6: ipv6Policy(mode, 60), Enabled: true,
	}}}
}

func managementIPv6Existing() Existing {
	return NewExisting(map[string]map[string]map[string]string{
		"network": {
			"lan":  {".type": "interface", "proto": "static", "ipaddr": "192.168.1.1", "ip6assign": "56", "ip6hint": "a", "ip6class": "wan6"},
			"wan":  {".type": "interface", "proto": "dhcp", "ipv6": "1"},
			"wan6": {".type": "interface", "proto": "dhcpv6", "auto": "1"},
		},
		"dhcp": {
			"cfg_lan": {".type": "dhcp", "interface": "lan", "start": "100", "ra": "server", "dhcpv6": "server", "ndp": "relay", "ra_default": "1"},
		},
		"firewall": stockFirewall(),
	})
}

func renderManagementIPv6Plan(t *testing.T, mode model.IPv6Mode, existing Existing) (Doc, Report, applyengine.Plan) {
	t.Helper()
	doc, report, err := Render(managementIPv6Site(mode),
		model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return doc, report, doc.Plan(existing)
}

func TestIPv6PreserveMakesNoManagementOrWANClaim(t *testing.T) {
	existing := managementIPv6Existing()
	for _, ipv6 := range []*model.IPv6Config{nil, ipv6Policy(model.IPv6Preserve, 60)} {
		site := managementIPv6Site(model.IPv6Preserve)
		site.Networks[0].IPv6 = ipv6
		doc, report, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway},
			routerCaps(), existing)
		if err != nil {
			t.Fatal(err)
		}
		if report.HasConflicts() || len(doc.Patches) != 0 || len(doc.Plan(existing).Ops) != 0 {
			t.Fatalf("preserve changed foreign IPv6 state: patches=%+v conflicts=%+v ops=%+v",
				doc.Patches, report.Conflicts, doc.Plan(existing).Ops)
		}
	}
}

func TestManagementIPv6DisabledIsExactNonOwningAndIdempotent(t *testing.T) {
	existing := managementIPv6Existing()
	doc, report, plan := renderManagementIPv6Plan(t, model.IPv6Disabled, existing)
	if report.HasConflicts() {
		t.Fatalf("conflicts: %+v", report.Conflicts)
	}
	if len(doc.Sections) != 0 {
		t.Fatalf("management policy claimed sections: %+v", doc.Sections)
	}
	if got := doc.Configs(); !reflect.DeepEqual(got, []string{"dhcp", "network"}) {
		t.Fatalf("Configs = %v", got)
	}
	want := []string{
		"delete dhcp.cfg_lan.ra_default",
		"set dhcp.cfg_lan dhcpv6=disabled,ndp=disabled,ra=disabled",
		"delete network.lan.ip6assign",
		"delete network.lan.ip6class",
		"delete network.lan.ip6hint",
		"set network.wan ipv6=0",
		"set network.wan6 auto=0",
	}
	if got := describeOps(plan.Ops); !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled plan:\n got %v\nwant %v", got, want)
	}
	assertPatchPlanDoesNotOwnOrDeleteSections(t, plan)
	if pruned := doc.Prune(existing); len(pruned) != 0 {
		t.Fatalf("foreign management sections were pruned: %+v", pruned)
	}
	updated := applyOps(existing, plan.Ops)
	_, report, again := renderManagementIPv6Plan(t, model.IPv6Disabled, updated)
	if report.HasConflicts() || len(again.Ops) != 0 {
		t.Fatalf("disabled policy did not converge: conflicts=%+v ops=%+v", report.Conflicts, again.Ops)
	}
}

func TestManagementPrefixDelegationPatchesOnlyIPv6Options(t *testing.T) {
	existing := managementIPv6Existing()
	doc, report, plan := renderManagementIPv6Plan(t, model.IPv6PrefixDelegation, existing)
	if report.HasConflicts() {
		t.Fatalf("conflicts: %+v", report.Conflicts)
	}
	want := []string{
		"delete dhcp.cfg_lan.ra_default",
		"set dhcp.cfg_lan ndp=disabled",
		"set network.lan ip6assign=60",
	}
	if got := describeOps(plan.Ops); !reflect.DeepEqual(got, want) {
		t.Fatalf("prefix-delegation plan:\n got %v\nwant %v", got, want)
	}
	// Prefix delegation does not erase an operator's explicit class or hint.
	for _, op := range plan.Ops {
		if op.Option == "ip6class" || op.Option == "ip6hint" {
			t.Fatalf("prefix delegation erased %s: %+v", op.Option, op)
		}
	}
	assertPatchPlanDoesNotOwnOrDeleteSections(t, plan)
	updated := applyOps(existing, plan.Ops)
	_, report, again := renderManagementIPv6Plan(t, model.IPv6PrefixDelegation, updated)
	if report.HasConflicts() || len(again.Ops) != 0 {
		t.Fatalf("prefix delegation did not converge: conflicts=%+v ops=%+v", report.Conflicts, again.Ops)
	}
	if len(doc.Patches) != 3 {
		t.Fatalf("patch targets = %+v", doc.Patches)
	}
}

func TestManagementPrefixDelegationHandlesPPPParentWithoutDuplicateClient(t *testing.T) {
	tests := []struct {
		name       string
		withWAN6   bool
		current    string
		wantParent string
	}{
		{"explicit wan6", true, "auto", "1"},
		{"dynamic child", false, "1", "auto"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			existing := managementIPv6Existing()
			existing.Configs["network"]["wan"]["proto"] = "pppoe"
			existing.Configs["network"]["wan"]["ipv6"] = tc.current
			if !tc.withWAN6 {
				delete(existing.Configs["network"], "wan6")
			}
			_, report, plan := renderManagementIPv6Plan(t, model.IPv6PrefixDelegation, existing)
			if report.HasConflicts() {
				t.Fatalf("conflicts: %+v", report.Conflicts)
			}
			var found bool
			for _, op := range plan.Ops {
				if op.Kind == applyengine.OpSet && op.Section == "wan" && op.Values["ipv6"] == tc.wantParent {
					found = true
				}
			}
			if !found {
				t.Fatalf("PPP parent did not request ipv6=%s: %+v", tc.wantParent, plan.Ops)
			}
		})
	}
}

func TestManagementPrefixDelegationDoesNotGuessNonPPPWANParent(t *testing.T) {
	existing := managementIPv6Existing()
	existing.Configs["network"]["wan"]["proto"] = "dhcp"
	existing.Configs["network"]["wan"]["ipv6"] = "vendor-policy"
	_, report, plan := renderManagementIPv6Plan(t, model.IPv6PrefixDelegation, existing)
	if report.HasConflicts() {
		t.Fatalf("conflicts: %+v", report.Conflicts)
	}
	for _, op := range plan.Ops {
		if op.Section == "wan" {
			t.Fatalf("non-PPP WAN parent was guessed: %+v", op)
		}
	}
}

func TestManagementIPv6LeavesCustomWAN6ProtocolsUntouched(t *testing.T) {
	for _, mode := range []model.IPv6Mode{
		model.IPv6PrefixDelegation,
		model.IPv6Disabled,
	} {
		for _, proto := range []string{"static", "vendor-ipv6"} {
			t.Run(string(mode)+"/"+proto, func(t *testing.T) {
				existing := managementIPv6Existing()
				existing.Configs["network"]["wan6"]["proto"] = proto
				existing.Configs["network"]["wan6"]["auto"] = "operator-value"

				_, report, plan := renderManagementIPv6Plan(t, mode, existing)
				if report.HasConflicts() {
					t.Fatalf("custom wan6 was treated as invalid: %+v", report.Conflicts)
				}
				for _, op := range plan.Ops {
					if op.Config == "network" && op.Section == "wan6" {
						t.Fatalf("custom wan6 protocol %q was changed: %+v", proto, op)
					}
				}
			})
		}
	}
}

func TestManagementIPv6PatchesOnlyGateways(t *testing.T) {
	site := managementIPv6Site(model.IPv6Disabled)
	doc, report, err := Render(site, model.Device{ID: 2, Role: model.RoleAP},
		routerCaps(), managementIPv6Existing())
	if err != nil {
		t.Fatal(err)
	}
	if report.HasConflicts() || len(doc.Patches) != 0 || len(doc.Plan(managementIPv6Existing()).Ops) != 0 {
		t.Fatalf("AP received gateway IPv6 patches: patches=%+v conflicts=%+v", doc.Patches, report.Conflicts)
	}
}

func TestManagementIPv6RequiresExactExistingTargets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(Existing)
		want   string
	}{
		{"missing LAN", func(e Existing) { delete(e.Configs["network"], "lan") }, "network.lan"},
		{"wrong LAN type", func(e Existing) { e.Configs["network"]["lan"][".type"] = "device" }, "network.lan"},
		{"missing DHCP", func(e Existing) { delete(e.Configs["dhcp"], "cfg_lan") }, "exactly one existing DHCP"},
		{"ambiguous DHCP", func(e Existing) {
			e.Configs["dhcp"]["other_lan"] = map[string]string{".type": "dhcp", "interface": "lan"}
		}, "found 2"},
		{"wrong WAN type", func(e Existing) { e.Configs["network"]["wan"][".type"] = "device" }, "network.wan"},
		{"wrong wan6 type", func(e Existing) { e.Configs["network"]["wan6"][".type"] = "route" }, "network.wan6"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			existing := managementIPv6Existing()
			tc.mutate(existing)
			doc, report, err := Render(managementIPv6Site(model.IPv6Disabled),
				model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
			if err != nil {
				t.Fatal(err)
			}
			if !report.HasConflicts() || !strings.Contains(conflictText(report), tc.want) {
				t.Fatalf("conflicts = %+v, want text %q", report.Conflicts, tc.want)
			}
			for _, op := range doc.Plan(existing).Ops {
				if op.Kind == applyengine.OpAdd {
					t.Fatalf("missing foreign target caused a create: %+v", op)
				}
			}
		})
	}
}

func TestManagementIPv6DoesNotInventUnconventionalWANSections(t *testing.T) {
	existing := managementIPv6Existing()
	delete(existing.Configs["network"], "wan")
	delete(existing.Configs["network"], "wan6")
	doc, report, plan := renderManagementIPv6Plan(t, model.IPv6Disabled, existing)
	if report.HasConflicts() {
		t.Fatalf("unconventional WAN layout was treated as invalid: %+v", report.Conflicts)
	}
	for _, patch := range doc.Patches {
		if patch.Section == "wan" || patch.Section == "wan6" {
			t.Fatalf("absent WAN section was targeted: %+v", patch)
		}
	}
	for _, op := range plan.Ops {
		if op.Section == "wan" || op.Section == "wan6" || op.Kind == applyengine.OpAdd {
			t.Fatalf("absent WAN section was created or changed: %+v", op)
		}
	}
}

func TestManagementIPv6DisableRefusesToDeleteStaticIPv6(t *testing.T) {
	for _, option := range []string{"ip6addr", "ip6prefix", "ip6gw"} {
		t.Run(option, func(t *testing.T) {
			existing := managementIPv6Existing()
			existing.Configs["network"]["lan"][option] = "2001:db8::1/64"
			doc, report, err := Render(managementIPv6Site(model.IPv6Disabled),
				model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
			if err != nil {
				t.Fatal(err)
			}
			if !report.HasConflicts() || !strings.Contains(conflictText(report), option) ||
				!strings.Contains(conflictText(report), "Remove those values deliberately in OpenWrt") {
				t.Fatalf("static %s was not an actionable conflict: %+v", option, report.Conflicts)
			}
			for _, op := range doc.Plan(existing).Ops {
				if op.Config == "network" && op.Section == "lan" && op.Option == option {
					t.Fatalf("static IPv6 was silently deleted: %+v", op)
				}
			}
		})
	}
}

func TestManagementIPv6DisableRefusesStaticIPv6OnConventionalWAN(t *testing.T) {
	for _, section := range []string{"wan", "wan6"} {
		for _, option := range []string{"ip6addr", "ip6prefix", "ip6gw"} {
			t.Run(section+"/"+option, func(t *testing.T) {
				existing := managementIPv6Existing()
				existing.Configs["network"][section][option] = "2001:db8::1/64"
				doc, report, err := Render(managementIPv6Site(model.IPv6Disabled),
					model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
				if err != nil {
					t.Fatal(err)
				}
				if !report.HasConflicts() ||
					!strings.Contains(conflictText(report), "network."+section) ||
					!strings.Contains(conflictText(report), option) ||
					!strings.Contains(conflictText(report), "Remove those values deliberately in OpenWrt") {
					t.Fatalf("static %s.%s was not an actionable conflict: %+v",
						section, option, report.Conflicts)
				}
				for _, op := range doc.Plan(existing).Ops {
					if op.Config == "network" && op.Section == section && op.Option == option {
						t.Fatalf("static IPv6 was silently changed: %+v", op)
					}
				}
			})
		}
	}
}

func managedIPv6Site(mode model.IPv6Mode, dhcp4 bool) model.Site {
	dhcp := &model.DHCPConfig{Enabled: dhcp4, Start: 20, Limit: 80, LeaseTime: "30m"}
	return model.Site{UUID: "managed-ipv6", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.45.0.1/24", Zone: "iot",
		DHCP: dhcp, IPv6: ipv6Policy(mode, 60), Enabled: true,
	}}}
}

func TestManagedVLANIPv6ModesAndIPv4Independence(t *testing.T) {
	tests := []struct {
		name   string
		mode   model.IPv6Mode
		dhcp4  bool
		wantV6 bool
	}{
		{"DHCPv4 off IPv6 on", model.IPv6PrefixDelegation, false, true},
		{"DHCPv4 on IPv6 off", model.IPv6Disabled, true, false},
		{"both on", model.IPv6PrefixDelegation, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc, report, err := Render(managedIPv6Site(tc.mode, tc.dhcp4),
				model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), vlanAware())
			if err != nil {
				t.Fatal(err)
			}
			if report.HasConflicts() {
				t.Fatalf("conflicts: %+v", report.Conflicts)
			}
			network := sectionsIn(doc, "network")["oowrt_net_iot"]
			dhcp := sectionsIn(doc, "dhcp")["oowrt_dhcp_iot"]
			if network.Values[OwnershipTag] != "1" || dhcp.Values[OwnershipTag] != "1" {
				t.Fatalf("managed sections lost ownership: network=%v dhcp=%v", network.Values, dhcp.Values)
			}
			if tc.wantV6 {
				if network.Values["ip6assign"] != "60" || dhcp.Values["ra"] != "server" ||
					dhcp.Values["dhcpv6"] != "server" || dhcp.Values["ndp"] != "disabled" {
					t.Fatalf("delegated IPv6 config = network %v dhcp %v", network.Values, dhcp.Values)
				}
				for _, name := range []string{"oowrt_in_iot_dhcpv6", "oowrt_in_iot_dnsv6", "oowrt_in_iot_icmpv6"} {
					if _, ok := sectionsIn(doc, "firewall")[name]; !ok {
						t.Errorf("missing IPv6 input rule %s", name)
					}
				}
			} else {
				if _, ok := network.Values["ip6assign"]; ok || dhcp.Values["ra"] != "disabled" ||
					dhcp.Values["dhcpv6"] != "disabled" || dhcp.Values["ndp"] != "disabled" {
					t.Fatalf("disabled IPv6 config = network %v dhcp %v", network.Values, dhcp.Values)
				}
				for name := range sectionsIn(doc, "firewall") {
					if strings.HasSuffix(name, "dhcpv6") || strings.HasSuffix(name, "dnsv6") || strings.HasSuffix(name, "icmpv6") {
						t.Errorf("disabled IPv6 retained input rule %s", name)
					}
				}
			}
			if tc.dhcp4 {
				if dhcp.Values["dhcpv4"] != "server" || dhcp.Values["ignore"] != "" {
					t.Fatalf("IPv4 DHCP on = %v", dhcp.Values)
				}
			} else if dhcp.Values["dhcpv4"] != "disabled" || dhcp.Values["ignore"] != "1" {
				t.Fatalf("IPv4 DHCP off = %v", dhcp.Values)
			}
		})
	}
}

func TestManagedVLANIPv6DisableDeletesPriorAssignmentAndRules(t *testing.T) {
	existing := vlanAware()
	existing.Configs["network"]["oowrt_net_iot"] = map[string]string{
		".type": "interface", OwnershipTag: "1", "proto": "static", "device": "br-lan.45",
		"ipaddr": "10.45.0.1", "netmask": "255.255.255.0", "ip6assign": "60", "ip6hint": "a", "ip6class": "wan6",
	}
	existing.Configs["dhcp"] = map[string]map[string]string{"oowrt_dhcp_iot": {
		".type": "dhcp", OwnershipTag: "1", "interface": "oowrt_net_iot", "networkid": "oowrt_net_iot",
		"ignore": "1", "dhcpv4": "disabled", "ra": "server", "dhcpv6": "server", "ndp": "relay", "ra_default": "1",
	}}
	existing.Configs["firewall"] = map[string]map[string]string{}
	for _, name := range []string{"dhcpv6", "dnsv6", "icmpv6"} {
		existing.Configs["firewall"]["oowrt_in_iot_"+name] = map[string]string{".type": "rule", OwnershipTag: "1"}
	}
	doc, report, err := Render(managedIPv6Site(model.IPv6Disabled, false),
		model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if report.HasConflicts() {
		t.Fatalf("conflicts: %+v", report.Conflicts)
	}
	ops := append(doc.Plan(existing).Ops, doc.Prune(existing)...)
	got := strings.Join(describeOps(ops), "\n")
	for _, want := range []string{
		"delete network.oowrt_net_iot.ip6assign", "delete network.oowrt_net_iot.ip6class",
		"delete network.oowrt_net_iot.ip6hint", "delete dhcp.oowrt_dhcp_iot.ra_default",
		"delete firewall.oowrt_in_iot_dhcpv6", "delete firewall.oowrt_in_iot_dnsv6", "delete firewall.oowrt_in_iot_icmpv6",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestManagedVLANPreserveKeepsHistoricalRenderExactly(t *testing.T) {
	site := managedIPv6Site(model.IPv6Preserve, true)
	explicit, report, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), vlanAware())
	if err != nil || report.HasConflicts() {
		t.Fatalf("explicit preserve: err=%v conflicts=%+v", err, report.Conflicts)
	}
	site.Networks[0].IPv6 = nil
	legacy, report, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), vlanAware())
	if err != nil || report.HasConflicts() {
		t.Fatalf("legacy preserve: err=%v conflicts=%+v", err, report.Conflicts)
	}
	if !reflect.DeepEqual(explicit, legacy) {
		t.Fatalf("explicit preserve changed historical render:\nexplicit=%+v\nlegacy=%+v", explicit, legacy)
	}
	for _, section := range explicit.Sections {
		if section.Values["ip6assign"] != "" || section.Values["ra"] != "" ||
			section.Values["dhcpv6"] != "" || section.Values["ndp"] != "" {
			t.Fatalf("preserve wrote IPv6 values: %+v", section)
		}
	}
}

func TestManagedVLANPrefixToPreserveEmitsNoIPv6Change(t *testing.T) {
	for _, dhcp4 := range []bool{false, true} {
		t.Run(map[bool]string{false: "DHCPv4 off", true: "DHCPv4 on"}[dhcp4], func(t *testing.T) {
			existing := vlanAware()
			prefix, report, err := Render(managedIPv6Site(model.IPv6PrefixDelegation, dhcp4),
				model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), existing)
			if err != nil || report.HasConflicts() {
				t.Fatalf("prefix render: err=%v conflicts=%+v", err, report.Conflicts)
			}
			existing = materializeDoc(existing, prefix)

			preserve, report, err := Render(managedIPv6Site(model.IPv6Preserve, dhcp4),
				model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), existing)
			if err != nil || report.HasConflicts() {
				t.Fatalf("preserve render: err=%v conflicts=%+v", err, report.Conflicts)
			}
			ops := append(preserve.Plan(existing).Ops, preserve.Prune(existing)...)
			if len(ops) != 0 {
				t.Fatalf("prefix -> preserve changed router IPv6 state: %+v", ops)
			}
			dhcp := sectionsIn(preserve, "dhcp")["oowrt_dhcp_iot"]
			if !dhcp4 && dhcp.Name == "" {
				t.Fatal("preserve pruned the IPv6-bearing DHCP section when DHCPv4 was off")
			}
			for _, option := range []string{"dynamicdhcpv6", "dhcpv6", "ra", "ra_management", "ndp", "ra_default"} {
				if slices.Contains(dhcp.Manages, option) {
					t.Errorf("preserve still manages IPv6 option %s: %v", option, dhcp.Manages)
				}
			}
		})
	}
}

func TestManagedVLANDisabledToPreserveRetainsExplicitOffState(t *testing.T) {
	existing := vlanAware()
	disabled, report, err := Render(managedIPv6Site(model.IPv6Disabled, false),
		model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), existing)
	if err != nil || report.HasConflicts() {
		t.Fatalf("disabled render: err=%v conflicts=%+v", err, report.Conflicts)
	}
	existing = materializeDoc(existing, disabled)
	preserve, report, err := Render(managedIPv6Site(model.IPv6Preserve, false),
		model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), existing)
	if err != nil || report.HasConflicts() {
		t.Fatalf("preserve render: err=%v conflicts=%+v", err, report.Conflicts)
	}
	if ops := append(preserve.Plan(existing).Ops, preserve.Prune(existing)...); len(ops) != 0 {
		t.Fatalf("disabled -> preserve changed explicit off state: %+v", ops)
	}
	if _, present := sectionsIn(preserve, "dhcp")["oowrt_dhcp_iot"]; !present {
		t.Fatal("preserve pruned the DHCP section carrying explicit IPv6 off state")
	}
}

func TestManagedVLANICMPv6RuleIsEssentialAndRateLimited(t *testing.T) {
	doc, report, err := Render(managedIPv6Site(model.IPv6PrefixDelegation, false),
		model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), vlanAware())
	if err != nil || report.HasConflicts() {
		t.Fatalf("render: err=%v conflicts=%+v", err, report.Conflicts)
	}
	rule := sectionsIn(doc, "firewall")["oowrt_in_iot_icmpv6"]
	wantTypes := []string{
		"echo-request", "echo-reply", "destination-unreachable", "packet-too-big",
		"time-exceeded", "bad-header", "unknown-header-type", "router-solicitation",
		"neighbour-solicitation", "router-advertisement", "neighbour-advertisement",
	}
	if rule.Values["proto"] != "icmp" || rule.Values["family"] != "ipv6" ||
		rule.Values["limit"] != "1000/sec" || !reflect.DeepEqual(rule.Lists["icmp_type"], wantTypes) {
		t.Fatalf("ICMPv6 rule is broader or incomplete: %+v", rule)
	}
}

func TestManagedVLANPrefixDelegationIsNotRenderedOnAP(t *testing.T) {
	doc, report, err := Render(managedIPv6Site(model.IPv6PrefixDelegation, true),
		model.Device{ID: 2, Role: model.RoleAP}, netCaps(), vlanAware())
	if err != nil {
		t.Fatal(err)
	}
	if report.HasConflicts() {
		t.Fatalf("conflicts: %+v", report.Conflicts)
	}
	if len(sectionsIn(doc, "dhcp")) != 0 {
		t.Fatalf("AP rendered DHCP/RA service: %+v", sectionsIn(doc, "dhcp"))
	}
	if iface := sectionsIn(doc, "network")["oowrt_net_iot"]; iface.Values["ip6assign"] != "" {
		t.Fatalf("AP requested a delegated prefix: %+v", iface)
	}
}

func TestDelegatedIPv6RejectsContradictoryForeignFirewallRules(t *testing.T) {
	for _, tc := range []struct {
		name, family, proto, srcPort, destPort string
	}{
		{"DHCPv6", "ipv6", "udp", "546", "547"},
		{"DNS", "ipv6", "tcp", "", "53"},
		{"ICMPv6", "ipv6", "icmpv6", "", ""},
		{"family any", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := vlanAware()
			existing.Configs["firewall"] = map[string]map[string]string{"manual_reject": {
				".type": "rule", "src": "iot", "family": tc.family, "proto": tc.proto,
				"src_port": tc.srcPort, "dest_port": tc.destPort, "target": "REJECT",
			}}
			_, report, err := Render(managedIPv6Site(model.IPv6PrefixDelegation, false),
				model.Device{ID: 1, Role: model.RoleGateway}, netCaps(), existing)
			if err != nil {
				t.Fatal(err)
			}
			if !report.HasConflicts() || !strings.Contains(conflictText(report), "manual_reject") {
				t.Fatalf("foreign %s rejection was not blocked: %+v", tc.name, report.Conflicts)
			}
		})
	}
}

func TestPatchCannotCreateClaimDeleteSectionOrEmitEmptySet(t *testing.T) {
	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {"lan": {".type": "interface", OwnershipTag: "foreign-value"}},
	})
	doc := Doc{Patches: []Patch{{
		Config: "network", Section: "lan",
		Values:  map[string]string{OwnershipTag: "1", ".type": "route"},
		Deletes: []string{OwnershipTag, ".type", "not_present"},
	}, {
		Config: "network", Section: "absent", Values: map[string]string{"ipv6": "0"},
	}}}
	if ops := doc.Plan(existing).Ops; len(ops) != 0 {
		t.Fatalf("unsafe or empty patch ops = %+v", ops)
	}
}

func assertPatchPlanDoesNotOwnOrDeleteSections(t *testing.T, plan applyengine.Plan) {
	t.Helper()
	for _, op := range plan.Ops {
		if op.Kind == applyengine.OpAdd || op.Kind == applyengine.OpDelete && op.Option == "" {
			t.Fatalf("foreign section ownership/deletion operation: %+v", op)
		}
		if _, owns := op.Values[OwnershipTag]; owns {
			t.Fatalf("patch wrote ownership marker: %+v", op)
		}
		if op.Kind == applyengine.OpSet && !op.Patch {
			t.Fatalf("foreign section set would be ownership-stamped by the apply engine: %+v", op)
		}
	}
}

func conflictText(report Report) string {
	var parts []string
	for _, conflict := range report.Conflicts {
		parts = append(parts, conflict.Config+"."+conflict.Section+" "+conflict.Reason)
	}
	return strings.Join(parts, "\n")
}

func describeOps(ops []applyengine.Op) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		prefix := string(op.Kind) + " " + op.Config + "." + op.Section
		if op.Kind == applyengine.OpDelete {
			if op.Option != "" {
				prefix += "." + op.Option
			}
			out = append(out, prefix)
			continue
		}
		var values []string
		for key, value := range op.Values {
			if key == OwnershipTag || key == "proto" || key == "device" || key == "ipaddr" || key == "netmask" ||
				key == "interface" || key == "networkid" || key == "ignore" || key == "dhcpv4" {
				continue
			}
			values = append(values, key+"="+value)
		}
		sort.Strings(values)
		if len(values) > 0 {
			prefix += " " + strings.Join(values, ",")
		}
		out = append(out, prefix)
	}
	return out
}

func applyOps(existing Existing, ops []applyengine.Op) Existing {
	for _, op := range ops {
		sections := existing.Configs[op.Config]
		if sections == nil {
			sections = map[string]map[string]string{}
			existing.Configs[op.Config] = sections
		}
		switch op.Kind {
		case applyengine.OpAdd:
			values := map[string]string{".type": op.Type}
			for key, value := range op.Values {
				values[key] = value
			}
			sections[op.Section] = values
		case applyengine.OpSet:
			for key, value := range op.Values {
				sections[op.Section][key] = value
			}
		case applyengine.OpDelete:
			if op.Option == "" {
				delete(sections, op.Section)
			} else {
				delete(sections[op.Section], op.Option)
			}
		}
	}
	return existing
}

func materializeDoc(existing Existing, doc Doc) Existing {
	for _, section := range doc.Sections {
		sections := existing.Configs[section.Config]
		if sections == nil {
			sections = map[string]map[string]string{}
			existing.Configs[section.Config] = sections
		}
		values := map[string]string{".type": section.Type}
		for key, value := range section.Values {
			values[key] = value
		}
		var lists []string
		for key, value := range section.Lists {
			values[key] = strings.Join(value, " ")
			lists = append(lists, key)
		}
		sort.Strings(lists)
		values[ListsKey] = strings.Join(lists, " ")
		sections[section.Name] = values
	}
	return existing
}
