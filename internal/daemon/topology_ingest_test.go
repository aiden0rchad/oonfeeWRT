package daemon

import (
	"context"
	"strings"
	"sync"
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
	if reason := sourceReason(db.sources, topology.SourceBridgeFDB); !strings.Contains(reason,
		"device:2/"+collector.TopologySourceNetworkDevices) || !strings.Contains(reason,
		"device:2/"+collector.TopologySourceWirelessDevices) {
		t.Fatalf("alias gap does not identify missing device sources: %q", reason)
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
	if reason := sourceReason(db.sources, topology.SourceBridgeFDB); !strings.Contains(reason,
		collector.TopologySourceBridgeSTP) || !strings.Contains(reason, "unavailable") ||
		strings.Contains(reason, collector.TopologySourceNetworkDevices) ||
		strings.Contains(reason, collector.TopologySourceWirelessDevices) {
		t.Fatalf("FDB downgrade does not identify the failed STP source: %q", reason)
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

func TestTopologyIngestAcceptsWiredOnlyAliasesWithoutWirelessRPC(t *testing.T) {
	adopted := time.Now().Unix()
	const (
		apMAC     = "02:00:00:00:00:01"
		switchMAC = "02:00:00:00:00:02"
	)
	db := &fakeTopologyStore{devices: []*store.Device{
		{ID: 1, MAC: apMAC, Name: "access point", Role: "ap",
			Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
		{ID: 2, MAC: switchMAC, Name: "wired switch", Role: "switch",
			Functions: []string{"switch"}, AdoptedAt: &adopted},
	}}
	ingest := newTopologyIngestor(db)
	at := int64(1_800_000_000_000)
	observed := func(deviceID int64, source string) model.TopologySourceObservation {
		return model.TopologySourceObservation{
			DeviceID: deviceID, Source: source, State: model.TopologySourceObserved, ObservedAt: at,
		}
	}

	ap := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at)}
	ap.Topology = collector.TopologySnapshot{
		Cycle:          true,
		NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", MAC: apMAC}},
		Wireless: []topology.WirelessRadio{{Key: "radio0", Interfaces: []topology.WirelessInterface{{
			IfName: "phy0-ap0", Mode: "ap", BSSID: "02:00:00:00:10:01",
		}}}},
		Sources: []model.TopologySourceObservation{
			observed(1, collector.TopologySourceNetworkDevices),
			observed(1, collector.TopologySourceWirelessDevices),
		},
	}
	if err := ingest.record(context.Background(), ap); err != nil {
		t.Fatal(err)
	}

	at += 1_000
	wired := collector.Snapshot{DeviceID: 2, At: time.UnixMilli(at)}
	wired.Topology = collector.TopologySnapshot{
		Cycle:          true,
		NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", MAC: switchMAC, BridgeOf: []string{"eth1"}}},
		Bridges: []topology.BridgeObservation{{
			DeviceID: 2, Bridge: "br-lan",
			Entries: []topology.FDBEntry{{Port: 1, MAC: apMAC}},
			STP: &topology.STPState{Bridge: "br-lan", Ports: []topology.STPPort{{
				Name: "eth1", Port: 1, State: "forwarding",
			}}},
			PortMedia: map[int]string{1: "wired"},
		}},
		Sources: []model.TopologySourceObservation{
			{DeviceID: 2, Source: collector.TopologySourceNetworkDevices,
				State: model.TopologySourceObserved, ObservedAt: at},
			{DeviceID: 2, Source: collector.TopologySourceWirelessDevices,
				State: model.TopologySourceError, Reason: "source call failure: unsupported operation", ObservedAt: at},
			{DeviceID: 2, Source: topology.SourceBridgeFDB,
				State: model.TopologySourceObserved, ObservedAt: at},
			{DeviceID: 2, Source: collector.TopologySourceBridgeSTP,
				State: model.TopologySourceObserved, ObservedAt: at},
		},
	}
	if err := ingest.record(context.Background(), wired); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 || db.active[0].ChildNode != "device:"+apMAC ||
		db.active[0].ParentNode != "device:"+switchMAC {
		t.Fatalf("wired-only bridge evidence was discarded: active=%+v sources=%+v", db.active, db.sources)
	}
	if sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceEmpty ||
		sourceReason(db.sources, collector.TopologySourceWirelessDevices) != "fresh luci-rpc.getNetworkDevices inventory contains no active wireless interfaces" ||
		sourceState(db.sources, topology.SourceBridgeFDB) != model.TopologySourceObserved {
		t.Fatalf("wired-only coverage was not normalized: %+v", db.sources)
	}
	if cached := ingest.aliases[2]; !cached.wirelessKnown || len(cached.wireless) != 0 {
		t.Fatalf("wired-only aliases=%+v", cached)
	}
}

func TestTopologyIngestRequiresFreshEmptyRuntimeWirelessInventory(t *testing.T) {
	tests := []struct {
		name        string
		functions   []string
		role        string
		ifacesFresh bool
		ifaces      []string
	}{
		{name: "non-AP without fresh interface inventory", functions: []string{"switch"}, role: "switch"},
		{name: "non-AP with an active wireless interface", functions: []string{"switch"}, role: "switch",
			ifacesFresh: true, ifaces: []string{"wlan0"}},
		{name: "AP with a proven empty runtime interface list", functions: []string{"ap", "switch"}, role: "ap",
			ifacesFresh: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adopted := time.Now().Unix()
			device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: tc.role,
				Functions: tc.functions, AdoptedAt: &adopted}
			db := &fakeTopologyStore{devices: []*store.Device{device}}
			at := int64(1_800_000_000_000)
			snapshot := collector.Snapshot{
				DeviceID: 1, At: time.UnixMilli(at), IfacesFresh: tc.ifacesFresh, Ifaces: tc.ifaces,
			}
			snapshot.Topology = collector.TopologySnapshot{
				Cycle:          true,
				NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", MAC: device.MAC, BridgeOf: []string{"eth1"}}},
				Bridges: []topology.BridgeObservation{{
					DeviceID: 1, Bridge: "br-lan", Entries: []topology.FDBEntry{{Port: 1, MAC: "02:00:00:00:00:44"}},
					STP: &topology.STPState{Bridge: "br-lan", Ports: []topology.STPPort{{
						Name: "eth1", Port: 1, State: "forwarding",
					}}}, PortMedia: map[int]string{1: "wired"},
				}},
				Sources: []model.TopologySourceObservation{
					{DeviceID: 1, Source: collector.TopologySourceNetworkDevices,
						State: model.TopologySourceError, Reason: "source call failure: transport error", ObservedAt: at},
					{DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
						State: model.TopologySourceError, Reason: "source call failure: unsupported operation", ObservedAt: at},
					{DeviceID: 1, Source: topology.SourceBridgeFDB,
						State: model.TopologySourceObserved, ObservedAt: at},
					{DeviceID: 1, Source: collector.TopologySourceBridgeSTP,
						State: model.TopologySourceObserved, ObservedAt: at},
				},
			}
			if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			if len(db.active) != 0 ||
				sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceError ||
				sourceState(db.sources, topology.SourceBridgeFDB) != model.TopologySourceUnknown {
				t.Fatalf("unproven wireless inventory was treated as empty: active=%+v sources=%+v", db.active, db.sources)
			}
			reason := sourceReason(db.sources, topology.SourceBridgeFDB)
			if !strings.Contains(reason, collector.TopologySourceWirelessDevices) ||
				!strings.Contains(reason, "unsupported operation") {
				t.Fatalf("wireless prerequisite failure was obscured: %q", reason)
			}
		})
	}
}

func TestTopologyIngestDoesNotTreatNetworkInventoryWithActiveWirelessAsEmpty(t *testing.T) {
	adopted := time.Now().Unix()
	device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: "switch",
		Functions: []string{"switch"}, AdoptedAt: &adopted}
	db := &fakeTopologyStore{devices: []*store.Device{device}}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at), IfacesFresh: true}
	snapshot.Topology = collector.TopologySnapshot{
		Cycle: true,
		NetworkDevices: []topology.NetworkDevice{
			{Name: "br-lan", MAC: device.MAC, BridgeOf: []string{"phy0-ap0"}},
			{Name: "phy0-ap0", MAC: "02:00:00:00:10:01", Wireless: true},
		},
		Sources: []model.TopologySourceObservation{
			{DeviceID: 1, Source: collector.TopologySourceNetworkDevices,
				State: model.TopologySourceObserved, ObservedAt: at},
			{DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
				State: model.TopologySourceError, Reason: "source call failure: unsupported operation", ObservedAt: at},
		},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceError {
		t.Fatalf("active wireless interface was treated as empty: %+v", db.sources)
	}
}

func TestTopologyIngestDoesNotLetEmptyInventoryOverrideFreshWirelessEvidence(t *testing.T) {
	adopted := time.Now().Unix()
	const clientMAC = "02:00:00:00:00:44"
	device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: "switch",
		Functions: []string{"switch"}, AdoptedAt: &adopted}
	db := &fakeTopologyStore{devices: []*store.Device{device}, clients: []store.Client{{MAC: clientMAC}}}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{
		DeviceID: 1, At: time.UnixMilli(at), IfacesFresh: true, APsFresh: true,
		APs: []collector.AP{{Iface: "phy0-ap0", Stations: map[string]collector.LiveStation{
			clientMAC: {Iface: "phy0-ap0"},
		}}},
		Topology: collector.TopologySnapshot{
			Cycle:          true,
			NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", MAC: device.MAC}},
			Wireless: []topology.WirelessRadio{{Key: "radio0", Interfaces: []topology.WirelessInterface{{
				IfName: "phy0-ap0", BSSID: "02:00:00:00:10:01", Mode: "ap",
			}}}},
			Sources: []model.TopologySourceObservation{
				{DeviceID: 1, Source: collector.TopologySourceNetworkDevices,
					State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
					State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: 1, Source: topology.SourceAssociations,
					State: model.TopologySourceObserved, ObservedAt: at},
			},
		},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceObserved ||
		sourceState(db.sources, topology.SourceAssociations) != model.TopologySourceObserved || len(db.active) != 1 {
		t.Fatalf("empty interface inventory overrode positive wireless evidence: active=%+v sources=%+v",
			db.active, db.sources)
	}
}

func TestTopologyIngestDoesNotTreatEmptyNetworkResponseAsRuntimeProof(t *testing.T) {
	adopted := time.Now().Unix()
	device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: "switch",
		Functions: []string{"switch"}, AdoptedAt: &adopted}
	db := &fakeTopologyStore{devices: []*store.Device{device}}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at)}
	snapshot.Topology = collector.TopologySnapshot{
		Cycle: true,
		Sources: []model.TopologySourceObservation{
			{DeviceID: 1, Source: collector.TopologySourceNetworkDevices,
				State: model.TopologySourceEmpty, ObservedAt: at},
			{DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
				State: model.TopologySourceError, Reason: "source call failure: unsupported operation", ObservedAt: at},
		},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceError {
		t.Fatalf("empty network response was treated as wireless runtime proof: %+v", db.sources)
	}
}

func TestTopologyIngestClearsWirelessAliasesOnlyAfterFreshEmptyRuntimeProof(t *testing.T) {
	adopted := time.Now().Unix()
	device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: "switch",
		Functions: []string{"switch"}, AdoptedAt: &adopted}
	db := &fakeTopologyStore{devices: []*store.Device{device}}
	ingest := newTopologyIngestor(db)
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at)}
	snapshot.Topology = collector.TopologySnapshot{
		Cycle: true,
		Wireless: []topology.WirelessRadio{{Key: "radio0", Interfaces: []topology.WirelessInterface{{
			IfName: "wlan0", Mode: "ap", BSSID: "02:00:00:00:10:01",
		}}}},
		Sources: []model.TopologySourceObservation{{
			DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
			State: model.TopologySourceObserved, ObservedAt: at,
		}},
	}
	if err := ingest.record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if cached := ingest.aliases[1]; !cached.wirelessKnown || len(cached.wireless) != 1 {
		t.Fatalf("unmanaged wireless alias was not retained: %+v", cached)
	}

	snapshot.Topology.Wireless = nil
	snapshot.Topology.Sources[0].State = model.TopologySourceError
	snapshot.Topology.Sources[0].Reason = "source call failure: unsupported operation"
	for _, runtime := range []struct {
		fresh  bool
		ifaces []string
	}{{fresh: false}, {fresh: true, ifaces: []string{"wlan0"}}} {
		at += 1_000
		snapshot.At = time.UnixMilli(at)
		snapshot.IfacesFresh, snapshot.Ifaces = runtime.fresh, runtime.ifaces
		snapshot.Topology.Sources[0].ObservedAt = at
		if err := ingest.record(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
		if cached := ingest.aliases[1]; !cached.wirelessKnown || len(cached.wireless) != 1 {
			t.Fatalf("unproven runtime state cleared unmanaged alias: fresh=%v ifaces=%v aliases=%+v",
				runtime.fresh, runtime.ifaces, cached)
		}
	}

	at += 1_000
	snapshot.At = time.UnixMilli(at)
	snapshot.IfacesFresh, snapshot.Ifaces = true, nil
	snapshot.Topology.Sources[0].ObservedAt = at
	if err := ingest.record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if cached := ingest.aliases[1]; !cached.wirelessKnown || len(cached.wireless) != 0 {
		t.Fatalf("fresh empty runtime proof did not clear wireless aliases: %+v", cached)
	}
	if sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceEmpty {
		t.Fatalf("fresh empty runtime proof did not normalize coverage: %+v", db.sources)
	}
}

func TestTopologyIngestPreservesRemoteAliasFailureDetailsAndLastSuccess(t *testing.T) {
	const (
		parentMAC = "02:00:00:00:00:01"
		remoteMAC = "02:00:00:00:00:02"
		aliasMAC  = "02:00:00:00:00:12"
	)
	devices := func() []*store.Device {
		adopted := time.Now().Unix()
		return []*store.Device{
			{ID: 1, MAC: parentMAC, Role: "ap", Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
			{ID: 2, MAC: remoteMAC, Role: "ap", Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
		}
	}
	bridgeSnapshot := func(at int64) collector.Snapshot {
		snapshot := collector.Snapshot{DeviceID: 1, At: time.UnixMilli(at)}
		snapshot.Topology = collector.TopologySnapshot{
			Cycle:          true,
			NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", MAC: parentMAC, BridgeOf: []string{"eth1"}}},
			Bridges: []topology.BridgeObservation{{
				DeviceID: 1, Bridge: "br-lan", Entries: []topology.FDBEntry{{Port: 1, MAC: aliasMAC}},
				STP: &topology.STPState{Bridge: "br-lan", Ports: []topology.STPPort{{
					Name: "eth1", Port: 1, State: "forwarding",
				}}}, PortMedia: map[int]string{1: "wired"},
			}},
			Sources: []model.TopologySourceObservation{
				{DeviceID: 1, Source: collector.TopologySourceNetworkDevices,
					State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
					State: model.TopologySourceEmpty, ObservedAt: at},
				{DeviceID: 1, Source: topology.SourceBridgeFDB,
					State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: 1, Source: collector.TopologySourceBridgeSTP,
					State: model.TopologySourceObserved, ObservedAt: at},
			},
		}
		return snapshot
	}

	t.Run("failure before inventory is reported by device source and cause", func(t *testing.T) {
		db := &fakeTopologyStore{devices: devices()}
		ingest := newTopologyIngestor(db)
		at := int64(1_800_000_000_000)
		remote := collector.Snapshot{DeviceID: 2, At: time.UnixMilli(at)}
		remote.Topology = collector.TopologySnapshot{Cycle: true, Sources: []model.TopologySourceObservation{
			{DeviceID: 2, Source: collector.TopologySourceNetworkDevices,
				State: model.TopologySourceError, Reason: "source call failure: decode/invalid data", ObservedAt: at},
			{DeviceID: 2, Source: collector.TopologySourceWirelessDevices,
				State: model.TopologySourceEmpty, ObservedAt: at},
		}}
		if err := ingest.record(context.Background(), remote); err != nil {
			t.Fatal(err)
		}
		if err := ingest.record(context.Background(), bridgeSnapshot(at+1_000)); err != nil {
			t.Fatal(err)
		}
		if len(db.active) != 0 || sourceState(db.sources, topology.SourceBridgeFDB) != model.TopologySourceUnknown {
			t.Fatalf("remote alias failure did not block FDB evidence: active=%+v sources=%+v", db.active, db.sources)
		}
		reason := sourceReason(db.sources, topology.SourceBridgeFDB)
		want := "device:2/" + collector.TopologySourceNetworkDevices + " (source call failure: decode/invalid data)"
		if !strings.Contains(reason, want) || strings.Contains(reason, "device:2/"+
			collector.TopologySourceNetworkDevices+" (not observed)") {
			t.Fatalf("remote alias failure lost its cause: %q", reason)
		}
	})

	t.Run("transient failure retains successful aliases", func(t *testing.T) {
		db := &fakeTopologyStore{devices: devices()}
		ingest := newTopologyIngestor(db)
		at := int64(1_800_000_000_000)
		remote := collector.Snapshot{DeviceID: 2, At: time.UnixMilli(at)}
		remote.Topology = collector.TopologySnapshot{
			Cycle:          true,
			NetworkDevices: []topology.NetworkDevice{{Name: "br-lan", MAC: aliasMAC}},
			Sources: []model.TopologySourceObservation{
				{DeviceID: 2, Source: collector.TopologySourceNetworkDevices,
					State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: 2, Source: collector.TopologySourceWirelessDevices,
					State: model.TopologySourceEmpty, ObservedAt: at},
			},
		}
		if err := ingest.record(context.Background(), remote); err != nil {
			t.Fatal(err)
		}
		at += 1_000
		remote.At = time.UnixMilli(at)
		remote.Topology.NetworkDevices = nil
		remote.Topology.Sources = []model.TopologySourceObservation{{
			DeviceID: 2, Source: collector.TopologySourceNetworkDevices,
			State: model.TopologySourceError, Reason: "source call failure: access/permission denied", ObservedAt: at,
		}}
		if err := ingest.record(context.Background(), remote); err != nil {
			t.Fatal(err)
		}
		cached := ingest.aliases[2]
		if !cached.networkKnown || len(cached.network) != 1 || cached.network[0] != aliasMAC ||
			cached.networkState != model.TopologySourceError || cached.networkReason != "source call failure: access/permission denied" {
			t.Fatalf("transient failure erased alias or provenance: %+v", cached)
		}
		if err := ingest.record(context.Background(), bridgeSnapshot(at+1_000)); err != nil {
			t.Fatal(err)
		}
		if len(db.active) != 1 || db.active[0].ChildNode != "device:"+remoteMAC ||
			db.active[0].ParentNode != "device:"+parentMAC ||
			sourceState(db.sources, topology.SourceBridgeFDB) != model.TopologySourceObserved {
			t.Fatalf("last successful alias was not usable after transient failure: active=%+v sources=%+v",
				db.active, db.sources)
		}
	})
}

func sourceState(sources []model.TopologySourceObservation, name string) model.TopologySourceState {
	for _, source := range sources {
		if source.Source == name {
			return source.State
		}
	}
	return ""
}

func sourceReason(sources []model.TopologySourceObservation, name string) string {
	for _, source := range sources {
		if source.Source == name {
			return source.Reason
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

func TestTopologyIngestClosesAssociationAfterFreshEmptyRuntimeProof(t *testing.T) {
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
	snapshot := collector.Snapshot{DeviceID: deviceID, At: time.UnixMilli(at), IfacesFresh: true}
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

func TestTopologyIngestRetainsUnmanagedAssociationWithoutAPFunction(t *testing.T) {
	adopted := time.Now().Unix()
	const clientMAC = "02:00:00:00:00:44"
	device := &store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: "switch",
		Functions: []string{"switch"}, AdoptedAt: &adopted}
	db := &fakeTopologyStore{devices: []*store.Device{device}, clients: []store.Client{{MAC: clientMAC}}}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{
		DeviceID: 1, At: time.UnixMilli(at), IfacesFresh: true, Ifaces: []string{"phy0-ap0"}, APsFresh: true,
		APs: []collector.AP{{Iface: "phy0-ap0", Stations: map[string]collector.LiveStation{
			clientMAC: {Iface: "phy0-ap0"},
		}}},
		Topology: collector.TopologySnapshot{
			Cycle: true,
			NetworkDevices: []topology.NetworkDevice{
				{Name: "br-lan", MAC: device.MAC, BridgeOf: []string{"phy0-ap0"}},
				{Name: "phy0-ap0", Wireless: true},
			},
			Sources: []model.TopologySourceObservation{
				{DeviceID: 1, Source: collector.TopologySourceNetworkDevices,
					State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: 1, Source: collector.TopologySourceWirelessDevices,
					State: model.TopologySourceError, Reason: "source call failure: unsupported operation", ObservedAt: at},
				{DeviceID: 1, Source: topology.SourceAssociations,
					State: model.TopologySourceObserved, ObservedAt: at},
			},
		},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 1 || db.active[0].ChildMAC != clientMAC ||
		db.active[0].ParentPort != "phy0-ap0" || db.active[0].Medium != "wireless" {
		t.Fatalf("unmanaged association was hidden by desired functions: active=%+v sources=%+v",
			db.active, db.sources)
	}
	if sourceState(db.sources, topology.SourceAssociations) != model.TopologySourceObserved {
		t.Fatalf("measured association coverage was rewritten: %+v", db.sources)
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
	if len(db.sources) != 3 ||
		sourceState(db.sources, topology.SourceDefaultRoute) != model.TopologySourceError ||
		sourceState(db.sources, collector.TopologySourceNetworkDevices) != model.TopologySourceError ||
		sourceState(db.sources, collector.TopologySourceWirelessDevices) != model.TopologySourceError {
		t.Fatalf("first failed poll source=%+v", db.sources)
	}
	gaps := strings.Join(ingest.unknownAliasSources(db.devices), "; ")
	for _, source := range []string{
		collector.TopologySourceNetworkDevices,
		collector.TopologySourceWirelessDevices,
	} {
		if !strings.Contains(gaps, source+" (device poll failed)") {
			t.Fatalf("%s failure was not retained in alias cache: %q", source, gaps)
		}
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

func TestTopologyIngestDoesNotVersionRichEdgeOnAssociationOnlyPoll(t *testing.T) {
	adopted := time.Now().Unix()
	deviceID := int64(1)
	const clientMAC = "02:00:00:00:00:44"
	active := model.TopologyEdge{
		ID: 61, ChildNode: "client:" + clientMAC, ChildMAC: clientMAC,
		ParentNode: "device:02:00:00:00:00:01", ParentDeviceID: &deviceID,
		ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "measured",
		ValidFrom: 1_799_999_000_000, LastSeen: 1_799_999_999_000,
		Evidence: []model.TopologyEvidence{
			{Kind: "association", Source: topology.SourceAssociations, DeviceID: &deviceID,
				Detail: map[string]any{"interface": "phy0-ap0", "observed_mac": clientMAC}},
			{Kind: "bridge_fdb", Source: topology.SourceBridgeFDB, DeviceID: &deviceID,
				Detail: map[string]any{"interface": "phy0-ap0", "observed_mac": clientMAC}},
			{Kind: "neighbor", Source: topology.SourceNeighbors(4), DeviceID: &deviceID,
				Detail: map[string]any{"address": "192.0.2.44", "interface": "br-lan"}},
			{Kind: "neighbor", Source: topology.SourceNeighbors(6), DeviceID: &deviceID,
				Detail: map[string]any{"address": "2001:db8::44", "interface": "br-lan"}},
		},
		Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}
	db := &fakeTopologyStore{
		devices: []*store.Device{{
			ID: deviceID, MAC: "02:00:00:00:00:01", Role: "ap",
			Functions: []string{"ap", "switch"}, AdoptedAt: &adopted,
		}},
		clients: []store.Client{{MAC: clientMAC}}, active: []model.TopologyEdge{active}, nextID: active.ID,
	}
	at := int64(1_800_000_000_000)
	snapshot := collector.Snapshot{
		DeviceID: deviceID, At: time.UnixMilli(at), APsFresh: true,
		APs: []collector.AP{{Iface: "phy0-ap0", Stations: map[string]collector.LiveStation{
			clientMAC: {Iface: "phy0-ap0"},
		}}},
		Topology: collector.TopologySnapshot{Sources: []model.TopologySourceObservation{{
			DeviceID: deviceID, Source: topology.SourceAssociations,
			State: model.TopologySourceObserved, ObservedAt: at,
		}}},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.last.Close) != 0 || len(db.last.Open) != 0 || len(db.last.Update) != 1 ||
		db.last.Update[0].ID != active.ID || len(db.active) != 1 || db.active[0].ID != active.ID ||
		db.active[0].ValidFrom != active.ValidFrom || db.active[0].LastSeen != at ||
		len(db.active[0].Evidence) != len(active.Evidence) {
		t.Fatalf("association-only poll versioned rich edge: changes=%+v active=%+v", db.last, db.active)
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

func TestTopologyIngestConcurrentStartupSuppressesReciprocalLLDP(t *testing.T) {
	const (
		wrtMAC    = "02:00:00:00:00:01"
		c6MAC     = "02:00:00:00:00:02"
		wrtClient = "02:00:00:00:00:44"
		c6Client  = "02:00:00:00:00:55"
	)
	adopted := time.Now().Unix()
	at := int64(1_800_000_000_000)
	source := func(deviceID int64, name string) model.TopologySourceObservation {
		return model.TopologySourceObservation{
			DeviceID: deviceID, Source: name, State: model.TopologySourceObserved, ObservedAt: at,
		}
	}
	snapshot := func(deviceID int64) collector.Snapshot {
		remote, port, client := wrtMAC, "eth0.1", c6Client
		if deviceID == 1 {
			remote, port, client = c6MAC, "lan3", wrtClient
		}
		out := collector.Snapshot{
			DeviceID: deviceID, At: time.UnixMilli(at), APsFresh: true,
			APs: []collector.AP{{Iface: "phy0-ap0", Stations: map[string]collector.LiveStation{
				client: {Iface: "phy0-ap0"},
			}}},
			Topology: collector.TopologySnapshot{
				Cycle: true,
				LLDP:  []topology.LLDPLink{{DeviceID: deviceID, Port: port, RemoteMAC: remote}},
				Sources: []model.TopologySourceObservation{
					source(deviceID, topology.SourceLLDP),
					source(deviceID, topology.SourceAssociations),
				},
			},
		}
		if deviceID == 1 {
			out.Topology.Uplinks = []topology.Uplink{{DeviceID: 1, Interface: "wan", Active: true}}
			out.Topology.Sources = append(out.Topology.Sources, source(1, topology.SourceDefaultRoute))
		}
		return out
	}

	for iteration := 0; iteration < 20; iteration++ {
		db := &fakeTopologyStore{
			devices: []*store.Device{
				{ID: 1, MAC: wrtMAC, Role: "gateway",
					Functions: []string{"gateway", "ap", "switch"}, AdoptedAt: &adopted},
				{ID: 2, MAC: c6MAC, Role: "ap",
					Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
			},
			clients: []store.Client{{MAC: wrtClient}, {MAC: c6Client}},
		}
		ingest := newTopologyIngestor(db)
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, deviceID := range []int64{1, 2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- ingest.record(context.Background(), snapshot(deviceID))
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(db.active) != 4 {
			t.Fatalf("iteration %d active links=%d, want 4: %+v", iteration, len(db.active), db.active)
		}
		found := map[string]bool{}
		for _, edge := range db.active {
			found[edge.ChildNode+"\x00"+edge.ParentNode] = true
		}
		if !found["device:"+wrtMAC+"\x00"+topology.InternetNode] ||
			!found["device:"+c6MAC+"\x00device:"+wrtMAC] ||
			found["device:"+wrtMAC+"\x00device:"+c6MAC] ||
			!found["client:"+wrtClient+"\x00device:"+wrtMAC] ||
			!found["client:"+c6Client+"\x00device:"+c6MAC] {
			t.Fatalf("iteration %d hierarchy=%v active=%+v", iteration, found, db.active)
		}
	}
}

func TestTopologyIngestStartupCleansPersistedReciprocalLLDP(t *testing.T) {
	const wrtMAC, c6MAC = "02:00:00:00:00:01", "02:00:00:00:00:02"
	wrtID, c6ID := int64(1), int64(2)
	adopted := time.Now().Unix()
	at := int64(1_800_000_000_000)
	link := func(id int64, child, parent, port string, deviceID int64, source string) model.TopologyEdge {
		return model.TopologyEdge{
			ID: id, ChildNode: child, ParentNode: parent, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wired", Confidence: "measured",
			ValidFrom: at - 1_000, LastSeen: at - 1,
			Evidence: []model.TopologyEvidence{{
				Kind: "lldp_neighbor", Source: source, DeviceID: &deviceID,
			}},
			Ambiguities: []string{},
		}
	}
	root := link(420, "device:"+wrtMAC, topology.InternetNode, "wan", wrtID, topology.SourceDefaultRoute)
	root.ParentDeviceID = nil
	correct := link(421, "device:"+c6MAC, "device:"+wrtMAC, "lan3", wrtID, topology.SourceLLDP)
	reverse := link(422, "device:"+wrtMAC, "device:"+c6MAC, "eth0.1", c6ID, topology.SourceLLDP)
	client := link(423, "client:02:00:00:00:00:55", "device:"+c6MAC, "phy0-ap0", c6ID, topology.SourceAssociations)
	client.Medium = "wireless"
	client.ChildMAC = "02:00:00:00:00:55"
	client.Evidence[0].Kind = "association"
	client.Evidence[0].Detail = map[string]any{
		"interface": "phy0-ap0", "observed_mac": "02:00:00:00:00:55",
	}
	db := &fakeTopologyStore{
		devices: []*store.Device{
			{ID: wrtID, MAC: wrtMAC, Role: "gateway",
				Functions: []string{"gateway", "ap", "switch"}, AdoptedAt: &adopted},
			{ID: c6ID, MAC: c6MAC, Role: "ap",
				Functions: []string{"ap", "switch"}, AdoptedAt: &adopted},
		},
		clients: []store.Client{{MAC: "02:00:00:00:00:55"}},
		active:  []model.TopologyEdge{root, correct, reverse, client}, nextID: 423,
	}
	snapshot := collector.Snapshot{
		DeviceID: c6ID, At: time.UnixMilli(at), APsFresh: true,
		APs: []collector.AP{{Iface: "phy0-ap0", Stations: map[string]collector.LiveStation{
			"02:00:00:00:00:55": {Iface: "phy0-ap0"},
		}}},
		Topology: collector.TopologySnapshot{
			Cycle: true,
			LLDP:  []topology.LLDPLink{{DeviceID: c6ID, Port: "eth0.1", RemoteMAC: wrtMAC}},
			Sources: []model.TopologySourceObservation{
				{DeviceID: c6ID, Source: topology.SourceLLDP, State: model.TopologySourceObserved, ObservedAt: at},
				{DeviceID: c6ID, Source: topology.SourceAssociations, State: model.TopologySourceObserved, ObservedAt: at},
			},
		},
	}
	if err := newTopologyIngestor(db).record(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(db.active) != 3 {
		t.Fatalf("startup cleanup active=%+v changes=%+v", db.active, db.last)
	}
	for _, edge := range db.active {
		if edge.ChildNode == reverse.ChildNode && edge.ParentNode == reverse.ParentNode {
			t.Fatalf("persisted reciprocal edge survived: %+v", db.active)
		}
	}
	if len(db.last.Close) != 1 || db.last.Close[0].ID != reverse.ID {
		t.Fatalf("startup cleanup closed=%+v, want reverse id %d", db.last.Close, reverse.ID)
	}
}
