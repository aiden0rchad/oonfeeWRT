package api

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const applyOperationPersistTimeout = 5 * time.Second

// ApplyOperationStatus is the durable, secret-free view of an Apply request.
// The request and preview token are deliberately absent.
type ApplyOperationStatus struct {
	OperationID string                       `json:"operation_id"`
	Actor       string                       `json:"actor"`
	State       string                       `json:"state"`
	CreatedAt   int64                        `json:"created_at"`
	StartedAt   *int64                       `json:"started_at,omitempty"`
	FinishedAt  *int64                       `json:"finished_at,omitempty"`
	Result      *ApplyResult                 `json:"result,omitempty"`
	Error       string                       `json:"error,omitempty"`
	WriteState  string                       `json:"write_state,omitempty"`
	Devices     []ApplyOperationDeviceStatus `json:"devices"`
}

// ApplyOperationDeviceStatus is the durable per-device write boundary and
// outcome. Identity is a snapshot: un-adoption cannot erase operation history.
type ApplyOperationDeviceStatus struct {
	Ordinal       int    `json:"ordinal"`
	DeviceID      int64  `json:"device_id"`
	DeviceMAC     string `json:"device_mac"`
	DeviceName    string `json:"device_name"`
	State         string `json:"state"`
	WriteState    string `json:"write_state"`
	RouterOutcome string `json:"router_outcome,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Changes       int    `json:"changes"`
	Reason        string `json:"reason,omitempty"`
	StartedAt     *int64 `json:"started_at,omitempty"`
	FinishedAt    *int64 `json:"finished_at,omitempty"`
}

func (s *Server) handleApplyOperation(w http.ResponseWriter, r *http.Request) {
	var req ApplyRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	if !validOperationID(req.OperationID) {
		writeApplyOperationErr(w, http.StatusBadRequest, "",
			"operation_id must be a lowercase UUID", "none")
		return
	}
	sess, ok := sessionFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	fingerprint, err := s.applyRequestFingerprint(req, sess.adminID)
	if err != nil {
		writeApplyOperationErr(w, http.StatusInternalServerError, req.OperationID,
			"could not bind the apply request", "none")
		return
	}
	op, created, err := s.Store.BeginApplyOperation(r.Context(), req.OperationID,
		fingerprint, sess.adminID, sess.username, s.now().Unix())
	if err != nil {
		writeApplyOperationErr(w, http.StatusInternalServerError, req.OperationID,
			"could not record the apply operation; nothing was written", "none")
		return
	}
	if subtle.ConstantTimeCompare([]byte(op.RequestHash), []byte(fingerprint)) != 1 {
		writeState := op.WriteState
		if writeState != store.ApplyWriteStatePossible {
			writeState = store.ApplyWriteStateNone
		}
		message := "operation_id was already used for a different apply request; this request was rejected"
		if writeState == store.ApplyWriteStatePossible {
			message += "; the existing operation crossed a device write boundary"
		} else {
			message += "; nothing was written for this request"
		}
		writeApplyOperationErr(w, http.StatusConflict, req.OperationID,
			message, writeState)
		return
	}
	if !created {
		// Begin returns the parent row needed for the collision check. Reload the
		// complete receipt so a running idempotent replay also sees the durable
		// per-device boundary and the latest terminal transition.
		replay, err := s.Store.ApplyOperation(r.Context(), req.OperationID)
		if err != nil {
			writeApplyOperationErr(w, http.StatusInternalServerError, req.OperationID,
				"could not read the existing apply operation; check its status before retrying",
				"possible")
			return
		}
		s.writeApplyOperationReplay(w, replay)
		return
	}

	if strings.TrimSpace(req.PreviewToken) == "" {
		s.rejectApplyOperation(w, r, req, http.StatusConflict,
			ErrPreviewRequired.Error(), "none")
		return
	}
	if s.Provision == nil {
		s.rejectApplyOperation(w, r, req, http.StatusServiceUnavailable,
			"provisioning is not available", "none")
		return
	}

	// A shutdown wakes queued work before it can become a fresh fleet write. The
	// queued receipt already exists, so close it durably as a definite no-write.
	if !s.siteMu.LockContext(r.Context(), s.requests.stopping) {
		s.rejectApplyOperation(w, r, req, http.StatusServiceUnavailable,
			shutdownNoWrite, "none")
		return
	}
	defer s.siteMu.Unlock()

	persistCtx, cancel := detachedApplyStoreContext(r.Context())
	err = s.Store.MarkApplyOperationRunning(persistCtx, req.OperationID, s.now().Unix())
	cancel()
	if err != nil {
		s.rejectApplyOperation(w, r, req, http.StatusInternalServerError,
			"could not mark the apply operation running; nothing was written", "none")
		return
	}

	// A disconnected browser must not cancel work whose durable ID it can poll.
	// Daemon.TrackApply supplies the bounded fleet deadline and shutdown drain.
	res, applyErr := s.Provision.ApplySite(context.WithoutCancel(r.Context()), req)
	if applyErr != nil {
		status := http.StatusBadRequest
		if errors.Is(applyErr, ErrPreviewRequired) || errors.Is(applyErr, ErrPreviewStale) {
			status = http.StatusConflict
		}
		s.rejectApplyOperation(w, r, req, status, applyErr.Error(), "possible")
		return
	}

	res = safeApplyResult(res, req.OperationID, req.PreviewToken)
	state := store.ApplyOperationCompleted
	errText := ""
	if res.Aborted {
		state = store.ApplyOperationFailed
		errText = "apply stopped before the fleet completed"
		if res.AbortedAfter != "" {
			errText = "apply stopped after " + res.AbortedAfter
		}
	}
	resultWriteState := applyResultWriteState(res)
	writeState := s.applyOperationWriteState(r.Context(), req.OperationID,
		resultWriteState, "possible")
	blob, err := json.Marshal(res)
	if err != nil {
		s.interruptApplyOperation(r.Context(), req.OperationID,
			"apply finished but its result could not be recorded")
		writeApplyOperationErr(w, http.StatusInternalServerError, req.OperationID,
			"apply finished but its result could not be recorded", "possible")
		return
	}
	persistCtx, cancel = detachedApplyStoreContext(r.Context())
	err = s.Store.FinishApplyOperation(persistCtx, req.OperationID, state,
		s.now().Unix(), blob, errText, writeState, http.StatusOK)
	cancel()
	if err != nil {
		s.interruptApplyOperation(r.Context(), req.OperationID,
			"apply finished but its durable terminal status could not be saved")
		writeApplyOperationErr(w, http.StatusInternalServerError, req.OperationID,
			"apply finished but its durable status could not be saved; recover this operation before retrying",
			"possible")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) rejectApplyOperation(w http.ResponseWriter, r *http.Request,
	req ApplyRequest, status int, message, readErrorWriteState string) {
	message = redactApplyToken(message, req.PreviewToken)
	writeState := s.applyOperationWriteState(r.Context(), req.OperationID,
		"none", readErrorWriteState)
	if writeState == "possible" {
		message = strings.ReplaceAll(message, "; nothing was written", "")
		message = strings.ReplaceAll(message, "nothing was written",
			"the device write outcome may be partial")
	}
	persistCtx, cancel := detachedApplyStoreContext(r.Context())
	err := s.Store.FinishApplyOperation(persistCtx, req.OperationID,
		store.ApplyOperationFailed, s.now().Unix(), nil, message, writeState, status)
	cancel()
	if err != nil {
		message := "apply could not finish recording its durable status"
		if writeState == "none" {
			message = "apply was rejected before any device write, but its durable status could not be saved"
		}
		s.interruptApplyOperation(r.Context(), req.OperationID, message)
		writeApplyOperationErr(w, http.StatusInternalServerError, req.OperationID,
			message, writeState)
		return
	}
	writeApplyOperationErr(w, status, req.OperationID, message, writeState)
}

func (s *Server) interruptApplyOperation(parent context.Context, operationID,
	reason string) {
	persistCtx, cancel := detachedApplyStoreContext(parent)
	defer cancel()
	if err := s.Store.InterruptApplyOperation(persistCtx, operationID,
		s.now().Unix(), reason); err != nil && s.Log != nil {
		s.Log.Error("could not mark Apply operation unknown",
			"operation_id", operationID, "err", err)
	}
}

func (s *Server) handleApplyOperationStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("operation_id")
	if !validOperationID(id) {
		writeErr(w, http.StatusBadRequest, "invalid apply operation ID")
		return
	}
	op, err := s.Store.ApplyOperation(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "apply operation not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "could not read apply operation")
		return
	}
	status, err := applyOperationStatus(op)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not decode apply operation result")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) writeApplyOperationReplay(w http.ResponseWriter, op *store.ApplyOperation) {
	status, err := applyOperationStatus(op)
	if err != nil {
		writeApplyOperationErr(w, http.StatusInternalServerError, op.OperationID,
			"could not decode the recorded apply result", "possible")
		return
	}
	if op.State == store.ApplyOperationQueued || op.State == store.ApplyOperationRunning {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, status)
		return
	}
	if len(op.ResultJSON) > 0 && op.HTTPStatus == http.StatusOK {
		writeJSON(w, http.StatusOK, status.Result)
		return
	}
	httpStatus := op.HTTPStatus
	if httpStatus == 0 {
		httpStatus = http.StatusServiceUnavailable
	}
	writeApplyOperationErr(w, httpStatus, op.OperationID, op.Error, op.WriteState)
}

func applyOperationStatus(op *store.ApplyOperation) (*ApplyOperationStatus, error) {
	out := &ApplyOperationStatus{
		OperationID: op.OperationID,
		Actor:       op.ActorUsername,
		State:       string(op.State),
		CreatedAt:   op.CreatedAt,
		StartedAt:   op.StartedAt,
		FinishedAt:  op.FinishedAt,
		Error:       op.Error,
		WriteState:  op.WriteState,
		Devices:     make([]ApplyOperationDeviceStatus, 0, len(op.Devices)),
	}
	for _, device := range op.Devices {
		out.Devices = append(out.Devices, ApplyOperationDeviceStatus{
			Ordinal: device.Ordinal, DeviceID: device.DeviceID,
			DeviceMAC: device.DeviceMAC, DeviceName: device.DeviceName,
			State: string(device.State), WriteState: device.WriteState,
			RouterOutcome: device.RouterOutcome, Outcome: device.Outcome,
			Changes: device.Changes, Reason: device.Reason,
			StartedAt: device.StartedAt, FinishedAt: device.FinishedAt,
		})
	}
	if len(op.ResultJSON) > 0 {
		var result ApplyResult
		if err := json.Unmarshal(op.ResultJSON, &result); err != nil {
			return nil, err
		}
		out.Result = &result
	}
	return out, nil
}

func (s *Server) applyRequestFingerprint(req ApplyRequest, actorAdminID int64) (string, error) {
	deviceIDs := append([]int64(nil), req.DeviceIDs...)
	sort.Slice(deviceIDs, func(i, j int) bool { return deviceIDs[i] < deviceIDs[j] })
	if len(deviceIDs) > 1 {
		out := deviceIDs[:1]
		for _, id := range deviceIDs[1:] {
			if id != out[len(out)-1] {
				out = append(out, id)
			}
		}
		deviceIDs = out
	}
	binding := struct {
		ActorAdminID            int64   `json:"actor_admin_id"`
		PreviewToken            string  `json:"preview_token"`
		DeviceIDs               []int64 `json:"device_ids,omitempty"`
		AcknowledgeTraversal    bool    `json:"acknowledge_traversal,omitempty"`
		AcknowledgeDriverRisk   bool    `json:"acknowledge_driver_risk,omitempty"`
		AcknowledgeCautions     bool    `json:"acknowledge_cautions,omitempty"`
		AcknowledgePartialFleet bool    `json:"acknowledge_partial_fleet,omitempty"`
	}{actorAdminID, req.PreviewToken, deviceIDs, req.AcknowledgeTraversal,
		req.AcknowledgeDriverRisk, req.AcknowledgeCautions,
		req.AcknowledgePartialFleet}
	blob, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	if s.Keys == nil {
		return "", errors.New("apply request binding key is unavailable")
	}
	digest, err := s.Keys.HMACSHA256("apply-request/v1", blob)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest), nil
}

func safeApplyResult(in *ApplyResult, operationID, previewToken string) *ApplyResult {
	if in == nil {
		in = &ApplyResult{}
	}
	out := *in
	out.OperationID = operationID
	out.AbortedAfter = redactApplyToken(out.AbortedAfter, previewToken)
	out.Devices = append([]DeviceApply(nil), in.Devices...)
	for i := range out.Devices {
		out.Devices[i].Name = redactApplyToken(out.Devices[i].Name, previewToken)
		out.Devices[i].Reason = redactApplyToken(out.Devices[i].Reason, previewToken)
	}
	if out.Devices == nil {
		out.Devices = []DeviceApply{}
	}
	return &out
}

func applyResultWriteState(result *ApplyResult) string {
	for _, device := range result.Devices {
		if device.Changes > 0 {
			return "possible"
		}
	}
	return "none"
}

func (s *Server) applyOperationWriteState(ctx context.Context, operationID,
	fallback, readErrorFallback string) string {
	persistCtx, cancel := detachedApplyStoreContext(ctx)
	defer cancel()
	op, err := s.Store.ApplyOperation(persistCtx, operationID)
	if err != nil {
		return readErrorFallback
	}
	if op.WriteState == "possible" {
		return "possible"
	}
	return fallback
}

func redactApplyToken(message, token string) string {
	if token == "" {
		return message
	}
	return strings.ReplaceAll(message, token, "[preview token redacted]")
}

func detachedApplyStoreContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), applyOperationPersistTimeout)
}

func validOperationID(id string) bool {
	if len(id) != 36 || id != strings.ToLower(id) {
		return false
	}
	for i := range id {
		switch i {
		case 8, 13, 18, 23:
			if id[i] != '-' {
				return false
			}
		default:
			c := id[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}

func writeApplyOperationErr(w http.ResponseWriter, status int, operationID, message,
	writeState string) {
	body := map[string]any{"error": message, "write_state": writeState}
	if operationID != "" {
		body["operation_id"] = operationID
	}
	writeJSON(w, status, body)
}
