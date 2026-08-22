package capability

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Probe had no coverage outside -tags=integration, which meant the one function
// that decides what every screen may render was only checked when hardware was
// present. These run against tools/mock_ubus.py, which models the reference
// device — so a regression in the three-state logic fails in CI rather than
// months later on someone's router.

var mockAddr string

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mockAddr = fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("python3", filepath.Join(root, "tools", "mock_ubus.py"),
		"--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := waitReady(mockAddr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "mock not ready:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	os.Exit(code)
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("go.mod not found")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func dial(t *testing.T) *ubus.Client {
	t.Helper()
	c := ubus.New(ubus.Options{Host: mockAddr})
	if err := c.Login(context.Background(), "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func setACLGap(t *testing.T, c *ubus.Client, pairs ...[2]string) {
	t.Helper()
	list := make([]map[string]string, 0, len(pairs))
	for _, p := range pairs {
		list = append(list, map[string]string{"object": p[0], "method": p[1]})
	}
	if err := c.Call(context.Background(), "__test", "set_acl_gap",
		map[string]any{"pairs": list}, nil); err != nil {
		t.Skipf("mock does not support ACL-gap simulation: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Call(context.Background(), "__test", "set_acl_gap",
			map[string]any{"pairs": []any{}}, nil)
	})
}

// Mirrors the integration test, so the two cannot drift.
func TestProbeMatchesTheReferenceDevice(t *testing.T) {
	r, err := Probe(context.Background(), dial(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	checks := []struct {
		feat Feature
		want State
	}{
		{FeatDSA, Present},
		{FeatSwitchPorts, Present},
		{FeatBridgeFDB, Present},
		{FeatFirewall4, Present},
		{FeatBatching, Present},
		{FeatPreflightDirty, Present},
		{FeatSurvey, Present},
		{FeatRadioScan, Present},
		{FeatAccounting, Present},
		// mwlwifi leaves rx_time/tx_time uninitialised, so the split is not
		// computable even though the fields are right there in the response.
		{FeatAirtimeSplit, Absent},
		// wpad-mesh-openssl IS installed — the daemon can do 802.11s — and this
		// is still Absent, which is the whole finding. mwlwifi advertises mesh
		// point and permits it in its interface combinations, then refuses to
		// bring the interface UP. Measured by applying one: uci accepted it,
		// the health check passed, the confirm landed, and `ip link` showed the
		// interface DOWN. See probeMesh.
		{FeatMesh, Absent},
	}
	for _, c := range checks {
		if got := r.State(c.feat); got != c.want {
			t.Errorf("%s = %s, want %s", c.feat, got, c.want)
		}
	}
	if r.Class != ClassA {
		t.Errorf("class = %s, want A (mvebu)", r.Class)
	}
	if !r.HasQuirk("iwinfo.survey", "noise") {
		t.Error("the unsigned-noise quirk should be recorded")
	}
	if !r.HasQuirk("iwinfo.survey", "rx_time/tx_time") {
		t.Error("the dead rx/tx counter quirk should be recorded")
	}
}

func TestRadioScanCapabilityIsProvedWithoutRunningAScan(t *testing.T) {
	r, err := Probe(context.Background(), dial(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.State(FeatRadioScan); got != Present {
		t.Fatalf("radio scan capability = %s", got)
	}

	c := dial(t)
	setACLGap(t, c, [2]string{"iwinfo", "scan"})
	r, err = Probe(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.State(FeatRadioScan); got != NotObservable {
		t.Fatalf("denied radio scan capability = %s, want not-observable", got)
	}
}

func TestInspectionAccessGapsDoNotClaimRouterFeaturesAreAbsent(t *testing.T) {
	c := dial(t)
	setACLGap(t, c,
		[2]string{"session", "access"},
		[2]string{"file", "exec"})
	r, err := Probe(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(r.Notes, "\n")
	for _, want := range []string{
		"this is not evidence that RF scanning is unsupported",
		"This is not evidence that the package manager is absent",
		"optional controller access payload",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("inspection notes missing %q:\n%s", want, notes)
		}
	}
}

// The rule this package exists for. A refused check is a gap in our reach, not
// a fact about the device, and recording it as Absent deletes a working feature
// from the UI. Each of these was a real defect at some point.
func TestRefusedChecksBecomeNotObservableNeverAbsent(t *testing.T) {
	cases := []struct {
		name string
		gaps [][2]string
		feat Feature
	}{
		{"dsa", [][2]string{{"luci-rpc", "getNetworkDevices"}}, FeatDSA},
		{"bridge fdb", [][2]string{{"file", "exec"}}, FeatBridgeFDB},
		{"survey", [][2]string{{"iwinfo", "survey"}}, FeatSurvey},
		{"preflight", [][2]string{{"file", "list"}}, FeatPreflightDirty},
		{"firewall4/accounting", [][2]string{{"file", "exec"}}, FeatFirewall4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t)
			setACLGap(t, c, tc.gaps...)
			r, err := Probe(context.Background(), c)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			got := r.State(tc.feat)
			if got == Absent {
				t.Fatalf("%s was refused, so it must be NotObservable — "+
					"reporting Absent hides a feature the device may well have", tc.feat)
			}
			if got != NotObservable {
				t.Fatalf("%s = %s, want not-observable", tc.feat, got)
			}
			if r.Can(tc.feat) {
				t.Errorf("%s must not be renderable when unobserved", tc.feat)
			}
		})
	}
}

func TestAirtimeNotesSeparateUnknownFromProvenDriverAbsence(t *testing.T) {
	t.Run("survey refused", func(t *testing.T) {
		c := dial(t)
		setACLGap(t, c, [2]string{"iwinfo", "survey"})
		r, err := Probe(context.Background(), c)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if got := r.State(FeatAirtimeSplit); got != NotObservable {
			t.Fatalf("airtime split = %s, want not-observable", got)
		}
		notes := strings.Join(r.Notes, "\n")
		for _, falseClaim := range []string{
			"this driver does not supply",
			"Channel utilization (busy/active) is still available",
		} {
			if strings.Contains(notes, falseClaim) {
				t.Errorf("a refused survey was reported as a driver answer (%q):\n%s",
					falseClaim, notes)
			}
		}
		for _, want := range []string{"could not be determined", "controller access payload"} {
			if !strings.Contains(notes, want) {
				t.Errorf("unknown airtime note does not mention %q:\n%s", want, notes)
			}
		}
	})

	t.Run("counters proven unusable", func(t *testing.T) {
		r, err := Probe(context.Background(), dial(t))
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if got := r.State(FeatAirtimeSplit); got != Absent {
			t.Fatalf("airtime split = %s, want absent", got)
		}
		notes := strings.Join(r.Notes, "\n")
		for _, want := range []string{
			"this driver does not supply usable rx_time/tx_time",
			"Channel utilization (busy/active) is still available",
		} {
			if !strings.Contains(notes, want) {
				t.Errorf("proven driver limitation does not mention %q:\n%s", want, notes)
			}
		}
	})
}

// Adoption uses this to tell the operator which grant would buy a feature back.
func TestUnobservableFeaturesAreReportedForTheOperator(t *testing.T) {
	c := dial(t)
	setACLGap(t, c, [2]string{"luci-rpc", "getNetworkDevices"})
	r, err := Probe(context.Background(), c)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(r.Unobservable()) == 0 {
		t.Fatal("a refused check should appear in Unobservable()")
	}
	if len(r.Notes) == 0 {
		t.Error("the operator needs a note saying which grant is missing")
	}
}

// Survey noise is not merely reported unsigned — on mwlwifi it moves. Measured
// 2026-08-13: the 2.4 GHz radio sat at -95 dBm and jumped to -70 dBm, a 25 dB
// spread, while the 5 GHz radio on the same driver held within 2 dB, and
// channel busy time did not explain the difference. Anything deriving a noise
// floor or an SNR from one sample is guessing.
func TestProbeRecordsUnstableSurveyNoise(t *testing.T) {
	r, err := Probe(context.Background(), dial(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// Two separate facts about one field: how to decode it, and whether one
	// read of it means anything. Both must survive — the registry dedupes by
	// source+field, so they carry different field names on purpose.
	if !r.HasQuirk("iwinfo.survey", "noise") {
		t.Errorf("the unsigned-encoding quirk was lost; quirks: %v", r.Quirks)
	}
	if !r.HasQuirk("iwinfo.survey", "noise:stability") {
		t.Fatalf("no survey noise instability quirk recorded; quirks: %v", r.Quirks)
	}
	var reason string
	for _, q := range r.Quirks {
		if q.Source == "iwinfo.survey" && q.Field == "noise:stability" {
			reason = q.Reason
		}
	}
	if !strings.Contains(reason, "dB between consecutive reads") {
		t.Errorf("the instability quirk does not say what moved; got %q", reason)
	}
}

// Firing proves instability; two samples agreeing proves nothing. The detector
// must not be read backwards, so the threshold has to sit above ordinary jitter
// — measured at 2 dB on a healthy radio.
func TestNoiseJumpThresholdIsAboveNormalJitter(t *testing.T) {
	if noiseJumpDB <= 2 {
		t.Fatalf("noiseJumpDB = %d, at or below the 2 dB jitter measured on a "+
			"stable radio; every device would be flagged", noiseJumpDB)
	}
	if got := noiseDBm(161); got != -95 {
		t.Errorf("noiseDBm(161) = %d, want -95", got)
	}
	if got := noiseDBm(-95); got != -95 {
		t.Errorf("noiseDBm(-95) = %d, want -95", got)
	}
}

// The instability belongs to the radio, not to the method. Measured 2026-08-13
// over 20 samples: iwinfo.info spread 42 dB and iwinfo.survey 46 dB on the same
// 2.4 GHz radio, while the 5 GHz radio on the same driver held within 7 dB on
// both. So it is recorded per radio — gating the device would throw away a good
// 5 GHz reading to punish a bad 2.4 GHz one — and switching source is not a fix.
func TestNoiseStabilityIsPerRadioAndPerSource(t *testing.T) {
	r, err := Probe(context.Background(), dial(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !r.HasQuirk("iwinfo.info", "noise:stability") {
		t.Errorf("iwinfo.info's noise was not checked for stability; quirks: %v", r.Quirks)
	}

	byDev := map[string]State{}
	for _, radio := range r.Radios {
		byDev[radio.Device] = radio.NoiseStable
	}
	if len(byDev) < 2 {
		t.Fatalf("expected two radios, got %v", byDev)
	}
	if got := byDev["wlan0"]; got != Present {
		t.Errorf("wlan0 (steady in the fixture) NoiseStable = %v, want Present", got)
	}
	if got := byDev["wlan1"]; got != Absent {
		t.Errorf("wlan1 (swinging in the fixture) NoiseStable = %v, want Absent", got)
	}
}

// 802.11k neighbour reports.
//
// The interesting state here is not Present — it is what happens when the grant
// is missing, because that is the state of every device adopted before this
// feature existed. The ACL is written to a device twice in its life (adoption
// and un-adoption), so a widened grant does not reach an already-adopted
// device, and recording that as Absent would tell an operator their hardware
// cannot do something it does perfectly well.
func TestProbeFindsNeighborReport(t *testing.T) {
	r, err := Probe(context.Background(), dial(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := r.State(FeatNeighborReport); got != Present {
		t.Errorf("neighbor-report = %s, want Present", got)
	}
}

func TestNeighborReportDeniedIsNotObservable(t *testing.T) {
	c := dial(t)
	setACLGap(t, c,
		[2]string{"hostapd.wlan0", "rrm_nr_get_own"},
		[2]string{"hostapd.wlan1", "rrm_nr_get_own"})

	r, err := Probe(context.Background(), c)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := r.State(FeatNeighborReport); got != NotObservable {
		t.Fatalf("neighbor-report = %s, want NotObservable — a refused check "+
			"is not a negative answer, and reporting Absent here sends an "+
			"operator looking for a hardware limit that does not exist", got)
	}
	// The note is the operator-facing half. Both states gate the same thing, so
	// what changes between them is what someone is told to do about it.
	var found bool
	for _, n := range r.Notes {
		if strings.Contains(n, "access payload is explicitly refreshed") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note names access-payload refresh as the remedy; notes = %q", r.Notes)
	}
}

// hostapd control being unreadable must not be reported twice. Two symptoms for
// one cause reads as two problems, and an operator fixes the wrong one first.
func TestNeighborReportIsNotAskedWithoutHostapd(t *testing.T) {
	c := dial(t)
	setACLGap(t, c,
		[2]string{"hostapd.wlan0", "get_status"},
		[2]string{"hostapd.wlan1", "get_status"})

	r, err := Probe(context.Background(), c)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := r.State(FeatHostapdControl); got != NotObservable {
		t.Errorf("hostapd-control = %s, want NotObservable", got)
	}
	if got := r.State(FeatNeighborReport); got != NotObservable {
		t.Errorf("neighbor-report = %s, want NotObservable", got)
	}
	for _, n := range r.Notes {
		if strings.Contains(n, "re-adopt") {
			t.Errorf("blamed the ACL for a device whose hostapd could not be "+
				"reached at all: %q", n)
		}
	}
}

// A supplicant is what lets a device JOIN a network rather than serve one, and
// it is the half of a wireless uplink that a package list can settle.
func TestUplinkFromPackages(t *testing.T) {
	cases := []struct {
		name string
		pkgs []string
		want State
	}{
		// Every wpad build carries a supplicant, including the ones named for
		// lacking things. wpad-basic is named for lacking MESH, not for lacking
		// the ability to join a network — which is why this rule is more
		// permissive than meshFromPackages and not a copy of it.
		{"full build", []string{"wpad-openssl-2025.08.26-r2"}, Present},
		{"mesh build", []string{"wpad-mesh-openssl-2025.08.26-r2"}, Present},
		{"basic build", []string{"wpad-basic-mbedtls-2025.08.26-r2"}, Present},
		{"mini build", []string{"wpad-mini-2025.08.26-r2"}, Present},
		// hostapd alone serves an AP and cannot join one. A real absence, and
		// one an operator can fix by installing wpad.
		{"hostapd only", []string{"hostapd-openssl-2025.08.26-r2"}, Absent},
		{"no 802.11 daemon", []string{"dnsmasq-2.90", "busybox-1.37"}, Absent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := uplinkFromPackages(c.pkgs); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// Mesh and uplink must not be confused: a build can carry a supplicant and no
// 802.11s, and reading one from the other is how a capability model starts
// lying. wpad-basic is the case that separates them.
func TestUplinkAndMeshDisagreeWhereTheyShould(t *testing.T) {
	basic := []string{"wpad-basic-mbedtls-2025.08.26-r2"}

	if uplinkFromPackages(basic) != Present {
		t.Error("wpad-basic carries a supplicant; it is named for lacking mesh")
	}
	if meshFromPackages(basic) != Absent {
		t.Error("wpad-basic is named for lacking 802.11s")
	}
}

// Present here means the SOFTWARE is there, and the note has to say so. §5q is
// exactly what happens when a capability inferred from a daemon gets read as a
// promise about a driver.
func TestUplinkPresentSaysWhatItDoesNotProve(t *testing.T) {
	r, err := Probe(context.Background(), dial(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if r.State(FeatWirelessUplink) != Present {
		t.Fatalf("uplink = %s, want Present", r.State(FeatWirelessUplink))
	}
	// The note must do two things, and the second one is what a measurement
	// bought: say that the radio half is unsettled, AND describe how it fails.
	// "Present means worth trying" is only useful if the reader can recognise
	// the failure — a station that comes up and never associates looks like a
	// dozen other problems, and on real hardware it cost an afternoon.
	var saysUnsettled, saysHowItFails bool
	for _, n := range r.Notes {
		if strings.Contains(n, "NOT settled") {
			saysUnsettled = true
		}
		if strings.Contains(n, "never associated") {
			saysHowItFails = true
		}
	}
	if !saysUnsettled {
		t.Errorf("nothing records that the radio half is unproven; notes = %q",
			r.Notes)
	}
	if !saysHowItFails {
		t.Errorf("the note does not say what the failure looks like, so a reader "+
			"cannot recognise it; notes = %q", r.Notes)
	}
}

// A stock OpenWrt device: radios present, every one of them disabled, so
// nothing is broadcasting and `iwinfo.devices` is empty.
//
// This is the state every freshly flashed router is in, and it is the one that
// made the controller unable to bring one into service. probeRadios enumerated
// interfaces and called them radios, so the device recorded ZERO radios, and
// the renderer then refused to give it a WLAN — which was the only thing that
// could have created an interface for the radios to become visible through.
//
// It survived for the life of the project because the one device that ever
// worked had its radios switched on by hand before it was adopted.
func TestRadiosAreFoundWhenNothingIsBroadcasting(t *testing.T) {
	c := dial(t)
	if err := c.Call(context.Background(), "__test", "disable_radios", nil, nil); err != nil {
		t.Skipf("mock does not support disable_radios: %v", err)
	}
	t.Cleanup(func() {
		// The fixture is process-wide, so put it back or every later test in
		// this package inherits a device with no interfaces.
		_ = c.Call(context.Background(), "__test", "add_wifi_iface",
			map[string]any{"radio": "radio0", "ifname": "wlan0",
				"section": "default_radio0", "mode": "ap"}, nil)
		_ = c.Call(context.Background(), "__test", "add_wifi_iface",
			map[string]any{"radio": "radio1", "ifname": "wlan1",
				"section": "default_radio1", "mode": "ap"}, nil)
	})

	// The precondition, asserted rather than assumed: if the fixture still
	// lists interfaces this test proves nothing.
	var devs struct {
		Devices []string `json:"devices"`
	}
	if err := c.Call(context.Background(), "iwinfo", "devices", nil, &devs); err != nil {
		t.Fatal(err)
	}
	if len(devs.Devices) != 0 {
		t.Fatalf("precondition failed: iwinfo still lists %v", devs.Devices)
	}

	r, err := Probe(context.Background(), c)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if len(r.Radios) == 0 {
		t.Fatal("a device with disabled radios reported none at all. The " +
			"renderer will refuse to give it a WLAN, and a WLAN is the only " +
			"thing that would make its radios visible — so it can never be " +
			"brought into service")
	}
	bands := map[string]bool{}
	for _, radio := range r.Radios {
		bands[radio.Band] = true
	}
	if !bands["2g"] || !bands["5g"] {
		t.Errorf("both configured bands should be known from the radio list, got %v", bands)
	}

	// And the sampled capabilities must be UNOBSERVABLE rather than Absent:
	// there is no interface to survey, and "we did not look" is not "it cannot".
	for _, f := range []Feature{FeatSurvey, FeatHostapdControl, FeatAirtimeSplit} {
		if got := r.State(f); got == Absent {
			t.Errorf("%s recorded Absent on a radio with no interface to "+
				"sample; nothing demonstrated an absence", f)
		}
	}
	// The mesh gate keys on a hardware name that only iwinfo supplies, so with
	// no interface it cannot run — and a check that cannot run must not pass.
	if got := r.State(FeatMesh); got == Present {
		t.Error("mesh reported Present while the per-driver check could not " +
			"run; an unrunnable check must not report a clean bill")
	}
}

// A device whose radios nothing could enumerate must record NotObservable, not
// Absent.
//
// The two sources are iwinfo.devices, which lists BROADCASTING interfaces, and
// luci-rpc.getWirelessDevices, which is keyed by radio. On a stock router
// nothing is broadcasting, so iwinfo legitimately returns an empty list — and
// if getWirelessDevices is also refused (rpcd-mod-luci absent, which the probe
// tolerates elsewhere), nothing has enumerated the radios at all.
//
// That used to resolve to Absent: the fallback merged "refused" with "there are
// none", the survey verdict got no observations, and the device rendered a
// clean preview with no hardware warnings while adoption told the operator to
// re-adopt it as a switch.
func TestRadiosNobodyCouldEnumerateAreNotObservable(t *testing.T) {
	c := dial(t)
	ctx := context.Background()

	// Nothing broadcasting: iwinfo.devices answers with an empty list.
	if err := c.Call(ctx, "__test", "disable_radios", nil, nil); err != nil {
		t.Skipf("mock cannot empty the interface list: %v", err)
	}
	// And the radio-keyed source is refused.
	setACLGap(t, c, [2]string{"luci-rpc", "getWirelessDevices"})
	t.Cleanup(func() {
		_ = c.Call(ctx, "__test", "add_wifi_iface", map[string]any{
			"radio": "radio0", "ifname": "wlan0",
			"section": "default_radio0", "mode": "ap"}, nil)
		_ = c.Call(ctx, "__test", "add_wifi_iface", map[string]any{
			"radio": "radio1", "ifname": "wlan1",
			"section": "default_radio1", "mode": "ap"}, nil)
	})

	r, err := Probe(ctx, c)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := r.State(FeatSurvey); got != NotObservable {
		t.Errorf("FeatSurvey=%v; nothing enumerated the radios, so this is a "+
			"gap in our reach and not a fact about the device", got)
	}
	if r.RadioInventory != NotObservable {
		t.Errorf("radio inventory=%v, want not-observable", r.RadioInventory)
	}
	for _, f := range []Feature{FeatHostapdControl, FeatNeighborReport} {
		if got := r.State(f); got == Absent {
			t.Errorf("%s recorded Absent on a device whose radios could not be "+
				"listed at all", f)
		}
	}
	var explained bool
	for _, n := range r.Notes {
		if strings.Contains(n, "radios undetermined") {
			explained = true
		}
	}
	if !explained {
		t.Error("nothing in the notes says why the radios are unknown")
	}
}

func TestRadioInventoryFallsBackWhenIWInfoDevicesIsDenied(t *testing.T) {
	c := dial(t)
	setACLGap(t, c, [2]string{"iwinfo", "devices"})
	r, err := Probe(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if r.RadioInventory != Present || len(r.Radios) != 2 {
		t.Fatalf("radio inventory=%v radios=%v, want two luci-rpc radios", r.RadioInventory, r.Radios)
	}
	for _, note := range r.Notes {
		if strings.Contains(note, "radios undetermined") || strings.Contains(note, "Re-adopt") {
			t.Errorf("successful fallback produced false remediation: %q", note)
		}
	}
}
