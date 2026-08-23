package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
)

const (
	diagnosticTextLimit       = 2 << 10
	diagnosticDeviceLimit     = 256
	diagnosticSourceLimit     = 1024
	diagnosticIdentifierLimit = 2048
	diagnosticInstallLimit    = 64
	diagnosticEventLimit      = 501
	diagnosticCapsLimit       = 64 << 10
)

// DiagnosticControllerState is bounded, read-only controller evidence for a
// support bundle. It intentionally excludes database contents and paths.
type DiagnosticControllerState struct {
	Schema         int
	Health         string
	MigrationState string
	IntegrityState string
	Gaps           []string
}

// DiagnosticDevice is the non-secret subset of a managed device row.
type DiagnosticDevice struct {
	ID              int64
	Identifier      string
	Name            string
	Host            string
	Model           string
	Target          string
	Firmware        string
	Kernel          string
	PackageManager  string
	CapabilityState string
	LastObservedAt  time.Time
	Gaps            []string
}

// DiagnosticSource is a bounded summary of stored collection evidence.
type DiagnosticSource struct {
	Kind             string
	DeviceIdentifier string
	State            string
	Detail           string
	ObservedAt       time.Time
}

type DiagnosticIdentifier struct {
	Kind  string
	Value string
}

type DiagnosticEvent struct {
	TS       int64
	DeviceID *int64
	Category string
	Severity string
	Event    string
	Source   string
	Action   string
}

func (db *DB) DiagnosticController(ctx context.Context) (DiagnosticControllerState, error) {
	var out DiagnosticControllerState
	var err error
	out.Schema, err = currentSchema(ctx, db.sql)
	if err != nil {
		return out, err
	}
	out.MigrationState = "current"
	if out.Schema != schemaVersion {
		out.MigrationState = "mismatch"
		out.Gaps = append(out.Gaps, "controller schema does not match this build")
	}

	var quick string
	if err := db.sql.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&quick); err != nil {
		return out, fmt.Errorf("store: diagnostics integrity check: %w", err)
	}
	out.IntegrityState = "ok"
	if quick != "ok" {
		out.IntegrityState = "failed"
		out.Gaps = append(out.Gaps, "database quick check did not pass")
	}

	rows, err := db.sql.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return out, fmt.Errorf("store: diagnostics foreign-key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		out.IntegrityState = "failed"
		out.Gaps = append(out.Gaps, "database foreign-key check did not pass")
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: diagnostics foreign-key check: %w", err)
	}
	out.Health = "healthy"
	if out.MigrationState != "current" || out.IntegrityState != "ok" {
		out.Health = "degraded"
	}
	return out, nil
}

func (db *DB) DiagnosticDevices(ctx context.Context, limit int) ([]DiagnosticDevice, bool, error) {
	if limit <= 0 || limit > diagnosticDeviceLimit {
		return nil, false, errors.New("store: diagnostics device limit is out of range")
	}
	rows, err := db.sql.QueryContext(ctx, `
WITH selected AS (
	SELECT id,
	       CAST(substr(CAST(mac AS BLOB),1,2048) AS TEXT) AS mac,
	       CAST(substr(CAST(name AS BLOB),1,2048) AS TEXT) AS name,
	       CAST(substr(CAST(host AS BLOB),1,2048) AS TEXT) AS host,
	       CAST(substr(CAST(caps_json AS BLOB),1,?) AS TEXT) AS caps_json,
	       COALESCE(CAST(substr(CAST(COALESCE(fw_release,'') AS BLOB),1,2048) AS TEXT),'') AS fw_release,
	       last_seen,
	       (length(CAST(mac AS BLOB))>2048 OR length(CAST(name AS BLOB))>2048 OR
	        length(CAST(host AS BLOB))>2048 OR length(CAST(COALESCE(fw_release,'') AS BLOB))>2048) AS text_truncated,
	       length(CAST(caps_json AS BLOB))>? AS caps_truncated
   FROM devices ORDER BY id LIMIT ?
), bounded_installs AS (
	SELECT i.device_id,
	       COALESCE(CAST(substr(CAST(COALESCE(i.capability,'') AS BLOB),1,2048) AS TEXT),'') AS capability,
	       COALESCE(CAST(substr(CAST(COALESCE(i.package_manager,'') AS BLOB),1,2048) AS TEXT),'') AS package_manager,
	       COALESCE(CAST(substr(CAST(COALESCE(i.state,'') AS BLOB),1,2048) AS TEXT),'') AS state,
	       (length(CAST(COALESCE(i.capability,'') AS BLOB))>2048 OR
	        length(CAST(COALESCE(i.package_manager,'') AS BLOB))>2048 OR
	        length(CAST(COALESCE(i.state,'') AS BLOB))>2048) AS text_truncated
	  FROM device_capability_installs i JOIN selected s ON s.id=i.device_id
), ranked_installs AS (
	SELECT *,ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY capability) AS install_rank
	  FROM bounded_installs
)
SELECT s.id,s.mac,s.name,s.host,s.caps_json,s.fw_release,s.last_seen,s.text_truncated,s.caps_truncated,
		COALESCE(i.capability,''),COALESCE(i.package_manager,''),COALESCE(i.state,''),
		COALESCE(i.text_truncated,0),COALESCE(i.install_rank,0)
  FROM selected s
  LEFT JOIN ranked_installs i ON i.device_id=s.id AND i.install_rank<=?
 ORDER BY s.id,i.capability`, diagnosticCapsLimit, diagnosticCapsLimit, limit+1, diagnosticInstallLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: diagnostics devices: %w", err)
	}
	defer rows.Close()

	var out []DiagnosticDevice
	var current *DiagnosticDevice
	var capsJSON, fallbackFirmware string
	var lastSeen sql.NullInt64
	var installs []string
	var managers map[string]struct{}
	var installsTruncated, installTextTruncated bool
	var textTruncated, capsTruncated bool
	finish := func() {
		if current == nil {
			return
		}
		populateDiagnosticDevice(current, capsJSON, fallbackFirmware, lastSeen, installs, managers, capsTruncated)
		if textTruncated {
			current.Gaps = append(current.Gaps, "device text evidence was truncated")
		}
		if installsTruncated {
			current.Gaps = append(current.Gaps, "capability installation evidence was truncated")
		}
		if installTextTruncated {
			current.Gaps = append(current.Gaps, "capability installation text evidence was truncated")
		}
		out = append(out, *current)
	}
	for rows.Next() {
		var id int64
		var mac, name, host, caps, firmware, install, manager, state string
		var installRank int
		var rowTextTruncated, rowCapsTruncated, rowInstallTextTruncated bool
		var seen sql.NullInt64
		if err := rows.Scan(&id, &mac, &name, &host, &caps, &firmware, &seen,
			&rowTextTruncated, &rowCapsTruncated,
			&install, &manager, &state, &rowInstallTextTruncated, &installRank); err != nil {
			return nil, false, err
		}
		if current == nil || current.ID != id {
			finish()
			current = &DiagnosticDevice{ID: id, Identifier: boundedDiagnosticText(mac),
				Name: boundedDiagnosticText(name), Host: boundedDiagnosticText(host)}
			capsJSON, fallbackFirmware, lastSeen = caps, firmware, seen
			installs = nil
			managers = map[string]struct{}{}
			installsTruncated, installTextTruncated = false, false
			textTruncated, capsTruncated = rowTextTruncated, rowCapsTruncated
		}
		if installRank > diagnosticInstallLimit {
			installsTruncated = true
		} else if install != "" {
			installs = append(installs, "managed:"+install+"="+state)
		}
		if installRank <= diagnosticInstallLimit && manager != "" {
			managers[manager] = struct{}{}
		}
		installTextTruncated = installTextTruncated || rowInstallTextTruncated
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	finish()
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	return out, truncated, nil
}

func populateDiagnosticDevice(out *DiagnosticDevice, capsJSON, fallbackFirmware string,
	lastSeen sql.NullInt64, installs []string, managers map[string]struct{}, capsTruncated bool) {
	var registry capability.Registry
	features := []string{}
	if capsTruncated {
		out.Gaps = append(out.Gaps, "capability registry was truncated")
	} else if capsJSON == "" || capsJSON == "{}" {
		out.Gaps = append(out.Gaps, "capability registry is unavailable")
	} else if err := json.Unmarshal([]byte(capsJSON), &registry); err != nil {
		out.Gaps = append(out.Gaps, "capability registry is invalid")
	} else {
		out.Model = boundedDiagnosticText(registry.Board.Model)
		out.Target = boundedDiagnosticText(registry.Board.Target)
		out.Kernel = boundedDiagnosticText(registry.Board.Kernel)
		out.Firmware = boundedDiagnosticText(registry.Board.Release)
		for feature, state := range registry.Features {
			features = append(features, string(feature)+"="+state.String())
		}
	}
	if out.Firmware == "" {
		out.Firmware = boundedDiagnosticText(fallbackFirmware)
	}
	sort.Strings(features)
	sort.Strings(installs)
	out.CapabilityState = boundedDiagnosticText(strings.Join(append(features, installs...), ","))
	if out.CapabilityState == "" {
		out.CapabilityState = "unknown"
	}
	managerNames := make([]string, 0, len(managers))
	for manager := range managers {
		managerNames = append(managerNames, manager)
	}
	sort.Strings(managerNames)
	out.PackageManager = strings.Join(managerNames, ",")
	if out.PackageManager == "" {
		out.PackageManager = "unknown"
		out.Gaps = append(out.Gaps, "package manager has no stored installation evidence")
	}
	if lastSeen.Valid && lastSeen.Int64 > 0 {
		out.LastObservedAt = time.Unix(lastSeen.Int64, 0).UTC()
	}
}

func (db *DB) DiagnosticSources(ctx context.Context, limit int) ([]DiagnosticSource, bool, error) {
	if limit <= 0 || limit > diagnosticSourceLimit {
		return nil, false, errors.New("store: diagnostics source limit is out of range")
	}
	rows, err := db.sql.QueryContext(ctx, `
WITH bounded_sources(kind,device_identifier,state,detail,observed_at,input_truncated) AS (
	 SELECT 'topology-source:'||CAST(substr(CAST(s.source AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(d.mac AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(s.state AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(s.reason AS BLOB),1,2048) AS TEXT),s.observed_at,
	        (length(CAST(s.source AS BLOB))>2048 OR length(CAST(d.mac AS BLOB))>2048 OR
	         length(CAST(s.state AS BLOB))>2048 OR length(CAST(s.reason AS BLOB))>2048)
   FROM topology_source_states s JOIN devices d ON d.id=s.device_id
 UNION ALL
	 SELECT 'topology-edge',
	        CAST(substr(CAST(COALESCE(d.mac,'') AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(e.confidence AS BLOB),1,2048) AS TEXT),
	        'medium='||CAST(substr(CAST(e.medium AS BLOB),1,2048) AS TEXT)||
	        CASE WHEN COALESCE(e.parent_port,'')='' THEN '' ELSE '; port='||CAST(substr(CAST(e.parent_port AS BLOB),1,2048) AS TEXT) END,
	        e.last_seen,
	        (length(CAST(COALESCE(d.mac,'') AS BLOB))>2048 OR length(CAST(e.confidence AS BLOB))>2048 OR
	         length(CAST(e.medium AS BLOB))>2048 OR length(CAST(COALESCE(e.parent_port,'') AS BLOB))>2048)
   FROM topology_edges e LEFT JOIN devices d ON d.id=e.parent_device_id
 UNION ALL
	 SELECT 'radio-scan:'||CAST(substr(CAST(r.radio_key AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(d.mac AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(r.status AS BLOB),1,2048) AS TEXT),'stored scan status',
	        COALESCE(r.finished_at,r.started_at),
	        (length(CAST(r.radio_key AS BLOB))>2048 OR length(CAST(d.mac AS BLOB))>2048 OR
	         length(CAST(r.status AS BLOB))>2048)
   FROM radio_scans r JOIN devices d ON d.id=r.device_id
 UNION ALL
	 SELECT 'event-source:'||CAST(substr(CAST(c.source AS BLOB),1,2048) AS TEXT),
	        CAST(substr(CAST(d.mac AS BLOB),1,2048) AS TEXT),
	        CASE WHEN c.continuity_gap_at=0 THEN 'observed' ELSE 'gap' END,
	        CASE WHEN c.continuity_gap_at=0 THEN '' ELSE 'stored continuity gap' END,
	        c.updated_at,
	        (length(CAST(c.source AS BLOB))>2048 OR length(CAST(d.mac AS BLOB))>2048)
   FROM ingest_cursors c JOIN devices d ON d.id=c.device_id
)
SELECT CAST(substr(CAST(kind AS BLOB),1,2048) AS TEXT),
	   CAST(substr(CAST(device_identifier AS BLOB),1,2048) AS TEXT),
	   CAST(substr(CAST(state AS BLOB),1,2048) AS TEXT),
	   CAST(substr(CAST(detail AS BLOB),1,2048) AS TEXT),observed_at,
	   (input_truncated OR length(CAST(kind AS BLOB))>2048 OR
	    length(CAST(device_identifier AS BLOB))>2048 OR length(CAST(state AS BLOB))>2048 OR
	    length(CAST(detail AS BLOB))>2048)
FROM bounded_sources ORDER BY observed_at DESC,kind,device_identifier LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: diagnostics sources: %w", err)
	}
	defer rows.Close()
	out := make([]DiagnosticSource, 0, limit)
	truncated := false
	for rows.Next() {
		var item DiagnosticSource
		var observed int64
		var textTruncated bool
		if err := rows.Scan(&item.Kind, &item.DeviceIdentifier, &item.State, &item.Detail, &observed, &textTruncated); err != nil {
			return nil, false, err
		}
		item.Kind = boundedDiagnosticText(item.Kind)
		item.DeviceIdentifier = boundedDiagnosticText(item.DeviceIdentifier)
		item.State = boundedDiagnosticText(item.State)
		item.Detail = boundedDiagnosticText(item.Detail)
		if observed > 0 {
			item.ObservedAt = time.UnixMilli(observed).UTC()
		}
		out = append(out, item)
		truncated = truncated || textTruncated
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

// DiagnosticIdentifiers returns only values needed to consistently
// pseudonymize support-bundle text. Notes, fixed addresses and hashes are not
// selected.
func (db *DB) DiagnosticIdentifiers(ctx context.Context, limit int) ([]DiagnosticIdentifier, bool, error) {
	if limit <= 0 || limit > diagnosticIdentifierLimit {
		return nil, false, errors.New("store: diagnostics identifier limit is out of range")
	}
	rows, err := db.sql.QueryContext(ctx, `
WITH raw_inventory(priority,kind,raw_value) AS (
	 SELECT 0,'site',name FROM site
	 UNION ALL SELECT 1,'network',name FROM networks
	 UNION ALL SELECT 1,'zone',zone FROM networks
	 UNION ALL SELECT 2,'group',name FROM ap_groups
	 UNION ALL SELECT 3,'wlan',ssid FROM wlans
	 UNION ALL SELECT 4,'mesh',mesh_id FROM meshes
	 UNION ALL SELECT 5,'zone',name FROM zones
	 UNION ALL SELECT 6,'wlan',ssid FROM foreign_ssid_notes
	 UNION ALL SELECT 10,'account',username FROM admins
	 UNION ALL SELECT 20,'client',mac FROM clients
	 UNION ALL SELECT 20,'client',name FROM clients
	 UNION ALL SELECT 20,'address',ip FROM clients
), inventory(priority,kind,value,text_truncated) AS (
	 SELECT priority,kind,
	        COALESCE(CAST(substr(CAST(COALESCE(raw_value,'') AS BLOB),1,2048) AS TEXT),''),
	        length(CAST(COALESCE(raw_value,'') AS BLOB))>2048
	 FROM raw_inventory
), unique_inventory AS (
	 SELECT MIN(priority) AS priority,kind,value,MAX(text_truncated) AS text_truncated FROM inventory
	 WHERE COALESCE(value,'')!='' GROUP BY kind,value
)
SELECT kind,value,text_truncated
FROM unique_inventory ORDER BY priority,kind,value LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: diagnostics identifiers: %w", err)
	}
	defer rows.Close()
	out := make([]DiagnosticIdentifier, 0, limit)
	truncated := false
	for rows.Next() {
		var item DiagnosticIdentifier
		var textTruncated bool
		if err := rows.Scan(&item.Kind, &item.Value, &textTruncated); err != nil {
			return nil, false, err
		}
		item.Kind = boundedDiagnosticText(item.Kind)
		item.Value = boundedDiagnosticText(item.Value)
		out = append(out, item)
		truncated = truncated || textTruncated
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

// DiagnosticEvents projects only bounded, non-detail fields. detail_json can
// contain historical names and credential metadata, so it is never selected.
func (db *DB) DiagnosticEvents(ctx context.Context, scope string, limit int) ([]DiagnosticEvent, bool, error) {
	if (scope != "general" && scope != "audit") || limit <= 0 || limit > diagnosticEventLimit {
		return nil, false, errors.New("store: diagnostics event query is out of range")
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT ts,device_id,
       CAST(substr(CAST(category AS BLOB),1,2048) AS TEXT),
       CAST(substr(CAST(severity AS BLOB),1,2048) AS TEXT),
       CAST(substr(CAST(event AS BLOB),1,2048) AS TEXT),
		CAST(substr(CAST(source AS BLOB),1,2048) AS TEXT),
		COALESCE(CAST(substr(CAST(COALESCE(action,'') AS BLOB),1,2048) AS TEXT),'')
  FROM events
 WHERE (?='audit' AND category='audit') OR (?='general' AND category!='audit')
 ORDER BY ts DESC,id DESC LIMIT ?`, scope, scope, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: diagnostics events: %w", err)
	}
	defer rows.Close()
	out := make([]DiagnosticEvent, 0, limit)
	truncated := false
	for rows.Next() {
		var item DiagnosticEvent
		var deviceID sql.NullInt64
		if err := rows.Scan(&item.TS, &deviceID, &item.Category, &item.Severity,
			&item.Event, &item.Source, &item.Action); err != nil {
			return nil, false, err
		}
		if deviceID.Valid {
			value := deviceID.Int64
			item.DeviceID = &value
		}
		item.Category = boundedDiagnosticText(item.Category)
		item.Severity = boundedDiagnosticText(item.Severity)
		item.Event = boundedDiagnosticText(item.Event)
		item.Source = boundedDiagnosticText(item.Source)
		item.Action = boundedDiagnosticText(item.Action)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

func boundedDiagnosticText(value string) string {
	value = strings.ToValidUTF8(value, "?")
	if len(value) <= diagnosticTextLimit {
		return value
	}
	end := diagnosticTextLimit
	for !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
