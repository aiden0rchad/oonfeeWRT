package render

import (
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func upCaps() *capability.Registry {
	r := dualBandCaps()
	r.Set(capability.FeatWirelessUplink, capability.Present)
	return r
}

func upSite(allow bool, uplinks ...model.Uplink) model.Site {
	s := testSite()
	s.WLANs[0].Options.AllowUplink = allow
	s.Uplinks = uplinks
	return s
}

func TestScratchAllowUplinkFalseStillRendersStation(t *testing.T) {
	site := upSite(false, model.Uplink{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true})
	if errs := site.Validate(); len(errs) > 0 {
		t.Logf("site.Validate errs: %v", errs)
	} else {
		t.Log("site.Validate: NO ERRORS despite AllowUplink=false")
	}
	doc, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, upCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sections {
		t.Logf("section %s mode=%s wds=%s ssid=%s", s.Name, s.Values["mode"], s.Values["wds"], s.Values["ssid"])
	}
	t.Logf("omissions: %+v", rep.Omissions)
}

func TestScratchForeignUplinkName(t *testing.T) {
	site := upSite(true, model.Uplink{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true})
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_up1_radio0": {".type": "wifi-iface", "mode": "ap", "ssid": "someone-elses"},
	})
	doc, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, upCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("conflicts: %+v", rep.Conflicts)
	for _, s := range doc.Sections {
		if s.Name == "oowrt_up1_radio0" {
			t.Logf("OVERWRITES foreign section: %+v", s.Values)
		}
	}
}

func TestScratchSwitchRoleDropsUplink(t *testing.T) {
	site := upSite(true, model.Uplink{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true})
	doc, rep, err := Render(site, model.Device{ID: 7, Role: model.RoleSwitch}, upCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sections: %d", len(doc.Sections))
	for _, s := range doc.Sections {
		t.Logf("  %s", s.Name)
	}
	t.Logf("omissions: %+v", rep.Omissions)
}

func TestScratchTwoUplinks(t *testing.T) {
	site := upSite(true,
		model.Uplink{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true},
		model.Uplink{ID: 2, DeviceID: 7, WLANID: 3, Band: model.Band2G, Enabled: true})
	if errs := site.Validate(); len(errs) > 0 {
		t.Logf("validate: %v", errs)
	} else {
		t.Log("site.Validate: NO ERRORS for two uplinks on one device")
	}
	doc, _, err := Render(site, model.Device{ID: 7, Role: "ap"}, upCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sections {
		if s.Values["mode"] == "sta" {
			t.Logf("station: %s band-radio %s", s.Name, s.Values["device"])
		}
	}
}

func TestScratchSelfJoin(t *testing.T) {
	// Device 7 is in the AP group publishing WLAN 3 AND uplinks to WLAN 3.
	site := upSite(true, model.Uplink{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true})
	doc, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, upCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sections {
		t.Logf("%s: mode=%s device=%s ssid=%s wds=%s", s.Name, s.Values["mode"],
			s.Values["device"], s.Values["ssid"], s.Values["wds"])
	}
	t.Logf("omissions: %+v", rep.Omissions)
}

func TestScratchUplinkDisabledWLAN(t *testing.T) {
	site := upSite(true, model.Uplink{ID: 1, DeviceID: 7, WLANID: 3, Band: model.Band5G, Enabled: true})
	site.WLANs[0].Enabled = false
	doc, rep, err := Render(site, model.Device{ID: 7, Role: "ap"}, upCaps(), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sections {
		t.Logf("%s: mode=%s ssid=%s", s.Name, s.Values["mode"], s.Values["ssid"])
	}
	t.Logf("omissions: %+v", rep.Omissions)
}

func TestScratchSupplicantOnly(t *testing.T) {
	t.Log("see capability package")
}
