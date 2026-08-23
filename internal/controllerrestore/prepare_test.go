package controllerrestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var testDestinationRuntimePassphrase = []byte("restore-preview-runtime-passphrase")

type prepareFixture struct {
	artifact    string
	dataDir     string
	database    string
	keyring     string
	live        *secrets.Keeper
	baseline    []string
	databaseSum [sha256.Size]byte
	keyringSum  [sha256.Size]byte
}

func newPrepareFixture(t *testing.T, manifestSchema int,
	mutate func(*testing.T, *store.DB)) *prepareFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	keyring := filepath.Join(dataDir, secrets.FileName)
	live, err := secrets.Create(keyring, testDestinationRuntimePassphrase,
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	database := filepath.Join(dataDir, preparedDatabaseName)
	db, err := store.Open(ctx, "sqlite", database, live)
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
	artifact := filepath.Join(t.TempDir(), "controller.oowrt-backup")
	if _, err := portablebackup.Create(ctx, artifact, database, live,
		testExportPassphrase, portablebackup.Metadata{
			ControllerVersion: "v0.1.0-test", SchemaVersion: manifestSchema,
			CreatedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		}); err != nil {
		t.Fatal(err)
	}
	fixture := &prepareFixture{
		artifact: artifact, dataDir: dataDir, database: database,
		keyring: keyring, live: live,
	}
	fixture.baseline = directoryNames(t, dataDir)
	fixture.databaseSum = fileSum(t, database)
	fixture.keyringSum = fileSum(t, keyring)
	return fixture
}

func directoryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func fileSum(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func (f *prepareFixture) assertLiveUnchanged(t *testing.T) {
	t.Helper()
	if got := fileSum(t, f.database); got != f.databaseSum {
		t.Fatal("restore preparation changed the live database")
	}
	if got := fileSum(t, f.keyring); got != f.keyringSum {
		t.Fatal("restore preparation changed the live keyring")
	}
}

func (f *prepareFixture) assertNoResidue(t *testing.T) {
	t.Helper()
	if got := directoryNames(t, f.dataDir); !reflect.DeepEqual(got, f.baseline) {
		t.Fatalf("data directory entries=%v, want baseline %v", got, f.baseline)
	}
	f.assertLiveUnchanged(t)
}

func TestPrepareBuildsExactValidatedPairAndCleans(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	exportPassphrase := bytes.Clone(testExportPassphrase)
	runtimePassphrase := bytes.Clone(testDestinationRuntimePassphrase)
	prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, exportPassphrase, runtimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exportPassphrase, testExportPassphrase) ||
		!bytes.Equal(runtimePassphrase, testDestinationRuntimePassphrase) {
		t.Fatal("Prepare changed a caller-owned passphrase buffer")
	}
	if strings.Contains(fmt.Sprintf("%#v", prepared), string(exportPassphrase)) ||
		strings.Contains(fmt.Sprintf("%#v", prepared), string(runtimePassphrase)) {
		t.Fatal("Prepared retained a passphrase")
	}
	preparedType := reflect.TypeOf(prepared).Elem()
	for index := range preparedType.NumField() {
		if preparedType.Field(index).Type.Kind() == reflect.Slice {
			t.Fatalf("Prepared field %s can retain caller bytes", preparedType.Field(index).Name)
		}
	}
	preview := prepared.Preview()
	if preview.SourceSchema != store.CurrentSchemaVersion() ||
		preview.TargetSchema != store.CurrentSchemaVersion() ||
		preview.Counts.Schema != store.CurrentSchemaVersion() {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if filepath.Dir(prepared.dir.path) != fixture.dataDir {
		t.Fatalf("prepared directory=%s is not an immediate data-dir child", prepared.dir.path)
	}
	if names := directoryNames(t, prepared.dir.path); !reflect.DeepEqual(names,
		[]string{preparedDatabaseName, secrets.FileName}) {
		t.Fatalf("prepared members=%v, want exact database/keyring pair", names)
	}
	for _, name := range []string{preparedDatabaseName, secrets.FileName} {
		info, err := os.Lstat(filepath.Join(prepared.dir.path, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("prepared member %s mode=%v err=%v", name, info.Mode(), err)
		}
	}
	encodedPair, err := json.Marshal(prepared.pair)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedPair, []byte(prepared.dir.path)) {
		t.Fatal("PreparedPair JSON exposed internal filesystem paths")
	}
	preparedPath := prepared.dir.path
	if err := prepared.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if _, err := os.Lstat(preparedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared directory remains after cleanup: %v", err)
	}
	fixture.assertNoResidue(t)
}

func TestPrepareRejectsWrongRuntimeAndExportPassphrasesBeforeRetention(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	if prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, []byte("wrong-runtime-passphrase")); prepared != nil || !errors.Is(err, secrets.ErrBadPassphrase) ||
		!strings.Contains(err.Error(), "runtime passphrase") {
		t.Fatalf("wrong runtime result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)
	if prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, []byte("wrong-export-passphrase"), testDestinationRuntimePassphrase); prepared != nil || !errors.Is(err, secrets.ErrBadPassphrase) ||
		!strings.Contains(err.Error(), "export passphrase") {
		t.Fatalf("wrong export result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)
}

func TestPrepareRejectsTamperedAndReplacedArtifacts(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	data, err := os.ReadFile(fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "tampered.oowrt-backup")
	data[len(data)-1] ^= 1
	if err := os.WriteFile(tampered, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if prepared, err := Prepare(context.Background(), tampered, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase); prepared != nil || err == nil {
		t.Fatalf("tampered artifact result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)

	replaced := filepath.Join(t.TempDir(), "replacement-link")
	if err := os.Symlink(fixture.artifact, replaced); err != nil {
		t.Fatal(err)
	}
	if prepared, err := Prepare(context.Background(), replaced, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase); prepared != nil || err == nil {
		t.Fatalf("symlink artifact result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)

	original := fixture.artifact + ".original"
	prepared, err := prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase,
		prepareOperations{afterRuntimeVerify: func() {
			if renameErr := os.Rename(fixture.artifact, original); renameErr != nil {
				t.Fatal(renameErr)
			}
			if linkErr := os.Symlink(original, fixture.artifact); linkErr != nil {
				t.Fatal(linkErr)
			}
		}})
	if removeErr := os.Remove(fixture.artifact); removeErr != nil {
		t.Fatal(removeErr)
	}
	if renameErr := os.Rename(original, fixture.artifact); renameErr != nil {
		t.Fatal(renameErr)
	}
	if prepared != nil || err == nil {
		t.Fatalf("post-verification replacement result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)
}

func TestPrepareCancellationAtEachMaterialPhaseCleansEverything(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	cases := []struct {
		name string
		set  func(*prepareOperations, context.CancelFunc)
	}{
		{"after runtime verify", func(ops *prepareOperations, cancel context.CancelFunc) { ops.afterRuntimeVerify = cancel }},
		{"after extract", func(ops *prepareOperations, cancel context.CancelFunc) { ops.afterExtract = cancel }},
		{"after migration", func(ops *prepareOperations, cancel context.CancelFunc) { ops.afterMigration = cancel }},
		{"after pair write", func(ops *prepareOperations, cancel context.CancelFunc) {
			ops.afterPairWritten = func(*scratchDirectory) { cancel() }
		}},
		{"after pair validation", func(ops *prepareOperations, cancel context.CancelFunc) { ops.afterPairValidated = cancel }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var ops prepareOperations
			tc.set(&ops, cancel)
			prepared, err := prepare(ctx, fixture.artifact, fixture.dataDir, fixture.live,
				testExportPassphrase, testDestinationRuntimePassphrase, ops)
			if prepared != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled result=(%v,%v)", prepared, err)
			}
			fixture.assertNoResidue(t)
		})
	}
}

func TestPrepareMigratesSupportedSchema18(t *testing.T) {
	fixture := newPrepareFixture(t, 18, func(t *testing.T, db *store.DB) {
		if _, err := db.SQL().Exec(`
DROP INDEX admins_username_nocase;
ALTER TABLE admins DROP COLUMN deleted_at;
ALTER TABLE admins DROP COLUMN enabled;
ALTER TABLE admins DROP COLUMN role;
UPDATE schema_version SET version=18 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
			t.Fatal(err)
		}
	})
	prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if preview := prepared.Preview(); preview.SourceSchema != 18 ||
		preview.TargetSchema != store.CurrentSchemaVersion() ||
		preview.Counts.Schema != store.CurrentSchemaVersion() {
		t.Fatalf("unexpected migrated preview: %+v", preview)
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatal(err)
	}
	fixture.assertNoResidue(t)
}

func TestPrepareRejectsInvalidGeneratedPair(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	prepared, err := prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase,
		prepareOperations{afterPairWritten: func(directory *scratchDirectory) {
			file, openErr := directory.root.OpenFile(secrets.FileName, os.O_WRONLY|os.O_TRUNC, 0o600)
			if openErr != nil {
				t.Fatal(openErr)
			}
			if _, writeErr := file.WriteString("invalid-keyring"); writeErr != nil {
				t.Fatal(writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}})
	if prepared != nil || err == nil {
		t.Fatalf("invalid generated pair result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)
}

func TestPreparedTransferFailureRetainsAndSuccessTransfersOwnership(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	failed, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	failedPreview := failed.Preview()
	callbackErr := errors.New("intent persistence failed")
	transferred, err := failed.Transfer(context.Background(), func(pair PreparedPair) error {
		if pair.Preview != failedPreview || filepath.Dir(pair.Directory) != fixture.dataDir {
			t.Fatalf("unexpected transfer pair: %+v", pair)
		}
		return callbackErr
	})
	if transferred || !errors.Is(err, callbackErr) {
		t.Fatalf("failed callback result=(%t,%v)", transferred, err)
	}
	failedPath := failed.dir.path
	if err := failed.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(failedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed-transfer pair remains after cleanup: %v", err)
	}

	succeeded, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(fixture.dataDir, "restore-intent.test")
	var callbacks atomic.Int32
	entered := make(chan struct{})
	allowReturn := make(chan struct{})
	transferResult := make(chan struct {
		transferred bool
		err         error
	}, 1)
	go func() {
		transferred, err := succeeded.Transfer(context.Background(), func(pair PreparedPair) error {
			callbacks.Add(1)
			payload := []byte(pair.DatabaseSHA256 + "\n" + pair.KeyringSHA256 + "\n")
			if err := os.WriteFile(markerPath, payload, 0o600); err != nil {
				return err
			}
			file, err := os.Open(markerPath)
			if err != nil {
				return err
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			directory, err := os.Open(fixture.dataDir)
			if err != nil {
				return err
			}
			syncErr := directory.Sync()
			closeErr := directory.Close()
			if syncErr != nil || closeErr != nil {
				return errors.New("marker directory sync failed")
			}
			close(entered)
			<-allowReturn
			return nil
		})
		transferResult <- struct {
			transferred bool
			err         error
		}{transferred, err}
	}()
	<-entered
	cleanupResult := make(chan error, 1)
	go func() { cleanupResult <- succeeded.Cleanup() }()
	close(allowReturn)
	result := <-transferResult
	if !result.transferred || result.err != nil {
		t.Fatalf("successful transfer result=(%t,%v)", result.transferred, result.err)
	}
	if err := <-cleanupResult; !errors.Is(err, ErrPreparedTransferred) {
		t.Fatalf("concurrent cleanup error=%v, want ErrPreparedTransferred", err)
	}
	if callbacks.Load() != 1 {
		t.Fatalf("callbacks=%d, want 1", callbacks.Load())
	}
	if transferred, err := succeeded.Transfer(context.Background(), func(PreparedPair) error {
		callbacks.Add(1)
		return nil
	}); !transferred || err != nil || callbacks.Load() != 1 {
		t.Fatalf("repeated Transfer=(%t,%v), callbacks=%d", transferred, err, callbacks.Load())
	}
	if _, err := os.Lstat(succeeded.pair.Directory); err != nil {
		t.Fatalf("transferred pair was removed: %v", err)
	}
	fixture.assertLiveUnchanged(t)
}

func TestPreparedRenameAndSymlinkReplacementAreNotFollowed(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	original := prepared.dir.path
	moved := original + ".moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, original); err != nil {
		t.Fatal(err)
	}
	called := false
	if transferred, err := prepared.Transfer(context.Background(), func(PreparedPair) error {
		called = true
		return nil
	}); transferred || err == nil || called {
		t.Fatalf("identity-changed Transfer=(%t,%v), called=%t", transferred, err, called)
	}
	if err := prepared.Cleanup(); err == nil {
		t.Fatal("identity-changed cleanup did not report the replacement")
	}
	info, err := os.Lstat(original)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cleanup followed or removed replacement symlink: info=%v err=%v", info, err)
	}
	if names := directoryNames(t, moved); len(names) != 0 {
		t.Fatalf("anchored moved directory still contains decrypted data: %v", names)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	fixture.assertNoResidue(t)
}

func TestPrepareRejectsSymlinkDataDirectory(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	link := filepath.Join(t.TempDir(), "data-link")
	if err := os.Symlink(fixture.dataDir, link); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(context.Background(), fixture.artifact, link, fixture.live,
		testExportPassphrase, testDestinationRuntimePassphrase)
	if prepared != nil || err == nil {
		t.Fatalf("symlink data directory result=(%v,%v)", prepared, err)
	}
	fixture.assertNoResidue(t)
}

func TestPrepareDetectsDataDirectoryRenameDuringStaging(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	moved := fixture.dataDir + ".moved"
	prepared, err := prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase,
		prepareOperations{afterExtract: func() {
			if renameErr := os.Rename(fixture.dataDir, moved); renameErr != nil {
				t.Fatal(renameErr)
			}
			if mkdirErr := os.Mkdir(fixture.dataDir, 0o700); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
		}})
	if prepared != nil || err == nil {
		t.Fatalf("renamed data-directory result=(%v,%v)", prepared, err)
	}
	if entries := directoryNames(t, fixture.dataDir); len(entries) != 0 {
		t.Fatalf("replacement data directory contains staged data: %v", entries)
	}
	if removeErr := os.Remove(fixture.dataDir); removeErr != nil {
		t.Fatal(removeErr)
	}
	if renameErr := os.Rename(moved, fixture.dataDir); renameErr != nil {
		t.Fatal(renameErr)
	}
	fixture.assertNoResidue(t)
}

func TestPreparedConcurrentCleanupIsIdempotent(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsSeen <- prepared.Cleanup()
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	fixture.assertNoResidue(t)
}

func TestPreparedCleanupRetriesTransientRemovalAndSyncFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*scratchDirectory, error)
	}{
		{
			name: "remove",
			inject: func(directory *scratchDirectory, injected error) {
				original := directory.ops.removeAll
				failed := false
				directory.ops.removeAll = func(root *os.Root, name string) error {
					if !failed {
						failed = true
						return injected
					}
					return original(root, name)
				}
			},
		},
		{
			name: "sync",
			inject: func(directory *scratchDirectory, injected error) {
				original := directory.ops.sync
				failed := false
				directory.ops.sync = func(root *os.Root) error {
					if !failed {
						failed = true
						return injected
					}
					return original(root)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
			prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
				fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected cleanup failure")
			tc.inject(prepared.dir, injected)
			if err := prepared.Cleanup(); !errors.Is(err, injected) {
				t.Fatalf("first Cleanup error=%v, want injected failure", err)
			}
			if prepared.state != preparedActive || prepared.dir.cleaned {
				t.Fatal("transient cleanup failure discarded ownership")
			}
			if err := prepared.Cleanup(); err != nil {
				t.Fatalf("retried Cleanup: %v", err)
			}
			fixture.assertNoResidue(t)
		})
	}
}

func TestPreparedTransferHonorsCanceledContextBeforeCallback(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	transferred, err := prepared.Transfer(ctx, func(PreparedPair) error {
		called = true
		return nil
	})
	if transferred || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled Transfer=(%t,%v), callback called=%t", transferred, err, called)
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatal(err)
	}
	fixture.assertNoResidue(t)
}
