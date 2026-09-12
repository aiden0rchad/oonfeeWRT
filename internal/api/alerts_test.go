package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/alerts"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestAlertRulesCRUDAndCSRF(t *testing.T) {
	h := newHarness(t)
	if response := h.do(http.MethodGet, "/api/v1/alerts", nil); response.Code != http.StatusUnauthorized {
		t.Fatal("anonymous alerts read was accepted")
	}
	h.setup()
	device := h.seedDevice("alerts-fixture", true, nil)
	config := alerts.Config{Name: "Offline fixture", Condition: "device_offline", DeviceID: device.ID, HoldSeconds: 60, CooldownSeconds: 600, Enabled: true}
	csrf := h.csrf
	h.csrf = ""
	if response := h.do(http.MethodPost, "/api/v1/alerts/rules", config); response.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF accepted: %d", response.Code)
	}
	h.csrf = csrf
	response := h.do(http.MethodPost, "/api/v1/alerts/rules", config)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var rule alerts.Rule
	if err := json.Unmarshal(response.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/alerts/rules/%d", rule.ID)
	config.Enabled = false
	if response := h.do(http.MethodPut, path, config); response.Code != http.StatusOK {
		t.Fatalf("update: %d %s", response.Code, response.Body.String())
	}
	response = h.do(http.MethodGet, "/api/v1/alerts", nil)
	var list alerts.Collection
	if json.Unmarshal(response.Body.Bytes(), &list) != nil || len(list.Rules) != 1 || list.Rules[0].State != "disabled" {
		t.Fatalf("list: %s", response.Body.String())
	}
	if list.Incidents == nil || list.EvaluatedAt != nil {
		t.Fatal("fresh collection invented incident history/evaluation")
	}
	if response := h.do(http.MethodDelete, path, nil); response.Code != http.StatusOK {
		t.Fatalf("delete: %d", response.Code)
	}
	if response := h.do(http.MethodDelete, path, nil); response.Code != http.StatusNotFound {
		t.Fatalf("repeat delete: %d", response.Code)
	}
	config.HoldSeconds = -1
	if response := h.do(http.MethodPost, "/api/v1/alerts/rules", config); response.Code != http.StatusBadRequest {
		t.Fatal("invalid hold accepted")
	}
}

func TestAlertReadAndConfigurationRoleBoundaries(t *testing.T) {
	h := newHarness(t)
	h.setup()
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []store.AccountRole{store.RoleViewer, store.RoleOperator, store.RoleAdmin, store.RoleOwner} {
		t.Run(string(role), func(t *testing.T) {
			token, session, err := h.srv.sessions.create(owner.ID, owner.Username, role, "192.0.2.10", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
			h.csrf = session.csrf
			if response := h.do(http.MethodGet, "/api/v1/alerts", nil); response.Code != http.StatusOK {
				t.Fatalf("read: %d", response.Code)
			}
			for _, path := range []string{"/api/v1/alerts/rules", "/api/v1/alerts/delivery"} {
				response := h.do(http.MethodPost, path, map[string]any{})
				if role == store.RoleOwner && response.Code == http.StatusForbidden {
					t.Fatal("owner configuration denied")
				}
				if role != store.RoleOwner && response.Code != http.StatusForbidden {
					t.Fatalf("%s configuration accepted: %d", role, response.Code)
				}
			}
		})
	}
}

func TestAlertDestinationNeverReturnsOrPersistsPlaintextSecrets(t *testing.T) {
	h := newHarness(t)
	h.setup()
	url, token := "https://hooks.example.com/sensitive-path?key=sensitive-query", "sensitive-bearer"
	response := h.do(http.MethodPost, "/api/v1/alerts/delivery", alerts.DeliveryRequest{Enabled: true, URL: &url, BearerToken: &token})
	if response.Code != http.StatusOK {
		t.Fatalf("delivery save: %d %s", response.Code, response.Body.String())
	}
	var raw []byte
	if err := h.db.SQL().QueryRow(`SELECT state_json FROM controller_alert_state WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sensitive-path", "sensitive-query", token} {
		if strings.Contains(string(raw), secret) || strings.Contains(response.Body.String(), secret) {
			t.Fatal("destination secret leaked")
		}
	}
	response = h.do(http.MethodGet, "/api/v1/alerts", nil)
	if strings.Contains(response.Body.String(), "ciphertext") || strings.Contains(response.Body.String(), "sensitive-") {
		t.Fatal("read endpoint exposed delivery secrets")
	}
	url = "https://hooks.example.com/%zz?sensitive-query"
	response = h.do(http.MethodPost, "/api/v1/alerts/delivery", alerts.DeliveryRequest{Enabled: true, URL: &url})
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "sensitive-query") {
		t.Fatalf("unsafe parse error: %d %s", response.Code, response.Body.String())
	}
}

func TestAlertOfflineEvidenceHonorsPollingAndDeviceIdentity(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	lastSeen := now.Add(-time.Hour).Unix()
	device := h.seedDevice("offline-fixture", true, &lastSeen)
	rules := []alerts.RuleRecord{{Rule: alerts.Rule{Config: alerts.Config{DeviceID: device.ID, Condition: "device_offline"}, ID: 1}, TargetIdentity: device.MAC}}
	evidence, err := h.srv.alertEvidence(context.Background(), rules, now)
	if err != nil {
		t.Fatal(err)
	}
	if evidence[1].Known {
		t.Fatal("untracked device inferred offline")
	}
	h.fleet.tier[device.ID] = collector.Baseline
	evidence, err = h.srv.alertEvidence(context.Background(), rules, now)
	if err != nil || !evidence[1].Known || evidence[1].Value != 1 {
		t.Fatalf("tracked offline: %+v %v", evidence, err)
	}
	h.fleet.quiesced[device.ID] = true
	evidence, _ = h.srv.alertEvidence(context.Background(), rules, now)
	if evidence[1].Known {
		t.Fatal("suspended polling inferred offline")
	}
	h.fleet.quiesced[device.ID] = false
	rules[0].TargetIdentity = "different-device"
	evidence, _ = h.srv.alertEvidence(context.Background(), rules, now)
	if evidence[1].Known {
		t.Fatal("reused device ID retargeted alert")
	}
}
