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
  name         TEXT NOT NULL,
  role         TEXT NOT NULL DEFAULT 'ap',     -- 'gateway'|'ap'|'switch'
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
  security_json TEXT NOT NULL,
  roaming_json TEXT NOT NULL DEFAULT '{}',
  options_json TEXT NOT NULL DEFAULT '{}',
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
  rendered_hash TEXT NOT NULL,         -- sha256 of canonical rendered values
  applied_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, config, section)
);
CREATE TABLE IF NOT EXISTS changesets (
  id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
  author TEXT NOT NULL, status TEXT NOT NULL,
  summary TEXT NOT NULL, detail_json TEXT NOT NULL
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

-- ===== events =====
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL,
  device_id INTEGER, category TEXT NOT NULL,
  severity TEXT NOT NULL,
  event TEXT NOT NULL, detail_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
