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

// Marketing names are ambiguous: classification keys on the target or the
// actual SoC string, never the product name. ath79/generic alone says too
// little; QCA956X is classified because that exact silicon was measured.
func TestClassifyKeysOnSiliconNotMarketing(t *testing.T) {
	cases := []struct {
		name  string
		board Board
		want  Class
	}{
		{"mvebu", Board{Target: "mvebu/cortexa9"}, ClassA},
		{"filogic", Board{Target: "mediatek/filogic"}, ClassB},
		{"mt7621", Board{Target: "ramips/mt7621"}, ClassC},
		{"generic ath79", Board{Target: "ath79/generic"}, ClassUnknown},
		{"measured QCA956X", Board{Target: "ath79/generic", System: "Qualcomm Atheros QCA956X ver 1 rev 0"}, ClassC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.board); got != tc.want {
				t.Errorf("classify(%+v) = %s, want %s", tc.board, got, tc.want)
			}
		})
	}
}

func TestSwitchPortCapabilityUsesDSAOrLegacySwitch(t *testing.T) {
	cases := []struct {
		dsa, legacy, want State
	}{
		{Present, Absent, Present},
		{Absent, Present, Present},
		{Absent, Absent, Absent},
		{NotObservable, Absent, NotObservable},
		{Absent, NotObservable, NotObservable},
	}
	for _, tc := range cases {
		if got := switchPortState(tc.dsa, tc.legacy); got != tc.want {
			t.Errorf("switchPortState(%s, %s) = %s, want %s",
				tc.dsa, tc.legacy, got, tc.want)
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
