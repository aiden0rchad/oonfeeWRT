package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

const upsertTopologySourceStateSQL = `
INSERT INTO topology_source_states (device_id, source, state, reason, observed_at)
VALUES (?,?,?,?,?)
ON CONFLICT(device_id, source) DO UPDATE SET
 state=excluded.state, reason=excluded.reason, observed_at=excluded.observed_at`

// TopologyChanges is the store-facing result of interval reconciliation. New
// intervals, continuing intervals, and closures are explicit so persistence
// can reject an accidental reopen or insert before changing durable state.
type TopologyChanges struct {
	Open              []model.TopologyEdge
	Update            []model.TopologyEdge
	Close             []model.TopologyEdge
	ReplaceSourcesFor []int64
}

type preparedTopologyEdge struct {
	edge                  model.TopologyEdge
	evidence, ambiguities string
}

// ApplyTopologyObservation persists one reconciled topology observation and
// its source outcomes atomically. All input is validated before the first
// write; a stale interval ID or any SQL failure rolls the whole observation
// back.
func (db *DB) ApplyTopologyObservation(ctx context.Context, changes TopologyChanges,
	sources []model.TopologySourceObservation) error {
	prepared := struct {
		open, update, close []preparedTopologyEdge
	}{}
	seenIDs := make(map[int64]string, len(changes.Update)+len(changes.Close))
	prepare := func(kind string, edges []model.TopologyEdge, dst *[]preparedTopologyEdge) error {
		for i := range edges {
			edge := edges[i]
			switch kind {
			case "open":
				if edge.ID != 0 || edge.ValidTo != nil {
					return fmt.Errorf("store: topology open %d must be a new active interval", i)
				}
			case "update":
				if edge.ID <= 0 || edge.ValidTo != nil {
					return fmt.Errorf("store: topology update %d must identify an active interval", i)
				}
			case "close":
				if edge.ID <= 0 || edge.ValidTo == nil {
					return fmt.Errorf("store: topology close %d must identify a closed interval", i)
				}
			}
			if edge.ID > 0 {
				if prior, duplicate := seenIDs[edge.ID]; duplicate {
					return fmt.Errorf("store: topology edge %d appears in both %s and %s", edge.ID, prior, kind)
				}
				seenIDs[edge.ID] = kind
			}
			if err := normalizeTopologyEdge(&edge); err != nil {
				return fmt.Errorf("store: topology %s %d: %w", kind, i, err)
			}
			if err := db.validateTopologyParent(ctx, edge.ParentDeviceID, edge.ParentNode); err != nil {
				return fmt.Errorf("store: topology %s %d: %w", kind, i, err)
			}
			evidence, ambiguities, err := topologyEdgeJSON(&edge)
			if err != nil {
				return fmt.Errorf("store: topology %s %d: %w", kind, i, err)
			}
			*dst = append(*dst, preparedTopologyEdge{edge: edge, evidence: evidence, ambiguities: ambiguities})
		}
		return nil
	}
	if err := prepare("open", changes.Open, &prepared.open); err != nil {
		return err
	}
	if err := prepare("update", changes.Update, &prepared.update); err != nil {
		return err
	}
	if err := prepare("close", changes.Close, &prepared.close); err != nil {
		return err
	}

	normalizedSources := make([]model.TopologySourceObservation, len(sources))
	seenSources := make(map[string]bool, len(sources))
	for i := range sources {
		normalizedSources[i] = sources[i]
		if err := normalizeTopologySourceState(&normalizedSources[i]); err != nil {
			return fmt.Errorf("store: topology source %d: %w", i, err)
		}
		key := fmt.Sprintf("%d\x00%s", normalizedSources[i].DeviceID, normalizedSources[i].Source)
		if seenSources[key] {
			return fmt.Errorf("store: duplicate topology source %d/%s", normalizedSources[i].DeviceID, normalizedSources[i].Source)
		}
		seenSources[key] = true
	}
	replaceDevices := map[int64]bool{}
	for _, deviceID := range changes.ReplaceSourcesFor {
		if deviceID <= 0 || replaceDevices[deviceID] {
			return errors.New("store: topology source replacement needs unique positive device ids")
		}
		replaceDevices[deviceID] = true
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, edge := range prepared.close {
		if err := updateActiveTopologyEdge(ctx, tx, edge); err != nil {
			return err
		}
	}
	for _, edge := range prepared.update {
		if err := updateActiveTopologyEdge(ctx, tx, edge); err != nil {
			return err
		}
	}
	for _, edge := range prepared.open {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO topology_edges (
 child_node, child_mac, parent_node, parent_device_id, parent_port,
 medium, confidence, valid_from, valid_to, last_seen, evidence_json, ambiguity_json
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, topologyEdgeArgs(edge)...); err != nil {
			return fmt.Errorf("store: open topology edge: %w", err)
		}
	}
	for deviceID := range replaceDevices {
		names := []string{}
		for _, source := range normalizedSources {
			if source.DeviceID == deviceID {
				names = append(names, source.Source)
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("store: topology source replacement for device %d has no observations", deviceID)
		}
		args := make([]any, 1, len(names)+1)
		args[0] = deviceID
		for _, name := range names {
			args = append(args, name)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM topology_source_states
WHERE device_id=? AND source NOT IN (`+placeholders(len(names))+`)`, args...); err != nil {
			return fmt.Errorf("store: replace topology source states: %w", err)
		}
	}
	for _, source := range normalizedSources {
		if _, err := tx.ExecContext(ctx, upsertTopologySourceStateSQL,
			source.DeviceID, source.Source, source.State, source.Reason, source.ObservedAt); err != nil {
			return fmt.Errorf("store: save topology source state: %w", err)
		}
	}
	return tx.Commit()
}

func topologyEdgeArgs(prepared preparedTopologyEdge) []any {
	edge := prepared.edge
	return []any{edge.ChildNode, nullString(edge.ChildMAC), edge.ParentNode,
		edge.ParentDeviceID, nullString(edge.ParentPort), edge.Medium, edge.Confidence,
		edge.ValidFrom, edge.ValidTo, edge.LastSeen, prepared.evidence, prepared.ambiguities}
}

func updateActiveTopologyEdge(ctx context.Context, tx *sql.Tx, prepared preparedTopologyEdge) error {
	args := append(topologyEdgeArgs(prepared), prepared.edge.ID)
	res, err := tx.ExecContext(ctx, `
UPDATE topology_edges SET
 child_node=?, child_mac=?, parent_node=?, parent_device_id=?, parent_port=?,
 medium=?, confidence=?, valid_from=?, valid_to=?, last_seen=?,
 evidence_json=?, ambiguity_json=?
WHERE id=? AND valid_to IS NULL`, args...)
	if err != nil {
		return fmt.Errorf("store: update active topology edge %d: %w", prepared.edge.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("store: active topology edge %d: %w", prepared.edge.ID, ErrNotFound)
	}
	return nil
}

// SaveTopologyEdge persists one graph interval. The inference layer decides
// when an interval changes; this boundary enforces stable identities and keeps
// malformed evidence out of durable/API state.
func (db *DB) SaveTopologyEdge(ctx context.Context, edge *model.TopologyEdge) error {
	if edge == nil {
		return errors.New("store: topology edge is required")
	}
	if err := normalizeTopologyEdge(edge); err != nil {
		return err
	}
	if err := db.validateTopologyParent(ctx, edge.ParentDeviceID, edge.ParentNode); err != nil {
		return err
	}
	evidenceJSON, ambiguityJSON, err := topologyEdgeJSON(edge)
	if err != nil {
		return err
	}
	if edge.ID == 0 {
		res, err := db.sql.ExecContext(ctx, `
INSERT INTO topology_edges (
  child_node, child_mac, parent_node, parent_device_id, parent_port,
  medium, confidence, valid_from, valid_to, last_seen, evidence_json, ambiguity_json
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			edge.ChildNode, nullString(edge.ChildMAC), edge.ParentNode, edge.ParentDeviceID,
			nullString(edge.ParentPort), edge.Medium, edge.Confidence, edge.ValidFrom,
			edge.ValidTo, edge.LastSeen, evidenceJSON, ambiguityJSON)
		if err != nil {
			return fmt.Errorf("store: insert topology edge: %w", err)
		}
		edge.ID, err = res.LastInsertId()
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE topology_edges SET
 child_node=?, child_mac=?, parent_node=?, parent_device_id=?, parent_port=?,
 medium=?, confidence=?, valid_from=?, valid_to=?, last_seen=?,
 evidence_json=?, ambiguity_json=?
WHERE id=?`,
		edge.ChildNode, nullString(edge.ChildMAC), edge.ParentNode, edge.ParentDeviceID,
		nullString(edge.ParentPort), edge.Medium, edge.Confidence, edge.ValidFrom,
		edge.ValidTo, edge.LastSeen, evidenceJSON, ambiguityJSON, edge.ID)
	if err != nil {
		return fmt.Errorf("store: update topology edge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizeTopologyEdge(edge *model.TopologyEdge) error {
	if !validTopologyNode(edge.ChildNode) || !validTopologyNode(edge.ParentNode) {
		return errors.New("store: topology node refs must be device:<inventory-mac>, client:<mac>, mac:<mac>, or synthetic:internet")
	}
	if edge.ChildMAC != "" {
		mac, err := canonicalMAC(edge.ChildMAC)
		if err != nil {
			return fmt.Errorf("store: topology child MAC: %w", err)
		}
		edge.ChildMAC = mac
	}
	if edge.ParentDeviceID != nil {
		if *edge.ParentDeviceID <= 0 {
			return errors.New("store: topology parent device id must be positive")
		}
	}
	if strings.TrimSpace(edge.Medium) == "" || strings.TrimSpace(edge.Confidence) == "" {
		return errors.New("store: topology medium and confidence are required")
	}
	if edge.ValidFrom <= 0 || edge.LastSeen < edge.ValidFrom ||
		(edge.ValidTo != nil && (*edge.ValidTo < edge.ValidFrom || edge.LastSeen > *edge.ValidTo)) {
		return errors.New("store: invalid topology interval")
	}
	if edge.Evidence == nil {
		edge.Evidence = []model.TopologyEvidence{}
	}
	for i := range edge.Evidence {
		evidence := &edge.Evidence[i]
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Source) == "" {
			return fmt.Errorf("store: topology evidence %d requires kind and source", i)
		}
		if evidence.DeviceID != nil && *evidence.DeviceID <= 0 {
			return fmt.Errorf("store: topology evidence %d has invalid device id", i)
		}
		if evidence.Detail == nil {
			evidence.Detail = map[string]any{}
		}
	}
	if edge.Ambiguities == nil {
		edge.Ambiguities = []string{}
	}
	for i, ambiguity := range edge.Ambiguities {
		if strings.TrimSpace(ambiguity) == "" {
			return fmt.Errorf("store: topology ambiguity %d is blank", i)
		}
	}
	return nil
}

func topologyEdgeJSON(edge *model.TopologyEdge) (string, string, error) {
	evidence, err := json.Marshal(edge.Evidence)
	if err != nil {
		return "", "", fmt.Errorf("store: encode topology evidence: %w", err)
	}
	ambiguities, err := json.Marshal(edge.Ambiguities)
	if err != nil {
		return "", "", fmt.Errorf("store: encode topology ambiguities: %w", err)
	}
	return string(evidence), string(ambiguities), nil
}

func validTopologyNode(ref string) bool {
	if ref == "synthetic:internet" {
		return true
	}
	kind, value, ok := strings.Cut(ref, ":")
	if !ok || value == "" {
		return false
	}
	switch kind {
	case "device":
		fallthrough
	case "client", "mac":
		mac, err := canonicalMAC(value)
		return err == nil && value == mac
	default:
		return false
	}
}

func (db *DB) validateTopologyParent(ctx context.Context, deviceID *int64, parentNode string) error {
	if deviceID == nil {
		return nil
	}
	var rawMAC string
	if err := db.sql.QueryRowContext(ctx, `SELECT mac FROM devices WHERE id=?`, *deviceID).
		Scan(&rawMAC); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	mac, err := canonicalMAC(rawMAC)
	if err != nil {
		return fmt.Errorf("store: malformed inventory MAC for topology parent: %w", err)
	}
	if parentNode != "device:"+mac {
		return errors.New("store: topology parent_device_id does not match the inventory MAC node")
	}
	return nil
}

// TopologyEdgesAt returns the current graph when at is zero, or the half-open
// historical intervals containing at otherwise.
func (db *DB) TopologyEdgesAt(ctx context.Context, at int64) ([]model.TopologyEdge, error) {
	query := `SELECT id, child_node, child_mac, parent_node, parent_device_id,
 parent_port, medium, confidence, valid_from, valid_to, last_seen,
 evidence_json, ambiguity_json FROM topology_edges WHERE valid_to IS NULL`
	args := []any{}
	if at > 0 {
		query = `SELECT id, child_node, child_mac, parent_node, parent_device_id,
 parent_port, medium, confidence, valid_from, valid_to, last_seen,
 evidence_json, ambiguity_json FROM topology_edges
 WHERE valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)`
		args = append(args, at, at)
	} else if at < 0 {
		return nil, errors.New("store: topology timestamp cannot be negative")
	}
	rows, err := db.sql.QueryContext(ctx, query+` ORDER BY child_node, id`, args...)
	if err != nil {
		return nil, err
	}
	return scanTopologyEdges(rows)
}

// TopologyEdgesBetween returns the newest bounded set of intervals intersecting
// [from,to), ordered chronologically for deterministic replay. truncated is
// explicit because silently dropping old intervals would turn a partial graph
// into a false complete history.
func (db *DB) TopologyEdgesBetween(ctx context.Context, from, to int64,
	limit int) ([]model.TopologyEdge, bool, error) {
	if from <= 0 || to <= from {
		return nil, false, errors.New("store: topology history requires 0 < from < to")
	}
	if limit <= 0 || limit > 100_000 {
		return nil, false, errors.New("store: topology history limit must be within 1..100000")
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT id, child_node, child_mac, parent_node, parent_device_id,
 parent_port, medium, confidence, valid_from, valid_to, last_seen,
 evidence_json, ambiguity_json FROM topology_edges
 WHERE valid_from < ? AND (valid_to IS NULL OR valid_to > ?)
 ORDER BY valid_from DESC, id DESC LIMIT ?`, to, from, limit+1)
	if err != nil {
		return nil, false, err
	}
	edges, err := scanTopologyEdges(rows)
	if err != nil {
		return nil, false, err
	}
	truncated := len(edges) > limit
	if truncated {
		edges = edges[:limit]
	}
	for left, right := 0, len(edges)-1; left < right; left, right = left+1, right-1 {
		edges[left], edges[right] = edges[right], edges[left]
	}
	return edges, truncated, nil
}

func scanTopologyEdges(rows *sql.Rows) ([]model.TopologyEdge, error) {
	defer rows.Close()
	out := []model.TopologyEdge{}
	for rows.Next() {
		var edge model.TopologyEdge
		var childMAC, parentPort sql.NullString
		var parentDevice, validTo sql.NullInt64
		var evidence, ambiguity string
		if err := rows.Scan(&edge.ID, &edge.ChildNode, &childMAC, &edge.ParentNode,
			&parentDevice, &parentPort, &edge.Medium, &edge.Confidence,
			&edge.ValidFrom, &validTo, &edge.LastSeen, &evidence, &ambiguity); err != nil {
			return nil, err
		}
		edge.ChildMAC, edge.ParentPort = childMAC.String, parentPort.String
		if parentDevice.Valid {
			v := parentDevice.Int64
			edge.ParentDeviceID = &v
		}
		if validTo.Valid {
			v := validTo.Int64
			edge.ValidTo = &v
		}
		if err := json.Unmarshal([]byte(evidence), &edge.Evidence); err != nil {
			return nil, fmt.Errorf("store: malformed topology edge %d evidence: %w", edge.ID, err)
		}
		if err := json.Unmarshal([]byte(ambiguity), &edge.Ambiguities); err != nil {
			return nil, fmt.Errorf("store: malformed topology edge %d ambiguities: %w", edge.ID, err)
		}
		if err := normalizeTopologyEdge(&edge); err != nil {
			return nil, fmt.Errorf("store: malformed topology edge %d: %w", edge.ID, err)
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// SaveTopologySourceState records whether a source answered with evidence,
// answered empty, failed, or has not yet been observed.
func (db *DB) SaveTopologySourceState(ctx context.Context, state model.TopologySourceObservation) error {
	if err := normalizeTopologySourceState(&state); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, upsertTopologySourceStateSQL,
		state.DeviceID, state.Source, state.State, state.Reason, state.ObservedAt)
	return err
}

func normalizeTopologySourceState(state *model.TopologySourceObservation) error {
	if state.DeviceID <= 0 || state.Source == "" || strings.TrimSpace(state.Source) != state.Source {
		return errors.New("store: topology source device and source are required")
	}
	switch state.State {
	case model.TopologySourceUnknown, model.TopologySourceEmpty,
		model.TopologySourceObserved, model.TopologySourceError:
	default:
		return fmt.Errorf("store: invalid topology source state %q", state.State)
	}
	if state.ObservedAt == 0 {
		state.ObservedAt = time.Now().UnixMilli()
	}
	if state.ObservedAt < 0 {
		return errors.New("store: topology source observation time cannot be negative")
	}
	return nil
}

func (db *DB) TopologySourceStates(ctx context.Context) ([]model.TopologySourceObservation, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT device_id, source, state, reason, observed_at
  FROM topology_source_states ORDER BY device_id, source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TopologySourceObservation{}
	for rows.Next() {
		var state model.TopologySourceObservation
		if err := rows.Scan(&state.DeviceID, &state.Source, &state.State,
			&state.Reason, &state.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, rows.Err()
}

// CreateRadioScan records an explicit scan before device work begins.
func (db *DB) CreateRadioScan(ctx context.Context, scan *model.RadioScan) error {
	if scan == nil {
		return errors.New("store: radio scan is required")
	}
	if err := validateRadioKey(scan.Radio); err != nil {
		return err
	}
	if scan.StartedAt == 0 {
		scan.StartedAt = time.Now().UnixMilli()
	}
	if scan.StartedAt < 0 || scan.FinishedAt != nil {
		return errors.New("store: a new radio scan needs a non-negative start and no finish time")
	}
	if scan.Status == "" {
		scan.Status = model.RadioScanRunning
	}
	if scan.Status != model.RadioScanPending && scan.Status != model.RadioScanRunning {
		return errors.New("store: a new radio scan must be pending or running")
	}
	detail, err := normalizedJSONObject(scan.Detail)
	if err != nil {
		return fmt.Errorf("store: radio scan detail: %w", err)
	}
	scan.Detail = detail
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO radio_scans
 (device_id, radio_key, started_at, finished_at, status, detail_json)
VALUES (?,?,?,?,?,?)`, scan.Radio.DeviceID, scan.Radio.Section, scan.StartedAt,
		scan.FinishedAt, scan.Status, string(scan.Detail))
	if err != nil {
		return fmt.Errorf("store: create radio scan: %w", err)
	}
	scan.ID, err = res.LastInsertId()
	return err
}

// FinishRadioScan atomically records the observations and terminal outcome.
func (db *DB) FinishRadioScan(ctx context.Context, scanID int64, status model.RadioScanStatus,
	finishedAt int64, detail json.RawMessage, observations []model.RadioScanBSS) error {
	if scanID <= 0 || (status != model.RadioScanCompleted && status != model.RadioScanFailed) {
		return errors.New("store: radio scan completion requires an id and terminal status")
	}
	if finishedAt == 0 {
		finishedAt = time.Now().UnixMilli()
	}
	detail, err := normalizedJSONObject(detail)
	if err != nil {
		return fmt.Errorf("store: radio scan detail: %w", err)
	}
	for i := range observations {
		observations[i].ScanID = scanID
		if err := normalizeRadioScanBSS(&observations[i]); err != nil {
			return fmt.Errorf("store: radio scan observation %d: %w", i, err)
		}
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var startedAt int64
	var current model.RadioScanStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT started_at, status FROM radio_scans WHERE id=?`, scanID).
		Scan(&startedAt, &current); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if current != model.RadioScanPending && current != model.RadioScanRunning {
		return errors.New("store: radio scan is already terminal")
	}
	if finishedAt < startedAt {
		return errors.New("store: radio scan finished before it started")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM radio_scan_bss WHERE scan_id=?`, scanID); err != nil {
		return err
	}
	for _, bss := range observations {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO radio_scan_bss (scan_id,bssid,ssid,mhz,channel,signal,width)
VALUES (?,?,?,?,?,?,?)`, scanID, bss.BSSID, bss.SSID, bss.MHz,
			bss.Channel, bss.Signal, bss.Width); err != nil {
			return fmt.Errorf("store: record radio scan BSS: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE radio_scans SET finished_at=?, status=?, detail_json=? WHERE id=?`,
		finishedAt, status, string(detail), scanID); err != nil {
		return err
	}
	return tx.Commit()
}

// RadioScanByID returns a scan and its observations.
func (db *DB) RadioScanByID(ctx context.Context, id int64) (model.RadioScan, []model.RadioScanBSS, error) {
	var scan model.RadioScan
	var finished sql.NullInt64
	var detail string
	err := db.sql.QueryRowContext(ctx, `
SELECT id,device_id,radio_key,started_at,finished_at,status,detail_json
  FROM radio_scans WHERE id=?`, id).Scan(&scan.ID, &scan.Radio.DeviceID,
		&scan.Radio.Section, &scan.StartedAt, &finished, &scan.Status, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return scan, nil, ErrNotFound
	}
	if err != nil {
		return scan, nil, err
	}
	if finished.Valid {
		v := finished.Int64
		scan.FinishedAt = &v
	}
	scan.Detail = json.RawMessage(detail)
	rows, err := db.sql.QueryContext(ctx, `
SELECT scan_id,bssid,ssid,mhz,channel,signal,width
  FROM radio_scan_bss WHERE scan_id=? ORDER BY signal DESC,bssid,mhz`, id)
	if err != nil {
		return scan, nil, err
	}
	defer rows.Close()
	observations := []model.RadioScanBSS{}
	for rows.Next() {
		var bss model.RadioScanBSS
		var signal, width sql.NullInt64
		if err := rows.Scan(&bss.ScanID, &bss.BSSID, &bss.SSID, &bss.MHz,
			&bss.Channel, &signal, &width); err != nil {
			return scan, nil, err
		}
		if signal.Valid {
			v := int(signal.Int64)
			bss.Signal = &v
		}
		if width.Valid {
			v := int(width.Int64)
			bss.Width = &v
		}
		observations = append(observations, bss)
	}
	return scan, observations, rows.Err()
}

func validateRadioKey(key model.RadioKey) error {
	if key.DeviceID <= 0 || !validUCIName(key.Section) {
		return errors.New("store: radio key requires a device and UCI wifi-device section")
	}
	return nil
}

func validUCIName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func normalizeRadioScanBSS(bss *model.RadioScanBSS) error {
	if bss.ScanID <= 0 || bss.MHz <= 0 || bss.Channel <= 0 {
		return errors.New("scan id, frequency and channel are required")
	}
	mac, err := canonicalMAC(bss.BSSID)
	if err != nil {
		return fmt.Errorf("BSSID: %w", err)
	}
	bss.BSSID = mac
	if bss.Width != nil && *bss.Width <= 0 {
		return errors.New("channel width must be positive when known")
	}
	return nil
}

func canonicalMAC(raw string) (string, error) {
	mac, err := net.ParseMAC(raw)
	if err != nil || len(mac) != 6 {
		return "", errors.New("invalid 48-bit MAC address")
	}
	return strings.ToLower(mac.String()), nil
}

func normalizedJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, errors.New("must be a JSON object")
	}
	return raw, nil
}

func normalizedJSONArray(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`[]`), nil
	}
	var value []any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, errors.New("must be a JSON array")
	}
	return raw, nil
}
