package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// The state capability.probeRadios records when the radio list is refused: no
// radios, and the wireless features NotObservable rather than Absent.
//
// Both of its early returns produce exactly this — iwinfo.devices denied, and
// getWirelessDevices failing with iwinfo listing nothing to fall back on — so
// it is a real stored CapsJSON, not a constructed one.
func radiosRefused() *capability.Registry {
	r := capability.NewRegistry()
	r.Set(capability.FeatSurvey, capability.NotObservable)
	r.Set(capability.FeatAirtimeSplit, capability.NotObservable)
	r.Set(capability.FeatHostapdControl, capability.NotObservable)
	return r
}

// A device that genuinely has no radios: the list was READ, and it was empty.
// The same empty radio map as above, arrived at by the opposite route.
func noRadios() *capability.Registry {
	r := capability.NewRegistry()
	r.Set(capability.FeatSurvey, capability.Absent)
	return r
}

func wirelessSite() model.Site {
	return model.Site{
		UUID:     "site-uuid",
		Networks: []model.Network{{ID: 1, Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}},
		Groups:   []model.APGroup{{ID: 1, Name: "all", DeviceIDs: []int64{1}}},
		WLANs: []model.WLAN{{ID: 1, SSID: "home", NetworkID: 1, GroupID: 1,
			Bands: []model.Band{model.Band2G}, Enabled: true,
			Security: model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"}}},
	}
}

func ourWireless() Existing {
	return NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio0":             {".type": "wifi-device", "band": "2g"},
			"radio1":             {".type": "wifi-device", "band": "5g"},
			"oowrt_wlan1_radio0": {".type": "wifi-iface", OwnershipTag: "1", "ssid": "home", "device": "radio0"},
			"oowrt_up1_radio1":   {".type": "wifi-iface", OwnershipTag: "1", "mode": "sta", "device": "radio1"},
		},
	})
}

func deleteOps(t *testing.T, doc Doc, existing Existing) []string {
	t.Helper()
	var out []string
	for _, op := range doc.Prune(existing) {
		out = append(out, op.Config+"."+op.Section)
	}
	return out
}

// A refused radio list must not delete the interfaces we own.
//
// This is the cardinal error at the point of deletion: Render produces no
// wireless sections because it could not read the radios, and a Prune that
// only knows "not in the document" reads that as "the operator removed them".
// The apply then succeeds, and the device — including the wireless uplink that
// may be its only path to the network — goes off the air.
func TestRefusedRadioListPrunesNothing(t *testing.T) {
	existing := ourWireless()
	doc, rep, err := Render(wirelessSite(), model.Device{ID: 1, Role: model.RoleAP},
		radiosRefused(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 0 {
		t.Errorf("deleted %v on a device whose radio list could not be read", got)
	}
	// And it must SAY so. Silence here is a preview reporting "no changes" for
	// a device whose config the controller can no longer account for.
	var kept string
	for _, o := range rep.Omissions {
		if o.Kind == KindUndetermined && strings.Contains(o.Reason, "left exactly as they are") {
			kept = o.Reason
		}
	}
	if kept == "" {
		t.Fatal("nothing told the operator that owned sections survived only " +
			"because the render could not see the device")
	}
	for _, want := range []string{"oowrt_wlan1_radio0", "oowrt_up1_radio1"} {
		if !strings.Contains(kept, want) {
			t.Errorf("the kept-sections message does not name %s: %q", want, kept)
		}
	}
}

// The other half, and the half that keeps the fix honest: a device that really
// has no radios must still be pruned. Retaining everything would be a fix that
// disabled the feature.
func TestGenuinelyRadiolessDevicePrunesNormally(t *testing.T) {
	existing := ourWireless()
	doc, _, err := Render(wirelessSite(), model.Device{ID: 1, Role: model.RoleAP},
		noRadios(), existing)
	if err != nil {
		t.Fatal(err)
	}
	got := deleteOps(t, doc, existing)
	if len(got) != 2 {
		t.Errorf("a device whose radio list was READ and was empty should have "+
			"its stale sections pruned; got %v", got)
	}
}

// A device the operator took out of every AP group must still be pruned, with
// its radios perfectly readable. This is what Prune is FOR, and it is the
// behaviour the fix must not cost.
func TestDeviceRemovedFromGroupStillPrunes(t *testing.T) {
	site := wirelessSite()
	site.Groups[0].DeviceIDs = nil // no longer a member of anything
	existing := ourWireless()
	doc, _, err := Render(site, model.Device{ID: 1, Role: model.RoleAP}, dualBandCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 2 {
		t.Errorf("a device in no AP group should have its sections pruned; got %v", got)
	}
}

// The messages an operator reads must not claim the radio is absent when the
// truth is that the question was never answered. tools/probe.py made exactly
// this claim about DSA and sent the reader to the wrong place.
func TestRefusedRadioListDoesNotClaimTheRadioIsAbsent(t *testing.T) {
	_, rep, err := Render(wirelessSite(), model.Device{ID: 1, Role: model.RoleAP},
		radiosRefused(), ourWireless())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range rep.Omissions {
		if strings.Contains(o.Reason, "device has no") {
			t.Errorf("claimed absence from a refused call: %q", o.Reason)
		}
	}
	// Absence is still stated plainly when it is actually known.
	_, rep, err = Render(wirelessSite(), model.Device{ID: 1, Role: model.RoleAP},
		noRadios(), ourWireless())
	if err != nil {
		t.Fatal(err)
	}
	var said bool
	for _, o := range rep.Omissions {
		if strings.Contains(o.Reason, "device has no 2g radio") {
			said = true
		}
	}
	if !said {
		t.Error("a device known to have no 2.4 GHz radio should say so plainly")
	}
}

// A feature gate that could not decide must not delete the interface either.
//
// Distinct from the blind-config case above: here the radios are perfectly
// readable, so the section NAME is known — what could not be read is the
// package list that says whether the device supports mesh or a wireless
// uplink. Both gates return NotObservable for that, both render nothing, and
// both were pruned. The uplink is the sharp one: it is the device's only path
// to the network, so deleting it on the strength of a check that did not run
// takes the device off the air and the controller with it.
func TestUndecidedFeatureGateKeepsTheInterface(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 2412, Hardware: "Generic MAC80211"},
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 5180, Hardware: "Generic MAC80211"},
	}
	caps.Set(capability.FeatSurvey, capability.Present)
	// The package list could not be read, so neither question has an answer.
	caps.Set(capability.FeatMesh, capability.NotObservable)
	caps.Set(capability.FeatWirelessUplink, capability.NotObservable)

	site := wirelessSite()
	site.WLANs[0].Options.AllowUplink = true
	// The uplink joins a WLAN this device does NOT publish — a device cannot
	// bridge to a network it serves itself (Uplink.Validate).
	site.Groups = append(site.Groups, model.APGroup{ID: 2, Name: "others", DeviceIDs: []int64{2}})
	site.WLANs = append(site.WLANs, model.WLAN{
		ID: 2, SSID: "backhaul", NetworkID: 1, GroupID: 2, Enabled: true,
		Bands:    []model.Band{model.Band5G},
		Options:  model.WLANOptions{AllowUplink: true},
		Security: model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"},
	})
	site.Uplinks = []model.Uplink{{ID: 1, DeviceID: 1, WLANID: 2, Band: model.Band5G, Enabled: true}}
	site.Meshes = []model.Mesh{{ID: 1, MeshID: "bh", NetworkID: 1, GroupID: 1,
		Band: model.Band5G, Key: "not-a-real-key", Enabled: true}}

	existing := NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio0":             {".type": "wifi-device", "band": "2g"},
			"radio1":             {".type": "wifi-device", "band": "5g"},
			"oowrt_wlan1_radio0": {".type": "wifi-iface", OwnershipTag: "1", "ssid": "home", "device": "radio0"},
			"oowrt_mesh1_radio1": {".type": "wifi-iface", OwnershipTag: "1", "mesh_id": "bh", "device": "radio1"},
			"oowrt_up1_radio1":   {".type": "wifi-iface", OwnershipTag: "1", "mode": "sta", "device": "radio1"},
		},
	})
	doc, rep, err := Render(site, model.Device{ID: 1, Role: model.RoleAP}, caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range deleteOps(t, doc, existing) {
		if got == "wireless.oowrt_mesh1_radio1" || got == "wireless.oowrt_up1_radio1" {
			t.Errorf("deleted %s because a feature gate could not decide", got)
		}
	}
	// The WLAN itself still renders — the radios were readable — so this is
	// not the blind-config path passing under another name.
	if len(doc.Sections) == 0 {
		t.Fatal("nothing rendered: the radios were readable and a WLAN targets " +
			"this device, so this test is not exercising the gate path")
	}
	kept := map[string]bool{}
	for _, o := range rep.Omissions {
		if o.Kind == KindUndetermined {
			kept[o.Reason] = true
		}
	}
	var sawMesh, sawUplink bool
	for r := range kept {
		if strings.Contains(r, "mesh section for bh") {
			sawMesh = true
		}
		if strings.Contains(r, "wireless uplink section for backhaul") {
			sawUplink = true
		}
	}
	if !sawMesh || !sawUplink {
		t.Errorf("the operator was not told which interfaces were kept "+
			"(mesh=%v uplink=%v): %v", sawMesh, sawUplink, kept)
	}
}

// The counterpart to TestUndecidedFeatureGateKeepsTheInterface, and the one
// that stops the fix from quietly becoming "never delete anything".
//
// A gate that decided AGAINST the device — the driver will not run a mesh
// point, the supplicant is not installed — is a decision, and the stale
// section must go. Absent and NotObservable render identically; only the
// second may keep the section alive.
func TestGateThatDecidedAgainstTheDeviceStillPrunes(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 5180, Hardware: "Generic MAC80211"},
	}
	caps.Set(capability.FeatSurvey, capability.Present)
	caps.Set(capability.FeatMesh, capability.Absent) // read, and the answer is no

	site := wirelessSite()
	site.WLANs = nil
	site.Meshes = []model.Mesh{{ID: 1, MeshID: "bh", NetworkID: 1, GroupID: 1,
		Band: model.Band5G, Key: "not-a-real-key", Enabled: true}}
	existing := NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio1":             {".type": "wifi-device", "band": "5g"},
			"oowrt_mesh1_radio1": {".type": "wifi-iface", OwnershipTag: "1", "mesh_id": "bh", "device": "radio1"},
		},
	})
	doc, _, err := Render(site, model.Device{ID: 1, Role: model.RoleAP}, caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	got := deleteOps(t, doc, existing)
	if len(got) != 1 || got[0] != "wireless.oowrt_mesh1_radio1" {
		t.Errorf("a device known not to support mesh should have its stale mesh "+
			"section pruned; got %v", got)
	}
}

func gatewaySite() model.Site {
	return model.Site{
		UUID: "site-uuid",
		Networks: []model.Network{
			{ID: 1, Name: "iot", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "iot", Enabled: true},
		},
	}
}

// The wired half of the same error.
//
// A device that did not report its port layout renders no VLAN, no addressing,
// no DHCP and no firewall zone — which reaches Prune looking exactly like an
// operator who deleted the network. Deleting a gateway's addressed interface
// and its DHCP server because a capability read came back empty is the same
// failure as the wireless one, on the config that carries the controller's own
// route to the device.
func TestUnreadablePortLayoutPrunesNothing(t *testing.T) {
	caps := capability.NewRegistry() // no Ports at all
	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {
			"their_bv1":     {".type": "bridge-vlan", "device": "br-lan", "vlan": "1"},
			"oowrt_net_iot": {".type": "interface", OwnershipTag: "1", "proto": "static"},
		},
		"dhcp":     {"oowrt_dhcp_iot": {".type": "dhcp", OwnershipTag: "1"}},
		"firewall": {"oowrt_zone_iot": {".type": "zone", OwnershipTag: "1"}},
	})
	doc, rep, err := Render(gatewaySite(), model.Device{ID: 1, Role: model.RoleGateway},
		caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 0 {
		t.Errorf("deleted %v on a device whose port layout could not be read", got)
	}
	var told bool
	for _, o := range rep.Omissions {
		if o.Kind == KindUndetermined && strings.Contains(o.Reason, "oowrt_net_iot") {
			told = true
		}
	}
	if !told {
		t.Error("the operator was not told the wired sections were kept rather " +
			"than removed")
	}
}

// And its counterpart: a port layout we DID read, on a bridge the operator has
// not made VLAN-aware, is a decision about config we can see. Those sections
// are still pruned — otherwise turning VLAN filtering back off would strand
// our config on the device forever.
func TestReadablePortLayoutOnAPlainBridgeStillPrunes(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2"}}
	existing := NewExisting(map[string]map[string]map[string]string{
		// No bridge-vlan: filtering is off, which we read rather than guessed.
		"network": {"oowrt_net_iot": {".type": "interface", OwnershipTag: "1"}},
	})
	doc, _, err := Render(gatewaySite(), model.Device{ID: 1, Role: model.RoleGateway},
		caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 1 {
		t.Errorf("a bridge we read and found unfiltered is a decision; stale "+
			"sections should still be pruned, got %v", got)
	}
}

// A render with no capability record at all must behave like a render that
// knows nothing — not panic, and not delete.
//
// radiosByBand, radioBySection and withLiveChannels all check for nil;
// renderNetwork and bridgeIsVLANAware did not, so the first VLAN network took
// the whole render down. Not reachable from the daemon, which never hands over
// a nil record without an error, but a contract that half the package honours
// is not a contract — and the half that ignored it is the half that writes.
func TestNilCapabilityRecordIsNothingKnown(t *testing.T) {
	existing := NewExisting(map[string]map[string]map[string]string{
		"wireless": {"oowrt_wlan1_radio0": {".type": "wifi-iface", OwnershipTag: "1"}},
		"network":  {"oowrt_net_iot": {".type": "interface", OwnershipTag: "1"}},
	})
	site := gatewaySite()
	site.Groups = []model.APGroup{{ID: 1, Name: "all", DeviceIDs: []int64{1}}}
	site.WLANs = []model.WLAN{{ID: 1, SSID: "home", NetworkID: 1, GroupID: 1,
		Bands: []model.Band{model.Band2G}, Enabled: true,
		Security: model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"}}}

	doc, _, err := Render(site, model.Device{ID: 1, Role: model.RoleGateway}, nil, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 0 {
		t.Errorf("deleted %v with no capability record to decide from", got)
	}
}

// A capability record with NO entry for a feature must not be reported as the
// device lacking it.
//
// Unknown is the zero value, and a capability record is JSON: a record written
// before a Feature existed simply has no key for it. So every device adopted
// before a feature was added reads Unknown for it, permanently, until
// re-probed — which makes this reachable across the whole existing fleet the
// moment any new Feature is added.
//
// The gates switched on NotObservable alone, so Unknown fell through to the
// Absent branch and produced a definite claim about the hardware, complete
// with a package to install. §5q is explicit that telling someone to install a
// package they already have is worse than saying nothing.
func TestUnknownFeatureIsNotReportedAsAbsent(t *testing.T) {
	unprobed := capability.NewRegistry() // every feature Unknown

	ok, why := MeshGate(unprobed)
	if ok {
		t.Fatal("an unprobed device was allowed to render a mesh")
	}
	for _, forbidden := range []string{"does not carry", "Installing a wpad"} {
		if strings.Contains(why, forbidden) {
			t.Errorf("MeshGate claims the device lacks mesh from a check that "+
				"never ran: %q", why)
		}
	}

	ok, why = UplinkGate(unprobed)
	if ok {
		t.Fatal("an unprobed device was allowed to render an uplink")
	}
	for _, forbidden := range []string{"has no wireless supplicant", "Installing a wpad"} {
		if strings.Contains(why, forbidden) {
			t.Errorf("UplinkGate claims the device lacks a supplicant from a "+
				"check that never ran: %q", why)
		}
	}

	// A device that really was checked still gets the plain answer, or this is
	// just a way of never saying anything.
	decided := capability.NewRegistry()
	decided.Set(capability.FeatMesh, capability.Absent)
	decided.Set(capability.FeatWirelessUplink, capability.Absent)
	if _, why := MeshGate(decided); !strings.Contains(why, "does not carry") {
		t.Errorf("a device known to lack mesh should say so plainly: %q", why)
	}
	if _, why := UplinkGate(decided); !strings.Contains(why, "no wireless supplicant") {
		t.Errorf("a device known to lack a supplicant should say so: %q", why)
	}
}

// And the same state must protect the interface from Prune, for the same
// reason it protects a NotObservable one: neither is a decision.
func TestUnknownFeatureAlsoKeepsTheInterface(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Device: "phy1-ap0", Phy: "phy1", Frequency: 5180, Hardware: "Generic MAC80211"},
	}
	caps.Set(capability.FeatSurvey, capability.Present)
	// FeatMesh deliberately never set: the record has no entry for it.

	site := wirelessSite()
	site.WLANs = nil
	site.Meshes = []model.Mesh{{ID: 1, MeshID: "bh", NetworkID: 1, GroupID: 1,
		Band: model.Band5G, Key: "not-a-real-key", Enabled: true}}
	existing := NewExisting(map[string]map[string]map[string]string{
		"wireless": {
			"radio1":             {".type": "wifi-device", "band": "5g"},
			"oowrt_mesh1_radio1": {".type": "wifi-iface", OwnershipTag: "1", "mesh_id": "bh"},
		},
	})
	doc, _, err := Render(site, model.Device{ID: 1, Role: model.RoleAP}, caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 0 {
		t.Errorf("deleted %v because the record had no entry for mesh support", got)
	}
}

// A board that reports a bridge and no switch ports has ANSWERED.
//
// probePorts fails by leaving Bridge empty. It sets Bridge from lan.Device,
// with no LAN ports, for a board whose LAN is a single interface rather than a
// set of individually taggable ports — a successful read of a real layout.
//
// Treating that as blindness disabled Prune across every such board, and told
// the operator the device "did not report its wired port layout" when it had.
// Measured on the reference Archer C6: bridge eth0.1, no LAN ports, DSA Absent.
func TestBoardWithNoTaggablePortsIsAnAnswerNotABlindSpot(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Ports = capability.Ports{Bridge: "eth0.1", WAN: "eth0.2"} // LAN nil
	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_net_iot": {".type": "interface", OwnershipTag: "1"}},
	})
	doc, rep, err := Render(gatewaySite(), model.Device{ID: 1, Role: model.RoleGateway},
		caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	if doc.blind("network") {
		t.Error("a board that reported its layout was treated as unreadable, " +
			"which disables pruning on every swconfig board")
	}
	if got := deleteOps(t, doc, existing); len(got) != 1 {
		t.Errorf("stale sections should still be pruned here, got %v", got)
	}
	// Keyed on the sentence that only this branch produces. The bridge NAME
	// appears in the VLAN-prerequisite message too, so matching on "eth0.1"
	// alone passed with this branch deleted.
	var said bool
	for _, o := range rep.Omissions {
		if strings.Contains(o.Reason, "single interface") &&
			strings.Contains(o.Reason, "eth0.1") && o.Kind != KindUndetermined {
			said = true
		}
		if strings.Contains(o.Reason, "did not report its wired port layout") {
			t.Errorf("claimed the read failed on a board that answered: %q", o.Reason)
		}
	}
	if !said {
		t.Error("the operator was not told that this board has no individually " +
			"taggable ports, which is a different problem from an unreadable one")
	}
}

// And the genuinely unreadable case keeps its blindness: an empty bridge is
// probePorts' only failure signal.
func TestUnreadableBoardDescriptionIsStillABlindSpot(t *testing.T) {
	caps := capability.NewRegistry() // Ports entirely zero
	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_net_iot": {".type": "interface", OwnershipTag: "1"}},
	})
	doc, rep, err := Render(gatewaySite(), model.Device{ID: 1, Role: model.RoleGateway},
		caps, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleteOps(t, doc, existing); len(got) != 0 {
		t.Errorf("deleted %v with no board description to decide from", got)
	}
	var undetermined bool
	for _, o := range rep.Omissions {
		if o.Kind == KindUndetermined && strings.Contains(o.Reason, "did not report") {
			undetermined = true
		}
	}
	if !undetermined {
		t.Error("an unreadable board description was not reported as undetermined")
	}
}

// The hardware-unidentified warning has two causes and only ever gave the
// remedy for one of them.
//
// "Apply a WLAN and re-probe" works when radios ARE listed but unnamed:
// radiosByBand falls back to the configured band, a wifi-iface renders, the
// apply puts the radio on the air, and iwinfo then names it.
//
// It is impossible when the radio LIST could not be read. The same condition
// makes radiosByBand return an empty map, every band lookup misses, and no
// wifi-iface is rendered at all — so there is no WLAN to apply, and re-probing
// reads the same refused list. STATUS §6 records this string as the lesson
// about advice that cannot work; §5as fixed the deletion it caused and left
// the sentence standing.
func TestTheUnidentifiedHardwareRemedyMatchesWhyItFired(t *testing.T) {
	fix := func(caps *capability.Registry) string {
		_, rep, err := Render(wirelessSite(), model.Device{ID: 1, Role: model.RoleAP},
			caps, ourWireless())
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range rep.Warnings {
			if w.DefectID == "hardware-unidentified" {
				return w.Mitigation
			}
		}
		t.Fatal("the hardware-unidentified warning did not fire")
		return ""
	}

	// Radios listed, none named: applying really does settle it.
	named := capability.NewRegistry()
	named.Set(capability.FeatSurvey, capability.Present)
	named.Radios = []capability.Radio{
		{Device: "phy0-ap0", Phy: "phy0", Frequency: 2412}, // no Hardware
	}
	if got := fix(named); !strings.Contains(got, "Apply a WLAN and re-probe") {
		t.Errorf("a radio that only lacks a NAME should still be told to apply "+
			"and re-probe, which works: %q", got)
	}

	// The list itself was refused: applying is not possible.
	refused := fix(radiosRefused())
	if strings.Contains(refused, "Apply a WLAN and re-probe") {
		t.Errorf("told the operator to apply a WLAN on a device where no WLAN "+
			"can be rendered, so there is nothing to apply: %q", refused)
	}
	for _, want := range []string{"Re-adopt", "access-control"} {
		if !strings.Contains(refused, want) {
			t.Errorf("the remedy for a refused radio list does not mention %q: %q",
				want, refused)
		}
	}
}
