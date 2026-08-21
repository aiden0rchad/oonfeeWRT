package daemon

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

type topologyStore interface {
	Devices(context.Context) ([]*store.Device, error)
	Clients(context.Context, int64, int) ([]store.Client, error)
	TopologyEdgesAt(context.Context, int64) ([]model.TopologyEdge, error)
	TopologySourceStates(context.Context) ([]model.TopologySourceObservation, error)
	ApplyTopologyObservation(context.Context, store.TopologyChanges,
		[]model.TopologySourceObservation) error
}

type topologyAliasCache struct {
	network, wireless           []string
	networkKnown, wirelessKnown bool
}

type topologyIngestor struct {
	mu              sync.Mutex
	store           topologyStore
	aliases         map[int64]topologyAliasCache
	lastReconcileAt int64
}

func newTopologyIngestor(db topologyStore) *topologyIngestor {
	return &topologyIngestor{store: db, aliases: map[int64]topologyAliasCache{}}
}

func (t *topologyIngestor) forgetDevice(deviceID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.aliases, deviceID)
}

func (t *topologyIngestor) record(ctx context.Context, snap collector.Snapshot) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	devices, err := t.store.Devices(ctx)
	if err != nil {
		return fmt.Errorf("topology ingest: devices: %w", err)
	}
	adopted := make([]*store.Device, 0, len(devices))
	var current *store.Device
	keep := map[int64]bool{}
	for _, device := range devices {
		if !device.Adopted() {
			continue
		}
		adopted = append(adopted, device)
		keep[device.ID] = true
		if device.ID == snap.DeviceID {
			current = device
		}
	}
	if current == nil {
		return nil
	}
	for deviceID := range t.aliases {
		if !keep[deviceID] {
			delete(t.aliases, deviceID)
		}
	}

	sources := append([]model.TopologySourceObservation(nil), snap.Topology.Sources...)
	if !deviceFunctions(current).Wireless() {
		sources = replaceTopologySource(sources, snap.DeviceID, topology.SourceAssociations,
			model.TopologySourceEmpty, "device has no access-point function", snap.At.UnixMilli())
	}
	t.updateAliases(snap, sources)

	bridges := append([]topology.BridgeObservation(nil), snap.Topology.Bridges...)
	if len(bridges) > 0 && topologySourceAnswered(sources, topology.SourceBridgeFDB) &&
		(!topologySourceAnswered(sources, collector.TopologySourceNetworkDevices) ||
			!topologySourceAnswered(sources, collector.TopologySourceWirelessDevices) ||
			!topologySourceAnswered(sources, collector.TopologySourceBridgeSTP)) {
		bridges = nil
		sources = replaceTopologySource(sources, snap.DeviceID, topology.SourceBridgeFDB,
			model.TopologySourceUnknown, "bridge port mapping sources are unavailable", snap.At.UnixMilli())
	}
	if len(bridges) > 0 && !t.allAliasesKnown(adopted) {
		bridges = nil
		sources = replaceTopologySource(sources, snap.DeviceID, topology.SourceBridgeFDB,
			model.TopologySourceUnknown, "managed-device alias inventory is incomplete", snap.At.UnixMilli())
	}
	if len(sources) == 0 {
		return nil
	}

	clients, err := t.store.Clients(ctx, 0, 10_000)
	if err != nil {
		return fmt.Errorf("topology ingest: clients: %w", err)
	}
	uplinks := append([]topology.Uplink(nil), snap.Topology.Uplinks...)
	if !deviceFunctions(current).Routes() {
		uplinks = nil
	}
	input := topology.InferenceInput{
		At: snap.At.UnixMilli(), Bridges: bridges, Uplinks: uplinks,
		Sources: sources, Neighbors: map[int64][]topology.Neighbor{}, LLDP: snap.Topology.LLDP,
	}
	for _, device := range adopted {
		cached := t.aliases[device.ID]
		aliases := append(append([]string(nil), cached.network...), cached.wireless...)
		input.Devices = append(input.Devices, topology.InventoryDevice{
			ID: device.ID, Name: device.Name, PrimaryMAC: device.MAC, Aliases: aliases,
		})
	}
	for _, client := range clients {
		input.Clients = append(input.Clients, topology.InventoryClient{MAC: client.MAC, Name: client.Name})
	}
	for _, rows := range snap.Topology.Neighbors {
		input.Neighbors[snap.DeviceID] = append(input.Neighbors[snap.DeviceID], rows...)
	}
	if deviceFunctions(current).Wireless() {
		if stations, known := snap.LiveStations(); known {
			macs := make([]string, 0, len(stations))
			for mac := range stations {
				macs = append(macs, mac)
			}
			sort.Strings(macs)
			for _, mac := range macs {
				for _, station := range stations[mac] {
					input.Associations = append(input.Associations, topology.Association{
						DeviceID: snap.DeviceID, Interface: station.Iface, MAC: mac,
					})
				}
			}
		}
	}

	result, err := topology.Infer(input)
	if err != nil {
		return fmt.Errorf("topology ingest: infer: %w", err)
	}
	active, err := t.store.TopologyEdgesAt(ctx, 0)
	if err != nil {
		return fmt.Errorf("topology ingest: active edges: %w", err)
	}
	// Polls are timestamped when their requests start but arrive here when they
	// finish. Different devices poll concurrently, so a slower, earlier-started
	// request can be reconciled after a faster, later-started one. Keep interval
	// changes monotonic in ingestion order without weakening ReconcileIntervals'
	// rejection of future durable state on the first observation after startup.
	reconcileAt := input.At
	if t.lastReconcileAt > reconcileAt {
		reconcileAt = t.lastReconcileAt
		for i := range result.Edges {
			result.Edges[i].ValidFrom = reconcileAt
			result.Edges[i].LastSeen = reconcileAt
		}
	}
	changes, err := topology.ReconcileIntervalsBySource(active, result.Edges,
		reconcileAt, result.Sources)
	if err != nil {
		return fmt.Errorf("topology ingest: reconcile: %w", err)
	}
	durable := store.TopologyChanges{Close: changes.Close}
	if snap.Topology.Cycle {
		durable.ReplaceSourcesFor = []int64{snap.DeviceID}
	}
	for _, edge := range changes.Upsert {
		if edge.ID == 0 {
			durable.Open = append(durable.Open, edge)
		} else {
			durable.Update = append(durable.Update, edge)
		}
	}
	if err := t.store.ApplyTopologyObservation(ctx, durable, result.Sources); err != nil {
		return fmt.Errorf("topology ingest: persist observation: %w", err)
	}
	t.lastReconcileAt = reconcileAt
	return nil
}

func (t *topologyIngestor) unavailable(ctx context.Context, snap collector.Snapshot) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	states, err := t.store.TopologySourceStates(ctx)
	if err != nil {
		return fmt.Errorf("topology ingest: source states: %w", err)
	}
	failed := make([]model.TopologySourceObservation, 0)
	for _, state := range states {
		if state.DeviceID != snap.DeviceID {
			continue
		}
		state.State = model.TopologySourceError
		state.Reason = "device poll failed"
		state.ObservedAt = snap.At.UnixMilli()
		failed = append(failed, state)
	}
	if len(failed) == 0 {
		failed = append(failed, model.TopologySourceObservation{
			DeviceID: snap.DeviceID, Source: topology.SourceDefaultRoute,
			State: model.TopologySourceError, Reason: "device poll failed",
			ObservedAt: snap.At.UnixMilli(),
		})
	}
	if err := t.store.ApplyTopologyObservation(ctx, store.TopologyChanges{}, failed); err != nil {
		return fmt.Errorf("topology ingest: persist device failure: %w", err)
	}
	return nil
}

func (t *topologyIngestor) updateAliases(snap collector.Snapshot,
	sources []model.TopologySourceObservation) {
	cached := t.aliases[snap.DeviceID]
	if topologySourceAnswered(sources, collector.TopologySourceNetworkDevices) {
		cached.network, cached.networkKnown = nil, true
		for _, device := range snap.Topology.NetworkDevices {
			if device.MAC != "" {
				cached.network = append(cached.network, device.MAC)
			}
		}
	}
	if topologySourceAnswered(sources, collector.TopologySourceWirelessDevices) {
		cached.wireless, cached.wirelessKnown = nil, true
		for _, radio := range snap.Topology.Wireless {
			for _, iface := range radio.Interfaces {
				if iface.BSSID != "" {
					cached.wireless = append(cached.wireless, iface.BSSID)
				}
			}
		}
	}
	cached.network = uniqueStrings(cached.network)
	cached.wireless = uniqueStrings(cached.wireless)
	t.aliases[snap.DeviceID] = cached
}

func (t *topologyIngestor) allAliasesKnown(devices []*store.Device) bool {
	for _, device := range devices {
		cached := t.aliases[device.ID]
		if !cached.networkKnown || !cached.wirelessKnown {
			return false
		}
	}
	return true
}

func topologySourceAnswered(sources []model.TopologySourceObservation, name string) bool {
	for _, source := range sources {
		if source.Source == name {
			return source.State == model.TopologySourceObserved || source.State == model.TopologySourceEmpty
		}
	}
	return false
}

func replaceTopologySource(sources []model.TopologySourceObservation, deviceID int64,
	name string, state model.TopologySourceState, reason string, at int64) []model.TopologySourceObservation {
	for i := range sources {
		if sources[i].DeviceID == deviceID && sources[i].Source == name {
			sources[i].State, sources[i].Reason, sources[i].ObservedAt = state, reason, at
			return sources
		}
	}
	return append(sources, model.TopologySourceObservation{
		DeviceID: deviceID, Source: name, State: state, Reason: reason, ObservedAt: at,
	})
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := values[:0]
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
