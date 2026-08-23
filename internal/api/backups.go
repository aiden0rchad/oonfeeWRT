package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	backupExportPlanID       = "controller-backup-export-v1"
	backupHistoryLimit       = 5
	backupRetention          = 15 * time.Minute
	backupExportTimeout      = 30 * time.Minute
	backupAuditTimeout       = 2 * time.Second
	minExportPassphraseRunes = 16
	maxExportPassphraseBytes = 4096
)

var (
	errBackupInProgress = errors.New("controller backup export is already active")
	errBackupNotFound   = errors.New("controller backup export not found")
	errBackupTerminal   = errors.New("controller backup export cannot be cancelled")
	errBackupNotReady   = errors.New("controller backup artifact is not ready")
	errBackupsClosed    = errors.New("controller backup exports are closed")
	errBackupRetention  = errors.New("previous controller backup artifacts could not be expired")
)

type backupJobDTO struct {
	ID                string `json:"id"`
	State             string `json:"state"`
	Phase             string `json:"phase"`
	ProgressPercent   int    `json:"progress_percent"`
	CreatedAt         int64  `json:"created_at"`
	StartedAt         *int64 `json:"started_at,omitempty"`
	FinishedAt        *int64 `json:"finished_at,omitempty"`
	ExpiresAt         *int64 `json:"expires_at,omitempty"`
	SizeBytes         *int64 `json:"size_bytes,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
	SchemaVersion     *int   `json:"schema_version,omitempty"`
	ControllerVersion string `json:"controller_version,omitempty"`
	Error             string `json:"error,omitempty"`
}

type backupJob struct {
	backupJobDTO
	path                    string
	actorAdminID            int64
	actorUsername           string
	cancelRequestedAdminID  int64
	cancelRequestedUsername string
	cancelRequested         bool
	cancel                  context.CancelFunc
	complete                func()
}

type backupManager struct {
	mu      sync.Mutex
	dir     string
	server  *Server
	jobs    map[string]*backupJob
	order   []string
	active  string
	root    context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running int
	closed  bool
}

type backupDescriptor struct {
	PlanID        string   `json:"plan_id"`
	Format        string   `json:"format"`
	FormatVersion int      `json:"format_version"`
	FileExtension string   `json:"file_extension"`
	Snapshot      string   `json:"snapshot"`
	Encryption    string   `json:"encryption"`
	Includes      []string `json:"includes"`
	Excludes      []string `json:"excludes"`
}

type backupDisclosure struct {
	RouterManagementCalls       bool   `json:"router_management_calls"`
	RouterChanges               bool   `json:"router_changes"`
	AutomaticRouterApply        bool   `json:"automatic_router_apply"`
	SeparateExportPassphrase    bool   `json:"separate_export_passphrase"`
	ExportPassphraseRecoverable bool   `json:"export_passphrase_recoverable"`
	Summary                     string `json:"summary"`
}

type backupLimits struct {
	History                       int   `json:"history"`
	RetentionSeconds              int64 `json:"retention_seconds"`
	ExportTimeoutSeconds          int64 `json:"export_timeout_seconds"`
	MaxDatabaseBytes              int64 `json:"max_database_bytes"`
	MaxArtifactBytes              int64 `json:"max_artifact_bytes"`
	MinExportPassphraseCharacters int   `json:"min_export_passphrase_characters"`
	MaxExportPassphraseBytes      int   `json:"max_export_passphrase_bytes"`
}

type backupsResponse struct {
	Descriptor backupDescriptor `json:"descriptor"`
	Disclosure backupDisclosure `json:"disclosure"`
	Limits     backupLimits     `json:"limits"`
	Jobs       []backupJobDTO   `json:"jobs"`
}

type backupJobResponse struct {
	Job backupJobDTO `json:"job"`
}

// secretBytes owns the request copy of a secret so handlers can clear it.
// encoding/json passes a mutable token buffer to UnmarshalJSON; clearing it
// also removes the quoted value from the decoder's retained input buffer.
type secretBytes []byte

func (s *secretBytes) UnmarshalJSON(data []byte) error {
	defer clear(data)
	plain, err := decodeJSONString(data, maxExportPassphraseBytes)
	if err != nil {
		return err
	}
	clear(*s)
	*s = plain
	return nil
}

type startBackupRequest struct {
	PlanID                      string      `json:"plan_id"`
	AcknowledgeSensitiveContent bool        `json:"acknowledge_sensitive_content"`
	ExportPassphrase            secretBytes `json:"export_passphrase"`
	ConfirmExportPassphrase     secretBytes `json:"confirm_export_passphrase"`
}

type backupCreateFunc func(context.Context, string, string, *secrets.Keeper,
	[]byte, portablebackup.Metadata) (portablebackup.Result, error)

func (s *Server) InitBackups() error {
	if s.BackupsDir == "" {
		return errors.New("api: backup directory is empty")
	}
	if s.backups != nil {
		return errors.New("api: backups are already initialized")
	}
	dir := filepath.Clean(s.BackupsDir)
	if !filepath.IsAbs(dir) || filepath.Base(dir) != "backups" {
		return errors.New("api: backup directory must be an absolute backups subdirectory")
	}
	if err := prepareBackupsDir(dir); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.backups = &backupManager{
		dir: dir, server: s, jobs: map[string]*backupJob{}, root: ctx, cancel: cancel,
	}
	return nil
}

func prepareBackupsDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("api: backup path is not a private directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("api: inspect backup directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("api: create backup directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("api: protect backup directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("api: inspect backup directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !ownedBackupFilename(entry.Name()) {
			return fmt.Errorf("api: backup directory contains unowned entry %q", entry.Name())
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("api: clean backup directory: %w", err)
		}
	}
	return nil
}

func ownedBackupFilename(name string) bool {
	if strings.HasSuffix(name, ".oowrtbak") {
		return validBackupID(strings.TrimSuffix(name, ".oowrtbak"))
	}
	if strings.HasSuffix(name, ".snapshot.db") {
		return validBackupID(strings.TrimSuffix(name, ".snapshot.db"))
	}
	for _, part := range []struct {
		prefix, suffix string
		hexLen         int
	}{
		{".oonfeewrt-backup-", ".db.tmp", 32},
		{".oonfeewrt-portable-", ".tmp", 32},
	} {
		if strings.HasPrefix(name, part.prefix) && strings.HasSuffix(name, part.suffix) {
			middle := strings.TrimSuffix(strings.TrimPrefix(name, part.prefix), part.suffix)
			return len(middle) == part.hexLen && validLowerHex(middle)
		}
	}
	return false
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if !requireBackupTransport(w, r) || !s.backupsAvailable(w) {
		return
	}
	writeJSON(w, http.StatusOK, backupsResponse{
		Descriptor: backupDescriptor{
			PlanID: backupExportPlanID, Format: "oonfeewrt-portable-backup",
			FormatVersion: 1, FileExtension: ".oowrtbak",
			Snapshot:   "Online, transactionally consistent SQLite snapshot.",
			Encryption: "XChaCha20-Poly1305 artifact; the controller data key is wrapped by an Argon2id key derived from the separate export passphrase.",
			Includes: []string{
				"Controller database: accounts and password hashes, settings, desired network and WLAN configuration, inventory, telemetry and events.",
				"Saved device and Wi-Fi credentials in their controller-encrypted database form.",
				"Portable controller data key wrapped by the separate export passphrase.",
			},
			Excludes: []string{
				"Controller runtime passphrase, active browser sessions and CSRF tokens.",
				"Router firmware, packages, files, live router reads and controller log files.",
				"Other backup and diagnostic artifacts.",
			},
		},
		Disclosure: backupDisclosure{
			RouterManagementCalls: false, RouterChanges: false, AutomaticRouterApply: false,
			SeparateExportPassphrase: true, ExportPassphraseRecoverable: false,
			Summary: "Anyone with this file and its export passphrase can recover the included controller state and saved credentials. The controller cannot recover a lost export passphrase.",
		},
		Limits: backupLimits{
			History: backupHistoryLimit, RetentionSeconds: int64(backupRetention / time.Second),
			ExportTimeoutSeconds:          int64(backupExportTimeout / time.Second),
			MaxDatabaseBytes:              controllerPortableDatabaseMaxBytes,
			MaxArtifactBytes:              controllerPortableArtifactMaxBytes,
			MinExportPassphraseCharacters: minExportPassphraseRunes,
			MaxExportPassphraseBytes:      maxExportPassphraseBytes,
		},
		Jobs: s.backups.list(s.now()),
	})
}

func (s *Server) handleStartBackup(w http.ResponseWriter, r *http.Request) {
	if !requireBackupTransport(w, r) || !s.backupsAvailable(w) {
		return
	}
	var req startBackupRequest
	if !decodeJSON(w, r, &req) {
		clear(req.ExportPassphrase)
		clear(req.ConfirmExportPassphrase)
		return
	}
	defer clear(req.ExportPassphrase)
	defer clear(req.ConfirmExportPassphrase)
	if req.PlanID != backupExportPlanID {
		writeCodedErr(w, http.StatusConflict, "backup_plan_changed", "the controller backup export plan changed; review it again")
		return
	}
	if !req.AcknowledgeSensitiveContent {
		writeCodedErr(w, http.StatusBadRequest, "backup_acknowledgement_required", "acknowledge_sensitive_content must be true")
		return
	}
	if err := validateExportPassphrase(req.ExportPassphrase); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "invalid_export_passphrase", err.Error())
		return
	}
	if len(req.ExportPassphrase) != len(req.ConfirmExportPassphrase) ||
		subtle.ConstantTimeCompare(req.ExportPassphrase, req.ConfirmExportPassphrase) != 1 {
		writeCodedErr(w, http.StatusBadRequest, "export_passphrase_mismatch", "export passphrase confirmation does not match")
		return
	}
	sess, _ := sessionFrom(r.Context())
	releaseHash, ok := s.beginBackupHashSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "backup_capacity_busy",
			"controller password work is busy; retry the backup export shortly")
		return
	}
	release, ok := s.beginOperation(w, operationBackup)
	if !ok {
		releaseHash()
		return
	}
	complete := func() {
		release()
		releaseHash()
	}
	job, err := s.backups.start(sess.adminID, sess.username, s.now(), req.ExportPassphrase, complete)
	if err != nil {
		complete()
	}
	switch {
	case errors.Is(err, errBackupInProgress):
		writeCodedErr(w, http.StatusConflict, "backup_in_progress", "a controller backup export is already active")
	case errors.Is(err, errBackupsClosed):
		writeCodedErr(w, http.StatusServiceUnavailable, "backups_unavailable", "controller backup exports are shutting down")
	case errors.Is(err, errBackupRetention):
		writeCodedErr(w, http.StatusServiceUnavailable, "backup_retention_blocked", "a previous backup artifact could not be safely expired")
	case err != nil:
		writeCodedErr(w, http.StatusInternalServerError, "backup_start_failed", "could not start controller backup export")
	default:
		writeJSON(w, http.StatusAccepted, backupJobResponse{Job: job})
	}
}

func (s *Server) beginBackupHashSlot() (func(), bool) {
	if s == nil || s.hashing == nil {
		return nil, false
	}
	select {
	case s.hashing <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.hashing }) }, true
	default:
		return nil, false
	}
}

func (s *Server) handleBackupJob(w http.ResponseWriter, r *http.Request) {
	if !requireBackupTransport(w, r) || !s.backupsAvailable(w) {
		return
	}
	job, err := s.backups.job(strings.TrimSpace(r.PathValue("id")), s.now())
	if errors.Is(err, errBackupNotFound) {
		writeCodedErr(w, http.StatusNotFound, "backup_not_found", "controller backup export not found")
		return
	}
	writeJSON(w, http.StatusOK, backupJobResponse{Job: job})
}

func (s *Server) handleCancelBackup(w http.ResponseWriter, r *http.Request) {
	if !requireBackupTransport(w, r) || !s.backupsAvailable(w) || !emptyRequestBody(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	job, err := s.backups.cancelJob(strings.TrimSpace(r.PathValue("id")), s.now(), sess.adminID, sess.username)
	switch {
	case errors.Is(err, errBackupNotFound):
		writeCodedErr(w, http.StatusNotFound, "backup_not_found", "controller backup export not found")
	case errors.Is(err, errBackupTerminal):
		writeCodedErr(w, http.StatusConflict, "backup_not_cancellable", "controller backup export cannot be cancelled")
	default:
		s.auditBackup("backup.export_cancel_requested", "info", sess.adminID, sess.username, job.ID, job.State)
		writeJSON(w, http.StatusOK, backupJobResponse{Job: job})
	}
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if !requireBackupTransport(w, r) || !s.backupsAvailable(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	job, path, err := s.backups.download(id, s.now())
	switch {
	case errors.Is(err, errBackupNotFound):
		writeCodedErr(w, http.StatusNotFound, "backup_not_found", "controller backup export not found")
		return
	case errors.Is(err, errBackupNotReady):
		writeCodedErr(w, http.StatusConflict, "backup_not_ready", "controller backup artifact is not ready")
		return
	case err != nil:
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "could not open controller backup artifact")
		return
	}
	if path != filepath.Join(s.backups.dir, id+".oowrtbak") {
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "could not open controller backup artifact")
		return
	}
	lstat, lstatErr := os.Lstat(path)
	if lstatErr != nil || !lstat.Mode().IsRegular() || lstat.Mode()&os.ModeSymlink != 0 || lstat.Mode().Perm() != 0o600 {
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "could not open controller backup artifact")
		return
	}
	if s.beforeBackupDownloadOpen != nil {
		s.beforeBackupDownloadOpen(id)
	}
	f, err := os.Open(path)
	if err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "could not open controller backup artifact")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !os.SameFile(lstat, info) ||
		job.SizeBytes == nil || info.Size() <= 0 || info.Size() != *job.SizeBytes || !validSHA256Hex(job.SHA256) {
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "could not open controller backup artifact")
		return
	}
	digest, err := hashBackupFile(r.Context(), f, info.Size())
	if err != nil || subtle.ConstantTimeCompare([]byte(digest), []byte(job.SHA256)) != 1 {
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "controller backup artifact failed its integrity check")
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "backup_download_failed", "could not open controller backup artifact")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="oonfeewrt-controller-backup-%s.oowrtbak"`, id))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("ETag", `"sha256-`+job.SHA256+`"`)
	w.WriteHeader(http.StatusOK)
	written, copyErr := io.Copy(w, f)
	if copyErr == nil && written == info.Size() {
		sess, _ := sessionFrom(r.Context())
		s.auditBackup("backup.export_downloaded", "info", sess.adminID, sess.username, job.ID, "completed")
	}
}

func (s *Server) backupsAvailable(w http.ResponseWriter) bool {
	if s.backups != nil {
		return true
	}
	writeCodedErr(w, http.StatusServiceUnavailable, "backups_unavailable", "controller backup exports are unavailable")
	return false
}

func requireBackupTransport(w http.ResponseWriter, r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer, err := netip.ParseAddr(clientAddr(r))
	if err == nil && peer.IsLoopback() {
		return true
	}
	writeCodedErr(w, http.StatusUpgradeRequired, "secure_transport_required",
		"controller backup exports require TLS or a direct loopback connection")
	return false
}

func validateExportPassphrase(passphrase []byte) error {
	if len(passphrase) == 0 || len(passphrase) > maxExportPassphraseBytes || !utf8.Valid(passphrase) {
		return errors.New("export passphrase must be valid UTF-8 and no more than 4096 bytes")
	}
	if utf8.RuneCount(passphrase) < minExportPassphraseRunes {
		return errors.New("export passphrase must be at least 16 characters")
	}
	return nil
}

func decodeJSONString(data []byte, limit int) ([]byte, error) {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return nil, errors.New("secret must be a JSON string")
	}
	out := make([]byte, 0, len(data)-2)
	for i := 1; i < len(data)-1; {
		if len(out) > limit {
			clear(out)
			return nil, errors.New("secret is too long")
		}
		if data[i] != '\\' {
			if data[i] < 0x20 {
				clear(out)
				return nil, errors.New("secret contains an invalid control character")
			}
			out = append(out, data[i])
			i++
			continue
		}
		i++
		if i >= len(data)-1 {
			clear(out)
			return nil, errors.New("secret contains an incomplete escape")
		}
		switch data[i] {
		case '"', '\\', '/':
			out = append(out, data[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, consumed, err := decodeJSONUnicode(data[i+1 : len(data)-1])
			if err != nil {
				clear(out)
				return nil, err
			}
			out = utf8.AppendRune(out, r)
			i += consumed
		default:
			clear(out)
			return nil, errors.New("secret contains an invalid escape")
		}
		i++
	}
	if len(out) > limit || !utf8.Valid(out) {
		clear(out)
		return nil, errors.New("secret is too long or is not valid UTF-8")
	}
	return out, nil
}

func decodeJSONUnicode(data []byte) (rune, int, error) {
	first, ok := decodeHex4(data)
	if !ok {
		return 0, 0, errors.New("secret contains an invalid Unicode escape")
	}
	if first < 0xd800 || first > 0xdfff {
		return rune(first), 4, nil
	}
	if first > 0xdbff || len(data) < 10 || data[4] != '\\' || data[5] != 'u' {
		return 0, 0, errors.New("secret contains an invalid Unicode surrogate")
	}
	second, ok := decodeHex4(data[6:])
	if !ok || second < 0xdc00 || second > 0xdfff {
		return 0, 0, errors.New("secret contains an invalid Unicode surrogate")
	}
	return rune(0x10000 + (first-0xd800)<<10 + second - 0xdc00), 10, nil
}

func decodeHex4(data []byte) (uint32, bool) {
	if len(data) < 4 {
		return 0, false
	}
	var out uint32
	for _, c := range data[:4] {
		out <<= 4
		switch {
		case c >= '0' && c <= '9':
			out += uint32(c - '0')
		case c >= 'a' && c <= 'f':
			out += uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			out += uint32(c-'A') + 10
		default:
			return 0, false
		}
	}
	return out, true
}

func (m *backupManager) start(adminID int64, username string, now time.Time,
	passphrase []byte, complete func()) (backupJobDTO, error) {
	id, err := randomToken()
	if err != nil {
		return backupJobDTO{}, err
	}
	ownedPassphrase := append([]byte(nil), passphrase...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	if m.closed {
		clear(ownedPassphrase)
		return backupJobDTO{}, errBackupsClosed
	}
	if m.active != "" {
		clear(ownedPassphrase)
		return backupJobDTO{}, errBackupInProgress
	}
	if !m.makeHistoryRoomLocked() {
		clear(ownedPassphrase)
		return backupJobDTO{}, errBackupRetention
	}
	ctx, cancel := context.WithTimeout(m.root, backupExportTimeout)
	job := &backupJob{backupJobDTO: backupJobDTO{
		ID: id, State: "queued", Phase: "Waiting to create an online controller snapshot.",
		ProgressPercent: 0, CreatedAt: now.UnixMilli(),
	}, actorAdminID: adminID, actorUsername: username, cancel: cancel, complete: complete}
	m.jobs[id], m.active = job, id
	m.order = append(m.order, id)
	m.running++
	m.wg.Add(1)
	go m.run(ctx, job, ownedPassphrase)
	return job.backupJobDTO, nil
}

func (m *backupManager) makeHistoryRoomLocked() bool {
	for len(m.jobs) >= backupHistoryLimit {
		removed := false
		for i, id := range m.order {
			job := m.jobs[id]
			if job == nil || id == m.active {
				continue
			}
			if err := os.Remove(job.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
			delete(m.jobs, id)
			m.order = append(m.order[:i], m.order[i+1:]...)
			removed = true
			break
		}
		if !removed {
			return false
		}
	}
	return true
}

func (m *backupManager) run(ctx context.Context, job *backupJob, passphrase []byte) {
	defer func() {
		m.mu.Lock()
		m.running--
		m.mu.Unlock()
		m.wg.Done()
	}()
	if job.complete != nil {
		defer job.complete()
	}
	defer clear(passphrase)
	m.server.auditBackup("backup.export_started", "info", job.actorAdminID, job.actorUsername, job.ID, "snapshotting")
	m.transition(job.ID, "snapshotting", "Creating a transactionally consistent controller snapshot.", 10)
	controller, err := m.server.Store.DiagnosticController(ctx)
	if err != nil {
		m.finishFailure(job, err)
		return
	}
	snapshotPath := filepath.Join(m.dir, job.ID+".snapshot.db")
	artifactPath := filepath.Join(m.dir, job.ID+".oowrtbak")
	if err := m.server.Store.BackupTo(ctx, snapshotPath); err != nil {
		m.finishFailure(job, err)
		return
	}
	if m.server.afterBackupSnapshot != nil {
		m.server.afterBackupSnapshot(job.ID)
	}
	if info, statErr := os.Lstat(snapshotPath); statErr != nil || !info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 || info.Size() > controllerPortableDatabaseMaxBytes {
		cleanupErr := os.Remove(snapshotPath)
		m.finishFailure(job, errors.Join(
			errors.New("controller database exceeds the portable backup limit"), statErr, cleanupErr))
		return
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := os.Remove(snapshotPath)
		m.finishFailure(job, errors.Join(err, cleanupErr))
		return
	}
	m.transition(job.ID, "encrypting", "Encrypting the portable controller backup.", 55)
	create := m.server.backupCreate
	if create == nil {
		create = portablebackup.Create
	}
	result, createErr := create(ctx, artifactPath, snapshotPath, m.server.Keys, passphrase,
		portablebackup.Metadata{
			ControllerVersion: m.server.ControllerVersion,
			SchemaVersion:     controller.Schema, CreatedAt: time.UnixMilli(job.CreatedAt).UTC(),
		})
	cleanupErr := os.Remove(snapshotPath)
	if createErr != nil || cleanupErr != nil {
		_ = os.Remove(artifactPath)
		m.finishFailure(job, errors.Join(createErr, cleanupErr))
		return
	}
	if m.server.afterBackupCreated != nil {
		m.server.afterBackupCreated(job.ID)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(artifactPath)
		m.finishFailure(job, err)
		return
	}
	if result.Path != artifactPath || result.Size <= 0 || result.Size > controllerPortableArtifactMaxBytes ||
		!validSHA256Hex(result.SHA256) ||
		result.Manifest.SchemaVersion != controller.Schema || result.Manifest.ControllerVersion != m.server.ControllerVersion {
		_ = os.Remove(artifactPath)
		m.finishFailure(job, errors.New("portable backup result metadata is inconsistent"))
		return
	}
	artifactInfo, artifactErr := os.Lstat(artifactPath)
	if artifactErr != nil || !artifactInfo.Mode().IsRegular() || artifactInfo.Mode()&os.ModeSymlink != 0 ||
		artifactInfo.Mode().Perm() != 0o600 || artifactInfo.Size() != result.Size ||
		artifactInfo.Size() > controllerPortableArtifactMaxBytes {
		_ = os.Remove(artifactPath)
		m.finishFailure(job, errors.Join(errors.New("portable backup artifact exceeds the controller limit"), artifactErr))
		return
	}
	now := m.server.now()
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		_ = os.Remove(artifactPath)
		m.finishFailure(job, err)
		return
	}
	if current := m.jobs[job.ID]; current != nil {
		finished, expires := backupTerminalTimes(current, now)
		size, schema := result.Size, result.Manifest.SchemaVersion
		current.State, current.Phase, current.ProgressPercent = "completed", "Encrypted backup ready to download.", 100
		current.FinishedAt, current.ExpiresAt = &finished, &expires
		current.SizeBytes, current.SHA256, current.SchemaVersion = &size, result.SHA256, &schema
		current.ControllerVersion, current.path = result.Manifest.ControllerVersion, result.Path
		m.active = ""
		m.pruneLocked(now)
	}
	m.mu.Unlock()
	m.server.auditBackup("backup.export_completed", "info", job.actorAdminID, job.actorUsername, job.ID, "completed")
}

func (m *backupManager) finishFailure(job *backupJob, cause error) {
	now := m.server.now()
	state, phase, severity, event := "failed", "Controller backup export failed.", "error", "backup.export_failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		state, phase, severity, event = "cancelled", "Controller backup export cancelled.", "info", "backup.export_cancelled"
	}
	auditAdminID, auditUsername := job.actorAdminID, job.actorUsername
	m.mu.Lock()
	if current := m.jobs[job.ID]; current != nil {
		finished, expires := backupTerminalTimes(current, now)
		current.State, current.Phase, current.FinishedAt, current.ExpiresAt = state, phase, &finished, &expires
		if state == "failed" {
			current.Error = "Controller backup export failed."
		}
		if state == "cancelled" && current.cancelRequested {
			auditAdminID, auditUsername = current.cancelRequestedAdminID, current.cancelRequestedUsername
		}
		m.active = ""
		m.pruneLocked(now)
	}
	m.mu.Unlock()
	_ = os.Remove(filepath.Join(m.dir, job.ID+".snapshot.db"))
	_ = os.Remove(filepath.Join(m.dir, job.ID+".oowrtbak"))
	m.server.auditBackup(event, severity, auditAdminID, auditUsername, job.ID, state)
}

func (m *backupManager) transition(id, state, phase string, progress int) {
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

func backupTerminalTimes(job *backupJob, now time.Time) (int64, int64) {
	finished := now.UnixMilli()
	floor := job.CreatedAt
	if job.StartedAt != nil && *job.StartedAt > floor {
		floor = *job.StartedAt
	}
	if finished < floor {
		finished = floor
	}
	return finished, finished + backupRetention.Milliseconds()
}

func (m *backupManager) list(now time.Time) []backupJobDTO {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	out := make([]backupJobDTO, 0, len(m.jobs))
	for i := len(m.order) - 1; i >= 0; i-- {
		if job := m.jobs[m.order[i]]; job != nil {
			out = append(out, job.backupJobDTO)
		}
	}
	return out
}

func (m *backupManager) job(id string, now time.Time) (backupJobDTO, error) {
	if !validBackupID(id) {
		return backupJobDTO{}, errBackupNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.jobs[id]
	if job == nil {
		return backupJobDTO{}, errBackupNotFound
	}
	return job.backupJobDTO, nil
}

func (m *backupManager) cancelJob(id string, now time.Time, adminID int64, username string) (backupJobDTO, error) {
	if !validBackupID(id) {
		return backupJobDTO{}, errBackupNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.jobs[id]
	if job == nil {
		return backupJobDTO{}, errBackupNotFound
	}
	if job.State == "completed" || job.State == "failed" || job.State == "cancelled" {
		return backupJobDTO{}, errBackupTerminal
	}
	job.cancelRequestedAdminID, job.cancelRequestedUsername, job.cancelRequested = adminID, username, true
	job.cancel()
	return job.backupJobDTO, nil
}

func (m *backupManager) download(id string, now time.Time) (*backupJob, string, error) {
	if !validBackupID(id) {
		return nil, "", errBackupNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.jobs[id]
	if job == nil {
		return nil, "", errBackupNotFound
	}
	if job.State != "completed" || job.path == "" {
		return nil, "", errBackupNotReady
	}
	copy := *job
	return &copy, job.path, nil
}

func validBackupID(id string) bool {
	return len(id) == 43 && validDiagnosticID(id)
}

func validLowerHex(value string) bool {
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validSHA256Hex(value string) bool {
	return len(value) == sha256.Size*2 && validLowerHex(value)
}

func (m *backupManager) pruneLocked(now time.Time) {
	kept := m.order[:0]
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil {
			continue
		}
		if job.ExpiresAt != nil && now.UnixMilli() >= *job.ExpiresAt {
			if err := os.Remove(job.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				kept = append(kept, id)
				continue
			}
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
	for terminalCount > backupHistoryLimit {
		removed := false
		for i, id := range m.order {
			if id == m.active {
				continue
			}
			job := m.jobs[id]
			if err := os.Remove(job.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
			delete(m.jobs, id)
			m.order = append(m.order[:i], m.order[i+1:]...)
			terminalCount--
			removed = true
			break
		}
		if !removed {
			break
		}
	}
}

func (m *backupManager) close(timeout time.Duration) bool {
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
		_ = os.Remove(filepath.Join(m.dir, job.ID+".snapshot.db"))
	}
	m.jobs, m.order, m.active = map[string]*backupJob{}, nil, ""
	m.mu.Unlock()
	_ = os.Remove(m.dir)
	return true
}

func (m *backupManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
}

func hashBackupFile(ctx context.Context, file *os.File, size int64) (string, error) {
	h := sha256.New()
	buffer := make([]byte, 128<<10)
	defer clear(buffer)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		n, err := io.ReadFull(file, buffer[:want])
		if err != nil {
			return "", err
		}
		_, _ = h.Write(buffer[:n])
		remaining -= int64(n)
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return "", errors.New("controller backup artifact changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) auditBackup(event, severity string, adminID int64, username, jobID, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), backupAuditTimeout)
	defer cancel()
	_ = s.Store.LogEvent(ctx, store.Event{Category: "audit", Severity: severity, Event: event,
		Detail: map[string]any{
			"job_id": jobID, "actor_admin_id": adminID,
			"actor_username": username, "state": state,
		}})
}
