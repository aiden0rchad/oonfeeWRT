package controllerrestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

var preparedOrphanShapes = [...]struct {
	prefix string
	suffix string
}{
	{prefix: ".oonfeewrt-restore-", suffix: ".stage"},
	{prefix: ".oonfeewrt-prepare-work-", suffix: ".tmp"},
	{prefix: ".oonfeewrt-prepared-pair-", suffix: ".stage"},
}

// CleanupOrphanPrepared removes exact Prepare-owned immediate children of
// dataDir. Call it at startup only after every durable restore intent has been
// applied or aborted; otherwise it could remove the pair referenced by one.
func CleanupOrphanPrepared(ctx context.Context, dataDir string) (retErr error) {
	if ctx == nil {
		return errors.New("restore preparation: orphan-cleanup context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return errors.New("restore preparation: data directory path could not be resolved")
	}
	identity, err := os.Lstat(abs)
	if err != nil || identity.Mode()&os.ModeSymlink != 0 || !identity.IsDir() ||
		identity.Mode().Perm()&0o022 != 0 {
		return errors.New("restore preparation: data directory must be a private real directory")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return errors.New("restore preparation: data directory could not be anchored")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			retErr = errors.Join(retErr,
				errors.New("restore preparation: orphan-cleanup root could not be closed"))
		}
	}()
	anchored, err := root.Stat(".")
	if err != nil || !os.SameFile(identity, anchored) {
		return errors.New("restore preparation: data directory identity changed")
	}

	directory, err := root.Open(".")
	if err != nil {
		return errors.New("restore preparation: data directory could not be inspected")
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("restore preparation: data directory could not be inspected")
	}
	sort.Strings(names)
	var cleanupErr error
	for _, name := range names {
		if !isPreparedOrphanName(name) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(cleanupErr, err)
		}
		cleanupErr = errors.Join(cleanupErr, cleanupPreparedOrphan(ctx, root, name))
	}

	named, identityErr := os.Lstat(abs)
	if identityErr != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, named) {
		cleanupErr = errors.Join(cleanupErr, ErrCleanupPathChanged)
	}
	return cleanupErr
}

func isPreparedOrphanName(name string) bool {
	for _, shape := range preparedOrphanShapes {
		if len(name) != len(shape.prefix)+32+len(shape.suffix) ||
			name[:len(shape.prefix)] != shape.prefix ||
			name[len(name)-len(shape.suffix):] != shape.suffix {
			continue
		}
		token := name[len(shape.prefix) : len(name)-len(shape.suffix)]
		valid := true
		for _, character := range []byte(token) {
			if !('0' <= character && character <= '9') &&
				!('a' <= character && character <= 'f') {
				valid = false
				break
			}
		}
		if valid {
			return true
		}
	}
	return false
}

func cleanupPreparedOrphan(ctx context.Context, parent *os.Root, name string) (retErr error) {
	identity, err := parent.Lstat(name)
	if err != nil || identity.Mode()&os.ModeSymlink != 0 || !identity.IsDir() ||
		identity.Mode().Perm() != 0o700 {
		return errors.New("restore preparation: owned orphan is not a private real directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return errors.New("restore preparation: owned orphan could not be anchored")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			retErr = errors.Join(retErr,
				errors.New("restore preparation: owned orphan handle could not be closed"))
		}
	}()
	anchored, err := root.Stat(".")
	if err != nil || !os.SameFile(identity, anchored) {
		return errors.New("restore preparation: owned orphan identity changed")
	}

	directory, err := root.Open(".")
	if err != nil {
		return errors.New("restore preparation: owned orphan could not be inspected")
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("restore preparation: owned orphan could not be inspected")
	}
	for _, child := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.RemoveAll(child); err != nil {
			return errors.New("restore preparation: owned orphan contents could not be removed")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err = root.Open(".")
	if err != nil {
		return errors.New("restore preparation: owned orphan could not be verified")
	}
	_, remainingErr := directory.Readdirnames(1)
	closeErr = directory.Close()
	if !errors.Is(remainingErr, io.EOF) || closeErr != nil {
		return errors.New("restore preparation: owned orphan still contains data")
	}
	if err := syncPreparedRoot(root); err != nil {
		return err
	}
	current, err := parent.Lstat(name)
	if err != nil || !current.IsDir() || !os.SameFile(identity, current) {
		return ErrCleanupPathChanged
	}
	if err := parent.Remove(name); err != nil {
		return errors.New("restore preparation: owned orphan directory could not be removed")
	}
	if err := syncPreparedRoot(parent); err != nil {
		return err
	}
	return nil
}
