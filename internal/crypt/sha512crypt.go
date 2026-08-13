// Package crypt implements SHA-512 crypt ($6$), the password hash rpcd expects
// in /etc/config/rpcd.
//
// Why this exists rather than a dependency: adoption has to write a login for
// the controller's own account, and measurement showed rpcd rejects a plaintext
// password outright (status 6 for both the right and wrong password). The
// target devices carry no mkpasswd, cryptpw or openssl, so the hash must be
// computed controller-side. The algorithm is small, frozen, and published with
// official test vectors — which makes it verifiable in a way a dependency is
// not, for the one primitive guarding device access.
//
// Reference: Ulrich Drepper, "Unix crypt using SHA-256 and SHA-512".
package crypt

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MinRounds/MaxRounds/DefaultRounds are from the specification.
	MinRounds     = 1000
	MaxRounds     = 999999999
	DefaultRounds = 5000

	magic       = "$6$"
	roundsMark  = "rounds="
	maxSaltLen  = 16
	b64Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// permutation is the byte order the specification uses when base64-encoding the
// 64-byte digest, three bytes at a time. The final group encodes one byte.
var permutation = [21][3]int{
	{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4},
	{47, 5, 26}, {6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51},
	{31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
	{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
	{62, 20, 41},
}

// GenerateSalt returns a random salt of the maximum useful length.
func GenerateSalt() (string, error) {
	buf := make([]byte, maxSaltLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, maxSaltLen)
	for i, b := range buf {
		out[i] = b64Alphabet[int(b)%len(b64Alphabet)]
	}
	return string(out), nil
}

// Hash returns a $6$ crypt string for password using a fresh random salt and
// the default round count.
func Hash(password string) (string, error) {
	salt, err := GenerateSalt()
	if err != nil {
		return "", err
	}
	return HashWithSalt(password, salt, DefaultRounds)
}

// HashWithSalt is Hash with an explicit salt and round count, for tests and for
// verifying an existing hash.
func HashWithSalt(password, salt string, rounds int) (string, error) {
	if rounds < MinRounds {
		rounds = MinRounds
	}
	if rounds > MaxRounds {
		rounds = MaxRounds
	}
	if len(salt) > maxSaltLen {
		salt = salt[:maxSaltLen]
	}
	if strings.ContainsAny(salt, "$:\n") {
		return "", errors.New("crypt: salt contains a reserved character")
	}

	pw, sl := []byte(password), []byte(salt)

	// Digest B: password, salt, password.
	bsum := sha512.New()
	bsum.Write(pw)
	bsum.Write(sl)
	bsum.Write(pw)
	b := bsum.Sum(nil)

	// Digest A: password, salt, then B repeated for len(password) bytes, then
	// one of B/password per bit of len(password), high bits first as the loop
	// shifts right.
	asum := sha512.New()
	asum.Write(pw)
	asum.Write(sl)
	for i := len(pw); i > 0; i -= sha512.Size {
		if i > sha512.Size {
			asum.Write(b)
		} else {
			asum.Write(b[:i])
		}
	}
	for i := len(pw); i > 0; i >>= 1 {
		if i%2 != 0 {
			asum.Write(b)
		} else {
			asum.Write(pw)
		}
	}
	a := asum.Sum(nil)

	// Sequence P: password repeated to its own length.
	dpsum := sha512.New()
	for i := 0; i < len(pw); i++ {
		dpsum.Write(pw)
	}
	dp := dpsum.Sum(nil)
	p := repeatTo(dp, len(pw))

	// Sequence S: salt repeated 16+A[0] times, truncated to the salt length.
	dssum := sha512.New()
	for i := 0; i < 16+int(a[0]); i++ {
		dssum.Write(sl)
	}
	ds := dssum.Sum(nil)
	s := repeatTo(ds, len(sl))

	// The stretching loop. This is the cost function; do not "optimise" the
	// allocation away by reusing a hash across iterations — each round is a
	// fresh SHA-512.
	prev := a
	for i := 0; i < rounds; i++ {
		h := sha512.New()
		if i%2 != 0 {
			h.Write(p)
		} else {
			h.Write(prev)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i%2 != 0 {
			h.Write(prev)
		} else {
			h.Write(p)
		}
		prev = h.Sum(nil)
	}

	var sb strings.Builder
	sb.WriteString(magic)
	if rounds != DefaultRounds {
		fmt.Fprintf(&sb, "%s%d$", roundsMark, rounds)
	}
	sb.WriteString(salt)
	sb.WriteByte('$')
	sb.WriteString(encode(prev))
	return sb.String(), nil
}

// Verify reports whether password produces hashed, re-deriving with the salt
// and rounds encoded in it.
func Verify(password, hashed string) bool {
	salt, rounds, err := parse(hashed)
	if err != nil {
		return false
	}
	got, err := HashWithSalt(password, salt, rounds)
	if err != nil {
		return false
	}
	// Not constant-time on purpose: this compares two locally derived strings
	// during self-test, never a secret against attacker-supplied input.
	return got == hashed
}

func parse(hashed string) (salt string, rounds int, err error) {
	if !strings.HasPrefix(hashed, magic) {
		return "", 0, errors.New("crypt: not a $6$ hash")
	}
	rest := hashed[len(magic):]
	rounds = DefaultRounds
	if strings.HasPrefix(rest, roundsMark) {
		end := strings.IndexByte(rest, '$')
		if end < 0 {
			return "", 0, errors.New("crypt: malformed rounds field")
		}
		n, convErr := strconv.Atoi(rest[len(roundsMark):end])
		if convErr != nil {
			return "", 0, fmt.Errorf("crypt: bad rounds: %w", convErr)
		}
		rounds = n
		rest = rest[end+1:]
	}
	end := strings.IndexByte(rest, '$')
	if end < 0 {
		return "", 0, errors.New("crypt: missing salt terminator")
	}
	return rest[:end], rounds, nil
}

// repeatTo builds n bytes by repeating src.
func repeatTo(src []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		take := n - len(out)
		if take > len(src) {
			take = len(src)
		}
		out = append(out, src[:take]...)
	}
	return out
}

// encode applies the specification's byte permutation and its little-endian
// base64 variant, which is NOT standard base64.
func encode(digest []byte) string {
	var sb strings.Builder
	sb.Grow(86)
	for _, idx := range permutation {
		write24(&sb, digest[idx[0]], digest[idx[1]], digest[idx[2]], 4)
	}
	write24(&sb, 0, 0, digest[63], 2)
	return sb.String()
}

func write24(sb *strings.Builder, b2, b1, b0 byte, n int) {
	w := uint32(b2)<<16 | uint32(b1)<<8 | uint32(b0)
	for i := 0; i < n; i++ {
		sb.WriteByte(b64Alphabet[w&0x3f])
		w >>= 6
	}
}
