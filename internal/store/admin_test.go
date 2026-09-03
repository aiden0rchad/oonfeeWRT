package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSchema19MigratesLegacyAdminsWithoutLosingSchema18(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v18.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateFirstAdmin(ctx, "legacy-owner", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX admins_username_nocase;
ALTER TABLE admins DROP COLUMN deleted_at;
ALTER TABLE admins DROP COLUMN enabled;
ALTER TABLE admins DROP COLUMN role;
UPDATE schema_version SET version=18 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v18: %v", err)
	}
	defer db.Close()
	admin, err := db.AdminByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != RoleOwner || !admin.Enabled || admin.DeletedAt != nil {
		t.Fatalf("migrated admin=%+v, want enabled owner", admin)
	}
	var version, speedTables int
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
WHERE type='table' AND name='speed_tests'`).Scan(&speedTables); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || speedTables != 1 {
		t.Fatalf("version=%d speed_tests=%d, want %d and 1", version, speedTables, schemaVersion)
	}
}

func TestSchema19CaseCollisionFailsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "collision.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFirstAdmin(ctx, "Alice", "hash-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX admins_username_nocase;
INSERT INTO admins(username,pass_hash,created_at,role,enabled)
VALUES('alice','hash-b',1,'owner',1);
ALTER TABLE admins DROP COLUMN deleted_at;
ALTER TABLE admins DROP COLUMN enabled;
ALTER TABLE admins DROP COLUMN role;
UPDATE schema_version SET version=18 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if migrated, err := Open(ctx, driver, path, protector); err == nil {
		migrated.Close()
		t.Fatal("ASCII case-colliding legacy usernames migrated")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed: admins.username") {
		t.Fatalf("migration error=%v", err)
	}
	raw, err := sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, admins, roleColumns int
	if err := raw.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('admins') WHERE name='role'`).Scan(&roleColumns); err != nil {
		t.Fatal(err)
	}
	if version != 18 || admins != 2 || roleColumns != 0 {
		t.Fatalf("failed migration committed state: version=%d admins=%d role_columns=%d", version, admins, roleColumns)
	}
}

func TestSchema19AttestsUsernameCollation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "attestation.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DROP INDEX admins_username_nocase;
CREATE UNIQUE INDEX admins_username_nocase ON admins(username)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenReadOnly(ctx, driver, path, protector); err == nil {
		reopened.Close()
		t.Fatal("read-only open accepted a case-sensitive account index")
	} else if !strings.Contains(err.Error(), "not ASCII case-insensitive") {
		t.Fatalf("attestation error=%v", err)
	}
}

func TestAccountRolesLookupAndValidation(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, err := db.CreateFirstAdmin(ctx, "Owner", "hash-owner")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Role != RoleOwner || !owner.Enabled {
		t.Fatalf("first admin=%+v", owner)
	}
	if byCase, err := db.AdminByName(ctx, "oWnEr"); err != nil || byCase.ID != owner.ID {
		t.Fatalf("case-insensitive lookup=%+v err=%v", byCase, err)
	}
	actor := actorOf(owner)
	for _, role := range []AccountRole{RoleOwner, RoleAdmin, RoleOperator, RoleViewer} {
		name := "user-" + string(role)
		admin, err := db.CreateAdmin(ctx, name, "hash-"+name, role, actor)
		if err != nil || admin.Role != role || !admin.Enabled {
			t.Fatalf("create %s=%+v err=%v", role, admin, err)
		}
	}
	if _, err := db.CreateAdmin(ctx, "bad-role", "hash", AccountRole("superuser"), actor); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role error=%v", err)
	}
	for _, username := range []string{"", " leading", ".leading", "unicode-☃", strings.Repeat("a", 65)} {
		if _, err := db.CreateAdmin(ctx, username, "hash", RoleViewer, actor); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("username %q error=%v", username, err)
		}
	}
}

func TestNonOwnersCannotManageAccounts(t *testing.T) {
	for _, role := range []AccountRole{RoleAdmin, RoleOperator, RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			db := open(t)
			ctx := context.Background()
			owner, err := db.CreateFirstAdmin(ctx, "owner", "hash-owner")
			if err != nil {
				t.Fatal(err)
			}
			actor, err := db.CreateAdmin(ctx, "actor", "hash-actor", role, actorOf(owner))
			if err != nil {
				t.Fatal(err)
			}
			target, err := db.CreateAdmin(ctx, "target", "hash-target", RoleViewer, actorOf(owner))
			if err != nil {
				t.Fatal(err)
			}
			mutations := []struct {
				name string
				run  func() error
			}{
				{"create", func() error {
					_, err := db.CreateAdmin(ctx, "forbidden-create", "hash", RoleViewer, actorOf(actor))
					return err
				}},
				{"role", func() error {
					_, err := db.SetAdminRole(ctx, target.ID, RoleOperator, actorOf(actor))
					return err
				}},
				{"disable", func() error {
					_, err := db.SetAdminEnabled(ctx, target.ID, false, actorOf(actor))
					return err
				}},
				{"delete", func() error {
					return db.DeleteAdmin(ctx, target.ID, actorOf(actor))
				}},
				{"reset password", func() error {
					return db.ResetAdminPassword(ctx, target.ID, "changed-hash", actorOf(actor))
				}},
			}
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					if err := mutation.run(); !errors.Is(err, ErrAccountActorForbidden) {
						t.Fatalf("mutation error=%v, want ErrAccountActorForbidden", err)
					}
				})
			}
			got, err := db.AdminByID(ctx, target.ID)
			if err != nil || !got.Enabled || got.Role != RoleViewer || got.PassHash != "hash-target" {
				t.Fatalf("forbidden mutation changed target: %+v err=%v", got, err)
			}
			var escaped int
			if err := db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM admins WHERE username='forbidden-create'`).Scan(&escaped); err != nil {
				t.Fatal(err)
			}
			if escaped != 0 {
				t.Fatal("non-owner created an account")
			}
			if err := db.SetAdminPassword(ctx, actor.ID, "self-changed-hash"); err != nil {
				t.Fatalf("self password change: %v", err)
			}
			if err := db.RehashAdminPassword(ctx, actor.ID, "self-rehashed-hash"); err != nil {
				t.Fatalf("self password rehash: %v", err)
			}
			if got, err := db.AdminByID(ctx, actor.ID); err != nil || got.PassHash != "self-rehashed-hash" {
				t.Fatalf("self password paths account=%+v err=%v", got, err)
			}
		})
	}
}

func TestLastEnabledOwnerCannotBeChanged(t *testing.T) {
	for name, mutate := range map[string]func(context.Context, *DB, *Admin, AccountActor) error{
		"disable": func(ctx context.Context, db *DB, owner *Admin, actor AccountActor) error {
			_, err := db.SetAdminEnabled(ctx, owner.ID, false, actor)
			return err
		},
		"demote": func(ctx context.Context, db *DB, owner *Admin, actor AccountActor) error {
			_, err := db.SetAdminRole(ctx, owner.ID, RoleAdmin, actor)
			return err
		},
		"delete": func(ctx context.Context, db *DB, owner *Admin, actor AccountActor) error {
			return db.DeleteAdmin(ctx, owner.ID, actor)
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			ctx := context.Background()
			owner, err := db.CreateFirstAdmin(ctx, "owner", "hash")
			if err != nil {
				t.Fatal(err)
			}
			if err := mutate(ctx, db, owner, actorOf(owner)); !errors.Is(err, ErrLastOwner) {
				t.Fatalf("mutation error=%v, want ErrLastOwner", err)
			}
			got, err := db.AdminByID(ctx, owner.ID)
			if err != nil || !got.Enabled || got.Role != RoleOwner {
				t.Fatalf("last owner changed: %+v err=%v", got, err)
			}
		})
	}
}

func TestSoftDeleteReservesUsernameAndRemovesVerifier(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, err := db.CreateFirstAdmin(ctx, "owner", "hash-owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := db.CreateAdmin(ctx, "Viewer", "hash-viewer", RoleViewer, actorOf(owner))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAdmin(ctx, viewer.ID, actorOf(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminByName(ctx, "viewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted account authenticated: %v", err)
	}
	if _, err := db.AdminByID(ctx, viewer.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted account remained manageable: %v", err)
	}
	admins, err := db.Admins(ctx)
	if err != nil || len(admins) != 1 || admins[0].ID != owner.ID {
		t.Fatalf("admins=%+v err=%v", admins, err)
	}
	if count, err := db.AdminCount(ctx); err != nil || count != 2 {
		t.Fatalf("AdminCount=%d err=%v, want tombstone included", count, err)
	}
	var enabled int
	var deleted sql.NullInt64
	var hash string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT enabled,deleted_at,pass_hash FROM admins WHERE id=?`, viewer.ID).
		Scan(&enabled, &deleted, &hash); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || !deleted.Valid || hash != "!deleted" {
		t.Fatalf("tombstone enabled=%d deleted=%v hash=%q", enabled, deleted, hash)
	}
	if _, err := db.CreateAdmin(ctx, "vIeWeR", "new-hash", RoleViewer, actorOf(owner)); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("reserved username error=%v", err)
	}
}

func TestDisabledAccountCannotAuthenticateButCanBeReenabled(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, err := db.CreateFirstAdmin(ctx, "owner", "hash-owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := db.CreateAdmin(ctx, "viewer", "hash-viewer", RoleViewer, actorOf(owner))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAdminEnabled(ctx, viewer.ID, false, actorOf(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminByName(ctx, "viewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled account authenticated: %v", err)
	}
	if disabled, err := db.AdminByID(ctx, viewer.ID); err != nil || disabled.Enabled {
		t.Fatalf("disabled account=%+v err=%v", disabled, err)
	}
	if _, err := db.SetAdminEnabled(ctx, viewer.ID, true, actorOf(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminByName(ctx, "VIEWER"); err != nil {
		t.Fatalf("re-enabled account lookup: %v", err)
	}
}

func TestAccountMutationRollsBackWhenAuditInsertFails(t *testing.T) {
	for name, mutate := range map[string]func(context.Context, *DB, *Admin, *Admin) error{
		"create": func(ctx context.Context, db *DB, owner, _ *Admin) error {
			_, err := db.CreateAdmin(ctx, "new-user", "hash-new", RoleViewer, actorOf(owner))
			return err
		},
		"role": func(ctx context.Context, db *DB, owner, target *Admin) error {
			_, err := db.SetAdminRole(ctx, target.ID, RoleOperator, actorOf(owner))
			return err
		},
		"disable": func(ctx context.Context, db *DB, owner, target *Admin) error {
			_, err := db.SetAdminEnabled(ctx, target.ID, false, actorOf(owner))
			return err
		},
		"delete": func(ctx context.Context, db *DB, owner, target *Admin) error {
			return db.DeleteAdmin(ctx, target.ID, actorOf(owner))
		},
		"reset password": func(ctx context.Context, db *DB, owner, target *Admin) error {
			return db.ResetAdminPassword(ctx, target.ID, "changed-hash", actorOf(owner))
		},
		"self password": func(ctx context.Context, db *DB, _, target *Admin) error {
			return db.SetAdminPassword(ctx, target.ID, "changed-hash")
		},
		"rehash password": func(ctx context.Context, db *DB, _, target *Admin) error {
			return db.RehashAdminPassword(ctx, target.ID, "changed-hash")
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			ctx := context.Background()
			owner, err := db.CreateFirstAdmin(ctx, "owner", "hash-owner")
			if err != nil {
				t.Fatal(err)
			}
			target, err := db.CreateAdmin(ctx, "target", "hash-target", RoleViewer, actorOf(owner))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().ExecContext(ctx, `CREATE TRIGGER reject_account_audit
BEFORE INSERT ON events WHEN NEW.event LIKE 'auth.account_%'
BEGIN SELECT RAISE(ABORT,'audit rejected'); END`); err != nil {
				t.Fatal(err)
			}
			if err := mutate(ctx, db, owner, target); err == nil || !strings.Contains(err.Error(), "audit rejected") {
				t.Fatalf("mutation error=%v", err)
			}
			got, err := db.AdminByID(ctx, target.ID)
			if err != nil || got.Role != RoleViewer || !got.Enabled || got.PassHash != "hash-target" {
				t.Fatalf("mutation escaped audit rollback: %+v err=%v", got, err)
			}
			var newUsers int
			if err := db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM admins WHERE username='new-user'`).Scan(&newUsers); err != nil {
				t.Fatal(err)
			}
			if newUsers != 0 {
				t.Fatal("account creation committed without its audit row")
			}
		})
	}
}

func TestConcurrentMutationsPreserveAnEnabledOwner(t *testing.T) {
	type mutation struct {
		name  string
		event string
		run   func(context.Context, *DB, int64, AccountActor) error
	}
	mutations := []mutation{
		{"disable", "auth.account_disabled", func(ctx context.Context, db *DB, id int64, actor AccountActor) error {
			_, err := db.SetAdminEnabled(ctx, id, false, actor)
			return err
		}},
		{"demote", "auth.account_role_changed", func(ctx context.Context, db *DB, id int64, actor AccountActor) error {
			_, err := db.SetAdminRole(ctx, id, RoleAdmin, actor)
			return err
		}},
		{"delete", "auth.account_deleted", func(ctx context.Context, db *DB, id int64, actor AccountActor) error {
			return db.DeleteAdmin(ctx, id, actor)
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), mutation.name+".db")
			db1, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer db1.Close()
			first, err := db1.CreateFirstAdmin(ctx, "owner-a", "hash-a")
			if err != nil {
				t.Fatal(err)
			}
			second, err := db1.CreateAdmin(ctx, "owner-b", "hash-b", RoleOwner, actorOf(first))
			if err != nil {
				t.Fatal(err)
			}
			db2, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer db2.Close()

			start := make(chan struct{})
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			for _, attempt := range []struct {
				db    *DB
				admin *Admin
			}{{db1, first}, {db2, second}} {
				wg.Add(1)
				go func(attempt struct {
					db    *DB
					admin *Admin
				}) {
					defer wg.Done()
					<-start
					errs <- mutation.run(ctx, attempt.db, attempt.admin.ID, actorOf(attempt.admin))
				}(attempt)
			}
			close(start)
			wg.Wait()
			close(errs)
			var succeeded, protected int
			for err := range errs {
				switch {
				case err == nil:
					succeeded++
				case errors.Is(err, ErrLastOwner):
					protected++
				default:
					t.Fatalf("concurrent mutation error=%v", err)
				}
			}
			var owners, audits int
			if err := db1.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM admins
WHERE enabled=1 AND deleted_at IS NULL AND role='owner'`).Scan(&owners); err != nil {
				t.Fatal(err)
			}
			if err := db1.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM events WHERE event=?`, mutation.event).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if succeeded != 1 || protected != 1 || owners != 1 || audits != 1 {
				t.Fatalf("success=%d protected=%d owners=%d audits=%d", succeeded, protected, owners, audits)
			}
		})
	}
}

func actorOf(admin *Admin) AccountActor {
	return AccountActor{AdminID: admin.ID, Username: admin.Username, Address: "192.0.2.10"}
}

func TestAccountAuditDoesNotContainPasswordHash(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, err := db.CreateFirstAdmin(ctx, "owner", "secret-hash-owner")
	if err != nil {
		t.Fatal(err)
	}
	actor := actorOf(owner)
	actor.Username = "forged-name"
	if _, err := db.CreateAdmin(ctx, "viewer", "secret-hash-viewer", RoleViewer, actor); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := db.SQL().QueryRowContext(ctx, `SELECT detail_json FROM events
WHERE event='auth.account_created' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-hash-owner", "secret-hash-viewer", "pass_hash", "forged-name"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("audit detail leaked %q: %s", forbidden, detail)
		}
	}
	if !strings.Contains(detail, fmt.Sprintf(`"actor_admin_id":%d`, owner.ID)) {
		t.Errorf("audit detail lacks actor identity: %s", detail)
	}
	if !strings.Contains(detail, `"actor_username":"owner"`) {
		t.Errorf("audit detail did not canonicalize actor identity: %s", detail)
	}
	missing := AccountActor{AdminID: owner.ID + 1000, Username: "owner"}
	if _, err := db.CreateAdmin(ctx, "rolled-back", "hash", RoleViewer, missing); !errors.Is(err, ErrAccountActorInactive) {
		t.Fatalf("missing actor error=%v", err)
	}
	var rolledBack int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admins WHERE username='rolled-back'`).Scan(&rolledBack); err != nil {
		t.Fatal(err)
	}
	if rolledBack != 0 {
		t.Fatal("account creation committed with a nonexistent audit actor")
	}
}

func TestDisabledAndDeletedActorsAreRejectedAfterSelfMutation(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, err := db.CreateFirstAdmin(ctx, "owner", "hash-owner")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := db.CreateAdmin(ctx, "actor", "hash-actor", RoleOwner, actorOf(owner))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAdminEnabled(ctx, actor.ID, false, actorOf(actor)); err != nil {
		t.Fatalf("self-disable: %v", err)
	}
	if _, err := db.CreateAdmin(ctx, "disabled-write", "hash", RoleViewer, actorOf(actor)); !errors.Is(err, ErrAccountActorInactive) {
		t.Fatalf("disabled actor error=%v", err)
	}
	if _, err := db.SetAdminEnabled(ctx, actor.ID, true, actorOf(owner)); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAdmin(ctx, actor.ID, actorOf(actor)); err != nil {
		t.Fatalf("self-delete: %v", err)
	}
	if _, err := db.CreateAdmin(ctx, "deleted-write", "hash", RoleViewer, actorOf(actor)); !errors.Is(err, ErrAccountActorInactive) {
		t.Fatalf("deleted actor error=%v", err)
	}
	var escaped int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM admins
WHERE username IN ('disabled-write','deleted-write')`).Scan(&escaped); err != nil {
		t.Fatal(err)
	}
	if escaped != 0 {
		t.Fatalf("%d writes committed under inactive actors", escaped)
	}
}

func TestConcurrentActorMutationRechecksAuthorizationAfterWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update string
		event  string
		detail map[string]any
		want   error
	}{
		{"disable", `UPDATE admins SET enabled=0 WHERE id=?`, "auth.account_disabled",
			map[string]any{"enabled": false}, ErrAccountActorInactive},
		{"demote", `UPDATE admins SET role='admin' WHERE id=?`, "auth.account_role_changed",
			map[string]any{"role": RoleAdmin}, ErrAccountActorForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "actor-race.db")
			db1, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer db1.Close()
			owner, err := db1.CreateFirstAdmin(ctx, "owner", "hash-owner")
			if err != nil {
				t.Fatal(err)
			}
			actor, err := db1.CreateAdmin(ctx, "actor", "hash-actor", RoleOwner, actorOf(owner))
			if err != nil {
				t.Fatal(err)
			}
			db2, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer db2.Close()

			// The held immediate transaction fixes the serial order: the actor
			// transition commits before the competing management mutation can
			// resolve its authority.
			tx, err := db1.beginAdminTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			canonical, err := resolveAccountOwnerOn(ctx, tx, actorOf(owner))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, tc.update, actor.ID); err != nil {
				t.Fatal(err)
			}
			changed, err := adminByIDOn(ctx, tx, actor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := appendAccountAudit(ctx, tx, tc.event, canonical, changed, tc.detail); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				_, err := db2.CreateAdmin(ctx, "must-not-exist", "hash", RoleViewer, actorOf(actor))
				done <- err
			}()
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, tc.want) {
				t.Fatalf("concurrent mutation error=%v, want %v", err, tc.want)
			}
			var escaped int
			if err := db1.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM admins WHERE username='must-not-exist'`).Scan(&escaped); err != nil {
				t.Fatal(err)
			}
			if escaped != 0 {
				t.Fatal("mutation committed after its actor lost authorization")
			}
		})
	}
}
