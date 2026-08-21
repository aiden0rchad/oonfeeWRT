package collector

import (
	"math"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

// ClientPresence is the latest authoritative proof that a MAC was present,
// keyed by lower-case MAC and expressed as Unix seconds.
type ClientPresence map[string]int64

// ClientPresenceState separates currently retained source evidence from the
// last authoritative proof kept for display.
type ClientPresenceState struct {
	Active   ClientPresence
	LastSeen ClientPresence
}

// ClientPresenceObservation is one successfully answered source. An empty
// Clients map is meaningful: it clears that source's previous active set.
type ClientPresenceObservation struct {
	Source  string
	Clients ClientPresence
}

func presenceRecorder(out ClientPresence) func(string, int64) {
	return func(mac string, at int64) {
		mac = strings.ToLower(strings.TrimSpace(mac))
		if mac != "" && at > out[mac] {
			out[mac] = at
		}
	}
}

// ClientPresenceObservations returns only sources that prove reachability.
// Host hints and DHCP leases are inventory: both can outlive a disconnected
// client and therefore never appear here.
func (s Snapshot) ClientPresenceObservations() []ClientPresenceObservation {
	observations := []ClientPresenceObservation{}
	if stations, known := s.LiveStations(); known {
		clients := ClientPresence{}
		record := presenceRecorder(clients)
		for mac := range stations {
			record(mac, s.At.Unix())
		}
		observations = append(observations, ClientPresenceObservation{
			Source: topology.SourceAssociations, Clients: clients,
		})
	}

	for _, family := range []int{4, 6} {
		if !topologySourceSuccessful(s.Topology.Sources, topology.SourceNeighbors(family)) {
			continue
		}
		clients := ClientPresence{}
		record := presenceRecorder(clients)
		rows := s.Topology.Neighbors[family]
		for _, row := range rows {
			switch row.State {
			case "reachable":
				at := s.At.Unix()
				if row.ConfirmedSeconds != nil {
					at -= *row.ConfirmedSeconds
				}
				record(row.MAC, at)
			case "stale", "delay", "probe":
				// These states are not themselves proof of life. BusyBox's
				// confirmed age is, when it is positive: retain that exact
				// source time and let the caller's active window decide whether
				// it is still fresh. Stock OpenWrt BusyBox can emit `used 0/0/0`
				// for every cached STALE row; zero there is unavailable, not a
				// just-confirmed client.
				if row.ConfirmedSeconds != nil && *row.ConfirmedSeconds > 0 {
					record(row.MAC, s.At.Unix()-*row.ConfirmedSeconds)
				}
			}
		}
		observations = append(observations, ClientPresenceObservation{
			Source: topology.SourceNeighbors(family), Clients: clients,
		})
	}

	if topologySourceSuccessful(s.Topology.Sources, topology.SourceBridgeFDB) {
		clients := ClientPresence{}
		record := presenceRecorder(clients)
		for _, bridge := range s.Topology.Bridges {
			for _, entry := range bridge.Entries {
				if entry.Local || entry.AgeSeconds < 0 ||
					math.IsNaN(entry.AgeSeconds) || math.IsInf(entry.AgeSeconds, 0) {
					continue
				}
				observed := s.At.Add(-time.Duration(entry.AgeSeconds * float64(time.Second)))
				record(entry.MAC, observed.Unix())
			}
		}
		observations = append(observations, ClientPresenceObservation{
			Source: topology.SourceBridgeFDB, Clients: clients,
		})
	}
	return observations
}

func (s Snapshot) ClientPresence() ClientPresence {
	out := ClientPresence{}
	record := presenceRecorder(out)
	for _, observation := range s.ClientPresenceObservations() {
		for mac, at := range observation.Clients {
			record(mac, at)
		}
	}
	return out
}
