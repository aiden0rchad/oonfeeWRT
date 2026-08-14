# oonfeeWRT — Device Resource Budget

The wrapper promise is worthless if the wrapper degrades the router. A management
tool that costs you 15% of your routing throughput is not a management tool, it's
a tax.

This document sets hard budgets, explains where the cost actually comes from
(it is not where most people assume), and lists the design rules that hit the
budget.

---

## 1. Target hardware classes

We do not target "OpenWrt devices" generically. We target three classes, and the
weakest class sets the budget.

| Class | Example | CPU | RAM | Flash | Notes |
|---|---|---|---|---|---|
| **A — Comfortable** | Linksys WRT3200ACM | Marvell Armada 385, 2× Cortex-A9 @1.6 GHz | 512 MB | 256 MB NAND | Genuinely roomy. Dual firmware partitions (see §6). `mwlwifi` driver quality is the real risk here, not resources. |
| **B — Modern efficient** | MT7981 / Filogic 820 "AX3000" units (Cudy WR3000, GL-MT3000 class) | 2× Cortex-A53 @1.3 GHz | 256–512 MB | 32–128 MB | Good CPU, good crypto. The sweet spot. |
| **C — Constrained** | MT7621 "AX3000" units (Archer AX53/AX55 class) | 2× MIPS 1004Kc @880 MHz | 128–256 MB | 16–128 MB | **This class sets the budget.** Weak scalar CPU, no crypto acceleration, and 16 MB flash on many SKUs leaves almost no room for added packages. |

> **"TP-Link AX3000" is ambiguous** — it spans both MT7621 (class C) and newer
> MT7981 (class B) silicon depending on model and revision, with very different
> flash sizes. The capability probe must classify by actual SoC and free flash,
> never by marketing name.

**Design to class C.** Class A and B then have headroom to spare, which is the
right direction for the error to point.

---

## 2. The budget

Measured, not estimated. These are test criteria, not aspirations.

| Condition | CPU (class C) | RAM | Network | Flash writes |
|---|---|---|---|---|
| **Idle** — device adopted, no UI open anywhere | **< 0.5%** | < 4 MB attributable | ≤ 1 request / 60 s | **zero** |
| **Observed** — a UI screen showing this device is open | **< 3%** | < 8 MB | ≤ 1 request / 10 s | **zero** |
| **Applying config** | transient spike acceptable | — | — | one commit |
| **Never** | routing throughput reduction | — | — | periodic writes |

**Zero flash writes in steady state is a hard rule.** These devices have NOR or
NAND with finite write cycles and no wear-levelling worth trusting. oonfeeWRT
persists nothing on the device — all state lives in the controller. Any tier-2
daemon we suggest must be configured to keep its working data in `/tmp` (tmpfs,
i.e. RAM), accepting loss on reboot, because the controller is the durable store
anyway.

---

## 3. Where the cost actually is

Most people assume the polling itself is expensive. It usually isn't. Ranked by
real cost on class C:

### 3.1 TLS handshakes — the dominant cost

A fresh TLS handshake in `uhttpd`/mbedTLS on an 880 MHz MIPS core with no crypto
acceleration is expensive — far more than the ubus call it wraps. Polling five
devices every 30 s with a new connection each time means you are paying for
handshakes, not for data.

**Rules:**
- **Persistent connections, always.** One long-lived HTTP/1.1 keep-alive
  connection per device, reused for the life of the poll session. Reconnect on
  failure, not on schedule.
- **Reuse the ubus session token** until it actually expires. Do not re-login per
  poll.
- Prefer ECDSA certificates over RSA if the device's cert is generated at
  adoption — dramatically cheaper handshakes on weak CPUs.
- Offer plain HTTP as a supported option on an isolated management VLAN, with an
  honest warning. On class C this is a real performance decision, not laziness.

### 3.2 Process spawns via `file.exec`

Every `file.exec` forks and execs a binary. On class C that's meaningful when
done per-radio, per-interval. Native ubus objects cost a fraction of the same
data via a spawned `iw`.

**Rules:**
- **Prefer native ubus objects over `file.exec` wherever the data exists there.**
  `iwinfo`, `network.device`, `system.info`, `luci-rpc.getHostHints` cost no
  process spawn.
- Use `file.exec` only for data with no ubus equivalent (LLDP neighbours,
  `ethtool -S`), and at the slow-loop interval, never the fast one. Channel
  survey is *not* one of these — `iwinfo.survey` is native ubus.
- **Measured, class A — and this is the single biggest win available.** The six
  `iwinfo` calls are ~92 % of a focused poll: seven non-wifi calls total 15.8 ms,
  and adding two `info`, two `assoclist` and two `survey` takes the same batch to
  194 ms. Batching amortises transport, not this; it is driver time inside
  `iwinfo`. Sourcing the same data from `hostapd.<iface>` instead collapses it:

  | Focused poll, one batched request | Measured |
  |---|---|
  | current shape (13 calls, 6 × `iwinfo`) | **196.6 ms** |
  | `hostapd` for status+clients, `iwinfo.survey` kept (12 calls) | **72.4 ms** |
  | `hostapd` only, survey demoted to the slow tier (10 calls) | **17.4 ms** |

  An 11× reduction, on the class where the budget is *comfortable* — so on class
  C, where every one of those driver calls is worse, this is the difference
  between the focused tier being affordable and not.

  **Take the radio-level win; the client-level one is smaller than it looks.**
  `hostapd.get_status` is a safe substitute for `iwinfo.info` and is where most
  of the saving is. `hostapd.get_clients` agrees with `iwinfo.assoclist` on who
  is connected (57 samples, 100 %), but it lacks `tx.retries`, `connected_time`,
  `signal_avg`, `noise` and `thr`, so the Client Devices row needs `assoclist`
  regardless. Budget ~30 ms per radio for it on the focused tier and take the
  saving on `info`. `iwinfo` also remains required
  for noise (signed), txpower, country and hwmodes, all near-static and
  belonging on the slow tier.
- **Batch ubus calls into one HTTP request** where the JSON-RPC batch form is
  supported **[verify on target release]** — one round trip, one TLS record,
  many calls. This is the single biggest cheap win available.

### 3.3 Flow offloading conflicts — the one that actually hurts

This is the serious one on class C, and it is not obvious.

MT7621-class devices only reach gigabit routing because of **flow offloading**.
Anything that requires the CPU to see every packet defeats it.

- **Software flow offload + accounting:** historically, offloaded packets
  bypassed the counting path, so byte/packet counters under-reported badly. This
  was fixed by enabling counters on the nftables flowtable — at a documented
  **~3% throughput penalty** and requiring kernel 5.7+
  ([openwrt#10399](https://github.com/openwrt/openwrt/issues/10399)).
- **Hardware flow offload:** packets bypass the CPU entirely. Per-flow accounting
  is fundamentally unavailable for offloaded flows. You cannot have both.
- Hardware offload on MT7621 has its own history of instability with nftables
  ([openwrt#9241](https://github.com/openwrt/openwrt/issues/9241),
  [openwrt#10354](https://github.com/openwrt/openwrt/issues/10354)).

**This means per-client bandwidth accounting and maximum routing throughput are
mutually exclusive on class C hardware.** That is a genuine tradeoff, not a bug
we can engineer around.

**Rule:** oonfeeWRT never silently changes offload settings. If the user enables
per-client traffic accounting on a device with hardware offload active, the UI
states the tradeoff explicitly — "this will disable hardware offload and may
reduce routing throughput on this device" — and makes them choose. Default:
**leave offload exactly as we found it, accounting off.**

### 3.4 DPI — simply out of budget

`netifyd`/nDPI inspects every packet. On class C at gigabit this is not a
degradation, it is a failure. Combined with §3.3, the Flows screen is
**unavailable on class C by design**, not by omission.

---

## 4. Design rules that hit the budget

1. **Zero new daemons by default.** A device adopted with default settings runs
   nothing it wasn't already running. Every tier-2 package is opt-in, per device,
   with its cost stated in the consent dialog.
2. **Poll only what a human is looking at.** Two rates:
   - **Baseline (always on, ~60 s):** device reachability, firmware version, and
     the handful of series that need unbroken history — WAN throughput, client
     count, CPU/RAM. Roughly one batched request per minute per device.
   - **Focused (only while a UI screen showing that device is open, ~5–10 s):**
     everything else — per-client RSSI, per-radio survey, port counters.
   When the last UI closes, the device drops back to baseline within one interval.
   Nobody watches the Radios page at 3 a.m.; don't poll for it.
3. **Compute on the controller, never on the device.** The device returns raw
   state. Every derivation — experience scores, interference percentages,
   rollups, rankings, topology inference — happens on the Pi/NAS, which has real
   CPU. Never ask a router to `awk` something for us.
4. **Stagger, don't stampede.** Spread device polls evenly across the interval.
   Ten devices at 60 s means one request every 6 s, not ten every 60.
5. **Back off on evidence.** If a device's load average, or its own response
   latency, crosses a threshold, lengthen its interval automatically and say so
   in the UI. Adaptive, not fixed.
6. **Never poll during an apply.** Quiesce collection for that device until the
   apply/confirm cycle completes.
7. **Fail quiet.** A device that's slow or unreachable gets exponential backoff,
   not retry storms. A struggling router must not be hammered by its manager.

---

## 5. Per-feature cost table

What each screen actually costs on class C. Use this to decide what ships.

| Feature | Tier | Mechanism | Class C cost | Default |
|---|---|---|---|---|
| Device status, firmware, uptime | 0 | `system.info`/`board` | negligible | **on** |
| Client list, names, IPs, vendors | 1 | `luci-rpc.getHostHints` | negligible, one call | **on** |
| Per-client RSSI / rate / retries | 0 | `iwinfo.assoclist`, or `iw station dump` | low; focused-rate only | **on (focused)** |
| Interface throughput | 0 | `network.device` counters | negligible | **on** |
| WiFi config read/write | 0 | `uci` | negligible | **on** |
| Firewall / VLAN / DHCP config | 0 | `uci` | negligible | **on** |
| Channel survey / utilization | 0 | `iwinfo.survey` (native ubus) | ~29 ms per radio, no spawn — focused loop is fine | **on** |
| Topology / LLDP neighbours | 2 | `lldpd` | small daemon, low duty cycle | opt-in |
| Per-client bandwidth + 24h usage | 2 | `nlbwmon` | **conflicts with flow offload — see §3.3** | **off** |
| Long-term interface totals | 2 | `vnstat` | small, but writes to disk — force `/tmp` | opt-in |
| Band steering / roaming assist | 2 | `usteer` or `dawn` | modest, event-driven | opt-in |
| Queue management / SQM | 2 | `sqm-scripts` (CAKE) | **significant CPU at gigabit on class C** — user's existing decision, we only expose it | opt-in |
| Firewall event log | 0 | nftables `log` → syslog, read via `file.read` | scales with rule hit rate — rate-limit the rules | opt-in |
| RF scan | 0 | `iw scan`, user-triggered | disrupts clients on that radio | manual only |
| **Flows / DPI / app identification** | 2 | `netifyd`, `ntopng` | **out of budget** | **unavailable on class C** |

---

## 6. Per-class notes

**Class A (WRT3200ACM).** Resources are not your problem — 512 MB RAM and 256 MB
NAND are luxurious by OpenWrt standards. Two other things are:

- The `mwlwifi` driver's telemetry is **partly** unreliable, and the split is
  now measured rather than assumed. Good: `iwinfo.survey` works natively and its
  `active_time`/`busy_time` are sound, so channel utilization is trustworthy.
  Bad: `rx_time`/`tx_time` come back uninitialised (a ~1.4e19 u64), and
  `iwinfo.survey` reports `noise` **unsigned** (161 where the true value is
  −95) while `iwinfo.info` reports it correctly signed. Take noise from
  `iwinfo.info`, and capability-gate anything needing rx/tx time.
  Showing confidently wrong data is worse than showing none.
- It has **dual firmware partitions**, which makes UniFi's per-device "Revert"
  button genuinely implementable here — one of the few places we can match a
  UniFi feature that most OpenWrt hardware can't support. Capability-gate it.
- Armada 385 routes near-gigabit **without flow offloading**, which dissolves
  the §3.3 accounting tradeoff on this device: nlbwmon can be offered with a
  clear conscience. A capability-registry fact, not a global assumption.

**Class B (MT7981/Filogic).** The comfortable target. ARMv8 with crypto
extensions makes TLS cheap, so §3.1 mostly stops mattering.

**Class C (MT7621).** Everything in §3 applies at full force. Also: many SKUs
have **16 MB flash**, which leaves single-digit megabytes free after a normal
install. The capability probe must check free `/overlay` space and simply not
offer tier-2 packages that won't fit — refusing cleanly, with the reason shown,
rather than attempting an install that fills the filesystem and bricks the
router.

---

## 7. Prove it — and show it

Two commitments that follow from having a budget at all:

**Test it.** A benchmark harness that adopts a class-C device, runs baseline and
focused polling for an hour, and asserts the §2 numbers. Run it per release. A
budget nobody measures is a wish.

> **Built, and it earned its keep on the first run.**
> `internal/daemon/budget_integration_test.go`; `OONFEE_BUDGET_MINUTES=60` for
> the real thing. It measures device-side state over SSH, which the controller's
> own credential deliberately cannot reach — a harness that could only see what
> the controller sees could not check whether the controller is lying.
>
> The first run failed, on a class-A device, at **1.08 requests/min idle against
> a ceiling of 1.0**. Two real defects:
>
> - **Interface discovery was a separate unbatched call**, breaking the
>   collector's own "one request per poll" rule and costing an extra request
>   every 15 minutes. It now travels inside the batch, and its answer is used by
>   the *next* poll — the list decides what goes in the batch, and the batch is
>   already built by the time the answer arrives. One poll of staleness on a
>   list that changes only when someone reconfigures the radios.
> - **The shipped focused default was 8 s**, which is 7.5 requests/min against
>   this table's ceiling of one per 10 s. §4.2's "~5–10 s" is the design range;
>   this table is the budget, and the shipped default has to meet it. Now 10 s.
>
> After both: **idle 1.00 polls/min (60 requests/hour, 1 non-poll request — the
> session login), observed 6.00 req/min, zero flash writes**, 21 polls with no
> failures, ~1.3 KB per request. Device CPU 0.49% across the run, all causes.
>
> Class C still sets the budget and remains unmeasured — see the open items in
> `STATUS.md`.

**Show it.** A per-device **Management Overhead** readout in the UI: requests per
minute, CPU percent attributable to oonfeeWRT, packages we installed, and the
current poll interval — with a control to loosen it.

> **Built**, in the device slide-over: current interval and tier, requests per
> minute against the budget, bytes sent, polls and failures. It flags a rate
> over budget, and separately flags requests that were not polls — logins
> amortise to nothing, so a number that grows with the poll count means a call
> escaped the batch, which is a defect rather than a rate to average away.
>
> **All three of the spec's remaining fields landed 2026-08-14.**
>
> **Attributable CPU percent.** It needed the control measurement, and the
> measurement showed why a live sample can never work: a baseline poll costs
> about 5 ms of device CPU once a minute — **0.009% of one core** — while the
> device's own idle CPU sits at 0.38–0.43%. What we are trying to measure is
> roughly *fifty times smaller* than the floor it would have to be measured
> against, and far smaller than the minute-to-minute jitter in that floor. A
> live reading would be noise with a decimal point on it.
>
> So it is derived from a control experiment: device CPU over a window with
> nothing polling, versus a window with a known number of polls.
>
> | | class A reference device |
> |---|---|
> | control, nothing polling | 0.38–0.43% busy |
> | baseline poll, 8 invocations | **5.33 ms** of device CPU |
> | focused poll, 12 invocations | **6.65 ms** of device CPU |
> | at the shipped baseline (1/60 s) | 0.0089% of one core |
> | at the shipped focused (6/60 s) | 0.067% of one core |
>
> Checked for linearity rather than assumed: 4.56 ms/poll at 6,049 polls/min
> and 4.38 ms/poll at 372 polls/min, within 4% — so the figure is not an
> artefact of saturating the device and extrapolates down honestly.
>
> **The finding worth carrying forward: the call that dominates a poll's wall
> time is not the call that dominates its CPU cost.** §4 measures iwinfo as
> ~92% of a focused poll, yet a focused poll costs only 1.25× a baseline one in
> CPU — because `iwinfo.survey` and `iwinfo.assoclist` block on the wireless
> driver rather than burning cycles. Latency and CPU load must not be used
> interchangeably when reasoning about what we cost a device.
>
> The figure is reported **only for classes it was measured on**. Class C has
> never been measured, and a class-A number shown against a class-C device
> would be a guess in a measurement's clothing — so those devices get no figure
> and a sentence saying why.
>
> **Packages we installed.** Empty, and reported rather than omitted: "the
> controller installs no packages" is the claim ARCHITECTURE §0 makes, and a
> field that only appears once it is non-empty cannot be used to check it.
>
> **The control to loosen the interval.** Per device, 60 s to 1 hour. It can
> only make polling *cheaper*: the collector clamps any override below the
> controller default. That clamp is in the collector rather than in request
> validation on purpose — this table is a promise about what the controller
> does to a device, the harness measures the default, and a knob that could
> raise the rate would put a device outside the budget in a way no test would
> ever see. Verified against real hardware: an override of 5 s stores as 5 and
> polls at 60.

UniFi never shows you this, and the reason it can afford not to is that it owns
the hardware. We don't. Surfacing our own cost is both the honest thing to do and
a real feature: it turns "is this thing slowing down my router?" from an anxiety
into a number the user can read and act on.

---

## 8. The controller's own envelope (Docker, decision D7)

The controller ships as a self-hosted container (Omada-style) and runs on a
NAS, mini-PC, Pi, or home server. That host has real CPU, real disk with wear
levelling, and RAM to spare — so the controller's own budget is about being a
polite long-running service, not about survival:

| Resource | Envelope | Notes |
|---|---|---|
| Image size | ≤ 40 MB | scratch/distroless base + static Go binary + embedded UI |
| Steady-state RSS | ≤ 256 MB at 25 devices | generous; measured per release, not fought over |
| CPU, idle fleet | ≤ 2% of one modern core | poll loops + rollup |
| Disk | ≤ 2 GB at full 13-month retention | one volume, one SQLite file, trivially backed up |
| UI bundle | ≤ 1.5 MB gzipped | unchanged — this budget was about the *browser*, not the host |

**Retention returns to full depth by default:** 5m rollups → 14 days, 1h
rollups → **13 months**. The trimmed on-router ladder is gone.

**Keep the write shape anyway.** The RAM ring for raw samples + one
transaction per 5-minute flush was designed for NAND survival, but it's simply
good engineering: it keeps SQLite happy, makes the flush path testable, and
costs nothing. Do not regress to per-sample INSERTs just because the disk can
now take it.

**What §8 no longer contains — deliberately:** NAND wear-shaping for the
controller host, the 25 MB binary cap, `GOMEMLIMIT` ceremony, Cortex-A9 TLS
arithmetic, and the two-hats self-management hazard. All of that existed to fit
the controller onto the WRT3200ACM. In the container model the WRT3200ACM is a
*managed device* — where its notes live in §6 like everyone else's — and the
apply-path-traversal warning applies to it only in the ordinary way (when a
change touches the path the controller reaches it through).

**And what does not relax:** §1–7. Managed devices didn't get faster. The
polling budgets, TLS handshake rules, flow-offload tradeoffs, and
zero-flash-writes rule for managed hardware are exactly as binding as before.
