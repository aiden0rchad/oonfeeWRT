package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestAuthenticatedWLANReadsNeverRevealThePassphrase(t *testing.T) {
	const secret = "wlan-api-redacted-placeholder-5vT2"
	h := newHarness(t)
	h.setup()

	ctx := context.Background()
	n := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := h.db.SaveNetwork(ctx, n); err != nil {
		t.Fatal(err)
	}
	g := &model.APGroup{Name: "all"}
	if err := h.db.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}

	created := h.do(http.MethodPost, "/api/v1/site/wlans", map[string]any{
		"ssid": "redacted-net", "network_id": n.ID, "group_id": g.ID,
		"bands": []string{"2g"}, "security_mode": "psk2", "pmf": "0",
		"key": secret, "enabled": true,
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	assertNoSecret := func(surface string, body string) {
		t.Helper()
		if strings.Contains(body, secret) || strings.Contains(body, `"key":"`) {
			t.Errorf("%s returned the passphrase: %s", surface, body)
		}
	}
	assertNoSecret("save response", created.Body.String())

	var saved struct {
		WLAN struct {
			ID     int  `json:"id"`
			HasKey bool `json:"has_key"`
		} `json:"wlan"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.WLAN.ID == 0 || !saved.WLAN.HasKey {
		t.Fatalf("save hid whether the WLAN is secured: %s", created.Body.String())
	}

	for name, path := range map[string]string{
		"site list":    "/api/v1/site",
		"single WLAN":  "/api/v1/site/wlans/" + itoa(saved.WLAN.ID),
		"old reveal":   "/api/v1/site/wlans/" + itoa(saved.WLAN.ID) + "?reveal=1",
		"audit events": "/api/v1/events?category=audit",
	} {
		res := h.do(http.MethodGet, path, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, res.Code, res.Body.String())
		}
		assertNoSecret(name, res.Body.String())
	}

	// The redacted edit contract remains safe: leaving key blank preserves the
	// stored value rather than silently opening the WLAN.
	updated := h.do(http.MethodPost, "/api/v1/site/wlans/"+itoa(saved.WLAN.ID), map[string]any{
		"ssid": "renamed-net", "network_id": n.ID, "group_id": g.ID,
		"bands": []string{"2g"}, "security_mode": "psk2", "pmf": "0",
		"enabled": true,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("blank-key update: %d %s", updated.Code, updated.Body.String())
	}
	site, err := h.db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.WLANs) != 1 || site.WLANs[0].Security.Key != secret {
		t.Fatal("a redacted write-back did not preserve the existing WLAN key")
	}

	// The legacy reveal-shaped URL is still behind authentication and remains
	// redacted when a session is absent.
	h.cookies, h.csrf = nil, ""
	unauth := h.do(http.MethodGet,
		"/api/v1/site/wlans/"+itoa(saved.WLAN.ID)+"?reveal=1", nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated legacy reveal: %d, want 401", unauth.Code)
	}
	assertNoSecret("unauthenticated error", unauth.Body.String())
}
