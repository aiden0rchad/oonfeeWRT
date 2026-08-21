package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

const interruptedRadioScanDetail = `{"source":"controller","interrupted":true,"reason":"controller restarted before the scan outcome was durably recorded"}`

// RecoverRadioScans truthfully closes work a previous controller process left
// unfinished. It never resumes a disruptive device operation after restart.
func (db *DB) RecoverRadioScans(ctx context.Context, finishedAt int64) (int64, error) {
	if finishedAt <= 0 {
		finishedAt = time.Now().UnixMilli()
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE radio_scans
   SET status=?,
       finished_at=CASE WHEN started_at>? THEN started_at ELSE ? END,
       detail_json=?
 WHERE status IN (?,?)`, model.RadioScanFailed, finishedAt, finishedAt,
		interruptedRadioScanDetail, model.RadioScanPending, model.RadioScanRunning)
	if err != nil {
		return 0, fmt.Errorf("store: recover radio scans: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: inspect radio scan recovery: %w", err)
	}
	return n, nil
}

// LatestRadioScan returns the newest persisted operator-triggered scan for one
// stable UCI radio key, including failed attempts and their empty result set.
func (db *DB) LatestRadioScan(ctx context.Context, key model.RadioKey) (model.RadioScan, []model.RadioScanBSS, error) {
	var id int64
	err := db.sql.QueryRowContext(ctx, `
SELECT id FROM radio_scans
 WHERE device_id=? AND radio_key=?
 ORDER BY started_at DESC,id DESC LIMIT 1`, key.DeviceID, key.Section).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.RadioScan{}, nil, ErrNotFound
	}
	if err != nil {
		return model.RadioScan{}, nil, err
	}
	return db.RadioScanByID(ctx, id)
}
