# Routine maintenance and health checks

oonfeeWRT is designed as one long-running controller process with bounded storage and adaptive router polling. Routine operation is mainly about preserving recovery material, watching explicit source gaps, and allowing clean shutdown during safety-critical work.

> **Outcome:** You can verify service health, read useful logs, understand automatic retention, adjust polling safely, collect redacted diagnostics, and stop the controller without stranding an Apply.

## Prerequisites and impact

- Read-only access is enough for ordinary health screens.
- Operator access is needed for transient tests such as RF scan and speed test.
- Administrator access is needed for device polling changes and diagnostics.
- Owner access is needed for backup/restore and account custody.

**Router write impact:** Health endpoints, dashboards, logs, diagnostics, and backups are read-only with respect to routers. Polling makes bounded read calls. RF scans, Apply, ACL refresh, optional LLDP, un-adoption, and resuming post-restore 802.11k maintenance have separately displayed write/disruption impact.

## Check controller health

The unauthenticated health endpoint is suitable for local service monitoring:

```sh
curl --fail http://127.0.0.1:8080/healthz
```

Expected output:

```text
ok
```

The executable also has a healthcheck mode that reads the configured listen address and checks `/healthz`:

```sh
oonfeewrtd -healthcheck
```

Docker Compose runs that mode every 30 seconds with a five-second timeout and three retries.

A healthy HTTP endpoint means the process is serving. It does not prove that every router, telemetry source, or optional capability is healthy. Use the Dashboard and device detail views for those layers.

## Read controller logs

### Foreground or service logs

The daemon writes human-readable structured logs to standard error. Read them through the terminal or service manager that starts it.

For Compose:

```sh
OONFEE_VERSION=v0.1.4 docker compose logs --tail=200 oonfeewrt
```

To follow new Compose output:

```sh
OONFEE_VERSION=v0.1.4 docker compose logs --follow oonfeewrt
```

### Retained private log

The controller also mirrors accepted records into:

```text
<data-dir>/controller.jsonl
```

It rotates at 2 MiB and retains three rotated segments. Each segment is forced to mode `0600`; an individual record larger than 64 KiB is replaced by an omission record.

Do not publish the raw log family casually. Use the diagnostics workflow when sharing support evidence because it bounds and redacts retained content.

## Use the application health views

### Dashboard

Review:

- online versus total devices;
- wireless and LAN clients;
- five-minute WAN download/upload/ICMP latency/loss history;
- route and fixed-probe provenance;
- missing samples rather than a false zero;
- recent warnings and errors;
- topology evidence summary.

The WAN probe runs only through the managed Gateway, sends exactly three ICMP packets to `1.1.1.1`, and runs at most once per minute. It is a fixed reachability observation, not DNS/HTTP validation or a claim of ISP uptime.

The displayed Gateway path is a separate, slower observation. v0.1.3 pairs the
installed main-table IPv4 route with OpenWrt logical-interface evidence about
every 15 minutes. It supports ordinary DHCP, static, and PPPoE defaults, but is
not a rapid failover monitor and does not model policy routing, `mwan3`,
manual WAN selection, per-uplink health, or bond members.

### Devices

For each device, review:

- last seen and polling state;
- firmware and capability record;
- current load, memory, clients, and throughput when available;
- degraded/unavailable source explanations;
- management overhead, request rate, observed device CPU cost, bytes sent, and packages recorded as controller-installed;
- current poll interval and scheduled tier.

An unavailable value is not zero. Unknown evidence may come from a denied rpcd call, missing module, failed decode, stale observation, or driver limitation.

### Logs

Use **General** for operational events and **Audit** for authorized controller actions. Event details retain producer identity, provenance, severity, and supplied network fields. Coverage gaps are part of the result; the controller does not claim history for a source it did not observe.

## Understand polling behavior

Shipped defaults:

| State | Interval/limit | Meaning |
|---|---:|---|
| Baseline | 60 seconds | Always-on device health and history. |
| Focused | 10 seconds | While at least one UI viewer focuses that device. |
| Adaptive maximum | 10 minutes | Backoff ceiling for unavailable or struggling devices. |
| Slow response threshold | 1.5 seconds | Evidence used to widen polling. |
| Load threshold | 5.0 | High one-minute load widens polling. |

Polls are staggered across devices, batched where supported, and completely quiesced during that device's Apply/confirm cycle.

Network and topology discovery, including the effective WAN route, uses a
15-minute cadence even while the device detail view is focused. Missing,
malformed, ambiguous, or inconsistent route/interface evidence leaves the
current source unavailable and preserves the last proved network cache; it
does not guess a new uplink.

The per-device poll-interval control can make baseline polling slower, not faster than the controller default. Use it for constrained or remote hardware. After saving, the daemon re-registers the device so the new interval takes effect without restart.

## Automatic retention

The controller holds raw telemetry in memory temporarily and stores completed rollups in SQLite:

| Data | v0.1.4 retention/bound |
|---|---|
| Five-minute average/min/max/count | 14 days |
| Hourly average/min/max/count | 396 days (13 months) |
| OpenWrt log events | 24 hours; 50,000/device and 100,000 total |
| Controller and audit events | newest 100,000 |
| One event's encoded text/detail | 64 KiB aggregate limit |
| Closed topology intervals | 31 days; active intervals remain |
| RF scans | newest terminal run per device/radio; active work preserved |
| Speed tests | newest three terminal attempts, plus any active test |
| Diagnostic downloads | 15 minutes; up to 20 job records |
| Backup downloads | 15 minutes; up to five job records |
| Restore upload/preview state | 30 minutes; up to five uploads and five previews |

Five-minute rows flush only after a bucket closes. Clean shutdown discards an unfinished in-memory bucket instead of writing a partial canonical row. Do not expect second-by-second raw history after restart.

The engineering envelope is:

- final container image at most 40 MB;
- at most 256 MB steady-state RSS at 25 devices;
- at most 2% of one modern CPU core for an idle fleet;
- at most 2 GB disk at the full retention depth;
- UI bundle at most 1.5 MB gzipped.

These are release engineering bounds, not substitutes for monitoring the host's actual free disk and memory.

## Run a controller-host speed test responsibly

The Dashboard speed test:

- runs from the controller process, not a router;
- uses Cloudflare's `https://speed.cloudflare.com/__down` and `__up` endpoints;
- transfers approximately 10 MiB down and 5 MiB up;
- is bounded to 30 seconds;
- may temporarily saturate the normal WAN route;
- exposes the controller host's public IP and test requests to Cloudflare;
- measures idle latency/jitter and throughput, not loaded latency/jitter.

The Run action is the plan-bound acknowledgement. Do not schedule repeated tests; v0.1.4 exposes an explicit operator action, not an automatic test loop.

## Generate safe diagnostics

Owners and administrators can open **Settings → Diagnostics**.

The bundle:

- uses stored controller evidence only;
- makes no router management/API/SSH call;
- includes controller/version/schema/health, stored device model/firmware/capability state, source coverage, bounded events, and a bounded controller log tail;
- pseudonymizes or redacts sensitive identifiers and known secret values;
- excludes credentials, WLAN/mesh keys, private keys, session material, runtime/export passphrases, and raw secret values;
- is bounded to 16 MiB of members plus 1 MiB archive overhead;
- remains downloadable for 15 minutes.

Procedure:

1. Open **Settings → Diagnostics**.
2. Read the **Stored evidence only** disclosure.
3. Select **Generate stored-only bundle**.
4. Wait for completion and download the ZIP before expiry.
5. Inspect its manifest and contents before sharing it.

Redaction reduces risk; it does not make arbitrary distribution safe. Controller metadata and network structure may remain sensitive.

## Shut down safely

The daemon handles SIGINT and SIGTERM. A normal shutdown:

- stops accepting new work;
- waits for admitted requests and controller jobs within bounds;
- lets an Apply that reached OpenWrt's rollback-protected stage continue toward confirm instead of abandoning its timer;
- flushes completed telemetry buckets and discards the incomplete one;
- checkpoints/closes SQLite;
- closes the private log.

For a foreground binary, press `Ctrl-C` once and wait. A second signal is an emergency termination path, not routine shutdown.

For Compose:

```sh
OONFEE_VERSION=v0.1.4 docker compose stop
```

The supplied service grants 150 seconds. Avoid `docker kill` during Apply or restore confirmation.

## Routine operator checklist

Before configuration work:

1. Confirm `/healthz` and controller logs are clean.
2. Confirm the target device is online and source gaps are understood.
3. Confirm no restore-based router-write suppression is unexpectedly active.
4. Take or verify a recent encrypted backup.
5. Preview and read the complete per-device plan.
6. Never reuse acknowledgements from an older preview; a changed plan requires new review.

Periodically:

1. Download a fresh `.oowrtbak` to off-host storage.
2. Keep its export passphrase separately and run a disposable restore preview.
3. Review owner accounts and active sessions.
4. Review device firmware/capability freshness and explicit ACL-refresh notices.
5. On PPPoE or multi-uplink sites, compare the Dashboard and topology uplink
   with the installed main-table route after planned routing changes.
6. Check host disk/RAM and the controller data-volume backup.
7. Read release notes before changing the daemon or image.

## Troubleshooting and recovery

### `/healthz` fails

Check whether the process/container is running, then inspect startup logs. Common causes include passphrase-file permission, wrong runtime passphrase, missing/mismatched keyring, unsupported schema downgrade, data-directory permission, and address/port collision.

### Health works but the browser says unavailable

Test the browser route from the same host, then check reverse-proxy routing and certificate state. If REST works but live updates do not, check WebSocket forwarding for `/api/v1/live`.

### One device is offline

Verify management routing and the device's uhttpd/rpcd service. Review certificate/SSH pin changes separately; do not silently accept a changed identity. Default adaptive backoff may wait up to ten minutes after repeated hard failures; an explicitly configured 15-minute baseline remains 15 minutes unless a safe explicit action retriggers contact.

### Data is missing or stale

Read the displayed source gap. Do not interpret `Unavailable` as zero. A stored capability may need explicit reprobe after firmware changes; ACL widening is an explicit reviewed refresh, never a polling side effect.

For missing WAN path or traffic data, distinguish the one-minute ICMP probe
from the 15-minute route/interface observation. Equal-metric distinct
defaults, ECMP/multipath, policy routing, or a kernel device that cannot map to
one active logical interface are intentional evidence gaps. After correcting
a normal DHCP/static/PPPoE route, wait for the next network/topology cycle.
Upgrading to v0.1.4 does not require re-adoption. Existing scoped access keeps
ordinary polling and management working. Router-clock status remains
unavailable on an older adoption until an Administrator separately reviews
and applies the updated controller-access payload.

### Apply status is unknown after reload

Use the durable Apply-operation recovery/status view. Do not start a second Apply until the first operation's per-device outcome and possible rollback window are understood.

### Disk backup looks smaller or older than expected

Do not trust a live main-file-only SQLite copy. Use `.backup`, a clean stop, or portable `.oowrtbak`, and always include the matching keyring.

### A container was recreated with an empty controller

Stop before adopting anything. Reattach the original volume and passphrase source. A new empty volume is a separate controller, even when the Compose file is unchanged.

## Next steps

- [Back up and restore](backups.md)
- [Upgrade and roll back](../installation/upgrades.md)
- [Review accounts and sessions](accounts.md)
- [Protect the browser path with TLS](../installation/reverse-proxy.md)
