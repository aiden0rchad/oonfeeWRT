package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestQueryObservabilityRollupsFiltersAndNamesStoredResolution(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1, 2)
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).Unix()
	mac := "02:00:00:00:00:44"
	if err := db.WriteRollups(ctx, []RollupRow{
		{DeviceID: 1, Kind: "sta_rssi", Key: mac, TS: base, Avg: -60, Min: -62, Max: -58, Cnt: 4},
		{DeviceID: 1, Kind: "sta_retry_delta_pct", Key: mac, TS: base + 300, Avg: 5, Min: 2, Max: 8, Cnt: 4},
		{DeviceID: 1, Kind: "sta_rssi", Key: "02:00:00:00:00:99", TS: base + 300, Avg: -40, Min: -41, Max: -39, Cnt: 4},
		{DeviceID: 2, Kind: "sta_rssi", Key: mac, TS: base + 300, Avg: -52, Min: -54, Max: -50, Cnt: 4},
	}); err != nil {
		t.Fatal(err)
	}

	rows, resolution, bucketMS, err := db.QueryObservabilityRollups(ctx,
		ObservabilityRollupQuery{
			DeviceIDs: []int64{1}, Kinds: []string{"sta_rssi", "sta_retry_delta_pct"},
			Key: &mac, From: base*1000 + 1, To: (base + 600) * 1000,
		})
	if err != nil {
		t.Fatal(err)
	}
	if resolution != "5m" || bucketMS != 300_000 {
		t.Fatalf("resolution=%q bucket=%d", resolution, bucketMS)
	}
	// The range begins one millisecond into the first bucket, so that partial
	// bucket is not relabelled as a complete in-range observation.
	if len(rows) != 1 || rows[0].Kind != "sta_retry_delta_pct" ||
		rows[0].DeviceID != 1 || rows[0].TS != (base+300)*1000 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestQueryObservabilityRollupsExcludesAnIncompleteFinalBucket(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	base := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC).Unix()
	if err := db.WriteRollups(ctx, []RollupRow{{
		DeviceID: 1, Kind: "sta_rssi", Key: "02:00:00:00:00:44",
		TS: base, Avg: -60, Min: -62, Max: -58, Cnt: 4,
	}}); err != nil {
		t.Fatal(err)
	}
	rows, _, _, err := db.QueryObservabilityRollups(ctx, ObservabilityRollupQuery{
		DeviceIDs: []int64{1}, Kinds: []string{"sta_rssi"},
		From: base * 1000, To: base*1000 + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("partial final bucket was presented as in-range: %+v", rows)
	}
}

func TestObservabilityBucketBoundsDoNotOverflowNearMaxInt64(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	from := int64(math.MaxInt64 - 807)
	rows, resolution, bucketMS, err := db.QueryObservabilityRollups(ctx,
		ObservabilityRollupQuery{
			Kinds: []string{"sta_rssi"}, From: from, To: math.MaxInt64,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || resolution != "5m" || bucketMS != 300_000 {
		t.Fatalf("rows=%+v resolution=%q bucket=%d", rows, resolution, bucketMS)
	}
	if _, ok := ceilMultiple(from, bucketMS); ok {
		t.Fatal("reported a representable aligned bucket beyond MaxInt64")
	}
	if got, want := ceilDiv(math.MaxInt64, 1000), int64(math.MaxInt64/1000+1); got != want {
		t.Fatalf("ceilDiv(MaxInt64,1000)=%d want %d", got, want)
	}
}

func TestClientEventsBetweenPreservesSourcedFieldsOrderAndTruncation(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)
	mac := "02:00:00:00:00:44"
	for _, event := range []Event{
		{TS: 101, DeviceID: ptrInt64(1), Category: "client", Severity: "info",
			Event: "associated", ClientMAC: mac, Action: "connect", Source: "openwrt-log",
			SourceID: "7", SourceBoot: "boot:1", InIface: "phy0-ap0"},
		{TS: 102, DeviceID: ptrInt64(1), Category: "client", Severity: "info",
			Event: "roamed", ClientMAC: mac, Action: "roam", Source: "openwrt-log",
			SourceID: "8", SourceBoot: "boot:1", InIface: "phy1-ap0"},
	} {
		if _, err := db.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	got, truncated, err := db.ClientEventsBetween(ctx, "02-00-00-00-00-44", nil, 100_001, 103_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(got) != 1 || got[0].SourceID != "8" ||
		got[0].SourceBoot != "boot:1" || got[0].Action != "roam" ||
		got[0].InIface != "phy1-ap0" || got[0].ClientMAC != mac {
		t.Fatalf("events=%+v truncated=%v", got, truncated)
	}
}

func TestClientEventsBetweenKeepsNewestIncidentWhenTruncated(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	mac := "02:00:00:00:00:44"
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO events (ts, category, severity, event, detail_json, source, ingested_at, client_mac)
VALUES (?, 'client', 'info', ?, '{}', 'openwrt-logd', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2001; i++ {
		if _, err := stmt.ExecContext(ctx, int64(i), fmt.Sprintf("event-%d", i), int64(i)*1000, mac); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events, truncated, err := db.ClientEventsBetween(ctx, mac, nil, 1_000, 2_002_000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(events) != 2000 || events[0].Event != "event-2" || events[1999].Event != "event-2001" {
		t.Fatalf("truncated=%v count=%d first=%q last=%q", truncated, len(events),
			events[0].Event, events[len(events)-1].Event)
	}
}

func ptrInt64(v int64) *int64 { return &v }
