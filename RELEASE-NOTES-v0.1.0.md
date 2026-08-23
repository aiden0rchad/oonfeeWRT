# oonfeeWRT v0.1.0

v0.1.0 is the first stable controller release. It manages stock OpenWrt over
existing SSH/ubus interfaces; it is not firmware and installs no
controller-authored executable on a router.

Publication is complete only when the `v0.1.0` GitHub tag workflow succeeds.
Use the checksums, OCI signature, SBOM, and provenance attached to that release;
do not substitute artifacts built from another commit.

## Highlights

- Polished, responsive Dashboard and navigation with accessible project-owned
  SVG icons, WAN health, compact topology, and summary-first notices.
- Explicit controller-host speed tests with a reviewed 15 MiB/30-second plan,
  cancellation, audit history, and no router management calls. Tests use
  Cloudflare's `https://speed.cloudflare.com/__down` and
  `https://speed.cloudflare.com/__up` endpoints; Cloudflare can observe the
  controller host's public IP and test requests.
- Schema-19 local accounts with owner, administrator, operator, and read-only
  roles, password step-up, session inventory, and revocation.
- Bounded, redacted diagnostics ZIPs containing stored controller and device
  evidence, model/firmware metadata, a manifest, and checksums.
- Encrypted portable `.oowrtbak` export plus previewed, plan-bound restore,
  pre-restore safety backup, session revocation, and persistent router-write
  suppression until owner review.
- Deterministic archives for Linux/macOS on amd64/arm64 and non-root scratch
  images for Linux amd64/arm64.

## Upgrade and rollback

Back up the matching database/keyring pair before upgrading. The controller
migrates `v0.1.0-rc.1` schema 17 to schema 19 on first start. A schema-19 data
directory cannot be opened by the older daemon; rollback requires stopping the
controller and restoring the pre-upgrade schema-17 database, matching keyring,
passphrase file, and prior image/binary together. No migration changes routers.

See `INSTALL.md` for verified download, container, backup, restore, and
signature commands.

## Security and scope

- The HTTP listener has no native TLS. The default Compose mapping is host
  loopback; use a trusted reverse proxy before remote access.
- Discovery, inspection, diagnostics, backup, and controller speed testing do
  not change routers. Adoption, Apply, RF scans, and the optional official-feed
  LLDP package workflow remain separate acknowledged actions.
- Optional TOTP MFA and gateway-run speed testing are deferred. Loaded latency
  and jitter are unavailable for the controller test method.
- Hardware validation covers two OpenWrt 25.12.5 devices. Review
  `FRESH-START-VALIDATION.md` for exact evidence and remaining coverage gaps.
