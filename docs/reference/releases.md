# Release notes

The documentation covers the current stable patch release, **v0.1.5**,
published September 10, 2026, with explicitly marked development additions.
Release artifacts, checksums, container digests,
signatures, and attached notes on the GitHub release are the publication source
of truth.

## Current release

- [v0.1.5 release and downloads](https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.5)
- [v0.1.5 notes in the repository](https://github.com/aiden0rchad/oonfeeWRT/blob/main/docs/releases/v0.1.5.md)
- [All GitHub releases](https://github.com/aiden0rchad/oonfeeWRT/releases)

v0.1.5 separates device observation from configuration authority. One managed
Gateway continues to own site intent, while multiple reachable OpenWrt routers
can use **Monitor only** across existing routed subnets or VPNs. Monitor-only
devices stay in polling, inventory, telemetry, events, and topology, but use the
distinct read-only `oonfeewrt-monitor` ACL and are fenced from Preview, Apply,
desired/site configuration, optional LLDP install/config/remove mutations,
wireless-neighbor mutation, and other package/config/remove operations.
Existing LLDP observation remains available. Their scoped ACL lifecycle and
un-adoption remain explicit controller-maintenance actions.

Monitor only fences persistent configuration, not every active observation. A
capability-proved RF scan remains available after its separate disruption
acknowledgement; the serving radio can go off-channel and interrupt clients,
but the scan has no intended persistent configuration change.

The Policy Engine gains named exact-MAC policy sets with CRUD, stable
`source_set_id` firewall references, concrete resolution in the Master Table,
and an Object Manager **Secure (IPv4)** draft. Empty/malformed sets, missing
references, mixing `source_set_id` with direct `source_macs`, and
deleting a referenced set fail closed. Members must be
backed by a stored `local` observation from the currently adopted Managed
Gateway. Schema 23 records source-relative scope and `last_seen` in
`client_observations`, keyed by device and MAC. Monitor-only observations neither
satisfy nor contaminate that proof, including after un-adoption. The same gate
applies to direct or set-backed MAC Secure drafts and blocked/fixed-address
intent. Upgrades start without this provenance, and portable restore deliberately
clears it instead of importing source-controller write authority. Evidence older
than 30 days or more than five minutes in the future is rejected independently
of cleanup. Active MAC desired state blocks Preview until every
referenced client is re-observed locally. Existing blocked/fixed-address intent
can still be cleared one client at a time. Set edits change desired state only
and require a new Preview and acknowledged Apply.

The main controller screens also share consistent page headers, actions, and
responsive light/dark treatment. Phase 5 flow visibility remains a documented
feasibility track: this release installs no `nlbwmon`, `netifyd`, or DPI
package and ships no application identity/history.

The first v0.1.5 start migrates schema 20 to schema 21, adding management mode
with all existing devices preserved as Managed, then to schema 22 for policy
sets, and schema 23 for source-relative `client_observations` plus a rebuilt
one-managed-Gateway uniqueness guard based on canonical device functions and
the legacy role. Schema 23 also indexes observation and case-insensitive global
client MAC lookups so maximum-size policy checks remain bounded. Migration
configures no router and does not infer source provenance from global client
rows. Preserve the complete pre-upgrade database, keyring, and runtime
passphrase recovery unit: v0.1.4 cannot open schema 23, so changing only the
binary or image tag is not a rollback.

## Development after v0.1.5

The following changes are in current development source. They do not alter the
published v0.1.5 artifacts or declare a new release. Follow a future release's
notes before expecting them in a stable binary or image.

### Statistics and clearer history

The read-only [Statistics workspace](../guide/statistics.md) provides 6-hour
through 30-day stored WAN, system, exact-interface, and available stable-radio
rollups. It defaults to six hours, uses lines and lighter min/max shading, and
keeps small markers for isolated measurements rather than every connected
sample. Expand **History gaps** for exact observed/expected interval counts and
**About history and missing samples** for collection guidance.

Missing intervals remain blank, not zero traffic or proved downtime. The page
does not focus devices, backfill uncollected history, or turn rate samples into
traffic accounting.

### Accounts and controller navigation

The bottom **Controller** sidebar group contains **Settings**, **Accounts**,
and **Logs**, in that order. [Accounts](../operations/accounts.md) opens
**My account** for every signed-in role and adds **Manage accounts** only for
owners. Settings keeps Network, permission-gated Diagnostics, and owner-only
Backup & Restore. Account forms align their role selector with the other
fields and associate the selected role's description with the control for
keyboard and screen-reader use. Roles, account data, reauthentication,
session revocation, and server authorization are unchanged.

### Adoption and source information

The [adoption workflow](../getting-started/first-adoption.md) groups address and
inspection, device responsibilities, and access review into clearer steps.
**Install the oonfeeWRT controller access payload?** now contains the short
explanation and expandable **View access details**. Consent stays required;
adoption still installs only the reviewed scoped login and ACL, not packages
or firmware, and does not apply network configuration.

Routine limitations—fresh collection with earlier log gaps, unavailable DFS
classification, and uninstalled optional LLDP—use compact information with
source explanations and remedies. Current missing/stale log coverage,
collection failures, and risky or destructive actions retain warnings and
safety gates. [Troubleshooting](./troubleshooting.md#understand-notices-without-treating-every-gap-as-a-fault)
explains what can be fixed and which missing evidence cannot be recovered.

### Frontend and backend correctness

- Empty discovery and topology collections serialize consistently and render
  safe empty states; null collections from older responses no longer crash
  the affected views. Empty telemetry inventories are returned as arrays.
- A delayed management-overhead response cannot replace evidence for a
  different device after switching the detail panel.
- Retained Statistics samples stay tied to their device, metric, and exact
  series key. A failed replacement request cannot relabel the previous
  gateway's, interface's, or metric's history as the newly selected source.
- Current client association evidence governs attribution. An old AP's retry
  bucket is not shown as a measurement from the new AP after a roam; missing
  or ambiguous live signal remains unavailable.
- Device-detail series lookup failures are surfaced as errors instead of
  silently masquerading as an empty metric inventory.

These fixes do not add router write authority, new measurement sources, or
broader hardware-validation claims.

## Earlier releases

- [v0.1.4 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/docs/releases/v0.1.4.md)
- [v0.1.3 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/docs/releases/v0.1.3.md)
- [v0.1.2 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/docs/releases/v0.1.2.md)
- [v0.1.1 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/docs/releases/v0.1.1.md)
- [v0.1.0 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/docs/releases/v0.1.0.md)

Before upgrading, read both the versioned notes and
[Upgrade and roll back](../installation/upgrades.md).

### v0.1.4 IPv6 and topology boundary

v0.1.4 added explicit per-network IPv6 preserve, prefix-delegation, and
disabled policies behind Preview and Apply; existing networks defaulted to
Router managed. It also introduced current IPv6-warning status, read-only
router-clock observation, source-aware transitive wired-topology projection,
lower unchanged-topology work, and configurable Compose publishing.

Final retest of the transitive topology correction on the reporter's
multi-router hardware in
[issue #25](https://github.com/aiden0rchad/oonfeeWRT/issues/25) remains pending.
Automated and candidate evidence must not be presented as that physical proof.

### v0.1.3 effective-WAN boundary

v0.1.3 changed how the controller proves the active WAN. It selects the unique
usable lowest-metric IPv4 default installed in the kernel main table and maps
its runtime device to one active netifd logical interface. That corrected
layouts such as a DrayTek modem-management network beside PPPoE, where logical
`wan` uses kernel device `pppoe-wan`.

Missing, malformed, equal-metric ambiguous, ECMP/multipath, or unmappable
evidence remains unavailable instead of being guessed. Custom policy routing,
`mwan3`, per-uplink health, manual selection, and bond-member monitoring remain
out of scope in v0.1.5 as well.

### v0.1.2 compatibility-report boundary

v0.1.2 corrected single-interface/two-GMAC inspection and physical-radio
counting, with reporter-confirmed Cudy M3000 v2 read-only evidence. It also
added **Export sanitized compatibility report** after Inspect. That format-v1,
server-built JSON is bounded and allowlisted; it excludes deployment identity,
addresses, credentials/secrets, network configuration, clients, live telemetry,
timestamps, runtime radio/PHY and bridge-member identifiers, and free-text
notes. Download is browser-local with no extra router call, controller
persistence, or upload.

The Cudy evidence proves only the reported physical-radio count and direct
LAN/WAN layout. It does not validate adoption, Apply, tagged VLAN management,
WLAN/client operation, topology, RF, telemetry/resource budgets, speed testing,
un-adoption, or broader Filogic hardware.

## Verify what you run

For a standalone archive, verify its entry in `SHA256SUMS` before extracting
or installing it. For the OCI image, pin `v0.1.5` or the immutable digest and
verify the GitHub Actions keyless signature as shown in the [Docker Compose
guide](../installation/docker.md).

The macOS binary is not Developer ID signed or notarized. A checksum mismatch
is never an instruction to bypass the check.

## Version and schema boundaries

The daemon prints its build version with:

```sh
oonfeewrtd -version
```

The current documentation targets database schema 23.

| Transition | Schema/data effect | Router-access effect |
|---|---|---|
| v0.1.1 → v0.1.2 | Schema 19; no migration | No router change for upgrade; compatibility reporting is an Inspect/UI feature |
| v0.1.2 → v0.1.3 | Schema 19; no migration or startup deletion | No ACL refresh or re-adoption; the exact read-only route command has been in the scoped ACL since v0.1.0 |
| v0.1.3 → v0.1.4 | Migrates schema 19 to 20; adds a topology index and normalizes two historical source names, retaining the newest duplicate observation | Ordinary polling needs no access change; existing adoptions need a separately acknowledged ACL refresh only for router-clock status; IPv6 remains Router managed until explicitly changed through Preview/Apply |
| v0.1.4 → v0.1.5 | Migrates schema 20 → 21 → 22 → 23; existing devices become Managed, policy-set tables/references are added, then per-device client provenance, observation/global-client MAC indexes, and the hardened one-managed-Gateway index are added | Startup makes no router write or inferred provenance; selecting Monitor only later uses the reviewed ACL lifecycle to install the distinct read-only ACL |
| v0.1.5 → v0.1.4 | Restore the matching pre-upgrade schema-20 database, keyring, and passphrase; v0.1.4 cannot open schema 23 | Controller rollback does not revert router configuration applied while v0.1.5 was running |

Preserve the matching database/keyring pair before every transition. The
controller migrates supported older state at startup and refuses unsupported
downgrades. A rollback across a schema boundary restores the matching
pre-upgrade database and `keyring.json`; changing only the binary or image tag
is not a data rollback. Historical `v0.1.0-rc.1` uses schema 17 and must not
open schema-19, schema-20, or schema-23 state.
