# Logs and diagnostics

Logs explain controller and network events; diagnostics package a bounded,
redacted subset of stored controller evidence for support. Diagnostics do not
poll or change routers while generating a bundle.

<div class="write-impact"><strong>Router write impact</strong><span>Reading logs and generating/downloading diagnostics are router-read-free operations. A diagnostics bundle is built from stored controller evidence and makes no router management call.</span></div>

## General and Audit logs

Open **Logs** and choose the appropriate view:

- **General** — device health, collection, topology, RF, controller operations,
  and other operational events.
- **Audit** — authenticated administrative and security-relevant actions, such
  as account changes, adoption, Apply, backup/restore, or session operations.

The exact detail panel preserves source provenance and fields that would be too
dense for the table.

## Filter effectively

Use filters/facets before paging. The event store applies the filter before the
limit and uses keyset pagination, so a page is a stable slice of the matching
history rather than an unfiltered batch trimmed in the browser.

A useful incident filter sequence:

1. choose General or Audit;
2. set severity and category facets when available;
3. page backward to the time around the first symptom;
4. use the Device column to locate events for the affected router;
5. open exact details rather than inferring from the short message;
6. note source gaps and timestamps;
7. correlate with Dashboard, Client Observability, or Topology at the same time.

v0.1.3 does not provide a device or free-text search filter on the Logs page.

## Retention boundaries

- OpenWrt-origin logs: 24 hours, bounded to 50,000 per device and 100,000
  globally.
- Controller/audit history: bounded to 100,000 records.

Retention is deliberately bounded. Export a diagnostics bundle or record the
needed evidence before a long investigation exceeds the window. A bundle is a
support snapshot, not a replacement for a dedicated long-term log platform.

## Generate a diagnostics bundle

Users with the required administrative permission can open **Settings →
Diagnostics**.

1. Read the descriptor before generation. It lists included sections, excluded
   secret classes, and size limits.
2. Confirm it reports `router_management_calls=false` and
   `router_changes=false`.
3. Select **Generate**.
4. Watch the job state. Only one active generation is accepted at a time.
5. Download the completed ZIP.
6. Store and share it as private network metadata, even though it is redacted.

The bundle content is bounded to 16 MiB plus 1 MiB of archive overhead. Job
history exposes completed, failed, and cancelled outcomes instead of hiding a
failed collection.

## What diagnostics include

The UI's descriptor is authoritative for the current build. Typical stored
evidence categories can include:

- controller version, schema, platform, uptime, health, migration, and
  integrity metadata;
- adopted device model, target, firmware, kernel, package-manager, and
  capability records;
- bounded events/audit evidence;
- stored topology-source/edge, radio-scan, event-source, and source-gap
  summaries;
- sanitized controller log material;
- a manifest describing sections, limits, and router-call/write assertions.

Generation uses the database/log evidence already on the controller. It does
not contact Fleet or fetch a fresh secret-bearing router configuration.

## What diagnostics exclude

Redaction and exclusion cover secret-bearing classes such as:

- router credentials;
- WLAN and mesh keys;
- private keys and keyring material;
- session cookies/tokens and CSRF material;
- controller runtime/export passphrases;
- account password hashes and authentication secrets.

The generator also sanitizes sensitive key names and patterns in stored text.
No automatic redactor is a reason to publish internal diagnostics publicly.
Review distribution and delete third-party copies when the support need ends.

## Verify a downloaded bundle

1. Confirm the ZIP size is within the descriptor limit.
2. Open the manifest first.
3. Check controller version and generation time.
4. Confirm included/excluded sections match the UI disclosure.
5. Search the extracted copy for known local secret canaries only in a private
   environment if you maintain a formal validation procedure.
6. Share through an access-controlled channel.

Do not edit the bundle and then present it as controller-generated evidence;
keep the original hash/file when chain of custody matters.

## Troubleshooting logs

| Symptom | Explanation | Action |
|---|---|---|
| Expected event is absent | Wrong view/filter, retention expired, operation never crossed its audit boundary, or storage error | Clear filters, check both views, inspect controller logs and durable operation receipt |
| Device log stream stops | Device/source unavailable or log retention/collection gap | Open device capability/source state and correlate last successful poll |
| Many repeated warnings | Persistent source failure or unstable device rather than separate incidents | Group by device/type/time; fix the root source instead of acknowledging each row |
| Timestamps appear surprising | Browser locale versus stored epoch/UTC context | Use exact detail and compare with controller host time |

## Troubleshooting diagnostics

| Symptom | Explanation | Action |
|---|---|---|
| Generate is unavailable | Role does not permit diagnostics or another exclusive operation is active | Sign in with the required admin role and let backup/restore/other conflicting work finish |
| Job fails on size | Stored evidence exceeded a bounded section/archive limit | Read the terminal detail; reduce unrelated retained evidence only through supported maintenance, never delete DB files manually |
| Download is refused | Job expired, state changed, or artifact validation failed | Generate a new bundle and preserve the terminal error for support |
| Bundle lacks a live router value | Diagnostics intentionally use stored evidence only | Reproduce/collect the source through normal controller polling first, then generate a new bundle |
| You suspect failed redaction | A local name/value resembles an uncovered secret pattern | Do not share the file; report the issue privately with a synthetic reproducer rather than the real secret |

## Related guides

- [Dashboard and Internet health](./dashboard.md)
- [Routine maintenance](../operations/maintenance.md)
- [Backup and staged restore](../operations/backups.md)
- [Troubleshooting reference](../reference/troubleshooting.md)
