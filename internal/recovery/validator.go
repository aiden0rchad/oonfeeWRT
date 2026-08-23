// Package recovery validates an already-open controller database for staged
// restore without owning paths, keyrings, passphrases, or replacement policy.
package recovery

import (
	"context"
	"errors"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// CredentialVerifier authenticates a sealed device credential without
// returning its username or password.
type CredentialVerifier interface {
	VerifyCredential(mac string, blob []byte) error
}

// Counts is the public, non-secret result of a successful validation.
type Counts struct {
	Schema        int
	Devices       int
	Credentials   int
	OwnedSections int
	WLANs         int
	Meshes        int
}

// Validate traverses one read-only snapshot and authenticates every sealed
// record. It neither closes nor writes through the caller-owned handles.
func Validate(ctx context.Context, db *store.DB, verifier CredentialVerifier) (Counts, error) {
	var counts Counts
	if ctx == nil {
		return counts, errors.New("recovery context is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if db == nil {
		return counts, errors.New("recovery database is unavailable")
	}
	if verifier == nil {
		return counts, errors.New("recovery credential verifier is unavailable")
	}

	result, err := db.InspectRecovery(ctx, verifier.VerifyCredential, secrets.ValidatePasswordHash)
	if err != nil {
		return counts, err
	}
	return Counts{
		Schema:        result.Schema,
		Devices:       result.Devices,
		Credentials:   result.Credentials,
		OwnedSections: result.OwnedSections,
		WLANs:         result.WLANs,
		Meshes:        result.Meshes,
	}, nil
}
