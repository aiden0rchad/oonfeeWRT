---
title: Flow visibility feasibility
description: Evidence, constraints, and release gates for future traffic accounting and application identification on OpenWrt gateways.
---

# Flow visibility feasibility

This page records the Phase 5 feasibility decision reviewed on **September 10,
2026**. It is an engineering boundary, not a promise that deep-packet inspection
(DPI) is active in v0.1.5.

## Decision

oonfeeWRT will not install or enable a DPI daemon in v0.1.5. The safe next step
is a separately reviewed, default-off hardware pilot. Until that pilot passes,
the controller must not show application names, risk scores, or flow history as
available.

Two official OpenWrt packages are relevant, but they answer different questions:

| Candidate | What it can establish | What it cannot establish | v0.1.5 decision |
|---|---|---|---|
| [`nlbwmon`](https://github.com/jow-/nlbwmon) | Per-host IPv4/IPv6 traffic, byte/packet/connection counts, and configurable port-based protocol labels from conntrack | Reliable application identity inside encrypted or shared-port traffic | Preferred first accounting pilot; not DPI |
| [`netifyd`](https://gitlab.com/netify.ai/public/netify-agent) | nDPI-backed protocol/application classification and local JSON flow metadata | Acceptable router cost, classification accuracy, or compatibility with hardware/software flow offload on an unmeasured device | DPI pilot only after resource and privacy gates |

Both packages are present in the official OpenWrt 25.12.5 package feed for the
three release architectures checked by this review:
`aarch64_cortex-a53`, `arm_cortex-a9_vfpv3-d16`, and `mips_24kc`.
Availability is necessary, but it is not evidence that enabling either daemon
is safe on a particular router.

The checked `netifyd` package is 4.4.7-r2 and its compressed package is roughly
1 MiB before dependencies. Its OpenWrt package depends on packet capture,
conntrack, HTTP, C++, compression, and support libraries. The default upstream
configuration exposes a local Unix socket and has its remote sink disabled.
oonfeeWRT must preserve the local-only boundary and verify it after installation;
it must not rely on a default remaining unchanged in a future package.

The checked `nlbwmon` build is 2025.06.02~29236be6-r1. Its package is much
smaller, but it maintains its own accounting database on the router. A pilot
must relocate, bound, or disable persistent history before the controller can
claim zero unexpected flash wear.

## Why package presence is not enough

DPI observes the data plane rather than a low-frequency management endpoint.
The cost can therefore scale with traffic volume, connection count, packet
rate, and enabled classification rules. It may also invalidate hardware or
software flow-offload assumptions that a constrained gateway needs for normal
throughput.

This creates four independent gates:

1. **Package gate:** the exact package and every dependency must come from the
   device's configured official OpenWrt feeds. An unavailable target is
   unsupported; the controller never uploads a replacement executable.
2. **Storage gate:** the plan must state download size, installed size, free
   space, files, services, configuration, and expected persistent writes.
3. **Performance gate:** the same router must be measured before, during, and
   after the pilot under idle and representative routed load.
4. **Privacy gate:** collection stays local. Remote sinks, cloud enrollment,
   telemetry submission, and vendor accounts remain disabled and are verified
   rather than assumed.

Failing any gate keeps the feature absent for that device. A greyed-out or empty
Flows screen must not imply that observation happened when it did not.

## Required pilot workflow

The optional-capability workflow must remain separate from adoption and normal
Apply:

1. Read the device target, release, package manager, free storage, memory,
   offload configuration, and current package/service/configuration state.
2. Refresh or query official package metadata only after explicit operator
   approval.
3. Present an exact plan containing packages and dependencies, sizes, commands,
   files, service behavior, traffic/privacy impact, rollback, and known gaps.
4. Require a fresh unchecked acknowledgement bound to that plan.
5. Install on one non-critical gateway. Do not start fleet-wide rollout.
6. Verify local-only configuration before admitting any collected record.
7. Run the resource matrix below and record the raw evidence.
8. Stop and remove only ledger-recorded additions, restore prior files and
   service state, then verify package/configuration drift.

Cancellation before step 4 makes no router change. A failed or partial install
must remain visible and must block un-adoption until cleanup is proved or the
operator explicitly accepts the residue.

## Resource matrix

The pilot needs paired measurements on the same router and traffic path:

| Condition | Required observations |
|---|---|
| Baseline idle | Controller polling cost, router CPU/load, free memory, request rate, flash writes |
| Baseline routed load | Throughput, latency/loss, CPU/load, offload state, connection count |
| Collector enabled, idle | Added CPU, memory, processes, sockets, database writes, log volume |
| Collector enabled, routed load | Throughput delta, latency/loss delta, CPU saturation, drops, thermal or watchdog events |
| Collector stopped | Process/socket closure and return toward baseline |
| Rollback complete | Package, service, file, UCI, offload, and persistent-write state match the recorded baseline |

At least one comfortable 64-bit gateway and one constrained target must pass.
No default can be derived from a desktop build, a package listing, or a mock.
Class-C hardware remains unavailable for DPI until it has its own measurement.

## Data contract if a pilot passes

Future flow records must retain their evidence boundary:

- source package, version, device, collection time, and coverage interval;
- observed addresses, ports, byte/packet counts, and direction only when the
  source supplied them;
- port-based protocol and DPI application labels as different fields;
- confidence/provenance instead of converting an unknown application into
  `Other` without explanation;
- bounded payloads, query windows, per-device retention, and global retention;
- local pseudonymization/redaction in diagnostics; and
- deletion/retention behavior that is tested under sustained input.

Risk is not an application name. A future risk score needs a separately
documented rule set, source timestamps, explainable contributing evidence, and
an explicit `unknown` state. No paid-feed or cloud verdict may be implied.

## What v0.1.5 may truthfully say

v0.1.5 may say that Phase 5 was evaluated against current official OpenWrt
packages and that a safe pilot contract is documented. It may not say that DPI,
application identity, flow history, traffic maps, or automated risk scoring is
shipped.

Until the pilot closes, current interface and telemetry counters remain the
authoritative traffic visibility. `nlbwmon` can become a separate accounting
capability without changing that DPI boundary.

## Primary references

- [OpenWrt 25.12.5 downloads](https://downloads.openwrt.org/releases/25.12.5/)
- [OpenWrt `netifyd` package source](https://git.openwrt.org/feed/packages/?h=openwrt-25.12&p=feed/packages.git&a=tree&f=net/netifyd)
- [OpenWrt `nlbwmon` package source](https://git.openwrt.org/feed/packages/?h=openwrt-25.12&p=feed/packages.git&a=tree&f=net/nlbwmon)
- [`netifyd` upstream source](https://gitlab.com/netify.ai/public/netify-agent)
- [`nlbwmon` upstream source](https://github.com/jow-/nlbwmon)

