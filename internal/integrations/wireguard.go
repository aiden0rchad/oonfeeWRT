package integrations

import (
	"encoding/base64"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WireGuardRaw contains only explicitly selected public wg fields. Neither
// private keys nor preshared keys may be collected and redacted afterwards.
type WireGuardRaw struct {
	ProtocolVersion int    `json:"protocol_version"`
	State           string `json:"state"`
	Interfaces      string `json:"interfaces"`
	Handshakes      string `json:"handshakes"`
	Transfers       string `json:"transfers"`
}

type WireGuardPeer struct {
	PublicKey      string `json:"public_key"`
	LastHandshake  *int64 `json:"last_handshake"`
	HandshakeState string `json:"handshake_state"`
	RXBytes        *int64 `json:"rx_bytes"`
	TXBytes        *int64 `json:"tx_bytes"`
}

type WireGuardInterface struct {
	Name  string          `json:"name"`
	Peers []WireGuardPeer `json:"peers"`
}

type WireGuardResult struct {
	DeviceID   int64                `json:"device_id"`
	State      string               `json:"state"`
	CheckedAt  int64                `json:"checked_at"`
	Interfaces []WireGuardInterface `json:"interfaces"`
	Notes      []string             `json:"notes"`
}

func WireGuardUnavailable(deviceID int64, note string) WireGuardResult {
	return WireGuardResult{DeviceID: deviceID, State: "unavailable", CheckedAt: time.Now().UnixMilli(), Interfaces: []WireGuardInterface{}, Notes: []string{note}}
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

func ParseWireGuard(deviceID int64, raw WireGuardRaw) WireGuardResult {
	result := WireGuardUnavailable(deviceID, "The optional read-only helper did not return usable WireGuard evidence. Verify the helper version, its separate read ACL, and wireguard-tools on the router.")
	if raw.ProtocolVersion != 1 || raw.State != "observed" || len(raw.Interfaces) > 2048 || len(raw.Handshakes) > 131072 || len(raw.Transfers) > 131072 {
		return result
	}
	names := strings.Fields(raw.Interfaces)
	if len(names) > 64 {
		return result
	}
	peers := make(map[string]map[string]*WireGuardPeer, len(names))
	for _, name := range names {
		if !interfaceName.MatchString(name) || peers[name] != nil {
			return result
		}
		peers[name] = map[string]*WireGuardPeer{}
	}
	count := 0
	getPeer := func(name, key string) (*WireGuardPeer, error) {
		if peers[name] == nil {
			return nil, errors.New("interface changed during observation")
		}
		public, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(public) != 32 || base64.StdEncoding.EncodeToString(public) != key {
			return nil, errors.New("invalid public key")
		}
		if peer := peers[name][key]; peer != nil {
			return peer, nil
		}
		count++
		if count > 1024 {
			return nil, errors.New("too many peers")
		}
		peer := &WireGuardPeer{PublicKey: key, HandshakeState: "unavailable"}
		peers[name][key] = peer
		return peer, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(raw.Handshakes), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 3 {
			return result
		}
		peer, err := getPeer(parts[0], parts[1])
		if err != nil || peer.HandshakeState != "unavailable" {
			return result
		}
		seconds, err := parseWGInteger(parts[2], (1<<53-1)/1000)
		if err != nil {
			return result
		}
		peer.HandshakeState = "never"
		if seconds != 0 {
			milliseconds := seconds * 1000
			peer.LastHandshake = &milliseconds
			peer.HandshakeState = "observed"
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(raw.Transfers), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 4 {
			return result
		}
		peer, err := getPeer(parts[0], parts[1])
		if err != nil || peer.RXBytes != nil {
			return result
		}
		rx, err := parseWGInteger(parts[2], 1<<53-1)
		if err != nil {
			return result
		}
		tx, err := parseWGInteger(parts[3], 1<<53-1)
		if err != nil {
			return result
		}
		peer.RXBytes, peer.TXBytes = &rx, &tx
	}
	result.State = "observed"
	result.Notes = []string{
		"Read-only WireGuard runtime counters and handshake timestamps, not an active tunnel-connectivity test. Idle peers may have old handshakes without a fault.",
		"Counters are cumulative since the interface or peer was reset. These separate command reads are not an atomic snapshot; unavailable fields remain unknown.",
	}
	sort.Strings(names)
	for _, name := range names {
		entry := WireGuardInterface{Name: name, Peers: []WireGuardPeer{}}
		for _, peer := range peers[name] {
			if peer.HandshakeState == "unavailable" || peer.RXBytes == nil || peer.TXBytes == nil {
				result.State = "partial"
			}
			entry.Peers = append(entry.Peers, *peer)
		}
		sort.Slice(entry.Peers, func(i, j int) bool { return entry.Peers[i].PublicKey < entry.Peers[j].PublicKey })
		result.Interfaces = append(result.Interfaces, entry)
	}
	return result
}

func parseWGInteger(value string, max int64) (int64, error) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, errors.New("invalid integer")
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 || number > max {
		return 0, errors.New("integer out of range")
	}
	return number, nil
}
