package portablebackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

const (
	runtimePassphrase = "placeholder-runtime-passphrase"
	exportPassphrase  = "placeholder-export-passphrase"
)

func testKeeper(t *testing.T) *secrets.Keeper {
	t.Helper()
	keeper, err := secrets.Create(filepath.Join(t.TempDir(), secrets.FileName),
		[]byte(runtimePassphrase), secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { keeper.Close() })
	return keeper
}

func testMetadata() Metadata {
	return Metadata{
		ControllerVersion: "v0.1.0-test",
		SchemaVersion:     19,
		CreatedAt:         time.Date(2026, 8, 22, 12, 34, 56, 123, time.FixedZone("test", -7*3600)),
	}
}

func testDatabase(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("database-plaintext-sentinel|"), size/28+1)
	data = data[:size]
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func createArtifact(t *testing.T, databaseSize int) (string, []byte, Result) {
	t.Helper()
	databasePath, database := testDatabase(t, databaseSize)
	path := filepath.Join(t.TempDir(), "controller.oowrt-backup")
	result, err := Create(context.Background(), path, databasePath, testKeeper(t),
		[]byte(exportPassphrase), testMetadata())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return path, database, result
}

func TestCreateExtractRoundTripAndCleanup(t *testing.T) {
	artifactPath, database, result := createArtifact(t, chunkSize+137)
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(artifact, []byte("database-plaintext-sentinel")) ||
		bytes.Contains(artifact, []byte(runtimePassphrase)) ||
		bytes.Contains(artifact, []byte(exportPassphrase)) {
		t.Fatal("artifact exposes database or passphrase plaintext")
	}
	info, err := os.Lstat(artifactPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode=%v err=%v, want regular 0600", info.Mode(), err)
	}
	digest := sha256.Sum256(artifact)
	if result.Path != artifactPath || result.Size != int64(len(artifact)) ||
		result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("result=%+v does not describe artifact", result)
	}
	if !result.Manifest.CreatedAt.Equal(testMetadata().CreatedAt) ||
		result.Manifest.CreatedAt.Location() != time.UTC {
		t.Fatalf("manifest time=%v, want normalized UTC", result.Manifest.CreatedAt)
	}

	stage, err := Extract(context.Background(), artifactPath, t.TempDir(), []byte(exportPassphrase))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stagedDatabase, err := os.ReadFile(stage.DatabasePath)
	if err != nil || !bytes.Equal(stagedDatabase, database) {
		t.Fatalf("staged database differs: size=%d err=%v", len(stagedDatabase), err)
	}
	stageInfo, err := os.Lstat(stage.Directory)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode().Perm() != 0o700 {
		t.Fatalf("stage mode=%v err=%v, want directory 0700", stageInfo.Mode(), err)
	}
	for _, path := range []string{stage.DatabasePath, stage.PortableKeyPath, stage.ManifestPath} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("stage member %s mode=%v err=%v, want regular 0600", path, info.Mode(), err)
		}
	}
	portable, err := os.ReadFile(stage.PortableKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := secrets.OpenPortableKey(portable, []byte(exportPassphrase))
	if err != nil {
		t.Fatalf("staged portable key: %v", err)
	}
	temporary.Close()
	if err := stage.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if err := stage.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if err := stage.Release(); !errors.Is(err, ErrStageCleaned) {
		t.Fatalf("Release after Cleanup=%v, want ErrStageCleaned", err)
	}
	if _, err := os.Lstat(stage.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage remains after cleanup: %v", err)
	}
}

func TestExtractRejectsTamperingTruncationAndTrailingData(t *testing.T) {
	artifactPath, _, _ := createArtifact(t, chunkSize+17)
	original, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	header, err := decodeHeader(original[:headerSize])
	if err != nil {
		t.Fatal(err)
	}
	headerTagOffset := headerSize + int(header.PortableLen)
	manifestOffset := headerTagOffset + headerTagSize
	databaseOffset := manifestOffset + int(header.ManifestLen) + headerTagSize
	cases := map[string]func([]byte) []byte{
		"reserved header": func(data []byte) []byte { data[96] ^= 1; return data },
		"portable key":    func(data []byte) []byte { data[headerSize] ^= 1; return data },
		"header tag":      func(data []byte) []byte { data[headerTagOffset] ^= 1; return data },
		"manifest":        func(data []byte) []byte { data[manifestOffset] ^= 1; return data },
		"database":        func(data []byte) []byte { data[databaseOffset] ^= 1; return data },
		"truncated":       func(data []byte) []byte { return data[:len(data)-1] },
		"trailing":        func(data []byte) []byte { return append(data, 0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			data := mutate(bytes.Clone(original))
			path := filepath.Join(t.TempDir(), "tampered.backup")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			staging := t.TempDir()
			if stage, err := Extract(context.Background(), path, staging,
				[]byte(exportPassphrase)); err == nil {
				stage.Cleanup()
				t.Fatal("Extract accepted a tampered artifact")
			}
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed extraction left staging entries %v, err=%v", entries, err)
			}
		})
	}
	staging := t.TempDir()
	if _, err := Extract(context.Background(), artifactPath, staging, []byte("wrong")); !errors.Is(err, secrets.ErrBadPassphrase) {
		t.Fatalf("wrong passphrase error=%v, want ErrBadPassphrase", err)
	}
	if entries, _ := os.ReadDir(staging); len(entries) != 0 {
		t.Fatalf("wrong passphrase left staging entries %v", entries)
	}
}

func TestCreateAndExtractCancellationRemovePartialOutput(t *testing.T) {
	databasePath, _ := testDatabase(t, 3*chunkSize+1)
	outputDir := t.TempDir()
	output := filepath.Join(outputDir, "cancelled.backup")
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	_, err := create(ctx, output, databasePath, testKeeper(t), []byte(exportPassphrase),
		testMetadata(), operations{afterCreateChunk: func() { once.Do(cancel) }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create cancellation=%v, want context.Canceled", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled create published output: %v", err)
	}
	if entries, _ := os.ReadDir(outputDir); len(entries) != 0 {
		t.Fatalf("cancelled create left files %v", entries)
	}

	artifactPath, _, _ := createArtifact(t, 3*chunkSize+1)
	staging := t.TempDir()
	extractCtx, extractCancel := context.WithCancel(context.Background())
	once = sync.Once{}
	_, err = extract(extractCtx, artifactPath, staging, []byte(exportPassphrase),
		operations{afterExtractChunk: func() { once.Do(extractCancel) }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("extract cancellation=%v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(staging); len(entries) != 0 {
		t.Fatalf("cancelled extract left entries %v", entries)
	}

	lateStaging := t.TempDir()
	lateCtx, lateCancel := context.WithCancel(context.Background())
	_, err = extract(lateCtx, artifactPath, lateStaging, []byte(exportPassphrase),
		operations{afterExtractFinish: lateCancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late extract cancellation=%v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(lateStaging); len(entries) != 0 {
		t.Fatalf("late-cancelled extract left entries %v", entries)
	}
}

func TestCreateNoClobberAndPublicationRollback(t *testing.T) {
	databasePath, _ := testDatabase(t, 128)
	keeper := testKeeper(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "controller.backup")
	sentinel := []byte("do-not-replace")
	if err := os.WriteFile(output, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), output, databasePath, keeper,
		[]byte(exportPassphrase), testMetadata()); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output error=%v, want os.ErrExist", err)
	}
	if after, _ := os.ReadFile(output); !bytes.Equal(after, sentinel) {
		t.Fatalf("existing output changed to %q", after)
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected post-link failure")
	if _, err := create(context.Background(), output, databasePath, keeper,
		[]byte(exportPassphrase), testMetadata(), operations{afterLink: func() error { return injected }}); !errors.Is(err, injected) {
		t.Fatalf("publication failure=%v, want injected error", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publication left output: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("failed publication left files %v", entries)
	}
}

func TestPathsRejectSymlinksAndUnsafeParents(t *testing.T) {
	databasePath, _ := testDatabase(t, 128)
	databaseLink := filepath.Join(t.TempDir(), "database-link")
	if err := os.Symlink(databasePath, databaseLink); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "output.backup")
	if _, err := Create(context.Background(), output, databaseLink, testKeeper(t),
		[]byte(exportPassphrase), testMetadata()); err == nil {
		t.Fatal("Create accepted a symlink database")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink database created output: %v", err)
	}

	artifactPath, _, _ := createArtifact(t, 128)
	artifactLink := filepath.Join(t.TempDir(), "artifact-link")
	if err := os.Symlink(artifactPath, artifactLink); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(context.Background(), artifactLink, t.TempDir(),
		[]byte(exportPassphrase)); err == nil {
		t.Fatal("Extract accepted a symlink artifact")
	}

	realParent := t.TempDir()
	linkedParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), filepath.Join(linkedParent, "output.backup"),
		databasePath, testKeeper(t), []byte(exportPassphrase), testMetadata()); err == nil {
		t.Fatal("Create accepted a symlink output parent")
	}
}

func TestHeaderAndManifestBoundsAreStrict(t *testing.T) {
	base := artifactHeader{
		PortableLen: 1, ManifestLen: 1, DatabaseLen: 1, Chunks: 1,
		Salt: [32]byte{1}, NonceSeed: [16]byte{1},
	}
	for name, mutate := range map[string]func(*artifactHeader){
		"portable": func(h *artifactHeader) { h.PortableLen = maxPortableKeyBytes + 1 },
		"manifest": func(h *artifactHeader) { h.ManifestLen = maxManifestBytes + 1 },
		"database": func(h *artifactHeader) { h.DatabaseLen = MaxDatabaseBytes + 1 },
		"chunks":   func(h *artifactHeader) { h.Chunks = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			h := base
			mutate(&h)
			if _, err := decodeHeader(encodeHeader(h)); err == nil {
				t.Fatal("decodeHeader accepted an out-of-bounds header")
			}
		})
	}

	disk := diskManifest{
		Format: formatName, Version: formatVersion,
		CreatedAt:         "2026-08-22T12:00:00Z",
		ControllerVersion: "v0.1.0", SchemaVersion: 19,
		Database:    Member{Name: "../../escape", Size: 1, SHA256: strings.Repeat("0", 64)},
		PortableKey: Member{Name: portableKeyMemberName, Size: 1, SHA256: sha256Hex([]byte("x"))},
	}
	data, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	h := base
	h.ManifestLen = uint32(len(data))
	if _, err := parseManifest(data, h, []byte("x")); err == nil {
		t.Fatal("parseManifest accepted a path-traversing member name")
	}
}

func TestStageCleanupRefusesReplacement(t *testing.T) {
	artifactPath, _, _ := createArtifact(t, 128)
	stage, err := Extract(context.Background(), artifactPath, t.TempDir(), []byte(exportPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	original := stage.Directory + ".original"
	if err := os.Rename(stage.Directory, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stage.Directory, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.Release(); err == nil {
		t.Fatal("Release accepted a replaced staging directory")
	}
	if err := stage.Cleanup(); err == nil {
		t.Fatal("Cleanup accepted a replaced staging directory")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("Cleanup damaged replacement: %q err=%v", got, err)
	}
	for _, name := range []string{databaseMemberName, portableKeyMemberName, manifestMemberName} {
		if _, err := os.Lstat(filepath.Join(original, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Cleanup orphaned decrypted member %s after stage rename: %v", name, err)
		}
	}
}

func TestStageReleaseIsDurableAndIdempotent(t *testing.T) {
	artifactPath, _, _ := createArtifact(t, chunkSize+31)
	stage, err := Extract(context.Background(), artifactPath, t.TempDir(), []byte(exportPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- stage.Release() }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Release: %v", err)
		}
	}
	if err := stage.Release(); err != nil {
		t.Fatalf("idempotent Release: %v", err)
	}
	if err := stage.Cleanup(); !errors.Is(err, ErrStageReleased) {
		t.Fatalf("Cleanup after Release=%v, want ErrStageReleased", err)
	}
	info, err := os.Lstat(stage.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("released stage mode=%v err=%v", info.Mode(), err)
	}
	for _, path := range []string{stage.DatabasePath, stage.PortableKeyPath, stage.ManifestPath} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("released member %s mode=%v err=%v", path, info.Mode(), err)
		}
	}
}

func TestStageReleaseRejectsChangedMember(t *testing.T) {
	artifactPath, _, _ := createArtifact(t, 128)
	stage, err := Extract(context.Background(), artifactPath, t.TempDir(), []byte(exportPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(stage.DatabasePath, 1); err != nil {
		t.Fatal(err)
	}
	if err := stage.Release(); err == nil {
		t.Fatal("Release accepted a changed staged database")
	}
	if err := stage.Cleanup(); err != nil {
		t.Fatalf("Cleanup after rejected Release: %v", err)
	}
}

func TestStageCleanupSurvivesParentRenameAndReplacement(t *testing.T) {
	artifactPath, _, _ := createArtifact(t, 128)
	container := t.TempDir()
	parent := filepath.Join(container, "staging")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	stage, err := Extract(context.Background(), artifactPath, parent, []byte(exportPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	renamedParent := filepath.Join(container, "staging-renamed")
	if err := os.Rename(parent, renamedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(parent, "replacement")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	actualStage := filepath.Join(renamedParent, filepath.Base(stage.Directory))
	if _, err := os.Stat(filepath.Join(actualStage, databaseMemberName)); err != nil {
		t.Fatalf("renamed stage missing before cleanup: %v", err)
	}
	if err := stage.Cleanup(); err != nil {
		t.Fatalf("Cleanup through retained parent root: %v", err)
	}
	if _, err := os.Lstat(actualStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup orphaned decrypted stage: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("cleanup damaged replacement parent: %q err=%v", got, err)
	}
}
