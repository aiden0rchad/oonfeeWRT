package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func topologyDevice(t *testing.T, h *harness, mac, name string, seen time.Time) *store.Device {
	t.Helper()
	adopted := int64(1)
	lastSeen := seen.Unix()
	device := &store.Device{
		MAC: mac, Host: "192.0.2.1", Name: name, Role: "ap",
		AdoptedAt: &adopted, LastSeen: &lastSeen,
	}
	if err := h.db.UpsertDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	return device
}

func TestTopologyReturnsCanonicalNodesEvidenceAndExplicitGaps(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	wrt := topologyDevice(t, h, "AA:BB:CC:00:00:01", "Gateway", now)
	c6 := topologyDevice(t, h, "AA:BB:CC:00:00:02", "Hall AP", now.Add(-time.Hour))
	if err := h.db.UpsertClients(context.Background(), []store.SeenClient{{
		MAC: "aa:bb:cc:00:00:44", Name: "Laptop", Scope: store.ScopeLocal,
	}}, now.Unix()); err != nil {
		t.Fatal(err)
	}
	parentID := wrt.ID
	edge := model.TopologyEdge{
		ChildNode: "device:aa:bb:cc:00:00:02", ChildMAC: "aa:bb:cc:00:00:12",
		ParentNode: "device:aa:bb:cc:00:00:01", ParentDeviceID: &parentID,
		ParentPort: "lan2", Medium: "wired", Confidence: "ambiguous",
		ValidFrom: now.Add(-time.Minute).UnixMilli(), LastSeen: now.UnixMilli(),
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: "brctl.showmacs", DeviceID: &parentID,
			Detail: map[string]any{"bridge": "br-lan", "observed_mac": "aa:bb:cc:00:00:12"},
		}},
		Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}
	if err := h.db.SaveTopologyEdge(context.Background(), &edge); err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.TopologySourceObservation{
		{DeviceID: wrt.ID, Source: "bridge-fdb", State: model.TopologySourceObserved, ObservedAt: now.UnixMilli()},
		{DeviceID: wrt.ID, Source: "lldp", State: model.TopologySourceError, Reason: "package unavailable", ObservedAt: now.UnixMilli()},
	} {
		if err := h.db.SaveTopologySourceState(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}

	w := h.do(http.MethodGet, "/api/v1/topology", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.At != now.UnixMilli() || got.Complete {
		t.Fatalf("at/complete = %d/%v", got.At, got.Complete)
	}
	if len(got.Edges) != 1 || got.Edges[0].ChildID != "device:aa:bb:cc:00:00:02" ||
		got.Edges[0].ParentID != "device:aa:bb:cc:00:00:01" || len(got.Edges[0].Evidence) != 1 {
		t.Fatalf("edges = %#v", got.Edges)
	}
	joinedGaps := strings.Join(got.Gaps, "\n")
	if !strings.Contains(joinedGaps, "lldp: package unavailable") ||
		!strings.Contains(joinedGaps, "does not identify VLAN") {
		t.Fatalf("gaps = %#v", got.Gaps)
	}
	if !sort.StringsAreSorted(got.Gaps) {
		t.Fatalf("gaps are unstable: %#v", got.Gaps)
	}
	nodeByID := map[string]topologyNodeView{}
	for _, node := range got.Nodes {
		nodeByID[node.ID] = node
	}
	if nodeByID["device:aa:bb:cc:00:00:01"].Name != "Gateway" ||
		nodeByID["device:aa:bb:cc:00:00:02"].Name != "Hall AP" ||
		nodeByID["device:aa:bb:cc:00:00:02"].Online == nil || *nodeByID["device:aa:bb:cc:00:00:02"].Online {
		t.Fatalf("device nodes = %#v", nodeByID)
	}
	if _, exists := nodeByID["client:aa:bb:cc:00:00:44"]; exists {
		t.Fatalf("unreferenced client became a disconnected topology node: %#v", nodeByID)
	}
	if c6.ID == 0 {
		t.Fatal("test device was not persisted")
	}
}

func TestTopologyReturnsOnlyGraphReferencedClients(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	device := topologyDevice(t, h, "aa:bb:cc:00:00:01", "Gateway", now)
	seen := make([]store.SeenClient, 10_000)
	for i := range seen {
		seen[i] = store.SeenClient{MAC: fmt.Sprintf("02:00:00:00:%02x:%02x",
			byte(i>>8), byte(i)), Name: fmt.Sprintf("client-%d", i), Scope: store.ScopeLocal}
	}
	if err := h.db.UpsertClients(context.Background(), seen, now.Unix()); err != nil {
		t.Fatal(err)
	}
	parentID := device.ID
	edge := model.TopologyEdge{
		ChildNode: "client:" + seen[9999].MAC, ChildMAC: seen[9999].MAC,
		ParentNode: "device:aa:bb:cc:00:00:01", ParentDeviceID: &parentID,
		ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "measured",
		ValidFrom: now.Add(-time.Minute).UnixMilli(), LastSeen: now.UnixMilli(),
		Evidence: []model.TopologyEvidence{}, Ambiguities: []string{},
	}
	if err := h.db.SaveTopologyEdge(context.Background(), &edge); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodGet, "/api/v1/topology", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || len(got.Edges) != 1 {
		t.Fatalf("unbounded topology response: nodes=%d edges=%d", len(got.Nodes), len(got.Edges))
	}
}

func TestTopologyClientPresenceUsesLiveFleetStateNotInventory(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 17, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	device := topologyDevice(t, h, "aa:bb:cc:00:00:01", "Gateway", now)
	mac := "02:00:00:00:00:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{
		MAC: mac, Name: "Laptop", Scope: store.ScopeLocal,
	}}, now.Unix()); err != nil {
		t.Fatal(err)
	}
	parentID := device.ID
	edge := model.TopologyEdge{
		ChildNode: "client:" + mac, ChildMAC: mac,
		ParentNode: "device:aa:bb:cc:00:00:01", ParentDeviceID: &parentID,
		ParentPort: "lan1", Medium: "wired", Confidence: "measured",
		ValidFrom: now.Add(-time.Minute).UnixMilli(), LastSeen: now.UnixMilli(),
		Evidence: []model.TopologyEvidence{}, Ambiguities: []string{},
	}
	if err := h.db.SaveTopologyEdge(ctx, &edge); err != nil {
		t.Fatal(err)
	}

	clientNode := func() topologyNodeView {
		t.Helper()
		w := h.do(http.MethodGet, "/api/v1/topology", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("topology: %d %s", w.Code, w.Body.String())
		}
		var got topologyResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, node := range got.Nodes {
			if node.ID == "client:"+mac {
				return node
			}
		}
		t.Fatalf("client node missing: %#v", got.Nodes)
		return topologyNodeView{}
	}

	// A fresh durable clients.last_seen survives restart; authoritative live
	// presence does not. Current topology must say unknown, not online.
	if node := clientNode(); node.Online != nil {
		t.Fatalf("inventory-derived client state after restart: %#v", node)
	}

	h.fleet.mu.Lock()
	h.fleet.presence[device.ID] = collector.ClientPresenceState{
		Active:   collector.ClientPresence{},
		LastSeen: collector.ClientPresence{mac: now.Unix()},
	}
	h.fleet.mu.Unlock()
	if node := clientNode(); node.Online == nil || *node.Online {
		t.Fatalf("known inactive client state=%#v, want offline", node)
	}

	h.fleet.mu.Lock()
	h.fleet.presence[device.ID] = activePresence(collector.ClientPresence{mac: now.Unix()})
	h.fleet.mu.Unlock()
	if node := clientNode(); node.Online == nil || !*node.Online {
		t.Fatalf("active client state=%#v, want online", node)
	}
}

func TestTopologyAtAndHistoryUseIntervalSemantics(t *testing.T) {
	h := newHarness(t)
	h.setup()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	wrt := topologyDevice(t, h, "aa:bb:cc:00:00:01", "Gateway", time.UnixMilli(base))
	parentID := wrt.ID
	firstEnd := base + 60_000
	first := model.TopologyEdge{
		ChildNode: "mac:aa:bb:cc:00:00:99", ChildMAC: "aa:bb:cc:00:00:99",
		ParentNode: "device:aa:bb:cc:00:00:01", ParentDeviceID: &parentID,
		ParentPort: "lan1", Medium: "wired", Confidence: "measured",
		ValidFrom: base, ValidTo: &firstEnd, LastSeen: base + 59_000,
		Evidence:    []model.TopologyEvidence{{Kind: "lldp_neighbor", Source: "lldp", Detail: map[string]any{}}},
		Ambiguities: []string{},
	}
	second := first
	second.ID = 0
	second.ParentPort = "lan2"
	second.ValidFrom = firstEnd
	second.ValidTo = nil
	second.LastSeen = base + 120_000
	for _, edge := range []*model.TopologyEdge{&first, &second} {
		if err := h.db.SaveTopologyEdge(context.Background(), edge); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.db.SaveTopologySourceState(context.Background(), model.TopologySourceObservation{
		DeviceID: wrt.ID, Source: "lldp", State: model.TopologySourceObserved, ObservedAt: base,
	}); err != nil {
		t.Fatal(err)
	}

	at := h.do(http.MethodGet, "/api/v1/topology?at="+strconv.FormatInt(base+30_000, 10), nil)
	if at.Code != http.StatusOK {
		t.Fatalf("at: %d %s", at.Code, at.Body.String())
	}
	var snapshot topologyResponse
	if err := json.Unmarshal(at.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Edges) != 1 || snapshot.Edges[0].ParentPort != "lan1" || snapshot.Complete ||
		!containsTopologyString(snapshot.Gaps, "historical source coverage is unavailable") {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	for _, node := range snapshot.Nodes {
		if node.Online != nil {
			t.Fatalf("historical node leaked current online state: %#v", node)
		}
	}

	path := "/api/v1/topology/history?from=" + strconv.FormatInt(base+30_000, 10) +
		"&to=" + strconv.FormatInt(base+90_000, 10)
	history := h.do(http.MethodGet, path, nil)
	if history.Code != http.StatusOK {
		t.Fatalf("history: %d %s", history.Code, history.Body.String())
	}
	if err := json.Unmarshal(history.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Edges) != 2 || snapshot.Edges[0].ParentPort != "lan1" || snapshot.Edges[1].ParentPort != "lan2" {
		t.Fatalf("history does not contain both intersecting intervals: %#v", snapshot.Edges)
	}
	if snapshot.Complete || !containsTopologyString(snapshot.Gaps, "historical source coverage is unavailable") {
		t.Fatalf("history coverage = complete:%v gaps:%#v", snapshot.Complete, snapshot.Gaps)
	}
	unknown := snapshot.Nodes[0]
	for _, node := range snapshot.Nodes {
		if node.ID == "mac:aa:bb:cc:00:00:99" {
			unknown = node
		}
	}
	if unknown.ID != "mac:aa:bb:cc:00:00:99" || unknown.Kind != "synthetic" || !unknown.Synthetic {
		t.Fatalf("unknown node = %#v", unknown)
	}
}

func TestHistoricalTopologyDoesNotBorrowCurrentSourceError(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	device := topologyDevice(t, h, "aa:bb:cc:00:00:01", "Gateway", time.UnixMilli(base))
	const currentFailure = "current LLDP failure after the requested interval"
	if err := h.db.SaveTopologySourceState(ctx, model.TopologySourceObservation{
		DeviceID: device.ID, Source: "lldp", State: model.TopologySourceError,
		Reason: currentFailure, ObservedAt: base + time.Hour.Milliseconds(),
	}); err != nil {
		t.Fatal(err)
	}

	current := h.do(http.MethodGet, "/api/v1/topology", nil)
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), currentFailure) {
		t.Fatalf("current topology did not expose current source error: %d %s",
			current.Code, current.Body.String())
	}
	paths := []string{
		"/api/v1/topology?at=" + strconv.FormatInt(base, 10),
		"/api/v1/topology/history?from=" + strconv.FormatInt(base, 10) +
			"&to=" + strconv.FormatInt(base+time.Minute.Milliseconds(), 10),
	}
	for _, path := range paths {
		w := h.do(http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var got topologyResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		gaps := strings.Join(got.Gaps, " | ")
		if !strings.Contains(gaps, "historical source coverage is unavailable") ||
			strings.Contains(gaps, currentFailure) || strings.Contains(gaps, "source state is newer") {
			t.Fatalf("%s leaked current source state: %v", path, got.Gaps)
		}
	}
}

func TestHistoricalTopologyRosterUsesAdoptionTimeUnlessEdgeReferenced(t *testing.T) {
	h := newHarness(t)
	h.setup()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return base.Add(time.Hour) }
	pastAdoption := base.Add(-time.Minute).Unix()
	futureAdoption := base.Add(time.Minute).Unix()
	devices := []*store.Device{
		{MAC: "aa:bb:cc:00:00:01", Host: "192.0.2.1", Name: "Past device", Role: "ap", AdoptedAt: &pastAdoption},
		{MAC: "aa:bb:cc:00:00:02", Host: "192.0.2.2", Name: "Future unreferenced", Role: "ap", AdoptedAt: &futureAdoption},
		{MAC: "aa:bb:cc:00:00:03", Host: "192.0.2.3", Name: "Future referenced", Role: "ap", AdoptedAt: &futureAdoption},
	}
	for _, device := range devices {
		if err := h.db.UpsertDevice(context.Background(), device); err != nil {
			t.Fatal(err)
		}
	}
	validTo := base.Add(time.Second).UnixMilli()
	edge := &model.TopologyEdge{
		ChildNode: "device:aa:bb:cc:00:00:03", ParentNode: "synthetic:internet",
		Medium: "uplink", Confidence: "measured", ValidFrom: base.Add(-time.Second).UnixMilli(),
		ValidTo: &validTo, LastSeen: base.UnixMilli(),
		Evidence: []model.TopologyEvidence{}, Ambiguities: []string{},
	}
	if err := h.db.SaveTopologyEdge(context.Background(), edge); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/topology?at="+strconv.FormatInt(base.UnixMilli(), 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]bool{}
	for _, node := range got.Nodes {
		nodes[node.ID] = true
	}
	if !nodes["device:aa:bb:cc:00:00:01"] || !nodes["device:aa:bb:cc:00:00:03"] ||
		nodes["device:aa:bb:cc:00:00:02"] {
		t.Fatalf("historical nodes=%#v", got.Nodes)
	}
}

func TestTopologyHistoryReportsRetentionTruncation(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	from := now.Add(-maxTopologyHistory - time.Minute).UnixMilli()
	to := from + time.Minute.Milliseconds()
	w := h.do(http.MethodGet, "/api/v1/topology/history?from="+
		strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.Complete ||
		!containsTopologyString(got.Gaps,
			"topology history is truncated by retention or the 10000-interval response limit") {
		t.Fatalf("retention contract=%#v", got)
	}
}

func TestTopologyCurrentMarksStaleSourceCoverageIncomplete(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	device := topologyDevice(t, h, "aa:bb:cc:00:00:01", "Gateway", now)
	if err := h.db.SaveTopologySourceState(context.Background(), model.TopologySourceObservation{
		DeviceID: device.ID, Source: "bridge-fdb", State: model.TopologySourceObserved,
		ObservedAt: now.Add(-maxCurrentTopologySourceAge - time.Second).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodGet, "/api/v1/topology", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Complete || !containsTopologyString(got.Gaps, "device:1/bridge-fdb: source state is stale") {
		t.Fatalf("stale coverage=%+v", got)
	}
}

func TestTopologyCurrentRequiresCoverageForEveryAdoptedDevice(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	gateway := topologyDevice(t, h, "aa:bb:cc:00:00:01", "Gateway", now)
	ap := topologyDevice(t, h, "aa:bb:cc:00:00:02", "AP", now)
	if err := h.db.SaveTopologySourceState(context.Background(), model.TopologySourceObservation{
		DeviceID: gateway.ID, Source: "bridge-fdb", State: model.TopologySourceEmpty,
		ObservedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodGet, "/api/v1/topology", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("device:%d: topology sources have not been observed", ap.ID)
	if got.Complete || !containsTopologyString(got.Gaps, want) {
		t.Fatalf("missing-device coverage=%+v", got)
	}
}

func containsTopologyString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTopologyRejectsMalformedAndUnboundedTimeQueries(t *testing.T) {
	h := newHarness(t)
	h.setup()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	tests := []string{
		"/api/v1/topology?at=not-a-time",
		"/api/v1/topology?at=1787140800", // seconds, not milliseconds
		"/api/v1/topology?at=" + strconv.FormatInt(base, 10) + "&at=" + strconv.FormatInt(base+1, 10),
		"/api/v1/topology/history",
		"/api/v1/topology/history?from=" + strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base, 10),
		"/api/v1/topology/history?from=" + strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+32*24*60*60*1000, 10),
	}
	for _, path := range tests {
		if got := h.do(http.MethodGet, path, nil); got.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", path, got.Code, got.Body.String())
		}
	}
}

func TestTopologyRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies, h.csrf = nil, ""
	if got := h.do(http.MethodGet, "/api/v1/topology", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", got.Code)
	}
}
