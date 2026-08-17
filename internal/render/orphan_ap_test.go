package render

import (
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// An AP that no WLAN targets broadcasts nothing, and used to say so nowhere.
//
// Preview reported "already matches — nothing to do", which is true and useless:
// the device genuinely matches a model that asks nothing of it. Seen for real on
// 2026-08-17 — a device came back from a re-adoption healthy, polling, adopted,
// and silently off the air, with a preview insisting everything was fine. The
// cause was group membership: un-adopt deletes the device row, its ap_group_members
// row goes with it by cascade, and the re-adopted device is a NEW id in no group.
func TestAnAPNoWLANTargetsSaysSo(t *testing.T) {
	site := model.Site{
		UUID:     "site-uuid-for-tests",
		Networks: []model.Network{{ID: 1, Name: "lan", VLAN: 1, Enabled: true}},
		// The group exists and contains somebody ELSE, which is exactly the
		// shape a re-adoption leaves behind.
		Groups: []model.APGroup{{ID: 1, Name: "all-aps", DeviceIDs: []int64{99}}},
		WLANs: []model.WLAN{{
			ID: 1, SSID: "Home", NetworkID: 1, GroupID: 1,
			Bands:    []model.Band{model.Band2G, model.Band5G},
			Security: model.Security{Mode: model.SecPSK2, Key: "not-a-real-key-2f8Qv1xLpZ"},
			Enabled:  true,
		}},
	}
	caps := &capability.Registry{Radios: []capability.Radio{
		{Device: "radio0", Phy: "radio0", Band: "5g"},
		{Device: "radio1", Phy: "radio1", Band: "2g"},
	}}

	_, rep, err := Render(site, model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var found bool
	for _, o := range rep.Omissions {
		if contains(o.Reason, "no WLAN targets this device") {
			found = true
			if !contains(o.Reason, "AP group") {
				t.Errorf("the omission does not point at the likely cause: %q", o.Reason)
			}
		}
	}
	if !found {
		t.Errorf("an AP that will broadcast nothing produced no explanation: %+v",
			rep.Omissions)
	}

	// And the same device IN the group gets no such note — otherwise it is a
	// warning on every healthy apply, which is how a warning stops being read.
	site.Groups[0].DeviceIDs = append(site.Groups[0].DeviceIDs, 7)
	_, rep2, err := Render(site, model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, o := range rep2.Omissions {
		if contains(o.Reason, "no WLAN targets this device") {
			t.Errorf("a device that IS targeted was told it is not: %q", o.Reason)
		}
	}
}

// A site with no WLANs at all describes the site, not the device — saying it
// per device would be noise on a fresh install.
func TestNoWLANsAnywhereIsNotADeviceProblem(t *testing.T) {
	site := model.Site{
		UUID:     "site-uuid-for-tests",
		Networks: []model.Network{{ID: 1, Name: "lan", VLAN: 1, Enabled: true}},
		Groups:   []model.APGroup{{ID: 1, Name: "all-aps"}},
	}
	caps := &capability.Registry{Radios: []capability.Radio{
		{Device: "radio0", Phy: "radio0", Band: "5g"},
	}}
	_, rep, err := Render(site, model.Device{ID: 7, Role: model.RoleAP}, caps, Existing{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, o := range rep.Omissions {
		if contains(o.Reason, "no WLAN targets this device") {
			t.Errorf("a site with no WLANs blamed the device: %q", o.Reason)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
