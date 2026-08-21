package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type cascadeStoreStub struct {
	devices map[int64]*store.Device
	edges   []model.TopologyEdge
	states  []model.TopologySourceObservation
	events  []store.Event
	fail    int
}

func (s *cascadeStoreStub) DeviceByID(_ context.Context, id int64) (*store.Device, error) {
	device := s.devices[id]
	if device == nil {
		return nil, store.ErrNotFound
	}
	copy := *device
	return &copy, nil
}

func (s *cascadeStoreStub) TopologyEdgesAt(context.Context, int64) ([]model.TopologyEdge, error) {
	return append([]model.TopologyEdge(nil), s.edges...), nil
}

func (s *cascadeStoreStub) TopologySourceStates(context.Context) ([]model.TopologySourceObservation, error) {
	return append([]model.TopologySourceObservation(nil), s.states...), nil
}

func (s *cascadeStoreStub) LogEvent(_ context.Context, event store.Event) error {
	if s.fail > 0 {
		s.fail--
		return errors.New("forced event write failure")
	}
	s.events = append(s.events, event)
	return nil
}

func cascadeFixture() *cascadeStoreStub {
	parentID, otherParentID := int64(1), int64(5)
	devices := map[int64]*store.Device{
		1: {ID: 1, MAC: "00:00:00:00:00:01"},
		2: {ID: 2, MAC: "00:00:00:00:00:02"},
		3: {ID: 3, MAC: "00:00:00:00:00:03"},
		4: {ID: 4, MAC: "00:00:00:00:00:04"},
		5: {ID: 5, MAC: "00:00:00:00:00:05"},
	}
	return &cascadeStoreStub{devices: devices, edges: []model.TopologyEdge{
		{ChildNode: "device:00:00:00:00:00:02", ParentNode: "device:00:00:00:00:00:01", ParentDeviceID: &parentID,
			LastSeen: 100_000, Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "bridge-fdb", DeviceID: &parentID}}},
		{ChildNode: "device:00:00:00:00:00:03", ParentNode: "device:00:00:00:00:00:01", ParentDeviceID: &parentID,
			LastSeen: 100_000, Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "bridge-fdb", DeviceID: &parentID}}},
		{ChildNode: "device:00:00:00:00:00:04", ParentNode: "device:00:00:00:00:00:05", ParentDeviceID: &otherParentID,
			LastSeen: 100_000, Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "bridge-fdb", DeviceID: &otherParentID}}},
	}, states: []model.TopologySourceObservation{
		{DeviceID: parentID, Source: "bridge-fdb", State: model.TopologySourceObserved, ObservedAt: 100_000},
		{DeviceID: otherParentID, Source: "bridge-fdb", State: model.TopologySourceObserved, ObservedAt: 100_000},
	}}
}

func TestCascadeGrouperEmitsOneOfflineAndOnlineEventForExactSiblings(t *testing.T) {
	ctx := context.Background()
	st := cascadeFixture()
	g := newCascadeGrouper(st, quietLogger(), 10*time.Second, 10*time.Second, false)
	base := time.Unix(100, 0)

	for _, observation := range []struct {
		id int64
		at time.Time
	}{{2, base}, {3, base.Add(2 * time.Second)}, {4, base.Add(3 * time.Second)}} {
		if err := g.observe(ctx, cascadeOffline, observation.id, observation.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.flush(ctx, base.Add(9*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if len(st.events) != 0 {
		t.Fatalf("cascade emitted before its window closed: %+v", st.events)
	}
	if err := g.flush(ctx, base.Add(14*time.Second), false); err != nil {
		t.Fatal(err)
	}
	assertCascadeEvent(t, st.events, 0, cascadeOfflineEvent, 1, []int64{2, 3})

	if err := g.observe(ctx, cascadeOnline, 2, base.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.observe(ctx, cascadeOnline, 3, base.Add(24*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.observe(ctx, cascadeOnline, 4, base.Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.flush(ctx, base.Add(35*time.Second), false); err != nil {
		t.Fatal(err)
	}
	assertCascadeEvent(t, st.events, 1, cascadeOnlineEvent, 1, []int64{2, 3})
	if len(st.events) != 2 {
		t.Fatalf("isolated sibling or retry emitted another cascade: %+v", st.events)
	}
	if err := g.flush(ctx, base.Add(time.Hour), false); err != nil || len(st.events) != 2 {
		t.Fatalf("completed windows were emitted twice: events=%+v err=%v", st.events, err)
	}
}

func TestCascadeGrouperRetainsFailedBatchForExactRetry(t *testing.T) {
	ctx := context.Background()
	st := cascadeFixture()
	st.fail = 1
	g := newCascadeGrouper(st, quietLogger(), 10*time.Second, 10*time.Second, false)
	base := time.Unix(100, 0)
	if err := g.observe(ctx, cascadeOffline, 2, base); err != nil {
		t.Fatal(err)
	}
	if err := g.observe(ctx, cascadeOffline, 3, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.flush(ctx, base.Add(10*time.Second), false); err == nil {
		t.Fatal("failed persistence was reported as success")
	}
	if len(st.events) != 0 || len(g.pending) != 1 {
		t.Fatalf("failed batch was lost or claimed durable: events=%+v pending=%d", st.events, len(g.pending))
	}
	if err := g.flush(ctx, base.Add(11*time.Second), false); err != nil {
		t.Fatal(err)
	}
	assertCascadeEvent(t, st.events, 0, cascadeOfflineEvent, 1, []int64{2, 3})
	if err := g.flush(ctx, base.Add(12*time.Second), false); err != nil || len(st.events) != 1 {
		t.Fatalf("successful retry duplicated: events=%+v err=%v", st.events, err)
	}
}

func TestCascadeWindowCoversIndependentlyPhasedBaselinePolls(t *testing.T) {
	ctx := context.Background()
	st := cascadeFixture()
	g := newCascadeGrouper(st, quietLogger(), cascadeOfflineWindow, cascadeOnlineWindow, false)
	base := time.Unix(100, 0)
	if err := g.observe(ctx, cascadeOffline, 2, base); err != nil {
		t.Fatal(err)
	}
	if err := g.observe(ctx, cascadeOffline, 3, base.Add(50*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.flush(ctx, base.Add(cascadeOfflineWindow-time.Millisecond), false); err != nil {
		t.Fatal(err)
	}
	if len(st.events) != 0 {
		t.Fatalf("normal baseline skew closed early: %+v", st.events)
	}
	if err := g.flush(ctx, base.Add(cascadeOfflineWindow), false); err != nil {
		t.Fatal(err)
	}
	assertCascadeEvent(t, st.events, 0, cascadeOfflineEvent, 1, []int64{2, 3})
}

func TestCascadeOnlineWindowCoversBackedOffRecoveryPolls(t *testing.T) {
	ctx := context.Background()
	st := cascadeFixture()
	g := newCascadeGrouper(st, quietLogger(), cascadeOfflineWindow, cascadeOnlineWindow, false)
	base := time.Unix(100, 0)
	if err := g.observe(ctx, cascadeOnline, 2, base); err != nil {
		t.Fatal(err)
	}
	if err := g.observe(ctx, cascadeOnline, 3, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := g.flush(ctx, base.Add(cascadeOnlineWindow-time.Millisecond), false); err != nil {
		t.Fatal(err)
	}
	if len(st.events) != 0 {
		t.Fatalf("backed-off recovery window closed early: %+v", st.events)
	}
	if err := g.flush(ctx, base.Add(cascadeOnlineWindow), false); err != nil {
		t.Fatal(err)
	}
	assertCascadeEvent(t, st.events, 0, cascadeOnlineEvent, 1, []int64{2, 3})
}

func TestCascadeGrouperRejectsErrorAndStaleTopologyEvidence(t *testing.T) {
	ctx := context.Background()
	base := time.Unix(100, 0)
	for _, tc := range []struct {
		name   string
		mutate func(*cascadeStoreStub)
	}{
		{name: "source error", mutate: func(st *cascadeStoreStub) {
			st.states[0].State = model.TopologySourceError
		}},
		{name: "edge stale", mutate: func(st *cascadeStoreStub) {
			for i := 0; i < 2; i++ {
				st.edges[i].LastSeen = base.Add(-maxCascadeTopologyAge - time.Millisecond).UnixMilli()
			}
		}},
		{name: "source stale", mutate: func(st *cascadeStoreStub) {
			st.states[0].ObservedAt = base.Add(-maxCascadeTopologyAge - time.Millisecond).UnixMilli()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cascadeFixture()
			tc.mutate(st)
			g := newCascadeGrouper(st, quietLogger(), 10*time.Second, 10*time.Second, false)
			if err := g.observe(ctx, cascadeOffline, 2, base); err != nil {
				t.Fatal(err)
			}
			if err := g.observe(ctx, cascadeOffline, 3, base.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := g.flush(ctx, base.Add(20*time.Second), false); err != nil {
				t.Fatal(err)
			}
			if len(st.events) != 0 || len(g.pending) != 0 {
				t.Fatalf("uncertain topology produced a cascade: events=%+v pending=%d", st.events, len(g.pending))
			}
		})
	}
}

func TestCascadeGrouperForgetsDeletedIdentityBeforeIDReuse(t *testing.T) {
	ctx := context.Background()
	st := cascadeFixture()
	g := newCascadeGrouper(st, quietLogger(), 10*time.Second, 10*time.Second, false)
	base := time.Unix(100, 0)
	if err := g.observe(ctx, cascadeOffline, 2, base); err != nil {
		t.Fatal(err)
	}
	if err := g.observe(ctx, cascadeOffline, 3, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	g.forgetDevice(3)
	st.devices[3] = &store.Device{ID: 3, MAC: "00:00:00:00:00:33"}
	if err := g.flush(ctx, base.Add(20*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if len(st.events) != 0 {
		t.Fatalf("replacement device inherited a pending cascade: %+v", st.events)
	}
}

func assertCascadeEvent(t *testing.T, events []store.Event, index int, name string,
	upstream int64, members []int64) {
	t.Helper()
	if len(events) <= index {
		t.Fatalf("missing %s event in %+v", name, events)
	}
	event := events[index]
	if event.Event != name || event.DeviceID == nil || *event.DeviceID != upstream {
		t.Fatalf("event[%d]=%+v, want %s on upstream %d", index, event, name, upstream)
	}
	detail, ok := event.Detail.(map[string]any)
	if !ok {
		t.Fatalf("event detail type=%T", event.Detail)
	}
	if got, ok := detail["device_ids"].([]int64); !ok || !reflect.DeepEqual(got, members) {
		t.Fatalf("members=%#v, want %#v", detail["device_ids"], members)
	}
}

func TestReachabilitySinkKeepsImmediateIndividualEventsAlongsideCascade(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	adopted := int64(1)
	devices := []*store.Device{
		{MAC: "00:00:00:00:10:01", Name: "upstream", Role: "gateway", AdoptedAt: &adopted},
		{MAC: "00:00:00:00:10:02", Name: "child-a", Role: "ap", AdoptedAt: &adopted},
		{MAC: "00:00:00:00:10:03", Name: "child-b", Role: "ap", AdoptedAt: &adopted},
		{MAC: "00:00:00:00:10:04", Name: "isolated", Role: "ap", AdoptedAt: &adopted},
		{MAC: "00:00:00:00:10:05", Name: "other-upstream", Role: "gateway", AdoptedAt: &adopted},
	}
	for _, device := range devices {
		if err := d.Store.UpsertDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the production flusher's wall clock behind this deterministic test;
	// the test advances the grouping clock explicitly below.
	base := time.Now().Add(time.Hour)
	parent, otherParent := devices[0].ID, devices[4].ID
	for _, edge := range []*model.TopologyEdge{
		{ChildNode: "device:" + devices[1].MAC, ParentNode: "device:" + devices[0].MAC, ParentDeviceID: &parent,
			Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "bridge-fdb", DeviceID: &parent}}},
		{ChildNode: "device:" + devices[2].MAC, ParentNode: "device:" + devices[0].MAC, ParentDeviceID: &parent,
			Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "bridge-fdb", DeviceID: &parent}}},
		{ChildNode: "device:" + devices[3].MAC, ParentNode: "device:" + devices[4].MAC, ParentDeviceID: &otherParent,
			Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "bridge-fdb", DeviceID: &otherParent}}},
	} {
		edge.Medium, edge.Confidence = "wired", "measured"
		edge.ValidFrom, edge.LastSeen = base.Add(-time.Minute).UnixMilli(), base.UnixMilli()
		edge.Ambiguities = []string{}
		if err := d.Store.SaveTopologyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []model.TopologySourceObservation{
		{DeviceID: parent, Source: "bridge-fdb", State: model.TopologySourceObserved, ObservedAt: base.UnixMilli()},
		{DeviceID: otherParent, Source: "bridge-fdb", State: model.TopologySourceObserved, ObservedAt: base.UnixMilli()},
	} {
		if err := d.Store.SaveTopologySourceState(ctx, state); err != nil {
			t.Fatal(err)
		}
	}

	sink := d.sink()
	for _, device := range devices[1:4] {
		sink.Observe(ctx, collector.Snapshot{
			DeviceID: device.ID, MAC: device.MAC, At: base.Add(-time.Second), Tier: collector.Baseline,
		})
	}
	for i, device := range devices[1:4] {
		sink.Observe(ctx, collector.Snapshot{
			DeviceID: device.ID, MAC: device.MAC, At: base.Add(time.Duration(i) * time.Second),
			Tier: collector.Baseline, Err: errors.New("offline"),
		})
	}
	assertReachabilityCounts(t, d, 3, 0, 0, 0)

	d.sinkMu.Lock()
	grouper := d.cascadeIngest
	d.sinkMu.Unlock()
	if err := grouper.flush(ctx, base.Add(80*time.Second), false); err != nil {
		t.Fatal(err)
	}
	assertReachabilityCounts(t, d, 3, 0, 1, 0)

	for i, device := range devices[1:4] {
		sink.Observe(ctx, collector.Snapshot{
			DeviceID: device.ID, MAC: device.MAC, At: base.Add(100*time.Second + time.Duration(i)*time.Second),
			Tier: collector.Baseline,
		})
	}
	assertReachabilityCounts(t, d, 3, 3, 1, 0)
	if err := grouper.flush(ctx, base.Add(13*time.Minute), false); err != nil {
		t.Fatal(err)
	}
	assertReachabilityCounts(t, d, 3, 3, 1, 1)
}

func assertReachabilityCounts(t *testing.T, d *Daemon, unreachable, reachable,
	offlineCascade, onlineCascade int) {
	t.Helper()
	events, err := d.Store.RecentEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Event]++
		if event.Event == cascadeOfflineEvent || event.Event == cascadeOnlineEvent {
			var detail struct {
				DeviceIDs []int64 `json:"device_ids"`
			}
			if err := json.Unmarshal(event.Detail.(json.RawMessage), &detail); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(detail.DeviceIDs, []int64{dID(d, "child-a"), dID(d, "child-b")}) {
				t.Fatalf("cascade members=%v", detail.DeviceIDs)
			}
		}
	}
	want := map[string]int{
		"device.unreachable": unreachable,
		"device.reachable":   reachable,
		cascadeOfflineEvent:  offlineCascade,
		cascadeOnlineEvent:   onlineCascade,
	}
	for name, count := range want {
		if counts[name] != count {
			t.Fatalf("%s count=%d, want %d (all=%v)", name, counts[name], count, counts)
		}
	}
}

func dID(d *Daemon, name string) int64 {
	devices, _ := d.Store.Devices(context.Background())
	for _, device := range devices {
		if device.Name == name {
			return device.ID
		}
	}
	return 0
}
