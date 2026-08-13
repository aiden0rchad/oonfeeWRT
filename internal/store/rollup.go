package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Retention is how long each resolution is kept. Defaults are decision D4's:
// 5-minute rollups for 14 days, hourly for 13 months, and the event log capped
// by row count rather than age so a quiet install keeps its history.
type Retention struct {
	FiveMinute time.Duration
	Hourly     time.Duration
	MaxEvents  int
}

// DefaultRetention returns the shipped policy.
func DefaultRetention() Retention {
	return Retention{
		FiveMinute: 14 * 24 * time.Hour,
		Hourly:     396 * 24 * time.Hour, // 13 months
		MaxEvents:  100_000,
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
// must not double-count a window, and the aggregate for a given window is
// idempotent by construction — the same samples produce the same average.
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
		`INSERT INTO series (device_id, kind, key) VALUES (?,?,?)`)
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
	for _, r := range rows {
		ck := RollupRow{DeviceID: r.DeviceID, Kind: r.Kind, Key: r.Key}
		id, ok := ids[ck]
		if !ok {
			id, err = seriesID(ctx, findSeries, addSeries, r.DeviceID, r.Kind, r.Key)
			if err != nil {
				return err
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

func seriesID(ctx context.Context, find, add *sql.Stmt, deviceID int64, kind, key string) (int64, error) {
	var id int64
	err := find.QueryRowContext(ctx, deviceID, kind, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("store: look up series %s/%s: %w", kind, key, err)
	}
	res, err := add.ExecContext(ctx, deviceID, kind, key)
	if err != nil {
		return 0, fmt.Errorf("store: create series %s/%s: %w", kind, key, err)
	}
	return res.LastInsertId()
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
	// to redo the same arithmetic. Re-folding the boundary hour is harmless
	// because the upsert below is idempotent: the same samples give the same
	// aggregate.
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO rollup_1h (series_id, ts, avg, min, max, cnt)
SELECT series_id, ts - (ts % 3600) AS hour,
       SUM(avg * cnt) / SUM(cnt), MIN(min), MAX(max), SUM(cnt)
  FROM rollup_5m
 WHERE ts >= (SELECT COALESCE(MAX(ts), 0) FROM rollup_1h)
   AND ts < ? AND cnt > 0
 GROUP BY series_id, hour
ON CONFLICT(series_id, ts) DO UPDATE SET
  avg=excluded.avg, min=excluded.min, max=excluded.max, cnt=excluded.cnt`, cutoff)
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
}

// Prune enforces retention. It runs after FoldHourly, never before: dropping a
// 5-minute row that has not been folded yet loses the data outright rather than
// downsampling it.
func (db *DB) Prune(ctx context.Context, now time.Time, r Retention) (PruneResult, error) {
	var out PruneResult
	if r.FiveMinute > 0 {
		res, err := db.sql.ExecContext(ctx, `DELETE FROM rollup_5m WHERE ts < ?`,
			now.Add(-r.FiveMinute).Unix())
		if err != nil {
			return out, fmt.Errorf("store: prune 5m rollups: %w", err)
		}
		out.FiveMinute, _ = res.RowsAffected()
	}
	if r.Hourly > 0 {
		res, err := db.sql.ExecContext(ctx, `DELETE FROM rollup_1h WHERE ts < ?`,
			now.Add(-r.Hourly).Unix())
		if err != nil {
			return out, fmt.Errorf("store: prune hourly rollups: %w", err)
		}
		out.Hourly, _ = res.RowsAffected()
	}
	if r.MaxEvents > 0 {
		// By row count, not by age: an install that sees little activity should
		// keep its history rather than lose it to a calendar.
		res, err := db.sql.ExecContext(ctx, `
DELETE FROM events WHERE id NOT IN (
  SELECT id FROM events ORDER BY id DESC LIMIT ?)`, r.MaxEvents)
		if err != nil {
			return out, fmt.Errorf("store: prune events: %w", err)
		}
		out.Events, _ = res.RowsAffected()
	}
	// Rollups whose series is gone. Deleting a device cascades to `series`, but
	// the rollup tables carry no foreign key of their own — that was deliberate,
	// since an index on series_id would cost a write on every row of the hottest
	// table in the schema. The consequence is that un-adopting a device would
	// otherwise leave its telemetry behind forever, so it is collected here
	// instead, on a tick that is already doing bulk deletes.
	for _, table := range []string{"rollup_5m", "rollup_1h"} {
		if _, err := db.sql.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE series_id NOT IN (SELECT id FROM series)`); err != nil {
			return out, fmt.Errorf("store: prune orphaned %s rows: %w", table, err)
		}
	}

	// Series rows whose every sample has been pruned are dead weight, and their
	// keys are client MACs — the one identifier guaranteed to churn. This runs
	// after the orphan sweep above, not before, or it would delete the series
	// that sweep uses to decide what is an orphan.
	if _, err := db.sql.ExecContext(ctx, `
DELETE FROM series WHERE id NOT IN (SELECT series_id FROM rollup_5m)
                     AND id NOT IN (SELECT series_id FROM rollup_1h)`); err != nil {
		return out, fmt.Errorf("store: prune empty series: %w", err)
	}
	return out, nil
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
