# v0.1.6 product refresh and release checklist

Release target: **v0.1.6**, database **schema 25**, prepared September 12, 2026.
The user approved this release. This is a delivery checklist, not evidence
that publication has finished. The exact tagged workflow, checksummed archives,
signed OCI image, and GitHub release establish publication; preserve all older
versioned notes and artifacts.

## v0.1.6 scope

- Original visual design: clearer hierarchy, dark/light surfaces, accessible
  controls, rich device cards, exact client facts, and full-width mobile pages.
- Editable, per-account browser-local topology layouts. Moving a node changes
  the drawing, not connection evidence or network configuration.
- Reports: period comparison, sample-weighted observations, explicit coverage,
  and CSV export without invented uptime or traffic totals.
- Durable alerts: sustained conditions, unknown-data handling, recovery,
  cooldowns, owner configuration, and bounded HTTPS webhook delivery.
- Isolated, populated, read-only demo build with synthetic fixtures and no
  controller, router, WebSocket, or notification access.
- Installable web app: manifest/icons and a static offline explanation. No
  cached private inventory or offline mutation queue.
- Firmware inventory and exact-board same-branch official catalogue checks.
- Optional read-only router helper source package, manually installed and
  explicitly granted access; default adoption remains agent-free.
- Read-only WireGuard and AdGuard Home visibility, with explicit checks and
  separate integration authority. No service orchestration.
- Thorough operating guides, README updates, source/release distinctions, and
  refreshed dark-only screenshots with visible MAC addresses solid-masked.

## Boundaries that remain work, not completed parity

The owner approved optional agents and firmware management. That approval is
for implementing the capability, not permission to flash a live router during
development. A safe firmware execution workflow still needs independent
on-device image validation, image authenticity checks, router backup and
package preservation, durable staging/confirmation, recovery procedures, and
post-reboot hardware proof. Catalogue metadata must not enable a Flash button.

Also track separately: SNMP switch monitoring, Web Push and Telegram-specific
delivery, full interface localization, hypervisor/VM inventory, richer
per-client traffic attribution, router service orchestration, and an externally
hosted public demo. Neither a generic webhook nor a local demo is completion of
those integrations. Native SDK package builds and real router helper validation
must be recorded separately from source/fixture tests.

## Release gates

- [x] Review changed frontend/backend code and high-risk trust boundaries.
- [x] Pass UI unit, browser, production-build and bundle-budget checks.
- [x] Pass Go tests, vet, race and migration/backup checks.
- [x] Verify dark and light presentation; capture only dark screenshots.
- [x] Verify live read-only workflows without router writes, service changes,
  firmware installs, speed tests or external notification sends.
- [x] Review feature documentation against the implemented source and observed workflows.
- [x] Recapture affected screens, mask visible MAC addresses, and run the
  screenshot checker and documentation build.
- [x] Select v0.1.6 with explicit release approval.
- [ ] Verify that v0.1.6 remains unused, merge the reviewed source to main, and
  tag only the intended passing commit. Keep immutable old release notes unchanged.
- [ ] Verify exact-SHA release gates, archive checksums, signed multi-platform
  OCI publication, and the GitHub release; then confirm download links resolve.

## Recorded pre-publication evidence — September 12, 2026

- UI: 543 unit tests, 47 browser tests, production/demo builds, and bundle budget passed.
- Backend: full Go tests, vet, module checks, key-package race reruns, and the
  earlier full race suite passed. Schema 23 → 25 migration and recovery checks
  passed; a stale schema expectation in container smoke validation was corrected
  with a drift regression.
- An isolated arm64 v0.1.6 container passed the backup/restore smoke workflow.
  This is not a substitute for the tagged multi-platform release pipeline.
- Documentation: 35 dark, solid-redacted screenshots and 48 placements passed
  dimension validation; OCR and visual mask review, eight checker tests,
  production build, and rendered browser review passed before final release
  wording changes. Rebuild the final tree before publication.
- Full-tree and all-ref Gitleaks checks covered 355 commits without findings.
  `govulncheck` found no reachable or imported-package vulnerabilities; one
  module-level advisory was outside the imported package set.
- The successful Linksys WRT3200ACM catalogue check was read-only and matched
  OpenWrt 25.12.5. No helper install, router flash, rule save, service connection,
  or notification send was performed for the screenshots.

These are local preparation records. The exact tagged GitHub Actions run must
repeat its required gates and publish verified artifacts before release
completion is claimed.
