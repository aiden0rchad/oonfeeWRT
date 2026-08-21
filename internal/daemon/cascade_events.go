package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	// Baseline pollers are independently phased across one 60-second cadence.
	// The margin keeps one physical outage in one group despite scheduling and
	// response skew, without turning unrelated later transitions into a batch.
	cascadeOfflineWindow = 75 * time.Second
	// Failed pollers back off independently to a ten-minute ceiling. Recovery
	// grouping needs that whole skew plus a response/scheduling margin.
	cascadeOnlineWindow   = 11 * time.Minute
	cascadeFlushInterval  = time.Second
	maxCascadeTopologyAge = 31 * time.Minute

	cascadeOfflineEvent = "topology.cascade_offline"
	cascadeOnlineEvent  = "topology.cascade_online"
)

type cascadeEventStore interface {
	DeviceByID(context.Context, int64) (*store.Device, error)
	TopologyEdgesAt(context.Context, int64) ([]model.TopologyEdge, error)
	TopologySourceStates(context.Context) ([]model.TopologySourceObservation, error)
	LogEvent(context.Context, store.Event) error
}

type cascadeDirection string

const (
	cascadeOffline cascadeDirection = "offline"
	cascadeOnline  cascadeDirection = "online"
)

type cascadeKey struct {
	direction cascadeDirection
	upstream  string
}

type cascadeBatch struct {
	upstreamDeviceID *int64
	firstAt          time.Time
	lastAt           time.Time
	members          map[int64]struct{}
}

// cascadeGrouper supplements, rather than replaces, immediate per-device
// reachability events. One worker owns all deadlines; fleet size never creates
// a goroutine or timer per device.
type cascadeGrouper struct {
	store         cascadeEventStore
	log           *slog.Logger
	offlineWindow time.Duration
	onlineWindow  time.Duration

	mu      sync.Mutex
	pending map[cascadeKey]*cascadeBatch

	running  bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func newCascadeGrouper(events cascadeEventStore, log *slog.Logger,
	offlineWindow, onlineWindow time.Duration, run bool) *cascadeGrouper {
	if offlineWindow <= 0 {
		offlineWindow = cascadeOfflineWindow
	}
	if onlineWindow <= 0 {
		onlineWindow = cascadeOnlineWindow
	}
	if log == nil {
		log = slog.Default()
	}
	g := &cascadeGrouper{
		store: events, log: log,
		offlineWindow: offlineWindow, onlineWindow: onlineWindow,
		pending: map[cascadeKey]*cascadeBatch{},
		stop:    make(chan struct{}), done: make(chan struct{}), running: run,
	}
	if run {
		go g.run()
	}
	return g
}

func (g *cascadeGrouper) run() {
	defer close(g.done)
	interval := cascadeFlushInterval
	if quarter := g.offlineWindow / 4; quarter > 0 && quarter < interval {
		interval = quarter
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := g.flush(ctx, now, false); err != nil {
				g.log.Error("could not persist grouped topology transition", "err", err)
			}
			cancel()
		case <-g.stop:
			return
		}
	}
}

// observe resolves the child's current, unambiguous parent before adding the
// transition. An Internet root is not an upstream device: grouping gateways
// merely because they all reach the Internet would invent a cascade.
func (g *cascadeGrouper) observe(ctx context.Context, direction cascadeDirection,
	deviceID int64, at time.Time) error {
	if direction != cascadeOffline && direction != cascadeOnline {
		return fmt.Errorf("cascade: unsupported direction %q", direction)
	}
	if deviceID <= 0 || at.IsZero() {
		return errors.New("cascade: transition requires a device and observation time")
	}
	// Close any older window before admitting this transition. A failed write
	// remains intact for retry; the new transition still has its immediate
	// per-device event and is deliberately not folded into an expired batch.
	if err := g.flush(ctx, at, false); err != nil {
		return err
	}

	upstream, err := g.parent(ctx, deviceID, at)
	if err != nil {
		return err
	}
	if upstream == nil {
		return nil
	}
	key := cascadeKey{direction: direction, upstream: upstream.node}
	window := g.window(direction)

	g.mu.Lock()
	defer g.mu.Unlock()
	if batch := g.pending[key]; batch != nil {
		if !at.Before(batch.firstAt.Add(window)) {
			return nil
		}
		batch.members[deviceID] = struct{}{}
		if at.After(batch.lastAt) {
			batch.lastAt = at
		}
		return nil
	}
	batch := &cascadeBatch{
		firstAt: at, lastAt: at, members: map[int64]struct{}{deviceID: {}},
	}
	if upstream.deviceID != nil {
		id := *upstream.deviceID
		batch.upstreamDeviceID = &id
	}
	g.pending[key] = batch
	return nil
}

func (g *cascadeGrouper) window(direction cascadeDirection) time.Duration {
	if direction == cascadeOnline {
		return g.onlineWindow
	}
	return g.offlineWindow
}

type cascadeParent struct {
	node     string
	deviceID *int64
}

func (g *cascadeGrouper) parent(ctx context.Context, deviceID int64,
	at time.Time) (*cascadeParent, error) {
	device, err := g.store.DeviceByID(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("cascade: device %d identity: %w", deviceID, err)
	}
	child := "device:" + strings.ToLower(device.MAC)
	edges, err := g.store.TopologyEdgesAt(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("cascade: active topology: %w", err)
	}
	states, err := g.store.TopologySourceStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("cascade: topology source state: %w", err)
	}
	stateByEvidence := make(map[string]model.TopologySourceObservation, len(states))
	for _, state := range states {
		stateByEvidence[topologyEvidenceKey(state.DeviceID, state.Source)] = state
	}
	var found *cascadeParent
	for _, edge := range edges {
		if edge.ChildNode != child || edge.ParentNode == "synthetic:internet" {
			continue
		}
		if !freshTopologyEdge(edge, stateByEvidence, at) {
			continue
		}
		candidate := &cascadeParent{node: edge.ParentNode}
		if edge.ParentDeviceID != nil {
			id := *edge.ParentDeviceID
			candidate.deviceID = &id
		}
		if found == nil {
			found = candidate
			continue
		}
		if found.node != candidate.node || !sameOptionalID(found.deviceID, candidate.deviceID) {
			// Competing active parents are explicit uncertainty, not evidence
			// that either parent's other children share this transition.
			return nil, nil
		}
	}
	return found, nil
}

func freshTopologyEdge(edge model.TopologyEdge,
	states map[string]model.TopologySourceObservation, at time.Time) bool {
	cutoff := at.Add(-maxCascadeTopologyAge).UnixMilli()
	if edge.LastSeen < cutoff || len(edge.Evidence) == 0 {
		return false
	}
	for _, evidence := range edge.Evidence {
		if evidence.DeviceID == nil {
			continue
		}
		state, ok := states[topologyEvidenceKey(*evidence.DeviceID, evidence.Source)]
		if ok && state.State == model.TopologySourceObserved && state.ObservedAt >= cutoff {
			return true
		}
	}
	return false
}

func topologyEvidenceKey(deviceID int64, source string) string {
	return fmt.Sprintf("%d\x00%s", deviceID, source)
}

func sameOptionalID(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// flush persists every closed window. Successful batches are removed; failed
// batches remain byte-for-byte intact so a retry cannot silently change their
// membership. force is used only after polling has stopped during shutdown.
func (g *cascadeGrouper) flush(ctx context.Context, now time.Time, force bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	keys := make([]cascadeKey, 0, len(g.pending))
	for key, batch := range g.pending {
		if force || !now.Before(batch.firstAt.Add(g.window(key.direction))) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].direction != keys[j].direction {
			return keys[i].direction < keys[j].direction
		}
		return keys[i].upstream < keys[j].upstream
	})

	var errs []error
	for _, key := range keys {
		batch := g.pending[key]
		if len(batch.members) < 2 {
			delete(g.pending, key)
			continue
		}
		ids := make([]int64, 0, len(batch.members))
		for id := range batch.members {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

		event, severity := cascadeOfflineEvent, "warning"
		if key.direction == cascadeOnline {
			event, severity = cascadeOnlineEvent, "info"
		}
		err := g.store.LogEvent(ctx, store.Event{
			TS: batch.lastAt.Unix(), DeviceID: batch.upstreamDeviceID,
			Category: "device", Severity: severity, Event: event,
			Detail: map[string]any{
				"upstream_node":      key.upstream,
				"upstream_device_id": batch.upstreamDeviceID,
				"device_ids":         ids,
				"window_started_at":  batch.firstAt.UnixMilli(),
				"window_ended_at":    batch.firstAt.Add(g.window(key.direction)).UnixMilli(),
			},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s via %s: %w", key.direction, key.upstream, err))
			continue
		}
		delete(g.pending, key)
	}
	return errors.Join(errs...)
}

// forgetDevice is part of the inventory identity boundary. A pending batch
// must neither name a deleted member nor attach an old upstream to a new router
// after SQLite reuses its numeric ID.
func (g *cascadeGrouper) forgetDevice(deviceID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, batch := range g.pending {
		if batch.upstreamDeviceID != nil && *batch.upstreamDeviceID == deviceID {
			delete(g.pending, key)
			continue
		}
		delete(batch.members, deviceID)
	}
}

func (g *cascadeGrouper) stopAndFlush(ctx context.Context) error {
	if g.running {
		g.stopOnce.Do(func() { close(g.stop) })
		<-g.done
	}
	return g.flush(ctx, time.Now(), true)
}
