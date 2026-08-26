# oonfeeWRT — Implementation Specification

This is the build document. It is written for a coding agent (or a human) who
will implement the system without access to the design conversations that
produced it. Where the other documents say *what* and *why*, this one says
*exactly how*, down to schemas, interfaces, state machines, and worked examples.

Read order for a builder: `README.md` → `ARCHITECTURE.md` → `DEVICE-BUDGET.md`
→ this file. `PARITY-MATRIX.md` and `UI-SPEC.md` are reference material per
screen. `BUILD-PROMPT.md` explains how to drive a build session.

---

## 0. Decision log (deltas that supersede earlier text)

| # | Decision | Supersedes |
|---|---|---|
| ~~D1~~ | ~~Controller runs on the WRT3200ACM itself~~ | **superseded by D7** |
| D2 | **Apply ordering: `uci.set` stages, `uci.apply {rollback:true}` commits.** Never `uci.commit` before `apply` — it silently disarms rollback. | earlier §4 text and probe behavior (both fixed) |
| D3 | **Pure-Go SQLite (`modernc.org/sqlite`), `CGO_ENABLED=0`.** No cgo means the container can be `FROM scratch` and cross-arch builds are trivial. | none |
| D4 | **Raw telemetry ring lives in RAM; only 5m/1h rollups reach SQLite**, one transaction per 5-minute flush. Retention default: 5m→14d, 1h→13mo. | the 30s-raw-persisted ladder |
| ~~D5~~ | ~~Self-management over loopback~~ | **superseded by D7** — there is no self to manage |
| D6 | Target UX reference originated with **UniFi Network 10.4** screenshots and tracks the current stable release; the active baseline is **Network 10.5.67** as of 2026-08-18. Copy interaction contracts and information architecture, never Ubiquiti branding or assets. | earlier fixed 10.4 target |
| D7 | **The controller is a self-hosted container (Omada-style)** — Docker/Podman image, amd64+arm64, one persistent volume, compose file provided. Managed devices remain agentless stock OpenWrt; the WRT3200ACM is a *managed device*, never the host. Discovery is a convenience layer (host networking gets full discovery; bridge/Desktop gets add-by-IP, which must be first-class UI). A sweep whose every dial reports `EHOSTUNREACH`/`ENETUNREACH` is an explicit per-network failure, never an empty result. | D1, D5 |
| D8 | **Optional router packages are two-stage, per-feature root actions.** Resolve and display the package manager's exact plan, bind it, then require a second unchecked acknowledgement. Persist the before-state and actual package diff; rollback removes that exact added set while preserving pre-existing packages, and un-adoption stays blocked until rollback succeeds. Adoption never selects a package. The first capability is official-feed `lldpd`. | previously documented future tier-2 flow |

### Published and historical hardware validation checkpoint (2026-08-22)

`v0.1.0-rc.1` and the historical v40 hardware checkpoint expect schema **17**.
The lab used schema 17 for that evidence. Do not collapse its
four recent compatibility
epochs: v14 is the one-time secret-sealing/scrub boundary, v15 makes the
cross-feature policy model authoritative, and v16 adds attested observability
tables/columns for producer-provenanced events and cursors, topology validity
intervals/source state, and explicit RF scans. Schema 17 adds the durable
device capability install/rollback ledger. Read-only tools require the current
schema; neither they nor an older daemon may silently accept newer intent.

The lab database has been promoted to schema 17. The earlier schema-16 pass
proved the joined Phase-4 surfaces and the separately acknowledged ACL refresh.
The fresh-start pass then proved schema 17's optional official-feed LLDP path on
both reference devices: exact plan, install, physical-interface configuration,
read-only diagnosis, drift-checked rollback with final package/service readback,
and clean reinstallation. Default adoption still installs no package, binary,
daemon or service. Historical source coverage remains unavailable because it is
not stored. Final release-candidate artifact
`dev-schema17-fresh-start-transparent-v40` (15,312,098 bytes; SHA-256
`9c3a797c1470d8630f42dc77619007370aad553fae00078716a5a5a457c6b4cc`)
passed the signed-in deep-link, settled regression, recovery, secret and
reproducibility gates. This v40 artifact is a merge-ready local evidence
checkpoint, not itself a Git tag or published release asset. Publication is
performed by the tag-triggered Release workflow; [GitHub
Releases](https://github.com/aiden0rchad/oonfeeWRT/releases) is the source of
truth. The later published `v0.1.0-rc.1` clean-install proof is recorded in
FRESH-START-VALIDATION FS-119; FS-120 records the newer SQLite-sidecar permission
hardening. Neither is retroactively part of the historical v40 claim.

### v0.1.0 schema-19 release boundary

The v0.1.0 source expects schema **19**. Migration 18 adds only the
controller-host `speed_tests` job/history table and its one-active/history
indexes; it does not add a device reference or router-side state. Migration 19
adds the account-store foundation: canonical `owner`, `admin`, `operator` and
`viewer` roles, enabled/deleted state, ASCII-NOCASE username uniqueness and
attestation. Every existing admin row becomes an enabled owner.

The controlled live upgrade/restart completed. The controller reports exact
binary version `dev-phase41-live-schema19` and schema 19, with two devices, two
credentials, one enabled owner, one WLAN and no mesh. A signed-in live UI smoke
passed Dashboard, Accounts, Diagnostics, Backup & Restore, Devices and Topology
with no browser errors. Fresh schema-17 rollback and schema-19 recovery sets
also passed verification.

`GET /api/v1/dashboard` now returns a server-selected six-hour WAN view with 72
complete five-minute buckets. Fixed-target `1.1.1.1` ICMP metrics and
exact-interface RX/download and TX/upload each carry independent freshness and
null gaps; interface counters are omitted unless route and durable series keys
match exactly. The React Dashboard renders these semantics.

The authenticated `/api/v1/speedtests` start/status/cancel/history surface uses
a bounded controller-host runner: Cloudflare direct endpoints, method
`controller-http-single-stream-v1`, 10 MiB download, 5 MiB upload, five idle
latency probes, 30-second timeout, one active job and three terminal rows.
The descriptor exposes endpoint/provenance/data estimate/privacy/saturation and
a deterministic `plan_id` before acknowledgement. Material impact stays beside
the Run action and exact details remain available in a nonmodal popover.
Selecting Run sends the current ID plus `acknowledge_data_use:true`; a changed
plan returns 409 before creating a job. Redirects fail closed; lifecycle events
are audited; cancel and
startup recovery reach durable terminal state. The package has no Fleet/router
dependency and performs no router call or change. Loaded latency/jitter remain
null because the method does not measure them. No public-provider speed test has
run; current evidence uses source tests and deterministic local adapters.

The schema-19 store accepts only the four canonical roles. New usernames use an
ASCII-only grammar and the unique `admins_username_nocase` index; ASCII case
collisions fail migration atomically. Soft deletion disables authentication,
replaces the password verifier and preserves the username tombstone. Conditional
updates protect the last enabled owner under concurrent disable, demote and
delete attempts. Account creation, role/state/deletion and password mutations
commit with their audit event or roll back together. Role-bearing sessions,
server-side route/live authorization, My Account, owner administration and
session listing/revocation are implemented. Owner writes require a five-minute
password step-up. Revocation closes affected `/live` sockets and cancels
in-flight requests.

The stored-only Diagnostics surface is implemented for `owner`/`admin`:
descriptor/preview, one active cancellable generation job, bounded terminal
history, status and private ZIP download. Its fixed, checksummed members contain
bounded redacted/pseudonymized controller, device, topology/radio/source, event
and rotating-controller-log evidence. The generator has no Fleet dependency and
makes zero router management/API/SSH calls or router changes. A private bounded
rotating `controller.jsonl` family supplies the controller-log member.

Encrypted portable controller backup/restore is implemented end to end in
schema-19 source. An owner can export a consistent live-WAL database snapshot
and matching wrapped key material as one authenticated, separately
passphrase-encrypted native `.oowrtbak`. Restore accepts only a bounded raw
artifact, authenticates it, proves its manifest schema matches the source
database, migrates and validates a disposable private copy, and returns a
secret-free preview. Confirmation is bound to that artifact and preview plan;
it requires recent password reauthentication, the export passphrase again, the
current destination runtime passphrase, exact `RESTORE CONTROLLER` text, and
four explicit acknowledgements.

The controlled in-process restart quiesces the daemon, checkpoints and closes
SQLite, verifies and swaps the prepared database/keyring pair, revokes all
sessions, and rolls back before serving if the replacement fails validation.
Before the swap it retains an encrypted mode-0600 safety artifact at
`<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak`. The artifact uses
the same export passphrase supplied at confirmation. Once its applied audit
receipt commits and is cleared, fixed-shape safety retention targets three
artifacts, fills slots newest-first and removes the rest. Any ID referenced by
an active restore marker, receipt or suppression record is always preserved,
even when that temporarily exceeds three.
Operators must copy an artifact off-host before pruning if they need longer
retention. Router writes stay
persistently suppressed until an owner explicitly enters `RESUME ROUTER WRITES`.
Restored desired state is never automatically applied, although read-only
monitoring of restored devices may resume after restart while suppression is
active. Resume immediately re-enables the automatic 802.11k neighbour
reconciler, which may issue hostapd `rrm_nr_set`; it does not start a restored
desired-configuration Apply.

The implementation has source/unit/race coverage, and the historical live
schema-19 migration/restart plus signed-in route/render smoke above. That smoke did not
execute diagnostics generation/download, backup export, restore
upload/preview/confirmation, a public-provider speed test or router restore.
The completed `v0.1.0` tag workflow and GitHub Release are the authority for
final publication; the workflow must pass the isolated release matrix before it
publishes artifacts.

---

## 1. Pinned stack

| Layer | Choice | Rationale |
|---|---|---|
| Language | Go ≥ 1.25; toolchain pinned to Go 1.26.6 | static binary in a scratch container, goroutine-per-device polling; patch pin matches the verified container build |
| SQLite driver | `modernc.org/sqlite` | pure Go (D3) |
| HTTP router | stdlib `net/http` (1.22+ pattern mux) | zero deps, auditable |
| WebSocket | `github.com/coder/websocket` | maintained, minimal |
| Secrets | `golang.org/x/crypto`: argon2id KDF + chacha20poly1305 | credential store |
| UI | React 19 + TypeScript + Vite, static build, embedded via `embed.FS` | current implementation; 1.5 MB gz budget |
| Charts | uPlot | fastest at dense time-series; matches UI-SPEC |
| Tables | TanStack Table (core) + virtual scrolling | Clients/Logs grids |
| Topology | d3 (`d3-hierarchy` tree layout + manual SVG) | UniFi's topology is a tidy tree |

**Forbidden:** cgo anywhere; any ORM; any component mega-framework; any
JS dependency that pushes the gzipped bundle past budget. Every dependency
addition is a decision, not a default.

Build target: multi-arch container image (`linux/amd64`, `linux/arm64`) via
`docker buildx`, `CGO_ENABLED=0`, binary stripped (`-ldflags "-s -w"`), final
stage `FROM scratch` with CA certificates only. CI fails if the image exceeds
40 MB or the UI bundle exceeds 1.5 MB gzipped.

---

## 2. Repository layout

```
oonfeewrt/
├── cmd/oonfeewrtd/main.go        # flags, config load, wiring, graceful shutdown
├── internal/
│   ├── ubus/                     # transport: client, session, batch, TOFU
│   │   ├── client.go
│   │   ├── session.go
│   │   └── types.go              # typed decoders for board/info/iwinfo/…
│   ├── capability/               # probe + registry (mirrors tools/probe.py)
│   ├── store/                    # SQLite: schema.sql, migrations, queries
│   ├── model/                    # site model structs + validation
│   ├── render/                   # site model → per-device UCI documents
│   ├── applyengine/              # the state machine (§6)
│   ├── collector/                # poll scheduling, ring buffer, rollups (§7)
│   ├── events/                   # event ingest, enrichment, pruning
│   ├── topology/                 # LLDP/fdb/ARP/assoc → graph inference
│   ├── api/                      # REST handlers + WS hub (§8)
│   └── secrets/                  # encrypted credential store
├── ui/                           # React app (built → embedded)
│   └── src/{components,lib,screens}/
├── tools/
│   ├── probe.py                  # hardware validation (exists)
│   ├── mock_ubus.py              # dev-harness device simulator (exists)
│   └── budget_check.sh           # binary/bundle/RSS assertions for CI
├── deploy/
│   ├── Dockerfile                # multi-stage: ui build → go build → scratch
│   ├── docker-compose.yml        # loopback bridge default, one volume
│   └── acl/oonfeewrt.json        # the rpcd ACL template pushed at adoption
└── docs/                         # these documents
```

Package dependency rule: `api → {store, applyengine, collector, model}`,
`applyengine → {render, ubus, store}`, `collector → {ubus, store}`,
`render → model`. Nothing imports `api`. `ubus` imports only stdlib + websocket.

---

## 3. Data layer

One SQLite database, WAL mode, at `$OONFEE_DATA_DIR` (default `/data`, the
container volume).
`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA wal_autocheckpoint=200;`

Abridged schema overview (`internal/store/schema.sql` is authoritative and ships
with a `schema_version` table and forward-only migrations):

```sql
-- ===== controller accounts (schema 19 store foundation) =====
CREATE TABLE admins (
  id         INTEGER PRIMARY KEY,
  username   TEXT NOT NULL UNIQUE,
  pass_hash  TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_login INTEGER,
  role       TEXT NOT NULL DEFAULT 'owner'
             CHECK (role IN ('owner','admin','operator','viewer')),
  enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  deleted_at INTEGER
);
CREATE UNIQUE INDEX admins_username_nocase
  ON admins (username COLLATE NOCASE); -- ASCII-only by SQLite design

-- ===== inventory =====
CREATE TABLE devices (
  id           INTEGER PRIMARY KEY,
  mac          TEXT NOT NULL UNIQUE,
  host         TEXT NOT NULL,            -- ip or name
  port         INTEGER NOT NULL DEFAULT 80,
  scheme       TEXT NOT NULL DEFAULT 'http',   -- 'http'|'https'
  cert_fp      TEXT,                     -- sha256 of DER, TOFU-pinned
  host_key_fp  TEXT,                     -- SSH host key, TOFU-pinned (v9)
  name         TEXT NOT NULL,
  role         TEXT NOT NULL DEFAULT 'ap',     -- legacy deterministic primary label
  functions_json TEXT NOT NULL DEFAULT '["ap","switch"]', -- authoritative responsibilities (v11)
  adopted_at   INTEGER,                  -- unix; NULL = pending
  cred_enc     BLOB,                     -- chacha20poly1305(username:password)
  class        TEXT,                     -- 'A'|'B'|'C' per DEVICE-BUDGET
  caps_json    TEXT NOT NULL DEFAULT '{}',     -- capability registry snapshot
  fw_release   TEXT,
  last_seen    INTEGER,
  poll_state   TEXT NOT NULL DEFAULT 'baseline' -- 'baseline'|'focused'|'quiesced'|'backoff'
);

-- ===== site model (desired state) =====
CREATE TABLE networks (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  vlan INTEGER NOT NULL UNIQUE, cidr TEXT NOT NULL,
  zone TEXT NOT NULL DEFAULT 'lan',
  dhcp_json TEXT NOT NULL DEFAULT '{}',  -- {enabled,start,limit,leasetime,options[]}
  ipv6_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE ap_groups (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE
);
CREATE TABLE ap_group_members (
  group_id INTEGER REFERENCES ap_groups(id) ON DELETE CASCADE,
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, device_id)
);
CREATE TABLE wlans (
  id INTEGER PRIMARY KEY, ssid TEXT NOT NULL,
  network_id INTEGER NOT NULL REFERENCES networks(id),
  group_id INTEGER NOT NULL REFERENCES ap_groups(id),
  bands TEXT NOT NULL DEFAULT '2g,5g',   -- csv subset of 2g,5g,6g
  security_json TEXT NOT NULL,           -- non-secret {mode,pmf} only (v14)
  security_key_enc BLOB,                 -- sealed PSK, AAD-bound to WLAN id (v14)
  roaming_json TEXT NOT NULL DEFAULT '{}', -- {ft:bool, ft_over_ds:bool, kv:bool}
  options_json TEXT NOT NULL DEFAULT '{}', -- {hidden,isolate,maxassoc,schedule,...}
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE meshes (
  id INTEGER PRIMARY KEY, mesh_id TEXT NOT NULL,
  network_id INTEGER NOT NULL REFERENCES networks(id),
  group_id INTEGER NOT NULL REFERENCES ap_groups(id),
  band TEXT NOT NULL,
  key TEXT NOT NULL DEFAULT '',           -- legacy v7 column; empty from v14
  key_enc BLOB,                           -- sealed SAE key, AAD-bound to mesh id (v14)
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE zones (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,   -- Internal/Guest/DMZ/External/VPN
  policy_json TEXT NOT NULL DEFAULT '{}' -- schema 12 explicit {"forward_to":[...]}
);
CREATE TABLE fw_rules (
  id INTEGER PRIMARY KEY, sort INTEGER NOT NULL,
  rule_json TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE device_overrides (       -- explicit per-device deviations
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  path TEXT NOT NULL,                 -- e.g. 'radio:radio0:channel'
  value_json TEXT NOT NULL,
  PRIMARY KEY (device_id, path)
);

-- ===== reconciliation bookkeeping =====
CREATE TABLE owned_sections (          -- ownership tags, mirrored from device
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  config TEXT NOT NULL, section TEXT NOT NULL,
  rendered_hash TEXT NOT NULL,         -- legacy clear verifier; empty from v14
  rendered_hash_enc BLOB,              -- sealed canonical hash, row-bound (v14)
  applied_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, config, section)
);
CREATE TABLE secret_state (            -- singleton database/keyring binding (v14)
  id INTEGER PRIMARY KEY CHECK (id = 1),
  key_check BLOB NOT NULL,
  scrub_complete INTEGER NOT NULL DEFAULT 0 CHECK (scrub_complete IN (0,1))
);
CREATE TABLE changesets (
  id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
  author TEXT NOT NULL, status TEXT NOT NULL,   -- 'pending'|'applying'|'applied'|'failed'|'rolledback'
  summary TEXT NOT NULL, detail_json TEXT NOT NULL  -- full per-device diffs, audit
);

-- ===== clients =====
CREATE TABLE clients (
  mac TEXT PRIMARY KEY, name TEXT, note TEXT,
  fixed_ip TEXT, blocked INTEGER NOT NULL DEFAULT 0,
  grp TEXT, first_seen INTEGER, last_seen INTEGER,
  fingerprint_json TEXT NOT NULL DEFAULT '{}'   -- oui vendor, dhcp hints, inferred type
);

-- ===== telemetry (rollups only — raw ring is RAM, D4) =====
CREATE TABLE series (
  id INTEGER PRIMARY KEY,
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,   -- 'if_rx_bps','if_tx_bps','sta_rssi','radio_busy_pct',
                        -- 'cpu_pct','mem_pct','wan_lat_ms','wan_loss_pct','client_bytes',…
  key TEXT NOT NULL,    -- interface name / station MAC / radio name / probe target
  UNIQUE (device_id, kind, key)
);
CREATE TABLE rollup_5m (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL,   -- slot start, unix
  avg REAL, min REAL, max REAL, cnt INTEGER NOT NULL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
CREATE TABLE rollup_1h (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  avg REAL, min REAL, max REAL, cnt INTEGER NOT NULL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;

-- ===== events =====
CREATE TABLE events (
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL,
  device_id INTEGER, category TEXT NOT NULL,  -- 'client'|'device'|'security'|'system'|'audit'
  severity TEXT NOT NULL,                     -- 'info'|'warning'|'error'
  event TEXT NOT NULL, detail_json TEXT NOT NULL DEFAULT '{}',
  source TEXT NOT NULL DEFAULT 'controller',
  source_id TEXT, source_boot TEXT, ingested_at INTEGER,
  client_mac TEXT, action TEXT, direction TEXT,
  in_iface TEXT, out_iface TEXT,
  src_ip TEXT, dst_ip TEXT, src_port INTEGER, dst_port INTEGER,
  zone_in TEXT, zone_out TEXT, policy_id INTEGER
);
CREATE INDEX events_ts ON events(ts);
CREATE UNIQUE INDEX events_source_identity
  ON events(device_id,source,source_boot,source_id)
  WHERE source_id IS NOT NULL AND trim(source_id) <> '';
CREATE INDEX events_client_time ON events(client_mac,ts,id);
CREATE INDEX events_category_time ON events(category,ts,id);
CREATE INDEX events_severity_time ON events(severity,ts,id);

CREATE TABLE ingest_cursors (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  source TEXT NOT NULL, boot_id TEXT NOT NULL,
  cursor TEXT NOT NULL, updated_at INTEGER NOT NULL,
  continuity_gap_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (device_id,source)
) WITHOUT ROWID;

-- ===== topology validity history =====
CREATE TABLE topology_edges (
  id INTEGER PRIMARY KEY,
  child_node TEXT NOT NULL, child_mac TEXT,
  parent_node TEXT NOT NULL,
  parent_device_id INTEGER REFERENCES devices(id) ON DELETE SET NULL,
  parent_port TEXT, medium TEXT NOT NULL, confidence TEXT NOT NULL,
  valid_from INTEGER NOT NULL, valid_to INTEGER, last_seen INTEGER NOT NULL,
  evidence_json TEXT NOT NULL DEFAULT '[]',
  ambiguity_json TEXT NOT NULL DEFAULT '[]',
  CHECK (valid_to IS NULL OR valid_to >= valid_from),
  CHECK (last_seen >= valid_from),
  CHECK (valid_to IS NULL OR last_seen <= valid_to)
);
CREATE INDEX topology_edges_active ON topology_edges(child_node,valid_to,last_seen);
CREATE INDEX topology_edges_replay ON topology_edges(valid_from,valid_to);

CREATE TABLE topology_source_states (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('unknown','empty','observed','error')),
  reason TEXT NOT NULL DEFAULT '', observed_at INTEGER NOT NULL,
  PRIMARY KEY (device_id,source)
) WITHOUT ROWID;

-- ===== explicit RF scans =====
CREATE TABLE radio_scans (
  id INTEGER PRIMARY KEY,
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  radio_key TEXT NOT NULL, started_at INTEGER NOT NULL, finished_at INTEGER,
  status TEXT NOT NULL CHECK (status IN ('pending','running','completed','failed')),
  detail_json TEXT NOT NULL DEFAULT '{}',
  CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX radio_scans_radio_time
  ON radio_scans(device_id,radio_key,started_at,id);
CREATE TABLE radio_scan_bss (
  scan_id INTEGER NOT NULL REFERENCES radio_scans(id) ON DELETE CASCADE,
  bssid TEXT NOT NULL, ssid TEXT NOT NULL,
  mhz INTEGER NOT NULL, channel INTEGER NOT NULL, signal INTEGER, width INTEGER,
  PRIMARY KEY (scan_id,bssid,mhz)
) WITHOUT ROWID;

-- ===== optional device capability ownership (v17) =====
CREATE TABLE device_capability_installs (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE RESTRICT,
  capability TEXT NOT NULL,
  package_manager TEXT NOT NULL CHECK (package_manager IN ('apk','opkg')),
  requested_packages_json TEXT NOT NULL,
  baseline_packages_json TEXT NOT NULL DEFAULT '[]',
  added_packages_json TEXT NOT NULL DEFAULT '[]',
  services_json TEXT NOT NULL DEFAULT '[]',
  state TEXT NOT NULL CHECK (state IN ('installing','installed','removing','error')),
  detail TEXT NOT NULL DEFAULT '', installed_at INTEGER, updated_at INTEGER NOT NULL,
  PRIMARY KEY (device_id,capability)
) WITHOUT ROWID;
```

Migration 11 adds and backfills `functions_json` in one transaction. Existing
`gateway` rows become `["gateway","ap","switch"]`, `ap` (and the historical
safe default) becomes `["ap","switch"]`, and `switch` becomes `["switch"]`.
The migrated live v10 database passed `PRAGMA integrity_check`; malformed or
present-empty function JSON remains visible as an invalid device state and is
never widened from `role`.

Migration 12 is deliberately DDL-empty and still mandatory. `zones` and
`policy_json` existed from v1, but older binaries ignored the rows and always
rendered source→`wan`; allowing a v11 process to open a database containing an
explicit block or inter-zone edge would silently weaken policy. A v11 binary
therefore refuses a consistent v12 database. Back up a live WAL database with
SQLite `.backup` (or a clean shutdown/checkpoint), never by copying the main
file alone, and preserve the matching `keyring.json`; the latter was measured
producing an apparently valid but stale schema-11 artifact while the
authoritative WAL held schema 12.

In schema 12, persisted policy must decode to exactly a non-null string array.
A missing row is the effective legacy `forward_to:["wan"]`; an explicit empty
array means no modeled forwarding. Malformed/trailing/null content fails the
site load instead of being repaired into an allow.

Migration 13 adds `apply_operations` plus ordered
`apply_operation_devices`. The parent stores a caller-generated lowercase UUID,
a controller-keyed request digest, actor snapshot, state, timestamps, redacted
result/error, HTTP status and the conservative `none|possible` write boundary.
Child rows snapshot device identity and record each queued/applying/terminal
outcome so later un-adoption cannot erase the receipt. Neither preview tokens,
rendered secrets nor raw UCI plans enter durable state.

Migration 14 is the one-time controller-secret migration. It requires the
unlocked controller keyring, verifies that keyring against every existing sealed
device credential when such evidence exists, and performs the logical rewrite
under the migration lock: WLAN keys leave `security_json` for
`security_key_enc`, mesh `key` values move to `key_enc`, and ownership
`rendered_hash` values move to `rendered_hash_enc`. Every ciphertext is bound by
AAD to its record identity. The same transaction writes a sealed database key
check, records schema 14, and leaves `scrub_complete=0`.

After commit, startup performs checkpoint → `VACUUM` → checkpoint, sets the
scrub marker, and checkpoints again before the HTTP server can start. This
physical scrub is idempotent after interruption and removes legacy plaintext
from free pages and WAL, not only from live rows. Every subsequent v14 open
verifies the database key check before enabling WAL or mutating the file. A
wrong/missing keyring, malformed key check, newer schema, or incomplete scrub on
a read-only open fails closed; a writable v14 controller resumes an incomplete
scrub.

At the storage layer, treat the database and `keyring.json` as a paired backup.
`Store.BackupTo` uses SQLite's online mechanism to create a private no-clobber
snapshot that includes committed WAL state and verifies it before publication;
clean shutdown/checkpoint remains an alternative. A manual recovery copy must
include the matching keyring; schema-19 portable export wraps both safely in one
`.oowrtbak`. A passphrase alone cannot recreate the random data key. Pre-v14
backups retain
plaintext WLAN/mesh keys and secret-derived ownership hashes even after the live
database is scrubbed; migration does not delete those artifacts. Protect them
as secrets, keep recovery material until the migrated pair is verified, and
never delete an old backup without explicit operator confirmation.

Migration 15 is DDL-empty but semantic: v14 binaries ignore `fw_rules` and
client policy intent while rendering, so they must refuse a store whose
firewall/NAT/route/client model is authoritative. Migration 16 adds the event,
cursor, topology and scan shape above. Startup attests every column in order,
the partial-unique source index predicate, required indexes/foreign keys,
WITHOUT-ROWID and CHECK clauses before accepting a v16 marker; `IF NOT EXISTS`
is never mistaken for validation.

Migration 17 adds `device_capability_installs`. `ON DELETE RESTRICT` is
load-bearing: un-adoption cannot discard the only package/service rollback
record. The row is written in `installing` state before SSH mutates the router,
then completed with the observed package diff; interruption or uncertainty
becomes `error`, never an invented clean state.

Migration 18 adds `speed_tests`, a partial unique index permitting one active
`controller-host` job and a descending history index. Consent records the exact
reviewed deterministic `plan_id`; jobs preserve provenance, progress, nullable
metrics, measured byte counts and terminal errors without a device foreign key.

Migration 19 adds `role`, `enabled` and `deleted_at` to `admins`, so every legacy
administrator becomes an enabled owner without a password rewrite. Its unique
`username COLLATE NOCASE` index is intentionally ASCII-only and makes a legacy
ASCII case collision abort and roll back the whole migration. New account names
are limited to the matching ASCII grammar. Store mutation methods soft-delete,
preserve one enabled owner atomically and append the corresponding audit event
inside the same transaction. The source layers server-side authorization,
role-bearing sessions, account/session APIs and My Account/owner administration
on that schema.

Maintenance runs every 5 minutes. The RAM-ring flush writes its completed
`rollup_5m` rows in one transaction; hourly folding is count-weighted and never
overwrites a fuller hour with a pruned partial. Event retention runs first in
an independent transaction and also runs before the daemon begins serving, so
a rollup failure or short restart cycle cannot leave event history unbounded.
Defaults are 5m→14d, 1h→396d, OpenWrt events→24h plus 50k/device and 100k
global, non-OpenWrt events→100k, and closed topology intervals→31d. Active
topology intervals do not expire. Terminal RF scans are capped to the newest
run per `(device_id,radio_key)`; pending/running rows are excluded and pruned
parents cascade to `radio_scan_bss`. Do not regress to per-sample inserts or
unbounded scan history. Every event is limited to 64 KiB across its stored text
and encoded detail. Exact repeated odhcpd IPv6-RA/no-default-route warnings are
kept as one warning condition per router-log epoch with an occurrence count and
first/latest source evidence; unrelated router warnings remain individual rows.

---

## 4. The ubus client (`internal/ubus`)

```go
type Client struct { /* host, scheme, http.Client with keep-alive, session token, mu */ }

// Call performs one ubus invocation. Handles: session refresh on JSON-RPC
// error -32002 with EXACTLY ONE retry; transport retry with jittered backoff;
// decoding [status, payload] result frames.
//
// Do NOT refresh on ubus status 6. Measured on hardware (§14): status 6 means
// the session is valid and the *target* is not permitted, so re-authenticating
// changes nothing and the retry is pure latency. -32002 is the ambiguous one —
// dead session OR an object+method in no granted access-group — which is why
// it gets one retry and not a loop: if it recurs after a successful re-login,
// surface it as a permanent capability error.
// Both retries MUST be suppressed while a confirmation window is open.
func (c *Client) Call(ctx context.Context, object, method string, args, out any) error

// Batch sends multiple invocations in one JSON-RPC array (one HTTP round trip).
// Falls back to sequential Calls if the device rejects array bodies —
// detected once at adoption, recorded in the capability registry.
func (c *Client) Batch(ctx context.Context, calls []Invocation) ([]Result, error)

// Login authenticates and stores the ubus_rpc_session token.
func (c *Client) Login(ctx context.Context, user, pass string) error
```

Non-negotiables (each is a DEVICE-BUDGET consequence):

1. **One `http.Transport` per device**, `MaxIdleConnsPerHost=1`,
   `IdleConnTimeout` ≥ 2× the baseline poll interval — the connection must
   survive between polls. Never `DisableKeepAlives`.
2. **TOFU pinning:** custom `tls.Config.VerifyPeerCertificate` that compares
   the leaf's SHA-256 against `devices.cert_fp`; mismatch = hard fail + event,
   never a prompt-through.
3. The null session `00000000000000000000000000000000` is used only for
   `session.login`.
4. Every device is a remote peer — there is no loopback special case (D7).
5. Timeouts: connect 5 s, call 15 s, `file.exec` calls 30 s. All calls take a
   `context.Context` and honor cancellation — the apply engine depends on this.

Typed decoders in `types.go` for every response shape the system consumes
(`SystemBoard`, `SystemInfo`, `IwinfoInfo`, `AssocEntry`, `NetworkDevice`,
`HostHints`, `DHCPLease`, `UciChanges`…). No `map[string]interface{}` escapes
this package.

---

## 5. Rendering: site model → UCI (`internal/render`)

Pure functions, no I/O: `Render(site model.Site, dev model.Device, caps capability.Caps) (Doc, error)`
where `Doc` is a set of UCI sections per config file. Fully unit-testable with
golden files — this package should carry the densest test suite in the repo.

### Naming and ownership

Every section we create is named `oowrt_<entity><id>[_<qualifier>]` and carries
`option oonfeewrt '1'`. The reconciler's contract, verbatim from ARCHITECTURE:
sections without the marker are read-only; name-collisions or functional
conflicts (foreign SSID with same name on same radio) are surfaced as conflicts
and abort the render for that device.

### Worked example 1 — WLAN fan-out (the product, in one example)

Site model: WLAN id=3, ssid `example-wlan`, security `sae-mixed` key
`example-passphrase`,
bands `2g,5g`, network `lan` (VLAN 1), roaming `{ft:true, ft_over_ds:true, kv:true}`,
group containing device 7 whose caps report radios `radio0` (5g) and `radio1` (2g).

Rendered staged calls for device 7 (order matters; all staged, no commit — D2):

```
uci.add {config:"wireless", type:"wifi-iface", name:"oowrt_wlan3_radio0", values:{
  device:"radio0", mode:"ap", ssid:"example-wlan", encryption:"sae-mixed", key:"example-passphrase",
  network:"lan", ieee80211w:"1",
  ieee80211r:"1", mobility_domain:"e3a1", ft_over_ds:"1", reassociation_deadline:"20000",
  bss_transition:"1", wnm_sleep_mode:"1", time_advertisement:"2", time_zone:"UTC",
  ieee80211k:"1", rrm_neighbor_report:"1", rrm_beacon_report:"1",
  oonfeewrt:"1"}}
uci.add {config:"wireless", type:"wifi-iface", name:"oowrt_wlan3_radio1", values:{ …same, device:"radio1"… }}
```

Rules encoded here:
- `mobility_domain` is **derived deterministically** from the WLAN id
  (`crc16(site_uuid + wlan_id)` hex) so every AP in the group renders the same
  value without coordination — this is the cross-device consistency a
  controller exists for.
- Band selection = intersection of the WLAN's `bands` and the device's radios
  by capability; a WLAN asking for 6g renders nothing on a device with no 6g
  radio (absent, not error).
- Options the device's hostapd doesn't support (from capability probe) are
  omitted, and the omission is recorded in the render report shown in the diff
  preview.
- 802.11r + WPA2-PSK is rendered only if the WLAN explicitly opted in past the
  compatibility warning (UI concern, but render enforces the stored flag).

### Worked example 2 — network + zone

Network `iot` VLAN 45, `203.0.113.1/24`, zone `Guest`, DHCP on with configured
pool start `100`, lease count `149` and lease time `12h`:

```
# /etc/config/network (staged)
uci.add {config:"network", type:"bridge-vlan", name:"oowrt_bv45", values:{device:"br-lan", vlan:"45", ports:[…per-device port map…], oonfeewrt:"1"}}
uci.add {config:"network", type:"interface", name:"oowrt_net_iot", values:{proto:"static", device:"br-lan.45", ipaddr:"203.0.113.1", netmask:"255.255.255.0", oonfeewrt:"1"}}
# /etc/config/dhcp
uci.add {config:"dhcp", type:"dhcp", name:"oowrt_dhcp_iot", values:{interface:"oowrt_net_iot", start:"100", limit:"149", leasetime:"12h", oonfeewrt:"1"}}
# /etc/config/firewall
uci.add {config:"firewall", type:"zone", name:"oowrt_zone_guest", values:{name:"guest", input:"REJECT", output:"ACCEPT", forward:"REJECT", network:["oowrt_net_iot"], oonfeewrt:"1"}}
uci.add {config:"firewall", type:"rule", name:"oowrt_in_guest_dhcp", values:{name:"oonfeeWRT Guest DHCP", src:"guest", proto:"udp", src_port:"68", dest_port:"67", target:"ACCEPT", family:"ipv4", oonfeewrt:"1"}}
uci.add {config:"firewall", type:"rule", name:"oowrt_in_guest_dns", values:{name:"oonfeeWRT Guest DNS", src:"guest", proto:["tcp","udp"], dest_port:"53", target:"ACCEPT", family:"ipv4", oonfeewrt:"1"}}
uci.add {config:"firewall", type:"forwarding", name:"oowrt_fwd_guest_wan", values:{src:"guest", dest:"wan", oonfeewrt:"1"}}
```

The forwarding shown is the effective schema-12 legacy default because Guest
has no explicit policy row. `forward_to:[]` emits none; e.g.
`forward_to:["cameras","wan"]` emits two owned directed sections and says
nothing about initiation in the reverse direction. Zone and forwarding
sections manage/remove stale `enabled` and `family` options so an old
`enabled=0` or restricted family cannot make the UI's effective state false.
They never edit foreign firewall sections. Active foreign forwardings,
overlapping `ACCEPT`/`REJECT`/`DROP` forwarding rules, and DNAT redirects with a
real `dest_ip` become deterministic conflicts when they defeat a matrix claim;
disabled sections and router-local redirects do not. Active foreign firewall
includes and reachable non-fw4 nftables policy block explicit matrix intent;
unreadable or malformed runtime nftables state fails closed.

The renderer keys each responsibility independently. Gateway emits addressing,
DHCP and firewall/routing; AP emits WLAN/mesh/on-air state; Switch enables the
switch-specific fit, monitoring and topology surfaces but does not claim that
arbitrary port/VLAN writes are safe. Gateway-only therefore emits no WLAN, and
AP-only/Switch-only emit no L3 stack. Every valid device still receives the
minimum L2 bridge/VLAN plumbing needed to carry its selected responsibilities,
because the capability record cannot safely identify a selective uplink port.
An explicit empty or corrupt function set aborts before any desired section is
produced. Legacy rows alone expand `role` through the schema-11 compatibility
map.

### Diffing

`Diff(rendered Doc, actual UciState) []ChangeItem` — actual state read via
`uci get` per config. Compare only sections we own (marker or `oowrt_` prefix)
plus detect foreign conflicts. Output is the human-readable diff the UI shows
before apply and the exact staged-call list the apply engine executes. The
`rendered_hash` in `owned_sections` short-circuits no-op reconciles.

---

## 6. The apply engine (`internal/applyengine`)

One state machine instance per bound preview plan × device. Before any of these
machines starts, Apply rebuilds a full-fleet preview and constant-time verifies
the opaque keyed token returned by Preview. It rejects the whole run for a site
error, device plan error/conflict, missing traversal/driver/caution
acknowledgment, or an intentionally narrowed fleet without
`acknowledge_partial_fleet`. The token binds site intent, adopted device state,
ownership rows and plans—including secret-bearing inputs without exposing a
secret-derived verifier to the browser.

Devices execute serially
in dependency order; **a device with the Gateway function applies last**
because the controller's management traffic may traverse it to reach the rest;
first failure aborts the remaining queue. Immediately before its write, each
device is locked, reloaded and replanned, and the site/fleet/ledger/plan
fingerprints are compared again.

```
IDLE → RENDER → PREFLIGHT → STAGE → APPLY → HEALTH → CONFIRM_POLL → CONFIRMED
                    │           │       │         │            │
                    └conflict   └err    └err      └timeout─────┴─fail→ AWAIT_REVERT → VERIFY_REVERTED → FAILED
```

| State | Action | Exit |
|---|---|---|
| RENDER | render + diff; an empty diff writes nothing but still runs every applicable runtime health check before it may report Applied | PREFLIGHT or no-write Applied |
| PREFLIGHT | quiesce collector for device; check session; **detect a foreign dirty delta by listing `/tmp/.uci` and treating any entry with `size > 0` as a config with unsaved LuCI/SSH edits** → abort with "unsaved changes on device". **`uci.changes` cannot do this** — see the note below. If the change touches the path the controller manages this device through, require the UI's explicit traversal acknowledgment flag | STAGE |
| STAGE | issue staged `uci.add/set/delete` batch; verify with `uci.changes` that the delta matches the plan exactly | APPLY |
| APPLY | `uci.apply {rollback:true, timeout:T}` — T = 90 s default, per-device override from caps. **Status 0 means "applied", never "healthy": an apply that killed dnsmasq still returns 0** | HEALTH |
| HEALTH | **runs before confirm, while the rollback timer is still armed** — if it fails, do nothing and let the device revert itself. Runtime/non-UCI checks cover expected interfaces; exact dnsmasq ranges/hosts; each desired wireless section mapped through `luci-rpc.getWirelessDevices`, its hostapd SSID/status, and `brport/isolated=1` for isolated BSSes; exact nftables zone/device/edge/service/policy state; static routes in netifd and the kernel; and the upstream gateway on non-Gateway devices. A no-write Apply runs the same applicable checks without a rollback boundary and reports failure rather than calling missing runtime state Applied. Same-BSS isolation and on-air truth remain behavioral/second-radio acceptance checks. A login during the window may return the applying token; mark that helper shared and never destroy it | CONFIRM_POLL |
| CONFIRM_POLL | poll `uci.confirm` every 3 s **on the applying session token** — reconnecting the socket is fine, re-authenticating is fatal, and a token refresh here is an unrecoverable abort. Stop on success or T expiry | CONFIRMED (record `owned_sections`, then audit/return, then resume collector) |
| AWAIT_REVERT | confirm never landed: wait T + grace (15 s), touching nothing | VERIFY_REVERTED |
| VERIFY_REVERTED | **log in afresh** (the applying session's staged delta masks a revert) and compare against the pre-apply snapshot. **Do not assume the device reverted** — if rpcd restarted inside the window the timer was lost and the change is permanent. If the change is still present, reverse it by applying the previous model. Emit an event either way | FAILED |

> **`uci.changes` is blind to LuCI and SSH edits.** rpcd scopes staged deltas to
> a per-session savedir (`/var/run/rpcd/uci-<sid>`), while the `uci` CLI and LuCI
> use the system one (`/tmp/.uci`). Measured: with the CLI holding
> `marker='OPERATOR_WIP'` staged, the controller's `uci.changes` returned `{}`.
> The PREFLIGHT gate as originally specified therefore could never fire for the
> case it existed to catch. Listing `/tmp/.uci` restores it — entries are named
> for the dirty config, and the size filter matters because stale zero-length
> files linger there indefinitely (three were present on a device with exactly
> one real pending change).
>
> The good news, also measured: because the savedirs are separate, **our apply
> does not commit their staged work.** With their `OPERATOR_WIP` pending, our
> apply+confirm landed only our own option and left theirs untouched and still
> staged. So the risk is not data loss on our side — it is that the operator can
> later run `uci commit` in LuCI and land their edit on top of our applied
> config without us knowing. Treat that as a reconciliation problem: re-read
> owned sections after the fact and surface drift, per the ownership rule.

Hard rules: HEALTH gates CONFIRM, so the ordinary failure path costs nothing —
the engine simply declines to confirm and the device reverts itself, which is
why the gate is worth the extra round trip. Reversing a *confirmed* change is
the expensive path and only arises when health degrades after confirmation: the
engine then renders and applies the previous model (a normal apply of the old
state), never by hand-editing. Traversal-sensitive changes require a separate
acknowledgment but remain rollback-protected; there is no unsafe
apply-without-rollback path.

The fleet run is detached from request cancellation once admitted and has one
outer `ApplyDrain` deadline. Nested device runs inherit that deadline rather
than buying another full budget, and a device does not start unless enough time
remains for a complete rollback-confirm cycle. Shutdown waits for tracked
applies. Per-device `Quiesce` first waits for any running collector cycle
through sink emission; release wakes an immediate poll. A confirmed or
already-matching result is not logged/returned as clean success until ownership
recording succeeds. Ledger failure preserves the device outcome in the reason,
writes an error audit and aborts later devices.

---

## 7. Collector (`internal/collector`)

Per-device goroutine owning that device's schedule (baseline/focused/slow per
DEVICE-BUDGET §4), a shared in-RAM ring store, and the 5-minute flush.

```go
type Ring struct { // per series: fixed []Sample{ts int64; v float32}, head index
}
// Ingest appends a sample; Flush drains completed 5m windows as (avg,min,max,cnt).
```

Sampling map (mechanism per metric is fixed in ARCHITECTURE §5's table; this
package implements exactly that table — no new device-side mechanisms). Counter
series (interface bytes) are stored as **rates**, computed at ingest from the
previous counter with wrap detection; the ring never stores raw counters.

Focused mode is reference-counted by the WS hub: `Acquire(deviceID)` on
subscribe, `Release` on unsubscribe/disconnect; the transition is logged as a
poll_state change so the Management Overhead panel can show it.

`Quiesce(deviceID)` is stronger than a flag: after incrementing the quiesce
count it waits on the poller's cycle lock, so a cycle that already started must
finish its sink emission before the call returns. Release is idempotent and
wakes the poller immediately. On the first hard poll failure, close the cached
transport but retain its rpcd session; the next tick redials on a repaired host
route. Only repeated failure drops the session and reauthenticates.

Derived metrics are computed on the controller, never on-device. `wifi-v1` is
fixed—not operator-weighted—and is emitted only when one station observation
has RSSI plus valid retry and TX-failure counter deltas. Its rollup is durable;
raw samples are not. Radio utilization is a survey-counter delta. RX/TX airtime
and interference are emitted only when the capability registry says those
counters are usable.

---

## 8. API and WebSocket

REST base `/api/v1`, session-cookie auth (`HttpOnly; SameSite=Strict; Secure`
when TLS). The current RC has one bootstrap administrator, bounded login
throttling, password change, audit events and memory-only sessions with idle and
absolute expiry. Schema 19 makes every existing administrator an enabled
`owner`; login excludes disabled/deleted rows; sessions carry the canonical
role; and a declarative policy authorizes every protected REST route and
`/live`. My Account and owner administration provide account/session lifecycle
controls. The session `done` channel makes revocation immediate for REST and
`/live`; connection cleanup releases focus. All mutating
endpoints require header `X-Oonfee-CSRF` matching a per-session token.

Diagnostics is owner/admin only. `GET /api/v1/diagnostics` returns the fixed
stored-mode disclosure and bounded job history; `POST /api/v1/diagnostics`
starts one job; `GET /api/v1/diagnostics/{id}` reports it;
`POST /api/v1/diagnostics/{id}/cancel` requests cancellation; and
`GET /api/v1/diagnostics/{id}/download` serves only a completed private ZIP.
The descriptor fixes `router_management_calls:false` and
`router_changes:false`. No handler calls Fleet or a router. Jobs and files are
bounded, cancelled/drained before store/log shutdown, and cleaned on startup,
expiry, failure and cancellation.

Portable backup/restore is owner-only and available only over TLS or direct
loopback. `GET /api/v1/backups` returns the descriptor/history;
`POST /api/v1/backups` starts an export; `GET /api/v1/backups/{id}` reports it;
`POST /api/v1/backups/{id}/cancel` requests cancellation; and
`GET /api/v1/backups/{id}/download` serves a completed native `.oowrtbak`.
Export start/download and every restore mutation require recent password
reauthentication. The export passphrase is a separate caller-owned value of
16 or more Unicode characters and at most 4096 UTF-8 bytes; it is never stored.

`GET /api/v1/restores` returns the restore descriptor. A raw
`application/vnd.oonfeewrt.backup` upload with an exact bounded
`Content-Length` goes to `POST /api/v1/restores/uploads`.
`POST /api/v1/restores/previews` starts disposable authentication/migration/
recovery validation; `GET /api/v1/restores/previews/{id}` reports its bounded
safe result; and `POST /api/v1/restores/previews/{id}/cancel` cancels it.
`POST /api/v1/restores/previews/{id}/confirm` accepts only the current
`plan_id`, re-entered export passphrase, verified current runtime passphrase,
exact destructive text and four acknowledgements, then returns 202 for the
controlled restart. `GET /api/v1/restores/suppression` reports the persistent
router-write gate; `POST /api/v1/restores/suppression/resume` requires recent
reauthentication, the active restore ID and exact `RESUME ROUTER WRITES`.
Upload/preview performs no router call. Confirmation never converts restored
desired state into an Apply; only read-only collection may resume while the
gate is active. Clearing it immediately starts automatic 802.11k neighbour
maintenance, which may write hostapd RRM neighbour state without starting a
desired-configuration Apply.

WLAN and mesh keys are write-only request fields. Authenticated list, detail,
and save responses expose `has_key` but never plaintext; event responses contain
no key, and legacy `?reveal=1` requests remain redacted. An omitted/empty key on
an update preserves the sealed value; an explicit keyless security mode (or mesh
`clear_key:true`) erases it. TLS remains the transport boundary for newly
submitted keys and session cookies—at-rest sealing does not protect browser
traffic or the daemon's live memory.

Endpoint table is ARCHITECTURE §9. The current desired-state/policy contract:

`GET /api/v1/site` includes effective directional policy:
```json
{"zones":[
  {"name":"cameras","forward_to":[],"explicit":true},
  {"name":"guest","forward_to":["wan"],"explicit":false}
]}
```

`POST /api/v1/site/zones/{name}` accepts
`{"forward_to":["cameras","wan"]}`. The field must be a non-null array;
names must be exact active managed destinations or `wan`. `DELETE` removes the
explicit row and returns the effective `forward_to:["wan"], explicit:false`;
deleting a source without an explicit row is 404.

`GET /api/v1/site/preview` returns the redacted fleet diff and
`preview_token`. Apply is synchronous at the HTTP surface but execution is
detached from request cancellation after admission:

```json
{"operation_id":"2a0552f3-f901-4bb1-96a5-8c2ba2de773e",
 "preview_token":"pv1_<opaque>",
 "device_ids":[7],
 "acknowledge_traversal":true,
 "acknowledge_driver_risk":true,
 "acknowledge_cautions":true,
 "acknowledge_partial_fleet":true}
```

`device_ids` omitted means the complete adopted fleet. The UI does not request
a partial fleet. The lowercase UUID idempotently binds one request; reusing it
with different input is rejected, while replaying the same request returns its
current/terminal receipt instead of repeating device writes. Missing/stale
preview token is 409; other full-fleet preflight refusals are 400. Errors carry
`write_state:"none"` only while no device write boundary was crossed; otherwise
the durable parent/device receipts retain `possible`. A completed response is
`{"operation_id":"...","devices":[...],"aborted":bool,"aborted_after":"..."}`.
`GET /api/v1/site/apply/{operation_id}` returns the durable status/result, and
running idempotent replays return 202 plus `Retry-After`. The live §5be browser
reload recovered a failed/reverted fleet run through this path.

WS `/api/v1/live`, JSON messages:
```json
→ {"type":"subscribe","topic":"device.stats","device_id":7}
← {"type":"stats","device_id":7,"ts":1753500000,
   "series":[{"kind":"sta_rssi","key":"aa:bb:…","v":-52.0}, …]}   // batched per tick
```
`device.stats` is the only accepted topic. The per-connection queue is 32
frames; enqueue drops when full, one write is bounded to 10 seconds, and a
30-second ping detects dead peers. It never blocks the collector, grows a
queue, invents an event topic or changes the poll cadence to hide a slow
browser. Durable events/logs remain paginated REST.

Phase-4 REST contracts:

- `GET /events?scope=general|audit` uses `(ts,id)` keyset pagination, 1–1000
  rows, database-wide facets and explicit General coverage. A router producer
  is stale after 3 minutes; coverage distinguishes unobserved, observed-empty
  and retained gaps over the 24-hour log window. Producer continuity follows
  logd's u32 IDs; exact `source_time_ms` is retained in detail, while the public
  event timestamp remains legacy whole-second resolution.
- `GET /topology[?at=ms]` and `/topology/history?from=ms&to=ms` expose stable
  node refs and half-open intervals. History/range is 31 days, one response is
  10,000 intervals, current source state is stale after 31 minutes, and
  historical source coverage is explicitly unavailable.
- `GET /clients/{mac}/observability?from=ms&to=ms` accepts at most 31 days and
  returns only complete stored buckets: 5-minute up to 7 days, hourly beyond.
  It caps exact events at 2,000, path intervals at 10,000 source intervals and
  path enumeration at 64 results/2,048 visits. Every metric carries
  available/partial/unavailable counts and half-open gaps.
- `GET /radios` returns stable UCI radio IDs plus source timestamps/staleness.
  Decoders cap 32 radios, 128 interfaces, 512 frequencies and 4,096 scan BSSes.
  Suggested channels require a completed scan ≤24h old, current radio state
  and a channel plan ≤15m old. `POST /devices/{id}/radios/{radio}/scan` requires
  `acknowledge_disruption:true`, has a 45-second device timeout and persists a
  failed outcome rather than resuming after restart. The maintenance tick keeps
  one newest terminal scan per stable radio key, preserves pending/running
  scans and cascades BSS rows for discarded terminal runs.
- Site WAN series come only from the adopted Gateway's once/minute three-packet
  ICMP probe to fixed `1.1.1.1`. No HTTP probe, DNS check, configurable target
  set or inferred ISP uptime is part of this source contract.
- `POST /devices/{id}/refresh-acl` is the optional oonfeeWRT controller
  capability installation for an adopted device. Its unchecked prompt says
  that accepting installs or replaces exactly
  `/usr/share/rpcd/acl.d/oonfeewrt.json`
  through pinned SSH, invalidates cached access/cadences, and proves a fresh
  login/MAC/capability record. This unlocks supported topology, radio
  channel/scan, OpenWrt log and fixed-target WAN ICMP observations. Its
  administrator password/private key is request-only; UCI, ownership and
  controller credentials remain unchanged. It installs no package, binary,
  daemon, service or firmware. Unchecked/cancelled sends no request, leaves the
  router unchanged and keeps dependent Phase-4 sources explicit gaps.
- `POST /devices/adopt` and `POST /devices/{id}/refresh-acl` require
  `acknowledge_router_changes:true`. Omitted/false is rejected before the
  Enroller, SSH or any mutation. Inspect remains read-only and needs no such
  acknowledgment.

The following additive response/request fields are safety contracts, not UI
conveniences:

- discovery responses may carry `failures:[{network,reason:"unreachable",attempts}]`;
  the normal “nothing found” state is valid only for scanned networks without
  such a failure;
- device detail always carries `owned_sections_known`. Only `true` permits
  `owned_sections`—including an empty list—to authorize an un-adopt preview;
- adopt and un-adopt accept optional `private_key` for their one-time SSH
  transaction. During adoption, `password` remains the required ubus
  credential and is not replaced by the key; during un-adopt, the stored scoped
  credential handles ubus and the supplied password/key are only for SSH
  cleanup;
- `POST /api/v1/devices/inspect` accepts only the device address and
  administrator ubus credential. It performs a read-only authenticated probe
  and returns model/class/firmware/radios/ports,
  `functions_supported|recommended|unknown`, `switch_mode`, and nullable
  `gateway_evidence.active_wan_default_route|lan_dhcp_enabled`. It never opens
  SSH, bootstraps the controller, installs a package or writes inventory;
- inspect/adopt resolves a hostname once per workflow and pins the chosen IP
  across HTTP, SSH and verification. Plain HTTP persists that IP; HTTPS may
  retain the name only with the observed certificate identity pin;
- adopt accepts a non-empty subset such as
  `functions:["gateway","ap","switch"]`. The array is canonicalized;
  omission alone invokes the legacy `role`
  bundle. Explicit `null`, `[]`, unknown values and corrupt stored state fail
  closed. Responses, inventory and previews return both the canonical primary
  `role` and authoritative `functions`;
- the Gateway inventory check and the entire external bootstrap through device
  row commit share a global adoption slot. A concurrent second Gateway is
  refused before device contact; AP-only remains valid for the first device;
- an un-adopt report may carry `cleanup_commands`, generated from validated
  residue identifiers/paths. Preserve the structured report on non-2xx and
  forced-removal paths instead of replacing it with a bare error;
- normal un-adopt inventory deletion requires a proved config phase
  (`config_revert_complete`) and no login/ACL footprint. If the controller
  session is missing while owned sections exist, or delete/commit cannot be
  proved, phase 2 is skipped and the row/ledger remain unless Force was
  explicitly requested;
- a network's `dhcp` object is `{enabled,start,limit,leasetime}`. Omission from
  an older client preserves the stored policy; presence is a complete explicit
  write and incomplete objects are rejected.

---

## 9. UI implementation notes

Structure mirrors UI-SPEC's navigation map; one route per screen, shared
components: `DataGrid` (TanStack core + virtualizer, column persistence in
localStorage), `TimeChart` (uPlot wrapper: crosshair, min/max band rendering,
rollup switching keyed to the range selector), `SlideOver`, `FilterRail`
(options with live counts from aggregate endpoints — never counted client-side
from the loaded page).

Design tokens: copy the CSS custom-property block from UI-SPEC §3 verbatim into
`ui/src/lib/tokens.css`. The categorical palette there is validated — do not
substitute hues without re-running the palette validator.

Dashboard chart: implement option **B** (stacked panels, shared crosshair) as
default with the "Combined axes" toggle rendering option A, per UI-SPEC §4's
dual-axis discussion.

Bundle budget enforcement: `tools/budget_check.sh` runs `vite build`, gzips,
fails CI over 1.5 MB total. Fonts: system stack only. Icons: a small,
project-owned inline SVG set with one stroke language; no Unicode/icon-font or
raster navigation glyphs. Navigation icons render at 22–24 px in controls at
least 44 px square and retain accessible names in collapsed mode.

---

## 10. Security implementation

- Secret store: controller-created per-device `user:pass`, WLAN/mesh keys, and
  secret-derived ownership verifiers are sealed with XChaCha20-Poly1305 under a
  random data key. Argon2id derives the key-encryption key that unwraps that
  data key from `keyring.json`; a mode-0600 passphrase file supports unattended
  boot as an explicit tradeoff. The device administrator password and optional
  SSH private key supplied to adopt/un-adopt are transaction inputs only. On
  adoption the key authenticates SSH in preference to the password, while the
  password still authenticates ubus; on un-adopt either administrator
  credential is used only for SSH cleanup. Neither is stored, echoed in a
  response, or logged.
- Keyring creation is atomic and no-clobber. Startup refuses to create a new
  keyring beside an existing non-empty database; schema 14 also stores a sealed
  key check so an unrelated restored database/keyring pair is rejected before
  WAL mode, schema DDL, or any other mutation. Back up and restore the database
  and keyring together.
- The generated device ACL (`deploy/acl/oonfeewrt.json`) grants: `uci`, `system`
  (board/info), `file` (read/stat/list + **exec restricted to an explicit
  command list**), `iwinfo` (all read), `network.*` (status/dump), `luci-rpc`
  (all getters), `session` (access/destroy). Review this file like code; it is
  the blast radius.
- **rpcd's ACL grammar is not what it looks like — verified on hardware
  2026-08-13.** Three facts the file must be written around:
  - `uci` is granted in **two independent dimensions**, and rpcd requires both
    to match. An access-group lists methods under `"ubus": {"uci": [...]}` *and*
    config names under a sibling top-level `"uci": [...]`. Granting "all uci
    methods" without naming the configs grants nothing.
  - `file.exec` is granted **per exact command line**, not per binary — stock
    groups carry entries like `"/sbin/ip -[46] neigh show"`. An "explicit binary
    list" is not expressible; every argv pattern must be enumerated.
  - A login with `list read '*'` is **not** a superuser. `*` expands over the
    access-groups defined in `/usr/share/rpcd/acl.d/`, so any method no group
    names is unreachable no matter who authenticates. Stock OpenWrt grants
    **zero** access to `uci.configs`, `uci.rollback` and `iwinfo.devices` — all
    three are ours to grant.
  - `file.exec` resolves the command to its **absolute path before matching**,
    so a caller may pass a bare name (`iw dev`) and still match an absolute
    grant (`/usr/sbin/iw dev`) — but a *grant* written as a bare name matches
    nothing.
  - File paths are **canonicalised and re-authorized**, and `*` **crosses `/`**.
    Together these make file grants behave the opposite of how they read: a
    grant on `/sys/class/net/*` never fires (those entries are symlinks into
    `/sys/devices`), while widening it to `/sys/devices/*` hands over that
    entire subtree. Prefer a ubus object to a file grant wherever the data
    exists in both — DSA presence, for instance, comes from
    `luci-rpc.getNetworkDevices` (`devtype: "dsa"`), which the poll already
    fetches, so it needs no filesystem grant at all. Read-only legacy switch
    and topology fallbacks use exact `swconfig list`, `swconfig dev * show`,
    and `brctl showmacs *` grants; no arbitrary BusyBox applet or shell is
    exposed.
  - The second authorization is independently load-bearing. dnsmasq advertises
    `/var/etc/dnsmasq.conf.*`, whose canonical target is
    `/tmp/etc/dnsmasq.conf.*`. Granting only the service path made post-apply
    DHCP health fail with `PERMISSION_DENIED` on real hardware. The ACL
    therefore grants **read only** on both patterns and tests reject write
    permission on either.
  - Isolated-BSS health has the same two-path requirement. It requests
    `/sys/class/net/*/brport/isolated` and separately grants the canonical
    `/sys/devices/*/brport/isolated` target, read-only; tests reject write access
    to either. A value of `1` proves same-AP cross-BSS bridge-port isolation. It
    does not replace the two-client same-BSS acceptance test or implement
    multi-AP L2 isolation.

**Verified end to end 2026-08-13** with a real dedicated login
(`rpcd.oonfeewrt`, SHA-512 crypt password, `list read/write 'oonfeewrt'`): the
session carries the `oonfeewrt` access-group *alone* (root's `*` carries ~20),
every call the controller makes succeeds, and every out-of-scope call is
refused — arbitrary shell, the `/bin/busybox <applet>` multicall escape,
`/etc/shadow`, rewriting root's password, `rc.init`, `system.reboot`, and
`luci.getConntrackList`. Test a candidate ACL against both halves: sufficient
*and* minimal. Testing as root proves neither, and will mask a broken grant,
because root's wildcard silently supplies what the file forgot.

**Package installation is deliberately outside the controller credential.** The
ACL grants `apk list --installed` (capability discovery) but not `apk add`, and
the scoped login is refused it — verified. This is a real constraint on the
tier-2 opt-in flow in DEVICE-BUDGET §5, not an oversight: a package's install
scripts run as root, so `apk add *` is indistinguishable from arbitrary root
code execution, in the one file we call the blast radius. The controller may
therefore *detect* that `nlbwmon`/`lldpd`/`usteer` are missing and show a
disabled-by-default, per-feature install option, but installing requires an
explicit selection plus the operator credential rather than the controller's
own—the same split that un-adoption needs. Adoption, polling and validation
never select it automatically. Anything that widens the device's attack surface
should cost an operator credential; anything that only reads or reconciles
owned UCI should not. The LLDP flow runs the package manager's simulation,
hash-binds that exact output, and requires a second acknowledgement before
`apk -U add lldpd` (OpenWrt 25.12+) or `opkg update && opkg install lldpd`
(24.10 and older). It records the complete pre-install package list, the actual
post-install diff, and the prior `lldpd` enabled/running state. After installation,
a credentialed read-only plan derives only non-wireless physical members of
`br-lan`; a separate unchecked acknowledgement can replace only
`lldpd.config.interface`, commit only `lldpd`, restart only that service, wait
boundedly for its control socket, and verify every planned runtime interface.
The ledger retains the exact baseline/applied UCI exports and configured
interfaces. Read-only diagnostics preserve that durable state in their response.
Rollback refuses external UCI drift, restores and verifies the exact baseline,
removes the complete recorded added-package set, and independently verifies the
final package inventory and `lldpd` enabled/running state before deleting the
ledger. A pre-existing package is retained and its prior service state restored.
An installing/error record survives crashes and blocks un-adoption instead of
silently losing ownership. Rejected credentialed plan requests clear transient
password/private-key fields before retry; successful plan/apply reuse remains
limited to the open review.
- Audit: every changeset stores author, timestamp, full diff, per-device
  outcome. Every login and failed login is an event.
- No default credentials anywhere. First run generates the admin account
  interactively and prints nothing secret to logs. Administrator creation is
  accepted only through `localhost` or a literal controller IP; DNS names are
  allowed for login after setup but cannot claim an unconfigured controller.
- Adoption deliberately warns rather than refuses when a device account accepts
  any password. The warning must give both exact remedies (`ssh -t
  'user@host' passwd` and LuCI `System → Administration → Router Password`) and
  state that the controller will not mutate `/etc/shadow` or set the device
  password.

---

## 11. Build, packaging, deployment (Docker, D7)

```
make ui        # vite build → ui/dist (precompressed .gz alongside)
make build     # go build (host arch) with embedded ui/dist — for dev
make image     # docker buildx: linux/amd64 + linux/arm64, FROM scratch
make check     # unit + integration-vs-mock + budget_check
```

`deploy/Dockerfile` is multi-stage: node stage builds the UI, Go stage builds
the stripped static binary with the UI embedded, final stage is `FROM scratch`
plus CA certificates. One process, PID 1, no shell or package manager in the
image. Named timezone data is not copied because the controller has no
named-timezone runtime contract.

Runtime contract (what `deploy/docker-compose.yml` encodes):

| Aspect | Value |
|---|---|
| Data | single volume mounted at `/data` (`oonfeewrt.db`, paired `keyring.json`, and retained `.oonfeewrt-recovery` safety artifacts; all remain sensitive) |
| Network | bridge default with add-by-IP adoption; Linux host networking is an explicit full-discovery opt-in |
| Ports | loopback `127.0.0.1:8080:8080` mapping by default; use a trusted reverse proxy for TLS (no native TLS listener in this release) |
| Config | env vars `OONFEE_DATA_DIR`, `OONFEE_LISTEN`, `OONFEE_PASSPHRASE_FILE` (secrets via file, never env value) |
| Health | `GET /healthz` (no auth, no body beyond `ok`) wired as the compose healthcheck |
| Upgrade | pull new image, restart; schema migrates forward on boot; downgrade = restore volume backup |
| Backup | schema-19 source exports a native authenticated-encryption `.oowrtbak` containing a consistent database/key pair, and restores only through bounded preview plus plan-bound confirmation. Keep `/data` and the same `OONFEE_PASSPHRASE_FILE` across ordinary container restarts; the current runtime passphrase is verified before a prepared keyring is created. Successful restore writes `<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak`; after audit acknowledgement, fixed-shape retention targets three newest-first while always preserving active restore references. |

Graceful shutdown on SIGTERM: finish (or abandon pre-APPLY) any in-flight
changeset, flush completed telemetry buckets, discard the in-progress RAM
bucket rather than persist a partial canonical row, checkpoint WAL, exit. An apply that has
reached APPLY continues to CONFIRM_POLL — never leave a device with a rollback
timer running because the container restarted, so shutdown blocks (bounded by
the rollback timeout) until confirm resolves one way or the other.

---

## 12. Testing strategy

| Layer | Harness |
|---|---|
| render/ | golden-file unit tests; property test: render is deterministic and idempotent |
| applyengine/ | table-driven state machine tests against `tools/mock_ubus.py`, including: rollback fires, confirm-poll survives connection death, dirty-delta abort, health-fail reversal |
| ubus/ | mock server; session-expiry replay; TOFU mismatch |
| collector/ | simulated clock; ring→rollup correctness incl. counter wrap |
| api/ | httptest against a seeded store |
| end-to-end | `make check` boots mock_ubus, runs adopt→render→apply→collect→query through the real daemon |
| hardware | `tools/probe.py` report imported as a capability fixture; the 60-minute budget harness from DEVICE-BUDGET §7 run against a measured class-C device per release (passed on Archer C6 v2, 2026-08-18) |

`tools/mock_ubus.py` is the contract fixture: it models staged-vs-committed UCI
state faithfully (set stages; apply commits+snapshots+arms timer; confirm
cancels; timer restores). It deliberately models the WRT3200ACM's mwlwifi gap
(valid active_time/busy_time, but uninitialised rx_time/tx_time and unsigned
noise from iwinfo.survey against signed noise from iwinfo.info) so capability
gating and the noise-source rule are exercised in CI, not discovered
in the field.

---

## 13. Milestones

Strictly ordered; each has a mechanical "done when" a build session can verify.

**M0 — Harness.** mock_ubus + `internal/ubus` + CI skeleton.
*Done when:* `go test ./internal/ubus/...` passes against the mock, including
batch fallback and session-expiry replay.

**M1 — Adoption + capability.** discover, login, probe, ACL write, credential
seal, TOFU pin, un-adopt.
*Done when:* inspect proves read-only/no-inventory behavior; concurrent Gateway
adoptions contact at most one device; adopt→un-adopt against the mock leaves
mock state byte-identical to pre-adoption; capability JSON matches the fixture;
an unreadable ownership ledger disables both un-adopt actions; and a partial
cleanup returns safe, exact manual commands without exposing either operator
credential.

**M2 — Apply engine.** render (examples 1+2), diff, full state machine.
*Done when:* the deliberate-rollback integration test passes: a staged change
with confirm withheld ends VERIFY_REVERTED with pre-apply state intact — and
the same test passes on real hardware via a probe.py cross-check.

**M3 — Read-only fleet.** collector, rollups, Dashboard + Devices + Clients
screens, DataGrid + TimeChart, WS live stats.
*Done when:* 24 h simulated at 40 clients stays within RAM budget; charts
switch rollup resolution per range; budget_check green.

**M4 — Site WiFi.** WLANs, AP groups, pending-changes UI, apply flow with
traversal warnings, usteer/dawn config rendering.
*Done when:* the Phase-2 proof from ROADMAP (one SSID edit → N devices, foreign
config untouched) passes as an automated end-to-end test on the mock fleet.

**M5 — Networks/zones/policy.** Example-2 rendering generalized, zone matrix
UI, client block/rate-limit via nftables sets.
*Done when:* guest-VLAN-in-under-a-minute proof passes end-to-end.

Current partial M5 state (2026-08-20): DHCP enablement, start, limit and lease
time are configurable end to end, with legacy defaults and UI/API/model/render
validation. Devices without Gateway do not render DHCP; VLAN 0/1 management
addressing stays foreign; a foreign DHCP server or firewall zone blocks rather
than being modified; and the controller never makes a flat bridge VLAN-aware.
After the operator explicitly converted the live Gateway + AP + Switch WRT to
management on `br-lan.1`, the browser applied the controller-owned VLAN2,
static interface, DHCP and multi-network firewall-zone LIST forms. The C6 was a
truthful no-op because its legacy switch is observe-only.

The schema-12 directional forwarding subset is built: active managed source
zones expose explicit `forward_to` destinations, absence preserves the legacy
WAN-only edge, explicit empty blocks all modeled forwarding, and reverse
initiation is independent. Model/store/API/render tests cover legacy no-diff,
one-way inter-zone policy, block-WAN, malformed/downgrade boundaries, foreign
forwarding/rule/DNAT conflicts and rename/orphan safety. The UI ships an
editable Zone Matrix. Schema 15 extends the source-only Master Table with
explicit IPv4 firewall rules, port forwards, static routes and client block,
fixed-IP and group intent. Its partial Object Manager compiles unsaved,
inspectable `Secure` IPv4-reject drafts for device/group/network objects and
static routes for network objects; per-device/group routing, QoS and application
outcomes return explicit gates. Saving a chosen draft changes desired state
only and still requires Preview/Apply. That schema-15 expansion has source
tests but no live proof yet. The authenticated hardware pass proved client DHCP/DNS/WAN,
directional WAN block/restore with DHCP/DNS retained, DHCP disable, and a
`50`–`59`/`1h` custom pool issuing `.54` for exactly 3600 seconds. A later
same-BSS run put two physical iPhones on one isolated WRT BSS; both proved
distinct DHCP, fixed-IP WAN, DNS plus WAN and denial to a known-live LAN HTTP
listener. UCI held `isolate=1` and `bridge_isolate=1`, and sysfs reported
`isolated=1`. Reciprocal raw Safari peer-IP failures had no known-live listener
or positive control, so literal bidirectional peer data-plane isolation remains
open. The confirmed
cleanup removed the proof WLAN, retained the operator-created Guest VLAN3 and
ended with a zero-change fleet plan (STATUS §5bk).

**M6 — Observability + topology + logs.** Correlated client timeline,
survey/station derived metrics, Radios screen, topology inference, event ingest
and Logs screen.
*Done when:* selecting an incident timestamp updates client, AP and site-health
evidence from one query; Radios shows interference/airtime for mt76 fixture
radios and correctly *omits* them for the mwlwifi fixture radio.

Historical Phase-4 live status (2026-08-22): the schema-17 store, producers, bounded
REST APIs and React screens are implemented. The joined timeline is rollup-only
and gap-aware; `wifi-v1` is fixed/all-or-null; topology preserves source
ambiguity and validity intervals; Radios uses explicit scans and freshness;
Logs distinguishes observed-empty from missing/stale/gapped producer coverage.
The WebSocket remains `device.stats` only. The live lab database is schema 17.
Both routers have separately opted-in, controller-recorded LLDP installations
and physical-interface configurations. Their runtime diagnostics and current
source state produce one measured AP-to-gateway edge. Peer rediscovery closed a
transient reciprocal interval after capability changes, but a v38 restart
exposed the same reverse edge briefly. Source-aware reconciliation now withholds
a new managed-device LLDP edge until its claimed parent has a proven Internet-
root path and suppresses a reciprocal direction that conflicts with rooted
parent/child depth. V39 first yielded five nodes, four links and no reciprocal
edge on the signed-in direct `/topology` render. V40 retained that graph; after a
complete poll, gateway association coverage was observed and AP coverage was
observed-empty. Only the two BusyBox `brctl showmacs` VLAN ambiguities remain as
current topology gaps. Historical log/topology coverage is still unavailable
rather than inferred; DFS and scan outcomes remain evidence-gated. Persisted RF
scan history is bounded to the newest terminal result per stable radio key by
the normal maintenance transaction.

Flows/DPI: not scheduled. Revisit only after M6 ships and only for capable
hardware, per PARITY-MATRIX.

**M7 / Phase 4.1 — UI polish, controller operations and v0.1.0.** Incremental
SVG navigation and disclosure primitives; freshness-aware Dashboard; bounded
controller-host speed tests; server-side RBAC/accounts/sessions;
redacted diagnostics ZIP; encrypted portable backup/restore; final release
hardening.

The v0.1.0 source includes the navigation/disclosure foundation, schema-18
Dashboard/controller-speed-test slice and schema-19 RBAC/account/session slice
are implemented. The stored-only diagnostics ZIP/API/UI and bounded rotating
controller log sink are implemented. Encrypted native `.oowrtbak` export,
bounded disposable restore preview, plan-bound confirmation, controlled restart,
retained encrypted safety backup, session revocation, and persistent
router-write suppression/resume are implemented. The live controller runs exact
binary version `dev-phase41-live-schema19` at schema 19; its signed-in Dashboard,
Accounts, Diagnostics, Backup & Restore, Devices and Topology routes passed smoke
without browser errors. Gateway-run speed testing is deferred.
The final tag workflow owns the immutable release, isolated restore, vulnerability,
signature, archive, image, and anonymous-verification gates.

*Done when:* a clean immutable release-candidate container can create
least-privilege accounts, run and cancel a clearly sourced controller test with
zero router management/API/SSH calls or writes/installs, generate a checksummed
ZIP containing no seeded secret, export/restore into a second clean container
without applying router state, and pass the RC→final upgrade/rollback gate. Its
traffic still follows the controller host's normal route. Candidate binaries/
images must create SQLite database, WAL and SHM files at mode 0600. Publish those
exact bytes/digest, then anonymously verify the public hashes/digest and clean
startup. A gateway-run speed test is a separate,
default-off official-feed capability with a bound exact plan and rollback; it
is not required for controller-mode completion.

---

## 14. Items resolved by hardware validation

Settled 2026-08-13 by `probe.py --write-tests` against the real WRT3200ACM
(OpenWrt 25.12.5 r33051, mvebu/cortexa9, class A). Raw findings in the probe's
`--json` output.

1. **`uci.apply` across multiple configs is all-or-nothing.** Two configs staged
   with deltas, one `apply {rollback:true}`: both committed together and both
   reverted together when the timer expired. STAGE may batch across configs —
   they share a single rollback transaction.
2. **`uci.confirm` requires the same ubus session that applied.** Confirm from a
   second authorized session returns `PERMISSION_DENIED` (6) and the change
   still reverts. CONFIRM_POLL must therefore hold the applying session open and
   confirm through it — a fresh-connection confirm strategy cannot work. Note
   the consequence: if the controller loses its session (restart, crash) it
   *cannot* confirm, and the device reverts. That is the correct safety
   behaviour, but it means the session token must outlive a controller restart
   if we want to confirm across one. Sessions expire in 300 s.
3. **mwlwifi does provide survey data** — the design's assumption that it
   doesn't is wrong. `iwinfo.survey` works natively on both radios (no
   `file.exec`, no process spawn) and returns `active_time` + `busy_time`, so
   **channel utilization** is computable on this hardware — from the DELTAS of
   those counters, see §14.7. That is
   *not* the same as the interference and airtime columns: both need
   `rx_time`/`tx_time` and so stay capability-gated — see PARITY-MATRIX, where
   they are 🟠 rather than 🟢. Two traps: `rx_time`/`tx_time` are uninitialised
   (`iw` shows a garbage u64, ~1.4e19), and `iwinfo.survey` reports `noise`
   **unsigned** (161) while `iwinfo.info` reports it correctly signed (−95) —
   always take noise from `iwinfo.info`. **The `iwinfo.assoclist` field surface
   is now captured** against two real associated stations — 21 keys, with the
   per-direction counters **nested** rather than flat:

   ```
   mac, signal, signal_avg, noise, inactive, connected_time, thr,
   authorized, authenticated, preamble, wme, mfp, tdls, mesh *,
   rx: {packets, bytes, rate, mcs, mhz, ht, vht, he, eht, short_gi,
        40mhz, drop_misc}
   tx: {packets, bytes, rate, mcs, mhz, ht, vht, he, eht, short_gi,
        40mhz, retries, failed}
   ```

   Everything the Radios and Client Devices columns need is here, including
   `tx.retries`/`tx.failed`, so **`iw station dump` is not required at all** —
   don't grant it. Note the nesting: probing for a flat `tx_retries` finds
   nothing and wrongly concludes a process spawn is needed.

   **A full Client Devices row is buildable from one batched request** —
   measured at **100 ms** for 7 calls covering both radios: name and IP from
   `luci-rpc.getHostHints` + `getDHCPLeases` (both joined cleanly on MAC),
   signal/PHY-rate/retry-%/connected-time from `assoclist`, and 24 h volume
   from `nlbw -c json`.

   **But the per-station `noise` field is unstable on mwlwifi** — sampled at
   3 s intervals it read −66, −95, −95, −58, −95, −70, a 37 dB swing. SNR
   computed per sample would visibly flail, so smooth it or show RSSI alone.
   This is the third mwlwifi entry on the quirk list in UI-SPEC §7, and the one
   that best illustrates why presence-probing is insufficient: every individual
   reading is well-formed and plausible.
4. **JSON-RPC array batching works** on this uhttpd build — a batch of 3 was
   accepted and returned 3 responses.
5. **The noise floor is a per-radio capability, and switching source does not
   rescue it.** Measured 2026-08-13 over 20 samples ~0.35 s apart, on one device
   running one driver:

   | Radio | `iwinfo.info` spread | `iwinfo.survey` spread |
   |---|---|---|
   | 5 GHz (`phy0-ap0`) | 7 dB | 5 dB |
   | 2.4 GHz (`phy1-ap0`) | **42 dB** | **46 dB** |

   The 2.4 GHz value sat at −95 dBm and jumped to −49…−71 dBm sporadically.
   Channel busy time does not explain it: busy averaged 82 % during the
   excursions against 76 % otherwise, with the two ranges fully overlapping. So
   the earlier advice — "`iwinfo.survey` reports noise unsigned, read it from
   `iwinfo.info` instead" — is correct about the encoding and silent about
   trust, which is the part that matters for rendering. Whether the excursions
   are a driver defect or real bursts on a congested band is unsettled and does
   not change the conclusion.

   `capability.checkNoiseStability` re-reads both sources and records
   `Radio.NoiseStable` per radio. It is **asymmetric**: a disagreement proves
   the value moves, agreement proves nothing. On one hardware run the survey
   pair agreed while the `iwinfo.info` pair jumped 45 dB, same radio, same
   minute — so `Present` means "not caught misbehaving", never "verified
   stable".
6. **`iwinfo.survey`'s `busy_time` and `active_time` are counters, and they do
   not share an epoch.** Both advance correctly — `active_time` tracked the wall
   clock to 99% over a 10-second window — but their absolute values are not
   comparable. Measured 2026-08-13:

   | Radio | absolute busy/active | Δbusy/Δactive | independent check |
   |---|---|---|---|
   | 5 GHz (`phy0-ap0`) | **1354.7 %** | 1.7 % | — |
   | 2.4 GHz (`phy1-ap0`) | **25.9 %** | 73.3 % | hostapd BSS load: 70 % |

   The 5 GHz row is the harmless case: 1354% is obviously broken and someone
   would catch it. The 2.4 GHz row is the dangerous one — 25.9% is a perfectly
   reasonable-looking utilization figure that is wrong by a factor of three, and
   nothing about it invites a second look. hostapd's `airtime.utilization` on
   the same radio at the same moment is what settled which number was real.

   This corrects a claim that was asserted as verified in ARCHITECTURE §5,
   PARITY-MATRIX and this document: "Utilization = busy / active — verified good
   on mwlwifi". The *fields* were verified good. The *formula* was never tested
   against a radio whose counters had drifted apart, because on a freshly booted
   device they have not.

   `collector.Survey` therefore offers no percentage method at all — the
   arithmetic lives in `internal/telemetry` beside the other counter-derived
   rates, where the previous reading is in hand. A single survey read produces
   no utilization sample, exactly like a single interface byte counter produces
   no throughput.
7. **Adoption cannot bootstrap over ubus. Root over ubus is not root.**
   Measured 2026-08-14 on stock OpenWrt 25.12.5, signed in as root:

   | call | result |
   |---|---|
   | `uci.get rpcd` | status 6 — refused |
   | `uci.set rpcd.<login>` | status 6 — refused |
   | `file.write /usr/share/rpcd/acl.d/*.json` | status 6 — refused |
   | `file.read /etc/rc.local` | status 0 — granted |

   rpcd's own ACL files bound what `/ubus` can reach, and stock OpenWrt grants
   write access to neither the `rpcd` config nor the ACL directory. That is a
   deliberate security property — it is what stops a compromised LuCI session
   widening its own permissions — and it means the design's "written via
   `file.write`" was impossible, not merely untested. No access group on the
   device grants it, and adding one would require writing to the directory we
   cannot write to.

   The footprint therefore arrives over **SSH, twice in a device's lifetime**:
   adoption and un-adoption. Everything else stays on ubus. Device-side
   assumptions, checked on that build rather than assumed:

   - **no `base64`**, so content is piped to `cat` over the SSH session's
     stdin — which also means it is never a shell argument and needs no
     quoting;
   - **no `sftp-server`**, so scp and sftp are unavailable;
   - `uci`, `cat`, `mktemp` and `sha256sum` are present, and the write is
     verified by hash rather than assumed from a zero exit.

   Verified end to end on hardware: the installed ACL's sha256 matched the
   source byte for byte, the created login authenticated, and re-adoption was
   refused.
8. **A stock device with no root password accepts anything.** The same device
   authenticated `root` over ubus with an empty password, the correct password
   and a deliberately wrong one, and over SSH with the `none` method. rpcd's
   `$p$root` resolves against `/etc/shadow`, and an empty entry matches
   everything. Adoption now probes for this with one deliberately-wrong login
   and surfaces it as a warning — not a refusal, since an operator may knowingly
   run that way on a trusted LAN, but the credential they typed proved nothing
   and they should know it.
9. **The capability probe must run on the CONTROLLER's session, after its ACL
   is installed — not on the operator's, first.** The registry gates what every
   screen renders, and screens render from what the controller can reach, so a
   probe answering "what can root see" answers the wrong question.

   It also gets a different answer. Stock OpenWrt grants **zero** access to
   `iwinfo.devices` (§10), so on a genuinely fresh device a probe run before the
   ACL exists cannot enumerate the radios at all. Measured 2026-08-14 by
   adopting a device whose footprint had been fully removed first: the probe
   reported `iwinfo-survey`, `hostapd-control`, `per-client-accounting` and
   `airtime-split` as **undetermined**, and the identical calls returned status
   0 the moment the ACL landed. After reordering, the same device records all
   seven features.

   Every earlier run missed this because a leftover ACL file was already on
   disk, which root's `list read '*'` expanded over — the bug was only reachable
   on a device that had genuinely never been adopted, which is precisely the
   case every real user hits first.
10. **The two poll tiers are worth the complexity — measured through the real
   collector, under the scoped credential.** Best of five polls each, both
   batched into a single request:

   | Tier | Calls | Wall time |
   |---|---|---|
   | Baseline (`system.info`, `network.device`, `hostapd.*`) | 7 | **8 ms** |
   | Focused (adds `iwinfo.assoclist` + `iwinfo.survey` per radio) | 11 | **116 ms** |

   A 14× difference for four extra calls, which is the whole argument for
   polling `iwinfo` only while somebody is looking. It also confirms the cheap
   sources: two radios' worth of SSID, channel, client count and BSS load cost
   single-digit milliseconds through `hostapd.<iface>`.

Also measured, and worth carrying into the design:

- **Rollback genuinely reverts**, but a controller **cannot observe its own
  rollback**. rpcd restores `/etc/config` while leaving the applying session's
  staged delta in place, and session-scoped `uci.get` overlays that delta. After
  a rollback the applying session still reads the value it failed to set; a
  fresh session reads the reverted value. Verification after apply must use a
  second session. (Closing the TCP connection is not enough — the session token,
  not the connection, scopes the delta.)
- **Transport is not a bottleneck on class A**: 1.2 ms keep-alive vs 1.7 ms
  fresh-connection over plain HTTP. TLS adds ~15 ms per handshake (TLS 1.3,
  and OpenWrt 25.12 already ships an **ECDSA P-256** cert, so the "consider
  ECDSA" note in ARCHITECTURE is already satisfied) — far under the 120 ms
  threshold that would force persistent connections. The cert is self-signed
  (`CN=OpenWrt`), so the controller must pin it, not chain-validate, and must
  expect it to change on reflash.
- **The full probe passes over HTTPS**, write-tests included, so nothing in the
  design depends on plain HTTP. Measured on class A: keep-alive request 1.3 ms
  vs fresh connection 17.1 ms, i.e. **15.8 ms of TLS setup per new connection**,
  and device CPU during a focused poll rises from ~0.75 % to **1.18 %**. TLS
  roughly doubles the poll's CPU cost on hardware that has cycles to spare —
  which is the concrete argument behind DEVICE-BUDGET §3.1 for class C, where
  there is no crypto acceleration. Cert: TLS 1.3, `TLS_AES_256_GCM_SHA384`,
  DER SHA-256 recorded in the JSON report for TOFU pinning.
- **uhttpd's idle keep-alive is exactly 20 s** (survives 19 s, dropped at 21 s).
  The focused tier at 5–10 s therefore reuses connections; the 60 s baseline
  tier **never** does, and pays a full handshake every poll. Budget accordingly
  rather than assuming keep-alive helps everywhere.
- **JSON-RPC batching scales far past what the design needs**: 550 calls in one
  request (65 KB) were accepted, with per-call cost flat at ~0.5 ms from ~10
  calls upward. Chunk on request bytes, not call count.
- **Software flow offloading does NOT break per-client accounting.** Measured
  with the flowtable active and a flow confirmed in the fast path
  (`[OFFLOAD]`): conntrack byte counters stayed complete (102 % of transferred
  bytes, the excess being headers and the reverse direction) both with and
  without offload, on kernel 6.12 + nftables flowtables. The tradeoff in the
  README applies to **hardware** offload, which mvebu does not implement — so
  it remains untested and must be scoped to class B/C rather than stated
  generally. Note also `nf_conntrack_acct` is already `1` by default.
- **The whole `network.wireless` object is unreachable over rpcd** — `status`,
  `up` and `down` all return `INVALID_ARGUMENT` (2) through `/ubus` at any
  argument, while working fine on the local ubus socket. rpcd injects
  `ubus_rpc_session` into the args and netifd's strict policy rejects the
  unknown field. **Do not grant `network.wireless` anything**; the grant is
  inert and only widens the stated blast radius. Radio state comes from
  `uci get wireless` + `iwinfo` + `hostapd.*`, and enable/disable is
  `uci set wireless.radioN.disabled` followed by `uci.apply` — verified working.

  This is a *class* of hazard, not one method, so the reachable surface was
  mapped explicitly:

  | netifd method | Through rpcd |
  |---|---|
  | `network.reload` | ✅ |
  | `network.get_proto_handlers` | ✅ |
  | `network.interface.dump` / `.status` | ✅ |
  | `network.device.status` (with or without `name`) | ✅ |
  | `network.wireless.status` / `.up` / `.down` / `.reconf` | ❌ status 2 |

  Test any new netifd call through `/ubus` before designing on it — a local
  `ubus call` proving it works tells you nothing about the rpcd path.
- **`dhcp.ipv4leases` does not exist on this build** — the `dhcp` object exposes
  only `ipv6leases`, `ipv6ra` and `add_lease`. Use `luci-rpc.getDHCPLeases`,
  which returns both families.
- **Device CPU is not the constraint on class A**: 0.65 % idle, 0.72 % with a
  full 13-call focused poll every 5 s, 9.8 % only when polling back-to-back with
  no delay. **Zero flash writes** were observed across sustained polling
  (`/overlay` used and mtd/ubi write counters both unchanged), so the
  zero-write claim holds.
- **`uci.add` cannot create a config that does not exist** — it returns
  `NOT_FOUND` (4). Anything creating a new UCI config must create the file
  first; this is why the probe's scratch config needs `touch
  /etc/config/oonfeewrt_probe` as a prep step.
- **`uci.rollback` reverts immediately**, without waiting out the timer — the
  right primitive behind a "revert now" control, and worth preferring to a long
  stall when the operator has already decided. It is **session-bound exactly
  like `uci.confirm`**: a second session calling it gets `PERMISSION_DENIED` (6)
  and the change stays applied until its own timer expires. So the applying
  session is the only party that can resolve an armed apply *either way*.
- **Two independent timeouts, and confusing them costs a re-login every poll.**
  The uhttpd TCP keep-alive is **20 s**; the ubus session idle timer is **300 s**
  and is *refreshed by any call*. Measured directly: a session called once every
  60 s stayed valid through t+360 s, while a session left untouched was dead at
  t+360 s with a JSON-RPC `-32002`. So at the 60 s baseline cadence the
  controller pays a fresh TCP connection every poll but **never** needs to
  re-authenticate — the token outlives the socket by 15×. Only a device polled
  more slowly than 300 s, or one quiesced during someone else's apply, needs a
  re-login on the next contact.
- **`uci.apply` is globally serialised, and refuses a second armed apply with
  status `6`.** With one session's rollback timer running, a *different*
  session's `uci.apply {rollback:true}` returned `PERMISSION_DENIED` and did
  nothing; the first session's change then reverted normally on its own timer.
  Good news for safety — two controllers, or a controller and LuCI, cannot
  clobber each other's rollback snapshot. But note the ambiguity it creates:
  **status 6 from `uci.apply` means "an apply is already armed", not an
  authorization failure.** Retry after the window; do not surface it as a
  permissions error, and do not let it trip the ACL-error path.
- **⚠️ While a rollback is armed, you cannot get a second session at all.**
  Measured: with a timer running, `session.login` returns the **applying
  session's token** to any caller, on any connection — six logins with no timer
  armed gave six distinct tokens, but one armed timer made a fresh login return
  the applier's. This is deliberate on the device's part (it is how a controller
  that lost its connection can still confirm), and it has two sharp
  consequences:
  1. **A health check inside the window cannot use an independent session**, so
     it must read *runtime* state — `network.interface`, `iwinfo`, `hostapd`, an
     exec probe — and never `uci.get`, which is overlaid with the applying
     session's own staged delta and would bless a change that is not really
     applied.
  2. **Destroying "the verification session" destroys the applying one.** Doing
     exactly that turned a healthy apply into a revert: the applier's next
     `uci.confirm` returned `-32002` and the device restored itself on schedule.
     Any client-side session helper must refuse to destroy a session whose token
     matches its parent's.

  After the window resolves, logins return fresh tokens again — so revert
  *verification* both can and must use a genuinely fresh session.
- **⚠️ An armed rollback does NOT survive an rpcd restart.** Applied with a 45 s
  timer, then restarted rpcd mid-window: the change was still on disk 75 s later
  and never reverted. The timer lives only in the running rpcd process, so a
  restart — or a crash, or an ACL reinstall — **silently converts "unconfirmed,
  will revert" into "permanently applied"**. Combined with the fact that an rpcd
  restart also destroys every session, a restart inside the confirmation window
  is doubly bad: the controller loses the ability to confirm *and* the device
  loses the ability to revert, yet the change stays. The engine must therefore
  treat "confirm failed" as *"state unknown"* rather than *"reverted"*: re-read
  from a fresh session and, if the change is still present, actively reverse it
  by applying the previous model. Never assume the device cleaned up for you.
- **An rpcd restart destroys every session.** Anything that reinstalls the ACL
  file or edits `/etc/config/rpcd` invalidates the controller's token, so
  adoption and ACL updates must expect to re-login immediately afterwards — and
  must never be scheduled while a confirmation window is open, since the
  applying token cannot survive it and the change would revert.
- **Staged deltas are session-private**, confirmed directly: with one session
  holding an uncommitted `uci.set`, that session reads the staged value while a
  concurrent session reads the committed one. Two controllers (or a controller
  and LuCI) can stage independently without seeing each other's work-in-progress.
- **The headline product operation is validated end to end on real hardware.**
  One session staged `option oonfeewrt '1'` onto *both* radios' `wifi-iface`
  sections, applied once with rollback armed, health-checked from a **fresh**
  session (tag present, both SSIDs on air), then confirmed through the
  **applying** session. Both bands took the change together; the on-air SSIDs
  never wavered. This is the README's "change it once, it lands on both bands,
  with rollback" claim, exercised exactly as ARCHITECTURE §4 now specifies.
- **Client disruption depends on *which* options changed, not on applying.**
  Across that whole sequence — apply, confirm, a foreign `uci commit`, and a
  `wifi reload` — both associated stations held `connected_time` of ~1896 s
  unbroken, with no `AP-STA-DISCONNECTED` events. netifd reloads differentially,
  so an apply touching only inert options (ownership tags, metadata, anything
  not requiring a BSS restart) costs clients nothing. Changing the SSID, by
  contrast, *was* observed restarting the BSS. **The UI should therefore warn
  about client disruption per-option, not per-apply** — a blanket "this will
  disconnect clients" banner on every change is both wrong and desensitising.
- **Ownership drift and orphaning are both detectable by re-read.** A human
  editing `ssid` inside a section we own left our `oonfeewrt` tag intact, so the
  section still reads as ours with an unexpected value — detectable by comparing
  against the rendered model. A human *deleting* our tag makes the section read
  as foreign, which is the correct outcome: the reconciler must then leave it
  alone rather than silently reclaim it. Both were verified, and the wireless
  config was restored byte-identical (md5-verified) afterwards.
- **Ownership tagging works as designed.** A `firewall` rule written with
  `option oonfeewrt '1'` keeps the option across commit, apply and an
  `/etc/init.d/firewall reload`; fw4 ignores the unknown option rather than
  erroring. Reading the config back cleanly partitions 1 owned section from 13
  foreign ones, and deleting only the owned section left the section count
  unchanged at 87. The coexistence rule in the README is implementable exactly
  as stated.

### Pre-auth behaviour of the ubus endpoint (measured 2026-08-14)

Both findings came from writing discovery and checking the spec against the
device instead of trusting it. Both contradicted a documented claim.

- **`ubus list` needs no credential.** `{"method":"list","params":["*"]}` with no
  session returns the device's complete object graph — 13,113 bytes and 39
  objects on the reference device. This is stock uhttpd-mod-ubus behaviour, not
  something adoption enables, and it is what discovery fingerprints on. It also
  carries usable pre-auth structure: `hostapd.phy0-ap0` / `hostapd.phy1-ap0`
  give the number of radios with a BSS up (count distinct **PHYs**, not BSSes —
  three SSIDs on one radio is one radio), `network.interface.wan` marks a
  gateway, `dnsmasq` marks a DHCP server.

- **`system.board` is refused pre-auth.** The same null session gets
  `-32002 Access denied`. ARCHITECTURE §6 previously said a pending device's
  model, MAC and firmware could be read "pre-auth where possible"; they cannot,
  ever. Both the doc and the UI now say the model is unknown until a credential
  is supplied.

- **A `session.login` probe is not safe on a passwordless device.** ARCHITECTURE
  §6 specified probing for a login that fails, on the grounds that the failure
  alone proves rpcd. On a device with no root password the login *succeeds* —
  status 0, a session token, and an ACL set with `uci` write and `file` exec, for
  the password `definitely-not-the-password-9f3a`. A sweep built on that probe
  would mint a root session on every passwordless host in the subnet on every
  scan. Corrected in ARCHITECTURE §6; `internal/discovery` never authenticates,
  and a test asserts the probe issues exactly one request and that it is a
  `list`.

Sweep cost, same day: 508 addresses across two /24s in **4.8 s** at 128
concurrent probes, 12 hosts answering TCP, 1 fingerprinting as OpenWrt. Wall
time is set almost entirely by dead addresses — a live host answers in under
5 ms, a dead one costs the full dial timeout — so it is
`(addresses / workers) x DialTimeout` and nothing else.

### Client scoping on a gateway (measured 2026-08-14)

A gateway's ARP, neighbour and DHCP tables cover every interface, so the client
inventory built from `luci-rpc.getHostHints` mixes the network the device serves
with the network it connects to. On the reference device, of 16 known hosts:

| | count |
|---|---|
| clients of this network (192.0.2.0/24) | **3** — a laptop, a phone, a watch |
| neighbours on the uplink (203.0.113.0/24, behind the WAN port) | **7** |
| no observed IPv4 at all | **4** |
| the device's own interface MACs, already filtered | 2 |

`network.interface dump` returns each logical interface with its IPv4 subnets
and its routes, and costs one more invocation in the existing batch on the
15-minute rediscovery cadence. Measured after adding it: idle **1.00
polls/min**, observed **6.00 req/min**, zero flash writes — identical to before,
with 118 more bytes per poll (9,677 → 9,795).

The upstream interface is the one carrying `0.0.0.0/0`, taken from the routing
table rather than from the interface being named `wan`. On this device `wan` and
the default route coincide, but nothing enforces that: a device bridged onto an
existing network can have the default route on `lan`, and both directions are
unit-tested.

Two storage rules that follow from the refresh cadence:

- **A determination is never overwritten by a non-determination.** Subnets are
  re-read every fifteen minutes and carried forward in between, so a poll before
  the first read reports `unknown` for every host. Letting that overwrite a
  correct classification flickers clients in and out of the default view.
- **A row with no stored scope reads as `unknown`, never `local`.** Defaulting it
  would assert something never measured, and the direction of that error puts
  someone else's hardware in a list captioned "your devices".

### Phase 2's first contact with hardware (2026-08-14)

The site model → render → apply pipeline was built and unit-tested in Phase 0,
and `STATUS.md` recorded it as "mock-verified only". Wiring it to a real device
found three things in the first hour, each invisible to a mock.

- **`uci.get` does not return only strings.** `ReadExisting` decoded the payload
  into `map[string]map[string]string`. On OpenWrt 25.12.5 every UCI *option* is
  a string, but the section metadata is not: `.anonymous` is a JSON bool and
  `.index` a number. Go's decoder failed the whole read with "cannot unmarshal
  bool", so **every device reported as unplannable**. The values are now decoded
  as `any` and coerced, with list options space-joined the way `uci get` renders
  them. Nothing is dropped — a key that vanished would read downstream as "the
  device does not have this option".

- **A new BSS is not up the instant `uci.apply` returns.** The health check read
  hostapd once, immediately, found the SSID absent and let the device revert —
  correctly, by its own logic, but wrongly in fact. Measured: a new BSS appears
  about **1 second** after the reload. The check now polls for up to 20 s, well
  inside the 90 s rollback window, and its error names what the radios *are*
  carrying rather than only what is missing. The revert itself was flawless —
  `/etc/config/wireless` came back byte-identical (same md5) with zero of our
  sections and the operator's own section untouched — which is the mechanism
  working exactly as designed, on a false alarm.

- **`Doc.Plan` emitted a set for every existing section without comparing it.**
  A device that already matched the model still reported "2 changes pending",
  forever, and `DevicePlan.Empty()` could never be true — so a no-op apply would
  still stage, apply and confirm against a device, arming a rollback for
  nothing. Plan now skips a section whose managed values already match. Only the
  keys we write are compared: the device adds defaults of its own and hostapd
  writes state back into these sections, so comparing whole sections would find
  a difference every time and never converge.

**ROADMAP Phase 2's proof, measured.** One WLAN, `sae-mixed`, bands `2g,5g`,
802.11r/k/v on, one AP group, one device:

| | |
|---|---|
| sections rendered from one WLAN | 2 — `oowrt_wlan1_radio0` (5 GHz), `oowrt_wlan1_radio1` (2.4 GHz) |
| mobility domain on each | `e8ee` — identical, derived from site UUID + WLAN id |
| passphrase changed once, landed on | both bands, no per-device work |
| mobility domain after the key change | `e8ee`, unchanged — a key change does not disturb roaming |
| hand-edited foreign section (`human_wlan`) | untouched through apply, re-apply and prune, key intact |
| prune after deleting the WLAN | both our sections removed, the human's kept |
| preview once converged | 0 changes |

The proof's "three APs" remains unmet for want of a **third** device. The
fan-out has since been run across **two** APs and four radios — a second device
was adopted 2026-08-16 — so what is unverified is the step from two to three,
not the idea of fanning out at all. That is the same open hardware item
`STATUS.md` and the README's not-tested table both track, and nothing in the
pipeline is per-device: the render is driven by group membership, and the
mobility domain is derived rather than coordinated precisely so that adding an
AP needs no new mechanism.

### Networks on the device, and the limit that stops them (measured 2026-08-14)

§5's worked example 2 shows a network rendering as a `bridge-vlan`, an
`interface`, a `dhcp` and a firewall `zone` + `forwarding`. All of that is now
built and verified on hardware. The worked example is also **incomplete in a way
that takes the LAN down**, and it took three separate outages of the reference
device to establish exactly why.

**Adding any `bridge-vlan` section switches the bridge to VLAN filtering.** A
stock `br-lan` runs with `vlan_filtering = 0` — one flat domain, `config
interface 'lan'` pointing straight at `br-lan`. The moment a bridge-vlan exists,
filtering comes on and `br-lan` stops being the untagged view of the LAN.

What that looks like, measured:

| observation | value |
|---|---|
| `vlan_filtering` | 0 → 1 |
| `br-lan` state | UP, still holding `192.0.2.1/24` |
| `ip neigh show dev br-lan` | **empty — not one neighbour** |
| apply engine's verdict | `applied — health passed and confirm landed` |
| actual device reachability | gone, until a pre-armed restore ran |

The health check passed because it asks whether the `lan` interface is up, and
it *was* up. The confirm landed. A confirmed, "healthy", network-severing
change. Nothing in the chain reported an error.

**The fix is not ours to apply.** Connectivity survives only if the existing
`lan` interface moves from `br-lan` to `br-lan.1` — verified the same way: with
that one edit, filtering on, `br-lan.1` held the address and the controller's
own host stayed `REACHABLE` in the neighbour table. But `config interface 'lan'`
is the operator's section, and rewriting the interface we reach the device
through, on a device we might then be unable to reach, is exactly what
ARCHITECTURE §0 forbids.

So: **a device whose bridge is not already VLAN-aware is refused, with an
explanation naming the one-time change.** Once an operator has made it, VLANs
are managed from the controller normally — verified end to end: bridge-VLAN,
interface at `203.0.113.1/24`, DHCP, a closed-by-default zone and its forwarding,
all applied and confirmed with the LAN intact, then pruned cleanly, leaving
`/etc/config/{network,firewall,dhcp}` byte-identical to their pre-test md5s.

**A UCI list is not a string with spaces in it.** Found in the same sequence.
`uci.set` accepts `option ports 'lan1:u* lan2:u*'` where UCI wants
`list ports 'lan1:u*'`, stores it without complaint, and netifd then ignores it.
No error at any layer. `render.Section` and `applyengine.Op` now carry `Lists`
separately, staged as JSON arrays, and the section hash covers them — a
bridge-VLAN whose port membership changed but whose options did not is a real
change.

### uci.get and uci.set semantics, measured 2026-08-17

Settled against the Archer C6 (OpenWrt 25.12.5 r33051) over rpcd, staged only
and reverted — `/etc/config` untouched, verified by re-reading the config from a
fresh session afterwards.

**`uci.set` with a JSON array DOES convert an existing string option into a
list.** Set `probe` to `"a b"`, then `uci.set` the same key with `["a","b"]`:
the value reads back as a JSON array. An explicit `uci.delete` of the option
first is therefore *not required* for the conversion.

`render.Doc.Plan` deletes it first anyway, and that stays. The conversion is
measured on one firmware; the failure if a different build does not convert is
the silent one — accepted, stored in a form netifd ignores, apply confirms
healthy — and the delete costs one staged call in the only case that reaches
it, a section an older version of the controller wrote wrong.

**A missing config is status 4; a config the ACL does not grant is status 6.**
Distinct, and `reconcile.isMissingConfig` keys on 4, which is correct. Measured
using `oonfeewrt_probe`, which the ACL grants and which has no file on disk
(status 4), against `ddns`, which is absent from the ACL (status 6). A name that
is neither granted nor present reports 6, because the ACL is consulted first —
so "status 6" alone does not mean the config exists.

**A missing OPTION returns status 0 with an empty body, never 4 or 5.** So does
a missing section, when queried as `{config, section, option}`:

| query | status | body |
|---|---|---|
| existing option | 0 | `{"value":"lan"}` |
| missing option, existing section | 0 | `{}` |
| missing section | 0 | `{}` |
| config not in the ACL | 6 | `{}` |

This makes `applyengine.snapshotPlanned`'s `StatusNotFound`/`StatusNoData`
branch unreachable on this firmware: a missing option is recorded as
`found=true, value=""` rather than `found=false`. The verdict is unaffected —
`planStillApplied` only asks whether the value equals what was written, and an
empty string never equals a non-empty one, so a reverted add still reads as
reverted. The branch is kept as insurance for builds that answer differently,
and `preApply`'s doc no longer claims a `found=false` entry can occur here.

The wider point: **an empty answer from rpcd is not always an error status.**
Any check that distinguishes "absent" by waiting for a non-zero status will
silently never fire against this firmware.

### Function inspection and clean re-adoption, measured 2026-08-18

The schema-11 path was driven against both reference routers, not only the
mock. C6 was cleanly un-adopted first and WRT second; each config phase reverted
exactly two owned WLAN sections and proved the controller login and ACL gone.
The fleet was then empty.

Read-only Inspect measured the WRT's active WAN default route, enabled LAN DHCP
and four DSA LAN ports, so it was adopted first as Gateway + AP + Switch. It
measured no active WAN default route and `dhcp.lan.ignore=1` on the C6, so that
device was adopted as AP + Switch; its legacy swconfig links remain
`observe-only`. Both were re-added to `all-aps`, and each apply created exactly
two WLAN sections. The next preview had 0 changes, the controller showed 2/2
online and both radios on both devices were broadcasting.

The cycle left 0 pending UCI changes and installed no package. The WRT retained
its default route via `203.0.113.1`, DHCP `100`/`150`/`12h`, firewall hash and flat
`lan1`–`lan4` bridge; the C6 retained DHCP ignore=1, down WAN and its active
read-only swconfig links. SQLite ended with schema 11, 2 device rows, 4 owned
WLAN sections and `integrity_check=ok`; the v10→v11 migration was separately
integrity-checked.

### Durable Phase-3 apply, measured 2026-08-19

After an operator-owned one-time WRT conversion put management on `br-lan.1`,
the schema-13 browser path applied the owned VLAN2 stack. Its first run
(`2a0552f3-f901-4bb1-96a5-8c2ba2de773e`) deliberately withheld confirmation
when dnsmasq health could not read the canonical `/tmp/etc` runtime file; all
seven sections reverted and no ledger residue remained. Adding the second
read-only ACL pattern fixed the observation boundary. The retry
(`fa6bb976-1ca8-4c73-8de8-64b308b27746`) recorded C6 as a zero-change no-op and
WRT as seven changes applied.

Runtime proof covered `br-lan.2` at `198.51.100.1/24`, tagged bridge membership,
the live dnsmasq range, firewall4 input/forward dispatch, separate TCP/UDP DNS,
DHCP UDP 68→67, closed tails and `lan2`→`wan`. A WRT-only temporary WLAN carried
a real Mac client; the C6 omission was explicit. Policy blocked WAN while
preserving DHCP/DNS, then restored it. DHCP disable removed the running range;
the custom `50`–`59`/`1h` configuration issued `.54` with a 3600-second lease.

At the §5be checkpoint, the temporary WLAN, custom pool and explicit allow
remained applied pending confirmation; the Mac did not automatically rejoin its
saved `Management` Wi-Fi, while wired `en9` kept the controller path intact.
That checkpoint is historical. Confirmed cleanup operation
`d93695b8-1b31-4550-936a-320dd1cf1bc6` later retained `testvlan`, restored DHCP
`100`/`150`/`12h`, reset `lan2` to the legacy WAN-only default, removed the
temporary WLAN and rotated both radios' managed BSS keys on both APs. Both
devices applied successfully, and the following Preview and `dryrun` were
zero-change. STATUS §5be remains the gateway chronology, §5bf records the live
schema-14 migration, and §5bg is the authoritative cleanup, runtime, client and
post-cleanup recovery evidence.

The final 2026-08-20 client pass used a separate WRT-only proof BSS. Two
physical iPhones were simultaneously authenticated, associated and authorized
on `phy1-ap1`, held distinct guest DHCP leases, and each loaded HTTPS from
`1.1.1.1` and `example.com`. Each was denied access to the Mac's known-live LAN
HTTP listener. UCI held `isolate=1` and `bridge_isolate=1`, while live
bridge-port sysfs reported `isolated=1`. Both clients also
reported reciprocal raw Safari failures to the other's IP, but neither peer ran
a known-live listener and no positive control established that either target
would answer that exact probe; those failures do not prove literal
bidirectional peer data-plane isolation. A completed durable operation removed
only the proof WLAN: the WRT
pruned one wireless section, the C6 was a no-op and the final plan was
zero-change. The operator-created and applied Guest network on VLAN 3 remains
intentional current state.

---

## 15. 802.11k neighbour reports, measured

Settled 2026-08-16 against both reference devices: a WRT3200ACM
(mvebu/mwlwifi) and an Archer C6 v2 (ath79, ath9k + ath10k), each publishing
one SSID on both bands.

### 15.1 The methods exist and the list is empty

`ubus -v list hostapd.<iface>` carries `rrm_nr_get_own`, `rrm_nr_list`,
`rrm_nr_set`, `rrm_beacon_req`, `bss_transition_request` and
`bss_mgmt_enable` on both devices. Stock rpcd grants none of them; the
controller's ACL now grants the first three and deliberately not the rest.

On every AP the renderer had configured with `ieee80211k=1` and
`rrm_neighbor_report=1`, `rrm_nr_list` returned `{"list":[]}`. **hostapd does
not populate its own neighbour list, not even with its own BSS.** The feature
was advertised in every beacon and answered with nothing.

### 15.2 The reply shapes

`rrm_nr_get_own` returns a **positional triple**, not an object:

    { "value": [ "<ap-bssid>", "example-managed-wlan",
                 "<sanitized-neighbor-report-hex>" ] }

The element decodes as BSSID (6 bytes) · BSS-info (4, LE) · operating class ·
channel · PHY type · optional subelements. The controller does not decode it:
hostapd already computes the regulatory mapping for its own BSS, and a second
implementation would disagree with the AP's own on exactly the bands where it
matters. It is relayed as the hex string the device produced.

`rrm_nr_set` takes `{"list": [[bssid, ssid, nr], ...]}`, **replaces** the whole
list for that BSS, and is scoped per BSS — setting one interface leaves the
others on the same radio untouched. Verified by reading back.

A short `value` array must be treated as "could not tell", never as a neighbour
with blank fields: relaying one makes an AP answer a client with a candidate it
has no channel to scan for.

### 15.3 `rrm_nr_list` returns entries in hostapd's own order

Pushing `[A, B, C]` reads back as `[C, B, A]` on both devices — neither
insertion order nor sorted. Comparison must be order-insensitive. An
order-sensitive one reports every list as changed on every cycle, so the
reconciler pushes to every AP forever and never converges, which is
indistinguishable from a broken one except that it also spends the request
budget.

### 15.4 `wifi reload` clears the list SELECTIVELY

The measurement that decides whether the current list can be remembered rather
than re-read. After editing one `wifi-iface` section and running `wifi reload`:

| interface | config changed | neighbour list after |
|---|---|---|
| `phy0-ap1` | yes | **cleared** |
| `phy1-ap1` | no | **kept, intact** |

Neither "an apply clears everything" nor "an apply clears nothing" is safe. The
list is therefore read back and compared on every cycle, which makes the
operation idempotent against every cause of loss — a hostapd crash, an
operator's own reload, a device that rebooted between cycles.

Confirmed in the large by a two-device run where the WRT had rebooted and the C6
had not: 2 pushed, 2 left alone, all four BSSes ending correct.

### 15.5 `bss_mgmt_enable` turns RRM on at runtime

`ubus call hostapd.<iface> bss_mgmt_enable
'{"neighbor_report":true,"beacon_report":true,"bss_transition":true}'` is
accepted and takes effect without a reload. Recorded because it is a real
alternative to a `wifi reload` for enabling neighbour-report answering — and
deliberately **not used**: the renderer writes `rrm_neighbor_report=1` to UCI,
which is the durable source, and enabling it at runtime as well would mask
config drift rather than surface it.

### 15.6 Cost

Per device per cycle: one `iwinfo.devices`, one batched request carrying two
calls per wireless interface, and — only when something differs — one more
batched request to push. At the shipped 15-minute cadence that is under a tenth
of DEVICE-BUDGET's one-request-per-minute allowance, and in the steady state the
push never happens. Requests are attributed to the device's Management Overhead
readout.

### 15.7 A partial read must not shrink a list

Observed while one AP was still bringing its radios up: the reconciled AP was
handed a neighbour list with the booting one removed. The missing AP contributed
no BSSes, so the computed table did not contain them, so they were deleted.

A cycle in which any device errored therefore may add and refresh and may not
remove. The failure modes are not symmetric — a stale neighbour costs a client
one wasted scan, a missing one costs it the full scan 802.11k exists to avoid.
Removals resume on the next complete read, verified by repairing the shrunken
lists back to three neighbours each.

### 15.8 What was NOT established

**That `rrm_nr_get_own` is safe on a driver that is already failing.** On the
WRT3200ACM, hostapd entered uninterruptible sleep with one of these calls in
flight, and the tempting conclusion — a fourth mwlwifi "says yes, means no"
quirk of the shape §14 documents three times — is not supported. The kernel log
showed `nl80211 ... nl_recvmsgs failed: -5` before the call, and on a freshly
booted device the same call returns instantly and leaves hostapd healthy,
checked deliberately. The device's 5 GHz path has been unstable since the
`txpower=0` incident; no specific operation has been shown to trigger it.

Recording a quirk here would have gated a working feature off working hardware
forever, with a measurement's authority behind it. A controlled repeat on a
known-good device costs a minute.
