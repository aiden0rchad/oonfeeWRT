// Package controllerrestore authenticates and inspects portable controller
// backups without changing live controller or router state.
package controllerrestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	_ "modernc.org/sqlite"
)

const (
	minimumSourceSchema = 13
	maxPathBytes        = 4096
	copyBufferBytes     = 128 << 10
	portableKeyMaxBytes = 4096
)

// Preview contains authenticated, non-secret restore information.
type Preview struct {
	Manifest     portablebackup.Manifest
	SourceSchema int
	TargetSchema int
	Counts       recovery.Counts
}

type inspectOperations struct {
	afterOpen func()
}

// Inspect authenticates artifact, migrates only a disposable copy, and
// validates the resulting controller state. The caller must hold one
// password-hashing slot for the whole call; Argon2 work is sequential. The
// caller retains ownership of exportPassphrase and must clear it after use.
func Inspect(ctx context.Context, artifactPath, scratchParent string,
	exportPassphrase []byte) (Preview, error) {
	return inspect(ctx, artifactPath, scratchParent, exportPassphrase, inspectOperations{})
}

func inspect(ctx context.Context, artifactPath, scratchParent string,
	exportPassphrase []byte, ops inspectOperations) (preview Preview, retErr error) {
	if ctx == nil {
		return preview, errors.New("restore preview context is nil")
	}
	if err := ctx.Err(); err != nil {
		return preview, err
	}
	if err := validatePath(artifactPath, "artifact"); err != nil {
		return preview, err
	}
	if err := validatePath(scratchParent, "scratch parent"); err != nil {
		return preview, err
	}

	stage, err := portablebackup.Extract(ctx, artifactPath, scratchParent, exportPassphrase)
	if err != nil {
		return preview, extractionError(err)
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("extracted stage", stage.Cleanup())) }()
	if err := ctx.Err(); err != nil {
		return preview, err
	}

	scratch, err := newScratch(scratchParent)
	if err != nil {
		return preview, err
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("scratch data", scratch.cleanup())) }()

	stageRoot, err := openStage(stage)
	if err != nil {
		return preview, err
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("extracted stage handle", stageRoot.Close())) }()
	if err := copyDatabase(ctx, stageRoot, scratch.root, stage.Manifest.Database); err != nil {
		return preview, err
	}
	portableKey, err := readMember(stageRoot, stage.Manifest.PortableKey, portableKeyMaxBytes)
	if err != nil {
		return preview, err
	}
	defer clear(portableKey)

	keeper, err := secrets.OpenPortableKey(portableKey, exportPassphrase)
	if err != nil {
		if errors.Is(err, secrets.ErrBadPassphrase) {
			return preview, fmt.Errorf("restore preview: export passphrase is incorrect: %w", secrets.ErrBadPassphrase)
		}
		return preview, errors.New("restore preview: portable key is invalid")
	}
	defer func() { retErr = errors.Join(retErr, cleanupError("portable key", keeper.Close())) }()

	if err := scratch.checkDatabaseIdentity(); err != nil {
		return preview, err
	}
	sourceSchema, err := store.ProbeSchemaVersion(ctx, "sqlite", scratch.databasePath())
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return preview, contextErr
		}
		return preview, errors.New("restore preview: source database schema could not be read")
	}
	if sourceSchema != stage.Manifest.SchemaVersion {
		return preview, fmt.Errorf("restore preview: manifest schema v%d does not match database schema v%d",
			stage.Manifest.SchemaVersion, sourceSchema)
	}
	targetSchema := store.CurrentSchemaVersion()
	if sourceSchema < minimumSourceSchema {
		return preview, fmt.Errorf("restore preview: source schema v%d is unsupported; minimum is v%d",
			sourceSchema, minimumSourceSchema)
	}
	if sourceSchema > targetSchema {
		return preview, fmt.Errorf("restore preview: source schema v%d is newer than this controller's v%d",
			sourceSchema, targetSchema)
	}
	if err := ctx.Err(); err != nil {
		return preview, err
	}
	if err := scratch.checkDatabaseIdentity(); err != nil {
		return preview, err
	}

	db, err := store.Open(ctx, "sqlite", scratch.databasePath(), keeper)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return preview, contextErr
		}
		return preview, errors.New("restore preview: disposable database migration failed")
	}
	dbOpen := true
	defer func() {
		if dbOpen {
			retErr = errors.Join(retErr, cleanupError("disposable database handle", db.Close()))
		}
	}()
	if ops.afterOpen != nil {
		ops.afterOpen()
	}
	counts, err := recovery.Validate(ctx, db, keeper)
	if err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return preview, contextErr
		}
		return preview, errors.New("restore preview: migrated database validation failed")
	}
	if counts.Schema != targetSchema {
		return preview, errors.New("restore preview: migration did not reach the current schema")
	}
	if err := db.Checkpoint(ctx); err != nil {
		if contextErr := canceledError(err); contextErr != nil {
			return preview, contextErr
		}
		return preview, errors.New("restore preview: disposable database checkpoint failed")
	}
	if err := db.Close(); err != nil {
		return preview, errors.New("restore preview: disposable database close failed")
	}
	dbOpen = false
	if err := ctx.Err(); err != nil {
		return preview, err
	}

	return Preview{
		Manifest: stage.Manifest, SourceSchema: sourceSchema,
		TargetSchema: targetSchema, Counts: counts,
	}, nil
}

func validatePath(path, label string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("restore preview: %s path is empty", label)
	}
	if len(path) > maxPathBytes || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("restore preview: %s path is invalid", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil || len(abs) > maxPathBytes || !utf8.ValidString(abs) {
		return fmt.Errorf("restore preview: %s path is invalid", label)
	}
	return nil
}

func extractionError(err error) error {
	if contextErr := canceledError(err); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, secrets.ErrBadPassphrase) {
		return fmt.Errorf("restore preview: export passphrase is incorrect: %w", secrets.ErrBadPassphrase)
	}
	if errors.Is(err, portablebackup.ErrAuthentication) {
		return fmt.Errorf("restore preview: artifact authentication failed: %w", portablebackup.ErrAuthentication)
	}
	return errors.New("restore preview: artifact could not be authenticated and extracted")
}

func canceledError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func cleanupError(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("restore preview: %s cleanup failed", label)
}

var ErrCleanupPathChanged = errors.New("restore preparation: sensitive contents were cleared but the directory path changed")

type directoryOperations struct {
	removeAll func(*os.Root, string) error
	remove    func(*os.Root, string) error
	sync      func(*os.Root) error
}

func (ops directoryOperations) defaults() directoryOperations {
	if ops.removeAll == nil {
		ops.removeAll = func(root *os.Root, name string) error { return root.RemoveAll(name) }
	}
	if ops.remove == nil {
		ops.remove = func(root *os.Root, name string) error { return root.Remove(name) }
	}
	if ops.sync == nil {
		ops.sync = syncPreparedRoot
	}
	return ops
}

type scratchDirectory struct {
	parentPath string
	name       string
	path       string
	parent     *os.Root
	root       *os.Root
	identity   os.FileInfo
	cleaned    bool
	unlinked   bool
	terminal   error
	ops        directoryOperations
}

func newScratch(parentPath string) (_ *scratchDirectory, retErr error) {
	return newPrivateDirectory(parentPath, ".oonfeewrt-preview-")
}

func newPrivateDirectory(parentPath, prefix string) (_ *scratchDirectory, retErr error) {
	return newPrivateDirectoryShape(parentPath, prefix, ".tmp")
}

func newPrivateDirectoryShape(parentPath, prefix, suffix string) (_ *scratchDirectory, retErr error) {
	abs, err := filepath.Abs(parentPath)
	if err != nil {
		return nil, errors.New("restore preview: scratch parent path could not be resolved")
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("restore preview: scratch parent must be a private real directory")
	}
	parent, err := os.OpenRoot(abs)
	if err != nil {
		return nil, errors.New("restore preview: scratch parent could not be anchored")
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanupError("scratch parent handle", parent.Close()))
		}
	}()
	anchored, err := parent.Stat(".")
	if err != nil || !os.SameFile(info, anchored) {
		return nil, errors.New("restore preview: scratch parent identity changed")
	}

	var name string
	for range 16 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, errors.New("restore preview: scratch name could not be generated")
		}
		name = prefix + hex.EncodeToString(random[:]) + suffix
		if err := parent.Mkdir(name, 0o700); err == nil {
			break
		} else if !errors.Is(err, os.ErrExist) {
			return nil, errors.New("restore preview: scratch directory could not be created")
		}
		name = ""
	}
	if name == "" {
		return nil, errors.New("restore preview: could not allocate scratch directory")
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			retErr = errors.Join(retErr, cleanupError("scratch allocation", parent.RemoveAll(name)))
		}
	}()
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, errors.New("restore preview: scratch directory could not be anchored")
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanupError("scratch directory handle", root.Close()))
		}
	}()
	directory, err := root.Open(".")
	if err != nil {
		return nil, errors.New("restore preview: scratch directory could not be opened")
	}
	chmodErr := directory.Chmod(0o700)
	closeErr := directory.Close()
	if chmodErr != nil || closeErr != nil {
		return nil, errors.New("restore preview: scratch directory could not be protected")
	}
	identity, err := parent.Lstat(name)
	if err != nil || !identity.IsDir() || identity.Mode().Perm() != 0o700 {
		return nil, errors.New("restore preview: scratch directory is not private")
	}
	removeOnError = false
	return &scratchDirectory{
		parentPath: abs, name: name, path: filepath.Join(abs, name),
		parent: parent, root: root, identity: identity, ops: (directoryOperations{}).defaults(),
	}, nil
}

func (s *scratchDirectory) databasePath() string { return filepath.Join(s.path, "controller.db") }

func (s *scratchDirectory) checkIdentity() error {
	anchoredParent, err := s.parent.Stat(".")
	if err != nil {
		return errors.New("restore preview: scratch parent identity changed")
	}
	namedParent, err := os.Lstat(s.parentPath)
	if err != nil || namedParent.Mode()&os.ModeSymlink != 0 || !os.SameFile(anchoredParent, namedParent) {
		return errors.New("restore preview: scratch parent identity changed")
	}
	anchored, err := s.root.Stat(".")
	if err != nil || !os.SameFile(anchored, s.identity) {
		return errors.New("restore preview: scratch directory identity changed")
	}
	named, err := s.parent.Lstat(s.name)
	if err != nil || !named.IsDir() || !os.SameFile(named, s.identity) {
		return errors.New("restore preview: scratch directory identity changed")
	}
	return nil
}

func (s *scratchDirectory) checkDatabaseIdentity() error {
	if err := s.checkIdentity(); err != nil {
		return err
	}
	anchored, err := s.root.Lstat("controller.db")
	if err != nil || anchored.Mode()&os.ModeSymlink != 0 || !anchored.Mode().IsRegular() ||
		anchored.Mode().Perm() != 0o600 {
		return errors.New("restore preview: disposable database identity changed")
	}
	named, err := os.Lstat(s.databasePath())
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(anchored, named) {
		return errors.New("restore preview: disposable database identity changed")
	}
	return nil
}

func (s *scratchDirectory) cleanup() error {
	if s == nil {
		return nil
	}
	if s.cleaned {
		return s.terminal
	}
	if s.root == nil || s.parent == nil {
		return errors.New("restore preview: scratch cleanup anchors are unavailable")
	}
	s.ops = s.ops.defaults()
	if s.unlinked {
		if err := s.ops.sync(s.parent); err != nil {
			return err
		}
		return s.finishCleanup(nil)
	}
	directory, err := s.root.Open(".")
	if err != nil {
		return err
	}
	names, readErr := directory.Readdirnames(-1)
	closeDirErr := directory.Close()
	if readErr != nil || closeDirErr != nil {
		return errors.Join(readErr, closeDirErr)
	}
	for _, name := range names {
		if err := s.ops.removeAll(s.root, name); err != nil {
			return err
		}
	}
	directory, err = s.root.Open(".")
	if err != nil {
		return err
	}
	_, remainingErr := directory.Readdirnames(1)
	closeDirErr = directory.Close()
	if !errors.Is(remainingErr, io.EOF) || closeDirErr != nil {
		return errors.Join(errors.New("restore preview: scratch directory still contains data"), closeDirErr)
	}
	if err := s.ops.sync(s.root); err != nil {
		return err
	}
	named, identityErr := s.parent.Lstat(s.name)
	if identityErr != nil || !named.IsDir() || !os.SameFile(named, s.identity) {
		return s.finishCleanup(ErrCleanupPathChanged)
	}
	if err := s.ops.remove(s.parent, s.name); err != nil {
		return err
	}
	s.unlinked = true
	if err := s.ops.sync(s.parent); err != nil {
		return err
	}
	return s.finishCleanup(nil)
}

func (s *scratchDirectory) finishCleanup(terminal error) error {
	rootErr := s.root.Close()
	parentErr := s.parent.Close()
	s.root = nil
	s.parent = nil
	s.cleaned = true
	s.terminal = terminal
	return errors.Join(terminal, rootErr, parentErr)
}

func openStage(stage *portablebackup.Stage) (*os.Root, error) {
	info, err := os.Lstat(stage.Directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("restore preview: extracted stage is not a private real directory")
	}
	root, err := os.OpenRoot(stage.Directory)
	if err != nil {
		return nil, errors.New("restore preview: extracted stage could not be anchored")
	}
	anchored, err := root.Stat(".")
	if err != nil || !os.SameFile(info, anchored) {
		root.Close()
		return nil, errors.New("restore preview: extracted stage identity changed")
	}
	return root, nil
}

func copyDatabase(ctx context.Context, source, destination *os.Root,
	member portablebackup.Member) (retErr error) {
	input, err := openMember(source, member, portablebackup.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, input.Close()) }()
	output, err := destination.OpenFile("controller.db", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("restore preview: create disposable database: %w", err)
	}
	outputOpen := true
	defer func() {
		if outputOpen {
			retErr = errors.Join(retErr, output.Close())
		}
	}()
	hasher := sha256.New()
	limited := io.LimitReader(input, int64(member.Size)+1)
	buffer := make([]byte, copyBufferBytes)
	defer clear(buffer)
	written, err := io.CopyBuffer(io.MultiWriter(output, hasher), &contextReader{ctx: ctx, reader: limited},
		buffer)
	if err != nil {
		return fmt.Errorf("restore preview: copy disposable database: %w", err)
	}
	if written != int64(member.Size) {
		return errors.New("restore preview: extracted database size changed")
	}
	want, err := decodeDigest(member.SHA256)
	if err != nil || subtle.ConstantTimeCompare(hasher.Sum(nil), want) != 1 {
		return errors.New("restore preview: extracted database authentication changed")
	}
	if err := output.Chmod(0o600); err != nil {
		return fmt.Errorf("restore preview: protect disposable database: %w", err)
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("restore preview: sync disposable database: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("restore preview: close disposable database: %w", err)
	}
	outputOpen = false
	return ctx.Err()
}

func readMember(root *os.Root, member portablebackup.Member, maximum uint64) ([]byte, error) {
	file, err := openMember(root, member, maximum)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data := make([]byte, int(member.Size))
	if _, err := io.ReadFull(file, data); err != nil {
		clear(data)
		return nil, errors.New("restore preview: extracted member changed while reading")
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		clear(data)
		return nil, errors.New("restore preview: extracted member length changed")
	}
	sum := sha256.Sum256(data)
	want, err := decodeDigest(member.SHA256)
	if err != nil || subtle.ConstantTimeCompare(sum[:], want) != 1 {
		clear(data)
		return nil, errors.New("restore preview: extracted member authentication changed")
	}
	return data, nil
}

func openMember(root *os.Root, member portablebackup.Member, maximum uint64) (*os.File, error) {
	if member.Size == 0 || member.Size > maximum {
		return nil, errors.New("restore preview: extracted member size is out of range")
	}
	before, err := root.Lstat(member.Name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm() != 0o600 || before.Size() < 0 || uint64(before.Size()) != member.Size {
		return nil, errors.New("restore preview: extracted member is not the authenticated regular file")
	}
	file, err := root.Open(member.Name)
	if err != nil {
		return nil, fmt.Errorf("restore preview: open extracted member: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		file.Close()
		return nil, errors.New("restore preview: extracted member identity changed")
	}
	return file, nil
}

func decodeDigest(encoded string) ([]byte, error) {
	digest, err := hex.DecodeString(encoded)
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("restore preview: authenticated digest is malformed")
	}
	return digest, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
