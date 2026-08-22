package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	_ "modernc.org/sqlite" // pure-Go driver, decision D3 (CGO_ENABLED=0)
)

const driver = "sqlite"

func testProtector(t *testing.T, dbPath string) *secrets.Keeper {
	t.Helper()
	const passphrase = "placeholder store test passphrase"
	path := dbPath + ".keyring"
	keeper, err := secrets.Open(path, []byte(passphrase))
	if errors.Is(err, os.ErrNotExist) {
		keeper, err = secrets.Create(path, []byte(passphrase),
			secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	}
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	t.Cleanup(func() { keeper.Close() })
	return keeper
}

func open(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	db, err := Open(context.Background(), driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAppliesSchemaAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")

	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var mode string
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal — the whole write budget assumes it", mode)
	}
	var versionsBefore int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version`).Scan(&versionsBefore); err != nil {
		t.Fatalf("count initial schema_version: %v", err)
	}
	db.Close()

	// Re-opening an existing database must migrate cleanly, not double-apply.
	db2, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	var versions int
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version`).Scan(&versions); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if versions != versionsBefore {
		t.Errorf("reopen added schema rows: before=%d after=%d", versionsBefore, versions)
	}
}

func TestOpenReadOnlyCannotChangeTheDatabase(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"plain", "has#hash", "pct%20name", "with space"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "oonfee.db")
			db, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatalf("create database: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			ro, err := OpenReadOnly(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatalf("OpenReadOnly(%q): %v", path, err)
			}
			var queryOnly int
			if err := ro.SQL().QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
				t.Fatal(err)
			}
			if queryOnly != 1 {
				t.Errorf("query_only=%d, want 1", queryOnly)
			}
			if _, err := ro.SQL().ExecContext(ctx, `CREATE TABLE dryrun_must_not_write (id INTEGER)`); err == nil {
				t.Fatal("a write through the read-only handle succeeded")
			}
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Error("opening and querying read-only changed the database file")
			}
		})
	}
}

func TestOpenReadOnlyRefusesAnOutdatedSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE schema_version SET version=?`, schemaVersion-1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if ro, err := OpenReadOnly(ctx, driver, path, testProtector(t, path)); err == nil {
		ro.Close()
		t.Fatal("an outdated schema was accepted without a migration")
	}
}

func TestDHCPPolicyCreatesADowngradeBoundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for the last release, whose renderer ignored dhcp_json even
	// though the column already existed.
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM secret_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE schema_version SET version=9`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("migrate semantic boundary: %v", err)
	}
	defer db.Close()
	var got int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != schemaVersion {
		t.Fatalf("schema version = %d, want %d so an older binary refuses downgrade", got, schemaVersion)
	}
}

func TestDeviceFunctionsMigrationBackfillsLegacyRoleSemantics(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	for i, role := range []string{"gateway", "ap", "switch", "nonsense"} {
		if _, err := db.SQL().ExecContext(ctx, `INSERT INTO devices
			(mac, host, name, role, functions_json) VALUES (?,?,?,?,?)`,
			fmt.Sprintf("aa:bb:cc:dd:ee:%02d", i), "192.0.2.1", role, role, `["switch"]`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM secret_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE schema_version SET version=10`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("migrate functions: %v", err)
	}
	defer db.Close()
	devices, err := db.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"gateway":  "gateway,ap,switch",
		"ap":       "ap,switch",
		"switch":   "switch",
		"nonsense": "ap,switch",
	}
	for _, d := range devices {
		if got := strings.Join(d.Functions, ","); got != want[d.Name] {
			t.Errorf("legacy %s functions=%q, want %q", d.Name, got, want[d.Name])
		}
	}
}

func TestDeviceFunctionsRoundTripAndCanonicalisePrimaryRole(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{
		MAC: "aa:bb:cc:dd:ee:99", Host: "192.0.2.99", Name: "combo",
		Role: "switch", Functions: []string{"switch", "AP", "gateway", "ap"},
	}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.Role != "gateway" || strings.Join(d.Functions, ",") != "gateway,ap,switch" {
		t.Fatalf("upsert canonicalised to role=%q functions=%v", d.Role, d.Functions)
	}
	got, err := db.DeviceByID(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != d.Role || strings.Join(got.Functions, ",") != strings.Join(d.Functions, ",") {
		t.Fatalf("round trip got role=%q functions=%v, want role=%q functions=%v",
			got.Role, got.Functions, d.Role, d.Functions)
	}
}

func TestDeviceFunctionsRejectPresentEmptySelection(t *testing.T) {
	db := open(t)
	err := db.UpsertDevice(context.Background(), &Device{
		MAC: "aa:bb:cc:dd:ee:98", Host: "192.0.2.98", Name: "none",
		Functions: []string{},
	})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("empty functions error=%v", err)
	}
}

func TestCorruptStoredFunctionsStayVisibleButFailClosed(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{
		MAC: "aa:bb:cc:dd:ee:97", Host: "192.0.2.97", Name: "corrupt",
		Role: "gateway", Functions: []string{"gateway"},
	}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`[]`, `null`, `["gateway","made-up"]`, `{not-json`} {
		if _, err := db.SQL().ExecContext(ctx,
			`UPDATE devices SET functions_json=? WHERE id=?`, raw, d.ID); err != nil {
			t.Fatal(err)
		}
		got, err := db.DeviceByID(ctx, d.ID)
		if err != nil {
			t.Fatalf("corrupt row disappeared for %q: %v", raw, err)
		}
		if got.FunctionError == "" || got.Functions == nil || len(got.Functions) != 0 {
			t.Fatalf("raw %q loaded as functions=%v error=%q", raw, got.Functions, got.FunctionError)
		}
		if got.ModelDevice().EffectiveFunctions().Routes() ||
			got.ModelDevice().EffectiveFunctions().Wireless() {
			t.Fatalf("raw %q widened into active functions", raw)
		}
		if got.ModelDevice().Role != model.RoleGateway {
			t.Fatalf("raw %q changed canonical role to %q", raw, got.ModelDevice().Role)
		}
	}
}

func TestUpsertDeviceKeysOnMAC(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	d := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.1.1", Name: "gw"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if d.ID == 0 {
		t.Fatal("insert should populate the ID")
	}
	if d.Adopted() {
		t.Error("a device with no adopted_at is pending, not adopted")
	}

	// A device's address can change; its MAC is the identity, so this must
	// update rather than create a second row.
	now := time.Now().Unix()
	d2 := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.9.9", Name: "gw",
		AdoptedAt: &now, Class: "A", CertFP: "deadbeef"}
	if err := db.UpsertDevice(ctx, d2); err != nil {
		t.Fatalf("update: %v", err)
	}
	all, err := db.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("MAC is the identity; want 1 device, got %d", len(all))
	}
	got := all[0]
	if got.Host != "192.168.9.9" || got.Class != "A" || got.CertFP != "deadbeef" {
		t.Errorf("update did not take: %+v", got)
	}
	if !got.Adopted() {
		t.Error("adopted_at was set, so Adopted() should be true")
	}
	if got.ID != d.ID {
		t.Errorf("row identity changed on update: %d -> %d", d.ID, got.ID)
	}
}

func TestDeviceByMACReportsNotFound(t *testing.T) {
	db := open(t)
	if _, err := db.DeviceByMAC(context.Background(), "no:su:ch:de:vi:ce"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// owned_sections is how the reconciler tells our config from a human's, so it
// has to survive re-application without duplicating.
func TestOwnedSectionsUpsertAndForget(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:00:11:22", Host: "10.0.0.1", Name: "ap"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("device: %v", err)
	}

	secs := []OwnedSection{
		{DeviceID: d.ID, Config: "wireless", Section: "default_radio0", RenderedHash: "h1"},
		{DeviceID: d.ID, Config: "wireless", Section: "default_radio1", RenderedHash: "h2"},
	}
	if err := db.RecordOwned(ctx, secs); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}
	// Re-applying the same sections with new hashes must update in place.
	secs[0].RenderedHash = "h1-updated"
	if err := db.RecordOwned(ctx, secs); err != nil {
		t.Fatalf("RecordOwned again: %v", err)
	}
	got, err := db.OwnedSections(ctx, d.ID)
	if err != nil {
		t.Fatalf("OwnedSections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 owned sections, got %d", len(got))
	}
	if got[0].RenderedHash != "h1-updated" {
		t.Errorf("hash should update in place, got %q", got[0].RenderedHash)
	}
	if got[0].AppliedAt == 0 {
		t.Error("applied_at should default to now")
	}

	if err := db.ForgetOwned(ctx, d.ID); err != nil {
		t.Fatalf("ForgetOwned: %v", err)
	}
	got, _ = db.OwnedSections(ctx, d.ID)
	if len(got) != 0 {
		t.Errorf("un-adopt should drop every ownership claim, got %d", len(got))
	}
}

// Deleting a device must not leave orphaned ownership claims behind, or a
// re-adopted device inherits stale beliefs about what we own.
func TestDeletingADeviceCascadesToOwnedSections(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:33:44:55", Host: "10.0.0.2", Name: "ap2"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("device: %v", err)
	}
	if err := db.RecordOwned(ctx, []OwnedSection{
		{DeviceID: d.ID, Config: "firewall", Section: "r1", RenderedHash: "h"}}); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, d.ID); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM owned_sections WHERE device_id=?`, d.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("foreign_keys=ON should cascade the delete, got %d orphans", n)
	}
}

func TestEventsRoundTripNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	for i, name := range []string{"first", "second", "third"} {
		if err := db.LogEvent(ctx, Event{
			TS: int64(1000 + i), Category: "audit", Severity: "info",
			Event: name, Detail: map[string]string{"outcome": name},
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	got, err := db.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	if got[0].Event != "third" {
		t.Errorf("newest first: got %q", got[0].Event)
	}
	seen := map[int64]bool{}
	for _, event := range got {
		if event.ID == 0 {
			t.Fatal("event query omitted the database id used as the UI row identity")
		}
		if seen[event.ID] {
			t.Fatalf("event id %d was returned twice", event.ID)
		}
		seen[event.ID] = true
	}
}

func TestSetCapabilitiesStoresSnapshot(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:66:77:88", Host: "10.0.0.3", Name: "ap3"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("device: %v", err)
	}
	caps := map[string]any{"dsa": "present", "airtime-split": "absent"}
	if err := db.SetCapabilities(ctx, d.ID, caps, "A"); err != nil {
		t.Fatalf("SetCapabilities: %v", err)
	}
	got, err := db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.Class != "A" {
		t.Errorf("class = %q, want A", got.Class)
	}
	if got.CapsJSON == "" || got.CapsJSON == "{}" {
		t.Errorf("capability snapshot not stored: %q", got.CapsJSON)
	}
}

func TestCheckpointTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "oonfee.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i := range 50 {
		if err := db.LogEvent(ctx, Event{
			Category: "test", Severity: "info", Event: "fill",
			Detail: map[string]any{"i": i},
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// TRUNCATE means the WAL is emptied, not merely folded in. That is what
	// makes copying the volume file a valid backup.
	fi, err := os.Stat(path + "-wal")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat WAL: %v", err)
	}
	if err == nil && fi.Size() != 0 {
		t.Fatalf("WAL is %d bytes after a TRUNCATE checkpoint, want 0", fi.Size())
	}
}

// A pin that quietly replaces itself is not a pin, so the second write must be
// refused rather than accepted.
func TestSetCertFPIsTrustOnFirstUseOnly(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.1.1", Name: "ap1", Scheme: "https"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if err := db.SetCertFP(ctx, d.ID, "aabb"); err != nil {
		t.Fatalf("first SetCertFP: %v", err)
	}
	got, err := db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.CertFP != "aabb" {
		t.Fatalf("CertFP = %q, want %q", got.CertFP, "aabb")
	}
	if err := db.SetCertFP(ctx, d.ID, "ccdd"); err == nil {
		t.Fatal("SetCertFP silently replaced an existing pin")
	}
	got, err = db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.CertFP != "aabb" {
		t.Fatalf("pin changed to %q despite the refusal", got.CertFP)
	}
}

// The SSH pin has a stronger version of the same rule, and a second guard the
// certificate does not need.
//
// A certificate is re-derived on every https connection and legitimately
// rotates. An SSH host key is generated once at first boot, so a second one
// means the box was reflashed or is not the box — which is precisely what
// un-adopt must not walk into carrying the operator's password.
func TestTheSSHHostKeyPinSurvivesAnUpsert(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	// Adoption's own write, which is genuinely first use.
	d := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.1.1", Name: "ap1",
		HostKeyFP: "SHA256:adopted"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	got, err := db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.HostKeyFP != "SHA256:adopted" {
		t.Fatalf("adoption's pin did not survive the round trip: %q", got.HostKeyFP)
	}

	// An upsert that does not carry the field must not blank it. Every field on
	// Device is written by UpsertDevice, so any future caller that builds a row
	// from something other than an adoption result would otherwise silently
	// un-pin the device — and an un-pinned device is indistinguishable from one
	// that predates the column, so nothing would report it.
	if err := db.UpsertDevice(ctx, &Device{
		MAC: d.MAC, Host: "192.168.1.9", Name: "ap1"}); err != nil {
		t.Fatalf("second UpsertDevice: %v", err)
	}
	got, err = db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.HostKeyFP != "SHA256:adopted" {
		t.Fatalf("an upsert blanked the host key pin: %q", got.HostKeyFP)
	}
	if got.Host != "192.168.1.9" {
		t.Fatalf("the upsert did not take at all, so this proves nothing: %+v", got)
	}

	// Nor may an upsert quietly REPLACE it. That is the same non-guard wearing
	// a different hat: a pin that updates itself when something new answers is
	// not a pin.
	if err := db.UpsertDevice(ctx, &Device{
		MAC: d.MAC, Host: "192.168.1.9", Name: "ap1",
		HostKeyFP: "SHA256:someone-else"}); err != nil {
		t.Fatalf("third UpsertDevice: %v", err)
	}
	got, _ = db.DeviceByMAC(ctx, d.MAC)
	if got.HostKeyFP != "SHA256:adopted" {
		t.Fatalf("an upsert re-pinned the device to %q", got.HostKeyFP)
	}
}

func TestSetHostKeyFPIsTrustOnFirstUseOnly(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	// A device adopted before the column existed: no pin, so its next SSH is
	// unpinned by necessity and this is what makes the one after that checked.
	d := &Device{MAC: "aa:bb:cc:dd:ee:ff", Host: "192.168.1.1", Name: "ap1"}
	if err := db.UpsertDevice(ctx, d); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if err := db.SetHostKeyFP(ctx, d.ID, "SHA256:first"); err != nil {
		t.Fatalf("first SetHostKeyFP: %v", err)
	}
	got, err := db.DeviceByMAC(ctx, d.MAC)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if got.HostKeyFP != "SHA256:first" {
		t.Fatalf("HostKeyFP = %q, want %q", got.HostKeyFP, "SHA256:first")
	}

	if err := db.SetHostKeyFP(ctx, d.ID, "SHA256:second"); err == nil {
		t.Error("SetHostKeyFP silently replaced an existing pin")
	}
	got, _ = db.DeviceByMAC(ctx, d.MAC)
	if got.HostKeyFP != "SHA256:first" {
		t.Fatalf("pin changed to %q despite the refusal", got.HostKeyFP)
	}

	// An empty fingerprint is refused rather than stored. The caller reads it
	// from a Bootstrap, and a fake — or a bootstrap that never handshook —
	// returns "". Writing that would leave the column looking un-pinned while
	// having been "set", and the first-use branch would never run again.
	fresh := &Device{MAC: "11:22:33:44:55:66", Host: "192.168.1.2", Name: "ap2"}
	if err := db.UpsertDevice(ctx, fresh); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if err := db.SetHostKeyFP(ctx, fresh.ID, ""); err == nil {
		t.Error("an empty host key was accepted as a pin")
	}
}

// The whole point of a facet count is that it is NOT the count of what came
// back. UI-SPEC §5 says so, and the log endpoint returns a few hundred of what
// can be tens of thousands of rows — so a count taken from the page reports
// "3 errors" from a table holding three hundred, in the same typeface as a true
// number.
func TestEventFacetsCountTheTableNotThePage(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// 300 events: 250 info/system, 50 error/device.
	for i := 0; i < 250; i++ {
		if err := db.LogEvent(ctx, Event{TS: int64(1000 + i),
			Category: "system", Severity: "info", Event: "tick"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := db.LogEvent(ctx, Event{TS: int64(2000 + i),
			Category: "device", Severity: "error", Event: "poll_failed"}); err != nil {
			t.Fatal(err)
		}
	}

	// A page far smaller than the table, which is the situation that makes
	// page-derived counts wrong.
	page, err := db.QueryEventsPage(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 10 {
		t.Fatalf("page holds %d rows, want 10", len(page))
	}

	cats, sevs, total, err := db.EventFacets(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300 — the total must count the table", total)
	}
	if got := facetCount(sevs, "info"); got != 250 {
		t.Errorf("info = %d, want 250 (the page held only %d rows in total)",
			got, len(page))
	}
	if got := facetCount(sevs, "error"); got != 50 {
		t.Errorf("error = %d, want 50", got)
	}
	if got := facetCount(cats, "system"); got != 250 {
		t.Errorf("system = %d, want 250", got)
	}
}

// A facet must be counted with the OTHER filters applied but not its own.
// Applying a facet to itself shows the selected option's count and zero next to
// every alternative, which cannot answer the only question the rail is for:
// how many would I get if I clicked that instead?
func TestEventFacetsExcludeTheirOwnFilter(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	seed := []Event{
		{TS: 1, Category: "system", Severity: "info", Event: "a"},
		{TS: 2, Category: "system", Severity: "error", Event: "b"},
		{TS: 3, Category: "device", Severity: "error", Event: "c"},
		{TS: 4, Category: "device", Severity: "error", Event: "d"},
		{TS: 5, Category: "audit", Severity: "info", Event: "e"},
	}
	for _, e := range seed {
		if err := db.LogEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// Severity "error" selected. The severity rail must still offer "info" with
	// a real count, or there is no way back.
	cats, sevs, total, err := db.EventFacets(ctx, "", "error")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 — the total DOES apply every filter", total)
	}
	if got := facetCount(sevs, "info"); got != 2 {
		t.Errorf("with severity=error selected, info = %d, want 2; a severity "+
			"facet must not filter on severity or the rail becomes one-way", got)
	}
	// The category rail, meanwhile, is scoped to the selected severity: those
	// counts answer "how many errors in each category", which is the question.
	if got := facetCount(cats, "device"); got != 2 {
		t.Errorf("category device = %d, want 2 (errors only)", got)
	}
	if got := facetCount(cats, "audit"); got != 0 {
		t.Errorf("audit has no errors, so it should be absent from the "+
			"error-scoped category rail; got %d", got)
	}
}

func facetCount(fs []Facet, value string) int {
	for _, f := range fs {
		if f.Value == value {
			return f.Count
		}
	}
	return 0
}

// Paging must not repeat or skip rows.
func TestEventPagingWalksTheTableExactlyOnce(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if err := db.LogEvent(ctx, Event{TS: int64(1000 + i),
			Category: "system", Severity: "info", Event: "e" + itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for offset := 0; offset < 25; offset += 10 {
		page, err := db.QueryEventsPage(ctx, "", "", 10, offset)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			if seen[e.Event] {
				t.Errorf("%s appeared on two pages", e.Event)
			}
			seen[e.Event] = true
		}
	}
	if len(seen) != 25 {
		t.Errorf("paging saw %d of 25 events", len(seen))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A determination must never be overwritten by a non-determination.
//
// The subnets are re-read every fifteen minutes and carried forward in between,
// but a poll that happens before the first read — or one against a device that
// refuses the call — reports "unknown" for every host. Letting that overwrite a
// correct classification would flicker clients in and out of the default view
// for reasons no operator could see.
func TestClientScopeIsNotDowngradedToUnknown(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:bb:cc:00:00:01", Name: "laptop", IPv4: "192.168.1.5", Scope: ScopeLocal},
	}, 1000); err != nil {
		t.Fatal(err)
	}
	// A later poll that could not classify anything.
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:bb:cc:00:00:01", Name: "laptop", IPv4: "192.168.1.5", Scope: ScopeUnknown},
	}, 2000); err != nil {
		t.Fatal(err)
	}
	got := clientByMAC(t, db, "aa:bb:cc:00:00:01")
	if got.Scope != ScopeLocal {
		t.Errorf("scope = %q after an unknown-scoped poll, want %q", got.Scope, ScopeLocal)
	}
	if got.LastSeen == nil || *got.LastSeen != 2000 {
		t.Errorf("last_seen was not updated by the second poll: %v", got.LastSeen)
	}
}

// A real reclassification must still land: a client that moves from one side of
// the router to the other is exactly what this column is for.
func TestClientScopeChangesWhenTheAnswerChanges(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:bb:cc:00:00:02", IPv4: "10.7.46.9", Scope: ScopeUpstream},
	}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:bb:cc:00:00:02", IPv4: "192.168.1.9", Scope: ScopeLocal},
	}, 2000); err != nil {
		t.Fatal(err)
	}
	if got := clientByMAC(t, db, "aa:bb:cc:00:00:02"); got.Scope != ScopeLocal {
		t.Errorf("scope = %q, want %q — a genuine reclassification must land",
			got.Scope, ScopeLocal)
	}
}

func TestClientLocalScopeWinsSameIPFleetConflict(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	const mac = "aa:bb:cc:00:00:04"

	if err := db.UpsertClients(ctx, []SeenClient{{
		MAC: mac, IPv4: "192.168.1.9", Scope: ScopeLocal,
	}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{{
		MAC: mac, IPv4: "192.168.1.9", Scope: ScopeUpstream,
	}}, 101); err != nil {
		t.Fatal(err)
	}
	if got := clientByMAC(t, db, mac); got.Scope != ScopeLocal {
		t.Errorf("scope = %q, want %q when a downstream AP sees the gateway LAN as upstream", got.Scope, ScopeLocal)
	}

	if err := db.UpsertClients(ctx, []SeenClient{{
		MAC: mac, IPv4: "10.7.46.9", Scope: ScopeUpstream,
	}}, 102); err != nil {
		t.Fatal(err)
	}
	if got := clientByMAC(t, db, mac); got.Scope != ScopeUpstream {
		t.Errorf("scope = %q after IP changed, want %q", got.Scope, ScopeUpstream)
	}
}

// A row from before the column existed reads as undetermined, not local.
func TestClientWithNoStoredScopeReadsAsUnknown(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO clients (mac, name, ip, first_seen, last_seen) VALUES (?,?,?,?,?)`,
		"aa:bb:cc:00:00:03", "old row", "192.168.1.7", 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if got := clientByMAC(t, db, "aa:bb:cc:00:00:03"); got.Scope != ScopeUnknown {
		t.Errorf("a pre-migration row reads as %q, want %q; defaulting it to "+
			"local would assert something never measured", got.Scope, ScopeUnknown)
	}
}

// The counts the dashboard and the client rail both read.
//
// The shape here is the one measured on the reference device and the reason the
// count was wrong: a handful of local clients among a majority of upstream
// neighbours, plus rows nothing has placed. Counting rows rather than scopes
// reported 6 devices "on the LAN" where 2 were.
func TestClientCountsByScope(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// Inventory timestamps keep the rows; only explicit reachability evidence
	// makes the first five active.
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:00:01", IPv4: "192.168.1.5", Scope: ScopeLocal},
		{MAC: "aa:00:00:00:00:02", IPv4: "192.168.1.6", Scope: ScopeLocal},
		{MAC: "aa:00:00:00:00:03", IPv4: "10.7.46.1", Scope: ScopeUpstream},
		{MAC: "aa:00:00:00:00:04", IPv4: "10.7.46.2", Scope: ScopeUpstream},
		{MAC: "aa:00:00:00:00:05", Scope: ScopeUnknown},
	}, 9500); err != nil {
		t.Fatal(err)
	}
	// One local client last seen long ago: known, but not active.
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:00:06", IPv4: "192.168.1.9", Scope: ScopeLocal},
	}, 1000); err != nil {
		t.Fatal(err)
	}

	active := []string{
		"aa:00:00:00:00:01", "aa:00:00:00:00:02", "aa:00:00:00:00:03",
		"aa:00:00:00:00:04", "aa:00:00:00:00:05",
	}
	counts, err := db.ClientCounts(ctx, ClientFilter{LiveActive: active})
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[ScopeLocal]; got.Total != 3 || got.Active != 2 {
		t.Errorf("local = %+v, want {Total:3 Active:2}", got)
	}
	if got := counts[ScopeUpstream]; got.Total != 2 || got.Active != 2 {
		t.Errorf("upstream = %+v, want {Total:2 Active:2}", got)
	}
	if got := counts[ScopeUnknown]; got.Total != 1 || got.Active != 1 {
		t.Errorf("unknown = %+v, want {Total:1 Active:1}", got)
	}

	// seenSince bounds what is counted at all — the client list's 24h window.
	recent, err := db.ClientCounts(ctx, ClientFilter{SeenSince: 9000, LiveActive: active})
	if err != nil {
		t.Fatal(err)
	}
	if got := recent[ScopeLocal].Total; got != 2 {
		t.Errorf("local total within the seen window = %d, want 2 "+
			"(the client last seen at t=1000 is outside it)", got)
	}
}

// The index the client list's connection rail depends on must exist, whether
// the database was created fresh or migrated.
//
// It is declared in both schema.sql and migration 6 — fresh databases get it
// from the former and existing ones from either, which is the same
// belt-and-braces the tables use. Asserted rather than assumed because the
// query works without it: it just scans series once per client row, so nothing
// fails, it only gets slow, and slow is not something a test notices.
func TestSeriesKindKeyIndexExists(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, path string)
	}{
		{"fresh", func(*testing.T, string) {}},
		{"migrated from v5", func(t *testing.T, path string) {
			db, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().ExecContext(ctx,
				`DROP INDEX IF EXISTS series_kind_key`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().ExecContext(ctx,
				`DELETE FROM secret_state;
				 DELETE FROM schema_version;
				 INSERT INTO schema_version (version, applied_at) VALUES (5, 0)`); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oonfee.db")
			tc.prep(t, path)
			db, err := Open(ctx, driver, path, testProtector(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var n int
			if err := db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master
				  WHERE type='index' AND name='series_kind_key'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Error("series_kind_key is missing; the connection facet " +
					"falls back to scanning series once per client row")
			}
		})
	}
}

// Filtering must happen in the database, not on the page it returned.
//
// This is the defect the event log had: filtering a fetched page selects from
// the newest N rows overall rather than the newest N matching, so a view
// filtered to a rare value comes back empty while rows with that value exist.
// Here the one upstream host is the OLDEST, so a client-side filter over a
// page of 2 would find nothing.
func TestClientsPageFiltersInTheDatabase(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:01:01", IPv4: "10.7.46.1", Scope: ScopeUpstream},
	}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:01:02", IPv4: "192.168.1.5", Scope: ScopeLocal},
		{MAC: "aa:00:00:00:01:03", IPv4: "192.168.1.6", Scope: ScopeLocal},
	}, 9000); err != nil {
		t.Fatal(err)
	}

	page, err := db.ClientsPage(ctx, ClientFilter{Scope: ScopeUpstream, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Clients) != 1 || page.Clients[0].MAC != "aa:00:00:00:01:01" {
		t.Fatalf("filtered page = %+v, want the one upstream host — filtering "+
			"after the fetch would have returned nothing", page.Clients)
	}
	if page.Total != 1 {
		t.Errorf("total = %d, want 1: the total counts matches, not rows fetched",
			page.Total)
	}
}

// The rail counts the whole filtered table, not the page.
//
// A rail counted over the returned page reports "1 upstream" from a page of 1
// while the table holds three — in the same typeface as a true number.
func TestClientsPageFacetsCountBeyondThePage(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	seen := []SeenClient{}
	for i := 0; i < 5; i++ {
		seen = append(seen, SeenClient{
			MAC: fmt.Sprintf("aa:00:00:00:02:%02d", i), Scope: ScopeUpstream,
		})
	}
	seen = append(seen, SeenClient{MAC: "aa:00:00:00:02:99", Scope: ScopeLocal})
	if err := db.UpsertClients(ctx, seen, 9000); err != nil {
		t.Fatal(err)
	}

	page, err := db.ClientsPage(ctx, ClientFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Clients) != 2 {
		t.Fatalf("page held %d rows, want 2", len(page.Clients))
	}
	if page.Total != 6 {
		t.Errorf("total = %d, want 6", page.Total)
	}
	got := map[string]int{}
	for _, f := range page.Scope {
		got[f.Value] = f.Count
	}
	if got[ScopeUpstream] != 5 || got[ScopeLocal] != 1 {
		t.Errorf("scope facet = %v, want 5 upstream and 1 local from a 2-row page", got)
	}
}

// Each rail counts with the OTHER filters applied but not its own.
//
// Applying a facet's own filter to its own count shows the selected value and
// zero beside everything else — a rail that can only ever narrow, and gives no
// answer to "how many would I get if I clicked that instead?".
func TestClientsPageFacetsExcludeTheirOwnFilter(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	// Two local (one current, one stale) and two upstream (both current).
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:03:01", Scope: ScopeLocal},
		{MAC: "aa:00:00:00:03:02", Scope: ScopeUpstream},
		{MAC: "aa:00:00:00:03:03", Scope: ScopeUpstream},
	}, 9000); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:03:04", Scope: ScopeLocal},
	}, 1000); err != nil {
		t.Fatal(err)
	}

	page, err := db.ClientsPage(ctx, ClientFilter{
		LiveActive: []string{"aa:00:00:00:03:01"}, Scope: ScopeLocal, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The scope rail ignores the scope filter, so upstream is still offered.
	scope := map[string]int{}
	for _, f := range page.Scope {
		scope[f.Value] = f.Count
	}
	if scope[ScopeUpstream] != 2 {
		t.Errorf("scope facet = %v; with 'local' selected it must still count "+
			"the 2 upstream hosts, or the rail cannot be un-narrowed", scope)
	}

	// The presence rail DOES respect the scope filter: of the two local hosts,
	// one is current and one is stale.
	presence := map[string]int{}
	for _, f := range page.Presence {
		presence[f.Value] = f.Count
	}
	if presence["online"] != 1 || presence["offline"] != 1 {
		t.Errorf("presence facet = %v, want 1 online and 1 offline among the "+
			"local hosts — the other rails' filters must apply", presence)
	}
}

func TestClientsPagePresenceRequiresEvidenceAndExcludesInfrastructure(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	staleHint := "aa:00:00:00:03:11"
	liveClient := "aa:00:00:00:03:12"
	infrastructure := "aa:00:00:00:03:13"
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: staleHint, Scope: ScopeLocal},
		{MAC: liveClient, Scope: ScopeLocal},
		{MAC: infrastructure, Scope: ScopeLocal},
	}, 10_000); err != nil {
		t.Fatal(err)
	}

	page, err := db.ClientsPage(ctx, ClientFilter{
		Presence: "online", LiveActive: []string{liveClient, infrastructure},
		ExcludeMACs: []string{infrastructure}, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Clients) != 1 || page.Clients[0].MAC != liveClient {
		t.Fatalf("online page=%+v", page)
	}
	presence := map[string]int{}
	for _, facet := range page.Presence {
		presence[facet.Value] = facet.Count
	}
	if presence["online"] != 1 || presence["offline"] != 1 {
		t.Fatalf("presence facets=%v; fresh inventory must remain offline and infrastructure absent",
			presence)
	}
}

// "Connection" is derived from telemetry, and it has to be derivable in SQL.
//
// It is the only rail whose value is not a column: a client is "wireless"
// because recent station telemetry carries its MAC. That had been computed in
// Go over the fetched rows, which cannot survive paging — so it is an
// expression now, and this pins the three things that expression must get
// right: a MAC with recent station telemetry is wireless, a MAC with only
// STALE telemetry is not, and a MAC with none is "unknown" rather than "wired".
func TestClientsPageConnectionComesFromTelemetry(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	dev := &Device{MAC: "60:38:e0:00:00:01", Host: "192.168.1.1", Name: "ap"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:04:01", Scope: ScopeLocal}, // associated, recently
		{MAC: "aa:00:00:00:04:02", Scope: ScopeLocal}, // associated, long ago
		{MAC: "aa:00:00:00:04:03", Scope: ScopeLocal}, // never seen on a radio
	}, 9000); err != nil {
		t.Fatal(err)
	}

	station := func(mac string, ts int64) {
		t.Helper()
		var id int64
		if err := db.SQL().QueryRowContext(ctx,
			`INSERT INTO series (device_id, kind, key) VALUES (?,?,?) RETURNING id`,
			dev.ID, "sta_rssi", mac).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO rollup_5m (series_id, ts, avg, min, max, cnt)
			 VALUES (?,?,?,?,?,?)`, id, ts, -52.0, -55.0, -50.0, 3); err != nil {
			t.Fatal(err)
		}
	}
	station("aa:00:00:00:04:01", 9500)
	// Stale: a station series persists for the whole retention period, so
	// without a recency bound this client reads as associated-at-−52-dBm weeks
	// after it left.
	station("aa:00:00:00:04:02", 100)

	page, err := db.ClientsPage(ctx, ClientFilter{
		ActiveSince:   9000,
		WirelessKinds: []string{"sta_rssi", "sta_retry"},
		Limit:         50,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range page.Connection {
		got[f.Value] = f.Count
	}
	if got["wireless"] != 1 || got["unknown"] != 2 {
		t.Errorf("connection facet = %v, want 1 wireless and 2 unknown", got)
	}

	// And filtering on it must select the right row, not merely count it.
	page, err = db.ClientsPage(ctx, ClientFilter{
		ActiveSince:   9000,
		WirelessKinds: []string{"sta_rssi", "sta_retry"},
		Connection:    "wireless",
		Limit:         50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Clients) != 1 || page.Clients[0].MAC != "aa:00:00:00:04:01" {
		t.Errorf("wireless filter returned %+v, want only the recently "+
			"associated client", page.Clients)
	}
}

// With no telemetry kinds supplied, nothing can be shown to be wireless — and
// the rail still exists, saying so.
//
// The alternative, omitting the dimension, makes the rail vanish, which reads
// as a build that does not know about wireless at all.
func TestClientsPageWithNoWirelessKindsCallsEverythingUnknown(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "aa:00:00:00:05:01", Scope: ScopeLocal},
	}, 9000); err != nil {
		t.Fatal(err)
	}
	page, err := db.ClientsPage(ctx, ClientFilter{ActiveSince: 9000, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Connection) != 1 || page.Connection[0].Value != "unknown" ||
		page.Connection[0].Count != 1 {
		t.Errorf("connection facet = %+v, want a single 'unknown: 1'", page.Connection)
	}
}

// Every scope is present even when nothing is in it.
//
// "0 local, 4 upstream" is an answer and renders as a rail a person can click.
// A missing key renders as no rail at all, which reads as "this build does not
// do scoping" rather than "none of these are yours".
func TestClientCountsAlwaysNamesEveryScope(t *testing.T) {
	db := open(t)
	counts, err := db.ClientCounts(context.Background(), ClientFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{ScopeLocal, ScopeUpstream, ScopeUnknown} {
		if _, ok := counts[scope]; !ok {
			t.Errorf("scope %q missing from an empty count", scope)
		}
	}
}

// A row written before the scope column existed counts as unplaced, not local —
// the same rule Clients() applies when reading one.
func TestClientCountsTreatsPreMigrationRowsAsUnknown(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO clients (mac, name, ip, first_seen, last_seen) VALUES (?,?,?,?,?)`,
		"aa:00:00:00:00:07", "old row", "192.168.1.7", 1000, 1000); err != nil {
		t.Fatal(err)
	}
	counts, err := db.ClientCounts(ctx, ClientFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if counts[ScopeLocal].Total != 0 || counts[ScopeUnknown].Total != 1 {
		t.Errorf("pre-migration row counted as local=%d unknown=%d, want 0 and 1",
			counts[ScopeLocal].Total, counts[ScopeUnknown].Total)
	}
}

func clientByMAC(t *testing.T, db *DB, mac string) Client {
	t.Helper()
	cs, err := db.Clients(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.MAC == mac {
			return c
		}
	}
	t.Fatalf("client %s not found", mac)
	return Client{}
}

// Un-adopting a device must take its ownership claims with it.
//
// ForgetOwned existed for this and was called from nowhere, so every removed
// device left its claims behind. Not merely untidy: sqlite reuses a freed
// INTEGER PRIMARY KEY, so the next device adopted takes the id of one that was
// removed and inherits what it claimed to own — and a later un-adopt would try
// to revert sections that device never had.
func TestOrphanedOwnershipClaimsAreSwept(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	dev := &Device{
		MAC: "aa:bb:cc:dd:ee:01", Host: "192.168.9.1", Name: "doomed", Role: "ap",
	}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordOwned(ctx, []OwnedSection{
		{DeviceID: dev.ID, Config: "wireless", Section: "oowrt_wlan1_radio0",
			RenderedHash: "h1"},
	}); err != nil {
		t.Fatal(err)
	}

	// The device goes away by a path that does not call ForgetOwned.
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM owned_sections WHERE device_id=?`, dev.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d ownership claim(s) outlived their device; the next device "+
			"to reuse this row id would inherit them", n)
	}
}

// Every connection must have foreign keys on, not just the first one.
//
// These settings are per CONNECTION in SQLite, and they used to be applied with
// an Exec after opening — which runs on whichever pooled connection serves it.
// database/sql discards a connection on a driver error and opens a fresh one
// with the SQLite defaults, and from that moment every ON DELETE CASCADE in the
// schema silently stops firing.
func TestForeignKeysAreOnForEveryConnection(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	var fk, busy int
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys=%d; every ON DELETE CASCADE in the schema is "+
			"inert without it", fk)
	}
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy == 0 {
		t.Error("busy_timeout=0 turns a moment's write contention into an " +
			"immediate SQLITE_BUSY instead of a short wait")
	}

	// And the cascade actually fires: the whole point of the setting.
	dev := &Device{MAC: "aa:bb:cc:dd:ee:02", Host: "192.168.9.2",
		Name: "cascade", Role: "ap"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordOwned(ctx, []OwnedSection{
		{DeviceID: dev.ID, Config: "wireless", Section: "oowrt_wlan1_radio0",
			RenderedHash: "h1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, dev.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM owned_sections WHERE device_id=?`, dev.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d ownership claim(s) survived their device: the cascade did "+
			"not fire", n)
	}
}

// A data directory whose name contains "#" or "%" must open the database it
// was asked for.
//
// dsnWithPragmas briefly prefixed the path with "file:", which makes SQLite
// parse it as a URI rather than as a filename. URI parsing truncates at "#"
// and percent-decodes "%HH", so the controller would silently come up on a
// different file — migrating a whole schema into a fresh empty database and
// reporting zero devices while the real one sat untouched beside it. The "%"
// case at least failed loudly; the "#" case did not fail at all.
func TestAwkwardDataDirectoryNamesOpenTheRightFile(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"plain", "has#hash", "pct%20name", "with space"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(dir, "oonfeewrt.db")

			db, err := Open(ctx, "sqlite", want, testProtector(t, want))
			if err != nil {
				t.Fatalf("Open(%q): %v", want, err)
			}
			defer db.Close()

			// It must be usable, and the settings must have survived.
			var fk int
			if err := db.SQL().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
				t.Fatal(err)
			}
			if fk != 1 {
				t.Errorf("foreign_keys=%d in a directory named %q", fk, name)
			}

			// And the file must be the one asked for, not a neighbour.
			if _, err := os.Stat(want); err != nil {
				t.Errorf("no database at the requested path %q: %v", want, err)
			}
		})
	}
}

// A path the DSN cannot express must be refused, not silently redirected.
//
// Without a "file:" prefix the driver splits at the first "?" before decoding
// anything, so a data directory containing one opens a DIFFERENT file — with
// the schema migrated into it and the controller reporting zero devices while
// the real database sits beside it untouched. Escaping cannot help: there is no
// decoding step to undo it. "file:" is not the answer either, since that
// truncates at "#" and percent-decodes "%HH".
func TestAPathTheDSNCannotExpressIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "oonfee?wrt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "oonfeewrt.db")

	db, err := Open(ctx, "sqlite", want, testProtector(t, want))
	if err == nil {
		db.Close()
		t.Fatalf("a path containing %q opened without complaint; it would have "+
			"opened a different file", "?")
	}
	if !strings.Contains(err.Error(), "?") {
		t.Errorf("the error does not say what is wrong with the path: %v", err)
	}
	// And it must not have created anything at the truncated location.
	if _, statErr := os.Stat(filepath.Dir(dir)); statErr == nil {
		entries, _ := os.ReadDir(filepath.Dir(dir))
		for _, e := range entries {
			if e.Name() == "oonfee" {
				t.Error("a database was created at the truncated path")
			}
		}
	}
}
