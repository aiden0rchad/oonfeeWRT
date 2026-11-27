package store

import (
	"context"
	"fmt"
	"strings"
)

// portRoleSchemaSQL is migration v26. It is not copied into schema.sql: a
// fresh database runs this statement after the base schema, and an upgraded
// database runs the same statement. Two copies drift.
const portRoleSchemaSQL = `CREATE TABLE IF NOT EXISTS port_roles (
  device_id INTEGER NOT NULL,
  port TEXT NOT NULL,
  mode TEXT NOT NULL,
  network_id INTEGER,
  PRIMARY KEY (device_id, port),
  FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE,
  FOREIGN KEY (network_id) REFERENCES networks(id) ON DELETE RESTRICT,
  CHECK (mode IN ('trunk', 'access')),
  CHECK ((mode = 'trunk' AND network_id IS NULL) OR (mode = 'access' AND network_id IS NOT NULL)),
  CHECK (length(port) BETWEEN 1 AND 32),
  CHECK (instr(port, ':') = 0 AND instr(port, ' ') = 0)
)`

func verifySchemaV26(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "port_roles", []schemaColumn{
		{name: "device_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
		{name: "port", typeName: "TEXT", notNull: 1, primaryKey: 2},
		{name: "mode", typeName: "TEXT", notNull: 1},
		{name: "network_id", typeName: "INTEGER"},
	}); err != nil {
		return fmt.Errorf("store: schema v26 attestation: %w", err)
	}
	// SQLite's foreign_key_list returns constraints in reverse definition order.
	if err := verifyForeignKeys(ctx, q, "port_roles", []schemaForeignKey{
		{from: "network_id", table: "networks", to: "id", onDelete: "RESTRICT"},
		{from: "device_id", table: "devices", to: "id", onDelete: "CASCADE"},
	}); err != nil {
		return fmt.Errorf("store: schema v26 attestation: %w", err)
	}
	var definition string
	if err := q.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='port_roles'`).Scan(&definition); err != nil {
		return err
	}
	normalized := normalizeSchemaSQL(definition)
	for _, want := range []string{
		"mode in ('trunk', 'access')",
		"mode = 'trunk' and network_id is null",
		"mode = 'access' and network_id is not null",
		"length(port) between 1 and 32",
		"instr(port, ':') = 0",
	} {
		if !strings.Contains(normalized, want) {
			return fmt.Errorf("store: schema v26 attestation: port role constraint %q is missing", want)
		}
	}
	return nil
}
