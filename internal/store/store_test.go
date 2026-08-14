package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, decision D3 (CGO_ENABLED=0)
)

const driver = "sqlite"

func open(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	db, err := Open(context.Background(), driver, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAppliesSchemaAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")

	db, err := Open(ctx, driver, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var mode string
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal — the whole write budget assumes it", mode)
	}
	db.Close()

	// Re-opening an existing database must migrate cleanly, not double-apply.
	db2, err := Open(ctx, driver, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	var versions int
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version`).Scan(&versions); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if versions != 1 {
		t.Errorf("schema_version should have exactly one row, got %d", versions)
	}
}

func TestUpsertDeviceKeysOnMAC(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	d := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.1.1", Name: "gw"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if d.ID == 0 {
		t.Fatal("insert should populate the ID")
	}
	if d.Adopted() {
		t.Error("a device with no adopted_at is pending, not adopted")
	}

	// A device's address can change; its MAC is the identity, so this must
	// update rather than create a second row.
	now := time.Now().Unix()
	d2 := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.9.9", Name: "gw",
		AdoptedAt: &now, Class: "A", CertFP: "deadbeef"}
	if err := db.UpsertDevice(ctx, d2); err != nil {
		t.Fatalf("update: %v", err)
	}
	all, err := db.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("MAC is the identity; want 1 device, got %d", len(all))
	}
	got := all[0]
	if got.Host != "192.168.9.9" || got.Class != "A" || got.CertFP != "deadbeef" {
		t.Errorf("update did not take: %+v", got)
	}
	if !got.Adopted() {
		t.Error("adopted_at was set, so Adopted() should be true")
	}
	if got.ID != d.ID {
		t.Errorf("row identity changed on update: %d -> %d", d.ID, got.ID)
	}
}

func TestDeviceByMACReportsNotFound(t *testing.T) {
	db := open(t)
	if _, err := db.DeviceByMAC(context.Background(), "no:su:ch:de:vi:ce"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// owned_sections is how the reconciler tells our config from a human's, so it
// has to survive re-application without duplicating.
func TestOwnedSectionsUpsertAndForget(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:00:11:22", Host: "10.0.0.1", Name: "ap"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("device: %v", err)
	}

	secs := []OwnedSection{
		{DeviceID: d.ID, Config: "wireless", Section: "default_radio0", RenderedHash: "h1"},
		{DeviceID: d.ID, Config: "wireless", Section: "default_radio1", RenderedHash: "h2"},
	}
	if err := db.RecordOwned(ctx, secs); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}
	// Re-applying the same sections with new hashes must update in place.
	secs[0].RenderedHash = "h1-updated"
	if err := db.RecordOwned(ctx, secs); err != nil {
		t.Fatalf("RecordOwned again: %v", err)
	}
	got, err := db.OwnedSections(ctx, d.ID)
	if err != nil {
		t.Fatalf("OwnedSections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 owned sections, got %d", len(got))
	}
	if got[0].RenderedHash != "h1-updated" {
		t.Errorf("hash should update in place, got %q", got[0].RenderedHash)
	}
	if got[0].AppliedAt == 0 {
		t.Error("applied_at should default to now")
	}

	if err := db.ForgetOwned(ctx, d.ID); err != nil {
		t.Fatalf("ForgetOwned: %v", err)
	}
	got, _ = db.OwnedSections(ctx, d.ID)
	if len(got) != 0 {
		t.Errorf("un-adopt should drop every ownership claim, got %d", len(got))
	}
}

// Deleting a device must not leave orphaned ownership claims behind, or a
// re-adopted device inherits stale beliefs about what we own.
func TestDeletingADeviceCascadesToOwnedSections(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:33:44:55", Host: "10.0.0.2", Name: "ap2"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("device: %v", err)
	}
	if err := db.RecordOwned(ctx, []OwnedSection{
		{DeviceID: d.ID, Config: "firewall", Section: "r1", RenderedHash: "h"}}); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, d.ID); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM owned_sections WHERE device_id=?`, d.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("foreign_keys=ON should cascade the delete, got %d orphans", n)
	}
}

func TestEventsRoundTripNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	for i, name := range []string{"first", "second", "third"} {
		if err := db.LogEvent(ctx, Event{
			TS: int64(1000 + i), Category: "audit", Severity: "info",
			Event: name, Detail: map[string]string{"outcome": name},
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	got, err := db.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	if got[0].Event != "third" {
		t.Errorf("newest first: got %q", got[0].Event)
	}
}

func TestSetCapabilitiesStoresSnapshot(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:66:77:88", Host: "10.0.0.3", Name: "ap3"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("device: %v", err)
	}
	caps := map[string]any{"dsa": "present", "airtime-split": "absent"}
	if err := db.SetCapabilities(ctx, d.ID, caps, "A"); err != nil {
		t.Fatalf("SetCapabilities: %v", err)
	}
	got, err := db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.Class != "A" {
		t.Errorf("class = %q, want A", got.Class)
	}
	if got.CapsJSON == "" || got.CapsJSON == "{}" {
		t.Errorf("capability snapshot not stored: %q", got.CapsJSON)
	}
}

func TestCheckpointTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "oonfee.db")
	db, err := Open(ctx, driver, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i := range 50 {
		if err := db.LogEvent(ctx, Event{
			Category: "test", Severity: "info", Event: "fill",
			Detail: map[string]any{"i": i},
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// TRUNCATE means the WAL is emptied, not merely folded in. That is what
	// makes copying the volume file a valid backup.
	fi, err := os.Stat(path + "-wal")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat WAL: %v", err)
	}
	if err == nil && fi.Size() != 0 {
		t.Fatalf("WAL is %d bytes after a TRUNCATE checkpoint, want 0", fi.Size())
	}
}

// A pin that quietly replaces itself is not a pin, so the second write must be
// refused rather than accepted.
func TestSetCertFPIsTrustOnFirstUseOnly(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.1.1", Name: "ap1", Scheme: "https"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if err := db.SetCertFP(ctx, d.ID, "aabb"); err != nil {
		t.Fatalf("first SetCertFP: %v", err)
	}
	got, err := db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.CertFP != "aabb" {
		t.Fatalf("CertFP = %q, want %q", got.CertFP, "aabb")
	}
	if err := db.SetCertFP(ctx, d.ID, "ccdd"); err == nil {
		t.Fatal("SetCertFP silently replaced an existing pin")
	}
	got, err = db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.CertFP != "aabb" {
		t.Fatalf("pin changed to %q despite the refusal", got.CertFP)
	}
}

// The whole point of a facet count is that it is NOT the count of what came
// back. UI-SPEC §5 says so, and the log endpoint returns a few hundred of what
// can be tens of thousands of rows — so a count taken from the page reports
// "3 errors" from a table holding three hundred, in the same typeface as a true
// number.
func TestEventFacetsCountTheTableNotThePage(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// 300 events: 250 info/system, 50 error/device.
	for i := 0; i < 250; i++ {
		if err := db.LogEvent(ctx, Event{TS: int64(1000 + i),
			Category: "system", Severity: "info", Event: "tick"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := db.LogEvent(ctx, Event{TS: int64(2000 + i),
			Category: "device", Severity: "error", Event: "poll_failed"}); err != nil {
			t.Fatal(err)
		}
	}

	// A page far smaller than the table, which is the situation that makes
	// page-derived counts wrong.
	page, err := db.QueryEventsPage(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 10 {
		t.Fatalf("page holds %d rows, want 10", len(page))
	}

	cats, sevs, total, err := db.EventFacets(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300 — the total must count the table", total)
	}
	if got := facetCount(sevs, "info"); got != 250 {
		t.Errorf("info = %d, want 250 (the page held only %d rows in total)",
			got, len(page))
	}
	if got := facetCount(sevs, "error"); got != 50 {
		t.Errorf("error = %d, want 50", got)
	}
	if got := facetCount(cats, "system"); got != 250 {
		t.Errorf("system = %d, want 250", got)
	}
}

// A facet must be counted with the OTHER filters applied but not its own.
// Applying a facet to itself shows the selected option's count and zero next to
// every alternative, which cannot answer the only question the rail is for:
// how many would I get if I clicked that instead?
func TestEventFacetsExcludeTheirOwnFilter(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	seed := []Event{
		{TS: 1, Category: "system", Severity: "info", Event: "a"},
		{TS: 2, Category: "system", Severity: "error", Event: "b"},
		{TS: 3, Category: "device", Severity: "error", Event: "c"},
		{TS: 4, Category: "device", Severity: "error", Event: "d"},
		{TS: 5, Category: "audit", Severity: "info", Event: "e"},
	}
	for _, e := range seed {
		if err := db.LogEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// Severity "error" selected. The severity rail must still offer "info" with
	// a real count, or there is no way back.
	cats, sevs, total, err := db.EventFacets(ctx, "", "error")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 — the total DOES apply every filter", total)
	}
	if got := facetCount(sevs, "info"); got != 2 {
		t.Errorf("with severity=error selected, info = %d, want 2; a severity "+
			"facet must not filter on severity or the rail becomes one-way", got)
	}
	// The category rail, meanwhile, is scoped to the selected severity: those
	// counts answer "how many errors in each category", which is the question.
	if got := facetCount(cats, "device"); got != 2 {
		t.Errorf("category device = %d, want 2 (errors only)", got)
	}
	if got := facetCount(cats, "audit"); got != 0 {
		t.Errorf("audit has no errors, so it should be absent from the "+
			"error-scoped category rail; got %d", got)
	}
}

func facetCount(fs []Facet, value string) int {
	for _, f := range fs {
		if f.Value == value {
			return f.Count
		}
	}
	return 0
}

// Paging must not repeat or skip rows.
func TestEventPagingWalksTheTableExactlyOnce(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if err := db.LogEvent(ctx, Event{TS: int64(1000 + i),
			Category: "system", Severity: "info", Event: "e" + itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for offset := 0; offset < 25; offset += 10 {
		page, err := db.QueryEventsPage(ctx, "", "", 10, offset)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			if seen[e.Event] {
				t.Errorf("%s appeared on two pages", e.Event)
			}
			seen[e.Event] = true
		}
	}
	if len(seen) != 25 {
		t.Errorf("paging saw %d of 25 events", len(seen))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
