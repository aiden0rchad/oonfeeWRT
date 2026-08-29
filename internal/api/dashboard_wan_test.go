package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
)

func seedDashboardGateway(t *testing.T, h *harness, name, routeInterface string,
	now, edgeSeen time.Time) *store.Device {
	t.Helper()
	recent := now.Add(-10 * time.Second).Unix()
	gateway := h.seedDevice(name, true, &recent)
	gateway.Role = "gateway"
	gateway.Functions = []string{"gateway"}
	if err := h.db.UpsertDevice(context.Background(), gateway); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SaveTopologySourceState(context.Background(), model.TopologySourceObservation{
		DeviceID: gateway.ID, Source: topology.SourceDefaultRoute,
		State: model.TopologySourceObserved, ObservedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	edge := model.TopologyEdge{
		ChildNode: "device:" + gateway.MAC, ParentNode: topology.InternetNode,
		ParentPort: routeInterface, Medium: "uplink", Confidence: "measured",
		ValidFrom: edgeSeen.Add(-time.Minute).UnixMilli(), LastSeen: edgeSeen.UnixMilli(),
		Evidence: []model.TopologyEvidence{{
			Kind: "default_route", Source: topology.SourceDefaultRoute,
			DeviceID: &gateway.ID, Detail: map[string]any{"interface": routeInterface, "active": true},
		}},
	}
	if err := h.db.SaveTopologyEdge(context.Background(), &edge); err != nil {
		t.Fatal(err)
	}
	return gateway
}

func readDashboard(t *testing.T, h *harness) dashboard {
	t.Helper()
	w := h.do(http.MethodGet, "/api/v1/dashboard", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	var out dashboard
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDashboardWANUsesObservedGatewayAndReturnsBoundedNullSafeSeries(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	gateway := seedDashboardGateway(t, h, "routing-gateway", "wan0", now, now)

	to := now.Truncate(telemetry.DefaultWindow)
	latest := to.Add(-telemetry.DefaultWindow).Unix()
	prior := to.Add(-2 * telemetry.DefaultWindow).Unix()
	if err := h.db.WriteRollups(context.Background(), []store.RollupRow{
		{DeviceID: gateway.ID, Kind: string(telemetry.KindIfaceRx), Key: "wan0", TS: prior, Avg: 125, Min: 100, Max: 150, Cnt: 5},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindIfaceTx), Key: "wan0", TS: latest, Avg: 75, Min: 50, Max: 100, Cnt: 5},
		// A real series on the same gateway is not the WAN merely because it exists.
		{DeviceID: gateway.ID, Kind: string(telemetry.KindIfaceRx), Key: "br-lan", TS: latest, Avg: 9999, Cnt: 5},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANLatency), Key: "", TS: latest, Avg: 8, Cnt: 5},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANLoss), Key: "", TS: latest, Avg: 0.5, Cnt: 5},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANUp), Key: "", TS: latest, Avg: 1, Cnt: 5},
	}); err != nil {
		t.Fatal(err)
	}

	wan := readDashboard(t, h).WAN
	if wan.Target != "1.1.1.1" || wan.Probe != "icmp" || wan.Freshness != "fresh" {
		t.Fatalf("WAN identity/freshness = %+v", wan)
	}
	if wan.Gateway == nil || wan.Gateway.DeviceID != gateway.ID ||
		wan.Gateway.RouteInterface != "wan0" || wan.Gateway.SeriesKey == nil ||
		*wan.Gateway.SeriesKey != "wan0" {
		t.Fatalf("selected gateway = %+v", wan.Gateway)
	}
	if wan.Resolution != "5m" || wan.BucketMS != int64((5*time.Minute)/time.Millisecond) ||
		wan.To-wan.From != int64((6*time.Hour)/time.Millisecond) {
		t.Fatalf("WAN range = resolution %q bucket %d [%d,%d)",
			wan.Resolution, wan.BucketMS, wan.From, wan.To)
	}
	for name, metric := range map[string]dashboardWANMetric{
		"download": wan.Metrics.Download, "upload": wan.Metrics.Upload,
		"latency": wan.Metrics.Latency, "loss": wan.Metrics.Loss,
		"reachable": wan.Metrics.Reachable,
	} {
		if len(metric.Points) != 72 {
			t.Errorf("%s has %d points, want 72", name, len(metric.Points))
		}
		if metric.Points[0].TS != wan.From || metric.Points[0].Value != nil {
			t.Errorf("%s first explicit gap = %+v", name, metric.Points[0])
		}
	}
	if wan.Metrics.Download.Value == nil || *wan.Metrics.Download.Value != 125 ||
		wan.Metrics.Upload.Value == nil || *wan.Metrics.Upload.Value != 75 {
		t.Fatalf("throughput values = download %v upload %v",
			wan.Metrics.Download.Value, wan.Metrics.Upload.Value)
	}
	if !strings.Contains(wan.Metrics.Download.Meaning, "received") ||
		!strings.Contains(wan.Metrics.Upload.Meaning, "transmitted") ||
		!strings.Contains(wan.Metrics.Upload.Meaning, "upstream") {
		t.Fatalf("direction semantics are unclear: %q / %q",
			wan.Metrics.Download.Meaning, wan.Metrics.Upload.Meaning)
	}
	if wan.Metrics.Reachable.Value == nil || *wan.Metrics.Reachable.Value != 1 ||
		wan.AsOf == nil || *wan.AsOf != to.UnixMilli() {
		t.Fatalf("reachability/as-of = value %v as_of %v", wan.Metrics.Reachable.Value, wan.AsOf)
	}
}

func TestDeviceDetailUsesProvedPPPoEL3DeviceForThroughput(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	gateway := seedDashboardGateway(t, h, "pppoe-gateway", "pppoe-wan", now, now)
	if err := h.db.WriteRollups(context.Background(), []store.RollupRow{{
		DeviceID: gateway.ID, Kind: string(telemetry.KindIfaceRx), Key: "pppoe-wan",
		TS: now.Truncate(time.Hour).Unix(), Avg: 100, Cnt: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/devices/"+strconv.FormatInt(gateway.ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d %s", w.Code, w.Body.String())
	}
	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.WANInterface == nil || *detail.WANInterface != "pppoe-wan" {
		t.Fatalf("wan_interface=%v", detail.WANInterface)
	}
}

func TestDeviceDetailSerializesExplicitWANAbsence(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("access-point", true, nil)
	w := h.do(http.MethodGet, "/api/v1/devices/"+strconv.FormatInt(device.ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	value, ok := body["wan_interface"]
	if !ok || string(value) != "null" {
		t.Fatalf("wan_interface=%s present=%v", value, ok)
	}
}

func TestDashboardWANNeverGuessesInterfaceKeyAndRetainsStaleObservation(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	gateway := seedDashboardGateway(t, h, "gateway-two", "logical-wan", now, now)

	stale := now.Truncate(telemetry.DefaultWindow).Add(-2 * time.Hour)
	if err := h.db.WriteRollups(context.Background(), []store.RollupRow{
		// eth1 may be the physical uplink, but no durable evidence maps it to the
		// observed logical route interface. The API must leave the key unknown.
		{DeviceID: gateway.ID, Kind: string(telemetry.KindIfaceRx), Key: "eth1", TS: stale.Unix(), Avg: 9000, Cnt: 5},
		{DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANUp), Key: "", TS: stale.Unix(), Avg: 0, Cnt: 5},
	}); err != nil {
		t.Fatal(err)
	}

	wan := readDashboard(t, h).WAN
	if wan.Gateway == nil || wan.Gateway.RouteInterface != "logical-wan" || wan.Gateway.SeriesKey != nil {
		t.Fatalf("unmapped gateway = %+v", wan.Gateway)
	}
	if wan.Metrics.Download.Status != "unavailable" || wan.Metrics.Download.Value != nil {
		t.Fatalf("guessed download metric = %+v", wan.Metrics.Download)
	}
	if wan.Freshness != "last_observed" || wan.Metrics.Reachable.Status != "last_observed" ||
		wan.Metrics.Reachable.Value == nil || *wan.Metrics.Reachable.Value != 0 {
		t.Fatalf("stale measured-down state was lost: %+v", wan.Metrics.Reachable)
	}
}

func TestDashboardWANServerSelectsNewestCurrentRoutingEvidence(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	seedDashboardGateway(t, h, "older-routing-gateway", "wan-old", now, now.Add(-time.Minute))
	newer := seedDashboardGateway(t, h, "newer-gateway", "wan-new", now, now)

	wan := readDashboard(t, h).WAN
	if wan.Gateway == nil || wan.Gateway.DeviceID != newer.ID ||
		wan.Gateway.RouteInterface != "wan-new" {
		t.Fatalf("server-selected gateway = %+v, want newest device %d", wan.Gateway, newer.ID)
	}
}

func TestDashboardWANWithoutCurrentRouteEvidenceIsExplicitlyUnavailable(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	recent := now.Add(-10 * time.Second).Unix()
	gateway := h.seedDevice("gateway-no-route", true, &recent)
	gateway.Role, gateway.Functions = "gateway", []string{"gateway"}
	if err := h.db.UpsertDevice(context.Background(), gateway); err != nil {
		t.Fatal(err)
	}
	if err := h.db.WriteRollups(context.Background(), []store.RollupRow{{
		DeviceID: gateway.ID, Kind: string(telemetry.KindSiteWANUp), Key: "",
		TS: now.Truncate(telemetry.DefaultWindow).Add(-telemetry.DefaultWindow).Unix(), Avg: 1, Cnt: 5,
	}}); err != nil {
		t.Fatal(err)
	}

	wan := readDashboard(t, h).WAN
	if wan.Gateway != nil || wan.Freshness != "unavailable" || wan.AsOf != nil ||
		wan.Metrics.Reachable.Value != nil {
		t.Fatalf("telemetry without current route evidence selected a gateway: %+v", wan)
	}
}

func TestDashboardWANDoesNotCallAStaleRouteEdgeActive(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	seedDashboardGateway(t, h, "gateway-stale-route", "wan", now,
		now.Add(-2*maxCurrentTopologySourceAge))

	dashboard := readDashboard(t, h)
	if dashboard.WAN.Gateway != nil || len(dashboard.GatewayUplinks) != 1 ||
		dashboard.GatewayUplinks[0].State != "missing" {
		t.Fatalf("stale route was presented as current: wan=%+v uplinks=%+v",
			dashboard.WAN.Gateway, dashboard.GatewayUplinks)
	}
}
