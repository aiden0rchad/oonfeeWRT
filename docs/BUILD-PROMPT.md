# Driving a build session (for Opus 5 on high, or any capable coding agent)

This repo is designed so a coding agent can build it milestone by milestone
without inventing anything. This file is the operating manual for those
sessions.

**Current UI reference (2026-08-18):** stable UniFi Network 10.5.67. Read the
Network-10.5 current-baseline section in `PARITY-MATRIX.md` and the Current
reference section in `UI-SPEC.md` before UI work. The milestone table below is
the build order, not the live queue; `STATUS.md` §5 is authoritative for what
comes next.

## Ground rules to give the agent (paste into every session)

> You are implementing oonfeeWRT per `docs/IMPLEMENTATION.md`. Rules:
>
> 1. **Do not invent ubus objects, methods, or fields.** Everything you call
>    must appear in `docs/ARCHITECTURE.md`, `tools/mock_ubus.py`, or a
>    `report.json` produced by `tools/probe.py` on real hardware. If you need
>    something none of those provide, stop and say so.
> 2. **Never call `uci.commit` before `uci.apply` when rollback protection is
>    intended.** `set` stages; `apply {rollback:true}` commits. This is
>    decision D2 and it is load-bearing.
> 3. Develop against `tools/mock_ubus.py` (`python3 tools/mock_ubus.py`).
>    Every feature lands with tests that pass against it.
> 4. Budgets are CI gates, not guidance: container image ≤ 40 MB, UI ≤ 1.5 MB
>    gzipped, `CGO_ENABLED=0` always, final stage `FROM scratch`
>    (DEVICE-BUDGET §8, decision D7). The managed-device budgets in
>    DEVICE-BUDGET §1–7 govern all polling/apply behavior and did not relax.
> 5. The no-device-code rule (ARCHITECTURE §0): nothing of ours executes on
>    managed devices, ever. The controller lives in its container (D7); there
>    is no host-device exception anymore.
> 6. Sections `docs/IMPLEMENTATION.md` §14 lists as pinned-to-hardware are
>    open questions. Code around them behind interfaces; do not resolve them
>    by assumption.
> 7. Work one milestone at a time (§13). A milestone is done when its "done
>    when" test passes, not before, and you do not start the next one in the
>    same session unless asked.

## Session sequence

One milestone per session keeps context small and reviewable:

| Session | Scope | Verify with |
|---|---|---|
| 1 | M0: `internal/ubus` + CI | `go test ./internal/ubus/...` vs mock |
| 2 | M1: adoption + capability | adopt/un-adopt round-trip test |
| 3 | M2: render + apply engine | deliberate-rollback integration test |
| 4 | M3: collector + first three screens | 24 h simulated soak + budget_check |
| 5 | M4: site WiFi + apply UI | ROADMAP Phase-2 proof, automated |
| 6 | M5: networks/zones/policy | guest-VLAN proof, automated |
| 7 | M6: insights/topology/logs | mwlwifi-gating test green |

Between sessions: review the diff yourself. The agent should also re-run all
prior milestones' tests (they're cheap, they run against the mock).

The table is historical build order. In the current tree, do not regress these
already-landed contracts while finishing a milestone:

- discovery distinguishes an unroutable CIDR from a successfully scanned empty
  one;
- un-adopt fails closed when `owned_sections_known` is false and preserves
  exact cleanup commands in partial/forced reports; no login/ACL or inventory
  deletion follows an unproved config hand-back unless Force is explicit;
- schema 11 stores an authoritative non-empty Gateway/AP/Switch function set.
  Omission alone expands a legacy role; explicit empty/null/corrupt state fails
  closed. Gateway adoption is serialized through row commit and a concurrent
  second Gateway is refused before device contact; AP-only may be first;
- schema 12 makes `zones.policy_json` authoritative directional intent. No row
  means the legacy source→`wan` edge; explicit `forward_to:[]` blocks every
  modeled edge; `wan` is destination-only and reverse initiation is
  independent. Validate the exact firewall4 identifier and active destinations,
  fail closed on malformed stored JSON, never edit foreign firewall policy, and
  block when foreign forwarding/rule/DNAT UCI makes the matrix claim
  unverifiable. Block explicit policy on active foreign firewall includes,
  reachable non-fw4 nftables policy, or an unreadable/malformed runtime ruleset;
- Preview returns an opaque keyed token bound to full site/fleet/ownership/plan
  state. Apply must rebuild and constant-time verify it, preflight every
  selected device before the first write, revalidate each plan at its device
  lock, require separate traversal/driver/caution/partial-fleet acknowledgments,
  run Gateway last and abort later devices on first failure. Execution detaches
  from HTTP cancellation under one `ApplyDrain` deadline; per-device quiesce
  waits through in-flight sink emission. Ownership-ledger failure after confirm
  is an error saying the device applied but controller recording failed;
- schema 13 stores a keyed, secret-free durable Apply receipt. Require a
  lowercase UUID, reject different-request reuse, return same-request replay
  without another device write, and expose parent plus ordered per-device
  write state at `GET /api/v1/site/apply/{operation_id}`;
- schema 14 seals WLAN keys, mesh keys, and secret-derived ownership verifiers
  in row-bound columns; non-secret WLAN mode/PMF remains separate. Require a
  `SecretProtector` for every store open. Verify a legacy keyring before the
  first migration write and verify the sealed database key check before WAL,
  DDL, or mutation on every v14 open. Commit ciphertext with a pending scrub,
  then idempotently checkpoint/VACUUM/checkpoint before serving. Never create or
  overwrite a keyring beside an existing database. Database and keyring backups
  are a pair; pre-v14 backups remain plaintext-sensitive and are never deleted
  without explicit operator confirmation. Local database tools require
  `OONFEE_PASSPHRASE_FILE`; read-only tools refuse pre-v14 or incomplete-scrub
  stores. WLAN/mesh API reads, including legacy `?reveal=1`, return only
  `has_key`; TLS is still required outside a trusted management network. Router
  configuration archives contain plaintext wireless keys unless encrypted as a
  stream; retain only verified encrypted archives and no temporary plaintext tar;
- schema 15 is a semantic boundary for the cross-feature policy model. Preserve
  strict desired-state records for IPv4 firewall rules, port forwards, static
  routes and client block/fixed-IP/group intent. The partial Object Manager may
  compile visible `Secure` drafts and static **network** routes only; its result
  is neither persisted nor applied. Per-device/group routing, QoS and
  application identity must return gates, not invented rules;
- schema 16 attests producer-provenanced events/cursors, topology validity
  intervals/source states, and explicit RF-scan tables/indexes/foreign keys.
  Telemetry remains rollup-only. `wifi-v1` is fixed 45/35/20 and null unless
  RSSI, retry delta and TX-failure delta coexist; never renormalize. The WAN
  series is exactly a three-packet, once/minute Gateway ICMP probe to `1.1.1.1`
  and must not be labelled HTTP/DNS/ISP uptime;
- keep Phase-4 bounds visible: OpenWrt logs 24h + 50k/device + 100k global,
  controller/audit 100k, event pages 1..1000 and producer coverage stale after
  3m; topology 31d/10k with current sources stale after 31m and historical
  source coverage unavailable; client events 2k and path enumeration 64/2048;
  radio inputs 32 radios/128 interfaces/512 frequencies/4096 scan rows,
  suggestions requiring scan ≤24h and channel plan ≤15m. Maintenance retains
  one newest terminal scan per stable radio key, never prunes pending/running
  work, and cascades discarded scan BSS rows;
- Logs/events/topology/radios/client history are REST. `/api/v1/live` accepts
  only `device.stats` and drops on a full 32-frame connection queue; do not add
  an events topic or describe it as one. RF scans are explicit,
  disruption-acknowledged and never scheduled. Existing routers may explicitly
  accept the optional oonfeeWRT controller capability installation; it installs
  or replaces one rpcd ACL JSON and unlocks supported topology, radio
  channel/scan, OpenWrt log and fixed-target WAN ICMP observations. It installs
  no package/binary/daemon/service/firmware, and administrator SSH secrets are
  request-only. Unchecked/cancelled leaves the router unchanged and source gaps
  visible. Refresh must preserve UCI, ownership and the controller login;
- authenticated Inspect is ubus-only and read-only. Keep WAN-route/LAN-DHCP
  evidence nullable, distinguish Gateway available from observed/recommended,
  and preserve `dsa-conditional` versus swconfig `observe-only` switch modes;
- an optional SSH private key is ephemeral and does not replace the ubus
  password;
- resolve an inspect/adopt hostname once and pin the chosen IP across every
  transport. On the first hard steady-state poll failure, force a fresh socket
  on the next tick without immediately discarding the rpcd session;
- DHCP enablement/start/limit/lease time are site-model data, validated before
  render; VLAN 0/1 management LANs, foreign DHCP/zone sections and flat bridges
  remain outside controller ownership.

## When validating new hardware

Run `tools/probe.py <router-ip> --write-tests --json report.json` on the target
OpenWrt router, commit `report.json` into the repo, and give the next session this
instruction: *"Resolve IMPLEMENTATION.md §14 open items against report.json;
adjust code where the mock and hardware disagree; where they disagree, hardware
wins and the mock gets fixed to match."*

That last clause matters: the mock is the contract fixture, so hardware
discoveries flow back into it — that's how CI stays honest after you've
touched real metal.

Probe stock capabilities first. Do not install, remove or upgrade router
packages to make a test pass. Optional official-feed packages need an explicit
feature benefit, exact package/dependency size and recovery headroom; under
8 MB free overlay, the default is no installation. STATUS §5be hardware-proves
the VLAN-aware WRT's bridge-VLAN/static-interface/DHCP/firewall path, durable
Apply recovery, signed-in directional WAN enforcement and DHCP off/custom
runtime behavior. Do not repeat those as pending. What remains is explicit
client-isolation/no-LAN proof plus live validation of the schema-15
cross-feature Master Table and partial Object Manager. QoS/application and
device/group routing remain honestly gated. STATUS §5bg closes the confirmed temporary-WLAN/custom-DHCP/
explicit-policy cleanup and managed-WLAN key rotation; do not repeat them as
pending. The controller must still never create VLAN-awareness itself.

**Current Phase-4 validation boundary (2026-08-20):** the live lab database is
schema 16. An initial signed-in pass exercised Topology/history, Radios,
General/Audit Logs and Client Observability under both routers' older ACLs. The
operator later accepted the separate scoped capability-refresh prompt on both
routers; later polls observed additional topology, OpenWrt-log and fixed-target
WAN ICMP sources. The refresh remains optional/default-off elsewhere. Accepting
installs or replaces one rpcd ACL JSON file; it installs no package, binary,
daemon, service or firmware and changes no UCI. No before/after package
inventory was captured, so do not claim it was unchanged. No RF scan ran.
Unchecked/cancelled leaves the router unchanged and source gaps visible.

## What NOT to delegate

Keep for yourself: reviewing every apply-engine change (it's the part that can
brick your router), the ACL file contents, and anything touching the credential
store. An agent can write these; a human signs off on them.
