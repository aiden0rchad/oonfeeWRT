package render

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func routerCaps() *capability.Registry {
	r := capability.NewRegistry()
	r.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2"}, WAN: "wan"}
	return r
}

// The device's own firewall, as stock OpenWrt ships it: zones named lan and
// wan, in anonymous sections, none of them ours.
func stockFirewall() map[string]map[string]string {
	return map[string]map[string]string{
		"cfg02dc81": {".type": "zone", "name": "lan", "input": "ACCEPT"},
		"cfg03dc81": {".type": "zone", "name": "wan", "input": "REJECT"},
	}
}

func gwExisting() Existing {
	return NewExisting(map[string]map[string]map[string]string{
		"network":  {"their_bv1": {".type": "bridge-vlan", "device": "br-lan", "vlan": "1"}},
		"firewall": stockFirewall(),
	})
}

func renderGateway(t *testing.T, nets []model.Network) (Doc, Report) {
	t.Helper()
	doc, rep, err := Render(model.Site{UUID: "site-uuid", Networks: nets},
		model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	return doc, rep
}

// Two networks the DB keeps distinct — name is UNIQUE in the schema — must not
// become one on the device.
//
// safe() capped every name at 11 characters, so these two rendered the same
// interface, the same DHCP server and the same firewall zone. UCI keeps the
// last, so VLAN 20 got its bridge-VLAN and nothing else: no gateway address,
// no leases, no zone. The preview reported no omission and no conflict.
func TestDistinctNetworkNamesDoNotShareSections(t *testing.T) {
	doc, _ := renderGateway(t, []model.Network{
		{ID: 1, Name: "Guest Network A", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guesta", Enabled: true},
		{ID: 2, Name: "Guest Network B", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "guestb", Enabled: true},
	})
	seen := map[string]int{}
	for _, s := range doc.Sections {
		seen[s.Config+"."+s.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%s rendered %d times: one network's config silently "+
				"overwrites the other's", name, n)
		}
	}
	// And both really are addressed, which is the thing the collision cost.
	var addressed int
	for _, s := range doc.Sections {
		if s.Config == "network" && s.Type == "interface" && s.Values["ipaddr"] != "" {
			addressed++
		}
	}
	if addressed != 2 {
		t.Errorf("%d networks got an address, want 2", addressed)
	}
}

// A zone is identified by its NAME, so two networks in one zone are one
// section holding both — not two sections with the same name.
func TestTwoNetworksInOneZoneRenderOneZone(t *testing.T) {
	doc, rep := renderGateway(t, []model.Network{
		{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "trusted", Enabled: true},
		{ID: 2, Name: "cams", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "trusted", Enabled: true},
	})
	if rep.HasConflicts() {
		t.Fatalf("sharing a zone is legitimate, got conflicts: %v", rep.Conflicts)
	}
	var zones []Section
	for _, s := range doc.Sections {
		if s.Type == "zone" {
			zones = append(zones, s)
		}
	}
	if len(zones) != 1 {
		t.Fatalf("rendered %d zone sections for one zone", len(zones))
	}
	got := zones[0].Lists["network"]
	if len(got) != 2 {
		t.Fatalf("zone holds %v; a network left out of its zone is a network "+
			"fw4 drops every packet for", got)
	}
	// A UCI list, not an option with a space in it — see Section.Lists.
	if zones[0].Values["network"] != "" {
		t.Error("the zone's networks were written as an option, not a list")
	}
}

func TestSharedZoneRendersOneServiceRulePairWhileAnyPoolIsEnabled(t *testing.T) {
	on := model.DHCPConfig{Enabled: true, Start: 20, Limit: 40, LeaseTime: "1h"}
	off := model.DHCPConfig{Enabled: false, Start: 20, Limit: 40, LeaseTime: "1h"}
	doc, rep := renderGateway(t, []model.Network{
		{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "trusted", DHCP: &on, Enabled: true},
		{ID: 2, Name: "cams", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "trusted", DHCP: &off, Enabled: true},
	})
	if rep.HasConflicts() {
		t.Fatalf("shared zone conflicted: %+v", rep.Conflicts)
	}
	seen := map[string]int{}
	for _, section := range doc.Sections {
		if section.Config == "firewall" {
			seen[section.Name]++
		}
	}
	for _, name := range []string{"oowrt_in_trusted_dhcp", "oowrt_in_trusted_dns"} {
		if seen[name] != 1 {
			t.Errorf("shared zone rendered %d copies of %s, want one", seen[name], name)
		}
	}
}

// A zone name the device already uses is the ownership rule applied to the
// namespace fw4 actually keys on. Our section name would not collide; the ZONE
// would, and the device would end up with two called lan.
//
// This was the default path: store.SaveNetwork stamped every network with zone
// "lan" and nothing in the UI ever set it.
func TestZoneNameTheDeviceAlreadyOwnsIsAConflict(t *testing.T) {
	_, _, err := Render(model.Site{UUID: "site-uuid", Networks: []model.Network{
		{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "lan", Enabled: true},
	}}, model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), gwExisting())
	if err == nil {
		t.Fatal("a second firewall zone named lan was rendered beside the " +
			"device's own, with input REJECT and forward REJECT, and nothing " +
			"said so")
	}
	if !strings.Contains(err.Error(), "lan") {
		t.Errorf("the refusal does not name the zone: %q", err)
	}
}

// Two zone names that fw4 cannot tell apart are one zone, whatever we call the
// sections. Merging two networks the operator separated is a firewall policy
// nobody chose.
func TestZoneNamesThatCollapsePastTheCapAreRefused(t *testing.T) {
	_, _, err := Render(model.Site{UUID: "site-uuid", Networks: []model.Network{
		{ID: 1, Name: "a", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guest_network_a", Enabled: true},
		{ID: 2, Name: "b", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "guest_network_b", Enabled: true},
	}}, model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), gwExisting())
	if err == nil {
		t.Fatal("two zone names identical to fw4 were merged silently")
	}
	r := err.Error()
	if !strings.Contains(r, "guest_network_a") || !strings.Contains(r, "guest_network_b") {
		t.Errorf("the conflict does not name both zones: %q", r)
	}
}

// And the ordinary case still works, or the checks above are just a way of
// refusing everything.
func TestOneNetworkRendersItsWholeStack(t *testing.T) {
	doc, rep := renderGateway(t, []model.Network{
		{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "iot", Enabled: true},
	})
	if rep.HasConflicts() {
		t.Fatalf("unexpected conflicts: %v", rep.Conflicts)
	}
	want := map[string]bool{
		"network.oowrt_bv20":         false,
		"network.oowrt_net_iot":      false,
		"dhcp.oowrt_dhcp_iot":        false,
		"firewall.oowrt_zone_iot":    false,
		"firewall.oowrt_fwd_iot_wan": false,
	}
	for _, s := range doc.Sections {
		k := s.Config + "." + s.Name
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, got := range want {
		if !got {
			t.Errorf("%s was not rendered", k)
		}
	}
}

func TestAbsentZonePolicyIsByteForByteLegacyWanForwarding(t *testing.T) {
	network := model.Network{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "iot", Enabled: true}
	legacy := model.Site{UUID: "site-uuid", Networks: []model.Network{network}}
	explicit := legacy
	explicit.Zones = []model.ZonePolicy{{Name: "iot", ForwardTo: []string{"wan"}, Explicit: true}}
	dev := model.Device{ID: 1, Role: model.RoleGateway}

	legacyDoc, legacyReport, err := Render(legacy, dev, routerCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	explicitDoc, explicitReport, err := Render(explicit, dev, routerCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacyDoc, explicitDoc) || !reflect.DeepEqual(legacyReport, explicitReport) {
		t.Fatalf("missing policy changed the legacy render:\nlegacy=%+v %+v\nexplicit=%+v %+v",
			legacyDoc, legacyReport, explicitDoc, explicitReport)
	}
	var forwarding *Section
	for i := range legacyDoc.Sections {
		if legacyDoc.Sections[i].Name == "oowrt_fwd_iot_wan" {
			forwarding = &legacyDoc.Sections[i]
		}
	}
	if forwarding == nil || forwarding.Config != "firewall" || forwarding.Type != "forwarding" ||
		!reflect.DeepEqual(forwarding.Values, map[string]string{
			"src": "iot", "dest": "wan", OwnershipTag: "1",
		}) {
		t.Fatalf("implicit policy changed the legacy UCI forwarding artifact: %+v", forwarding)
	}
}

func TestExplicitEmptyZonePolicyBlocksWan(t *testing.T) {
	site := model.Site{
		UUID: "site-uuid",
		Networks: []model.Network{{ID: 1, Name: "guest", VLAN: 20,
			CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}},
		Zones: []model.ZonePolicy{{Name: "guest", ForwardTo: []string{}, Explicit: true}},
	}
	existing := gwExisting()
	existing.Configs["firewall"]["oowrt_fwd_guest_wan"] = map[string]string{
		".type": "forwarding", "src": "guest", "dest": "wan", OwnershipTag: "1",
	}
	doc, rep, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway},
		routerCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasConflicts() {
		t.Fatalf("block-all policy conflicted without foreign forwarding: %v", rep.Conflicts)
	}
	var zones, forwards int
	for _, sec := range doc.Sections {
		if sec.Config != "firewall" {
			continue
		}
		if sec.Type == "zone" {
			zones++
		}
		if sec.Type == "forwarding" {
			forwards++
		}
	}
	if zones != 1 || forwards != 0 {
		t.Fatalf("firewall render has %d zones/%d forwardings, want 1/0", zones, forwards)
	}
	ops := doc.Prune(existing)
	if len(ops) != 1 || ops[0].Config != "firewall" || ops[0].Section != "oowrt_fwd_guest_wan" {
		t.Fatalf("block policy did not prune the old owned wan edge: %+v", ops)
	}
}

func TestInterZoneForwardingIsOneWayAndStatefulReturnNeedsNoReverseEdge(t *testing.T) {
	site := model.Site{
		UUID: "site-uuid",
		Networks: []model.Network{
			{ID: 1, Name: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true},
			{ID: 2, Name: "iot", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "iot", Enabled: true},
		},
		Zones: []model.ZonePolicy{
			{Name: "guest", ForwardTo: []string{"iot"}, Explicit: true},
			{Name: "iot", ForwardTo: []string{}, Explicit: true},
		},
	}
	doc, rep, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway},
		routerCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasConflicts() {
		t.Fatalf("one-way policy conflicts: %v", rep.Conflicts)
	}
	var got []string
	for _, sec := range doc.Sections {
		if sec.Config == "firewall" && sec.Type == "forwarding" {
			got = append(got, sec.Values["src"]+"->"+sec.Values["dest"])
		}
	}
	if !reflect.DeepEqual(got, []string{"guest->iot"}) {
		t.Fatalf("directed edges = %v, want guest->iot only; fw4 conntrack admits replies without iot->guest", got)
	}
}

func TestForeignForwardingMakesExplicitBlockAConflict(t *testing.T) {
	existing := gwExisting()
	existing.Configs["firewall"]["z_human_guest_wan"] = map[string]string{
		".type": "forwarding", "src": "guest", "dest": "wan",
	}
	existing.Configs["firewall"]["a_human_guest_lan"] = map[string]string{
		".type": "forwarding", "src": "guest", "dest": "lan",
	}
	site := model.Site{
		UUID: "site-uuid",
		Networks: []model.Network{{ID: 1, Name: "guest", VLAN: 20,
			CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}},
		Zones: []model.ZonePolicy{{Name: "guest", ForwardTo: []string{}, Explicit: true}},
	}
	doc, rep, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway},
		routerCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicts) != 2 || rep.Conflicts[0].Section != "a_human_guest_lan" ||
		rep.Conflicts[1].Section != "z_human_guest_wan" ||
		!strings.Contains(rep.Conflicts[0].Reason, "cannot be guaranteed") {
		t.Fatalf("foreign allow defeated a block without conflict: %+v", rep.Conflicts)
	}
	for _, sec := range doc.Sections {
		if sec.Name == "a_human_guest_lan" || sec.Name == "z_human_guest_wan" {
			t.Fatal("renderer adopted or edited a foreign forwarding")
		}
	}
}

func TestForeignDesiredForwardingDuplicateDoesNotDefeatAllow(t *testing.T) {
	existing := gwExisting()
	existing.Configs["firewall"]["human_guest_wan"] = map[string]string{
		".type": "forwarding", "src": "guest", "dest": "wan",
	}
	_, rep, err := Render(model.Site{
		UUID: "site-uuid",
		Networks: []model.Network{{ID: 1, Name: "guest", VLAN: 20,
			CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}},
	}, model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasConflicts() {
		t.Fatalf("foreign duplicate of an allowed edge made the allow unverifiable: %v", rep.Conflicts)
	}
}

func TestOwnedFirewallSectionsClearStaleEnabledAndFamily(t *testing.T) {
	site := model.Site{UUID: "site-uuid", Networks: []model.Network{{
		ID: 1, Name: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true,
	}}}
	doc, _, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway},
		routerCaps(), gwExisting())
	if err != nil {
		t.Fatal(err)
	}
	firewall := map[string]map[string]string{}
	for _, sec := range doc.Sections {
		if sec.Config != "firewall" {
			continue
		}
		vals := map[string]string{".type": sec.Type, "enabled": "0", "family": "ipv4"}
		for key, value := range sec.Values {
			vals[key] = value
		}
		for key, values := range sec.Lists {
			vals[key] = strings.Join(values, " ")
		}
		firewall[sec.Name] = vals
	}
	plan := doc.Plan(NewExisting(map[string]map[string]map[string]string{"firewall": firewall}))
	deleted := map[string]bool{}
	for _, op := range plan.Ops {
		if op.Config == "firewall" && (op.Option == "enabled" || op.Option == "family") {
			deleted[op.Section+"."+op.Option] = true
		}
	}
	for _, section := range []string{"oowrt_zone_guest", "oowrt_fwd_guest_wan"} {
		for _, option := range []string{"enabled", "family"} {
			if !deleted[section+"."+option] {
				t.Errorf("did not clear stale %s from %s: %+v", option, section, plan.Ops)
			}
		}
	}
}

func TestDefinitivelyDisabledForeignForwardingDoesNotDefeatBlock(t *testing.T) {
	existing := gwExisting()
	existing.Configs["firewall"]["disabled_guest_wan"] = map[string]string{
		".type": "forwarding", "src": "guest", "dest": "wan", "enabled": "0",
	}
	_, rep, err := Render(model.Site{
		UUID: "site-uuid",
		Networks: []model.Network{{ID: 1, Name: "guest", VLAN: 20,
			CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}},
		Zones: []model.ZonePolicy{{Name: "guest", ForwardTo: []string{}, Explicit: true}},
	}, model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasConflicts() {
		t.Fatalf("enabled=0 foreign forwarding was treated as active: %v", rep.Conflicts)
	}
}

func TestForeignRulesAndDNATCannotContradictMatrix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		section   map[string]string
		forwardTo []string
		conflict  bool
	}{
		{"accept defeats block", map[string]string{".type": "rule", "src": "guest", "dest": "wan", "target": "ACCEPT", "proto": "tcp", "dest_port": "22"}, []string{}, true},
		{"accept wildcard defeats block", map[string]string{".type": "rule", "src": "guest", "dest": "*", "target": "ACCEPT"}, []string{"wan"}, true},
		{"reject defeats allow", map[string]string{".type": "rule", "src": "guest", "dest": "wan", "target": "REJECT", "proto": "udp", "dest_port": "53"}, []string{"wan"}, true},
		{"drop wildcard defeats allow", map[string]string{".type": "rule", "src": "*", "dest": "*", "target": "DROP"}, []string{"wan"}, true},
		{"disabled accept is inert", map[string]string{".type": "rule", "src": "guest", "dest": "wan", "target": "ACCEPT", "enabled": "0"}, []string{}, false},
		{"disabled reject is inert", map[string]string{".type": "rule", "src": "guest", "dest": "wan", "target": "REJECT", "enabled": "0"}, []string{"wan"}, false},
		{"accept agrees with allow", map[string]string{".type": "rule", "src": "guest", "dest": "wan", "target": "ACCEPT"}, []string{"wan"}, false},
		{"reject agrees with block", map[string]string{".type": "rule", "src": "guest", "dest": "wan", "target": "REJECT"}, []string{}, false},
		{"dnat without dest defeats block", map[string]string{".type": "redirect", "src": "guest", "target": "DNAT", "dest_ip": "192.168.1.2"}, []string{}, true},
		{"dnat dest does not prove an allowed edge", map[string]string{".type": "redirect", "src": "guest", "dest": "wan", "target": "DNAT", "dest_ip": "192.168.1.2"}, []string{"wan"}, true},
		{"wildcard dnat defeats allow", map[string]string{".type": "redirect", "src": "*", "target": "DNAT", "dest_ip": "192.168.1.2"}, []string{"wan"}, true},
		{"default dnat without dest defeats block", map[string]string{".type": "redirect", "src": "guest", "dest_ip": "192.168.1.2"}, []string{}, true},
		{"dnat without dest ip is router-local", map[string]string{".type": "redirect", "src": "guest", "dest": "wan", "target": "DNAT"}, []string{}, false},
		{"default redirect without dest ip is router-local", map[string]string{".type": "redirect", "src": "guest", "dest": "wan"}, []string{}, false},
		{"router-local redirect is inert", map[string]string{".type": "redirect", "src": "guest", "target": "REDIRECT"}, []string{}, false},
		{"disabled dnat without dest is inert", map[string]string{".type": "redirect", "src": "guest", "target": "DNAT", "dest_ip": "192.168.1.2", "enabled": "0"}, []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := gwExisting()
			existing.Configs["firewall"]["human_policy"] = tc.section
			_, rep, err := Render(model.Site{
				UUID: "site-uuid",
				Networks: []model.Network{{ID: 1, Name: "guest", VLAN: 20,
					CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}},
				Zones: []model.ZonePolicy{{Name: "guest", ForwardTo: tc.forwardTo, Explicit: true}},
			}, model.Device{ID: 1, Role: model.RoleGateway}, routerCaps(), existing)
			if err != nil {
				t.Fatal(err)
			}
			if rep.HasConflicts() != tc.conflict {
				t.Fatalf("conflicts = %+v, want conflict=%v", rep.Conflicts, tc.conflict)
			}
		})
	}
}
