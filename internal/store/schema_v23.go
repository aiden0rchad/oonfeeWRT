package store

import (
	"context"
	"fmt"
	"strings"
)

func verifySchemaV23(ctx context.Context, q schemaInspector) error {
	if err := verifyIndex(ctx, q, "devices", "devices_one_managed_gateway",
		[]string{"management_mode"}, 1, 1,
		"adopted_at is not null and management_mode='managed' and (role='gateway' or instr(functions_json,'\"gateway\"')>0)"); err != nil {
		return fmt.Errorf("store: schema v23 attestation: %w", err)
	}
	if err := verifyIndex(ctx, q, "clients", "clients_mac_nocase",
		[]string{"mac"}, 0, 0, ""); err != nil {
		return fmt.Errorf("store: schema v23 attestation: %w", err)
	}
	var clientIndexSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='clients_mac_nocase'`).
		Scan(&clientIndexSQL); err != nil {
		return fmt.Errorf("store: schema v23 attestation: read clients_mac_nocase definition: %w", err)
	}
	if !strings.Contains(normalizeSchemaSQL(clientIndexSQL), "mac collate nocase") {
		return fmt.Errorf("store: schema v23 attestation: client MAC lookup is not ASCII case-insensitive")
	}
	if err := verifyIndex(ctx, q, "policy_set_members", "policy_set_members_mac_nocase",
		[]string{"mac"}, 0, 0, ""); err != nil {
		return fmt.Errorf("store: schema v23 attestation: %w", err)
	}
	var memberIndexSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='policy_set_members_mac_nocase'`).
		Scan(&memberIndexSQL); err != nil {
		return fmt.Errorf("store: schema v23 attestation: read policy_set_members_mac_nocase definition: %w", err)
	}
	if !strings.Contains(normalizeSchemaSQL(memberIndexSQL), "mac collate nocase") {
		return fmt.Errorf("store: schema v23 attestation: policy-set member lookup is not ASCII case-insensitive")
	}
	if err := verifyTableColumns(ctx, q, "client_observations", []schemaColumn{
		{name: "device_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
		{name: "mac", typeName: "TEXT", notNull: 1, primaryKey: 2},
		{name: "scope", typeName: "TEXT", notNull: 1},
		{name: "last_seen", typeName: "INTEGER", notNull: 1},
	}); err != nil {
		return fmt.Errorf("store: schema v23 attestation: %w", err)
	}
	if err := verifyForeignKeys(ctx, q, "client_observations", []schemaForeignKey{{
		from: "device_id", table: "devices", to: "id", onDelete: "CASCADE",
	}}); err != nil {
		return fmt.Errorf("store: schema v23 attestation: %w", err)
	}
	if err := verifyIndex(ctx, q, "client_observations", "client_observations_mac",
		[]string{"mac"}, 0, 0, ""); err != nil {
		return fmt.Errorf("store: schema v23 attestation: %w", err)
	}
	var tableSQL string
	if err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='client_observations'`).
		Scan(&tableSQL); err != nil {
		return fmt.Errorf("store: schema v23 attestation: read client_observations definition: %w", err)
	}
	normalized := normalizeSchemaSQL(tableSQL)
	checks, err := schemaCheckExpressions(normalized)
	if err != nil {
		return fmt.Errorf("store: schema v23 attestation: table client_observations: %w", err)
	}
	if !equalSchemaStrings(checks, []string{
		"mac=lower(mac)", "scope in ('local','upstream','unknown')",
	}) {
		return fmt.Errorf("store: schema v23 attestation: client_observations has CHECK constraints %q", checks)
	}
	if !strings.Contains(normalized, "without rowid") {
		return fmt.Errorf("store: schema v23 attestation: client_observations is not WITHOUT ROWID")
	}
	return nil
}
