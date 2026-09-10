// Package toolstore opens the controller database for local maintenance tools
// without weakening the daemon's keyring boundary.
package toolstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiden0rchad/oonfeewrt/internal/processlock"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	_ "modernc.org/sqlite"
)

const passphraseFileEnv = "OONFEE_PASSPHRASE_FILE"

// Handle keeps the database and its in-memory data key alive together.
type Handle struct {
	DB       *store.DB
	keeper   *secrets.Keeper
	writable bool
	lock     *processlock.Lock
}

// VerifyCredential proves that a stored device credential opens under this
// handle's exact sibling keyring without materialising the username or
// password. Recovery tooling uses it to inspect every ciphertext while keeping
// the unwrapped data key private to this package.
func (h *Handle) VerifyCredential(mac string, blob []byte) error {
	if h == nil || h.keeper == nil {
		return errors.New("toolstore: credential verifier is unavailable")
	}
	return h.keeper.VerifyCredential(mac, blob)
}

// OpenReadOnly opens a current, fully scrubbed controller database.
func OpenReadOnly(ctx context.Context, dbPath string) (*Handle, error) {
	return open(ctx, dbPath, true)
}

// OpenWritable opens and, when necessary, migrates the controller database.
func OpenWritable(ctx context.Context, dbPath string) (*Handle, error) {
	return open(ctx, dbPath, false)
}

func open(ctx context.Context, dbPath string, readOnly bool) (*Handle, error) {
	resolvedDBPath, err := filepath.EvalSymlinks(dbPath)
	if err != nil {
		return nil, fmt.Errorf("toolstore: resolve controller database path: %w", err)
	}
	resolvedDBPath, err = filepath.Abs(resolvedDBPath)
	if err != nil {
		return nil, fmt.Errorf("toolstore: resolve absolute controller database path: %w", err)
	}
	dbPath = resolvedDBPath
	var lock *processlock.Lock
	if !readOnly {
		lock, err = processlock.Acquire(filepath.Dir(dbPath))
		if err != nil {
			return nil, fmt.Errorf("toolstore: exclusive controller access: %w", err)
		}
		defer func() {
			if lock != nil {
				_ = lock.Close()
			}
		}()
	}
	passPath := os.Getenv(passphraseFileEnv)
	if passPath == "" {
		return nil, fmt.Errorf("toolstore: %s must name the controller passphrase file", passphraseFileEnv)
	}
	passphrase, err := secrets.ReadPassphraseFile(passPath)
	if err != nil {
		return nil, err
	}
	defer clear(passphrase)
	keeper, err := secrets.Open(filepath.Join(filepath.Dir(dbPath), secrets.FileName), passphrase)
	if err != nil {
		return nil, err
	}
	var db *store.DB
	if readOnly {
		db, err = store.OpenReadOnly(ctx, "sqlite", dbPath, keeper)
	} else {
		db, err = store.Open(ctx, "sqlite", dbPath, keeper)
	}
	if err != nil {
		keeper.Close()
		return nil, err
	}
	handle := &Handle{DB: db, keeper: keeper, writable: !readOnly, lock: lock}
	lock = nil
	return handle, nil
}

// Close releases the database before zeroing its data key.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	var errs []error
	dbClosed, keeperClosed := h.DB == nil, h.keeper == nil
	if h.DB != nil {
		if h.writable {
			errs = append(errs, h.DB.Checkpoint(context.Background()))
		}
		if err := h.DB.Close(); err != nil {
			errs = append(errs, err)
		} else {
			dbClosed = true
		}
	}
	if h.keeper != nil {
		if err := h.keeper.Close(); err != nil {
			errs = append(errs, err)
		} else {
			keeperClosed = true
		}
	}
	if h.lock != nil && dbClosed && keeperClosed {
		errs = append(errs, h.lock.Close())
	}
	return errors.Join(errs...)
}
