package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

type fakeTopologyStore struct {
	devices []*store.Device
	clients []store.Client
	active  []model.TopologyEdge
	nextID  int64
	last    store.TopologyChanges
	sources []model.TopologySourceObservation
}

func (f *fakeTopologyStore) Devices(context.Context) ([]*store.Device, error) {
	return f.devices, nil
}
func (f *fakeTopologyStore) Clients(context.Context, int64, int) ([]store.Client, error) {
	return f.clients, nil
}
func (f *fakeTopologyStore) TopologyEdgesAt(context.Context, int64) ([]model.TopologyEdge, error) {
	return append([]model.TopologyEdge(nil), f.active...), nil
}
func (f *fakeTopologyStore) TopologySourceStates(context.Context) ([]model.TopologySourceObservation, error) {
	return append([]model.TopologySourceObservation(nil), f.sources...), nil
}
func (f *fakeTopologyStore) ApplyTopologyObservation(_ context.Context, changes store.TopologyChanges,
	sources []model.TopologySourceObservation) error {
	f.last, f.sources = changes, append([]model.TopologySourceObservation(nil), sources...)
	byID := map[int64]model.TopologyEdge{}
	for _, edge := range f.active {
		byID[edge.ID] = edge
	}
	for _, edge := range changes.Close {
		delete(byID, edge.ID)
	}
	for _, edge := range changes.Update {
		byID[edge.ID] = edge
	}
	for _, edge := range changes.Open {
		f.nextID++
		edge.ID = f.nextID
		byID[edge.ID] = edge
	}
	f.active = f.active[:0]
	for _, edge := range byID {
		f.active = append(f.active, edge)
	}
	return nil
}

func TestTopologyIngestWaitsForFleetAliasesAndClosesOnlyOnAnsweredSource(t *testing.T) {
	adopted := time.Now().Unix()
	db := &fakeTopologyStore{devices: []*store.Device{
		{ID: 1, MAC: "02:00:00:00:00:01", Name: "gateway", Role: "gateway",
			Functions: []string{"gateway", "ap", "switch"}, AdoptedAt: &adopted},
		{ID: 2, MAC: "02:00:00:00:00:02", Name: "ap", Role: "ap",
			Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
	}}
	ingest := newTopologyIngestor(db)
	at := int64(1_800_000_000_000)
	answered := func(deviceID int64, source string, state model.TopologySourceState) model.TopologySourceObservation {
		return model.TopologySourceObservation{DeviceID: deviceID, Source: source, State: state, ObservedAt: at}
	}
	wrt := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at)}
	wrt.Topology = collector.TopologySnapshot{
		Cycle:          true,
		NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", BridgeOf: []string{"lan1"}}},
		Wireless:       []topology.WirelessRadio{},
		Bridges: []topology.BridgeObservation{{DeviceID: 1, Bridge: "br-lan",
			Entries:   []topology.FDBEntry{{Port: 1, MAC: "02:00:00:00:00:12"}},
			STP:       &topology.STPState{Bridge: "br-lan", Ports: []topology.STPPort{{Name: "lan1", Port: 1, State: "forwarding"}}},
			PortMedia: map[int]string{1: "wired"}}},
		Sources: []model.TopologySourceObservation{
			answered(1, collector.TopologySourceNetworkDevices, model.TopologySourceObserved),
			answered(1, collector.TopologySourceWirelessDevices, model.TopologySourceEmpty),
			answered(1, topology.SourceBridgeFDB, model.TopologySourceObserved),
			answered(1, collector.TopologySourceBridgeSTP, model.TopologySourceObserved),
		},
	}
	if err := ingest.record(context.Background(), wrt); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 0 || sourceState(db.sources, topology.SourceBridgeFDB) != model.TopologySourceUnknown {
		t.Fatalf("incomplete alias inventory created an edge: active=%+v sources=%+v", db.active, db.sources)
	}

	at += 1_000
	c6 := collector.Snapshot{DeviceID: 2, At: time.UnixMilli(at)}
	c6.Topology = collector.TopologySnapshot{Cycle: true,
		NetworkDevices: []topology.NetworkDevice{{Name: "eth0.1", MAC: "02:00:00:00:00:12"}},
		Wireless:       []topology.WirelessRadio{},
		Sources: []model.TopologySourceObservation{
			{DeviceID: 2, Source: collector.TopologySourceNetworkDevices, State: model.TopologySourceObserved, ObservedAt: at},
			{DeviceID: 2, Source: collector.TopologySourceWirelessDevices, State: model.TopologySourceEmpty, ObservedAt: at},
		}}
	if err := ingest.record(context.Background(), c6); err != nil {
		t.Fatal(err)
	}

	at += 1_000
	wrt.At = time.UnixMilli(at)
	for i := range wrt.Topology.Sources {
		wrt.Topology.Sources[i].ObservedAt = at
	}
	if err := ingest.record(context.Background(), wrt); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 || db.active[0].ChildNode != "device:02:00:00:00:00:02" ||
		db.active[0].ParentNode != "device:02:00:00:00:00:01" {
		t.Fatalf("resolved edge=%+v", db.active)
	}

	// Losing only the port-mapping source must retain the same last-known FDB
	// link, not close it and open a portless/unknown replacement.
	at += 1_000
	wrt.At = time.UnixMilli(at)
	wrt.Topology.Sources = []model.TopologySourceObservation{
		answered(1, collector.TopologySourceNetworkDevices, model.TopologySourceObserved),
		answered(1, collector.TopologySourceWirelessDevices, model.TopologySourceEmpty),
		answered(1, topology.SourceBridgeFDB, model.TopologySourceObserved),
		{DeviceID: 1, Source: collector.TopologySourceBridgeSTP, State: model.TopologySourceError,
			Reason: "unavailable", ObservedAt: at},
	}
	wrt.Topology.Bridges[0].STP = nil
	if err := ingest.record(context.Background(), wrt); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 || db.active[0].ParentPort != "lan1" || db.active[0].Medium != "wired" ||
		sourceState(db.sources, topology.SourceBridgeFDB) != model.TopologySourceUnknown {
		t.Fatalf("mapping loss mutated link: active=%+v sources=%+v", db.active, db.sources)
	}

	// A failed whole-device poll also keeps the interval but makes current
	// coverage explicitly incomplete.
	at += 1_000
	if err := ingest.unavailable(context.Background(), collector.Snapshot{
		DeviceID: 1, At: time.UnixMilli(at), Err: context.DeadlineExceeded,
	}); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 {
		t.Fatal("failed device poll closed an active edge")
	}
	for _, source := range db.sources {
		if source.State != model.TopologySourceError || source.Reason != "device poll failed" {
			t.Fatalf("failed source=%+v", source)
		}
	}

	// A failed read is unknown, not a link-down.
	at += 1_000
	wrt.At = time.UnixMilli(at)
	wrt.Topology.Bridges = nil
	wrt.Topology.Sources = []model.TopologySourceObservation{{
		DeviceID: 1, Source: topology.SourceBridgeFDB, State: model.TopologySourceError,
		Reason: "unavailable", ObservedAt: at,
	}}
	if err := ingest.record(context.Background(), wrt); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 {
		t.Fatal("failed FDB read closed an active edge")
	}

	// A newer demonstrated empty table closes it.
	at += 1_000
	wrt.At = time.UnixMilli(at)
	wrt.Topology.Sources[0].State = model.TopologySourceEmpty
	wrt.Topology.Sources[0].Reason = ""
	wrt.Topology.Sources[0].ObservedAt = at
	if err := ingest.record(context.Background(), wrt); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 0 || len(db.last.Close) != 1 {
		t.Fatalf("demonstrated empty FDB did not close edge: active=%+v changes=%+v", db.active, db.last)
	}
}

func sourceState(sources []model.TopologySourceObservation, name string) model.TopologySourceState {
	for _, source := range sources {
		if source.Source == name {
			return source.State
		}
	}
	return ""
}

func TestTopologyIngestOnlyTreatsGatewayDefaultRouteAsInternetRoot(t *testing.T) {
	adopted := time.Now().Unix()
	device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Name: "ap", Role: "ap",
		Functions: []string{"ap", "switch"}, AdoptedAt: &adopted}
	db := &fakeTopologyStore{devices: []*store.Device{device}}
	ingest := newTopologyIngestor(db)
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at)}
	snapshot.Topology = collector.TopologySnapshot{
		Cycle: true, Uplinks: []topology.Uplink{{DeviceID: 1, Interface: "wan", Active: true}},
		Sources: []model.TopologySourceObservation{
			{DeviceID: 1, Source: collector.TopologySourceNetworkDevices, State: model.TopologySourceObserved, ObservedAt: at},
			{DeviceID: 1, Source: collector.TopologySourceWirelessDevices, State: model.TopologySourceEmpty, ObservedAt: at},
			{DeviceID: 1, Source: topology.SourceDefaultRoute, State: model.TopologySourceObserved, ObservedAt: at},
		},
	}
	if err := ingest.record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 0 {
		t.Fatalf("AP management default route became Internet root: %+v", db.active)
	}
	device.Functions, device.Role = []string{"gateway", "ap", "switch"}, "gateway"
	snapshot.At = time.UnixMilli(at + 1_000)
	for i := range snapshot.Topology.Sources {
		snapshot.Topology.Sources[i].ObservedAt = at + 1_000
	}
	if err := ingest.record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 || db.active[0].ParentNode != topology.InternetNode {
		t.Fatalf("gateway default route was not an Internet root: %+v", db.active)
	}
}

func TestTopologyIngestClosesAssociationWhenDeviceLosesAPFunction(t *testing.T) {
	adopted := time.Now().Unix()
	deviceID := int64(1)
	device := &store.Device{ID: deviceID, MAC: "02:00:00:00:00:01", Role: "switch",
		Functions: []string{"switch"}, AdoptedAt: &adopted}
	active := model.TopologyEdge{
		ID: 51, ChildNode: "client:02:00:00:00:00:44", ChildMAC: "02:00:00:00:00:44",
		ParentNode: "device:" + device.MAC, ParentDeviceID: &deviceID,
		ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "measured",
		ValidFrom: 1_799_999_000_000, LastSeen: 1_799_999_999_000,
		Evidence: []model.TopologyEvidence{{
			Kind: "association", Source: topology.SourceAssociations, DeviceID: &deviceID,
		}}, Ambiguities: []string{},
	}
	db := &fakeTopologyStore{devices: []*store.Device{device}, active: []model.TopologyEdge{active}}
	ingest := newTopologyIngestor(db)
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{DeviceID: deviceID, At: time.UnixMilli(at)}
	snapshot.Topology = collector.TopologySnapshot{Cycle: true, Sources: []model.TopologySourceObservation{
		{DeviceID: deviceID, Source: collector.TopologySourceNetworkDevices,
			State: model.TopologySourceObserved, ObservedAt: at},
	}}
	if err := ingest.record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 0 || len(db.last.Close) != 1 ||
		sourceState(db.sources, topology.SourceAssociations) != model.TopologySourceEmpty {
		t.Fatalf("role transition left association active: active=%+v changes=%+v sources=%+v",
			db.active, db.last, db.sources)
	}
}

func TestSuccessfulNoBridgeInventoryClosesFDBIntervalAndPreservesHistory(t *testing.T) {
	adopted := time.Now().Unix()
	deviceID := int64(1)
	active := model.TopologyEdge{
		ID: 41, ChildNode: "mac:02:00:00:00:00:44", ChildMAC: "02:00:00:00:00:44",
		ParentNode: "device:02:00:00:00:00:01", ParentDeviceID: &deviceID,
		ParentPort: "lan1", Medium: "wired", Confidence: "measured",
		ValidFrom: 1_799_999_000_000, LastSeen: 1_799_999_999_000,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: topology.SourceBridgeFDB, DeviceID: &deviceID,
			Detail: map[string]any{"bridge": "br-old"},
		}}, Ambiguities: []string{},
	}
	db := &fakeTopologyStore{
		devices: []*store.Device{{
			ID: deviceID, MAC: "02:00:00:00:00:01", Role: "ap",
			Functions: []string{"ap", "switch"}, AdoptedAt: &adopted,
		}},
		active: []model.TopologyEdge{active}, nextID: active.ID,
	}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{DeviceID: deviceID, At: time.UnixMilli(at)}
	snapshot.Topology = collector.TopologySnapshot{
		Cycle: true, NetworkDevices: []topology.NetworkDevice{}, Bridges: nil,
		Sources: []model.TopologySourceObservation{
			{DeviceID: deviceID, Source: collector.TopologySourceNetworkDevices, State: model.TopologySourceEmpty, ObservedAt: at},
			{DeviceID: deviceID, Source: collector.TopologySourceWirelessDevices, State: model.TopologySourceEmpty, ObservedAt: at},
			{DeviceID: deviceID, Source: topology.SourceBridgeFDB, State: model.TopologySourceEmpty, ObservedAt: at},
			{DeviceID: deviceID, Source: collector.TopologySourceBridgeSTP, State: model.TopologySourceEmpty, ObservedAt: at},
		},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 0 || len(db.last.Close) != 1 || db.last.Close[0].ID != active.ID ||
		db.last.Close[0].ValidTo == nil || *db.last.Close[0].ValidTo != at {
		t.Fatalf("closure/history=%+v active=%+v", db.last.Close, db.active)
	}
}

func TestTopologyUnavailableRecordsFirstFailedPoll(t *testing.T) {
	adopted := time.Now().Unix()
	db := &fakeTopologyStore{devices: []*store.Device{{
		ID: 1, MAC: "02:00:00:00:00:01", Role: "ap",
		Functions: []string{"ap", "switch"}, AdoptedAt: &adopted,
	}}}
	ingest := newTopologyIngestor(db)
	if err := ingest.unavailable(context.Background(), collector.Snapshot{
		DeviceID: 1, At: time.UnixMilli(1_800_000_000_000), Err: context.DeadlineExceeded,
	}); err != nil {
		t.Fatal(err)
	}
	if len(db.sources) != 1 || db.sources[0].DeviceID != 1 ||
		db.sources[0].Source != topology.SourceDefaultRoute ||
		db.sources[0].State != model.TopologySourceError {
		t.Fatalf("first failed poll source=%+v", db.sources)
	}
}

func TestTopologyIngestPreservesCompetingBSSAssociations(t *testing.T) {
	adopted := time.Now().Unix()
	const clientMAC = "02:00:00:00:00:44"
	db := &fakeTopologyStore{
		devices: []*store.Device{{
			ID: 1, MAC: "02:00:00:00:00:01", Role: "ap",
			Functions: []string{"ap", "switch"}, AdoptedAt: &adopted,
		}},
		clients: []store.Client{{MAC: clientMAC}},
	}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{
		DeviceID: 1, At: time.UnixMilli(at), APsFresh: true,
		APs: []collector.AP{
			{Iface: "phy0-ap0", Stations: map[string]collector.LiveStation{
				clientMAC: {Iface: "phy0-ap0"},
			}},
			{Iface: "phy1-ap0", Stations: map[string]collector.LiveStation{
				clientMAC: {Iface: "phy1-ap0"},
			}},
		},
	}
	snapshot.Topology = collector.TopologySnapshot{
		Cycle: true,
		Sources: []model.TopologySourceObservation{{
			DeviceID: 1, Source: topology.SourceAssociations,
			State: model.TopologySourceObserved, ObservedAt: at,
		}},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 2 {
		t.Fatalf("competing BSSes collapsed to one edge: %+v", db.active)
	}
	ports := map[string]bool{}
	for _, edge := range db.active {
		ports[edge.ParentPort] = true
		if edge.Confidence != "ambiguous" {
			t.Fatalf("competing BSS edge reported measured: %+v", edge)
		}
	}
	if !ports["phy0-ap0"] || !ports["phy1-ap0"] {
		t.Fatalf("competing BSS ports missing: %+v", db.active)
	}
}

func TestTopologyIngestReconcilesCrossDevicePollSkewMonotonically(t *testing.T) {
	adopted := time.Now().Unix()
	const clientMAC = "02:00:00:00:00:44"
	db := &fakeTopologyStore{
		devices: []*store.Device{
			{ID: 1, MAC: "02:00:00:00:00:01", Role: "ap",
				Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
			{ID: 2, MAC: "02:00:00:00:00:02", Role: "ap",
				Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
		},
		clients: []store.Client{{MAC: clientMAC}},
	}
	ingest := newTopologyIngestor(db)
	snapshot := func(deviceID, at int64, iface string) collector.Snapshot {
		return collector.Snapshot{
			DeviceID: deviceID, At: time.UnixMilli(at), APsFresh: true,
			APs: []collector.AP{{Iface: iface, Stations: map[string]collector.LiveStation{
				clientMAC: {Iface: iface},
			}}},
			Topology: collector.TopologySnapshot{Sources: []model.TopologySourceObservation{{
				DeviceID: deviceID, Source: topology.SourceAssociations,
				State: model.TopologySourceObserved, ObservedAt: at,
			}}},
		}
	}

	// Device 2's later-started, faster poll finishes first. Device 1's older
	// request then adds a competing parent; this is normal fleet skew, not a
	// corrupt future interval.
	if err := ingest.record(context.Background(), snapshot(2, 200, "phy1-ap0")); err != nil {
		t.Fatal(err)
	}
	if err := ingest.record(context.Background(), snapshot(1, 100, "phy0-ap0")); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 2 {
		t.Fatalf("cross-device poll lost a candidate parent: %+v", db.active)
	}
	for _, edge := range db.active {
		if edge.ValidFrom != 200 || edge.LastSeen != 200 || edge.Confidence != "ambiguous" {
			t.Fatalf("non-monotonic competing-parent interval: %+v", edge)
		}
	}
}
