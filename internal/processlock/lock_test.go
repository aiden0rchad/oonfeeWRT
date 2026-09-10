package processlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusiveAndReleases(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Acquire(dir); !errors.Is(err, ErrInUse) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second acquire error=%v, want ErrInUse", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock permissions=%#o, want private", info.Mode().Perm())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
