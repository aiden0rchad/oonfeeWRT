package daemon

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/render"
)

// A pre-apply hazard must not be filed under the heading that calls omissions
// "not an error — the hardware or firmware cannot take it".
//
// Two omissions describe a network that stops working: an unencrypted mesh
// anyone in range can join, and a wireless bridge that is a layer-2 loop if the
// device is also cabled. Both sat in that list, in muted grey, directly under
// the reassurance. A third kind says a section was KEPT because the device
// could not be read, which is the reverse of "left out".
func TestOmissionsAreRoutedByWhatTheyActuallyAre(t *testing.T) {
	omitted, cautions, undetermined := splitOmissions([]render.Omission{
		{WLAN: "home", Reason: "device has no 6g radio"},
		{WLAN: "roam", Reason: "layer-2 loop", Kind: render.KindCaution},
		{WLAN: "bh", Reason: "this mesh is unencrypted", Kind: render.KindCaution},
		{WLAN: "oowrt_up1", Reason: "left exactly as it is", Kind: render.KindUndetermined},
	})
	if len(cautions) != 2 {
		t.Errorf("cautions = %v; a hazard shown as a hardware limit is a hazard "+
			"the operator will scroll past", cautions)
	}
	if len(undetermined) != 1 {
		t.Errorf("undetermined = %v", undetermined)
	}
	if len(omitted) != 1 || !strings.Contains(omitted[0], "no 6g radio") {
		t.Errorf("omitted = %v", omitted)
	}
	// Nothing may be dropped on the way through: an omission that vanished
	// here is the silent drop the whole report exists to prevent.
	if n := len(omitted) + len(cautions) + len(undetermined); n != 4 {
		t.Errorf("routed %d of 4 omissions", n)
	}
}

// An omission with no kind falls to the neutral list, never to a heading that
// asserts a cause nobody established. The zero value must claim nothing.
func TestAnUnclassifiedOmissionClaimsNothing(t *testing.T) {
	omitted, cautions, undetermined := splitOmissions([]render.Omission{
		{WLAN: "x", Reason: "some future reason nobody classified"},
	})
	if len(cautions) != 0 || len(undetermined) != 0 {
		t.Error("an unclassified omission was given a meaning it does not have")
	}
	if len(omitted) != 1 {
		t.Errorf("omitted = %v", omitted)
	}
}
