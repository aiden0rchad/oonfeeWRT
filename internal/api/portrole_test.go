package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestPortRoleSaveIsDesiredStateAndSkipsWAN(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := &store.Device{
		MAC: "aa:bb:cc:00:00:55", Host: "192.0.2.55", Name: "gateway",
		Role: "gateway", Functions: []string{"gateway"},
	}
	if err := h.db.UpsertDevice(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	caps := capability.NewRegistry()
	caps.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2", "lan3"}, WAN: "wan"}
	if err := h.db.SetCapabilities(context.Background(), dev.ID, caps, "A"); err != nil {
		t.Fatal(err)
	}
	created := h.do(http.MethodPost, "/api/v1/site/networks", map[string]any{
		"name": "Dom", "vlan": 50, "cidr": "192.168.50.1/24", "zone": "dom", "enabled": true,
		"dhcp": map[string]any{"enabled": true, "start": 100, "limit": 150, "leasetime": "12h"},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create network: %d %s", created.Code, created.Body.String())
	}
	networkID := int(h.json(created)["id"].(float64))

	saved := h.do(http.MethodPost, "/api/v1/site/port-roles", map[string]any{
		"device_id": dev.ID, "port": "lan2", "mode": "access", "network_id": networkID,
	})
	if saved.Code != http.StatusOK {
		t.Fatalf("save role: %d %s", saved.Code, saved.Body.String())
	}
	if note, _ := h.json(saved)["note"].(string); note == "" {
		t.Fatal("save response did not say that no router was written")
	}

	site := h.json(h.do(http.MethodGet, "/api/v1/site", nil))
	roles := site["port_roles"].([]any)
	if len(roles) != 1 {
		t.Fatalf("port_roles = %v", roles)
	}
	role := roles[0].(map[string]any)
	if role["port"] != "lan2" || role["mode"] != "access" || int(role["network_id"].(float64)) != networkID {
		t.Fatalf("role = %v", role)
	}
	var sawLAN, sawWAN bool
	for _, raw := range site["lan_ports"].([]any) {
		row := raw.(map[string]any)
		if int64(row["device_id"].(float64)) != dev.ID {
			continue
		}
		for _, port := range row["ports"].([]any) {
			if port == "lan2" {
				sawLAN = true
			}
			if port == "wan" {
				sawWAN = true
			}
		}
	}
	if !sawLAN || sawWAN {
		t.Fatalf("lan_ports = %v", site["lan_ports"])
	}

	rejected := h.do(http.MethodPost, "/api/v1/site/port-roles", map[string]any{
		"device_id": dev.ID, "port": "wan", "mode": "access", "network_id": networkID,
	})
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("WAN role: %d %s", rejected.Code, rejected.Body.String())
	}
}
