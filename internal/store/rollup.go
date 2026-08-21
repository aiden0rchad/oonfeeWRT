package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Retention is how long each resolution is kept. Defaults are decision D4's:
// 5-minute rollups for 14 days, hourly for 13 months, controller/audit events
// capped by row count, OpenWrt logd events kept for 24 hours, closed topology
// intervals kept for the API's 31-day history window, and explicit RF scans
// capped per stable radio identity.
type Retention struct {
	FiveMinute      time.Duration
	Hourly          time.Duration
	OpenWRTLogs     time.Duration
	TopologyHistory time.Duration
	// MaxRadioScansPerRadio caps terminal rows only; active work is preserved.
	MaxRadioScansPerRadio     int
	MaxOpenWRTEvents          int
	MaxOpenWRTEventsPerDevice int
	MaxEvents                 int
}

// DefaultRetention returns the shipped policy.
func DefaultRetention() Retention {
	return Retention{
		FiveMinute:                14 * 24 * time.Hour,
		Hourly:                    396 * 24 * time.Hour, // 13 months
		OpenWRTLogs:               24 * time.Hour,
		TopologyHistory:           31 * 24 * time.Hour,
		MaxRadioScansPerRadio:     1,
		MaxOpenWRTEvents:          100_000,
		MaxOpenWRTEventsPerDevice: 50_000,
		MaxEvents:                 100_000,
	}
}

// RollupRow is one aggregated window on its way into the database.
type RollupRow struct {
	DeviceID int64
	Kind     string
	Key      string
	TS       int64
	Avg      float64
	Min      float64
	Max      float64
	Cnt      int
}

// WriteRollups persists a flush in ONE transaction.
//
// One transaction per flush is decision D4's write shape and the reason this
// function takes a slice rather than a row. It survived the move from a router
// to a container because it is right regardless of the disk: it bounds SQLite's
// work per tick, keeps the WAL small, and makes the whole flush either land or
// not.
//
// Rows are upserted rather than inserted. A flush that is retried after a crash
// must not double-count a window, and the aggregate for a completed window is
// idempotent by construction — the same samples produce the same average.
// Callers must never pass an in-progress window: aggregates carry no durable
// fragment identity, so a later process could only replace or double-count it.
func (db *DB) WriteRollups(ctx context.Context, rows []RollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin rollup write: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	findSeries, err := tx.PrepareContext(ctx,
		`SELECT id FROM series WHERE device_id=? AND kind=? AND key=?`)
	if err != nil {
		return err
	}
	defer findSeries.Close()
	addSeries, err := tx.PrepareContext(ctx,
		`INSERT INTO series (device_id, kind, key)
		 SELECT ?,?,? WHERE EXISTS (SELECT 1 FROM devices WHERE id=?)`)
	if err != nil {
		return err
	}
	defer addSeries.Close()
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO rollup_5m (series_id, ts, avg, min, max, cnt) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(series_id, ts) DO UPDATE SET
		   avg=excluded.avg, min=excluded.min, max=excluded.max, cnt=excluded.cnt`)
	if err != nil {
		return err
	}
	defer insert.Close()

	// Series IDs are cached for the transaction. A focused poll of a busy AP
	// produces hundreds of rows across a handful of series, and looking each one
	// up per row is the difference between one query and hundreds.
	ids := map[RollupRow]int64{}
	deletedDevices := map[int64]bool{}
	for _, r := range rows {
		if deletedDevices[r.DeviceID] {
			continue
		}
		ck := RollupRow{DeviceID: r.DeviceID, Kind: r.Kind, Key: r.Key}
		id, ok := ids[ck]
		if !ok {
			var parentExists bool
			id, parentExists, err = seriesID(ctx, findSeries, addSeries,
				r.DeviceID, r.Kind, r.Key)
			if err != nil {
				return err
			}
			if !parentExists {
				// A completed poll can already be in the in-memory flush when
				// un-adoption deletes its device. Drop only that deleted
				// parent's rows; all other rows in this window still land.
				deletedDevices[r.DeviceID] = true
				continue
			}
			ids[ck] = id
		}
		if _, err := insert.ExecContext(ctx, id, r.TS, r.Avg, r.Min, r.Max, r.Cnt); err != nil {
			return fmt.Errorf("store: write rollup %s/%s: %w", r.Kind, r.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit rollups: %w", err)
	}
	return nil
}

func seriesID(ctx context.Context, find, add *sql.Stmt, deviceID int64,
	kind, key string) (int64, bool, error) {
	var id int64
	err := find.QueryRowContext(ctx, deviceID, kind, key).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("store: look up series %s/%s: %w", kind, key, err)
	}
	res, err := add.ExecContext(ctx, deviceID, kind, key, deviceID)
	if err != nil {
		return 0, false, fmt.Errorf("store: create series %s/%s: %w", kind, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("store: inspect created series %s/%s: %w", kind, key, err)
	}
	if n == 0 {
		return 0, false, nil
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("store: identify created series %s/%s: %w", kind, key, err)
	}
	return id, true, nil
}

// FoldHourly aggregates completed 5-minute rollups into hourly ones.
//
// The averaging is weighted by sample count, not a mean of means. Windows do not
// all carry the same number of samples — a device polled at the focused rate for
// part of an hour contributes twelve times as many readings as one at baseline —
// and an unweighted average would let a sparse window pull the hour around.
//
// before must be an hour boundary at or below the current hour; anything later
// would fold an hour that is still accumulating.
func (db *DB) FoldHourly(ctx context.Context, before time.Time) error {
	cutoff := before.Truncate(time.Hour).Unix()
	// Fold only from the last hour already folded. Without this floor every tick
	// re-aggregates the entire 14-day 5-minute table, which is millions of rows
	// to redo the same arithmetic.
	//
	// Re-folding the boundary hour is NOT unconditionally harmless, which an
	// earlier version of this comment claimed. The floor only advances when new
	// 5-minute data arrives, so a fleet-wide gap pins it — and once retention
	// pruning starts eating that hour's 5-minute rows, each tick re-folds it
	// from a shrinking suffix and overwrites the correct hourly aggregate with a
	// partial one. By then the hourly row is the only copy of that hour left.
	//
	// The `WHERE excluded.cnt >= rollup_1h.cnt` guard makes the upsert
	// monotonic: an hour can be completed or re-stated with at least as many
	// samples, never quietly reduced. Prune aligning its own cutoff to an hour
	// boundary is the other half.
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO rollup_1h (series_id, ts, avg, min, max, cnt)
SELECT series_id, ts - (ts % 3600) AS hour,
       SUM(avg * cnt) / SUM(cnt), MIN(min), MAX(max), SUM(cnt)
  FROM rollup_5m
 WHERE ts >= (SELECT COALESCE(MAX(ts), 0) FROM rollup_1h)
   AND ts < ? AND cnt > 0
 GROUP BY series_id, hour
ON CONFLICT(series_id, ts) DO UPDATE SET
  avg=excluded.avg, min=excluded.min, max=excluded.max, cnt=excluded.cnt
  WHERE excluded.cnt >= rollup_1h.cnt`, cutoff)
	if err != nil {
		return fmt.Errorf("store: fold hourly rollups: %w", err)
	}
	return nil
}

// PruneResult reports what maintenance removed, so the daemon can log something
// truthful rather than "maintenance ran".
type PruneResult struct {
	FiveMinute int64
	Hourly     int64
	Events     int64
	Topology   int64
	RadioScans int64
}

// Prune enforces retention. It runs after FoldHourly, never before: dropping a
// 5-minute row that has not been folded yet loses the data outright rather than
// downsampling it.
func (db *DB) Prune(ctx context.Context, now time.Time, r Retention) (PruneResult, error) {
	var out PruneResult
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("store: begin prune: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if r.FiveMinute > 0 {
		// Aligned DOWN to an hour boundary so an hour is always either wholly
		// present at 5-minute resolution or wholly gone. An unaligned cutoff
		// cuts through an hour, and anything that later re-folds that hour sees
		// only the surviving tail.
		cut := now.Add(-r.FiveMinute).Unix()
		cut -= ((cut % 3600) + 3600) % 3600
		res, err := tx.ExecContext(ctx, `DELETE FROM rollup_5m WHERE ts < ?`, cut)
		if err != nil {
			return out, fmt.Errorf("store: prune 5m rollups: %w", err)
		}
		out.FiveMinute, _ = res.RowsAffected()
	}
	if r.Hourly > 0 {
		res, err := tx.ExecContext(ctx, `DELETE FROM rollup_1h WHERE ts < ?`,
			now.Add(-r.Hourly).Unix())
		if err != nil {
			return out, fmt.Errorf("store: prune hourly rollups: %w", err)
		}
		out.Hourly, _ = res.RowsAffected()
	}
	if r.OpenWRTLogs > 0 {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM events WHERE source='openwrt-logd' AND COALESCE(ingested_at, ts * 1000) < ?`,
			now.Add(-r.OpenWRTLogs).UnixMilli())
		if err != nil {
			return out, fmt.Errorf("store: prune OpenWrt logd events: %w", err)
		}
		n, _ := res.RowsAffected()
		out.Events += n
	}
	if r.MaxOpenWRTEventsPerDevice > 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE ingest_cursors
   SET continuity_gap_at=MAX(continuity_gap_at, ?)
 WHERE source='openwrt-logd' AND device_id IN (
   SELECT device_id FROM (
     SELECT device_id,
            ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY id DESC) AS n
       FROM events
      WHERE source='openwrt-logd' AND device_id IS NOT NULL
   ) WHERE n > ?
)`, now.UnixMilli(), r.MaxOpenWRTEventsPerDevice); err != nil {
			return out, fmt.Errorf("store: mark per-device OpenWrt logd truncation: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE id IN (
  SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY id DESC) AS n
      FROM events WHERE source='openwrt-logd'
  ) WHERE n > ?
)`, r.MaxOpenWRTEventsPerDevice)
		if err != nil {
			return out, fmt.Errorf("store: prune per-device OpenWrt logd events: %w", err)
		}
		n, _ := res.RowsAffected()
		out.Events += n
	}
	if r.MaxOpenWRTEvents > 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE ingest_cursors
   SET continuity_gap_at=MAX(continuity_gap_at, ?)
 WHERE source='openwrt-logd' AND device_id IN (
   SELECT DISTINCT device_id FROM events
    WHERE source='openwrt-logd' AND device_id IS NOT NULL AND id NOT IN (
      SELECT id FROM events WHERE source='openwrt-logd' ORDER BY id DESC LIMIT ?
    )
)`, now.UnixMilli(), r.MaxOpenWRTEvents); err != nil {
			return out, fmt.Errorf("store: mark global OpenWrt logd truncation: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE source='openwrt-logd' AND id NOT IN (
  SELECT id FROM events WHERE source='openwrt-logd' ORDER BY id DESC LIMIT ?
)`, r.MaxOpenWRTEvents)
		if err != nil {
			return out, fmt.Errorf("store: prune global OpenWrt logd events: %w", err)
		}
		n, _ := res.RowsAffected()
		out.Events += n
	}
	if r.MaxEvents > 0 {
		// Controller/audit history remains row-count based. OpenWrt logd rows use
		// their independent time window above, so a noisy router cannot evict
		// controller outcomes or lose recent logs to this cap.
		res, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE source <> 'openwrt-logd' AND id NOT IN (
  SELECT id FROM events WHERE source <> 'openwrt-logd' ORDER BY id DESC LIMIT ?)`, r.MaxEvents)
		if err != nil {
			return out, fmt.Errorf("store: prune events: %w", err)
		}
		n, _ := res.RowsAffected()
		out.Events += n
	}
	if r.TopologyHistory > 0 {
		// Active edges are the current graph and have no expiry. Only intervals
		// whose exclusive end is outside the retained replay window are removed.
		res, err := tx.ExecContext(ctx,
			`DELETE FROM topology_edges WHERE valid_to IS NOT NULL AND valid_to <= ?`,
			now.Add(-r.TopologyHistory).UnixMilli())
		if err != nil {
			return out, fmt.Errorf("store: prune topology history: %w", err)
		}
		out.Topology, _ = res.RowsAffected()
	}
	if r.MaxRadioScansPerRadio > 0 {
		// The Radios API serves only the newest explicit result for a stable
		// radio key. Bound terminal history to that contract while leaving
		// pending/running device work untouched. Child BSS rows cascade.
		res, err := tx.ExecContext(ctx, `
DELETE FROM radio_scans WHERE id IN (
  SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (
      PARTITION BY device_id, radio_key ORDER BY started_at DESC, id DESC
    ) AS n
      FROM radio_scans WHERE status IN ('completed','failed')
  ) WHERE n > ?
)`, r.MaxRadioScansPerRadio)
		if err != nil {
			return out, fmt.Errorf("store: prune RF scans: %w", err)
		}
		out.RadioScans, _ = res.RowsAffected()
	}
	// Rollups whose series is gone, collected only when one actually can be.
	//
	// Deleting a device cascades to `series`, but the rollup tables carry no
	// foreign key of their own, so those rows would otherwise outlive the
	// device forever. The sweep that collects them is a full scan of the two
	// largest tables in the schema, and the process has exactly one database
	// connection (SetMaxOpenConns(1)) — so running it every five minutes when
	// nothing has been deleted blocks every API read and every poll write for
	// the length of two scans, forever, to find nothing.
	//
	// SweepOrphans is therefore called from the paths that remove a device.
	// (An earlier comment justified the missing foreign key by claiming an
	// index on series_id would cost a write per row; that was wrong — these are
	// WITHOUT ROWID tables whose primary key already leads with series_id.)

	// Series rows whose every sample has been pruned are dead weight, and their
	// keys are client MACs — the one identifier guaranteed to churn. This runs
	// after the orphan sweep above, not before, or it would delete the series
	// that sweep uses to decide what is an orphan.
	if _, err := tx.ExecContext(ctx, `
DELETE FROM series WHERE id NOT IN (SELECT series_id FROM rollup_5m)
                     AND id NOT IN (SELECT series_id FROM rollup_1h)`); err != nil {
		return out, fmt.Errorf("store: prune empty series: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("store: commit prune: %w", err)
	}
	return out, nil
}

// SweepOrphans deletes rollups whose series row is gone, and any series row
// left with no rollups at all.
//
// Called when a device is removed, not on the maintenance tick: it is a full
// scan of both rollup tables, and doing that every five minutes to find nothing
// stalls the single database connection for no benefit.
func (db *DB) SweepOrphans(ctx context.Context) error {
	for _, table := range []string{"rollup_5m", "rollup_1h"} {
		if _, err := db.sql.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE series_id NOT IN (SELECT id FROM series)`); err != nil {
			return fmt.Errorf("store: sweep orphaned %s rows: %w", table, err)
		}
	}
	if _, err := db.sql.ExecContext(ctx, `
DELETE FROM series WHERE id NOT IN (SELECT series_id FROM rollup_5m)
                     AND id NOT IN (SELECT series_id FROM rollup_1h)`); err != nil {
		return fmt.Errorf("store: sweep empty series: %w", err)
	}
	// Ownership claims whose device is gone. Un-adopt drops them itself now,
	// but this catches the rows left by every un-adopt before it did, and any
	// path that removes a device without going through it. Left alone they are
	// not merely stale: sqlite reuses a freed row id, so the next device adopted
	// would inherit them.
	// Rows keyed on a device_id that no longer exists. Both tables carry
	// ON DELETE CASCADE, so this is a backstop for rows written before the
	// constraint took effect — and for the window when the connection pragmas
	// were applied per-connection rather than in the DSN, which is exactly how
	// the lab database came to hold claims for devices 1 and 2.
	for _, table := range []string{"owned_sections", "foreign_ssid_notes"} {
		if _, err := db.sql.ExecContext(ctx,
			`DELETE FROM `+table+
				` WHERE device_id NOT IN (SELECT id FROM devices)`); err != nil {
			return fmt.Errorf("store: sweep orphaned %s: %w", table, err)
		}
	}
	return nil
}

// Point is one value in a queried series.
type Point struct {
	TS  int64   `json:"ts"`
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Cnt int     `json:"cnt"`
}

// Series is one queried series with its points.
type Series struct {
	DeviceID int64   `json:"device_id"`
	Kind     string  `json:"kind"`
	Key      string  `json:"key"`
	Res      string  `json:"resolution"` // "5m" or "1h"
	Points   []Point `json:"points"`
}

// QuerySeries reads one series over a time range.
//
// Resolution is chosen from the range rather than requested: a caller asking for
// 5-minute points over a year wants a year of data, not 105,000 points it will
// immediately throw away. The threshold is where 5-minute retention ends, so the
// answer is also the only one that can be complete.
func (db *DB) QuerySeries(ctx context.Context, deviceID int64, kind, key string,
	from, to time.Time) (*Series, error) {

	table, res := "rollup_5m", "5m"
	if time.Since(from) > 14*24*time.Hour || to.Sub(from) > 7*24*time.Hour {
		table, res = "rollup_1h", "1h"
	}
	out := &Series{DeviceID: deviceID, Kind: kind, Key: key, Res: res, Points: []Point{}}

	rows, err := db.sql.QueryContext(ctx, `
SELECT r.ts, r.avg, r.min, r.max, r.cnt
  FROM `+table+` r JOIN series s ON s.id = r.series_id
 WHERE s.device_id=? AND s.kind=? AND s.key=? AND r.ts >= ? AND r.ts < ?
 ORDER BY r.ts`, deviceID, kind, key, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: query series %s/%s: %w", kind, key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.TS, &p.Avg, &p.Min, &p.Max, &p.Cnt); err != nil {
			return nil, err
		}
		out.Points = append(out.Points, p)
	}
	return out, rows.Err()
}

// SeriesKeys lists the keys present for a device and kind — the interfaces that
// have throughput, the stations that have RSSI. It is what lets a screen offer
// a series picker without the caller guessing what exists.
func (db *DB) SeriesKeys(ctx context.Context, deviceID int64, kind string) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT key FROM series WHERE device_id=? AND kind=? ORDER BY key`,
		deviceID, kind)
	if err != nil {
		return nil, fmt.Errorf("store: list series keys for %s: %w", kind, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
