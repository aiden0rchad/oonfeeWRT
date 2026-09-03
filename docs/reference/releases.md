# Release notes

The documentation describes the current patch release, **v0.1.4**. Release
artifacts, checksums, container digests, signatures, and attached notes on the
GitHub release are the publication source of truth.

## Current release

- [v0.1.4 release and downloads](https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.4)
- [v0.1.4 notes in the repository](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.4.md)
- [All GitHub releases](https://github.com/aiden0rchad/oonfeeWRT/releases)

v0.1.4 adds explicit per-network IPv6 preserve, prefix-delegation, and disabled
policies behind Preview and Apply. Existing networks default to Router managed
and receive no IPv6 write merely by upgrading. It also condenses repeated IPv6
router-advertisement warnings in the controller database, adds read-only router
clock observation, corrects transitive FDB presentation in wired topologies,
and reduces work on unchanged topology data.

Docker Compose can now publish on one explicitly selected management IP through
`OONFEE_HTTP_BIND`; loopback remains the default. The release also updates
`golang.org/x/crypto` for two reachable SSH denial-of-service fixes. See the
versioned notes for the IPv6 evidence boundary, existing-adoption ACL refresh,
and reporter-validation status.

## Earlier releases

- [v0.1.3 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.3.md)
- [v0.1.2 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.2.md)
- [v0.1.1 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.1.md)
- [v0.1.0 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.0.md)

Before upgrading, read both the release notes and [Upgrade and roll back](../installation/upgrades.md).

### v0.1.3 effective-WAN boundary

v0.1.3 changed how the controller proves the active WAN. It selects the unique
usable lowest-metric IPv4 default installed in the kernel main table and maps
its runtime device to one active netifd logical interface. That corrected
layouts such as a DrayTek modem-management network beside PPPoE, where logical
`wan` uses kernel device `pppoe-wan`.

Missing, malformed, equal-metric ambiguous, ECMP/multipath, or unmappable
evidence remains unavailable instead of being guessed. Custom policy routing,
`mwan3`, per-uplink health, manual selection, and bond-member monitoring remain
out of scope in v0.1.4 as well.

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
or installing it. For the OCI image, pin `v0.1.4` or the immutable digest and
verify the GitHub Actions keyless signature as shown in the [Docker Compose
guide](../installation/docker.md).

The macOS binary is not Developer ID signed or notarized. A checksum mismatch
is never an instruction to bypass the check.

## Version and schema boundaries

The daemon prints its build version with:

```sh
oonfeewrtd -version
```

The current documentation targets database schema 20.

| Transition | Schema/data effect | Router-access effect |
|---|---|---|
| v0.1.1 → v0.1.2 | Schema 19; no migration | No router change for upgrade; compatibility reporting is an Inspect/UI feature |
| v0.1.2 → v0.1.3 | Schema 19; no migration or startup deletion | No ACL refresh or re-adoption; the exact read-only route command has been in the scoped ACL since v0.1.0 |
| v0.1.3 → v0.1.4 | Migrates schema 19 to 20; adds a topology index and normalizes two historical source names, retaining the newest duplicate observation | Ordinary polling needs no access change; existing adoptions need a separately acknowledged ACL refresh only for router-clock status; IPv6 remains Router managed until explicitly changed through Preview/Apply |
| v0.1.4 → v0.1.3 | Restore the matching pre-upgrade schema-19 database, keyring, and passphrase; v0.1.3 cannot open schema 20 | Controller rollback does not revert router configuration applied while v0.1.4 was running |

Preserve the matching database/keyring pair before every transition. The
controller migrates supported older state at startup and refuses unsupported
downgrades. A rollback across a schema boundary restores the matching
pre-upgrade database and `keyring.json`; changing only the binary or image tag
is not a data rollback. Historical `v0.1.0-rc.1` uses schema 17 and must not
open schema-19 or schema-20 state.
