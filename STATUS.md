# Where this project is

Written 2026-08-13 as a handoff, updated the same day when Phase 0 finished.
Everything below is either committed or measured on real hardware; nothing here
is aspiration.

Repo: <https://github.com/aiden0rchad/oonfeewrt> · License: Apache-2.0

---

## 1. The short version

The design is no longer a design. It was validated against a real
**Linksys WRT3200ACM running OpenWrt 25.12.5**, which corrected several
assumptions, and then **Phase 0 was built in Go** against those findings.
Eleven packages plus a daemon entrypoint, all tested; eight verified against the
device itself rather than only against the mock.

**Phase 0 is complete.** The whole path runs end to end against real hardware: a
device credential sealed under an operator passphrase, stored in SQLite,
unsealed at connect time, accepted by the device's rpcd, and a poll loop writing
what it learns back to the database — with applies that arm a rollback, verify
health, and confirm only if the device is really healthy.

What is next is Phase 1: the read-only fleet view, which is an HTTP API and a UI
over state the controller already collects.

---

## 2. The test device

| | |
|---|---|
| Model | Linksys WRT3200ACM, OpenWrt 25.12.5 r33051 (mvebu/cortexa9, class A) |
| Reached at | `192.168.1.1` over ethernet from the dev Mac at `192.168.1.181` (`en9`) |
| Root access | SSH key auth works; **root has no password set** |
| WAN | up, on the UniFi-routed `10.7.46.0/24` |
| Radios | both enabled, `oonfeewrt-probe-2g` / `oonfeewrt-probe-5g`, WPA2 |

**Our footprint on it right now:** `/usr/share/rpcd/acl.d/oonfeewrt.json`, one
`rpcd` login (`oonfeewrt`, password in the session scratchpad — regenerate if
lost, see §7), two empty scratch configs, and **nlbwmon** (installed to test the
tier-2 path; `apk del nlbwmon` removes it).

Running the hardware tests:

```bash
OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt OONFEE_TEST_PASS=... \
  go test -tags=integration -p 1 ./internal/... -run Integration -timeout 400s
```

`-p 1` is **required**: packages otherwise run in parallel against one device,
and one package's armed rollback makes another's login shared (see §4).

The suite now includes `internal/collector` (a real poll under the scoped
credential, which is where a missing ACL grant shows up) and `internal/daemon`
(the sealed credential opening a real session, and the collector writing
`last_seen` back to SQLite).

---

## 3. What is built

| Package | What it is | Hardware-verified |
|---|---|---|
| `internal/ubus` | JSON-RPC transport, denial channels, batching, TLS pinning | ✅ |
| `internal/applyengine` | APPLY → HEALTH → CONFIRM, three outcomes, PREFLIGHT | ✅ |
| `internal/capability` | Three-state probe + registry, driver quirk list | ✅ |
| `internal/store` | SQLite schema, WAL, ownership bookkeeping | mock only |
| `internal/crypt` | SHA-512 crypt (`$6$`) | ✅ accepted by real rpcd |
| `internal/adoption` | Adopt / un-adopt, the two-credential split | mock only |
| `internal/model` | Site model: networks, WLANs, AP groups | pure |
| `internal/render` | Site model → per-device UCI, deterministic | pure |
| `internal/reconcile` | Read → render → diff → apply → record | mock only |
| `internal/secrets` | argon2id → XChaCha20-Poly1305 credential store | ✅ end to end |
| `internal/collector` | The poll loop: two tiers, batching, backoff, quiesce | ✅ |
| `internal/daemon` | Config, lifecycle, `/healthz`, shutdown, fleet wiring | ✅ |
| `cmd/oonfeewrtd` | The entrypoint | ✅ runs |

Also: `tools/probe.py` (hardware validation), `tools/mock_ubus.py` (the contract
fixture — it reproduces the measured semantics, including the awkward ones),
`deploy/acl/oonfeewrt.json` (the entire device-side footprint).

---

## 4. Measured device behaviour that the code depends on

These were discovered by measurement and several **contradicted the original
design**. Full detail in `docs/IMPLEMENTATION.md` §14; this is the short list
for someone picking the work up.

**The apply path**
- `uci.apply {rollback:true}` works and **reloads services** — an SSID change was
  observed changing on air and reverting on air.
- `uci.apply` returns **status 0 even when the applied config kills the service**
  (an invalid dnsmasq port applied cleanly while DNS died). Status is not health.
- Health must therefore run **before** confirm, while the timer is still armed:
  a failed change then costs nothing, because we simply decline to confirm.
- `uci.confirm` **and** `uci.rollback` are bound to the session that applied.
- `uci.apply` is one **all-or-nothing transaction** across every staged config,
  and is **globally serialised** — a second session's armed apply is refused with
  status 6, which means "already armed", *not* an authorization failure.
- **An armed rollback does not survive an rpcd restart.** The change stays
  applied and unconfirmable. This is why outcomes are three-valued.
- After a rollback, the applying session still reads the value it **failed** to
  set; a fresh session reads the reverted one.
- **While a rollback is armed, `session.login` returns the applying session's
  token to any caller.** So an independent session is unavailable inside the
  window, and destroying "the verification session" destroys the applier's —
  which silently reverts a healthy change. `FreshSession` marks these `Shared()`
  and `Destroy` refuses to act on them.

**ACLs and denials**
- `list read '*'` is **not** a superuser. It expands only over access-groups
  defined on disk.
- `uci` grants are **two-dimensional**: methods under `"ubus"` *and* config names
  under a sibling `"uci"` key. Both must match.
- `file.exec` matches the command **resolved to its absolute path**.
- File paths are **canonicalised** before matching and `*` **crosses `/`** — so a
  `/sys/class/net/*` grant never fires, while `/sys/devices/*` grants a subtree.
- **ubus status 6** = session valid, target refused → permanent, never retry.
  **JSON-RPC −32002** = rpcd refused to proxy → ambiguous (dead session *or*
  ungranted method) → exactly one re-login disambiguates.
- An rpcd restart destroys every session. Adoption avoids one: rpcd re-reads
  both the ACL dir and the login config at session-creation time.

**Telemetry**
- `iwinfo` is ~92% of a focused poll (194 ms vs 15.8 ms without it).
  `hostapd.<iface>` is ~30× cheaper for per-AP status, but **not** a substitute
  for `assoclist`, which alone carries `tx.retries`, `connected_time`,
  `signal_avg`, `noise`, `thr`.
- `hostapd.rate` is **100×** `iwinfo.rate`. Never mix them.
- Three mwlwifi quirks, all *present but wrong*: `rx_time`/`tx_time` never
  advance (so interference and the airtime split are not computable); survey
  `noise` is **unsigned** (161 for −95); per-station `noise` swings **37 dB**
  between reads (so per-sample SNR flails).
- `network.wireless` is **entirely unreachable** through rpcd.
- uhttpd keep-alive is **20 s**; the ubus session idle timer is **300 s** and any
  call refreshes it.
- Zero flash writes under sustained polling. Software flow offload does **not**
  break per-client accounting.
- **The two poll tiers are worth their complexity**, measured through the real
  collector under the scoped credential, best of five, each one batched request:
  **baseline 8 ms for 7 calls, focused 116 ms for 11.** A 14× difference for
  four extra calls is the whole argument for fetching `iwinfo` only while
  somebody is looking.
- **The noise floor is a per-radio capability, and changing source does not
  rescue it.** The documented advice was "`iwinfo.survey` reports noise
  unsigned, so read it from `iwinfo.info`" — right about the encoding, silent
  about trust. Over 20 samples the 2.4 GHz radio swung **42 dB through
  `iwinfo.info` and 46 dB through `iwinfo.survey`**, while the 5 GHz radio on
  the same driver held within 7 dB. Channel busy time does not explain it.
  `Radio.NoiseStable` now gates it per radio, and the detector is asymmetric:
  a disagreement proves the value moves, agreement proves nothing.

---

## 5. What to do next

Phase 0's list is done. `internal/secrets`, `cmd/oonfeewrtd` and
`internal/collector` are built, tested, and verified against the device.

**Phase 1 — the read-only fleet view** (`ROADMAP.md`), in dependency order:

1. **The HTTP API.** `/healthz` is the only endpoint today. Phase 1 needs the
   inventory, per-device detail, client list and events, over the state the
   collector already gathers. Two hooks are already in place and should be used
   rather than reinvented: `Daemon.Focus(deviceID)` is what a device screen
   holds open while it is being viewed, and `Daemon.TrackApply(ctx, deviceID,…)`
   is how any write must be run — it counts the apply for shutdown *and*
   quiesces polling for that device.
2. **Authentication for it.** There is none yet, and `/healthz` is
   unauthenticated on purpose (§11) — nothing else should be.
3. **The telemetry pipeline.** The collector emits `Snapshot`s; the sink
   currently records only `last_seen`, poll state and reachability transitions.
   The in-RAM ring plus 5 m/1 h rollups from `IMPLEMENTATION.md` §3 is the piece
   that turns snapshots into graphs. Note the units traps are already encoded on
   the snapshot types — `Airtime.UtilizationPercent`, `Survey.NoiseDBm`,
   `Survey.BusyPercent` — so derive through those rather than the raw fields.
4. **The UI**, per `UI-SPEC.md`.

**Open items that need hardware I do not have:**
- Class B/C devices. **Class C (MT7621) sets the budget** and every number so far
  comes from the comfortable class — TLS alone doubled poll CPU there.
- Hardware flow offload (mvebu has none), so the README's accounting tradeoff
  remains scoped to hardware offload and untested.
- A second device, for genuine fleet behaviour. The stagger, the per-device
  backoff and the "ten devices at 60 s is one request every 6 s" rule are all
  unit-tested and none of them has met a second real router.

**Known gaps worth closing cheaply:**
- `internal/model` has no tests of its own (it is exercised through `render`).
- `store`, `adoption` and `reconcile` are mock-verified only. An integration
  test for adoption against the real device would be worth having, but it writes
  a real login, so it needs a deliberate decision.
- The collector's sink drops everything except reachability. That is correct for
  Phase 0 and is the first thing Phase 1 replaces.

---

## 6. Working practices that earned their place

Stated because they repeatedly caught real bugs, including bugs I had already
written and believed.

- **A refused check is not a negative answer.** Denied vs absent must stay
  distinct. Conflating them made `probe.py` report "no DSA" and "legacy
  iptables" for a device that has both, and the same class of bug reappeared
  three times afterwards — twice inside code written to prevent it.
- **Presence-probing cannot detect a field that is present and wrong.** Three
  mwlwifi quirks needed *re-reading* to find. Anything that decides a capability
  from one sample is guessing.
- **Verify with the rawest source available.** Reading "on disk" with `uci get`
  (which overlays a pending delta) produced a confident, wrong finding that was
  committed before being caught.
- **A measurement script that cannot distinguish a failed call from an empty
  result will eventually report a failure as data.** That produced a 131-second
  "divergence" that did not exist.
- **Mock-green is not hardware-green.** The mock passed throughout while real
  hardware exposed the shared-session bug that reverted healthy changes.
- **A fix for one defect is not a fix for the next one in the same field.**
  "`iwinfo.survey` reports noise unsigned, so read it from `iwinfo.info`" was
  written up as settled and repeated in four documents. It settles the encoding
  and says nothing about whether the value can be trusted — which, on the 2.4
  GHz radio, it cannot, from either source. The advice was not wrong; it was
  answering a different question than the one a reader would use it for.
- **Say what a check proves, not what it suggests.** The noise-stability
  detector fires on a disagreement and stays silent on agreement, so silence is
  not evidence. On one hardware run the survey pair agreed while the
  `iwinfo.info` pair jumped 45 dB — same radio, same minute. `Present` therefore
  means "not caught misbehaving", and the code, the docs and the field name all
  say so, because a future reader will otherwise round it to "verified".

---

## 7. Practical notes

- Go 1.26.5 installed via Homebrew (`brew uninstall go` to reverse). The module
  requires **go 1.25** because `modernc.org/sqlite` does, above
  `IMPLEMENTATION.md` §1's stated "Go ≥ 1.23" floor.
- `CGO_ENABLED=0` cross-compiles cleanly for `linux/amd64` and `linux/arm64` —
  verified, and the reason decision D3 chose that driver.
- The device credential lives in the session scratchpad. If lost, delete
  `rpcd.oonfeewrt` on the device and re-run adoption, or regenerate a `$6$` hash
  with `internal/crypt` and write it into `/etc/config/rpcd`.
- Running the daemon from a checkout:

  ```bash
  go run ./cmd/oonfeewrtd -data-dir "$PWD/.run" -listen 127.0.0.1:8080
  ```

  It prompts for an operator passphrase on a terminal (twice on first run,
  because a typo there is a keyring nobody can open), or reads one from a mode
  0600 file given by `-passphrase-file` / `OONFEE_PASSPHRASE_FILE`. It refuses a
  passphrase in `OONFEE_PASSPHRASE` — env is readable from `/proc`, inherited by
  children, and printed by `docker inspect`. The data directory is created 0700
  and holds `keyring.json` plus `oonfeewrt.db`.

- Adoption has no CLI yet, so seeding a device by hand means sealing its
  credential with `Keeper.SealCredential(mac, user, pass)` and writing a
  `store.Device` with `AdoptedAt` set. `internal/daemon/integration_test.go`
  does exactly that and is the shortest working example.

- `docs/IMPLEMENTATION.md` §14 is the authoritative record of measured
  behaviour. When code and docs disagree, the measurement wins — and if neither
  matches the device, re-measure before changing either.
