package meshlink

import "testing"

// A fully healthy observation, which each test then breaks in exactly one way.
// Written this way on purpose: a ladder is only correct if each rung is reached
// for its own reason, and a test that constructs its input from scratch tends
// to accidentally trip an earlier rung and pass for the wrong one.
func healthy() Observation {
	return Observation{
		DeviceID: 1, MeshID: 2, Name: "backhaul", Section: "oowrt_mesh2_radio0",
		Iface:     "phy0-mesh0",
		Buildable: true, SectionSeen: true,
		IfaceKnown: true, NetDevsFresh: true, NetDevFound: true, Up: true,
		PeersAsked: true, PeersKnown: true,
		Peers: []Peer{{MAC: "aa:bb:cc:dd:ee:01", Plink: "ESTAB"}},
	}
}

func TestPeeredIsTheHealthyState(t *testing.T) {
	l := Evaluate(healthy(), true)

	if l.State != StatePeered || l.Tone != ToneOK {
		t.Fatalf("want peered/ok, got %s/%s (%s)", l.State, l.Tone, l.Reason)
	}
	if !l.Healthy() || l.Actionable() {
		t.Error("a peered backhaul is healthy and not actionable")
	}
	if l.Established == nil || *l.Established != 1 {
		t.Errorf("established count wrong: %v", l.Established)
	}
}

// The rung this package exists for. A mesh that applied cleanly, passed its
// health check, landed its confirm, and whose interface the driver never
// created (§5q) must be reported as a fault — not as a mesh with no peers,
// which is true and useless.
func TestInterfaceAbsentIsReportedAsAFault(t *testing.T) {
	o := healthy()
	o.SectionAppliedNoIface = true
	o.Iface = ""
	o.IfaceKnown = true

	l := Evaluate(o, true)

	if l.State != StateInterfaceAbsent {
		t.Fatalf("want interface-absent, got %s (%s)", l.State, l.Reason)
	}
	if l.Tone != ToneCritical || !l.Actionable() {
		t.Error("a configured mesh with no interface is actionable and critical")
	}
	if l.Peers != nil {
		t.Error("reported peers for an interface that does not exist")
	}
}

// A device that cannot run a mesh at all must never be described in terms of
// peers. The count would be true and the sentence useless — the thing to fix is
// three rungs earlier.
func TestNotBuildableOutranksEverything(t *testing.T) {
	o := healthy()
	o.Buildable = false
	o.GateReason = "this device's wireless driver will not run a mesh point"
	// Deliberately leave every downstream field healthy.

	l := Evaluate(o, true)

	if l.State != StateNotBuildable {
		t.Fatalf("want not-buildable, got %s", l.State)
	}
	if l.Reason != o.GateReason {
		t.Errorf("the gate's own sentence was not used: %q", l.Reason)
	}
	// Muted, not critical: nothing here is broken, the hardware simply cannot.
	if l.Tone != ToneMuted || l.Actionable() {
		t.Error("unsupported hardware is not an outage to go and fix")
	}
}

// Every way of not knowing must be its own state. This is the rule the whole
// package exists to hold, so it is checked exhaustively rather than by example.
func TestEveryUnknownIsDistinctAndNoneIsAnAbsence(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*Observation)
		want   State
	}{
		{"cannot locate the interface", func(o *Observation) { o.IfaceKnown = false }, StateUnidentifiable},
		{"cannot read liveness", func(o *Observation) { o.NetDevsFresh = false }, StateLivenessUnknown},
		{"peers not asked for", func(o *Observation) { o.PeersAsked = false }, StatePeersNotCounted},
		{"peers refused", func(o *Observation) { o.PeersKnown = false }, StatePeersRefused},
	}
	seen := map[State]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := healthy()
			tc.break_(&o)
			l := Evaluate(o, true)

			if l.State != tc.want {
				t.Fatalf("want %s, got %s (%s)", tc.want, l.State, l.Reason)
			}
			if l.State == StateNoPeers {
				t.Fatal("an unknown was reported as an absence of peers")
			}
			if l.Actionable() {
				t.Error("something the controller could not see is not something " +
					"to go and fix; rendering it as one is the collapse the " +
					"capability model exists to prevent")
			}
			if l.Reason == "" {
				t.Error("a state with no sentence is a code nobody looks up")
			}
			if seen[l.State] {
				t.Errorf("%s is reachable from two different causes", l.State)
			}
			seen[l.State] = true
		})
	}
}

// Zero peers on the only node of a mesh is correct and permanent. Rendering it
// red forever is exactly how a screen teaches people to ignore red.
func TestLonelyMeshIsNotAnEmergency(t *testing.T) {
	o := healthy()
	o.Peers = nil

	alone := Evaluate(o, false)
	if alone.State != StateNoPeers {
		t.Fatalf("want no-peers, got %s", alone.State)
	}
	if alone.Tone == ToneWarning || alone.Tone == ToneCritical {
		t.Errorf("a single-node mesh was rendered as a problem: %s", alone.Tone)
	}
	if !containsPhrase(alone.Reason, "nothing for it to peer with") {
		t.Errorf("the reason does not explain why zero is expected: %q", alone.Reason)
	}

	withPeer := Evaluate(o, true)
	if withPeer.Tone != ToneWarning {
		t.Errorf("a mesh that SHOULD have a peer and has none is a warning, got %s",
			withPeer.Tone)
	}
}

// A count without link state cannot tell a working backhaul from one stuck
// mid-handshake, so it must not be rendered as either.
func TestPeersWithoutLinkStateAreNotCalledHealthy(t *testing.T) {
	o := healthy()
	o.Peers = []Peer{{MAC: "aa:bb:cc:dd:ee:01"}, {MAC: "aa:bb:cc:dd:ee:02"}}

	l := Evaluate(o, true)

	if l.State != StatePlinkUnknown {
		t.Fatalf("want plink-unknown, got %s (%s)", l.State, l.Reason)
	}
	if l.Healthy() {
		t.Error("a backhaul whose link state is unknown was counted as carrying")
	}
	if l.Established != nil {
		t.Error("reported an established count that was never observed")
	}
	if len(l.Peers) != 2 {
		t.Errorf("the peer count is real and must survive: %v", l.Peers)
	}
}

// Peers that exist and have not finished establishing is the genuine outage:
// the mesh looks configured, the interface is up, and nothing crosses it.
func TestPeersStuckMidHandshakeIsAnOutage(t *testing.T) {
	o := healthy()
	o.Peers = []Peer{
		{MAC: "aa:bb:cc:dd:ee:01", Plink: "OPN_SNT"},
		{MAC: "aa:bb:cc:dd:ee:02", Plink: "LISTEN"},
	}

	l := Evaluate(o, true)

	if l.State != StatePeering || l.Tone != ToneCritical {
		t.Fatalf("want peering/critical, got %s/%s", l.State, l.Tone)
	}
	if !l.Actionable() || l.Healthy() {
		t.Error("peers that never establish is a fault, not a healthy backhaul")
	}
	if l.Established == nil || *l.Established != 0 {
		t.Errorf("established should be a demonstrated zero, got %v", l.Established)
	}
}

// One established peer is enough to carry the backhaul, even beside a peer that
// is still coming up.
func TestOneEstablishedPeerCarriesTheBackhaul(t *testing.T) {
	o := healthy()
	o.Peers = []Peer{
		{MAC: "bb:bb:bb:bb:bb:02", Plink: "OPN_SNT"},
		{MAC: "aa:aa:aa:aa:aa:01", Plink: "ESTAB"},
	}

	l := Evaluate(o, true)

	if l.State != StatePeered || !l.Healthy() {
		t.Fatalf("want peered, got %s", l.State)
	}
	if *l.Established != 1 {
		t.Errorf("want 1 established, got %d", *l.Established)
	}
	// Sorted, so an unchanged mesh renders identically on every poll rather
	// than reshuffling its rows.
	if l.Peers[0].MAC != "aa:aa:aa:aa:aa:01" {
		t.Errorf("peers not ordered deterministically: %v", l.Peers)
	}
}

// A mesh assigned but never applied is not a fault, and must not be described
// as one — nothing has happened yet.
func TestNotAppliedIsNotAFault(t *testing.T) {
	o := healthy()
	o.SectionSeen = false

	l := Evaluate(o, true)

	if l.State != StateNotApplied {
		t.Fatalf("want not-applied, got %s", l.State)
	}
	if l.Actionable() || l.Tone == ToneCritical {
		t.Error("a mesh nobody has applied yet is not an outage")
	}
}

func containsPhrase(s, want string) bool {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
