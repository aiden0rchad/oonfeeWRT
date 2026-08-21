package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func policyCaps() *capability.Registry {
	caps := routerCaps()
	caps.Set(capability.FeatFirewall4, capability.Present)
	return caps
}

func renderPolicySite() model.Site {
	return model.Site{UUID: "policy-site", Networks: []model.Network{
		{ID: 1, Name: "guest", Zone: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Enabled: true},
		{ID: 2, Name: "iot", Zone: "iot", VLAN: 30, CIDR: "10.0.30.1/24", Enabled: true},
	}}
}

func TestPolicyRenderProducesOwnedFirewallNATRouteLeaseAndDualStackBlock(t *testing.T) {
	site := renderPolicySite()
	site.Policies = []model.Policy{
		{ID: 1, Order: 100, Name: "deny telemetry", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
			Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan", Protocols: []string{"udp"}, DestinationPort: "123"}},
		{ID: 2, Order: 200, Name: "camera", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
			PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443, DestinationIP: "10.0.30.20", DestinationPort: 8443}},
		{ID: 3, Order: 300, Name: "lab", Kind: model.PolicyStaticRoute, Origin: model.PolicyOriginManual, Enabled: true,
			StaticRoute: &model.StaticRoute{Target: "203.0.113.0/24", Gateway: "192.0.2.1", Metric: 10}},
	}
	site.PolicyClients = []model.PolicyClient{{MAC: "00:11:22:33:44:55", Blocked: true, FixedIP: "10.0.20.50"}}
	doc, report, err := Render(site, model.Device{ID: 7, Role: model.RoleGateway}, policyCaps(), gwExisting())
	if err != nil || report.HasConflicts() {
		t.Fatalf("render err=%v conflicts=%+v omissions=%+v", err, report.Conflicts, report.Omissions)
	}
	seen := map[string]int{}
	for _, section := range doc.Sections {
		if section.Values[OwnershipTag] != "1" {
			t.Fatalf("rendered unowned section: %+v", section)
		}
		switch {
		case section.Config == "firewall" && section.Type == "rule" && strings.HasPrefix(section.Values["name"], "oonfeeWRT policy 1 "):
			seen["firewall"]++
			if section.Values["src"] != "guest" || section.Values["dest"] != "wan" || section.Values["target"] != "DROP" {
				t.Fatalf("firewall rule=%+v", section)
			}
		case section.Config == "firewall" && section.Type == "redirect":
			seen["redirect"]++
			if section.Values["src"] != "wan" || section.Values["dest_ip"] != "10.0.30.20" || section.Values["target"] != "DNAT" {
				t.Fatalf("redirect=%+v", section)
			}
		case section.Config == "network" && section.Type == "route":
			seen["route"]++
		case section.Config == "dhcp" && section.Type == "host":
			seen["host"]++
		case section.Config == "firewall" && section.Type == "rule" && strings.HasPrefix(section.Values["name"], "oonfeeWRT client-block "):
			seen["block"]++
			if section.Values["dest"] != "*" || section.Values["family"] != "" || !hasString(section.Manages, "family") {
				t.Fatalf("client block is not dual-stack forwarded-only: %+v", section)
			}
		}
	}
	for kind, want := range map[string]int{"firewall": 1, "redirect": 1, "route": 1, "host": 1, "block": 2} {
		if seen[kind] != want {
			t.Errorf("%s sections=%d, want %d", kind, seen[kind], want)
		}
	}
}

func TestClientBlockUpgradeDeletesStaleIPv4Family(t *testing.T) {
	site := renderPolicySite()
	site.PolicyClients = []model.PolicyClient{{MAC: "00:11:22:33:44:55", Blocked: true}}
	doc, report, err := Render(site, model.Device{ID: 7, Role: model.RoleGateway}, policyCaps(), gwExisting())
	if err != nil || report.HasConflicts() {
		t.Fatalf("render err=%v report=%+v", err, report)
	}
	var block Section
	for _, section := range doc.Sections {
		if strings.HasPrefix(section.Values["name"], "oonfeeWRT client-block ") && section.Values["src"] == "guest" {
			block = section
			break
		}
	}
	if block.Name == "" {
		t.Fatal("guest client block missing")
	}
	existing := gwExisting()
	existing.Configs["firewall"][block.Name] = map[string]string{
		".type": "rule", "name": block.Values["name"], "src": "guest", "dest": "*", "target": "REJECT",
		"family": "ipv4", OwnershipTag: "1",
	}
	plan := doc.Plan(existing)
	found := false
	for _, op := range plan.Ops {
		found = found || op.Config == "firewall" && op.Section == block.Name &&
			op.Kind == applyengine.OpDelete && op.Option == "family"
	}
	if !found {
		t.Fatalf("stale family=ipv4 was not cleared: %+v", plan.Ops)
	}
}

func TestPolicyRenderClearsRealRouteAndHostDisableOptions(t *testing.T) {
	site := renderPolicySite()
	site.Policies = []model.Policy{{ID: 3, Order: 100, Name: "lab", Kind: model.PolicyStaticRoute,
		Origin: model.PolicyOriginManual, Enabled: true,
		StaticRoute: &model.StaticRoute{Target: "203.0.113.0/24", Gateway: "192.0.2.1"}}}
	site.PolicyClients = []model.PolicyClient{{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"}}
	doc, report, err := Render(site, model.Device{ID: 7, Role: model.RoleGateway}, policyCaps(), gwExisting())
	if err != nil || report.HasConflicts() {
		t.Fatalf("render err=%v report=%+v", err, report)
	}
	existing := gwExisting()
	for _, section := range doc.Sections {
		if section.Type != "route" && section.Type != "host" {
			continue
		}
		if existing.Configs[section.Config] == nil {
			existing.Configs[section.Config] = map[string]map[string]string{}
		}
		values := existingSection(section)
		values["enabled"] = "0"
		if section.Type == "route" {
			values["disabled"] = "1"
			values["table"] = "123"
			values["source"] = "198.51.100.0/24"
		} else {
			values["enable"] = "0"
			values["match_tag"] = "never"
			values["instance"] = "other"
			values["networkid"] = "legacy"
			values["broadcast"] = "1"
		}
		existing.Configs[section.Config][section.Name] = values
	}
	cleared := map[string]bool{"disabled": false, "enable": false, "enabled": false, "table": false, "source": false,
		"match_tag": false, "instance": false, "networkid": false, "broadcast": false}
	for _, op := range doc.Plan(existing).Ops {
		if op.Kind == applyengine.OpDelete {
			if _, ok := cleared[op.Option]; ok {
				cleared[op.Option] = true
			}
		}
		if op.Values["table"] == "main" {
			cleared["table"] = true
		}
	}
	for option, found := range cleared {
		if !found {
			t.Errorf("stale %s was not cleared", option)
		}
	}
}

func TestForeignRouteAndHostUseTheirRealDisableKeys(t *testing.T) {
	route := model.Policy{Name: "lab", Kind: model.PolicyStaticRoute,
		StaticRoute: &model.StaticRoute{Target: "203.0.113.0/24", Gateway: "192.0.2.1"}}
	client := model.PolicyClient{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"}
	for name, test := range map[string]struct {
		config string
		values map[string]string
		active bool
	}{
		"route irrelevant enabled zero remains active": {"network", map[string]string{".type": "route", "target": "203.0.113.0/24", "enabled": "0"}, true},
		"route disabled one is inactive":               {"network", map[string]string{".type": "route", "target": "203.0.113.0/24", "disabled": "1"}, false},
		"host irrelevant enabled zero remains active":  {"dhcp", map[string]string{".type": "host", "mac": client.MAC, "ip": "10.0.20.99", "enabled": "0"}, true},
		"host enable zero is inactive":                 {"dhcp", map[string]string{".type": "host", "mac": client.MAC, "ip": "10.0.20.99", "enable": "0"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			existing := NewExisting(map[string]map[string]map[string]string{test.config: {"foreign": test.values}})
			var conflicts []Conflict
			if test.config == "network" {
				conflicts = foreignRouteConflicts(route, existing)
			} else {
				conflicts = foreignHostConflicts(client, existing)
			}
			if got := len(conflicts) > 0; got != test.active {
				t.Fatalf("conflict=%v, want active=%v: %+v", got, test.active, conflicts)
			}
		})
	}
}

func policySection(doc Doc, sectionType string) Section {
	for _, section := range doc.Sections {
		if section.Config == "firewall" && section.Type == sectionType && strings.HasPrefix(section.Values["name"], "oonfeeWRT policy ") {
			return section
		}
	}
	return Section{}
}

func existingSection(section Section) map[string]string {
	values := map[string]string{".type": section.Type}
	for key, value := range section.Values {
		values[key] = value
	}
	var listNames []string
	for key, value := range section.Lists {
		values[key] = strings.Join(value, " ")
		listNames = append(listNames, key)
	}
	if len(listNames) > 0 {
		values[ListsKey] = strings.Join(listNames, " ")
	}
	return values
}

func TestFirewallPolicyEditClearsEveryOmittedConditionalOption(t *testing.T) {
	oldSite := renderPolicySite()
	oldSite.Policies = []model.Policy{{ID: 1, Order: 100, Name: "test", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan",
			Protocols: []string{"tcp", "udp"}, SourceCIDR: "10.0.20.0/24", DestinationCIDR: "192.0.2.0/24",
			SourcePort: "1000-2000", DestinationPort: "443", SourceMACs: []string{"00:11:22:33:44:55", "00:11:22:33:44:66"}}}}
	oldDoc, _, err := Render(oldSite, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	old := policySection(oldDoc, "rule")
	newSite := renderPolicySite()
	newSite.Policies = []model.Policy{{ID: 1, Order: 100, Name: "test", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", Protocols: []string{"all"}}}}
	newDoc, _, err := Render(newSite, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	oldValues := existingSection(old)
	for key, value := range map[string]string{
		"limit": "1/minute", "ipset": "blocked src", "device": "eth0", "start_time": "09:00:00",
	} {
		oldValues[key] = value
	}
	existing := NewExisting(map[string]map[string]map[string]string{"firewall": {old.Name: oldValues}})
	plan := newDoc.Plan(existing)
	wantDeleted := map[string]bool{"dest": false, "src_ip": false, "dest_ip": false, "src_port": false, "dest_port": false,
		"src_mac": false, "proto": false, "limit": false, "ipset": false, "device": false, "start_time": false}
	for _, op := range plan.Ops {
		if op.Config != "firewall" || op.Section != old.Name {
			continue
		}
		if op.Kind == applyengine.OpDelete && op.Option != "" {
			if _, tracked := wantDeleted[op.Option]; tracked {
				wantDeleted[op.Option] = true
			}
		}
		if _, changesProto := op.Values["proto"]; changesProto {
			wantDeleted["proto"] = true
		}
	}
	for option, changed := range wantDeleted {
		if !changed {
			t.Errorf("policy edit left stale %s: %+v", option, plan.Ops)
		}
	}
}

func TestRedirectEditClearsSourceAndListProtocol(t *testing.T) {
	oldSite := renderPolicySite()
	oldSite.Policies = []model.Policy{{ID: 2, Order: 100, Name: "camera", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
		PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp", "udp"}, ExternalPort: 443,
			DestinationIP: "10.0.30.20", DestinationPort: 8443, SourceCIDR: "192.0.2.0/24"}}}
	oldDoc, _, _ := Render(oldSite, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	old := policySection(oldDoc, "redirect")
	newSite := renderPolicySite()
	newSite.Policies = []model.Policy{{ID: 2, Order: 100, Name: "camera", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
		PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443,
			DestinationIP: "10.0.30.20", DestinationPort: 8443}}}
	newDoc, _, _ := Render(newSite, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	oldValues := existingSection(old)
	for key, value := range map[string]string{
		"src_port": "1024", "src_dip": "192.0.2.10", "reflection": "0", "ipset": "blocked src", "limit": "1/minute",
	} {
		oldValues[key] = value
	}
	plan := newDoc.Plan(NewExisting(map[string]map[string]map[string]string{"firewall": {old.Name: oldValues}}))
	wantChanged := map[string]bool{"src_ip": false, "src_port": false, "src_dip": false, "reflection": false, "ipset": false, "limit": false, "proto": false}
	for _, op := range plan.Ops {
		if op.Section != old.Name {
			continue
		}
		if op.Kind == applyengine.OpDelete {
			if _, ok := wantChanged[op.Option]; ok {
				wantChanged[op.Option] = true
			}
		}
		if op.Values["proto"] == "tcp" {
			wantChanged["proto"] = true
		}
	}
	for option, changed := range wantChanged {
		if !changed {
			t.Errorf("redirect edit retained stale %s: %+v", option, plan.Ops)
		}
	}
}

func TestPolicyKindChangeUsesPruneAndAddNotInPlaceTypeMutation(t *testing.T) {
	oldSite := renderPolicySite()
	oldSite.Policies = []model.Policy{{ID: 9, Order: 100, Name: "change", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan"}}}
	oldDoc, _, _ := Render(oldSite, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	old := policySection(oldDoc, "rule")
	newSite := renderPolicySite()
	newSite.Policies = []model.Policy{{ID: 9, Order: 100, Name: "change", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
		PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443, DestinationIP: "10.0.30.20", DestinationPort: 443}}}
	newDoc, _, _ := Render(newSite, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	newSection := policySection(newDoc, "redirect")
	if old.Name == newSection.Name || !strings.Contains(old.Name, "_rule_") || !strings.Contains(newSection.Name, "_dnat_") {
		t.Fatalf("kind-specific section names old=%q new=%q", old.Name, newSection.Name)
	}
	existing := NewExisting(map[string]map[string]map[string]string{"firewall": {old.Name: existingSection(old)}})
	ops := append(newDoc.Plan(existing).Ops, newDoc.Prune(existing)...)
	added, removed := false, false
	for _, op := range ops {
		added = added || op.Section == newSection.Name && (op.Kind == applyengine.OpAdd || op.Kind == applyengine.OpSet)
		removed = removed || op.Section == old.Name && op.Kind == applyengine.OpDelete && op.Option == ""
	}
	if !added || !removed {
		t.Fatalf("kind change was not add+prune: %+v", ops)
	}
}

func TestPolicyRenderFailsClosedWithoutFirewall4Observation(t *testing.T) {
	site := renderPolicySite()
	site.Policies = []model.Policy{{ID: 1, Order: 100, Name: "deny", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan"}}}
	caps := policyCaps()
	caps.Set(capability.FeatFirewall4, capability.NotObservable)
	_, report, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway}, caps, gwExisting())
	if err != nil || !report.HasConflicts() || !strings.Contains(report.Conflicts[0].Reason, "requires observable firewall4") {
		t.Fatalf("unobservable firewall4 err=%v report=%+v", err, report)
	}
}

func TestPolicyForeignSemanticConflictsFailClosed(t *testing.T) {
	tests := map[string]struct {
		policy model.Policy
		client *model.PolicyClient
		config string
		name   string
		vals   map[string]string
	}{
		"opposing rule": {
			policy: model.Policy{ID: 1, Name: "deny", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
				Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan"}},
			config: "firewall", name: "foreign_allow", vals: map[string]string{".type": "rule", "src": "guest", "dest": "*", "target": "ACCEPT"}},
		"redirect port": {
			policy: model.Policy{ID: 2, Name: "camera", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
				PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443, DestinationIP: "10.0.30.20", DestinationPort: 443}},
			config: "firewall", name: "foreign_dnat", vals: map[string]string{".type": "redirect", "src": "wan", "src_dport": "443", "proto": "tcp", "target": "DNAT", "dest_ip": "10.0.30.99"}},
		"redirect unknown protocol syntax": {
			policy: model.Policy{ID: 2, Name: "camera", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
				PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443, DestinationIP: "10.0.30.20", DestinationPort: 443}},
			config: "firewall", name: "foreign_dnat", vals: map[string]string{".type": "redirect", "src": "wan", "src_dport": "443", "proto": "tcp,udp", "target": "DNAT", "dest_ip": "10.0.30.99"}},
		"foreign WAN denial defeats port forward": {
			policy: model.Policy{ID: 2, Name: "camera", Kind: model.PolicyPortForward, Origin: model.PolicyOriginManual, Enabled: true,
				PortForward: &model.PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443, DestinationIP: "10.0.30.20", DestinationPort: 8443}},
			config: "firewall", name: "foreign_drop", vals: map[string]string{".type": "rule", "src": "wan", "dest": "iot", "family": "ipv4", "proto": "tcp", "dest_ip": "10.0.30.20", "dest_port": "8443", "target": "DROP"}},
		"route overlap": {
			policy: model.Policy{ID: 3, Name: "lab", Kind: model.PolicyStaticRoute, Origin: model.PolicyOriginManual, Enabled: true,
				StaticRoute: &model.StaticRoute{Target: "203.0.113.0/24", Gateway: "192.0.2.1"}},
			config: "network", name: "foreign_route", vals: map[string]string{".type": "route", "target": "203.0.113.128", "netmask": "255.255.255.128"}},
		"fixed lease": {
			client: &model.PolicyClient{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"},
			config: "dhcp", name: "foreign_host", vals: map[string]string{".type": "host", "mac": "00:11:22:33:44:55", "ip": "10.0.20.99"}},
		"fixed lease wildcard MAC": {
			client: &model.PolicyClient{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"},
			config: "dhcp", name: "foreign_host", vals: map[string]string{".type": "host", "mac": "*", "ip": "10.0.20.99"}},
		"fixed lease unreadable MAC": {
			client: &model.PolicyClient{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"},
			config: "dhcp", name: "foreign_host", vals: map[string]string{".type": "host", "mac": "not-a-mac", "ip": "10.0.20.99"}},
		"fixed lease alternate identity": {
			client: &model.PolicyClient{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"},
			config: "dhcp", name: "foreign_host", vals: map[string]string{".type": "host", "duid": "000100012345", "ip": "10.0.20.99"}},
		"blocked client any destination": {
			client: &model.PolicyClient{MAC: "00:11:22:33:44:55", Blocked: true},
			config: "firewall", name: "foreign_allow_iot", vals: map[string]string{".type": "rule", "src": "guest", "dest": "iot", "target": "ACCEPT"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			site := renderPolicySite()
			if test.policy.Name != "" {
				site.Policies = []model.Policy{test.policy}
			}
			if test.client != nil {
				site.PolicyClients = []model.PolicyClient{*test.client}
			}
			existing := gwExisting()
			if existing.Configs[test.config] == nil {
				existing.Configs[test.config] = map[string]map[string]string{}
			}
			existing.Configs[test.config][test.name] = test.vals
			_, report, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), existing)
			if err != nil || !report.HasConflicts() {
				t.Fatalf("foreign conflict missed err=%v report=%+v", err, report)
			}
		})
	}
}

func TestWanFirewallReferenceNeverCreatesOrOwnsWanZone(t *testing.T) {
	site := renderPolicySite()
	site.Policies = []model.Policy{{ID: 1, Name: "inbound", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "wan", DestinationZone: "guest"}}}
	doc, report, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway}, policyCaps(), gwExisting())
	if err != nil || report.HasConflicts() {
		t.Fatalf("WAN reference err=%v report=%+v", err, report)
	}
	found := false
	for _, section := range doc.Sections {
		if section.Config == "firewall" && section.Type == "zone" && section.Values["name"] == "wan" {
			t.Fatalf("foreign WAN zone was adopted or duplicated: %+v", section)
		}
		if section.Config == "firewall" && section.Type == "rule" && section.Values["src"] == "wan" {
			found = true
		}
	}
	if !found {
		t.Fatal("WAN-referencing rule was not rendered")
	}
}

func TestBlankSourceOutputRulesDoNotConflictButWildcardIngressDoes(t *testing.T) {
	policy := model.Policy{Name: "deny", Kind: model.PolicyFirewallRule,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan"}}
	client := model.PolicyClient{MAC: "00:11:22:33:44:55", Blocked: true}
	for _, source := range []string{"", "*"} {
		existing := NewExisting(map[string]map[string]map[string]string{"firewall": {
			"foreign": {".type": "rule", "src": source, "dest": "wan", "target": "ACCEPT"},
		}})
		want := source == "*"
		for name, conflicts := range map[string][]Conflict{
			"policy": foreignRuleConflicts(policy, existing),
			"client": foreignClientBlockConflicts(client, "guest", existing),
			"matrix": foreignFirewallPolicyConflicts(existing, "guest", nil),
		} {
			if got := len(conflicts) > 0; got != want {
				t.Errorf("source=%q %s conflict=%v want=%v: %+v", source, name, got, want, conflicts)
			}
		}
		serviceExisting := NewExisting(map[string]map[string]map[string]string{"firewall": {
			"foreign": {".type": "rule", "src": source, "target": "DROP", "proto": "udp", "src_port": "68", "dest_port": "67"},
		}})
		if got := len(foreignRouterServiceConflicts(serviceExisting, "guest")) > 0; got != want {
			t.Errorf("source=%q router-service conflict=%v want=%v", source, got, want)
		}
	}
}
