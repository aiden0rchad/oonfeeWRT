package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

const InternetNode = "synthetic:internet"
const competingParentsAmbiguity = "concurrent evidence yields multiple candidate parents or ports"

const (
	PortAttachmentPhysical  = "physical"
	PortAttachmentAggregate = "aggregate"
)

const (
	SourceBridgeFDB    = "brctl.showmacs"
	SourceBridgeSTP    = "brctl.showstp"
	SourceAssociations = "hostapd.get_clients"
	SourceLLDP         = "lldp"
	// SourceDefaultRoute is the durable compatibility key for the composite
	// kernel-route plus network.interface.dump observation.
	SourceDefaultRoute = "network.interface.dump"
)

func SourceNeighbors(family int) string { return fmt.Sprintf("ip-%d-neigh", family) }

// InventoryDevice carries controller identity separately from every observed
// MAC alias. The canonical inventory MAC is the stable node identity; ID is
// retained only for current database joins and evidence provenance.
type InventoryDevice struct {
	ID         int64
	Name       string
	PrimaryMAC string
	Aliases    []string
}

type InventoryClient struct {
	MAC  string
	Name string
}

type BridgeObservation struct {
	DeviceID int64
	Bridge   string
	Entries  []FDBEntry
	// STP supplies BusyBox brctl's numeric-port to interface-name mapping.
	// Nil means that mapping was not observed.
	STP *STPState
	// PortMedia is populated only after the mapped interface is positively
	// classified from runtime device data. Interface-name prefixes are not
	// evidence of a link medium.
	PortMedia map[int]string
	// PortAttachment distinguishes an exact physical bridge member from a
	// legacy CPU/VLAN aggregate that can expose MACs reached through the switch.
	PortAttachment map[int]string
}

type Association struct {
	DeviceID  int64
	Interface string
	MAC       string
}

// LLDPLink is optional enrichment. Absence is represented by a source-state
// row, never by an empty slice alone.
type LLDPLink struct {
	DeviceID  int64
	Port      string
	RemoteMAC string
}

// Uplink proves an active default-route attachment. Device role alone never
// creates the synthetic internet node or this edge.
type Uplink struct {
	DeviceID         int64
	Interface        string
	LogicalInterface string
	Active           bool
}

type InferenceInput struct {
	At           int64 // Unix milliseconds
	Devices      []InventoryDevice
	Clients      []InventoryClient
	Bridges      []BridgeObservation
	Neighbors    map[int64][]Neighbor
	Associations []Association
	LLDP         []LLDPLink
	Uplinks      []Uplink
	Sources      []model.TopologySourceObservation
}

type EdgeEvidence struct {
	Kind     string         `json:"kind"`
	Source   string         `json:"source"`
	DeviceID int64          `json:"device_id,omitempty"`
	Detail   map[string]any `json:"detail"`
}

type InferenceResult struct {
	Edges    []model.TopologyEdge
	Sources  []model.TopologySourceObservation
	Complete bool
	Gaps     []string
}

type edgeKey struct {
	child  string
	parent string
	port   string
	medium string
}

type edgeCandidate struct {
	edge        model.TopologyEdge
	evidence    []EdgeEvidence
	ambiguities map[string]bool
}

type identityResolver struct {
	devices map[string]map[string]bool
	clients map[string]bool
}

// Infer builds graph intervals for one observation timestamp. It never picks a
// winner where the sources leave several parents, ports or managed-device
// aliases possible; those alternatives remain explicit ambiguous edges.
func Infer(input InferenceInput) (InferenceResult, error) {
	if input.At <= 0 {
		return InferenceResult{}, errors.New("topology: observation time must be Unix milliseconds")
	}
	resolver, deviceNodes, err := newIdentityResolver(input.Devices, input.Clients)
	if err != nil {
		return InferenceResult{}, err
	}
	if err := validateSourceStates(input.Sources, deviceNodes); err != nil {
		return InferenceResult{}, err
	}
	result := InferenceResult{Sources: normalizedSourceStates(input.Sources)}
	result.Gaps = sourceGaps(result.Sources)
	if len(result.Sources) == 0 {
		result.Gaps = append(result.Gaps, "topology sources have not been observed")
	}

	candidates := map[edgeKey]*edgeCandidate{}
	add := func(edge model.TopologyEdge, evidence EdgeEvidence, ambiguities ...string) {
		key := edgeKey{edge.ChildNode, edge.ParentNode, edge.ParentPort, edge.Medium}
		candidate := candidates[key]
		if candidate == nil {
			candidate = &edgeCandidate{edge: edge, ambiguities: map[string]bool{}}
			candidates[key] = candidate
		} else if edge.ChildMAC != "" &&
			(candidate.edge.ChildMAC == "" || edge.ChildMAC < candidate.edge.ChildMAC) {
			// Several interface/BSSID aliases may resolve to one managed node.
			// Keep a deterministic representative; every observed alias remains
			// in Evidence, which is the provenance that actually matters.
			candidate.edge.ChildMAC = edge.ChildMAC
		}
		candidate.evidence = append(candidate.evidence, evidence)
		for _, ambiguity := range ambiguities {
			if ambiguity != "" {
				candidate.ambiguities[ambiguity] = true
			}
		}
		if edge.Confidence == "measured" {
			candidate.edge.Confidence = "measured"
		}
	}

	neighborByDeviceMAC := map[int64]map[string][]Neighbor{}
	for deviceID, rows := range input.Neighbors {
		if _, ok := deviceNodes[deviceID]; !ok {
			return InferenceResult{}, fmt.Errorf("topology: neighbors reference unknown device %d", deviceID)
		}
		neighborByDeviceMAC[deviceID] = map[string][]Neighbor{}
		for _, row := range rows {
			if row.MAC == "" {
				continue
			}
			mac, err := canonicalMAC(row.MAC)
			if err != nil {
				return InferenceResult{}, err
			}
			neighborByDeviceMAC[deviceID][mac] = append(neighborByDeviceMAC[deviceID][mac], row)
		}
	}

	for _, observation := range input.Bridges {
		parent, ok := deviceNodes[observation.DeviceID]
		if !ok {
			return InferenceResult{}, fmt.Errorf("topology: bridge references unknown device %d", observation.DeviceID)
		}
		if !validInterfaceName(observation.Bridge) {
			return InferenceResult{}, fmt.Errorf("topology: invalid bridge %q", observation.Bridge)
		}
		portNames := map[int]string{}
		for port, medium := range observation.PortMedia {
			if port < 0 || (medium != "wired" && medium != "wireless" && medium != "mesh" && medium != "unknown") {
				return InferenceResult{}, fmt.Errorf("topology: bridge %q port %d has invalid medium %q", observation.Bridge, port, medium)
			}
		}
		for port, attachment := range observation.PortAttachment {
			if port < 0 || (attachment != PortAttachmentPhysical && attachment != PortAttachmentAggregate) {
				return InferenceResult{}, fmt.Errorf("topology: bridge %q port %d has invalid attachment %q", observation.Bridge, port, attachment)
			}
		}
		if observation.STP != nil {
			if observation.STP.Bridge != observation.Bridge {
				return InferenceResult{}, fmt.Errorf("topology: STP bridge %q does not match FDB bridge %q", observation.STP.Bridge, observation.Bridge)
			}
			for _, port := range observation.STP.Ports {
				if old := portNames[port.Port]; old != "" && old != port.Name {
					return InferenceResult{}, fmt.Errorf("topology: bridge %q port %d maps to both %q and %q", observation.Bridge, port.Port, old, port.Name)
				}
				portNames[port.Port] = port.Name
			}
		}
		for _, entry := range observation.Entries {
			if entry.Local {
				continue
			}
			mac, err := canonicalMAC(entry.MAC)
			if err != nil {
				return InferenceResult{}, err
			}
			child, identityAmbiguity := resolver.resolve(mac)
			if child == parent {
				continue
			}
			port := portNames[entry.Port]
			medium := "unknown"
			attachment := ""
			if port != "" && observation.PortMedia[entry.Port] != "" {
				medium = observation.PortMedia[entry.Port]
			}
			if port != "" {
				attachment = observation.PortAttachment[entry.Port]
			}
			ambiguities := []string{
				"BusyBox brctl showmacs does not identify VLAN",
				identityAmbiguity,
			}
			if port == "" {
				ambiguities = append(ambiguities, "bridge port number could not be mapped to an interface")
				result.Gaps = append(result.Gaps, fmt.Sprintf("device:%d/port-mapping: bridge %s port %d is unknown", observation.DeviceID, observation.Bridge, entry.Port))
			}
			if medium == "unknown" {
				ambiguities = append(ambiguities, "bridge evidence does not identify link medium")
				result.Gaps = append(result.Gaps, fmt.Sprintf("device:%d/medium: bridge %s port %d is unclassified", observation.DeviceID, observation.Bridge, entry.Port))
			}
			result.Gaps = append(result.Gaps, fmt.Sprintf("device:%d/vlan: brctl showmacs does not expose VLAN", observation.DeviceID))
			detail := map[string]any{
				"bridge": observation.Bridge, "port_number": entry.Port, "observed_mac": mac,
			}
			if port != "" {
				detail["interface"] = port
			}
			if attachment != "" {
				detail["attachment"] = attachment
			}
			parentID := observation.DeviceID
			edge := model.TopologyEdge{
				ChildNode: child, ChildMAC: mac, ParentNode: parent,
				ParentDeviceID: &parentID, ParentPort: port, Medium: medium,
				Confidence: "ambiguous", ValidFrom: input.At, LastSeen: input.At,
			}
			add(edge, EdgeEvidence{Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: observation.DeviceID, Detail: detail}, ambiguities...)

			for _, neighbor := range neighborByDeviceMAC[observation.DeviceID][mac] {
				add(edge, EdgeEvidence{
					Kind: "neighbor", Source: SourceNeighbors(neighbor.Family), DeviceID: observation.DeviceID,
					Detail: map[string]any{"address": neighbor.Address, "interface": neighbor.Interface, "state": neighbor.State, "observed_mac": mac},
				}, ambiguities...)
			}
		}
	}

	for _, association := range input.Associations {
		parent, ok := deviceNodes[association.DeviceID]
		if !ok {
			return InferenceResult{}, fmt.Errorf("topology: association references unknown device %d", association.DeviceID)
		}
		if !validInterfaceName(association.Interface) {
			return InferenceResult{}, errors.New("topology: valid association interface is required")
		}
		mac, err := canonicalMAC(association.MAC)
		if err != nil {
			return InferenceResult{}, err
		}
		child, identityAmbiguity := resolver.resolve(mac)
		if child == parent {
			continue
		}
		parentID := association.DeviceID
		confidence := "measured"
		if identityAmbiguity != "" {
			confidence = "ambiguous"
		}
		add(model.TopologyEdge{
			ChildNode: child, ChildMAC: mac, ParentNode: parent,
			ParentDeviceID: &parentID, ParentPort: association.Interface,
			Medium: "wireless", Confidence: confidence, ValidFrom: input.At, LastSeen: input.At,
		}, EdgeEvidence{
			Kind: "association", Source: SourceAssociations, DeviceID: association.DeviceID,
			Detail: map[string]any{"interface": association.Interface, "observed_mac": mac},
		}, identityAmbiguity)
	}

	for _, link := range input.LLDP {
		parent, ok := deviceNodes[link.DeviceID]
		if !ok {
			return InferenceResult{}, fmt.Errorf("topology: LLDP references unknown device %d", link.DeviceID)
		}
		if !validInterfaceName(link.Port) {
			return InferenceResult{}, errors.New("topology: valid LLDP local port is required")
		}
		mac, err := canonicalMAC(link.RemoteMAC)
		if err != nil {
			return InferenceResult{}, err
		}
		child, identityAmbiguity := resolver.resolve(mac)
		if child == parent {
			continue
		}
		parentID := link.DeviceID
		confidence := "measured"
		if identityAmbiguity != "" {
			confidence = "ambiguous"
		}
		add(model.TopologyEdge{
			ChildNode: child, ChildMAC: mac, ParentNode: parent,
			ParentDeviceID: &parentID, ParentPort: link.Port,
			Medium: "wired", Confidence: confidence, ValidFrom: input.At, LastSeen: input.At,
		}, EdgeEvidence{
			Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: link.DeviceID,
			Detail: map[string]any{"local_port": link.Port, "remote_mac": mac},
		}, identityAmbiguity)
	}

	for _, uplink := range input.Uplinks {
		child, ok := deviceNodes[uplink.DeviceID]
		if !ok {
			return InferenceResult{}, fmt.Errorf("topology: uplink references unknown device %d", uplink.DeviceID)
		}
		if !uplink.Active {
			continue
		}
		if !validInterfaceName(uplink.Interface) {
			return InferenceResult{}, errors.New("topology: valid active uplink interface is required")
		}
		detail := map[string]any{"interface": uplink.Interface, "active": true}
		if uplink.LogicalInterface != "" && uplink.LogicalInterface != uplink.Interface {
			detail["logical_interface"] = uplink.LogicalInterface
		}
		add(model.TopologyEdge{
			ChildNode: child, ParentNode: InternetNode,
			ParentPort: uplink.Interface, Medium: "uplink", Confidence: "measured",
			ValidFrom: input.At, LastSeen: input.At,
		}, EdgeEvidence{
			Kind: "default_route", Source: SourceDefaultRoute, DeviceID: uplink.DeviceID,
			Detail: detail,
		})
	}

	for _, candidate := range candidates {
		sortEvidence(candidate.evidence)
		for _, evidence := range candidate.evidence {
			var deviceID *int64
			if evidence.DeviceID > 0 {
				id := evidence.DeviceID
				deviceID = &id
			}
			candidate.edge.Evidence = append(candidate.edge.Evidence, model.TopologyEvidence{
				Kind: evidence.Kind, Source: evidence.Source, DeviceID: deviceID,
				Detail: evidence.Detail,
			})
		}
	}
	pruneContradictedAggregateCandidates(candidates)
	markCompetingParents(candidates)
	for _, candidate := range candidates {
		ambiguities := sortedSet(candidate.ambiguities)
		candidate.edge.Ambiguities = ambiguities
		result.Edges = append(result.Edges, candidate.edge)
	}
	sort.Slice(result.Edges, func(i, j int) bool {
		a, b := result.Edges[i], result.Edges[j]
		return edgeSortKey(a) < edgeSortKey(b)
	})
	result.Gaps = uniqueSorted(result.Gaps)
	result.Complete = len(result.Gaps) == 0
	return result, nil
}

func newIdentityResolver(devices []InventoryDevice, clients []InventoryClient) (*identityResolver, map[int64]string, error) {
	r := &identityResolver{devices: map[string]map[string]bool{}, clients: map[string]bool{}}
	nodes := map[int64]string{}
	primaryNodes := map[string]int64{}
	for _, device := range devices {
		if device.ID <= 0 || nodes[device.ID] != "" {
			return nil, nil, fmt.Errorf("topology: invalid or duplicate device id %d", device.ID)
		}
		primary, err := canonicalMAC(device.PrimaryMAC)
		if err != nil {
			return nil, nil, fmt.Errorf("topology: device %d inventory MAC: %w", device.ID, err)
		}
		if prior := primaryNodes[primary]; prior != 0 {
			return nil, nil, fmt.Errorf("topology: devices %d and %d share inventory MAC %s", prior, device.ID, primary)
		}
		primaryNodes[primary] = device.ID
		node := deviceNode(primary)
		nodes[device.ID] = node
		for _, raw := range append([]string{primary}, device.Aliases...) {
			if raw == "" {
				continue
			}
			mac, err := canonicalMAC(raw)
			if err != nil {
				return nil, nil, err
			}
			if r.devices[mac] == nil {
				r.devices[mac] = map[string]bool{}
			}
			r.devices[mac][node] = true
		}
	}
	for _, client := range clients {
		mac, err := canonicalMAC(client.MAC)
		if err != nil {
			return nil, nil, err
		}
		r.clients[mac] = true
	}
	return r, nodes, nil
}

func (r *identityResolver) resolve(mac string) (string, string) {
	ids := r.devices[mac]
	if len(ids) == 1 {
		for node := range ids {
			return node, ""
		}
	}
	if len(ids) > 1 {
		var refs []string
		for node := range ids {
			refs = append(refs, node)
		}
		sort.Strings(refs)
		return "mac:" + mac, "MAC alias belongs to multiple managed devices: " + strings.Join(refs, ", ")
	}
	if r.clients[mac] {
		return "client:" + mac, ""
	}
	return "mac:" + mac, ""
}

func pruneContradictedAggregateCandidates(candidates map[edgeKey]*edgeCandidate) {
	edges := make([]model.TopologyEdge, 0, len(candidates))
	for _, candidate := range candidates {
		edges = append(edges, candidate.edge)
	}
	contradicted := contradictedAggregateEdgeKeys(edges)
	for key, candidate := range candidates {
		if contradicted[edgeSortKey(candidate.edge)] {
			delete(candidates, key)
		}
	}
}

// An aggregate CPU/VLAN bridge member can see MACs from either side of a
// legacy switch. It is contradicted by an exact association above its device,
// an exact physical observation in the opposite direction, or exact physical
// observations on different ports of the same parent. Equal placements and
// associations to different devices remain ambiguous.
func contradictedAggregateEdgeKeys(edges []model.TopologyEdge) map[string]bool {
	physical := map[string][]model.TopologyEdge{}
	associationParents := map[string]map[string]bool{}
	for _, edge := range edges {
		if edgePortAttachment(edge) == PortAttachmentPhysical {
			physical[edge.ChildNode] = append(physical[edge.ChildNode], edge)
		}
		if edgeHasAssociation(edge) {
			if associationParents[edge.ChildNode] == nil {
				associationParents[edge.ChildNode] = map[string]bool{}
			}
			associationParents[edge.ChildNode][edge.ParentNode] = true
		}
	}
	contradicted := map[string]bool{}
	for _, edge := range edges {
		if edgePortAttachment(edge) != PortAttachmentAggregate {
			continue
		}
		for _, reverse := range physical[edge.ParentNode] {
			if reverse.ParentNode == edge.ChildNode {
				contradicted[edgeSortKey(edge)] = true
				break
			}
		}
		if contradicted[edgeSortKey(edge)] ||
			len(associationParents[edge.ChildNode]) > 1 {
			continue
		}
		for associationParent := range associationParents[edge.ChildNode] {
			placements := physical[edge.ParentNode]
			if len(placements) == 0 {
				break
			}
			allBelowAssociationParent := true
			for _, placement := range placements {
				if placement.ParentNode != associationParent {
					allBelowAssociationParent = false
					break
				}
			}
			if allBelowAssociationParent {
				contradicted[edgeSortKey(edge)] = true
			}
		}
		if contradicted[edgeSortKey(edge)] ||
			len(physical[edge.ChildNode]) != 1 || len(physical[edge.ParentNode]) != 1 {
			continue
		}
		child, aggregateParent := physical[edge.ChildNode][0], physical[edge.ParentNode][0]
		if child.ParentNode == aggregateParent.ParentNode && child.ParentPort != "" &&
			aggregateParent.ParentPort != "" && child.ParentPort != aggregateParent.ParentPort {
			contradicted[edgeSortKey(edge)] = true
		}
	}
	return contradicted
}

func edgeHasAssociation(edge model.TopologyEdge) bool {
	for _, evidence := range edge.Evidence {
		if evidence.Kind == "association" && evidence.Source == SourceAssociations {
			return true
		}
	}
	return false
}

func edgePortAttachment(edge model.TopologyEdge) string {
	attachment := ""
	for _, evidence := range edge.Evidence {
		value, _ := evidence.Detail["attachment"].(string)
		if value == PortAttachmentPhysical {
			return value
		}
		if value == PortAttachmentAggregate {
			attachment = value
		}
	}
	return attachment
}

func markCompetingParents(candidates map[edgeKey]*edgeCandidate) {
	parents := map[string]map[string]bool{}
	for key := range candidates {
		if key.parent == InternetNode {
			continue
		}
		if parents[key.child] == nil {
			parents[key.child] = map[string]bool{}
		}
		parents[key.child][key.parent+"\x00"+key.port] = true
	}
	for key, candidate := range candidates {
		if len(parents[key.child]) > 1 {
			candidate.ambiguities[competingParentsAmbiguity] = true
			candidate.edge.Confidence = "ambiguous"
		}
	}
}

func normalizedSourceStates(states []model.TopologySourceObservation) []model.TopologySourceObservation {
	out := append([]model.TopologySourceObservation(nil), states...)
	for i := range out {
		out[i].Reason = safeReason(out[i].Reason)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID == out[j].DeviceID {
			return out[i].Source < out[j].Source
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

func validateSourceStates(states []model.TopologySourceObservation, deviceNodes map[int64]string) error {
	seen := map[string]bool{}
	for _, state := range states {
		if deviceNodes[state.DeviceID] == "" {
			return fmt.Errorf("topology: source %q references unknown device %d", state.Source, state.DeviceID)
		}
		if strings.TrimSpace(state.Source) != state.Source || state.Source == "" || state.ObservedAt <= 0 {
			return errors.New("topology: source requires a name and observation time")
		}
		switch state.State {
		case model.TopologySourceUnknown, model.TopologySourceEmpty,
			model.TopologySourceObserved, model.TopologySourceError:
		default:
			return fmt.Errorf("topology: source %q has invalid state %q", state.Source, state.State)
		}
		key := strconv.FormatInt(state.DeviceID, 10) + "\x00" + state.Source
		if seen[key] {
			return fmt.Errorf("topology: duplicate source state for device %d/%s", state.DeviceID, state.Source)
		}
		seen[key] = true
	}
	return nil
}

func sourceGaps(states []model.TopologySourceObservation) []string {
	var gaps []string
	for _, state := range states {
		if state.State == model.TopologySourceObserved || state.State == model.TopologySourceEmpty {
			continue
		}
		reason := state.Reason
		if reason == "" {
			reason = string(state.State)
		}
		gaps = append(gaps, fmt.Sprintf("device:%d/%s: %s", state.DeviceID, state.Source, reason))
	}
	return gaps
}

func safeReason(reason string) string {
	reason = strings.Join(strings.FieldsFunc(reason, unicode.IsSpace), " ")
	if chars := []rune(reason); len(chars) > 240 {
		reason = string(chars[:240])
	}
	return reason
}

func sortEvidence(evidence []EdgeEvidence) {
	sort.Slice(evidence, func(i, j int) bool {
		a, _ := json.Marshal(evidence[i].Detail)
		b, _ := json.Marshal(evidence[j].Detail)
		left := evidence[i].Source + "\x00" + evidence[i].Kind + "\x00" + strconv.FormatInt(evidence[i].DeviceID, 10) + "\x00" + string(a)
		right := evidence[j].Source + "\x00" + evidence[j].Kind + "\x00" + strconv.FormatInt(evidence[j].DeviceID, 10) + "\x00" + string(b)
		return left < right
	})
}

func edgeSortKey(edge model.TopologyEdge) string {
	return edge.ChildNode + "\x00" + edge.ParentNode + "\x00" + edge.ParentPort + "\x00" + edge.Medium
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func uniqueSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return sortedSet(set)
}

func deviceNode(mac string) string { return "device:" + mac }

// IntervalChanges describes the minimum durable writes for a new inference.
// Missing edges close only after a complete observation; a denied source may
// not turn silence into a disconnection event.
type IntervalChanges struct {
	Upsert []model.TopologyEdge
	Close  []model.TopologyEdge
}

func ReconcileIntervals(active, observed []model.TopologyEdge, at int64, complete bool) (IntervalChanges, error) {
	if at <= 0 {
		return IntervalChanges{}, errors.New("topology: reconciliation time must be positive")
	}
	byKey := map[string]model.TopologyEdge{}
	for _, edge := range active {
		if edge.ValidTo != nil {
			return IntervalChanges{}, fmt.Errorf("topology: edge %d is not active", edge.ID)
		}
		if edge.ValidFrom <= 0 || edge.LastSeen < edge.ValidFrom || at < edge.LastSeen {
			return IntervalChanges{}, fmt.Errorf("topology: edge %d has an invalid or future interval", edge.ID)
		}
		key := edgeSortKey(edge)
		if _, duplicate := byKey[key]; duplicate {
			return IntervalChanges{}, fmt.Errorf("topology: duplicate active edge %q", key)
		}
		byKey[key] = edge
	}
	changes := IntervalChanges{}
	seen := map[string]bool{}
	for _, edge := range observed {
		key := edgeSortKey(edge)
		if seen[key] {
			return IntervalChanges{}, fmt.Errorf("topology: duplicate observed edge %q", key)
		}
		seen[key] = true
		if prior, ok := byKey[key]; ok {
			same, err := sameTopologySemantics(prior, edge)
			if err != nil {
				return IntervalChanges{}, err
			}
			if same {
				edge.ID = prior.ID
				edge.ValidFrom = prior.ValidFrom
			} else {
				// Geometry alone is not the interval identity. Confidence,
				// ambiguity and evidence are facts at a point in time; rewriting
				// them on the old row would make historical replay describe the
				// present. End the old semantic version and start a new one.
				closedAt := at
				prior.ValidTo = &closedAt
				changes.Close = append(changes.Close, prior)
				edge.ID = 0
				edge.ValidFrom = at
			}
		}
		edge.ValidTo = nil
		edge.LastSeen = at
		if edge.ValidFrom == 0 {
			edge.ValidFrom = at
		}
		changes.Upsert = append(changes.Upsert, edge)
	}
	if complete {
		for key, edge := range byKey {
			if seen[key] {
				continue
			}
			closedAt := at
			edge.ValidTo = &closedAt
			changes.Close = append(changes.Close, edge)
		}
	}
	sort.Slice(changes.Upsert, func(i, j int) bool { return edgeSortKey(changes.Upsert[i]) < edgeSortKey(changes.Upsert[j]) })
	sort.Slice(changes.Close, func(i, j int) bool { return edgeSortKey(changes.Close[i]) < edgeSortKey(changes.Close[j]) })
	return changes, nil
}

// ReconcileIntervalsBySource closes a disappeared edge only when every source
// that originally established that edge answered this cycle. It also retains
// the prior semantic payload when a same-geometry observation has only partial
// source coverage. Presentation gaps such as brctl's missing VLAN do not freeze
// valid FDB interval changes, while an unavailable source cannot manufacture a
// link-down or a new historical version.
func ReconcileIntervalsBySource(active, observed []model.TopologyEdge, at int64,
	sources []model.TopologySourceObservation) (IntervalChanges, error) {
	states := map[string]model.TopologySourceObservation{}
	for _, source := range sources {
		if source.DeviceID <= 0 || strings.TrimSpace(source.Source) != source.Source || source.Source == "" ||
			source.ObservedAt <= 0 || source.ObservedAt > at {
			return IntervalChanges{}, errors.New("topology: source-aware reconciliation requires device and source")
		}
		switch source.State {
		case model.TopologySourceUnknown, model.TopologySourceEmpty,
			model.TopologySourceObserved, model.TopologySourceError:
		default:
			return IntervalChanges{}, fmt.Errorf("topology: source %q has invalid state %q", source.Source, source.State)
		}
		key := sourceStateKey(source.DeviceID, source.Source)
		if _, duplicate := states[key]; duplicate {
			return IntervalChanges{}, fmt.Errorf("topology: duplicate source state for device %d/%s", source.DeviceID, source.Source)
		}
		states[key] = source
	}
	activeByKey := make(map[string]model.TopologyEdge, len(active))
	for _, edge := range active {
		activeByKey[edgeSortKey(edge)] = edge
	}
	observed = append([]model.TopologyEdge(nil), observed...)
	for i, edge := range observed {
		prior, ok := activeByKey[edgeSortKey(edge)]
		if ok && !edgeSemanticSourcesAnswered(prior, states) {
			observed[i] = withTopologySemantics(edge, prior)
		}
	}
	changes, err := ReconcileIntervals(active, observed, at, false)
	if err != nil {
		return IntervalChanges{}, err
	}
	seen := map[string]bool{}
	for _, edge := range observed {
		seen[edgeSortKey(edge)] = true
	}
	for _, edge := range active {
		if seen[edgeSortKey(edge)] || !edgeSourcesAnswered(edge, states) {
			continue
		}
		closedAt := at
		edge.ValidTo = &closedAt
		changes.Close = append(changes.Close, edge)
	}
	sort.Slice(changes.Close, func(i, j int) bool {
		return edgeSortKey(changes.Close[i]) < edgeSortKey(changes.Close[j])
	})
	changes, err = reconcileFleetCompetingParents(active, changes, at)
	if err != nil {
		return IntervalChanges{}, err
	}
	return changes, nil
}

func reconcileFleetCompetingParents(active []model.TopologyEdge, changes IntervalChanges,
	at int64) (IntervalChanges, error) {
	closed := map[int64]bool{}
	for _, edge := range changes.Close {
		closed[edge.ID] = true
	}
	future := map[string]model.TopologyEdge{}
	prior := map[string]model.TopologyEdge{}
	for _, edge := range active {
		key := edgeSortKey(edge)
		prior[key] = edge
		if !closed[edge.ID] {
			future[key] = edge
		}
	}
	for _, edge := range changes.Upsert {
		future[edgeSortKey(edge)] = edge
	}
	observedKeys := make(map[string]bool, len(changes.Upsert))
	for _, edge := range changes.Upsert {
		observedKeys[edgeSortKey(edge)] = true
	}
	edges := make([]model.TopologyEdge, 0, len(future))
	for _, edge := range future {
		edges = append(edges, edge)
	}
	contradicted := contradictedAggregateEdgeKeys(edges)
	for key := range unrootedAggregateEdgeKeys(edges) {
		contradicted[key] = true
	}
	for key := range unrootedManagedDeviceLLDPEdgeKeys(edges) {
		contradicted[key] = true
	}
	for key := range reciprocalManagedDeviceEdgeKeys(edges) {
		contradicted[key] = true
	}
	for key := range contradicted {
		edge := future[key]
		delete(future, key)
		if edge.ID == 0 || closed[edge.ID] {
			continue
		}
		closedAt := at
		edge.ValidTo = &closedAt
		changes.Close = append(changes.Close, edge)
		closed[edge.ID] = true
	}
	parents := map[string]map[string]bool{}
	for _, edge := range future {
		if edge.ParentNode == InternetNode {
			continue
		}
		if parents[edge.ChildNode] == nil {
			parents[edge.ChildNode] = map[string]bool{}
		}
		parents[edge.ChildNode][edge.ParentNode+"\x00"+edge.ParentPort] = true
	}
	for key, edge := range future {
		other := make([]string, 0, len(edge.Ambiguities))
		hadCompetition := false
		for _, ambiguity := range edge.Ambiguities {
			if ambiguity == competingParentsAmbiguity {
				hadCompetition = true
				continue
			}
			other = append(other, ambiguity)
		}
		edge.Ambiguities = other
		if len(parents[edge.ChildNode]) > 1 {
			edge.Ambiguities = append(edge.Ambiguities, competingParentsAmbiguity)
			edge.Confidence = "ambiguous"
		} else if hadCompetition && len(edge.Ambiguities) == 0 && edge.Confidence == "ambiguous" {
			edge.Confidence = "measured"
		}
		sort.Strings(edge.Ambiguities)
		future[key] = edge
	}
	upserts := make([]model.TopologyEdge, 0, len(future))
	for key, edge := range future {
		old, existed := prior[key]
		wasObserved := observedKeys[key]
		split := false
		if existed && !closed[old.ID] {
			same, err := sameTopologySemantics(old, edge)
			if err != nil {
				return IntervalChanges{}, err
			}
			if !same {
				closedAt := at
				old.ValidTo = &closedAt
				changes.Close = append(changes.Close, old)
				closed[old.ID] = true
				edge.ID = 0
				edge.ValidFrom = at
				edge.ValidTo = nil
				edge.LastSeen = at
				split = true
			}
		}
		if wasObserved || !existed || split {
			upserts = append(upserts, edge)
		}
	}
	sort.Slice(upserts, func(i, j int) bool { return edgeSortKey(upserts[i]) < edgeSortKey(upserts[j]) })
	sort.Slice(changes.Close, func(i, j int) bool {
		return edgeSortKey(changes.Close[i]) < edgeSortKey(changes.Close[j])
	})
	changes.Upsert = upserts
	return changes, nil
}

// A managed-device LLDP observation has no inherent direction. Admit it only
// when its claimed parent already has a path to the proven Internet root. This
// withholds an AP's startup view of its upstream router until the gateway poll
// establishes the hierarchy, while retaining rooted downstream links.
func unrootedManagedDeviceLLDPEdgeKeys(edges []model.TopologyEdge) map[string]bool {
	depth := managedDeviceRootDepths(edges)
	unrooted := map[string]bool{}
	for _, edge := range edges {
		_, parentRooted := depth[edge.ParentNode]
		if edge.ID == 0 && strings.HasPrefix(edge.ChildNode, "device:") &&
			strings.HasPrefix(edge.ParentNode, "device:") && edgeHasLLDP(edge) &&
			!parentRooted {
			unrooted[edgeSortKey(edge)] = true
		}
	}
	return unrooted
}

func managedDeviceRootDepths(edges []model.TopologyEdge) map[string]int {
	depth := map[string]int{}
	for _, edge := range edges {
		if strings.HasPrefix(edge.ChildNode, "device:") && edge.ParentNode == InternetNode {
			depth[edge.ChildNode] = 0
		}
	}
	for changed := true; changed; {
		changed = false
		for _, edge := range edges {
			if !strings.HasPrefix(edge.ChildNode, "device:") ||
				!strings.HasPrefix(edge.ParentNode, "device:") {
				continue
			}
			parentDepth, rooted := depth[edge.ParentNode]
			childDepth, seen := depth[edge.ChildNode]
			if !rooted || seen && childDepth <= parentDepth+1 {
				continue
			}
			depth[edge.ChildNode], changed = parentDepth+1, true
		}
	}
	return depth
}

func edgeHasLLDP(edge model.TopologyEdge) bool {
	for _, evidence := range edge.Evidence {
		if evidence.Kind == "lldp_neighbor" && evidence.Source == SourceLLDP {
			return true
		}
	}
	return false
}

// LLDP reports the same physical link from both ends. A parent-child graph may
// retain only one direction: the device with a proven Internet/default-route
// attachment is the parent. Without exactly one such root, withholding both
// directions is safer than inventing a hierarchy from poll order or timestamps.
func reciprocalManagedDeviceEdgeKeys(edges []model.TopologyEdge) map[string]bool {
	type direction struct{ child, parent string }
	directions := map[direction][]model.TopologyEdge{}
	for _, edge := range edges {
		if strings.HasPrefix(edge.ChildNode, "device:") &&
			strings.HasPrefix(edge.ParentNode, "device:") && edgeHasLLDP(edge) {
			key := direction{edge.ChildNode, edge.ParentNode}
			directions[key] = append(directions[key], edge)
		}
	}
	depth := managedDeviceRootDepths(edges)

	contradicted := map[string]bool{}
	seen := map[direction]bool{}
	for key, forward := range directions {
		reverseKey := direction{key.parent, key.child}
		reverse := directions[reverseKey]
		if len(reverse) == 0 || seen[key] || seen[reverseKey] {
			continue
		}
		seen[key], seen[reverseKey] = true, true
		childDepth, childRooted := depth[key.child]
		parentDepth, parentRooted := depth[key.parent]
		suppressForward := !childRooted || !parentRooted || parentDepth >= childDepth
		suppressReverse := !childRooted || !parentRooted || childDepth >= parentDepth
		for _, edge := range forward {
			if suppressForward {
				contradicted[edgeSortKey(edge)] = true
			}
		}
		for _, edge := range reverse {
			if suppressReverse {
				contradicted[edgeSortKey(edge)] = true
			}
		}
	}
	return contradicted
}

// A legacy switch CPU/VLAN port sees traffic from both sides. Until the device
// carrying that aggregate port has a proven place in the fleet, treating those
// MACs as its children can invert the whole graph. Keep the evidence withheld;
// a later physical, wireless/mesh, or Internet placement makes it usable.
func unrootedAggregateEdgeKeys(edges []model.TopologyEdge) map[string]bool {
	rooted := map[string]bool{}
	for _, edge := range edges {
		if !strings.HasPrefix(edge.ChildNode, "device:") {
			continue
		}
		if edge.ParentNode == InternetNode || edgePortAttachment(edge) == PortAttachmentPhysical ||
			edge.Medium == "wireless" || edge.Medium == "mesh" {
			rooted[edge.ChildNode] = true
		}
	}
	unrooted := map[string]bool{}
	for _, edge := range edges {
		if edgePortAttachment(edge) == PortAttachmentAggregate && !rooted[edge.ParentNode] {
			unrooted[edgeSortKey(edge)] = true
		}
	}
	return unrooted
}

// sameTopologySemantics deliberately ignores interval identity and observation
// time. JSON normalises numbers restored from SQLite (float64) and freshly
// inferred integers to the same representation, avoiding a split on every poll.
func sameTopologySemantics(left, right model.TopologyEdge) (bool, error) {
	type semanticEdge struct {
		ChildMAC     string
		ParentDevice *int64
		Confidence   string
		Evidence     []model.TopologyEvidence
		Ambiguities  []string
	}
	normalize := func(edge model.TopologyEdge) semanticEdge {
		evidence := edge.Evidence
		if evidence == nil {
			evidence = []model.TopologyEvidence{}
		}
		ambiguities := edge.Ambiguities
		if ambiguities == nil {
			ambiguities = []string{}
		}
		return semanticEdge{
			ChildMAC: edge.ChildMAC, ParentDevice: edge.ParentDeviceID,
			Confidence: edge.Confidence, Evidence: evidence, Ambiguities: ambiguities,
		}
	}
	a, err := json.Marshal(normalize(left))
	if err != nil {
		return false, fmt.Errorf("topology: encode prior edge semantics: %w", err)
	}
	b, err := json.Marshal(normalize(right))
	if err != nil {
		return false, fmt.Errorf("topology: encode observed edge semantics: %w", err)
	}
	return string(a) == string(b), nil
}

func withTopologySemantics(edge, prior model.TopologyEdge) model.TopologyEdge {
	edge.ChildMAC = prior.ChildMAC
	edge.ParentDeviceID = prior.ParentDeviceID
	edge.Confidence = prior.Confidence
	edge.Evidence = append([]model.TopologyEvidence(nil), prior.Evidence...)
	edge.Ambiguities = append([]string(nil), prior.Ambiguities...)
	return edge
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func edgeSourcesAnswered(edge model.TopologyEdge, states map[string]model.TopologySourceObservation) bool {
	required := map[string]bool{}
	for _, evidence := range edge.Evidence {
		switch evidence.Source {
		case SourceBridgeFDB, SourceAssociations, SourceLLDP, SourceDefaultRoute:
			if evidence.DeviceID == nil {
				return false
			}
			required[sourceStateKey(*evidence.DeviceID, evidence.Source)] = true
		}
	}
	if len(required) == 0 {
		return false
	}
	for key := range required {
		state := states[key]
		if state.ObservedAt <= edge.LastSeen ||
			(state.State != model.TopologySourceObserved && state.State != model.TopologySourceEmpty) {
			return false
		}
	}
	return true
}

func edgeSemanticSourcesAnswered(edge model.TopologyEdge,
	states map[string]model.TopologySourceObservation) bool {
	required := map[string]bool{}
	for _, evidence := range edge.Evidence {
		if evidence.Source == "" || evidence.DeviceID == nil {
			return false
		}
		required[sourceStateKey(*evidence.DeviceID, evidence.Source)] = true
	}
	if len(required) == 0 {
		return false
	}
	for key := range required {
		state := states[key]
		if state.ObservedAt <= edge.LastSeen ||
			(state.State != model.TopologySourceObserved && state.State != model.TopologySourceEmpty) {
			return false
		}
	}
	return true
}

func sourceStateKey(deviceID int64, source string) string {
	return strconv.FormatInt(deviceID, 10) + "\x00" + source
}
