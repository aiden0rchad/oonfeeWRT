package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/topology"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func TestTopologyCallsUseExactStockCommandsAndSlowCadence(t *testing.T) {
	now := time.Unix(1_000, 0)
	p := &poller{
		c:      New(newRecorder(), Options{Now: func() time.Time { return now }}),
		target: Target{DeviceID: 1}, topologyBridges: []string{"br-lan"},
		topologyBridgesKnown: true,
	}
	assert := func(calls []call, want int) {
		t.Helper()
		got := 0
		commands := map[string]bool{}
		for _, call := range calls {
			if call.topologySource == "" {
				continue
			}
			got++
			if call.inv.Object == "file" && call.inv.Method == "exec" {
				args := call.inv.Args.(map[string]any)
				commands[fmt.Sprint(args["command"], " ", args["params"])] = true
			}
		}
		if got != want {
			t.Fatalf("topology calls=%d, want %d", got, want)
		}
		if want == 0 {
			return
		}
		for _, exact := range []string{
			"/sbin/ip [-4 neigh show]", "/sbin/ip [-6 neigh show]",
			"/usr/sbin/brctl [showmacs br-lan]", "/usr/sbin/brctl [showstp br-lan]",
			"/usr/sbin/lldpcli [-f json show neighbors hidden]",
		} {
			if !commands[exact] {
				t.Errorf("missing exact command %q in %#v", exact, commands)
			}
		}
	}
	assert(p.buildCalls(Baseline, nil, nil), 8)
	assert(p.buildCalls(Baseline, nil, nil), 0)
	now = now.Add(rediscoverInterval)
	assert(p.buildCalls(Baseline, nil, nil), 8)
}

func TestTopologySelectiveDecodersMapStableRadiosAndPortMediaWithoutSecrets(t *testing.T) {
	snap := Snapshot{DeviceID: 4, At: time.Unix(1_800_000_000, 0)}
	calls := []call{
		{topologySource: TopologySourceNetworkDevices},
		{topologySource: TopologySourceWirelessDevices},
		{topologySource: topology.SourceBridgeFDB},
		{topologySource: TopologySourceBridgeSTP},
	}
	snap.prepareTopology(calls)
	networkRaw := json.RawMessage(`{
	  "br-lan":{"devtype":"bridge","ports":["lan1","phy0-ap0","eth0.1"]},
	  "lan1":{"devtype":"Ethernet device","parent":"eth0"},
	  "eth0":{"devtype":"Ethernet device"},
	  "eth0.1":{"devtype":"VLAN device","parent":"eth0"},
	  "phy0-ap0":{"devtype":"Wireless device"}
	}`)
	if err := decodeTopologyNetworkDevices(networkRaw, &snap); err != nil {
		t.Fatal(err)
	}
	snap.topologyAnswered(TopologySourceNetworkDevices)
	const credentialSentinel = "must-not-survive"
	wirelessRaw := json.RawMessage(`{"radio0":{"up":true,"config":{"band":"5g","channel":36},"interfaces":[
	  {"ifname":"phy0-ap0","section":"wlan","iwinfo":{"bssid":"AA:BB:CC:DD:EE:99"},"config":{"mode":"ap","network":"lan","key":"` + credentialSentinel + `"}}
	]}}`)
	if err := decodeIfaceModes(wirelessRaw, &snap); err != nil {
		t.Fatal(err)
	}
	snap.topologyAnswered(TopologySourceWirelessDevices)
	fdb := json.RawMessage(`{"code":0,"stdout":"port no mac addr is local? ageing timer\n1 aa:bb:cc:dd:ee:01 no 1.0\n2 aa:bb:cc:dd:ee:02 no 2.0\n3 aa:bb:cc:dd:ee:03 no 3.0\n"}`)
	if err := decodeTopologyFDB("br-lan")(fdb, &snap); err != nil {
		t.Fatal(err)
	}
	snap.topologyAnswered(topology.SourceBridgeFDB)
	stp := json.RawMessage(`{"code":0,"stdout":"br-lan\nlan1 (1)\n port id 8001 state forwarding\nphy0-ap0 (2)\n port id 8002 state forwarding\neth0.1 (3)\n port id 8003 state forwarding\n"}`)
	if err := decodeTopologySTP("br-lan")(stp, &snap); err != nil {
		t.Fatal(err)
	}
	snap.topologyAnswered(TopologySourceBridgeSTP)
	snap.finalizeTopology()

	if snap.IfaceRadios["phy0-ap0"] != "radio0" {
		t.Fatalf("stable radio map=%v", snap.IfaceRadios)
	}
	wantMedia := map[int]string{1: "wired", 2: "wireless", 3: "wired"}
	wantAttachment := map[int]string{
		1: topology.PortAttachmentPhysical,
		2: topology.PortAttachmentPhysical,
		3: topology.PortAttachmentAggregate,
	}
	if len(snap.Topology.Bridges) != 1 ||
		!reflect.DeepEqual(snap.Topology.Bridges[0].PortMedia, wantMedia) ||
		!reflect.DeepEqual(snap.Topology.Bridges[0].PortAttachment, wantAttachment) {
		t.Fatalf("bridge=%+v", snap.Topology.Bridges)
	}
	if strings.Contains(fmt.Sprintf("%+v", snap), credentialSentinel) {
		t.Fatal("plaintext wireless key survived selective decoding")
	}
}

func TestTopologySourceAggregationFailsClosedAcrossSeveralBridges(t *testing.T) {
	snap := Snapshot{DeviceID: 8, At: time.Unix(1_800_000_000, 0)}
	snap.prepareTopology([]call{
		{topologySource: topology.SourceBridgeFDB},
		{topologySource: topology.SourceBridgeFDB},
	})
	snap.topologyEvidence(topology.SourceBridgeFDB, 3)
	snap.topologyAnswered(topology.SourceBridgeFDB)
	snap.topologyFailed(topology.SourceBridgeFDB, CauseTransport)
	snap.finalizeTopology()
	if len(snap.Topology.Sources) != 1 ||
		snap.Topology.Sources[0].State != model.TopologySourceError {
		t.Fatalf("sources=%+v", snap.Topology.Sources)
	}
	if got := snap.Topology.Sources[0].Reason; got != "source call failure: transport error" {
		t.Fatalf("reason=%q", got)
	}
}

func TestTopologySourceFailureReasonsPreserveDistinctCauses(t *testing.T) {
	for _, tc := range []struct {
		cause DegradationCause
		want  string
	}{
		{CausePermission, "source call failure: access/permission denied"},
		{CauseUnsupported, "source call failure: unsupported operation"},
		{CauseTransport, "source call failure: transport error"},
		{CauseDecode, "source call failure: decode/invalid data"},
	} {
		t.Run(string(tc.cause), func(t *testing.T) {
			snap := Snapshot{DeviceID: 8, At: time.Unix(1_800_000_000, 0)}
			snap.prepareTopology([]call{{topologySource: topology.SourceNeighbors(4)}})
			snap.topologyFailed(topology.SourceNeighbors(4), tc.cause)
			snap.finalizeTopology()
			if got := snap.Topology.Sources[0].Reason; got != tc.want {
				t.Fatalf("reason=%q, want %q", got, tc.want)
			}
		})
	}

	snap := Snapshot{DeviceID: 8, At: time.Unix(1_800_000_000, 0)}
	snap.prepareTopology([]call{
		{topologySource: topology.SourceBridgeFDB},
		{topologySource: topology.SourceBridgeFDB},
	})
	snap.topologyFailed(topology.SourceBridgeFDB, CauseDecode)
	snap.topologyFailed(topology.SourceBridgeFDB, CausePermission)
	snap.finalizeTopology()
	if got, want := snap.Topology.Sources[0].Reason,
		"source call failure: access/permission denied, decode/invalid data"; got != want {
		t.Fatalf("mixed reason=%q, want %q", got, want)
	}
}

func TestTopologyPermissionDeniedExecPersistsAccessReason(t *testing.T) {
	ctx := context.Background()
	admin := ubus.New(ubus.Options{Host: mockAddr})
	if err := admin.Login(ctx, "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer admin.Close()
	if err := admin.Call(ctx, "__test", "set_acl_gap", map[string]any{
		"pairs": []map[string]string{{"object": "file", "method": "exec"}},
	}, nil); err != nil {
		t.Skipf("mock does not support ACL-gap simulation: %v", err)
	}
	defer admin.Call(ctx, "__test", "set_acl_gap", map[string]any{"pairs": []any{}}, nil)

	now := time.Unix(3_000, 0)
	p := &poller{
		c:                    New(newRecorder(), Options{Now: func() time.Time { return now }, Log: quiet()}),
		target:               Target{DeviceID: 1},
		topologyBridges:      []string{"br-lan"},
		topologyBridgesKnown: true,
		meshAt:               now,
		logAt:                now,
	}
	client, err := mockConnect(t)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap := p.poll(ctx, client, p.target, Baseline, nil, nil)
	if snap.Err != nil {
		t.Fatalf("optional topology denial failed poll: %v", snap.Err)
	}

	want := map[string]bool{
		topology.SourceNeighbors(4): false,
		topology.SourceNeighbors(6): false,
		topology.SourceLLDP:         false,
	}
	for _, source := range snap.Topology.Sources {
		if _, tracked := want[source.Source]; !tracked {
			continue
		}
		want[source.Source] = true
		if source.State != model.TopologySourceError ||
			source.Reason != "source call failure: access/permission denied" {
			t.Errorf("source %s = %s/%q", source.Source, source.State, source.Reason)
		}
	}
	for source, found := range want {
		if !found {
			t.Errorf("missing denied source %s: %+v", source, snap.Topology.Sources)
		}
	}
	for _, degradation := range snap.Degraded {
		if degradation.Object == "file" && degradation.Method == "exec" &&
			degradation.Cause != CausePermission {
			t.Errorf("file.exec degradation lost permission cause: %+v", degradation)
		}
	}
}

func TestSuccessfulNoBridgeInventoryRetiresCachedBridgeSources(t *testing.T) {
	snap := Snapshot{DeviceID: 8, At: time.Unix(1_800_000_000, 0)}
	snap.prepareTopology([]call{
		{topologySource: TopologySourceNetworkDevices},
		{topologySource: topology.SourceBridgeFDB},
		{topologySource: TopologySourceBridgeSTP},
	})
	if err := decodeTopologyNetworkDevices(json.RawMessage(`{}`), &snap); err != nil {
		t.Fatal(err)
	}
	snap.topologyAnswered(TopologySourceNetworkDevices)
	snap.topologyFailed(topology.SourceBridgeFDB, CauseUnsupported)
	snap.topologyFailed(TopologySourceBridgeSTP, CauseUnsupported)
	snap.Topology.Bridges = []topology.BridgeObservation{{DeviceID: 8, Bridge: "br-old"}}
	snap.finalizeTopology()

	if len(snap.Topology.Bridges) != 0 {
		t.Fatalf("removed bridge observations survived: %+v", snap.Topology.Bridges)
	}
	for _, source := range []string{topology.SourceBridgeFDB, TopologySourceBridgeSTP} {
		if got := topologySourceState(snap.Topology.Sources, source); got != model.TopologySourceEmpty {
			t.Fatalf("source %s=%s, want empty; all=%+v", source, got, snap.Topology.Sources)
		}
	}
}

func topologySourceState(sources []model.TopologySourceObservation,
	want string) model.TopologySourceState {
	for _, source := range sources {
		if source.Source == want {
			return source.State
		}
	}
	return ""
}
