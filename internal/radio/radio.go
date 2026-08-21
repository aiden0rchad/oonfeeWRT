// Package radio decodes the narrow, non-secret wireless state needed by the
// controller's radio inventory and explicit scan surfaces.
package radio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

const (
	maxRadios      = 32
	maxInterfaces  = 128
	maxFrequencies = 512
	maxScanRows    = 4096
	maxFlags       = 32
)

// InventoryRadio is keyed by the UCI wifi-device section. Runtime interfaces
// and PHY names are observations, never identities.
type InventoryRadio struct {
	Key               string      `json:"radio_key"`
	Up                *bool       `json:"up,omitempty"`
	Disabled          *bool       `json:"disabled,omitempty"`
	Pending           *bool       `json:"pending,omitempty"`
	Band              string      `json:"band,omitempty"`
	ConfiguredChannel string      `json:"configured_channel,omitempty"`
	HTMode            string      `json:"htmode,omitempty"`
	CurrentMHz        *int        `json:"current_mhz,omitempty"`
	CurrentChannel    *int        `json:"current_channel,omitempty"`
	CurrentAmbiguous  bool        `json:"current_ambiguous,omitempty"`
	Interfaces        []Interface `json:"interfaces"`
}

type Interface struct {
	Name string `json:"name"`
	Mode string `json:"mode,omitempty"`
}

// Frequency is one iwinfo.freqlist row. Restricted and Active are pointers so
// an older driver omitting either field does not become a measured false.
// DFS is deliberately absent: `restricted` does not establish radar state.
type Frequency struct {
	Band       string   `json:"band,omitempty"`
	Channel    int      `json:"channel"`
	MHz        int      `json:"mhz"`
	Restricted *bool    `json:"restricted,omitempty"`
	Active     *bool    `json:"active,omitempty"`
	Flags      []string `json:"flags"`
}

// ScanBSS is one bounded, validated iwinfo.scan observation.
type ScanBSS struct {
	BSSID   string `json:"bssid"`
	SSID    string `json:"ssid"`
	MHz     int    `json:"mhz"`
	Channel int    `json:"channel"`
	Signal  *int   `json:"signal,omitempty"`
	Width   *int   `json:"width,omitempty"`
}

// LiveState joins the slowly changing inventory with its independently cached
// channel list. FrequenciesKnown separates a proved empty list from a call no
// poll has successfully completed.
type LiveState struct {
	InventoryRadio
	InventoryObservedAt   int64       `json:"inventory_observed_at"`
	Frequencies           []Frequency `json:"frequencies"`
	FrequenciesKnown      bool        `json:"frequencies_known"`
	FrequenciesObservedAt int64       `json:"frequencies_observed_at,omitempty"`
}

// CollectionStatus separates response generation time from source freshness.
// A stale cache remains useful as last-known state, but is never current truth.
type CollectionStatus struct {
	ObservedAt          int64 `json:"observed_at,omitempty"`
	LastPollAt          int64 `json:"last_poll_at,omitempty"`
	LastPollOK          bool  `json:"last_poll_ok"`
	ConsecutiveFailures int   `json:"consecutive_failures"`
	// LastSourceAttempt* is the latest optional getWirelessDevices attempt.
	// It is separate from LastPollOK because system.info can succeed while the
	// radio inventory is denied or unreadable.
	LastSourceAttemptAt int64 `json:"last_source_attempt_at,omitempty"`
	LastSourceAttemptOK bool  `json:"last_source_attempt_ok"`
	Stale               bool  `json:"stale"`
}

// DecodeWirelessDevices selects only non-secret fields. In particular, the
// response's interfaces[].config.key is never represented and therefore can
// neither be retained nor logged by a caller.
func DecodeWirelessDevices(raw []byte) ([]InventoryRadio, error) {
	var payload map[string]struct {
		Up       *bool `json:"up"`
		Disabled *bool `json:"disabled"`
		Pending  *bool `json:"pending"`
		Config   struct {
			Band    string          `json:"band"`
			Channel json.RawMessage `json:"channel"`
			HTMode  string          `json:"htmode"`
		} `json:"config"`
		Interfaces []struct {
			IfName string `json:"ifname"`
			Config struct {
				Mode string `json:"mode"`
			} `json:"config"`
			IWInfo struct {
				Frequency int `json:"frequency"`
				Channel   int `json:"channel"`
			} `json:"iwinfo"`
		} `json:"interfaces"`
	}
	if err := decodeObject(raw, &payload); err != nil {
		return nil, fmt.Errorf("radio: getWirelessDevices: %w", err)
	}
	if len(payload) > maxRadios {
		return nil, fmt.Errorf("radio: getWirelessDevices has %d radios; limit is %d", len(payload), maxRadios)
	}
	out := make([]InventoryRadio, 0, len(payload))
	for key, value := range payload {
		if !validUCIName(key) {
			return nil, fmt.Errorf("radio: invalid UCI radio key %q", key)
		}
		if len(value.Interfaces) > maxInterfaces {
			return nil, fmt.Errorf("radio: %s has too many interfaces", key)
		}
		channel, err := scalarString(value.Config.Channel)
		if err != nil {
			return nil, fmt.Errorf("radio: %s channel: %w", key, err)
		}
		row := InventoryRadio{
			Key: key, Up: value.Up, Disabled: value.Disabled, Pending: value.Pending,
			Band:              strings.ToLower(strings.TrimSpace(value.Config.Band)),
			ConfiguredChannel: channel, HTMode: strings.TrimSpace(value.Config.HTMode),
			Interfaces: []Interface{},
		}
		var mhz, currentChannel int
		for _, iface := range value.Interfaces {
			if iface.IfName == "" {
				continue
			}
			if !validIface(iface.IfName) {
				return nil, fmt.Errorf("radio: %s has invalid interface %q", key, iface.IfName)
			}
			row.Interfaces = append(row.Interfaces, Interface{
				Name: iface.IfName, Mode: strings.ToLower(strings.TrimSpace(iface.Config.Mode)),
			})
			if iface.IWInfo.Frequency > 0 {
				if mhz != 0 && mhz != iface.IWInfo.Frequency {
					row.CurrentAmbiguous = true
				}
				mhz = iface.IWInfo.Frequency
			}
			if iface.IWInfo.Channel > 0 {
				if currentChannel != 0 && currentChannel != iface.IWInfo.Channel {
					row.CurrentAmbiguous = true
				}
				currentChannel = iface.IWInfo.Channel
			}
		}
		if !row.CurrentAmbiguous {
			if mhz > 0 {
				row.CurrentMHz = ptr(mhz)
			}
			if currentChannel > 0 {
				row.CurrentChannel = ptr(currentChannel)
			}
		}
		sort.Slice(row.Interfaces, func(i, j int) bool { return row.Interfaces[i].Name < row.Interfaces[j].Name })
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func DecodeFrequencyList(raw []byte) ([]Frequency, error) {
	var payload struct {
		Results []struct {
			Band       json.RawMessage `json:"band"`
			Channel    int             `json:"channel"`
			MHz        int             `json:"mhz"`
			Restricted *bool           `json:"restricted"`
			Active     *bool           `json:"active"`
			Flags      []string        `json:"flags"`
		} `json:"results"`
	}
	if err := decodeObject(raw, &payload); err != nil {
		return nil, fmt.Errorf("radio: freqlist: %w", err)
	}
	if len(payload.Results) > maxFrequencies {
		return nil, fmt.Errorf("radio: freqlist has %d rows; limit is %d", len(payload.Results), maxFrequencies)
	}
	seen := map[[2]int]bool{}
	out := make([]Frequency, 0, len(payload.Results))
	for i, value := range payload.Results {
		if value.Channel <= 0 || value.MHz < 2000 || value.MHz > 8000 {
			return nil, fmt.Errorf("radio: freqlist row %d has invalid channel/frequency", i)
		}
		if err := validateBandScalar(value.Band); err != nil {
			return nil, fmt.Errorf("radio: freqlist row %d band: %w", i, err)
		}
		if len(value.Flags) > maxFlags {
			return nil, fmt.Errorf("radio: freqlist row %d has too many flags", i)
		}
		flags := make([]string, 0, len(value.Flags))
		for _, flag := range value.Flags {
			flag = strings.TrimSpace(flag)
			if flag == "" || len(flag) > 64 {
				return nil, fmt.Errorf("radio: freqlist row %d has invalid flag", i)
			}
			flags = append(flags, flag)
		}
		id := [2]int{value.Channel, value.MHz}
		if seen[id] {
			continue
		}
		seen[id] = true
		sort.Strings(flags)
		// rpcd-mod-iwinfo versions disagree on whether band is a string or a
		// numeric enum. The enum is not a stable public contract, so derive the
		// band from the measured frequency instead of assigning it a meaning.
		band := BandForMHz(value.MHz)
		out = append(out, Frequency{Band: band, Channel: value.Channel, MHz: value.MHz,
			Restricted: value.Restricted, Active: value.Active, Flags: flags})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MHz == out[j].MHz {
			return out[i].Channel < out[j].Channel
		}
		return out[i].MHz < out[j].MHz
	})
	return out, nil
}

func DecodeScan(raw []byte) ([]ScanBSS, error) {
	var payload struct {
		Results []struct {
			BSSID   string `json:"bssid"`
			SSID    string `json:"ssid"`
			MHz     int    `json:"mhz"`
			Channel int    `json:"channel"`
			Signal  *int   `json:"signal"`
			Width   *int   `json:"width"`
		} `json:"results"`
	}
	if err := decodeObject(raw, &payload); err != nil {
		return nil, fmt.Errorf("radio: scan: %w", err)
	}
	if len(payload.Results) > maxScanRows {
		return nil, fmt.Errorf("radio: scan has %d rows; limit is %d", len(payload.Results), maxScanRows)
	}
	byID := map[string]ScanBSS{}
	for i, value := range payload.Results {
		mac, err := net.ParseMAC(value.BSSID)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("radio: scan row %d has invalid BSSID", i)
		}
		if value.Channel <= 0 || value.MHz < 2000 || value.MHz > 8000 {
			return nil, fmt.Errorf("radio: scan row %d has invalid channel/frequency", i)
		}
		if len(value.SSID) > 128 {
			return nil, fmt.Errorf("radio: scan row %d has an oversized SSID", i)
		}
		if value.Width != nil && (*value.Width <= 0 || *value.Width > 320) {
			return nil, fmt.Errorf("radio: scan row %d has invalid width", i)
		}
		row := ScanBSS{BSSID: strings.ToLower(mac.String()), SSID: value.SSID,
			MHz: value.MHz, Channel: value.Channel, Signal: value.Signal, Width: value.Width}
		id := row.BSSID + "/" + strconv.Itoa(row.MHz)
		old, exists := byID[id]
		if !exists || stronger(row.Signal, old.Signal) {
			byID[id] = row
		}
	}
	out := make([]ScanBSS, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Signal != nil && out[j].Signal != nil && *out[i].Signal != *out[j].Signal {
			return *out[i].Signal > *out[j].Signal
		}
		if out[i].MHz != out[j].MHz {
			return out[i].MHz < out[j].MHz
		}
		return out[i].BSSID < out[j].BSSID
	})
	return out, nil
}

func BandForMHz(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return "2g"
	case mhz >= 4900 && mhz < 5925:
		return "5g"
	case mhz >= 5925 && mhz <= 7125:
		return "6g"
	default:
		return ""
	}
}

func decodeObject(raw []byte, value any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("expected JSON object")
	}
	if len(raw) > 4<<20 {
		return errors.New("response exceeds 4 MiB")
	}
	return json.Unmarshal(raw, value)
}

func scalarString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text), nil
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err == nil {
		return number.String(), nil
	}
	return "", errors.New("must be a string or number")
}

func validateBandScalar(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if len(text) > 64 {
			return errors.New("string exceeds 64 bytes")
		}
		return nil
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return errors.New("must be a string, integer, null, or absent")
	}
	if _, err := number.Int64(); err != nil {
		return errors.New("number must be a bounded integer")
	}
	return nil
}

func validUCIName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func validIface(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && !strings.ContainsRune("_.-:@", r) {
			return false
		}
	}
	return true
}

func stronger(next, old *int) bool {
	return next != nil && (old == nil || *next > *old)
}

func ptr[T any](value T) *T { return &value }
