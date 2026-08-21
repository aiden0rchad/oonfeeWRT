package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func applyOperationBody(id, token string) map[string]any {
	return map[string]any{"operation_id": id, "preview_token": token}
}

func applyCallCount(p *recordingProvisioner) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.applyCalls
}

type applyOperationProvisioner struct {
	apply func(context.Context, ApplyRequest) (*ApplyResult, error)
}

func (p applyOperationProvisioner) Preview(context.Context) (*PreviewResult, error) {
	return &PreviewResult{}, nil
}

func (p applyOperationProvisioner) ApplySite(ctx context.Context,
	req ApplyRequest) (*ApplyResult, error) {
	return p.apply(ctx, req)
}

func TestApplyOperationIsIdempotentAndRecoverable(t *testing.T) {
	h := newHarness(t)
	h.setup()
	p := &recordingProvisioner{applyResult: &ApplyResult{Devices: []DeviceApply{{
		DeviceID: 7, Name: "ap", Outcome: "applied", Changes: 2,
	}}}}
	h.srv.Provision = p
	body := applyOperationBody(testApplyOperationID, "pv-current")

	first := h.do(http.MethodPost, "/api/v1/site/apply", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	if got := h.json(first)["operation_id"]; got != testApplyOperationID {
		t.Fatalf("success operation_id = %v", got)
	}

	replay := h.do(http.MethodPost, "/api/v1/site/apply", body)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d: %s", replay.Code, replay.Body.String())
	}
	if applyCallCount(p) != 1 {
		t.Fatalf("identical replay invoked ApplySite %d times", applyCallCount(p))
	}

	mismatch := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-different"))
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d: %s", mismatch.Code, mismatch.Body.String())
	}
	if got := h.json(mismatch)["write_state"]; got != "possible" {
		t.Fatalf("mismatch write_state = %v", got)
	}
	if applyCallCount(p) != 1 {
		t.Fatalf("mismatched replay invoked ApplySite %d times", applyCallCount(p))
	}

	status := h.do(http.MethodGet,
		"/api/v1/site/apply/"+testApplyOperationID, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", status.Code, status.Body.String())
	}
	got := h.json(status)
	if got["state"] != "completed" || got["actor"] != "admin" ||
		got["write_state"] != "possible" {
		t.Fatalf("durable operation = %#v", got)
	}
	result := got["result"].(map[string]any)
	if result["operation_id"] != testApplyOperationID ||
		len(result["devices"].([]any)) != 1 {
		t.Fatalf("durable result = %#v", result)
	}
}

func TestClosedAdmissionRejectsNewApplyWithoutCreatingAReceipt(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Provision = &recordingProvisioner{}
	h.srv.CloseAdmission()

	res := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-current"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed admission = %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "nothing was written") {
		t.Fatalf("closed admission response is ambiguous: %s", res.Body.String())
	}
	if _, err := h.db.ApplyOperation(context.Background(), testApplyOperationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("closed admission created an operation: %v", err)
	}
}

func TestClosedAdmissionWakesQueuedSiteMutationWithoutWriting(t *testing.T) {
	h := newHarness(t)
	h.setup()
	before, err := h.db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.srv.siteMu.Lock()
	locked := true
	defer func() {
		if locked {
			h.srv.siteMu.Unlock()
		}
	}()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- h.do(http.MethodPost, "/api/v1/site/name",
			map[string]any{"name": "must-not-land"})
	}()
	deadline := time.Now().Add(time.Second)
	for h.srv.ActiveRequests() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.srv.ActiveRequests() == 0 {
		t.Fatal("site mutation was not admitted")
	}
	h.srv.CloseAdmission()
	select {
	case res := <-done:
		if res.Code != http.StatusServiceUnavailable ||
			!strings.Contains(res.Body.String(), "nothing was written") {
			t.Fatalf("queued site mutation = %d: %s", res.Code, res.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("queued site mutation did not wake when admission closed")
	}
	h.srv.siteMu.Unlock()
	locked = false
	after, err := h.db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name {
		t.Fatalf("queued mutation changed site name from %q to %q", before.Name, after.Name)
	}
}

func TestApplyRequestBindingIsKeyedAndCanonical(t *testing.T) {
	first := newHarness(t)
	second := newHarness(t)
	req := ApplyRequest{
		PreviewToken: "pv1_placeholder", DeviceIDs: []int64{7, 3, 7},
		AcknowledgeTraversal: true,
	}
	digest, err := first.srv.applyRequestFingerprint(req, 42)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := first.srv.applyRequestFingerprint(ApplyRequest{
		PreviewToken: "pv1_placeholder", DeviceIDs: []int64{3, 7},
		AcknowledgeTraversal: true,
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if digest != canonical {
		t.Fatal("device-id order or duplicates changed the semantic request binding")
	}
	otherKey, err := second.srv.applyRequestFingerprint(req, 42)
	if err != nil {
		t.Fatal(err)
	}
	if digest == otherKey {
		t.Fatal("request binding was not keyed to this controller")
	}
	otherActor, err := first.srv.applyRequestFingerprint(req, 43)
	if err != nil {
		t.Fatal(err)
	}
	if digest == otherActor {
		t.Fatal("authenticated actor was absent from the request binding")
	}
}

func TestApplyOperationDuplicateWhileRunningDoesNotQueueAnotherWrite(t *testing.T) {
	h := newHarness(t)
	h.setup()
	release := make(chan struct{})
	p := &recordingProvisioner{
		applyStarted: make(chan struct{}), applyRelease: release,
	}
	h.srv.Provision = p
	body := applyOperationBody(testApplyOperationID, "pv-current")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.do(http.MethodPost, "/api/v1/site/apply", body) }()

	select {
	case <-p.applyStarted:
	case <-time.After(time.Second):
		t.Fatal("ApplySite did not start")
	}
	replay := h.do(http.MethodPost, "/api/v1/site/apply", body)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("running replay = %d: %s", replay.Code, replay.Body.String())
	}
	if got := h.json(replay)["state"]; got != "running" {
		t.Fatalf("running replay state = %v", got)
	}
	mismatch := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-other"))
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("running mismatch = %d: %s", mismatch.Code, mismatch.Body.String())
	}
	if applyCallCount(p) != 1 {
		t.Fatalf("running duplicate invoked ApplySite %d times", applyCallCount(p))
	}
	close(release)
	if res := <-done; res.Code != http.StatusOK {
		t.Fatalf("original = %d: %s", res.Code, res.Body.String())
	}
}

func TestApplyOperationRunningReplayIncludesDurableDeviceBoundary(t *testing.T) {
	h := newHarness(t)
	h.setup()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	h.srv.Provision = applyOperationProvisioner{apply: func(ctx context.Context,
		req ApplyRequest) (*ApplyResult, error) {
		calls.Add(1)
		if err := h.db.InitializeApplyOperationDevices(ctx, req.OperationID,
			[]store.ApplyOperationDevice{{
				DeviceID: 7, DeviceMAC: "aa:bb:cc:dd:ee:ff",
				DeviceName: "gateway", Changes: 1,
			}}); err != nil {
			return nil, err
		}
		if err := h.db.MarkApplyOperationDeviceApplying(ctx, req.OperationID, 0, 2); err != nil {
			return nil, err
		}
		close(started)
		<-release
		if err := h.db.FinishApplyOperationDevice(ctx, req.OperationID, 0,
			store.ApplyOperationDeviceCompleted, 3, "applied", "applied", 1, "",
			store.ApplyWriteStatePossible); err != nil {
			return nil, err
		}
		return &ApplyResult{Devices: []DeviceApply{{
			DeviceID: 7, Name: "gateway", RouterOutcome: "applied",
			Outcome: "applied", Changes: 1,
		}}}, nil
	}}
	body := applyOperationBody(testApplyOperationID, "pv-current")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.do(http.MethodPost, "/api/v1/site/apply", body) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ApplySite did not cross its durable device boundary")
	}

	replay := h.do(http.MethodPost, "/api/v1/site/apply", body)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("running replay = %d: %s", replay.Code, replay.Body.String())
	}
	got := h.json(replay)
	devices := got["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("running replay devices = %#v", devices)
	}
	device := devices[0].(map[string]any)
	if device["state"] != "applying" || device["write_state"] != "possible" {
		t.Fatalf("running replay device = %#v", device)
	}
	if calls.Load() != 1 {
		t.Fatalf("idempotent replay invoked ApplySite %d times", calls.Load())
	}
	close(release)
	if res := <-done; res.Code != http.StatusOK {
		t.Fatalf("original = %d: %s", res.Code, res.Body.String())
	}
}

func TestApplyOperationPersistsNoWritePreflightAndPartialResult(t *testing.T) {
	t.Run("preflight", func(t *testing.T) {
		h := newHarness(t)
		h.setup()
		p := &recordingProvisioner{applyErr: ErrPreviewStale}
		h.srv.Provision = p
		body := applyOperationBody(testApplyOperationID, "pv-stale")

		res := h.do(http.MethodPost, "/api/v1/site/apply", body)
		if res.Code != http.StatusConflict {
			t.Fatalf("preflight = %d: %s", res.Code, res.Body.String())
		}
		status := h.json(h.do(http.MethodGet,
			"/api/v1/site/apply/"+testApplyOperationID, nil))
		if status["state"] != "failed" || status["write_state"] != "none" ||
			status["error"] == "" || status["result"] != nil {
			t.Fatalf("preflight operation = %#v", status)
		}
		replay := h.do(http.MethodPost, "/api/v1/site/apply", body)
		if replay.Code != http.StatusConflict || applyCallCount(p) != 1 {
			t.Fatalf("preflight replay = %d calls=%d: %s",
				replay.Code, applyCallCount(p), replay.Body.String())
		}
	})

	t.Run("partial", func(t *testing.T) {
		h := newHarness(t)
		h.setup()
		p := &recordingProvisioner{applyResult: &ApplyResult{
			Devices: []DeviceApply{
				{DeviceID: 1, Name: "ap", Outcome: "applied", Changes: 1},
				{DeviceID: 2, Name: "gateway", Outcome: "reverted", Changes: 1,
					Reason: "health check failed"},
			},
			Aborted: true, AbortedAfter: "gateway",
		}}
		h.srv.Provision = p

		res := h.do(http.MethodPost, "/api/v1/site/apply",
			applyOperationBody(testApplyOperationID, "pv-current"))
		if res.Code != http.StatusOK {
			t.Fatalf("partial = %d: %s", res.Code, res.Body.String())
		}
		status := h.json(h.do(http.MethodGet,
			"/api/v1/site/apply/"+testApplyOperationID, nil))
		if status["state"] != "failed" || status["write_state"] != "possible" {
			t.Fatalf("partial operation = %#v", status)
		}
		result := status["result"].(map[string]any)
		if result["aborted"] != true || len(result["devices"].([]any)) != 2 {
			t.Fatalf("partial result = %#v", result)
		}
	})
}

func TestApplyOperationSurvivesLostRequestContext(t *testing.T) {
	h := newHarness(t)
	h.setup()
	release := make(chan struct{})
	p := &recordingProvisioner{
		applyStarted:       make(chan struct{}),
		applyRelease:       release,
		requireLiveContext: true,
		applyResult: &ApplyResult{Devices: []DeviceApply{{
			DeviceID: 1, Name: "ap", Outcome: "applied", Changes: 1,
		}},
		},
	}
	h.srv.Provision = p
	blob, err := json.Marshal(applyOperationBody(testApplyOperationID, "pv-current"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/site/apply",
		bytes.NewReader(blob)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, h.csrf)
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.mux.ServeHTTP(w, req)
		close(done)
	}()
	select {
	case <-p.applyStarted:
	case <-time.After(time.Second):
		t.Fatal("ApplySite did not start")
	}
	cancel() // stand in for the browser losing the POST response
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detached Apply did not finish")
	}

	status := h.json(h.do(http.MethodGet,
		"/api/v1/site/apply/"+testApplyOperationID, nil))
	if status["state"] != "completed" || status["result"] == nil {
		t.Fatalf("recovered operation = %#v", status)
	}
}

func TestApplyOperationNeverPersistsOrReturnsPreviewToken(t *testing.T) {
	h := newHarness(t)
	h.setup()
	token := "pv1_super-secret-token-that-must-not-be-stored"
	h.srv.Provision = &recordingProvisioner{
		applyErr: fmt.Errorf("remote repeated %s", token),
	}

	res := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, token))
	if strings.Contains(res.Body.String(), token) {
		t.Fatalf("response leaked preview token: %s", res.Body.String())
	}
	var requestHash string
	var result, errText sql.NullString
	if err := h.db.SQL().QueryRow(`SELECT request_hash, result_json, error
		FROM apply_operations WHERE operation_id=?`, testApplyOperationID).
		Scan(&requestHash, &result, &errText); err != nil {
		t.Fatal(err)
	}
	stored := requestHash + result.String + errText.String
	if strings.Contains(stored, token) {
		t.Fatalf("database leaked preview token: %q", stored)
	}
	if len(requestHash) != 64 {
		t.Fatalf("request binding length = %d, want keyed SHA-256 hex", len(requestHash))
	}
}

func TestApplyOperationTerminalPersistenceFailureBecomesRecoverableUnknown(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Provision = &recordingProvisioner{}
	if _, err := h.db.SQL().Exec(`
CREATE TRIGGER fail_apply_terminal
BEFORE UPDATE OF state ON apply_operations
WHEN NEW.state IN ('completed', 'failed')
BEGIN
  SELECT RAISE(ABORT, 'test terminal receipt failure');
END`); err != nil {
		t.Fatal(err)
	}

	res := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-current"))
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("terminal persistence response = %d: %s", res.Code, res.Body.String())
	}
	if got := h.json(res)["write_state"]; got != "possible" {
		t.Fatalf("terminal persistence write_state = %v", got)
	}
	status := h.json(h.do(http.MethodGet,
		"/api/v1/site/apply/"+testApplyOperationID, nil))
	if status["state"] != "unknown" || status["write_state"] != "none" ||
		!strings.Contains(fmt.Sprint(status["error"]), "terminal status") {
		t.Fatalf("recoverable terminal persistence status = %#v", status)
	}
}

func TestApplyOperationTerminalPersistenceFailurePreservesCrossedBoundary(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Provision = applyOperationProvisioner{apply: func(ctx context.Context,
		req ApplyRequest) (*ApplyResult, error) {
		if err := h.db.InitializeApplyOperationDevices(ctx, req.OperationID,
			[]store.ApplyOperationDevice{{
				DeviceID: 7, DeviceMAC: "aa:bb:cc:dd:ee:ff",
				DeviceName: "gateway", Changes: 1,
			}}); err != nil {
			return nil, err
		}
		if err := h.db.MarkApplyOperationDeviceApplying(ctx, req.OperationID, 0, 2); err != nil {
			return nil, err
		}
		if err := h.db.FinishApplyOperationDevice(ctx, req.OperationID, 0,
			store.ApplyOperationDeviceCompleted, 3, "applied", "applied", 1, "",
			store.ApplyWriteStatePossible); err != nil {
			return nil, err
		}
		return &ApplyResult{Devices: []DeviceApply{{
			DeviceID: 7, Name: "gateway", RouterOutcome: "applied",
			Outcome: "applied", Changes: 1,
		}}}, nil
	}}
	if _, err := h.db.SQL().Exec(`
CREATE TRIGGER fail_apply_terminal_after_boundary
BEFORE UPDATE OF state ON apply_operations
WHEN NEW.state IN ('completed', 'failed')
BEGIN
  SELECT RAISE(ABORT, 'test terminal receipt failure after boundary');
END`); err != nil {
		t.Fatal(err)
	}

	res := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-current"))
	if res.Code != http.StatusInternalServerError ||
		h.json(res)["write_state"] != "possible" {
		t.Fatalf("terminal persistence response = %d: %s", res.Code, res.Body.String())
	}
	status := h.json(h.do(http.MethodGet,
		"/api/v1/site/apply/"+testApplyOperationID, nil))
	if status["state"] != "unknown" || status["write_state"] != "possible" {
		t.Fatalf("recoverable parent = %#v", status)
	}
	devices := status["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("recoverable devices = %#v", devices)
	}
	device := devices[0].(map[string]any)
	if device["state"] != "completed" || device["write_state"] != "possible" ||
		device["router_outcome"] != "applied" || device["outcome"] != "applied" {
		t.Fatalf("recoverable device = %#v", device)
	}
}

func TestApplyOperationStatusAuthAndCSRF(t *testing.T) {
	h := newHarness(t)
	unauth := h.do(http.MethodGet,
		"/api/v1/site/apply/"+testApplyOperationID, nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauth.Code)
	}
	h.setup()
	h.srv.Provision = &recordingProvisioner{}

	csrf := h.csrf
	h.csrf = ""
	forbidden := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-current"))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d: %s", forbidden.Code, forbidden.Body.String())
	}
	h.csrf = csrf
	if res := h.do(http.MethodPost, "/api/v1/site/apply",
		applyOperationBody(testApplyOperationID, "pv-current")); res.Code != http.StatusOK {
		t.Fatalf("authenticated POST = %d: %s", res.Code, res.Body.String())
	}
	h.csrf = "" // GET status is authenticated but is not a mutation.
	status := h.do(http.MethodGet,
		"/api/v1/site/apply/"+testApplyOperationID, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("GET status without CSRF = %d: %s", status.Code, status.Body.String())
	}
}

func TestApplyOperationStatusIncludesDurableDeviceOutcome(t *testing.T) {
	h := newHarness(t)
	h.setup()
	const operationID = "11962c09-7d62-4cd7-a1c2-450eba830893"
	ctx := context.Background()
	if _, created, err := h.db.BeginApplyOperation(ctx, operationID, "keyed-hash",
		42, "operator", 1); err != nil || !created {
		t.Fatalf("begin operation: created=%v err=%v", created, err)
	}
	if err := h.db.MarkApplyOperationRunning(ctx, operationID, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.db.InitializeApplyOperationDevices(ctx, operationID,
		[]store.ApplyOperationDevice{{
			DeviceID: 7, DeviceMAC: "aa:bb:cc:dd:ee:ff",
			DeviceName: "gateway", Changes: 3,
		}}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.MarkApplyOperationDeviceApplying(ctx, operationID, 0, 3); err != nil {
		t.Fatal(err)
	}
	if err := h.db.FinishApplyOperationDevice(ctx, operationID, 0,
		store.ApplyOperationDeviceFailed, 4, "applied", "error", 3,
		"ownership receipt failed", store.ApplyWriteStatePossible); err != nil {
		t.Fatal(err)
	}

	res := h.do(http.MethodGet, "/api/v1/site/apply/"+operationID, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body.String())
	}
	got := h.json(res)
	devices := got["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("devices = %#v", devices)
	}
	device := devices[0].(map[string]any)
	if device["device_name"] != "gateway" || device["state"] != "failed" ||
		device["write_state"] != "possible" || device["router_outcome"] != "applied" ||
		device["outcome"] != "error" {
		t.Fatalf("durable device status = %#v", device)
	}
}

func TestApplyOperationRejectsMissingOrInvalidIDBeforeProvisioning(t *testing.T) {
	h := newHarness(t)
	h.setup()
	p := &recordingProvisioner{}
	h.srv.Provision = p
	for _, body := range []map[string]any{
		{"preview_token": "pv"},
		{"operation_id": "../../events", "preview_token": "pv"},
		{"operation_id": strings.ToUpper(testApplyOperationID), "preview_token": "pv"},
	} {
		res := h.do(http.MethodPost, "/api/v1/site/apply", body)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("body %#v status = %d: %s", body, res.Code, res.Body.String())
		}
	}
	if applyCallCount(p) != 0 {
		t.Fatalf("invalid operation ID invoked ApplySite %d times", applyCallCount(p))
	}
}
