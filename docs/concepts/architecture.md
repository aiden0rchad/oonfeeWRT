---
title: Architecture
description: How oonfeeWRT is divided, where it runs, and how controller intent becomes safe OpenWrt configuration.
---

# Architecture

This page describes the architecture shipped in **oonfeeWRT v0.1.3**. For the
implementation record and historical design decisions, see
[`ARCHITECTURE.md`](../ARCHITECTURE.md) and
[`IMPLEMENTATION.md`](../IMPLEMENTATION.md).

## The short version

oonfeeWRT is a self-hosted controller for stock OpenWrt. One Go process:

- serves an embedded React interface;
- stores controller state in SQLite;
- polls routers through OpenWrt's existing `rpcd`/ubus interface;
- turns a site-level network model into per-device UCI changes; and
- previews, applies, verifies, and confirms those changes.

The controller runs on a Linux or macOS computer, NAS, mini-PC, or server. No
oonfeeWRT executable runs on a managed router.

```text
Browser
  │ HTTP or reverse-proxied HTTPS
  ▼
oonfeewrtd
  ├─ embedded React UI
  ├─ REST API and WebSocket
  ├─ site model, renderer, and apply engine
  ├─ collector, topology inference, and event processing
  ├─ capability registry
  └─ SQLite + keyring.json
          │
          ├─ steady state: OpenWrt /ubus JSON-RPC
          └─ bounded bootstrap/cleanup: SSH
                    │
                    ▼
          stock OpenWrt routers, APs, and switches
```

## Components and responsibilities

### Controller process

`oonfeewrtd` owns the HTTP listener, database, keyring, router sessions,
collection loops, background maintenance, and embedded UI. It is deliberately a
single process: there is no separate database service or UI server to operate.

The release binary is static and the container image is built from `scratch`.
The container contains no shell or package manager and runs as a non-root user
by default.

### Web interface

The React/TypeScript interface is built into `ui/dist` and embedded in the Go
binary. It talks to the controller over same-origin REST and WebSocket
connections, so a normal deployment has no cross-origin configuration.

The main workspaces are Dashboard, Topology, Radios, Devices, Client Devices,
Policy Engine, Settings, Adopt a device, and Logs. What appears in those
workspaces depends on measured device capabilities; unavailable evidence is not
silently replaced with zeroes.

### Store and keyring

Controller state lives in a WAL-mode SQLite database named `oonfeewrt.db`.
Router and Wi-Fi credentials are sealed with a random data key held by the
adjacent `keyring.json`; the runtime passphrase unwraps that key.

These three items have different jobs:

| Item | Purpose | Can another item recreate it? |
|---|---|---|
| `oonfeewrt.db` | Inventory, desired state, accounts, events, audit history, rollups, sealed credential records | No |
| `keyring.json` | Wrapped random controller data key | No; neither the database nor passphrase can regenerate it |
| Runtime passphrase or passphrase file | Unlocks the keyring | No; it is not stored in the database or keyring |

Back up the database and matching keyring as one unit, and protect the runtime
passphrase separately. See [Data retention](./data-retention.md) and the
[installation and recovery guide](../INSTALL.md).

## Router communication

### Steady-state transport: ubus

Normal polling, capability reads, and configuration use OpenWrt's JSON-RPC ubus
endpoint, usually `/ubus` through `uhttpd`. A dedicated, scoped `oonfeewrt`
login is used instead of the router's administrator account.

The controller relies on stock OpenWrt objects such as `session`, `uci`,
`network`, `network.interface`, `network.device`, wireless/hostapd objects, and
capability-dependent `iwinfo` or `luci-rpc` methods. A missing object, method, or
driver fact becomes an explicit capability gap.

### Bounded exception: SSH

Stock rpcd cannot create its own scoped login or write an ACL file even when a
caller signs in to ubus as root. After explicit operator approval, adoption
therefore uses SSH to create at most:

- one scoped `oonfeewrt` login in rpcd configuration; and
- `/usr/share/rpcd/acl.d/oonfeewrt.json`.

The supplied device-administrator credential is used for that transaction and
is not stored. SSH is also used for approved cleanup, ACL refresh, and the
separately authorized optional-package workflow. It is not the polling or Apply
transport. See [Safety model](./safety.md).

## Capability-driven behavior

OpenWrt is a distribution across many kernels, drivers, switch designs, and
package sets. oonfeeWRT does not assume that two devices expose the same facts.
Adoption probes and stores evidence per device, including model, firmware,
radio, interface, package-manager, topology, and supported-method information.

The important state distinction is:

- **observed:** the source answered with usable evidence;
- **empty:** the source answered successfully and reported no entries;
- **unavailable/unsupported:** the source could not provide the fact;
- **stale:** older evidence exists, but a current read did not refresh it; or
- **unknown:** the controller has not established the fact.

This prevents an unsupported driver counter from looking like a real `0`, or a
failed topology read from looking like an empty network. The exact v0.1.3
feature and evidence boundary is in [Capabilities](../reference/capabilities.md).

## Site model and ownership

Configuration is authored as site intent—WLANs, groups, networks, zones,
policies, device functions, uplinks, meshes, and bounded per-device overrides.
The renderer turns that model into deterministic UCI sections for each selected
device.

oonfeeWRT coexists with LuCI by owning only the sections it creates. Managed
sections have an `oowrt_` name and/or an `oonfeewrt=1` marker, and the database
also records ownership. Existing human-managed sections remain visible but are
not silently rewritten or deleted. A foreign section that conflicts with the
desired result is a blocking conflict, not permission to take it over.

## Preview and Apply pipeline

Saving desired state changes the controller database only. It does not write a
router. Router changes follow a separate workflow:

1. **Render:** build the complete per-device desired documents.
2. **Diff:** compare desired state with current, owned router sections.
3. **Preview:** show exact creates, updates, deletes, gaps, and required
   acknowledgements.
4. **Fleet preflight:** verify every selected device before the first write.
5. **Apply:** stage owned UCI changes with OpenWrt rollback enabled.
6. **Health verification:** reconnect and read the expected runtime state.
7. **Confirm:** cancel OpenWrt's rollback timer only after verification passes.
8. **Receipt:** preserve the operation and per-device outcomes so a page reload
   does not retry the write.

Polling is quiesced around a write so collection cannot race the change. The
engine stops before later devices after the first failure. If connectivity is
lost, OpenWrt's own timer is the recovery mechanism. See
[Safety model](./safety.md) for operator recovery steps.

## Collection and live updates

The collector batches router reads and uses a baseline cadence of about one
poll per minute. A focused device view may temporarily use the six-per-minute
focused cadence. Per-device overrides can make full-state polling slower, up to
15 minutes; lightweight router-log collection remains once per minute.

Raw metric samples stay in memory until a complete five-minute window can be
written as one SQLite transaction. Older data is folded into hourly rollups.
The WebSocket carries bounded live `device.stats` frames; durable history still
comes from SQLite. See [Data retention](./data-retention.md) for the exact
limits.

## Deployment boundary

The default container mapping publishes `127.0.0.1:8080`. Bridge networking
supports normal L3 management and add-by-address adoption, but does not carry
LAN ARP or mDNS discovery. Linux host networking can enable full local
discovery, but exposes the listener according to the host's network and
firewall configuration.

The v0.1.3 HTTP listener has no native TLS. Keep it on loopback or a trusted,
isolated management network and use a trusted reverse proxy for remote browser
access. Review [Requirements](../reference/requirements.md) before deployment.

## Deliberate non-components

The architecture excludes:

- controller-authored agents, daemons, services, firmware, or package feeds on
  routers;
- a cloud relay, NAT traversal service, or multi-site broker;
- a separate database server;
- automatic package installation during adoption; and
- silent takeover of LuCI-managed configuration.

Those exclusions define the trust and support boundary; they are not missing
boxes in the diagram.

## Continue reading

- [Safety model](./safety.md)
- [Permissions and sessions](./permissions.md)
- [Data retention](./data-retention.md)
- [Requirements](../reference/requirements.md)
- [Capabilities and limits](../reference/capabilities.md)
