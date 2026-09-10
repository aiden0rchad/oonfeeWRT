package model

import (
	"strings"
	"testing"
)

func policySite() Site {
	return Site{Networks: []Network{
		{ID: 1, Name: "guest", Zone: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Enabled: true},
		{ID: 2, Name: "iot", Zone: "iot", VLAN: 30, CIDR: "10.0.30.1/24", Enabled: true},
	}}
}

func TestPolicyValidationCanonicalizesRenderableRules(t *testing.T) {
	site := policySite()
	site.Policies = []Policy{
		{Name: "deny telemetry", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			Firewall: &FirewallRule{Action: FirewallReject, SourceZone: "guest", DestinationZone: "wan",
				Protocols: []string{" UDP ", "all", "tcp"}, SourceCIDR: "10.0.20.77/24",
				SourceMACs: []string{"AA-BB-CC-DD-EE-FF", "aa:bb:cc:dd:ee:ff"}}},
		{Name: "https", Kind: PolicyPortForward, Origin: PolicyOriginManual, Enabled: true,
			PortForward: &PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"udp", "TCP"},
				ExternalPort: 443, DestinationIP: "10.0.30.22", DestinationPort: 8443, SourceCIDR: "192.0.2.9/24"}},
		{Name: "lab", Kind: PolicyStaticRoute, Origin: PolicyOriginManual, Enabled: true,
			StaticRoute: &StaticRoute{Target: "203.0.113.0/24", Gateway: "192.0.2.1"}},
	}
	site.PolicyClients = []PolicyClient{{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"}}
	if errs := site.ValidatePolicies(); len(errs) != 0 {
		t.Fatalf("valid policy set rejected: %v", errs)
	}
	if got := strings.Join(site.Policies[0].Firewall.Protocols, ","); got != "all" {
		t.Fatalf("protocols=%q, want collapsed all", got)
	}
	if got := site.Policies[0].Firewall.SourceCIDR; got != "10.0.20.0/24" {
		t.Fatalf("source CIDR=%q", got)
	}
	if got := strings.Join(site.Policies[0].Firewall.SourceMACs, ","); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("source MACs=%q", got)
	}
	if got := site.Policies[1].PortForward.SourceCIDR; got != "192.0.2.0/24" {
		t.Fatalf("port-forward source CIDR=%q", got)
	}
}

func TestPolicyValidationRejectsUnsafeOrAmbiguousRules(t *testing.T) {
	tests := map[string]Policy{
		"wrong payload": {Name: "bad", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			StaticRoute: &StaticRoute{Target: "203.0.113.0/24", Gateway: "192.0.2.1"}},
		"unknown zone": {Name: "bad", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			Firewall: &FirewallRule{Action: FirewallDrop, SourceZone: "missing", DestinationZone: "wan"}},
		"all with port": {Name: "bad", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			Firewall: &FirewallRule{Action: FirewallDrop, SourceZone: "guest", DestinationZone: "wan", Protocols: []string{"all"}, DestinationPort: "53"}},
		"forward to router": {Name: "bad", Kind: PolicyPortForward, Origin: PolicyOriginManual, Enabled: true,
			PortForward: &PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 80, DestinationIP: "10.0.30.1", DestinationPort: 80}},
		"connected route": {Name: "bad", Kind: PolicyStaticRoute, Origin: PolicyOriginManual, Enabled: true,
			StaticRoute: &StaticRoute{Target: "10.0.30.0/24", Gateway: "192.0.2.1"}},
	}
	for name, policy := range tests {
		t.Run(name, func(t *testing.T) {
			site := policySite()
			site.Policies = []Policy{policy}
			if errs := site.ValidatePolicies(); len(errs) == 0 {
				t.Fatalf("unsafe policy accepted: %+v", policy)
			}
		})
	}
}

func TestPolicyValidationRejectsOrderDependentManagedRules(t *testing.T) {
	site := policySite()
	site.Policies = []Policy{
		{Name: "allow DNS", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			Firewall: &FirewallRule{Action: FirewallAccept, SourceZone: "guest", DestinationZone: "wan", Protocols: []string{"udp"}, DestinationPort: "53"}},
		{Name: "deny web", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			Firewall: &FirewallRule{Action: FirewallDrop, SourceZone: "guest", DestinationZone: "wan", Protocols: []string{"tcp"}, DestinationPort: "443"}},
	}
	if errs := site.ValidatePolicies(); len(errs) == 0 || !strings.Contains(errs[len(errs)-1].Error(), "order") {
		t.Fatalf("opposing same-scope rules did not fail closed: %v", errs)
	}

	site = policySite()
	site.Policies = []Policy{
		{Name: "one", Kind: PolicyPortForward, Origin: PolicyOriginManual, Enabled: true,
			PortForward: &PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443, DestinationIP: "10.0.30.20", DestinationPort: 443}},
		{Name: "two", Kind: PolicyPortForward, Origin: PolicyOriginManual, Enabled: true,
			PortForward: &PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp", "udp"}, ExternalPort: 443, DestinationIP: "10.0.30.21", DestinationPort: 443}},
	}
	if errs := site.ValidatePolicies(); len(errs) == 0 || !strings.Contains(errs[len(errs)-1].Error(), "both claim") {
		t.Fatalf("overlapping WAN claim accepted: %v", errs)
	}

	site = policySite()
	site.Policies = []Policy{
		{Name: "deny inbound", Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true,
			Firewall: &FirewallRule{Action: FirewallDrop, SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}}},
		{Name: "camera", Kind: PolicyPortForward, Origin: PolicyOriginManual, Enabled: true,
			PortForward: &PortForward{SourceZone: "wan", DestinationZone: "iot", Protocols: []string{"tcp"}, ExternalPort: 443,
				DestinationIP: "10.0.30.20", DestinationPort: 8443}},
	}
	if errs := site.ValidatePolicies(); len(errs) == 0 || !strings.Contains(errs[len(errs)-1].Error(), "after DNAT") {
		t.Fatalf("managed denial that defeats port forward accepted: %v", errs)
	}
}

func TestBlockedClientRejectsManagedAcceptButNotWanSourceOrRouterInput(t *testing.T) {
	for name, rule := range map[string]FirewallRule{
		"forwarded managed source": {Action: FirewallAccept, SourceZone: "guest", DestinationZone: "wan"},
		"router input":             {Action: FirewallAccept, SourceZone: "guest"},
		"wan source":               {Action: FirewallAccept, SourceZone: "wan", DestinationZone: "guest"},
	} {
		t.Run(name, func(t *testing.T) {
			site := policySite()
			site.PolicyClients = []PolicyClient{{MAC: "00:11:22:33:44:55", Blocked: true}}
			site.Policies = []Policy{{Name: name, Kind: PolicyFirewallRule, Origin: PolicyOriginManual, Enabled: true, Firewall: &rule}}
			errs := site.ValidatePolicies()
			if name == "forwarded managed source" && len(errs) == 0 {
				t.Fatal("managed accept defeated client block")
			}
			if name != "forwarded managed source" && len(errs) != 0 {
				t.Fatalf("non-overlapping scope rejected: %v", errs)
			}
		})
	}
}

func TestFixedIPMustBeUniqueUsableAndServed(t *testing.T) {
	site := policySite()
	site.PolicyClients = []PolicyClient{
		{MAC: "00:11:22:33:44:55", FixedIP: "10.0.20.50"},
		{MAC: "00:11:22:33:44:66", FixedIP: "10.0.20.50"},
	}
	if errs := site.ValidatePolicies(); len(errs) == 0 || !strings.Contains(errs[len(errs)-1].Error(), "assigned to both") {
		t.Fatalf("duplicate fixed IP accepted: %v", errs)
	}
}

func TestClientGroupIsBoundedPlainText(t *testing.T) {
	for name, group := range map[string]string{
		"leading whitespace": " cameras",
		"blank":              "   ",
		"control":            "camera\nadmin",
		"too long":           strings.Repeat("x", 129),
	} {
		t.Run(name, func(t *testing.T) {
			site := policySite()
			site.PolicyClients = []PolicyClient{{MAC: "00:11:22:33:44:55", Group: group}}
			if errs := site.ValidatePolicies(); len(errs) == 0 {
				t.Fatalf("invalid client group %q accepted", group)
			}
		})
	}
}

func TestPolicySetIsCanonicalAndReusableByFirewallRules(t *testing.T) {
	site := policySite()
	site.PolicySets = []PolicySet{{ID: 7, Name: "Cameras", Members: []string{
		"AA-BB-CC-DD-EE-FF", "00:11:22:33:44:55", "aa:bb:cc:dd:ee:ff",
	}}}
	site.Policies = []Policy{{Name: "secure cameras", Kind: PolicyFirewallRule,
		Origin: PolicyOriginObjectManager, Enabled: true,
		Firewall: &FirewallRule{Action: FirewallReject, SourceZone: "guest",
			DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: 7}}}
	if errs := site.ValidatePolicies(); len(errs) != 0 {
		t.Fatalf("valid reusable set rejected: %v", errs)
	}
	want := "00:11:22:33:44:55,aa:bb:cc:dd:ee:ff"
	if got := strings.Join(site.PolicySets[0].Members, ","); got != want {
		t.Fatalf("canonical members=%q want=%q", got, want)
	}
	if got, ok := site.FirewallSourceMACs(site.Policies[0].Firewall); !ok || strings.Join(got, ",") != want {
		t.Fatalf("effective members=%v ok=%v", got, ok)
	}
}

func TestPolicySetValidationFailsClosed(t *testing.T) {
	tests := map[string]Site{
		"empty set": {
			PolicySets: []PolicySet{{ID: 1, Name: "empty"}},
		},
		"duplicate name": {
			PolicySets: []PolicySet{
				{ID: 1, Name: "Cameras", Members: []string{"00:11:22:33:44:55"}},
				{ID: 2, Name: "cameras", Members: []string{"00:11:22:33:44:66"}},
			},
		},
		"unknown reference": {
			Policies: []Policy{{Name: "bad", Kind: PolicyFirewallRule, Origin: PolicyOriginManual,
				Enabled: true, Firewall: &FirewallRule{Action: FirewallDrop, SourceZone: "wan",
					Protocols: []string{"all"}, SourceSetID: 99}}},
		},
		"mixed source forms": {
			PolicySets: []PolicySet{{ID: 1, Name: "Cameras", Members: []string{"00:11:22:33:44:55"}}},
			Policies: []Policy{{Name: "bad", Kind: PolicyFirewallRule, Origin: PolicyOriginManual,
				Enabled: true, Firewall: &FirewallRule{Action: FirewallDrop, SourceZone: "wan",
					Protocols: []string{"all"}, SourceSetID: 1, SourceMACs: []string{"00:11:22:33:44:66"}}}},
		},
	}
	for name, site := range tests {
		t.Run(name, func(t *testing.T) {
			if errs := site.ValidatePolicies(); len(errs) == 0 {
				t.Fatal("unsafe reusable set state accepted")
			}
		})
	}
}

func TestPolicySetBackedAcceptCannotDefeatClientBlock(t *testing.T) {
	site := policySite()
	site.PolicySets = []PolicySet{{ID: 1, Name: "Allowed", Members: []string{"00:11:22:33:44:55"}}}
	site.PolicyClients = []PolicyClient{{MAC: "00:11:22:33:44:55", Blocked: true}}
	site.Policies = []Policy{{Name: "allow set", Kind: PolicyFirewallRule,
		Origin: PolicyOriginManual, Enabled: true,
		Firewall: &FirewallRule{Action: FirewallAccept, SourceZone: "guest",
			DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: 1}}}
	if errs := site.ValidatePolicies(); len(errs) == 0 || !strings.Contains(errs[len(errs)-1].Error(), "blocked client") {
		t.Fatalf("set-backed allow defeated client block: %v", errs)
	}
}
