# oonfeeWRT — Risks

Ordered by how likely they are to kill the project.

---

## 1. Telemetry is the hidden project

Every serious attempt at this underestimates it. Reading and writing config is a
weekend; a metrics pipeline with retention, rollup, backfill, gap handling, and
charts that stay fast at 10k points is months. The OpenWrt forum thread on this
exact idea contains the line: *"Reading/writing configuration is obvious, but
telemetry/statistics add many layers of complexity."*

The wrapper constraint makes it harder, not easier: with no device-side agent,
every sample is an HTTP round-trip through `/ubus`, so you're paying latency and
CPU per metric per device per interval.

**Mitigation:** build the rollup ladder in Phase 1, before any screen depends on
it — retrofitting retention onto a naive `INSERT` table is a rewrite. Batch
aggressively (one composite call beats twenty small ones), stagger device polls
across the interval, and sample only what an open screen actually needs.

**Current source closure (2026-08-19):** raw samples stay in RAM and SQLite gets
only completed 5-minute/hourly rollups. Phase-4 reads are bounded and disclose
coverage: OpenWrt logs are 24h/50k-per-device/100k-global, controller/audit rows
have a separate 100k cap, topology is 31d/10k, client incidents cap exact events
and path work, and the WebSocket is a 32-frame drop-on-full `device.stats`
channel rather than a durable log bus. `wifi-v1` is all-or-null, so a missing
counter cannot improve a score by silently changing the denominator. These are
source contracts pending live schema-16 validation.

RF scan retention is closed at the stored/API contract: the five-minute
maintenance transaction retains one newest terminal run per stable
`(device_id, radio_key)`, never deletes pending/running work, and relies on the
declared foreign key to cascade each pruned run's BSS rows. Together with the
4,096-row decoder bound and explicit-only scan arrival, repeated scans no
longer grow terminal history over time.

---

## 1b. Degrading the router you're supposed to be managing

The wrapper-specific failure mode. A management tool that costs 15% of routing
throughput isn't a management tool, it's a tax — and users will remove it.

The two real threats on MT7621-class hardware aren't what people expect: **TLS
handshake cost** (no crypto acceleration at 880 MHz) and **flow-offload
conflicts** (per-client accounting and gigabit routing are mutually exclusive).
Polling volume itself is comparatively cheap.

**Mitigation:** the whole of `DEVICE-BUDGET.md` — hard measured budgets against
the weakest target, zero new daemons by default, demand-driven collection,
persistent connections, never touching offload settings silently, and a
per-device Management Overhead readout so our cost is a number the user can see
rather than a suspicion they act on.

---

## 2. Bricking a device

The failure mode that ends adoption. Push a wrong VLAN or wrong wireless config
to a remote AP and it's a car trip. This risk is *higher* for a wrapper than for
a firmware vendor, because you're changing devices whose exact state you don't
control and didn't ship.

**Mitigation:** the apply/confirm/rollback cycle, built in Phase 0, tested by
deliberately breaking a device. Warn in the UI whenever the management path
traverses the device being changed. Show a config diff before every apply.

---

## 3. Coexistence with LuCI and SSH

Your users keep using LuCI. That's the whole premise — you're a wrapper, not a
replacement. If the controller overwrites hand-written config even once, trust is
gone permanently, and rightly so.

**Mitigation:** ownership tagging (`option oonfeewrt '1'`) on every managed
section. Foreign sections are read-only. Conflicts are surfaced loudly, never
resolved silently. Ship the diff preview. And make **un-adoption** a real,
tested, one-click feature that leaves the device as it was found — a wrapper that
can't cleanly remove itself doesn't deserve to be installed.

---

## 4. Hardware fragmentation

UniFi's UI assumes its own hardware. GL.iNet's UI assumes its own hardware.
oonfeeWRT assumes *nothing* — OpenWrt runs on a thousand devices with wildly
different capabilities. Some have DSA switches with per-port stats, some have a
`swconfig` relic, some have neither. Almost none have controllable PoE. 6 GHz
depends on driver and regulatory domain. Flash and RAM vary by 100×.

This is the structural cost of the wrapper positioning, and it's a real one.

**Mitigation:** the capability registry (ARCHITECTURE §6.1), probed at adoption.
The UI renders *from* capabilities: unsupported features are **absent**, not
greyed out. Greying out teaches users the product is broken; absence teaches them
their hardware is limited.

---

## 5. Upstream drift

We depend on ubus object names, UCI schema, and package availability that we
don't control and can't pin. OpenWrt makes real breaking changes: `swconfig` →
DSA, iptables → firewall4/nftables, `opkg` → `apk`, LuCI → ucode. Each one would
have broken a naive wrapper.

**Mitigation:** version-aware adapters behind an interface, not `if` statements
sprinkled through the codebase. A compatibility test matrix across at least the
current and previous stable release. And detect the release at adoption — refuse
to manage a device on an untested version rather than corrupting its config, with
a clear "untested, proceed at your own risk" override.

Accept that you will be chasing upstream forever. That's the rent for not
maintaining a fork, and it's much cheaper rent.

---

## 6. Installing packages on someone else's router

The one place we reach beyond reading and writing config. A failed install on a
full `/overlay` can render a router unbootable.

**Mitigation:** official feeds only; always itemized and consented; check free
space and refuse rather than attempt; track what we installed so un-adoption can
offer to remove it; adoption must fully succeed with zero packages installed.
Never upgrade the user's packages or firmware on our own initiative.

---

## 7. Security posture

The controller holds a scoped credential for every managed device, wireless
keys, and secret-derived ownership verifiers; its explicit `file.exec` surface
is still privileged. It is the highest-value target on the LAN, and the ACL file
is security-critical code that happens to be JSON.

**Mitigation:** schema 14 seals credentials, WLAN/mesh keys, and ownership
verifiers under a random key in `keyring.json`, with record-bound AAD. A sealed
database key check rejects a mismatched database/keyring pair before WAL, DDL,
or mutation; migration checkpoint/VACUUM scrubs legacy plaintext before serving.
Keyring creation is atomic/no-clobber and startup refuses a missing keyring
beside an existing database. Backups pair a consistent SQLite snapshot with its
matching keyring. Pre-v14 backups remain plaintext-sensitive and are never
deleted without explicit operator confirmation.

The API never reveals WLAN or mesh keys—even to an authenticated session or a
legacy `?reveal=1` request—and reports only `has_key`. This does not replace
transport security: use TLS outside an explicitly trusted management network,
because browser submissions and live daemon memory are outside at-rest sealing.
Also retain the scoped ACL rather than root, review every exact `file.exec`
command like code, pin device TLS certificates (TOFU, refuse on change), keep a
full audit log, ship no default password, and write the threat model before v1.0.

ACL evolution uses one explicit adopted-device refresh transaction, not silent
widening during a poll. It is opt-in, identifies the exact rpcd ACL JSON file it
writes or replaces, and states that it installs no package, binary, daemon or
service. Declining it leaves the router unchanged and dependent observations
unavailable. If accepted, it proves the stored controller login and inventory
MAC before SSH, verifies/pins the host key, replaces only the scoped ACL, then
proves a fresh controller login/MAC/capability record. The administrator
password/private key is not stored or returned; UCI, ownership and the scoped
login are preserved. The 2026-08-20 live validation deliberately did not run
this transaction.

---

## 8. The name

Renaming from `UnifyWRT` to `oonfeeWRT` reduces the visual similarity to
**UniFi**, a registered Ubiquiti trademark — but be clear-eyed about what it
does and doesn't solve. Trademark similarity is assessed on **sight, sound, and
meaning**, and "oonfee" reads as a phonetic respelling of the incumbent's mark in
the same product category. Sound-alike marks (*idem sonans*) are a recognized
similarity factor, not a loophole.

This is a reduced risk, not an eliminated one. If you want it gone rather than
smaller, pick something that doesn't evoke UniFi at all — `OpenSite`, `Helm`,
`Beacon`, `Nucleus`, `Wrtrol`. Renaming is free now and expensive after you have
a domain, a package name, and users.

**Regardless of name, do not:**

- copy Ubiquiti's icons, illustrations, typeface, CSS, or any compiled asset
- use the UniFi wordmark or logo anywhere, including in comparisons
- reimplement the inform protocol to adopt Ubiquiti hardware
- use "UniFi for OpenWrt" as a tagline

**You may:** build equivalent components from scratch, adopt the same information
architecture and interaction patterns (layouts and IA are broadly not protectable
the way assets are), and factually describe the interface as *inspired by*
commercial network controllers. Clean-room the visuals; keep the ideas.

---

## 9. Scope collapse

The parity matrix has ~200 line items. Attempting them in parallel produces a demo
that does everything badly.

**Mitigation:** the phase gates in `ROADMAP.md`, and the discipline to ship
Phase 2 — one SSID change fanning out safely to N APs — before building anything
in Phase 4 or 5, however much more fun those screens are.

---

## 10. Solo-maintainer burnout

The most common cause of death in this category. OpenWISP survives because an
organization backs it; the OpenWrt forum is a graveyard of promising
single-developer controllers.

**Mitigation:** the no-device-code rule is itself the primary mitigation — it
eliminates build systems, per-arch packaging, and firmware release engineering,
which is where most of the recurring work in this category lives. Beyond that:
make Phases 0–2 genuinely shippable on their own so the project is useful, and
therefore attracts contributors, long before it is complete. Publish the
architecture docs early; they are the recruiting material.
