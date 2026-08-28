---
title: Requirements and compatibility
description: Controller host, network, OpenWrt, storage, and security requirements for oonfeeWRT v0.1.1.
---

# Requirements and compatibility

Use this checklist before installing or adopting a router with **oonfeeWRT
v0.1.1**.

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
| Browser → controller | TCP 8080 by default | Bare daemon defaults to `:8080` on all interfaces; supplied Compose publishes host loopback only |
| Controller → router | TCP 22 by default | Explicitly approved SSH bootstrap, cleanup, ACL refresh, or optional capability work |
| Controller → router | Router `uhttpd` HTTP/HTTPS port | `/ubus` polling and configuration |
| Controller host → Internet | HTTPS, when used | Operator release/image downloads and the explicitly run Cloudflare speed test |
| Router → package feed | Feed HTTP/HTTPS port, when used | Package-index/install traffic only during an explicitly approved optional-capability workflow |

Router endpoints may use a non-default port when included in the management
address. Firewall rules must permit the actual configured endpoint.

## Container networking

The supplied Compose setup uses bridge networking and publishes
`127.0.0.1:8080:8080`. In bridge mode:

- add-by-address, adoption, polling, and Apply work over normal routed L3;
- subnet TCP discovery can work where routing permits; and
- ARP-table and mDNS discovery do not cross the container bridge.

Linux host networking is an explicit opt-in for full local layer-2 discovery.
Docker Desktop does not provide equivalent true host networking for this use;
add routers by address. Discovery is a convenience, never an adoption
requirement.

## Browser-to-controller security

v0.1.1 has no native TLS listener. Choose one of these deployments:

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

For v0.1.1:

- download release archives and `SHA256SUMS` from the v0.1.1 GitHub release;
- reject any checksum mismatch;
- note that macOS binaries are not Developer ID signed or notarized; and
- verify the OCI image's keyless signature before first use where `cosign` is
  available.

The immutable image is `ghcr.io/aiden0rchad/oonfeewrt:v0.1.1`. Stable aliases
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

both on OpenWrt 25.12.5. Three-or-more-AP fan-out, real mesh backhaul, wireless
uplink, MT7621, and MT7981/Filogic remain unverified for the release. The Archer
C6 v2 also passed the full 60-minute class-C polling/resource budget harness.
The record was produced through the pre-stable/RC workflow that underlies
v0.1.1; the patch release did not rerun the complete hardware procedure, and
its router-operation code was unchanged.

This is evidence, not an allow-list. Another OpenWrt device may work, partially
work, or expose driver-specific gaps. Adopt one non-critical device first and
read its capability report.

## Pre-adoption checklist

- [ ] Controller runs `v0.1.1` (`oonfeewrtd -version`).
- [ ] Data directory and matching passphrase backup are protected.
- [ ] Controller healthcheck passes.
- [ ] Browser access is loopback-only, trusted-LAN-only, or behind trusted TLS.
- [ ] Controller host reaches router SSH and `/ubus` endpoints.
- [ ] Router runs supported OpenWrt with `rpcd` and the `uhttpd` ubus handler.
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
