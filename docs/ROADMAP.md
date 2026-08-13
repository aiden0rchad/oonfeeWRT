# oonfeeWRT — Roadmap

Six phases. Each has a **proof** — the one thing that must work before the phase
counts as done. Ship in order; the ordering is chosen so that each phase is
independently useful and each de-risks the next.

---

## Phase 0 — Transport & safety (the foundation)

Nothing user-visible. Build this first anyway.

- Go controller skeleton, SQLite schema, embedded SPA shell.
- ubus JSON-RPC client: `session.login`, token refresh on expiry, retry/backoff,
  TLS with TOFU cert pinning.
- The ACL file (`/usr/share/rpcd/acl.d/oonfeewrt.json`): dedicated user, scoped
  object list, explicit `file.exec` allow-list. **Written via `file.write` — there
  is no OpenWrt package to build, ever.**
- Capability probe + capability registry (ARCHITECTURE §6.1).
- Adoption: discover → credential → probe → create user → write ACL → pin cert →
  discard the original credential.
- **Un-adoption**, in the same sprint: remove user + ACL, optionally revert every
  UCI section we own. A wrapper that can't cleanly remove itself doesn't get
  trusted.
- **The apply cycle**: batched set → commit → `apply {rollback, timeout}` →
  health probe → poll `confirm`, with a full audit record.
- Ownership tagging + the "what will change on this device" diff preview.

**Proof:** two things, both required. (1) Deliberately push a config that breaks
the device's uplink — the device must come back on its own within the timeout and
the controller must report the failure honestly. (2) Adopt a device, make changes,
un-adopt it, and diff its config against a pre-adoption snapshot — the only
residue should be nothing. *Do not proceed until both work.*

---

## Phase 1 — Read-only fleet view

The first thing anyone sees.

- Inventory: adopt N devices, show model/firmware/uptime/IP.
- Telemetry loops (live/standard/slow) + TSDB rollup ladder.
- **Screens:** Dashboard (WAN throughput, latency probes, uptime, device counts),
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

---

## Phase 3 — Networks, zones, policy

- Networks/VLANs with DHCP, DNS, IPv6, bridge VLAN filtering.
- Zone model → firewall4 zones + forwardings; the zone-to-zone matrix UI.
- Firewall rules, port forwards, traffic routes, nftables sets for client groups.
- Client actions wired to real enforcement: block, rate limit, fixed IP.
- **Screens:** Settings → Networks, Policy Engine, per-client policy.

**Proof:** create a guest VLAN with client isolation and no LAN access, in the
UI, in under a minute, and have it verifiably enforced.

---

## Phase 4 — Insights, topology, logs

The screens that make people *enjoy* the tool.

- LLDP + fdb + ARP + assoc → topology graph.
- Survey/station-dump derived metrics: interference, airtime, TX retries.
- The Experience score, with its components exposed on hover.
- Channel Plan + suggested-channel scoring.
- nflog/syslog ingest → event store with enrichment (identity, zone, GeoIP) and
  the detail slide-over.
- **Screens:** Topology, Insights → Radios, Logs (General + Audit).

**Proof:** the Radios screen tells you why a client is having a bad time —
channel overlap, high retries, weak RSSI — without you touching a terminal.

---

## Phase 5 — Flows & security

The expensive phase. Deliberately last.

- `netifyd`/nDPI on the gateway → flow records with application identification.
- Flow store with aggressive retention + summary rollups.
- Risk heuristic (blocklists + geo + ports + optional Suricata verdict),
  documented and non-magical.
- **Screens:** Flows, Flows on Map, Top Apps/Destinations, Security settings.

**Proof:** "which device is talking to a host in a country I don't do business
with" is answerable in three clicks.

---

## Phase 6 — Fleet operations

- Backup/restore of controller config and per-device UCI snapshots.
- Alerts + notifications (webhook/ntfy/email), SIEM export.
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
