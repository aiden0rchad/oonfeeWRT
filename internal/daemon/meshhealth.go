package daemon

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/meshlink"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
)

// Mesh backhaul health.
//
// internal/meshlink holds the vocabulary and the ladder; this file gathers the
// four things the ladder needs and does nothing clever with them.
//
// # Where each fact comes from, and what it costs
//
// Nothing here adds a single device request. That is not a happy accident — it
// is why this shape was chosen over the obvious one of asking the device how
// its mesh is doing:
//
//   - CAN this device carry a mesh: the stored capability record, through
//     render.MeshGate, so this and the apply preview say the same sentence.
//   - WAS a mesh applied to it: owned_sections, which records what was applied
//     AND confirmed. Observation alone can never supply this — without it, a
//     mesh whose interface the driver refused to create is indistinguishable
//     from a device that was never asked to run one, which is exactly the §5q
//     failure going unreported.
//   - WHICH interface it is: luci-rpc.getWirelessDevices, already fetched on
//     the 15-minute rediscovery cadence for the interface-mode map.
//   - IS it up: network.device status, already the second call of every
//     baseline poll. The §5q state — `phy0-mesh0 state DOWN` — has been
//     arriving in every snapshot this project has ever taken. Only the join
//     was missing.
//
// Peer counting is the one fact that would cost a request, and it is not here
// yet. See the note on meshFacts.PeersAsked.

// meshFacts is the poll-derived half of a mesh reading, retained per device.
//
// Kept in RAM rather than read back from the database because it is a
// description of right now: a stale interface list read from disk would answer
// "is this backhaul up" with something that was true at some point.
type meshFacts struct {
	modes    map[string]string
	sections map[string]string
	absent   []string
	up       map[string]bool
	// peers is per mesh interface. A key present with an empty slice is a
	// demonstrated zero; an absent key means nobody asked or nothing came back.
	peers map[string][]meshlink.Peer
	// peersAskedFor names the interfaces this device was asked about, so a
	// refusal ("asked, no answer") is distinguishable from the baseline case
	// ("nobody asked"). Two different states, two different sentences.
	peersAskedFor map[string]bool
	// ifacesFresh records that the interface list answered; netDevsFresh that
	// the liveness call did. Two calls, two cadences, two ways of not knowing.
	ifacesFresh  bool
	netDevsFresh bool
	// PeersAsked is deliberately absent from this struct.
	//
	// Peer counting needs `iw dev <if> station dump`, which is a process spawn.
	// DEVICE-BUDGET §3.2 says file.exec belongs on the slow loop and never the
	// fast one; the feature table at §7 sanctions `iw station dump` at focused
	// rate. Those conflict, and this resolves toward the rule: mesh peers
	// change when somebody unplugs a node, not while a screen is open, so the
	// slow cadence loses nothing. Until that lands, every link reports
	// peers-not-counted, which is a real state that says so rather than a zero
	// that lies.
}

type meshStore struct {
	mu   sync.Mutex
	byID map[int64]meshFacts
}

func (m *meshStore) put(s collector.Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID == nil {
		m.byID = map[int64]meshFacts{}
	}
	prev := m.byID[s.DeviceID]
	f := prev

	// The interface list rides a 15-minute cadence, so a baseline poll in
	// between carries none. Carrying the previous answer forward is the same
	// rule the client-scoping subnets follow: a determination is never
	// overwritten by a non-determination, or the readout flickers between
	// "peered" and "cannot tell" for reasons no operator could see.
	if s.IfaceModes != nil {
		f.modes, f.sections, f.absent = s.IfaceModes, s.IfaceSections, s.ConfiguredIfacesAbsent
		f.ifacesFresh = true
	}
	// Peers ride the same slow cadence as the interface list, so most polls
	// carry none and the previous answer is carried forward rather than erased.
	if s.MeshPeers != nil {
		f.peers = s.MeshPeers
		asked := map[string]bool{}
		for iface, mode := range f.modes {
			if mode == "mesh" {
				asked[iface] = true
			}
		}
		f.peersAskedFor = asked
	}
	f.netDevsFresh = s.NetDevsFresh
	if s.NetDevsFresh {
		up := make(map[string]bool, len(s.Interfaces))
		for name, iface := range s.Interfaces {
			up[name] = iface.Up
		}
		f.up = up
	}
	m.byID[s.DeviceID] = f
}

func (m *meshStore) get(deviceID int64) meshFacts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[deviceID]
}

// MeshHealth reports every configured backhaul in the site, per device.
//
// Reads nothing from any device: every input is either the site model, a stored
// record, or the last poll. So it is safe to call from a screen refresh without
// touching the request budget.
func (d *Daemon) MeshHealth(ctx context.Context) ([]meshlink.Link, error) {
	site, err := d.Store.Site(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := d.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}

	groups := map[int][]int64{}
	for _, g := range site.Groups {
		groups[g.ID] = g.DeviceIDs
	}

	// Which devices could carry a mesh at all, decided before anything else
	// needs the answer.
	//
	// It is needed twice: to gate each device's own reading, and to decide
	// whether a mesh with no peers is DISAPPOINTING or simply alone. The naive
	// version of that second question — "is another device in this group" — is
	// wrong on this very fleet, where the second device cannot run a mesh at
	// all. It would leave the working node sitting at warning permanently,
	// which is the thing this readout is supposed to avoid rather than commit.
	type devGate struct {
		ok     bool
		reason string
	}
	gates := map[int64]devGate{}
	for _, dev := range devices {
		if !dev.Adopted() || !model.RoleOf(dev.Role).Wireless() {
			continue
		}
		if caps, err := deviceCaps(dev); err == nil {
			ok, reason := render.MeshGate(caps)
			gates[dev.ID] = devGate{ok, reason}
		}
	}
	// A mesh expects peers when the site model puts a SECOND device that can
	// actually carry it in the same group.
	peerExpected := map[int]bool{}
	for _, m := range site.Meshes {
		n := 0
		for _, id := range groups[m.GroupID] {
			if gates[id].ok {
				n++
			}
		}
		peerExpected[m.ID] = n > 1
	}

	out := []meshlink.Link{}
	for _, dev := range devices {
		gate, known := gates[dev.ID]
		if !known {
			continue
		}
		buildable, gateReason := gate.ok, gate.reason

		applied, err := d.appliedMeshSections(ctx, dev.ID)
		if err != nil {
			return nil, err
		}
		facts := d.meshes.get(dev.ID)

		for _, m := range site.Meshes {
			if !m.Enabled || !contains(groups[m.GroupID], dev.ID) {
				continue
			}
			o := meshlink.Observation{
				DeviceID: dev.ID, MeshID: m.ID, Name: m.MeshID,
				Buildable: buildable, GateReason: gateReason,
				IfaceKnown:   facts.ifacesFresh,
				NetDevsFresh: facts.netDevsFresh,
			}
			o.Section, o.SectionSeen = applied[m.ID]

			if o.SectionSeen && facts.ifacesFresh {
				o.Iface = ifaceForSection(facts.sections, facts.modes, o.Section)
				if o.Iface == "" {
					// The section is applied and the device names no interface
					// for it. Only a positive claim when the device actually
					// listed a section-without-interface: a device that reports
					// no `section` field at all cannot support this conclusion,
					// and guessing it would invent §5q where none happened.
					o.SectionAppliedNoIface = containsStr(facts.absent, o.Section)
					if !o.SectionAppliedNoIface {
						o.IfaceKnown = false
					}
				}
			}
			if o.Iface != "" && facts.netDevsFresh {
				o.Up, o.NetDevFound = facts.up[o.Iface]
			}
			if o.Iface != "" {
				o.PeersAsked = facts.peersAskedFor[o.Iface]
				o.Peers, o.PeersKnown = facts.peers[o.Iface]
			}
			out = append(out, meshlink.Evaluate(o, peerExpected[m.ID]))
		}
	}
	return out, nil
}

// appliedMeshSections maps a site mesh id to the UCI section applied for it.
//
// Read from owned_sections, which records what was applied AND confirmed,
// rather than re-deriving the name from the renderer. The recorded fact is the
// one that matters: a section the controller believes it wrote is exactly what
// makes a missing interface a fault rather than a device nobody configured.
func (d *Daemon) appliedMeshSections(ctx context.Context, deviceID int64) (map[int]string, error) {
	owned, err := d.Store.OwnedSections(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, s := range owned {
		if s.Config != "wireless" {
			continue
		}
		if id, ok := meshIDFromSection(s.Section); ok {
			out[id] = s.Section
		}
	}
	return out, nil
}

// meshIDFromSection is the inverse of render.meshIfaceName, which builds
// `oowrt_mesh<id>_<radio>`.
//
// Parsed rather than reconstructed because reconstructing needs the radio
// section name, which means asking the renderer what it would do — and the
// question here is what it already did.
func meshIDFromSection(section string) (int, bool) {
	const prefix = render.NamePrefix + "_mesh"
	if !strings.HasPrefix(section, prefix) {
		return 0, false
	}
	rest := section[len(prefix):]
	i := strings.IndexByte(rest, '_')
	if i <= 0 {
		return 0, false
	}
	id, err := strconv.Atoi(rest[:i])
	if err != nil {
		return 0, false
	}
	return id, true
}

// ifaceForSection finds the interface a section created.
//
// Prefers the device's own section mapping. Falls back to the single
// mesh-mode interface when the device reported no section names — which the
// captured hardware fixture shows really happens — and refuses to guess when
// there is more than one, because the site model permits one mesh id on two
// bands and a wrong attribution is worse here than none.
func ifaceForSection(sections, modes map[string]string, section string) string {
	for iface, sec := range sections {
		if sec == section {
			return iface
		}
	}
	var only string
	for iface, mode := range modes {
		if mode != "mesh" {
			continue
		}
		if only != "" {
			return ""
		}
		only = iface
	}
	return only
}

func contains(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// MeshHealthReport adapts MeshHealth to the API shape.
//
// The device NAME is joined here rather than carried through meshlink, which
// deliberately knows nothing about the inventory: it decides what a link is
// doing, and who owns it is presentation.
func (d *Daemon) MeshHealthReport(ctx context.Context) (*api.MeshHealthResult, error) {
	links, err := d.MeshHealth(ctx)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	if devices, err := d.Store.Devices(ctx); err == nil {
		for _, dev := range devices {
			names[dev.ID] = dev.Name
		}
	}
	out := &api.MeshHealthResult{Links: make([]api.MeshLink, 0, len(links))}
	for _, l := range links {
		row := api.MeshLink{
			DeviceID: l.DeviceID, DeviceName: names[l.DeviceID],
			MeshID: l.MeshID, Name: l.Name, Iface: l.Iface,
			State: string(l.State), Tone: string(l.Tone), Reason: l.Reason,
			Established: l.Established, Actionable: l.Actionable(),
		}
		// nil, not an empty slice, when nobody counted. The JSON has to keep
		// "none" and "not counted" apart, and this is the field that would
		// blur them.
		if l.Peers != nil {
			row.Peers = make([]api.MeshPeer, 0, len(l.Peers))
			for _, p := range l.Peers {
				row.Peers = append(row.Peers, api.MeshPeer{
					MAC: p.MAC, Plink: p.Plink,
					SignalDBm: p.SignalDBm, InactiveMS: p.InactiveMS,
				})
			}
		}
		out.Links = append(out.Links, row)
	}
	return out, nil
}
