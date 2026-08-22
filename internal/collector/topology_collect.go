package collector

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

const (
	TopologySourceNetworkDevices  = "luci.getNetworkDevices"
	TopologySourceWirelessDevices = "luci.getWirelessDevices"
	TopologySourceBridgeSTP       = "brctl.showstp"
)

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func topologyBridgeNames(devices []topology.NetworkDevice) []string {
	var out []string
	for _, device := range devices {
		if len(device.BridgeOf) > 0 {
			out = append(out, device.Name)
		}
	}
	sort.Strings(out)
	return out
}

func topologySourceSuccessful(sources []model.TopologySourceObservation, source string) bool {
	for _, observation := range sources {
		if observation.Source == source {
			return observation.State == model.TopologySourceObserved ||
				observation.State == model.TopologySourceEmpty
		}
	}
	return false
}

func (p *poller) topologyBridgeList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.topologyBridges...)
}

func (p *poller) needTopology() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.topologyAt.IsZero() || p.c.now().Sub(p.topologyAt) >= rediscoverInterval ||
		(!p.ifaceRefetchAt.IsZero() && !p.c.now().Before(p.ifaceRefetchAt))
}

func (s *Snapshot) prepareTopology(calls []call) {
	for _, spec := range calls {
		if spec.topologySource == "" {
			continue
		}
		if s.Topology.expected == nil {
			s.Topology.Cycle = true
			s.Topology.expected = map[string]int{}
			s.Topology.answered = map[string]int{}
			s.Topology.evidence = map[string]int{}
			s.Topology.failures = map[string]int{}
			s.Topology.failureCauses = map[string]map[DegradationCause]struct{}{}
		}
		s.Topology.expected[spec.topologySource]++
	}
}

func (s *Snapshot) topologyAnswered(source string) {
	if source != "" {
		s.Topology.answered[source]++
	}
}

func (s *Snapshot) topologyFailed(source string, cause DegradationCause) {
	if source == "" {
		return
	}
	s.Topology.failures[source]++
	if s.Topology.failureCauses == nil {
		s.Topology.failureCauses = map[string]map[DegradationCause]struct{}{}
	}
	if s.Topology.failureCauses[source] == nil {
		s.Topology.failureCauses[source] = map[DegradationCause]struct{}{}
	}
	s.Topology.failureCauses[source][cause] = struct{}{}
}

func (s *Snapshot) topologyEvidence(source string, count int) {
	if source != "" && count > 0 && s.Topology.expected[source] > 0 {
		s.Topology.evidence[source] += count
	}
}

func (s *Snapshot) finalizeTopology() {
	if !s.Topology.Cycle {
		return
	}
	media, attachment := topologyPortClassification(s.Topology.NetworkDevices, s.Topology.Wireless)
	for i := range s.Topology.Bridges {
		bridge := &s.Topology.Bridges[i]
		if bridge.STP == nil {
			continue
		}
		bridge.PortMedia = map[int]string{}
		bridge.PortAttachment = map[int]string{}
		for _, port := range bridge.STP.Ports {
			if medium := media[port.Name]; medium != "" {
				bridge.PortMedia[port.Port] = medium
			}
			if scope := attachment[port.Name]; scope != "" {
				bridge.PortAttachment[port.Port] = scope
			}
		}
	}
	at := s.At.UnixMilli()
	for source, expected := range s.Topology.expected {
		observation := model.TopologySourceObservation{
			DeviceID: s.DeviceID, Source: source, ObservedAt: at,
		}
		switch {
		case s.Topology.failures[source] > 0 || s.Topology.answered[source] != expected:
			observation.State = model.TopologySourceError
			observation.Reason = topologyFailureReason(s.Topology.failureCauses[source])
		case s.Topology.evidence[source] > 0:
			observation.State = model.TopologySourceObserved
		default:
			observation.State = model.TopologySourceEmpty
		}
		s.Topology.Sources = append(s.Topology.Sources, observation)
	}
	// A successful inventory with no bridges is authoritative for the prior
	// bridge generation. Cached showmacs/showstp calls may have failed because
	// their last bridge was just removed; retaining those failures would leave
	// the old FDB intervals active forever once the empty bridge list is cached.
	if topologySourceSuccessful(s.Topology.Sources, TopologySourceNetworkDevices) &&
		len(topologyBridgeNames(s.Topology.NetworkDevices)) == 0 {
		s.Topology.Bridges = nil
		for _, source := range []string{topology.SourceBridgeFDB, TopologySourceBridgeSTP} {
			s.Topology.Sources = setTopologySourceEmpty(
				s.Topology.Sources, s.DeviceID, source, at)
		}
	}
	sort.Slice(s.Topology.Sources, func(i, j int) bool {
		return s.Topology.Sources[i].Source < s.Topology.Sources[j].Source
	})
}

func topologyFailureReason(causes map[DegradationCause]struct{}) string {
	labels := make([]string, 0, len(causes))
	for _, cause := range []struct {
		value DegradationCause
		label string
	}{
		{CausePermission, "access/permission denied"},
		{CauseUnsupported, "unsupported operation"},
		{CauseTransport, "transport error"},
		{CauseProtocol, "protocol error"},
		{CauseDecode, "decode/invalid data"},
		{CauseDevice, "device-reported error"},
		{CauseUnknown, "unclassified error"},
	} {
		if _, ok := causes[cause.value]; ok {
			labels = append(labels, cause.label)
		}
	}
	if len(labels) == 0 {
		labels = append(labels, "unclassified error")
	}
	return "source call failure: " + strings.Join(labels, ", ")
}

func setTopologySourceEmpty(sources []model.TopologySourceObservation, deviceID int64,
	name string, at int64) []model.TopologySourceObservation {
	for i := range sources {
		if sources[i].DeviceID == deviceID && sources[i].Source == name {
			sources[i].State = model.TopologySourceEmpty
			sources[i].Reason = ""
			sources[i].ObservedAt = at
			return sources
		}
	}
	return append(sources, model.TopologySourceObservation{
		DeviceID: deviceID, Source: name, State: model.TopologySourceEmpty, ObservedAt: at,
	})
}

func decodeTopologyNetworkDevices(raw json.RawMessage, s *Snapshot) error {
	rows, err := topology.DecodeNetworkDevices(raw)
	if err != nil {
		return err
	}
	s.Topology.NetworkDevices = rows
	s.topologyEvidence(TopologySourceNetworkDevices, len(rows))
	return nil
}

func decodeTopologyNeighbors(family int) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		exec, err := topology.DecodeExecOutput(raw)
		if err != nil {
			return err
		}
		rows, err := topology.ParseNeighbors(family, exec.Stdout)
		if err != nil {
			return err
		}
		if s.Topology.Neighbors == nil {
			s.Topology.Neighbors = map[int][]topology.Neighbor{}
		}
		s.Topology.Neighbors[family] = rows
		s.topologyEvidence(topology.SourceNeighbors(family), len(rows))
		return nil
	}
}

func decodeTopologyFDB(bridge string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		exec, err := topology.DecodeExecOutput(raw)
		if err != nil {
			return err
		}
		rows, err := topology.ParseShowMACs(exec.Stdout)
		if err != nil {
			return err
		}
		observation := s.topologyBridge(bridge)
		observation.Entries = rows
		s.topologyEvidence(topology.SourceBridgeFDB, len(rows))
		return nil
	}
}

func decodeTopologySTP(bridge string) func(json.RawMessage, *Snapshot) error {
	return func(raw json.RawMessage, s *Snapshot) error {
		exec, err := topology.DecodeExecOutput(raw)
		if err != nil {
			return err
		}
		state, err := topology.ParseShowSTP(exec.Stdout)
		if err != nil {
			return err
		}
		if state.Bridge != bridge {
			return &topologyBridgeMismatch{want: bridge, got: state.Bridge}
		}
		observation := s.topologyBridge(bridge)
		observation.STP = &state
		s.topologyEvidence(TopologySourceBridgeSTP, len(state.Ports))
		return nil
	}
}

func decodeTopologyLLDP(raw json.RawMessage, s *Snapshot) error {
	exec, err := topology.DecodeExecOutput(raw)
	if err != nil {
		return err
	}
	links, err := topology.DecodeLLDPJSON(s.DeviceID, exec.Stdout)
	if err != nil {
		return err
	}
	s.Topology.LLDP = links
	s.topologyEvidence(topology.SourceLLDP, len(links))
	return nil
}

type topologyBridgeMismatch struct{ want, got string }

func (e *topologyBridgeMismatch) Error() string {
	return "topology: showstp bridge mismatch: wanted " + e.want + ", got " + e.got
}

func (s *Snapshot) topologyBridge(name string) *topology.BridgeObservation {
	for i := range s.Topology.Bridges {
		if s.Topology.Bridges[i].Bridge == name {
			return &s.Topology.Bridges[i]
		}
	}
	s.Topology.Bridges = append(s.Topology.Bridges, topology.BridgeObservation{
		DeviceID: s.DeviceID, Bridge: name,
	})
	return &s.Topology.Bridges[len(s.Topology.Bridges)-1]
}

func topologyAssociationSource(s *Snapshot) model.TopologySourceObservation {
	state := model.TopologySourceObservation{
		DeviceID: s.DeviceID, Source: topology.SourceAssociations,
		State: model.TopologySourceUnknown, Reason: "association coverage is unavailable",
		ObservedAt: s.At.UnixMilli(),
	}
	stations, known := s.LiveStations()
	if !known {
		return state
	}
	if len(stations) == 0 {
		state.State, state.Reason = model.TopologySourceEmpty, ""
	} else {
		state.State, state.Reason = model.TopologySourceObserved, ""
	}
	return state
}

func topologyPortClassification(networks []topology.NetworkDevice,
	radios []topology.WirelessRadio) (map[string]string, map[string]string) {
	media := map[string]string{}
	attachment := map[string]string{}
	for _, radio := range radios {
		for _, iface := range radio.Interfaces {
			if iface.IfName == "" {
				continue
			}
			if iface.Mode == "mesh" {
				media[iface.IfName] = "mesh"
				attachment[iface.IfName] = topology.PortAttachmentPhysical
			} else if iface.Mode == "ap" || iface.Mode == "sta" {
				media[iface.IfName] = "wireless"
				attachment[iface.IfName] = topology.PortAttachmentPhysical
			}
		}
	}
	byName := map[string]topology.NetworkDevice{}
	for _, device := range networks {
		byName[device.Name] = device
	}
	var wired func(string, map[string]bool) string
	wired = func(name string, seen map[string]bool) string {
		if seen[name] {
			return ""
		}
		seen[name] = true
		device, ok := byName[name]
		if !ok {
			return ""
		}
		kind := strings.ToLower(device.DevType)
		if strings.Contains(kind, "ethernet") || kind == "dsa" {
			return topology.PortAttachmentPhysical
		}
		if device.Parent != "" && wired(device.Parent, seen) != "" {
			return topology.PortAttachmentAggregate
		}
		return ""
	}
	for name := range byName {
		if media[name] != "" {
			continue
		}
		if scope := wired(name, map[string]bool{}); scope != "" {
			media[name] = "wired"
			attachment[name] = scope
		}
	}
	return media, attachment
}
