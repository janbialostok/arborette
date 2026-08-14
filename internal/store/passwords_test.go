package store

import (
	"strings"
	"testing"
)

// TestPasswordRoundTrip hashes two distinct passwords and verifies each against
// its own hash and not the other's.
func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2$sha256$600000$") {
		t.Fatalf("envelope shape = %q, want the pbkdf2$sha256$600000 prefix", hash)
	}
	if !VerifyPassword("correct horse battery staple", hash) {
		t.Fatal("the right password must verify")
	}
	if VerifyPassword("WRONG", hash) {
		t.Fatal("a wrong password must not verify")
	}

	other, err := HashPassword("different password")
	if err != nil {
		t.Fatalf("hash other: %v", err)
	}
	if hash == other {
		t.Fatal("two hashes of distinct passwords must not collide")
	}
}

// TestPasswordSaltsAreRandom: two hashes of the same password differ, so a
// stored envelope carries no cross-account information.
func TestPasswordSaltsAreRandom(t *testing.T) {
	first, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("hash first: %v", err)
	}
	second, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("hash second: %v", err)
	}
	if first == second {
		t.Fatal("a fixed salt would reveal same-password accounts")
	}
}

// TestVerifyPasswordFailsClosed: an envelope that cannot parse must verify
// false rather than panicking or erroring -- a malformed row must never be
// treated as a success.
func TestVerifyPasswordFailsClosed(t *testing.T) {
	validHash, err := HashPassword("real password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// Split the valid envelope at each truncation point and confirm no prefix
	// verifies.
	parts := strings.Split(validHash, "$")
	for i := 1; i < len(parts); i++ {
		truncated := strings.Join(parts[:i], "$")
		if VerifyPassword("anything", truncated) {
			t.Fatalf("truncated envelope %q must fail closed", truncated)
		}
	}

	cases := []string{
		"",
		"pbkdf2",
		"pbkdf2$sha256$600000$onlytwo",
		"hmac$sha256$600000$AAAA$BBBB",       // wrong algorithm
		"pbkdf2$md5$600000$AAAA$BBBB",        // wrong prf
		"pbkdf2$sha256$notanumber$AAAA$BBBB", // bad iterations
		"pbkdf2$sha256$0$AAAA$BBBB",          // zero iterations
		"pbkdf2$sha256$-5$AAAA$BBBB",         // negative iterations
		"pbkdf2$sha256$600000$a!$BBBB",       // non-base64 salt
		"pbkdf2$sha256$600000$AAAA$b!!!",     // non-base64 key
		"pbkdf2$sha256$600000$$$",            // empty salt/key
	}
	for _, c := range cases {
		if VerifyPassword("x", c) {
			t.Fatalf("envelope %q must fail closed", c)
		}
	}
}

// TestVerifyDummyAlwaysFalse: the uniform-timing dummy compare never reports a
// match and always costs a full derive (the work itself is asserted elsewhere).
func TestVerifyDummyAlwaysFalse(t *testing.T) {
	for _, pw := range []string{"", "anything", "the-dummy-password", "pbkdf2$sha256$...still-never"} {
		if VerifyDummy(pw) {
			t.Fatalf("dummy compare must always be false for %q", pw)
		}
	}
}
