package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	_ "modernc.org/sqlite"
)

type recoveryFixture struct {
	dbPath, passFile string
	privateValues    []string
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oonfeewrt.db")
	passphrase := "recovery-passphrase-sentinel-7Qv2"
	passFile := filepath.Join(dir, "passphrase")
	if err := os.WriteFile(passFile, []byte(passphrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keeper, err := secrets.Create(filepath.Join(dir, secrets.FileName), []byte(passphrase),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, "sqlite", dbPath, keeper)
	if err != nil {
		keeper.Close()
		t.Fatal(err)
	}
	if _, err := db.Site(ctx); err != nil {
		t.Fatal(err)
	}
	ownerHash, err := secrets.HashPassword([]byte("private-owner-password"),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFirstAdmin(ctx, "private-owner", ownerHash); err != nil {
		t.Fatal(err)
	}

	network := &model.Network{Name: "private-network-name", VLAN: 1,
		CIDR: "192.0.2.1/24", Zone: "private-zone-name", Enabled: true}
	if err := db.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	devices := []*store.Device{
		{MAC: "02:00:00:00:00:01", Host: "private-host-one.invalid", Name: "private-device-one",
			Role: "ap", Functions: []string{"ap", "switch"}, AdoptedAt: &now},
		{MAC: "02:00:00:00:00:02", Host: "private-host-two.invalid", Name: "private-device-two",
			Role: "ap", Functions: []string{"ap", "switch"}, AdoptedAt: &now},
	}
	privateValues := []string{passphrase, "private-owner", "private-owner-password", ownerHash,
		network.Name, network.Zone}
	for i, device := range devices {
		username := "private-user-" + string(rune('a'+i))
		password := "private-password-" + string(rune('a'+i))
		device.CredEnc, err = keeper.SealCredential(device.MAC, username, password)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
		privateValues = append(privateValues, device.MAC, device.Host, device.Name, username, password)
	}
	group := &model.APGroup{Name: "private-group-name", DeviceIDs: []int64{devices[0].ID, devices[1].ID}}
	if err := db.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	wlan := &model.WLAN{SSID: "private-ssid-name", NetworkID: network.ID, GroupID: group.ID,
		Bands: []model.Band{model.Band2G}, Security: model.Security{Mode: model.SecPSK2,
			Key: "private-wlan-key", PMF: model.PMFDisabled}, Enabled: true}
	if err := db.SaveWLAN(ctx, wlan); err != nil {
		t.Fatal(err)
	}
	mesh := &model.Mesh{MeshID: "private-mesh-name", NetworkID: network.ID, GroupID: group.ID,
		Band: model.Band5G, Key: "private-mesh-key", Enabled: true}
	if err := db.SaveMesh(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	owned := []store.OwnedSection{
		{DeviceID: devices[0].ID, Config: "wireless", Section: "private-owned-one", RenderedHash: "private-owned-hash-one"},
		{DeviceID: devices[1].ID, Config: "wireless", Section: "private-owned-two", RenderedHash: "private-owned-hash-two"},
	}
	if err := db.RecordOwned(ctx, owned); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}
	privateValues = append(privateValues, group.Name, wlan.SSID, wlan.Security.Key,
		mesh.MeshID, mesh.Key, owned[0].Section, owned[0].RenderedHash,
		owned[1].Section, owned[1].RenderedHash)
	t.Setenv("OONFEE_PASSPHRASE_FILE", passFile)
	return recoveryFixture{dbPath: dbPath, passFile: passFile, privateValues: privateValues}
}

func TestRecoveryCheckTraversesEverySealedRecordAndPrintsCountsOnly(t *testing.T) {
	fixture := newRecoveryFixture(t)
	before := fileSHA256(t, fixture.dbPath)
	var output bytes.Buffer
	if err := run(context.Background(), fixture.dbPath, &output); err != nil {
		t.Fatal(err)
	}
	const want = "schema=19 devices=2 credentials=2 owned_sections=2 wlans=1 meshes=1\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
	for _, private := range fixture.privateValues {
		if strings.Contains(output.String(), private) {
			t.Fatalf("counts-only output exposed a private fixture value: %q", output.String())
		}
	}
	if after := fileSHA256(t, fixture.dbPath); after != before {
		t.Fatal("read-only recovery check changed the database main file")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if info, err := os.Lstat(fixture.dbPath + suffix); err == nil && info.Size() != 0 {
			t.Fatalf("read-only recovery check left non-empty SQLite sidecar %q", suffix)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := run(context.Background(), fixture.dbPath, io.Discard); err != nil {
		t.Fatalf("repeat recovery check after read-only SQLite sidecars: %v", err)
	}
}

func TestRecoveryCheckPreservesInvalidContexts(t *testing.T) {
	fixture := newRecoveryFixture(t)
	if err := run(nil, fixture.dbPath, io.Discard); err == nil {
		t.Fatal("nil context was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, fixture.dbPath, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}

func TestRecoveryCheckUsesOnlyTheDatabaseSiblingKeyring(t *testing.T) {
	fixture := newRecoveryFixture(t)
	wrongDir := t.TempDir()
	wrongDB := filepath.Join(wrongDir, "oonfeewrt.db")
	contents, err := os.ReadFile(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrongDB, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	wrong, err := secrets.Create(filepath.Join(wrongDir, secrets.FileName),
		[]byte("recovery-passphrase-sentinel-7Qv2"),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = run(context.Background(), wrongDB, &output)
	if err == nil {
		t.Fatal("database opened with an unrelated sibling keyring")
	}
	if output.Len() != 0 {
		t.Fatalf("failed verification printed a success record: %q", output.String())
	}
	for _, private := range fixture.privateValues {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("keyring mismatch exposed a private fixture value: %v", err)
		}
	}
}

func TestRecoveryCheckRejectsSymlinkedPairMembers(t *testing.T) {
	fixture := newRecoveryFixture(t)
	for _, member := range []string{"database", "keyring"} {
		t.Run(member, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "oonfeewrt.db")
			keyPath := filepath.Join(dir, secrets.FileName)
			if member == "database" {
				if err := os.Symlink(fixture.dbPath, dbPath); err != nil {
					t.Fatal(err)
				}
				copyFile(t, filepath.Join(filepath.Dir(fixture.dbPath), secrets.FileName), keyPath)
			} else {
				copyFile(t, fixture.dbPath, dbPath)
				if err := os.Symlink(filepath.Join(filepath.Dir(fixture.dbPath), secrets.FileName), keyPath); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := run(context.Background(), dbPath, &output); err == nil {
				t.Fatalf("symlinked %s was accepted", member)
			}
			if output.Len() != 0 {
				t.Fatalf("failed verification printed a success record: %q", output.String())
			}
		})
	}
}

func TestRecoveryCheckRejectsSQLiteSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			if err := os.WriteFile(fixture.dbPath+suffix, []byte("sidecar"), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := run(context.Background(), fixture.dbPath, &output)
			if err == nil || !strings.Contains(err.Error(), suffix) {
				t.Fatalf("sidecar %q error = %v", suffix, err)
			}
			if output.Len() != 0 {
				t.Fatalf("failed verification printed a success record: %q", output.String())
			}
		})
	}
}

func TestRecoveryCheckRejectsCorruptSealedRecordsWithoutLeakingThem(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
	}{
		{"last credential", `UPDATE devices SET cred_enc=x'01' WHERE id=(SELECT MAX(id) FROM devices)`},
		{"last ownership verifier", `UPDATE owned_sections SET rendered_hash_enc=x'01' WHERE device_id=(SELECT MAX(device_id) FROM owned_sections)`},
		{"missing ownership verifier", `UPDATE owned_sections SET rendered_hash_enc=NULL WHERE device_id=(SELECT MAX(device_id) FROM owned_sections)`},
		{"WLAN key", `UPDATE wlans SET security_key_enc=x'01' WHERE id=(SELECT MAX(id) FROM wlans)`},
		{"mesh key", `UPDATE meshes SET key_enc=x'01' WHERE id=(SELECT MAX(id) FROM meshes)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			mutateFixture(t, fixture.dbPath, test.sql)
			var output bytes.Buffer
			err := run(context.Background(), fixture.dbPath, &output)
			if err == nil {
				t.Fatal("corrupt sealed record was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("failed verification printed a success record: %q", output.String())
			}
			for _, private := range fixture.privateValues {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("error exposed a private fixture value: %v", err)
				}
			}
		})
	}
}

func TestRecoveryCheckRejectsInvalidSiteWithoutPrintingItsValues(t *testing.T) {
	fixture := newRecoveryFixture(t)
	mutateFixture(t, fixture.dbPath, `UPDATE site SET uuid=''`)
	var output bytes.Buffer
	err := run(context.Background(), fixture.dbPath, &output)
	if err == nil || !strings.Contains(err.Error(), "site validation") {
		t.Fatalf("invalid site error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("failed verification printed a success record: %q", output.String())
	}
	for _, private := range fixture.privateValues {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error exposed a private fixture value: %v", err)
		}
	}
}

func TestRecoveryDatabasePathRequiresOneExistingRegularFile(t *testing.T) {
	var output bytes.Buffer
	if _, err := recoveryDatabasePath(nil, &output); err == nil {
		t.Fatal("missing path was accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := recoveryDatabasePath([]string{missing}, &output); err == nil {
		t.Fatal("missing database was accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argument validation created the database: %v", err)
	}
	if _, err := recoveryDatabasePath([]string{t.TempDir()}, &output); err == nil {
		t.Fatal("directory was accepted as a database")
	}
	path := filepath.Join(t.TempDir(), "oonfeewrt.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := recoveryDatabasePath([]string{path}, &output); err != nil || got != path {
		t.Fatalf("path = %q, %v; want %q", got, err, path)
	}
	if _, err := recoveryDatabasePath([]string{"-h"}, &output); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v, want flag.ErrHelp", err)
	}
}

func mutateFixture(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(statement); err != nil {
		db.Close()
		t.Fatal(err)
	}
	var busy, logFrames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if busy != 0 {
		db.Close()
		t.Fatalf("fixture checkpoint remained busy: log=%d checkpointed=%d", logFrames, checkpointed)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(contents)
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
