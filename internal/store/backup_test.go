package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

func openBackupFixture(t *testing.T) (*DB, string, *secrets.Keeper) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.db")
	protector := testProtector(t, path)
	db, err := Open(context.Background(), driver, path, protector)
	if err != nil {
		t.Fatalf("open backup fixture: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path, protector
}

func verifyBackupFile(t *testing.T, path string, protector *secrets.Keeper) *DB {
	t.Helper()
	ctx := context.Background()
	backup, err := OpenReadOnly(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("open completed backup: %v", err)
	}
	var version int
	if err := backup.SQL().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		backup.Close()
		t.Fatalf("read backup schema: %v", err)
	}
	if version != schemaVersion {
		backup.Close()
		t.Fatalf("backup schema=%d, want %d", version, schemaVersion)
	}
	var integrity string
	if err := backup.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		backup.Close()
		t.Fatalf("backup integrity=%q err=%v", integrity, err)
	}
	foreignKeys, err := backup.SQL().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		backup.Close()
		t.Fatalf("backup foreign-key check: %v", err)
	}
	if foreignKeys.Next() || foreignKeys.Err() != nil {
		foreignKeys.Close()
		backup.Close()
		t.Fatal("backup contains a foreign-key failure")
	}
	if err := foreignKeys.Close(); err != nil {
		backup.Close()
		t.Fatalf("close backup foreign-key check: %v", err)
	}
	var journal string
	if err := backup.SQL().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		backup.Close()
		t.Fatalf("read backup journal mode: %v", err)
	}
	if journal != "delete" {
		backup.Close()
		t.Fatalf("backup journal mode=%q, want delete", journal)
	}
	return backup
}

func assertNoBackupSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup left sidecar %q: %v", suffix, err)
		}
	}
}

func TestBackupToCapturesLiveWALWithConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	db, sourcePath, protector := openBackupFixture(t)
	if _, err := db.SQL().ExecContext(ctx, `
		PRAGMA wal_autocheckpoint=0;
		CREATE TABLE backup_race (id INTEGER PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO backup_race(id,value) VALUES (1,'initial');
		CREATE TABLE backup_bulk (data BLOB NOT NULL);
		INSERT INTO backup_bulk(data) VALUES (zeroblob(16777216));
	`); err != nil {
		t.Fatalf("seed live-WAL fixture: %v", err)
	}
	if info, err := os.Stat(sourcePath + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("source WAL is not live: info=%v err=%v", info, err)
	}

	started := make(chan struct{})
	done := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		defer close(done)
		var once sync.Once
		for id := 2; id <= 501; id++ {
			if _, err := db.SQL().ExecContext(ctx,
				`INSERT INTO backup_race(id,value) VALUES (?,?)`, id, "concurrent"); err != nil {
				writerErr <- err
				return
			}
			once.Do(func() { close(started) })
		}
	}()
	<-started

	destination := filepath.Join(t.TempDir(), "live backup ?#%.db")
	backupErr := db.BackupTo(ctx, destination)
	<-done
	select {
	case err := <-writerErr:
		t.Fatalf("concurrent writer: %v", err)
	default:
	}
	if backupErr != nil {
		t.Fatalf("BackupTo: %v", backupErr)
	}

	var liveCount int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_race`).Scan(&liveCount); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	backup := verifyBackupFile(t, destination, protector)
	var backupCount, maxID int
	if err := backup.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*),COALESCE(MAX(id),0) FROM backup_race`).Scan(&backupCount, &maxID); err != nil {
		backup.Close()
		t.Fatalf("read backup race rows: %v", err)
	}
	if err := backup.Close(); err != nil {
		t.Fatalf("close completed backup: %v", err)
	}
	if backupCount < 1 || backupCount > liveCount || maxID != backupCount {
		t.Fatalf("backup snapshot is inconsistent: count=%d max_id=%d live=%d",
			backupCount, maxID, liveCount)
	}
	assertNoBackupSidecars(t, destination)
}

func TestBackupToCancellationRemovesPartialFiles(t *testing.T) {
	ctx := context.Background()
	db, _, _ := openBackupFixture(t)
	if _, err := db.SQL().ExecContext(ctx, `
		CREATE TABLE backup_cancel_bulk (data BLOB NOT NULL);
		INSERT INTO backup_cancel_bulk(data) VALUES (zeroblob(67108864));
	`); err != nil {
		t.Fatalf("seed cancellation fixture: %v", err)
	}

	out := t.TempDir()
	destination := filepath.Join(out, "cancelled.db")
	backupCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- db.BackupTo(backupCtx, destination) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(out)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".oonfeewrt-backup-") {
				found = true
				break
			}
		}
		if found {
			cancel()
			break
		}
		select {
		case err := <-result:
			t.Fatalf("backup completed before cancellation: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("backup staging file did not appear")
		}
		time.Sleep(100 * time.Microsecond)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("BackupTo cancellation=%v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled backup left files: %v", entries)
	}
}

func TestBackupToPathModeAndNoOverwrite(t *testing.T) {
	ctx := context.Background()
	db, _, protector := openBackupFixture(t)
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE backup_marker (value TEXT NOT NULL);
		INSERT INTO backup_marker(value) VALUES ('complete')`); err != nil {
		t.Fatal(err)
	}

	t.Run("missing parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "missing")
		if err := db.BackupTo(ctx, filepath.Join(parent, "backup.db")); err == nil {
			t.Fatal("backup created a missing parent")
		}
		if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing parent was changed: %v", err)
		}
	})

	t.Run("symlink parent", func(t *testing.T) {
		root := t.TempDir()
		realParent := filepath.Join(root, "real")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(root, "linked")
		if err := os.Symlink(realParent, linkedParent); err != nil {
			t.Fatal(err)
		}
		if err := db.BackupTo(ctx, filepath.Join(linkedParent, "backup.db")); err == nil {
			t.Fatal("backup accepted a symlink parent")
		}
	})

	t.Run("writable parent", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		if err := db.BackupTo(ctx, filepath.Join(parent, "backup.db")); err == nil {
			t.Fatal("backup accepted a group- or world-writable parent")
		}
	})

	t.Run("existing file", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "existing.db")
		before := []byte("do not replace")
		if err := os.WriteFile(destination, before, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := db.BackupTo(ctx, destination); !errors.Is(err, os.ErrExist) {
			t.Fatalf("existing destination error=%v, want os.ErrExist", err)
		}
		after, err := os.ReadFile(destination)
		if err != nil || string(after) != string(before) {
			t.Fatalf("existing destination changed: %q err=%v", after, err)
		}
	})

	t.Run("existing symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("target sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(dir, "backup.db")
		if err := os.Symlink(target, destination); err != nil {
			t.Fatal(err)
		}
		if err := db.BackupTo(ctx, destination); !errors.Is(err, os.ErrExist) {
			t.Fatalf("symlink destination error=%v, want os.ErrExist", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "target sentinel" {
			t.Fatalf("symlink target changed: %q err=%v", got, err)
		}
	})

	t.Run("destination created during backup", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "raced.db")
		before := []byte("concurrent winner")
		ops := backupOperations{beforeLink: func() {
			if err := os.WriteFile(destination, before, 0o600); err != nil {
				t.Errorf("create concurrent destination: %v", err)
			}
		}}
		if err := db.backupTo(ctx, destination, ops); !errors.Is(err, os.ErrExist) {
			t.Fatalf("concurrent destination error=%v, want os.ErrExist", err)
		}
		after, err := os.ReadFile(destination)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("concurrent destination changed: %q err=%v", after, err)
		}
	})

	t.Run("complete private file", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "backup ?#%.db")
		if err := db.BackupTo(ctx, destination); err != nil {
			t.Fatalf("BackupTo: %v", err)
		}
		info, err := os.Lstat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("backup mode=%v, want regular 0600", info.Mode())
		}
		backup := verifyBackupFile(t, destination, protector)
		var marker string
		if err := backup.SQL().QueryRowContext(ctx, `SELECT value FROM backup_marker`).Scan(&marker); err != nil {
			backup.Close()
			t.Fatalf("read backup marker: %v", err)
		}
		if err := backup.Close(); err != nil {
			t.Fatal(err)
		}
		if marker != "complete" {
			t.Fatalf("backup marker=%q", marker)
		}
		assertNoBackupSidecars(t, destination)

		before, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		beforeHash := sha256.Sum256(before)
		if err := db.BackupTo(ctx, destination); !errors.Is(err, os.ErrExist) {
			t.Fatalf("second backup error=%v, want os.ErrExist", err)
		}
		after, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(after) != beforeHash {
			t.Fatal("second backup changed the completed destination")
		}
	})
}

func TestBackupToParentAnchoring(t *testing.T) {
	ctx := context.Background()
	db, _, _ := openBackupFixture(t)

	t.Run("parent path swap", func(t *testing.T) {
		base := t.TempDir()
		parent := filepath.Join(base, "output")
		replacement := filepath.Join(base, "replacement")
		moved := filepath.Join(base, "moved")
		for _, path := range []string{parent, replacement} {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		destination := filepath.Join(parent, "backup.db")
		ops := backupOperations{beforeLink: func() {
			if err := os.Rename(parent, moved); err != nil {
				t.Errorf("move anchored parent: %v", err)
				return
			}
			if err := os.Rename(replacement, parent); err != nil {
				t.Errorf("install replacement parent: %v", err)
			}
		}}
		if err := db.backupTo(ctx, destination, ops); err == nil ||
			!strings.Contains(err.Error(), "parent path changed") {
			t.Fatalf("parent swap error=%v", err)
		}
		assertDirectoryEmpty(t, parent)
		assertDirectoryEmpty(t, moved)
	})

	t.Run("ancestor symlink retarget", func(t *testing.T) {
		base := t.TempDir()
		first := filepath.Join(base, "first")
		second := filepath.Join(base, "second")
		for _, path := range []string{first, second} {
			if err := os.MkdirAll(filepath.Join(path, "private"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		alias := filepath.Join(base, "alias")
		if err := os.Symlink(first, alias); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(alias, "private", "backup.db")
		ops := backupOperations{beforeLink: func() {
			if err := os.Remove(alias); err != nil {
				t.Errorf("remove ancestor symlink: %v", err)
				return
			}
			if err := os.Symlink(second, alias); err != nil {
				t.Errorf("retarget ancestor symlink: %v", err)
			}
		}}
		if err := db.backupTo(ctx, destination, ops); err == nil ||
			!strings.Contains(err.Error(), "parent path changed") {
			t.Fatalf("ancestor retarget error=%v", err)
		}
		assertDirectoryEmpty(t, filepath.Join(first, "private"))
		assertDirectoryEmpty(t, filepath.Join(second, "private"))
	})
}

func TestBackupToRollsBackLateFailures(t *testing.T) {
	db, _, _ := openBackupFixture(t)

	t.Run("late cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		out := t.TempDir()
		destination := filepath.Join(out, "cancelled.db")
		err := db.backupTo(ctx, destination, backupOperations{afterLink: func() error {
			cancel()
			return nil
		}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("late cancellation=%v, want context.Canceled", err)
		}
		assertDirectoryEmpty(t, out)
	})

	t.Run("post-link failure", func(t *testing.T) {
		failure := errors.New("injected post-link failure")
		out := t.TempDir()
		destination := filepath.Join(out, "failed.db")
		err := db.backupTo(context.Background(), destination,
			backupOperations{afterLink: func() error { return failure }})
		if !errors.Is(err, failure) {
			t.Fatalf("post-link error=%v, want injected failure", err)
		}
		assertDirectoryEmpty(t, out)
	})

	t.Run("cleanup failure is reported and retried", func(t *testing.T) {
		failure := errors.New("injected cleanup failure")
		out := t.TempDir()
		destination := filepath.Join(out, "cleanup.db")
		failed := false
		ops := backupOperations{removeRoot: func(root *os.Root, name string) error {
			if !failed {
				failed = true
				return failure
			}
			return root.Remove(name)
		}}
		err := db.backupTo(context.Background(), destination, ops)
		if !errors.Is(err, failure) {
			t.Fatalf("cleanup error=%v, want injected failure", err)
		}
		assertDirectoryEmpty(t, out)
	})
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory %s contains backup artifacts: %v", path, entries)
	}
}
