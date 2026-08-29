package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
)

const (
	compatibilityReportFormat   = "oonfeewrt-compatibility-report"
	compatibilityReportVersion  = 1
	maxCompatibilityReportBytes = 64 << 10
	maxCompatibilityTextBytes   = 256
	maxCompatibilityRadios      = 16
	maxCompatibilityHWModes     = 16
	maxCompatibilityLANPorts    = 64
)

var (
	compatibilityInterface = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
	compatibilityMAC       = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)
	compatibilityIPv4      = regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}\b`)
	compatibilityIPv6      = regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}`)
	compatibilitySecret    = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|psk|api[_ .-]?key|private[_ .-]?key|secret|token|authorization|cookie|csrf|credential)\b\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	compatibilityBearer    = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	compatibilityJWT       = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
)

var compatibilityFeatureAllowlist = []capability.Feature{
	capability.FeatAirtimeSplit,
	capability.FeatBatching,
	capability.FeatBridgeFDB,
	capability.FeatDSA,
	capability.FeatFirewall4,
	capability.FeatHostapdControl,
	capability.FeatMesh,
	capability.FeatNeighborReport,
	capability.FeatAccounting,
	capability.FeatPreflightDirty,
	capability.FeatRadioScan,
	capability.FeatSwitchPorts,
	capability.FeatSurvey,
	capability.FeatWirelessUplink,
}

func buildCompatibilityReport(controllerVersion string, caps *capability.Registry,
	supported, unknown []string, switchMode string, sensitive ...string,
) (*api.CompatibilityReport, error) {
	if caps == nil || len(caps.Radios) > maxCompatibilityRadios || len(caps.Ports.LAN) > maxCompatibilityLANPorts {
		return nil, errors.New("compatibility report evidence exceeds its bounds")
	}
	if switchMode != "dsa-conditional" && switchMode != "observe-only" &&
		switchMode != "unknown" && switchMode != "none" {
		return nil, errors.New("compatibility report contains an unknown switch mode")
	}
	clean := func(value string) string { return compatibilityText(value, sensitive) }
	lan, err := compatibilityInterfaces(caps.Ports.LAN, sensitive)
	if err != nil {
		return nil, err
	}
	lanDevice, err := compatibilityInterfaceName(caps.Ports.Bridge, sensitive)
	if err != nil {
		return nil, err
	}
	wanDevice, err := compatibilityInterfaceName(caps.Ports.WAN, sensitive)
	if err != nil {
		return nil, err
	}

	radios := make([]api.CompatibilityRadio, 0, len(caps.Radios))
	for _, radio := range caps.Radios {
		if len(radio.HWModes) > maxCompatibilityHWModes {
			return nil, errors.New("compatibility report radio modes exceed their bound")
		}
		modes := make([]string, 0, len(radio.HWModes))
		for _, mode := range radio.HWModes {
			modes = append(modes, clean(mode))
		}
		sort.Strings(modes)
		radios = append(radios, api.CompatibilityRadio{
			Band: clean(radio.Band), Hardware: clean(radio.Hardware), HWModes: modes,
			SurveyState: radio.SurveyUsest.String(), NoiseStability: radio.NoiseStable.String(),
		})
	}
	sort.Slice(radios, func(i, j int) bool {
		left, right := radios[i], radios[j]
		if left.Band != right.Band {
			return left.Band < right.Band
		}
		if left.Hardware != right.Hardware {
			return left.Hardware < right.Hardware
		}
		return strings.Join(left.HWModes, ",") < strings.Join(right.HWModes, ",")
	})

	features := make([]api.CompatibilityFeature, 0, len(compatibilityFeatureAllowlist))
	for _, feature := range compatibilityFeatureAllowlist {
		features = append(features, api.CompatibilityFeature{
			Name: string(feature), State: caps.State(feature).String(),
		})
	}
	sort.Slice(features, func(i, j int) bool { return features[i].Name < features[j].Name })

	supported, err = compatibilityFunctions(supported)
	if err != nil {
		return nil, err
	}
	unknown, err = compatibilityFunctions(unknown)
	if err != nil {
		return nil, err
	}
	var radioCount *int
	if caps.RadioInventory.Decided() {
		count := len(caps.Radios)
		radioCount = &count
	}
	report := &api.CompatibilityReport{
		Format: compatibilityReportFormat, FormatVersion: compatibilityReportVersion,
		ControllerVersion: clean(controllerVersion),
		Evidence: api.CompatibilityEvidence{
			Source: "read-only-inspection", RouterChanges: false, Persisted: false,
		},
		Privacy: api.CompatibilityPrivacy{
			Sanitized: true,
			Excluded: []string{
				"credentials and secrets", "router identifiers and site identity", "free-text probe notes",
				"live telemetry and timestamps", "network configuration",
			},
		},
		Hardware: api.CompatibilityHardware{
			Board: api.CompatibilityBoard{
				Model: clean(caps.Board.Model), BoardName: clean(caps.Board.BoardName),
				System: clean(caps.Board.System), Kernel: clean(caps.Board.Kernel),
				Target: clean(caps.Board.Target), Release: clean(caps.Board.Release),
				RootFSType: clean(caps.Board.RootFSType),
			},
			Class: clean(string(caps.Class)), RadioInventoryState: caps.RadioInventory.String(),
			RadioCount: radioCount, Radios: radios,
			Ports: api.CompatibilityPorts{
				LANDevice: lanDevice, LANPorts: lan, WANDevice: wanDevice, SwitchMode: switchMode,
			},
		},
		Features:  features,
		Functions: api.CompatibilityFunctions{Supported: supported, Unknown: unknown},
	}
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > maxCompatibilityReportBytes {
		return nil, errors.New("compatibility report output exceeds its bound")
	}
	return report, nil
}

func compatibilityFunctions(values []string) ([]string, error) {
	allowed := map[string]bool{"gateway": true, "ap": true, "switch": true}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !allowed[value] {
			return nil, errors.New("compatibility report contains an unknown function")
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out, nil
}

func compatibilityInterfaces(values, sensitive []string) ([]string, error) {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value, err := compatibilityInterfaceName(value, sensitive)
		if err != nil {
			return nil, err
		}
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out, nil
}

func compatibilityInterfaceName(value string, sensitive []string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !compatibilityInterface.MatchString(value) {
		return "", errors.New("compatibility report contains an unsafe interface name")
	}
	if net.ParseIP(value) != nil || compatibilityMAC.MatchString(value) {
		return "", errors.New("compatibility report interface name resembles an address")
	}
	for _, secret := range sensitive {
		if secret == "" {
			continue
		}
		if strings.Contains(value, secret) {
			return "", errors.New("compatibility report interface name resembles sensitive input")
		}
		compact := strings.NewReplacer(":", "", "-", "").Replace(secret)
		if len(compact) == 12 && strings.EqualFold(value, compact) {
			return "", errors.New("compatibility report interface name resembles an address")
		}
	}
	return value, nil
}

func compatibilityText(value string, sensitive []string) string {
	value = strings.ToValidUTF8(value, "�")
	for _, secret := range sensitive {
		if secret == "" {
			continue
		}
		if len(secret) < 4 && strings.Contains(value, secret) {
			return "[redacted]"
		}
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	value = compatibilitySecret.ReplaceAllString(value, "$1=[redacted]")
	value = compatibilityBearer.ReplaceAllString(value, "Bearer [redacted]")
	value = compatibilityJWT.ReplaceAllString(value, "[redacted token]")
	value = compatibilityMAC.ReplaceAllString(value, "[redacted address]")
	value = compatibilityIPv4.ReplaceAllString(value, "[redacted address]")
	value = compatibilityIPv6.ReplaceAllStringFunc(value, func(candidate string) string {
		if net.ParseIP(candidate) != nil {
			return "[redacted address]"
		}
		return candidate
	})
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maxCompatibilityTextBytes {
		return value
	}
	value = value[:maxCompatibilityTextBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
