---
title: Safety model
description: Which oonfeeWRT actions can affect routers, how Apply rollback works, and how to recover safely.
---

# Safety model

oonfeeWRT v0.1.3 separates observation, controller desired state, and router
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
| Apply | Yes, over ubus | Yes | Changes only reviewed, controller-owned UCI sections |
| RF scan | Yes | No intended persistent change | Serving radio leaves channel temporarily; clients may be disrupted |
| Verify on air | Yes | No intended persistent change | Uses disruptive scans and therefore requires acknowledgement |
| Optional LLDP workflow | Yes, over SSH | Yes | May refresh package indexes, install official-feed packages, configure interfaces, and start a service after separate approvals |
| Un-adopt | Yes | Yes | Reverts/removes controller-owned configuration, then removes scoped access |
| Confirmed controller restore | No router call during restore | Controller data changes only | Restarts the controller, revokes sessions, and suppresses future router writes pending review |

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

Normal polling and Apply use the stored scoped credential. Refreshing the ACL
later again requires an ephemeral device-administrator credential and explicit
approval.

## Ownership prevents silent takeover

The controller renders named/marked UCI sections and records their ownership.
It writes and cleans up only those sections. Foreign sections created through
LuCI, SSH, another controller, or a package stay outside that boundary.

If a foreign section conflicts with desired state, stop and decide which system
should own it. Do not delete or rename the foreign section merely to make a
Preview green without first understanding its role.

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

The apply engine stages owned UCI changes and calls OpenWrt Apply with a 90-second
rollback timer, followed by a 15-second revert-verification grace period. It then
reconnects and verifies the expected configuration and runtime
health. Only a successful verification is confirmed. If connectivity is lost
or verification fails, the OpenWrt rollback timer is left active so it can
restore the previous state. A fresh read then distinguishes a proved
`reverted` outcome from `unknown`; an unknown or stranded operation may still
have the change live and must be inspected before retrying.

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
   state and the affected owned sections.
6. Confirm whether OpenWrt restored the pre-change configuration before making
   another change.
7. Generate a new Preview. Never assume an old preview token still describes
   the router.
8. Download a diagnostics bundle if the state remains unclear; it uses stored
   evidence and makes no router call.

If you must repair manually, change only the sections named by the Preview and
operation receipt. Preserve foreign sections and collect the event/audit detail
before clearing anything.

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

## Security limits to keep visible

- The controller has no native TLS listener in v0.1.3. Use loopback or a
  trusted management LAN and a trusted reverse proxy.
- No independent security audit or penetration test has been completed.
- Hardware support is capability-driven. The two-device end-to-end record and
  the separate Cudy read-only inspection report do not guarantee another
  OpenWrt target or validate Cudy adoption/Apply.
- Rollback protects UCI changes, not unrelated physical, firmware, upstream,
  or power failures.
- A portable backup contains sensitive controller state and saved credentials.
  Anyone with the file and export passphrase can recover that content.

See [Permissions](./permissions.md), [Troubleshooting](../reference/troubleshooting.md),
and [Requirements](../reference/requirements.md).
