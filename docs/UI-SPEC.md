# oonfeeWRT — UI Specification

Modeled on UniFi OS 5.1.19 / Network 10.4.57 (July 2026), dark theme.

**Legal framing:** this spec describes *layout patterns and information
architecture*, which are not protectable in the way assets are. Do not copy
Ubiquiti's icons, illustrations, fonts, CSS, or wordmark. Build the equivalent
components from scratch. See `RISKS.md`.

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
│52│                      │   └────────────────────────────┘             │
│px│                      │                                              │
└──┴──────────────────────┴──────────────────────────────────────────────┘
```

**Icon rail (52px).** Two groups separated by a divider. Primary: Dashboard,
Topology, WiFi, Devices, Client Devices, Insights, Flows. Secondary (bottom):
Settings, Logs, Tools, Alerts, Admins. Icons only — tooltip on hover, active
state is a filled/accented glyph.

**Context rail (264px).** Screen-specific. Two personalities:
- *Filter rail* (Topology, Clients, Devices, Insights, Flows, Logs) — stacked
  collapsible filter groups, each option with a live count, sticky footer links
  (`Clear Filters`, `Customize Columns`, `Download`).
- *Card rail* (Dashboard) — stacked summary cards.

Collapse chevron `«` sits on the rail's outer edge, vertically near the top.

**Detail slide-over (370px).** Enters from the right over the content area,
never over the rail. Header = entity name + close. Sub-navigation as an icon
segmented control (overview / stats / settings). Body = stacked property groups
and mini-charts. Used for: device detail, client detail, log entry detail.

**Content header.** Left: sub-tab segmented control (`Topology | Infrastructure`,
`Main | IP Table`, `Flows | Activity`). Right: time range segmented control
(`1h 1D 1W 1M` + calendar icon) and series toggles.

---

## 2. Navigation map

```
Dashboard
Topology            → Topology · Infrastructure
WiFi
Devices             → Main · SFP  ▸ per-device: Overview · Stats · Settings · Ports
Client Devices      → Main · IP Table
Insights            → Radios · Coverage · RF Scan
Flows               → Flows · Activity
Logs                → General · Audit
Settings
 ├ Overview         (summary tables for every domain, with Create New / Manage)
 ├ WiFi
 ├ Networks
 ├ Internet
 ├ VPN
 ├ Policy Engine    → Firewall Rules · Zone Matrix · Traffic Rules · Traffic Routes · Port Forwarding
 ├ Security         (IDS/IPS, blocklists)
 ├ High Availability
 ├ System           (updates, backup, admins, timezone, SIEM export, notifications)
 └ Console          (control plane, identity, device credentials)
```

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
| Experience score | stat tile + **variable-arity** component breakdown on hover | never a bare number — show the inputs. Render whichever components the device actually supplies, renormalize the weights over the available terms, and name the omitted ones in the tooltip ("airtime unavailable on this radio"). Never show an empty slot, and never silently drop a term — a score computed over different inputs on different hardware is not comparable |

---

## 5. Table system

Every major screen is a filtered, virtualized data grid. Build one component.

Requirements:
- Virtualized rows (Flows and Logs run to 10k+ rows; the screenshots show
  "1-100 of 13106 Logs").
- Server-side pagination with page size selector (default 100).
- Column show/hide + reorder, persisted per user (`Customize Columns`).
- Multi-select filter rail driving the query, with **live counts per filter
  option** — this is a big part of why UniFi feels responsive, and it means your
  filter counts must come from an aggregate query, not from counting the loaded page.
- Row leading indicator: status dot + label.
- Semantic value coloring: link speeds, experience ratings, allow/block actions,
  channel-utilization percentages. Coloring is *additive* — the text still says
  the value. Do not colour-grade interference: it is capability-gated (§7).
- Click a row → detail slide-over, URL updates, deep-linkable.
- Sticky header, horizontal scroll with a persistent scrollbar.

---

## 6. Screen specs (abbreviated)

**Dashboard.** Card rail: console summary, ISP/WAN, WiFi defaults, traffic
prioritization, security. Content: sub-tabs `Internet | WiFi | Flows`, WAN
selector, main chart, uptime strip, Top APs/Clients/Apps icon strips, Most Common
Devices strip, two donut+table cards. All cards user-reorderable
(`Dashboard Widgets`).

**Topology.** Filter rail. Canvas: tidy tree, internet at top. Node = icon +
label; expand/collapse badge on nodes with hidden children. Link thickness
optionally encodes throughput. Zoom controls bottom-left; VLAN chip row
bottom-center filters/colorizes paths. Click node → device slide-over.

**Client Devices.** Filter rail (status, connection, groups, APs, WLANs, VLANs,
vendors). Grid with the column set in PARITY-MATRIX. Row → client slide-over
with per-client history, actions (block, reconnect, rename, fix IP, rate limit).

**Devices → per-device → Ports.** Left: device selector, model/version, view
toggles (Port Diagram, VLANs, Stats), health score, filters (VLAN, status, PoE,
link speed). Content: chart toolbar (`Total | By Port`, `Packets | Usage | PoE |
Errors | Dropped`, `Bps | Bytes`, `All | Download | Upload`), chart, port table.
**Hide the PoE column entirely on hardware that can't report it.**

**Insights → Radios.** Left: channel occupancy heatmap, AP/band filters, Channel
Plan legend, MIMO filter. Content: per-radio table with channel-utilization
(always present) plus capability-gated interference/airtime/
retry columns, color-graded.

**Settings → Overview.** Collapsible summary cards per domain, each a table +
`Create New | Manage`. Search across settings at the top of the settings nav.

**Logs.** Filter rail: time range, severity swatches, General/Audit toggle,
category checkboxes with counts, event-type checkboxes with counts, device
filter, `Push Notification Settings`, `Export to SIEM Server`. Content: paginated
event table. Row → detail slide-over with full enrichment (source/destination
identity, zone, policy link, geo).

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
AP. No port-statistics panel on hardware with no DSA driver. Greyed-out controls
read as "this app is broken"; absence reads as "this device doesn't do that,"
which is the truth.

**There are three capability states, not two.** A field can be missing, present
and good, or **present and untrustworthy** — and the third is the dangerous one,
because probing for presence finds it and probing for absence does not. mwlwifi
returns `rx_time`/`tx_time` in every survey result; the values are uninitialised
garbage (~1.4e19). `iwinfo.survey` returns `noise` on every radio; it is
unsigned, so −95 dBm arrives as 161. Both would render as confident nonsense.

So capability gating keys on a **driver/model quirk list** (matched on
`system.board` plus driver), not on field presence. Any metric derived from a
field on that list is treated exactly as unsupported: never rendered, never
color-graded, never averaged into a composite score. The two known entries
today are mwlwifi's `rx_time`/`tx_time`, and `noise` sourced from
`iwinfo.survey` rather than `iwinfo.info`.

Where absence would be confusing, replace rather than hide — a short inline note
in the space the feature would have occupied ("This access point has no 6 GHz
radio"), as muted secondary text, never styled as an error.

The Devices list should surface capability differences as small badges (PoE,
6 GHz, DSA, WiFi 7) so a mixed fleet's asymmetry is visible at a glance instead
of being discovered screen by screen.

---

## 8. Accessibility floor

- Every status/severity encoded by color also carries text or an icon.
- Charts have a table view toggle.
- Filter rails are keyboard navigable; the grid supports arrow-key row movement.
- Focus rings visible on the dark surface (`--accent` at 2px, 2px offset).
- Respect `prefers-reduced-motion` — the topology graph's physics and the
  slide-over transition both need a static path.
- Dark is the primary theme, but light must be a real, separately validated
  theme, not `filter: invert()`.
