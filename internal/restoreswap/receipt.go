package restoreswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
)

const (
	appliedReceiptName    = "restore-applied-receipt.json"
	appliedReceiptFormat  = "oonfeewrt-restore-applied"
	appliedReceiptVersion = 1
	safetyRetentionCount  = 3
)

var ErrNoAppliedReceipt = errors.New("restore swap: no unapplied audit receipt")

type appliedReceipt struct {
	Format                 string      `json:"format"`
	Version                int         `json:"version"`
	RestoreID              string      `json:"restore_id"`
	CreatedAt              string      `json:"created_at"`
	SafetyBackup           string      `json:"safety_backup"`
	SafetyBackupSHA256     string      `json:"safety_backup_sha256"`
	PreparedDatabaseSHA256 string      `json:"database_sha256"`
	PreparedKeyringSHA256  string      `json:"keyring_sha256"`
	AuthorizingAdminID     int64       `json:"authorizing_admin_id"`
	AuthorizingUsername    string      `json:"authorizing_username"`
	PreviewID              string      `json:"preview_id"`
	PlanID                 string      `json:"plan_id"`
	Counts                 recoveryDTO `json:"counts"`
}

func receiptFor(m marker) appliedReceipt {
	return appliedReceipt{
		Format: appliedReceiptFormat, Version: appliedReceiptVersion,
		RestoreID: m.ID, CreatedAt: m.SafetyBackupCreatedAt,
		SafetyBackup: m.safetyName(), SafetyBackupSHA256: m.SafetyBackupFile.SHA256,
		PreparedDatabaseSHA256: m.PreparedDatabaseFile.SHA256,
		PreparedKeyringSHA256:  m.PreparedKeyringFile.SHA256,
		AuthorizingAdminID:     m.AuthorizingAdminID, AuthorizingUsername: m.AuthorizingUsername,
		PreviewID: m.PreviewID, PlanID: m.PlanID,
		Counts: m.ValidatedCounts,
	}
}

func validateAppliedReceipt(value appliedReceipt) error {
	created, err := time.Parse(time.RFC3339Nano, value.CreatedAt)
	if value.Format != appliedReceiptFormat || value.Version != appliedReceiptVersion ||
		!validRestoreID(value.RestoreID) || err != nil || created.Location() != time.UTC ||
		value.SafetyBackup != "safety-"+value.RestoreID+".oowrtbak" ||
		!validFileRecord(fileRecord{Size: 1, SHA256: value.SafetyBackupSHA256}, 1, 1) ||
		!validFileRecord(fileRecord{Size: 1, SHA256: value.PreparedDatabaseSHA256}, 1, 1) ||
		!validFileRecord(fileRecord{Size: 1, SHA256: value.PreparedKeyringSHA256}, 1, 1) ||
		value.AuthorizingAdminID <= 0 || !validBoundedText(value.AuthorizingUsername, 1, 128) ||
		!validBoundedText(value.PreviewID, 1, 128) || !validBoundedText(value.PlanID, 1, 128) ||
		value.Counts.Schema != storeCurrentSchema() || !validCounts(value.Counts) {
		return errors.New("restore swap: applied audit receipt is invalid")
	}
	return nil
}

func readAppliedReceipt(root *os.Root) (appliedReceipt, error) {
	var value appliedReceipt
	data, err := readRootRegular(root, filepath.Join(recoveryDirName, appliedReceiptName),
		1, markerMaxBytes, true)
	if errors.Is(err, os.ErrNotExist) {
		return value, ErrNoAppliedReceipt
	}
	if err != nil {
		return value, err
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, errors.New("restore swap: applied audit receipt is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value, errors.New("restore swap: applied audit receipt is malformed")
	}
	if err := validateAppliedReceipt(value); err != nil {
		return value, err
	}
	return value, nil
}

func ensureAppliedReceipt(root *os.Root, m marker) error {
	want := receiptFor(m)
	if got, err := readAppliedReceipt(root); err == nil {
		if got != want {
			return errors.New("restore swap: an earlier applied audit receipt is still pending")
		}
		return nil
	} else if !errors.Is(err, ErrNoAppliedReceipt) {
		return err
	}
	return writeJSONAtomic(root, filepath.Join(recoveryDirName, appliedReceiptName), want, true)
}

// PendingAppliedReceipt returns the nonsecret restore result that must be
// durably audited before the controller serves requests.
func PendingAppliedReceipt(dataDir string) (Result, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	value, err := readAppliedReceipt(root)
	if err != nil {
		return Result{}, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return Result{}, err
	}
	return value.result(), nil
}

// ClearAppliedReceipt acknowledges restoreID only after its audit event has
// committed. A failed clear leaves the receipt available for the next boot.
func ClearAppliedReceipt(ctx context.Context, dataDir, restoreID string) error {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	if err := validContext(ctx); err != nil {
		return err
	}
	if !validRestoreID(restoreID) {
		return errors.New("restore swap: restore ID is invalid")
	}
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	value, err := readAppliedReceipt(root)
	if err != nil {
		return err
	}
	if !constantTimeStringEqual(value.RestoreID, restoreID) {
		return errors.New("restore swap: applied audit receipt belongs to another restore")
	}
	if err := requireHash(ctx, root, filepath.Join(recoveryDirName, value.SafetyBackup),
		fileRecord{Size: fileSizeFromReceipt(root, value.SafetyBackup), SHA256: value.SafetyBackupSHA256},
		portablebackup.MaxDatabaseBytes+(16<<20)); err != nil {
		return errors.New("restore swap: applied audit receipt safety artifact is missing or changed")
	}
	if err := pruneSafetyArtifacts(root, value.RestoreID); err != nil {
		return err
	}
	if err := root.Remove(filepath.Join(recoveryDirName, appliedReceiptName)); err != nil {
		return err
	}
	if err := syncDirectory(root, recoveryDirName); err != nil {
		return err
	}
	return checkNamedDataRoot(root)
}

func pruneSafetyArtifacts(root *os.Root, currentRestoreID string) error {
	preserve := map[string]bool{currentRestoreID: true}
	if m, err := readMarker(root); err == nil {
		preserve[m.ID] = true
	} else if !errors.Is(err, ErrNoPendingIntent) {
		return err
	}
	if receipt, err := readAppliedReceipt(root); err == nil {
		preserve[receipt.RestoreID] = true
	} else if !errors.Is(err, ErrNoAppliedReceipt) {
		return err
	}
	if suppression, err := readSuppression(root); err == nil {
		preserve[suppression.RestoreID] = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	directory, err := root.Open(recoveryDirName)
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	type candidate struct {
		name string
		id   string
		when time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "safety-") || !strings.HasSuffix(name, ".oowrtbak") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "safety-"), ".oowrtbak")
		if !validRestoreID(id) {
			continue
		}
		info, err := root.Lstat(filepath.Join(recoveryDirName, name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			info.Mode().Perm() != 0o600 || info.Size() <= 0 ||
			uint64(info.Size()) > portablebackup.MaxDatabaseBytes+(16<<20) {
			continue
		}
		candidates = append(candidates, candidate{name: name, id: id, when: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].when.Equal(candidates[j].when) {
			return candidates[i].name > candidates[j].name
		}
		return candidates[i].when.After(candidates[j].when)
	})
	keep := make(map[string]bool, safetyRetentionCount)
	for _, item := range candidates {
		if preserve[item.id] {
			keep[item.name] = true
		}
	}
	for _, item := range candidates {
		if len(keep) >= safetyRetentionCount {
			break
		}
		keep[item.name] = true
	}
	removed := false
	for _, item := range candidates {
		if keep[item.name] {
			continue
		}
		if err := root.Remove(filepath.Join(recoveryDirName, item.name)); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(root, recoveryDirName)
	}
	return nil
}

func fileSizeFromReceipt(root *os.Root, safety string) uint64 {
	info, err := root.Lstat(filepath.Join(recoveryDirName, safety))
	if err != nil || info.Size() < 0 {
		return 0
	}
	return uint64(info.Size())
}

func (value appliedReceipt) result() Result {
	return Result{
		Applied: true, RestoreID: value.RestoreID, SafetyBackup: value.SafetyBackup,
		SafetyBackupSHA256:     value.SafetyBackupSHA256,
		PreparedDatabaseSHA256: value.PreparedDatabaseSHA256,
		PreparedKeyringSHA256:  value.PreparedKeyringSHA256,
		AuthorizingAdminID:     value.AuthorizingAdminID, AuthorizingUsername: value.AuthorizingUsername,
		PreviewID: value.PreviewID, PlanID: value.PlanID,
		Counts: recovery.Counts{Schema: value.Counts.Schema, Devices: value.Counts.Devices,
			Credentials: value.Counts.Credentials, OwnedSections: value.Counts.OwnedSections,
			WLANs: value.Counts.WLANs, Meshes: value.Counts.Meshes},
	}
}
