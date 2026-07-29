package service

import (
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
)

// bearerScheme is the Authorization scheme the middleware accepts. RFC 9110
// defines the scheme token as case-insensitive, so it is matched that way -- a
// client that spells it "bearer" would otherwise be rejected exactly like a
// wrong token, which is near-impossible to diagnose from the outside. The token
// following it is matched exactly.
const bearerScheme = "bearer "

// BearerAuth guards a handler behind a shared bearer token, rejecting anything
// else with a 401. It is service-agnostic HTTP plumbing, so it lives beside
// RunHTTPServer rather than in any one service.
//
// An empty token disables authentication and returns the handler unwrapped: the
// local compose stack and the end-to-end flows run without minting a token. That
// fail-open leans on the same trusted-network assumption every other published
// port makes, and stops being tenable the moment the listener is reachable more
// widely.
func BearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		log.Printf("service: bearer auth disabled (no token configured) -- do NOT tunnel or publicly expose this listener in this state")
		return next
	}
	// Digests, not the raw tokens: ConstantTimeCompare returns early when the
	// lengths differ, so comparing the tokens directly would leak the configured
	// token's length to a caller guessing against a public listener. Hashing makes
	// both sides a fixed width.
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := cutBearerScheme(r.Header.Get("Authorization"))
		got := sha256.Sum256([]byte(presented))
		if !ok || subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
			// RFC 9110 requires a challenge on a 401, and without it a client
			// cannot tell "needs a token" from "forbidden".
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cutBearerScheme cuts the scheme prefix. A bare token with no scheme is not
// accepted.
func cutBearerScheme(header string) (string, bool) {
	if len(header) < len(bearerScheme) || !strings.EqualFold(header[:len(bearerScheme)], bearerScheme) {
		return "", false
	}
	return header[len(bearerScheme):], true
}
