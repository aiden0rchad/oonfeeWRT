# oonfeeWRT — Architecture

> Target platform assumption: **stock OpenWrt**, current stable (the 25.12 series
> at time of writing; 24.10 also supported). Verify ubus/package specifics against
> your actual target release before relying on them — this document flags uncertain
> items with **[verify]**.

**Published and historical hardware boundary (2026-08-22):** `v0.1.0-rc.1` and
the v40 checkpoint use schema 17. The lab store used schema 17 for this proof.
Both reference routers were factory-reset and
re-adopted only
after the operator accepted the default-off controller-access-payload
disclosure. Adoption installed no package, binary, daemon, service or firmware.
The operator later exercised the separate, default-off official-feed `lldpd`
workflow on both routers, including exact planning, installation,
physical-interface configuration, read-only diagnosis, drift-checked rollback
and clean reinstallation. V39 remains the historical startup-fix checkpoint.
V40 passed the settled runtime, recovery, regression, secret and reproducibility
gates before `v0.1.0-rc.1` was published and clean-tested from its public binary
and container artifacts. FS-119 records that proof; FS-120 records permission
hardening newer than the immutable tag.

**v0.1.0 source and historical live boundary:** final source targets schema 19. Schema 18
adds the Dashboard/controller-speed-test slice; schema 19 adds complete
server-enforced RBAC, account and session management. Stored-only diagnostics
ZIP/API/UI, its bounded rotating log sink, and encrypted native `.oowrtbak`
controller export/restore with disposable preview, controlled restart, retained
safety artifact, session revocation and persistent router-write suppression are
also implemented. The controlled live upgrade/restart completed with exact
binary version `dev-phase41-live-schema19`. The schema-19 recovery state has two
devices, two credentials, one enabled owner, one WLAN and no mesh. A signed-in
live UI smoke passed Dashboard, Accounts, Diagnostics, Backup & Restore,
Devices and Topology with no browser errors; fresh schema-17 rollback and
schema-19 recovery sets also passed verification. The smoke did not execute a
restore, diagnostics generation/download, public-provider speed test or router
restore. The completed `v0.1.0` tag workflow and
GitHub Release, not this historical checkpoint, are the publication authority
and own the isolated restore/container evidence.

---

## 0. The constraint that shapes everything

oonfeeWRT does not maintain OpenWrt. Concretely:

| We may | We may not |
|---|---|
| Call ubus/rpcd methods stock OpenWrt already exposes | Ship a daemon, service, or binary of our own that runs on the device |
| Read and write UCI, the same as LuCI does | Patch, fork, or rebuild OpenWrt |
| Offer optional packages **from the official OpenWrt feeds** only for an explicitly selected feature; never install them by default or to close a validation gate | Host a feed of device-side code we wrote |
| With explicit opt-in, write or replace one rpcd ACL JSON file in `/usr/share/rpcd/acl.d/` | Install kernel modules, init scripts, or cron jobs of our authorship |
| Read files and run allow-listed binaries via `file.exec` | Leave unreviewed or unrecorded persistent state; every ACL/login, controller-owned UCI section, or optional package/service/configuration change needs explicit ownership and rollback |

**Maximum default controller-access footprint: one JSON file and one scoped user
account.** The UI calls this the optional **oonfeeWRT controller access
payload**. Accepting it during adoption installs or replaces that ACL file
and may create the login; accepting it later replaces only the ACL file. It
unlocks controller access to supported topology, radio channel/scan, OpenWrt log
and fixed-target WAN ICMP observations. It installs no package, binary, daemon,
service or firmware. Leaving its box unchecked or cancelling leaves the router
unchanged and keeps dependent observations explicitly unavailable. Everything
else lives in the controller.

Separately selected official-feed packages are itemized capabilities, not part
of adoption. Schema 17 records the package manager, before-state, packages
actually added, service before-state, operation state, and rollback result. A
device cannot be un-adopted while that record exists.

This forecloses some features — accept it. The alternative is a build system,
per-architecture packages, a release process tracking every OpenWrt version, and
a support burden that has killed most projects in this category.

**Corollary:** every capability in this document must be traced to something
stock OpenWrt or an official-feed package already does. If a design needs
controller-authored code on the router, the design is wrong.

---

## 1. System shape

```
                    ┌──────────────────────────────────────┐
                    │            oonfeewrt-ui              │
                    │   React SPA — embedded               │
                    └───────────────┬──────────────────────┘
                          REST + WebSocket (localhost)
                    ┌───────────────┴──────────────────────┐
                    │        oonfeewrt-controller          │
                    │  ┌────────────┬──────────┬────────┐  │
                    │  │ Reconciler │ Collector│ Events │  │
                    │  ├────────────┴──────────┴────────┤  │
                    │  │  Capability registry per device │  │
                    │  ├────────────────────────────────┤  │
                    │  │  Store: SQLite + TSDB rollups  │  │
                    │  └────────────────────────────────┘  │
                    └───────────────┬──────────────────────┘
                 HTTPS  /ubus JSON-RPC  (poll + call)
        ┌───────────────┬───────────┴────────┬───────────────┐
        ▼               ▼                    ▼               ▼
   OpenWrt GW      OpenWrt AP           OpenWrt AP      OpenWrt switch
 stock (+ ACL)   stock (+ ACL)        stock (+ ACL)   stock (+ ACL)
```

Note what is *absent* from that diagram: any component of ours on the right-hand
side. The devices run stock OpenWrt; the optional ACL is data for stock rpcd,
not executable code. That is the whole integration.

### Components

**`oonfeewrt-controller`** — one static binary (Go recommended: single-file
deploy, good concurrency for N-device polling, cross-compiles to arm64/x86_64/
mips if you want to host it on an OpenWrt box). Owns the database, the
reconciliation loop, the telemetry collectors, the capability registry, and
serves the embedded SPA.

**`oonfeewrt-ui`** — SPA compiled and embedded into the controller binary.
No separate web server, no CORS, no second thing to run.

**Phase 4.1 controller services** — the current source includes a bounded,
durable controller-host speed-test runner; schema-19 RBAC, account and session
management; a private bounded rotating structured-log sink; and a stored-only,
bounded diagnostics ZIP job/API/UI. Diagnostics has no Fleet dependency and
makes zero router management/API/SSH calls or router changes. The encrypted
portable backup/restore service packages a consistent database/key pair,
previews it in disposable staging, and applies a confirmed pair only through a
controlled process restart. All stay inside the
same controller process and SQLite trust boundary; none creates a router-side
agent.

**`oonfeewrt.json`** — the optional rpcd ACL file, our only device-side file.
After the operator accepts the optional-capability installation prompt, it is
generated by the controller at adoption or replaced by the access-refresh
workflow and written **over SSH**, because ubus cannot write it—see §2's
bootstrap note. This adds scoped access to OpenWrt's existing rpcd API; it does
not install a package, binary, daemon, service or firmware.

### Where the controller runs

It is a client of OpenWrt, not part of it, so it can live anywhere with network
reach to the devices:

### Deployment model: a self-hosted container (decision D7, August 2026)

oonfeeWRT ships the way the Omada software controller and the self-hosted UniFi
Network Application do: **a Docker image you run on hardware you own** — NAS,
mini-PC, Pi, home server — that connects out to your OpenWrt devices. This
supersedes the short-lived "controller runs on the WRT3200ACM" decision (old
D1/D5): the breathing room is worth more than saving one small computer, and it
deletes an entire class of constraints (NAND wear-shaping, 25 MB binary budget,
tight RSS limits, armv7-first builds, procd packaging, the two-hats
self-management hazard).

| Target | Verdict |
|---|---|
| **Docker / Podman container, amd64 or arm64** | **Primary target.** One image, one volume, compose file provided. |
| Bare binary on any always-on Linux box | **Fully supported.** Same binary the image wraps. |
| The WRT3200ACM (or any router) hosting the controller | **Not supported in v1.** Nothing forbids it — it's a static binary — but it is not built, packaged, tested, or budgeted for. The WRT3200ACM's role is *managed device*, where it's a class-A citizen. |
| Router with 128MB flash / 128MB RAM | **No.** |

**What does not relax:** everything in DEVICE-BUDGET §1–7. Those budgets govern
what the controller does *to managed devices* — polling cost, TLS handshakes on
weak CPUs, flow-offload conflicts, zero flash writes on managed hardware. The
routers didn't get faster because the controller moved into a container. Only
the controller's *own* resource envelope relaxed (DEVICE-BUDGET §8).

**The Docker networking caveat — read this before writing discovery code.**
A containerized controller on the default bridge network is *not on the LAN's
layer 2*. Consequences, same as Omada's documented ones:

| Mode | Discovery | Recommendation |
|---|---|---|
| `network_mode: host` (Linux) | Full: subnet TCP probe + ARP + mDNS all work | Explicit opt-in; the listener is exposed directly on host interfaces, so supply firewall/TLS policy. |
| Bridge network with loopback port mapping | Subnet TCP probe works (it's L3); ARP-table and mDNS discovery do not cross the bridge | **Default.** The UI offers **add-device-by-IP** as a first-class path. |
| Docker Desktop (macOS/Windows) | No true host networking | Add-by-IP only; document it plainly |

Everything after discovery — adoption, polling, apply — is ordinary outbound
L3 HTTP and works identically in every mode. Design rule: **discovery is a
convenience layer; adoption must never depend on it.**

---

## 2. Transport: why `/ubus`, not SSH

OpenWrt's `rpcd` already exposes ubus as JSON-RPC over HTTP at `/ubus`, served by
`uhttpd`. This is a real, typed, ACL-scoped API. SSH is a shell you have to parse.

**Choose `/ubus`.** Concretely:

> **The bootstrap is the one transport exception after explicit operator
> opt-in.** Measured on a
> stock OpenWrt 25.12.5 on 2026-08-14, signed in over `/ubus` **as root**:
>
> | call | result |
> |---|---|
> | `uci.get rpcd` | ubus status 6 — refused |
> | `uci.set rpcd.<login>` | ubus status 6 — refused |
> | `file.write /usr/share/rpcd/acl.d/*.json` | ubus status 6 — refused |
> | `file.read /etc/rc.local` | status 0 — granted |
>
> Root over ubus is not root. rpcd's own ACL files bound what `/ubus` can reach,
> and stock OpenWrt grants write access to neither the `rpcd` config nor the ACL
> directory — deliberately, since that is exactly the escalation a compromised
> web session would want. No access group on the device grants it, and adding
> one would require writing to the directory we cannot write to.
>
> The writes adoption exists to perform therefore go over **SSH**: write the ACL
> JSON and create the scoped login after the operator approves the capability
> extension; un-adoption may remove them. A later, separately approved access
> refresh may replace only that ACL JSON. None of these actions installs a
> package, binary, daemon or service. Every poll, Apply and ordinary read is
> ubus. SSH remains a bounded bootstrap/cleanup channel, not the transport.
>
> The device-side assumptions were checked rather than assumed: that build has
> **no `base64`** and **no `sftp-server`**, so file content is piped to `cat`
> over the session's stdin (which also means it is never a shell argument), and
> the write is verified with `sha256sum` rather than trusted to have landed.

- Device requirements: `rpcd`, `uhttpd` with the ubus handler enabled, plus the
  rpcd modules for the data you need.
- Enable the handler in `/etc/config/uhttpd`:
  ```
  config uhttpd 'main'
      option ubus_prefix '/ubus'
  ```
- Auth: `POST /ubus` calling `session.login` with `{username, password}` returns
  a `ubus_rpc_session` token. Every subsequent call carries it as the first
  JSON-RPC parameter. Sessions expire (300 s idle).

  **The two denial channels do not split the way you would expect** — measured
  directly, because guessing it wrong inverts the whole retry policy:

  | What happened | How it arrives |
  |---|---|
  | Call succeeded | ubus status `0` in a 200 |
  | Session valid, but the **target** is not permitted (a uci config name, a file path) | ubus status **`6`** in a 200 |
  | Session invalid, expired or destroyed | JSON-RPC **error `−32002`** |
  | Session valid, but the **object+method** is in no granted access-group | JSON-RPC **error `−32002`** |

  So status `6` is always permanent — the session is fine and re-authenticating
  changes nothing. And `−32002` is **ambiguous**: it means rpcd's ACL layer
  refused to proxy the call at all, which covers both a dead session and a
  method the ACL never granted. Disambiguate with exactly **one** re-login: if
  the retried call still returns `−32002`, it is a permanent ACL gap, so log it
  as a capability error and stop. Never retry status `6`, and never loop on
  `−32002`. **Neither retry is permitted during a confirmation window**, where a
  token refresh is an unrecoverable abort — see §4.
- Authorization: JSON files in `/usr/share/rpcd/acl.d/`. oonfeeWRT ships
  `/usr/share/rpcd/acl.d/oonfeewrt.json` granting exactly the objects/methods it
  needs to a dedicated `oonfeewrt` user — **not** root, and not the full LuCI ACL.

### rpcd modules and what they give you

| Package | ubus object(s) | Use |
|---|---|---|
| *(base rpcd)* | `session`, `uci`, `file` | auth, config, exec/read |
| `rpcd-mod-iwinfo` | `iwinfo` | radio info, assoclist, freqlist, txpowerlist, scan |
| `rpcd-mod-luci` | `luci-rpc` | `getNetworkDevices`, `getWirelessDevices`, `getHostHints`, `getDHCPLeases` — big composite reads in one call |
| `rpcd-mod-rpcsys` | `system` (upgrade parts) | sysupgrade, password |
| `rpcd-mod-ucode` | ucode-based extensions | write your own ubus objects in ucode |
| *(netifd)* | `network`, `network.interface`, `network.device`, `network.wireless` | interface status, reload, wireless status |
| *(hostapd)* | `hostapd.<iface>` | station data, BSS transition (802.11v), neighbor reports |
| *(dnsmasq/odhcpd)* | `dhcp` **[verify per release]** | leases |

**Practical note:** `luci-rpc.getHostHints` is the fastest way to get the
name/MAC/IP/vendor picture that fills the Client Devices table. Use it; don't
reinvent it.

**The `file` object is a loaded gun.** `file.exec` grants arbitrary command
execution. You *will* need it (for `iw`, `lldpcli`, `nlbw`), so scope the ACL to
an explicit allow-list of executables rather than granting `file.exec` broadly.
This is the highest-risk surface in the whole design — treat the ACL file as
security-critical code and review it like one.

### TLS

`uhttpd` with `px5g`/`libustream-mbedtls` gives HTTPS with a self-signed cert.
The controller does **trust-on-first-use**: at adoption, record the device's
certificate fingerprint; refuse to talk to a device whose fingerprint changes
without an explicit re-adopt. Plain HTTP is acceptable only on an explicitly
trusted management VLAN, and the UI should say so loudly.

The same boundary applies to the controller UI. Sealing wireless keys at rest
does not protect a newly submitted key, an authenticated session cookie, or the
daemon's live memory. Authenticated WLAN/mesh reads expose only `has_key`; there
is no reveal endpoint, and legacy `?reveal=1` requests remain redacted. Use TLS
whenever the browser-to-controller path leaves a trusted management network.

---

## 3. Data model

The whole point of the product lives here. Configuration is authored at the
**site** level and *rendered* down to devices. Nothing is authored per-device
except device-specific overrides.

```
Site
├── Zone            (Internal, DMZ, Guest, External, VPN)  ← firewall policy anchor
├── Network         (VLAN id, subnet, DHCP, DNS, IPv6, zone ref)
├── WLAN            (SSID, security, band mask, network ref, AP-group ref, schedule)
├── APGroup         (which devices broadcast which WLANs)
├── Device          (adopted OpenWrt box: functions, legacy primary role, model, credentials, pins)
│   ├── Radio       (band, channel, width, txpower — auto or pinned)
│   ├── Port        (name, profile ref, native VLAN, tagged VLANs, PoE mode)
│   └── Override    (per-device deviation from site config, explicit + visible)
├── Policy          (zone→zone matrix, firewall rules, port forwards, traffic rules)
├── Profile         (port profiles, RADIUS profiles, schedules, IP groups)
└── Client          (MAC-keyed: name, fixed IP, group, note, blocked, rate limit)
```

### Ownership tagging — the coexistence rule

oonfeeWRT must never clobber config a human wrote in LuCI. Every UCI section the
controller creates carries a marker:

```
config wifi-iface 'oowrt_wlan_guest_ap1'
    option oonfeewrt '1'
    ...
```

The reconciler operates only on sections where `oonfeewrt=1` **or** whose name
matches the `oowrt_` prefix. Foreign sections are read (for display) but never
written or deleted. If a foreign section conflicts with desired state (e.g. a
hand-made SSID with the same name), surface it in the UI as a **conflict** and
stop — do not silently win.

### Storage

- **SQLite** for config, inventory, clients, events, audit log. WAL mode.
  No server to run. `Store.BackupTo` now wraps SQLite's online backup mechanism
  in a private, no-clobber, verified snapshot that captures committed WAL state;
  clean shutdown/checkpoint remains an operational alternative. Never copy the
  main file alone while WAL is active; that can omit committed schema/data. Pair
  every database backup with that controller's `keyring.json`: neither a
  database nor the passphrase can reconstruct the keyring's random data key.
  This is correct at home and small-business scale; resist Postgres until you
  have a reason.
- **Schema 11 device intent:** `functions_json` is the authoritative non-empty
  set of Gateway/AP/Switch responsibilities. `role` remains a deterministic
  primary label and legacy compatibility field. Only a missing pre-v11 value
  expands the old bundles (`gateway` → all three, `ap` → AP + Switch,
  `switch` → Switch); malformed or explicitly empty state fails closed.
- **Schema 12 zone intent:** the pre-existing `zones.policy_json` becomes a
  semantic compatibility boundary. Each explicit row is
  `{"forward_to":[...]}` for one active managed source; no row retains the
  historical source→`wan` default and an explicit empty array blocks all
  modeled forwarding. A v11 binary would ignore an explicit block/inter-zone
  edge, so it must refuse a v12 database. Malformed stored policy fails closed.
- **Schema 14 secret storage:** `wlans.security_json` contains mode/PMF only;
  WLAN PSKs, mesh keys, and ownership `rendered_hash` values live in row-bound
  sealed columns. A sealed `secret_state.key_check` binds every v14 database to
  its keyring before WAL mode, DDL, or any other mutation. The v13→v14 migration
  first verifies the legacy keyring, commits ciphertext plus a pending-scrub
  marker, then checkpoint/VACUUM/checkpoint removes plaintext from the main file,
  free pages, and WAL. The scrub is idempotent and must complete before serving;
  read-only opens refuse an incomplete scrub.
- **Schema 15 policy semantics:** the already-present `fw_rules` and client
  policy columns become authoritative desired state for firewall rules, port
  forwards, static routes, client block/fixed-IP/group records and the
  inspectable Object Manager compiler. This is a semantic boundary: a v14
  process must refuse the database rather than silently render without that
  intent.
- **Schema 16 observability:** events gain producer identity/provenance and
  enrichment fields, per-device/source ingest cursors distinguish empty,
  reset and missing log coverage, topology is stored as half-open validity
  intervals with source state, and RF scans are persisted as explicit runs.
  The migration attests the complete table/index/foreign-key shape before the
  v16 marker is accepted; a colliding partial table fails closed.
- **Schema 17 capability ownership:** optional official-feed installations retain
  their package/service/configuration baseline and rollback state. A device row
  cannot be removed while that ledger exists.
- **Schema 18 controller speed tests:** bounded controller-host jobs retain
  descriptor provenance, the consented `plan_id`, progress, nullable results and
  terminal history. They have no device foreign key.
- **Schema 19 controller accounts:** the canonical roles are `owner`, `admin`,
  `operator` and `viewer`; every existing admin migrates to an enabled owner.
  New usernames use an ASCII-only grammar and a unique ASCII-NOCASE index. Soft
  deletion disables the row, removes its verifier and retains the username.
  Conditional writes preserve the last enabled owner under concurrency; create,
  role/state/delete and password mutations share a transaction with their audit
  event. Role-bearing sessions, declarative REST/live authorization, My Account
  and owner-only account/session administration are implemented on this store.
- **Time series**: a dedicated rollup schema in the same SQLite file for v1
  (see §5). Graduate to VictoriaMetrics only if you exceed ~50 devices.
- **Secrets**: Argon2id derives a key-encryption key from the operator
  passphrase; it unwraps a random data key in `keyring.json`, which seals device
  credentials, wireless keys, and secret-derived verifiers with XChaCha20-
  Poly1305 and record-specific AAD. Keyring creation is atomic and never
  overwrites an existing file; if a non-empty database exists without its
  keyring, startup refuses rather than minting an unrelated replacement. A
  mismatched database/keyring pair fails its key check before mutation.
  Controller metadata remains sensitive even when reusable secrets are sealed.

Pre-v14 database backups may contain plaintext WLAN/mesh keys and ownership
hashes. Schema migration cannot scrub copies it does not control and must never
delete them automatically. Protect those backups as secrets, retain one until
the migrated pair is verified, and require explicit operator confirmation for
their deletion.

Router configuration tarballs contain wireless keys in plaintext too. A retained
router backup must be encrypted as a stream, recovered through a verified
stream-decrypt/archive-read path, and leave no temporary plaintext archive. This
moves—not removes—the trust boundary to the encryption passphrase and recovery
tooling; filesystem snapshots and external backups remain outside the
controller's erasure claim.

Schema-19 source implements portable controller backup/restore on
`Store.BackupTo`. Owner export packages its consistent live-WAL snapshot and
matching wrapped key material as one authenticated native `.oowrtbak`, encrypted
under a caller-owned export passphrase separate from the controller runtime
passphrase. The passphrase is never retained.

Restore accepts a bounded raw artifact over TLS or direct loopback. Disposable
private staging authenticates the fixed artifact, proves manifest schema equals
the actual source database schema, rejects unsupported future schema, migrates a
scratch copy to exactly schema 19, and validates integrity, secrets and a usable
owner. The preview returns only authenticated manifest/schema/count information.
Confirmation is bound to its artifact and `plan_id`; it requires recent password
reauthentication, the export passphrase again, the current destination runtime
passphrase, exact `RESTORE CONTROLLER`, and four acknowledgements. The runtime
passphrase is checked against the live keyring/in-memory data key before the
prepared keyring is written. Neither passphrase enters URLs, environment
variables, filenames, logs or retained job state.

Confirmation creates a mode-0600 encrypted safety artifact at
`<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak` using that export
passphrase. After the applied audit receipt is durably cleared, fixed-shape
retention targets three safety artifacts, fills available slots newest-first
and prunes the rest. An artifact referenced by an active marker, receipt or
suppression record is always preserved, so active recovery state can
temporarily raise that count.
Operators must copy an artifact off-host before pruning for longer retention.
A durable marker then drives a clean in-process restart:
work is quiesced, SQLite is checkpointed/closed, the prepared pair is verified
and swapped, and startup rolls back before serving if validation fails. Open
SQLite handles and the in-memory key are never hot-swapped. Success revokes all
sessions and persists router-write suppression until owner review plus exact
`RESUME ROUTER WRITES`. Restored desired state is never automatically applied;
read-only monitoring of restored devices may resume after restart while the
gate remains active. Explicit resume immediately enables automatic 802.11k
neighbour reconciliation, which may call hostapd `rrm_nr_set`; it does not
start a restored desired-configuration Apply.

---

## 4. Provisioning and rollback

### The apply cycle

```
1. User edits site model in UI          → writes to `desired` tables
2. Reconciler computes, per device:
     render(desired) → target UCI doc
     diff(target, last_applied_hash)    → change set
3. Server returns the redacted fleet preview plus an opaque keyed token bound
   to site intent, adopted fleet, ownership state and every device plan
4. Apply rebuilds that fleet state and preflights every selected device before
   the first write; traversal, driver, caution and partial-fleet risks require
   explicit acknowledgments
5. On Apply, serially per device in dependency order (Gateway last):
     a. lock device; revalidate the bound site/fleet/ledger/plan state
     b. quiesce polling after any in-flight poll has completed sink emission
     c. uci.set / uci.delete / uci.add   (batched — these only STAGE a delta)
     d. uci.apply   {rollback: true, timeout: 90}
        └─ apply is what COMMITS the staged delta, snapshotting the
           pre-apply state for the revert. NEVER call uci.commit first:
           a manual commit leaves nothing staged, the snapshot equals the
           new state, and rollback silently protects nothing.
     e. controller *repeatedly* polls uci.confirm until it succeeds or the timer expires
        — **always on the same session token that issued the apply**
     f. success → rollback timer cancelled | failure → device self-reverts
     g. after outcome verification, record ownership, then audit/return
6. Release quiesce and wake an immediate refresh. First non-applied result
   aborts every later device.
```

The token is server-authenticated and compared in constant time. It commits to
secret-bearing state without putting secrets or an unkeyed secret-derived hash
in the browser. A browser edit, second tab, capability/endpoint change or
ownership-ledger change after Preview therefore produces a stale-preview
refusal before a router write; each device is checked again immediately before
its own write.

Once the fleet run is admitted it is detached from HTTP request cancellation
under one bounded `ApplyDrain` deadline. This is required because an armed
router rollback timer outlives a closed browser connection. Shutdown waits for
tracked applies; a later device is not started unless enough of the outer
deadline remains for a complete rollback-confirm cycle. Schema 13 closes the
response-observability gap: the caller supplies a UUID bound idempotently to
the request, parent and per-device write boundaries are durable, and a status
endpoint recovers running or terminal results. A live browser reload recovered
the same failed/reverted operation rather than retrying it (STATUS §5be).

**`uci.apply` returns success even when the config it applied kills a service.**
Measured: an invalid `dnsmasq` port applied cleanly (`status 0`) while dnsmasq
died and DNS stopped resolving for the whole LAN. A controller that treats the
apply status as a health signal will confirm that config and make the outage
permanent. **Confirmation must be gated on an independent post-apply health check,
not on the status code** — re-read the affected service's real state
(`service.list`, an `iwinfo` call, a resolve, an interface status) and confirm
only if it is actually healthy. Rollback does recover from this: the same test
saw the config revert at t+38s and dnsmasq come back healthy at t+50s. Note the
lag — the service is not usable the instant the config reverts, so a verifier
that samples too eagerly will misread a successful rollback as a failed one.

**Two session rules, both measured on hardware, both easy to get wrong:**

1. **`uci.confirm` is bound to the session that applied.** A second authorized
   session calling confirm gets `PERMISSION_DENIED` (6) and the change reverts
   anyway. So the confirm poll must hold the applying token for the whole
   window: reconnecting is fine, re-authenticating is fatal. If the token is
   lost mid-window the apply *will* revert — treat that as an abort and report
   it, rather than retrying into a false success. (This is the correct safety
   behaviour, but it means a controller restart inside the window cannot be
   recovered unless the token itself was persisted.)
2. **A session cannot observe its own rollback.** When the timer fires, rpcd
   restores `/etc/config` but the applying session's staged delta comes back
   with it, and session-scoped `uci.get` reads through that delta — so the
   applying session still returns the value it *failed* to set, indefinitely.
   Closing the TCP connection does not clear it; the token scopes the delta, not
   the connection. During the armed window, health must use non-UCI runtime
   state (`network.interface`, `iwinfo`, `hostapd`) because another login may
   return the applying token. After the window resolves, verify configuration
   through a genuinely fresh session.

Two further apply-path facts: `uci.apply` is a **single all-or-nothing
transaction across every staged config**, so the ordering list below is a
staging order, not a sequence of independently gated applies; and `uci.add`
returns `NOT_FOUND` (4) rather than creating a config file that does not exist,
so a genuinely new config must be created on disk first.

> This ordering was initially specified wrong in this document (commit before
> apply) and caught in review. The staged-not-committed distinction is the whole
> mechanism: `save()` stages, `apply()` commits-with-rollback — exactly LuCI's
> own flow. `tools/probe.py` §11 validates the correct order on real hardware.

**Steps (d)/(e) are non-negotiable, and they already exist.** The ubus `uci` object
exposes `apply` / `confirm` / `rollback`: `apply` commits and reloads, then opens
a confirmation window; `confirm` cancels the pending revert; the timer fires
`rollback` if confirm never lands. This is precisely the mechanism LuCI itself
uses for its "Apply unchecked / countdown" behavior — see
[openwrt/luci#1769](https://github.com/openwrt/luci/pull/1769).

Two lessons inherited from LuCI's implementation:

- The client should **poll `confirm` repeatedly** during the window rather than
  calling it once — connectivity often takes 10–20s to fully re-establish after
  a network or wireless reload, and a single early confirm attempt fails on a
  device that would have recovered fine.
- A traversal-sensitive edit requires a separate acknowledgment, but remains
  rollback-protected. The controller does not currently offer an unsafe
  "apply without rollback" escape hatch.

Make the timeout configurable per device: 90s is a reasonable default, but a
device doing a full wireless reconfigure on slow hardware may need more.

### Ordering

Changes are not independent. Preflight the complete selected fleet before the
first router write, then apply devices serially with the Gateway last. Within
one device stage in this order, and treat a failure as abort-and-revert for that
device plus abort of every later device:

1. Networks/VLANs and bridge VLAN filtering (the plumbing)
2. Interfaces and DHCP
3. Firewall zones and policy
4. Wireless (depends on networks existing)
5. Per-device overrides

Wireless changes are the dangerous ones: reconfiguring a radio drops every
client on it, including — if you are careless — the admin's laptop and possibly
the controller's own path to the device. Always warn in the UI when the apply
path traverses the device being changed.

### Health probe

After apply, "the device answered ubus" is not enough. Probe:
- ubus reachable and session valid
- expected interfaces up (`network.interface` status)
- expected SSIDs beaconing (`iwinfo` / `hostapd` runtime status)
- uplink still reaches the gateway

Only then `confirm`.

After confirmation (or an empty plan that already matches), ownership must be
recorded before success is logged or returned. If that controller write fails,
the truthful result is “device applied; controller ownership recording failed,”
the audit severity is error, and the remaining fleet is not touched.

---

## 5. Telemetry pipeline

This is where the project's perceived quality comes from — UniFi's charm is
largely graphs — and it is also where a wrapper can quietly ruin the device it's
supposed to be helping.

**Read [`DEVICE-BUDGET.md`](DEVICE-BUDGET.md) before implementing any of this.**
It sets hard resource budgets against the weakest target hardware (MT7621 class)
and explains why the dominant cost is TLS handshakes and flow-offload conflicts
rather than the polling itself.

### Two rates, not three loops

Earlier drafts had a fixed three-tier loop. That's wrong for heterogeneous
hardware — it polls a struggling MT7621 exactly as hard as an idle x86 box.
Collection is **demand-driven**:

| Rate | When | Interval | Contents | Persisted? |
|---|---|---|---|---|
| **Baseline** | always, per adopted device | ~60s, adaptive | reachability, firmware, CPU/RAM, WAN throughput, client count — only series that need unbroken history | Yes |
| **Focused** | only while a UI screen showing *this device* is open | 5–10s | per-client RSSI/rate/retries, per-radio survey, port counters, live throughput | Streamed to UI; downsampled before persisting |
| **Slow** | always, low priority | 5–15m | channel survey, LLDP neighbours, DHCP leases, temperature | Yes |

When the last UI viewing a device closes, it drops back to baseline within one
interval. Nobody watches the Radios page at 3 a.m. — don't collect for it.

Intervals are **adaptive**: if a device's response latency or load average
crosses a threshold, its interval lengthens automatically and the UI says so.
Polls are **staggered** across the interval rather than fired on a common tick.
Collection **quiesces entirely** during an apply/confirm cycle for that device.
This is a boundary, not a scheduler hint: quiesce marks the poller, waits for
an already-running cycle through sink emission, and only then lets staging
begin. Release wakes an immediate refresh rather than waiting for the next
baseline interval.

Never run a **scan** on a serving radio in a loop — it kicks clients off-channel.
Use survey deltas for continuous RF health. Full scans are only the explicit
`POST /devices/{id}/radios/{radio}/scan` path, require
`acknowledge_disruption:true`, time out after 45 seconds, persist their outcome,
and are never resumed after controller restart.

### Sources

| Metric | Source | Notes |
|---|---|---|
| **Which clients are connected** | **`iwinfo.assoclist`** — it carries the full row anyway, so there is no reason to source presence from anywhere else. `hostapd.get_clients` agrees with it and is fine as a cheap cross-check. | Settled by measurement: **57 samples over ~10 minutes, 100 % agreement**, including a station sitting idle to 140 s repeatedly, with every call's status verified as `ubus 0`. An earlier run appeared to show them disagreeing for 131 s; that was **my instrumentation, not the device** — the probe script treated a failed `assoclist` call as "zero stations", so one bad call looked like an empty radio. Not reproducible once the calls were status-checked. |
| Per-client RSSI, PHY rate, TX retries, connected time | `iwinfo.assoclist` (**30.3 ms**) — it is the only source with the full row. `hostapd.<iface> get_clients` (**2.1 ms**) enriches it. `iw station dump` is **not needed at all** and should not be granted. | Measured against two real associated stations: byte/packet counters are **identical** across both sources, so hostapd is fine for volume. But it **lacks** `tx.retries`, `tx.failed`, `connected_time`, `signal_avg`, `noise` and `thr`, so TX-Retries, SNR and "connected for" all require `iwinfo` regardless of the presence question above. Two unit traps: `hostapd.rate` is **100× `iwinfo.rate`** (14440000 vs 144400 kbit/s) — never mix them in one series; and hostapd's per-client `airtime` is `{rx:0, tx:0}` on mwlwifi. |
| Client connect/disconnect **events** | hostapd's `AP-STA-CONNECTED` / `AP-STA-DISCONNECTED` log lines | The real-time signal, and the correct trigger for a focused-poll refresh. Do not infer connection state by diffing `hostapd.get_clients` between polls — per the row above, its departures are unreliable. |
| Per-AP channel, SSID, BSSID, airtime | **`hostapd.<iface> get_status`** | **1.0 ms** vs `iwinfo.info` at **35.7 ms**. Carries `airtime: {time, time_busy, utilization}`, so channel utilization needs no survey call at all on the fast loop. Two traps: `utilization` is the 802.11 BSS-Load **0–255** scale, not a percent (172 ≈ 67.5%, which matched a counter-delta of 68.9%), and the counters refresh on hostapd's own cadence — a sample 5 s apart was observed unchanged while `iwinfo.survey` kept advancing. Use it for the fast tier and reconcile against `iwinfo.survey` on the slow one. |
| Channel utilization / interference | **`iwinfo.survey`** (native ubus; `iw dev <dev> survey dump` only as fallback) | Utilization = **Δbusy / Δactive between two samples**, never the ratio of the absolute values. Measured 2026-08-13: `busy_time` and `active_time` are monotonic ms counters that do **not share an epoch** — the 5 GHz radio read `active=24427` against `busy=922104` while both advanced correctly. The absolute ratio gave 1354% there and, worse, **25.9% on the 2.4 GHz radio where the true figure was 73.3%** — plausible and wrong by 3x. hostapd's independent BSS-load reading of 70% on the same radio settled it. The fields are good; the formula was not. Interference ≈ (busy − rx − tx) / active needs `rx_time`/`tx_time`, which mwlwifi leaves uninitialised, so that column is capability-gated. Take `noise` from `iwinfo.info`: `iwinfo.survey` reports it **unsigned** (161 for −95). That fixes the *encoding* only — measured 2026-08-13, the 2.4 GHz radio's noise floor swung **42 dB through `iwinfo.info` and 46 dB through `iwinfo.survey`** over 20 samples, while the 5 GHz radio on the same driver held within 7 dB. The instability belongs to the radio, not the method, so noise and SNR are gated **per radio** (`Radio.NoiseStable`), and utilization — which does not depend on the noise floor — stays available on radios where they are not. |
| Per-client bandwidth + 24h usage | `nlbwmon` via `nlbw -c json -g mac` (**no ubus object — CLI only**) | Purpose-built netlink accounting with its own retention DB. The right answer; don't parse conntrack yourself. Three things measured on install: `commit_interval` defaults to **24h**, so a read returns zeroes until `nlbw -c commit` runs — the controller must commit before reading, which is a *write* grant; and nlbwmon warns at startup that its netlink receive buffer is capped unless `net.core.rmem_max` is raised, so on a busy network it silently under-counts. Verified accurate otherwise: 24.30 MiB recorded for a 25 MB transfer. |
| Per-interface throughput | `network.device` status counters, or `/proc/net/dev` | Delta between polls. |
| Per-port switch stats | native `network.device`/`luci-rpc` on DSA; stock `swconfig dev … show` on legacy switches; `ethtool -S` only as enrichment | Read/config on DSA, read-only on measured swconfig hardware. |
| PoE control/state | Hardware-specific ubus objects on the few supported PoE switches | **Mostly unavailable.** See RISKS. |
| CPU / memory / temperature | `system.info`, `/sys/class/thermal` | |
| WAN latency / loss / reachability | Gateway-vantage `file.exec` of stock `/bin/ping`: exactly 3 ICMP packets to fixed target `1.1.1.1`, at most once/minute | This is one fixed reachability vantage, not HTTP validation, ISP uptime, DNS health or a configurable multi-target SLA. Missing/refused/malformed output is unknown; zero replies is measured 100% loss. |
| Topology adjacency | stock `brctl showmacs` (or `bridge -j fdb`) + ARP + wireless assoc; optional `lldpd` (`lldpcli show neighbors -f json`) | FDB gives the no-install baseline. LLDP enriches managed adjacency and removes ambiguity. |
| DNS queries | dnsmasq query log, tailed via `file.read` with an offset | Optional; privacy-sensitive, off by default. |
| Flows + application ID | `netifyd` (nDPI) if it's in the official feed for the target **[verify]**, else `ntopng`; both write to disk and are polled via `file.read` | The expensive one, and the one most likely to fail the no-device-code rule. See PARITY-MATRIX. |

**Everything above is reachable agentlessly.** Prefer the built-in `brctl`,
`swconfig` and native ubus paths before offering packages. `lldpcli`, `ethtool`
and `nlbw` remain optional official-feed enrichments invoked through
`file.exec`; nothing requires code of ours on the device.

**Prefer native ubus objects over `file.exec` wherever the data exists in both.**
A ubus call is IPC; `file.exec` forks and execs a binary, which is a real cost on
an 880 MHz MIPS core when done per-radio per-interval. Reserve `file.exec` for
data with no ubus equivalent — LLDP, `ethtool -S` — and only at the slow rate.
Channel survey is **not** such a case: `iwinfo.survey` is native ubus (measured
on mwlwifi), so it belongs in the focused tier at ordinary IPC cost, with
`iw survey dump` kept only as a fallback for drivers that lack it.

**Two of these carry a warning on constrained hardware:**

- `nlbwmon` (per-client bandwidth) depends on connection accounting, which is in
  direct tension with flow offloading — the thing that makes MT7621-class devices
  route at gigabit. Default it **off**. See DEVICE-BUDGET §3.3.
- `netifyd`/DPI is out of budget on that hardware entirely.

Batch aggressively — one `luci-rpc.getHostHints` beats twenty small reads — and
keep one persistent HTTP connection per device rather than reconnecting per poll.
TLS handshake cost dominates everything else on class-C hardware.

### Retention and rollup

The raw poll ring is controller memory only. SQLite stores completed rollups,
never raw samples:

| Durable data | Retained / bounded | Serves |
|---|---|---|
| 5m avg/min/max/count | 14d | ranges up to 7d |
| 1h avg/min/max/count | 13mo (396d) | longer ranges |
| OpenWrt `logd` events | 24h, at most 50,000/device and 100,000 total; exact repeated odhcpd IPv6-RA/no-default-route warnings condense per producer epoch without changing warning severity | General Logs and client incidents |
| Controller/audit events | newest 100,000 rows; every event has a 64 KiB aggregate stored-text/detail limit | Audit and controller history |
| Closed topology intervals | 31d; active intervals do not expire | current graph and replay |
| RF scan runs/BSS rows | newest terminal run per `(device_id,radio_key)`; pending/running preserved; BSS cascade | newest explicit scan per radio |

Five-minute rows are flushed in one transaction only after the bucket closes;
shutdown discards the in-progress RAM bucket rather than writing a partial row
that a restarted process could overwrite. Hourly rows are count-weighted
folds. Store min/max alongside avg or long-range charts hide the spikes people
open them to find. The same maintenance transaction caps terminal RF-scan
history per stable radio key and cascades discarded BSS rows without touching
an active scan.

SQLite's `(series_id, ts)` primary keys and time indexes serve those bounded
windows. Storage is not the hard part; write amplification is—batch rollups in
transactions and do not regress to per-sample inserts.

### Derived metrics you must define yourself

UniFi's composite is proprietary. The shipped comparable score is explicitly
`wifi-v1`:

```
rssi_score = clamp((rssi_dBm + 90) × 2.5, 0, 100)
wifi-v1 = 0.45·rssi_score + 0.35·(100 − retry_delta_pct)
        + 0.20·(100 − tx_failure_delta_pct)
```

It is **all-or-null**. RSSI, retry delta and TX-failure delta must come from one
portable station sample; missing/reset/roamed/zero-packet inputs produce no
score, and weights are never renormalized. Only the rollup is persisted. Show
the three components and missing-input reason in the UI.

---

### 5.1 Client scoping — whose network is this host on?

A gateway's ARP, neighbour and DHCP tables cover **every** interface, so the
client inventory built from them mixes the network the device serves with the
network it connects to. Measured on the reference device: of 16 known hosts, 7
were neighbours on the upstream network behind the WAN port and only 3 were
actual clients.

Scope comes from `network.interface dump`, on the same slow refresh cadence as
the radio list and inside the same batch, so it costs no extra requests. A host
is:

| Scope | Meaning |
|---|---|
| `local` | its address is in a subnet of an interface that does **not** carry the default route |
| `upstream` | its address is in a subnet of the interface that **does** — a neighbour on the uplink, not a client |
| `unknown` | no observed address, or an address in no interface's subnet |

**Upstream is decided by the routing table, never by an interface being named
`wan`.** The name is a convention; a device bridged onto an existing network can
carry the default route on the interface called `lan`.

`unknown` is a real answer and must not collapse into `local`. A host that has
not been shown to be on this network must not be counted as one — that is the
same three-state rule as everywhere else, applied to a question where guessing
puts someone else's hardware in a list captioned "your devices".

Note this does **not** need the site model (§5). The site model is *our*
description of a network; this question needs the *device's*, which netifd
already publishes.

## 6. Discovery, adoption, identity

**Discovery (LAN):** sweep the management subnet probing TCP/80 for a `/ubus`
endpoint, and fingerprint it with **`list` on the null session** — an
unauthenticated call that stock OpenWrt answers with its full ubus object graph.
Require several objects together (`session.login`, `uci`, `system.board`) before
calling a host a device; one is not enough to justify putting an address in
front of someone as "type your router password here". Implemented in
`internal/discovery`.

> **Corrected 2026-08-14 — the probe this section used to specify was unsafe.**
> It said to probe with "a `session.login` that answers with an auth failure —
> that response alone proves it's OpenWrt rpcd, without logging in". It does not.
> On a stock device with no root password, `session.login` **succeeds** for any
> password at all: rpcd resolves the account through `/etc/shadow` and an empty
> entry matches everything. Measured on the reference device — logging in as
> root with `definitely-not-the-password-9f3a` returned status 0, a session
> token, and an ACL set including `uci` write and `file` exec.
>
> So the specified probe would have minted a **root session on every
> passwordless device in the subnet, on every scan**. `list` needs no credential
> guess, creates no session, writes no failed-login record, cannot lock an
> account out, and returns strictly more information.

Optionally also listen for mDNS if `umdns` is running, but don't depend on it:
stock OpenWrt doesn't advertise anything useful for us, and making it do so would
mean config we'd have to own. **Not implemented for that reason** — the subnet
sweep finds everything mDNS would and needs nothing installed on the device.

Discovery is **on demand only**: no periodic rescan, no background timer. A
controller that sweeps someone's subnet on a schedule generates unsolicited
traffic against hosts nobody asked it to touch, forever, and noticing a device
that appeared while nobody was looking does not pay for that. The sweep refuses
anything wider than a **/22**, skips point-to-point and tunnel interfaces (their
far side is routed, not local), and never touches IPv6 — a /64 is 1.8e19
addresses. Everything it declines to look at is **reported**, because a
controller that quietly skipped the operator's subnet reports "no devices
found", which reads as a fact about their network rather than about itself.
If every attempted address in one CIDR returns `EHOSTUNREACH`/`ENETUNREACH`,
the result carries an explicit per-network failure instead of an empty-fleet
claim. One route error is insufficient; a normal refusal elsewhere in that
CIDR proves the route was usable.

Add-by-address accepts a hostname, but an inspect/adopt workflow resolves it
exactly once and pins the chosen IP across authenticated HTTP, SSH bootstrap
and post-bootstrap HTTP verification. Otherwise DNS movement could inspect one
router, install credentials on another and persist a third identity. Plain HTTP
inventory stores the resolved address because there is no peer-identity pin;
HTTPS may retain the operator hostname only with the observed certificate pin.
For steady-state polling, the first hard failure closes the cached keep-alive
transport while retaining the rpcd session, so the next tick redials on a
repaired host route without creating a re-login storm.

**Adoption flow** (the UniFi one-click adopt feel, without an agent):

1. Device appears in **Pending** with its IP and the shape the object list
   reveals — how many radios have a BSS up, whether it routes, whether it serves
   DHCP.

   **Not model, MAC or firmware: those cannot be read pre-auth.** This step used
   to say they came from `system.board` / `system.info` "pre-auth where
   possible". Measured 2026-08-14: never possible. Stock rpcd answers
   `system.board` on the null session with JSON-RPC `-32002, Access denied`. The
   UI therefore says the model is unknown until a credential is supplied, rather
   than inventing one from the object list.
2. Operator supplies device credentials **once**. Before adoption, the
   controller may use them for an authenticated, read-only **Inspect** call.
   Inspect uses ubus only: no SSH, ACL/login bootstrap, UCI write, package
   operation or inventory row. It measures model, radios and ports, and keeps
   denied/unknown evidence distinct from a measured negative.
3. Operator selects a non-empty set of device functions: **Gateway**, **AP**
   and/or **Switch**. An active WAN default route or enabled LAN DHCP can
   recommend Gateway; an ordinary AP management route over LAN cannot. DSA
   switch support is conditional on an already VLAN-aware bridge, while legacy
   swconfig is observe-only. The UI may preselect positive recommendations,
   but the operator can change them and Adopt always sends the reviewed set
   explicitly.
4. Controller, in one transaction:
   - serializes the inventory check through row commit before contacting the
     device when Gateway is selected. Only one managed Gateway is allowed
     until HA exists; an empty fleet may still adopt AP-only when an external
     gateway owns routing
   - installs no package during the default path. A separate, disabled-by-
     default option may offer an official-feed package for one named feature.
     LLDP uses a package-manager simulation first, binds the reviewed plan, then
     requires a second acknowledgement before installing `lldpd` and enabling
     its service
   - after an explicit capability-extension acknowledgment, writes
     `/usr/share/rpcd/acl.d/oonfeewrt.json` **over SSH** (ubus refuses it even
     to root—§2), verified by `sha256sum`
   - creates a dedicated `oonfeewrt` user with a generated password, also over
     SSH, and only *after* the ACL is in place — a login whose access-groups do
     not exist yet is a credential that authenticates and can do nothing
   - verifies the new login actually works
   - runs the **capability probe** (§6.1) **last, on the controller's own
     session** — not first, and not as the operator. The registry gates what
     every screen renders, and screens render from what the *controller* can
     reach. Stock OpenWrt grants zero access to `iwinfo.devices`, so a probe run
     before the ACL exists cannot see the radios at all: measured 2026-08-14 on
     a genuinely fresh device, probing first recorded survey, hostapd control
     and per-client accounting as *undetermined* on hardware that has all three,
     and the identical calls returned status 0 the moment the ACL landed
   - records the TLS certificate fingerprint and the SSH host key
     (trust-on-first-use)
   - **discards the operator's original credential** — it is never stored, and
     is requested once again at un-adopt
5. Device goes green. No config is pushed yet; adoption and provisioning are
   separate steps, and the UI should make that obvious.

**Un-adoption must be a real, tested feature**, and it is deliberately *not* one
click. It runs in two phases:

1. Under the `oonfeewrt` credential: revert every UCI section we own. Fully
   granted already, and it is the part that touches the user's config.
2. Prompted for the **operator credential**, held in memory for that one
   transaction exactly as at adoption: delete the `config login 'oonfeewrt'`
   section from `/etc/config/rpcd`, then remove
   `/usr/share/rpcd/acl.d/oonfeewrt.json` — in that order, so the controller's
   own session is not cut before phase 2 finishes.

> **The controller is never granted write access to its own ACL file or to the
> rpcd config.** Self-removal is self-escalation: the contents of that file are
> unconstrained, so a controller that could rewrite it could grant itself
> `file.exec` on a shell and collect at the next reload. The rpcd login lives in
> `/etc/config/rpcd`, and uci grants have no section-level scoping — only a
> method dimension and a config-name dimension — so "delete just our own login"
> is not expressible; it would require write on the whole rpcd config, which is
> the power to add a login or re-point an existing one.

The cost is stated plainly: a device whose admin password has been lost, or that
is offline, cannot be un-adopted from the controller. That case must degrade to
showing the exact residue — the two paths above — and the commands that remove
them. Surface the credential requirement **at adoption time**, not only when the
user clicks un-adopt. Package installs sit inside the same operator-credentialed
envelope, at adoption and again at un-adopt for removal; the steady-state
credential can query installed packages but never mutate them.

Phase ordering is fail-closed. If the scoped controller session is unavailable
while owned sections remain, a delete fails, or a config commit cannot be
proved, phase 1 is incomplete: phase 2 does not remove the login/ACL and the
inventory plus ownership ledger stays. Normal row deletion requires both
`config_revert_complete=true` and no controller footprint. Explicit Force is a
separate operator decision for hardware that is gone and must report residue.

**Multi-site / NAT:** out of scope, by construction. Reaching a device behind NAT
requires either a device-side dial-out agent (which we've ruled out) or a tunnel.
The answer is a tunnel the *user* already runs — WireGuard from the remote site
back to the controller's network, configured through oonfeeWRT like any other
OpenWrt WireGuard peer. Document that path; don't build a broker for it.

### 6.1 Capability probing

Because we run against whatever OpenWrt the user happens to have, we cannot
assume anything. At adoption, and on a slow re-check thereafter, probe and store:

| Probe | Determines |
|---|---|
| `system.board` — model, target, board name, actual system/SoC string | hardware class, arch; generic targets are not classified without measured silicon evidence |
| `ubus list` | which ubus objects exist → which rpcd modules are installed |
| package manager query (`apk`/`opkg` list-installed via `file.exec`) | `nlbwmon`, `lldpd`, `usteer`/`dawn`, `sqm-scripts`, `wireguard-tools` present? |
| `iwinfo.devices` + `freqlist` + `txpowerlist` | radios, bands, 6 GHz support, regulatory limits |
| DSA and runtime bridge names from `luci-rpc.getNetworkDevices`; exact read-only `swconfig` probe; `brctl`/`bridge` FDB probe | port configuration, read-only port stats and topology evidence as separate capabilities; the runtime bridge may differ from board JSON's UCI LAN device |
| `iw list` capability flags | 802.11k/v/r, MU-MIMO, VHT/HE/EHT support |
| firewall4 vs legacy | zone model support |
| free flash + RAM | whether package installs are even safe to offer |

Inspect exposes switch management as one of four modes: `dsa-conditional`
(configuration only when the existing bridge is already VLAN-aware),
`observe-only` (legacy swconfig telemetry/topology only), `unknown`, or `none`.
A Switch function therefore promises wired participation and available port
visibility, not universal selective per-port/VLAN control.

**The UI renders from the capability record.** A feature the device cannot do is
*absent* from its screens — not greyed out. Greying out teaches users the product
is broken; absence teaches them their hardware is limited. This is the single
biggest UX difference between managing homogeneous UniFi hardware and
heterogeneous OpenWrt hardware, and getting it wrong makes the app feel unfinished.

### 6.2 Package installation policy

Installing packages is the one place we reach beyond "read and write config," so
it needs rules:

- **Official feeds only.** Never a feed we host.
- **Explicit opt-in only.** Installation is disabled by default. The operator
  must select an itemized package, size/dependency set, reason and unlocked
  feature; merely continuing adoption cannot authorize it.
- **Never required.** Declining a package degrades one feature, never the app.
  Adoption must succeed with zero packages installed beyond stock.
- **Check free space first.** A full `/overlay` is a bricked router. Refuse the
  install rather than attempting it, and say why. Below 8 MB free, default to
  no installation until the package manager reports the exact package and
  dependency size and at least 2 MB recovery headroom remains afterward.
- **Track what we installed** so un-adoption can offer to remove it.
- **Never upgrade the device's packages or firmware on our own initiative.**
  Surfacing that updates exist is helpful; applying them is the user's call.

---

## 7. Firewall / zone model

UniFi moved to a zone-based firewall (visible in the screenshots as Zone: DMZ /
External on log entries). This maps cleanly onto OpenWrt.

```
oonfeeWRT Zone  →  /etc/config/firewall  config zone
Networks in a zone → that zone's `network` list
Zone-to-zone matrix cell → config forwarding (src, dest) + default policy
Rules → config rule; order is display-only until a proved evaluation-order backend exists
Port forwards → config redirect
Traffic rules (block a client, block a category) → config rule + ipset/nftset
```

**Implemented schema-12 forwarding contract.** Enabled managed networks with
VLAN > 1 produce source zones. `wan` is a foreign destination only; `lan` stays
foreign. For each source, `forward_to` names the active managed zones and/or
`wan` to which it may initiate traffic. No explicit database row preserves the
old source→`wan` behavior; an explicit empty array emits no forwarding. The
reverse edge is independent, while replies to an allowed flow use firewall4's
normal conntrack state.

Human labels are validated against firewall4's actual sanitized, lowercase,
11-character identifier: it must start with a letter, cannot normalize to
`lan`/`wan`, and cannot collide with another zone. Malformed stored JSON, an
unknown destination, self-edge or orphaned source is a site error, never a
fallback allow. Desired-state network/policy mutations are serialized so two
individually valid concurrent edits cannot create an orphan policy.

The renderer creates only marked owned zones and directed forwardings. It never
adopts or edits a foreign zone, forwarding, rule or redirect. An active foreign
forwarding, an overlapping `ACCEPT`/`REJECT`/`DROP` forwarding rule, or a DNAT
with real `dest_ip` from a managed source blocks the plan when it makes the
claimed allow/block state unverifiable; definitively disabled sections and
router-local redirects are ignored. Active foreign firewall includes and
reachable non-fw4 nftables policy block explicit matrix intent; an unreadable or
malformed runtime ruleset fails closed. Schema 15's current
source adds explicit IPv4 firewall rules, port forwards, static routes and
client block/fixed-IP/group intent to the same Master Table. Its Object Manager
partially compiles visible, unsaved IPv4 `Secure` drafts and static network
routes. Device/group policy routing, QoS/rate limiting, application identity,
switch ACLs and proved rule-priority semantics remain gated expansion. The
whole-zone subset is the latest live-proven boundary; the schema-15 expansion
is source-tested pending live proof.

Since OpenWrt moved to **firewall4/nftables**, ip sets are cheap: use named
nftables sets for IP groups, client groups, and blocklists, and update set
*contents* without reloading the whole ruleset. That's what makes "block this
client" feel instant instead of triggering a full firewall restart.

---

## 8. Multi-AP roaming

This is the feature people actually mean when they say "I want it to feel like
UniFi." Do not write your own steering daemon.

- **`usteer`** — OpenWrt-project band steering and client steering daemon.
  Prefer it for new work; it's in-tree and maintained alongside netifd/hostapd.
- **`dawn`** — community decentralized WiFi controller; more feature-complete in
  some respects, mesh of daemons that gossip between APs. Well-liked; assess
  maturity on your target release.

Either way oonfeeWRT's job is **configuration and observation**, not steering
logic: render the daemon's UCI config from the site model (min RSSI, band
steering on/off, thresholds), read its state for the UI, and stay out of the
control loop.

Alongside it, configure hostapd for:
- **802.11k** neighbor reports (`rrm_neighbor_report`)
- **802.11v** BSS transition management (`bss_transition`)
- **802.11r** fast transition — over-the-DS and over-the-air, with a shared
  mobility domain and consistent `nas_identifier`/PMK-R0 key holder config
  across APs. **This is exactly the kind of cross-device consistency a
  controller exists to guarantee**, and doing it correctly by hand is where
  DIY OpenWrt multi-AP setups usually fail.

Caveat to document in the UI: 802.11r with WPA2-PSK breaks some older clients.
UniFi ships this as an opt-in toggle with a warning; do the same.

---

## 9. API surface (controller ↔ UI)

REST owns CRUD **and every durable Phase-4 query**. The WebSocket is deliberately
narrow: authenticated, same-origin `device.stats` subscribe/unsubscribe only,
used for reference-counted focus and current snapshots. Logs, events, topology,
history, radios, scans and joined client timelines are REST; there is no
`events` WebSocket topic.

```
GET    /api/v1/devices                     ?status=&type=
GET    /api/v1/devices/:id
POST   /api/v1/devices/inspect             ← authenticated ubus-only evidence; no writes
POST   /api/v1/devices/adopt               ← explicit functions[]; legacy role accepted
POST   /api/v1/devices/:id/unadopt
POST   /api/v1/devices/:id/refresh-acl      ← operator SSH credential is one-request-only
GET    /api/v1/devices/:id/capabilities/lldp ← durable install/configuration state
POST   /api/v1/devices/:id/capabilities/lldp ← plan, diagnose, install, configure or remove under action-specific acknowledgements
GET    /api/v1/clients                     ?connection=&network=&ap=
GET    /api/v1/clients/:mac/observability  ?from=<ms>&to=<ms>
GET    /api/v1/site                        ← desired state + effective zones[]
POST   /api/v1/site/networks[/{id}]        ← desired state only
POST   /api/v1/site/zones/{name}           ← {"forward_to":[...]}; desired state only
DELETE /api/v1/site/zones/{name}           ← remove explicit row; restore wan-only default
GET    /api/v1/site/preview                ← fleet diff + opaque preview_token
POST   /api/v1/site/apply                  ← bound, preflighted batched apply
GET    /api/v1/site/apply/:operation_id    ← durable parent/per-device status/result
GET    /api/v1/events                      ← keyset REST pages + General/Audit coverage
GET    /api/v1/events/:id                  ← exact durable detail
GET    /api/v1/topology[?at=<ms>]          ← current or historical instant
GET    /api/v1/topology/history            ?from=<ms>&to=<ms>
GET    /api/v1/radios                      ← last-known state + freshness/gaps
POST   /api/v1/devices/:id/radios/:radio/scan
GET    /api/v1/speedtests                  ← descriptor, bounded history and active job
POST   /api/v1/speedtests                  ← acknowledgement + current descriptor plan_id; start
GET    /api/v1/speedtests/:id              ← durable status/result
POST   /api/v1/speedtests/:id/cancel       ← request cancellation
POST   /api/v1/session/reauth               ← five-minute owner step-up; per-session throttle
GET    /api/v1/account                      ← current account and role
GET    /api/v1/account/sessions             ← own in-memory sessions
DELETE /api/v1/account/sessions/:session_id ← revoke one own session
GET    /api/v1/accounts                     ← owner-only account list
POST   /api/v1/accounts                     ← owner-only account create
PATCH  /api/v1/accounts/:id/role            ← owner-only role change
PATCH  /api/v1/accounts/:id/enabled         ← owner-only enable/disable
DELETE /api/v1/accounts/:id                 ← owner-only soft delete
POST   /api/v1/accounts/:id/password        ← owner-only password reset
GET    /api/v1/accounts/:id/sessions        ← owner-only target sessions
DELETE /api/v1/accounts/:id/sessions/:session_id ← owner-only target-session revoke
DELETE /api/v1/accounts/:id/sessions        ← owner-only revoke all target sessions
GET    /api/v1/diagnostics                  ← stored-only disclosure + bounded jobs
POST   /api/v1/diagnostics                  ← start one stored-evidence ZIP job
GET    /api/v1/diagnostics/:id              ← job status
POST   /api/v1/diagnostics/:id/cancel       ← request cancellation
GET    /api/v1/diagnostics/:id/download     ← completed ZIP; owner/admin only
GET    /api/v1/backups                      ← owner descriptor/history
POST   /api/v1/backups                      ← owner export; recent reauth
GET    /api/v1/backups/:id                  ← export status
POST   /api/v1/backups/:id/cancel           ← owner cancel; recent reauth
GET    /api/v1/backups/:id/download         ← native `.oowrtbak`; recent reauth
GET    /api/v1/restores                     ← owner descriptor
POST   /api/v1/restores/uploads             ← bounded raw native artifact; recent reauth
POST   /api/v1/restores/previews            ← disposable authenticated preview; recent reauth
GET    /api/v1/restores/previews/:id        ← safe preview status/result
POST   /api/v1/restores/previews/:id/cancel ← owner cancel; recent reauth
POST   /api/v1/restores/previews/:id/confirm ← plan-bound controlled restart; recent reauth
GET    /api/v1/restores/suppression         ← persistent router-write gate
POST   /api/v1/restores/suppression/resume  ← owner exact-text resume; recent reauth
GET    /api/v1/live                        ← WebSocket; `device.stats` only
```

Schema 18 implements the speed-test routes above as bounded jobs rather than a
long synchronous response. The default runner uses Cloudflare's direct download
and upload endpoints from the controller host, with a 10 MiB download, 5 MiB
upload, five idle-latency probes, one active job and a 30-second hard bound.
The GET descriptor includes a deterministic `plan_id`; Start requires both
`acknowledge_data_use:true` and that exact reviewed ID, and rejects a changed
plan with 409 before creating a job. It rejects redirects and has no Fleet/router
dependency. Loaded latency/jitter remain null because the single-stream method
does not probe them under load.
No public-provider speed test has run; current evidence is source and local-test
coverage only.

Schema 19 implements account storage, atomic audited mutations, role-bearing
sessions, exhaustive route/live authorization, My Account and owner account/
session administration. Owner writes require a recent password step-up;
revocation closes `/live` and cancels in-flight REST work. The diagnostics
routes implement one owner/admin stored-evidence job with bounded history,
cancellation and private ZIP download. The backup/restore routes implement the
owner-only portable workflow described in Storage. Every backup/restore endpoint
requires TLS or direct loopback; export start/download and restore mutations
also require recent password reauthentication. These
trust boundaries are not optional:

- authorization is enforced server-side on every protected REST route and live
  channel. Only health, setup/bootstrap and login are intentionally anonymous;
  `owner`, `admin`, `operator` and `viewer` are distinct, and the last enabled
  owner is not removable;
- controller speed tests are explicitly started, bounded and cancellable and
  make no router management/API/SSH call or write/install. Their traffic follows
  the controller host's normal route and may saturate its gateway/
  WAN. Gateway execution will be a separate optional official-feed capability
  with its own package-index, plan, install, run and rollback acknowledgements;
- diagnostics uses stored evidence only and makes zero router management/API/SSH
  calls or router changes. No live refresh exists; any future refresh must be a
  separately selected, named read-only operation and must not install, restart,
  write or initiate an RF scan;
- backup/restore is owner-only, staged and fail-closed. Upload/preview makes no
  router call. Confirmed restore cleanly restarts and revokes sessions, keeps
  router writes suppressed until exact-text owner resume, and never silently
  turns restored intent into an Apply. Read-only collection may resume while
  suppressed. Clearing the gate immediately enables automatic 802.11k
  neighbour reconciliation and may write hostapd RRM neighbour state; it does
  not start desired-configuration Apply.

Current bounds are part of the contract: event pages accept 1–1000 rows;
General coverage is stale after 3 minutes and reports missing/empty/gapped
router producers over the retained 24-hour window. Topology accepts at most a
31-day range, retains closed intervals for 31 days, marks current source state
stale after 31 minutes and caps a response at 10,000 intervals. Client
observability accepts at most 31 days, returns complete 5-minute buckets for
ranges up to 7 days or complete hourly buckets beyond that, caps exact events
at 2,000 and path enumeration at 64 results/2,048 visits. Radio decoding caps
32 radios, 128 interfaces/radio, 512 frequencies and 4,096 scan rows; a channel
suggestion requires a completed scan no older than 24 hours, non-stale radio
state, and a channel plan no older than 15 minutes.

`POST /devices/:id/refresh-acl` implements the optional oonfeeWRT controller
capability installation for already-adopted routers whose scoped ACL predates
these reads. A separate `acknowledge_router_changes:true` identifies and
authorizes the exact file it installs or replaces:
`/usr/share/rpcd/acl.d/oonfeewrt.json`. This unlocks controller access to
supported topology, radio channel/scan, OpenWrt log and fixed-target WAN ICMP
observations; it installs no package, binary, daemon, service or firmware. If
the box is left unchecked or the form is cancelled, it makes no request and the
router stays unchanged; dependent source gaps remain visible. If accepted, it
first proves the existing controller login and inventory MAC, pins/verifies SSH
identity, replaces only that ACL, drops cached access state, then proves a fresh
controller login, MAC and capability probe. The administrator password/private
key is neither stored nor returned; this endpoint does not re-adopt, change UCI,
or replace the controller login.

Adoption requires the same `acknowledge_router_changes:true` before Enroller or
SSH execution because it may write that ACL and create the scoped rpcd login.
The read-only Inspect endpoint does not.

`GET /api/v1/site` returns each effective zone as
`{"name":string,"forward_to":string[],"explicit":bool}`. POST requires a
non-null array; DELETE returns 404 when no explicit row exists.

Apply requires a lowercase UUID `operation_id` and the `preview_token`.
Optional `device_ids` narrows the fleet, and
the booleans `acknowledge_traversal`, `acknowledge_driver_risk`,
`acknowledge_cautions` and `acknowledge_partial_fleet` authorize only their
named risks. Missing/stale tokens and failed preflight return without a router
write. The result reports each device's outcome and, on a stopped queue,
`aborted` plus `aborted_after`. Same-request UUID replay is idempotent; UUID
reuse with different input is rejected. The status route returns the durable
write state and device receipts for response-loss recovery.

This Preview + Apply pair produces UniFi's unapplied-change behavior. Model it
explicitly rather than applying on every field edit.

---

## 10. Technology recommendation

| Layer | Pick | Why |
|---|---|---|
| Controller | **Go** | Single static binary, cross-compiles to every OpenWrt arch, excellent concurrent I/O for polling N devices, embeds the SPA with `embed.FS`. Rust is defensible; Python/Node are not (deployment weight on constrained targets). |
| Database | **SQLite** (WAL) | Zero-ops and ample at this scale. Schema-19 portable export wraps `Store.BackupTo` plus matching key material in one authenticated `.oowrtbak`; a manual snapshot or clean checkpoint must still be paired with that state's `keyring.json`. Never copy the live main file alone. |
| UI framework | **React + TypeScript + Vite** | Current implementation; static output embedded in the Go binary. |
| Charts | **uPlot** or **ECharts** | uPlot is dramatically faster for the dense time-series UniFi uses. Avoid Chart.js at these point counts. |
| Topology graph | **Cytoscape.js** or **d3-hierarchy** | The UniFi topology is a tidy tree — d3's `tree()`/`cluster()` layout matches it closely. |
| Tables | **TanStack Table** (headless) | The Client/Flows/Logs grids need virtualization, column customization, and multi-filter. Don't hand-roll. |

---

## 11. Things to steal instead of building

Under the no-device-code rule, "steal" mostly means "configure and read from,"
which is exactly the right relationship.

- **LuCI's JS client API** — `LuCI.rpc` is a working, battle-tested ubus client
  with auth and session handling already solved. Read it as the specification for
  your transport layer. This is the highest-value thing on the list.
- **LuCI's apply/rollback flow** — the countdown-and-confirm UX is solved;
  copy the state machine.
- **`usteer` / `dawn`** — roaming and steering, solved. We render their UCI
  config and read their state. We never join the control loop.
- **`nlbwmon`** — per-host accounting, solved. Official feed.
- **`sqm-scripts`** (CAKE) — queue management, solved, and arguably better than
  UniFi's.
- **`owut` / attendedsysupgrade** — firmware upgrade orchestration. Wrap it if you
  do firmware at all; never reimplement image selection.
- **OpenWISP** — mature, organization-backed, and instructive as a *contrast*:
  it solves the multi-site/NAT problem with a device-side pull agent, which is
  precisely the design we've ruled out. Read it to understand what that choice
  buys and costs, then stay on your side of the line.

---

## 6.2 Per-device overrides — and what they deliberately cannot reach

A device may deviate from the site model in four ways, and only four:

| overridable | not overridable |
|---|---|
| whether a WLAN is published on this AP | SSID |
| whether it beacons its name here | passphrase |
| whether clients here are isolated | security mode, PMF |
| how many clients may associate here | 802.11r/k/v and the derived mobility domain |

The right-hand column is not an oversight and is not a "not yet". Keeping those
settings identical across every AP is the guarantee this system exists to
provide: they are the ones that are miserable to maintain by hand, and they fail
*confusingly* rather than cleanly when they drift. A client roaming between two
APs that disagree about the key does not get an error — it gets an intermittent
drop that looks like interference.

An escape hatch that can break the one guarantee the product offers is not an
escape hatch, it is a slow leak. So those keys are absent from the override
vocabulary entirely rather than present behind a warning, and the API refuses an
unknown key by name with the reason.

**Every deviation is surfaced**, in the settings screen, on the device's row in
the apply preview, and in a site-level summary. The danger of overrides is never
a single one; it is a fleet drifting apart device by device until nobody can say
what is deployed.

Implementation note: overrides are folded into a **copy** of the WLAN during
render. Mutating the site model would leak one device's deviations into the next
device rendered — which, given renders are per device in a loop, would be a bug
that only appears with two or more APs.
