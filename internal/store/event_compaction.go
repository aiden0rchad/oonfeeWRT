package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var openWRTIPv6RANoDefaultRouteLog = regexp.MustCompile(
	`^odhcpd(?:\[[0-9]+\])?: No default route present, (?:setting|overriding) ra_lifetime(?: to 0)?!$`,
)

// IsOpenWRTIPv6RANoDefaultRouteLog identifies the exact odhcpd warning that is
// safe to condense. Severity and every near-match remain ordinary raw events.
func IsOpenWRTIPv6RANoDefaultRouteLog(priority uint32, message string) bool {
	return priority == 28 && openWRTIPv6RANoDefaultRouteLog.MatchString(message)
}

type ipv6RAConditionDetail struct {
	fields        map[string]json.RawMessage
	occurrences   int64
	firstSourceID uint32
	lastSourceID  uint32
}

type storedIPv6RACondition struct {
	id             int64
	fixedTextBytes int
	detail         ipv6RAConditionDetail
}

func loadIPv6RACondition(ctx context.Context, tx *sql.Tx, deviceID int64,
	sourceBoot string) (storedIPv6RACondition, bool, error) {
	var out storedIPv6RACondition
	var category, severity, event string
	var detailBytes int
	var detail []byte
	err := tx.QueryRowContext(ctx, `
SELECT id, category, severity, event,
       length(CAST(detail_json AS BLOB)),
       substr(CAST(detail_json AS BLOB), 1, 65537),
       length(CAST(COALESCE(category,'') AS BLOB)) +
       length(CAST(COALESCE(severity,'') AS BLOB)) +
       length(CAST(COALESCE(event,'') AS BLOB)) +
       length(CAST(COALESCE(source,'') AS BLOB)) +
       length(CAST(COALESCE(source_id,'') AS BLOB)) +
       length(CAST(COALESCE(source_boot,'') AS BLOB)) +
       length(CAST(COALESCE(client_mac,'') AS BLOB)) +
       length(CAST(COALESCE(action,'') AS BLOB)) +
       length(CAST(COALESCE(direction,'') AS BLOB)) +
       length(CAST(COALESCE(in_iface,'') AS BLOB)) +
       length(CAST(COALESCE(out_iface,'') AS BLOB)) +
       length(CAST(COALESCE(src_ip,'') AS BLOB)) +
       length(CAST(COALESCE(dst_ip,'') AS BLOB)) +
       length(CAST(COALESCE(zone_in,'') AS BLOB)) +
       length(CAST(COALESCE(zone_out,'') AS BLOB))
  FROM events
 WHERE device_id=? AND source='openwrt-logd' AND source_boot=? AND source_id=?`,
		deviceID, sourceBoot, EventOpenWRTIPv6RANoDefaultRouteSourceID).
		Scan(&out.id, &category, &severity, &event, &detailBytes, &detail, &out.fixedTextBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return storedIPv6RACondition{}, false, nil
	}
	if err != nil {
		return storedIPv6RACondition{}, false, fmt.Errorf("store: read IPv6 RA condition: %w", err)
	}
	if category != "system" || severity != "warning" ||
		event != EventOpenWRTIPv6RANoDefaultRoute {
		return storedIPv6RACondition{}, false,
			errors.New("store: IPv6 RA condition identity has unexpected event fields")
	}
	if detailBytes != len(detail) || detailBytes > maxEncodedEventBytes ||
		out.fixedTextBytes > maxEncodedEventBytes-detailBytes {
		return storedIPv6RACondition{}, false,
			errors.New("store: IPv6 RA condition exceeds the encoded storage limit")
	}
	parsed, err := decodeIPv6RAConditionDetail(detail)
	if err != nil {
		return storedIPv6RACondition{}, false, err
	}
	out.detail = parsed
	return out, true, nil
}

func decodeIPv6RAConditionDetail(raw []byte) (ipv6RAConditionDetail, error) {
	var out ipv6RAConditionDetail
	if len(raw) == 0 || len(raw) > maxEncodedEventBytes ||
		json.Unmarshal(raw, &out.fields) != nil || out.fields == nil {
		return out, errors.New("store: IPv6 RA condition has invalid detail JSON")
	}
	if err := decodeJSONField(out.fields, "occurrences", &out.occurrences); err != nil ||
		out.occurrences < 1 {
		return out, errors.New("store: IPv6 RA condition has an invalid occurrence count")
	}
	first, err := canonicalUint32JSONField(out.fields, "first_source_id")
	if err != nil {
		return out, err
	}
	last, err := canonicalUint32JSONField(out.fields, "last_source_id")
	if err != nil {
		return out, err
	}
	out.firstSourceID, out.lastSourceID = first, last
	for _, key := range []string{"source_time_ms", "first_source_time_ms", "last_source_time_ms"} {
		var value int64
		if err := decodeJSONField(out.fields, key, &value); err != nil || value <= 0 {
			return out, fmt.Errorf("store: IPv6 RA condition has an invalid %s", key)
		}
	}
	var priority uint32
	var message, condition, addressFamily string
	var lifetime int64
	if err := decodeJSONField(out.fields, "priority", &priority); err != nil ||
		decodeJSONField(out.fields, "message", &message) != nil ||
		!IsOpenWRTIPv6RANoDefaultRouteLog(priority, message) ||
		decodeJSONField(out.fields, "condition", &condition) != nil ||
		condition != "ipv6_ra_no_default_route" ||
		decodeJSONField(out.fields, "address_family", &addressFamily) != nil ||
		addressFamily != "ipv6" ||
		decodeJSONField(out.fields, "router_advertisement_lifetime", &lifetime) != nil || lifetime != 0 {
		return out, errors.New("store: IPv6 RA condition has invalid source evidence")
	}
	return out, nil
}

func decodeJSONField(fields map[string]json.RawMessage, key string, out any) error {
	raw, ok := fields[key]
	if !ok || json.Unmarshal(raw, out) != nil {
		return fmt.Errorf("invalid %s", key)
	}
	return nil
}

func canonicalUint32JSONField(fields map[string]json.RawMessage, key string) (uint32, error) {
	var text string
	if err := decodeJSONField(fields, key, &text); err != nil {
		return 0, fmt.Errorf("store: IPv6 RA condition has an invalid %s", key)
	}
	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil || strconv.FormatUint(value, 10) != text {
		return 0, fmt.Errorf("store: IPv6 RA condition has an invalid %s", key)
	}
	return uint32(value), nil
}

func setJSONField(fields map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fields[key] = raw
	return nil
}

func mergeIPv6RACondition(ctx context.Context, tx *sql.Tx, existing storedIPv6RACondition,
	incoming Event, incomingDetail string, increment int64) (bool, error) {
	if increment <= 0 {
		return false, errors.New("store: IPv6 RA condition increment must be positive")
	}
	next, err := decodeIPv6RAConditionDetail([]byte(incomingDetail))
	if err != nil {
		return false, err
	}
	if next.occurrences != increment {
		return false, errors.New("store: IPv6 RA condition increment does not match its evidence")
	}
	delta := next.lastSourceID - existing.detail.lastSourceID
	if delta == 0 || delta >= 1<<31 {
		return false, nil
	}
	if existing.detail.occurrences > math.MaxInt64-increment {
		return false, errors.New("store: IPv6 RA condition occurrence count overflow")
	}
	fields := make(map[string]json.RawMessage, len(existing.detail.fields))
	for key, value := range existing.detail.fields {
		fields[key] = value
	}
	for _, key := range []string{"last_source_time_ms", "last_source_id", "source_time_ms", "message"} {
		fields[key] = next.fields[key]
	}
	if err := setJSONField(fields, "occurrences", existing.detail.occurrences+increment); err != nil {
		return false, err
	}
	detail, err := json.Marshal(fields)
	if err != nil {
		return false, fmt.Errorf("store: encode updated IPv6 RA condition: %w", err)
	}
	if len(detail) > maxEncodedEventBytes || existing.fixedTextBytes > maxEncodedEventBytes-len(detail) {
		return false, errors.New("store: IPv6 RA condition exceeds the encoded storage limit")
	}
	res, err := tx.ExecContext(ctx, `UPDATE events
SET ts=MAX(ts,?), ingested_at=MAX(COALESCE(ingested_at,0),?), detail_json=? WHERE id=?`,
		incoming.TS, incoming.IngestedAt, string(detail), existing.id)
	if err != nil {
		return false, fmt.Errorf("store: update IPv6 RA condition: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: inspect IPv6 RA condition update: %w", err)
	}
	if n != 1 {
		return false, errors.New("store: IPv6 RA condition changed during update")
	}
	return true, nil
}

// coalesceOpenWRTIPv6RAEvent handles both a forward event and a replay of one
// already represented by the stable condition row. Malformed durable state is
// an error, so the caller cannot advance its cursor past missing evidence.
func coalesceOpenWRTIPv6RAEvent(ctx context.Context, tx *sql.Tx, event Event,
	detail string) (bool, error) {
	existing, found, err := loadIPv6RACondition(ctx, tx, *event.DeviceID, event.SourceBoot)
	if err != nil || !found {
		return false, err
	}
	_, err = mergeIPv6RACondition(ctx, tx, existing, event, detail, 1)
	return true, err
}

type legacyIPv6RACompactionLimits struct {
	events int
	groups int
	bytes  int64
}

var defaultLegacyIPv6RACompactionLimits = legacyIPv6RACompactionLimits{
	events: 100_000,
	groups: 4096,
	bytes:  16 << 20,
}

type legacyIPv6RAKey struct {
	deviceID   int64
	sourceBoot string
}

type legacyIPv6RAEntry struct {
	rowID        int64
	sourceID     uint32
	sourceIDText string
	sourceTimeMS int64
	message      string
	ts           int64
	ingestedAt   int64
}

type legacyIPv6RAGroup struct {
	key     legacyIPv6RAKey
	entries []legacyIPv6RAEntry
}

// CompactOpenWRTIPv6RANoDefaultRouteEvents converts exact legacy raw rows into
// the same bounded condition record used by current ingest. It runs once per
// daemon startup, in one transaction, before any API can observe the database.
func (db *DB) CompactOpenWRTIPv6RANoDefaultRouteEvents(ctx context.Context) (int64, error) {
	return db.compactOpenWRTIPv6RANoDefaultRouteEvents(ctx,
		defaultLegacyIPv6RACompactionLimits)
}

func (db *DB) compactOpenWRTIPv6RANoDefaultRouteEvents(ctx context.Context,
	limits legacyIPv6RACompactionLimits) (int64, error) {
	if limits.events <= 0 || limits.groups <= 0 || limits.bytes <= 0 {
		return 0, errors.New("store: IPv6 RA compaction limits must be positive")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin IPv6 RA event compaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, legacyIPv6RAEventsSQL)
	if err != nil {
		return 0, fmt.Errorf("store: query legacy IPv6 RA events: %w", err)
	}
	groups := map[legacyIPv6RAKey]*legacyIPv6RAGroup{}
	ids := make([]int64, 0)
	var inputBytes int64
	for rows.Next() {
		var entry legacyIPv6RAEntry
		var deviceID int64
		var sourceBoot string
		if err := rows.Scan(&entry.rowID, &deviceID, &sourceBoot, &entry.sourceIDText,
			&entry.ts, &entry.ingestedAt, &entry.message, &entry.sourceTimeMS); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan legacy IPv6 RA event: %w", err)
		}
		value, err := strconv.ParseUint(entry.sourceIDText, 10, 32)
		if err != nil || strconv.FormatUint(value, 10) != entry.sourceIDText ||
			!IsOpenWRTIPv6RANoDefaultRouteLog(28, entry.message) {
			continue
		}
		entry.sourceID = uint32(value)
		if len(ids) == limits.events {
			rows.Close()
			return 0, fmt.Errorf("store: IPv6 RA compaction exceeds %d events", limits.events)
		}
		rowBytes := int64(len(sourceBoot) + len(entry.sourceIDText) + len(entry.message))
		if rowBytes > limits.bytes-inputBytes {
			rows.Close()
			return 0, fmt.Errorf("store: IPv6 RA compaction exceeds %d input bytes", limits.bytes)
		}
		inputBytes += rowBytes
		key := legacyIPv6RAKey{deviceID: deviceID, sourceBoot: sourceBoot}
		group := groups[key]
		if group == nil {
			if len(groups) == limits.groups {
				rows.Close()
				return 0, fmt.Errorf("store: IPv6 RA compaction exceeds %d producer groups", limits.groups)
			}
			group = &legacyIPv6RAGroup{key: key}
			groups[key] = group
		}
		group.entries = append(group.entries, entry)
		ids = append(ids, entry.rowID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("store: read legacy IPv6 RA events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: close legacy IPv6 RA events: %w", err)
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("store: commit IPv6 RA event compaction: %w", err)
		}
		return 0, nil
	}

	for _, group := range groups {
		if err := compactLegacyIPv6RAGroup(ctx, tx, group); err != nil {
			return 0, err
		}
	}
	if err := deleteLegacyIPv6RAEvents(ctx, tx, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit IPv6 RA event compaction: %w", err)
	}
	return int64(len(ids)), nil
}

const legacyIPv6RAEventsSQL = `
SELECT id, device_id, source_boot, source_id, ts,
       COALESCE(ingested_at, ts * 1000),
       substr(json_extract(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                           '$.message'), 1, 4097),
       CAST(json_extract(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                             '$.source_time_ms') AS INTEGER)
  FROM events
 WHERE source='openwrt-logd' AND event='openwrt.log' AND severity='warning'
   AND category='system' AND device_id IS NOT NULL
   AND source_boot IS NOT NULL AND length(source_boot) BETWEEN 1 AND 256
   AND source_id IS NOT NULL AND length(source_id) BETWEEN 1 AND 10
   AND json_type(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                 '$.priority')='integer'
   AND json_extract(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                    '$.priority')=28
   AND json_type(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                 '$.message')='text'
   AND length(json_extract(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                           '$.message')) <= 4096
   AND json_type(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                 '$.source_time_ms')='integer'
   AND json_extract(CASE WHEN json_valid(detail_json) THEN detail_json ELSE '{}' END,
                    '$.source_time_ms') > 0
 ORDER BY id`

func compactLegacyIPv6RAGroup(ctx context.Context, tx *sql.Tx,
	group *legacyIPv6RAGroup) error {
	existing, found, err := loadIPv6RACondition(ctx, tx, group.key.deviceID,
		group.key.sourceBoot)
	if err != nil {
		return err
	}
	hasLast := found
	lastID := existing.detail.lastSourceID
	var first, last legacyIPv6RAEntry
	var accepted int64
	var maxTS, maxIngestedAt int64
	for _, entry := range group.entries {
		if hasLast {
			delta := entry.sourceID - lastID
			if delta == 0 || delta >= 1<<31 {
				continue
			}
		}
		if accepted == 0 {
			first, maxTS, maxIngestedAt = entry, entry.ts, entry.ingestedAt
		}
		last = entry
		accepted++
		maxTS = max(maxTS, entry.ts)
		maxIngestedAt = max(maxIngestedAt, entry.ingestedAt)
		lastID, hasLast = entry.sourceID, true
	}
	if accepted == 0 {
		return nil
	}
	deviceID := group.key.deviceID
	event := Event{
		TS: maxTS, DeviceID: &deviceID, Category: "system", Severity: "warning",
		Event: EventOpenWRTIPv6RANoDefaultRoute, Source: "openwrt-logd",
		SourceID: EventOpenWRTIPv6RANoDefaultRouteSourceID, SourceBoot: group.key.sourceBoot,
		IngestedAt: maxIngestedAt,
		Detail: map[string]any{
			"message": last.message, "facility": uint32(3), "priority": uint32(28),
			"source_time_ms": last.sourceTimeMS,
			"condition":      "ipv6_ra_no_default_route", "occurrences": accepted,
			"first_source_time_ms": first.sourceTimeMS,
			"last_source_time_ms":  last.sourceTimeMS,
			"first_source_id":      first.sourceIDText, "last_source_id": last.sourceIDText,
			"address_family": "ipv6", "router_advertisement_lifetime": 0,
		},
	}
	event, detail, err := normalizeEvent(event)
	if err != nil {
		return fmt.Errorf("store: encode compacted IPv6 RA event: %w", err)
	}
	if _, err := decodeIPv6RAConditionDetail([]byte(detail)); err != nil {
		return fmt.Errorf("store: validate compacted IPv6 RA event: %w", err)
	}
	if found {
		updated, err := mergeIPv6RACondition(ctx, tx, existing, event, detail, accepted)
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("store: compacted IPv6 RA evidence was not forward")
		}
		return nil
	}
	res, err := tx.ExecContext(ctx, appendEventSQL, eventInsertArgs(event, detail)...)
	if err != nil {
		return fmt.Errorf("store: insert compacted IPv6 RA event: %w", err)
	}
	inserted, err := eventInsertResult(res)
	if err != nil {
		return err
	}
	if !inserted {
		return errors.New("store: compacted IPv6 RA event identity already exists")
	}
	return nil
}

func deleteLegacyIPv6RAEvents(ctx context.Context, tx *sql.Tx, ids []int64) error {
	const batchSize = 256
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		query := `DELETE FROM events WHERE source='openwrt-logd' AND event='openwrt.log' AND id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("store: delete legacy IPv6 RA events: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: inspect legacy IPv6 RA deletion: %w", err)
		}
		if n != int64(len(args)) {
			return errors.New("store: legacy IPv6 RA event set changed during compaction")
		}
	}
	return nil
}
