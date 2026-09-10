package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchema23IndexesCanonicalGatewayFunction(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v22.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX devices_one_managed_gateway;
CREATE UNIQUE INDEX devices_one_managed_gateway ON devices(role)
 WHERE adopted_at IS NOT NULL AND management_mode='managed' AND role='gateway';
UPDATE schema_version SET version=22
 WHERE version=(SELECT MAX(version) FROM schema_version);
INSERT INTO devices(mac,host,name,role,functions_json,management_mode,adopted_at)
VALUES('02:00:00:00:23:01','192.0.2.1','mismatch-one','ap','["gateway"]','managed',1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v22: %v", err)
	}
	defer db.Close()
	if err := verifySchemaV23(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO devices(mac,host,name,role,functions_json,management_mode,adopted_at)
VALUES('02:00:00:00:23:02','198.51.100.1','mismatch-two','ap','["gateway"]','managed',1)`); err == nil {
		t.Fatal("schema admitted a second managed device claiming the Gateway function")
	}
	loaded, err := db.DeviceByMAC(ctx, "02:00:00:00:23:01")
	if err != nil || loaded.FunctionError != "" || loaded.Role != "gateway" || !loaded.Configurable() {
		t.Fatalf("migration did not canonicalize the sole safe claim: device=%+v err=%v", loaded, err)
	}
}

func TestSchema23MigrationRefusesTwoConflictingGatewayClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conflict.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX devices_one_managed_gateway;
CREATE UNIQUE INDEX devices_one_managed_gateway ON devices(role)
 WHERE adopted_at IS NOT NULL AND management_mode='managed' AND role='gateway';
UPDATE schema_version SET version=22
 WHERE version=(SELECT MAX(version) FROM schema_version);
INSERT INTO devices(mac,host,name,role,functions_json,management_mode,adopted_at) VALUES
 ('02:00:00:00:23:11','192.0.2.1','first','ap','["gateway"]','managed',1),
 ('02:00:00:00:23:12','198.51.100.1','second','switch','["gateway"]','managed',1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if reopened, err := Open(ctx, driver, path, protector); err == nil {
		reopened.Close()
		t.Fatal("conflicting gateway claims migrated successfully")
	} else if !strings.Contains(err.Error(), "devices_one_managed_gateway") && !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("migration error did not identify the durable conflict: %v", err)
	}
}

func TestSchema23MigrationCanSwapCanonicalGatewayRole(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "swap.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX devices_one_managed_gateway;
CREATE UNIQUE INDEX devices_one_managed_gateway ON devices(role)
 WHERE adopted_at IS NOT NULL AND management_mode='managed' AND role='gateway';
UPDATE schema_version SET version=22
 WHERE version=(SELECT MAX(version) FROM schema_version);
INSERT INTO devices(mac,host,name,role,functions_json,management_mode,adopted_at) VALUES
 ('02:00:00:00:23:31','192.0.2.1','becomes-gateway','ap','["gateway"]','managed',1),
 ('02:00:00:00:23:32','192.0.2.2','becomes-ap','gateway','["ap"]','managed',1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("safe role swap migration failed: %v", err)
	}
	defer db.Close()
	gateway, err := db.DeviceByMAC(ctx, "02:00:00:00:23:31")
	if err != nil || gateway.Role != "gateway" || gateway.FunctionError != "" {
		t.Fatalf("gateway after swap=%+v err=%v", gateway, err)
	}
	ap, err := db.DeviceByMAC(ctx, "02:00:00:00:23:32")
	if err != nil || ap.Role != "ap" || ap.FunctionError != "" {
		t.Fatalf("AP after swap=%+v err=%v", ap, err)
	}
}

func TestClientObservationOlderNonLocalSupersedesFutureLocal(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := &Device{MAC: "02:00:00:00:23:21", Host: "192.0.2.21", Name: "source",
		Role: "ap", Functions: []string{"ap"}}
	if err := db.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	const mac = "AA:BB:CC:DD:EE:21"
	future := time.Now().Add(MaxClientObservationFutureSkew + time.Hour).Unix()
	if err := db.UpsertClients(ctx, []SeenClient{{
		DeviceID: device.ID, MAC: mac, Scope: ScopeLocal,
	}}, future); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{{
		DeviceID: device.ID, MAC: mac, Scope: ScopeUpstream,
	}}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var storedMAC, scope string
	var seen int64
	if err := db.SQL().QueryRowContext(ctx, `
SELECT mac,scope,last_seen FROM client_observations WHERE device_id=?`, device.ID).
		Scan(&storedMAC, &scope, &seen); err != nil {
		t.Fatal(err)
	}
	if storedMAC != "aa:bb:cc:dd:ee:21" || scope != ScopeUpstream || seen != future {
		t.Fatalf("observation=(%q,%q,%d), want canonical upstream newest=%d", storedMAC, scope, seen, future)
	}
	if err := db.UpsertClients(ctx, []SeenClient{{
		DeviceID: device.ID, MAC: mac, Scope: ScopeLocal,
	}}, future); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `
SELECT scope,last_seen FROM client_observations WHERE device_id=?`, device.ID).
		Scan(&scope, &seen); err != nil {
		t.Fatal(err)
	}
	if scope != ScopeUpstream || seen != future {
		t.Fatalf("equal-time local observation reauthorized client: scope=%q last_seen=%d", scope, seen)
	}
	if err := db.UpsertClients(ctx, []SeenClient{{
		DeviceID: device.ID, MAC: mac, Scope: ScopeLocal,
	}}, future+1); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `
SELECT scope,last_seen FROM client_observations WHERE device_id=?`, device.ID).
		Scan(&scope, &seen); err != nil {
		t.Fatal(err)
	}
	if scope != ScopeLocal || seen != future+1 {
		t.Fatalf("strictly newer local observation did not restore scope: scope=%q last_seen=%d", scope, seen)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, device.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM client_observations`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("device delete left %d client observations: %v", rows, err)
	}
}
