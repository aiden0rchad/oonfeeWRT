---
title: Requirements and compatibility
description: Controller host, network, OpenWrt, storage, and security requirements for oonfeeWRT v0.1.3.
---

# Requirements and compatibility

Use this checklist before installing or adopting a router with **oonfeeWRT
v0.1.3**.

## Controller host

The controller runs separately from managed routers.

| Installation | Supported release platforms |
|---|---|
| Standalone binary | `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` |
| Container image | `linux/amd64`, `linux/arm64` |

Running the controller on an OpenWrt router is not supported or packaged. Use an
always-on 64-bit computer, NAS, mini-PC, SBC, server, or Mac.

Practical starting capacity:

- 1 GiB RAM;
- 2 GiB free storage; and
- a persistent local filesystem for the data directory.

The engineering envelope is at most 256 MiB steady-state RSS at 25 devices, 2%
of one modern CPU core for an idle fleet, and 2 GiB disk at full 13-month metric
retention. Twenty-five devices is a sizing/evidence target, not a hard adoption
cap. These are release targets, not a promise that every workload or diagnostic
export stays below them.

## Managed OpenWrt devices

The documented minimum is OpenWrt **21.02 or newer** with:

- SSH reachable from the controller host for approved bootstrap and cleanup;
- `rpcd`;
- `uhttpd`; and
- the `uhttpd` ubus handler at `/ubus`.

OpenWrt 24.10 and the 25.12 series are the primary current assumptions. Actual
support is capability-driven because rpcd modules, switch architectures,
wireless drivers, and board packaging differ.

For adoption, prepare:

- the router's management IP address or resolvable name;
- an existing device-administrator login, normally `root`;
- a non-empty router administrator password; and
- independent LuCI or SSH access for recovery.

A factory-default passwordless root account is unsafe. oonfeeWRT warns rather
than blocking an explicitly trusted lab device, but production adoption should
start only after setting a password.

### Capability-dependent OpenWrt components

Base rpcd and netifd provide core authentication, UCI, system, interface, and
network state. Richer views may depend on existing official OpenWrt components
such as `rpcd-mod-luci` or `rpcd-mod-iwinfo`, and on hostapd/driver methods.

Missing components are reported as unavailable or unsupported. Adoption does
not silently install them. The only shipped optional-package workflow is LLDP,
which separately plans and may install official-feed `lldpd` packages after
explicit approval. See [Capabilities](./capabilities.md).

### Effective-WAN observation prerequisites

v0.1.3's WAN selection needs two read-only facts from a gateway in the same
slow topology poll:

- successful `network.interface dump` through ubus; and
- successful scoped `file.exec` of `/sbin/ip -4 route show table all`.

The shipped ACL has allowed that exact route command since v0.1.0, so upgrading
an already adopted v0.1.2 device requires no ACL refresh, re-adoption, package,
or device-administrator credential. The router must provide the stock `ip`
command and expose an ordinary installed main-table IPv4 default whose kernel
device maps to exactly one active netifd interface that also reports a default
route.

Ordinary single DHCP, static, and PPPoE uplinks satisfy the modeled shape.
Equal-metric distinct defaults, ECMP/multipath, custom policy routing,
`mwan3`, unmappable runtime devices, and bond-member selection remain
unavailable rather than guessed. Those layouts can still be managed outside
oonfeeWRT, but v0.1.3 does not claim their Dashboard WAN path is authoritative.

## Network reachability

The controller host must be able to reach every router's management address.
Normal operation is outbound L3 communication from controller to router; remote
sites therefore need an existing routed management path or VPN.

oonfeeWRT does not provide:

- a cloud relay;
- automatic NAT traversal;
- multi-site dial-out agents; or
- its own VPN.

Use an existing WireGuard or other routed management network for remote sites.

Common ports are:

| Direction | Port/service | Purpose |
|---|---|---|
| Browser → controller | TCP 8080 by default | Bare daemon defaults to `:8080` on all interfaces; supplied Compose publishes host loopback unless `OONFEE_HTTP_BIND` explicitly selects a management IP or wildcard |
| Controller → router | TCP 22 by default | Explicitly approved SSH bootstrap, cleanup, ACL refresh, or optional capability work |
| Controller → router | Router `uhttpd` HTTP/HTTPS port | `/ubus` polling and configuration |
| Controller host → Internet | HTTPS, when used | Operator release/image downloads and the explicitly run Cloudflare speed test |
| Router → package feed | Feed HTTP/HTTPS port, when used | Package-index/install traffic only during an explicitly approved optional-capability workflow |

Router endpoints may use a non-default port when included in the management
address. Firewall rules must permit the actual configured endpoint.

## Container networking

The supplied Compose setup uses bridge networking and publishes port 8080 on
host loopback by default. `OONFEE_HTTP_BIND` can explicitly select one host
management IP or `0.0.0.0`; it does not change the container's internal
`OONFEE_LISTEN=:8080`. In bridge mode:

- add-by-address, adoption, polling, and Apply work over normal routed L3;
- an explicitly requested, bounded IPv4 subnet scan can work where routing
  permits; and
- automatic discovery planning sees the container namespace's eligible
  directly attached IPv4 networks, not the host's LAN interfaces.

Discovery is not ARP- or mDNS-based in any mode. It opens bounded TCP/HTTP
probes and fingerprints stock rpcd with an unauthenticated `/ubus` object-list
request. Linux host networking is an explicit opt-in that lets the planner see
eligible host LAN interfaces; it does not turn discovery into a layer-2
protocol. Docker Desktop does not provide the equivalent interface view for
this use. Add routers by address when the planned networks omit the router's
subnet. Discovery is a convenience, never an adoption requirement.

## Browser-to-controller security

The controller has no native TLS listener. Choose one of these deployments:

1. bind to `127.0.0.1:8080` and use it only from the host;
2. keep the controller on loopback and publish it through a trusted TLS reverse
   proxy; or
3. deliberately bind it to an isolated, trusted management LAN.

Do not expose port 8080 directly to the Internet. A reverse proxy must preserve
WebSocket upgrades and `X-Forwarded-Proto: https` so session cookies are marked
`Secure`. The [installation guide](../INSTALL.md#put-tls-in-front) includes a
minimal Caddy configuration.

## Persistent data and secrets

The data directory must:

- use an absolute path;
- be writable by the controller's operating-system user;
- persist across restarts and container replacement; and
- remain private (`0700` is recommended).

The controller creates `oonfeewrt.db`, `keyring.json`, and private working
directories there. Preserve the database and matching keyring together. The
runtime passphrase must also be recoverable from separate protected storage.

For unattended startup, point `-passphrase-file` or
`OONFEE_PASSPHRASE_FILE` at a regular file that is not group- or world-readable
(`0600` is the documented mode). `OONFEE_PASSPHRASE` is intentionally rejected;
do not put the passphrase value in the environment, Compose file, command line,
or process arguments.

See [Data and retention](../concepts/data-retention.md) before designing backup
or volume snapshots.

## Installation artifacts

For v0.1.3:

- download release archives and `SHA256SUMS` from the v0.1.3 GitHub release;
- reject any checksum mismatch;
- note that macOS binaries are not Developer ID signed or notarized; and
- verify the OCI image's keyless signature before first use where `cosign` is
  available.

The immutable image is `ghcr.io/aiden0rchad/oonfeewrt:v0.1.3`. Stable aliases
exist, but deployments should pin the exact version or digest.

## Source-build requirements

Building is not required to run a release. To build the repository:

- Go toolchain **1.26.6** (declared by `go.mod`);
- Node.js **22**;
- npm; and
- a POSIX shell for repository scripts.

Container image builds additionally require Docker Buildx. Browser tests need
the pinned Playwright Chromium installed by `npm --prefix ui run
test:browser:install`.

See [Engineering reference](./engineering.md).

## Verified hardware boundary

The stable release's published hardware record covers:

- Linksys WRT3200ACM; and
- TP-Link Archer C6 v2;

both on OpenWrt 25.12.5. The Archer C6 v2 also passed the full 60-minute class-C
polling/resource budget harness. v0.1.3 adds external, reporter-confirmed
read-only inspection evidence for a Cudy M3000 v2 with Motorcomm YT8821 on
OpenWrt 25.12.5 (`mediatek/filogic`): two physical radios, direct LAN `eth1`,
WAN `eth0`, and no independent switch ports.

That Cudy evidence does not cover adoption/bootstrap, Apply/rollback, WLAN and
client operation, tagged VLAN management, polling/resource budgets, topology,
RF scans, speed tests, un-adoption, or other Filogic boards. Three-or-more-AP
fan-out, real mesh backhaul, wireless uplink, and MT7621 also remain unverified.

v0.1.3's PPPoE/default-route correction has separate evidence: issue #20
provided real route output, and automated regression tests cover the
DrayTek-management-plus-PPPoE shape, lower metrics, equal-metric ambiguity,
direct-interface fallback, composite failure, and rolling API/UI compatibility.
That does not add a third end-to-end hardware-validation target.

This is evidence, not an allow-list. Another OpenWrt device may work, partially
work, or expose driver-specific gaps. Adopt one non-critical device first and
read its capability report.

## Pre-adoption checklist

- [ ] Controller runs `v0.1.3` (`oonfeewrtd -version`).
- [ ] Data directory and matching passphrase backup are protected.
- [ ] Controller healthcheck passes.
- [ ] Browser access is loopback-only, trusted-LAN-only, or behind trusted TLS.
- [ ] Controller host reaches router SSH and `/ubus` endpoints.
- [ ] Router runs supported OpenWrt with `rpcd` and the `uhttpd` ubus handler.
- [ ] A gateway provides the stock `/sbin/ip`; the standard adoption payload
      will grant its exact read-only route command to the scoped login.
- [ ] Router administrator password is set.
- [ ] OpenWrt configuration backup exists.
- [ ] Independent LuCI/SSH recovery path is available.
- [ ] The first target is non-critical.
- [ ] You understand the access payload and will review capability gaps before
      Apply.

## Verify the controller host

```sh
oonfeewrtd -version
oonfeewrtd \
  -listen 127.0.0.1:8080 \
  -healthcheck
```

The healthcheck must exit successfully and the running controller's `/healthz`
body must be exactly `ok`. Then sign in, open **Devices**, and use read-only
Inspect before authorizing adoption.

For failures, use [Troubleshooting](./troubleshooting.md).
