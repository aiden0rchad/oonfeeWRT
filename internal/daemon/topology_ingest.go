package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	network, wireless             []string
	networkKnown, wirelessKnown   bool
	networkState, wirelessState   model.TopologySourceState
	networkReason, wirelessReason string
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
	wirelessProvenEmpty, wirelessEmptyReason := wirelessAliasesProvenEmpty(current, snap, sources)
	if wirelessProvenEmpty {
		sources = replaceTopologySource(sources, snap.DeviceID, topology.SourceAssociations,
			model.TopologySourceEmpty, wirelessEmptyReason, snap.At.UnixMilli())
		sources = replaceTopologySource(sources, snap.DeviceID, collector.TopologySourceWirelessDevices,
			model.TopologySourceEmpty, wirelessEmptyReason, snap.At.UnixMilli())
	}
	t.updateAliases(snap, sources, wirelessProvenEmpty)

	bridges := append([]topology.BridgeObservation(nil), snap.Topology.Bridges...)
	if gaps := unansweredTopologySources(sources,
		collector.TopologySourceNetworkDevices,
		collector.TopologySourceWirelessDevices,
		collector.TopologySourceBridgeSTP,
	); len(bridges) > 0 && topologySourceAnswered(sources, topology.SourceBridgeFDB) && len(gaps) > 0 {
		bridges = nil
		sources = replaceTopologySource(sources, snap.DeviceID, topology.SourceBridgeFDB,
			model.TopologySourceUnknown, "bridge evidence prerequisites unavailable: "+strings.Join(gaps, "; "), snap.At.UnixMilli())
	}
	if gaps := t.unknownAliasSources(adopted); len(bridges) > 0 && len(gaps) > 0 {
		bridges = nil
		sources = replaceTopologySource(sources, snap.DeviceID, topology.SourceBridgeFDB,
			model.TopologySourceUnknown, "managed-device alias inventory is incomplete: "+strings.Join(gaps, "; "), snap.At.UnixMilli())
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
	if !wirelessProvenEmpty {
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

func wirelessAliasesProvenEmpty(device *store.Device, snap collector.Snapshot,
	sources []model.TopologySourceObservation) (bool, string) {
	if deviceFunctions(device).Wireless() {
		return false, ""
	}
	if snap.APsFresh && len(snap.APs) > 0 {
		return false, ""
	}
	for _, radio := range snap.Topology.Wireless {
		for _, iface := range radio.Interfaces {
			if iface.IfName != "" || iface.BSSID != "" {
				return false, ""
			}
		}
	}
	for _, networkDevice := range snap.Topology.NetworkDevices {
		if networkDevice.Wireless {
			return false, ""
		}
	}
	if snap.IfacesFresh && len(snap.Ifaces) == 0 {
		return true, "fresh iwinfo.devices inventory contains no active wireless interfaces"
	}
	if source, ok := topologySource(sources, collector.TopologySourceNetworkDevices); ok &&
		source.State == model.TopologySourceObserved && len(snap.Topology.NetworkDevices) > 0 {
		return true, "fresh luci-rpc.getNetworkDevices inventory contains no active wireless interfaces"
	}
	return false, ""
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
	for _, source := range []string{
		collector.TopologySourceNetworkDevices,
		collector.TopologySourceWirelessDevices,
	} {
		if _, ok := topologySource(failed, source); ok {
			continue
		}
		failed = append(failed, model.TopologySourceObservation{
			DeviceID: snap.DeviceID, Source: source,
			State: model.TopologySourceError, Reason: "device poll failed",
			ObservedAt: snap.At.UnixMilli(),
		})
	}
	if err := t.store.ApplyTopologyObservation(ctx, store.TopologyChanges{}, failed); err != nil {
		return fmt.Errorf("topology ingest: persist device failure: %w", err)
	}
	t.updateAliases(snap, failed, false)
	return nil
}

func (t *topologyIngestor) updateAliases(snap collector.Snapshot,
	sources []model.TopologySourceObservation, wirelessProvenEmpty bool) {
	cached := t.aliases[snap.DeviceID]
	if source, ok := topologySource(sources, collector.TopologySourceNetworkDevices); ok {
		cached.networkState, cached.networkReason = source.State, source.Reason
		if topologySourceStateAnswered(source.State) {
			cached.network, cached.networkKnown = nil, true
			for _, device := range snap.Topology.NetworkDevices {
				if device.MAC != "" {
					cached.network = append(cached.network, device.MAC)
				}
			}
		}
	}
	if source, ok := topologySource(sources, collector.TopologySourceWirelessDevices); ok {
		cached.wirelessState, cached.wirelessReason = source.State, source.Reason
		if topologySourceStateAnswered(source.State) {
			cached.wireless, cached.wirelessKnown = nil, true
			if !wirelessProvenEmpty {
				for _, radio := range snap.Topology.Wireless {
					for _, iface := range radio.Interfaces {
						if iface.BSSID != "" {
							cached.wireless = append(cached.wireless, iface.BSSID)
						}
					}
				}
			}
		}
	}
	cached.network = uniqueStrings(cached.network)
	cached.wireless = uniqueStrings(cached.wireless)
	t.aliases[snap.DeviceID] = cached
}

func (t *topologyIngestor) unknownAliasSources(devices []*store.Device) []string {
	var gaps []string
	for _, device := range devices {
		cached := t.aliases[device.ID]
		if !cached.networkKnown {
			gaps = append(gaps, aliasSourceGap(device.ID, collector.TopologySourceNetworkDevices,
				cached.networkState, cached.networkReason))
		}
		if !cached.wirelessKnown {
			gaps = append(gaps, aliasSourceGap(device.ID, collector.TopologySourceWirelessDevices,
				cached.wirelessState, cached.wirelessReason))
		}
	}
	return gaps
}

func aliasSourceGap(deviceID int64, source string, state model.TopologySourceState, reason string) string {
	detail := strings.TrimSpace(reason)
	if detail == "" && state != "" {
		detail = string(state)
	}
	if detail == "" {
		detail = "not observed"
	}
	return fmt.Sprintf("device:%d/%s (%s)", deviceID, source, detail)
}

func topologySourceAnswered(sources []model.TopologySourceObservation, name string) bool {
	source, ok := topologySource(sources, name)
	return ok && topologySourceStateAnswered(source.State)
}

func topologySource(sources []model.TopologySourceObservation,
	name string) (model.TopologySourceObservation, bool) {
	for _, source := range sources {
		if source.Source == name {
			return source, true
		}
	}
	return model.TopologySourceObservation{}, false
}

func topologySourceStateAnswered(state model.TopologySourceState) bool {
	return state == model.TopologySourceObserved || state == model.TopologySourceEmpty
}

func unansweredTopologySources(sources []model.TopologySourceObservation, names ...string) []string {
	var gaps []string
	for _, name := range names {
		found := false
		for _, source := range sources {
			if source.Source != name {
				continue
			}
			found = true
			if source.State == model.TopologySourceObserved || source.State == model.TopologySourceEmpty {
				break
			}
			reason := strings.TrimSpace(source.Reason)
			if reason == "" {
				reason = string(source.State)
			}
			gaps = append(gaps, fmt.Sprintf("%s (%s)", name, reason))
			break
		}
		if !found {
			gaps = append(gaps, name+" (not observed)")
		}
	}
	return gaps
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
