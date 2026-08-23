package store

import (
	"context"
	"fmt"
)

func verifySchemaV18(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "speed_tests", []schemaColumn{
		{name: "id", typeName: "TEXT", primaryKey: 1},
		{name: "state", typeName: "TEXT", notNull: 1},
		{name: "phase", typeName: "TEXT", notNull: 1},
		{name: "progress_percent", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("0")},
		{name: "provider", typeName: "TEXT", notNull: 1},
		{name: "method", typeName: "TEXT", notNull: 1},
		{name: "provenance", typeName: "TEXT", notNull: 1},
		{name: "endpoint", typeName: "TEXT", notNull: 1},
		{name: "estimated_bytes", typeName: "INTEGER", notNull: 1},
		{name: "actor_admin_id", typeName: "INTEGER", notNull: 1},
		{name: "actor_username", typeName: "TEXT", notNull: 1},
		{name: "created_at", typeName: "INTEGER", notNull: 1},
		{name: "started_at", typeName: "INTEGER"},
		{name: "finished_at", typeName: "INTEGER"},
		{name: "plan_id", typeName: "TEXT", notNull: 1},
		{name: "download_mbps", typeName: "REAL"},
		{name: "upload_mbps", typeName: "REAL"},
		{name: "idle_latency_ms", typeName: "REAL"},
		{name: "idle_jitter_ms", typeName: "REAL"},
		{name: "loaded_latency_ms", typeName: "REAL"},
		{name: "loaded_jitter_ms", typeName: "REAL"},
		{name: "bytes_downloaded", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("0")},
		{name: "bytes_uploaded", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("0")},
		{name: "error", typeName: "TEXT"},
	}); err != nil {
		return fmt.Errorf("store: schema v18 attestation: %w", err)
	}
	if err := verifyIndex(ctx, q, "speed_tests", "speed_tests_history",
		[]string{"created_at", "id"}, 0, 0, ""); err != nil {
		return fmt.Errorf("store: schema v18 attestation: %w", err)
	}
	if err := verifyIndex(ctx, q, "speed_tests", "speed_tests_one_active",
		[]string{"provenance"}, 1, 1,
		"state IN ('queued','running','cancelling')"); err != nil {
		return fmt.Errorf("store: schema v18 attestation: %w", err)
	}
	var createSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='speed_tests'`).
		Scan(&createSQL); err != nil {
		return fmt.Errorf("store: schema v18 attestation: read speed_tests definition: %w", err)
	}
	checks, err := schemaCheckExpressions(normalizeSchemaSQL(createSQL))
	if err != nil {
		return fmt.Errorf("store: schema v18 attestation: table speed_tests: %w", err)
	}
	wantChecks := []string{
		"state in ('queued','running','cancelling','completed','failed')",
		"progress_percent between 0 and 100",
		"provenance = 'controller-host'",
		"estimated_bytes > 0",
		"length(plan_id) > 0",
		"started_at is null or started_at >= created_at",
		"finished_at is null or finished_at >= created_at",
	}
	if !equalSchemaStrings(checks, wantChecks) {
		return fmt.Errorf("store: schema v18 attestation: table speed_tests has CHECK constraints %q, want %q",
			checks, wantChecks)
	}
	return nil
}
