package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSchema26MigratesV25AndCreatesPortRoles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v25.db")
	keeper := testProtector(t, path)
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP TABLE port_roles;
UPDATE schema_version SET version=25 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatalf("migrate v25: %v", err)
	}
	defer db.Close()
	if err := verifySchemaV26(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
}
