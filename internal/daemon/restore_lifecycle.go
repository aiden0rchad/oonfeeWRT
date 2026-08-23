package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/controllerrestore"
	"github.com/aiden0rchad/oonfeewrt/internal/restoreswap"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var ErrRestoreRestart = errors.New("daemon: controlled controller-restore restart")

const restorePromotionTimeout = 30 * time.Minute

type RestartKind string

const RestartControllerRestore RestartKind = "controller_restore"

type RestartRequest struct {
	Kind RestartKind
}

type RestoreRestartError struct {
	Request RestartRequest
}

func (e *RestoreRestartError) Error() string { return ErrRestoreRestart.Error() }
func (e *RestoreRestartError) Unwrap() error { return ErrRestoreRestart }

type shutdownOperations struct {
	checkpoint        func(context.Context) error
	closeStore        func() error
	closeKeys         func() error
	markClean         func(context.Context, string, string) error
	afterRequestDrain func() error
	promotionTimeout  time.Duration
}

func (d *Daemon) checkpointStore(ctx context.Context) error {
	if d.shutdownOps.checkpoint != nil {
		return d.shutdownOps.checkpoint(ctx)
	}
	return d.Store.Checkpoint(ctx)
}

func (d *Daemon) closeStore() error {
	if d.shutdownOps.closeStore != nil {
		return d.shutdownOps.closeStore()
	}
	return d.Store.Close()
}

func (d *Daemon) closeKeys() error {
	if d.shutdownOps.closeKeys != nil {
		return d.shutdownOps.closeKeys()
	}
	return d.Keys.Close()
}

func (d *Daemon) markRestoreClean(ctx context.Context) error {
	if d.shutdownOps.markClean != nil {
		return d.shutdownOps.markClean(ctx, d.Config.DataDir, d.restoreOwnerInstanceID)
	}
	_, err := restoreswap.MarkCleanShutdown(ctx, d.Config.DataDir, d.restoreOwnerInstanceID)
	return err
}

func newRestoreOwnerInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("daemon: generate restore owner instance ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func keyringFirstRun(cfg Config, pendingRestore bool) (bool, error) {
	path := secrets.DefaultPath(cfg.DataDir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if pendingRestore {
			return false, nil
		}
		if info, dbErr := os.Stat(cfg.DBPath()); dbErr == nil && info.Size() > 0 {
			return false, fmt.Errorf("daemon: keyring %s is missing but database %s already exists; restore the matching keyring backup or move the database aside before starting a new controller",
				path, cfg.DBPath())
		} else if dbErr != nil && !errors.Is(dbErr, os.ErrNotExist) {
			return false, fmt.Errorf("daemon: stat database before keyring creation: %w", dbErr)
		}
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("daemon: stat keyring: %w", err)
	}
	return false, nil
}

func (d *Daemon) reconcileRestoreStartup(ctx context.Context, passphrase []byte) error {
	result, err := restoreswap.ApplyPending(ctx, d.Config.DataDir, passphrase, d.Config.Version)
	switch {
	case err == nil:
		d.restoreApplied = result
	case errors.Is(err, restoreswap.ErrNoPendingIntent):
	case errors.Is(err, restoreswap.ErrUncleanIntent):
		if abortErr := restoreswap.AbortUnclean(ctx, d.Config.DataDir); abortErr != nil {
			return errors.Join(errors.New("daemon: abort unclean restore intent"), abortErr)
		}
		d.Log.Warn("discarded an unclean controller-restore intent; canonical controller state was not swapped")
	default:
		return fmt.Errorf("daemon: reconcile pending controller restore: %w", err)
	}
	if err := controllerrestore.CleanupOrphanPrepared(ctx, d.Config.DataDir); err != nil {
		return fmt.Errorf("daemon: clean orphaned restore preparation: %w", err)
	}
	suppression, err := restoreswap.SuppressionStatus(d.Config.DataDir)
	if err != nil {
		return fmt.Errorf("daemon: read router-write suppression: %w", err)
	}
	d.suppression = suppression
	return nil
}

func (d *Daemon) recordAppliedRestore(ctx context.Context) error {
	result, err := restoreswap.PendingAppliedReceipt(d.Config.DataDir)
	if errors.Is(err, restoreswap.ErrNoAppliedReceipt) {
		if d.restoreApplied.Applied {
			return errors.New("daemon: applied controller restore is missing its durable audit receipt")
		}
		suppression := d.RouterWriteSuppression()
		if !suppression.Active {
			return nil
		}
		var recorded int
		if err := d.Store.SQL().QueryRowContext(ctx, `
SELECT COUNT(*) FROM events
WHERE event='controller.restore_applied'
  AND json_extract(detail_json,'$.restore_id')=?`, suppression.RestoreID).Scan(&recorded); err != nil {
			return fmt.Errorf("daemon: verify applied controller restore audit: %w", err)
		}
		if recorded == 0 {
			return errors.New("daemon: router writes are suppressed for a restore with no durable applied audit receipt")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemon: read applied controller restore receipt: %w", err)
	}
	if d.restoreApplied.Applied && d.restoreApplied.RestoreID != result.RestoreID {
		return errors.New("daemon: applied controller restore receipt identity mismatch")
	}
	suppression := d.RouterWriteSuppression()
	if !suppression.Active || suppression.RestoreID != result.RestoreID {
		return errors.New("daemon: applied controller restore is missing its matching router-write suppression")
	}
	d.restoreApplied = result
	detail := map[string]any{
		"restore_id": result.RestoreID, "safety_backup": result.SafetyBackup,
		"authorizing_admin_id": result.AuthorizingAdminID,
		"authorizing_username": result.AuthorizingUsername,
		"preview_id":           result.PreviewID, "plan_id": result.PlanID,
		"safety_backup_sha256": result.SafetyBackupSHA256,
		"database_sha256":      result.PreparedDatabaseSHA256,
		"keyring_sha256":       result.PreparedKeyringSHA256,
		"schema":               result.Counts.Schema, "devices": result.Counts.Devices,
		"credentials":    result.Counts.Credentials,
		"owned_sections": result.Counts.OwnedSections,
		"wlans":          result.Counts.WLANs, "meshes": result.Counts.Meshes,
		"router_writes_suppressed": true,
	}
	d.Log.Warn("controller restore applied; router writes remain suppressed for owner review",
		"restore_id", result.RestoreID, "safety_backup", result.SafetyBackup,
		"safety_backup_sha256", result.SafetyBackupSHA256)
	exists, err := d.restoreAppliedAuditExists(ctx, result)
	if err != nil {
		return err
	}
	if !exists {
		if err := d.Store.LogEvent(ctx, store.Event{TS: time.Now().Unix(), Category: "audit",
			Severity: "warning", Event: "controller.restore_applied", Detail: detail}); err != nil {
			return fmt.Errorf("daemon: record applied controller restore: %w", err)
		}
	}
	if err := restoreswap.ClearAppliedReceipt(ctx, d.Config.DataDir, result.RestoreID); err != nil {
		return fmt.Errorf("daemon: acknowledge applied controller restore audit: %w", err)
	}
	return nil
}

func (d *Daemon) restoreAppliedAuditExists(ctx context.Context, result restoreswap.Result) (bool, error) {
	var total, matching int
	err := d.Store.SQL().QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(CASE
  WHEN CAST(json_extract(detail_json,'$.authorizing_admin_id') AS INTEGER)=?
   AND json_extract(detail_json,'$.authorizing_username')=?
   AND json_extract(detail_json,'$.preview_id')=?
   AND json_extract(detail_json,'$.plan_id')=? THEN 1 ELSE 0 END),0)
FROM events
WHERE event='controller.restore_applied'
  AND json_extract(detail_json,'$.restore_id')=?`,
		result.AuthorizingAdminID, result.AuthorizingUsername, result.PreviewID, result.PlanID,
		result.RestoreID).Scan(&total, &matching)
	if err != nil {
		return false, fmt.Errorf("daemon: inspect applied controller restore audit: %w", err)
	}
	if total != matching {
		return false, errors.New("daemon: applied controller restore audit binding conflicts with its receipt")
	}
	return total > 0, nil
}

// RequestRestoreRestart is safe to call after an accepted confirmation has
// written its 202 response. It never blocks and only the first call wins.
func (d *Daemon) RequestRestoreRestart() {
	if d == nil {
		return
	}
	d.restoreRestartOnce.Do(func() {
		d.restoreRestartAccepted.Store(true)
		select {
		case d.restoreRestart <- RestartRequest{Kind: RestartControllerRestore}:
		default:
			d.restoreRestartAccepted.Store(false)
		}
	})
}

func (d *Daemon) RouterWritesSuppressed() bool {
	if d == nil {
		return false
	}
	d.suppressionMu.RLock()
	defer d.suppressionMu.RUnlock()
	return d.suppression.Active
}

func (d *Daemon) RouterWriteSuppression() restoreswap.Suppression {
	if d == nil {
		return restoreswap.Suppression{}
	}
	d.suppressionMu.RLock()
	defer d.suppressionMu.RUnlock()
	return d.suppression
}

// ResumeRouterWrites removes the durable fence before opening admission in
// memory. A failed disk clear therefore always leaves router writes blocked.
func (d *Daemon) ResumeRouterWrites(ctx context.Context, restoreID string) error {
	if d == nil {
		return errors.New("daemon: controller is unavailable")
	}
	if err := restoreswap.ClearSuppression(ctx, d.Config.DataDir, restoreID); err != nil {
		return err
	}
	d.suppressionMu.Lock()
	d.suppression = restoreswap.Suppression{}
	d.suppressionMu.Unlock()
	return nil
}
