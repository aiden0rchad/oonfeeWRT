package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchema21AddsDeviceManagementModeAndManagedGatewayConstraint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v20.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX devices_one_managed_gateway;
ALTER TABLE devices DROP COLUMN management_mode;
UPDATE schema_version SET version=20
 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v20: %v", err)
	}
	defer db.Close()
	if err := verifySchemaV21(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}

	at := int64(1)
	first := &Device{MAC: "02:00:00:00:21:01", Host: "192.0.2.1", Name: "managed",
		Role: "gateway", Functions: []string{"gateway"}, AdoptedAt: &at}
	if err := db.UpsertDevice(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &Device{MAC: "02:00:00:00:21:02", Host: "198.51.100.1", Name: "second",
		Role: "gateway", Functions: []string{"gateway"}, AdoptedAt: &at}
	if err := db.UpsertDevice(ctx, second); err == nil {
		t.Fatal("schema admitted a second managed gateway")
	}
	second.ManagementMode = "monitor_only"
	if err := db.UpsertDevice(ctx, second); err != nil {
		t.Fatalf("monitor-only gateway was rejected: %v", err)
	}
	loaded, err := db.DeviceByID(ctx, second.ID)
	if err != nil || loaded.ManagementMode != "monitor_only" || loaded.Configurable() {
		t.Fatalf("monitor-only round trip=(%+v,%v)", loaded, err)
	}
}

func TestUpsertDeviceRejectsUnknownManagementModeBeforeSQL(t *testing.T) {
	db := open(t)
	err := db.UpsertDevice(context.Background(), &Device{
		MAC: "02:00:00:00:21:03", Host: "203.0.113.1", Name: "bad-mode",
		ManagementMode: "multi_site",
	})
	if err == nil || !strings.Contains(err.Error(), "management mode") {
		t.Fatalf("unknown mode error=%v", err)
	}
}
