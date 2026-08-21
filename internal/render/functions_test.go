package render

import (
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestIndependentFunctionRenderMatrix(t *testing.T) {
	site := model.Site{
		UUID: "function-matrix",
		Networks: []model.Network{{
			ID: 1, Name: "iot", VLAN: 45, CIDR: "10.7.45.1/24", Zone: "guest", Enabled: true,
		}},
		Groups: []model.APGroup{{ID: 1, Name: "all", DeviceIDs: []int64{7}}},
		WLANs: []model.WLAN{{
			ID: 1, SSID: "IoT", NetworkID: 1, GroupID: 1,
			Bands: []model.Band{model.Band2G, model.Band5G}, Enabled: true,
		}},
	}
	caps := dualBandCaps()
	caps.Ports = netCaps().Ports

	for _, tc := range []struct {
		name      string
		functions model.DeviceFunctions
		wantL3    bool
		wantWLAN  bool
	}{
		{"gateway only", model.DeviceFunctions{model.FunctionGateway}, true, false},
		{"AP only", model.DeviceFunctions{model.FunctionAP}, false, true},
		{"switch only", model.DeviceFunctions{model.FunctionSwitch}, false, false},
		{"AP and switch", model.DeviceFunctions{model.FunctionAP, model.FunctionSwitch}, false, true},
		{"all three", model.DeviceFunctions{model.FunctionGateway, model.FunctionAP, model.FunctionSwitch}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, _, err := Render(site, model.Device{
				ID: 7, Role: tc.functions.PrimaryRole(), Functions: tc.functions,
			}, caps, vlanAware())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := sectionsIn(doc, "network")["oowrt_bv45"]; !ok {
				t.Fatal("minimum L2 bridge plumbing was omitted")
			}
			hasL3 := len(sectionsIn(doc, "dhcp")) > 0 && len(sectionsIn(doc, "firewall")) > 0
			if hasL3 != tc.wantL3 {
				t.Errorf("L3/DHCP/firewall=%v, want %v", hasL3, tc.wantL3)
			}
			hasWLAN := len(sectionsIn(doc, "wireless")) > 0
			if hasWLAN != tc.wantWLAN {
				t.Errorf("WLAN=%v, want %v", hasWLAN, tc.wantWLAN)
			}
		})
	}
}

func TestInvalidPersistedFunctionSetCannotRenderOrPrune(t *testing.T) {
	doc, _, err := Render(testSite(), model.Device{
		ID: 7, Role: model.RoleGateway, Functions: model.DeviceFunctions{},
	}, dualBandCaps(), Existing{})
	if err == nil {
		t.Fatal("explicit invalid function set rendered by falling back to legacy role")
	}
	if len(doc.Sections) != 0 {
		t.Fatalf("invalid function set produced desired sections: %+v", doc.Sections)
	}
}
