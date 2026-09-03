---
title: Architecture
description: How oonfeeWRT is divided, where it runs, and how controller intent becomes safe OpenWrt configuration.
---

# Architecture

This page describes the architecture shipped in **oonfeeWRT v0.1.4**. For the
implementation record and historical design decisions, see
[`ARCHITECTURE.md`](../ARCHITECTURE.md) and
[`IMPLEMENTATION.md`](../IMPLEMENTATION.md).

## The short version

oonfeeWRT is a self-hosted controller for stock OpenWrt. One Go process:

- serves an embedded React interface;
- stores controller state in SQLite;
- polls routers through OpenWrt's existing `rpcd`/ubus interface;
- turns read-only inspection into a bounded, shareable compatibility report;
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
failed topology read from looking like an empty network. The exact v0.1.4
feature and evidence boundary is in [Capabilities](../reference/capabilities.md).

### Shareable compatibility evidence

The ordinary Inspect result contains deployment-specific data and free-text
notes, so it is not safe to serialize wholesale. Since v0.1.2, the server also
builds a separate compatibility document from an explicit allowlist. Format
version 1 contains bounded controller-version, board, firmware, radio, port,
feature-state, and supported-function evidence. It excludes the management
address, MAC address, site/router identity, credentials, network configuration,
clients, live telemetry, timestamps, runtime radio/PHY identifiers, bridge
members, and free-text notes.

Sanitization and size checks happen on the server. Evidence outside the strict
bounds makes the report unavailable while the underlying read-only inspection
can still succeed. The browser only pretty-prints that already-sanitized object
and downloads it; it does not make another router call, persist the report in
the controller, or upload it anywhere.

## Site model and ownership

Configuration is authored as site intent—WLANs, groups, networks, zones,
policies, device functions, uplinks, meshes, and bounded per-device overrides.
The renderer turns that model into deterministic UCI sections for each selected
device.

oonfeeWRT coexists with LuCI by owning the sections it creates. Managed sections
have an `oowrt_` name and/or an `oonfeewrt=1` marker, and the database also
records ownership. Existing human-managed sections remain visible and ordinary
rendering never rewrites or deletes them.

v0.1.4 has one narrow, non-owning exception. After an operator explicitly
selects **Prefix delegation** or **Disabled**, Preview may contain allowlisted
option patches for the exact existing management LAN interface, its single
matching DHCP section, and supported conventional `wan`/`wan6` sections. These
patches cannot create, claim, rename, prune, or delete a foreign section.
Ambiguous, wrong-type, or conflicting static-IPv6 targets block Preview rather
than widening ownership.

## Preview and Apply pipeline

Saving desired state changes the controller database only. It does not write a
router. Router changes follow a separate workflow:

1. **Render:** build the complete per-device desired documents.
2. **Diff:** compare desired state with current owned sections and any
   explicitly selected, allowlisted management-LAN IPv6 option patches.
3. **Preview:** show exact creates, updates, deletes, gaps, and required
   acknowledgements.
4. **Fleet preflight:** verify every selected device before the first write.
5. **Apply:** stage the reviewed UCI changes with OpenWrt rollback enabled.
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

### Effective WAN route evidence

On the slower network/topology cycle, v0.1.3 observes the installed IPv4 route
table and netifd's logical-interface dump in the same batch. It selects the one
usable, lowest-metric default in the kernel main table, then requires that
kernel device to map to exactly one active logical interface that also reports
a default route. This maps layouts such as logical `wan` over runtime
`pppoe-wan` without letting another netifd default candidate win by list order.

The two reads are one proof. A missing command result, malformed route,
equal-metric route on a different device, ECMP/multipath route, or ambiguous or
absent netifd mapping makes the observation unavailable. Partial evidence does
not overwrite the last proved network scope or become an invented Internet
edge.

The measured topology edge retains the kernel interface used by the route and,
when different, the logical interface used for address scoping. Device Detail
receives that proved kernel interface as its current chart candidate; the chart
can remain empty until samples for the key exist. Dashboard promotes the route
interface to WAN throughput only when that exact key is present in the durable
RX/TX series catalog. Otherwise Dashboard throughput remains unavailable
instead of falling back to a name such as `wan` or the first Ethernet
interface. The baseline 15-minute topology cadence is not a failover monitor
and does not model policy-routing-table selection, `mwan3`, ECMP, per-uplink
health, or bond members.

### Transitive wired-topology evidence

v0.1.4 keeps raw FDB/LLDP observations in topology history, but projects the
live graph through a fresh source-aware multi-hop path when those observations
prove that one managed device is reached through another. This prevents a
downstream managed device from appearing as a second direct child of the
gateway. The projection is not permanent truth: stale, failed, or ambiguous
sources expose the ambiguity again instead of carrying a prior path forward.

Unchanged links do not churn history rows. Closed-history lookup uses the
schema-20 index, and browser refreshes cancel abandoned requests rather than
stacking overlapping topology queries.

## Deployment boundary

The default container mapping publishes port 8080 on host loopback.
`OONFEE_HTTP_BIND` can deliberately select one host management IP or every
IPv4 interface without changing the container's internal `:8080` listener.
Bridge networking supports normal L3 management and add-by-address adoption,
but does not carry host LAN interfaces into the container's automatic
discovery plan. Discovery is a bounded IPv4 TCP/HTTP scan with an
unauthenticated `/ubus` object-list
fingerprint; it implements neither ARP-table discovery nor mDNS. Linux host
networking can expose eligible directly attached host LAN interfaces to that
same scanner, but also exposes the listener according to the host's network and
firewall configuration.

The HTTP listener has no native TLS. Keep it on loopback or a trusted,
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
