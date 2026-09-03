package topology

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

const (
	wrtMAC   = "02:00:00:00:00:01"
	c6MAC    = "02:00:00:00:00:02"
	c6Alias1 = "02:00:00:00:00:12"
	c6Alias2 = "02:00:00:00:00:22"
)

func observedSource(deviceID int64, source string) model.TopologySourceObservation {
	return model.TopologySourceObservation{
		DeviceID: deviceID, Source: source, State: model.TopologySourceObserved,
		ObservedAt: 1_787_100_000_000,
	}
}

func TestInferResolvesSeveralC6MACsToOneStableDeviceNode(t *testing.T) {
	input := InferenceInput{
		At: 1_787_100_000_000,
		Devices: []InventoryDevice{
			{ID: 1, Name: "WRT", PrimaryMAC: wrtMAC},
			{ID: 2, Name: "C6", PrimaryMAC: c6MAC, Aliases: []string{c6Alias1, c6Alias2}},
		},
		Bridges: []BridgeObservation{{
			DeviceID: 1, Bridge: "br-lan",
			Entries: []FDBEntry{
				{Port: 2, MAC: c6Alias1},
				{Port: 2, MAC: c6Alias2},
				{Port: 1, MAC: wrtMAC, Local: true},
			},
			STP:       &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "lan2", Port: 2, State: "forwarding"}}},
			PortMedia: map[int]string{2: "wired"},
		}},
		Neighbors: map[int64][]Neighbor{1: {{
			Family: 4, Address: "192.168.1.2", Interface: "br-lan", MAC: c6Alias1, State: "reachable",
		}}},
		Sources: []model.TopologySourceObservation{
			observedSource(1, SourceBridgeFDB),
			observedSource(1, SourceNeighbors(4)),
			{DeviceID: 1, Source: SourceLLDP, State: model.TopologySourceUnknown, Reason: "not installed", ObservedAt: 1_787_100_000_000},
		},
	}
	result, err := Infer(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 {
		t.Fatalf("want one merged device edge, got %#v", result.Edges)
	}
	edge := result.Edges[0]
	if edge.ChildNode != "device:"+c6MAC || edge.ParentNode != "device:"+wrtMAC || edge.ParentPort != "lan2" {
		t.Fatalf("edge = %#v", edge)
	}
	if edge.Confidence != "inferred" {
		t.Errorf("confidence = %q; FDB proves a path through the port, not direct attachment", edge.Confidence)
	}
	evidence := edge.Evidence
	if len(evidence) != 3 {
		t.Fatalf("want two FDB aliases plus ARP corroboration, got %#v", evidence)
	}
	encodedBytes, _ := json.Marshal(edge.Evidence)
	encoded := string(encodedBytes)
	if !strings.Contains(encoded, c6Alias1) || !strings.Contains(encoded, c6Alias2) {
		t.Fatalf("alias provenance missing: %s", encoded)
	}
	if len(edge.Ambiguities) != 0 {
		t.Fatalf("intrinsic missing VLAN metadata became an edge ambiguity: %#v", edge.Ambiguities)
	}
	if result.Complete {
		t.Fatal("unavailable LLDP was reported complete")
	}
	if len(result.Gaps) != 1 || !strings.Contains(result.Gaps[0], "lldp: not installed") {
		t.Fatalf("gaps = %#v", result.Gaps)
	}
	if edge.Evidence[0].Detail["vlan_available"] != false {
		t.Fatalf("missing VLAN metadata is not explicit in evidence: %#v", edge.Evidence)
	}
}

func TestInferDoesNotInventMediumFromBridgeOrInterfaceName(t *testing.T) {
	result, err := Infer(InferenceInput{
		At:      1000,
		Devices: []InventoryDevice{{ID: 1, PrimaryMAC: wrtMAC}},
		Bridges: []BridgeObservation{{
			DeviceID: 1, Bridge: "br-lan", Entries: []FDBEntry{{Port: 2, MAC: c6MAC}},
			STP: &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "lan2", Port: 2, State: "forwarding"}}},
		}},
		Sources: []model.TopologySourceObservation{observedSource(1, SourceBridgeFDB)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].Medium != "unknown" ||
		!strings.Contains(strings.Join(result.Edges[0].Ambiguities, " "), "does not identify link medium") ||
		!strings.Contains(strings.Join(result.Gaps, " "), "/medium:") {
		t.Fatalf("unmeasured medium was not preserved as unknown: %#v", result)
	}
}

func TestInferDoesNotMACDedupeTwoManagedDevices(t *testing.T) {
	shared := "02:00:00:00:00:99"
	result, err := Infer(InferenceInput{
		At: 1000,
		Devices: []InventoryDevice{
			{ID: 1, PrimaryMAC: wrtMAC},
			{ID: 2, PrimaryMAC: c6MAC, Aliases: []string{shared}},
			{ID: 3, PrimaryMAC: "02:00:00:00:00:03", Aliases: []string{shared}},
		},
		Bridges: []BridgeObservation{{
			DeviceID: 1, Bridge: "br-lan", Entries: []FDBEntry{{Port: 2, MAC: shared}},
		}},
		Sources: []model.TopologySourceObservation{observedSource(1, SourceBridgeFDB)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].ChildNode != "mac:"+shared {
		t.Fatalf("a shared alias was attached to one device: %#v", result.Edges)
	}
	if !strings.Contains(strings.Join(result.Edges[0].Ambiguities, " "), "multiple managed devices") {
		t.Fatalf("identity ambiguity absent: %v", result.Edges[0].Ambiguities)
	}
}

func TestInferRejectsDuplicateInventoryIdentity(t *testing.T) {
	_, err := Infer(InferenceInput{
		At: 1000,
		Devices: []InventoryDevice{
			{ID: 1, PrimaryMAC: wrtMAC},
			{ID: 2, PrimaryMAC: wrtMAC},
		},
	})
	if err == nil {
		t.Fatal("duplicate inventory MAC silently merged two managed devices")
	}
}

func TestInferWirelessAssociationIsMeasuredAndUsesClientIdentity(t *testing.T) {
	client := "02:00:00:00:00:44"
	result, err := Infer(InferenceInput{
		At:           2000,
		Devices:      []InventoryDevice{{ID: 2, PrimaryMAC: c6MAC}},
		Clients:      []InventoryClient{{MAC: client, Name: "Laptop"}},
		Associations: []Association{{DeviceID: 2, Interface: "phy0-ap0", MAC: client}},
		Sources:      []model.TopologySourceObservation{observedSource(2, SourceAssociations)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Edges) != 1 {
		t.Fatalf("result = %#v", result)
	}
	edge := result.Edges[0]
	if edge.ChildNode != "client:"+client || edge.ParentNode != "device:"+c6MAC ||
		edge.Medium != "wireless" || edge.Confidence != "measured" {
		t.Fatalf("edge = %#v", edge)
	}
}

func TestInferOptionalLLDPAbsenceRemainsAnExplicitGap(t *testing.T) {
	result, err := Infer(InferenceInput{
		At:      3000,
		Devices: []InventoryDevice{{ID: 1, PrimaryMAC: wrtMAC}},
		Sources: []model.TopologySourceObservation{{
			DeviceID: 1, Source: SourceLLDP, State: model.TopologySourceError,
			Reason: "  package   unavailable\nwithout install  ", ObservedAt: 3000,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Edges) != 0 {
		t.Fatalf("result = %#v", result)
	}
	want := "device:1/lldp: package unavailable without install"
	if len(result.Gaps) != 1 || result.Gaps[0] != want {
		t.Fatalf("gaps = %#v, want %q", result.Gaps, want)
	}
}

func TestInferPreservesCompetingParentsInsteadOfPickingOne(t *testing.T) {
	client := "02:00:00:00:00:55"
	result, err := Infer(InferenceInput{
		At: 4000,
		Devices: []InventoryDevice{
			{ID: 1, PrimaryMAC: wrtMAC},
			{ID: 2, PrimaryMAC: c6MAC},
		},
		Clients: []InventoryClient{{MAC: client}},
		Bridges: []BridgeObservation{
			{DeviceID: 1, Bridge: "br-lan", Entries: []FDBEntry{{Port: 2, MAC: client}},
				STP:       &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "lan2", Port: 2}}},
				PortMedia: map[int]string{2: "wired"}, PortAttachment: map[int]string{2: PortAttachmentPhysical}},
			{DeviceID: 2, Bridge: "br-lan", Entries: []FDBEntry{{Port: 1, MAC: client}},
				STP:       &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "lan1", Port: 1}}},
				PortMedia: map[int]string{1: "wired"}, PortAttachment: map[int]string{1: PortAttachmentPhysical}},
		},
		Sources: []model.TopologySourceObservation{
			observedSource(1, SourceBridgeFDB), observedSource(2, SourceBridgeFDB),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 2 {
		t.Fatalf("competing parents collapsed: %#v", result.Edges)
	}
	for _, edge := range result.Edges {
		if edge.Confidence != "ambiguous" || !strings.Contains(strings.Join(edge.Ambiguities, " "), "multiple candidate parents") {
			t.Fatalf("ambiguity lost: %#v", edge)
		}
	}
}

func TestInferMarksPossibleFDBTransitOnFreshManagedLLDPPort(t *testing.T) {
	client := "02:00:00:00:00:55"
	third := "02:00:00:00:00:03"
	devices := []InventoryDevice{
		{ID: 1, PrimaryMAC: wrtMAC},
		{ID: 2, PrimaryMAC: c6MAC},
		{ID: 3, PrimaryMAC: third},
	}
	bridge := func(deviceID int64, peer, clientPort int, peerMAC, peerName, clientName string) BridgeObservation {
		return BridgeObservation{
			DeviceID: deviceID, Bridge: "br-lan",
			Entries: []FDBEntry{{Port: peer, MAC: peerMAC}, {Port: clientPort, MAC: client}},
			STP: &STPState{Bridge: "br-lan", Ports: []STPPort{
				{Name: peerName, Port: peer, State: "forwarding"},
				{Name: clientName, Port: clientPort, State: "forwarding"},
			}},
			PortMedia: map[int]string{peer: "wired", clientPort: "wired"},
			PortAttachment: map[int]string{
				peer: PortAttachmentPhysical, clientPort: PortAttachmentPhysical,
			},
		}
	}

	for _, tt := range []struct {
		name        string
		deviceID    int64
		peerMAC     string
		peerPort    string
		clientPort  int
		wantParent  string
		wantPort    string
		wantTransit bool
	}{
		{
			name:     "gateway does not claim a client beyond the downstream switch",
			deviceID: 1, peerMAC: c6MAC, peerPort: "eth1", clientPort: 1,
			wantParent: "device:" + wrtMAC, wantPort: "eth1", wantTransit: true,
		},
		{
			name:     "switch does not claim an upstream Wi-Fi client",
			deviceID: 2, peerMAC: wrtMAC, peerPort: "eth0", clientPort: 1,
			wantParent: "device:" + c6MAC, wantPort: "eth0", wantTransit: true,
		},
		{
			name:     "unrelated local port remains a candidate",
			deviceID: 2, peerMAC: wrtMAC, peerPort: "eth0", clientPort: 2,
			wantParent: "device:" + c6MAC, wantPort: "eth1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			peerNumber := 1
			clientName := tt.peerPort
			if tt.clientPort != peerNumber {
				clientName = "eth1"
			}
			input := InferenceInput{
				At: 4000, Devices: devices, Clients: []InventoryClient{{MAC: client}},
				Bridges: []BridgeObservation{bridge(
					tt.deviceID, peerNumber, tt.clientPort, tt.peerMAC, tt.peerPort, clientName,
				)},
				LLDP: []LLDPLink{{DeviceID: tt.deviceID, Port: tt.peerPort, RemoteMAC: tt.peerMAC}},
				Sources: []model.TopologySourceObservation{
					{DeviceID: tt.deviceID, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: 4000},
					{DeviceID: tt.deviceID, Source: SourceBridgeSTP, State: model.TopologySourceObserved, ObservedAt: 4000},
					{DeviceID: tt.deviceID, Source: SourceLLDP, State: model.TopologySourceObserved, ObservedAt: 4000},
				},
			}
			result, err := Infer(input)
			if err != nil {
				t.Fatal(err)
			}
			var clientEdges []model.TopologyEdge
			var peerEdge *model.TopologyEdge
			for _, edge := range result.Edges {
				if edge.ChildNode == "client:"+client {
					clientEdges = append(clientEdges, edge)
				}
				if edge.ChildMAC == tt.peerMAC {
					copy := edge
					peerEdge = &copy
				}
			}
			if peerEdge == nil || peerEdge.Confidence != "measured" || len(peerEdge.Ambiguities) != 0 {
				t.Fatalf("matching FDB and LLDP did not produce one measured peer edge: %#v", result.Edges)
			}
			if len(clientEdges) != 1 || clientEdges[0].ParentNode != tt.wantParent ||
				clientEdges[0].ParentPort != tt.wantPort {
				t.Fatalf("local placement = %#v", clientEdges)
			}
			marked, _ := clientEdges[0].Evidence[0].Detail[managedLinkTransitDetail].(bool)
			if tt.wantTransit {
				if clientEdges[0].Confidence != "ambiguous" || !marked ||
					!strings.Contains(strings.Join(clientEdges[0].Ambiguities, " "), managedLinkTransitAmbiguity) {
					t.Fatalf("possible transit evidence was not retained as ambiguous: %#v", clientEdges[0])
				}
			} else if marked || clientEdges[0].Confidence != "inferred" {
				t.Fatalf("unrelated local placement was marked as transit: %#v", clientEdges[0])
			}
		})
	}

	for _, tt := range []struct {
		name       string
		state      model.TopologySourceState
		observedAt int64
	}{
		{name: "failed LLDP coverage", state: model.TopologySourceError, observedAt: 4000},
		{name: "stale LLDP coverage", state: model.TopologySourceObserved, observedAt: 3999},
		{name: "unknown LLDP coverage", state: model.TopologySourceUnknown, observedAt: 4000},
	} {
		t.Run(tt.name+" cannot suppress FDB", func(t *testing.T) {
			result, err := Infer(InferenceInput{
				At: 4000, Devices: devices, Clients: []InventoryClient{{MAC: client}},
				Bridges: []BridgeObservation{bridge(1, 1, 1, c6MAC, "eth1", "eth1")},
				LLDP:    []LLDPLink{{DeviceID: 1, Port: "eth1", RemoteMAC: c6MAC}},
				Sources: []model.TopologySourceObservation{
					{DeviceID: 1, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: 4000},
					{DeviceID: 1, Source: SourceBridgeSTP, State: model.TopologySourceObserved, ObservedAt: 4000},
					{DeviceID: 1, Source: SourceLLDP, State: tt.state, ObservedAt: tt.observedAt},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, edge := range result.Edges {
				found = found || edge.ChildNode == "client:"+client
				for _, evidence := range edge.Evidence {
					if evidence.Kind == "lldp_neighbor" {
						t.Fatalf("unusable LLDP emitted measured evidence: %#v", result.Edges)
					}
				}
			}
			if !found {
				t.Fatalf("unusable LLDP erased FDB evidence: %#v", result.Edges)
			}
		})
	}

	t.Run("multiple managed peers on one port remain ambiguous", func(t *testing.T) {
		result, err := Infer(InferenceInput{
			At: 4000, Devices: devices, Clients: []InventoryClient{{MAC: client}},
			Bridges: []BridgeObservation{bridge(1, 1, 1, c6MAC, "eth1", "eth1")},
			LLDP: []LLDPLink{
				{DeviceID: 1, Port: "eth1", RemoteMAC: c6MAC},
				{DeviceID: 1, Port: "eth1", RemoteMAC: third},
			},
			Sources: []model.TopologySourceObservation{
				{DeviceID: 1, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: 4000},
				{DeviceID: 1, Source: SourceBridgeSTP, State: model.TopologySourceObserved, ObservedAt: 4000},
				{DeviceID: 1, Source: SourceLLDP, State: model.TopologySourceObserved, ObservedAt: 4000},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var found *model.TopologyEdge
		for _, edge := range result.Edges {
			if edge.ChildNode == "client:"+client {
				copy := edge
				found = &copy
			}
		}
		if found == nil || found.Confidence != "ambiguous" ||
			!strings.Contains(strings.Join(found.Ambiguities, " "), "multiple managed LLDP peers") {
			t.Fatalf("ambiguous shared segment was collapsed: %#v", result.Edges)
		}
	})

	for _, tt := range []struct {
		name       string
		attachment string
		state      string
		fdbAt      int64
		stpAt      int64
	}{
		{name: "aggregate port", attachment: PortAttachmentAggregate, state: "forwarding", fdbAt: 4000, stpAt: 4000},
		{name: "blocked physical port", attachment: PortAttachmentPhysical, state: "blocking", fdbAt: 4000, stpAt: 4000},
		{name: "disabled physical port", attachment: PortAttachmentPhysical, state: "disabled", fdbAt: 4000, stpAt: 4000},
		{name: "stale FDB source", attachment: PortAttachmentPhysical, state: "forwarding", fdbAt: 3999, stpAt: 4000},
		{name: "stale STP source", attachment: PortAttachmentPhysical, state: "forwarding", fdbAt: 4000, stpAt: 3999},
	} {
		t.Run(tt.name+" is not classified as transit", func(t *testing.T) {
			observation := bridge(1, 1, 1, c6MAC, "eth1", "eth1")
			observation.PortAttachment[1] = tt.attachment
			for i := range observation.STP.Ports {
				observation.STP.Ports[i].State = tt.state
			}
			result, err := Infer(InferenceInput{
				At: 4000, Devices: devices, Clients: []InventoryClient{{MAC: client}},
				Bridges: []BridgeObservation{observation},
				LLDP:    []LLDPLink{{DeviceID: 1, Port: "eth1", RemoteMAC: c6MAC}},
				Sources: []model.TopologySourceObservation{
					{DeviceID: 1, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: tt.fdbAt},
					{DeviceID: 1, Source: SourceBridgeSTP, State: model.TopologySourceObserved, ObservedAt: tt.stpAt},
					{DeviceID: 1, Source: SourceLLDP, State: model.TopologySourceObserved, ObservedAt: 4000},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, edge := range result.Edges {
				if edge.ChildNode != "client:"+client {
					continue
				}
				if marked, _ := edge.Evidence[0].Detail[managedLinkTransitDetail].(bool); marked {
					t.Fatalf("unsafe port was marked as managed-link transit: %#v", edge)
				}
				return
			}
			t.Fatalf("sole FDB evidence disappeared: %#v", result.Edges)
		})
	}
}

func TestInferRejectsAggregateTransitObservationContradictedByPhysicalPorts(t *testing.T) {
	client := "02:00:00:00:00:55"
	result, err := Infer(InferenceInput{
		At: 4000,
		Devices: []InventoryDevice{
			{ID: 1, PrimaryMAC: wrtMAC},
			{ID: 2, PrimaryMAC: c6MAC},
		},
		Clients: []InventoryClient{{MAC: client}},
		Bridges: []BridgeObservation{
			{
				DeviceID: 1, Bridge: "br-lan",
				Entries: []FDBEntry{{Port: 1, MAC: client}, {Port: 3, MAC: c6MAC}},
				STP: &STPState{Bridge: "br-lan", Ports: []STPPort{
					{Name: "lan1", Port: 1}, {Name: "lan3", Port: 3},
				}},
				PortMedia:      map[int]string{1: "wired", 3: "wired"},
				PortAttachment: map[int]string{1: PortAttachmentPhysical, 3: PortAttachmentPhysical},
			},
			{
				DeviceID: 2, Bridge: "br-lan", Entries: []FDBEntry{{Port: 1, MAC: client}},
				STP:            &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "eth0.1", Port: 1}}},
				PortMedia:      map[int]string{1: "wired"},
				PortAttachment: map[int]string{1: PortAttachmentAggregate},
			},
		},
		Sources: []model.TopologySourceObservation{
			observedSource(1, SourceBridgeFDB), observedSource(2, SourceBridgeFDB),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 2 {
		t.Fatalf("aggregate transit observation survived: %#v", result.Edges)
	}
	want := map[string]string{
		"client:" + client: "device:" + wrtMAC + "\x00lan1",
		"device:" + c6MAC:  "device:" + wrtMAC + "\x00lan3",
	}
	for _, edge := range result.Edges {
		if got := edge.ParentNode + "\x00" + edge.ParentPort; got != want[edge.ChildNode] {
			t.Fatalf("edge = %#v", edge)
		}
	}
}

func TestInferAssociationSuppressesAggregateWithoutCollapsingBSS(t *testing.T) {
	client := "02:00:00:00:00:55"
	for _, tt := range []struct {
		name   string
		ifaces []string
	}{
		{name: "one BSS", ifaces: []string{"phy0-ap0"}},
		{name: "same-device multi-BSS", ifaces: []string{"phy0-ap0", "phy1-ap0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := InferenceInput{
				At: 4000,
				Devices: []InventoryDevice{
					{ID: 1, PrimaryMAC: wrtMAC},
					{ID: 2, PrimaryMAC: c6MAC},
				},
				Clients: []InventoryClient{{MAC: client}},
				Bridges: []BridgeObservation{
					{
						DeviceID: 1, Bridge: "br-lan", Entries: []FDBEntry{{Port: 3, MAC: c6MAC}},
						STP:            &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "lan3", Port: 3}}},
						PortMedia:      map[int]string{3: "wired"},
						PortAttachment: map[int]string{3: PortAttachmentPhysical},
					},
					{
						DeviceID: 2, Bridge: "br-lan", Entries: []FDBEntry{{Port: 1, MAC: client}},
						STP:            &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "eth0.1", Port: 1}}},
						PortMedia:      map[int]string{1: "wired"},
						PortAttachment: map[int]string{1: PortAttachmentAggregate},
					},
				},
				Sources: []model.TopologySourceObservation{
					{DeviceID: 1, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: 4000},
					{DeviceID: 1, Source: SourceAssociations, State: model.TopologySourceObserved, ObservedAt: 4000},
					{DeviceID: 2, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: 4000},
				},
			}
			for _, iface := range tt.ifaces {
				input.Associations = append(input.Associations, Association{DeviceID: 1, Interface: iface, MAC: client})
			}
			result, err := Infer(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Edges) != len(tt.ifaces)+1 {
				t.Fatalf("aggregate survived or BSS collapsed: %#v", result.Edges)
			}
			associations := 0
			for _, edge := range result.Edges {
				if edge.ChildNode == "client:"+client {
					associations++
					if edge.ParentNode != "device:"+wrtMAC || edge.Medium != "wireless" {
						t.Fatalf("aggregate client edge survived: %#v", edge)
					}
					if len(tt.ifaces) > 1 && (edge.Confidence != "ambiguous" ||
						!strings.Contains(strings.Join(edge.Ambiguities, " "), "multiple candidate parents")) {
						t.Fatalf("multi-BSS ambiguity collapsed: %#v", edge)
					}
				}
			}
			if associations != len(tt.ifaces) {
				t.Fatalf("association edges=%d, want %d: %#v", associations, len(tt.ifaces), result.Edges)
			}
		})
	}
}

func TestInferRejectsReverseAggregateDeviceEdgeButKeepsDownstreamClient(t *testing.T) {
	client := "02:00:00:00:00:55"
	result, err := Infer(InferenceInput{
		At: 4000,
		Devices: []InventoryDevice{
			{ID: 1, PrimaryMAC: wrtMAC},
			{ID: 2, PrimaryMAC: c6MAC},
		},
		Clients: []InventoryClient{{MAC: client}},
		Bridges: []BridgeObservation{
			{
				DeviceID: 1, Bridge: "br-lan", Entries: []FDBEntry{{Port: 3, MAC: c6MAC}},
				STP:            &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "lan3", Port: 3}}},
				PortMedia:      map[int]string{3: "wired"},
				PortAttachment: map[int]string{3: PortAttachmentPhysical},
			},
			{
				DeviceID: 2, Bridge: "br-lan",
				Entries:        []FDBEntry{{Port: 1, MAC: wrtMAC}, {Port: 1, MAC: client}},
				STP:            &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "eth0.1", Port: 1}}},
				PortMedia:      map[int]string{1: "wired"},
				PortAttachment: map[int]string{1: PortAttachmentAggregate},
			},
		},
		Sources: []model.TopologySourceObservation{
			observedSource(1, SourceBridgeFDB), observedSource(2, SourceBridgeFDB),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 2 {
		t.Fatalf("reverse aggregate edge was not pruned independently: %#v", result.Edges)
	}
	want := map[string]string{
		"device:" + c6MAC:  "device:" + wrtMAC + "\x00lan3",
		"client:" + client: "device:" + c6MAC + "\x00eth0.1",
	}
	for _, edge := range result.Edges {
		if got := edge.ParentNode + "\x00" + edge.ParentPort; got != want[edge.ChildNode] {
			t.Fatalf("edge = %#v", edge)
		}
	}
}

func TestSourceAwareReconcileMarksCompetingParentsAcrossDevicePolls(t *testing.T) {
	device1, device2 := int64(1), int64(2)
	active := []model.TopologyEdge{{
		ID: 1, ChildNode: "client:02:00:00:00:00:55", ParentNode: "device:" + wrtMAC,
		ParentDeviceID: &device1, ParentPort: "lan1", Medium: "wired", Confidence: "ambiguous",
		ValidFrom: 100, LastSeen: 100,
		Evidence:    []model.TopologyEvidence{{Source: SourceBridgeFDB, DeviceID: &device1}},
		Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}}
	observed := []model.TopologyEdge{{
		ChildNode: "client:02:00:00:00:00:55", ParentNode: "device:" + c6MAC,
		ParentDeviceID: &device2, ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "measured",
		ValidFrom: 200, LastSeen: 200,
		Evidence:    []model.TopologyEvidence{{Source: SourceAssociations, DeviceID: &device2}},
		Ambiguities: []string{},
	}}
	changes, err := ReconcileIntervalsBySource(active, observed, 200, []model.TopologySourceObservation{{
		DeviceID: device2, Source: SourceAssociations, State: model.TopologySourceObserved, ObservedAt: 200,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upsert) != 2 || len(changes.Close) != 1 ||
		changes.Close[0].ID != active[0].ID || changes.Close[0].ValidTo == nil ||
		*changes.Close[0].ValidTo != 200 {
		t.Fatalf("changes=%+v", changes)
	}
	for _, edge := range changes.Upsert {
		if edge.ID != 0 || edge.ValidFrom != 200 || edge.Confidence != "ambiguous" ||
			!strings.Contains(strings.Join(edge.Ambiguities, " "), "multiple candidate parents") {
			t.Fatalf("fleet competition missing: %+v", edge)
		}
	}
}

func TestSourceAwareReconcileRestoresInferredConfidenceAfterFDBCompetitionEnds(t *testing.T) {
	device1, device2 := int64(1), int64(2)
	client := "client:02:00:00:00:00:55"
	fdb := func(id, deviceID int64, parent, port string) model.TopologyEdge {
		return model.TopologyEdge{
			ID: id, ChildNode: client, ParentNode: parent, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wired", Confidence: "ambiguous",
			ValidFrom: 100, LastSeen: 100,
			Evidence: []model.TopologyEvidence{{
				Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID,
				Detail: map[string]any{"attachment": PortAttachmentPhysical},
			}},
			Ambiguities: []string{competingParentsAmbiguity},
		}
	}
	remaining := fdb(1, device1, "device:"+wrtMAC, "lan1")
	disappeared := fdb(2, device2, "device:"+c6MAC, "eth1")

	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{remaining, disappeared}, nil, 200,
		[]model.TopologySourceObservation{{
			DeviceID: device2, Source: SourceBridgeFDB,
			State: model.TopologySourceEmpty, ObservedAt: 200,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	closed := map[int64]bool{}
	for _, edge := range changes.Close {
		closed[edge.ID] = true
	}
	if !closed[remaining.ID] || !closed[disappeared.ID] || len(changes.Upsert) != 1 {
		t.Fatalf("competition cleanup did not version both old states: %+v", changes)
	}
	got := changes.Upsert[0]
	if got.ID != 0 || got.Confidence != "inferred" || len(got.Ambiguities) != 0 {
		t.Fatalf("remaining FDB placement was falsely promoted: %+v", got)
	}
}

func TestCurrentPresentationEdgesShadowsOnlyFreshMatchingManagedLinkTransit(t *testing.T) {
	directOwner, transitOwner := int64(1), int64(2)
	client := "client:02:00:00:00:00:55"
	peer := "device:" + wrtMAC
	direct := model.TopologyEdge{
		ID: 1, ChildNode: client, ParentNode: peer, ParentDeviceID: &directOwner,
		ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "ambiguous",
		ValidFrom: 100, LastSeen: 190,
		Evidence: []model.TopologyEvidence{{
			Kind: "association", Source: SourceAssociations, DeviceID: &directOwner,
		}},
		Ambiguities: []string{competingParentsAmbiguity},
	}
	transit := model.TopologyEdge{
		ID: 2, ChildNode: client, ParentNode: "device:" + c6MAC,
		ParentDeviceID: &transitOwner, ParentPort: "eth0", Medium: "wired",
		Confidence: "ambiguous", ValidFrom: 100, LastSeen: 190,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &transitOwner,
			Detail: map[string]any{
				"attachment": PortAttachmentPhysical, stpStateDetail: "forwarding",
				managedLinkTransitDetail: true, managedLinkPeerDetail: peer,
			},
		}},
		Ambiguities: []string{managedLinkTransitAmbiguity, competingParentsAmbiguity},
	}
	associationState := func(state model.TopologySourceState, observedAt int64) []model.TopologySourceObservation {
		return []model.TopologySourceObservation{{
			DeviceID: directOwner, Source: SourceAssociations, State: state, ObservedAt: observedAt,
		}}
	}

	t.Run("fresh matching association hides only the presentation copy", func(t *testing.T) {
		got := CurrentPresentationEdges([]model.TopologyEdge{direct, transit},
			associationState(model.TopologySourceObserved, 200), 200, 50)
		if len(got) != 1 || got[0].ID != direct.ID || got[0].Confidence != "measured" ||
			len(got[0].Ambiguities) != 0 {
			t.Fatalf("visible edges = %+v", got)
		}
		if len(direct.Ambiguities) != 1 || len(transit.Ambiguities) != 2 {
			t.Fatal("presentation mutated stored evidence")
		}
	})

	for _, tt := range []struct {
		name       string
		state      model.TopologySourceState
		observedAt int64
	}{
		{name: "error", state: model.TopologySourceError, observedAt: 200},
		{name: "unknown", state: model.TopologySourceUnknown, observedAt: 200},
		{name: "stale", state: model.TopologySourceObserved, observedAt: 149},
	} {
		t.Run(tt.name+" direct source cannot hide transit", func(t *testing.T) {
			got := CurrentPresentationEdges([]model.TopologyEdge{direct, transit},
				associationState(tt.state, tt.observedAt), 200, 50)
			if len(got) != 2 || got[0].Confidence != "ambiguous" || got[1].Confidence != "ambiguous" {
				t.Fatalf("visible edges = %+v", got)
			}
		})
	}

	t.Run("fresh source survives a later fleet reconcile timestamp", func(t *testing.T) {
		clamped := direct
		clamped.LastSeen = 200
		got := CurrentPresentationEdges([]model.TopologyEdge{clamped, transit},
			associationState(model.TopologySourceObserved, 189), 200, 50)
		if len(got) != 1 || got[0].ID != direct.ID || got[0].Confidence != "measured" {
			t.Fatalf("visible edges = %+v", got)
		}
	})

	t.Run("unrelated direct parent cannot hide transit", func(t *testing.T) {
		unrelated := direct
		unrelated.ParentNode = "device:02:00:00:00:00:03"
		got := CurrentPresentationEdges([]model.TopologyEdge{unrelated, transit},
			associationState(model.TopologySourceObserved, 200), 200, 50)
		if len(got) != 2 {
			t.Fatalf("unrelated placement hid transit: %+v", got)
		}
	})

	t.Run("fresh matching physical FDB hides transit", func(t *testing.T) {
		wired := direct
		wired.ParentPort, wired.Medium = "eth1", "wired"
		wired.Evidence = []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &directOwner,
			Detail: map[string]any{"attachment": PortAttachmentPhysical, stpStateDetail: "forwarding"},
		}}
		states := []model.TopologySourceObservation{
			{DeviceID: directOwner, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: 200},
			{DeviceID: directOwner, Source: SourceBridgeSTP, State: model.TopologySourceObserved, ObservedAt: 200},
		}
		got := CurrentPresentationEdges([]model.TopologyEdge{wired, transit}, states, 200, 50)
		if len(got) != 1 || got[0].ID != wired.ID || got[0].Confidence != "inferred" {
			t.Fatalf("visible edges = %+v", got)
		}
		for _, state := range []string{"blocking", "disabled"} {
			wired.Evidence[0].Detail[stpStateDetail] = state
			if got := CurrentPresentationEdges([]model.TopologyEdge{wired, transit}, states, 200, 50); len(got) != 2 {
				t.Fatalf("%s FDB hid transit: %+v", state, got)
			}
		}
	})

	t.Run("fresh transit chain resolves multiple managed hops", func(t *testing.T) {
		upstreamOwner := int64(3)
		upstream := model.TopologyEdge{
			ID: 3, ChildNode: client, ParentNode: "device:02:00:00:00:00:03",
			ParentDeviceID: &upstreamOwner, ParentPort: "eth0", Medium: "wired",
			Confidence: "ambiguous", ValidFrom: 100, LastSeen: 190,
			Evidence: []model.TopologyEvidence{{
				Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &upstreamOwner,
				Detail: map[string]any{
					"attachment": PortAttachmentPhysical, stpStateDetail: "forwarding",
					managedLinkTransitDetail: true, managedLinkPeerDetail: transit.ParentNode,
				},
			}},
			Ambiguities: []string{managedLinkTransitAmbiguity, competingParentsAmbiguity},
		}
		states := associationState(model.TopologySourceObserved, 200)
		for _, source := range []string{SourceBridgeFDB, SourceBridgeSTP, SourceLLDP} {
			states = append(states, model.TopologySourceObservation{
				DeviceID: transitOwner, Source: source, State: model.TopologySourceObserved, ObservedAt: 200,
			})
		}
		got := CurrentPresentationEdges([]model.TopologyEdge{direct, transit, upstream}, states, 200, 50)
		if len(got) != 1 || got[0].ID != direct.ID || got[0].Confidence != "measured" {
			t.Fatalf("visible edges = %+v", got)
		}

		states[len(states)-1].State = model.TopologySourceError
		got = CurrentPresentationEdges([]model.TopologyEdge{direct, transit, upstream}, states, 200, 50)
		if len(got) != 2 || got[0].ID != direct.ID || got[1].ID != upstream.ID {
			t.Fatalf("stale intermediate transit hid upstream evidence: %+v", got)
		}
	})

	t.Run("transit resurfaces immediately after direct placement closes", func(t *testing.T) {
		if got := CurrentPresentationEdges([]model.TopologyEdge{transit},
			associationState(model.TopologySourceEmpty, 210), 210, 50); len(got) != 1 ||
			got[0].ID != transit.ID || got[0].Confidence != "ambiguous" ||
			len(got[0].Ambiguities) != 1 || got[0].Ambiguities[0] != managedLinkTransitAmbiguity {
			t.Fatalf("resurfaced transit = %+v", got)
		}
	})
}

func TestSourceAwareReconcileRemovesAggregateTransitCandidate(t *testing.T) {
	const vlanAmbiguity = "BusyBox brctl showmacs does not identify VLAN"
	client := "client:02:00:00:00:00:55"
	physical := func(id, deviceID int64, child, parent, port string, ambiguities ...string) model.TopologyEdge {
		return model.TopologyEdge{
			ID: id, ChildNode: child, ParentNode: parent, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wired", Confidence: "ambiguous",
			ValidFrom: 100, LastSeen: 100,
			Evidence: []model.TopologyEvidence{{
				Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID,
				Detail: map[string]any{"attachment": PortAttachmentPhysical},
			}},
			Ambiguities: ambiguities,
		}
	}
	direct := physical(1, 1, client, "device:"+wrtMAC, "lan1", vlanAmbiguity, competingParentsAmbiguity)
	aggregate := physical(2, 2, client, "device:"+c6MAC, "eth0.1", vlanAmbiguity, competingParentsAmbiguity)
	aggregate.Evidence[0].Detail["attachment"] = PortAttachmentAggregate
	c6 := physical(3, 1, "device:"+c6MAC, "device:"+wrtMAC, "lan3", vlanAmbiguity)

	observedDirect, observedC6 := direct, c6
	observedDirect.ID, observedDirect.ValidFrom = 0, 200
	observedDirect.Ambiguities = []string{vlanAmbiguity}
	observedC6.ID, observedC6.ValidFrom = 0, 200
	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{direct, aggregate, c6},
		[]model.TopologyEdge{observedDirect, observedC6}, 200,
		[]model.TopologySourceObservation{{
			DeviceID: 1, Source: SourceBridgeFDB,
			State: model.TopologySourceObserved, ObservedAt: 200,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	closed := map[int64]bool{}
	for _, edge := range changes.Close {
		closed[edge.ID] = true
	}
	if len(changes.Close) != 2 || !closed[direct.ID] || !closed[aggregate.ID] {
		t.Fatalf("stale competing intervals not closed: %+v", changes)
	}
	if len(changes.Upsert) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	for _, edge := range changes.Upsert {
		if edge.ParentNode == "device:"+c6MAC ||
			strings.Contains(strings.Join(edge.Ambiguities, " "), "multiple candidate parents") {
			t.Fatalf("aggregate competition survived: %+v", edge)
		}
	}
}

func TestSourceAwareReconcileRejectsAggregateTransitInEitherPollOrder(t *testing.T) {
	client := "02:00:00:00:00:55"
	inferDevice := func(t *testing.T, deviceID, at int64) InferenceResult {
		t.Helper()
		input := InferenceInput{
			At: at,
			Devices: []InventoryDevice{
				{ID: 1, PrimaryMAC: wrtMAC},
				{ID: 2, PrimaryMAC: c6MAC},
			},
			Clients: []InventoryClient{{MAC: client}},
			Sources: []model.TopologySourceObservation{{
				DeviceID: deviceID, Source: SourceBridgeFDB,
				State: model.TopologySourceObserved, ObservedAt: at,
			}},
		}
		if deviceID == 1 {
			input.Bridges = []BridgeObservation{{
				DeviceID: 1, Bridge: "br-lan",
				Entries: []FDBEntry{{Port: 1, MAC: client}, {Port: 3, MAC: c6MAC}},
				STP: &STPState{Bridge: "br-lan", Ports: []STPPort{
					{Name: "lan1", Port: 1}, {Name: "lan3", Port: 3},
				}},
				PortMedia:      map[int]string{1: "wired", 3: "wired"},
				PortAttachment: map[int]string{1: PortAttachmentPhysical, 3: PortAttachmentPhysical},
			}}
		} else {
			input.Bridges = []BridgeObservation{{
				DeviceID: 2, Bridge: "br-lan", Entries: []FDBEntry{{Port: 1, MAC: client}},
				STP:            &STPState{Bridge: "br-lan", Ports: []STPPort{{Name: "eth0.1", Port: 1}}},
				PortMedia:      map[int]string{1: "wired"},
				PortAttachment: map[int]string{1: PortAttachmentAggregate},
			}}
		}
		result, err := Infer(input)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	open := func(t *testing.T, result InferenceResult, at int64) []model.TopologyEdge {
		t.Helper()
		changes, err := ReconcileIntervalsBySource(nil, result.Edges, at, result.Sources)
		if err != nil {
			t.Fatal(err)
		}
		for i := range changes.Upsert {
			changes.Upsert[i].ID = int64(i + 1)
		}
		return changes.Upsert
	}

	t.Run("WRT then C6", func(t *testing.T) {
		active := open(t, inferDevice(t, 1, 100), 100)
		c6Poll := inferDevice(t, 2, 200)
		changes, err := ReconcileIntervalsBySource(active, c6Poll.Edges, 200, c6Poll.Sources)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Upsert) != 0 || len(changes.Close) != 0 {
			t.Fatalf("aggregate candidate was opened after exact physical placement: %+v", changes)
		}
	})

	t.Run("C6 then WRT", func(t *testing.T) {
		active := open(t, inferDevice(t, 2, 100), 100)
		if len(active) != 0 {
			t.Fatalf("unplaced aggregate parent was rendered: %+v", active)
		}
		wrtPoll := inferDevice(t, 1, 200)
		changes, err := ReconcileIntervalsBySource(active, wrtPoll.Edges, 200, wrtPoll.Sources)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Close) != 0 || len(changes.Upsert) != 2 {
			t.Fatalf("physical placement did not replace withheld aggregate evidence: %+v", changes)
		}
		for _, edge := range changes.Upsert {
			if edge.ParentNode == "device:"+c6MAC {
				t.Fatalf("aggregate candidate survived: %+v", changes)
			}
		}
	})
}

func TestSourceAwareReconcileKeepsAggregateChildrenOnlyAfterParentIsRooted(t *testing.T) {
	deviceID := int64(2)
	aggregate := model.TopologyEdge{
		ID: 1, ChildNode: "client:02:00:00:00:00:55", ParentNode: "device:" + c6MAC,
		ParentDeviceID: &deviceID, ParentPort: "eth0.1", Medium: "wired",
		Confidence: "ambiguous", ValidFrom: 100, LastSeen: 100,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID,
			Detail: map[string]any{"attachment": PortAttachmentAggregate},
		}},
	}
	changes, err := reconcileFleetCompetingParents([]model.TopologyEdge{aggregate}, IntervalChanges{}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Close) != 1 || changes.Close[0].ID != aggregate.ID {
		t.Fatalf("unrooted aggregate edge stayed active: %+v", changes)
	}

	wrtID := int64(1)
	placement := model.TopologyEdge{
		ID: 2, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC,
		ParentDeviceID: &wrtID, ParentPort: "lan3", Medium: "wired",
		Confidence: "ambiguous", ValidFrom: 100, LastSeen: 100,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &wrtID,
			Detail: map[string]any{"attachment": PortAttachmentPhysical},
		}},
	}
	changes, err = reconcileFleetCompetingParents(
		[]model.TopologyEdge{aggregate, placement}, IntervalChanges{}, 200,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Close) != 0 {
		t.Fatalf("rooted aggregate child was discarded: %+v", changes)
	}
}

func TestSourceAwareReconcileAssociationRejectsAggregateTransit(t *testing.T) {
	wrtID, c6ID := int64(1), int64(2)
	client := "client:02:00:00:00:00:55"
	association := model.TopologyEdge{
		ID: 1, ChildNode: client, ParentNode: "device:" + wrtMAC, ParentDeviceID: &wrtID,
		ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "measured",
		ValidFrom: 100, LastSeen: 100,
		Evidence: []model.TopologyEvidence{{
			Kind: "association", Source: SourceAssociations, DeviceID: &wrtID,
			Detail: map[string]any{"interface": "phy0-ap0"},
		}}, Ambiguities: []string{},
	}
	c6Placement := model.TopologyEdge{
		ID: 2, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC, ParentDeviceID: &wrtID,
		ParentPort: "lan3", Medium: "wired", Confidence: "ambiguous",
		ValidFrom: 100, LastSeen: 100,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &wrtID,
			Detail: map[string]any{"attachment": PortAttachmentPhysical},
		}}, Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}
	aggregate := model.TopologyEdge{
		ID: 3, ChildNode: client, ParentNode: "device:" + c6MAC, ParentDeviceID: &c6ID,
		ParentPort: "eth0.1", Medium: "wired", Confidence: "ambiguous",
		ValidFrom: 100, LastSeen: 100,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &c6ID,
			Detail: map[string]any{"attachment": PortAttachmentAggregate},
		}}, Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}
	fresh := func(edge model.TopologyEdge, at int64) model.TopologyEdge {
		edge.ID, edge.ValidFrom, edge.LastSeen = 0, at, at
		return edge
	}
	wrtSources := func(at int64) []model.TopologySourceObservation {
		return []model.TopologySourceObservation{
			{DeviceID: wrtID, Source: SourceAssociations, State: model.TopologySourceObserved, ObservedAt: at},
			{DeviceID: wrtID, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: at},
		}
	}
	c6Sources := func(at int64) []model.TopologySourceObservation {
		return []model.TopologySourceObservation{{
			DeviceID: c6ID, Source: SourceBridgeFDB, State: model.TopologySourceObserved, ObservedAt: at,
		}}
	}

	t.Run("association then aggregate", func(t *testing.T) {
		changes, err := ReconcileIntervalsBySource(
			[]model.TopologyEdge{association, c6Placement}, []model.TopologyEdge{fresh(aggregate, 200)},
			200, c6Sources(200),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Upsert) != 0 || len(changes.Close) != 0 {
			t.Fatalf("aggregate opened after association: %+v", changes)
		}
	})

	t.Run("aggregate then association", func(t *testing.T) {
		changes, err := ReconcileIntervalsBySource(
			[]model.TopologyEdge{aggregate},
			[]model.TopologyEdge{fresh(association, 200), fresh(c6Placement, 200)},
			200, wrtSources(200),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Close) != 1 || changes.Close[0].ID != aggregate.ID || len(changes.Upsert) != 2 {
			t.Fatalf("persisted aggregate not replaced: %+v", changes)
		}
	})

	t.Run("existing competing intervals", func(t *testing.T) {
		activeAssociation, activeAggregate := association, aggregate
		activeAssociation.Confidence = "ambiguous"
		activeAssociation.Ambiguities = []string{competingParentsAmbiguity}
		activeAggregate.Ambiguities = append(activeAggregate.Ambiguities, competingParentsAmbiguity)
		changes, err := ReconcileIntervalsBySource(
			[]model.TopologyEdge{activeAssociation, activeAggregate, c6Placement},
			[]model.TopologyEdge{fresh(association, 200), fresh(c6Placement, 200)},
			200, wrtSources(200),
		)
		if err != nil {
			t.Fatal(err)
		}
		closed := map[int64]bool{}
		for _, edge := range changes.Close {
			closed[edge.ID] = true
		}
		if len(changes.Close) != 2 || !closed[association.ID] || !closed[aggregate.ID] || len(changes.Upsert) != 2 {
			t.Fatalf("existing aggregate competition not cleaned up: %+v", changes)
		}
		for _, edge := range changes.Upsert {
			if edge.ParentNode == "device:"+c6MAC ||
				strings.Contains(strings.Join(edge.Ambiguities, " "), "multiple candidate parents") {
				t.Fatalf("aggregate competition survived: %+v", edge)
			}
		}
	})
}

func TestSourceAwareReconcileRejectsReverseAggregateDeviceEdgeInEitherPollOrder(t *testing.T) {
	physical := func(id int64, child, parent, port, attachment string, deviceID, at int64) model.TopologyEdge {
		return model.TopologyEdge{
			ID: id, ChildNode: child, ParentNode: parent, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wired", Confidence: "ambiguous",
			ValidFrom: at, LastSeen: at,
			Evidence: []model.TopologyEvidence{{
				Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID,
				Detail: map[string]any{"attachment": attachment},
			}},
			Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
		}
	}
	physicalEdge := physical(1, "device:"+c6MAC, "device:"+wrtMAC, "lan3", PortAttachmentPhysical, 1, 100)
	aggregateEdge := physical(2, "device:"+wrtMAC, "device:"+c6MAC, "eth0.1", PortAttachmentAggregate, 2, 100)
	source := func(deviceID, at int64) []model.TopologySourceObservation {
		return []model.TopologySourceObservation{{
			DeviceID: deviceID, Source: SourceBridgeFDB,
			State: model.TopologySourceObserved, ObservedAt: at,
		}}
	}

	t.Run("physical then aggregate", func(t *testing.T) {
		observed := aggregateEdge
		observed.ID, observed.ValidFrom, observed.LastSeen = 0, 200, 200
		changes, err := ReconcileIntervalsBySource(
			[]model.TopologyEdge{physicalEdge}, []model.TopologyEdge{observed}, 200, source(2, 200),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Upsert) != 0 || len(changes.Close) != 0 {
			t.Fatalf("reverse aggregate candidate was opened after physical edge: %+v", changes)
		}
	})

	t.Run("aggregate then physical", func(t *testing.T) {
		observed := physicalEdge
		observed.ID, observed.ValidFrom, observed.LastSeen = 0, 200, 200
		changes, err := ReconcileIntervalsBySource(
			[]model.TopologyEdge{aggregateEdge}, []model.TopologyEdge{observed}, 200, source(1, 200),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Close) != 1 || changes.Close[0].ID != aggregateEdge.ID ||
			len(changes.Upsert) != 1 || changes.Upsert[0].ParentNode != "device:"+wrtMAC {
			t.Fatalf("persisted reverse aggregate edge was not replaced: %+v", changes)
		}
	})
}

func TestSourceAwareReconcileSuppressesReciprocalManagedDeviceLLDPInEitherPollOrder(t *testing.T) {
	wrtID, c6ID := int64(1), int64(2)
	root := model.TopologyEdge{
		ChildNode: "device:" + wrtMAC, ParentNode: InternetNode, ParentPort: "wan",
		Medium: "uplink", Confidence: "measured", ValidFrom: 100, LastSeen: 100,
		Evidence: []model.TopologyEvidence{{
			Kind: "default_route", Source: SourceDefaultRoute, DeviceID: &wrtID,
		}},
	}
	link := func(child, parent, port string, deviceID int64) model.TopologyEdge {
		return model.TopologyEdge{
			ChildNode: child, ParentNode: parent, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wired", Confidence: "measured",
			ValidFrom: 100, LastSeen: 100,
			Evidence: []model.TopologyEvidence{{
				Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: &deviceID,
			}},
		}
	}
	correct := link("device:"+c6MAC, "device:"+wrtMAC, "lan3", wrtID)
	reverse := link("device:"+wrtMAC, "device:"+c6MAC, "eth0.1", c6ID)
	sources := func(deviceID, at int64, names ...string) []model.TopologySourceObservation {
		out := make([]model.TopologySourceObservation, 0, len(names))
		for _, name := range names {
			out = append(out, model.TopologySourceObservation{
				DeviceID: deviceID, Source: name, State: model.TopologySourceObserved, ObservedAt: at,
			})
		}
		return out
	}
	activate := func(edges []model.TopologyEdge) []model.TopologyEdge {
		for i := range edges {
			edges[i].ID = int64(i + 1)
		}
		return edges
	}
	fresh := func(edges ...model.TopologyEdge) []model.TopologyEdge {
		for i := range edges {
			edges[i].ID, edges[i].ValidFrom, edges[i].LastSeen = 0, 200, 200
		}
		return edges
	}

	t.Run("gateway poll then AP poll", func(t *testing.T) {
		first, err := ReconcileIntervalsBySource(nil, []model.TopologyEdge{root, correct}, 100,
			sources(wrtID, 100, SourceDefaultRoute, SourceLLDP))
		if err != nil {
			t.Fatal(err)
		}
		active := activate(first.Upsert)
		second, err := ReconcileIntervalsBySource(active, fresh(reverse), 200,
			sources(c6ID, 200, SourceLLDP))
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Upsert) != 0 || len(second.Close) != 0 {
			t.Fatalf("reverse LLDP survived gateway-first startup: %+v", second)
		}
	})

	t.Run("AP poll then gateway poll", func(t *testing.T) {
		first, err := ReconcileIntervalsBySource(nil, []model.TopologyEdge{reverse}, 100,
			sources(c6ID, 100, SourceLLDP))
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Upsert) != 0 || len(first.Close) != 0 {
			t.Fatalf("unrooted startup LLDP was exposed: %+v", first)
		}
		active := activate(first.Upsert)
		second, err := ReconcileIntervalsBySource(active, fresh(root, correct), 200,
			sources(wrtID, 200, SourceDefaultRoute, SourceLLDP))
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Close) != 0 || len(second.Upsert) != 2 {
			t.Fatalf("gateway poll did not replace startup reverse LLDP: %+v", second)
		}
		for _, edge := range second.Upsert {
			if edge.ChildNode == "device:"+wrtMAC && edge.ParentNode == "device:"+c6MAC {
				t.Fatalf("reverse LLDP was reopened: %+v", second)
			}
		}
	})
}

func TestReciprocalManagedDeviceSuppressionPreservesDownstreamEdges(t *testing.T) {
	wrtID, c6ID := int64(1), int64(2)
	root := model.TopologyEdge{ChildNode: "device:" + wrtMAC, ParentNode: InternetNode}
	correct := model.TopologyEdge{
		ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC,
		ParentDeviceID: &wrtID, ParentPort: "lan3", Medium: "wired",
		Evidence: []model.TopologyEvidence{{
			Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: &wrtID,
		}},
	}
	reverse := model.TopologyEdge{
		ChildNode: "device:" + wrtMAC, ParentNode: "device:" + c6MAC,
		ParentDeviceID: &c6ID, ParentPort: "eth0.1", Medium: "wired",
		Evidence: []model.TopologyEvidence{{
			Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: &c6ID,
		}},
	}
	client := model.TopologyEdge{
		ChildNode: "client:02:00:00:00:00:55", ParentNode: "device:" + c6MAC,
		ParentDeviceID: &c6ID, ParentPort: "phy0-ap0", Medium: "wireless",
	}
	got := reciprocalManagedDeviceEdgeKeys([]model.TopologyEdge{root, correct, reverse, client})
	if len(got) != 1 || !got[edgeSortKey(reverse)] || got[edgeSortKey(correct)] || got[edgeSortKey(client)] {
		t.Fatalf("rooted reciprocal suppression=%v", got)
	}
	got = reciprocalManagedDeviceEdgeKeys([]model.TopologyEdge{correct, reverse, client})
	if len(got) != 2 || !got[edgeSortKey(correct)] || !got[edgeSortKey(reverse)] || got[edgeSortKey(client)] {
		t.Fatalf("unrooted reciprocal suppression=%v", got)
	}
	unrooted := unrootedManagedDeviceLLDPEdgeKeys([]model.TopologyEdge{reverse, client})
	if len(unrooted) != 1 || !unrooted[edgeSortKey(reverse)] || unrooted[edgeSortKey(client)] {
		t.Fatalf("lone unrooted LLDP suppression=%v", unrooted)
	}
	unrooted = unrootedManagedDeviceLLDPEdgeKeys([]model.TopologyEdge{root, correct, client})
	if len(unrooted) != 0 {
		t.Fatalf("rooted downstream LLDP was suppressed=%v", unrooted)
	}

	switchID := int64(3)
	switchNode := "device:02:00:00:00:00:03"
	upstream := model.TopologyEdge{
		ChildNode: switchNode, ParentNode: "device:" + wrtMAC,
		ParentDeviceID: &wrtID, ParentPort: "lan2", Medium: "wired",
		Evidence: []model.TopologyEvidence{{
			Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: &wrtID,
		}},
	}
	downstream := correct
	downstream.ParentNode, downstream.ParentDeviceID, downstream.ParentPort = switchNode, &switchID, "lan3"
	downstream.Evidence = []model.TopologyEvidence{{
		Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: &switchID,
	}}
	reverseDownstream := reverse
	reverseDownstream.ChildNode, reverseDownstream.ParentNode = switchNode, "device:"+c6MAC
	for name, edges := range map[string][]model.TopologyEdge{
		"root-to-leaf order": {root, upstream, downstream, reverseDownstream, client},
		"leaf-to-root order": {client, reverseDownstream, downstream, upstream, root},
	} {
		t.Run(name, func(t *testing.T) {
			depth := managedDeviceRootDepths(edges)
			if depth["device:"+wrtMAC] != 0 || depth[switchNode] != 1 || depth["device:"+c6MAC] != 2 {
				t.Fatalf("transitive root depths=%v", depth)
			}
			got := reciprocalManagedDeviceEdgeKeys(edges)
			if len(got) != 1 || !got[edgeSortKey(reverseDownstream)] || got[edgeSortKey(downstream)] {
				t.Fatalf("transitive reciprocal suppression=%v", got)
			}
			active := append([]model.TopologyEdge(nil), edges...)
			var reverseID int64
			for i := range active {
				active[i].ID, active[i].ValidFrom, active[i].LastSeen = int64(i+1), 100, 100
				if edgeSortKey(active[i]) == edgeSortKey(reverseDownstream) {
					reverseID = active[i].ID
				}
			}
			changes, err := reconcileFleetCompetingParents(active, IntervalChanges{}, 200)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes.Upsert) != 0 || len(changes.Close) != 1 || changes.Close[0].ID != reverseID {
				t.Fatalf("transitive fleet reconciliation=%+v", changes)
			}
		})
	}
}

func TestSourceAwareReconcileVersionsCompetingParentSemantics(t *testing.T) {
	device1, device2 := int64(1), int64(2)
	child := "client:02:00:00:00:00:55"
	associationEdge := func(id, deviceID int64, parent, port string, at int64) model.TopologyEdge {
		return model.TopologyEdge{
			ID: id, ChildNode: child, ParentNode: parent, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wireless", Confidence: "measured",
			ValidFrom: at, LastSeen: at,
			Evidence: []model.TopologyEvidence{{
				Kind: "association", Source: SourceAssociations, DeviceID: &deviceID,
				Detail: map[string]any{"interface": port},
			}},
			Ambiguities: []string{},
		}
	}

	// t1: one measured attachment.
	first := associationEdge(1, device1, "device:"+wrtMAC, "phy0-ap0", 100)
	// t2: another device reports the same client. The original edge remains a
	// candidate, but its new ambiguity must start a new interval at t2.
	second := associationEdge(0, device2, "device:"+c6MAC, "phy1-ap0", 200)
	atT2, err := ReconcileIntervalsBySource([]model.TopologyEdge{first},
		[]model.TopologyEdge{second}, 200, []model.TopologySourceObservation{{
			DeviceID: device2, Source: SourceAssociations,
			State: model.TopologySourceObserved, ObservedAt: 200,
		}})
	if err != nil {
		t.Fatal(err)
	}
	if len(atT2.Close) != 1 || atT2.Close[0].ID != first.ID ||
		atT2.Close[0].ValidTo == nil || *atT2.Close[0].ValidTo != 200 ||
		len(atT2.Upsert) != 2 {
		t.Fatalf("t2 changes=%+v", atT2)
	}
	activeT2 := make([]model.TopologyEdge, len(atT2.Upsert))
	for i, edge := range atT2.Upsert {
		if edge.ID != 0 || edge.ValidFrom != 200 || edge.Confidence != "ambiguous" ||
			!strings.Contains(strings.Join(edge.Ambiguities, " "), "multiple candidate parents") {
			t.Fatalf("t2 semantic interval=%+v", edge)
		}
		edge.ID = int64(i + 2)
		activeT2[i] = edge
	}

	// t3: device 1 still sees the client and device 2 proves it absent. Both t2
	// ambiguous rows close; the restored measured state begins at t3.
	observedAgain := associationEdge(0, device1, "device:"+wrtMAC, "phy0-ap0", 300)
	atT3, err := ReconcileIntervalsBySource(activeT2, []model.TopologyEdge{observedAgain},
		300, []model.TopologySourceObservation{
			{DeviceID: device1, Source: SourceAssociations, State: model.TopologySourceObserved, ObservedAt: 300},
			{DeviceID: device2, Source: SourceAssociations, State: model.TopologySourceEmpty, ObservedAt: 300},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(atT3.Close) != 2 || len(atT3.Upsert) != 1 {
		t.Fatalf("t3 changes=%+v", atT3)
	}
	restored := atT3.Upsert[0]
	if restored.ID != 0 || restored.ValidFrom != 300 || restored.Confidence != "measured" ||
		len(restored.Ambiguities) != 0 {
		t.Fatalf("t3 semantic interval=%+v", restored)
	}
}

func TestInferCreatesInternetOnlyFromActiveDefaultRouteEvidence(t *testing.T) {
	result, err := Infer(InferenceInput{
		At:      5000,
		Devices: []InventoryDevice{{ID: 1, PrimaryMAC: wrtMAC}},
		Uplinks: []Uplink{
			{DeviceID: 1, Interface: "wan", Active: false},
			{DeviceID: 1, Interface: "pppoe-wan", LogicalInterface: "wan", Active: true},
		},
		Sources: []model.TopologySourceObservation{observedSource(1, SourceDefaultRoute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].ParentNode != InternetNode ||
		result.Edges[0].ChildNode != "device:"+wrtMAC || result.Edges[0].ParentDeviceID != nil ||
		result.Edges[0].ParentPort != "pppoe-wan" {
		t.Fatalf("uplink edge = %#v", result.Edges)
	}
	detail := result.Edges[0].Evidence[0].Detail
	if detail["interface"] != "pppoe-wan" || detail["logical_interface"] != "wan" {
		t.Fatalf("uplink evidence=%v", detail)
	}
}

func TestReconcileIntervalsPreservesHistoryAndDoesNotCloseOnUnknown(t *testing.T) {
	active := model.TopologyEdge{
		ID: 8, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC, ParentPort: "lan2",
		Medium: "wired", Confidence: "ambiguous", ValidFrom: 1000, LastSeen: 1900,
		Evidence: []model.TopologyEvidence{}, Ambiguities: []string{},
	}
	observed := active
	observed.ID = 0
	observed.ValidFrom = 2000

	changes, err := ReconcileIntervals([]model.TopologyEdge{active}, []model.TopologyEdge{observed}, 2000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Upsert) != 1 || changes.Upsert[0].ID != 8 ||
		changes.Upsert[0].ValidFrom != 1000 || changes.Upsert[0].LastSeen != 2000 || len(changes.Close) != 0 {
		t.Fatalf("continuing interval = %#v", changes)
	}

	changes, err = ReconcileIntervals([]model.TopologyEdge{active}, nil, 2100, false)
	if err != nil || len(changes.Close) != 0 {
		t.Fatalf("partial observation closed an edge: %#v, %v", changes, err)
	}
	changes, err = ReconcileIntervals([]model.TopologyEdge{active}, nil, 2200, true)
	if err != nil || len(changes.Close) != 1 || changes.Close[0].ValidTo == nil || *changes.Close[0].ValidTo != 2200 {
		t.Fatalf("complete disappearance did not close edge: %#v, %v", changes, err)
	}
}

func TestInferRejectsInvalidSourceStateInsteadOfTreatingItAsCoverage(t *testing.T) {
	_, err := Infer(InferenceInput{
		At:      6000,
		Devices: []InventoryDevice{{ID: 1, PrimaryMAC: wrtMAC}},
		Sources: []model.TopologySourceObservation{{
			DeviceID: 1, Source: "bridge-fdb", State: "maybe", ObservedAt: 6000,
		}},
	})
	if err == nil {
		t.Fatal("invalid source state was accepted")
	}
}

func TestReconcileIntervalsRejectsTimeBeforeLastObservation(t *testing.T) {
	active := model.TopologyEdge{
		ID: 9, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC,
		Medium: "wired", Confidence: "measured", ValidFrom: 1000, LastSeen: 2000,
	}
	if _, err := ReconcileIntervals([]model.TopologyEdge{active}, nil, 1500, true); err == nil {
		t.Fatal("reconciliation moved an interval backwards")
	}
}

func TestSourceAwareReconcileClosesFDBDespiteVLANGaps(t *testing.T) {
	deviceID := int64(1)
	active := model.TopologyEdge{
		ID: 10, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC,
		Medium: "wired", Confidence: "ambiguous", ValidFrom: 1000, LastSeen: 1900,
		Evidence: []model.TopologyEvidence{
			{Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID, Detail: map[string]any{}},
			{Kind: "neighbor", Source: SourceNeighbors(4), DeviceID: &deviceID, Detail: map[string]any{}},
		},
		Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}
	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{active}, nil, 2000,
		[]model.TopologySourceObservation{{
			DeviceID: deviceID, Source: SourceBridgeFDB, State: model.TopologySourceEmpty, ObservedAt: 2000,
		}},
	)
	if err != nil || len(changes.Close) != 1 {
		t.Fatalf("answered-empty FDB did not close edge: %#v err=%v", changes, err)
	}
}

func TestSourceAwareReconcileWaitsForEveryGenerator(t *testing.T) {
	deviceID := int64(1)
	active := model.TopologyEdge{
		ID: 11, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC,
		Medium: "wired", Confidence: "measured", ValidFrom: 1000, LastSeen: 1900,
		Evidence: []model.TopologyEvidence{
			{Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID, Detail: map[string]any{}},
			{Kind: "lldp_neighbor", Source: SourceLLDP, DeviceID: &deviceID, Detail: map[string]any{}},
		},
	}
	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{active}, nil, 2000,
		[]model.TopologySourceObservation{
			{DeviceID: deviceID, Source: SourceBridgeFDB, State: model.TopologySourceEmpty, ObservedAt: 2000},
			{DeviceID: deviceID, Source: SourceLLDP, State: model.TopologySourceError, ObservedAt: 2000},
		},
	)
	if err != nil || len(changes.Close) != 0 {
		t.Fatalf("unavailable LLDP manufactured a link-down: %#v err=%v", changes, err)
	}
}

func TestSourceAwareReconcileDoesNotUseStaleCoverage(t *testing.T) {
	deviceID := int64(1)
	active := model.TopologyEdge{
		ID: 12, ChildNode: "device:" + c6MAC, ParentNode: "device:" + wrtMAC,
		Medium: "wired", Confidence: "measured", ValidFrom: 1000, LastSeen: 1900,
		Evidence: []model.TopologyEvidence{{
			Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID, Detail: map[string]any{},
		}},
	}
	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{active}, nil, 2000,
		[]model.TopologySourceObservation{{
			DeviceID: deviceID, Source: SourceBridgeFDB, State: model.TopologySourceEmpty, ObservedAt: 1800,
		}},
	)
	if err != nil || len(changes.Close) != 0 {
		t.Fatalf("stale source state manufactured a link-down: %#v err=%v", changes, err)
	}
}

func TestSourceAwareReconcileWaitsForCompleteSemanticCoverage(t *testing.T) {
	deviceID := int64(1)
	association := model.TopologyEvidence{
		Kind: "association", Source: SourceAssociations, DeviceID: &deviceID,
		Detail: map[string]any{"interface": "phy0-ap0"},
	}
	active := model.TopologyEdge{
		ID: 13, ChildNode: "client:02:00:00:00:00:55", ChildMAC: "02:00:00:00:00:55",
		ParentNode: "device:" + wrtMAC, ParentDeviceID: &deviceID,
		ParentPort: "phy0-ap0", Medium: "wireless", Confidence: "measured",
		ValidFrom: 1000, LastSeen: 1900,
		Evidence: []model.TopologyEvidence{
			association,
			{Kind: "bridge_fdb", Source: SourceBridgeFDB, DeviceID: &deviceID, Detail: map[string]any{}},
			{Kind: "neighbor", Source: SourceNeighbors(4), DeviceID: &deviceID, Detail: map[string]any{}},
			{Kind: "neighbor", Source: SourceNeighbors(6), DeviceID: &deviceID, Detail: map[string]any{}},
		},
		Ambiguities: []string{"BusyBox brctl showmacs does not identify VLAN"},
	}
	observed := active
	observed.ID, observed.ValidFrom, observed.LastSeen = 0, 2000, 2000
	observed.Evidence = []model.TopologyEvidence{association}
	observed.Ambiguities = []string{}

	source := func(name string, state model.TopologySourceState, observedAt int64) model.TopologySourceObservation {
		return model.TopologySourceObservation{
			DeviceID: deviceID, Source: name, State: state, ObservedAt: observedAt,
		}
	}
	for name, sources := range map[string][]model.TopologySourceObservation{
		"missing enrichment cycle": {
			source(SourceAssociations, model.TopologySourceObserved, 2000),
		},
		"failed enrichment source": {
			source(SourceAssociations, model.TopologySourceObserved, 2000),
			source(SourceBridgeFDB, model.TopologySourceObserved, 2000),
			source(SourceNeighbors(4), model.TopologySourceEmpty, 2000),
			source(SourceNeighbors(6), model.TopologySourceError, 2000),
		},
		"stale enrichment source": {
			source(SourceAssociations, model.TopologySourceObserved, 2000),
			source(SourceBridgeFDB, model.TopologySourceObserved, 2000),
			source(SourceNeighbors(4), model.TopologySourceEmpty, 2000),
			source(SourceNeighbors(6), model.TopologySourceEmpty, 1800),
		},
	} {
		t.Run(name, func(t *testing.T) {
			changes, err := ReconcileIntervalsBySource(
				[]model.TopologyEdge{active}, []model.TopologyEdge{observed}, 2000, sources)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes.Close) != 0 || len(changes.Upsert) != 1 ||
				changes.Upsert[0].ID != active.ID || changes.Upsert[0].ValidFrom != active.ValidFrom ||
				changes.Upsert[0].LastSeen != 2000 {
				t.Fatalf("partial coverage versioned interval: %+v", changes)
			}
			same, err := sameTopologySemantics(active, changes.Upsert[0])
			if err != nil || !same {
				t.Fatalf("partial coverage replaced prior semantics: %+v err=%v", changes.Upsert[0], err)
			}
		})
	}

	complete := []model.TopologySourceObservation{
		source(SourceAssociations, model.TopologySourceObserved, 2000),
		source(SourceBridgeFDB, model.TopologySourceEmpty, 2000),
		source(SourceNeighbors(4), model.TopologySourceEmpty, 2000),
		source(SourceNeighbors(6), model.TopologySourceEmpty, 2000),
	}
	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{active}, []model.TopologyEdge{observed}, 2000, complete)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Close) != 1 || changes.Close[0].ID != active.ID ||
		len(changes.Upsert) != 1 || changes.Upsert[0].ID != 0 || changes.Upsert[0].ValidFrom != 2000 {
		t.Fatalf("complete coverage did not version changed semantics: %+v", changes)
	}
	same, err := sameTopologySemantics(observed, changes.Upsert[0])
	if err != nil || !same {
		t.Fatalf("complete coverage did not accept current semantics: %+v err=%v", changes.Upsert[0], err)
	}

	t.Run("alternating partial and rich cycles", func(t *testing.T) {
		current := active
		for cycle := 1; cycle <= 15; cycle++ {
			at := int64(2000 + cycle)
			next := observed
			next.ValidFrom, next.LastSeen = at, at
			sources := []model.TopologySourceObservation{
				source(SourceAssociations, model.TopologySourceObserved, at),
			}
			if cycle%2 == 0 {
				next = withTopologySemantics(next, active)
				sources = []model.TopologySourceObservation{
					source(SourceAssociations, model.TopologySourceObserved, at),
					source(SourceBridgeFDB, model.TopologySourceObserved, at),
					source(SourceNeighbors(4), model.TopologySourceObserved, at),
					source(SourceNeighbors(6), model.TopologySourceObserved, at),
				}
			}
			changes, err := ReconcileIntervalsBySource(
				[]model.TopologyEdge{current}, []model.TopologyEdge{next}, at, sources)
			if err != nil {
				t.Fatalf("cycle %d: %v", cycle, err)
			}
			if len(changes.Close) != 0 || len(changes.Upsert) != 1 ||
				changes.Upsert[0].ID != active.ID || changes.Upsert[0].ValidFrom != active.ValidFrom ||
				changes.Upsert[0].LastSeen != at {
				t.Fatalf("cycle %d churned interval: %+v", cycle, changes)
			}
			current = changes.Upsert[0]
		}
	})
}

func TestSourceAwareReconcileAllowsAnsweredGeometryChange(t *testing.T) {
	deviceID := int64(1)
	association := func(id int64, port string, at int64) model.TopologyEdge {
		return model.TopologyEdge{
			ID: id, ChildNode: "client:02:00:00:00:00:55", ChildMAC: "02:00:00:00:00:55",
			ParentNode: "device:" + wrtMAC, ParentDeviceID: &deviceID,
			ParentPort: port, Medium: "wireless", Confidence: "measured",
			ValidFrom: at, LastSeen: at,
			Evidence: []model.TopologyEvidence{{
				Kind: "association", Source: SourceAssociations, DeviceID: &deviceID,
				Detail: map[string]any{"interface": port},
			}}, Ambiguities: []string{},
		}
	}
	active := association(14, "phy0-ap0", 1900)
	observed := association(0, "phy1-ap0", 2000)
	changes, err := ReconcileIntervalsBySource(
		[]model.TopologyEdge{active}, []model.TopologyEdge{observed}, 2000,
		[]model.TopologySourceObservation{{
			DeviceID: deviceID, Source: SourceAssociations,
			State: model.TopologySourceObserved, ObservedAt: 2000,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Close) != 1 || changes.Close[0].ID != active.ID ||
		len(changes.Upsert) != 1 || changes.Upsert[0].ParentPort != "phy1-ap0" {
		t.Fatalf("answered geometry change was not applied: %+v", changes)
	}
}
