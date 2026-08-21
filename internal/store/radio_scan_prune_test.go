package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPruneCapsTerminalRadioScansPerRadio(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	seedDevices(t, db, 1)

	type scan struct {
		id     int64
		radio  string
		start  int64
		status string
	}
	scans := []scan{
		{1, "radio0", 100, "completed"},
		{2, "radio0", 200, "failed"},
		{3, "radio0", 300, "completed"},
		{4, "radio1", 100, "completed"},
		{5, "radio1", 200, "completed"},
		{6, "radio0", 400, "pending"},
		{7, "radio0", 500, "running"},
	}
	for _, scan := range scans {
		var finished any
		if scan.status == "completed" || scan.status == "failed" {
			finished = scan.start + 1
		}
		if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO radio_scans
 (id,device_id,radio_key,started_at,finished_at,status,detail_json)
VALUES (?,1,?,?,?,?, '{}')`, scan.id, scan.radio, scan.start, finished, scan.status); err != nil {
			t.Fatalf("insert scan %d: %v", scan.id, err)
		}
		if scan.status == "completed" {
			if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO radio_scan_bss (scan_id,bssid,ssid,mhz,channel)
VALUES (?,?,'test',2412,1)`, scan.id, fmt.Sprintf("02:00:00:00:00:%02x", scan.id)); err != nil {
				t.Fatalf("insert scan BSS %d: %v", scan.id, err)
			}
		}
	}

	result, err := db.Prune(ctx, time.UnixMilli(1_000), Retention{MaxRadioScansPerRadio: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.RadioScans != 1 {
		t.Fatalf("pruned radio scans=%d, want 1", result.RadioScans)
	}
	for _, id := range []int64{2, 3, 4, 5, 6, 7} {
		var n int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM radio_scans WHERE id=?`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("scan %d was not preserved: count=%d err=%v", id, n, err)
		}
	}
	var oldScan, oldBSS int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM radio_scans WHERE id=1`).Scan(&oldScan); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM radio_scan_bss WHERE scan_id=1`).Scan(&oldBSS); err != nil {
		t.Fatal(err)
	}
	if oldScan != 0 || oldBSS != 0 {
		t.Fatalf("old scan/BSS survived: scan=%d bss=%d", oldScan, oldBSS)
	}

	result, err = db.Prune(ctx, time.UnixMilli(1_000), Retention{MaxRadioScansPerRadio: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.RadioScans != 0 {
		t.Fatalf("idempotent prune removed %d additional scans", result.RadioScans)
	}
}

func TestDefaultRetentionKeepsOnlyNewestTerminalRadioScan(t *testing.T) {
	if got := DefaultRetention().MaxRadioScansPerRadio; got != 1 {
		t.Fatalf("default terminal RF scans per radio=%d, want 1", got)
	}
}
