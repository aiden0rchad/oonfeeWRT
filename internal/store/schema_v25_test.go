package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/integrations"
)

func TestSchema25MigratesV24AndPreservesExistingState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v24.db")
	keeper := testProtector(t, path)
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO controller_alert_state(id,state_json) VALUES(1,'{}');
DROP TABLE controller_adguard_config;
UPDATE schema_version SET version=24 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatalf("migrate v24: %v", err)
	}
	if err := verifySchemaV25(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	var version int
	var priorState string
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT state_json FROM controller_alert_state WHERE id=1`).Scan(&priorState); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || priorState != "{}" {
		t.Fatalf("migration state: version=%d alerts=%q", version, priorState)
	}
	if config, err := db.LoadAdGuardConfig(ctx); err != nil || config != nil {
		t.Fatalf("new integration must be unconfigured: %v", err)
	}
	config := integrations.AdGuardConfig{URL: "https://dns.example.test", Username: "reader"}
	if err := db.SaveAdGuardConfig(ctx, config, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	defer db.Close()
	if got, err := db.LoadAdGuardConfig(ctx); err != nil || got == nil || *got != config {
		t.Fatalf("migrated integration lost on reopen: %v", err)
	}
}

func TestSchema25BoundsRejectInvalidRows(t *testing.T) {
	db := open(t)
	for _, query := range []string{
		`INSERT INTO controller_adguard_config VALUES(2,zeroblob(1))`,
		`INSERT INTO controller_adguard_config VALUES(1,zeroblob(0))`,
		`INSERT INTO controller_adguard_config VALUES(1,zeroblob(16385))`,
		`INSERT INTO controller_adguard_config VALUES(1,NULL)`,
	} {
		if _, err := db.SQL().Exec(query); err == nil {
			t.Fatalf("invalid integration row accepted: %s", query)
		}
	}
}

func TestSchema25RejectsMalformedChecksAndRollsBackMigration(t *testing.T) {
	for name, checks := range map[string]string{
		"missing checks":       "",
		"missing upper bound":  ", CHECK(id=1), CHECK(length(config_enc)>0)",
		"weakened upper bound": ", CHECK(id=1), CHECK(length(config_enc)>0 AND length(config_enc)<=32768)",
		"weakened singleton":   ", CHECK(id>=1), CHECK(length(config_enc)>0 AND length(config_enc)<=16384)",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "collision.db")
			keeper := testProtector(t, path)
			db, err := Open(ctx, driver, path, keeper)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().ExecContext(ctx, `
DROP TABLE controller_adguard_config;
UPDATE schema_version SET version=24 WHERE version=(SELECT MAX(version) FROM schema_version);
CREATE TABLE controller_adguard_config(id INTEGER PRIMARY KEY, config_enc BLOB NOT NULL`+checks+`)`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if migrated, err := Open(ctx, driver, path, keeper); err == nil {
				migrated.Close()
				t.Fatal("migration accepted malformed integration bounds")
			} else if !strings.Contains(err.Error(), "schema v25 attestation") {
				t.Fatalf("wrong migration failure: %v", err)
			}
			raw, err := sql.Open(driver, path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var version int
			if err := raw.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 24 {
				t.Fatalf("failed migration advanced schema to %d", version)
			}
		})
	}
}
