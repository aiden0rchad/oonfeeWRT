---
title: Engineering reference
description: Repository layout, build/test commands, invariants, evidence, and release workflow for oonfeeWRT contributors.
---

# Engineering reference

This page orients contributors to the **v0.1.2** codebase. The repository's
long-form specifications remain authoritative for invariants and measured
hardware behavior.

## Toolchain

| Layer | Current choice |
|---|---|
| Go | Module declares Go 1.25 semantics and pins toolchain `go1.26.6` |
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
internal/ubus/           OpenWrt JSON-RPC transport and typed decoding
internal/adoption/       bounded SSH bootstrap/cleanup transport
internal/capability/     per-device probing, defects and capability diffing
internal/model/          validated site, function, zone, policy and observability models
internal/render/         deterministic site intent → per-device UCI operations
internal/applyengine/    preview/apply/verify/confirm state machine
internal/collector/      polling, snapshots and collection scheduling
internal/telemetry/      counter deltas and experience calculations
internal/topology/       route/FDB/neighbor/association/LLDP graph inference
internal/store/          SQLite schema, migrations, queries, retention and recovery
internal/secrets/        keyring, sealing and password hashing
internal/diagnostics/    bounded, redacted stored-evidence bundle generation
internal/portablebackup/ encrypted portable-backup format
internal/controllerrestore/ staged restore validation/preparation
internal/restoreswap/    crash-safe live-state replacement and suppression
ui/src/components/       shared accessible controls, grids and charts
ui/src/screens/          product workspaces
ui/src/lib/              API client, WebSocket client, columns and tokens
deploy/                  Dockerfile, Compose, ACL template and release contracts
tools/                   probes, mocks, release checks, recovery helper, secret scans
docs/                    user documentation, architecture, evidence and specifications
```

Use the code as the final source of truth for behavior, but preserve the reasons
recorded in [`ARCHITECTURE.md`](../ARCHITECTURE.md),
[`IMPLEMENTATION.md`](../IMPLEMENTATION.md), and
[`DEVICE-BUDGET.md`](../DEVICE-BUDGET.md). When hardware evidence contradicts
an assumption, update the implementation and documentation together.

## Build and test

### Fast local gates

```sh
make check
```

`make check`:

1. installs UI dependencies and builds the embedded UI;
2. verifies Go modules and checks `go mod tidy -diff`;
3. runs all normal Go tests;
4. runs `go vet`;
5. runs UI unit tests;
6. enforces the gzipped UI bundle budget; and
7. scans the working tree for repository-specific secret patterns.

It does not run the Go race detector, Playwright suite, `govulncheck`, full
history secret scan, reproducible-build check, or physical-router tests. CI and
release gates add those.

### Individual commands

```sh
npm --prefix ui ci
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

### No controller code on routers

The controller may call stock ubus/rpcd methods and, after explicit approval,
manage one ACL/login plus separately planned official-feed capabilities. It
must not ship an oonfeeWRT daemon, executable, firmware, cron job, init script,
or package feed to routers.

### Capability evidence is tri-state-plus

Unknown, unavailable, stale, partial, observed-empty, and observed data are not
interchangeable. Decoders retain field presence; UI/API changes must not turn a
failed or absent source into a real zero/false/empty collection.

### Foreign UCI is read-only

Render/apply/cleanup operates only on controller-owned sections. A conflicting
foreign section is a gate. Ownership and cleanup tests are security and data-loss
tests, not formatting tests.

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
- UI unit/browser tests, dependency audit, production build, and bundle budget;
- reproducible release archive and container restore smoke checks;
- multi-platform container build without publishing; and
- tree/history secret scans.

A `v*` tag triggers the release workflow. It requires strict SemVer on `main`,
repeats the complete gates at the tagged SHA, builds reproducible Linux/macOS
amd64/arm64 archives, builds and publishes the linux/amd64+arm64 OCI image,
attaches SBOM/provenance, signs the immutable digest with GitHub Actions OIDC,
verifies public aliases, and finally publishes the GitHub release.

Use:

```sh
make release-check RELEASE_VERSION=v0.1.2
```

only from the exact intended clean release tree. A local build from another
commit is not the published release even if its version string is changed.

## Documentation and evidence discipline

Build the documentation site before submitting a content change:

```sh
npm --prefix docs ci
npm --prefix docs audit --audit-level=high
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
