package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
func (p *poller) poll(ctx context.Context, c *ubus.Client, tier Tier, ifaces []string) Snapshot {
	snap := Snapshot{
		DeviceID: p.target.DeviceID,
		MAC:      p.target.MAC,
		Name:     p.target.Name,
		Tier:     tier,
		At:       p.c.now(),
	}
	calls := p.buildCalls(tier, ifaces)
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
	return snap
}

// buildCalls assembles the request set for a tier.
//
// The split between tiers is the budget. Baseline reads only what is cheap and
// what needs unbroken history; everything driver-expensive waits until somebody
// is actually looking. Measured: iwinfo is ~92% of a focused poll (194 ms vs
// 15.8 ms without it), and hostapd answers the per-AP questions ~30× faster.
func (p *poller) buildCalls(tier Tier, ifaces []string) []call {
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
	}
	if p.needBoard() {
		calls = append(calls, call{
			inv:      ubus.Invocation{Object: "system", Method: "board"},
			decode:   decodeBoard,
			optional: true,
		})
	}
	for _, iface := range ifaces {
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
		calls = append(calls,
			call{
				inv: ubus.Invocation{Object: "iwinfo", Method: "assoclist",
					Args: map[string]any{"device": iface}},
				decode:   decodeAssoclist(iface),
				optional: true,
			},
			call{
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

// needIfaces reports whether this poll should re-read the radio list.
func (p *poller) needIfaces() bool {
	return p.ifaceAt.IsZero() || p.c.now().Sub(p.ifaceAt) >= rediscoverInterval
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
