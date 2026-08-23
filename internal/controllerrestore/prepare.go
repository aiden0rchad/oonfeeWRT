package controllerrestore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const preparedDatabaseName = "controller.db"

var (
	ErrPreparedCleaned     = errors.New("restore preview: prepared pair was cleaned")
	ErrPreparedTransferred = errors.New("restore preview: prepared pair ownership was transferred")
)

// PreparedPair is disclosed only to the durable-intent callback passed to
// Prepared.Transfer. It contains no credentials or passphrases.
type PreparedPair struct {
	Directory      string `json:"-"`
	DatabasePath   string `json:"-"`
	KeyringPath    string `json:"-"`
	DatabaseSize   int64
	KeyringSize    int64
	DatabaseSHA256 string
	KeyringSHA256  string
	Preview        Preview
}

type preparedState uint8

const (
	preparedActive preparedState = iota
	preparedCleaned
	preparedTransferred
)

// Prepared owns one anchored private DB/keyring pair until Cleanup removes it
// or Transfer hands it to a durable restore intent.
type Prepared struct {
	mu      sync.Mutex
	state   preparedState
	dir     *scratchDirectory
	pair    PreparedPair
	preview Preview
}

type prepareOperations struct {
	afterRuntimeVerify func()
	afterExtract       func()
	afterMigration     func()
	afterPairWritten   func(*scratchDirectory)
	afterPairValidated func()
}

type intentCallbackError struct{ cause error }

func (e intentCallbackError) Error() string {
	return "restore preparation: durable intent was not created"
}
func (e intentCallbackError) Unwrap() error { return e.cause }

type preparedCleanupError struct{ cause error }

func (e preparedCleanupError) Error() string {
	return "restore preparation: prepared pair cleanup failed"
}
func (e preparedCleanupError) Unwrap() error { return e.cause }

// Prepare authenticates, migrates and validates an artifact into an anchored
// immediate child of dataDir. The caller must hold one password-hashing slot
// for the whole call; all Argon2 work is sequential. It retains neither
// caller-owned passphrase, and callers must clear both buffers after use.
// destinationRuntimePassphrase is verified against live before any staging.
func Prepare(ctx context.Context, artifactPath, dataDir string, live *secrets.Keeper,
	exportPassphrase, destinationRuntimePassphrase []byte) (*Prepared, error) {
	return prepare(ctx, artifactPath, dataDir, live, exportPassphrase,
		destinationRuntimePassphrase, prepareOperations{})
}

func prepare(ctx context.Context, artifactPath, dataDir string, live *secrets.Keeper,
	exportPassphrase, destinationRuntimePassphrase []byte,
	ops prepareOperations) (prepared *Prepared, retErr error) {
	defer func() {
		if retErr != nil && prepared != nil {
			retErr = errors.Join(retErr,
				cleanupError("prepared restore pair", prepared.dir.cleanup()))
			prepared = nil
		}
	}()
	if ctx == nil {
		return nil, errors.New("restore preparation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if live == nil {
		return nil, errors.New("restore preparation requires the live keyring")
	}
	if err := validatePath(artifactPath, "artifact"); err != nil {
		return nil, err
	}
	if err := validatePath(dataDir, "data directory"); err != nil {
		return nil, err
	}
	if err := validatePrivateDataDirectory(dataDir); err != nil {
		return nil, err
	}
	if err := live.VerifyPassphrase(destinationRuntimePassphrase); err != nil {
		if errors.Is(err, secrets.ErrBadPassphrase) {
			return nil, fmt.Errorf("restore preparation: runtime passphrase is incorrect: %w", secrets.ErrBadPassphrase)
		}
		return nil, errors.New("restore preparation: live runtime keyring could not be verified")
	}
	if ops.afterRuntimeVerify != nil {
		ops.afterRuntimeVerify()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stage, err := portablebackup.Extract(ctx, artifactPath, dataDir, exportPassphrase)
	if err != nil {
		return nil, extractionError(err)
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("extracted stage", stage.Cleanup())) }()
	if ops.afterExtract != nil {
		ops.afterExtract()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	working, err := newPrivateDirectory(dataDir, ".oonfeewrt-prepare-work-")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("preparation scratch data", working.cleanup())) }()
	stageRoot, err := openStage(stage)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("extracted stage handle", stageRoot.Close())) }()
	if err := copyDatabase(ctx, stageRoot, working.root, stage.Manifest.Database); err != nil {
		return nil, err
	}
	portableKey, err := readMember(stageRoot, stage.Manifest.PortableKey, portableKeyMaxBytes)
	if err != nil {
		return nil, err
	}
	defer clear(portableKey)
	keeper, err := secrets.OpenPortableKey(portableKey, exportPassphrase)
	if err != nil {
		if errors.Is(err, secrets.ErrBadPassphrase) {
			return nil, fmt.Errorf("restore preparation: export passphrase is incorrect: %w", secrets.ErrBadPassphrase)
		}
		return nil, errors.New("restore preparation: portable key is invalid")
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("portable key", keeper.Close())) }()

	if err := working.checkDatabaseIdentity(); err != nil {
		return nil, err
	}
	sourceSchema, err := store.ProbeSchemaVersion(ctx, "sqlite", working.databasePath())
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: source database schema could not be read")
	}
	if sourceSchema != stage.Manifest.SchemaVersion {
		return nil, fmt.Errorf("restore preparation: manifest schema v%d does not match database schema v%d",
			stage.Manifest.SchemaVersion, sourceSchema)
	}
	targetSchema := store.CurrentSchemaVersion()
	if sourceSchema < minimumSourceSchema {
		return nil, fmt.Errorf("restore preparation: source schema v%d is unsupported; minimum is v%d",
			sourceSchema, minimumSourceSchema)
	}
	if sourceSchema > targetSchema {
		return nil, fmt.Errorf("restore preparation: source schema v%d is newer than this controller's v%d",
			sourceSchema, targetSchema)
	}
	if err := working.checkDatabaseIdentity(); err != nil {
		return nil, err
	}

	db, err := store.Open(ctx, "sqlite", working.databasePath(), keeper)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: disposable database migration failed")
	}
	dbOpen := true
	defer func() {
		if dbOpen {
			retErr = errors.Join(retErr, cleanupError("disposable database handle", db.Close()))
		}
	}()
	counts, err := recovery.Validate(ctx, db, keeper)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: migrated database validation failed")
	}
	if counts.Schema != targetSchema {
		return nil, errors.New("restore preparation: migration did not reach the current schema")
	}
	if ops.afterMigration != nil {
		ops.afterMigration()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := db.Checkpoint(ctx); err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: disposable database checkpoint failed")
	}

	final, err := newPrivateDirectoryShape(dataDir, ".oonfeewrt-prepared-pair-", ".stage")
	if err != nil {
		return nil, err
	}
	finalOwned := true
	defer func() {
		if finalOwned {
			retErr = errors.Join(retErr, cleanupError("prepared restore pair", final.cleanup()))
		}
	}()
	if err := db.BackupTo(ctx, final.databasePath()); err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: clean database snapshot failed")
	}
	if err := db.Close(); err != nil {
		return nil, errors.New("restore preparation: disposable database close failed")
	}
	dbOpen = false
	if err := keeper.WriteNewKeyring(final.keyringPath(), destinationRuntimePassphrase); err != nil {
		return nil, errors.New("restore preparation: destination keyring could not be written")
	}
	if ops.afterPairWritten != nil {
		ops.afterPairWritten(final)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	preview := Preview{
		Manifest: stage.Manifest, SourceSchema: sourceSchema,
		TargetSchema: targetSchema, Counts: counts,
	}
	pair, err := inspectPreparedPair(ctx, final, preview)
	if err != nil {
		return nil, err
	}
	generatedKeeper, err := secrets.Open(final.keyringPath(), destinationRuntimePassphrase)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: destination keyring verification failed")
	}
	generatedOpen := true
	defer func() {
		if generatedOpen {
			retErr = errors.Join(retErr, cleanupError("destination keyring handle", generatedKeeper.Close()))
		}
	}()
	verifiedDB, err := store.OpenReadOnly(ctx, "sqlite", final.databasePath(), generatedKeeper)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: destination database/keyring pair could not be reopened")
	}
	verifiedOpen := true
	defer func() {
		if verifiedOpen {
			retErr = errors.Join(retErr, cleanupError("verified destination database handle", verifiedDB.Close()))
		}
	}()
	verifiedCounts, err := recovery.Validate(ctx, verifiedDB, generatedKeeper)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("restore preparation: destination database validation failed")
	}
	if verifiedCounts != counts {
		return nil, errors.New("restore preparation: destination database inventory changed during preparation")
	}
	if err := verifiedDB.Close(); err != nil {
		return nil, errors.New("restore preparation: verified destination database close failed")
	}
	verifiedOpen = false
	if err := generatedKeeper.Close(); err != nil {
		return nil, errors.New("restore preparation: destination keyring close failed")
	}
	generatedOpen = false
	pairAfter, err := inspectPreparedPair(ctx, final, preview)
	if err != nil {
		return nil, err
	}
	if !samePreparedPair(pair, pairAfter) {
		return nil, errors.New("restore preparation: prepared pair changed during final validation")
	}
	if ops.afterPairValidated != nil {
		ops.afterPairValidated()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prepared = &Prepared{dir: final, pair: pairAfter, preview: preview}
	finalOwned = false
	return prepared, nil
}

func (s *scratchDirectory) keyringPath() string { return filepath.Join(s.path, secrets.FileName) }

// Preview returns the non-secret confirmation binding for this prepared pair.
func (p *Prepared) Preview() Preview {
	if p == nil {
		return Preview{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preview
}

// Cleanup destroys an untransferred pair. It is idempotent and serializes with
// Transfer. Cleanup after a successful transfer returns ErrPreparedTransferred.
func (p *Prepared) Cleanup() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case preparedCleaned:
		return nil
	case preparedTransferred:
		return ErrPreparedTransferred
	}
	err := p.dir.cleanup()
	if p.dir.cleaned {
		p.state = preparedCleaned
	}
	if err != nil {
		return preparedCleanupError{cause: err}
	}
	return nil
}

// Transfer invokes adopt while this object still owns anchored files. ctx
// bounds verification before adopt begins. adopt
// must return nil only after its restore intent is durable, return an error
// only when no durable intent remains, and must not call methods on this
// Prepared object. A callback error leaves ownership here; nil transfers
// ownership even if anchor closing warns.
func (p *Prepared) Transfer(ctx context.Context,
	adopt func(PreparedPair) error) (transferred bool, retErr error) {
	if p == nil || adopt == nil {
		return false, errors.New("restore preparation: durable-intent callback is required")
	}
	if ctx == nil {
		return false, errors.New("restore preparation: transfer context is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case preparedCleaned:
		return false, ErrPreparedCleaned
	case preparedTransferred:
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := inspectPreparedPair(ctx, p.dir, p.preview)
	if err != nil {
		return false, err
	}
	if !samePreparedPair(p.pair, current) {
		return false, errors.New("restore preparation: prepared pair changed before transfer")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := adopt(current); err != nil {
		return false, intentCallbackError{cause: err}
	}
	p.state = preparedTransferred
	return true, cleanupError("prepared restore ownership transfer", p.dir.release())
}

func validatePrivateDataDirectory(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return errors.New("restore preparation: data directory path could not be resolved")
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("restore preparation: data directory must be a private real directory")
	}
	return nil
}

func inspectPreparedPair(ctx context.Context, directory *scratchDirectory,
	preview Preview) (PreparedPair, error) {
	if ctx == nil {
		return PreparedPair{}, errors.New("restore preparation: prepared-pair context is nil")
	}
	if err := ctx.Err(); err != nil {
		return PreparedPair{}, err
	}
	if directory == nil || directory.root == nil || directory.parent == nil {
		return PreparedPair{}, errors.New("restore preparation: prepared pair is unavailable")
	}
	if err := directory.checkIdentity(); err != nil {
		return PreparedPair{}, errors.New("restore preparation: prepared directory identity changed")
	}
	entries, err := directory.root.Open(".")
	if err != nil {
		return PreparedPair{}, errors.New("restore preparation: prepared directory could not be inspected")
	}
	names, readErr := entries.Readdirnames(-1)
	closeErr := entries.Close()
	if readErr != nil || closeErr != nil {
		return PreparedPair{}, errors.New("restore preparation: prepared directory could not be inspected")
	}
	sort.Strings(names)
	wantNames := []string{preparedDatabaseName, secrets.FileName}
	sort.Strings(wantNames)
	if len(names) != len(wantNames) || names[0] != wantNames[0] || names[1] != wantNames[1] {
		return PreparedPair{}, errors.New("restore preparation: prepared directory must contain exactly the database and keyring")
	}
	databaseSize, databaseHash, err := hashPreparedMember(ctx, directory,
		preparedDatabaseName, portablebackup.MaxDatabaseBytes)
	if err != nil {
		return PreparedPair{}, err
	}
	keyringSize, keyringHash, err := hashPreparedMember(ctx, directory,
		secrets.FileName, portableKeyMaxBytes)
	if err != nil {
		return PreparedPair{}, err
	}
	if err := syncPreparedRoot(directory.root); err != nil {
		return PreparedPair{}, err
	}
	if err := syncPreparedRoot(directory.parent); err != nil {
		return PreparedPair{}, err
	}
	if err := directory.checkIdentity(); err != nil {
		return PreparedPair{}, errors.New("restore preparation: prepared directory identity changed")
	}
	return PreparedPair{
		Directory: directory.path, DatabasePath: directory.databasePath(),
		KeyringPath: directory.keyringPath(), DatabaseSize: databaseSize,
		KeyringSize: keyringSize, DatabaseSHA256: databaseHash,
		KeyringSHA256: keyringHash, Preview: preview,
	}, nil
}

func hashPreparedMember(ctx context.Context, directory *scratchDirectory,
	name string, maximum uint64) (size int64, digest string, retErr error) {
	before, err := directory.root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm() != 0o600 || before.Size() <= 0 || uint64(before.Size()) > maximum {
		return 0, "", errors.New("restore preparation: prepared member is invalid")
	}
	public, err := os.Lstat(filepath.Join(directory.path, name))
	if err != nil || public.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, public) {
		return 0, "", errors.New("restore preparation: prepared member identity changed")
	}
	file, err := directory.root.Open(name)
	if err != nil {
		return 0, "", errors.New("restore preparation: prepared member could not be opened")
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("prepared member handle", file.Close())) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return 0, "", errors.New("restore preparation: prepared member identity changed")
	}
	hasher := sha256.New()
	buffer := make([]byte, copyBufferBytes)
	defer clear(buffer)
	limited := io.LimitReader(file, before.Size()+1)
	written, err := io.CopyBuffer(hasher, &contextReader{ctx: ctx, reader: limited}, buffer)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return 0, "", contextErr
		}
		return 0, "", errors.New("restore preparation: prepared member could not be hashed")
	}
	if written != before.Size() {
		return 0, "", errors.New("restore preparation: prepared member size changed")
	}
	if err := file.Sync(); err != nil {
		return 0, "", errors.New("restore preparation: prepared member could not be synced")
	}
	after, err := directory.root.Lstat(name)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() ||
		after.Mode().Perm() != 0o600 {
		return 0, "", errors.New("restore preparation: prepared member changed during verification")
	}
	return before.Size(), hex.EncodeToString(hasher.Sum(nil)), nil
}

func syncPreparedRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("restore preparation: prepared directory could not be synced")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("restore preparation: prepared directory could not be synced")
	}
	return nil
}

func samePreparedPair(left, right PreparedPair) bool {
	return left.Directory == right.Directory && left.DatabasePath == right.DatabasePath &&
		left.KeyringPath == right.KeyringPath && left.DatabaseSize == right.DatabaseSize &&
		left.KeyringSize == right.KeyringSize &&
		subtle.ConstantTimeCompare([]byte(left.DatabaseSHA256), []byte(right.DatabaseSHA256)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.KeyringSHA256), []byte(right.KeyringSHA256)) == 1 &&
		left.Preview == right.Preview
}

func (s *scratchDirectory) release() error {
	if s == nil || s.root == nil || s.parent == nil {
		return errors.New("restore preparation: prepared directory is unavailable")
	}
	identityErr := s.checkIdentity()
	rootErr := s.root.Close()
	parentErr := s.parent.Close()
	s.root = nil
	s.parent = nil
	s.cleaned = true
	return errors.Join(identityErr, rootErr, parentErr)
}
