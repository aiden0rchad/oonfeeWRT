package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func (db *DB) portRolesOn(ctx context.Context, q siteReader) ([]model.PortRole, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT device_id, port, mode, network_id FROM port_roles
		  ORDER BY device_id, port`)
	if err != nil {
		return nil, fmt.Errorf("store: list LAN port roles: %w", err)
	}
	defer rows.Close()
	var out []model.PortRole
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var role model.PortRole
		var mode string
		var networkID sql.NullInt64
		if err := rows.Scan(&role.DeviceID, &role.Port, &mode, &networkID); err != nil {
			return nil, err
		}
		role.Mode = model.PortRoleMode(mode)
		if networkID.Valid {
			role.NetworkID = int(networkID.Int64)
		}
		out = append(out, role)
	}
	return out, rows.Err()
}

// SavePortRole inserts or replaces one LAN port role.
//
// Trunk stores a row rather than deleting one. Absence means "the renderer
// has never been asked about this port" and must not edit VLAN 1. An explicit
// trunk is how a port that was an access port gets back onto VLAN 1.
func (db *DB) SavePortRole(ctx context.Context, role model.PortRole) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if err := role.ValidateShape(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	var networkID any
	if role.Mode == model.PortRoleAccess {
		var vlan, enabled int
		err := db.sql.QueryRowContext(ctx,
			`SELECT vlan, enabled FROM networks WHERE id=?`, role.NetworkID).
			Scan(&vlan, &enabled)
		if err == sql.ErrNoRows {
			return fmt.Errorf("store: network %d does not exist", role.NetworkID)
		}
		if err != nil {
			return fmt.Errorf("store: read network %d: %w", role.NetworkID, err)
		}
		if enabled == 0 {
			return fmt.Errorf("store: network %d is disabled, so it cannot be an access network yet", role.NetworkID)
		}
		if vlan <= 1 {
			return fmt.Errorf("store: network %d is VLAN %d, and VLAN 1 is the existing management LAN", role.NetworkID, vlan)
		}
		networkID = role.NetworkID
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO port_roles (device_id, port, mode, network_id) VALUES (?,?,?,?)
		 ON CONFLICT(device_id, port) DO UPDATE SET
		   mode = excluded.mode, network_id = excluded.network_id`,
		role.DeviceID, role.Port, string(role.Mode), networkID); err != nil {
		return fmt.Errorf("store: save LAN port role for device %d port %s: %w",
			role.DeviceID, role.Port, err)
	}
	return nil
}
