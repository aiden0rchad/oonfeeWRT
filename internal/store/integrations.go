package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/aiden0rchad/oonfeewrt/internal/integrations"
)

const integrationSchemaSQL = `CREATE TABLE IF NOT EXISTS controller_adguard_config (
 id INTEGER PRIMARY KEY CHECK (id=1),
 config_enc BLOB NOT NULL CHECK (length(config_enc)>0 AND length(config_enc)<=16384)
)`

func (db *DB) LoadAdGuardConfig(ctx context.Context) (*integrations.AdGuardConfig, error) {
	var blob []byte
	err := db.sql.QueryRowContext(ctx, `SELECT config_enc FROM controller_adguard_config WHERE id=1`).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("store: integration configuration could not be read")
	}
	return db.decodeAdGuardConfig(blob)
}

func (db *DB) decodeAdGuardConfig(blob []byte) (*integrations.AdGuardConfig, error) {
	if len(blob) == 0 || len(blob) > 16384 {
		return nil, errors.New("store: integration ciphertext is invalid")
	}
	plain, err := db.openText(blob, secretAAD("adguard-config"), "AdGuard Home configuration")
	if err != nil {
		return nil, errors.New("store: integration configuration could not be authenticated")
	}
	var config integrations.AdGuardConfig
	if json.Unmarshal([]byte(plain), &config) != nil || config.Normalize() != nil {
		return nil, errors.New("store: integration configuration is invalid")
	}
	return &config, nil
}

func (db *DB) SaveAdGuardConfig(ctx context.Context, config integrations.AdGuardConfig, actor string) error {
	if err := config.Normalize(); err != nil {
		return err
	}
	plain, err := json.Marshal(config)
	if err != nil {
		return errors.New("store: integration configuration could not be encoded")
	}
	defer clear(plain)
	blob, err := db.protector.Seal(plain, secretAAD("adguard-config"))
	if err != nil {
		return errors.New("store: integration configuration could not be encrypted")
	}
	return db.writeAdGuardConfig(ctx, blob, actor)
}

func (db *DB) DeleteAdGuardConfig(ctx context.Context, actor string) error {
	return db.writeAdGuardConfig(ctx, nil, actor)
}

func (db *DB) writeAdGuardConfig(ctx context.Context, blob []byte, actor string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("store: integration configuration could not be saved")
	}
	defer tx.Rollback()
	action := "integration.adguard.configured"
	if blob == nil {
		action = "integration.adguard.removed"
		_, err = tx.ExecContext(ctx, `DELETE FROM controller_adguard_config WHERE id=1`)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO controller_adguard_config(id,config_enc) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET config_enc=excluded.config_enc`, blob)
	}
	if err != nil {
		return errors.New("store: integration configuration could not be saved")
	}
	event, detail, err := normalizeEvent(Event{Category: "audit", Severity: "info", Event: action, Detail: map[string]string{"actor": actor}})
	if err != nil {
		return errors.New("store: integration change could not be audited")
	}
	if _, err := tx.ExecContext(ctx, appendEventSQL, eventInsertArgs(event, detail)...); err != nil {
		return errors.New("store: integration change could not be audited")
	}
	return tx.Commit()
}

func (db *DB) validateRecoveryIntegrations(ctx context.Context, tx *sql.Tx) error {
	var blob []byte
	err := tx.QueryRowContext(ctx, `SELECT config_enc FROM controller_adguard_config WHERE id=1`).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return errors.New("store: integration recovery configuration could not be read")
	}
	_, err = db.decodeAdGuardConfig(blob)
	return err
}
