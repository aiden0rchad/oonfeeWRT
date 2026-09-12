package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/integrations"
)

func TestAdGuardConfigIsEncryptedAndRecoveryAuthenticatesIt(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	config := integrations.AdGuardConfig{URL: "https://dns.example.test", Username: "integration-reader", Password: "PASSWORD-SENTINEL"}
	if err := db.SaveAdGuardConfig(ctx, config, "owner"); err != nil {
		t.Fatal(err)
	}
	var blob []byte
	if err := db.sql.QueryRowContext(ctx, `SELECT config_enc FROM controller_adguard_config WHERE id=1`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(config.Password)) || bytes.Contains(blob, []byte(config.URL)) || bytes.Contains(blob, []byte(config.Username)) {
		t.Fatal("plaintext configuration persisted")
	}
	got, err := db.LoadAdGuardConfig(ctx)
	if err != nil || got == nil || *got != config {
		t.Fatalf("roundtrip failed: %v", err)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.validateRecoveryIntegrations(ctx, tx); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	blob[len(blob)-1] ^= 0xff
	if _, err := db.sql.ExecContext(ctx, `UPDATE controller_adguard_config SET config_enc=? WHERE id=1`, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadAdGuardConfig(ctx); err == nil {
		t.Fatal("corrupt ciphertext accepted")
	}
	tx, err = db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.validateRecoveryIntegrations(ctx, tx); err == nil {
		t.Fatal("recovery accepted corrupt integration")
	}
	tx.Rollback()
	if err := db.DeleteAdGuardConfig(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if config, err := db.LoadAdGuardConfig(ctx); err != nil || config != nil {
		t.Fatal("configuration not removed")
	}
}

func TestAdGuardChangeAndAuditAreAtomic(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	config := integrations.AdGuardConfig{URL: "https://dns.example.test", Username: "reader", Password: "PASSWORD-SENTINEL"}
	if _, err := db.sql.ExecContext(ctx, `CREATE TRIGGER reject_integration_audit BEFORE INSERT ON events WHEN NEW.event='integration.adguard.configured' BEGIN SELECT RAISE(ABORT,'fixture audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAdGuardConfig(ctx, config, "owner"); err == nil {
		t.Fatal("save ignored audit failure")
	}
	if got, err := db.LoadAdGuardConfig(ctx); err != nil || got != nil {
		t.Fatal("config committed without audit")
	}
}
