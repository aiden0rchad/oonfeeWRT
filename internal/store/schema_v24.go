package store

import (
	"context"
	"fmt"
)

func verifySchemaV24(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "controller_alert_state", []schemaColumn{
		{name: "id", typeName: "INTEGER", primaryKey: 1},
		{name: "state_json", typeName: "BLOB", notNull: 1},
	}); err != nil {
		return fmt.Errorf("store: schema v24 attestation: %w", err)
	}
	var definition string
	if err := q.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='controller_alert_state'`).Scan(&definition); err != nil {
		return err
	}
	checks, err := schemaCheckExpressions(normalizeSchemaSQL(definition))
	if err != nil || !equalSchemaStrings(checks, []string{"id=1", "length(state_json)<=2097152"}) {
		return fmt.Errorf("store: schema v24 attestation: alert state bounds are missing")
	}
	return nil
}
