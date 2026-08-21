package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func phase3GuestSite() model.Site {
	return model.Site{
		UUID: "phase3-guest-proof",
		Networks: []model.Network{{
			ID: 2, Name: "testvlan", VLAN: 2, CIDR: "192.168.2.1/24",
			Zone: "lan2", Enabled: true,
		}},
		Zones: []model.ZonePolicy{{
			Name: "lan2", ForwardTo: []string{"wan"}, Explicit: true,
		}},
		Groups: []model.APGroup{{ID: 1, Name: "all-aps", DeviceIDs: []int64{1, 2}}},
		WLANs: []model.WLAN{{
			ID: 9, SSID: "phase3-guest", NetworkID: 2, GroupID: 1,
			// Hostapd isolation plus bridge-port isolation covers both BSSes on
			// this AP. A second AP still requires additional L2 policy.
			Bands:    []model.Band{model.Band2G, model.Band5G},
			Security: model.Security{Mode: model.SecPSK2, Key: "phase3-test-key", PMF: model.PMFDisabled},
			Roaming:  model.Roaming{},
			Options:  model.WLANOptions{Isolate: true},
			Enabled:  true,
		}},
	}
}

func TestPhase3GuestProofRendersExactWRTContract(t *testing.T) {
	caps := marvellCaps()
	caps.Ports = capability.Ports{
		Bridge: "br-lan", LAN: []string{"lan1", "lan2", "lan3", "lan4"}, WAN: "wan",
	}
	doc, rep, err := Render(phase3GuestSite(), model.Device{
		ID: 1, Functions: model.DeviceFunctions{
			model.FunctionGateway, model.FunctionAP, model.FunctionSwitch,
		},
	}, caps, vlanAware())
	if err != nil || rep.HasConflicts() {
		t.Fatalf("render: %v, conflicts=%v", err, rep.Conflicts)
	}

	network := sectionsIn(doc, "network")
	if bv := network["oowrt_bv2"]; bv.Values["device"] != "br-lan" ||
		bv.Values["vlan"] != "2" || bv.Values["local"] != "1" ||
		strings.Join(bv.Lists["ports"], " ") != "lan1:t lan2:t lan3:t lan4:t" {
		t.Fatalf("bridge VLAN = %+v", bv)
	}
	if iface := network["oowrt_net_testvlan"]; iface.Values["device"] != "br-lan.2" ||
		iface.Values["ipaddr"] != "192.168.2.1" || iface.Values["netmask"] != "255.255.255.0" {
		t.Fatalf("guest interface = %+v", iface)
	}
	if dhcp := sectionsIn(doc, "dhcp")["oowrt_dhcp_testvlan"]; dhcp.Values["start"] != "100" ||
		dhcp.Values["limit"] != "150" || dhcp.Values["leasetime"] != "12h" {
		t.Fatalf("guest DHCP = %+v", dhcp)
	}

	firewall := sectionsIn(doc, "firewall")
	zone := firewall["oowrt_zone_lan2"]
	if zone.Values["input"] != "REJECT" || zone.Values["output"] != "ACCEPT" ||
		zone.Values["forward"] != "REJECT" ||
		strings.Join(zone.Lists["network"], " ") != "oowrt_net_testvlan" {
		t.Fatalf("guest zone = %+v", zone)
	}
	var forwardings int
	for _, section := range firewall {
		if section.Type == "forwarding" {
			forwardings++
			if section.Values["src"] != "lan2" || section.Values["dest"] != "wan" {
				t.Fatalf("unexpected guest forwarding = %+v", section)
			}
		}
	}
	if forwardings != 1 {
		t.Fatalf("rendered %d forwarding edges, want only lan2 -> wan", forwardings)
	}
	for _, name := range []string{"oowrt_in_lan2_dhcp", "oowrt_in_lan2_dns"} {
		if firewall[name].Values["target"] != "ACCEPT" {
			t.Fatalf("router-local service rule %s = %+v", name, firewall[name])
		}
	}

	wireless := sectionsIn(doc, "wireless")
	if len(wireless) != 2 {
		t.Fatalf("rendered %d guest BSSes, want both WRT radios", len(wireless))
	}
	for name, section := range wireless {
		if section.Values["network"] != "oowrt_net_testvlan" ||
			section.Values["isolate"] != "1" || section.Values["bridge_isolate"] != "1" ||
			section.Values["ieee80211w"] != "0" ||
			section.Values["ieee80211r"] != "0" || section.Values["ft_over_ds"] != "0" {
			t.Errorf("%s guest safety options = %+v", name, section.Values)
		}
	}
}

func TestNonIsolatedWLANOmitsUnsupportedFalseAndClearsStaleBridgeIsolation(t *testing.T) {
	site := phase3GuestSite()
	site.WLANs[0].Options.Isolate = false
	caps := marvellCaps()
	caps.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1"}, WAN: "wan"}
	doc, rep, err := Render(site, model.Device{
		ID: 1, Functions: model.DeviceFunctions{model.FunctionGateway, model.FunctionAP, model.FunctionSwitch},
	}, caps, vlanAware())
	if err != nil || rep.HasConflicts() {
		t.Fatalf("render: %v, conflicts=%v", err, rep.Conflicts)
	}
	wireless := sectionsIn(doc, "wireless")
	for name, section := range wireless {
		if section.Values["isolate"] != "0" {
			t.Errorf("%s did not explicitly clear hostapd isolation: %+v", name, section.Values)
		}
		if _, written := section.Values["bridge_isolate"]; written {
			t.Errorf("%s wrote bridge_isolate=0, which older wifi-scripts reject: %+v",
				name, section.Values)
		}
	}
	for name, section := range wireless {
		existing := WirelessOnly(map[string]map[string]string{name: {
			"ssid": section.Values["ssid"], "device": section.Values["device"],
			OwnershipTag: "1", "isolate": "1", "bridge_isolate": "1",
		}})
		var clearedHostapd, clearedBridge bool
		for _, op := range doc.Plan(existing).Ops {
			if op.Config != "wireless" || op.Section != name {
				continue
			}
			if op.Kind == applyengine.OpSet && op.Values["isolate"] == "0" {
				clearedHostapd = true
			}
			if op.Kind == applyengine.OpDelete && op.Option == "bridge_isolate" {
				clearedBridge = true
			}
		}
		if !clearedHostapd || !clearedBridge {
			t.Errorf("plan did not clear both stale isolation layers on %s: %+v",
				name, doc.Plan(existing).Ops)
		}
		break
	}
}

func TestPhase3GuestProofTruthfullyOmitsLegacyC6(t *testing.T) {
	caps := dualBandCaps()
	caps.Ports = capability.Ports{Bridge: "eth0.1", WAN: "eth0.2"}
	doc, rep, err := Render(phase3GuestSite(), model.Device{
		ID: 2, Functions: model.DeviceFunctions{model.FunctionAP, model.FunctionSwitch},
	}, caps, Existing{})
	if err != nil || rep.HasConflicts() {
		t.Fatalf("render: %v, conflicts=%v", err, rep.Conflicts)
	}
	if len(doc.Sections) != 0 {
		t.Fatalf("legacy swconfig C6 received VLAN-bound sections: %+v", doc.Sections)
	}
	var reasons string
	for _, omission := range rep.Omissions {
		reasons += omission.Reason + "\n"
	}
	for _, want := range []string{"swconfig", "no usable interface", "will not broadcast this WLAN"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("C6 omission does not say %q: %s", want, reasons)
		}
	}
}
