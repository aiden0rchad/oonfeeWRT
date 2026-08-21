package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabasePathRequiresAnExistingFile(t *testing.T) {
	var output bytes.Buffer
	if _, err := databasePath(nil, &output); err == nil {
		t.Fatal("missing path was accepted")
	}

	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := databasePath([]string{missing}, &output); err == nil {
		t.Fatal("a missing path was accepted; SQLite would create it")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argument validation created %q: %v", missing, err)
	}

	dir := t.TempDir()
	if _, err := databasePath([]string{dir}, &output); err == nil {
		t.Fatal("a directory was accepted as the controller database")
	}

	path := filepath.Join(t.TempDir(), "oonfeewrt.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := databasePath([]string{path}, &output); err != nil || got != path {
		t.Fatalf("databasePath() = %q, %v; want %q", got, err, path)
	}
}

func TestDatabasePathHelpDoesNotBecomeAPath(t *testing.T) {
	var output bytes.Buffer
	_, err := databasePath([]string{"-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h error = %v, want flag.ErrHelp", err)
	}
	if output.Len() == 0 {
		t.Fatal("-h printed no usage")
	}
}
