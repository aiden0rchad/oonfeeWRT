package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/restoreswap"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var restoreFixtureParams = secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1}

func makePreparedRestore(t *testing.T, dataDir, runtimeValue string) restoreswap.PreparedPair {
	t.Helper()
	dir := filepath.Join(dataDir, ".oonfeewrt-prepared-pair-0123456789abcdef0123456789abcdef.stage")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, secrets.FileName)
	keeper, err := secrets.Create(keyPath, []byte(runtimeValue), restoreFixtureParams)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "controller.db")
	db, err := store.Open(context.Background(), driverName, dbPath, keeper)
	if err != nil {
		keeper.Close()
		t.Fatal(err)
	}
	hash, err := secrets.HashPassword([]byte("fixture-owner-value-4Lm"), restoreFixtureParams)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFirstAdmin(context.Background(), "restored-owner", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Site(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		t.Fatal(err)
	}
	return restoreswap.PreparedPair{DatabasePath: dbPath, KeyringPath: keyPath,
		AuthorizingAdminID: 1, AuthorizingUsername: "restore-owner",
		PreviewID: "preview-fixture", PlanID: "plan-fixture"}
}

func createDaemonRestoreIntent(t *testing.T, process *Process, runtimeValue string) (*Daemon, restoreswap.IntentResult) {
	t.Helper()
	d, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := secrets.HashPassword([]byte("fixture-original-owner-2Kr"), restoreFixtureParams)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	if _, err := d.Store.CreateFirstAdmin(context.Background(), "original-owner", hash); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if _, err := d.Store.Site(context.Background()); err != nil {
		d.Close()
		t.Fatal(err)
	}
	prepared := makePreparedRestore(t, d.Config.DataDir, runtimeValue)
	result, err := restoreswap.CreateIntent(context.Background(), d.Config.DataDir, prepared,
		d.Keys, []byte("fixture-export-value-7Qm9"), d.restoreOwnerInstanceID)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	return d, result
}

func applyDaemonRestoreWithoutAudit(t *testing.T, process *Process,
	runtimeValue string) (restoreswap.IntentResult, restoreswap.Result) {
	t.Helper()
	d, intent := createDaemonRestoreIntent(t, process, runtimeValue)
	d.RequestRestoreRestart()
	if err := d.shutdownForRestore(); err != nil {
		t.Fatal(err)
	}
	result, err := restoreswap.ApplyPending(context.Background(), d.Config.DataDir,
		[]byte(runtimeValue), d.Config.Version)
	if err != nil {
		t.Fatal(err)
	}
	return intent, result
}

func openRestoredPair(t *testing.T, cfg Config, runtimeValue string) (*store.DB, *secrets.Keeper) {
	t.Helper()
	keeper, err := secrets.Open(secrets.DefaultPath(cfg.DataDir), []byte(runtimeValue))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), driverName, cfg.DBPath(), keeper)
	if err != nil {
		keeper.Close()
		t.Fatal(err)
	}
	return db, keeper
}

func closeRestoredPair(t *testing.T, cfg Config, db *store.DB, keeper *secrets.Keeper) {
	t.Helper()
	if err := db.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.DBPath(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProcessAcquiresRuntimeValueOnceAndClearsIt(t *testing.T) {
	cfg := testConfig(t, "unused-file-value")
	process := NewProcess(cfg, quietLogger())
	var calls atomic.Int32
	process.acquire = func(bool) ([]byte, error) {
		calls.Add(1)
		return []byte("fixture-runtime-value-8Ks"), nil
	}
	for range 2 {
		d, err := process.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("runtime value acquired %d times", calls.Load())
	}
	retained := process.pass
	if len(retained) == 0 {
		t.Fatal("process retained no runtime value")
	}
	process.Close()
	if !bytes.Equal(retained, make([]byte, len(retained))) || process.pass != nil {
		t.Fatal("process close did not clear its retained runtime value")
	}
}

func TestProcessClearsRejectedRuntimeValue(t *testing.T) {
	process := NewProcess(Config{}, quietLogger())
	value := []byte("fixture-rejected-runtime-value-9Ht")
	process.acquire = func(bool) ([]byte, error) {
		return value, errors.New("injected acquisition failure")
	}
	if _, err := process.runtimePassphrase(false); err == nil {
		t.Fatal("runtime acquisition failure was accepted")
	}
	if !bytes.Equal(value, make([]byte, len(value))) || process.pass != nil {
		t.Fatal("failed runtime acquisition retained secret bytes")
	}
}

func TestRestoreRestartRequestIsBufferedNonblockingAndIdempotent(t *testing.T) {
	d := &Daemon{restoreRestart: make(chan RestartRequest, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			d.RequestRestoreRestart()
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restart request blocked without a receiver")
	}
	if len(d.restoreRestart) != 1 {
		t.Fatalf("buffered restart requests=%d, want 1", len(d.restoreRestart))
	}
	if !d.restoreRestartAccepted.Load() {
		t.Fatal("queued accepted restart did not set its promotion flag")
	}
	if request := <-d.restoreRestart; request.Kind != RestartControllerRestore {
		t.Fatalf("restart request=%+v", request)
	}
}

func TestUncleanIntentIsAbortedBeforeStoreOpen(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-2Np"
	cfg := testConfig(t, runtimeValue)
	process := NewProcess(cfg, quietLogger())
	defer process.Close()
	d, intent := createDaemonRestoreIntent(t, process, runtimeValue)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(intent.MarkerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unclean intent marker remains: %v", err)
	}
	if status, err := restoreswap.SuppressionStatus(cfg.DataDir); err != nil || status.Active {
		t.Fatalf("unclean intent created suppression: %+v %v", status, err)
	}
}

func TestRetainedIntentWithoutAcceptedRestartIsNotPromoted(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-9Rt"
	cfg := testConfig(t, runtimeValue)
	process := NewProcess(cfg, quietLogger())
	defer process.Close()
	d, intent := createDaemonRestoreIntent(t, process, runtimeValue)

	// An IntentRetainedError leaves this exact durable state, but the API does
	// not call RequestRestoreRestart because it returned 500.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Serve(ctx); err != nil {
		t.Fatalf("ordinary shutdown: %v", err)
	}
	if d.restoreRestartAccepted.Load() {
		t.Fatal("ordinary shutdown acquired an accepted-restart flag")
	}
	if _, err := restoreswap.ApplyPending(context.Background(), cfg.DataDir,
		[]byte(runtimeValue), cfg.Version); !errors.Is(err, restoreswap.ErrUncleanIntent) {
		t.Fatalf("unaccepted retained intent was promoted: %v", err)
	}

	reopened, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(intent.MarkerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("next boot did not abort retained intent: %v", err)
	}
	if reopened.RouterWritesSuppressed() {
		t.Fatal("aborted retained intent enabled router-write suppression")
	}
}

func TestControlledRestoreRestartAppliesAuditAndPersistsSuppression(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-5Vz"
	cfg := testConfig(t, runtimeValue)
	process := NewProcess(cfg, quietLogger())
	defer process.Close()
	d, intent := createDaemonRestoreIntent(t, process, runtimeValue)
	d.StartNeighbourReconciler(context.Background())
	d.mu.Lock()
	neighbourDone := d.neighbourDone
	d.mu.Unlock()
	d.RequestRestoreRestart()
	if err := d.Serve(context.Background()); !errors.Is(err, ErrRestoreRestart) {
		t.Fatalf("controlled restart: %v", err)
	}
	select {
	case <-neighbourDone:
	default:
		t.Fatal("old neighbour reconciler survived controlled restart")
	}

	restored, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RouterWritesSuppressed() {
		restored.Close()
		t.Fatal("restored controller opened without router-write suppression")
	}
	if release, ok := restored.api.BeginCapabilityOperation(); ok {
		release()
		restored.Close()
		t.Fatal("automatic capability probe crossed suppression")
	}
	if release, ok := restored.api.BeginNeighbourReconcileOperation(); ok {
		release()
		restored.Close()
		t.Fatal("neighbour reconcile crossed suppression")
	}
	restored.StartNeighbourReconciler(context.Background())
	restored.mu.Lock()
	startedWhileSuppressed := restored.neighbourDone != nil
	restored.mu.Unlock()
	if startedWhileSuppressed {
		restored.Close()
		t.Fatal("suppressed controller started the neighbour writer")
	}
	events, err := restored.Store.RecentEvents(context.Background(), 20)
	if err != nil {
		restored.Close()
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || event.Event == "controller.restore_applied"
	}
	if !found {
		restored.Close()
		t.Fatal("restored boot omitted controller.restore_applied audit")
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.RouterWritesSuppressed() {
		restarted.Close()
		t.Fatal("router-write suppression did not survive restart")
	}
	if err := restarted.ResumeRouterWrites(context.Background(), intent.ID); err != nil {
		restarted.Close()
		t.Fatal(err)
	}
	if restarted.RouterWritesSuppressed() {
		restarted.Close()
		t.Fatal("daemon suppression remained after durable clear")
	}
	restarted.api.RouterWritesResumed()
	restarted.mu.Lock()
	neighbourRestarted := restarted.neighbourDone != nil
	restarted.mu.Unlock()
	if !neighbourRestarted {
		restarted.Close()
		t.Fatal("explicit resume did not restart neighbour reconciliation")
	}
	if status, err := restoreswap.SuppressionStatus(cfg.DataDir); err != nil || status.Active {
		restarted.Close()
		t.Fatalf("durable suppression remained: %+v %v", status, err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContextSelectedBeforeConfirmationStillPromotesDrainedIntent(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-4Bx"
	cfg := testConfig(t, runtimeValue)
	process := NewProcess(cfg, quietLogger())
	defer process.Close()
	d, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := secrets.HashPassword([]byte("fixture-original-owner-8Wq"), restoreFixtureParams)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.CreateFirstAdmin(context.Background(), "original-owner", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.Site(context.Background()); err != nil {
		t.Fatal(err)
	}
	prepared := makePreparedRestore(t, cfg.DataDir, runtimeValue)
	var intent restoreswap.IntentResult
	d.shutdownOps.afterRequestDrain = func() error {
		var err error
		intent, err = restoreswap.CreateIntent(context.Background(), cfg.DataDir, prepared,
			d.Keys, []byte("fixture-export-value-5Zp"), d.restoreOwnerInstanceID)
		if err == nil {
			d.RequestRestoreRestart()
		}
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Serve(ctx); err != nil {
		t.Fatalf("context-selected shutdown: %v", err)
	}
	if intent.ID == "" {
		t.Fatal("drained confirmation did not publish an intent")
	}
	restored, err := process.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.RouterWritesSuppressed() || restored.RouterWriteSuppression().RestoreID != intent.ID {
		t.Fatal("context-selected shutdown lost the accepted restore intent")
	}
}

func TestAppliedRestoreAuditReceiptRetriesAndDeduplicates(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-7Dw"
	t.Run("retry after insert failure", func(t *testing.T) {
		cfg := testConfig(t, runtimeValue)
		process := NewProcess(cfg, quietLogger())
		defer process.Close()
		intent, _ := applyDaemonRestoreWithoutAudit(t, process, runtimeValue)
		db, keeper := openRestoredPair(t, cfg, runtimeValue)
		if _, err := db.SQL().ExecContext(context.Background(), `
CREATE TRIGGER reject_restore_audit
BEFORE INSERT ON events WHEN NEW.event='controller.restore_applied'
BEGIN SELECT RAISE(FAIL,'injected restore audit failure'); END`); err != nil {
			t.Fatal(err)
		}
		closeRestoredPair(t, cfg, db, keeper)

		if d, err := process.Open(context.Background()); err == nil {
			d.Close()
			t.Fatal("restore audit insert failure did not stop startup")
		}
		if receipt, err := restoreswap.PendingAppliedReceipt(cfg.DataDir); err != nil ||
			receipt.RestoreID != intent.ID {
			t.Fatalf("failed audit lost receipt: %+v %v", receipt, err)
		}
		db, keeper = openRestoredPair(t, cfg, runtimeValue)
		if _, err := db.SQL().ExecContext(context.Background(), `DROP TRIGGER reject_restore_audit`); err != nil {
			t.Fatal(err)
		}
		closeRestoredPair(t, cfg, db, keeper)

		restored, err := process.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		var count int
		if err := restored.Store.SQL().QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM events WHERE event='controller.restore_applied'
AND json_extract(detail_json,'$.restore_id')=?`, intent.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("applied restore audit count=%d", count)
		}
		if _, err := restoreswap.PendingAppliedReceipt(cfg.DataDir); !errors.Is(err, restoreswap.ErrNoAppliedReceipt) {
			t.Fatalf("successful retry retained receipt: %v", err)
		}
	})

	t.Run("existing audit is not duplicated", func(t *testing.T) {
		cfg := testConfig(t, runtimeValue)
		process := NewProcess(cfg, quietLogger())
		defer process.Close()
		intent, result := applyDaemonRestoreWithoutAudit(t, process, runtimeValue)
		db, keeper := openRestoredPair(t, cfg, runtimeValue)
		if err := db.LogEvent(context.Background(), store.Event{Category: "audit", Severity: "warning",
			Event: "controller.restore_applied", Detail: map[string]any{
				"restore_id": intent.ID, "authorizing_admin_id": result.AuthorizingAdminID,
				"authorizing_username": result.AuthorizingUsername, "preview_id": result.PreviewID,
				"plan_id": result.PlanID,
			}}); err != nil {
			t.Fatal(err)
		}
		closeRestoredPair(t, cfg, db, keeper)

		restored, err := process.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		var count int
		if err := restored.Store.SQL().QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM events WHERE event='controller.restore_applied'
AND json_extract(detail_json,'$.restore_id')=?`, intent.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("duplicate applied restore audits=%d", count)
		}
	})
}

func TestRestorePromotionRequiresSuccessfulPairClose(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-6Jt"
	cfg := testConfig(t, runtimeValue)
	process := NewProcess(cfg, quietLogger())
	defer process.Close()
	d, _ := createDaemonRestoreIntent(t, process, runtimeValue)
	var markCalls atomic.Int32
	d.shutdownOps.closeStore = func() error { return errors.New("injected store close failure") }
	d.shutdownOps.markClean = func(context.Context, string, string) error {
		markCalls.Add(1)
		return nil
	}
	d.RequestRestoreRestart()
	if err := d.Serve(context.Background()); err == nil || errors.Is(err, ErrRestoreRestart) {
		t.Fatalf("failed close returned %v", err)
	}
	if markCalls.Load() != 0 {
		t.Fatal("failed database close promoted restore intent")
	}
	if _, err := restoreswap.ApplyPending(context.Background(), cfg.DataDir,
		[]byte(runtimeValue), cfg.Version); !errors.Is(err, restoreswap.ErrUncleanIntent) {
		t.Fatalf("failed close changed intent state: %v", err)
	}
	if err := d.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restoreswap.AbortUnclean(context.Background(), cfg.DataDir); err != nil {
		t.Fatal(err)
	}
}

func TestRestorePromotionTimeoutLeavesIntentUnclean(t *testing.T) {
	const runtimeValue = "fixture-runtime-value-3Fc"
	cfg := testConfig(t, runtimeValue)
	process := NewProcess(cfg, quietLogger())
	defer process.Close()
	d, _ := createDaemonRestoreIntent(t, process, runtimeValue)
	d.shutdownOps.promotionTimeout = 10 * time.Millisecond
	d.shutdownOps.markClean = func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	d.RequestRestoreRestart()
	if err := d.Serve(context.Background()); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRestoreRestart) {
		t.Fatalf("promotion timeout returned %v", err)
	}
	if _, err := restoreswap.ApplyPending(context.Background(), cfg.DataDir,
		[]byte(runtimeValue), cfg.Version); !errors.Is(err, restoreswap.ErrUncleanIntent) {
		t.Fatalf("timed-out promotion changed intent state: %v", err)
	}
	if err := restoreswap.AbortUnclean(context.Background(), cfg.DataDir); err != nil {
		t.Fatal(err)
	}
}
