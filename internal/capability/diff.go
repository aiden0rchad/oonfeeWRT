package capability

import (
	"fmt"
	"sort"
	"strings"
)

// Comparing two probes of the same device.
//
// # Why this is not a struct comparison
//
// The three-state model makes most of this file. A feature that goes from
// Present to Absent is a device that lost something. A feature that goes from
// Present to NotObservable is a device we lost sight of — the ACL narrowed, a
// binary went missing, a method stopped being granted. The hardware may be
// entirely unchanged. Reporting the second as "lost 802.11r" would be exactly
// the failure the three-state model exists to prevent, restated one level up:
// tools/probe.py collapsed NotObservable into Absent and reported "no DSA" for
// a device with a DSA switch, and a diff that collapses it reports the same lie
// as an event, with a timestamp, which is worse because it looks like news.
//
// So every transition is classified by what it licenses a reader to conclude,
// and Effect — not the raw states — is what the UI and the event log render.

// Effect is what a change licenses a reader to conclude.
type Effect string

const (
	// EffectGained: demonstrably has something it demonstrably lacked.
	EffectGained Effect = "gained"
	// EffectLost: demonstrably lacks something it demonstrably had.
	EffectLost Effect = "lost"
	// EffectVisible: we can now check something we previously could not. The
	// device may have had it all along.
	EffectVisible Effect = "now-observable"
	// EffectHidden: we can no longer check something. NOT a loss of capability
	// — this is the one that must never be reported as a loss.
	EffectHidden Effect = "no-longer-observable"
	// EffectFirst: the first determination, from no prior knowledge.
	EffectFirst Effect = "first-observation"
	// EffectRenamed: the same thing under a new identifier, with nothing about
	// what it can do different.
	//
	// Its own category because none of the others is true of it. It is not a
	// gain, not a loss, and not a visibility change — we could see it before
	// and we can see it now. Reporting it as EffectChanged made it Actionable,
	// which offered a rename as the probable cause of a WLAN that did not
	// render; reporting it as now-observable would assert a visibility change
	// that did not happen.
	EffectRenamed Effect = "renamed"
	// EffectChanged: a value that is not three-state — a radio, the class, the
	// port map, the firmware string.
	EffectChanged Effect = "changed"
	// EffectAmbiguous: the available identity or capability evidence cannot
	// determine which concrete change occurred.
	//
	// Deliberately NOT actionable. It is the honest answer when matching
	// evidence cannot establish unique continuity, and calling it a loss puts "WLANs
	// targeted at its band will not render on this device" against hardware
	// that is present and broadcasting.
	EffectAmbiguous Effect = "ambiguous"
)

// Actionable reports whether a change alters what the controller may render or
// send. Visibility changes do not: the device is the same device.
func (e Effect) Actionable() bool {
	return e == EffectGained || e == EffectLost || e == EffectChanged
}

// Change is one difference between two probes.
type Change struct {
	Kind   string `json:"kind"` // feature, radio, class, ports, firmware, quirk
	Name   string `json:"name"`
	From   string `json:"from"`
	To     string `json:"to"`
	Effect Effect `json:"effect"`
	// Detail is the sentence an operator reads. It exists because "hostapd
	// -control: present -> not-observable" is not something anybody should have
	// to interpret at the moment they are trying to work out whether their AP
	// is broken.
	Detail string `json:"detail"`
}

func (c Change) String() string {
	return fmt.Sprintf("%s %s: %s", c.Kind, c.Name, c.Detail)
}

// Diff compares an earlier registry with a later one.
//
// old may be nil, which is the first probe: everything determined is reported
// as EffectFirst rather than as a gain, because a device did not gain a radio
// by being looked at for the first time.
func Diff(old, new *Registry) []Change {
	var out []Change
	if new == nil {
		return nil
	}

	// Features.
	names := map[Feature]bool{}
	for f := range new.Features {
		names[f] = true
	}
	if old != nil {
		for f := range old.Features {
			names[f] = true
		}
	}
	keys := make([]Feature, 0, len(names))
	for f := range names {
		keys = append(keys, f)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, f := range keys {
		var before State
		if old != nil {
			before = old.State(f)
		}
		after := new.State(f)
		if before == after {
			continue
		}
		if c, ok := featureChange(f, before, after); ok {
			out = append(out, c)
		}
	}

	// Firmware. Reported even though the collector already logs it, because a
	// capability diff whose cause is missing reads as spontaneous.
	if old != nil && old.Board.Release != new.Board.Release {
		out = append(out, Change{
			Kind: "firmware", Name: "release", Effect: EffectChanged,
			From: old.Board.Release, To: new.Board.Release,
			Detail: fmt.Sprintf("firmware changed from %q to %q",
				old.Board.Release, new.Board.Release),
		})
	}

	// Class drives poll cadence, so a change here changes what the controller
	// costs the device — not a label.
	if old != nil && old.Class != new.Class {
		out = append(out, Change{
			Kind: "class", Name: "device-class", Effect: EffectChanged,
			From: string(old.Class), To: string(new.Class),
			Detail: fmt.Sprintf("hardware class changed from %s to %s, which "+
				"changes the poll budget this device is held to",
				old.Class, new.Class),
		})
	}

	out = append(out, radioChanges(old, new)...)
	out = append(out, portChanges(old, new)...)
	return out
}

// featureChange classifies one feature transition.
func featureChange(f Feature, before, after State) (Change, bool) {
	c := Change{Kind: "feature", Name: string(f),
		From: before.String(), To: after.String()}

	switch {
	case before == Unknown:
		// No prior determination. Even a Present here is not a gain.
		if after == Unknown {
			return c, false
		}
		c.Effect = EffectFirst
		c.Detail = fmt.Sprintf("%s: first determination, %s", f, after)
		if after == NotObservable {
			c.Detail = fmt.Sprintf("%s could not be checked on this device", f)
		}

	case after == NotObservable:
		// The one that must not read as a loss.
		c.Effect = EffectHidden
		c.Detail = fmt.Sprintf("%s can no longer be checked (it was %s). This "+
			"is a change in what the controller can see, not in what the "+
			"device has — usually a narrowed ACL or a removed binary. The "+
			"feature is treated as unavailable because acting on an "+
			"unverifiable capability is how a screen offers a control the "+
			"device rejects", f, before)

	case before == NotObservable:
		c.Effect = EffectVisible
		c.Detail = fmt.Sprintf("%s can be checked now, and is %s. It may have "+
			"been %s all along — the previous probe was refused, not answered",
			f, after, after)

	case before == Absent && after == Present:
		c.Effect = EffectGained
		c.Detail = fmt.Sprintf("%s is now available on this device", f)

	case before == Present && after == Absent:
		c.Effect = EffectLost
		c.Detail = fmt.Sprintf("%s is gone. Anything configured against it "+
			"will not render until it returns", f)

	default:
		c.Effect = EffectChanged
		c.Detail = fmt.Sprintf("%s went from %s to %s", f, before, after)
	}
	return c, true
}

// radioChanges reports radios appearing, disappearing, or changing supported modes.
//
// Explicit configured section identity wins per record. Unmatched keyed and
// legacy records bridge by PHY only when the correspondence is unique; two
// authoritative sections never become the same radio merely because they share
// a PHY. Section and PHY identities occupy separate internal namespaces.
//
// # What is deliberately NOT compared
//
// Channel and Frequency: a radio that moved channel under ACS has not changed
// what it can do, and reporting it would fire a capability-change event every
// time the driver picked a cleaner channel.
//
// NoiseStable: it is decided by sampling the noise floor twice, so it can
// legitimately differ between two probes of an unchanged radio. Diffing a
// sampled property means every probe reports churn, and churn arriving right
// after a firmware upgrade would be read as the upgrade's doing. The
// measurement is still stored — it is just not news.
//
// This is what makes probing an unchanged device return nothing, which is the
// property the hardware test asserts.
func radioChanges(old, new *Registry) []Change {
	if old == nil {
		return nil
	}
	before, after := old.Radios, new.Radios
	beforeMatch := make([]int, len(before))
	afterMatch := make([]int, len(after))
	for i := range beforeMatch {
		beforeMatch[i] = -1
	}
	for i := range afterMatch {
		afterMatch[i] = -1
	}
	type pair struct{ before, after int }
	var pairs []pair
	match := func(bi, ai int) {
		beforeMatch[bi], afterMatch[ai] = ai, bi
		pairs = append(pairs, pair{before: bi, after: ai})
	}

	// Authoritative identity is exact and takes precedence over every
	// compatibility path. Pair records rather than putting them in a map so a
	// malformed duplicate cannot silently overwrite another record.
	for bi, b := range before {
		if b.Section == "" {
			continue
		}
		bkey, _ := radioComparisonIdentity(b)
		for ai, a := range after {
			if afterMatch[ai] >= 0 || a.Section == "" {
				continue
			}
			akey, _ := radioComparisonIdentity(a)
			if bkey == akey {
				match(bi, ai)
				break
			}
		}
	}

	// Compatibility bridge for records captured before Section existed, or
	// from an interface-only fallback. Mutual uniqueness is required: with two
	// configured radios on one PHY, PHY alone does not say which is which.
	beforeCandidate, beforeCandidates := make([]int, len(before)), make([]int, len(before))
	afterCandidate, afterCandidates := make([]int, len(after)), make([]int, len(after))
	for bi, b := range before {
		if beforeMatch[bi] >= 0 {
			continue
		}
		beforeCandidate[bi] = -1
		for ai, a := range after {
			if afterMatch[ai] >= 0 || !radioPHYCompatible(b, a) {
				continue
			}
			beforeCandidate[bi] = ai
			beforeCandidates[bi]++
		}
	}
	for ai, a := range after {
		if afterMatch[ai] >= 0 {
			continue
		}
		afterCandidate[ai] = -1
		for bi, b := range before {
			if beforeMatch[bi] >= 0 || !radioPHYCompatible(b, a) {
				continue
			}
			afterCandidate[ai] = bi
			afterCandidates[ai]++
		}
	}
	for bi, ai := range beforeCandidate {
		if beforeMatch[bi] < 0 && beforeCandidates[bi] == 1 &&
			afterCandidates[ai] == 1 && afterCandidate[ai] == bi {
			match(bi, ai)
		}
	}

	var out []Change
	for _, p := range pairs {
		b, a := before[p.before], after[p.after]
		// An absent mode list is unresolved evidence, not proof that the
		// hardware changed its supported modes.
		if len(b.HWModes) == 0 || len(a.HWModes) == 0 || sameModes(b.HWModes, a.HWModes) {
			continue
		}
		name := radioPairName(b, a)
		out = append(out, Change{
			Kind: "radio", Name: name, Effect: EffectChanged,
			From: strings.Join(b.HWModes, ","), To: strings.Join(a.HWModes, ","),
			Detail: fmt.Sprintf("radio %s changed the modes it reports, "+
				"from %s to %s, which can change which band it serves", name,
				strings.Join(b.HWModes, ","), strings.Join(a.HWModes, ",")),
		})
	}

	// A shared PHY with unmatched legacy records on both sides has persisted,
	// but its configured radios cannot be paired uniquely. Report that once as
	// uncertainty; definite per-record loss/gain claims would be invented.
	type phyGroup struct{ before, after []int }
	groups := map[string]*phyGroup{}
	group := func(phy string) *phyGroup {
		if groups[phy] == nil {
			groups[phy] = &phyGroup{}
		}
		return groups[phy]
	}
	for bi, b := range before {
		if beforeMatch[bi] < 0 && b.Phy != "" {
			g := group(b.Phy)
			g.before = append(g.before, bi)
		}
	}
	for ai, a := range after {
		if afterMatch[ai] < 0 && a.Phy != "" {
			g := group(a.Phy)
			g.after = append(g.after, ai)
		}
	}
	ambiguousBefore := make([]bool, len(before))
	ambiguousAfter := make([]bool, len(after))
	phys := make([]string, 0, len(groups))
	for phy := range groups {
		phys = append(phys, phy)
	}
	sort.Strings(phys)
	for _, phy := range phys {
		g := groups[phy]
		if len(g.before) == 0 || len(g.after) == 0 ||
			!radioGroupHasLegacy(before, after, g.before, g.after) {
			continue
		}
		for _, bi := range g.before {
			ambiguousBefore[bi] = true
		}
		for _, ai := range g.after {
			ambiguousAfter[ai] = true
		}
		from, to := radioGroupNames(before, g.before), radioGroupNames(after, g.after)
		out = append(out, Change{
			Kind: "radio", Name: phy, Effect: EffectAmbiguous,
			From: strings.Join(from, ","), To: strings.Join(to, ","),
			Detail: fmt.Sprintf("radios on %s cannot be matched uniquely across probes "+
				"(before: [%s], now: [%s]). The PHY is still present, so this does "+
				"not prove radio hardware was lost or gained — re-probe to settle "+
				"configured section identity", phy, strings.Join(from, ", "),
				strings.Join(to, ", ")),
		})
	}

	// Pair a radio that vanished with one that appeared carrying the same
	// modes, and call it what it is: a rename.
	//
	// The identifier is not stable. It comes from how the probe enumerates
	// radios, and that changed under this project once already — every radio on
	// every device was then reported as lost AND gained, with "WLANs targeted at
	// its band will not render on this device" attached to hardware that was
	// working and did render. A firmware upgrade that renames interfaces would
	// do the same to any user.
	//
	// Modes are the evidence: a radio that reports the same modes under a new
	// name is the same radio. Remaining records are classified below according
	// to whether their evidence proves loss, gain, or only uncertainty.
	renamedBefore := make([]bool, len(before))
	renamedAfter := make([]bool, len(after))
	type renamePair struct{ before, after int }
	var renames []renamePair
	for bi, b := range before {
		if beforeMatch[bi] >= 0 || ambiguousBefore[bi] {
			continue
		}
		for ai, a := range after {
			if afterMatch[ai] >= 0 || ambiguousAfter[ai] || renamedAfter[ai] || len(a.HWModes) == 0 {
				continue
			}
			if sameModes(b.HWModes, a.HWModes) {
				renamedBefore[bi], renamedAfter[ai] = true, true
				renames = append(renames, renamePair{before: bi, after: ai})
				break
			}
		}
	}
	for _, rename := range renames {
		b, a := before[rename.before], after[rename.after]
		fromName, toName := radioDisplayName(b), radioDisplayName(a)
		out = append(out, Change{
			Kind: "radio", Name: toName, Effect: EffectRenamed,
			From: fromName, To: toName,
			Detail: fmt.Sprintf("radio %s is now called %s. It reports the "+
				"same modes (%s), so this is the same hardware under a new "+
				"name and nothing about what it can carry has changed",
				fromName, toName, strings.Join(a.HWModes, ",")),
		})
	}
	for bi, b := range before {
		if beforeMatch[bi] >= 0 || ambiguousBefore[bi] || renamedBefore[bi] {
			continue
		}
		name := radioDisplayName(b)
		if len(b.HWModes) == 0 {
			out = append(out, Change{
				Kind: "radio", Name: name, Effect: EffectAmbiguous,
				Detail: fmt.Sprintf("radio %s is no longer listed under that "+
					"name. Its earlier record carries no mode information, so "+
					"whether it was removed or renamed cannot be told from "+
					"here — re-probe to settle it", name),
			})
			continue
		}
		out = append(out, Change{
			Kind: "radio", Name: name, Effect: EffectLost,
			From: strings.Join(b.HWModes, ","),
			Detail: fmt.Sprintf("radio %s is gone. WLANs targeted at its "+
				"band will not render on this device", name),
		})
	}
	for ai, a := range after {
		if afterMatch[ai] >= 0 || ambiguousAfter[ai] || renamedAfter[ai] {
			continue
		}
		name := radioDisplayName(a)
		out = append(out, Change{
			Kind: "radio", Name: name, Effect: EffectGained,
			To: strings.Join(a.HWModes, ","),
			Detail: fmt.Sprintf("radio %s appeared (%s). It can carry "+
				"WLANs once the site is applied", name, strings.Join(a.HWModes, ",")),
		})
	}
	return out
}

func radioPHYCompatible(before, after Radio) bool {
	return before.Phy != "" && before.Phy == after.Phy &&
		(before.Section == "" || after.Section == "")
}

func radioComparisonIdentity(radio Radio) (key, display string) {
	if radio.Section != "" {
		return "section\x00" + radio.Section, radio.Section
	}
	return "phy\x00" + radio.Phy, radio.Phy
}

func radioDisplayName(radio Radio) string {
	_, name := radioComparisonIdentity(radio)
	if name == "" {
		return radio.Device
	}
	return name
}

func radioPairName(before, after Radio) string {
	if after.Section != "" {
		return after.Section
	}
	if before.Section != "" {
		return before.Section
	}
	if after.Phy != "" {
		return after.Phy
	}
	return radioDisplayName(before)
}

func radioGroupHasLegacy(before, after []Radio, beforeIndexes, afterIndexes []int) bool {
	for _, i := range beforeIndexes {
		if before[i].Section == "" {
			return true
		}
	}
	for _, i := range afterIndexes {
		if after[i].Section == "" {
			return true
		}
	}
	return false
}

func radioGroupNames(radios []Radio, indexes []int) []string {
	names := make([]string, 0, len(indexes))
	for _, i := range indexes {
		names = append(names, radioDisplayName(radios[i]))
	}
	return names
}

// portChanges reports the wired layout moving.
//
// It matters because the network renderer gates on the probed port map: a VLAN
// is tagged onto names read from the board, and a firmware that renames lan1..4
// to something else silently changes what a rendered bridge-vlan means.
func portChanges(old, new *Registry) []Change {
	if old == nil {
		return nil
	}
	var out []Change
	if old.Ports.Bridge != new.Ports.Bridge {
		out = append(out, Change{
			Kind: "ports", Name: "bridge", Effect: EffectChanged,
			From: old.Ports.Bridge, To: new.Ports.Bridge,
			Detail: fmt.Sprintf("the LAN bridge is now %q (was %q)",
				new.Ports.Bridge, old.Ports.Bridge),
		})
	}
	if !sameModes(old.Ports.LAN, new.Ports.LAN) {
		out = append(out, Change{
			Kind: "ports", Name: "lan", Effect: EffectChanged,
			From: strings.Join(old.Ports.LAN, ","),
			To:   strings.Join(new.Ports.LAN, ","),
			Detail: fmt.Sprintf("the wired port list changed from [%s] to [%s]. "+
				"VLANs are tagged onto these names, so any network rendered "+
				"before this should be re-previewed",
				strings.Join(old.Ports.LAN, ","), strings.Join(new.Ports.LAN, ",")),
		})
	}
	// Empty means this field was not recorded or its source was unreachable,
	// not that the runtime has no bridges. Avoid turning a schema addition or
	// ACL gap into an actionable topology change.
	if len(old.Ports.BridgeDevices) > 0 && len(new.Ports.BridgeDevices) > 0 &&
		!sameModes(old.Ports.BridgeDevices, new.Ports.BridgeDevices) {
		out = append(out, Change{
			Kind: "ports", Name: "runtime-bridges", Effect: EffectChanged,
			From: strings.Join(old.Ports.BridgeDevices, ","),
			To:   strings.Join(new.Ports.BridgeDevices, ","),
			Detail: fmt.Sprintf("runtime bridge devices changed from [%s] to [%s]; "+
				"topology FDB collection should be rebuilt",
				strings.Join(old.Ports.BridgeDevices, ","),
				strings.Join(new.Ports.BridgeDevices, ",")),
		})
	}
	return out
}

func sameModes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Actionable filters a diff to the changes that alter what may be rendered or
// sent. Used to decide whether a re-probe is worth an operator's attention:
// visibility changes are logged, but they do not raise a warning about hardware
// that did not change.
func Actionable(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Effect.Actionable() {
			out = append(out, c)
		}
	}
	return out
}
