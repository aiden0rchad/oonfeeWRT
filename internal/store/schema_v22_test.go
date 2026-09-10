package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSchema22AddsReusablePolicySets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v21.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP TABLE policy_set_members;
DROP TABLE policy_sets;
UPDATE schema_version SET version=21
 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v21: %v", err)
	}
	defer db.Close()
	if err := verifySchemaV22(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version=%d, want %d", version, schemaVersion)
	}
}

func TestSchema22AttestationRejectsMissingNameIndex(t *testing.T) {
	db := open(t)
	if _, err := db.SQL().Exec(`DROP INDEX policy_sets_name_nocase`); err != nil {
		t.Fatal(err)
	}
	if err := verifySchemaV22(context.Background(), db.SQL()); err == nil {
		t.Fatal("schema without case-insensitive policy set names passed attestation")
	}
}
