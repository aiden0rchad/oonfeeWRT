package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type CapabilityServiceState struct {
	Name                 string   `json:"name"`
	WasEnabled           bool     `json:"was_enabled"`
	WasRunning           bool     `json:"was_running"`
	ConfigBaseline       string   `json:"config_baseline,omitempty"`
	ConfigApplied        string   `json:"config_applied,omitempty"`
	ConfiguredInterfaces []string `json:"configured_interfaces,omitempty"`
}

func (db *DB) UpdateCapabilityServices(ctx context.Context, deviceID int64, capability string, services []CapabilityServiceState) error {
	servicesJSON, err := json.Marshal(services)
	if err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE device_capability_installs SET services_json=?, updated_at=?
 WHERE device_id=? AND capability=? AND state IN ('installed','removing','error')`,
		servicesJSON, time.Now().Unix(), deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: update device capability services: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func (db *DB) UpdateCapabilityAddedPackages(ctx context.Context, deviceID int64, capability string, added []string) error {
	addedJSON, err := json.Marshal(added)
	if err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE device_capability_installs SET added_packages_json=?, updated_at=?
 WHERE device_id=? AND capability=? AND state IN ('installing','error')`,
		addedJSON, time.Now().Unix(), deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: update device capability added packages: %w", err)
	}
	return requireOneCapabilityRow(res)
}

type CapabilityInstall struct {
	DeviceID          int64
	Capability        string
	PackageManager    string
	RequestedPackages []string
	BaselinePackages  []string
	AddedPackages     []string
	Services          []CapabilityServiceState
	State             string
	Detail            string
	InstalledAt       *time.Time
	UpdatedAt         time.Time
}

func (db *DB) CapabilityInstall(ctx context.Context, deviceID int64, capability string) (*CapabilityInstall, error) {
	var row CapabilityInstall
	var requested, baseline, added, services string
	var installedAt sql.NullInt64
	var updatedAt int64
	err := db.sql.QueryRowContext(ctx, `
SELECT device_id, capability, package_manager, requested_packages_json,
       baseline_packages_json, added_packages_json, services_json, state, detail, installed_at, updated_at
  FROM device_capability_installs WHERE device_id=? AND capability=?`,
		deviceID, capability).Scan(&row.DeviceID, &row.Capability, &row.PackageManager,
		&requested, &baseline, &added, &services, &row.State, &row.Detail, &installedAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read device capability install: %w", err)
	}
	if err := json.Unmarshal([]byte(requested), &row.RequestedPackages); err != nil {
		return nil, fmt.Errorf("store: decode requested capability packages: %w", err)
	}
	if err := json.Unmarshal([]byte(baseline), &row.BaselinePackages); err != nil {
		return nil, fmt.Errorf("store: decode baseline capability packages: %w", err)
	}
	if err := json.Unmarshal([]byte(added), &row.AddedPackages); err != nil {
		return nil, fmt.Errorf("store: decode added capability packages: %w", err)
	}
	if err := json.Unmarshal([]byte(services), &row.Services); err != nil {
		return nil, fmt.Errorf("store: decode capability services: %w", err)
	}
	if installedAt.Valid {
		at := time.Unix(installedAt.Int64, 0)
		row.InstalledAt = &at
	}
	row.UpdatedAt = time.Unix(updatedAt, 0)
	return &row, nil
}

func (db *DB) CapabilityInstalls(ctx context.Context, deviceID int64) ([]CapabilityInstall, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT capability FROM device_capability_installs WHERE device_id=? ORDER BY capability`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: list device capability installs: %w", err)
	}
	defer rows.Close()
	var capabilities []string
	for rows.Next() {
		var capability string
		if err := rows.Scan(&capability); err != nil {
			return nil, err
		}
		capabilities = append(capabilities, capability)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]CapabilityInstall, 0, len(capabilities))
	for _, capability := range capabilities {
		install, err := db.CapabilityInstall(ctx, deviceID, capability)
		if err != nil {
			return nil, err
		}
		if install != nil {
			out = append(out, *install)
		}
	}
	return out, nil
}

func (db *DB) BeginCapabilityInstall(ctx context.Context, deviceID int64, capability, manager string, packages, baseline []string, services []CapabilityServiceState) error {
	requested, err := json.Marshal(packages)
	if err != nil {
		return err
	}
	baselineJSON, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	servicesJSON, err := json.Marshal(services)
	if err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO device_capability_installs
       (device_id, capability, package_manager, requested_packages_json,
        baseline_packages_json, services_json, state, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'installing', ?)
ON CONFLICT(device_id, capability) DO UPDATE SET
	   state='installing', detail='', updated_at=excluded.updated_at
WHERE device_capability_installs.state='error'
  AND device_capability_installs.installed_at IS NULL
  AND device_capability_installs.package_manager=excluded.package_manager
  AND device_capability_installs.requested_packages_json=excluded.requested_packages_json`,
		deviceID, capability, manager, requested, baselineJSON, servicesJSON, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: begin device capability install: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func (db *DB) CompleteCapabilityInstall(ctx context.Context, deviceID int64, capability string, added []string, services []CapabilityServiceState) error {
	addedJSON, err := json.Marshal(added)
	if err != nil {
		return err
	}
	servicesJSON, err := json.Marshal(services)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
UPDATE device_capability_installs
   SET added_packages_json=?, services_json=?, state='installed', detail='',
       installed_at=?, updated_at=?
 WHERE device_id=? AND capability=? AND state='installing'`,
		addedJSON, servicesJSON, now, now, deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: complete device capability install: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func (db *DB) MarkCapabilityInstallError(ctx context.Context, deviceID int64, capability, detail string) error {
	res, err := db.sql.ExecContext(ctx, `
UPDATE device_capability_installs SET state='error', detail=?, updated_at=?
 WHERE device_id=? AND capability=?`, detail, time.Now().Unix(), deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: mark device capability install error: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func (db *DB) CompleteCapabilityConfiguration(ctx context.Context, deviceID int64, capability string) error {
	res, err := db.sql.ExecContext(ctx, `
UPDATE device_capability_installs SET state='installed', detail='', updated_at=?
 WHERE device_id=? AND capability=? AND state='error'`, time.Now().Unix(), deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: complete device capability configuration: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func (db *DB) BeginCapabilityRemoval(ctx context.Context, deviceID int64, capability string) error {
	res, err := db.sql.ExecContext(ctx, `
UPDATE device_capability_installs SET state='removing', detail='', updated_at=?
 WHERE device_id=? AND capability=? AND state IN ('installing','installed','removing','error')`,
		time.Now().Unix(), deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: begin device capability removal: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func (db *DB) CompleteCapabilityRemoval(ctx context.Context, deviceID int64, capability string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM device_capability_installs
WHERE device_id=? AND capability=? AND state='removing'`, deviceID, capability)
	if err != nil {
		return fmt.Errorf("store: complete device capability removal: %w", err)
	}
	return requireOneCapabilityRow(res)
}

func requireOneCapabilityRow(result sql.Result) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("store: capability state changed concurrently")
	}
	return nil
}
