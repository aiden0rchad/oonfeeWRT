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
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 5180},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 2412},
	}
	return r
}

func singleBandCaps() *capability.Registry {
	r := capability.NewRegistry()
	r.Radios = []capability.Radio{{Device: "phy0-ap0", Phy: "phy0", Frequency: 2412}}
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
	existing := Existing{WifiIfaces: map[string]map[string]string{
		"oowrt_wlan3_radio0": {"ssid": "SomethingElse", "device": "radio0"},
	}}
	_, rep, err := Render(testSite(), model.Device{ID: 7}, dualBandCaps(), existing)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !rep.HasConflicts() {
		t.Fatal("a section with our name that is not ours must be a conflict")
	}
	if !strings.Contains(rep.Conflicts[0].Reason, "not ours") {
		t.Errorf("the conflict should say why: %q", rep.Conflicts[0].Reason)
	}
}

func TestForeignSSIDOnTheSameRadioIsAConflict(t *testing.T) {
	existing := Existing{WifiIfaces: map[string]map[string]string{
		"default_radio0": {"ssid": "Home", "device": "radio0"}, // human-made
	}}
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
	existing := Existing{WifiIfaces: map[string]map[string]string{
		"oowrt_wlan3_radio0": {"ssid": "Home", "device": "radio0", OwnershipTag: "1"},
	}}
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
	for _, s := range doc.Sections {
		if s.Values["ieee80211r"] != "" {
			t.Errorf("802.11r must not render on WPA2-PSK without the opt-in: %v", s.Values)
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
	existing := Existing{WifiIfaces: map[string]map[string]string{
		"oowrt_wlan3_radio0": {OwnershipTag: "1"},
		"oowrt_wlan3_radio1": {OwnershipTag: "1"},
	}}
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
	existing := Existing{WifiIfaces: map[string]map[string]string{
		"oowrt_wlan3_radio0": {OwnershipTag: "1"},
		"oowrt_wlan3_radio1": {OwnershipTag: "1"},
		"oowrt_wlan9_radio0": {OwnershipTag: "1"},   // ours, no longer wanted
		"default_radio0":     {"ssid": "TheirWifi"}, // a human's
		"guest_ap":           {"ssid": "Guest"},     // also a human's
	}}
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
