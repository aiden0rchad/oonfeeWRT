package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/alerts"
)

const alertSchemaSQL = `CREATE TABLE IF NOT EXISTS controller_alert_state (
 id INTEGER PRIMARY KEY CHECK (id=1),
 state_json BLOB NOT NULL CHECK (length(state_json)<=2097152)
)`

func (db *DB) LoadAlertState(ctx context.Context) (alerts.PersistentState, error) {
	var state alerts.PersistentState
	var raw []byte
	err := db.sql.QueryRowContext(ctx, `SELECT state_json FROM controller_alert_state WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("store: read alert state: %w", err)
	}
	if len(raw) > 2097152 || json.Unmarshal(raw, &state) != nil {
		return state, errors.New("store: alert state is invalid")
	}
	if err := alerts.ValidatePersistentState(state); err != nil {
		return state, err
	}
	return state, nil
}

func (db *DB) SaveAlertState(ctx context.Context, state alerts.PersistentState) error {
	if err := alerts.ValidatePersistentState(state); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return errors.New("store: alert state could not be encoded")
	}
	if len(raw) > 2097152 {
		return errors.New("store: alert state exceeds its size limit")
	}
	_, err = db.sql.ExecContext(ctx, `INSERT INTO controller_alert_state(id,state_json) VALUES(1,?)
 ON CONFLICT(id) DO UPDATE SET state_json=excluded.state_json`, raw)
	if err != nil {
		return fmt.Errorf("store: save alert state: %w", err)
	}
	return nil
}

// PausePortableRestoreAlertDelivery applies only to the disposable restored
// database. Copying a controller must not copy permission to send notifications.
func (db *DB) PausePortableRestoreAlertDelivery(ctx context.Context) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.validateRecoveryAlerts(ctx, tx); err != nil {
		return err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT state_json FROM controller_alert_state WHERE id=1`).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	} else if err != nil {
		return err
	}
	var state alerts.PersistentState
	if err := json.Unmarshal(raw, &state); err != nil {
		return errors.New("store: restored alert state is invalid")
	}
	state.Delivery.Enabled, state.EvaluatedAt = false, nil
	for _, queued := range state.Queue {
		for i := range state.Incidents {
			if state.Incidents[i].ID == queued.Notification.Incident.ID {
				state.Incidents[i].DeliveryState = "cancelled"
				state.Incidents[i].DeliveryError = "Pending delivery cancelled by controller restore; no historical replay was sent"
			}
		}
	}
	state.Queue = nil
	for i := range state.Rules {
		rule := &state.Rules[i]
		rule.PendingSince, rule.LastEvaluation = 0, 0
		rule.Since, rule.Value, rule.ObservedAt = nil, nil, nil
		if rule.IncidentID == 0 {
			rule.LastEvidence = 0
		}
		rule.State, rule.Reason = "disabled", "Rule is disabled"
		if rule.Enabled {
			rule.State, rule.Reason = "unknown", "Waiting for fresh evaluation after controller restore; existing incidents remain open"
		}
	}
	if err := alerts.ValidatePersistentState(state); err != nil {
		return err
	}
	raw, err = json.Marshal(state)
	if err != nil || len(raw) > 2097152 {
		return errors.New("store: restored alert state could not be encoded")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE controller_alert_state SET state_json=? WHERE id=1`, raw); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) validateRecoveryAlerts(ctx context.Context, q siteReader) error {
	var size int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(length(state_json)),0) FROM controller_alert_state`).Scan(&size); err != nil || size > 2097152 {
		return errors.New("stored alert state bounds could not be verified")
	}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT state_json FROM controller_alert_state WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return errors.New("stored alert state could not be read")
	}
	var state alerts.PersistentState
	if json.Unmarshal(raw, &state) != nil || alerts.ValidatePersistentState(state) != nil {
		return errors.New("stored alert state failed validation")
	}
	if len(state.DestinationCiphertext) == 0 {
		return nil
	}
	plain, err := db.protector.Unseal(state.DestinationCiphertext, []byte(alerts.DestinationContext))
	if err != nil {
		return errors.New("stored alert destination failed authentication")
	}
	defer clear(plain)
	if err := alerts.ValidateDestination(plain, state.Delivery.Host); err != nil {
		return errors.New("stored alert destination failed validation")
	}
	return nil
}
