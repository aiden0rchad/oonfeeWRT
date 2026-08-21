package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type schemaInspector interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type schemaColumn struct {
	name       string
	typeName   string
	notNull    int
	primaryKey int
	defaultSQL *string
}

func schemaDefault(value string) *string { return &value }

// verifySchemaV16 rejects a version marker attached to anything other than
// the schema this binary understands. CREATE TABLE/INDEX IF NOT EXISTS is not
// validation: a colliding partial table would otherwise be silently accepted
// and fail later, after schema_version had made rollback to the v15 binary
// impossible.
func verifySchemaV16(ctx context.Context, q schemaInspector) error {
	tables := map[string][]schemaColumn{
		"events": {
			{name: "id", typeName: "INTEGER", primaryKey: 1},
			{name: "ts", typeName: "INTEGER", notNull: 1},
			{name: "device_id", typeName: "INTEGER"},
			{name: "category", typeName: "TEXT", notNull: 1},
			{name: "severity", typeName: "TEXT", notNull: 1},
			{name: "event", typeName: "TEXT", notNull: 1},
			{name: "detail_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'{}'")},
			{name: "source", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'controller'")},
			{name: "source_id", typeName: "TEXT"},
			{name: "source_boot", typeName: "TEXT"},
			{name: "ingested_at", typeName: "INTEGER"},
			{name: "client_mac", typeName: "TEXT"},
			{name: "action", typeName: "TEXT"},
			{name: "direction", typeName: "TEXT"},
			{name: "in_iface", typeName: "TEXT"},
			{name: "out_iface", typeName: "TEXT"},
			{name: "src_ip", typeName: "TEXT"},
			{name: "dst_ip", typeName: "TEXT"},
			{name: "src_port", typeName: "INTEGER"},
			{name: "dst_port", typeName: "INTEGER"},
			{name: "zone_in", typeName: "TEXT"},
			{name: "zone_out", typeName: "TEXT"},
			{name: "policy_id", typeName: "INTEGER"},
		},
		"ingest_cursors": {
			{name: "device_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
			{name: "source", typeName: "TEXT", notNull: 1, primaryKey: 2},
			{name: "boot_id", typeName: "TEXT", notNull: 1},
			{name: "cursor", typeName: "TEXT", notNull: 1},
			{name: "updated_at", typeName: "INTEGER", notNull: 1},
			{name: "continuity_gap_at", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("0")},
		},
		"topology_edges": {
			{name: "id", typeName: "INTEGER", primaryKey: 1},
			{name: "child_node", typeName: "TEXT", notNull: 1},
			{name: "child_mac", typeName: "TEXT"},
			{name: "parent_node", typeName: "TEXT", notNull: 1},
			{name: "parent_device_id", typeName: "INTEGER"},
			{name: "parent_port", typeName: "TEXT"},
			{name: "medium", typeName: "TEXT", notNull: 1},
			{name: "confidence", typeName: "TEXT", notNull: 1},
			{name: "valid_from", typeName: "INTEGER", notNull: 1},
			{name: "valid_to", typeName: "INTEGER"},
			{name: "last_seen", typeName: "INTEGER", notNull: 1},
			{name: "evidence_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'[]'")},
			{name: "ambiguity_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'[]'")},
		},
		"topology_source_states": {
			{name: "device_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
			{name: "source", typeName: "TEXT", notNull: 1, primaryKey: 2},
			{name: "state", typeName: "TEXT", notNull: 1},
			{name: "reason", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("''")},
			{name: "observed_at", typeName: "INTEGER", notNull: 1},
		},
		"radio_scans": {
			{name: "id", typeName: "INTEGER", primaryKey: 1},
			{name: "device_id", typeName: "INTEGER", notNull: 1},
			{name: "radio_key", typeName: "TEXT", notNull: 1},
			{name: "started_at", typeName: "INTEGER", notNull: 1},
			{name: "finished_at", typeName: "INTEGER"},
			{name: "status", typeName: "TEXT", notNull: 1},
			{name: "detail_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'{}'")},
		},
		"radio_scan_bss": {
			{name: "scan_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
			{name: "bssid", typeName: "TEXT", notNull: 1, primaryKey: 2},
			{name: "ssid", typeName: "TEXT", notNull: 1},
			{name: "mhz", typeName: "INTEGER", notNull: 1, primaryKey: 3},
			{name: "channel", typeName: "INTEGER", notNull: 1},
			{name: "signal", typeName: "INTEGER"},
			{name: "width", typeName: "INTEGER"},
		},
	}
	for table, want := range tables {
		if err := verifyTableColumns(ctx, q, table, want); err != nil {
			return fmt.Errorf("store: schema v16 attestation: %w", err)
		}
	}

	indexes := []struct {
		table, name string
		columns     []string
		unique      int
		partial     int
		predicate   string
	}{
		{"events", "events_ts", []string{"ts"}, 0, 0, ""},
		{"events", "events_source_identity", []string{"device_id", "source", "source_boot", "source_id"}, 1, 1,
			"source_id is not null and trim(source_id) <> ''"},
		{"events", "events_client_time", []string{"client_mac", "ts", "id"}, 0, 0, ""},
		{"events", "events_category_time", []string{"category", "ts", "id"}, 0, 0, ""},
		{"events", "events_severity_time", []string{"severity", "ts", "id"}, 0, 0, ""},
		{"topology_edges", "topology_edges_active", []string{"child_node", "valid_to", "last_seen"}, 0, 0, ""},
		{"topology_edges", "topology_edges_replay", []string{"valid_from", "valid_to"}, 0, 0, ""},
		{"radio_scans", "radio_scans_radio_time", []string{"device_id", "radio_key", "started_at", "id"}, 0, 0, ""},
	}
	for _, index := range indexes {
		if err := verifyIndex(ctx, q, index.table, index.name, index.columns,
			index.unique, index.partial, index.predicate); err != nil {
			return fmt.Errorf("store: schema v16 attestation: %w", err)
		}
	}

	foreignKeys := map[string][]schemaForeignKey{
		"ingest_cursors":         {{from: "device_id", table: "devices", to: "id", onDelete: "CASCADE"}},
		"topology_edges":         {{from: "parent_device_id", table: "devices", to: "id", onDelete: "SET NULL"}},
		"topology_source_states": {{from: "device_id", table: "devices", to: "id", onDelete: "CASCADE"}},
		"radio_scans":            {{from: "device_id", table: "devices", to: "id", onDelete: "CASCADE"}},
		"radio_scan_bss":         {{from: "scan_id", table: "radio_scans", to: "id", onDelete: "CASCADE"}},
	}
	for table, want := range foreignKeys {
		if err := verifyForeignKeys(ctx, q, table, want); err != nil {
			return fmt.Errorf("store: schema v16 attestation: %w", err)
		}
	}

	tableShapes := map[string]struct {
		checks    []string
		fragments []string
	}{
		"events":                 {},
		"ingest_cursors":         {fragments: []string{"without rowid"}},
		"topology_edges":         {checks: []string{"valid_to is null or valid_to >= valid_from", "last_seen >= valid_from", "valid_to is null or last_seen <= valid_to"}},
		"topology_source_states": {checks: []string{"state in ('unknown','empty','observed','error')"}, fragments: []string{"without rowid"}},
		"radio_scans":            {checks: []string{"status in ('pending','running','completed','failed')", "finished_at is null or finished_at >= started_at"}},
		"radio_scan_bss":         {fragments: []string{"without rowid"}},
	}
	for table, want := range tableShapes {
		var createSQL string
		if err := q.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).
			Scan(&createSQL); err != nil {
			return fmt.Errorf("store: schema v16 attestation: read %s definition: %w", table, err)
		}
		normalized := normalizeSchemaSQL(createSQL)
		checks, err := schemaCheckExpressions(normalized)
		if err != nil {
			return fmt.Errorf("store: schema v16 attestation: table %s: %w", table, err)
		}
		if !equalSchemaStrings(checks, want.checks) {
			return fmt.Errorf("store: schema v16 attestation: table %s has CHECK constraints %q, want %q",
				table, checks, want.checks)
		}
		for _, fragment := range want.fragments {
			if !strings.Contains(normalized, fragment) {
				return fmt.Errorf("store: schema v16 attestation: table %s is missing %q", table, fragment)
			}
		}
	}
	return nil
}

func verifyTableColumns(ctx context.Context, q schemaInspector, table string, want []schemaColumn) error {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	defer rows.Close()
	got := []schemaColumn{}
	for rows.Next() {
		var cid int
		var column schemaColumn
		var defaultSQL sql.NullString
		if err := rows.Scan(&cid, &column.name, &column.typeName, &column.notNull,
			&defaultSQL, &column.primaryKey); err != nil {
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		column.typeName = strings.ToUpper(strings.TrimSpace(column.typeName))
		if defaultSQL.Valid {
			column.defaultSQL = schemaDefault(defaultSQL.String)
		}
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("table %s has %d columns, want %d", table, len(got), len(want))
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].typeName != want[i].typeName ||
			got[i].notNull != want[i].notNull || got[i].primaryKey != want[i].primaryKey ||
			!equalSchemaDefault(got[i].defaultSQL, want[i].defaultSQL) {
			return fmt.Errorf("table %s column %d is %+v, want %+v", table, i, got[i], want[i])
		}
	}
	return nil
}

func equalSchemaDefault(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func verifyIndex(ctx context.Context, q schemaInspector, table, name string,
	wantColumns []string, wantUnique, wantPartial int, wantPredicate string) error {
	rows, err := q.QueryContext(ctx, `PRAGMA index_list(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect indexes on %s: %w", table, err)
	}
	found := false
	for rows.Next() {
		var seq, unique, partial int
		var indexName, origin string
		if err := rows.Scan(&seq, &indexName, &unique, &origin, &partial); err != nil {
			rows.Close()
			return err
		}
		if indexName == name {
			found = unique == wantUnique && partial == wantPartial
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("index %s is missing or has the wrong unique/partial shape", name)
	}
	rows, err = q.QueryContext(ctx, `PRAGMA index_info(`+name+`)`)
	if err != nil {
		return err
	}
	columns := []string{}
	for rows.Next() {
		var seq, cid int
		var column string
		if err := rows.Scan(&seq, &cid, &column); err != nil {
			rows.Close()
			return err
		}
		columns = append(columns, column)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if strings.Join(columns, "\x00") != strings.Join(wantColumns, "\x00") {
		return fmt.Errorf("index %s columns are %v, want %v", name, columns, wantColumns)
	}
	if wantPartial != 0 {
		var createSQL string
		if err := q.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND name=?`,
			table, name).Scan(&createSQL); err != nil {
			return fmt.Errorf("read index %s definition: %w", name, err)
		}
		normalized := strings.TrimSuffix(normalizeSchemaSQL(createSQL), ";")
		const where = " where "
		at := strings.LastIndex(normalized, where)
		if at < 0 || strings.TrimSpace(normalized[at+len(where):]) != normalizeSchemaSQL(wantPredicate) {
			return fmt.Errorf("index %s has the wrong predicate", name)
		}
	}
	return nil
}

type schemaForeignKey struct {
	from, table, to, onDelete string
}

func verifyForeignKeys(ctx context.Context, q schemaInspector, table string,
	want []schemaForeignKey) error {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	got := []schemaForeignKey{}
	for rows.Next() {
		var id, seq int
		var fk schemaForeignKey
		var onUpdate, match string
		if err := rows.Scan(&id, &seq, &fk.table, &fk.from, &fk.to,
			&onUpdate, &fk.onDelete, &match); err != nil {
			return err
		}
		got = append(got, fk)
	}
	if len(got) != len(want) {
		return fmt.Errorf("table %s foreign keys are %+v, want %+v", table, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("table %s foreign keys are %+v, want %+v", table, got, want)
		}
	}
	return rows.Err()
}

func normalizeSchemaSQL(raw string) string {
	return strings.Join(strings.Fields(strings.ToLower(stripSchemaComments(raw))), " ")
}

func stripSchemaComments(raw string) string {
	var clean strings.Builder
	clean.Grow(len(raw))
	quote := byte(0)
	for i := 0; i < len(raw); i++ {
		character := raw[i]
		if quote != 0 {
			clean.WriteByte(character)
			if character == quote {
				if i+1 < len(raw) && raw[i+1] == quote {
					i++
					clean.WriteByte(raw[i])
					continue
				}
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' || character == '`' {
			quote = character
			clean.WriteByte(character)
			continue
		}
		if character == '-' && i+1 < len(raw) && raw[i+1] == '-' {
			for i += 2; i < len(raw) && raw[i] != '\n'; i++ {
			}
			clean.WriteByte(' ')
			continue
		}
		if character == '/' && i+1 < len(raw) && raw[i+1] == '*' {
			i += 2
			for i+1 < len(raw) && (raw[i] != '*' || raw[i+1] != '/') {
				i++
			}
			if i+1 < len(raw) {
				i++
			}
			clean.WriteByte(' ')
			continue
		}
		clean.WriteByte(character)
	}
	return clean.String()
}

func schemaCheckExpressions(normalized string) ([]string, error) {
	var checks []string
	for offset := 0; offset < len(normalized); {
		relative := strings.Index(normalized[offset:], "check")
		if relative < 0 {
			break
		}
		start := offset + relative
		end := start + len("check")
		if (start > 0 && schemaIdentifierByte(normalized[start-1])) ||
			(end < len(normalized) && schemaIdentifierByte(normalized[end])) {
			offset = end
			continue
		}
		for end < len(normalized) && normalized[end] == ' ' {
			end++
		}
		if end >= len(normalized) || normalized[end] != '(' {
			offset = end
			continue
		}
		expressionStart := end + 1
		depth := 1
		quote := byte(0)
		for end++; end < len(normalized); end++ {
			character := normalized[end]
			if quote != 0 {
				if character == quote {
					if end+1 < len(normalized) && normalized[end+1] == quote {
						end++
						continue
					}
					quote = 0
				}
				continue
			}
			switch character {
			case '\'', '"', '`':
				quote = character
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					checks = append(checks, strings.TrimSpace(normalized[expressionStart:end]))
					offset = end + 1
					break
				}
			}
			if depth == 0 {
				break
			}
		}
		if depth != 0 {
			return nil, fmt.Errorf("contains an unterminated CHECK constraint")
		}
	}
	return checks, nil
}

func schemaIdentifierByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func equalSchemaStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
