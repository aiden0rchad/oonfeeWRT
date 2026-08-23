package restoreswap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/portablebackup"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var (
	testRuntime = []byte("placeholder-runtime-value")
	testExport  = []byte("placeholder-export-value")
	testParams  = secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1}
)

type restoreFixture struct {
	dataDir     string
	preparedDir string
	oldDB       *store.DB
	oldKeeper   *secrets.Keeper
	intent      IntentResult
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	mustMkdir(t, dataDir, 0o700)
	oldDB, oldKeeper := createPair(t, dataDir, databaseName, keyringName, "old", false)
	preparedDir := filepath.Join(dataDir, "prepared")
	mustMkdir(t, preparedDir, 0o700)
	preparedDB, preparedKeeper := createPair(t, preparedDir, "controller.db", "prepared-keyring.json", "new", true)
	closePair(t, preparedDB, preparedKeeper, filepath.Join(preparedDir, "controller.db"))

	intent, err := CreateIntent(context.Background(), dataDir, PreparedPair{
		DatabasePath:       filepath.Join(preparedDir, "controller.db"),
		KeyringPath:        filepath.Join(preparedDir, "prepared-keyring.json"),
		AuthorizingAdminID: 1, AuthorizingUsername: "restore-owner", PreviewID: "preview-fixture", PlanID: "plan-fixture",
	}, oldKeeper, testExport, "daemon-instance-1")
	if err != nil {
		t.Fatalf("CreateIntent: %v", err)
	}
	return &restoreFixture{dataDir: dataDir, preparedDir: preparedDir,
		oldDB: oldDB, oldKeeper: oldKeeper, intent: intent}
}

func (f *restoreFixture) markReady(t *testing.T) IntentResult {
	t.Helper()
	closePair(t, f.oldDB, f.oldKeeper, filepath.Join(f.dataDir, databaseName))
	result, err := MarkCleanShutdown(context.Background(), f.dataDir, "daemon-instance-1")
	if err != nil {
		t.Fatalf("MarkCleanShutdown: %v", err)
	}
	f.oldDB, f.oldKeeper = nil, nil
	return result
}

func createPair(t *testing.T, dir, dbName, keyName, suffix string, extraAccount bool) (*store.DB, *secrets.Keeper) {
	t.Helper()
	keeper, err := secrets.Create(filepath.Join(dir, keyName), testRuntime, testParams)
	if err != nil {
		t.Fatalf("create %s keeper: %v", suffix, err)
	}
	db, err := store.Open(context.Background(), "sqlite", filepath.Join(dir, dbName), keeper)
	if err != nil {
		keeper.Close()
		t.Fatalf("create %s database: %v", suffix, err)
	}
	hash, err := secrets.HashPassword([]byte("owner password"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := db.CreateFirstAdmin(context.Background(), "owner-"+suffix, hash)
	if err != nil {
		t.Fatalf("create %s owner: %v", suffix, err)
	}
	if _, err := db.Site(context.Background()); err != nil {
		t.Fatalf("create %s site: %v", suffix, err)
	}
	if extraAccount {
		viewerHash, err := secrets.HashPassword([]byte("viewer password"), testParams)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateAdmin(context.Background(), "viewer-new", viewerHash,
			store.RoleViewer, store.AccountActor{AdminID: owner.ID, Username: owner.Username}); err != nil {
			t.Fatalf("create prepared viewer: %v", err)
		}
	}
	return db, keeper
}

func closePair(t *testing.T, db *store.DB, keeper *secrets.Keeper, databasePath string) {
	t.Helper()
	if err := db.Checkpoint(context.Background()); err != nil {
		t.Fatalf("checkpoint pair: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pair database: %v", err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatalf("close pair keeper: %v", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatalf("protect database: %v", err)
	}
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestApplyPendingRoundTripAndSuppressionLifecycle(t *testing.T) {
	fixture := newRestoreFixture(t)
	ready := fixture.markReady(t)
	result, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime, "v0.1.0-test")
	if err != nil {
		t.Fatalf("ApplyPending: %v", err)
	}
	if !result.Applied || result.RestoreID != ready.ID || result.SafetyBackup == "" ||
		result.PreparedDatabaseSHA256 != ready.PreparedDatabaseSHA256 ||
		result.PreparedKeyringSHA256 != ready.PreparedKeyringSHA256 ||
		result.AuthorizingAdminID != 1 || result.AuthorizingUsername != "restore-owner" ||
		result.PreviewID != "preview-fixture" || result.PlanID != "plan-fixture" ||
		result.Counts.Schema != store.CurrentSchemaVersion() {
		t.Fatalf("unexpected apply result: %+v", result)
	}
	assertRecord(t, filepath.Join(fixture.dataDir, databaseName),
		fileRecord{SHA256: ready.PreparedDatabaseSHA256, Size: fileSize(t, filepath.Join(fixture.dataDir, databaseName))})
	if _, err := os.Stat(filepath.Join(fixture.dataDir, recoveryDirName, markerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending marker survived success: %v", err)
	}
	if _, err := os.Stat(fixture.preparedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared directory survived success: %v", err)
	}
	safetyPath := filepath.Join(fixture.dataDir, recoveryDirName, result.SafetyBackup)
	assertMode(t, safetyPath, 0o600)
	stage, err := portablebackup.Extract(context.Background(), safetyPath,
		filepath.Join(fixture.dataDir, recoveryDirName), testExport)
	if err != nil {
		t.Fatalf("extract retained safety artifact: %v", err)
	}
	if stage.Manifest.SchemaVersion != store.CurrentSchemaVersion() {
		t.Fatalf("safety schema=%d", stage.Manifest.SchemaVersion)
	}
	if err := stage.Cleanup(); err != nil {
		t.Fatal(err)
	}
	oldSafetyIDs := []string{
		"22222222222222222222222222222222", "33333333333333333333333333333333",
		"44444444444444444444444444444444", "55555555555555555555555555555555",
	}
	for index, id := range oldSafetyIDs {
		path := filepath.Join(fixture.dataDir, recoveryDirName, "safety-"+id+".oowrtbak")
		if err := os.WriteFile(path, []byte("bounded-old-safety"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(1000+index), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	notePath := filepath.Join(fixture.dataDir, recoveryDirName, "operator-note")
	if err := os.WriteFile(notePath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := PendingAppliedReceipt(fixture.dataDir)
	if err != nil || receipt.RestoreID != result.RestoreID || receipt.AuthorizingAdminID != 1 ||
		receipt.AuthorizingUsername != "restore-owner" || receipt.PreviewID != "preview-fixture" ||
		receipt.PlanID != "plan-fixture" {
		t.Fatalf("applied receipt=%+v err=%v", receipt, err)
	}
	if err := ClearAppliedReceipt(context.Background(), fixture.dataDir,
		"ffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatal("wrong restore ID cleared applied receipt")
	}
	if err := ClearAppliedReceipt(context.Background(), fixture.dataDir, result.RestoreID); err != nil {
		t.Fatalf("ClearAppliedReceipt: %v", err)
	}
	if _, err := PendingAppliedReceipt(fixture.dataDir); !errors.Is(err, ErrNoAppliedReceipt) {
		t.Fatalf("applied receipt survived acknowledgement: %v", err)
	}
	for _, id := range oldSafetyIDs[:2] {
		if _, err := os.Stat(filepath.Join(fixture.dataDir, recoveryDirName,
			"safety-"+id+".oowrtbak")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired safety artifact %s survived: %v", id, err)
		}
	}
	for _, id := range oldSafetyIDs[2:] {
		if _, err := os.Stat(filepath.Join(fixture.dataDir, recoveryDirName,
			"safety-"+id+".oowrtbak")); err != nil {
			t.Fatalf("newest retained safety artifact %s: %v", id, err)
		}
	}
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("retention removed unrelated recovery member: %v", err)
	}
	status, err := SuppressionStatus(fixture.dataDir)
	if err != nil || !status.Active || status.RestoreID != result.RestoreID || status.Reason != suppressionReason {
		t.Fatalf("suppression status=%+v err=%v", status, err)
	}
	if err := ClearSuppression(context.Background(), fixture.dataDir,
		"ffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatal("wrong restore ID cleared suppression")
	}
	if err := ClearSuppression(context.Background(), fixture.dataDir, result.RestoreID); err != nil {
		t.Fatalf("ClearSuppression: %v", err)
	}
	status, err = SuppressionStatus(fixture.dataDir)
	if err != nil || status.Active {
		t.Fatalf("suppression survived clear: %+v %v", status, err)
	}
}

func TestUncleanIntentRequiresSameInstancePromotionAndCanAbort(t *testing.T) {
	fixture := newRestoreFixture(t)
	if _, err := MarkCleanShutdown(context.Background(), fixture.dataDir,
		"restarted-daemon-instance"); !errors.Is(err, ErrUncleanIntent) {
		t.Fatalf("wrong-instance promotion=%v", err)
	}
	if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
		"v0.1.0-test"); !errors.Is(err, ErrUncleanIntent) {
		t.Fatalf("unclean apply=%v", err)
	}
	if err := AbortUnclean(context.Background(), fixture.dataDir); err != nil {
		t.Fatalf("AbortUnclean: %v", err)
	}
	if _, err := os.Stat(fixture.preparedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared directory survived abort: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.dataDir, databaseName)); err != nil {
		t.Fatalf("abort touched canonical database: %v", err)
	}
	fixture.oldDB.Close()
	fixture.oldKeeper.Close()
}

func TestMarkCleanShutdownRejectsSQLiteSidecars(t *testing.T) {
	fixture := newRestoreFixture(t)
	closePair(t, fixture.oldDB, fixture.oldKeeper, filepath.Join(fixture.dataDir, databaseName))
	fixture.oldDB, fixture.oldKeeper = nil, nil
	sidecar := filepath.Join(fixture.dataDir, databaseName+"-wal")
	if err := os.WriteFile(sidecar, []byte("committed WAL data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MarkCleanShutdown(context.Background(), fixture.dataDir,
		"daemon-instance-1"); err == nil {
		t.Fatal("nonempty WAL sidecar was accepted")
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if _, err := MarkCleanShutdown(context.Background(), fixture.dataDir,
		"daemon-instance-1"); err != nil {
		t.Fatalf("promotion after sidecar removal: %v", err)
	}
}

func TestWrongRuntimePassphraseAndPreparedTamperNeverSwap(t *testing.T) {
	fixture := newRestoreFixture(t)
	ready := fixture.markReady(t)
	oldDigest := shaFile(t, filepath.Join(fixture.dataDir, databaseName))
	if _, err := ApplyPending(context.Background(), fixture.dataDir, []byte("wrong runtime passphrase"),
		"v0.1.0-test"); err == nil {
		t.Fatal("wrong runtime passphrase was accepted")
	}
	if got := shaFile(t, filepath.Join(fixture.dataDir, databaseName)); got != oldDigest {
		t.Fatal("wrong passphrase changed canonical database")
	}
	preparedPath := filepath.Join(fixture.preparedDir, "controller.db")
	file, err := os.OpenFile(preparedPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("tamper"))
	_ = file.Close()
	if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
		"v0.1.0-test"); err == nil {
		t.Fatal("tampered prepared database was accepted")
	}
	if got := shaFile(t, filepath.Join(fixture.dataDir, databaseName)); got != oldDigest {
		t.Fatal("prepared tamper changed canonical database")
	}
	if ready.PreparedDatabaseSHA256 == shaFile(t, preparedPath) {
		t.Fatal("test did not alter prepared database")
	}
}

func TestCancellationAfterFirstRenameRestoresOldPair(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.markReady(t)
	oldDatabase := shaFile(t, filepath.Join(fixture.dataDir, databaseName))
	oldKeyring := shaFile(t, filepath.Join(fixture.dataDir, keyringName))
	ctx, cancel := context.WithCancel(context.Background())
	ops := defaultOperations()
	ops.boundary = func(name string) error {
		if name == "old-database-renamed" {
			cancel()
		}
		return nil
	}
	if _, err := applyPending(ctx, fixture.dataDir, testRuntime, "v0.1.0-test", ops); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled apply=%v", err)
	}
	if got := shaFile(t, filepath.Join(fixture.dataDir, databaseName)); got != oldDatabase {
		t.Fatal("cancellation did not restore old database")
	}
	if got := shaFile(t, filepath.Join(fixture.dataDir, keyringName)); got != oldKeyring {
		t.Fatal("cancellation did not restore old keyring")
	}
	status, err := SuppressionStatus(fixture.dataDir)
	if err != nil || status.Active {
		t.Fatalf("canceled apply left suppression: %+v %v", status, err)
	}
}

func TestConcurrentApplyPublishesExactlyOnce(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.markReady(t)
	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			_, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime, "v0.1.0-test")
			results <- err
		}()
	}
	start.Done()
	var success, absent int
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrNoPendingIntent) {
			absent++
		} else {
			t.Fatalf("unexpected concurrent apply error: %v", err)
		}
	}
	if success != 1 || absent != 1 {
		t.Fatalf("concurrent results success=%d absent=%d", success, absent)
	}
}

func TestCrashRecoveryAtEverySwapBoundary(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.markReady(t)
	baseOps := defaultOperations()
	baseOps.boundary = func(name string) error {
		if name == "safety-state-published" {
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
		"v0.1.0-test", baseOps); err == nil {
		t.Fatal("safety-state failure injection did not fire")
	}

	boundaries := []string{
		"old-database-renamed",
		"old_db_parked-state-published",
		"old-keyring-renamed",
		"old_pair_parked-state-published",
		"new-database-renamed",
		"new_db_published-state-published",
		"new-keyring-renamed",
		"new_pair_published-state-published",
		"validated-state-published",
		"suppression-published",
		"suppressed-state-published",
		"cleanup-state-published",
		"applied-receipt-published",
	}
	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			copyTree(t, fixture.dataDir, dataDir)
			seen := false
			ops := defaultOperations()
			ops.boundary = func(name string) error {
				if name == boundary {
					seen = true
					return errors.New("simulated crash")
				}
				return nil
			}
			if _, err := applyPending(context.Background(), dataDir, testRuntime,
				"v0.1.0-test", ops); err == nil {
				t.Fatal("failure injection did not stop apply")
			}
			if !seen {
				t.Fatalf("boundary %q was not reached", boundary)
			}
			result, err := ApplyPending(context.Background(), dataDir, testRuntime, "v0.1.0-test")
			if err != nil {
				if _, statErr := os.Stat(filepath.Join(dataDir, "prepared")); statErr == nil {
					t.Logf("prepared entries after failed resume: %v", directoryEntries(t, filepath.Join(dataDir, "prepared")))
				}
				t.Logf("data entries after failed resume: %v", directoryEntries(t, dataDir))
				t.Logf("recovery entries after failed resume: %v", directoryEntries(t, filepath.Join(dataDir, recoveryDirName)))
				t.Fatalf("resume after %s: %v", boundary, err)
			}
			if !result.Applied {
				t.Fatal("resumed apply did not complete")
			}
			if got := shaFile(t, filepath.Join(dataDir, databaseName)); got != result.PreparedDatabaseSHA256 {
				t.Fatal("resumed database digest mismatch")
			}
			if got := shaFile(t, filepath.Join(dataDir, keyringName)); got != result.PreparedKeyringSHA256 {
				t.Fatal("resumed keyring digest mismatch")
			}
			if err := ClearAppliedReceipt(context.Background(), dataDir, result.RestoreID); err != nil {
				t.Fatalf("clear applied receipt: %v", err)
			}
			if got := directoryEntries(t, filepath.Join(dataDir, recoveryDirName)); len(got) != 1 || got[0] != result.SafetyBackup {
				t.Fatalf("recovery residue=%v", got)
			}
		})
	}
}

func TestCrashAfterSafetyArtifactPublicationResumes(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.markReady(t)
	seen := false
	ops := defaultOperations()
	ops.boundary = func(name string) error {
		if name == "safety-artifact-published" {
			seen = true
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
		"v0.1.0-test", ops); err == nil || !seen {
		t.Fatalf("safety publication injection err=%v seen=%v", err, seen)
	}
	result, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime, "v0.1.0-test")
	if err != nil || !result.Applied {
		t.Fatalf("resume unrecorded safety artifact: %+v %v", result, err)
	}
}

func TestIntentAndReadyPublicationFailuresRemainRecoverable(t *testing.T) {
	t.Run("publish reports failure after durable link", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		mustMkdir(t, dataDir, 0o700)
		oldDB, oldKeeper := createPair(t, dataDir, databaseName, keyringName, "old", false)
		preparedDir := filepath.Join(dataDir, "prepared")
		mustMkdir(t, preparedDir, 0o700)
		preparedDB, preparedKeeper := createPair(t, preparedDir, "controller.db",
			"prepared-keyring.json", "new", true)
		closePair(t, preparedDB, preparedKeeper, filepath.Join(preparedDir, "controller.db"))
		ops := defaultOperations()
		ops.publishIntent = func(root *os.Root, value marker) error {
			if err := writeMarkerNew(root, value); err != nil {
				return err
			}
			return errors.New("simulated post-link sync failure")
		}
		result, err := createIntent(context.Background(), dataDir, PreparedPair{
			DatabasePath:       filepath.Join(preparedDir, "controller.db"),
			KeyringPath:        filepath.Join(preparedDir, "prepared-keyring.json"),
			AuthorizingAdminID: 1, AuthorizingUsername: "restore-owner", PreviewID: "preview-fixture", PlanID: "plan-fixture",
		}, oldKeeper, testExport, "daemon-instance-1", ops)
		if err == nil || result.ID != "" || IntentOwnershipRetained(err) {
			t.Fatalf("publication rollback result=%+v err=%v", result, err)
		}
		if _, err := ApplyPending(context.Background(), dataDir, testRuntime,
			"v0.1.0-test"); !errors.Is(err, ErrNoPendingIntent) {
			t.Fatalf("rolled-back marker remained pending: %v", err)
		}
		if _, err := os.Stat(preparedDir); err != nil {
			t.Fatalf("caller-owned prepared pair was removed: %v", err)
		}
		oldDB.Close()
		oldKeeper.Close()
	})
	t.Run("intent", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		mustMkdir(t, dataDir, 0o700)
		oldDB, oldKeeper := createPair(t, dataDir, databaseName, keyringName, "old", false)
		preparedDir := filepath.Join(dataDir, "prepared")
		mustMkdir(t, preparedDir, 0o700)
		preparedDB, preparedKeeper := createPair(t, preparedDir, "controller.db",
			"prepared-keyring.json", "new", true)
		closePair(t, preparedDB, preparedKeeper, filepath.Join(preparedDir, "controller.db"))
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name == "intent-published" {
				return errors.New("simulated crash")
			}
			return nil
		}
		if _, err := createIntent(context.Background(), dataDir, PreparedPair{
			DatabasePath:       filepath.Join(preparedDir, "controller.db"),
			KeyringPath:        filepath.Join(preparedDir, "prepared-keyring.json"),
			AuthorizingAdminID: 1, AuthorizingUsername: "restore-owner", PreviewID: "preview-fixture", PlanID: "plan-fixture",
		}, oldKeeper, testExport, "daemon-instance-1", ops); err == nil {
			t.Fatal("intent publication injection did not fire")
		}
		if _, err := ApplyPending(context.Background(), dataDir, testRuntime,
			"v0.1.0-test"); !errors.Is(err, ErrNoPendingIntent) {
			t.Fatalf("rolled-back intent remained pending: %v", err)
		}
		if _, err := os.Stat(preparedDir); err != nil {
			t.Fatalf("callback-owned prepared pair was removed: %v", err)
		}
		oldDB.Close()
		oldKeeper.Close()
	})
	t.Run("intent rollback failure retains ownership", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		mustMkdir(t, dataDir, 0o700)
		oldDB, oldKeeper := createPair(t, dataDir, databaseName, keyringName, "old", false)
		preparedDir := filepath.Join(dataDir, "prepared")
		mustMkdir(t, preparedDir, 0o700)
		preparedDB, preparedKeeper := createPair(t, preparedDir, "controller.db",
			"prepared-keyring.json", "new", true)
		closePair(t, preparedDB, preparedKeeper, filepath.Join(preparedDir, "controller.db"))
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name == "intent-published" {
				return errors.New("simulated post-publication failure")
			}
			return nil
		}
		ops.rollbackIntent = func(*os.Root) error { return errors.New("simulated rollback failure") }
		result, err := createIntent(context.Background(), dataDir, PreparedPair{
			DatabasePath:       filepath.Join(preparedDir, "controller.db"),
			KeyringPath:        filepath.Join(preparedDir, "prepared-keyring.json"),
			AuthorizingAdminID: 1, AuthorizingUsername: "restore-owner", PreviewID: "preview-fixture", PlanID: "plan-fixture",
		}, oldKeeper, testExport, "daemon-instance-1", ops)
		if result.ID == "" || !IntentOwnershipRetained(err) {
			t.Fatalf("retained result=%+v err=%v", result, err)
		}
		if abortErr := AbortUnclean(context.Background(), dataDir); abortErr != nil {
			t.Fatalf("retained intent was not recoverable: %v", abortErr)
		}
		oldDB.Close()
		oldKeeper.Close()
	})
	t.Run("ready", func(t *testing.T) {
		fixture := newRestoreFixture(t)
		closePair(t, fixture.oldDB, fixture.oldKeeper, filepath.Join(fixture.dataDir, databaseName))
		fixture.oldDB, fixture.oldKeeper = nil, nil
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name == "ready-published" {
				return errors.New("simulated crash")
			}
			return nil
		}
		if _, err := markCleanShutdown(context.Background(), fixture.dataDir,
			"daemon-instance-1", ops); err == nil {
			t.Fatal("ready publication injection did not fire")
		}
		if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test"); err != nil {
			t.Fatalf("apply after ready publication crash: %v", err)
		}
	})
}

func TestSafetyAndPartialSwapTamperFailClosed(t *testing.T) {
	t.Run("safety artifact", func(t *testing.T) {
		fixture := newRestoreFixture(t)
		fixture.markReady(t)
		oldDatabase := shaFile(t, filepath.Join(fixture.dataDir, databaseName))
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name == "safety-state-published" {
				return errors.New("simulated crash")
			}
			return nil
		}
		if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test", ops); err == nil {
			t.Fatal("safety-state injection did not fire")
		}
		root, _, err := openDataRoot(fixture.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		m, err := readMarker(root)
		root.Close()
		if err != nil {
			t.Fatal(err)
		}
		safety := filepath.Join(fixture.dataDir, recoveryDirName, m.safetyName())
		file, err := os.OpenFile(safety, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte("tamper"))
		_ = file.Close()
		if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test"); err == nil {
			t.Fatal("tampered safety artifact was accepted")
		}
		if got := shaFile(t, filepath.Join(fixture.dataDir, databaseName)); got != oldDatabase {
			t.Fatal("safety tamper changed canonical database")
		}
	})

	t.Run("prepared key after partial swap", func(t *testing.T) {
		fixture := newRestoreFixture(t)
		fixture.markReady(t)
		oldDatabase := shaFile(t, filepath.Join(fixture.dataDir, databaseName))
		oldKeyring := shaFile(t, filepath.Join(fixture.dataDir, keyringName))
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name == "new-database-renamed" {
				return errors.New("simulated crash")
			}
			return nil
		}
		if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test", ops); err == nil {
			t.Fatal("partial-swap injection did not fire")
		}
		keyPath := filepath.Join(fixture.preparedDir, "prepared-keyring.json")
		file, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte("tamper"))
		_ = file.Close()
		if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test"); err == nil {
			t.Fatal("tampered partial swap resumed")
		}
		if got := shaFile(t, filepath.Join(fixture.dataDir, databaseName)); got != oldDatabase {
			t.Fatal("failed partial swap did not restore old database")
		}
		if got := shaFile(t, filepath.Join(fixture.dataDir, keyringName)); got != oldKeyring {
			t.Fatal("failed partial swap did not restore old keyring")
		}
		root, _, err := openDataRoot(fixture.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		m, err := readMarker(root)
		root.Close()
		if err != nil || m.State != stateSafety {
			t.Fatalf("rollback marker state=%s err=%v", m.State, err)
		}
	})
}

func TestCleanupRefusesMissingSafetyArtifact(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.markReady(t)
	ops := defaultOperations()
	ops.boundary = func(name string) error {
		if name == "cleanup-state-published" {
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
		"v0.1.0-test", ops); err == nil {
		t.Fatal("cleanup-state injection did not fire")
	}
	root, _, err := openDataRoot(fixture.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := readMarker(root)
	root.Close()
	if err != nil || m.State != stateCleanup {
		t.Fatalf("cleanup marker=%s err=%v", m.State, err)
	}
	if err := os.Remove(filepath.Join(fixture.dataDir, recoveryDirName, m.safetyName())); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
		"v0.1.0-test"); err == nil {
		t.Fatal("cleanup succeeded without retained safety artifact")
	}
	if _, err := os.Stat(filepath.Join(fixture.dataDir, recoveryDirName, markerName)); err != nil {
		t.Fatalf("cleanup removed marker after safety loss: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.dataDir, recoveryDirName, m.rollbackName())); err != nil {
		t.Fatalf("cleanup removed raw rollback after safety loss: %v", err)
	}
}

func TestNamedDataDirectoryReplacementFailsClosed(t *testing.T) {
	t.Run("during safety publication", func(t *testing.T) {
		fixture := newRestoreFixture(t)
		fixture.markReady(t)
		original := fixture.dataDir + ".original"
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name != "safety-artifact-published" {
				return nil
			}
			if err := os.Rename(fixture.dataDir, original); err != nil {
				return err
			}
			if err := os.Mkdir(fixture.dataDir, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(fixture.dataDir, "replacement-canary"), []byte("replacement"), 0o600)
		}
		if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test", ops); err == nil {
			t.Fatal("named data-directory replacement was accepted")
		}
		if got := directoryEntries(t, fixture.dataDir); len(got) != 1 || got[0] != "replacement-canary" {
			t.Fatalf("replacement directory was modified: %v", got)
		}
		if err := os.RemoveAll(fixture.dataDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, fixture.dataDir); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test"); err != nil {
			t.Fatalf("resume after restoring data-directory name: %v", err)
		}
	})

	t.Run("before final validation", func(t *testing.T) {
		fixture := newRestoreFixture(t)
		fixture.markReady(t)
		original := fixture.dataDir + ".original"
		ops := defaultOperations()
		ops.boundary = func(name string) error {
			if name != "new-keyring-renamed" {
				return nil
			}
			if err := os.Rename(fixture.dataDir, original); err != nil {
				return err
			}
			if err := os.Mkdir(fixture.dataDir, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(fixture.dataDir, "replacement-canary"), []byte("replacement"), 0o600)
		}
		if _, err := applyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test", ops); err == nil {
			t.Fatal("replacement directory passed final validation")
		}
		if got := directoryEntries(t, fixture.dataDir); len(got) != 1 || got[0] != "replacement-canary" {
			t.Fatalf("replacement directory was modified: %v", got)
		}
		if _, err := os.Stat(filepath.Join(fixture.dataDir, suppressionName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("replacement received suppression marker: %v", err)
		}
		if err := os.RemoveAll(fixture.dataDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, fixture.dataDir); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test"); err != nil {
			t.Fatalf("resume after validation retarget: %v", err)
		}
	})
}

func TestMarkerNoClobberSecrecyModesAndSymlinkDefense(t *testing.T) {
	fixture := newRestoreFixture(t)
	markerPath := filepath.Join(fixture.dataDir, recoveryDirName, markerName)
	assertMode(t, markerPath, 0o600)
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(markerData, testExport) {
		t.Fatal("marker retained plaintext export passphrase")
	}
	if _, err := CreateIntent(context.Background(), fixture.dataDir, PreparedPair{
		DatabasePath:       filepath.Join(fixture.preparedDir, "controller.db"),
		KeyringPath:        filepath.Join(fixture.preparedDir, "prepared-keyring.json"),
		AuthorizingAdminID: 1, AuthorizingUsername: "restore-owner", PreviewID: "preview-fixture", PlanID: "plan-fixture",
	}, fixture.oldKeeper, testExport, "daemon-instance-1"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second intent clobber error=%v", err)
	}
	canonicalDigest := shaFile(t, filepath.Join(fixture.dataDir, databaseName))
	preparedDatabase := filepath.Join(fixture.preparedDir, "controller.db")
	if err := os.Remove(preparedDatabase); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.dataDir, databaseName), preparedDatabase); err != nil {
		t.Fatal(err)
	}
	if err := AbortUnclean(context.Background(), fixture.dataDir); err == nil {
		t.Fatal("abort followed a prepared-member symlink")
	}
	if got := shaFile(t, filepath.Join(fixture.dataDir, databaseName)); got != canonicalDigest {
		t.Fatal("symlinked abort changed canonical database")
	}
	fixture.oldDB.Close()
	fixture.oldKeeper.Close()
}

func TestPrivateDirectoryAndMarkerValidation(t *testing.T) {
	t.Run("data directory mode", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		mustMkdir(t, dataDir, 0o755)
		if _, err := SuppressionStatus(dataDir); err == nil {
			t.Fatal("world-readable data directory was accepted")
		}
	})
	t.Run("recovery directory symlink", func(t *testing.T) {
		parent := t.TempDir()
		dataDir := filepath.Join(parent, "data")
		target := filepath.Join(parent, "target")
		mustMkdir(t, dataDir, 0o700)
		mustMkdir(t, target, 0o700)
		if err := os.Symlink(target, filepath.Join(dataDir, recoveryDirName)); err != nil {
			t.Fatal(err)
		}
		root, _, err := openDataRoot(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if err := ensureRecoveryDir(root); err == nil {
			t.Fatal("symlinked recovery directory was accepted")
		}
	})
	t.Run("unknown marker field", func(t *testing.T) {
		fixture := newRestoreFixture(t)
		markerPath := filepath.Join(fixture.dataDir, recoveryDirName, markerName)
		data, err := os.ReadFile(markerPath)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data[:len(data)-1], []byte(`,"unexpected":true}`)...)
		if err := os.WriteFile(markerPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyPending(context.Background(), fixture.dataDir, testRuntime,
			"v0.1.0-test"); err == nil {
			t.Fatal("marker with unknown field was accepted")
		}
		fixture.oldDB.Close()
		fixture.oldKeeper.Close()
	})
}

func assertMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		t.Fatalf("%s mode=%v err=%v, want %v", path, infoMode(info), err, mode)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func fileSize(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(info.Size())
}

func shaFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func assertRecord(t *testing.T, path string, want fileRecord) {
	t.Helper()
	if got := shaFile(t, path); got != want.SHA256 || fileSize(t, path) != want.Size {
		t.Fatalf("%s did not match expected record", path)
	}
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return errors.New("unexpected non-regular fixture member")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err == nil {
			_, err = io.Copy(output, input)
		}
		closeErr := errors.Join(input.Close(), func() error {
			if output != nil {
				return output.Close()
			}
			return nil
		}())
		return errors.Join(err, closeErr)
	}); err != nil {
		t.Fatalf("copy fixture tree: %v", err)
	}
}

func directoryEntries(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
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
