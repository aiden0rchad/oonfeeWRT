package crypt

import (
	"strings"
	"testing"
)

// The official vectors from Ulrich Drepper's specification. A hand-rolled
// implementation of a security primitive is only defensible if it is checked
// against the reference — these are that check.
func TestOfficialVectors(t *testing.T) {
	cases := []struct {
		password string
		salt     string
		rounds   int
		want     string
	}{
		{
			"Hello world!", "saltstring", DefaultRounds,
			"$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1",
		},
		{
			"Hello world!", "saltstringsaltstring", 10000,
			"$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v.",
		},
		{
			"This is just a test", "toolongsaltstring", 5000,
			"$6$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0",
		},
		{
			"a very much longer text to encrypt.  This one even stretches over more" +
				"than one line.", "small", 1400,
			"$6$rounds=1400$anotherlongsalts$POfYwTEok97VWcjxIiSOjiykti.o/pQs.wPvMxQ6Fm7I6IoYN3CmLs66x9t0oSwbtEW7o7UmJEiDwGqd8p4ur1",
		},
	}
	for _, tc := range cases {
		// The fourth vector uses a salt longer than the string given as
		// "salt" in the spec text; parse the expected value to get the real
		// salt and rounds so the test cannot drift from the vector.
		salt, rounds, err := parse(tc.want)
		if err != nil {
			t.Fatalf("parse(%q): %v", tc.want, err)
		}
		got, err := HashWithSalt(tc.password, salt, rounds)
		if err != nil {
			t.Fatalf("HashWithSalt: %v", err)
		}
		if got != tc.want {
			t.Errorf("password %q salt %q rounds %d\n got  %s\n want %s",
				tc.password, salt, rounds, got, tc.want)
		}
	}
}

func TestSaltIsTruncatedToSixteen(t *testing.T) {
	h, err := HashWithSalt("pw", "0123456789abcdefTOOLONG", DefaultRounds)
	if err != nil {
		t.Fatalf("HashWithSalt: %v", err)
	}
	salt, _, err := parse(h)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if salt != "0123456789abcdef" {
		t.Errorf("salt should be truncated to 16 chars, got %q", salt)
	}
}

func TestRoundsAppearOnlyWhenNonDefault(t *testing.T) {
	def, err := HashWithSalt("pw", "abcd", DefaultRounds)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(def, "rounds=") {
		t.Errorf("default rounds must be implicit, got %q", def)
	}
	custom, err := HashWithSalt("pw", "abcd", 7500)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(custom, "$6$rounds=7500$abcd$") {
		t.Errorf("non-default rounds must be encoded, got %q", custom)
	}
}

func TestVerifyRoundTrips(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(h, "$6$") {
		t.Fatalf("want a $6$ hash, got %q", h)
	}
	if !Verify("correct horse battery staple", h) {
		t.Error("Verify should accept the original password")
	}
	if Verify("wrong", h) {
		t.Error("Verify must reject a wrong password")
	}
}

func TestGenerateSaltIsFreshAndInAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := GenerateSalt()
		if err != nil {
			t.Fatalf("GenerateSalt: %v", err)
		}
		if len(s) != maxSaltLen {
			t.Fatalf("salt length %d, want %d", len(s), maxSaltLen)
		}
		for _, c := range s {
			if !strings.ContainsRune(b64Alphabet, c) {
				t.Fatalf("salt %q contains %q, outside the crypt alphabet", s, c)
			}
		}
		if seen[s] {
			t.Fatalf("GenerateSalt repeated %q within 32 draws", s)
		}
		seen[s] = true
	}
}

func TestRoundsAreClamped(t *testing.T) {
	low, err := HashWithSalt("pw", "abcd", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, rounds, err := parse(low)
	if err != nil {
		t.Fatal(err)
	}
	if rounds != MinRounds {
		t.Errorf("rounds below the minimum should clamp to %d, got %d", MinRounds, rounds)
	}
}
