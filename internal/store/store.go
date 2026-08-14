// Package store is the controller's durable state: one SQLite database, WAL
// mode, holding inventory, the site model, reconciliation bookkeeping and
// telemetry rollups.
//
// The device holds none of this. That is the point — DEVICE-BUDGET's hard rule
// is zero flash writes on the router in steady state, which is only affordable
// because everything durable lives here instead.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the migration level this build expects.
const schemaVersion = 2

// migrations are applied in order for any database below schemaVersion.
//
// Forward-only, and never by editing schema.sql: that file uses CREATE TABLE IF
// NOT EXISTS, so a changed column list is silently ignored on a database that
// already exists. Anything that changes an existing table has to appear here.
var migrations = map[int][]string{
	2: {
		// The address a client was last seen at, as observed. Distinct from
		// fixed_ip, which is a reservation an operator asked for — conflating
		// "where it is" with "where it must be" would make a DHCP lease look
		// like a policy.
		`ALTER TABLE clients ADD COLUMN ip TEXT`,
	},
}

// DB is the controller's database handle.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the database at path and migrates it.
//
// driverName is passed in rather than imported so this package does not force a
// SQLite driver on its dependents or on tests. Production wires
// "sqlite" (modernc.org/sqlite — pure Go, per decision D3, which is what lets
// the container be FROM scratch with CGO_ENABLED=0).
func Open(ctx context.Context, driverName, path string) (*DB, error) {
	sqldb, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite is single-writer. More than one connection buys contention and
	// SQLITE_BUSY, not throughput.
	sqldb.SetMaxOpenConns(1)

	db := &DB{sql: sqldb}
	if err := db.pragmas(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	if err := db.migrate(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	return db, nil
}

// pragmas applies the settings IMPLEMENTATION §3 specifies. wal_autocheckpoint
// is deliberately large: the controller writes one transaction per 5-minute
// maintenance tick, and checkpointing more eagerly buys nothing.
func (db *DB) pragmas(ctx context.Context) error {
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA wal_autocheckpoint=200",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.sql.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("store: %s: %w", p, err)
		}
	}
	return nil
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	var current int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	if err != nil {
		return fmt.Errorf("store: read schema_version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("store: database is at schema v%d but this build "+
			"understands v%d — refusing to downgrade", current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}
	for v := current + 1; v <= schemaVersion; v++ {
		for _, stmt := range migrations[v] {
			if _, err := db.sql.ExecContext(ctx, stmt); err != nil {
				// A fresh database gets its columns from schema.sql, so the
				// ALTER is a duplicate there rather than a failure. Anything
				// else is real.
				if !strings.Contains(err.Error(), "duplicate column name") {
					return fmt.Errorf("store: migration to v%d (%s): %w", v, stmt, err)
				}
			}
		}
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
		schemaVersion, time.Now().Unix())
	return err
}

// Close releases the database.
func (db *DB) Close() error { return db.sql.Close() }

// Checkpoint folds the write-ahead log back into the database file and
// truncates it. Shutdown calls this so the volume can be copied or restored as
// a single file, which is what the backup instructions in IMPLEMENTATION §11
// tell an operator to do.
//
// TRUNCATE blocks on active readers rather than doing a partial job, so the
// caller should run it after the serving paths have stopped. A busy database
// returns SQLITE_BUSY here, which is a real failure to report: it means the WAL
// still holds committed data that a naive file copy would miss.
func (db *DB) Checkpoint(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("store: checkpoint WAL: %w", err)
	}
	return nil
}

// SQL exposes the handle for packages that need their own queries.
func (db *DB) SQL() *sql.DB { return db.sql }

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("store: not found")

// Device is one managed (or pending) OpenWrt device.
//
// CredEnc holds the sealed controller credential. The operator credential used
// at adoption is NEVER stored: it is held in memory for that one transaction
// and requested again at un-adopt, because a controller that could remove its
// own ACL could also rewrite it (ARCHITECTURE §6).
type Device struct {
	ID        int64
	MAC       string
	Host      string
	Port      int
	Scheme    string
	CertFP    string
	Name      string
	Role      string
	AdoptedAt *int64
	CredEnc   []byte
	Class     string
	CapsJSON  string
	FWRelease string
	LastSeen  *int64
	PollState string
}

// Adopted reports whether adoption completed.
func (d Device) Adopted() bool { return d.AdoptedAt != nil }

// UpsertDevice inserts or updates by MAC, which is the stable identity — a
// device's address can change, its MAC does not.
func (db *DB) UpsertDevice(ctx context.Context, d *Device) error {
	if d.MAC == "" {
		return errors.New("store: device MAC is required")
	}
	if d.Port == 0 {
		d.Port = 80
	}
	if d.Scheme == "" {
		d.Scheme = "http"
	}
	if d.Role == "" {
		d.Role = "ap"
	}
	if d.PollState == "" {
		d.PollState = "baseline"
	}
	if d.CapsJSON == "" {
		d.CapsJSON = "{}"
	}
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO devices (mac, host, port, scheme, cert_fp, name, role, adopted_at,
                     cred_enc, class, caps_json, fw_release, last_seen, poll_state)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(mac) DO UPDATE SET
  host=excluded.host, port=excluded.port, scheme=excluded.scheme,
  cert_fp=excluded.cert_fp, name=excluded.name, role=excluded.role,
  adopted_at=excluded.adopted_at, cred_enc=excluded.cred_enc,
  class=excluded.class, caps_json=excluded.caps_json,
  fw_release=excluded.fw_release, last_seen=excluded.last_seen,
  poll_state=excluded.poll_state`,
		d.MAC, d.Host, d.Port, d.Scheme, nullString(d.CertFP), d.Name, d.Role,
		d.AdoptedAt, d.CredEnc, nullString(d.Class), d.CapsJSON,
		nullString(d.FWRelease), d.LastSeen, d.PollState)
	if err != nil {
		return fmt.Errorf("store: upsert device %s: %w", d.MAC, err)
	}
	if d.ID == 0 {
		if id, err := res.LastInsertId(); err == nil && id > 0 {
			d.ID = id
		} else {
			// ON CONFLICT UPDATE does not report a useful LastInsertId.
			_ = db.sql.QueryRowContext(ctx,
				`SELECT id FROM devices WHERE mac=?`, d.MAC).Scan(&d.ID)
		}
	}
	return nil
}

// DeviceByMAC looks a device up by its stable identity.
func (db *DB) DeviceByMAC(ctx context.Context, mac string) (*Device, error) {
	row := db.sql.QueryRowContext(ctx, deviceCols+` WHERE mac=?`, mac)
	return scanDevice(row)
}

// DeviceByID looks a device up by its row id, which is what URLs carry.
func (db *DB) DeviceByID(ctx context.Context, id int64) (*Device, error) {
	row := db.sql.QueryRowContext(ctx, deviceCols+` WHERE id=?`, id)
	return scanDevice(row)
}

// Devices lists every known device, pending ones included.
func (db *DB) Devices(ctx context.Context) ([]*Device, error) {
	rows, err := db.sql.QueryContext(ctx, deviceCols+` ORDER BY name, mac`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const deviceCols = `SELECT id, mac, host, port, scheme, COALESCE(cert_fp,''),
 name, role, adopted_at, cred_enc, COALESCE(class,''), caps_json,
 COALESCE(fw_release,''), last_seen, poll_state FROM devices`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(s scanner) (*Device, error) {
	var d Device
	err := s.Scan(&d.ID, &d.MAC, &d.Host, &d.Port, &d.Scheme, &d.CertFP,
		&d.Name, &d.Role, &d.AdoptedAt, &d.CredEnc, &d.Class, &d.CapsJSON,
		&d.FWRelease, &d.LastSeen, &d.PollState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// SetCertFP records a trust-on-first-use certificate pin.
//
// It writes only when the column is still empty. A pin that silently updates
// itself is not a pin: the whole value of TOFU is that the *second* certificate
// is rejected, so an overwrite has to be a deliberate re-pin by an operator who
// knows why the device's certificate changed, not a side effect of connecting.
func (db *DB) SetCertFP(ctx context.Context, deviceID int64, fp string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET cert_fp=? WHERE id=? AND (cert_fp IS NULL OR cert_fp='')`,
		fp, deviceID)
	if err != nil {
		return fmt.Errorf("store: pin certificate for device %d: %w", deviceID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: device %d already has a pinned certificate; "+
			"re-pinning must be an explicit operator action", deviceID)
	}
	return nil
}

// SetLastSeen records that a device answered, and its current poll state.
//
// This is the one write the poll loop makes, and it is deliberately narrow: a
// full UpsertDevice on every poll would rewrite the sealed credential and the
// capability snapshot sixty times an hour for no reason, and any bug in the
// caller would then be able to lose them.
func (db *DB) SetLastSeen(ctx context.Context, deviceID int64, ts int64, pollState string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET last_seen=?, poll_state=? WHERE id=?`,
		ts, pollState, deviceID)
	if err != nil {
		return fmt.Errorf("store: touch device %d: %w", deviceID, err)
	}
	return nil
}

// SetFirmware records the release string the board reported. Called whenever a
// poll re-reads the board, which is how an upgrade becomes visible without
// anyone telling the controller about it.
func (db *DB) SetFirmware(ctx context.Context, deviceID int64, release string) error {
	if release == "" {
		return nil
	}
	_, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET fw_release=? WHERE id=? AND COALESCE(fw_release,'') != ?`,
		release, deviceID, release)
	return err
}

// SetCapabilities stores a capability registry snapshot against a device.
func (db *DB) SetCapabilities(ctx context.Context, deviceID int64, caps any, class string) error {
	blob, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		`UPDATE devices SET caps_json=?, class=? WHERE id=?`,
		string(blob), nullString(class), deviceID)
	return err
}

// OwnedSection records that we wrote one UCI section, and what we wrote.
type OwnedSection struct {
	DeviceID     int64
	Config       string
	Section      string
	RenderedHash string
	AppliedAt    int64
}

// RecordOwned marks a section as ours after a confirmed apply.
//
// Only call this once the apply is CONFIRMED. Recording at stage time would
// claim ownership of sections the device may revert seconds later, and the
// reconciler would then believe it owns config that is not there.
func (db *DB) RecordOwned(ctx context.Context, secs []OwnedSection) error {
	if len(secs) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO owned_sections (device_id, config, section, rendered_hash, applied_at)
VALUES (?,?,?,?,?)
ON CONFLICT(device_id, config, section) DO UPDATE SET
  rendered_hash=excluded.rendered_hash, applied_at=excluded.applied_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, s := range secs {
		if s.AppliedAt == 0 {
			s.AppliedAt = time.Now().Unix()
		}
		if _, err := stmt.ExecContext(ctx, s.DeviceID, s.Config, s.Section,
			s.RenderedHash, s.AppliedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OwnedSections returns everything we believe we own on a device.
func (db *DB) OwnedSections(ctx context.Context, deviceID int64) ([]OwnedSection, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT device_id, config, section, rendered_hash, applied_at
		 FROM owned_sections WHERE device_id=? ORDER BY config, section`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnedSection
	for rows.Next() {
		var s OwnedSection
		if err := rows.Scan(&s.DeviceID, &s.Config, &s.Section,
			&s.RenderedHash, &s.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ForgetOwned drops our ownership claims for a device. Used at un-adopt, after
// the sections themselves have been reverted on the device.
func (db *DB) ForgetOwned(ctx context.Context, deviceID int64) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM owned_sections WHERE device_id=?`, deviceID)
	return err
}

// Event is an audit or telemetry event.
type Event struct {
	TS       int64
	DeviceID *int64
	Category string // client|device|security|system|audit
	Severity string // info|warning|error
	Event    string
	Detail   any
}

// LogEvent appends to the event log. Every apply outcome lands here, including
// the Unknown one — especially that one.
func (db *DB) LogEvent(ctx context.Context, e Event) error {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	detail := "{}"
	if e.Detail != nil {
		blob, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = string(blob)
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO events (ts, device_id, category, severity, event, detail_json)
		 VALUES (?,?,?,?,?,?)`,
		e.TS, e.DeviceID, e.Category, e.Severity, e.Event, detail)
	return err
}

// RecentEvents returns the newest events first.
func (db *DB) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.QueryContext(ctx,
		`SELECT ts, device_id, category, severity, event, detail_json
		 FROM events ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var detail string
		if err := rows.Scan(&e.TS, &e.DeviceID, &e.Category, &e.Severity,
			&e.Event, &detail); err != nil {
			return nil, err
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
