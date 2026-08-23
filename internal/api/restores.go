package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/controllerrestore"
	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/restoreswap"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	restoreMediaType            = "application/vnd.oonfeewrt.backup"
	restoreUploadMaxBytes       = controllerPortableArtifactMaxBytes
	restoreHistoryLimit         = 5
	restoreRetention            = 30 * time.Minute
	restorePreviewTimeout       = 30 * time.Minute
	restoreConfirmationTimeout  = 30 * time.Minute
	restoreAuditTimeout         = 2 * time.Second
	restoreConfirmationContract = "controller-restore-confirm-v1"
	restoreTypedConfirmation    = "RESTORE CONTROLLER"
	restoreResumeConfirmation   = "RESUME ROUTER WRITES"
)

var (
	errRestoreClosed            = errors.New("controller restores are closed")
	errRestoreUploadNotFound    = errors.New("restore upload not found")
	errRestorePreviewNotFound   = errors.New("restore preview not found")
	errRestorePreviewInProgress = errors.New("restore preview is already active")
	errRestorePreviewTerminal   = errors.New("restore preview cannot be cancelled")
	errRestorePreviewNotReady   = errors.New("restore preview is not ready")
	errRestoreConfirmInProgress = errors.New("restore confirmation is already active")
	errRestoreRetention         = errors.New("restore retention cleanup failed")
)

type restoreUploadDTO struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type restoreManifestDTO struct {
	Format            string    `json:"format"`
	FormatVersion     int       `json:"format_version"`
	CreatedAt         time.Time `json:"created_at"`
	ControllerVersion string    `json:"controller_version"`
	SchemaVersion     int       `json:"schema_version"`
	DatabaseSizeBytes uint64    `json:"database_size_bytes"`
}

type restoreCountsDTO struct {
	Devices       int `json:"devices"`
	Credentials   int `json:"credentials"`
	OwnedSections int `json:"owned_sections"`
	WLANs         int `json:"wlans"`
	Meshes        int `json:"meshes"`
}

type restorePreviewDTO struct {
	ID              string              `json:"id"`
	UploadID        string              `json:"upload_id"`
	State           string              `json:"state"`
	Phase           string              `json:"phase"`
	ProgressPercent int                 `json:"progress_percent"`
	CreatedAt       int64               `json:"created_at"`
	StartedAt       *int64              `json:"started_at,omitempty"`
	FinishedAt      *int64              `json:"finished_at,omitempty"`
	ExpiresAt       *int64              `json:"expires_at,omitempty"`
	PlanID          string              `json:"plan_id,omitempty"`
	Manifest        *restoreManifestDTO `json:"manifest,omitempty"`
	SourceSchema    *int                `json:"source_schema,omitempty"`
	TargetSchema    *int                `json:"target_schema,omitempty"`
	Counts          *restoreCountsDTO   `json:"counts,omitempty"`
	ErrorCode       string              `json:"error_code,omitempty"`
	Error           string              `json:"error,omitempty"`
}

type restoreUpload struct {
	restoreUploadDTO
	name     string
	identity os.FileInfo
}

type restorePreviewJob struct {
	restorePreviewDTO
	uploadSHA              string
	actorAdminID           int64
	actorUsername          string
	cancelRequestedAdminID int64
	cancelRequestedUser    string
	cancelRequested        bool
	cancel                 context.CancelFunc
	complete               func()
}

type restoreManager struct {
	mu              sync.Mutex
	dir             string
	uploadsDir      string
	scratchDir      string
	dirRoot         *os.Root
	uploadsRoot     *os.Root
	scratchRoot     *os.Root
	dirIdentity     os.FileInfo
	uploadsIdentity os.FileInfo
	scratchIdentity os.FileInfo
	server          *Server
	uploads         map[string]*restoreUpload
	uploadOrder     []string
	previews        map[string]*restorePreviewJob
	previewOrder    []string
	active          string
	confirming      string
	root            context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	running         int
	uploading       int
	closed          bool
	cleaned         bool
}

type restoreDescriptor struct {
	Format               string   `json:"format"`
	FormatVersion        int      `json:"format_version"`
	UploadContentType    string   `json:"upload_content_type"`
	ConfirmationContract string   `json:"confirmation_contract"`
	TypedConfirmation    string   `json:"typed_confirmation"`
	ConfirmationRequires []string `json:"confirmation_requires"`
}

type restoreDisclosure struct {
	RouterManagementCalls bool   `json:"router_management_calls"`
	RouterChanges         bool   `json:"router_changes"`
	LiveControllerChanges bool   `json:"live_controller_changes"`
	AutomaticRouterApply  bool   `json:"automatic_router_apply"`
	Summary               string `json:"summary"`
}

type restoreLimits struct {
	MaxUploadBytes                int64 `json:"max_upload_bytes"`
	MaxDatabaseBytes              int64 `json:"max_database_bytes"`
	History                       int   `json:"history"`
	RetentionSeconds              int64 `json:"retention_seconds"`
	PreviewTimeoutSeconds         int64 `json:"preview_timeout_seconds"`
	ConfirmationTimeoutSeconds    int64 `json:"confirmation_timeout_seconds"`
	MinExportPassphraseCharacters int   `json:"min_export_passphrase_characters"`
	MaxExportPassphraseBytes      int   `json:"max_export_passphrase_bytes"`
}

type restoresResponse struct {
	Descriptor restoreDescriptor   `json:"descriptor"`
	Disclosure restoreDisclosure   `json:"disclosure"`
	Limits     restoreLimits       `json:"limits"`
	Uploads    []restoreUploadDTO  `json:"uploads"`
	Previews   []restorePreviewDTO `json:"previews"`
}

type restoreUploadResponse struct {
	Upload restoreUploadDTO `json:"upload"`
}

type restorePreviewResponse struct {
	Preview restorePreviewDTO `json:"preview"`
}

type startRestorePreviewRequest struct {
	UploadID         string      `json:"upload_id"`
	ExportPassphrase secretBytes `json:"export_passphrase"`
}

type restoreInspectFunc func(context.Context, string, string, []byte) (controllerrestore.Preview, error)

type restorePrepared interface {
	Preview() controllerrestore.Preview
	Cleanup() error
	Transfer(context.Context, func(controllerrestore.PreparedPair) error) (bool, error)
}

type restorePrepareFunc func(context.Context, string, string, *secrets.Keeper,
	[]byte, []byte) (restorePrepared, error)

type restoreCreateIntentFunc func(context.Context, string, restoreswap.PreparedPair,
	*secrets.Keeper, []byte, string) (restoreswap.IntentResult, error)

type confirmRestoreRequest struct {
	PlanID                            string      `json:"plan_id"`
	ExportPassphrase                  secretBytes `json:"export_passphrase"`
	DestinationRuntimePassphrase      secretBytes `json:"destination_runtime_passphrase"`
	TypedConfirmation                 string      `json:"typed_confirmation"`
	AcknowledgeRestart                bool        `json:"acknowledge_restart"`
	AcknowledgeSessionRevocation      bool        `json:"acknowledge_session_revocation"`
	AcknowledgeRouterWritesSuppressed bool        `json:"acknowledge_router_writes_suppressed"`
	AcknowledgeNoAutomaticRouterApply bool        `json:"acknowledge_no_automatic_router_apply"`
}

type restoreIntentDTO struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	AcceptedAt int64  `json:"accepted_at"`
}

type restoreIntentResponse struct {
	Intent restoreIntentDTO `json:"intent"`
}

type restoreSuppressionDTO struct {
	Active    bool       `json:"active"`
	RestoreID string     `json:"restore_id,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type restoreSuppressionResponse struct {
	Suppression restoreSuppressionDTO `json:"suppression"`
}

type resumeRouterWritesRequest struct {
	RestoreID         string `json:"restore_id"`
	TypedConfirmation string `json:"typed_confirmation"`
}

func (s *Server) InitRestores() error {
	if s.RestoresDir == "" {
		return errors.New("api: restores directory is empty")
	}
	if s.restores != nil {
		return errors.New("api: restores are already initialized")
	}
	dir := filepath.Clean(s.RestoresDir)
	if !filepath.IsAbs(dir) || filepath.Base(dir) != "restores" {
		return errors.New("api: restores directory must be an absolute restores subdirectory")
	}
	prepared, err := prepareRestoresDir(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.restores = &restoreManager{
		dir: dir, uploadsDir: prepared.uploadsDir, scratchDir: prepared.scratchDir,
		dirRoot: prepared.dirRoot, uploadsRoot: prepared.uploadsRoot, scratchRoot: prepared.scratchRoot,
		dirIdentity: prepared.dirIdentity, uploadsIdentity: prepared.uploadsIdentity,
		scratchIdentity: prepared.scratchIdentity,
		server:          s, uploads: map[string]*restoreUpload{}, previews: map[string]*restorePreviewJob{},
		root: ctx, cancel: cancel,
	}
	return nil
}

type preparedRestoreDirs struct {
	uploadsDir, scratchDir                        string
	dirRoot, uploadsRoot, scratchRoot             *os.Root
	dirIdentity, uploadsIdentity, scratchIdentity os.FileInfo
}

func prepareRestoresDir(dir string) (_ *preparedRestoreDirs, retErr error) {
	if err := preparePrivateDirectory(dir, "restore"); err != nil {
		return nil, err
	}
	namedRoot, err := os.Lstat(dir)
	if err != nil || !namedRoot.IsDir() || namedRoot.Mode()&os.ModeSymlink != 0 || namedRoot.Mode().Perm() != 0o700 {
		return nil, errors.New("api: restores directory is not private")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("api: anchor restores directory")
	}
	defer func() {
		if retErr != nil {
			_ = root.Close()
		}
	}()
	anchoredRoot, err := root.Stat(".")
	if err != nil || !os.SameFile(namedRoot, anchoredRoot) {
		return nil, errors.New("api: restores directory identity changed")
	}
	entries, err := readRootEntries(root)
	if err != nil {
		return nil, fmt.Errorf("api: inspect restores directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != "uploads" && entry.Name() != "scratch" {
			return nil, fmt.Errorf("api: restores directory contains unowned entry %q", entry.Name())
		}
	}
	for _, name := range []string{"uploads", "scratch"} {
		if err := root.MkdirAll(name, 0o700); err != nil {
			return nil, fmt.Errorf("api: create restores %s directory: %w", name, err)
		}
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("api: restores %s path is not a real directory", name)
		}
		child, err := root.OpenRoot(name)
		if err != nil {
			return nil, fmt.Errorf("api: anchor restores %s directory: %w", name, err)
		}
		if err := chmodRootDirectory(child, 0o700); err != nil {
			child.Close()
			return nil, fmt.Errorf("api: protect restores %s directory: %w", name, err)
		}
		childEntries, readErr := readRootEntries(child)
		if readErr != nil {
			child.Close()
			return nil, fmt.Errorf("api: inspect restores %s directory: %w", name, readErr)
		}
		for _, entry := range childEntries {
			if !ownedRestoreEntry(name, entry) {
				child.Close()
				return nil, fmt.Errorf("api: restores %s directory contains unowned entry %q", name, entry.Name())
			}
			if err := child.RemoveAll(entry.Name()); err != nil {
				child.Close()
				return nil, fmt.Errorf("api: clean restores %s directory: %w", name, err)
			}
		}
		if err := syncRoot(child); err != nil {
			child.Close()
			return nil, fmt.Errorf("api: sync restores %s directory: %w", name, err)
		}
		if err := child.Close(); err != nil {
			return nil, fmt.Errorf("api: close restores %s directory: %w", name, err)
		}
	}
	uploadsRoot, err := root.OpenRoot("uploads")
	if err != nil {
		return nil, errors.New("api: anchor restore uploads directory")
	}
	scratchRoot, err := root.OpenRoot("scratch")
	if err != nil {
		uploadsRoot.Close()
		return nil, errors.New("api: anchor restore scratch directory")
	}
	uploadsIdentity, uploadsErr := uploadsRoot.Stat(".")
	scratchIdentity, scratchErr := scratchRoot.Stat(".")
	currentRoot, namedErr := os.Lstat(dir)
	if uploadsErr != nil || scratchErr != nil || namedErr != nil || !os.SameFile(currentRoot, anchoredRoot) {
		uploadsRoot.Close()
		scratchRoot.Close()
		return nil, errors.New("api: restore directory identity changed")
	}
	for _, child := range []struct {
		name, path string
		identity   os.FileInfo
	}{
		{"uploads", filepath.Join(dir, "uploads"), uploadsIdentity},
		{"scratch", filepath.Join(dir, "scratch"), scratchIdentity},
	} {
		inside, insideErr := root.Lstat(child.name)
		named, namedErr := os.Lstat(child.path)
		if insideErr != nil || namedErr != nil || inside.Mode()&os.ModeSymlink != 0 ||
			named.Mode()&os.ModeSymlink != 0 || !os.SameFile(inside, child.identity) ||
			!os.SameFile(named, child.identity) {
			uploadsRoot.Close()
			scratchRoot.Close()
			return nil, errors.New("api: restore child directory identity changed")
		}
	}
	return &preparedRestoreDirs{
		uploadsDir: filepath.Join(dir, "uploads"), scratchDir: filepath.Join(dir, "scratch"),
		dirRoot: root, uploadsRoot: uploadsRoot, scratchRoot: scratchRoot,
		dirIdentity: anchoredRoot, uploadsIdentity: uploadsIdentity, scratchIdentity: scratchIdentity,
	}, nil
}

func readRootEntries(root *os.Root) ([]os.DirEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := dir.ReadDir(-1)
	return entries, errors.Join(readErr, dir.Close())
}

func preparePrivateDirectory(dir, label string) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("api: %s path is not a private directory", label)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("api: inspect %s directory: %w", label, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("api: create %s directory: %w", label, err)
	}
	named, err := os.Lstat(dir)
	if err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("api: %s path is not a private directory", label)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("api: anchor %s directory: %w", label, err)
	}
	defer root.Close()
	anchored, err := root.Stat(".")
	if err != nil || !os.SameFile(named, anchored) {
		return fmt.Errorf("api: %s directory identity changed", label)
	}
	if err := chmodRootDirectory(root, 0o700); err != nil {
		return fmt.Errorf("api: protect %s directory: %w", label, err)
	}
	current, err := os.Lstat(dir)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, anchored) || current.Mode().Perm() != 0o700 {
		return fmt.Errorf("api: %s directory identity changed", label)
	}
	return nil
}

func chmodRootDirectory(root *os.Root, mode os.FileMode) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Chmod(mode), dir.Close())
}

func ownedRestoreEntry(kind string, entry os.DirEntry) bool {
	name := entry.Name()
	switch kind {
	case "uploads":
		if strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp") {
			return !entry.IsDir() && validBackupID(strings.TrimSuffix(strings.TrimPrefix(name, ".upload-"), ".tmp"))
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".oowrtbak") {
			return false
		}
		return validBackupID(strings.TrimSuffix(name, ".oowrtbak"))
	case "scratch":
		if !entry.IsDir() {
			return false
		}
		for _, shape := range []struct{ prefix, suffix string }{
			{".oonfeewrt-preview-", ".tmp"}, {".oonfeewrt-restore-", ".stage"},
		} {
			if strings.HasPrefix(name, shape.prefix) && strings.HasSuffix(name, shape.suffix) {
				middle := strings.TrimSuffix(strings.TrimPrefix(name, shape.prefix), shape.suffix)
				return len(middle) == 32 && validLowerHex(middle)
			}
		}
	}
	return false
}

func (s *Server) handleRestores(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) || !s.restoresAvailable(w) {
		return
	}
	uploads, previews := s.restores.list(s.now())
	writeJSON(w, http.StatusOK, restoresResponse{
		Descriptor: restoreDescriptor{
			Format: "oonfeewrt-portable-backup", FormatVersion: 1,
			UploadContentType:    restoreMediaType,
			ConfirmationContract: restoreConfirmationContract,
			TypedConfirmation:    restoreTypedConfirmation,
			ConfirmationRequires: []string{
				"Re-enter the export passphrase.",
				"Enter the current destination controller runtime passphrase.",
				"Acknowledge controller restart and active-session revocation.",
				"Acknowledge that router writes stay suppressed after restore until an owner resumes them.",
				"Acknowledge that restored desired configuration is never applied to routers automatically.",
			},
		},
		Disclosure: restoreDisclosure{
			RouterManagementCalls: false, RouterChanges: false, LiveControllerChanges: false,
			AutomaticRouterApply: false,
			Summary:              "Upload and preview authenticate, migrate and validate only disposable controller state. They do not change this controller or contact routers.",
		},
		Limits: restoreLimits{
			MaxUploadBytes: restoreUploadMaxBytes, MaxDatabaseBytes: controllerPortableDatabaseMaxBytes,
			History:                       restoreHistoryLimit,
			RetentionSeconds:              int64(restoreRetention / time.Second),
			PreviewTimeoutSeconds:         int64(restorePreviewTimeout / time.Second),
			ConfirmationTimeoutSeconds:    int64(restoreConfirmationTimeout / time.Second),
			MinExportPassphraseCharacters: minExportPassphraseRunes,
			MaxExportPassphraseBytes:      maxExportPassphraseBytes,
		},
		Uploads: uploads, Previews: previews,
	})
}

func (s *Server) handleRestoreUpload(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) || !s.restoresAvailable(w) {
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != restoreMediaType || len(params) != 0 {
		writeCodedErr(w, http.StatusUnsupportedMediaType, "invalid_restore_media_type",
			"restore uploads must use application/vnd.oonfeewrt.backup")
		return
	}
	if r.ContentLength <= 0 {
		writeCodedErr(w, http.StatusLengthRequired, "restore_content_length_required",
			"restore uploads require an exact Content-Length")
		return
	}
	if r.ContentLength > restoreUploadMaxBytes || len(r.TransferEncoding) != 0 {
		writeCodedErr(w, http.StatusRequestEntityTooLarge, "restore_upload_too_large",
			"restore upload exceeds the controller portable-backup limit or uses an ambiguous transfer length")
		return
	}
	upload, err := s.restores.receiveUpload(r.Context(), r.Body, r.ContentLength, s.now())
	switch {
	case errors.Is(err, errRestoreClosed):
		writeCodedErr(w, http.StatusServiceUnavailable, "restores_unavailable", "controller restores are shutting down")
	case errors.Is(err, errRestoreRetention):
		writeCodedErr(w, http.StatusServiceUnavailable, "restore_retention_blocked", "previous restore uploads could not be safely expired")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeCodedErr(w, http.StatusRequestTimeout, "restore_upload_interrupted", "restore upload was interrupted and discarded")
	case errors.Is(err, errRestoreUploadLength):
		writeCodedErr(w, http.StatusBadRequest, "restore_upload_length_mismatch", "restore upload did not match Content-Length and was discarded")
	case err != nil:
		writeCodedErr(w, http.StatusInternalServerError, "restore_upload_failed", "restore upload could not be stored")
	default:
		sess, _ := sessionFrom(r.Context())
		s.auditRestore("restore.upload_completed", "info", sess.adminID, sess.username,
			"upload_id", upload.ID, "uploaded")
		writeJSON(w, http.StatusCreated, restoreUploadResponse{Upload: upload})
	}
}

var errRestoreUploadLength = errors.New("restore upload length mismatch")

func (s *Server) handleStartRestorePreview(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) || !s.restoresAvailable(w) {
		return
	}
	var req startRestorePreviewRequest
	if !decodeJSON(w, r, &req) {
		clear(req.ExportPassphrase)
		return
	}
	defer clear(req.ExportPassphrase)
	if !validBackupID(strings.TrimSpace(req.UploadID)) {
		writeCodedErr(w, http.StatusBadRequest, "invalid_restore_upload_id", "restore upload id is invalid")
		return
	}
	if err := validateExportPassphrase(req.ExportPassphrase); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "invalid_export_passphrase", err.Error())
		return
	}
	releaseHash, ok := s.beginBackupHashSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "restore_capacity_busy",
			"controller password work is busy; retry the restore preview shortly")
		return
	}
	release, ok := s.beginOperation(w, operationRestorePrepare)
	if !ok {
		releaseHash()
		return
	}
	complete := func() { release(); releaseHash() }
	sess, _ := sessionFrom(r.Context())
	preview, err := s.restores.startPreview(strings.TrimSpace(req.UploadID), req.ExportPassphrase,
		sess.adminID, sess.username, s.now(), complete)
	if err != nil {
		complete()
	}
	switch {
	case errors.Is(err, errRestoreUploadNotFound):
		writeCodedErr(w, http.StatusNotFound, "restore_upload_not_found", "restore upload not found")
	case errors.Is(err, errRestorePreviewInProgress):
		writeCodedErr(w, http.StatusConflict, "restore_preview_in_progress", "a restore preview is already active")
	case errors.Is(err, errRestoreClosed):
		writeCodedErr(w, http.StatusServiceUnavailable, "restores_unavailable", "controller restores are shutting down")
	case errors.Is(err, errRestoreRetention):
		writeCodedErr(w, http.StatusServiceUnavailable, "restore_retention_blocked", "previous restore previews could not be safely expired")
	case err != nil:
		writeCodedErr(w, http.StatusInternalServerError, "restore_preview_start_failed", "restore preview could not be started")
	default:
		writeJSON(w, http.StatusAccepted, restorePreviewResponse{Preview: preview})
	}
}

func (s *Server) handleRestorePreview(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) || !s.restoresAvailable(w) {
		return
	}
	preview, err := s.restores.preview(strings.TrimSpace(r.PathValue("id")), s.now())
	if errors.Is(err, errRestorePreviewNotFound) {
		writeCodedErr(w, http.StatusNotFound, "restore_preview_not_found", "restore preview not found")
		return
	}
	writeJSON(w, http.StatusOK, restorePreviewResponse{Preview: preview})
}

func (s *Server) handleCancelRestorePreview(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) || !s.restoresAvailable(w) || !emptyRequestBody(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	preview, err := s.restores.cancelPreview(strings.TrimSpace(r.PathValue("id")), s.now(),
		sess.adminID, sess.username)
	switch {
	case errors.Is(err, errRestorePreviewNotFound):
		writeCodedErr(w, http.StatusNotFound, "restore_preview_not_found", "restore preview not found")
	case errors.Is(err, errRestorePreviewTerminal):
		writeCodedErr(w, http.StatusConflict, "restore_preview_not_cancellable", "restore preview cannot be cancelled")
	default:
		s.auditRestore("restore.preview_cancel_requested", "info", sess.adminID, sess.username,
			"preview_id", preview.ID, preview.State)
		writeJSON(w, http.StatusOK, restorePreviewResponse{Preview: preview})
	}
}

func (s *Server) handleConfirmRestore(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) || !s.restoresAvailable(w) {
		return
	}
	var req confirmRestoreRequest
	if !decodeJSON(w, r, &req) {
		clear(req.ExportPassphrase)
		clear(req.DestinationRuntimePassphrase)
		return
	}
	defer clear(req.ExportPassphrase)
	defer clear(req.DestinationRuntimePassphrase)
	previewID := strings.TrimSpace(r.PathValue("id"))
	if !validBackupID(previewID) {
		writeCodedErr(w, http.StatusNotFound, "restore_preview_not_found", "restore preview not found")
		return
	}
	if !validRestorePlanID(req.PlanID) {
		writeCodedErr(w, http.StatusBadRequest, "invalid_restore_plan", "restore plan id is invalid")
		return
	}
	if !constantTimeText(req.TypedConfirmation, restoreTypedConfirmation) ||
		!req.AcknowledgeRestart || !req.AcknowledgeSessionRevocation ||
		!req.AcknowledgeRouterWritesSuppressed || !req.AcknowledgeNoAutomaticRouterApply {
		writeCodedErr(w, http.StatusBadRequest, "restore_confirmation_incomplete",
			"the exact typed confirmation and every restore acknowledgement are required")
		return
	}
	if err := validateExportPassphrase(req.ExportPassphrase); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "invalid_export_passphrase", err.Error())
		return
	}
	if err := validateRuntimePassphrase(req.DestinationRuntimePassphrase); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "invalid_runtime_passphrase", err.Error())
		return
	}
	restart := s.RequestRestart
	if s.Keys == nil || restart == nil || !validRestoreIntentID(s.RestoreOwnerInstanceID) {
		writeCodedErr(w, http.StatusServiceUnavailable, "restore_confirmation_unavailable",
			"controller restore confirmation is unavailable")
		return
	}
	if s.restoreSuppressionDTO().Active {
		writeCodedErr(w, http.StatusConflict, "router_review_required",
			"finish the active restored-state router review before confirming another restore")
		return
	}
	job, upload, finish, err := s.restores.beginConfirmation(previewID, s.now())
	if err != nil {
		switch {
		case errors.Is(err, errRestorePreviewNotFound):
			writeCodedErr(w, http.StatusNotFound, "restore_preview_not_found", "restore preview not found")
		case errors.Is(err, errRestorePreviewNotReady):
			writeCodedErr(w, http.StatusConflict, "restore_preview_not_ready", "restore preview is not ready for confirmation")
		case errors.Is(err, errRestorePreviewInProgress):
			writeCodedErr(w, http.StatusConflict, "restore_preview_in_progress", "a restore preview is already active")
		case errors.Is(err, errRestoreConfirmInProgress):
			writeCodedErr(w, http.StatusConflict, "restore_confirmation_in_progress", "a restore confirmation is already active")
		case errors.Is(err, errRestoreUploadNotFound):
			writeCodedErr(w, http.StatusConflict, "restore_upload_changed", "restore upload changed; upload and preview it again")
		default:
			writeCodedErr(w, http.StatusServiceUnavailable, "restores_unavailable", "controller restores are shutting down")
		}
		return
	}
	defer finish()
	if !constantTimeText(req.PlanID, job.PlanID) {
		writeCodedErr(w, http.StatusConflict, "restore_plan_changed", "restore plan changed; run a new preview")
		return
	}
	releaseHash, ok := s.beginBackupHashSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "restore_capacity_busy",
			"controller password work is busy; retry restore confirmation shortly")
		return
	}
	defer releaseHash()
	releasePrepare, ok := s.beginOperation(w, operationRestorePrepare)
	if !ok {
		return
	}
	upgraded := false
	defer func() {
		if !upgraded {
			releasePrepare()
		}
	}()

	timeout := s.restoreConfirmTimeout
	if timeout <= 0 {
		timeout = restoreConfirmationTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	stop := context.AfterFunc(s.restores.root, cancel)
	defer func() { stop(); cancel() }()
	path, valid := s.restores.checkedUploadPath(upload)
	if !valid || s.restores.verifyUploadHash(ctx, upload) != nil {
		writeCodedErr(w, http.StatusConflict, "restore_upload_changed", "restore upload changed; upload and preview it again")
		return
	}
	prepare := s.restorePrepare
	if prepare == nil {
		prepare = func(ctx context.Context, artifactPath, dataDir string, live *secrets.Keeper,
			exportPassphrase, runtimePassphrase []byte) (restorePrepared, error) {
			return controllerrestore.Prepare(ctx, artifactPath, dataDir, live,
				exportPassphrase, runtimePassphrase)
		}
	}
	prepared, err := prepare(ctx, path, filepath.Dir(s.RestoresDir), s.Keys,
		req.ExportPassphrase, req.DestinationRuntimePassphrase)
	if s.afterRestorePrepared != nil {
		s.afterRestorePrepared()
	}
	if err != nil {
		if prepared != nil {
			if cleanupErr := prepared.Cleanup(); cleanupErr != nil {
				writeCodedErr(w, http.StatusInternalServerError, "restore_cleanup_failed",
					"temporary restore state could not be safely removed")
				return
			}
		}
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeCodedErr(w, http.StatusRequestTimeout, "restore_confirmation_interrupted",
				"restore confirmation was interrupted before any intent was created")
		case errors.Is(err, secrets.ErrBadPassphrase), errors.Is(err, portablebackup.ErrAuthentication):
			writeCodedErr(w, http.StatusUnprocessableEntity, "restore_authentication_failed",
				"the backup, export passphrase, or destination runtime passphrase could not be authenticated")
		default:
			writeCodedErr(w, http.StatusUnprocessableEntity, "restore_prepare_failed",
				"the backup could not be safely prepared; no restore intent was created")
		}
		return
	}
	if prepared == nil {
		writeCodedErr(w, http.StatusInternalServerError, "restore_prepare_failed",
			"the backup could not be safely prepared; no restore intent was created")
		return
	}
	cleanupPrepared := func() bool {
		if prepared.Cleanup() == nil {
			return true
		}
		writeCodedErr(w, http.StatusInternalServerError, "restore_cleanup_failed",
			"temporary restore state could not be safely removed")
		return false
	}
	if err := ctx.Err(); err != nil {
		if cleanupPrepared() {
			writeCodedErr(w, http.StatusRequestTimeout, "restore_confirmation_interrupted",
				"restore confirmation was interrupted before any intent was created")
		}
		return
	}
	if _, valid := s.restores.checkedUploadPath(upload); !valid || s.restores.verifyUploadHash(ctx, upload) != nil {
		if cleanupPrepared() {
			writeCodedErr(w, http.StatusConflict, "restore_upload_changed", "restore upload changed; upload and preview it again")
		}
		return
	}
	preparedPlan, err := restorePlanID(upload.SHA256, prepared.Preview())
	if err != nil || !constantTimeText(preparedPlan, req.PlanID) || !constantTimeText(preparedPlan, job.PlanID) {
		if cleanupPrepared() {
			writeCodedErr(w, http.StatusConflict, "restore_plan_changed", "restore plan changed; run a new preview")
		}
		return
	}
	releaseExclusive, conflicts, err := s.upgradeRestorePrepareToExclusive()
	if err != nil {
		if cleanupPrepared() {
			if errors.Is(err, errOperationRouterSuppressed) {
				writeCodedErr(w, http.StatusConflict, "router_review_required",
					"finish the active restored-state router review before confirming another restore")
			} else if errors.Is(err, errOperationAdmissionBusy) || errors.Is(err, errOperationAdmissionExclusive) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"code": "restore_operation_conflict", "error": "active operations must finish before restore confirmation",
					"conflicts": conflicts,
				})
			} else {
				writeErr(w, http.StatusServiceUnavailable, shutdownNoWrite)
			}
		}
		return
	}
	upgraded = true
	defer releaseExclusive()
	sess, _ := sessionFrom(r.Context())
	if err := s.auditRestoreDurable(ctx, "restore.confirmation_authorized", "warning", map[string]any{
		"preview_id": previewID, "upload_id": job.UploadID, "plan_id": preparedPlan,
		"actor_admin_id": sess.adminID, "actor_username": sess.username, "state": "authorized",
	}); err != nil {
		if cleanupPrepared() {
			writeCodedErr(w, http.StatusInternalServerError, "restore_audit_failed",
				"restore confirmation was not recorded; no restore intent was created")
		}
		return
	}
	createIntent := s.restoreCreateIntent
	if createIntent == nil {
		createIntent = restoreswap.CreateIntent
	}
	var intent restoreswap.IntentResult
	var retainedErr error
	transferred, transferErr := prepared.Transfer(ctx, func(pair controllerrestore.PreparedPair) error {
		result, createErr := createIntent(ctx, filepath.Dir(s.RestoresDir), restoreswap.PreparedPair{
			DatabasePath: pair.DatabasePath, KeyringPath: pair.KeyringPath,
			AuthorizingAdminID: sess.adminID, AuthorizingUsername: sess.username,
			PreviewID: previewID, PlanID: preparedPlan,
		}, s.Keys, req.ExportPassphrase, s.RestoreOwnerInstanceID)
		if createErr != nil && restoreswap.IntentOwnershipRetained(createErr) {
			intent, retainedErr = result, createErr
			return nil
		}
		if createErr == nil {
			intent = result
		}
		return createErr
	})
	if !transferred {
		if !cleanupPrepared() {
			return
		}
		if errors.Is(transferErr, context.Canceled) || errors.Is(transferErr, context.DeadlineExceeded) {
			writeCodedErr(w, http.StatusRequestTimeout, "restore_confirmation_interrupted",
				"restore confirmation was interrupted before any intent was created")
		} else {
			writeCodedErr(w, http.StatusInternalServerError, "restore_intent_failed",
				"a durable restore intent could not be created")
		}
		return
	}
	if transferErr != nil || retainedErr != nil {
		writeCodedErr(w, http.StatusInternalServerError, "restore_intent_finalize_failed",
			"a restore intent may require manual controller restart; no automatic restart was requested")
		return
	}
	if !validRestoreIntentID(intent.ID) {
		writeCodedErr(w, http.StatusInternalServerError, "restore_intent_invalid",
			"the durable restore intent response was invalid; no automatic restart was requested")
		return
	}
	s.CloseAdmission()
	writeJSON(w, http.StatusAccepted, restoreIntentResponse{Intent: restoreIntentDTO{
		ID: intent.ID, State: "accepted", AcceptedAt: s.now().UnixMilli(),
	}})
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
	restart()
}

func (s *Server) handleRestoreSuppression(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, restoreSuppressionResponse{Suppression: s.restoreSuppressionDTO()})
}

func (s *Server) handleResumeRouterWrites(w http.ResponseWriter, r *http.Request) {
	if !requireRestoreTransport(w, r) {
		return
	}
	var req resumeRouterWritesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.RestoreID = strings.TrimSpace(req.RestoreID)
	current := s.restoreSuppressionDTO()
	if !current.Active {
		writeCodedErr(w, http.StatusConflict, "router_writes_not_suppressed", "router writes are not suppressed")
		return
	}
	if !validRestoreIntentID(req.RestoreID) || !constantTimeText(req.RestoreID, current.RestoreID) ||
		!constantTimeText(req.TypedConfirmation, restoreResumeConfirmation) {
		writeCodedErr(w, http.StatusBadRequest, "invalid_resume_confirmation",
			"the active restore id and exact typed confirmation are required")
		return
	}
	resume := s.ResumeRouterWrites
	if resume == nil {
		writeCodedErr(w, http.StatusServiceUnavailable, "resume_router_writes_unavailable",
			"router-write resumption is unavailable")
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.auditRestoreDurable(r.Context(), "restore.router_writes_resume_authorized", "warning", map[string]any{
		"restore_id": req.RestoreID, "actor_admin_id": sess.adminID,
		"actor_username": sess.username, "state": "authorized",
	}); err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "restore_audit_failed",
			"router-write resumption was not recorded; suppression remains active")
		return
	}
	if err := resume(r.Context(), req.RestoreID); err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "resume_router_writes_failed",
			"router-write suppression could not be durably cleared")
		return
	}
	s.suppressionMu.Lock()
	s.RouterWriteSuppression = restoreswap.Suppression{}
	s.suppressionMu.Unlock()
	if s.operations != nil {
		s.operations.setSuppression(false)
	}
	auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(r.Context()), restoreAuditTimeout)
	err := s.auditRestoreDurable(auditCtx,
		"restore.router_writes_resumed", "warning", map[string]any{
			"restore_id": req.RestoreID, "actor_admin_id": sess.adminID,
			"actor_username": sess.username, "state": "resumed",
		})
	cancelAudit()
	if err != nil {
		s.Log.Error("router writes resumed but the success audit could not be recorded",
			"restore_id", req.RestoreID, "err", err)
	}
	if s.RouterWritesResumed != nil {
		s.RouterWritesResumed()
	}
	writeJSON(w, http.StatusOK, restoreSuppressionResponse{Suppression: restoreSuppressionDTO{Active: false}})
}

func (s *Server) restoreSuppressionDTO() restoreSuppressionDTO {
	s.suppressionMu.Lock()
	value := s.RouterWriteSuppression
	s.suppressionMu.Unlock()
	dto := restoreSuppressionDTO{Active: value.Active}
	if value.Active {
		created := value.CreatedAt
		dto.RestoreID, dto.CreatedAt, dto.Reason = value.RestoreID, &created, value.Reason
	}
	return dto
}

func (s *Server) restoresAvailable(w http.ResponseWriter) bool {
	if s.restores != nil {
		return true
	}
	writeCodedErr(w, http.StatusServiceUnavailable, "restores_unavailable", "controller restores are unavailable")
	return false
}

func requireRestoreTransport(w http.ResponseWriter, r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer, err := netip.ParseAddr(clientAddr(r))
	if err == nil && peer.IsLoopback() {
		return true
	}
	writeCodedErr(w, http.StatusUpgradeRequired, "secure_transport_required",
		"controller restores require TLS or a direct loopback connection")
	return false
}

func (m *restoreManager) receiveUpload(request context.Context, body io.Reader, size int64,
	now time.Time) (_ restoreUploadDTO, retErr error) {
	m.mu.Lock()
	m.pruneLocked(now)
	if m.closed {
		m.mu.Unlock()
		return restoreUploadDTO{}, errRestoreClosed
	}
	if !m.directoriesCurrent() {
		m.mu.Unlock()
		return restoreUploadDTO{}, errors.New("restore directory identity changed")
	}
	if len(m.uploads)+m.uploading >= restoreHistoryLimit && !m.makeUploadRoomLocked() {
		m.mu.Unlock()
		return restoreUploadDTO{}, errRestoreRetention
	}
	m.uploading++
	m.running++
	m.wg.Add(1)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.uploading--
		m.running--
		m.mu.Unlock()
		m.wg.Done()
	}()

	ctx, cancel := context.WithCancel(request)
	stop := context.AfterFunc(m.root, cancel)
	defer func() { stop(); cancel() }()
	id, err := randomToken()
	if err != nil {
		return restoreUploadDTO{}, err
	}
	temporary := ".upload-" + id + ".tmp"
	final := id + ".oowrtbak"
	file, err := m.uploadsRoot.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return restoreUploadDTO{}, err
	}
	temporaryIdentity, err := file.Stat()
	if err != nil || !temporaryIdentity.Mode().IsRegular() || temporaryIdentity.Mode().Perm() != 0o600 {
		_ = file.Close()
		return restoreUploadDTO{}, errors.New("restore upload temporary identity is invalid")
	}
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			retErr = errors.Join(retErr, file.Close())
		}
		if retErr != nil {
			retErr = errors.Join(retErr, removeIdentityFile(m.uploadsRoot, temporary, temporaryIdentity))
		}
	}()
	hasher := sha256.New()
	limited := io.LimitReader(&restoreContextReader{ctx: ctx, reader: body}, size+1)
	written, err := io.CopyBuffer(io.MultiWriter(file, hasher), limited, make([]byte, 128<<10))
	if err != nil {
		return restoreUploadDTO{}, err
	}
	if written != size {
		return restoreUploadDTO{}, errRestoreUploadLength
	}
	if err := file.Chmod(0o600); err != nil {
		return restoreUploadDTO{}, err
	}
	if err := file.Sync(); err != nil {
		return restoreUploadDTO{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != size {
		return restoreUploadDTO{}, errors.New("restore upload identity is invalid")
	}
	anchored, err := m.uploadsRoot.Lstat(temporary)
	if err != nil || anchored.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, anchored) {
		return restoreUploadDTO{}, errors.New("restore upload identity changed")
	}
	if err := file.Close(); err != nil {
		return restoreUploadDTO{}, err
	}
	temporaryOpen = false
	if m.server.beforeRestoreUploadPublish != nil {
		m.server.beforeRestoreUploadPublish(temporary, final)
	}
	if err := m.uploadsRoot.Link(temporary, final); err != nil {
		return restoreUploadDTO{}, err
	}
	published := true
	defer func() {
		if retErr != nil && published {
			retErr = errors.Join(retErr, removeIdentityFile(m.uploadsRoot, final, info))
		}
	}()
	if m.server.afterRestoreUploadPublish != nil {
		m.server.afterRestoreUploadPublish()
	}
	if err := ctx.Err(); err != nil {
		return restoreUploadDTO{}, err
	}
	if err := syncRoot(m.uploadsRoot); err != nil {
		return restoreUploadDTO{}, err
	}
	if err := m.uploadsRoot.Remove(temporary); err != nil {
		return restoreUploadDTO{}, err
	}
	if err := syncRoot(m.uploadsRoot); err != nil {
		return restoreUploadDTO{}, err
	}
	if err := ctx.Err(); err != nil {
		return restoreUploadDTO{}, err
	}
	dto := restoreUploadDTO{
		ID: id, CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(restoreRetention).UnixMilli(),
		SizeBytes: size, SHA256: hex.EncodeToString(hasher.Sum(nil)),
	}
	upload := &restoreUpload{restoreUploadDTO: dto, name: final, identity: info}
	if !m.directoriesCurrent() {
		return restoreUploadDTO{}, errors.New("restore directory identity changed")
	}
	if err := m.verifyUploadHash(ctx, upload); err != nil {
		return restoreUploadDTO{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return restoreUploadDTO{}, errRestoreClosed
	}
	m.uploads[id] = upload
	m.uploadOrder = append(m.uploadOrder, id)
	m.mu.Unlock()
	published = false
	return dto, nil
}

func syncRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func removeIdentityFile(root *os.Root, name string, identity os.FileInfo) error {
	current, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if identity == nil || !os.SameFile(current, identity) {
		return errors.New("restore artifact identity changed during cleanup")
	}
	if err := root.Remove(name); err != nil {
		return err
	}
	return syncRoot(root)
}

func (m *restoreManager) directoriesCurrent() bool {
	anchored, err := m.dirRoot.Stat(".")
	if err != nil || !os.SameFile(anchored, m.dirIdentity) {
		return false
	}
	named, err := os.Lstat(m.dir)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(named, anchored) {
		return false
	}
	for _, child := range []struct {
		name, path string
		root       *os.Root
		identity   os.FileInfo
	}{
		{"uploads", m.uploadsDir, m.uploadsRoot, m.uploadsIdentity},
		{"scratch", m.scratchDir, m.scratchRoot, m.scratchIdentity},
	} {
		inside, err := m.dirRoot.Lstat(child.name)
		if err != nil || inside.Mode()&os.ModeSymlink != 0 || !inside.IsDir() || !os.SameFile(inside, child.identity) {
			return false
		}
		handled, err := child.root.Stat(".")
		if err != nil || !os.SameFile(handled, child.identity) {
			return false
		}
		named, err := os.Lstat(child.path)
		if err != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(named, child.identity) {
			return false
		}
	}
	return true
}

func (m *restoreManager) startPreview(uploadID string, passphrase []byte, adminID int64,
	username string, now time.Time, complete func()) (restorePreviewDTO, error) {
	id, err := randomToken()
	if err != nil {
		return restorePreviewDTO{}, err
	}
	ownedPassphrase := append([]byte(nil), passphrase...)
	m.mu.Lock()
	m.pruneLocked(now)
	if m.closed {
		clear(ownedPassphrase)
		m.mu.Unlock()
		return restorePreviewDTO{}, errRestoreClosed
	}
	if m.active != "" || m.confirming != "" {
		clear(ownedPassphrase)
		m.mu.Unlock()
		return restorePreviewDTO{}, errRestorePreviewInProgress
	}
	upload := m.uploads[uploadID]
	if upload == nil {
		clear(ownedPassphrase)
		m.mu.Unlock()
		return restorePreviewDTO{}, errRestoreUploadNotFound
	}
	if !m.makePreviewRoomLocked() {
		clear(ownedPassphrase)
		m.mu.Unlock()
		return restorePreviewDTO{}, errRestoreRetention
	}
	ctx, cancel := context.WithTimeout(m.root, restorePreviewTimeout)
	job := &restorePreviewJob{restorePreviewDTO: restorePreviewDTO{
		ID: id, UploadID: uploadID, State: "queued", Phase: "waiting",
		ProgressPercent: 0, CreatedAt: now.UnixMilli(),
	}, uploadSHA: upload.SHA256, actorAdminID: adminID, actorUsername: username,
		cancel: cancel, complete: complete}
	m.previews[id] = job
	m.previewOrder = append(m.previewOrder, id)
	m.active = id
	m.running++
	m.wg.Add(1)
	dto := job.restorePreviewDTO
	m.mu.Unlock()
	m.server.auditRestore("restore.preview_started", "info", adminID, username,
		"preview_id", id, "queued")
	go m.runPreview(ctx, job, upload, ownedPassphrase)
	return dto, nil
}

func (m *restoreManager) beginConfirmation(id string, now time.Time) (
	restorePreviewDTO, *restoreUpload, func(), error) {
	if !validBackupID(id) {
		return restorePreviewDTO{}, nil, nil, errRestorePreviewNotFound
	}
	m.mu.Lock()
	m.pruneLocked(now)
	if m.closed {
		m.mu.Unlock()
		return restorePreviewDTO{}, nil, nil, errRestoreClosed
	}
	if m.confirming != "" {
		m.mu.Unlock()
		return restorePreviewDTO{}, nil, nil, errRestoreConfirmInProgress
	}
	if m.active != "" {
		m.mu.Unlock()
		return restorePreviewDTO{}, nil, nil, errRestorePreviewInProgress
	}
	job := m.previews[id]
	if job == nil {
		m.mu.Unlock()
		return restorePreviewDTO{}, nil, nil, errRestorePreviewNotFound
	}
	if job.State != "completed" || job.PlanID == "" {
		m.mu.Unlock()
		return restorePreviewDTO{}, nil, nil, errRestorePreviewNotReady
	}
	upload := m.uploads[job.UploadID]
	if upload == nil || !constantTimeText(upload.SHA256, job.uploadSHA) {
		m.mu.Unlock()
		return restorePreviewDTO{}, nil, nil, errRestoreUploadNotFound
	}
	copyUpload := *upload
	m.confirming = id
	m.running++
	m.wg.Add(1)
	dto := job.restorePreviewDTO
	m.mu.Unlock()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			m.mu.Lock()
			if m.confirming == id {
				m.confirming = ""
			}
			m.running--
			m.pruneLocked(m.server.now())
			m.mu.Unlock()
			m.wg.Done()
		})
	}
	return dto, &copyUpload, finish, nil
}

func (m *restoreManager) runPreview(ctx context.Context, job *restorePreviewJob,
	upload *restoreUpload, passphrase []byte) {
	defer func() {
		clear(passphrase)
		job.complete()
		m.wg.Done()
	}()
	m.transition(job.ID, "running", "authenticating", 10)

	path, ok := m.checkedUploadPath(upload)
	if !ok {
		m.finishPreview(job, controllerrestore.Preview{}, errors.New("restore upload identity changed"))
		return
	}
	if err := m.verifyUploadHash(ctx, upload); err != nil {
		m.finishPreview(job, controllerrestore.Preview{}, err)
		return
	}
	inspect := m.server.restoreInspect
	if inspect == nil {
		inspect = controllerrestore.Inspect
	}
	result, err := inspect(ctx, path, m.scratchDir, passphrase)
	if m.server.afterRestoreInspected != nil {
		m.server.afterRestoreInspected(job.ID)
	}
	if err == nil {
		err = m.verifyUploadHash(ctx, upload)
	}
	if err == nil {
		err = ctx.Err()
	}
	m.finishPreview(job, result, err)
}

func (m *restoreManager) checkedUploadPath(upload *restoreUpload) (string, bool) {
	if upload == nil || upload.name != upload.ID+".oowrtbak" {
		return "", false
	}
	if m.server.beforeRestorePreviewCheck != nil {
		m.server.beforeRestorePreviewCheck(upload.ID)
	}
	if !m.directoriesCurrent() {
		return "", false
	}
	anchored, err := m.uploadsRoot.Lstat(upload.name)
	if err != nil || anchored.Mode()&os.ModeSymlink != 0 || !anchored.Mode().IsRegular() ||
		anchored.Mode().Perm() != 0o600 || anchored.Size() != upload.SizeBytes ||
		!os.SameFile(anchored, upload.identity) {
		return "", false
	}
	named, err := os.Lstat(filepath.Join(m.uploadsDir, upload.name))
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(named, anchored) {
		return "", false
	}
	return filepath.Join(m.uploadsDir, upload.name), true
}

func (m *restoreManager) verifyUploadHash(ctx context.Context, upload *restoreUpload) error {
	if upload == nil || !validSHA256Hex(upload.SHA256) {
		return errors.New("restore upload digest is invalid")
	}
	file, err := m.uploadsRoot.Open(upload.name)
	if err != nil {
		return errors.New("restore upload could not be verified")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm() != 0o600 || before.Size() != upload.SizeBytes ||
		!os.SameFile(before, upload.identity) {
		return errors.New("restore upload identity changed")
	}
	hasher := sha256.New()
	read := io.LimitReader(&restoreContextReader{ctx: ctx, reader: file}, upload.SizeBytes+1)
	written, err := io.CopyBuffer(hasher, read, make([]byte, 128<<10))
	if err != nil {
		return err
	}
	if written != upload.SizeBytes {
		return errors.New("restore upload size changed")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() != upload.SizeBytes {
		return errors.New("restore upload identity changed")
	}
	want, err := hex.DecodeString(upload.SHA256)
	if err != nil || subtle.ConstantTimeCompare(hasher.Sum(nil), want) != 1 {
		return errors.New("restore upload digest changed")
	}
	return ctx.Err()
}

func (m *restoreManager) transition(id, state, phase string, progress int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.previews[id]
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

func (m *restoreManager) finishPreview(job *restorePreviewJob, result controllerrestore.Preview, cause error) {
	now := m.server.now()
	event, severity := "restore.preview_failed", "warning"
	m.mu.Lock()
	job = m.previews[job.ID]
	if job == nil {
		m.running--
		m.mu.Unlock()
		return
	}
	finished := now.UnixMilli()
	if finished < job.CreatedAt {
		finished = job.CreatedAt
	}
	expires := finished + restoreRetention.Milliseconds()
	job.FinishedAt, job.ExpiresAt = &finished, &expires
	job.ProgressPercent = 100
	switch {
	case job.cancelRequested:
		job.State, job.Phase = "cancelled", "cancelled"
		event, severity = "restore.preview_cancelled", "info"
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		job.State, job.Phase = "failed", "failed"
		job.ErrorCode, job.Error = "preview_interrupted", "Restore preview was interrupted; no controller state changed."
	default:
		if cause != nil {
			job.State, job.Phase = "failed", "failed"
			if errors.Is(cause, secrets.ErrBadPassphrase) || errors.Is(cause, portablebackup.ErrAuthentication) {
				job.ErrorCode, job.Error = "authentication_failed", "The backup or export passphrase could not be authenticated."
			} else {
				job.ErrorCode, job.Error = "invalid_backup", "The backup could not be safely migrated and validated."
			}
			break
		}
		manifest := restoreManifestFrom(result.Manifest)
		counts := restoreCountsFrom(result.Counts)
		source, target := result.SourceSchema, result.TargetSchema
		planID, err := restorePlanID(job.uploadSHA, result)
		if err != nil {
			job.State, job.Phase = "failed", "failed"
			job.ErrorCode, job.Error = "preview_failed", "The restore confirmation plan could not be created."
			break
		}
		job.State, job.Phase, job.PlanID = "completed", "ready", planID
		job.Manifest, job.Counts = &manifest, &counts
		job.SourceSchema, job.TargetSchema = &source, &target
		event, severity = "restore.preview_completed", "info"
		if upload := m.uploads[job.UploadID]; upload != nil && upload.ExpiresAt < expires {
			upload.ExpiresAt = expires
		}
	}
	if m.active == job.ID {
		m.active = ""
	}
	m.running--
	state := job.State
	adminID, username := job.actorAdminID, job.actorUsername
	if job.cancelRequested {
		adminID, username = job.cancelRequestedAdminID, job.cancelRequestedUser
	}
	m.pruneLocked(now)
	m.mu.Unlock()
	m.server.auditRestore(event, severity, adminID, username, "preview_id", job.ID, state)
}

func restoreManifestFrom(in portablebackup.Manifest) restoreManifestDTO {
	return restoreManifestDTO{
		Format: in.Format, FormatVersion: in.Version, CreatedAt: in.CreatedAt,
		ControllerVersion: in.ControllerVersion, SchemaVersion: in.SchemaVersion,
		DatabaseSizeBytes: in.Database.Size,
	}
}

func restoreCountsFrom(in recovery.Counts) restoreCountsDTO {
	return restoreCountsDTO{
		Devices: in.Devices, Credentials: in.Credentials, OwnedSections: in.OwnedSections,
		WLANs: in.WLANs, Meshes: in.Meshes,
	}
}

type restorePlanBinding struct {
	Contract                  string                  `json:"contract"`
	UploadSHA256              string                  `json:"upload_sha256"`
	Manifest                  portablebackup.Manifest `json:"manifest"`
	SourceSchema              int                     `json:"source_schema"`
	TargetSchema              int                     `json:"target_schema"`
	Counts                    restoreCountsDTO        `json:"counts"`
	TypedConfirmation         string                  `json:"typed_confirmation"`
	RequiresExportPassphrase  bool                    `json:"requires_export_passphrase"`
	RequiresRuntimePassphrase bool                    `json:"requires_runtime_passphrase"`
	Restart                   bool                    `json:"restart"`
	RevokeSessions            bool                    `json:"revoke_sessions"`
	SuppressRouterWrites      bool                    `json:"suppress_router_writes"`
	AutomaticRouterApply      bool                    `json:"automatic_router_apply"`
}

func restorePlanID(uploadSHA string, preview controllerrestore.Preview) (string, error) {
	if !validSHA256Hex(uploadSHA) {
		return "", errors.New("invalid upload digest")
	}
	payload := restorePlanBinding{
		Contract: restoreConfirmationContract, UploadSHA256: uploadSHA, Manifest: preview.Manifest,
		SourceSchema: preview.SourceSchema, TargetSchema: preview.TargetSchema,
		Counts: restoreCountsFrom(preview.Counts), TypedConfirmation: restoreTypedConfirmation,
		RequiresExportPassphrase: true, RequiresRuntimePassphrase: true,
		Restart: true, RevokeSessions: true, SuppressRouterWrites: true, AutomaticRouterApply: false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return restoreConfirmationContract + "." + hex.EncodeToString(digest[:]), nil
}

func validRestorePlanID(value string) bool {
	prefix := restoreConfirmationContract + "."
	return strings.HasPrefix(value, prefix) && len(value) == len(prefix)+sha256.Size*2 &&
		validLowerHex(strings.TrimPrefix(value, prefix))
}

func validRestoreIntentID(value string) bool {
	if len(value) != 32 || !validLowerHex(value) {
		return false
	}
	for i := range value {
		if value[i] != '0' {
			return true
		}
	}
	return false
}

func constantTimeText(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func validateRuntimePassphrase(passphrase []byte) error {
	if len(passphrase) == 0 || len(passphrase) > maxExportPassphraseBytes || !utf8.Valid(passphrase) {
		return errors.New("destination runtime passphrase must be valid UTF-8, non-empty, and no more than 4096 bytes")
	}
	return nil
}

func (s *Server) auditRestoreDurable(parent context.Context, event, severity string,
	detail map[string]any) error {
	ctx, cancel := context.WithTimeout(parent, restoreAuditTimeout)
	defer cancel()
	write := s.restoreAuditWrite
	if write == nil {
		if s.Store == nil {
			return errors.New("restore audit store is unavailable")
		}
		write = s.Store.LogEvent
	}
	return write(ctx, store.Event{Category: "audit", Severity: severity, Event: event, Detail: detail})
}

func (m *restoreManager) list(now time.Time) ([]restoreUploadDTO, []restorePreviewDTO) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	uploads := make([]restoreUploadDTO, 0, len(m.uploads))
	for i := len(m.uploadOrder) - 1; i >= 0; i-- {
		if upload := m.uploads[m.uploadOrder[i]]; upload != nil {
			uploads = append(uploads, upload.restoreUploadDTO)
		}
	}
	previews := make([]restorePreviewDTO, 0, len(m.previews))
	for i := len(m.previewOrder) - 1; i >= 0; i-- {
		if preview := m.previews[m.previewOrder[i]]; preview != nil {
			previews = append(previews, preview.restorePreviewDTO)
		}
	}
	return uploads, previews
}

func (m *restoreManager) preview(id string, now time.Time) (restorePreviewDTO, error) {
	if !validBackupID(id) {
		return restorePreviewDTO{}, errRestorePreviewNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.previews[id]
	if job == nil {
		return restorePreviewDTO{}, errRestorePreviewNotFound
	}
	return job.restorePreviewDTO, nil
}

func (m *restoreManager) cancelPreview(id string, now time.Time, adminID int64,
	username string) (restorePreviewDTO, error) {
	if !validBackupID(id) {
		return restorePreviewDTO{}, errRestorePreviewNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	job := m.previews[id]
	if job == nil {
		return restorePreviewDTO{}, errRestorePreviewNotFound
	}
	if job.State == "completed" || job.State == "failed" || job.State == "cancelled" {
		return restorePreviewDTO{}, errRestorePreviewTerminal
	}
	job.cancelRequestedAdminID, job.cancelRequestedUser, job.cancelRequested = adminID, username, true
	job.cancel()
	return job.restorePreviewDTO, nil
}

func (m *restoreManager) makeUploadRoomLocked() bool {
	for len(m.uploads)+m.uploading >= restoreHistoryLimit {
		removed := false
		for i, id := range m.uploadOrder {
			if m.uploadReferencedLocked(id) {
				continue
			}
			upload := m.uploads[id]
			if !m.removeUploadLocked(upload) {
				continue
			}
			delete(m.uploads, id)
			m.uploadOrder = append(m.uploadOrder[:i], m.uploadOrder[i+1:]...)
			removed = true
			break
		}
		if !removed {
			return false
		}
	}
	return true
}

func (m *restoreManager) makePreviewRoomLocked() bool {
	for len(m.previews) >= restoreHistoryLimit {
		removed := false
		for i, id := range m.previewOrder {
			if id == m.active || id == m.confirming {
				continue
			}
			delete(m.previews, id)
			m.previewOrder = append(m.previewOrder[:i], m.previewOrder[i+1:]...)
			removed = true
			break
		}
		if !removed {
			return false
		}
	}
	return true
}

func (m *restoreManager) uploadReferencedLocked(uploadID string) bool {
	for _, preview := range m.previews {
		if preview.UploadID == uploadID {
			return true
		}
	}
	return false
}

func (m *restoreManager) removeUploadLocked(upload *restoreUpload) bool {
	if upload == nil {
		return true
	}
	current, err := m.uploadsRoot.Lstat(upload.name)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || !os.SameFile(current, upload.identity) {
		return false
	}
	return removeIdentityFile(m.uploadsRoot, upload.name, upload.identity) == nil
}

func (m *restoreManager) pruneLocked(now time.Time) {
	previews := m.previewOrder[:0]
	for _, id := range m.previewOrder {
		job := m.previews[id]
		if job == nil {
			continue
		}
		if job.ExpiresAt != nil && now.UnixMilli() >= *job.ExpiresAt && id != m.active && id != m.confirming {
			delete(m.previews, id)
			continue
		}
		previews = append(previews, id)
	}
	m.previewOrder = previews
	uploads := m.uploadOrder[:0]
	for _, id := range m.uploadOrder {
		upload := m.uploads[id]
		if upload == nil {
			continue
		}
		if now.UnixMilli() >= upload.ExpiresAt && !m.uploadReferencedLocked(id) && m.removeUploadLocked(upload) {
			delete(m.uploads, id)
			continue
		}
		uploads = append(uploads, id)
	}
	m.uploadOrder = uploads
	_ = m.makePreviewRoomLocked()
	_ = m.makeUploadRoomLocked()
}

func (m *restoreManager) close(timeout time.Duration) bool {
	m.mu.Lock()
	if m.cleaned {
		m.mu.Unlock()
		return true
	}
	if !m.closed {
		m.closed = true
		m.cancel()
		for _, job := range m.previews {
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
	if !m.directoriesCurrent() {
		m.mu.Unlock()
		return false
	}
	for _, upload := range m.uploads {
		if !m.removeUploadLocked(upload) {
			m.mu.Unlock()
			return false
		}
	}
	if err := cleanupOwnedRestoreEntries(m.uploadsRoot, "uploads"); err != nil {
		m.mu.Unlock()
		return false
	}
	if err := m.cleanupScratchLocked(); err != nil {
		m.mu.Unlock()
		return false
	}
	m.uploads, m.uploadOrder = map[string]*restoreUpload{}, nil
	m.previews, m.previewOrder, m.active, m.confirming = map[string]*restorePreviewJob{}, nil, "", ""
	m.cleaned = true
	m.mu.Unlock()
	return errors.Join(m.scratchRoot.Close(), m.uploadsRoot.Close(), m.dirRoot.Close()) == nil
}

func (m *restoreManager) cleanupScratchLocked() error {
	return cleanupOwnedRestoreEntries(m.scratchRoot, "scratch")
}

func cleanupOwnedRestoreEntries(root *os.Root, kind string) error {
	entries, err := readRootEntries(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !ownedRestoreEntry(kind, entry) {
			return fmt.Errorf("restore %s contains unowned entry %q", kind, entry.Name())
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return syncRoot(root)
}

func (m *restoreManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cleaned {
		return
	}
	m.pruneLocked(now)
}

func (s *Server) auditRestore(event, severity string, adminID int64, username, idKey, id, state string) {
	_ = s.auditRestoreDurable(context.Background(), event, severity, map[string]any{
		idKey: id, "actor_admin_id": adminID, "actor_username": username, "state": state,
	})
}

type restoreContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *restoreContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
