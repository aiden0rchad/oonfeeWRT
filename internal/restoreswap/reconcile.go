package restoreswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
)

type pathMatch uint8

const (
	matchMissing pathMatch = iota
	matchExpected
	matchOther
)

func pathRecord(ctx context.Context, root *os.Root, path string, expected fileRecord,
	maximum uint64) (pathMatch, error) {
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return matchMissing, nil
	}
	if err != nil {
		return matchOther, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return matchOther, errors.New("restore swap: member path is unsafe")
	}
	if info.Size() < 0 || uint64(info.Size()) > maximum {
		return matchOther, nil
	}
	record, err := hashRootFile(ctx, root, path, 1, maximum, false)
	if err != nil {
		return matchOther, err
	}
	if record == expected {
		return matchExpected, nil
	}
	return matchOther, nil
}

func requireHash(ctx context.Context, root *os.Root, path string, expected fileRecord,
	maximum uint64) error {
	match, err := pathRecord(ctx, root, path, expected, maximum)
	if err != nil {
		return err
	}
	if match != matchExpected {
		return errors.New("restore swap: file is missing or does not match its recorded digest")
	}
	return nil
}

func locateExpected(ctx context.Context, root *os.Root, expected fileRecord,
	primary, fallback string, maximum uint64) (string, error) {
	primaryMatch, err := pathRecord(ctx, root, primary, expected, maximum)
	if err != nil {
		return "", err
	}
	if primaryMatch == matchExpected {
		return primary, nil
	}
	fallbackMatch, err := pathRecord(ctx, root, fallback, expected, maximum)
	if err != nil {
		return "", err
	}
	if fallbackMatch == matchExpected {
		return fallback, nil
	}
	return "", errors.New("restore swap: recorded old controller member is unavailable")
}

func renameNoClobber(root *os.Root, source, destination string) error {
	if _, err := root.Lstat(destination); err == nil {
		return fmt.Errorf("restore swap: rename destination already exists: %w", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore swap: inspect rename destination: %w", err)
	}
	if err := root.Rename(source, destination); err != nil {
		return fmt.Errorf("restore swap: rename %s to %s: %w", filepath.Base(source), filepath.Base(destination), err)
	}
	return nil
}

func syncRenameParents(root *os.Root, source, destination string) error {
	sourceParent := filepath.Dir(source)
	destinationParent := filepath.Dir(destination)
	if err := syncDirectory(root, sourceParent); err != nil {
		return err
	}
	if destinationParent != sourceParent {
		return syncDirectory(root, destinationParent)
	}
	return nil
}

func maxFor(canonical string) uint64 {
	if canonical == keyringName {
		return keyringMaxBytes
	}
	return portablebackup.MaxDatabaseBytes
}

func restoreOld(ctx context.Context, root *os.Root, m *marker, runtime []byte) error {
	if err := ensureRollbackDirectory(root, *m); err != nil {
		return fmt.Errorf("restore swap: recover rollback directory: %w", err)
	}
	for _, member := range []struct {
		canonical, prepared, rollback string
		old, new                      fileRecord
		maximum                       uint64
	}{
		{databaseName, filepath.Join(m.PreparedDir, m.PreparedDatabase),
			filepath.Join(recoveryDirName, m.rollbackName(), databaseName),
			m.OldDatabase, m.PreparedDatabaseFile, portablebackup.MaxDatabaseBytes},
		{keyringName, filepath.Join(m.PreparedDir, m.PreparedKeyring),
			filepath.Join(recoveryDirName, m.rollbackName(), keyringName),
			m.OldKeyring, m.PreparedKeyringFile, keyringMaxBytes},
	} {
		if member.old == member.new {
			if err := requireHash(ctx, root, member.canonical, member.old, member.maximum); err != nil {
				return fmt.Errorf("restore swap: unchanged old member failed recovery: %w", err)
			}
			continue
		}
		canonicalMatch, err := pathRecord(ctx, root, member.canonical, member.old, member.maximum)
		if err != nil {
			return err
		}
		if canonicalMatch != matchExpected {
			newMatch, err := pathRecord(ctx, root, member.canonical, member.new, member.maximum)
			if err != nil {
				return err
			}
			if newMatch == matchExpected {
				preparedMatch, err := pathRecord(ctx, root, member.prepared, member.new, member.maximum)
				if err != nil {
					return err
				}
				if preparedMatch == matchMissing {
					if err := renameNoClobber(root, member.canonical, member.prepared); err != nil {
						return err
					}
					if err := syncRenameParents(root, member.canonical, member.prepared); err != nil {
						return err
					}
				} else if preparedMatch == matchExpected {
					if err := root.Remove(member.canonical); err != nil {
						return fmt.Errorf("restore swap: remove duplicate prepared member during rollback: %w", err)
					}
					if err := syncDirectory(root, "."); err != nil {
						return err
					}
				} else {
					return errors.New("restore swap: prepared member conflicts during rollback")
				}
			} else if newMatch == matchOther {
				return errors.New("restore swap: canonical member is tampered; refusing destructive rollback")
			}
			rollbackMatch, err := pathRecord(ctx, root, member.rollback, member.old, member.maximum)
			if err != nil || rollbackMatch != matchExpected {
				return errors.New("restore swap: raw rollback member is unavailable")
			}
			if err := renameNoClobber(root, member.rollback, member.canonical); err != nil {
				return err
			}
			if err := syncRenameParents(root, member.rollback, member.canonical); err != nil {
				return err
			}
		}
	}
	if _, err := validatePair(ctx, root, databaseName, keyringName, runtime,
		m.OldDatabase, m.OldKeyring); err != nil {
		return fmt.Errorf("restore swap: recovered old pair failed validation: %w", err)
	}
	m.State = stateSafety
	m.ValidatedCounts = recoveryDTO{}
	if err := replaceMarker(root, *m); err != nil {
		return err
	}
	return removeRollbackOwned(ctx, root, *m)
}

func removeRollbackOwned(ctx context.Context, root *os.Root, m marker) error {
	dir := filepath.Join(recoveryDirName, m.rollbackName())
	info, err := root.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("restore swap: rollback directory is unsafe to remove")
	}
	if err := requireDirectorySubset(root, dir, []string{databaseName, keyringName}); err != nil {
		return err
	}
	for _, member := range []struct {
		name string
		want fileRecord
		max  uint64
	}{
		{databaseName, m.OldDatabase, portablebackup.MaxDatabaseBytes},
		{keyringName, m.OldKeyring, keyringMaxBytes},
	} {
		path := filepath.Join(dir, member.name)
		match, err := pathRecord(ctx, root, path, member.want, member.max)
		if err != nil {
			return err
		}
		if match == matchMissing {
			continue
		}
		if match != matchExpected {
			return errors.New("restore swap: rollback member is unsafe to remove")
		}
		if err := root.Remove(path); err != nil {
			return fmt.Errorf("restore swap: remove rollback member: %w", err)
		}
	}
	if err := requireExactDirectory(root, dir, nil); err != nil {
		return err
	}
	if err := root.Remove(dir); err != nil {
		return fmt.Errorf("restore swap: remove rollback directory: %w", err)
	}
	return syncDirectory(root, recoveryDirName)
}
