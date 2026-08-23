package portablebackup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var (
	ErrStageReleased = errors.New("portable backup: stage ownership was released")
	ErrStageCleaned  = errors.New("portable backup: stage was cleaned")
)

// Stage contains fixed, authenticated files for restore preview. Cleanup is
// idempotent and refuses to remove a directory whose identity has changed.
type Stage struct {
	Directory       string
	DatabasePath    string
	PortableKeyPath string
	ManifestPath    string
	Manifest        Manifest

	state *stageState
}

type stageState struct {
	mu           sync.Mutex
	parent       string
	name         string
	identity     os.FileInfo
	parentRoot   *os.Root
	stageRoot    *os.Root
	databaseSize uint64
	portableSize uint64
	manifestSize uint64
	status       stageStatus
}

type stageStatus uint8

const (
	stageActive stageStatus = iota
	stageCleaned
	stageReleased
)

func (s *Stage) Cleanup() error {
	if s == nil || s.state == nil {
		return nil
	}
	state := s.state
	state.mu.Lock()
	defer state.mu.Unlock()
	switch state.status {
	case stageCleaned:
		return nil
	case stageReleased:
		return ErrStageReleased
	}
	if state.parentRoot == nil || state.stageRoot == nil {
		return errors.New("portable backup: staging directory is not anchored")
	}
	// Retained roots still address the original files if either public path is
	// renamed or replaced after extraction.
	cleared, cleanupErr := cleanupStage(state.parentRoot, state.stageRoot,
		state.name, state.identity)
	if !cleared {
		return cleanupErr
	}
	stageCloseErr := state.stageRoot.Close()
	parentCloseErr := state.parentRoot.Close()
	state.stageRoot = nil
	state.parentRoot = nil
	state.status = stageCleaned
	return errors.Join(cleanupErr, stageCloseErr, parentCloseErr)
}

// Release durably transfers ownership of the staged files to a later restore
// coordinator. It leaves the fixed files in place and closes retained anchors.
// A second Release is a no-op; Cleanup after Release returns ErrStageReleased.
func (s *Stage) Release() error {
	if s == nil || s.state == nil {
		return errors.New("portable backup: stage is invalid")
	}
	state := s.state
	state.mu.Lock()
	defer state.mu.Unlock()
	switch state.status {
	case stageReleased:
		return nil
	case stageCleaned:
		return ErrStageCleaned
	}
	if state.parentRoot == nil || state.stageRoot == nil {
		return errors.New("portable backup: staging directory is not anchored")
	}
	if err := checkRootIdentity(state.parentRoot, state.parent); err != nil {
		return err
	}
	current, err := state.parentRoot.Lstat(state.name)
	if err != nil || !current.IsDir() || current.Mode().Perm() != 0o700 ||
		!os.SameFile(current, state.identity) {
		return errors.New("portable backup: staging directory identity changed before release")
	}
	for _, member := range []struct {
		name string
		size uint64
	}{
		{databaseMemberName, state.databaseSize},
		{portableKeyMemberName, state.portableSize},
		{manifestMemberName, state.manifestSize},
	} {
		if err := validateAndSyncStageMember(state.stageRoot, member.name, member.size); err != nil {
			return err
		}
	}
	if err := syncRoot(state.stageRoot, "staging directory release"); err != nil {
		return err
	}
	if err := syncRoot(state.parentRoot, "staging parent release"); err != nil {
		return err
	}
	if err := checkRootIdentity(state.parentRoot, state.parent); err != nil {
		return err
	}
	current, err = state.parentRoot.Lstat(state.name)
	if err != nil || !current.IsDir() || current.Mode().Perm() != 0o700 ||
		!os.SameFile(current, state.identity) {
		return errors.New("portable backup: staging directory identity changed during release")
	}
	stageCloseErr := state.stageRoot.Close()
	parentCloseErr := state.parentRoot.Close()
	state.stageRoot = nil
	state.parentRoot = nil
	state.status = stageReleased
	return errors.Join(stageCloseErr, parentCloseErr)
}

type stageBuilder struct {
	stage  *Stage
	parent *os.Root
	root   *os.Root
	done   bool
}

func createStage(parentPath string, manifest Manifest, manifestSize uint64) (*stageBuilder, error) {
	abs, err := filepath.Abs(parentPath)
	if err != nil {
		return nil, fmt.Errorf("portable backup: resolve staging parent: %w", err)
	}
	parent, err := openPrivateDirectory(abs, "staging parent")
	if err != nil {
		return nil, err
	}
	name, err := createRandomDirectory(parent, ".oonfeewrt-restore-", ".stage")
	if err != nil {
		parent.Close()
		return nil, err
	}
	identity, err := parent.Lstat(name)
	if err != nil {
		parent.Remove(name)
		parent.Close()
		return nil, fmt.Errorf("portable backup: inspect staging directory: %w", err)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		parent.Remove(name)
		parent.Close()
		return nil, fmt.Errorf("portable backup: anchor staging directory: %w", err)
	}
	directoryFile, err := root.Open(".")
	if err != nil {
		root.Close()
		parent.Remove(name)
		parent.Close()
		return nil, fmt.Errorf("portable backup: open staging directory permissions: %w", err)
	}
	chmodErr := directoryFile.Chmod(0o700)
	closeErr := directoryFile.Close()
	if chmodErr != nil || closeErr != nil {
		root.Close()
		parent.Remove(name)
		parent.Close()
		return nil, errors.Join(chmodErr, closeErr)
	}
	if err := syncRoot(parent, "staging parent"); err != nil {
		root.Close()
		parent.Remove(name)
		parent.Close()
		return nil, err
	}
	directory := filepath.Join(abs, name)
	stage := &Stage{
		Directory:       directory,
		DatabasePath:    filepath.Join(directory, databaseMemberName),
		PortableKeyPath: filepath.Join(directory, portableKeyMemberName),
		ManifestPath:    filepath.Join(directory, manifestMemberName),
		Manifest:        manifest,
		state: &stageState{
			parent: abs, name: name, identity: identity,
			databaseSize: manifest.Database.Size, portableSize: manifest.PortableKey.Size,
			manifestSize: manifestSize,
		},
	}
	return &stageBuilder{stage: stage, parent: parent, root: root}, nil
}

func validateAndSyncStageMember(root *os.Root, name string, expectedSize uint64) error {
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm() != 0o600 || before.Size() < 0 || uint64(before.Size()) != expectedSize {
		return fmt.Errorf("portable backup: staged member %s changed before release", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("portable backup: open staged member %s for release: %w", name, err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, opened) {
		file.Close()
		return fmt.Errorf("portable backup: staged member %s identity changed", name)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	after, err := root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return fmt.Errorf("portable backup: staged member %s identity changed during release", name)
	}
	return nil
}

func (b *stageBuilder) finish() (*Stage, error) {
	if err := syncRoot(b.root, "staging directory"); err != nil {
		return nil, err
	}
	if err := checkRootIdentity(b.parent, b.stage.state.parent); err != nil {
		return nil, err
	}
	current, err := b.parent.Lstat(b.stage.state.name)
	if err != nil || !os.SameFile(current, b.stage.state.identity) || !current.IsDir() {
		return nil, errors.New("portable backup: staging directory identity changed")
	}
	b.done = true
	b.stage.state.parentRoot = b.parent
	b.stage.state.stageRoot = b.root
	b.parent = nil
	b.root = nil
	return b.stage, nil
}

func (b *stageBuilder) abort() error {
	if b == nil || b.done {
		return nil
	}
	_, err := cleanupStage(b.parent, b.root, b.stage.state.name, b.stage.state.identity)
	b.root.Close()
	b.parent.Close()
	b.done = true
	return err
}

func cleanupStage(parent, root *os.Root, name string, identity os.FileInfo) (bool, error) {
	anchored, err := root.Stat(".")
	if err != nil || !anchored.IsDir() || !os.SameFile(anchored, identity) {
		return false, errors.New("portable backup: staging directory identity changed")
	}
	for _, member := range []string{databaseMemberName, portableKeyMemberName, manifestMemberName} {
		if err := root.Remove(member); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("portable backup: remove staging member %s: %w", member, err)
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("portable backup: inspect staging cleanup: %w", err)
	}
	_, readErr := directory.Readdirnames(1)
	directory.Close()
	if !errors.Is(readErr, io.EOF) {
		return false, errors.New("portable backup: staging directory contains unexpected entries")
	}
	if err := syncRoot(root, "staging directory cleanup"); err != nil {
		return false, err
	}
	current, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return true, errors.New("portable backup: staging directory moved; decrypted contents were cleared")
	}
	if err != nil {
		return true, fmt.Errorf("portable backup: inspect staging directory entry: %w", err)
	}
	if !current.IsDir() || !os.SameFile(current, identity) {
		return true, errors.New("portable backup: staging directory entry was replaced; original contents were cleared")
	}
	if err := parent.Remove(name); err != nil {
		return true, fmt.Errorf("portable backup: remove staging directory: %w", err)
	}
	return true, syncRoot(parent, "staging parent cleanup")
}

func writeStageFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("portable backup: create staging member %s: %w", name, err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("portable backup: protect staging member %s: %w", name, err)
	}
	if err := writeAll(file, data); err != nil {
		file.Close()
		return fmt.Errorf("portable backup: write staging member %s: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("portable backup: sync staging member %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("portable backup: close staging member %s: %w", name, err)
	}
	return nil
}

func openPrivateDestination(path string) (string, string, string, *os.Root, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", "", nil, errors.New("portable backup: output path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("portable backup: resolve output path: %w", err)
	}
	name := filepath.Base(abs)
	if !safeBaseName(name) {
		return "", "", "", nil, errors.New("portable backup: output filename is invalid")
	}
	parentPath := filepath.Dir(abs)
	parent, err := openPrivateDirectory(parentPath, "output parent")
	if err != nil {
		return "", "", "", nil, err
	}
	if _, err := parent.Lstat(name); err == nil {
		parent.Close()
		return "", "", "", nil, fmt.Errorf("portable backup: output already exists: %w", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		parent.Close()
		return "", "", "", nil, fmt.Errorf("portable backup: inspect output: %w", err)
	}
	return abs, parentPath, name, parent, nil
}

func openPrivateDirectory(path, label string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("portable backup: %s must exist: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("portable backup: %s must be a private real directory", label)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("portable backup: anchor %s: %w", label, err)
	}
	if err := checkRootIdentity(root, path); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func checkRootIdentity(root *os.Root, path string) error {
	anchored, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("portable backup: inspect anchored directory: %w", err)
	}
	named, err := os.Lstat(path)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !named.IsDir() || !os.SameFile(anchored, named) {
		return errors.New("portable backup: directory path identity changed")
	}
	return nil
}

func openBoundedRegular(path, label string, minimum, maximum uint64) (*os.File, uint64, error) {
	if strings.TrimSpace(path) == "" {
		return nil, 0, fmt.Errorf("portable backup: %s path is empty", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, 0, fmt.Errorf("portable backup: resolve %s: %w", label, err)
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return nil, 0, fmt.Errorf("portable backup: inspect %s: %w", label, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("portable backup: %s must be a regular file, not a symlink", label)
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, 0, fmt.Errorf("portable backup: open %s: %w", label, err)
	}
	fail := func(err error) (*os.File, uint64, error) {
		file.Close()
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fail(fmt.Errorf("portable backup: %s identity changed while opening", label))
	}
	after, err := os.Lstat(abs)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return fail(fmt.Errorf("portable backup: %s path identity changed", label))
	}
	size := opened.Size()
	if size < 0 || uint64(size) < minimum || uint64(size) > maximum {
		return fail(fmt.Errorf("portable backup: %s size is out of range", label))
	}
	return file, uint64(size), nil
}

func createRandomFile(root *os.Root, prefix, suffix string) (*os.File, string, error) {
	for range 100 {
		name, err := randomName(prefix, suffix)
		if err != nil {
			return nil, "", err
		}
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("portable backup: create temporary output: %w", err)
		}
		return file, name, nil
	}
	return nil, "", errors.New("portable backup: could not allocate a temporary output")
}

func createRandomDirectory(root *os.Root, prefix, suffix string) (string, error) {
	for range 100 {
		name, err := randomName(prefix, suffix)
		if err != nil {
			return "", err
		}
		if err := root.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("portable backup: create staging directory: %w", err)
		}
		return name, nil
	}
	return "", errors.New("portable backup: could not allocate a staging directory")
}

func randomName(prefix, suffix string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("portable backup: generate temporary name: %w", err)
	}
	return prefix + hex.EncodeToString(token[:]) + suffix, nil
}

func syncRoot(root *os.Root, label string) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("portable backup: open %s for sync: %w", label, err)
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return fmt.Errorf("portable backup: sync %s: %w", label, err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("portable backup: close %s after sync: %w", label, err)
	}
	return nil
}

func safeBaseName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
