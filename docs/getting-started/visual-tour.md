---
title: Visual tour
description: Explore the current oonfeeWRT interface with real screenshots of every main workspace and links to illustrated step-by-step guides.
---

# Visual tour

Use this page to find your way around the controller, then follow the linked
guides for the complete workflow. Screenshots show the controller in **dark
mode**, while the documentation itself supports both light and dark themes.
Select an image to open its full-resolution version in a new tab.

::: info Which version am I looking at?
These are real development-controller captures from **12 September 2026**,
using the **v0.1.6 release-candidate source before tagging**. Every illustrated
workspace below uses the refreshed interface. The older v0.1.5 binary and
container do not include all of these changes. See the
[release summary](../reference/releases.md).
:::

Device names, measurements, activity, and empty states belong to this one
development environment. They are not default configuration or performance
benchmarks. Your available controls also depend on your role, device
capabilities, and collected history.

Visible MAC addresses are covered with solid masks for sharing.

## Find the right workspace

| I want to… | Open | Illustrated guide |
|---|---|---|
| See current fleet and Internet health | **Dashboard** | [Dashboard and speed tests](../guide/dashboard.md) |
| Compare historical measurements | **Statistics** | [Statistics](../guide/statistics.md) |
| Compare periods and export a summary | **Reports** (v0.1.6) | [Reports](../guide/reports.md) |
| Review sustained conditions and delivery | **Alerts** (v0.1.6) | [Alerts](../guide/alerts.md) |
| See how devices and clients connect | **Topology** | [Clients and topology](../guide/clients-topology.md) |
| Review channel information | **Radios** | [Radios and channel planning](../guide/radios.md) |
| Inspect a router or AP | **Devices**, then select a device | [Devices](../guide/devices.md) |
| Investigate a client | **Client Devices**, then select a client | [Client Observability](../guide/clients-topology.md#open-client-observability) |
| Manage zones, rules, and reusable objects | **Policy Engine** | [Policy Engine](../guide/policy-engine.md) |
| Connect another router or AP | **Adopt a device** | [First adoption](./first-adoption.md) |
| Check official firmware metadata | **Firmware** (v0.1.6) | [Firmware](../guide/firmware.md) |
| Review external DNS or VPN observations | **Integrations** (v0.1.6) | [Integrations](../guide/integrations.md) |
| Configure networks, VLANs, and DHCP | **Settings → Network** | [Networks](../guide/networks.md) |
| Configure WLANs and AP groups | **Settings → Network** | [Wi-Fi](../guide/wifi.md) |
| Inspect my sessions or manage users | **Accounts** | [Accounts and roles](../operations/accounts.md) |
| Read router events or controller audit history | **Logs** | [Logs and diagnostics](../guide/logs-diagnostics.md) |
| Prepare a support bundle | **Settings → Diagnostics** | [Diagnostics](../guide/logs-diagnostics.md) |
| Export or stage controller recovery | **Settings → Backup & Restore** | [Backup and restore](../operations/backups.md) |

For a populated fictional network, follow the [isolated demo guide](../guide/demo.md).
It is a separate build, not the live environment pictured below. For smaller
screens, see [Mobile and installed app](../operations/mobile-app.md).

## Start with current health

**Dashboard** is the starting point for current health. Keep source and
freshness labels in view: a missing observation is not the same as a healthy
zero. The lower **Controller** navigation keeps **Settings**, **Accounts**, and
**Logs** together.

<DocScreenshot src="dashboard-overview" :width="1499" :height="982" alt="Dashboard with fleet counts, health observations, and controller navigation" caption="Use the fleet overview to decide where to investigate next." />

For exact meanings and speed-test limitations, follow the
[Dashboard guide](../guide/dashboard.md).

## Move from current values to history

**Statistics** separates current observation from retained history. Start with
the time range, check the selected source, and then compare changes across
charts. The same page has a device-history section for available system,
interface, and radio series.

<DocScreenshot src="statistics-internet" :width="1499" :height="982" alt="Statistics Internet traffic and ICMP charts with retained observations and collection gaps" caption="This capture compares traffic and reachability observations over 24 hours; empty intervals remain gaps. New visits default to six hours." />

See the [illustrated Statistics guide](../guide/statistics.md) for chart
coverage, device selection, and collection limitations.

## Explore the new v0.1.6 workspaces

These four workspaces extend the existing controller without making every
page a live router operation. Reports reads retained measurements, Alerts
evaluates existing evidence, Firmware checks official release metadata, and
Integrations contacts an explicitly selected service only when you request a
check.

### Reports: compare periods with their coverage

Choose a period, review the gateway and observed-interval counts, and compare
the summaries with the preceding period. Sparse history remains visibly sparse;
missing observations do not become zeros.

<DocScreenshot src="reports-overview" :width="1499" :height="982" alt="Reports workspace comparing observed WAN summaries across two periods" caption="Averages and coverage belong together. This live development snapshot is not an ISP uptime report or a total-usage measurement." />

Follow the [Reports guide](../guide/reports.md) for weighting, period boundaries,
gateway changes, and CSV exports.

### Alerts: follow sustained conditions

Review rules, current condition states, incidents, and optional delivery in one
place. Rule evaluation and external notifications are separate: an Owner must
explicitly configure delivery before events can be sent elsewhere.

<DocScreenshot src="alerts-overview" :width="1499" :height="982" alt="Alerts workspace showing rule, incident, and optional delivery sections" caption="No rule was saved and no delivery destination was configured for this capture. Missing evidence is unknown, not proof of a healthy or recovered condition." />

The [Alerts guide](../guide/alerts.md) includes the unsaved rule form, condition
requirements, cooldown behavior, and notification boundaries.

### Firmware: check a reported version against the catalogue

Review the device's stored identity, then explicitly request an official
same-branch catalogue check. Firmware image selection and firmware installation
are not the same operation; this workspace does not flash routers.

<DocScreenshot src="firmware-check" :width="1499" :height="982" alt="Current in branch result for the Linksys WRT3200ACM and OpenWrt 25.12.5" caption="The 12 September 2026 check matched this device to OpenWrt 25.12.5. This is one-device catalogue evidence—not an installed-package security assessment, image-byte verification, or completed firmware upgrade." />

Read the [Firmware guide](../guide/firmware.md) before interpreting a match or
considering an external OpenWrt installation workflow.

### Integrations: connect only what you intend to inspect

AdGuard Home and WireGuard provide optional, read-only visibility. The overview
does not invent measurements when no connection or observation exists. Merely
opening the page does not configure DNS, install a helper, or create a tunnel.

<DocScreenshot src="integrations-overview" :width="1499" :height="982" alt="Integrations overview with AdGuard Home not connected and no requested WireGuard observation" caption="No service was connected for this capture. An unconfigured integration is not evidence that an existing DNS service or VPN is offline." />

The [Integrations guide](../guide/integrations.md) illustrates the unsaved
connection form and explains credentials, permissions, and explicit checks.

## Inspect connections and radio evidence

**Topology** helps explain observed relationships. **Client Devices** opens a
per-client investigation, and **Radios** shows the channel evidence available
for each stable radio. These are complementary views, not a guarantee that
every physical link or neighbouring network has been observed.

<DocScreenshot src="topology-current" :width="1499" :height="982" alt="Current topology workspace showing observed device and client relationships" caption="Follow an observed relationship, then inspect its source evidence before drawing conclusions." />

<DocScreenshot src="radios-channel-plan" :width="1499" :height="982" alt="Radios workspace showing the channel plan and channel-state legend" caption="Channel states distinguish observed use, availability, restrictions, and unknown information." />

Continue with [clients and topology](../guide/clients-topology.md) or
[radios and channel planning](../guide/radios.md).

## Review configuration before changing routers

Use **Settings → Network** for site configuration and **Policy Engine** for
zones, rules, and reusable objects. Editing controller intent, previewing it,
and applying it are separate steps. A screenshot of an editor is not evidence
that its contents have been applied to a router.

<DocScreenshot src="network-overview" :width="1165" :height="982" alt="Settings Network tab with site configuration sections" caption="Network configuration belongs in Settings; account administration now has its own workspace." />

The [network](../guide/networks.md), [Wi-Fi](../guide/wifi.md), and
[policy](../guide/policy-engine.md) guides illustrate the individual sections.
For a new device, start with the [adoption walkthrough](./first-adoption.md).

## Manage access and recovery separately

**Accounts → My account** holds your password and sessions. Owners can use
**Manage accounts** to administer other users. Diagnostics and backup/restore
remain under **Settings**, while router events and controller audit history
are in **Logs**.

<DocScreenshot src="accounts-manage" :width="1165" :height="982" alt="Manage accounts tab showing account creation, role selection, and existing accounts" caption="The selected role has an explanation beside the form; account-management permissions remain owner-only." />

Use the illustrated [account guide](../operations/accounts.md),
[logs and diagnostics guide](../guide/logs-diagnostics.md), and
[backup/restore guide](../operations/backups.md) for the detailed procedures.
