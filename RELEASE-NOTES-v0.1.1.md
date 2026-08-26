# oonfeeWRT v0.1.1

v0.1.1 is a patch release focused on clearer operational evidence, bounded
controller storage, and a denser, more accessible interface. It manages stock
OpenWrt through existing SSH/ubus interfaces; it is not firmware and installs no
controller-authored executable on a router.

Publication is complete only when the `v0.1.1` tag workflow succeeds. Verify
downloaded archives with `SHA256SUMS`; the published OCI image also carries a
keyless signature, SBOM, and provenance attestation.

## Highlights

- Reworked Dashboard WAN health into truthful five-minute activity charts with
  explicit missing samples, route/probe provenance, a table view, and fixed
  polling backoff that no longer mistakes the expected ICMP probe duration for
  router load.
- Added compact topology and warning/error summaries, responsive page layouts,
  keyboard-accessible chart details, stronger contrast, and passive information
  popovers while keeping warnings, errors, authorization, and Apply safety
  details inline.
- Simplified controller-host speed testing: the Run action is the explicit
  plan-bound acknowledgement, exact impact/privacy details remain available in
  a nonmodal popover, and the three newest terminal attempts are retained
  separately from any active test.
- Condensed the repeated OpenWrt IPv6 router-advertisement condition without
  changing its warning severity or losing its raw evidence, bounded stored event
  size, and made event pruning independent of telemetry folding failures.
- Added desktop and narrow-width browser regression gates and clarified install,
  project-scope, AI-assistance, and validation documentation.

## Upgrade and rollback

Back up the matching database/keyring pair before upgrading. v0.1.1 keeps schema
19, so upgrading from v0.1.0 requires no schema migration and makes no router
change. A clean binary/image rollback to v0.1.0 is schema-compatible; retain the
v0.1.1 data pair before rollback as normal operational practice.

On first v0.1.1 startup, completed or failed speed-test rows older than the
newest three are permanently removed. Back up the controller data first if that
history matters.

See `INSTALL.md` for verified download, container, backup, restore, and
signature commands.

## Security and scope

- The HTTP listener has no native TLS. The default Compose mapping is host
  loopback; use a trusted reverse proxy before remote access.
- Discovery, inspection, diagnostics, backup, and controller speed testing do
  not change routers. Adoption, Apply, RF scans, and optional capability
  installation remain separately acknowledged actions.
- Gateway-run speed testing remains deferred. Loaded latency and jitter are
  unavailable for the controller-host method.
- No independent security audit or penetration test has been completed.
