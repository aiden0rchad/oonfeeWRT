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
		r.Radios = append(r.Radios, capability.Radio{Phy: "phy" + string(rune('0'+i))})
	}
	r.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1"}, WAN: "wan"}
	return r
}

// An empty radio list means either "this device has none" or "we could not
// ask", and those need different messages: one says the role is wrong, the
// other says the ACL is narrow. Telling an operator to change the role when the
// real problem is a refused call sends them to fix the wrong thing.
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
	if !strings.Contains(got[0], "refused") {
		t.Errorf("the message %q does not say the check was refused", got[0])
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
	blind.Radios = append(blind.Radios, capability.Radio{Phy: "phy0"})
	if got := roleFit(model.RoleGateway, blind); len(got) != 0 {
		t.Errorf("an unreadable port map was reported as a missing WAN: %v", got)
	}
}

func TestRoleFitToleratesAMissingRegistry(t *testing.T) {
	if got := roleFit(model.RoleAP, nil); got != nil {
		t.Errorf("got %v for a nil registry", got)
	}
}
