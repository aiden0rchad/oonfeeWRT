---
title: Engineering reference
description: Repository layout, build/test commands, invariants, evidence, and release workflow for oonfeeWRT contributors.
---

# Engineering reference

This page orients contributors to the **v0.1.6**, schema-25 codebase. The repository's
long-form specifications remain authoritative for invariants and measured
hardware behavior.

## Toolchain

| Layer | Current choice |
|---|---|
| Go | Module declares Go 1.26 semantics and pins toolchain `go1.26.6` |
| Database | Pure-Go `modernc.org/sqlite`; cgo is not used |
| HTTP | Standard-library `net/http` pattern mux |
| WebSocket | `github.com/coder/websocket` |
| Secrets | Argon2id and XChaCha20-Poly1305 through `golang.org/x/crypto` |
| UI | React 19, TypeScript 5.9, Vite 7 |
| Charts | uPlot |
| Tests | Go test/race/vet; Vitest; Playwright browser tests |
| Release runtime | Static Go binary; non-root `scratch` container |

Use Go **1.26.6** and Node.js **22** to match release and CI.

## Repository map

```text
cmd/oonfeewrtd/          flags, environment, logging, signals, healthcheck
internal/api/            REST, WebSocket, auth/RBAC, jobs and operation admission
internal/daemon/         process wiring, Inspect and the compatibility-report projection
internal/ubus/           OpenWrt JSON-RPC transport and typed decoding
internal/adoption/       bounded SSH bootstrap/cleanup transport
internal/capability/     per-device probing, defects and capability diffing
internal/model/          validated site, function, zone, policy and observability models
internal/render/         deterministic site intent → per-device UCI operations
internal/applyengine/    preview/apply/verify/confirm state machine
internal/collector/      polling, snapshots and collection scheduling
internal/telemetry/      counter deltas and experience calculations
internal/topology/       route/FDB/neighbor/association/LLDP graph inference
internal/alerts/         sustained rules, incidents and bounded webhook delivery
internal/firmware/       official same-branch catalogue metadata checks
internal/integrations/   bounded read-only AdGuard Home and WireGuard adapters
internal/store/          SQLite schema, migrations, queries, retention and recovery
internal/secrets/        keyring, sealing and password hashing
internal/diagnostics/    bounded, redacted stored-evidence bundle generation
internal/portablebackup/ encrypted portable-backup format
internal/controllerrestore/ staged restore validation/preparation
internal/restoreswap/    crash-safe live-state replacement and suppression
ui/src/components/       shared accessible controls, grids and charts
ui/src/screens/          product workspaces
ui/src/lib/              API client, WebSocket client, columns and tokens
ui/src/demo/             isolated synthetic API/live/PWA adapters and fixtures
ui/public/               controller manifest, original icons and offline guidance
deploy/                  Dockerfile, Compose, ACL template and release contracts
deploy/openwrt-agent/    experimental manually built read-only rpcd helper source
tools/                   probes, mocks, release checks, recovery helper, secret scans
docs/                    user documentation, architecture, evidence and specifications
```

Use the code as the final source of truth for behavior, but preserve the reasons
recorded in [`ARCHITECTURE.md`](../ARCHITECTURE.md),
[`IMPLEMENTATION.md`](../IMPLEMENTATION.md), and
[`DEVICE-BUDGET.md`](../DEVICE-BUDGET.md). When hardware evidence contradicts
an assumption, update the implementation and documentation together.

## Build and test

### Refresh documentation screenshots

The [visual tour](../getting-started/visual-tour.md) and individual guides use
real development-controller screenshots, not UI mockups. Keep them in step
with the code that readers will actually run:

1. Build the UI and controller from the intended revision, start the
   development controller, and sign in through the normal browser flow.
2. Visit the relevant workspace or local tab. Let loading finish and keep
   source, freshness, missing-data, and safety labels visible. Open editors
   without saving; do not run scans, Apply, adoption, account changes, or
   recovery operations just to stage a picture.
3. Capture the app in **dark mode only** at the native browser viewport size;
   focused panels may use a clipped capture instead of the entire viewport.
   Keep enough context to identify the screen and its controls. Save JPEGs in
   `docs/public/screenshots/` as `<screen>-dark.jpg`. Cover every visible MAC
   address with an opaque solid mask, not blur, while preserving measurements,
   source notes, and safety labels. Review the final exported image at full
   size to verify that no MAC address remains readable.
4. Add or update the nearby `<DocScreenshot src="<screen>" alt="..."
   caption="..." />`. Set its `:width` and `:height` to the final JPEG's pixel
   dimensions, including any crop, so space is reserved before loading.
   The component handles the deployed base path, lazy
   loading, and full-size image link; the image stays dark in either docs
   theme. Describe what is actually visible, not an operation that was never
   performed. Match the caption's selected time range to the capture without
   changing the documented default.
5. Update the capture date/revision in the visual tour. Preserve the
   distinction between a development build and published release artifacts.
6. Run `npm --prefix docs run check:screenshots` and
   `npm --prefix docs run build`, then inspect the rendered guides in both
   documentation themes. Check the README's linked dark JPEG previews when
   replacing its images.

Screenshots supplement the written instructions: all essential steps, source
limitations, role restrictions, and warnings must remain available as text.

### Fast local gates

```sh
make check
```

`make check`:

1. installs UI dependencies and builds the embedded UI;
2. scans the complete UI lockfile against the OSV vulnerability database;
3. verifies Go modules and checks `go mod tidy -diff`;
4. runs all normal Go tests;
5. runs `go vet`;
6. runs UI unit tests;
7. builds the isolated synthetic demo without replacing the controller UI;
8. enforces the gzipped controller UI bundle budget; and
9. scans the working tree for repository-specific secret patterns.

It does not run the Go race detector, Playwright suite, `govulncheck`, full
history secret scan, reproducible-build check, or physical-router tests. CI and
release gates add those.

### Individual commands

```sh
npm --prefix ui ci --no-audit
./tools/osv-audit.sh ui/package-lock.json
npm --prefix ui test
npm --prefix ui run build
go test -count=1 ./...
go vet ./...
```

Browser suite:

```sh
npm --prefix ui run test:browser:install
npm --prefix ui run test:browser
```

Race suite:

```sh
go test -race -count=1 ./...
```

Build a host binary:

```sh
make build
./oonfeewrtd -version
```

The UI must be built before the Go binary so `ui/dist` can be embedded. A
`.gitkeep` lets Go compile a development binary without a built app, but such a
binary serves the API with an explicit “no UI embedded” log and is not a release
artifact.

## Local development

Run the controller on loopback:

```sh
make build
./oonfeewrtd \
  -data-dir "$PWD/.run" \
  -listen 127.0.0.1:8080
```

In another terminal, the Vite development server proxies `/api` to
`127.0.0.1:8080`:

```sh
npm --prefix ui run dev
```

Use disposable state for development. Never point tests or dev helpers at a
production data directory unless the helper explicitly documents a read-only
boundary and you have a verified backup.

## Core invariants

### Agent-free by default; explicit optional helper boundary

The controller may call stock ubus/rpcd methods and, after explicit approval,
manage one ACL/login plus separately planned official-feed capabilities.
Historical v0.1.5 installs no controller executable. v0.1.6 adds
the separately packaged `deploy/openwrt-agent` rpcd helper source as a narrow
exception: manual SDK build, manual opt-in installation and read grant,
on-demand allowlisted methods, no daemon or added listener. Ordinary adoption
must not install it or widen access for it automatically. Never add a general
shell/file/command method, firmware-write path, update loop, or package feed
under the guise of a read helper. Matching OpenWrt SDK builds and real-router
helper validation remain outstanding; no prebuilt helper package is included
in the controller release archives.

### Release UI, demo, and schema checks

The [v0.1.6 release summary](./releases.md#development-after-v0-1-5)
distinguishes this release from historical v0.1.5. Current schema is **25**:
schema 24 persists bounded alert state and schema 25 encrypted integration
configuration. Keep forward migrations, schema attestation, portable backup
validation, restore preparation, and old-version refusal consistent. Do not
rename the released version or change historical release notes to imply that
schema 25 shipped in v0.1.5.

Use `npm --prefix ui run build:demo` followed by
`npm --prefix ui run preview:demo` for the separate synthetic preview on port
4180. It must never include the production API, live-channel, or service-worker
registration implementation;
new unsupported methods must fail locally. Use the regular UI test command
from `ui/` to include the no-network adapter tests. Never supply real router
credentials or copy captured API responses into demo fixtures.

Preserve evidence semantics when improving graphics: absent is not zero,
page-level counts are not inventory totals, layout movement is not a topology
mutation, and a generic glyph is not inferred hardware identity. Tests should
cover stale replacement responses, scoped preferences, keyboard interaction,
time-range boundaries, and missing data as well as populated cases.

### Capability evidence is tri-state-plus

Unknown, unavailable, stale, partial, observed-empty, and observed data are not
interchangeable. Decoders retain field presence; UI/API changes must not turn a
failed or absent source into a real zero/false/empty collection.

### Compatibility export is an allowlist, not serialization

`internal/daemon/compatibility_report.go` constructs a versioned DTO explicitly;
never marshal `InspectResult`, the capability registry, or raw probe output as a
shareable report. Keep bounds, sensitive-input matching, address/secret
redaction, interface-name validation, known function/switch enums, and the
64-KiB final limit fail-closed. A report-generation failure may omit the report
without turning a successful read-only Inspect into a router mutation or a
partially sanitized download. The UI may format the DTO but must not add fields
from the ordinary Inspect result or send it back to the server.

### Effective WAN is one composite proof

The installed main-table IPv4 route and `network.interface dump` are sampled in
one topology batch. `internal/topology.ParseIPv4MainDefaultRoute` must select a
unique usable lowest-metric kernel device; `Snapshot.finalizeNetworks` must map
it to exactly one active netifd default-route candidate. Neither half may
publish network scope or an Internet edge alone. Failure retains the last proved
cache and records a gap; it must not make a partial observation look empty.

The topology edge's `ParentPort` is the kernel device, while evidence can also
carry the different logical interface. Dashboard may promote that kernel name
to its WAN metric key only after exact RX/TX series-catalog proof. Device Detail
receives the current route-device candidate directly and may therefore render
an empty chart until samples for that key exist. In the additive Device Detail
contract, omitted `wan_interface` means an older server and retains the rolling
compatibility fallback; JSON `null` from v0.1.3 means no current proof and must
not trigger a client-side guess.

### Foreign UCI is read-only except for explicit management-LAN IPv6 patches

Ordinary render/apply/cleanup operates only on controller-owned sections. The
sole exception is an explicitly selected v0.1.4 management-LAN IPv6 mode, whose
rendered `Patch` operations are option-allowlisted and limited to the exact
existing LAN interface, its single matching DHCP section, and supported
conventional `wan`/`wan6` sections. Patches may not create, claim, rename,
prune, or delete foreign sections. A conflicting or ambiguous foreign target is
a gate. Ownership and cleanup tests are security and data-loss tests, not
formatting tests.

### Management mode is enforced below the UI

Legacy devices migrate to `managed`. At most one device whose canonical
`functions_json` or compatibility role indicates Gateway may also be managed;
schema 23 rebuilds that uniqueness guard so the representations cannot
disagree around it. Monitor-only devices use the distinct read-only
`oonfeewrt-monitor` ACL and remain poll/topology targets, but must be excluded
from render, Preview/Apply, optional LLDP installation/configuration/removal,
wireless-neighbour mutation, and other package/config/remove paths. Existing
LLDP observation remains available. ACL refresh and un-adoption are intentional
lifecycle exceptions.
Capability-proved RF scan is also available to either mode after separate
disruption acknowledgement; it is a transient active observation, not a
persistent configuration mutation.

### Policy-set references resolve or fail closed

`source_set_id` and direct `source_macs` are mutually exclusive. Model, store,
API, master-table, restore, and render paths must reject invalid/empty/dangling
sets; deletion must refuse every enabled or disabled reference. Rendering uses
current canonical members, so a membership edit invalidates an old Preview.
Schema 23 keeps source-relative `client_observations` keyed by `(device_id,
MAC)`, with scope and `last_seen`. Its MAC lookup and the case-insensitive
`clients_mac_nocase` index keep maximum-size policy validation bounded. Every
set member, direct or set-backed MAC Secure draft, and blocked/fixed-address
intent must have a stored `local` observation from the currently adopted
Managed Gateway. Monitor-only observations neither satisfy nor contaminate
that proof, including after un-adoption. Upgrades create no such rows, and
portable restore deliberately clears them because source-controller evidence
cannot authorize destination-controller writes. Until the next successful
managed-Gateway poll, active MAC intent must
then become a Preview error until every referenced client is re-observed
locally. The one-client clear path for existing block/fixed-address intent must
remain available. Pruning removes observation provenance at the normal
client-retention cutoff even when desired intent preserves the merged client
row; retained active intent must continue to block Preview until local Gateway
evidence returns.

### Preview does not authorize a changed plan

Apply authorization is bound to rendered desired state, fleet, capabilities,
and acknowledgements. Full-fleet preflight occurs before the first write.
Durable operation state must survive browser cancellation/reload without
reissuing the mutation.

### OpenWrt rollback is part of correctness

Once Apply is staged, the engine must continue through health/confirm or a
proved rollback outcome. Shutdown must not close the database or exit under an
in-flight critical Apply merely to stop quickly.

### Secrets never become ordinary strings at boundaries

The runtime passphrase is prompted or read from a protected file, never accepted
as an environment value. Device administrator credentials are ephemeral.
Authenticated WLAN/mesh reads expose `has_key`, not secret values. Diagnostic,
backup, event, log, and error paths have explicit redaction/size tests.

### SQLite backups are paired and consistent

Never copy a live WAL main file alone. Recovery requires a consistent database,
matching sibling keyring, and passphrase. Restore validates disposable state
before replacement and activates a durable router-write fence.

## Test layers

| Layer | Primary proof |
|---|---|
| Models/rendering | Table/golden/property tests for validation, deterministic output, and ownership |
| ubus/capability | Mock JSON-RPC, bounded decoders, denial/session behavior, hardware defect fixtures |
| Compatibility report | Explicit DTO shape, sanitizer/redaction, bounds/fail-closed behavior, Cudy inspection preservation, and browser-only download tests |
| Route/topology | Main-table parser, metric/ambiguity/malformed cases, PPPoE logical-to-kernel mapping, composite failure, Internet-edge evidence, WAN-series selection, and rolling API/UI tests |
| Apply engine | State-machine and integration tests for preflight, rollback, confirmation, cancellation, restart receipts |
| Store | Schema attestation, migration, retention, recovery, WAL snapshot, concurrency/integrity tests |
| API/auth | `httptest`, complete route-role matrix, CSRF/session/step-up/race tests |
| UI | Vitest component/screen tests plus Playwright dark/light, desktop/narrow-width and keyboard coverage |
| Packaging | Release-contract, archive, container-smoke, signature/provenance and reproducibility checks |
| Hardware | Operator-authorized integration tests and published fresh-start evidence |

`tools/mock_ubus.py` models staged versus committed UCI, Apply rollback/confirm,
session behavior, and known driver defects. It is a contract fixture, not proof
that an arbitrary physical router behaves the same.

## Hardware tests are opt-in

Integration tests use explicit environment gates such as `OONFEE_TEST_*`; tests
that can write a router require a dedicated opt-in variable. Do not run a broad
`-tags=integration` command against production credentials without reading the
target test first.

The release resource gate is the 60-minute class-C harness documented in
[`DEVICE-BUDGET.md`](../DEVICE-BUDGET.md). Its preferred mode opens a current
controller database/keyring read-only to obtain the scoped credential, while
separate root SSH reads measure whole-device resources and flash evidence. It
must run on an otherwise idle, explicitly selected lab router.

Published physical behavior and accepted gaps live in
[`FRESH-START-VALIDATION.md`](../FRESH-START-VALIDATION.md). Do not promote a
source-only pass to “hardware verified.”

The Cudy M3000 v2 record is reporter-confirmed read-only Inspect evidence, not
an end-to-end adoption record. Issue #20's real PPPoE/management-network route
output is a reproduction input for v0.1.3 regression tests, not a new physical
controller-validation run. Keep those labels distinct in code comments,
release notes, and user documentation.

## UI constraints

- The static output is embedded, so Vite uses relative asset paths.
- The total JS/CSS/HTML bundle ceiling is 1.5 MiB gzipped.
- Dark and light themes are separate validated token sets.
- Warnings, authorization, and Apply safety details remain inline; passive
  explanations may use progressive disclosure.
- Keyboard focus, readable status without color alone, correct table semantics,
  and narrow-width behavior are release gates.
- Virtualized grids must disclose that browser find sees rendered rows only.

See [`UI-SPEC.md`](../UI-SPEC.md) before changing visual tokens, charts, grids,
or Apply interaction.

## CI and release

Pull requests and main-branch pushes run:

- Go module verification, tests, vet, and `govulncheck`;
- the full Go race suite;
- UI unit/browser tests, a fail-closed OSV lockfile scan, production build, and
  bundle budget;
- reproducible release archive and container restore smoke checks;
- multi-platform container build without publishing; and
- tree/history secret scans.

The separate documentation workflow runs when documentation or its workflow
changes. It audits `docs/package-lock.json` and builds VitePress on pull
requests and `main`, then publishes the built site from `main`. Verify that
main-branch job on the intended release commit before tagging. The tag-triggered
release workflow independently installs the pinned documentation dependencies,
repeats the OSV audit, and rebuilds VitePress at the tagged SHA.

The OSV gate is intentionally strict: any known vulnerability match, download
or checksum failure, package-extraction failure, or scanner/API error fails the
job. It replaces npm's unavailable audit service without weakening dependency
screening and checks all severities rather than only high and critical results.

A `v*` tag triggers the release workflow. It requires strict SemVer on `main`,
repeats the complete code, dependency, and documentation-build gates at the
tagged SHA, builds reproducible Linux/macOS amd64/arm64 archives, builds and
publishes the linux/amd64+arm64 OCI image, attaches SBOM/provenance, signs the
immutable digest with GitHub Actions OIDC, verifies public aliases, and finally
publishes the GitHub release.

Use:

```sh
make release-check RELEASE_VERSION=v0.1.6
```

only from the exact intended clean release tree. A local build from another
commit is not the published release even if its version string is changed.

## Documentation and evidence discipline

Build the documentation site before submitting a content change:

```sh
npm --prefix docs ci --no-audit
./tools/osv-audit.sh docs/package-lock.json
npm --prefix docs run build
```

For a local authoring server:

```sh
npm --prefix docs run dev
```

VitePress checks internal links during the production build. Also inspect the
changed pages in both themes and at desktop and narrow widths; a successful
Markdown render does not prove a table, callout, or navigation label remains
usable on mobile.

- User docs describe released behavior, not roadmap intent.
- Name the exact release and evidence status.
- Separate source-tested, simulated, hardware-verified, and unavailable claims.
- Include prerequisites, expected result, verification, and recovery for every
  operation that can change routers or controller recovery state.
- Do not paste live credentials, SSIDs, addresses, tokens, database/keyring
  files, or unredacted diagnostics into examples.
- Run `./tools/secret-scan.sh` before committing and treat any historical secret
  as compromised even after removing it from the current tree.

## Key documents

- [Architecture concept](../concepts/architecture.md)
- [Safety model](../concepts/safety.md)
- [Implementation specification](../IMPLEMENTATION.md)
- [Device resource budget](../DEVICE-BUDGET.md)
- [UI specification](../UI-SPEC.md)
- [Feature parity/evidence](../PARITY-MATRIX.md)
- [Fresh-start hardware validation](../FRESH-START-VALIDATION.md)
- [Risk register](../RISKS.md)
- [Roadmap](../ROADMAP.md)
