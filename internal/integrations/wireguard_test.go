package integrations

import (
	"encoding/base64"
	"strings"
	"testing"
)

var peerKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))

func TestWireGuardObservedZeroAndNeverAreNotMissing(t *testing.T) {
	raw := WireGuardRaw{ProtocolVersion: 1, State: "observed", Interfaces: "wg0", Handshakes: "wg0\t" + peerKey + "\t0\n", Transfers: "wg0\t" + peerKey + "\t0\t5\n"}
	result := ParseWireGuard(7, raw)
	if result.State != "observed" || result.DeviceID != 7 || len(result.Interfaces) != 1 || len(result.Interfaces[0].Peers) != 1 {
		t.Fatal(result)
	}
	peer := result.Interfaces[0].Peers[0]
	if peer.HandshakeState != "never" || peer.LastHandshake != nil || peer.RXBytes == nil || *peer.RXBytes != 0 || peer.TXBytes == nil || *peer.TXBytes != 5 {
		t.Fatalf("%+v", peer)
	}
	raw.Handshakes = "wg0\t" + peerKey + "\t1700000000"
	raw.Transfers = ""
	result = ParseWireGuard(7, raw)
	peer = result.Interfaces[0].Peers[0]
	if result.State != "partial" || peer.RXBytes != nil || peer.LastHandshake == nil || *peer.LastHandshake != 1700000000000 {
		t.Fatalf("partial observation: %+v", result)
	}
}

func TestWireGuardEmptyObservationDiffersFromUnavailable(t *testing.T) {
	result := ParseWireGuard(7, WireGuardRaw{ProtocolVersion: 1, State: "observed"})
	if result.State != "observed" || result.Interfaces == nil || len(result.Interfaces) != 0 {
		t.Fatal(result)
	}
	result = ParseWireGuard(7, WireGuardRaw{ProtocolVersion: 1, State: "unavailable"})
	if result.State != "unavailable" {
		t.Fatal(result)
	}
}

func TestWireGuardRejectsInvalidOrOversizedEvidence(t *testing.T) {
	for _, raw := range []WireGuardRaw{
		{ProtocolVersion: 2, State: "observed"},
		{ProtocolVersion: 1, State: "observed", Interfaces: "wg0 wg0"},
		{ProtocolVersion: 1, State: "observed", Interfaces: `wg"0`},
		{ProtocolVersion: 1, State: "observed", Interfaces: "wg0", Handshakes: "wg1\t" + peerKey + "\t0"},
		{ProtocolVersion: 1, State: "observed", Interfaces: "wg0", Handshakes: "wg0\tPRIVATE-KEY-SENTINEL\t0"},
		{ProtocolVersion: 1, State: "observed", Interfaces: "wg0", Handshakes: "wg0\t" + peerKey + "\t-1"},
		{ProtocolVersion: 1, State: "observed", Interfaces: "wg0", Transfers: "wg0\t" + peerKey + "\t9007199254740992\t0"},
		{ProtocolVersion: 1, State: "observed", Interfaces: strings.Repeat("x", 2049)},
		{ProtocolVersion: 1, State: "observed", Handshakes: strings.Repeat("x", 131073)},
	} {
		result := ParseWireGuard(7, raw)
		if result.State != "unavailable" || len(result.Interfaces) != 0 {
			t.Fatalf("invalid evidence accepted: %+v", result)
		}
		for _, note := range result.Notes {
			if strings.Contains(note, "PRIVATE-KEY-SENTINEL") {
				t.Fatal("raw evidence leaked")
			}
		}
	}
}
