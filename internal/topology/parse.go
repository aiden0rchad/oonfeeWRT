// Package topology decodes stock OpenWrt observations and turns them into a
// provenance-preserving graph. Text parsers live here because constrained
// targets commonly ship BusyBox ip and brctl, but not iproute2's JSON tools.
package topology

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxExecOutput = 1 << 20

// ExecOutput is the bounded result returned by OpenWrt file.exec.
type ExecOutput struct {
	Code   int
	Stdout []byte
	Stderr string
}

// DecodeExecOutput validates the file.exec envelope before a text parser sees
// stdout. A missing code is not assumed to mean success, and failed commands
// remain source errors rather than demonstrated empty tables.
func DecodeExecOutput(raw []byte) (ExecOutput, error) {
	var payload struct {
		Code   *int   `json:"code"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if err := decodeJSONObject(raw, &payload); err != nil {
		return ExecOutput{}, fmt.Errorf("topology: file.exec: %w", err)
	}
	if payload.Code == nil {
		return ExecOutput{}, fmt.Errorf("topology: file.exec response has no exit code")
	}
	if len(payload.Stdout) > maxExecOutput || len(payload.Stderr) > maxExecOutput ||
		!utf8.ValidString(payload.Stdout) || !utf8.ValidString(payload.Stderr) {
		return ExecOutput{}, fmt.Errorf("topology: file.exec output is invalid or exceeds %d bytes", maxExecOutput)
	}
	out := ExecOutput{Code: *payload.Code, Stdout: []byte(payload.Stdout), Stderr: payload.Stderr}
	if out.Code != 0 {
		reason := strings.Join(strings.Fields(out.Stderr), " ")
		if chars := []rune(reason); len(chars) > 240 {
			reason = string(chars[:240])
		}
		if reason == "" {
			reason = "no diagnostic"
		}
		return out, fmt.Errorf("topology: command exited %d: %s", out.Code, reason)
	}
	return out, nil
}

// ParseIPv4MainDefaultRoute returns the one usable, lowest-metric main-table
// IPv4 default device from stock BusyBox `ip -4 route show table all` output.
// The installed kernel table is stronger evidence than netifd intent, which
// may retain a route whose installation failed. Policy routing remains outside
// this observation and must not be inferred from it.
func ParseIPv4MainDefaultRoute(raw []byte) (string, bool, error) {
	bestDevice := ""
	bestMetric := 0
	found := false
	for lineNo, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		device, metric, eligible, err := parseIPv4DefaultRouteLine(fields)
		if err != nil {
			return "", false, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		if !eligible {
			continue
		}
		if !found || metric < bestMetric {
			bestDevice, bestMetric, found = device, metric, true
			continue
		}
		if metric == bestMetric && device != bestDevice {
			return "", false, fmt.Errorf("ambiguous equal-metric default routes on %q and %q", bestDevice, device)
		}
	}
	return bestDevice, found, nil
}

func parseIPv4DefaultRouteLine(fields []string) (string, int, bool, error) {
	index := 0
	eligible := true
	if routeType(fields[0]) {
		eligible = strings.EqualFold(fields[0], "unicast")
		index++
	}
	if index >= len(fields) || fields[index] != "default" {
		return "", 0, false, nil
	}
	index++
	device := ""
	metric := 0
	viaSet, metricSet, tableSet, sourceSet := false, false, false, false
	for index < len(fields) {
		keyword := fields[index]
		index++
		value := func() (string, error) {
			if index >= len(fields) {
				return "", fmt.Errorf("%s has no value", keyword)
			}
			out := fields[index]
			index++
			return out, nil
		}
		switch keyword {
		case "via":
			if viaSet {
				return "", 0, false, fmt.Errorf("route has multiple gateways")
			}
			viaSet = true
			gateway, err := value()
			if err != nil {
				return "", 0, false, err
			}
			if gateway == "inet" {
				gateway, err = value()
				if err != nil {
					return "", 0, false, err
				}
			}
			if parsed := net.ParseIP(gateway); parsed == nil || parsed.To4() == nil {
				return "", 0, false, fmt.Errorf("invalid IPv4 gateway %q", gateway)
			}
		case "dev":
			if device != "" {
				return "", 0, false, fmt.Errorf("route has multiple interfaces")
			}
			var err error
			device, err = value()
			if err != nil {
				return "", 0, false, err
			}
			if !validInterfaceName(device) {
				return "", 0, false, fmt.Errorf("invalid interface %q", device)
			}
		case "metric":
			if metricSet {
				return "", 0, false, fmt.Errorf("route has multiple metrics")
			}
			metricSet = true
			rawMetric, err := value()
			if err != nil {
				return "", 0, false, err
			}
			parsed, err := strconv.Atoi(rawMetric)
			if err != nil || parsed < 0 {
				return "", 0, false, fmt.Errorf("invalid metric %q", rawMetric)
			}
			metric = parsed
		case "table":
			if tableSet {
				return "", 0, false, fmt.Errorf("route has multiple tables")
			}
			tableSet = true
			table, err := value()
			if err != nil {
				return "", 0, false, err
			}
			eligible = eligible && (table == "main" || table == "254")
		case "from":
			if sourceSet {
				return "", 0, false, fmt.Errorf("route has multiple sources")
			}
			sourceSet = true
			source, err := value()
			if err != nil {
				return "", 0, false, err
			}
			unconstrained := source == "all" || source == "0.0.0.0/0"
			if !unconstrained {
				if ip := net.ParseIP(source); ip == nil || ip.To4() == nil {
					if _, subnet, parseErr := net.ParseCIDR(source); parseErr != nil || subnet == nil || subnet.IP.To4() == nil {
						return "", 0, false, fmt.Errorf("invalid IPv4 source")
					}
				}
			}
			eligible = eligible && unconstrained
		case "proto", "scope", "src", "pref", "expires", "mtu", "advmss", "hoplimit", "realm",
			"rtt", "rttvar", "rto_min", "ssthresh", "cwnd", "initcwnd", "initrwnd", "features",
			"quickack", "congctl", "fastopen_no_cookie", "uid", "nhid":
			if _, err := value(); err != nil {
				return "", 0, false, err
			}
		case "linkdown", "dead":
			eligible = false
		case "onlink", "pervasive", "offload", "trap", "notify", "cache":
		case "nexthop":
			return "", 0, false, fmt.Errorf("multipath default route is not supported")
		default:
			return "", 0, false, fmt.Errorf("unsupported default-route attribute %q", keyword)
		}
	}
	if !eligible {
		return "", 0, false, nil
	}
	if device == "" {
		return "", 0, false, fmt.Errorf("default route has no interface")
	}
	return device, metric, true, nil
}

func routeType(raw string) bool {
	switch strings.ToLower(raw) {
	case "unicast", "local", "broadcast", "multicast", "throw", "unreachable", "prohibit", "blackhole", "nat", "anycast":
		return true
	default:
		return false
	}
}

// Neighbor is one row from `ip -4/-6 neigh show`.
type Neighbor struct {
	Family           int      `json:"family"`
	Address          string   `json:"address"`
	Interface        string   `json:"interface"`
	MAC              string   `json:"mac,omitempty"`
	State            string   `json:"state"`
	Flags            []string `json:"flags,omitempty"`
	UsedSeconds      *int64   `json:"used_seconds,omitempty"`
	ConfirmedSeconds *int64   `json:"confirmed_seconds,omitempty"`
	UpdatedSeconds   *int64   `json:"updated_seconds,omitempty"`
}

var neighborStates = map[string]bool{
	"NONE": true, "INCOMPLETE": true, "REACHABLE": true, "STALE": true,
	"DELAY": true, "PROBE": true, "FAILED": true, "NOARP": true,
	"PERMANENT": true,
}

var neighborFlags = map[string]bool{
	"router": true, "proxy": true, "extern_learn": true, "extern_valid": true,
	"offload": true, "managed": true, "use": true,
}

// ParseNeighbors strictly reads BusyBox `ip neigh show` output. A malformed
// row fails the observation rather than becoming a plausible-looking edge.
func ParseNeighbors(family int, out []byte) ([]Neighbor, error) {
	if family != 4 && family != 6 {
		return nil, fmt.Errorf("topology: neighbor family must be 4 or 6")
	}
	var rows []Neighbor
	err := scanLines(out, func(lineNo int, line string) error {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return nil
		}
		addr, err := netip.ParseAddr(fields[0])
		if err != nil || addr.Zone() != "" || (family == 4) != addr.Is4() {
			return fmt.Errorf("line %d: invalid IPv%d address %q", lineNo, family, fields[0])
		}
		row := Neighbor{Family: family, Address: addr.String()}
		var haveRef, haveUsed, haveProbes bool
		for i := 1; i < len(fields); i++ {
			switch token := fields[i]; {
			case token == "dev":
				if row.Interface != "" || i+1 == len(fields) {
					return fmt.Errorf("line %d: invalid dev field", lineNo)
				}
				i++
				if !validInterfaceName(fields[i]) {
					return fmt.Errorf("line %d: invalid interface %q", lineNo, fields[i])
				}
				row.Interface = fields[i]
			case token == "lladdr":
				if row.MAC != "" || i+1 == len(fields) {
					return fmt.Errorf("line %d: invalid lladdr field", lineNo)
				}
				i++
				row.MAC, err = canonicalMAC(fields[i])
				if err != nil {
					return fmt.Errorf("line %d: %w", lineNo, err)
				}
			case neighborStates[strings.ToUpper(token)]:
				if row.State != "" {
					return fmt.Errorf("line %d: multiple neighbor states", lineNo)
				}
				row.State = strings.ToLower(token)
			case token == "ref" || token == "probes":
				if i+1 == len(fields) || (token == "ref" && haveRef) ||
					(token == "probes" && haveProbes) {
					return fmt.Errorf("line %d: invalid %s field", lineNo, token)
				}
				i++
				value, err := strconv.ParseUint(fields[i], 10, 32)
				if err != nil {
					return fmt.Errorf("line %d: invalid %s field", lineNo, token)
				}
				_ = value // Presence does not depend on refcount or probe count.
				if token == "ref" {
					haveRef = true
				} else {
					haveProbes = true
				}
			case token == "used":
				if i+1 == len(fields) || haveUsed {
					return fmt.Errorf("line %d: invalid used field", lineNo)
				}
				i++
				ages := strings.Split(fields[i], "/")
				if len(ages) != 3 {
					return fmt.Errorf("line %d: invalid used field", lineNo)
				}
				parsed := [3]int64{}
				for j, age := range ages {
					value, err := strconv.ParseInt(age, 10, 64)
					if err != nil || value < 0 {
						return fmt.Errorf("line %d: invalid used field", lineNo)
					}
					parsed[j] = value
				}
				row.UsedSeconds = &parsed[0]
				row.ConfirmedSeconds = &parsed[1]
				row.UpdatedSeconds = &parsed[2]
				haveUsed = true
			case neighborFlags[token]:
				row.Flags = append(row.Flags, token)
			default:
				return fmt.Errorf("line %d: unsupported neighbor token %q", lineNo, token)
			}
		}
		if row.Interface == "" || row.State == "" {
			return fmt.Errorf("line %d: neighbor requires dev and state", lineNo)
		}
		sort.Strings(row.Flags)
		rows = append(rows, row)
		return nil
	})
	return rows, err
}

// FDBEntry is one row from `brctl showmacs BRIDGE`. BusyBox does not expose a
// VLAN in this format; consumers must retain that ambiguity.
type FDBEntry struct {
	Port       int     `json:"port"`
	MAC        string  `json:"mac"`
	Local      bool    `json:"local"`
	AgeSeconds float64 `json:"age_seconds"`
}

// ParseShowMACs strictly reads BusyBox `brctl showmacs` output.
func ParseShowMACs(out []byte) ([]FDBEntry, error) {
	var rows []FDBEntry
	header := false
	err := scanLines(out, func(lineNo int, line string) error {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return nil
		}
		if !header {
			if strings.Join(strings.Fields(strings.ToLower(trimmed)), " ") !=
				"port no mac addr is local? ageing timer" {
				return fmt.Errorf("line %d: unexpected showmacs header", lineNo)
			}
			header = true
			return nil
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 4 {
			return fmt.Errorf("line %d: showmacs row has %d fields", lineNo, len(fields))
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || port < 0 {
			return fmt.Errorf("line %d: invalid bridge port %q", lineNo, fields[0])
		}
		mac, err := canonicalMAC(fields[1])
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		var local bool
		switch fields[2] {
		case "yes":
			local = true
		case "no":
		default:
			return fmt.Errorf("line %d: invalid local flag %q", lineNo, fields[2])
		}
		age, err := strconv.ParseFloat(fields[3], 64)
		if err != nil || age < 0 {
			return fmt.Errorf("line %d: invalid ageing timer %q", lineNo, fields[3])
		}
		rows = append(rows, FDBEntry{Port: port, MAC: mac, Local: local, AgeSeconds: age})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !header {
		return nil, fmt.Errorf("topology: showmacs header missing")
	}
	return rows, nil
}

// STPPort maps BusyBox bridge port numbers to interface names and states.
type STPPort struct {
	Name  string `json:"name"`
	Port  int    `json:"port"`
	State string `json:"state"`
}

// STPState is the useful subset of `brctl showstp BRIDGE`.
type STPState struct {
	Bridge string    `json:"bridge"`
	Ports  []STPPort `json:"ports"`
}

var stpPortHeader = regexp.MustCompile(`^(.+?)\s+\(([0-9]+)\)$`)
var stpStates = map[string]bool{
	"disabled": true, "listening": true, "learning": true,
	"forwarding": true, "blocking": true,
}

// ParseShowSTP strictly reads the bridge name, port-number mapping and port
// states while ignoring unrelated timers and root-election fields.
func ParseShowSTP(out []byte) (STPState, error) {
	var state STPState
	var current *STPPort
	flush := func(lineNo int) error {
		if current == nil {
			return nil
		}
		if current.State == "" {
			return fmt.Errorf("line %d: STP port %q has no state", lineNo, current.Name)
		}
		state.Ports = append(state.Ports, *current)
		current = nil
		return nil
	}
	lastLine := 1
	err := scanLines(out, func(lineNo int, line string) error {
		lastLine = lineNo
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return nil
		}
		if state.Bridge == "" {
			if !validInterfaceName(trimmed) {
				return fmt.Errorf("line %d: invalid bridge name %q", lineNo, trimmed)
			}
			state.Bridge = trimmed
			return nil
		}
		if match := stpPortHeader.FindStringSubmatch(trimmed); match != nil {
			if err := flush(lineNo); err != nil {
				return err
			}
			name := strings.TrimSpace(match[1])
			port, _ := strconv.Atoi(match[2])
			if !validInterfaceName(name) || port < 0 {
				return fmt.Errorf("line %d: invalid STP port header", lineNo)
			}
			current = &STPPort{Name: name, Port: port}
			return nil
		}
		if current == nil {
			return nil
		}
		fields := strings.Fields(trimmed)
		for i, field := range fields {
			if field != "state" {
				continue
			}
			if i+1 == len(fields) || !stpStates[fields[i+1]] || current.State != "" {
				return fmt.Errorf("line %d: invalid STP state", lineNo)
			}
			current.State = fields[i+1]
			break
		}
		return nil
	})
	if err != nil {
		return STPState{}, err
	}
	if state.Bridge == "" {
		return STPState{}, fmt.Errorf("topology: showstp bridge missing")
	}
	if err := flush(lastLine + 1); err != nil {
		return STPState{}, err
	}
	sort.Slice(state.Ports, func(i, j int) bool {
		if state.Ports[i].Port == state.Ports[j].Port {
			return state.Ports[i].Name < state.Ports[j].Name
		}
		return state.Ports[i].Port < state.Ports[j].Port
	})
	return state, nil
}

// NetworkDevice is the non-secret topology subset of
// luci-rpc.getNetworkDevices.
type NetworkDevice struct {
	Name     string   `json:"name"`
	DevType  string   `json:"devtype,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	MAC      string   `json:"mac,omitempty"`
	Up       *bool    `json:"up,omitempty"`
	BridgeOf []string `json:"bridge_of,omitempty"`
}

// DecodeNetworkDevices selectively decodes getNetworkDevices. Unknown fields
// are discarded rather than retained as arbitrary device payload.
func DecodeNetworkDevices(raw []byte) ([]NetworkDevice, error) {
	var payload map[string]struct {
		DevType  string   `json:"devtype"`
		Parent   string   `json:"parent"`
		MACAddr  string   `json:"macaddr"`
		MAC      string   `json:"mac"`
		Up       *bool    `json:"up"`
		BridgeOf []string `json:"ports"`
	}
	if err := decodeJSONObject(raw, &payload); err != nil {
		return nil, fmt.Errorf("topology: getNetworkDevices: %w", err)
	}
	rows := make([]NetworkDevice, 0, len(payload))
	for name, v := range payload {
		if !validInterfaceName(name) {
			return nil, fmt.Errorf("topology: getNetworkDevices: invalid device name %q", name)
		}
		row := NetworkDevice{Name: name, DevType: v.DevType, Parent: v.Parent, Up: v.Up}
		if row.Parent != "" && !validInterfaceName(row.Parent) {
			return nil, fmt.Errorf("topology: getNetworkDevices: invalid parent %q", row.Parent)
		}
		mac := v.MACAddr
		if mac == "" {
			mac = v.MAC
		}
		if mac != "" {
			var err error
			row.MAC, err = canonicalMAC(mac)
			if err != nil {
				return nil, fmt.Errorf("topology: getNetworkDevices %q: %w", name, err)
			}
		}
		for _, port := range v.BridgeOf {
			if !validInterfaceName(port) {
				return nil, fmt.Errorf("topology: getNetworkDevices %q: invalid bridge port %q", name, port)
			}
			row.BridgeOf = append(row.BridgeOf, port)
		}
		sort.Strings(row.BridgeOf)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// WirelessInterface is the non-secret topology subset of one wifi-iface.
type WirelessInterface struct {
	IfName   string   `json:"ifname,omitempty"`
	Section  string   `json:"section,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	BSSID    string   `json:"bssid,omitempty"`
	Networks []string `json:"networks,omitempty"`
}

// WirelessRadio is keyed by the stable UCI radio name (for example radio0),
// never by a transient phy or BSS interface name.
type WirelessRadio struct {
	Key        string              `json:"key"`
	Up         *bool               `json:"up,omitempty"`
	Band       string              `json:"band,omitempty"`
	Channel    string              `json:"channel,omitempty"`
	Interfaces []WirelessInterface `json:"interfaces"`
}

type stringList []string

func (s *stringList) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one != "" {
			*s = []string{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return fmt.Errorf("network must be a string or string array")
	}
	*s = many
	return nil
}

// DecodeWirelessDevices selectively decodes getWirelessDevices. Its structs
// intentionally have no key/passphrase field, so plaintext WLAN credentials
// are discarded inside json.Unmarshal and cannot escape through logs or graph
// evidence.
func DecodeWirelessDevices(raw []byte) ([]WirelessRadio, error) {
	var payload map[string]struct {
		Up     *bool `json:"up"`
		Config struct {
			Band    string          `json:"band"`
			Channel json.RawMessage `json:"channel"`
		} `json:"config"`
		Interfaces []struct {
			IfName  string `json:"ifname"`
			Section string `json:"section"`
			IWInfo  struct {
				BSSID string `json:"bssid"`
			} `json:"iwinfo"`
			Config struct {
				Mode    string     `json:"mode"`
				Network stringList `json:"network"`
			} `json:"config"`
		} `json:"interfaces"`
	}
	if err := decodeJSONObject(raw, &payload); err != nil {
		return nil, fmt.Errorf("topology: getWirelessDevices: %w", err)
	}
	rows := make([]WirelessRadio, 0, len(payload))
	for key, v := range payload {
		if !validIdentifier(key) {
			return nil, fmt.Errorf("topology: getWirelessDevices: invalid radio key %q", key)
		}
		channel, err := decodeChannel(v.Config.Channel)
		if err != nil {
			return nil, fmt.Errorf("topology: getWirelessDevices %q: %w", key, err)
		}
		row := WirelessRadio{Key: key, Up: v.Up, Band: v.Config.Band, Channel: channel}
		for _, iface := range v.Interfaces {
			if iface.IfName != "" && !validInterfaceName(iface.IfName) {
				return nil, fmt.Errorf("topology: getWirelessDevices %q: invalid interface %q", key, iface.IfName)
			}
			bssid := ""
			if iface.IWInfo.BSSID != "" {
				bssid, err = canonicalMAC(iface.IWInfo.BSSID)
				if err != nil {
					return nil, fmt.Errorf("topology: getWirelessDevices %q: %w", key, err)
				}
			}
			entry := WirelessInterface{
				IfName: iface.IfName, Section: iface.Section, Mode: iface.Config.Mode,
				BSSID: bssid, Networks: append([]string(nil), iface.Config.Network...),
			}
			sort.Strings(entry.Networks)
			row.Interfaces = append(row.Interfaces, entry)
		}
		sort.Slice(row.Interfaces, func(i, j int) bool {
			if row.Interfaces[i].IfName == row.Interfaces[j].IfName {
				return row.Interfaces[i].Section < row.Interfaces[j].Section
			}
			return row.Interfaces[i].IfName < row.Interfaces[j].IfName
		})
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows, nil
}

func decodeJSONObject(raw []byte, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	if err := json.Unmarshal(trimmed, dst); err != nil {
		return err
	}
	return nil
}

func decodeChannel(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return "", fmt.Errorf("channel must be a string or integer")
	}
	n, err := strconv.ParseInt(number.String(), 10, 32)
	if err != nil || n < 0 {
		return "", fmt.Errorf("channel must be a string or integer")
	}
	return strconv.FormatInt(n, 10), nil
}

func canonicalMAC(raw string) (string, error) {
	mac, err := net.ParseMAC(raw)
	if err != nil || len(mac) != 6 {
		return "", fmt.Errorf("invalid MAC address %q", raw)
	}
	return strings.ToLower(mac.String()), nil
}

func validInterfaceName(name string) bool {
	return len(name) > 0 && len(name) <= 15 && validIdentifier(name)
}

func validIdentifier(value string) bool {
	if strings.TrimSpace(value) != value || value == "" {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func scanLines(out []byte, fn func(int, string) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	line := 0
	for scanner.Scan() {
		line++
		if err := fn(line, scanner.Text()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("topology: read output: %w", err)
	}
	return nil
}
