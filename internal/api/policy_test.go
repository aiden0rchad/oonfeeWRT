package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestPolicyGatewayGateUsesOnlyValidManagedGateway(t *testing.T) {
	at := int64(1)
	present := capability.NewRegistry()
	present.Set(capability.FeatFirewall4, capability.Present)
	presentJSON, _ := json.Marshal(present)
	absent := capability.NewRegistry()
	absent.Set(capability.FeatFirewall4, capability.NotObservable)
	absentJSON, _ := json.Marshal(absent)
	monitor := &store.Device{Name: "observed", Role: "gateway", Functions: []string{"gateway"},
		ManagementMode: "monitor_only", AdoptedAt: &at, CapsJSON: string(absentJSON)}
	if ok, reason := policyGatewayGate([]*store.Device{monitor}, true); ok || !strings.Contains(reason, "managed") {
		t.Fatalf("monitor-only gateway gate=(%t,%q)", ok, reason)
	}
	managed := &store.Device{Name: "managed", Role: "gateway", Functions: []string{"gateway"},
		ManagementMode: "managed", AdoptedAt: &at, CapsJSON: string(presentJSON)}
	if ok, reason := policyGatewayGate([]*store.Device{monitor, managed}, true); !ok || reason != "" {
		t.Fatalf("monitor capability poisoned managed gateway gate=(%t,%q)", ok, reason)
	}
	invalid := *managed
	invalid.ManagementModeError = "corrupt"
	if ok, reason := policyGatewayGate([]*store.Device{&invalid}, false); ok || !strings.Contains(reason, "invalid") {
		t.Fatalf("invalid gateway gate=(%t,%q)", ok, reason)
	}
}

func TestBoundedObjectPolicyNameIsStableUTF8(t *testing.T) {
	for _, name := range []string{strings.Repeat("x", 512), "Secure policy_set " + strings.Repeat("é", 128) + " from guest"} {
		first, second := boundedPolicyName(name), boundedPolicyName(name)
		if first != second || len(first) > 128 || !utf8.ValidString(first) {
			t.Fatalf("bounded name len=%d valid=%t stable=%t", len(first), utf8.ValidString(first), first == second)
		}
	}
	if boundedPolicyName(strings.Repeat("x", 511)+"a") == boundedPolicyName(strings.Repeat("x", 511)+"b") {
		t.Fatal("distinct long object names lost their stable collision suffix")
	}
}

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

func observeAPIPolicyClients(t *testing.T, h *harness, deviceID int64, seen []store.SeenClient) {
	t.Helper()
	for i := range seen {
		seen[i].DeviceID = deviceID
	}
	if err := h.db.UpsertClients(context.Background(), seen, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
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
	gateway := seedPolicyGateway(t, h, capability.Present)
	observeAPIPolicyClients(t, h, gateway.ID, []store.SeenClient{{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal}})

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
	gateway := seedPolicyGateway(t, h, capability.Present)
	observeAPIPolicyClients(t, h, gateway.ID, []store.SeenClient{{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal}})

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
	gateway := seedPolicyGateway(t, h, capability.Present)
	observeAPIPolicyClients(t, h, gateway.ID, []store.SeenClient{{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal}})
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

func TestReusablePolicySetCRUDCompileAndEffectiveScope(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, iot := seedAPIZones(t, h)
	iot.Enabled = false
	if err := h.db.SaveNetwork(context.Background(), iot); err != nil {
		t.Fatal(err)
	}
	gateway := seedPolicyGateway(t, h, capability.Present)
	observeAPIPolicyClients(t, h, gateway.ID, []store.SeenClient{
		{MAC: "00:11:22:33:44:55", Scope: store.ScopeLocal},
		{MAC: "00:11:22:33:44:66", Scope: store.ScopeLocal},
	})

	created := h.do(http.MethodPost, "/api/v1/site/policy-sets", map[string]any{
		"name": "Cameras", "members": []string{"00-11-22-33-44-66", "00:11:22:33:44:55"},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create set=%d %s", created.Code, created.Body.String())
	}
	set := h.json(created)["policy_set"].(map[string]any)
	setID := int(set["id"].(float64))
	if setID == 0 || set["members"].([]any)[0] != "00:11:22:33:44:55" {
		t.Fatalf("created set=%v", set)
	}

	compiled := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
		"objects":  []any{map[string]any{"kind": "policy_set", "id": strconv.Itoa(setID)}},
		"outcomes": []any{map[string]any{"kind": "secure", "destination_zone": "wan"}},
	})
	if compiled.Code != http.StatusOK {
		t.Fatalf("compile set=%d %s", compiled.Code, compiled.Body.String())
	}
	drafts := h.json(compiled)["drafts"].([]any)
	if len(drafts) != 1 {
		t.Fatalf("compiled drafts=%v", drafts)
	}
	draft := drafts[0].(map[string]any)
	firewall := draft["firewall"].(map[string]any)
	if firewall["source_set_id"] != float64(setID) {
		t.Fatalf("compiled firewall=%v", firewall)
	}

	delete(draft, "id")
	draft["order"] = 100
	saved := h.do(http.MethodPost, "/api/v1/site/policies", draft)
	if saved.Code != http.StatusOK {
		t.Fatalf("save compiled policy=%d %s", saved.Code, saved.Body.String())
	}
	policyID := int(h.json(saved)["id"].(float64))

	master := h.json(h.do(http.MethodGet, "/api/v1/site/policies", nil))
	found := false
	for _, raw := range master["rows"].([]any) {
		row := raw.(map[string]any)
		if row["id"] != fmt.Sprintf("policy:%d", policyID) {
			continue
		}
		found = true
		scope := row["effective_scope"].(map[string]any)
		if scope["source_set"].(map[string]any)["name"] != "Cameras" || len(scope["source_macs"].([]any)) != 2 {
			t.Fatalf("effective set scope=%v", scope)
		}
	}
	if !found {
		t.Fatal("saved set-backed policy missing from master")
	}

	updated := h.do(http.MethodPost, fmt.Sprintf("/api/v1/site/policy-sets/%d", setID), map[string]any{
		"name": "Cameras", "members": []string{"00:11:22:33:44:66"},
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update set=%d %s", updated.Code, updated.Body.String())
	}
	master = h.json(h.do(http.MethodGet, "/api/v1/site/policies", nil))
	for _, raw := range master["rows"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == fmt.Sprintf("policy:%d", policyID) && len(row["effective_scope"].(map[string]any)["source_macs"].([]any)) != 1 {
			t.Fatalf("master retained stale set membership: %v", row)
		}
	}

	blocked := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/site/policy-sets/%d", setID), nil)
	if blocked.Code != http.StatusBadRequest || !strings.Contains(blocked.Body.String(), "still references") {
		t.Fatalf("referenced set delete=%d %s", blocked.Code, blocked.Body.String())
	}
	if res := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/site/policies/%d", policyID), nil); res.Code != http.StatusOK {
		t.Fatalf("delete policy=%d %s", res.Code, res.Body.String())
	}
	if res := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/site/policy-sets/%d", setID), nil); res.Code != http.StatusOK {
		t.Fatalf("delete set=%d %s", res.Code, res.Body.String())
	}
}

func TestObjectManagerRefusesUnprovableClientMACScope(t *testing.T) {
	for _, test := range []struct {
		name          string
		scope         string
		monitorSource bool
		want          string
	}{
		{name: "upstream client", scope: store.ScopeUpstream, want: "observed as upstream"},
		{name: "monitor AP inventory", scope: store.ScopeLocal, monitorSource: true, want: "has not been observed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			seedAPIZones(t, h)
			gateway := seedPolicyGateway(t, h, capability.Present)
			sourceID := gateway.ID
			if test.monitorSource {
				at := int64(1)
				monitor := &store.Device{
					MAC: "aa:bb:cc:dd:ee:09", Host: "198.51.100.1", Name: "observed-ap",
					Role: "ap", Functions: []string{"ap"}, ManagementMode: "monitor_only", AdoptedAt: &at,
				}
				if err := h.db.UpsertDevice(context.Background(), monitor); err != nil {
					t.Fatal(err)
				}
				sourceID = monitor.ID
			}
			const mac = "00:11:22:33:44:99"
			observeAPIPolicyClients(t, h, sourceID, []store.SeenClient{{MAC: mac, Scope: test.scope}})
			compiled := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
				"objects":  []any{map[string]any{"kind": "device", "id": mac}},
				"outcomes": []any{map[string]any{"kind": "secure", "destination_zone": "wan"}},
			})
			if compiled.Code != http.StatusOK {
				t.Fatalf("compile=%d %s", compiled.Code, compiled.Body.String())
			}
			body := h.json(compiled)
			if len(body["drafts"].([]any)) != 0 || len(body["gates"].([]any)) == 0 ||
				!strings.Contains(compiled.Body.String(), test.want) {
				t.Fatalf("unsafe scope was not explicitly gated: %s", compiled.Body.String())
			}
		})
	}
}

func TestObjectManagerRejectsSetExpansionBeyondSiteBudget(t *testing.T) {
	site := model.Site{Networks: []model.Network{
		{ID: 1, Name: "guest", Zone: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Enabled: true},
		{ID: 2, Name: "iot", Zone: "iot", VLAN: 30, CIDR: "10.0.30.1/24", Enabled: true},
	}}
	members := make([]string, model.MaxPolicySetMembers)
	for i := range members {
		members[i] = fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x",
			byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	}
	site.PolicySets = []model.PolicySet{{ID: 7, Name: strings.Repeat("é", 64), Members: members}}
	for i := 0; i < model.MaxExpandedPolicySourceMACs/model.MaxPolicySetMembers-1; i++ {
		site.Policies = append(site.Policies, model.Policy{ID: i + 1, Name: fmt.Sprintf("existing-%d", i),
			Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: false,
			Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: "guest",
				DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: 7}})
	}
	if _, _, err := compileObjectOutcome(site, objectTarget{Kind: "policy_set", ID: "7"},
		objectOutcome{Kind: "secure", DestinationZone: "wan"}); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("over-budget multi-zone compile error=%v", err)
	}
}

func TestObjectManagerRejectsCumulativeDraftExpansionBeyondSiteBudget(t *testing.T) {
	h := newHarness(t)
	h.setup()
	seedAPIZones(t, h)
	gateway := seedPolicyGateway(t, h, capability.Present)
	members := make([]string, model.MaxPolicySetMembers)
	clients := make([]store.SeenClient, len(members))
	for i := range members {
		members[i] = fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x",
			byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
		clients[i] = store.SeenClient{MAC: members[i], Scope: store.ScopeLocal}
	}
	observeAPIPolicyClients(t, h, gateway.ID, clients)
	set := &model.PolicySet{Name: "bounded", Members: members}
	if err := h.db.SavePolicySet(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	const compiledPerOutcome = 2 // seedAPIZones creates two active source zones.
	for i := 0; i < model.MaxExpandedPolicySourceMACs/model.MaxPolicySetMembers-compiledPerOutcome; i++ {
		policy := &model.Policy{Name: fmt.Sprintf("existing-%d", i), Kind: model.PolicyFirewallRule,
			Origin: model.PolicyOriginManual, Enabled: false,
			Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: "guest",
				DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: set.ID}}
		if err := h.db.SavePolicy(context.Background(), policy); err != nil {
			t.Fatalf("save existing policy %d: %v", i, err)
		}
	}

	res := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
		"objects": []any{map[string]any{"kind": "policy_set", "id": strconv.Itoa(set.ID)}},
		"outcomes": []any{
			map[string]any{"kind": "secure", "destination_zone": "wan"},
			map[string]any{"kind": "secure", "destination_zone": "wan"},
		},
	})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "expansion exceed") {
		t.Fatalf("cumulative over-budget compile=%d %s", res.Code, res.Body.String())
	}
}

func TestObjectManagerCapsCrossProductBeforeCompilation(t *testing.T) {
	h := newHarness(t)
	h.setup()
	objects := make([]any, 65)
	outcomes := make([]any, 65)
	for i := range objects {
		objects[i] = map[string]any{"kind": "network", "id": "wan"}
		outcomes[i] = map[string]any{"kind": "qos"}
	}
	res := h.do(http.MethodPost, "/api/v1/site/object-manager/compile", map[string]any{
		"objects": objects, "outcomes": outcomes,
	})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "at most 4096") {
		t.Fatalf("over-limit cross-product compile=%d %s", res.Code, res.Body.String())
	}
}
