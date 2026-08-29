package daemon

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func inspectCaps() *capability.Registry {
	caps := capability.NewRegistry()
	caps.Ports = capability.Ports{
		Bridge: "br-lan", LAN: []string{"lan1", "lan2", "lan3", "lan4"}, WAN: "wan",
	}
	caps.Radios = []capability.Radio{{Device: "radio0"}, {Device: "radio1"}}
	caps.Set(capability.FeatSurvey, capability.Present)
	caps.Set(capability.FeatSwitchPorts, capability.Present)
	return caps
}

func TestFunctionRecommendationNeedsActiveWANOrLANDHCP(t *testing.T) {
	caps := inspectCaps()

	_, recommended, unknown := assessFunctions(caps, gatewayEvidence{
		routeKnown: true, dhcpKnown: true,
	})
	if !reflect.DeepEqual(recommended, []string{"ap", "switch"}) {
		t.Fatalf("AP with down WAN and disabled DHCP recommended %v, want AP+switch", recommended)
	}
	if containsString(unknown, "gateway") {
		t.Fatalf("two measured negatives reported gateway as unknown: %v", unknown)
	}

	_, recommended, _ = assessFunctions(caps, gatewayEvidence{
		routeKnown: true, activeDefault: true, dhcpKnown: true,
	})
	if !containsString(recommended, "gateway") {
		t.Fatalf("active WAN default route did not recommend gateway: %v", recommended)
	}

	_, recommended, _ = assessFunctions(caps, gatewayEvidence{
		routeKnown: true, dhcpKnown: true, lanDHCPEnabled: true,
	})
	if !containsString(recommended, "gateway") {
		t.Fatalf("enabled LAN DHCP did not recommend gateway: %v", recommended)
	}
}

func TestAManagementDefaultRouteIsNotGatewayEvidence(t *testing.T) {
	def := []runtimeRoute{{Target: "0.0.0.0", Mask: 0}}
	for _, tc := range []struct {
		name string
		dump runtimeInterfaceDump
		want bool
	}{
		{"LAN management route", runtimeInterfaceDump{Interface: []runtimeInterface{{Name: "lan", Up: true, Route: def}}}, false},
		{"down WAN route", runtimeInterfaceDump{Interface: []runtimeInterface{{Name: "wan", Up: false, Route: def}}}, false},
		{"active WAN route", runtimeInterfaceDump{Interface: []runtimeInterface{{Name: "wan", Up: true, Route: def}}}, true},
		{"non-default WAN route", runtimeInterfaceDump{Interface: []runtimeInterface{{Name: "wan", Up: true, Route: []runtimeRoute{{Target: "192.0.2.0", Mask: 24}}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activeWANDefault(tc.dump, "eth1"); got != tc.want {
				t.Fatalf("activeWANDefault=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestCustomWANNameMatchesTheBoardWANDevice(t *testing.T) {
	dump := runtimeInterfaceDump{Interface: []runtimeInterface{{
		Name: "uplink", L3Device: "eth1", Up: true,
		Route: []runtimeRoute{{Target: "0.0.0.0", Mask: 0}},
	}}}
	if !activeWANDefault(dump, "eth1") {
		t.Fatal("custom WAN interface with the board's WAN device was not recognised")
	}
	if activeWANDefault(dump, "eth9") {
		t.Fatal("unrelated default-route interface was treated as the board WAN")
	}
}

func TestFunctionAssessmentKeepsDeniedEvidenceUnknown(t *testing.T) {
	caps := capability.NewRegistry()
	supported, recommended, unknown := assessFunctions(caps, gatewayEvidence{})
	if len(supported) != 0 || len(recommended) != 0 {
		t.Fatalf("unknown probe invented supported=%v recommended=%v", supported, recommended)
	}
	for _, want := range []string{"gateway", "ap", "switch"} {
		if !containsString(unknown, want) {
			t.Errorf("unknown functions %v omit %q", unknown, want)
		}
	}
}

func TestSwitchModeDoesNotPromiseLegacyVLANControl(t *testing.T) {
	caps := inspectCaps()
	caps.Set(capability.FeatDSA, capability.Absent)
	if got := switchMode(caps); got != "observe-only" {
		t.Fatalf("legacy switch mode=%q, want observe-only", got)
	}
	caps.Set(capability.FeatDSA, capability.Present)
	if got := switchMode(caps); got != "dsa-conditional" {
		t.Fatalf("DSA switch mode=%q, want dsa-conditional", got)
	}
	caps.Ports.LAN = nil
	if got := switchMode(caps); got != "none" {
		t.Fatalf("DSA without manageable LAN ports mode=%q, want none", got)
	}
}

func TestDirectLANDeviceDoesNotInventSwitchSupport(t *testing.T) {
	caps := capability.NewRegistry()
	caps.Ports = capability.Ports{Bridge: "eth1", WAN: "eth0"}
	caps.Radios = []capability.Radio{{Device: "radio0"}, {Device: "radio1"}}
	caps.Set(capability.FeatSurvey, capability.Present)
	caps.Set(capability.FeatDSA, capability.Absent)
	caps.Set(capability.FeatSwitchPorts, capability.Absent)

	supported, recommended, unknown := assessFunctions(caps, gatewayEvidence{
		routeKnown: true, activeDefault: true, dhcpKnown: true,
	})
	for _, got := range [][]string{supported, recommended, unknown} {
		if containsString(got, "switch") {
			t.Fatalf("direct LAN device invented Switch support: supported=%v recommended=%v unknown=%v",
				supported, recommended, unknown)
		}
	}
	if !containsString(supported, "gateway") || !containsString(supported, "ap") ||
		switchMode(caps) != "none" {
		t.Fatalf("direct-LAN gateway/AP assessment is wrong: supported=%v mode=%q",
			supported, switchMode(caps))
	}
}

func containsString(in []string, want string) bool {
	for _, got := range in {
		if got == want {
			return true
		}
	}
	return false
}

func TestInspectProbesWithoutBootstrappingOrWritingInventory(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	d, err := Open(ctx, testConfig(t, "inspect-only"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Inspect(ctx, api.InspectRequest{
		Host: addr, Username: "root", Password: "good",
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.MAC == "" || res.Model == "" || res.RadioCount == nil || *res.RadioCount != 2 {
		t.Fatalf("inspection returned incomplete measured facts: %+v", res)
	}
	if res.LANDevice != "br-lan" || len(res.LANPorts) != 4 || res.WANPort != "wan" {
		t.Fatalf("inspection lost the board's wired layout: %+v", res)
	}
	for _, want := range []string{"gateway", "ap", "switch"} {
		if !containsString(res.FunctionsRecommended, want) {
			t.Errorf("mock WRT recommendation %v omits %q", res.FunctionsRecommended, want)
		}
	}
	if res.SwitchMode != "dsa-conditional" {
		t.Errorf("switch mode=%q, want dsa-conditional", res.SwitchMode)
	}
	if res.CompatibilityReport == nil || res.CompatibilityReport.FormatVersion != 1 ||
		res.CompatibilityReport.Evidence.Source != "read-only-inspection" ||
		res.CompatibilityReport.Evidence.RouterChanges || res.CompatibilityReport.Evidence.Persisted {
		t.Fatalf("inspection did not return a safe compatibility report: %+v", res.CompatibilityReport)
	}
	devices, err := d.Store.Devices(ctx)
	if err != nil || len(devices) != 0 {
		t.Fatalf("read-only inspection changed inventory: devices=%v err=%v", devices, err)
	}

	// The mock has no SSH server, so success already proves the path did not
	// bootstrap over SSH. It also records file writes explicitly.
	c := ubus.New(ubus.Options{Host: addr})
	defer c.Close()
	if err := c.Login(ctx, "root", "good"); err != nil {
		t.Fatal(err)
	}
	var written struct {
		Paths []string `json:"paths"`
	}
	if err := c.Call(ctx, "__test", "written", nil, &written); err != nil {
		t.Fatal(err)
	}
	if len(written.Paths) != 0 {
		t.Fatalf("inspection wrote device files: %v", written.Paths)
	}
}

func TestInspectPreservesCudyM3000V2DirectLANShape(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	c := ubus.New(ubus.Options{Host: addr})
	defer c.Close()
	if err := c.Login(ctx, "root", "good"); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "__test", "set_board", map[string]any{
		"board": map[string]any{
			"kernel": "6.12.63", "hostname": "cudy-m3000",
			"system": "ARMv8 Processor rev 4", "model": "Cudy M3000 v2",
			"board_name": "cudy,m3000-v2-yt8821", "rootfs_type": "squashfs",
			"release": map[string]any{
				"distribution": "OpenWrt", "version": "25.12.5", "revision": "r-test",
				"target": "mediatek/filogic", "description": "OpenWrt 25.12.5",
			},
		},
		"network": map[string]any{
			"lan": map[string]any{"device": "eth1"},
			"wan": map[string]any{"device": "eth0"},
		},
		"network_devices": map[string]any{
			"eth0":  map[string]any{"devtype": "ethernet"},
			"eth1":  map[string]any{"devtype": "ethernet"},
			"wlan0": map[string]any{"devtype": "wlan"},
			"wlan1": map[string]any{"devtype": "wlan"},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, testConfig(t, "inspect-cudy-m3000"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	res, err := d.Inspect(ctx, api.InspectRequest{
		Host: addr, Username: "root", Password: "good",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "Cudy M3000 v2" || res.Class != "B" || res.RadioCount == nil || *res.RadioCount != 2 ||
		res.LANDevice != "eth1" || len(res.LANPorts) != 0 || res.WANPort != "eth0" ||
		res.SwitchMode != "none" || containsString(res.FunctionsSupported, "switch") ||
		containsString(res.FunctionsRecommended, "switch") {
		t.Fatalf("Cudy inspection shape was not preserved: %+v", res)
	}
	report := res.CompatibilityReport
	if report == nil || report.Hardware.Board.Target != "mediatek/filogic" ||
		report.Hardware.Class != "B" || report.Hardware.Ports.LANDevice != "eth1" ||
		report.Hardware.Ports.WANDevice != "eth0" || len(report.Hardware.Ports.LANPorts) != 0 ||
		report.Hardware.Ports.SwitchMode != "none" || containsString(report.Functions.Supported, "switch") {
		t.Fatalf("Cudy compatibility report was not preserved: %+v", report)
	}
}

func TestInspectPinsAHostnameToOneResolvedAddress(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Open(ctx, testConfig(t, "inspect-hostname"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	resolver := &fixedHostResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	d.resolver = resolver

	res, err := d.Inspect(ctx, api.InspectRequest{
		Host: "router.test:" + port, Username: "root", Password: "good",
	})
	if err != nil {
		t.Fatalf("Inspect through resolved hostname: %v", err)
	}
	if res.MAC == "" || res.Model == "" {
		t.Fatalf("inspection returned no identity: %+v", res)
	}
	if resolver.calls != 1 || resolver.host != "router.test" {
		t.Fatalf("workflow resolved %d times for %q", resolver.calls, resolver.host)
	}
}
