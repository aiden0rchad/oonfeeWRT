# Getting started

oonfeeWRT is a self-hosted controller for stock OpenWrt devices. It gives one browser interface for device health, clients, radios, topology, logs, reviewed network configuration, and safe multi-device changes.

It is **not firmware**. The controller runs on a Linux or macOS computer, NAS, mini-PC, or server. Your routers continue running stock OpenWrt and remain usable through LuCI and SSH.

> **Outcome:** This section takes you from an empty controller to one adopted OpenWrt device without applying an unreviewed network change.

## Choose your path

| If you want… | Start here |
|---|---|
| A populated read-only preview without connecting a router | [Isolated demo](../guide/demo.md) |
| The shortest supported setup | [Quick start](quick-start.md) |
| A standalone Linux or macOS executable | [Install the binary](../installation/binary.md) |
| A container on a NAS, server, or Docker Desktop | [Install with Docker](../installation/docker.md) |
| HTTPS access through an existing host | [Put a reverse proxy in front](../installation/reverse-proxy.md) |
| To connect the first router | [First adoption](first-adoption.md) |

::: info v0.1.6 and schema 25
Installation examples target **v0.1.6**, schema **25**, including Statistics,
Reports, Alerts, editable topology, Firmware catalogue checks, Integrations,
Accounts, and mobile improvements. Preserve a matching pre-upgrade recovery
unit before opening existing controller data; returning to v0.1.5 requires
its schema-23 database, keyring, runtime passphrase, and binary/image.
The isolated demo has no database or router connection and performs no migration.
:::

## What you need

### Controller host

Choose a host that stays on and can reach every router's management address.

- Standalone binaries: `linux/amd64`, `linux/arm64`, `darwin/amd64`, or `darwin/arm64`.
- Container images: `linux/amd64` or `linux/arm64`.
- Practical starting point: a 64-bit host with 1 GB RAM and 2 GB free storage.
- Default browser address in the guides: `http://127.0.0.1:8080`.

Remote routers need an existing routed management network or VPN. oonfeeWRT does not provide cloud brokering or automatic NAT traversal.

### OpenWrt router

The documented minimum is OpenWrt 21.02 or newer with:

- SSH;
- `rpcd`;
- `uhttpd` with its ubus handler available at `/ubus`;
- network reachability from the controller host.

Radio, switch, topology, and policy features depend on the router, driver, installed rpcd modules, and OpenWrt release. oonfeeWRT records those differences instead of treating missing evidence as zero.

For your first run, use a non-critical router that you can reach physically. Back up the router before testing configuration changes.

## Understand the two credentials

oonfeeWRT uses two unrelated kinds of secret:

1. The **controller runtime passphrase** unlocks `keyring.json` when the daemon starts. It protects saved router credentials and wireless keys. It is not a browser account password.
2. A **controller account password** signs a person into the web interface. The first account is an owner.

For unattended startup, the runtime passphrase is stored in a private mode-`0600` file. Back up that file separately from, but together with, the controller database and `keyring.json`. Losing the runtime passphrase or the matching keyring cannot be repaired from the database alone.

## Know when a router changes

On a new controller, starting it, opening the dashboard, scanning the LAN, adding an address, inspecting a device, generating diagnostics, exporting a controller backup, and running the controller-host speed test do **not** change a router. On later starts, adopted devices resume read-only polling; if managed WLANs request 802.11k neighbour reports and router writes are not suppressed, the automatic reconciler may also update runtime hostapd neighbour lists.

Router-changing actions are explicit:

- **Adoption** installs one scoped `oonfeewrt` rpcd login and `/usr/share/rpcd/acl.d/oonfeewrt.json` after you acknowledge the displayed plan. Managed devices use the managed ACL group; Monitor only uses the distinct read-only `oonfeewrt-monitor` group. Both modes require this bootstrap. It installs no package, executable, service, daemon, or firmware.
- **Apply** changes reviewed controller-owned network, wireless, DHCP, and firewall UCI sections only after Preview and safety acknowledgements. An explicit non-Router-managed IPv6 mode on the Gateway management LAN is the one narrower exception: it may patch allowlisted IPv6 options on exact existing LAN/DHCP and supported conventional WAN sections.
- **RF scan** takes the selected serving radio off-channel temporarily and requires disruption acknowledgement.
- **Optional LLDP** may install the official OpenWrt `lldpd` package after separate plan and installation approvals.
- **Un-adoption** reverts controller-owned state and removes the scoped login and ACL after review.

Choose **Monitor only** when the router should contribute polling, inventory,
telemetry, events, and topology but remain outside desired configuration.
Monitor-only devices cannot enter Preview or Apply, install/configure/remove
the optional LLDP capability, receive wireless-neighbor mutations, or run other
package/config/remove operations. Existing LLDP observation, explicit ACL
maintenance, and un-adoption remain available. Multiple monitor-only routers
can sit across routed subnets; the controller host must already reach their SSH
and HTTP or HTTPS `/ubus` endpoints.

A supported RF scan remains available on Monitor only after its own disruption
acknowledgement. It is an active, transient observation: the serving radio goes
off-channel and clients may pause, roam, or disconnect, but no persistent
configuration change is intended.

Existing human-managed UCI sections remain foreign and are normally read-only.
The management-LAN IPv6 exception never creates, claims, renames, or deletes a
foreign section; missing, ambiguous, wrong-type, or unsafe static-IPv6 targets
block Preview. Other conflicts block Preview or Apply instead of being silently
overwritten.

## The safe first-run sequence

1. Install the controller by [binary](../installation/binary.md) or [Docker](../installation/docker.md).
2. Keep the HTTP listener on loopback. Add [reverse-proxy TLS](../installation/reverse-proxy.md) before remote browser access.
3. Create the first owner account.
4. In **Adopt a device**, enter the router address and existing administrator login.
5. Run **Inspect capabilities**. This is a read-only ubus operation.
6. Optionally download the sanitized compatibility report if you need to share
   bounded hardware-support evidence.
7. Choose **Managed** or **Monitor only**, then review the device's Gateway,
   AP, and/or Switch functions. A site permits one managed Gateway.
8. Review and acknowledge the controller access payload, then Adopt. Monitor
   only installs the scoped identity with the distinct read-only
   `oonfeewrt-monitor` ACL.
9. Confirm that the device is online and review unavailable capability sources.
10. Make desired-state changes only when ready. **Preview** first, read every warning, then **Apply**.

## What the interface covers

- **Dashboard:** fleet state, clients, Internet reachability and traffic history, topology summary, warnings, and controller-host speed tests.
- **Statistics:** 6-hour through 30-day stored WAN,
  system, interface, and available stable-radio rollups, with exact series
  provenance and visible missing-bucket coverage.
- **Reports:** comparable periods, sample-weighted WAN summaries, coverage,
  and CSV export; not billing-grade usage or a complete uptime guarantee.
- **Alerts:** sustained device-offline and WAN latency/loss conditions, durable
  incidents, and optional explicitly enabled HTTPS webhook delivery.
- **Topology:** current and historical links with source and confidence information. v0.1.4 uses fresh multi-hop FDB/LLDP evidence to avoid presenting one managed device as directly attached to several upstream devices; raw intervals remain available in history.
- **Radios:** radio inventory, channel plans, utilization evidence, and explicit RF scans.
- **Devices:** management mode, health, capabilities, collection overhead, polling, ACL refresh, mode-appropriate optional actions, and un-adoption. Older adoptions need a separately reviewed ACL refresh only if you want router-clock status; ordinary management continues without re-adoption.
- **Client Devices:** client inventory, filters, and a time-aligned observability workspace.
- **Policy Engine:** objects, named exact-MAC policy sets, firewall/NAT/route records, whole-zone forwarding, concrete set resolution, and inspectable desired state.
- **Firmware:** stored identity and manual official same-branch catalogue checks;
  no download, staging, flashing, or automatic helper installation.
- **Integrations:** explicitly requested AdGuard Home aggregate checks and
  optional-helper WireGuard peer/counter reads, without service configuration.
- **Accounts:** your password and sessions; Owners can also administer accounts.
- **Settings:** networks, DHCP, WLANs, AP groups, roaming, mesh backhauls, wireless uplinks, diagnostics, and backup/restore.
- **Logs:** General and Audit events with provenance and coverage information, an active IPv6 no-default-route condition, and fresh router-clock skew warnings.

Unavailable features are capability-gated. For example, a legacy `swconfig` device may provide port observations without supporting managed per-port VLAN changes.

## Current limits to keep in mind

- End-to-end physical evidence covers a Linksys WRT3200ACM and TP-Link Archer
  C6 v2 on OpenWrt 25.12.5. v0.1.3 also has reporter-confirmed read-only Inspect
  evidence for one Cudy M3000 v2/Filogic variant, but not adoption, Apply, VLAN,
  WLAN/client operation, resource budgets, topology, RF scans, speed tests,
  un-adoption, or broader Filogic validation.
- Only one managed Gateway is supported. Multiple monitor-only routers are
  allowed when already reachable, but are not managed failover gateways,
  configuration targets, or a replacement for multi-site routing/VPNs.
- Internet-uplink evidence models one effective main-table IPv4 default route.
  Equal-metric distinct defaults, ECMP/multipath, custom policy routing,
  `mwan3`, manual WAN selection, and bond-member monitoring are not modeled.
  Collection runs on the slower network/topology cycle, not as a rapid
  failover monitor.
- The discovery scan probes TCP `/ubus` on eligible interface subnets; it does
  not use ARP or mDNS. A Docker bridge usually requires add-by-address.
- The controller has no native TLS listener.
- Existing networks upgrade to **Router managed** IPv6 and receive no IPv6
  router write merely from installing v0.1.6. Prefix delegation and Disabled
  remain explicit Preview-and-Apply choices.
- The speed test runs on the controller host through Cloudflare, not on the router. It transfers about 15 MiB and is bounded to 30 seconds.
- Cloud remote access, automatic NAT traversal, native mobile apps, firmware execution, gateway-run speed tests, DPI/application identification, and universal PoE or switch control are not included in v0.1.6. Flow visibility is documented as a gated feasibility track only.

## Next steps

- [Run the quick start](quick-start.md)
- [Adopt your first device](first-adoption.md)
- [Learn the backup and recovery workflow](../operations/backups.md)
- [Review routine maintenance](../operations/maintenance.md)
