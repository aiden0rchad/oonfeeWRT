package daemon

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
)

func TestCompatibilityReportIsUsefulBoundedAndStrictlySanitized(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Board = capability.Board{
		Model:     "Cudy M3000 pw <script>\r\npassword=router-secret 192.0.2.19",
		BoardName: "cudy,m3000-v2-yt8821", System: "MediaTek root inspect-secret aa:bb:cc:dd:ee:ff",
		Kernel: "6.12.63", Target: "mediatek/filogic", Release: "OpenWrt 25.12.5",
		RootFSType: "squashfs",
	}
	caps.Class = capability.ClassB
	caps.RadioInventory = capability.Present
	caps.Radios = []capability.Radio{{
		Device: "OMIT-RADIO-DEVICE", Phy: "OMIT-PHY", Channel: 987654, Frequency: 7654321,
		Band: "5g", Hardware: "MediaTek MT7981", HWModes: []string{"ax", "ac"},
		SurveyUsest: capability.Present, NoiseStable: capability.Unknown,
	}}
	caps.Ports = capability.Ports{
		Bridge: "eth1", LAN: []string{"lan2", "lan1"}, WAN: "eth0",
		BridgeDevices: []string{"OMIT-RUNTIME-BRIDGE"},
	}
	caps.Notes = []string{"OMIT-NOTE password=note-secret"}
	caps.Binaries["OMIT-BINARY"] = "/usr/bin/secret"
	caps.Set(capability.FeatDSA, capability.Absent)
	caps.Set(capability.FeatSurvey, capability.Present)

	report, err := buildCompatibilityReport(
		"v0.1.2-test", caps, []string{"gateway", "ap"}, []string{"switch"}, "none",
		"192.0.2.19", "root", "pw", "inspect-secret", "aa:bb:cc:dd:ee:ff",
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxCompatibilityReportBytes {
		t.Fatalf("report is %d bytes, limit %d", len(encoded), maxCompatibilityReportBytes)
	}
	for _, forbidden := range []string{
		"router-secret", "inspect-secret", "192.0.2.19", "aa:bb:cc:dd:ee:ff", " pw ", " root ",
		"OMIT-RADIO-DEVICE", "OMIT-PHY", "OMIT-RUNTIME-BRIDGE", "OMIT-NOTE", "OMIT-BINARY",
		"<script>",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("sanitized report leaked %q: %s", forbidden, encoded)
		}
	}
	if report.Format != compatibilityReportFormat || report.FormatVersion != 1 ||
		report.Hardware.Board.Target != "mediatek/filogic" ||
		report.Hardware.Ports.LANDevice != "eth1" || report.Hardware.Ports.WANDevice != "eth0" ||
		!reflect.DeepEqual(report.Hardware.Ports.LANPorts, []string{"lan1", "lan2"}) ||
		!reflect.DeepEqual(report.Functions.Supported, []string{"ap", "gateway"}) {
		t.Fatalf("report lost useful compatibility facts: %+v", report)
	}

	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	assertCompatibilityKeys(t, document,
		"controller_version", "evidence", "features", "format", "format_version", "functions", "hardware", "privacy")
	assertCompatibilityKeys(t, document["hardware"].(map[string]any),
		"board", "class", "ports", "radio_count", "radio_inventory_state", "radios")
	assertCompatibilityKeys(t, document["hardware"].(map[string]any)["board"].(map[string]any),
		"board_name", "kernel", "model", "release", "rootfs_type", "system", "target")
	assertCompatibilityKeys(t, document["hardware"].(map[string]any)["ports"].(map[string]any),
		"lan_device", "lan_ports", "switch_mode", "wan_device")
	assertCompatibilityKeys(t, document["evidence"].(map[string]any),
		"persisted", "router_changes", "source")
	assertCompatibilityKeys(t, document["privacy"].(map[string]any),
		"excluded", "sanitized")
	assertCompatibilityKeys(t, document["functions"].(map[string]any),
		"supported", "unknown")
	assertCompatibilityKeys(t, document["features"].([]any)[0].(map[string]any),
		"name", "state")
	assertCompatibilityKeys(t, document["hardware"].(map[string]any)["radios"].([]any)[0].(map[string]any),
		"band", "hardware", "hw_modes", "noise_stability", "survey_state")
}

func TestCompatibilityReportFailsClosedOutsideSafetyBounds(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Ports.LAN = make([]string, maxCompatibilityLANPorts+1)
	if report, err := buildCompatibilityReport("test", caps, nil, nil, "unknown"); err == nil || report != nil {
		t.Fatalf("oversized port evidence produced report=%+v err=%v", report, err)
	}
	caps.Ports.LAN = []string{"lan1;password=secret"}
	if report, err := buildCompatibilityReport("test", caps, nil, nil, "unknown"); err == nil || report != nil {
		t.Fatalf("unsafe interface evidence produced report=%+v err=%v", report, err)
	}
	caps.Ports.LAN = []string{"192.0.2.1"}
	if report, err := buildCompatibilityReport("test", caps, nil, nil, "unknown"); err == nil || report != nil {
		t.Fatalf("address-like interface evidence produced report=%+v err=%v", report, err)
	}
	caps.Ports = capability.Ports{Bridge: "root"}
	if report, err := buildCompatibilityReport("test", caps, nil, nil, "unknown", "root"); err == nil || report != nil {
		t.Fatalf("credential-shaped interface evidence produced report=%+v err=%v", report, err)
	}
	caps.Ports = capability.Ports{WAN: "aabbccddeeff"}
	if report, err := buildCompatibilityReport("test", caps, nil, nil, "unknown", "aa:bb:cc:dd:ee:ff"); err == nil || report != nil {
		t.Fatalf("compact-address interface evidence produced report=%+v err=%v", report, err)
	}
	if report, err := buildCompatibilityReport("test", capability.NewRegistry(), []string{"router"}, nil, "unknown"); err == nil || report != nil {
		t.Fatalf("unknown function produced report=%+v err=%v", report, err)
	}
	if report, err := buildCompatibilityReport("test", capability.NewRegistry(), nil, nil, "<script>"); err == nil || report != nil {
		t.Fatalf("unknown switch mode produced report=%+v err=%v", report, err)
	}
}

func TestCompatibilityTextRemovesInjectionAndStaysValidUTF8(t *testing.T) {
	raw := "<script>\r\nBearer bearer-secret password=router-secret " +
		"eyJabcdefgh.ijklmnop.qrstuvwx 2001:db8::1 " + strings.Repeat("界", 200) + string([]byte{0xff})
	got := compatibilityText(raw, nil)
	if !utf8.ValidString(got) || len(got) > maxCompatibilityTextBytes {
		t.Fatalf("sanitized text is invalid or oversized: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
	for _, forbidden := range []string{"<script>", "bearer-secret", "router-secret", "eyJabcdefgh", "2001:db8::1", "\r", "\n"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitized text leaked %q: %q", forbidden, got)
		}
	}
}

func assertCompatibilityKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys=%v, want %v", got, want)
	}
}
