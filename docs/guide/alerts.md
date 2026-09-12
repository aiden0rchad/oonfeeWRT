---
title: Alerts and webhook notifications
description: Configure sustained offline, WAN latency, and WAN loss conditions with honest evidence states and optional encrypted webhook delivery.
---

# Alerts and webhook notifications

::: info Added in v0.1.6
Alerts was introduced in v0.1.6. The screenshots were captured on 12 September
2026 from the v0.1.7 release-candidate UI source, before tagging; no rule or external
notification destination was enabled for the captures.
:::

Alerts turn the controller's existing observations into conditions you can
follow over time. They answer questions such as “Has this router remained
offline?” and “Has the gateway's measured loss stayed above my threshold?”

The controller evaluates rules even when no browser tab is open. It stores
condition state, incident history, cooldowns, and pending notifications in its
own database. A missing observation is **unknown**, not a healthy measurement
and not confirmation that an existing incident recovered.

<div class="write-impact"><strong>Router write impact</strong><span>Alert rules are controller-local. Evaluation uses existing inventory and stored telemetry; it does not contact, reconfigure, restart, or install software on a router. Optional webhook delivery makes outbound HTTPS requests only after an Owner explicitly configures and enables it.</span></div>

<DocScreenshot src="alerts-overview" :width="1600" :height="1000" alt="Alerts workspace with rule, incident, and notification-delivery sections" caption="Rules, incidents, and optional external delivery are separate. No rule was saved and no notification delivery was configured for this capture." />

## Create a rule

1. Sign in as an **Owner** and open **Insights → Alerts** in the sidebar
   (direct route: `/alerts`).
2. Choose an adopted device and one of the conditions below.
3. Give the rule a short, recognizable name.
4. Set its threshold, sustained duration, and notification cooldown.
5. Enable and save the rule. Allow the next one-minute evaluation to run.

<DocScreenshot src="alerts-rule" :width="1600" :height="1000" alt="Unsaved alert rule form with device, condition, sustained-duration, and cooldown controls" caption="Review the target, condition, sustained duration, and cooldown before saving. This form was left unsaved; it did not create a rule or send a notification." />

All signed-in roles can read rules and incidents. Only Owners can create,
change, or remove rules and configure notification delivery. The endpoints
retain the controller's normal session and CSRF protections.

| Condition | Threshold | Required evidence |
|---|---|---|
| Device offline | Not applicable | An adopted device that has answered before, has active polling, and has exceeded its effective offline timeout |
| WAN latency | Greater than the configured milliseconds | Fresh, consecutive five-minute ICMP latency rollups from the currently observed managed gateway |
| WAN loss | Greater than the configured percentage | Fresh, consecutive five-minute ICMP loss rollups from the currently observed managed gateway |

The offline timeout follows the device's effective polling interval, including
an intentionally widened interval. Pending adoption, never-observed devices,
and polling temporarily suspended for configuration work do not produce a
fabricated offline reading.

WAN probes measure the gateway's ICMP path to `1.1.1.1`. They do not prove the
availability of every Internet service, DNS resolution, or complete ISP uptime.
A monitor-only router or a device without proved current managed-gateway
placement cannot provide these WAN rule inputs.

### Sustained duration and cooldown are different

- **Sustained duration** is how long recurring evidence must show the condition
  before an incident opens. The allowed range is 60 seconds to 24 hours.
- **Cooldown** limits new firing notifications from a rule after a previous
  firing notification was queued. The allowed range is 60 seconds to seven days.
- A continuing incident is not sent repeatedly every minute. A fresh recovery
  is a separate transition and can produce its own notification.

WAN evidence is recorded in five-minute buckets. A one-minute sustained setting
does not turn those samples into minute-resolution measurements: another fresh
bucket is still required. Reading the same retained point repeatedly never
satisfies a hold. Missing buckets or a long evaluation interruption reset a
pending hold rather than inventing continuity.

Maximums: 50 rules; 80 characters in a rule name; latency threshold above zero
and at most 60,000 ms; loss threshold above zero and at most 100%.

## Understand a rule's state

| State | Meaning | What to do |
|---|---|---|
| Disabled | Evaluation is switched off for this rule | Enable it when monitoring is wanted |
| Unknown | Fresh usable evidence is unavailable | Check device reachability, polling state, gateway selection, and telemetry coverage |
| Pending | The condition is observed, but the sustained duration is not yet proved | Allow fresh observations to continue; inspect the source rather than assuming an alarm |
| Firing | A sustained condition opened an incident | Investigate the router or WAN path named in the incident |
| Clear | The latest usable observation is within the rule threshold | No action is required for that observation |

An incident remains open through unknown coverage. It resolves only after a
newer usable observation proves recovery. The incident list and the current
rule state therefore can differ: an open incident alongside an unknown rule
means “recovery has not been confirmed,” not a contradictory health score.

Disabling a rule stops evaluation and cancels its pending notification attempts,
including retries. It does not falsely mark an active incident resolved.
Re-enabling the rule does not replay those cancelled notifications.
Changing its target or condition while that incident is active is blocked;
wait for observed recovery or remove the rule. Removing a rule stops its
evaluation and pending delivery, but retains its historical incident record.

Rule targets also bind to the original device identity internally. Removing a
router and later reusing its numeric inventory ID cannot silently redirect an
old rule to a different device.

## Enable optional webhook delivery

Local incident monitoring works without an external service. To receive events
elsewhere, configure a receiver that accepts a generic JSON webhook.

1. Open the notification delivery configuration as an Owner.
2. Enter the receiver's **public HTTPS** URL.
3. If required by the receiver, enter its bearer token.
4. Enable delivery and save. Saving does not send a test notification.
5. When a new qualifying condition occurs, inspect delivery status in Alerts
   and confirm receipt in the receiving service.

The entire destination URL and bearer token are encrypted with the controller
keyring. Read responses expose only the destination hostname and whether a
destination is configured; they never return its URL path, query string, token,
or ciphertext. Leaving a secret field omitted preserves its saved value;
clearing the URL removes the destination when delivery is disabled.

This is a **generic webhook**, not an implicit Telegram, Slack, or Discord API
adapter. Services with a different required payload need an adapter you control.

### Network and privacy boundary

- HTTPS and valid TLS certificates are required. Redirects are not followed.
- Localhost, private networks, link-local addresses, reserved ranges, and
  metadata-service addresses are blocked. Self-hosted private-LAN receivers are
  not supported by this delivery path.
- DNS answers are checked at send time and the actual connection is pinned to
  an approved public address. A hostname resolving partly to private addresses
  is rejected. Environment proxy settings cannot bypass this check.
- The receiver sees the controller's outbound public address and the incident
  fields, including rule name, device name, condition, value, and timestamps.
  Configure delivery only to a destination you trust with that information.

### Payload and delivery behavior

Each request uses `Content-Type: application/json` and includes:

```json
{
  "event_id": "oonfeewrt-alert-12-firing",
  "event": "firing",
  "at": 1800000000,
  "incident": {
    "id": 12,
    "rule_id": 3,
    "rule_name": "Gateway latency",
    "device_id": 1,
    "device_name": "Gateway",
    "condition": "wan_latency",
    "state": "firing",
    "started_at": 1800000000,
    "resolved_at": null,
    "value": 130,
    "delivery_state": "pending",
    "delivery_error": ""
  }
}
```

All API and notification timestamps are Unix **seconds**. `resolved_at` is
`null` until a newer observation confirms recovery. The request also includes
`X-OonfeeWRT-Event-ID` with the same stable event identifier.

The receiver should return a `2xx` response and deduplicate by `event_id`.
Delivery is **at least once when retried, not exactly once**: the receiver may
accept a request just before the controller loses its response or restarts.

Delivery is bounded to two attempts per evaluation, a five-second request
timeout, up to three attempts per event, five minutes between retries, and a
30-minute pending-event lifetime. The queue holds at most 100 events. Failures,
expiry, queue exhaustion, and cooldown suppression are visible; transport
errors are redacted so destination credentials cannot appear in status text.

The controller retains up to 500 incident records and returns the newest 100
in the workspace. Configuration changes cancel pending delivery rather than
replaying old incidents to a newly selected destination. A restart waits one
evaluation cadence before processing; cooldowns and open incidents survive.

### Restoring a controller backup

A portable restore pauses external webhook delivery and cancels queued
notifications before the restored database can become active. It preserves
rules, incident history, cooldown history, and the encrypted destination.
Pending holds and the last evaluation are reset; existing open incidents are
not marked recovered without newer evidence.

After restoring, review the destination and the new controller environment in
**Alerts → Notification delivery**, then explicitly enable external delivery
when ready. Re-enabling does not replay cancelled historical notifications.
In-app rule evaluation can resume independently of external delivery.

## Troubleshoot without hiding useful evidence

| Symptom | Checks |
|---|---|
| Rule stays unknown | Confirm the router has completed adoption and a successful poll; check whether polling is suspended and whether its identity still matches |
| WAN rule stays unknown | Confirm the selected device is the observed managed gateway and Statistics contains consecutive fresh ICMP buckets |
| Hold takes longer than expected | WAN resolution is five minutes; a gap restarts the pending hold, and a retained point cannot advance it |
| New incident has no notification | Check whether delivery is configured and enabled, whether the rule is in cooldown, and whether the pending queue is full |
| Webhook delivery failed | Check public DNS, HTTPS/TLS validity, receiver authentication, and its required JSON format; confirm it returns `2xx` without redirecting |
| Open incident becomes unknown | Observations stopped before recovery could be confirmed; restore coverage rather than treating missing data as success |
| Rule editing is blocked | The incident is still active. Disable the rule to stop evaluation, or wait for proved recovery before changing its condition |

Alerts do not replace [Logs](./logs-diagnostics), detailed [Statistics](./statistics), or
manual investigation. They provide a compact, sustained-condition view of the
same observed network, with delivery controls that remain explicit.
