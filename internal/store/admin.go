package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AccountRole is a controller account's authorization tier. Route-level
// authorization is deliberately outside this store-only schema slice.
type AccountRole string

const (
	RoleOwner    AccountRole = "owner"
	RoleAdmin    AccountRole = "admin"
	RoleOperator AccountRole = "operator"
	RoleViewer   AccountRole = "viewer"
)

func (r AccountRole) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleOperator, RoleViewer:
		return true
	default:
		return false
	}
}

// Admin is a controller account, not a device credential.
// PassHash is argon2id, computed by internal/secrets.
type Admin struct {
	ID        int64
	Username  string
	PassHash  string
	CreatedAt int64
	LastLogin *int64
	Role      AccountRole
	Enabled   bool
	DeletedAt *int64
}

// AccountActor is the authenticated controller account responsible for a
// management mutation. Address is optional but useful in the audit bundle.
type AccountActor struct {
	AdminID  int64
	Username string
	Address  string
}

var (
	// ErrNoAdmin means nobody has been enrolled yet.
	ErrNoAdmin = errors.New("store: no administrator account exists")
	// ErrAdminExists guards the unauthenticated first-run endpoint.
	ErrAdminExists = errors.New("store: an administrator account already exists")
	// ErrAccountExists includes a username reserved by a soft-deleted account.
	ErrAccountExists        = errors.New("store: controller account username already exists")
	ErrInvalidRole          = errors.New("store: invalid controller account role")
	ErrLastOwner            = errors.New("store: the last enabled owner cannot be disabled, demoted, or deleted")
	ErrAccountActorInactive = errors.New(
		"store: account mutation actor is disabled, deleted, or does not exist")
	ErrAccountActorForbidden = errors.New(
		"store: account mutation actor is not an owner")
	ErrInvalidUsername = errors.New(
		"store: account username must be 1-64 ASCII characters, start with a letter or digit, and contain only letters, digits, '.', '_' or '-'")
)

const adminColumns = `id,username,pass_hash,created_at,last_login,role,enabled,deleted_at`

// AdminByName looks up an account that may authenticate. Disabled and deleted
// accounts deliberately look absent so the API can spend its fixed fake-hash
// work and return the same response as an unknown username.
func (db *DB) AdminByName(ctx context.Context, username string) (*Admin, error) {
	return scanAdmin(db.sql.QueryRowContext(ctx, `SELECT `+adminColumns+`
FROM admins WHERE username=? COLLATE NOCASE AND enabled=1 AND deleted_at IS NULL`, username))
}

// AdminByID returns a non-deleted account, including a disabled one.
func (db *DB) AdminByID(ctx context.Context, id int64) (*Admin, error) {
	return adminByIDOn(ctx, db.sql, id)
}

// Admins lists non-deleted accounts. Tombstones reserve usernames but are not
// an account-management surface.
func (db *DB) Admins(ctx context.Context) ([]*Admin, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+adminColumns+`
FROM admins WHERE deleted_at IS NULL ORDER BY username COLLATE NOCASE,id`)
	if err != nil {
		return nil, fmt.Errorf("store: list admins: %w", err)
	}
	defer rows.Close()
	var admins []*Admin
	for rows.Next() {
		admin, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		admins = append(admins, admin)
	}
	return admins, rows.Err()
}

// AdminCount counts every account row, including tombstones. Deleting an
// account must never reopen unauthenticated first-run setup.
func (db *DB) AdminCount(ctx context.Context) (int, error) {
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count admins: %w", err)
	}
	return n, nil
}

// CreateFirstAdmin atomically creates the only account allowed without an
// authenticated actor. It is always an enabled owner.
func (db *DB) CreateFirstAdmin(ctx context.Context, username, passHash string) (*Admin, error) {
	if err := validateNewAccount(username, passHash); err != nil {
		return nil, err
	}
	tx, err := db.beginAdminTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin first admin creation: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
INSERT INTO admins (username,pass_hash,created_at,role,enabled)
SELECT ?,?,?,?,1 WHERE NOT EXISTS (SELECT 1 FROM admins)`, username, passHash, now, RoleOwner)
	if err != nil {
		return nil, fmt.Errorf("store: create first admin %q: %w", username, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: inspect first admin creation: %w", err)
	}
	if n == 0 {
		return nil, ErrAdminExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: read first admin id: %w", err)
	}
	admin := &Admin{ID: id, Username: username, PassHash: passHash, CreatedAt: now,
		Role: RoleOwner, Enabled: true}
	actor := AccountActor{AdminID: id, Username: username}
	if err := appendAccountAudit(ctx, tx, "auth.account_created", actor, admin,
		map[string]any{"bootstrap": true, "role": RoleOwner, "enabled": true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit first admin creation: %w", err)
	}
	return admin, nil
}

// CreateAdmin creates an enabled account and its security audit row in the
// same transaction.
func (db *DB) CreateAdmin(ctx context.Context, username, passHash string, role AccountRole,
	actor AccountActor) (*Admin, error) {
	if err := validateNewAccount(username, passHash); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, ErrInvalidRole
	}
	if err := validateAccountActor(actor); err != nil {
		return nil, err
	}
	tx, err := db.beginAdminTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin account creation: %w", err)
	}
	defer tx.Rollback()
	actor, err = resolveAccountOwnerOn(ctx, tx, actor)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
INSERT INTO admins (username,pass_hash,created_at,role,enabled) VALUES (?,?,?,?,1)`,
		username, passHash, now, role)
	if err != nil {
		if isAdminUsernameConflict(err) {
			return nil, ErrAccountExists
		}
		return nil, fmt.Errorf("store: create admin %q: %w", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: read new admin id: %w", err)
	}
	admin := &Admin{ID: id, Username: username, PassHash: passHash, CreatedAt: now,
		Role: role, Enabled: true}
	if err := appendAccountAudit(ctx, tx, "auth.account_created", actor, admin,
		map[string]any{"role": role, "enabled": true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit account creation: %w", err)
	}
	return admin, nil
}

// SetAdminRole changes one account's role. The conditional UPDATE performs the
// last-owner check inside the write statement, so concurrent writers cannot
// both observe an owner that the other is about to demote.
func (db *DB) SetAdminRole(ctx context.Context, id int64, role AccountRole,
	actor AccountActor) (*Admin, error) {
	if !role.Valid() {
		return nil, ErrInvalidRole
	}
	if err := validateAccountActor(actor); err != nil {
		return nil, err
	}
	tx, err := db.beginAdminTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin account role change: %w", err)
	}
	defer tx.Rollback()
	actor, err = resolveAccountOwnerOn(ctx, tx, actor)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE admins SET role=?
WHERE id=? AND deleted_at IS NULL AND role<>?
  AND NOT (enabled=1 AND role='owner' AND ?<>'owner' AND
    (SELECT COUNT(*) FROM admins WHERE enabled=1 AND deleted_at IS NULL AND role='owner')=1)`,
		role, id, role, role)
	if err != nil {
		return nil, fmt.Errorf("store: set admin role: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: inspect admin role change: %w", err)
	}
	admin, err := adminByIDOn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		if admin.Role == role {
			return admin, nil
		}
		if admin.Enabled && admin.Role == RoleOwner && role != RoleOwner {
			return nil, ErrLastOwner
		}
		return nil, errors.New("store: account role change did not match its target")
	}
	if err := appendAccountAudit(ctx, tx, "auth.account_role_changed", actor, admin,
		map[string]any{"role": role}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit account role change: %w", err)
	}
	return admin, nil
}

// SetAdminEnabled enables or disables an account without deleting its identity.
func (db *DB) SetAdminEnabled(ctx context.Context, id int64, enabled bool,
	actor AccountActor) (*Admin, error) {
	if err := validateAccountActor(actor); err != nil {
		return nil, err
	}
	want := 0
	if enabled {
		want = 1
	}
	tx, err := db.beginAdminTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin account state change: %w", err)
	}
	defer tx.Rollback()
	actor, err = resolveAccountOwnerOn(ctx, tx, actor)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE admins SET enabled=?
WHERE id=? AND deleted_at IS NULL AND enabled<>?
  AND NOT (?=0 AND enabled=1 AND role='owner' AND
    (SELECT COUNT(*) FROM admins WHERE enabled=1 AND deleted_at IS NULL AND role='owner')=1)`,
		want, id, want, want)
	if err != nil {
		return nil, fmt.Errorf("store: set admin enabled state: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: inspect admin state change: %w", err)
	}
	admin, err := adminByIDOn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		if admin.Enabled == enabled {
			return admin, nil
		}
		if !enabled && admin.Enabled && admin.Role == RoleOwner {
			return nil, ErrLastOwner
		}
		return nil, errors.New("store: account state change did not match its target")
	}
	event := "auth.account_disabled"
	if enabled {
		event = "auth.account_enabled"
	}
	if err := appendAccountAudit(ctx, tx, event, actor, admin,
		map[string]any{"enabled": enabled}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit account state change: %w", err)
	}
	return admin, nil
}

// DeleteAdmin reserves the username permanently, disables authentication and
// removes the password verifier. The row remains for stable identity/audit use.
func (db *DB) DeleteAdmin(ctx context.Context, id int64, actor AccountActor) error {
	if err := validateAccountActor(actor); err != nil {
		return err
	}
	tx, err := db.beginAdminTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin account deletion: %w", err)
	}
	defer tx.Rollback()
	actor, err = resolveAccountOwnerOn(ctx, tx, actor)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `UPDATE admins
SET enabled=0,deleted_at=?,pass_hash='!deleted'
WHERE id=? AND deleted_at IS NULL
  AND NOT (enabled=1 AND role='owner' AND
    (SELECT COUNT(*) FROM admins WHERE enabled=1 AND deleted_at IS NULL AND role='owner')=1)`,
		now, id)
	if err != nil {
		return fmt.Errorf("store: delete admin: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect admin deletion: %w", err)
	}
	if changed == 0 {
		admin, lookupErr := adminByIDOn(ctx, tx, id)
		if errors.Is(lookupErr, ErrNotFound) {
			return ErrNotFound
		}
		if lookupErr != nil {
			return lookupErr
		}
		if admin.Enabled && admin.Role == RoleOwner {
			return ErrLastOwner
		}
		return errors.New("store: account deletion did not match its target")
	}
	admin, err := adminByIDIncludingDeletedOn(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := appendAccountAudit(ctx, tx, "auth.account_deleted", actor, admin,
		map[string]any{"deleted_at": now}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit account deletion: %w", err)
	}
	return nil
}

// ResetAdminPassword is the owner management path. It may reset a
// disabled account, but never a deleted one.
func (db *DB) ResetAdminPassword(ctx context.Context, id int64, passHash string,
	actor AccountActor) error {
	if passHash == "" {
		return errors.New("store: admin password hash is required")
	}
	if err := validateAccountActor(actor); err != nil {
		return err
	}
	return db.setAdminPassword(ctx, id, passHash, actor, "auth.account_password_reset", false)
}

// SetAdminPassword is the existing self-change path. Its audit actor is the
// account itself and disabled accounts cannot use it.
func (db *DB) SetAdminPassword(ctx context.Context, id int64, passHash string) error {
	if passHash == "" {
		return errors.New("store: admin password hash is required")
	}
	return db.setAdminPassword(ctx, id, passHash, AccountActor{}, "auth.account_password_changed", true)
}

// RehashAdminPassword upgrades Argon2 parameters after a successful login.
// Keeping it separate prevents maintenance from being reported as a password
// change initiated by the account holder.
func (db *DB) RehashAdminPassword(ctx context.Context, id int64, passHash string) error {
	if passHash == "" {
		return errors.New("store: admin password hash is required")
	}
	return db.setAdminPassword(ctx, id, passHash, AccountActor{}, "auth.account_password_rehashed", true)
}

func (db *DB) setAdminPassword(ctx context.Context, id int64, passHash string,
	actor AccountActor, event string, requireEnabled bool) error {
	tx, err := db.beginAdminTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin admin password change: %w", err)
	}
	defer tx.Rollback()
	manager := actor.AdminID != 0
	if actor.AdminID == 0 {
		actor.AdminID = id
	}
	if manager {
		actor, err = resolveAccountOwnerOn(ctx, tx, actor)
	} else {
		actor, err = resolveAccountActorOn(ctx, tx, actor)
	}
	if err != nil {
		return err
	}
	query := `UPDATE admins SET pass_hash=? WHERE id=? AND deleted_at IS NULL`
	if requireEnabled {
		query += ` AND enabled=1`
	}
	res, err := tx.ExecContext(ctx, query, passHash, id)
	if err != nil {
		return fmt.Errorf("store: set admin password: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: inspect admin password change: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	admin, err := adminByIDOn(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := appendAccountAudit(ctx, tx, event, actor, admin, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit admin password change: %w", err)
	}
	return nil
}

// TouchAdminLogin records a successful sign-in. It is presence metadata, not
// an account-management mutation; the login event is recorded by the API.
func (db *DB) TouchAdminLogin(ctx context.Context, id int64) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE admins SET last_login=?
WHERE id=? AND enabled=1 AND deleted_at IS NULL`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: touch admin login: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

type adminQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// adminTx uses BEGIN IMMEDIATE so actor validity and the target mutation have
// one serial order across controller processes/DB handles. A deferred
// transaction would validate under a read lock, then race while upgrading it.
type adminTx struct {
	conn     *sql.Conn
	finished bool
}

func (db *DB) beginAdminTx(ctx context.Context) (*adminTx, error) {
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &adminTx{conn: conn}, nil
}

func (tx *adminTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx *adminTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (tx *adminTx) Commit(ctx context.Context) error {
	if _, err := tx.conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	tx.finished = true
	return tx.conn.Close()
}

func (tx *adminTx) Rollback() {
	if tx == nil || tx.finished {
		return
	}
	_, _ = tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	tx.finished = true
	_ = tx.conn.Close()
}

func adminByIDOn(ctx context.Context, q adminQueryer, id int64) (*Admin, error) {
	return scanAdmin(q.QueryRowContext(ctx, `SELECT `+adminColumns+`
FROM admins WHERE id=? AND deleted_at IS NULL`, id))
}

func adminByIDIncludingDeletedOn(ctx context.Context, q adminQueryer, id int64) (*Admin, error) {
	return scanAdmin(q.QueryRowContext(ctx, `SELECT `+adminColumns+` FROM admins WHERE id=?`, id))
}

func scanAdmin(row scanner) (*Admin, error) {
	var admin Admin
	var enabled int
	var deleted sql.NullInt64
	err := row.Scan(&admin.ID, &admin.Username, &admin.PassHash, &admin.CreatedAt,
		&admin.LastLogin, &admin.Role, &enabled, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan admin: %w", err)
	}
	if !admin.Role.Valid() || (enabled != 0 && enabled != 1) {
		return nil, errors.New("store: admin row has an invalid role or enabled state")
	}
	admin.Enabled = enabled == 1
	if deleted.Valid {
		value := deleted.Int64
		admin.DeletedAt = &value
	}
	return &admin, nil
}

func validateNewAccount(username, passHash string) error {
	if passHash == "" {
		return errors.New("store: admin password hash is required")
	}
	return ValidateAccountUsername(username)
}

// ValidateAccountUsername applies the ASCII grammar used by SQLite NOCASE.
// Existing legacy names remain readable; this is only a creation boundary.
func ValidateAccountUsername(username string) error {
	if username == "" || len(username) > 64 || strings.TrimSpace(username) != username {
		return ErrInvalidUsername
	}
	for i := 0; i < len(username); i++ {
		character := username[i]
		alphanumeric := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		if !alphanumeric && (i == 0 || character != '.' && character != '_' && character != '-') {
			return ErrInvalidUsername
		}
	}
	return nil
}

func validateAccountActor(actor AccountActor) error {
	if actor.AdminID <= 0 {
		return errors.New("store: account mutation requires an authenticated actor")
	}
	return nil
}

func resolveAccountActorOn(ctx context.Context, q adminQueryer, actor AccountActor) (AccountActor, error) {
	return resolveAccountActorForRoleOn(ctx, q, actor, "")
}

func resolveAccountOwnerOn(ctx context.Context, q adminQueryer, actor AccountActor) (AccountActor, error) {
	return resolveAccountActorForRoleOn(ctx, q, actor, RoleOwner)
}

func resolveAccountActorForRoleOn(ctx context.Context, q adminQueryer, actor AccountActor,
	required AccountRole) (AccountActor, error) {
	if err := validateAccountActor(actor); err != nil {
		return AccountActor{}, err
	}
	var role AccountRole
	if err := q.QueryRowContext(ctx, `SELECT username,role FROM admins
WHERE id=? AND enabled=1 AND deleted_at IS NULL`, actor.AdminID).Scan(&actor.Username, &role); errors.Is(err, sql.ErrNoRows) {
		return AccountActor{}, ErrAccountActorInactive
	} else if err != nil {
		return AccountActor{}, fmt.Errorf("store: resolve account mutation actor: %w", err)
	}
	if required != "" && role != required {
		return AccountActor{}, ErrAccountActorForbidden
	}
	return actor, nil
}

func isAdminUsernameConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed: admins.username")
}

func appendAccountAudit(ctx context.Context, tx *adminTx, event string, actor AccountActor,
	target *Admin, extra map[string]any) error {
	if actor.AdminID <= 0 || actor.Username == "" {
		return errors.New("store: account audit requires a resolved actor")
	}
	detail := map[string]any{
		"actor_admin_id":  actor.AdminID,
		"actor_username":  actor.Username,
		"target_admin_id": target.ID,
		"target_username": target.Username,
	}
	if actor.Address != "" {
		detail["addr"] = actor.Address
	}
	for key, value := range extra {
		detail[key] = value
	}
	audit, encoded, err := normalizeEvent(Event{
		Category: "audit", Severity: "info", Event: event, Detail: detail,
	})
	if err != nil {
		return fmt.Errorf("store: build account audit event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, appendEventSQL, eventInsertArgs(audit, encoded)...); err != nil {
		return fmt.Errorf("store: append account audit event: %w", err)
	}
	return nil
}
