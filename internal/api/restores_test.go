package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/controllerrestore"
	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/restoreswap"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const restoreTestPassphrase = "placeholder-restore-export-passphrase"

func newRestoreHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.RestoresDir = filepath.Join(t.TempDir(), "restores")
	h.srv.RestoreOwnerInstanceID = strings.Repeat("1", 32)
	h.srv.RequestRestart = func() {}
	if err := h.srv.InitRestores(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !h.srv.CloseJobs(10 * time.Second) {
			t.Error("restore jobs did not close")
		}
	})
	h.setup()
	return h
}

type fakeRestorePrepared struct {
	preview        controllerrestore.Preview
	pair           controllerrestore.PreparedPair
	beforeTransfer error
	afterTransfer  error
	cleanupErr     error
	cleanupCalls   atomic.Int32
	transferCalls  atomic.Int32
	transferred    atomic.Bool
}

func (p *fakeRestorePrepared) Preview() controllerrestore.Preview { return p.preview }

func (p *fakeRestorePrepared) Cleanup() error {
	p.cleanupCalls.Add(1)
	if p.transferred.Load() {
		return controllerrestore.ErrPreparedTransferred
	}
	return p.cleanupErr
}

func (p *fakeRestorePrepared) Transfer(ctx context.Context,
	adopt func(controllerrestore.PreparedPair) error) (bool, error) {
	p.transferCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.beforeTransfer != nil {
		return false, p.beforeTransfer
	}
	if err := adopt(p.pair); err != nil {
		return false, err
	}
	p.transferred.Store(true)
	return true, p.afterTransfer
}

func completedRestorePreview(t *testing.T, h *harness) (restoreUploadDTO, restorePreviewDTO) {
	t.Helper()
	h.srv.restoreInspect = func(context.Context, string, string, []byte) (controllerrestore.Preview, error) {
		return successfulRestorePreview(), nil
	}
	upload := uploadRestore(t, h, []byte("encrypted-portable-backup-placeholder"))
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	preview := waitRestorePreview(t, h, started.ID, "completed")
	h.srv.restores.wg.Wait()
	return upload, preview
}

func restoreConfirmationRequest(planID string) map[string]any {
	return map[string]any{
		"plan_id": planID, "export_passphrase": restoreTestPassphrase,
		"destination_runtime_passphrase": "destination-runtime-secret-value",
		"typed_confirmation":             restoreTypedConfirmation,
		"acknowledge_restart":            true, "acknowledge_session_revocation": true,
		"acknowledge_router_writes_suppressed":  true,
		"acknowledge_no_automatic_router_apply": true,
	}
}

func restoreRawDo(h *harness, data []byte, declared int64, contentType, remote string,
	tlsState bool) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/restores/uploads", bytes.NewReader(data))
	req.Host, req.RemoteAddr, req.ContentLength = testSetupHost, remote, declared
	req.Header.Set("Content-Type", contentType)
	if tlsState {
		req.TLS = &tls.ConnectionState{}
	}
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	req.Header.Set(csrfHeader, h.csrf)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

func uploadRestore(t *testing.T, h *harness, data []byte) restoreUploadDTO {
	t.Helper()
	w := restoreRawDo(h, data, int64(len(data)), restoreMediaType, "127.0.0.1:12345", false)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", w.Code, w.Body.String())
	}
	var body restoreUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Upload
}

func startRestorePreview(t *testing.T, h *harness, uploadID, passphrase string) restorePreviewDTO {
	t.Helper()
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/previews", map[string]any{
		"upload_id": uploadID, "export_passphrase": passphrase,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("preview status=%d body=%s", w.Code, w.Body.String())
	}
	var body restorePreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Preview
}

func waitRestorePreview(t *testing.T, h *harness, id string, states ...string) restorePreviewDTO {
	t.Helper()
	wanted := map[string]bool{}
	for _, state := range states {
		wanted[state] = true
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := loopbackBackupDo(h, http.MethodGet, "/api/v1/restores/previews/"+id, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("preview read status=%d body=%s", w.Code, w.Body.String())
		}
		var body restorePreviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if wanted[body.Preview.State] {
			return body.Preview
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("preview %s did not reach %v", id, states)
	return restorePreviewDTO{}
}

func successfulRestorePreview() controllerrestore.Preview {
	created := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	return controllerrestore.Preview{
		Manifest: portablebackup.Manifest{
			Format: "oonfeewrt-portable-backup", Version: 1, CreatedAt: created,
			ControllerVersion: "v0.1.0-test", SchemaVersion: store.CurrentSchemaVersion(),
			Database: portablebackup.Member{Name: "controller.db", Size: 4096,
				SHA256: strings.Repeat("a", 64)},
			PortableKey: portablebackup.Member{Name: "portable-key.json", Size: 512,
				SHA256: strings.Repeat("b", 64)},
		},
		SourceSchema: store.CurrentSchemaVersion(), TargetSchema: store.CurrentSchemaVersion(),
		Counts: recovery.Counts{Schema: store.CurrentSchemaVersion(), Devices: 2,
			Credentials: 2, OwnedSections: 8, WLANs: 1, Meshes: 0},
	}
}

func TestRestoreInformationalAuditUsesSourceIdentifier(t *testing.T) {
	var events []store.Event
	s := &Server{restoreAuditWrite: func(_ context.Context, event store.Event) error {
		events = append(events, event)
		return nil
	}}
	for _, tc := range []struct {
		event string
		key   string
		id    string
	}{
		{event: "restore.upload_completed", key: "upload_id", id: strings.Repeat("1", 32)},
		{event: "restore.preview_completed", key: "preview_id", id: strings.Repeat("2", 32)},
	} {
		s.auditRestore(tc.event, "info", 1, "admin", tc.key, tc.id, "completed")
		detail, ok := events[len(events)-1].Detail.(map[string]any)
		if !ok || detail[tc.key] != tc.id {
			t.Fatalf("%s detail=%+v", tc.event, events[len(events)-1].Detail)
		}
		if _, exists := detail["restore_id"]; exists {
			t.Fatalf("%s mislabeled source as restore_id: %+v", tc.event, detail)
		}
	}
}

func TestRestoreUploadAndPreviewContractHasNoLiveOrRouterEffects(t *testing.T) {
	h := newRestoreHarness(t)
	h.srv.restoreInspect = func(ctx context.Context, path, scratch string,
		passphrase []byte) (controllerrestore.Preview, error) {
		if ctx.Err() != nil || filepath.Dir(path) != filepath.Join(h.srv.RestoresDir, "uploads") ||
			scratch != filepath.Join(h.srv.RestoresDir, "scratch") || string(passphrase) != restoreTestPassphrase {
			t.Fatalf("unsafe inspect inputs path=%q scratch=%q", path, scratch)
		}
		return successfulRestorePreview(), nil
	}
	w := loopbackBackupDo(h, http.MethodGet, "/api/v1/restores", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("descriptor status=%d body=%s", w.Code, w.Body.String())
	}
	var descriptor restoresResponse
	if err := json.Unmarshal(w.Body.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.Descriptor.ConfirmationContract != restoreConfirmationContract ||
		descriptor.Descriptor.TypedConfirmation != restoreTypedConfirmation ||
		descriptor.Limits.MaxUploadBytes != controllerPortableArtifactMaxBytes ||
		descriptor.Limits.MaxDatabaseBytes != controllerPortableDatabaseMaxBytes ||
		descriptor.Limits.MinExportPassphraseCharacters != minExportPassphraseRunes ||
		descriptor.Limits.ConfirmationTimeoutSeconds != int64(restoreConfirmationTimeout/time.Second) ||
		descriptor.Disclosure.RouterManagementCalls || descriptor.Disclosure.RouterChanges ||
		descriptor.Disclosure.LiveControllerChanges || descriptor.Disclosure.AutomaticRouterApply ||
		descriptor.Uploads == nil || descriptor.Previews == nil {
		t.Fatalf("unexpected restore descriptor: %+v", descriptor)
	}

	data := []byte("encrypted-portable-backup-placeholder")
	upload := uploadRestore(t, h, data)
	digest := sha256.Sum256(data)
	if upload.SizeBytes != int64(len(data)) || upload.SHA256 != hex.EncodeToString(digest[:]) ||
		!validBackupID(upload.ID) {
		t.Fatalf("upload=%+v", upload)
	}
	path := filepath.Join(h.srv.RestoresDir, "uploads", upload.ID+".oowrtbak")
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored upload info=%v err=%v", info, err)
	}

	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	completed := waitRestorePreview(t, h, started.ID, "completed")
	if completed.PlanID == "" || !strings.HasPrefix(completed.PlanID, restoreConfirmationContract+".") ||
		completed.Manifest == nil || completed.Manifest.DatabaseSizeBytes != 4096 ||
		completed.SourceSchema == nil || completed.TargetSchema == nil || completed.Counts == nil ||
		completed.Counts.Devices != 2 || completed.Error != "" || completed.ErrorCode != "" {
		t.Fatalf("completed preview=%+v", completed)
	}
	encoded, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		h.srv.RestoresDir, restoreTestPassphrase, "portable-key.json", "controller.db", strings.Repeat("a", 64),
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("preview DTO leaked %q: %s", forbidden, encoded)
		}
	}
	if len(h.fleet.focused) != 0 {
		t.Fatalf("restore preview touched fleet: %+v", h.fleet.focused)
	}
}

func TestRestoreConfirmationCreatesOnlyDurableIntentThenRespondsBeforeRestart(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	prepared := &fakeRestorePrepared{preview: successfulRestorePreview()}
	var exportSeen, runtimeSeen []byte
	h.srv.restorePrepare = func(ctx context.Context, artifactPath, dataDir string,
		live *secrets.Keeper, exportPassphrase, runtimePassphrase []byte) (restorePrepared, error) {
		if ctx.Err() != nil || filepath.Dir(artifactPath) != filepath.Join(h.srv.RestoresDir, "uploads") ||
			dataDir != filepath.Dir(h.srv.RestoresDir) || live != h.srv.Keys {
			t.Fatalf("unsafe prepare boundary artifact=%q data=%q", artifactPath, dataDir)
		}
		exportSeen, runtimeSeen = exportPassphrase, runtimePassphrase
		return prepared, nil
	}
	intentID := strings.Repeat("2", 32)
	h.srv.restoreCreateIntent = func(ctx context.Context, dataDir string, pair restoreswap.PreparedPair,
		live *secrets.Keeper, exportPassphrase []byte, ownerID string) (restoreswap.IntentResult, error) {
		if ctx.Err() != nil || dataDir != filepath.Dir(h.srv.RestoresDir) || live != h.srv.Keys ||
			string(exportPassphrase) != restoreTestPassphrase || ownerID != h.srv.RestoreOwnerInstanceID {
			t.Fatalf("unsafe intent boundary data=%q owner=%q", dataDir, ownerID)
		}
		return restoreswap.IntentResult{ID: intentID}, nil
	}
	restarted := make(chan bool, 1)
	var response *httptest.ResponseRecorder
	h.srv.RequestRestart = func() {
		restarted <- response != nil && response.Flushed && response.Code == http.StatusAccepted
	}
	body, err := json.Marshal(restoreConfirmationRequest(preview.PlanID))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm", bytes.NewReader(body))
	req.Host, req.RemoteAddr = testSetupHost, "127.0.0.1:12345"
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	req.Header.Set(csrfHeader, h.csrf)
	response = httptest.NewRecorder()
	h.mux.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("confirm status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case ordered := <-restarted:
		if !ordered {
			t.Fatal("restart callback ran before the 202 response was flushed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart callback was not requested")
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &outer); err != nil || len(outer) != 1 || outer["intent"] == nil {
		t.Fatalf("intent response contract=%s err=%v", response.Body.String(), err)
	}
	var intent map[string]any
	if err := json.Unmarshal(outer["intent"], &intent); err != nil || len(intent) != 3 ||
		intent["id"] != intentID || intent["state"] != "accepted" || intent["accepted_at"] == nil {
		t.Fatalf("intent response=%s err=%v", outer["intent"], err)
	}
	for _, forbidden := range []string{restoreTestPassphrase, "destination-runtime-secret-value", h.srv.RestoresDir} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("intent response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	if prepared.cleanupCalls.Load() != 0 || prepared.transferCalls.Load() != 1 || !prepared.transferred.Load() ||
		len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 ||
		!bytes.Equal(exportSeen, make([]byte, len(exportSeen))) ||
		!bytes.Equal(runtimeSeen, make([]byte, len(runtimeSeen))) {
		t.Fatalf("confirmation lifecycle cleanup=%d transfer=%d transferred=%v hashing=%d ops=%v export=%q runtime=%q",
			prepared.cleanupCalls.Load(), prepared.transferCalls.Load(), prepared.transferred.Load(),
			len(h.srv.hashing), h.srv.ActiveOperations(), exportSeen, runtimeSeen)
	}
	if len(h.fleet.focused) != 0 {
		t.Fatalf("restore confirmation touched fleet: %+v", h.fleet.focused)
	}
}

func TestRestoreConfirmationRejectsInvalidContractBeforePrepare(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	var prepareCalls atomic.Int32
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		prepareCalls.Add(1)
		return &fakeRestorePrepared{preview: successfulRestorePreview()}, nil
	}
	tests := []struct {
		name   string
		id     string
		mutate func(map[string]any)
		status int
		code   string
	}{
		{"bad preview id", "bad", func(map[string]any) {}, http.StatusNotFound, "restore_preview_not_found"},
		{"malformed plan", preview.ID, func(v map[string]any) { v["plan_id"] = "bad" }, http.StatusBadRequest, "invalid_restore_plan"},
		{"stale plan", preview.ID, func(v map[string]any) { v["plan_id"] = restoreConfirmationContract + "." + strings.Repeat("f", 64) }, http.StatusConflict, "restore_plan_changed"},
		{"typed confirmation", preview.ID, func(v map[string]any) { v["typed_confirmation"] = "restore controller" }, http.StatusBadRequest, "restore_confirmation_incomplete"},
		{"restart acknowledgement", preview.ID, func(v map[string]any) { v["acknowledge_restart"] = false }, http.StatusBadRequest, "restore_confirmation_incomplete"},
		{"session acknowledgement", preview.ID, func(v map[string]any) { v["acknowledge_session_revocation"] = false }, http.StatusBadRequest, "restore_confirmation_incomplete"},
		{"suppression acknowledgement", preview.ID, func(v map[string]any) { v["acknowledge_router_writes_suppressed"] = false }, http.StatusBadRequest, "restore_confirmation_incomplete"},
		{"automatic apply acknowledgement", preview.ID, func(v map[string]any) { v["acknowledge_no_automatic_router_apply"] = false }, http.StatusBadRequest, "restore_confirmation_incomplete"},
		{"short export passphrase", preview.ID, func(v map[string]any) { v["export_passphrase"] = "short" }, http.StatusBadRequest, "invalid_export_passphrase"},
		{"empty runtime passphrase", preview.ID, func(v map[string]any) { v["destination_runtime_passphrase"] = "" }, http.StatusBadRequest, "invalid_runtime_passphrase"},
		{"unknown field", preview.ID, func(v map[string]any) { v["unexpected"] = true }, http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := restoreConfirmationRequest(preview.PlanID)
			tc.mutate(body)
			w := loopbackBackupDo(h, http.MethodPost,
				"/api/v1/restores/previews/"+tc.id+"/confirm", body)
			assertCodedError(t, w, tc.status, tc.code)
		})
	}
	if prepareCalls.Load() != 0 {
		t.Fatalf("invalid confirmations prepared state %d times", prepareCalls.Load())
	}
}

func TestRestoreConfirmationRevalidatesPreparedPlanAndOperationExclusivity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(*controllerrestore.Preview)
		hasConflict bool
		conflict    operationKind
		wantCode    string
		wantStatus  int
	}{
		{name: "prepared facts changed", mutate: func(p *controllerrestore.Preview) { p.Counts.Devices++ },
			wantCode: "restore_plan_changed", wantStatus: http.StatusConflict},
		{name: "active operation", hasConflict: true, conflict: operationBackup,
			wantCode: "restore_operation_conflict", wantStatus: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestoreHarness(t)
			_, preview := completedRestorePreview(t, h)
			facts := successfulRestorePreview()
			if tc.mutate != nil {
				tc.mutate(&facts)
			}
			prepared := &fakeRestorePrepared{preview: facts}
			h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
				[]byte, []byte) (restorePrepared, error) {
				return prepared, nil
			}
			var intentCalls atomic.Int32
			h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
				*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
				intentCalls.Add(1)
				return restoreswap.IntentResult{ID: strings.Repeat("2", 32)}, nil
			}
			var release func()
			if tc.hasConflict {
				var err error
				release, err = h.srv.operations.begin(tc.conflict)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			w := loopbackBackupDo(h, http.MethodPost,
				"/api/v1/restores/previews/"+preview.ID+"/confirm",
				restoreConfirmationRequest(preview.PlanID))
			assertCodedError(t, w, tc.wantStatus, tc.wantCode)
			if prepared.cleanupCalls.Load() != 1 || prepared.transferCalls.Load() != 0 || intentCalls.Load() != 0 {
				t.Fatalf("rejected confirmation cleanup=%d transfer=%d intent=%d",
					prepared.cleanupCalls.Load(), prepared.transferCalls.Load(), intentCalls.Load())
			}
		})
	}
}

func TestRestoreConfirmationCancellationAndAuditFailureCreateNoIntent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*harness, context.CancelFunc)
		status    int
		code      string
	}{
		{name: "cancelled", status: http.StatusRequestTimeout, code: "restore_confirmation_interrupted",
			configure: func(h *harness, cancel context.CancelFunc) { h.srv.afterRestorePrepared = cancel }},
		{name: "audit failure", status: http.StatusInternalServerError, code: "restore_audit_failed",
			configure: func(h *harness, _ context.CancelFunc) {
				h.srv.restoreAuditWrite = func(context.Context, store.Event) error { return errors.New("store failed") }
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestoreHarness(t)
			_, preview := completedRestorePreview(t, h)
			prepared := &fakeRestorePrepared{preview: successfulRestorePreview()}
			h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
				[]byte, []byte) (restorePrepared, error) {
				return prepared, nil
			}
			var intentCalls atomic.Int32
			h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
				*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
				intentCalls.Add(1)
				return restoreswap.IntentResult{}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			tc.configure(h, cancel)
			data, err := json.Marshal(restoreConfirmationRequest(preview.PlanID))
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/restores/previews/"+preview.ID+"/confirm", bytes.NewReader(data)).WithContext(ctx)
			req.Host, req.RemoteAddr = testSetupHost, "127.0.0.1:12345"
			for _, cookie := range h.cookies {
				req.AddCookie(cookie)
			}
			req.Header.Set(csrfHeader, h.csrf)
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)
			assertCodedError(t, w, tc.status, tc.code)
			if prepared.cleanupCalls.Load() != 1 || prepared.transferCalls.Load() != 0 || intentCalls.Load() != 0 {
				t.Fatalf("failed confirmation cleanup=%d transfer=%d intent=%d",
					prepared.cleanupCalls.Load(), prepared.transferCalls.Load(), intentCalls.Load())
			}
		})
	}
}

func TestRestoreConfirmationTimeoutCleansPreparedStateAndCreatesNoIntent(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	h.srv.restoreConfirmTimeout = time.Millisecond
	prepared := &fakeRestorePrepared{preview: successfulRestorePreview()}
	h.srv.restorePrepare = func(ctx context.Context, _ string, _ string, _ *secrets.Keeper,
		_, _ []byte) (restorePrepared, error) {
		<-ctx.Done()
		return prepared, ctx.Err()
	}
	var intentCalls atomic.Int32
	h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
		*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
		intentCalls.Add(1)
		return restoreswap.IntentResult{}, nil
	}
	var restarted atomic.Bool
	h.srv.RequestRestart = func() { restarted.Store(true) }
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusRequestTimeout, "restore_confirmation_interrupted")
	if prepared.cleanupCalls.Load() != 1 || prepared.transferCalls.Load() != 0 ||
		intentCalls.Load() != 0 || restarted.Load() || len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 {
		t.Fatalf("timeout cleanup=%d transfer=%d intent=%d restarted=%v hashing=%d ops=%v",
			prepared.cleanupCalls.Load(), prepared.transferCalls.Load(), intentCalls.Load(),
			restarted.Load(), len(h.srv.hashing), h.srv.ActiveOperations())
	}
}

func TestRestoreConfirmationRejectsActiveRouterReviewBeforePrepare(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	h.srv.suppressionMu.Lock()
	h.srv.RouterWriteSuppression = restoreswap.Suppression{
		Active: true, RestoreID: strings.Repeat("5", 32), CreatedAt: time.Now().UTC(),
	}
	h.srv.suppressionMu.Unlock()
	var prepareCalls, intentCalls atomic.Int32
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		prepareCalls.Add(1)
		return nil, nil
	}
	h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
		*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
		intentCalls.Add(1)
		return restoreswap.IntentResult{}, nil
	}
	var restarted atomic.Bool
	h.srv.RequestRestart = func() { restarted.Store(true) }
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusConflict, "router_review_required")
	if prepareCalls.Load() != 0 || intentCalls.Load() != 0 || restarted.Load() {
		t.Fatalf("active review prepare=%d intent=%d restarted=%v",
			prepareCalls.Load(), intentCalls.Load(), restarted.Load())
	}
}

func TestRestoreConfirmationSerializesAndClearsCancelledSecrets(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	started := make(chan struct{})
	var exportSeen, runtimeSeen []byte
	h.srv.restorePrepare = func(ctx context.Context, _ string, _ string, _ *secrets.Keeper,
		exportPassphrase, runtimePassphrase []byte) (restorePrepared, error) {
		exportSeen, runtimeSeen = exportPassphrase, runtimePassphrase
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	data, err := json.Marshal(restoreConfirmationRequest(preview.PlanID))
	if err != nil {
		t.Fatal(err)
	}
	firstReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm", bytes.NewReader(data)).WithContext(ctx)
	firstReq.Host, firstReq.RemoteAddr = testSetupHost, "127.0.0.1:12345"
	for _, cookie := range h.cookies {
		firstReq.AddCookie(cookie)
	}
	firstReq.Header.Set(csrfHeader, h.csrf)
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.mux.ServeHTTP(first, firstReq)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first confirmation did not start")
	}
	second := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, second, http.StatusConflict, "restore_confirmation_in_progress")
	if len(h.srv.hashing) != 1 || h.srv.operations.active[operationRestorePrepare] != 1 {
		t.Fatalf("serialized confirmation resources hashing=%d leases=%d",
			len(h.srv.hashing), h.srv.operations.active[operationRestorePrepare])
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled confirmation did not return")
	}
	assertCodedError(t, first, http.StatusRequestTimeout, "restore_confirmation_interrupted")
	if len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 || h.srv.restores.confirming != "" ||
		!bytes.Equal(exportSeen, make([]byte, len(exportSeen))) ||
		!bytes.Equal(runtimeSeen, make([]byte, len(runtimeSeen))) {
		t.Fatalf("cancel leaked resources hashing=%d ops=%v confirming=%q export=%q runtime=%q",
			len(h.srv.hashing), h.srv.ActiveOperations(), h.srv.restores.confirming,
			exportSeen, runtimeSeen)
	}
}

func TestRestoreConfirmationRejectsUploadMutationAfterPrepare(t *testing.T) {
	h := newRestoreHarness(t)
	upload, preview := completedRestorePreview(t, h)
	prepared := &fakeRestorePrepared{preview: successfulRestorePreview()}
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		path := filepath.Join(h.srv.RestoresDir, "uploads", upload.ID+".oowrtbak")
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		replacement := bytes.Repeat([]byte{'x'}, int(upload.SizeBytes))
		if _, err := file.WriteAt(replacement, 0); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			t.Fatal(err)
		}
		return prepared, nil
	}
	var intentCalls atomic.Int32
	h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
		*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
		intentCalls.Add(1)
		return restoreswap.IntentResult{}, nil
	}
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusConflict, "restore_upload_changed")
	if prepared.cleanupCalls.Load() != 1 || prepared.transferCalls.Load() != 0 || intentCalls.Load() != 0 {
		t.Fatalf("mutated upload cleanup=%d transfer=%d intent=%d",
			prepared.cleanupCalls.Load(), prepared.transferCalls.Load(), intentCalls.Load())
	}
}

func TestRestoreIntentFailureLeavesOnlyTruthfulAuthorizationAudit(t *testing.T) {
	h := newRestoreHarness(t)
	upload, preview := completedRestorePreview(t, h)
	prepared := &fakeRestorePrepared{preview: successfulRestorePreview()}
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		return prepared, nil
	}
	var events []store.Event
	h.srv.restoreAuditWrite = func(_ context.Context, event store.Event) error {
		events = append(events, event)
		return nil
	}
	var restarted atomic.Bool
	h.srv.RequestRestart = func() { restarted.Store(true) }
	var intentPair restoreswap.PreparedPair
	h.srv.restoreCreateIntent = func(_ context.Context, _ string, pair restoreswap.PreparedPair,
		_ *secrets.Keeper, _ []byte, _ string) (restoreswap.IntentResult, error) {
		intentPair = pair
		return restoreswap.IntentResult{}, errors.New("intent store failed")
	}
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusInternalServerError, "restore_intent_failed")
	if prepared.cleanupCalls.Load() != 1 || prepared.transferCalls.Load() != 1 || restarted.Load() {
		t.Fatalf("intent failure cleanup=%d transfer=%d restarted=%v",
			prepared.cleanupCalls.Load(), prepared.transferCalls.Load(), restarted.Load())
	}
	if len(events) != 1 || events[0].Event != "restore.confirmation_authorized" {
		t.Fatalf("intent failure audits=%+v", events)
	}
	detail, ok := events[0].Detail.(map[string]any)
	if !ok || detail["preview_id"] != preview.ID || detail["upload_id"] != upload.ID ||
		detail["restore_id"] != nil {
		t.Fatalf("confirmation audit identifiers=%+v", events[0].Detail)
	}
	if intentPair.AuthorizingAdminID <= 0 || intentPair.AuthorizingUsername != "admin" ||
		intentPair.PreviewID != preview.ID || intentPair.PlanID != preview.PlanID {
		t.Fatalf("intent actor=%+v", intentPair)
	}
	for _, event := range events {
		if strings.Contains(event.Event, "accepted") || strings.Contains(event.Event, "applied") {
			t.Fatalf("failed intent emitted false audit event %q", event.Event)
		}
	}
	marker := filepath.Join(filepath.Dir(h.srv.RestoresDir), ".oonfeewrt-recovery", "pending-restore.json")
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed intent left marker: %v", err)
	}
}

func TestRestoreTransferredWarningDoesNotCleanupOrRestart(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	prepared := &fakeRestorePrepared{preview: successfulRestorePreview(), afterTransfer: errors.New("anchor close warning")}
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		return prepared, nil
	}
	h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
		*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
		return restoreswap.IntentResult{ID: strings.Repeat("2", 32)}, nil
	}
	var restarted atomic.Bool
	h.srv.RequestRestart = func() { restarted.Store(true) }
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusInternalServerError, "restore_intent_finalize_failed")
	if prepared.cleanupCalls.Load() != 0 || !prepared.transferred.Load() || restarted.Load() {
		t.Fatalf("transferred warning cleanup=%d transferred=%v restarted=%v",
			prepared.cleanupCalls.Load(), prepared.transferred.Load(), restarted.Load())
	}
}

func TestRestoreConfirmationWrongPassphraseLeaksNothingAndClearsBuffers(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	var exportSeen, runtimeSeen []byte
	h.srv.restorePrepare = func(_ context.Context, _ string, _ string, _ *secrets.Keeper,
		exportPassphrase, runtimePassphrase []byte) (restorePrepared, error) {
		exportSeen, runtimeSeen = exportPassphrase, runtimePassphrase
		return nil, errors.Join(errors.New("private path "+h.srv.RestoresDir), secrets.ErrBadPassphrase)
	}
	var intentCalls atomic.Int32
	h.srv.restoreCreateIntent = func(context.Context, string, restoreswap.PreparedPair,
		*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error) {
		intentCalls.Add(1)
		return restoreswap.IntentResult{}, nil
	}
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusUnprocessableEntity, "restore_authentication_failed")
	for _, forbidden := range []string{restoreTestPassphrase, "destination-runtime-secret-value", h.srv.RestoresDir, "private path"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("authentication failure leaked %q: %s", forbidden, w.Body.String())
		}
	}
	if intentCalls.Load() != 0 || !bytes.Equal(exportSeen, make([]byte, len(exportSeen))) ||
		!bytes.Equal(runtimeSeen, make([]byte, len(runtimeSeen))) {
		t.Fatalf("authentication failure intent=%d export=%q runtime=%q",
			intentCalls.Load(), exportSeen, runtimeSeen)
	}
}

func TestRestoreConfirmationSurfacesPreparedCleanupFailure(t *testing.T) {
	h := newRestoreHarness(t)
	_, preview := completedRestorePreview(t, h)
	facts := successfulRestorePreview()
	facts.Counts.Devices++
	prepared := &fakeRestorePrepared{preview: facts, cleanupErr: errors.New("cleanup failed")}
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		return prepared, nil
	}
	w := loopbackBackupDo(h, http.MethodPost,
		"/api/v1/restores/previews/"+preview.ID+"/confirm",
		restoreConfirmationRequest(preview.PlanID))
	assertCodedError(t, w, http.StatusInternalServerError, "restore_cleanup_failed")
	if prepared.cleanupCalls.Load() != 1 || prepared.transferCalls.Load() != 0 {
		t.Fatalf("cleanup failure calls cleanup=%d transfer=%d",
			prepared.cleanupCalls.Load(), prepared.transferCalls.Load())
	}
}

func TestRestoreConfirmationAuthTransportAndReauthFailBeforePrepare(t *testing.T) {
	h := newRestoreHarness(t)
	var prepareCalls atomic.Int32
	h.srv.restorePrepare = func(context.Context, string, string, *secrets.Keeper,
		[]byte, []byte) (restorePrepared, error) {
		prepareCalls.Add(1)
		return nil, nil
	}
	id := strings.Repeat("A", 43)
	plan := restoreConfirmationContract + "." + strings.Repeat("a", 64)
	w := backupDo(h, http.MethodPost, "/api/v1/restores/previews/"+id+"/confirm",
		restoreConfirmationRequest(plan), "192.0.2.10:12345", false)
	assertCodedError(t, w, http.StatusUpgradeRequired, "secure_transport_required")

	base := h.srv.now()
	h.srv.Now = func() time.Time { return base.Add(reauthValidity + time.Second) }
	w = loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/previews/"+id+"/confirm",
		restoreConfirmationRequest(plan))
	assertCodedError(t, w, http.StatusPreconditionRequired, "reauth_required")
	if prepareCalls.Load() != 0 {
		t.Fatalf("denied confirmation prepared state %d times", prepareCalls.Load())
	}
}

func TestRestoreSuppressionStatusResumeAndOperationGate(t *testing.T) {
	h := newHarness(t)
	h.srv.RestoresDir = filepath.Join(t.TempDir(), "restores")
	h.srv.RestoreOwnerInstanceID = strings.Repeat("1", 32)
	h.srv.RequestRestart = func() {}
	created := time.Date(2026, 8, 23, 15, 0, 0, 0, time.UTC)
	restoreID := strings.Repeat("3", 32)
	h.srv.RouterWriteSuppression = restoreswap.Suppression{
		Active: true, RestoreID: restoreID, CreatedAt: created, Reason: "owner review required",
	}
	if err := h.srv.InitRestores(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !h.srv.CloseJobs(10 * time.Second) {
			t.Error("restore jobs did not close")
		}
	})
	var resumeCalls, resumedCalls atomic.Int32
	h.srv.ResumeRouterWrites = func(_ context.Context, id string) error {
		resumeCalls.Add(1)
		if id != restoreID {
			t.Fatalf("resume id=%q", id)
		}
		if _, err := h.srv.operations.begin(operationApply); !errors.Is(err, errOperationRouterSuppressed) {
			t.Fatalf("router gate opened before durable clear: %v", err)
		}
		return nil
	}
	h.srv.RouterWritesResumed = func() {
		resumedCalls.Add(1)
		release, err := h.srv.operations.begin(operationApply)
		if err != nil {
			t.Fatalf("router gate remained closed after durable clear: %v", err)
		}
		release()
	}
	h.setup()
	h.srv.operations.setSuppression(true)
	status := loopbackBackupDo(h, http.MethodGet, "/api/v1/restores/suppression", nil)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), restoreID) ||
		!strings.Contains(status.Body.String(), created.Format(time.RFC3339)) {
		t.Fatalf("suppression status=%d body=%s", status.Code, status.Body.String())
	}
	for _, kind := range []operationKind{
		operationApply, operationAdopt, operationUnadopt, operationRFScan,
		operationCapability, operationNeighbourReconcile,
	} {
		if _, err := h.srv.operations.begin(kind); !errors.Is(err, errOperationRouterSuppressed) {
			t.Fatalf("suppressed operation %s error=%v", operationKindNames[kind], err)
		}
	}
	for _, kind := range []operationKind{
		operationSpeedTest, operationDiagnostics, operationBackup, operationRestorePrepare,
	} {
		release, err := h.srv.operations.begin(kind)
		if err != nil {
			t.Fatalf("allowed operation %s error=%v", operationKindNames[kind], err)
		}
		release()
	}
	resume := loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/suppression/resume", map[string]any{
		"restore_id": restoreID, "typed_confirmation": restoreResumeConfirmation,
	})
	if resume.Code != http.StatusOK || !strings.Contains(resume.Body.String(), `"active":false`) {
		t.Fatalf("resume status=%d body=%s", resume.Code, resume.Body.String())
	}
	if resumeCalls.Load() != 1 || resumedCalls.Load() != 1 {
		t.Fatalf("resume hooks durable=%d resumed=%d", resumeCalls.Load(), resumedCalls.Load())
	}
}

func TestRestoreSuppressionResumeFailureKeepsGateClosed(t *testing.T) {
	h := newHarness(t)
	h.srv.RestoresDir = filepath.Join(t.TempDir(), "restores")
	restoreID := strings.Repeat("4", 32)
	h.srv.RouterWriteSuppression = restoreswap.Suppression{Active: true, RestoreID: restoreID, CreatedAt: time.Now().UTC()}
	h.srv.ResumeRouterWrites = func(context.Context, string) error { return errors.New("disk clear failed") }
	if err := h.srv.InitRestores(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.srv.CloseJobs(time.Second) })
	h.setup()
	h.srv.operations.setSuppression(true)
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/suppression/resume", map[string]any{
		"restore_id": restoreID, "typed_confirmation": restoreResumeConfirmation,
	})
	assertCodedError(t, w, http.StatusInternalServerError, "resume_router_writes_failed")
	if _, err := h.srv.operations.begin(operationApply); !errors.Is(err, errOperationRouterSuppressed) {
		t.Fatalf("failed durable clear opened router gate: %v", err)
	}
	if !h.srv.restoreSuppressionDTO().Active {
		t.Fatal("failed durable clear removed suppression status")
	}
}

func TestRestoreSuppressionSuccessAuditFailureDoesNotRecloseGate(t *testing.T) {
	h := newHarness(t)
	h.srv.RestoresDir = filepath.Join(t.TempDir(), "restores")
	restoreID := strings.Repeat("5", 32)
	h.srv.RouterWriteSuppression = restoreswap.Suppression{
		Active: true, RestoreID: restoreID, CreatedAt: time.Now().UTC(),
	}
	h.srv.ResumeRouterWrites = func(context.Context, string) error { return nil }
	var audits atomic.Int32
	h.srv.restoreAuditWrite = func(ctx context.Context, _ store.Event) error {
		if audits.Add(1) == 2 {
			deadline, ok := ctx.Deadline()
			remaining := time.Until(deadline)
			if !ok || remaining <= 0 || remaining > restoreAuditTimeout {
				t.Errorf("resumed audit deadline ok=%v remaining=%s", ok, remaining)
			}
			return errors.New("injected resumed audit failure")
		}
		return nil
	}
	if err := h.srv.InitRestores(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.srv.CloseJobs(time.Second) })
	h.setup()
	h.srv.operations.setSuppression(true)
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/suppression/resume", map[string]any{
		"restore_id": restoreID, "typed_confirmation": restoreResumeConfirmation,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", w.Code, w.Body.String())
	}
	if audits.Load() != 2 || h.srv.restoreSuppressionDTO().Active {
		t.Fatalf("audits=%d suppression=%+v", audits.Load(), h.srv.restoreSuppressionDTO())
	}
	release, err := h.srv.operations.begin(operationApply)
	if err != nil {
		t.Fatalf("success audit failure reclosed router gate: %v", err)
	}
	release()
}

func TestRestoreTransportOwnerAndRecentReauthentication(t *testing.T) {
	h := newRestoreHarness(t)
	w := backupDo(h, http.MethodGet, "/api/v1/restores", nil, "192.0.2.10:12345", false)
	assertCodedError(t, w, http.StatusUpgradeRequired, "secure_transport_required")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/restores", nil)
	req.Host, req.RemoteAddr = testSetupHost, "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	assertCodedError(t, w, http.StatusUpgradeRequired, "secure_transport_required")
	w = backupDo(h, http.MethodGet, "/api/v1/restores", nil, "192.0.2.10:12345", true)
	if w.Code != http.StatusOK {
		t.Fatalf("TLS descriptor status=%d body=%s", w.Code, w.Body.String())
	}

	_, viewer := seedAccount(t, h, "restore-viewer", store.RoleViewer)
	token, sess, err := h.srv.sessions.create(viewer.ID, viewer.Username, viewer.Role,
		"127.0.0.1", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	h.cookies, h.csrf = []*http.Cookie{{Name: sessionCookie, Value: token}}, sess.csrf
	w = loopbackBackupDo(h, http.MethodGet, "/api/v1/restores", nil)
	assertCodedError(t, w, http.StatusForbidden, "insufficient_role")

	h = newRestoreHarness(t)
	base := h.srv.now()
	h.srv.Now = func() time.Time { return base.Add(reauthValidity + time.Second) }
	w = restoreRawDo(h, []byte("encrypted"), 9, restoreMediaType, "127.0.0.1:12345", false)
	assertCodedError(t, w, http.StatusPreconditionRequired, "reauth_required")
	if entries, err := os.ReadDir(filepath.Join(h.srv.RestoresDir, "uploads")); err != nil || len(entries) != 0 {
		t.Fatalf("expired reauth wrote files=%v err=%v", entries, err)
	}
}

func TestRestoreUploadRejectsMediaLengthTransferAndTrailingBytes(t *testing.T) {
	h := newRestoreHarness(t)
	cases := []struct {
		name, media string
		data        []byte
		length      int64
		transfer    bool
		status      int
		code        string
	}{
		{"media", "application/octet-stream", []byte("x"), 1, false, http.StatusUnsupportedMediaType, "invalid_restore_media_type"},
		{"missing length", restoreMediaType, []byte("x"), -1, false, http.StatusLengthRequired, "restore_content_length_required"},
		{"empty", restoreMediaType, nil, 0, false, http.StatusLengthRequired, "restore_content_length_required"},
		{"too large", restoreMediaType, nil, restoreUploadMaxBytes + 1, false, http.StatusRequestEntityTooLarge, "restore_upload_too_large"},
		{"transfer", restoreMediaType, []byte("x"), 1, true, http.StatusRequestEntityTooLarge, "restore_upload_too_large"},
		{"trailing", restoreMediaType, []byte("two"), 2, false, http.StatusBadRequest, "restore_upload_length_mismatch"},
		{"truncated", restoreMediaType, []byte("x"), 2, false, http.StatusBadRequest, "restore_upload_length_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/restores/uploads", bytes.NewReader(tc.data))
			req.Host, req.RemoteAddr, req.ContentLength = testSetupHost, "127.0.0.1:12345", tc.length
			req.Header.Set("Content-Type", tc.media)
			if tc.transfer {
				req.TransferEncoding = []string{"chunked"}
			}
			for _, cookie := range h.cookies {
				req.AddCookie(cookie)
			}
			req.Header.Set(csrfHeader, h.csrf)
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)
			assertCodedError(t, w, tc.status, tc.code)
		})
	}
	if entries, err := os.ReadDir(filepath.Join(h.srv.RestoresDir, "uploads")); err != nil || len(entries) != 0 {
		t.Fatalf("rejected uploads left files=%v err=%v", entries, err)
	}
}

func TestRestoreUploadNoClobberAndPreviewRejectsSymlinkSwap(t *testing.T) {
	h := newRestoreHarness(t)
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var collision string
	h.srv.beforeRestoreUploadPublish = func(_, final string) {
		collision = filepath.Join(h.srv.RestoresDir, "uploads", final)
		if err := os.Symlink(sentinel, collision); err != nil {
			t.Fatal(err)
		}
	}
	w := restoreRawDo(h, []byte("encrypted"), 9, restoreMediaType, "127.0.0.1:12345", false)
	assertCodedError(t, w, http.StatusInternalServerError, "restore_upload_failed")
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("upload collision changed sentinel data=%q err=%v", data, err)
	}
	if info, err := os.Lstat(collision); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("collision was overwritten info=%v err=%v", info, err)
	}
	if err := os.Remove(collision); err != nil {
		t.Fatal(err)
	}
	h.srv.beforeRestoreUploadPublish = nil

	upload := uploadRestore(t, h, []byte("encrypted"))
	called := false
	h.srv.restoreInspect = func(context.Context, string, string, []byte) (controllerrestore.Preview, error) {
		called = true
		return successfulRestorePreview(), nil
	}
	h.srv.beforeRestorePreviewCheck = func(id string) {
		path := filepath.Join(h.srv.RestoresDir, "uploads", id+".oowrtbak")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sentinel, path); err != nil {
			t.Fatal(err)
		}
		h.srv.beforeRestorePreviewCheck = nil
	}
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	failed := waitRestorePreview(t, h, started.ID, "failed")
	if called || failed.ErrorCode != "invalid_backup" || failed.Manifest != nil || failed.Counts != nil || failed.PlanID != "" {
		t.Fatalf("swapped upload reached inspector or leaked facts called=%v preview=%+v", called, failed)
	}
	if err := os.Remove(filepath.Join(h.srv.RestoresDir, "uploads", upload.ID+".oowrtbak")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreWrongPassphraseAndCancellationClearSecretsAndLeases(t *testing.T) {
	h := newRestoreHarness(t)
	upload := uploadRestore(t, h, []byte("encrypted"))
	h.srv.restoreInspect = func(context.Context, string, string, []byte) (controllerrestore.Preview, error) {
		return controllerrestore.Preview{}, fmtWrapBadPassphrase()
	}
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	failed := waitRestorePreview(t, h, started.ID, "failed")
	if failed.ErrorCode != "authentication_failed" || failed.Manifest != nil || failed.SourceSchema != nil ||
		failed.TargetSchema != nil || failed.Counts != nil || failed.PlanID != "" ||
		strings.Contains(failed.Error, restoreTestPassphrase) || strings.Contains(failed.Error, h.srv.RestoresDir) {
		t.Fatalf("wrong-pass preview leaked facts: %+v", failed)
	}

	startedCall := make(chan struct{})
	var captured []byte
	h.srv.restoreInspect = func(ctx context.Context, _ string, _ string, passphrase []byte) (controllerrestore.Preview, error) {
		captured = passphrase
		close(startedCall)
		<-ctx.Done()
		return controllerrestore.Preview{}, ctx.Err()
	}
	second := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	select {
	case <-startedCall:
	case <-time.After(3 * time.Second):
		t.Fatal("preview did not start")
	}
	if len(h.srv.hashing) != 1 || !containsString(h.srv.ActiveOperations(), "restore_prepare") {
		t.Fatalf("preview resources hashing=%d operations=%v", len(h.srv.hashing), h.srv.ActiveOperations())
	}
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/previews", map[string]any{
		"upload_id": upload.ID, "export_passphrase": restoreTestPassphrase,
	})
	assertCodedError(t, w, http.StatusConflict, "restore_preview_in_progress")
	if len(h.srv.hashing) != 1 || h.srv.operations.active[operationRestorePrepare] != 1 {
		t.Fatalf("rejected preview leaked resources hashing=%d leases=%d",
			len(h.srv.hashing), h.srv.operations.active[operationRestorePrepare])
	}
	w = loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/previews/"+second.ID+"/cancel", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", w.Code, w.Body.String())
	}
	_ = waitRestorePreview(t, h, second.ID, "cancelled")
	deadline := time.Now().Add(3 * time.Second)
	for (len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	h.srv.restores.wg.Wait()
	if len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 ||
		!bytes.Equal(captured, make([]byte, len(captured))) {
		t.Fatalf("cancel leaked secret/resources hashing=%d operations=%v secret=%q",
			len(h.srv.hashing), h.srv.ActiveOperations(), captured)
	}
}

func TestRestoreCancelAfterSuccessfulInspectCannotPublishPlan(t *testing.T) {
	h := newRestoreHarness(t)
	upload := uploadRestore(t, h, []byte("encrypted"))
	inspected, release := make(chan struct{}), make(chan struct{})
	h.srv.restoreInspect = func(context.Context, string, string, []byte) (controllerrestore.Preview, error) {
		return successfulRestorePreview(), nil
	}
	h.srv.afterRestoreInspected = func(string) {
		close(inspected)
		<-release
	}
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	select {
	case <-inspected:
	case <-time.After(3 * time.Second):
		t.Fatal("preview did not reach post-inspection boundary")
	}
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/restores/previews/"+started.ID+"/cancel", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", w.Code, w.Body.String())
	}
	close(release)
	finished := waitRestorePreview(t, h, started.ID, "cancelled")
	if finished.PlanID != "" || finished.Manifest != nil || finished.SourceSchema != nil ||
		finished.TargetSchema != nil || finished.Counts != nil {
		t.Fatalf("cancelled preview published restore facts: %+v", finished)
	}
}

func TestRestorePreviewRejectsSameInodeSameSizeMutationAfterInspect(t *testing.T) {
	h := newRestoreHarness(t)
	original := []byte("encrypted-one")
	replacement := []byte("encrypted-two")
	if len(original) != len(replacement) {
		t.Fatal("mutation fixture size mismatch")
	}
	upload := uploadRestore(t, h, original)
	h.srv.restoreInspect = func(_ context.Context, path, _ string, _ []byte) (controllerrestore.Preview, error) {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt(replacement, 0); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return successfulRestorePreview(), nil
	}
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	failed := waitRestorePreview(t, h, started.ID, "failed")
	if failed.ErrorCode != "invalid_backup" || failed.PlanID != "" || failed.Manifest != nil || failed.Counts != nil {
		t.Fatalf("mutated upload published preview facts: %+v", failed)
	}
}

func TestRestoreUploadCancellationAfterPublicationRemovesAndSyncsArtifact(t *testing.T) {
	h := newRestoreHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.srv.afterRestoreUploadPublish = cancel
	_, err := h.srv.restores.receiveUpload(ctx, bytes.NewReader([]byte("encrypted")), 9, h.srv.now())
	h.srv.afterRestoreUploadPublish = nil
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-publication cancellation error=%v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(h.srv.RestoresDir, "uploads")); err != nil || len(entries) != 0 {
		t.Fatalf("cancelled publication residue=%v err=%v", entries, err)
	}
}

func TestRestoreCloseSurfacesResidueAndRetriesWithoutDeletingUnknownData(t *testing.T) {
	h := newRestoreHarness(t)
	upload := uploadRestore(t, h, []byte("encrypted"))
	unknown := filepath.Join(h.srv.RestoresDir, "scratch", "operator-file")
	if err := os.WriteFile(unknown, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h.srv.CloseJobs(time.Second) {
		t.Fatal("restore close succeeded with unowned scratch residue")
	}
	if data, err := os.ReadFile(unknown); err != nil || string(data) != "keep" {
		t.Fatalf("failed close changed unknown data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.srv.RestoresDir, "uploads", upload.ID+".oowrtbak")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned encrypted upload was not cleaned before refusal: %v", err)
	}
	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	if !h.srv.CloseJobs(time.Second) {
		t.Fatal("restore close did not succeed after residue was resolved")
	}
}

func TestRestoreParentReplacementNeverReadsOrCleansWrongTree(t *testing.T) {
	h := newRestoreHarness(t)
	upload := uploadRestore(t, h, []byte("encrypted"))
	original := h.srv.RestoresDir + ".original"
	if err := os.Rename(h.srv.RestoresDir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.srv.RestoresDir, "uploads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.srv.RestoresDir, "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(h.srv.RestoresDir, "uploads", upload.ID+".oowrtbak")
	if err := os.WriteFile(sentinel, []byte("wrong-tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	h.srv.restoreInspect = func(context.Context, string, string, []byte) (controllerrestore.Preview, error) {
		called = true
		return successfulRestorePreview(), nil
	}
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	failed := waitRestorePreview(t, h, started.ID, "failed")
	if called || failed.PlanID != "" || failed.Manifest != nil {
		t.Fatalf("replacement tree reached preview called=%v preview=%+v", called, failed)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "wrong-tree" {
		t.Fatalf("replacement tree changed data=%q err=%v", data, err)
	}
	if h.srv.CloseJobs(time.Second) {
		t.Fatal("close accepted a replaced restore parent")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "wrong-tree" {
		t.Fatalf("refused close changed replacement data=%q err=%v", data, err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.srv.RestoresDir, "uploads")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.srv.RestoresDir, "scratch")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.srv.RestoresDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, h.srv.RestoresDir); err != nil {
		t.Fatal(err)
	}
	if !h.srv.CloseJobs(time.Second) {
		t.Fatal("close did not recover after original restore parent returned")
	}
}

func TestRestoreCloseTimesOutWithoutClosingUnderRunningPreview(t *testing.T) {
	h := newRestoreHarness(t)
	upload := uploadRestore(t, h, []byte("encrypted"))
	startedCall, release := make(chan struct{}), make(chan struct{})
	h.srv.restoreInspect = func(context.Context, string, string, []byte) (controllerrestore.Preview, error) {
		close(startedCall)
		<-release
		return successfulRestorePreview(), nil
	}
	started := startRestorePreview(t, h, upload.ID, restoreTestPassphrase)
	select {
	case <-startedCall:
	case <-time.After(3 * time.Second):
		t.Fatal("preview did not start")
	}
	if h.srv.CloseJobs(5 * time.Millisecond) {
		t.Fatal("close succeeded while preview ignored cancellation")
	}
	if _, err := os.Stat(filepath.Join(h.srv.RestoresDir, "uploads", upload.ID+".oowrtbak")); err != nil {
		t.Fatalf("timed-out close removed active upload: %v", err)
	}
	close(release)
	_ = waitRestorePreview(t, h, started.ID, "failed")
	if !h.srv.CloseJobs(3 * time.Second) {
		t.Fatal("close did not drain released preview")
	}
}

func fmtWrapBadPassphrase() error {
	return errors.Join(errors.New("private internal detail"), secrets.ErrBadPassphrase)
}

func TestRestorePlanDigestStableAndBindsEveryConfirmationFact(t *testing.T) {
	preview := successfulRestorePreview()
	uploadSHA := strings.Repeat("c", 64)
	first, err := restorePlanID(uploadSHA, preview)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restorePlanID(uploadSHA, preview)
	if err != nil || first != second {
		t.Fatalf("unstable plan first=%q second=%q err=%v", first, second, err)
	}
	mutations := []func(*controllerrestore.Preview){
		func(p *controllerrestore.Preview) { p.SourceSchema-- },
		func(p *controllerrestore.Preview) { p.TargetSchema-- },
		func(p *controllerrestore.Preview) { p.Counts.Devices++ },
		func(p *controllerrestore.Preview) { p.Manifest.ControllerVersion += "-changed" },
		func(p *controllerrestore.Preview) { p.Manifest.Database.SHA256 = strings.Repeat("d", 64) },
	}
	for i, mutate := range mutations {
		changed := preview
		mutate(&changed)
		got, err := restorePlanID(uploadSHA, changed)
		if err != nil || got == first {
			t.Fatalf("mutation %d did not change plan got=%q err=%v", i, got, err)
		}
	}
	if changed, err := restorePlanID(strings.Repeat("d", 64), preview); err != nil || changed == first {
		t.Fatalf("upload digest did not bind plan got=%q err=%v", changed, err)
	}
	if _, err := restorePlanID("not-a-digest", preview); err == nil {
		t.Fatal("invalid upload digest was accepted")
	}
}

func TestRestoreStartupAndCloseCleanOnlyOwnedResidue(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "restores")
	uploads, scratch := filepath.Join(dir, "uploads"), filepath.Join(dir, "scratch")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, id+".oowrtbak"), []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	tempID, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, ".upload-"+tempID+".tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(scratch, ".oonfeewrt-preview-"+strings.Repeat("a", 32)+".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := New(newHarness(t).db, newStubFleet(), nil, quiet())
	srv.RestoresDir = dir
	if err := srv.InitRestores(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(uploads); err != nil || len(entries) != 0 {
		t.Fatalf("startup upload cleanup=%v err=%v", entries, err)
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Fatalf("startup scratch cleanup=%v err=%v", entries, err)
	}
	if !srv.restores.close(time.Second) {
		t.Fatal("idle restores did not close")
	}
	for _, path := range []string{uploads, scratch} {
		if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
			t.Fatalf("restore close residue at %s: %v err=%v", path, entries, err)
		}
	}

	unowned := filepath.Join(t.TempDir(), "restores")
	if err := os.MkdirAll(filepath.Join(unowned, "uploads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unowned, "operator-file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prepared, err := prepareRestoresDir(unowned); err == nil {
		prepared.dirRoot.Close()
		prepared.uploadsRoot.Close()
		prepared.scratchRoot.Close()
		t.Fatal("unowned restore entry was accepted")
	}
	if data, err := os.ReadFile(filepath.Join(unowned, "operator-file")); err != nil || string(data) != "keep" {
		t.Fatalf("unowned restore entry changed data=%q err=%v", data, err)
	}

	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "restores")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}
	if prepared, err := prepareRestoresDir(linked); err == nil {
		prepared.dirRoot.Close()
		prepared.uploadsRoot.Close()
		prepared.scratchRoot.Close()
		t.Fatal("symlink restore root was accepted")
	}
}

var _ io.Reader = (*restoreContextReader)(nil)
