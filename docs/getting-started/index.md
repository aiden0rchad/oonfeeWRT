# Getting started

oonfeeWRT is a self-hosted controller for stock OpenWrt devices. It gives one browser interface for device health, clients, radios, topology, logs, reviewed network configuration, and safe multi-device changes.

It is **not firmware**. The controller runs on a Linux or macOS computer, NAS, mini-PC, or server. Your routers continue running stock OpenWrt and remain usable through LuCI and SSH.

> **Outcome:** This section takes you from an empty controller to one adopted OpenWrt device without applying an unreviewed network change.

## Choose your path

| If you want… | Start here |
|---|---|
| The shortest supported setup | [Quick start](quick-start.md) |
| A standalone Linux or macOS executable | [Install the binary](../installation/binary.md) |
| A container on a NAS, server, or Docker Desktop | [Install with Docker](../installation/docker.md) |
| HTTPS access through an existing host | [Put a reverse proxy in front](../installation/reverse-proxy.md) |
| To connect the first router | [First adoption](first-adoption.md) |

## What you need

### Controller host

Choose a host that stays on and can reach every router's management address.

- Standalone binaries: `linux/amd64`, `linux/arm64`, `darwin/amd64`, or `darwin/arm64`.
- Container images: `linux/amd64` or `linux/arm64`.
- Practical starting point: a 64-bit host with 1 GB RAM and 2 GB free storage.
- Default browser address in the guides: `http://127.0.0.1:8080`.

Remote routers need an existing routed management network or VPN. oonfeeWRT does not provide cloud brokering or automatic NAT traversal.

### Managed router

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

- **Adoption** installs one scoped `oonfeewrt` rpcd login and `/usr/share/rpcd/acl.d/oonfeewrt.json` after you acknowledge the displayed plan. It installs no package, executable, service, daemon, or firmware.
- **Apply** changes controller-owned network, wireless, DHCP, and firewall UCI sections only after Preview and safety acknowledgements.
- **RF scan** takes the selected serving radio off-channel temporarily and requires disruption acknowledgement.
- **Optional LLDP** may install the official OpenWrt `lldpd` package after separate plan and installation approvals.
- **Un-adoption** reverts controller-owned state and removes the scoped login and ACL after review.

Existing human-managed UCI sections remain foreign and read-only. A conflict blocks Preview or Apply instead of being silently overwritten.

## The safe first-run sequence

1. Install the controller by [binary](../installation/binary.md) or [Docker](../installation/docker.md).
2. Keep the HTTP listener on loopback. Add [reverse-proxy TLS](../installation/reverse-proxy.md) before remote browser access.
3. Create the first owner account.
4. In **Adopt a device**, enter the router address and existing administrator login.
5. Run **Inspect capabilities**. This is a read-only ubus operation.
6. Review and select the device's Gateway, AP, and/or Switch functions.
7. Review and acknowledge the controller access payload, then Adopt.
8. Confirm that the device is online and review unavailable capability sources.
9. Make desired-state changes only when ready. **Preview** first, read every warning, then **Apply**.

## What the interface covers

- **Dashboard:** fleet state, clients, Internet reachability and traffic history, topology summary, warnings, and controller-host speed tests.
- **Topology:** current and historical links with source and confidence information.
- **Radios:** radio inventory, channel plans, utilization evidence, and explicit RF scans.
- **Devices:** health, capabilities, collection overhead, polling, ACL refresh, optional LLDP, and un-adoption.
- **Client Devices:** client inventory, filters, and a time-aligned observability workspace.
- **Policy Engine:** objects, firewall/NAT/route records, whole-zone forwarding, and inspectable desired state.
- **Settings:** networks, DHCP, WLANs, AP groups, roaming, mesh backhauls, wireless uplinks, accounts, diagnostics, and backup/restore.
- **Logs:** General and Audit events with provenance and coverage information.

Unavailable features are capability-gated. For example, a legacy `swconfig` device may provide port observations without supporting managed per-port VLAN changes.

## Current limits to keep in mind

- The stable release's published physical record covers a Linksys WRT3200ACM and TP-Link Archer C6 v2 on OpenWrt 25.12.5. That record came from the pre-stable/RC workflow underlying v0.1.1; the patch release did not rerun the complete procedure. Other targets may work but do not have the same published evidence.
- Only one managed Gateway is supported.
- Docker bridge mode cannot perform layer-2 discovery; add the router by address.
- The controller has no native TLS listener.
- The speed test runs on the controller host through Cloudflare, not on the router. It transfers about 15 MiB and is bounded to 30 seconds.
- Cloud remote access, automatic NAT traversal, native mobile apps, gateway-run speed tests, DPI/application identification, and universal PoE or switch control are not included in v0.1.1.

## Next steps

- [Run the quick start](quick-start.md)
- [Adopt your first device](first-adoption.md)
- [Learn the backup and recovery workflow](../operations/backups.md)
- [Review routine maintenance](../operations/maintenance.md)
