package store

import (
	"context"
	"fmt"
)

func verifyCurrentSchema(ctx context.Context, q schemaInspector) error {
	if err := verifySchemaV16(ctx, q); err != nil {
		return err
	}
	if err := verifyTableColumns(ctx, q, "device_capability_installs", []schemaColumn{
		{name: "device_id", typeName: "INTEGER", notNull: 1, primaryKey: 1},
		{name: "capability", typeName: "TEXT", notNull: 1, primaryKey: 2},
		{name: "package_manager", typeName: "TEXT", notNull: 1},
		{name: "requested_packages_json", typeName: "TEXT", notNull: 1},
		{name: "baseline_packages_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'[]'")},
		{name: "added_packages_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'[]'")},
		{name: "services_json", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("'[]'")},
		{name: "state", typeName: "TEXT", notNull: 1},
		{name: "detail", typeName: "TEXT", notNull: 1, defaultSQL: schemaDefault("''")},
		{name: "installed_at", typeName: "INTEGER"},
		{name: "updated_at", typeName: "INTEGER", notNull: 1},
	}); err != nil {
		return fmt.Errorf("store: schema v17 attestation: %w", err)
	}
	if err := verifyForeignKeys(ctx, q, "device_capability_installs", []schemaForeignKey{{
		from: "device_id", table: "devices", to: "id", onDelete: "RESTRICT",
	}}); err != nil {
		return fmt.Errorf("store: schema v17 attestation: %w", err)
	}
	if err := verifySchemaV18(ctx, q); err != nil {
		return err
	}
	if err := verifySchemaV19(ctx, q); err != nil {
		return err
	}
	return verifySchemaV20(ctx, q)
}
