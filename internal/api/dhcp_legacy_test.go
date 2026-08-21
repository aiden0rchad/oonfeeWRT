package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestSiteAPIExposesActionableLegacyDHCPUpgradeBlock(t *testing.T) {
	h := newHarness(t)
	h.setup()
	res, err := h.db.SQL().ExecContext(context.Background(),
		`INSERT INTO networks (name, vlan, cidr, zone, dhcp_json, enabled)
		 VALUES ('small', 20, '10.0.20.1/25', 'small', '{}', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/site", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("site: %d %s", w.Code, w.Body.String())
	}
	var site struct {
		Networks []struct {
			DHCP struct {
				LegacyDefault bool `json:"legacy_default"`
			} `json:"dhcp"`
		} `json:"networks"`
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	if len(site.Networks) != 1 || !site.Networks[0].DHCP.LegacyDefault {
		t.Fatalf("legacy marker missing from site response: %+v", site.Networks)
	}
	if len(site.Problems) != 1 ||
		!strings.Contains(site.Problems[0], "customize Pool start") ||
		!strings.Contains(site.Problems[0], "turn DHCP server off") {
		t.Fatalf("site problems are not actionable: %v", site.Problems)
	}

	// Turning the server off is an explicit resolution. The response and the
	// stored row must stop carrying the upgrade marker.
	w = h.do(http.MethodPost, "/api/v1/site/networks/"+strconv.FormatInt(id, 10), map[string]any{
		"dhcp": map[string]any{
			"enabled": false, "start": 100, "limit": 150, "leasetime": "12h",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("resolve legacy DHCP: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "legacy_default") {
		t.Fatalf("explicit DHCP choice retained legacy marker: %s", w.Body.String())
	}
}
