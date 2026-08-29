# oonfeeWRT v0.1.2

v0.1.2 is a patch release focused on hardware-inspection correctness and a
safe way to share compatibility evidence. It manages stock OpenWrt through
existing SSH/ubus interfaces; it is not firmware and installs no
controller-authored executable on a router.

Publication is complete only when the `v0.1.2` tag workflow succeeds. Verify
downloaded archives with `SHA256SUMS`; the published OCI image also carries a
keyless signature, SBOM, and provenance attestation.

## Highlights

- Fixed read-only inspection for single-interface, two-GMAC OpenWrt gateways.
  On a real Cudy M3000 v2 with Motorcomm YT8821, inspection now reports two
  physical radios, LAN `eth1`, WAN `eth0`, and no independent switch ports.
- Stopped counting multiple BSS interfaces as physical radios and prefer an AP
  interface when sampling a radio that also has a station interface.
- Added **Export sanitized compatibility report** after a successful read-only
  inspection. The versioned JSON contains bounded hardware, firmware, radio,
  port, feature-state, and supported-function evidence.
- The compatibility report excludes router and site identity, MAC and network
  addresses, credentials and secrets, network configuration, clients, live
  telemetry, timestamps, runtime radio/PHY and bridge-member identifiers, and
  free-text probe notes. Board-declared LAN/WAN labels remain because they are
  compatibility evidence. The report is downloaded locally; no additional
  router call, persistence, or upload occurs.
- Corrected VLAN omission guidance for a generic direct-interface LAN. The
  renderer no longer labels every such layout as legacy `swconfig`, and still
  leaves existing LAN/VLAN configuration untouched when it cannot safely
  create a tagged attachment.
- Added the comprehensive documentation portal and kept release/build checks
  aligned with the controller's bounded five-minute rollup retention.

## Cudy and Filogic scope

The Cudy result is deliberately narrow. Read-only **Inspect capabilities** was
confirmed by the issue reporter on a Cudy M3000 v2 running OpenWrt 25.12.5,
target `mediatek/filogic`; [issue #19](https://github.com/aiden0rchad/oonfeeWRT/issues/19)
records that evidence.

This does not validate adoption/bootstrap, Apply/rollback/confirm, WLAN and
client operation, tagged VLAN management, polling/resource budgets, topology,
RF scans, speed tests, un-adoption, or other Filogic boards. Tagged VLAN
attachments on this single-interface LAN layout remain unsupported and are
omitted rather than guessed.

## Upgrade and rollback

Back up the matching database/keyring pair before upgrading. v0.1.2 keeps
schema 19, so upgrading from v0.1.1 requires no schema migration and makes no
router change. A clean binary/image rollback to v0.1.1 is schema-compatible;
retain the v0.1.2 data pair before rollback as normal operational practice.

See the bundled `INSTALL.md` (source: `docs/INSTALL.md`) for verified download,
container, backup, restore, and signature commands.

## Security and scope

- Compatibility export is available only after authenticated, CSRF-protected
  read-only inspection and fails closed when evidence exceeds strict bounds.
- The HTTP listener has no native TLS. The default Compose mapping is host
  loopback; use a trusted reverse proxy before remote access.
- Discovery, inspection, compatibility export, diagnostics, backup, and
  controller speed testing do not change routers. Adoption, Apply, RF scans,
  and optional capability installation remain separately acknowledged actions.
- No independent security audit or penetration test has been completed.
