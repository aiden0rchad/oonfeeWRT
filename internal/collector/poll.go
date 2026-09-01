package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/meshlink"
	"github.com/aiden0rchad/oonfeewrt/internal/observability"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// loadScale is the fixed-point divisor system.info uses for load averages.
const loadScale = 65536.0

const (
	clockMethodUnix      = "getUnixtime"
	clockMethodLocal     = "getLocaltime"
	maxExactClockInteger = int64(1<<53 - 1)
)

// call is one invocation plus what to do with its result. Keeping the two
// together is what lets the whole poll be assembled, sent as a single batch,
// and decoded positionally without an index-to-meaning table to get wrong.
type call struct {
	inv    ubus.Invocation
	decode func(json.RawMessage, *Snapshot) error
	// adaptiveWait is deterministic elapsed time deliberately spent inside the
	// call. It remains part of Snapshot.Duration for diagnostics, but is not
	// evidence that the router itself needs a slower polling interval.
	adaptiveWait time.Duration
	// radioInventory marks the optional getWirelessDevices source. Its outcome
	// is tracked independently from system.info: a healthy device poll does not
	// make a denied radio refresh current.
	radioInventory bool
	// radioKey marks a freqlist attempt for one stable UCI radio. The poll uses
	// it to preserve a failed/denied answer as unknown rather than stale data.
	radioKey string
	// topologySource groups one or more calls into one explicit source state.
	topologySource string
	// assocIface identifies one focused assoclist question. Asked and answered
	// are tracked separately so a partial multi-BSS radio never becomes a
	// whole-radio aggregate.
	assocIface string
	// clockMethod marks the version-dependent LuCI UTC clock source. It is kept
	// separate from system.info.localtime, which is timezone-adjusted wall time.
	clockMethod string

	// optional marks a call whose failure degrades the snapshot rather than
	// meaning the device is unreachable.
	optional bool
}

// setBusyDuration removes only waits whose result decoded successfully from the
// duration used by adaptive backoff. The total Snapshot.Duration is unchanged.
func (snap *Snapshot) setBusyDuration(completedWait time.Duration) {
	busy := snap.Duration - completedWait
	if busy < 0 {
		busy = 0
	}
	snap.busyDuration = busy
	snap.busyDurationKnown = true
}

func degradationCause(err error, status ubus.Status) DegradationCause {
	switch status {
	case ubus.StatusPermissionDenied:
		return CausePermission
	case ubus.StatusMethodNotFound, ubus.StatusNotFound, ubus.StatusNoData,
		ubus.StatusNotSupported:
		return CauseUnsupported
	case ubus.StatusTimeout, ubus.StatusConnectionFailed:
		return CauseTransport
	case ubus.StatusUnknownError:
		return CauseDevice
	case ubus.StatusInvalidCommand, ubus.StatusInvalidArgument:
		return CauseProtocol
	}
	var denied *ubus.DeniedError
	if errors.As(err, &denied) {
		return CausePermission
	}
	var protocol *ubus.ProtocolError
	if errors.As(err, &protocol) {
		return CauseProtocol
	}
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return CauseTransport
	}
	return CauseUnknown
}

// poll performs one complete poll in a single HTTP round trip.
//
// One request, not one per metric. Measured on class A: batching is flat at
// ~0.5 ms per call from ten calls up, and a 60 s baseline poll never reuses its
// connection anyway (uhttpd's keep-alive is 20 s), so every call it does not
// batch costs another handshake.
func (p *poller) poll(ctx context.Context, c *ubus.Client, target Target, tier Tier,
	ifaces []string, modes map[string]string) Snapshot {
	snap := Snapshot{
		DeviceID:     target.DeviceID,
		MAC:          target.MAC,
		Name:         target.Name,
		Tier:         tier,
		At:           p.c.now(),
		AirtimeSplit: target.AirtimeSplit,
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
	for _, spec := range calls {
		if spec.radioInventory {
			snap.radioInventoryAsked = true
		}
		if spec.radioKey == "" {
			if spec.assocIface != "" {
				if snap.AssocAsked == nil {
					snap.AssocAsked = map[string]bool{}
					snap.AssocAnswered = map[string]bool{}
				}
				snap.AssocAsked[spec.assocIface] = true
			}
			continue
		}
		if snap.radioFrequencyAsked == nil {
			snap.radioFrequencyAsked = map[string]bool{}
		}
		snap.radioFrequencyAsked[spec.radioKey] = true
	}
	snap.prepareTopology(calls)
	invs := make([]ubus.Invocation, len(calls))
	for i, c := range calls {
		invs[i] = c.inv
	}

	start := p.c.now()
	results, err := c.Batch(ctx, invs)
	end := p.c.now()
	snap.Duration = end.Sub(start)
	if !end.Before(start) {
		snap.RouterClockAt = start.Add(end.Sub(start) / 2)
	}
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
	var completedWait time.Duration

	for i, res := range results {
		spec := calls[i]
		if spec.clockMethod != "" {
			snap.clockAttempted = true
		}
		if res.Err != nil {
			// getUnixtime replaced getLocaltime on newer LuCI. A missing new
			// method is compatibility negotiation, not a standing degradation;
			// the next full poll tries the legacy method. No other failure,
			// especially an ACL denial, is allowed to trigger the fallback.
			if shouldFallbackClock(spec.clockMethod, res.Status) {
				snap.clockMethodMissing = spec.clockMethod
				continue
			}
			if clockSourceUnavailable(spec.clockMethod, res.Status) {
				snap.clockUnavailable = true
				continue
			}
			cause := degradationCause(res.Err, res.Status)
			snap.topologyFailed(spec.topologySource, cause)
			d := Degradation{
				Object: spec.inv.Object, Method: spec.inv.Method,
				Target: degradationTarget(spec.inv),
				Status: res.Status, Cause: cause,
				Err:       res.Err.Error(),
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
			snap.topologyFailed(spec.topologySource, CauseDecode)
			d := Degradation{
				Object: spec.inv.Object, Method: spec.inv.Method,
				Target: degradationTarget(spec.inv),
				Cause:  CauseDecode, Err: fmt.Sprintf("decode: %v", err),
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
		} else {
			if spec.clockMethod != "" {
				snap.clockMethod = spec.clockMethod
				snap.clockAttemptOK = snap.RouterUnixTime != nil && !snap.RouterClockAt.IsZero()
			}
			completedWait += spec.adaptiveWait
			snap.topologyAnswered(spec.topologySource)
			if spec.assocIface != "" {
				snap.AssocAnswered[spec.assocIface] = true
			}
		}
	}
	snap.setBusyDuration(completedWait)
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
	snap.LogsFresh = snap.logReadOK && snap.logBootOK && snap.logPIDOK
	if err := snap.finalizeNetworks(); err != nil {
		if snap.Topology.Cycle {
			snap.topologyFailed(topology.SourceDefaultRoute, CauseDecode)
		}
		snap.Degraded = append(snap.Degraded, Degradation{
			Object: "network.interface", Method: "dump", Target: "main IPv4 route mapping",
			Cause: CauseDecode, Err: fmt.Sprintf("decode: %v", err),
		})
	}
	snap.Topology.Sources = append(snap.Topology.Sources, topologyAssociationSource(&snap))
	snap.finalizeTopology()
	return snap
}

// buildAuxCalls assembles the one-minute lightweight request. WAN and router
// logs share the same batch when both are due; neither repeats full-poll work.
func (p *poller) buildAuxCalls() (calls []call, wan, logs bool) {
	if p.takeWANProbe() {
		calls = append(calls, wanProbeCall())
		wan = true
	}
	if p.takeLogs() {
		calls = append(calls, logCalls()...)
		logs = true
	}
	return calls, wan, logs
}

func (p *poller) pollAux(ctx context.Context, c *ubus.Client, target Target,
	calls []call, wan, logs bool) Snapshot {
	snap := Snapshot{DeviceID: target.DeviceID, MAC: target.MAC, Name: target.Name,
		Tier: Baseline, At: p.c.now(), WANOnly: wan, LogOnly: logs}
	invs := make([]ubus.Invocation, len(calls))
	for i := range calls {
		invs[i] = calls[i].inv
	}
	start := p.c.now()
	results, err := c.Batch(ctx, invs)
	snap.Duration = p.c.now().Sub(start)
	if err != nil {
		snap.Err = err
		return snap
	}
	if len(results) != len(calls) {
		snap.Err = fmt.Errorf("collector: auxiliary poll returned %d results for %d calls",
			len(results), len(calls))
		return snap
	}
	for i, result := range results {
		spec := calls[i]
		if result.Err != nil {
			snap.Degraded = append(snap.Degraded, Degradation{
				Object: spec.inv.Object, Method: spec.inv.Method, Status: result.Status,
				Target: degradationTarget(spec.inv),
				Cause:  degradationCause(result.Err, result.Status), Err: result.Err.Error(),
				Permanent: ubus.IsPermanent(result.Err),
			})
			continue
		}
		if err := spec.decode(result.Data, &snap); err != nil {
			snap.Degraded = append(snap.Degraded, Degradation{
				Object: spec.inv.Object, Method: spec.inv.Method,
				Target: degradationTarget(spec.inv),
				Cause:  CauseDecode, Err: fmt.Sprintf("decode: %v", err),
			})
		}
	}
	snap.LogsFresh = snap.logReadOK && snap.logBootOK && snap.logPIDOK
	return snap
}

// buildCalls assembles the request set for a tier.
//
// The split between tiers is the budget. Baseline reads only what is cheap and
// what needs unbroken history; everything driver-expensive waits until somebody
// is actually looking. Measured: iwinfo is ~92% of a focused poll (194 ms vs
// 15.8 ms without it), and hostapd answers the per-AP questions ~30× faster.
func (p *poller) buildCalls(tier Tier, ifaces []string, modes map[string]string) []call {
	needTopology := p.needTopology()
	// Interface inventory is a topology prerequisite, not merely a radio-cache
	// refresh. A successful empty iwinfo list on the same cycle is the positive
	// runtime evidence that lets a wired-only device distinguish "no radios"
	// from an unavailable getWirelessDevices method.
	needIfaces := p.needIfaces() || needTopology
	clockMethod := p.clockSourceMethod()
	calls := []call{
		{inv: ubus.Invocation{Object: "system", Method: "info"}, decode: decodeInfo},
	}
	if clockMethod != "" {
		calls = append(calls, call{
			inv:         ubus.Invocation{Object: "luci", Method: clockMethod},
			decode:      decodeRouterUnixTime,
			optional:    true,
			clockMethod: clockMethod,
		})
	}
	calls = append(calls, call{
		inv:      ubus.Invocation{Object: "network.device", Method: "status"},
		decode:   decodeNetDevices,
		optional: true,
	})
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
	if p.takeWANProbe() {
		calls = append(calls, wanProbeCall())
	}
	if needIfaces {
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
	if needIfaces || needTopology {
		// One selective decoder supplies configured modes plus stable radio and
		// topology identities without retaining the plaintext key in this reply.
		calls = append(calls, call{
			inv:            ubus.Invocation{Object: "luci-rpc", Method: "getWirelessDevices"},
			decode:         decodeIfaceModes,
			optional:       true,
			radioInventory: true,
			topologySource: func() string {
				if needTopology {
					return TopologySourceWirelessDevices
				}
				return ""
			}(),
		})
	}
	// One read-only frequency-list call per stable UCI radio, on the same slow
	// cadence as inventory. Multiple BSSes share one radio and therefore never
	// multiply this work. AP interfaces are preferred because they are the
	// serving runtime identity; a mesh/STA interface is a fallback only.
	for _, target := range p.radioFrequencyTargets() {
		calls = append(calls, call{
			inv: ubus.Invocation{Object: "iwinfo", Method: "freqlist",
				Args: map[string]any{"device": target.iface}},
			decode: decodeRadioFrequencies(target.key), optional: true,
			radioKey: target.key,
		})
	}
	if needTopology {
		calls = append(calls,
			call{inv: ubus.Invocation{Object: "luci-rpc", Method: "getNetworkDevices"},
				decode: decodeTopologyNetworkDevices, optional: true,
				topologySource: TopologySourceNetworkDevices},
			call{inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
				"command": "/sbin/ip", "params": []string{"-4", "neigh", "show"},
			}}, decode: decodeTopologyNeighbors(4), optional: true,
				topologySource: topology.SourceNeighbors(4)},
			call{inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
				"command": "/sbin/ip", "params": []string{"-6", "neigh", "show"},
			}}, decode: decodeTopologyNeighbors(6), optional: true,
				topologySource: topology.SourceNeighbors(6)},
			call{inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
				"command": "/usr/sbin/lldpcli", "params": []string{"-f", "json", "show", "neighbors", "hidden"},
			}}, decode: decodeTopologyLLDP, optional: true,
				topologySource: topology.SourceLLDP},
		)
		for _, bridge := range p.topologyBridgeList() {
			calls = append(calls,
				call{inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
					"command": "/usr/sbin/brctl", "params": []string{"showmacs", bridge},
				}}, decode: decodeTopologyFDB(bridge), optional: true,
					topologySource: topology.SourceBridgeFDB},
				call{inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
					"command": "/usr/sbin/brctl", "params": []string{"showstp", bridge},
				}}, decode: decodeTopologySTP(bridge), optional: true,
					topologySource: TopologySourceBridgeSTP},
			)
		}
		p.mu.Lock()
		p.topologyAt = p.c.now()
		p.mu.Unlock()
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
	needNetworks := p.needNetworks()
	if needNetworks || needTopology {
		// netifd supplies logical interfaces and subnets, but it can retain a
		// default-route candidate that the kernel did not install. Pair it with
		// the already allow-listed kernel table so PPPoE and management networks
		// resolve to the device that actually carries the route.
		source := ""
		if needTopology {
			source = topology.SourceDefaultRoute
		}
		calls = append(calls,
			call{inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
				"command": "/sbin/ip", "params": []string{"-4", "route", "show", "table", "all"},
			}}, decode: decodeMainIPv4Route, optional: true, topologySource: source},
			call{inv: ubus.Invocation{Object: "network.interface", Method: "dump"},
				decode: decodeNetworks, optional: true, topologySource: source},
		)
		// Stamped on the ATTEMPT, not on the answer. A device whose ACL does not
		// grant network.interface would otherwise never set the timestamp and so
		// would re-request the call on every single poll, forever, for an answer
		// it is never going to give. The decoder separately marks a successful
		// read, which is what decides whether the cached subnets are replaced.
		if needNetworks {
			p.mu.Lock()
			p.netAt = p.c.now()
			p.mu.Unlock()
		}
	}
	if p.needBoard() {
		calls = append(calls, call{
			inv:      ubus.Invocation{Object: "system", Method: "board"},
			decode:   decodeBoard,
			optional: true,
		})
	}
	if p.takeLogs() {
		calls = append(calls, logCalls()...)
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
		// assoclist on mesh/STA interfaces returns infrastructure peers. Unlike
		// hostapd probing above, a mode-less iwinfo call cannot distinguish those
		// peers from downstream clients, so focused station telemetry requires an
		// explicit AP classification.
		if modes[iface] == "ap" {
			calls = append(calls, call{
				inv: ubus.Invocation{Object: "iwinfo", Method: "assoclist",
					Args: map[string]any{"device": iface}},
				decode:     decodeAssoclist(iface),
				optional:   true,
				assocIface: iface,
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

func shouldFallbackClock(method string, status ubus.Status) bool {
	return method == clockMethodUnix && status == ubus.StatusMethodNotFound
}

func clockSourceUnavailable(method string, status ubus.Status) bool {
	return method == clockMethodUnix && status == ubus.StatusNotFound ||
		method == clockMethodLocal &&
			(status == ubus.StatusMethodNotFound || status == ubus.StatusNotFound)
}

func (p *poller) clockSourceMethod() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clockUnavailable {
		return ""
	}
	if p.clockMethod != "" {
		return p.clockMethod
	}
	if p.clockFallback {
		return clockMethodLocal
	}
	return clockMethodUnix
}

type radioFrequencyTarget struct {
	key   string
	iface string
}

func (p *poller) radioFrequencyTargets() []radioFrequencyTarget {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.radiosKnown || (!p.radioAt.IsZero() &&
		p.c.now().Sub(p.radioAt) < rediscoverInterval) {
		return nil
	}
	targets := make([]radioFrequencyTarget, 0, len(p.radios))
	for _, state := range p.radios {
		if iface := preferredRadioInterface(state.Interfaces); iface != "" {
			targets = append(targets, radioFrequencyTarget{key: state.Key, iface: iface})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].key < targets[j].key })
	if len(targets) > 0 {
		// Stamp the attempt, not the answer. A denied optional call must not run
		// on every baseline poll forever.
		p.radioAt = p.c.now()
	}
	return targets
}

func preferredRadioInterface(interfaces []radio.Interface) string {
	var ap, fallback string
	for _, iface := range interfaces {
		if iface.Name == "" {
			continue
		}
		if fallback == "" || iface.Name < fallback {
			fallback = iface.Name
		}
		if iface.Mode == "ap" && (ap == "" || iface.Name < ap) {
			ap = iface.Name
		}
	}
	if ap != "" {
		return ap
	}
	return fallback
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

func decodeRouterUnixTime(raw json.RawMessage, s *Snapshot) error {
	var v struct {
		Result *int64 `json:"result"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Result == nil {
		return errors.New("luci clock response carried no Unix time")
	}
	if *v.Result < 0 || *v.Result > maxExactClockInteger {
		return fmt.Errorf("luci clock response Unix time %d is outside the supported range", *v.Result)
	}
	s.RouterUnixTime = v.Result
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
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	v := make(map[string]Interface, len(rows))
	for name, row := range rows {
		var iface Interface
		if err := json.Unmarshal(row, &iface); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		var present struct {
			Stats *struct {
				RxBytes   *int64 `json:"rx_bytes"`
				TxBytes   *int64 `json:"tx_bytes"`
				RxPackets *int64 `json:"rx_packets"`
				TxPackets *int64 `json:"tx_packets"`
				RxErrors  *int64 `json:"rx_errors"`
				TxErrors  *int64 `json:"tx_errors"`
			} `json:"statistics"`
		}
		if err := json.Unmarshal(row, &present); err != nil {
			return fmt.Errorf("%s presence: %w", name, err)
		}
		if present.Stats != nil {
			iface.Stats.RxBytesKnown = present.Stats.RxBytes != nil
			iface.Stats.TxBytesKnown = present.Stats.TxBytes != nil
			iface.Stats.RxPacketsKnown = present.Stats.RxPackets != nil
			iface.Stats.TxPacketsKnown = present.Stats.TxPackets != nil
			iface.Stats.RxErrorsKnown = present.Stats.RxErrors != nil
			iface.Stats.TxErrorsKnown = present.Stats.TxErrors != nil
		}
		if (iface.Stats.RxBytesKnown && iface.Stats.RxBytes < 0) ||
			(iface.Stats.TxBytesKnown && iface.Stats.TxBytes < 0) {
			return fmt.Errorf("%s: negative byte counter", name)
		}
		v[name] = iface
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
			Clients map[string]struct {
				Signal *int `json:"signal"`
			} `json:"clients"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		n := len(v.Clients)
		ap := s.ap(iface)
		ap.Clients = &n
		// The MACs, not just how many there are. This call already carries
		// them and their RSSI; keeping only len() threw away the answer to
		// "which AP is this client on" that the grid then reported as unknown.
		//
		// Lower-cased on the way in. Measured on the reference WRT3200ACM:
		// hostapd.get_clients can return lower case while iwinfo.assoclist returns
		// upper case for the same station in the same minute,
		// and the clients table stores lower case. A join that did not
		// normalise would miss every row and look like an empty result.
		ap.Stations = make(map[string]LiveStation, len(v.Clients))
		for mac, c := range v.Clients {
			ap.Stations[strings.ToLower(mac)] = LiveStation{Iface: iface, Signal: c.Signal}
		}
		return nil
	}
}

func decodeAssoclist(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		decoded := make([]Station, 0, len(v.Results))
		for _, item := range v.Results {
			var st Station
			if err := json.Unmarshal(item, &st); err != nil {
				return err
			}
			var present struct {
				Signal    *int `json:"signal"`
				SignalAvg *int `json:"signal_avg"`
				RX        *struct {
					Bytes   *int64 `json:"bytes"`
					Packets *int64 `json:"packets"`
					Rate    *int64 `json:"rate"`
				} `json:"rx"`
				TX *struct {
					Bytes   *int64 `json:"bytes"`
					Packets *int64 `json:"packets"`
					Rate    *int64 `json:"rate"`
					Retries *int64 `json:"retries"`
					Failed  *int64 `json:"failed"`
				} `json:"tx"`
			}
			if err := json.Unmarshal(item, &present); err != nil {
				return err
			}
			st.Iface = iface
			st.PresenceKnown = true
			st.SignalKnown = present.Signal != nil
			st.SignalAvgKnown = present.SignalAvg != nil
			if present.RX != nil {
				st.RX.BytesKnown = present.RX.Bytes != nil
				st.RX.PacketsKnown = present.RX.Packets != nil
				st.RX.RateKnown = present.RX.Rate != nil
			}
			if present.TX != nil {
				st.TX.BytesKnown = present.TX.Bytes != nil
				st.TX.PacketsKnown = present.TX.Packets != nil
				st.TX.RateKnown = present.TX.Rate != nil
				st.TX.RetriesKnown = present.TX.Retries != nil
				st.TX.FailedKnown = present.TX.Failed != nil
			}
			st.TXQualityKnown = present.TX != nil && present.TX.Packets != nil &&
				present.TX.Retries != nil && present.TX.Failed != nil
			if (st.RX.BytesKnown && st.RX.Bytes < 0) ||
				(st.TX.BytesKnown && st.TX.Bytes < 0) ||
				(st.RX.RateKnown && st.RX.Rate < 0) ||
				(st.TX.RateKnown && st.TX.Rate < 0) {
				return errors.New("assoclist carried a negative byte counter or rate")
			}
			decoded = append(decoded, st)
		}
		s.Stations = append(s.Stations, decoded...)
		return nil
	}
}

func decodeSurvey(iface string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		var v struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		decoded := make([]Survey, 0, len(v.Results))
		for _, item := range v.Results {
			var survey Survey
			if err := json.Unmarshal(item, &survey); err != nil {
				return err
			}
			var present struct {
				MHz        *int    `json:"mhz"`
				Noise      *int    `json:"noise"`
				ActiveTime *int64  `json:"active_time"`
				BusyTime   *int64  `json:"busy_time"`
				RxTime     *uint64 `json:"rx_time"`
				TxTime     *uint64 `json:"tx_time"`
			}
			if err := json.Unmarshal(item, &present); err != nil {
				return err
			}
			survey.Iface = iface
			survey.PresenceKnown = true
			survey.MHzKnown = present.MHz != nil
			survey.NoiseKnown = present.Noise != nil
			survey.ActiveTimeKnown = present.ActiveTime != nil
			survey.BusyTimeKnown = present.BusyTime != nil
			survey.RxTimeKnown = present.RxTime != nil
			survey.TxTimeKnown = present.TxTime != nil
			if !survey.MHzKnown || !survey.ActiveTimeKnown || !survey.BusyTimeKnown {
				return errors.New("survey row omitted mhz, active_time, or busy_time")
			}
			if survey.MHz <= 0 || survey.ActiveTime < 0 || survey.BusyTime < 0 {
				return errors.New("survey row carried an invalid frequency or counter")
			}
			decoded = append(decoded, survey)
		}
		s.Surveys = append(s.Surveys, decoded...)
		return nil
	}
}

func decodeLogRows(raw json.RawMessage, s *Snapshot) error {
	rows, err := observability.DecodeLogRead(raw)
	if err != nil {
		return err
	}
	s.Logs, s.logReadOK = rows, true
	return nil
}

func decodeLogBootID(raw json.RawMessage, s *Snapshot) error {
	var response struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return err
	}
	bootID := strings.ToLower(strings.TrimSpace(response.Data))
	if !validBootID(bootID) {
		return errors.New("invalid kernel boot ID")
	}
	s.LogEpoch.BootID, s.logBootOK = bootID, true
	return nil
}

func decodeLogService(raw json.RawMessage, s *Snapshot) error {
	var services map[string]struct {
		Instances map[string]struct {
			Running bool  `json:"running"`
			PID     int64 `json:"pid"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(raw, &services); err != nil {
		return err
	}
	var pids []int64
	for _, instance := range services["log"].Instances {
		if instance.Running && instance.PID > 0 {
			pids = append(pids, instance.PID)
		}
	}
	if len(pids) != 1 {
		return fmt.Errorf("logd has %d running instances with a positive pid", len(pids))
	}
	s.LogEpoch.PID, s.logPIDOK = pids[0], true
	return nil
}

func validBootID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
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
type networkCandidate struct {
	name, device, l3Device string
	up, defaultIPv4        bool
	addresses              []struct {
		address string
		mask    int
	}
}

func decodeMainIPv4Route(raw json.RawMessage, s *Snapshot) error {
	exec, err := topology.DecodeExecOutput(raw)
	if err != nil {
		return err
	}
	device, found, err := topology.ParseIPv4MainDefaultRoute(exec.Stdout)
	if err != nil {
		return err
	}
	s.mainIPv4RouteKnown = true
	if found {
		s.mainIPv4Device = device
	}
	return nil
}

func decodeNetworks(raw json.RawMessage, s *Snapshot) error {
	var v struct {
		Interface []struct {
			Name     string `json:"interface"`
			Up       bool   `json:"up"`
			Device   string `json:"device"`
			L3Device string `json:"l3_device"`
			IPv4     []struct {
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
	candidates := make([]networkCandidate, 0, len(v.Interface))
	for _, i := range v.Interface {
		candidate := networkCandidate{name: i.Name, device: i.Device, l3Device: i.L3Device, up: i.Up}
		for _, route := range i.Route {
			if route.Target == "0.0.0.0" && route.Mask == 0 {
				candidate.defaultIPv4 = true
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
			candidate.addresses = append(candidate.addresses, struct {
				address string
				mask    int
			}{address: a.Address, mask: a.Mask})
		}
		candidates = append(candidates, candidate)
	}
	s.networkCandidates = candidates
	s.networkDumpKnown = true
	return nil
}

func (s *Snapshot) finalizeNetworks() error {
	if !s.networkDumpKnown || !s.mainIPv4RouteKnown {
		return nil
	}
	logical := ""
	if s.mainIPv4Device != "" {
		for _, match := range []func(networkCandidate) bool{
			func(candidate networkCandidate) bool { return candidate.l3Device == s.mainIPv4Device },
			func(candidate networkCandidate) bool {
				return candidate.l3Device == "" && candidate.device == s.mainIPv4Device
			},
			func(candidate networkCandidate) bool {
				return candidate.l3Device == "" && candidate.device == "" && candidate.name == s.mainIPv4Device
			},
		} {
			matches := []string{}
			for _, candidate := range s.networkCandidates {
				if candidate.up && candidate.defaultIPv4 && match(candidate) {
					matches = append(matches, candidate.name)
				}
			}
			if len(matches) > 1 {
				return fmt.Errorf("kernel route device %q maps to several logical interfaces: %s",
					s.mainIPv4Device, strings.Join(matches, ", "))
			}
			if len(matches) == 1 {
				logical = matches[0]
				break
			}
		}
		if logical == "" {
			return fmt.Errorf("kernel route device %q has no active logical interface", s.mainIPv4Device)
		}
	}

	networks := []Network{}
	for _, candidate := range s.networkCandidates {
		for _, address := range candidate.addresses {
			networks = append(networks, Network{Name: candidate.name,
				CIDR:     fmt.Sprintf("%s/%d", address.address, address.mask),
				Upstream: candidate.name == logical && logical != ""})
		}
	}
	uplinks := []topology.Uplink{}
	if s.mainIPv4Device != "" {
		uplinks = append(uplinks, topology.Uplink{DeviceID: s.DeviceID,
			Interface: s.mainIPv4Device, LogicalInterface: logical, Active: true})
	}
	s.Networks = networks
	s.Topology.Uplinks = uplinks
	s.askedNetworks = true
	s.topologyEvidence(topology.SourceDefaultRoute, len(uplinks))
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
// Two ways it is not, and they need saying together because standing on either
// one alone gets the other backwards.
//
// It is untrustworthy if any AP's count is missing: summing the ones that
// answered would draw a dip in the client-count graph that means "one radio did
// not reply", which is precisely the reading nobody would interpret correctly.
//
// It is equally untrustworthy if the AP LIST itself is incomplete, which is a
// different failure and invisible from the entries — a refused get_status
// leaves no entry at all, so the radio that did not answer is simply absent and
// what remains looks whole. APsFresh is the flag that knows, and it also says
// LiveStations is every associated client the last poll saw, across all of a
// device's APs, keyed by lower-case MAC.
//
// Gated on APsFresh for the same reason ClientCount is: a stale or refused read
// must report "we do not know", not "nobody is connected". An AP whose station
// map is nil failed its get_clients call, which is also not an empty AP.
func (s *Snapshot) LiveStations() (LiveStationSet, bool) {
	if !s.APsFresh {
		return nil, false
	}
	out := LiveStationSet{}
	for _, ap := range s.APs {
		if ap.Stations == nil {
			return nil, false
		}
		for mac, st := range ap.Stations {
			mac = strings.ToLower(mac)
			out[mac] = append(out[mac], st)
		}
	}
	for mac := range out {
		sort.Slice(out[mac], func(i, j int) bool { return out[mac][i].Iface < out[mac][j].Iface })
	}
	return out, true
}

// yes for a device with no AP interfaces at all: zero clients, known.
func (s *Snapshot) ClientCount() (int, bool) {
	// Gated on APsFresh, which is the only thing that knows whether the AP list
	// is an answer. `len(s.APs) > 0` was standing in for it and got the two
	// interesting cases backwards, in opposite directions.
	//
	// A device with NO AP interfaces — radios off, a switch, an AP whose WLAN
	// has not been applied yet — has zero wireless clients, and that is a fact.
	// Reported as unknown, it suppressed the dashboard's whole fleet total and
	// named the device as one that "did not report a client count", which it
	// had: it reported that it has none.
	//
	// Worse in the other direction: a device where SOME hostapd get_status
	// calls failed has entries for only the radios that answered. Summing those
	// and calling the result known is precisely the dip the dashboard's own
	// message says it refuses to draw — "adding up the rest would show a dip
	// that looks like clients leaving" — arrived at inside the function that
	// message trusts.
	//
	// APsFresh is true only when every AP interface we know about answered, and
	// true with an empty list when there was nothing to ask. Both cases right,
	// from the flag built for the question.
	if !s.APsFresh {
		return 0, false
	}
	total := 0
	for _, ap := range s.APs {
		// get_status answered and get_clients did not: the radio is there and
		// its population is not known. Still unknown, and not zero.
		if ap.Clients == nil {
			return 0, false
		}
		total += *ap.Clients
	}
	return total, true
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
// nothing here should hold it, so the narrow structs below name only
// operational fields and discard the rest rather than carrying the response in
// a map[string]any that some later log line might print.
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
	inventory, err := radio.DecodeWirelessDevices(raw)
	if err != nil {
		return err
	}
	modes := map[string]string{}
	sections := map[string]string{}
	radios := map[string]string{}
	var configuredButAbsent []string
	for radioKey, radio := range v {
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
			radios[i.IfName] = radioKey
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
	s.IfaceRadios = radios
	s.radioInventory = inventory
	s.radioInventoryOK = true
	s.ConfiguredIfacesAbsent = configuredButAbsent
	if s.Topology.expected[TopologySourceWirelessDevices] > 0 {
		wireless, err := topology.DecodeWirelessDevices(raw)
		if err != nil {
			return err
		}
		s.Topology.Wireless = wireless
		s.topologyEvidence(TopologySourceWirelessDevices, len(wireless))
	}
	return nil
}

func decodeRadioFrequencies(key string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		frequencies, err := radio.DecodeFrequencyList(raw)
		if err != nil {
			return err
		}
		if s.radioFrequencies == nil {
			s.radioFrequencies = map[string][]radio.Frequency{}
		}
		s.radioFrequencies[key] = frequencies
		return nil
	}
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
	p.mu.Lock()
	defer p.mu.Unlock()
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

func logCalls() []call {
	return []call{
		{inv: ubus.Invocation{Object: "file", Method: "read", Args: map[string]any{
			"path": "/proc/sys/kernel/random/boot_id",
		}}, decode: decodeLogBootID, optional: true},
		{inv: ubus.Invocation{Object: "service", Method: "list", Args: map[string]any{
			"name": "log", "verbose": true,
		}}, decode: decodeLogService, optional: true},
		{inv: ubus.Invocation{Object: "log", Method: "read", Args: map[string]any{
			"lines": 512, "stream": false,
		}}, decode: decodeLogRows, optional: true},
	}
}

// takeLogs is the cadence gate and attempt stamp. A denied optional call still
// waits until the next minute; a backwards controller clock is rebased rather
// than suppressing coverage until wall time catches up.
func (p *poller) takeLogs() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.c.now()
	elapsed := now.Sub(p.logAt)
	if !p.logAt.IsZero() && elapsed >= 0 && elapsed < p.logIntervalLocked() {
		return false
	}
	p.logAt = now
	return true
}

// rediscoverInterval governs the two facts that change only when a human
// changes them: the firmware identity and the list of radios. Asking for either
// every minute would add calls to every poll for an answer that is almost always
// the one we already have.
const rediscoverInterval = 15 * time.Minute

const logReadInterval = time.Minute

func (p *poller) logIntervalLocked() time.Duration {
	if p.logInterval > 0 {
		return p.logInterval
	}
	return logReadInterval
}

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
