package store

import (
	"context"
	"fmt"
	"strings"
)

func verifySchemaV19(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "admins", []schemaColumn{
		{name: "id", typeName: "INTEGER", primaryKey: 1},
		{name: "username", typeName: "TEXT", notNull: 1},
		{name: "pass_hash", typeName: "TEXT", notNull: 1},
		{name: "created_at", typeName: "INTEGER", notNull: 1},
		{name: "last_login", typeName: "INTEGER"},
		{name: "role", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'owner'")},
		{name: "enabled", typeName: "INTEGER", notNull: 1, defaultSQL: schemaDefault("1")},
		{name: "deleted_at", typeName: "INTEGER"},
	}); err != nil {
		return fmt.Errorf("store: schema v19 attestation: %w", err)
	}
	if err := verifyIndex(ctx, q, "admins", "admins_username_nocase",
		[]string{"username"}, 1, 0, ""); err != nil {
		return fmt.Errorf("store: schema v19 attestation: %w", err)
	}

	var tableSQL, indexSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='admins'`).
		Scan(&tableSQL); err != nil {
		return fmt.Errorf("store: schema v19 attestation: read admins definition: %w", err)
	}
	checks, err := schemaCheckExpressions(normalizeSchemaSQL(tableSQL))
	if err != nil {
		return fmt.Errorf("store: schema v19 attestation: table admins: %w", err)
	}
	wantChecks := []string{
		"role in ('owner','admin','operator','viewer')",
		"enabled in (0,1)",
	}
	if !equalSchemaStrings(checks, wantChecks) {
		return fmt.Errorf("store: schema v19 attestation: table admins has CHECK constraints %q, want %q",
			checks, wantChecks)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name='admins' AND name='admins_username_nocase'`).
		Scan(&indexSQL); err != nil {
		return fmt.Errorf("store: schema v19 attestation: read admins_username_nocase definition: %w", err)
	}
	if !strings.Contains(normalizeSchemaSQL(indexSQL), "username collate nocase") {
		return fmt.Errorf("store: schema v19 attestation: admins_username_nocase is not ASCII case-insensitive")
	}
	return nil
}
