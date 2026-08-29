# oonfeeWRT v0.1.3

v0.1.3 is a patch release focused on correctly identifying the Internet uplink
on PPPoE and other OpenWrt systems where more than one logical interface reports
a default route. It manages stock OpenWrt through existing SSH/ubus interfaces;
it is not firmware and installs no controller-authored executable on a router.

Publication is complete only when the `v0.1.3` tag workflow succeeds. Verify
downloaded archives with `SHA256SUMS`; the published OCI image also carries a
keyless signature, SBOM, and provenance attestation.

## Highlights

- The controller now selects the unique usable, lowest-metric IPv4 default from
  the installed main routing table instead of trusting netifd interface order.
  A modem-management network can no longer win merely because it appears as
  another default-route candidate.
- PPPoE logical and runtime identities are mapped correctly. For example,
  logical `wan` can be associated with kernel device `pppoe-wan`, allowing the
  Dashboard and device-detail traffic charts to use the counters that actually
  exist.
- Route and logical-interface evidence are treated as one observation. Missing,
  malformed, ambiguous, or inconsistent evidence is reported as unavailable
  while the last proved cache is preserved; the controller does not guess.
- The Device Detail API exposes the proved WAN series key explicitly. The field
  is additive, and the UI retains a compatibility fallback when connected to an
  older controller that does not provide it.
- Added reporter-shaped DrayTek-management plus PPPoE regression coverage,
  lower-metric route selection, equal-metric ambiguity, direct-interface
  fallback, composite-source failure, and rolling API/UI compatibility tests.

Issue [#20](https://github.com/aiden0rchad/oonfeeWRT/issues/20) supplied the
real OpenWrt route evidence used to reproduce this failure mode.

## Routing scope

v0.1.3 observes one effective main-table IPv4 default route. Ordinary single
DHCP, static, and PPPoE uplinks are supported. Equal-metric distinct defaults,
ECMP/multipath, and kernel devices that cannot be mapped to exactly one active
OpenWrt logical interface are left unavailable instead of being selected by
iteration order.

Custom policy routing, `mwan3`, per-uplink health, manual WAN selection, and
bond-member monitoring are not modeled by this release. A policy-selected path
may differ from the main-table route. Route evidence is collected on the slower
network/topology cycle, so this is not a rapid failover monitor.

## Upgrade and rollback

Back up the matching database/keyring pair before upgrading. v0.1.2 and v0.1.3
both use schema 19, so this upgrade requires no database migration and adds no
startup data deletion. The new route observation uses a read-only command that
has been present in the scoped controller ACL since v0.1.0; adopted routers do
not need an ACL refresh or re-adoption.

A clean binary/image rollback to v0.1.2 is schema-compatible. Retain the v0.1.3
data pair before rollback as normal operational practice.

See the bundled `INSTALL.md` (source: `docs/INSTALL.md`) for verified download,
container, backup, restore, and signature commands.

## Security and scope

- Collecting the installed route table is read-only. This release does not
  change routes, metrics, PPPoE, firewall, failover, or router configuration.
- The HTTP listener has no native TLS. The default Compose mapping is host
  loopback; use a trusted reverse proxy before remote access.
- Discovery, inspection, compatibility export, diagnostics, backup, and
  controller speed testing do not change routers. Adoption, Apply, RF scans,
  and optional capability installation remain separately acknowledged actions.
- No independent security audit or penetration test has been completed.
