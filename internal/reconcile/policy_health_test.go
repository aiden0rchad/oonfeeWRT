package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/render"
)

func nftEtherMatch(mac string) map[string]any {
	return map[string]any{"match": map[string]any{
		"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": "ether", "field": "saddr"}}, "right": mac,
	}}
}

func nftAction(action string) map[string]any {
	if action == "reject" {
		return map[string]any{"jump": map[string]any{"target": "handle_reject"}}
	}
	return map[string]any{action: nil}
}

func exactRuleRuntime(t *testing.T, action string, port int, mac string, ipv4 bool) *nftRuntime {
	t.Helper()
	exprs := []map[string]any{}
	if ipv4 {
		exprs = append(exprs, nftMetaMatch("nfproto", "ipv4"))
	}
	exprs = append(exprs,
		nftMetaMatch("l4proto", "tcp"), nftPortMatch("tcp", "dport", port),
		nftEtherMatch(mac), map[string]any{"jump": map[string]any{"target": action + "_to_wan"}})
	anyExpr := make([]any, len(exprs))
	for i := range exprs {
		anyExpr[i] = exprs[i]
	}
	records := []any{
		nftChainRecord("inet", "fw4", "input", "input", "drop"),
		nftChainRecord("inet", "fw4", "input_guest", "", ""),
		nftChainRecord("inet", "fw4", "forward", "forward", "drop"),
		nftChainRecord("inet", "fw4", "forward_guest", "", ""),
		nftChainRecord("inet", "fw4", action+"_to_wan", "", ""),
		nftChainRecord("inet", "fw4", "handle_reject", "", ""),
		nftRuleRecord("inet", "fw4", action+"_to_wan", "!fw4: action traffic towards wan",
			nftMetaMatch("oifname", "wan"), nftAction(action)),
		nftRuleRecord("inet", "fw4", "input", "!fw4: input guest", nftMetaMatch("iifname", "br-guest"),
			map[string]any{"jump": map[string]any{"target": "input_guest"}}),
		nftRuleRecord("inet", "fw4", "forward", "!fw4: forward guest", nftMetaMatch("iifname", "br-guest"),
			map[string]any{"jump": map[string]any{"target": "forward_guest"}}),
		nftRuleRecord("inet", "fw4", "forward_guest", "!fw4: oonfeeWRT policy 1 test", anyExpr...),
	}
	runtime, err := parseNFTRuntime(nftJSON(t, records...))
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestFirewallPolicyRuntimeAcceptsRealFW4ActionChainShape(t *testing.T) {
	for _, action := range []string{"accept", "drop", "reject"} {
		t.Run(action, func(t *testing.T) {
			want, ok := managedFirewallPolicyExpectation("rule", map[string]string{
				"name": "oonfeeWRT policy 1 test", "src": "guest", "dest": "wan", "family": "ipv4",
				"proto": "tcp", "dest_port": "443", "src_mac": "00:11:22:33:44:55", "target": strings.ToUpper(action),
			})
			if !ok || !exactRuleRuntime(t, action, 443, "00:11:22:33:44:55", true).hasExactPolicy(want) {
				t.Fatal("real firewall4 action-chain rule rejected")
			}
		})
	}
}

func TestExplicitFirewallRuleMustPrecedeContradictingTerminalPath(t *testing.T) {
	for _, test := range []struct{ want, earlier string }{{"drop", "accept"}, {"accept", "reject"}} {
		t.Run(test.want+" after "+test.earlier, func(t *testing.T) {
			want, _ := managedFirewallPolicyExpectation("rule", map[string]string{
				"name": "oonfeeWRT policy 1 test", "src": "guest", "dest": "wan", "family": "ipv4",
				"proto": "tcp", "dest_port": "443", "src_mac": "00:11:22:33:44:55", "target": strings.ToUpper(test.want),
			})
			runtime := exactRuleRuntime(t, test.want, 443, "00:11:22:33:44:55", true)
			key := nftChainKey{Family: "inet", Table: "fw4", Name: "forward_guest"}
			prior := nftRule{Family: "inet", Table: "fw4", Chain: "forward_guest",
				Expr: []json.RawMessage{json.RawMessage(`{"` + test.earlier + `":null}`)}}
			runtime.rules[key] = append([]nftRule{prior}, runtime.rules[key]...)
			if runtime.hasExactPolicy(want) {
				t.Fatal("policy after a contradicting terminal path was treated as effective")
			}
		})
	}
}

func TestPolicyRuntimeRejectsStableCommentAtOldLocation(t *testing.T) {
	want, _ := managedFirewallPolicyExpectation("rule", map[string]string{
		"name": "oonfeeWRT policy 1 test", "src": "guest", "dest": "wan", "family": "ipv4",
		"proto": "tcp", "dest_port": "443", "src_mac": "00:11:22:33:44:55", "target": "DROP",
	})
	for _, old := range []nftChainKey{
		{Family: "inet", Table: "fw4", Name: "forward_iot"},
		{Family: "inet", Table: "fw4", Name: "input_guest"},
		{Family: "inet", Table: "fw4", Name: "dstnat_wan"},
	} {
		t.Run(old.Name, func(t *testing.T) {
			runtime := exactRuleRuntime(t, "drop", 443, "00:11:22:33:44:55", true)
			current := nftChainKey{Family: "inet", Table: "fw4", Name: "forward_guest"}
			stale := runtime.rules[current][0]
			stale.Chain = old.Name
			runtime.chains[old] = nftChain{Family: old.Family, Table: old.Table, Name: old.Name}
			runtime.rules[old] = []nftRule{stale}
			if runtime.hasExactPolicy(want) {
				t.Fatal("same-comment stale policy at its old chain was accepted")
			}
		})
	}
}

func TestLoggedSourceActionChainsRemainProvable(t *testing.T) {
	router, _ := managedFirewallPolicyExpectation("rule", map[string]string{
		"name": "oonfeeWRT policy 3 router", "src": "wan", "family": "ipv4", "proto": "tcp", "target": "DROP",
	})
	client, _ := managedFirewallPolicyExpectation("rule", map[string]string{
		"name": "oonfeeWRT client-block 00:11:22:33:44:55 guest", "src": "guest", "dest": "*",
		"proto": "all", "src_mac": "00:11:22:33:44:55", "target": "REJECT",
	})
	runtime, err := parseNFTRuntime(nftJSON(t,
		nftChainRecord("inet", "fw4", "input", "input", "drop"),
		nftChainRecord("inet", "fw4", "input_wan", "", ""),
		nftChainRecord("inet", "fw4", "input_guest", "", ""),
		nftChainRecord("inet", "fw4", "forward", "forward", "drop"),
		nftChainRecord("inet", "fw4", "forward_wan", "", ""),
		nftChainRecord("inet", "fw4", "drop_from_wan", "", ""),
		nftChainRecord("inet", "fw4", "forward_guest", "", ""),
		nftChainRecord("inet", "fw4", "reject_from_guest", "", ""),
		nftChainRecord("inet", "fw4", "handle_reject", "", ""),
		nftRuleRecord("inet", "fw4", "input", "!fw4: input wan", nftMetaMatch("iifname", "wan"),
			map[string]any{"jump": map[string]any{"target": "input_wan"}}),
		nftRuleRecord("inet", "fw4", "input", "!fw4: input guest", nftMetaMatch("iifname", "br-guest"),
			map[string]any{"jump": map[string]any{"target": "input_guest"}}),
		nftRuleRecord("inet", "fw4", "forward", "!fw4: forward wan", nftMetaMatch("iifname", "wan"),
			map[string]any{"jump": map[string]any{"target": "forward_wan"}}),
		nftRuleRecord("inet", "fw4", "forward", "!fw4: forward guest", nftMetaMatch("iifname", "br-guest"),
			map[string]any{"jump": map[string]any{"target": "forward_guest"}}),
		nftRuleRecord("inet", "fw4", "input_wan", router.comment,
			nftMetaMatch("nfproto", "ipv4"), nftMetaMatch("l4proto", "tcp"),
			map[string]any{"jump": map[string]any{"target": "drop_from_wan"}}),
		nftRuleRecord("inet", "fw4", "drop_from_wan", "!fw4: drop wan",
			nftMetaMatch("iifname", "wan"), map[string]any{"drop": nil}),
		nftRuleRecord("inet", "fw4", "forward_guest", client.comment,
			nftEtherMatch("00:11:22:33:44:55"), map[string]any{"jump": map[string]any{"target": "reject_from_guest"}}),
		nftRuleRecord("inet", "fw4", "reject_from_guest", "!fw4: reject guest",
			nftMetaMatch("iifname", "br-guest"), map[string]any{"jump": map[string]any{"target": "handle_reject"}}),
	))
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.hasExactPolicy(router) || !runtime.hasExactPolicy(client) {
		t.Fatal("real firewall4 logged-source action chain was rejected")
	}
}

func TestFirewallPolicyRuntimeRequiresExactChangedRuleSemantics(t *testing.T) {
	want, ok := managedFirewallPolicyExpectation("rule", map[string]string{
		"name": "oonfeeWRT policy 1 test", "src": "guest", "dest": "wan", "family": "ipv4",
		"proto": "tcp", "dest_port": "443", "src_mac": "00:11:22:33:44:55", "target": "DROP",
	})
	if !ok {
		t.Fatal("valid rule expectation rejected")
	}
	if !exactRuleRuntime(t, "drop", 443, "00:11:22:33:44:55", true).hasExactPolicy(want) {
		t.Fatal("exact rule runtime rejected")
	}
	for name, runtime := range map[string]*nftRuntime{
		"stale action": exactRuleRuntime(t, "accept", 443, "00:11:22:33:44:55", true),
		"stale port":   exactRuleRuntime(t, "drop", 80, "00:11:22:33:44:55", true),
		"stale MAC":    exactRuleRuntime(t, "drop", 443, "00:11:22:33:44:66", true),
		"lost family":  exactRuleRuntime(t, "drop", 443, "00:11:22:33:44:55", false),
	} {
		t.Run(name, func(t *testing.T) {
			if runtime.hasExactPolicy(want) {
				t.Fatal("same-comment stale runtime accepted")
			}
		})
	}
}

func TestClientBlockRuntimeProofIsDualStackForwardOnly(t *testing.T) {
	want, ok := managedFirewallPolicyExpectation("rule", map[string]string{
		"name": "oonfeeWRT client-block 00:11:22:33:44:55 guest", "src": "guest", "dest": "*",
		"proto": "all", "src_mac": "00:11:22:33:44:55", "target": "REJECT",
	})
	if !ok {
		t.Fatal("client block expectation rejected")
	}
	runtimeFor := func(ipv4, acceptFirst bool) *nftRuntime {
		exprs := []any{}
		if ipv4 {
			exprs = append(exprs, nftMetaMatch("nfproto", "ipv4"))
		}
		exprs = append(exprs, nftEtherMatch("00:11:22:33:44:55"),
			map[string]any{"jump": map[string]any{"target": "handle_reject"}})
		records := []any{
			nftChainRecord("inet", "fw4", "input", "input", "drop"),
			nftChainRecord("inet", "fw4", "input_guest", "", ""),
			nftChainRecord("inet", "fw4", "forward", "forward", "drop"),
			nftChainRecord("inet", "fw4", "forward_guest", "", ""),
			nftChainRecord("inet", "fw4", "accept_to_wan", "", ""),
			nftChainRecord("inet", "fw4", "handle_reject", "", ""),
			nftRuleRecord("inet", "fw4", "accept_to_wan", "!fw4: accept wan", nftMetaMatch("oifname", "wan"), map[string]any{"accept": nil}),
			nftRuleRecord("inet", "fw4", "input", "!fw4: input guest", nftMetaMatch("iifname", "br-guest"),
				map[string]any{"jump": map[string]any{"target": "input_guest"}}),
			nftRuleRecord("inet", "fw4", "forward", "!fw4: forward guest", nftMetaMatch("iifname", "br-guest"),
				map[string]any{"jump": map[string]any{"target": "forward_guest"}}),
		}
		if acceptFirst {
			records = append(records, nftRuleRecord("inet", "fw4", "forward_guest", "!fw4: allow wan",
				map[string]any{"jump": map[string]any{"target": "accept_to_wan"}}))
		}
		records = append(records, nftRuleRecord("inet", "fw4", "forward_guest", want.comment, exprs...))
		runtime, err := parseNFTRuntime(nftJSON(t, records...))
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	if !runtimeFor(false, false).hasExactPolicy(want) {
		t.Fatal("family-agnostic forwarded MAC reject was not proved")
	}
	if runtimeFor(true, false).hasExactPolicy(want) {
		t.Fatal("stale family=ipv4 client block was accepted as dual-stack")
	}
	if runtimeFor(false, true).hasExactPolicy(want) {
		t.Fatal("client block after an unconditional forwarding accept was treated as effective")
	}
}

func TestDNATRuntimeRequiresExactPortAndDestination(t *testing.T) {
	want, ok := managedFirewallPolicyExpectation("redirect", map[string]string{
		"name": "oonfeeWRT policy 2 camera", "src": "wan", "dest": "iot", "family": "ipv4", "proto": "tcp",
		"src_dport": "443", "dest_ip": "10.0.30.20", "dest_port": "8443", "target": "DNAT",
	})
	if !ok {
		t.Fatal("DNAT expectation rejected")
	}
	runtimeFor := func(port int, addr, path string) *nftRuntime {
		records := []any{
			nftChainRecord("inet", "fw4", "forward", "forward", "drop"),
			nftChainRecord("inet", "fw4", "forward_wan", "", ""),
			nftChainRecord("inet", "fw4", "reject_to_wan", "", ""),
			nftChainRecord("inet", "fw4", "dstnat", "prerouting", "accept"),
			nftChainRecord("inet", "fw4", "dstnat_wan", "", ""),
			nftRuleRecord("inet", "fw4", "dstnat_wan", want.comment,
				nftMetaMatch("nfproto", "ipv4"), nftMetaMatch("l4proto", "tcp"),
				nftPortMatch("tcp", "dport", port),
				map[string]any{"dnat": map[string]any{"addr": addr, "port": 8443}}),
		}
		forwardDevice := "wan"
		if path == "wrong-iif" {
			forwardDevice = "lan"
		}
		forwardDispatch := nftRuleRecord("inet", "fw4", "forward", "!fw4: forward wan", nftMetaMatch("iifname", forwardDevice),
			map[string]any{"jump": map[string]any{"target": "forward_wan"}})
		dstnatDispatch := nftRuleRecord("inet", "fw4", "dstnat", "!fw4: dstnat wan", nftMetaMatch("iifname", "wan"),
			map[string]any{"jump": map[string]any{"target": "dstnat_wan"}})
		if path == "late-dispatch" {
			records = append(records, nftRuleRecord("inet", "fw4", "forward", "", map[string]any{"drop": nil}))
		}
		records = append(records, forwardDispatch)
		if path != "missing-dstnat" {
			records = append(records, dstnatDispatch)
		}
		accept := nftRuleRecord("inet", "fw4", "forward_wan", "!fw4: Accept port forwards",
			map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"ct": map[string]any{"key": "status"}}, "right": "dnat"}},
			map[string]any{"accept": nil})
		tail := nftRuleRecord("inet", "fw4", "forward_wan", "", map[string]any{"jump": map[string]any{"target": "reject_to_wan"}})
		switch path {
		case "missing", "late":
		default:
			records = append(records, accept, tail)
		}
		switch path {
		case "late":
			records = append(records, tail, accept)
		case "missing":
			records = append(records, tail)
		}
		records = append(records, nftRuleRecord("inet", "fw4", "reject_to_wan", "!fw4: reject wan", map[string]any{"reject": nil}))
		runtime, err := parseNFTRuntime(nftJSON(t, records...))
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	if !runtimeFor(443, "10.0.30.20", "exact").hasExactPolicy(want) {
		t.Fatal("exact DNAT rejected")
	}
	if runtimeFor(80, "10.0.30.20", "exact").hasExactPolicy(want) || runtimeFor(443, "10.0.30.99", "exact").hasExactPolicy(want) {
		t.Fatal("same-comment stale DNAT accepted")
	}
	if runtimeFor(443, "10.0.30.20", "missing").hasExactPolicy(want) || runtimeFor(443, "10.0.30.20", "late").hasExactPolicy(want) {
		t.Fatal("DNAT translation without a reachable forward acceptance was treated as enforced")
	}
	for _, path := range []string{"wrong-iif", "late-dispatch", "missing-dstnat"} {
		if runtimeFor(443, "10.0.30.20", path).hasExactPolicy(want) {
			t.Fatalf("DNAT with %s base dispatch was treated as reachable", path)
		}
	}
}

type policyRuntimeCaller struct {
	routes       []map[string]any
	kernel       []map[string]any
	kernelCode   int
	kernelText   string
	execParams   [][]string
	config       string
	configs      map[string]string
	paths        []string
	wanDevice    string
	wanMetric    int
	guestUp      bool
	guestAddress string
}

func (c *policyRuntimeCaller) Call(_ context.Context, object, method string, args any, out any) error {
	var value any
	switch object + "." + method {
	case "network.interface.dump":
		value = map[string]any{"interface": []any{
			map[string]any{"interface": "wan", "l3_device": c.wanDevice, "metric": c.wanMetric, "up": true, "route": c.routes},
			map[string]any{"interface": "oowrt_net_guest", "l3_device": "br-lan.20", "up": c.guestUp,
				"ipv4-address": []any{map[string]any{"address": c.guestAddress, "mask": 24}}},
		}}
	case "file.exec":
		request, _ := args.(map[string]any)
		params, _ := request["params"].([]string)
		c.execParams = append(c.execParams, append([]string(nil), params...))
		if slices.Contains(params, "-j") {
			raw, _ := json.Marshal(c.kernel)
			value = map[string]any{"code": c.kernelCode, "stdout": string(raw)}
		} else {
			value = map[string]any{"code": 0, "stdout": c.kernelText}
		}
	case "service.list":
		paths := c.paths
		if len(paths) == 0 {
			paths = []string{"/var/etc/dnsmasq.conf.main"}
		}
		instances := map[string]any{}
		for i, path := range paths {
			instances[fmt.Sprintf("instance-%d", i)] = map[string]any{
				"running": true, "pid": 123 + i, "command": []string{"/usr/sbin/dnsmasq", "-C", path},
			}
		}
		value = map[string]any{"dnsmasq": map[string]any{"instances": instances}}
	case "file.read":
		path := ""
		if request, ok := args.(map[string]string); ok {
			path = request["path"]
		}
		config := c.config
		if c.configs != nil {
			config = c.configs[path]
		}
		value = map[string]any{"data": config}
	default:
		return nil
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, out)
}

func policyRuntimePlan() *DevicePlan {
	return &DevicePlan{Doc: render.Doc{Sections: []render.Section{
		{Config: "network", Type: "route", Name: "oowrt_policy_1", Values: map[string]string{
			"interface": "wan", "target": "203.0.113.0/24", "gateway": "192.0.2.1", "table": "main", render.OwnershipTag: "1",
		}},
		{Config: "network", Type: "interface", Name: "oowrt_net_guest", Values: map[string]string{
			"ipaddr": "10.0.20.1", "netmask": "255.255.255.0", render.OwnershipTag: "1",
		}},
		{Config: "dhcp", Type: "dhcp", Name: "oowrt_dhcp_guest", Values: map[string]string{
			"interface": "oowrt_net_guest", "start": "20", "limit": "80", "leasetime": "12h", "dhcpv4": "server", render.OwnershipTag: "1",
		}},
		{Config: "dhcp", Type: "host", Name: "oowrt_fixed", Values: map[string]string{
			"mac": "00:11:22:33:44:55", "ip": "10.0.20.50", render.OwnershipTag: "1",
		}},
	}}}
}

func exactPolicyRuntimeCaller() *policyRuntimeCaller {
	return &policyRuntimeCaller{
		routes: []map[string]any{{"target": "203.0.113.0", "mask": 24, "nexthop": "192.0.2.1",
			"source": "0.0.0.0/0", "table": 254}},
		kernel: []map[string]any{{"dst": "203.0.113.0/24", "gateway": "192.0.2.1", "dev": "eth0.2", "metric": 25, "table": "main"}},
		config: "dhcp-range=set:oowrt_net_guest,10.0.20.20,10.0.20.99,255.255.255.0,12h\n" +
			"dhcp-host=00:11:22:33:44:55,10.0.20.50\n",
		wanDevice: "eth0.2", wanMetric: 25, guestUp: true, guestAddress: "10.0.20.1",
	}
}

func TestRouteAndFixedIPRuntimeProofCoversNoOpAndStaleRemoval(t *testing.T) {
	plan := policyRuntimePlan() // no operations: UCI already-matches path
	want, needed, err := buildPolicyRuntimeExpectation(plan)
	if err != nil || !needed || composeManagedRuntimeHealth(plan, nil) == nil {
		t.Fatalf("no-op runtime dependency needed=%v err=%v", needed, err)
	}
	caller := exactPolicyRuntimeCaller()
	if err := want.verifyOnce(context.Background(), caller); err != nil {
		t.Fatalf("exact runtime rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*policyRuntimeCaller)
		want   string
	}{
		{"kernel route absent", func(c *policyRuntimeCaller) { c.kernel = []map[string]any{} }, "route"},
		{"kernel inherited metric differs", func(c *policyRuntimeCaller) { c.kernel[0]["metric"] = 0 }, "route"},
		{"netifd route failed", func(c *policyRuntimeCaller) { c.routes[0]["failed"] = true }, "route"},
		{"netifd source constrained", func(c *policyRuntimeCaller) { c.routes[0]["source"] = "198.51.100.0/24" }, "route"},
		{"wrong route nexthop", func(c *policyRuntimeCaller) { c.routes[0]["nexthop"] = "192.0.2.99" }, "route"},
		{"fixed host absent", func(c *policyRuntimeCaller) {
			c.config = "dhcp-range=set:oowrt_net_guest,10.0.20.20,10.0.20.99,255.255.255.0,12h\n"
		}, "fixed-IP"},
		{"host conditional", func(c *policyRuntimeCaller) {
			c.config = "dhcp-range=set:oowrt_net_guest,10.0.20.20,10.0.20.99,255.255.255.0,12h\n" +
				"dhcp-host=tag:never,00:11:22:33:44:55,10.0.20.50\n"
		}, "conditional"},
		{"exact host without range", func(c *policyRuntimeCaller) { c.config = "dhcp-host=00:11:22:33:44:55,10.0.20.50\n" }, "range"},
		{"foreign broad range cannot substitute", func(c *policyRuntimeCaller) {
			c.config = "dhcp-range=set:foreign,10.0.20.2,10.0.20.250,255.255.255.0,12h\n" +
				"dhcp-host=00:11:22:33:44:55,10.0.20.50\n"
		}, "exact managed"},
		{"managed interface down", func(c *policyRuntimeCaller) { c.guestUp = false }, "managed interface"},
		{"managed interface address differs", func(c *policyRuntimeCaller) { c.guestAddress = "10.0.20.2" }, "managed interface"},
		{"second applicable instance lacks host", func(c *policyRuntimeCaller) {
			c.paths = []string{"/var/etc/dnsmasq.conf.main", "/var/etc/dnsmasq.conf.other"}
			c.configs = map[string]string{
				"/var/etc/dnsmasq.conf.main":  c.config,
				"/var/etc/dnsmasq.conf.other": "dhcp-range=set:other,10.0.20.2,10.0.20.200,255.255.255.0,12h\n",
			}
		}, "applicable dnsmasq instance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := exactPolicyRuntimeCaller()
			test.mutate(caller)
			if err := want.verifyOnce(context.Background(), caller); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe runtime accepted or wrong error: %v", err)
			}
		})
	}
	unrelated := exactPolicyRuntimeCaller()
	unrelated.paths = []string{"/var/etc/dnsmasq.conf.main", "/var/etc/dnsmasq.conf.other"}
	unrelated.configs = map[string]string{
		"/var/etc/dnsmasq.conf.main":  unrelated.config,
		"/var/etc/dnsmasq.conf.other": "dhcp-range=set:other,10.0.30.2,10.0.30.200,255.255.255.0,12h\n",
	}
	if err := want.verifyOnce(context.Background(), unrelated); err != nil {
		t.Fatalf("unrelated dnsmasq instance rejected: %v", err)
	}

	// Desired document removed both owned records; old runtime must disappear.
	removed := &DevicePlan{Existing: render.NewExisting(map[string]map[string]map[string]string{
		"network": {"old_route": {".type": "route", "interface": "wan", "target": "203.0.113.0/24", "gateway": "192.0.2.1", render.OwnershipTag: "1"}},
		"dhcp":    {"old_host": {".type": "host", "mac": "00:11:22:33:44:55", "ip": "10.0.20.50", render.OwnershipTag: "1"}},
	})}
	want, needed, err = buildPolicyRuntimeExpectation(removed)
	if err != nil || !needed {
		t.Fatalf("removed expectation needed=%v err=%v", needed, err)
	}
	caller = exactPolicyRuntimeCaller()
	if err := want.verifyOnce(context.Background(), caller); err == nil || !strings.Contains(err.Error(), "stale managed static route") {
		t.Fatalf("stale route accepted: %v", err)
	}
	scoped := exactPolicyRuntimeCaller()
	scoped.routes[0]["table"] = 123
	scoped.routes[0]["source"] = "198.51.100.0/24"
	scoped.kernel[0]["table"] = 123
	scoped.kernel[0]["from"] = "198.51.100.0/24"
	if err := want.verifyOnce(context.Background(), scoped); err == nil || !strings.Contains(err.Error(), "stale managed static route") {
		t.Fatalf("stale source/table-scoped route accepted: %v", err)
	}
	caller.routes = nil
	caller.kernel = []map[string]any{}
	if err := want.verifyOnce(context.Background(), caller); err == nil || !strings.Contains(err.Error(), "stale managed fixed-IP") {
		t.Fatalf("stale fixed IP accepted: %v", err)
	}
	caller.config = "dhcp-range=set:oowrt_net_guest,10.0.20.20,10.0.20.99,255.255.255.0,12h\n"
	if err := want.verifyOnce(context.Background(), caller); err != nil {
		t.Fatalf("removed runtime rejected: %v", err)
	}
}

func TestStaticRouteRuntimeFallsBackToStockBusyBoxIP(t *testing.T) {
	want, needed, err := buildPolicyRuntimeExpectation(policyRuntimePlan())
	if err != nil || !needed {
		t.Fatalf("expectation needed=%v err=%v", needed, err)
	}
	caller := exactPolicyRuntimeCaller()
	caller.kernelCode = 1 // BusyBox ip v1.37 rejects -j.
	caller.kernelText = strings.Join([]string{
		"default via 192.168.1.1 dev wan src 192.168.1.2",
		"192.0.2.0/24 dev eth0.2 scope link src 192.0.2.2",
		"203.0.113.0/24  via 192.0.2.1 dev eth0.2 metric 25",
		"local 192.0.2.2 dev eth0.2 table local scope host src 192.0.2.2",
		"broadcast 192.0.2.255 dev eth0.2 table local scope link src 192.0.2.2",
	}, "\n") + "\n"
	if err := want.verifyOnce(context.Background(), caller); err != nil {
		t.Fatalf("stock BusyBox route table rejected: %v", err)
	}
	wantCalls := [][]string{
		{"-4", "-j", "route", "show", "table", "all"},
		{"-4", "route", "show", "table", "all"},
	}
	if !slices.EqualFunc(caller.execParams[:2], wantCalls, slices.Equal[[]string]) {
		t.Fatalf("route exec argv = %v, want %v", caller.execParams[:2], wantCalls)
	}
}

func TestBusyBoxRouteProofFailsClosedForScopeTableAndType(t *testing.T) {
	desired, _, err := buildPolicyRuntimeExpectation(policyRuntimePlan())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		line string
	}{
		{"source scoped", "203.0.113.0/24 from 198.51.100.0/24 via 192.0.2.1 dev eth0.2 metric 25"},
		{"foreign table", "203.0.113.0/24 via 192.0.2.1 dev eth0.2 table 123 metric 25"},
		{"non-unicast", "blackhole 203.0.113.0/24 dev eth0.2 metric 25"},
	} {
		t.Run("desired "+test.name, func(t *testing.T) {
			caller := exactPolicyRuntimeCaller()
			caller.kernelCode, caller.kernelText = 1, test.line+"\n"
			if err := desired.verifyOnce(context.Background(), caller); err == nil || !strings.Contains(err.Error(), "absent or differs") {
				t.Fatalf("unsafe desired route accepted: %v", err)
			}
		})
	}

	removedPlan := &DevicePlan{Existing: render.NewExisting(map[string]map[string]map[string]string{
		"network": {"old_route": {".type": "route", "interface": "wan", "target": "203.0.113.0/24",
			"gateway": "192.0.2.1", "metric": "25", render.OwnershipTag: "1"}},
	})}
	removed, needed, err := buildPolicyRuntimeExpectation(removedPlan)
	if err != nil || !needed {
		t.Fatalf("removed expectation needed=%v err=%v", needed, err)
	}
	for _, test := range []struct {
		name string
		line string
	}{
		{"source scoped", "203.0.113.0/24 from 198.51.100.0/24 via 192.0.2.1 dev eth0.2 metric 25"},
		{"foreign table", "203.0.113.0/24 via 192.0.2.1 dev eth0.2 table 123 metric 25"},
		{"non-unicast", "blackhole 203.0.113.0/24 dev eth0.2 metric 25"},
	} {
		t.Run("stale "+test.name, func(t *testing.T) {
			caller := exactPolicyRuntimeCaller()
			caller.routes = nil
			caller.kernelCode, caller.kernelText = 1, test.line+"\n"
			if err := removed.verifyOnce(context.Background(), caller); err == nil || !strings.Contains(err.Error(), "stale managed static route") {
				t.Fatalf("unsafe stale route accepted: %v", err)
			}
		})
	}
}

func TestBusyBoxRouteParserRejectsAmbiguousOrUnboundedOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		result nftExecResult
	}{
		{"exit failure", nftExecResult{Code: 1, Stdout: "usage"}},
		{"empty", nftExecResult{}},
		{"oversized", nftExecResult{Stdout: strings.Repeat("x", nftRulesetLimit+1)}},
		{"bad destination", nftExecResult{Stdout: "not-an-ip via 192.0.2.1 dev eth0\n"}},
		{"bad gateway", nftExecResult{Stdout: "203.0.113.0/24 via nope dev eth0\n"}},
		{"bad metric", nftExecResult{Stdout: "203.0.113.0/24 via 192.0.2.1 dev eth0 metric -1\n"}},
		{"bad source", nftExecResult{Stdout: "203.0.113.0/24 from nope via 192.0.2.1 dev eth0\n"}},
		{"duplicate semantic field", nftExecResult{Stdout: "203.0.113.0/24 via 192.0.2.1 via 192.0.2.2 dev eth0\n"}},
		{"unknown attribute", nftExecResult{Stdout: "203.0.113.0/24 via 192.0.2.1 dev eth0 mystery value\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseKernelIPv4RoutesText(test.result); err == nil {
				t.Fatal("unsafe route output accepted")
			}
		})
	}
}
