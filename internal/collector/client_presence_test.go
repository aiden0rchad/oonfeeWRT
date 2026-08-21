package collector

import (
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

func TestClientPresenceRejectsHintsLeasesAndStaleNeighbors(t *testing.T) {
	at := time.Unix(10_000, 0)
	zero := int64(0)
	s := Snapshot{
		At: at,
		Hosts: []Host{
			{MAC: "aa:00:00:00:00:01", IPv4: []string{"192.0.2.10"}},
			{MAC: "aa:00:00:00:00:02", Lease: at.Add(time.Hour).Unix()},
		},
		Topology: TopologySnapshot{
			Neighbors: map[int][]topology.Neighbor{4: {
				{Family: 4, MAC: "aa:00:00:00:00:03", State: "stale"},
				{Family: 4, MAC: "aa:00:00:00:00:04", State: "failed"},
				// Live stock BusyBox output reports used 0/0/0 on cached
				// STALE rows. That zero is not a fresh confirmation.
				{Family: 4, MAC: "aa:00:00:00:00:05", State: "stale",
					ConfirmedSeconds: &zero},
			}},
			Sources: []model.TopologySourceObservation{{
				Source: topology.SourceNeighbors(4), State: model.TopologySourceObserved,
			}},
		},
	}
	if got := s.ClientPresence(); len(got) != 0 {
		t.Fatalf("non-authoritative inventory reported present: %v", got)
	}
}

func TestClientPresenceKeepsAuthoritativeSourceTimes(t *testing.T) {
	at := time.Unix(20_000, 0)
	signal := -48
	confirmed := int64(11)
	s := Snapshot{
		At:       at,
		APsFresh: true,
		APs: []AP{{Stations: map[string]LiveStation{
			"AA:00:00:00:00:01": {Iface: "phy0-ap0", Signal: &signal},
		}}},
		Topology: TopologySnapshot{
			Neighbors: map[int][]topology.Neighbor{4: {
				{Family: 4, MAC: "aa:00:00:00:00:02", State: "reachable"},
				{Family: 4, MAC: "aa:00:00:00:00:05", State: "stale",
					ConfirmedSeconds: &confirmed},
			}},
			Bridges: []topology.BridgeObservation{{Entries: []topology.FDBEntry{
				{MAC: "aa:00:00:00:00:03", AgeSeconds: 7.5},
				{MAC: "aa:00:00:00:00:04", AgeSeconds: 0, Local: true},
			}}},
			Sources: []model.TopologySourceObservation{
				{Source: topology.SourceNeighbors(4), State: model.TopologySourceObserved},
				{Source: topology.SourceBridgeFDB, State: model.TopologySourceObserved},
			},
		},
	}

	got := s.ClientPresence()
	for _, mac := range []string{"aa:00:00:00:00:01", "aa:00:00:00:00:02"} {
		if got[mac] != at.Unix() {
			t.Errorf("%s presence=%d, want %d", mac, got[mac], at.Unix())
		}
	}
	if got["aa:00:00:00:00:03"] != at.Add(-7500*time.Millisecond).Unix() {
		t.Errorf("FDB source age lost: %v", got)
	}
	if got["aa:00:00:00:00:05"] != at.Add(-11*time.Second).Unix() {
		t.Errorf("neighbor confirmation age lost: %v", got)
	}
	if _, ok := got["aa:00:00:00:00:04"]; ok {
		t.Errorf("local bridge entry reported as a client: %v", got)
	}
}

func TestClientPresenceRequiresSuccessfulSourceObservation(t *testing.T) {
	s := Snapshot{
		At: time.Unix(30_000, 0),
		Topology: TopologySnapshot{
			Neighbors: map[int][]topology.Neighbor{4: {{
				Family: 4, MAC: "aa:00:00:00:00:01", State: "reachable",
			}}},
			Bridges: []topology.BridgeObservation{{Entries: []topology.FDBEntry{{
				MAC: "aa:00:00:00:00:02",
			}}}},
			Sources: []model.TopologySourceObservation{
				{Source: topology.SourceNeighbors(4), State: model.TopologySourceError},
				{Source: topology.SourceBridgeFDB, State: model.TopologySourceError},
			},
		},
	}
	if got := s.ClientPresence(); len(got) != 0 {
		t.Fatalf("failed sources refreshed presence: %v", got)
	}
}
