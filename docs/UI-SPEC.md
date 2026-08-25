# oonfeeWRT — UI Specification

Modeled originally on UniFi OS 5.1.19 / Network 10.4.57 (July 2026), dark
theme, and re-baselined on **2026-08-18** against the stable **UniFi Network
10.5.67** Client Observability and Safe Ops screens.

**Legal framing:** this spec describes *layout patterns and information
architecture*, which are not protectable in the way assets are. Do not copy
Ubiquiti's icons, illustrations, fonts, CSS, or wordmark. Build the equivalent
components from scratch. See `RISKS.md`.

### Current reference, not a frozen clone

The current UniFi visual language is denser and more task-oriented than the
older Dashboard-first reference:

- a narrow global icon rail, then a contextual rail rather than one wide menu;
- synchronized multi-pane workspaces for investigation;
- small, quiet controls and borders with blue reserved for selection/action;
- detail stays in the workspace instead of navigating through a stack of pages;
- safety state is visible beside the setting it protects.

Client Observability is the clearest current reference: facets → client list →
24-hour event spine → correlated Client Health, AP Health and Site Health
charts. One cursor drives every panel. Safe Ops uses a settings-category rail,
an infrastructure summary column and one focused configuration pane. These are
layout patterns only; do not copy Ubiquiti assets or branding. The current
policy pattern is similarly two-level: an inspectable Policy Engine master
table, plus Object Manager for outcome-first `Secure`, `Route` and `QoS`
configuration. The Zone Matrix is directional: source zones are rows,
destination zones are columns, and a cell opens the concrete policies for that
pair.

Official references: [UniFi Network 10.5.67 release notes](https://community.ui.com/releases/UniFi-Network-Application-10-5-67/375288b9-a4b4-46f1-a19d-5c787d342c2b),
[Traffic & Policy Management](https://help.ui.com/hc/en-us/articles/5546542486551-Traffic-Policy-Management-in-UniFi),
and [Zone-Based Firewalls](https://help.ui.com/hc/en-us/articles/115003173168-Zone-Based-Firewalls-in-UniFi).

**Published and historical hardware boundary (2026-08-22):** the lab used
schema 17 for this proof, and `v0.1.0-rc.1` retains that UI contract.
Both factory-reset reference routers were re-adopted only after the operator
accepted the default-off controller-access-payload disclosure; adoption
installed no package, binary, daemon, service or firmware. The separate,
default-off official-feed `lldpd` workflow was exercised through exact planning,
installation, physical-interface configuration, read-only diagnosis,
drift-checked rollback and clean reinstallation on both routers. V39 remains the
historical startup-fix checkpoint; v40 supplied the signed-in deep-link and
complete-poll evidence before `v0.1.0-rc.1` was published and clean-tested from
its public artifacts.
All durable Phase-4 data is REST; the WebSocket supplies only `device.stats`
focus/current updates.

**Current schema-19 UI and historical live checkpoint:** the post-`v0.1.0` source
renders equal-height WAN-health and controller-speed-test cards. Internet health
shows the observed gateway, default-route interface and fixed-target probe as
separate evidence, then plots server-selected six-hour throughput, latency and
loss as zero-based five-minute activity columns. Missing buckets remain missing
and appear only in a thin coverage rail; a table toggle exposes every aligned sample.
The speed-test review shows controller-host vantage point, Cloudflare
endpoint, single-stream method, 15 MiB estimate, 30-second limit, privacy and
saturation before consent; Start binds a fresh acknowledgement to the reviewed
`plan_id`, then shows progress/cancel and bounded history as paired
download/upload bars on one zero-based Mbps scale, with a full table toggle.
Loaded latency/jitter display unavailable. Schema 19 also supplies role-bearing
sessions, exhaustive server-side RBAC, My Account, owner account administration,
password step-up and session revocation. The Diagnostics screen supplies the
fixed stored-evidence
disclosure, one cancellable job, bounded history and private ZIP download for
owner/admin; it makes zero router management/API/SSH calls and zero router
changes. The bounded rotating controller log sink is implemented. The
owner-only Backup & Restore screen now exports encrypted
native `.oowrtbak` artifacts, accepts bounded raw uploads, runs disposable
authenticated previews, and presents plan-bound destructive confirmation plus
the persistent router-write suppression/resume gate. The controlled live
upgrade/restart completed with exact binary version
`dev-phase41-live-schema19` at schema 19. Recovery verification reports two
devices, two credentials, one enabled owner, one WLAN and no mesh. A signed-in
live UI smoke passed Dashboard, Accounts, Diagnostics, Backup & Restore,
Devices and Topology with no browser errors; fresh schema-17 rollback and
schema-19 recovery sets also passed verification. This was a route/render smoke,
not a dark/light visual audit, diagnostics generation/download, backup export,
restore execution, public-provider speed-test run or router restore. The
completed `v0.1.0` tag workflow and GitHub Release are the
publication authority and own the final isolated release evidence.

---

## 1. Frame

```
┌────────────────────────────────────────────────────────────────────────┐
│ [Site ▾] [App tabs]            oonfeeWRT            [◐ theme] [avatar]  │  40px topbar
├──┬──────────────────────┬──────────────────────────────────────────────┤
│  │                      │                                              │
│I │  Context rail        │   Content                                    │
│c │  264px, collapsible  │   fills                                      │
│o │  «                   │                                              │
│n │  filters / cards     │   ┌────────────────────────────┐             │
│  │                      │   │  Detail slide-over  370px  │  ← optional │
│64│                      │   └────────────────────────────┘             │
│px│                      │                                              │
└──┴──────────────────────┴──────────────────────────────────────────────┘
```

**Navigation rail (64px collapsed).** The landed first slice uses one route list;
a remaining visual-polish pass will split it into two groups separated by a
divider and add a dedicated hover treatment.
Phase 4.1 first covers the routes that exist now: Dashboard, Topology, Radios,
Devices, Client Devices, Policy, Settings, Adopt and Logs. Future routes in §2
do not get empty placeholders. Use one project-owned inline SVG set—never font
glyphs or raster icons. Icons are 22–24px in controls at least 44px square, with
a consistent stroke, visible focus/active states, accessible names and
tooltips. The rail expands to show text labels and stores its preference locally,
namespaced by controller and account; collapsed mode remains keyboard-usable.

**Context rail (264px).** Screen-specific. Two personalities:
- *Filter rail* (Topology, Clients, Devices, Observability, Insights, Flows,
  Logs) — stacked collapsible filter groups, each option with a live count,
  sticky footer links (`Clear Filters`, `Customize Columns`, `Download`).
- *Card rail* (Dashboard) — stacked summary cards.

Collapse chevron `«` sits on the rail's outer edge, vertically near the top.

**Detail slide-over (370px).** Enters from the right over the content area,
never over the rail. Header = entity name + close. Sub-navigation as an icon
segmented control (overview / stats / settings). Body = stacked property groups
and mini-charts. Used for: device detail, client detail, log entry detail.

**Content header.** Left: sub-tab segmented control (`Topology | Infrastructure`,
`Main | IP Table`, `Flows | Activity`). Right: time range segmented control
(`1h 1D 1W 1M` + calendar icon) and series toggles.

**Progressive disclosure.** Use one `Notice`/`Disclosure` primitive instead of
ad hoc prose or character-count truncation. Its authored one- or two-line
summary always names severity, affected component and consequence. `More
information` expands the complete technical details with native keyboard
semantics and `aria-expanded`; action buttons and acknowledgements remain
visible outside the collapsed region. Informational and coverage details default
closed. An optional capability initially shows a compact summary plus `Review`;
once reviewed, the exact router mutation/rollback, fresh acknowledgement and
action remain visible/default-open. Security, destructive, connectivity-loss
and active-operation essentials are never line-clamped. Apply this contract to
coverage gaps, adoption/ACL, LLDP, RF scan, speed test, Apply preview,
diagnostics, backup/restore and long Settings help.

---

## 2. Navigation map

```
Dashboard
Topology            → Topology · Infrastructure
WiFi
Devices             → Main · SFP  ▸ per-device: Overview · Stats · Settings · Ports
Client Devices      → Main · IP Table
Observability       → Clients · 24h Timeline
Insights            → Radios · Coverage · RF Scan
Flows               → Flows · Activity
Logs                → General · Audit
Alarm Manager       → Triggers · Scope · Actions
Settings
 ├ Overview         (summary tables for every domain, with Create New / Manage)
 ├ WiFi
 ├ Networks
 ├ Internet
 ├ VPN
 ├ Policy Engine    → Objects · Master Table · Zone Matrix
 │                    Master Table facets: Firewall · Filtering · Routes · QoS · ACL · NAT/DNS
 ├ Security         (IDS/IPS, blocklists)
 ├ High Availability → Safe Apply · Recovery · Link Protection
 ├ My Account       (password, own sessions)
 ├ Accounts         (owner-only users, roles, state, session revocation)
 ├ Diagnostics      (redacted support-bundle preview and download)
 ├ Backup & Restore (encrypted export and staged owner-only restore)
 ├ System           (updates, timezone, SIEM export, notifications)
 └ Console          (control plane, identity, device credentials)
```

This is the target map. My Account, Accounts, Diagnostics and Backup & Restore
now exist. Other future entries remain specifications and do not get empty
navigation destinations.

The **Settings → Overview** page is a strong pattern worth copying exactly: every
domain gets a collapsible card containing a summary table plus `Create New |
Manage` footer links. It makes the settings area browsable rather than a menu
maze. Build this page early — it doubles as your integration test surface.

---

## 3. Design tokens

### Surfaces & ink (dark — the primary theme)

```css
--surface-0:      #0F1114;   /* app background */
--surface-1:      #16181C;   /* cards, rails, tables */
--surface-2:      #1E2126;   /* raised: hover rows, popovers */
--border:         #2A2E35;
--border-strong:  #3A3F48;

--text-primary:   #F2F4F7;
--text-secondary: #A0A6B0;
--text-muted:     #6B7280;

--accent:         #3987e5;   /* links, selected chips, primary buttons */
--accent-soft:    #1D3B63;   /* selected row / chip background */
```

Light theme mirrors this from the same ramps; it is **selected, not an inverted
flip** — re-validate every series color against the light surface before shipping
it.

### Status (reserved — never reused as a series color)

```css
--good:     #199e70;   /* online, Excellent, allow */
--warning:  #c98500;   /* degraded, medium severity */
--serious:  #d95926;   /* poor experience, high interference */
--critical: #e66767;   /* offline, blocked, threat */
```

Status always ships with an icon or text label. The dot alone is never the only
signal — UniFi leans on color-only dots in several tables and it is a genuine
accessibility flaw. Don't inherit it: pair every dot with a text status in the
row or an accessible label.

### Categorical series palette (validated)

Eight slots, assigned in fixed order, never cycled. A ninth series folds into
"Other" or becomes a small-multiples facet.

| Slot | Hue | Dark | Light |
|---|---|---|---|
| 1 | blue | `#3987e5` | `#2a78d6` |
| 2 | orange | `#d95926` | `#eb6834` |
| 3 | aqua | `#199e70` | `#1baf7a` |
| 4 | yellow | `#c98500` | `#eda100` |
| 5 | magenta | `#d55181` | `#e87ba4` |
| 6 | green | `#008300` | `#008300` |
| 7 | violet | `#9085e9` | `#4a3aa7` |
| 8 | red | `#e66767` | `#e34948` |

Validated against `--surface-1` (`#16181C`): lightness band ✅, chroma floor ✅,
adjacent-pair CVD ΔE 8.4 ✅, normal-vision ΔE 19.3 ✅, contrast ≥3:1 ✅.

**Rule:** color follows the *entity*, not its rank. Filtering the Top Apps list
must not repaint the survivors — "Discord" is orange in every chart on every
screen, forever.

Scatter/bubble/map forms use only the **first three slots** all-pairs; beyond
that, facet.

Sequential (heatmaps — the Channel Plan grid, the TX-retries timeline): one hue,
light→dark blue ramp. Diverging (only where polarity is real): blue↔red with a
gray midpoint.

### Type & density

| Role | Size | Weight |
|---|---|---|
| Table header | 11px | 600, `--text-secondary` |
| Table cell | 13px | 400 |
| Card title | 13px | 600 |
| Property label | 12px | 400, `--text-secondary` |
| Property value | 12–13px | 500, `--text-primary` |
| Hero number | 28–34px | 600, tabular-nums |

Row height 32–34px. Numeric columns right-aligned, `font-variant-numeric:
tabular-nums`. System font stack — do not ship Ubiquiti's typeface.

### Shape

Cards: 8px radius, 1px `--border`, no drop shadow (the dark theme separates by
value, not elevation). Chips/pills: 4px radius, 11px text. Buttons: 6px radius,
28px tall.

---

## 4. Chart conventions

### The dual-axis problem — read this before building the Dashboard

UniFi's main Dashboard chart plots **latency (ms) on the left axis and throughput
(Mbps) on the right**. Dual-axis charts are the single most common way to imply
a correlation that isn't there — the visual crossing point is an artifact of two
arbitrary scale choices.

You have two defensible options. Pick one deliberately:

- **A — Faithful (default for parity).** Reproduce the dual axis, but make the
  axis-to-series binding unmistakable: color the axis labels and ticks to match
  their series, and keep the two groups visually separated (throughput as filled
  area, latency/loss as thin dashed/dotted lines above it). This is what UniFi
  does and what your reference screenshots show.
- **B — Correct (recommended).** Stack two panels sharing one x-axis and one
  crosshair: throughput on top, latency + packet loss below. You lose nothing,
  the reading is unambiguous, and the crosshair still ties them together.

Ship B behind a "Combined axes" toggle defaulting to A if fidelity matters more
to you than the principle. Document the choice; don't drift into it.

Everywhere else in the app: **one axis per chart.**

### Marks

- Lines 2px, no point markers on dense series; markers ≥8px only on sparse ones.
- Area fills at ~18% opacity of the series color, hard 2px surface gap between
  stacked segments.
- Bars: 4px rounded data-end anchored to the baseline; square at the baseline.
- Grid: horizontal only, 1px `--border` at ~40% — recessive. No vertical grid on
  time series (the crosshair does that job).
- Never a number on every point. Direct-label the last value of ≤4 series;
  everything else lives in the tooltip.

### Interaction (mandatory, not optional)

Every time-series chart ships a **crosshair + tooltip** on hover showing the
timestamp range and every series' value at that x — exactly the pattern in the
Dashboard screenshot (`Jul 24 11:20 PM – 11:25 PM` with four rows). Bar, donut,
and heatmap cells get per-mark tooltips.

Legend present whenever ≥2 series. Series toggles live in one row above the
chart (the `☑ Avg. Latency ☑ Packet Loss ☐ Connections` pattern), with the color
swatch rendered in the series' actual mark style (dashed line for a dashed
series) so the legend teaches the encoding.

Time range control top-right, always `1h · 1D · 1W · 1M · 📅`. The chart must
switch rollup resolution to match (see ARCHITECTURE §5) and show min/max bands
at coarse resolutions so spikes survive aggregation.

### Chart-form assignments

| Screen element | Form | Note |
|---|---|---|
| WAN throughput / latency over time | area (throughput) + line (latency) | see dual-axis note |
| Per-port / per-interface traffic | multi-line | one axis, Bps or Bytes toggle |
| Traffic by application | donut + ranked table beside it | donut only because the table carries the numbers |
| Connections by WiFi generation | donut + table | same |
| Channel Plan | categorical heatmap grid | legend: In Use / Enabled / DFS / Unavailable / Excluded |
| TX retries over time (device panel) | horizontal segmented timeline bar | sequential ramp, low→high |
| ISP sparkline (card) | bare area sparkline, no axes | tooltip on hover |
| Uptime strip | binary status bar | good/critical only |
| Experience score | stat tile + fixed three-component breakdown on hover | never a bare number — show RSSI score, retry delta and TX-failure delta. `wifi-v1` is all-or-null: if any input is unavailable, show the named gap and no score; never renormalize the remaining weights |

---

## 5. Table system

Every major screen is a filtered, virtualized data grid. Build one component.

Requirements:
- Virtualized rows (Flows and Logs run to 10k+ rows; the screenshots show
  "1-100 of 13106 Logs"). Row height must be **measured, not assumed** — the
  window computes row N's offset as `N x height`, so a cell that renders a
  fraction of a pixel taller than its CSS is invisible at the top of the grid
  and most of a screen out by the bottom. Windowing also breaks find-in-page,
  which the grid has to admit on screen rather than let a reader conclude a
  value is absent.
- Server-side pagination with page size selector (default 100).
- Column show/hide + reorder, persisted per user (`Customize Columns`).
- Multi-select filter rail driving the query, with **live counts per filter
  option** — this is a big part of why UniFi feels responsive, and it means your
  filter counts must come from an aggregate query, not from counting the loaded page.
  Two things this requirement does not say, both learned the hard way
  (STATUS §5b): each facet must be counted with the *other* filters applied but
  **not its own**, or selecting an option zeroes every alternative and the rail
  becomes one-way; and the **selected** option must stay in the list even at a
  count of zero, or choosing it makes it vanish and leaves an empty grid with
  nothing to explain why.
- Row leading indicator: status dot + label.
- Semantic value coloring: link speeds, experience ratings, allow/block actions,
  channel-utilization percentages. Coloring is *additive* — the text still says
  the value. Do not colour-grade interference: it is capability-gated (§7).
- Click a row → detail slide-over, URL updates, deep-linkable.
- Sticky header, horizontal scroll with a persistent scrollbar. The grid must
  own its own scroll container to get this: `position: sticky` resolves against
  the nearest scrolling ancestor, and any wrapper with `overflow: hidden` — a
  card with rounded corners, for instance — silently becomes that ancestor and
  the header stops sticking without ever looking broken.

---

## 6. Screen specs (abbreviated)

**Dashboard.** Keep the existing fleet counts, device status and recent events,
then add WAN interface download/upload throughput, fixed-target ICMP
reachability/freshness/latency/loss, recent warning/critical events, a compact
topology summary and speed-test history. `site_wan_*` comes from the selected
gateway; interface counters state download/upload direction and freshness.
Label the availability strip `ICMP reachability to 1.1.1.1`, never ISP uptime.
Missing or stale evidence reads `Unavailable` or `Last observed`, never zero.
Cards link to the corresponding filtered screen; count methodology moves behind
`How these counts are calculated`.

The speed-test card defaults to **Controller test** and says `Runs from the
controller host/container; makes no router management call or change`. It also
states that test traffic follows the controller host's normal route and may
saturate the gateway/WAN. Before Start, show
vantage point, provider/endpoint, estimated data use, saturation impact, timeout
and privacy implications. Start sends the descriptor's opaque `plan_id` with a
fresh data-use acknowledgement; the server rejects a changed plan before
creating a job. While running, show progress and Cancel; results show
download/upload, idle/loaded latency, jitter, time, method and provenance with
honest unavailable fields. History defaults to grouped horizontal throughput
bars; latency stays off the Mbps axis, and the table view retains every exact
result and failure. A gateway-run test is a separately installed,
default-off official-feed capability and never appears to be part of ordinary
adoption or the controller Start button.

**Topology.** Filter rail. Canvas: tidy tree, internet at top only when a
gateway default-route observation supports it. Node = icon + label;
expand/collapse badge on nodes with hidden children. Link thickness optionally
encodes throughput. Zoom controls bottom-left; VLAN chip row bottom-center
filters/colorizes paths. Click node → device slide-over. `Now` and history
requests are generation-bound so a slow old response cannot replace the
selected instant. Show `complete`, `truncated`, source-gap and edge-ambiguity
copy beside the graph: current source state is stale after 31 minutes; history
is limited/retained to 31 days and 10,000 intervals; historical source coverage
is unavailable rather than inferred from today.

**Client Devices.** Filter rail (status, connection, groups, APs, WLANs, VLANs,
vendors). Grid with the column set in PARITY-MATRIX. Row → client slide-over
with per-client history, actions (block, reconnect, rename, fix IP, rate limit).

**Client Observability.** Four synchronized columns on desktop: faceted client
filters, client list, narrow 24-hour event spine, and the analysis pane. The
analysis pane stacks Client Health, AP Health and Site Health on the same time
axis, followed by connection logs. Hovering or selecting a time updates every
chart, the AP/path summary and the relevant event. On narrower screens the
filters and client list collapse into drawers, but the time spine and analysis
remain one coordinated surface. This is a Phase-4 screen and depends on honest
gaps: unavailable retries, loss or application identity are named, never drawn
as zero. The joined response is durable rollups only: complete 5-minute buckets
through 7 days, complete hourly buckets beyond, never raw samples or a partial
edge bucket. `wifi-v1` is 45% RSSI, 35% retry delta and 20% TX-failure delta;
any missing input makes that bucket null. AP attribution is null when multiple
BSS/device observations make it ambiguous. Site cards say exactly “ICMP … to
1.1.1.1”; they do not say Internet/HTTP/DNS uptime. A moving 24-hour window
periodically refreshes/rebinds, and a live subscription follows the currently
attributed AP rather than the client inventory row's last device.

**Devices → per-device → Ports.** Left: device selector, model/version, view
toggles (Port Diagram, VLANs, Stats), health score, filters (VLAN, status, PoE,
link speed). Content: chart toolbar (`Total | By Port`, `Packets | Usage | PoE |
Errors | Dropped`, `Bps | Bytes`, `All | Download | Upload`), chart, port table.
On legacy swconfig devices the same workspace is read-only and includes only
the link, VLAN, ARL and MIB fields the switch reports; DSA-only profile/edit
controls are absent. **Hide the PoE column entirely on hardware that can't
report it.**

**Insights → Radios.** Left: channel occupancy heatmap, AP/band filters, Channel
Plan legend, MIMO filter. Content: per-radio table with capability-gated
channel-utilization, interference/airtime and retry columns, color-graded only
when their required counter deltas are valid. Stable identity is the UCI `wifi-device` section,
not a PHY or BSS name. Show inventory/channel observation times and last-known
staleness. `Scan` opens a keyboard-trapped confirmation modal, warns that the
serving radio goes off-channel, and sends the request only after explicit
acknowledgment. A suggestion is shown only for a completed scan ≤24 hours old
with current radio state and a channel plan ≤15 minutes old. Persisted history
exposes only the newest terminal result per stable radio key; pending/running
work is preserved until it reaches a terminal outcome.

**Settings → Overview.** Collapsible summary cards per domain, each a table +
`Create New | Manage`. Search across settings at the top of the settings nav.

**Discovery and adoption.** An empty scan is a statement about what answered;
it is never also used for a scan the controller could not perform. If every
dial to a CIDR returns a host/network-unreachable route error, show a critical
banner naming the CIDR and say the scan does not establish whether devices are
present; direct the operator to the controller host's routes and interfaces.
Adoption keeps separate fields for the device password (required by ubus,
including when SSH uses a key) and an optional SSH private key used only for the
bootstrap. Neither credential is echoed or persisted. If the account accepts
any password, show both exact remedies—`ssh -t 'user@host' passwd` and LuCI
`System → Administration → Router Password`—and state that the controller will
not alter `/etc/shadow` or set the password itself.

Before Adopt, **Inspect capabilities** reuses the address and administrator
ubus credential for a read-only probe. Its card shows model/firmware/radios,
observed LAN/WAN ports, nullable active-WAN-default-route and LAN-DHCP evidence,
switch mode, recommendations and inspection limits. Say explicitly that it
creates no router account or configuration. A failed inspection may fall back
to direct adoption, whose scoped-login probe still runs after bootstrap.

The function picker uses independent checkboxes for **Gateway**, **Access
Point** and **Switch** and requires at least one. Mark a function `recommended`
only from positive authenticated evidence, `available` when hardware supports
it without proving it is in use, and `not observable` when the check did not
answer. A completed inspection may preselect its recommended set, but the
operator can change every checkbox and Adopt sends that reviewed set. In
particular, a WAN port on the Archer C6 makes Gateway available, not observed;
only an active default route on WAN or enabled LAN DHCP recommends it. Switch
copy promises wired responsibility and visibility, not selective per-port
configuration: `dsa-conditional` names the VLAN-aware-bridge precondition and
`observe-only` names legacy swconfig telemetry/FDB with no managed-VLAN writes.

When the fleet is empty, show contextual gateway-first guidance: if inspection
confirms Gateway, recommend adopting that routing anchor first; otherwise
explain how to identify the DHCP/router device. This is guidance, not a hard
gate—AP-only remains valid when the gateway is intentionally managed elsewhere.
The backend enforces the different safety invariant: at most one managed
Gateway, with a concurrent second request rejected before either bootstrap can
escape the inventory check.

An adopted device whose controller ACL predates a new read grant shows
**Install optional oonfeeWRT capability**. The slide-over takes an administrator
username plus password and/or private key for this transaction only, clears
them after success or cancellation, and requires a separate unchecked
acknowledgment before submit. It must call this an optional oonfeeWRT controller
capability installation and say plainly that accepting installs or replaces
exactly one rpcd ACL JSON file. It unlocks controller access to supported
topology, radio channel/scan, OpenWrt log and fixed-target WAN ICMP observations;
it installs no package, binary, daemon, service or firmware. Leaving the box
unchecked or cancelling sends no request and leaves the router unchanged;
dependent observations stay explicit gaps. It also says exactly what is
preserved: the controller login,
inventory, ownership ledger and all UCI configuration. Success means a fresh
controller login, inventory-MAC check and capability re-probe passed; “file
uploaded” is not enough. A refresh failure must not suggest re-adoption or claim
that router configuration changed.

Adopt and capability installation submit `acknowledge_router_changes:true` only
after their respective unchecked disclosure is selected. Adoption's disclosure
also says that acceptance creates the scoped controller login. Omitted/false
requests are rejected before SSH or any mutation. Inspect never sends it
because Inspect is read-only.

An unsupported LLDP topology source shows a separate compact notice. It never
claims a package is absent from a generic source error; it says LLDP evidence is
unavailable and links to the device's **Optional LLDP topology capability**.
That panel first requires acknowledgement that resolving the exact plan may
refresh only the package-index cache. It displays the package manager's full
bounded plan. A second unchecked acknowledgement authorizes installing the
official-feed `lldpd` package and dependencies in that plan, then enabling and
starting its service. A credentialed, explicitly acknowledged read-only plan
then identifies only non-wireless physical bridge members. Replacing only
`lldpd.config.interface`, committing only `/etc/config/lldpd`, and restarting
only `lldpd` requires another unchecked acknowledgement bound to that plan.
Read-only diagnosis reports the retained durable install/configuration state,
UCI export, runtime interfaces, and neighbors without changing the router.
Removal has its own reviewed plan and acknowledgement, drift-checks and restores
the exact UCI baseline, removes the recorded controller-added package set, keeps
pre-existing packages, restores and verifies prior service state, and must
complete before un-adoption. Administrator credentials are never stored. They
remain only in the open review for its plan/apply pair and clear when the review
closes, after a router change completes, or after a failed or rejected request.

**Settings → Networks.** The network slide-over edits name, VLAN, zone, enabled
state, IPv4 gateway/CIDR and DHCP `{enabled, start, limit, leasetime}`. Derived
gateway, netmask, broadcast, usable-host count and pool range update live. Save
is blocked for malformed or unusable gateway addresses, an invalid lease-time
unit, a pool outside the subnet or a pool containing the gateway. VLAN 0/1 show
an explicit management-LAN note instead of DHCP controls: their addressing and
DHCP remain operator-owned. Preview must also name the non-UI boundaries:
devices without Gateway do not run a second DHCP server, a foreign DHCP server
or firewall zone is not adopted implicitly, and the controller never enables
VLAN filtering on a flat management bridge.

**Policy Engine — current shipped subset.** `Zone Matrix` renders active
managed source zones as rows and destinations as columns, with Internet/WAN as
a destination-only/read-only source row. Same-zone cells are explicitly outside
the firewall matrix. A direct edge is `Allow All`; if only the reverse edge
exists, the cell is `Allow Return Traffic`; neither is `Block All`. Editing any
cell opens the source zone's complete destination checklist, because one save
replaces that source's whole directed policy. Saving changes desired state only
and points back to Preview/Apply. An explicit policy may be empty. Reset has a
confirmation step and explains that it removes the row and restores the legacy
Internet-only default.

Below the matrix, `Master Table` joins every effective zone edge with explicit
IPv4 firewall rules, port forwards, static routes and client block/fixed-IP/
group desired state. It exposes origin, effective scope, renderability and the
exact capability gate. “Order” is display-only in this release; overlapping
managed rules are rejected rather than claiming UCI names establish priority.
QoS/rate-limit and application identity remain unavailable. Invalid policy is
a critical banner that says Preview/Apply will refuse. A network zone change
into a target without an explicit row warns *before save* that the target uses
WAN-only legacy policy and directs the operator to Policy Engine then Preview.

**Policy Engine — partial Object Manager.** `Objects` selects observed client
devices, non-empty client groups or active managed networks. Compile is
read-only: `Secure` produces concrete, IPv4-only reject drafts with exact
source zone/MAC scope; `Route` produces static routes for network objects only.
Device/group policy routing, QoS and application outcomes return visible gates.
Drafts say **Not persisted · Not applied** and each chosen draft has a separate
save action before Preview/Apply. Object Manager remains a compiler into the
same visible policy records, never a hidden source of truth.

**Safe Apply & Recovery.** One settings surface groups the existing
test/confirm rollback contract, device-health monitoring, recovery thresholds
and link protections. Test & Confirm is always available where the apply engine
supports it. Auto-recovery is capability- and consent-gated, defaults to
monitor-only, shows its cooldown and last action, and can never repeatedly
reboot a failing router. DSA-only STP/link controls disappear on unsupported
hardware.

**Logs.** General/Audit toggle plus category and severity filters. Counts come
from the whole matching store, never the loaded page. Pages use the exact
`(ts,id)` keyset and `Load older`; a filter/scope change invalidates any slow
older request. Row → focus-trapped detail modal; Escape closes it and focus
returns to the exact trigger. General includes controller events and persisted
OpenWrt `log.read`/hostapd association events. Its coverage banner distinguishes
unobserved routers, observed-empty producers, >3-minute staleness and retained
continuity gaps; only complete observed-empty coverage may say “No general
events were observed.” Router wall-clock values never drive producer-cursor
continuity; the exact millisecond source time remains detail while public event
time is whole-second. Messages and structured secret fields are redacted before
persistence/display. Logs are REST, not WebSocket; SIEM export, GeoIP detail
and notification settings remain later parity targets.

**Alarm Manager.** A table of explicit trigger → scope → action rules. Network
triggers include device/offline, WAN latency/loss, client traffic, port errors,
security and admin changes. Scope may be site, device, client or network; action
may be in-app, email or webhook. Schedules, cooldowns and “ignore repeats” are
visible columns, because an alert that can spam is an operational defect.

**Account-screen source boundary.** Schema 19 has the account foundation:
canonical `owner`, `admin`, `operator` and `viewer` roles; existing admin to
enabled owner; ASCII-NOCASE uniqueness; soft-delete tombstones; an atomic
last-enabled-owner guard; and transactional account-mutation audit.
Account-management endpoints, role-bearing sessions, declarative route/live
middleware, My Account and owner account-management screens are implemented.
Logout, password change, role/enable/delete/reset, explicit revocation, REST expiry and
Sweep close affected `/live` sockets and cancel in-flight requests.

**Settings → My Account.** Every signed-in user can change their own password,
and list/revoke their own in-memory sessions. Sessions state plainly that
controller restart invalidates them.

**Settings → Accounts.** Owner-only management lists accounts, canonical role
(`owner`, `admin`, `operator` or `viewer`), enabled state and recent login without
exposing password material. Owner can create, change role, enable/disable,
soft-delete and revoke any account's sessions; the last enabled owner is
protected. Client address is shown only under the documented trusted-proxy
policy. The UI reflects server permissions but never serves as the authorization
boundary.

**Settings → Diagnostics.** Implemented for `owner`/`admin`. Before generation,
the screen shows fixed stored-only sections, explicit excluded secret classes,
bounds, redaction policy and controller-log availability/gaps. It then exposes
one cancellable job, bounded terminal history and completed ZIP download. The
descriptor states `mode: stored`, `router_management_calls: false` and
`router_changes: false`; there is no live-router refresh. The ZIP has fixed
members, a manifest and checksums. Stored evidence gaps do not fail the whole
bundle.

**Settings → Backup & Restore.** Implemented for `owner` in schema-19 source;
the live owner screen has passed route/render smoke, but its workflow actions
are not claimed by that smoke.
The existing export UI remains available and explains that its native
`.oowrtbak` contains a consistent live-WAL controller database/key pair under a
separate export passphrase. Backup and restore are available only over TLS or
direct loopback. Export start/download and every restore mutation require a
recent account-password reauthentication; the UI does not auto-retry them.

Restore uploads the raw native file, polls or cancels a bounded disposable
preview, and fails closed on malformed descriptor/job responses. The concise
result shows only authenticated manifest identity, source/target schema and
recovery counts; technical details live under disclosure. The preview export
passphrase is cleared and must be entered again for confirmation. Confirmation
also requires the current controller boot/keyring passphrase—explicitly not the
signed-in account password—exact `RESTORE CONTROLLER`, and four distinct
acknowledgements for restart, session revocation, router-write suppression and
no automatic router Apply. Passphrase controls disable password-manager account
autofill; all secret fields clear on error, refresh, success and unmount.

HTTP 202 changes the screen to reconnecting; it does not claim router management
has resumed. The post-restart safety state stays prominent: restored desired
state is not applied automatically, router writes remain persistently
suppressed, and read-only monitoring of restored devices may resume using
restored credentials. While suppression is active the UI blocks another confirm
and offers only a separately reauthenticated exact-text
`RESUME ROUTER WRITES` review. That default-open critical review states that
resume immediately re-enables automatic 802.11k neighbour maintenance and may
write hostapd RRM neighbour state; a separate unchecked acknowledgement and the
exact text are both required before the UI enables resume. It does not start a
restored desired-configuration Apply. The retained encrypted safety artifact
lives at
`<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak`. After its applied
audit receipt is cleared, retention targets three recognized artifacts and
fills slots newest-first. Every artifact referenced by active marker, receipt
or suppression state is preserved even when that exceeds three. Longer
retention requires an off-host copy before pruning.

**Un-adopt.** This operation is not rollback-armed, so its ownership preview
has three states: loading, known (including known-empty), and unreadable. Both
destructive actions remain disabled unless the ledger is known; unreadable is
never rendered as “nothing to revert.” The operator password and optional SSH
private key are one-transaction inputs and are cleared after success. A partial
cleanup report must survive errors and forced inventory removal, list every
residue item, and show exact validated stock-OpenWrt cleanup plus verification
commands while the report is still available.

The report separates `config_revert_complete`, remaining managed sections and
the login/ACL footprint. If the controller cannot prove that every owned config
was committed away, phase 2 must not remove its login/ACL and ordinary removal
must not delete the inventory row. Keep the device tracked and make retry the
primary action. Force is a separately acknowledged “hardware is gone” escape
hatch, never an automatic response to a failed config phase.

**Flows.** Filter rail: risk swatches, time range, flow type radio, `Flows on
Map` toggle with map thumbnail, deep filter accordions (source/destination ×
zone/network/MAC/IP/port/region). Content: four summary cards, then the flow
grid with country flags and allow/block actions.

---

## 7. Capability-driven rendering

Unlike UniFi and GL.iNet, oonfeeWRT doesn't know what hardware it's talking to
until it asks. Every screen renders from the device's capability record
(ARCHITECTURE §6.1), and the rule is absolute:

> **A feature the hardware cannot do is absent, not disabled.**

No greyed-out PoE column on a switch without PoE. No empty 6 GHz tab on a WiFi 5
AP. A legacy swconfig switch gets its measured read-only port state and counters
without DSA-only configuration controls; hardware with neither path gets no
port panel. Greyed-out controls read as "this app is broken"; absence reads as
"this device doesn't do that," which is the truth.

**There are three capability values, plus an observation state.** A field can
be missing, present and good, or **present and untrustworthy** — and the third is
the dangerous one, because probing for presence finds it and probing for
absence does not. Separately, `NotObservable` means the check did not establish
any of those values: refusal, transport failure, missing API or another failed
read is not proof of hardware absence. mwlwifi
returns `rx_time`/`tx_time` in every survey result; the values are uninitialised
garbage (~1.4e19). `iwinfo.survey` returns `noise` on every radio; it is
unsigned, so −95 dBm arrives as 161. Both would render as confident nonsense.

So capability gating keys on a **driver/model quirk list** (matched on
`system.board` plus driver), not on field presence. Any metric derived from a
field on that list is treated exactly as unsupported: never rendered, never
color-graded, never averaged into a composite score. Five entries measured on
mwlwifi so far:

| Field | Defect | Consequence |
|---|---|---|
| `rx_time` / `tx_time` (survey) | uninitialised (~1.4e19) | interference and the airtime split are not computable |
| `noise` from `iwinfo.survey` | reported **unsigned** (161 for −95) | `iwinfo.info` reports it signed — but that fixes the encoding only, see the next row |
| `noise` per station (`assoclist`) | **unstable** — swung 37 dB between reads 3 s apart | **never compute per-sample SNR from it** |
| `noise` per radio, from **both** `iwinfo.survey` and `iwinfo.info` | **unstable on the 2.4 GHz radio** — 42 dB (info) and 46 dB (survey) spread over 20 samples ~0.35 s apart, while the 5 GHz radio on the same driver held within 7 dB | the noise floor is a **per-radio** capability. Where it is unstable, show utilization or RSSI, never a noise figure or an SNR |
| `busy_time` / `active_time` (survey) | **counters with different epochs** — not a ratio. Absolutes read 25.9% on a radio truly at 73.3% | compute utilization from Δbusy/Δactive between two samples; never divide the absolute values |

That last one is the reason presence-probing is not enough: the field is there,
correctly typed, and plausible in any single sample. Only re-reading exposes it.
Where the noise floor is unstable, show RSSI alone, or compute SNR from
`signal_avg` against a smoothed noise floor — and never colour-grade a value
that will visibly flail on the next refresh. `tools/probe.py` samples the noise
floor four times and warns above a 6 dB spread; that check is now **ported into
the capability probe**, which re-reads both sources and records the result per
radio as `Radio.NoiseStable`.

Two corrections that came out of porting it, both measured 2026-08-13:

- **Switching source does not help.** The advice above — read `noise` from
  `iwinfo.info` because `iwinfo.survey` reports it unsigned — is right about the
  encoding and says nothing about trust. On the reference device the 2.4 GHz
  radio swung 42 dB through `iwinfo.info` and 46 dB through `iwinfo.survey`. The
  instability belongs to the radio, not to the method.
- **It is per radio, not per device.** The 5 GHz radio on the very same driver
  and the very same device was steady within 7 dB. Gating device-wide would
  discard a perfectly good reading to punish a bad one.

The detector is deliberately **asymmetric**, and anything consuming it must read
it that way: two samples disagreeing proves the value moves, but two samples
agreeing proves nothing. `NoiseStable == Present` means "not caught
misbehaving", never "verified stable". On one hardware run the survey pair
agreed while the `iwinfo.info` pair jumped 45 dB — the same radio, the same
minute.

Where absence would be confusing, replace rather than hide — a short inline note
in the space the feature would have occupied ("This access point has no 6 GHz
radio"), as muted secondary text, never styled as an error.

Unknown and absent messages must not be stacked into a contradiction. If the
renderer has already named a requested band as unknown because radio inventory
was not observable, it must not append a generic “no matching radio” sentence.
Device detail separates standing permission/firmware/driver limits from current
transport/protocol/decode poll failures; each row carries its cause and any ubus
status so remedies do not turn every unknown into “re-adopt.”

The Devices list should surface capability differences as small badges (PoE,
6 GHz, DSA, WiFi 7) so a mixed fleet's asymmetry is visible at a glance instead
of being discovered screen by screen.

---

## 8. The apply flow

The single most important interaction in the product, and the one with the most
hardware-imposed constraints. Every rule below comes from a measured device
behaviour recorded in IMPLEMENTATION §14 — none of it is stylistic.

**Pending changes** accumulate in a bar showing a count and `Review | Apply`.
Review shows the per-device diff (the "what will change on this device"
preview), grouped by device, with sections we own visually distinct from foreign
config we are leaving alone.

**Preview authorization is server-bound.** Apply is disabled without the opaque
`preview_token` returned by the current full-fleet Preview. Any desired-state
edit clears the preview and every acknowledgment; a fresh Preview must earn
them again. The server also rebuilds and verifies the site, fleet, ownership
ledger and plans, so a second tab cannot authorize an unseen policy change.
Before the first router write, every selected device must be plannable and
unblocked. Traversal changes, radio-death driver defects and behavior cautions
each get their own exact acknowledgment. The normal UI always selects the full
fleet; only an API client can request a subset, and it must explicitly
acknowledge partial-fleet inconsistency.

Apply runs serially, Gateway last, and stops after the first non-applied result;
the result names the stopping device and leaves later devices untouched. Once
admitted it continues under a bounded server deadline even if the browser
disconnects, because abandoning an armed rollback is less safe than finishing.
The browser generates a UUID before the request and retains it through reload;
Settings loads its durable parent/per-device status instead of retrying the
write. An HTTP error with `write_state:"none"` may be shown as a definite
refusal. Show **Apply result unknown** only when the durable receipt itself is
unknown/unreadable, direct the operator to its device boundaries/audit and
forbid blind retry. STATUS §5be proves reload recovery on a real failed/reverted
fleet run.

If a router confirmed but the controller could not record ownership, do not
collapse it into ordinary Applied. Show that the device applied while
controller ownership recording failed, and that later devices were not
touched.

**Warn per option, not per apply.** Applying is not inherently disruptive:
measured, an apply touching only inert options left both associated clients
connected for ~1896 s unbroken, while an SSID change restarted the BSS. So the
diff marks *individual rows* as client-disrupting (SSID, encryption, channel,
htmode, radio enable) and the Apply button summarises: "3 changes, 1 will
briefly disconnect WiFi clients on Living Room AP." A blanket warning on every
apply is both wrong and desensitising — users stop reading it, which is exactly
when it matters.

**During the window**, show a countdown tied to the device's rollback timer, and
say plainly what happens if it runs out: *"If we can't confirm within 90 s this
device reverts itself."* That is the honest description and it is reassuring
rather than alarming, because it is a safety net, not a failure.
The current operation UUID and durable parent/per-device status make that
progress recoverable after a reload; the UI resumes the same receipt and never
blindly retries the write.

**Three outcomes, not two.** The device's own timer means the failure space has
a third state, and the UI must not collapse it:

| Outcome | What happened | How it reads |
|---|---|---|
| **Applied** | health passed, confirm landed | green, normal |
| **Reverted** | we declined to confirm, or couldn't; device restored itself | neutral, *not* an error — the safety net worked. Show why health failed |
| **⚠️ Unknown** | confirm failed *and* the change is still present on re-read | the only alarming state. Offer "reverse this change" as an explicit action |

That third row is not hypothetical: an rpcd restart inside the confirmation
window destroys both the session that would confirm and the timer that would
revert, leaving the change applied and unconfirmed. Never render that as
"Applied".

**Do not offer "retry" mid-window.** Confirm is bound to the session that
applied, so a re-authentication inside the window guarantees the revert. If the
controller loses that session, the correct UI is "reverting…", not a retry
button that cannot work.

---

## 9. Accessibility floor

- Every status/severity encoded by color also carries text or an icon.
- Charts have a table view toggle.
- Filter rails are keyboard navigable; the grid supports arrow-key row movement.
- Focus rings visible on the dark surface (`--accent` at 2px, 2px offset).
- Respect `prefers-reduced-motion` — the topology graph's physics and the
  slide-over transition both need a static path.
- Dark is the primary theme, but light must be a real, separately validated
  theme, not `filter: invert()`.
