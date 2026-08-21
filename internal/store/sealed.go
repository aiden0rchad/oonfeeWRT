package store

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// SecretProtector is the keyring surface the store needs. The concrete Keeper
// remains in internal/secrets; keeping the interface here avoids making the
// persistence package own passphrase or keyring-file policy.
type SecretProtector interface {
	Seal(plaintext, aad []byte) ([]byte, error)
	Unseal(blob, aad []byte) ([]byte, error)
	VerifyCredential(mac string, blob []byte) error
}

const (
	secretAADVersion = "oonfeewrt/store-secret/v1"
	keyCheckPlain    = "oonfeewrt/store-key-check/v1"
)

// storedSecurity deliberately has no Key field. That makes persisting a WLAN
// passphrase in security_json impossible by construction rather than dependent
// on every caller remembering to blank one field before json.Marshal.
type storedSecurity struct {
	Mode model.SecurityMode `json:"mode"`
	PMF  model.PMF          `json:"pmf"`
}

// secretAAD length-frames every component. Delimiters are unsafe here: section
// names and future identifiers may contain the delimiter and make two different
// records authenticate under the same byte string.
func secretAAD(kind string, parts ...string) []byte {
	var out bytes.Buffer
	writeAADPart(&out, secretAADVersion)
	writeAADPart(&out, kind)
	for _, part := range parts {
		writeAADPart(&out, part)
	}
	return out.Bytes()
}

func writeAADPart(out *bytes.Buffer, part string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(part)))
	out.Write(size[:])
	out.WriteString(part)
}

func wlanKeyAAD(id int) []byte {
	return secretAAD("wlan-key", strconv.Itoa(id))
}

func meshKeyAAD(id int) []byte {
	return secretAAD("mesh-key", strconv.Itoa(id))
}

func ownedHashAAD(deviceID int64, config, section string) []byte {
	return secretAAD("owned-rendered-hash", strconv.FormatInt(deviceID, 10), config, section)
}

func keyCheckAAD() []byte { return secretAAD("key-check") }

func (db *DB) sealText(plain string, aad []byte) ([]byte, error) {
	if db.protector == nil {
		return nil, errors.New("store: secret protector is unavailable")
	}
	plaintext := []byte(plain)
	defer clear(plaintext)
	return db.protector.Seal(plaintext, aad)
}

func (db *DB) openText(blob []byte, aad []byte, what string) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	if db.protector == nil {
		return "", errors.New("store: secret protector is unavailable")
	}
	plain, err := db.protector.Unseal(blob, aad)
	if err != nil {
		return "", fmt.Errorf("store: cannot open %s (wrong keyring or corrupt record): %w", what, err)
	}
	defer clear(plain)
	return string(plain), nil
}

// verifyLegacyKeyring proves that the supplied keyring belongs to a pre-v14
// database before migration writes anything. A database with no sealed device
// credential has no historical binding to prove; its plaintext site keys may be
// paired with the supplied keyring for the first time.
func (db *DB) verifyLegacyKeyring(ctx context.Context) error {
	return db.verifyLegacyKeyringOn(ctx, db.sql)
}

type rowsQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (db *DB) verifyLegacyKeyringOn(ctx context.Context, q rowsQuerier) error {
	rows, err := q.QueryContext(ctx,
		`SELECT mac, cred_enc FROM devices WHERE cred_enc IS NOT NULL AND length(cred_enc) > 0 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("store: verify legacy keyring: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mac string
		var blob []byte
		if err := rows.Scan(&mac, &blob); err != nil {
			return fmt.Errorf("store: verify legacy keyring: %w", err)
		}
		if err := db.protector.VerifyCredential(mac, blob); err != nil {
			return fmt.Errorf("store: keyring does not open the existing credential for device %s; refusing schema migration: %w", mac, err)
		}
	}
	return rows.Err()
}

// verifySecretState binds every v14 open to the same keyring that encrypted the
// database. It runs before WAL mode, schema DDL, or any other database mutation.
func (db *DB) verifySecretState(ctx context.Context) (bool, error) {
	return db.verifySecretStateOn(ctx, db.sql)
}

func (db *DB) verifySecretStateOn(ctx context.Context, q schemaQuerier) (bool, error) {
	var blob []byte
	var complete int
	if err := q.QueryRowContext(ctx,
		`SELECT key_check, scrub_complete FROM secret_state WHERE id=1`).Scan(&blob, &complete); err != nil {
		return false, fmt.Errorf("store: schema v14 secret state is missing or unreadable: %w", err)
	}
	plain, err := db.protector.Unseal(blob, keyCheckAAD())
	if err != nil {
		return false, fmt.Errorf("store: keyring does not belong to this database; refusing to open it: %w", err)
	}
	defer clear(plain)
	if subtle.ConstantTimeCompare(plain, []byte(keyCheckPlain)) != 1 {
		return false, errors.New("store: database key check is malformed; refusing to open it")
	}
	return complete == 1, nil
}

type migrationExecutor interface {
	schemaQuerier
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type migratedWLAN struct {
	id       int
	security []byte
	key      []byte
}

type migratedMesh struct {
	id  int
	key []byte
}

type migratedOwned struct {
	deviceID int64
	config   string
	section  string
	hash     []byte
}

// migrateSecretsV14 seals every controller-side Wi-Fi secret and every
// secret-derived ownership verifier in one transaction. The schema version and
// pending-scrub marker commit with the ciphertext, so an older binary refuses a
// half-finished upgrade and this binary knows to resume the physical scrub.
func (db *DB) migrateSecretsV14Locked(ctx context.Context, tx migrationExecutor) error {
	for _, stmt := range []string{
		`ALTER TABLE wlans ADD COLUMN security_key_enc BLOB`,
		`ALTER TABLE meshes ADD COLUMN key_enc BLOB`,
		`ALTER TABLE owned_sections ADD COLUMN rendered_hash_enc BLOB`,
		`CREATE TABLE IF NOT EXISTS secret_state (
		   id INTEGER PRIMARY KEY CHECK (id = 1),
		   key_check BLOB NOT NULL,
		   scrub_complete INTEGER NOT NULL DEFAULT 0 CHECK (scrub_complete IN (0,1))
		 )`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil &&
			!strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return fmt.Errorf("store: schema v14 DDL: %w", err)
		}
	}

	wlans, err := readLegacyWLANs(ctx, tx, db)
	if err != nil {
		return err
	}
	meshes, err := readLegacyMeshes(ctx, tx, db)
	if err != nil {
		return err
	}
	owned, err := readLegacyOwned(ctx, tx, db)
	if err != nil {
		return err
	}

	for _, row := range wlans {
		if _, err := tx.ExecContext(ctx,
			`UPDATE wlans SET security_json=?, security_key_enc=? WHERE id=?`,
			string(row.security), nullableBlob(row.key), row.id); err != nil {
			return fmt.Errorf("store: migrate WLAN %d secret: %w", row.id, err)
		}
	}
	for _, row := range meshes {
		if _, err := tx.ExecContext(ctx,
			`UPDATE meshes SET key='', key_enc=? WHERE id=?`, nullableBlob(row.key), row.id); err != nil {
			return fmt.Errorf("store: migrate mesh %d secret: %w", row.id, err)
		}
	}
	for _, row := range owned {
		if _, err := tx.ExecContext(ctx,
			`UPDATE owned_sections SET rendered_hash='', rendered_hash_enc=?
			  WHERE device_id=? AND config=? AND section=?`,
			nullableBlob(row.hash), row.deviceID, row.config, row.section); err != nil {
			return fmt.Errorf("store: migrate ownership verifier for device %d %s.%s: %w",
				row.deviceID, row.config, row.section, err)
		}
	}

	check, err := db.protector.Seal([]byte(keyCheckPlain), keyCheckAAD())
	if err != nil {
		return fmt.Errorf("store: create database key check: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO secret_state (id, key_check, scrub_complete) VALUES (1,?,0)
		 ON CONFLICT(id) DO UPDATE SET key_check=excluded.key_check, scrub_complete=0`, check); err != nil {
		return fmt.Errorf("store: record database key check: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (14,?)`, time.Now().Unix()); err != nil {
		return fmt.Errorf("store: record schema v14: %w", err)
	}
	return nil
}

func readLegacyWLANs(ctx context.Context, tx migrationExecutor, db *DB) ([]migratedWLAN, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, security_json FROM wlans ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: read legacy WLAN secrets: %w", err)
	}
	defer rows.Close()
	var out []migratedWLAN
	for rows.Next() {
		var id int
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("store: read legacy WLAN secret: %w", err)
		}
		var old model.Security
		if err := json.Unmarshal([]byte(raw), &old); err != nil {
			return nil, fmt.Errorf("store: WLAN %d has unreadable security during migration: %w", id, err)
		}
		clean, err := json.Marshal(storedSecurity{Mode: old.Mode, PMF: old.PMF})
		if err != nil {
			return nil, fmt.Errorf("store: encode WLAN %d security during migration: %w", id, err)
		}
		var sealed []byte
		// Keyless modes must not retain a dormant passphrase. The v13 update path
		// accidentally did, so migration is the point where it is deleted.
		if old.Mode.NeedsKey() && old.Key != "" {
			sealed, err = db.sealText(old.Key, wlanKeyAAD(id))
			if err != nil {
				return nil, fmt.Errorf("store: seal WLAN %d key: %w", id, err)
			}
		}
		out = append(out, migratedWLAN{id: id, security: clean, key: sealed})
	}
	return out, rows.Err()
}

func readLegacyMeshes(ctx context.Context, tx migrationExecutor, db *DB) ([]migratedMesh, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, key FROM meshes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: read legacy mesh secrets: %w", err)
	}
	defer rows.Close()
	var out []migratedMesh
	for rows.Next() {
		var id int
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("store: read legacy mesh secret: %w", err)
		}
		var sealed []byte
		if key != "" {
			sealed, err = db.sealText(key, meshKeyAAD(id))
			if err != nil {
				return nil, fmt.Errorf("store: seal mesh %d key: %w", id, err)
			}
		}
		out = append(out, migratedMesh{id: id, key: sealed})
	}
	return out, rows.Err()
}

func readLegacyOwned(ctx context.Context, tx migrationExecutor, db *DB) ([]migratedOwned, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT device_id, config, section, rendered_hash FROM owned_sections
		  ORDER BY device_id, config, section`)
	if err != nil {
		return nil, fmt.Errorf("store: read legacy ownership verifiers: %w", err)
	}
	defer rows.Close()
	var out []migratedOwned
	for rows.Next() {
		var row migratedOwned
		var hash string
		if err := rows.Scan(&row.deviceID, &row.config, &row.section, &hash); err != nil {
			return nil, fmt.Errorf("store: read legacy ownership verifier: %w", err)
		}
		if hash != "" {
			row.hash, err = db.sealText(hash,
				ownedHashAAD(row.deviceID, row.config, row.section))
			if err != nil {
				return nil, fmt.Errorf("store: seal ownership verifier for device %d %s.%s: %w",
					row.deviceID, row.config, row.section, err)
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// finishSecretScrub is deliberately idempotent. If power is lost after the
// logical migration commits, scrub_complete remains zero and the next writable
// open repeats the physical cleanup before the HTTP server can start.
func (db *DB) finishSecretScrub(ctx context.Context) error {
	var complete int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&complete); err != nil {
		return fmt.Errorf("store: read schema v14 scrub state: %w", err)
	}
	if complete == 1 {
		// A crash can land after committing the marker but before its final
		// checkpoint. Fold it in before serving so a subsequent clean main-file
		// backup never depends on an old WAL sidecar.
		return db.Checkpoint(ctx)
	}
	if err := db.Checkpoint(ctx); err != nil {
		return fmt.Errorf("store: pre-scrub checkpoint: %w", err)
	}
	if _, err := db.sql.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("store: scrub legacy plaintext with VACUUM: %w", err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		return fmt.Errorf("store: post-scrub checkpoint: %w", err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE secret_state SET scrub_complete=1 WHERE id=1`); err != nil {
		return fmt.Errorf("store: complete schema v14 scrub: %w", err)
	}
	// The marker itself contains no secret, but folding it in keeps the documented
	// clean-shutdown single-file backup property true immediately after migration.
	if err := db.Checkpoint(ctx); err != nil {
		return fmt.Errorf("store: checkpoint schema v14 completion: %w", err)
	}
	return nil
}

func nullableBlob(blob []byte) any {
	if len(blob) == 0 {
		return nil
	}
	return blob
}
