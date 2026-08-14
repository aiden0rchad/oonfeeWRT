package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Admin is the controller's own operator account. Not a device credential —
// this is who is allowed to open the UI.
//
// PassHash is argon2id, computed by internal/secrets. It is deliberately not
// the SHA-512 crypt that internal/crypt produces: that exists to satisfy rpcd's
// on-device format, and its cost is set by what a router can afford. Nothing
// constrains this one to be cheap.
type Admin struct {
	ID        int64
	Username  string
	PassHash  string
	CreatedAt int64
	LastLogin *int64
}

// ErrNoAdmin means nobody has been enrolled yet. It is a distinct error because
// first-run setup and a wrong username must not look the same to a caller.
var ErrNoAdmin = errors.New("store: no administrator account exists")

// AdminByName looks up an operator.
func (db *DB) AdminByName(ctx context.Context, username string) (*Admin, error) {
	var a Admin
	err := db.sql.QueryRowContext(ctx,
		`SELECT id, username, pass_hash, created_at, last_login
		   FROM admins WHERE username = ?`, username).
		Scan(&a.ID, &a.Username, &a.PassHash, &a.CreatedAt, &a.LastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: look up admin: %w", err)
	}
	return &a, nil
}

// AdminCount reports how many operators exist, which is how first-run setup
// knows it is the first run.
func (db *DB) AdminCount(ctx context.Context) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count admins: %w", err)
	}
	return n, nil
}

// ErrAdminExists means an administrator account already exists, so first-run
// enrolment must not proceed.
var ErrAdminExists = errors.New("store: an administrator account already exists")

// CreateFirstAdmin enrols the FIRST operator, atomically.
//
// The conditional insert is the guard, not a count in the caller. Enrolment is
// unauthenticated and the caller has to derive an argon2id hash — tens of
// milliseconds — between checking and inserting, which is ample room for a
// second request to pass the same check. Two different usernames would then
// both insert cleanly, since only `username` is unique, and the endpoint's one
// security property ("works exactly once") would hold for sequential requests
// only.
//
// SQLite evaluates the WHERE NOT EXISTS and the insert in a single statement,
// so the race has nowhere to happen.
func (db *DB) CreateFirstAdmin(ctx context.Context, username, passHash string) (*Admin, error) {
	if username == "" || passHash == "" {
		return nil, errors.New("store: admin username and password hash are required")
	}
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO admins (username, pass_hash, created_at)
SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM admins)`, username, passHash, now)
	if err != nil {
		return nil, fmt.Errorf("store: create first admin %q: %w", username, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrAdminExists
	}
	id, _ := res.LastInsertId()
	return &Admin{ID: id, Username: username, PassHash: passHash, CreatedAt: now}, nil
}

// CreateAdmin enrolls an operator. The username is unique, so a second call
// with the same name fails rather than silently replacing a password.
func (db *DB) CreateAdmin(ctx context.Context, username, passHash string) (*Admin, error) {
	if username == "" || passHash == "" {
		return nil, errors.New("store: admin username and password hash are required")
	}
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO admins (username, pass_hash, created_at) VALUES (?,?,?)`,
		username, passHash, now)
	if err != nil {
		return nil, fmt.Errorf("store: create admin %q: %w", username, err)
	}
	id, _ := res.LastInsertId()
	return &Admin{ID: id, Username: username, PassHash: passHash, CreatedAt: now}, nil
}

// SetAdminPassword replaces an operator's password hash.
func (db *DB) SetAdminPassword(ctx context.Context, id int64, passHash string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE admins SET pass_hash=? WHERE id=?`, passHash, id)
	if err != nil {
		return fmt.Errorf("store: set admin password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAdminLogin records a successful sign-in.
func (db *DB) TouchAdminLogin(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE admins SET last_login=? WHERE id=?`, time.Now().Unix(), id)
	return err
}
