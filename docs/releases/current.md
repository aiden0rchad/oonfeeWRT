# oonfeeWRT v0.1.6

v0.1.6 refreshes the controller interface and adds clearer ways to investigate
retained observations: historical Statistics, period Reports, sustained Alerts,
official firmware catalogue checks, and optional read-only integrations.
Device inventory, topology, accounts, and mobile navigation are easier to
explore without disguising missing data or widening ordinary router authority.

Prepared September 12, 2026. The completed `v0.1.6` tag workflow and GitHub
release are authoritative for published artifacts. Verify standalone archives
with `SHA256SUMS`; verify the OCI image's keyless signature, SBOM, provenance,
and immutable digest. Source notes alone do not establish publication.

## Highlights

- **Refreshed interface:** a clearer dark/light shell, illustrated device
  Cards/List views, search and status filtering, readable client identities,
  stronger page hierarchy, and a full-width mobile navigation drawer. Icons
  express known functions; they do not invent hardware or client types.
- **Editable topology presentation:** drag or use the keyboard to arrange
  nodes. Layouts are saved in the browser per controller origin, account, and
  Current/History mode. Reset restores automatic placement. Moving a node does
  not create a link, modify evidence, or change a router.
- **Historical Statistics:** six-hour through 30-day WAN, system, exact-interface,
  and supported stable-radio series. The initial view is six hours. Clear trend
  lines, hover details, and expandable coverage information keep missing
  intervals distinct from measured zero and avoid filling gaps with guesses.
- **Reports:** compare adjacent equal-length periods using sample-weighted WAN
  summaries, explicit observed/expected interval counts, and CSV export.
  These are retained observations, not billing totals, continuous uptime
  guarantees, or speed-test capacity measurements.
- **Durable Alerts:** Owner-created device-offline, WAN-latency, and WAN-loss
  rules evaluate existing evidence, with sustained durations, unknown states,
  incident history, and cooldowns. Optional generic HTTPS webhook delivery
  stores its destination encrypted and uses bounded, visible retries.
- **Firmware catalogue checks:** review stored device identity and explicitly
  compare it with OpenWrt's official stable/oldstable maintenance catalogue.
  Only a unique, exact-board, same-branch sysupgrade image is selected.
  Catalogue metadata is not an image-byte verification, package-security
  assessment, or permission to flash.
- **Read-only integrations:** explicitly check an existing AdGuard Home HTTPS
  endpoint for status and aggregate statistics, or read WireGuard interface,
  peer, handshake, and byte-counter evidence through an optional router helper.
  Missing observations remain unavailable rather than zero or healthy.
- **Separate Accounts workspace:** My account, password changes, and sessions
  move to Accounts between Settings and Logs. Owner-only management retains
  the existing role and reauthentication boundaries, with aligned role controls
  and readable permission descriptions.
- **Installable web application:** original application icons, a standalone
  manifest, and static offline guidance. It does not cache private inventory,
  queue changes, or operate a controller offline. Remote-host installation
  requires a browser-supported secure origin.
- **Isolated demo build:** original synthetic devices, clients, history, radio
  plans, and alert examples in a separate read-only build. It replaces the API,
  live channel, and service-worker registration locally, and blocks connection
  requests. It is not a publicly hosted demo or hardware validation.
- **Illustrated documentation:** expanded workflows and dark-mode screenshots
  with visible MAC addresses solid-masked. Unsaved forms, unconfigured services,
  observed results, and synthetic examples are identified explicitly.

## Clearer information without removing safeguards

Adoption combines the scoped-access explanation and explicit consent in one
review. Ordinary adoption still installs only the reviewed scoped rpcd login
and ACL; it does not install a package, executable, daemon, or firmware.

Historical log gaps, limited channel classification, and other routine source
limitations have compact explanations and remedies. Current failures, stale or
missing coverage, and destructive-operation warnings remain visible. A gap
does not establish either downtime or uninterrupted success.

Empty discovery, topology, and telemetry inventories render safely. Late
device responses and retained history remain bound to the selected device,
metric, interface, and client association. Series lookup failures appear as
errors rather than false empty histories. Chart inspection avoids attributing
nearby measurements to a missing interval.

## Alerts and integration authority

All signed-in roles can read alert rules, incidents, firmware inventory, and
redacted integration settings. Owners manage alert rules and delivery.
AdGuard Home connection changes require an Owner and recent reauthentication;
explicit firmware, AdGuard, and WireGuard checks require an Admin or Owner.
Existing session, CSRF, audit, and configuration permissions remain in force.

Generic webhook delivery requires an explicitly configured public HTTPS
receiver. It does not provide a Telegram, Slack, Discord, or Web Push adapter.
Webhook URLs and bearer tokens are encrypted. Private/reserved destinations,
redirects, and unsafe DNS resolutions are rejected. A successful delivery may
be retried after an interrupted response, so receivers should deduplicate by
event ID. No destination is enabled by upgrading.

AdGuard Home is a separate trust boundary: an explicitly configured private-LAN
or public HTTPS origin is allowed, while loopback, link-local, metadata, and
other excluded ranges remain blocked. Passwords are encrypted, never returned
by read APIs, and require explicit replacement when connection identity
changes. Certificate verification is mandatory, using normal TLS validation
or an explicitly supplied exact certificate fingerprint. Saving settings makes
no service request. Checks read only `/control/status` and `/control/stats`;
the controller neither changes DNS protection nor collects query logs.

WireGuard checks read only interface names, public peer keys, handshake times,
and transfer counters. No private/preshared keys, configuration dump, tunnel
configuration, or active connectivity probe is collected or executed. An old
handshake does not prove an idle peer is offline. An absent helper or denied
read ACL is unavailable evidence, not zero peers.

## Optional helper and firmware boundary

The source tree includes experimental `oonfeewrt-agent` version `0.1.1`,
protocol 1, under `deploy/openwrt-agent`. It is an on-demand rpcd helper with
four fixed read-only methods: `status`, `board`, `info`, and `wireguard`.
It adds no listener, daemon, cloud connection, or general shell capability.
Ordinary adoption and monitoring remain agent-free.

The helper requires a manual matching-SDK build, package inspection, installation,
and explicit assignment of its separate read ACL. No prebuilt OpenWrt package
is included in the controller release archives. Host-side contract tests do
not replace SDK builds or router tests: those validations remain outstanding.
There is no automatic helper installation or expansion of the default ACL.

**Controller-managed firmware installation is not implemented.** There is no
download, staging, transfer, `sysupgrade -T`, flash, or post-reboot recovery
engine in this release. A future execution workflow still requires verified
image bytes/authenticity, fresh device compatibility evidence, router backups,
package-preservation decisions, explicit confirmation, and hardware-tested
recovery. UCI Apply rollback does not roll back a firmware flash.

## Upgrade and rollback: schema 23 to 25

Before upgrading from v0.1.5, export and verify a portable backup. For direct
rollback, also retain a stopped, consistent **schema-23 database + matching
keyring + runtime passphrase + v0.1.5 binary/image**, or a verified whole-volume
snapshot of that recovery unit. Prefer a disposable copy when evaluating a
candidate. A portable backup is restored through its supported controller
workflow; it is not an unencrypted database file to unpack over live state.

On first startup, v0.1.6 applies two ordered migrations:

1. **23 → 24:** adds bounded persistent controller alert state.
2. **24 → 25:** adds the bounded, encrypted AdGuard Home connection record.

These migrations do not create alert rules, enable external delivery, configure
services, install helpers, change adoption mode, or write routers. Existing
device, policy, and credential ownership is preserved. Fresh installations use
schema 25. Supported older databases pass through the existing earlier
migrations before reaching it.

v0.1.5 cannot open schema 24 or 25. Returning to v0.1.5 requires restoring the
matching untouched schema-23 recovery unit, not changing only the executable,
image tag, or schema number. A schema-25 portable backup cannot make an older
controller understand newer state. Keep any post-upgrade recovery copy separate.

Portable restore into v0.1.6 pauses external alert delivery and cancels its
queued notifications. Rules, incidents, cooldown history, and the encrypted
destination are retained; pending continuity is reset and recovery is not
fabricated. Review the receiver and explicitly re-enable delivery when ready.
Cancelled historical notifications are not replayed. Existing restored
router-write gates and client-provenance checks still apply.

Follow `docs/installation/upgrades.md` and the bundled `INSTALL.md` for the
complete verified installation, migration, backup, and rollback procedures.

## Validation and remaining limits

The required automated gates cover Go tests, race detection, vet, module and
dependency checks, UI unit/browser tests, production and isolated-demo builds,
bundle limits, screenshot validation, documentation builds, secret scanning,
reproducible archives, and container backup/restore and publication checks.
Schema-25 tests cover direct migration, bounded records, malformed-check
rejection, encrypted recovery validation, and refusal to downgrade. The app
manifest is served with an explicit MIME type for minimal containers.

Local pre-publication checks on September 12 passed 543 UI unit tests, 47
browser tests, production/demo builds, the bundle budget, full Go tests, vet,
module checks, focused race reruns, and the earlier full race suite. An isolated
arm64 container passed backup/restore smoke validation. Documentation checks
covered 35 dark redacted screenshots, 48 placements, eight checker tests, OCR
and visual mask review, and a production build. Full-tree/all-ref Gitleaks
reported no findings. `govulncheck` found no reachable or imported-package
vulnerabilities; one module-level advisory was outside the imported package
set. These preparation results do not replace the exact tagged release gates.

Read-only release preparation on September 12, 2026 exercised the live
controller interface and a Linksys WRT3200ACM catalogue check reporting OpenWrt
25.12.5 as Current in branch. This is one-device metadata evidence, not a
package-security result or firmware-installation test. No alert rule or service
connection was saved for documentation captures; no notification, helper
installation, firmware flash, or new router configuration was performed for
those captures.

The documented hardware limits remain: issue #25's reporter-side multi-router
topology retest; delegated IPv6 and end-to-end client paths; third-AP fan-out;
real mesh/uplink and literal peer-isolation proofs; and the complete
Filogic/class-B and MT7621 resource/compatibility envelope. Existing two-device
lab evidence is not broadened by a visual refresh.

SNMP, service provisioning, Web Push, Telegram-specific delivery, full
localization, VM inventory, application/DPI flow history, multi-WAN management,
native controller TLS, cloud remote access, public demo hosting, and firmware
execution remain outside this release. This is not a claim of complete parity
with another project. No independent security audit or penetration test has
been completed.
