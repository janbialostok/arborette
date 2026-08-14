package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing is self-describing PBKDF2-HMAC-SHA256 over the stdlib
// crypto/pbkdf2 (Go 1.24), so there is no algorithm-switch migration to run
// when parameters change and no dependency to pin. Schema:
//
//	pbkdf2$sha256$<iterations>$<saltB64>$<keyB64>
//
// The envelope records its own parameters, so iterations can be raised without
// invalidating rows hashed at an earlier cost -- the implementation derived by
// research R5 and mandated by the data-model.
const (
	pbkdf2Algorithm  = "pbkdf2"
	pbkdf2PRF        = "sha256"
	pbkdf2Iterations = 600_000
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 32
)

// dummyPasswordHash is a genuine envelope of a fixed non-secret string, hashed
// at the same iteration count as real rows. The login path verifies against it
// for a username that does not exist (or whose account is deactivated), so an
// unknown-username attempt costs the same PBKDF2 work as a wrong password on a
// real account -- nobody can tell which branch ran by timing (FR-004, R5).
//
// It is baked at build time rather than derived on first login so the cost is
// paid by package init on every process start, never by one unlucky request.
const dummyPasswordHash = "pbkdf2$sha256$600000$z36/ooimyJDudpRGSShm1w$LZBZcysClV5Ba79jA7isvPzYXEbamoLrCvyJtyVC9Mk"

// HashPassword derives a PBKDF2 key from the password with a fresh salt and
// returns its self-describing envelope. The salt is random per call, so two
// hashes of the same password never collide.
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	if err != nil {
		return "", fmt.Errorf("derive password key: %w", err)
	}
	return encodeEnvelope("sha256", pbkdf2Iterations, salt, key), nil
}

// VerifyPassword checks a password against a stored envelope in constant time,
// failing closed on any envelope shape it cannot parse. An unparseable envelope
// must never be treated as "not a match" that then records a successful login.
func VerifyPassword(password, envelope string) bool {
	prf, iterations, salt, key, ok := parseEnvelope(envelope)
	if !ok {
		return false
	}
	if prf != pbkdf2PRF {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, key) == 1
}

// VerifyDummy is the uniform-timing compare for an unknown or deactivated
// username: it always runs the full PBKDF2 cost against the dummy envelope and
// always returns false, so login response times do not disclose whether a
// username exists.
func VerifyDummy(password string) bool {
	return VerifyPassword(password, dummyPasswordHash)
}

// encodeEnvelope renders the self-describing envelope base64url-short (no
// padding). base64url so the '$'-free alphabet keeps the envelope awk/cexpr
// friendly; the Raw variant avoids the padding '=' that would terminate the
// split when parsing.
func encodeEnvelope(prf string, iterations int, salt, key []byte) string {
	return pbkdf2Algorithm + "$" + prf + "$" + strconv.Itoa(iterations) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)
}

// parseEnvelope splits a self-describing envelope, decoding the salt and key.
// It fails closed on any structural deviation: wrong field count, a non-numeric
// or non-positive iteration count, or non-decodable base64. An oversized
// iteration count is bounded so a hostile envelope cannot force a multi-minute
// derive.
func parseEnvelope(envelope string) (string, int, []byte, []byte, bool) {
	parts := strings.Split(envelope, "$")
	if len(parts) != 5 || parts[0] != pbkdf2Algorithm {
		return "", 0, nil, nil, false
	}
	iterations, err := strconv.Atoi(parts[2])
	if err != nil || iterations < 1 || iterations > 20_000_000 {
		return "", 0, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) == 0 {
		return "", 0, nil, nil, false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(key) == 0 {
		return "", 0, nil, nil, false
	}
	return parts[1], iterations, salt, key, true
}
