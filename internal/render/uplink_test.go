package render

import (
	"strings"
	"testing"

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

	w.Options.AllowUplink = false
	sec, _ = renderWifiIface(site, w, model.Network{Name: "lan"}, "radio0", caps)
	if _, present := sec.Values["wds"]; present {
		t.Error("wds was rendered on a WLAN that does not accept bridges; that " +
			"changes what the AP accepts from the air without anyone asking")
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
