package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// seedDevices creates the device rows the series table's foreign key requires.
// Production never hits this: the collector only polls devices it read out of
// the inventory, so every device_id it reports already exists.
func seedDevices(t *testing.T, db *DB, ids ...int64) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		if _, err := db.sql.ExecContext(ctx,
			`INSERT INTO devices (id, mac, host, name) VALUES (?,?,?,?)`,
			id, fmt.Sprintf("aa:bb:cc:00:00:%02d", id), "192.0.2.1",
			fmt.Sprintf("dev%d", id)); err != nil {
			t.Fatalf("seed device %d: %v", id, err)
		}
	}
}

func rows(deviceID int64, kind, key string, base int64, vals ...[4]float64) []RollupRow {
	out := make([]RollupRow, 0, len(vals))
	for i, v := range vals {
		out = append(out, RollupRow{
			DeviceID: deviceID, Kind: kind, Key: key,
			TS:  base + int64(i)*300,
			Avg: v[0], Min: v[1], Max: v[2], Cnt: int(v[3]),
		})
	}
	return out
}

func TestWriteRollupsCreatesSeriesOnce(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)

	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC).Unix()
	in := rows(1, "iface_rx_bps", "eth0", base,
		[4]float64{100, 50, 150, 12},
		[4]float64{200, 100, 300, 12})
	if err := db.WriteRollups(ctx, in); err != nil {
		t.Fatalf("WriteRollups: %v", err)
	}
	// A second flush of the same series must reuse its row rather than fail on
	// the unique constraint or create a duplicate.
	if err := db.WriteRollups(ctx, rows(1, "iface_rx_bps", "eth0", base+600,
		[4]float64{300, 200, 400, 12})); err != nil {
		t.Fatalf("second WriteRollups: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM series`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("series rows = %d, want 1", n)
	}

	got, err := db.QuerySeries(ctx, 1, "iface_rx_bps", "eth0",
		time.Unix(base, 0), time.Unix(base+3600, 0))
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(got.Points) != 3 {
		t.Fatalf("got %d points, want 3", len(got.Points))
	}
	if got.Points[0].Avg != 100 || got.Points[2].Avg != 300 {
		t.Errorf("points out of order or wrong: %+v", got.Points)
	}
}

// A flush retried after a crash must not double-count a window.
func TestWriteRollupsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC).Unix()
	in := rows(1, "sys_load1", "", base, [4]float64{1, 1, 1, 5})

	for range 3 {
		if err := db.WriteRollups(ctx, in); err != nil {
			t.Fatalf("WriteRollups: %v", err)
		}
	}
	got, err := db.QuerySeries(ctx, 1, "sys_load1", "",
		time.Unix(base, 0), time.Unix(base+3600, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 1 || got.Points[0].Cnt != 5 {
		t.Fatalf("replaying a flush changed the data: %+v", got.Points)
	}
}

// The hourly average must be weighted by sample count. A device polled at the
// focused rate for part of an hour contributes twelve times as many readings as
// one at baseline, and a mean of means would let the sparse window dominate.
func TestFoldHourlyWeightsByCount(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	hour := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)
	base := hour.Unix()

	in := []RollupRow{
		{DeviceID: 1, Kind: "sys_load1", TS: base, Avg: 1, Min: 0.5, Max: 2, Cnt: 60},
		{DeviceID: 1, Kind: "sys_load1", TS: base + 300, Avg: 10, Min: 9, Max: 11, Cnt: 5},
	}
	if err := db.WriteRollups(ctx, in); err != nil {
		t.Fatalf("WriteRollups: %v", err)
	}
	if err := db.FoldHourly(ctx, hour.Add(time.Hour)); err != nil {
		t.Fatalf("FoldHourly: %v", err)
	}

	var avg, mn, mx float64
	var cnt int
	err := db.SQL().QueryRowContext(ctx,
		`SELECT avg, min, max, cnt FROM rollup_1h WHERE ts=?`, base).Scan(&avg, &mn, &mx, &cnt)
	if err != nil {
		t.Fatalf("read hourly rollup: %v", err)
	}
	// (1*60 + 10*5) / 65 = 1.6923..., not the unweighted 5.5.
	want := (1.0*60 + 10.0*5) / 65
	if math.Abs(avg-want) > 1e-9 {
		t.Errorf("hourly avg = %v, want %v (weighted, not a mean of means)", avg, want)
	}
	if mn != 0.5 || mx != 11 || cnt != 65 {
		t.Errorf("got min=%v max=%v cnt=%d, want 0.5/11/65", mn, mx, cnt)
	}
}

// Folding must not consume the hour that is still accumulating.
func TestFoldHourlyLeavesTheCurrentHourAlone(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	hour := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)

	in := []RollupRow{
		{DeviceID: 1, Kind: "sys_load1", TS: hour.Add(-time.Hour).Unix(), Avg: 1, Cnt: 12},
		{DeviceID: 1, Kind: "sys_load1", TS: hour.Unix(), Avg: 2, Cnt: 12},
	}
	if err := db.WriteRollups(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := db.FoldHourly(ctx, hour.Add(30*time.Minute)); err != nil {
		t.Fatalf("FoldHourly: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM rollup_1h`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hourly rows = %d, want 1 (the in-progress hour must not fold)", n)
	}
}

// Folding runs every five minutes forever, so it must be idempotent and must
// not re-aggregate the whole retained history each time.
func TestFoldHourlyRepeats(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	hour := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)

	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "sys_load1", TS: hour.Unix(), Avg: 3, Min: 3, Max: 3, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := db.FoldHourly(ctx, hour.Add(time.Hour)); err != nil {
			t.Fatalf("FoldHourly: %v", err)
		}
	}
	var avg float64
	var cnt, n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollup_1h`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hourly rows = %d after three folds, want 1", n)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT avg, cnt FROM rollup_1h WHERE ts=?`, hour.Unix()).Scan(&avg, &cnt); err != nil {
		t.Fatal(err)
	}
	if avg != 3 || cnt != 12 {
		t.Errorf("re-folding changed the value: avg=%v cnt=%d", avg, cnt)
	}

	// A later hour still folds, so the watermark advances rather than sticking.
	next := hour.Add(time.Hour)
	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "sys_load1", TS: next.Unix(), Avg: 4, Min: 4, Max: 4, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.FoldHourly(ctx, next.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollup_1h`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("hourly rows = %d, want 2 — the fold watermark did not advance", n)
	}
}

func TestPruneEnforcesRetention(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	old5m := now.Add(-20 * 24 * time.Hour)
	new5m := now.Add(-1 * time.Hour)
	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "sys_load1", TS: old5m.Unix(), Avg: 1, Cnt: 1},
		{DeviceID: 1, Kind: "sys_load1", TS: new5m.Unix(), Avg: 2, Cnt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Fold first, or pruning loses the old window outright instead of
	// downsampling it.
	if err := db.FoldHourly(ctx, now); err != nil {
		t.Fatal(err)
	}
	res, err := db.Prune(ctx, now, DefaultRetention())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.FiveMinute != 1 {
		t.Errorf("pruned %d 5m rows, want 1", res.FiveMinute)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM rollup_5m`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("5m rows left = %d, want 1", n)
	}
	// The pruned window survives at hourly resolution — downsampled, not lost.
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollup_1h WHERE ts=?`,
		old5m.Truncate(time.Hour).Unix()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the pruned 5-minute window was not preserved hourly")
	}
}

func TestPruneCapsEventsByRowCount(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	for i := range 25 {
		if err := db.LogEvent(ctx, Event{
			TS: int64(1000 + i), Category: "system", Severity: "info", Event: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := db.Prune(ctx, time.Now(), Retention{MaxEvents: 10})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Events != 15 {
		t.Errorf("pruned %d events, want 15", res.Events)
	}
	got, err := db.RecentEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("events left = %d, want 10", len(got))
	}
	// The newest are kept, not the oldest.
	if got[0].TS != 1024 {
		t.Errorf("newest event ts = %d, want 1024", got[0].TS)
	}
}

// A series whose every sample has been pruned is dead weight, and its key is a
// client MAC — the one identifier guaranteed to churn.
func TestPruneRemovesEmptySeries(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	old := now.Add(-500 * 24 * time.Hour)

	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "sta_rssi", Key: "aa:bb", TS: old.Unix(), Avg: -50, Cnt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.FoldHourly(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Prune(ctx, now, DefaultRetention()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM series`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("series rows = %d after everything was pruned, want 0", n)
	}
}

// A caller asking for 5-minute points across a year wants a year of data, not
// 105,000 points it will discard — and beyond 14 days the 5-minute table cannot
// answer completely anyway.
func TestQuerySeriesChoosesResolutionFromTheRange(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	now := time.Now()

	for _, tc := range []struct {
		name string
		from time.Time
		to   time.Time
		want string
	}{
		{"last hour", now.Add(-time.Hour), now, "5m"},
		{"last day", now.Add(-24 * time.Hour), now, "5m"},
		{"last month", now.Add(-30 * 24 * time.Hour), now, "1h"},
		{"a long window inside retention", now.Add(-10 * 24 * time.Hour), now, "1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.QuerySeries(ctx, 1, "sys_load1", "", tc.from, tc.to)
			if err != nil {
				t.Fatalf("QuerySeries: %v", err)
			}
			if got.Res != tc.want {
				t.Errorf("resolution = %q, want %q", got.Res, tc.want)
			}
			if got.Points == nil {
				t.Error("Points is nil; an empty series must marshal as [] not null")
			}
		})
	}
}

func TestSeriesKeys(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1, 2)
	base := time.Now().Unix()
	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "iface_rx_bps", Key: "wan", TS: base, Cnt: 1},
		{DeviceID: 1, Kind: "iface_rx_bps", Key: "lan1", TS: base, Cnt: 1},
		{DeviceID: 1, Kind: "sys_load1", Key: "", TS: base, Cnt: 1},
		{DeviceID: 2, Kind: "iface_rx_bps", Key: "eth0", TS: base, Cnt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.SeriesKeys(ctx, 1, "iface_rx_bps")
	if err != nil {
		t.Fatalf("SeriesKeys: %v", err)
	}
	if len(got) != 2 || got[0] != "lan1" || got[1] != "wan" {
		t.Fatalf("got %v, want [lan1 wan] — this device's interfaces, sorted", got)
	}
}

// Un-adopting a device must not leave its telemetry behind. The device cascade
// removes the series; the rollup tables carry no foreign key of their own, so
// maintenance has to collect what is left.
func TestPruneCollectsRollupsOfDeletedDevices(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1, 2)
	base := time.Now().Truncate(time.Hour).Unix()

	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "sys_load1", TS: base, Avg: 1, Cnt: 1},
		{DeviceID: 2, Kind: "sys_load1", TS: base, Avg: 2, Cnt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=1`); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	// The cascade took the series row with it...
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM series`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("series rows = %d after deleting one device, want 1", n)
	}
	// ...but the rollup row is orphaned until the sweep runs. The sweep is NOT
	// on the maintenance tick: it is a full scan of both rollup tables, and the
	// process has one database connection, so doing it every five minutes to
	// find nothing would stall every read and write for no benefit.
	if _, err := db.Prune(ctx, time.Now(), DefaultRetention()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var still int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM rollup_5m`).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 2 {
		t.Fatalf("the maintenance tick swept orphans: %d rows left, want 2", still)
	}
	if err := db.SweepOrphans(ctx); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM rollup_5m`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rollup_5m rows = %d, want 1 — the deleted device's telemetry survived", n)
	}
	// The surviving device kept its data.
	got, err := db.QuerySeries(ctx, 2, "sys_load1", "",
		time.Unix(base-60, 0), time.Unix(base+3600, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 1 {
		t.Errorf("the remaining device lost its telemetry: %+v", got.Points)
	}
}

// The fold's floor only advances when new 5-minute data arrives, so a
// fleet-wide gap pins it. Once retention starts eating that hour's 5-minute
// rows, an unguarded re-fold would overwrite the correct hourly aggregate with
// a partial one — and by then the hourly row is the only copy left.
func TestFoldHourlyNeverShrinksAnHour(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)

	hour := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	full := make([]RollupRow, 0, 12)
	for i := range 12 {
		full = append(full, RollupRow{
			DeviceID: 1, Kind: "sys_load1", TS: hour.Unix() + int64(i)*300,
			Avg: float64(i), Min: float64(i), Max: float64(i), Cnt: 1,
		})
	}
	if err := db.WriteRollups(ctx, full); err != nil {
		t.Fatal(err)
	}
	if err := db.FoldHourly(ctx, hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var avg float64
	var cnt int
	read := func() {
		t.Helper()
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT avg, cnt FROM rollup_1h WHERE ts=?`, hour.Unix()).Scan(&avg, &cnt); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if cnt != 12 || avg != 5.5 {
		t.Fatalf("initial fold: avg=%v cnt=%d, want 5.5/12", avg, cnt)
	}

	// Simulate retention having eaten the first seven windows, then re-fold —
	// which is exactly what a pinned watermark does on every subsequent tick.
	if _, err := db.SQL().ExecContext(ctx,
		`DELETE FROM rollup_5m WHERE ts < ?`, hour.Unix()+7*300); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := db.FoldHourly(ctx, hour.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if cnt != 12 || avg != 5.5 {
		t.Fatalf("re-folding from a partial hour rewrote it: avg=%v cnt=%d, "+
			"want the original 5.5/12", avg, cnt)
	}
}

// An unaligned 5-minute cutoff cuts through an hour, leaving a tail that any
// later re-fold would mistake for the whole hour.
func TestPruneAlignsTheFiveMinuteCutoffToAnHour(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)

	now := time.Date(2026, 8, 13, 12, 35, 0, 0, time.UTC)
	// Exactly on the retention edge: without alignment this hour would be cut
	// in half.
	edge := now.Add(-14 * 24 * time.Hour)
	hourStart := edge.Truncate(time.Hour)
	rows := []RollupRow{}
	for i := range 12 {
		rows = append(rows, RollupRow{
			DeviceID: 1, Kind: "sys_load1", TS: hourStart.Unix() + int64(i)*300, Avg: 1, Cnt: 1,
		})
	}
	if err := db.WriteRollups(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Prune(ctx, now, DefaultRetention()); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var kept, inHour int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM rollup_5m`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollup_5m WHERE ts >= ? AND ts < ?`,
		hourStart.Unix(), hourStart.Add(time.Hour).Unix()).Scan(&inHour); err != nil {
		t.Fatal(err)
	}
	// The hour is either wholly present or wholly gone — never partial.
	if inHour != 0 && inHour != 12 {
		t.Fatalf("the boundary hour was cut in half: %d of 12 windows survive", inHour)
	}
	_ = kept
}
