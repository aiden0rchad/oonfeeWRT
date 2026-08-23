package restoreswap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type injectedBoundaryError struct {
	name string
	err  error
}

func (e *injectedBoundaryError) Error() string {
	return "restore swap: boundary " + e.name + ": " + e.err.Error()
}
func (e *injectedBoundaryError) Unwrap() error { return e.err }

func applyReady(ctx context.Context, root *os.Root, m marker, runtime []byte,
	controllerVersion string, ops operations) (result Result, retErr error) {
	oldDatabasePath, err := locateExpected(ctx, root, m.OldDatabase,
		databaseName, filepath.Join(recoveryDirName, m.rollbackName(), databaseName), portablebackup.MaxDatabaseBytes)
	if err != nil {
		return result, err
	}
	oldKeyringPath, err := locateExpected(ctx, root, m.OldKeyring,
		keyringName, filepath.Join(recoveryDirName, m.rollbackName(), keyringName), keyringMaxBytes)
	if err != nil {
		return result, err
	}
	if oldDatabasePath == databaseName && oldKeyringPath == keyringName {
		if err := verifyPrepared(ctx, root, m); err != nil {
			return result, err
		}
	}
	if err := checkNamedDataRoot(root); err != nil {
		return result, err
	}
	oldKeeper, err := secrets.Open(filepath.Join(root.Name(), oldKeyringPath), runtime)
	if err != nil {
		return result, fmt.Errorf("restore swap: open old controller keyring: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, oldKeeper.Close()) }()
	if err := checkNamedDataRoot(root); err != nil {
		return result, err
	}
	if err := requireHash(ctx, root, oldKeyringPath, m.OldKeyring, keyringMaxBytes); err != nil {
		return result, errors.New("restore swap: old keyring changed while opening")
	}
	aad := intentAAD(m)
	sealed, err := base64.StdEncoding.DecodeString(m.SealedExportPassphrase)
	if err != nil {
		clear(aad)
		return result, errors.New("restore swap: decode sealed export passphrase failed")
	}
	exportPassphrase, err := oldKeeper.Unseal(sealed, aad)
	clear(sealed)
	clear(aad)
	if err != nil || len(exportPassphrase) == 0 || len(exportPassphrase) > passphraseMaxBytes {
		clear(exportPassphrase)
		return result, errors.New("restore swap: sealed export passphrase authentication failed")
	}
	defer clear(exportPassphrase)

	oldCounts, err := validatePair(ctx, root, oldDatabasePath, oldKeyringPath, runtime,
		m.OldDatabase, m.OldKeyring)
	if err != nil {
		return result, fmt.Errorf("restore swap: old controller pair failed validation: %w", err)
	}
	if !stateAtLeast(m.State, stateSafety) {
		if oldDatabasePath != databaseName || oldKeyringPath != keyringName {
			return result, errors.New("restore swap: swap started before a safety artifact was recorded")
		}
		m, err = ensureSafety(ctx, root, m, oldKeeper, exportPassphrase,
			controllerVersion, oldCounts.Schema, ops)
		if err != nil {
			return result, err
		}
	} else if err := verifySafety(ctx, root, m, exportPassphrase); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	fail := func(cause error) (Result, error) {
		var injected *injectedBoundaryError
		if errors.As(cause, &injected) {
			return Result{}, cause
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rollbackErr := restoreOld(rollbackCtx, root, &m, runtime)
		return Result{}, errors.Join(cause, rollbackErr)
	}

	if err := ensureRollbackDirectory(root, m); err != nil {
		return fail(err)
	}
	if err := parkOld(ctx, root, m, databaseName, m.PreparedDatabaseFile,
		m.OldDatabase, filepath.Join(recoveryDirName, m.rollbackName(), databaseName),
		"old-database-renamed", ops); err != nil {
		return fail(err)
	}
	if m, err = advance(root, m, stateOldDBParked, ops); err != nil {
		return fail(err)
	}
	if err := parkOld(ctx, root, m, keyringName, m.PreparedKeyringFile,
		m.OldKeyring, filepath.Join(recoveryDirName, m.rollbackName(), keyringName),
		"old-keyring-renamed", ops); err != nil {
		return fail(err)
	}
	if m, err = advance(root, m, stateOldPairParked, ops); err != nil {
		return fail(err)
	}
	if err := publishNew(ctx, root, m, databaseName, m.PreparedDatabase,
		m.PreparedDatabaseFile, m.OldDatabase, "new-database-renamed", ops); err != nil {
		return fail(err)
	}
	if m, err = advance(root, m, stateNewDBPublished, ops); err != nil {
		return fail(err)
	}
	if err := publishNew(ctx, root, m, keyringName, m.PreparedKeyring,
		m.PreparedKeyringFile, m.OldKeyring, "new-keyring-renamed", ops); err != nil {
		return fail(err)
	}
	if m, err = advance(root, m, stateNewPairPublished, ops); err != nil {
		return fail(err)
	}
	counts, err := validatePair(ctx, root, databaseName, keyringName, runtime,
		m.PreparedDatabaseFile, m.PreparedKeyringFile)
	if err != nil {
		return fail(fmt.Errorf("restore swap: prepared controller pair failed validation: %w", err))
	}
	m.ValidatedCounts = countsToDTO(counts)
	if m, err = advance(root, m, stateValidated, ops); err != nil {
		return fail(err)
	}
	if err := ensureSuppression(root, m, ops.now().UTC()); err != nil {
		return fail(err)
	}
	if err := hitBoundary(ops, "suppression-published"); err != nil {
		return fail(err)
	}
	if m, err = advance(root, m, stateSuppressed, ops); err != nil {
		return fail(err)
	}
	if m, err = advance(root, m, stateCleanup, ops); err != nil {
		return fail(err)
	}
	return finishCleanup(ctx, root, m, runtime, ops)
}

func ensureSafety(ctx context.Context, root *os.Root, m marker, keeper *secrets.Keeper,
	exportPassphrase []byte, controllerVersion string, schema int, ops operations) (marker, error) {
	if err := ctx.Err(); err != nil {
		return m, err
	}
	name := m.safetyName()
	rel := filepath.Join(recoveryDirName, name)
	abs := filepath.Join(root.Name(), rel)
	created := ops.now().UTC()
	if created.IsZero() || created.Year() < 1 || created.Year() > 9999 {
		return m, errors.New("restore swap: current time is invalid")
	}
	if _, err := root.Lstat(rel); errors.Is(err, os.ErrNotExist) {
		if err := checkNamedDataRoot(root); err != nil {
			return m, err
		}
		backup, err := portablebackup.Create(ctx, abs, filepath.Join(root.Name(), databaseName),
			keeper, exportPassphrase, portablebackup.Metadata{
				ControllerVersion: controllerVersion, SchemaVersion: schema, CreatedAt: created,
			})
		if err != nil {
			return m, fmt.Errorf("restore swap: create pre-restore safety artifact: %w", err)
		}
		if err := checkNamedDataRoot(root); err != nil {
			return m, err
		}
		if err := requireHash(ctx, root, databaseName, m.OldDatabase, portablebackup.MaxDatabaseBytes); err != nil {
			return m, errors.New("restore swap: old database changed while creating safety artifact")
		}
		m.SafetyBackupFile = fileRecord{Size: uint64(backup.Size), SHA256: backup.SHA256}
		m.SafetyBackupCreatedAt = created.Format(time.RFC3339Nano)
		if err := hitBoundary(ops, "safety-artifact-published"); err != nil {
			return m, err
		}
	} else if err != nil {
		return m, fmt.Errorf("restore swap: inspect safety artifact: %w", err)
	} else {
		record, err := authenticateUnrecordedSafety(ctx, root, m, exportPassphrase)
		if err != nil {
			return m, err
		}
		m.SafetyBackupFile = record
		m.SafetyBackupCreatedAt = created.Format(time.RFC3339Nano)
	}
	if err := verifySafety(ctx, root, m, exportPassphrase); err != nil {
		return m, err
	}
	m.State = stateSafety
	if err := replaceMarker(root, m); err != nil {
		return m, err
	}
	if err := hitBoundary(ops, "safety-state-published"); err != nil {
		return m, err
	}
	return m, nil
}

func authenticateUnrecordedSafety(ctx context.Context, root *os.Root, m marker,
	exportPassphrase []byte) (fileRecord, error) {
	rel := filepath.Join(recoveryDirName, m.safetyName())
	record, err := hashRootFile(ctx, root, rel, 1, portablebackup.MaxDatabaseBytes+(16<<20), false)
	if err != nil {
		return fileRecord{}, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return fileRecord{}, err
	}
	stage, err := portablebackup.Extract(ctx, filepath.Join(root.Name(), rel),
		filepath.Join(root.Name(), recoveryDirName), exportPassphrase)
	if err != nil {
		return fileRecord{}, fmt.Errorf("restore swap: authenticate recovered safety artifact: %w", err)
	}
	if err := checkNamedDataRoot(root); err != nil {
		return fileRecord{}, errors.Join(err, stage.Cleanup())
	}
	if after, err := hashRootFile(ctx, root, rel, 1, portablebackup.MaxDatabaseBytes+(16<<20), false); err != nil || after != record {
		return fileRecord{}, errors.Join(errors.New("restore swap: safety artifact changed while authenticating"), stage.Cleanup())
	}
	if stage.Manifest.Database.SHA256 != m.OldDatabase.SHA256 ||
		stage.Manifest.Database.Size != m.OldDatabase.Size {
		return fileRecord{}, errors.Join(errors.New("restore swap: safety artifact does not contain the old controller database"), stage.Cleanup())
	}
	if err := stage.Cleanup(); err != nil {
		return fileRecord{}, fmt.Errorf("restore swap: clean safety verification stage: %w", err)
	}
	return record, nil
}

func verifySafety(ctx context.Context, root *os.Root, m marker, exportPassphrase []byte) error {
	rel := filepath.Join(recoveryDirName, m.safetyName())
	record, err := hashRootFile(ctx, root, rel, 1, portablebackup.MaxDatabaseBytes+(16<<20), false)
	if err != nil || record != m.SafetyBackupFile {
		return errors.New("restore swap: pre-restore safety artifact changed")
	}
	if err := checkNamedDataRoot(root); err != nil {
		return err
	}
	stage, err := portablebackup.Extract(ctx, filepath.Join(root.Name(), rel),
		filepath.Join(root.Name(), recoveryDirName), exportPassphrase)
	if err != nil {
		return fmt.Errorf("restore swap: authenticate pre-restore safety artifact: %w", err)
	}
	if err := checkNamedDataRoot(root); err != nil {
		return errors.Join(err, stage.Cleanup())
	}
	if after, err := hashRootFile(ctx, root, rel, 1, portablebackup.MaxDatabaseBytes+(16<<20), false); err != nil || after != record {
		return errors.Join(errors.New("restore swap: safety artifact changed while authenticating"), stage.Cleanup())
	}
	if stage.Manifest.Database.SHA256 != m.OldDatabase.SHA256 ||
		stage.Manifest.Database.Size != m.OldDatabase.Size {
		return errors.Join(errors.New("restore swap: safety artifact does not contain the old controller database"), stage.Cleanup())
	}
	return stage.Cleanup()
}

func ensureRollbackDirectory(root *os.Root, m marker) error {
	path := filepath.Join(recoveryDirName, m.rollbackName())
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("restore swap: create rollback directory: %w", err)
		}
		return syncDirectory(root, recoveryDirName)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("restore swap: rollback directory is unsafe")
	}
	return nil
}

func parkOld(ctx context.Context, root *os.Root, m marker, canonical string,
	newRecord, oldRecord fileRecord, rollback, boundary string, ops operations) error {
	if newRecord == oldRecord {
		return requireHash(ctx, root, canonical, oldRecord, maxFor(canonical))
	}
	rollbackState, err := pathRecord(ctx, root, rollback, oldRecord, maxFor(canonical))
	if err != nil {
		return err
	}
	canonicalState, err := pathRecord(ctx, root, canonical, oldRecord, maxFor(canonical))
	if err != nil {
		return err
	}
	if rollbackState == matchExpected {
		if canonicalState != matchMissing {
			candidate, candidateErr := hashRootFile(ctx, root, canonical, 1, maxFor(canonical), false)
			if candidateErr != nil || candidate != newRecord {
				return errors.New("restore swap: canonical member conflicts with parked rollback")
			}
		}
		return nil
	}
	if rollbackState != matchMissing || canonicalState != matchExpected {
		return errors.New("restore swap: old controller member is missing or changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameNoClobber(root, canonical, rollback); err != nil {
		return err
	}
	if err := syncRenameParents(root, canonical, rollback); err != nil {
		return err
	}
	return hitBoundary(ops, boundary)
}

func publishNew(ctx context.Context, root *os.Root, m marker, canonical, preparedName string,
	newRecord, oldRecord fileRecord, boundary string, ops operations) error {
	if newRecord == oldRecord {
		return requireHash(ctx, root, canonical, newRecord, maxFor(canonical))
	}
	canonicalState, err := pathRecord(ctx, root, canonical, newRecord, maxFor(canonical))
	if err != nil {
		return err
	}
	if canonicalState == matchExpected {
		return nil
	}
	if canonicalState != matchMissing {
		return errors.New("restore swap: canonical destination is not empty before publication")
	}
	prepared := filepath.Join(m.PreparedDir, preparedName)
	if err := requireHash(ctx, root, prepared, newRecord, maxFor(canonical)); err != nil {
		return errors.New("restore swap: prepared member is missing or changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameNoClobber(root, prepared, canonical); err != nil {
		return err
	}
	if err := syncRenameParents(root, prepared, canonical); err != nil {
		return err
	}
	return hitBoundary(ops, boundary)
}

func advance(root *os.Root, m marker, state markerState, ops operations) (marker, error) {
	if stateAtLeast(m.State, state) {
		return m, nil
	}
	m.State = state
	if err := replaceMarker(root, m); err != nil {
		return m, err
	}
	if err := hitBoundary(ops, string(state)+"-state-published"); err != nil {
		return m, err
	}
	return m, nil
}

func validatePair(ctx context.Context, root *os.Root, database, keyring string,
	runtime []byte, wantDatabase, wantKeyring fileRecord) (counts recovery.Counts, retErr error) {
	if err := requireHash(ctx, root, database, wantDatabase, portablebackup.MaxDatabaseBytes); err != nil {
		return counts, err
	}
	if err := requireHash(ctx, root, keyring, wantKeyring, keyringMaxBytes); err != nil {
		return counts, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return counts, err
	}
	keeper, err := secrets.Open(filepath.Join(root.Name(), keyring), runtime)
	if err != nil {
		return counts, err
	}
	defer func() { retErr = errors.Join(retErr, keeper.Close()) }()
	if err := checkNamedDataRoot(root); err != nil {
		return counts, err
	}
	if err := requireHash(ctx, root, keyring, wantKeyring, keyringMaxBytes); err != nil {
		return counts, errors.New("restore swap: keyring changed while opening")
	}
	db, err := store.OpenClosedReadOnly(ctx, "sqlite", filepath.Join(root.Name(), database), keeper)
	if err != nil {
		return counts, err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	if err := checkNamedDataRoot(root); err != nil {
		return counts, err
	}
	if err := requireHash(ctx, root, database, wantDatabase, portablebackup.MaxDatabaseBytes); err != nil {
		return counts, errors.New("restore swap: database changed while opening")
	}
	counts, err = recovery.Validate(ctx, db, keeper)
	if err != nil {
		return counts, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return counts, err
	}
	if err := requireHash(ctx, root, database, wantDatabase, portablebackup.MaxDatabaseBytes); err != nil {
		return counts, errors.New("restore swap: database changed while validating")
	}
	if err := requireHash(ctx, root, keyring, wantKeyring, keyringMaxBytes); err != nil {
		return counts, errors.New("restore swap: keyring changed while validating")
	}
	return counts, nil
}

func finishCleanup(ctx context.Context, root *os.Root, m marker, runtime []byte,
	ops operations) (Result, error) {
	if err := checkNamedDataRoot(root); err != nil {
		return Result{}, err
	}
	if err := requireHash(ctx, root, filepath.Join(recoveryDirName, m.safetyName()),
		m.SafetyBackupFile, portablebackup.MaxDatabaseBytes+(16<<20)); err != nil {
		return Result{}, errors.New("restore swap: retained safety artifact is missing or changed")
	}
	counts, err := validatePair(ctx, root, databaseName, keyringName, runtime,
		m.PreparedDatabaseFile, m.PreparedKeyringFile)
	if err != nil {
		return Result{}, fmt.Errorf("restore swap: final controller pair failed validation: %w", err)
	}
	if countsToDTO(counts) != m.ValidatedCounts {
		return Result{}, errors.New("restore swap: final validation inventory changed")
	}
	suppression, err := readSuppression(root)
	if err != nil {
		return Result{}, err
	}
	if !constantTimeStringEqual(suppression.RestoreID, m.ID) {
		return Result{}, errors.New("restore swap: router-write suppression belongs to another restore")
	}
	if err := ensureAppliedReceipt(root, m); err != nil {
		return Result{}, err
	}
	if err := hitBoundary(ops, "applied-receipt-published"); err != nil {
		return Result{}, err
	}
	if err := removeRollbackOwned(ctx, root, m); err != nil {
		return Result{}, err
	}
	if err := removePreparedOwned(ctx, root, m, true); err != nil {
		return Result{}, err
	}
	if err := removeMarker(root); err != nil {
		return Result{}, err
	}
	if err := checkNamedDataRoot(root); err != nil {
		return Result{}, err
	}
	return Result{
		Applied: true, RestoreID: m.ID, SafetyBackup: m.safetyName(), SafetyBackupSHA256: m.SafetyBackupFile.SHA256,
		PreparedDatabaseSHA256: m.PreparedDatabaseFile.SHA256,
		PreparedKeyringSHA256:  m.PreparedKeyringFile.SHA256,
		AuthorizingAdminID:     m.AuthorizingAdminID, AuthorizingUsername: m.AuthorizingUsername,
		Counts:    counts,
		PreviewID: m.PreviewID, PlanID: m.PlanID,
	}, nil
}

func countsToDTO(value recovery.Counts) recoveryDTO {
	return recoveryDTO{Schema: value.Schema, Devices: value.Devices, Credentials: value.Credentials,
		OwnedSections: value.OwnedSections, WLANs: value.WLANs, Meshes: value.Meshes}
}

func storeCurrentSchema() int { return store.CurrentSchemaVersion() }
