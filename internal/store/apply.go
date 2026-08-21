package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ApplyOperationState is the durable lifecycle of one fleet Apply request.
type ApplyOperationState string

const (
	ApplyOperationQueued    ApplyOperationState = "queued"
	ApplyOperationRunning   ApplyOperationState = "running"
	ApplyOperationCompleted ApplyOperationState = "completed"
	ApplyOperationFailed    ApplyOperationState = "failed"
	ApplyOperationUnknown   ApplyOperationState = "unknown"
)

// ApplyOperationDeviceState is the durable lifecycle of one device within a
// fleet Apply. Applying is committed before the first call that may write UCI.
type ApplyOperationDeviceState string

const (
	ApplyOperationDeviceQueued    ApplyOperationDeviceState = "queued"
	ApplyOperationDeviceApplying  ApplyOperationDeviceState = "applying"
	ApplyOperationDeviceCompleted ApplyOperationDeviceState = "completed"
	ApplyOperationDeviceFailed    ApplyOperationDeviceState = "failed"
	ApplyOperationDeviceUnknown   ApplyOperationDeviceState = "unknown"
	ApplyOperationDeviceSkipped   ApplyOperationDeviceState = "skipped"
)

const (
	ApplyWriteStateNone     = "none"
	ApplyWriteStatePossible = "possible"
)

// ErrApplyOperationState reports a compare-and-swap lifecycle conflict.
var ErrApplyOperationState = errors.New("store: apply operation state conflict")

// ApplyOperation is the durable, public-safe receipt for one Apply request.
// Empty strings and a zero HTTPStatus represent nullable SQL fields.
type ApplyOperation struct {
	OperationID   string
	RequestHash   string
	ActorAdminID  int64
	ActorUsername string
	State         ApplyOperationState
	CreatedAt     int64
	StartedAt     *int64
	FinishedAt    *int64
	ResultJSON    []byte
	Error         string
	WriteState    string
	HTTPStatus    int
	Devices       []ApplyOperationDevice
}

// ApplyOperationDevice snapshots identity and outcome so un-adoption or
// SQLite device-id reuse cannot rewrite operation history.
type ApplyOperationDevice struct {
	OperationID   string
	Ordinal       int
	DeviceID      int64
	DeviceMAC     string
	DeviceName    string
	State         ApplyOperationDeviceState
	WriteState    string
	RouterOutcome string
	Outcome       string
	Changes       int
	Reason        string
	StartedAt     *int64
	FinishedAt    *int64
}

// BeginApplyOperation atomically creates a queued operation or loads the row
// already using operationID. The trusted API caller supplies actor identity;
// it is never accepted from the Apply request body. The caller compares
// RequestHash when created is false.
func (db *DB) BeginApplyOperation(ctx context.Context, operationID, requestHash string,
	actorAdminID int64, actorUsername string, createdAt int64) (*ApplyOperation, bool, error) {
	if operationID == "" {
		return nil, false, errors.New("store: apply operation id is empty")
	}
	if requestHash == "" {
		return nil, false, errors.New("store: apply operation request hash is empty")
	}
	if actorAdminID <= 0 {
		return nil, false, errors.New("store: apply operation actor id is invalid")
	}
	if actorUsername == "" {
		return nil, false, errors.New("store: apply operation actor username is empty")
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("store: begin apply operation: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	res, err := tx.ExecContext(ctx, `
INSERT INTO apply_operations
       (operation_id, request_hash, actor_admin_id, actor_username, state, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(operation_id) DO NOTHING`,
		operationID, requestHash, actorAdminID, actorUsername,
		ApplyOperationQueued, createdAt)
	if err != nil {
		return nil, false, fmt.Errorf("store: create apply operation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("store: inspect apply operation insert: %w", err)
	}
	op, err := scanApplyOperation(tx.QueryRowContext(ctx, applyOperationSelect+
		` WHERE operation_id = ?`, operationID))
	if err != nil {
		return nil, false, fmt.Errorf("store: load begun apply operation: %w", err)
	}
	op.Devices = []ApplyOperationDevice{}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("store: commit apply operation: %w", err)
	}
	return op, n == 1, nil
}

// ApplyOperation returns one durable Apply receipt and its devices in fleet
// order.
func (db *DB) ApplyOperation(ctx context.Context, operationID string) (*ApplyOperation, error) {
	op, err := scanApplyOperation(db.sql.QueryRowContext(ctx, applyOperationSelect+
		` WHERE operation_id = ?`, operationID))
	if err != nil {
		return nil, err
	}
	op.Devices, err = db.applyOperationDevices(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return op, nil
}

// InitializeApplyOperationDevices records the complete fleet order after the
// parent run starts and before any device begins. Ordinals come from slice
// order rather than caller-controlled fields, making a duplicate or missing
// position impossible.
func (db *DB) InitializeApplyOperationDevices(ctx context.Context, operationID string,
	devices []ApplyOperationDevice) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin apply device initialization: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var state string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM apply_operations WHERE operation_id = ?`, operationID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: load apply operation for device initialization: %w", err)
	}
	if ApplyOperationState(state) != ApplyOperationRunning {
		return fmt.Errorf("store: apply operation %q is %s, cannot initialize devices: %w",
			operationID, state, ErrApplyOperationState)
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM apply_operation_devices WHERE operation_id = ?`,
		operationID).Scan(&existing); err != nil {
		return fmt.Errorf("store: count initialized apply devices: %w", err)
	}
	if existing != 0 {
		return fmt.Errorf("store: apply operation %q already has devices: %w",
			operationID, ErrApplyOperationState)
	}

	for ordinal, dev := range devices {
		if dev.DeviceID <= 0 || dev.DeviceMAC == "" || dev.DeviceName == "" {
			return fmt.Errorf("store: apply operation device %d has incomplete identity", ordinal)
		}
		if dev.Changes < 0 {
			return fmt.Errorf("store: apply operation device %d has negative changes", ordinal)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO apply_operation_devices
       (operation_id, ordinal, device_id, device_mac, device_name,
        state, write_state, changes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, operationID, ordinal, dev.DeviceID,
			dev.DeviceMAC, dev.DeviceName, ApplyOperationDeviceQueued,
			ApplyWriteStateNone, dev.Changes); err != nil {
			return fmt.Errorf("store: initialize apply operation device %d: %w", ordinal, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit apply device initialization: %w", err)
	}
	return nil
}

// MarkApplyOperationRunning advances a queued operation exactly once.
func (db *DB) MarkApplyOperationRunning(ctx context.Context, operationID string,
	startedAt int64) error {
	res, err := db.sql.ExecContext(ctx, `
UPDATE apply_operations
   SET state = ?, started_at = ?
 WHERE operation_id = ? AND state = ?`,
		ApplyOperationRunning, startedAt, operationID, ApplyOperationQueued)
	if err != nil {
		return fmt.Errorf("store: mark apply operation running: %w", err)
	}
	return db.checkApplyTransition(ctx, operationID, res,
		string(ApplyOperationQueued), string(ApplyOperationRunning))
}

// MarkApplyOperationDeviceApplying commits the possible-write boundary before
// the caller invokes any router operation. The child and parent change in one
// transaction, so neither can claim no write while the other says otherwise.
func (db *DB) MarkApplyOperationDeviceApplying(ctx context.Context,
	operationID string, ordinal int, startedAt int64) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin apply device boundary: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	parent, err := tx.ExecContext(ctx, `
UPDATE apply_operations
   SET write_state = ?
 WHERE operation_id = ? AND state = ?`,
		ApplyWriteStatePossible, operationID, ApplyOperationRunning)
	if err != nil {
		return fmt.Errorf("store: mark parent apply write boundary: %w", err)
	}
	if err := checkTxApplyTransition(ctx, tx, operationID, parent,
		string(ApplyOperationRunning), "write boundary"); err != nil {
		return err
	}
	child, err := tx.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, write_state = ?, started_at = ?
 WHERE operation_id = ? AND ordinal = ? AND state = ?`,
		ApplyOperationDeviceApplying, ApplyWriteStatePossible, startedAt,
		operationID, ordinal, ApplyOperationDeviceQueued)
	if err != nil {
		return fmt.Errorf("store: mark apply device applying: %w", err)
	}
	if err := checkTxApplyDeviceTransition(ctx, tx, operationID, ordinal, child,
		string(ApplyOperationDeviceQueued), string(ApplyOperationDeviceApplying)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit apply device boundary: %w", err)
	}
	return nil
}

// FinishApplyOperationDevice records a terminal device result. A no-write
// result must transition directly from queued with write_state=none; once the
// applying boundary was crossed it remains possible, even after a definitive
// router outcome.
func (db *DB) FinishApplyOperationDevice(ctx context.Context, operationID string,
	ordinal int, state ApplyOperationDeviceState, finishedAt int64,
	routerOutcome, outcome string, changes int, reason, writeState string) error {
	if !terminalApplyOperationDeviceState(state) {
		return fmt.Errorf("store: terminal apply device state %q: %w",
			state, ErrApplyOperationState)
	}
	if writeState != ApplyWriteStateNone && writeState != ApplyWriteStatePossible {
		return fmt.Errorf("store: apply device write state %q is invalid", writeState)
	}
	if changes < 0 {
		return errors.New("store: apply device changes cannot be negative")
	}
	from := ApplyOperationDeviceQueued
	if writeState == ApplyWriteStatePossible {
		from = ApplyOperationDeviceApplying
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, write_state = ?, router_outcome = ?, outcome = ?,
       changes = ?, reason = ?, finished_at = ?
 WHERE operation_id = ? AND ordinal = ? AND state = ?`,
		state, writeState, nullableString(routerOutcome), nullableString(outcome),
		changes, nullableString(reason), finishedAt, operationID, ordinal, from)
	if err != nil {
		return fmt.Errorf("store: finish apply operation device: %w", err)
	}
	return db.checkApplyDeviceTransition(ctx, operationID, ordinal, res,
		string(from), string(state))
}

// FinishApplyOperation records a terminal result from either queued preflight
// or a running fleet operation. resultJSON must already be safe for the public
// API; raw requests, preview tokens and plans must never be passed here.
func (db *DB) FinishApplyOperation(ctx context.Context, operationID string,
	state ApplyOperationState, finishedAt int64, resultJSON []byte, errText,
	writeState string, httpStatus int) error {
	if !terminalApplyOperationState(state) {
		return fmt.Errorf("store: terminal apply operation state %q: %w",
			state, ErrApplyOperationState)
	}
	if writeState != ApplyWriteStateNone && writeState != ApplyWriteStatePossible {
		return fmt.Errorf("store: apply operation write state %q is invalid", writeState)
	}

	var resultValue any
	if resultJSON != nil {
		resultValue = resultJSON
	}
	var statusValue any
	if httpStatus != 0 {
		statusValue = httpStatus
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin apply operation finish: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, write_state = ?, outcome = COALESCE(outcome, ?),
       finished_at = ?
 WHERE operation_id = ? AND state = ?`,
		ApplyOperationDeviceSkipped, ApplyWriteStateNone, "skipped", finishedAt,
		operationID, ApplyOperationDeviceQueued); err != nil {
		return fmt.Errorf("store: skip remaining apply operation devices: %w", err)
	}
	var applying int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM apply_operation_devices
 WHERE operation_id = ? AND state = ?`, operationID,
		ApplyOperationDeviceApplying).Scan(&applying); err != nil {
		return fmt.Errorf("store: count applying operation devices: %w", err)
	}
	if applying != 0 {
		return fmt.Errorf("store: apply operation %q still has %d applying device(s): %w",
			operationID, applying, ErrApplyOperationState)
	}
	res, err := tx.ExecContext(ctx, `
UPDATE apply_operations
   SET state = ?, finished_at = ?, result_json = ?, error = ?,
       write_state = CASE WHEN write_state = ? THEN ? ELSE ? END,
       http_status = ?
 WHERE operation_id = ? AND state IN (?, ?)`,
		state, finishedAt, resultValue, nullableString(errText),
		ApplyWriteStatePossible, ApplyWriteStatePossible, writeState,
		statusValue, operationID, ApplyOperationQueued, ApplyOperationRunning)
	if err != nil {
		return fmt.Errorf("store: finish apply operation: %w", err)
	}
	if err := checkTxApplyTransition(ctx, tx, operationID, res,
		"queued or running", string(state)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit apply operation finish: %w", err)
	}
	return nil
}

// InterruptApplyOperation truthfully closes one run whose normal terminal
// receipt could not be persisted. It never resumes, confirms or infers a
// router outcome: applying devices become unknown/possible and untouched
// devices become skipped/none.
func (db *DB) InterruptApplyOperation(ctx context.Context, operationID string,
	finishedAt int64, reason string) error {
	if reason == "" {
		return errors.New("store: interrupted apply operation reason is empty")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin apply operation interruption: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, finished_at = ?, write_state = ?, outcome = COALESCE(outcome, ?),
       reason = COALESCE(reason, ?)
 WHERE operation_id = ? AND state = ?`, ApplyOperationDeviceSkipped, finishedAt,
		ApplyWriteStateNone, "skipped", reason, operationID,
		ApplyOperationDeviceQueued); err != nil {
		return fmt.Errorf("store: skip interrupted apply devices: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, finished_at = ?, write_state = ?,
       router_outcome = COALESCE(router_outcome, ?),
       outcome = COALESCE(outcome, ?), reason = COALESCE(reason, ?)
 WHERE operation_id = ? AND state = ?`, ApplyOperationDeviceUnknown, finishedAt,
		ApplyWriteStatePossible, "unknown", "unknown", reason, operationID,
		ApplyOperationDeviceApplying); err != nil {
		return fmt.Errorf("store: mark interrupted applying devices unknown: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
UPDATE apply_operations
   SET state = ?, finished_at = ?, error = ?, http_status = ?,
       write_state = CASE WHEN write_state = ? THEN ? ELSE ? END
 WHERE operation_id = ? AND state IN (?, ?)`, ApplyOperationUnknown, finishedAt,
		reason, 503, ApplyWriteStatePossible, ApplyWriteStatePossible,
		ApplyWriteStateNone, operationID, ApplyOperationQueued, ApplyOperationRunning)
	if err != nil {
		return fmt.Errorf("store: interrupt apply operation: %w", err)
	}
	if err := checkTxApplyTransition(ctx, tx, operationID, res,
		"queued or running", string(ApplyOperationUnknown)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit apply operation interruption: %w", err)
	}
	return nil
}

// RecoverApplyOperations closes every operation and device a previous process
// left unfinished. Recovery never resumes a router write or guesses that it
// completed.
func (db *DB) RecoverApplyOperations(ctx context.Context, finishedAt int64) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recovery: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, finished_at = ?, write_state = ?, outcome = COALESCE(outcome, ?),
       reason = COALESCE(reason, ?)
 WHERE state = ? AND operation_id IN (
       SELECT operation_id FROM apply_operations WHERE state IN (?, ?)
 )`, ApplyOperationDeviceSkipped, finishedAt, ApplyWriteStateNone, "skipped",
		"controller restarted before this device began",
		ApplyOperationDeviceQueued, ApplyOperationQueued, ApplyOperationRunning); err != nil {
		return fmt.Errorf("recover queued apply operation devices: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operation_devices
   SET state = ?, finished_at = ?, write_state = ?,
       router_outcome = COALESCE(router_outcome, ?),
       outcome = COALESCE(outcome, ?), reason = COALESCE(reason, ?)
 WHERE state = ? AND operation_id IN (
       SELECT operation_id FROM apply_operations WHERE state IN (?, ?)
	)`, ApplyOperationDeviceUnknown, finishedAt, ApplyWriteStatePossible,
		"unknown", "unknown", "controller restarted while this device write was in progress",
		ApplyOperationDeviceApplying,
		ApplyOperationQueued, ApplyOperationRunning); err != nil {
		return fmt.Errorf("recover applying operation devices: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operations
   SET state = ?, finished_at = ?, write_state = ?, error = ?, http_status = ?
 WHERE state = ?`, ApplyOperationUnknown, finishedAt, ApplyWriteStateNone,
		"controller restarted before this queued Apply started", 503,
		ApplyOperationQueued); err != nil {
		return fmt.Errorf("recover queued apply operations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE apply_operations
   SET state = ?, finished_at = ?,
       error = CASE WHEN write_state = ? THEN ? ELSE ? END,
       http_status = ?
 WHERE state = ?`, ApplyOperationUnknown, finishedAt, ApplyWriteStatePossible,
		"controller restarted while this Apply was running; device outcome may be incomplete",
		"controller restarted while this Apply was running before any device write began",
		503, ApplyOperationRunning); err != nil {
		return fmt.Errorf("recover running apply operations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit apply operation recovery: %w", err)
	}
	return nil
}

const applyOperationSelect = `SELECT operation_id, request_hash,
       actor_admin_id, actor_username, state, created_at, started_at,
       finished_at, result_json, error, write_state, http_status
  FROM apply_operations`

type applyOperationScanner interface {
	Scan(dest ...any) error
}

func scanApplyOperation(row applyOperationScanner) (*ApplyOperation, error) {
	var (
		op         ApplyOperation
		state      string
		errorText  sql.NullString
		writeState sql.NullString
		httpStatus sql.NullInt64
	)
	if err := row.Scan(&op.OperationID, &op.RequestHash, &op.ActorAdminID,
		&op.ActorUsername, &state, &op.CreatedAt, &op.StartedAt, &op.FinishedAt,
		&op.ResultJSON, &errorText, &writeState, &httpStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan apply operation: %w", err)
	}
	op.State = ApplyOperationState(state)
	if errorText.Valid {
		op.Error = errorText.String
	}
	if writeState.Valid {
		op.WriteState = writeState.String
	}
	if httpStatus.Valid {
		op.HTTPStatus = int(httpStatus.Int64)
	}
	return &op, nil
}

func (db *DB) applyOperationDevices(ctx context.Context,
	operationID string) ([]ApplyOperationDevice, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT operation_id, ordinal, device_id, device_mac, device_name, state,
       write_state, router_outcome, outcome, changes, reason,
       started_at, finished_at
  FROM apply_operation_devices
 WHERE operation_id = ?
 ORDER BY ordinal`, operationID)
	if err != nil {
		return nil, fmt.Errorf("store: load apply operation devices: %w", err)
	}
	defer rows.Close()
	out := []ApplyOperationDevice{}
	for rows.Next() {
		var (
			dev                            ApplyOperationDevice
			state                          string
			routerOutcome, outcome, reason sql.NullString
		)
		if err := rows.Scan(&dev.OperationID, &dev.Ordinal, &dev.DeviceID,
			&dev.DeviceMAC, &dev.DeviceName, &state, &dev.WriteState,
			&routerOutcome, &outcome, &dev.Changes, &reason, &dev.StartedAt,
			&dev.FinishedAt); err != nil {
			return nil, fmt.Errorf("store: scan apply operation device: %w", err)
		}
		dev.State = ApplyOperationDeviceState(state)
		if routerOutcome.Valid {
			dev.RouterOutcome = routerOutcome.String
		}
		if outcome.Valid {
			dev.Outcome = outcome.String
		}
		if reason.Valid {
			dev.Reason = reason.String
		}
		out = append(out, dev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate apply operation devices: %w", err)
	}
	return out, nil
}

func terminalApplyOperationState(state ApplyOperationState) bool {
	switch state {
	case ApplyOperationCompleted, ApplyOperationFailed, ApplyOperationUnknown:
		return true
	default:
		return false
	}
}

func terminalApplyOperationDeviceState(state ApplyOperationDeviceState) bool {
	switch state {
	case ApplyOperationDeviceCompleted, ApplyOperationDeviceFailed,
		ApplyOperationDeviceUnknown, ApplyOperationDeviceSkipped:
		return true
	default:
		return false
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (db *DB) checkApplyTransition(ctx context.Context, operationID string,
	res sql.Result, from, to string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect apply operation transition: %w", err)
	}
	if n == 1 {
		return nil
	}
	if n > 1 {
		return fmt.Errorf("store: apply operation %q updated %d rows", operationID, n)
	}
	var current string
	err = db.sql.QueryRowContext(ctx,
		`SELECT state FROM apply_operations WHERE operation_id = ?`, operationID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: inspect apply operation state: %w", err)
	}
	return fmt.Errorf("store: apply operation %q is %s, cannot transition from %s to %s: %w",
		operationID, current, from, to, ErrApplyOperationState)
}

func (db *DB) checkApplyDeviceTransition(ctx context.Context, operationID string,
	ordinal int, res sql.Result, from, to string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect apply device transition: %w", err)
	}
	if n == 1 {
		return nil
	}
	if n > 1 {
		return fmt.Errorf("store: apply operation %q device %d updated %d rows",
			operationID, ordinal, n)
	}
	var current string
	err = db.sql.QueryRowContext(ctx, `
SELECT state FROM apply_operation_devices
 WHERE operation_id = ? AND ordinal = ?`, operationID, ordinal).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: inspect apply device state: %w", err)
	}
	return fmt.Errorf("store: apply operation %q device %d is %s, cannot transition from %s to %s: %w",
		operationID, ordinal, current, from, to, ErrApplyOperationState)
}

func checkTxApplyTransition(ctx context.Context, tx *sql.Tx, operationID string,
	res sql.Result, from, to string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect apply operation transition: %w", err)
	}
	if n == 1 {
		return nil
	}
	var current string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM apply_operations WHERE operation_id = ?`, operationID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: inspect apply operation state: %w", err)
	}
	return fmt.Errorf("store: apply operation %q is %s, cannot transition from %s to %s: %w",
		operationID, current, from, to, ErrApplyOperationState)
}

func checkTxApplyDeviceTransition(ctx context.Context, tx *sql.Tx,
	operationID string, ordinal int, res sql.Result, from, to string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect apply device transition: %w", err)
	}
	if n == 1 {
		return nil
	}
	var current string
	err = tx.QueryRowContext(ctx, `
SELECT state FROM apply_operation_devices
 WHERE operation_id = ? AND ordinal = ?`, operationID, ordinal).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: inspect apply device state: %w", err)
	}
	return fmt.Errorf("store: apply operation %q device %d is %s, cannot transition from %s to %s: %w",
		operationID, ordinal, current, from, to, ErrApplyOperationState)
}
