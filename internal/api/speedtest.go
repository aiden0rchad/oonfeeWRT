package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/speedtest"
)

type startSpeedTestRequest struct {
	AcknowledgeDataUse bool   `json:"acknowledge_data_use"`
	PlanID             string `json:"plan_id"`
}

type speedTestLimits struct {
	MaxHistory int `json:"max_history"`
}

type speedTestDescriptor struct {
	PlanID             string `json:"plan_id"`
	Provider           string `json:"provider"`
	Method             string `json:"method"`
	Provenance         string `json:"provenance"`
	Endpoint           string `json:"endpoint"`
	DownloadEndpoint   string `json:"download_endpoint"`
	UploadEndpoint     string `json:"upload_endpoint"`
	EstimatedBytes     int64  `json:"estimated_bytes"`
	MaxDurationSeconds int64  `json:"max_duration_seconds"`
}

type speedTestDisclosure struct {
	VantagePoint          string `json:"vantage_point"`
	RouterManagementCalls bool   `json:"router_management_calls"`
	RouterChanges         bool   `json:"router_changes"`
	SaturationWarning     string `json:"saturation_warning"`
	Privacy               string `json:"privacy"`
}

type speedTestsResponse struct {
	Jobs       []speedtest.Job     `json:"jobs"`
	Active     *speedtest.Job      `json:"active"`
	Test       speedTestDescriptor `json:"test"`
	Limits     speedTestLimits     `json:"limits"`
	Disclosure speedTestDisclosure `json:"disclosure"`
}

func (s *Server) handleSpeedTests(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > speedtest.MaxHistory {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 50")
			return
		}
		limit = parsed
	}
	jobs, active, err := s.SpeedTests.List(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read speed tests")
		return
	}
	d := s.SpeedTests.Descriptor()
	writeJSON(w, http.StatusOK, speedTestsResponse{
		Jobs: jobs, Active: active,
		Test: speedTestDescriptor{Provider: d.Provider, Method: d.Method,
			PlanID:     d.PlanID(),
			Provenance: d.Provenance, Endpoint: d.Endpoint, EstimatedBytes: d.EstimatedBytes,
			DownloadEndpoint: d.DownloadEndpoint, UploadEndpoint: d.UploadEndpoint,
			MaxDurationSeconds: int64(d.MaxDuration / time.Second)},
		Limits: speedTestLimits{MaxHistory: speedtest.MaxHistory},
		Disclosure: speedTestDisclosure{
			VantagePoint: "controller-host", RouterManagementCalls: false, RouterChanges: false,
			SaturationWarning: "Test traffic follows the controller host's normal route and may temporarily saturate the gateway/WAN.",
			Privacy:           "Test requests and the controller host's public IP are visible to the provider; measurements remain in the controller database and are not submitted to a results endpoint.",
		},
	})
}

func (s *Server) handleStartSpeedTest(w http.ResponseWriter, r *http.Request) {
	var req startSpeedTestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.AcknowledgeDataUse {
		writeErr(w, http.StatusBadRequest, "acknowledge_data_use must be true")
		return
	}
	if strings.TrimSpace(req.PlanID) == "" {
		writeErr(w, http.StatusBadRequest, "plan_id is required")
		return
	}
	sess, ok := sessionFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	release, ok := s.beginOperation(w, operationSpeedTest)
	if !ok {
		return
	}
	job, err := s.SpeedTests.StartWithCompletion(r.Context(), true, req.PlanID,
		sess.adminID, sess.username, release)
	if err != nil {
		release()
	}
	if errors.Is(err, speedtest.ErrPlanChanged) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, speedtest.ErrActive) {
		_, active, _ := s.SpeedTests.List(r.Context(), 1)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a speed test is already active", "active": active,
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start speed test")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 128 {
		writeErr(w, http.StatusBadRequest, "invalid speed test id")
		return
	}
	job, err := s.SpeedTests.Job(r.Context(), id)
	if errors.Is(err, speedtest.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "speed test not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read speed test")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelSpeedTest(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 128 {
		writeErr(w, http.StatusBadRequest, "invalid speed test id")
		return
	}
	job, err := s.SpeedTests.Cancel(r.Context(), id)
	switch {
	case errors.Is(err, speedtest.ErrNotFound):
		writeErr(w, http.StatusNotFound, "speed test not found")
	case errors.Is(err, speedtest.ErrTerminal):
		writeErr(w, http.StatusConflict, "speed test is already finished")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "could not cancel speed test")
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

// CloseJobs must complete before the database and controller log close.
func (s *Server) CloseJobs(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	remaining := func() time.Duration {
		left := time.Until(deadline)
		if left < 0 {
			return 0
		}
		return left
	}
	ok := true
	if s.SpeedTests != nil && !s.SpeedTests.Close(remaining()) {
		ok = false
	}
	if s.diagnostics != nil && !s.diagnostics.close(remaining()) {
		ok = false
	}
	if s.backups != nil && !s.backups.close(remaining()) {
		ok = false
	}
	if s.restores != nil && !s.restores.close(remaining()) {
		ok = false
	}
	return ok
}
