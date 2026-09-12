package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/alerts"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	collection, err := s.Alerts.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read alert state")
		return
	}
	writeJSON(w, http.StatusOK, collection)
}

func (s *Server) handleSaveAlertRule(w http.ResponseWriter, r *http.Request) {
	var config alerts.Config
	if !decodeJSON(w, r, &config) {
		return
	}
	if err := config.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var id int64
	if r.PathValue("id") != "" {
		var ok bool
		id, ok = pathID(w, r, "id")
		if !ok {
			return
		}
	}
	device, err := s.Store.DeviceByID(r.Context(), config.DeviceID)
	if handleStoreErr(w, err, "device") {
		return
	}
	if !device.Adopted() {
		writeErr(w, http.StatusBadRequest, "alert targets must be adopted devices")
		return
	}
	rule, err := s.Alerts.SaveRule(r.Context(), id, config, strings.ToLower(device.MAC))
	if errors.Is(err, alerts.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, alerts.ErrConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save alert rule")
		return
	}
	status := http.StatusOK
	if id == 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, rule)
}

func (s *Server) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	err := s.Alerts.DeleteRule(r.Context(), id)
	if errors.Is(err, alerts.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not delete alert rule")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleAlertDelivery(w http.ResponseWriter, r *http.Request) {
	var request alerts.DeliveryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.URL != nil && strings.TrimSpace(*request.URL) != "" {
		if _, err := alerts.ValidateWebhookURL(strings.TrimSpace(*request.URL)); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if request.BearerToken != nil && (len(*request.BearerToken) > 4096 || strings.ContainsAny(*request.BearerToken, "\r\n\x00")) {
		writeErr(w, http.StatusBadRequest, "bearer token must be at most 4096 characters without line breaks")
		return
	}
	result, err := s.Alerts.SetDelivery(r.Context(), request)
	if err != nil {
		// All destination errors are deliberately redacted; query strings can
		// contain webhook credentials, including on URL parser failures.
		writeErr(w, http.StatusBadRequest, "could not save delivery: configure a public HTTPS URL and ensure the controller keyring is available")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) alertEvidence(ctx context.Context, rules []alerts.RuleRecord, now time.Time) (map[int64]alerts.Evidence, error) {
	out := make(map[int64]alerts.Evidence, len(rules))
	if len(rules) == 0 {
		return out, nil
	}
	devices, err := s.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*store.Device, len(devices))
	for _, device := range devices {
		byID[device.ID] = device
	}
	_, gateway := s.dashboardGatewayTopology(ctx, devices, now)
	wan := s.dashboardWANTelemetry(ctx, gateway, now)
	for _, rule := range rules {
		evidence := alerts.Evidence{Reason: "Device or current observation is unavailable"}
		device := byID[rule.DeviceID]
		if device == nil || !device.Adopted() || !strings.EqualFold(device.MAC, rule.TargetIdentity) {
			out[rule.ID] = evidence
			continue
		}
		evidence.DeviceName = device.Name
		if evidence.DeviceName == "" {
			evidence.DeviceName = "Device " + itoa(int(device.ID))
		}
		switch rule.Condition {
		case "device_offline":
			if s.Fleet == nil {
				break
			}
			if device.LastSeen != nil && *device.LastSeen > now.Unix() {
				evidence.Reason = "The last observation is in the future; check the controller clock before inferring device state"
				break
			}
			if _, tracked := s.Fleet.Tier(device.ID); !tracked || s.Fleet.Quiesced(device.ID) {
				evidence.Reason = "Polling is not active or is temporarily suspended; no offline alarm is inferred"
				break
			}
			status := s.viewDevice(device, now).Status
			if status != "online" && status != "offline" {
				break
			}
			evidence.Known, evidence.ObservedAt, evidence.MaxGap = true, now.Unix(), 120
			if status == "offline" {
				evidence.Value = 1
			}
		case "wan_latency", "wan_loss":
			if gateway == nil || gateway.DeviceID != device.ID {
				evidence.Reason = "This device is not the currently observed managed WAN gateway"
				break
			}
			metric := wan.Metrics.Latency
			if rule.Condition == "wan_loss" {
				metric = wan.Metrics.Loss
			}
			if metric.Status != "fresh" || metric.Value == nil || metric.AsOf == nil || math.IsNaN(*metric.Value) || math.IsInf(*metric.Value, 0) || *metric.Value < 0 || (rule.Condition == "wan_loss" && *metric.Value > 100) {
				break
			}
			// Consecutive five-minute buckets are required. The Dashboard may
			// still display a ten-minute-old value, but that is not enough to
			// prove this condition persisted across a missing bucket.
			evidence.Known, evidence.Value, evidence.ObservedAt, evidence.MaxGap = true, *metric.Value, *metric.AsOf/1000, 300
		}
		out[rule.ID] = evidence
	}
	return out, nil
}
