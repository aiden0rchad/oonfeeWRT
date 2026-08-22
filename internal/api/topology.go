package api

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

const maxTopologyHistory = 31 * 24 * time.Hour
const maxTopologyHistoryEdges = 10_000
const maxTopologyLastKnownAge = 24 * time.Hour
const minTopologyUnixMillis int64 = 1_000_000_000_000
const maxCurrentTopologySourceAge = 31 * time.Minute

type topologyNodeView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	DeviceID  *int64 `json:"device_id,omitempty"`
	MAC       string `json:"mac,omitempty"`
	Online    *bool  `json:"online,omitempty"`
	Synthetic bool   `json:"synthetic"`
}

type topologyEdgeView struct {
	ID             int64                    `json:"id"`
	ChildID        string                   `json:"child_id"`
	ParentID       string                   `json:"parent_id"`
	ParentDeviceID *int64                   `json:"parent_device_id,omitempty"`
	ParentPort     string                   `json:"parent_port,omitempty"`
	Medium         string                   `json:"medium"`
	Confidence     string                   `json:"confidence"`
	ValidFrom      int64                    `json:"valid_from"`
	ValidTo        *int64                   `json:"valid_to,omitempty"`
	LastSeen       int64                    `json:"last_seen"`
	Evidence       []model.TopologyEvidence `json:"evidence"`
	Ambiguities    []string                 `json:"ambiguities"`
}

type topologyResponse struct {
	At        int64              `json:"at"`
	Complete  bool               `json:"complete"`
	Truncated bool               `json:"truncated"`
	Nodes     []topologyNodeView `json:"nodes"`
	Edges     []topologyEdgeView `json:"edges"`
	LastKnown []topologyEdgeView `json:"last_known_edges,omitempty"`
	Gaps      []string           `json:"gaps"`
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	values, exists := r.URL.Query()["at"]
	at := int64(0)
	if exists {
		if len(values) != 1 || values[0] == "" {
			writeErr(w, http.StatusBadRequest, "at must be one Unix-millisecond timestamp")
			return
		}
		var err error
		at, err = strconv.ParseInt(values[0], 10, 64)
		if err != nil || at < minTopologyUnixMillis {
			writeErr(w, http.StatusBadRequest, "at must be a positive Unix-millisecond timestamp")
			return
		}
	}
	edges, err := s.Store.TopologyEdgesAt(r.Context(), at)
	if handleStoreErr(w, err, "topology") {
		return
	}
	responseAt := at
	if responseAt == 0 {
		responseAt = s.now().UnixMilli()
	}
	var lastKnown []model.TopologyEdge
	if !exists {
		lastKnown, err = s.Store.LatestClosedTopologyEdgesSince(r.Context(), responseAt-maxTopologyLastKnownAge.Milliseconds())
		if handleStoreErr(w, err, "last-known topology") {
			return
		}
		activeChildren, connected := map[string]bool{}, map[string]bool{}
		for _, edge := range edges {
			activeChildren[edge.ChildNode], connected[edge.ChildNode], connected[edge.ParentNode] = true, true, true
		}
		lastKnown = slices.DeleteFunc(lastKnown, func(edge model.TopologyEdge) bool {
			return activeChildren[edge.ChildNode] || !connected[edge.ParentNode] ||
				!strings.HasPrefix(edge.ChildNode, "device:") || !strings.HasPrefix(edge.ParentNode, "device:")
		})
	}
	truncated := exists && at < s.now().Add(-maxTopologyHistory).UnixMilli()
	s.writeTopology(w, r, responseAt, responseAt, !exists, truncated, edges, lastKnown)
}

func (s *Server) handleTopologyHistory(w http.ResponseWriter, r *http.Request) {
	from, ok := topologyTimeParam(r, "from")
	if !ok {
		writeErr(w, http.StatusBadRequest, "from must be one positive Unix-millisecond timestamp")
		return
	}
	to, ok := topologyTimeParam(r, "to")
	if !ok {
		writeErr(w, http.StatusBadRequest, "to must be one positive Unix-millisecond timestamp")
		return
	}
	if to <= from {
		writeErr(w, http.StatusBadRequest, "topology history requires from before to")
		return
	}
	if to-from > maxTopologyHistory.Milliseconds() {
		writeErr(w, http.StatusBadRequest, "topology history range cannot exceed 31 days")
		return
	}
	edges, queryTruncated, err := s.Store.TopologyEdgesBetween(
		r.Context(), from, to, maxTopologyHistoryEdges)
	if handleStoreErr(w, err, "topology history") {
		return
	}
	retentionTruncated := from < s.now().Add(-maxTopologyHistory).UnixMilli()
	s.writeTopology(w, r, to, from, false, queryTruncated || retentionTruncated, edges, nil)
}

func topologyTimeParam(r *http.Request, name string) (int64, bool) {
	values, exists := r.URL.Query()[name]
	if !exists || len(values) != 1 || values[0] == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	return value, err == nil && value >= minTopologyUnixMillis
}

func (s *Server) writeTopology(w http.ResponseWriter, r *http.Request, at, coverageAt int64,
	includeLive, truncated bool, edges, lastKnown []model.TopologyEdge) {
	nodes, err := s.topologyNodes(r, edges, includeLive, at)
	if handleStoreErr(w, err, "topology nodes") {
		return
	}
	views := make([]topologyEdgeView, 0, len(edges))
	lastKnownViews := make([]topologyEdgeView, 0, len(lastKnown))
	var gaps []string
	for _, edge := range edges {
		views = append(views, topologyEdgeViewFromModel(edge))
		for _, ambiguity := range edge.Ambiguities {
			gaps = append(gaps, fmt.Sprintf("edge:%d: %s", edge.ID, ambiguity))
		}
	}
	for _, edge := range lastKnown {
		lastKnownViews = append(lastKnownViews, topologyEdgeViewFromModel(edge))
	}
	if includeLive {
		states, err := s.Store.TopologySourceStates(r.Context())
		if handleStoreErr(w, err, "topology source state") {
			return
		}
		for _, state := range states {
			switch {
			case state.State == model.TopologySourceUnknown || state.State == model.TopologySourceError:
				reason := strings.TrimSpace(state.Reason)
				if reason == "" {
					reason = string(state.State)
				}
				gaps = append(gaps, fmt.Sprintf("device:%d/%s: %s", state.DeviceID, state.Source, reason))
			case coverageAt > 0 && state.ObservedAt > coverageAt:
				gaps = append(gaps, fmt.Sprintf("device:%d/%s: source state is newer than the requested interval", state.DeviceID, state.Source))
			case state.ObservedAt < at-maxCurrentTopologySourceAge.Milliseconds():
				gaps = append(gaps, fmt.Sprintf("device:%d/%s: source state is stale", state.DeviceID, state.Source))
			}
		}
		covered := map[int64]bool{}
		for _, state := range states {
			covered[state.DeviceID] = true
		}
		for _, node := range nodes {
			if node.DeviceID != nil && !covered[*node.DeviceID] {
				gaps = append(gaps, fmt.Sprintf("device:%d: topology sources have not been observed", *node.DeviceID))
			}
		}
		if len(states) == 0 {
			gaps = append(gaps, "topology sources have not been observed")
		}
	} else {
		// Current source-state rows cannot prove coverage at a historical instant.
		gaps = append(gaps, "historical source coverage is unavailable")
	}
	if truncated {
		gaps = append(gaps, "topology history is truncated by retention or the 10000-interval response limit")
	}
	gaps = uniqueTopologyStrings(gaps)
	writeJSON(w, http.StatusOK, topologyResponse{
		At: at, Complete: len(gaps) == 0, Truncated: truncated,
		Nodes: nodes, Edges: views, LastKnown: lastKnownViews, Gaps: gaps,
	})
}

func topologyEdgeViewFromModel(edge model.TopologyEdge) topologyEdgeView {
	return topologyEdgeView{
		ID: edge.ID, ChildID: edge.ChildNode, ParentID: edge.ParentNode,
		ParentDeviceID: edge.ParentDeviceID, ParentPort: edge.ParentPort,
		Medium: edge.Medium, Confidence: edge.Confidence,
		ValidFrom: edge.ValidFrom, ValidTo: edge.ValidTo, LastSeen: edge.LastSeen,
		Evidence: edge.Evidence, Ambiguities: edge.Ambiguities,
	}
}

func (s *Server) topologyNodes(r *http.Request, edges []model.TopologyEdge, includeLive bool,
	at int64) ([]topologyNodeView, error) {
	devices, err := s.Store.Devices(r.Context())
	if err != nil {
		return nil, err
	}
	referenced := map[string]bool{}
	for _, edge := range edges {
		referenced[edge.ChildNode] = true
		referenced[edge.ParentNode] = true
	}
	clientMACs := []string{}
	for ref := range referenced {
		if strings.HasPrefix(ref, "client:") {
			clientMACs = append(clientMACs, strings.TrimPrefix(ref, "client:"))
		}
	}
	clients, err := s.Store.ClientsByMACs(r.Context(), clientMACs)
	if err != nil {
		return nil, err
	}
	nodes := map[string]topologyNodeView{}
	now := s.now()
	clientPresence := map[string]int64{}
	activeClients := map[string]bool{}
	if includeLive {
		var active []string
		clientPresence, active = s.liveClientPresence(devices, now)
		for _, mac := range active {
			activeClients[mac] = true
		}
	}
	for _, device := range devices {
		if !device.Adopted() {
			continue
		}
		mac, err := canonicalTopologyMAC(device.MAC)
		if err != nil {
			return nil, fmt.Errorf("api: topology device %d: %w", device.ID, err)
		}
		id := "device:" + mac
		if !includeLive && !referenced[id] && *device.AdoptedAt*1000 > at {
			continue
		}
		name := device.Name
		if name == "" {
			name = mac
		}
		deviceID := device.ID
		var online *bool
		if includeLive {
			status := s.viewDevice(device, now).Status
			if status == "online" || status == "offline" {
				value := status == "online"
				online = &value
			}
		}
		nodes[id] = topologyNodeView{
			ID: id, Kind: "device", Name: name, DeviceID: &deviceID,
			MAC: mac, Online: online,
		}
	}
	for _, client := range clients {
		mac, err := canonicalTopologyMAC(client.MAC)
		if err != nil {
			return nil, fmt.Errorf("api: topology client: %w", err)
		}
		id := "client:" + mac
		name := client.Name
		if name == "" {
			name = mac
		}
		var online *bool
		if includeLive {
			if activeClients[mac] {
				value := true
				online = &value
			} else if _, seen := clientPresence[mac]; seen {
				value := false
				online = &value
			}
		}
		nodes[id] = topologyNodeView{ID: id, Kind: "client", Name: name, MAC: mac, Online: online}
	}
	for ref := range referenced {
		if _, exists := nodes[ref]; exists {
			continue
		}
		kind, value, _ := strings.Cut(ref, ":")
		switch kind {
		case "device":
			nodes[ref] = topologyNodeView{ID: ref, Kind: "device", Name: value, MAC: value}
		case "client":
			nodes[ref] = topologyNodeView{ID: ref, Kind: "client", Name: value, MAC: value}
		case "mac":
			nodes[ref] = topologyNodeView{ID: ref, Kind: "synthetic", Name: "Unknown · " + value, MAC: value, Synthetic: true}
		case "synthetic":
			name := value
			if ref == "synthetic:internet" {
				name = "Internet"
			}
			nodes[ref] = topologyNodeView{ID: ref, Kind: "synthetic", Name: name, Synthetic: true}
		}
	}
	out := make([]topologyNodeView, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func canonicalTopologyMAC(raw string) (string, error) {
	mac, err := net.ParseMAC(raw)
	if err != nil || len(mac) != 6 {
		return "", fmt.Errorf("invalid 48-bit MAC address %q", raw)
	}
	return strings.ToLower(mac.String()), nil
}

func uniqueTopologyStrings(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
