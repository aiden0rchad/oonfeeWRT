---
title: Troubleshooting
description: Symptom-based diagnosis, verification, and recovery for oonfeeWRT v0.1.3.
---

# Troubleshooting

Start with the smallest read-only check that separates controller, network,
credential, capability, and router-state failures. Do not retry a router write
until you know the previous operation's terminal state.

## First five checks

1. Confirm the executable version:

   ```sh
   oonfeewrtd -version
   ```

   Expected for this guide: `v0.1.3`.

2. Check controller liveness using the same listener configuration as the
   running process:

   ```sh
   oonfeewrtd -listen 127.0.0.1:8080 -healthcheck
   ```

3. Read controller logs. For a container:

   ```sh
   docker logs --tail 200 oonfeewrt
   ```

4. Confirm the data directory, keyring, and passphrase file still refer to the
   same controller.
5. If the controller runs but fleet data is wrong, open the relevant device and
   read its freshness, source coverage, and capability gaps before changing it.

## Controller does not start

### `data directory ... must be an absolute path`

Use an absolute path:

```sh
oonfeewrtd -data-dir /absolute/path/to/oonfeewrt-data
```

This guard prevents an unexpected working directory from creating a second
empty controller.

### `listen ... address already in use`

Another process owns the configured address. Stop the duplicate controller or
choose a different listener. Make the same change in the healthcheck, reverse
proxy, and container mapping. Do not start two controllers against the same
data directory.

### `OONFEE_PASSPHRASE is set`

Remove that environment value and put the passphrase in a protected file:

```sh
unset OONFEE_PASSPHRASE
chmod 600 /absolute/path/to/passphrase
export OONFEE_PASSPHRASE_FILE=/absolute/path/to/passphrase
```

### Passphrase file is not accepted

Check that it is a non-empty regular file, readable by the daemon user, and not
group/world-readable:

```sh
ls -l /absolute/path/to/passphrase
chmod 600 /absolute/path/to/passphrase
```

For the supplied container, the file must be readable by UID `65532` unless the
container is explicitly run as another UID/GID.

### Wrong passphrase or keyring error

Stop. Do not delete `keyring.json` or let the controller initialize a new data
directory beside the existing database. A new keyring cannot decrypt existing
credentials.

Restore the exact matching set:

- `oonfeewrt.db`;
- sibling `keyring.json`; and
- the passphrase/passphrase file that opens that keyring.

Validate an isolated copy with [the recovery helper](./cli.md#oonfeewrt-recoverycheck).

## Healthcheck fails but the process appears to run

`-healthcheck` probes the listener passed through `-listen` or
`OONFEE_LISTEN`; it does not discover another process's flags. Use the same
address/port as the running controller. If a reverse proxy is healthy but the
direct check fails, diagnose the controller listener before the proxy.

The check expects HTTP 200 and body exactly `ok` within four seconds. It does
not require the passphrase or open the database, so a successful healthcheck is
only HTTP liveness—not proof that sign-in, storage, or routers work.

## Browser cannot connect or repeatedly returns to sign-in

1. Verify `/healthz` directly from the browser host.
2. Check that the proxy forwards ordinary HTTP and WebSocket upgrades.
3. For TLS termination, preserve `X-Forwarded-Proto: https` so Secure cookies
   are issued correctly.
4. Do not mix access through an IP address and DNS hostname; cookies are scoped
   to a host/domain and path. Keep scheme, host, and port consistent as well,
   because the controller separately enforces same-origin checks on mutations.
5. Remember that sessions are intentionally lost on controller restart and
   expire after 12 idle hours or seven absolute days.

If a password was changed, account disabled/deleted, role changed, or session
revoked, signing out is expected. Ask an Owner to inspect the account/session
state rather than repeatedly attempting credentials.

## Discovery finds no routers

oonfeeWRT discovery does not use ARP tables or mDNS. It plans eligible,
directly attached IPv4 networks, makes bounded TCP/HTTP probes, and identifies
stock rpcd from an unauthenticated `/ubus` object list. A default bridged
container usually exposes only its container interfaces to that planner, so an
empty or skipped LAN scan is expected rather than proof that no router exists.

- Use **Adopt a device → add by address**.
- Ensure the controller process or container network namespace can route to the
  management IP.
- Read the scan plan, skipped networks, and route-failure details; a `/31`,
  `/32`, IPv6, point-to-point/tunnel, link-local, or broader-than-`/22`
  interface is deliberately not swept automatically.
- On Linux only, host networking can expose eligible host LAN interfaces to the
  same TCP scanner if you accept the listener/firewall implications.
- A firewall, nonstandard `uhttpd` port, HTTPS-only endpoint without the HTTPS
  scan option, or inaccessible `/ubus` endpoint can prevent a fingerprint even
  when the host answers.
- On Docker Desktop, use add-by-address.

Discovery is not required for adoption, polling, or Apply.

## Inspect or adoption cannot reach a router

From the controller host, verify:

- the management address is correct;
- SSH is reachable on the configured/default port;
- `uhttpd` exposes the ubus handler;
- the supplied device administrator username/password works;
- a firewall or management VLAN is not blocking the controller; and
- the address does not point to a different device.

Inspect is read-only. Use it to confirm model, firmware, interface, and
capability facts before authorizing adoption. Do not weaken router authentication
or expose SSH publicly to make the check pass.

### SSH host key or device fingerprint changed

Treat an unexpected identity change as a security event or device replacement.
Verify the current key/fingerprint through an independent trusted path such as
local LuCI, physical access, or a known console. Re-adopt only after establishing
why it changed (for example, a deliberate reflash). Never accept the change
blindly.

## Inspect succeeds but compatibility export is unavailable

This can be intentional fail-closed behavior. The compatibility document is a
separate, server-built allowlist. Text is normalized, redacted, and bounded;
the controller omits the report if interface names, function/switch values,
radio/port counts, or encoded output still fall outside strict safety bounds.
Inspect can still display the ordinary result and adds a note explaining that
the sanitized report was unavailable.

1. Confirm both daemon and UI are the same v0.1.2-or-newer release; for this
   guide, both should be v0.1.3.
2. Repeat read-only Inspect once after confirming the target address and
   credentials. It makes a fresh probe, but do not loop it aggressively.
3. Record the controller version, router model/OpenWrt version, the displayed
   inspection limits, and the exact availability note.
4. Report the omission as a sanitizer/compatibility issue. Do not work around
   it by publishing the full Inspect API response, router logs, ubus dumps, or
   SSH command output; those are not the share-safe format.

No stored report can be recovered later because compatibility export is not a
controller job and is not persisted.

## A feature says unavailable, unsupported, partial, or stale

These states mean different things:

- **unsupported/unavailable:** the router, driver, package, or ACL did not
  expose the fact;
- **partial:** some sources answered, but not enough for a complete claim;
- **stale:** prior evidence exists but a current poll did not refresh it;
- **empty:** a source answered successfully with no rows.

Use **Re-probe capabilities** after an intentional firmware/package/ACL change.
Read the exact source reason. Do not install packages manually merely to remove
an unavailable badge; adoption promises not to do that, and optional capability
state needs a recorded plan and rollback.

## Preview is blocked

Read the named gate. Common causes include:

- a device is unreachable;
- capability evidence is missing or stale;
- a foreign/human-managed section conflicts with desired state;
- uncommitted router edits are present;
- the change touches the controller's management path;
- hardware/driver facts do not prove a requested option; or
- the Preview was generated for older desired/router state.

Resolve the condition and generate a fresh Preview. A prior preview/acknowledgement
does not authorize a changed plan.

## Apply failed or its outcome is uncertain

Do not click Apply again immediately.

1. Read the durable operation and per-device receipt.
2. Wait through the displayed OpenWrt rollback window.
3. Test the management address from the controller host.
4. Use independent LuCI/SSH access to inspect pending UCI and named owned
   sections.
5. Determine whether OpenWrt reverted or the controller confirmed.
6. After the device is stable, generate a new Preview.
7. Download diagnostics if evidence remains unclear.

The operation continues independently of the browser request. A reload should
read status, not duplicate the write. See [Safety model](../concepts/safety.md#recovery-after-an-uncertain-apply).

## Device is online but topology shows “Unplaced”

Online status proves controller reachability, not a current physical parent.
Dynamic bridge FDB/neighbor evidence can age out, and BusyBox FDB output may not
identify VLAN provenance. The topology deliberately avoids inventing a link.

Check source coverage and last-observed times. If durable physical adjacency is
worth a router package/service, review the optional LLDP plan. Do not install
`lldpd` outside the controller and then expect its rollback ledger to be
accurate.

## WAN route or WAN throughput is unavailable

Online state does not prove an effective Internet route, and a route does not
prove that a matching durable counter series exists. v0.1.3 requires one
composite observation: the installed main-table IPv4 route and netifd's logical
interfaces must both answer in the same slow topology poll.

Start read-only:

1. Confirm the controller is v0.1.3 and the device is adopted as a Gateway.
2. Allow one topology cycle (normally up to 15 minutes) after startup, adoption,
   or a route change, then read the device's source/degradation reason.
3. From an independently trusted router shell, if appropriate, inspect the two
   sources without changing them:

   ```sh
   /sbin/ip -4 route show table all
   ubus call network.interface dump
   ```

   These outputs contain addresses and network configuration. Do not post them
   publicly without careful redaction.
4. Verify there is one usable lowest-metric IPv4 default in table `main`/`254`
   and that its kernel `dev` maps to exactly one *up* netifd interface with a
   default route. PPPoE commonly maps logical `wan` to `l3_device` `pppoe-wan`.
5. If the route is shown but throughput is not, wait for completed five-minute
   rollups and check whether the exact kernel route interface appears in both
   the device's interface series and route evidence.

Distinct equal-metric main-table defaults, ECMP/multipath (`nexthop`), an
unmappable kernel device, malformed command output, a refused ACL call, or
either half of the batch failing leave the selection unavailable. Source-bound
and non-main policy-table routes are ignored by this selector; a coexisting
unique usable main-table default can still be observed, but it may differ from
the route chosen for policy traffic. `mwan3` and custom policy routing are not
modeled. The collector preserves the last proved network scope on failure, but
stale evidence does not become a current Dashboard WAN path.

Do not change route metrics, PPPoE, firewall, or failover configuration merely
to populate a chart. If the route layout is intentional but outside the modeled
scope, treat WAN selection as unavailable in v0.1.3. Re-probing capabilities
does not force or repair this topology observation.

## Charts are initially empty after startup or adoption

Many charts use completed five-minute rollups. A current poll can prove a
device is online before the first complete durable bucket exists. Counter-based
metrics also need a baseline and a later sample before a delta is meaningful.

Wait for a complete collection window and check source freshness. Do not
interpret missing points as zero traffic, zero utilization, or zero clients.

## RF scan cannot run or has no suggestion

A scan requires the selected radio/source capability, fresh identity, and an
explicit disruption acknowledgement. It may take the serving radio off-channel
and is never scheduled automatically.

A channel suggestion additionally requires:

- a completed scan no older than 24 hours;
- non-stale radio state; and
- a channel plan observed within 15 minutes.

An ACL refresh or capability probe does not run a scan. The published hardware
evidence includes one explicitly authorized C6 5 GHz scan (14 BSS entries,
suggested channel 44); it does not authorize future scans or prove identical
behavior on another radio.

## WRT3200ACM configuration is present but no BSS comes on air

The published Marvell 88W8964/mwlwifi evidence found that WPA3/SAE and PMF can
wedge the radio until a physical cold power cycle. A valid UCI section or
reported-enabled interface is not independent on-air proof.

Inspect OpenWrt logs for missing `AP-ENABLED`/hostapd failures, let any active
rollback finish, then use a known-safe configuration. The demonstrated boundary
was WPA2-only, PMF disabled, FT disabled, and 802.11k/v enabled after cold boot.
Do not manufacture repeated Applies while the radio is wedged.

## Controller-host speed test fails or affects users

The test runs from the controller/container through Cloudflare, not from a
router. It can use about 15 MiB and temporarily saturate the WAN for up to 30
seconds. Only one test may be active.

- Wait for or cancel the active job before starting another.
- Verify the controller host/container has HTTPS and DNS access to the provider.
- Run during a quiet period if saturation affects clients.
- Do not interpret the result as router-local forwarding performance.
- Loaded latency and jitter are unavailable in v0.1.3.

## Diagnostics or backup download expired

Completed diagnostics and backup-export downloads expire after 15 minutes.
Restore upload/preview working state expires after 30 minutes. Start a new job;
do not search temporary controller directories for an expired artifact.

Diagnostics makes no live router call. Portable backup contains sensitive
controller state and should be transferred only over loopback or trusted TLS.

## Recovery helper rejects a backup

`oonfeewrt-recoverycheck` requires a current-schema, isolated database with its
exact sibling keyring. Common failures:

- `OONFEE_PASSPHRASE_FILE` is missing, unreadable, or wrong;
- `keyring.json` is absent or belongs to another snapshot;
- the database or keyring is a symlink;
- a non-empty `-wal` or `-journal` indicates an unsafe/incomplete copy; or
- the database schema is older/newer than the helper supports.

Create a consistent SQLite `.backup` or stop the controller cleanly before
copying. Never “repair” the pair by deleting sidecars from the live controller;
work on an isolated copy and preserve the originals.

## Router writes remain suppressed after restore

This is expected. Read-only monitoring may resume, but restored desired state
is not automatically Applied. An Owner must:

1. inspect the restored devices, credentials, WLANs, networks, policies, and
   operation history;
2. reauthenticate with the current controller account password;
3. understand that automatic 802.11k neighbour maintenance resumes too; and
4. enter exact `RESUME ROUTER WRITES` for the matching restore.

Do not remove the gate merely to clear the banner.

## Un-adoption is blocked

An optional LLDP capability ledger may still own packages/configuration/service
state. Review and complete its rollback first. This prevents un-adoption from
orphaning package residue.

If the router is permanently lost, forced controller removal may be the only
remaining inventory action. Preserve the exact manual cleanup recipe and audit
record; forced removal cannot prove the inaccessible router is clean.

## Upgrade or rollback trouble

v0.1.3 and v0.1.2 both use schema 19. A clean binary/image rollback from
v0.1.3 to v0.1.2 is schema-compatible, but keep the v0.1.3 data pair first.
The v0.1.2 → v0.1.3 upgrade performs no migration or startup deletion, and the
route observation needs no ACL refresh or re-adoption because its exact
read-only command was already in the scoped ACL.

Rollback to historical `v0.1.0-rc.1` is different: that daemon uses schema 17
and must not open schema-19 state. Stop the controller and restore the untouched
pre-upgrade schema-17 database, matching keyring, passphrase file, and old
binary/image together. Migration/rollback does not revert router configuration.

## Escalation bundle

Before reporting a problem, collect:

- exact `oonfeewrtd -version` output;
- installation method and host platform;
- controller logs around the event;
- affected router model and OpenWrt version;
- the UI's capability/source reason and timestamps;
- the durable Apply/scan/speed-test/backup/restore job ID if relevant; and
- a freshly generated redacted diagnostics ZIP.

Do not include the database, keyring, passphrase, router administrator password,
Wi-Fi key, session cookies, or unredacted private logs in a public issue.
