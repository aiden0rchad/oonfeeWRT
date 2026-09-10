package store

import (
	"context"
	"fmt"
)

func verifySchemaV21(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "devices", []schemaColumn{
		{name: "id", typeName: "INTEGER", primaryKey: 1},
		{name: "mac", typeName: "TEXT", notNull: 1},
		{name: "host", typeName: "TEXT", notNull: 1},
		{name: "port", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("80")},
		{name: "scheme", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'http'")},
		{name: "cert_fp", typeName: "TEXT"},
		{name: "host_key_fp", typeName: "TEXT"},
		{name: "name", typeName: "TEXT", notNull: 1},
		{name: "role", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'ap'")},
		{name: "functions_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'[\"ap\",\"switch\"]'")},
		{name: "adopted_at", typeName: "INTEGER"},
		{name: "cred_enc", typeName: "BLOB"},
		{name: "class", typeName: "TEXT"},
		{name: "caps_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'{}'")},
		{name: "fw_release", typeName: "TEXT"},
		{name: "last_seen", typeName: "INTEGER"},
		{name: "poll_state", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'baseline'")},
		{name: "poll_interval_s", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("0")},
		{name: "management_mode", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'managed'")},
	}); err != nil {
		return fmt.Errorf("store: schema v21 attestation: %w", err)
	}
	var tableSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='devices'`).
		Scan(&tableSQL); err != nil {
		return fmt.Errorf("store: schema v21 attestation: read devices definition: %w", err)
	}
	checks, err := schemaCheckExpressions(normalizeSchemaSQL(tableSQL))
	if err != nil {
		return fmt.Errorf("store: schema v21 attestation: table devices: %w", err)
	}
	wantChecks := []string{"management_mode in ('managed','monitor_only')"}
	if !equalSchemaStrings(checks, wantChecks) {
		return fmt.Errorf("store: schema v21 attestation: table devices has CHECK constraints %q, want %q",
			checks, wantChecks)
	}
	return nil
}
