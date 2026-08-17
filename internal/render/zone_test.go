package render

import (
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

// A zone name the device already uses is the ownership rule applied to the
// namespace fw4 actually keys on. Our section name would not collide; the ZONE
// would, and the device would end up with two called lan.
//
// This was the default path: store.SaveNetwork stamped every network with zone
// "lan" and nothing in the UI ever set it.
func TestZoneNameTheDeviceAlreadyOwnsIsAConflict(t *testing.T) {
	_, rep := renderGateway(t, []model.Network{
		{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "lan", Enabled: true},
	})
	if !rep.HasConflicts() {
		t.Fatal("a second firewall zone named lan was rendered beside the " +
			"device's own, with input REJECT and forward REJECT, and nothing " +
			"said so")
	}
	if !strings.Contains(rep.Conflicts[0].Reason, "lan") {
		t.Errorf("the conflict does not name the zone: %q", rep.Conflicts[0].Reason)
	}
}

// Two zone names that fw4 cannot tell apart are one zone, whatever we call the
// sections. Merging two networks the operator separated is a firewall policy
// nobody chose.
func TestZoneNamesThatCollapsePastTheCapAreRefused(t *testing.T) {
	_, rep := renderGateway(t, []model.Network{
		{ID: 1, Name: "a", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guest_network_a", Enabled: true},
		{ID: 2, Name: "b", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "guest_network_b", Enabled: true},
	})
	if !rep.HasConflicts() {
		t.Fatal("two zone names identical to fw4 were merged silently")
	}
	r := rep.Conflicts[0].Reason
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
