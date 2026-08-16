# oonfeeWRT — Feature Parity Matrix

Derived from a live UniFi install running **UniFi OS 5.1.19 / Network 10.4.57**
(July 2026), screen by screen.

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

Tier 2 items degrade gracefully: if the user declines the package, that one
feature is absent and everything else works.

---

## Dashboard

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Console card: device counts, gateway IP, uptime | `system.info`, `network.interface` status, controller inventory | 🟢 |
| "Network / UniFi OS / Devices — Up to date" | our own version + `owut` check | 🟢 |
| ISP card: name, IPv4, uptime %, throughput sparkline | WAN interface counters + PPPoE/DHCP info; ISP name from ASN lookup of the WAN IP | 🟢 |
| Monthly data usage (4.44 TB) | `vnstat` on the WAN interface | 🟢 |
| Latency pills (Microsoft 18ms / Google 72ms / Cloud 21ms) | controller-scheduled ICMP/TCP probes to a configurable target list | 🟢 |
| Main chart: download/upload/latency/packet loss over 1h–1M | our TSDB + probe series, dual-axis | 🟢 |
| WAN uptime strip under the chart | probe series → uptime bar | 🟢 |
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
| Tree graph internet → gateway → switches → APs → clients | `lldpd` neighbors + `bridge fdb` + ARP + wireless assoc tables | 🟢 LLDP is the backbone; expect ambiguity for unmanaged switches (they're invisible — multiple MACs on one port is the tell) |
| Expand/collapse node badges, zoom/pan, navigation mode | UI-side (d3/Cytoscape) | 🟢 |
| Filter rail: device status, client type, VLAN, WiFi broadcast, vendor | our indices | 🟢 |
| "Show Internet Traffic" overlay on links | live throughput per link from interface counters | 🟢 |
| VLAN chip row at the bottom (colorized paths) | network model | 🟢 |
| Infrastructure sub-tab (physical rack/port view) | requires port-level topology | 🟠 |
| Right slide-over: radio rows (Ch/width/MIMO/clients), AirView, active clients, TX retries timeline, memory, uptime, WiFi Exp % | `iwinfo`, `iw station dump`, `iw survey dump`, `system.info` | 🟢 for all but AirView |
| **AirView** (continuous spectrum analyzer waterfall) | 🔴 requires dedicated spectrum-scan silicon on Ubiquiti radios. Substitute: periodic `iw scan` + survey → a coarse channel-occupancy heatmap. Useful, visibly not the same thing |
| Device Version + one-click **Revert** to prior firmware | dual-image/failsafe support is device-specific on OpenWrt | 🟠 |

---

## Client Devices

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Table: Name, Vendor, Connected To, Network, WiFi, Experience, Technology, Channel, IP, Activity, Down, Up, 24h Usage | `luci-rpc.getHostHints`, `iw station dump`, DHCP leases, `nlbwmon` | 🟢 |
| Online/Offline status dot + history | our poller + presence tracking | 🟢 |
| Vendor column | MAC OUI database | 🟢 |
| Experience column ("Excellent") | **our formula** — see ARCHITECTURE §5 | 🟡 define and document it |
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
| Port table: Port, Name, STP, Connection, Speed, Connected MAC/IP, Profile, Native VLAN | DSA: `ip link`, `bridge vlan`, `bridge fdb`, `ethtool` | 🟠 good on DSA-supported switches; nonexistent on unmanaged hardware |
| Per-port throughput chart, Total/By Port, Packets/Usage/Errors/Dropped | `ethtool -S` + `/proc/net/dev` deltas | 🟠 same dependency |
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
| Channel Plan visualization (In Use / Enabled / DFS / Not available / Excluded) | `iwinfo.freqlist` + regulatory domain + our exclusion model | 🟢 |
| **Channel AI View** (auto channel selection heatmap) | 🔴 as branded. Substitute: our own channel scoring from survey + scan data → "Suggested Channels". The underlying math (least-congested selection weighted by neighbor RSSI) is not hard; the branding is theirs |
| RF scan / spectrum sub-tabs | `iw scan` (user-triggered only) | 🟡 disruptive on serving radios — must be explicit and warned |

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
| Policy Engine group | our zone/policy model | 🟢 |
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
| Client isolation | bridge `isolate` / ebtables | 🟢 |
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

---

## Logs / Events

| UniFi element | OpenWrt source | Verdict |
|---|---|---|
| Event stream by category (Client Devices, Internet/WAN, Power, Security, Software Updates, Devices, Ports, VPN, Host) | our event bus: poller state transitions + nflog + syslog ingest | 🟢 |
| "Blocked by Firewall" entries at ~12K/month scale | nftables `log` → **nflog** → `ulogd` or agent → controller | 🟢 volume is real; design the ingest for it |
| Detail panel: severity, risk, action, service, policy link, direction, in/out interface, source client/IP/MAC/hostname/vendor/model/port/zone/network/subnet, destination IP/region/port/zone | nflog metadata + our enrichment (client identity, zone, GeoIP) | 🟢 enrichment is ours to build and it's the good part |
| Destination country flags | MaxMind GeoLite2 lookup in the controller | 🟢 |
| WiFi Client Connected/Disconnected with duration + data used | hostapd events + accounting | 🟢 |
| General vs **Audit** log split | our own admin action audit trail | 🟢 |
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
