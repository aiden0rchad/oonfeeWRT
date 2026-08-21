package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func fw4HealthRecords() []any {
	return []any{
		map[string]any{"metainfo": map[string]any{"version": "1.1.6"}},
		nftChainRecord("inet", "fw4", "input", "input", "drop"),
		nftChainRecord("inet", "fw4", "forward", "forward", "drop"),
		nftChainRecord("inet", "fw4", "input_guest", "", ""),
		nftChainRecord("inet", "fw4", "forward_guest", "", ""),
		nftChainRecord("inet", "fw4", "accept_to_wan", "", ""),
		nftChainRecord("inet", "fw4", "reject_from_guest", "", ""),
		nftChainRecord("inet", "fw4", "reject_to_guest", "", ""),
		nftChainRecord("inet", "fw4", "handle_reject", "", ""),
		nftRuleRecord("inet", "fw4", "input", "!fw4: Handle guest input traffic",
			map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"meta": map[string]any{"key": "iifname"}}, "right": "br-guest"}},
			map[string]any{"jump": map[string]any{"target": "input_guest"}}),
		nftRuleRecord("inet", "fw4", "forward", "!fw4: Handle guest forward traffic",
			map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"meta": map[string]any{"key": "iifname"}}, "right": "br-guest"}},
			map[string]any{"jump": map[string]any{"target": "forward_guest"}}),
		nftRuleRecord("inet", "fw4", "forward_guest", "!fw4: Accept guest to wan forwarding",
			map[string]any{"jump": map[string]any{"target": "accept_to_wan"}}),
		nftRuleRecord("inet", "fw4", "forward_guest", "",
			map[string]any{"jump": map[string]any{"target": "reject_to_guest"}}),
		nftRuleRecord("inet", "fw4", "accept_to_wan", "!fw4: Accept traffic towards wan",
			nftMetaMatch("oifname", "wan"), map[string]any{"accept": nil}),
		nftRuleRecord("inet", "fw4", "reject_from_guest", "!fw4: Reject guest input traffic",
			nftMetaMatch("iifname", "br-guest"), map[string]any{"jump": map[string]any{"target": "handle_reject"}}),
		nftRuleRecord("inet", "fw4", "reject_to_guest", "!fw4: Reject guest traffic",
			nftMetaMatch("oifname", "br-guest"), map[string]any{"jump": map[string]any{"target": "handle_reject"}}),
		nftRuleRecord("inet", "fw4", "handle_reject", "!fw4: Reject traffic",
			nftMetaMatch("l4proto", "tcp"), map[string]any{"reject": map[string]any{"type": "tcp reset"}}),
		nftRuleRecord("inet", "fw4", "handle_reject", "!fw4: Reject traffic",
			map[string]any{"reject": map[string]any{"type": "icmpx port-unreachable"}}),
		nftRuleRecord("inet", "fw4", "input_guest", "!fw4: oonfeeWRT guest DHCP",
			nftMetaMatch("nfproto", "ipv4"), nftMetaMatch("l4proto", "udp"),
			nftPortMatch("udp", "sport", 68), nftPortMatch("udp", "dport", 67),
			map[string]any{"accept": nil}),
		nftRuleRecord("inet", "fw4", "input_guest", "!fw4: oonfeeWRT guest DNS",
			nftMetaMatch("nfproto", "ipv4"), nftMetaMatch("l4proto", "tcp"),
			nftPortMatch("tcp", "dport", 53), map[string]any{"accept": nil}),
		nftRuleRecord("inet", "fw4", "input_guest", "!fw4: oonfeeWRT guest DNS",
			nftMetaMatch("nfproto", "ipv4"), nftMetaMatch("l4proto", "udp"),
			nftPortMatch("udp", "dport", 53), map[string]any{"accept": nil}),
		nftRuleRecord("inet", "fw4", "input_guest", "",
			map[string]any{"jump": map[string]any{"target": "reject_from_guest"}}),
	}
}

func fw4HealthExpectation() *firewallRuntimeExpectation {
	return &firewallRuntimeExpectation{
		touchedSources: map[string]bool{"guest": true},
		desiredZones:   map[string]bool{"guest": true},
		desiredDevices: map[string]map[string]bool{"guest": {"br-guest": true}},
		desiredEdges:   map[string]map[string]bool{"guest": {"wan": true}},
		destinationSet: map[string]bool{"guest": true, "lan": true, "wan": true},
		desiredService: map[string]firewallService{
			"!fw4: oonfeeWRT guest DHCP": {comment: "!fw4: oonfeeWRT guest DHCP", kind: "dhcp", chain: "guest"},
			"!fw4: oonfeeWRT guest DNS":  {comment: "!fw4: oonfeeWRT guest DNS", kind: "dns", chain: "guest"},
		},
		oldService: map[string]bool{},
	}
}

func TestFirewallRuntimeVerifierAcceptsFW4ZoneAndServiceShape(t *testing.T) {
	runtime, err := parseNFTRuntime(nftJSON(t, fw4HealthRecords()...))
	if err != nil {
		t.Fatal(err)
	}
	want := fw4HealthExpectation()
	if err := want.verify(runtime); err != nil {
		t.Fatalf("fw4-shaped runtime rejected: %v", err)
	}
}

func TestFirewallRuntimeVerifierRejectsIncompleteZoneProof(t *testing.T) {
	t.Run("accepting forward fallthrough", func(t *testing.T) {
		records := fw4HealthRecords()
		for _, record := range records {
			object := record.(map[string]any)
			chain, ok := object["chain"].(map[string]any)
			if ok && chain["name"] == "forward" {
				chain["policy"] = "accept"
			}
		}
		assertFirewallRuntimeRejected(t, records, fw4HealthExpectation(), "forward base chain")
	})

	t.Run("extra input dispatch device", func(t *testing.T) {
		records := fw4HealthRecords()
		for _, record := range records {
			object := record.(map[string]any)
			rule, ok := object["rule"].(map[string]any)
			if !ok || rule["comment"] != "!fw4: Handle guest input traffic" {
				continue
			}
			exprs := rule["expr"].([]any)
			match := exprs[0].(map[string]any)["match"].(map[string]any)
			match["op"] = "in"
			match["right"] = map[string]any{"set": []string{"br-guest", "br-stale"}}
		}
		assertFirewallRuntimeRejected(t, records, fw4HealthExpectation(), `still includes device "br-stale"`)
	})

	t.Run("missing input reject tail", func(t *testing.T) {
		records := fw4HealthRecords()
		records = records[:len(records)-1]
		assertFirewallRuntimeRejected(t, records, fw4HealthExpectation(), "no default reject/drop input path")
	})

	t.Run("wrong managed destination device", func(t *testing.T) {
		records := fw4HealthRecords()
		for _, record := range records {
			object := record.(map[string]any)
			rule, ok := object["rule"].(map[string]any)
			if !ok || rule["comment"] != "!fw4: Accept guest to wan forwarding" {
				continue
			}
			exprs := rule["expr"].([]any)
			exprs[0].(map[string]any)["jump"].(map[string]any)["target"] = "accept_to_iot"
		}
		records = append(records,
			nftChainRecord("inet", "fw4", "accept_to_iot", "", ""),
			nftRuleRecord("inet", "fw4", "accept_to_iot", "!fw4: Accept traffic towards iot",
				nftMetaMatch("oifname", "br-wrong"), map[string]any{"accept": nil}),
		)
		want := fw4HealthExpectation()
		want.desiredEdges["guest"] = map[string]bool{"iot": true}
		want.destinationSet["iot"] = true
		want.desiredDevices["iot"] = map[string]bool{"br-iot": true}
		assertFirewallRuntimeRejected(t, records, want, `missing device "br-iot"`)
	})

	for _, dest := range []string{"lan", "iot"} {
		t.Run("stale forwarding to "+dest, func(t *testing.T) {
			records := fw4HealthRecords()
			stale := nftRuleRecord("inet", "fw4", "forward_guest",
				"!fw4: stale guest forwarding",
				map[string]any{"jump": map[string]any{"target": "accept_to_" + dest}})
			for i, record := range records {
				rule, ok := record.(map[string]any)["rule"].(map[string]any)
				if ok && rule["comment"] == "!fw4: Accept guest to wan forwarding" {
					i++
					records = append(records[:i], append([]any{stale}, records[i:]...)...)
					break
				}
			}
			records = append(records,
				nftChainRecord("inet", "fw4", "accept_to_"+dest, "", ""),
				nftRuleRecord("inet", "fw4", "accept_to_"+dest,
					"!fw4: Accept traffic towards "+dest,
					nftMetaMatch("oifname", "br-"+dest), map[string]any{"accept": nil}),
			)
			want := fw4HealthExpectation()
			want.destinationSet[dest] = true
			assertFirewallRuntimeRejected(t, records, want,
				"stale managed forwarding guest -> "+dest)
		})
	}
}

func assertFirewallRuntimeRejected(t *testing.T, records []any, want *firewallRuntimeExpectation, detail string) {
	t.Helper()
	runtime, err := parseNFTRuntime(nftJSON(t, records...))
	if err != nil {
		t.Fatal(err)
	}
	if err := want.verify(runtime); err == nil || !strings.Contains(err.Error(), detail) {
		t.Fatalf("verify error=%v, want %q", err, detail)
	}
}

func nftMetaMatch(key string, right any) map[string]any {
	return map[string]any{"match": map[string]any{
		"op": "==", "left": map[string]any{"meta": map[string]any{"key": key}}, "right": right,
	}}
}

func nftPortMatch(protocol, field string, right any) map[string]any {
	return map[string]any{"match": map[string]any{
		"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": protocol, "field": field}}, "right": right,
	}}
}

func TestFirewallRuntimeHealthTreatsUnreadableEvidenceAsPermanent(t *testing.T) {
	want := &firewallRuntimeExpectation{settle: time.Second}
	tests := []recordingNFTCaller{
		{err: errors.New("access denied")},
		{result: nftExecResult{Code: 1}},
		{result: nftExecResult{Stdout: "{"}},
	}
	for i := range tests {
		started := time.Now()
		err := want.wait(context.Background(), &tests[i])
		if err == nil || tests[i].calls != 1 {
			t.Fatalf("case %d err=%v calls=%d", i, err, tests[i].calls)
		}
		if elapsed := time.Since(started); elapsed >= firewallPollEvery {
			t.Fatalf("case %d retried permanent failure for %v", i, elapsed)
		}
	}
}

func TestFirewallRuntimeExpectationCoversTouchedManagedWLAN(t *testing.T) {
	doc := managedFirewallDoc(1, true, true)
	doc.Sections = append(doc.Sections, render.Section{
		Config: "wireless", Type: "wifi-iface", Name: "oowrt_wlan1_radio0",
		Values: map[string]string{
			"network": "oowrt_net_guest", render.OwnershipTag: "1",
		},
	})
	plan := &DevicePlan{Doc: doc, Plan: applyengine.Plan{Ops: []applyengine.Op{{
		Kind: applyengine.OpAdd, Config: "wireless", Section: "oowrt_wlan1_radio0",
	}}}}
	want, needed, err := buildFirewallRuntimeExpectation(plan)
	if err != nil || !needed {
		t.Fatalf("touched managed WLAN firewall expectation: needed=%v err=%v", needed, err)
	}
	if !want.desiredZones["guest"] || !want.desiredEdges["guest"]["wan"] ||
		len(want.desiredService) != 2 {
		t.Fatalf("touched WLAN did not pull in its complete firewall policy: %+v", want)
	}
}

func managedFirewallDoc(deviceID int64, forwarding, services bool) render.Doc {
	return managedFirewallDocOnDevice(deviceID, forwarding, services, "br-guest")
}

func managedFirewallDocOnDevice(deviceID int64, forwarding, services bool, device string) render.Doc {
	doc := render.Doc{DeviceID: deviceID, Sections: []render.Section{
		{
			Config: "network", Type: "interface", Name: "oowrt_net_guest",
			Values: map[string]string{
				"proto": "static", "device": device, "ipaddr": "192.168.45.1",
				"netmask": "255.255.255.0", render.OwnershipTag: "1",
			},
		},
		{
			Config: "firewall", Type: "zone", Name: "oowrt_zone_guest",
			Values: map[string]string{
				"name": "guest", "input": "REJECT", "output": "ACCEPT",
				"forward": "REJECT", render.OwnershipTag: "1",
			},
			Lists: map[string][]string{"network": {"oowrt_net_guest"}},
		},
	}}
	if forwarding {
		doc.Sections = append(doc.Sections, render.Section{
			Config: "firewall", Type: "forwarding", Name: "oowrt_fwd_guest_wan",
			Values: map[string]string{
				"src": "guest", "dest": "wan", render.OwnershipTag: "1",
			},
		})
	}
	if services {
		doc.Sections = append(doc.Sections,
			render.Section{
				Config: "firewall", Type: "rule", Name: "oowrt_in_guest_dhcp",
				Values: map[string]string{
					"name": "oonfeeWRT guest DHCP", "src": "guest",
					"proto": "udp", "src_port": "68", "dest_port": "67",
					"target": "ACCEPT", "family": "ipv4", render.OwnershipTag: "1",
				},
			},
			render.Section{
				Config: "firewall", Type: "rule", Name: "oowrt_in_guest_dns",
				Values: map[string]string{
					"name": "oonfeeWRT guest DNS", "src": "guest",
					"dest_port": "53", "target": "ACCEPT", "family": "ipv4",
					render.OwnershipTag: "1",
				},
				Lists: map[string][]string{"proto": {"tcp", "udp"}},
			},
		)
	}
	return doc
}

func managedFirewallPlan(t *testing.T, cExisting render.Existing, doc render.Doc, timeout time.Duration) *DevicePlan {
	t.Helper()
	plan := doc.Plan(cExisting)
	plan.Ops = append(plan.Ops, doc.Prune(cExisting)...)
	plan.AcknowledgeTraversal = true
	plan.Timeout = timeout
	return &DevicePlan{Doc: doc, Existing: cExisting, Plan: plan}
}

func setNFTRuntime(t *testing.T, c interface {
	Call(context.Context, string, string, any, any) error
}, mode string, reads int) {
	t.Helper()
	if err := c.Call(context.Background(), "__test", "set_nft_runtime", map[string]any{
		"mode": mode, "reads": reads,
	}, nil); err != nil {
		t.Fatalf("set nft runtime %s: %v", mode, err)
	}
}

func TestManagedFirewallPostApplyHealthRejectsStaleRuntimeAndSettles(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)

	existing, err := ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	allow := managedFirewallDoc(dev.ID, true, true)
	allowPlan := managedFirewallPlan(t, existing, allow, 4*time.Second)
	res, err := r.Apply(ctx, c, dev.ID, allowPlan, nil)
	if err != nil || res.Outcome != applyengine.Applied {
		t.Fatalf("initial live firewall apply: result=%+v err=%v", res, err)
	}

	// A no-op still has to prove the runtime it is about to call Applied. Make
	// nft empty and ensure neither device config nor ownership crosses a write
	// boundary. Firewall proof fails first, before the caller's later checks.
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	noOp := managedFirewallPlan(t, existing, allow, 400*time.Millisecond)
	if !noOp.Empty() {
		t.Fatalf("fixture should be a no-op: %+v", noOp.Plan.Ops)
	}
	if err := db.ReplaceOwned(ctx, dev.ID, nil); err != nil {
		t.Fatal(err)
	}
	callerHealthRan := false
	setNFTRuntime(t, c, "empty", 0)
	res, err = r.Apply(ctx, c, dev.ID, noOp, func(context.Context, *ubus.Client) error {
		callerHealthRan = true
		return nil
	})
	if err == nil || res.Outcome != "" || res.HealthErr == nil || callerHealthRan ||
		!strings.Contains(err.Error(), "no configuration was written") {
		t.Fatalf("unhealthy no-op was not a truthful no-write failure: result=%+v err=%v caller_health=%v", res, err, callerHealthRan)
	}
	if owned, ownedErr := db.OwnedSections(ctx, dev.ID); ownedErr != nil || len(owned) != 0 {
		t.Fatalf("unhealthy no-op crossed ownership boundary: owned=%+v err=%v", owned, ownedErr)
	}
	setNFTRuntime(t, c, "live", 0)
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if retry := managedFirewallPlan(t, existing, allow, 4*time.Second); !retry.Empty() {
		t.Fatalf("unhealthy no-op changed UCI: %+v", retry.Plan.Ops)
	}

	// The firewall UCI can remain byte-for-byte unchanged while the managed L3
	// device moves. A network-only plan must still require fw4's iif/oif
	// dispatch to follow that device change.
	moved := managedFirewallDocOnDevice(dev.ID, true, true, "br-guest-next")
	movedPlan := managedFirewallPlan(t, existing, moved, time.Second)
	for _, op := range movedPlan.Plan.Ops {
		if op.Config != "network" {
			t.Fatalf("device-move fixture unexpectedly changes %s.%s", op.Config, op.Section)
		}
	}
	setNFTRuntime(t, c, "stale", 0)
	res, err = r.Apply(ctx, c, dev.ID, movedPlan, nil)
	if err != nil || res.Outcome == applyengine.Applied || res.HealthErr == nil ||
		!strings.Contains(res.HealthErr.Error(), `missing device "br-guest-next"`) {
		t.Fatalf("stale network-only firewall dispatch was confirmed: result=%+v err=%v", res, err)
	}
	setNFTRuntime(t, c, "live", 0)
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if existing.In("network")["oowrt_net_guest"]["device"] != "br-guest" {
		t.Fatalf("failed network-only health did not roll back: %+v", existing.In("network")["oowrt_net_guest"])
	}

	// Freeze nft at Allow, then commit a Block plan. UCI says the forwarding
	// is gone, but the old runtime edge remains; health must not confirm it.
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	block := managedFirewallDoc(dev.ID, false, true)
	blockPlan := managedFirewallPlan(t, existing, block, time.Second)
	setNFTRuntime(t, c, "stale", 0)
	res, err = r.Apply(ctx, c, dev.ID, blockPlan, nil)
	if err != nil {
		t.Fatalf("stale runtime apply returned transport error: %v", err)
	}
	if res.Outcome == applyengine.Applied || res.HealthErr == nil ||
		!strings.Contains(res.HealthErr.Error(), "stale managed forwarding guest -> wan") {
		t.Fatalf("stale forwarding was confirmed: %+v", res)
	}
	setNFTRuntime(t, c, "live", 0)
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, restored := existing.In("firewall")["oowrt_fwd_guest_wan"]; !restored {
		t.Fatal("failed health did not allow rollback to restore the forwarding")
	}

	// A one-read stale view is tolerated. The settled Block runtime must have
	// a reachable managed zone chain and an unconditional reject/drop tail;
	// an empty/missing ruleset is not evidence that Block landed.
	blockPlan = managedFirewallPlan(t, existing, block, 4*time.Second)
	setNFTRuntime(t, c, "lag", 1)
	res, err = r.Apply(ctx, c, dev.ID, blockPlan, nil)
	if err != nil || res.Outcome != applyengine.Applied {
		t.Fatalf("settled block-all runtime: result=%+v err=%v", res, err)
	}

	// Freeze nft with the old DHCP/DNS accepts, then delete those managed UCI
	// rules. Their continued runtime presence is also a failed apply.
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	noServices := managedFirewallDoc(dev.ID, false, false)
	noServicesPlan := managedFirewallPlan(t, existing, noServices, time.Second)
	setNFTRuntime(t, c, "stale", 0)
	res, err = r.Apply(ctx, c, dev.ID, noServicesPlan, nil)
	if err != nil {
		t.Fatalf("stale service runtime apply returned transport error: %v", err)
	}
	if res.Outcome == applyengine.Applied || res.HealthErr == nil ||
		!strings.Contains(res.HealthErr.Error(), "stale managed service rule") {
		t.Fatalf("stale DHCP/DNS accepts were confirmed: %+v", res)
	}
	setNFTRuntime(t, c, "live", 0)
	existing, err = ReadExisting(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"oowrt_in_guest_dhcp", "oowrt_in_guest_dns"} {
		if _, restored := existing.In("firewall")[section]; !restored {
			t.Fatalf("failed health did not allow rollback to restore %s", section)
		}
	}
}

func TestFirewallPostApplyHealthOnlyWrapsOwnedFirewallOps(t *testing.T) {
	unrelated := &DevicePlan{Plan: applyengine.Plan{Ops: []applyengine.Op{{
		Kind: applyengine.OpSet, Config: "wireless", Section: "radio0",
		Values: map[string]string{"channel": "44"},
	}}}}
	next := applyengine.HealthCheck(func(context.Context, *ubus.Client) error { return nil })
	if got := composeFirewallRuntimeHealth(unrelated, next); got == nil {
		t.Fatal("unrelated plan lost its caller health check")
	}

	foreign := &DevicePlan{
		Doc: render.Doc{Sections: []render.Section{{
			Config: "firewall", Type: "rule", Name: "human",
			Values: map[string]string{"target": "ACCEPT"},
		}}},
		Plan: applyengine.Plan{Ops: []applyengine.Op{{
			Kind: applyengine.OpSet, Config: "firewall", Section: "human",
		}}},
	}
	if got := composeFirewallRuntimeHealth(foreign, nil); got != nil {
		t.Fatal("foreign firewall operation unexpectedly gained controller-owned health dependency")
	}
}
