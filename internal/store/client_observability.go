package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	observabilityFiveMinuteMS int64 = 5 * 60 * 1000
	observabilityHourlyMS     int64 = 60 * 60 * 1000
)

// ObservabilityRollup is one durable aggregate used by the joined client
// timeline. Raw poll samples deliberately never enter SQLite.
type ObservabilityRollup struct {
	DeviceID int64
	Kind     string
	Key      string
	TS       int64 // Unix milliseconds, aligned to Resolution.
	Avg      float64
	Min      float64
	Max      float64
	Cnt      int
}

// ObservabilityRollupQuery selects durable rollups without exposing a general
// SQL surface. A nil Key accepts every key; a non-nil empty Key selects only
// device/site-wide series.
type ObservabilityRollupQuery struct {
	DeviceIDs []int64
	Kinds     []string
	Key       *string
	From      int64 // Unix milliseconds, inclusive.
	To        int64 // Unix milliseconds, exclusive.
}

// ClientIncidentWindow says one device was on a client's observed topology
// path for this half-open interval.
type ClientIncidentWindow struct {
	DeviceID int64
	From     int64
	To       int64
}

// QueryObservabilityRollups returns the one stored resolution that can serve
// the requested window. It never labels rollups as raw data.
func (db *DB) QueryObservabilityRollups(ctx context.Context, q ObservabilityRollupQuery) (
	rows []ObservabilityRollup, resolution string, bucketMS int64, err error,
) {
	if q.From <= 0 || q.To <= q.From {
		return nil, "", 0, errors.New("store: observability range requires 0 < from < to")
	}
	if len(q.Kinds) == 0 || len(q.Kinds) > 64 {
		return nil, "", 0, errors.New("store: observability query requires 1..64 kinds")
	}
	for _, kind := range q.Kinds {
		if strings.TrimSpace(kind) == "" || kind != strings.TrimSpace(kind) {
			return nil, "", 0, errors.New("store: observability kind must be a non-blank identifier")
		}
	}
	if len(q.DeviceIDs) > 512 {
		return nil, "", 0, errors.New("store: observability query cannot exceed 512 devices")
	}
	for _, id := range q.DeviceIDs {
		if id <= 0 {
			return nil, "", 0, errors.New("store: observability device id must be positive")
		}
	}

	table := "rollup_5m"
	resolution, bucketMS = "5m", observabilityFiveMinuteMS
	if q.To-q.From > 7*24*60*60*1000 {
		table, resolution, bucketMS = "rollup_1h", "1h", observabilityHourlyMS
	}
	// The response timeline begins at the first complete stored bucket start in
	// the requested interval and ends before the first bucket that would extend
	// past To. A partial bucket is never relabelled as wholly in-range.
	fromBucketMS, ok := ceilMultiple(q.From, bucketMS)
	if !ok {
		// The next aligned bucket begins beyond the largest representable Unix
		// millisecond, so this range cannot contain a complete stored bucket.
		return []ObservabilityRollup{}, resolution, bucketMS, nil
	}
	fromBucket := fromBucketMS / 1000
	toBucket := (q.To / bucketMS * bucketMS) / 1000

	query := `SELECT s.device_id, s.kind, s.key, r.ts, r.avg, r.min, r.max, r.cnt
 FROM ` + table + ` r JOIN series s ON s.id=r.series_id
 WHERE r.ts >= ? AND r.ts < ? AND s.kind IN (` + placeholders(len(q.Kinds)) + `)`
	args := make([]any, 0, 2+len(q.Kinds)+len(q.DeviceIDs)+1)
	args = append(args, fromBucket, toBucket)
	for _, kind := range q.Kinds {
		args = append(args, kind)
	}
	if len(q.DeviceIDs) > 0 {
		query += ` AND s.device_id IN (` + placeholders(len(q.DeviceIDs)) + `)`
		for _, id := range q.DeviceIDs {
			args = append(args, id)
		}
	}
	if q.Key != nil {
		query += ` AND s.key=?`
		args = append(args, *q.Key)
	}
	query += ` ORDER BY r.ts, s.device_id, s.kind, s.key`

	dbRows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: query observability rollups: %w", err)
	}
	defer dbRows.Close()
	rows = []ObservabilityRollup{}
	for dbRows.Next() {
		var row ObservabilityRollup
		var tsSeconds int64
		if err := dbRows.Scan(&row.DeviceID, &row.Kind, &row.Key, &tsSeconds,
			&row.Avg, &row.Min, &row.Max, &row.Cnt); err != nil {
			return nil, "", 0, err
		}
		row.TS = tsSeconds * 1000
		rows = append(rows, row)
	}
	return rows, resolution, bucketMS, dbRows.Err()
}

// ClientEventsBetween returns exact durable rows sourced for one client plus
// device/site incidents on that client's observed path, oldest first.
// truncated is explicit; silently dropping an incident would turn a bounded
// query into a false complete history.
func (db *DB) ClientEventsBetween(ctx context.Context, clientMAC string,
	pathWindows []ClientIncidentWindow, from, to int64, limit int) (events []Event, truncated bool, err error) {
	mac, err := canonicalMAC(strings.TrimSpace(clientMAC))
	if err != nil {
		return nil, false, fmt.Errorf("store: client observability MAC: %w", err)
	}
	if from <= 0 || to <= from {
		return nil, false, errors.New("store: client event range requires 0 < from < to")
	}
	if limit <= 0 || limit > 5000 {
		return nil, false, errors.New("store: client event limit must be within 1..5000")
	}
	if len(pathWindows) > 10_000 {
		return nil, false, errors.New("store: client event path cannot exceed 10000 intervals")
	}
	for _, window := range pathWindows {
		if window.DeviceID <= 0 || window.From <= 0 || window.To <= window.From ||
			window.From < from || window.To > to {
			return nil, false, errors.New("store: client event path interval is invalid")
		}
	}
	// Event.TS is the legacy whole-second event time. Compare its represented
	// instant (TS*1000) to the millisecond range; never imply sub-second data.
	fromSeconds := ceilDiv(from, 1000)
	toSeconds := ceilDiv(to, 1000)
	query := ""
	args := []any{}
	if len(pathWindows) > 0 {
		query = `WITH path(device_id,from_ms,to_ms) AS (VALUES `
		for i, window := range pathWindows {
			if i > 0 {
				query += ","
			}
			query += `(?,?,?)`
			args = append(args, window.DeviceID, window.From, window.To)
		}
		query += `) `
	}
	query += `SELECT ` + eventColumns + ` FROM events WHERE (client_mac=?`
	args = append(args, mac)
	if len(pathWindows) > 0 {
		query += ` OR ((client_mac IS NULL OR client_mac='') AND category='device'
 AND EXISTS (SELECT 1 FROM path
              WHERE path.device_id=events.device_id
                AND events.ts*1000>=path.from_ms AND events.ts*1000<path.to_ms))`
	}
	query += `) AND ts >= ? AND ts < ? ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, fromSeconds, toSeconds, limit+1)
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: query client observability events: %w", err)
	}
	events, err = scanEvents(rows)
	if err != nil {
		return nil, false, err
	}
	if len(events) > limit {
		events, truncated = events[:limit], true
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, truncated, nil
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func ceilDiv(value, divisor int64) int64 {
	quotient := value / divisor
	if value%divisor != 0 {
		quotient++
	}
	return quotient
}

func ceilMultiple(value, unit int64) (int64, bool) {
	remainder := value % unit
	if remainder == 0 {
		return value, true
	}
	delta := unit - remainder
	if value > math.MaxInt64-delta {
		return 0, false
	}
	return value + delta, true
}
