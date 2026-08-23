package controllerrestore

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

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var testExportPassphrase = []byte("restore-preview-export-passphrase")

type backupFixture struct {
	artifact string
	scratch  string
}

func newBackupFixture(t *testing.T, manifestSchema int, mutate func(*testing.T, *store.DB)) backupFixture {
	t.Helper()
	ctx := context.Background()
	source := t.TempDir()
	keeper, err := secrets.Create(filepath.Join(source, secrets.FileName),
		[]byte("restore-preview-runtime-passphrase"),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(source, "controller.db")
	db, err := store.Open(ctx, "sqlite", dbPath, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Site(ctx); err != nil {
		t.Fatal(err)
	}
	hash, err := secrets.HashPassword([]byte("restore-preview-owner-password"),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFirstAdmin(ctx, "restore-owner", hash); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(t, db)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	artifactDir := t.TempDir()
	artifact := filepath.Join(artifactDir, "controller.oowrt-backup")
	if _, err := portablebackup.Create(ctx, artifact, dbPath, keeper,
		testExportPassphrase, portablebackup.Metadata{
			ControllerVersion: "v0.1.0-test", SchemaVersion: manifestSchema,
			CreatedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		}); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	return backupFixture{artifact: artifact, scratch: scratch}
}

func assertNoResidue(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("restore preview left %d scratch entries: %v", len(entries), entries)
	}
}

func TestInspectAuthenticatesMigratesValidatesAndCleans(t *testing.T) {
	fixture := newBackupFixture(t, store.CurrentSchemaVersion(), nil)
	before, err := os.ReadFile(fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)
	passphraseBefore := bytes.Clone(testExportPassphrase)

	preview, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
		testExportPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceSchema != store.CurrentSchemaVersion() ||
		preview.TargetSchema != store.CurrentSchemaVersion() ||
		preview.Counts.Schema != store.CurrentSchemaVersion() ||
		preview.Manifest.SchemaVersion != store.CurrentSchemaVersion() ||
		preview.Counts.Devices != 0 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if !bytes.Equal(testExportPassphrase, passphraseBefore) {
		t.Fatal("restore preview changed its caller-owned passphrase buffer")
	}
	after, err := os.ReadFile(fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	if afterHash := sha256.Sum256(after); beforeHash != afterHash || !bytes.Equal(before, after) {
		t.Fatal("restore preview changed its source artifact")
	}
	assertNoResidue(t, fixture.scratch)
}

func TestInspectWrongPassphraseAndCancellationLeaveNoResidue(t *testing.T) {
	fixture := newBackupFixture(t, store.CurrentSchemaVersion(), nil)
	if _, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
		[]byte("wrong-restore-preview-passphrase")); err == nil {
		t.Fatal("wrong export passphrase was accepted")
	}
	assertNoResidue(t, fixture.scratch)

	ctx, cancel := context.WithCancel(context.Background())
	_, err := inspect(ctx, fixture.artifact, fixture.scratch, testExportPassphrase,
		inspectOperations{afterOpen: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preview error=%v, want context.Canceled", err)
	}
	assertNoResidue(t, fixture.scratch)
}

func TestInspectRejectsSchemaMismatchFutureAndUnsupported(t *testing.T) {
	cases := []struct {
		name           string
		manifestSchema int
		mutate         func(*testing.T, *store.DB)
		want           string
	}{
		{
			name: "manifest mismatch", manifestSchema: store.CurrentSchemaVersion() - 1,
			want: "does not match database schema",
		},
		{
			name: "future", manifestSchema: store.CurrentSchemaVersion() + 1,
			mutate: func(t *testing.T, db *store.DB) {
				if _, err := db.SQL().Exec(`UPDATE schema_version SET version=?`, store.CurrentSchemaVersion()+1); err != nil {
					t.Fatal(err)
				}
			},
			want: "newer than this controller",
		},
		{
			name: "unsupported", manifestSchema: minimumSourceSchema - 1,
			mutate: func(t *testing.T, db *store.DB) {
				if _, err := db.SQL().Exec(`UPDATE schema_version SET version=?`, minimumSourceSchema-1); err != nil {
					t.Fatal(err)
				}
			},
			want: "is unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newBackupFixture(t, tc.manifestSchema, tc.mutate)
			_, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
				testExportPassphrase)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Inspect error=%v, want %q", err, tc.want)
			}
			assertNoResidue(t, fixture.scratch)
		})
	}
}

func TestInspectMigratesSupportedSchema18Copy(t *testing.T) {
	fixture := newBackupFixture(t, 18, func(t *testing.T, db *store.DB) {
		if _, err := db.SQL().Exec(`
DROP INDEX admins_username_nocase;
ALTER TABLE admins DROP COLUMN deleted_at;
ALTER TABLE admins DROP COLUMN enabled;
ALTER TABLE admins DROP COLUMN role;
UPDATE schema_version SET version=18 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
			t.Fatal(err)
		}
	})
	preview, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
		testExportPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceSchema != 18 || preview.TargetSchema != store.CurrentSchemaVersion() ||
		preview.Counts.Schema != store.CurrentSchemaVersion() {
		t.Fatalf("unexpected migrated preview: %+v", preview)
	}
	assertNoResidue(t, fixture.scratch)
}

func TestInspectRejectsCorruptSecretAndCleansSidecars(t *testing.T) {
	fixture := newBackupFixture(t, store.CurrentSchemaVersion(), func(t *testing.T, db *store.DB) {
		if _, err := db.SQL().Exec(`UPDATE secret_state SET key_check=zeroblob(length(key_check))`); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
		testExportPassphrase); err == nil {
		t.Fatal("corrupt sealed state was accepted")
	}
	assertNoResidue(t, fixture.scratch)
}

func TestInspectRejectsDatabaseWithoutUsableOwner(t *testing.T) {
	fixture := newBackupFixture(t, store.CurrentSchemaVersion(), func(t *testing.T, db *store.DB) {
		if _, err := db.SQL().Exec(`UPDATE admins SET enabled=0`); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
		testExportPassphrase); err == nil {
		t.Fatal("database without a usable owner was accepted")
	}
	assertNoResidue(t, fixture.scratch)
}

func TestInspectRejectsUnsafeParentsAndBoundedPaths(t *testing.T) {
	fixture := newBackupFixture(t, store.CurrentSchemaVersion(), nil)
	writable := filepath.Join(t.TempDir(), "writable")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), fixture.artifact, writable,
		testExportPassphrase); err == nil {
		t.Fatal("group/world-writable scratch parent was accepted")
	}

	realParent := t.TempDir()
	symlinkParent := filepath.Join(t.TempDir(), "scratch-link")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), fixture.artifact, symlinkParent,
		testExportPassphrase); err == nil {
		t.Fatal("symlink scratch parent was accepted")
	}
	assertNoResidue(t, realParent)
	if _, err := Inspect(context.Background(), strings.Repeat("x", maxPathBytes+1),
		fixture.scratch, testExportPassphrase); err == nil {
		t.Fatal("oversized artifact path was accepted")
	}
	missing := filepath.Join(fixture.scratch, "internal-artifact-path-must-not-escape")
	if _, err := Inspect(context.Background(), missing, fixture.scratch,
		testExportPassphrase); err == nil || strings.Contains(err.Error(), missing) ||
		strings.Contains(err.Error(), fixture.scratch) {
		t.Fatalf("missing-artifact error exposed a path: %v", err)
	}
	assertNoResidue(t, fixture.scratch)
}

func TestInspectConcurrentCallsUseIndependentScratch(t *testing.T) {
	fixture := newBackupFixture(t, store.CurrentSchemaVersion(), nil)
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			preview, err := Inspect(context.Background(), fixture.artifact, fixture.scratch,
				testExportPassphrase)
			if err == nil && preview.Counts.Schema != store.CurrentSchemaVersion() {
				err = errors.New("concurrent preview returned the wrong schema")
			}
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertNoResidue(t, fixture.scratch)
}
