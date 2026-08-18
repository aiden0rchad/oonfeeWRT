package capability

import (
	"errors"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

var errBroken = errors.New("malformed batch response")

func denied() error { return &ubus.StatusError{Status: ubus.StatusPermissionDenied} }

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

// A member call the ACL refused says nothing about whether the device batches.
//
// The batch was carried, answered and correlated — which is exactly what this
// check tests — and one probe inside it was not granted. Recorded as Absent
// that reads as "this device mishandles batches" and drops every poll to
// sequential calls for the life of the adoption, which is a verdict spent
// silently, forever, on evidence that was never gathered.
func TestBatchingIsNotJudgedByARefusedMemberCall(t *testing.T) {
	if got := batchVerdict(2, denied(), nil); got != NotObservable {
		t.Errorf("a denied member call gave %s, want not-observable", got)
	}
	if got := batchVerdict(2, nil, denied()); got != NotObservable {
		t.Errorf("a denied second member call gave %s, want not-observable", got)
	}
	// A device that genuinely mishandles the batch is still Absent, or the
	// sequential-poll fallback could never be selected.
	if got := batchVerdict(1, nil, nil); got != Absent {
		t.Errorf("a short batch gave %s, want absent", got)
	}
	if got := batchVerdict(2, errBroken, nil); got != Absent {
		t.Errorf("a failed member call gave %s, want absent", got)
	}
	if got := batchVerdict(2, nil, nil); got != Present {
		t.Errorf("a clean batch gave %s, want present", got)
	}
}
