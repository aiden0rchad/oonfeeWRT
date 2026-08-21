package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func seedAPIZones(t *testing.T, h *harness) (guest, iot *model.Network) {
	t.Helper()
	ctx := context.Background()
	guest = &model.Network{Name: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}
	iot = &model.Network{Name: "iot", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "iot", Enabled: true}
	for _, n := range []*model.Network{guest, iot} {
		if err := h.db.SaveNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	return guest, iot
}

func TestSiteZonePolicyAPIExposesEffectiveDefaultsAndExplicitState(t *testing.T) {
	h := newHarness(t)
	h.setup()
	seedAPIZones(t, h)

	site := h.json(h.do(http.MethodGet, "/api/v1/site", nil))
	zones := site["zones"].([]any)
	if len(zones) != 2 {
		t.Fatalf("effective zones = %v", zones)
	}
	for i, name := range []string{"guest", "iot"} {
		z := zones[i].(map[string]any)
		forward := z["forward_to"].([]any)
		if z["name"] != name || z["explicit"] != false || len(forward) != 1 || forward[0] != "wan" {
			t.Fatalf("legacy zone %d = %v", i, z)
		}
	}

	saved := h.do(http.MethodPost, "/api/v1/site/zones/guest", map[string]any{
		"forward_to": []string{"wan", "iot", "wan"},
	})
	if saved.Code != http.StatusOK {
		t.Fatalf("save status %d: %s", saved.Code, saved.Body.String())
	}
	body := h.json(saved)
	forward := body["forward_to"].([]any)
	if body["name"] != "guest" || body["explicit"] != true ||
		len(forward) != 2 || forward[0] != "iot" || forward[1] != "wan" {
		t.Fatalf("save response = %v", body)
	}

	reset := h.do(http.MethodDelete, "/api/v1/site/zones/guest", nil)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset status %d: %s", reset.Code, reset.Body.String())
	}
	body = h.json(reset)
	if body["explicit"] != false || len(body["forward_to"].([]any)) != 1 || body["forward_to"].([]any)[0] != "wan" {
		t.Fatalf("reset response = %v", body)
	}
	missing := h.do(http.MethodDelete, "/api/v1/site/zones/guest", nil)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "no explicit zone policy") {
		t.Fatalf("second reset = %d %s", missing.Code, missing.Body.String())
	}
}

func TestZonePolicyAPIRejectsIncompleteAndInvalidClaims(t *testing.T) {
	h := newHarness(t)
	h.setup()
	seedAPIZones(t, h)

	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"missing list":   {"/api/v1/site/zones/guest", map[string]any{}},
		"null list":      {"/api/v1/site/zones/guest", map[string]any{"forward_to": nil}},
		"self":           {"/api/v1/site/zones/guest", map[string]any{"forward_to": []string{"guest"}}},
		"unknown dest":   {"/api/v1/site/zones/guest", map[string]any{"forward_to": []string{"missing"}}},
		"unknown source": {"/api/v1/site/zones/missing", map[string]any{"forward_to": []string{"wan"}}},
		"reserved source": {"/api/v1/site/zones/wan", map[string]any{
			"forward_to": []string{"guest"},
		}},
		"unknown field": {"/api/v1/site/zones/guest", map[string]any{
			"forward_to": []string{}, "forward_from": []string{},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := h.do(http.MethodPost, tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestZonePolicyAPIPreservesRestrictivePolicyAcrossNetworkRename(t *testing.T) {
	h := newHarness(t)
	h.setup()
	guest, _ := seedAPIZones(t, h)
	if w := h.do(http.MethodPost, "/api/v1/site/zones/guest", map[string]any{
		"forward_to": []string{},
	}); w.Code != http.StatusOK {
		t.Fatalf("save policy: %d %s", w.Code, w.Body.String())
	}

	rename := h.do(http.MethodPost,
		fmt.Sprintf("/api/v1/site/networks/%d", guest.ID),
		map[string]any{"zone": "visitors"})
	if rename.Code != http.StatusBadRequest ||
		!strings.Contains(rename.Body.String(), "update or reset") ||
		!strings.Contains(rename.Body.String(), "guest") {
		t.Fatalf("rename response = %d %s", rename.Code, rename.Body.String())
	}
	site, err := h.db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if site.Networks[0].Zone != "guest" || len(site.Zones) != 1 {
		t.Fatalf("refused rename lost policy: %+v %+v", site.Networks, site.Zones)
	}
}

func TestNetworkAPIRejectsReservedWanManagedZone(t *testing.T) {
	h := newHarness(t)
	h.setup()
	w := h.do(http.MethodPost, "/api/v1/site/networks", map[string]any{
		"name": "bad", "vlan": 20, "cidr": "10.0.20.1/24",
		"zone": "WAN", "enabled": true,
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "destination-only") {
		t.Fatalf("reserved wan response = %d %s", w.Code, w.Body.String())
	}
}

func TestConcurrentPartialNetworkUpdatesDoNotLoseFields(t *testing.T) {
	h := newHarness(t)
	h.setup()
	guest, _ := seedAPIZones(t, h)
	path := fmt.Sprintf("/api/v1/site/networks/%d", guest.ID)
	start := make(chan struct{})
	results := make(chan int, 2)
	go func() {
		<-start
		results <- h.do(http.MethodPost, path, map[string]any{"name": "visitors"}).Code
	}()
	go func() {
		<-start
		results <- h.do(http.MethodPost, path, map[string]any{"cidr": "10.0.20.2/24"}).Code
	}()
	close(start)
	for range 2 {
		if status := <-results; status != http.StatusOK {
			t.Fatalf("partial update status = %d", status)
		}
	}
	site, err := h.db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := site.NetworkByID(guest.ID)
	if !ok || got.Name != "visitors" || got.CIDR != "10.0.20.2/24" {
		t.Fatalf("concurrent partial updates lost a field: %+v", got)
	}
}
