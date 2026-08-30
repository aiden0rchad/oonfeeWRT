---
layout: home
title: oonfeeWRT Documentation
titleTemplate: false

hero:
  name: oonfeeWRT
  text: OpenWrt management without replacing OpenWrt
  tagline: One self-hosted controller for visibility, adoption, safe configuration, radio planning, policy, backups, and operations across stock OpenWrt devices.
  image:
    src: /logo-light.svg
    alt: oonfeeWRT signal mark
  actions:
    - theme: brand
      text: Get started
      link: /getting-started/
    - theme: alt
      text: Explore capabilities
      link: /reference/capabilities
    - theme: alt
      text: View on GitHub
      link: https://github.com/aiden0rchad/oonfeeWRT

features:
  - title: Stock OpenWrt stays in control
    details: The controller runs on your server, NAS, mini-PC, or Mac. Routers keep their existing firmware, LuCI, and human-managed configuration.
  - title: Evidence before assumptions
    details: Every screen separates measured values, unavailable sources, stale data, and unsupported capabilities instead of filling gaps with guesses.
  - title: Preview, rollback, confirm
    details: Configuration is reviewed first, staged through UCI, protected by OpenWrt's rollback timer, and confirmed only after the controller reads the expected state.
  - title: Useful fleet visibility
    details: See Internet health and throughput when one usable main-table WAN is proved and its exact runtime device has RX/TX history, including PPPoE, plus clients, device telemetry, topology history, radios, events, management overhead, and controller-host speed tests.
  - title: Local security boundaries
    details: Local accounts and roles, scoped router access, encrypted stored secrets, redacted diagnostics, and no cloud broker or controller-authored router package.
  - title: Recovery designed in
    details: Export encrypted portable backups, validate them in staging, restore through a controlled restart, and keep router writes suppressed until an owner reviews the result.
---

<p class="doc-kicker">Documentation for v0.1.3</p>

## One control plane, explicit boundaries

oonfeeWRT is a self-hosted controller for small OpenWrt networks. It gives a
single, UniFi-inspired interface to devices that still run stock OpenWrt. The
controller observes the fleet, keeps desired configuration, and makes only the
changes you review and approve.

It is **not firmware**. Nothing is installed on a router when you start the
controller, discover a device, or add an address. Adoption can create one
scoped `oonfeewrt` login and one reviewable rpcd ACL after consent. The only
optional package workflow in v0.1.3 is LLDP, with a separate plan and rollback.

<div class="status-strip">
  <span class="status-pill">OpenWrt 21.02+</span>
  <span class="status-pill">Linux and macOS hosts</span>
  <span class="status-pill">Docker optional</span>
  <span class="status-pill">Local accounts</span>
  <span class="status-pill">SQLite persistence</span>
  <span class="status-pill">No cloud broker</span>
</div>

## Choose your path

<div class="path-grid">
  <a class="path-card" href="./getting-started/">
    <h3>I am evaluating oonfeeWRT</h3>
    <p>Understand the fit, requirements, hardware evidence, security model, and current limitations before installing anything.</p>
  </a>
  <a class="path-card" href="./getting-started/quick-start">
    <h3>I want to install it</h3>
    <p>Choose a standalone binary or Docker Compose, preserve the data directory, create the first owner, and verify the controller.</p>
  </a>
  <a class="path-card" href="./getting-started/first-adoption">
    <h3>I am ready to add a router</h3>
    <p>Inspect capabilities first, review the scoped access payload, adopt with the minimum functions, then verify the capability report.</p>
  </a>
  <a class="path-card" href="./concepts/safety">
    <h3>I need to understand router writes</h3>
    <p>See which actions are read-only, which require acknowledgement, how ownership works, and what happens during rollback or recovery.</p>
  </a>
</div>

## Capabilities at a glance

<div class="capability-grid">
  <div class="capability-card">
    <h3>Fleet and Internet health</h3>
    <p>A uniquely proved, usable lowest-metric main-table IPv4 default route, exact-match runtime counters (including PPPoE), ICMP reachability, six-hour trends, recent warnings, topology summary, and bounded Cloudflare speed tests from the controller host.</p>
  </div>
  <div class="capability-card">
    <h3>Discovery and adoption</h3>
    <p>On-demand IPv4 discovery, add-by-address, read-only pre-adoption inspection, independently selected Gateway/AP/Switch functions, pinned device identity, and a local sanitized compatibility-report download.</p>
  </div>
  <div class="capability-card">
    <h3>Devices and clients</h3>
    <p>Firmware, load, memory, throughput, radio series, management overhead, adjustable polling, client presence and attribution, and a joined observability workspace.</p>
  </div>
  <div class="capability-card">
    <h3>Topology and RF</h3>
    <p>Current and historical topology with evidence confidence, VLAN and medium filters, radio inventory, channel plans, utilization, interference, and acknowledged RF scans.</p>
  </div>
  <div class="capability-card">
    <h3>Site configuration</h3>
    <p>Networks, VLANs, IPv4 CIDRs, DHCP, firewall zones, WLANs, AP groups, 802.11k/v/r, mesh backhauls, wireless uplinks, and bounded per-device overrides.</p>
  </div>
  <div class="capability-card">
    <h3>Policy Engine</h3>
    <p>Zone matrix, explicit firewall rules, port forwards, static routes, fixed client addresses, client groups, and visible compiled drafts before they enter desired state.</p>
  </div>
  <div class="capability-card">
    <h3>Safe operations</h3>
    <p>Redacted preview, fleet preflight, acknowledged Apply, Gateway-last ordering, OpenWrt rollback, durable operation status, ownership records, and careful un-adoption.</p>
  </div>
  <div class="capability-card">
    <h3>Administration and recovery</h3>
    <p>Owner, administrator, operator, and read-only roles; session control; audit history; stored-only redacted diagnostics; encrypted portable backups; compatibility preview; and staged restore.</p>
  </div>
</div>

[See the complete capability and support matrix →](/reference/capabilities)

## See the controller

<figure class="docs-screenshot">
  <img src="./images/dashboard-overview.jpg" alt="oonfeeWRT dashboard showing Internet health, speed tests, fleet status, topology, and recent events" loading="lazy">
  <figcaption>The live dashboard keeps health, provenance, trends, and gaps together instead of reducing the network to a single status color.</figcaption>
</figure>

<figure class="docs-screenshot">
  <img src="./images/radios-channel-plan.jpg" alt="oonfeeWRT radio inventory and channel planning screen" loading="lazy">
  <figcaption>Radio inventory and evidence-aware channel planning. Disruptive scans stay explicit and acknowledged.</figcaption>
</figure>

## What runs where

| Component | Location | Responsibility |
|---|---|---|
| `oonfeewrtd` | Your 64-bit Linux or macOS host | UI, API, collection, desired state, encrypted secrets, SQLite history |
| Web browser | A trusted management client | Local sign-in, review, configuration, and operations |
| Stock OpenWrt | Each managed router or AP | Networking, wireless, firewall, DHCP, ubus/rpcd, rollback |
| Optional reverse proxy | Usually the controller host | Trusted TLS and secure remote access over an existing routed network or VPN |

The controller does not provide cloud access, NAT traversal, firmware, or a
router-hosted agent. Remote sites need an existing management route or VPN.

## A workflow designed for safe changes

1. **Observe.** Discover or add a device and read what the controller can
   establish without making a router change.
2. **Adopt deliberately.** Select the device functions and approve the scoped
   access payload. The administrator credential is used for that action and is
   not stored.
3. **Describe intent.** Define site networks, WLANs, policy, or overrides in the
   controller. Saving desired state does not silently Apply it.
4. **Preview.** Review per-device changes, omissions, conflicts, source gaps,
   and acknowledgements.
5. **Apply with rollback.** The controller stages owned UCI changes, uses the
   OpenWrt rollback window, reconnects, and confirms only after reading the
   expected state.
6. **Keep evidence.** Events, audit history, rollups, and durable operation
   receipts explain what happened later.

## Current boundaries

oonfeeWRT v0.1.3 deliberately does not claim capabilities it cannot prove.

- One managed Gateway; no controller high availability.
- No native TLS, SSO, cloud broker, mobile app, DPI, application identity,
  PoE control, switch ACL management, or gateway-run speed test.
- IPv4 on-demand discovery probes eligible local subnets no wider than `/22`
  for an unauthenticated `/ubus` endpoint. A bridged container may not see the
  LAN subnets to scan, so add devices by address.
- WAN observation covers one usable, uniquely lowest-metric main-table IPv4
  default route. Equal-metric distinct defaults and ECMP are unavailable;
  custom policy routing, `mwan3`, and manual WAN selection are not modeled.
- Hardware and driver support varies. End-to-end evidence covers a Linksys
  WRT3200ACM and TP-Link Archer C6 v2 on OpenWrt 25.12.5. Read-only inspection
  is additionally reporter-confirmed on one Cudy M3000 v2/MT7981 Filogic
  variant; adoption, Apply, VLANs, polling budgets, and other Filogic boards
  remain unverified. Several mesh and wireless-uplink scenarios also remain
  unverified.
- The project has not received an independent security audit or penetration
  test. Begin with non-critical hardware and keep tested recovery material.

[Review requirements and compatibility →](/reference/requirements)

## Start with one device

The shortest safe path is to install the controller on a host that can reach
your router, create the first owner, add one non-critical OpenWrt device by
address, inspect it, and adopt only the functions you need.

[Start the guided setup →](/getting-started/)
