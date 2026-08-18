package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// dualBandCaps mirrors the reference device: radio0 is 5 GHz, radio1 is 2.4.
func dualBandCaps() *capability.Registry {
	r := capability.NewRegistry()
	r.Class = capability.ClassA
	r.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5180, Hardware: "Generic MAC80211"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 2412, Hardware: "Generic MAC80211"},
	}
	return r
}

func singleBandCaps() *capability.Registry {
	r := capability.NewRegistry()
	r.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 2412, Hardware: "Generic MAC80211"},
	}
	return r
}

func testSite() model.Site {
	return model.Site{
		UUID: "9d1f0c7a-2b44-4f8e-9f7a-1c2d3e4f5a6b",
		Name: "home",
		Networks: []model.Network{
			{ID: 1, Name: "lan", VLAN: 1, CIDR: "192.168.1.0/24", Zone: "lan", Enabled: true},
		},
		Groups: []model.APGroup{{ID: 1, Name: "all-aps", DeviceIDs: []int64{7, 8, 9}}},
		WLANs: []model.WLAN{{
			ID: 3, SSID: "Home", NetworkID: 1, GroupID: 1,
			Bands:    []model.Band{model.Band2G, model.Band5G},
			Security: model.Security{Mode: model.SecSAEMixed, Key: "s3cret", PMF: model.PMFOptional},
			Roaming:  model.Roaming{FT: true, FTOverDS: true, KV: true},
			Enabled:  true,
		}},
	}
}

func sectionByName(d Doc, name string) (Section, bool) {
	for _, s := range d.Sections {
		if s.Name == name {
			return s, true
		}
	}
	return Section{}, false
}

// The product, in one test: one WLAN definition lands on both bands of a
// device, correctly, with no per-radio configuration by the operator.
func TestWLANFansOutAcrossBothBands(t *testing.T) {
	doc, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rep.HasConflicts() {
		t.Fatalf("unexpected conflicts: %v", rep.Conflicts)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("want one wifi-iface per band, got %d: %+v", len(doc.Sections), doc.Sections)
	}
	for _, want := range []string{"oowrt_wlan3_radio0", "oowrt_wlan3_radio1"} {
		s, ok := sectionByName(doc, want)
		if !ok {
			t.Fatalf("missing section %s", want)
		}
		if s.Values["ssid"] != "Home" || s.Values["encryption"] != "sae-mixed" ||
			s.Values["key"] != "s3cret" || s.Values["network"] != "lan" {
			t.Errorf("%s has wrong core values: %v", want, s.Values)
		}
		if s.Values[OwnershipTag] != "1" {
			t.Errorf("%s must carry the ownership tag", want)
		}
		if s.Type != "wifi-iface" || s.Config != "wireless" {
			t.Errorf("%s wrong config/type: %s/%s", want, s.Config, s.Type)
		}
	}
	// Each section must target its own radio.
	r0, _ := sectionByName(doc, "oowrt_wlan3_radio0")
	r1, _ := sectionByName(doc, "oowrt_wlan3_radio1")
	if r0.Values["device"] != "radio0" || r1.Values["device"] != "radio1" {
		t.Errorf("radios not bound correctly: %q / %q",
			r0.Values["device"], r1.Values["device"])
	}
}

// The cross-device guarantee: every AP must derive the SAME mobility domain,
// independently, or fast transition simply does not happen between them.
func TestMobilityDomainIsIdenticalAcrossDevicesAndStable(t *testing.T) {
	site := testSite()
	var seen string
	for _, dev := range []model.Device{{ID: 7}, {ID: 8}, {ID: 9}} {
		doc, _, err := Render(site, dev, dualBandCaps(), Existing{})
		if err != nil {
			t.Fatalf("Render(dev %d): %v", dev.ID, err)
		}
		for _, s := range doc.Sections {
			md := s.Values["mobility_domain"]
			if md == "" {
				t.Fatalf("device %d section %s has no mobility_domain", dev.ID, s.Name)
			}
			if len(md) != 4 {
				t.Errorf("mobility_domain should be 4 hex chars, got %q", md)
			}
			if seen == "" {
				seen = md
			} else if md != seen {
				t.Fatalf("mobility domains differ (%q vs %q); roaming between "+
					"these APs would silently not work", seen, md)
			}
		}
	}
	// And a different WLAN must not collide with it.
	other := MobilityDomain(site.UUID, 4)
	if other == seen {
		t.Errorf("WLAN 3 and WLAN 4 derived the same mobility domain %q", seen)
	}
	// Different sites, same WLAN id, must also differ.
	if MobilityDomain("another-site-uuid", 3) == seen {
		t.Error("two sites with WLAN 3 collided; overlapping coverage would clash")
	}
}

// A band the device cannot serve is an absence, not a failure.
func TestMissingBandIsOmittedNotAnError(t *testing.T) {
	site := testSite()
	site.WLANs[0].Bands = []model.Band{model.Band2G, model.Band5G, model.Band6G}

	doc, rep, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("a device without 6 GHz must not fail the render: %v", err)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("2g and 5g should still render, got %d sections", len(doc.Sections))
	}
	var found bool
	for _, o := range rep.Omissions {
		if strings.Contains(o.Reason, "6g") {
			found = true
		}
	}
	if !found {
		t.Error("the omission must be reported, or the operator silently loses " +
			"a band they asked for")
	}
}

func TestSingleBandDeviceRendersOnlyItsBand(t *testing.T) {
	doc, rep, err := Render(testSite(), model.Device{ID: 7}, singleBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("want 1 section on a single-band device, got %d", len(doc.Sections))
	}
	if doc.Sections[0].Values["device"] != "radio0" {
		t.Errorf("wrong radio: %v", doc.Sections[0].Values)
	}
	if len(rep.Omissions) == 0 {
		t.Error("the unrenderable band should be reported")
	}
}

// You are a guest on someone else's router: a foreign section is never
// overwritten, and the clash is surfaced rather than resolved.
func TestForeignSectionWithOurNameIsAConflict(t *testing.T) {
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan3_radio0": {"ssid": "SomethingElse", "device": "radio0"},
	})
	_, rep, err := Render(testSite(), model.Device{ID: 7}, dualBandCaps(), existing)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !rep.HasConflicts() {
		t.Fatal("a section with our name that is not ours must be a conflict")
	}
	// Why, and what to do about it. A conflict BLOCKS the whole apply, so a
	// reason that stops at "refusing to overwrite config we did not write"
	// leaves the operator with a device that will not take any change and no
	// idea which lever moves it.
	reason := rep.Conflicts[0].Reason
	if !strings.Contains(reason, "ownership marker") {
		t.Errorf("the conflict does not say why: %q", reason)
	}
	if !strings.Contains(reason, "To unblock this apply") {
		t.Errorf("the conflict does not say how to unblock it: %q", reason)
	}
	if !strings.Contains(reason, "oowrt_wlan3_radio0") {
		t.Errorf("the conflict does not name the section: %q", reason)
	}
}

func TestForeignSSIDOnTheSameRadioIsAConflict(t *testing.T) {
	existing := WirelessOnly(map[string]map[string]string{
		"default_radio0": {"ssid": "Home", "device": "radio0"}, // human-made
	})
	_, rep, err := Render(testSite(), model.Device{ID: 7}, dualBandCaps(), existing)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !rep.HasConflicts() {
		t.Fatal("the same SSID published by a foreign section on the same radio " +
			"must be a conflict, not a silent duplicate")
	}
}

// Our own section from a previous render is not a conflict — it is the thing
// we are updating.
func TestOurOwnPriorSectionIsNotAConflict(t *testing.T) {
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan3_radio0": {"ssid": "Home", "device": "radio0", OwnershipTag: "1"},
	})
	doc, rep, err := Render(testSite(), model.Device{ID: 7}, dualBandCaps(), existing)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rep.HasConflicts() {
		t.Fatalf("re-rendering our own section must not conflict: %v", rep.Conflicts)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("want 2 sections, got %d", len(doc.Sections))
	}
}

// 802.11r on WPA2-PSK breaks older clients, so it needs an explicit opt-in.
func TestFastTransitionOnPSK2RequiresExplicitOptIn(t *testing.T) {
	site := testSite()
	site.WLANs[0].Security = model.Security{Mode: model.SecPSK2, Key: "hunter22"}
	site.WLANs[0].Roaming = model.Roaming{FT: true}

	doc, rep, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Not ENABLED — which is not the same as "the key is missing", and the
	// difference is the whole of §5aw. An omitted option is not "off" on the
	// device, it is whatever the last apply left there, so the renderer now
	// writes the disabled state explicitly and this asserts the state rather
	// than the absence.
	for _, s := range doc.Sections {
		if s.Values["ieee80211r"] != "0" {
			t.Errorf("802.11r must not be enabled on WPA2-PSK without the opt-in, "+
				"and must be explicitly disabled rather than omitted: %v", s.Values)
		}
	}
	if len(rep.Omissions) == 0 {
		t.Error("the operator must be told why roaming did not render")
	}

	// With the warning accepted, it renders.
	site.WLANs[0].Roaming.FTWithPSK2 = true
	doc, _, err = Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, s := range doc.Sections {
		if s.Values["ieee80211r"] != "1" {
			t.Errorf("after opting in, 802.11r should render: %v", s.Values)
		}
	}
}

// WPA3 without protected management frames produces an AP clients reject.
func TestSAEForcesProtectedManagementFrames(t *testing.T) {
	site := testSite()
	site.WLANs[0].Security = model.Security{Mode: model.SecSAE, Key: "s3cret", PMF: model.PMFOptional}
	doc, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, s := range doc.Sections {
		if s.Values["ieee80211w"] != string(model.PMFRequired) {
			t.Errorf("sae must render ieee80211w=2, got %q", s.Values["ieee80211w"])
		}
	}
}

func TestOpenNetworkRendersNoKey(t *testing.T) {
	site := testSite()
	site.WLANs[0].Security = model.Security{Mode: model.SecNone}
	doc, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, s := range doc.Sections {
		if _, has := s.Values["key"]; has {
			t.Errorf("an open network must not render a key: %v", s.Values)
		}
	}
}

func TestDeviceOutsideTheGroupGetsNothing(t *testing.T) {
	doc, _, err := Render(testSite(), model.Device{ID: 99}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc.Sections) != 0 {
		t.Fatalf("a device in no group should render nothing, got %d", len(doc.Sections))
	}
}

func TestDisabledWLANRendersNothing(t *testing.T) {
	site := testSite()
	site.WLANs[0].Enabled = false
	doc, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc.Sections) != 0 {
		t.Fatalf("a disabled WLAN should render nothing, got %d", len(doc.Sections))
	}
}

// Rendering must be deterministic, or the diff preview churns and the stored
// hashes report drift that is not there.
func TestRenderIsDeterministic(t *testing.T) {
	site, caps := testSite(), dualBandCaps()
	first, _, err := Render(site, model.Device{ID: 7}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, _, err := Render(site, model.Device{ID: 7}, caps, Existing{})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if len(again.Sections) != len(first.Sections) {
			t.Fatalf("section count changed between renders")
		}
		for j := range again.Sections {
			if again.Sections[j].Hash() != first.Sections[j].Hash() {
				t.Fatalf("render %d differs at section %d; the hash is stored and "+
					"would report phantom drift on every poll", i, j)
			}
		}
	}
}

func TestSectionHashIgnoresMapOrdering(t *testing.T) {
	a := Section{Config: "wireless", Type: "wifi-iface", Name: "x",
		Values: map[string]string{"a": "1", "b": "2", "c": "3"}}
	b := Section{Config: "wireless", Type: "wifi-iface", Name: "x",
		Values: map[string]string{"c": "3", "b": "2", "a": "1"}}
	if a.Hash() != b.Hash() {
		t.Fatal("hash must be canonical: Go map ordering is randomised, and a " +
			"hash that varies would report drift every poll")
	}
	c := a
	c.Values = map[string]string{"a": "1", "b": "2", "c": "4"}
	if c.Hash() == a.Hash() {
		t.Fatal("a changed value must change the hash")
	}
}

// The document has to become staged operations the apply engine can run.
func TestPlanStagesAddsAndUpdatesWithoutCommitting(t *testing.T) {
	doc, _, err := Render(testSite(), model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	plan := doc.Plan(Existing{})
	if len(plan.Ops) != 2 {
		t.Fatalf("want 2 ops, got %d", len(plan.Ops))
	}
	for _, op := range plan.Ops {
		if op.Kind != applyengine.OpAdd {
			t.Errorf("a new section should be an add, got %s", op.Kind)
		}
		if op.Name == "" {
			t.Error("sections must be NAMED, or a re-render appends duplicates")
		}
	}

	// Re-planning against a device that already has them updates in place.
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan3_radio0": {OwnershipTag: "1"},
		"oowrt_wlan3_radio1": {OwnershipTag: "1"},
	})
	plan = doc.Plan(existing)
	for _, op := range plan.Ops {
		if op.Kind != applyengine.OpSet {
			t.Errorf("an existing section should be a set, got %s", op.Kind)
		}
	}
}

// A WLAN removed from the site model must be removed from the device — but
// only sections we own.
func TestPruneRemovesOurStaleSectionsAndNothingElse(t *testing.T) {
	site := testSite()
	doc, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan3_radio0": {OwnershipTag: "1"},
		"oowrt_wlan3_radio1": {OwnershipTag: "1"},
		"oowrt_wlan9_radio0": {OwnershipTag: "1"},   // ours, no longer wanted
		"default_radio0":     {"ssid": "TheirWifi"}, // a human's
		"guest_ap":           {"ssid": "Guest"},     // also a human's
	})
	ops := doc.Prune(existing)
	if len(ops) != 1 {
		t.Fatalf("exactly one stale owned section should be pruned, got %d: %+v",
			len(ops), ops)
	}
	if ops[0].Section != "oowrt_wlan9_radio0" || ops[0].Kind != applyengine.OpDelete {
		t.Fatalf("wrong prune target: %+v", ops[0])
	}
}

func TestInvalidSiteIsRejectedBeforeTouchingADevice(t *testing.T) {
	cases := map[string]func(*model.Site){
		"no UUID":         func(s *model.Site) { s.UUID = "" },
		"unknown network": func(s *model.Site) { s.WLANs[0].NetworkID = 42 },
		"unknown group":   func(s *model.Site) { s.WLANs[0].GroupID = 42 },
		"no bands":        func(s *model.Site) { s.WLANs[0].Bands = nil },
		"key-less WPA":    func(s *model.Site) { s.WLANs[0].Security.Key = "" },
		"duplicate VLAN":  func(s *model.Site) { s.Networks = append(s.Networks, model.Network{ID: 2, Name: "guest", VLAN: 1}) },
		"empty SSID":      func(s *model.Site) { s.WLANs[0].SSID = "  " },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			site := testSite()
			mutate(&site)
			if _, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{}); err == nil {
				t.Fatal("an invalid site must be rejected before any device is touched")
			}
		})
	}
}

func TestNilCapabilitiesRenderNothingRatherThanGuessing(t *testing.T) {
	doc, rep, err := Render(testSite(), model.Device{ID: 7}, nil, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(doc.Sections) != 0 {
		t.Fatal("with no capability information we cannot know which radios " +
			"exist; rendering anything would be a guess")
	}
	if len(rep.Omissions) == 0 {
		t.Error("the omission should be reported")
	}
}

// A device that already holds what we would write produces no operations.
//
// Without this, every preview reports changes that change nothing — "2 changes
// pending" against a device that already matches — which is how an operator
// learns to stop reading the preview. It also means DevicePlan.Empty() could
// never be true, so a no-op apply would still stage, apply and confirm against
// a device for no reason, with a rollback armed each time.
func TestPlanSkipsSectionsThatAlreadyMatch(t *testing.T) {
	doc := Doc{Sections: []Section{{
		Config: "wireless", Type: "wifi-iface", Name: "oowrt_wlan1_radio0",
		Values: map[string]string{
			"ssid": "Home", "device": "radio0", "encryption": "sae-mixed",
			OwnershipTag: "1",
		},
	}}}

	// Exactly what we would write, plus device-added keys we do not manage.
	same := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan1_radio0": {
			"ssid": "Home", "device": "radio0", "encryption": "sae-mixed",
			OwnershipTag: "1",
			// The device's own additions must not count as a difference.
			".type": "wifi-iface", ".index": "3", "macaddr": "aa:bb:cc:dd:ee:ff",
		},
	})
	if ops := doc.Plan(same).Ops; len(ops) != 0 {
		t.Errorf("a matching section produced %d op(s): %+v", len(ops), ops)
	}

	// One managed value different: a set, not an add.
	differs := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan1_radio0": {
			"ssid": "Renamed", "device": "radio0", "encryption": "sae-mixed",
			OwnershipTag: "1",
		},
	})
	ops := doc.Plan(differs).Ops
	if len(ops) != 1 {
		t.Fatalf("a changed section produced %d op(s), want 1", len(ops))
	}
	if ops[0].Kind != applyengine.OpSet {
		t.Errorf("kind = %v, want set — add would drop options a previous "+
			"version of us wrote and this one no longer manages", ops[0].Kind)
	}

	// Absent entirely: an add.
	ops = doc.Plan(WirelessOnly(map[string]map[string]string{})).Ops
	if len(ops) != 1 || ops[0].Kind != applyengine.OpAdd {
		t.Errorf("a missing section produced %+v, want one add", ops)
	}
}

// ---- networks ----

func netCaps() *capability.Registry {
	r := capability.NewRegistry()
	r.Ports = capability.Ports{
		Bridge: "br-lan",
		LAN:    []string{"lan1", "lan2", "lan3", "lan4"},
		WAN:    "wan",
	}
	return r
}

// vlanAware is a device whose operator has already enabled VLAN filtering —
// the precondition for oonfeeWRT managing any additional VLAN. See
// bridgeIsVLANAware for why we will not create that state ourselves.
func vlanAware() Existing {
	return NewExisting(map[string]map[string]map[string]string{
		"network": {
			"their_bv1": {".type": "bridge-vlan", "device": "br-lan", "vlan": "1"},
		},
	})
}

func sectionsIn(doc Doc, config string) map[string]Section {
	out := map[string]Section{}
	for _, s := range doc.Sections {
		if s.Config == config {
			out[s.Name] = s
		}
	}
	return out
}

// A gateway renders the whole stack; an AP renders only the bridge-VLAN.
//
// An AP that also ran DHCP on the same VLAN would put two servers on one
// broadcast domain, which fails intermittently and is miserable to diagnose.
// The subsetting is by role and tested, not an if-cascade that happens to work
// on one topology.
func TestNetworkRenderingIsRoleAware(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Zone: "guest", Enabled: true,
	}}}

	gw, _, err := Render(site, model.Device{ID: 1, Name: "gw", Role: "gateway"},
		netCaps(), vlanAware())
	if err != nil {
		t.Fatal(err)
	}
	net := sectionsIn(gw, "network")
	if _, ok := net["oowrt_bv45"]; !ok {
		t.Error("gateway did not render the bridge-VLAN")
	}
	if iface, ok := net["oowrt_net_iot"]; !ok {
		t.Error("gateway did not render the interface")
	} else {
		if iface.Values["ipaddr"] != "10.7.45.1" || iface.Values["netmask"] != "255.255.255.0" {
			t.Errorf("interface addressing = %v", iface.Values)
		}
		if iface.Values["device"] != "br-lan.45" {
			t.Errorf("interface device = %q, want br-lan.45", iface.Values["device"])
		}
	}
	if len(sectionsIn(gw, "dhcp")) != 1 {
		t.Error("gateway did not render a DHCP server")
	}
	fw := sectionsIn(gw, "firewall")
	if len(fw) != 2 {
		t.Errorf("gateway rendered %d firewall sections, want a zone and a forwarding", len(fw))
	}

	ap, _, err := Render(site, model.Device{ID: 2, Name: "ap", Role: "ap"},
		netCaps(), vlanAware())
	if err != nil {
		t.Fatal(err)
	}
	apNet := sectionsIn(ap, "network")
	if _, ok := apNet["oowrt_bv45"]; !ok {
		t.Error("AP did not render the bridge-VLAN, so tagged frames cannot traverse it")
	}
	if len(apNet) != 1 {
		t.Errorf("AP rendered %d network sections, want only the bridge-VLAN: %v",
			len(apNet), apNet)
	}
	if n := len(sectionsIn(ap, "dhcp")); n != 0 {
		t.Errorf("AP rendered %d DHCP sections; two servers on one broadcast "+
			"domain fail intermittently", n)
	}
	if n := len(sectionsIn(ap, "firewall")); n != 0 {
		t.Errorf("AP rendered %d firewall sections; routing is the gateway's job", n)
	}
}

// Every LAN port carries the VLAN tagged. An untagged member would change what
// an existing port already does, which is the device's config and not ours.
func TestBridgeVLANTagsEveryPort(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Enabled: true,
	}}}
	doc, _, _ := Render(site, model.Device{ID: 1, Role: "gateway"}, netCaps(), vlanAware())
	bv := sectionsIn(doc, "network")["oowrt_bv45"]
	if got := strings.Join(bv.Lists["ports"], " "); got != "lan1:t lan2:t lan3:t lan4:t" {
		t.Errorf("ports = %q; every port must be tagged, never untagged — an "+
			"untagged member repurposes a port the device already uses", got)
	}
	if _, wrong := bv.Values["ports"]; wrong {
		t.Error("ports was rendered as a plain option; UCI needs a list, and the " +
			"string form is stored without complaint and then ignored by netifd")
	}
	if bv.Values["device"] != "br-lan" || bv.Values["vlan"] != "45" {
		t.Errorf("bridge-vlan = %v", bv.Values)
	}
}

// VLAN 1 is the device's existing LAN, which we do not own.
func TestVLAN1IsNotRenderedAndSaysWhy(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true,
	}}}
	doc, rep, _ := Render(site, model.Device{ID: 1, Role: "gateway"}, netCaps(), vlanAware())
	if len(doc.Sections) != 0 {
		t.Errorf("VLAN 1 rendered %d sections; the existing LAN is not ours to "+
			"rewrite: %+v", len(doc.Sections), doc.Sections)
	}
	if len(rep.Omissions) == 0 {
		t.Error("nothing was rendered and no reason was given")
	}
}

// A device that will not report its ports gets no VLAN and a reason, never
// invented port names.
func TestNetworkWithoutAPortMapIsOmitted(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Enabled: true,
	}}}
	doc, rep, _ := Render(site, model.Device{ID: 1, Role: "gateway"},
		capability.NewRegistry(), vlanAware())
	if len(doc.Sections) != 0 {
		t.Errorf("rendered %d sections with no known ports: %+v",
			len(doc.Sections), doc.Sections)
	}
	if len(rep.Omissions) == 0 {
		t.Error("no port map and no explanation")
	}
}

// A new zone defaults to "can reach out, cannot reach in". That is the safe
// direction to be wrong in: an operator who wanted a guest network and got an
// isolated one notices at once; one who wanted isolation and got an open zone
// may never notice.
func TestNewZoneDefaultsToClosed(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Zone: "guest", Enabled: true,
	}}}
	doc, _, _ := Render(site, model.Device{ID: 1, Role: "gateway"}, netCaps(), vlanAware())
	zone := sectionsIn(doc, "firewall")["oowrt_zone_guest"]
	if zone.Values["input"] != "REJECT" || zone.Values["forward"] != "REJECT" {
		t.Errorf("zone = %v; a new zone must not accept input or forward by default",
			zone.Values)
	}
	if zone.Values["output"] != "ACCEPT" {
		t.Errorf("zone output = %q; a network that cannot reach anything is not a network",
			zone.Values["output"])
	}
}

// A section name a human typed must not become a config file the device
// rejects.
//
// It must also not become a DIFFERENT operator's section. safe() used to cap
// everything at 11 characters, which is fw4's limit on a zone NAME and not a
// limit on UCI section names at all — so "Guest Network A" and "Guest Network
// B" produced one set of sections between them. The cap existed to stop two
// zones colliding past it and, applied here, caused exactly that.
//
// The old test asserted `got != want && len(got) > 11`, which fails only when
// a value is BOTH wrong and too long: every wrong-but-short answer passed. The
// mapping is checked outright now.
func TestNamesAreMadeSafeForUCI(t *testing.T) {
	for in, want := range map[string]string{
		"Guest WiFi (2.4)":      "guest_wifi_24",
		"iot":                   "iot",
		"  Spaced  Out  ":       "spaced__out",
		"!!!":                   "net",
		"averyveryverylongzone": "averyveryverylongzone",
	} {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
	// Distinct names stay distinct, which is the whole job.
	if safe("Guest Network A") == safe("Guest Network B") {
		t.Error("two distinct network names produced one section name; their " +
			"interface, DHCP and firewall sections would all collide and the " +
			"device would keep whichever came last")
	}
}

// The cap is real, and it belongs to the zone name fw4 reads.
func TestFirewallZoneNamesAreCappedWhereFw4CapsThem(t *testing.T) {
	if got := fwZoneName("averyveryverylongzone"); len(got) > maxZoneName {
		t.Errorf("fwZoneName = %q, longer than fw4's %d-character cap", got, maxZoneName)
	}
	if got := fwZoneName("iot"); got != "iot" {
		t.Errorf("fwZoneName(iot) = %q", got)
	}
}

func TestSplitCIDRRejectsNonsense(t *testing.T) {
	if ip, mask, ok := splitCIDR("10.7.45.1/24"); !ok || ip != "10.7.45.1" || mask != "255.255.255.0" {
		t.Errorf("splitCIDR = %q %q %v", ip, mask, ok)
	}
	if _, mask, ok := splitCIDR("192.168.9.1/25"); !ok || mask != "255.255.255.128" {
		t.Errorf("/25 mask = %q %v", mask, ok)
	}
	for _, bad := range []string{"", "10.7.45.1", "10.7.45.1/", "10.7.45.1/33",
		"999.1.1.1/24", "not-an-ip/24", "10.7.45.1/4"} {
		if _, _, ok := splitCIDR(bad); ok {
			t.Errorf("splitCIDR(%q) accepted", bad)
		}
	}
}

// oonfeeWRT will not turn VLAN filtering on by itself.
//
// Measured three times on real hardware: adding a bridge-VLAN to an unfiltered
// br-lan flips vlan_filtering 0 -> 1, br-lan keeps its address and reports UP,
// and every neighbour disappears. The health check passes (the interface IS
// up), the confirm lands, and the device is then unreachable. Connectivity
// survives only if the operator's own lan interface moves to br-lan.1 — a
// section we do not own.
func TestAVLANIsRefusedOnABridgeThatIsNotVLANAware(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Enabled: true,
	}}}
	doc, rep, _ := Render(site, model.Device{ID: 1, Role: "gateway"},
		netCaps(), Existing{}) // nothing existing: an unfiltered bridge

	if n := len(sectionsIn(doc, "network")); n != 0 {
		t.Fatalf("rendered %d network sections onto an unfiltered bridge; this "+
			"takes the LAN down on a real device", n)
	}
	if len(rep.Omissions) == 0 {
		t.Fatal("refused with no explanation")
	}
	// The explanation has to name the one-time change, or an operator is stuck.
	reason := rep.Omissions[0].Reason
	for _, want := range []string{"br-lan.1", "VLAN filtering", "does not own"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal does not mention %q: %s", want, reason)
		}
	}
}

// A site with no tagged VLANs must not switch filtering on at all.
func TestNoVLANsMeansNoBridgeVLANSections(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true,
	}}}
	doc, _, _ := Render(site, model.Device{ID: 1, Role: "gateway"}, netCaps(), vlanAware())
	if n := len(sectionsIn(doc, "network")); n != 0 {
		t.Errorf("rendered %d network sections for a site with no tagged VLAN; "+
			"turning on VLAN filtering for nothing would break the LAN for nothing",
			n)
	}
}

// A device that is ALREADY VLAN-aware belongs to its operator. We add our VLAN
// and leave membership alone — second-guessing an existing layout is exactly
// the "helpful" edit the ownership rule forbids.
func TestExistingBridgeVLANsAreLeftAlone(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Enabled: true,
	}}}
	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {
			"their_vlans": {".type": "bridge-vlan", "device": "br-lan", "vlan": "10"},
		},
	})
	doc, _, _ := Render(site, model.Device{ID: 1, Role: "gateway"}, netCaps(), existing)
	if _, present := sectionsIn(doc, "network")["oowrt_bv1"]; present {
		t.Error("added an untagged default VLAN to a bridge that is already " +
			"VLAN-aware; its membership is the operator's, not ours")
	}
	if _, present := sectionsIn(doc, "network")["oowrt_bv45"]; !present {
		t.Error("our own VLAN should still be rendered")
	}
}

// A UCI list is not a string with spaces in it.
//
// `option ports 'lan1:u* lan2:u*'` where UCI wants `list ports 'lan1:u*'` is
// accepted by uci.set, stored, and then ignored by netifd. Measured: rendering
// a bridge-VLAN's ports that way produced VLAN filtering with no untagged
// membership and took the LAN down — after the apply had already been confirmed
// healthy. There is no error anywhere in that chain, which is why this is a
// separate field and a test rather than a convention.
func TestListOptionsAreListsNotJoinedStrings(t *testing.T) {
	site := model.Site{UUID: "abc", Networks: []model.Network{{
		ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Enabled: true,
	}}}
	doc, _, _ := Render(site, model.Device{ID: 1, Role: "gateway"}, netCaps(), vlanAware())
	for _, s := range doc.Sections {
		if _, wrong := s.Values["ports"]; wrong {
			t.Errorf("%s.%s renders ports as a plain option", s.Config, s.Name)
		}
	}
	// And the ops carry them through to the engine.
	for _, op := range doc.Plan(Existing{}).Ops {
		if op.Section == "oowrt_bv45" {
			if len(op.Lists["ports"]) != 4 {
				t.Errorf("op for %s carries %d list ports, want 4: %+v",
					op.Section, len(op.Lists["ports"]), op)
			}
		}
	}
}

// A section's hash must cover its lists, or a port-membership change looks
// identical to no change at all.
func TestHashCoversLists(t *testing.T) {
	a := Section{Config: "network", Type: "bridge-vlan", Name: "oowrt_bv45",
		Values: map[string]string{"vlan": "45"},
		Lists:  map[string][]string{"ports": {"lan1:t", "lan2:t"}}}
	b := a
	b.Lists = map[string][]string{"ports": {"lan1:t", "lan2:t", "lan3:t"}}
	if a.Hash() == b.Hash() {
		t.Error("two bridge-VLANs differing only in port membership hash the same")
	}
}

// A device already holding the list must count as matching, so a converged
// device does not report a change forever.
func TestPlanMatchesAgainstFlattenedLists(t *testing.T) {
	doc := Doc{Sections: []Section{{
		Config: "network", Type: "bridge-vlan", Name: "oowrt_bv45",
		Values: map[string]string{"vlan": "45", OwnershipTag: "1"},
		Lists:  map[string][]string{"ports": {"lan1:t", "lan2:t"}},
	}}}
	// reconcile.flatten space-joins a list, which is what the device returns.
	same := NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_bv45": {
			"vlan": "45", OwnershipTag: "1", "ports": "lan1:t lan2:t",
		}},
	})
	if ops := doc.Plan(same).Ops; len(ops) != 0 {
		t.Errorf("a matching bridge-VLAN produced %d op(s): %+v", len(ops), ops)
	}
	differs := NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_bv45": {
			"vlan": "45", OwnershipTag: "1", "ports": "lan1:t",
		}},
	})
	if ops := doc.Plan(differs).Ops; len(ops) != 1 {
		t.Errorf("a changed port list produced %d op(s), want 1", len(ops))
	}
}

// A role that does not publish WLANs gets none, even where the hardware could
// carry them and the site model asks for them.
//
// This is what the role is FOR. An old router repurposed as a switch almost
// always still has radios, and "has radios" is not "should be broadcasting".
// Before roles were a closed vocabulary this branch did not exist: a device
// adopted as a switch was an access point in every respect that mattered.
func TestASwitchIsSentNoWLANsEvenWithRadios(t *testing.T) {
	doc, rep, err := Render(testSite(),
		model.Device{ID: 7, Role: model.RoleSwitch}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sections {
		if s.Config == "wireless" {
			t.Errorf("a switch was sent %s.%s", s.Config, s.Name)
		}
	}
	if len(rep.Omissions) == 0 {
		t.Fatal("nothing explained why the WLANs are missing; a silently empty " +
			"plan is the defect this replaced")
	}
	// The message has to name both ways out, because either the role or the
	// group membership is wrong and the controller cannot tell which. Searched
	// rather than indexed: the network omissions come first and are unrelated.
	var msg string
	for _, om := range rep.Omissions {
		if strings.Contains(om.Reason, "role") {
			msg = om.Reason
		}
	}
	if msg == "" {
		t.Fatalf("no omission explains the role: %+v", rep.Omissions)
	}
	for _, want := range []string{"switch", "role", "AP group"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the omission %q does not mention %q", msg, want)
		}
	}
}

// And the roles that do publish still do.
func TestAnAccessPointStillGetsItsWLANs(t *testing.T) {
	for _, role := range []model.Role{model.RoleAP, model.RoleGateway, ""} {
		doc, _, err := Render(testSite(),
			model.Device{ID: 7, Role: role}, dualBandCaps(), Existing{})
		if err != nil {
			t.Fatal(err)
		}
		wireless := 0
		for _, s := range doc.Sections {
			if s.Config == "wireless" {
				wireless++
			}
		}
		if wireless == 0 {
			t.Errorf("role %q rendered no wireless sections", role)
		}
	}
}

// The other half of the stock-device bug. capability/probe_test.go pins that a
// device with no interfaces still reports its radios; this pins that the
// renderer will then actually give those radios a WLAN.
//
// Both halves were needed to break adoption and either alone would have hidden
// it: a radio with no interface has no frequency, so a renderer that keys only
// on frequency skips it and reports "device has no 5 GHz radio" — about a radio
// the probe just handed it. The device then never gets an interface, so the
// frequency never appears, so it is never configurable. Stock OpenWrt ships
// its radios disabled, which is to say this was every new router.
func TestRadiosWithNoFrequencyStillGetWLANs(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	// Frequency deliberately zero: iwinfo answers per-interface, and there are
	// none. The CONFIGURED band is all that is knowable, and it is enough.
	caps.Radios = []capability.Radio{
		{Device: "radio0", Phy: "phy0", Band: "5g"},
		{Device: "radio1", Phy: "phy1", Band: "2g"},
	}

	doc, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rep.HasConflicts() {
		t.Fatalf("unexpected conflicts: %v", rep.Conflicts)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("a dual-band device with disabled radios must still get a "+
			"WLAN on each band; got %d section(s): %+v", len(doc.Sections), doc.Sections)
	}
	r0, ok := sectionByName(doc, "oowrt_wlan3_radio0")
	if !ok {
		t.Fatal("no section for radio0")
	}
	r1, ok := sectionByName(doc, "oowrt_wlan3_radio1")
	if !ok {
		t.Fatal("no section for radio1")
	}
	if r0.Values["device"] != "radio0" || r1.Values["device"] != "radio1" {
		t.Errorf("radios not bound correctly: %q / %q",
			r0.Values["device"], r1.Values["device"])
	}
	// And nothing may be omitted for want of a radio. This is the assertion
	// that actually reproduces the reported symptom — "device has no 5g radio"
	// arrived as an omission, not as an error.
	for _, om := range rep.Omissions {
		if strings.Contains(strings.ToLower(om.Reason), "radio") {
			t.Errorf("renderer omitted %q for want of a radio it was given: %s",
				om.WLAN, om.Reason)
		}
	}
}

// marvellCaps is the reference WRT3200ACM: two radios whose driver has
// documented defects.
func marvellCaps() *capability.Registry {
	r := capability.NewRegistry()
	r.Class = capability.ClassA
	r.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5180, Hardware: "Marvell 88W8964"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 2412, Hardware: "Marvell 88W8964"},
	}
	return r
}

// The defect this whole registry exists for. OpenWrt's own page for this board
// says not to enable 802.11w because mwlwifi does not support it properly —
// and oonfeeWRT rendered ieee80211w=1 onto that board for weeks, because
// nothing in the controller knew. The device accepted it without complaint.
func TestKnownDriverDefectsAreWarnedBeforeTheyLand(t *testing.T) {
	site := testSite()
	// PSK2 with PMF optional: precisely what was on the reference device.
	site.WLANs[0].Security = model.Security{
		Mode: model.SecPSK2, Key: "s3cret", PMF: model.PMFOptional,
	}

	doc, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, marvellCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var pmf *Warning
	pmfCount := 0
	for i := range rep.Warnings {
		if rep.Warnings[i].DefectID == "mwlwifi-80211w-unsupported" {
			pmf = &rep.Warnings[i]
			pmfCount++
		}
	}
	if pmf == nil {
		t.Fatalf("no warning for 802.11w on a Marvell radio; got %+v", rep.Warnings)
	}
	// Once per WLAN, not once per radio. A WLAN fans out to every band the
	// device has, so an untreated defect match arrives twice for one SSID —
	// which reads as two problems and teaches an operator to skim.
	if pmfCount != 1 {
		t.Errorf("the same defect on the same WLAN was reported %d times", pmfCount)
	}
	if pmf.WLAN != "Home" {
		t.Errorf("the warning must name the WLAN that triggers it, got %q", pmf.WLAN)
	}
	// Measured, not documented — and the change of value is the point.
	//
	// It came from the device's OpenWrt page until 2026-08-17, when the defect
	// was reproduced on the reference hardware: PMF on, one forced 802.11r roam,
	// key installation failed, and 85 seconds later the firmware stopped
	// answering and took every radio with it (STATUS §5an). The warning an
	// operator sees has to carry that, because "we measured this" and "a wiki
	// says so" are not the same claim about their hardware.
	if pmf.Confidence != string(capability.ConfMeasuredHere) {
		t.Errorf("confidence %q; this one was reproduced on hardware", pmf.Confidence)
	}
	if pmf.Source == "" || pmf.Mitigation == "" {
		t.Error("a warning without a source or a mitigation is folklore")
	}

	// Crucially: the config is still rendered. The controller does not silently
	// downgrade what the operator asked for — it says so and applies it.
	if len(doc.Sections) == 0 {
		t.Fatal("the render was suppressed; warnings must not drop config")
	}
	for _, s := range doc.Sections {
		if s.Type == "wifi-iface" && s.Values["ieee80211w"] != "1" {
			t.Errorf("the operator's PMF setting was rewritten to %q; warn, "+
				"never rewrite", s.Values["ieee80211w"])
		}
	}
	// And it must not block the apply — a warning is not a conflict.
	if rep.HasConflicts() {
		t.Error("a driver defect must not abort the render")
	}

	// The unconditional hardware defect is reported against the DEVICE, not
	// against a WLAN, and exactly once however many SSIDs there are.
	hangs := 0
	for _, w := range rep.Warnings {
		if w.DefectID == "mwlwifi-firmware-hang" {
			hangs++
			if w.WLAN != "" {
				t.Errorf("a defect no configuration triggers must not be "+
					"attributed to WLAN %q", w.WLAN)
			}
		}
	}
	if hangs != 1 {
		t.Errorf("the firmware-hang defect should appear exactly once, got %d", hangs)
	}
}

// Hardware with no known defects must produce no warnings at all. A registry
// that warns about everything is a registry nobody reads.
func TestCleanHardwareGetsNoWarnings(t *testing.T) {
	caps := dualBandCaps()
	for i := range caps.Radios {
		caps.Radios[i].Hardware = "Qualcomm Atheros QCA9880"
	}
	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("warned about hardware with no registry entry: %+v", rep.Warnings)
	}
}

// A radio whose hardware string is unknown must not be assumed clean. This is
// the package's cardinal rule reaching the defect registry: not matching is
// "nothing was written down", never "this hardware is fine".
func TestUnknownHardwareMatchesNothingAndSaysSo(t *testing.T) {
	caps := dualBandCaps()
	for i := range caps.Radios {
		caps.Radios[i].Hardware = "" // a radio with no interface names nothing
	}
	if got := capability.DefectsFor(caps); len(got) != 0 {
		t.Errorf("matched %d defects against radios with no hardware string", len(got))
	}
	if capability.HardwareIdentified(caps) {
		t.Error("HardwareIdentified is true with no hardware anywhere")
	}

	// Matching nothing must not read as "no known defects". Stock OpenWrt
	// ships its default wifi-iface disabled, iwinfo only names a radio that has
	// an interface,
	// so this is precisely the freshly adopted device — the one whose operator
	// is choosing the security settings the registry exists to warn about.
	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var said bool
	for _, w := range rep.Warnings {
		if w.DefectID == "hardware-unidentified" {
			said = true
		}
	}
	if !said {
		t.Error("a device whose radios could not be identified got no warning " +
			"at all; that is a clean bill of health from a check that never ran")
	}
}

// A radio switched off swallows the WLAN silently.
//
// The section we write is correct, the apply succeeds, and nothing broadcasts —
// then the health check fails looking for an SSID that was never going to
// appear, for a reason no screen could explain. Nothing in the controller read
// a radio's disabled flag at all, in any source.
func TestAWLANOnASwitchedOffRadioIsWarnedAbout(t *testing.T) {
	existing := NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio0": {".type": "wifi-device", "disabled": "1", "band": "5g"},
			"radio1": {".type": "wifi-device", "band": "2g"}, // absent = enabled
		},
	})

	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, dualBandCaps(), existing)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var off []string
	for _, w := range rep.Warnings {
		if w.DefectID == "radio-disabled" {
			off = append(off, w.Summary)
		}
	}
	if len(off) != 1 {
		t.Fatalf("want exactly one disabled-radio warning (radio0), got %d: %v",
			len(off), off)
	}
	if !strings.Contains(off[0], "radio0") {
		t.Errorf("the warning does not name the radio: %q", off[0])
	}
	// radio1 has no `disabled` option at all, which is UCI's default for
	// enabled. Warning about it would send an operator to fix a working radio.
	if strings.Contains(off[0], "radio1") {
		t.Error("a radio with no disabled option was reported as switched off")
	}
	// And the config is still written: the controller does not silently drop a
	// WLAN, it says what will happen.
	if rep.HasConflicts() {
		t.Error("a disabled radio must not abort the render")
	}
}

// A radio the renderer knows nothing about must not be accused of being off.
// The warning tells an operator to go and change something on their device.
func TestAnUnknownRadioIsNotCalledDisabled(t *testing.T) {
	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"},
		dualBandCaps(), Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, w := range rep.Warnings {
		if w.DefectID == "radio-disabled" {
			t.Errorf("no wireless config was readable, yet a radio was reported "+
				"switched off: %s", w.Summary)
		}
	}
}

// A defect belongs to the radio whose driver has it, not to the device.
//
// The warning list was device-wide, so a WLAN rendered onto an Atheros radio
// was accused of a Marvell driver's defects. The DFS case is the cleanest: a
// 2.4 GHz Marvell radio cannot be on a DFS channel at any value, and the
// warning fired anyway — sourced from the 5 GHz Atheros radio beside it.
func TestADefectDoesNotFollowTheDeviceOntoAnotherVendorsRadio(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 2412, Channel: 6,
			Hardware: "Marvell 88W8964"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 5500, Channel: 100,
			Hardware: "Qualcomm Atheros QCA9880"},
	}

	site := testSite()
	site.WLANs[0].Bands = []model.Band{model.Band5G}
	site.WLANs[0].Security = model.Security{
		Mode: model.SecSAE, Key: "s3cret", PMF: model.PMFRequired,
	}

	_, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, w := range rep.Warnings {
		switch w.DefectID {
		case "mwlwifi-dfs-channels":
			t.Errorf("the only Marvell radio here is 2.4 GHz and cannot be on a " +
				"DFS channel; the warning came from the Atheros radio's channel")
		case "mwlwifi-wpa3-unsupported", "mwlwifi-80211w-unsupported":
			t.Errorf("a WLAN on the Atheros radio was accused of a Marvell "+
				"driver defect: %s", w.DefectID)
		}
	}
}

// But a radio that has not said what it is must NOT silence a defect.
//
// This is the trap in the obvious fix. On a homogeneous Marvell board whose
// second radio has no interface, that radio's Hardware is "" — the same §5ab
// case HardwareIdentified exists for — and a plain per-radio filter would go
// quiet on the reference device. Warning about the wrong chip is noise; going
// silent about the right one is the cardinal error.
func TestAnUnidentifiedRadioDoesNotSilenceADefect(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5180, Hardware: "Marvell 88W8964"},
		{Device: "phy1-ap0", Phy: "phy1", Band: "2g"}, // no interface, so no hardware
	}
	site := testSite()
	site.WLANs[0].Bands = []model.Band{model.Band2G}
	site.WLANs[0].Security = model.Security{
		Mode: model.SecPSK2, Key: "s3cret", PMF: model.PMFOptional,
	}

	_, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var warned bool
	for _, w := range rep.Warnings {
		if w.DefectID == "mwlwifi-80211w-unsupported" {
			warned = true
		}
	}
	if !warned {
		t.Error("a radio that did not identify itself silenced a defect the " +
			"device demonstrably has; unidentified is not 'a different chip'")
	}
}

// A channel-keyed defect must judge the channel the radio is on NOW.
//
// capability.Radio.Channel is frozen at adoption, and the controller does not
// manage channels — so the value can only change behind its back. It then fails
// both ways: silent when an operator moves a radio onto a DFS channel after
// adoption, and crying wolf forever after they move it off.
func TestADFSWarningFollowsTheLiveChannelNotTheAdoptionSnapshot(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5180, Channel: 36,
			Hardware: "Marvell 88W8964"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 2412, Channel: 6,
			Hardware: "Marvell 88W8964"},
	}

	// Snapshot says 36. The device has since been moved to 100, which is DFS.
	moved := NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio0": {".type": "wifi-device", "channel": "100"},
			"radio1": {".type": "wifi-device", "channel": "6"},
		},
	})
	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, moved)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var fired bool
	for _, w := range rep.Warnings {
		if w.DefectID == "mwlwifi-dfs-channels" {
			fired = true
		}
	}
	if !fired {
		t.Error("the radio is on channel 100 now; the warning judged the " +
			"adoption snapshot and stayed silent")
	}

	// The other direction, and it needs its own registry: adopted ON a DFS
	// channel and moved off it since. Reusing the snapshot-36 caps above would
	// assert something true under every possible implementation, including one
	// that ignores the live channel entirely — which is what the first version
	// of this half did.
	adoptedOnDFS := capability.NewRegistry()
	adoptedOnDFS.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5500, Channel: 100,
			Hardware: "Marvell 88W8964"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 2412, Channel: 6,
			Hardware: "Marvell 88W8964"},
	}
	stayed := NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio0": {".type": "wifi-device", "channel": "36"},
			"radio1": {".type": "wifi-device", "channel": "6"},
		},
	})
	_, rep2, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, adoptedOnDFS, stayed)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, w := range rep2.Warnings {
		if w.DefectID == "mwlwifi-dfs-channels" {
			t.Error("the radio was adopted on channel 100 and is on 36 now; the " +
				"warning is crying wolf from the adoption snapshot")
		}
	}
}

// `channel auto` has no number in UCI, and the snapshot's reading is then the
// only evidence of what the radio actually picked. Dropping it would go silent
// on an ACS-selected DFS channel, which is the case the warning is most for.
func TestAutoChannelFallsBackToTheObservedChannel(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5500, Channel: 100,
			Hardware: "Marvell 88W8964"},
	}
	auto := NewExisting(map[string]map[string]map[string]string{
		"wireless": {"radio0": {".type": "wifi-device", "channel": "auto"}},
	})
	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, auto)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var fired bool
	for _, w := range rep.Warnings {
		if w.DefectID == "mwlwifi-dfs-channels" {
			fired = true
		}
	}
	if !fired {
		t.Error("channel is `auto` in UCI and the radio was observed on 100; " +
			"the unparseable value overwrote the only evidence there was")
	}
}

// PMF must be constrained by what the security mode can actually carry.
//
// Every new WLAN is created with pmf="1", and the editor hides the control for
// modes that cannot use it — so a WLAN switched to Open keeps a setting nobody
// chose and nobody can clear. It was then rendered onto the device, where it is
// meaningless without RSN and, on a Marvell radio, triggered a driver warning
// the operator had no control to act on.
func TestPMFIsConstrainedByTheSecurityMode(t *testing.T) {
	cases := []struct {
		mode model.SecurityMode
		pmf  model.PMF
		want string
		why  string
	}{
		// An open network has no RSN, so PMF is meaningless there — but the
		// option is written as an explicit 0 rather than omitted, because a
		// WLAN switched from WPA2 to Open would otherwise keep whatever
		// ieee80211w the last apply left on the device. Zero does not trip the
		// mwlwifi trigger, which fires on != "" && != "0".
		{model.SecNone, model.PMFOptional, "0",
			"an open network has no RSN, so PMF must be off — and said to be off"},
		{model.SecSAE, model.PMFDisabled, "2",
			"WPA3 mandates PMF; disabled produces an AP clients reject"},
		{model.SecOWE, model.PMFDisabled, "2",
			"OWE mandates PMF just as SAE does"},
		{model.SecSAEMixed, model.PMFDisabled, "1",
			"disabling PMF on a transitional network silently removes its WPA3 half"},
		{model.SecSAEMixed, model.PMFRequired, "2",
			"an explicit choice above the floor is kept"},
		{model.SecPSK2, model.PMFDisabled, "0",
			"WPA2 may legitimately run without PMF, which is the mwlwifi mitigation"},
	}
	for _, c := range cases {
		t.Run(string(c.mode)+"/"+string(c.pmf), func(t *testing.T) {
			site := testSite()
			site.WLANs[0].Security = model.Security{Mode: c.mode, Key: "s3cret", PMF: c.pmf}
			site.WLANs[0].Roaming = model.Roaming{}
			doc, _, err := Render(site, model.Device{ID: 7, Role: "ap"}, dualBandCaps(), Existing{})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if len(doc.Sections) == 0 {
				t.Fatal("nothing rendered")
			}
			got, present := doc.Sections[0].Values["ieee80211w"]
			if !present {
				t.Fatalf("ieee80211w not written at all for %s/%s — an omitted "+
					"option leaves whatever the last apply put on the device",
					c.mode, c.pmf)
			}
			if got != c.want {
				t.Errorf("ieee80211w=%q, want %q for %s/%s — %s",
					got, c.want, c.mode, c.pmf, c.why)
			}
		})
	}
}

// The "could not be checked" warning must fire when the RADIO LIST itself is
// missing, not only when radios exist and cannot be named.
//
// probeRadios returns early with no radios and the wireless features
// NotObservable when iwinfo.devices is refused. Gating the warning on there
// being radios made it silent in the case where the least is known — the same
// collapse of "could not ask" into "nothing there" the warning exists for.
func TestAnUnreadableRadioListAlsoSaysTheCheckDidNotRun(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Set(capability.FeatSurvey, capability.NotObservable) // iwinfo refused
	// No radios at all: the list itself could not be read.

	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var said bool
	for _, w := range rep.Warnings {
		if w.DefectID == "hardware-unidentified" {
			said = true
		}
	}
	if !said {
		t.Error("a device whose radio list could not be read got no warning; " +
			"that is a clean bill of health from a check that never ran")
	}
}

// But a device that genuinely HAS no radios — asked, and there are none — must
// not be warned about. That is a real answer, and roleFit already reports it.
func TestADeviceWithNoRadiosIsNotWarnedAboutUnreadableHardware(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Set(capability.FeatSurvey, capability.Absent) // asked; there are none

	_, rep, err := Render(testSite(), model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, w := range rep.Warnings {
		if w.DefectID == "hardware-unidentified" {
			t.Error("a device that answered 'no radios' was told the check " +
				"could not run; asked-and-none is an answer")
		}
	}
}

// iwinfo answers with a placeholder when it cannot name a part, and a
// placeholder is not an identification.
//
// "Generic MAC80211" is what the lab's own Archer C6 reports for one of its two
// radios. Treating it as "a different chip" dropped every Marvell defect for
// that radio on a device the probe had already established was Marvell —
// silence about a radio-death defect, from a name that means "I do not know".
func TestAPlaceholderHardwareNameDoesNotSilenceADefect(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5180, Hardware: "Marvell 88W8964"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 2412, Hardware: "Generic MAC80211"},
	}
	site := testSite()
	site.WLANs[0].Bands = []model.Band{model.Band2G} // lands on the placeholder radio
	site.WLANs[0].Security = model.Security{
		Mode: model.SecPSK2, Key: "s3cret", PMF: model.PMFOptional,
	}

	_, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var warned bool
	for _, w := range rep.Warnings {
		if w.DefectID == "mwlwifi-80211w-unsupported" {
			warned = true
		}
	}
	if !warned {
		t.Error("a radio named \"Generic MAC80211\" silenced a defect on a " +
			"device already established as Marvell; a placeholder is not an " +
			"identification")
	}
}
