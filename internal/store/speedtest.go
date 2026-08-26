package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/speedtest"
)

const speedTestColumns = `id,state,phase,progress_percent,provider,method,provenance,
endpoint,estimated_bytes,actor_admin_id,actor_username,created_at,started_at,finished_at,
plan_id,
download_mbps,upload_mbps,idle_latency_ms,idle_jitter_ms,loaded_latency_ms,loaded_jitter_ms,
bytes_downloaded,bytes_uploaded,error`

func (db *DB) CreateSpeedTest(ctx context.Context, job speedtest.Job, maxHistory int) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin speed test creation: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO speed_tests (id,state,phase,progress_percent,provider,method,provenance,
 endpoint,estimated_bytes,actor_admin_id,actor_username,created_at,plan_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, job.ID, job.State, job.Phase, job.ProgressPercent,
		job.Provider, job.Method, job.Provenance, job.Endpoint, job.EstimatedBytes,
		job.ActorAdminID, job.ActorUsername, job.CreatedAt, job.PlanID)
	if err != nil {
		_ = tx.Rollback()
		if active, activeErr := db.ActiveSpeedTest(ctx); activeErr == nil && active != nil {
			return speedtest.ErrActive
		}
		return fmt.Errorf("store: create speed test: %w", err)
	}
	if err := pruneSpeedTestsOn(ctx, tx, maxHistory); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit speed test creation: %w", err)
	}
	return nil
}

func (db *DB) SpeedTest(ctx context.Context, id string) (*speedtest.Job, error) {
	return scanSpeedTest(db.sql.QueryRowContext(ctx,
		`SELECT `+speedTestColumns+` FROM speed_tests WHERE id=?`, id))
}

// SpeedTests returns terminal history; ActiveSpeedTest owns live job state.
func (db *DB) SpeedTests(ctx context.Context, limit int) ([]speedtest.Job, error) {
	if limit < 1 {
		limit = speedtest.MaxHistory
	} else if limit > speedtest.MaxListLimit {
		limit = speedtest.MaxListLimit
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT `+speedTestColumns+`
FROM speed_tests WHERE state IN ('completed','failed')
ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list speed tests: %w", err)
	}
	defer rows.Close()
	jobs := make([]speedtest.Job, 0, limit)
	for rows.Next() {
		job, err := scanSpeedTest(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

func (db *DB) ActiveSpeedTest(ctx context.Context) (*speedtest.Job, error) {
	job, err := scanSpeedTest(db.sql.QueryRowContext(ctx, `SELECT `+speedTestColumns+`
FROM speed_tests WHERE state IN ('queued','running','cancelling') LIMIT 1`))
	if errors.Is(err, speedtest.ErrNotFound) {
		return nil, nil
	}
	return job, err
}

func (db *DB) StartSpeedTest(ctx context.Context, id string, at int64) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE speed_tests
SET state='running',phase='idle-latency',progress_percent=1,started_at=max(?,created_at)
WHERE id=? AND state='queued'`, at, id)
	return speedTestTransition(res, err)
}

func (db *DB) UpdateSpeedTestProgress(ctx context.Context, id string, p speedtest.Progress) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE speed_tests
SET phase=?,progress_percent=?,bytes_downloaded=?,bytes_uploaded=?
WHERE id=? AND state='running'`, p.Phase, p.Percent, p.BytesDownloaded, p.BytesUploaded, id)
	return speedTestTransition(res, err)
}

func (db *DB) RequestSpeedTestCancel(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE speed_tests
SET state='cancelling',phase='cancelling'
WHERE id=? AND state IN ('queued','running')`, id)
	if err != nil {
		return fmt.Errorf("store: cancel speed test: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	job, readErr := db.SpeedTest(ctx, id)
	if errors.Is(readErr, speedtest.ErrNotFound) {
		return speedtest.ErrNotFound
	}
	if readErr != nil {
		return readErr
	}
	if job.State == "cancelling" {
		return nil
	}
	return speedtest.ErrTerminal
}

func (db *DB) FinishSpeedTest(ctx context.Context, id, state string, m speedtest.Measurement,
	detail string, at int64, maxHistory int) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin speed test outcome: %w", err)
	}
	defer tx.Rollback()
	var errorValue any
	if detail != "" {
		errorValue = detail
	}
	progress := 100
	if state == "failed" {
		progress = 0
	}
	res, err := tx.ExecContext(ctx, `UPDATE speed_tests SET
 state=?,phase=?,progress_percent=CASE WHEN progress_percent>? THEN progress_percent ELSE ? END,
	 finished_at=max(?,created_at,coalesce(started_at,created_at)),download_mbps=?,upload_mbps=?,idle_latency_ms=?,idle_jitter_ms=?,
 loaded_latency_ms=?,loaded_jitter_ms=?,bytes_downloaded=?,bytes_uploaded=?,error=?
WHERE id=? AND state IN ('queued','running','cancelling')`, state, state, progress, progress,
		at, m.DownloadMbps, m.UploadMbps, m.IdleLatencyMS, m.IdleJitterMS,
		m.LoadedLatencyMS, m.LoadedJitterMS, m.BytesDownloaded, m.BytesUploaded,
		errorValue, id)
	if err := speedTestTransition(res, err); err != nil {
		return err
	}
	if err := pruneSpeedTestsOn(ctx, tx, maxHistory); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit speed test outcome: %w", err)
	}
	return nil
}

// RecoverSpeedTests marks work abandoned by a previous controller process and
// enforces terminal retention. Daemon startup calls it before accepting requests.
func (db *DB) RecoverSpeedTests(ctx context.Context, at int64) ([]speedtest.Job, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin speed-test recovery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+speedTestColumns+`
FROM speed_tests WHERE state IN ('queued','running','cancelling') ORDER BY created_at,id`)
	if err != nil {
		return nil, fmt.Errorf("store: read interrupted speed tests: %w", err)
	}
	var jobs []speedtest.Job
	for rows.Next() {
		job, err := scanSpeedTest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(jobs) > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE speed_tests
SET state='failed',phase='failed',finished_at=max(?,created_at,coalesce(started_at,created_at)),error='controller restarted before the speed test finished'
WHERE state IN ('queued','running','cancelling')`, at)
		if err != nil {
			return nil, fmt.Errorf("store: recover interrupted speed tests: %w", err)
		}
		for _, job := range jobs {
			recoveredAt := at
			if recoveredAt < job.CreatedAt {
				recoveredAt = job.CreatedAt
			}
			if job.StartedAt != nil && recoveredAt < *job.StartedAt {
				recoveredAt = *job.StartedAt
			}
			event, detail, err := normalizeEvent(Event{TS: recoveredAt / 1000, IngestedAt: recoveredAt,
				Category: "audit", Severity: "error", Event: "speedtest.failure",
				Detail: map[string]any{
					"job_id": job.ID, "username": job.ActorUsername,
					"provider": job.Provider, "method": job.Method,
					"provenance": job.Provenance, "plan_id": job.PlanID,
					"reason": "controller restarted before the speed test finished",
				}})
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, appendEventSQL, eventInsertArgs(event, detail)...); err != nil {
				return nil, fmt.Errorf("store: audit interrupted speed test: %w", err)
			}
		}
	}
	if err := pruneSpeedTestsOn(ctx, tx, speedtest.MaxHistory); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit speed-test recovery: %w", err)
	}
	return jobs, nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func pruneSpeedTestsOn(ctx context.Context, exec contextExecer, maxHistory int) error {
	if maxHistory < 0 {
		maxHistory = 0
	}
	_, err := exec.ExecContext(ctx, `DELETE FROM speed_tests
WHERE state IN ('completed','failed') AND id NOT IN (
 SELECT id FROM speed_tests WHERE state IN ('completed','failed')
 ORDER BY created_at DESC,id DESC LIMIT ?
)`, maxHistory)
	if err != nil {
		return fmt.Errorf("store: prune speed tests: %w", err)
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanSpeedTest(row rowScanner) (*speedtest.Job, error) {
	var job speedtest.Job
	var started, finished sql.NullInt64
	var download, upload, idleLatency, idleJitter, loadedLatency, loadedJitter sql.NullFloat64
	var detail sql.NullString
	err := row.Scan(&job.ID, &job.State, &job.Phase, &job.ProgressPercent, &job.Provider,
		&job.Method, &job.Provenance, &job.Endpoint, &job.EstimatedBytes,
		&job.ActorAdminID, &job.ActorUsername, &job.CreatedAt, &started, &finished, &job.PlanID,
		&download, &upload, &idleLatency, &idleJitter, &loadedLatency, &loadedJitter,
		&job.BytesDownloaded, &job.BytesUploaded, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, speedtest.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan speed test: %w", err)
	}
	job.StartedAt = int64Ptr(started)
	job.FinishedAt = int64Ptr(finished)
	job.DownloadMbps = float64Ptr(download)
	job.UploadMbps = float64Ptr(upload)
	job.IdleLatencyMS = float64Ptr(idleLatency)
	job.IdleJitterMS = float64Ptr(idleJitter)
	job.LoadedLatencyMS = float64Ptr(loadedLatency)
	job.LoadedJitterMS = float64Ptr(loadedJitter)
	if detail.Valid {
		job.Error = &detail.String
	}
	return &job, nil
}

func speedTestTransition(res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("store: transition speed test: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return speedtest.ErrTerminal
	}
	return nil
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func float64Ptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}
