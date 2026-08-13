package capability

import "testing"

// The rule the whole package exists to enforce. tools/probe.py once reported
// "no DSA" and "legacy iptables" for a device that had both, because a denied
// check was recorded as a negative answer. Only Present may render.
func TestOnlyPresentIsBuildable(t *testing.T) {
	for _, s := range []State{Unknown, Absent, NotObservable} {
		if s.Buildable() {
			t.Errorf("%s must not be buildable", s)
		}
	}
	if !Present.Buildable() {
		t.Error("Present must be buildable")
	}
}

func TestCanIsStrict(t *testing.T) {
	r := NewRegistry()
	r.Set(FeatDSA, NotObservable)
	if r.Can(FeatDSA) {
		t.Fatal("NotObservable must never satisfy Can(); that is how a screen " +
			"gets deleted from hardware that supports it")
	}
	r.Set(FeatDSA, Present)
	if !r.Can(FeatDSA) {
		t.Fatal("Present should satisfy Can()")
	}
}

func TestUnobservableIsReportedSoTheACLCanBeWidened(t *testing.T) {
	r := NewRegistry()
	r.Set(FeatDSA, Present)
	r.Set(FeatFirewall4, NotObservable)
	r.Set(FeatAccounting, NotObservable)
	r.Set(FeatSurvey, Absent)

	got := r.Unobservable()
	if len(got) != 2 {
		t.Fatalf("want 2 unobservable features, got %v", got)
	}
	if got[0] != FeatFirewall4 || got[1] != FeatAccounting {
		t.Fatalf("expected a sorted list of the unobservable ones, got %v", got)
	}
}

func TestQuirksDeduplicate(t *testing.T) {
	r := NewRegistry()
	q := Quirk{Source: "iwinfo.survey", Field: "noise", Reason: "unsigned"}
	r.AddQuirk(q)
	r.AddQuirk(q)
	if len(r.Quirks) != 1 {
		t.Fatalf("a quirk seen on every radio should be recorded once, got %d", len(r.Quirks))
	}
	if !r.HasQuirk("iwinfo.survey", "noise") {
		t.Fatal("HasQuirk should find a recorded quirk")
	}
	if r.HasQuirk("iwinfo.info", "noise") {
		t.Fatal("the same field from a different source is a different quirk — " +
			"iwinfo.info's noise is correct, iwinfo.survey's is not")
	}
}

// Marketing names are ambiguous: "AX3000" spans MT7621 (class C, which sets the
// budget) and MT7981 (class B). Classification must key on the SoC target.
func TestClassifyKeysOnTargetNotMarketing(t *testing.T) {
	cases := []struct {
		target string
		want   Class
	}{
		{"mvebu/cortexa9", ClassA},
		{"mediatek/filogic", ClassB},
		{"ramips/mt7621", ClassC},
		{"ath79/generic", ClassUnknown},
	}
	for _, tc := range cases {
		if got := classify(Board{Target: tc.target}); got != tc.want {
			t.Errorf("classify(%q) = %s, want %s", tc.target, got, tc.want)
		}
	}
}

func TestSummaryNamesUnobservableSeparately(t *testing.T) {
	r := NewRegistry()
	r.Board = Board{Model: "Linksys WRT3200ACM"}
	r.Class = ClassA
	r.Set(FeatDSA, Present)
	r.Set(FeatAirtimeSplit, Absent)
	r.Set(FeatAccounting, NotObservable)

	s := r.Summary()
	for _, want := range []string{"class A", "has: dsa", "lacks: airtime-split",
		"UNOBSERVABLE: per-client-accounting"} {
		if !contains(s, want) {
			t.Errorf("summary %q should contain %q", s, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
