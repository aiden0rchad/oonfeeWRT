package capability

import "testing"

// Present and Absent are answers about the device. Unknown and NotObservable
// are not, and every gate in the tree used to switch on NotObservable alone —
// letting Unknown fall through to the branch that tells an operator, in a
// complete sentence, that their hardware lacks a feature.
func TestOnlyPresentAndAbsentAreAnswersAboutTheDevice(t *testing.T) {
	for _, tc := range []struct {
		s    State
		want bool
	}{
		{Present, true},
		{Absent, true},
		{NotObservable, false},
		{Unknown, false},
	} {
		if got := tc.s.Decided(); got != tc.want {
			t.Errorf("%s.Decided() = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// The zero value must be Unknown, because that is what a capability record
// written before a Feature existed decodes to. If it were Absent, every device
// adopted before a feature was added would report that its hardware lacks it.
func TestTheZeroStateIsUnknown(t *testing.T) {
	var s State
	if s != Unknown {
		t.Fatalf("zero State is %s; a record with no entry for a feature would "+
			"claim that answer about the device", s)
	}
	if s.Decided() {
		t.Error("the zero State claims to be an answer")
	}
	// And a registry that was never probed says so for every feature.
	r := NewRegistry()
	for _, f := range []Feature{FeatMesh, FeatWirelessUplink, FeatSurvey,
		FeatNeighborReport, FeatDSA} {
		if r.State(f).Decided() {
			t.Errorf("%s reports a decision on an unprobed registry", f)
		}
	}
}
