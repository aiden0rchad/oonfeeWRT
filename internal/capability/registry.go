// Package capability answers "what can this device actually do?" — and, just as
// importantly, "what could we not find out?".
//
// The three-state model is the whole point. A feature is Present, Absent, or
// NotObservable, and collapsing the third into the second is how a controller
// deletes a screen from hardware that supports it. tools/probe.py did exactly
// that against the reference device and reported "no DSA" and "legacy iptables"
// for a router with a DSA switch and firewall4.
package capability

import (
	"fmt"
	"sort"
	"strings"
)

// State is what we learned about one capability.
type State int

const (
	// Unknown means we never looked.
	Unknown State = iota
	// Present means the device demonstrably has it.
	Present
	// Absent means the device demonstrably lacks it. Safe to hide the feature.
	Absent
	// NotObservable means the check itself was refused or unreachable — the ACL
	// blocked it, a binary was missing, a method was ungranted. NEVER render
	// this as Absent: it is a gap in our reach, not in the device.
	NotObservable
)

func (s State) String() string {
	switch s {
	case Present:
		return "present"
	case Absent:
		return "absent"
	case NotObservable:
		return "not-observable"
	}
	return "unknown"
}

// Buildable reports whether a UI feature gated on this capability may render.
// Only Present qualifies: Unknown and NotObservable mean we do not know, and
// showing a feature we cannot back with data is worse than omitting it.
func (s State) Buildable() bool { return s == Present }

// Feature names a capability the UI gates on.
type Feature string

const (
	// FeatDSA gates the Ports screen. Detected from luci-rpc.getNetworkDevices
	// (devtype "dsa") rather than /sys, because rpcd canonicalises paths and a
	// /sys/class/net/* grant silently never matches.
	FeatDSA Feature = "dsa"
	// FeatFirewall4 selects the nftables zone model over legacy iptables.
	FeatFirewall4 Feature = "firewall4"
	// FeatBatching allows collapsing a poll into one request.
	FeatBatching Feature = "jsonrpc-batching"
	// FeatSurvey gates channel utilization (busy/active), the portable metric.
	FeatSurvey Feature = "iwinfo-survey"
	// FeatAirtimeSplit gates Interference and the Airtime split, which need
	// rx_time/tx_time and are NOT computable where the driver leaves them
	// uninitialised.
	FeatAirtimeSplit Feature = "airtime-split"
	// FeatHostapdControl gates per-client actions (reconnect/block).
	FeatHostapdControl Feature = "hostapd-control"
	// FeatAccounting gates per-client bandwidth (needs nlbwmon).
	FeatAccounting Feature = "per-client-accounting"
	// FeatPreflightDirty gates the "unsaved changes on device" guard. Without
	// it the apply path cannot detect a human mid-edit in LuCI.
	FeatPreflightDirty Feature = "preflight-dirty-check"
)

// Class is the DEVICE-BUDGET hardware class. The weakest class sets the budget,
// so this drives poll cadence rather than merely labelling the device.
type Class string

const (
	ClassA       Class = "A" // comfortable (mvebu, WRT3200ACM)
	ClassB       Class = "B" // modern efficient (MT7981/Filogic)
	ClassC       Class = "C" // constrained (MT7621) — sets the budget
	ClassUnknown Class = "?"
)

// Quirk is a field that is present, correctly typed, plausible in any single
// sample, and wrong.
//
// This is the category presence-probing cannot catch, and the reason capability
// gating keys on a driver/model list rather than on whether a key exists. All
// three entries below were measured on mwlwifi.
type Quirk struct {
	Source string // ubus object/method the field comes from
	Field  string
	Reason string
}

// Board identifies the device.
type Board struct {
	Model      string
	BoardName  string
	Kernel     string
	Target     string
	Release    string
	RootFSType string
}

// Radio is one PHY as the UI needs to know it.
type Radio struct {
	Device      string // e.g. phy0-ap0
	Phy         string
	Channel     int
	Frequency   int
	HWModes     []string
	Hardware    string
	SurveyUsest State // channel utilization from busy/active

	// NoiseStable is whether this radio's noise floor survives re-reading.
	//
	// Per radio, not per device: on the reference WRT3200ACM the 5 GHz radio is
	// steady within a few dB while the 2.4 GHz radio swings 40+, on the same
	// driver. Gating device-wide would throw away a good reading to punish a
	// bad one.
	//
	// Absent means caught moving. Present means NOT caught moving in two
	// samples, which is weaker than "verified stable" and must not be read as
	// a guarantee.
	NoiseStable State
}

// Registry is the answer for one device. It is persisted per device and is what
// every screen consults before rendering.
type Registry struct {
	Board    Board
	Class    Class
	Features map[Feature]State
	Quirks   []Quirk
	Radios   []Radio

	// Binaries reachable through the ACL's file.exec grants. Absent entries may
	// mean "not installed" OR "not granted" — Notes records which.
	Binaries map[string]string

	// Notes carries anything the operator should read, including every reason a
	// check came back NotObservable.
	Notes []string
}

// NewRegistry returns an empty registry with all features Unknown.
func NewRegistry() *Registry {
	return &Registry{
		Features: map[Feature]State{},
		Binaries: map[string]string{},
	}
}

// Set records a capability state.
func (r *Registry) Set(f Feature, s State) { r.Features[f] = s }

// State returns a feature's state, defaulting to Unknown.
func (r *Registry) State(f Feature) State { return r.Features[f] }

// Can is the question every screen asks. It is deliberately strict: anything
// other than Present is a no.
func (r *Registry) Can(f Feature) bool { return r.Features[f].Buildable() }

// Note records an operator-facing observation.
func (r *Registry) Note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// AddQuirk records an untrustworthy field, and marks any feature that depends
// on it as Absent — the field exists, so it is not NotObservable; it is
// unusable, which for the UI is the same as missing.
func (r *Registry) AddQuirk(q Quirk) {
	for _, existing := range r.Quirks {
		if existing.Source == q.Source && existing.Field == q.Field {
			return
		}
	}
	r.Quirks = append(r.Quirks, q)
}

// HasQuirk reports whether a given source/field is known-untrustworthy here.
func (r *Registry) HasQuirk(source, field string) bool {
	for _, q := range r.Quirks {
		if q.Source == source && q.Field == field {
			return true
		}
	}
	return false
}

// Unobservable lists features whose checks we could not run, so adoption can
// tell the operator what a wider ACL would buy them.
func (r *Registry) Unobservable() []Feature {
	var out []Feature
	for f, s := range r.Features {
		if s == NotObservable {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Summary is a one-line human description, for logs and the device detail pane.
func (r *Registry) Summary() string {
	var present, absent, unobs []string
	keys := make([]Feature, 0, len(r.Features))
	for f := range r.Features {
		keys = append(keys, f)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, f := range keys {
		switch r.Features[f] {
		case Present:
			present = append(present, string(f))
		case Absent:
			absent = append(absent, string(f))
		case NotObservable:
			unobs = append(unobs, string(f))
		}
	}
	s := fmt.Sprintf("class %s %s", r.Class, r.Board.Model)
	if len(present) > 0 {
		s += "; has: " + strings.Join(present, ",")
	}
	if len(absent) > 0 {
		s += "; lacks: " + strings.Join(absent, ",")
	}
	if len(unobs) > 0 {
		s += "; UNOBSERVABLE: " + strings.Join(unobs, ",")
	}
	if len(r.Quirks) > 0 {
		s += fmt.Sprintf("; %d quirk(s)", len(r.Quirks))
	}
	return s
}
