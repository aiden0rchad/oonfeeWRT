-- oonfeeWRT controller schema. Authoritative copy lives in
-- docs/IMPLEMENTATION.md §3; this file is what actually runs.
--
-- Forward-only migrations: never edit a shipped statement, add a new migration
-- instead. schema_version records how far we have come.

CREATE TABLE IF NOT EXISTS schema_version (
  version    INTEGER NOT NULL,
  applied_at INTEGER NOT NULL
);

-- ===== operators =====
-- Who may open the UI. Distinct from a device credential in every way that
-- matters: this hash is argon2id (internal/secrets), not the SHA-512 crypt that
-- rpcd's on-device format forces, because nothing here has to run on a router.
CREATE TABLE IF NOT EXISTS admins (
  id         INTEGER PRIMARY KEY,
  username   TEXT NOT NULL UNIQUE,
  pass_hash  TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_login INTEGER
);

-- ===== inventory =====
CREATE TABLE IF NOT EXISTS devices (
  id           INTEGER PRIMARY KEY,
  mac          TEXT NOT NULL UNIQUE,
  host         TEXT NOT NULL,            -- ip or name
  port         INTEGER NOT NULL DEFAULT 80,
  scheme       TEXT NOT NULL DEFAULT 'http',   -- 'http'|'https'
  cert_fp      TEXT,                     -- sha256 of DER, TOFU-pinned
  host_key_fp  TEXT,                     -- SSH host key, TOFU-pinned (migration v9)
  name         TEXT NOT NULL,
  role         TEXT NOT NULL DEFAULT 'ap',     -- 'gateway'|'ap'|'switch'
  functions_json TEXT NOT NULL DEFAULT '["ap","switch"]', -- independent responsibilities (migration v11)
  adopted_at   INTEGER,                  -- unix; NULL = pending
  cred_enc     BLOB,                     -- chacha20poly1305(username:password)
  class        TEXT,                     -- 'A'|'B'|'C' per DEVICE-BUDGET
  caps_json    TEXT NOT NULL DEFAULT '{}',     -- capability registry snapshot
  fw_release   TEXT,
  last_seen    INTEGER,
  poll_state   TEXT NOT NULL DEFAULT 'baseline', -- 'baseline'|'focused'|'quiesced'|'backoff'
  poll_interval_s INTEGER NOT NULL DEFAULT 0     -- per-device baseline; 0 = controller default (migration v4)
);

-- ===== site model (desired state) =====
-- One row, id=1. The uuid seeds the deterministic mobility-domain derivation
-- and must never change once written (migration v5).
CREATE TABLE IF NOT EXISTS site (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  uuid TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT 'Site'
);
CREATE TABLE IF NOT EXISTS networks (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  vlan INTEGER NOT NULL UNIQUE, cidr TEXT NOT NULL,
  zone TEXT NOT NULL DEFAULT 'lan',
  dhcp_json TEXT NOT NULL DEFAULT '{}',
  ipv6_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS ap_groups (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS ap_group_members (
  group_id INTEGER REFERENCES ap_groups(id) ON DELETE CASCADE,
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, device_id)
);
CREATE TABLE IF NOT EXISTS wlans (
  id INTEGER PRIMARY KEY, ssid TEXT NOT NULL,
  network_id INTEGER NOT NULL REFERENCES networks(id),
  group_id INTEGER NOT NULL REFERENCES ap_groups(id),
  bands TEXT NOT NULL DEFAULT '2g,5g',
  security_json TEXT NOT NULL,             -- non-secret mode/PMF only (v14)
  security_key_enc BLOB,                   -- sealed PSK, AAD-bound to wlan id (v14)
  roaming_json TEXT NOT NULL DEFAULT '{}',
  options_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1
);
-- 802.11s mesh backhauls (migration v7). Separate from wlans because a mesh
-- point is a different interface mode, not a WLAN with a flag: a mesh ID rather
-- than an SSID, exactly one band rather than a list, and none of the roaming or
-- isolation options a WLAN carries.
CREATE TABLE IF NOT EXISTS meshes (
  id INTEGER PRIMARY KEY, mesh_id TEXT NOT NULL,
  network_id INTEGER NOT NULL REFERENCES networks(id),
  group_id INTEGER NOT NULL REFERENCES ap_groups(id),
  band TEXT NOT NULL,
  key TEXT NOT NULL DEFAULT '',             -- legacy v7 column; always empty from v14
  key_enc BLOB,                             -- sealed SAE key, AAD-bound to mesh id (v14)
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS zones (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  policy_json TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS fw_rules (
  id INTEGER PRIMARY KEY, sort INTEGER NOT NULL,
  rule_json TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS device_overrides (
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  value_json TEXT NOT NULL,
  PRIMARY KEY (device_id, path)
);

-- ===== reconciliation bookkeeping =====
-- owned_sections is the second-most-important table in the system: it is how we
-- tell our own UCI sections from a human's. A section we wrote carries
-- `option oonfeewrt '1'` on the device AND a row here; anything else is foreign
-- and is read for display, never rewritten.
CREATE TABLE IF NOT EXISTS owned_sections (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  config TEXT NOT NULL, section TEXT NOT NULL,
  rendered_hash TEXT NOT NULL,         -- legacy clear verifier; always empty from v14
  rendered_hash_enc BLOB,              -- sealed canonical hash, row-bound (v14)
  applied_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, config, section)
);

-- Binds this database to its keyring and makes the post-v13 plaintext scrub
-- crash-resumable. A v14 daemon verifies key_check before it opens any site
-- secret. scrub_complete stays 0 until checkpoint -> VACUUM -> checkpoint has
-- removed the legacy values from WAL, free pages and the main database.
CREATE TABLE IF NOT EXISTS secret_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  key_check BLOB NOT NULL,
  scrub_complete INTEGER NOT NULL DEFAULT 0 CHECK (scrub_complete IN (0,1))
);
CREATE TABLE IF NOT EXISTS changesets (
  id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
  author TEXT NOT NULL, status TEXT NOT NULL,
  summary TEXT NOT NULL, detail_json TEXT NOT NULL
);

-- A durable receipt for one fleet Apply request (migration v13).
--
-- request_hash is a controller-keyed digest supplied by the caller. The raw
-- preview token and request body never belong in durable state. result_json is
-- likewise the deliberately redacted public result, not a render or UCI plan.
CREATE TABLE IF NOT EXISTS apply_operations (
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
);
CREATE TABLE IF NOT EXISTS apply_operation_devices (
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
);

-- ===== clients =====
CREATE TABLE IF NOT EXISTS clients (
  mac TEXT PRIMARY KEY, name TEXT, note TEXT,
  ip TEXT,                             -- last observed address (migration v2)
  fixed_ip TEXT, blocked INTEGER NOT NULL DEFAULT 0,
  grp TEXT, first_seen INTEGER, last_seen INTEGER,
  scope TEXT,                          -- local|upstream, NULL = undetermined (migration v3)
  fingerprint_json TEXT NOT NULL DEFAULT '{}'
);

-- ===== telemetry (rollups only — the raw ring lives in RAM, decision D4) =====
CREATE TABLE IF NOT EXISTS series (
  id INTEGER PRIMARY KEY,
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  key TEXT NOT NULL,
  UNIQUE (device_id, kind, key)
);
CREATE TABLE IF NOT EXISTS rollup_5m (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  avg REAL, min REAL, max REAL, cnt INTEGER NOT NULL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS rollup_1h (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  avg REAL, min REAL, max REAL, cnt INTEGER NOT NULL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
-- The primary key serves reads (one series over a range). Maintenance goes the
-- other way — every series within a time range — so retention pruning and the
-- 5m->1h fold would otherwise scan the whole table every five minutes.
CREATE INDEX IF NOT EXISTS rollup_5m_ts ON rollup_5m(ts);
CREATE INDEX IF NOT EXISTS rollup_1h_ts ON rollup_1h(ts);
-- The client list asks "has this MAC any recent station telemetry" without
-- knowing which device saw it. The UNIQUE constraint above leads with
-- device_id, so that question cannot use it. (migration v6)
CREATE INDEX IF NOT EXISTS series_kind_key ON series(kind, key);

-- ===== events =====
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL,
  device_id INTEGER, category TEXT NOT NULL,
  severity TEXT NOT NULL,
  event TEXT NOT NULL, detail_json TEXT NOT NULL DEFAULT '{}',
  source TEXT NOT NULL DEFAULT 'controller',
  source_id TEXT, source_boot TEXT, ingested_at INTEGER, -- Unix milliseconds; ts remains legacy seconds
  client_mac TEXT, action TEXT, direction TEXT,
  in_iface TEXT, out_iface TEXT,
  src_ip TEXT, dst_ip TEXT, src_port INTEGER, dst_port INTEGER,
  zone_in TEXT, zone_out TEXT, policy_id INTEGER
);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
-- The v16 migration creates the provenance indexes after adding these columns.
-- Keeping those CREATE INDEX statements in schema.sql would run them before
-- ALTER TABLE on an existing v15 database (CREATE TABLE IF NOT EXISTS leaves
-- its old events shape untouched) and make the migration impossible.

-- Durable producer cursors. boot_id includes any producer-local generation
-- needed to distinguish resets (for example kernel boot id + logd generation).
CREATE TABLE IF NOT EXISTS ingest_cursors (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  boot_id TEXT NOT NULL,
  cursor TEXT NOT NULL,
  updated_at INTEGER NOT NULL, -- Unix milliseconds
  continuity_gap_at INTEGER NOT NULL DEFAULT 0, -- Unix milliseconds; 0 means none retained
  PRIMARY KEY (device_id, source)
) WITHOUT ROWID;

-- ===== topology history =====
-- Stable refs, not observed interface aliases, are node identity. Managed
-- devices use device:<inventory-mac>, which survives unadopt/re-adopt; aliases
-- remain evidence so an FDB cannot duplicate one physical device.
CREATE TABLE IF NOT EXISTS topology_edges (
  id INTEGER PRIMARY KEY,
  child_node TEXT NOT NULL,
  child_mac TEXT,
  parent_node TEXT NOT NULL,
  parent_device_id INTEGER REFERENCES devices(id) ON DELETE SET NULL,
  parent_port TEXT,
  medium TEXT NOT NULL,
  confidence TEXT NOT NULL,
  valid_from INTEGER NOT NULL, -- Unix milliseconds
  valid_to INTEGER,           -- exclusive Unix-millisecond bound; NULL=current
  last_seen INTEGER NOT NULL, -- Unix milliseconds
  evidence_json TEXT NOT NULL DEFAULT '[]',
  ambiguity_json TEXT NOT NULL DEFAULT '[]',
  CHECK (valid_to IS NULL OR valid_to >= valid_from),
  CHECK (last_seen >= valid_from),
  CHECK (valid_to IS NULL OR last_seen <= valid_to)
);
CREATE INDEX IF NOT EXISTS topology_edges_active
  ON topology_edges(child_node, valid_to, last_seen);
CREATE INDEX IF NOT EXISTS topology_edges_replay
  ON topology_edges(valid_from, valid_to);

-- A successful empty observation is distinct from a source nobody could read.
CREATE TABLE IF NOT EXISTS topology_source_states (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('unknown','empty','observed','error')),
  reason TEXT NOT NULL DEFAULT '',
  observed_at INTEGER NOT NULL, -- Unix milliseconds
  PRIMARY KEY (device_id, source)
) WITHOUT ROWID;

-- ===== explicit RF scans =====
-- radio_key is the UCI wifi-device section (radio0, radio1, ...), never a
-- runtime phy/interface. Scans are operator-triggered; no schedule lives here.
CREATE TABLE IF NOT EXISTS radio_scans (
  id INTEGER PRIMARY KEY,
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  radio_key TEXT NOT NULL,
  started_at INTEGER NOT NULL, -- Unix milliseconds
  finished_at INTEGER,        -- Unix milliseconds
  status TEXT NOT NULL CHECK (status IN ('pending','running','completed','failed')),
  detail_json TEXT NOT NULL DEFAULT '{}',
  CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX IF NOT EXISTS radio_scans_radio_time
  ON radio_scans(device_id, radio_key, started_at, id);
CREATE TABLE IF NOT EXISTS radio_scan_bss (
  scan_id INTEGER NOT NULL REFERENCES radio_scans(id) ON DELETE CASCADE,
  bssid TEXT NOT NULL,
  ssid TEXT NOT NULL,
  mhz INTEGER NOT NULL,
  channel INTEGER NOT NULL,
  signal INTEGER,
  width INTEGER,
  PRIMARY KEY (scan_id, bssid, mhz)
) WITHOUT ROWID;

-- Wireless uplinks (migration v8). One row per device by UNIQUE: a router with
-- two wireless uplinks into the same network is a layer-2 loop rather than
-- redundancy, so the constraint says it once instead of every writer
-- remembering. ON DELETE CASCADE because an un-adopted device's uplink
-- describes how it reaches a network it is no longer part of.
CREATE TABLE IF NOT EXISTS uplinks (
  id INTEGER PRIMARY KEY,
  device_id INTEGER NOT NULL UNIQUE REFERENCES devices(id) ON DELETE CASCADE,
  wlan_id INTEGER NOT NULL REFERENCES wlans(id),
  band TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1
);

-- A decision an operator recorded ABOUT a foreign wireless section.
--
-- It holds no copy of the section: no values, no passphrase, nothing to leak
-- and nothing that could later be restored wrongly. The section stays where it
-- has always been — on the operator's device, owned by them. All this records
-- is that a human looked at it and decided something, so the controller can
-- stop reporting it as an open question.
CREATE TABLE IF NOT EXISTS foreign_ssid_notes (
  device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  section    TEXT NOT NULL,
  ssid       TEXT NOT NULL,   -- as seen when the note was written
  note       TEXT NOT NULL,
  decided_at INTEGER NOT NULL,
  decided_by TEXT NOT NULL,
  PRIMARY KEY (device_id, section)
);
