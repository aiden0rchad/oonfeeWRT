package store

import (
	"context"
	"fmt"
)

func verifySchemaV25(ctx context.Context, q schemaInspector) error {
	if err := verifyTableColumns(ctx, q, "controller_adguard_config", []schemaColumn{
		{name: "id", typeName: "INTEGER", primaryKey: 1},
		{name: "config_enc", typeName: "BLOB", notNull: 1},
	}); err != nil {
		return fmt.Errorf("store: schema v25 attestation: %w", err)
	}
	var definition string
	if err := q.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='controller_adguard_config'`).Scan(&definition); err != nil {
		return err
	}
	checks, err := schemaCheckExpressions(normalizeSchemaSQL(definition))
	if err != nil || !equalSchemaStrings(checks, []string{"id=1", "length(config_enc)>0 and length(config_enc)<=16384"}) {
		return fmt.Errorf("store: schema v25 attestation: integration ciphertext bounds are missing")
	}
	return nil
}
