package restoreswap

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
)

type markerState string

const (
	stateIntent           markerState = "intent"
	stateReady            markerState = "ready"
	stateSafety           markerState = "safety"
	stateOldDBParked      markerState = "old_db_parked"
	stateOldPairParked    markerState = "old_pair_parked"
	stateNewDBPublished   markerState = "new_db_published"
	stateNewPairPublished markerState = "new_pair_published"
	stateValidated        markerState = "validated"
	stateSuppressed       markerState = "suppressed"
	stateCleanup          markerState = "cleanup"
)

type fileRecord struct {
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

type marker struct {
	Format                 string      `json:"format"`
	Version                int         `json:"version"`
	ID                     string      `json:"id"`
	State                  markerState `json:"state"`
	OwnerInstanceID        string      `json:"owner_instance_id"`
	CreatedAt              string      `json:"created_at"`
	PreparedDir            string      `json:"prepared_dir"`
	PreparedDatabase       string      `json:"prepared_database"`
	PreparedKeyring        string      `json:"prepared_keyring"`
	PreparedDatabaseFile   fileRecord  `json:"prepared_database_file"`
	PreparedKeyringFile    fileRecord  `json:"prepared_keyring_file"`
	AuthorizingAdminID     int64       `json:"authorizing_admin_id"`
	AuthorizingUsername    string      `json:"authorizing_username"`
	PreviewID              string      `json:"preview_id"`
	PlanID                 string      `json:"plan_id"`
	SealedExportPassphrase string      `json:"sealed_export_passphrase"`
	OldDatabase            fileRecord  `json:"old_database,omitempty"`
	OldKeyring             fileRecord  `json:"old_keyring,omitempty"`
	SafetyBackupFile       fileRecord  `json:"safety_backup_file,omitempty"`
	SafetyBackupCreatedAt  string      `json:"safety_backup_created_at,omitempty"`
	ValidatedCounts        recoveryDTO `json:"validated_counts,omitempty"`
}

type recoveryDTO struct {
	Schema        int `json:"schema,omitempty"`
	Devices       int `json:"devices,omitempty"`
	Credentials   int `json:"credentials,omitempty"`
	OwnedSections int `json:"owned_sections,omitempty"`
	WLANs         int `json:"wlans,omitempty"`
	Meshes        int `json:"meshes,omitempty"`
}

type suppressionMarker struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	RestoreID string `json:"restore_id"`
	CreatedAt string `json:"created_at"`
	Reason    string `json:"reason"`
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("restore swap: generate restore ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (m marker) safetyName() string   { return "safety-" + m.ID + ".oowrtbak" }
func (m marker) rollbackName() string { return "rollback-" + m.ID }

func intentAAD(m marker) []byte {
	// Length framing avoids delimiter ambiguity and makes all immutable path
	// selectors and hashes part of the passphrase seal's purpose.
	values := []string{
		markerFormat, strconv.Itoa(markerVersion), m.ID, m.OwnerInstanceID,
		m.PreparedDir, m.PreparedDatabase, m.PreparedKeyring,
		strconv.FormatUint(m.PreparedDatabaseFile.Size, 10), m.PreparedDatabaseFile.SHA256,
		strconv.FormatUint(m.PreparedKeyringFile.Size, 10), m.PreparedKeyringFile.SHA256,
		strconv.FormatInt(m.AuthorizingAdminID, 10), m.AuthorizingUsername,
		m.PreviewID, m.PlanID,
	}
	var out bytes.Buffer
	out.WriteString("oonfeewrt/restore-swap/intent/v1")
	for _, value := range values {
		out.WriteByte(0)
		out.WriteString(strconv.Itoa(len(value)))
		out.WriteByte(':')
		out.WriteString(value)
	}
	return out.Bytes()
}

func readMarker(root *os.Root) (marker, error) {
	var m marker
	if _, err := root.Lstat(recoveryDirName); errors.Is(err, os.ErrNotExist) {
		return m, ErrNoPendingIntent
	}
	if err := validateRecoveryDir(root); err != nil {
		return m, err
	}
	data, err := readRootRegular(root, filepath.Join(recoveryDirName, markerName), 1, markerMaxBytes, true)
	if errors.Is(err, os.ErrNotExist) {
		return m, ErrNoPendingIntent
	}
	if err != nil {
		return m, fmt.Errorf("restore swap: read pending marker: %w", err)
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return marker{}, errors.New("restore swap: pending marker is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return marker{}, errors.New("restore swap: pending marker has trailing data")
	}
	if err := validateMarker(m); err != nil {
		return marker{}, err
	}
	return m, nil
}

func validateMarker(m marker) error {
	if m.Format != markerFormat || m.Version != markerVersion {
		return errors.New("restore swap: pending marker format is unsupported")
	}
	if !validRestoreID(m.ID) {
		return errors.New("restore swap: pending marker restore ID is invalid")
	}
	if err := validateInstanceID(m.OwnerInstanceID); err != nil {
		return errors.New("restore swap: pending marker instance ID is invalid")
	}
	created, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
	if err != nil || created.Location() != time.UTC || created.Year() < 1 || created.Year() > 9999 {
		return errors.New("restore swap: pending marker creation time is invalid")
	}
	if !safeBaseName(m.PreparedDir) || !safeBaseName(m.PreparedDatabase) ||
		!safeBaseName(m.PreparedKeyring) || m.PreparedDatabase == m.PreparedKeyring ||
		m.PreparedDir == recoveryDirName {
		return errors.New("restore swap: pending marker prepared paths are invalid")
	}
	if !validFileRecord(m.PreparedDatabaseFile, 1, portablebackup.MaxDatabaseBytes) ||
		!validFileRecord(m.PreparedKeyringFile, 1, keyringMaxBytes) {
		return errors.New("restore swap: pending marker prepared hashes are invalid")
	}
	if m.AuthorizingAdminID <= 0 || !validBoundedText(m.AuthorizingUsername, 1, 128) {
		return errors.New("restore swap: pending marker audit actor is invalid")
	}
	if !validBoundedText(m.PreviewID, 1, 128) || !validBoundedText(m.PlanID, 1, 128) {
		return errors.New("restore swap: pending marker restore binding is invalid")
	}
	sealed, err := base64.StdEncoding.DecodeString(m.SealedExportPassphrase)
	if err != nil || len(sealed) < 1+24+16 || len(sealed) > passphraseMaxBytes+1+24+16 {
		clear(sealed)
		return errors.New("restore swap: pending marker sealed passphrase is invalid")
	}
	clear(sealed)
	if !validState(m.State) {
		return errors.New("restore swap: pending marker state is invalid")
	}
	if m.State != stateIntent && (!validFileRecord(m.OldDatabase, 1, portablebackup.MaxDatabaseBytes) ||
		!validFileRecord(m.OldKeyring, 1, keyringMaxBytes)) {
		return errors.New("restore swap: pending marker old-pair hashes are invalid")
	}
	if m.State == stateIntent && (m.OldDatabase != (fileRecord{}) || m.OldKeyring != (fileRecord{})) {
		return errors.New("restore swap: pending marker contains premature old-pair state")
	}
	if stateAtLeast(m.State, stateSafety) {
		if !validFileRecord(m.SafetyBackupFile, 1, portablebackup.MaxDatabaseBytes+(16<<20)) {
			return errors.New("restore swap: pending marker safety artifact is invalid")
		}
		safetyTime, err := time.Parse(time.RFC3339Nano, m.SafetyBackupCreatedAt)
		if err != nil || safetyTime.Location() != time.UTC {
			return errors.New("restore swap: pending marker safety time is invalid")
		}
	} else if m.SafetyBackupFile != (fileRecord{}) || m.SafetyBackupCreatedAt != "" {
		return errors.New("restore swap: pending marker contains premature safety state")
	}
	if stateAtLeast(m.State, stateValidated) {
		if m.ValidatedCounts.Schema != storeCurrentSchema() || !validCounts(m.ValidatedCounts) {
			return errors.New("restore swap: pending marker validation counts are invalid")
		}
	} else if m.ValidatedCounts != (recoveryDTO{}) {
		return errors.New("restore swap: pending marker contains premature validation state")
	}
	return nil
}

func validState(state markerState) bool {
	for _, candidate := range orderedStates {
		if state == candidate {
			return true
		}
	}
	return false
}

var orderedStates = []markerState{
	stateIntent, stateReady, stateSafety, stateOldDBParked, stateOldPairParked,
	stateNewDBPublished, stateNewPairPublished, stateValidated, stateSuppressed, stateCleanup,
}

func stateAtLeast(state, threshold markerState) bool {
	left, right := -1, -1
	for index, candidate := range orderedStates {
		if state == candidate {
			left = index
		}
		if threshold == candidate {
			right = index
		}
	}
	return left >= right && right >= 0
}

func validFileRecord(record fileRecord, minimum, maximum uint64) bool {
	if record.Size < minimum || record.Size > maximum || len(record.SHA256) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(record.SHA256)
	return err == nil && len(decoded) == 32 && record.SHA256 == strings.ToLower(record.SHA256)
}

func validCounts(value recoveryDTO) bool {
	return value.Schema > 0 && value.Devices >= 0 && value.Credentials >= 0 &&
		value.OwnedSections >= 0 && value.WLANs >= 0 && value.Meshes >= 0
}

func writeMarkerNew(root *os.Root, m marker) error {
	if err := validateMarker(m); err != nil {
		return err
	}
	return writeJSONAtomic(root, filepath.Join(recoveryDirName, markerName), m, true)
}

func replaceMarker(root *os.Root, m marker) error {
	if err := validateMarker(m); err != nil {
		return err
	}
	return writeJSONAtomic(root, filepath.Join(recoveryDirName, markerName), m, false)
}

func removeMarker(root *os.Root) error {
	path := filepath.Join(recoveryDirName, markerName)
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("restore swap: pending marker is unsafe to remove")
	}
	if err := root.Remove(path); err != nil {
		return fmt.Errorf("restore swap: remove pending marker: %w", err)
	}
	return syncDirectory(root, recoveryDirName)
}
