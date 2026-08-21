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

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/toolstore"
)

type recoveryCounts struct {
	schema, devices, credentials, owned, wlans, meshes int
}

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
		counts.schema, counts.devices, counts.credentials, counts.owned,
		counts.wlans, counts.meshes)
	return err
}

func inspectRecovery(ctx context.Context, dbPath string) (recoveryCounts, error) {
	if err := validatePairFiles(dbPath); err != nil {
		return recoveryCounts{}, err
	}
	handle, err := toolstore.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return recoveryCounts{}, fmt.Errorf("open recovery pair: %w", err)
	}
	counts, inspectErr := inspectOpenRecovery(ctx, handle)
	if closeErr := handle.Close(); inspectErr == nil && closeErr != nil {
		return recoveryCounts{}, errors.New("close recovery pair failed")
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

func inspectOpenRecovery(ctx context.Context, handle *toolstore.Handle) (recoveryCounts, error) {
	var counts recoveryCounts
	if err := handle.DB.SQL().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&counts.schema); err != nil {
		return counts, errors.New("read schema version failed")
	}
	var integrity string
	if err := handle.DB.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return counts, errors.New("database integrity check failed")
	}
	foreignKeys, err := handle.DB.SQL().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return counts, errors.New("foreign-key check failed")
	}
	hasForeignKeyFailure := foreignKeys.Next()
	foreignKeyReadErr := foreignKeys.Err()
	foreignKeyCloseErr := foreignKeys.Close()
	if hasForeignKeyFailure || foreignKeyReadErr != nil || foreignKeyCloseErr != nil {
		return counts, errors.New("foreign-key check failed")
	}

	site, err := handle.DB.Site(ctx)
	if err != nil {
		return counts, errors.New("stored site could not be opened")
	}
	if len(site.Validate()) != 0 {
		return counts, errors.New("stored site validation failed")
	}
	counts.wlans, counts.meshes = len(site.WLANs), len(site.Meshes)

	devices, err := handle.DB.Devices(ctx)
	if err != nil {
		return counts, errors.New("device inventory could not be read")
	}
	counts.devices = len(devices)
	for _, device := range devices {
		if device.FunctionError != "" {
			return counts, errors.New("device inventory validation failed")
		}
		if device.Adopted() && len(device.CredEnc) == 0 {
			return counts, errors.New("an adopted device has no stored credential")
		}
		if len(device.CredEnc) == 0 {
			continue
		}
		if err := handle.VerifyCredential(device.MAC, device.CredEnc); err != nil {
			return counts, errors.New("a stored device credential failed verification")
		}
		counts.credentials++
	}

	var invalidOwned int
	if err := handle.DB.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM owned_sections
		 WHERE rendered_hash_enc IS NULL OR length(rendered_hash_enc)=0
		    OR COALESCE(rendered_hash,'')<>''`).Scan(&invalidOwned); err != nil || invalidOwned != 0 {
		return counts, errors.New("an owned-section verifier is missing or not sealed")
	}
	rows, err := handle.DB.SQL().QueryContext(ctx,
		`SELECT DISTINCT device_id FROM owned_sections ORDER BY device_id`)
	if err != nil {
		return counts, errors.New("owned-section inventory could not be read")
	}
	var ownerIDs []int64
	for rows.Next() {
		var deviceID int64
		if err := rows.Scan(&deviceID); err != nil {
			rows.Close()
			return counts, errors.New("owned-section inventory could not be read")
		}
		ownerIDs = append(ownerIDs, deviceID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return counts, errors.New("owned-section inventory could not be read")
	}
	if err := rows.Close(); err != nil {
		return counts, errors.New("owned-section inventory could not be read")
	}
	for _, deviceID := range ownerIDs {
		owned, err := handle.DB.OwnedSections(ctx, deviceID)
		if err != nil {
			return counts, errors.New("an owned-section verifier failed verification")
		}
		counts.owned += len(owned)
	}
	return counts, nil
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
