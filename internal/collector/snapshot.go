package collector

import (
	"fmt"
	"net"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/meshlink"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/observability"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
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

	// WANOnly marks a lightweight snapshot containing a gateway probe and no
	// full-poll state. LogOnly may be true on the same batched request. Consumers
	// must use only the explicitly marked auxiliary payloads: repeating cached
	// uptime, load, clients, or topology at this cadence would turn old values
	// into new measurements.
	WANOnly bool
	// LogOnly marks a lightweight snapshot containing a router-log page and no
	// full-poll state. It keeps the one-minute continuity cursor independent of a
	// deliberately slower full baseline.
	LogOnly bool

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

	// IfaceRadios maps runtime interfaces to stable UCI wifi-device keys. It is
	// selectively decoded; plaintext wireless credentials never enter this map.
	IfaceRadios map[string]string

	// Radios is the cached per-radio inventory and channel list. Its identity is
	// always the UCI wifi-device key; runtime interfaces and PHY names are only
	// observations. RadiosKnown distinguishes a proved empty inventory from one
	// getWirelessDevices has never successfully supplied.
	Radios      []radio.LiveState
	RadiosKnown bool
	// RadiosStale means cached inventory outlived its slow refresh or its latest
	// refresh failed. Last-known values may still be displayed, but must not
	// select a current survey row or stable-key radio aggregation.
	RadiosStale bool

	// AirtimeSplit carries the target's persisted capability proof into
	// telemetry. False includes absent, refused, and unknown: none may license
	// interference or RX/TX airtime values.
	AirtimeSplit bool

	// These fields carry only this batch's radio answers into the poller's
	// cache reconciliation. A requested key absent from radioFrequencies failed
	// and therefore becomes unknown instead of inheriting a stale success.
	radioInventory      []radio.InventoryRadio
	radioInventoryAsked bool
	radioInventoryOK    bool
	radioFrequencyAsked map[string]bool
	radioFrequencies    map[string][]radio.Frequency

	// ConfiguredIfacesAbsent names wifi-iface sections that have a configured
	// mode and NO interface.
	//
	// This is §5q's signature and it used to be discarded: a mesh that applied
	// cleanly, passed its health check, landed its confirm, and whose interface
	// the driver never created. Without this the controller cannot tell "the
	// mesh you configured does not exist" from "you configured no mesh".
	ConfiguredIfacesAbsent []string

	// MeshPeers is each mesh interface's peers, read on the slow cadence.
	//
	// A key present with an empty slice is a demonstrated zero; a key ABSENT
	// means nobody asked or the answer did not come back. The state ladder has
	// a separate rung for each, and this map is where that distinction has to
	// survive — collapse it here and nothing downstream can rebuild it.
	MeshPeers map[string][]meshlink.Peer

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

	// APsFresh records that this poll ASKED hostapd/interface inventory which
	// BSSs are enabled, whether or not anything in it serves clients. This is
	// not independent on-air scan evidence.
	//
	// Needed for the same reason as IfacesFresh, and missing for the reason
	// that keeps catching this package out: the cache was written only when APs
	// was non-empty, which is a proxy for "asked" and wrong in both directions.
	// A device broadcasting nothing could never record that it had been looked
	// at, so the API answered "no poll has looked" about a device polled
	// hundreds of times. And a BSS that went away was never cleared, so a
	// removed SSID stayed reported as on the air forever — including one an
	// operator had just been told to remove by the takeover brief.
	APsFresh bool

	// apStatusOK counts the hostapd get_status calls that ANSWERED on this
	// poll. APsFresh is computed from it rather than from intent — see poll().
	apStatusOK int

	// Logs are a bounded non-streaming logd page. LogsFresh is true only when
	// the rows, boot ID and running logd PID all answered on the same poll.
	Logs      []observability.LogEntry
	LogEpoch  observability.LogEpoch
	LogsFresh bool
	logReadOK bool
	logBootOK bool
	logPIDOK  bool

	// WAN is a completed gateway-vantage reachability probe. Nil means it was
	// not attempted or did not produce a trustworthy result; a non-nil probe
	// with Up=false is the distinct, measured 100%-loss state.
	WAN *WANProbe

	// Topology carries only the current poll's no-install observations. Sources
	// explicitly distinguish an answered empty result from unavailable data.
	Topology TopologySnapshot

	Stations []Station // focused only
	Surveys  []Survey  // focused only

	// AssocAsked/AssocAnswered preserve focused-call completeness per BSS. An
	// answered BSS may still contribute per-client facts, but a radio aggregate
	// is valid only when every BSS asked on that radio answered.
	AssocAsked    map[string]bool
	AssocAnswered map[string]bool
}

type TopologySnapshot struct {
	Cycle          bool
	NetworkDevices []topology.NetworkDevice
	Wireless       []topology.WirelessRadio
	Neighbors      map[int][]topology.Neighbor
	Bridges        []topology.BridgeObservation
	LLDP           []topology.LLDPLink
	Uplinks        []topology.Uplink
	Sources        []model.TopologySourceObservation

	expected map[string]int
	answered map[string]int
	evidence map[string]int
	failures map[string]int
	// failureCauses preserves why each topology source failed. The durable
	// source state needs this distinction so a permission gap can offer an
	// explicit opt-in without presenting decode, unsupported, or transport
	// failures as something an ACL change can fix.
	failureCauses map[string]map[DegradationCause]struct{}
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
	// Target identifies the fixed controller-side command behind a generic
	// file.exec call. It never comes from a router response and contains no
	// credentials; without it every missing optional utility collapses to the
	// same unactionable "file.exec" row.
	Target string
	Status ubus.Status
	// Cause is the failure domain, kept separately from the rendered error text
	// so an API/UI can distinguish an ACL or unsupported driver operation from a
	// malformed/transient exchange without parsing an English sentence.
	Cause DegradationCause
	Err   string

	// Permanent marks a failure that retrying cannot fix, so a caller can stop
	// asking rather than re-failing every interval forever.
	Permanent bool
}

// DegradationCause identifies who can plausibly fix a failed poll call.
type DegradationCause string

const (
	CausePermission  DegradationCause = "permission"
	CauseUnsupported DegradationCause = "unsupported"
	CauseDevice      DegradationCause = "device"
	CauseTransport   DegradationCause = "transport"
	CauseProtocol    DegradationCause = "protocol"
	CauseDecode      DegradationCause = "decode"
	CauseUnknown     DegradationCause = "unknown"
)

func (d Degradation) String() string {
	call := d.Object + "." + d.Method
	if d.Target != "" {
		call += " " + d.Target
	}
	return fmt.Sprintf("%s: %s", call, d.Err)
}

func degradationTarget(inv ubus.Invocation) string {
	if inv.Object != "file" || inv.Method != "exec" {
		return ""
	}
	args, ok := inv.Args.(map[string]any)
	if !ok {
		return ""
	}
	command, _ := args["command"].(string)
	return command
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
	Up      bool           `json:"up"`
	Carrier bool           `json:"carrier"`
	MTU     int            `json:"mtu"`
	MAC     string         `json:"macaddr"`
	Stats   InterfaceStats `json:"statistics"`
}

// InterfaceStats keeps field presence beside each counter. OpenWrt versions
// and drivers may omit one direction; an omitted counter must not seed a zero
// baseline that later looks like traffic or a 32-bit wrap.
type InterfaceStats struct {
	RxBytes   int64 `json:"rx_bytes"`
	TxBytes   int64 `json:"tx_bytes"`
	RxPackets int64 `json:"rx_packets"`
	TxPackets int64 `json:"tx_packets"`
	RxErrors  int64 `json:"rx_errors"`
	TxErrors  int64 `json:"tx_errors"`

	RxBytesKnown   bool `json:"-"`
	TxBytesKnown   bool `json:"-"`
	RxPacketsKnown bool `json:"-"`
	TxPacketsKnown bool `json:"-"`
	RxErrorsKnown  bool `json:"-"`
	TxErrorsKnown  bool `json:"-"`
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

	// Stations is every client hostapd reported on this BSS, keyed by MAC.
	//
	// From the same get_clients call that produces Clients, at the BASELINE
	// rate, so this costs nothing extra. It used to be discarded: the decoder
	// read the MAC-keyed map and kept `len()`, so the controller knew how MANY
	// clients an AP had and not WHICH, every sixty seconds, on every device.
	//
	// The clients grid showed "unknown" for connection, access point and signal
	// on a fleet where two devices were associated and hostapd was reporting
	// both of them with an RSSI. Only TX retries genuinely needs the focused
	// tier — hostapd does not report retries here, iwinfo.assoclist does.
	//
	// Nil when the call failed, for the same reason Clients is.
	Stations map[string]LiveStation
}

// LiveStation is one associated client as hostapd described it at the baseline
// rate.
type LiveStation struct {
	// Iface is the BSS it is associated to.
	Iface string
	// Signal is RSSI in dBm, nil when hostapd did not report one. Absent and
	// zero are different: 0 dBm is a real, implausible reading and would draw
	// as a perfect signal.
	Signal *int
}

// LiveStationSet preserves every BSS that reported a MAC. A client can appear
// on two BSSes during a roam (or because a driver briefly reports stale
// association state); collapsing those sightings to one map value would make
// whichever AP happened to be iterated last look measured.
type LiveStationSet map[string][]LiveStation

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

	// PresenceKnown distinguishes a decoded zero from a field the driver did
	// not report. Hand-built snapshots in tests predate this metadata.
	PresenceKnown  bool `json:"-"`
	SignalKnown    bool `json:"-"`
	SignalAvgKnown bool `json:"-"`
	TXQualityKnown bool `json:"-"`
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

	BytesKnown   bool `json:"-"`
	PacketsKnown bool `json:"-"`
	RateKnown    bool `json:"-"`
	RetriesKnown bool `json:"-"`
	FailedKnown  bool `json:"-"`
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

	PresenceKnown   bool `json:"-"`
	MHzKnown        bool `json:"-"`
	NoiseKnown      bool `json:"-"`
	ActiveTimeKnown bool `json:"-"`
	BusyTimeKnown   bool `json:"-"`
	RxTimeKnown     bool `json:"-"`
	TxTimeKnown     bool `json:"-"`
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
