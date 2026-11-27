package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSchema27MigratesV26AndDefaultsRouterAccessClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v26.db")
	keeper := testProtector(t, path)
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySchemaV27(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	// Put the networks table back to its v26 shape and leave one row that
	// predates the switch. Reopening must add the column as 0, not as open.
	if _, err := db.SQL().ExecContext(ctx, `
PRAGMA foreign_keys=OFF;
CREATE TABLE networks_v26 (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  vlan INTEGER NOT NULL UNIQUE, cidr TEXT NOT NULL,
  zone TEXT NOT NULL DEFAULT 'lan',
  dhcp_json TEXT NOT NULL DEFAULT '{}',
  ipv6_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1
);
INSERT INTO networks_v26 (id, name, vlan, cidr, zone, dhcp_json, ipv6_json, enabled)
  SELECT id, name, vlan, cidr, zone, dhcp_json, ipv6_json, enabled FROM networks;
DROP TABLE networks;
ALTER TABLE networks_v26 RENAME TO networks;
INSERT INTO networks (name, vlan, cidr, zone, enabled)
  VALUES ('legacy', 50, '192.168.50.1/24', 'dom', 1);
UPDATE schema_version SET version=26 WHERE version=(SELECT MAX(version) FROM schema_version);
PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatalf("migrate v26: %v", err)
	}
	defer db.Close()
	if err := verifySchemaV27(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	var version, allow int
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT allow_router_access FROM networks WHERE name='legacy'`).Scan(&allow); err != nil {
		t.Fatal(err)
	}
	if allow != 0 {
		t.Fatalf("upgraded row allow_router_access = %d, want 0", allow)
	}
}
