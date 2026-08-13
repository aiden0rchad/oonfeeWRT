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
		{FeatFirewall4, Present},
		{FeatBatching, Present},
		{FeatPreflightDirty, Present},
		{FeatSurvey, Present},
		{FeatAccounting, Present},
		// mwlwifi leaves rx_time/tx_time uninitialised, so the split is not
		// computable even though the fields are right there in the response.
		{FeatAirtimeSplit, Absent},
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
