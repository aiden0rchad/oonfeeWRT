package daemon

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func radioCaps(n int) *capability.Registry {
	r := capability.NewRegistry()
	r.Set(capability.FeatSurvey, capability.Present)
	for i := 0; i < n; i++ {
		// A hardware name, because a real probe of a broadcasting radio always
		// gets one. Without it every device here would trip the
		// "could not be checked against the known-defect list" note, which is
		// correct behaviour but not what these tests are about.
		r.Radios = append(r.Radios, capability.Radio{
			Phy:      "phy" + string(rune('0'+i)),
			Hardware: "Generic MAC80211",
		})
	}
	r.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1"}, WAN: "wan"}
	return r
}

// An empty radio list means either "this device has none" or "we could not
// ask", and those need different messages: one says the role is wrong, the
// other says the check failed. Telling an operator to change the role when the
// probe established nothing sends them to fix the wrong thing; claiming every
// NotObservable state is an ACL refusal is equally misleading.
func TestRoleFitSeparatesNoRadiosFromNoAnswer(t *testing.T) {
	none := capability.NewRegistry()
	none.Set(capability.FeatSurvey, capability.Absent) // asked; there are none
	got := roleFit(model.RoleAP, none)
	if len(got) != 1 {
		t.Fatalf("got %v, want one warning", got)
	}
	if !strings.Contains(got[0], "reported no radios") ||
		!strings.Contains(got[0], string(model.RoleSwitch)) {
		t.Errorf("the message %q should name the fact and the role that fits", got[0])
	}

	refused := capability.NewRegistry()
	refused.Set(capability.FeatSurvey, capability.NotObservable) // could not ask
	got = roleFit(model.RoleAP, refused)
	if len(got) != 1 {
		t.Fatalf("got %v, want one warning", got)
	}
	if strings.Contains(got[0], "reported no radios") {
		t.Errorf("a refused check was reported as an absence of radios: %q", got[0])
	}
	if !strings.Contains(got[0], "could not be listed") ||
		!strings.Contains(got[0], "capability notes") {
		t.Errorf("the message %q does not point to the recorded cause", got[0])
	}
	if strings.Contains(got[0], "This is an access-control gap") {
		t.Errorf("an unclassified failure was asserted to be an ACL refusal: %q", got[0])
	}
}

// A wireless role on a device with radios is simply correct, and must produce
// no noise — a warning on the ordinary case teaches operators to ignore them.
func TestRoleFitIsQuietWhenTheRoleMatches(t *testing.T) {
	for _, role := range []model.Role{model.RoleAP, model.RoleGateway} {
		if got := roleFit(role, radioCaps(2)); len(got) != 0 {
			t.Errorf("role %q on a two-radio device warned: %v", role, got)
		}
	}
}

// A switch WITH radios is the intended use of the role, not a problem — but it
// is worth confirming, because "I adopted it and it never broadcast" is exactly
// the question this pre-empts.
func TestRoleFitConfirmsASwitchWillNotBroadcast(t *testing.T) {
	got := roleFit(model.RoleSwitch, radioCaps(2))
	if len(got) != 1 {
		t.Fatalf("got %v, want one note", got)
	}
	if !strings.Contains(got[0], string(model.RoleAP)) {
		t.Errorf("the note %q does not say which role would broadcast", got[0])
	}
}

// A gateway with no WAN port is worth flagging — and a device whose port map
// could not be read at all is not, because then nothing was established.
func TestRoleFitFlagsAGatewayWithNoWANOnlyWhenPortsWereReadable(t *testing.T) {
	caps := radioCaps(2)
	caps.Ports.WAN = ""
	got := roleFit(model.RoleGateway, caps)
	if len(got) != 1 || !strings.Contains(got[0], "WAN") {
		t.Fatalf("a gateway with no WAN port produced %v", got)
	}

	// Port map unreadable: bridge and LAN list both empty. Nothing was
	// established, so there is nothing to report.
	blind := capability.NewRegistry()
	blind.Set(capability.FeatSurvey, capability.Present)
	blind.Radios = append(blind.Radios, capability.Radio{
		Phy: "phy0", Hardware: "Generic MAC80211"})
	if got := roleFit(model.RoleGateway, blind); len(got) != 0 {
		t.Errorf("an unreadable port map was reported as a missing WAN: %v", got)
	}
}

func TestRoleFitToleratesAMissingRegistry(t *testing.T) {
	if got := roleFit(model.RoleAP, nil); got != nil {
		t.Errorf("got %v for a nil registry", got)
	}
}

func TestFunctionFitDoesNotInventSwitchControl(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Set(capability.FeatSurvey, capability.Absent)
	caps.Set(capability.FeatSwitchPorts, capability.Absent)
	caps.Ports.Bridge = "eth0"
	got := functionFit(model.DeviceFunctions{model.FunctionSwitch}, caps)
	if len(got) != 1 || !strings.Contains(got[0], "does not invent") ||
		!strings.Contains(got[0], "managed-VLAN") {
		t.Fatalf("unobservable switch control warning=%v", got)
	}
}

// Someone repurposing an old router should learn its radio has a known defect
// at ADOPTION, while they are deciding whether to build on it — not three
// screens later when a preview warns, and not never.
func TestAdoptionWarnsAboutKnownHardwareDefects(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Set(capability.FeatSurvey, capability.Present)
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Hardware: "Marvell 88W8964", Frequency: 5180},
	}

	got := roleFit(model.RoleAP, caps)
	var found string
	for _, w := range got {
		if strings.Contains(w, "known defect") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("adopting hardware with a registry entry produced no defect "+
			"warning; got %v", got)
	}
	// The confidence and the source must travel with it, or the operator
	// cannot tell a maintainer's statement from a forum post.
	if !strings.Contains(found, "measured") && !strings.Contains(found, "documented") {
		t.Errorf("warning carries no confidence: %q", found)
	}
	if !strings.Contains(found, "Source:") {
		t.Errorf("warning carries no source: %q", found)
	}

	// A defect that only fires on a specific setting must NOT appear here.
	// Telling someone 802.11w is broken when they have not enabled it is noise,
	// and noise is how real warnings get ignored.
	for _, w := range got {
		if strings.Contains(w, "802.11w") {
			t.Errorf("a config-triggered defect leaked into the adoption "+
				"warnings, where there is no config yet: %q", w)
		}
	}
}

// Hardware nobody has catalogued must adopt silently. A registry that warns
// about every device is one nobody reads.
func TestAdoptionIsSilentForUncataloguedHardware(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Set(capability.FeatSurvey, capability.Present)
	caps.Radios = []capability.Radio{
		{Device: "phy0-ap0", Hardware: "Qualcomm Atheros QCA9880", Frequency: 5180},
	}
	for _, w := range roleFit(model.RoleAP, caps) {
		if strings.Contains(w, "known defect") {
			t.Errorf("warned about hardware with no registry entry: %q", w)
		}
	}
}

// A device whose radios cannot be identified must be TOLD it was not checked.
//
// This is the cardinal error of the capability package reaching the defect
// registry. The hardware name comes from iwinfo, iwinfo only answers for a
// radio that has an interface, and stock OpenWrt ships its default wifi-iface
// disabled so nothing is broadcasting —
// so the freshly adopted router, the one whose operator is at that moment
// choosing the security settings the registry exists to warn about, is exactly
// the device that would otherwise look defect-free.
func TestAdoptionSaysWhenItCouldNotCheckTheHardware(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Set(capability.FeatSurvey, capability.Present)
	caps.Radios = []capability.Radio{{Phy: "phy0"}, {Phy: "phy1"}} // no Hardware

	var said bool
	for _, w := range roleFit(model.RoleAP, caps) {
		if strings.Contains(w, "could not be checked") {
			said = true
			if !strings.Contains(w, "not a clean bill of health") {
				t.Errorf("the note does not say silence means nothing: %q", w)
			}
		}
	}
	if !said {
		t.Error("adopting a device whose radios named nothing produced no note; " +
			"matching zero defects is not the same as having none")
	}
}
