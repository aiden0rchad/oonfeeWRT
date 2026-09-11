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
These are real captures of the development controller on **11 September 2026**,
built from [`c433c35`](https://github.com/aiden0rchad/oonfeeWRT/commit/c433c35d0b6dcd7329fd60592db533ff448e1fb9).
They show development changes after **v0.1.5**, including Statistics and the
separate Accounts page. The published v0.1.5 binary and container do not include
all of these changes. See the [development change summary](../reference/releases.md#development-after-v0-1-5).
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
| See how devices and clients connect | **Topology** | [Clients and topology](../guide/clients-topology.md) |
| Review channel information | **Radios** | [Radios and channel planning](../guide/radios.md) |
| Inspect a router or AP | **Devices**, then select a device | [Devices](../guide/devices.md) |
| Investigate a client | **Client Devices**, then select a client | [Client Observability](../guide/clients-topology.md#open-client-observability) |
| Manage zones, rules, and reusable objects | **Policy Engine** | [Policy Engine](../guide/policy-engine.md) |
| Connect another router or AP | **Adopt a device** | [First adoption](./first-adoption.md) |
| Configure networks, VLANs, and DHCP | **Settings → Network** | [Networks](../guide/networks.md) |
| Configure WLANs and AP groups | **Settings → Network** | [Wi-Fi](../guide/wifi.md) |
| Inspect my sessions or manage users | **Accounts** | [Accounts and roles](../operations/accounts.md) |
| Read router events or controller audit history | **Logs** | [Logs and diagnostics](../guide/logs-diagnostics.md) |
| Prepare a support bundle | **Settings → Diagnostics** | [Diagnostics](../guide/logs-diagnostics.md) |
| Export or stage controller recovery | **Settings → Backup & Restore** | [Backup and restore](../operations/backups.md) |

## Start with current health

**Dashboard** is the starting point for current health. Keep source and
freshness labels in view: a missing observation is not the same as a healthy
zero. The lower **Controller** navigation keeps **Settings**, **Accounts**, and
**Logs** together.

<DocScreenshot src="dashboard-overview" :width="1620" :height="959" alt="Dashboard with fleet counts, health observations, and controller navigation" caption="Use the fleet overview to decide where to investigate next." />

For exact meanings and speed-test limitations, follow the
[Dashboard guide](../guide/dashboard.md).

## Move from current values to history

**Statistics** separates current observation from retained history. Start with
the time range, check the selected source, and then compare changes across
charts. The same page has a device-history section for available system,
interface, and radio series.

<DocScreenshot src="statistics-internet" :width="1620" :height="959" alt="Statistics with the 24-hour range selected and Internet traffic and ICMP charts" caption="This capture compares traffic and reachability observations over 24 hours; empty intervals remain gaps. New visits default to six hours." />

See the [illustrated Statistics guide](../guide/statistics.md) for chart
coverage, device selection, and collection limitations.

## Inspect connections and radio evidence

**Topology** helps explain observed relationships. **Client Devices** opens a
per-client investigation, and **Radios** shows the channel evidence available
for each stable radio. These are complementary views, not a guarantee that
every physical link or neighbouring network has been observed.

<DocScreenshot src="topology-current" :width="1620" :height="959" alt="Current topology workspace showing observed device and client relationships" caption="Follow an observed relationship, then inspect its source evidence before drawing conclusions." />

<DocScreenshot src="radios-channel-plan" :width="1620" :height="959" alt="Radios workspace showing the channel plan and channel-state legend" caption="Channel states distinguish observed use, availability, restrictions, and unknown information." />

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
