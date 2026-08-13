package secrets

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// HashPassword produces a PHC-format argon2id hash for an operator password.
//
// The format is the standard one — `$argon2id$v=19$m=...,t=...,p=...$salt$hash`
// — so the parameters travel with the hash. That is what makes it possible to
// raise the cost later without invalidating everyone's password: an old hash
// still verifies under its own parameters, and can be re-hashed on the next
// successful sign-in.
//
// This is not internal/crypt. That package produces SHA-512 crypt because rpcd's
// on-device format demands it, at a cost a router can afford. Nothing constrains
// this one to be cheap, so it is not.
func HashPassword(password []byte, p Params) (string, error) {
	if len(password) == 0 {
		return "", errors.New("secrets: password is empty")
	}
	if err := p.validate(); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secrets: generate salt: %w", err)
	}
	sum := argon2.IDKey(password, salt, p.Time, p.MemoryKiB, p.Threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// ErrBadPassword is returned when a password does not match.
var ErrBadPassword = errors.New("secrets: password does not match")

// VerifyPassword checks a password against a PHC-format argon2id hash.
//
// The comparison is constant-time. A byte-wise early exit leaks how much of the
// hash matched, which over enough attempts is a usable signal — and an
// authentication endpoint is exactly where an attacker gets enough attempts.
func VerifyPassword(password []byte, encoded string) error {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey(password, salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrBadPassword
	}
	return nil
}

// NeedsRehash reports that a stored hash was made with weaker parameters than
// the ones now in force, so it should be replaced on the next successful login.
// Raising the cost is worth nothing if existing accounts keep the old one.
func NeedsRehash(encoded string, want Params) bool {
	p, _, _, err := parsePHC(encoded)
	if err != nil {
		return true // unparseable is worse than weak
	}
	return p.Time < want.Time || p.MemoryKiB < want.MemoryKiB || p.Threads < want.Threads
}

func parsePHC(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, errors.New("secrets: password hash is malformed")
	}
	if parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("secrets: password hash uses %q, not argon2id", parts[1])
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, errors.New("secrets: password hash has no version")
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf(
			"secrets: password hash is argon2 v%d, this build implements v%d",
			version, argon2.Version)
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, errors.New("secrets: password hash has malformed parameters")
	}
	// The parameters come out of the database, and argon2 panics on time or
	// threads of zero. A corrupted row must be an error, not a crash.
	if err := p.validate(); err != nil {
		return Params{}, nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Params{}, nil, nil, errors.New("secrets: password hash has a malformed salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 {
		return Params{}, nil, nil, errors.New("secrets: password hash is malformed")
	}
	return p, salt, want, nil
}
