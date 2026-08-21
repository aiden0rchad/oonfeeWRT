package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
)

type recordingNFTCaller struct {
	result nftExecResult
	err    error
	calls  int
	object string
	method string
	args   map[string]any
}

func (c *recordingNFTCaller) Call(_ context.Context, object, method string, args, out any) error {
	c.calls++
	c.object, c.method = object, method
	c.args, _ = args.(map[string]any)
	if c.err != nil {
		return c.err
	}
	*(out.(*nftExecResult)) = c.result
	return nil
}

func explicitPolicySite() model.Site {
	return model.Site{
		UUID:     "firewall-policy-test-site",
		Networks: []model.Network{{ID: 10, Name: "guest", Zone: "guest", VLAN: 10, CIDR: "192.168.10.1/24", Enabled: true}},
		Zones:    []model.ZonePolicy{{Name: "guest", ForwardTo: nil}},
	}
}

func TestExplicitFirewallPolicyRuntimeGate(t *testing.T) {
	safe := nftJSON(t, map[string]any{"metainfo": map[string]any{"version": "1.1.6"}})
	tests := []struct {
		name  string
		site  model.Site
		dev   model.Device
		calls int
	}{
		{name: "explicit gateway", site: explicitPolicySite(), dev: model.Device{Role: model.RoleGateway}, calls: 1},
		{name: "legacy implicit gateway", site: func() model.Site { s := explicitPolicySite(); s.Zones = nil; return s }(), dev: model.Device{Role: model.RoleGateway}},
		{name: "explicit AP", site: explicitPolicySite(), dev: model.Device{Role: model.RoleAP}},
		{name: "inactive explicit row", site: model.Site{Zones: []model.ZonePolicy{{Name: "guest"}}}, dev: model.Device{Role: model.RoleGateway}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := &recordingNFTCaller{result: nftExecResult{Stdout: safe}}
			if got := explicitFirewallPolicyConflict(context.Background(), caller, tt.site, tt.dev, render.Existing{}); got != nil {
				t.Fatalf("unexpected conflict: %+v", got)
			}
			if caller.calls != tt.calls {
				t.Fatalf("nft calls=%d, want %d", caller.calls, tt.calls)
			}
		})
	}
}

func TestExplicitFirewallPolicyUsesExactReadOnlyNFTCommand(t *testing.T) {
	caller := &recordingNFTCaller{result: nftExecResult{Stdout: nftJSON(t,
		map[string]any{"metainfo": map[string]any{"version": "1.1.6"}},
	)}}
	if got := explicitFirewallPolicyConflict(context.Background(), caller, explicitPolicySite(),
		model.Device{Role: model.RoleGateway}, render.Existing{}); got != nil {
		t.Fatalf("unexpected conflict: %+v", got)
	}
	if caller.object != "file" || caller.method != "exec" || caller.args["command"] != "/usr/sbin/nft" {
		t.Fatalf("unexpected call: %s.%s %#v", caller.object, caller.method, caller.args)
	}
	params, ok := caller.args["params"].([]string)
	if !ok || strings.Join(params, " ") != "--terse --json list ruleset" {
		t.Fatalf("unexpected nft params: %#v", caller.args["params"])
	}
}

func TestActiveFirewallIncludeDetection(t *testing.T) {
	tests := []struct {
		name string
		vals map[string]string
		want bool
	}{
		{name: "nftables", vals: map[string]string{".type": "include", "type": "nftables", "path": "/etc/custom.nft"}, want: true},
		{name: "compatible script", vals: map[string]string{".type": "include", "type": "script", "path": "/etc/custom", "fw4_compatible": "1"}, want: true},
		{name: "custom script defaults compatible", vals: map[string]string{".type": "include", "path": "/etc/custom"}, want: true},
		{name: "disabled", vals: map[string]string{".type": "include", "type": "nftables", "path": "/etc/custom.nft", "enabled": "0"}},
		{name: "missing path", vals: map[string]string{".type": "include", "type": "nftables"}},
		{name: "legacy firewall user is not fw4 compatible by default", vals: map[string]string{".type": "include", "path": "/etc/firewall.user"}},
		{name: "explicitly compatible firewall user", vals: map[string]string{".type": "include", "path": "/etc/firewall.user", "fw4_compatible": "1"}, want: true},
		{name: "explicitly incompatible script", vals: map[string]string{".type": "include", "path": "/etc/custom", "fw4_compatible": "false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := render.NewExisting(map[string]map[string]map[string]string{
				"firewall": {"custom": tt.vals},
			})
			name, got := activeFirewallInclude(existing)
			if got != tt.want || (got && name != "custom") {
				t.Fatalf("activeFirewallInclude()=(%q,%v), want custom,%v", name, got, tt.want)
			}
		})
	}
}

func TestActiveFirewallIncludeConflictIsDeterministicAndAvoidsRuntimeCall(t *testing.T) {
	existing := render.NewExisting(map[string]map[string]map[string]string{
		"firewall": {
			"z-last":  {".type": "include", "type": "nftables", "path": "/etc/z.nft"},
			"a-first": {".type": "include", "type": "script", "path": "/etc/a"},
		},
	})
	caller := &recordingNFTCaller{}
	got := explicitFirewallPolicyConflict(context.Background(), caller, explicitPolicySite(),
		model.Device{Role: model.RoleGateway}, existing)
	if got == nil || got.Section != "a-first" || !strings.Contains(got.Reason, "active foreign firewall include a-first") {
		t.Fatalf("unexpected conflict: %+v", got)
	}
	if caller.calls != 0 {
		t.Fatalf("runtime called %d times despite decisive UCI include", caller.calls)
	}
}

func TestForeignNFTRuntimePolicyAcceptsStockFW4(t *testing.T) {
	got, err := foreignNFTRuntimePolicy(nftJSON(t, stockFW4Records()...))
	if err != nil || got != "" {
		t.Fatalf("stock ruleset got artifact=%q err=%v", got, err)
	}
}

func TestForeignNFTRuntimePolicyFindsReachableInjectedRule(t *testing.T) {
	records := stockFW4Records()
	records = append(records, nftRuleRecord("inet", "fw4", "forward_guest", "",
		map[string]any{"drop": nil}))
	got, err := foreignNFTRuntimePolicy(nftJSON(t, records...))
	if err != nil || got != "inet/fw4/forward_guest" {
		t.Fatalf("got artifact=%q err=%v", got, err)
	}
}

func TestForeignNFTRuntimePolicyFindsCustomTransitTableAndHook(t *testing.T) {
	tests := []struct {
		name    string
		records []any
		want    string
	}{
		{
			name: "effectful forward rule",
			records: []any{
				nftChainRecord("ip", "guard", "forward_guard", "forward", "accept"),
				nftRuleRecord("ip", "guard", "forward_guard", "custom", map[string]any{"drop": nil}),
			},
			want: "ip/guard/forward_guard",
		},
		{
			name:    "drop base policy without rules",
			records: []any{nftChainRecord("inet", "guard", "guard", "forward", "drop")},
			want:    "inet/guard/guard",
		},
		{
			name: "postrouting can still deny delivery",
			records: []any{
				nftChainRecord("inet", "guard", "after", "postrouting", "accept"),
				nftRuleRecord("inet", "guard", "after", "", map[string]any{"reject": nil}),
			},
			want: "inet/guard/after",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := foreignNFTRuntimePolicy(nftJSON(t, tt.records...))
			if err != nil || got != tt.want {
				t.Fatalf("got artifact=%q err=%v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestRouterInputPolicyInspectsCustomInputHooks(t *testing.T) {
	rules := nftJSON(t,
		nftChainRecord("inet", "guard", "router_guard", "input", "accept"),
		nftRuleRecord("inet", "guard", "router_guard", "custom", map[string]any{"drop": nil}),
	)
	if got, err := foreignNFTRuntimePolicy(rules); err != nil || got != "" {
		t.Fatalf("forward-only scan got artifact=%q err=%v", got, err)
	}
	if got, err := foreignNFTRuntimePolicyScope(rules, true); err != nil || got != "inet/guard/router_guard" {
		t.Fatalf("router-input scan got artifact=%q err=%v", got, err)
	}

	site := explicitPolicySite()
	site.Policies = []model.Policy{{Name: "router", Kind: model.PolicyFirewallRule,
		Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallAccept, SourceZone: "guest"}}}
	caller := &recordingNFTCaller{result: nftExecResult{Stdout: rules}}
	if conflict := explicitFirewallPolicyConflict(context.Background(), caller, site,
		model.Device{Role: model.RoleGateway}, render.Existing{}); conflict == nil {
		t.Fatal("custom input hook did not block router-input policy Preview")
	}
	caller = &recordingNFTCaller{result: nftExecResult{Stdout: rules}}
	if conflict := explicitFirewallPolicyConflict(context.Background(), caller, explicitPolicySite(),
		model.Device{Role: model.RoleGateway}, render.Existing{}); conflict == nil {
		t.Fatal("custom input hook did not block managed zone router-service policy")
	}
}

func TestForeignNFTRuntimePolicyIgnoresInertAndCommentOnlyRuntime(t *testing.T) {
	tests := []struct {
		name    string
		records []any
	}{
		{name: "metainfo only", records: []any{map[string]any{"metainfo": map[string]any{"version": "1.1.6"}}}},
		{name: "unhooked custom chain", records: []any{
			nftChainRecord("inet", "custom", "unused", "", ""),
			nftRuleRecord("inet", "custom", "unused", "", map[string]any{"drop": nil}),
		}},
		{name: "output does not govern forwarded packets", records: []any{
			nftChainRecord("inet", "custom", "out", "output", "drop"),
		}},
		{name: "log-only transit hook", records: []any{
			nftChainRecord("inet", "custom", "audit", "forward", "accept"),
			nftRuleRecord("inet", "custom", "audit", "audit only",
				map[string]any{"counter": map[string]any{"packets": 99, "bytes": 1000}},
				map[string]any{"log": map[string]any{"prefix": "audit"}}),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := foreignNFTRuntimePolicy(nftJSON(t, tt.records...))
			if err != nil || got != "" {
				t.Fatalf("got artifact=%q err=%v", got, err)
			}
		})
	}
}

func TestForeignNFTRuntimePolicyReturnsDeterministicArtifact(t *testing.T) {
	records := []any{
		nftChainRecord("inet", "zeta", "guard", "forward", "drop"),
		nftChainRecord("inet", "alpha", "guard", "forward", "drop"),
	}
	got, err := foreignNFTRuntimePolicy(nftJSON(t, records...))
	if err != nil || got != "inet/alpha/guard" {
		t.Fatalf("got artifact=%q err=%v", got, err)
	}
}

func TestForeignNFTRuntimePolicyRejectsUnreadableEvidence(t *testing.T) {
	tests := []string{
		"",
		"not-json",
		`{}`,
		nftJSON(t, nftRuleRecord("inet", "fw4", "missing", "", map[string]any{"drop": nil})),
	}
	for _, input := range tests {
		if got, err := foreignNFTRuntimePolicy(input); got != "" || !errors.Is(err, errNFTRulesetMalformed) {
			t.Fatalf("input %.20q got artifact=%q err=%v", input, got, err)
		}
	}
	if got, err := foreignNFTRuntimePolicy(strings.Repeat(" ", nftRulesetLimit+1)); got != "" || !errors.Is(err, errNFTRulesetTooLarge) {
		t.Fatalf("oversize got artifact=%q err=%v", got, err)
	}
}

func TestExplicitFirewallPolicyFailsClosedWhenNFTUnavailable(t *testing.T) {
	tests := []recordingNFTCaller{
		{err: errors.New("denied")},
		{result: nftExecResult{Code: 1}},
		{result: nftExecResult{Stdout: "{"}},
	}
	for i := range tests {
		got := explicitFirewallPolicyConflict(context.Background(), &tests[i], explicitPolicySite(),
			model.Device{Role: model.RoleGateway}, render.Existing{})
		if got == nil || got.Section != "runtime-nftables" || !strings.Contains(got.Reason, "Explicit directional zone policy is blocked") {
			t.Fatalf("case %d unexpected conflict: %+v", i, got)
		}
	}
}

func TestPlanDeviceBlocksExplicitPolicyWhenNFTACLIsUnavailableButLegacyIsUnaffected(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	if err := c.Call(ctx, "__test", "set_acl_gap", map[string]any{
		"pairs": []map[string]string{{"object": "file", "method": "exec"}},
	}, nil); err != nil {
		t.Fatalf("set ACL gap: %v", err)
	}
	r := New(nil)
	dev := model.Device{ID: 1, Role: model.RoleGateway}

	plan, err := r.PlanDevice(ctx, c, explicitPolicySite(), dev, caps())
	if err != nil {
		t.Fatalf("explicit plan: %v", err)
	}
	if !plan.Blocked() || len(plan.Plan.Ops) != 0 || !hasRuntimeNFTConflict(plan.Report.Conflicts) {
		t.Fatalf("explicit plan did not fail closed: %+v", plan.Report.Conflicts)
	}

	legacy := explicitPolicySite()
	legacy.Zones = nil
	plan, err = r.PlanDevice(ctx, c, legacy, dev, caps())
	if err != nil {
		t.Fatalf("legacy plan: %v", err)
	}
	if hasRuntimeNFTConflict(plan.Report.Conflicts) {
		t.Fatalf("legacy policy unexpectedly depends on nft observation: %+v", plan.Report.Conflicts)
	}
}

func hasRuntimeNFTConflict(conflicts []render.Conflict) bool {
	for _, conflict := range conflicts {
		if conflict.Section == "runtime-nftables" {
			return true
		}
	}
	return false
}

func nftJSON(t *testing.T, records ...any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"nftables": records})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func nftChainRecord(family, table, name, hook, policy string) map[string]any {
	chain := map[string]any{"family": family, "table": table, "name": name}
	if hook != "" {
		chain["type"] = "filter"
		chain["hook"] = hook
		chain["prio"] = 0
	}
	if policy != "" {
		chain["policy"] = policy
	}
	return map[string]any{"chain": chain}
}

func nftRuleRecord(family, table, chain, comment string, expr ...any) map[string]any {
	rule := map[string]any{"family": family, "table": table, "chain": chain, "expr": expr}
	if comment != "" {
		rule["comment"] = comment
	}
	return map[string]any{"rule": rule}
}

func stockFW4Records() []any {
	return []any{
		map[string]any{"metainfo": map[string]any{"version": "1.1.6"}},
		map[string]any{"table": map[string]any{"family": "inet", "name": "fw4"}},
		nftChainRecord("inet", "fw4", "forward", "forward", "drop"),
		nftChainRecord("inet", "fw4", "forward_guest", "", ""),
		nftChainRecord("inet", "fw4", "accept_to_wan", "", ""),
		nftChainRecord("inet", "fw4", "reject_to_guest", "", ""),
		nftChainRecord("inet", "fw4", "handle_reject", "", ""),
		nftRuleRecord("inet", "fw4", "forward", "!fw4: Handle forwarded flows",
			map[string]any{"match": map[string]any{"op": "==", "left": "ct state", "right": "established"}},
			map[string]any{"accept": nil}),
		nftRuleRecord("inet", "fw4", "forward", "!fw4: Forward guest traffic",
			map[string]any{"match": map[string]any{"op": "==", "left": "iifname", "right": "br-guest"}},
			map[string]any{"jump": map[string]any{"target": "forward_guest"}}),
		nftRuleRecord("inet", "fw4", "forward", "",
			map[string]any{"match": map[string]any{"op": "in", "left": "meta l4proto", "right": []string{"tcp", "udp"}}},
			map[string]any{"flow": map[string]any{"op": "offload", "flowtable": "@ft"}}),
		nftRuleRecord("inet", "fw4", "forward", "", map[string]any{"jump": map[string]any{"target": "handle_reject"}}),
		nftRuleRecord("inet", "fw4", "forward_guest", "!fw4: Allow guest to wan",
			map[string]any{"jump": map[string]any{"target": "accept_to_wan"}}),
		nftRuleRecord("inet", "fw4", "forward_guest", "",
			map[string]any{"jump": map[string]any{"target": "reject_to_guest"}}),
		nftRuleRecord("inet", "fw4", "forward_guest", "",
			map[string]any{"limit": map[string]any{"rate": 10}},
			map[string]any{"log": map[string]any{"prefix": "reject guest forward: "}}),
		nftRuleRecord("inet", "fw4", "reject_to_guest", "!fw4: Reject guest traffic", map[string]any{"reject": nil}),
		nftRuleRecord("inet", "fw4", "handle_reject", "!fw4: Reject any other traffic", map[string]any{"reject": nil}),
	}
}
