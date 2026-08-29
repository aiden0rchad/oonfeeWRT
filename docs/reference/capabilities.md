---
title: Capabilities and limits
description: What oonfeeWRT v0.1.2 can do, what depends on device evidence, and what is unavailable.
---

# Capabilities and limits

This is the user-facing capability boundary for **oonfeeWRT v0.1.2**. It is
intentionally narrower than the long-term roadmap.

## How to read status

| Status | Meaning |
|---|---|
| **Shipped** | Present in v0.1.2 source, UI/API, and automated tests |
| **Hardware-verified** | Exercised on the published physical-router validation setup |
| **Capability-dependent** | Shipped, but visibility or operation depends on the router, driver, package set, and measured source |
| **Source-tested only** | Automated contracts exist, but the published hardware run did not execute the disruptive or optional operation |
| **Unverified** | Intended/shipped path lacks the stated physical topology or hardware proof |
| **Unavailable** | Not provided by v0.1.2 or deliberately outside the project boundary |

An unavailable measurement is not a zero. The UI distinguishes unknown,
unsupported, stale, partial, and observed-empty evidence.

## Deployment and controller

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Standalone Linux/macOS controller | **Shipped** | amd64 and arm64 release archives |
| Linux container/Compose deployment | **Shipped** | amd64/arm64, non-root scratch image, read-only root filesystem in supplied Compose setup |
| Embedded web UI | **Shipped** | One process; no separate web server |
| Local SQLite storage | **Shipped** | WAL mode; database and keyring must be backed up together |
| Light and dark controller themes | **Shipped** | UI-side switch; v0.1.2 defaults to dark and does not persist the choice across reloads |
| Native controller TLS | **Unavailable** | Use a trusted reverse proxy or trusted isolated management LAN |
| Cloud account or relay | **Unavailable** | Self-hosted only |
| Multi-site/NAT traversal | **Unavailable by design** | Use an existing routed management network or VPN |
| Controller running on a router | **Unsupported** | Not built, packaged, tested, or budgeted for OpenWrt-hosted operation |

## Discovery, inspection, and adoption

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Add router by management address | **Shipped, hardware-verified** | Works without layer-2 discovery |
| On-demand discovery | **Shipped** | ARP/mDNS coverage depends on host/container networking |
| Read-only pre-adoption inspection | **Shipped, hardware-verified** | Authenticates to the router but creates no router/controller inventory state |
| Sanitized compatibility-report export | **Shipped** | Versioned hardware/capability allowlist from Inspect; no identity, secrets, network configuration, extra router call, persistence, or upload |
| Explicit Gateway/AP/Switch function selection | **Shipped, hardware-verified** | A device may have multiple functions; function choice controls rendered intent |
| Scoped controller login and ACL | **Shipped, hardware-verified** | Separate default-off consent; one login and one ACL file, no package/executable |
| Capability probe and re-probe | **Shipped, hardware-verified** | Stores measured support/gaps; firmware changes do not have to leave stale capability truth forever |
| SSH-free steady-state management | **Shipped** | Polling and Apply use ubus; SSH remains bounded bootstrap/cleanup/optional-capability transport |
| Automatic package install during adoption | **Unavailable by design** | Optional capabilities use separate plan/consent |

## Fleet visibility and telemetry

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Device inventory, health, model, firmware, uptime | **Shipped, hardware-verified** | Freshness and source gaps remain visible |
| Interface throughput and durable metric history | **Shipped, hardware-verified** | Five-minute rollups for 14 days; hourly for 396 days |
| Management-overhead readout | **Shipped, hardware-verified** | Reports poll interval, request rate/bytes, failures, installed-capability packages, and only measured attributable CPU estimates |
| Per-device poll interval override | **Shipped** | UI offers 60 seconds to 15 minutes. `0` clears the override, and values below the controller default do not increase the effective poll rate |
| OpenWrt log ingestion | **Shipped, hardware-verified** | Once per minute, bounded retention and explicit continuity gaps |
| Live UI updates | **Shipped** | Bounded WebSocket `device.stats`; durable history remains SQLite-backed |
| Application/DPI identification and flow history | **Unavailable** | No controller-authored router agent; DPI is outside the constrained-device budget |

## Dashboard and WAN evidence

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Fleet status and warning/error summary | **Shipped** | Partial data is disclosed rather than flattened |
| WAN reachability charts/table | **Shipped, hardware-verified** | Gateway sends three ICMP probes to fixed `1.1.1.1` at most once per minute; this is not full ISP uptime, HTTP, or DNS validation |
| WAN/interface throughput | **Shipped** | Source and missing-sample provenance are shown |
| Controller-host speed test | **Shipped** | Cloudflare endpoint, about 15 MiB, one active job, 30-second hard bound, three terminal results retained |
| Gateway-run speed test | **Unavailable in v0.1.2** | Would need a separately approved router capability |
| Loaded latency and loaded jitter | **Unavailable** | The controller-host method reports idle latency/jitter only |

## Topology

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Internet → gateway → infrastructure → client graph | **Shipped, hardware-verified** | Inferred from route, FDB/neighbor, association, and optional LLDP evidence |
| Current measured/inferred source labels | **Shipped** | Ambiguous links stay ambiguous; expired evidence can leave an online device unplaced |
| Topology history | **Shipped** | Closed intervals retained for 31 days |
| Baseline topology without extra package | **Shipped** | BusyBox FDB sources may lack VLAN identity |
| LLDP adjacency enrichment | **Shipped optional workflow, hardware-verified** | Exact official-feed package/config/service plan, separate consent, durable rollback ledger |
| Continuous physical truth without LLDP | **Unavailable** | Dynamic FDB evidence can age out |

## Clients and observability

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Client inventory, address/name/vendor hints, association and connection source | **Shipped, hardware-verified** | Depends on host hints, DHCP/neighbor and wireless sources available on each router |
| Current wireless signal/rates/retries | **Capability-dependent, hardware-verified** | Driver/hostapd source gaps are disclosed |
| Client observability timeline | **Shipped, hardware-verified** | Joins bounded exact events, topology intervals, AP/radio/path evidence, and rollups at one cursor |
| Wi-Fi experience score | **Shipped, capability-dependent** | Requires RSSI, retry delta, and TX-failure delta in one sample; missing inputs do not get reweighted |
| Private-MAC indication | **Shipped** | Warning from the locally administered MAC bit, not identity proof |
| Durable per-client application usage | **Unavailable** | Requires optional accounting/DPI not shipped as a v0.1.2 controller capability |

## Radios and RF

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Radio inventory, band, channel, width, power, client count | **Shipped, hardware-verified** | Stable radio identity is separated from driver naming |
| Channel-plan view | **Shipped, hardware-verified** | Shows In use, Enabled, Restricted, or Unknown from actual frequency evidence |
| Channel utilization | **Shipped, hardware-verified** | Derived from deltas of driver `busy_time`/`active_time`, never absolute counters |
| Noise/SNR | **Capability-dependent** | Hidden/unavailable when driver evidence is unstable or incorrectly encoded |
| Interference and airtime split | **Capability-dependent** | Requires trustworthy RX/TX airtime; unavailable on the verified mwlwifi reference |
| Manual RF scan | **Shipped, hardware-verified** | Explicit disruption acknowledgement, 45-second timeout, 4,096 BSS-row bound, newest terminal result retained; one C6 5 GHz validation scan found 14 BSS entries and suggested channel 44 |
| Suggested channels | **Shipped, capability-dependent** | Requires scan no older than 24 hours, fresh radio state, and recent channel plan |
| Continuous spectrum-analyzer waterfall | **Unavailable** | Requires radio/silicon capabilities not exposed as portable OpenWrt evidence |

## Site configuration

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Site-wide WLANs and AP groups | **Shipped, hardware-verified** | Deterministic fan-out to selected APs; write-only secrets remain redacted after save |
| 2.4/5/6 GHz and WPA2/WPA3/OWE/open WLAN fields | **Shipped, capability-dependent** | Includes PMF and 802.11r/k/v fields; hardware defects and missing support can block or warn |
| Networks, VLANs, addressing, and DHCP intent | **Shipped** | Rendered only to devices with the required Gateway/Switch function and capabilities |
| Firewall zones and directed forwarding | **Shipped** | Controller-owned firewall4 sections only |
| IPv4 policy records, static routes, and port forwards | **Shipped** | Preview/gates remain authoritative; application-based matching is unavailable without DPI |
| Client block and fixed-IPv4 desired policy | **Shipped** | Per-client rate limiting, QoS/SQM, and application/DPI policy backends are unavailable in v0.1.2 |
| Per-device overrides | **Shipped** | Limited to WLAN publication, hidden beacon, isolation, and client limit; SSID/key/security/PMF/roaming settings cannot diverge per AP |
| Preview and multi-device Apply | **Shipped, hardware-verified** | Full-fleet preflight, OpenWrt rollback timer, runtime verification, durable receipts |
| Automatic silent reconciliation of every desired edit | **Unavailable by design** | Saving does not Apply; operator review remains required |

## Roaming, mesh, and uplink

The site model and controller include 802.11k neighbour distribution, WLAN
roaming fields, mesh/backhaul records, uplinks, and health surfaces. The
published two-router evidence verifies WLAN fan-out and client reassociation
between the reference APs. Controller-managed 802.11k neighbour distribution is
built and source-tested, not physically proven by that record. The roam also
does not prove that 802.11r Fast Transition was used.

The following release boundaries remain:

- three-or-more-AP fan-out is **unverified**;
- real mesh backhaul is **unverified**;
- wireless uplink is **unverified**; and
- the controller does not claim to replace hardware/driver-specific steering
  behavior with its own router agent.

## Accounts, backup, and diagnostics

| Capability | v0.1.2 status | Important boundary |
|---|---|---|
| Owner, Administrator, Operator, Read-only roles | **Shipped** | Server-enforced; see [Permissions](../concepts/permissions.md) |
| Session inventory/revocation and password step-up | **Shipped** | Sessions are in memory and end on restart |
| Redacted diagnostics ZIP | **Shipped** | Stored evidence only; no router calls; Administrator+ |
| Encrypted `.oowrtbak` export | **Shipped** | Separate unrecoverable export passphrase; Owner + recent reauth |
| Staged restore preview | **Shipped** | Disposable validation/migration before replacement |
| Confirmed restore with safety artifact and write suppression | **Shipped** | No automatic router Apply; all sessions revoked |

## Published hardware evidence

The stable-release documentation carries end-to-end physical evidence for
Linksys WRT3200ACM and TP-Link Archer C6 v2 on OpenWrt 25.12.5. The Archer C6
v2 passed the 60-minute
class-C polling/resource budget: 209 poll batches, no failures, and no observed
overlay write. That fresh-start evidence was produced through the pre-stable/RC
workflow and underlies the stable release.

v0.1.2 additionally carries external, reporter-confirmed read-only inspection
evidence for one Cudy M3000 v2/MT7981 Filogic variant. It proves physical-radio
counting and its direct LAN/WAN layout only; it does not prove adoption, Apply,
tagged VLAN management, operational telemetry, resource budgets, or other
Filogic boards.

That evidence is deliberately specific. It does not prove all ath79, mwlwifi,
MT7621, broader Filogic, DSA, swconfig, mesh, or multi-AP combinations. Review the
[fresh-start validation record](../FRESH-START-VALIDATION.md) for exact evidence
and accepted gaps.

The WRT3200ACM evidence also records a severe board/driver boundary: its Marvell
88W8964/mwlwifi setup can wedge under WPA3/SAE or PMF until a physical cold power
cycle. The safely demonstrated configuration was WPA2-only, PMF disabled, FT
disabled, and 802.11k/v enabled after cold boot. Do not generalize a WLAN option
being present in the model into proof that this router can run it safely.

The current controller serves REST/WebSocket routes under `/api/v1`, but v0.1.2
does not publish a stable third-party API compatibility guarantee. Treat that
surface as the controller/UI contract unless a future release documents one.

## Related reference

- [Requirements](./requirements.md)
- [Safety model](../concepts/safety.md)
- [Feature parity and evidence matrix](../PARITY-MATRIX.md)
- [Troubleshooting](./troubleshooting.md)
- [Roadmap](../ROADMAP.md)
