package store

import (
	"context"
	"fmt"
	"strings"
)

func verifySchemaV22(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "policy_sets", []schemaColumn{
		{name: "id", typeName: "INTEGER", primaryKey: 1},
		{name: "name", typeName: "TEXT", notNull: 1},
	}); err != nil {
		return fmt.Errorf("store: schema v22 attestation: %w", err)
	}
	if err := verifyTableColumns(ctx, q, "policy_set_members", []schemaColumn{
		{name: "set_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
		{name: "mac", typeName: "TEXT", notNull: 1, primaryKey: 2},
	}); err != nil {
		return fmt.Errorf("store: schema v22 attestation: %w", err)
	}
	if err := verifyIndex(ctx, q, "policy_sets", "policy_sets_name_nocase",
		[]string{"name"}, 1, 0, ""); err != nil {
		return fmt.Errorf("store: schema v22 attestation: %w", err)
	}
	if err := verifyForeignKeys(ctx, q, "policy_set_members", []schemaForeignKey{{
		from: "set_id", table: "policy_sets", to: "id", onDelete: "CASCADE",
	}}); err != nil {
		return fmt.Errorf("store: schema v22 attestation: %w", err)
	}
	var tableSQL, indexSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='policy_set_members'`).
		Scan(&tableSQL); err != nil {
		return fmt.Errorf("store: schema v22 attestation: read policy_set_members definition: %w", err)
	}
	if !strings.Contains(normalizeSchemaSQL(tableSQL), "without rowid") {
		return fmt.Errorf("store: schema v22 attestation: policy_set_members is not WITHOUT ROWID")
	}
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='policy_sets_name_nocase'`).
		Scan(&indexSQL); err != nil {
		return fmt.Errorf("store: schema v22 attestation: read policy_sets_name_nocase definition: %w", err)
	}
	if !strings.Contains(normalizeSchemaSQL(indexSQL), "name collate nocase") {
		return fmt.Errorf("store: schema v22 attestation: policy set names are not ASCII case-insensitive")
	}
	return nil
}
