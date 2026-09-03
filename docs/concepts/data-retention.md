---
title: Data and retention
description: What oonfeeWRT stores, for how long, and what must be backed up together.
---

# Data and retention

oonfeeWRT v0.1.4 keeps configuration, evidence, and audit history locally. It
does not require a cloud account or external database.

## Storage locations

The data directory is selected by `-data-dir` or `OONFEE_DATA_DIR`. It must be
an absolute path; the container default is `/data`.

Important contents include:

| Path | Contents | Persistence |
|---|---|---|
| `oonfeewrt.db` | SQLite database: accounts, site intent, device inventory, sealed credential records, rollups, events, audit history, operations | Persistent |
| `keyring.json` | Wrapped random data key used to seal credentials | Persistent and inseparable from the database |
| `diagnostics/` | Temporary generated diagnostics ZIPs | Terminal artifact expires after 15 minutes |
| `backups/` | Temporary portable export jobs and downloads | Terminal artifact expires after 15 minutes |
| `restores/` | Uploaded backup and disposable preview state | Expires after 30 minutes |
| `.oonfeewrt-recovery/` | Encrypted pre-restore safety artifacts and restore recovery records | Persistent, bounded after restore acknowledgement |
| controller log files | Bounded structured controller logs used by diagnostics | Persistent within the controller's log policy; not included in `.oowrtbak` |

All controller data is sensitive. Keep the data directory private and do not
serve it as static content.

## Compatibility reports are not retained

The v0.1.2+ compatibility report is returned only as an optional field of a
successful read-only Inspect response. It is not written to SQLite, a job
directory, diagnostics, or portable backup. Clicking **Export sanitized
compatibility report** serializes that already-sanitized object in the browser;
there is no second router call, controller-side file, or upload.

Once downloaded, `oonfeewrt-compatibility-report.json` is outside oonfeeWRT's
retention and deletion controls. Protect or delete it using the workstation's
normal file policy. It intentionally excludes timestamps, live telemetry,
network configuration, deployment identity, addresses, clients, credentials,
and secrets, but it still describes the router model, firmware, radios, ports,
and capability states. Do not substitute the full Inspect response when a
report is unavailable: that response is not the share-safe format.

## Metric retention

Raw samples are not inserted into SQLite one at a time. They stay in an
in-memory ring until a complete five-minute window is ready, then the completed
rollups are written in one transaction. A shutdown flushes complete buckets and
discards an incomplete bucket rather than presenting partial data as canonical.

The shipped policy is:

| Data | Retention | Detail |
|---|---:|---|
| Raw telemetry samples | Memory only | Lost on restart before the current window completes |
| Five-minute rollups | 14 days | High-resolution recent metrics |
| Hourly rollups | 396 days | The code's 13-month retention value |

Series queries choose resolution from the requested range. Requests beginning
more than 14 days ago, or spanning more than seven days, use hourly data. This
keeps responses bounded and avoids implying that old five-minute points still
exist.

## Event and topology retention

| Data | Shipped bound |
|---|---:|
| OpenWrt `logd` events | 24 hours |
| OpenWrt events per device | Newest 50,000 |
| OpenWrt events across the controller | Newest 100,000 |
| Controller and audit events | Newest 100,000, independent of router-log caps |
| Encoded event text/detail | 64 KiB per row |
| Closed topology intervals | 31 days |
| Current topology intervals | Kept while active; they have no expiry timestamp |

When per-device or global row-count caps truncate router-log evidence, the
controller records a continuity gap rather than making the remaining history
look complete. The ordinary 24-hour age prune does not add that marker.
Since v0.1.1, the exact OpenWrt odhcpd IPv6
router-advertisement/no-default-route warning has been condensed into one
warning condition per router-log producer epoch. Repeats increment its
occurrence count while preserving warning severity and
first/latest source evidence; similar or unrelated warnings remain individual
rows. This bounds controller SQLite rows, not the router's own `logd` output.
Correct the upstream IPv6/prefix-delegation configuration or disable IPv6
service on the affected LAN to stop the message at its source.

Also since v0.1.1, startup condenses exact matching legacy raw rows in one
transaction before the API serves requests. It retains their original
first/latest evidence and receive timing, then removes only the raw duplicates
represented by the condition record. It does not relabel an old record as newly
observed.

v0.1.4 adds the separate, non-destructive current-condition query and guided
card in **Logs → General**. It is independent of event filters and pagination,
names affected routers, totals occurrences, links to **Review IPv6 and Apply**,
and displays only a recent condition backed by a fresh router-log cursor. The
shipped windows are 10 minutes for recent evidence, three minutes for cursor
freshness, and 15 minutes of continuous quiet before historical
classification. The banner clears when evidence is no longer recent; stale or
gapped coverage and unsettled producer changes are **unknown**, not proof of
recovery. Thus a retained compact row can outlive the active card, and removing
a row is neither necessary nor sufficient to fix IPv6 on the router.

Router-clock comparisons are different again: the collector keeps only a
current, successful UTC comparison in memory for the General Logs notice. They
are not a durable clock time series and do not rewrite persisted router event
timestamps. The notice is shown for a fresh absolute offset of at least five
minutes.

### Current topology projection versus retained intervals

v0.1.4 can remove a redundant managed-parent candidate from the **current**
presentation while fresh FDB/LLDP/direct-placement evidence proves that it was
seen only through a multi-hop path. This is a view-time projection, not
retention or data deletion: the raw link interval and its evidence remain in
topology history. If a required source becomes stale, fails, is ambiguous, or
the direct placement closes, the candidate can appear again rather than being
suppressed by old proof.

Clean FDB-only placements are stored as inferred. Missing BusyBox VLAN
provenance is retained as neutral `vlan_available=false` evidence instead of an
edge warning by itself. Unchanged link observations extend the existing
interval instead of closing and recreating equivalent history; closed intervals
still follow the 31-day policy.

### Effective-WAN retention (introduced in v0.1.3)

The collector does not store the raw `ip` route-table output or raw netifd dump
as WAN history. A successful composite observation produces the scoped logical
network state plus a measured Internet topology edge whose port is the kernel
route interface. Current Dashboard and Device Detail use that edge only while
the device is online and both its route source and edge are at most 31 minutes
old.

If either half of the composite read fails, the collector records a source gap
and preserves the last proved network scope instead of replacing it with a
partial guess. Preserved is not the same as current: stale proof does not power
the current WAN selection. Closed route-derived topology intervals follow the
31-day topology policy above. Interface throughput still follows the ordinary
five-minute/hourly metric retention; no distinct per-uplink or failover history
is created.

## Bounded operational history

| Surface | Retention |
|---|---|
| RF scans | Newest terminal result for each stable device/radio identity; pending/running work is preserved |
| Controller-host speed tests | Newest three completed or failed attempts; an active attempt is separate |
| Diagnostics jobs | Up to 20 job records; completed ZIP available for 15 minutes |
| Portable backup exports | Up to five job records; completed download available for 15 minutes |
| Restore uploads/previews | Up to five uploads and five previews; 30-minute working retention |

On the first v0.1.1 start, older completed/failed speed-test rows beyond the
newest three are permanently pruned. Back up v0.1.0 data before upgrading if
that history matters.

v0.1.4 migrates schema 19 to schema 20. The migration adds an index for closed
topology-history lookup and normalizes old development-era topology source
keys; it does not delete user configuration, credentials, secrets, or topology
intervals. The v0.1.3 daemon cannot open schema-20 state, so rollback requires
the matching pre-upgrade schema-19 database, keyring, passphrase, and old
binary/image together.

## Diagnostics content and limits

Diagnostics are generated from stored controller evidence only. Generation
makes no router management call and changes no router.

The bundle can contain controller version/schema/health, stored device
model/firmware/capability facts, coverage state, bounded events, and a redacted
controller-log tail. It excludes controller passphrases, password hashes,
session/CSRF tokens, router credentials, Wi-Fi keys, private keys/certificates,
the raw database/keyring, client notes, and fixed-address assignments.

The generator caps input and output, including 256 devices, 1,024 source rows,
1,000 event rows, a 2 MiB output log tail, 4 MiB per archive member, and 16 MiB
total uncompressed member data. These are safety ceilings, not promised bundle
sizes.

## Portable backups and safety artifacts

An owner can export an encrypted `.oowrtbak` containing a transactionally
consistent database snapshot and portable wrapped key material. It includes
controller accounts/password hashes, settings, desired state, inventory,
telemetry/events, and saved credentials in encrypted form. It excludes active
sessions, the runtime passphrase, router firmware/files, controller logs, and
other backup/diagnostic artifacts.

The export passphrase is separate from the runtime passphrase, is never stored
by the controller, and cannot be recovered. Keep the artifact and its
passphrase in separate protected locations.

Before a confirmed restore replaces live state, the controller writes an
encrypted safety artifact at:

```text
<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak
```

After the applied-restore audit receipt is cleared, retention targets the three
newest recognized safety artifacts. Files referenced by an active restore
marker, receipt, or suppression record are preserved even if that temporarily
exceeds three. Copy any safety artifact needed for long-term retention before
it becomes eligible for pruning.

## What a valid filesystem backup contains

`oonfeewrt.db` and its matching `keyring.json` are one recovery pair. The
passphrase is required to open the keyring but cannot reconstruct a lost
keyring. A keyring from another controller cannot open the sealed credential
records in the database.

Because SQLite uses WAL mode, copying only a live `oonfeewrt.db` can omit
committed data. Use one of these methods:

- SQLite's online `.backup`, followed by a copy of the matching keyring; or
- a clean controller shutdown, then copy the database and keyring together.

For the container, stop it cleanly before copying the persistent volume. Keep
the passphrase file separately.

Verify an isolated pair with the release helper:

```sh
OONFEE_PASSPHRASE_FILE=/absolute/path/to/mode-600-passphrase \
  oonfeewrt-recoverycheck /absolute/path/to/backup/oonfeewrt.db
```

The sibling `keyring.json` must be beside the database. The helper is read-only,
makes no network call, requires current schema, rejects symlinks, and rejects a
non-empty `-wal` or `-journal` sidecar. See [CLI reference](../reference/cli.md).

## Deletion and un-adoption

Removing a device deletes its controller inventory and eventually sweeps metric
series that no longer have a device. Un-adoption is not merely database
deletion: it first attempts the reviewed router cleanup, and optional-capability
rollback can block it. Export diagnostics or evidence before removal if the
history is needed.

The controller does not claim that every deletion is recoverable. Treat
account deletion, device removal, retention pruning, and restore replacement as
material operations and preserve a verified backup first.

## Related pages

- [Architecture](./architecture.md)
- [Safety model](./safety.md)
- [CLI reference](../reference/cli.md)
- [Troubleshooting recovery problems](../reference/troubleshooting.md)
- [Installation, backup, upgrade, and restore](../INSTALL.md)
