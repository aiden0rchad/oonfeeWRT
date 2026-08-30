# Release notes

The documentation describes the current patch release, **v0.1.3**. Release
artifacts, checksums, container digests, signatures, and attached notes on the
GitHub release are the publication source of truth.

## Current release

- [v0.1.3 release and downloads](https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.3)
- [v0.1.3 notes in the repository](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.3.md)
- [All GitHub releases](https://github.com/aiden0rchad/oonfeeWRT/releases)

v0.1.3 changes how the controller proves the active WAN. It selects the unique
usable lowest-metric IPv4 default installed in the kernel main table and maps
its runtime device to one active netifd logical interface. This fixes layouts
such as a DrayTek modem-management network beside PPPoE, where logical `wan`
uses kernel device `pppoe-wan`.

The route table and logical-interface dump are one observation. Missing,
malformed, equal-metric ambiguous, ECMP/multipath, or unmappable evidence stays
unavailable and preserves the last proved network scope instead of guessing.
Custom policy routing, `mwan3`, per-uplink health, manual selection, and
bond-member monitoring remain out of scope. Collection is on the slow topology
cycle, not a rapid failover monitor. The change is read-only and does not alter
routes, metrics, PPPoE, firewall, failover, or other router configuration.

## Earlier releases

- [v0.1.2 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.2.md)
- [v0.1.1 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.1.md)
- [v0.1.0 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.0.md)

Before upgrading, read both the release notes and [Upgrade and roll back](../installation/upgrades.md).

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
or installing it. For the OCI image, pin `v0.1.3` or the immutable digest and
verify the GitHub Actions keyless signature as shown in the [Docker Compose
guide](../installation/docker.md).

The macOS binary is not Developer ID signed or notarized. A checksum mismatch
is never an instruction to bypass the check.

## Version and schema boundaries

The daemon prints its build version with:

```sh
oonfeewrtd -version
```

The current documentation targets database schema 19.

| Transition | Schema/data effect | Router-access effect |
|---|---|---|
| v0.1.1 → v0.1.2 | Schema 19; no migration | No router change for upgrade; compatibility reporting is an Inspect/UI feature |
| v0.1.2 → v0.1.3 | Schema 19; no migration or startup deletion | No ACL refresh or re-adoption; the exact read-only route command has been in the scoped ACL since v0.1.0 |
| v0.1.3 → v0.1.2 | Schema-compatible binary/image rollback | Retains controller data; loses the v0.1.3 WAN-selection fix |

Preserve the matching database/keyring pair before every transition. The
controller migrates supported older state at startup and refuses unsupported
downgrades. A rollback across a schema boundary restores the matching
pre-upgrade database and `keyring.json`; changing only the binary or image tag
is not a data rollback. Historical `v0.1.0-rc.1` uses schema 17 and must not
open schema-19 state.
