package secrets

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

const (
	portableKeyFormat     = "oonfeewrt-portable-key"
	portableKeyVersion    = 1
	maxPortableKeyBytes   = 4096
	maxPortablePassphrase = 4096
	maxPortableTime       = 6
	maxPortableMemoryKiB  = 128 * 1024
	maxPortableThreads    = 8
)

type portableKey struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	KDF     string `json:"kdf"`
	Params  Params `json:"params"`
	Salt    string `json:"salt"`
	Wrapped string `json:"wrapped_key"`
}

func (p portableKey) wrapAAD() []byte {
	return []byte(fmt.Sprintf("oonfeewrt/portable-key/v%d|%s|%s|t=%d,m=%d,p=%d|%s",
		p.Version, p.Format, p.KDF, p.Params.Time, p.Params.MemoryKiB,
		p.Params.Threads, p.Salt))
}

// VerifyPassphrase proves candidate opens this Keeper's current on-disk
// keyring to the same live data key. Opening an unrelated swapped keyring is
// therefore not accepted merely because candidate opens that file.
func (k *Keeper) VerifyPassphrase(candidate []byte) error {
	passphrase, err := ownedPortablePassphrase(candidate)
	if err != nil {
		return err
	}
	defer zero(passphrase)

	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.dek == nil {
		return ErrClosed
	}
	if k.path == "" {
		return errors.New("secrets: a pathless keeper has no runtime keyring to verify")
	}
	kr, err := readKeyring(k.path)
	if err != nil {
		return err
	}
	// The live parameters were already paid at startup. Refusing a changed
	// header before Argon2 prevents a swapped file from selecting a hostile
	// memory or CPU cost during reauthentication.
	if kr.Params != k.params {
		return ErrBadPassphrase
	}
	diskDEK, err := unwrapKeyring(kr, passphrase, k.path)
	if err != nil {
		return err
	}
	defer zero(diskDEK)
	if subtle.ConstantTimeCompare(diskDEK, k.dek) != 1 {
		return ErrBadPassphrase
	}
	return nil
}

// ExportPortableKey wraps the live data key under a separate export
// passphrase. It never changes the live keyring or returns the raw data key.
// The caller retains ownership of exportPassphrase and must clear it.
func (k *Keeper) ExportPortableKey(exportPassphrase []byte) ([]byte, error) {
	return k.exportPortableKey(exportPassphrase, DefaultParams())
}

func (k *Keeper) exportPortableKey(exportPassphrase []byte, p Params) ([]byte, error) {
	passphrase, err := ownedPortablePassphrase(exportPassphrase)
	if err != nil {
		return nil, err
	}
	defer zero(passphrase)
	if err := validatePortableParams(p); err != nil {
		return nil, err
	}

	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.dek == nil {
		return nil, ErrClosed
	}
	wrapped, err := wrapPortableKey(k.dek, passphrase, p)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(wrapped)
	if err != nil {
		return nil, fmt.Errorf("secrets: encode portable key: %w", err)
	}
	if len(data) > maxPortableKeyBytes {
		zero(data)
		return nil, errors.New("secrets: encoded portable key exceeds its size ceiling")
	}
	return data, nil
}

// OpenPortableKey unwraps a bounded portable key into a pathless temporary
// Keeper. Close must be called to zero its data key.
func OpenPortableKey(data, exportPassphrase []byte) (*Keeper, error) {
	passphrase, err := ownedPortablePassphrase(exportPassphrase)
	if err != nil {
		return nil, err
	}
	defer zero(passphrase)
	portable, salt, wrapped, err := decodePortableKey(data)
	if err != nil {
		return nil, err
	}
	defer zero(salt)
	defer zero(wrapped)

	kek := argon2.IDKey(passphrase, salt, portable.Params.Time,
		portable.Params.MemoryKiB, portable.Params.Threads, keyLen)
	defer zero(kek)
	dek, err := openWith(kek, wrapped, portable.wrapAAD())
	if err != nil {
		return nil, ErrBadPassphrase
	}
	if len(dek) != keyLen {
		zero(dek)
		return nil, fmt.Errorf("secrets: portable data key is %d bytes, expected %d", len(dek), keyLen)
	}
	return &Keeper{dek: dek, params: portable.Params}, nil
}

// WriteNewKeyring re-wraps this Keeper's data key under a destination runtime
// passphrase at a nonexistent path. It does not mutate this Keeper.
// The caller retains ownership of runtimePassphrase and must clear it.
func (k *Keeper) WriteNewKeyring(path string, runtimePassphrase []byte) error {
	return k.writeNewKeyring(path, runtimePassphrase, DefaultParams())
}

func (k *Keeper) writeNewKeyring(path string, runtimePassphrase []byte, p Params) error {
	passphrase, err := ownedPortablePassphrase(runtimePassphrase)
	if err != nil {
		return err
	}
	defer zero(passphrase)
	if err := validatePortableParams(p); err != nil {
		return err
	}

	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.dek == nil {
		return ErrClosed
	}
	kr, err := wrap(k.dek, passphrase, p)
	if err != nil {
		return err
	}
	return writeNewKeyring(path, kr)
}

func wrapPortableKey(dek, passphrase []byte, p Params) (portableKey, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		zero(salt)
		return portableKey{}, fmt.Errorf("secrets: generate portable key salt: %w", err)
	}
	defer zero(salt)
	portable := portableKey{
		Format:  portableKeyFormat,
		Version: portableKeyVersion,
		KDF:     "argon2id",
		Params:  p,
		Salt:    base64.StdEncoding.EncodeToString(salt),
	}
	kek := argon2.IDKey(passphrase, salt, p.Time, p.MemoryKiB, p.Threads, keyLen)
	defer zero(kek)
	wrapped, err := sealWith(kek, dek, portable.wrapAAD())
	if err != nil {
		return portableKey{}, err
	}
	defer zero(wrapped)
	portable.Wrapped = base64.StdEncoding.EncodeToString(wrapped)
	return portable, nil
}

func decodePortableKey(data []byte) (portableKey, []byte, []byte, error) {
	if len(data) == 0 || len(data) > maxPortableKeyBytes {
		return portableKey{}, nil, nil,
			fmt.Errorf("secrets: portable key size must be 1..%d bytes", maxPortableKeyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var portable portableKey
	if err := decoder.Decode(&portable); err != nil {
		return portableKey{}, nil, nil, fmt.Errorf("secrets: invalid portable key JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("another JSON value follows the portable key")
		}
		return portableKey{}, nil, nil, fmt.Errorf("secrets: invalid portable key JSON: %w", err)
	}
	if portable.Format != portableKeyFormat || portable.Version != portableKeyVersion ||
		portable.KDF != "argon2id" {
		return portableKey{}, nil, nil, errors.New("secrets: unsupported portable key format")
	}
	if err := validatePortableParams(portable.Params); err != nil {
		return portableKey{}, nil, nil, fmt.Errorf("secrets: portable key KDF: %w", err)
	}
	salt, err := decodeFixedBase64(portable.Salt, saltLen)
	if err != nil {
		return portableKey{}, nil, nil, fmt.Errorf("secrets: portable key salt: %w", err)
	}
	wrapped, err := decodeFixedBase64(portable.Wrapped, wrappedKeyRawSize)
	if err != nil {
		zero(salt)
		return portableKey{}, nil, nil, fmt.Errorf("secrets: portable wrapped key: %w", err)
	}
	return portable, salt, wrapped, nil
}

func decodeFixedBase64(encoded string, size int) ([]byte, error) {
	if len(encoded) != base64.StdEncoding.EncodedLen(size) {
		return nil, fmt.Errorf("encoded length is %d, expected %d",
			len(encoded), base64.StdEncoding.EncodedLen(size))
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != size {
		zero(decoded)
		return nil, errors.New("value is not canonical base64 of the expected size")
	}
	return decoded, nil
}

func validatePortableParams(p Params) error {
	switch {
	case p.Time > maxPortableTime:
		return fmt.Errorf("argon2id time=%d exceeds portable ceiling %d", p.Time, maxPortableTime)
	case p.MemoryKiB > maxPortableMemoryKiB:
		return fmt.Errorf("argon2id memory=%d KiB exceeds portable ceiling %d KiB",
			p.MemoryKiB, maxPortableMemoryKiB)
	case p.Threads > maxPortableThreads:
		return fmt.Errorf("argon2id threads=%d exceeds portable ceiling %d",
			p.Threads, maxPortableThreads)
	}
	return p.validate()
}

func ownedPortablePassphrase(passphrase []byte) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, ErrNoPassphrase
	}
	if len(passphrase) > maxPortablePassphrase {
		return nil, fmt.Errorf("secrets: passphrase exceeds %d-byte ceiling", maxPortablePassphrase)
	}
	return bytes.Clone(passphrase), nil
}
