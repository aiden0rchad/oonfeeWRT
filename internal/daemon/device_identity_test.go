package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/observability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

func boardWithRelease(release string) *collector.Board {
	board := &collector.Board{}
	board.Release.Description = release
	return board
}

func TestWANOnlySnapshotDoesNotMutateFullPollState(t *testing.T) {
	d := openDaemon(t)
	latency := 9.5
	d.sink().Observe(context.Background(), collector.Snapshot{
		DeviceID: 77, MAC: "02:00:00:00:77:01", At: time.Unix(100, 0),
		Tier: collector.Baseline, WANOnly: true,
		WAN: &collector.WANProbe{Up: true, LatencyMS: &latency},
	})

	d.sinkMu.Lock()
	_, reachabilityKnown := d.sinkKnown[77]
	d.sinkMu.Unlock()
	if reachabilityKnown {
		t.Fatal("WAN-only probe changed device reachability state")
	}
	if _, ok := d.liveClients(77); ok {
		t.Fatal("WAN-only probe changed live client state")
	}
	rows := d.Samples.Flush(time.Unix(600, 0))
	if len(rows) != 3 {
		t.Fatalf("WAN-only daemon path emitted non-WAN telemetry: %+v", rows)
	}
	for _, row := range rows {
		switch row.Kind {
		case telemetry.KindSiteWANUp, telemetry.KindSiteWANLoss, telemetry.KindSiteWANLatency:
		default:
			t.Fatalf("WAN-only daemon path emitted %s", row.Kind)
		}
	}
}

func TestDeviceIDReuseStartsUnknownAndDoesNotEmitPredecessorTransitions(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	at := int64(1)
	old := &store.Device{MAC: "02:00:00:00:10:01", Host: "192.0.2.1",
		Name: "old", Role: "ap", Functions: []string{"ap", "switch"}, AdoptedAt: &at}
	if err := d.Store.UpsertDevice(ctx, old); err != nil {
		t.Fatal(err)
	}

	sink := d.sink()
	clientMAC := "02:00:00:00:20:01"
	clients, signal := 1, -42
	oldEpoch := observability.LogEpoch{
		BootID: "11111111-2222-4333-8444-555555555555", PID: 10,
	}
	oldGood := collector.Snapshot{
		DeviceID: old.ID, MAC: old.MAC, Name: old.Name,
		At: time.UnixMilli(101_000), Tier: collector.Baseline,
		Board: boardWithRelease("old-firmware"), APsFresh: true,
		APs: []collector.AP{{Iface: "phy0-ap0", Clients: &clients,
			Stations: map[string]collector.LiveStation{clientMAC: {
				Iface: "phy0-ap0", Signal: &signal,
			}}}},
		IfaceModes:    map[string]string{"mesh0": "mesh"},
		IfaceSections: map[string]string{"mesh0": "old_mesh"},
		NetDevsFresh:  true,
		Interfaces:    map[string]collector.Interface{"mesh0": {Up: true}},
		LogsFresh:     true,
		LogEpoch:      oldEpoch,
		Logs: []observability.LogEntry{
			{ID: 1, TimeMS: 100_000, Priority: 30,
				Message: "phy0-ap0: AP-STA-CONNECTED " + clientMAC},
			{ID: 2, TimeMS: 100_500, Priority: 30,
				Message: "phy0-ap0: FT authentication completed for STA " + clientMAC},
		},
	}
	oldGood.Topology = collector.TopologySnapshot{
		Cycle: true,
		NetworkDevices: []topology.NetworkDevice{{
			Name: "br-lan", MAC: "02:00:00:00:10:aa",
		}},
		Sources: []model.TopologySourceObservation{
			{DeviceID: old.ID, Source: collector.TopologySourceNetworkDevices,
				State: model.TopologySourceObserved, ObservedAt: oldGood.At.UnixMilli()},
			{DeviceID: old.ID, Source: collector.TopologySourceWirelessDevices,
				State: model.TopologySourceEmpty, ObservedAt: oldGood.At.UnixMilli()},
		},
	}
	sink.Observe(ctx, oldGood)
	sink.Observe(ctx, collector.Snapshot{DeviceID: old.ID, MAC: old.MAC,
		Name: old.Name, At: oldGood.At.Add(time.Second), Tier: collector.Baseline,
		Err: errors.New("old router offline")})
	d.rememberNeighbourRun(&api.NeighbourResult{}, nil)
	gateAt := time.Unix(1_700_000_000, 0)
	if !d.reprobes.enter(old.ID, true, gateAt, reprobeMinInterval) {
		t.Fatal("could not seed the old automatic reprobe floor")
	}
	d.reprobes.leave(old.ID)

	if n, ok := d.liveClients(old.ID); !ok || n != 1 {
		t.Fatalf("old live client count = %d,%v", n, ok)
	}
	if stations, ok := d.liveStations(old.ID); !ok || len(stations[clientMAC]) != 1 {
		t.Fatalf("old live stations = %+v,%v", stations, ok)
	}
	if facts := d.meshes.get(old.ID); !facts.ifacesFresh || !facts.up["mesh0"] {
		t.Fatalf("old mesh carry-forward was not seeded: %+v", facts)
	}

	if err := d.deleteDevice(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	replacement := &store.Device{MAC: "02:00:00:00:10:02", Host: "192.0.2.2",
		Name: "replacement", Role: "ap", Functions: []string{"ap", "switch"}, AdoptedAt: &at}
	if err := d.Store.UpsertDevice(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.ID != old.ID {
		t.Fatalf("fixture did not reuse id: old=%d replacement=%d", old.ID, replacement.ID)
	}

	// Before the replacement answers, every live view is unknown rather than
	// inherited from the router that previously owned this integer ID.
	if n, ok := d.liveClients(replacement.ID); ok {
		t.Fatalf("replacement inherited live client count %d", n)
	}
	if stations, ok := d.liveStations(replacement.ID); ok {
		t.Fatalf("replacement inherited live stations %+v", stations)
	}
	if facts := d.meshes.get(replacement.ID); facts.ifacesFresh || facts.netDevsFresh {
		t.Fatalf("replacement inherited mesh carry-forward: %+v", facts)
	}
	if _, _, _, ok := d.LastNeighbourRun(); ok {
		t.Fatal("replacement inherited a predecessor fleet-neighbour result")
	}
	d.sinkMu.Lock()
	_, reachabilityKnown := d.sinkKnown[replacement.ID]
	_, firmwareKnown := d.sinkFirmware[replacement.ID]
	logs, topologyIngest := d.logIngest, d.topologyIngest
	d.sinkMu.Unlock()
	if reachabilityKnown || firmwareKnown {
		t.Fatalf("replacement inherited sink state: reachability=%v firmware=%v",
			reachabilityKnown, firmwareKnown)
	}
	logs.mu.Lock()
	_, cursorLoaded := logs.loaded[replacement.ID]
	logs.mu.Unlock()
	if cursorLoaded {
		t.Fatal("replacement inherited an in-memory log cursor")
	}
	topologyIngest.mu.Lock()
	_, aliasesKnown := topologyIngest.aliases[replacement.ID]
	topologyIngest.mu.Unlock()
	if aliasesKnown {
		t.Fatal("replacement inherited topology aliases")
	}
	d.reprobes.mu.Lock()
	_, reprobeLimited := d.reprobes.last[replacement.ID]
	d.reprobes.mu.Unlock()
	if reprobeLimited {
		t.Fatal("replacement inherited the predecessor's automatic reprobe floor")
	}
	if _, err := d.Store.LoadIngestCursor(ctx, replacement.ID, openWRTLogSource); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replacement durable log cursor = %v, want not found", err)
	}
	for _, source := range mustTopologySources(t, d, ctx) {
		if source.DeviceID == replacement.ID {
			t.Fatalf("replacement inherited topology source state: %+v", source)
		}
	}

	newEpoch := observability.LogEpoch{
		BootID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", PID: 20,
	}
	replacementGood := collector.Snapshot{
		DeviceID: replacement.ID, MAC: replacement.MAC, Name: replacement.Name,
		At: time.UnixMilli(111_000), Tier: collector.Baseline,
		Board: boardWithRelease("new-firmware"), APsFresh: true,
		LogsFresh: true, LogEpoch: newEpoch,
		Logs: []observability.LogEntry{{ID: 1, TimeMS: 110_000, Priority: 30,
			Message: "phy1-ap0: AP-STA-CONNECTED " + clientMAC}},
	}
	replacementGood.Topology = collector.TopologySnapshot{
		Cycle: true,
		NetworkDevices: []topology.NetworkDevice{{
			Name: "br-lan", MAC: "02:00:00:00:10:bb",
		}},
		Sources: []model.TopologySourceObservation{
			{DeviceID: replacement.ID, Source: collector.TopologySourceNetworkDevices,
				State: model.TopologySourceObserved, ObservedAt: replacementGood.At.UnixMilli()},
			{DeviceID: replacement.ID, Source: collector.TopologySourceWirelessDevices,
				State: model.TopologySourceEmpty, ObservedAt: replacementGood.At.UnixMilli()},
		},
	}
	sink.Observe(ctx, replacementGood)

	if n, ok := d.liveClients(replacement.ID); !ok || n != 0 {
		t.Fatalf("replacement's first answer = %d,%v, want known zero", n, ok)
	}
	if stations, ok := d.liveStations(replacement.ID); !ok || len(stations) != 0 {
		t.Fatalf("replacement's first station answer = %+v,%v", stations, ok)
	}
	events, err := d.Store.RecentEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	association := ""
	fastTransition := false
	for _, event := range events {
		if event.DeviceID == nil || *event.DeviceID != replacement.ID {
			continue
		}
		if event.Event == "device.reachable" || event.Event == "device.firmware_changed" {
			t.Fatalf("replacement emitted a predecessor transition: %+v", event)
		}
		if event.ClientMAC == clientMAC {
			association = event.Event
			if detail, ok := event.Detail.(map[string]any); ok {
				fastTransition, _ = detail["fast_transition"].(bool)
			}
		}
	}
	if association != "client.connect" {
		t.Fatalf("replacement's first association event = %q, want client.connect", association)
	}
	if fastTransition {
		t.Fatal("replacement inherited the predecessor's pending FT marker")
	}
	d.reprobes.mu.Lock()
	_, reprobed := d.reprobes.last[replacement.ID]
	d.reprobes.mu.Unlock()
	if reprobed {
		t.Fatal("replacement's first firmware observation triggered an automatic reprobe")
	}
}

func TestReplacementRegistrationWaitsForCommittedDeletionPurge(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	adopted := int64(1)
	old := &store.Device{MAC: "02:00:00:00:40:01", Host: "192.0.2.41",
		Name: "old", Role: "ap", AdoptedAt: &adopted}
	if err := d.Store.UpsertDevice(ctx, old); err != nil {
		t.Fatal(err)
	}
	_ = d.sink() // construct every post-commit ingestor before blocking one

	// Hold the first post-commit cache lock. deleteDevice has committed the row
	// deletion when DeviceByID becomes not found, but it still owns
	// telemetryLifecycle until this lock is released and the entire purge ends.
	d.logIngest.mu.Lock()
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- d.deleteDevice(ctx, old.ID) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := d.Store.DeviceByID(ctx, old.ID)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			d.logIngest.mu.Unlock()
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			d.logIngest.mu.Unlock()
			t.Fatal("device deletion did not commit before the purge barrier")
		}
		time.Sleep(time.Millisecond)
	}

	replacement := &store.Device{MAC: "02:00:00:00:40:02", Host: "192.0.2.42",
		Name: "replacement", Role: "ap", AdoptedAt: &adopted}
	registrationDone := make(chan error, 1)
	base := time.Now().Truncate(telemetry.DefaultWindow).Add(-telemetry.DefaultWindow)
	go func() {
		if err := d.registerDevice(ctx, replacement); err != nil {
			registrationDone <- err
			return
		}
		clients, signal := 1, -38
		snapshot := collector.Snapshot{DeviceID: replacement.ID, MAC: replacement.MAC,
			At: base.Add(time.Second), APsFresh: true,
			APs: []collector.AP{{Iface: "phy0-ap0", Clients: &clients,
				Stations: map[string]collector.LiveStation{
					"02:00:00:00:40:ff": {Iface: "phy0-ap0", Signal: &signal},
				}}},
			IfaceModes: map[string]string{"mesh-new": "mesh"}, NetDevsFresh: true,
			Interfaces: map[string]collector.Interface{"mesh-new": {Up: true}},
		}
		d.recordLiveClients(snapshot)
		d.recordLiveStations(snapshot)
		d.meshes.put(snapshot)
		d.Samples.Gauge(telemetry.SeriesKey{DeviceID: replacement.ID,
			Kind: telemetry.KindLoad1}, base.Add(time.Second).Unix(), 7)
		registrationDone <- d.Store.RecordOwned(ctx, []store.OwnedSection{{
			DeviceID: replacement.ID, Config: "wireless", Section: "replacement_owned",
			RenderedHash: "replacement", AppliedAt: 2,
		}})
	}()

	select {
	case err := <-registrationDone:
		d.logIngest.mu.Unlock()
		<-deleteDone
		t.Fatalf("replacement crossed the post-commit purge boundary early: %v", err)
	case <-time.After(50 * time.Millisecond):
		if _, err := d.Store.DeviceByID(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
			d.logIngest.mu.Unlock()
			<-deleteDone
			t.Fatalf("replacement row appeared while purge was blocked: %v", err)
		}
	}
	d.logIngest.mu.Unlock()
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err := <-registrationDone; err != nil {
		t.Fatal(err)
	}
	if replacement.ID != old.ID {
		t.Fatalf("fixture did not reuse id: old=%d replacement=%d", old.ID, replacement.ID)
	}

	if clients, ok := d.liveClients(replacement.ID); !ok || clients != 1 {
		t.Fatalf("replacement live state was purged: %d,%v", clients, ok)
	}
	if facts := d.meshes.get(replacement.ID); !facts.ifacesFresh || !facts.up["mesh-new"] {
		t.Fatalf("replacement mesh state was purged: %+v", facts)
	}
	owned, err := d.ownedSections(ctx, replacement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].Section != "replacement_owned" {
		t.Fatalf("replacement ownership was purged: %+v", owned)
	}

	maintainer := telemetry.NewMaintainer(d.Store, d.Samples, quietLogger())
	maintainer.Lifecycle = &d.telemetryLifecycle
	maintainer.Now = func() time.Time { return base.Add(2 * telemetry.DefaultWindow) }
	maintainer.Tick(ctx)
	series, err := d.Store.QuerySeries(ctx, replacement.ID, string(telemetry.KindLoad1), "",
		base, base.Add(3*telemetry.DefaultWindow))
	if err != nil {
		t.Fatal(err)
	}
	if len(series.Points) != 1 || series.Points[0].Avg != 7 {
		t.Fatalf("replacement samples were purged: %+v", series.Points)
	}

	client, apiBase := serveAuthenticatedDaemon(t, d)
	ws := dialDaemonLive(t, client, apiBase)
	writeLiveMessage(t, ws, map[string]any{
		"type": "subscribe", "topic": "device.stats", "device_id": replacement.ID,
	})
	readLiveUntil(t, ws, func(message map[string]any) bool {
		return message["type"] == "subscribed"
	})
	d.api.Hub.Publish(replacement.ID, map[string]any{
		"type": "stats", "marker": "replacement-survived-purge",
	})
	readLiveUntil(t, ws, func(message map[string]any) bool {
		return message["marker"] == "replacement-survived-purge"
	})
}

func mustTopologySources(t *testing.T, d *Daemon, ctx context.Context) []model.TopologySourceObservation {
	t.Helper()
	sources, err := d.Store.TopologySourceStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return sources
}
