package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

const testExportPassphrase = "placeholder-backup-export-passphrase"

func newBackupHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.BackupsDir = filepath.Join(t.TempDir(), "backups")
	h.srv.ControllerVersion = "backup-api-test"
	if err := h.srv.InitBackups(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !h.srv.CloseJobs(10 * time.Second) {
			t.Error("backup jobs did not close")
		}
	})
	h.setup()
	return h
}

func backupRequestBody(passphrase, confirmation string) map[string]any {
	return map[string]any{
		"plan_id": backupExportPlanID, "acknowledge_sensitive_content": true,
		"export_passphrase": passphrase, "confirm_export_passphrase": confirmation,
	}
}

func backupDo(h *harness, method, path string, body any, remote string, tlsState bool) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = testSetupHost
	req.RemoteAddr = remote
	if tlsState {
		req.TLS = &tls.ConnectionState{}
	}
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	if h.csrf != "" {
		req.Header.Set(csrfHeader, h.csrf)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

func loopbackBackupDo(h *harness, method, path string, body any) *httptest.ResponseRecorder {
	return backupDo(h, method, path, body, "127.0.0.1:12345", false)
}

func startBackup(t *testing.T, h *harness, passphrase, confirmation string) backupJobDTO {
	t.Helper()
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/backups",
		backupRequestBody(passphrase, confirmation))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status=%d body=%s", w.Code, w.Body.String())
	}
	var body backupJobResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Job
}

func waitBackupState(t *testing.T, h *harness, id string, states ...string) backupJobDTO {
	t.Helper()
	wanted := make(map[string]bool, len(states))
	for _, state := range states {
		wanted[state] = true
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		w := loopbackBackupDo(h, http.MethodGet, "/api/v1/backups/"+id, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("job status=%d body=%s", w.Code, w.Body.String())
		}
		var body backupJobResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if wanted[body.Job.State] {
			return body.Job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backup %s did not reach %v", id, states)
	return backupJobDTO{}
}

func TestBackupExportRoundTripAndFrozenDisclosure(t *testing.T) {
	h := newBackupHarness(t)
	w := loopbackBackupDo(h, http.MethodGet, "/api/v1/backups", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("descriptor status=%d body=%s", w.Code, w.Body.String())
	}
	var descriptor backupsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.Descriptor.PlanID != backupExportPlanID || descriptor.Descriptor.FormatVersion != 1 ||
		descriptor.Descriptor.FileExtension != ".oowrtbak" || len(descriptor.Descriptor.Includes) != 3 ||
		len(descriptor.Descriptor.Excludes) != 3 || descriptor.Disclosure.RouterManagementCalls ||
		descriptor.Disclosure.RouterChanges || descriptor.Disclosure.AutomaticRouterApply ||
		!descriptor.Disclosure.SeparateExportPassphrase || descriptor.Disclosure.ExportPassphraseRecoverable ||
		descriptor.Limits.History != backupHistoryLimit ||
		descriptor.Limits.MaxDatabaseBytes != controllerPortableDatabaseMaxBytes ||
		descriptor.Limits.MaxArtifactBytes != controllerPortableArtifactMaxBytes || descriptor.Jobs == nil {
		t.Fatalf("unexpected backup descriptor: %+v", descriptor)
	}

	started := startBackup(t, h, testExportPassphrase, testExportPassphrase)
	completed := waitBackupState(t, h, started.ID, "completed")
	if completed.SizeBytes == nil || *completed.SizeBytes <= 0 || !validSHA256Hex(completed.SHA256) ||
		completed.SchemaVersion == nil || completed.ControllerVersion != "backup-api-test" {
		t.Fatalf("completed job=%+v", completed)
	}
	artifactPath := filepath.Join(h.srv.BackupsDir, started.ID+".oowrtbak")
	if info, err := os.Lstat(artifactPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(h.srv.BackupsDir, started.ID+".snapshot.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext snapshot remains: %v", err)
	}

	w = loopbackBackupDo(h, http.MethodGet, "/api/v1/backups/"+started.ID+"/download", nil)
	if w.Code != http.StatusOK || int64(w.Body.Len()) != *completed.SizeBytes ||
		w.Header().Get("Content-Length") == "" || !strings.Contains(w.Header().Get("Content-Disposition"), ".oowrtbak") {
		t.Fatalf("download status=%d headers=%v bytes=%d body=%s", w.Code, w.Header(), w.Body.Len(), w.Body.String())
	}
	digest := sha256.Sum256(w.Body.Bytes())
	if hex.EncodeToString(digest[:]) != completed.SHA256 {
		t.Fatal("download hash differs from the frozen job descriptor")
	}
	downloaded := filepath.Join(t.TempDir(), "downloaded.oowrtbak")
	if err := os.WriteFile(downloaded, w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := portablebackup.Extract(context.Background(), downloaded, t.TempDir(), []byte(testExportPassphrase))
	if err != nil {
		t.Fatalf("extract downloaded artifact: %v", err)
	}
	defer stage.Cleanup()
	if stage.Manifest.SchemaVersion != *completed.SchemaVersion || stage.Manifest.ControllerVersion != completed.ControllerVersion {
		t.Fatalf("manifest=%+v job=%+v", stage.Manifest, completed)
	}
	db, err := sql.Open("sqlite", stage.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var owners int
	if err := db.QueryRow(`SELECT count(*) FROM admins WHERE role='owner' AND enabled=1 AND deleted_at IS NULL`).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("restored owner count=%d err=%v", owners, err)
	}
}

func TestBackupPortableLimitsMatchRestoreAdmission(t *testing.T) {
	h := newBackupHarness(t)
	createCalled := false
	h.srv.backupCreate = func(context.Context, string, string, *secrets.Keeper,
		[]byte, portablebackup.Metadata) (portablebackup.Result, error) {
		createCalled = true
		return portablebackup.Result{}, errors.New("should not run")
	}
	h.srv.afterBackupSnapshot = func(id string) {
		path := filepath.Join(h.srv.BackupsDir, id+".snapshot.db")
		if err := os.Truncate(path, controllerPortableDatabaseMaxBytes+1); err != nil {
			t.Error(err)
		}
	}
	started := startBackup(t, h, testExportPassphrase, testExportPassphrase)
	failed := waitBackupState(t, h, started.ID, "failed")
	if createCalled || failed.SizeBytes != nil || failed.SHA256 != "" {
		t.Fatalf("oversize database reached export create=%v job=%+v", createCalled, failed)
	}
	if entries, err := os.ReadDir(h.srv.BackupsDir); err != nil || len(entries) != 0 {
		t.Fatalf("oversize export residue=%v err=%v", entries, err)
	}
	if restoreUploadMaxBytes != controllerPortableArtifactMaxBytes {
		t.Fatalf("restore max=%d export max=%d", restoreUploadMaxBytes, controllerPortableArtifactMaxBytes)
	}

	h = newBackupHarness(t)
	h.srv.backupCreate = func(_ context.Context, outputPath, _ string, _ *secrets.Keeper,
		_ []byte, meta portablebackup.Metadata) (portablebackup.Result, error) {
		if err := os.WriteFile(outputPath, []byte("small"), 0o600); err != nil {
			return portablebackup.Result{}, err
		}
		return portablebackup.Result{
			Path: outputPath, Size: controllerPortableArtifactMaxBytes + 1,
			SHA256: strings.Repeat("a", 64),
			Manifest: portablebackup.Manifest{SchemaVersion: meta.SchemaVersion,
				ControllerVersion: meta.ControllerVersion},
		}, nil
	}
	started = startBackup(t, h, testExportPassphrase, testExportPassphrase)
	failed = waitBackupState(t, h, started.ID, "failed")
	if failed.SizeBytes != nil || failed.SHA256 != "" {
		t.Fatalf("oversize artifact was published: %+v", failed)
	}
	if entries, err := os.ReadDir(h.srv.BackupsDir); err != nil || len(entries) != 0 {
		t.Fatalf("oversize artifact residue=%v err=%v", entries, err)
	}
}

func TestBackupRequiresDirectSecureTransportAndOwner(t *testing.T) {
	h := newBackupHarness(t)
	w := backupDo(h, http.MethodGet, "/api/v1/backups", nil, "192.0.2.10:12345", false)
	if w.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain remote status=%d body=%s", w.Code, w.Body.String())
	}
	assertCodedError(t, w, http.StatusUpgradeRequired, "secure_transport_required")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil)
	req.Host, req.RemoteAddr = testSetupHost, "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	assertCodedError(t, w, http.StatusUpgradeRequired, "secure_transport_required")
	w = backupDo(h, http.MethodGet, "/api/v1/backups", nil, "192.0.2.10:12345", true)
	if w.Code != http.StatusOK {
		t.Fatalf("TLS status=%d body=%s", w.Code, w.Body.String())
	}

	_, viewer := seedAccount(t, h, "backup-viewer", "viewer")
	token, sess, err := h.srv.sessions.create(viewer.ID, viewer.Username, viewer.Role,
		"127.0.0.1", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
	h.csrf = sess.csrf
	w = loopbackBackupDo(h, http.MethodGet, "/api/v1/backups", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBackupPassphraseMismatchAndCapacityFailBeforeJob(t *testing.T) {
	h := newBackupHarness(t)
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/backups",
		backupRequestBody(testExportPassphrase, testExportPassphrase+"-typo"))
	assertCodedError(t, w, http.StatusBadRequest, "export_passphrase_mismatch")
	if jobs := h.srv.backups.list(h.srv.now()); len(jobs) != 0 {
		t.Fatalf("mismatch created jobs: %+v", jobs)
	}
	if entries, err := os.ReadDir(h.srv.BackupsDir); err != nil || len(entries) != 0 {
		t.Fatalf("mismatch files=%v err=%v", entries, err)
	}
	if len(h.srv.ActiveOperations()) != 0 || len(h.srv.hashing) != 0 {
		t.Fatalf("mismatch leaked admission: operations=%v hashing=%d", h.srv.ActiveOperations(), len(h.srv.hashing))
	}

	for range cap(h.srv.hashing) {
		h.srv.hashing <- struct{}{}
	}
	defer func() {
		for len(h.srv.hashing) > 0 {
			<-h.srv.hashing
		}
	}()
	w = loopbackBackupDo(h, http.MethodPost, "/api/v1/backups",
		backupRequestBody(testExportPassphrase, testExportPassphrase))
	assertCodedError(t, w, http.StatusServiceUnavailable, "backup_capacity_busy")
	if w.Header().Get("Retry-After") != "2" || len(h.srv.backups.list(h.srv.now())) != 0 || len(h.srv.ActiveOperations()) != 0 {
		t.Fatalf("capacity response headers=%v jobs=%v operations=%v", w.Header(), h.srv.backups.list(h.srv.now()), h.srv.ActiveOperations())
	}
	if entries, err := os.ReadDir(h.srv.BackupsDir); err != nil || len(entries) != 0 {
		t.Fatalf("capacity files=%v err=%v", entries, err)
	}
}

func TestBackupOneActiveCancellationDrainsLeaseAndSecretWork(t *testing.T) {
	h := newBackupHarness(t)
	createStarted := make(chan struct{})
	var workerSecret []byte
	h.srv.backupCreate = func(ctx context.Context, _, _ string, _ *secrets.Keeper,
		passphrase []byte, _ portablebackup.Metadata) (portablebackup.Result, error) {
		if string(passphrase) != testExportPassphrase {
			t.Error("worker did not receive the owned export passphrase")
		}
		workerSecret = passphrase
		close(createStarted)
		<-ctx.Done()
		return portablebackup.Result{}, ctx.Err()
	}
	started := startBackup(t, h, testExportPassphrase, testExportPassphrase)
	select {
	case <-createStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("backup create did not start")
	}
	if len(h.srv.hashing) != 1 || !containsString(h.srv.ActiveOperations(), "backup") {
		t.Fatalf("active resources hashing=%d operations=%v", len(h.srv.hashing), h.srv.ActiveOperations())
	}
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/backups",
		backupRequestBody(testExportPassphrase, testExportPassphrase))
	assertCodedError(t, w, http.StatusConflict, "backup_in_progress")
	if len(h.srv.hashing) != 1 || h.srv.operations.active[operationBackup] != 1 {
		t.Fatalf("rejected second job leaked admission: hashing=%d backup_leases=%d",
			len(h.srv.hashing), h.srv.operations.active[operationBackup])
	}
	w = loopbackBackupDo(h, http.MethodPost, "/api/v1/backups/"+started.ID+"/cancel", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", w.Code, w.Body.String())
	}
	job := waitBackupState(t, h, started.ID, "cancelled")
	deadline := time.Now().Add(3 * time.Second)
	for (len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if job.Error != "" || len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 {
		t.Fatalf("cancelled job=%+v hashing=%d operations=%v", job, len(h.srv.hashing), h.srv.ActiveOperations())
	}
	if !bytes.Equal(workerSecret, make([]byte, len(workerSecret))) {
		t.Fatal("worker export passphrase was not cleared")
	}
	if entries, err := os.ReadDir(h.srv.BackupsDir); err != nil || len(entries) != 0 {
		t.Fatalf("cancel cleanup files=%v err=%v", entries, err)
	}
}

func TestBackupDownloadRejectsModeAndHashChanges(t *testing.T) {
	h := newBackupHarness(t)
	h.srv.backupCreate = fakeBackupCreate
	started := startBackup(t, h, testExportPassphrase, testExportPassphrase)
	completed := waitBackupState(t, h, started.ID, "completed")
	path := filepath.Join(h.srv.BackupsDir, started.ID+".oowrtbak")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	w := loopbackBackupDo(h, http.MethodGet, "/api/v1/backups/"+started.ID+"/download", nil)
	assertCodedError(t, w, http.StatusInternalServerError, "backup_download_failed")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	w = loopbackBackupDo(h, http.MethodGet, "/api/v1/backups/"+started.ID+"/download", nil)
	assertCodedError(t, w, http.StatusInternalServerError, "backup_download_failed")
	if completed.SizeBytes == nil || int64(len(data)) != *completed.SizeBytes {
		t.Fatal("tamper test did not preserve the exact artifact size")
	}
}

func TestBackupDownloadRequiresFreshReauthenticationBeforeFileOpen(t *testing.T) {
	h := newBackupHarness(t)
	h.srv.backupCreate = fakeBackupCreate
	started := startBackup(t, h, testExportPassphrase, testExportPassphrase)
	_ = waitBackupState(t, h, started.ID, "completed")
	opened := false
	h.srv.beforeBackupDownloadOpen = func(string) { opened = true }
	base := h.srv.now()
	h.srv.Now = func() time.Time { return base.Add(reauthValidity + time.Second) }
	w := loopbackBackupDo(h, http.MethodGet, "/api/v1/backups/"+started.ID+"/download", nil)
	assertCodedError(t, w, http.StatusPreconditionRequired, "reauth_required")
	if opened {
		t.Fatal("expired reauthentication reached the artifact open boundary")
	}
}

func TestBackupHistoryRetentionAndStartupOwnership(t *testing.T) {
	h := newBackupHarness(t)
	now := h.srv.now()
	var oldestPath string
	for i := 0; i < backupHistoryLimit+2; i++ {
		id, err := randomToken()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(h.srv.BackupsDir, id+".oowrtbak")
		if err := os.WriteFile(path, []byte("encrypted"), 0o600); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldestPath = path
		}
		h.srv.backups.jobs[id] = &backupJob{backupJobDTO: backupJobDTO{
			ID: id, State: "completed", CreatedAt: now.Add(time.Duration(i) * time.Second).UnixMilli(),
		}, path: path, cancel: func() {}}
		h.srv.backups.order = append(h.srv.backups.order, id)
	}
	if jobs := h.srv.backups.list(now); len(jobs) != backupHistoryLimit {
		t.Fatalf("retained jobs=%d want=%d", len(jobs), backupHistoryLimit)
	}
	if _, err := os.Stat(oldestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest artifact was not pruned: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operator-file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareBackupsDir(dir); err == nil {
		t.Fatal("backup initialization accepted an unowned file")
	}
}

func TestBackupRefusesToExceedHistoryWhenCleanupIsBlocked(t *testing.T) {
	h := newBackupHarness(t)
	for i := 0; i < backupHistoryLimit; i++ {
		id, err := randomToken()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(h.srv.BackupsDir, id+".retained")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "busy"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		h.srv.backups.jobs[id] = &backupJob{backupJobDTO: backupJobDTO{
			ID: id, State: "completed", CreatedAt: int64(i + 1),
		}, path: path, cancel: func() {}}
		h.srv.backups.order = append(h.srv.backups.order, id)
	}
	w := loopbackBackupDo(h, http.MethodPost, "/api/v1/backups",
		backupRequestBody(testExportPassphrase, testExportPassphrase))
	assertCodedError(t, w, http.StatusServiceUnavailable, "backup_retention_blocked")
	if len(h.srv.backups.jobs) != backupHistoryLimit || h.srv.backups.running != 0 ||
		len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 {
		t.Fatalf("blocked retention jobs=%d running=%d hashing=%d operations=%v",
			len(h.srv.backups.jobs), h.srv.backups.running, len(h.srv.hashing), h.srv.ActiveOperations())
	}
}

func TestBackupCloseZeroTimeoutDistinguishesIdleAndBusy(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		h := newBackupHarness(t)
		if !h.srv.backups.close(0) {
			t.Fatal("idle backup manager did not close with a zero timeout")
		}
		if _, err := os.Stat(h.srv.BackupsDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("idle close retained the export directory: %v", err)
		}
	})
	t.Run("busy", func(t *testing.T) {
		h := newBackupHarness(t)
		startedCreate := make(chan struct{})
		unblock := make(chan struct{})
		h.srv.backupCreate = func(ctx context.Context, outputPath, databasePath string,
			keeper *secrets.Keeper, passphrase []byte, meta portablebackup.Metadata) (portablebackup.Result, error) {
			close(startedCreate)
			<-unblock
			return fakeBackupCreate(ctx, outputPath, databasePath, keeper, passphrase, meta)
		}
		job := startBackup(t, h, testExportPassphrase, testExportPassphrase)
		select {
		case <-startedCreate:
		case <-time.After(5 * time.Second):
			t.Fatalf("backup %s did not reach portable creation", job.ID)
		}
		closedWhileBusy := h.srv.backups.close(0)
		close(unblock)
		drained := h.srv.backups.close(3 * time.Second)
		if closedWhileBusy || !drained {
			t.Fatalf("close results busy=%v drained=%v", closedWhileBusy, drained)
		}
		if len(h.srv.hashing) != 0 || len(h.srv.ActiveOperations()) != 0 {
			t.Fatalf("close leaked resources hashing=%d operations=%v", len(h.srv.hashing), h.srv.ActiveOperations())
		}
	})
}

func TestSecretBytesUnicodeAndBounds(t *testing.T) {
	raw := []byte(`"abcdefghijklmnop\uD83D\uDE00"`)
	copyRaw := append([]byte(nil), raw...)
	var secret secretBytes
	if err := secret.UnmarshalJSON(copyRaw); err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	if string(secret) != "abcdefghijklmnop😀" {
		t.Fatalf("decoded secret=%q", secret)
	}
	if !bytes.Equal(copyRaw, make([]byte, len(copyRaw))) {
		t.Fatal("raw JSON secret buffer was not cleared")
	}
	if _, err := decodeJSONString([]byte(`"bad\uD800"`), 4096); err == nil {
		t.Fatal("invalid surrogate was accepted")
	}
	if err := validateExportPassphrase(bytes.Repeat([]byte("x"), maxExportPassphraseBytes+1)); err == nil {
		t.Fatal("oversize export passphrase was accepted")
	}
}

func fakeBackupCreate(_ context.Context, outputPath, _ string, _ *secrets.Keeper,
	_ []byte, meta portablebackup.Metadata) (portablebackup.Result, error) {
	data := []byte("fake-encrypted-portable-backup")
	if err := os.WriteFile(outputPath, data, 0o600); err != nil {
		return portablebackup.Result{}, err
	}
	digest := sha256.Sum256(data)
	return portablebackup.Result{
		Path: outputPath, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		Manifest: portablebackup.Manifest{
			Format: "oonfeewrt-portable-backup", Version: 1, CreatedAt: meta.CreatedAt,
			ControllerVersion: meta.ControllerVersion, SchemaVersion: meta.SchemaVersion,
		},
	}, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
