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
| D6 | Target UX reference is **UniFi Network 10.4** (per the user's screenshots of UniFi OS 5.1.19 / Network 10.4.57). | none |
| D7 | **The controller is a self-hosted container (Omada-style)** — Docker/Podman image, amd64+arm64, one persistent volume, compose file provided. Managed devices remain agentless stock OpenWrt; the WRT3200ACM is a *managed device*, never the host. Discovery is a convenience layer (host networking gets full discovery; bridge/Desktop gets add-by-IP, which must be first-class UI). | D1, D5 |

---

## 1. Pinned stack

| Layer | Choice | Rationale |
|---|---|---|
| Language | Go ≥ 1.23 | static binary in a scratch container, goroutine-per-device polling |
| SQLite driver | `modernc.org/sqlite` | pure Go (D3) |
| HTTP router | stdlib `net/http` (1.22+ pattern mux) | zero deps, auditable |
| WebSocket | `github.com/coder/websocket` | maintained, minimal |
| Secrets | `golang.org/x/crypto`: argon2id KDF + chacha20poly1305 | credential store |
| UI | Svelte 5 + Vite, static build, embedded via `embed.FS` | smallest bundles of the mainstream options; 1.5 MB gz budget |
| Charts | uPlot | fastest at dense time-series; matches UI-SPEC |
| Tables | TanStack Table (core) + virtual scrolling | Clients/Logs grids |
| Topology | d3 (`d3-hierarchy` tree layout + manual SVG) | UniFi's topology is a tidy tree |

**Forbidden:** cgo anywhere; any ORM; any component mega-framework; any
JS dependency that pushes the gzipped bundle past budget. Every dependency
addition is a decision, not a default.

Build target: multi-arch container image (`linux/amd64`, `linux/arm64`) via
`docker buildx`, `CGO_ENABLED=0`, binary stripped (`-ldflags "-s -w"`), final
stage `FROM scratch` (+ tzdata and CA certs copied in). CI fails if the image
exceeds 40 MB or the UI bundle exceeds 1.5 MB gzipped.

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
├── ui/                           # Svelte app (built → embedded)
│   └── src/{lib,routes,stores}/
├── tools/
│   ├── probe.py                  # hardware validation (exists)
│   ├── mock_ubus.py              # dev-harness device simulator (exists)
│   └── budget_check.sh           # binary/bundle/RSS assertions for CI
├── deploy/
│   ├── Dockerfile                # multi-stage: ui build → go build → scratch
│   ├── docker-compose.yml        # host-networking default, one volume
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

Schema (authoritative; ship as `internal/store/schema.sql` with a
`schema_version` table and forward-only migrations):

```sql
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
  role         TEXT NOT NULL DEFAULT 'ap',     -- 'gateway'|'ap'|'switch'
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
  security_json TEXT NOT NULL,           -- {mode:'sae-mixed'|'sae'|'psk2'|'owe'|'none', key, pmf:'optional'|'required'}
  roaming_json TEXT NOT NULL DEFAULT '{}', -- {ft:bool, ft_over_ds:bool, kv:bool}
  options_json TEXT NOT NULL DEFAULT '{}', -- {hidden,isolate,maxassoc,schedule,...}
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE zones (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,   -- Internal/Guest/DMZ/External/VPN
  policy_json TEXT NOT NULL DEFAULT '{}' -- default input/output/forward
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
  rendered_hash TEXT NOT NULL,         -- sha256 of canonical rendered values
  applied_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, config, section)
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
  event TEXT NOT NULL, detail_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX events_ts ON events(ts);
```

Maintenance tick (every 5 min, one transaction): flush RAM ring → `rollup_5m`;
fold aged 5m → `rollup_1h`; prune both per retention config; prune `events`
beyond 100k rows. This single-transaction shape is what keeps NAND writes inside
the DEVICE-BUDGET §8 cap — do not "improve" it into per-sample inserts.

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

Site model: WLAN id=3, ssid `Home`, security `sae-mixed` key `s3cret`,
bands `2g,5g`, network `lan` (VLAN 1), roaming `{ft:true, ft_over_ds:true, kv:true}`,
group containing device 7 whose caps report radios `radio0` (5g) and `radio1` (2g).

Rendered staged calls for device 7 (order matters; all staged, no commit — D2):

```
uci.add {config:"wireless", type:"wifi-iface", name:"oowrt_wlan3_radio0", values:{
  device:"radio0", mode:"ap", ssid:"Home", encryption:"sae-mixed", key:"s3cret",
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

Network `iot` VLAN 45, `10.7.45.1/24`, zone `Guest`, DHCP on:

```
# /etc/config/network (staged)
uci.add {config:"network", type:"bridge-vlan", name:"oowrt_bv45", values:{device:"br-lan", vlan:"45", ports:[…per-device port map…], oonfeewrt:"1"}}
uci.add {config:"network", type:"interface", name:"oowrt_net_iot", values:{proto:"static", device:"br-lan.45", ipaddr:"10.7.45.1", netmask:"255.255.255.0", oonfeewrt:"1"}}
# /etc/config/dhcp
uci.add {config:"dhcp", type:"dhcp", name:"oowrt_dhcp_iot", values:{interface:"oowrt_net_iot", start:"100", limit:"149", leasetime:"12h", oonfeewrt:"1"}}
# /etc/config/firewall
uci.add {config:"firewall", type:"zone", name:"oowrt_zone_guest", values:{name:"guest", input:"REJECT", output:"ACCEPT", forward:"REJECT", network:["oowrt_net_iot"], oonfeewrt:"1"}}
uci.add {config:"firewall", type:"forwarding", name:"oowrt_fwd_guest_wan", values:{src:"guest", dest:"wan", oonfeewrt:"1"}}
```

Gateway renders all of it; an AP renders only the bridge-VLAN + an unmanaged
`interface` stanza so tagged traffic bridges through. The render function takes
the device *role* and *port map* from capabilities and emits the right subset —
this role-aware subsetting is a first-class tested behavior, not an if-cascade.

### Diffing

`Diff(rendered Doc, actual UciState) []ChangeItem` — actual state read via
`uci get` per config. Compare only sections we own (marker or `oowrt_` prefix)
plus detect foreign conflicts. Output is the human-readable diff the UI shows
before apply and the exact staged-call list the apply engine executes. The
`rendered_hash` in `owned_sections` short-circuits no-op reconciles.

---

## 6. The apply engine (`internal/applyengine`)

One state machine instance per (changeset × device). Devices execute serially
in dependency order; **any device the controller's own management traffic
traverses to reach other devices (typically the gateway) applies last**; first
failure aborts the remaining queue.

```
IDLE → RENDER → PREFLIGHT → STAGE → APPLY → CONFIRM_POLL → HEALTH → CONFIRMED
                    │           │       │         │            │
                    └conflict   └err    └err      └timeout─────┴─fail→ AWAIT_REVERT → VERIFY_REVERTED → FAILED
```

| State | Action | Exit |
|---|---|---|
| RENDER | render + diff; empty diff → skip device | PREFLIGHT |
| PREFLIGHT | quiesce collector for device; check session; **detect a foreign dirty delta by listing `/tmp/.uci` and treating any entry with `size > 0` as a config with unsaved LuCI/SSH edits** → abort with "unsaved changes on device". **`uci.changes` cannot do this** — see the note below. If the change touches the path the controller manages this device through, require the UI's explicit traversal acknowledgment flag | STAGE |
| STAGE | issue staged `uci.add/set/delete` batch; verify with `uci.changes` that the delta matches the plan exactly | APPLY |
| APPLY | `uci.apply {rollback:true, timeout:T}` — T = 90 s default, per-device override from caps. **Status 0 means "applied", never "healthy": an apply that killed dnsmasq still returns 0** | HEALTH |
| HEALTH | **runs before confirm, while the rollback timer is still armed** — if it fails, do nothing and let the device revert itself. Expected interfaces up (`network.interface dump`), expected SSIDs present via `iwinfo` + `hostapd.<iface> get_status` (**not** `network.wireless status` — unreachable via rpcd, see §14), gateway reachable if role=ap. Read all of it on a **fresh session**: the applying session's staged delta masks a revert | CONFIRM_POLL |
| CONFIRM_POLL | poll `uci.confirm` every 3 s **on the applying session token** — reconnecting the socket is fine, re-authenticating is fatal, and a token refresh here is an unrecoverable abort. Stop on success or T expiry | CONFIRMED (write `owned_sections`, audit, resume collector) |
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
state), never by hand-editing. "Apply without rollback" exists as a separately-authorized flag
for changes that legitimately sever the management path (re-addressing the
management network on the host) and is logged as such in the audit record.

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

Derived metrics computed at flush time on the controller (never on-device):
experience score per client (formula + default weights from ARCHITECTURE §5,
weights in config), interference/airtime pct from survey deltas.

---

## 8. API and WebSocket

REST base `/api/v1`, session-cookie auth (single admin user in v1;
`HttpOnly; SameSite=Strict; Secure` when TLS). Login rate-limited. All mutating
endpoints require header `X-Oonfee-CSRF` matching a per-session token.

Endpoint table is ARCHITECTURE §9; implementation notes and the two examples a
builder gets wrong without samples:

`GET /api/v1/changes` → the pending changeset (rendered diff, per device):
```json
{"changeset": {"id": 41, "status": "pending", "devices": [
  {"device_id": 7, "name": "attic-ap", "items": [
    {"op": "add", "config": "wireless", "section": "oowrt_wlan3_radio0",
     "summary": "Broadcast 'Home' (5 GHz)", "values": {"…": "…"}}],
   "warnings": ["applies via the interface being changed — confirm traversal"]}]}}
```

`POST /api/v1/changes/apply` body `{"changeset_id":41,"ack_traversal":[7]}` →
`202` + apply progress streamed on the WS as `apply.state` messages.

WS `/api/v1/live`, JSON messages:
```json
→ {"type":"subscribe","topic":"device.stats","device_id":7}
→ {"type":"subscribe","topic":"apply","changeset_id":41}
← {"type":"stats","device_id":7,"ts":1753500000,
   "series":[{"kind":"sta_rssi","key":"aa:bb:…","v":-52.0}, …]}   // batched per tick
← {"type":"apply.state","changeset_id":41,"device_id":7,"state":"CONFIRM_POLL","detail":"…"}
← {"type":"event","category":"client","event":"wifi.roam","detail":{…}}
```
Server batches stats per tick (one frame per device per focused interval), and
drops to a slower cadence automatically if the client stops reading (backpressure
via send-buffer high-water mark; never unbounded queues).

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
fails CI over 1.5 MB total. Fonts: system stack only. Icons: single SVG sprite,
hand-picked subset (~60 icons), not an icon-font dependency.

---

## 10. Security implementation

- Credential store: per-device `user:pass` sealed with chacha20poly1305; key
  derived via argon2id from the operator passphrase at daemon start (or a
  keyfile for unattended boot — explicit, documented tradeoff flag).
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
  - File paths are **canonicalised before matching**, and `*` **crosses `/`**.
    Together these make file grants behave the opposite of how they read: a
    grant on `/sys/class/net/*` never fires (those entries are symlinks into
    `/sys/devices`), while widening it to `/sys/devices/*` hands over that
    entire subtree. Prefer a ubus object to a file grant wherever the data
    exists in both — DSA presence, for instance, comes from
    `luci-rpc.getNetworkDevices` (`devtype: "dsa"`), which the poll already
    fetches, so it needs no filesystem grant at all.

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
therefore *detect* that `nlbwmon`/`lldpd`/`usteer` are missing and *offer* the
install, but the install itself must be authorised with the operator credential
rather than performed with the controller's own — the same split that
un-adoption needs. Anything that widens the device's attack surface should cost
an operator credential; anything that only reads or reconciles owned UCI should
not.
- Audit: every changeset stores author, timestamp, full diff, per-device
  outcome. Every login and failed login is an event.
- No default credentials anywhere. First run generates the admin account
  interactively (or via one env var for containerized installs) and prints
  nothing secret to logs.

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
plus CA certificates and tzdata. One process, PID 1, no shell in the image.

Runtime contract (what `deploy/docker-compose.yml` encodes):

| Aspect | Value |
|---|---|
| Data | single volume mounted at `/data` (SQLite, config, backups) |
| Network | `network_mode: host` recommended (full discovery); bridge + `8080:8080` supported with add-by-IP adoption |
| Ports | `:8080` HTTP, `:8443` TLS with generated ECDSA cert (optional) |
| Config | env vars `OONFEE_DATA_DIR`, `OONFEE_LISTEN`, `OONFEE_PASSPHRASE_FILE` (secrets via file, never env value) |
| Health | `GET /healthz` (no auth, no body beyond `ok`) wired as the compose healthcheck |
| Upgrade | pull new image, restart; schema migrates forward on boot; downgrade = restore volume backup |
| Backup | `POST /api/v1/system/backup` streams a consistent snapshot (SQLite `VACUUM INTO`) — also just copy the volume while stopped |

Graceful shutdown on SIGTERM: finish (or abandon pre-APPLY) any in-flight
changeset, flush the telemetry ring, checkpoint WAL, exit. An apply that has
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
| hardware | `tools/probe.py` report imported as a capability fixture; the budget harness from DEVICE-BUDGET §7 run against the real WRT3200ACM per release |

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
*Done when:* adopt→un-adopt against the mock leaves mock state byte-identical
to pre-adoption; capability JSON matches the fixture.

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

**M6 — Insights + topology + logs.** survey/station derived metrics,
Radios screen, topology inference, event ingest + Logs screen.
*Done when:* Radios shows interference/airtime for mt76 fixture radios and
correctly *omits* them for the mwlwifi fixture radio.

Flows/DPI: not scheduled. Revisit only after M6 ships and only for capable
hardware, per PARITY-MATRIX.

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
| clients of this network (192.168.1.0/24) | **3** — a laptop, a phone, a watch |
| neighbours on the uplink (10.7.46.0/24, behind the WAN port) | **7** |
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
| `br-lan` state | UP, still holding `192.168.1.1/24` |
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
interface at `10.7.45.1/24`, DHCP, a closed-by-default zone and its forwarding,
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

    { "value": [ "32:23:03:db:be:43", "oonfee-roam",
                 "322303dbbe43ef1900008024090603022a00" ] }

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
