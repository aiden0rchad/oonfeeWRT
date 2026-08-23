package controllerrestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var orphanToken = strings.Repeat("a", 32)

func TestCleanupOrphanPreparedRemovesOnlyExactOwnedShapes(t *testing.T) {
	dataDir := t.TempDir()
	owned := []string{
		".oonfeewrt-restore-" + orphanToken + ".stage",
		".oonfeewrt-prepare-work-" + orphanToken + ".tmp",
		".oonfeewrt-prepared-pair-" + orphanToken + ".stage",
	}
	for _, name := range owned {
		path := filepath.Join(dataDir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "sensitive"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := []string{
		".oonfeewrt-restore-0123456789abcdef0123456789abcde.stage",
		".oonfeewrt-restore-0123456789abcdef0123456789abcdeg.stage",
		".oonfeewrt-restore-" + orphanToken + ".tmp",
		".oonfeewrt-preview-" + orphanToken + ".tmp",
		"unrelated",
	}
	for _, name := range unrelated {
		if err := os.Mkdir(filepath.Join(dataDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanupOrphanPrepared(context.Background(), dataDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range owned {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned orphan %q remains: %v", name, err)
		}
	}
	for _, name := range unrelated {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); err != nil {
			t.Fatalf("unrelated child %q was removed: %v", name, err)
		}
	}
}

func TestCleanupOrphanPreparedRejectsSymlinksAndNonPrivateMatches(t *testing.T) {
	dataDir := t.TempDir()
	target := t.TempDir()
	targetFile := filepath.Join(target, "keep")
	if err := os.WriteFile(targetFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkName := ".oonfeewrt-restore-" + orphanToken + ".stage"
	if err := os.Symlink(target, filepath.Join(dataDir, linkName)); err != nil {
		t.Fatal(err)
	}
	nonPrivateName := ".oonfeewrt-prepare-work-" + orphanToken + ".tmp"
	if err := os.Mkdir(filepath.Join(dataDir, nonPrivateName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanupOrphanPrepared(context.Background(), dataDir); err == nil {
		t.Fatal("unsafe matching entries were accepted")
	}
	if info, err := os.Lstat(filepath.Join(dataDir, linkName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("matching symlink was followed or removed: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, nonPrivateName)); err != nil {
		t.Fatalf("non-private matching directory was removed: %v", err)
	}
	if _, err := os.Lstat(targetFile); err != nil {
		t.Fatalf("symlink target was changed: %v", err)
	}
}

func TestCleanupOrphanPreparedHonorsCancellation(t *testing.T) {
	dataDir := t.TempDir()
	name := ".oonfeewrt-prepared-pair-" + orphanToken + ".stage"
	if err := os.Mkdir(filepath.Join(dataDir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CleanupOrphanPrepared(ctx, dataDir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup error=%v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, name)); err != nil {
		t.Fatalf("canceled cleanup changed the owned directory: %v", err)
	}
}

func TestCleanupOrphanPreparedRemovesCrashedPreparedPair(t *testing.T) {
	fixture := newPrepareFixture(t, store.CurrentSchemaVersion(), nil)
	prepared, err := Prepare(context.Background(), fixture.artifact, fixture.dataDir,
		fixture.live, testExportPassphrase, testDestinationRuntimePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := prepared.dir.path
	if err := prepared.dir.release(); err != nil {
		t.Fatal(err)
	}
	if !isPreparedOrphanName(filepath.Base(path)) {
		t.Fatalf("prepared directory has non-owned shape: %q", filepath.Base(path))
	}
	if err := CleanupOrphanPrepared(context.Background(), fixture.dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed prepared pair remains: %v", err)
	}
	fixture.assertNoResidue(t)
}
