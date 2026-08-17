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

Change an SSID password once. It lands on every AP across two bands each,
correctly, with automatic rollback if anything goes wrong. That's the product.

---

## What is NOT tested yet

Stated up front rather than discovered later. Everything below is **built and
unit-tested, and has never run on the hardware it is for** — because the lab has
exactly two devices: a Linksys WRT3200ACM and a TP-Link Archer C6 v2.

Nothing here is known to be broken. It is unverified, which is a different claim,
and this project's whole position is that those two must not be blurred.

| Feature | The claim nobody has checked | What would settle it |
|---|---|---|
| **Mesh `peered`** | that two nodes find each other and the backhaul carries traffic | a second *mesh-capable* device — only one of these two is: the WRT advertises mesh support and its driver then refuses to bring the interface up, which is one of the defects the controller warns about |
| **Wireless uplink** | that a station associates and bridges | a device whose radio runs station mode — measured, *neither* of the two here does |
| **Fan-out beyond two APs** | that a site applies cleanly across three or more | any third AP |
| **Class B / C device budget** | the CPU and RAM figures in [`DEVICE-BUDGET.md`](docs/DEVICE-BUDGET.md) | specifically an **MT7621** (class C) or **MT7981/Filogic** (class B) |
| **Per-client accounting under *hardware* flow offload** | that the two genuinely conflict there | an MT7621-class part with hardware offload on |
| **Un-adopt giving a device's config *back*** | that a device we made changes to, un-adopted, diffs clean against a pre-adoption snapshot — [ROADMAP](docs/ROADMAP.md) Phase 0's second proof | nothing but running it: the test exists (`TestIntegrationAdoptUnadoptLeavesNothing`) and needs a device it can adopt and release. Footprint *removal* is verified; reverting owned sections is not |

The budget row is the one worth not lumping in with the others: an ath79 or
ipq40xx box closes the first three and leaves it exactly where it is, because
**class C sets the budget** and every measured figure in `DEVICE-BUDGET.md` comes
from the roomiest class.

**What HAS been verified on real hardware** — adoption; removal of the
controller's own footprint (login and ACL file) from a device, checked by
reading both back; apply with an armed rollback, watched changing on air and
reverting on air; 802.11r/k roaming across two APs; capability probing and the
driver-defect warnings; telemetry and the whole UI. Plus one thing
learned the hard way: on Marvell hardware, PMF (`ieee80211w`) kills the 5 GHz
radio within ~90 seconds of a fast-transition roam and needs a physical power
cycle. The controller warns before it lets you do that, with the measurement
attached.

---

## Getting it running

Two sides, and only one of them needs anything installed. Every step below is
what the code actually does today, verified against a Linksys WRT3200ACM on
OpenWrt 25.12.5.

### The router: nothing to install

There is no package to build, no opkg feed, no init script. A stock OpenWrt
device already has everything: `rpcd`, and `uhttpd` with the ubus handler
enabled. What adoption needs from you is **SSH access, once**.

```
Prerequisites on the device
  1. OpenWrt 21.02 or newer, reachable on the network
  2. SSH enabled (dropbear is on by default)
  3. A root password set          <-- see the warning below
```

**Set a root password before adopting.** A stock OpenWrt with no root password
authenticates *anything* — we measured it accepting an empty password, the
correct one, and a deliberately wrong one over ubus, plus the SSH `none` method.
Adoption probes for this and shows a warning, and it deliberately does not
refuse, because you may be knowingly running that way on a trusted lab network.
But it means the credential you type proves nothing about who you are:

```sh
ssh root@192.168.1.1 passwd     # do this first
```

Adoption then uses that credential exactly **once**, to write one file and
create one login, and never stores it. Removing the device asks for it again.

### The controller

```sh
npm --prefix ui install && npm --prefix ui run build   # builds the embedded UI
go build -o oonfeewrtd ./cmd/oonfeewrtd
./oonfeewrtd -data-dir "$PWD/.run" -listen 127.0.0.1:8080
```

On first start it asks for an **operator passphrase**, twice. That passphrase
encrypts every device credential at rest; there is no recovery if it is lost.
Then open the address and create the administrator account the UI asks for.

### Finding the device

The adopt screen has a **Scan** button that sweeps the networks this host is
attached to and lists what answers as OpenWrt. It tells you how many addresses
it will probe before it probes them, and what it is *not* covering and why —
because a controller that quietly skipped your subnet would report "no devices
found", which reads as a fact about your network rather than about itself.

The probe sends **no password and creates no session**. It is one
unauthenticated request asking the device to list what it can do, which stock
OpenWrt answers to anyone who can reach the port. Scanning is on demand only:
there is no periodic rescan, because sweeping your subnet on a timer forever is
not something a controller should do unasked.

It will not tell you the model. That needs a credential — stock OpenWrt refuses
`system.board` to an unauthenticated caller — so the list shows the address and
the shape of the device (radios up, gateway, DHCP server) and says the model is
unknown until you sign in. Better a blank than a guess.

**Add-by-address stays first-class**, and you will need it if the controller
runs in a container on a bridge network or on Docker Desktop: there is no LAN
layer 2 to sweep from there, so the scan will come up empty while adoption by
address works perfectly. Discovery is a convenience; adoption never depends on
it.

For an unattended host (a container, a systemd unit) supply the passphrase from
a file instead:

```sh
OONFEE_DATA_DIR=/data OONFEE_LISTEN=:8080 OONFEE_PASSPHRASE_FILE=/run/secrets/oonfee-passphrase   ./oonfeewrtd
```

The file must be mode `600` or it is refused. There is deliberately **no
`OONFEE_PASSPHRASE` environment variable** — env is readable from `/proc`,
inherited by child processes, and printed by `docker inspect` — and setting one
is an error rather than being ignored, so the mistake is loud.

### What the setup is protecting, and what it is not

Low friction and secure pull in opposite directions in exactly three places.
Here is where each line was drawn, so you can move it knowingly:

| Choice | Friction | What it buys |
|---|---|---|
| **No default credentials, anywhere.** First run creates the admin account interactively | one extra screen | A shipped default nobody rotates is the most common way a self-hosted controller ends up on the internet with a known password |
| **The passphrase is not in the environment** | you must create a file for unattended boot | `/proc`, child processes and `docker inspect` never see it |
| **A device with no root password is warned about, not refused** | none | You keep control of a real tradeoff; the controller's own login is password-protected regardless |

And the parts that are simply free, because they cost you nothing to have:

- **The controller does not run as root on your device.** Adoption creates a
  dedicated `oonfeewrt` login scoped to one ACL file, and that file is the
  entire device-side footprint. Review it like code — it is the blast radius.
- **The operator credential is never stored.** It is used for one transaction
  and requested again at removal, because a controller that could delete its own
  permissions could also widen them.
- **Certificates and host keys are pinned on first use.** A device whose TLS
  certificate or SSH host key changes is refused, not clicked through.
- **Removal is complete and tested.** Adopt, use, remove, and the device is
  byte-for-byte as it was — there is a test that asserts exactly that against
  real hardware.

### One thing to decide about TLS

Over plain HTTP the session cookie cannot carry the `Secure` attribute, because
a browser silently drops a `Secure` cookie on an insecure origin and you would
be unable to sign in at all. On a trusted LAN that is a reasonable place to
start. If the controller is reachable from anywhere you do not fully trust, put
it behind TLS — the cookie attributes upgrade themselves automatically once the
request arrives over HTTPS or through a proxy that sets
`X-Forwarded-Proto: https`.

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
