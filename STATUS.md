# Where this project is

Written 2026-08-13 as a handoff, and rewritten as the work moved. Current
through **2026-08-14**, ending with Phase 2's networks work. Everything below is
either committed or measured on real hardware; nothing here is aspiration.

Repo: <https://github.com/aiden0rchad/oonfeewrt> · License: Apache-2.0

---

## 0. Picking this up

Read §5 first — it opens with what to do next, in order. Then §5g, then §6.

To get running:

```bash
npm --prefix ui install && npm --prefix ui run build && go build -o oonfeewrtd ./cmd/oonfeewrtd
./oonfeewrtd -data-dir "$PWD/.run" -listen 127.0.0.1:8080
```

It prompts for an operator passphrase (twice on first run) and serves the UI at
that address. §7 has the unattended variant and everything else operational.

To run the tests:

```bash
go test ./internal/...
```

The hardware suite needs the device and a credential — §7 explains the rotation
dance, which is the one genuinely fiddly part of this repo:

```bash
OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt OONFEE_TEST_PASS=...   go test -tags=integration ./internal/... -timeout 25m
```

**State of the test device right now:** adopted, polling, `/etc/config` byte
identical to its pre-session md5s, `vlan_filtering` back to 0, no oonfeeWRT
sections left in `network`/`dhcp`/`firewall`. The `oonfeewrt-probe` scratch ACL
scope is granted, so the applyengine hardware tests run as-is.

**One habit worth inheriting:** before any experiment that writes to the device's
network config, arm a restore on the device itself first (§6, "arm the undo
before the experiment"). It saved this session three times.

---

## 1. The short version

The design is no longer a design. It was validated against a real
**Linksys WRT3200ACM running OpenWrt 25.12.5**, which corrected several
assumptions, and then **Phases 0, 1 and most of 2 were built in Go and
TypeScript** against those findings.

**Phase 0 is complete, including both of ROADMAP's proofs.** Proof 1 (a broken
config reverts on its own and is reported honestly from a second session) and
Proof 2 (adopt, use, remove, and the config matches a pre-adoption snapshot
byte for byte — 369 UCI lines and 9 ACL files before, 374 and 10 while adopted,
369 and 9 after) are both asserted against real hardware.

**Phase 1 is complete.** Adopt a device from the UI — found by a network scan or
by address — poll it inside a measured budget, roll the samples into SQLite,
serve them through an authenticated API, push live updates over a WebSocket, and
render it in a browser: dashboard, devices with charts, a virtualized client
grid, logs paged and faceted in SQL. ~105 KB of UI gzipped against a 1.5 MB
budget.

**Phase 2 is largely complete and its ROADMAP proof is met.** One SSID edited
once lands on both bands of an AP with an identical derived mobility domain, a
hand-edited section elsewhere on the device is untouched, and the whole thing is
previewed per device before anything is written. Networks (VLAN, DHCP, firewall
zone) render too, within a limit that hardware imposed — §5g is the single most
important thing in this file.

Seventeen Go packages plus a UI. Everything that touches a device has been
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
| `internal/model` | Site model: networks, WLANs, AP groups, per-device overrides | pure |
| `internal/render` | Site model → per-device UCI (wireless + network/dhcp/firewall), deterministic | pure |
| `internal/reconcile` | Read → render → diff → apply → record | ✅ |
| `internal/discovery` | Unauthenticated subnet sweep for OpenWrt candidates | ✅ |
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

**Phases 0 and 1 are complete. Phase 2 is complete except for two items that
need something I do not have.** Everything below is the honest remaining list,
most-worth-doing first.

### Do these next

1. **Look at the client grid and the re-probe panel in a browser.** Both are new
   (§5i, §5j) and verified from unit tests up through the real device, but
   nobody has *looked* at either. Every UI defect this project has found was
   found by looking — see the standing gap below.
2. **Column reorder.** Show/hide and persistence are done; drag-to-reorder is
   the remaining half of UI-SPEC §5's "Customize Columns". Lowest value here.
3. **A capability the renderer gates on going Absent should surface where the
   config is.** Re-probe now detects it and logs a warning, but the preview
   screen does not say "this WLAN stopped rendering because that device lost
   the feature". The detection is done; the connection to the site model is not.

### Blocked, and by what

- **`usteer` / `dawn` configuration and state readout.** Neither is installed on
  the reference device; both are in the official feeds. This sits behind the
  package-installation flow ARCHITECTURE §6 step 3 describes and nothing has
  built. Writing config for an absent package would be untestable, so it was not
  written.
- **A second and third AP.** The WLAN fan-out is proven across two bands of one
  device. ROADMAP's Phase 2 sentence says three APs. Nothing in the pipeline is
  per-device — render is driven by group membership and the mobility domain is
  derived rather than agreed — so this needs hardware, not code.
- **Class B and C devices.** Class C (MT7621) *sets* the budget and every number
  so far comes from class A. The budget harness runs anywhere; it has only ever
  run against the comfortable class.

### Before starting anything, read these

They are the parts of this file that will save the most time, in order:

- **§5g — the limit networks ran into.** A confirmed, "healthy" change that
  severed the network, and why the controller now refuses to enable VLAN
  filtering at all. If you touch `internal/render/network.go`, read this first.
- **§6 — working practices.** Every entry is there because it caught a real bug,
  usually one already written and believed.
- **§4 — measured device behaviour.** When code and docs disagree, the
  measurement wins; when neither matches the device, re-measure before changing
  either.

### How each piece landed

Chronological, 2026-08-14. Each is a section below with the findings, which are
the useful part — the code is in git either way.

| | |
|---|---|
| §5a | Discovery — the specified probe would have minted a root session on every passwordless device |
| §5b | The table system — six defects that 13,000 rows exposed, including a header that had never been sticky |
| §5c | Client scoping — it did not need the site model, only the device's |
| §5d | Management Overhead — attributable CPU cannot be sampled live, and latency is not load |
| §5e | Phase 2's first contact with hardware — three defects a mock could not reach |
| §5f | Per-device overrides — and the four things they deliberately cannot touch |
| §5g | Networks — **read this one** |
| §5h | The fleet client total — two screens answering one question two ways |
| §5i | Client paging — and the one rail that is not a column |
| §5j | Re-probing — and the difference between losing a feature and losing sight of it |

### 5a. What discovery corrected

Both found by checking the spec against the device instead of implementing it,
and both are recorded in IMPLEMENTATION §14 and fixed in ARCHITECTURE §6.

- **The specified probe was unsafe.** ARCHITECTURE §6 said to fingerprint a
  device by a `session.login` that fails, "without logging in". On a device with
  no root password that login **succeeds** — status 0, a session token, an ACL
  set with `uci` write and `file` exec, for the password
  `definitely-not-the-password-9f3a`. The specified sweep would have minted a
  root session on every passwordless host in the subnet, on every scan. The
  probe used instead is `list` on the null session: no credential, no session,
  no failed-login record, and a much stronger fingerprint because it returns the
  whole object graph. A test asserts the probe makes exactly one request and
  that it is a `list`.
- **Nothing identifying is readable pre-auth.** §6 expected a pending device to
  show model, MAC and firmware "from `system.board` / `system.info` pre-auth
  where possible". Never possible: stock rpcd answers `system.board` on the null
  session with `-32002 Access denied`. The object list does carry the device's
  *shape* — radios with a BSS up (count PHYs, not BSSes), a wan interface, a
  DHCP server — so the UI shows that and says the model is unknown until you
  sign in.

mDNS was deliberately **not** built. ARCHITECTURE already said not to depend on
it because stock OpenWrt advertises nothing useful, and the subnet sweep finds
everything it would without needing anything installed on the device.

Measured: 508 addresses across two /24s in 4.8 s at 128 concurrent probes.
Sweep time is `(addresses / workers) x DialTimeout` and essentially nothing
else — a live host answers in under 5 ms, a dead one costs the full timeout.
The scan refuses anything wider than a /22, skips tunnel interfaces and IPv6,
is on demand only with no background timer, and **reports everything it
declined to look at** — a controller that silently skips the operator's subnet
reports "no devices found", which reads as a fact about their network rather
than about itself. Its one request against an already-managed device is
attributed to that device's Management Overhead readout
(`Collector.NoteExternalRequest`), because "negligible, therefore uncounted" is
how a readout stops being trustworthy.

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

### 5b. What the table system corrected

Every one of these was found by running the grid against 13,106 seeded events —
the row count UI-SPEC quotes from the UniFi screenshots. None of them is
reachable at the 13 rows the screens had before.

- **The filter counts were the lie the spec warns about.** `Logs.tsx` carried a
  comment reading "Filter counts come from the whole result set, never from the
  visible page — a count computed from what happens to be loaded is a lie". It
  counted the array it was handed, which was the newest 300 of 13,106 rows. The
  comment asserted precisely the property it did not have. Counts now come from
  SQL `GROUP BY` over the whole table, each facet computed with the *other*
  filters applied but not its own — so "info 8,819" stays clickable while
  `severity=error` is selected, and the category rail re-scopes to the 2,116
  errors and sums to exactly that.
- **The sticky header was never sticky.** `position: sticky` resolves against
  the nearest scrolling ancestor, and `Card` sets `overflow: hidden` for its
  rounded corners — which made Card that ancestor. The header was pinned to the
  top of a box that does not scroll, so it slid away with the rows. Invisible
  for as long as no grid had enough rows to scroll. The grid now owns its
  scroll container.
- **`height: 33` on a table cell is a minimum, not a height.** Rows measured
  33.84px. Virtualization computes row N's position as `N x height`, so that
  0.84px compounded to 840px by row 1000 — a full screen of drift. Fixed by
  pinning the line box *and* measuring a real row, because a font that renders
  differently would silently reintroduce it. Verified after the fix: at
  scrollTop 16530 of 33060 the window shows row 500 of 1000 exactly, and the
  last row at the bottom is exactly the last row.
- **A windowed grid breaks find-in-page, so it says so.** ⌘F only searches
  rendered rows. The grid prints "1,000 rows, drawn as you scroll — ⌘F searches
  only what is on screen" whenever it is windowed, because a search that comes
  up empty is otherwise indistinguishable from the value being absent.
  Virtualization only engages above 150 rows for the same reason: below that the
  DOM cost is irrelevant and full-text search is worth more.
- **A selected filter with no matches vanished from the rail.** The client
  list defaults to `online`, none of its 14 clients were online, so the option
  dropped out of the count query — leaving an empty grid, "0 of 14", and
  nothing highlighted to explain why. The rail now always renders the selected
  option, at zero if that is the truth.
- **`Force` on un-adopt was dead code.** It is documented as removing a device
  "even if the device could not be reached at all — for hardware that is gone
  for good", and the check sat *after* the early return for
  `ErrOperatorRequired`. An unreachable device always takes that path, so the
  flag could never fire in the only case it exists for; the caller got a 409
  asking for the credential of a router that no longer exists. Found by trying
  it on a device whose credential had gone stale. Fixed, tested, and confirmed
  against real hardware — and a forced removal now logs the residue at WARN,
  because deleting the inventory row deletes the only record of what is still
  on that device.

### 5c. Client scoping needed the device's model, not ours

This item was in the backlog with a stated reason: "telling LAN from WAN needs
the site model to know what a LAN is, so it is really a Phase 3 dependency".
That was wrong, and wrong in a way worth writing down — the site model is *our*
description of a network, and the question does not need ours. It needs the
device's, which netifd already publishes and which one call returns.

`network.interface dump` gives every logical interface, its IPv4 subnets, and
its routes. A host is a client of this network when its address falls in a
subnet of an interface that is *not* carrying the default route, and a
neighbour on the uplink when it falls in one that is. Upstream is decided by
the routing table rather than by an interface being named `wan`, because the
name is a convention and a device bridged onto an existing network can have the
default route on the interface called `lan` — tested both ways round.

What it found on the reference device, which is why the item mattered: of 16
known hosts, **7 were upstream neighbours** on the UniFi network behind the WAN
port and only **3 were actual clients** (a laptop, a phone, a watch). Four have
no observed IPv4 at all and are therefore `unknown` — not `local`, because a
host that has not been shown to be on this network must not be counted as one.
The grid went from listing 14 things to listing 3, with the other 11 one click
away in the rail and labelled.

Cost: the call joins the existing batch on the same 15-minute cadence as the
radio list and the board identity, so it adds no requests. Budget harness after
the change: **1.00 polls/min idle, 6.00 req/min observed, zero flash writes** —
identical, with 118 more bytes per poll. The timestamp is stamped where the
call is *built*, not where its answer is decoded, so a device whose ACL refuses
`network.interface` does not re-ask on every poll forever.

Two rules the storage had to respect. A determination is never overwritten by a
non-determination — the subnets are re-read every fifteen minutes and carried
forward in between, so a poll that cannot classify would otherwise flicker
clients out of the default view for reasons no operator could see. And a row
written before the column existed reads as `unknown`, never `local`: defaulting
it would assert something never measured.

### 5d. What the CPU measurement found

DEVICE-BUDGET §7 asked for "CPU percent attributable to oonfeeWRT" and the
backlog note said it "needs a control measurement to be honest". It did, and
the measurement's first result was that **a live sample can never work**: a
baseline poll costs ~5 ms of device CPU once a minute — 0.009% of one core —
against a device whose own idle CPU is 0.38–0.43%. The quantity is about fifty
times below the floor it would be measured against and far below that floor's
minute-to-minute jitter. Sampling it live would report noise with a decimal
point on it.

So it is derived from a control experiment (CPU over a window with nothing
polling vs a window with a known number of polls), and the UI says so — the
tooltip carries the entire basis rather than a reassuring word.

| | class A reference device |
|---|---|
| control, nothing polling | 0.38–0.43% busy |
| baseline poll, 8 invocations | 5.33 ms of device CPU |
| focused poll, 12 invocations | 6.65 ms of device CPU |
| at the shipped baseline (1/60 s) | 0.0089% of one core |
| at the shipped focused (6/60 s) | 0.067% of one core |

Linearity was checked rather than assumed: 4.56 ms/poll at 6,049 polls/min and
4.38 ms/poll at 372 polls/min, within 4%.

**The finding worth carrying forward:** DEVICE-BUDGET §4 measures iwinfo as
~92% of a focused poll, but a focused poll costs only **1.25×** a baseline one
in CPU. That 92% is latency — `iwinfo.survey` and `iwinfo.assoclist` block on
the wireless driver rather than burning cycles. Wall time and CPU load are
different quantities and the docs had been using the first to reason about the
second.

The figure is reported only for classes it was measured on. Class C gets no
number and a sentence saying why, for the same reason everything else here is
three-state.

The interval control only ever loosens, and the clamp lives in the collector
rather than in request validation — the budget is a promise, the harness
measures the default, and a knob that could raise the rate would put a device
outside the budget where no test would look. Verified on hardware: an override
of 5 s stores as 5 and polls at 60.

### 5e. Phase 2's first contact with hardware

The render → apply pipeline was built and unit-tested in Phase 0 and recorded
here as "mock-verified only". Wiring it to a real device found three defects in
the first hour, all of them invisible to a mock, and then met the proof.

- **`uci.get` does not return only strings.** `ReadExisting` decoded into
  `map[string]map[string]string`. Every UCI *option* is a string, but the
  section metadata is not — `.anonymous` is a bool and `.index` a number — so
  the decoder failed the entire read and **every device reported as
  unplannable**. This is the exact shape of the mock-green problem §6 already
  names: the mock returned strings throughout.
- **A new BSS is not up the instant `uci.apply` returns.** The health check read
  hostapd once, immediately, found the SSID absent, and let the device revert.
  Correct by its own logic, wrong in fact — measured, a BSS appears about a
  second after the reload. It now polls for up to 20 s inside the 90 s window,
  and names what the radios *are* carrying when it fails. Worth saying plainly:
  the revert was flawless. `/etc/config/wireless` came back byte-identical, zero
  of our sections, the operator's own section untouched. The safety mechanism
  did exactly its job on a false alarm, which is the failure mode you want.
- **`Doc.Plan` never compared before writing.** It emitted a set for every
  existing section, so a converged device reported "2 changes pending" forever
  and `Empty()` could never be true — a no-op apply would still stage, apply and
  confirm, arming a rollback for nothing. Fixed to skip sections whose managed
  values already match, comparing only the keys we write.

**The proof, on hardware.** One WLAN on two bands, 802.11r/k/v:

| | |
|---|---|
| sections from one WLAN | 2 — one per radio |
| mobility domain on each | `e8ee`, identical, derived not coordinated |
| passphrase changed **once**, landed on | both bands, no per-device work |
| mobility domain after that change | `e8ee`, unchanged |
| hand-edited foreign section | untouched through apply, re-apply and prune |
| prune after deleting the WLAN | our sections gone, the human's kept |
| preview once converged | 0 changes |

"Three APs" is unmet for want of hardware, and that is the only part that is.
Nothing in the pipeline is per-device — render is driven by group membership and
the mobility domain is derived rather than agreed — so a second AP needs no new
mechanism, only a second AP.

### 5f. What per-device overrides deliberately cannot do

The `device_overrides` table had been in the schema since the beginning with
nothing reading it. It now works, and the design decision worth defending is the
short list of what may be overridden:

| overridable | not overridable |
|---|---|
| whether a WLAN is published on this AP | SSID |
| whether it beacons its name here | passphrase |
| whether clients here are isolated | security mode, PMF |
| how many clients may associate here | 802.11r/k/v and the mobility domain |

The right-hand column is the product. A controller exists to keep exactly those
settings identical across every AP, because they are miserable to maintain by
hand and they fail *confusingly* when they drift — a client roaming between two
APs that disagree about the key does not fail cleanly, it fails intermittently,
and the resulting support question is "why does WiFi drop when I walk down the
hall".

So they are absent from the vocabulary rather than present with a warning. An
escape hatch that can break the one guarantee the system offers is not an escape
hatch; it is a slow leak. The API refuses an unknown key by name and says why.

The left-hand column all vary legitimately per AP — a guest network in the lobby
and not the server room is a real requirement — and none of them can
desynchronise a client's view of a network it is already associated with.

Two implementation points that needed care. Overrides are applied to a **copy**
of the WLAN, or the second device rendered inherits the first device's
deviations. And malformed values fail closed: anything but `1`/`true` reads as
false, so a corrupt row cannot quietly switch something on.

Verified on hardware: a forbidden key refused with its reason; `disabled` on one
device rendered nothing there and reported both an omission and a deviation;
`hidden` applied to both radios while `encryption`, the key and the mobility
domain stayed identical across them.

Every deviation is listed in three places — the settings screen, the per-device
preview row, and a site-level summary — because the risk of overrides is never
any single one. It is a fleet that drifts apart device by device until nobody
can say what is actually deployed.

### 5g. The limit that networks ran into

IMPLEMENTATION §5's worked example 2 shows a network rendering as a
bridge-VLAN, an interface, a DHCP server and a firewall zone. All of it is now
built. The worked example is also **incomplete in a way that takes the LAN
down**, and it took three outages of the reference device to pin down why.

**Adding any `bridge-vlan` switches the bridge to VLAN filtering**, and a stock
`br-lan` is not ready for that. Measured:

| | |
|---|---|
| `vlan_filtering` | 0 → 1 |
| `br-lan` | UP, still holding `192.168.1.1/24` |
| `ip neigh show dev br-lan` | **empty — not one neighbour** |
| the apply engine's verdict | `applied — health passed and confirm landed` |
| actual reachability | gone, until a pre-armed restore fired |

Read that table twice. The health check passed because it asks whether the
`lan` interface is up — and it was. The confirm landed. **A confirmed,
"healthy", network-severing change, with no error anywhere in the chain.**

Connectivity survives only if the operator's own `lan` interface moves from
`br-lan` to `br-lan.1`. Verified: with that one edit, filtering on, `br-lan.1`
held the address and this machine stayed `REACHABLE` in the device's neighbour
table. But that section is the operator's, and rewriting the interface we reach
a device through — on a device we might then be unable to reach — is exactly
what ARCHITECTURE §0 forbids.

So the controller **refuses**, and names the one-time change. Once an operator
has made it, VLANs are managed normally: verified end to end, then pruned,
leaving `/etc/config/{network,firewall,dhcp}` byte-identical to their pre-test
md5s.

Two other things fell out of the same sequence.

**A UCI list is not a string with spaces in it.** `uci.set` accepts
`option ports 'lan1:u* lan2:u*'` where UCI wants `list ports`, stores it, and
netifd ignores it — no error at any layer. `Section` and `Op` now carry `Lists`
separately.

**Two guards for one concern is defence in depth; two definitions of it is a
bug.** The apply engine already had a management-path gate covering `network`;
the daemon grew its own covering `network` and `firewall`. They would have
drifted — an operator warned about a change the engine then allowed, or worse
the reverse. There is now one exported definition, `applyengine.TouchesManagementPath`,
and it covers both configs: a zone whose input policy is REJECT blocks the
controller as effectively as a broken interface, and the zone we render for a
new network defaults to exactly that.

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
- ~~`internal/model` has no tests of its own.~~ Closed 2026-08-14: the override
  vocabulary has its own suite (`override_test.go`), and the rest is exercised
  through `render` and `store`'s site-model round-trips.
- ~~`reconcile` is mock-verified only.~~ Closed 2026-08-14: it now runs against
  the real device through the Phase 2 apply flow, which is how the `uci.get`
  decode bug was found (§5e).
- **The UI has no automated tests — the single highest-value gap in the
  project's testing.** It has been driven in a browser against the real device,
  which has caught **fifteen** defects no unit test would have
  — three from the discovery screen and six more from the table system (§5b),
  including a sticky header that had never once been sticky and a
  virtualization drift of 840px. That is a manual step someone has to remember,
  and it is now the single highest-value gap in the project's testing.
- Nothing re-probes capabilities after adoption. A firmware upgrade is detected
  and logged as a warning, and the stale registry is left in place. This one has
  grown teeth now that the renderer gates network rendering on the probed port
  map — it is item 2 in the do-next list above.

### 5h. One question, two screens, two answers

The client grid was scoped in §5c and the dashboard was not, so the same network
was described as **14 devices** on one screen and **3** on the other, both
captioned as this network's. That is worse than either number being wrong on its
own: whichever a person reads first is the one they stop trusting.

The fix worth noting is not the scoping — that was already done and measured —
it is that the two counts came from two different mechanisms. The dashboard
loaded up to 5000 client rows to call `len()` on them; the grid tallied whatever
page the browser had received. Both are correct only while the page is the whole
table, and neither survives the paging that is now item 1. They are one query
now, `store.ClientCounts`, counted in SQL and shared by both callers.

Two things fell out of doing it that way rather than patching the number:

- **The headline says what it excludes.** "3" with "7 upstream, 4 unplaced"
  under it. Without that line the count is simply smaller than the previous
  build's, and nothing distinguishes a correct rescoping from lost devices.
- **Every scope is always present in the result, zero-filled.** "0 local, 7
  upstream" renders as a rail a person can click; a missing key renders as no
  rail at all, which reads as "this build does not do scoping".

Verified on hardware through the daemon integration test, which seeds the
existing credential rather than adopting — so the check costs zero device
writes. Real device, real client mix: `3 on this network (3 active), 7 upstream,
4 unplaced` against a grid of `14 row(s), scopes map[local:3 unknown:4
upstream:7]`. The test now asserts the two agree, so they cannot drift apart
again silently.

### 5i. Client paging, and the one rail that is not a column

The client list now pages and facets in SQL, the way the event log does. The
mechanical half followed §5b's rules directly. The interesting half was
**Connection**, which is the only rail whose value is not a column: a client is
"wireless" because recent station telemetry carries its MAC, which had been
computed in Go over the fetched rows. That cannot survive paging, so it is an
SQL expression now — a correlated `EXISTS` against the station series.

Three things that came out of it:

- **The derivation exists twice, deliberately, and is tested for agreement.**
  The facet and filter are in SQL; the per-row `connection` field stays in Go
  because it also carries signal and retry. Two definitions of "wireless" is
  precisely how a grid lists a row its own rail did not count, so the wireless
  kind list is one variable passed into both, and both an API test and the
  hardware test assert the counts match the rows.
- **It needed an index nothing else needed.** `series` is uniquely indexed on
  `(device_id, kind, key)` and this query does not know the device, so it could
  not use the leftmost column and scanned `series` once per client row. Migration
  6 adds `series(kind, key)`; the plan now shows a covering index search. The
  test asserts the index exists, because without it nothing fails — it only gets
  slow, and slow is not something a test notices.
- **Measured at 13,000 clients**, seeded locally: 30 ms for a page, 68 ms for
  the wireless filter (which runs the `EXISTS` for both the rows and the
  facets), 28 ms at offset 8500. Verified on the device too: 14 of 14, and the
  facets identical between the full list and a one-row page.

`/clients` no longer returns a `scopes` map — the counts are in `facets` beside
the other two rails, and the response carries `total`, `limit` and `offset`. The
screen fetches its own page now, so `App.tsx` no longer pulls the whole client
inventory every 30 seconds for a screen that is usually not open.

**Also fixed here:** `.run/` was not in `.gitignore`, and `.run/` is the path
§0 tells a reader to run the controller with. Following the documented command
left `keyring.json` untracked in a public repo, one `git add -A` from being
published. `data/` was ignored; the name in the docs was not.

### 5j. Re-probing, and the distinction it exists to protect

Capabilities were probed once, at adoption, and never again. A firmware change
was detected, logged as a warning, and then nothing happened — the stale record
stayed, describing a build that was no longer installed. Now the same detection
triggers a probe, and `POST /devices/{id}/reprobe` does it on demand for the
cases no automatic trigger can see: a package installed, an ACL widened.

**The valuable output is the diff, not the new record.** And the diff had one
job to get right, which is the same job the three-state model has:

> A check that stopped being *possible* is not a capability that stopped
> *existing*.

`Present -> Absent` is a device that lost something. `Present -> NotObservable`
is a narrowed ACL, a removed binary, an ungranted method — on hardware that is
very likely unchanged. They are indistinguishable in the raw states.
`tools/probe.py` collapsed the two and reported "no DSA" for a device with a DSA
switch; a diff that collapses them reports the same lie *as an event, with a
timestamp*, which is worse because it looks like news. So every transition is
classified by what it licenses a reader to conclude — `capability.Effect` — and
that classification, not the raw states, is what the log, the API and the UI
render. Visibility changes log at info and colour muted; only real ones warn.

Three decisions worth not re-litigating:

- **A probe is a burst, so it is not on the poll path.** It runs on a firmware
  change or an operator's request, quiesces the device's poller while it runs
  (the rule an apply follows, for the same reason), and is gated per device.
  Automatic probes have a 10-minute floor; operator-initiated ones have none —
  someone pressing a button has a reason, and refusing them because a
  background probe ran two minutes ago makes the button look broken.
- **A failed probe leaves the old record alone.** It learned nothing. Clearing
  it would make the device unplannable — `deviceCaps` refuses an empty
  record — for a transient network fault. The failure is logged as an event that
  names the consequence, so a misdescribed device is never silent.
- **Sampled and volatile fields are not diffed.** Channel (ACS moves it),
  frequency, and per-radio noise stability (decided by sampling twice, so it can
  differ between two probes of an unchanged radio). Diffing those means every
  probe reports churn, and churn arriving right after an upgrade reads as the
  upgrade's doing.

Verified on the device, read-only, using the existing credential: first probe
`class A Linksys WRT3200ACM`, 8 changes, **0 actionable** — a device does not
gain a radio by being looked at for the first time. Second probe of the same
unchanged device: **no changes at all**, which is the property that makes the
whole thing usable.

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
- **A health check can only fail on what it looks at.** The VLAN change passed
  health, landed its confirm, and severed the network. The check asked "is the
  lan interface up" and the interface was up — address intact, state UP, and
  zero neighbours. Liveness of an interface is not connectivity through it, and
  the gap between those two is exactly where a confirmed change can still be
  catastrophic. When a health check gates something irreversible, ask what
  passing it actually proves.
- **Arm the undo before the experiment, not after.** Three times a change took
  the device off the network mid-command, and three times a pre-armed
  `sleep N; restore` running locally on the device brought it back. A recovery
  path that depends on the connection you are about to break is not a recovery
  path. This is also why the apply engine's rollback lives on the device rather
  than in the controller.
- **A mock that is easier to write than the real thing is testing the wrong
  thing.** `internal/reconcile` was mock-verified and green for weeks. Its mock
  returned `map[string]string` because that is the obvious shape for UCI values,
  and the device returns a bool and a number among them — so the very first read
  against hardware failed completely. The mock did not merely miss a bug; it
  encoded a simpler world than the one the code runs in, and every test written
  against it inherited that. Where a mock has to invent a payload shape, get the
  shape from a real capture.
- **Latency is not load.** Four documents described `iwinfo` as "~92% of a
  focused poll" and that number was being used, implicitly, to reason about
  what focused polling costs a device. It is 92% of the poll's *wall time*; in
  CPU a focused poll costs only 1.25× a baseline one, because those calls block
  on the wireless driver instead of burning cycles. The original measurement
  was correct and the inference drawn from it was not. When a figure gets
  reused, check that the quantity it measured is the quantity now being argued
  about.
- **Check whose model a question actually needs.** Client scoping sat in the
  backlog behind "needs the site model, so it is a Phase 3 dependency". The
  reasoning was that telling a LAN from a WAN requires a definition of a LAN —
  true — and the unexamined step was assuming the definition had to be *ours*.
  The device already has one and publishes it in a single call. The item was
  half a day's work sitting behind a phase boundary that did not exist. When a
  dependency is asserted, check which system actually holds the fact.
- **A comment that states a guarantee is a claim, and claims need checking.**
  `Logs.tsx` carried an accurate, well-argued paragraph about why filter counts
  must come from an aggregate rather than the loaded page — sitting directly
  above code that counted the loaded page. The prose was not wrong about the
  principle; it was wrong that the code implemented it. Nothing flags this: it
  reads as documentation of a decision rather than an assertion about
  behaviour. Same failure as the 32-bit wrap guard whose comment claimed it
  "bites at the focused rate". When a comment promises a property, the property
  is a test, not a sentence.
- **A default CSS value is not a fixed value.** `height: 33` on a `<td>` is a
  minimum in table layout; the row came out 33.84px. Virtualization multiplies
  that error by the row index, so it was invisible at the top of the grid and
  most of a screen wrong at the bottom — the worst possible signature, because
  every casual check happens at the top. Anything that gets multiplied by N
  should be measured rather than assumed.
- **A probe is only read-only if it cannot succeed.** ARCHITECTURE specified
  fingerprinting devices with a `session.login` that fails, on the reasoning
  that a failed login reads nothing and writes nothing. The reasoning is sound
  and the probe is not, because it assumes the login fails. On a device with no
  root password it succeeds, and the "read-only" sweep becomes a sweep that
  mints a root session on every passwordless host in the subnet. The design
  error is the same shape as reading a denial as an absence: an operation was
  classified by its *intended* outcome rather than by the outcomes it can
  actually have. The fix — `list`, which has no success case worth having —
  turned out to be cheaper, faster and more informative than the thing it
  replaced, which is usually what happens when the honest version is found.
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
- The device credential lives in the session scratchpad
  (`oonfeewrt-device-password.txt`), currently
  `oonfeewrt / usJAW5PSBYGjsex35nS7gNZqKARH662M`. It is rotated by every
  adoption, and `TestIntegrationAdoptARealDevice` prints the one it creates —
  re-run that to get a known credential, then re-grant the `oonfeewrt-probe`
  scope (below) or the applyengine hardware tests fail. If lost, delete
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
