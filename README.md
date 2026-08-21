# oonfeeWRT

A UniFi-grade management interface for the OpenWrt you already run.

Not a fork. Not a firmware. Not a distribution. **A front end that connects to
stock OpenWrt over its existing API and makes it manageable the way UniFi is.**

**Status:** Phase 4 is complete on `main`, hardened and working on two devices.
Not released.

**Current live checkpoint (2026-08-20):** the repository and lab database run
schema 16.
Schema 14 remains the secret-sealing epoch, schema 15 is the semantic boundary
for cross-feature policy intent, and schema 16 adds event provenance, topology
intervals/source coverage and explicit RF-scan records. The signed-in Phase-4
pass initially exercised the live schema-16 controller under both routers'
older ACLs and truthfully retained the resulting source gaps. That historical
no-router-change checkpoint was superseded when the operator explicitly
accepted the separately prompted, scoped ACL refresh on both routers at 15:16
and 15:17. Subsequent polls persisted topology-source and OpenWrt-log
observations from both routers plus fixed-`1.1.1.1` ICMP observations from the
Gateway. The refresh installs no package, binary, daemon, service or firmware;
no before/after package-inventory hashes were captured, so this checkpoint
makes no claim that the live package inventory was unchanged. No
disruption-acknowledged RF scan was run. The final audited binary, zero-change
fleet Preview and verified schema-16 recovery pair are recorded in STATUS
§5bk.
Phase 3 now has two-client DHCP/DNS/WAN and no-LAN hardware proof. Its literal
bidirectional peer data-plane claim remains partial because reciprocal raw
Safari peer-IP failures had no known-live peer listener or positive control.

The Phase-4 completion and hardening work was merged through
[PR #1](https://github.com/aiden0rchad/oonfeewrt/pull/1) as `ee15e2f`. At that
checkpoint the local release gates passed: full Go normal and race suites,
`go vet`, module-tidiness check, all 247 UI tests, production UI build, bundle
budget, tree secret scan, `git diff --check` and reproducible binary check. This
is a source checkpoint, not a tagged or packaged release.

There is a controller you can run: a Go daemon and an embedded React UI that
adopt an OpenWrt device, reconcile a site model onto it, and take themselves
back off leaving the device byte-for-byte as it was — there is a test that
asserts exactly that against real hardware. Devices are adopted, polled, charted
and applied to today, on a Linksys WRT3200ACM and a TP-Link Archer C6 running
OpenWrt 25.12.5.

What that does **not** mean: it has never run on more than two devices, several
features have never met the hardware they are for, and nothing here has been
packaged or released. The section below says exactly which parts are unverified,
and it is worth reading before the install instructions rather than after.

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
already in the official package feeds. The optional **oonfeeWRT controller
capability installation** has only two possible device-side artifacts: **one
JSON file**—an rpcd ACL granting a dedicated user scoped permissions—and the
scoped login that adoption may create. Accepting its separate prompt installs
or replaces that ACL file; it unlocks controller access to supported topology,
radio channel/scan, OpenWrt log and fixed-target WAN ICMP observations. It does
**not** install a package, binary, daemon, service or firmware. Leaving the box
unchecked or cancelling leaves the router unchanged, and observations needing
newer read grants stay visible as unavailable/partial source gaps.

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

In the current source, topology, radios, client observability and Logs expose
their limits instead of filling gaps: telemetry is durable rollups only; the
`wifi-v1` experience score is null unless RSSI, retry delta and TX-failure delta
all exist in the same sample; site latency/loss is one fixed gateway-vantage
ICMP probe to `1.1.1.1`; RF scans are explicit and disruption-acknowledged; and
router logs use bounded REST pagination with source coverage. The WebSocket is
only the bounded `device.stats` focus/live channel, not a log stream.
The schema-15 Policy Engine also has one cross-feature Master Table and a
partial Object Manager: it compiles visible, unsaved IPv4 `Secure` drafts and
static network routes; device/group routing, QoS and application outcomes stay
explicitly gated.

The initial 2026-08-20 live pass kept router capability refresh off. It showed
four Topology nodes and three current links (partial), four history intervals,
four stable radios with unknown channel-list/DFS evidence and no scan access,
63 General log rows with missing router-log coverage, and 169 Audit rows with
keyset pagination and detail. Client Observability joined client/AP/radio/path
evidence while fixed-`1.1.1.1` ICMP and historical source coverage remained
unavailable. The later, explicitly accepted scoped ACL refresh superseded only
that source-access boundary: both routers then supplied current topology and
OpenWrt-log observations, and the Gateway supplied fixed-target ICMP rollups.
Historical source coverage remains unavailable rather than inferred, and no RF
scan was run. Object Manager compiled one visible static-route draft; it was
neither saved nor applied, so it changed neither the database nor a router. The
subsequent bounded Phase-3 work created and removed one redundant
documentation-network firewall policy, then put two physical iPhones on one
isolated WRT BSS. Both proved distinct DHCP, fixed-IP WAN, DNS plus WAN and
denial to a known-live LAN HTTP listener. Reciprocal raw Safari peer-IP failures
were observed but lacked a known-live peer listener or positive control, so
literal bidirectional peer data-plane isolation remains open. A durable cleanup
operation removed only the proof WLAN, retained the
operator-created Guest network on VLAN 3 and ended with a zero-change fleet
plan. STATUS §5bk records the corrected boundary.

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
| **Class B / second class-C generation budget** | that the passed ath79/QCA956X budget generalizes to a different constrained SoC/release generation | specifically an **MT7621** (class C) or **MT7981/Filogic** (class B) |
| **Per-client accounting under *hardware* flow offload** | that the two genuinely conflict there | an MT7621-class part with hardware offload on |

The budget row is the one worth not lumping in with the others: the exact
60-minute gate passed on the class-C ath79/QCA956X C6 with zero poll failures,
flash writes or package changes. An MT7621/Filogic device now adds ecosystem
breadth rather than supplying the first constrained-device measurement.

The gateway row was closed on 2026-08-19. After an explicit one-time operator
conversion made the adopted Gateway + AP + Switch WRT VLAN-aware, the signed-in
browser applied VLAN2, its static interface, configurable DHCP, firewall-zone
LIST and forwarding/rules. A real Mac proved DHCP, DNS and WAN; Policy Engine
blocked WAN while retaining DHCP/DNS and restored it; DHCP disable/custom pool
behavior was also measured. oonfeeWRT still will not create the VLAN-aware
precondition itself. STATUS §5be records the proof operations; §5bg records the
confirmed cleanup that retained VLAN2, restored DHCP `100`/`150`/`12h`, removed
the temporary WLAN and returned policy provenance to the legacy WAN-only
default.

**What HAS been verified on real hardware** — adoption, and un-adoption that
leaves the device **byte-for-byte as it was**: adopt, apply a WLAN, un-adopt,
and every UCI config and the ACL directory diff clean against a pre-adoption
snapshot, with the two owned sections handed back and the login and ACL file
gone (ROADMAP Phase 0's second proof, run 2026-08-17). Apply with an armed
rollback, watched changing on air and reverting on air; 802.11r/k roaming across two APs; capability probing and the
driver-defect warnings; telemetry and the whole UI. Plus one thing
learned the hard way: on Marvell hardware, PMF (`ieee80211w`) kills the 5 GHz
radio within ~90 seconds of a fast-transition roam and needs a physical power
cycle. The controller warns before it lets you do that, with the measurement
attached.

---

## Getting it running

Two sides, and only the controller needs executable software installed. Every
step below is what the code actually does today, verified against a Linksys
WRT3200ACM on OpenWrt 25.12.5.

### The router: no executable software to install

There is no package to build, no opkg feed, no init script. A stock OpenWrt
device already has everything: `rpcd`, and `uhttpd` with the ubus handler
enabled. If you explicitly opt in to controller access during adoption, it
needs **SSH access once** to write the ACL JSON and create the scoped login.
That is a capability grant to software OpenWrt already runs, not a software
installation.

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

After the separate optional-capability installation acknowledgment, adoption
uses that credential to install or replace one ACL JSON file and create one
scoped login, and never stores it. Removing the device asks for it again.
Leaving the prompt unchecked or cancelling makes neither change; observations
that require the capability remain visibly unavailable.

### The controller

```sh
make build                    # npm ci + embedded UI + versioned static binary
./oonfeewrtd -data-dir "$PWD/.run" -listen 127.0.0.1:8080
```

Run `make check` before deployment. A release candidate additionally requires
`make release-check RELEASE_VERSION=vX.Y.Z`, which refuses a dirty tree and
byte-compares two complete UI-plus-Go builds.

On first start it asks for an **operator passphrase**, twice. That passphrase
unwraps the controller keyring used to seal device credentials, WLAN and mesh
keys, and secret-derived ownership verifiers at rest. There is no recovery if
either the passphrase or the matching keyring is lost. Then open the address and
create the administrator account the UI asks for. Perform that one-time setup
through `localhost` or the controller's literal IPv4/IPv6 address; DNS hostnames
are refused until an administrator exists to prevent first-run DNS rebinding.

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

Treat `oonfeewrt.db` and `keyring.json` as one restore unit. Back up a live WAL
database with SQLite's backup API, or stop the controller cleanly so its
checkpoint completes; pair that database snapshot with the keyring from the
same controller state. A passphrase cannot recreate the keyring's random data
key. Database backups made before schema 14 may contain plaintext WLAN/mesh keys
and secret-derived ownership hashes, and migration does not rewrite or delete
old backups. Keep them protected and require explicit operator confirmation
before deleting them.

Router configuration archives are a separate secret boundary: a normal tarball
contains wireless keys in plaintext. Encrypt the stream before retaining it,
verify recovery by streaming decryption into archive inspection, and do not
leave a temporary plaintext tar behind. The encrypted archive is only as safe
and recoverable as its passphrase and encryption tooling.

### What the setup is protecting, and what it is not

Low friction and secure pull in opposite directions in exactly three places.
Here is where each line was drawn, so you can move it knowingly:

| Choice | Friction | What it buys |
|---|---|---|
| **No default credentials, anywhere.** First run creates the admin account interactively | one extra screen | A shipped default nobody rotates is the most common way a self-hosted controller ends up on the internet with a known password |
| **The passphrase is not in the environment** | you must create a file for unattended boot | `/proc`, child processes and `docker inspect` never see it |
| **A device with no root password is warned about, not refused** | none | You keep control of a real tradeoff; the controller's own login is password-protected regardless |

And the parts that are simply free, because they cost you nothing to have:

- **The controller does not run as root on your device.** After explicit
  adoption/capability opt-in, it uses a dedicated `oonfeewrt` login scoped to
  one ACL file. That login and JSON file are the maximum controller-access
  footprint; neither is executable code. Review the ACL like code—it defines
  the blast radius.
- **The operator credential is never stored.** It is used for one transaction
  and requested again at removal, because a controller that could delete its own
  permissions could also widen them.
- **Certificates and host keys are pinned on first use.** A device whose TLS
  certificate or SSH host key changes is refused, not clicked through.
- **Wireless keys are write-only through the API.** Authenticated WLAN and mesh
  reads return `has_key`, never the key; legacy `?reveal=1` URLs remain redacted.
  Leaving the key blank on an edit preserves the stored value.
- **Removal is complete and tested.** Adopt, use, remove, and the device is
  byte-for-byte as it was — there is a test that asserts exactly that against
  real hardware.

### One thing to decide about TLS

At-rest sealing does not protect a key while the daemon is using it or while a
browser submits a new one. Over plain HTTP the session cookie cannot carry the
`Secure` attribute, because a browser silently drops a `Secure` cookie on an
insecure origin and you would be unable to sign in at all. Use plain HTTP only
on an explicitly trusted management network. If the controller is reachable
from anywhere else, terminate TLS at a trusted reverse proxy. This release has
no native controller TLS listener. Cookie attributes upgrade automatically once
the proxy sends `X-Forwarded-Proto: https`.

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
| **[OpenSOHO](https://github.com/rubenbe/opensoho)** — nearest neighbour in intent | AGPL-3.0 | ❌ Read it, don't copy it |
| **[OpenWISP](https://openwisp.org/)** — the incumbent, fleet/WISP scale | GPL-3.0 | ❌ Read it, don't copy it |
| **[obsy/apcontroller](https://github.com/obsy/apcontroller)** — agentless, SSH push | GPL-3.0 | ❌ Read it, don't copy it |

**On borrowing from the adjacent projects.** The question comes up, so: the
copyleft ones are a **one-way door**. Apache-2.0 code can go *into* a GPL-3.0
project; GPL-3.0 or AGPL-3.0 code cannot come *here* without relicensing all of
oonfeeWRT. That is a licence fact, not a judgement about their quality — they
are good projects, and the interfaces they drive (`ubus`, `uci`, `rpcd`) belong
to OpenWrt and are free for anyone to read.

It is also worth knowing how little overlap there would be. Each takes a
device-side dependency this project's hard rule forbids: OpenSOHO needs
`openwisp-config`, `openwisp-monitoring` and `luci-app-openwisp` installed;
OpenWISP ships agent packages; apcontroller `scp`s a script and runs it over
SSH. And the mechanism that costs the most effort here — `uci apply` with a
rollback timer, health-gated before confirm — has no counterpart in any of
them, because an agent-based design does not need one: a broken push just means
the agent stops checking in. There is no code to lift for the hardest part.

Worth reading for **design**, particularly OpenSOHO on per-device versus
fleet-wide wifi modelling, and it does read state back off the device rather
than only templating at it — the closest anyone comes to this project's
position.

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
