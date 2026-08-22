package api

import (
	"net/http"
	"strings"
)

type LLDPCapabilityRequest struct {
	DeviceID                       int64  `json:"-"`
	Action                         string `json:"action"`
	Username                       string `json:"username"`
	Password                       string `json:"password"`
	PrivateKey                     string `json:"private_key,omitempty"`
	PlanHash                       string `json:"plan_hash,omitempty"`
	AcknowledgePackageIndexRefresh bool   `json:"acknowledge_package_index_refresh,omitempty"`
	AcknowledgeReadOnlyDiagnostics bool   `json:"acknowledge_read_only_diagnostics,omitempty"`
	AcknowledgeRouterChanges       bool   `json:"acknowledge_router_changes,omitempty"`
}

type LLDPCapabilityResult struct {
	DeviceID             int64    `json:"device_id"`
	Name                 string   `json:"name"`
	State                string   `json:"state"`
	PackageManager       string   `json:"package_manager,omitempty"`
	RequestedPackages    []string `json:"requested_packages"`
	AddedPackages        []string `json:"added_packages"`
	Plan                 string   `json:"plan,omitempty"`
	PlanHash             string   `json:"plan_hash,omitempty"`
	Diagnostics          string   `json:"diagnostics,omitempty"`
	ConfigurationState   string   `json:"configuration_state,omitempty"`
	ConfiguredInterfaces []string `json:"configured_interfaces,omitempty"`
	ServiceEnabled       *bool    `json:"service_enabled,omitempty"`
	ServiceRunning       *bool    `json:"service_running,omitempty"`
	Detail               string   `json:"detail,omitempty"`
}

func (s *Server) handleLLDPStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	dev, err := s.deviceByID(r, id)
	if handleStoreErr(w, err, "device") {
		return
	}
	install, err := s.Store.CapabilityInstall(r.Context(), id, "lldp")
	if handleStoreErr(w, err, "LLDP capability") {
		return
	}
	out := LLDPCapabilityResult{
		DeviceID: id, Name: dev.Name, State: "not_installed",
		RequestedPackages: []string{"lldpd"}, AddedPackages: []string{},
	}
	if install != nil {
		out.State = install.State
		out.PackageManager = install.PackageManager
		out.RequestedPackages = install.RequestedPackages
		out.AddedPackages = install.AddedPackages
		out.Detail = install.Detail
		if len(install.Services) == 1 {
			service := install.Services[0]
			out.ConfiguredInterfaces = service.ConfiguredInterfaces
			switch {
			case install.State == "error" && service.ConfigBaseline != "":
				out.ConfigurationState = "incomplete"
			case service.ConfigApplied != "":
				out.ConfigurationState = "configured"
			case service.ConfigBaseline != "":
				out.ConfigurationState = "incomplete"
			default:
				out.ConfigurationState = "package_default"
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLLDPCapability(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		writeErr(w, http.StatusServiceUnavailable, "LLDP capability management is not available")
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req LLDPCapabilityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeErr(w, http.StatusBadRequest, "the device's administrator username is required")
		return
	}
	switch req.Action {
	case "diagnose", "plan_configure":
		if !req.AcknowledgeReadOnlyDiagnostics {
			writeErr(w, http.StatusBadRequest, "acknowledge_read_only_diagnostics must be true to authorize reading the router's LLDP configuration and runtime interfaces")
			return
		}
	case "plan_install":
		if !req.AcknowledgePackageIndexRefresh {
			writeErr(w, http.StatusBadRequest, "acknowledge_package_index_refresh must be true because resolving the exact plan may refresh the router package index cache")
			return
		}
	case "install":
		if !req.AcknowledgeRouterChanges {
			writeErr(w, http.StatusBadRequest, "acknowledge_router_changes must be true to authorize changing router packages or services")
			return
		}
		if !req.AcknowledgePackageIndexRefresh {
			writeErr(w, http.StatusBadRequest, "acknowledge_package_index_refresh must be true because installation refreshes the package index once more before applying the reviewed plan")
			return
		}
		if strings.TrimSpace(req.PlanHash) == "" {
			writeErr(w, http.StatusBadRequest, "plan_hash is required; review the exact package plan first")
			return
		}
	case "configure", "remove":
		if !req.AcknowledgeRouterChanges {
			writeErr(w, http.StatusBadRequest, "acknowledge_router_changes must be true to authorize changing router packages or services")
			return
		}
		if strings.TrimSpace(req.PlanHash) == "" {
			writeErr(w, http.StatusBadRequest, "plan_hash is required; review the exact package plan first")
			return
		}
	case "plan_remove":
	default:
		writeErr(w, http.StatusBadRequest, "action must be diagnose, plan_install, install, plan_configure, configure, plan_remove, or remove")
		return
	}
	req.DeviceID = id
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	res, err := s.Enroll.LLDPCapability(r.Context(), req)
	if err != nil {
		s.Log.Warn("LLDP capability action failed", "device", id, "action", req.Action, "err", err)
		s.logAuth(r.Context(), "device.lldp_capability_failed", "warning", req.Username, clientAddr(r))
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
