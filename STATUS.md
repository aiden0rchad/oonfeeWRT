# Where this project is

Written 2026-08-13 as a handoff, and rewritten as the work moved. Current
through **2026-08-16**, ending with 802.11k neighbour distribution across two
real APs. Everything below is either committed or measured on real hardware;
nothing here is aspiration.

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

And the UI:

```bash
npm --prefix ui test
```

The hardware suite needs the device and a credential — §7 explains the rotation
dance, which is the one genuinely fiddly part of this repo:

```bash
OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt OONFEE_TEST_PASS=...   go test -tags=integration ./internal/... -timeout 25m
```

**Two devices, both adopted through the controller and both clean as of
2026-08-16:**

| | WRT3200ACM | Archer C6 v2 (US) |
|---|---|---|
| address | `192.168.1.1` (gateway, WAN to UniFi) | `192.168.1.2`, DHCP off, static |
| identity (`br-lan`) | `30:23:03:db:be:40` | `c4:e9:84:...` — ask, do not assume |
| SoC / target | mvebu/cortexa9 — **class A** | ath79/generic — **class ?** |
| radios | mwlwifi ×2 | ath10k (5G) + ath9k (2.4G) |
| firmware | OpenWrt 25.12.5 | OpenWrt 25.12.5 |
| mesh | **gated off** (driver quirk, §5q) | **Present**, verified working |
| our footprint | ACL + rpcd login (reinstalled after the reset) | ACL + rpcd login installed |
| airtime-split | absent (dead counters) | **Present** — only device with it |
| neighbour reports | **Present**, re-adopted after the reset | **Present** |
| wired layout | `br-lan`, DSA | `eth0.1` / `eth0.2`, swconfig, no DSA |
| health | **reset 2026-08-16 after four wedges; 28 min clean since, unproven** | stable, 3h+ uptime unattended |

**The wired topology, corrected 2026-08-16 by looking rather than by trusting
this file.** It was wrong in two ways that both cost time:

| | |
|---|---|
| dev Mac | `192.168.1.3` on **en13** — this file said `.181` on `en9` |
| WRT `lan1` | the dev Mac |
| WRT `lan3` | the C6's LAN port (`eth0.1`), bridged into `192.168.1.0/24` |
| WRT `wan` | the UniFi network |
| **C6 `wan` (`eth0.2`)** | **also the UniFi network, `10.7.46.52`** — undocumented until now |

The C6 being **dual-homed** is the part worth knowing. Its WAN is routed rather
than bridged, so it is not a layer-2 loop — but it is a second path, and it
means unplugging the WRT-to-C6 cable does not isolate the device the way the
one-line description implied. The dev Mac cannot reach `10.7.46.52`, so from
here the C6 still goes away when that cable does; from the UniFi side it does
not.

Anything that reasons about "what happens if this device loses its cable" has to
start from this table rather than from the sentence that used to be here.

> ### ⚠ The WRT3200ACM is failing hardware. Do not treat it as a reference device.
>
> Five wedges across 2026-08-15/16, one of them AFTER a factory reset. The
> factory reset did not fix it. Signature every time:
>
>     nl80211: nl80211_recv_beacons->nl_recvmsgs failed: -5
>
> repeating once a minute, with `hostapd`, `rpcd`, `netifd` and `iwinfo` all in
> `D` state and load climbing. A clean `reboot` is **blocked** — procd cannot
> kill the D-state processes — so the only recovery is a hard reset:
>
> ```bash
> ssh root@192.168.1.1 'sync; printf b > /proc/sysrq-trigger'
> ```
>
> #### The cause — see §5aa, which supersedes what this section used to claim
>
> **It is the 5 GHz firmware.** Caught live on 2026-08-16: the driver stops
> reaching the 88W8964 on phy0 (`cmd 0x801d=MEMAddrAccess timed out`, then every
> ~20s forever), and the `-5` above arrives 40 seconds later as `EIO` from a
> driver that cannot reach its firmware. hostapd's D state is it blocking on
> that driver. Because nl80211 operations serialise, one stuck phy0 call blocks
> **every** radio — the healthy 2.4 GHz one included.
>
> This section previously named a trigger: a client deauthenticated for
> inactivity, 66 seconds before the first error. **That was a coincidence in one
> sample.** The occurrence caught live had no deauth at all — the last wireless
> event was a routine opmode change 8.5 minutes earlier. No trigger has been
> identified. What is known is the failing component and the order of collapse.
>
> **A client associated, went idle, was deauthenticated, and the driver failed
> 66 seconds later.** That sequence is in every pre-reset log too; this is the
> first time it was observed rather than reconstructed, and it explains why the
> device looked healthy for hours whenever no client was on it.
>
> #### The failure mode that invalidates a management-plane check
>
> Worse than the wedge, and found only by scanning the air from the other
> device: for roughly **14 hours** the WRT beaconed `wrt-cleanroom` — an SSID
> that existed in **no configuration anywhere on the device** — while
> `/etc/config/wireless`, hostapd's running conf, `iwinfo`, ubus **and the
> kernel's own `iw dev info`** all reported `oonfee-roam`. A `wifi reload` did
> not clear it. Only a hard reset did.
>
> The stale SSID lived in firmware state nothing in Linux could see. This is the
> fourth mwlwifi entry in the same family as the mesh-point and `txpower=0`
> quirks, and the most consequential, because **every management interface can
> agree on a configuration the radio is not running.**
>
> #### What that means for this project's own claims
>
> Earlier readings in this file of "no wedge in 3h36m" and "14 hours healthy"
> were measuring whether hostapd answered ubus. It did — throughout the period
> the radio was transmitting the wrong SSID. **An ubus read is not an RF
> measurement**, and anything claiming hardware verification of a wireless
> property needs a scan from a second device to mean what it says. §5t's
> neighbour verification is affected: the C6's half was real, the WRT's half was
> read from hostapd, and for those 14 hours the WRT's `oonfee-roam` was not on
> the air at all.
>
> #### Recommendation
>
> Stop putting WLANs on it and stop using it as a reference device. It fails
> within 9–30 minutes of a client associating and going idle, and no software
> change in this project can affect that. A firmware reflash is worth one
> attempt before writing it off; after that it is a hardware replacement. The
> Archer C6 has run 16+ hours through every experiment here without a stumble.

**Wireless currently on air:**

- `oonfee-roam` on **both** APs, 2.4 + 5 GHz — the controller-managed WLAN,
  WPA2-PSK with 802.11r/k/v, `mobility_domain=90e4`, and the neighbour lists
  §5t distributes. Restored to the WRT after the reset by the setup helper.
- `oonfee-c6-5g` / `-2g` on the C6 — created by hand to enable its radios (stock
  OpenWrt ships them disabled, and enabling them unsecured would broadcast two
  open networks). **Their neighbour lists are empty and stay empty**, which is
  the "never touch an SSID we do not manage" rule visible on hardware.
- The WRT's stock `default_radio0/1` are back to `disabled=1`. The consequence
  worth knowing: `oonfee-roam` is now the *first* interface on each of its
  radios, so its BSSIDs moved from `32:23:03:db:be:43`/`:40` to
  `30:23:03:db:be:42`/`:41`. The distributor propagated that to the C6 by
  itself — two updates on a device nobody had touched.

**Credentials.** Both controller logins are sealed in `.run/keyring.json` under
the operator passphrase, and that is the only copy — adoption never returns the
password it generates. Nothing in this repo holds a password and nothing should;
§7 has how to check or reset one by asking the device rather than a document.

The WRT's record went stale when it was reset, and the recovery ran for real:
the controller diagnosed it (§5u) with the message it had gained an hour
earlier, then the setup helper force-un-adopted and re-adopted without being
told anything about the reset.

**Root has no password on either device, and that is a deliberate hold.**
Decided 2026-08-16: the lab stays open while the system is being built, and
device authentication is taken up as its own piece of work once everything else
is buttoned up — so that it can be *verified* rather than assumed. It is not an
oversight and it does not need raising again.

Two things follow from it that a reader should know rather than rediscover. It
is why adoption can bootstrap over SSH with an empty credential, which is how
both devices were re-adopted for the ACL change without anyone typing a
password. And the controller's own behaviour here is already built and tested —
`acceptsAnyPassword` detects it at adoption and warns (§4), the discovery probe
was redesigned around it (§5a), and none of that is waiting on the hold. What is
deferred is hardening the *devices*, not the controller's handling of them.

**When that work starts**, the pieces already in place are worth reusing rather
than rebuilding: the adoption warning, the two-credential split (operator for
SSH, scoped login for ubus), credential sealing in the keyring, and §7's
check-and-reset procedure. The open questions are whether the controller should
offer to *set* a root password during adoption, and whether an SSH key should
replace password auth for the bootstrap channel entirely.

**One habit worth inheriting:** before any experiment that writes to a device's
network config, arm a restore on the device itself first (§6, "arm the undo
before the experiment"). It saved this work three times.

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

**Roaming is now more than configuration.** As of 2026-08-16 the controller
distributes 802.11k neighbour lists across the fleet — reading each AP's own
neighbour element and telling every other AP on the same SSID about it. That is
the first feature here that hand configuration cannot reproduce at all, because
no AP can learn what is around it. §5t.

**Phase 2 is largely complete and its ROADMAP proof is met.** One SSID edited
once lands on both bands of an AP with an identical derived mobility domain, a
hand-edited section elsewhere on the device is untouched, and the whole thing is
previewed per device before anything is written. Networks (VLAN, DHCP, firewall
zone) render too, within a limit that hardware imposed — §5g is the single most
important thing in this file.

Twenty Go packages plus a UI. Everything that touches a device has been
verified against one.

---

## 2. The test device

| | |
|---|---|
| Model | Linksys WRT3200ACM, OpenWrt 25.12.5 r33051 (mvebu/cortexa9, class A) |
| Reached at | `192.168.1.1` over ethernet from the dev Mac at `192.168.1.3` (`en13`) |
| Root access | SSH key auth works; **root has no password set** |
| WAN | up, on the UniFi-routed `10.7.46.0/24` |
| Radios | both enabled, `oonfeewrt-probe-2g` / `oonfeewrt-probe-5g`, WPA2 |

**Our footprint on it right now:** `/usr/share/rpcd/acl.d/oonfeewrt.json`, one
`rpcd` login (`oonfeewrt`, password sealed in `.run/keyring.json` — re-adopt if
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
| `internal/roaming` | Which APs are each other's 802.11k neighbours | pure |
| `internal/meshlink` | What a backhaul is actually doing, and `iw station dump` | pure |
| `internal/onair` | Whether a BSS is really transmitting, cross-checked between APs | ✅ |
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
design**. Full detail in `docs/IMPLEMENTATION.md` §14 and §15; this is the short list
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
- **802.11k neighbour lists are runtime state, and `wifi reload` clears them
  SELECTIVELY.** Measured: after editing one wifi-iface section and reloading,
  the reconfigured BSS came back with an empty list while an untouched BSS on
  the same device kept its own intact. So neither "an apply clears everything"
  nor "an apply clears nothing" is a safe assumption, and the list must be read
  back rather than remembered. `rrm_nr_get_own` returns a **positional triple**
  `[bssid, ssid, nr_hex]`, and `rrm_nr_list` returns entries in hostapd's own
  storage order — neither insertion order nor sorted — so comparison has to be
  order-insensitive or the reconciler never converges.
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

0. ~~**Neighbour-report distribution (`rrm_nr_set`).**~~ **Done 2026-08-16** —
   §5t. The ACL decision §5s left open was made: `rrm_nr_get_own` and
   `rrm_nr_list` to read, `rrm_nr_set` to write, and deliberately **not**
   `bss_transition_request`. Verified across both APs.

   The obvious follow-on is **`rrm_beacon_req`**, which asks a *client* what it
   can hear. That is the missing half of roaming: the controller now knows where
   its APs are and still has no idea what any client's radio actually sees, so
   it cannot tell "this phone is stuck on a far AP" from "this phone is exactly
   where it should be". It is a bigger step than it looks — a beacon request is
   a frame sent to a client, so it needs a policy for which clients and how
   often, and the answer arrives asynchronously as an event rather than as a
   call's return value. Nothing in the collector consumes device-pushed events
   yet.

1. ~~**Look at the screens in a browser.**~~ **Done 2026-08-16** — §5v. Four
   defects in one sitting, none reachable by any test in the repo, including a
   grid whose headers could not be dragged and a suite that tested dragging
   anyway. The sticky header was checked by eye and holds.

   **Still unlooked-at:** the Clients grid with a real client on it (both APs
   currently have none associated), the Logs facets under load, and the adopt
   and discovery screens. Keep doing this — it remains the highest-yield check
   this project has, and the count is now nineteen.
2. ~~**A mesh backhaul whose health cannot be seen is half a feature.**~~
   **Done 2026-08-16** — §5w. Thirteen states, no new device requests for four
   of the five facts, and the peer read on the slow slot. Four bugs found by
   running it, one of which would have reported a critical fault after every
   successful mesh apply.

   **The half that could not be done here:** no `peered` state has ever been
   observed, because mesh is Present only on the C6 and there is no second node
   to peer with. Three rungs of the ladder are unit-tested and have never met
   hardware.

   **One more device is now by far the highest-value thing this project could
   acquire**, and after §5x the case is stronger than it was. A third router
   would close four separate gaps at once: mesh `peered`, the wireless uplink
   (which needs a radio that will run a station — neither of these two will),
   ROADMAP Phase 2's "three APs", and the first class B or C measurement, since
   every number in DEVICE-BUDGET comes from the comfortable class. Any cheap
   MT7621 or ath79 box that takes OpenWrt does all four.

   The remaining device-side gap is `mpath dump`, the forwarding table, which
   is the one thing here that would need an ACL grant. Worth it only once a
   mesh actually carries traffic, since a path table with no peers is empty.
3. ~~**A WDS/relay bridge is still entirely unmodelled.**~~ **Built
   2026-08-16** — §5x. Capability, model, renderer, store, API and screen, with
   two review-found guards and three hardware-found bugs fixed. Unproven on this
   hardware because **station mode does not work on the C6 at all** — isolated
   three ways, so not the controller, not 4-address, not concurrency.

   **`classify()` still covers three SoC families**, and the half of that item
   which needs no lab is done (§5m item 3): the panel names the board target
   instead of a bare `?`. Adding targets to the map still needs measuring.

4. ~~**Look at the three new screens, and the Clients grid with a real client on
   it.**~~ **Done 2026-08-16** — §5ad. All three cards seen, plus the Clients
   grid with a real client, the Logs screen and the adopt/discovery screen. Four
   more defects, **count now twenty-three**, still none reachable by any test.

   **The unadopt flow is now looked at too** (2026-08-17, §5ai and §5aj): eight
   more defects, **count thirty-one**, and one of them made a device that cannot
   be reached permanently un-removable. Two came from reading the panel, two
   from driving it, and four from three review rounds over those — 2, then 3,
   then 1, which is the first thinning tail in a review sequence here. **Still unlooked-at:** any screen under a
   fleet larger than two devices.

5. ~~**The adoption bug has no regression test.**~~ **Done 2026-08-16** — §5z.
   Both halves pinned and mutation-verified, and the fixture gained the two
   things whose absence had made the bug untestable.

6. ~~**Persist the SSH host-key pin.**~~ **Done 2026-08-17** — §5ah. The last
   open review finding. **No numbered item is left**; what follows is a
   practice, not a list.

7. **Nothing else pressing.** The rest is hardware- or package-blocked (below).

### If you are picking this up cold

Read **§0** first (the reference hardware lies, and why), then **§6** (the
mistakes already made and the rules that came out of them). Those two explain
most of the decisions in the code.

Then, in order of value:

- **There is no open finding and no numbered item left.** Everything below is a
  way of working rather than a task, and the yield from each has been measured.
- **Look at a screen in a browser.** **Thirty-one** defects have been found
  this way and not one was reachable by any test in the repo. Everything has
  been looked at once now, so the yield is in what CHANGES — and in the screen
  ABOVE whatever was just changed, which is where the last two came from
  (§5ai): the daemon's un-adopt path grew a new way to fail, and reading the
  panel over it found that the panel had no way to recover from ANY of them.
- **Review whatever was written last, not just the code.** Three review rounds
  ran on 2026-08-16/17. The second found more than the first *because* it
  reviewed the first's fixes, including one that committed the exact error it
  was fixing. The third found four highs in code nobody had reviewed at all.
- **Mutation-test every new test.** Revert the fix; if the test still passes it
  asserts nothing. Six were caught that way in two days, none by reading.

**Two things need the operator, not the next session:**

1. **Rotate the Archer C6's WPA passphrase.** It was committed to this public
   repository on 2026-08-16 (`02e99d0`, removed in `5982cec`) and must be
   treated as compromised. `tools/secret-scan.sh` confirms nothing else leaked.
2. **A third router** unblocks the whole remaining backlog at once — mesh
   `peered`, the wireless uplink, three-AP fan-out, and the first class B/C
   budget measurement. Any cheap MT7621 or ath79 box does all four.

### Where I would start, if picking this up cold

Not with a feature. The most valuable half-hour is **running the on-air check
(§5y) and then reading §0**, in that order. The first tells you whether the
fleet is actually doing what the controller believes; the second tells you why
that question needed asking. Everything else in this file is downstream of the
day those two things came apart.

**Landed 2026-08-16 alongside the neighbour work**, all from things the session
tripped over rather than planned: a factory reset is now diagnosed instead of
reported as a permission error (§5u); adoption refuses a second device at one
address; the setup helpers adopt instead of assuming a login exists; and an
unmeasured hardware class names its board target instead of rendering a bare
`?` (§5m item 3).

### Built but NOT TESTED, and what each one waits on

Nothing here is broken. Each is complete in code and tested from unit level up,
and each has a specific claim nobody has been able to check. Listed together so
a reader can see the shape of what the lab cannot reach — **the answer to almost
all of it is one more router**, and as of 2026-08-16 there is not one.

| what | untested claim | what would settle it |
|---|---|---|
| **Mesh `peered`** (§5w) | that two nodes find each other and the backhaul carries | any second mesh-capable device |
| **Wireless uplink** (§5x) | that a station associates and bridges | a device whose radio runs station mode — measured, neither of these two does |
| **Three-AP fan-out** | ROADMAP Phase 2's stated proof | any third AP |
| **Class B / C budget** | DEVICE-BUDGET's CPU and RAM rows | specifically an **MT7621** (class C) or **MT7981/filogic** (class B) |

The last row is narrower than the others and worth not conflating with them: an
ath79 or ipq40xx box closes the first three and leaves the fourth exactly where
it is, because `classify()` would call it `?` and no measured number would
attach to a class. **Class C sets the budget**, so that row is the one where the
shipped defaults are least justified — every figure in DEVICE-BUDGET comes from
the comfortable class.

Two things worth saying about this list. None of it blocks further development:
the pipeline is not per-device, so a third AP needs hardware rather than code.
And none of it should be quietly closed by reasoning — §5q, §5w and §5x are each
a case where every available signal said a thing would work and the device
disagreed.

### Blocked on something other than hardware

- **`usteer` / `dawn` configuration and state readout.** Neither is installed on
  the reference devices; both are in the official feeds. This sits behind the
  package-installation flow ARCHITECTURE §6 step 3 describes and nothing has
  built. Writing config for an absent package would be untestable, so it was not
  written.
- **The WRT3200ACM under a client.** Not a hardware purchase — a client that
  prefers it. It has now run 13 hours since the factory reset with no wedge,
  through polling, applies, a mesh and an 802.11k reconciler, and has carried
  **zero clients** the entire time, because the one client on this network
  associates to the C6 and stays. A client was the single condition every
  pre-reset failure shared, so the reset cannot be called a fix until one is on
  it. See §0.

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
| §5k | What diffing two probes found in the probe itself |
| §5l | Column reorder, and getting UI logic under a check without a test runner |
| §5m | **Hardware breadth** — the stated direction, what it needs, and what assumed otherwise |
| §5n | 802.11s mesh — modelled as an interface mode, not a role |
| §5o | The bug mesh support created in the collector, found by looking for it |
| §5q | **Applying a mesh to real hardware** — and what every other source got wrong |
| §5r | **Two devices at last** — fast roaming verified across different SoCs |
| §5s | The roam demo — what it proved, what it did not, and the txpower trap |
| §5t | **Neighbour reports** — the first thing built that hand configuration cannot do |
| §5u | What a factory reset looks like from the controller, and why it used to look like nothing |
| §5v | **The browser pass** — four defects in one sitting, and why none was reachable by a test |
| §5w | **Mesh backhaul health** — a closed state vocabulary, and the four bugs only hardware could show |
| §5x | **The wireless uplink** — built end to end, and unprovable on this hardware for a sharper reason than mesh |
| §5y | **Asking the air** — the one check that does not trust the management plane, and the adoption bug it exposed |

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
- **The UI's automated tests cannot see layout, and a person is still the only
  thing that catches a dead affordance.** Driving it in a browser has now caught
  **nineteen** defects no unit test would have
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

### 5k. What diffing two probes found in the probe itself

Comparing two probes turns out to be a test of the probe. Run it twice against
an unchanged device and every non-deterministic determination shows up as a
change — which is how this was found, not by reading the code.

**The bug: an idle channel was reported as a broken driver.** `FeatAirtimeSplit`
is decided by sampling `busy_time`, `rx_time` and `tx_time` twice and checking
the timers advance in proportion. `splitOK` started at `Absent`, and the branch
for an idle channel — which carries the comment *"this sample proves nothing
either way"* — fell straight through to that default. So it recorded proof of
exactly what it had just said it could not prove.

On a driver whose counters work, that makes the feature `Present` when the
channel happened to be busy during the probe and `Absent` when it happened to be
quiet, so re-probing reports the device gaining and losing airtime-split at
random, with a warning each time. The reference device hides it completely:
mwlwifi's counters are genuinely broken, so it is stably `Absent` for a real
reason, and no amount of re-probing this hardware would have shown it.

It is the same collapse the package exists to prevent — "could not determine"
becoming "does not have" — one level in from where the rule is usually applied.
The three-way outcome is now an explicit `judgeSplit`, testable without a device
that happens to be busy, and an undetermined split records `NotObservable` with
a note saying to re-probe while there is traffic. No screen changes: `Buildable`
already accepted only `Present`. What changes is what the record *claims*, which
is what a diff reads and what an operator is told.

**Then the same bug twice more, found by looking for it.** Having seen the shape
once, the other radio-derived features were audited rather than waited for:

- **`FeatSurvey`** was decided by `active_time > 0` with the same `Absent`
  default, so a device with every radio switched off reported that its driver
  cannot do channel utilization — and enabling a radio would then look like the
  device *gaining* a feature. The reference hardware could never show it: both
  radios are up, and one radio with active time settles the device-wide state.
- **`FeatHostapdControl`** was worse, because the two causes are genuinely
  indistinguishable from the error alone. `hostapd.<dev>` only exists while a
  BSS is running, so a missing object means either "hostapd is not managing
  this radio" or "the radio is off". It now uses whether the radio reported
  active time to tell them apart: a *running* radio with no hostapd object is a
  real absence; a dead one demonstrates nothing.

Three instances of one bug, written three times, is a structural problem rather
than three mistakes. The rule is now a `verdict` accumulator — Present wins,
demonstrated absence beats an inconclusive check, anything tried-but-unresolved
is `NotObservable`, and you cannot get an `Absent` out of it without calling
`demonstrated(Absent)`. A device that reports *no radios* still comes out
`Absent`, which is right: there is nothing there to survey, and telling someone
to re-probe their switch would be nonsense.

The reference device reports exactly what it did before the change — survey
present, split absent, hostapd-control present — which is the point. None of
this was visible on hardware that has both radios up and a genuinely broken
driver.

**Also observed:** the reference device reported 2 quirks on one probe and 3 on
the next. The extra one is `noise:stability`, which is decided by sampling the
noise floor twice — and the 2.4 GHz radio on this board swings 40+ dB. That is
real device behaviour, not a bug, and it is the reason quirks are not diffed.
The features they gate are, so a quirk that actually costs a capability is still
reported; the noisy list itself is not. Worth remembering if quirks ever start
driving something directly.

### 5l. Column reorder, and a way to check UI logic without a test runner

Drag-to-reorder was the last half of UI-SPEC §5's "Customize Columns". The
feature is small; what it ran into is not.

**There is no way to click anything here.** Every UI defect this project has
found was found by a human looking at a screen, and shipping a drag interaction
that nobody has dragged is exactly the pattern that produced a sticky header
which had never once been sticky.

So the UI got a test runner: **vitest**, with **happy-dom** and
`@testing-library/react`. Vitest because it reuses the existing vite config and
TypeScript setup rather than needing a parallel one; happy-dom over jsdom for a
materially smaller dependency tree, which matters in a public repo where every
dev dependency is surface. Both packages shipped with critical advisories at the
versions npm resolved first — `npm audit` is worth running after any install
here, and the pinned versions are the patched ones.

**The runner found a real bug within minutes of existing.** happy-dom provides a
`localStorage` object with none of the Storage methods on it, which surfaced
that `useColumnPrefs` read `localStorage.getItem` outside a `try`. Reaching for
localStorage *throws* in some browsers — Safari private mode historically, any
profile with site data blocked — and that read runs inside a `useState`
initialiser. So the failure mode was not "forgets which columns you hid", it was
"blank screen". The guard now wraps the access, not just the parse.

It also cost an hour to a hook-ordering trap worth knowing: **vitest runs
`afterEach` in reverse registration order**, so a teardown hook that throws
stops the ones registered before it from running at all. A broken localStorage
teardown therefore prevented `cleanup` from unmounting, and every test after the
first saw the previous test's DOM — producing failures that pointed everywhere
except at the cause.

**48 tests now**, across the shared grid and the screens. The grid file covers
windowing engaging on a large grid, required columns resisting being hidden,
saved order and hidden-column positions surviving a remount, a drop not also
sorting the column it landed on, and `Unknown` staying distinguishable from
zero. The screen file covers the rules that live above it, and it is worth
saying which ones were worth writing:

- **The mesh editor must not warn "this will be open" when editing an encrypted
  mesh.** The list omits the passphrase, so a round-trip sends an empty one; if
  the editor read that as "open", a rename would strip encryption from a
  wireless backhaul. Both directions are tested — silence on an edit, a warning
  on a genuinely new open mesh — because getting either wrong is bad in a
  different way.
- **A visibility change renders as "not a loss".** The three-state rule at the
  UI layer: rendering `no-longer-observable` the same as `lost` recreates, on
  screen, exactly the bug the capability model exists to prevent.
- **A filter change resets the paging offset**, and a failed refresh keeps the
  last good page rather than blanking the grid — "no clients" and "the fetch
  failed" are different claims. What they do **not** reach: row
height and the sticky header, because happy-dom has no layout engine and
`getBoundingClientRect` returns zeros — the two defects §5b found by eye are
precisely the two this cannot catch. And nothing here says whether a drag
actually starts or whether any of it is usable. That still needs a person.

Rules worth keeping, each of which the checks pin down:

- **Moving right and moving left are not symmetric.** Removing the dragged key
  first shifts the target left into the slot it just left, so "drag one place
  right" silently does nothing unless the insert index compensates.
- **Reordering rewrites the full key list, hidden columns included.** Ordering
  only the visible ones loses the hidden ones' positions, so unhiding a column
  later drops it somewhere the operator never chose.
- **A column a later build ADDS must still appear**, and one a later build
  REMOVES must not break the saved order. Storage outlives any one build.
- **The old storage format migrates.** Preferences were a bare array of hidden
  keys; someone who hid four columns must not get them all back because a later
  build started storing an order alongside.

The picker's ◀ ▶ arrows are not a fallback for the drag — they are the only
path that works without a mouse, and the only one that can move a *hidden*
column, which dragging cannot because there is no header to grab.

### 5m. Hardware breadth: the direction, and the audit

**The stated goal**, 2026-08-14: support as much hardware as possible —
whatever old router is lying around, flashed with OpenWrt and adopted off the
network the way a UniFi device is, working as an access point, a switch, or a
bridge/mesh node with switch support. So anyone can extend their network with
hardware they already own.

That reframes several things that looked settled. This is the audit.

#### What already generalises

- **Poll cadence is not class-dependent.** One conservative default (60 s) for
  everything, plus adaptive widening when a device reports it is busy. The
  DEVICE-BUDGET ceiling is applied to every device rather than computed per
  class, which is the right shape for unknown hardware — a device nobody has
  measured is not polled harder than one that has been.
- **Capability probing is three-state and now structurally cannot invent an
  absence** (§5k). This matters far more with varied hardware than with one
  reference device: everything the controller offers is gated on what the probe
  demonstrated, so a driver nobody has seen degrades to "we could not tell"
  rather than to a wrong claim.
- **Discovery fingerprints on `ubus list`**, which any OpenWrt with rpcd
  answers. No model list to maintain.

#### Fixed here: the role was free text

`Role` was a string, stored exactly as the API received it and compared with
`dev.Role != "gateway"`. Three consequences, all silent:

- `"Gateway"` is not `"gateway"`, so the obvious capitalisation adopted a router
  as an access point — no address, no DHCP, no firewall zone, no forwarding.
- A typo did the same, and the only clue was a preview that did less than
  expected.
- **`"switch"` was accepted and then never consulted.** A device adopted as a
  switch was an access point in every respect that mattered, and would happily
  be sent WLANs.

It is a closed vocabulary now (`internal/model/role.go`), refused at the API
boundary before anything contacts the device, normalised on the way out of the
database, and the renderer asks it what it licenses rather than comparing
strings. A non-wireless role gets no WLANs even where the hardware could carry
them and the site model asks — with an omission naming *both* ways out, since
either the role or the AP-group membership is wrong and the controller cannot
tell which.

**The Adopt screen had no role field at all**, which made a gateway impossible
to adopt from the UI. It has one now, defaulting to access point — the role
that changes least about a device.

#### Fixed here: the role is now checked against the hardware

`roleFit` compares the role an operator chose against what the probe found, at
adoption and on every re-probe. It **warns and never refuses**: the role is a
statement of intent and the probe is a snapshot, and they disagree for good
reasons — radios switched off today and wanted tomorrow, a board file that
under-reports, hardware being prepared before it is cabled. Refusing would turn
a note into a wall, and the operator is the one who knows which of the two is
wrong. What it must not do is stay quiet, and silence was the previous
behaviour: adopt an old router as an access point, get no WLANs, no error, and
a preview that renders nothing.

The three-state rule shows up again inside it. An empty radio list means either
"this device has none" or "we could not ask" — `probeRadios` returns early with
the wireless features `NotObservable` when `iwinfo.devices` is refused, and the
list is empty either way. Those need different messages: one says the role is
wrong, the other says the ACL is narrow, and telling someone to change the role
when the real problem is a refused call sends them to fix the wrong thing.
`FeatSurvey` separates them without a second call.

Verified against the reference device's own registry — 2 radios, survey
present, `br-lan` and `wan` declared — so the premise is checked on real shapes
rather than only on hand-built fixtures: an AP role is silent, a switch role
says plainly that nothing will broadcast.

#### Added here: 802.11s mesh can be detected

The first honest step toward mesh is knowing which devices could carry it, and
that turned out to need measuring rather than reading. Three obvious sources
**cannot** answer it, checked on the reference device 2026-08-14:

| source | what it gives |
|---|---|
| `iwinfo.info`, `luci-rpc.getWirelessDevices` | `hwmodes` are PHY modes (`n`, `ac`) — no supported-interface-mode list |
| `hostapd.<dev> get_features` | `{ht_supported, vht_supported}` and nothing else |
| `file.exec /usr/sbin/iw phy <phy> info` | **ubus status 6** — not in the ACL, and status 6 is permanent |

What does answer it is which **wpad build** is installed, and that grant already
exists. On OpenWrt the 802.11s daemon is a build of wpad: `wpad-mesh-*` carries
mesh with SAE, `wpad-basic-*` and `wpad-mini` deliberately do not. So no ACL
widening was needed — which matters, because a new grant only reaches devices
adopted *after* it, and existing ones would have reported NotObservable forever.

Two things fell out of doing it this way:

- **The reference device uses `apk`, not `opkg`** — `opkg` exits 4 there. The
  probe tries both. And apk glues the version onto the name with a hyphen, so
  splitting on the first hyphen truncates `wpad-mesh-openssl` to `wpad`, which
  would report a mesh-capable device as unclassifiable. The mock now answers
  `apk` in apk format, so that path is exercised without hardware.
- **A full build such as `wpad-openssl` records `NotObservable`, not Present.**
  Those are not named for their feature set and none has been verified here.
  Claiming mesh from a package name that does not settle it is precisely the
  guess §5k caught the probe making elsewhere.

Confirmed on hardware: `mesh-80211s` present, from `wpad-mesh-openssl`.

#### What is still missing, in the order it matters

1. **A WDS/relay bridge is still unmodelled.** The goal names "AP bridge mesh";
   802.11s covers the mesh half, and a WDS bridge is a different mechanism
   (`wds`/4addr rather than `mode mesh`) for the case where the far end is not
   mesh-capable.
2. **The collector now knows what each wireless interface is FOR** — see §5o.
   Applying a mesh would otherwise have reported the backhaul as clients.
3. **Nothing verifies a mesh actually peers.** The controller can configure one
   and can see `mode mesh` on a radio, but there is no mesh-neighbour readout —
   `iw dev <if> mpath dump` / `station dump` would give it, and neither is in
   the ACL. A backhaul you cannot see the health of is half a feature, and this
   is the first thing worth building once there are two nodes.
3. **`classify()` covers three SoC families.** mvebu, filogic/MT7981, MT7621 —
   everything else is `ClassUnknown`, which is *most* old routers: ath79,
   ramips/MT7620, ipq40xx, bcm53xx, lantiq. **Adding targets to that map
   without measuring them would be a guess wearing a measurement's clothes**, so
   the map is unchanged and the second half of this item is done instead: the
   device panel now renders `?` with the board target and the actual
   consequence — polled at the conservative default, no CPU cost claimed —
   rather than a bare question mark that reads as a fault. The C6 was the
   device that made this visible, reporting `class=?` for `ath79/generic`.

   Three UI tests, and the third is the one that matters: a device with **no**
   class is distinct from one whose class is *unmeasured*. "We never asked" and
   "we asked and nobody has measured this" are different claims, which is the
   same distinction the whole capability model turns on.
4. **Class B and C remain unmeasured.** Class C (MT7621) sets the budget and
   every number in this project comes from class A. The budget harness runs
   anywhere; it has only ever run against the comfortable class — and §5k is
   the standing reminder that one reference device hides whole categories of
   bug.

### 5n. Mesh, and why it is not a role

The design decision worth not re-litigating: **a mesh point is a wifi-iface
mode, not a device role.**

The obvious modelling — "mesh" alongside gateway/ap/switch — is wrong in exactly
the way that matters for the hardware this is aimed at. On OpenWrt a mesh point
is a `wifi-iface` with `mode 'mesh'`, and a device carries one *at the same time*
as an AP serving clients. That combination is the whole of "AP bridge mesh with
switch support": an old router extending the network over the air while still
serving clients and its wired ports. A role would make those mutually exclusive
and force a choice between the two things an operator wants together.

Three more rules, each encoded and tested:

- **One band per mesh, not a list.** A WLAN publishes on several bands because a
  client picks one and roams. Mesh nodes peer only within a band, so "the same
  mesh" on 2.4 and 5 GHz is two disjoint backhauls whose halves each look
  healthy. The band is a field, not a slice, and a device without that radio is
  told *why* it cannot join rather than just that it has no 5 GHz.
- **SAE implies required PMF.** 802.11s encryption is SAE, and SAE without
  protected management frames gives peers that refuse each other for reasons
  nobody enjoys debugging.
- **An empty passphrase on update preserves the stored one.** Same rule as
  `SaveWLAN` and sharper here: the API never sends a mesh key back out, so a
  read-modify-write would silently convert an encrypted mesh into an open one —
  and an open mesh is joinable by anyone in radio range, with access to the
  network behind it. An open mesh is still *allowed* (a trusted segment is a
  real case) but the renderer says what it means, once, on the preview.

The capability gate is three-state for a concrete reason: rendering a mesh
interface into a build that cannot carry it produces a radio that silently does
not come up. "Your device cannot" and "we could not find out" send an operator
to different places — different hardware versus a package or an ACL — so the
omissions say which.

**API and UI landed with it.** `GET/POST/DELETE /site/meshes`, and a "Mesh
backhauls" card on the settings screen. The passphrase rule is the one worth
checking: the listing carries `has_key` and never the key, the single-mesh
endpoint reveals it as a deliberate separate request, and an update with an
empty key preserves the stored one. Verified against a running controller —
create, list, rename-without-key, reveal — because the failure it prevents is
silent and severe: a rename that quietly drops encryption leaves a backhaul
anyone in radio range can join.

**Verified on hardware, preview only.** Applying an 802.11s interface to the
reference device would write wireless config to the router everything else is
reached through, and a one-node mesh has nothing to peer with. What was checked
is that the plan is built from what the device actually reported: real radios,
real wpad build, real existing config. Result: `oowrt_mesh1_radio0` planned,
flagged as writing a key, with the passphrase itself kept out of the preview.

**The apply and prune path is covered against the mock**, which the preview
could not reach: a mesh applies and is recorded as ours, a second apply of an
unchanged mesh is a no-op, and deleting it from the model plans its removal.
The no-op matters more here than for a WLAN — a mesh section carries a
passphrase, so a plan that never converged would rewrite it on every apply, and
a rewrite briefly drops the interface. Which on a backhaul is the link.

### 5o. What mesh support broke in the collector

Adding a feature creates the conditions for bugs elsewhere, so the question
after landing mesh was what it had just made reachable. One thing, and it was
real.

`discoverIfaces` uses `iwinfo devices`, which lists **every** wireless
interface. The poll then asks each one for `hostapd get_clients`, and on the
focused tier `iwinfo assoclist`. **A mesh point's associated stations are its
peers — other access points.** So the first time anyone applied a mesh, the
backhaul would have been counted as connected users: infrastructure in a list
captioned "your devices", which is the identical complaint client scoping (§5c)
already fixed once for upstream neighbours.

Nothing had gone wrong yet — no mesh has been applied — but the feature that
makes it possible shipped an hour earlier.

The fix needed to know each interface's mode, and the source took measuring
again. `iwinfo.info` reports it per interface (`"Master"` for an AP) but that is
one call per interface. `luci-rpc.getWirelessDevices` gives every interface's
`ifname` and configured `mode` in **one** call, and is already granted — so this
costs one extra call per 15-minute rediscovery, on the cadence that already
exists for the board and the radio list.

Three things worth keeping:

- **The decode is deliberately narrow.** `getWirelessDevices` returns each
  interface's whole UCI config *including `key`, the wireless passphrase, in
  plaintext*. The struct names exactly two fields so the rest is discarded by
  the decoder rather than carried around where a later log line could print it.
  There is a test asserting no passphrase reaches the snapshot.
- **An unknown mode means "assume AP"**, which is what the controller did before
  modes existed. Answering the other way would let a denied call quietly stop
  counting real clients — a number that is too low, with nothing saying so.
- **The survey is still asked of every interface.** Channel utilization is a
  property of the radio's channel, not of what the interface is for, and a radio
  carrying only a mesh point would otherwise report none at all.

### 5p. Standing limitations now have somewhere to be read

The collector has always recorded a `Degradation` for every optional call that
was refused or unreadable, and logged them at **debug**. The reason is sound —
a degradation is a standing property of a device's ACL or driver, not an event,
and logging it per poll would bury everything else. The consequence was that
nobody could ever see one.

That became load-bearing with §5o. Without `luci-rpc.getWirelessDevices` the
poll cannot tell a mesh point from an access point, so it falls back to treating
every interface as an AP — the right fallback, since the alternative silently
stops counting real clients, but it means a device with a narrow ACL quietly
gets the bug the fix was for.

So the device detail carries them now, with **what each one costs**:
`luci-rpc.getWirelessDevices: Permission denied` tells an operator nothing;
"the poll cannot tell a mesh point from an access point, so a mesh backhaul's
peers are counted as clients" tells them everything. Permanent refusals — an
ACL gap — are marked apart from transient ones, because the two call for
different responses.

A device that has never been polled reports **no list at all** rather than an
empty one, which would read as "everything is fine here".

This matters more as the fleet widens. Adopting whatever old routers are around
means varied firmware, varied packages and varied ACLs, and "what can this
controller not see on this device" stops being an edge case and becomes a
routine question.

### 5q. What applying a mesh to real hardware found

Mesh had been verified by preview, by the mock, and by the apply/prune path
against that mock. Then it was applied to the reference device, and the result
is the most useful thing in this file.

**It applied cleanly and did not exist.**

    apply: wrt3200acm -> applied (1 changes) health passed and confirm landed
    on device: oowrt_mesh1_radio0 mode=mesh mesh_id=oonfee-hw-mesh
    interface modes after the apply: map[phy0-ap0:ap phy1-ap0:ap]

uci accepted the config. The apply's health check passed — it asks whether the
SSIDs are on air, and they were. The confirm landed. The section is on the
device. And no mesh point is running.

SSH answered what ubus cannot:

    wpa_supplicant: Could not set interface phy0-mesh0 flags (UP):
                    Operation not permitted
    wpa_supplicant: phy0-mesh0: Failed to initialize driver interface

`ip link` shows `phy0-mesh0 ... state DOWN`. netifd creates the interface and
the driver refuses to raise it.

**Every source a controller can consult said this would work:**

| source | says |
|---|---|
| installed packages | `wpad-mesh-openssl` — the daemon does 802.11s |
| `iw phy0 info` | supported interface modes include **mesh point** |
| `iw phy0 info` | combinations allow `#{AP} <= 16, #{mesh point} <= 1` |
| `uci`, apply, health check, confirm | all succeeded |

Disabling the AP on the same phy changes nothing — it is not a combination
limit. mwlwifi simply will not bring a mesh point up.

This is precisely the category `Quirk` was made for: *present, correctly typed,
plausible, and wrong*. The same driver already supplies three others. So mesh is
gated off on Marvell radios with a quirk that records the measurement, and the
capability is `Absent` on this board **even though the daemon supports it**.

Two consequences worth keeping:

- **A package list is not a capability.** `probeMesh` reads which wpad build is
  installed because nothing else can answer, and that answer is necessary and
  not sufficient. The daemon's capability and the radio's are different
  questions.
- **Absent has two causes, and they send an operator to opposite places.** A
  missing wpad-mesh package is fixable by installing one; a driver that refuses
  is fixable only with different hardware. The renderer's message said "install
  wpad-mesh-*" for both — advice to install a package that was *already
  installed*. It distinguishes them now, and there is a test for each.

**The device was left byte-identical to how it was found**, all four managed
configs, with a dead-man restore armed on the device before anything was written
(§6) and disarmed afterwards. Worth noting that the disarm needed checking: the
first `pkill` did not take, and a `sleep 1200` was still pending a `wifi reload`
at an arbitrary future moment. Arming an undo is half the practice; confirming
it is gone is the other half.

### 5r. Two devices: fast roaming, verified across different silicon

A second router arrived — a TP-Link Archer C6 v2 (US), `ath79/generic`, ath9k +
ath10k — cabled LAN-to-LAN behind the WRT3200ACM. First time this project has
had two.

#### What the second device confirmed immediately

Adoption on hardware it had never seen, and three separate pieces of work firing
correctly for the first time outside a fixture:

- **`class=?`** — `ath79` is not one of the three SoC families `classify()`
  knows, and it says so rather than guessing. §5m item 3, visible in production.
- **`roleFit` diagnosed the radios**: *"adopted as gateway, but this device
  reported no radios… its radios may be disabled — enable one and re-probe."*
  Stock OpenWrt ships radios disabled, so the message written that morning met
  its exact case within hours.
- **The passwordless-root warning fired** — this device accepts any password for
  root, the behaviour ARCHITECTURE §6's probe was redesigned around.

Then, with the radios enabled: **`airtime-split` is Present** — the first
device where it is. mwlwifi's counters are dead, so the WRT3200ACM will never
have it, and §5k's `judgeSplit` correctly found working counters here. Probe
stability also held on a second, entirely different device: second probe,
**unchanged**.

Its wired layout is `bridge="eth0.1" wan="eth0.2"` — swconfig VLANs, not
`br-lan`. Nothing had produced that shape before.

#### Mesh works here, and that validated §5o

`FeatMesh` is **Present** on the C6 and gated **Absent** on the WRT3200ACM —
the two-cause distinction (§5q) exercised from both sides on real hardware.
Applied by hand, the interface came up properly:

    phy0-mesh0: joining mesh oonfee-hw-mesh
    phy0-mesh0: MESH-GROUP-STARTED ssid="oonfee-hw-mesh"
    br-lan: port 4(phy0-mesh0) entered forwarding state

And `iwinfo devices` then lists `phy0-mesh0` alongside the APs — which is
exactly why §5o matters: without the mode filter the poll would ask a mesh point
for its "clients" and report backhaul peers as users.

**The hardware apply test read the modes too early.** A mesh takes ~4 s to come
up and the assertion ran immediately after the apply returned. The product was
right; the test was wrong. Worth remembering when the next one is written: an
apply returning is not the same as a radio being ready.

#### Fast roaming across two APs — the actual verification

The question was whether 802.11r works reliably when adopting other OpenWrt
routers. The renderer emits `ieee80211r`, `mobility_domain`,
`reassociation_deadline` and `ft_over_ds`, and **not** `nas_identifier`, `r0kh`,
`r1kh` or `ft_psk_generate_local` — the four that decide whether FT completes or
falls back to a full reauth. So: look at what the device generates.

**WPA2-PSK.** OpenWrt fills in `ft_psk_generate_local=1`. Every AP derives the
FT keys from the shared passphrase, no key-holder exchange needed.

**SAE / WPA3** — which is the UI default, and where local generation cannot
apply because the key comes from the handshake:

    ft_psk_generate_local=0
    r0kh=ff:ff:ff:ff:ff:ff * 141f748db78bb03c75216d6248ca68fc
    r1kh=00:00:00:00:00:00 00:00:00:00:00:00 141f748db78bb03c75216d6248ca68fc
    wpa_key_mgmt=SAE FT-SAE WPA-PSK WPA-PSK-SHA256 FT-PSK

OpenWrt generates wildcard key holders with a key derived from the mobility
domain and the passphrase. **The identical config on the Archer C6 produced the
identical key** — `141f748db78bb03c75216d6248ca68fc` on Marvell/mvebu and on
Qualcomm/ath79, different drivers, different radio vendors.

That is the whole design working. The controller derives the mobility domain
deterministically so every AP computes the same value without coordination; that
same value plus the passphrase is what OpenWrt hashes into the FT key. And the
reason it holds is `override.go`: SSID, passphrase, security mode and roaming are
**deliberately not overridable per device**, precisely because APs that disagree
about them do not fail cleanly — they fail intermittently.

`nas_identifier` is unset on both. With wildcard key holders hostapd falls back
to the BSSID, so FT still completes; recorded because it is a field the renderer
could set and does not.

**Still unverified: an actual client roaming between them.** The configuration
is right on both APs and the keys match, which is the hard part — but nothing
has yet watched a phone hand off. That needs a client and a `logread` on both
ends, and it is the obvious next thing.

**Automating this is blocked on one thing:** the check reads
`/var/run/hostapd-phy0.conf`, which needs SSH — no ACL grant covers it, and none
should. A two-device integration test would have to drive SSH the way adoption
does.

### 5s. The roam demo: what it proved, and the trap it found

A real client (an iPhone) on `oonfee-roam`, one SSID across both APs.

**Proved:** the phone held IP `192.168.1.249` throughout, with `DHCPREQUEST`/
`ACK` renewals and no fresh `DHCPDISCOVER`. Same lease, same subnet — the L2
arrangement is right. Both APs carry the same SSID, same `mobility_domain=90e4`,
same FT key, on all four radios.

**Not proved: an observable fast transition.** A `bss_transition_request` from
the C6 was accepted (`exit=0`) and the phone did leave — then re-scanned and
chose the C6 again, because at that position the C6 was genuinely stronger. That
is correct client behaviour: iOS weighs an 802.11v hint against its own
measurements rather than obeying it. Watching a handoff still needs the target
AP to actually be the better choice.

#### The trap: `txpower=0` wedges mwlwifi until a reboot

Reducing transmit power is the obvious way to force a roam between two APs
sitting side by side. On the WRT3200ACM, setting `txpower=0`:

- was accepted by uci and reported as applied;
- made mwlwifi fail to program keys into hardware
  (`failed to set key ... (-5)`), so the second BSS never came up;
- then took the **whole 5 GHz radio** down —
  `Could not set interface phy0-ap0 flags (UP): I/O error`, and
  `nl80211 driver initialization failed`;
- survived `wifi reload` AND `wifi down/up`.

Worse, `rmmod mwlwifi` (an attempt to recover it) left `modprobe` hung in R
state with no phys at all. **Only a reboot cleared it.**

`iwinfo txpowerlist` advertises `0` as supported on this driver. It is not. Same
shape as the mesh-point claim in §5q: the device asserts a capability, accepts
the configuration, and fails in hardware.

**By contrast the Archer C6 (ath10k) took `txpower=4` cleanly** — radio stayed
up, all four interfaces intact, power applied. So the rule is per-driver, not
universal.

**Consequence for the product.** The controller does not expose transmit power
today. If it ever does, mwlwifi needs a floor above 0 — otherwise it hands an
operator a config that applies successfully, reports healthy, and kills their
5 GHz until they power-cycle the router. That is the §5g failure shape exactly:
a confirmed, "healthy" change that breaks the device.

#### The gap this surfaced: nobody populates the neighbour list

The renderer sets `ieee80211k=1` and `rrm_neighbor_report=1`, so each AP
*advertises* that it can answer neighbour reports — while knowing about no
neighbours. `rrm_nr_get_own` and `rrm_nr_set` are in hostapd's ubus API on both
devices, and a controller is the one component ideally placed to use them: it
knows every AP in a group, their BSSIDs and their channels.

This is the "essentially impossible to maintain by hand across a fleet" claim
the roaming code makes about itself, currently unfulfilled. It is the strongest
candidate for the next real feature.

**It needs an ACL change.** `hostapd.*` currently grants `get_status`,
`get_clients`, `get_features`, `list_bans` and `del_client` — not `rrm_nr_set`
or `bss_transition_request`. And a new grant only reaches devices adopted
*after* it (§5q), so widening the ACL means existing devices report the feature
NotObservable until re-adopted. That is the whole decision. **It was made the
next day — see §5t.**

---

### 5t. Neighbour reports: the first thing built that hand configuration cannot do

Every AP the renderer touches has carried `ieee80211k=1` and
`rrm_neighbor_report=1` since Phase 2, which makes it advertise that it will
answer a client asking "what else is around?". Measured on both reference
devices, every one of them answered with an **empty list**.

That is not a small gap. The whole value of 802.11k is that a client scans three
channels instead of all of them, and a client that asks and gets nothing scans
all of them anyway — so the feature was switched on across the fleet, costing a
beacon information element, and doing nothing.

An AP cannot close it. It knows its own BSS and nothing about the AP down the
hall; the two never talk. Something has to hold the whole fleet and tell each
member about the others, and the controller is the only component that does.
This is the first feature in the project that is not "LuCI, but for several
devices at once" — it is a thing that cannot be configured by hand at all.

#### The controller relays and never constructs

A neighbour report element packs a BSSID, a capability bitfield, an operating
class, a channel, a PHY type and optional subelements. Getting the operating
class alone right means mapping frequency and bandwidth through a regulatory
table.

hostapd already computes it, correctly, for its own BSS, and hands it over
verbatim:

    ubus call hostapd.phy0-ap1 rrm_nr_get_own
    { "value": [ "32:23:03:db:be:43", "oonfee-roam",
                 "322303dbbe43ef1900008024090603022a00" ] }

So the controller reads that and relays the bytes untouched. It never parses or
builds one. Doing otherwise would put a second regulatory mapping in the system,
disagreeing with the AP's own on exactly the bands where it matters.

Note the reply is a **positional triple**, not an object. A short array from a
firmware that shapes it differently must read as "could not tell", never as a
neighbour with blank fields — relaying one of those makes an AP answer a client
with a candidate it has no channel to scan for.

#### Why it reconciles instead of applying

Everything else the controller writes is UCI, and survives a reboot because it
is on disk. This does not: `rrm_nr_set` writes hostapd's **runtime** state and
there is no UCI option that carries it.

That is the right shape rather than a limitation to work around. A neighbour
list is derived from where the other APs are *now*, so one written to flash
would be worse than none — an AP confidently sending a client to a BSS that
moved channels a month ago. And it means none of the apply machinery applies:
no rollback (there is nothing to roll back to but the empty list it already
had), no confirm, no health gate, and no taking the fleet-wide apply lock for a
change that cannot make a device unhealthy.

#### The measurement that decided the design

The tempting optimisation is to remember what was last pushed and skip the read.
It does not survive contact with the device:

| after `wifi reload`, having edited one section | neighbour list |
|---|---|
| `phy0-ap1` — the BSS whose config changed | **cleared** |
| `phy1-ap1` — untouched BSS on the same device | **kept, intact** |

Neither "an apply clears everything" nor "an apply clears nothing" is true. So
the current list is **read back** and compared, which makes the operation
idempotent against every cause of loss including ones nobody has thought of — a
hostapd crash, an operator's own `wifi reload`, a device that rebooted between
cycles.

The best evidence it works is the hardware run where the WRT had rebooted and
the C6 had not: **2 updated on the WRT, 2 unchanged on the C6**, all four BSSes
ending with three neighbours. The reconciler repaired exactly what was broken.

**Comparison is order-insensitive, and that is measured rather than preferred.**
hostapd returns `rrm_nr_list` in its own storage order — on both devices neither
insertion order nor sorted. An order-sensitive comparison reports every list as
changed on every cycle and pushes to every AP forever: a reconciler that never
converges, indistinguishable from a broken one except that it also spends the
request budget.

#### What it costs

Per device per cycle: one `iwinfo.devices`, one batched request carrying two
calls per wireless interface, and — only when something differs — one more to
push. At the 15-minute cadence that is under a tenth of DEVICE-BUDGET's
one-request-per-minute allowance, and in the steady state the third request
never happens. Requests are attributed to the device's Management Overhead
readout, the same rule discovery follows.

Triggers are the 15-minute loop, one cycle at startup (a controller that just
started is most likely starting because something restarted, which is exactly
when the lists are empty), and after every apply.

#### The ACL decision §5s left open

Made, and narrowly:

| granted | not granted |
|---|---|
| `rrm_nr_get_own`, `rrm_nr_list` (read) | `bss_transition_request` |
| `rrm_nr_set` (write) | `rrm_beacon_req` |

The controller tells APs about each other and leaves the roam decision to the
client. Steering a client is a different feature with client-visible effects and
a policy behind it, and granting the method "while we are here" would put the
capability on every device ahead of the decision to use it.

Widening the ACL only reaches a device through adoption, so both devices were
re-adopted — which is the real upgrade path, and the integration test walks it
rather than arranging the end state by hand.

#### Three defects, none found by hardware

- **`verdict`'s empty default is `Absent`.** Right for a device that reported no
  radios — there is nothing there to give a neighbour list to — and wrong for
  one whose hostapd could not be reached, where nothing was recorded because
  nothing could be asked. A denied `get_status` therefore reported neighbour
  support as *absent*. Found by a test written specifically to check that one
  cause does not produce two symptoms. The fix ties the neighbour verdict to the
  hostapd verdict on the same radio, since you cannot learn about a method on an
  object you could not reach.
- **`Distribute` did not deduplicate by BSSID.** A list naming the same BSSID
  twice is malformed. The controller cannot assume its inventory is clean — one
  physical AP reached the function under two device rows (below) — so the
  identity that matters on the wire is made unique. Fixing it needed a matching
  two-value lookup in the caller, because *"no plan, another row covers this"*
  and *"planned with an empty list, clear your neighbours"* are different
  instructions, and treating the first as the second overwrites a correct list
  with nothing.
- **The mock advertised a hostapd with no `get_status`.** `OBJECTS` had
  duplicate `hostapd.wlan0` and `hostapd.wlan1` keys and Python kept the last,
  silently discarding the full method lists. Invisible, because the dispatcher
  answers `hostapd.*` before consulting that table — while `ubus list`, which is
  what discovery and the capability probe fingerprint on, read the truncated
  version.

#### And one the lab found: a fleet can hold the same AP twice

Adoption identifies a device by its `br-lan` MAC. The test seed helpers wrote
MACs as **literals** — one of them the box's WAN-side address, the other a
radio's — so a seeded row and a real adoption of the same physical box became
two devices in the inventory, both marked adopted, both pointed at
`192.168.1.1`. One AP polled twice, against a budget of one request a minute.

The helpers ask the device now, through the same function the real path uses. A
helper that computes an identity its own way produces rows that look adopted and
are not the rows adopting would produce.

**Still open, and worth a decision:** adoption refuses a device whose MAC it has
already seen, and has no guard on *host*. Nothing stops the same box being
adopted twice under two identities if its identifying interface ever changes —
which a bridge rename or a board file change would do.

#### And one more the hardware found: a partial cycle must not remove

One AP was still bringing its radios up while the other was reconciled, and the
healthy AP was handed a list with the booting one **deleted from it**. The
reconciler had done exactly what it was told: the missing AP contributed no
BSSes, so the computed table did not contain them, so they were removed.

That is the project's own rule broken at the fleet level. A device that could
not be read is not a device with no radios — and the failure modes here are not
symmetric. A stale neighbour costs a client one wasted scan; a missing one costs
it the full scan 802.11k exists to avoid.

So a cycle in which any device **errored** may add and refresh, and may not
remove (`roaming.Union`). Removals resume the moment a complete read succeeds —
verified: after the partial cycle had shrunk the C6's lists, the next complete
cycle repaired all four BSSes back to three neighbours each.

A device that was *skipped* does not make a cycle incomplete. It was reached, or
its own capability record ruled it out; either way its APs are not silently
missing from the table, and treating that as incomplete would mean a fleet with
one un-upgraded device could never remove a neighbour again.

#### Verified

Two APs, two bands each, one SSID, on mvebu/mwlwifi and ath79/ath10k:

| | |
|---|---|
| BSSes carrying `oonfee-roam` | 4 |
| neighbours each ended up with | 3 — every other BSS, and never itself |
| second cycle | 0 updated, 4 unchanged |
| second cycle from a *fresh database* | 0 updated, 4 unchanged |

The fresh-database run is the one worth keeping: the reconciler holds no state
between runs, so a controller that has lost its database still converges the
fleet from what the devices themselves report.

One more thing the fault conditions demonstrated for free. While the WRT's
hostapd was wedged, its re-probe recorded `neighbor-report: not-observable` —
**not absent**. The three-state rule held under a real fault, on a device that
genuinely has the capability, without anyone arranging it.

**And then it ran unattended.** A controller left up for an hour logged nine
lines in total, none of them a distribution: four 15-minute cycles found the
fleet converged and said nothing, which is what the "only a cycle that changed
something is worth a line" rule is for. Checked against the devices rather than
inferred from the silence — every managed BSS still holding three neighbours,
every unmanaged SSID still holding none. Quiet because there was nothing to do,
which is the only version of quiet worth having.

### 5u. A factory reset, seen from the controller

The reference device was factory reset mid-session. That is a real lifecycle
event — someone recovering a misbehaving router does it without telling their
controller — and the controller handled it badly enough to be worth fixing on
the spot.

A reset removes the rpcd login and the ACL file **and leaves everything else
intact**. So the controller is left holding a sealed credential for a box that
is on the network, healthy, answering, and has never heard of it. What that
produced was `ubus session.login: PERMISSION_DENIED`, once a minute, forever.

That message is also what an operator sees when a password was rotated, when an
ACL was narrowed, and when the keyring is wrong. Four different problems behind
one sentence, and only one of them has an obvious fix.

`Connect` now adds a diagnosis when the device answers discovery's
unauthenticated `list` and still refuses the credential. Three things about how
it does it:

- **It adds to the error rather than replacing it.** The login failure is still
  what happened, and callers matching on it have to keep working.
- **It says nothing when the device did not answer.** Telling someone to
  re-adopt a router that is merely unplugged sends them to rebuild something
  that only needs to come back. The check is asymmetric on purpose: answering
  proves the credential is the problem, and silence proves nothing.
- **It reuses `discovery.Probe` rather than rolling its own call.** A
  hand-written `session.list` is refused by a stock ACL, and a refusal is not
  the question being asked.

Recovery is un-adopt then adopt, and un-adopt must be **forced**: the footprint
it exists to remove is already gone, so phase 2 can never report a clean
removal. That is exactly the case §5b's `Force` fix was for, met for real. The
two-AP setup helper now does it by itself.

#### A factory reset also breaks LuCI, and that is not the controller's doing

Found while the WRT was being investigated for something else, and worth
recording because anyone following this project's adopt/reset lifecycle will
meet it. After the reset the web interface returned **403** on
`/cgi-bin/luci` while `/` and HTTPS served fine.

Stock `/etc/config/uhttpd` carries `lua_prefix` pointing at
`/usr/lib/lua/luci/sgi/uhttpd.lua`. On LuCI 26.x that file does not exist —
LuCI is ucode now — and `uhttpd-mod-lua` is not installed. The `ucode_prefix`
line that makes it work is added by `luci-base` through a uci-defaults script,
which runs **at package install and not on a factory reset**. So the reset
restores the base config and LuCI's addition never comes back. The handler
itself, `/usr/share/ucode/luci/uhttpd.uc`, is present the whole time and simply
unwired.

    uci del uhttpd.main.lua_prefix
    uci add_list uhttpd.main.ucode_prefix='/cgi-bin/luci=/usr/share/ucode/luci/uhttpd.uc'
    uci commit uhttpd && /etc/init.d/uhttpd restart

Nothing in oonfeeWRT causes or fixes this — it is recorded so that a reset
during an adoption experiment does not get mistaken for something the
controller did, which is exactly how the first ten minutes of finding it went.

#### The mock was wrong about the one call discovery depends on

Found while testing the above. `tools/mock_ubus.py` answered `list` with `{}`
unless its first parameter was 32 characters long — i.e. unless it looked like a
session token.

That is backwards from the device. `list` needs **no session**, and that is
precisely why discovery uses it (§5a): no credential, no session, no failed-login
record, and it returns the whole object graph. Discovery sends `params: ["*"]`,
so against the mock it saw an empty object list and graded a perfectly good
OpenWrt box as merely *reachable* rather than *OpenWrt*.

Nothing caught it because discovery's own tests use their own fixture. It
surfaced only when a daemon test asked discovery to identify the mock — one
component's fixture being checked by a different component's expectations, which
is the only thing that finds this class of bug. Same family as §6's "a mock that
is easier to write than the real thing is testing the wrong thing", one level
further out: the mock was not simpler here, it was *inverted*, and every test
that never asked this question passed either way.

### 5v. The browser pass, and the four things it found

Item 1 of the do-next list, done 2026-08-16 by an operator opening the screens
while I watched the daemon log. Four defects in one sitting, and the useful part
is that **not one of them was reachable by any test in the repo**.

#### What was confirmed working

Worth recording, because a pass that only lists faults reads as a broken build.
The neighbour card rendered exactly as designed — `oonfee-roam` named, "4
already correct", both APs, `knows 3 neighbours` on every BSS. The unmeasured
class explained itself on the C6 and stayed a bare `A` on the WRT. Both
re-probes reported `neighbor-report` present and no changes on a second run.
**The sticky header stays pinned when a long grid scrolls** — the defect that
was silently broken for the entire life of the project once (§5b), checked by
eye because happy-dom has no layout engine and never will.

And the daemon log carried **zero warnings or errors** for the whole session,
which is the half of this that a screenshot cannot show.

#### 1. The grid people look at most could not be reordered

Reported as "I tried to drag the column header, nothing happens" — on Devices,
which was the one `DataGrid` with no column preferences. Clients and Logs had
them. Without `onPrefsChange` the header is not `draggable` at all, so there was
no reorder, no picker, and not even the tooltip that says dragging is possible.

**Why it survived a suite that tests dragging.** Every drag test fires
`fireEvent.dragStart` directly, and fireEvent dispatches the event whatever the
DOM says. They all passed against a header a real mouse could never pick up:
they proved the *drop handler* worked and said nothing about whether a drag can
*start*. §5l predicted this in so many words — *"nothing here says whether a drag
actually starts"* — and then it happened anyway, which is the difference between
naming a gap and closing it.

`draggable` is asserted directly now, in both directions. A grid that cannot
reorder must not advertise that it can, so the absent case is pinned too.

#### 2. "Radios" was listing BSSes

The device panel iterated `stats.aps` — one row per broadcasting interface —
under a heading that said Radios. On a two-radio AP carrying two SSIDs that
rendered **four radios**. Two rows read `oonfee-roam` and were distinguishable
only by a channel number. And the airtime figure appeared **twice per radio**.

That last one is the §5h shape again: one quantity presented as two
measurements. Channel occupancy belongs to the channel, and every BSS sitting on
it reports the same number correctly — but printing it once per BSS invites the
reader to believe two radios were measured. It reads *"channel 1 is 58.0%
busy"* now, once per interface, with the interface named.

#### 3. "Packages installed: none" was true and unreadable

It means packages **the controller** installed — always none, reported rather
than omitted precisely so ARCHITECTURE §0's "we install nothing on your router"
can be checked instead of believed. The tooltip said so. Nobody hovers. Under
that label the value was a claim about the *device*, which for any real router
is plainly false, so the field looked broken. Now "Packages we installed".

The general shape: **a correct value under a wrong label is a wrong readout.**
The `packages_note` was doing real work and reaching nobody.

#### 4. The keyring passphrase has no recovery path, and it showed

Not a UI defect, and the most operationally serious of the four. Starting the
daemon by hand prompted `Operator passphrase:` — the key that unseals the device
credentials — which was generated in a previous session and exists only in the
session scratchpad. There is nothing an operator could type. The prompt gives no
hint that a file is the intended path, and the failure is a flat "the passphrase
is wrong, or the keyring file has been corrupted".

Nothing was lost here (both devices re-adopt over SSH), but the shape is worth
naming: **the only copy of the key to every device credential lives in a
directory that has already been wiped once during this project.** §7 documents
`-passphrase-file`; the running daemon does not mention it. A prompt that cannot
be answered should say what would answer it.

### 5w. Mesh backhaul health

§5m called this "the first thing worth building once there are two nodes", and
it was the last thing the mesh feature was missing: the controller could
configure a backhaul and could see that an interface was in mode `mesh`, and had
no idea whether anything crossed it.

Designed with a fan-out of readers and an adversarial judge panel, then built
by hand from the synthesis. Two of its claims were checkable and both were true;
one of its judgements was overridden. What follows is what survived contact.

#### The deliverable is a closed vocabulary, not a struct of nullables

Thirteen states, one per way of being right or wrong, decided once in
`internal/meshlink` and switched on at the render boundary. The alternative —
peer count, interface up, capability present, handed to a screen that decides
what they mean together — has failed twice in this project already: a count
computed from whatever happened to be loaded (§5b), and one question answered
two ways on two screens (§5h). A UI that re-derives health from nullables is a
second implementation of this logic, and two implementations drift.

**The order of the ladder is the design.** Every rung is reached only when the
ones above did not apply, so the first state that matches is also the first
thing worth doing something about. A device whose driver will not run a mesh
must never be described as having zero peers: the count would be true and the
sentence useless, because the thing to fix is three rungs earlier.

Two judgements worth defending:

- **A count without peer-link state is its own state**, not a healthy one. It
  cannot distinguish a working backhaul from one stuck mid-handshake.
- **Zero peers on the only node of a mesh is toned down.** On the one
  mesh-capable device here it is correct and permanent, and rendering it red
  forever is precisely how a screen teaches people to ignore red.

#### It costs no device request, and that is why it works this way

| fact | source | cost |
|---|---|---|
| can this device carry a mesh | capability record via `render.MeshGate` | none |
| was one applied to it | `owned_sections` (applied AND confirmed) | none |
| which interface it is | `getWirelessDevices`, already on the 15-min slot | none |
| is it up | `network.device status`, already call #2 of every poll | none |
| how many peers, and their link state | `iw dev <if> station dump`, 15-min slot | one exec |

The liveness row is the one worth staring at: **`phy0-mesh0 state DOWN` has been
arriving in every snapshot this project has ever taken.** §5q went looking for
it over SSH. Only the join was missing.

`owned_sections` is load-bearing rather than an optimisation. Observation alone
can never distinguish a mesh whose interface the driver refused to create from a
device nobody asked to run one — so without the record of what was applied,
§5q is unreportable in principle.

#### Where the design was overridden

DEVICE-BUDGET disagrees with itself: §3.2's rule says `file.exec` belongs "at the
slow-loop interval, never the fast one", while its feature table lists
`iw station dump` as focused-rate. The synthesis resolved toward the table,
gated on re-running the budget harness. This resolves toward the rule, and the
gate becomes unnecessary: a mesh peer appears when somebody unplugs a node or a
link finally establishes, not on the timescale of somebody watching a screen.

`iwinfo.assoclist` is **deliberately not used** even though it is already
granted, returns the same peers as structured JSON, and needs no process spawn.
It carries no `mesh plink`. That single field is the entire difference between a
count and a health reading.

#### The parser is written against a capture, not against the format

`iw station dump` output was taken verbatim from the C6 on 2026-08-16, and it
contains something nobody would have guessed:

    inactive time:	7700 ms          <- key, tab, value
    signal:  	-37 [-37, -47, -77, -77] dBm
    beacon interval:100              <- NO whitespace at all
    short slot time:yes

A parser splitting on `":\t"` drops every field the device chose not to pad —
and on a device that formats `mesh plink` that way, it would drop the one field
the whole judgement turns on. Splitting on the first colon and trimming both
halves is the only shape that reads both.

#### Four bugs, and only one was findable without hardware

1. **An apply invalidated the interface cache immediately**, and the refetch
   landed in the four to six seconds *before* the new interface exists. It
   cached the absence and held it for the full 15-minute cadence — so every
   successful mesh apply would have shouted `interface-absent`, critical, about
   a backhaul that came up fine two seconds later. A second, delayed re-read
   fixes it. §5r's lesson arriving for the third time: **an apply returning is
   not a radio being ready.**
2. **The peer exec was gated on `needIfaces()`**, and the mode map that says
   which interface is a mesh comes *from* that fetch. The poll that learns about
   the mesh has not got the modes yet; the poll that has them is not re-reading.
   It could never fire. Its own cadence now.
3. **The exec iterated `iwinfo.devices`, which did not list the live
   `phy0-mesh0`** — measured — while `getWirelessDevices` did. Two sources of
   "which interfaces exist" disagree, and only one of them knows about meshes.
   §5o chose `getWirelessDevices` for modes and this had quietly chosen the
   other list for the same question.
4. **"Expect a peer" counted devices in the group**, so the C6 sat at warning
   permanently because the WRT is in that group and cannot run a mesh at all.
   It counts devices that could actually *carry* one now. Left alone, the
   readout would have committed the exact failure it was built to avoid.

#### What is verified, and what cannot be

Verified on both devices, end to end through real polls: the WRT reaches
`not-buildable` carrying the driver's own sentence — §5q's gate working from the
health side rather than the render side — and the C6 reaches a live
`phy0-mesh0`, then a **demonstrated** zero peers, toned normal with the reason.
The test waits for the reading to settle rather than for a fixed time, and
starts the collector *before* the apply on purpose: started afterwards there is
no stale cache to invalidate and it would pass on a lucky ordering.

**Not verified, and not verifiable here: any peered state.** Mesh is Present
only on the C6 and gated off on the WRT, so nothing in this project has ever
watched two nodes find each other. `peered`, `peering` and `plink-unknown` are
unit-tested against constructed input and have never met hardware. That needs a
third device or a WRT replacement, and until then the most interesting half of
this ladder is theory.

### 5x. The wireless uplink (WDS), and what the hardware said about it

The last unmodelled piece of the stated direction: the router in the room with
no ethernet run to it. A mesh covers that when both ends can carry 802.11s;
this covers it when they cannot — which, measured, includes one of the two
devices here (§5q).

Built end to end in one pass: capability, model, renderer, store (migration 8),
API, and a screen. Then reviewed adversarially, then met hardware. Each of those
three stages found something the previous one had not, and the hardware finding
is the one that decides what this feature is worth.

#### The modelling decision

**A device property plus a WLAN flag, not a Bridge object.** The two ends are
not symmetric and do not belong to the same owner: the AP end is a property of a
network ("this one accepts wireless bridges") and applies to every AP publishing
it; the station end is a property of one device ("this one has no cable"). A
Bridge object forces an operator to describe a relationship where the real
decision is two independent facts, and breaks the moment a second device wants
to join the same way.

Credentials are never restated on the uplink — it references a WLAN, so the
SSID, passphrase and security mode live in one place. Same rule `override.go`
enforces, same reason: a bridge whose key drifts fails the way a client with a
stale password fails.

#### Two hazards the controller cannot check, so it says them

**The loop.** A station bridged into `br-lan` on a device that is ALSO cabled is
a layer-2 loop, OpenWrt bridges ship with STP off, and the symptom is a network
that stops working rather than an error — §5g's shape exactly. The controller
cannot see the far end of a cable and does not pretend to: it states the
condition on the preview and on every API response, and leaves it to whoever can
see the room.

**Removing one is editing the road while driving down it.** On a device with no
cable the station IS the route. `applyengine.IsUplinkSection` makes pruning it
count as touching the management path, so it needs an explicit acknowledgment
rather than going through as an ordinary wireless change.

#### What the review caught, and what it cost to ignore one finding

Four lenses, two skeptics per finding. Two criticals, both real, both latent —
nothing could create an uplink yet, so they would have gone live in the very
next commit:

- **`Site.Validate` never iterated `Uplinks`**, so every sentence
  `Uplink.Validate` produces was unreachable. §6's guard that cannot fire.
- **`TouchesManagementPath` covered only `network` and `firewall`.** A wireless
  section was never a management path — until an uplink, where it is the only
  one. Matched on the section name now rather than a caller-set flag, with the
  coupling pinned by a test from render's side, because applyengine cannot
  import render.

And one finding I read and did not act on: *what if an uplink names a device
that also serves that WLAN?* Hardware charged me for it within the hour.

#### What hardware found

**Turning the AP half off did not turn it off.** The renderer omitted `wds` when
the flag was false, and a plan compares only the keys it writes — so whatever
was last applied stayed. Measured: after switching it off and applying, both
access points still carried `wds='1'`. An AP still accepting 4-address frames
while the screen says it does not is a security posture nobody chose. It writes
`"0"` explicitly now, as `ft_over_ds` three lines above it already did. **My own
test asserted the buggy behaviour** — it checked the key was absent, which is
what the code did rather than what it should do.

**A device cannot bridge to a network it publishes.** The C6 was in the AP group
serving `oonfee-roam` and told to join it, so a station came up on the radio
already carrying that SSID and sat at channel 0. Refused now rather than warned
about, because nothing in that config looks wrong.

#### And the finding that decides the feature's status

**Station mode does not work on the Archer C6 at all.** Not 4-address; not the
controller. Isolated three ways before being written down:

| ruled out | how |
|---|---|
| the controller | a hand-written UCI section fails identically |
| 4-address framing | fails the same with `wds` removed |
| AP/STA concurrency | fails with every AP on that radio disabled, station alone |

The interface comes up in Client mode at channel 0 with 0 dBm, `iw link` says
"Not connected", and a scan returns **zero BSSes**. Every signal a controller can
consult says it should work: `wpad-mesh-openssl` installed, `wpa_supplicant`
running with a control socket open for that interface, and `iw phy` declaring
`#{ managed } <= 16` alongside the APs on one channel.

That is §5q's shape on a second driver — advertised, accepted, refused.

**Deliberately NOT recorded as a Quirk.** A quirk gates the feature off, and one
board is not a driver: this could be the board, that firmware build, or ath10k
generally, and those three send an operator to three different places.
`FeatWirelessUplink` stays Present from the package list, and the note now says
what Present means — *the software is there, worth trying* — and **describes how
it fails**, because a station that comes up and never associates looks like a
dozen other problems and cost an afternoon here.

#### Status

Complete in code, tested from unit through API and UI, and **unproven on this
hardware for a sharper reason than mesh**. Mesh needs a second mesh-capable
node; this needs a device whose radio will run a station at all. The feature
rests on the assumption that some OpenWrt device can, which is well founded and
now explicitly untested here.

### 5y. Asking the air, and the adoption bug that fell out of it

§0 records a WRT3200ACM that beaconed `wrt-cleanroom` — an SSID present in no
configuration anywhere on the device — for about fourteen hours while
`/etc/config/wireless`, hostapd's running conf, `iwinfo`, ubus **and the
kernel's own `iw dev info`** all reported `oonfee-roam`. Every verification this
controller had was on the wrong side of the driver.

`internal/onair` is the answer: a second radio. A beacon is a physical thing and
cannot be produced by a stale config or a confused daemon, so the fleet
cross-checks itself — each device scans, and what it hears is compared against
what the others claim to broadcast.

**Verified on both APs, 2026-08-16:** six BSSes, every one confirmed by the
other device's radio, zero faults. The WRT's two witnessed by the C6, the C6's
four witnessed by the WRT.

#### Almost all of the design is about not crying wolf

A fleet that lights up red is a fleet nobody looks at, and access points placed
for coverage routinely cannot hear each other. So the negative state is
`Unheard`, never `Absent`, and four measured or known reasons a scan misses a
live BSS are enumerated in the package comment — including one from this lab:
**the C6's 2.4 GHz radio returned 20 BSSes while its 5 GHz radio, serving an AP,
returned zero.** Not a quiet band; a scan that never happened. `BandsCovered`
exists so that silence cannot become evidence.

Exactly one combination is ever a fault: a BSSID another radio **did** hear, on
a band that **was** scanned, broadcasting a **different SSID** than its own
device claims. No distance or timing story explains that away.

It is operator-initiated and on no timer. A scan takes a radio off-channel,
which whoever is using the network feels — unlike the capability probe, which is
merely expensive.

#### The bug it exposed, which is bigger than the one it was built for

`probeRadios` enumerated `iwinfo.devices` — which lists **broadcasting
interfaces, not radios**. A device with no `wifi-iface` therefore recorded
**zero radios**, and the renderer then refused to give it one ("device has no
5g radio"), so it could never get an interface for the radios to become visible
through.

**Stock OpenWrt ships its radios disabled.** So this hit every freshly adopted
router: the controller could not bring a stock device into service at all,
which is this project's entire stated direction. The C6 only ever worked
because its radios had been enabled by hand before adoption — the one accident
that hid this for the life of the project.

Measured on the WRT with two working radios and no WLAN:

| source | says |
|---|---|
| `iwinfo.devices` | `[]` |
| `luci-rpc.getWirelessDevices` | radio0 (5g, ch36) and radio1 (2g, ch1), both up |
| `/sys/class/ieee80211/` | phy0, phy1 |

The radio list now comes from `getWirelessDevices`, which is keyed by radio and
answers when nothing is broadcasting, and which the ACL already granted. iwinfo
still supplies the per-interface detail where an interface exists.

**Two corrections fell out of it, both the capability model's own cardinal error
reached sideways.** With no radios to inspect, the Marvell mesh quirk could not
fire, so `FeatMesh` flipped from correctly-Absent to **Present** on a driver that
demonstrably will not run a mesh point — an unrunnable check reported as a clean
bill. It records NotObservable now. And a radio with no interface has no
frequency, so `radiosByBand` skipped it entirely; it falls back to the
configured band, because "this device has no 5 GHz radio" about hardware sitting
right there is a claim no apply could ever fix.

End to end: the WRT went from *"device has no 2g radio"* to `applied (2 changes)
health passed and confirm landed`, with `oonfee-roam` on both radios.

### 5z. The adoption bug, pinned — and what a real roam exposed on the way

**Done 2026-08-16.** §5y's bug was proven only by hardware and had no test. It
now has two, one per half, because **either half alone would have hidden it**:
the probe must find radios on a device with no interfaces, and the renderer must
then give those radios a WLAN despite their having no frequency. Both are
mutation-verified — putting each bug back reproduces its own symptom and no
other (`"a device with disabled radios reported none at all"` /
`"got 0 section(s)"`).

Writing the test found the fixture had the same blind spot **in two places**,
which is why no test had ever caught this: `iwinfo.devices` was a hardcoded
constant that could not go empty, and `WIRELESS_DEVICES` carried no radio-level
config at all — no `band`, no `channel`. Those live on the *radio*, and they are
the only facts that survive a device having no interfaces. **A fixture that
cannot express the broken state cannot catch the bug.**

#### A real roam, and the grid that could not say where the client went

A client roamed C6 → WRT under 802.11r while the monitor was running
(`AP-STA-CONNECTED ... auth_alg=ft`). The obvious next question — does the
Clients grid now say WRT? — had no confident answer, and chasing it found a bug
with nothing to do with roaming.

`recentStations` scanned every station rollup flat and let the last row win. Two
APs report the same MAC in one five-minute bucket on every roam, and also
whenever an operator opens two device pages within five minutes, since a focused
poll is the only thing that produces station telemetry at all. **Whichever
collector wrote second took the client.** Proven rather than argued: hold both
readings fixed, reverse only the write order, and the client moves to the other
AP. The same grid, refreshed, could relocate a stationary client.

Two more consequences came from the same scan. Each field was overwritten
independently, so one row could carry **one AP's identity, a second's signal and
a third's retry rate** — three sources, one plausible-looking row, undetectable
from outside. And a retry row could carry the attribution alone, so an AP with
no RSSI reading for a client could still be named as the AP it was on.

The AP is now chosen in SQL, per MAC, before any metric is read: newest bucket,
then strongest signal, then `device_id` purely so a real tie is stable. Retry is
excluded from the ranking — a retry percentage says nothing about which radio a
client is near.

#### Measured, so nobody chases it again

**mwlwifi logs two errors on every successful FT association** and they are
noise:

```
phy0-ap0: nl80211: kernel reports: key addition failed
nl80211: NL80211_ATTR_STA_VLAN (addr=… ifname=phy0-ap0 vlan_id=0) failed: -2
```

Both land in the same second as the association. The client was then
`authorized/authenticated/associated` and moved **539 KB in 93 seconds**, so the
key is installed and traffic flows. Checked rather than inferred, because "key
addition failed" reads exactly like the cause of a connects-but-no-traffic bug.

#### The monitor gave two false signals before it gave a true one

Both worth stating, because a watchdog that lies is worse than none:

1. It counted **any** `daemon.err` as a driver fault, so it reported a FAULT for
   the successful roam above. It now matches the actual wedge signature
   (`nl80211_recv_beacons->nl_recvmsgs failed: -5`, or hostapd in D state).
2. It held the ssh command in a string and ran `$S …`. **zsh does not word-split
   an unquoted parameter**, so the whole command line became one command name,
   every probe returned empty, and the monitor reported the router UNREACHABLE
   while it was answering fine. A false "down" is the same failure as a false
   fault.

The WRT has now run **49 minutes with a clean signature**, past the ~28-minute
mark it wedged at before.

### 5aa. The WRT failure, diagnosed — it is the 5 GHz firmware

**Caught live 2026-08-16, 52 minutes after a clean boot.** The user asked to
watch rather than act, and the watch paid: this is the first time the failure
has been observed from the inside while it was happening.

**The causal chain, measured in order:**

```
22:08:21  ieee80211 phy0: cmd 0x801d=MEMAddrAccess timed out   <- CAUSE
          (return code 0x001d, then every ~20s, forever)
22:09:01  nl80211: nl80211_recv_beacons->nl_recvmsgs failed: -5  <- 40s later
          hostapd (pid 1793) in D state, ubus silent
```

`MEMAddrAccess` is the mwlwifi driver failing to reach the **88W8964 firmware on
phy0**. Everything previously treated as the fault is downstream of it: the
netlink `-5` is `EIO` from a driver that cannot reach its firmware, and hostapd's
uninterruptible sleep is it blocking on that driver.

**The blocking is global, not per-radio.** A bounded probe (watchdog per call,
so a hang is measured rather than inferred):

| call | result |
|---|---|
| `iw dev phy1-ap0 station dump` (2.4 GHz, first call) | **completed, 1s** |
| `iw dev phy0-ap0 station dump` (5 GHz) | **blocked** |
| `iw dev phy1-ap0 info` (2.4 GHz, after the above) | **blocked** |

phy1's firmware is healthy — it answers until a phy0 call is outstanding, and
then it does not. nl80211 operations serialise, so one stuck phy0 command holds
the lock against every radio. `kill -9` does not release it; D state is
uninterruptible. **One hung radio takes the entire wireless control plane with
it, including the working one.**

#### This explains the 14-hour lie

§0's most confusing event — the WRT beaconing `wrt-cleanroom`, an SSID in no
config anywhere, while `/etc/config`, the hostapd conf, `iwinfo`, ubus and `iw
dev info` all said `oonfee-roam` — now has a mechanism. The control plane
accepted and reported the new config; **phy0's firmware was hung and never
applied it**, and kept transmitting from the last configuration it had actually
loaded. Every reader was telling the truth about what it had been told. Only the
air knew. That is precisely the gap `internal/onair` (§5y) was built to close,
and it turns out to have a specific, reproducible cause rather than a mystery.

#### A correction to §0

§0 states the trigger was caught directly: `STA … deauthenticated due to
inactivity`, then `-5` 66 seconds later. **That does not hold.** In this
occurrence there was no deauth at all — the last wireless event was a routine
`STA-OPMODE-*` change 8.5 minutes earlier, then silence, then the firmware
timeout. The deauth was a coincidence in one sample. The honest statement is
that **no trigger has been identified**; what is now known is the failing
component and the order of collapse.

Timing is not fixed either: the earlier wedge was ~28 minutes in, this one 50.

#### What this rules out

No ubus call causes it (already disproved by controlled repeat, §6). Nothing the
controller does can prevent it, and nothing it does can recover it — the recovery
is below the level any management protocol reaches. **This is not a software
problem this project can fix.** The device is a firmware-faulty AP and should be
treated as one: useful as a hostile test subject, not as a reference.

#### The watchdog was wrong three times before it was right

Kept in `tools/wrt-wedge-watch.sh` now, with all three fixed:

1. Counted **any** `daemon.err` as a fault, so it fired on a successful 802.11r
   roam (§5z).
2. Held the ssh command in a string and ran `$S …`; **zsh does not word-split an
   unquoted parameter**, so every probe returned empty and it reported the
   router UNREACHABLE while it was answering.
3. Matched D state on `$3` of busybox `ps w`, which is the **VSZ** column, not
   `STAT` (`$4`). It printed `hostapd_D=0` throughout, while hostapd was in D
   state the entire time. A watchdog reading the wrong column reports healthy
   through the exact failure it exists to catch.

It now watches `MEMAddrAccess timed out` as well, which is the earliest signal —
40 seconds ahead of the netlink error and a full minute ahead of anything a user
would notice.

#### Second capture, and two refinements

It wedged again **17 minutes after a clean boot** — the interval is shortening
(~28 min, ~50 min, ~17 min), which matches what was seen before the factory
reset. All **10** firmware timeouts were on `phy0`; `phy1` recorded none and
still went unreachable with it, reproducing the global-blocking result exactly.

Two things the second capture corrected:

- **A single D-state sample proves nothing.** The watchdog fired on
  `hostapd_D=1` while the device went on to serve traffic for another two
  minutes — the daemon distributed neighbour lists successfully 14 seconds
  later. `D` is a normal momentary state for any process in a blocking syscall.
  It now requires D across five samples in five seconds; a wedged hostapd never
  leaves it.
- **A firmware timeout is not instantly fatal.** Two `MEMAddrAccess` timeouts
  fired and the radio kept working. By ten, both radios were blocked. So the
  useful signal is a *rate*, not a first occurrence — worth knowing for anything
  that tries to detect this generally.

#### Corrected by the driver source: MEMAddrAccess is the detector, not the cause

A research pass over the mwlwifi source (not the bug tracker — the code) makes
one thing in this section wrong.

`cmd 0x801d=MEMAddrAccess timed out` **is the driver's own heartbeat probe
failing.** `mwl_heartbeat_handle()` calls `mwl_fwcmd_get_addr_value()`, which
issues `HOSTCMD_CMD_MEM_ADDR_ACCESS` (`0x001d`), and the wait is on the response
`0x8000|cmd` = `0x801d`. So a repeating `0x801d` at a fixed interval is the
watchdog **confirming the firmware is already dead**, not the thing that killed
it. Two consequences: it is still the most reliable confirmation, and its
*absence proves nothing*, because the heartbeat only runs when `priv->heartbeat`
is non-zero.

#### And a correction to §5z: the key error was not noise

§5z recorded the two errors mwlwifi logs during an FT association as benign,
because the client went on to move 539 KB in 93 seconds. The traffic
measurement was right. **The conclusion drawn from it was wrong.**

Both wedges were preceded by a key-install failure, on the same client, at an
802.11r association:

| | key-install failure | first heartbeat timeout | gap |
|---|---|---|---|
| wedge 1 | 21:57:48 | 22:08:21 | 10.5 min |
| wedge 2 | 01:04:41 | 01:06:06 | **85 s** |

Then, once the radio was already gone, the encryption command itself started
timing out — `cmd 0x9122=UpdateEncryption timed out`, `failed to remove key (0,
36:e0:c7:4f:d0:fb) from hardware (-5)`, `cmd 0x9111=SetNewStation timed out`.
Four independent bug reports on the mwlwifi and OpenWrt trackers describe the
same ordering.

Two for two, on the same client, in the same code path, is correlation and not
proof. But it is the **only signal that arrives while the radio still works**,
and it is now what the watchdog leads with.

It also settles §0's original trigger claim in the other direction. The
`deauthenticated due to inactivity` line does appear in wedge 2 — at 01:11:13,
**five minutes after the radio was already dead**. It was a consequence the
whole time.

#### A hypothesis this makes testable, and worth testing

The failing path is key installation during an 802.11r association, and
oonfeeWRT enabled **both** 802.11r and PMF on this hardware — with `ieee80211w=1`
rendered onto a board whose own OpenWrt page says not to enable 802.11w at all.
That does not make the config the cause. It does mean the obvious experiment has
never been run: **turn PMF off on this device and see whether it survives longer
than 50 minutes.** Nothing else about the deployment needs to change.

#### Verified in passing: the neighbour reconciler self-heals a rebooted AP

A reboot clears runtime `rrm_nr` state, and **a device reboot is not one of the
three nudge triggers** (adopt, unadopt, apply). So the only thing that could
restore it was the periodic cycle reading `rrm_nr_list` back and noticing it was
empty — the "reconciles rather than applies" claim of §5t, which until now had
only ever been tested against an apply.

It held. The WRT came back with `0/0` neighbours and was refilled to `3/3` at the
next 15-minute cycle, **with no apply and no adoption**.

The collector's recovery held too, and its silence was misleading rather than
wrong: `fail()` logs at Warn only on the *first* consecutive failure and at Debug
after, so a device that stays unreachable produces one line and then nothing at
INFO. It had been retrying at the 10-minute capped interval for two and a half
hours, exactly as `DefaultMaxInterval` documents, and picked the device back up
within a minute of its return without a restart.

### 5ab. Known-hardware-defect warnings

**Done 2026-08-16.** oonfeeWRT rendered `ieee80211w=1` onto a WRT3200ACM for
weeks. OpenWrt's own page for that board says plainly **not** to enable 802.11w,
because mwlwifi does not support it properly and it is off by default there for
that reason. The device accepted it without complaint, and nothing anywhere
would have told the operator.

That is a failure the capability model cannot reach. The three-state model asks
the device what it can do, and a driver broken in this particular way answers
**yes** — it takes the config, reports success, and does not work. `Quirk` covers
the narrow case of one field that is present and wrong; this is wider, a property
of a driver that no probing will reveal because the device does not know it is
broken.

`internal/capability/defects.go` is a small sourced registry matched on the
radio's reported hardware string. Two rules:

- **Warn, never rewrite.** A controller that silently downgrades the security
  settings a user asked for is worse than one that says what will not work and
  why. Auto-remediation would also make the defect invisible, and an invisible
  workaround becomes folklore the moment the driver is fixed. The test asserts
  the config still renders with the operator's PMF value untouched.
- **Say how well it is known.** Every entry carries `documented` / `measured` /
  `reported` / `anecdotal` and a Source, and the UI shows both with a tooltip
  explaining each — so folklore is never shown with a maintainer's authority,
  and a warning that goes stale can be traced and deleted rather than repeated
  forever.

Warnings are split by where the operator can act: config-triggered defects at
render time (on the **rendered** values, so it catches what the renderer derives
— WPA3 forcing PMF on is exactly that), radio-state defects against the device,
and defects no configuration causes once at adoption, while someone is still
deciding whether to build on the hardware.

#### The research pass was worth more for what it killed

A fan-out over the driver source and trackers produced 90 claims; **7 of 8
non-anecdotal ones were refuted** on adversarial re-check — including one traced
to a ticket that had been fixed upstream eight years earlier ("the claimant read
the ticket, not the driver repo"). Only entries traceable to the device's own
OpenWrt page or to measurements here were shipped. `irqbalance`, the most-cited
workaround for this board, is not even installed on the reference device.

The one claim that survived is now in the entry: **mwlwifi has no firmware
recovery path.** A timed-out host command logs, sets `cmd_timeout`, returns
`-EIO`; nothing resets the chip, and firmware is re-downloaded only on PCI
probe — which is why no `wifi` restart or re-apply can recover it. Driver-wide
across 88W8864/8997/8964; the hang is what the 8964 does in the field.

And the refutation caught a piece of folklore in the *fix*: it recommended
`rmmod mwlwifi; modprobe mwlwifi`, checked STATUS.md, and found this project had
already measured that leaving `modprobe` hung with no radios at all and still
needing the reboot. The registry now warns against it. A registry whose job is
to stop people acting on folklore must not ship any.

#### Two bugs in it, found by review within the hour

- **A clean bill from a check that never ran.** `Hardware` comes from
  `iwinfo.info`, which only answers for a radio that has an **interface** — so a
  stock OpenWrt router matched nothing and got silence. Same root cause as the
  §5z adoption bug, on the same devices. `HardwareIdentified()` separates them
  and both the preview and adoption now say the check did not run.
- **A guard that could not fire.** The DFS entry read `channel` from a
  wifi-iface, which never carries one — the renderer emits no `wifi-device`
  sections at all. Defects about the radio's current state now get
  `TriggersRadio` and are evaluated against the device.

### 5ac. Foreign SSIDs: the takeover brief, and three defects in the badge

**Done 2026-08-16.** A user noticed the Archer C6 broadcasting `oonfee-c6-2g`
and `oonfee-c6-5g` and asked why oonfeeWRT did not manage them — "wouldn't it be
better if all SSIDs were managed?"

**The default is right, and the reason is worth stating plainly.** A section is
managed **iff** this controller wrote it and can put it back. That is what makes
un-adopt a promise rather than a hope, and what stops a bug here from eating
config a human made by hand. Widening "managed" to mean "an SSID I have opinions
about" would not manage more; it would make the word stop meaning anything.

#### The panel killed the automated import, twice

Three designs were generated and judged adversarially. **Both automated import
designs failed the same way**: each confirmed its own irreversible step with a
health check that could not see what it claimed to prove. One would have let
un-adopt **delete a network the operator had before oonfeeWRT existed**, with the
restore "confirmed" by a check that short-circuits when the render contains
nothing to look for. The other gated on a config read — which §5y already
established cannot tell you what is on the air.

So the controller prints the recipe and runs none of it. Four properties, each a
test rather than a sentence:

| property | why |
|---|---|
| no passphrase field, and no field saying whether one exists | redaction as a property of the TYPE; the test marshals the whole response and greps the bytes for the C6's real key |
| nothing but `mode='ap'` gets disable advice | a station or mesh iface may be the device's only path to the network; unknown mode refuses too |
| the recipe ends in `wifi reload` | `uci commit` writes the file and does not take a BSS off the air |
| the cost names the OTHER devices | there are no per-device WLANs, so recreating a foreign SSID starts it on every AP in the group |

The cheaper half is a recorded decision: `foreign_ssid_notes` holds a note
**about** a section and never a copy of one — nothing to leak, and nothing that
could later be restored over whatever the operator has since done to their device.

#### Three defects in the badge that shipped an hour earlier

Found by review before a user hit them, and all three are §6 entries:

1. **It answered the wrong question.** `managedSSIDs` compared the SSID *string*
   against the site model, so creating a WLAN named `oonfee-c6-5g` would flip the
   still-foreign, still-broadcasting BSS to "managed" and withdraw its warning —
   while the controller still did not own the section. My own comment called it
   "the honest approximation".
2. **Two sources joined by a string** — the unmanaged set came from the REST
   detail and was rendered over the live stats list. The §6 practice written that
   morning, broken the same afternoon.
3. **The explanation lived in a `title` tooltip.**

Provenance is now keyed on the **UCI section**, three states. `ProvUnknown`
covers a device whose ACL refuses `getWirelessDevices`: calling an operator's own
SSID foreign for want of asking is the worse error. The test that matters carries
a foreign section whose SSID is *identical* to a managed one.

#### Verified in a browser, on the lab hardware

The whole chain, seen rather than asserted: the C6 reports `section` and
`mode='ap'` for all four interfaces; the panel marks both `default_radio*`
unmanaged, names the section, and offers the recipe including the fan-out warning
*"ap-192-168-1-1 would start broadcasting it too"*; the note round-tripped to the
database attributed to `admin` and cleared cleanly.

**And opening a device populated the Clients grid for the first time.** Focusing
an AP produced the first `sta_rssi` series this deployment has ever held, and the
grid attributed the iPhone to `ap-192-168-1-1` at −81 dBm. Checked against
hostapd on both APs: the iPhone is indeed on the WRT. The Watch, on the C6, still
shows a dash — correct, because no focused poll has covered it yet.

### 5ad. The browser pass that closed §5 item 4

**Done 2026-08-16.** Four defects, none reachable by any test in the repo. The
count is now **twenty-three found by looking**.

- **The 802.11k card showed nothing until you made it happen.** It renders the
  last distribution only after somebody presses "Distribute now" — on a feature
  whose own text in that same card says it runs every fifteen minutes. So on
  arrival an operator could not tell whether 802.11k was working, and the only
  way to find out was to trigger it, which is not an observation. Every
  automatic cycle that had run all day left no trace anywhere a user looks. The
  daemon now remembers its last cycle and `GET /roaming/neighbours` reports it
  **without running one** — the test asserts that reading does not trigger.
- **The event log never said which device an event was about.** Every device
  event carries a `device_id`, the API has always returned it, the UI type has
  always declared it, and the grid had no column for it. Not hidden behind
  Customize columns: absent. `device.unreachable` told you something was
  unreachable and not what.
- **A whole serialised array in one table cell.** The Detail summariser
  `JSON.stringify`d anything object-shaped, so a `config.apply` event put its
  omissions — each a full sentence of prose — into a single cell, which ran off
  the screen and forced a horizontal scrollbar. Lists are counted now:
  `omissions=2 items` says there is something to look at without pretending the
  cell can hold it, and the count can never quietly drop the fact that more
  exists.
- **Two networks rendered as one token.** The discovery plan separated CIDRs
  with a CSS margin, so the DOM said `192.168.1.0/2410.7.42.0/24` — a gap made
  only of CSS disappears in copied text and in a screen reader.

The wireless-uplink card, the per-device override card and the adopt form all
read correctly. The discovery card's "2 things not scanned" disclosure — tunnel
interfaces and IPv6 — is exactly the kind of honesty this project is for.

### 5ae. The adversarial review of a day's work

**Done 2026-08-17.** A day that produced ~4,700 lines had been reviewed only by
the person who wrote it. Four dimensions reviewed the diff independently, then
every non-anecdotal finding faced a refuter told to default to "refuted".

**30 candidates, 6 survived.** A 20% survival rate is roughly what a review that
is not merely agreeing with itself should produce. **Four of the six were bugs
introduced that same day.**

#### The one that mattered most was not a bug

`TestTheTakeoverBriefNeverCarriesThePassphrase` hardcoded the lab C6's **actual
pre-shared key**, read off the device with `uci get`, with a comment saying so.
It went into a **public** repository — in the test whose entire subject is that
passphrases do not leak, and against a rule this project had already written
down. Removing it from the tree does not unpublish it; the key has to be rotated
on the device. Nothing about the test needed a real secret.

#### The two high-severity findings

**A URI-parsed database path.** The pragma fix earlier that day prefixed the path
with `file:`, which makes SQLite parse it as a URI rather than a filename.
Measured, one directory per case:

| path | plain | with `file:` |
|---|---|---|
| `/tmp/x` | opens x | opens x |
| `/tmp/has#hash` | opens it | **opens a different file, no error** |
| `/tmp/pct%20name` | opens it | `unable to open database file (14)` |

The `#` row is silent: a data directory containing one would bring the controller
up on a fresh empty database, migrate the whole schema into it, and report zero
devices while the real database sat beside it untouched.

**A BSS cache that could never go empty.** It was written only when the AP list
was non-empty — a proxy for "this poll asked", and wrong in both directions. A
device broadcasting nothing could never record having been looked at, so the API
answered "no poll has looked" about a device polled hundreds of times; and a
removed SSID stayed reported as on the air indefinitely, including the one the
takeover brief had just told an operator to remove. `Snapshot.APsFresh` follows
`IfacesFresh` and `NetDevsFresh` — the same rule this package had already written
down twice.

#### The trap in an obvious fix

A driver defect matched on one radio was applied to every radio: on mixed silicon
a WLAN on an Atheros radio was accused of Marvell defects, and the DFS warning
fired for a 2.4 GHz Marvell radio that cannot be on a DFS channel at any value.

**The obvious per-radio filter silences a real warning.** A homogeneous Marvell
board whose second radio has no interface reports `Hardware ""` — the §5ab case —
and filtering it out is the cardinal error by the same road. `MayAffect` excludes
only a radio *known* to be a different chip. Warning about the wrong chip is
noise; going silent about the right one is not.

Also fixed: a BSS the detail response did not mention rendered identically to one
we manage, and a channel-keyed defect judging a snapshot frozen at adoption while
the check beside it read live config.

#### The PMF experiment, five hours in

`ieee80211w=0` on both radios since 2026-08-16 21:00. The WRT has now run
**5h23m** against previous runs of **17, 28 and 50 minutes** — with **zero**
firmware heartbeat timeouts, where it previously accumulated hundreds within the
hour.

Stated as what it is: suggestive, not proven. The interval was never fixed, and
this run also began with a power cycle rather than a sysrq reset, so the
configuration is not the only thing that changed. What it does refute is my own
early-warning claim — **three key-install failures have now occurred with no
wedge following**, so that signature is a frequent event that sometimes precedes
a wedge, not a predictor.

### 5af. Reviewing the fixes, and what un-adopt was missing

**Done 2026-08-17.** The fixes from §5ae had been written quickly, to close
findings, and nobody had reviewed them. So they got the same treatment — four
dimensions, then a refuter on **every** candidate rather than the first fourteen,
because §5ae's own run had capped verification silently and that is the rule it
was written to catch.

**19 candidates, 19 verified, 13 confirmed.** The second round found more than
the first.

#### The worst finding was a fix that committed the error it was fixing

`APsFresh` was set from `len(ifaces) > 0` **before the batch ran** — from the
intent to ask, not from an answer. A device whose hostapd calls were all refused
then reported `broadcast_known: true` with an empty list: a positive claim that
nothing is on the air, from a check that never answered.

Measured rather than argued: the same input gives `known=true` on the fixed
version and `known=false` on the code it replaced. **That case was made strictly
worse by the fix for a different one.** It is now computed from the answers.

#### Six tests of mine asserted nothing

All six were caught by mutation testing, none by reading:

- the `APsFresh` producer had **no test at all** — hardcoding the line to either
  constant left the whole suite green
- the provenance test rendered zero rows, because the live channel was mocked to
  a no-op
- the PMF clamp test used a fixture value already valid for every mode
- the DFS "other direction" used a snapshot channel already non-DFS, so it held
  under any implementation including one ignoring the live channel entirely
- the `Enhanced open` PMF exclusion had no coverage
- the seed clamp had none either

#### Un-adopt had less ceremony than the operation it undoes

Found by opening the last screen nobody had looked at:

| | apply | un-adopt |
|---|---|---|
| rollback armed | yes | **no** |
| confirmation | "I understand" | **none** |
| shows what it touches | full preview | **a count, afterwards** |
| destructive button | secondary | **primary** |

All four are now aligned the other way. And **listing the sections immediately
found a bug in the data behind the list**: the C6 claimed a mesh section and an
uplink section that had not existed on that device for months. The apply prunes,
but `RecordOwned` only upserted, so every pruned section left its claim behind
and `owned_sections` grew monotonically. `ReplaceOwned` makes the record exactly
the rendered set.

That one is worth remembering as a method: **a count could not have surfaced it.**
Showing the individual items is what made the data wrong in a visible way.

### 5ag. The core-package review

**Done 2026-08-17.** `applyengine`, `adoption`, `ubus`, `secrets` and `store`
had never had a review pass; the two earlier rounds covered recent diffs only.
**22 candidates, all 22 verified, 6 confirmed** — four of them high, in the code
that writes to live routers.

#### The apply engine reported clean reverts as stranded, two different ways

Both spend the engine's one alarming signal on a non-event, which is worse than
noise: a genuinely stranded change then looks like all the others.

1. **`planStillApplied` matched on a key that could never differ.** It returned
   "still applied" on the first planned option that read back equal, and render
   emits an `OpSet` carrying the WHOLE section — including the ownership tag,
   which a section we already own necessarily still has after a perfect revert.
   So **every apply after the first to an owned section** that reverted cleanly
   was reported `Unknown` + `Stranded`, with an error-severity audit event
   telling the operator to hand-reverse correct config. Only options that can
   *distinguish* the two states are consulted now, snapshotted before staging.
2. **The confirm-failure path never reached verification.** Both waits were
   anchored at "now" rather than at the moment `uci.apply` armed the device's
   timer — 90s + 105s against a 180s deadline — so the context expired inside
   the wait. Anchored at the arm time, waiting only what remains.

The existing tests could not catch the first: their plans carry one key, so
nothing was ever unchanged. The new one uses the shape render actually produces.

#### `internal/adoption/ssh.go` had no tests at all

The code that writes the ACL, creates the controller's login and removes them
was exercised only through a fake. Two real bugs in it:

- **`DialSSH`'s handshake had no deadline and ignored ctx.**
  `ClientConfig.Timeout` is read only by `ssh.Dial`. A host that accepts TCP and
  never speaks SSH held adoption open forever, past the adopt timeout and past
  the cancelled request. Bounded now — and *cleared* afterwards, which is the
  load-bearing half: the same connection carries every later write.
- **`RemoveFootprint` reported success from `rm -f`.** Three statements joined
  by `;`, so the verdict was the last one's, and `rm -f` succeeds on a file that
  was never there. Anything that made uci fail while unlink worked reported a
  clean un-adopt, leaving the login to return at the next reboot. It reads both
  halves back now.

#### And a batch that ran on the device with its answers discarded

`ubus`'s re-login retry discarded `buildChunk`'s end index. A fresh session's ids
can be wider, so across a power-of-ten boundary the rebuild holds one call fewer
— and it was posted anyway. The device ran N-1 calls, the length check rejected
the reply, and the original results were returned: **the writes landed while
every call was reported denied**, with `Retried` false, which makes
`IsPermanent` report a permanent ACL gap as transient.

#### The SSH host-key pin — fixed in §5ah

The sixth finding: `internal/adoption/ssh.go` captured the fingerprint at
adoption and threw it away, so the host-key-change refusal could not fire on
any device. Closed 2026-08-17; the write-up moved to §5ah.

---

### 5ah. A guard that was written, reviewed, shipped — and unreachable

**Done 2026-08-17.** The last open finding from §5ag, and the most instructive
one in the set, because nothing about it looked wrong.

`DialSSH` refuses a device whose SSH host key has changed. The refusal is
correct, its error text is good, and it **could not run**. Both call sites left
`SSHOptions.HostKeyFP` empty, `adoption.Result.HostKeyFP` was dropped when the
`store.Device` was built, there was no column to hold it, and no test touched
the branch. Five separate places, each of which reads as an omission only once
you know about the other four — which is why a review that reads a file at a
time will not find this class at all. **It took following one value end to end.**

It matters at **un-adopt** rather than adoption. Adoption is genuinely first
use: there is nothing to check against, and refusing to adopt until an operator
has collected fingerprints by hand is a worse answer. Un-adopt dials the
**stored** address carrying the administrator password the operator has just
typed into the panel.

What landed:

- **Migration 9** adds `devices.host_key_fp`, NULL for everything adopted
  before it. That is the honest value — nobody recorded a key for those devices,
  and back-filling one would pin whatever answers next, which is precisely what
  a pin exists to catch. Un-adopt learns the key on its first dial, so the
  second attempt is checked even though the first could not be.
- **`UpsertDevice` COALESCEs on the stored value**, not the incoming one, so
  neither a caller that omits the field nor one carrying a different key can
  blank or quietly re-pin it. `cert_fp` deliberately keeps the older
  take-the-new-value rule: a certificate is re-derived on every https connection
  and rotates legitimately, whereas a host key changing means the box was
  reflashed.
- **`SetHostKeyFP` is first-use-only and refuses an empty fingerprint.** The
  caller reads it from a `Bootstrap`, and a fake — or a bootstrap that never
  handshook — returns `""`. Storing that would leave the column looking
  unpinned while having been "set", so the first-use branch would never run
  again: a non-guard that reports itself as configured.
- **Force survives a refused dial.** This is created by the fix rather than
  found by it. Reflashing is the commonest reason a host key changes, and a
  reflash also wipes the footprint un-adopt came to remove — so without an
  escape, adding the pin would make a reflashed device permanently un-removable
  from the inventory, failing at the dial before Force was ever consulted. The
  residue is still reported honestly: with no SSH session, phase 2 never runs
  and the report says the login and ACL remain.
- **`AdoptResult.HostKeyFP` is filled in.** Declared since adoption was written,
  never set. A fingerprint nobody is shown is one nobody can compare against
  `ssh-keygen -lf`, and adoption is the single moment both ends are known to be
  the same box.

**The two lab APs are still unpinned, and will be until they are re-adopted.**
Worth saying plainly rather than leaving implied by the migration note: they
were adopted before the column existed, `host_key_fp` is NULL for both, and the
only code path that can learn a key is un-adopt — which is too late to protect
that same dial. There is no other SSH path (re-probe is ubus), so nothing pins
them in the background. A deliberate hold, not an oversight: the alternative is
a separate "pin now" action that asks for an SSH credential, which is a feature
for a fleet, not for two devices that can be re-adopted in a minute.

**The tests use a real in-process SSH server**, generated key and all, because
the guard lives inside the handshake and no fake reaches it. Two servers in one
test is what lets "a different box is answering at this address" be expressed at
all. Five mutations, five failures: unwiring the pin, taking the new value in
the upsert, dropping the TOFU clause, dropping the empty-key guard, and removing
the Force escape. The migration was run against a copy of the live database —
both devices read back unpinned, which is the documented legacy state rather
than a bug.

---

### 5ai. The screen above the change — un-adopt had no way out

**Done 2026-08-17.** §5ah gave un-adopt a new way to fail, so the next thing to
read was the panel sitting on top of it. Two defects, neither reachable by any
test that existed, and the first is the worst thing found in a screen so far.

#### A device that cannot be reached could never leave the inventory

`force` has been on `UnadoptRequest` since un-adopt was written. `api.unadopt`
in the client has always accepted it. **No screen ever sent it.** So dead
hardware, a reflashed box, a lost administrator password — and, as of §5ah, a
refused host key — left a row that could not be removed at all: listed, polled,
counted, forever, with a hand-written API call as the only escape.

This is §5ah's shape one layer up, and worth noticing as a pattern rather than
an incident: **a capability declared at every layer but the last one is
indistinguishable from a capability that was never built**, and it reads as
complete from every angle except actually using it. The Go side had a field, a
JSON tag, a documented meaning and a comment explaining the ordering that made
it work. The TypeScript side had it in the request type. Nothing called it.

The recovery is offered only *after* something fails, from both places a
failure can land — the result view when the row survives, and the form when the
request threw — and behind its own confirmation, because it is its own
decision. The one above says "revert this device"; this one says "give up on
reaching it, and lose the record of what is still installed". It carries the
credential when the failed attempt had one, since the daemon still tries phase 2
and only skips it when the connection fails.

#### And the report was rendered and discarded in the same tick

`onDone()` ran the moment the request returned, and it unmounts the whole
slide-over. So the residue list — the last copy of what is still installed on a
device whose inventory row has just been deleted — was painted and thrown away
before anything could read it. `Close` does it now, and only when the row is
really gone.

Harmless until today, which is why it survived: a removal could only happen when
it was *clean*, so the discarded list was always empty. Making forced removal
reachable is what turns that list into the only copy. **A latent defect and its
activating change arrived in the same afternoon, from opposite ends.**

#### Then driving it found two more

Reading the panel found the first two. **Running** it found two more, which is
the distinction worth keeping: a screen can be correct in every state you
imagined and wrong in the state the flow actually reaches.

The flow was exercised against a **throwaway inventory row pointing at a closed
port** — not the lab APs. A wrong password against a real device is not a safe
way to produce this failure: the reference hardware accepts *any* password when
root has none (§0), so the "failed" attempt would have succeeded and genuinely
un-adopted a working AP. A dead address fails at `DialSSH`, which happens
*before* `Adopter.Unadopt`, so phase 1 never runs and nothing is written
anywhere.

- **The residue hint said "supply the credential and try again."** True while a
  row survives; nonsense after a forced removal, which is the case where that
  list is the *only* record of what is still installed. It now says to copy the
  list before closing, and offers the retry only while there is something to
  retry against.
- **The "Revert config only" note had drifted two cards away** from the button
  it explains, below the forced-removal card.

End to end against the running daemon: row removed, report still on screen with
both residue entries, audit event recording `forced=true
footprint_remains=true`, no orphaned `owned_sections`, and the fleet count
refreshing only on Close.

---

### 5aj. Reviewing the afternoon's own fixes — four more, three self-inflicted

**Done 2026-08-17.** §5af's rule again, and it held again: the round that
reviews the fixes found more than the round that found the bugs.

#### A failed un-adopt threw away the report that failure produced

`writeErr` sends `{"error": "..."}` and nothing else, and the handler used it for
every error. But `Unadopt` returns a result **and** an error together in two real
cases: a phase-2 failure with the credential supplied, and a **forced** removal
whose phase 2 connected and then could not commit — a full `/overlay`, a held
uci lock, exactly the states §5ag's `RemoveFootprint` verification exists to
catch.

In that second case **the inventory row is already gone** and `Residue` is the
only surviving record of what is installed on that device. The bare error string
destroyed the one thing nobody could recover, and there was no row left to ask
about. This is the §5ai defect again, by a different route — and reaching it
required making forced removal reachable, which was mine, three commits earlier.
**A fix that opens a path is responsible for what is already on it.**

- `UnadoptResult` gained `error`, named to match what every other endpoint puts
  in an error body, so a generic client still finds a message where it looks for
  one. The report now travels with the 502; with no report, a plain error still
  goes, because an empty report renders as "nothing removed, nothing remains".
- The panel accepted a body only on 409. It takes any report-shaped body now,
  keyed on `removed_from_inventory` — the one field the Go type always emits.
  **Keying it on an omittable field passed the first two tests**; a third case (a
  report with no residue: phase 1 failed on one section, phase 2 cleaned up, row
  removed) is what distinguishes them, and it exists because the mutation found
  the gap rather than the reading.
- **"Still in the inventory" and "needs the administrator credential" were one
  banner.** A phase-2 failure with a credential supplied was described as
  needing one, sending the operator to re-type a password already correct.

#### And the forced-removal confirmation was sticky

Tick it, think better of it, retry with a corrected password, fail again — and
the destructive action is one click away, un-reconfirmed, at exactly the point
the speed bump exists for. Every attempt re-earns it now. The ordinary
confirmation deliberately does **not** reset: that one is consent to un-adopt
this device, which retrying the same operation does not withdraw.

#### A third round, and the fix's own fix

Reviewing §5aj found one more, again mine, again from the commit before.

**Moving `onDone` to Close made the slide-over's `×` a silent second exit.**
`onDone` both refreshes the fleet and closes; firing it the instant the request
returned is what threw the report away, so it moved to Close — which left the
`×` as a way out that refreshes nothing. A removed device stayed in the table:
**a controller listing a router it had just deleted.** The `×` lives outside the
component and cannot be intercepted, so the unmount catches it, guarded by a
flag that Close clears so the refresh does not happen twice.

The flag is set in **one** place, because a report arrives on both paths and the
worst case arrives on the failure one — a forced removal whose phase 2 could not
commit returns 502 with `removed_from_inventory` already true. Setting it beside
only the success path would leave precisely that case listing a deleted router.

And one of the three tests written for it **was itself a no-op**, caught by
mutation rather than by reading: the "refreshes once, not twice" spy only
recorded, so the component stayed mounted, the cleanup never ran, and the
assertion held whether or not the flag was cleared. A spy standing in for a
callback whose *effect* is what the test depends on has to have that effect —
`onDone` now tears the panel down the way `setOpenID(null)` does.

**Three rounds on one panel: 2, then 3, then 1.** The tail is real but it is
thinning, which is the first time that has been true of a review sequence here.

#### Verified against a device that answers and can do nothing

`tools/hostilessh` was written for this and kept: it accepts any password and
fails every command with the stderr uci gives on a read-only overlay. Pointing a
throwaway inventory row at it produces the half-succeeded state — phase 2 runs,
nothing can be removed, `Unadopt` returns a result *and* an error — which is the
hardest case to reach and the one that was silently discarding its own report.

Driven in the browser, it now renders: the new banner (still in the inventory,
**not** claiming a missing credential, since one was supplied), the residue list
on an error path, and the device's own `uci: Cannot write to file: Read-only file
system`. It also exercised **§5ag's `RemoveFootprint` verification against a
hostile device for the first time** — it refused to report a clean un-adopt when
`rm -f` would have succeeded and uci did not, which is precisely what it was
written for and had only ever been checked by a unit test.

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
- **A fixture that cannot express the broken state cannot catch the bug.** The
  adoption bug that stopped every stock router survived because the mock had a
  hardcoded interface list that could not go empty, and no radio-level config at
  all. Two blind spots in the fixture, in the exact shape of the blind spot in
  the code.
- **When a value can come from more than one source, check what decides.** The
  client grid picked an AP by which collector wrote second. Holding the evidence
  fixed and reversing only the write order is what proved it; reading the query
  had not.
- **A watchdog that cries wolf will be ignored on the day it is right.** The WRT
  monitor reported a FAULT for a normal 802.11r roam, and separately reported the
  router UNREACHABLE while it was answering — a false "down" and a false alarm
  are the same failure.
- **Never put a real credential in a test, least of all in the test about
  credentials.** The lab AP's live passphrase went into a public repository
  inside `TestTheTakeoverBriefNeverCarriesThePassphrase`. A sentinel does
  identical work, and the load-bearing assertion was never the constant anyway —
  it was that the response has no field a secret could live in.
- **A guard written from one radio must not be applied to the device.** And the
  obvious per-radio fix is a trap: excluding a radio that has not identified
  itself turns a real warning into silence, which is the cardinal error wearing
  a different hat. Exclude only what is KNOWN to be different.
- **A test that passes while asserting nothing is worse than no test.** Eight of
  these were shipped in three days and every one was caught by mutation testing
  rather than by reading. Revert the fix; if the test still passes, it tests
  nothing. The commonest causes: a fixture value already satisfying the
  assertion, a mock returning undefined so the path never runs, asserting the
  absence of something never present in that fixture, keying a check on a field
  every fixture happens to carry — and **a spy that only records when the test
  depends on the callback's effect**. `onDone` closes a panel; a spy that did
  not close it left the cleanup unrun, so an assertion about double-firing could
  not fail.
- **Follow the value, not the file.** A security guard can be correct, well
  worded and completely unreachable. The SSH host-key refusal was dead in five
  places at once — no column, no field on the result, neither call site passing
  it, no test — and each one reads as a harmless omission unless you already
  know about the other four. Reading a file at a time cannot find this; tracing
  one value from where it is produced to where it is checked can.
- **A capability declared at every layer but the last one is the same as one
  that was never built.** `force` had a field, a JSON tag, a documented meaning,
  a comment explaining the ordering that made it work, and a slot in the
  TypeScript request type. No screen sent it, so a device that could not be
  reached could never be removed. It reads as finished from every angle except
  using it — which is the only angle that settles it.
- **Read the screen above whatever you just changed.** Not the code you wrote:
  the surface a person touches to reach it. §5ah added a new way for un-adopt to
  fail, and the panel over it turned out to have no way to recover from any of
  them, including the ones that had always existed.
- **Then DRIVE it, on a throwaway.** Reading the un-adopt panel found two
  defects; running it found two more, and driving the *error* path found the
  three in §5aj. A screen can be right in every state you imagined and wrong in
  the one the flow reaches. Manufacture the failure on a disposable inventory
  row: a closed port for "cannot connect", and `tools/hostilessh` — which
  accepts any password then fails every command — for "connects and can do
  nothing", the half-succeeded case that is hardest to reach and worst to get
  wrong. **Never by feeding a real device a wrong password**, because the
  reference hardware accepts any password when root has none, so the "failure"
  would succeed and un-adopt a working AP.
- **A guard that reports itself as configured is worse than an absent one.**
  The empty-fingerprint case is the whole pattern in miniature: store `""` and
  the column says "pinned", the first-use branch never runs again, and nothing
  anywhere is checking anything. Refuse the empty value at the setter.
- **A fix that opens a path owns what is already on it.** Making forced removal
  reachable from the UI turned a latent handler bug — a failed un-adopt
  discarding the very report the failure produced — into a route an operator can
  walk, and the payload it destroys is irrecoverable because the row it
  described is gone. Ask what a new path leads to, not only whether the path
  itself is right.
- **An error response is not always only an error.** Two endpoints here return a
  result and an error together, and the generic "send the error string" helper
  silently drops the half that matters. Whenever a call can half-succeed, the
  body is part of the answer on the failure path too.
- **Review the fixes, not just the code.** A fix written quickly to close a
  finding is where the next bug is. The second review round found MORE than the
  first, including a fix that committed the exact error it was fixing.
- **Show the items, not the count.** The un-adopt panel was changed to list the
  sections it would revert instead of counting them afterwards, and the list
  immediately exposed two claims for sections that no longer existed. A count
  cannot be wrong in a way anybody notices.
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
- **The last thing you did is not the cause.** hostapd on the reference device
  went into uninterruptible sleep with an `rrm_nr_get_own` in flight, and the
  obvious conclusion — a driver quirk in that call, of exactly the shape §5q and
  §5s already document twice — was wrong. The kernel log showed the driver
  already failing before the call, and on a freshly booted device the same call
  returns instantly and leaves hostapd healthy. A project that has found three
  real "the device says yes and means no" quirks is primed to see a fourth
  everywhere; a controlled repeat on a known-good device is cheap and settles
  it. The cost of getting this wrong is not a wasted hour — it is a quirk
  recorded in the capability model, gating a working feature off working
  hardware forever, with a measurement's authority behind it.
- **One component's fixture, checked by another component's expectations.**
  The mock answered ubus `list` with `{}` unless its first parameter looked like
  a session token — backwards, since `list` needs no session and that is exactly
  why discovery uses it. Discovery's own tests use their own fixture, so the
  disagreement was invisible for the life of the project; it surfaced the first
  time a daemon test asked discovery to identify the mock. Not the usual
  mock-is-simpler failure: this mock was *inverted*, and every test that never
  asked the question passed either way. Where two components share a fixture,
  make at least one test cross the boundary.
- **Two sources that answer the same question do not answer it the same way.**
  `iwinfo.devices` and `luci-rpc.getWirelessDevices` both list wireless
  interfaces, and only the second listed a live 802.11s mesh point — measured.
  A feature that read one list and looked up the other's map silently never
  fired. §5o had already chosen getWirelessDevices for modes; the same code
  then used the other list for the same question, two lines apart. When two
  calls overlap, write down which one is authoritative for what.
- **A cache invalidated at the right moment can still be invalidated too
  early.** An apply invalidates the interface list, correctly — and the refetch
  landed in the seconds before the new interface existed, so it cached the
  absence and held it for the full cadence. Invalidation says "this is stale";
  it does not say "the replacement is ready". Where a change takes effect
  asynchronously on the device, the re-read has to be scheduled after it, not
  after the call that requested it. Third appearance of "an apply returning is
  not a radio being ready".
- **Firing an event is not performing a gesture.** Every drag test in the UI
  suite called `fireEvent.dragStart` on a header and asserted the reorder
  landed. They all passed on a grid whose headers were `draggable={false}` —
  a header no mouse could ever pick up — because fireEvent dispatches the event
  whatever the DOM says. The tests proved the *handler* worked and said nothing
  about whether the gesture can begin. Where a feature depends on an attribute
  the browser consults rather than on code you wrote, assert the attribute.
- **A correct value under a wrong label is a wrong readout.** "Packages
  installed: none" was true — the controller installs none, and the field exists
  so that claim can be checked rather than believed — and it read as a statement
  about the device, which for any real router is plainly false. The explanation
  was in a `title` attribute nobody hovers. Two more in the same sitting:
  "Radios" listing BSSes, and one channel's occupancy printed once per BSS so
  that one measurement looked like two. None of the three had a wrong number in
  it, and all three told the reader something untrue.
- **Enumerating the wrong noun makes hardware disappear.** `iwinfo.devices`
  lists broadcasting interfaces; `probeRadios` treated them as radios. A device
  with no WLAN therefore had no radios, so the renderer would not give it a
  WLAN, so it could never have one — and stock OpenWrt ships radios disabled, so
  that was every freshly adopted router. It survived because the one device that
  ever worked had its radios switched on by hand first. When a call answers a
  question, check it is the question being asked.
- **A check that cannot run must not report a clean result.** With no radios to
  inspect, the Marvell mesh quirk could not fire, and mesh flipped from
  correctly-Absent to Present on a driver that will not run a mesh point. The
  three-state rule is usually applied to a call that was refused; this was a
  check whose INPUTS went missing, and it reached the same wrong answer by a
  different road. Where a gate depends on data that can be absent, absence of
  the data is NotObservable, not a pass.
- **A management-plane read is not a measurement of the physical world.** The
  WRT3200ACM beaconed an SSID that existed in no configuration for fourteen
  hours while `/etc/config`, hostapd's running conf, `iwinfo`, ubus AND the
  kernel's `iw dev info` all reported the correct one. Every check this project
  had was on the wrong side of the driver. The consequence was not just a wrong
  reading — it was hours of "verified on hardware" claims about wireless
  behaviour that no radio was performing. Where a property is physical, confirm
  it physically: a scan from a second device costs one command.
- **A topology written down once is a topology that is wrong later.** This file
  described the lab as "cabled LAN-to-LAN, C6 behind the WRT", with the dev Mac
  at an address and interface that had both changed. It also never mentioned
  that the C6's WAN port is plugged into the UniFi network, which makes it
  dual-homed — so every sentence anyone wrote about "what happens when this
  device loses its cable" was reasoning from a diagram that did not match the
  room. Cheap to check (`carrier` under /sys/class/net, one `network.interface
  dump`), and it took an accidental unplug to expose. Re-measure the wiring
  before designing anything that depends on it.
- **A test helper that computes an identity its own way will invent a second
  device.** The seed helpers wrote device MACs as literals while adoption reads
  the LAN bridge, so a seeded row and a real adoption of one physical box
  coexisted as two adopted devices — one AP polled twice against a budget of one
  request a minute. Fixtures that stand in for a real path must call the real
  path's function, not agree with it by hand.
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
- **The device credential is not recorded in this repo, and must not be.** This
  repo is public, and a password committed to it stays in the history after any
  later edit. One was committed here and is now dead — rotated 2026-08-15 — but
  it cost an hour first, in the way stale secrets always do: it was *recorded*,
  so it was *trusted*, so a login failure looked like a broken device rather
  than a wrong password.

  It is rotated by every adoption, which is what makes drift the normal case
  rather than the exception.

  **To find out whether the one you have is right**, ask the device rather than
  a document:

  ```bash
  curl -s http://192.168.1.1/ubus -d '{"jsonrpc":"2.0","id":1,"method":"call",
    "params":["00000000000000000000000000000000","session","login",
    {"username":"oonfeewrt","password":"THE-ONE-YOU-HAVE"}]}'
  ```

  A `ubus_rpc_session` in the reply means yes; `"result":[6]` means no.

  **To settle it definitively** — whether the password is wrong or something
  else is — compare against the stored hash, which SSH can read and ubus
  deliberately cannot:

  ```bash
  ssh root@192.168.1.1 "uci get rpcd.oonfeewrt.password"
  openssl passwd -6 -salt "<the salt between the 2nd and 3rd \$>" "THE-ONE-YOU-HAVE"
  ```

  Equal means the password is fine and the problem is elsewhere; unequal means
  it was rotated. That check turned an hour of guessing into one command.

  **To set a known one** (no re-adoption, does not touch the ACL file):

  ```bash
  ssh root@192.168.1.1 "uci set rpcd.oonfeewrt.password='$(openssl passwd -6 'NEW')' \
    && uci commit rpcd"
  ```

  rpcd re-reads the login config at session-creation time, so no restart is
  needed — and restarting it would destroy every live session.

  If the login section is gone entirely, re-adopt: that rewrites both
  `/usr/share/rpcd/acl.d/oonfeewrt.json` and the `rpcd` login, and
  `TestIntegrationAdoptARealDevice` prints the credential it creates.
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

- **Both devices are adopted into `.run/`**, with their credentials sealed in
  `.run/keyring.json` under the operator passphrase. That is the only copy —
  adoption never returns the password it generated, deliberately, so it exists
  in the keyring and nowhere else. Losing the passphrase means re-adopting, and
  re-adopting works because root has no password over SSH.

  The two-AP neighbour test is also the setup helper. It re-adopts what it can,
  reuses and re-probes what is already adopted, and reuses the site model rather
  than recreating it, so it is safe to run repeatedly:

  ```bash
  OONFEE_NEIGHBOURS=1 OONFEE_SEED_DIR="$PWD/.run" OONFEE_SEED_PASSFILE=/path/pass \
    OONFEE_AP1=192.168.1.1 OONFEE_AP2=192.168.1.2 \
    OONFEE_ADMIN_USER=root OONFEE_ADMIN_PASS= \
    OONFEE_WLAN_SSID=oonfee-roam OONFEE_WLAN_KEY=... \
    go test -tags=integration ./internal/daemon/ -run TestIntegrationNeighbours -v
  ```

  Re-adopting narrows the login to production scope, so §7's `add_list` grant
  command has to be re-run afterwards for the applyengine hardware tests.

- `docs/IMPLEMENTATION.md` §14 and §15 are the authoritative record of measured
  behaviour. When code and docs disagree, the measurement wins — and if neither
  matches the device, re-measure before changing either.
