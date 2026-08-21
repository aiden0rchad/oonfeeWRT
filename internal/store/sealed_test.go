package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

type v13Values struct {
	wlan, dormant, mesh, hash string
}

type barrierProtector struct {
	SecretProtector
	arrived chan<- struct{}
	release <-chan struct{}
}

type rejectLockedVerifyProtector struct {
	SecretProtector
	calls int
}

func (p *rejectLockedVerifyProtector) VerifyCredential(mac string, blob []byte) error {
	p.calls++
	if p.calls == 2 {
		return errors.New("fixture rejects locked verification")
	}
	return p.SecretProtector.VerifyCredential(mac, blob)
}

func (p barrierProtector) VerifyCredential(mac string, blob []byte) error {
	err := p.SecretProtector.VerifyCredential(mac, blob)
	p.arrived <- struct{}{}
	<-p.release
	return err
}

func generatedFixtureValues() v13Values {
	return v13Values{
		wlan: strings.Repeat("W", 27), dormant: strings.Repeat("D", 25),
		mesh: strings.Repeat("M", 29), hash: strings.Repeat("H", 64),
	}
}

func createKeeper(t *testing.T, path, label string) *secrets.Keeper {
	t.Helper()
	passphrase := []byte(strings.Repeat(label, 16))
	keeper, err := secrets.Create(path, passphrase,
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { keeper.Close() })
	return keeper
}

func buildV13Fixture(t *testing.T, path string, keeper *secrets.Keeper, values v13Values) {
	t.Helper()
	ctx := context.Background()
	raw, err := sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE secret_state`,
		`ALTER TABLE wlans DROP COLUMN security_key_enc`,
		`ALTER TABLE meshes DROP COLUMN key_enc`,
		`ALTER TABLE owned_sections DROP COLUMN rendered_hash_enc`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("construct v13 schema: %v", err)
		}
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO schema_version(version,applied_at) VALUES(13,1)`); err != nil {
		t.Fatal(err)
	}
	mac := "02:00:00:00:00:13"
	credential, err := keeper.SealCredential(mac, "fixture-user", strings.Repeat("C", 20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO devices(id,mac,host,name,role,adopted_at,cred_enc)
		 VALUES(1,?,'192.0.2.13','fixture','ap',1,?)`, mac, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO networks(id,name,vlan,cidr,zone) VALUES(1,'fixture-net',13,'192.0.2.1/24','lan')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO ap_groups(id,name) VALUES(1,'fixture-group')`); err != nil {
		t.Fatal(err)
	}
	keyed, _ := json.Marshal(model.Security{Mode: model.SecSAEMixed, Key: values.wlan, PMF: model.PMFOptional})
	open, _ := json.Marshal(model.Security{Mode: model.SecNone, Key: values.dormant, PMF: model.PMFDisabled})
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO wlans(id,ssid,network_id,group_id,security_json) VALUES
		 (1,'fixture-keyed',1,1,?),(2,'fixture-open',1,1,?)`, string(keyed), string(open)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO meshes(id,mesh_id,network_id,group_id,band,key)
		 VALUES(1,'fixture-mesh',1,1,'5g',?)`, values.mesh); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO owned_sections(device_id,config,section,rendered_hash,applied_at)
		 VALUES(1,'wireless','fixture-section',?,1)`, values.hash); err != nil {
		t.Fatal(err)
	}
}

func TestSchema14MigrationSealsAndPhysicallyScrubsLegacySecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "fixture-keyring.json"), "A")
	values := generatedFixtureValues()
	buildV13Fixture(t, path, keeper, values)

	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	var version, scrub int
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&scrub); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || scrub != 1 {
		t.Fatalf("migration state version=%d scrub=%d", version, scrub)
	}

	var wlanPlain, meshPlain, hashPlain string
	var wlanEnc, meshEnc, hashEnc []byte
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT security_json,security_key_enc FROM wlans WHERE id=1`).Scan(&wlanPlain, &wlanEnc); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT key,key_enc FROM meshes WHERE id=1`).Scan(&meshPlain, &meshEnc); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT rendered_hash,rendered_hash_enc FROM owned_sections WHERE device_id=1`).Scan(&hashPlain, &hashEnc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wlanPlain, values.wlan) || meshPlain != "" || hashPlain != "" ||
		len(wlanEnc) == 0 || len(meshEnc) == 0 || len(hashEnc) == 0 {
		t.Fatal("schema14 left a clear value or omitted a ciphertext")
	}
	var dormant []byte
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT security_key_enc FROM wlans WHERE id=2`).Scan(&dormant); err != nil {
		t.Fatal(err)
	}
	if len(dormant) != 0 {
		t.Fatal("open WLAN retained a dormant key")
	}

	wlans, err := db.wlans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	meshes, err := db.meshes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := db.OwnedSections(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(wlans) != 2 || wlans[0].Security.Key != values.wlan || wlans[1].Security.Key != "" ||
		len(meshes) != 1 || meshes[0].Key != values.mesh ||
		len(owned) != 1 || owned[0].RenderedHash != values.hash {
		t.Fatal("sealed values did not round-trip")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		blob, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, sentinel := range []string{values.wlan, values.dormant, values.mesh, values.hash} {
			if bytes.Contains(blob, []byte(sentinel)) {
				t.Fatalf("legacy clear value survived physical scrub in %s", filepath.Base(candidate))
			}
		}
	}
}

func TestSchema14RefusesWrongKeyBeforeV13Mutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	correct := createKeeper(t, filepath.Join(dir, "correct.json"), "A")
	wrong := createKeeper(t, filepath.Join(dir, "wrong.json"), "B")
	buildV13Fixture(t, path, correct, generatedFixtureValues())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if db, err := Open(ctx, driver, path, wrong); err == nil {
		db.Close()
		t.Fatal("wrong keyring migrated a v13 database")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("wrong-key preflight changed the database file")
	}
	raw, err := sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if hasColumn(t, raw, "wlans", "security_key_enc") {
		t.Fatal("wrong-key preflight reached schema14 DDL")
	}
}

func TestSchema14WrongKeyDoesNotChangeActiveDatabaseOrWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	correct := createKeeper(t, filepath.Join(dir, "correct.json"), "A")
	wrong := createKeeper(t, filepath.Join(dir, "wrong.json"), "B")
	db, err := Open(ctx, driver, path, correct)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO events(ts,category,severity,event) VALUES(1,'system','info','wal-fixture')`); err != nil {
		t.Fatal(err)
	}
	files := []string{path, path + "-wal", path + "-shm"}
	before := make(map[string][]byte, len(files))
	for _, file := range files {
		before[file], err = os.ReadFile(file)
		if err != nil {
			t.Fatalf("read active %s: %v", filepath.Base(file), err)
		}
	}
	if wrongDB, err := Open(ctx, driver, path, wrong); err == nil {
		wrongDB.Close()
		t.Fatal("wrong keyring opened active WAL database")
	}
	for _, file := range files[:2] {
		after, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read active %s after refusal: %v", filepath.Base(file), err)
		}
		if !bytes.Equal(before[file], after) {
			t.Fatalf("wrong-key preflight changed active %s", filepath.Base(file))
		}
	}
	// modernc updates SQLite's shared-memory WAL-index reader marks even for a
	// mode=ro connection. That file is ephemeral coordination state, not a
	// backup component; ensure the refusal neither replaces it nor writes DB
	// payload into it. The durable main DB and WAL above must remain exact.
	afterSHM, err := os.ReadFile(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSHM) != len(before[path+"-shm"]) || bytes.Contains(afterSHM, []byte("wal-fixture")) {
		t.Fatal("wrong-key preflight replaced SHM or copied database payload into it")
	}
}

func TestSchema14RevalidatesLegacyKeyringUnderMigrationLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	buildV13Fixture(t, path, keeper, generatedFixtureValues())
	raw, err := sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := raw.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("fixture journal_mode=%q, want delete", mode)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected preflight sidecar %s: %v", suffix, err)
		}
	}
	protector := &rejectLockedVerifyProtector{SecretProtector: keeper}
	if db, err := Open(ctx, driver, path, protector); err == nil {
		db.Close()
		t.Fatal("migration did not repeat keyring validation under its write lock")
	}
	if protector.calls != 2 {
		t.Fatalf("credential verification calls=%d, want preflight plus locked recheck", protector.calls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed locked keyring verification changed DELETE-mode database")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed locked verification left sidecar %s: %v", suffix, err)
		}
	}
	raw, err = sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if hasColumn(t, raw, "wlans", "security_key_enc") {
		t.Fatal("failed locked keyring verification persisted schema14 DDL")
	}
}

func TestSchema14ConcurrentMigrationRechecksVersionUnderWriteLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	values := generatedFixtureValues()
	buildV13Fixture(t, path, keeper, values)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	type result struct {
		db  *DB
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		protector := barrierProtector{SecretProtector: keeper, arrived: arrived, release: release}
		go func() {
			db, err := Open(ctx, driver, path, protector)
			results <- result{db: db, err: err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent opens did not both finish v13 keyring preflight")
		}
	}
	close(release)
	var opened []*DB
	defer func() {
		for _, db := range opened {
			_ = db.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent Open: %v", result.err)
			}
			// Keep both handles open until both Open calls have returned. Closing
			// the first result here can hide a checkpoint race by releasing the
			// exact connection which the second opener is contending with.
			opened = append(opened, result.db)
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent migration did not complete")
		}
	}
	for _, db := range opened {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	opened = nil

	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wlans, err := db.wlans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	meshes, err := db.meshes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := db.OwnedSections(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(wlans) != 2 || wlans[0].Security.Key != values.wlan ||
		len(meshes) != 1 || meshes[0].Key != values.mesh ||
		len(owned) != 1 || owned[0].RenderedHash != values.hash {
		t.Fatal("concurrent migration lost sealed site state")
	}
	var versions int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version=14`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("schema14 was recorded %d times", versions)
	}
}

func TestSchema14ConcurrentFreshDatabaseCreation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	start := make(chan struct{})
	type result struct {
		db  *DB
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			db, err := Open(ctx, driver, path, keeper)
			results <- result{db: db, err: err}
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent fresh Open: %v", result.err)
			}
			if err := result.db.Close(); err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent fresh Open did not complete")
		}
	}
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var versions, complete int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version=14`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&complete); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || complete != 1 || integrity != "ok" {
		t.Fatalf("fresh race state: versions=%d scrub=%d integrity=%q", versions, complete, integrity)
	}
}

func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func TestSchema14AADRejectsCiphertextSwapAndLegacyFallback(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	first := &model.WLAN{SSID: "first", NetworkID: netID, GroupID: groupID,
		Security: model.Security{Mode: model.SecSAE, Key: strings.Repeat("A", 18), PMF: model.PMFRequired}, Enabled: true}
	second := &model.WLAN{SSID: "second", NetworkID: netID, GroupID: groupID,
		Security: model.Security{Mode: model.SecSAE, Key: strings.Repeat("B", 18), PMF: model.PMFRequired}, Enabled: true}
	if err := db.SaveWLAN(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWLAN(ctx, second); err != nil {
		t.Fatal(err)
	}
	var a, b []byte
	if err := db.SQL().QueryRowContext(ctx, `SELECT security_key_enc FROM wlans WHERE id=?`, first.ID).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT security_key_enc FROM wlans WHERE id=?`, second.ID).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE wlans SET security_key_enc=CASE id WHEN ? THEN ? WHEN ? THEN ? END WHERE id IN (?,?)`,
		first.ID, b, second.ID, a, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.wlans(ctx); err == nil {
		t.Fatal("ciphertexts swapped between WLAN rows still opened")
	}

	legacy, _ := json.Marshal(model.Security{Mode: model.SecSAE, Key: strings.Repeat("L", 22), PMF: model.PMFRequired})
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE wlans SET security_json=?,security_key_enc=x'01' WHERE id=?`, string(legacy), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.wlans(ctx); err == nil {
		t.Fatal("corrupt ciphertext fell back to legacy clear JSON")
	}

	if bytes.Equal(ownedHashAAD(1, "ab", "c"), ownedHashAAD(1, "a", "bc")) {
		t.Fatal("length-framed AAD collided")
	}
}

func TestSchema14BlankUpdatePreservesCiphertextAndKeylessModeClearsIt(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "fixture", NetworkID: netID, GroupID: groupID,
		Security: model.Security{Mode: model.SecSAEMixed, Key: strings.Repeat("W", 19), PMF: model.PMFOptional}, Enabled: true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := db.SQL().QueryRowContext(ctx, `SELECT security_key_enc FROM wlans WHERE id=?`, w.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w.Security.Key = ""
	w.SSID = "fixture-renamed"
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := db.SQL().QueryRowContext(ctx, `SELECT security_key_enc FROM wlans WHERE id=?`, w.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || w.Security.Key == "" {
		t.Fatal("blank WLAN update did not preserve the existing sealed key")
	}
	w.Security.Mode = model.SecNone
	w.Security.Key = strings.Repeat("D", 23)
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	var cleared []byte
	var public string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT security_key_enc,security_json FROM wlans WHERE id=?`, w.ID).Scan(&cleared, &public); err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 || w.Security.Key != "" || strings.Contains(public, strings.Repeat("D", 23)) {
		t.Fatal("keyless WLAN mode retained a dormant key")
	}

	m := &model.Mesh{MeshID: "fixture-mesh", NetworkID: netID, GroupID: groupID,
		Band: model.Band5G, Key: strings.Repeat("M", 17), Enabled: true}
	if err := db.SaveMesh(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT key_enc FROM meshes WHERE id=?`, m.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	m.Key = ""
	if err := db.SaveMesh(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT key_enc FROM meshes WHERE id=?`, m.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || m.Key == "" {
		t.Fatal("blank mesh update did not preserve the existing sealed key")
	}
	if err := db.SaveMeshWithOptions(ctx, m, SaveMeshOptions{ClearKey: true}); err == nil {
		t.Fatal("store accepted a mesh key together with ClearKey")
	}
	m.Key = ""
	if err := db.SaveMeshWithOptions(ctx, m, SaveMeshOptions{ClearKey: true}); err != nil {
		t.Fatal(err)
	}
	var meshPlain string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT key,key_enc FROM meshes WHERE id=?`, m.ID).Scan(&meshPlain, &cleared); err != nil {
		t.Fatal(err)
	}
	if meshPlain != "" || len(cleared) != 0 || m.Key != "" {
		t.Fatal("explicit mesh clear retained key material")
	}
}

func TestSchema14KeyringRotationWrongKeyAndScrubResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keyringPath := filepath.Join(dir, "keyring.json")
	oldPass := []byte(strings.Repeat("O", 18))
	newPass := []byte(strings.Repeat("N", 18))
	keeper, err := secrets.Create(keyringPath, oldPass,
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "rotation", NetworkID: netID, GroupID: groupID,
		Security: model.Security{Mode: model.SecSAE, Key: strings.Repeat("R", 21), PMF: model.PMFRequired}, Enabled: true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	beforeRotation, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := keeper.ChangePassphrase(newPass,
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1}); err != nil {
		t.Fatal(err)
	}
	afterRotation, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeRotation, afterRotation) {
		t.Fatal("keyring passphrase rotation rewrote the database")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}
	keeper, err = secrets.Open(keyringPath, newPass)
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	wlans, err := db.wlans(ctx)
	if err != nil || len(wlans) != 1 || wlans[0].Security.Key != strings.Repeat("R", 21) {
		t.Fatal("rotated keyring did not reopen sealed site data")
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE secret_state SET scrub_complete=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if ro, err := OpenReadOnly(ctx, driver, path, keeper); err == nil {
		ro.Close()
		t.Fatal("read-only open accepted an incomplete physical scrub")
	}
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var complete int
	if err := db.SQL().QueryRowContext(ctx, `SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&complete); err != nil {
		t.Fatal(err)
	}
	if complete != 1 {
		t.Fatal("writable startup did not resume the physical scrub")
	}

	wrong := createKeeper(t, filepath.Join(dir, "unrelated.json"), "Z")
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	beforeWrong, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if wrongDB, err := Open(ctx, driver, path, wrong); err == nil {
		wrongDB.Close()
		t.Fatal("unrelated keyring opened a schema14 database")
	}
	afterWrong, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeWrong, afterWrong) {
		t.Fatal("wrong-key schema14 preflight changed the database")
	}
}

func TestSchema14RefusesDowngradeBeforeMutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE schema_version SET version=?`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if newer, err := Open(ctx, driver, path, keeper); err == nil {
		newer.Close()
		t.Fatalf("schema v%d database was accepted", schemaVersion+1)
	} else if !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("unexpected downgrade error: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("downgrade refusal changed the database")
	}
}

func TestSchema14RefusesToRerunOneTimeSecretMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "one-time", NetworkID: netID, GroupID: groupID,
		Security: model.Security{Mode: model.SecSAE, Key: strings.Repeat("T", 20), PMF: model.PMFRequired}, Enabled: true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT security_key_enc FROM wlans WHERE id=?`, w.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE schema_version SET version=13`); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(ctx, driver, path, keeper); err == nil {
		reopened.Close()
		t.Fatal("lowered schema_version re-ran the one-time secret migration")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("one-time migration refusal changed the database")
	}
	raw, err := sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var afterCiphertext []byte
	if err := raw.QueryRowContext(ctx,
		`SELECT security_key_enc FROM wlans WHERE id=?`, w.ID).Scan(&afterCiphertext); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ciphertext, afterCiphertext) {
		t.Fatal("one-time migration refusal replaced sealed WLAN data")
	}
}

func copyFixtureFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSchema14MainOnlyBackupRestoresWithoutWALSidecars(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	sourceDB := filepath.Join(source, "controller.db")
	sourceKeyring := filepath.Join(source, secrets.FileName)
	keeper := createKeeper(t, sourceKeyring, "A")
	db, err := Open(ctx, driver, sourceDB, keeper)
	if err != nil {
		t.Fatal(err)
	}
	netID, groupID := seedSite(t, db)
	w := &model.WLAN{SSID: "backup", NetworkID: netID, GroupID: groupID,
		Security: model.Security{Mode: model.SecSAE, Key: strings.Repeat("B", 20), PMF: model.PMFRequired}, Enabled: true}
	if err := db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	restore := t.TempDir()
	restoredDB := filepath.Join(restore, "controller.db")
	restoredKeyring := filepath.Join(restore, secrets.FileName)
	copyFixtureFile(t, sourceDB, restoredDB)
	copyFixtureFile(t, sourceKeyring, restoredKeyring)
	restoredKeeper, err := secrets.Open(restoredKeyring, []byte(strings.Repeat("A", 16)))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredKeeper.Close()
	if ro, err := OpenReadOnly(ctx, driver, restoredDB, restoredKeeper); err != nil {
		t.Fatalf("read-only main-file restore: %v", err)
	} else if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, driver, restoredDB, restoredKeeper)
	if err != nil {
		t.Fatalf("writable main-file restore: %v", err)
	}
	wlans, err := restored.wlans(ctx)
	if err != nil || len(wlans) != 1 || wlans[0].Security.Key != strings.Repeat("B", 20) {
		t.Fatal("main-file restore lost sealed WLAN data")
	}
	restored.Close()
}

func TestSchema14MigratesMainOnlyV13Backup(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	sourceDB := filepath.Join(source, "controller.db")
	sourceKeyring := filepath.Join(source, secrets.FileName)
	keeper := createKeeper(t, sourceKeyring, "A")
	values := generatedFixtureValues()
	buildV13Fixture(t, sourceDB, keeper, values)

	restore := t.TempDir()
	restoredDB := filepath.Join(restore, "controller.db")
	restoredKeyring := filepath.Join(restore, secrets.FileName)
	copyFixtureFile(t, sourceDB, restoredDB)
	copyFixtureFile(t, sourceKeyring, restoredKeyring)
	restoredKeeper, err := secrets.Open(restoredKeyring, []byte(strings.Repeat("A", 16)))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredKeeper.Close()
	before, err := os.ReadFile(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	wrong := createKeeper(t, filepath.Join(restore, "wrong.json"), "Z")
	if wrongDB, err := Open(ctx, driver, restoredDB, wrong); err == nil {
		wrongDB.Close()
		t.Fatal("wrong keyring opened restored v13 main file")
	}
	after, err := os.ReadFile(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("wrong-key restore preflight changed the main database")
	}
	db, err := Open(ctx, driver, restoredDB, restoredKeeper)
	if err != nil {
		t.Fatalf("migrate restored v13 main file: %v", err)
	}
	defer db.Close()
	wlans, err := db.wlans(ctx)
	if err != nil || len(wlans) != 2 || wlans[0].Security.Key != values.wlan {
		t.Fatal("restored v13 main file did not preserve WLAN state")
	}
}

func TestSchema14BusyCheckpointCannotCompleteScrub(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO events(ts,category,severity,event) VALUES(1,'system','info','fixture-a')`); err != nil {
		t.Fatal(err)
	}

	reader, err := openReadOnlySQL(ctx, driver, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO events(ts,category,severity,event) VALUES(2,'system','info','fixture-b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE secret_state SET scrub_complete=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	err = db.finishSecretScrub(ctx)
	if err == nil {
		t.Fatal("scrub completed while WAL truncation was blocked by a reader")
	}
	if !strings.Contains(err.Error(), "remained busy after the retry window") {
		t.Fatalf("long reader returned the wrong checkpoint failure: %v", err)
	}
	var complete int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&complete); err != nil {
		t.Fatal(err)
	}
	if complete != 0 {
		t.Fatal("blocked checkpoint advanced scrub_complete")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.finishSecretScrub(ctx); err != nil {
		t.Fatalf("scrub did not resume after the external reader released: %v", err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&complete); err != nil {
		t.Fatal(err)
	}
	if complete != 1 {
		t.Fatal("successful scrub did not advance scrub_complete")
	}
}

func TestSchema14TransientBusyCheckpointRetries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.db")
	keeper := createKeeper(t, filepath.Join(dir, "keyring.json"), "A")
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO events(ts,category,severity,event) VALUES(1,'system','info','fixture-a')`); err != nil {
		t.Fatal(err)
	}

	reader, err := openReadOnlySQL(ctx, driver, path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		reader.Close()
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		tx.Rollback()
		reader.Close()
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO events(ts,category,severity,event) VALUES(2,'system','info','fixture-b')`); err != nil {
		tx.Rollback()
		reader.Close()
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE secret_state SET scrub_complete=0 WHERE id=1`); err != nil {
		tx.Rollback()
		reader.Close()
		t.Fatal(err)
	}

	finished := make(chan error, 1)
	go func() { finished <- db.finishSecretScrub(ctx) }()
	select {
	case err := <-finished:
		tx.Rollback()
		reader.Close()
		t.Fatalf("transient reader was not retried: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		reader.Close()
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("checkpoint did not resume after transient reader released: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint retry did not finish after transient reader released")
	}
	var complete int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT scrub_complete FROM secret_state WHERE id=1`).Scan(&complete); err != nil {
		t.Fatal(err)
	}
	if complete != 1 {
		t.Fatal("successful retry did not advance scrub_complete")
	}
}
