package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/speedtest"
)

func speedJob(id string, created int64) speedtest.Job {
	return speedtest.Job{ID: id, State: "queued", Phase: "queued",
		Provider: "test", Method: "controller-http-single-stream-v1",
		Provenance: "controller-host", Endpoint: "http://127.0.0.1",
		EstimatedBytes: 100, PlanID: "sha256:test", ActorAdminID: 1,
		ActorUsername: "admin", CreatedAt: created}
}

func TestSpeedTestsEnforceOneActiveAndBoundHistory(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	first := speedJob("active", 1)
	if err := db.CreateSpeedTest(ctx, first, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSpeedTest(ctx, speedJob("second", 2), 2); !errors.Is(err, speedtest.ErrActive) {
		t.Fatalf("second active error=%v", err)
	}
	if err := db.StartSpeedTest(ctx, first.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSpeedTest(ctx, first.ID, "completed", speedtest.Measurement{}, "", 3, 2); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"one", "two", "three"} {
		job := speedJob(id, int64(10+i))
		if err := db.CreateSpeedTest(ctx, job, 2); err != nil {
			t.Fatal(err)
		}
		if err := db.StartSpeedTest(ctx, id, job.CreatedAt+1); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishSpeedTest(ctx, id, "failed", speedtest.Measurement{}, "test", job.CreatedAt+2, 2); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := db.SpeedTests(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "three" || jobs[1].ID != "two" {
		t.Fatalf("history=%+v", jobs)
	}
}

func TestSpeedTestCreateAndFinishRollbackWhenPruneFails(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	seed := speedJob("seed", 1)
	if err := db.CreateSpeedTest(ctx, seed, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.StartSpeedTest(ctx, seed.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSpeedTest(ctx, seed.ID, "completed", speedtest.Measurement{}, "", 3, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `CREATE TRIGGER reject_speed_prune BEFORE DELETE ON speed_tests
BEGIN SELECT RAISE(ABORT, 'prune blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSpeedTest(ctx, speedJob("rolled-back", 4), 0); err == nil {
		t.Fatal("creation succeeded despite prune failure")
	}
	if _, err := db.SpeedTest(ctx, "rolled-back"); !errors.Is(err, speedtest.ErrNotFound) {
		t.Fatalf("failed creation persisted: %v", err)
	}
	active := speedJob("finish-rollback", 5)
	if err := db.CreateSpeedTest(ctx, active, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.StartSpeedTest(ctx, active.ID, 6); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSpeedTest(ctx, active.ID, "completed", speedtest.Measurement{}, "", 7, 0); err == nil {
		t.Fatal("finish succeeded despite prune failure")
	}
	got, err := db.SpeedTest(ctx, active.ID)
	if err != nil || got.State != "running" {
		t.Fatalf("failed finish state=%+v err=%v", got, err)
	}
}

func TestSpeedTestTransitionsClampBackwardClock(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	job := speedJob("clock-rollback", 1_000)
	if err := db.CreateSpeedTest(ctx, job, speedtest.MaxHistory); err != nil {
		t.Fatal(err)
	}
	if err := db.StartSpeedTest(ctx, job.ID, 1); err != nil {
		t.Fatal(err)
	}
	started, err := db.SpeedTest(ctx, job.ID)
	if err != nil || started.StartedAt == nil || *started.StartedAt != job.CreatedAt {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	if err := db.FinishSpeedTest(ctx, job.ID, "completed", speedtest.Measurement{}, "",
		2, speedtest.MaxHistory); err != nil {
		t.Fatal(err)
	}
	finished, err := db.SpeedTest(ctx, job.ID)
	if err != nil || finished.FinishedAt == nil || *finished.FinishedAt != job.CreatedAt {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
}

func TestRecoverSpeedTestsFailsInterruptedJobs(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	job := speedJob("interrupted", 10)
	if err := db.CreateSpeedTest(ctx, job, 50); err != nil {
		t.Fatal(err)
	}
	if err := db.StartSpeedTest(ctx, job.ID, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `CREATE TRIGGER reject_speed_audit BEFORE INSERT ON events
WHEN NEW.event='speedtest.failure' BEGIN SELECT RAISE(ABORT, 'audit blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverSpeedTests(ctx, 12); err == nil {
		t.Fatal("recovery succeeded without its required audit row")
	}
	stillRunning, err := db.SpeedTest(ctx, job.ID)
	if err != nil || stillRunning.State != "running" {
		t.Fatalf("failed recovery was not atomic: job=%+v err=%v", stillRunning, err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DROP TRIGGER reject_speed_audit`); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.RecoverSpeedTests(ctx, 12)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	got, err := db.SpeedTest(ctx, job.ID)
	if err != nil || got.State != "failed" || got.Error == nil ||
		!strings.Contains(*got.Error, "controller restarted") {
		t.Fatalf("job=%+v err=%v", got, err)
	}
	events, err := db.RecentEvents(ctx, 10)
	if err != nil || len(events) != 1 || events[0].Event != "speedtest.failure" {
		t.Fatalf("recovery audit=%+v err=%v", events, err)
	}
}

func TestRecoverSpeedTestsBoundsHistory(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for i := 0; i < speedtest.MaxHistory; i++ {
		id := fmt.Sprintf("complete-%02d", i)
		job := speedJob(id, int64(i+1))
		if err := db.CreateSpeedTest(ctx, job, speedtest.MaxHistory+1); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishSpeedTest(ctx, id, "completed", speedtest.Measurement{}, "",
			job.CreatedAt+1, speedtest.MaxHistory+1); err != nil {
			t.Fatal(err)
		}
	}
	interrupted := speedJob("interrupted", speedtest.MaxHistory+1)
	if err := db.CreateSpeedTest(ctx, interrupted, speedtest.MaxHistory+1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverSpeedTests(ctx, interrupted.CreatedAt+1); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.SpeedTests(ctx, speedtest.MaxHistory)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != speedtest.MaxHistory || jobs[0].ID != interrupted.ID {
		t.Fatalf("history count=%d newest=%q", len(jobs), jobs[0].ID)
	}
	if _, err := db.SpeedTest(ctx, "complete-00"); !errors.Is(err, speedtest.ErrNotFound) {
		t.Fatalf("oldest recovered history entry was not pruned: %v", err)
	}
}

func TestRecoverSpeedTestsClampsFutureDatedJobAndAudit(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	const created = int64(2_000_000_000_000)
	job := speedJob("future", created)
	if err := db.CreateSpeedTest(ctx, job, speedtest.MaxHistory); err != nil {
		t.Fatal(err)
	}
	if err := db.StartSpeedTest(ctx, job.ID, created+10); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverSpeedTests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got, err := db.SpeedTest(ctx, job.ID)
	if err != nil || got.FinishedAt == nil || *got.FinishedAt != created+10 {
		t.Fatalf("recovered=%+v err=%v", got, err)
	}
	events, err := db.RecentEvents(ctx, 1)
	if err != nil || len(events) != 1 || events[0].TS != (created+10)/1_000 ||
		events[0].IngestedAt != created+10 {
		t.Fatalf("recovery audit=%+v err=%v", events, err)
	}
}

func TestSchema18MigratesAndAttestsActiveIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v17.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DROP TABLE speed_tests;
UPDATE schema_version SET version=17 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v17: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DROP INDEX speed_tests_one_active;
CREATE UNIQUE INDEX speed_tests_one_active ON speed_tests(provenance)
WHERE state IN ('queued','running')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenReadOnly(ctx, driver, path, protector); err == nil {
		reopened.Close()
		t.Fatal("read-only open accepted tampered speed-test active index")
	} else if !strings.Contains(err.Error(), "speed_tests_one_active has the wrong predicate") {
		t.Fatalf("attestation error=%v", err)
	}
}

func TestSchema18AttestsSpeedTestChecks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checks.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA writable_schema=ON`); err != nil {
		t.Fatal(err)
	}
	res, err := conn.ExecContext(ctx, `UPDATE sqlite_master SET sql=replace(sql,?,?)
WHERE type='table' AND name='speed_tests'`,
		"provenance TEXT NOT NULL CHECK (provenance = 'controller-host')",
		"provenance TEXT NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("tampered rows=%d err=%v", n, err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA writable_schema=OFF`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenReadOnly(ctx, driver, path, protector); err == nil {
		reopened.Close()
		t.Fatal("read-only open accepted speed_tests without its provenance CHECK")
	} else if !strings.Contains(err.Error(), "table speed_tests has CHECK constraints") {
		t.Fatalf("attestation error=%v", err)
	}
}
