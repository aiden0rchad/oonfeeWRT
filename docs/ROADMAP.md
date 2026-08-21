# oonfeeWRT — Roadmap

Six phases. Each has a **proof** — the one thing that must work before the phase
counts as done. Ship in order; the ordering is chosen so that each phase is
independently useful and each de-risks the next.

The interface/function reference was refreshed on 2026-08-18 against stable
UniFi Network 10.5.67. Its Client Observability and Safe Ops work changes the
shape of Phases 4 and 6 below, but does **not** justify skipping the safety and
site-model work ahead of them.

---

## Phase 0 — Transport & safety (the foundation)

**Status 2026-08-18: complete, and both proofs have been run on hardware.**
Apply with an armed rollback watched changing and reverting on air, and
un-adoption that leaves the device byte-for-byte as it was (STATUS §5ap). The
apply path was re-proven end to end on 2026-08-18 after the review sweep
(§5aw), and re-adoption of both devices exercised discovery → credential →
probe → ACL → pin on 2026-08-18 (§5ax). The later schema-11 function-selection
cycle cleanly un-adopted, inspected, re-adopted and reconverged both routers on
2026-08-18 (§5bc). The later bound-preview/fleet-safety pass added a keyed
server token, whole-selected-fleet preflight, explicit traversal/driver/caution/
partial-fleet gates, per-device revalidation, detached execution, a hard
quiesce boundary and truthful ownership-ledger failure reporting (§5bd).

Schema 14 is now live on the two-router `.run` controller. The promotion used a
stopped schema-13 database/keyring backup, an isolated copy-only rehearsal and a
controlled restart. The live store passed integrity, key-check/scrub, structural
sealing and legacy-value byte scans; passphrase-authenticated `dryrun` reported
zero operations/prunes on both routers, which also had zero pending UCI changes.
STATUS §5bf records the migration artifacts and evidence. After explicit
operator approval, §5bg rotated the managed WLAN key, retired the inspected
pre-v14 material and superseded v14 recovery pair, and verified a new sealed
post-cleanup database/keyring pair. A follow-up found the disclosed old key in
four plaintext router archives; all four were deleted and replaced by two
AES256-encrypted, stream-verified post-rotation archives, with no plaintext
`.tgz` left.

UniFi Network 10.5 now presents the same idea as Test & Confirm inside Safe
Ops. oonfeeWRT already has the load-bearing mechanism: the device owns the
rollback timer, health runs before confirmation, and the UI distinguishes
applied, reverted and unknown. Future work should consolidate that contract
into a Safe Apply & Recovery surface, not replace the proven state machine.
The operator-facing edges are built too: an unroutable discovery sweep is not
reported as an empty subnet; adoption accepts an optional one-time SSH private
key while retaining the password for ubus; and un-adoption refuses to proceed
when its ownership ledger cannot be read.

The mechanism is foundational; its state and recovery paths are deliberately
user-visible.

- Go controller skeleton, SQLite schema, embedded SPA shell.
- ubus JSON-RPC client: `session.login`, token refresh on expiry, retry/backoff,
  TLS with TOFU cert pinning.
- The ACL file (`/usr/share/rpcd/acl.d/oonfeewrt.json`): dedicated user, scoped
  object list, explicit `file.exec` allow-list. Stock rpcd cannot bootstrap this
  over ubus, even as root, so an explicitly acknowledged adoption writes it over
  a one-time SSH session and verifies its hash. This grants capabilities to
  stock rpcd; it installs no package, binary, daemon or service. SSH may use the
  device password or an operator-supplied private key; the password remains the
  separate ubus credential. Neither operator credential is persisted.
- Capability probe + capability registry (ARCHITECTURE §6.1).
- Adoption: discover → credential → read-only ubus inspection → explicit
  Gateway/AP/Switch selection and capability-extension acknowledgment →
  serialized uniqueness preflight → write ACL → create scoped user → verify that
  login → capability probe on that scoped
  session → pin cert/host key → discard the original credential. Inspection
  performs no SSH/bootstrap/store write. A second managed Gateway is refused
  before device contact; AP-only is valid in an empty fleet with an external
  gateway.
- **Un-adoption**, in the same sprint: remove user + ACL, optionally revert every
  UCI section we own. The ownership ledger has an explicit known/unreadable
  signal and both destructive actions fail closed until it is known. If a
  partial removal leaves residue, the final report carries exact, validated
  stock-OpenWrt cleanup and verification commands. The login/ACL and inventory
  row remain unless the config hand-back is proved complete (or the operator
  explicitly forces inventory removal). A wrapper that can't cleanly remove
  itself doesn't get trusted.
- **The apply cycle**: batched staged `set`/`add`/`delete` → `apply {rollback,
  timeout}` → health probe → poll `confirm`, with a full audit record. **No
  `commit` before `apply`** — apply is what commits the staged delta with the
  rollback snapshot, so committing first silently disarms the protection
  (IMPLEMENTATION §6 has the state machine; ARCHITECTURE §4 the reasoning).
  `confirm` must go out on the token that applied. While the timer is armed a
  new login may return that same token, so health uses runtime/non-UCI evidence
  and never destroys a session that is shared with the applier; configuration
  verification uses a genuinely fresh session only after the window resolves.
- Preview returns an opaque keyed token bound to the complete desired site,
  adopted fleet, ownership state and plans. Apply rebuilds and verifies that
  state before any write, preflights every selected device before the first,
  and aborts later devices on the first non-applied outcome. It outlives an
  HTTP disconnect under one bounded drain deadline; per-device polling is
  quiesced through in-flight sink emission and refreshed on release. A device
  is not reported/audited as cleanly applied until the controller has recorded
  its ownership ledger. Schema 13 records a caller-generated operation UUID,
  idempotently binds it to the request and exposes durable parent/per-device
  status so a reload or lost response can recover the result without retrying
  a router write.
- Schema 14 seals WLAN and mesh keys plus secret-derived ownership verifiers,
  binds the database to its keyring with a sealed key check before mutation, and
  completes a crash-resumable checkpoint/VACUUM scrub before serving. Keyring
  creation never overwrites an existing file, and a missing keyring beside an
  existing database is a hard refusal. Authenticated API reads expose
  `has_key`, never plaintext, including legacy reveal-shaped URLs.
- Ownership tagging + the "what will change on this device" diff preview.

**Proof:** two things, both required. (1) Deliberately push a config that breaks
the device's uplink — the device must come back on its own within the timeout and
the controller must report the failure honestly. **Inside the armed window the
failure must be detected from runtime/non-UCI evidence** (reachability, netifd,
iwinfo or hostapd state), never from `uci.get` on the applying token. After the
window resolves, a genuinely fresh session verifies the committed/reverted
configuration. (2) Adopt a device, make changes, un-adopt it, and diff its config
against a pre-adoption snapshot — the only residue should be nothing. Note
un-adoption needs the operator credential re-prompted (ARCHITECTURE §6); the
controller's own login deliberately cannot remove itself. *Do not proceed until
both work.*

---

## Phase 1 — Read-only fleet view

**Status 2026-08-18: built and running on two devices.** Inventory, adoption,
the poll ladder, the rollup tables, Dashboard, Devices, and the Client Devices
grid all exist and are exercised daily against real hardware. The shared
`DataGrid` (virtualization, column prefs, filter rail) is used by Clients,
Devices and — since §5ax — Networks. The resource-budget harness is built. Its
full 60-minute hardware gate passed on the constrained QCA956X Archer C6 v2 on
2026-08-18: 30 minutes each at idle/focused cadence, 209 poll batches with zero
failures, unchanged flash snapshots/write counters and no package or UCI write
(STATUS §5bd; DEVICE-BUDGET §7).

The first 2026-08-19 browser pass found three Phase-1 presentation
disagreements: Dashboard's client total versus Client Devices/device detail,
Logs' error-filter count versus visible rows, and Discovery's inferred
Gateway/DHCP label for the AP + Switch C6 despite disabled LAN DHCP. All three
fixes are regression-tested and live-reconciled under the rebuilt schema-14
asset: Dashboard/Client Devices agree on one wireless client, the one-error Logs
facet shows one row, and Discovery uses generic capability wording (STATUS
§5bf).

The first thing anyone sees.

- Inventory: adopt N devices, show model/firmware/uptime/IP and the independently
  selected Gateway/AP/Switch functions. The legacy primary role is display/API
  compatibility, not rendering authority.
- On-demand discovery that distinguishes “nothing answered” from “the
  controller could not route to this network”; the latter names the affected
  CIDR and tells the operator to check the controller host's routes/interfaces.
- Telemetry loops (live/standard/slow) + TSDB rollup ladder.
- **Screens:** Dashboard (WAN throughput, fixed-target ICMP latency/loss/reachability, device counts),
  Devices list + device slide-over, Client Devices grid with the full column set.
- The shared table component: virtualization, column customization, filter rail
  with live counts.
- **The resource-budget harness** (DEVICE-BUDGET §7): adopt a class-C device, run
  baseline and focused polling for an hour, assert CPU/RAM/request-rate/zero-flash
  -writes. Build it alongside the collectors, not after — a budget nobody measures
  is a wish.
- The per-device **Management Overhead** readout in the UI.
- The shared chart component: crosshair, tooltip, time-range control, rollup
  switching, min/max bands.

**Proof:** open the Client Devices grid on a 40-client network and it is faster
and more informative than LuCI's status page. That's the moment the project
becomes real to a user.

---

## Phase 2 — The site model (this is the product)

Everything before this was a nicer LuCI. This is where it becomes a controller.

- Site → WLAN → APGroup → Device render pipeline with ownership tagging.
- Pending-changes batching + the Apply flow in the UI.
- **Screens:** Settings → Overview, Settings → WiFi (full SSID options),
  AP groups, per-device overrides with explicit conflict surfacing.
- Consistent 802.11r/k/v config across all APs — the thing a controller uniquely
  guarantees.
- `usteer` or `dawn` configuration + state readout.

**Proof:** change one SSID's password once; it lands correctly on three APs
across two bands each, with no manual per-device work, and a hand-edited LuCI
section elsewhere on those devices is untouched.

> **Status: met for TWO APs, not three.** Everything in the proof has been run
> on real hardware across two devices and four radios — including the untouched
> hand-edited section, and including the full adopt → change → un-adopt → diff
> round trip, which comes back byte-for-byte identical (2026-08-17). The "three"
> is unmet purely for want of a third device;
> see the not-tested table in the README. Nothing in the pipeline is per-device
> — the render is driven by group membership and the mobility domain is derived
> rather than coordinated, precisely so that adding an AP needs no new mechanism
> — but that is a reason to expect it to work, not evidence that it does.
> The 2026-08-18 schema-11 cycle re-proved this two-device state after a clean
> un-adopt: WRT as Gateway + AP + Switch, C6 as AP + Switch, two WLAN creates
> each, then 0 changes with all four radios broadcasting (§5bc).

---

## Phase 3 — Networks, zones, policy

**Status 2026-08-20: the whole-zone Phase-3 subset and two-client no-LAN path
are hardware-proven.** The renderer produces the whole stack
for a device with Gateway selected—bridge-VLAN, addressed interface, DHCP
server, firewall zone and its forwarding—and STATUS §5as rewrote zones to render
once per zone with their networks as a UCI list. AP independently gates WLANs;
Switch records wired participation/visibility without promising unsupported
per-port writes. The Networks grid and editor now
configure DHCP enablement, pool start, lease count and lease time. The UI,
model, API and renderer reject invalid CIDRs, pools outside the subnet, pools
containing the gateway and invalid lease times before a device plan exists;
older rows retain the historical `100`/`150`/`12h` behavior. Turning DHCP off
removes only the owned server, and a foreign DHCP server on the managed
interface is a blocking conflict rather than something the controller edits.

The directional forwarding subset is now built end to end: schema-12 site
policy, validated API/store/model, owned firewall4 render, effective Master
Table and editable Zone Matrix. Each managed source has an explicit
`forward_to` set; no row preserves the historical Internet-only edge to foreign
`wan`, while an explicit empty set blocks all modeled forwarding. Direction is
independent and return traffic uses firewall conntrack state rather than an
invented reverse allow. Foreign UCI forwarding/rule/DNAT contradictions block
Preview and are never edited. Active foreign firewall includes and reachable
non-fw4 nftables policy also block explicit matrix policy; an unreadable or
malformed runtime ruleset fails closed instead of being treated as clean.

The WRT3200ACM is adopted as Gateway + AP + Switch. After an explicit,
operator-owned one-time conversion put management on `br-lan.1`, the signed-in
browser applied VLAN 2, `br-lan.2` at `192.168.2.1/24`, configurable DHCP and
the zone-LIST/firewall4 stack. A real Mac client proved DHCP, DNS and WAN;
Policy Engine then blocked WAN while retaining DHCP/DNS and restored it; DHCP
disable removed the live range, and a `50`–`59`/`1h` pool issued `.54` for
exactly 3600 seconds. The legacy-swconfig C6 stayed a truthful no-op and omitted
the VLAN-bound WLAN. STATUS §5be has the durable operation IDs and rollback
evidence. The confirmed §5bg cleanup retained VLAN2 and its seven WRT sections,
restored DHCP `100`/`150`/`12h`, removed the temporary WLAN, reset `lan2` to its
legacy WAN-only policy provenance and ended with a zero-change Preview/dryrun.
A later run put two physical iPhones on the same isolated WRT BSS at once. Both
received distinct DHCP leases, loaded HTTPS from `1.1.1.1` and `example.com`,
and failed against a known-live Mac LAN HTTP listener. UCI held `isolate=1` and
`bridge_isolate=1`, and sysfs reported `isolated=1`. Reciprocal raw Safari
peer-IP failures lacked a known-live peer listener or positive control, so they
do not close literal bidirectional peer data-plane isolation. A durable cleanup
operation removed the proof WLAN with one WRT prune
and a C6 no-op; the following fleet plan was zero-change and the separately
operator-created Guest network on VLAN 3 remained applied. That is the current
live Phase-3 proof boundary. The current schema-15 source
goes further: the Master Table includes explicit IPv4 firewall rules, port
forwards, static routes and client block/fixed-IP/group desired state. A partial
Object Manager selects client devices, groups or managed networks and compiles
inspectable, unsaved `Secure` IPv4-reject drafts or static **network** routes.
QoS/application outcomes and per-device/group policy routing return visible
capability gates; nothing is invented. Chosen drafts still require a separate
save, Preview and Apply. The 2026-08-20 signed-in pass compiled one visible
static-route draft and left it unsaved/unapplied; that proves the compiler/UI
path without changing the database or routers, not the Apply path.

Two limits are deliberate and documented rather than pending: oonfeeWRT will
not make a bridge VLAN-aware itself (the WRT conversion was an explicit
operator-owned prerequisite), and it will not add a network to a firewall zone
it did not write.

- Networks/VLANs with DHCP, DNS, IPv6, bridge VLAN filtering.
- **Built subset:** Zone model → owned firewall4 zones + directed forwardings;
  a Zone Matrix with source rows, destination columns, independent direction,
  and an effective forwarding Master Table.
- **Built in current source:** one inspectable Master Table for zone forwarding,
  IPv4 firewall rules, port forwards, static routes and client block/fixed-IP/
  group intent; partial Object Manager compilation for `Secure` and static
  network `Route` outcomes.
- **Remaining expansion:** QoS/rate-limit and application/DPI backends,
  per-device/group policy routing, ordered-overlap semantics, switch ACLs and
  richer reusable sets. Every unavailable outcome stays an explicit gate.
- **Screens:** Settings → Networks, Policy Engine, per-client policy.

**Proof:** create a guest VLAN with client isolation and no LAN access, in the
UI, in under a minute, and have it verifiably enforced.

> **Status: transport, whole-zone enforcement and two-client no-LAN behavior are
> proved; the literal peer-isolation edge remains partial.** §5be proves the
> UI-created VLAN, real DHCP/DNS/WAN client traffic, WAN block/restore, runtime
> firewall shape and DHCP off/custom states. The latest run proves two clients
> simultaneously on one isolated BSS, each with DHCP/DNS/WAN success and each
> denied access to a known-live LAN listener. Reciprocal raw Safari peer-IP
> failures had no known-live peer listener or positive control, so they are not
> promoted to bidirectional peer data-plane proof. Cleanup removed the proof
> WLAN, preserved the operator's Guest VLAN3 and ended at a zero-change Preview.

---

## Phase 4 — Insights, topology, logs

The screens that make people *enjoy* the tool.

**Live status 2026-08-20: the source contract and explicit capability path are
exercised.** The controller is promoted to schema 16. An initial signed-in pass
used the routers' older ACLs; that no-router-change checkpoint was superseded
when the operator explicitly acknowledged ACL refresh for both routers at
15:16 and 15:17. Subsequent polls persisted OpenWrt-log and topology-source
observations from both routers and fixed-`1.1.1.1` ICMP observations from the
Gateway. The refresh code installs no package, binary, daemon or service. No
before/after package-inventory hashes were taken, so this live proof does not
claim that package inventory was unchanged. The contract remains bounded and
gap-aware:

- schema 16 persists producer-provenanced events/cursors, half-open topology
  intervals/source state and explicit RF-scan runs, with full schema
  attestation on migration;
- Client Observability joins durable rollups, exact events and persisted path
  intervals under one cursor. It persists no raw poll samples; `wifi-v1` is
  fixed 45/35/20 RSSI/retry/failure and all-or-null;
- site health is the gateway's fixed once/minute, three-packet ICMP probe to
  `1.1.1.1`—not HTTP validation or a configurable multi-target SLA;
- Logs are keyset-paginated REST. General coverage distinguishes missing,
  observed-empty, stale (3 minutes) and retained producer gaps; WebSocket
  exposes only `device.stats`, with a 32-frame drop-on-full queue;
- topology history is retained/range-limited to 31 days, current source state
  is stale after 31 minutes, and one response is capped at 10,000 intervals;
- radio inventory uses stable UCI radio keys, refreshes inventory/frequencies
  on the 15-minute cadence, exposes last-known freshness, and permits only an
  explicit, disruption-acknowledged scan. Suggested channels require a scan
  no older than 24 hours plus a channel plan no older than 15 minutes;
- an adopted router with an older scoped ACL can optionally use the explicit
  ACL-refresh workflow. It requires separate opt-in, writes or replaces exactly
  one rpcd ACL JSON file, and installs no package, binary, daemon or service.
  Its administrator SSH credential is one-request-only; UCI, ownership and the
  controller login stay unchanged. Declining leaves the router unchanged and
  dependent observations explicitly unavailable.

After the explicit refreshes, both routers produced current topology-source and
OpenWrt-log observations, and the Gateway produced fixed-target ICMP
observations. This does not retroactively create historical source-coverage
snapshots: history continues to report that coverage as unavailable rather
than borrowing today's state. Stable radio identity and truth-gated metrics
remain in place; DFS is not inferred, and no disruption-acknowledged RF scan
was run merely because scan access became possible. Client Observability keeps
one cursor across client/AP/radio/path evidence and now has measured fixed-
`1.1.1.1` source data where completed rollups cover the selected interval.

Persisted RF scans are now bounded on the five-minute maintenance tick: only
the newest terminal result per `(device_id, radio_key)` is retained,
pending/running work is never pruned, and deleting an older terminal run
cascades to its BSS rows. Historical topology/log source-coverage snapshots
remain unstored; historical APIs say that coverage is unavailable instead of
borrowing the current state.

- LLDP + fdb + ARP + assoc → topology graph.
- **Client Observability:** a correlated 24-hour timeline joining associations,
  roaming, signal, retries, latency/loss, AP health, site health and events.
  One time cursor drives every chart and path summary. Application identity is
  shown only where Phase 5's DPI capability exists.
- Infrastructure Topology history: wired downlinks, uplink changes,
  third-party-device connections and grouped cascading offline/online events.
- Survey/station-dump derived metrics: **channel utilization** (portable — the
  one that works everywhere measured so far) and TX retries; interference and
  the airtime split only where the driver reports usable `rx_time`/`tx_time`,
  which mwlwifi does not. The `iwinfo.assoclist` field surface is now captured
  against real associated stations (IMPLEMENTATION §14.3), so the per-client
  columns can be specified from measured fields rather than assumed ones.
- The fixed `wifi-v1` Experience score with its three components exposed. It is
  null when any input is missing; it never renormalizes around a capability gap.
- Channel Plan + suggested-channel scoring.
- OpenWrt `log.read` ingest plus controller/audit events → an enriched,
  provenance-preserving event store and detail slide-over. nflog/GeoIP remains
  later work rather than being implied by the shipped general log.
- **Screens:** Client Observability, Topology, Insights → Radios, Logs (General
  + Audit).

**Proof:** select a bad client and move the 24-hour cursor to the incident; the
same surface shows whether the cause was its signal/retries, an AP/uplink event,
or site latency/loss—without touching a terminal and without inventing a metric
the hardware did not supply.

> **Status: complete with the optional ACL capability path explicitly
> exercised.** The schema-16 joined surface, topology/history, Radios, General
> Logs and Audit interaction are live-rendered. Current OpenWrt-log,
> topology-source and fixed-ICMP producers have observations from both refreshed
> routers where applicable. Historical source coverage remains unavailable by
> design; DFS and scan results remain evidence-gated, and no disruptive scan was
> run. ACL refresh remains default-off for other routers.

---

## Phase 5 — Flows & security

The expensive phase. Deliberately after the portable core.

- `netifyd`/nDPI on the gateway → flow records with application identification.
- Flow store with aggressive retention + summary rollups.
- Risk heuristic (blocklists + geo + ports + optional Suricata verdict),
  documented and non-magical.
- **Screens:** Flows, Flows on Map, Top Apps/Destinations, Security settings.

**Proof:** "which device is talking to a host in a country I don't do business
with" is answerable in three clicks.

---

## Phase 6 — Fleet operations

- **Built:** durable Apply operation IDs, idempotent request binding and a
  status/result endpoint. The browser recovered a running/failed operation
  after a reload during the live §5be pass, including its per-device write
  boundary and reverted outcome.
- Backup/restore of controller config and per-device UCI snapshots. A controller
  restore artifact must pair a consistent SQLite snapshot with its matching
  `keyring.json`; a database or passphrase alone cannot recover the random data
  key. Pre-v14 backups may contain plaintext WLAN/mesh keys and ownership hashes,
  so protect them and never delete them without explicit operator confirmation.
- Alarm Manager: explicit trigger → scope → action rules, schedules, cooldowns
  and repeat suppression; webhook/ntfy/email plus CEF/SIEM export.
- Safe recovery controls over collector health: monitor-only by default,
  operator-consented actions, bounded retries and visible last action.
- HA gateway pairing via `keepalived`/VRRP.
- Firmware: **surface** available updates via `owut`/attendedsysupgrade, and
  optionally orchestrate staggered upgrades. Treat this cautiously — it is the
  one place a wrapper can do irreversible damage, and the user chose stock
  OpenWrt partly to control their own upgrade cadence. Defaulting to "notify,
  don't touch" is the respectful posture.

**Proof:** upgrade three APs, staggered, without dropping the WLAN — with the
user pressing the button each time.

---

## Explicitly out of scope, permanently

These are ruled out by the project's constraints, not deferred:

| Out | Why |
|---|---|
| Device-side agent or daemon | Violates the no-device-code rule (ARCHITECTURE §0) |
| Multi-site / NAT traversal | Requires a dial-out agent. Answer: a WireGuard tunnel the user already runs |
| Custom firmware, forks, package feeds | We don't maintain OpenWrt |
| Adopting UniFi or other non-OpenWrt hardware | No inform protocol, no vendor APIs |
| Cloud remote access, SSO brokering | A service business, not a feature |
| Native mobile apps | A responsive web UI covers it; native is a second project |
| Spectrum analysis, paid threat feeds, AI-branded features | Proprietary silicon or paid data |

---

## Effort reality check

The OpenWrt forum thread on exactly this idea contains a developer noting they'd
worked on a comparable product for 3+ years with a small team, and that it "is a
very involved project; not at all trivial." Treat that as calibration, not
discouragement.

Phases 0–2 are the ones that matter. A tool that does only those — safe,
multi-device WiFi configuration from one screen with a decent live view — would
already be the best OSS answer to "I want UniFi but OpenWrt." Phases 3–6 are
where the years go. Scope accordingly, and ship Phase 2 before you touch Flows.
