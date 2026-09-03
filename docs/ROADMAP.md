# oonfeeWRT — Roadmap

Six numbered phases, with a focused Phase 4.1 between observability and DPI.
Each has a **proof** — the one thing that must work before the phase counts as
done. Ship in order; the ordering is chosen so that each phase is independently
useful and each de-risks the next.

The interface/function reference was refreshed on 2026-08-18 against stable
UniFi Network 10.5.67. Its Client Observability and Safe Ops work changes the
shape of Phases 4 and 6 below, but does **not** justify skipping the safety and
site-model work ahead of them.

**Current patch roll-up (v0.1.4):** v0.1.2 shipped the bounded, sanitized,
browser-local compatibility report on successful read-only Inspect and corrected
physical-radio/direct-Ethernet inspection for the externally reported Cudy
M3000 v2 variant. v0.1.3 replaced WAN interface-name heuristics with a
read-only installed-route + active-netifd proof, including logical PPPoE `wan`
to runtime counter device `pppoe-wan`. Equal-best defaults, multipath, custom
policy routing, `mwan3`, manual selection, per-uplink health and bond-member
attribution remain explicit gaps. v0.1.4 adds explicit per-network IPv6
preserve, prefix-delegation and disabled policies behind Preview and Apply;
read-only router-clock observation; bounded IPv6 warning conditions; transitive
wired-topology presentation fixes; reduced unchanged-topology churn; and an
explicit Docker management-address bind. Existing networks remain Router
managed after upgrade, so the release does not silently broaden router writes.

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

The historical schema-14 promotion on the two-router lab controller used a
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
  gateway. A successful Inspect may also return the server-built compatibility
  report; downloading it adds no router request, persistence or upload, and an
  unsafe/oversized report is omitted without failing Inspect.
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
- **Screens:** Dashboard (WAN throughput from the uniquely proved installed
  main-table route/netifd runtime device only when its exact RX/TX series key
  exists, fixed-target ICMP
  latency/loss/reachability, device counts),
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
browser applied VLAN 2, `br-lan.2` at `198.51.100.1/24`, configurable DHCP and
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

**Hardware status through 2026-08-22: both explicit capability paths are
exercised.** The controller is promoted to schema 17. The tag-triggered workflow
published `v0.1.0-rc.1`; its public binary archives and multi-platform container
passed the zero-device clean installation recorded in FS-119. An
initial signed-in schema-16 pass
used the routers' older ACLs; that no-router-change checkpoint was superseded
when the operator explicitly acknowledged ACL refresh for both routers at
15:16 and 15:17. Subsequent polls persisted OpenWrt-log and topology-source
observations from both routers and fixed-`1.1.1.1` ICMP observations from the
Gateway. The refresh code installs no package, binary, daemon or service. No
before/after package-inventory hashes were taken, so this live proof does not
claim that package inventory was unchanged. The contract remains bounded and
gap-aware:

- schema 17 retains schema 16's producer-provenanced events/cursors, half-open topology
  intervals/source state and explicit RF-scan runs, with full schema
  attestation on migration, and adds a durable optional-capability package and
  service rollback ledger;
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
- unsupported LLDP evidence may separately offer the official-feed `lldpd`
  capability. Package-index refresh/plan resolution and package/service
  installation have distinct unchecked acknowledgements; the reviewed plan is
  bound, actual additions and prior service state are durable. Physical-interface
  planning is read-only; changing only `lldpd.config.interface` requires another
  acknowledgement and retains the exact UCI baseline. Rollback drift-checks and
  restores that baseline, removes only the recorded additions, independently
  verifies package/service state, and must complete before un-adoption. Adoption
  never selects this option.

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

> **Status: hardware-verified with the optional ACL and LLDP capability paths
> explicitly exercised; final release-candidate gates pass.** The v40 result is
> the local evidence behind the subsequently published `v0.1.0-rc.1` tag and
> public multi-platform container. The schema-17 joined
> surface, topology/history, Radios, General
> Logs and Audit interaction are live-rendered. ACL refresh remains default-off.
> The separate LLDP workflow proved exact official-feed plans, installation,
> physical-interface configuration, read-only diagnosis, drift-checked rollback,
> verified package/service cleanup and clean reinstallation on both reference
> routers. The final signed-in `/topology` deep-link check has five nodes, four
> current links, one measured AP-to-gateway edge and no reciprocal stale edge.
> After a complete poll, association coverage is observed/observed-empty; only
> the two BusyBox VLAN ambiguity gaps remain. Historical source coverage remains
> unavailable by design; DFS
> and scan results remain evidence-gated, and no disruptive scan was run merely
> to prove access.

---

## Phase 4.1 — UI polish, controller operations, and v0.1.0

Phase 4 proved the data and safety contracts. Phase 4.1 makes those capabilities
feel coherent, adds the controller operations needed for support and recovery,
and promotes the already-published `v0.1.0-rc.1` pipeline to final schema-19
`v0.1.0`.
This is an incremental redesign, not a UniFi clone: keep the current routes,
truthful unavailable states, accessible tables and topology workspace; copy no
UniFi assets or proprietary visual details.

**Historical live checkpoint (2026-08-23):** the development controller used
schema 19. The controlled upgrade/restart completed with exact
binary version `dev-phase41-live-schema19`; recovery verification reports two
devices, two credentials, one enabled owner, one WLAN and no mesh. A signed-in
live UI smoke passed Dashboard, Accounts, Diagnostics, Backup & Restore,
Devices and Topology with no browser errors. Fresh schema-17 rollback and
schema-19 recovery sets also passed verification. This was a route/render smoke,
not a backup export, restore execution, diagnostics generation/download,
public-provider speed-test run or router restore. Publication state is not
embedded in this roadmap: the completed `v0.1.0` tag workflow and GitHub Release
are authoritative, while `v0.1.0-rc.1` remains the historical upgrade baseline.

### 4.1.1 Visual system, navigation and dashboard

Source status: the project-owned SVG rail and summary-first `Notice` primitive
are in place. Dashboard now renders the server-selected six-hour WAN series:
fixed ICMP `1.1.1.1` latency/loss/reachability, exact-interface RX/download and
TX/upload only, independent freshness, and null gaps. Its compact topology
summary now uses the current topology snapshot, separates active from
last-known placement, and links to the full graph. The recent activity card now
uses a separately bounded warning/error query, so newer informational events
cannot hide alerts and an unavailable query cannot look like an empty history.
An automated pinned-Chromium gate exercises the signed-in Dashboard at 1280×720
and 1440×900 in both themes, with expanded navigation, horizontal-overflow
checks and keyboard disclosure behavior; a partial Topology fixture also proves
that review actions remain available while technical details are collapsed.
The same gate covers Devices, Client Devices and Logs at both desktop sizes;
those routes now share one wrapping page-header hierarchy with truthful
loading/unavailable counts and preserved controls. Extending that hierarchy to
the remaining routes and the rest of the desktop/text-density pass is still
open.

- Replace the font-dependent Unicode rail glyphs with one coherent set of
  project-owned inline SVG icons. Render them at 22–24 px in controls at least
  44 px square, with visible hover/focus/active states, accessible names and
  tooltips. The rail can expand to show labels; the preference persists.
- Establish reusable page headers, metric cards, notices, disclosures and
  capability cards. Move repeated layout into tokens/classes incrementally;
  do not rewrite the working grids or screen routing.
- Expand Dashboard with real, freshness-aware WAN throughput, fixed-target ICMP
  reachability/latency/loss, devices, clients, recent warning/critical events,
  a compact topology summary and speed-test history. Label the current probe
  `ICMP reachability to 1.1.1.1`—never ISP uptime—and state WAN-interface
  direction semantics. Missing or stale evidence says `Unavailable` or `Last
  observed`, never zero.
- Distinguish loading, empty, partial, stale and failed states. Desktop dark and
  light themes must remain keyboard-usable, WCAG-AA readable and free of clipped
  labels or unintended horizontal overflow at 1280×720 and 1440×900. A broader
  mobile redesign remains deferred.

### 4.1.2 Speed tests

Source status: controller mode is implemented as an explicitly acknowledged,
single-stream Cloudflare direct test with a 15 MiB estimate and 30-second hard
limit. The pre-run descriptor names vantage point, endpoint, method, privacy and
saturation; jobs are durable, one-active, bounded-history, cancellable, audited
and restart-recovered. The launcher keeps material impact visible and puts exact
details in a nonmodal popover; selecting Run is the fresh acknowledgement bound
to the current deterministic `plan_id`. A changed plan is rejected before job
creation. There is no Fleet/router
dependency. Loaded latency and jitter remain null because this method does not
measure them. No public-provider speed test has run; the live Dashboard route
has passed signed-in smoke.

The first implementation runs from the **controller host/container**. It makes
no router call or change and must say so beside the Run button. Before every
run, keep provider, vantage point, estimated data use, timeout, privacy and
temporary WAN-saturation impact visible; exact endpoints and method remain
available from the keyboard/touch-accessible impact popover.

- Persist the three newest terminal attempts with download/upload throughput,
  idle/loaded latency, jitter, method, provider, timestamp and provenance.
  Unknown fields stay null; the separate active job never crowds out history.
- Use server-side `queued`, `running`, `cancelling`, `completed` and `failed`
  jobs; allow one active controller test, hard-limit time and bytes, expose
  progress/cancel, and audit start/cancel/complete/failure.
- Never run a test automatically or on a schedule by default. An integration
  test must prove controller mode makes zero router management/API/SSH calls and
  zero router writes/installs. Its test traffic still follows the controller
  host's normal route and may saturate the gateway/WAN.
- Treat a future **gateway-run test** as a separate, default-off capability,
  never as part of adoption or the Dashboard button. Only official OpenWrt-feed
  packages are eligible. Package-index refresh, exact plan, installation, test
  execution and rollback are separate actions. Before installation show exact
  packages/dependencies, download and installed size, services/configuration,
  commands/method and rollback. Bind a fresh unchecked acknowledgement to the
  reviewed plan and reuse the durable package/service-baseline ledger. No
  controller-authored router executable, daemon, feed or firmware is allowed.
  This gateway mode is deferred from the required `v0.1.0` proof; if it ships in
  that release, real OpenWrt plan/install/run/rollback evidence becomes a gate.
  Unsupported package managers/releases fall back to controller mode. Rollback
  drift-checks state, removes only ledger-recorded additions, preserves every
  pre-existing package, restores prior service/configuration state and verifies
  cleanup. Any ACL expansion is a separate reviewed capability acknowledgement;
  run/cancel/timeout/concurrency/audit bounds match controller mode.

### 4.1.3 Progressive disclosure instead of walls of text

Source status: authored `Notice` summaries now cover Dashboard methodology,
topology/radio/log coverage, Policy lifecycle and Zone Matrix scope, Apply
readiness/behavior/management-path/driver-risk/per-device previews, adoption and
optional capabilities, diagnostics, backup/restore, neighbour reports and
wireless-uplink guidance. Passive information on Dashboard methodology, general
event sources, account/session guidance, Zone Matrix scope, neighbour reports
and wireless uplinks now uses the shared nonmodal details popover. Warnings,
errors, adoption authorization, Apply plans, actions and acknowledgements remain
inline; critical consequences and RF scan consent remain fully visible.
Remaining control-adjacent Settings help stays inline by design; other long
Settings guidance remains an incremental cleanup.

Use one authored disclosure contract: `summary`, `details`, severity, affected
component and always-visible actions. A passive informational row stays one or
two lines and `More information` opens the complete technical text without
changing page height. Inline warnings and errors retain their existing details;
nothing is discarded or duplicated for assistive technology.

- Apply it to Dashboard explanations, topology/radio/log coverage, adoption and
  ACL prompts, LLDP and other optional capabilities, RF scans, Apply previews,
  diagnostics, backup/restore and long Settings help.
- Passive informational details may use the popover and default closed. Coverage
  warnings, errors, authorization and optional-feature consent remain inline.
- Security, destructive and connectivity-loss notices remain prominent. An
  optional capability starts as a compact summary plus `Review`; after Review,
  its exact router mutation/rollback, fresh checkbox and action remain visible
  and default-open. Never auto-collapse an active consent plan.
- Mouse, keyboard and touch opening, dismissal, `aria-expanded`, focus return,
  narrow containment and representative desktop layouts are release-tested.

### 4.1.4 Accounts, roles and sessions

Source status: schema 19 implements the account foundation and management UI. The
migration preserves each existing bootstrap administrator and makes it an
enabled `owner` without changing its password. Canonical roles are `owner`,
`admin`, `operator` and `viewer`. New usernames use an ASCII-only grammar and a
unique `COLLATE NOCASE` index; an ASCII case collision fails the migration
transaction. Soft deletion disables authentication, removes the verifier and
reserves the username. Conditional writes preserve one enabled owner under
concurrency, and account mutations commit with their audit event or roll back
together. Sessions carry their canonical role; every protected REST route and
`/live` is server-authorized. My Account and owner administration expose account
creation, role/state/password changes and session listing/revocation. Owner
mutations require a fresh five-minute password step-up. Revocation closes
`/live` and cancels in-flight requests. This account-security scope is complete.

Preserve the existing Argon2id passwords, login throttling, CSRF protection,
secure cookies, expiry, password change and audit events while completing the
remaining surfaces:

- Server-enforce four roles on every protected route and live channel; only
  health, setup/bootstrap and login remain intentionally anonymous: `owner` (all,
  including accounts and restore), `admin` (device/site mutation,
  capabilities and diagnostics), `operator` (read plus acknowledged controller
  speed tests and transient RF scans), and `viewer` (ordinary non-secret
  reads only). Gateway package-index/install/run/rollback, credential-bearing
  reads, diagnostics, backup and account operations require their explicit
  higher privilege. UI hiding is not an authorization boundary.
- Expose account list/create, role change, enable/disable, soft deletion and
  password reset through owner-authorized API/UI paths backed by the landed
  store invariants. A table-driven matrix must assert
  allow/deny behavior for every protected REST route and live channel, including
  sensitive GET/download endpoints.
- Retain listable metadata in the existing in-memory session store; sessions
  never survive controller restart. A user can revoke their own sessions and
  an owner can revoke any. Capture client address only under the controller's
  documented trusted-proxy policy. Disabling an account or changing its
  password/role revokes the affected sessions immediately. Audit every account,
  role, session and login-security action without recording secrets. No default
  credentials are allowed.

### 4.1.5 Downloadable diagnostics bundle

Source status: implemented for `owner`/`admin` as stored-only descriptor,
generate, status, cancel and download API/UI. One active cancellable job,
bounded terminal history/retention, private mode-0600 files, fixed members,
manifest/checksums and startup/shutdown cleanup are enforced. The private
bounded rotating controller JSONL sink is implemented. Descriptor and bundle
gaps state that pre-existing Docker/service-manager logs outside this sink are
unavailable. `mode: "stored"`, `router_management_calls: false` and
`router_changes: false` are fixed: generation has no Fleet dependency and makes
zero live router management/API/SSH calls or router changes.

The bundle includes bounded controller logs/errors; controller version, schema,
platform, uptime, health and migration/integrity results; General/Audit event
summaries; topology/radio/source coverage; and each device's stored model,
target, firmware, kernel, package manager, last-observed time and capability
state.

The ZIP opens with a human-readable `README.txt` summary of controller and
router models/versions, detected gaps, collection times, redaction policy and a
member index so diagnosis does not require custom tooling.

- No live-router refresh exists in current source. If added later, it must be a
  separate unchecked option that lists exact read-only methods and never writes,
  installs, restarts or initiates RF scans; an offline device becomes a gap
  rather than failing the bundle.
- Redact configuration summaries and pseudonymize client identifiers by
  default. Never include database/keyring files, controller/export passphrases,
  password hashes, device credentials, private keys, session/CSRF tokens,
  WLAN/mesh keys, TOTP secrets, recovery codes or raw secret-bearing UCI.
- Use safe fixed ZIP paths, row/file/total bounds, mode-0600 temporary files and
  cleanup on success, error and cancel. Audit generation/download/cancel without
  logging contents. Database integrity collection uses a bounded read-only
  snapshot with a timeout. Redact free-text logs/events and use one stable
  per-bundle pseudonym map across every member. Seed every secret class in tests,
  extract every member, then scan filenames, decompressed contents, manifest and
  metadata; searching compressed bytes alone is not a valid leak test.

### 4.1.6 Encrypted controller backup and restore

Source status: implemented end to end at schema 19. Owner-only export produces
one versioned native `.oowrtbak` containing a transactionally consistent SQLite
snapshot and matching wrapped key material, including accounts, settings,
desired state, sealed credentials and capability ledgers. Bounded Argon2id and
authenticated encryption use a separate export passphrase; the passphrase is
never retained or logged and the live data key is rewrapped rather than exported
raw. Export/status/download, restore upload/preview/cancel/confirm, and persistent
router-write suppression/status/resume are available through owner-authorized
API/UI surfaces over TLS or direct loopback. Sensitive actions require recent
password reauthentication.

This is a controller backup, not an undisclosed export of every live/foreign
router UCI value. Controller-owned desired configuration round-trips here;
separately reviewed per-device UCI snapshots remain Phase 6.

- The manifest authenticates app/format/schema versions, creation time, member sizes and hashes.
  Wrong passphrase, corruption, truncation, mismatched DB/key material or an
  unsupported newer schema fails before mutation.
- Restore is bounded raw upload → decrypt/validate in disposable staging →
  read-only preview of versions,
  counts, migrations and overwrite impact → explicit owner confirmation. Enter
  the export passphrase for preview and discard it; re-enter it at confirmation
  instead of retaining plaintext stage data. Confirm the destination runtime
  passphrase matches the live keyring/boot secret before rewrapping. It is not
  the signed-in account password. Neither passphrase is retained. Confirmation is
  cryptographically bound to the fixed upload, authenticated manifest, preview
  result and exact plan. It is
  blocked during Apply, adoption, RF scan, speed test, diagnostics or capability
  operations. Enforce upload/member/total-size and KDF cost bounds and stream
  encryption/decryption instead of buffering an artifact in memory.
- Confirmation quiesces work and creates a mode-0600 encrypted pre-restore
  safety artifact at
  `<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak`, using the
  confirmed export passphrase. After the applied audit receipt is durably
  cleared, retention targets three recognized fixed-shape safety artifacts,
  fills slots newest-first and prunes the rest. Artifacts referenced by an
  active marker, receipt or suppression record are always preserved, even if
  that temporarily exceeds three. Operators copy artifacts off-host before
  pruning when longer retention is required. A fsynced marker drives a clean
  controlled close/reopen. Startup verifies schema/integrity/key state before
  serving and rolls back the raw pair on failure; open SQLite handles and an
  in-memory data key are never hot-swapped. Success revokes all sessions.
- Restored desired state never silently applies to routers. Persistent
  router-write suppression survives restart and requires an owner, recent
  reauthentication, the active restore ID and exact `RESUME ROUTER WRITES` text
  to clear. Read-only monitoring of restored devices may resume after restart
  using restored credentials while the gate remains active. Clearing the gate
  immediately re-enables automatic 802.11k neighbour maintenance; that
  reconciler may write hostapd RRM neighbour state, but does not start a
  restored desired-configuration Apply.
- Source tests cover live-WAL export, invalid passphrases/members, supported
  migration, forced swap/start failures, rollback, session revocation,
  suppression and zero automatic router writes before explicit resumption.
  The live schema-19 migration/restart and signed-in screen smoke have passed.
  The final release gate requires a fresh-container export/restore/recovery
  rehearsal. The historical live checkpoint did not run a controller or router
  restore.

### 4.1.7 Final v0.1.0 release

This promotes the existing release pipeline; it does not rebuild packaging from
zero. Preserve checksum-verified archives, reproducible builds, public
`linux/amd64` and `linux/arm64` non-root scratch images, health check, SBOM and
provenance. Add a documented release-failing vulnerability policy, keyless OCI
signing/identity verification, and tags `v0.1.0`, `0.1.0`, `0.1` and `latest`
that resolve to the same manifest digest.

The pre-tag gate tests immutable candidate archives and an OCI digest from the
exact release commit: accounts, a controller-only
speed test against a deterministic local adapter, diagnostics ZIP,
fresh-container backup/restore,
`v0.1.0-rc.1` upgrade and documented rollback. Promote those exact bytes and
manifest digest; do not rebuild different release material. After publication,
anonymous downloads/pulls must match the recorded hashes/digest and pass a
minimal clean-install smoke check. Neither gate contacts a router or can install
a capability. Installation, upgrade, rollback, backup/restore, signature, SBOM
and provenance instructions must be complete before tagging.

Archive reproducibility is byte-for-byte: two complete four-platform sets and
their `SHA256SUMS` files are built with one fixed epoch and compared. OCI uses
the complementary exact-source check: the gate creates a multi-platform OCI
layout from the immutable tag without pushing, while publication rebuilds that
same tag with SBOM/provenance and signs the resulting registry digest. OCI
attestation and registry metadata are therefore verified by source identity and
digest, not by pretending their serialized envelopes are byte-identical.

The public binary and image must also prove newly created SQLite database,
`-wal` and `-shm` files are mode 0600, closing the post-`rc.1` sidecar-permission
defect before the final tag.

A public-provider speed-test rehearsal is separate, explicit and opt-in so CI
does not consume unbounded bandwidth or fail on an external service. Only fields
the selected protocol measures are populated; loaded latency/jitter remain null
when unsupported.

### Order and proof

1. SVG navigation and disclosure primitives.
2. Landed account-store foundation, then multi-account API, server-side RBAC,
   and sessions; privileged new APIs do not land before their authorization
   rules.
3. Landed bounded controller logging and stored-only diagnostics ZIP.
4. Dashboard composition and controller-only speed tests.
5. Optional gateway speed-test capability, if a safe official-feed plan exists.
6. Land encrypted portable backup/restore on the online database snapshot
   foundation. **Complete in schema-19 source; the final gate owns isolated
   restore/container proof.**
7. Complete the desktop/accessibility/text-density pass and final release gates.

**Proof:** from a clean release-candidate container, an owner can understand fleet/WAN
health at a glance, run and cancel a clearly sourced controller speed test,
manage a least-privilege account, download a secret-free diagnostics ZIP, and
restore an encrypted backup into a second clean container—without any router
write. Final `v0.1.0` is published only after that proof passes, then its public
bytes/digest pass anonymous verification.

**Out of scope for 4.1:** copying UniFi assets/layouts exactly, the broader
mobile overhaul, DPI/application identity (Phase 5), hidden router installs,
automatic speed tests, firmware modification and controller-authored router
packages.

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
- Per-device UCI snapshots for fleet recovery. Portable controller backup and
  restore moved to Phase 4.1; Phase 6 consumes that primitive rather than
  defining a second format.
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
