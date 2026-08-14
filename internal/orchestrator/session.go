package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/service"
	"github.com/jackc/pgx/v5"
)

// sessionCookieDefaultName is the cookie name used when the server receives no
// configured name. It must track the config default (ARBORETTE_SESSION_COOKIE ->
// "arborette_session") so the web middleware and every handler issue/read the
// same value.
const sessionCookieDefaultName = "arborette_session"

// sessionAuthScheme is the WWW-Authenticate challenge the session guard answers
// with on a 401, letting a client tell "needs a session" from "forbidden".
const sessionAuthScheme = "Session"

// sessionExemptPaths are the only analyst-facing routes that need no session:
// the sign-in surfaces (registration and login are what establish one).
var sessionExemptPaths = []string{"/register", "/login"}

// sessionGuard is the session-authentication middleware around the
// analyst-facing routes. It reads the signed-in cookie, resolves the session
// and its user, re-checks the user's active flag (deactivation takes effect on
// the next request), and stashes the acting Analyst in the request context for
// the audit-write path (which SessionIdentity reads).
//
// A nil userStore/sessionStore fails the guard open -- serve without session
// auth -- mirroring the BearerAuth empty-token fail-open discipline in
// internal/service/auth.go. Unit tests that do not exercise accounts wire no
// store and get unmetered access; production always wires the real store.
type sessionGuard struct {
	sessions sessionStore
	users    userStore
	cookie   string // empty => sessionCookieDefaultName
}

// wrap guards next: exempt routes bypass the session check, everything else
// requires a valid, active session.
func (g *sessionGuard) wrap(exempt []string, next http.Handler) http.Handler {
	skip := make(map[string]bool, len(exempt))
	for _, p := range exempt {
		skip[p] = true
	}

	if g == nil || g.sessions == nil || g.users == nil {
		// Fail open when no session store is wired (unit-test helper default).
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(g.cookieName())
		if err != nil {
			g.reject(w)
			return
		}
		sess, err := g.sessions.GetSessionByTokenHash(r.Context(), tokenHash(cookie.Value))
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("orchestrator: session lookup: %v", err)
			}
			g.reject(w)
			return
		}
		user, err := g.users.GetByID(r.Context(), sess.UserID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("orchestrator: session user lookup: %v", err)
			}
			g.reject(w)
			return
		}
		if !user.Active {
			g.reject(w)
			return
		}
		ctx := context.WithValue(r.Context(), analystKey{}, Analyst{ID: user.ID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cookieName resolves the configured cookie name, falling back to the default.
func (g *sessionGuard) cookieName() string {
	if g.cookie != "" {
		return g.cookie
	}
	return sessionCookieDefaultName
}

// reject answers the uniform 401 with the session challenge. It never reveals
// whether the cause was a missing cookie, an unknown token, or a deactivated
// account.
func (g *sessionGuard) reject(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", sessionAuthScheme)
	service.WriteErr(w, http.StatusUnauthorized, "unauthorized")
}

// analystKey is the context key for the acting Analyst stashed by the guard.
type analystKey struct{}

// AnalystsFromContext returns the Analyst the session guard stashed, and
// whether one is present. SessionIdentity reads it; a background goroutine or
// the /internal/audit path that never passed the guard gets ok=false.
func AnalystsFromContext(ctx context.Context) (Analyst, bool) {
	a, ok := ctx.Value(analystKey{}).(Analyst)
	return a, ok
}

// newMintToken returns an opaque random session token (base64url). Only its
// SHA-256 digest is stored and looked up; the raw value lives solely in the
// cookie.
func newMintToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenHash digests an opaque token for storage and lookup. The database holds
// digests, never raw tokens (the auth.go rule).
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
