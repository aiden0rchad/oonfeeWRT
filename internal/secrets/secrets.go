// Package secrets seals controller credentials, wireless keys, and
// secret-derived verifiers so a copied controller database does not disclose
// reusable access material.
//
// The shape is a two-level key hierarchy, which is worth stating plainly
// because it is the reason changing the operator passphrase is cheap:
//
//	passphrase --argon2id--> KEK --wraps--> DEK --seals--> each protected value
//
// Credentials are sealed under a random 32-byte data key (the DEK). The DEK is
// itself sealed under a key derived from the operator passphrase (the KEK) and
// stored in a small keyring file beside the database. Changing the passphrase
// re-wraps one 32-byte key; it does not touch database ciphertexts, so it cannot
// half-succeed across a fleet and leave only some values unreadable.
//
// Failing to unwrap the DEK is the passphrase check. There is no separate
// verifier, and there should not be: a verifier is one more thing that can
// disagree with the key it claims to describe.
//
// # What this does and does not protect
//
// It protects controller secrets at rest — a stolen database file, a backup,
// a snapshot. It does not protect against an attacker who can read the daemon's
// memory while it runs, because the DEK is in memory the whole time by
// necessity: polling and rendering need them while running. Claiming
// otherwise would be theatre.
package secrets

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// FileName is the keyring's name inside the data directory.
const FileName = "keyring.json"

// DefaultPath returns the keyring path for a data directory.
func DefaultPath(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Errors callers are expected to branch on.
var (
	// ErrBadPassphrase means the DEK would not unwrap. Deliberately worded for
	// both causes: an AEAD failure cannot distinguish a wrong passphrase from a
	// corrupted keyring, and pretending otherwise would send an operator
	// hunting the wrong problem.
	ErrBadPassphrase = errors.New("secrets: cannot unwrap the key — the passphrase is " +
		"wrong, or the keyring file has been corrupted or truncated")

	// ErrExists is returned by Create when a keyring is already present.
	// Overwriting one destroys every protected database value, so it is never
	// silent.
	ErrExists = errors.New("secrets: a keyring already exists at this path")

	// ErrClosed is returned once Close has zeroed the key material.
	ErrClosed = errors.New("secrets: keeper is closed")

	// ErrNoPassphrase rejects an empty passphrase up front. An empty passphrase
	// derives a perfectly valid key that protects nothing, and it would not be
	// noticed until someone read the database.
	ErrNoPassphrase = errors.New("secrets: passphrase is empty")
)

// Params are the argon2id cost parameters. They are stored in the keyring
// because the derivation's output depends on every one of them — a key derived
// with different parallelism is a different key, not a slower one.
type Params struct {
	Time      uint32 `json:"time"`       // passes
	MemoryKiB uint32 `json:"memory_kib"` // memory cost
	Threads   uint8  `json:"threads"`    // parallelism
}

// DefaultParams follows RFC 9106's second recommended option (t=3, m=64 MiB,
// p=4). The derivation happens once, at daemon start, so the cost is paid on a
// path where a second of latency does not matter and an attacker's dictionary
// run does.
func DefaultParams() Params { return Params{Time: 3, MemoryKiB: 64 * 1024, Threads: 4} }

// Bounds on what we will accept out of a keyring file.
//
// This is not pedantry: argon2.IDKey *panics* on time or threads of zero, so a
// truncated or hostile header would otherwise take the daemon down, and an
// absurd memory figure would have it try to allocate its way out of existence.
// Validating on the way in turns both into an error message.
const (
	maxMemoryKiB        = 128 * 1024
	maxTime             = 6
	maxThreads          = 8
	saltLen             = 16
	keyLen              = 32
	blobVersion         = 1
	wrappedKeyRawSize   = 1 + chacha20poly1305.NonceSizeX + keyLen + chacha20poly1305.Overhead
	maxKeyringFileBytes = 4096
)

func (p Params) validate() error {
	switch {
	case p.Time < 1 || p.Time > maxTime:
		return fmt.Errorf("secrets: argon2id time=%d out of range 1..%d", p.Time, maxTime)
	case p.Threads < 1:
		return errors.New("secrets: argon2id threads must be at least 1")
	case p.Threads > maxThreads:
		return fmt.Errorf("secrets: argon2id threads=%d exceeds the %d ceiling",
			p.Threads, maxThreads)
	case p.MemoryKiB < 8*uint32(p.Threads):
		return fmt.Errorf("secrets: argon2id memory=%d KiB is below the floor of 8 KiB per thread",
			p.MemoryKiB)
	case p.MemoryKiB > maxMemoryKiB:
		return fmt.Errorf("secrets: argon2id memory=%d KiB exceeds the %d KiB ceiling",
			p.MemoryKiB, maxMemoryKiB)
	}
	return nil
}

// keyring is the on-disk file. It holds no secret that is usable without the
// passphrase, but it is still written 0600 — its contents are exactly what an
// offline dictionary attack needs.
type keyring struct {
	Version int    `json:"version"`
	KDF     string `json:"kdf"`
	Params  Params `json:"params"`
	Salt    string `json:"salt"`        // base64
	Wrapped string `json:"wrapped_key"` // base64, sealed DEK
}

// wrapAAD binds the wrapped DEK to the header it lives in, so any edit to that
// header fails cleanly at unwrap.
//
// Most of this is belt-and-braces — salt and cost parameters already feed the
// derivation, so changing them produces a different KEK and the unwrap fails
// regardless. The part that is not redundant is the version and algorithm
// identifier, which the KDF never sees: without the AAD, a future build that
// understands two wrapping schemes could be steered to the weaker one by editing
// one string in a file.
func (k keyring) wrapAAD() []byte {
	return []byte(fmt.Sprintf("oonfeewrt/keyring/v%d|%s|t=%d,m=%d,p=%d|%s",
		k.Version, k.KDF, k.Params.Time, k.Params.MemoryKiB, k.Params.Threads, k.Salt))
}

// Keeper holds the unwrapped data key and seals and opens values with it.
//
// It is safe for concurrent use: the poll loop opens credentials from several
// goroutines, and Close may land at any point during shutdown.
type Keeper struct {
	path string

	mu     sync.RWMutex
	dek    []byte
	params Params
}

// Create writes a new keyring, generating a fresh data key.
//
// It refuses to overwrite an existing file. That is the difference between
// first-run setup and destroying every credential in the database, and the two
// must never be one flag apart.
func Create(path string, passphrase []byte, p Params) (*Keeper, error) {
	if len(passphrase) == 0 {
		return nil, ErrNoPassphrase
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secrets: stat %s: %w", path, err)
	}

	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		zero(dek)
		return nil, fmt.Errorf("secrets: generate data key: %w", err)
	}
	kr, err := wrap(dek, passphrase, p)
	if err != nil {
		zero(dek)
		return nil, err
	}
	if err := writeNewKeyring(path, kr); err != nil {
		zero(dek)
		return nil, err
	}
	return &Keeper{path: path, dek: dek, params: p}, nil
}

// Open reads a keyring and unwraps the data key.
func Open(path string, passphrase []byte) (*Keeper, error) {
	if len(passphrase) == 0 {
		return nil, ErrNoPassphrase
	}
	kr, err := readKeyring(path)
	if err != nil {
		return nil, err
	}
	dek, err := unwrapKeyring(kr, passphrase, path)
	if err != nil {
		return nil, err
	}
	return &Keeper{path: path, dek: dek, params: kr.Params}, nil
}

func unwrapKeyring(kr keyring, passphrase []byte, source string) ([]byte, error) {
	salt, err := decodeFixedBase64(kr.Salt, saltLen)
	if err != nil {
		return nil, fmt.Errorf("secrets: %s: malformed salt: %w", source, err)
	}
	defer zero(salt)
	wrapped, err := decodeFixedBase64(kr.Wrapped, wrappedKeyRawSize)
	if err != nil {
		return nil, fmt.Errorf("secrets: %s: malformed wrapped key: %w", source, err)
	}
	defer zero(wrapped)
	kek := argon2.IDKey(passphrase, salt, kr.Params.Time, kr.Params.MemoryKiB,
		kr.Params.Threads, keyLen)
	defer zero(kek)
	dek, err := openWith(kek, wrapped, kr.wrapAAD())
	if err != nil {
		return nil, ErrBadPassphrase
	}
	if len(dek) != keyLen {
		zero(dek)
		return nil, fmt.Errorf("secrets: %s: data key is %d bytes, expected %d",
			source, len(dek), keyLen)
	}
	return dek, nil
}

// OpenOrCreate opens an existing keyring or creates one, reporting which it did
// so the caller can tell the operator that this was first-run initialisation
// rather than let a typo'd data directory look like a fresh install.
//
// p applies only when creating; an existing keyring keeps the parameters it was
// written with, which is what makes it openable at all.
func OpenOrCreate(path string, passphrase []byte, p Params) (k *Keeper, created bool, err error) {
	switch _, statErr := os.Stat(path); {
	case statErr == nil:
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return nil, false, err
		}
		k, err = Open(path, passphrase)
		return k, false, err
	case errors.Is(statErr, os.ErrNotExist):
		k, err = Create(path, passphrase, p)
		if errors.Is(err, ErrExists) {
			// Another first-run process atomically installed its keyring after
			// our Stat. Its complete file is now the only valid winner.
			if syncErr := syncDirectory(filepath.Dir(path)); syncErr != nil {
				return nil, false, syncErr
			}
			k, err = Open(path, passphrase)
			return k, false, err
		}
		return k, true, err
	default:
		return nil, false, fmt.Errorf("secrets: stat %s: %w", path, statErr)
	}
}

// Params reports the cost parameters this keyring was derived with.
func (k *Keeper) Params() Params {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.params
}

// Seal encrypts plaintext, binding it to aad. The ciphertext is
// self-describing: a version byte, then the nonce, then the sealed body.
//
// aad is authenticated but not stored, so opening requires the caller to
// reproduce it — which is the point. See SealCredential for how that is used to
// pin a credential to one device.
func (k *Keeper) Seal(plaintext, aad []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.dek == nil {
		return nil, ErrClosed
	}
	return sealWith(k.dek, plaintext, aad)
}

// Unseal reverses Seal. A wrong aad fails exactly like a corrupt ciphertext,
// because to the AEAD it is the same event.
func (k *Keeper) Unseal(blob, aad []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.dek == nil {
		return nil, ErrClosed
	}
	return openWith(k.dek, blob, aad)
}

// HMACSHA256 returns a deterministic keyed digest for one purpose and message.
// The purpose is length-framed under a versioned prefix, so equal messages in
// different domains cannot share a digest. The data key never leaves Keeper.
func (k *Keeper) HMACSHA256(domain string, message []byte) ([]byte, error) {
	if domain == "" {
		return nil, errors.New("secrets: HMAC domain is empty")
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.dek == nil {
		return nil, ErrClosed
	}

	mac := hmac.New(sha256.New, k.dek)
	_, _ = mac.Write([]byte("oonfeewrt/keyed-digest/v1"))
	var domainLen [8]byte
	binary.BigEndian.PutUint64(domainLen[:], uint64(len(domain)))
	_, _ = mac.Write(domainLen[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write(message)
	return mac.Sum(nil), nil
}

// ChangePassphrase re-derives the key-encryption key and rewrites the keyring.
//
// Only the wrapped data key changes, so no credential is re-sealed and nothing
// in the database is touched. The write is atomic; a crash mid-change leaves the
// old passphrase working rather than leaving no passphrase working.
func (k *Keeper) ChangePassphrase(newPassphrase []byte, p Params) error {
	if len(newPassphrase) == 0 {
		return ErrNoPassphrase
	}
	if err := p.validate(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dek == nil {
		return ErrClosed
	}
	if k.path == "" {
		return errors.New("secrets: a pathless keeper cannot change an on-disk passphrase")
	}
	kr, err := wrap(k.dek, newPassphrase, p)
	if err != nil {
		return err
	}
	if err := writeKeyring(k.path, kr); err != nil {
		return err
	}
	k.params = p
	return nil
}

// Close zeroes the data key. Every later Seal or Unseal returns ErrClosed
// rather than a nil-pointer panic in a shutdown path.
//
// Go gives no guarantee the bytes are not still in a copy somewhere — the
// garbage collector moves nothing today, but this is a best effort, not a
// promise. It is worth doing anyway: it bounds the window in a core dump.
func (k *Keeper) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	zero(k.dek)
	k.dek = nil
	return nil
}

// ---- credential helpers ----

// SealCredential seals a device login, binding it to that device's MAC.
//
// The binding matters: without it, a sealed blob copied from one device row to
// another would open cleanly, and the controller would cheerfully offer device
// A's credential to device B. With it, a moved blob fails to open. It does not
// stop an attacker who can rewrite the MAC column too — nothing at this layer
// could — but it makes the accident, which is the likely case, impossible.
func (k *Keeper) SealCredential(mac, username, password string) ([]byte, error) {
	if mac == "" {
		return nil, errors.New("secrets: device MAC is required to seal a credential")
	}
	// The stored form is `username:password` (schema.sql), so a colon in the
	// username would make the split ambiguous. rpcd logins never contain one;
	// rejecting it is cheaper than a format that could silently truncate a
	// credential.
	if strings.Contains(username, ":") {
		return nil, errors.New("secrets: username must not contain ':'")
	}
	if username == "" {
		return nil, errors.New("secrets: username is required")
	}
	return k.Seal([]byte(username+":"+password), credAAD(mac))
}

// OpenCredential opens a credential sealed for this device.
func (k *Keeper) OpenCredential(mac string, blob []byte) (username, password string, err error) {
	if mac == "" {
		return "", "", errors.New("secrets: device MAC is required to open a credential")
	}
	if len(blob) == 0 {
		return "", "", errors.New("secrets: no sealed credential stored for this device")
	}
	pt, err := k.Unseal(blob, credAAD(mac))
	if err != nil {
		return "", "", fmt.Errorf("secrets: cannot open the credential for %s "+
			"(wrong keyring, or the record does not belong to this device): %w", mac, err)
	}
	defer zero(pt)
	user, pass, ok := strings.Cut(string(pt), ":")
	if !ok {
		return "", "", fmt.Errorf("secrets: stored credential for %s is malformed", mac)
	}
	return user, pass, nil
}

// VerifyCredential proves that a sealed credential belongs to this keyring and
// device without materialising it as Go strings. Store uses this before a
// schema migration so a wrong keyring cannot mutate an existing database.
func (k *Keeper) VerifyCredential(mac string, blob []byte) error {
	if mac == "" {
		return errors.New("secrets: device MAC is required to verify a credential")
	}
	if len(blob) == 0 {
		return errors.New("secrets: no sealed credential stored for this device")
	}
	pt, err := k.Unseal(blob, credAAD(mac))
	if err != nil {
		return fmt.Errorf("secrets: cannot verify the credential for %s (wrong keyring, or the record does not belong to this device): %w", mac, err)
	}
	defer zero(pt)
	for _, b := range pt {
		if b == ':' {
			return nil
		}
	}
	return fmt.Errorf("secrets: stored credential for %s is malformed", mac)
}

// credAAD normalises the MAC so that a device recorded as AA:BB:.. and looked up
// as aa:bb:.. does not fail to decrypt for a reason nobody would guess.
func credAAD(mac string) []byte {
	return []byte("oonfeewrt/cred/v1|" + strings.ToLower(strings.TrimSpace(mac)))
}

// ---- passphrase sources ----

// ReadPassphraseFile loads a passphrase from a file, for unattended boot.
//
// This is the documented tradeoff in IMPLEMENTATION §10: a file the daemon can
// read without a human is a file an attacker who reaches the host can read too,
// so the passphrase stops being a second factor and becomes a filesystem
// permission. That is a legitimate choice for a headless install and a bad
// surprise if nobody said it out loud.
//
// The permission check is strict — any group or world bit is refused — because
// unlike the keyring, this file alone is sufficient. It is never accepted from
// an environment variable: env is visible in /proc, inherited by children, and
// captured by crash reporters.
func ReadPassphraseFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: passphrase file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets: passphrase file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("secrets: passphrase file %s is mode %#o; it must not be "+
			"readable by group or other (chmod 600)", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: passphrase file: %w", err)
	}
	// Strip one trailing newline, which every editor and `echo` adds. Nothing
	// else is trimmed: trailing spaces in a passphrase are legitimate, and
	// quietly removing them would lock the operator out of their own keyring.
	pass := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if pass == "" {
		return nil, fmt.Errorf("secrets: passphrase file %s is empty", path)
	}
	return []byte(pass), nil
}

// ---- AEAD ----

// sealWith produces [version][nonce][ciphertext+tag].
//
// XChaCha20-Poly1305's 192-bit nonce is the reason nonces are simply random
// here. With the 96-bit variant, random nonces carry a birthday bound that has
// to be reasoned about per key; at 192 bits it is not a consideration, and a
// counter that must survive restarts is one fewer thing to get wrong.
func sealWith(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: init cipher: %w", err)
	}
	out := make([]byte, 1+aead.NonceSize(), 1+aead.NonceSize()+len(plaintext)+aead.Overhead())
	out[0] = blobVersion
	nonce := out[1:]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	return aead.Seal(out, nonce, plaintext, aad), nil
}

func openWith(key, blob, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: init cipher: %w", err)
	}
	if len(blob) < 1+aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("secrets: sealed value is too short to be valid")
	}
	if blob[0] != blobVersion {
		return nil, fmt.Errorf("secrets: sealed value has format version %d, this build "+
			"understands %d", blob[0], blobVersion)
	}
	nonce := blob[1 : 1+aead.NonceSize()]
	pt, err := aead.Open(nil, nonce, blob[1+aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("secrets: sealed value failed authentication")
	}
	return pt, nil
}

func wrap(dek, passphrase []byte, p Params) (keyring, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return keyring{}, fmt.Errorf("secrets: generate salt: %w", err)
	}
	kr := keyring{
		Version: 1,
		KDF:     "argon2id",
		Params:  p,
		Salt:    base64.StdEncoding.EncodeToString(salt),
	}
	kek := argon2.IDKey(passphrase, salt, p.Time, p.MemoryKiB, p.Threads, keyLen)
	defer zero(kek)

	wrapped, err := sealWith(kek, dek, kr.wrapAAD())
	if err != nil {
		return keyring{}, err
	}
	kr.Wrapped = base64.StdEncoding.EncodeToString(wrapped)
	return kr, nil
}

// ---- file I/O ----

func readKeyring(path string) (keyring, error) {
	file, err := os.Open(path)
	if err != nil {
		return keyring{}, fmt.Errorf("secrets: read keyring: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return keyring{}, fmt.Errorf("secrets: inspect keyring: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxKeyringFileBytes {
		file.Close()
		return keyring{}, fmt.Errorf("secrets: keyring must be a regular file no larger than %d bytes",
			maxKeyringFileBytes)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxKeyringFileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return keyring{}, fmt.Errorf("secrets: read keyring: %w", readErr)
	}
	if closeErr != nil {
		return keyring{}, fmt.Errorf("secrets: close keyring after read: %w", closeErr)
	}
	if len(data) > maxKeyringFileBytes {
		return keyring{}, fmt.Errorf("secrets: keyring exceeds %d-byte ceiling", maxKeyringFileBytes)
	}
	var kr keyring
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&kr); err != nil {
		return keyring{}, fmt.Errorf("secrets: %s is not a valid keyring file: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("another JSON value follows the keyring")
		}
		return keyring{}, fmt.Errorf("secrets: %s is not a valid keyring file: %w", path, err)
	}
	if kr.Version != 1 {
		return keyring{}, fmt.Errorf("secrets: keyring version %d is not supported by this build",
			kr.Version)
	}
	if kr.KDF != "argon2id" {
		return keyring{}, fmt.Errorf("secrets: keyring uses unknown KDF %q", kr.KDF)
	}
	if err := kr.Params.validate(); err != nil {
		return keyring{}, fmt.Errorf("secrets: keyring %s: %w", path, err)
	}
	return kr, nil
}

// writeNewKeyring installs the first keyring atomically without replacing a
// concurrent winner. Linking a fully synced same-directory temp file is the
// portable no-clobber primitive: link fails with EEXIST instead of overwriting.
func writeNewKeyring(path string, kr keyring) error {
	return writeKeyringFile(path, kr, true, syncDirectory)
}

// writeKeyring replaces the keyring atomically.
//
// A torn keyring makes every protected database value unreadable, so this is
// the one place in the package that earns the full temp-file-fsync-rename-fsync dance: rename is
// atomic, but without the fsyncs a crash can leave the directory entry pointing
// at a file whose contents never reached the disk.
func writeKeyring(path string, kr keyring) error {
	return writeKeyringFile(path, kr, false, syncDirectory)
}

func writeKeyringFile(path string, kr keyring, exclusive bool, syncDir func(string) error) error {
	data, err := json.MarshalIndent(kr, "", "  ")
	if err != nil {
		return fmt.Errorf("secrets: encode keyring: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".keyring-*.tmp")
	if err != nil {
		return fmt.Errorf("secrets: create temp keyring: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has happened

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: chmod temp keyring: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: write temp keyring: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: sync temp keyring: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: close temp keyring: %w", err)
	}
	if exclusive {
		if err := os.Link(tmpName, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%w: %s", ErrExists, path)
			}
			return fmt.Errorf("secrets: install new keyring: %w", err)
		}
	} else if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("secrets: replace keyring: %w", err)
	}
	if err := syncDir(dir); err != nil {
		if !exclusive {
			return err
		}
		rollbackErr := removeLinkedKeyring(path, tmpName)
		if rollbackErr == nil {
			rollbackErr = syncDir(dir)
		}
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("secrets: roll back new keyring: %w", rollbackErr))
		}
		return err
	}
	return nil
}

func removeLinkedKeyring(path, tempPath string) error {
	tempInfo, err := os.Lstat(tempPath)
	if err != nil {
		return fmt.Errorf("inspect temporary keyring: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect installed keyring: %w", err)
	}
	if !os.SameFile(tempInfo, pathInfo) {
		return errors.New("installed keyring changed before rollback")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove installed keyring: %w", err)
	}
	return nil
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("secrets: open keyring directory for sync: %w", err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("secrets: sync keyring directory: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("secrets: close keyring directory after sync: %w", err)
	}
	return nil
}

// zero overwrites key material in place.
func zero(b []byte) { clear(b) }
