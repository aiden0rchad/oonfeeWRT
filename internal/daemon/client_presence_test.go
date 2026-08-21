package daemon

import (
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

func TestLivePresenceRetainsProofWithoutRefreshingHints(t *testing.T) {
	d := &Daemon{}
	at := time.Unix(50_000, 0)
	d.recordLivePresence(collector.Snapshot{
		DeviceID: 7,
		At:       at,
		Hosts:    []collector.Host{{MAC: "aa:00:00:00:00:01"}},
		Topology: collector.TopologySnapshot{
			Bridges: []topology.BridgeObservation{{Entries: []topology.FDBEntry{{
				MAC: "aa:00:00:00:00:02", AgeSeconds: 4,
			}}}},
			Sources: []model.TopologySourceObservation{{
				Source: topology.SourceBridgeFDB, State: model.TopologySourceObserved,
			}},
		},
	})

	first, ok := d.livePresence(7)
	if !ok || len(first.Active) != 1 ||
		first.Active["aa:00:00:00:00:02"] != at.Add(-4*time.Second).Unix() {
		t.Fatalf("presence=%v known=%v", first, ok)
	}

	// A later inventory-only poll may update names and leases, but it must not
	// turn that repetition into fresh presence.
	d.recordLivePresence(collector.Snapshot{
		DeviceID: 7, At: at.Add(time.Minute),
		Hosts: []collector.Host{{MAC: "aa:00:00:00:00:01"}},
	})
	second, _ := d.livePresence(7)
	if second.Active["aa:00:00:00:00:02"] != first.Active["aa:00:00:00:00:02"] {
		t.Fatalf("inventory poll refreshed presence: before=%v after=%v", first, second)
	}
	if _, exists := second.Active["aa:00:00:00:00:01"]; exists {
		t.Fatalf("host hint became presence: %v", second)
	}
}

func TestLivePresenceSuccessfulEmptyClearsSourceButKeepsLastSeen(t *testing.T) {
	d := &Daemon{}
	t0 := time.Unix(60_000, 0)
	mac := "aa:00:00:00:00:09"
	signal := -45
	d.recordLivePresence(collector.Snapshot{
		DeviceID: 8, At: t0, APsFresh: true,
		APs: []collector.AP{{Stations: map[string]collector.LiveStation{
			mac: {Iface: "phy0-ap0", Signal: &signal},
		}}},
	})

	// A failed/unasked source emits no observation, so the timestamp is retained
	// for the API freshness window rather than becoming a false negative.
	d.recordLivePresence(collector.Snapshot{DeviceID: 8, At: t0.Add(time.Minute)})
	retained, _ := d.livePresence(8)
	if retained.Active[mac] != t0.Unix() {
		t.Fatalf("unasked source cleared presence: %+v", retained)
	}

	// A successfully answered empty hostapd set is authoritative negative
	// evidence for that source and clears it immediately.
	d.recordLivePresence(collector.Snapshot{
		DeviceID: 8, At: t0.Add(2 * time.Minute), APsFresh: true,
	})
	cleared, _ := d.livePresence(8)
	if _, online := cleared.Active[mac]; online {
		t.Fatalf("known-empty association retained active client: %+v", cleared)
	}
	if cleared.LastSeen[mac] != t0.Unix() {
		t.Fatalf("known-empty association erased history: %+v", cleared)
	}
}
