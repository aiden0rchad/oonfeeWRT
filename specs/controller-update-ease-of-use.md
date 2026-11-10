# Spec: make controller update notices easier to use

Builds on the controller release-notice feature already in this tree
(`internal/updates/`, `GET /api/v1/updates`, Settings → Updates, the first-load
dialog). Read those files first; this spec changes them, it does not replace them.

## Goal

An ordinary owner can see at a glance whether they are up to date, force a fresh
check, and copy exactly the commands that apply to their install. People who
cannot act on an update are not interrupted. Done means all five changes below
are implemented, the existing safety behaviour is intact (see Constraints), and
the verification command passes.

### 1. A real "Check now"

- Add `func (c *Checker) Refresh(ctx context.Context, current string) Status` in
  `internal/updates/checker.go`. It behaves like `Check` but ignores the 12-hour
  success TTL. It must still return the cached status without contacting GitHub
  when either:
  - the last attempt failed and `nextCheckAt` (failure backoff / `Retry-After` /
    rate-limit reset) has not passed, or
  - the last attempt succeeded less than `MinManualInterval = 60 * time.Second` ago.
- Share one code path with `Check` (for example an internal
  `check(ctx, current, force bool)`), including the in-flight coalescing, the
  conditional `If-None-Match` request, and the "unknown version never contacts
  GitHub" rule.
- Add `Refresh` to the `UpdateChecker` interface in `internal/api/updates.go` and
  a handler for `POST /api/v1/updates/check`, registered in `protectedRoutes()`
  with `store.RoleAdmin`. Same response shape as `GET /api/v1/updates`; same
  typed `checker_unavailable` response when the checker is nil.
- UI: `api.checkUpdates()` in `ui/src/lib/api.ts` (follow the existing `post`
  helper). In `ControllerUpdates.tsx`, owners and admins get a **Check now**
  button that calls it; viewers get no button. Delete the paragraph explaining
  that refresh only re-reads the cache, and replace it with one line:
  "Checks run automatically about twice a day."
- If the response's `checked_at` did not advance, show
  "Checked a moment ago — try again in a minute." (or, when `status.error` is
  set, the existing error banner) instead of silently doing nothing.
- Add the route to the demo API (`ui/src/demo/api.ts`) following the existing
  pattern for demo mutations; it returns the same `unknown` demo status.

### 2. Show only the instructions that apply

- `cmd/oonfeewrtd/main.go`: add `var installKind = ""` beside `version`, stamped
  with `-X main.installKind=...`. Carry it through `daemon.Config` (new
  `InstallKind` field, same "build metadata, never environment" rule as
  `Version`) to a new `api.Server.ControllerInstallKind`, set next to
  `ControllerVersion` in `internal/daemon/http.go`.
- `updates.Status` gains `InstallKind string` with JSON name `install_kind`.
  The API handlers set it; the checker does not know it. Allowed values:
  `"container"`, `"binary"`, `"unknown"`. Anything else, including empty,
  is normalised to `"unknown"`.
- Stamp it: `deploy/Dockerfile` appends `-X main.installKind=container`;
  `tools/release-build.sh` appends `-X main.installKind=binary` to the
  `oonfeewrtd` build only. Keep the existing `-buildid= -X main.version=$VERSION`
  substring unchanged — `deploy/release_contract_test.go` asserts it. Add a
  contract assertion for each new flag. Leave `Makefile` `build` unstamped.
- UI (`ControllerUpdates.tsx`, `ControllerUpdateNotice.tsx`): `container` shows
  only the Docker Compose steps, `binary` shows only the standalone-binary steps,
  `unknown` shows both as today. The dialog's Compose sentence follows the same
  rule and is omitted for `unknown`.

### 3. Copy buttons

- Add a small `CopyButton` to `ui/src/components/ui.tsx` and put one on each
  command block in `ControllerUpdates.tsx` (the `.env` line and the two-line
  `docker compose` block).
- The controller is commonly served over plain HTTP on a LAN address, where
  `navigator.clipboard` is undefined. Use it when available; otherwise fall back
  to selecting the block's text and `document.execCommand('copy')`; if both fail,
  leave the text selected and show "Press Ctrl/Cmd+C to copy". Never throw.
- Announce success in an `aria-live="polite"` region ("Copied"); the button has
  an accessible name that says what it copies.

### 4. Plain language first, caveats second

State label in `ControllerUpdates.tsx`:

| State | Label |
|---|---|
| `available` | `Update available — <latest_tag>` |
| `current` | `You're up to date — <current_version>` |
| `unknown` | `Can't tell which version this is` |
| `unavailable` or browser error | `Couldn't check for updates` |
| loading | `Checking…` |

Fact labels: "Installed version", "Latest release", "Last checked",
"Next automatic check".

Server messages in `statusFor`:
- available: "A newer oonfeeWRT release is available."
- current: "You're running the latest published stable release."
- current, git-describe build: "No release newer than this build's base version
  has been published."

Move the security-posture and privacy text into one collapsed
`<details>` titled "What this check does and doesn't do", replacing the
"Release-check privacy and scope" card. Keep every sentence of that text; add
the caveat "This does not assess the controller's or the routers' security."
there instead of in the status messages. The upgrade card's intro becomes one
sentence: "Read the release notes, then follow the steps below."

### 5. Stop interrupting people who can't act

- `ControllerUpdateNotice` never opens for `session.role === 'viewer'`. Viewers
  still see the navigation badge and the Updates tab with its existing
  "ask the deployment owner" banner.
- The dialog offers three actions:
  - **Remind me later** — snoozes this target for 7 days.
  - **Skip this version** — the existing permanent per-target dismissal.
  - **How to update** (primary) — existing behaviour: record the permanent
    dismissal, then navigate to Settings → Updates.
- The × button and Escape now mean **Remind me later**, not a permanent dismissal.
- Snooze storage: `localStorage` key
  `<existing dismissal key>:snoozed-until` holding epoch milliseconds, with the
  same in-memory fallback as dismissals when storage is denied. A missing,
  non-numeric, or past value means not snoozed. A newer target is a different
  key, so it prompts immediately.

## Files

Modify:
- `internal/updates/checker.go`, `internal/updates/checker_test.go`
- `internal/api/updates.go`, `internal/api/updates_test.go`, `internal/api/api.go`,
  `internal/api/rbac_test.go`
- `internal/daemon/config.go`, `internal/daemon/http.go`
- `cmd/oonfeewrtd/main.go`
- `deploy/Dockerfile`, `deploy/release_contract_test.go`, `tools/release-build.sh`
- `ui/src/lib/api.ts`, `ui/src/demo/api.ts`, `ui/src/demo/demo.test.ts`
- `ui/src/App.tsx`, `ui/src/App.test.tsx`, `ui/src/Shell.css`
- `ui/src/components/ui.tsx`
- `ui/src/components/ControllerUpdateNotice.tsx` and its test
- `ui/src/screens/ControllerUpdates.tsx` and its test
- `ui/src/screens/Settings.tsx`, `ui/src/screens/SettingsTabs.test.tsx` (prop
  plumbing only)
- `ui/e2e/desktop.spec.ts` (update the existing update-notice scenarios)
- `README.md`, `docs/installation/upgrades.md` — only the paragraphs describing
  this feature, to match the new behaviour (Check now, Remind me later, viewers
  not prompted)

Off-limits: `internal/store/` (no schema change — schema stays 25),
`deploy/docker-compose.yml`, `.github/workflows/`, `docs/releases/`.

## Constraints

- These invariants must keep holding and keep their tests:
  - `unknown`, `unavailable`, and browser-error states are never labelled
    "up to date" and never show target-specific commands.
  - An unrecognised or development version never causes an outbound request,
    including through `Refresh`.
  - Error text shown to users never contains upstream response text.
  - The request to GitHub stays credential-free and identity-free.
  - There is no automatic or one-click upgrade.
- `POST /api/v1/updates/check` must not let any caller exceed one upstream
  request per 60 seconds, and must never bypass a failure backoff.
- Go: standard library only; follow the existing clock and HTTP seams
  (`NewCheckerWithClient`) in tests — no real network, no `time.Sleep`.
- UI: no new dependencies. Use the existing `Button`, `Banner`, `Card`
  components and the CSS variables already used in `Shell.css`. Keep the native
  `<dialog>`, its focus trap, focus restoration, and the "defer behind another
  modal" behaviour. Touch targets stay ≥ 44px under `(pointer: coarse)`.
- Tests assert structure and behaviour; no real hostnames, credentials, or
  lab addresses in fixtures.
- Add tests for: `Refresh` bypassing the success TTL; `Refresh` respecting the
  60-second floor and the failure backoff; `Refresh` not contacting GitHub for an
  unknown version; RBAC on the new route (viewer 403); `install_kind`
  normalisation; each `install_kind` rendering the right instructions; copy with
  and without `navigator.clipboard`; viewer never sees the dialog; snooze
  expiry; Escape snoozes rather than dismisses.

## Verification

```sh
go test -count=1 ./internal/updates/ ./internal/api/ ./internal/daemon/ ./deploy/ ./cmd/... \
  && go vet ./... \
  && npm --prefix ui test \
  && npm --prefix ui run build
```

If Playwright browsers are installed, also run the e2e suite from `ui/` and
report the result; if they are not, say so rather than skipping silently.

## Out of scope

- Server-side (per-account, cross-device) dismissal. It needs a schema
  migration, and the upgrade docs promise schema 25 is unchanged.
- Keeping the last known release after a failed refresh, detaching the upstream
  request from the browser request, redirect handling, and an opt-out setting.
- Any change to how releases are built, signed, or published beyond the two
  `-X main.installKind` flags.
