# Where this project is

Written 2026-08-13 as a handoff. Updated when Phase 0 finished, when Phase 1's
read-only fleet view came up, and again on 2026-08-14 after adoption, the
budget harness, the live channel and un-adoption landed. Everything below is
either committed or measured on real hardware; nothing here is aspiration.

Repo: <https://github.com/aiden0rchad/oonfeewrt> · License: Apache-2.0

---

## 1. The short version

The design is no longer a design. It was validated against a real
**Linksys WRT3200ACM running OpenWrt 25.12.5**, which corrected several
assumptions, and then **Phases 0 and 1 were built in Go and TypeScript** against
those findings.

**Phase 0 is complete, including both of ROADMAP's proofs.** Proof 1 (a broken
config reverts on its own and is reported honestly from a second session) was
met earlier. **Proof 2 is now met too**: adopt a device, use it, remove it, and
its config matches a pre-adoption snapshot exactly — 369 UCI lines and 9 ACL
files before, 374 and 10 while adopted, 369 and 9 after, asserted by
`TestIntegrationAdoptUnadoptLeavesNothing` against real hardware.

**Phase 1 is nearly complete.** The whole path runs against real hardware:
adopt a device from the UI, poll it, roll the samples into SQLite, serve them
through an authenticated API, push live updates over a WebSocket, and render it
in a browser — dashboard, devices with charts, client grid, logs. 94 KB of UI
gzipped against a 1.5 MB budget, and the resource budget is measured rather
than asserted.

Fifteen Go packages plus a UI. Everything that touches a device has been
verified against one.

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
| `internal/ubus` | JSON-RPC transport, denial channels, batching, TLS pinning, request accounting | ✅ |
| `internal/applyengine` | APPLY → HEALTH → CONFIRM, three outcomes, PREFLIGHT | ✅ |
| `internal/capability` | Three-state probe + registry, driver quirk list | ✅ |
| `internal/store` | SQLite schema, migrations, rollups, inventory, operators | ✅ |
| `internal/crypt` | SHA-512 crypt (`$6$`) for rpcd | ✅ |
| `internal/adoption` | Adopt / un-adopt, the SSH bootstrap, the two-credential split | ✅ |
| `internal/model` | Site model: networks, WLANs, AP groups | pure |
| `internal/render` | Site model → per-device UCI, deterministic | pure |
| `internal/reconcile` | Read → render → diff → apply → record | mock only |
| `internal/secrets` | argon2id → XChaCha20-Poly1305; operator passwords | ✅ |
| `internal/collector` | Two-tier poll loop, batching, backoff, quiesce, overhead | ✅ |
| `internal/telemetry` | RAM ring → 5m → 1h, counter/ratio arithmetic | ✅ |
| `internal/api` | REST, session auth, CSRF, throttle, adoption, WebSocket | ✅ |
| `internal/daemon` | Lifecycle, shutdown ordering, fleet wiring, static serving | ✅ |
| `deploy` | The embedded ACL — the entire device-side footprint | ✅ |
| `cmd/oonfeewrtd` | The entrypoint | ✅ |
| `ui/` | Vite + React SPA, embedded via `go:embed` | ✅ driven in a browser |

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
- **`iwinfo.survey`'s `busy_time` and `active_time` are COUNTERS with different
  epochs.** Both advance correctly, but their absolute values are not
  comparable, so utilization is Δbusy/Δactive and never the ratio of absolutes.
  Measured: 5 GHz absolute 1354.7 % against delta 1.7 %; 2.4 GHz absolute 25.9 %
  against delta **73.3 %** — the dangerous one, because 25.9 % looks entirely
  reasonable and is wrong by 3×. hostapd's independent BSS-load reading settled
  it. This corrected a claim asserted as verified in three documents.
- **A WebSocket handshake is not protected by the CSRF token.** The same-origin
  policy does not apply to WebSockets: any page anywhere can open one to the
  controller and the browser attaches the session cookie, and the upgrade is a
  GET so no mutating-request check fires. `/api/v1/live` checks Origin itself.
- **The shipped defaults now meet the shipped budget, because the harness
  checked.** Measured through the real collector: **idle 1.00 polls/min (60
  requests/hour), observed 6.00 req/min, zero flash writes**, 0.49% device CPU
  across the run. The first run failed at 1.08 req/min idle and found two real
  defects — interface discovery was a separate unbatched call, and the focused
  default was 8 s against a stated ceiling of one request per 10 s.
- **Root over ubus is not root, so adoption cannot bootstrap over it.** As
  root: `uci.get rpcd`, `uci.set rpcd.*` and `file.write` into
  `/usr/share/rpcd/acl.d/` all return status 6, while `file.read /etc/rc.local`
  returns 0. rpcd's own ACLs bound `/ubus`, and stock OpenWrt grants write
  access to neither the rpcd config nor the ACL directory — deliberately. The
  footprint therefore arrives over **SSH, twice in a device's lifetime**
  (adoption and un-adoption); everything else stays ubus. That build has **no
  `base64`** and **no `sftp-server`**, so content is piped to `cat` over stdin.
- **A stock device with no root password accepts anything** — any password over
  ubus, and the SSH `none` method. Adoption probes for it and warns.
- **The capability probe must run on the CONTROLLER's session, after its ACL
  exists.** Probing first as the operator answers "what can root see", which is
  a different question and a different answer: on a genuinely fresh device it
  recorded survey, hostapd control and per-client accounting as *undetermined*
  on hardware that has all three.
- **The client inventory is cheap.** `luci-rpc.getHostHints` 5.1 ms,
  `getDHCPLeases` 2.9 ms; adding both took the baseline poll from 8 ms to 11 ms
  batched. `luci-rpc.getWirelessDevices` is **128.8 ms** — as expensive as an
  entire focused poll — so it belongs to adoption and must never enter a poll.
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

Phase 0 is done. Phase 1 is done except for the list below.

**Finish Phase 1** (in the order I would do them):

1. **Discovery.** Adoption works by address; nothing scans for candidates. mDNS
   or an ARP sweep of the management subnet. Add-by-address must stay, since it
   is the only thing that works across subnets.
2. **Grid virtualization and column customization** (UI-SPEC §5). The grid
   renders every row — fine at 13 clients, not at the 10k the spec anticipates
   for Logs and Flows. Also the filter rail with live counts, which only the
   Logs screen has.
3. **Client-list scoping.** The grid lists every host the device sees, which on
   a WAN-facing gateway includes the upstream network's neighbours. Telling LAN
   from WAN needs the site model to know what a LAN is, so it is really a
   Phase 3 dependency — but it is visible now and will confuse people.
4. **The remaining Management Overhead fields** (DEVICE-BUDGET §7): attributable
   CPU percent (needs a control measurement to be honest — the device only
   reports total), the list of packages we installed (nothing installs any yet),
   and the control to loosen the poll interval.

**Then Phase 2**, which is where this becomes a controller rather than a nicer
LuCI: the site model → render → apply pipeline is already built and tested
(`internal/model`, `internal/render`, `internal/reconcile`), so Phase 2 is
largely the *screens* for it plus the pending-changes batching. Read
ROADMAP.md Phase 2 and IMPLEMENTATION §5–6 before starting.

**Setup is documented in the README** (`## Getting it running`), and every claim
in it was verified by following it: the build, the passphrase-file path, the
mode-600 refusal and the `OONFEE_PASSPHRASE` refusal. It states the three places
where low friction and security actually pull against each other and which way
each was decided — no default credentials, the passphrase never in the
environment, and a device with no root password warned about rather than
refused. If any of those decisions change, that table is the thing to update.

**Open items that need hardware I do not have:**
- Class B/C devices. **Class C (MT7621) sets the budget** and every number so
  far comes from the comfortable class — TLS alone doubled poll CPU there. The
  budget harness runs anywhere; it has only ever run against class A, so the
  CPU and RAM rows of DEVICE-BUDGET §2 remain unverified where they bind.
- Hardware flow offload (mvebu has none), so the README's accounting tradeoff
  remains scoped to hardware offload and untested.
- A second device, for genuine fleet behaviour. The stagger, the per-device
  backoff and "ten devices at 60 s is one request every 6 s" are unit-tested and
  none of them has met a second real router.
- **A 32-bit interface counter.** The wrap-recovery path is unit-tested but has
  never seen a real wrap: determining the reference device's counter width would
  need 3 GB pushed through it. The code is written to be correct either way.

**Known gaps worth closing cheaply:**
- `internal/model` has no tests of its own (it is exercised through `render`).
- `reconcile` is mock-verified only.
- The UI has no automated tests. It has been driven in a browser against the
  real device, which has now caught six defects no unit test would have — but
  that is a manual step someone has to remember.
- Nothing re-probes capabilities after adoption. A firmware upgrade is detected
  and logged as a warning, and the stale registry is left in place.

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
- **A component that depends on another having run first has a bug waiting.**
  The live channel's subscriber assumed something else had opened the
  connection. When that assumption broke, the symptom was a panel that silently
  showed nothing while the server pushed correctly — indistinguishable from a
  server fault, and it cost an hour of looking in the wrong place. Making the
  subscriber connect for itself removed the coupling and the whole class of
  confusion.
- **Test against a genuinely clean subject, not a convenient one.** The
  capability probe ran in the wrong order for the entire life of the project and
  every test passed, because a leftover ACL file was always already on disk and
  root's wildcard expanded over it. The bug was reachable only on a device that
  had never been adopted — which is the state every real user starts from, and
  the one no test covered until adoption could actually clean up after itself.
- **A guard that cannot fire is worse than no guard.** The 32-bit wrap check
  was tested against a bound so loose it could only reject readings 1.7 seconds
  apart, while the comment beside it claimed it "bites at the focused rate".
  Both the code and the prose read as protection; neither was. When a bound is
  written down, do the arithmetic on the range of inputs that can actually
  reach it.
- **Run it and look at it.** Four defects in Phase 1 survived a green test
  suite and died within minutes of a browser pointing at the real thing:
  firmware never persisted, client IPs collected and dropped, every page load
  301-ing, and a chart axis labelled with years for data from that afternoon.
  Tests check what you thought to assert; opening the page checks what is
  actually there.
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

- Building and running the whole thing:

  ```bash
  npm --prefix ui install && npm --prefix ui run build && ./tools/budget_check.sh
  ```

  Then `go run ./cmd/oonfeewrtd -data-dir "$PWD/.run" -listen 127.0.0.1:8080`
  and open <http://127.0.0.1:8080>. The Go binary embeds `ui/dist`; building
  without it still works and serves an explanation instead of a blank page.
  `npm --prefix ui run dev` proxies /api to a daemon on :8080 for UI work.

- Adoption now works from the UI (the `＋` rail icon) or
  `POST /api/v1/devices/adopt`. It needs the device's admin credential once, for
  SSH — see §4. Re-adopting rotates the controller login and narrows it to
  production scope, which breaks the applyengine hardware tests: they write to a
  scratch config in the ACL's separate `oonfeewrt-probe` group, which adoption
  deliberately does not grant. Re-enable them with:

  ```bash
  ssh root@192.168.1.1 "uci add_list rpcd.oonfeewrt.read=oonfeewrt-probe; uci add_list rpcd.oonfeewrt.write=oonfeewrt-probe; uci commit rpcd"
  ```

- The older path, if you need it: seeding a device by hand means sealing its
  credential with `Keeper.SealCredential(mac, user, pass)` and writing a
  `store.Device` with `AdoptedAt` set. `internal/daemon/integration_test.go`
  does exactly that and is the shortest working example.

- The device is left adopted with the credential in the session scratchpad
  (`oonfeewrt-device-password.txt`). Re-adopting rotates it — the adoption
  integration test prints the new one, and `STATUS.md` §7's grant command has to
  be re-run afterwards for the applyengine hardware tests.

- `docs/IMPLEMENTATION.md` §14 is the authoritative record of measured
  behaviour. When code and docs disagree, the measurement wins — and if neither
  matches the device, re-measure before changing either.
