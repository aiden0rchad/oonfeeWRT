// Package meshlink decides what one 802.11s backhaul is actually doing, from
// what the controller was able to observe about it.
//
// # Why this is a closed vocabulary and not a bag of fields
//
// The obvious shape is a struct of nullables — peer count, interface up,
// capability present — handed to a screen that decides what they mean together.
// That shape has failed in this project twice already, in both directions: a
// count computed from whatever happened to be loaded (§5b), and one question
// answered two different ways on two screens (§5h). A UI that re-derives health
// from nullables is a second implementation of this logic, and the two drift.
//
// So the judgement is made once, here, and travels as one value. A renderer
// switches on it and never re-decides.
//
// # Why the order of the ladder is the design
//
// Every state below is reached only when the ones above it did not apply, and
// that ordering encodes what an operator should be told FIRST. A device whose
// driver will not run a mesh at all must never be described as having zero
// peers: the peer count is true and the sentence is useless, because the thing
// to fix is three steps earlier in the chain. Ordering the ladder from "can
// this device do it" through "did we ask it to" and "does the interface exist"
// down to "is anyone there" means the first state that matches is also the
// first thing worth doing something about.
//
// # The rule that shapes every entry
//
// Nothing here may report an absence it did not observe. "No peers" requires
// that we demonstrably looked; a refused call, an un-issued call, and an
// interface we could not see are each their own state. That is the same rule
// internal/capability enforces for hardware, applied to a link — and the reason
// there are thirteen states rather than four.
package meshlink

import "sort"

// State is what a backhaul is doing. Closed set: a renderer switches on it.
type State string

const (
	// StateNotBuildable — the device cannot carry a mesh at all. The reason
	// comes from render.MeshGate, so this screen and the apply preview say the
	// same sentence.
	StateNotBuildable State = "not-buildable"
	// StateNotApplied — the site model assigns a mesh here and no section for
	// it has been applied and confirmed. Nothing is wrong; nothing has happened.
	StateNotApplied State = "not-applied"
	// StateUnidentifiable — the call that says which interface is the mesh
	// never answered, so the link cannot be located to be examined.
	StateUnidentifiable State = "mesh-unidentifiable"
	// StateInterfaceAbsent — a section was applied and confirmed and the device
	// reports no interface for it. This is §5q: applied cleanly, never existed.
	StateInterfaceAbsent State = "interface-absent"
	// StateLivenessUnknown — the interface is known and the call that would say
	// whether it is up did not answer.
	StateLivenessUnknown State = "liveness-unknown"
	// StateNoNetdev — the interface is known, the device answered, and no such
	// netdev exists.
	StateNoNetdev State = "no-netdev"
	// StateInterfaceDown — the netdev exists and is down.
	StateInterfaceDown State = "interface-down"
	// StatePeersNotCounted — up, and nobody asked how many peers. A budget
	// decision, not a fault: peer counting rides a slow cadence.
	StatePeersNotCounted State = "peers-not-counted"
	// StatePeersRefused — up, asked, and the answer was refused or unreadable.
	StatePeersRefused State = "peers-refused"
	// StateNoPeers — up, asked, answered, and nobody is there.
	StateNoPeers State = "no-peers"
	// StatePeering — peers exist and none has finished establishing.
	StatePeering State = "peering"
	// StatePlinkUnknown — peers exist and this driver reported no link state
	// for any of them. The count is real; the health is not knowable.
	StatePlinkUnknown State = "plink-unknown"
	// StatePeered — at least one peer is established. The backhaul carries.
	StatePeered State = "peered"
)

// Tone is how a renderer should weight a state. Kept out of the UI because the
// interesting judgements are not obvious, and repeating them per screen is how
// two screens come to disagree.
type Tone string

const (
	ToneOK       Tone = "ok"
	ToneNormal   Tone = "normal"
	ToneMuted    Tone = "muted"
	ToneWarning  Tone = "warning"
	ToneCritical Tone = "critical"
)

// Observation is everything known about one configured mesh on one device.
//
// Every field that can be unknown is explicitly three-valued rather than
// defaulted, because a zero value that means "no" is how this package's whole
// reason for existing gets undone.
type Observation struct {
	DeviceID int64
	MeshID   int    // site-model mesh id, 0 when it could not be attributed
	Name     string // the mesh id string an operator recognises
	Section  string // the applied UCI section, when one is recorded
	Iface    string // the interface name, when it is known

	// Buildable is the render.MeshGate verdict, with its reason.
	Buildable   bool
	GateReason  string
	SectionSeen bool // a section for this mesh was applied AND confirmed

	// IfaceKnown records that the call naming interfaces answered. Without it,
	// an empty Iface means either "not created" or "we could not ask", and
	// those are opposite conclusions.
	IfaceKnown bool
	// SectionAppliedNoIface is §5q: the section exists and the device reports
	// no interface for it.
	SectionAppliedNoIface bool

	// NetDevsFresh records that the interface-liveness call answered.
	NetDevsFresh bool
	NetDevFound  bool
	Up           bool

	// PeersAsked is false when nobody issued the peer query on this cycle,
	// which is the normal baseline case and is not a fault.
	PeersAsked bool
	// PeersKnown is false when it was asked and did not answer.
	PeersKnown bool
	Peers      []Peer
}

// Peer is one mesh neighbour.
type Peer struct {
	MAC string
	// Plink is the 802.11s peer-link state — "ESTAB", "OPN_SNT", "LISTEN".
	// Empty when the driver reported none, which is a real possibility and is
	// why StatePlinkUnknown exists: a count without link state cannot tell a
	// working backhaul from one stuck mid-handshake.
	Plink string
	// SignalDBm is nil when it was not reported.
	SignalDBm *int
	// InactiveMS is how long since traffic, nil when not reported.
	InactiveMS *int
}

// Established reports whether this peer has finished peering.
func (p Peer) Established() bool { return p.Plink == "ESTAB" }

// Link is the judgement, and is what leaves this package.
type Link struct {
	DeviceID int64
	MeshID   int
	Name     string
	Iface    string
	State    State
	Tone     Tone
	// Reason is always populated. A state with no sentence is a code an
	// operator has to look up, and nobody looks it up.
	Reason string
	// Peers is nil unless peers were actually counted. Not an empty slice: the
	// difference between "none" and "not counted" is the whole point.
	Peers []Peer
	// Established is how many peers have finished peering, nil when unknown.
	Established *int
}

// Evaluate runs the ladder. Pure, and the only place a mesh's health is decided.
//
// `peerExpected` says whether the site model puts another device in this mesh.
// It changes only one thing — the tone of StateNoPeers — and that one thing
// matters: on a single-node mesh, zero peers is the correct and permanent
// state, and rendering it red forever is precisely how a screen teaches people
// to ignore red.
func Evaluate(o Observation, peerExpected bool) Link {
	l := Link{DeviceID: o.DeviceID, MeshID: o.MeshID, Name: o.Name, Iface: o.Iface}

	set := func(s State, t Tone, reason string) Link {
		l.State, l.Tone, l.Reason = s, t, reason
		return l
	}

	switch {
	case !o.Buildable:
		return set(StateNotBuildable, ToneMuted, o.GateReason)

	case !o.SectionSeen:
		return set(StateNotApplied, ToneNormal,
			"this mesh is assigned to this device and has not been applied to "+
				"it yet, so there is nothing running to report on")

	case o.SectionAppliedNoIface:
		// §5q, and the reason this package exists.
		return set(StateInterfaceAbsent, ToneCritical,
			"the configuration for this mesh is on the device and no interface "+
				"exists for it. That is a driver refusing to create the "+
				"interface after accepting the config — the apply succeeded, "+
				"the health check passed, and the backhaul is not there")

	case !o.IfaceKnown:
		return set(StateUnidentifiable, ToneMuted,
			"this device did not report which of its wireless interfaces is the "+
				"mesh, so its backhaul could not be located. That is a gap in "+
				"what the controller can see, not a fault on the device")

	case !o.NetDevsFresh:
		return set(StateLivenessUnknown, ToneMuted,
			"whether "+o.Iface+" is up could not be read on this poll, so its "+
				"state is unknown rather than down")

	case !o.NetDevFound:
		return set(StateNoNetdev, ToneCritical,
			"the device answered and reports no interface called "+o.Iface+
				", so the mesh is configured and not running")

	case !o.Up:
		return set(StateInterfaceDown, ToneCritical,
			o.Iface+" exists and is down, so this backhaul carries nothing")

	case !o.PeersAsked:
		return set(StatePeersNotCounted, ToneMuted,
			o.Iface+" is up. Its peers are counted on a slower cadence than "+
				"this reading, so the number is not known yet — that is a "+
				"budget decision, not a fault")

	case !o.PeersKnown:
		return set(StatePeersRefused, ToneMuted,
			o.Iface+" is up and its peers could not be read. The count is "+
				"unknown, which is not the same as zero")

	case len(o.Peers) == 0:
		tone, tail := ToneWarning, ""
		if !peerExpected {
			tone = ToneNormal
			tail = ". No other device is assigned to this mesh, so there is " +
				"nothing for it to peer with yet"
		}
		return set(StateNoPeers, tone,
			o.Iface+" is up and has no peers"+tail)
	}

	// Peers exist. Sorted so two readings of an unchanged mesh render
	// identically rather than reshuffling on every poll.
	peers := append([]Peer(nil), o.Peers...)
	sort.Slice(peers, func(i, j int) bool { return peers[i].MAC < peers[j].MAC })
	l.Peers = peers

	estab, anyPlink := 0, false
	for _, p := range peers {
		if p.Plink != "" {
			anyPlink = true
		}
		if p.Established() {
			estab++
		}
	}
	if !anyPlink {
		// A count with no link state cannot tell a working backhaul from one
		// stuck mid-handshake, so it must not be rendered as either.
		return set(StatePlinkUnknown, ToneNormal,
			plural(len(peers))+" visible on "+o.Iface+", and this driver "+
				"reported no peer-link state for any of them, so whether the "+
				"backhaul is actually carrying traffic is not knowable here")
	}
	l.Established = &estab
	if estab == 0 {
		return set(StatePeering, ToneCritical,
			plural(len(peers))+" visible on "+o.Iface+" and none has finished "+
				"establishing, so the backhaul is not carrying traffic")
	}
	return set(StatePeered, ToneOK,
		plural(estab)+" established on "+o.Iface)
}

func plural(n int) string {
	if n == 1 {
		return "1 peer"
	}
	return itoa(n) + " peers"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Healthy reports whether a link is carrying traffic, for a fleet-level tally.
//
// Deliberately narrow: only StatePeered qualifies. Every other state is either
// a fault or an unknown, and counting an unknown as healthy is how a summary
// comes to disagree with the rows beneath it.
func (l Link) Healthy() bool { return l.State == StatePeered }

// Actionable reports whether this state is something to go and fix, as opposed
// to something the controller merely could not see.
//
// The split matters more than the states do: a screen that renders "we could
// not ask" the same as "it is broken" recreates, one layer up, exactly the
// collapse the capability model exists to prevent.
func (l Link) Actionable() bool {
	switch l.State {
	case StateInterfaceAbsent, StateNoNetdev, StateInterfaceDown, StatePeering:
		return true
	}
	return false
}
