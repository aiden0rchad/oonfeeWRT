package render

import (
	"sort"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func roamingSite(ft, kv bool) model.Site {
	s := testSite()
	s.WLANs[0].Security = model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"}
	s.WLANs[0].Roaming = model.Roaming{FT: ft, FTWithPSK2: true, KV: kv}
	return s
}

// Turning a setting OFF has to reach the device.
//
// plan.matches compares only the keys we write, so an option written under a
// condition and then no longer written was never compared and never cleared.
// Measured on both reference devices: turning 802.11r off produced ZERO
// operations. The preview said "already matches", and ieee80211r=1 and its
// mobility domain stayed on the air.
//
// On the WRT3200ACM that is the setting §5an found wedges the radio 85 seconds
// after a roam — so the one remedy an operator would reach for could be chosen,
// applied, confirmed, and have no effect at all.
func TestTurningRoamingOffReachesTheDevice(t *testing.T) {
	// What the device holds after an apply with everything on.
	on, _, err := Render(roamingSite(true, true), model.Device{ID: 7},
		dualBandCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	device := map[string]map[string]string{}
	for _, s := range on.Sections {
		vals := map[string]string{".type": "wifi-iface"}
		for k, v := range s.Values {
			vals[k] = v
		}
		device[s.Name] = vals
	}
	existing := WirelessOnly(device)

	// Now the operator turns both off.
	off, _, err := Render(roamingSite(false, false), model.Device{ID: 7},
		dualBandCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	ops := off.Plan(existing).Ops
	if len(ops) == 0 {
		t.Fatal("turning 802.11r and 802.11k/v off produced no operations at " +
			"all: the preview reports 'already matches' and the device keeps " +
			"broadcasting with fast transition enabled")
	}
	for _, s := range off.Sections {
		for _, k := range []string{"ieee80211r", "ieee80211k", "bss_transition",
			"rrm_neighbor_report", "rrm_beacon_report", "wnm_sleep_mode"} {
			if s.Values[k] != "0" {
				t.Errorf("%s.%s = %q; an omitted option is not off on the "+
					"device, it is whatever the last apply left there",
					s.Name, k, s.Values[k])
			}
		}
	}
}

// The structural guard: a flag writes both directions unless its false value
// is unsafe and the option is explicitly managed away. bridge_isolate is that
// exception: old wifi-scripts reject even bridge_isolate=0.
func TestNoFlagChangesWhichOptionsExist(t *testing.T) {
	all := func(on bool) model.Site {
		s := testSite()
		s.WLANs[0].Security = model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"}
		s.WLANs[0].Roaming = model.Roaming{FT: on, FTWithPSK2: on, FTOverDS: on, KV: on}
		s.WLANs[0].Options = model.WLANOptions{Hidden: on, Isolate: on, AllowUplink: on}
		return s
	}
	keys := func(on bool) []string {
		doc, _, err := Render(all(on), model.Device{ID: 7}, dualBandCaps(), Existing{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for k := range doc.Sections[0].Values {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	onKeys, offKeys := keys(true), keys(false)
	var onlyOn []string
	offSet := map[string]bool{}
	for _, key := range offKeys {
		offSet[key] = true
	}
	for _, key := range onKeys {
		if !offSet[key] {
			onlyOn = append(onlyOn, key)
		}
	}
	if len(onKeys) != len(offKeys)+1 || len(onlyOn) != 1 || onlyOn[0] != "bridge_isolate" {
		t.Fatalf("turning every flag off changed which options are written.\n"+
			" on: %v\noff: %v\nAn option that disappears is not set to off on "+
			"the device unless it is explicitly managed; only bridge_isolate may "+
			"disappear because older wifi-scripts reject its false value.", onKeys, offKeys)
	}
	for _, key := range onKeys {
		if key != "bridge_isolate" && !offSet[key] {
			t.Errorf("option %q disappeared when flags were turned off", key)
		}
	}
}

// maxassoc is the exception, and the reason Section.Manages exists: hostapd
// does not read `maxassoc 0` as "no limit", so there is no safe value to write
// for "unset" and the option has to be removed instead.
func TestClearingMaxAssocDeletesTheOptionRatherThanWritingZero(t *testing.T) {
	site := testSite()
	site.WLANs[0].Security = model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"}
	site.WLANs[0].Options.MaxAssoc = 32
	withCap, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	if withCap.Sections[0].Values["maxassoc"] != "32" {
		t.Fatalf("maxassoc not written: %v", withCap.Sections[0].Values)
	}
	device := map[string]map[string]string{}
	for _, s := range withCap.Sections {
		vals := map[string]string{".type": "wifi-iface"}
		for k, v := range s.Values {
			vals[k] = v
		}
		device[s.Name] = vals
	}
	existing := WirelessOnly(device)

	site.WLANs[0].Options.MaxAssoc = 0
	cleared, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cleared.Sections[0].Values["maxassoc"]; ok {
		t.Errorf("maxassoc=%q written for 'no limit'; hostapd does not read "+
			"zero that way and this would cap the AP at no clients", v)
	}
	var deleted bool
	for _, op := range cleared.Plan(existing).Ops {
		if op.Kind == applyengine.OpDelete && op.Option == "maxassoc" {
			deleted = true
		}
	}
	if !deleted {
		t.Error("clearing the client limit left maxassoc on the device")
	}
}

// A delete is only emitted for an option the device actually has. stage()
// aborts the whole batch on any failed op, so deleting an absent option would
// turn a good apply into no apply at all.
func TestNoDeleteIsEmittedForAnOptionTheDeviceDoesNotHave(t *testing.T) {
	site := testSite()
	site.WLANs[0].Security = model.Security{Mode: model.SecPSK2, Key: "not-a-real-key"}
	// A section that is ours, differs, and has none of the managed extras.
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_wlan1_radio0": {".type": "wifi-iface", OwnershipTag: "1", "ssid": "old"},
		"oowrt_wlan1_radio1": {".type": "wifi-iface", OwnershipTag: "1", "ssid": "old"},
	})
	doc, _, err := Render(site, model.Device{ID: 7}, dualBandCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range doc.Plan(existing).Ops {
		if op.Kind == applyengine.OpDelete {
			t.Errorf("delete emitted for %s.%s option %q, which the device does "+
				"not have; stage() aborts the batch on a failed op",
				op.Config, op.Section, op.Option)
		}
	}
}
