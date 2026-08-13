package store

import (
	"context"
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
