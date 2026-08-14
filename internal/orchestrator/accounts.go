package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
	"unicode"

	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// The account/session handlers: POST /register, POST /login, POST /logout, and
// the identity bootstrap GET /me plus PATCH /me. username/password validation
// lives here (the store stores opaque envelopes and never sees a plaintext
// password); the seed and last-admin rules are enforced server-side here,
// never client-side.

// accountDTO selects the account fields the web UI renders. It is deliberately
// separate from store.User (whose password hash and time.Time fields carry no
// json tags) so the wire shape never leaks the hash envelope and the internal
// naming stays internal.
type accountDTO struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// meDTO extends the account shape with the one-shot admin-grant notice flag the
// /me bootstrap and the chip read. AdminNotice is true exactly when a seeded
// admin hasn't dismissed the notice yet (role=admin and the ack flag false).
type meDTO struct {
	accountDTO
	AdminNotice bool `json:"admin_notice"`
}

// toAccountDTO copies a store user to the wire shape.
func toAccountDTO(u store.User) accountDTO {
	return accountDTO{
		ID:        u.ID,
		Username:  u.Username,
		Role:      u.Role,
		Active:    u.Active,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

// toMeDTO computes the admin-notice flag from the current row: only an
// unacknowledged admin carries it.
func toMeDTO(u store.User) meDTO {
	return meDTO{
		accountDTO:  toAccountDTO(u),
		AdminNotice: u.Role == store.RoleAdmin && !u.AdminNoticeAcknowledged,
	}
}

// credentialsRequest is the shared register/login body.
type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// validateUsername enforces the account-name shape (3-32 chars, letters,
// digits, underscore, hyphen) on the shared path so register, rename, and
// nothing else can mint a divergent rule.
func validateUsername(username string) string {
	if len([]rune(username)) < 3 || len([]rune(username)) > 32 {
		return "username must be 3-32 characters"
	}
	for _, r := range username {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return "username must not contain spaces"
		}
		return "username may only contain letters, digits, _, and -"
	}
	return ""
}

// validatePassword enforces the shared password floor (>= 8 characters).
func validatePassword(password string) string {
	if len(password) < 8 {
		return "password must be at least 8 characters"
	}
	return ""
}

// signIn is the shared register/login tail: it mints a session, persists its
// token digest, and issues the session cookie. The cookie carries no plaintext
// credential and holds only the opaque token whose digest the sessions table
// stores. status is 201 for a registration and 200 for a login.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, user store.User, status int) {
	token, err := newMintToken()
	if err != nil {
		log.Printf("orchestrator: mint session token: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := s.sessions.CreateSession(r.Context(), tokenHash(token), user.ID); err != nil {
		log.Printf("orchestrator: create session: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.setSessionCookie(w, token)
	service.WriteJSON(w, status, toMeDTO(user))
}

// setSessionCookie issues the signed-in cookie with the session's security
// defaults: HttpOnly (no JS reads it), SameSite=Lax (CSRF-safe for same-site
// fetches), Path=/ (every route). Secure is applied only when configured (off
// under local plain-HTTP compose).
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure,
	})
}

// clearSessionCookie expires the session cookie so the browser drops it even if
// the session row delete raced a concurrent request.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

// meRequest is the PATCH /me body. It carries the self-service account changes:
// the username rename, the password change (which requires the current password
// to be verified against the stored hash), and the one-shot admin-grant notice
// acknowledgement. All fields are optional; a body that names none of them is a
// 400 so a partial client cannot silently no-op.
type meRequest struct {
	Username               *string `json:"username"`
	Password               *string `json:"password"`
	CurrentPassword        *string `json:"current_password"`
	AcknowledgeAdminNotice *bool   `json:"acknowledge_admin_notice"`
}

// handleMe returns the acting analyst's identity bootstrap. The session guard
// has already resolved the cookie to a user id (AnalystsFromContext); the
// handler re-reads the row so the response reflects any change since the
// request began. Every signed-in page and the corner chip read this.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	analyst, ok := AnalystsFromContext(r.Context())
	if !ok {
		service.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, err := s.users.GetByID(r.Context(), analyst.ID)
	if err != nil {
		log.Printf("orchestrator: get me: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	service.WriteJSON(w, http.StatusOK, toMeDTO(user))
}

// handlePatchMe applies a self-service account change: rename the username,
// change the password (current password required), or acknowledge the admin-grant
// notice. It is the acting analyst's own row -- the identity comes from the
// session context, so an attacker cannot target another user's id here.
//
// The username rename validates the same shape registration enforces and is
// unique-checked (409 on a clash). A password change verifies CurrentPassword
// against the stored hash and refuses a wrong one with 403, producing the
// uniform "current password is incorrect" reason. The session is NOT rotated
// (it keys on the user id, and the cookie's token survives a rename/password
// change -- only the credential used to prove identity next time changes).
// Renames and password changes are audited (R6); the notice acknowledgement is
// an acknowledgment, not a privileged mutation, and is not.
func (s *Server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	analyst, ok := AnalystsFromContext(r.Context())
	if !ok {
		service.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req meRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == nil && req.Password == nil && req.AcknowledgeAdminNotice == nil {
		service.WriteErr(w, http.StatusBadRequest, "nothing to update")
		return
	}

	updates := store.UserUpdate{}
	if req.Username != nil {
		if msg := validateUsername(*req.Username); msg != "" {
			service.WriteErr(w, http.StatusBadRequest, msg)
			return
		}
		updates.Username = req.Username
	}
	if req.Password != nil {
		if msg := validatePassword(*req.Password); msg != "" {
			service.WriteErr(w, http.StatusBadRequest, msg)
			return
		}
		// A password change requires the current password verified against the
		// stored hash; a wrong one is refused (403) and the update is not applied.
		if req.CurrentPassword == nil || !store.VerifyPassword(*req.CurrentPassword, s.mustCurrentPassword(r.Context(), analyst.ID)) {
			service.WriteErr(w, http.StatusForbidden, "current password is incorrect")
			return
		}
		hash, err := store.HashPassword(*req.Password)
		if err != nil {
			log.Printf("orchestrator: hash new password: %v", err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		updates.PasswordHash = &hash
	}
	if req.AcknowledgeAdminNotice != nil {
		updates.AdminNoticeAcknowledged = req.AcknowledgeAdminNotice
	}

	// A rename is unique-checked by the store (409), never by the handler. The
	// pre-update row is fetched only when renaming, to audit the previous name.
	var previousUsername string
	if req.Username != nil {
		if u, err := s.users.GetByID(r.Context(), analyst.ID); err == nil {
			previousUsername = u.Username
		}
	}
	user, err := s.users.Update(r.Context(), analyst.ID, updates)
	if err != nil {
		if errors.Is(err, store.ErrUsernameConflict) {
			service.WriteErr(w, http.StatusConflict, "username is already in use")
			return
		}
		log.Printf("orchestrator: update me: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if req.Username != nil {
		// previous_username is part of the audit vocabulary (FR-008): rename
		// records the old name and the new one. previous_username may be empty
		// for a not-yet-seen row, which is still a valid audit value.
		if err := s.recordAudit(r.Context(), "user_username_change", "user", map[string]any{
			"user_id": user.ID, "username": user.Username, "previous_username": previousUsername,
		}); err != nil {
			log.Printf("orchestrator: audit username change: %v", err)
		}
	}
	if req.Password != nil {
		if err := s.recordAudit(r.Context(), "user_password_change", "user", map[string]any{
			"user_id": user.ID, "username": user.Username,
		}); err != nil {
			log.Printf("orchestrator: audit password change: %v", err)
		}
	}

	service.WriteJSON(w, http.StatusOK, toMeDTO(user))
}

// mustCurrentPassword re-reads the acting analyst's row to load the stored hash
// for a password-change check. A missing row is treated as an empty hash (the
// verify fails) so the 403 path is uniform.
func (s *Server) mustCurrentPassword(ctx context.Context, id string) string {
	u, err := s.users.GetByID(ctx, id)
	if err != nil {
		return ""
	}
	return u.PasswordHash
}

// handleRegister creates an account and signs it in. The seed decision follows
// CountActiveAdmins (the store re-derives the role under its advisory lock, so
// a concurrent first registration cannot double-seed), and the seeded admin
// path carries the one-shot grant notice. A registration never leaks whether it
// hit the username index or any other write path -- the 409 names the clash.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if msg := validateUsername(req.Username); msg != "" {
		service.WriteErr(w, http.StatusBadRequest, msg)
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		service.WriteErr(w, http.StatusBadRequest, msg)
		return
	}

	n, err := s.users.CountActiveAdmins(r.Context())
	if err != nil {
		log.Printf("orchestrator: count active admins: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	hash, err := store.HashPassword(req.Password)
	if err != nil {
		log.Printf("orchestrator: hash password: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	user, err := s.users.Create(r.Context(), req.Username, hash, n == 0)
	if err != nil {
		if errors.Is(err, store.ErrUsernameConflict) {
			service.WriteErr(w, http.StatusConflict, "username is already in use")
			return
		}
		log.Printf("orchestrator: create user: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.recordAudit(r.Context(), "user_register", "user", map[string]any{
		"user_id":  user.ID,
		"username": user.Username,
		"role":     user.Role,
	}); err != nil {
		log.Printf("orchestrator: audit user register: %v", err)
	}

	s.signIn(w, r, user, http.StatusCreated)
}

// handleLogin verifies credentials and signs the user in. The failure path is a
// single generic 401 whose compare costs the same whether the username exists
// (wrong password), does not (missing row, dummy compare), or the account is
// deactivated (dummy compare) -- FR-004 forbids revealing which. Sign-in is a
// session transition and deliberately not audited.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" {
		// Keep the envelope identical to a wrong-password attempt: an empty
		// username is a failed login, not a validation fault.
		store.VerifyDummy(req.Password)
		service.WriteErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	user, err := s.users.GetByUsername(r.Context(), req.Username)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("orchestrator: get user by username: %v", err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		// Unknown username: pay the full PBKDF2 cost against the dummy envelope
		// so total response time matches a wrong-password on a real account.
		store.VerifyDummy(req.Password)
		service.WriteErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if !user.Active {
		store.VerifyDummy(req.Password)
		service.WriteErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if !store.VerifyPassword(req.Password, user.PasswordHash) {
		service.WriteErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	s.signIn(w, r, user, http.StatusOK)
}

// handleLogout ends the session: the row is deleted by the cookie's token
// digest and the cookie itself is cleared. It is idempotent -- a cookie whose
// session already died (repeat logout, server restart) is still cleared with a
// 204. Logout is a session transition and deliberately not audited.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cookieName); err == nil {
		if err := s.sessions.DeleteSessionByTokenHash(r.Context(), tokenHash(cookie.Value)); err != nil {
			log.Printf("orchestrator: delete session: %v", err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
