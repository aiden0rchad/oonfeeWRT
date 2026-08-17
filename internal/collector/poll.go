package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aiden0rchad/oonfeewrt/internal/meshlink"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// loadScale is the fixed-point divisor system.info uses for load averages.
const loadScale = 65536.0

// call is one invocation plus what to do with its result. Keeping the two
// together is what lets the whole poll be assembled, sent as a single batch,
// and decoded positionally without an index-to-meaning table to get wrong.
type call struct {
	inv    ubus.Invocation
	decode func(json.RawMessage, *Snapshot) error

	// optional marks a call whose failure degrades the snapshot rather than
	// meaning the device is unreachable.
	optional bool
}

// poll performs one complete poll in a single HTTP round trip.
//
// One request, not one per metric. Measured on class A: batching is flat at
// ~0.5 ms per call from ten calls up, and a 60 s baseline poll never reuses its
// connection anyway (uhttpd's keep-alive is 20 s), so every call it does not
// batch costs another handshake.
func (p *poller) poll(ctx context.Context, c *ubus.Client, tier Tier,
	ifaces []string, modes map[string]string) Snapshot {
	snap := Snapshot{
		DeviceID: p.target.DeviceID,
		MAC:      p.target.MAC,
		Name:     p.target.Name,
		Tier:     tier,
		At:       p.c.now(),
	}
	// What this poll is about to ask about broadcasting interfaces. The flag
	// itself is set at the END, from what came BACK — see below.
	listed := len(ifaces) > 0 || p.everListedIfaces()
	askedAPs := 0
	for _, iface := range ifaces {
		if servesClients(modes, iface) {
			askedAPs++
		}
	}
	calls := p.buildCalls(tier, ifaces, modes)
	invs := make([]ubus.Invocation, len(calls))
	for i, c := range calls {
		invs[i] = c.inv
	}

	start := p.c.now()
	results, err := c.Batch(ctx, invs)
	snap.Duration = p.c.now().Sub(start)
	if err != nil {
		snap.Err = err
		return snap
	}
	if len(results) != len(calls) {
		// A batch that returns the wrong number of results cannot be matched to
		// its requests, and guessing the alignment would silently file one
		// object's data under another's name.
		snap.Err = fmt.Errorf("collector: device returned %d results for %d calls",
			len(results), len(calls))
		return snap
	}

	for i, res := range results {
		spec := calls[i]
		if res.Err != nil {
			d := Degradation{
				Object: spec.inv.Object, Method: spec.inv.Method,
				Status: res.Status, Err: res.Err.Error(),
				Permanent: ubus.IsPermanent(res.Err),
			}
			if !spec.optional {
				// A required call failing means we did not really reach the
				// device in any useful sense; say so rather than emitting a
				// snapshot full of zeroes.
				snap.Err = fmt.Errorf("collector: %s: %w", d, res.Err)
				snap.Degraded = append(snap.Degraded, d)
				return snap
			}
			snap.Degraded = append(snap.Degraded, d)
			continue
		}
		if err := spec.decode(res.Data, &snap); err != nil {
			d := Degradation{
				Object: spec.inv.Object, Method: spec.inv.Method,
				Err: fmt.Sprintf("decode: %v", err),
			}
			snap.Degraded = append(snap.Degraded, d)
			if !spec.optional {
				// A required call that answered with something we cannot read is
				// no better than one that did not answer. Previously only
				// res.Err failed the poll, so an unparseable system.info left
				// Load and Memory at their zero values and the telemetry layer
				// recorded a load average of 0 — a measurement that was never
				// taken, indistinguishable from an idle device.
				snap.Err = fmt.Errorf("collector: %s: %w", d, err)
				return snap
			}
		}
	}
	// Decided by the ANSWERS, not by the intent.
	//
	// Having a current interface list is necessary and not sufficient. The
	// first version of this set the flag from `len(ifaces) > 0` before the
	// batch ran, so a device whose hostapd calls were all REFUSED reported
	// broadcast_known:true with an empty list — a positive claim that nothing
	// is on the air, produced by a check that never answered. That is the
	// cardinal error, introduced by the fix for the cardinal error.
	//
	// askedAPs == 0 still counts as fresh: a device whose radios are off, or
	// whose only interfaces are mesh, legitimately has nothing to ask about,
	// and "asked, and there are none" is the answer this flag exists to make
	// recordable. Every early return above leaves it false, which is correct —
	// none of them reached the device in a useful sense.
	snap.APsFresh = listed && snap.apStatusOK == askedAPs
	return snap
}

// buildCalls assembles the request set for a tier.
//
// The split between tiers is the budget. Baseline reads only what is cheap and
// what needs unbroken history; everything driver-expensive waits until somebody
// is actually looking. Measured: iwinfo is ~92% of a focused poll (194 ms vs
// 15.8 ms without it), and hostapd answers the per-AP questions ~30× faster.
func (p *poller) buildCalls(tier Tier, ifaces []string, modes map[string]string) []call {
	calls := []call{
		{inv: ubus.Invocation{Object: "system", Method: "info"}, decode: decodeInfo},
		{
			inv:      ubus.Invocation{Object: "network.device", Method: "status"},
			decode:   decodeNetDevices,
			optional: true,
		},
	}
	// The client inventory. Cheap enough for every poll (5.1 ms + 2.9 ms
	// measured) and the only way the Client Devices screen has data when nobody
	// is looking at a particular device.
	calls = append(calls,
		call{
			inv:      ubus.Invocation{Object: "luci-rpc", Method: "getHostHints"},
			decode:   decodeHostHints,
			optional: true,
		},
		call{
			inv:      ubus.Invocation{Object: "luci-rpc", Method: "getDHCPLeases"},
			decode:   decodeDHCPLeases,
			optional: true,
		})
	if p.needIfaces() {
		// In the batch, not beside it. A separate Call here was the one thing
		// breaking this package's own "one request per poll" rule, and it cost a
		// whole extra HTTP request — measured by the budget harness as 1.08
		// req/min at steady state against a stated ceiling of 1.0.
		//
		// The result is used by the NEXT poll rather than this one, because the
		// interface list decides which calls go in the batch and the batch is
		// already built. Interfaces change only when someone reconfigures the
		// radios, so a poll of staleness costs nothing; the alternative costs a
		// request every time, forever.
		calls = append(calls, call{
			inv:      ubus.Invocation{Object: "iwinfo", Method: "devices"},
			decode:   decodeIfaces,
			optional: true,
		})
		// And what each of them is FOR, from the one call that answers it for
		// every interface at once. iwinfo.info would need one call per
		// interface; this needs one per device, on the 15-minute cadence.
		calls = append(calls, call{
			inv:      ubus.Invocation{Object: "luci-rpc", Method: "getWirelessDevices"},
			decode:   decodeIfaceModes,
			optional: true,
		})
	}
	if p.needMeshPeers() {
		// Mesh peers, on their own slow cadence and only for interfaces already
		// known to be mesh points.
		//
		// Deliberately NOT inside needIfaces(). The mode map that says which
		// interface is a mesh comes FROM that fetch and is only available to
		// the following poll, so an exec gated on the same condition can never
		// fire: the poll that learns about the mesh has not got the modes yet,
		// and the poll that has them is not re-reading. Found by watching a
		// live mesh sit at peers-not-counted forever.
		//
		// This is the one process spawn in the poll, and the tier is a
		// deliberate reading of a documentation conflict. DEVICE-BUDGET §3.2's
		// rule says file.exec belongs "at the slow-loop interval, never the
		// fast one"; its feature table lists `iw station dump` as focused-rate.
		// The rule wins, and nothing is lost by it: a mesh peer appears or
		// disappears when somebody unplugs a node or a link finally
		// establishes, not on the timescale of somebody watching a screen.
		//
		// `iwinfo.assoclist` is NOT used even though it is granted, returns the
		// same peers as JSON, and needs no spawn. It carries no `mesh plink`,
		// and plink is the entire difference between a count and a health
		// reading — a peer stuck at OPN_SNT is indistinguishable from an
		// established one without it, so a backhaul carrying nothing would read
		// as healthy.
		// Iterated over MODES, not over the iwinfo interface list.
		//
		// The two disagree, and only one of them is authoritative here.
		// `iwinfo.devices` did not list the live `phy0-mesh0` on the reference
		// device — measured — while `luci-rpc.getWirelessDevices`, which is
		// where modes come from, did. Iterating the iwinfo list meant the exec
		// was never issued for any mesh at all, and a working backhaul sat at
		// peers-not-counted forever.
		var meshIfaces []string
		for iface, mode := range modes {
			if mode == "mesh" {
				meshIfaces = append(meshIfaces, iface)
			}
		}
		sort.Strings(meshIfaces)
		meshed := len(meshIfaces) > 0
		for _, iface := range meshIfaces {
			calls = append(calls, call{
				inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
					"command": "/usr/sbin/iw",
					"params":  []string{"dev", iface, "station", "dump"},
				}},
				decode:   decodeMeshPeers(iface),
				optional: true,
			})
		}
		// Stamped on the ATTEMPT, and only when there was something to ask —
		// the same rule needNetworks follows. A device whose ACL refuses the
		// exec would otherwise re-request it on every poll forever; a device
		// with no mesh must not have its timer reset by a poll that asked
		// nothing, or the first mesh it gains waits a full cadence.
		if meshed {
			p.mu.Lock()
			p.meshAt = p.c.now()
			p.mu.Unlock()
		}
	}
	if p.needNetworks() {
		// Same reasoning as the radio list: in the batch, on the slow cadence,
		// and used by the poll that asked for it rather than the next one — the
		// hosts it classifies arrive in this same batch. Subnets change when a
		// human renumbers a network or a WAN lease moves, neither of which
		// happens between polls.
		calls = append(calls, call{
			inv:      ubus.Invocation{Object: "network.interface", Method: "dump"},
			decode:   decodeNetworks,
			optional: true,
		})
		// Stamped on the ATTEMPT, not on the answer. A device whose ACL does not
		// grant network.interface would otherwise never set the timestamp and so
		// would re-request the call on every single poll, forever, for an answer
		// it is never going to give. The decoder separately marks a successful
		// read, which is what decides whether the cached subnets are replaced.
		p.mu.Lock()
		p.netAt = p.c.now()
		p.mu.Unlock()
	}
	if p.needBoard() {
		calls = append(calls, call{
			inv:      ubus.Invocation{Object: "system", Method: "board"},
			decode:   decodeBoard,
			optional: true,
		})
	}
	for _, iface := range ifaces {
		// A mesh point's peers are other access points, not clients. Asking
		// hostapd for its "clients" reports the backhaul as connected users.
		if !servesClients(modes, iface) {
			continue
		}
		obj := "hostapd." + iface
		calls = append(calls,
			call{
				inv:      ubus.Invocation{Object: obj, Method: "get_status"},
				decode:   decodeAPStatus(iface),
				optional: true,
			},
			call{
				inv:      ubus.Invocation{Object: obj, Method: "get_clients"},
				decode:   decodeAPClients(iface),
				optional: true,
			},
		)
	}
	if tier != Focused {
		return calls
	}
	for _, iface := range ifaces {
		// assoclist on a mesh point returns its peers, which would land in the
		// station telemetry and then in the client grid as wireless clients.
		if servesClients(modes, iface) {
			calls = append(calls, call{
				inv: ubus.Invocation{Object: "iwinfo", Method: "assoclist",
					Args: map[string]any{"device": iface}},
				decode:   decodeAssoclist(iface),
				optional: true,
			})
		}
		// The survey is asked of every interface regardless. Channel
		// utilization is a property of the radio's channel, not of what the
		// interface is for, and a radio carrying only a mesh point would
		// otherwise report no utilization at all.
		calls = append(calls, call{
			inv: ubus.Invocation{Object: "iwinfo", Method: "survey",
				Args: map[string]any{"device": iface}},
			decode:   decodeSurvey(iface),
			optional: true,
		})
	}
	return calls
}

// system.info is the one required call. If it fails the device is not usefully
// reachable, and nothing else in the snapshot would mean anything.
func decodeInfo(raw json.RawMessage, s *Snapshot) error {
	var v struct {
		Uptime int64   `json:"uptime"`
		Load   []int64 `json:"load"`
		Memory Memory  `json:"memory"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if len(v.Load) == 0 {
		// Present, well-formed JSON, and missing the one field the whole
		// device-health series is built from. Zeroes here would be recorded as
		// an idle device.
		return errors.New("system.info carried no load average")
	}
	s.Uptime = v.Uptime
	s.Memory = v.Memory
	for i := 0; i < len(v.Load) && i < 3; i++ {
		s.Load[i] = float64(v.Load[i]) / loadScale
	}
	return nil
}

func decodeBoard(raw json.RawMessage, s *Snapshot) error {
	var b Board
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	s.Board = &b
	return nil
}

func decodeNetDevices(raw json.RawMessage, s *Snapshot) error {
	var v map[string]Interface
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	s.Interfaces = v
	// Answered — recorded explicitly rather than inferred from the map.
	//
	// A nil Interfaces map usually does mean "never asked", because a
	// successful empty reply decodes to an empty non-nil map. Usually is not a
	// contract: a device answering `null` lands on nil too, and any later
	// caller that reads absence-from-the-map as a statement about the kernel
	// would then be making a positive claim from a call that said nothing. The
	// consumer that needs the distinction is "this interface is not up" versus
	// "we could not see whether it is up", which is the same denied-vs-absent
	// rule everything else here follows.
	s.NetDevsFresh = true
	return nil
}

func decodeAPStatus(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			SSID    string   `json:"ssid"`
			BSSID   string   `json:"bssid"`
			Channel int      `json:"channel"`
			Freq    int      `json:"freq"`
			Airtime *Airtime `json:"airtime"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		ap := s.ap(iface)
		ap.SSID, ap.BSSID, ap.Channel, ap.Freq = v.SSID, v.BSSID, v.Channel, v.Freq
		ap.Airtime = v.Airtime
		// Counted here, where an answer actually arrived. APsFresh is decided
		// from these rather than from having intended to ask.
		s.apStatusOK++
		return nil
	}
}

func decodeAPClients(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			Clients map[string]json.RawMessage `json:"clients"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		n := len(v.Clients)
		s.ap(iface).Clients = &n
		return nil
	}
}

func decodeAssoclist(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			Results []Station `json:"results"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		for i := range v.Results {
			v.Results[i].Iface = iface
		}
		s.Stations = append(s.Stations, v.Results...)
		return nil
	}
}

func decodeSurvey(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			Results []Survey `json:"results"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		for i := range v.Results {
			v.Results[i].Iface = iface
			s.Surveys = append(s.Surveys, v.Results[i])
		}
		return nil
	}
}

// decodeHostHints reads luci-rpc's ARP/neighbour/DHCP merge, keyed by MAC.
func decodeHostHints(raw json.RawMessage, s *Snapshot) error {
	var v map[string]struct {
		Name     string   `json:"name"`
		IPAddrs  []string `json:"ipaddrs"`
		IP6Addrs []string `json:"ip6addrs"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	for mac, h := range v {
		e := s.host(mac)
		e.IPv4, e.IPv6 = h.IPAddrs, h.IP6Addrs
		if h.Name != "" {
			e.Name = strings.TrimSuffix(h.Name, ".lan")
		}
	}
	return nil
}

// decodeNetworks reads netifd's interface dump into the subnets that decide
// whether a host is a client of this network or a neighbour on its uplink.
//
// Loopback is skipped: nothing in a host list is ever 127.x, and keeping it
// would let a bad address match something.
func decodeNetworks(raw json.RawMessage, s *Snapshot) error {
	var v struct {
		Interface []struct {
			Name string `json:"interface"`
			IPv4 []struct {
				Address string `json:"address"`
				Mask    int    `json:"mask"`
			} `json:"ipv4-address"`
			Route []struct {
				Target string `json:"target"`
				Mask   int    `json:"mask"`
			} `json:"route"`
		} `json:"interface"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	out := make([]Network, 0, len(v.Interface))
	for _, i := range v.Interface {
		upstream := false
		for _, r := range i.Route {
			// The default route, which is what actually makes an interface the
			// way out. Not the interface being called "wan".
			if r.Target == "0.0.0.0" && r.Mask == 0 {
				upstream = true
				break
			}
		}
		for _, a := range i.IPv4 {
			ip := net.ParseIP(a.Address)
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if a.Mask < 0 || a.Mask > 32 {
				continue
			}
			out = append(out, Network{
				Name:     i.Name,
				CIDR:     fmt.Sprintf("%s/%d", a.Address, a.Mask),
				Upstream: upstream,
			})
		}
	}
	s.Networks = out
	s.askedNetworks = true
	return nil
}

// decodeDHCPLeases adds the hostname the client asked to be called, which is
// often better than the reverse-DNS name in the hints.
func decodeDHCPLeases(raw json.RawMessage, s *Snapshot) error {
	var v struct {
		Leases []struct {
			MAC      string `json:"macaddr"`
			IP       string `json:"ipaddr"`
			Hostname string `json:"hostname"`
			Expires  int64  `json:"expires"`
		} `json:"dhcp_leases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	for _, l := range v.Leases {
		e := s.host(l.MAC)
		e.Lease = l.Expires
		if l.Hostname != "" {
			e.Name = l.Hostname
		}
		if l.IP != "" && !slices.Contains(e.IPv4, l.IP) {
			e.IPv4 = append(e.IPv4, l.IP)
		}
	}
	return nil
}

// host returns the entry for a MAC, creating it so the two sources can each
// fill in their half. MACs are normalised: the hints report them uppercase and
// the leases uppercase too, but nothing guarantees that stays true, and a client
// listed twice under two spellings is worse than one listed once.
func (s *Snapshot) host(mac string) *Host {
	mac = strings.ToLower(strings.TrimSpace(mac))
	for i := range s.Hosts {
		if s.Hosts[i].MAC == mac {
			return &s.Hosts[i]
		}
	}
	s.Hosts = append(s.Hosts, Host{MAC: mac})
	return &s.Hosts[len(s.Hosts)-1]
}

// ap returns the AP entry for an interface, creating it in place so the two
// hostapd calls can each fill in their half.
func (s *Snapshot) ap(iface string) *AP {
	for i := range s.APs {
		if s.APs[i].Iface == iface {
			return &s.APs[i]
		}
	}
	s.APs = append(s.APs, AP{Iface: iface})
	return &s.APs[len(s.APs)-1]
}

// ClientCount totals associated clients across APs, reporting whether the total
// is trustworthy.
//
// It is not trustworthy if any AP's count is missing: summing the ones that
// answered would draw a dip in the client-count graph that means "one radio did
// not reply", which is precisely the reading nobody would interpret correctly.
func (s *Snapshot) ClientCount() (int, bool) {
	total := 0
	for _, ap := range s.APs {
		if ap.Clients == nil {
			return 0, false
		}
		total += *ap.Clients
	}
	return total, len(s.APs) > 0
}

// decodeIfaces records the wireless interface list a poll discovered, for the
// next poll to use.
func decodeIfaces(raw json.RawMessage, s *Snapshot) error {
	var v struct {
		Devices []string `json:"devices"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	s.Ifaces = v.Devices
	s.IfacesFresh = true
	return nil
}

// decodeIfaceModes records what each wireless interface is configured as.
//
// # The decode is deliberately narrow
//
// getWirelessDevices returns each interface's whole UCI config — including
// `key`, the wireless passphrase, in plaintext. Nothing here needs it and
// nothing here should hold it, so the struct below names exactly two fields and
// the rest of the response is discarded by the JSON decoder rather than being
// carried around in a map[string]any that some later log line might print.
func decodeIfaceModes(raw json.RawMessage, s *Snapshot) error {
	var v map[string]struct {
		Interfaces []struct {
			IfName  string `json:"ifname"`
			Section string `json:"section"`
			Config  struct {
				Mode string `json:"mode"`
			} `json:"config"`
		} `json:"interfaces"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	modes := map[string]string{}
	sections := map[string]string{}
	var configuredButAbsent []string
	for _, radio := range v {
		for _, i := range radio.Interfaces {
			if i.Config.Mode == "" {
				continue
			}
			// A configured interface with NO ifname is the interesting one.
			//
			// It used to be dropped by the same condition that skipped junk,
			// and it is the exact signature of §5q: a section that applied
			// cleanly and whose interface the driver never brought into
			// existence. Discarding it is how "the mesh you configured does not
			// exist" became indistinguishable from "you configured no mesh".
			if i.IfName == "" {
				if i.Section != "" {
					configuredButAbsent = append(configuredButAbsent, i.Section)
				}
				continue
			}
			modes[i.IfName] = i.Config.Mode
			// Optional, and treated as such. The captured fixture carries
			// `section` on some entries and not others, so an interface without
			// one is attributed to the device rather than guessed at by mesh id
			// — the site model permits one mesh id on two bands, so a guess
			// would be wrong precisely where a mesh is most interesting.
			if i.Section != "" {
				sections[i.IfName] = i.Section
			}
		}
	}
	sort.Strings(configuredButAbsent)
	s.IfaceModes = modes
	s.IfaceSections = sections
	s.ConfiguredIfacesAbsent = configuredButAbsent
	return nil
}

// servesClients reports whether an interface is one whose associated stations
// are CLIENTS.
//
// Unknown means yes, which is the behaviour that existed before modes were
// read at all. Answering "no" for an interface whose mode was never read would
// let a denied call quietly stop counting real clients — the failure would be a
// number that is too low, with nothing anywhere saying so.
func servesClients(modes map[string]string, iface string) bool {
	m, known := modes[iface]
	if !known {
		return true
	}
	return m == "ap"
}

// discoverIfaces lists the wireless interfaces in a single call.
//
// Only adoption and the integration tests use it — the poll loop gets the same
// answer inside its batch. Kept because "what radios does this device have" is
// a reasonable one-off question.
func (p *poller) discoverIfaces(ctx context.Context, c *ubus.Client) ([]string, error) {
	var v struct {
		Devices []string `json:"devices"`
	}
	if err := c.Call(ctx, "iwinfo", "devices", nil, &v); err != nil {
		return nil, err
	}
	return v.Devices, nil
}

// discoverIfaceModes reads each wireless interface's configured mode in one
// call.
//
// Like discoverIfaces, the poll loop gets the same answer inside its batch;
// this exists for adoption and the integration tests, where "what is each of
// these interfaces for" is a reasonable one-off question.
func (p *poller) discoverIfaceModes(ctx context.Context, c *ubus.Client) (map[string]string, error) {
	return IfaceModes(ctx, c)
}

// IfaceModes reads each wireless interface's configured mode over an existing
// session.
//
// Exported because "did the mesh point actually come up" is a question worth
// asking from outside this package — a config that uci accepted and hostapd
// then refused looks identical in the config and completely different here.
func IfaceModes(ctx context.Context, c *ubus.Client) (map[string]string, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "luci-rpc", "getWirelessDevices", nil, &raw); err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := decodeIfaceModes(raw, &snap); err != nil {
		return nil, err
	}
	return snap.IfaceModes, nil
}

// needIfaces reports whether this poll should re-read the radio list.
func (p *poller) needIfaces() bool {
	if p.ifaceAt.IsZero() || p.c.now().Sub(p.ifaceAt) >= rediscoverInterval {
		return true
	}
	// A scheduled second look after an apply. Without it, the re-read triggered
	// by an apply lands in the seconds before a new interface exists and caches
	// its absence for the whole cadence.
	return !p.ifaceRefetchAt.IsZero() && !p.c.now().Before(p.ifaceRefetchAt)
}

// needMeshPeers reports whether this poll should re-read mesh peer lists.
//
// Its own timer rather than needIfaces()'s, because it consumes what that fetch
// produces and would otherwise never fire — see the call site.
func (p *poller) needMeshPeers() bool {
	return p.meshAt.IsZero() || p.c.now().Sub(p.meshAt) >= rediscoverInterval
}

// needNetworks reports whether this poll should re-read the interface subnets.
func (p *poller) needNetworks() bool {
	return p.netAt.IsZero() || p.c.now().Sub(p.netAt) >= rediscoverInterval
}

// needBoard reports whether this poll should re-read the firmware identity.
//
// Rarely, but not never: the board is static until somebody flashes the device,
// and re-reading is the only way that gets noticed.
func (p *poller) needBoard() bool {
	return p.boardAt.IsZero() || p.c.now().Sub(p.boardAt) >= rediscoverInterval
}

// rediscoverInterval governs the two facts that change only when a human
// changes them: the firmware identity and the list of radios. Asking for either
// every minute would add calls to every poll for an answer that is almost always
// the one we already have.
const rediscoverInterval = 15 * time.Minute

// decodeMeshPeers reads one mesh interface's peer list.
//
// Records that the question was ASKED and ANSWERED separately from what the
// answer was. Zero peers and a refused exec are different facts, and the state
// ladder has a different rung for each — collapsing them here would undo that
// two layers down, where nothing could tell them apart any more.
func decodeMeshPeers(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			Code   int    `json:"code"`
			Stdout string `json:"stdout"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if s.MeshPeers == nil {
			s.MeshPeers = map[string][]meshlink.Peer{}
		}
		if v.Code != 0 {
			// The command ran and failed — an interface that went away between
			// the list and the call, most likely. Not an answer about peers.
			return nil
		}
		// A successful exec with no stations is a real zero, and the empty
		// slice is what says so: nil would read as "never asked".
		peers := meshlink.ParseStationDump(v.Stdout)
		if peers == nil {
			peers = []meshlink.Peer{}
		}
		s.MeshPeers[iface] = peers
		return nil
	}
}
