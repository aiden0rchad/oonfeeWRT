---
title: Safety model
description: Which oonfeeWRT actions can affect routers, how Apply rollback works, and how to recover safely.
---

# Safety model

oonfeeWRT v0.1.7 separates observation, controller desired state, and router
mutation. A device appearing in the UI is never permission to change it.

## Know what an action can change

| Action | Router management calls | Persistent router change | Operational effect |
|---|---:|---:|---|
| Start the controller | Yes, read-only polling after startup | No | Opens controller data, starts HTTP service, and resumes collection for adopted devices |
| Discovery scan or add by address | Network probe only | No | Discovery coverage depends on container networking |
| Pre-adoption Inspect | Read-only authenticated calls | No | Uses the supplied device credential only for the inspection |
| Export sanitized compatibility report | No additional router call | No | Downloads the server-produced, bounded Inspect projection in the browser; no controller persistence or upload |
| Save site settings | No | No | Changes desired state in the controller database |
| Preview | Read-only calls | No | Computes exact per-device differences and gates |
| Diagnostics bundle | No live router calls | No | Packages bounded, redacted stored controller evidence |
| Portable backup/restore preview | No | No | Reads, authenticates, and stages controller data |
| Controller-host speed test | No router API/SSH call | No | Sends about 15 MiB through the normal WAN path; may saturate it for up to 30 seconds |
| Effective-WAN/topology collection | Read-only ubus and bounded `file.exec` calls | No | Observes the route table and logical interfaces; does not change routes, metrics, PPPoE, firewall, or failover |
| Adoption with access payload accepted | Yes, over SSH | Yes | Creates/replaces the scoped login and ACL only |
| Apply (managed devices only) | Yes, over ubus | Yes | Changes reviewed controller-owned UCI sections and, only for an explicit management-LAN IPv6 mode, the allowlisted options on exact existing foreign sections described below |
| RF scan | Yes | No intended persistent change | Serving radio leaves channel temporarily; clients may be disrupted |
| Verify on air | Yes | No intended persistent change | Uses disruptive scans and therefore requires acknowledgement |
| Optional LLDP workflow (managed devices only) | Yes, over SSH | Yes | May refresh package indexes, install official-feed packages, configure interfaces, and start a service after separate approvals; monitor-only devices are refused |
| Un-adopt | Yes | Yes | Reverts/removes controller-owned configuration, then removes scoped access |
| Confirmed controller restore | No router call during restore | Controller data changes only | Restarts the controller, revokes sessions, and suppresses future router writes pending review |

### Optional actions retained in v0.1.7

These actions were introduced in v0.1.6 and are retained in v0.1.7.
The Precision navigation and compact presentation do not relax their gates.
Opening Settings → Firmware or Settings → Integrations loads stored state;
it does not perform an external catalogue or service check.

| Action | Contact or change boundary |
|---|---|
| Statistics, Reports, CSV export, inventory filters | Reads stored observations; CSV is downloaded in the browser, not uploaded |
| Topology arrangement/reset | Changes account-scoped browser coordinates only; never creates a link or edits controller/router state |
| Create an alert rule | Owner changes controller state; evaluation uses existing evidence, not extra router probes or automatic remediation |
| Enable webhook delivery | Explicitly authorizes bounded outbound notification requests to the configured destination; this is external data disclosure, not router configuration |
| Check firmware catalogue | Administrator/Owner explicitly contacts the official OpenWrt service using stored target/release information; no router call, image download, or flash |
| Save/remove AdGuard connection | Reauthenticated Owner changes encrypted controller configuration; saving does not contact AdGuard |
| Check AdGuard | Administrator/Owner explicitly reads aggregate service observations; no DNS configuration change or client query-log collection |
| Check WireGuard | Administrator/Owner requests narrow helper-backed peer/counter reads; missing access stays unavailable, with no automatic install or permission grant |
| Build/run isolated demo | Synthetic local fixtures only; no real API/live channel, controller database, external checks, or router contact |

The optional experimental rpcd helper is a manually installed, on-demand
read-only extension, not part of the adoption payload. It adds no background
daemon, network listener, general remote command method, or firmware-write
operation. A stock UCI rollback window is not a firmware recovery plan. See
[Firmware](../guide/firmware.md) before treating catalogue metadata as a next
step toward a manual upgrade.

## Three separate permissions

Treat these as different decisions:

1. **Observe a router.** Inspection and polling establish facts. Missing facts
   remain unavailable.
2. **Save desired state.** Editing a WLAN or network records intent in SQLite.
   It does not Apply it.
3. **Change routers.** Apply, adoption payloads, RF scans, and optional
   capabilities each have their own review and acknowledgement.

Accepting the adoption payload does not authorize a later WLAN, network,
firewall, DHCP, or package change.

## Management mode is an independent boundary

Every adopted device is **Managed** or **Monitor only**. Both modes use an
explicit scoped-login/ACL bootstrap and can be polled; monitor only installs
the distinct read-only `oonfeewrt-monitor` ACL group. Management mode then
controls whether the device can receive desired configuration:

- Managed devices can participate in Preview and Apply when their selected
  functions, capabilities, and ownership checks permit it.
- Monitor-only devices contribute inventory, telemetry, events, and topology,
  but are excluded from Preview, Apply, desired/site configuration, optional
  LLDP install/config/remove mutations, wireless-neighbor mutations, and other
  package/config/remove operations. Existing LLDP observation remains
  available.
- A capable monitor-only radio can still run a separately acknowledged RF scan.
  This is an active, transient observation: it takes the radio off-channel and
  may interrupt clients, but makes no intended persistent configuration change.
- Scoped ACL maintenance and un-adoption remain deliberate, reviewed actions
  for monitor-only devices; otherwise the access footprint could not be
  updated or removed safely.

The server enforces these checks even when a caller bypasses hidden UI controls.
A site permits at most one managed Gateway, while multiple reachable
monitor-only routed devices are allowed. This is an authority boundary, not a
routing feature: the controller still requires an existing route or VPN to each
device's SSH and HTTP or HTTPS `/ubus` endpoint.

## Compatibility reports are a separate safe projection

Do not share the full Inspect response. It can contain the target MAC,
deployment-specific facts, and free-text notes. **Export sanitized compatibility
report** is a server-built format-version-1 document with only allowlisted
hardware, firmware, radio, port, feature-state, and supported-function fields.

The builder rejects unknown functions or switch modes, unsafe interface names,
excessive radio/port evidence, and encoded output over 64 KiB. It normalizes and
caps individual text fields at 256 bytes, strips address- and secret-shaped
text, and replaces the exact sensitive values used for Inspect. If those checks
cannot prove the output safe, Inspect can succeed but the report is omitted
with an explanatory note. Do not reconstruct a report by copying raw API
responses or router command output.

Downloading the report is browser-local. It makes no second router request,
creates no controller job or stored artifact, and performs no automatic upload.
The downloaded file is then governed by the operator's browser, workstation,
and chosen sharing channel rather than controller retention or RBAC.

## The access payload

When accepted during adoption, the controller may install or replace one rpcd
ACL JSON file and create one scoped login named `oonfeewrt`. This payload:

- grants only the documented OpenWrt object, method, path, and executable
  surface;
- installs no package, binary, daemon, service, firmware, or custom device
  code;
- does not itself change WLAN, network, firewall, or DHCP configuration; and
- uses the device administrator credential only for the SSH transaction.

Normal polling and managed-device Apply use the stored scoped credential.
Monitor-only adoption and refresh install the distinct read-only
`oonfeewrt-monitor` ACL group. Refreshing the ACL
later again requires an ephemeral device-administrator credential and explicit
approval.

## Reusable policy-set safety

Named policy sets contain canonical exact MAC addresses and can be referenced
by firewall rules through a stable set ID. Saving or editing a set changes
controller intent only. At validation, Master Table, and render time, the
controller resolves the current members so the concrete affected clients can
be reviewed.

Empty or malformed sets, missing references, a rule that mixes direct MACs with
a set reference, and membership outside the stored observed-client inventory
or outside proved local managed-Gateway scope fail closed. Schema 23 requires a
stored `local` observation for each MAC from the currently adopted Managed
Gateway. Monitor-only observations neither satisfy nor contaminate this proof,
including after un-adoption. Until a successful Gateway poll establishes proof,
set creation/update and MAC-based Object Manager drafts are refused and active
direct/set firewall, blocked-client, or fixed-address intent blocks Preview.
Existing blocked/fixed-address intent can still be cleared one client at a
time. A set cannot be deleted while any enabled or disabled rule still
references it. Membership changes require a fresh Preview and acknowledged
Apply before a managed router changes.

MAC membership is not authentication. A client can randomize or spoof an
address; use a stronger network/application control for high-trust boundaries.

## Ownership prevents silent takeover

The controller renders named/marked UCI sections and records their ownership.
It ordinarily writes and cleans up only those sections. Foreign sections
created through LuCI, SSH, another controller, or a package stay outside that
boundary.

v0.1.4 has one deliberately narrow exception. When an operator selects
**Prefix delegation** or **Disabled** for the Gateway management LAN, Preview
may propose option-only patches to the exact existing LAN interface, its one
matching DHCP section, and supported conventional `network.wan`/`network.wan6`
sections when present. The patch is limited to IPv6 assignment, RA, DHCPv6,
NDP, and conventional upstream IPv6-enable options. It cannot create, claim,
rename, delete, or mark a foreign section as owned. A missing or ambiguous LAN
or DHCP target, a wrong-type present target, or static IPv6 state that Disabled
would need to remove blocks Preview. Absent `wan`/`wan6` sections remain absent.

**Router managed** makes no such option patch. Returning to Router managed or
un-adopting a device does not reconstruct the values from before an earlier
explicit IPv6 Apply. Keep a router backup and restore operator-authored values
deliberately when reversal is required.

If a foreign section conflicts with desired state, stop and decide which system
should own it. Do not delete or rename the foreign section merely to make a
Preview green without first understanding its role. The management-LAN IPv6
exception does not turn any other foreign conflict into writable state.

## How Apply protects connectivity

Every Apply has two layers of protection.

### Before the first write

- Preview is generated for a particular desired state and fleet.
- Required acknowledgements are bound to that plan.
- Every selected device is preflighted before any selected device is changed.
- Dirty or foreign configuration, missing capabilities, management-path risk,
  and other gates can block the operation.
- The operation and actor are recorded durably.

If preflight fails, nothing should be written. Fix the named condition and
generate a fresh Preview; do not reuse a stale plan.

### After staging

The apply engine stages reviewed controller-owned UCI changes and any explicit,
allowlisted management-LAN IPv6 option patches, then calls OpenWrt Apply with a
90-second rollback timer followed by a 15-second revert-verification grace
period. It reconnects and verifies the expected configuration and runtime
health. Only a successful verification is confirmed. If connectivity is lost
or verification fails, the OpenWrt rollback timer is left active so it can
restore the previous state. A fresh read then distinguishes a proved `reverted`
outcome from `unknown`; an unknown or stranded operation may still have the
change live and must be inspected before retrying.

The operation continues independently of the browser request. Reloading the
page reads the durable status; it does not submit the write again. A fleet
operation stops before later devices after the first failed device.

## Safe operating procedure

For a change that could affect connectivity:

1. Back up the router configuration using OpenWrt's normal backup facility.
2. Back up the controller database/keyring pair or export a portable backup.
3. Start with one non-critical device.
4. Save the smallest useful desired-state change.
5. Read every Preview row, capability gap, and acknowledgement.
6. Confirm that the controller's path to the device is not being removed.
7. Apply and keep independent access to the management network available.
8. Wait for the durable operation to become terminal.
9. Verify connectivity, expected SSIDs/interfaces, DHCP, DNS, and Internet
   access as appropriate.
10. Re-run Preview. A successful rollout should report no unexplained drift.
11. Continue to the next device only after the first result is understood.

## Recovery after an uncertain Apply

If the UI reports a failed, unknown, interrupted, or stranded result:

1. **Do not immediately retry.** A second plan can hide the first operation's
   real state.
2. Wait through the displayed OpenWrt rollback window.
3. Test the device's management address from the controller host.
4. Check the durable Apply receipt after reconnecting or restarting the UI.
5. Use LuCI or SSH from an independent management path to inspect pending UCI
   state, the affected owned sections, and any exact management-LAN IPv6 patch
   targets named by the operation.
6. Confirm whether OpenWrt restored the pre-change configuration before making
   another change.
7. Generate a new Preview. Never assume an old preview token still describes
   the router.
8. Download a diagnostics bundle if the state remains unclear; it uses stored
   evidence and makes no router call.

If you must repair manually, change only the sections and options named by the
Preview and operation receipt. Preserve unrelated foreign state and collect the
event/audit detail before clearing anything.

## Optional packages require a second boundary

v0.1.3's optional LLDP workflow is not part of adoption. The controller first
resolves an exact `apk` or `opkg` plan. Package-index refresh, installation,
service configuration, and rollback have explicit review/consent steps.

The capability ledger records package manager, prior package/service state,
packages actually added, and rollback outcome. Un-adoption is blocked while an
LLDP capability record still requires rollback, preventing silent package
residue. Rollback removes only recorded additions and restores the prior
service/configuration state.

## Restore is fenced from routers

A confirmed `.oowrtbak` restore changes controller state and restarts the
process; it does not Apply restored desired state to routers. Successful restore:

- revokes every controller session;
- retains an encrypted pre-restore safety artifact;
- records the restore outcome; and
- activates a durable router-write suppression gate.

Read-only monitoring may resume with restored credentials while the gate is
active. An owner must review inventory and desired state, reauthenticate, and
type `RESUME ROUTER WRITES` to remove it. Removing the gate also permits
automatic 802.11k neighbour maintenance, so review roaming intent first.

In v0.1.7, restore separately pauses external webhook delivery, cancels
the pending outbox, and resets alert continuity. Rules/history and the
encrypted destination remain for review. An Owner explicitly re-enables only
future delivery after checking the restored environment; resuming router
writes does not resume notifications. Missing observations cannot manufacture
a recovery event. Saved AdGuard settings are not an instruction to poll it
automatically after restore.

## Security limits to keep visible

- The controller has no native TLS listener in v0.1.7. Use loopback or a
  trusted management LAN and a trusted reverse proxy.
- No independent security audit or penetration test has been completed.
- Hardware support is capability-driven. The two-device end-to-end record and
  the separate Cudy read-only inspection report do not guarantee another
  OpenWrt target or validate Cudy adoption/Apply.
- Rollback protects UCI changes, not unrelated physical, firmware, upstream,
  or power failures.
- A portable backup contains sensitive controller state and saved credentials.
  Anyone with the file and export passphrase can recover that content.
- The Phase 5 flow-visibility document is a feasibility plan. v0.1.7 installs
  no DPI/flow package and stores no application-flow history.

See [Permissions](./permissions.md), [Troubleshooting](../reference/troubleshooting.md),
and [Requirements](../reference/requirements.md).
