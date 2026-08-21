package api

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	maxObservabilityPaths    = 64
	maxObservabilityPathWork = 2048
)

type observabilityPathBudget struct {
	work      int
	paths     int
	truncated bool
}

func buildClientPathIntervals(clientNode string, from, to int64,
	edges []model.TopologyEdge, devices []*store.Device) []observabilityPathInterval {
	edges = relevantTopologyEdges(clientNode, edges)
	boundaries := []int64{from, to}
	labels := map[string]string{"synthetic:internet": "Internet"}
	for _, device := range devices {
		mac, err := canonicalObservabilityMAC(device.MAC)
		if err == nil {
			labels["device:"+mac] = deviceDisplayName(device)
		}
	}
	labels[clientNode] = strings.TrimPrefix(clientNode, "client:")
	for _, edge := range edges {
		if edge.ValidFrom > from && edge.ValidFrom < to {
			boundaries = append(boundaries, edge.ValidFrom)
		}
		if edge.ValidTo != nil && *edge.ValidTo > from && *edge.ValidTo < to {
			boundaries = append(boundaries, *edge.ValidTo)
		}
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i] < boundaries[j] })
	boundaries = uniqueInt64s(boundaries)

	intervals := []observabilityPathInterval{}
	for i := 0; i+1 < len(boundaries); i++ {
		start, end := boundaries[i], boundaries[i+1]
		active := map[string][]model.TopologyEdge{}
		for _, edge := range edges {
			if edge.ValidFrom <= start && (edge.ValidTo == nil || *edge.ValidTo > start) {
				active[edge.ChildNode] = append(active[edge.ChildNode], edge)
			}
		}
		for child := range active {
			sort.Slice(active[child], func(i, j int) bool {
				if active[child][i].ParentNode != active[child][j].ParentNode {
					return active[child][i].ParentNode < active[child][j].ParentNode
				}
				return active[child][i].ID < active[child][j].ID
			})
		}
		budget := &observabilityPathBudget{}
		paths, gaps := walkObservabilityPaths(clientNode, active, labels,
			map[string]bool{}, []string{clientNode}, nil, "unknown", budget)
		if budget.truncated {
			gaps = append(gaps, "topology paths were truncated because the candidate-parent graph is too ambiguous")
		}
		complete := len(paths) > 0 && len(gaps) == 0
		for _, path := range paths {
			if len(path.NodeIDs) == 0 || path.NodeIDs[len(path.NodeIDs)-1] != "synthetic:internet" {
				complete = false
			}
		}
		interval := observabilityPathInterval{
			From: start, To: end, Complete: complete, Paths: paths,
			Gaps: uniqueTopologyStrings(gaps),
		}
		if len(interval.Paths) == 0 {
			interval.Paths = []observabilityPath{}
		}
		if len(interval.Gaps) == 0 {
			interval.Gaps = []string{}
		}
		// Changes elsewhere in the graph must not fragment this client's path.
		if len(intervals) > 0 && intervals[len(intervals)-1].To == start &&
			samePathInterval(intervals[len(intervals)-1], interval) {
			intervals[len(intervals)-1].To = end
			continue
		}
		intervals = append(intervals, interval)
	}
	return intervals
}

func relevantTopologyEdges(root string, edges []model.TopologyEdge) []model.TopologyEdge {
	byChild := make(map[string][]model.TopologyEdge)
	for _, edge := range edges {
		byChild[edge.ChildNode] = append(byChild[edge.ChildNode], edge)
	}
	seen := map[string]bool{root: true}
	queue := []string{root}
	out := make([]model.TopologyEdge, 0)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, edge := range byChild[node] {
			out = append(out, edge)
			if !seen[edge.ParentNode] {
				seen[edge.ParentNode] = true
				queue = append(queue, edge.ParentNode)
			}
		}
	}
	return out
}

func walkObservabilityPaths(node string, active map[string][]model.TopologyEdge,
	labels map[string]string, seen map[string]bool, nodes, mediums []string,
	confidence string, budget *observabilityPathBudget) ([]observabilityPath, []string) {
	if budget.work >= maxObservabilityPathWork || budget.paths >= maxObservabilityPaths {
		budget.truncated = true
		return nil, nil
	}
	budget.work++
	if node == "synthetic:internet" {
		budget.paths++
		return []observabilityPath{{
			NodeIDs: append([]string{}, nodes...), Labels: pathLabels(nodes, labels),
			Mediums: append([]string{}, mediums...), Confidence: confidence,
		}}, nil
	}
	if seen[node] {
		return nil, []string{"topology path contains a cycle at " + node}
	}
	nextSeen := cloneStringSet(seen)
	nextSeen[node] = true
	parents := active[node]
	if len(parents) == 0 {
		budget.paths++
		return []observabilityPath{{
			NodeIDs: append([]string{}, nodes...), Labels: pathLabels(nodes, labels),
			Mediums: append([]string{}, mediums...), Confidence: confidence,
		}}, []string{"topology path stops without an observed parent at " + node}
	}
	gaps := []string{}
	branchConfidence := confidence
	if len(parents) > 1 {
		gaps = append(gaps, "topology path has multiple candidate parents at "+node)
		branchConfidence = pathConfidence(branchConfidence, "ambiguous")
	}
	paths := []observabilityPath{}
	for _, edge := range parents {
		if budget.work >= maxObservabilityPathWork || budget.paths >= maxObservabilityPaths {
			budget.truncated = true
			break
		}
		nextConfidence := pathConfidence(branchConfidence, edge.Confidence)
		for _, ambiguity := range edge.Ambiguities {
			gaps = append(gaps, "edge "+strconv.FormatInt(edge.ID, 10)+": "+ambiguity)
			nextConfidence = pathConfidence(nextConfidence, "ambiguous")
		}
		branchPaths, branchGaps := walkObservabilityPaths(edge.ParentNode, active, labels,
			nextSeen, appendCopy(nodes, edge.ParentNode), appendCopy(mediums, edge.Medium), nextConfidence, budget)
		paths, gaps = append(paths, branchPaths...), append(gaps, branchGaps...)
	}
	return paths, gaps
}

func pathConfidence(current, next string) string {
	if current == "unknown" {
		return next
	}
	if current == "ambiguous" || next == "ambiguous" {
		return "ambiguous"
	}
	if current == "inferred" || next == "inferred" {
		return "inferred"
	}
	return "measured"
}

func samePathInterval(left, right observabilityPathInterval) bool {
	left.From, left.To, right.From, right.To = 0, 0, 0, 0
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}

func pathLabels(nodes []string, labels map[string]string) []string {
	out := make([]string, len(nodes))
	for i, node := range nodes {
		if label := labels[node]; label != "" {
			out[i] = label
			continue
		}
		_, value, ok := strings.Cut(node, ":")
		if !ok {
			value = node
		}
		out[i] = value
	}
	return out
}

func cloneStringSet(input map[string]bool) map[string]bool {
	out := make(map[string]bool, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func appendCopy(values []string, value string) []string {
	out := append([]string(nil), values...)
	return append(out, value)
}

func uniqueInt64s(values []int64) []int64 {
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
