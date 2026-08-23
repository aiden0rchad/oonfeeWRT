package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

const backupStepPages int32 = 128

type onlineBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

type backupOperations struct {
	removePath func(string) error
	removeRoot func(*os.Root, string) error
	beforeLink func()
	afterLink  func() error
}

func (ops backupOperations) defaults() backupOperations {
	if ops.removePath == nil {
		ops.removePath = os.Remove
	}
	if ops.removeRoot == nil {
		ops.removeRoot = func(root *os.Root, name string) error { return root.Remove(name) }
	}
	return ops
}

// BackupTo writes a transactionally consistent online backup to destination.
// The destination's parent must already exist and destination must not: the
// completed, verified file is installed with a no-clobber hard link only after
// its contents and metadata are durable.
func (db *DB) BackupTo(ctx context.Context, destination string) error {
	return db.backupTo(ctx, destination, backupOperations{})
}

func (db *DB) backupTo(ctx context.Context, destination string, ops backupOperations) (retErr error) {
	if db == nil || db.sql == nil {
		return errors.New("store: backup source is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ops = ops.defaults()
	_, parent, destinationName, root, err := openBackupDestination(destination)
	if err != nil {
		return err
	}
	defer root.Close()

	stagePath := ""
	rootTempName := ""
	published := false
	defer func() {
		if stagePath != "" {
			retErr = errors.Join(retErr, removeBackupFiles(stagePath, ops.removePath))
		}
		if rootTempName != "" {
			retErr = errors.Join(retErr,
				removeRootBackupFile(root, rootTempName, ops.removeRoot))
		}
		if retErr != nil && published {
			rollbackErr := removeRootBackupFile(root, destinationName, ops.removeRoot)
			if rollbackErr == nil {
				rollbackErr = syncBackupRoot(root)
			}
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()

	sourcePath, err := db.mainDatabasePath(ctx)
	if err != nil {
		return err
	}
	sourcePath, err = filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return fmt.Errorf("store: resolve backup source links: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(sourcePath), ".oonfeewrt-backup-*.db.tmp")
	if err != nil {
		return fmt.Errorf("store: create backup staging file: %w", err)
	}
	stagePath = tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("store: protect backup staging file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close backup staging file: %w", err)
	}

	if err := copyOnlineBackup(ctx, sourcePath, stagePath); err != nil {
		return err
	}
	if err := normalizeBackupJournal(ctx, stagePath); err != nil {
		return err
	}
	if err := db.verifyBackup(ctx, stagePath); err != nil {
		return err
	}
	if err := rejectBackupSidecars(stagePath); err != nil {
		return err
	}
	if err := syncBackupFile(stagePath); err != nil {
		return err
	}

	rootTemp, name, err := createBackupRootTemp(root)
	if err != nil {
		return err
	}
	rootTempName = name
	if err := copyBackupFile(ctx, stagePath, rootTemp); err != nil {
		return err
	}
	if err := removeBackupFiles(stagePath, ops.removePath); err != nil {
		return err
	}
	stagePath = ""

	if ops.beforeLink != nil {
		ops.beforeLink()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkBackupRoot(root, parent); err != nil {
		return err
	}
	if err := root.Link(rootTempName, destinationName); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("store: backup destination already exists: %w", os.ErrExist)
		}
		return fmt.Errorf("store: install backup without overwrite: %w", err)
	}
	published = true
	if ops.afterLink != nil {
		if err := ops.afterLink(); err != nil {
			return fmt.Errorf("store: finish backup publication: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkBackupRoot(root, parent); err != nil {
		return err
	}
	if err := syncBackupRoot(root); err != nil {
		return err
	}
	if err := removeRootBackupFile(root, rootTempName, ops.removeRoot); err != nil {
		return err
	}
	rootTempName = ""
	if err := syncBackupRoot(root); err != nil {
		return err
	}
	if err := checkBackupRoot(root, parent); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func openBackupDestination(destination string) (string, string, string, *os.Root, error) {
	if strings.TrimSpace(destination) == "" {
		return "", "", "", nil, errors.New("store: backup destination is empty")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("store: resolve backup destination: %w", err)
	}
	parent := filepath.Dir(abs)
	info, err := os.Lstat(parent)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("store: backup parent must already exist: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", "", "", nil,
			errors.New("store: backup parent must be a real directory, not a symlink")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("store: anchor backup parent: %w", err)
	}
	fail := func(err error) (string, string, string, *os.Root, error) {
		root.Close()
		return "", "", "", nil, err
	}
	anchored, err := root.Stat(".")
	if err != nil {
		return fail(fmt.Errorf("store: inspect anchored backup parent: %w", err))
	}
	if !anchored.IsDir() || anchored.Mode().Perm()&0o022 != 0 {
		return fail(errors.New("store: backup parent must be a private directory, not group- or world-writable"))
	}
	if err := checkBackupRoot(root, parent); err != nil {
		return fail(err)
	}
	name := filepath.Base(abs)
	if _, err := root.Lstat(name); err == nil {
		return fail(fmt.Errorf("store: backup destination already exists: %w", os.ErrExist))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("store: inspect backup destination: %w", err))
	}
	return abs, parent, name, root, nil
}

func (db *DB) mainDatabasePath(ctx context.Context) (string, error) {
	var path string
	if err := db.sql.QueryRowContext(ctx,
		`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		return "", fmt.Errorf("store: locate backup source: %w", err)
	}
	if path == "" {
		return "", errors.New("store: online backup requires a file-backed database")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("store: resolve backup source: %w", err)
	}
	return abs, nil
}

func sqliteFileURI(path, mode string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := &url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", mode)
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func copyOnlineBackup(ctx context.Context, sourcePath, destinationPath string) error {
	source, err := openReadOnlySQL(ctx, "sqlite", sourcePath)
	if err != nil {
		return fmt.Errorf("store: open online backup source: %w", err)
	}
	defer source.Close()
	conn, err := source.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: reserve online backup source: %w", err)
	}
	defer conn.Close()
	destinationURI, err := sqliteFileURI(destinationPath, "rw")
	if err != nil {
		return fmt.Errorf("store: resolve backup staging file: %w", err)
	}

	err = conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(onlineBackuper)
		if !ok {
			return errors.New("store: SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destinationURI)
		if err != nil {
			return fmt.Errorf("store: start online backup: %w", err)
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(backupStepPages)
			if err != nil {
				return fmt.Errorf("store: step online backup: %w", err)
			}
			if !more {
				break
			}
		}
		if err := backup.Finish(); err != nil {
			return fmt.Errorf("store: finish online backup: %w", err)
		}
		finished = true
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func normalizeBackupJournal(ctx context.Context, path string) error {
	dsn, err := sqliteFileURI(path, "rw")
	if err != nil {
		return fmt.Errorf("store: resolve backup journal path: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("store: open backup for journal normalization: %w", err)
	}
	db.SetMaxOpenConns(1)
	var mode string
	queryErr := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode)
	closeErr := db.Close()
	if queryErr != nil {
		return fmt.Errorf("store: normalize backup journal: %w", queryErr)
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("store: backup journal mode is %q, expected delete", mode)
	}
	if closeErr != nil {
		return fmt.Errorf("store: close normalized backup: %w", closeErr)
	}
	return nil
}

func (db *DB) verifyBackup(ctx context.Context, path string) (err error) {
	backup, err := OpenReadOnly(ctx, "sqlite", path, db.protector)
	if err != nil {
		return fmt.Errorf("store: verify backup schema and key state: %w", err)
	}
	defer func() {
		if closeErr := backup.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("store: close verified backup: %w", closeErr)
		}
	}()

	rows, err := backup.sql.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("store: verify backup integrity: %w", err)
	}
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			rows.Close()
			return fmt.Errorf("store: read backup integrity result: %w", err)
		}
		count++
		if result != "ok" {
			rows.Close()
			return fmt.Errorf("store: backup integrity check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: read backup integrity result: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close backup integrity check: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("store: backup integrity check returned %d rows", count)
	}

	foreignKeys, err := backup.sql.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("store: verify backup foreign keys: %w", err)
	}
	hasFailure := foreignKeys.Next()
	readErr := foreignKeys.Err()
	closeErr := foreignKeys.Close()
	if hasFailure {
		return errors.New("store: backup foreign-key check failed")
	}
	if readErr != nil {
		return fmt.Errorf("store: read backup foreign-key result: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("store: close backup foreign-key check: %w", closeErr)
	}
	return nil
}

func rejectBackupSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store: inspect backup sidecar %s: %w", suffix, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("store: backup sidecar %s is not a regular file", suffix)
		}
		if suffix != "-shm" && info.Size() != 0 {
			return fmt.Errorf("store: backup sidecar %s contains uncheckpointed data", suffix)
		}
		if err := os.Remove(path + suffix); err != nil {
			return fmt.Errorf("store: remove backup sidecar %s: %w", suffix, err)
		}
	}
	return nil
}

func syncBackupFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("store: protect completed backup: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("store: open completed backup for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: sync completed backup: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close completed backup: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("store: inspect completed backup: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("store: completed backup is not a regular file")
	}
	return nil
}

func createBackupRootTemp(root *os.Root) (*os.File, string, error) {
	for range 100 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, "", fmt.Errorf("store: generate backup staging name: %w", err)
		}
		name := ".oonfeewrt-backup-" + hex.EncodeToString(token[:]) + ".db.tmp"
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("store: create anchored backup staging file: %w", err)
		}
		return file, name, nil
	}
	return nil, "", errors.New("store: could not allocate a unique backup staging name")
}

type contextBackupReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextBackupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func copyBackupFile(ctx context.Context, sourcePath string, destination *os.File) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		destination.Close()
		return fmt.Errorf("store: open verified backup for publication: %w", err)
	}
	info, err := source.Stat()
	if err != nil {
		source.Close()
		destination.Close()
		return fmt.Errorf("store: inspect verified backup for publication: %w", err)
	}
	written, copyErr := io.CopyBuffer(destination,
		contextBackupReader{ctx: ctx, r: source}, make([]byte, 128*1024))
	sourceCloseErr := source.Close()
	if copyErr != nil {
		destination.Close()
		return fmt.Errorf("store: copy verified backup for publication: %w", copyErr)
	}
	if sourceCloseErr != nil {
		destination.Close()
		return fmt.Errorf("store: close verified backup after publication copy: %w", sourceCloseErr)
	}
	if written != info.Size() {
		destination.Close()
		return fmt.Errorf("store: backup publication copied %d bytes, expected %d", written, info.Size())
	}
	if err := destination.Chmod(0o600); err != nil {
		destination.Close()
		return fmt.Errorf("store: protect published backup staging file: %w", err)
	}
	if err := destination.Sync(); err != nil {
		destination.Close()
		return fmt.Errorf("store: sync published backup staging file: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("store: close published backup staging file: %w", err)
	}
	return nil
}

func checkBackupRoot(root *os.Root, path string) error {
	anchored, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("store: inspect anchored backup parent: %w", err)
	}
	named, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("store: revalidate backup parent: %w", err)
	}
	if !os.SameFile(anchored, named) {
		return errors.New("store: backup parent path changed during backup")
	}
	return nil
}

func syncBackupRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("store: open backup directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("store: sync backup directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("store: close backup directory: %w", err)
	}
	return nil
}

func removeRootBackupFile(root *os.Root, name string,
	remove func(*os.Root, string) error) error {
	return removeBackupFile(name, func(_ string) error { return remove(root, name) })
}

func removeBackupFiles(path string, remove func(string) error) error {
	var cleanupErr error
	cleanupErr = errors.Join(cleanupErr, removeBackupFile(path, remove))
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		cleanupErr = errors.Join(cleanupErr, removeBackupFile(path+suffix, remove))
	}
	return cleanupErr
}

func removeBackupFile(path string, remove func(string) error) error {
	first := remove(path)
	if first == nil || errors.Is(first, os.ErrNotExist) {
		return nil
	}
	second := remove(path)
	if errors.Is(second, os.ErrNotExist) {
		second = nil
	}
	return errors.Join(fmt.Errorf("store: remove backup file %s: %w", path, first), second)
}
