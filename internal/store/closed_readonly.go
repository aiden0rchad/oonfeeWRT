package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// OpenClosedReadOnly opens a checkpointed database which the caller guarantees
// cannot change for the handle's lifetime. SQLite's immutable mode avoids
// creating WAL/SHM sidecars while a startup restore validates parked files.
// Never use it for a live database: immutable mode intentionally ignores later
// writes and any WAL created after the handle opens.
func OpenClosedReadOnly(ctx context.Context, driverName, path string,
	protector SecretProtector) (*DB, error) {
	if protector == nil {
		return nil, errors.New("store: a secret protector is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	info, err := os.Lstat(absPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, errors.New("store: closed read-only database must be a nonempty regular file, not a symlink")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(absPath + suffix); err == nil {
			return nil, fmt.Errorf("store: closed read-only database has SQLite sidecar %s", filepath.Base(absPath)+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("store: inspect closed read-only database sidecar: %w", err)
		}
	}
	u := &url.URL{Scheme: "file", Path: absPath}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Set("_query_only", "1")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()

	sqldb, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open %s closed read-only: %w", path, err)
	}
	sqldb.SetMaxOpenConns(1)
	closeOnError := func(err error) (*DB, error) {
		sqldb.Close()
		return nil, err
	}
	if err := sqldb.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("store: open %s closed read-only: %w", path, err))
	}
	opened, err := os.Lstat(absPath)
	if err != nil || opened.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, opened) {
		return closeOnError(errors.New("store: closed read-only database identity changed while opening"))
	}
	current, err := currentSchema(ctx, sqldb)
	if err != nil {
		return closeOnError(err)
	}
	if current != schemaVersion {
		return closeOnError(fmt.Errorf("store: database is at schema v%d; closed read-only tools require v%d",
			current, schemaVersion))
	}
	if err := verifyCurrentSchema(ctx, sqldb); err != nil {
		return closeOnError(err)
	}
	db := &DB{sql: sqldb, protector: protector}
	complete, err := db.verifySecretState(ctx)
	if err != nil {
		return closeOnError(err)
	}
	if !complete {
		return closeOnError(errors.New("store: schema v14 secret scrub is incomplete"))
	}
	return db, nil
}
