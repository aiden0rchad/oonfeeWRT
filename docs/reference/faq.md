---
title: Frequently asked questions
description: Direct answers about deployment, router changes, compatibility, security, backup, and current limits.
---

# Frequently asked questions

Answers below describe **oonfeeWRT v0.1.3**.

## What is oonfeeWRT?

It is a self-hosted controller for stock OpenWrt. It provides one interface for
fleet visibility, site configuration, safe Apply/rollback, topology, clients,
radios, events, accounts, diagnostics, and controller backup/restore.

## Is oonfeeWRT firmware?

No. It does not build, replace, patch, or upgrade OpenWrt firmware. Managed
routers stay on stock OpenWrt and continue to work with LuCI.

## Does the controller run on a router?

No supported release does. Run the controller on a 64-bit Linux or macOS host,
NAS, mini-PC, SBC, server, or Docker host. OpenWrt-hosted operation is not built,
packaged, tested, or budgeted.

## Is Docker required?

No. The standalone binary and container run the same controller and embedded
UI. Release binaries support Linux/macOS on amd64/arm64; the image supports
Linux amd64/arm64.

## Does it require the cloud?

No. Controller state and accounts are local. Remote sites require an existing
routed management network or VPN; oonfeeWRT does not provide cloud relay or NAT
traversal.

Internet access is still needed for actions that inherently use it, such as
downloading releases/images, reaching OpenWrt package feeds for an explicitly
approved optional capability, or running the Cloudflare speed test.

## Can it manage non-OpenWrt or UniFi devices?

No. Its transport, capability model, and renderer target OpenWrt. It does not
implement vendor inform protocols or proprietary device APIs.

## Does adoption install software on my router?

Adoption never installs a package, executable, daemon, service, firmware, or
custom controller code. With explicit default-off consent, it creates/replaces
one rpcd ACL JSON file and creates one scoped `oonfeewrt` login.

The optional LLDP capability is a separate workflow. It may install official
OpenWrt feed packages only after showing and binding an exact plan to separate
acknowledgements.

## Why does adoption ask for SSH?

Stock rpcd cannot create its own scoped login or write an ACL file through ubus,
even when the ubus caller is root. SSH is therefore used for the approved
bootstrap transaction. Normal polling and configuration use scoped ubus.

The device administrator credential is used for the transaction and is not
stored.

## Can I keep using LuCI and SSH?

Yes. oonfeeWRT writes only sections it owns and leaves foreign/human-managed
sections alone. If a foreign section conflicts with desired state, Preview
blocks or reports the conflict instead of silently taking it over.

Avoid editing the same controller-owned section simultaneously through LuCI.
Finish or abandon one change path, then generate a fresh Preview.

## Does saving a WLAN or network immediately change routers?

No. Saving changes controller desired state only. Preview computes exact
per-device changes; Apply is a separate reviewed action.

## What happens if Apply breaks connectivity?

oonfeeWRT stages UCI with OpenWrt rollback enabled. It confirms only after
reconnecting and verifying expected runtime state. If connectivity or health
verification fails, the router's rollback timer is left active. A subsequent
read can prove that OpenWrt reverted; until it does, the outcome is `unknown`
and the changed state may still be live. Do not retry until the first operation
is understood.

Keep independent LuCI/SSH access and start with one non-critical device. See the
[Safety model](../concepts/safety.md).

## Does it support every OpenWrt device?

No universal claim is possible. Hardware, drivers, switch architecture, rpcd
modules, and package sets vary. The controller probes each device and reports
unsupported, unavailable, stale, and partial facts.

The full published physical record covers a Linksys WRT3200ACM and TP-Link
Archer C6 v2 on OpenWrt 25.12.5. v0.1.3 additionally has reporter-confirmed,
read-only inspection evidence for one Cudy M3000 v2/Filogic variant; it does not
claim adoption, Apply, VLAN, or broader Filogic validation. Other devices may
work with different gaps.

## What can I share from Inspect?

Use **Export sanitized compatibility report**, not the full Inspect response or
raw router output. The format-version-1 JSON contains a bounded allowlist of
board/firmware, physical-radio, LAN/WAN label, switch-mode, feature-state, and
supported-function evidence. It excludes the target address and MAC, site and
router identity, credentials, network configuration, clients, live telemetry,
timestamps, runtime radio/PHY and bridge-member identifiers, and free-text
notes.

The server omits the report when it cannot satisfy strict safety and size
bounds. The browser download makes no extra router call, is not stored or
uploaded by the controller, and becomes an ordinary local file after download.
It helps maintainers compare compatibility evidence; it is not proof that
adoption, Apply, VLANs, RF, telemetry, or resource budgets work on that device.

## Why are some values “Unavailable” instead of zero?

Zero is a measurement. Unavailable means the controller could not obtain or
trust the measurement. Treating a missing driver counter or failed RPC as zero
would produce confident but false charts and health claims.

## How does v0.1.3 choose the WAN interface?

It reads the installed IPv4 route table and netifd logical interfaces in one
topology poll. The controller selects the unique usable lowest-metric default
in the kernel main table, then maps its kernel device to exactly one active
logical interface that also reports a default. This is why logical `wan` over
runtime `pppoe-wan` can use `pppoe-wan` traffic counters while a modem
management interface no longer wins by list order.

It does not choose between distinct equal-metric defaults, ECMP/multipath,
policy-routing tables, `mwan3`, unmappable devices, or bond members. Missing,
malformed, ambiguous, or inconsistent evidence stays unavailable, and an exact
matching RX/TX series must exist before WAN throughput is shown. Collection is
on a baseline 15-minute topology cycle, not a rapid failover monitor.

## Why is an online device “Unplaced” in Topology?

Online proves controller reachability. It does not prove a current physical
parent. Dynamic FDB or neighbor evidence can expire, and some stock BusyBox
sources omit VLAN/port identity. The graph refuses to invent a link.

Optional LLDP can provide stronger managed adjacency, at the cost of an
explicit official-package/service workflow.

## Does RF scanning run automatically?

No. A serving-radio scan can disrupt clients, so it requires an explicit
acknowledgement and is never scheduled. Capability refresh/re-probe does not run
a scan.

## Where does the speed test run?

From the controller host or container through Cloudflare. It does not call or
modify a router, but its traffic follows the normal WAN path. The test uses
about 15 MiB, is bounded to 30 seconds, and can temporarily saturate the WAN.

Gateway-run testing, loaded latency, and loaded jitter are unavailable in
v0.1.3.

## Does the controller have HTTPS?

Not natively in v0.1.3. Bind it to loopback or a trusted isolated management
LAN and use a trusted reverse proxy for TLS. Do not expose port 8080 directly to
the Internet.

## What account roles are available?

Owner, Administrator, Operator, and Read-only. Owners manage accounts and
portable backup/restore; Administrators manage devices/configuration and
diagnostics; Operators run approved operational tests; Read-only users view
state. See [Permissions](../concepts/permissions.md).

## Where are credentials stored?

Router and Wi-Fi credentials are sealed in SQLite using a random data key from
`keyring.json`. The runtime passphrase unwraps that key. The device
administrator credential supplied for bootstrap/cleanup is not stored.

Authenticated WLAN/mesh reads report only whether a key exists; there is no
reveal endpoint.

## What must I back up?

For filesystem recovery:

- a consistent `oonfeewrt.db` snapshot;
- its exact sibling `keyring.json`; and
- the runtime passphrase/passphrase file, kept separately.

The database, keyring, and passphrase cannot recreate one another. Do not copy
only a live WAL-mode database main file.

Alternatively, an Owner can export encrypted `.oowrtbak` controller state with
a separate export passphrase. See [Data and retention](../concepts/data-retention.md).

## Does portable restore change routers?

No router is contacted or automatically configured during restore. Successful
restore restarts the controller, revokes sessions, creates a pre-restore safety
artifact, and suppresses router writes until an Owner reviews state and types
`RESUME ROUTER WRITES` after reauthentication.

## What is in a diagnostics bundle?

Bounded stored controller evidence: controller health/version/schema, stored
device model/firmware/capability facts, coverage state, bounded events, and a
redacted controller-log tail. Generation makes zero live router management
calls.

It excludes passwords, password hashes, sessions/CSRF tokens, router
credentials, Wi-Fi keys, private keys/certificates, raw database/keyring, client
notes, and fixed-address assignments.

## How long is history kept?

Defaults include five-minute metrics for 14 days, hourly metrics for 396 days,
OpenWrt logs for 24 hours, closed topology intervals for 31 days, 100,000
controller/audit events, and the newest three terminal speed tests. See the
complete [retention table](../concepts/data-retention.md).

## Can I downgrade from v0.1.3?

v0.1.3 and v0.1.2 both use schema 19, so a clean binary/image rollback is
schema-compatible; preserve the v0.1.3 data pair first.

v0.1.1 also uses schema 19, but rolling back skips later fixes and features.
Preserve the current database/keyring pair and use the exact release notes when
choosing a target; schema compatibility alone is not an operational guarantee.

Historical `v0.1.0-rc.1` uses schema 17. Rolling back that far requires the
untouched pre-upgrade schema-17 database, matching keyring, prior passphrase,
and old binary/image together. Do not open schema-19 data with the RC daemon.

## What is deliberately out of scope?

- controller-authored router agents/daemons;
- custom firmware, forks, or package feeds;
- non-OpenWrt device adoption;
- cloud remote access, SSO brokering, multi-site, and automatic NAT traversal;
- native mobile apps;
- continuous proprietary spectrum analysis, paid threat feeds, and branded AI
  features; and
- DPI/application flow history on constrained routers in v0.1.3.

## Where should I start?

Read [Requirements](./requirements.md), follow [`INSTALL.md`](../INSTALL.md),
and adopt one non-critical router. For a failure, use
[Troubleshooting](./troubleshooting.md) before changing the router manually.
