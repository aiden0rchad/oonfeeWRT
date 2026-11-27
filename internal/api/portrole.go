package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type portRoleView struct {
	DeviceID  int64  `json:"device_id"`
	Port      string `json:"port"`
	Mode      string `json:"mode"`
	NetworkID int    `json:"network_id,omitempty"`
}

type lanPortsView struct {
	DeviceID int64    `json:"device_id"`
	Ports    []string `json:"ports"`
}

func viewPortRole(role model.PortRole) portRoleView {
	return portRoleView{
		DeviceID: role.DeviceID, Port: role.Port, Mode: string(role.Mode),
		NetworkID: role.NetworkID,
	}
}

func (s *Server) lanPortCatalog(ctx context.Context) ([]lanPortsView, error) {
	devices, err := s.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]lanPortsView, 0, len(devices))
	for _, dev := range devices {
		if dev == nil || !dev.Configurable() {
			continue
		}
		out = append(out, lanPortsView{DeviceID: dev.ID, Ports: lanPortsFromCaps(dev.CapsJSON)})
	}
	return out, nil
}

func lanPortsFromCaps(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var caps capability.Registry
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(caps.Ports.LAN))
	for _, port := range caps.Ports.LAN {
		if port == "" || port == caps.Ports.WAN || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	return out
}

func (s *Server) handleSavePortRole(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	var req portRoleView
	if !decodeJSON(w, r, &req) {
		return
	}
	role := model.PortRole{
		DeviceID: req.DeviceID, Port: strings.TrimSpace(req.Port),
		Mode: model.PortRoleMode(req.Mode), NetworkID: req.NetworkID,
	}
	if role.Mode == model.PortRoleTrunk {
		role.NetworkID = 0
	}
	if err := role.ValidateShape(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.requireConfigurableSiteDevice(w, r, role.DeviceID, "LAN port role") {
		return
	}
	dev, err := s.Store.DeviceByID(r.Context(), role.DeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusBadRequest, "LAN port role device does not exist")
			return
		}
		writeErr(w, http.StatusInternalServerError, "could not read the device")
		return
	}
	if !lanPortAllowed(dev.CapsJSON, role.Port) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"%q is not one of this device's known LAN ports. WAN is not assigned here. Re-probe the device if the port list is stale",
			role.Port))
		return
	}
	if err := s.Store.SavePortRole(r.Context(), role); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.port_role_saved", map[string]any{
		"device_id": role.DeviceID, "port": role.Port, "mode": role.Mode,
		"network_id": role.NetworkID,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"port_role": viewPortRole(role),
		"note": "Saved in the site model. Nothing was written to a router. " +
			"Preview, then Apply, is what changes the port.",
	})
}

func lanPortAllowed(raw, port string) bool {
	for _, known := range lanPortsFromCaps(raw) {
		if known == port {
			return true
		}
	}
	return false
}
