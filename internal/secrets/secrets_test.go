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

// cheap keeps the test suite fast. Every property under test is independent of
// the cost parameters; DefaultParams is exercised once, in TestDefaultParams.
var cheap = Params{Time: 1, MemoryKiB: 64, Threads: 1}

func newKeeper(t *testing.T, pass string) (*Keeper, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	k, err := Create(path, []byte(pass), cheap)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { k.Close() })
	return k, path
}

func TestSealUnsealSurvivesReopen(t *testing.T) {
	k, path := newKeeper(t, "correct horse battery staple")

	blob, err := k.Seal([]byte("hello"), []byte("ctx"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, []byte("hello")) {
		t.Fatal("plaintext appears verbatim in the sealed value")
	}
	k.Close()

	// The point of the file: a new process with the same passphrase opens what
	// the old one sealed.
	k2, err := Open(path, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer k2.Close()

	got, err := k2.Unseal(blob, []byte("ctx"))
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("round trip: got %q, want %q", got, "hello")
	}
}

func TestWrongPassphrase(t *testing.T) {
	_, path := newKeeper(t, "right")

	_, err := Open(path, []byte("wrong"))
	if !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("Open with wrong passphrase: got %v, want ErrBadPassphrase", err)
	}
}

func TestEmptyPassphraseRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(filepath.Join(dir, FileName), nil, cheap); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("Create with empty passphrase: got %v, want ErrNoPassphrase", err)
	}

	k, path := newKeeper(t, "pass")
	if _, err := Open(path, []byte("")); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("Open with empty passphrase: got %v, want ErrNoPassphrase", err)
	}
	if err := k.ChangePassphrase(nil, cheap); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("ChangePassphrase to empty: got %v, want ErrNoPassphrase", err)
	}
}

// Overwriting a keyring destroys every credential it protects, so Create must
// never do it by accident.
func TestCreateRefusesToOverwrite(t *testing.T) {
	_, path := newKeeper(t, "pass")

	_, err := Create(path, []byte("other"), cheap)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Create: got %v, want ErrExists", err)
	}
}

func TestOpenOrCreateReportsWhichItDid(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	k, created, err := OpenOrCreate(path, []byte("pass"), cheap)
	if err != nil || !created {
		t.Fatalf("first OpenOrCreate: created=%v err=%v", created, err)
	}
	k.Close()

	k2, created, err := OpenOrCreate(path, []byte("pass"), cheap)
	if err != nil {
		t.Fatalf("second OpenOrCreate: %v", err)
	}
	defer k2.Close()
	if created {
		t.Fatal("second OpenOrCreate reported it created the keyring")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	k, _ := newKeeper(t, "pass")

	// A password with a colon in it: the stored form is user:pass, so only the
	// first separator may count.
	const pw = "s3cr:et:with:colons"
	blob, err := k.SealCredential("AA:BB:CC:DD:EE:FF", "oonfeewrt", pw)
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	user, pass, err := k.OpenCredential("aa:bb:cc:dd:ee:ff", blob) // case differs on purpose
	if err != nil {
		t.Fatalf("OpenCredential: %v", err)
	}
	if user != "oonfeewrt" || pass != pw {
		t.Fatalf("got %q/%q, want %q/%q", user, pass, "oonfeewrt", pw)
	}
}

// A sealed credential must not open against a different device. Otherwise a
// blob copied between rows would hand device A's login to device B.
func TestCredentialIsBoundToItsDevice(t *testing.T) {
	k, _ := newKeeper(t, "pass")

	blob, err := k.SealCredential("aa:bb:cc:dd:ee:ff", "oonfeewrt", "pw")
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	if _, _, err := k.OpenCredential("11:22:33:44:55:66", blob); err == nil {
		t.Fatal("a credential sealed for one device opened against another")
	}
}

func TestCredentialInputValidation(t *testing.T) {
	k, _ := newKeeper(t, "pass")

	if _, err := k.SealCredential("", "u", "p"); err == nil {
		t.Error("SealCredential accepted an empty MAC")
	}
	if _, err := k.SealCredential("aa:bb", "", "p"); err == nil {
		t.Error("SealCredential accepted an empty username")
	}
	if _, err := k.SealCredential("aa:bb", "has:colon", "p"); err == nil {
		t.Error("SealCredential accepted a username containing ':'")
	}
	// A device that was never adopted has a NULL cred_enc, which arrives as an
	// empty slice. That must read as "not adopted", not as a crypto failure.
	if _, _, err := k.OpenCredential("aa:bb", nil); err == nil {
		t.Error("OpenCredential accepted an empty blob")
	} else if !strings.Contains(err.Error(), "no sealed credential") {
		t.Errorf("empty blob gave an unhelpful error: %v", err)
	}
}

func TestTamperedCiphertextIsRejected(t *testing.T) {
	k, _ := newKeeper(t, "pass")

	blob, err := k.Seal([]byte("payload"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func([]byte) []byte
	}{
		{"flip a body byte", func(b []byte) []byte {
			c := bytes.Clone(b)
			c[len(c)-1] ^= 0x01
			return c
		}},
		{"flip a nonce byte", func(b []byte) []byte {
			c := bytes.Clone(b)
			c[2] ^= 0x01
			return c
		}},
		{"truncate", func(b []byte) []byte { return b[:len(b)-1] }},
		{"empty", func([]byte) []byte { return nil }},
		{"unknown format version", func(b []byte) []byte {
			c := bytes.Clone(b)
			c[0] = 0xFE
			return c
		}},
	} {
		if _, err := k.Unseal(tc.mut(blob), nil); err == nil {
			t.Errorf("%s: Unseal accepted a corrupted value", tc.name)
		}
	}
}

// Two seals of the same plaintext must differ, or the ciphertext leaks equality
// — which for credentials means leaking that two devices share a password.
func TestNoncesAreFresh(t *testing.T) {
	k, _ := newKeeper(t, "pass")

	a, err := k.Seal([]byte("same"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := k.Seal([]byte("same"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext produced identical ciphertext")
	}
}

// The whole reason for the two-level hierarchy: changing the passphrase must
// not require re-sealing anything.
func TestChangePassphraseLeavesSealedValuesReadable(t *testing.T) {
	k, path := newKeeper(t, "old")

	blob, err := k.SealCredential("aa:bb:cc:dd:ee:ff", "oonfeewrt", "device-pw")
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	if err := k.ChangePassphrase([]byte("new"), cheap); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}

	// The live keeper keeps working — it holds the same data key.
	if _, _, err := k.OpenCredential("aa:bb:cc:dd:ee:ff", blob); err != nil {
		t.Fatalf("live keeper after rekey: %v", err)
	}
	k.Close()

	if _, err := Open(path, []byte("old")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("old passphrase still works after change: %v", err)
	}
	k2, err := Open(path, []byte("new"))
	if err != nil {
		t.Fatalf("Open with new passphrase: %v", err)
	}
	defer k2.Close()

	user, pass, err := k2.OpenCredential("aa:bb:cc:dd:ee:ff", blob)
	if err != nil {
		t.Fatalf("credential sealed before the rekey no longer opens: %v", err)
	}
	if user != "oonfeewrt" || pass != "device-pw" {
		t.Fatalf("got %q/%q after rekey", user, pass)
	}
}

func TestChangePassphraseCanChangeCost(t *testing.T) {
	k, path := newKeeper(t, "pass")
	stronger := Params{Time: 2, MemoryKiB: 128, Threads: 1}
	if err := k.ChangePassphrase([]byte("pass"), stronger); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}
	if got := k.Params(); got != stronger {
		t.Fatalf("Params after rekey: got %+v, want %+v", got, stronger)
	}
	k.Close()

	k2, err := Open(path, []byte("pass"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer k2.Close()
	if got := k2.Params(); got != stronger {
		t.Fatalf("Params from disk: got %+v, want %+v", got, stronger)
	}
}

func TestClosedKeeperRefusesWork(t *testing.T) {
	k, _ := newKeeper(t, "pass")
	blob, err := k.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	k.Close()

	if _, err := k.Seal([]byte("x"), nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Seal after Close: got %v, want ErrClosed", err)
	}
	if _, err := k.Unseal(blob, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Unseal after Close: got %v, want ErrClosed", err)
	}
	if err := k.ChangePassphrase([]byte("new"), cheap); !errors.Is(err, ErrClosed) {
		t.Errorf("ChangePassphrase after Close: got %v, want ErrClosed", err)
	}
	if err := k.Close(); err != nil { // idempotent: shutdown paths double-close
		t.Errorf("second Close: %v", err)
	}
}

func TestKeyringIsNotWorldReadable(t *testing.T) {
	_, path := newKeeper(t, "pass")

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyring is mode %#o, want 0600", perm)
	}
}

// A corrupt or hostile keyring must produce an error, never a panic. argon2
// panics outright on time=0 or threads=0, so this is load-bearing.
func TestMalformedKeyringIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"not json", `{`},
		{"future version", `{"version":99,"kdf":"argon2id","params":{"time":1,"memory_kib":64,"threads":1},"salt":"AAAA","wrapped_key":"AAAA"}`},
		{"unknown kdf", `{"version":1,"kdf":"scrypt","params":{"time":1,"memory_kib":64,"threads":1},"salt":"AAAA","wrapped_key":"AAAA"}`},
		{"zero time", `{"version":1,"kdf":"argon2id","params":{"time":0,"memory_kib":64,"threads":1},"salt":"AAAA","wrapped_key":"AAAA"}`},
		{"zero threads", `{"version":1,"kdf":"argon2id","params":{"time":1,"memory_kib":64,"threads":0},"salt":"AAAA","wrapped_key":"AAAA"}`},
		{"absurd memory", `{"version":1,"kdf":"argon2id","params":{"time":1,"memory_kib":4294967295,"threads":1},"salt":"AAAA","wrapped_key":"AAAA"}`},
		{"memory below floor", `{"version":1,"kdf":"argon2id","params":{"time":1,"memory_kib":1,"threads":4},"salt":"AAAA","wrapped_key":"AAAA"}`},
		{"bad salt", `{"version":1,"kdf":"argon2id","params":{"time":1,"memory_kib":64,"threads":1},"salt":"!!!","wrapped_key":"AAAA"}`},
		{"empty salt", `{"version":1,"kdf":"argon2id","params":{"time":1,"memory_kib":64,"threads":1},"salt":"","wrapped_key":"AAAA"}`},
		{"short wrapped key", `{"version":1,"kdf":"argon2id","params":{"time":1,"memory_kib":64,"threads":1},"salt":"AAAAAAAAAAAAAAAAAAAAAA==","wrapped_key":"AAAA"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, []byte("pass")); err == nil {
				t.Fatal("Open accepted a malformed keyring")
			}
		})
	}
}

func TestEditedHeaderFailsToUnwrap(t *testing.T) {
	_, path := newKeeper(t, "pass")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kr map[string]any
	if err := json.Unmarshal(raw, &kr); err != nil {
		t.Fatal(err)
	}
	// Claim cheaper parameters than the ones the key was wrapped under.
	kr["params"] = map[string]any{"time": 1, "memory_kib": 32, "threads": 1}
	edited, err := json.Marshal(kr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, []byte("pass")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("edited header: got %v, want ErrBadPassphrase", err)
	}
}

func TestMissingKeyring(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), FileName), []byte("pass"))
	if err == nil {
		t.Fatal("Open succeeded on a missing keyring")
	}
	if errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("a missing keyring is reported as a bad passphrase: %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing keyring should wrap os.ErrNotExist, got %v", err)
	}
}

func TestReadPassphraseFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("strips one trailing newline", func(t *testing.T) {
		p := filepath.Join(dir, "pass1")
		if err := os.WriteFile(p, []byte("hunter2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPassphraseFile(p)
		if err != nil {
			t.Fatalf("ReadPassphraseFile: %v", err)
		}
		if string(got) != "hunter2" {
			t.Fatalf("got %q, want %q", got, "hunter2")
		}
	})

	t.Run("keeps internal and leading whitespace", func(t *testing.T) {
		p := filepath.Join(dir, "pass2")
		if err := os.WriteFile(p, []byte(" a b \r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPassphraseFile(p)
		if err != nil {
			t.Fatalf("ReadPassphraseFile: %v", err)
		}
		if string(got) != " a b " {
			t.Fatalf("got %q, want %q", got, " a b ")
		}
	})

	t.Run("refuses group or world readable", func(t *testing.T) {
		p := filepath.Join(dir, "pass3")
		if err := os.WriteFile(p, []byte("hunter2"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPassphraseFile(p); err == nil {
			t.Fatal("accepted a world-readable passphrase file")
		}
	})

	t.Run("refuses empty", func(t *testing.T) {
		p := filepath.Join(dir, "pass4")
		if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPassphraseFile(p); err == nil {
			t.Fatal("accepted an empty passphrase file")
		}
	})

	t.Run("refuses a directory", func(t *testing.T) {
		if _, err := ReadPassphraseFile(dir); err == nil {
			t.Fatal("accepted a directory as a passphrase file")
		}
	})

	t.Run("reports a missing file", func(t *testing.T) {
		if _, err := ReadPassphraseFile(filepath.Join(dir, "nope")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("got %v, want os.ErrNotExist", err)
		}
	})
}

// The shipped cost parameters must actually be usable, and the derivation must
// be deterministic — a keyring that opens on one run and not the next would be
// the worst possible failure here.
func TestDefaultParams(t *testing.T) {
	if err := DefaultParams().validate(); err != nil {
		t.Fatalf("DefaultParams does not validate: %v", err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	k, err := Create(path, []byte("pass"), DefaultParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blob, err := k.Seal([]byte("v"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	k.Close()

	for i := range 2 {
		k2, err := Open(path, []byte("pass"))
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		got, err := k2.Unseal(blob, nil)
		if err != nil || string(got) != "v" {
			t.Fatalf("Open %d: unseal gave %q, %v", i, got, err)
		}
		k2.Close()
	}
}

func TestConcurrentUse(t *testing.T) {
	k, _ := newKeeper(t, "pass")
	blob, err := k.SealCredential("aa:bb:cc:dd:ee:ff", "u", "p")
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	done := make(chan error, 8)
	for range 8 {
		go func() {
			for range 20 {
				if _, _, err := k.OpenCredential("aa:bb:cc:dd:ee:ff", blob); err != nil {
					done <- err
					return
				}
				if _, err := k.Seal([]byte("x"), nil); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent use: %v", err)
		}
	}
}

// ---- operator passwords ----

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword([]byte("correct horse"), cheap)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("hash is not in PHC format: %q", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Fatal("the hash contains the password")
	}
	if err := VerifyPassword([]byte("correct horse"), h); err != nil {
		t.Fatalf("VerifyPassword on the right password: %v", err)
	}
	if err := VerifyPassword([]byte("wrong horse"), h); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("VerifyPassword on a wrong password: got %v, want ErrBadPassword", err)
	}
	if err := VerifyPassword(nil, h); err == nil {
		t.Fatal("an empty password verified")
	}
}

// Two hashes of one password must differ, or the store leaks which operators
// share a password.
func TestPasswordSaltIsFresh(t *testing.T) {
	a, err := HashPassword([]byte("same"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword([]byte("same"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical")
	}
	// Both still verify: the salt travels inside the hash.
	if err := VerifyPassword([]byte("same"), a); err != nil {
		t.Error(err)
	}
	if err := VerifyPassword([]byte("same"), b); err != nil {
		t.Error(err)
	}
}

// The parameters travel with the hash, so raising the cost later must not lock
// existing operators out.
func TestPasswordVerifiesUnderItsOwnParameters(t *testing.T) {
	old, err := HashPassword([]byte("pw"), Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword([]byte("pw"), old); err != nil {
		t.Fatalf("a hash made with weaker parameters no longer verifies: %v", err)
	}
	stronger := Params{Time: 3, MemoryKiB: 256, Threads: 2}
	if !NeedsRehash(old, stronger) {
		t.Error("NeedsRehash did not flag a hash weaker than the current policy")
	}
	fresh, err := HashPassword([]byte("pw"), stronger)
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(fresh, stronger) {
		t.Error("NeedsRehash flagged a hash made with the current policy")
	}
}

// A corrupted row must be an error, never a panic — argon2 panics outright on
// time or threads of zero.
func TestMalformedPasswordHashIsRejected(t *testing.T) {
	for _, tc := range []struct{ name, hash string }{
		{"empty", ""},
		{"not phc", "hunter2"},
		{"wrong algorithm", "$argon2i$v=19$m=64,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"},
		{"future version", "$argon2id$v=99$m=64,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"},
		{"zero time", "$argon2id$v=19$m=64,t=0,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"},
		{"zero threads", "$argon2id$v=19$m=64,t=1,p=0$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"},
		{"absurd memory", "$argon2id$v=19$m=4294967295,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"},
		{"short salt", "$argon2id$v=19$m=64,t=1,p=1$YWJj$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"},
		{"missing hash", "$argon2id$v=19$m=64,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyPassword([]byte("pw"), tc.hash); err == nil {
				t.Fatal("VerifyPassword accepted a malformed hash")
			}
			if !NeedsRehash(tc.hash, DefaultParams()) {
				t.Error("NeedsRehash did not flag an unparseable hash")
			}
		})
	}
}
