package store

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// CurrentSchemaVersion is the exact migration level understood by this build.
func CurrentSchemaVersion() int { return schemaVersion }

// ProbeSchemaVersion reads an existing database's migration level without
// enabling WAL or changing the file.
func ProbeSchemaVersion(ctx context.Context, driverName, path string) (_ int, retErr error) {
	if ctx == nil {
		return 0, errors.New("store: schema probe context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("store: inspect schema probe database: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return 0, errors.New("store: schema probe requires a nonempty regular database, not a symlink")
	}
	db, err := openReadOnlySQL(ctx, driverName, path)
	if err != nil {
		return 0, err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	return currentSchema(ctx, db)
}
