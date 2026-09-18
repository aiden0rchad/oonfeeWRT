package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestRenderRejectsLegacySharedPHYBeforeBuildingWireless(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bands []model.Band
	}{
		{name: "single requested band", bands: []model.Band{model.Band2G}},
		{name: "dual requested band", bands: []model.Band{model.Band2G, model.Band5G}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := capability.NewRegistry()
			caps.Radios = []capability.Radio{
				{Device: "phy0.0-ap0", Phy: "phy0", Frequency: 2412},
				{Device: "phy0.1-ap0", Phy: "phy0", Frequency: 5180},
			}
			site := testSite()
			site.WLANs[0].Bands = tc.bands

			doc, _, err := Render(site, model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
			if err == nil || !strings.Contains(err.Error(), "Re-probe capabilities") {
				t.Fatalf("ambiguous legacy shared-PHY inventory was not rejected with re-probe guidance: %v", err)
			}
			if len(doc.Sections) != 0 {
				t.Fatalf("rejected radio inventory returned a partial document: %+v", doc.Sections)
			}
		})
	}
}

func TestRenderUsesAuthoritativeSectionsForSharedPHY(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Section: "radio0", Device: "phy0.0-ap0", Phy: "phy0", Frequency: 2412},
		{Section: "radio1", Device: "phy0.1-ap0", Phy: "phy0", Frequency: 5180},
	}
	doc, rep, err := Render(testSite(), model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
	if err != nil || rep.HasConflicts() {
		t.Fatalf("Render: err=%v conflicts=%+v", err, rep.Conflicts)
	}
	for name, target := range map[string]string{
		"oowrt_wlan3_radio0": "radio0",
		"oowrt_wlan3_radio1": "radio1",
	} {
		section, ok := sectionByName(doc, name)
		if !ok || section.Values["device"] != target {
			t.Errorf("%s = %+v, want device %s", name, section, target)
		}
	}
}

func TestRenderUsesCustomSectionDespitePHYIndexMismatch(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Section: "wifi_2g_custom", Device: "phy8.0-ap0", Phy: "phy8", Frequency: 2412},
		{Section: "wifi_5g_custom", Device: "phy0.1-ap0", Phy: "phy0", Frequency: 5180},
	}
	doc, _, err := Render(testSite(), model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"oowrt_wlan3_wifi_2g_custom": "wifi_2g_custom",
		"oowrt_wlan3_wifi_5g_custom": "wifi_5g_custom",
	} {
		section, ok := sectionByName(doc, name)
		if !ok || section.Values["device"] != target {
			t.Errorf("%s = %+v, want device %s", name, section, target)
		}
	}
}

func TestRenderRejectsInvalidOrCollidingRadioTargets(t *testing.T) {
	valid := func(section, phy string, frequency int) capability.Radio {
		return capability.Radio{Section: section, Device: section + "-ap0", Phy: phy, Frequency: frequency}
	}
	for _, tc := range []struct {
		name   string
		radios []capability.Radio
	}{
		{name: "explicit duplicate", radios: []capability.Radio{
			valid("radio0", "phy0", 2412), valid("radio0", "phy1", 5180),
		}},
		{name: "explicit fallback collision", radios: []capability.Radio{
			valid("radio0", "phy9", 2412), {Device: "phy0-ap0", Phy: "phy0", Frequency: 5180},
		}},
		{name: "invalid explicit never falls back", radios: []capability.Radio{
			valid("radio-0", "phy0", 2412),
		}},
		{name: "invalid legacy", radios: []capability.Radio{
			{Device: "bad-radio", Phy: "bad-radio", Frequency: 2412},
		}},
		{name: "empty resolved target", radios: []capability.Radio{
			{Frequency: 2412},
		}},
		{name: "overlong explicit", radios: []capability.Radio{
			valid(strings.Repeat("r", 65), "phy0", 2412),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := capability.NewRegistry()
			caps.Radios = tc.radios
			doc, _, err := Render(testSite(), model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
			if err == nil || !strings.Contains(err.Error(), "Re-probe capabilities") {
				t.Fatalf("invalid targets were not rejected with re-probe guidance: %v", err)
			}
			if doc.DeviceID != 0 || len(doc.Sections) != 0 || len(doc.Patches) != 0 {
				t.Fatalf("invalid targets returned partial document: %+v", doc)
			}
		})
	}
}

func TestInvalidRadioInventoryDoesNotBlockNonWirelessRole(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Device: "phy0.0-ap0", Phy: "phy0", Frequency: 2412},
		{Device: "phy0.1-ap0", Phy: "phy0", Frequency: 5180},
	}
	if _, _, err := Render(testSite(), model.Device{ID: 7, Role: model.RoleSwitch}, caps, Existing{}); err != nil {
		t.Fatalf("non-wireless role was blocked by unused radio inventory: %v", err)
	}
}

func TestInvalidRadioInventoryReturnsNoEarlierNetworkSections(t *testing.T) {
	caps := vlanWirelessCaps()
	caps.Radios = []capability.Radio{{Section: "radio-0", Phy: "phy0", Frequency: 2412}}
	doc, _, err := Render(vlanWirelessSite(), model.Device{ID: 7, Role: model.RoleAP}, caps, vlanAware())
	if err == nil {
		t.Fatal("invalid explicit radio section was accepted")
	}
	if doc.DeviceID != 0 || len(doc.Sections) != 0 || len(doc.Patches) != 0 {
		t.Fatalf("rejected wireless plan leaked earlier network sections: %+v", doc)
	}
}

func TestDocValidateRejectsIdenticalDuplicateSections(t *testing.T) {
	section := Section{Config: "wireless", Type: "wifi-iface", Name: "same"}
	err := (Doc{Sections: []Section{section, section}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate desired section wireless.same") {
		t.Fatalf("duplicate validation error = %v", err)
	}
	if err := (Doc{Sections: []Section{
		{Config: "wireless", Name: "same"},
		{Config: "network", Name: "same"},
	}}).Validate(); err != nil {
		t.Fatalf("same name in different configs is valid: %v", err)
	}
}
