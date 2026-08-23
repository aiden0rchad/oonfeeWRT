package restoreswap

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	suppressionFormat = "oonfeewrt-router-write-suppression"
	suppressionReason = "restored controller state requires owner review before router writes"
)

// Suppression is the non-secret external router-write fence. It survives
// restore cleanup and controller restarts until explicitly cleared.
type Suppression struct {
	Active    bool
	RestoreID string
	CreatedAt time.Time
	Reason    string
}

// SuppressionStatus reads the durable router-write fence without opening the
// controller database or keyring.
func SuppressionStatus(dataDir string) (Suppression, error) {
	coordinatorMu.Lock()
	defer coordinatorMu.Unlock()
	root, _, err := openDataRoot(dataDir)
	if err != nil {
		return Suppression{}, err
	}
	defer root.Close()
	value, err := readSuppression(root)
	if errors.Is(err, os.ErrNotExist) {
		return Suppression{}, checkNamedDataRoot(root)
	}
	if err != nil {
		return Suppression{}, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return Suppression{}, err
	}
	created, _ := time.Parse(time.RFC3339Nano, value.CreatedAt)
	return Suppression{Active: true, RestoreID: value.RestoreID,
		CreatedAt: created, Reason: value.Reason}, nil
}

// ClearSuppression clears only the fence created by restoreID. The caller is
// responsible for owner authorization and review before invoking it.
func ClearSuppression(ctx context.Context, dataDir, restoreID string) error {
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
	if err := checkNamedDataRoot(root); err != nil {
		return err
	}
	value, err := readSuppression(root)
	if err != nil {
		return err
	}
	if !constantTimeStringEqual(value.RestoreID, restoreID) {
		return errors.New("restore swap: router-write suppression belongs to another restore")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Remove(suppressionName); err != nil {
		return fmt.Errorf("restore swap: clear router-write suppression: %w", err)
	}
	if err := syncDirectory(root, "."); err != nil {
		return err
	}
	return checkNamedDataRoot(root)
}

func ensureSuppression(root *os.Root, m marker, created time.Time) error {
	if existing, err := readSuppression(root); err == nil {
		if !constantTimeStringEqual(existing.RestoreID, m.ID) {
			return errors.New("restore swap: an earlier router-write suppression is still active")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if created.IsZero() || created.Year() < 1 || created.Year() > 9999 {
		return errors.New("restore swap: suppression creation time is invalid")
	}
	value := suppressionMarker{
		Format: suppressionFormat, Version: 1, RestoreID: m.ID,
		CreatedAt: created.Format(time.RFC3339Nano), Reason: suppressionReason,
	}
	return writeJSONAtomic(root, suppressionName, value, true)
}

func readSuppression(root *os.Root) (suppressionMarker, error) {
	var value suppressionMarker
	data, err := readRootRegular(root, suppressionName, 1, markerMaxBytes, true)
	if err != nil {
		return value, err
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, errors.New("restore swap: router-write suppression is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value, errors.New("restore swap: router-write suppression has trailing data")
	}
	created, err := time.Parse(time.RFC3339Nano, value.CreatedAt)
	if value.Format != suppressionFormat || value.Version != 1 ||
		!validRestoreID(value.RestoreID) || err != nil || created.Location() != time.UTC ||
		value.Reason != suppressionReason {
		return value, errors.New("restore swap: router-write suppression is invalid")
	}
	return value, nil
}

func validRestoreID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 {
		return false
	}
	var nonzero byte
	for _, item := range decoded {
		nonzero |= item
	}
	return nonzero != 0
}
