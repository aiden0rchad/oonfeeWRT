package restoreswap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
)

type preparedPaths struct {
	dir, database, keyring    string
	databaseFile, keyringFile fileRecord
}

func openDataRoot(dataDir string) (*os.Root, string, error) {
	if strings.TrimSpace(dataDir) == "" || len(dataDir) > 4096 || !utf8.ValidString(dataDir) || strings.IndexByte(dataDir, 0) >= 0 {
		return nil, "", errors.New("restore swap: data directory path is invalid")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, "", fmt.Errorf("restore swap: resolve data directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, "", errors.New("restore swap: data directory must be a private real directory")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, "", fmt.Errorf("restore swap: anchor data directory: %w", err)
	}
	anchored, err := root.Stat(".")
	if err != nil || !os.SameFile(info, anchored) {
		root.Close()
		return nil, "", errors.New("restore swap: data directory identity changed")
	}
	return root, abs, nil
}

func checkNamedDataRoot(root *os.Root) error {
	anchored, err := root.Stat(".")
	if err != nil {
		return errors.New("restore swap: anchored data directory is unavailable")
	}
	named, err := os.Lstat(root.Name())
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !named.IsDir() ||
		named.Mode().Perm()&0o077 != 0 || !os.SameFile(anchored, named) {
		return errors.New("restore swap: named data directory identity changed")
	}
	return nil
}

func ensureRecoveryDir(root *os.Root) error {
	_, err := root.Lstat(recoveryDirName)
	if errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir(recoveryDirName, 0o700); err != nil {
			return fmt.Errorf("restore swap: create recovery directory: %w", err)
		}
		if err := syncDirectory(root, "."); err != nil {
			return err
		}
		err = nil
	}
	if err != nil {
		return err
	}
	return validateRecoveryDir(root)
}

func validateRecoveryDir(root *os.Root) error {
	info, err := root.Lstat(recoveryDirName)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("restore swap: recovery directory must be a private real directory")
	}
	return ensureSameFilesystem(root, ".", recoveryDirName)
}

func inspectPrepared(ctx context.Context, root *os.Root, absData string, prepared PreparedPair) (preparedPaths, error) {
	dir, database, err := directChildMember(absData, prepared.DatabasePath)
	if err != nil {
		return preparedPaths{}, fmt.Errorf("restore swap: prepared database path: %w", err)
	}
	keyDir, keyring, err := directChildMember(absData, prepared.KeyringPath)
	if err != nil {
		return preparedPaths{}, fmt.Errorf("restore swap: prepared keyring path: %w", err)
	}
	if dir != keyDir || database == keyring || dir == recoveryDirName {
		return preparedPaths{}, errors.New("restore swap: prepared pair must be two files in one dedicated directory")
	}
	if err := requireExactDirectory(root, dir, []string{database, keyring}); err != nil {
		return preparedPaths{}, err
	}
	databaseFile, err := hashRootFile(ctx, root, filepath.Join(dir, database), 1, portablebackup.MaxDatabaseBytes, true)
	if err != nil {
		return preparedPaths{}, fmt.Errorf("restore swap: hash prepared database: %w", err)
	}
	keyringFile, err := hashRootFile(ctx, root, filepath.Join(dir, keyring), 1, keyringMaxBytes, true)
	if err != nil {
		return preparedPaths{}, fmt.Errorf("restore swap: hash prepared keyring: %w", err)
	}
	if err := syncDirectory(root, dir); err != nil {
		return preparedPaths{}, err
	}
	if err := ensureSameFilesystem(root, ".", dir); err != nil {
		return preparedPaths{}, err
	}
	if err := ensureSameFilesystem(root, ".", filepath.Join(dir, database)); err != nil {
		return preparedPaths{}, err
	}
	if err := ensureSameFilesystem(root, ".", filepath.Join(dir, keyring)); err != nil {
		return preparedPaths{}, err
	}
	return preparedPaths{dir: dir, database: database, keyring: keyring,
		databaseFile: databaseFile, keyringFile: keyringFile}, nil
}

func directChildMember(dataDir, path string) (string, string, error) {
	if strings.TrimSpace(path) == "" || len(path) > 4096 || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return "", "", errors.New("path is invalid")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(dataDir, abs)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return "", "", errors.New("path is outside the controller data directory")
	}
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	if len(parts) != 2 || !safeBaseName(parts[0]) || !safeBaseName(parts[1]) {
		return "", "", errors.New("path must name a file in one immediate-child directory")
	}
	return parts[0], parts[1], nil
}

func safeBaseName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) ||
		strings.ContainsAny(name, `/\\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func requireExactDirectory(root *os.Root, name string, members []string) error {
	info, err := root.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("restore swap: prepared directory must be a private real directory")
	}
	directory, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("restore swap: open prepared directory: %w", err)
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	sort.Strings(names)
	expected := append([]string(nil), members...)
	sort.Strings(expected)
	if len(names) != len(expected) {
		return errors.New("restore swap: prepared directory contains unexpected entries")
	}
	for index := range names {
		if names[index] != expected[index] {
			return errors.New("restore swap: prepared directory contains unexpected entries")
		}
	}
	return nil
}

func verifyPrepared(ctx context.Context, root *os.Root, m marker) error {
	if err := requireExactDirectory(root, m.PreparedDir, []string{m.PreparedDatabase, m.PreparedKeyring}); err != nil {
		return err
	}
	for _, member := range []struct {
		path string
		want fileRecord
		max  uint64
	}{
		{filepath.Join(m.PreparedDir, m.PreparedDatabase), m.PreparedDatabaseFile, portablebackup.MaxDatabaseBytes},
		{filepath.Join(m.PreparedDir, m.PreparedKeyring), m.PreparedKeyringFile, keyringMaxBytes},
	} {
		got, err := hashRootFile(ctx, root, member.path, 1, member.max, true)
		if err != nil || got != member.want {
			return errors.New("restore swap: prepared pair changed after intent creation")
		}
	}
	return nil
}

func removePreparedOwned(ctx context.Context, root *os.Root, m marker, allowPartial bool) error {
	info, err := root.Lstat(m.PreparedDir)
	if errors.Is(err, os.ErrNotExist) && allowPartial {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("restore swap: prepared directory is unsafe to remove")
	}
	if err := requireDirectorySubset(root, m.PreparedDir,
		[]string{m.PreparedDatabase, m.PreparedKeyring}); err != nil {
		return err
	}
	for _, member := range []struct {
		name string
		want fileRecord
		max  uint64
	}{
		{m.PreparedDatabase, m.PreparedDatabaseFile, portablebackup.MaxDatabaseBytes},
		{m.PreparedKeyring, m.PreparedKeyringFile, keyringMaxBytes},
	} {
		path := filepath.Join(m.PreparedDir, member.name)
		got, hashErr := hashRootFile(ctx, root, path, 1, member.max, false)
		if errors.Is(hashErr, os.ErrNotExist) && allowPartial {
			continue
		}
		if hashErr != nil || got != member.want {
			return errors.New("restore swap: prepared member is unsafe to remove")
		}
		if err := root.Remove(path); err != nil {
			return fmt.Errorf("restore swap: remove prepared member: %w", err)
		}
	}
	if err := requireExactDirectory(root, m.PreparedDir, nil); err != nil {
		return err
	}
	if err := root.Remove(m.PreparedDir); err != nil {
		return fmt.Errorf("restore swap: remove prepared directory: %w", err)
	}
	return syncDirectory(root, ".")
}

func hashRootFile(ctx context.Context, root *os.Root, path string, minimum, maximum uint64, syncFile bool) (fileRecord, error) {
	if err := ctx.Err(); err != nil {
		return fileRecord{}, err
	}
	before, err := root.Lstat(path)
	if err != nil {
		return fileRecord{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 ||
		before.Size() < 0 || uint64(before.Size()) < minimum || uint64(before.Size()) > maximum {
		return fileRecord{}, errors.New("file must be a bounded regular 0600 file")
	}
	file, err := root.Open(path)
	if err != nil {
		return fileRecord{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return fileRecord{}, errors.New("file identity changed while opening")
	}
	hasher := sha256.New()
	buffer := make([]byte, 128<<10)
	defer clear(buffer)
	remaining := opened.Size()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return fileRecord{}, err
		}
		amount := int64(len(buffer))
		if remaining < amount {
			amount = remaining
		}
		if _, err := io.ReadFull(file, buffer[:amount]); err != nil {
			return fileRecord{}, err
		}
		_, _ = hasher.Write(buffer[:amount])
		clear(buffer[:amount])
		remaining -= amount
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fileRecord{}, errors.New("file changed while hashing")
	}
	if syncFile {
		if err := file.Sync(); err != nil {
			return fileRecord{}, err
		}
	}
	after, err := root.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) || after.Size() != opened.Size() {
		return fileRecord{}, errors.New("file identity changed while hashing")
	}
	return fileRecord{Size: uint64(opened.Size()), SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func readRootRegular(root *os.Root, path string, minimum, maximum uint64, exactMode bool) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || (exactMode && info.Mode().Perm() != 0o600) ||
		info.Size() < 0 || uint64(info.Size()) < minimum || uint64(info.Size()) > maximum {
		return nil, errors.New("unsafe or oversized regular file")
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("file identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || uint64(len(data)) > maximum {
		clear(data)
		return nil, errors.New("file changed or exceeded its size ceiling")
	}
	after, err := root.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) ||
		after.Size() != opened.Size() || int64(len(data)) != opened.Size() {
		clear(data)
		return nil, errors.New("file identity changed while reading")
	}
	return data, nil
}

func writeJSONAtomic(root *os.Root, path string, value any, noClobber bool) (retErr error) {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("restore swap: encode durable marker: %w", err)
	}
	defer clear(data)
	if len(data) == 0 || len(data) > markerMaxBytes {
		return errors.New("restore swap: durable marker exceeds its size ceiling")
	}
	parent, base := filepath.Dir(path), filepath.Base(path)
	if !safeBaseName(base) || (parent != "." && !safeBaseName(parent)) {
		return errors.New("restore swap: durable marker path is invalid")
	}
	temp := "." + base + "." + randomSuffix() + ".tmp"
	if temp == "" {
		return errors.New("restore swap: allocate marker name failed")
	}
	tempPath := filepath.Join(parent, temp)
	file, err := root.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("restore swap: create marker temporary: %w", err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			retErr = errors.Join(retErr, ignoreNotExist(root.Remove(tempPath)))
		}
	}()
	if written, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("restore swap: write marker temporary: %w", err)
	} else if written != len(data) {
		file.Close()
		return fmt.Errorf("restore swap: write marker temporary: %w", io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("restore swap: sync marker temporary: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("restore swap: close marker temporary: %w", err)
	}
	if noClobber {
		if err := root.Link(tempPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("restore swap: durable marker already exists: %w", os.ErrExist)
			}
			return fmt.Errorf("restore swap: publish durable marker: %w", err)
		}
	} else if err := root.Rename(tempPath, path); err != nil {
		return fmt.Errorf("restore swap: replace durable marker: %w", err)
	} else {
		removeTemp = false
	}
	if err := syncDirectory(root, parent); err != nil {
		return err
	}
	if noClobber {
		if err := root.Remove(tempPath); err != nil {
			return err
		}
		removeTemp = false
		if err := syncDirectory(root, parent); err != nil {
			return err
		}
	}
	return nil
}

func randomSuffix() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(value[:])
}

func syncDirectory(root *os.Root, path string) error {
	directory, err := root.Open(path)
	if err != nil {
		return fmt.Errorf("restore swap: open directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return fmt.Errorf("restore swap: sync directory: %w", err)
	}
	return directory.Close()
}

func ensureSameFilesystem(root *os.Root, left, right string) error {
	leftInfo, err := root.Stat(left)
	if err != nil {
		return err
	}
	rightInfo, err := root.Stat(right)
	if err != nil {
		return err
	}
	if !sameDevice(leftInfo, rightInfo) {
		return errors.New("restore swap: prepared pair is not on the controller data filesystem")
	}
	return nil
}

func removeSafeSQLiteSidecars(root *os.Root) error {
	for _, name := range []string{databaseName + "-wal", databaseName + "-shm", databaseName + "-journal"} {
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			info.Mode().Perm() != 0o600 || info.Size() != 0 {
			return fmt.Errorf("restore swap: SQLite sidecar %s is nonempty or unsafe", name)
		}
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("restore swap: remove empty SQLite sidecar %s: %w", name, err)
		}
	}
	return syncDirectory(root, ".")
}

func ignoreNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func requireDirectorySubset(root *os.Root, name string, allowed []string) error {
	info, err := root.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("restore swap: owned directory is unsafe")
	}
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	wanted := make(map[string]struct{}, len(allowed))
	for _, entry := range allowed {
		wanted[entry] = struct{}{}
	}
	for _, entry := range names {
		if _, ok := wanted[entry]; !ok {
			return errors.New("restore swap: owned directory contains an unexpected entry")
		}
	}
	return nil
}
