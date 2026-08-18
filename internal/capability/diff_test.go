package capability

import (
	"errors"
	"strings"
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

// A radio that is switched off says nothing about whether its driver can
// survey.
//
// Same defect as the airtime split, found by auditing for it rather than by
// tripping over it: `active_time == 0` fell through to an Absent default, so a
// device with every radio disabled reported that its driver cannot do channel
// utilization. Enabling a radio would then make the device appear to GAIN the
// feature. The reference hardware never exercised it — both radios are up, and
// one radio with active time is enough to set the device-wide state.
func TestASwitchedOffRadioDoesNotProveTheDriverCannotSurvey(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  surveyRow
		err  error
		want surveyJudgement
	}{
		{"radio off", surveyRow{ActiveTime: 0}, nil, surveyIdle},
		{"no rows at all", surveyRow{}, nil, surveyIdle},
		{"counters running", surveyRow{ActiveTime: 5000, BusyTime: 900}, nil, surveyUsable},
		// A real failure IS a determination: the driver was asked and could not.
		{"driver failed", surveyRow{}, errNoSecondSample, surveyUnsupported},
	} {
		if got := judgeSurvey(tc.row, tc.err); got != tc.want {
			t.Errorf("%s: judged %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The accumulator that makes the rule structural.
//
// Three features on the radio path had the same bug written three times: a
// State initialised to Absent and at least one branch that reached the end
// without demonstrating anything. This is that rule in one place, so the next
// feature added to probeRadios cannot reintroduce it by omission.
func TestVerdictNeverInventsAnAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(v *verdict)
		want State
	}{
		{"nothing demonstrated, one radio could not tell",
			func(v *verdict) { v.undecided() }, NotObservable},
		{"nothing demonstrated, one check refused",
			func(v *verdict) { v.refuse() }, NotObservable},
		{"one radio has it",
			func(v *verdict) { v.demonstrated(Present) }, Present},
		{"one radio demonstrably lacks it",
			func(v *verdict) { v.demonstrated(Absent) }, Absent},
		// Present wins: one radio that has it settles the device.
		{"one has it, another could not tell", func(v *verdict) {
			v.demonstrated(Present)
			v.undecided()
		}, Present},
		{"one has it, another lacks it", func(v *verdict) {
			v.demonstrated(Absent)
			v.demonstrated(Present)
		}, Present},
		// Evidence beats the absence of evidence.
		{"one demonstrably lacks it, another could not tell", func(v *verdict) {
			v.demonstrated(Absent)
			v.undecided()
		}, Absent},
		// Nothing was recorded at all: the device reported no radios, so there
		// is genuinely nothing here. Absent, not "re-probe your switch".
		{"no radios at all", func(*verdict) {}, Absent},
	} {
		var v verdict
		tc.fill(&v)
		if got := v.state(); got != tc.want {
			t.Errorf("%s: state = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The package-name parsing that the mesh probe rests on.
//
// apk glues the version onto the name with a hyphen, so taking the name up to
// the first hyphen truncates "wpad-mesh-openssl" to "wpad" — which would report
// a mesh-capable device as one whose build cannot be classified.
func TestPackageNamesSurviveBothManagersFormats(t *testing.T) {
	apk := `wpad-mesh-openssl-2025.08.26~ca266cc2-r2 arm_cortex-a9_vfpv3-d16 {feeds/base} (BSD-3-Clause) [installed]
hostapd-common-2025.08.26~ca266cc2-r2 arm_cortex-a9_vfpv3-d16 {feeds/base} (BSD-3-Clause) [installed]`
	got := packageNames(apk)
	want := []string{"wpad-mesh-openssl", "hostapd-common"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("apk names = %v, want %v", got, want)
		}
	}

	opkg := "wpad-basic-mbedtls - 2024.03.15-r1\nkmod-cfg80211 - 6.6.63-r1"
	got = packageNames(opkg)
	if len(got) != 2 || got[0] != "wpad-basic-mbedtls" {
		t.Fatalf("opkg names = %v", got)
	}
}

// The mesh determination, over the shapes a real fleet produces.
func TestMeshVerdictFromInstalledPackages(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkgs []string
		want State
	}{
		// The reference device.
		{"wpad-mesh build", []string{"hostapd-common", "wpad-mesh-openssl"}, Present},
		// Named for lacking it: OpenWrt's default on constrained targets.
		{"wpad-basic build", []string{"wpad-basic-mbedtls"}, Absent},
		{"wpad-mini build", []string{"wpad-mini"}, Absent},
		// No 802.11 daemon at all: it cannot run an AP either.
		{"no wpad or hostapd", []string{"kmod-cfg80211", "luci"}, Absent},
		// A full build whose name does not settle its feature set. Claiming
		// Present here would be a guess from a package name — exactly what this
		// package refuses to do.
		{"unnamed full build", []string{"wpad-openssl"}, NotObservable},
	} {
		if got := meshFromPackages(tc.pkgs); got != tc.want {
			t.Errorf("%s: %v -> %s, want %s", tc.name, tc.pkgs, got, tc.want)
		}
	}
}

// The daemon supporting mesh does not mean the radio can run one.
//
// Measured on the reference device: mwlwifi advertises "mesh point" in its
// supported interface modes AND permits it in its interface combinations, so
// every source a controller can consult says yes. Configure one and the driver
// refuses to bring the interface UP — uci accepts the config, the apply's
// health check passes, the confirm lands, and the mesh does not exist.
//
// A package list alone would report that as available.
func TestAMarvellRadioGatesMeshOffDespiteTheDaemonSupportingIt(t *testing.T) {
	r := NewRegistry()
	r.Radios = []Radio{{Phy: "phy0", Hardware: "Marvell 88W8964"}}
	if got := marvellRadio(r); got == "" {
		t.Fatal("the Marvell radio was not recognised")
	}

	// And a radio that is not one is left alone: this is a measured quirk of
	// one driver, not a blanket refusal.
	other := NewRegistry()
	other.Radios = []Radio{{Phy: "phy0", Hardware: "MediaTek MT7915E"}}
	if got := marvellRadio(other); got != "" {
		t.Errorf("a non-Marvell radio was gated: %q", got)
	}

	// Case-insensitively, since hardware names are free text from the driver.
	lower := NewRegistry()
	lower.Radios = []Radio{{Phy: "phy0", Hardware: "marvell 88w8964"}}
	if marvellRadio(lower) == "" {
		t.Error("the match is case-sensitive; hardware names are free text")
	}
}

// A radio that reports the same modes under a new name is the same radio.
//
// The identifier is not stable — it comes from how the probe enumerates radios,
// and that changed under this project once already. Every radio on every device
// was then reported as lost AND gained, and the loss carried "WLANs targeted at
// its band will not render on this device" about hardware that was working and
// did render. A firmware upgrade that renames interfaces would do this to any
// user.
func TestARenamedRadioIsNotReportedAsLost(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{
		{Phy: "radio0", HWModes: []string{"n", "ac"}},
		{Phy: "radio1", HWModes: []string{"b", "g", "n"}},
	}
	after := NewRegistry()
	after.Radios = []Radio{
		{Phy: "phy0", HWModes: []string{"n", "ac"}},
		{Phy: "phy1", HWModes: []string{"b", "g", "n"}},
	}

	for _, c := range Diff(before, after) {
		if c.Kind != "radio" {
			continue
		}
		if c.Effect == EffectLost {
			t.Errorf("a renamed radio was reported as lost: %s", c.Detail)
		}
		if c.Effect == EffectGained {
			t.Errorf("a renamed radio was reported as newly appeared: %s", c.Detail)
		}
		if strings.Contains(c.Detail, "will not render") {
			t.Errorf("a rename claimed WLANs would stop rendering: %s", c.Detail)
		}
	}
}

// But a radio that genuinely goes away must still be reported. The rename
// pairing must not become a way to lose a real loss.
func TestARadioThatGenuinelyVanishesIsStillReported(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{
		{Phy: "phy0", HWModes: []string{"n", "ac"}},
		{Phy: "phy1", HWModes: []string{"b", "g", "n"}},
	}
	after := NewRegistry()
	after.Radios = []Radio{{Phy: "phy1", HWModes: []string{"b", "g", "n"}}}

	var lost bool
	for _, c := range Diff(before, after) {
		if c.Kind == "radio" && c.Effect == EffectLost {
			lost = true
		}
	}
	if !lost {
		t.Error("a radio that disappeared with no replacement was not reported")
	}
}

// A rename must not be offered as the probable cause of anything.
//
// Reporting it as EffectChanged made it Actionable, and the capability-cause
// panel then suggested a rename — which the code itself describes as changing
// nothing about what the radio can carry — as the explanation for a WLAN that
// did not render.
func TestARenameIsNotActionable(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{{Phy: "radio0", HWModes: []string{"n", "ac"}}}
	after := NewRegistry()
	after.Radios = []Radio{{Phy: "phy0", HWModes: []string{"n", "ac"}}}

	var seen bool
	for _, c := range Diff(before, after) {
		if c.Kind != "radio" {
			continue
		}
		seen = true
		if c.Effect != EffectRenamed {
			t.Errorf("a rename reported as %q", c.Effect)
		}
		if c.Effect.Actionable() {
			t.Error("a rename was marked actionable, so it will be offered as " +
				"the cause of something it cannot have caused")
		}
	}
	if !seen {
		t.Fatal("the rename was not reported at all")
	}
}

// A radio that disappears from a record that never described it is not a lost
// radio.
//
// The rename pairing compares HWModes, and its guard tests only the NEW side
// (len(a.HWModes) == 0). When the OLD record has no modes there is nothing to
// pair on, so every radio came out as lost AND gained — which is exactly the
// failure the pairing was written to prevent.
//
// Seen on the reference WRT3200ACM: an earlier probe recorded radio0/radio1
// with a band and no modes, a later one recorded phy0/phy1 with modes, and the
// apply preview told the operator "radio radio0 is gone. WLANs targeted at its
// band will not render on this device" about a radio that was up and carrying
// oonfee-roam — offered as the probable cause of unrelated omissions.
func TestARadioThatVanishesWithNoModeEvidenceIsNotCalledLost(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{
		{Device: "radio0", Phy: "radio0", Band: "5g"}, // no HWModes
		{Device: "radio1", Phy: "radio1", Band: "2g"},
	}
	after := NewRegistry()
	after.Radios = []Radio{
		{Device: "phy0-ap0", Phy: "phy0", HWModes: []string{"n", "ac"}},
		{Device: "phy1-ap0", Phy: "phy1", HWModes: []string{"b", "g", "n"}},
	}
	var lost, ambiguous int
	for _, c := range Diff(before, after) {
		if c.Kind != "radio" {
			continue
		}
		switch c.Effect {
		case EffectLost:
			lost++
			t.Errorf("claimed a loss with no evidence: %q", c.Detail)
		case EffectAmbiguous:
			ambiguous++
			if c.Effect.Actionable() {
				t.Error("an ambiguous change must not be actionable: it would " +
					"be offered as the probable cause of missing config")
			}
		}
	}
	if lost != 0 {
		t.Errorf("%d radios reported as gone", lost)
	}
	if ambiguous != 2 {
		t.Errorf("ambiguous = %d, want 2", ambiguous)
	}
}

// And a radio that really does go, from a record that DID describe it, is
// still reported as lost — or the fix is just a way of never saying anything.
func TestARadioThatGenuinelyDisappearsIsStillLost(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{
		{Device: "phy0-ap0", Phy: "phy0", HWModes: []string{"n", "ac"}},
		{Device: "phy1-ap0", Phy: "phy1", HWModes: []string{"b", "g", "n"}},
	}
	after := NewRegistry()
	after.Radios = []Radio{
		{Device: "phy1-ap0", Phy: "phy1", HWModes: []string{"b", "g", "n"}},
	}
	var lost int
	for _, c := range Diff(before, after) {
		if c.Kind == "radio" && c.Effect == EffectLost {
			lost++
			if !c.Effect.Actionable() {
				t.Error("a real loss must stay actionable")
			}
		}
	}
	if lost != 1 {
		t.Errorf("lost = %d, want 1 — a radio that really went must still be reported", lost)
	}
}

// The rename path still works when both sides carry modes.
func TestARenameWithModesOnBothSidesIsStillARename(t *testing.T) {
	before := NewRegistry()
	before.Radios = []Radio{{Device: "radio0", Phy: "radio0", HWModes: []string{"n", "ac"}}}
	after := NewRegistry()
	after.Radios = []Radio{{Device: "phy0-ap0", Phy: "phy0", HWModes: []string{"n", "ac"}}}
	var renamed int
	for _, c := range Diff(before, after) {
		if c.Kind == "radio" && c.Effect == EffectRenamed {
			renamed++
		}
		if c.Kind == "radio" && c.Effect == EffectLost {
			t.Errorf("a rename with mode evidence was reported as a loss: %q", c.Detail)
		}
	}
	if renamed != 1 {
		t.Errorf("renamed = %d, want 1", renamed)
	}
}
