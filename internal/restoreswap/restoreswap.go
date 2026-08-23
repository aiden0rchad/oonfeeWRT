// Package restoreswap coordinates a controller restore only while the live
// database and key keeper are closed. It never contacts or configures routers.
package restoreswap

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	_ "modernc.org/sqlite"
)

const (
	databaseName       = "oonfeewrt.db"
	keyringName        = "keyring.json"
	recoveryDirName    = ".oonfeewrt-recovery"
	markerName         = "pending-restore.json"
	suppressionName    = ".oonfeewrt-router-writes-suppressed.json"
	markerFormat       = "oonfeewrt-restore-swap"
	markerVersion      = 1
	markerMaxBytes     = 64 << 10
	keyringMaxBytes    = 4096
	passphraseMaxBytes = 4096
	instanceMaxBytes   = 128
	controllerMaxBytes = 128
)

var (
	ErrNoPendingIntent = errors.New("restore swap: no pending restore intent")
	ErrUncleanIntent   = errors.New("restore swap: intent was not promoted by its creating controller instance")
	coordinatorMu      sync.Mutex
)

// HasPending reports whether a valid durable restore marker exists. Startup
// uses it before passphrase acquisition so a crash-partial canonical pair is
// routed to ApplyPending instead of mistaken for an unrelated missing file.
func HasPending(dataDir string) (bool, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	_, err = readMarker(root)
	if errors.Is(err, ErrNoPendingIntent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// OwnsUncleanIntent reports whether ownerInstanceID created the durable intent
// which still awaits a clean shutdown. It never promotes or mutates the marker.
func OwnsUncleanIntent(dataDir, ownerInstanceID string) (bool, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	if err := validateInstanceID(ownerInstanceID); err != nil {
		return false, err
	}
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	m, err := readMarker(root)
	if errors.Is(err, ErrNoPendingIntent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return false, err
	}
	return m.State == stateIntent && constantTimeStringEqual(m.OwnerInstanceID, ownerInstanceID), nil
}

// PreparedPair is a fully migrated and validated pair in one private,
// immediate-child directory of dataDir. CreateIntent takes ownership after it
// durably publishes the marker.
type PreparedPair struct {
	DatabasePath        string
	KeyringPath         string
	AuthorizingAdminID  int64
	AuthorizingUsername string
	PreviewID           string
	PlanID              string
}

// IntentResult contains only non-secret identifiers and digests.
type IntentResult struct {
	ID                     string
	MarkerPath             string
	PreparedDatabaseSHA256 string
	PreparedKeyringSHA256  string
}

// IntentRetainedError means CreateIntent published a durable marker but could
// not prove that rolling it back succeeded. The prepared pair is therefore
// owned by the marker and must not be cleaned by the caller.
type IntentRetainedError struct {
	cause    error
	rollback error
}

func (e *IntentRetainedError) Error() string {
	return "restore swap: durable intent ownership was retained after a post-publication failure"
}

func (e *IntentRetainedError) Unwrap() error { return e.cause }

// IntentOwnershipRetained reports the only CreateIntent error for which the
// durable marker, rather than the caller, still owns the prepared pair.
func IntentOwnershipRetained(err error) bool {
	var retained *IntentRetainedError
	return errors.As(err, &retained)
}

// Result describes a verified applied restore. SafetyBackup is a basename in
// the controller recovery directory, never an absolute host path.
type Result struct {
	Applied                bool
	RestoreID              string
	SafetyBackup           string
	SafetyBackupSHA256     string
	PreparedDatabaseSHA256 string
	PreparedKeyringSHA256  string
	AuthorizingAdminID     int64
	AuthorizingUsername    string
	PreviewID              string
	PlanID                 string
	Counts                 recovery.Counts
}

type operations struct {
	now            func() time.Time
	randomID       func() (string, error)
	boundary       func(string) error
	publishIntent  func(*os.Root, marker) error
	rollbackIntent func(*os.Root) error
}

func defaultOperations() operations {
	return operations{now: time.Now, randomID: newID}
}

// CreateIntent seals the caller-owned export passphrase and records the
// immutable prepared-pair hashes. It does not stop or replace live state.
func CreateIntent(ctx context.Context, dataDir string, prepared PreparedPair,
	old *secrets.Keeper, exportPassphrase []byte, ownerInstanceID string) (IntentResult, error) {
	return createIntent(ctx, dataDir, prepared, old, exportPassphrase, ownerInstanceID, defaultOperations())
}

func createIntent(ctx context.Context, dataDir string, prepared PreparedPair,
	old *secrets.Keeper, exportPassphrase []byte, ownerInstanceID string,
	ops operations) (IntentResult, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	if err := validContext(ctx); err != nil {
		return IntentResult{}, err
	}
	if old == nil {
		return IntentResult{}, errors.New("restore swap: live secrets keeper is unavailable")
	}
	if err := validateInstanceID(ownerInstanceID); err != nil {
		return IntentResult{}, err
	}
	passphrase, err := ownPassphrase(exportPassphrase)
	if err != nil {
		return IntentResult{}, err
	}
	defer clear(passphrase)

	root, absData, err := openDataRoot(dataDir)
	if err != nil {
		return IntentResult{}, err
	}
	defer root.Close()
	if err := ensureRecoveryDir(root); err != nil {
		return IntentResult{}, err
	}
	if _, err := root.Lstat(filepath.Join(recoveryDirName, markerName)); err == nil {
		return IntentResult{}, fmt.Errorf("restore swap: pending marker already exists: %w", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return IntentResult{}, fmt.Errorf("restore swap: inspect pending marker: %w", err)
	}

	preparedPath, err := inspectPrepared(ctx, root, absData, prepared)
	if err != nil {
		return IntentResult{}, err
	}
	id, err := ops.randomID()
	if err != nil {
		return IntentResult{}, err
	}
	created := ops.now().UTC()
	if created.IsZero() || created.Year() < 1 || created.Year() > 9999 {
		return IntentResult{}, errors.New("restore swap: current time is invalid")
	}
	m := marker{
		Format: markerFormat, Version: markerVersion, ID: id, State: stateIntent,
		OwnerInstanceID: ownerInstanceID, CreatedAt: created.Format(time.RFC3339Nano),
		PreparedDir: preparedPath.dir, PreparedDatabase: preparedPath.database,
		PreparedKeyring: preparedPath.keyring, PreparedDatabaseFile: preparedPath.databaseFile,
		PreparedKeyringFile: preparedPath.keyringFile,
		AuthorizingAdminID:  prepared.AuthorizingAdminID,
		AuthorizingUsername: prepared.AuthorizingUsername,
		PreviewID:           prepared.PreviewID, PlanID: prepared.PlanID,
	}
	aad := intentAAD(m)
	sealed, err := old.Seal(passphrase, aad)
	clear(aad)
	if err != nil {
		return IntentResult{}, fmt.Errorf("restore swap: seal export passphrase: %w", err)
	}
	m.SealedExportPassphrase = base64.StdEncoding.EncodeToString(sealed)
	clear(sealed)
	if err := checkNamedDataRoot(root); err != nil {
		return IntentResult{}, err
	}
	publish := writeMarkerNew
	if ops.publishIntent != nil {
		publish = ops.publishIntent
	}
	if err := publish(root, m); err != nil {
		return reconcileIntentPublicationFailure(root, absData, m, err, ops)
	}
	if err := hitBoundary(ops, "intent-published"); err != nil {
		return rollbackPublishedIntent(root, absData, m, err, ops)
	}
	if err := checkNamedDataRoot(root); err != nil {
		return rollbackPublishedIntent(root, absData, m, err, ops)
	}
	return intentResult(absData, m), nil
}

func rollbackPublishedIntent(root *os.Root, absData string, m marker, cause error,
	ops operations) (IntentResult, error) {
	current, err := readMarker(root)
	if err != nil || current != m {
		return intentResult(absData, m), &IntentRetainedError{cause: cause, rollback: err}
	}
	rollback := removeMarker
	if ops.rollbackIntent != nil {
		rollback = ops.rollbackIntent
	}
	if err := rollback(root); err != nil {
		return intentResult(absData, m), &IntentRetainedError{cause: cause, rollback: err}
	}
	return IntentResult{}, cause
}

func reconcileIntentPublicationFailure(root *os.Root, absData string, m marker, cause error,
	ops operations) (IntentResult, error) {
	info, err := root.Lstat(filepath.Join(recoveryDirName, markerName))
	if errors.Is(err, os.ErrNotExist) {
		return IntentResult{}, cause
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 {
		return intentResult(absData, m), &IntentRetainedError{cause: cause, rollback: err}
	}
	return rollbackPublishedIntent(root, absData, m, cause, ops)
}

// MarkCleanShutdown promotes only the intent created by ownerInstanceID. The
// caller must first checkpoint and close the live DB, then close its Keeper.
func MarkCleanShutdown(ctx context.Context, dataDir, ownerInstanceID string) (IntentResult, error) {
	return markCleanShutdown(ctx, dataDir, ownerInstanceID, defaultOperations())
}

func markCleanShutdown(ctx context.Context, dataDir, ownerInstanceID string,
	ops operations) (IntentResult, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	if err := validContext(ctx); err != nil {
		return IntentResult{}, err
	}
	if err := validateInstanceID(ownerInstanceID); err != nil {
		return IntentResult{}, err
	}
	root, absData, err := openDataRoot(dataDir)
	if err != nil {
		return IntentResult{}, err
	}
	defer root.Close()
	m, err := readMarker(root)
	if err != nil {
		return IntentResult{}, err
	}
	if m.State != stateIntent {
		return IntentResult{}, errors.New("restore swap: pending marker is not an unpromoted intent")
	}
	if !constantTimeStringEqual(m.OwnerInstanceID, ownerInstanceID) {
		return IntentResult{}, ErrUncleanIntent
	}
	if err := verifyPrepared(ctx, root, m); err != nil {
		return IntentResult{}, err
	}
	if err := removeSafeSQLiteSidecars(root); err != nil {
		return IntentResult{}, err
	}
	database, err := hashRootFile(ctx, root, databaseName, 1, portablebackup.MaxDatabaseBytes, true)
	if err != nil {
		return IntentResult{}, fmt.Errorf("restore swap: inspect closed controller database: %w", err)
	}
	keyring, err := hashRootFile(ctx, root, keyringName, 1, keyringMaxBytes, true)
	if err != nil {
		return IntentResult{}, fmt.Errorf("restore swap: inspect closed controller keyring: %w", err)
	}
	if err := ensureSameFilesystem(root, ".", databaseName); err != nil {
		return IntentResult{}, err
	}
	if err := ensureSameFilesystem(root, ".", keyringName); err != nil {
		return IntentResult{}, err
	}
	if err := syncDirectory(root, "."); err != nil {
		return IntentResult{}, err
	}
	m.OldDatabase = database
	m.OldKeyring = keyring
	m.State = stateReady
	if err := checkNamedDataRoot(root); err != nil {
		return IntentResult{}, err
	}
	if err := replaceMarker(root, m); err != nil {
		return IntentResult{}, err
	}
	if err := hitBoundary(ops, "ready-published"); err != nil {
		return IntentResult{}, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return IntentResult{}, err
	}
	return intentResult(absData, m), nil
}

// AbortUnclean removes only an unpromoted marker's still-matching prepared
// pair. It never reads, removes, or renames canonical controller files.
func AbortUnclean(ctx context.Context, dataDir string) error {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	if err := validContext(ctx); err != nil {
		return err
	}
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	m, err := readMarker(root)
	if err != nil {
		return err
	}
	if m.State != stateIntent {
		return errors.New("restore swap: only an unclean intent can be aborted")
	}
	if err := removePreparedOwned(ctx, root, m, true); err != nil {
		return err
	}
	return removeMarker(root)
}

// ApplyPending applies only a clean-shutdown marker. It is intended to run at
// process startup before any controller database or Keeper is opened.
func ApplyPending(ctx context.Context, dataDir string, runtimePassphrase []byte,
	controllerVersion string) (Result, error) {
	return applyPending(ctx, dataDir, runtimePassphrase, controllerVersion, defaultOperations())
}

func applyPending(ctx context.Context, dataDir string, runtimePassphrase []byte,
	controllerVersion string, ops operations) (Result, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	if err := validContext(ctx); err != nil {
		return Result{}, err
	}
	if err := validateControllerVersion(controllerVersion); err != nil {
		return Result{}, err
	}
	runtime, err := ownPassphrase(runtimePassphrase)
	if err != nil {
		return Result{}, err
	}
	defer clear(runtime)
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return Result{}, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return Result{}, err
	}
	defer root.Close()
	m, err := readMarker(root)
	if err != nil {
		return Result{}, err
	}
	if m.State == stateIntent {
		return Result{}, ErrUncleanIntent
	}
	if m.State == stateCleanup {
		return finishCleanup(ctx, root, m, runtime, ops)
	}
	return applyReady(ctx, root, m, runtime, controllerVersion, ops)
}

func validContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("restore swap: context is nil")
	}
	return ctx.Err()
}

func ownPassphrase(passphrase []byte) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("restore swap: passphrase is empty")
	}
	if len(passphrase) > passphraseMaxBytes {
		return nil, fmt.Errorf("restore swap: passphrase exceeds %d-byte ceiling", passphraseMaxBytes)
	}
	return append([]byte(nil), passphrase...), nil
}

func validateInstanceID(value string) error {
	if !validBoundedText(value, 1, instanceMaxBytes) {
		return errors.New("restore swap: controller instance ID is invalid")
	}
	return nil
}

func validateControllerVersion(value string) error {
	if !validBoundedText(value, 1, controllerMaxBytes) {
		return errors.New("restore swap: controller version is invalid")
	}
	return nil
}

func validBoundedText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || !utf8.ValidString(value) ||
		strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func constantTimeStringEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func intentResult(absData string, m marker) IntentResult {
	return IntentResult{
		ID: m.ID, MarkerPath: filepath.Join(absData, recoveryDirName, markerName),
		PreparedDatabaseSHA256: m.PreparedDatabaseFile.SHA256,
		PreparedKeyringSHA256:  m.PreparedKeyringFile.SHA256,
	}
}

func hitBoundary(ops operations, name string) error {
	if ops.boundary == nil {
		return nil
	}
	if err := ops.boundary(name); err != nil {
		return &injectedBoundaryError{name: name, err: err}
	}
	return nil
}
