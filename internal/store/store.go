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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the migration level this build expects.
const schemaVersion = 16

// secretSchemaVersion is the one-time plaintext-to-ciphertext migration. Keep
// it explicit: a future schema bump must never re-run it against already
// scrubbed legacy columns and overwrite ciphertext with NULL.
const secretSchemaVersion = 14

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
	3: {
		// Which side of the router a client is on: "local", "upstream", or ""
		// for not yet determined. A gateway's ARP and neighbour tables cover
		// every interface, so without this the client list mixes the network's
		// own devices with the uplink's neighbours and cannot tell them apart —
		// measured 8 upstream of 16 on the reference device.
		`ALTER TABLE clients ADD COLUMN scope TEXT`,
	},
	4: {
		// A per-device baseline poll interval, in seconds. 0 means the
		// controller default. It can only make polling cheaper — the collector
		// clamps anything below the default — because DEVICE-BUDGET's ceiling
		// is a promise, and a knob that could raise the rate would make it a
		// suggestion that no test measures.
		`ALTER TABLE devices ADD COLUMN poll_interval_s INTEGER NOT NULL DEFAULT 0`,
	},
	5: {
		// The site row. One per controller, created on first read.
		//
		// Its UUID seeds the deterministic mobility-domain derivation
		// (IMPLEMENTATION §5) that lets every AP compute the same 802.11r
		// domain without coordination, so it is generated once and never
		// changes — regenerating it would silently re-key roaming fleet-wide.
		`CREATE TABLE IF NOT EXISTS site (
		   id INTEGER PRIMARY KEY CHECK (id = 1),
		   uuid TEXT NOT NULL,
		   name TEXT NOT NULL DEFAULT 'Site'
		 )`,
	},
	6: {
		// The client list's "connection" rail asks whether a MAC has recent
		// station telemetry, which is a lookup by (kind, key). series' only
		// index is UNIQUE(device_id, kind, key), and device_id is the leftmost
		// column — so a query that does not know the device cannot use it and
		// scans the table once per client row.
		`CREATE INDEX IF NOT EXISTS series_kind_key ON series(kind, key)`,
	},
	7: {
		// 802.11s mesh backhauls. A separate table from wlans, not a column on
		// it, because a mesh point is a different interface mode rather than a
		// WLAN with a flag: it has a mesh ID instead of an SSID, exactly one
		// band instead of a list (nodes peer only within a band), and no
		// concept of the roaming or client-isolation options a WLAN carries.
		`CREATE TABLE IF NOT EXISTS meshes (
		   id INTEGER PRIMARY KEY,
		   mesh_id TEXT NOT NULL,
		   network_id INTEGER NOT NULL REFERENCES networks(id),
		   group_id INTEGER NOT NULL REFERENCES ap_groups(id),
		   band TEXT NOT NULL,
		   key TEXT NOT NULL DEFAULT '',
		   enabled INTEGER NOT NULL DEFAULT 1
		 )`,
	},
	8: {
		// Wireless uplinks: a device that reaches the network over the air
		// rather than over a cable.
		//
		// A table rather than a column on devices, and one row per device by
		// UNIQUE, because a router with two wireless uplinks into the same
		// network is a layer-2 loop rather than redundancy — the constraint
		// says so once, here, instead of every writer remembering.
		//
		// ON DELETE CASCADE from devices: an un-adopted device's uplink is not
		// a thing to keep. It describes how that device reaches a network it
		// is no longer part of, and leaving it behind would put a row in the
		// site model that renders for nobody.
		`CREATE TABLE IF NOT EXISTS uplinks (
		   id INTEGER PRIMARY KEY,
		   device_id INTEGER NOT NULL UNIQUE REFERENCES devices(id) ON DELETE CASCADE,
		   wlan_id INTEGER NOT NULL REFERENCES wlans(id),
		   band TEXT NOT NULL,
		   enabled INTEGER NOT NULL DEFAULT 1
		 )`,
		// No column for the AP half. WLANOptions is stored whole in
		// wlans.options_json, so AllowUplink persists with the rest of it and
		// an `allow_uplink` column would be a second place for the same fact to
		// live — which is how two sources of one truth start disagreeing. It
		// defaults to false for every existing row because a missing JSON key
		// unmarshals to the zero value, which is the answer we want: a network
		// nobody asked to accept wireless bridges does not accept them.
	},
	9: {
		// The device's SSH host key, pinned at adoption.
		//
		// NULL for every device adopted before this migration, and that is the
		// honest value: nobody recorded a key for them, and inventing one would
		// pin whatever answers next — which is the thing a pin exists to catch.
		// Those devices stay trust-on-first-use until an un-adopt learns their
		// key (see daemon.Unadopt).
		`ALTER TABLE devices ADD COLUMN host_key_fp TEXT`,
	},
	10: {
		// Semantic compatibility boundary for configurable DHCP policy.
		//
		// networks.dhcp_json has existed since the first schema, so there is no
		// DDL to apply. Older v9 binaries nevertheless ignore its contents and
		// always render the historical 100/150/12h pool. Marking a database v10
		// makes those binaries refuse to open it instead of silently re-enabling
		// or resetting DHCP after an operator has customized or disabled it.
	},
	11: {
		// Roles used to bundle responsibilities: gateway also meant AP and
		// switch, AP also meant switch. Keep role for old clients, but persist
		// the independent set so a routing-only gateway or AP-only device does
		// not silently inherit responsibilities from its primary label.
		`ALTER TABLE devices ADD COLUMN functions_json TEXT NOT NULL DEFAULT '["ap","switch"]'`,
		`UPDATE devices SET functions_json = CASE lower(trim(role))
		   WHEN 'gateway' THEN '["gateway","ap","switch"]'
		   WHEN 'switch' THEN '["switch"]'
		   ELSE '["ap","switch"]'
		 END`,
	},
	12: {
		// Semantic compatibility boundary for directional zone policy.
		//
		// The zones table and policy_json column have existed since v1, so no
		// DDL is needed. A v11 binary ignores those rows and always renders a
		// forwarding to wan, however, which would silently undo an explicit
		// block or inter-zone policy. The version bump makes it refuse the DB.
	},
	13: {
		// Durable Apply receipts. An HTTP response can disappear while the
		// detached fleet run continues, so the result must be recoverable by an
		// operation id rather than existing only in the response body.
		//
		// The table stores a keyed request digest and a redacted result. It never
		// stores the preview token, desired model or device plan.
		`CREATE TABLE IF NOT EXISTS apply_operations (
		   operation_id TEXT PRIMARY KEY,
		   request_hash TEXT NOT NULL,
		   actor_admin_id INTEGER NOT NULL,
		   actor_username TEXT NOT NULL,
		   state TEXT NOT NULL CHECK (
		     state IN ('queued', 'running', 'completed', 'failed', 'unknown')
		   ),
		   created_at INTEGER NOT NULL,
		   started_at INTEGER,
		   finished_at INTEGER,
		   result_json BLOB,
		   error TEXT,
		   write_state TEXT NOT NULL DEFAULT 'none' CHECK (write_state IN ('none', 'possible')),
		   http_status INTEGER
		 )`,
		`CREATE TABLE IF NOT EXISTS apply_operation_devices (
		   operation_id TEXT NOT NULL REFERENCES apply_operations(operation_id) ON DELETE CASCADE,
		   ordinal INTEGER NOT NULL,
		   device_id INTEGER NOT NULL,
		   device_mac TEXT NOT NULL,
		   device_name TEXT NOT NULL,
		   state TEXT NOT NULL CHECK (
		     state IN ('queued', 'applying', 'completed', 'failed', 'unknown', 'skipped')
		   ),
		   write_state TEXT NOT NULL CHECK (write_state IN ('none', 'possible')),
		   router_outcome TEXT,
		   outcome TEXT,
		   changes INTEGER NOT NULL DEFAULT 0,
		   reason TEXT,
		   started_at INTEGER,
		   finished_at INTEGER,
		   PRIMARY KEY (operation_id, ordinal)
		 )`,
	},
	15: {
		// Semantic compatibility boundary for the cross-feature policy model.
		// fw_rules and the client policy columns already exist, but v14 ignores
		// them while rendering. A v14 process must therefore refuse a database
		// whose firewall/NAT/route/client intent it would silently weaken.
	},
	16: {
		// Phase 4 event provenance. The source tuple is the identity exposed by
		// the upstream producer (for OpenWrt logd, source_boot includes the boot
		// id and local logd generation and source_id is the u32 log id). Keeping
		// it separate from the controller's INTEGER PRIMARY KEY makes replay
		// idempotent without pretending a producer cursor is globally unique.
		`ALTER TABLE events ADD COLUMN source TEXT NOT NULL DEFAULT 'controller'`,
		`ALTER TABLE events ADD COLUMN source_id TEXT`,
		`ALTER TABLE events ADD COLUMN source_boot TEXT`,
		`ALTER TABLE events ADD COLUMN ingested_at INTEGER`,
		`ALTER TABLE events ADD COLUMN client_mac TEXT`,
		`ALTER TABLE events ADD COLUMN action TEXT`,
		`ALTER TABLE events ADD COLUMN direction TEXT`,
		`ALTER TABLE events ADD COLUMN in_iface TEXT`,
		`ALTER TABLE events ADD COLUMN out_iface TEXT`,
		`ALTER TABLE events ADD COLUMN src_ip TEXT`,
		`ALTER TABLE events ADD COLUMN dst_ip TEXT`,
		`ALTER TABLE events ADD COLUMN src_port INTEGER`,
		`ALTER TABLE events ADD COLUMN dst_port INTEGER`,
		`ALTER TABLE events ADD COLUMN zone_in TEXT`,
		`ALTER TABLE events ADD COLUMN zone_out TEXT`,
		`ALTER TABLE events ADD COLUMN policy_id INTEGER`,
		`UPDATE events SET ingested_at = ts * 1000 WHERE ingested_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS events_source_identity
		   ON events(device_id, source, source_boot, source_id)
		   WHERE source_id IS NOT NULL AND trim(source_id) <> ''`,
		`CREATE INDEX IF NOT EXISTS events_client_time
		   ON events(client_mac, ts, id)`,
		`CREATE INDEX IF NOT EXISTS events_category_time
		   ON events(category, ts, id)`,
		`CREATE INDEX IF NOT EXISTS events_severity_time
		   ON events(severity, ts, id)`,

		// A cursor is scoped to one device and producer. source_boot is kept in
		// boot_id because logd ids wrap/reset and a cursor from a previous boot or
		// daemon generation cannot order the new stream.
		`CREATE TABLE IF NOT EXISTS ingest_cursors (
		   device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		   source TEXT NOT NULL,
		   boot_id TEXT NOT NULL,
		   cursor TEXT NOT NULL,
		   updated_at INTEGER NOT NULL,
		   continuity_gap_at INTEGER NOT NULL DEFAULT 0,
		   PRIMARY KEY (device_id, source)
		 ) WITHOUT ROWID`,

		// Topology is interval history, not a periodically overwritten diagram.
		// child_node/parent_node are controller-stable refs (device:<inventory-mac>,
		// client:<mac>, mac:<mac>); observed MAC aliases stay evidence rather
		// than becoming a second identity for one managed device.
		`CREATE TABLE IF NOT EXISTS topology_edges (
		   id INTEGER PRIMARY KEY,
		   child_node TEXT NOT NULL,
		   child_mac TEXT,
		   parent_node TEXT NOT NULL,
		   parent_device_id INTEGER REFERENCES devices(id) ON DELETE SET NULL,
		   parent_port TEXT,
		   medium TEXT NOT NULL,
		   confidence TEXT NOT NULL,
		   valid_from INTEGER NOT NULL,
		   valid_to INTEGER,
		   last_seen INTEGER NOT NULL,
		   evidence_json TEXT NOT NULL DEFAULT '[]',
		   ambiguity_json TEXT NOT NULL DEFAULT '[]',
		   CHECK (valid_to IS NULL OR valid_to >= valid_from),
		   CHECK (last_seen >= valid_from),
		   CHECK (valid_to IS NULL OR last_seen <= valid_to)
		 )`,
		`CREATE INDEX IF NOT EXISTS topology_edges_active
		   ON topology_edges(child_node, valid_to, last_seen)`,
		`CREATE INDEX IF NOT EXISTS topology_edges_replay
		   ON topology_edges(valid_from, valid_to)`,
		`CREATE TABLE IF NOT EXISTS topology_source_states (
		   device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		   source TEXT NOT NULL,
		   state TEXT NOT NULL CHECK (state IN ('unknown','empty','observed','error')),
		   reason TEXT NOT NULL DEFAULT '',
		   observed_at INTEGER NOT NULL,
		   PRIMARY KEY (device_id, source)
		 ) WITHOUT ROWID`,

		// An explicit RF scan targets exactly one configured UCI wifi-device.
		// radio_key is that LuCI/UCI section (for example radio0), never a
		// runtime phy or interface name which netifd may recreate or rename.
		`CREATE TABLE IF NOT EXISTS radio_scans (
		   id INTEGER PRIMARY KEY,
		   device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		   radio_key TEXT NOT NULL,
		   started_at INTEGER NOT NULL,
		   finished_at INTEGER,
		   status TEXT NOT NULL CHECK (status IN ('pending','running','completed','failed')),
		   detail_json TEXT NOT NULL DEFAULT '{}',
		   CHECK (finished_at IS NULL OR finished_at >= started_at)
		 )`,
		`CREATE INDEX IF NOT EXISTS radio_scans_radio_time
		   ON radio_scans(device_id, radio_key, started_at, id)`,
		`CREATE TABLE IF NOT EXISTS radio_scan_bss (
		   scan_id INTEGER NOT NULL REFERENCES radio_scans(id) ON DELETE CASCADE,
		   bssid TEXT NOT NULL,
		   ssid TEXT NOT NULL,
		   mhz INTEGER NOT NULL,
		   channel INTEGER NOT NULL,
		   signal INTEGER,
		   width INTEGER,
		   PRIMARY KEY (scan_id, bssid, mhz)
		 ) WITHOUT ROWID`,
	},
}

// DB is the controller's database handle.
type DB struct {
	sql       *sql.DB
	protector SecretProtector
	// siteMu makes desired-state mutations whose validation spans multiple SQL
	// statements atomic with respect to one another. Without it, a zone policy
	// save and network rename could each validate the old state and both commit,
	// leaving an orphan policy the effective API cannot expose.
	siteMu sync.Mutex
}

// Open opens (creating if needed) the database at path and migrates it.
//
// driverName is passed in rather than imported so this package does not force a
// SQLite driver on its dependents or on tests. Production wires
// "sqlite" (modernc.org/sqlite — pure Go, per decision D3, which is what lets
// the container be FROM scratch with CGO_ENABLED=0).
func Open(ctx context.Context, driverName, path string, protector SecretProtector) (*DB, error) {
	if protector == nil {
		return nil, errors.New("store: a secret protector is required")
	}
	existing, err := existingDatabase(path)
	if err != nil {
		return nil, err
	}
	current := 0
	if existing {
		probe, err := openReadOnlySQL(ctx, driverName, path)
		if err != nil {
			return nil, err
		}
		current, err = currentSchema(ctx, probe)
		if err == nil && current > schemaVersion {
			err = fmt.Errorf("store: database is at schema v%d but this build understands v%d — refusing to downgrade",
				current, schemaVersion)
		}
		if err == nil {
			probeDB := &DB{sql: probe, protector: protector}
			if current >= secretSchemaVersion {
				_, err = probeDB.verifySecretState(ctx)
			} else {
				err = probeDB.verifyLegacyKeyring(ctx)
			}
		}
		closeErr := probe.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("store: close preflight database: %w", closeErr)
		}
	}
	// Applied for any driver, not only the one named "sqlite".
	//
	// Open takes driverName so this package does not force a SQLite driver on
	// its dependents, and gating the pragmas on that exact string quietly
	// undid the guarantee for every other one — a fork registering the same
	// driver under another name would run with foreign keys OFF and every
	// ON DELETE CASCADE in the schema inert. The parameters are inert
	// themselves on a driver that does not understand them.
	dsn, err := dsnWithPragmas(path)
	if err != nil {
		return nil, err
	}
	sqldb, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite is single-writer. More than one connection buys contention and
	// SQLITE_BUSY, not throughput.
	sqldb.SetMaxOpenConns(1)

	db := &DB{sql: sqldb, protector: protector}
	if err := db.migrate(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	// journal_mode persists in the database header. Set it only after the
	// locked migration path has revalidated key identity and finished any
	// plaintext scrub, so a swapped/restored wrong database is not changed by a
	// connection that will then refuse it.
	if err := db.pragmas(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	return db, nil
}

// OpenReadOnly opens an existing, current-schema SQLite database without
// enabling WAL or running migrations. mode=ro protects the file at the VFS
// boundary; query_only independently rejects writes issued through SQL.
//
// The file URI is built rather than concatenated so spaces, '#', and literal
// percent signs in a valid controller data path keep naming the same file.
func OpenReadOnly(ctx context.Context, driverName, path string, protector SecretProtector) (*DB, error) {
	if protector == nil {
		return nil, errors.New("store: a secret protector is required")
	}
	sqldb, err := openReadOnlySQL(ctx, driverName, path)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*DB, error) {
		sqldb.Close()
		return nil, err
	}
	current, err := currentSchema(ctx, sqldb)
	if err != nil {
		return closeOnError(err)
	}
	if current != schemaVersion {
		return closeOnError(fmt.Errorf("store: database is at schema v%d; read-only tools require v%d (start the controller to migrate it)",
			current, schemaVersion))
	}
	if schemaVersion == 16 {
		if err := verifySchemaV16(ctx, sqldb); err != nil {
			return closeOnError(err)
		}
	}
	db := &DB{sql: sqldb, protector: protector}
	complete, err := db.verifySecretState(ctx)
	if err != nil {
		return closeOnError(err)
	}
	if !complete {
		return closeOnError(errors.New("store: schema v14 secret scrub is incomplete; start the controller writable to finish it"))
	}
	return db, nil
}

func openReadOnlySQL(ctx context.Context, driverName, path string) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	u := &url.URL{Scheme: "file", Path: absPath}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "1")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()

	sqldb, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open %s read-only: %w", path, err)
	}
	sqldb.SetMaxOpenConns(1)
	closeOnError := func(err error) (*sql.DB, error) {
		sqldb.Close()
		return nil, err
	}
	if err := sqldb.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("store: open %s read-only: %w", path, err))
	}
	return sqldb, nil
}

type schemaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func currentSchema(ctx context.Context, sqldb schemaQuerier) (int, error) {
	var current int
	if err := sqldb.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return 0, fmt.Errorf("store: read schema_version: %w", err)
	}
	return current, nil
}

func existingDatabase(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("store: database path %s is not a regular file", path)
	}
	return info.Size() > 0, nil
}

// dsnWithPragmas puts the CONNECTION-scoped settings in the DSN, so every
// connection gets them.
//
// They used to be applied with ExecContext after opening, which is a trap.
// foreign_keys, busy_timeout, synchronous and wal_autocheckpoint are per
// CONNECTION in SQLite, and an Exec through database/sql runs on whichever
// pooled connection serves it. SetMaxOpenConns(1) keeps that to one connection
// in the ordinary case — but database/sql discards a connection on a driver
// error and silently opens a fresh one, and that replacement gets the SQLite
// defaults: foreign_keys OFF and busy_timeout 0.
//
// The consequences are quiet and bad. Every ON DELETE CASCADE in the schema
// stops firing, so removing a device leaves its rows behind — which is how
// owned_sections came to hold claims for devices that no longer exist. And a
// busy_timeout of 0 turns a moment's write contention into an immediate
// SQLITE_BUSY instead of a short wait.
//
// Measured with modernc.org/sqlite: a bare path reports foreign_keys=0
// busy_timeout=0, and the same path with these parameters appended reports 1
// and 5000.
//
// journal_mode is deliberately NOT here: WAL is a property of the database
// file, not of a connection, so it is set once and persists.
func dsnWithPragmas(path string) (string, error) {
	// A "?" in the path cannot be expressed at all.
	//
	// Without a "file:" prefix the driver splits the DSN at the first "?"
	// before decoding anything, so a data directory containing one opens a
	// DIFFERENT file — silently, with the schema migrated into it and the
	// controller reporting zero devices while the real database sits beside it.
	// Percent-escaping does not help: there is no decoding step to undo it.
	//
	// And "file:" is not the answer either — that turns the whole path into a
	// URI, which truncates at "#" and percent-decodes "%HH". So the honest
	// answer is to refuse a path this cannot represent rather than open
	// something else and say nothing.
	if strings.Contains(path, "?") {
		return "", fmt.Errorf("store: the database path %q contains a %q, "+
			"which cannot be passed to the driver without opening a different "+
			"file; rename the data directory", path, "?")
	}
	if strings.Contains(path, "_pragma=") {
		return path, nil // caller has already said what it wants
	}
	// wal_autocheckpoint is deliberately large: the controller writes one
	// transaction per 5-minute maintenance tick, and checkpointing more eagerly
	// buys nothing.
	return path + "?" + strings.Join([]string{
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		"_pragma=synchronous(1)",
		"_pragma=wal_autocheckpoint(200)",
	}, "&"), nil
}

// pragmas applies the settings that belong to the DATABASE rather than to a
// connection. Only journal_mode qualifies: WAL is recorded in the file itself
// and survives every later connection. Everything else moved to the DSN — see
// dsnWithPragmas for why applying them here was wrong.
func (db *DB) pragmas(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("store: PRAGMA journal_mode=WAL: %w", err)
	}
	return nil
}

func (db *DB) migrate(ctx context.Context) error {
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("store: acquire migration lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	current := 0
	var hasVersionTable int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).
		Scan(&hasVersionTable); err != nil {
		return fmt.Errorf("store: inspect schema before migration: %w", err)
	}
	if hasVersionTable != 0 {
		current, err = currentSchema(ctx, conn)
		if err != nil {
			return err
		}
		if current > schemaVersion {
			return fmt.Errorf("store: database is at schema v%d but this build "+
				"understands v%d — refusing to downgrade", current, schemaVersion)
		}
		if current >= secretSchemaVersion {
			complete, err := db.verifySecretStateOn(ctx, conn)
			if err != nil {
				return err
			}
			if current == schemaVersion {
				if schemaVersion == 16 {
					if err := verifySchemaV16(ctx, conn); err != nil {
						return err
					}
				}
				if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
					return fmt.Errorf("store: commit migration check: %w", err)
				}
				committed = true
				if err := conn.Close(); err != nil {
					return fmt.Errorf("store: release migration connection: %w", err)
				}
				return db.finishSecretScrub(ctx)
			}
			if !complete {
				return fmt.Errorf("store: schema v%d secret scrub is incomplete; finish it with a v%d controller before applying later migrations",
					secretSchemaVersion, secretSchemaVersion)
			}
		}
		if current < secretSchemaVersion {
			var hasSecretState, secretStateRows int
			if err := conn.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='secret_state'`).
				Scan(&hasSecretState); err != nil {
				return fmt.Errorf("store: inspect legacy secret state: %w", err)
			}
			if hasSecretState != 0 {
				if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM secret_state`).
					Scan(&secretStateRows); err != nil {
					return fmt.Errorf("store: inspect legacy secret marker: %w", err)
				}
				if secretStateRows != 0 {
					return fmt.Errorf("store: schema_version is v%d but schema v14 secret state already exists; refusing to re-run the one-time secret migration",
						current)
				}
			}
			// The read-only preflight protected the path as it existed then. Re-check
			// under the write lock before even idempotent DDL, so a replaced/restored
			// v13 file cannot be touched with a protector validated against another DB.
			if err := db.verifyLegacyKeyringOn(ctx, conn); err != nil {
				return err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	legacyTarget := secretSchemaVersion - 1
	if schemaVersion < legacyTarget {
		legacyTarget = schemaVersion
	}
	for v := current + 1; v <= legacyTarget; v++ {
		for _, stmt := range migrations[v] {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				// A fresh database gets its columns from schema.sql, so the
				// ALTER is a duplicate there rather than a failure. Anything
				// else is real.
				if !strings.Contains(err.Error(), "duplicate column name") {
					return fmt.Errorf("store: migration to v%d (%s): %w", v, stmt, err)
				}
			}
		}
	}
	if current < legacyTarget {
		current = legacyTarget
	}
	if current < secretSchemaVersion && schemaVersion >= secretSchemaVersion {
		if err := db.migrateSecretsV14Locked(ctx, conn); err != nil {
			return err
		}
		current = secretSchemaVersion
	}
	for v := current + 1; v <= schemaVersion; v++ {
		for _, stmt := range migrations[v] {
			if _, err := conn.ExecContext(ctx, stmt); err != nil &&
				!strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("store: migration to v%d (%s): %w", v, stmt, err)
			}
		}
	}
	if schemaVersion == 16 {
		if err := verifySchemaV16(ctx, conn); err != nil {
			return err
		}
	}
	if current < schemaVersion {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
			schemaVersion, time.Now().Unix()); err != nil {
			return fmt.Errorf("store: record schema v%d: %w", schemaVersion, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("store: commit schema migration: %w", err)
	}
	committed = true
	if err := conn.Close(); err != nil {
		return fmt.Errorf("store: release migration connection: %w", err)
	}
	return db.finishSecretScrub(ctx)
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
	// Two processes can reach the physical v14 scrub immediately after SQLite
	// serialises their schema transactions. In that case one checkpoint reports
	// busy with frame counts of -1 while the other checkpoint holds SQLite's
	// checkpoint lock. That is transient, unlike a reader which keeps an old WAL
	// snapshot pinned. Give the former a small bounded handoff window; the latter
	// must still fail so scrub_complete is never advanced over plaintext pages
	// which could not be folded out of the WAL.
	deadline := time.Now().Add(time.Second)
	for {
		// wal_checkpoint reports lock contention as a result row, not necessarily
		// as an SQL error. Exec discards that row and can therefore report success
		// after doing no checkpoint at all.
		var busy, logFrames, checkpointed int
		if err := db.sql.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
			Scan(&busy, &logFrames, &checkpointed); err != nil {
			return fmt.Errorf("store: checkpoint WAL: %w", err)
		}
		if busy == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("store: checkpoint WAL remained busy after the retry window; an active reader or concurrent checkpoint still holds it (busy=%d, log_frames=%d, checkpointed=%d)",
				busy, logFrames, checkpointed)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("store: checkpoint WAL: %w", ctx.Err())
		case <-timer.C:
		}
	}
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
	ID     int64
	MAC    string
	Host   string
	Port   int
	Scheme string
	CertFP string
	// HostKeyFP pins the SSH host key of the bootstrap channel. Empty means
	// unpinned — either the device predates the pin, or nothing has ever
	// opened SSH to it. Only adoption and un-adoption use that channel, so an
	// empty value costs nothing in steady state and everything at un-adopt,
	// which carries the operator's administrator password.
	HostKeyFP string
	Name      string
	Role      string
	// Functions is the independent responsibility set. Role is retained as a
	// deterministic primary label for older clients.
	Functions []string
	// FunctionError is non-empty only when functions_json could not be decoded
	// or validated. Such a row remains visible but is not renderable.
	FunctionError string
	AdoptedAt     *int64
	CredEnc       []byte
	Class         string
	CapsJSON      string
	FWRelease     string
	LastSeen      *int64
	PollState     string
	// PollInterval is a per-device baseline interval in seconds; 0 uses the
	// controller default. Only ever loosens the rate — see migration 4.
	PollInterval int
}

// Adopted reports whether adoption completed.
func (d Device) Adopted() bool { return d.AdoptedAt != nil }

// ModelDevice is the renderer's view of this inventory row. Centralising the
// conversion prevents a caller from consulting primary Role and accidentally
// widening a gateway-only device back into a legacy gateway+AP+switch.
func (d Device) ModelDevice() model.Device {
	functions := model.DeviceFunctionsOf(d.Functions, d.Role)
	role := functions.PrimaryRole()
	if d.Functions != nil && len(functions) == 0 {
		role = model.RoleOf(d.Role)
	}
	return model.Device{
		ID: d.ID, Name: d.Name, Role: role,
		Functions: functions,
	}
}

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
	role, err := model.ParseRole(d.Role)
	if err != nil {
		return fmt.Errorf("store: device role: %w", err)
	}
	functions, err := model.ParseDeviceFunctions(d.Functions, role)
	if err != nil {
		return fmt.Errorf("store: device functions: %w", err)
	}
	d.Functions = functions.Strings()
	d.Role = string(functions.PrimaryRole())
	functionsJSON, err := json.Marshal(d.Functions)
	if err != nil {
		return fmt.Errorf("store: encode device functions: %w", err)
	}
	if d.PollState == "" {
		d.PollState = "baseline"
	}
	if d.CapsJSON == "" {
		d.CapsJSON = "{}"
	}
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO devices (mac, host, port, scheme, cert_fp, host_key_fp, name, role, functions_json,
                     adopted_at, cred_enc, class, caps_json, fw_release,
                     last_seen, poll_state)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(mac) DO UPDATE SET
  host=excluded.host, port=excluded.port, scheme=excluded.scheme,
  cert_fp=excluded.cert_fp, name=excluded.name, role=excluded.role,
  functions_json=excluded.functions_json,
  adopted_at=excluded.adopted_at, cred_enc=excluded.cred_enc,
  class=excluded.class, caps_json=excluded.caps_json,
  fw_release=excluded.fw_release, last_seen=excluded.last_seen,
  poll_state=excluded.poll_state,
  -- The pin is set on INSERT and never replaced by an upsert. COALESCE on the
  -- stored value, not on the incoming one, so a caller that does not carry the
  -- field cannot blank it and a caller that carries a DIFFERENT one cannot
  -- quietly re-pin: the only way past an existing pin is SetHostKeyFP, which
  -- refuses. cert_fp deliberately keeps the older take-the-new-value rule —
  -- it is re-derived on every https connection and a certificate legitimately
  -- rotates, whereas a host key changing means the box was reflashed.
  host_key_fp=COALESCE(devices.host_key_fp, excluded.host_key_fp)`,
		d.MAC, d.Host, d.Port, d.Scheme, nullString(d.CertFP),
		nullString(d.HostKeyFP), d.Name, d.Role, string(functionsJSON),
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
 COALESCE(host_key_fp,''), name, role, adopted_at, cred_enc,
 COALESCE(class,''), caps_json, COALESCE(fw_release,''), last_seen, poll_state,
 COALESCE(poll_interval_s,0), COALESCE(functions_json,'') FROM devices`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(s scanner) (*Device, error) {
	var d Device
	var functionsJSON string
	err := s.Scan(&d.ID, &d.MAC, &d.Host, &d.Port, &d.Scheme, &d.CertFP,
		&d.HostKeyFP, &d.Name, &d.Role, &d.AdoptedAt, &d.CredEnc, &d.Class,
		&d.CapsJSON, &d.FWRelease, &d.LastSeen, &d.PollState, &d.PollInterval,
		&functionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var stored []string
	decodeErr := json.Unmarshal([]byte(functionsJSON), &stored)
	functions, validationErr := model.ParseDeviceFunctions(stored, model.RoleOf(d.Role))
	if decodeErr != nil || stored == nil || validationErr != nil {
		d.Functions = []string{}
		d.FunctionError = "stored device functions are invalid; restore the controller database from a known-good backup or re-adopt this device"
		d.Role = string(model.RoleOf(d.Role))
	} else {
		d.Functions = functions.Strings()
		d.Role = string(functions.PrimaryRole())
	}
	return &d, nil
}

// SetPollInterval sets a per-device baseline interval in seconds. Zero restores
// the controller default.
//
// The collector separately refuses to let an override make polling FASTER than
// the default; this only stores what was asked for. Both halves matter: the
// store should record the operator's intent, and the collector should be the
// one place that decides what is actually done to a device.
func (db *DB) SetPollInterval(ctx context.Context, id int64, seconds int) error {
	if seconds < 0 {
		return fmt.Errorf("store: poll interval cannot be negative")
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET poll_interval_s=? WHERE id=?`, seconds, id)
	if err != nil {
		return fmt.Errorf("store: set poll interval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetName renames a device.
//
// A narrow write like SetPollInterval rather than a full UpsertDevice: the
// latter rewrites the sealed credential and the capability record, and a rename
// has no business touching either.
//
// The name is display only. Nothing keys on it — the MAC is the identity, and
// the id is what group membership and telemetry reference — so this cannot
// orphan anything. Rejecting the empty string is the whole validation: the
// column is NOT NULL, and a device with a blank name renders as an empty cell
// nobody can click.
func (db *DB) SetName(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("store: a device name cannot be empty")
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE devices SET name=? WHERE id=?`, name, id)
	if err != nil {
		return fmt.Errorf("store: rename device %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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

// SetHostKeyFP records a trust-on-first-use SSH host key pin.
//
// Same rule as SetCertFP and for a stronger reason. A device's certificate is
// re-derived on every https connection and can legitimately rotate; its SSH
// host key is generated once at first boot and changes only if the box was
// reflashed or is not the box we think it is. So the second key must be
// refused, and re-pinning has to be a deliberate act by someone who knows which
// of those two happened.
//
// Adoption does not call this — it inserts the pin with the row, which is
// genuinely first use. This exists for the devices adopted before the column
// did: their first SSH after the migration is unpinned by necessity, and this
// is what makes the second one pinned.
func (db *DB) SetHostKeyFP(ctx context.Context, deviceID int64, fp string) error {
	if fp == "" {
		return errors.New("store: refusing to pin an empty SSH host key")
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET host_key_fp=? WHERE id=? AND (host_key_fp IS NULL OR host_key_fp='')`,
		fp, deviceID)
	if err != nil {
		return fmt.Errorf("store: pin SSH host key for device %d: %w", deviceID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: device %d already has a pinned SSH host key; "+
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
INSERT INTO owned_sections (device_id, config, section, rendered_hash, rendered_hash_enc, applied_at)
VALUES (?,?,?,'',?,?)
ON CONFLICT(device_id, config, section) DO UPDATE SET
  rendered_hash='', rendered_hash_enc=excluded.rendered_hash_enc, applied_at=excluded.applied_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, s := range secs {
		if s.AppliedAt == 0 {
			s.AppliedAt = time.Now().Unix()
		}
		sealed, err := db.sealText(s.RenderedHash,
			ownedHashAAD(s.DeviceID, s.Config, s.Section))
		if err != nil {
			return fmt.Errorf("store: seal ownership verifier for device %d %s.%s: %w",
				s.DeviceID, s.Config, s.Section, err)
		}
		if _, err := stmt.ExecContext(ctx, s.DeviceID, s.Config, s.Section,
			nullableBlob(sealed), s.AppliedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OwnedSections returns everything we believe we own on a device.
func (db *DB) OwnedSections(ctx context.Context, deviceID int64) ([]OwnedSection, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT device_id, config, section, rendered_hash_enc, applied_at
		 FROM owned_sections WHERE device_id=? ORDER BY config, section`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnedSection
	for rows.Next() {
		var s OwnedSection
		var sealed []byte
		if err := rows.Scan(&s.DeviceID, &s.Config, &s.Section,
			&sealed, &s.AppliedAt); err != nil {
			return nil, err
		}
		s.RenderedHash, err = db.openText(sealed,
			ownedHashAAD(s.DeviceID, s.Config, s.Section),
			fmt.Sprintf("ownership verifier for device %d %s.%s", s.DeviceID, s.Config, s.Section))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReplaceOwned makes the recorded claims for a device exactly this set.
//
// Replace rather than merge, because an apply PRUNES. Doc.Prune removes every
// section carrying our marker that the render no longer produces, so after a
// confirmed apply the device holds exactly the rendered set — and recording
// only the additions left a claim behind for every section ever pruned.
//
// Observed on the lab C6: it claimed oowrt_mesh1_radio0 and oowrt_up1_radio0
// months after the mesh and the uplink were deleted from the site model and
// removed from the device. Harmless to the apply path, and not harmless to the
// operator: the un-adopt panel lists what it is about to revert, and listing
// sections that are not there is exactly the kind of wrong detail that makes
// someone doubt the rest of the screen.
//
// One transaction, so a failure cannot leave the record half-updated — which
// would be worse than either state.
func (db *DB) ReplaceOwned(ctx context.Context, deviceID int64, secs []OwnedSection) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace ownership claims: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM owned_sections WHERE device_id=?`, deviceID); err != nil {
		return fmt.Errorf("store: clear ownership claims: %w", err)
	}
	for _, s := range secs {
		sealed, err := db.sealText(s.RenderedHash,
			ownedHashAAD(deviceID, s.Config, s.Section))
		if err != nil {
			return fmt.Errorf("store: seal ownership verifier for device %d %s.%s: %w",
				deviceID, s.Config, s.Section, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO owned_sections (device_id, config, section, rendered_hash, rendered_hash_enc, applied_at)
VALUES (?, ?, ?, '', ?, ?)`,
			deviceID, s.Config, s.Section, nullableBlob(sealed), s.AppliedAt); err != nil {
			return fmt.Errorf("store: record ownership claim: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit ownership claims: %w", err)
	}
	return nil
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
	ID         int64
	TS         int64
	DeviceID   *int64
	Category   string // client|device|security|system|audit
	Severity   string // info|warning|error
	Event      string
	Detail     any
	Source     string
	SourceID   string
	SourceBoot string
	IngestedAt int64 // Unix milliseconds; TS is the legacy Unix-seconds event time.
	ClientMAC  string
	Action     string
	Direction  string
	InIface    string
	OutIface   string
	SrcIP      string
	DstIP      string
	SrcPort    *int
	DstPort    *int
	ZoneIn     string
	ZoneOut    string
	PolicyID   *int64
}

// LogEvent appends to the event log. Every apply outcome lands here, including
// the Unknown one — especially that one.
func (db *DB) LogEvent(ctx context.Context, e Event) error {
	_, err := db.AppendEvent(ctx, e)
	return err
}

// AppendEvent appends an event and reports whether it was new. Events carrying
// a producer source_id are idempotent within (device, source, source_boot), so
// reconnect/replay can safely submit the overlap around a durable cursor.
func (db *DB) AppendEvent(ctx context.Context, e Event) (bool, error) {
	e, detail, err := normalizeEvent(e)
	if err != nil {
		return false, err
	}
	res, err := db.sql.ExecContext(ctx, appendEventSQL, eventInsertArgs(e, detail)...)
	if err != nil {
		return false, err
	}
	return eventInsertResult(res)
}

const appendEventSQL = `
INSERT INTO events (
  ts, device_id, category, severity, event, detail_json,
  source, source_id, source_boot, ingested_at, client_mac, action, direction,
  in_iface, out_iface, src_ip, dst_ip, src_port, dst_port, zone_in, zone_out, policy_id
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT DO NOTHING`

func normalizeEvent(e Event) (Event, string, error) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	if e.Source == "" {
		e.Source = "controller"
	}
	if strings.TrimSpace(e.Source) != e.Source || e.Source == "" {
		return e, "", errors.New("store: event source must be a non-blank identifier")
	}
	if e.DeviceID != nil && *e.DeviceID <= 0 {
		return e, "", errors.New("store: event device id must be positive")
	}
	if e.SourceID != "" && (e.DeviceID == nil || strings.TrimSpace(e.SourceBoot) == "") {
		return e, "", errors.New("store: sourced event identity requires a device and source boot")
	}
	if e.ClientMAC != "" {
		mac, err := canonicalMAC(e.ClientMAC)
		if err != nil {
			return e, "", fmt.Errorf("store: event client MAC: %w", err)
		}
		e.ClientMAC = mac
	}
	for _, port := range []*int{e.SrcPort, e.DstPort} {
		if port != nil && (*port < 0 || *port > 65535) {
			return e, "", errors.New("store: event port is outside 0..65535")
		}
	}
	if e.IngestedAt == 0 {
		e.IngestedAt = time.Now().UnixMilli()
	}
	if e.IngestedAt < 0 {
		return e, "", errors.New("store: event ingest time cannot be negative")
	}
	detail := "{}"
	if e.Detail != nil {
		blob, err := json.Marshal(e.Detail)
		if err != nil {
			return e, "", err
		}
		detail = string(blob)
	}
	return e, detail, nil
}

func eventInsertArgs(e Event, detail string) []any {
	return []any{
		e.TS, e.DeviceID, e.Category, e.Severity, e.Event, detail,
		e.Source, nullString(e.SourceID), nullString(e.SourceBoot), e.IngestedAt,
		nullString(e.ClientMAC), nullString(e.Action), nullString(e.Direction),
		nullString(e.InIface), nullString(e.OutIface), nullString(e.SrcIP), nullString(e.DstIP),
		e.SrcPort, e.DstPort, nullString(e.ZoneIn), nullString(e.ZoneOut), e.PolicyID,
	}
}

func eventInsertResult(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: inspect event insert: %w", err)
	}
	return n != 0, nil
}

// AppendEventsAndCursor persists one producer page and its next cursor in one
// SQLite transaction. Replay overlap is ignored by producer identity, and the
// cursor never advances if validation or insertion of any event fails.
func (db *DB) AppendEventsAndCursor(ctx context.Context, events []Event,
	cursor IngestCursor) (inserted int, err error) {
	if len(events) > 512 {
		return 0, errors.New("store: an ingest batch cannot exceed 512 events")
	}
	cursor, err = normalizeIngestCursor(cursor)
	if err != nil {
		return 0, err
	}
	type preparedEvent struct {
		event  Event
		detail string
	}
	prepared := make([]preparedEvent, len(events))
	for i, event := range events {
		event, detail, err := normalizeEvent(event)
		if err != nil {
			return 0, fmt.Errorf("store: ingest event %d: %w", i, err)
		}
		if event.SourceID != "" &&
			(*event.DeviceID != cursor.DeviceID || event.Source != cursor.Source ||
				event.SourceBoot != cursor.BootID) {
			return 0, fmt.Errorf("store: ingest event %d identity does not match its cursor", i)
		}
		prepared[i] = preparedEvent{event: event, detail: detail}
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.PrepareContext(ctx, appendEventSQL)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for i, event := range prepared {
		res, err := stmt.ExecContext(ctx, eventInsertArgs(event.event, event.detail)...)
		if err != nil {
			return 0, fmt.Errorf("store: ingest event %d: %w", i, err)
		}
		added, err := eventInsertResult(res)
		if err != nil {
			return 0, err
		}
		if added {
			inserted++
		}
	}
	if err := saveIngestCursorOn(ctx, tx, cursor); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// RecentEvents returns the newest events first.
func (db *DB) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	return db.QueryEvents(ctx, "", "", limit)
}

// QueryEvents returns the newest events matching the filters.
//
// The filters are in the SQL, not applied to the result afterwards. Filtering a
// page that the database already truncated selects from the newest N events
// overall rather than the newest N matching ones — so a screen filtered to
// "error" can show nothing while errors exist, simply because a hundred routine
// events arrived after them.
func (db *DB) QueryEvents(ctx context.Context, category, severity string, limit int) ([]Event, error) {
	return db.QueryEventsPage(ctx, category, severity, limit, 0)
}

// DeviceEvents returns the newest events of one kind for one device.
//
// Narrow on purpose. The general query pages the whole log by category and
// severity, which is the wrong shape for "what most recently happened to THIS
// device" — that would page through everything else to find it, and on a busy
// controller would not find it at all.
func (db *DB) DeviceEvents(ctx context.Context, deviceID int64, event string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+eventColumns+`
		   FROM events
		  WHERE device_id = ? AND event = ?
		  ORDER BY ts DESC, id DESC LIMIT ?`, deviceID, event, limit)
	if err != nil {
		return nil, fmt.Errorf("store: device events: %w", err)
	}
	return scanEvents(rows)
}

// LatestClientAssociationEvents returns the newest durable producer event for
// every client that has an association-state transition. Disconnect rows are
// deliberately returned: the restart seed must suppress a stale earlier
// connect rather than resurrecting an association that has already ended.
func (db *DB) LatestClientAssociationEvents(ctx context.Context) ([]Event, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+eventColumns+`
  FROM events AS current
 WHERE current.client_mac IS NOT NULL AND trim(current.client_mac) <> ''
   AND current.source_id IS NOT NULL AND trim(current.source_id) <> ''
   AND current.action IN ('connect','disconnect','roam')
   AND NOT EXISTS (
     SELECT 1 FROM ingest_cursors AS cursor
      WHERE cursor.source = current.source
        AND cursor.continuity_gap_at > COALESCE(current.ingested_at, current.ts * 1000)
   )
   AND current.id = (
     SELECT candidate.id FROM events AS candidate
      WHERE candidate.client_mac = current.client_mac
        AND candidate.source_id IS NOT NULL AND trim(candidate.source_id) <> ''
        AND candidate.action IN ('connect','disconnect','roam')
      ORDER BY candidate.ts DESC, candidate.id DESC LIMIT 1
   )
 ORDER BY current.client_mac, current.ts DESC, current.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: latest client association events: %w", err)
	}
	return scanEvents(rows)
}

// QueryEventsPage is QueryEvents with an offset, for server-side paging.
func (db *DB) QueryEventsPage(ctx context.Context, category, severity string, limit, offset int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+eventColumns+`
		   FROM events
		  WHERE (? = '' OR category = ?) AND (? = '' OR severity = ?)
		  ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`,
		category, category, severity, severity, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// scanEvents reads the event column list, which two queries share.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var detail string
		var sourceID, sourceBoot, clientMAC, action, direction sql.NullString
		var inIface, outIface, srcIP, dstIP, zoneIn, zoneOut sql.NullString
		var srcPort, dstPort, policyID sql.NullInt64
		if err := rows.Scan(&e.ID, &e.TS, &e.DeviceID, &e.Category, &e.Severity,
			&e.Event, &detail, &e.Source, &sourceID, &sourceBoot, &e.IngestedAt,
			&clientMAC, &action, &direction, &inIface, &outIface, &srcIP, &dstIP,
			&srcPort, &dstPort, &zoneIn, &zoneOut, &policyID); err != nil {
			return nil, err
		}
		e.SourceID, e.SourceBoot = sourceID.String, sourceBoot.String
		e.ClientMAC, e.Action, e.Direction = clientMAC.String, action.String, direction.String
		e.InIface, e.OutIface = inIface.String, outIface.String
		e.SrcIP, e.DstIP = srcIP.String, dstIP.String
		e.ZoneIn, e.ZoneOut = zoneIn.String, zoneOut.String
		if srcPort.Valid {
			v := int(srcPort.Int64)
			e.SrcPort = &v
		}
		if dstPort.Valid {
			v := int(dstPort.Int64)
			e.DstPort = &v
		}
		if policyID.Valid {
			v := policyID.Int64
			e.PolicyID = &v
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

const eventColumns = `id, ts, device_id, category, severity, event, detail_json,
 source, source_id, source_boot, COALESCE(ingested_at, ts * 1000), client_mac, action, direction,
 in_iface, out_iface, src_ip, dst_ip, src_port, dst_port, zone_in, zone_out, policy_id`

// EventCursor is the stable descending keyset identity. Timestamp alone is not
// unique; id resolves ties without skips or duplicates as new rows arrive.
type EventCursor struct {
	TS int64 `json:"ts"` // legacy event timestamp, Unix seconds
	ID int64 `json:"id"`
}

// QueryEventsBefore returns a stable keyset page older than before. A nil
// cursor starts at the newest event. clientMAC is optional and canonicalized.
func (db *DB) QueryEventsBefore(ctx context.Context, category, severity, clientMAC string,
	before *EventCursor, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + eventColumns + ` FROM events
	 WHERE (? = '' OR category = ?)
	   AND (? = '' OR severity = ?)
	   AND (? = '' OR client_mac = ?)`
	args := []any{category, category, severity, severity,
		strings.ToLower(clientMAC), strings.ToLower(clientMAC)}
	if before != nil {
		if before.TS < 0 || before.ID <= 0 {
			return nil, errors.New("store: invalid event cursor")
		}
		query += ` AND (ts < ? OR (ts = ? AND id < ?))`
		args = append(args, before.TS, before.TS, before.ID)
	}
	query += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// IngestCursor is the last producer position durably accepted for one device.
type IngestCursor struct {
	DeviceID        int64
	Source          string
	BootID          string
	Cursor          string
	UpdatedAt       int64 // Unix milliseconds
	ContinuityGapAt int64 // Unix milliseconds; zero means no retained gap
}

// SaveIngestCursor advances or resets a producer cursor. Callers encode local
// producer generation in BootID; the store deliberately does not compare
// opaque cursor formats from different sources.
func (db *DB) SaveIngestCursor(ctx context.Context, c IngestCursor) error {
	c, err := normalizeIngestCursor(c)
	if err != nil {
		return err
	}
	return saveIngestCursorOn(ctx, db.sql, c)
}

func normalizeIngestCursor(c IngestCursor) (IngestCursor, error) {
	if c.DeviceID <= 0 || strings.TrimSpace(c.Source) == "" ||
		strings.TrimSpace(c.BootID) == "" || strings.TrimSpace(c.Cursor) == "" {
		return c, errors.New("store: ingest cursor requires device, source, boot id and cursor")
	}
	if strings.TrimSpace(c.Source) != c.Source {
		return c, errors.New("store: ingest cursor source must not have surrounding whitespace")
	}
	if c.UpdatedAt == 0 {
		c.UpdatedAt = time.Now().UnixMilli()
	}
	if c.UpdatedAt < 0 {
		return c, errors.New("store: ingest cursor update time cannot be negative")
	}
	if c.ContinuityGapAt < 0 {
		return c, errors.New("store: ingest cursor gap time cannot be negative")
	}
	return c, nil
}

type cursorExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func saveIngestCursorOn(ctx context.Context, exec cursorExecer, c IngestCursor) error {
	_, err := exec.ExecContext(ctx, `
INSERT INTO ingest_cursors
       (device_id, source, boot_id, cursor, updated_at, continuity_gap_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(device_id, source) DO UPDATE SET
  boot_id=excluded.boot_id, cursor=excluded.cursor, updated_at=excluded.updated_at,
  continuity_gap_at=MAX(ingest_cursors.continuity_gap_at, excluded.continuity_gap_at)`,
		c.DeviceID, c.Source, c.BootID, c.Cursor, c.UpdatedAt, c.ContinuityGapAt)
	return err
}

// LoadIngestCursor returns ErrNotFound until that producer has committed one.
func (db *DB) LoadIngestCursor(ctx context.Context, deviceID int64, source string) (IngestCursor, error) {
	var c IngestCursor
	err := db.sql.QueryRowContext(ctx, `
SELECT device_id, source, boot_id, cursor, updated_at, continuity_gap_at
  FROM ingest_cursors WHERE device_id=? AND source=?`, deviceID, source).
		Scan(&c.DeviceID, &c.Source, &c.BootID, &c.Cursor, &c.UpdatedAt,
			&c.ContinuityGapAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// IngestCursorsBySource reports producer coverage without reading event rows.
// A cursor can represent an observed-empty page, so absence and empty remain
// distinct at the API boundary.
func (db *DB) IngestCursorsBySource(ctx context.Context, source string) ([]IngestCursor, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(source) != source {
		return nil, errors.New("store: ingest cursor source must be a non-blank identifier")
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT device_id, source, boot_id, cursor, updated_at, continuity_gap_at
  FROM ingest_cursors WHERE source=? ORDER BY device_id`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IngestCursor{}
	for rows.Next() {
		var cursor IngestCursor
		if err := rows.Scan(&cursor.DeviceID, &cursor.Source, &cursor.BootID,
			&cursor.Cursor, &cursor.UpdatedAt, &cursor.ContinuityGapAt); err != nil {
			return nil, err
		}
		out = append(out, cursor)
	}
	return out, rows.Err()
}

// Facet is one filter option and how many rows would match it.
type Facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// EventFacets counts the filter options for the event log, and totals the
// current selection.
//
// The counts are computed by the database over the whole table, not by counting
// the page that was returned. UI-SPEC §5 singles this out, and it is not
// pedantry: the log endpoint returns at most a few hundred of ~13k rows, so a
// count taken from the response says "3 errors" when the table holds three
// hundred, and it says it with the same confidence as the true number.
//
// Each facet is counted with the OTHER filters applied but not its own, which
// is what makes the counts useful rather than decorative. Applying a facet to
// itself would show the selected option's count and zero beside every
// alternative, so the rail could never answer the only question anyone asks of
// it: "how many would I get if I clicked that instead?"
func (db *DB) EventFacets(ctx context.Context, category, severity string) (cats, sevs []Facet, total int, err error) {
	// Severity options: category filter applies, severity filter does not.
	sevs, err = db.facet(ctx, "severity", "category", category)
	if err != nil {
		return nil, nil, 0, err
	}
	// Category options: severity filter applies, category filter does not.
	cats, err = db.facet(ctx, "category", "severity", severity)
	if err != nil {
		return nil, nil, 0, err
	}
	row := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events
		  WHERE (? = '' OR category = ?) AND (? = '' OR severity = ?)`,
		category, category, severity, severity)
	if err := row.Scan(&total); err != nil {
		return nil, nil, 0, err
	}
	return cats, sevs, total, nil
}

// facet groups by one column while filtering on another. Both column names are
// literals from EventFacets, never from a request — this builds SQL by
// concatenation and that is only safe because of it.
func (db *DB) facet(ctx context.Context, groupCol, filterCol, filterVal string) ([]Facet, error) {
	switch groupCol {
	case "category", "severity":
	default:
		return nil, fmt.Errorf("store: %q is not a facetable column", groupCol)
	}
	switch filterCol {
	case "category", "severity":
	default:
		return nil, fmt.Errorf("store: %q is not a filterable column", filterCol)
	}
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+groupCol+`, COUNT(*) FROM events
		  WHERE (? = '' OR `+filterCol+` = ?)
		  GROUP BY `+groupCol+` ORDER BY COUNT(*) DESC, `+groupCol,
		filterVal, filterVal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Facet{}
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
