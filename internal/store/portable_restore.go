package store

import (
	"context"
	"fmt"
)

// ClearPortableRestoreClientProvenance removes source-relative observations
// from a disposable portable-restore copy. Those observations attest what one
// specific controller instance recently learned from one device; moving them
// to another instance would turn historical evidence into write authority.
// Global client inventory and desired intent remain intact.
func (db *DB) ClearPortableRestoreClientProvenance(ctx context.Context) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM client_observations`); err != nil {
		return fmt.Errorf("store: clear portable-restore client provenance: %w", err)
	}
	return nil
}
