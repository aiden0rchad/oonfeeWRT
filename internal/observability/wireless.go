package observability

import (
	"net"
	"regexp"
	"strings"
)

type WirelessAction string

const (
	WirelessConnect    WirelessAction = "connect"
	WirelessDisconnect WirelessAction = "disconnect"
	WirelessFT         WirelessAction = "fast_transition"
	WirelessRoam       WirelessAction = "roam"
)

type WirelessLog struct {
	Action WirelessAction
	MAC    string
	Iface  string
}

var (
	macText       = `[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}`
	connectLog    = regexp.MustCompile(`(?:AP-STA-CONNECTED\s+(` + macText + `)|STA\s+(` + macText + `)\s+IEEE 802\.11:\s+associated)`)
	disconnectLog = regexp.MustCompile(`(?:AP-STA-DISCONNECTED\s+(` + macText + `)|STA\s+(` + macText + `)\s+IEEE 802\.11:\s+disassociated)`)
	ftLog         = regexp.MustCompile(`(?i)(?:FT|fast transition).*?(` + macText + `)|(` + macText + `).*?(?:FT|fast transition)`)
	ifacePrefix   = regexp.MustCompile(`^([A-Za-z0-9_.-]{1,32}):\s`)
	secretValue   = regexp.MustCompile(`(?i)(["']?\b(?:password|passwd|passphrase|wpa_passphrase|psk|private_key|secret|token|authorization)\b["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^,\s}\]]+)`)
	bearerValue   = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	privateHeader = regexp.MustCompile(`(?i)-----BEGIN (?:OPENSSH |RSA |EC )?PRIVATE KEY-----`)
)

func SanitizeLogMessage(message string) string {
	if privateHeader.MatchString(message) {
		return "[redacted sensitive log line]"
	}
	message = secretValue.ReplaceAllString(message, "${1}[redacted]")
	return bearerValue.ReplaceAllString(message, "Bearer [redacted]")
}

func ParseWirelessLog(message string) (WirelessLog, bool) {
	for _, candidate := range []struct {
		action WirelessAction
		re     *regexp.Regexp
	}{
		{WirelessConnect, connectLog},
		{WirelessDisconnect, disconnectLog},
		{WirelessFT, ftLog},
	} {
		match := candidate.re.FindStringSubmatch(message)
		if match == nil {
			continue
		}
		mac := firstNonempty(match[1:])
		parsed, err := net.ParseMAC(mac)
		if err != nil || len(parsed) != 6 {
			return WirelessLog{}, false
		}
		iface := ""
		if prefix := ifacePrefix.FindStringSubmatch(message); prefix != nil {
			iface = prefix[1]
		}
		return WirelessLog{Action: candidate.action, MAC: strings.ToLower(parsed.String()), Iface: iface}, true
	}
	return WirelessLog{}, false
}

func firstNonempty(values []string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type Association struct {
	DeviceID       int64
	Iface          string
	AtMS           uint64
	Connected      bool
	FastTransition bool
}

type AssociationEvent struct {
	Action        WirelessAction
	MAC           string
	From          *Association
	To            *Association
	IgnoredReason string
}

// AssociationCorrelator turns ordered per-client logs into connect,
// disconnect and roam transitions. Restore can seed it from the newest durable
// event after a controller restart.
type AssociationCorrelator struct {
	current      map[string]Association
	pendingFT    map[string]pendingFastTransition
	RoamWindowMS uint64
}

type pendingFastTransition struct {
	DeviceID int64
	AtMS     uint64
}

func NewAssociationCorrelator() *AssociationCorrelator {
	return &AssociationCorrelator{current: map[string]Association{}, pendingFT: map[string]pendingFastTransition{}, RoamWindowMS: 30_000}
}

func (c *AssociationCorrelator) Clone() *AssociationCorrelator {
	clone := NewAssociationCorrelator()
	clone.RoamWindowMS = c.RoamWindowMS
	for mac, association := range c.current {
		clone.current[mac] = association
	}
	for mac, at := range c.pendingFT {
		clone.pendingFT[mac] = at
	}
	return clone
}

func (c *AssociationCorrelator) Restore(mac string, association Association) {
	if c.current == nil {
		c.current = map[string]Association{}
	}
	c.current[strings.ToLower(mac)] = association
}

// ResetDevice discards only correlation state produced by one router. A log
// gap, producer restart, or backward clock step makes that router's prior
// ordering unusable, but must not erase a client's newer attachment elsewhere.
func (c *AssociationCorrelator) ResetDevice(deviceID int64) {
	for mac, association := range c.current {
		if association.DeviceID == deviceID {
			delete(c.current, mac)
		}
	}
	for mac, pending := range c.pendingFT {
		if pending.DeviceID == deviceID {
			delete(c.pendingFT, mac)
		}
	}
}

func (c *AssociationCorrelator) Observe(deviceID int64, atMS uint64, log WirelessLog) (AssociationEvent, bool) {
	if c.current == nil {
		c.current = map[string]Association{}
	}
	if c.pendingFT == nil {
		c.pendingFT = map[string]pendingFastTransition{}
	}
	mac := strings.ToLower(log.MAC)
	if pending, ok := c.pendingFT[mac]; ok && atMS > pending.AtMS && atMS-pending.AtMS > c.RoamWindowMS {
		delete(c.pendingFT, mac)
	}
	previous, had := c.current[mac]
	if had && atMS < previous.AtMS {
		return AssociationEvent{MAC: mac, IgnoredReason: "older than current association evidence"}, false
	}
	if had && atMS == previous.AtMS &&
		(previous.DeviceID != deviceID || (previous.Iface != "" && log.Iface != "" && previous.Iface != log.Iface)) {
		return AssociationEvent{MAC: mac, IgnoredReason: "same-timestamp attachment evidence is ambiguous"}, false
	}
	if had && log.Action == WirelessDisconnect && previous.Connected &&
		(previous.DeviceID != deviceID || (previous.Iface != "" && log.Iface != "" && previous.Iface != log.Iface)) {
		return AssociationEvent{MAC: mac, IgnoredReason: "disconnect came from a non-current attachment"}, false
	}
	switch log.Action {
	case WirelessFT:
		c.pendingFT[mac] = pendingFastTransition{DeviceID: deviceID, AtMS: atMS}
		return AssociationEvent{}, false
	case WirelessDisconnect:
		from := previous
		if !had {
			from = Association{DeviceID: deviceID, Iface: log.Iface, AtMS: atMS, Connected: false}
		}
		previous.AtMS, previous.Connected = atMS, false
		if log.Iface != "" {
			previous.Iface = log.Iface
		}
		if previous.DeviceID == 0 {
			previous.DeviceID = deviceID
		}
		c.current[mac] = previous
		return AssociationEvent{Action: WirelessDisconnect, MAC: mac, From: &from}, true
	case WirelessConnect:
		next := Association{DeviceID: deviceID, Iface: log.Iface, AtMS: atMS, Connected: true}
		if pending, ok := c.pendingFT[mac]; ok && pending.DeviceID == deviceID &&
			atMS >= pending.AtMS && atMS-pending.AtMS <= c.RoamWindowMS {
			next.FastTransition = true
		}
		delete(c.pendingFT, mac)
		c.current[mac] = next
		if had && (previous.DeviceID != next.DeviceID || previous.Iface != next.Iface) &&
			atMS >= previous.AtMS && atMS-previous.AtMS <= c.RoamWindowMS {
			from := previous
			return AssociationEvent{Action: WirelessRoam, MAC: mac, From: &from, To: &next}, true
		}
		return AssociationEvent{Action: WirelessConnect, MAC: mac, To: &next}, true
	}
	return AssociationEvent{}, false
}
