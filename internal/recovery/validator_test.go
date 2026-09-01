package recovery_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	_ "modernc.org/sqlite"
)

type countingVerifier struct {
	keeper *secrets.Keeper
	calls  int
	err    error
}

type mutatingVerifier struct {
	keeper *secrets.Keeper
	mutate func() error
}

type cancelingVerifier struct {
	keeper *secrets.Keeper
	cancel context.CancelFunc
}

func (v *mutatingVerifier) VerifyCredential(mac string, blob []byte) error {
	if err := v.keeper.VerifyCredential(mac, blob); err != nil {
		return err
	}
	return v.mutate()
}

func (v *cancelingVerifier) VerifyCredential(mac string, blob []byte) error {
	if err := v.keeper.VerifyCredential(mac, blob); err != nil {
		return err
	}
	v.cancel()
	return nil
}

func (v *countingVerifier) VerifyCredential(mac string, blob []byte) error {
	v.calls++
	if v.err != nil {
		return v.err
	}
	return v.keeper.VerifyCredential(mac, blob)
}

func newValidatorDB(t *testing.T, owner bool) (*store.DB, *secrets.Keeper) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	keeper, err := secrets.Create(filepath.Join(dir, secrets.FileName),
		[]byte("validator-runtime-passphrase"), secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, "sqlite", filepath.Join(dir, "controller.db"), keeper)
	if err != nil {
		keeper.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if err := keeper.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Site(ctx); err != nil {
		t.Fatal(err)
	}
	if owner {
		passwordHash, err := secrets.HashPassword([]byte("restore-owner-password"),
			secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateFirstAdmin(ctx, "restore-owner", passwordHash); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	credential, err := keeper.SealCredential("02:00:00:00:00:19", "private-user", "private-password")
	if err != nil {
		t.Fatal(err)
	}
	device := &store.Device{
		MAC: "02:00:00:00:00:19", Host: "192.0.2.19", Name: "restore-device",
		Role: "ap", Functions: []string{"ap"}, AdoptedAt: &now, CredEnc: credential,
	}
	if err := db.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	return db, keeper
}

func TestValidateReturnsCountsAndUsesVerifier(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	verifier := &countingVerifier{keeper: keeper}
	var changesBefore int64
	if err := db.SQL().QueryRow(`SELECT total_changes()`).Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}

	counts, err := recovery.Validate(context.Background(), db, verifier)
	if err != nil {
		t.Fatal(err)
	}
	want := recovery.Counts{Schema: 20, Devices: 1, Credentials: 1}
	if counts != want {
		t.Fatalf("counts=%+v, want %+v", counts, want)
	}
	if verifier.calls != 1 {
		t.Fatalf("credential verifier calls=%d, want 1", verifier.calls)
	}
	var changesAfter int64
	if err := db.SQL().QueryRow(`SELECT total_changes()`).Scan(&changesAfter); err != nil {
		t.Fatal(err)
	}
	if changesAfter != changesBefore {
		t.Fatalf("writable database changes=%d after validation, want %d", changesAfter, changesBefore)
	}
}

func TestValidateAcceptsSecuredWLANWithoutRetainingItsKey(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	addSecuredSiteState(t, db)

	counts, err := recovery.Validate(context.Background(), db, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if counts.WLANs != 1 || counts.Meshes != 1 {
		t.Fatalf("counts=%+v, want one secured WLAN and mesh", counts)
	}
}

func TestValidateRequiresAnEnabledOwnerForCurrentSchema(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate string
	}{
		{name: "missing"},
		{name: "disabled", mutate: `UPDATE admins SET enabled=0`},
		{name: "deleted", mutate: `UPDATE admins SET deleted_at=1`},
		{name: "non-owner", mutate: `UPDATE admins SET role='admin'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, keeper := newValidatorDB(t, test.name != "missing")
			if test.mutate != "" {
				if _, err := db.SQL().Exec(test.mutate); err != nil {
					t.Fatal(err)
				}
			}
			_, err := recovery.Validate(context.Background(), db, keeper)
			if err == nil || err.Error() != "recovery database has no enabled owner account" {
				t.Fatalf("error=%v, want enabled-owner rejection", err)
			}
		})
	}
}

func TestValidateDoesNotExposeVerifierErrors(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	const private = "private-credential-verifier-sentinel"
	verifier := &countingVerifier{keeper: keeper, err: errors.New(private)}

	_, err := recovery.Validate(context.Background(), db, verifier)
	if err == nil || strings.Contains(err.Error(), private) {
		t.Fatalf("error exposed verifier detail: %v", err)
	}
	if err.Error() != "a stored device credential failed verification" {
		t.Fatalf("error=%v", err)
	}
}

func TestValidateRejectsInvalidAdminPasswordHashes(t *testing.T) {
	for _, test := range []struct{ name, invalid string }{
		{"empty", ""},
		{"plaintext", "plaintext-owner-secret"},
		{"oversized", strings.Repeat("x", 1025)},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, keeper := newValidatorDB(t, true)
			if _, err := db.SQL().Exec(`UPDATE admins SET pass_hash=?`, test.invalid); err != nil {
				t.Fatal(err)
			}
			_, err := recovery.Validate(context.Background(), db, keeper)
			if err == nil || err.Error() != "controller account validation failed" ||
				test.invalid != "" && strings.Contains(err.Error(), test.invalid) {
				t.Fatalf("error=%v, want redacted password-hash rejection", err)
			}
		})
	}
}

func TestValidateRejectsLegacyPlaintextSecretState(t *testing.T) {
	const secret = "legacy-secret-placeholder"
	for _, test := range []struct {
		name      string
		statement string
	}{
		{"mesh key", `UPDATE meshes SET key='` + secret + `'`},
		{"WLAN JSON key", `UPDATE wlans SET security_json='{"mode":"psk2","pmf":"disabled","key":"` + secret + `"}'`},
		{"ownership verifier", `UPDATE owned_sections SET rendered_hash='` + secret + `'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, keeper := newValidatorDB(t, true)
			addSecuredSiteState(t, db)
			device, err := db.DeviceByMAC(context.Background(), "02:00:00:00:00:19")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.RecordOwned(context.Background(), []store.OwnedSection{{
				DeviceID: device.ID, Config: "wireless", Section: "restore", RenderedHash: "hash",
			}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().Exec(test.statement); err != nil {
				t.Fatal(err)
			}
			_, err = recovery.Validate(context.Background(), db, keeper)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("error=%v, want redacted legacy-secret rejection", err)
			}
		})
	}
}

func TestValidateReauthenticatesSecretState(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	if _, err := db.SQL().Exec(`UPDATE secret_state SET key_check=x'01' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	_, err := recovery.Validate(context.Background(), db, keeper)
	if err == nil || err.Error() != "recovery database secret state failed verification" {
		t.Fatalf("error=%v, want key-check rejection", err)
	}
}

func TestValidateRejectsUnboundedStateBeforeCredentialVerification(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
		args      []any
	}{
		{"oversized text row", `UPDATE devices SET caps_json=?`, []any{strings.Repeat("x", 257<<10)}},
		{"oversized ciphertext", `UPDATE devices SET cred_enc=zeroblob(65537)`, nil},
		{"too many networks", `WITH RECURSIVE seq(n) AS (
			SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<257
		) INSERT INTO networks(name,vlan,cidr,zone,dhcp_json,ipv6_json,enabled)
		  SELECT 'bounded-'||n,n,'','bounded-'||n,'{}','{}',0 FROM seq`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, keeper := newValidatorDB(t, true)
			if _, err := db.SQL().Exec(test.statement, test.args...); err != nil {
				t.Fatal(err)
			}
			verifier := &countingVerifier{keeper: keeper}
			_, err := recovery.Validate(context.Background(), db, verifier)
			if err == nil || !strings.Contains(err.Error(), "bounds") {
				t.Fatalf("error=%v, want bounds rejection", err)
			}
			if verifier.calls != 0 {
				t.Fatalf("credential verifier called %d times before preflight rejection", verifier.calls)
			}
		})
	}
}

func TestValidateUsesOneSnapshot(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	device, err := db.DeviceByMAC(context.Background(), "02:00:00:00:00:19")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordOwned(context.Background(), []store.OwnedSection{{
		DeviceID: device.ID, Config: "wireless", Section: "snapshot", RenderedHash: "hash",
	}}); err != nil {
		t.Fatal(err)
	}
	path := databasePath(t, db)
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	verifier := &mutatingVerifier{keeper: keeper, mutate: func() error {
		_, err := writer.Exec(`DELETE FROM owned_sections`)
		return err
	}}

	counts, err := recovery.Validate(context.Background(), db, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if counts.OwnedSections != 1 {
		t.Fatalf("owned-section count=%d, want snapshot count 1", counts.OwnedSections)
	}
	var remaining int
	if err := writer.QueryRow(`SELECT COUNT(*) FROM owned_sections`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("concurrent mutation did not commit: %d rows remain", remaining)
	}
}

func TestValidatePreservesCancellation(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	if _, err := recovery.Validate(nil, db, keeper); err == nil {
		t.Fatal("nil context was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recovery.Validate(ctx, db, keeper); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	verifier := &cancelingVerifier{keeper: keeper, cancel: cancel}
	if _, err := recovery.Validate(ctx, db, verifier); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-traversal error=%v, want context.Canceled", err)
	}
}

func TestValidateRequiresExactSchema(t *testing.T) {
	for _, version := range []int{18, 21} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			db, keeper := newValidatorDB(t, true)
			if _, err := db.SQL().Exec(`UPDATE schema_version SET version=?
				WHERE version=(SELECT MAX(version) FROM schema_version)`, version); err != nil {
				t.Fatal(err)
			}
			_, err := recovery.Validate(context.Background(), db, keeper)
			if err == nil || err.Error() != "recovery database schema is unsupported" {
				t.Fatalf("schema %d error=%v", version, err)
			}
		})
	}
}

func TestValidateDoesNotCreateAMissingSiteOnWritableInput(t *testing.T) {
	db, keeper := newValidatorDB(t, true)
	if _, err := db.SQL().Exec(`DELETE FROM site`); err != nil {
		t.Fatal(err)
	}

	_, err := recovery.Validate(context.Background(), db, keeper)
	if err == nil || err.Error() != "stored site could not be opened" {
		t.Fatalf("error=%v, want missing-site rejection", err)
	}
	var rows int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM site`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("validation created %d site rows", rows)
	}
}

func TestValidateRejectsMissingInputs(t *testing.T) {
	if _, err := recovery.Validate(context.Background(), nil, nil); err == nil {
		t.Fatal("nil database was accepted")
	}
	db, _ := newValidatorDB(t, true)
	if _, err := recovery.Validate(context.Background(), db, nil); err == nil {
		t.Fatal("nil verifier was accepted")
	}
}

func addSecuredSiteState(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	network := &model.Network{Name: "restore-network", VLAN: 19, CIDR: "192.0.2.1/24",
		Zone: "restore", Enabled: false}
	if err := db.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	device, err := db.DeviceByMAC(ctx, "02:00:00:00:00:19")
	if err != nil {
		t.Fatal(err)
	}
	group := &model.APGroup{Name: "restore-group", DeviceIDs: []int64{device.ID}}
	if err := db.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	wlan := &model.WLAN{SSID: "restore-wlan", NetworkID: network.ID, GroupID: group.ID,
		Bands: []model.Band{model.Band2G}, Security: model.Security{
			Mode: model.SecPSK2, Key: "restore-wlan-key", PMF: model.PMFDisabled,
		}, Enabled: true}
	if err := db.SaveWLAN(ctx, wlan); err != nil {
		t.Fatal(err)
	}
	mesh := &model.Mesh{MeshID: "restore-mesh", NetworkID: network.ID, GroupID: group.ID,
		Band: model.Band5G, Key: "restore-mesh-key", Enabled: true}
	if err := db.SaveMesh(ctx, mesh); err != nil {
		t.Fatal(err)
	}
}

func databasePath(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.SQL().Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var name, path string
		if err := rows.Scan(&sequence, &name, &path); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			return path
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("main database path was unavailable")
	return ""
}
