package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type IPv6RAConditionState string

const (
	IPv6RAConditionRecent     IPv6RAConditionState = "recent"
	IPv6RAConditionHistorical IPv6RAConditionState = "historical"
	IPv6RAConditionUnknown    IPv6RAConditionState = "unknown"
)

// IPv6RAConditionWindows keeps evidence classification policy outside the
// store. RecentFor and QuietFor may differ so callers can leave an explicit
// uncertainty interval instead of changing state at one arbitrary instant.
type IPv6RAConditionWindows struct {
	Now            time.Time
	CursorFreshFor time.Duration
	RecentFor      time.Duration
	QuietFor       time.Duration
}

// IPv6RAConditionStatus is the newest retained condition evidence for one
// device. LastObservedAt is controller receive time in Unix milliseconds, so a
// router clock error cannot make recent evidence look current or historical.
type IPv6RAConditionStatus struct {
	EventID        int64                `json:"event_id"`
	DeviceID       int64                `json:"device_id"`
	State          IPv6RAConditionState `json:"state"`
	Occurrences    int64                `json:"occurrences"`
	LastObservedAt int64                `json:"last_observed_at"`
}

// IPv6RAConditionStatuses classifies condition evidence independently of an
// event page or its filters. It does not resolve or delete retained history.
func (db *DB) IPv6RAConditionStatuses(ctx context.Context,
	windows IPv6RAConditionWindows) ([]IPv6RAConditionStatus, error) {
	if windows.Now.IsZero() || windows.CursorFreshFor <= 0 || windows.RecentFor <= 0 ||
		windows.QuietFor <= 0 || windows.RecentFor > windows.QuietFor {
		return nil, errors.New("store: IPv6 RA condition windows require now and positive durations with recent no longer than quiet")
	}
	now := windows.Now.UnixMilli()
	rows, err := db.sql.QueryContext(ctx, `
WITH ranked AS (
  SELECT id, device_id, source_boot,
         COALESCE(ingested_at, ts * 1000) AS last_observed_at,
         detail_json,
         ROW_NUMBER() OVER (
           PARTITION BY device_id
           ORDER BY COALESCE(ingested_at, ts * 1000) DESC, id DESC
         ) AS rank
    FROM events
   WHERE device_id IS NOT NULL
     AND source='openwrt-logd'
     AND severity='warning'
     AND event=?
     AND source_id=?
)
SELECT ranked.id, ranked.device_id, ranked.source_boot,
       ranked.last_observed_at, ranked.detail_json,
       cursor.boot_id, cursor.updated_at, cursor.continuity_gap_at
  FROM ranked
  LEFT JOIN ingest_cursors AS cursor
    ON cursor.device_id=ranked.device_id AND cursor.source='openwrt-logd'
 WHERE ranked.rank=1
 ORDER BY ranked.device_id`,
		EventOpenWRTIPv6RANoDefaultRoute, EventOpenWRTIPv6RANoDefaultRouteSourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := []IPv6RAConditionStatus{}
	for rows.Next() {
		var status IPv6RAConditionStatus
		var sourceBoot, detail string
		var cursorBoot sql.NullString
		var cursorUpdatedAt, continuityGapAt sql.NullInt64
		if err := rows.Scan(&status.EventID, &status.DeviceID, &sourceBoot,
			&status.LastObservedAt, &detail, &cursorBoot, &cursorUpdatedAt,
			&continuityGapAt); err != nil {
			return nil, err
		}
		decoded, err := decodeIPv6RAConditionDetail([]byte(detail))
		if err != nil {
			return nil, err
		}
		status.Occurrences = decoded.occurrences
		status.State = classifyIPv6RACondition(status.LastObservedAt, sourceBoot,
			cursorBoot, cursorUpdatedAt, continuityGapAt, now, windows)
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func classifyIPv6RACondition(lastObservedAt int64, sourceBoot string,
	cursorBoot sql.NullString, cursorUpdatedAt, continuityGapAt sql.NullInt64,
	now int64, windows IPv6RAConditionWindows) IPv6RAConditionState {
	if !cursorBoot.Valid || !cursorUpdatedAt.Valid || cursorBoot.String == "" ||
		cursorUpdatedAt.Int64 > now || lastObservedAt > now ||
		now-cursorUpdatedAt.Int64 > windows.CursorFreshFor.Milliseconds() ||
		(continuityGapAt.Valid && continuityGapAt.Int64 > now) {
		return IPv6RAConditionUnknown
	}
	sameBoot := cursorBoot.String == sourceBoot
	if sameBoot && now-lastObservedAt <= windows.RecentFor.Milliseconds() {
		return IPv6RAConditionRecent
	}
	if !sameBoot && (!continuityGapAt.Valid || continuityGapAt.Int64 <= 0) {
		return IPv6RAConditionUnknown
	}
	quietSince := lastObservedAt
	if continuityGapAt.Valid && continuityGapAt.Int64 > quietSince {
		quietSince = continuityGapAt.Int64
	}
	if now-quietSince >= windows.QuietFor.Milliseconds() {
		return IPv6RAConditionHistorical
	}
	return IPv6RAConditionUnknown
}
