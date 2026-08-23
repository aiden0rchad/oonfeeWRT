// Command recoverycheck proves that one controller database and its exact
// sibling keyring form a readable current-schema recovery pair. It performs no
// network calls and opens SQLite through the store's read-only boundary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aiden0rchad/oonfeewrt/internal/recovery"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/toolstore"
)

func main() {
	dbPath, err := recoveryDatabasePath(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "recoverycheck:", err)
		os.Exit(2)
	}
	if err := run(context.Background(), dbPath, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "recoverycheck:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dbPath string, output io.Writer) error {
	counts, err := inspectRecovery(ctx, dbPath)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output,
		"schema=%d devices=%d credentials=%d owned_sections=%d wlans=%d meshes=%d\n",
		counts.Schema, counts.Devices, counts.Credentials, counts.OwnedSections,
		counts.WLANs, counts.Meshes)
	return err
}

func inspectRecovery(ctx context.Context, dbPath string) (recovery.Counts, error) {
	if ctx == nil {
		return recovery.Counts{}, errors.New("recovery context is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return recovery.Counts{}, err
	}
	if err := validatePairFiles(dbPath); err != nil {
		return recovery.Counts{}, err
	}
	handle, err := toolstore.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return recovery.Counts{}, fmt.Errorf("open recovery pair: %w", err)
	}
	counts, inspectErr := recovery.Validate(ctx, handle.DB, handle)
	if closeErr := handle.Close(); inspectErr == nil && closeErr != nil {
		return recovery.Counts{}, errors.New("close recovery pair failed")
	}
	return counts, inspectErr
}

func validatePairFiles(dbPath string) error {
	for label, path := range map[string]string{
		"recovery database": dbPath,
		"sibling keyring":   filepath.Join(filepath.Dir(dbPath), secrets.FileName),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s is unavailable: %w", label, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular non-symlink file", label)
		}
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Lstat(dbPath + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect recovery database SQLite sidecar %q: %w", suffix, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != 0 {
			return fmt.Errorf("recovery database has non-empty or unsafe SQLite sidecar %q; "+
				"verify an isolated SQLite .backup or clean-shutdown/checkpoint copy", suffix)
		}
	}
	return nil
}

func recoveryDatabasePath(args []string, output io.Writer) (string, error) {
	fs := flag.NewFlagSet("recoverycheck", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "usage: recoverycheck /path/to/recovery/oonfeewrt.db")
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return "", errors.New("expected exactly one recovery database")
	}
	path := fs.Arg(0)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("recovery database %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("recovery database %q is not a regular non-symlink file", path)
	}
	return path, nil
}
