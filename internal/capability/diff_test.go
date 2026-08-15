package capability

import (
	"errors"
	"testing"
)

func reg(feats map[Feature]State) *Registry {
	r := NewRegistry()
	for f, s := range feats {
		r.Set(f, s)
	}
	return r
}

// The rule the whole file exists for: losing sight of a capability is not the
// device losing it.
//
// tools/probe.py collapsed NotObservable into Absent and reported "no DSA" for
// a device with a DSA switch. A diff that collapses it reports the same lie as
// an event, with a timestamp, which is worse — it looks like news.
func TestDiffDoesNotReportLostVisibilityAsLostCapability(t *testing.T) {
	old := reg(map[Feature]State{FeatHostapdControl: Present})
	new := reg(map[Feature]State{FeatHostapdControl: NotObservable})

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %v", len(changes), changes)
	}
	c := changes[0]
	if c.Effect != EffectHidden {
		t.Errorf("effect = %q, want %q — present->not-observable is a narrowed "+
			"ACL, not hardware that vanished", c.Effect, EffectHidden)
	}
	if c.Effect.Actionable() {
		t.Error("a visibility change must not be actionable: nothing about the " +
			"device changed, so nothing about the config should")
	}
	if len(Actionable(changes)) != 0 {
		t.Error("Actionable() included a visibility change")
	}
}

// And the converse: gaining sight of one is not the device gaining it.
func TestDiffDoesNotReportNewVisibilityAsAGain(t *testing.T) {
	old := reg(map[Feature]State{FeatDSA: NotObservable})
	new := reg(map[Feature]State{FeatDSA: Present})

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %v", len(changes), changes)
	}
	if got := changes[0].Effect; got != EffectVisible {
		t.Errorf("effect = %q, want %q — the previous probe was refused, not "+
			"answered, so the device may have had DSA all along", got, EffectVisible)
	}
}

// A real loss must still be reported as one, or the caution above becomes a
// blanket excuse and the diff reports nothing at all.
func TestDiffReportsGenuineGainsAndLosses(t *testing.T) {
	old := reg(map[Feature]State{FeatDSA: Absent, FeatFirewall4: Present})
	new := reg(map[Feature]State{FeatDSA: Present, FeatFirewall4: Absent})

	changes := Diff(old, new)
	byName := map[string]Change{}
	for _, c := range changes {
		byName[c.Name] = c
	}
	if got := byName[string(FeatDSA)].Effect; got != EffectGained {
		t.Errorf("absent->present = %q, want %q", got, EffectGained)
	}
	if got := byName[string(FeatFirewall4)].Effect; got != EffectLost {
		t.Errorf("present->absent = %q, want %q", got, EffectLost)
	}
	if len(Actionable(changes)) != 2 {
		t.Errorf("both are actionable; got %d", len(Actionable(changes)))
	}
}

// The first probe is not a device gaining everything it has.
func TestDiffAgainstNilIsFirstObservation(t *testing.T) {
	new := reg(map[Feature]State{FeatDSA: Present, FeatSurvey: NotObservable})
	for _, c := range Diff(nil, new) {
		if c.Effect != EffectFirst {
			t.Errorf("%s: effect = %q, want %q — a device does not gain a "+
				"feature by being looked at for the first time",
				c.Name, c.Effect, EffectFirst)
		}
		if c.Effect.Actionable() {
			t.Errorf("%s: a first observation is not actionable", c.Name)
		}
	}
}

// An unchanged registry produces no changes at all — otherwise every scheduled
// re-probe logs an event and the log stops meaning anything.
func TestDiffOfIdenticalRegistriesIsEmpty(t *testing.T) {
	r := reg(map[Feature]State{FeatDSA: Present, FeatSurvey: Absent})
	r.Radios = []Radio{{Phy: "phy0", Device: "phy0-ap0", HWModes: []string{"a", "n", "ac"}}}
	r.Ports = Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2"}}
	r.Board.Release = "OpenWrt 24.10.0"
	r.Class = ClassA

	other := reg(map[Feature]State{FeatDSA: Present, FeatSurvey: Absent})
	other.Radios = []Radio{{Phy: "phy0", Device: "phy0-ap0", HWModes: []string{"a", "n", "ac"}}}
	other.Ports = Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2"}}
	other.Board.Release = "OpenWrt 24.10.0"
	other.Class = ClassA

	if got := Diff(r, other); len(got) != 0 {
		t.Errorf("identical registries differ: %v", got)
	}
}

// Radios are keyed on the phy, not the interface.
//
// `phy0-ap0` is an interface this controller creates and removes itself, so
// keying on it reports the radio as lost every time an SSID is deleted.
func TestDiffKeysRadiosOnThePhyNotTheInterface(t *testing.T) {
	old := NewRegistry()
	old.Radios = []Radio{{Phy: "phy0", Device: "phy0-ap0", HWModes: []string{"g", "n"}}}
	new := NewRegistry()
	new.Radios = []Radio{{Phy: "phy0", Device: "phy0-ap1", HWModes: []string{"g", "n"}}}

	if got := Diff(old, new); len(got) != 0 {
		t.Errorf("renaming the AP interface reported %v; the phy is the hardware", got)
	}
}

func TestDiffReportsRadioAppearingAndDisappearing(t *testing.T) {
	old := NewRegistry()
	old.Radios = []Radio{{Phy: "phy0", HWModes: []string{"g", "n"}}}
	new := NewRegistry()
	new.Radios = []Radio{
		{Phy: "phy0", HWModes: []string{"g", "n"}},
		{Phy: "phy1", HWModes: []string{"a", "n", "ac"}},
	}

	changes := Diff(old, new)
	if len(changes) != 1 || changes[0].Name != "phy1" ||
		changes[0].Effect != EffectGained {
		t.Fatalf("a new radio was reported as %v", changes)
	}
	// And the other direction.
	back := Diff(new, old)
	if len(back) != 1 || back[0].Name != "phy1" || back[0].Effect != EffectLost {
		t.Fatalf("a removed radio was reported as %v", back)
	}
}

// The port map feeds the network renderer, which tags VLANs onto these exact
// names. A firmware that renames them changes what an applied bridge-vlan
// means, so it cannot pass silently.
func TestDiffReportsPortLayoutChanges(t *testing.T) {
	old := NewRegistry()
	old.Ports = Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2", "lan3", "lan4"}}
	new := NewRegistry()
	new.Ports = Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2"}}

	changes := Diff(old, new)
	if len(changes) != 1 || changes[0].Kind != "ports" ||
		!changes[0].Effect.Actionable() {
		t.Fatalf("port layout change reported as %v", changes)
	}
}

// The class sets the poll budget, so it is not a label.
func TestDiffReportsClassChange(t *testing.T) {
	old := NewRegistry()
	old.Class = ClassA
	new := NewRegistry()
	new.Class = ClassC

	changes := Diff(old, new)
	if len(changes) != 1 || changes[0].Kind != "class" ||
		!changes[0].Effect.Actionable() {
		t.Fatalf("class change reported as %v", changes)
	}
}

// An idle channel demonstrates nothing about the airtime counters, and must not
// be recorded as demonstrating they are broken.
//
// Found by diffing two probes rather than by reading the code. splitOK starts
// at Absent and the idle-channel branch fell through to it, so on a driver
// whose counters work the feature came out Present when the channel happened to
// be busy and Absent when it happened to be quiet. Re-probing then reported the
// device gaining and losing airtime-split at random, with a warning each time.
// The reference device hid it: mwlwifi's counters are genuinely broken, so it
// is stably Absent for a real reason.
func TestIdleChannelDoesNotProveTheAirtimeSplitIsBroken(t *testing.T) {
	// busy_time did not advance: the channel was quiet for the whole sample.
	idle := judgeSplit(
		surveyRow{BusyTime: 1000, ActiveTime: 5000, RxTime: 400, TxTime: 200},
		surveyRow{BusyTime: 1000, ActiveTime: 9000, RxTime: 400, TxTime: 200},
		nil)
	if idle != splitUndemonstrated {
		t.Errorf("an idle channel judged %v, want splitUndemonstrated — quiet "+
			"counters prove nothing about counters that would move", idle)
	}

	// The second read failed: likewise nothing demonstrated.
	if got := judgeSplit(surveyRow{}, surveyRow{}, errNoSecondSample); got != splitUndemonstrated {
		t.Errorf("a failed second sample judged %v, want splitUndemonstrated", got)
	}

	// Counters that track busy time: usable.
	if got := judgeSplit(
		surveyRow{BusyTime: 1000, RxTime: 400, TxTime: 200},
		surveyRow{BusyTime: 3000, RxTime: 1600, TxTime: 800},
		nil); got != splitUsable {
		t.Errorf("proportional counters judged %v, want splitUsable", got)
	}

	// Counters that do not move while busy time climbs: demonstrably broken,
	// which is a real Absent and must stay one.
	if got := judgeSplit(
		surveyRow{BusyTime: 1000, RxTime: 0, TxTime: 100},
		surveyRow{BusyTime: 4000, RxTime: 0, TxTime: 102},
		nil); got != splitBroken {
		t.Errorf("stuck counters judged %v, want splitBroken", got)
	}
}

var errNoSecondSample = errors.New("second sample failed")
