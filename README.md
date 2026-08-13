# oonfeeWRT

A UniFi-grade management interface for the OpenWrt you already run.

Not a fork. Not a firmware. Not a distribution. **A front end that connects to
stock OpenWrt over its existing API and makes it manageable the way UniFi is.**

**Status:** design phase — no product code yet. These documents are the plan.

What does exist: [`tools/probe.py`](tools/probe.py), which validates the design's
assumptions against a real device, and [`deploy/acl/oonfeewrt.json`](deploy/acl/oonfeewrt.json),
the rpcd ACL that is the project's entire device-side footprint. The design has
been validated against a WRT3200ACM on OpenWrt 25.12.5 — including the
apply/confirm/rollback safety mechanism everything else depends on. See
[`docs/IMPLEMENTATION.md`](docs/IMPLEMENTATION.md) §14 for what that run settled.

---

## The positioning, precisely

GL.iNet ships a friendly UI on top of OpenWrt — but it's their firmware, their
fork, their maintenance burden, and it manages exactly one router. LuCI is stock
and manages one device, exposing OpenWrt's full complexity with none of its
ergonomics. UniFi has the ergonomics and the fleet view, but it's a closed
appliance ecosystem.

oonfeeWRT takes the third position:

> **Stock OpenWrt firmware. Multiple devices. UniFi's ergonomics.**

It deploys the way the Omada software controller and self-hosted UniFi Network
do: **a Docker container you run yourself** — on a NAS, mini-PC, Pi, or home
server — that connects out to your OpenWrt devices. One image, one volume,
compose file included. The routers run nothing of ours; the controller has all
the room it needs.

You keep running whatever OpenWrt you already have, from wherever you already get
it, upgraded on whatever schedule you like. oonfeeWRT connects to it, reads its
state, and writes its config — the same config LuCI writes, through the same API
LuCI uses.

### The hard rule that defines this project

**We do not maintain OpenWrt.** Not a fork, not a firmware image, not a build
system, not a patch set, not a kernel module, not a device-side daemon we wrote.

Everything oonfeeWRT touches on a device is either already in stock OpenWrt or
already in the official package feeds. Our entire device-side footprint is
**one JSON file** — an rpcd ACL granting a dedicated user scoped permissions.
That's it. Nothing to maintain, nothing to rebuild when OpenWrt releases, nothing
that breaks when the user upgrades their router.

If a feature would require shipping code that runs on the router, the feature is
cut. That constraint is the entire reason this project can survive with a small
team.

### And we scope to what OpenWrt can already do

No invented capabilities. If OpenWrt can't do it, oonfeeWRT doesn't pretend to.
The value is *presentation and orchestration* of existing functionality, not new
functionality.

---

## Non-goals, stated plainly

- ❌ Building, patching, or distributing OpenWrt firmware
- ❌ A device-side agent or daemon of our own authorship
- ❌ Adopting or managing UniFi hardware, or any non-OpenWrt device
- ❌ Reimplementing Ubiquiti's inform protocol
- ❌ Features OpenWrt doesn't already support
- ❌ Cloud services, SSO, remote-access brokering
- ❌ Replacing LuCI — oonfeeWRT coexists with it, permanently and safely

---

## What you get

One screen where you define a **site** — networks, VLANs, WiFi, firewall zones —
and it reconciles onto every OpenWrt device you've pointed it at. Plus the live
view UniFi is loved for: topology, clients, radios, traffic, logs.

Change an SSID password once. It lands on three APs across two bands each,
correctly, with automatic rollback if anything goes wrong. That's the product.

---

## Documents

| File | What's in it |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Components, transport, data model, provisioning + rollback, telemetry, capability probing |
| [`docs/DEVICE-BUDGET.md`](docs/DEVICE-BUDGET.md) | Target hardware classes, hard resource budgets, where the cost actually is |
| [`docs/PARITY-MATRIX.md`](docs/PARITY-MATRIX.md) | Every UniFi screen → OpenWrt source → verdict, with dependency tier |
| [`docs/UI-SPEC.md`](docs/UI-SPEC.md) | Navigation map, layout system, validated design tokens, screen specs |
| [`docs/ROADMAP.md`](docs/ROADMAP.md) | Phases with acceptance criteria |
| [`docs/RISKS.md`](docs/RISKS.md) | What kills this project |

---

## The single most important design decision

Every config apply goes out as:

```
uci.set (batched, stages only) → uci.apply {rollback: true, timeout: 90}
                               → poll uci.confirm until it succeeds
                               → timer cancelled, change is permanent
                               (if confirm never lands, the device reverts itself)
```

Note: **no `uci.commit` before `apply`** — apply is what commits the staged
delta with the rollback snapshot. Committing first silently disarms the
protection. See ARCHITECTURE §4.

This mechanism already exists in OpenWrt — it is what LuCI's apply countdown
uses. Build it in Phase 0 and test it by deliberately breaking a device. Without
it, one bad VLAN push means a car trip, and the project dies in its first week of
real use.

---

## Target hardware, and the budget that follows

Three classes, weakest one sets the rules: **A** WRT3200ACM (roomy — 512 MB RAM,
256 MB NAND), **B** MT7981/Filogic AX3000 units (the sweet spot), **C** MT7621
AX3000 units (880 MHz MIPS, often 16 MB flash — **this class sets the budget**).

| Condition | CPU on class C | Flash writes |
|---|---|---|
| Idle, no UI open | < 0.5% | **zero** |
| A UI screen showing this device is open | < 3% | **zero** |

Zero new daemons by default. Everything beyond stock is opt-in per device with
its cost stated. Collection is demand-driven: baseline ~60s always, focused
5–10s only while someone is looking. See [`DEVICE-BUDGET.md`](docs/DEVICE-BUDGET.md).

**The one tradeoff you can't engineer away** — now narrower than we thought.
Per-client bandwidth accounting needs connection accounting, which *hardware*
flow offloading bypasses on the MT7621-class parts that need it to route at
gigabit. **Software** offloading does not: measured on kernel 6.12 with an
nftables flowtable and a flow confirmed in the fast path, conntrack byte
counters stayed complete. So the conflict is real only where hardware offload
is, and remains untested there. Either way we never change offload settings
silently — we state the tradeoff and let the user choose. Default: leave it
alone, accounting off.

---

## The second most important design decision

**Ownership tagging.** oonfeeWRT only ever writes UCI sections it created, marked
with `option oonfeewrt '1'`. Anything a human wrote in LuCI or over SSH is read
for display and never touched. Conflicts are surfaced loudly, never resolved
silently.

You are a guest on someone else's router. Act like one.

---

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

That choice has a practical consequence worth stating, because it decides what
this project may borrow from:

| Source | License | Usable here |
|---|---|---|
| **LuCI** — drives the same rpcd/ubus API we do | Apache-2.0 | ✅ Compatible. Attribute in `NOTICE` |
| `rpcd`, `uhttpd` interfaces | ISC | ✅ Permissive |
| **GL.iNet firmware and packages** | GPL-2.0 | ❌ Incompatible with Apache-2.0 |

This lands the right way round. LuCI is both the legally compatible option and
the technically relevant one — it is the only widely-deployed client that talks
to `rpcd` over HTTP the way a controller must, so its handling of sessions,
batching and ACLs is grounded in the same constraints we measured.

Vendor firmware, GL.iNet's included, is the opposite on both counts: licensed
incompatibly, and architecturally inverted — it runs **on** the router, as root,
over the local ubus socket, managing one device. Almost none of the behaviour
this project had to discover (session-bound confirm, the two denial channels,
ACL scoping, the armed-window token) is visible from that position, so there is
little there to learn from even setting the license aside.
