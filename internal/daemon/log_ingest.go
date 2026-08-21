package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/observability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	openWRTLogSource       = "openwrt-logd"
	emptyLogCursorPosition = "empty"
)

type logIngestStore interface {
	LoadIngestCursor(context.Context, int64, string) (store.IngestCursor, error)
	AppendEventsAndCursor(context.Context, []store.Event, store.IngestCursor) (int, error)
	LatestClientAssociationEvents(context.Context) ([]store.Event, error)
}

// logIngestor serialises pages because roaming is correlated across devices.
// Its cursor and correlator only advance after the matching SQLite transaction.
type logIngestor struct {
	mu           sync.Mutex
	store        logIngestStore
	cursors      map[int64]observability.LogCursor
	empty        map[int64]bool
	gaps         map[int64]int64
	loaded       map[int64]bool
	restored     bool
	associations *observability.AssociationCorrelator
}

func newLogIngestor(db logIngestStore) *logIngestor {
	return &logIngestor{
		store:        db,
		cursors:      map[int64]observability.LogCursor{},
		empty:        map[int64]bool{},
		gaps:         map[int64]int64{},
		loaded:       map[int64]bool{},
		associations: observability.NewAssociationCorrelator(),
	}
}

// forgetDevice drops every process-local producer position and association
// inference that could name a reusable device ID. The correlator is rebuilt
// from durable events on the next page; cursor and event persistence advance
// together, so that rebuild loses no committed state and also clears pending
// FT markers that do not themselves carry a device ID.
func (l *logIngestor) forgetDevice(deviceID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.cursors, deviceID)
	delete(l.empty, deviceID)
	delete(l.gaps, deviceID)
	delete(l.loaded, deviceID)
	l.associations = observability.NewAssociationCorrelator()
	l.restored = false
}

func (l *logIngestor) record(ctx context.Context, deviceID int64, ingestedAt time.Time,
	epoch observability.LogEpoch, rows []observability.LogEntry) error {
	if deviceID <= 0 || epoch.BootID == "" || epoch.PID <= 0 {
		return errors.New("log ingest: device and producer epoch are required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.restore(ctx); err != nil {
		return err
	}

	cursor, err := l.cursor(ctx, deviceID)
	if err != nil {
		return err
	}
	advance := observability.AdvanceLogCursor(cursor, epoch, rows)
	if l.empty[deviceID] && len(advance.Entries) > 0 {
		advance.Gap = true
		advance.Reason = "continuity after an observed-empty log page cannot be proven"
	}
	gapAt := l.gaps[deviceID]
	if len(rows) == 0 {
		// A successful full-ring read becoming empty means the producer ring was
		// cleared. Even with the same boot/PID, rows may have existed and vanished
		// between polls. Start a new local generation so reused u32 IDs cannot
		// collide with durable events from before the clear.
		nextCursor := observability.LogCursor{Epoch: epoch, Generation: cursor.Generation}
		discontinuous := cursor.Valid || (l.empty[deviceID] && cursor.Epoch != epoch)
		if discontinuous {
			nextCursor.Generation++
			gapAt = ingestedAt.UnixMilli()
		}
		next := store.IngestCursor{
			DeviceID: deviceID, Source: openWRTLogSource,
			BootID: nextCursor.SourceBoot(), Cursor: emptyLogCursorPosition,
			UpdatedAt: ingestedAt.UnixMilli(), ContinuityGapAt: gapAt,
		}
		if _, err := l.store.AppendEventsAndCursor(ctx, nil, next); err != nil {
			return fmt.Errorf("log ingest: persist empty observation: %w", err)
		}
		if discontinuous {
			// Any missing AP log interval can hide a cross-device roam. Keeping
			// another AP's prior association would make the next row look like a
			// proven roam from stale fleet state.
			l.associations = observability.NewAssociationCorrelator()
		}
		nextCursor.Valid = false
		l.cursors[deviceID] = nextCursor
		l.empty[deviceID] = true
		l.gaps[deviceID] = gapAt
		l.loaded[deviceID] = true
		return nil
	}
	if !advance.Cursor.Valid || (len(advance.Entries) == 0 && advance.Cursor == cursor) {
		next := store.IngestCursor{
			DeviceID: deviceID, Source: openWRTLogSource,
			BootID: cursor.SourceBoot(), Cursor: cursor.Position(),
			UpdatedAt: ingestedAt.UnixMilli(), ContinuityGapAt: gapAt,
		}
		if _, err := l.store.AppendEventsAndCursor(ctx, nil, next); err != nil {
			return fmt.Errorf("log ingest: persist observed overlap: %w", err)
		}
		l.loaded[deviceID] = true
		return nil
	}
	if advance.Gap || advance.Reset {
		gapAt = ingestedAt.UnixMilli()
	}

	correlator := l.associations.Clone()
	if advance.Reset || advance.Gap || advance.ClockRegressed {
		correlator = observability.NewAssociationCorrelator()
	}
	events := make([]store.Event, 0, len(advance.Entries))
	for i, row := range advance.Entries {
		event := logStoreEvent(deviceID, ingestedAt.UnixMilli(), advance.Cursor.SourceBoot(), row, correlator)
		if i == 0 && (advance.Gap || advance.Reset || advance.ClockRegressed) {
			detail, _ := event.Detail.(map[string]any)
			detail["ingest_gap"] = advance.Gap
			detail["producer_reset"] = advance.Reset
			detail["producer_clock_regressed"] = advance.ClockRegressed
			detail["association_correlation_reset"] = true
			detail["continuity_reason"] = advance.Reason
			event.Detail = detail
		}
		events = append(events, event)
	}

	next := store.IngestCursor{
		DeviceID:        deviceID,
		Source:          openWRTLogSource,
		BootID:          advance.Cursor.SourceBoot(),
		Cursor:          advance.Cursor.Position(),
		UpdatedAt:       ingestedAt.UnixMilli(),
		ContinuityGapAt: gapAt,
	}
	if _, err := l.store.AppendEventsAndCursor(ctx, events, next); err != nil {
		return fmt.Errorf("log ingest: persist page: %w", err)
	}
	l.cursors[deviceID] = advance.Cursor
	delete(l.empty, deviceID)
	l.gaps[deviceID] = gapAt
	l.loaded[deviceID] = true
	l.associations = correlator
	return nil
}

func (l *logIngestor) restore(ctx context.Context) error {
	if l.restored {
		return nil
	}
	events, err := l.store.LatestClientAssociationEvents(ctx)
	if err != nil {
		return fmt.Errorf("log ingest: restore associations: %w", err)
	}
	for _, event := range events {
		if event.ClientMAC == "" || event.DeviceID == nil ||
			(event.Action != string(observability.WirelessConnect) &&
				event.Action != string(observability.WirelessRoam)) {
			continue
		}
		l.associations.Restore(event.ClientMAC, observability.Association{
			DeviceID: *event.DeviceID, Iface: event.InIface,
			AtMS: associationSourceTimeMS(event), Connected: true,
		})
	}
	l.restored = true
	return nil
}

func (l *logIngestor) cursor(ctx context.Context, deviceID int64) (observability.LogCursor, error) {
	if l.loaded[deviceID] {
		return l.cursors[deviceID], nil
	}
	stored, err := l.store.LoadIngestCursor(ctx, deviceID, openWRTLogSource)
	if errors.Is(err, store.ErrNotFound) {
		l.loaded[deviceID] = true
		return observability.LogCursor{}, nil
	}
	if err != nil {
		return observability.LogCursor{}, fmt.Errorf("log ingest: load cursor: %w", err)
	}
	if stored.Cursor == emptyLogCursorPosition {
		cursor, err := observability.ParseLogCursor(stored.BootID, "0:1")
		if err != nil {
			return observability.LogCursor{}, fmt.Errorf("log ingest: stored empty cursor: %w", err)
		}
		cursor.LastID, cursor.LastTimeMS, cursor.Valid = 0, 0, false
		l.cursors[deviceID] = cursor
		l.empty[deviceID] = true
		l.gaps[deviceID] = stored.ContinuityGapAt
		l.loaded[deviceID] = true
		return cursor, nil
	}
	cursor, err := observability.ParseLogCursor(stored.BootID, stored.Cursor)
	if err != nil {
		return observability.LogCursor{}, fmt.Errorf("log ingest: stored cursor: %w", err)
	}
	l.cursors[deviceID] = cursor
	l.gaps[deviceID] = stored.ContinuityGapAt
	l.loaded[deviceID] = true
	return cursor, nil
}

func logStoreEvent(deviceID, ingestedAt int64, sourceBoot string,
	row observability.LogEntry, correlator *observability.AssociationCorrelator) store.Event {
	detail := map[string]any{
		"message":        observability.SanitizeLogMessage(row.Message),
		"facility":       row.Facility(),
		"priority":       row.Priority,
		"source_time_ms": row.TimeMS,
	}
	event := store.Event{
		TS:         int64(row.TimeMS / 1000),
		DeviceID:   &deviceID,
		Category:   "system",
		Severity:   logSeverity(row.Severity()),
		Event:      "openwrt.log",
		Detail:     detail,
		Source:     openWRTLogSource,
		SourceID:   row.SourceID(),
		SourceBoot: sourceBoot,
		IngestedAt: ingestedAt,
	}
	wireless, ok := observability.ParseWirelessLog(row.Message)
	if !ok {
		return event
	}
	event.Category = "client"
	event.ClientMAC = wireless.MAC
	event.InIface = wireless.Iface
	event.Action = string(wireless.Action)
	event.Event = "client." + string(wireless.Action)
	transition, emitted := correlator.Observe(deviceID, row.TimeMS, wireless)
	if !emitted {
		if transition.IgnoredReason != "" {
			event.Action = ""
			event.Event = "openwrt.log"
			detail["association_correlation"] = "ignored"
			detail["association_correlation_reason"] = transition.IgnoredReason
		}
		return event
	}
	event.Action = string(transition.Action)
	event.Event = "client." + string(transition.Action)
	if transition.From != nil {
		detail["from_device_id"] = transition.From.DeviceID
		detail["from_iface"] = transition.From.Iface
	}
	if transition.To != nil {
		detail["to_device_id"] = transition.To.DeviceID
		detail["to_iface"] = transition.To.Iface
		detail["fast_transition"] = transition.To.FastTransition
	}
	return event
}

func associationSourceTimeMS(event store.Event) uint64 {
	fallback := uint64(event.TS) * 1000
	blob, err := json.Marshal(event.Detail)
	if err != nil {
		return fallback
	}
	var detail struct {
		SourceTimeMS uint64 `json:"source_time_ms"`
	}
	if err := json.Unmarshal(blob, &detail); err != nil || detail.SourceTimeMS == 0 {
		return fallback
	}
	return detail.SourceTimeMS
}

func logSeverity(priority uint32) string {
	switch {
	case priority <= 3:
		return "error"
	case priority == 4:
		return "warning"
	default:
		return "info"
	}
}
