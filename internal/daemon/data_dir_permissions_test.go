package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

func TestOpenTightensAnExistingDataDirectory(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "controller-data")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "operator passphrase")
	cfg.DataDir = dataDir

	d, err := Open(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for path, want := range map[string]os.FileMode{
		dataDir:                      0o700,
		secrets.DefaultPath(dataDir): 0o600,
		cfg.DBPath():                 0o600,
		cfg.DBPath() + "-wal":        0o600,
		cfg.DBPath() + "-shm":        0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", filepath.Base(path), got, want)
		}
	}
}
