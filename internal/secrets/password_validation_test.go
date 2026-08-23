package secrets

import (
	"strings"
	"testing"
)

func TestValidatePasswordHashParsesWithoutPassword(t *testing.T) {
	hash, err := HashPassword([]byte("correct horse battery staple"),
		Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePasswordHash(hash); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		"",
		"plaintext",
		strings.Replace(hash, "v=19", "v=19junk", 1),
		strings.Replace(hash, "p=1", "p=1junk", 1),
		strings.Repeat("x", maxPasswordHashBytes+1),
	} {
		if err := ValidatePasswordHash(invalid); err == nil {
			t.Fatalf("invalid password hash of length %d was accepted", len(invalid))
		}
	}
}
