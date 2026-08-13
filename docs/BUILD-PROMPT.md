# Driving a build session (for Opus 5 on high, or any capable coding agent)

This repo is designed so a coding agent can build it milestone by milestone
without inventing anything. This file is the operating manual for those
sessions.

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

## When hardware arrives

Run `tools/probe.py <router-ip> --write-tests --json report.json` on the real
WRT3200ACM, commit `report.json` into the repo, and give the next session this
instruction: *"Resolve IMPLEMENTATION.md §14 open items against report.json;
adjust code where the mock and hardware disagree; where they disagree, hardware
wins and the mock gets fixed to match."*

That last clause matters: the mock is the contract fixture, so hardware
discoveries flow back into it — that's how CI stays honest after you've
touched real metal.

## What NOT to delegate

Keep for yourself: reviewing every apply-engine change (it's the part that can
brick your router), the ACL file contents, and anything touching the credential
store. An agent can write these; a human signs off on them.
