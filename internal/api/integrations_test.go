package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/integrations"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type stubAdGuardChecker struct{ calls []integrations.AdGuardConfig }

func (s *stubAdGuardChecker) Check(_ context.Context, config integrations.AdGuardConfig) integrations.AdGuardResult {
	s.calls = append(s.calls, config)
	return integrations.AdGuardResult{State: "partial", SourceURL: config.URL, CheckedAt: 1234, Notes: []string{"Fixture snapshot"}}
}

type stubWireGuardReader struct {
	id  int64
	err error
}

func (s *stubWireGuardReader) ReadWireGuard(_ context.Context, id int64) (integrations.WireGuardResult, error) {
	s.id = id
	return integrations.WireGuardUnavailable(id, "No helper observation"), s.err
}

func TestIntegrationConfigurationIsRedactedAndOnlyChecksContactService(t *testing.T) {
	h := newHarness(t)
	h.setup()
	checker := &stubAdGuardChecker{}
	h.srv.AdGuardChecker = checker
	request := map[string]any{"url": "https://dns.example.test/", "username": "reader", "password": "PASSWORD-SENTINEL"}
	w := h.do(http.MethodPost, "/api/v1/integrations/adguard", request)
	if w.Code != http.StatusOK {
		t.Fatalf("save %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "PASSWORD-SENTINEL") || !strings.Contains(w.Body.String(), `"has_password":true`) {
		t.Fatal("secret response or missing presence flag")
	}
	w = h.do(http.MethodGet, "/api/v1/integrations/adguard", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "PASSWORD-SENTINEL") || len(checker.calls) != 0 {
		t.Fatalf("GET had side effects: %d %s", w.Code, w.Body)
	}
	w = h.do(http.MethodPost, "/api/v1/integrations/adguard/check", nil)
	if w.Code != http.StatusOK || len(checker.calls) != 1 || checker.calls[0].Password != "PASSWORD-SENTINEL" {
		t.Fatalf("check %d: %s", w.Code, w.Body)
	}
	var result integrations.AdGuardResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SourceURL != "https://dns.example.test" || result.State != "partial" || result.DNSQueries != nil {
		t.Fatalf("%+v", result)
	}
	w = h.do(http.MethodDelete, "/api/v1/integrations/adguard", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	w = h.do(http.MethodPost, "/api/v1/integrations/adguard/check", nil)
	if w.Code != http.StatusConflict || len(checker.calls) != 1 {
		t.Fatal("removed service contacted")
	}
}

func TestIntegrationCredentialsAreNotForwardedToChangedIdentity(t *testing.T) {
	h := newHarness(t)
	h.setup()
	request := map[string]any{"url": "https://dns.example.test", "username": "reader", "password": "PASSWORD-SENTINEL"}
	if w := h.do(http.MethodPost, "/api/v1/integrations/adguard", request); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	delete(request, "password")
	if w := h.do(http.MethodPost, "/api/v1/integrations/adguard", request); w.Code != http.StatusOK {
		t.Fatalf("unchanged origin should preserve password: %s", w.Body)
	}
	for _, field := range []string{"url", "username", "tls_fingerprint"} {
		changed := map[string]any{"url": "https://dns.example.test", "username": "reader"}
		switch field {
		case "url":
			changed[field] = "https://different.example.test"
		case "username":
			changed[field] = "other"
		case "tls_fingerprint":
			changed[field] = strings.Repeat("ab", 32)
		}
		if w := h.do(http.MethodPost, "/api/v1/integrations/adguard", changed); w.Code != http.StatusBadRequest {
			t.Fatalf("silently forwarded saved credentials on %s change", field)
		}
	}
	request["password"] = ""
	if w := h.do(http.MethodPost, "/api/v1/integrations/adguard", request); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"has_password":false`) {
		t.Fatal("explicit empty password not honored")
	}
}

func TestIntegrationConfigRequiresOwnerCSRFAndRecentReauth(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.srv.Now = func() time.Time { return now }
	h.setup()
	request := map[string]any{"url": "https://dns.example.test", "username": "reader", "password": "PASSWORD-SENTINEL"}
	csrf := h.csrf
	h.csrf = ""
	if w := h.do(http.MethodPost, "/api/v1/integrations/adguard", request); w.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	h.csrf = csrf
	now = now.Add(reauthValidity + time.Second)
	if w := h.do(http.MethodPost, "/api/v1/integrations/adguard", request); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing reauth: %d %s", w.Code, w.Body)
	}
	seedAccount(t, h, "viewer", store.RoleViewer)
	loginAs(t, h, "viewer")
	for _, path := range []string{"/api/v1/integrations/adguard", "/api/v1/integrations/adguard/check", "/api/v1/devices/1/wireguard/check"} {
		if w := h.do(http.MethodPost, path, request); w.Code != http.StatusForbidden {
			t.Fatalf("viewer reached %s: %d", path, w.Code)
		}
	}
	if w := h.do(http.MethodGet, "/api/v1/integrations/adguard", nil); w.Code != http.StatusOK {
		t.Fatalf("viewer cannot read safe config: %d", w.Code)
	}
}

func TestWireGuardCheckBoundToDeviceAndErrorsAreSanitized(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("router", true, nil)
	reader := &stubWireGuardReader{err: errors.New("PRIVATE-KEY-SENTINEL")}
	h.srv.WireGuard = reader
	w := h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/wireguard/check", device.ID), nil)
	if w.Code != http.StatusOK || reader.id != device.ID || strings.Contains(w.Body.String(), "PRIVATE-KEY-SENTINEL") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var result integrations.WireGuardResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.DeviceID != device.ID || result.State != "unavailable" || result.Interfaces == nil {
		t.Fatalf("%+v", result)
	}
	h.srv.WireGuard = nil
	w = h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/wireguard/check", device.ID), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"unavailable"`) {
		t.Fatal("missing reader misreported")
	}
}
