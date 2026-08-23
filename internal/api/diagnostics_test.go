package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/diagnostics"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func diagnosticHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.DiagnosticsDir = filepath.Join(t.TempDir(), "diagnostics")
	h.srv.ControllerVersion = "v0.1.0-test"
	h.srv.ControllerStartedAt = time.Now().Add(-time.Minute)
	h.srv.ControllerLogTail = func(int) ([]byte, []string, error) {
		return []byte("{\"level\":\"INFO\",\"username\":\"RouterLoginSecret\",\"password\":\"BundlePasswordSecret\",\"client\":\"KitchenCameraUnique\",\"device\":\"StaleUnadoptedDeviceUnique\",\"host\":\"router.private.example\",\"hostname\":\"RouterHostnameUnique\",\"ssids\":[\"PrivateSSIDUnique\"],\"network\":\"PrivateNetworkUnique\",\"mesh_id\":\"PrivateMeshUnique\",\"zone\":\"PrivateZoneUnique\",\"data_dir\":\"/Users/private/ControllerDataUnique\",\"cert_fingerprint\":\"CertificateFingerprintUnique\",\"sha256\":\"CertificateHashUnique\"}\n"),
			[]string{"controller log backup-3 segment is unavailable"}, nil
	}
	if err := h.srv.InitDiagnostics(); err != nil {
		t.Fatal(err)
	}
	h.mux = h.srv.Routes()
	t.Cleanup(func() {
		if !h.srv.CloseJobs(3 * time.Second) {
			t.Error("diagnostic jobs did not close")
		}
	})
	return h
}

func TestDiagnosticsDescriptorRolesStoredModeAndZeroFleetCalls(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	h.srv.Fleet = nil // Any diagnostics Fleet use would panic.
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role store.AccountRole
		want int
	}{{store.RoleViewer, http.StatusForbidden}, {store.RoleOperator, http.StatusForbidden},
		{store.RoleAdmin, http.StatusOK}, {store.RoleOwner, http.StatusOK}} {
		t.Run(string(tc.role), func(t *testing.T) {
			token, sess, err := h.srv.sessions.create(owner.ID, owner.Username, tc.role, "192.0.2.10", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
			h.csrf = sess.csrf
			w := h.do(http.MethodGet, "/api/v1/diagnostics", nil)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusOK {
				body := responseMap(t, w)
				if body["mode"] != "stored" || body["router_management_calls"] != false || body["router_changes"] != false {
					t.Fatalf("descriptor=%v", body)
				}
				if strings.Contains(w.Body.String(), h.srv.DiagnosticsDir) {
					t.Fatal("descriptor leaked diagnostics path")
				}
			}
		})
	}
}

func TestDiagnosticsGenerateDownloadAndRedactStoredEvidence(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	ctx := context.Background()
	registry := capability.NewRegistry()
	registry.Board = capability.Board{Model: "Router Model", Target: "ath79/generic", Kernel: "6.6", Release: "OpenWrt test"}
	device := h.seedDevice("HallRouterUnique", true, ptrInt64(1_700_000_123))
	device.Host = "router.private.example"
	if err := h.db.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetCapabilities(ctx, device.ID, registry, "A"); err != nil {
		t.Fatal(err)
	}
	const sourceMS = int64(1_700_000_123_456)
	if _, err := h.db.SQL().ExecContext(ctx, `INSERT INTO topology_source_states
(device_id,source,state,reason,observed_at) VALUES (?,?,?,?,?)`, device.ID, "lldp", "error", "permission denied", sourceMS); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `INSERT INTO clients
(mac,name,note,ip,fixed_ip) VALUES (?,?,?,?,?)`, "02:00:00:00:00:22", "KitchenCameraUnique",
		"ClientPrivateNote", "192.0.2.88", "192.0.2.99"); err != nil {
		t.Fatal(err)
	}
	owner, account := seedAccount(t, h, "DiagnosticAccountUnique", store.RoleViewer)
	_ = owner
	if err := h.db.LogEvent(ctx, store.Event{TS: 1_700_000_124, DeviceID: &device.ID,
		Category: "system", Severity: "warning", Event: "diagnostic.test",
		Source: "controller", Action: "inspect", Detail: map[string]any{
			"login": "RouterEventLoginSecret", "old_name": "HistoricalDeviceNameSecret",
			"account": account.Username,
		}}); err != nil {
		t.Fatal(err)
	}

	input, err := h.srv.collectDiagnosticInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Sources) == 0 || input.Sources[0].ObservedAt.UnixMilli() != sourceMS {
		t.Fatalf("source timestamp=%+v", input.Sources)
	}
	foundEvent := false
	for _, event := range input.Events {
		if strings.Contains(event.Message, "diagnostic.test") {
			foundEvent = true
			if event.At.Unix() != 1_700_000_124 || strings.Contains(event.Message, "RouterEventLoginSecret") ||
				strings.Contains(event.Message, "HistoricalDeviceNameSecret") {
				t.Fatalf("event=%+v", event)
			}
		}
	}
	if !foundEvent {
		t.Fatal("diagnostic event missing")
	}

	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start=%d %s", w.Code, w.Body.String())
	}
	var started struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, w, &started)
	if started.Job.ID == "" || strings.Contains(w.Body.String(), h.srv.DiagnosticsDir) {
		t.Fatalf("unsafe start response: %s", w.Body.String())
	}
	job := waitDiagnosticState(t, h, started.Job.ID, "completed")
	if job.SizeBytes == nil || *job.SizeBytes <= 0 {
		t.Fatalf("completed job=%+v", job)
	}
	w = h.do(http.MethodGet, "/api/v1/diagnostics/"+started.Job.ID+"/download", nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/zip" ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
		t.Fatalf("download=%d headers=%v", w.Code, w.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var unpacked strings.Builder
	for _, file := range zr.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(&unpacked, r)
		_ = r.Close()
	}
	for _, secret := range []string{
		"RouterLoginSecret", "BundlePasswordSecret", "KitchenCameraUnique",
		"router.private.example", "HallRouterUnique", "DiagnosticAccountUnique",
		"ClientPrivateNote", "192.0.2.99", "RouterEventLoginSecret", "HistoricalDeviceNameSecret",
		"RouterHostnameUnique", "PrivateSSIDUnique", "PrivateNetworkUnique", "PrivateMeshUnique", "PrivateZoneUnique",
		"ControllerDataUnique", "CertificateFingerprintUnique", "CertificateHashUnique",
		"StaleUnadoptedDeviceUnique",
	} {
		if strings.Contains(unpacked.String(), secret) {
			t.Errorf("bundle leaked %q", secret)
		}
	}
}

func TestSanitizeControllerLogRedactsStaleDeviceAndClientNames(t *testing.T) {
	raw := []byte(`{"level":"WARN","device":"DeletedRouterUnique","nested":{"client":"DeletedClientUnique"}}` + "\n")
	got, omitted := sanitizeControllerLog(raw)
	if omitted {
		t.Fatal("valid log record was omitted")
	}
	for _, secret := range []string{"DeletedRouterUnique", "DeletedClientUnique"} {
		if bytes.Contains(got, []byte(secret)) {
			t.Fatalf("sanitized controller log leaked stale identifier %q: %s", secret, got)
		}
	}
	if strings.Count(string(got), `"[redacted]"`) != 2 {
		t.Fatalf("structured identifiers were not redacted: %s", got)
	}
}

func TestDiagnosticsCancellationWinsAfterSuccessfulGeneratorReturn(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	h.srv.diagnosticGenerate = func(_ context.Context, output string, _ diagnostics.Input) (diagnostics.Result, error) {
		if err := os.WriteFile(output, []byte("not retained"), 0o600); err != nil {
			return diagnostics.Result{}, err
		}
		return diagnostics.Result{Path: output, Size: 12}, nil
	}
	h.srv.afterDiagnosticGenerated = func(id string) {
		if _, err := h.srv.diagnostics.cancelJob(id, h.srv.now(), 0, "test"); err != nil {
			t.Errorf("cancel after Generate: %v", err)
		}
	}
	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	var body struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, w, &body)
	wantPath := filepath.Join(h.srv.DiagnosticsDir, body.Job.ID+".zip")
	waitDiagnosticState(t, h, body.Job.ID, "cancelled")
	if _, err := os.Stat(wantPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled successful output remains: %v", err)
	}
}

func TestDiagnosticsOneActiveCancelAndEmptyBodyContract(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	_, canceller := seedAccount(t, h, "CancelOwner", store.RoleOwner)
	entered := make(chan struct{})
	h.srv.diagnosticGenerate = func(ctx context.Context, _ string, _ diagnostics.Input) (diagnostics.Result, error) {
		close(entered)
		<-ctx.Done()
		return diagnostics.Result{}, ctx.Err()
	}
	assertCodedError(t, h.do(http.MethodPost, "/api/v1/diagnostics", map[string]bool{"unexpected": true}),
		http.StatusBadRequest, "invalid_request")
	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start=%d body=%s", w.Code, w.Body.String())
	}
	var started struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, w, &started)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("generator did not start")
	}
	assertCodedError(t, h.do(http.MethodPost, "/api/v1/diagnostics", nil),
		http.StatusConflict, "diagnostic_in_progress")
	assertCodedError(t, h.do(http.MethodGet, "/api/v1/diagnostics/"+started.Job.ID+"/download", nil),
		http.StatusConflict, "diagnostic_not_ready")

	token, sess, err := h.srv.sessions.create(canceller.ID, canceller.Username, canceller.Role, "192.0.2.11", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
	h.csrf = sess.csrf
	assertCodedError(t, h.do(http.MethodPost, "/api/v1/diagnostics/"+started.Job.ID+"/cancel",
		map[string]bool{"unexpected": true}), http.StatusBadRequest, "invalid_request")
	w = h.do(http.MethodPost, "/api/v1/diagnostics/"+started.Job.ID+"/cancel", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel=%d body=%s", w.Code, w.Body.String())
	}
	waitDiagnosticState(t, h, started.Job.ID, "cancelled")
	assertCodedError(t, h.do(http.MethodPost, "/api/v1/diagnostics/"+started.Job.ID+"/cancel", nil),
		http.StatusConflict, "diagnostic_not_cancellable")
	for _, event := range []string{"diagnostics.cancel_requested", "diagnostics.cancelled"} {
		detail := waitDiagnosticAudit(t, h, event)
		if detail["actor_username"] != canceller.Username {
			t.Fatalf("%s attribution=%v want=%q", event, detail, canceller.Username)
		}
	}
}

func TestDiagnosticsClockRollbackKeepsJobTimestampsMonotonic(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	base := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(base.UnixMilli())
	h.srv.Now = func() time.Time { return time.UnixMilli(clock.Load()) }
	entered, release := make(chan struct{}), make(chan struct{})
	h.srv.diagnosticGenerate = func(ctx context.Context, output string, input diagnostics.Input) (diagnostics.Result, error) {
		close(entered)
		<-release
		return diagnostics.Generate(ctx, output, input)
	}
	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	var body struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, w, &body)
	<-entered
	clock.Store(base.Add(-time.Hour).UnixMilli())
	close(release)
	job := waitDiagnosticState(t, h, body.Job.ID, "completed")
	if job.StartedAt == nil || job.FinishedAt == nil || job.ExpiresAt == nil ||
		*job.StartedAt < job.CreatedAt || *job.FinishedAt < *job.StartedAt ||
		*job.ExpiresAt != *job.FinishedAt+diagnosticRetention.Milliseconds() {
		t.Fatalf("non-monotonic job=%+v", job)
	}
}

func TestDiagnosticsHistoryExpiryAndCloseCleanup(t *testing.T) {
	h := diagnosticHarness(t)
	now := time.Now().UTC()
	m := h.srv.diagnostics
	m.mu.Lock()
	for i := 0; i < diagnosticHistoryLimit+2; i++ {
		id := fmt.Sprintf("%043d", i)
		path := filepath.Join(m.dir, id+".zip")
		if err := os.WriteFile(path, []byte("zip"), 0o600); err != nil {
			m.mu.Unlock()
			t.Fatal(err)
		}
		finished, expires, size := now.UnixMilli(), now.Add(time.Hour).UnixMilli(), int64(3)
		m.jobs[id] = &diagnosticJob{diagnosticJobDTO: diagnosticJobDTO{
			ID: id, State: "completed", Phase: "Bundle ready to download.", ProgressPercent: 100,
			CreatedAt: finished, FinishedAt: &finished, ExpiresAt: &expires, SizeBytes: &size,
		}, path: path, cancel: func() {}}
		m.order = append(m.order, id)
	}
	m.pruneLocked(now)
	if len(m.jobs) != diagnosticHistoryLimit {
		m.mu.Unlock()
		t.Fatalf("history size=%d want=%d", len(m.jobs), diagnosticHistoryLimit)
	}
	expiredID := m.order[0]
	expiredPath := m.jobs[expiredID].path
	expires := now.UnixMilli()
	m.jobs[expiredID].ExpiresAt = &expires
	m.mu.Unlock()
	m.sweep(now)
	if _, _, err := m.download(expiredID, now); !errors.Is(err, errDiagnosticNotFound) {
		t.Fatalf("expired download err=%v", err)
	}
	if _, err := os.Stat(expiredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired file remains: %v", err)
	}
}

func TestDiagnosticsCloseJobsTimesOutThenCleans(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	entered, release := make(chan struct{}), make(chan struct{})
	h.srv.diagnosticGenerate = func(ctx context.Context, _ string, _ diagnostics.Input) (diagnostics.Result, error) {
		close(entered)
		<-release
		return diagnostics.Result{}, ctx.Err()
	}
	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start=%d body=%s", w.Code, w.Body.String())
	}
	<-entered
	if h.srv.CloseJobs(5 * time.Millisecond) {
		t.Fatal("CloseJobs succeeded while generator was still blocked")
	}
	close(release)
	if !h.srv.CloseJobs(3 * time.Second) {
		t.Fatal("CloseJobs did not drain released generator")
	}
	if _, err := os.Stat(h.srv.DiagnosticsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostics directory remains after close: %v", err)
	}
}

func TestDiagnosticsCloseZeroTimeoutReportsIdleAndCleans(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, cancel := context.WithCancel(context.Background())
	m := &diagnosticManager{
		dir: dir, jobs: map[string]*diagnosticJob{}, root: root, cancel: cancel,
	}
	if !m.close(0) {
		t.Fatal("idle diagnostics manager reported undrained with a zero timeout")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostics directory remains after idle close: %v", err)
	}
}

func TestDiagnosticsOwnsOperationLeaseThroughCancelAndTerminalCleanup(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	entered, releaseGenerator := make(chan struct{}), make(chan struct{})
	h.srv.diagnosticGenerate = func(ctx context.Context, _ string,
		_ diagnostics.Input) (diagnostics.Result, error) {
		close(entered)
		<-releaseGenerator
		return diagnostics.Result{}, ctx.Err()
	}

	exclusive, _, err := h.srv.operations.beginExclusive()
	if err != nil {
		t.Fatal(err)
	}
	blocked := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	assertCodedError(t, blocked, http.StatusServiceUnavailable, "restore_in_progress")
	if jobs := h.srv.diagnostics.list(h.srv.now()); len(jobs) != 0 {
		t.Fatalf("restore-blocked diagnostics created jobs: %+v", jobs)
	}
	exclusive()

	started := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	if started.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", started.Code, started.Body.String())
	}
	var body struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, started, &body)
	<-entered
	if got := h.srv.ActiveOperations(); len(got) != 1 || got[0] != "diagnostics" {
		t.Fatalf("active operations = %v", got)
	}
	cancelled := h.do(http.MethodPost, "/api/v1/diagnostics/"+body.Job.ID+"/cancel", nil)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", cancelled.Code, cancelled.Body.String())
	}
	if got := h.srv.ActiveOperations(); len(got) != 1 || got[0] != "diagnostics" {
		t.Fatalf("cancel response released job lease: %v", got)
	}
	close(releaseGenerator)
	if !h.srv.WaitForOperations(2 * time.Second) {
		t.Fatalf("terminal diagnostics did not release lease: %v", h.srv.ActiveOperations())
	}
	waitDiagnosticState(t, h, body.Job.ID, "cancelled")
	events, err := h.db.RecentEvents(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || event.Event == "diagnostics.cancelled"
	}
	if !found {
		t.Fatal("operation lease released before terminal audit")
	}
}

func TestDiagnosticsRejectsTamperedBundleSize(t *testing.T) {
	h := diagnosticHarness(t)
	h.setup()
	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	var body struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, w, &body)
	waitDiagnosticState(t, h, body.Job.ID, "completed")
	path := filepath.Join(h.srv.DiagnosticsDir, body.Job.ID+".zip")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("tampered"))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("tamper write=%v close=%v", writeErr, closeErr)
	}
	assertCodedError(t, h.do(http.MethodGet, "/api/v1/diagnostics/"+body.Job.ID+"/download", nil),
		http.StatusInternalServerError, "diagnostic_download_failed")
}

func TestDiagnosticsFailureAndDirectoryErrorsDoNotLeakPathsOrDeleteUnknownFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "keep-me.txt")
	if err := os.WriteFile(unknown, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.DiagnosticsDir = dir
	if err := h.srv.InitDiagnostics(); err == nil {
		t.Fatal("InitDiagnostics accepted an unowned file")
	}
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "keep" {
		t.Fatalf("unknown file changed: %q %v", got, err)
	}

	h = diagnosticHarness(t)
	h.setup()
	h.srv.diagnosticGenerate = func(_ context.Context, output string, _ diagnostics.Input) (diagnostics.Result, error) {
		return diagnostics.Result{}, fmt.Errorf("failure at secret path %s", output)
	}
	w := h.do(http.MethodPost, "/api/v1/diagnostics", nil)
	var body struct {
		Job diagnosticJobDTO `json:"job"`
	}
	decodeResponse(t, w, &body)
	waitDiagnosticState(t, h, body.Job.ID, "failed")
	w = h.do(http.MethodGet, "/api/v1/diagnostics/"+body.Job.ID, nil)
	if strings.Contains(w.Body.String(), h.srv.DiagnosticsDir) || strings.Contains(w.Body.String(), "secret path") {
		t.Fatalf("failure response leaked internal error/path: %s", w.Body.String())
	}
	for _, id := range []string{"bad!id", strings.Repeat("a", 129)} {
		w = h.do(http.MethodGet, "/api/v1/diagnostics/"+id+"/download", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("invalid id status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func waitDiagnosticState(t *testing.T, h *harness, id, state string) diagnosticJobDTO {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := h.do(http.MethodGet, "/api/v1/diagnostics/"+id, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("job status=%d body=%s", w.Code, w.Body.String())
		}
		var body struct {
			Job diagnosticJobDTO `json:"job"`
		}
		decodeResponse(t, w, &body)
		if body.Job.State == state {
			return body.Job
		}
		if body.Job.State == "failed" || body.Job.State == "cancelled" || body.Job.State == "completed" {
			t.Fatalf("job reached %s, want %s: %+v", body.Job.State, state, body.Job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for diagnostic state %s", state)
	return diagnosticJobDTO{}
}

func waitDiagnosticAudit(t *testing.T, h *harness, event string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var raw string
		err := h.db.SQL().QueryRowContext(context.Background(),
			`SELECT detail_json FROM events WHERE event=? ORDER BY id DESC LIMIT 1`, event).Scan(&raw)
		if err == nil {
			var detail map[string]any
			if err := json.Unmarshal([]byte(raw), &detail); err != nil {
				t.Fatal(err)
			}
			return detail
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for audit event %s", event)
	return nil
}

func ptrInt64(value int64) *int64 { return &value }
