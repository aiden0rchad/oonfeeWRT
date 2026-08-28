# Clients and topology

The Clients and Topology workspaces connect endpoint presence with the
infrastructure path used to observe it. Both preserve evidence confidence and
coverage gaps so an inferred link never looks like a measured cable.

<div class="write-impact"><strong>Router write impact</strong><span>Viewing, filtering, and investigating clients or topology is read-only. Optional LLDP is a separate reviewed package/configuration workflow.</span></div>

## Client Devices

The Client Devices table is scoped to managed LANs and excludes adopted
infrastructure from the client count without deleting it from inventory.

### Use the filters

Filters operate on the complete matching result before pagination:

- network scope (**This network**, **Upstream**, **Unknown**, or all);
- presence (**Online**, **Offline**, or all);
- connection evidence (**Wireless**, **Unknown**, or all).

The current table does not claim that an endpoint is wired merely because no
managed AP reports it, and v0.1.1 has no client text-search or source-coverage
filter.

The count above the table is the filtered total, not merely the number of rows
on the current page.

### Read presence carefully

Presence is based on fresh fleet evidence, not on the fact that a MAC once
appeared in inventory. A historical neighbour or stale station entry must not
keep a client “online” after its live evidence expires.

Possible explanations for a missing or partial row include:

- the responsible device is offline or has not completed a poll;
- the client is outside the managed network scope;
- station or neighbour evidence is stale;
- AP attribution is incomplete;
- the client uses a randomized MAC and has a second inventory identity;
- the source is unavailable on this hardware/driver.

## Open Client Observability

Select a client row to open the joined investigation workspace. It keeps a
shared time cursor across the evidence it can correlate, including:

- current identity and presence;
- network and AP/path attribution;
- client, AP, and site health context;
- an event spine around the selected time;
- wireless/traffic metrics and accessible tables when present;
- explicit source and freshness gaps.

Use the same time cursor when comparing a client symptom with AP load, radio
quality, WAN health, and events. Correlation by time is more reliable than
comparing each screen's latest value after the incident has passed.

### A client investigation sequence

1. Confirm the client identity, including MAC randomization possibility.
2. Check whether presence is current and which source asserted it.
3. Identify the managed network and AP/device attribution.
4. Move the shared cursor to the reported incident time.
5. Correlate signal/traffic data with AP/device and site health.
6. Read events immediately before and after the change.
7. Open Topology at the same time when the infrastructure path may have moved.
8. Record unavailable evidence as a limitation in the conclusion.

## Current topology

Open **Topology** to view nodes and active link intervals. Sources can include:

- client wireless associations;
- bridge forwarding-database observations;
- neighbour data;
- default-route/uplink evidence;
- optional LLDP.

Each edge includes a confidence and medium. Confidence describes the evidence,
not the importance of the device.

### Filter and inspect

Use the controls to filter by:

- confidence;
- medium;
- VLAN;
- current versus historical mode;
- selected historical time/range.

Zoom changes the visual workspace only. The accessible topology details and
complete interval table preserve the underlying information for keyboard and
screen-reader use.

Select a node to open its identity or device workspace. For exact edge source,
time range, confidence, port, and ambiguity evidence, use the **Accessible
topology details** table and expand its **Evidence** cell. When duplicate names
exist, use the stable device/node identity rather than the label alone.

## Historical topology

Historical mode answers **what links did the controller have evidence for at a
given time?** It does not reconstruct packets or invent missing intervals.

Choose a preset or custom range. The view uses interval semantics: a link is
present when its evidence interval overlaps the selected time. A last-known
placement may be shown separately from a currently supported link.

Topology history is retained for 31 days. A request near or beyond that bound
can be marked retention-truncated. Export or record incident evidence before
the window expires when it must be kept longer.

## Evidence coverage and LLDP

The coverage indicator accounts for every adopted device relevant to the
current request. One device with stale/unreadable topology sources makes the
fleet view incomplete; oonfeeWRT does not hide that by drawing only the easier
links.

LLDP can strengthen direct infrastructure adjacency evidence. It remains
optional because enabling it may install the official OpenWrt `lldpd` package
and configure physical interfaces. Review the exact package and interface
plans on the device page, and retain the rollback ledger.

LLDP does not replace client association, FDB, neighbour, or route evidence.
Each source answers a different question.

## Troubleshooting clients

| Symptom | Likely explanation | Action |
|---|---|---|
| Client missing from current list | Presence expired, device poll failed, client out of scope, or randomized identity | Clear filters, check source coverage/device state, and compare MAC/hostname evidence |
| Wireless client attributed to wrong AP | Stale station data or simultaneous/ambiguous evidence | Check timestamp and association history; wait for fresh evidence rather than editing inventory |
| Wired neighbour appears on uplink | It is on the subnet carrying the default route, not a managed LAN client | Review network scoping and topology source; do not count it as a client merely because ARP saw it |
| Metrics are blank | Focused source has not flushed or hardware does not expose it | Leave the client/device view focused through the next collection/rollup and read the source note |

## Troubleshooting topology

| Symptom | Likely explanation | Action |
|---|---|---|
| No edge between known devices | No shared fresh source proves adjacency | Inspect coverage gaps; add LLDP only if its footprint is justified |
| Edge confidence is lower than expected | Inference comes from FDB/neighbour/association rather than direct LLDP | Expand the edge's Evidence cell in the accessible table and base the conclusion on the named source |
| Historical device is unplaced | Device existed but no edge interval supports placement at that time | Use last-known placement as context, not as a historical fact |
| History ends early | 31-day retention bound or missing collection interval | Read the truncation/gap notice and preserve future incidents earlier |
| Duplicate node names | Devices share display names or defaults | Rename controller display identities and use stable IDs during review |

## Related guides

- [Discovery, adoption, and devices](./devices.md)
- [Radios and channel planning](./radios.md)
- [Telemetry and retention](../concepts/data-retention.md)
- [Logs and diagnostics](./logs-diagnostics.md)
