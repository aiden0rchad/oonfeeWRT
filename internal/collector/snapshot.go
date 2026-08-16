package collector

import (
	"fmt"
	"net"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Tier is how hard we are polling one device.
type Tier string

const (
	// Baseline runs always, at ~60 s: reachability, firmware, and the few
	// series that need unbroken history. DEVICE-BUDGET §4.2.
	Baseline Tier = "baseline"

	// Focused runs only while a UI screen showing this device is open, at
	// ~5–10 s. Nobody watches the Radios page at 3 a.m.
	Focused Tier = "focused"
)

// Snapshot is one completed poll of one device.
//
// Every field that could be absent says so explicitly rather than defaulting to
// a zero that reads like data. That is not fastidiousness: a helper that
// returned nothing on a failed call, whose caller then treated it as an empty
// result, is exactly how this project once reported a two-minute outage that
// had not happened.
type Snapshot struct {
	DeviceID int64
	MAC      string
	Name     string
	Tier     Tier
	At       time.Time
	Duration time.Duration

	// Err is set when the poll as a whole failed — unreachable, not logged in,
	// transport error. Every other field is then meaningless.
	Err error

	// Degraded lists calls that failed inside an otherwise successful poll. A
	// snapshot with entries here is partial, not bad: one denied or unsupported
	// method must not discard the rest of the poll, and must not be mistaken
	// for a zero reading either.
	Degraded []Degradation

	Board *Board
	Hosts []Host

	// Ifaces is the radio list this poll discovered, when it asked. The NEXT
	// poll uses it: the list decides which calls go in the batch, and the batch
	// is already built by the time the answer arrives.
	Ifaces      []string
	IfacesFresh bool

	// IfaceModes is each wireless interface's CONFIGURED mode — "ap", "mesh",
	// "sta" — read on the same slow cadence as the list itself.
	//
	// It exists because "which interfaces exist" is not "which interfaces have
	// clients". An 802.11s mesh point is a wireless interface like any other,
	// and `iwinfo assoclist` on one returns its PEERS: other access points. Ask
	// it without checking the mode and the controller reports the backhaul as
	// connected users — infrastructure in a list captioned "your devices",
	// which is the same complaint scoping fixed for upstream neighbours.
	//
	// An interface missing from this map is treated as an AP, which is what the
	// controller did before the map existed. Deciding the other way would let a
	// denied call silently stop counting real clients.
	IfaceModes map[string]string

	// IfaceSections maps an interface name to the UCI wifi-iface section that
	// created it, on the same slow cadence. Optional per interface: the device
	// does not always report one, and an interface without a section is
	// attributed to the DEVICE rather than guessed at — the site model permits
	// one mesh id on two bands, so a guess would be wrong exactly where it
	// would matter most.
	IfaceSections map[string]string

	// ConfiguredIfacesAbsent names wifi-iface sections that have a configured
	// mode and NO interface.
	//
	// This is §5q's signature and it used to be discarded: a mesh that applied
	// cleanly, passed its health check, landed its confirm, and whose interface
	// the driver never created. Without this the controller cannot tell "the
	// mesh you configured does not exist" from "you configured no mesh".
	ConfiguredIfacesAbsent []string

	// Networks are the device's IPv4 subnets, refreshed on the slow cadence and
	// carried forward on every poll in between — the hosts they classify arrive
	// every poll, so a snapshot without them could not scope its own clients.
	Networks []Network
	// askedNetworks records whether THIS poll requested them, which is what
	// decides freshness. A device with no IPv4 address returns an empty list
	// legitimately, so len(Networks) cannot tell "asked, and there are none"
	// from "did not ask" — the same distinction IfacesFresh exists for.
	askedNetworks bool

	Uptime     int64
	Load       [3]float64 // 1/5/15 minute, already unscaled from /65536
	Memory     Memory
	Interfaces map[string]Interface

	// NetDevsFresh records that network.device status ANSWERED on this poll.
	//
	// Without it, absence from Interfaces is ambiguous — a refused call and a
	// device with no such interface look identical, and reading the first as
	// the second turns silence into a claim about the kernel.
	NetDevsFresh bool
	APs          []AP
	Stations     []Station // focused only
	Surveys      []Survey  // focused only
}

// OK reports a poll that reached the device.
func (s *Snapshot) OK() bool { return s.Err == nil }

// Complete reports a poll where every call also succeeded.
func (s *Snapshot) Complete() bool { return s.Err == nil && len(s.Degraded) == 0 }

// Degradation is one call that failed within a poll.
//
// Status is kept because it is the difference between "this device cannot do
// this" and "we were not granted it" — the distinction the capability model is
// built on, and one that is lost the moment a failure is flattened to a bool.
type Degradation struct {
	Object string
	Method string
	Status ubus.Status
	Err    string

	// Permanent marks a failure that retrying cannot fix, so a caller can stop
	// asking rather than re-failing every interval forever.
	Permanent bool
}

func (d Degradation) String() string {
	return fmt.Sprintf("%s.%s: %s", d.Object, d.Method, d.Err)
}

// Board is the firmware identity, re-read rarely because it changes only on
// upgrade — but re-read, because that is how an upgrade is noticed.
type Board struct {
	Model     string `json:"model"`
	BoardName string `json:"board_name"`
	Kernel    string `json:"kernel"`
	Hostname  string `json:"hostname"`
	Release   struct {
		Distribution string `json:"distribution"`
		Version      string `json:"version"`
		Revision     string `json:"revision"`
		Target       string `json:"target"`
		Description  string `json:"description"`
	} `json:"release"`
}

// Memory is what system.info reports, in bytes.
type Memory struct {
	Total     int64 `json:"total"`
	Free      int64 `json:"free"`
	Buffered  int64 `json:"buffered"`
	Cached    int64 `json:"cached"`
	Available int64 `json:"available"`
}

// Used reports memory in use, preferring the kernel's own "available" figure.
//
// free+buffered+cached overstates pressure badly on a router, where the page
// cache is most of RAM and is reclaimable. Older builds omit available, so the
// fallback stays.
func (m Memory) Used() int64 {
	if m.Available > 0 {
		return m.Total - m.Available
	}
	return m.Total - m.Free - m.Buffered - m.Cached
}

// Interface is one network device's counters, the input to throughput.
//
// Counters, not rates. Rates are computed on the controller from two samples,
// per DEVICE-BUDGET §4.3 — never ask the router to do arithmetic for us.
type Interface struct {
	Up      bool   `json:"up"`
	Carrier bool   `json:"carrier"`
	MTU     int    `json:"mtu"`
	MAC     string `json:"macaddr"`
	Stats   struct {
		RxBytes   int64 `json:"rx_bytes"`
		TxBytes   int64 `json:"tx_bytes"`
		RxPackets int64 `json:"rx_packets"`
		TxPackets int64 `json:"tx_packets"`
		RxErrors  int64 `json:"rx_errors"`
		TxErrors  int64 `json:"tx_errors"`
	} `json:"statistics"`
}

// Host is one thing seen on the LAN, merged from the two cheap sources.
//
// Measured on the reference device: luci-rpc.getHostHints costs 5.1 ms and
// getDHCPLeases 2.9 ms. Together they roughly double the baseline poll (8 ms to
// ~16 ms) and buy the entire client inventory, which is why they sit in the
// baseline tier rather than waiting for someone to open a screen — a client list
// that only exists while you are looking at it is not an inventory.
//
// Note what is NOT here: luci-rpc.getWirelessDevices, measured at 128.8 ms, as
// expensive as an entire focused poll. It belongs to adoption, never to a poll.
type Host struct {
	MAC   string   `json:"mac"`
	Name  string   `json:"name"`
	IPv4  []string `json:"ipv4"`
	IPv6  []string `json:"ipv6"`
	Lease int64    `json:"lease_expires"` // 0 when there is no DHCP lease
}

// Network is one logical interface's IPv4 subnet, as netifd reports it.
//
// This is what makes it possible to say whether a host the router can see is
// actually on the network the router serves. On a gateway, the ARP and
// neighbour tables the client inventory is built from cover EVERY interface —
// so a device with a WAN uplink lists its upstream network's hosts alongside
// its own clients, indistinguishably. Measured on the reference device: 8 of
// its 16 known hosts were neighbours on the uplink, not clients.
//
// Upstream is taken from the routing table rather than from the interface being
// named "wan". The name is a convention and nothing enforces it; carrying the
// default route is what actually makes an interface the way out.
type Network struct {
	Name     string `json:"name"`     // netifd's logical name: "lan", "wan"
	CIDR     string `json:"cidr"`     // "192.168.1.1/24"
	Upstream bool   `json:"upstream"` // carries the 0.0.0.0/0 route
}

// Scope reports which side of the router an address is on.
//
// Three-state, and the third value carries weight: an address in no known
// subnet has not been shown to be either local or upstream. That happens for
// real — a host seen in a neighbour table before it has an address, a static
// address outside every configured subnet, a device on a VLAN the controller
// has not read. Calling those "local" would put someone else's hardware in a
// list captioned "your devices"; calling them "upstream" would hide a device
// that genuinely belongs to the operator.
func (s *Snapshot) Scope(ip string) string {
	if ip == "" || len(s.Networks) == 0 {
		return ScopeUnknown
	}
	addr := net.ParseIP(ip)
	if addr == nil {
		return ScopeUnknown
	}
	for _, n := range s.Networks {
		_, subnet, err := net.ParseCIDR(n.CIDR)
		if err != nil || subnet == nil {
			continue
		}
		if subnet.Contains(addr) {
			if n.Upstream {
				return ScopeUpstream
			}
			return ScopeLocal
		}
	}
	return ScopeUnknown
}

// Scope values, mirroring store's. Duplicated rather than imported because the
// collector does not depend on the store — the dependency runs one way.
const (
	ScopeLocal    = "local"
	ScopeUpstream = "upstream"
	ScopeUnknown  = "unknown"
)

// AP is one BSS as hostapd reports it — the cheap source.
//
// Measured: hostapd.<iface> costs ~1 ms against iwinfo's ~30 ms, which is why
// the baseline tier uses it. It is not a substitute for assoclist, which alone
// carries tx.retries, connected_time, signal_avg, noise and thr.
type AP struct {
	Iface   string
	SSID    string
	BSSID   string
	Channel int
	Freq    int

	// Clients is nil when the call that would have counted them failed.
	//
	// Not zero. "We could not ask" and "nobody is connected" are different
	// answers, and a graph that draws the first as the second invents an outage.
	Clients *int

	// Airtime is the BSS load. Utilization is the 802.11 0–255 scale, NOT a
	// percentage — 172 is about 67%. Anything rendering it directly as a percent
	// is wrong.
	Airtime *Airtime
}

// Airtime is hostapd's channel occupancy for one BSS.
type Airtime struct {
	Time        int64 `json:"time"`
	TimeBusy    int64 `json:"time_busy"`
	Utilization int   `json:"utilization"` // 0–255, not a percentage
}

// UtilizationPercent converts the BSS-Load scale to a percentage, which is the
// only form a UI should ever show.
func (a Airtime) UtilizationPercent() float64 { return float64(a.Utilization) * 100 / 255 }

// Station is one associated client, from iwinfo.assoclist.
//
// Noise is deliberately absent. On mwlwifi the per-station value swings 37 dB
// between consecutive reads, so a per-sample SNR built from it flails visibly.
// Callers wanting SNR must smooth over several samples, which is a decision for
// whatever draws it, not something to bake in here.
type Station struct {
	Iface         string
	MAC           string `json:"mac"`
	Signal        int    `json:"signal"`
	SignalAvg     int    `json:"signal_avg"`
	Noise         int    `json:"noise"`
	InactiveMs    int64  `json:"inactive"`
	ConnectedTime int64  `json:"connected_time"`
	Thr           int64  `json:"thr"`
	RX            Rate   `json:"rx"`
	TX            Rate   `json:"tx"`
}

// Rate is one direction of a station's PHY state.
//
// Units are iwinfo's: rate is kbit/s. hostapd's get_clients reports the same
// quantity 100× larger. Never mix the two sources in one series.
type Rate struct {
	Bytes   int64 `json:"bytes"`
	Packets int64 `json:"packets"`
	Rate    int64 `json:"rate"` // kbit/s
	MCS     int   `json:"mcs"`
	MHz     int   `json:"mhz"`
	ShortGI bool  `json:"short_gi"`
	Retries int64 `json:"retries"`
	Failed  int64 `json:"failed"`
}

// Survey is one channel survey sample.
//
// ActiveTime and BusyTime are monotonic COUNTERS in milliseconds, not a ratio,
// and they do not share an epoch. Measured 2026-08-13 on the reference device:
// the 5 GHz radio reported active=24427 against busy=922104 — busy 37x larger —
// while both advanced sanely (active tracked the wall clock to 99%). Channel
// utilization is therefore the ratio of two DELTAS, and the ratio of the
// absolute values is meaningless.
//
// That is not a pedantic distinction. On 5 GHz the absolute ratio yields 1354%,
// which anyone would spot. On 2.4 GHz it yields 25.9% while the true figure was
// 73.3% — plausible, wrong by 3x, and nobody would question it. hostapd's
// independent BSS-load reading on the same radio said 70%, which is what
// settled it.
//
// So this type deliberately offers NO percentage method. Utilization is
// computed in internal/telemetry alongside the other counter-derived rates,
// where the previous reading is in hand.
//
// rx_time and tx_time are separately unusable on mwlwifi: they never advance,
// so the airtime split and interference are not computable — present but wrong,
// which no presence probe would have caught.
//
// RxTime and TxTime are unsigned for a concrete reason. mwlwifi does not merely
// leave them at zero, it leaves them uninitialised, and the values that come
// back exceed the range of a signed 64-bit integer. Decoding them as int64
// fails, and because one decode error discards the whole object, that would
// throw away the busy/active times in the same response — losing the only part
// of the survey that works, to a field documented as unusable.
type Survey struct {
	Iface      string
	MHz        int    `json:"mhz"`
	Noise      int    `json:"noise"`
	ActiveTime int64  `json:"active_time"`
	BusyTime   int64  `json:"busy_time"`
	RxTime     uint64 `json:"rx_time"`
	TxTime     uint64 `json:"tx_time"`
}

// NoiseDBm returns the survey noise floor in dBm.
//
// iwinfo.survey reports noise UNSIGNED here while iwinfo.info reports the same
// quantity signed: 161 means −95. Anything above 0 is a wrapped negative.
//
// Correctly decoded is not the same as trustworthy. Measured on mwlwifi
// 2026-08-13: the 2.4 GHz radio read −95 dBm and jumped to −70 dBm sporadically
// — a 25 dB spread over 12 samples — while the 5 GHz radio on the same driver
// held within 2 dB, and channel busy time did not explain the excursions
// (82% mean busy during them against 76% otherwise, with fully overlapping
// ranges). The collector reports the raw value and the capability probe records
// the instability as a quirk; smoothing belongs to whatever draws it. A single
// sample is not a noise floor.
func (s Survey) NoiseDBm() int {
	if s.Noise > 0 {
		return s.Noise - 256
	}
	return s.Noise
}
