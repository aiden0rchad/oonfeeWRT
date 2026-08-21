# oonfeeWRT — Feature Parity Matrix

Originally derived from a live UniFi install running **UniFi OS 5.1.19 /
Network 10.4.57** (July 2026), screen by screen. Re-baselined on **2026-08-18**
against the current stable **UniFi Network 10.5.67** release and its official
screenshots. The current stable UniFi OS train is 5.1.26 for Dream Machines;
5.1.27 was its release candidate at the time of the review (other console
families have platform-specific version numbers).

References: [UniFi Network 10.5.67 release notes](https://community.ui.com/releases/UniFi-Network-Application-10-5-67/375288b9-a4b4-46f1-a19d-5c787d342c2b),
[Traffic & Policy Management](https://help.ui.com/hc/en-us/articles/5546542486551-Traffic-Policy-Management-in-UniFi),
and [Zone-Based Firewalls](https://help.ui.com/hc/en-us/articles/115003173168-Zone-Based-Firewalls-in-UniFi).

**Verdict key**

| | Meaning |
|---|---|
| 🟢 | Direct OpenWrt source exists. Build it. |
| 🟡 | Achievable, but needs an official-feed package, a derived metric of our own design, or meaningful engineering. |
| 🟠 | Hardware-dependent — works on some OpenWrt devices, not others. Gate on the capability registry. |
| 🔴 | Not achievable. Proprietary silicon, cloud service, or paid data. Substitute or drop. |

**Dependency tiers** — every 🟢/🟡 item must land in tier 0, 1, or 2. Anything
that would need tier 3 is cut, per the no-device-code rule (ARCHITECTURE §0).

| Tier | Means | Example |
|---|---|---|
| **0** | Stock OpenWrt, no additions | `uci`, `iwinfo`, `iw`, `system.info`, hostapd ubus |
| **1** | An rpcd module from the official feed | `rpcd-mod-luci`, `rpcd-mod-iwinfo` |
| **2** | A daemon from the official feed, user-consented | `nlbwmon`, `lldpd`, `usteer`, `sqm-scripts`, `vnstat` |
| **3** | ~~Code we wrote running on the device~~ | **Ruled out. Cut the feature instead.** |

Controller adoption, polling, validation and ACL refresh install no tier-1 or
tier-2 package, binary, daemon or service. A future package flow, where a
feature requires one, remains disabled-by-default and explicit per feature.
No before/after package-inventory hashes were captured around the current live
ACL refreshes, so the live checkpoint makes no package-inventory-unchanged
claim.

---

## Network 10.5 current baseline — Observability, Safe Ops and policy

Network 10.5 made Client Observability and Safe Ops first-class. The current
application also carries the Policy Engine/Object Manager/Zone Matrix patterns
introduced on earlier trains. Together they are the active parity target;
copying isolated cards from older Dashboard screenshots is no longer enough.

**oonfeeWRT live checkpoint (2026-08-20):** the lab database is schema 16. The
initial signed-in pass under the routers' older ACLs was superseded when the
operator explicitly acknowledged ACL refresh for both routers at 15:16 and
15:17. Subsequent polls recorded OpenWrt-log and topology observations from
both routers and fixed-`1.1.1.1` ICMP observations from the Gateway. Historical
source coverage remains unavailable because snapshots are not stored. Phase
3's literal client-isolation/no-LAN proof remains partial.

| Current UniFi element | OpenWrt / controller source | Verdict |
|---|---|---|
| **Client Observability:** one 24-hour timeline correlating association, roam, signal, retries, latency/loss, AP/site health and events | Schema-16 exact events + topology validity intervals + durable 5m/1h rollups; application identity remains DPI-gated | 🟢 **source-built and live-observed**: the joined client/AP/radio/path workspace rendered from schema 16; the Gateway now supplies fixed-`1.1.1.1` ICMP observations. Historical router/topology source coverage remains explicitly unavailable rather than inferred |
| Timeline event cursor that updates every chart and the connection/AP path at the same instant | One bounded joined REST response at a shared millisecond cursor | 🟢 **source-built, live-rendered** through the joined workspace; exact events cap 2,000 and paths cap 64/2,048 work |
| People/device-group/status/connection/AP facets beside the client list | Existing faceted `DataGrid` queries; People needs local identity labels, not UniFi Identity | 🟢 for local groups, 🟡 for directory-backed identity |
| **Test & Confirm** with automatic rollback after lost connectivity | Keyed full-fleet Preview → durable operation receipt → preflight/acknowledgment gates → `uci.apply {rollback:true}` → runtime health → confirm, with polling quiesced | 🟢 **built and live-proven**. Runs detached from HTTP cancellation, aborts later devices on first failure, and recovers parent/per-device status after a reload without retrying the write (STATUS §5be) |
| Device Supervisor / global auto-recovery thresholds | Collector health state + explicit, bounded recovery actions | 🟡. Start monitor-only; never reboot or rewrite a third-party router without operator opt-in and cooldowns |
| Auto STP Edge and Link Debounce | DSA bridge/port attributes where the driver exposes them | 🟠 per-port and driver-gated |
| Data Plane Protection | Queueing/QoS and resource controls vary by target | 🟠. Report capability and measured protection; do not reuse UniFi's claim where OpenWrt cannot prove it |
| Infrastructure Topology timeline: wired downlinks, uplink changes, third-party devices and grouped cascades | Source-state-aware FDB/neighbor/association/default-route inference + half-open intervals | 🟢 **source-built and live-observed**: both refreshed routers now persist current topology-source observations. Ambiguity and missing evidence remain explicit; historical source coverage is unavailable rather than borrowed from current state |
| Unified **Policy Engine** master table across firewall, application filtering, policy routing, QoS, ACL, NAT/port-forward and DNS policy | Schema-15 site-model records rendered through capability-specific gateway/switch backends | 🟢 source-built for zone forwarding, IPv4 firewall, NAT/port-forward, static route and client desired state; 🟠 switch ACL/QoS; application matching DPI-gated. Live proof remains the whole-zone subset |
| **Object Manager** outcome workflow: choose devices, groups or networks, then `Secure`, `Route` and/or `QoS`; generated rules remain inspectable | Read-only compiler → concrete shared policy drafts + explicit gates | 🟡 **partially source-built, compiler live-proven**: the signed-in UI compiled one inspectable static network-route draft. It remained unsaved/unapplied, so no database or router mutation occurred. Device/group routing, QoS and application outcomes remain gated |
| **Zone Matrix:** source-zone rows, destination-zone columns, distinct state in both directions and cells that open the governing policies | schema-12 `forward_to` intent → owned firewall4 zones/directed forwardings, with foreign UCI conflict checks | 🟢 **implemented for whole-zone forwarding**: `Allow All`, `Block All`, `Allow Return Traffic`, explicit-vs-legacy origin and source policy editing/reset. Ordered per-pair rules/`Policies` remain part of the broader engine |
| Firewall policy Duplicate, Hit and Last Hit | Clone the site-model rule; nftables counters and last-observed timestamps | 🟢 |
| Network Lists with name, domain and description | nftables sets + dnsmasq domain sets in the site model | 🟢 for IP/MAC/domain objects; application objects remain DPI-gated |
| Radios column customization | Shared persisted `DataGrid` column preferences | 🟢; already available in the component system |
| Private-MAC warning on clients | Locally administered MAC-bit detection plus an explanatory client badge | 🟢; warning only, never identity certainty |

The release calls its safety group “Safe Ops”. oonfeeWRT may use the interaction
pattern, but must use its own naming, icons and assets. The useful parity is the
contract—preview, test, confirm, recover—not the trademark.

---

## Adoption and mixed-hardware functions

UniFi can infer responsibilities from a fixed product catalog. OpenWrt cannot,
so oonfeeWRT makes the equivalent intent explicit and backs the picker with a
read-only authenticated inspection.

| Controller element | OpenWrt/controller source | Verdict |
|---|---|---|
| Inspect before Adopt: model, firmware, radios, LAN/WAN ports and current gateway evidence | administrator ubus login + capability probe + runtime interface/DHCP reads; no SSH/bootstrap/store write | 🟢 implemented. Unknown remains distinct from a measured negative |
| Independently select Gateway, AP and Switch on one router | schema-11 `functions_json`; legacy `role` retained only as the primary compatibility label | 🟢 implemented. Gateway-only does not broadcast; AP-only does not gain DHCP/routing |
| Gateway recommendation and single managed gateway | active default route on active WAN and/or enabled LAN DHCP; adoption serialized before device contact | 🟢 implemented. A LAN management default route is not evidence; AP-only may be adopted first when routing is external |
| Gateway availability badge | WAN-capable hardware means available; only strong runtime evidence means observed/recommended | 🟢 corrected live on the C6—the badge no longer claims gateway operation merely because a WAN port exists |
| Switch responsibility | DSA port map or legacy swconfig stats/FDB | 🟠 `dsa-conditional` when the existing bridge is VLAN-aware; `observe-only` on measured C6 swconfig. Selection promises participation/visibility, not universal VLAN writes |

---

## Dashboard

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Console card: device counts, gateway IP, uptime | `system.info`, `network.interface` status, controller inventory | 🟢 |
| "Network / UniFi OS / Devices — Up to date" | our own version + `owut` check | 🟢 |
| ISP card: name, IPv4, uptime %, throughput sparkline | WAN interface counters + PPPoE/DHCP info; ISP name from ASN lookup of the WAN IP | 🟢 |
| Monthly data usage (4.44 TB) | `vnstat` on the WAN interface | 🟢 |
| Latency/loss indicator | adopted Gateway runs exactly three ICMP packets to fixed `1.1.1.1` at most once/minute | 🟢 **source-built and live-observed after explicit ACL refresh**. It is not HTTP/DNS validation, ISP uptime or a configurable multi-target pill set |
| Main chart: download/upload/latency/packet loss over 1h–1M | our TSDB + probe series, dual-axis | 🟢 |
| WAN uptime strip under the chart | fixed ICMP reachability series | 🟡 labels must say ICMP reachability to `1.1.1.1`; do not promote it to ISP uptime |
| ISP Speed Test button | `iperf3` to a public server, or `librespeed-cli` / `speedtest-go` on the gateway via `file.exec` | 🟡 accuracy varies; be honest in the UI about method |
| WiFi Doctor | 🔴 branded diagnostic. Substitute: our own "WiFi Health Check" running the same checks we already have data for (weak RSSI clients, high retries, channel overlap, DFS events) |
| Top APs / Top Clients / Top Apps strips | TSDB rankings; Top Apps needs DPI | 🟢 / 🟢 / 🟡 |
| "Most Common Devices" (device-type icons + counts) | MAC OUI + DHCP fingerprint (vendor class, hostname patterns) → device-type classifier | 🟡 needs a fingerprint database; `fingerbank`-style data, or ship a curated OUI+DHCP ruleset |
| Total Traffic donut by application | DPI (`netifyd`/nDPI) | 🟡 |
| Total Connections donut by WiFi generation/band + Experience | assoc data: HT/VHT/HE/EHT capability from `iw station dump` | 🟢 |
| Default WiFi Speeds (channel-width matrix, "Conservative") | our own preset that renders channel widths per band | 🟢 |
| Critical Traffic Prioritization | nftables DSCP marking + CAKE/`sqm-scripts` tin assignment, app-matched via DPI or port/IP heuristics | 🟡 |
| CyberSecure Enhanced (Proofpoint/Cloudflare) | 🔴 paid threat feeds. Substitute: Suricata + ET Open rules, firehol/Spamhaus blocklists, and say plainly that it isn't the same |
| Dashboard Widgets (user-arranged) | our own layout persistence | 🟢 |

---

## Topology

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Tree graph internet → gateway → switches → APs → clients | bridge FDB (`brctl showmacs` stock fallback, `bridge -j fdb` where installed) + ARP + wireless assoc; optional `lldpd` enrichment | 🟢 Baseline topology needs no added package. LLDP resolves managed adjacency; without it, multiple MACs on one port remain explicitly ambiguous |
| Expand/collapse node badges, zoom/pan, navigation mode | UI-side (d3/Cytoscape) | 🟢 |
| Filter rail: device status, client type, VLAN, WiFi broadcast, vendor | our indices | 🟢 |
| "Show Internet Traffic" overlay on links | live throughput per link from interface counters | 🟢 |
| VLAN chip row at the bottom (colorized paths) | network model | 🟢 |
| Infrastructure sub-tab (physical rack/port view) | requires port-level topology | 🟠 |
| Right slide-over: radio rows (Ch/width/MIMO/clients), AirView, active clients, TX retries timeline, memory, uptime, WiFi Exp % | `iwinfo`, `iw station dump`, `iw survey dump`, `system.info` | 🟢 for all but AirView |
| **AirView** (continuous spectrum analyzer waterfall) | 🔴 requires dedicated spectrum-scan silicon on Ubiquiti radios. Substitute: continuous survey deltas plus an **explicit** disruption-acknowledged `iwinfo.scan` snapshot. Never schedule serving-radio scans; useful, visibly not the same thing |
| Device Version + one-click **Revert** to prior firmware | dual-image/failsafe support is device-specific on OpenWrt | 🟠 |

---

## Client Devices

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Table: Name, Vendor, Connected To, Network, WiFi, Experience, Technology, Channel, IP, Activity, Down, Up, 24h Usage | `luci-rpc.getHostHints`, native `iwinfo.assoclist` + hostapd enrichment, DHCP leases; 24h usage still needs optional `nlbwmon` | 🟢 for current association/RF, 🟡 for durable usage |
| Online/Offline status dot + history | our poller + presence tracking | 🟢 |
| Vendor column | MAC OUI database | 🟢 |
| Experience column ("Excellent") | fixed `wifi-v1`: 45% RSSI + 35% retry delta + 20% TX-failure delta | 🟢 source-built; the live schema-16 view preserved unavailable components rather than fabricating a score. All three inputs are required in one sample; weights never renormalize |
| Technology ("WiFi 4, 1x1") | HT/VHT/HE/EHT + NSS from `iw station dump` | 🟢 |
| Filter rail: status, connection type, groups, APs, WiFi broadcasts, VLANs, vendors | our indices | 🟢 |
| WiFi Usage Diagram toggle | derived viz | 🟢 |
| Client groups, Create New, Add Client (manual entry) | our model | 🟢 |
| Customize Columns | UI-side, persisted per user | 🟢 |
| IP Table sub-tab | lease table + ARP + static reservations | 🟢 |
| Per-client actions: block, reconnect, rate limit, fixed IP | nftables set membership; `hostapd` deauth via ubus; SQM per-IP; dnsmasq static lease | 🟢 |

---

## Devices → Ports (gateway/switch detail)

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Port table: Port, Name, STP, Connection, Speed, Connected MAC/IP, Profile, Native VLAN | DSA: native `network.device`/`luci-rpc`; legacy: stock `swconfig dev … show`; FDB via `brctl`/`bridge` | 🟠 read/config on DSA only when the existing bridge is safely VLAN-aware; read-only status, VLAN membership and counters on measured swconfig hardware; absent only when neither path exists |
| Per-port throughput chart, Total/By Port, Packets/Usage/Errors/Dropped | native DSA counters; legacy swconfig MIB counters; `ethtool -S` only as enrichment | 🟠 capability-gated per counter, not per switch generation |
| **PoE Mode column + PoE control** | requires a PoE controller the driver exposes | 🟠→🔴 in practice. Very few OpenWrt-supported PoE switches expose control. **Design the UI to hide the column when unsupported, not to show it greyed out.** |
| Port Diagram / VLAN visual toggles | UI-side over the port model | 🟢 |
| Port Profiles (reusable VLAN/PoE templates) | our model → `bridge vlan` config | 🟢 |
| **24h AI Anomaly Score** + per-port Anomaly column | 🔴 as branded. Substitute: statistical outlier detection on our own port series (error rate, flap count, throughput z-score). Call it "Port Health", not AI |
| Time Machine toggle | historical replay of port state from TSDB | 🟡 |

---

## Insights → Radios

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Table: Band, Channel, Ch. Width, TX Power, Clients, Avg. Signal, 24h data, **Channel Utilization**, Avg. TX Retries, Uplink, Model | `hostapd.<iface>` (status + clients), `iwinfo` (survey/info), TSDB | 🟢 **Mostly achievable and still one of the strongest arguments for the project** — but Avg. Interference and Avg. Airtime are 🟠 gated per driver (see the two rows below), so the table is not uniformly green |
| Channel utilization % | **Δ`busy_time` / Δ`active_time`** between two `iwinfo.survey` samples | 🟢 The portable airtime metric — both fields verified good on mwlwifi, but they are **counters with different epochs**. Dividing the absolutes read 25.9% on a radio truly at 73.3% (measured 2026-08-13, confirmed against hostapd BSS load). Computed in `internal/telemetry`; `collector.Survey` deliberately offers no percentage method |
| Noise floor / SNR | `noise` from `iwinfo.info` (signed) or `iwinfo.survey` (unsigned) | 🟠 **Capability-gated per radio.** Measured 2026-08-13: the reference device's 2.4 GHz radio swung 42–46 dB between consecutive reads on *both* sources; its 5 GHz radio held within 7 dB. Gated by `Radio.NoiseStable`, which reports "not caught misbehaving", not "verified stable" |
| Avg. Interference % | `(busy_time − rx_time − tx_time) / active_time` | 🟠 **Capability-gated.** Needs `rx_time`/`tx_time`, which mwlwifi returns uninitialised (a garbage u64). Not computable on the class-A reference device |
| Avg. Airtime % | `(rx_time + tx_time) / active_time` | 🟠 Same dependency, same gate. Where rx/tx are unusable, show channel utilization instead — never fabricate the split |
| Avg. TX Retries % | `tx.retries / tx.packets` from **`iwinfo.assoclist`** | 🟢 Confirmed against real associated stations — the counters are nested inside `tx`, and no `iw station dump` spawn is needed |
| Channel Plan visualization | `iwinfo.freqlist` with pointer-valued restricted/active facts and stable UCI radio identity | 🟢 source-built for In Use / Enabled / Restricted / Unknown. DFS and exclusion are not inferred from `restricted` and remain unavailable without their own evidence |
| **Channel AI View** (auto channel selection heatmap) | 🔴 as branded. Substitute: our own channel scoring from survey + scan data → "Suggested Channels". The underlying math (least-congested selection weighted by neighbor RSSI) is not hard; the branding is theirs |
| RF scan / spectrum sub-tabs | native `iwinfo.scan`, explicit `acknowledge_disruption:true` only | 🟢 source-built; ACL refresh does not itself run a scan, and no disruption-acknowledged live scan was attempted. The 45s/4,096-row bounds and newest-terminal-per-radio retention remain source-tested only |

Radio freshness is part of the verdict: inventory/frequency rediscovery is on a
15-minute cadence, last-known state is marked stale after a failed poll, and a
suggested channel requires a completed scan ≤24 hours old plus non-stale radio
state and a channel list observed ≤15 minutes ago. Decoders bound one response
to 32 radios, 128 interfaces/radio and 512 frequencies.

---

## Settings → Overview

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| WiFi table: Name, Network, Broadcasting APs, Radio Band chips, Clients, Security | our WLAN model | 🟢 |
| Networks table: Name, VLAN ID, Router, Subnet, IPv6 Subnet, DHCP, IP Leases (16/180), Available | our network model + odhcpd/dnsmasq lease counts | 🟢 |
| Internet table: Interface, ISP, IPv4/IPv6, Port, Uptime, Peak Util, Latency | WAN state + probes | 🟢 |
| VPN Server table: WireGuard, subnet, server address, port, active clients | OpenWrt WireGuard + `wg show` | 🟢 |
| **One-Click VPN** (auto cloud-brokered) | 🔴 the "one-click" is cloud brokering. WireGuard itself is 🟢 — you supply the endpoint |
| High Availability | VRRP (`keepalived`) between two gateways | 🟡 real work, genuinely possible |
| Policy Engine: Objects, Master Table and Zone Matrix | schema-15 object/zone/policy model → firewall4, nftables, routing and optional switch ACL/QoS backends | Matrix + cross-feature Master Table are source-built. Object Manager is partial (`Secure` IPv4 drafts + static network routes); QoS/application and device/group routing remain gated. Only the whole-zone subset is live-proven |
| Control Plane / Identity (UniFi Identity SSO) | 🔴 substitute: local users + optional OIDC/LDAP |

---

## Settings → WiFi (per-SSID)

| Setting | OpenWrt mapping | Verdict |
|---|---|---|
| SSID, password, WPA2/WPA3/WPA3-only/Enhanced Open | `wifi-iface` encryption modes | 🟢 |
| PMF (optional/required) | hostapd `ieee80211w` | 🟢 |
| Network/VLAN assignment | `wifi-iface.network` | 🟢 |
| Band selection (2.4/5/6 GHz chips) | one `wifi-iface` per radio, rendered from one WLAN object | 🟢 **this fan-out is the product** |
| Broadcasting AP groups | our APGroup model | 🟢 |
| Hide SSID | `hidden` | 🟢 |
| Band steering | `usteer` / `dawn` config | 🟢 |
| Fast roaming (802.11r) | hostapd FT: `ieee80211r`, `mobility_domain`, `ft_over_ds`, `r0kh`/`r1kh` | 🟢 and controller-guaranteed consistency is the whole value |
| BSS transition (802.11v), neighbor reports (802.11k) | hostapd `bss_transition`, `rrm_neighbor_report`, and `rrm_nr_set` to fill the list | 🟢 **built 2026-08-16** — the config flags alone leave every AP advertising the feature and answering with nothing, because no AP can discover its neighbours. IMPLEMENTATION §15 |
| Minimum RSSI / client kick threshold | `usteer`/`dawn` thresholds | 🟢 |
| Client isolation | hostapd `isolate` + bridge-port `bridge_isolate` | 🟢 source-built for same-BSS intent and same-AP cross-BSS bridge state; literal two-client live proof remains partial |
| Multicast enhancement / IGMP snooping | bridge `multicast_snooping` | 🟢 |
| MAC filter allow/deny | hostapd macfilter | 🟢 |
| Schedules (SSID on/off by time) | cron → `wifi up/down` on that iface, or a scheduled reconcile | 🟢 |
| Rate limiting per SSID | SQM/tc on the wireless iface | 🟢 |
| Guest portal / captive portal | `uspot` or `opennds` | 🟡 |
| RADIUS / WPA-Enterprise | hostapd RADIUS config + `freeradius3` | 🟢 |
| Private Pre-Shared Keys (multi-PSK) | hostapd supports per-MAC PSK via `wpa_psk_file` | 🟡 |

---

## Settings → Networks, Routing, Firewall

| Feature | OpenWrt mapping | Verdict |
|---|---|---|
| VLAN networks with subnet/DHCP/DNS | `network` + `dhcp` UCI + bridge VLAN filtering | 🟢 |
| IPv6 (prefix delegation, RA, DHCPv6) | odhcpd | 🟢 |
| DHCP options, reservations, lease time | dnsmasq/odhcpd | 🟢 |
| mDNS repeater across VLANs | `umdns` / `avahi` reflector | 🟢 |
| IGMP proxy | `igmpproxy` | 🟢 |
| **Zone-based firewall** (Internal/DMZ/Guest/External matrix) | firewall4 zones + forwardings | 🟢 maps almost 1:1 |
| Firewall rules with IP/port/zone matching | firewall4 rules + nftables sets | 🟢 |
| Port forwarding | `config redirect` | 🟢 |
| Traffic rules (block category/app/domain) | domain sets in dnsmasq/nftables; app-matching needs DPI | 🟡 |
| Traffic routes (policy-based routing per client/network) | `ip rule` + routing tables, `mwan3` | 🟢 |
| WAN failover / load balance | `mwan3` | 🟢 |
| VPN: WireGuard / OpenVPN / IPsec site-to-site / L2TP | all present in OpenWrt | 🟢 |
| QoS / Smart Queues | `sqm-scripts` (CAKE) — arguably better than UniFi's | 🟢 |

**Current implementation boundary (2026-08-19).** Network DHCP is no longer a
renderer constant: enablement, pool start, lease count and lease time round-trip
through the UI/API/model/store and render to UCI, with legacy defaults retained.
CIDR/pool/gateway/lease validation happens before planning; VLAN 0/1 management
LAN addressing is never taken over; devices without Gateway never render a competing DHCP
server; foreign DHCP sections and firewall zones are blocking conflicts; and
the controller refuses to make a non-VLAN-aware bridge VLAN-aware. After the
operator explicitly supplied that prerequisite on the live WRT, the browser
hardware-proved bridge-VLAN, static interface, DHCP, multi-network zone-list and
directional WAN enforcement. The C6 stayed an honest legacy-swconfig no-op.
Schema 12, the API/store/model/render contract, an editable directional Zone
Matrix and its effective forwarding Master Table are shipped. No explicit row
preserves source→WAN; an explicit empty row blocks every modeled edge; reverse
initiation is independent. Foreign UCI forwarding/rule/DNAT contradictions
block rather than being edited. Schema 15 adds the source-built cross-feature
Master Table and partial Object Manager described above; only the whole-zone
subset is live-proven. QoS/application/switch-ACL and device/group routing
remain parity targets. Active foreign firewall includes,
reachable non-fw4 nft policy and an unreadable/malformed runtime ruleset block
explicit matrix policy. The signed-in live pass proved DHCP/DNS/WAN, WAN
block/restore and custom/off DHCP states; full no-LAN/client-isolation remains
open. §5bg and §5bj completed the temporary-state cleanup and final zero-change
Preview.

---

## Logs / Events

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Event stream by category | controller/audit events + once/minute OpenWrt `log.read`, source-provenanced and keyset-paginated over REST | 🟢 **live-observed**: both refreshed routers now persist OpenWrt-log producer observations; General and Audit retain keyset pagination, provenance and exact detail. Historical router-log coverage remains unavailable |
| "Blocked by Firewall" entries at ~12K/month scale | rate-limited nftables messages only if present in `logd`; dedicated nflog/ulogd ingest remains later work | 🟡; current router-log retention is 24h, 50k/device and 100k total |
| Detail panel: severity, source identity, client/action/direction/interface/IP/port/zone/policy fields where supplied | schema-16 event provenance/enrichment fields + redacted detail JSON | 🟢 source-built for stored fields; GeoIP/risk/service enrichment remains later work |
| Destination country flags | MaxMind GeoLite2 lookup in the controller | 🟢 |
| WiFi Client Connected/Disconnected/roam | redacted hostapd log events + durable per-device producer cursor | 🟢 source-built for association transitions; duration/data-used enrichment is not implied |
| General vs **Audit** log split | separate scoped REST queries; controller/audit events have their own 100k cap | 🟢 source-built; Logs are not a WebSocket topic |
| Push Notification Settings | webhook / ntfy / Gotify / email | 🟢 |
| **Export to SIEM Server** | syslog/CEF forwarder | 🟢 |
| Threat Detected and Blocked | Suricata + ET Open | 🟡 |

---

## Flows

The hardest screen in the product. Everything here depends on per-connection
logging with application identification.

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Flow table: Source, Destination, Service, Risk, Direction, In/Out zone, Action, timestamp — at "1-100 of 10000+" scale | conntrack events (`conntrackd`/netlink) + nflog for blocked | 🟡 high volume; needs a real ingest path and aggressive retention policy |
| **Application/Service identification** (SSL/TLS, Discord, QUIC, GitHub, YouTube) | `netifyd` (nDPI) on the gateway **[verify package availability]**, or `ntopng` | 🟡 the single biggest build item on this screen |
| Risk scoring (Low/Suspicious/Concerning) | 🔴 as shipped (Proofpoint feeds). Substitute: blocklist membership + geo + port heuristics + Suricata verdict. Document the heuristic |
| Flows on Map (geo visualization) | GeoLite2 + map component | 🟢 once you have flows |
| Top Destinations / Clients / Apps summary cards | aggregation over flows | 🟡 |
| Reverse DNS names for destinations | `rpcd-mod-rrdns` or controller-side rDNS with caching | 🟢 |
| Download / Customize Columns | UI + CSV export | 🟢 |

**Recommendation:** Flows is the screen most at odds with the wrapper
constraint, and it should be the last thing you build — or the first thing you
cut.

Tier check: the whole screen depends on a DPI daemon being available *in the
official feed* for the user's target, running on their gateway, with CPU to
spare. If `netifyd`/`ntopng` isn't there for a given platform, we do not solve
that by shipping our own collector — that's tier 3. We degrade to
port/IP-based classification with honest labels ("HTTPS", not "Netflix"), or we
don't ship the screen for that device.

Budget check (DEVICE-BUDGET §3.4): DPI inspects every packet, which defeats the
flow offloading that MT7621-class gateways depend on for gigabit routing. On
class-C hardware this screen is **unavailable by design**, and the UI should say
so plainly rather than showing an empty table.

Everything in Phases 0–3 runs on tier 0–1 and fits the budget on every target
class. Shipping those alone gets you a product people would use.

---

## Cross-cutting: hard blockers

| Blocker | Consequence |
|---|---|
| **No device-side code (our own rule)** | Anything needing a daemon we wrote is cut, not worked around. This is the constraint that keeps the project maintainable by a small team. |
| **Ubiquiti inform protocol** | Out of scope by design — oonfeeWRT manages OpenWrt devices only. Worth noting the appealing edge case: UniFi APs *reflashed* with OpenWrt are perfectly good managed devices. |
| **PoE control** | Most OpenWrt hardware can't. Gate on the capability registry: hide the column, don't grey it out. |
| **Spectrum analysis (AirView)** | Needs dedicated radio silicon. Coarse survey-based substitute only, honestly labelled. |
| **Paid threat intelligence** | OSS feeds are meaningfully worse. Say so rather than implying parity. |
| **NAT'd / multi-site devices** | Would need a dial-out agent. Answer: a WireGuard tunnel the user already runs, configured through oonfeeWRT like any other peer. |
| **Cloud remote access / SSO** | Out of scope. See above — the tunnel is the answer, and is arguably better. |
| **Mobile apps** | A responsive web UI covers most of it. Native is a second project. |
