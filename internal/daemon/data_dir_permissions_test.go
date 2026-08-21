package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("data directory mode = %04o, want 0700", got)
	}
}
