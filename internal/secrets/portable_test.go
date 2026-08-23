package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortableKeyRoundTripDoesNotChangeLiveKeyring(t *testing.T) {
	live, livePath := newKeeper(t, "runtime passphrase")
	before, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := live.Seal([]byte("portable fixture"), []byte("backup/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := live.VerifyPassphrase([]byte("runtime passphrase")); err != nil {
		t.Fatalf("VerifyPassphrase: %v", err)
	}
	if err := live.writeNewKeyring(livePath, []byte("must not replace live"), cheap); !errors.Is(err, ErrExists) {
		t.Fatalf("re-wrap at live path=%v, want ErrExists", err)
	}

	portable, err := live.ExportPortableKey([]byte("separate export passphrase"))
	if err != nil {
		t.Fatalf("ExportPortableKey: %v", err)
	}
	if len(portable) > maxPortableKeyBytes || bytes.Contains(portable, live.dek) {
		t.Fatalf("portable key size=%d or contains raw data key", len(portable))
	}
	temporary, err := OpenPortableKey(portable, []byte("separate export passphrase"))
	if err != nil {
		t.Fatalf("OpenPortableKey: %v", err)
	}
	defer temporary.Close()
	plain, err := temporary.Unseal(sealed, []byte("backup/v1"))
	if err != nil || string(plain) != "portable fixture" {
		t.Fatalf("temporary Keeper cannot open live ciphertext: %q err=%v", plain, err)
	}
	zero(plain)

	destination := filepath.Join(t.TempDir(), FileName)
	if err := temporary.WriteNewKeyring(destination, []byte("destination runtime passphrase")); err != nil {
		t.Fatalf("WriteNewKeyring: %v", err)
	}
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("destination keyring mode=%v, want regular 0600", info.Mode())
	}
	reopened, err := Open(destination, []byte("destination runtime passphrase"))
	if err != nil {
		t.Fatalf("open destination keyring: %v", err)
	}
	defer reopened.Close()
	plain, err = reopened.Unseal(sealed, []byte("backup/v1"))
	if err != nil || string(plain) != "portable fixture" {
		t.Fatalf("destination Keeper cannot open live ciphertext: %q err=%v", plain, err)
	}
	zero(plain)

	after, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("portable export or re-wrap changed the live keyring")
	}
}

func TestPortableKeyRejectsWrongAndSwappedPassphrases(t *testing.T) {
	live, livePath := newKeeper(t, "runtime")
	portable, err := live.exportPortableKey([]byte("export"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPortableKey(portable, []byte("wrong")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong export passphrase=%v, want ErrBadPassphrase", err)
	}
	if err := live.VerifyPassphrase([]byte("wrong")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong runtime passphrase=%v, want ErrBadPassphrase", err)
	}

	original, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(t.TempDir(), FileName)
	other, err := Create(otherPath, []byte("other runtime"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	swapped, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, swapped, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(livePath, original, 0o600) })
	if err := live.VerifyPassphrase([]byte("other runtime")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("swapped keyring=%v, want ErrBadPassphrase", err)
	}
}

func TestPortableKeyRejectsHostileKDFBeforeDerivation(t *testing.T) {
	live, livePath := newKeeper(t, "runtime")
	portable, err := live.exportPortableKey([]byte("export"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	var envelope portableKey
	if err := json.Unmarshal(portable, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Params.MemoryKiB = maxPortableMemoryKiB + 1
	hostile, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPortableKey(hostile, []byte("export")); err == nil ||
		!strings.Contains(err.Error(), "portable ceiling") {
		t.Fatalf("hostile portable KDF error=%v", err)
	}

	original, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	var runtime keyring
	if err := json.Unmarshal(original, &runtime); err != nil {
		t.Fatal(err)
	}
	runtime.Params.MemoryKiB = maxMemoryKiB
	hostile, err = json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, hostile, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(livePath, original, 0o600) })
	if err := live.VerifyPassphrase([]byte("runtime")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("hostile runtime KDF error=%v, want ErrBadPassphrase", err)
	}
}

func TestPortableKeyInputBounds(t *testing.T) {
	live, _ := newKeeper(t, "runtime")
	portable, err := live.exportPortableKey([]byte("export"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(portable, &envelope); err != nil {
		t.Fatal(err)
	}
	withUnknown := make(map[string]any, len(envelope)+1)
	for key, value := range envelope {
		withUnknown[key] = value
	}
	withUnknown["unexpected"] = true
	unknown, err := json.Marshal(withUnknown)
	if err != nil {
		t.Fatal(err)
	}
	badBase64 := make(map[string]any, len(envelope))
	for key, value := range envelope {
		badBase64[key] = value
	}
	badBase64["salt"] = strings.Repeat("!", len(envelope["salt"].(string)))
	badEncoding, err := json.Marshal(badBase64)
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{
		"empty":          nil,
		"oversized":      make([]byte, maxPortableKeyBytes+1),
		"trailing value": append(bytes.Clone(portable), []byte(` {}`)...),
		"unknown field":  unknown,
		"bad base64":     badEncoding,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenPortableKey(input, []byte("export")); err == nil {
				t.Fatal("OpenPortableKey accepted hostile input")
			}
		})
	}
	tooLong := bytes.Repeat([]byte{'x'}, maxPortablePassphrase+1)
	if _, err := live.ExportPortableKey(tooLong); err == nil {
		t.Fatal("ExportPortableKey accepted an oversized passphrase")
	}
}

func TestWriteNewKeyringIsNoClobber(t *testing.T) {
	live, _ := newKeeper(t, "runtime")
	portable, err := live.exportPortableKey([]byte("export"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := OpenPortableKey(portable, []byte("export"))
	if err != nil {
		t.Fatal(err)
	}
	defer temporary.Close()

	destination := filepath.Join(t.TempDir(), FileName)
	sentinel := []byte("do not replace")
	if err := os.WriteFile(destination, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := temporary.writeNewKeyring(destination, []byte("new runtime"), cheap); !errors.Is(err, ErrExists) {
		t.Fatalf("existing destination=%v, want ErrExists", err)
	}
	after, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(after, sentinel) {
		t.Fatalf("existing destination changed: %q err=%v", after, err)
	}
	if err := temporary.ChangePassphrase([]byte("not allowed"), cheap); err == nil {
		t.Fatal("pathless Keeper accepted ChangePassphrase")
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(filepath.Dir(target), FileName)
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if err := temporary.writeNewKeyring(symlink, []byte("new runtime"), cheap); !errors.Is(err, ErrExists) {
		t.Fatalf("symlink destination=%v, want ErrExists", err)
	}
	after, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(after, sentinel) {
		t.Fatalf("symlink target changed: %q err=%v", after, err)
	}

	missingParent := filepath.Join(t.TempDir(), "missing")
	if err := temporary.writeNewKeyring(filepath.Join(missingParent, FileName),
		[]byte("new runtime"), cheap); err == nil {
		t.Fatal("WriteNewKeyring created a missing parent")
	}
	if _, err := os.Lstat(missingParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent was changed: %v", err)
	}
}

func TestWriteNewKeyringRollsBackFailedDirectorySync(t *testing.T) {
	dek := bytes.Repeat([]byte{0x42}, keyLen)
	defer zero(dek)
	kr, err := wrap(dek, []byte("runtime"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, FileName)
	syncFailure := errors.New("injected directory sync failure")
	syncCalls := 0
	err = writeKeyringFile(destination, kr, true, func(gotDir string) error {
		if gotDir != dir {
			t.Fatalf("sync directory=%q, want %q", gotDir, dir)
		}
		syncCalls++
		if syncCalls == 1 {
			return syncFailure
		}
		return nil
	})
	if !errors.Is(err, syncFailure) {
		t.Fatalf("write error=%v, want injected sync failure", err)
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls=%d, want install plus rollback", syncCalls)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed write left published destination: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".keyring-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("failed write left temporary files %v, err=%v", matches, err)
	}
	if err := writeNewKeyring(destination, kr); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}
