package toolstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestOpenReadOnlyRequiresPassphraseFileAndMatchingKeyring(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "controller.db")
	keyringPath := filepath.Join(dir, secrets.FileName)
	passphrase := []byte("placeholder tool passphrase")
	keeper, err := secrets.Create(keyringPath, passphrase,
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, "sqlite", dbPath, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	keeper.Close()

	t.Setenv(passphraseFileEnv, "")
	if handle, err := OpenReadOnly(ctx, dbPath); err == nil {
		handle.Close()
		t.Fatal("tool opened the database without a passphrase-file path")
	}
	passFile := filepath.Join(dir, "passphrase")
	if err := os.WriteFile(passFile, append(passphrase, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(passphraseFileEnv, passFile)
	handle, err := OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	handle, err = OpenWritable(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.DB.SQL().ExecContext(ctx,
		`INSERT INTO events(ts,category,severity,event) VALUES(1,'system','info','tool-fixture')`); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("writable tool left %d bytes in WAL", info.Size())
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	wrongFile := filepath.Join(dir, "wrong-passphrase")
	if err := os.WriteFile(wrongFile, []byte(strings.Repeat("X", 24)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(passphraseFileEnv, wrongFile)
	if handle, err := OpenReadOnly(ctx, dbPath); err == nil {
		handle.Close()
		t.Fatal("tool opened the database with an unrelated passphrase")
	}
}
