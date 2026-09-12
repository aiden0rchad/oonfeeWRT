package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/firmware"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type stubFirmwareChecker struct {
	identities []firmware.Identity
	result     firmware.Result
}

func (s *stubFirmwareChecker) Check(_ context.Context, identity firmware.Identity) firmware.Result {
	s.identities = append(s.identities, identity)
	return s.result
}

func TestFirmwareInventoryIsStoredEvidenceOnly(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("router", true, nil)
	h.seedDevice("pending-router", false, nil)
	setCaps(t, h, device.ID, `{"Board":{"BoardName":"vendor,router","Target":"test/small","RootFSType":"squashfs","Release":"OpenWrt 25.12.1"}}`)
	checker := &stubFirmwareChecker{}
	h.srv.FirmwareChecker = checker
	w := h.do(http.MethodGet, "/api/v1/firmware", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
	var response struct {
		Devices []firmwareDevice `json:"devices"`
		Agent   struct {
			InstalledState string `json:"installed_state"`
		} `json:"agent"`
		Installation struct {
			Enabled        bool     `json:"enabled"`
			RequiredChecks []string `json:"required_checks"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Devices) != 1 || response.Devices[0].Identity.BoardName != "vendor,router" || response.Devices[0].IdentitySource != "stored_capability_probe" {
		t.Fatalf("%+v", response.Devices)
	}
	if len(checker.identities) != 0 {
		t.Fatal("GET contacted a catalogue")
	}
	if response.Installation.Enabled || len(response.Installation.RequiredChecks) < 5 || response.Agent.InstalledState != "not_observed" {
		t.Fatalf("false installation promise: %+v", response)
	}
}

func TestFirmwareCheckUsesStoredIdentityAndRetainsFailureState(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("router", true, nil)
	setCaps(t, h, device.ID, `{"Board":{"BoardName":"vendor,router","Target":"test/small","RootFSType":"squashfs","Release":"OpenWrt 25.12.1"}}`)
	checker := &stubFirmwareChecker{result: firmware.Result{State: "error", Message: "Catalogue unavailable", CheckedAt: 1234, Limitations: []string{}}}
	h.srv.FirmwareChecker = checker
	w := h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/firmware/check", device.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
	var response struct {
		DeviceID int64           `json:"device_id"`
		Result   firmware.Result `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.DeviceID != device.ID || response.Result.State != "error" || len(checker.identities) != 1 || checker.identities[0].Target != "test/small" {
		t.Fatalf("%+v; calls %+v", response, checker.identities)
	}
}

func TestFirmwareIdentityRejectsAProbeFromDifferentFirmware(t *testing.T) {
	device := &store.Device{FWRelease: "OpenWrt 25.12.5", CapsJSON: `{"Board":{"BoardName":"vendor,router","Target":"test/small","RootFSType":"squashfs","Release":"OpenWrt 25.12.1"}}`}
	identity := firmwareIdentity(device)
	if identity.Release != device.FWRelease || identity.BoardName != "" || identity.Target != "" || identity.RootFSType != "" {
		t.Fatalf("combined different firmware observations: %+v", identity)
	}
	device.CapsJSON = "invalid-json"
	if identity = firmwareIdentity(device); identity.Release != device.FWRelease || identity.BoardName != "" {
		t.Fatalf("%+v", identity)
	}
}

func TestFirmwareCheckUnavailablePendingAndMissing(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("router", true, nil)
	pending := h.seedDevice("pending-router", false, nil)
	h.srv.FirmwareChecker = nil
	for _, test := range []struct {
		id     int64
		status int
	}{{device.ID, http.StatusServiceUnavailable}, {pending.ID, http.StatusConflict}, {99999, http.StatusNotFound}} {
		w := h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/firmware/check", test.id), nil)
		if w.Code != test.status {
			t.Fatalf("device %d: %d want %d: %s", test.id, w.Code, test.status, w.Body)
		}
	}
}

func TestFirmwareEndpointsRequireAuthentication(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies = nil
	h.csrf = ""
	for _, test := range []struct{ method, path string }{{http.MethodGet, "/api/v1/firmware"}, {http.MethodPost, "/api/v1/devices/1/firmware/check"}} {
		w := h.do(test.method, test.path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", test.path, w.Code)
		}
	}
}
