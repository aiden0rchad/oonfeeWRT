package store

import (
	"context"
	"fmt"
)

// A note an operator wrote about a wireless section this controller did not
// create.
//
// The point of the table is what it does NOT hold. There is no copy of the
// section's values and no copy of its passphrase — nothing to leak, and nothing
// that could later be restored on top of whatever the operator has since done
// to their own device. It records only that a human looked and decided, which
// is enough to stop the controller reporting a settled question as an open one.
type ForeignNote struct {
	DeviceID  int64
	Section   string
	SSID      string
	Note      string
	DecidedAt int64
	DecidedBy string
}

// SetForeignNote records (or replaces) the decision about one section.
func (db *DB) SetForeignNote(ctx context.Context, n ForeignNote) error {
	if n.Section == "" {
		return fmt.Errorf("store: a foreign note needs a section")
	}
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO foreign_ssid_notes (device_id, section, ssid, note, decided_at, decided_by)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, section) DO UPDATE SET
  ssid=excluded.ssid, note=excluded.note,
  decided_at=excluded.decided_at, decided_by=excluded.decided_by`,
		n.DeviceID, n.Section, n.SSID, n.Note, n.DecidedAt, n.DecidedBy)
	if err != nil {
		return fmt.Errorf("store: record foreign note: %w", err)
	}
	return nil
}

// ClearForeignNote drops one decision, so the section goes back to being an
// open question rather than a settled one.
func (db *DB) ClearForeignNote(ctx context.Context, deviceID int64, section string) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM foreign_ssid_notes WHERE device_id=? AND section=?`,
		deviceID, section)
	if err != nil {
		return fmt.Errorf("store: clear foreign note: %w", err)
	}
	return nil
}

// ForeignNotes returns the decisions recorded for one device, keyed by section.
func (db *DB) ForeignNotes(ctx context.Context, deviceID int64) (map[string]ForeignNote, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT section, ssid, note, decided_at, decided_by
  FROM foreign_ssid_notes WHERE device_id=?`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: read foreign notes: %w", err)
	}
	defer rows.Close()
	out := map[string]ForeignNote{}
	for rows.Next() {
		n := ForeignNote{DeviceID: deviceID}
		if err := rows.Scan(&n.Section, &n.SSID, &n.Note, &n.DecidedAt, &n.DecidedBy); err != nil {
			return nil, fmt.Errorf("store: scan foreign note: %w", err)
		}
		out[n.Section] = n
	}
	return out, rows.Err()
}

// ForgetForeignNotes drops every decision for a device.
//
// The ON DELETE CASCADE already covers this. Called explicitly anyway, for the
// same reason ForgetOwned is: sqlite reuses a freed INTEGER PRIMARY KEY, and a
// claim that survives its device is one the next device inherits.
func (db *DB) ForgetForeignNotes(ctx context.Context, deviceID int64) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM foreign_ssid_notes WHERE device_id=?`, deviceID)
	if err != nil {
		return fmt.Errorf("store: forget foreign notes: %w", err)
	}
	return nil
}
