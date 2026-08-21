package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

func observabilityDevice(t *testing.T, h *harness, id int64, mac, name string,
	functions []string, now int64) *store.Device {
	t.Helper()
	adopted := now
	device := &store.Device{
		ID: id, MAC: mac, Host: fmt.Sprintf("192.0.2.%d", id), Name: name,
		Role: functions[0], Functions: functions, AdoptedAt: &adopted,
	}
	if err := h.db.UpsertDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	return device
}

func TestClientObservabilityJoinsAlignedHealthEventsAndPathsWithoutInventingRawData(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	gateway := observabilityDevice(t, h, 1, "aa:bb:cc:00:00:01", "Gateway",
		[]string{"gateway", "ap", "switch"}, base/1000)
	ap := observabilityDevice(t, h, 2, "aa:bb:cc:00:00:02", "Hall AP",
		[]string{"ap", "switch"}, base/1000)
	unrelated := observabilityDevice(t, h, 3, "aa:bb:cc:00:00:03", "Unrelated AP",
		[]string{"ap", "switch"}, base/1000)
	mac := "02:00:00:00:00:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{
		MAC: mac, Name: "Laptop", IPv4: "192.0.2.44", Scope: store.ScopeLocal,
	}}, base/1000); err != nil {
		t.Fatal(err)
	}
	sec := base / 1000
	rollups := []store.RollupRow{
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaRSSI), Key: mac, TS: sec, Avg: -60, Min: -62, Max: -58, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaRetryDelta), Key: mac, TS: sec, Avg: 5, Min: 4, Max: 6, Cnt: 4},
		// wifi-v1 must be null above: TX failure is absent. All three inputs
		// exist here, so exactly this bucket gets a score.
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaRSSI), Key: mac, TS: sec + 300, Avg: -50, Min: -52, Max: -49, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaRetryDelta), Key: mac, TS: sec + 300, Avg: 10, Min: 8, Max: 12, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaTXFailDelta), Key: mac, TS: sec + 300, Avg: 5, Min: 4, Max: 7, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaExperienceWiFiV1), Key: mac, TS: sec + 300, Avg: 95.5, Min: 94, Max: 97, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindLoad1), Key: "", TS: sec + 300, Avg: .4, Min: .3, Max: .5, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindMemPct), Key: "", TS: sec + 300, Avg: 42, Min: 41, Max: 43, Cnt: 4},
		{DeviceID: ap.ID, Kind: string(telemetry.KindRadioUtilization), Key: "radio0", TS: sec + 300, Avg: 37, Min: 30, Max: 45, Cnt: 4},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANLatency), Key: "", TS: sec + 300, Avg: 8, Min: 6, Max: 12, Cnt: 4},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANLoss), Key: "", TS: sec + 300, Avg: 0, Min: 0, Max: 0, Cnt: 4},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANUp), Key: "", TS: sec + 300, Avg: 1, Min: 1, Max: 1, Cnt: 4},
	}
	if err := h.db.WriteRollups(ctx, rollups); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.AppendEvent(ctx, store.Event{
		TS: sec + 60, DeviceID: &ap.ID, Category: "client", Severity: "info",
		Event: "associated", ClientMAC: mac, Action: "connect", InIface: "phy0-ap0",
		Source: "openwrt-log", SourceID: "77", SourceBoot: "boot:logd:1",
		Detail: map[string]any{"interface": "phy0-ap0"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, incident := range []store.Event{
		{TS: sec + 120, DeviceID: &ap.ID, Category: "device", Severity: "warning",
			Event: "device.unreachable", Detail: map[string]any{"reason": "poll failed"}},
		{TS: sec + 121, DeviceID: &unrelated.ID, Category: "device", Severity: "warning",
			Event: "device.unreachable"},
	} {
		if err := h.db.LogEvent(ctx, incident); err != nil {
			t.Fatal(err)
		}
	}

	clientEnd := base + 15*60*1000
	gatewayNode := "device:aa:bb:cc:00:00:01"
	apNode := "device:aa:bb:cc:00:00:02"
	for _, edge := range []*model.TopologyEdge{
		{ChildNode: "client:" + mac, ChildMAC: mac, ParentNode: apNode,
			ParentDeviceID: &ap.ID, Medium: "wireless", Confidence: "measured",
			ValidFrom: base, LastSeen: clientEnd},
		{ChildNode: apNode, ChildMAC: ap.MAC, ParentNode: gatewayNode,
			ParentDeviceID: &gateway.ID, ParentPort: "lan2", Medium: "wired",
			Confidence: "measured", ValidFrom: base, LastSeen: clientEnd},
		{ChildNode: gatewayNode, ChildMAC: gateway.MAC, ParentNode: "synthetic:internet",
			Medium: "uplink", Confidence: "measured", ValidFrom: base, LastSeen: clientEnd},
	} {
		if err := h.db.SaveTopologyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	for _, deviceID := range []int64{gateway.ID, ap.ID} {
		if err := h.db.SaveTopologySourceState(ctx, model.TopologySourceObservation{
			DeviceID: deviceID, Source: "associations", State: model.TopologySourceObserved,
			ObservedAt: base,
		}); err != nil {
			t.Fatal(err)
		}
	}

	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(clientEnd, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Resolution != "5m" || got.BucketMS != 300_000 || len(got.Timestamps) != 3 ||
		got.DataContract.RawSamplesPersisted || got.DataContract.MetricSource != "rollup_5m" {
		t.Fatalf("timeline/data contract = %+v", got)
	}
	if got.Formula.Name != telemetry.ExperienceFormula || got.Formula.Weights["rssi"] != .45 ||
		got.Formula.MissingPolicy == "" {
		t.Fatalf("formula = %+v", got.Formula)
	}
	experience := metricByID(t, got.Metrics, "client:"+string(telemetry.KindStaExperienceWiFiV1))
	if experience.Values[0] != nil || experience.Values[1] == nil || experience.Values[2] != nil ||
		mathAbs(*experience.Values[1]-95.5) > .001 || experience.Available.State != "partial" {
		t.Fatalf("experience = %+v", experience)
	}
	signal := metricByID(t, got.Metrics, "client:"+string(telemetry.KindStaRSSI))
	if len(signal.Mins) != len(got.Timestamps) || len(signal.Maxs) != len(got.Timestamps) ||
		len(signal.Counts) != len(got.Timestamps) || signal.Mins[0] == nil ||
		signal.Maxs[0] == nil || signal.Counts[0] == nil || *signal.Mins[0] != -62 ||
		*signal.Maxs[0] != -58 || *signal.Counts[0] != 4 {
		t.Fatalf("signal rollup envelope = %+v", signal)
	}
	for _, id := range []string{
		fmt.Sprintf("ap:%d:%s", ap.ID, telemetry.KindLoad1),
		fmt.Sprintf("ap:%d:%s:radio0", ap.ID, telemetry.KindRadioUtilization),
		fmt.Sprintf("site:%d:%s", gateway.ID, telemetry.KindSiteWANLatency),
	} {
		metric := metricByID(t, got.Metrics, id)
		if len(metric.Values) != len(got.Timestamps) || metric.Values[1] == nil {
			t.Fatalf("unaligned metric %s = %+v", id, metric)
		}
	}
	if got := metricByID(t, got.Metrics,
		fmt.Sprintf("site:%d:%s", gateway.ID, telemetry.KindSiteWANLatency)); got.Label != "ICMP latency to 1.1.1.1" {
		t.Fatalf("site probe label=%q", got.Label)
	}
	if len(got.Events) != 2 || got.Events[0].Source != "openwrt-log" ||
		got.Events[0].SourceID != "77" || got.Events[0].SourceBoot != "boot:logd:1" ||
		got.Events[0].Action != "connect" || got.Events[0].InIface != "phy0-ap0" ||
		got.Events[1].Event != "device.unreachable" || got.Events[1].DeviceID == nil ||
		*got.Events[1].DeviceID != ap.ID ||
		got.DataContract.EventTimeResolution != 1000 {
		t.Fatalf("events = %+v contract=%+v", got.Events, got.DataContract)
	}
	if !strings.Contains(strings.Join(got.Gaps, " | "),
		"historical router-log source coverage is unavailable") {
		t.Fatalf("historical router-log coverage gap absent: %v", got.Gaps)
	}
	if len(got.Paths) != 1 || !got.Paths[0].Complete || len(got.Paths[0].Paths) != 1 {
		t.Fatalf("paths = %+v", got.Paths)
	}
	wantNodes := []string{"client:" + mac, apNode, gatewayNode, "synthetic:internet"}
	if fmt.Sprint(got.Paths[0].Paths[0].NodeIDs) != fmt.Sprint(wantNodes) {
		t.Fatalf("path nodes=%v want=%v", got.Paths[0].Paths[0].NodeIDs, wantNodes)
	}
}

func TestClientObservabilityZeroHopPathSerializesEmptyMediums(t *testing.T) {
	intervals := buildClientPathIntervals("client:02:00:00:00:00:44", 1, 2, nil, nil)
	if len(intervals) != 1 || len(intervals[0].Paths) != 1 {
		t.Fatalf("zero-hop intervals = %+v", intervals)
	}
	if intervals[0].Paths[0].Mediums == nil || len(intervals[0].Paths[0].Mediums) != 0 {
		t.Fatalf("zero-hop mediums must encode as []: %+v", intervals[0].Paths[0].Mediums)
	}
	raw, err := json.Marshal(intervals)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"mediums":null`) {
		t.Fatalf("zero-hop path exposed nullable mediums: %s", raw)
	}
}

func TestClientObservabilityRejectsBadRangesUnknownClientsAndRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.setup()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	paths := []string{
		"/api/v1/clients/not-a-mac/observability?from=" + strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+1, 10),
		"/api/v1/clients/02:00:00:00:00:44/observability",
		"/api/v1/clients/02:00:00:00:00:44/observability?from=" + strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base, 10),
		"/api/v1/clients/02:00:00:00:00:44/observability?from=" + strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+32*24*60*60*1000, 10),
	}
	for _, path := range paths {
		if got := h.do(http.MethodGet, path, nil); got.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d want 400: %s", path, got.Code, got.Body.String())
		}
	}
	unknown := "/api/v1/clients/02:00:00:00:00:44/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+300_000, 10)
	if got := h.do(http.MethodGet, unknown, nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown: got %d want 404: %s", got.Code, got.Body.String())
	}
	h.cookies, h.csrf = nil, ""
	if got := h.do(http.MethodGet, unknown, nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d want 401", got.Code)
	}
}

func TestClientObservabilityBoundsNearMaxInt64WithoutOverflow(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	mac := "02:00:00:00:00:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{
		MAC: mac, Scope: store.ScopeLocal,
	}}, time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).Unix()); err != nil {
		t.Fatal(err)
	}
	from := int64(math.MaxInt64 - 807)
	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(from, 10) + "&to=" + strconv.FormatInt(math.MaxInt64, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.From != from || got.To != math.MaxInt64 || len(got.Timestamps) != 0 {
		t.Fatalf("unbounded/unaligned response: from=%d to=%d timestamps=%v",
			got.From, got.To, got.Timestamps)
	}
	for _, metric := range got.Metrics {
		if len(metric.Values) != 0 || metric.Available.Expected != 0 {
			t.Fatalf("metric %q invented a bucket near MaxInt64: %+v", metric.ID, metric)
		}
	}
}

func TestClientObservabilityHistoricalCoverageDoesNotBorrowCurrentSourceError(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	device := observabilityDevice(t, h, 1, "aa:bb:cc:00:00:01", "AP",
		[]string{"ap"}, base/1000)
	mac := "02:00:00:00:00:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{MAC: mac, Scope: store.ScopeLocal}},
		base/1000); err != nil {
		t.Fatal(err)
	}
	const currentFailure = "current LLDP failure after the requested interval"
	if err := h.db.SaveTopologySourceState(ctx, model.TopologySourceObservation{
		DeviceID: device.ID, Source: "lldp", State: model.TopologySourceError,
		Reason: currentFailure, ObservedAt: base + time.Hour.Milliseconds(),
	}); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+5*60*1000, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	gaps := strings.Join(got.Gaps, " | ")
	if !strings.Contains(gaps, "historical topology source coverage is unavailable") ||
		strings.Contains(gaps, currentFailure) {
		t.Fatalf("historical coverage leaked current state: %v", got.Gaps)
	}
}

func TestClientObservabilityDisconnectedPathEncodesArrays(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC).UnixMilli()
	mac := "02:00:00:00:00:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{MAC: mac, Scope: store.ScopeLocal}},
		base/1000); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+5*60*1000, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, `"node_ids":null`) ||
		strings.Contains(body, `"labels":null`) || strings.Contains(body, `"mediums":null`) ||
		!strings.Contains(body, `"mediums":[]`) {
		t.Fatalf("path arrays were not encoded as arrays: %s", body)
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || len(got.Paths[0].Paths) != 1 {
		t.Fatalf("disconnected path=%+v", got.Paths)
	}
	disconnected := got.Paths[0].Paths[0]
	if disconnected.NodeIDs == nil || disconnected.Labels == nil || disconnected.Mediums == nil ||
		len(disconnected.NodeIDs) != 1 || len(disconnected.Labels) != 1 || len(disconnected.Mediums) != 0 {
		t.Fatalf("disconnected path arrays=%+v", disconnected)
	}
}

func TestClientObservabilityNeverSynthesizesExperienceOrChoosesAnAPInAnAmbiguousBucket(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC).UnixMilli()
	ap1 := observabilityDevice(t, h, 1, "aa:bb:cc:00:10:01", "AP one", []string{"ap"}, base/1000)
	ap2 := observabilityDevice(t, h, 2, "aa:bb:cc:00:10:02", "AP two", []string{"ap"}, base/1000)
	mac := "02:00:00:00:10:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{MAC: mac, Scope: store.ScopeLocal}}, base/1000); err != nil {
		t.Fatal(err)
	}
	sec := base / 1000
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		// All formula inputs were averaged in this bucket, but no persisted
		// experience sample proves they coexisted in any one observation.
		{DeviceID: ap1.ID, Kind: string(telemetry.KindStaRSSI), Key: mac, TS: sec, Avg: -50, Min: -60, Max: -45, Cnt: 2},
		{DeviceID: ap1.ID, Kind: string(telemetry.KindStaRetryDelta), Key: mac, TS: sec, Avg: 10, Min: 0, Max: 20, Cnt: 2},
		{DeviceID: ap1.ID, Kind: string(telemetry.KindStaTXFailDelta), Key: mac, TS: sec, Avg: 5, Min: 0, Max: 10, Cnt: 2},
		// Two APs report the client in the next rollup bucket. Stronger RSSI is
		// not evidence that either one alone owned the whole bucket.
		{DeviceID: ap1.ID, Kind: string(telemetry.KindStaRSSI), Key: mac, TS: sec + 300, Avg: -40, Min: -45, Max: -35, Cnt: 1},
		{DeviceID: ap2.ID, Kind: string(telemetry.KindStaRSSI), Key: mac, TS: sec + 300, Avg: -70, Min: -75, Max: -65, Cnt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+15*60*1000, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	experience := metricByID(t, got.Metrics, "client:"+string(telemetry.KindStaExperienceWiFiV1))
	if experience.Values[0] != nil {
		t.Fatalf("experience was synthesized from independent averages: %+v", experience.Values)
	}
	if got.APAt[0] == nil || *got.APAt[0] != ap1.ID || got.APAt[1] != nil {
		t.Fatalf("AP attribution=%v, want AP1 then ambiguous", got.APAt)
	}
	if !strings.Contains(strings.Join(got.Gaps, " | "), "ambiguous in 1 rollup buckets") {
		t.Fatalf("ambiguity gap absent: %v", got.Gaps)
	}
}

func TestClientObservabilityKeepsUnavailableChannelUtilizationForAttributedAP(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 12, 30, 0, 0, time.UTC).UnixMilli()
	ap := observabilityDevice(t, h, 1, "aa:bb:cc:00:20:01", "AP without surveys",
		[]string{"ap"}, base/1000)
	mac := "02:00:00:00:20:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{MAC: mac, Scope: store.ScopeLocal}},
		base/1000); err != nil {
		t.Fatal(err)
	}
	if err := h.db.WriteRollups(ctx, []store.RollupRow{{
		DeviceID: ap.ID, Kind: string(telemetry.KindStaRSSI), Key: mac,
		TS: base / 1000, Avg: -55, Min: -55, Max: -55, Cnt: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+10*60*1000, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	metric := metricByID(t, got.Metrics,
		fmt.Sprintf("ap:%d:%s", ap.ID, telemetry.KindRadioUtilization))
	if metric.DeviceID == nil || *metric.DeviceID != ap.ID || metric.Key != "" ||
		metric.Available.State != "unavailable" || metric.Available.Observed != 0 ||
		metric.Available.Expected != len(got.Timestamps) || len(metric.Values) != len(got.Timestamps) {
		t.Fatalf("unavailable utilization metric = %+v", metric)
	}
	for i, value := range metric.Values {
		if value != nil {
			t.Fatalf("unavailable utilization bucket %d = %v", i, *value)
		}
	}
	if !strings.Contains(metric.Available.Reason, "no stored stable-radio") {
		t.Fatalf("unavailable utilization reason = %q", metric.Available.Reason)
	}
	if len(metric.Available.Gaps) != 1 || metric.Available.Gaps[0].From != base ||
		metric.Available.Gaps[0].To != base+10*60*1000 {
		t.Fatalf("unavailable utilization gaps = %+v", metric.Available.Gaps)
	}
}

func TestClientObservabilityUnionsRollupsWithKnownStableRadios(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 12, 45, 0, 0, time.UTC).UnixMilli()
	ap := observabilityDevice(t, h, 1, "aa:bb:cc:00:21:01", "Dual-radio AP",
		[]string{"ap"}, base/1000)
	mac := "02:00:00:00:21:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{MAC: mac, Scope: store.ScopeLocal}},
		base/1000); err != nil {
		t.Fatal(err)
	}
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: ap.ID, Kind: string(telemetry.KindStaRSSI), Key: mac,
			TS: base / 1000, Avg: -55, Min: -55, Max: -55, Cnt: 1},
		{DeviceID: ap.ID, Kind: string(telemetry.KindRadioUtilization), Key: "radio0",
			TS: base / 1000, Avg: 31, Min: 31, Max: 31, Cnt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.Fleet = &radioFleetStub{stubFleet: h.fleet, states: map[int64][]radio.LiveState{
		ap.ID: {
			{InventoryRadio: radio.InventoryRadio{Key: "radio0"}},
			{InventoryRadio: radio.InventoryRadio{Key: "radio1"}},
		},
	}}

	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(base+10*60*1000, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	radio0 := metricByID(t, got.Metrics,
		fmt.Sprintf("ap:%d:%s:radio0", ap.ID, telemetry.KindRadioUtilization))
	if radio0.Key != "radio0" || radio0.Values[0] == nil || *radio0.Values[0] != 31 {
		t.Fatalf("stored radio metric = %+v", radio0)
	}
	radio1 := metricByID(t, got.Metrics,
		fmt.Sprintf("ap:%d:%s:radio1", ap.ID, telemetry.KindRadioUtilization))
	if radio1.Key != "radio1" || radio1.Available.State != "unavailable" ||
		radio1.Available.Observed != 0 || len(radio1.Values) != len(got.Timestamps) ||
		!strings.Contains(radio1.Available.Reason, "known radio") {
		t.Fatalf("known radio without rollups = %+v", radio1)
	}
	for i, value := range radio1.Values {
		if value != nil {
			t.Fatalf("radio1 bucket %d = %v", i, *value)
		}
	}
}

func TestClientObservabilityIncludesDeviceIncidentsOnlyWhileOnThatPath(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	base := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC).UnixMilli()
	mid, end := base+5*60*1000, base+10*60*1000
	apA := observabilityDevice(t, h, 1, "aa:bb:cc:00:30:01", "AP A", []string{"ap"}, base/1000)
	apB := observabilityDevice(t, h, 2, "aa:bb:cc:00:30:02", "AP B", []string{"ap"}, base/1000)
	mac := "02:00:00:00:30:44"
	if err := h.db.UpsertClients(ctx, []store.SeenClient{{MAC: mac, Scope: store.ScopeLocal}},
		base/1000); err != nil {
		t.Fatal(err)
	}
	for _, edge := range []*model.TopologyEdge{
		{ChildNode: "client:" + mac, ChildMAC: mac, ParentNode: "device:" + apA.MAC,
			ParentDeviceID: &apA.ID, Medium: "wireless", Confidence: "measured",
			ValidFrom: base, ValidTo: &mid, LastSeen: mid},
		{ChildNode: "client:" + mac, ChildMAC: mac, ParentNode: "device:" + apB.MAC,
			ParentDeviceID: &apB.ID, Medium: "wireless", Confidence: "measured",
			ValidFrom: mid, ValidTo: &end, LastSeen: end},
	} {
		if err := h.db.SaveTopologyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []store.Event{
		{TS: (mid + 1_000) / 1000, DeviceID: &apA.ID, Category: "device",
			Severity: "warning", Event: "device.unreachable"},
		{TS: (mid + 2_000) / 1000, DeviceID: &apB.ID, Category: "device",
			Severity: "warning", Event: "device.unreachable"},
		{TS: (mid + 3_000) / 1000, DeviceID: &apB.ID, Category: "system",
			Severity: "info", Event: "openwrt.log"},
	} {
		if err := h.db.LogEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/v1/clients/" + mac + "/observability?from=" +
		strconv.FormatInt(base, 10) + "&to=" + strconv.FormatInt(end, 10)
	w := h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var got clientObservabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].Event != "device.unreachable" ||
		got.Events[0].DeviceID == nil || *got.Events[0].DeviceID != apB.ID {
		t.Fatalf("temporally joined incidents=%+v", got.Events)
	}
}

func TestClientObservabilityPathKeepsCompetingParentsAsAlternatives(t *testing.T) {
	from, to := int64(1_787_140_800_000), int64(1_787_141_100_000)
	client := "client:02:00:00:00:00:44"
	edges := []model.TopologyEdge{
		{ID: 1, ChildNode: client, ParentNode: "device:aa:bb:cc:00:00:01",
			Medium: "wireless", Confidence: "measured", ValidFrom: from, LastSeen: to},
		{ID: 2, ChildNode: client, ParentNode: "device:aa:bb:cc:00:00:02",
			Medium: "wireless", Confidence: "ambiguous", ValidFrom: from, LastSeen: to,
			Ambiguities: []string{"concurrent association evidence"}},
	}
	intervals := buildClientPathIntervals(client, from, to, edges, nil)
	if len(intervals) != 1 || intervals[0].Complete || len(intervals[0].Paths) != 2 ||
		intervals[0].Paths[0].Confidence != "ambiguous" ||
		intervals[0].Paths[1].Confidence != "ambiguous" {
		t.Fatalf("intervals=%+v", intervals)
	}
}

func TestClientObservabilityPathIgnoresUnrelatedTopologyChurn(t *testing.T) {
	from, to := int64(1_787_140_800_000), int64(1_787_141_100_000)
	client := "client:02:00:00:00:00:44"
	edges := []model.TopologyEdge{
		{ID: 1, ChildNode: client, ParentNode: "device:aa:bb:cc:00:00:01",
			Medium: "wireless", Confidence: "measured", ValidFrom: from, LastSeen: to},
		{ID: 2, ChildNode: "device:aa:bb:cc:00:00:01", ParentNode: topology.InternetNode,
			Medium: "uplink", Confidence: "measured", ValidFrom: from, LastSeen: to},
	}
	for i := 0; i < 20_000; i++ {
		end := from + int64(i%299+1)*1_000
		edges = append(edges, model.TopologyEdge{
			ID: int64(i + 3), ChildNode: fmt.Sprintf("mac:02:00:01:%02x:%02x:%02x", i>>16, i>>8, i),
			ParentNode: "device:aa:bb:cc:00:00:99", Medium: "wired", Confidence: "measured",
			ValidFrom: from, ValidTo: &end, LastSeen: end,
		})
	}
	if got := relevantTopologyEdges(client, edges); len(got) != 2 {
		t.Fatalf("relevant edges=%d, want 2", len(got))
	}
	intervals := buildClientPathIntervals(client, from, to, edges, nil)
	if len(intervals) != 1 || !intervals[0].Complete || len(intervals[0].Paths) != 1 {
		t.Fatalf("intervals=%+v", intervals)
	}
}

func TestClientObservabilityPathCapsDenseAmbiguousGraphs(t *testing.T) {
	from, to := int64(1_787_140_800_000), int64(1_787_141_100_000)
	client := "client:02:00:00:00:00:44"
	nodes := make([]string, 12)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("device:02:00:00:00:01:%02x", i)
	}
	edges := make([]model.TopologyEdge, 0, len(nodes)*len(nodes))
	nextID := int64(1)
	add := func(child, parent string) {
		edges = append(edges, model.TopologyEdge{
			ID: nextID, ChildNode: child, ParentNode: parent,
			Medium: "unknown", Confidence: "ambiguous", ValidFrom: from, LastSeen: to,
		})
		nextID++
	}
	for _, parent := range nodes {
		add(client, parent)
	}
	for i, child := range nodes {
		add(child, topology.InternetNode)
		for j, parent := range nodes {
			if i != j {
				add(child, parent)
			}
		}
	}

	intervals := buildClientPathIntervals(client, from, to, edges, nil)
	if len(intervals) != 1 || len(intervals[0].Paths) > maxObservabilityPaths {
		t.Fatalf("dense graph returned unbounded paths: %+v", intervals)
	}
	if !strings.Contains(strings.Join(intervals[0].Gaps, " | "), "paths were truncated") {
		t.Fatalf("dense graph did not disclose truncation: %+v", intervals[0].Gaps)
	}
}

func metricByID(t *testing.T, metrics []clientMetricView, id string) clientMetricView {
	t.Helper()
	for _, metric := range metrics {
		if metric.ID == id {
			return metric
		}
	}
	t.Fatalf("metric %q absent: %+v", id, metrics)
	return clientMetricView{}
}

func mathAbs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
