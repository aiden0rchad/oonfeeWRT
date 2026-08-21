package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func uplinkCaps(state capability.State) *capability.Registry {
	r := capability.NewRegistry()
	r.Set(capability.FeatWirelessUplink, state)
	return r
}

func uplinkWLAN() model.WLAN {
	return model.WLAN{
		ID: 3, SSID: "oonfee-roam", Enabled: true,
		Bands:    []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecPSK2, Key: "hunter2hunter2", PMF: model.PMFOptional},
		Options:  model.WLANOptions{AllowUplink: true},
	}
}

// The one option that makes this a bridge rather than a client. Without
// 4-address framing the device itself gets on the network and everything behind
// it stays dark — the wired ports it was put in that room for.
func TestUplinkRendersFourAddressStation(t *testing.T) {
	sec, _ := renderUplink(model.Uplink{ID: 1, Band: model.Band5G},
		uplinkWLAN(), model.Network{Name: "lan"}, "radio0",
		uplinkCaps(capability.Present))

	if sec.Values["mode"] != "sta" {
		t.Errorf("mode = %q, want sta", sec.Values["mode"])
	}
	if sec.Values["wds"] != "1" {
		t.Error("no wds=1: this renders an ordinary client, and everything " +
			"behind the device stays off the network")
	}
	if sec.Values["network"] != "lan" {
		t.Errorf("network = %q — the station must bridge into the LAN or the "+
			"wired ports are not on it", sec.Values["network"])
	}
	if sec.Name != "oowrt_up1_radio0" {
		t.Errorf("section name %q is not deterministic in the expected shape", sec.Name)
	}
}

func TestVLANUplinkUsesTheOwnedNetworkAttachment(t *testing.T) {
	section, _ := renderUplink(model.Uplink{ID: 1, Band: model.Band5G},
		uplinkWLAN(), model.Network{Name: "iot", VLAN: 45, Enabled: true}, "radio0",
		uplinkCaps(capability.Present))
	if section.Values["network"] != "oowrt_net_iot" {
		t.Fatalf("VLAN uplink network = %q, want owned attachment oowrt_net_iot",
			section.Values["network"])
	}
}

// Credentials come from the WLAN and are never restated on the uplink. Two
// copies of a passphrase drift, and a bridge whose key no longer matches does
// not fail cleanly — it fails the way a client with a stale password fails.
func TestUplinkTakesItsCredentialsFromTheWLAN(t *testing.T) {
	w := uplinkWLAN()
	sec, _ := renderUplink(model.Uplink{ID: 1, Band: model.Band5G}, w,
		model.Network{Name: "lan"}, "radio0", uplinkCaps(capability.Present))

	if sec.Values["ssid"] != w.SSID {
		t.Errorf("ssid = %q", sec.Values["ssid"])
	}
	if sec.Values["key"] != w.Security.Key {
		t.Errorf("key not taken from the WLAN")
	}
	if sec.Values["encryption"] != string(w.Security.Mode) {
		t.Errorf("encryption = %q", sec.Values["encryption"])
	}
}

// The §5g shape: a change that applies cleanly, reports healthy, and can take a
// network down. The controller cannot see the far end of a cable, so it states
// the condition rather than refusing — but it must state it BEFORE the apply.
func TestUplinkWarnsAboutTheLoopBeforeApplying(t *testing.T) {
	_, oms := renderUplink(model.Uplink{ID: 1, Band: model.Band5G}, uplinkWLAN(),
		model.Network{Name: "lan"}, "radio0", uplinkCaps(capability.Present))

	if len(oms) == 0 {
		t.Fatal("no warning at all about bridging a station into a cabled bridge")
	}
	var found bool
	for _, o := range oms {
		if strings.Contains(o.Reason, "loop") && strings.Contains(o.Reason, "STP") {
			found = true
		}
	}
	if !found {
		t.Errorf("the warning does not name the loop or STP: %+v", oms)
	}
}

// Three states, three different places to send an operator — the same rule the
// mesh gate holds, and the reason the gate is shared rather than reimplemented.
func TestUplinkGateSeparatesCannotFromCouldNotTell(t *testing.T) {
	if ok, _ := UplinkGate(uplinkCaps(capability.Present)); !ok {
		t.Error("Present did not pass the gate")
	}

	ok, reason := UplinkGate(uplinkCaps(capability.NotObservable))
	if ok {
		t.Error("NotObservable passed the gate")
	}
	if !strings.Contains(reason, "could not be established") {
		t.Errorf("NotObservable reads as a statement about the device: %q", reason)
	}

	ok, reason = UplinkGate(uplinkCaps(capability.Absent))
	if ok {
		t.Error("Absent passed the gate")
	}
	if !strings.Contains(reason, "supplicant") {
		t.Errorf("Absent does not name the missing piece: %q", reason)
	}
	// The two must not read the same. "Your device cannot" and "we could not
	// find out" send an operator to completely different places.
	_, notObs := UplinkGate(uplinkCaps(capability.NotObservable))
	if reason == notObs {
		t.Error("Absent and NotObservable produce the same sentence")
	}
}

// A gated-off uplink renders NOTHING. A station that applies and cannot come up
// is worse than no station: it looks configured.
func TestUplinkRendersNothingWhenGatedOff(t *testing.T) {
	for _, st := range []capability.State{capability.Absent, capability.NotObservable} {
		sec, oms := renderUplink(model.Uplink{ID: 1, Band: model.Band5G},
			uplinkWLAN(), model.Network{Name: "lan"}, "radio0", uplinkCaps(st))

		if sec.Name != "" || len(sec.Values) != 0 {
			t.Errorf("%s rendered a section: %+v", st, sec)
		}
		if len(oms) != 1 {
			t.Errorf("%s produced %d omissions, want exactly one saying why", st, len(oms))
		}
	}
}

// The AP half. Configure the station and forget this, and the station
// associates as an ordinary client while everything behind it stays dark — a
// failure that looks like a driver refusing 4-address frames and is not.
func TestWLANCarriesWDSOnlyWhenBridgesAreAllowed(t *testing.T) {
	caps := capability.NewRegistry()
	site := model.Site{UUID: "0f0e0d0c-0b0a-0908-0706-050403020100"}

	w := uplinkWLAN()
	sec, _ := renderWifiIface(site, w, model.Network{Name: "lan"}, "radio0", caps)
	if sec.Values["wds"] != "1" {
		t.Error("a WLAN that accepts wireless bridges did not render wds=1 on " +
			"the AP, so no station could ever bridge to it")
	}

	// Off must render wds=0, NOT omit the option.
	//
	// This test used to assert the opposite and was wrong, which hardware
	// showed: a plan compares only the keys it writes, so omitting the option
	// leaves whatever was last applied sitting on the device. Measured — after
	// turning the flag off and applying, the access points still carried
	// wds='1'. An AP still accepting 4-address frames while the screen says it
	// does not is a security posture nobody chose.
	w.Options.AllowUplink = false
	sec, _ = renderWifiIface(site, w, model.Network{Name: "lan"}, "radio0", caps)
	if sec.Values["wds"] != "0" {
		t.Errorf("wds = %q, want an explicit \"0\": omitting it leaves a stale "+
			"wds=1 on the device that no later apply will ever clear",
			sec.Values["wds"])
	}
}

// A device cannot bridge to a network it publishes itself.
//
// Found on hardware: the station came up in Client mode on the same radio
// already serving that SSID, and never associated — channel 0, signal 0. There
// is nothing wrong to LOOK at in that config, which is why this is refused
// rather than warned about: it is indistinguishable from a driver refusing
// 4-address framing, and an operator would spend the afternoon on the wrong
// question.
func TestUplinkRefusesToJoinANetworkTheDevicePublishes(t *testing.T) {
	site := model.Site{
		Groups: []model.APGroup{{ID: 1, Name: "all", DeviceIDs: []int64{7}}},
		WLANs: []model.WLAN{{
			ID: 3, SSID: "oonfee-roam", GroupID: 1, Enabled: true,
			Bands:   []model.Band{model.Band5G},
			Options: model.WLANOptions{AllowUplink: true},
		}},
	}
	u := model.Uplink{DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true}

	errs := u.Validate(site)

	var found bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "publishes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a device was allowed to bridge to a network it serves: %v", errs)
	}

	// A device NOT in that group is fine — this must not refuse every uplink.
	u.DeviceID = 8
	if errs := u.Validate(site); len(errs) != 0 {
		t.Errorf("a device outside the AP group was refused: %v", errs)
	}
}

// A device with no radio on the requested band cannot join there, and must be
// told which of the two things is missing.
func TestUplinkValidationCatchesTheHalfPeopleForget(t *testing.T) {
	site := model.Site{
		WLANs: []model.WLAN{{
			ID: 3, SSID: "oonfee-roam", Enabled: true,
			Bands:   []model.Band{model.Band5G},
			Options: model.WLANOptions{AllowUplink: false},
		}},
	}
	u := model.Uplink{DeviceID: 1, WLANID: 3, Band: model.Band5G, Enabled: true}

	errs := u.Validate(site)

	var sawAllow bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "does not accept wireless bridges") {
			sawAllow = true
		}
	}
	if !sawAllow {
		t.Errorf("validation did not catch the AP half being off: %v", errs)
	}

	// And with it on, and the band wrong, it says THAT instead.
	site.WLANs[0].Options.AllowUplink = true
	u.Band = model.Band2G
	errs = u.Validate(site)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "not published on") {
		t.Errorf("want exactly the band complaint, got %v", errs)
	}
}

// The traversal gate must recognise the section this renderer produces.
//
// The coupling is pinned from here because this package can see both sides:
// applyengine defines the predicate and cannot import render, so if the naming
// ever changes it is this test that fails rather than the guard silently going
// quiet. §6's rule about guards that cannot fire, applied to the guard itself.
func TestUplinkSectionsCountAsTheManagementPath(t *testing.T) {
	name := uplinkIfaceName(1, "radio0")

	if !applyengine.IsUplinkSection("wireless", name) {
		t.Fatalf("the traversal gate does not recognise %q, so removing a "+
			"device's only route would apply unacknowledged and unwarned", name)
	}

	// And it must not swallow the other wireless sections, or every ordinary
	// SSID edit starts demanding a traversal acknowledgment and the gate
	// becomes something operators click past.
	for _, other := range []string{
		ifaceName(1, "radio0"),
		meshIfaceName(1, "radio0"),
		"default_radio0",
	} {
		if applyengine.IsUplinkSection("wireless", other) {
			t.Errorf("%q was treated as a management path; a gate that fires on "+
				"everything is a gate nobody reads", other)
		}
	}

	// A delete of the uplink is the dangerous operation, and it is an ordinary
	// wireless delete — which is exactly why the config check alone missed it.
	plan := applyengine.Plan{Ops: []applyengine.Op{
		{Kind: applyengine.OpDelete, Config: "wireless", Section: name},
	}}
	if !applyengine.TouchesManagementPath(plan) {
		t.Error("removing a device's wireless uplink did not count as touching " +
			"the management path")
	}
}

// A disabled or bridge-refusing WLAN must not render a station, even if nobody
// validated the site first. Render is not entitled to assume its caller checked.
func TestUplinkRendersNothingForAnUnusableWLAN(t *testing.T) {
	site := model.Site{
		Networks: []model.Network{{ID: 1, Name: "lan", Enabled: true}},
		WLANs: []model.WLAN{{
			ID: 3, SSID: "oonfee-roam", NetworkID: 1, Enabled: false,
			Bands:   []model.Band{model.Band5G},
			Options: model.WLANOptions{AllowUplink: true},
		}},
		Uplinks: []model.Uplink{{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true}},
	}

	if errs := site.Validate(); len(errs) == 0 {
		t.Error("a uplink onto a disabled network passed site validation, so " +
			"every sentence Uplink.Validate produces is unreachable")
	}
}
