package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// verifySchemaV27 checks the column added onto networks. The rest of that
// table is older than v27, so this does not restate every column: an exact
// list would fail the moment a later migration appends another one, and it
// would also have to duplicate defaults that schema.sql already owns.
func verifySchemaV27(ctx context.Context, q schemaInspector) error {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(networks)`)
	if err != nil {
		return fmt.Errorf("store: schema v27 attestation: %w", err)
	}
	defer rows.Close()
	var (
		found schemaColumn
		seen  bool
	)
	for rows.Next() {
		var cid int
		var column schemaColumn
		var defaultSQL sql.NullString
		if err := rows.Scan(&cid, &column.name, &column.typeName, &column.notNull,
			&defaultSQL, &column.primaryKey); err != nil {
			return fmt.Errorf("store: schema v27 attestation: %w", err)
		}
		if column.name != "allow_router_access" {
			continue
		}
		seen = true
		column.typeName = strings.ToUpper(strings.TrimSpace(column.typeName))
		if defaultSQL.Valid {
			column.defaultSQL = schemaDefault(defaultSQL.String)
		}
		found = column
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: schema v27 attestation: %w", err)
	}
	if !seen {
		return fmt.Errorf("store: schema v27 attestation: networks.allow_router_access is missing")
	}
	if found.typeName != "INTEGER" || found.notNull != 1 || found.primaryKey != 0 ||
		found.defaultSQL == nil || *found.defaultSQL != "0" {
		return fmt.Errorf("store: schema v27 attestation: allow_router_access is %+v, want INTEGER NOT NULL DEFAULT 0", found)
	}
	return nil
}
