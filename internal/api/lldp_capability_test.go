package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

func TestLLDPStatusNormalizesPackageArrays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested []string
		added     []string
		wantReq   string
		wantAdded string
	}{
		{name: "nil", wantReq: "[]", wantAdded: "[]"},
		{
			name: "nonempty", requested: []string{"lldpd"}, added: []string{"lldpd", "libcap"},
			wantReq: `["lldpd"]`, wantAdded: `["lldpd","libcap"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			dev := h.seedDevice("lldp-"+tc.name, true, nil)
			ctx := context.Background()
			if err := h.db.BeginCapabilityInstall(ctx, dev.ID, "lldp", "apk", tc.requested, []string{}, nil); err != nil {
				t.Fatal(err)
			}
			if err := h.db.CompleteCapabilityInstall(ctx, dev.ID, "lldp", tc.added, nil); err != nil {
				t.Fatal(err)
			}

			w := h.do(http.MethodGet, "/api/v1/devices/"+strconv.FormatInt(dev.ID, 10)+"/capabilities/lldp", nil)
			assertLLDPPackageJSON(t, w.Code, w.Body.Bytes(), tc.wantReq, tc.wantAdded)
		})
	}
}

func TestLLDPActionNormalizesPackageArrays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    *LLDPCapabilityResult
		wantReq   string
		wantAdded string
	}{
		{
			name: "nil", result: &LLDPCapabilityResult{DeviceID: 7, State: "installed"},
			wantReq: "[]", wantAdded: "[]",
		},
		{
			name: "nonempty", result: &LLDPCapabilityResult{
				DeviceID: 7, State: "installed", RequestedPackages: []string{"lldpd"}, AddedPackages: []string{"lldpd"},
			},
			wantReq: `["lldpd"]`, wantAdded: `["lldpd"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, enroller := harnessWithEnroller(t)
			enroller.lldpRes = tc.result
			w := h.do(http.MethodPost, "/api/v1/devices/7/capabilities/lldp", map[string]any{
				"action": "diagnose", "username": "root", "acknowledge_read_only_diagnostics": true,
			})
			assertLLDPPackageJSON(t, w.Code, w.Body.Bytes(), tc.wantReq, tc.wantAdded)
		})
	}
}

func assertLLDPPackageJSON(t *testing.T, status int, body []byte, wantRequested, wantAdded string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v: %s", err, body)
	}
	if string(got["requested_packages"]) != wantRequested || string(got["added_packages"]) != wantAdded {
		t.Fatalf("requested_packages=%s added_packages=%s body=%s", got["requested_packages"], got["added_packages"], body)
	}
	if _, ok := got["configured_interfaces"]; ok {
		t.Fatalf("configured_interfaces should remain omitted: %s", body)
	}
}
