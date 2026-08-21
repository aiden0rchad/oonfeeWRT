package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func seedPolicyGateway(t *testing.T, h *harness, firewall capability.State) *store.Device {
	t.Helper()
	at := int64(1)
	caps := capability.NewRegistry()
	caps.Set(capability.FeatFirewall4, firewall)
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Device{MAC: "aa:bb:cc:dd:ee:01", Host: "192.0.2.1", Name: "gateway",
		Role: string(model.RoleGateway), Functions: []string{"gateway"}, AdoptedAt: &at, CapsJSON: string(raw)}
	if err := h.db.UpsertDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPolicyAPICRUDMasterRowsAndDisplayOnlyOrder(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, iot := seedAPIZones(t, h)
	iot.Enabled = false
	if err := h.db.SaveNetwork(context.Background(), iot); err != nil {
		t.Fatal(err)
	}
	seedPolicyGateway(t, h, capability.Present)

	created := h.do(http.MethodPost, "/api/v1/site/policies", map[string]any{
		"name": "deny telemetry", "kind": "firewall_rule", "origin": "object_manager", "enabled": true,
		"firewall": map[string]any{"action": "drop", "source_zone": "guest", "destination_zone": "wan", "protocols": []string{"UDP"}},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	policy := h.json(created)
	id := int(policy["id"].(float64))
	if id == 0 || policy["order"] != float64(100) || policy["origin"] != "object_manager" {
		t.Fatalf("created policy=%v", policy)
	}

	// Omitting display order and origin on a full update preserves both.
	updated := h.do(http.MethodPost, fmt.Sprintf("/api/v1/site/policies/%d", id), map[string]any{
		"name": "deny telemetry renamed", "kind": "firewall_rule", "enabled": false,
		"firewall": map[string]any{"action": "drop", "source_zone": "guest", "destination_zone": "wan", "protocols": []string{"udp"}},
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update=%d %s", updated.Code, updated.Body.String())
	}
	policy = h.json(updated)
	if policy["order"] != float64(100) || policy["origin"] != "object_manager" {
		t.Fatalf("update lost omitted fields: %v", policy)
	}

	master := h.json(h.do(http.MethodGet, "/api/v1/site/policies", nil))
	rows := master["rows"].([]any)
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["id"] != fmt.Sprintf("policy:%d", id) {
			continue
		}
		found = true
		if row["origin"] != "object_manager" || row["order_scope"] != "display_only" || row["mutable"] != true {
			t.Fatalf("master row=%v", row)
		}
		if scope := row["effective_scope"].(map[string]any); scope["connection_scope"] != "new" ||
			scope["existing_connections"] != "may_persist_until_conntrack_expiry" {
			t.Fatalf("firewall conntrack scope=%v", scope)
		}
	}
	if !found {
		t.Fatalf("persisted row absent from %v", rows)
	}
	priorityGate := false
	for _, raw := range master["capabilities"].([]any) {
		cap := raw.(map[string]any)
		priorityGate = priorityGate || cap["kind"] == "priority" && cap["available"] == false && strings.Contains(cap["reason"].(string), "display-only")
	}
	if !priorityGate {
		t.Fatalf("missing explicit priority gate: %v", master["capabilities"])
	}

	bad := h.do(http.MethodPost, "/api/v1/site/policies", map[string]any{
		"name": "x", "kind": "firewall_rule", "enabled": true, "priority": 1,
		"firewall": map[string]any{"action": "drop", "source_zone": "guest", "destination_zone": "wan"},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d %s", bad.Code, bad.Body.String())
	}
	deleted := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/site/policies/%d", id), nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
}

func TestObjectManagerCompilesDraftsAndNeverPersistsOrApplies(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, iot := seedAPIZones(t, h)
	iot.Enabled = false
	if err := h.db.SaveNetwork(context.Background(), iot); err != nil {
		t.Fatal(err)
	}
	seedPolicyGateway(t, h, capability.Present)
	if err := h.db.UpsertClients(context.Background(), []store.SeenClient{{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal}}, 1); err != nil {
		t.Fatal(err)
	}

	compiled := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
		"objects": []any{map[string]any{"kind": "device", "id": "00:11:22:33:44:55"}},
		"outcomes": []any{
			map[string]any{"kind": "secure", "destination_zone": "wan"},
			map[string]any{"kind": "qos", "rate_kbps": 5000},
			map[string]any{"kind": "application"},
		},
	})
	if compiled.Code != http.StatusOK {
		t.Fatalf("compile=%d %s", compiled.Code, compiled.Body.String())
	}
	body := h.json(compiled)
	if body["persisted"] != false || body["applied"] != false || len(body["drafts"].([]any)) != 1 || len(body["gates"].([]any)) != 2 {
		t.Fatalf("compile response=%v", body)
	}
	var count int
	if err := h.db.SQL().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM fw_rules`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("compile persisted %d rules err=%v", count, err)
	}
}

func TestObjectManagerCompilesEverySecureZoneAndNetworkRoutes(t *testing.T) {
	h := newHarness(t)
	h.setup()
	guest, _ := seedAPIZones(t, h)
	seedPolicyGateway(t, h, capability.Present)
	if err := h.db.UpsertClients(context.Background(), []store.SeenClient{{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal}}, 1); err != nil {
		t.Fatal(err)
	}

	secure := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
		"objects": []any{map[string]any{"kind": " device ", "id": " 00:11:22:33:44:55 "}},
		"outcomes": []any{
			map[string]any{"kind": "secure", "destination_zone": "wan"},
			map[string]any{"kind": "route", "target": "203.0.113.0/24", "gateway": "10.0.20.2"},
		},
	})
	if secure.Code != http.StatusOK {
		t.Fatalf("secure compile=%d %s", secure.Code, secure.Body.String())
	}
	body := h.json(secure)
	drafts := body["drafts"].([]any)
	if len(drafts) != 2 || len(body["gates"].([]any)) != 1 || body["persisted"] != false || body["applied"] != false {
		t.Fatalf("multi-zone secure response=%v", body)
	}
	zones := map[string]bool{}
	for _, raw := range drafts {
		draft := raw.(map[string]any)
		zones[draft["firewall"].(map[string]any)["source_zone"].(string)] = true
	}
	if !zones["guest"] || !zones["iot"] {
		t.Fatalf("secure drafts did not cover every managed source zone: %v", drafts)
	}

	route := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
		"objects": []any{map[string]any{"kind": "network", "id": strconv.Itoa(guest.ID)}},
		"outcomes": []any{map[string]any{
			"kind": "route", "target": "203.0.113.0/24", "gateway": "10.0.20.2", "metric": 10,
		}},
	})
	if route.Code != http.StatusOK {
		t.Fatalf("route compile=%d %s", route.Code, route.Body.String())
	}
	body = h.json(route)
	if len(body["drafts"].([]any)) != 1 || len(body["gates"].([]any)) != 0 || body["persisted"] != false || body["applied"] != false {
		t.Fatalf("network route response=%v", body)
	}
	var count int
	if err := h.db.SQL().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM fw_rules`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("compile persisted %d rules err=%v", count, err)
	}
}

func TestPolicyMasterDoesNotOverclaimManagedNetworkRouteRenderability(t *testing.T) {
	h := newHarness(t)
	h.setup()
	guest, iot := seedAPIZones(t, h)
	iot.Enabled = false
	if err := h.db.SaveNetwork(context.Background(), iot); err != nil {
		t.Fatal(err)
	}
	seedPolicyGateway(t, h, capability.Present)
	p := &model.Policy{Name: "documentation route", Kind: model.PolicyStaticRoute,
		Origin: model.PolicyOriginManual, Enabled: true,
		StaticRoute: &model.StaticRoute{NetworkID: guest.ID, Target: "203.0.113.0/24", Gateway: "10.0.20.2"}}
	if err := h.db.SavePolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	master := h.json(h.do(http.MethodGet, "/api/v1/site/policies", nil))
	for _, raw := range master["rows"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == fmt.Sprintf("policy:%d", p.ID) {
			if row["renderable"] != false || !strings.Contains(row["gated_reason"].(string), "Preview") {
				t.Fatalf("managed-network route overclaimed deployability: %v", row)
			}
			return
		}
	}
	t.Fatal("route row missing")
}

func TestClientPolicyAPIUsesLowerSnakeCaseAndSurfacesDesiredFields(t *testing.T) {
	h := newHarness(t)
	h.setup()
	seedAPIZones(t, h)
	seedPolicyGateway(t, h, capability.Present)
	if err := h.db.UpsertClients(context.Background(), []store.SeenClient{{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal}}, 1); err != nil {
		t.Fatal(err)
	}
	res := h.do(http.MethodPost, "/api/v1/clients/00:11:22:33:44:55/policy", map[string]any{
		"blocked": true, "fixed_ip": "10.0.20.50", "group": "cameras",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("client policy=%d %s", res.Code, res.Body.String())
	}
	body := h.json(res)
	client := body["client"].(map[string]any)
	for _, field := range []string{"mac", "blocked", "fixed_ip", "group"} {
		if _, ok := client[field]; !ok {
			t.Fatalf("client response lacks %q: %v", field, client)
		}
	}
	for _, bad := range []string{"MAC", "Blocked", "FixedIP", "Group"} {
		if _, ok := client[bad]; ok {
			t.Fatalf("client response leaked Go field %q: %v", bad, client)
		}
	}
	list := h.json(h.do(http.MethodGet, "/api/v1/clients?all=1", nil))
	row := list["clients"].([]any)[0].(map[string]any)
	if row["group"] != "cameras" || row["fixed_ip"] != "10.0.20.50" || row["blocked"] != true {
		t.Fatalf("GET client row=%v", row)
	}
	site := h.json(h.do(http.MethodGet, "/api/v1/site", nil))
	rows := site["policies"].([]any)
	var block map[string]any
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["kind"] == "client_block" {
			block = row
		}
	}
	if block == nil || block["order_scope"] != "display_only" {
		t.Fatalf("client block row=%v", block)
	}
	scope := block["effective_scope"].(map[string]any)
	if scope["traffic"] != "routed_forwarding" || scope["destination_zones"] != "any" || scope["address_families"].([]any)[1] != "ipv6" ||
		scope["connection_scope"] != "new" || scope["existing_connections"] != "may_persist_until_conntrack_expiry" {
		t.Fatalf("client block overclaimed scope: %v", scope)
	}
}

func TestPolicyMasterGatesUnobservableBackends(t *testing.T) {
	h := newHarness(t)
	h.setup()
	seedAPIZones(t, h)
	seedPolicyGateway(t, h, capability.NotObservable)
	master := h.json(h.do(http.MethodGet, "/api/v1/site/policies", nil))
	byKind := map[string]map[string]any{}
	for _, raw := range master["capabilities"].([]any) {
		row := raw.(map[string]any)
		byKind[row["kind"].(string)] = row
	}
	for _, kind := range []string{"firewall", "nat", "qos", "rate_limit", "application", "priority"} {
		if byKind[kind]["available"] != false || byKind[kind]["reason"] == "" {
			t.Fatalf("%s gate=%v", kind, byKind[kind])
		}
	}
}
