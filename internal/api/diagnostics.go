package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/diagnostics"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	diagnosticHistoryLimit      = 20
	diagnosticRetention         = 15 * time.Minute
	diagnosticCollectionTimeout = 10 * time.Second
	diagnosticAuditTimeout      = 2 * time.Second
)

var (
	errDiagnosticInProgress = errors.New("diagnostic job is already active")
	errDiagnosticNotFound   = errors.New("diagnostic job not found")
	errDiagnosticTerminal   = errors.New("diagnostic job cannot be cancelled")
	errDiagnosticNotReady   = errors.New("diagnostic bundle is not ready")
	errDiagnosticsClosed    = errors.New("diagnostic jobs are closed")
)

type diagnosticJobDTO struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	Phase           string `json:"phase"`
	ProgressPercent int    `json:"progress_percent"`
	CreatedAt       int64  `json:"created_at"`
	StartedAt       *int64 `json:"started_at,omitempty"`
	FinishedAt      *int64 `json:"finished_at,omitempty"`
	ExpiresAt       *int64 `json:"expires_at,omitempty"`
	SizeBytes       *int64 `json:"size_bytes,omitempty"`
	Error           string `json:"error,omitempty"`
}

type diagnosticJob struct {
	diagnosticJobDTO
	path                    string
	actorAdminID            int64
	actorUsername           string
	cancelRequestedAdminID  int64
	cancelRequestedUsername string
	cancelRequested         bool
	cancel                  context.CancelFunc
	complete                func()
}

type diagnosticManager struct {
	mu      sync.Mutex
	dir     string
	server  *Server
	jobs    map[string]*diagnosticJob
	order   []string
	active  string
	root    context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running int
	closed  bool
}

type diagnosticSection struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

func (s *Server) InitDiagnostics() error {
	if s.DiagnosticsDir == "" {
		return errors.New("api: diagnostics directory is empty")
	}
	if s.diagnostics != nil {
		return errors.New("api: diagnostics are already initialized")
	}
	dir := filepath.Clean(s.DiagnosticsDir)
	if !filepath.IsAbs(dir) || filepath.Base(dir) != "diagnostics" {
		return errors.New("api: diagnostics directory must be an absolute diagnostics subdirectory")
	}
	if err := prepareDiagnosticsDir(dir); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.diagnostics = &diagnosticManager{
		dir: dir, server: s, jobs: map[string]*diagnosticJob{}, root: ctx, cancel: cancel,
	}
	return nil
}

func prepareDiagnosticsDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("api: diagnostics path is not a private directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("api: inspect diagnostics directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("api: create diagnostics directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("api: protect diagnostics directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("api: inspect diagnostics directory: %w", err)
	}
	for _, entry := range entries {
		if !ownedDiagnosticFilename(entry.Name()) || entry.IsDir() {
			return fmt.Errorf("api: diagnostics directory contains unowned entry %q", entry.Name())
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("api: clean diagnostics directory: %w", err)
		}
	}
	return nil
}

func ownedDiagnosticFilename(name string) bool {
	if strings.HasPrefix(name, ".oonfeewrt-diagnostics-") && strings.HasSuffix(name, ".tmp") {
		return len(name) > len(".oonfeewrt-diagnostics-.tmp")
	}
	if !strings.HasSuffix(name, ".zip") {
		return false
	}
	id := strings.TrimSuffix(name, ".zip")
	return len(id) == 43 && validDiagnosticID(id)
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, _ *http.Request) {
	if s.diagnostics == nil {
		writeCodedErr(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "diagnostics are unavailable")
		return
	}
	available := s.ControllerLogTail != nil
	gaps := []string{}
	if available {
		if _, foundGaps, err := s.ControllerLogTail(diagnostics.MaxControllerLogInputBytes); err != nil {
			available = false
			gaps = append(gaps, "controller log could not be read")
		} else {
			gaps = append(gaps, foundGaps...)
		}
	} else {
		gaps = append(gaps, "controller log collector is unavailable")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":                    "stored",
		"router_management_calls": false,
		"router_changes":          false,
		"sections": []diagnosticSection{
			{"controller", "Controller", "Version, schema, platform, uptime, health and integrity state."},
			{"devices", "Devices", "Stored router model, target, firmware, kernel and capability evidence."},
			{"coverage", "Coverage", "Stored topology, radio and collection-source state."},
			{"events", "Events", "Bounded general and security-audit event summaries."},
			{"logs", "Controller logs", "A bounded, redacted tail of retained controller JSON logs."},
		},
		"excluded_secret_classes": []string{
			"controller passphrases", "password hashes", "session and CSRF tokens",
			"router credentials", "Wi-Fi keys", "private keys and certificates",
			"raw database and keyring", "client notes and fixed-address assignments",
		},
		"limits": map[string]any{
			"devices": diagnostics.MaxDevices, "sources": diagnostics.MaxSources,
			"events":                      diagnostics.MaxEvents,
			"controller_log_input_bytes":  diagnostics.MaxControllerLogInputBytes,
			"controller_log_output_bytes": diagnostics.MaxControllerLogBytes,
			"archive_bytes":               diagnostics.MaxArchiveBytes,
			"history":                     diagnosticHistoryLimit,
			"retention_seconds":           int64(diagnosticRetention / time.Second),
			"collection_timeout_seconds":  int64(diagnosticCollectionTimeout / time.Second),
		},
		"controller_log": map[string]any{"available": available, "gaps": gaps},
		"jobs":           s.diagnostics.list(s.now()),
	})
}

func (s *Server) handleStartDiagnostics(w http.ResponseWriter, r *http.Request) {
	if s.diagnostics == nil {
		writeCodedErr(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "diagnostics are unavailable")
		return
	}
	if !emptyRequestBody(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	release, ok := s.beginOperation(w, operationDiagnostics)
	if !ok {
		return
	}
	job, err := s.diagnostics.startWithCompletion(sess.adminID, sess.username, s.now(), release)
	if err != nil {
		release()
	}
	switch {
	case errors.Is(err, errDiagnosticInProgress):
		writeCodedErr(w, http.StatusConflict, "diagnostic_in_progress", "a diagnostic job is already active")
	case errors.Is(err, errDiagnosticsClosed):
		writeCodedErr(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "diagnostics are shutting down")
	case err != nil:
		writeCodedErr(w, http.StatusInternalServerError, "diagnostic_start_failed", "could not start diagnostics")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
	}
}

func emptyRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(body) != 0 {
		writeCodedErr(w, http.StatusBadRequest, "invalid_request", "request body must be empty")
		return false
	}
	return true
}

func (s *Server) handleDiagnosticJob(w http.ResponseWriter, r *http.Request) {
	if s.diagnostics == nil {
		writeCodedErr(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "diagnostics are unavailable")
		return
	}
	job, err := s.diagnostics.job(strings.TrimSpace(r.PathValue("id")), s.now())
	if errors.Is(err, errDiagnosticNotFound) {
		writeCodedErr(w, http.StatusNotFound, "diagnostic_not_found", "diagnostic job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (s *Server) handleCancelDiagnostics(w http.ResponseWriter, r *http.Request) {
	if s.diagnostics == nil {
		writeCodedErr(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "diagnostics are unavailable")
		return
	}
	if !emptyRequestBody(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	job, err := s.diagnostics.cancelJob(strings.TrimSpace(r.PathValue("id")), s.now(), sess.adminID, sess.username)
	switch {
	case errors.Is(err, errDiagnosticNotFound):
		writeCodedErr(w, http.StatusNotFound, "diagnostic_not_found", "diagnostic job not found")
	case errors.Is(err, errDiagnosticTerminal):
		writeCodedErr(w, http.StatusConflict, "diagnostic_not_cancellable", "diagnostic job cannot be cancelled")
	default:
		s.auditDiagnostic("diagnostics.cancel_requested", "info", sess.adminID, sess.username, job.ID, job.State)
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
	}
}

func (s *Server) handleDownloadDiagnostics(w http.ResponseWriter, r *http.Request) {
	if s.diagnostics == nil {
		writeCodedErr(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "diagnostics are unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	job, path, err := s.diagnostics.download(id, s.now())
	switch {
	case errors.Is(err, errDiagnosticNotFound):
		writeCodedErr(w, http.StatusNotFound, "diagnostic_not_found", "diagnostic job not found")
		return
	case errors.Is(err, errDiagnosticNotReady):
		writeCodedErr(w, http.StatusConflict, "diagnostic_not_ready", "diagnostic bundle is not ready")
		return
	case err != nil:
		writeCodedErr(w, http.StatusInternalServerError, "diagnostic_download_failed", "could not open diagnostic bundle")
		return
	}
	if path != filepath.Join(s.diagnostics.dir, id+".zip") {
		writeCodedErr(w, http.StatusInternalServerError, "diagnostic_download_failed", "could not open diagnostic bundle")
		return
	}
	lstat, lstatErr := os.Lstat(path)
	if lstatErr != nil || !lstat.Mode().IsRegular() || lstat.Mode()&os.ModeSymlink != 0 {
		writeCodedErr(w, http.StatusInternalServerError, "diagnostic_download_failed", "could not open diagnostic bundle")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "diagnostic_download_failed", "could not open diagnostic bundle")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !os.SameFile(lstat, info) ||
		job.SizeBytes == nil || info.Size() <= 0 || info.Size() > diagnostics.MaxArchiveBytes || info.Size() != *job.SizeBytes {
		writeCodedErr(w, http.StatusInternalServerError, "diagnostic_download_failed", "could not open diagnostic bundle")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="oonfeewrt-diagnostics-%s.zip"`, id))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	written, copyErr := io.Copy(w, f)
	if copyErr == nil && written == info.Size() {
		sess, _ := sessionFrom(r.Context())
		s.auditDiagnostic("diagnostics.downloaded", "info", sess.adminID, sess.username, job.ID, "completed")
	}
}

func (m *diagnosticManager) start(adminID int64, username string, now time.Time) (diagnosticJobDTO, error) {
	return m.startWithCompletion(adminID, username, now, nil)
}

func (m *diagnosticManager) startWithCompletion(adminID int64, username string, now time.Time,
	complete func()) (diagnosticJobDTO, error) {
	id, err := randomToken()
	if err != nil {
		return diagnosticJobDTO{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	if m.closed {
		return diagnosticJobDTO{}, errDiagnosticsClosed
	}
	if m.active != "" {
		return diagnosticJobDTO{}, errDiagnosticInProgress
	}
	ctx, cancel := context.WithCancel(m.root)
	job := &diagnosticJob{diagnosticJobDTO: diagnosticJobDTO{
		ID: id, State: "queued", Phase: "Waiting to collect stored evidence.",
		ProgressPercent: 0, CreatedAt: now.UnixMilli(),
	}, actorAdminID: adminID, actorUsername: username, cancel: cancel, complete: complete}
	m.jobs[id], m.active = job, id
	m.order = append(m.order, id)
	m.running++
	m.wg.Add(1)
	go m.run(ctx, job)
	return job.diagnosticJobDTO, nil
}

func (m *diagnosticManager) run(ctx context.Context, job *diagnosticJob) {
	defer func() {
		m.mu.Lock()
		m.running--
		m.mu.Unlock()
		m.wg.Done()
	}()
	if job.complete != nil {
		defer job.complete()
	}
	m.server.auditDiagnostic("diagnostics.started", "info", job.actorAdminID, job.actorUsername, job.ID, "collecting")
	m.transition(job.ID, "collecting", "Collecting bounded stored evidence.", 20)
	collectCtx, cancel := context.WithTimeout(ctx, diagnosticCollectionTimeout)
	input, err := m.server.collectDiagnosticInput(collectCtx)
	cancel()
	if err != nil {
		m.finishFailure(job, err)
		return
	}
	if err := ctx.Err(); err != nil {
		m.finishFailure(job, err)
		return
	}
	m.transition(job.ID, "generating", "Redacting and packaging the diagnostic bundle.", 65)
	generate := m.server.diagnosticGenerate
	if generate == nil {
		generate = diagnostics.Generate
	}
	result, err := generate(ctx, filepath.Join(m.dir, job.ID+".zip"), input)
	if err != nil {
		m.finishFailure(job, err)
		return
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(result.Path)
		m.finishFailure(job, err)
		return
	}
	if m.server.afterDiagnosticGenerated != nil {
		m.server.afterDiagnosticGenerated(job.ID)
	}
	now := m.server.now()
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		_ = os.Remove(result.Path)
		m.finishFailure(job, err)
		return
	}
	if current := m.jobs[job.ID]; current != nil {
		finished, expires := diagnosticTerminalTimes(current, now)
		size := result.Size
		current.State, current.Phase, current.ProgressPercent = "completed", "Bundle ready to download.", 100
		current.FinishedAt, current.ExpiresAt, current.SizeBytes, current.path = &finished, &expires, &size, result.Path
		m.active = ""
		m.pruneLocked(now)
	}
	m.mu.Unlock()
	m.server.auditDiagnostic("diagnostics.completed", "info", job.actorAdminID, job.actorUsername, job.ID, "completed")
}

func (m *diagnosticManager) finishFailure(job *diagnosticJob, err error) {
	now := m.server.now()
	state, phase, severity, event := "failed", "Diagnostic generation failed.", "error", "diagnostics.failed"
	if errors.Is(err, context.Canceled) {
		state, phase, severity, event = "cancelled", "Diagnostic generation cancelled.", "info", "diagnostics.cancelled"
	}
	auditAdminID, auditUsername := job.actorAdminID, job.actorUsername
	m.mu.Lock()
	if current := m.jobs[job.ID]; current != nil {
		finished, expires := diagnosticTerminalTimes(current, now)
		current.State, current.Phase, current.FinishedAt, current.ExpiresAt = state, phase, &finished, &expires
		if state == "failed" {
			current.Error = "Diagnostic generation failed."
		}
		if state == "cancelled" {
			if current.cancelRequested {
				auditAdminID, auditUsername = current.cancelRequestedAdminID, current.cancelRequestedUsername
			} else {
				auditAdminID, auditUsername = 0, "controller"
			}
		}
		m.active = ""
		m.pruneLocked(now)
	}
	m.mu.Unlock()
	_ = os.Remove(filepath.Join(m.dir, job.ID+".zip"))
	m.server.auditDiagnostic(event, severity, auditAdminID, auditUsername, job.ID, state)
}

func (m *diagnosticManager) transition(id, state, phase string, progress int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return
	}
	job.State, job.Phase, job.ProgressPercent = state, phase, progress
	if job.StartedAt == nil {
		started := m.server.now().UnixMilli()
		if started < job.CreatedAt {
			started = job.CreatedAt
		}
		job.StartedAt = &started
	}
}

func diagnosticTerminalTimes(job *diagnosticJob, now time.Time) (int64, int64) {
	finished := now.UnixMilli()
	floor := job.CreatedAt
	if job.StartedAt != nil && *job.StartedAt > floor {
		floor = *job.StartedAt
	}
	if finished < floor {
		finished = floor
	}
	return finished, finished + diagnosticRetention.Milliseconds()
}

func (m *diagnosticManager) list(now time.Time) []diagnosticJobDTO {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	out := make([]diagnosticJobDTO, 0, len(m.jobs))
	for i := len(m.order) - 1; i >= 0; i-- {
		if job := m.jobs[m.order[i]]; job != nil {
			out = append(out, job.diagnosticJobDTO)
		}
	}
	return out
}

func (m *diagnosticManager) job(id string, now time.Time) (diagnosticJobDTO, error) {
	if !validDiagnosticID(id) {
		return diagnosticJobDTO{}, errDiagnosticNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.jobs[id]
	if job == nil {
		return diagnosticJobDTO{}, errDiagnosticNotFound
	}
	return job.diagnosticJobDTO, nil
}

func (m *diagnosticManager) cancelJob(id string, now time.Time, adminID int64, username string) (diagnosticJobDTO, error) {
	if !validDiagnosticID(id) {
		return diagnosticJobDTO{}, errDiagnosticNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.jobs[id]
	if job == nil {
		return diagnosticJobDTO{}, errDiagnosticNotFound
	}
	if job.State == "completed" || job.State == "failed" || job.State == "cancelled" {
		return diagnosticJobDTO{}, errDiagnosticTerminal
	}
	job.cancelRequestedAdminID, job.cancelRequestedUsername, job.cancelRequested = adminID, username, true
	job.cancel()
	return job.diagnosticJobDTO, nil
}

func (m *diagnosticManager) download(id string, now time.Time) (*diagnosticJob, string, error) {
	if !validDiagnosticID(id) {
		return nil, "", errDiagnosticNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.jobs[id]
	if job == nil {
		return nil, "", errDiagnosticNotFound
	}
	if job.State != "completed" || job.path == "" {
		return nil, "", errDiagnosticNotReady
	}
	copy := *job
	return &copy, job.path, nil
}

func validDiagnosticID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r != '-' && r != '_' && (r < '0' || r > '9') &&
			(r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func (m *diagnosticManager) pruneLocked(now time.Time) {
	kept := m.order[:0]
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil {
			continue
		}
		if job.ExpiresAt != nil && now.UnixMilli() >= *job.ExpiresAt {
			_ = os.Remove(job.path)
			delete(m.jobs, id)
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
	terminalCount := len(m.jobs)
	if m.active != "" {
		terminalCount--
	}
	for terminalCount > diagnosticHistoryLimit {
		id := m.order[0]
		job := m.jobs[id]
		if id == m.active {
			break
		}
		_ = os.Remove(job.path)
		delete(m.jobs, id)
		m.order = m.order[1:]
		terminalCount--
	}
}

func (m *diagnosticManager) close(timeout time.Duration) bool {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
		for _, job := range m.jobs {
			job.cancel()
		}
	}
	idle := m.running == 0
	m.mu.Unlock()
	if !idle {
		if timeout <= 0 {
			return false
		}
		done := make(chan struct{})
		go func() { m.wg.Wait(); close(done) }()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			return false
		}
	}
	m.mu.Lock()
	for _, job := range m.jobs {
		_ = os.Remove(job.path)
	}
	m.jobs, m.order, m.active = map[string]*diagnosticJob{}, nil, ""
	m.mu.Unlock()
	_ = os.Remove(m.dir)
	return true
}

func (m *diagnosticManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
}

func (s *Server) collectDiagnosticInput(ctx context.Context) (diagnostics.Input, error) {
	now := s.now().UTC()
	controller, err := s.Store.DiagnosticController(ctx)
	if err != nil {
		return diagnostics.Input{}, err
	}
	devices, devicesTruncated, err := s.Store.DiagnosticDevices(ctx, diagnostics.MaxDevices)
	if err != nil {
		return diagnostics.Input{}, err
	}
	sources, sourcesTruncated, err := s.Store.DiagnosticSources(ctx, diagnostics.MaxSources)
	if err != nil {
		return diagnostics.Input{}, err
	}
	general, generalTruncated, err := s.Store.DiagnosticEvents(ctx, "general", diagnostics.MaxEvents/2+1)
	if err != nil {
		return diagnostics.Input{}, err
	}
	audit, auditTruncated, err := s.Store.DiagnosticEvents(ctx, "audit", diagnostics.MaxEvents/2+1)
	if err != nil {
		return diagnostics.Input{}, err
	}
	if len(general) > diagnostics.MaxEvents/2 {
		general = general[:diagnostics.MaxEvents/2]
		generalTruncated = true
	}
	if len(audit) > diagnostics.MaxEvents/2 {
		audit = audit[:diagnostics.MaxEvents/2]
		auditTruncated = true
	}
	if generalTruncated {
		controller.Gaps = append(controller.Gaps, "general event summaries were truncated")
	}
	if auditTruncated {
		controller.Gaps = append(controller.Gaps, "audit event summaries were truncated")
	}
	if devicesTruncated {
		controller.Gaps = append(controller.Gaps, "device evidence was truncated")
	}
	if sourcesTruncated {
		controller.Gaps = append(controller.Gaps, "source evidence was truncated")
	}

	out := diagnostics.Input{GeneratedAt: now, Controller: diagnostics.ControllerSnapshot{
		Version: s.ControllerVersion, Schema: controller.Schema,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		UptimeSeconds:  diagnosticUptime(now, s.ControllerStartedAt),
		Health:         controller.Health,
		MigrationState: controller.MigrationState,
		IntegrityState: controller.IntegrityState,
		CollectedAt:    now,
		Gaps:           controller.Gaps,
	}}
	if out.Controller.Version == "" {
		out.Controller.Version = "dev"
	}
	deviceIDs := make(map[int64]string, len(devices))
	for _, device := range devices {
		deviceIDs[device.ID] = device.Identifier
		out.Devices = append(out.Devices, diagnostics.DeviceSnapshot{
			Identifier: device.Identifier, Name: device.Name, Model: device.Model,
			Target: device.Target, Firmware: device.Firmware, Kernel: device.Kernel,
			PackageManager: device.PackageManager, CapabilityState: device.CapabilityState,
			LastObservedAt: device.LastObservedAt, Gaps: device.Gaps,
		})
		for _, value := range []string{device.Identifier, device.Name, device.Host} {
			if value != "" {
				out.Identifiers = append(out.Identifiers, diagnostics.Identifier{Kind: "device", Value: value})
			}
		}
	}
	for _, source := range sources {
		out.Sources = append(out.Sources, diagnostics.SourceSnapshot{
			Kind: source.Kind, DeviceIdentifier: source.DeviceIdentifier,
			State: source.State, Detail: source.Detail, ObservedAt: source.ObservedAt,
		})
	}
	for _, event := range append(general, audit...) {
		identifier := ""
		if event.DeviceID != nil {
			identifier = deviceIDs[*event.DeviceID]
		}
		message, _ := json.Marshal(map[string]any{
			"event": event.Event, "source": event.Source, "action": event.Action,
		})
		if len(message) > diagnostics.MaxFreeTextBytes {
			message = message[:diagnostics.MaxFreeTextBytes]
		}
		out.Events = append(out.Events, diagnostics.EventSnapshot{
			At: time.Unix(event.TS, 0).UTC(), Category: event.Category,
			Severity: event.Severity, DeviceIdentifier: identifier, Message: strings.ToValidUTF8(string(message), "?"),
		})
	}
	remaining := diagnostics.MaxIdentifiers - len(out.Identifiers)
	if remaining > 0 {
		identifiers, truncated, err := s.Store.DiagnosticIdentifiers(ctx, remaining)
		if err != nil {
			return diagnostics.Input{}, err
		}
		for _, identifier := range identifiers {
			out.Identifiers = append(out.Identifiers, diagnostics.Identifier{Kind: identifier.Kind, Value: identifier.Value})
		}
		if truncated {
			out.Controller.Gaps = append(out.Controller.Gaps, "identifier redaction inventory was truncated")
		}
	}
	if s.ControllerLogTail == nil {
		out.Controller.Gaps = append(out.Controller.Gaps, "controller log collector is unavailable")
	} else if logJSONL, gaps, err := s.ControllerLogTail(diagnostics.MaxControllerLogInputBytes); err != nil {
		out.Controller.Gaps = append(out.Controller.Gaps, "controller log could not be read")
	} else {
		var omitted bool
		out.ControllerLogJSONL, omitted = sanitizeControllerLog(logJSONL)
		if omitted {
			out.Controller.Gaps = append(out.Controller.Gaps, "invalid or sensitive controller log fields were omitted")
		}
		out.Controller.Gaps = append(out.Controller.Gaps, gaps...)
	}
	return out, nil
}

func sanitizeControllerLog(raw []byte) ([]byte, bool) {
	parts := make([][]byte, 0)
	total, omitted := 0, false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var value any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			omitted = true
			continue
		}
		scrubDiagnosticLogValue(value)
		encoded, err := json.Marshal(value)
		if err != nil {
			omitted = true
			continue
		}
		encoded = append(encoded, '\n')
		parts = append(parts, encoded)
		total += len(encoded)
		for total > diagnostics.MaxControllerLogInputBytes && len(parts) > 0 {
			total -= len(parts[0])
			parts = parts[1:]
			omitted = true
		}
	}
	return bytes.Join(parts, nil), omitted
}

func scrubDiagnosticLogValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if sensitiveDiagnosticLogKey(key) {
				current[key] = "[redacted]"
				continue
			}
			scrubDiagnosticLogValue(child)
		}
	case []any:
		for _, child := range current {
			scrubDiagnosticLogValue(child)
		}
	}
}

func sensitiveDiagnosticLogKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{
		"password", "passphrase", "credential", "authorization", "cookie",
		"csrf", "token", "secret", "private_key", "wifi_key",
	} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	switch key {
	case "user", "username", "login", "host", "hostname", "name", "device", "client",
		"ssid", "ssids", "site", "sites", "network", "networks",
		"group", "groups", "mesh", "meshes", "mesh_id", "wlan", "wlans",
		"zone", "zones", "path", "data_dir", "sha256", "hash", "fingerprint",
		"cert", "certificate":
		return true
	}
	return strings.HasSuffix(key, "_name") || strings.HasSuffix(key, "_path") ||
		strings.HasSuffix(key, "_hash") || strings.HasSuffix(key, "_fingerprint")
}

func diagnosticUptime(now, started time.Time) int64 {
	if started.IsZero() || now.Before(started) {
		return 0
	}
	return int64(now.Sub(started) / time.Second)
}

func (s *Server) auditDiagnostic(event, severity string, adminID int64, username, jobID, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticAuditTimeout)
	defer cancel()
	_ = s.Store.LogEvent(ctx, store.Event{Category: "audit", Severity: severity, Event: event,
		Detail: map[string]any{
			"job_id": jobID, "actor_admin_id": adminID,
			"actor_username": username, "state": state,
		}})
}
