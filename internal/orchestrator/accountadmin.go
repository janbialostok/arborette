package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// The admin account surface: GET/POST /users and PATCH /users/{id}. Membership
// is enforced here (a session must resolve to an active admin), not in the web
// UI which only hides the entry. The last-active-admin guard (FR-013) is the
// one mutation rule unique to this surface: a change that would leave zero
// active admins is refused with 409 before any write.

// UserAdminDto is the roster row shape the admin list renders. fields align
// with accountDTO; the role is the single extensible value and active reflects
// whether the account's next session works.
type UserAdminDto struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// createUserRequest is the admin create-member body. Admin-created accounts are
// always active, role=member (only the seed path ever grants admin).
type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// updateUserRequest is the PATCH /users/{id} body; at least one field is
// required. role promotes/demotes, active deactivates/reactivates, and
// new_password resets without knowing the current one.
type updateUserRequest struct {
	Role        *string `json:"role"`
	Active      *bool   `json:"active"`
	NewPassword *string `json:"new_password"`
}

// adminRequired extracts the acting analyst and enforces the admin role against
// the live row. A non-admin (or absent) actor gets 403; the server is the only
// authority (FR-014), never the UI.
func (s *Server) adminRequired(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	analyst, ok := AnalystsFromContext(r.Context())
	if !ok {
		service.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return store.User{}, false
	}
	me, err := s.users.GetByID(r.Context(), analyst.ID)
	if err != nil {
		log.Printf("orchestrator: admin gate lookup: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return store.User{}, false
	}
	if me.Role != store.RoleAdmin || !me.Active {
		service.WriteErr(w, http.StatusForbidden, "admin access required")
		return store.User{}, false
	}
	return me, true
}

// ensureActiveAdminRule checks that the pending mutation would not leave zero
// active admins. It is a pre-write guard (FR-013): for the target account it
// predicts its resulting admin/active state and refuses a demote or deactivate
// that, added to the current count, would drop the total to zero.
func (s *Server) ensureActiveAdminRule(ctx context.Context, target store.User, pendingRole *string, pendingActive *bool) error {
	newRole := target.Role
	if pendingRole != nil {
		newRole = *pendingRole
	}
	newActive := target.Active
	if pendingActive != nil {
		newActive = *pendingActive
	}
	// The change only risks "last admin removed" when the target currently is an
	// active admin and the pending change makes it not one.
	if !(target.Role == store.RoleAdmin && target.Active && (newRole != store.RoleAdmin || !newActive)) {
		return nil
	}

	// Count every currently-active admin other than the target; if none remain
	// after removing this one, the change is refused.
	total, err := s.users.CountActiveAdmins(ctx)
	if err != nil {
		return err
	}
	if total <= 1 {
		return errLastAdminRemoval
	}
	return nil
}

var errLastAdminRemoval = &lastAdminRemovalError{}

type lastAdminRemovalError struct{}

func (*lastAdminRemovalError) Error() string { return "the last active admin cannot be removed" }

// handleListUsers returns the full account roster to an admin.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminRequired(w, r); !ok {
		return
	}
	users, err := s.users.List(r.Context())
	if err != nil {
		log.Printf("orchestrator: list users: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]UserAdminDto, 0, len(users))
	for _, u := range users {
		out = append(out, toUserAdminDTO(u))
	}
	service.WriteJSON(w, http.StatusOK, out)
}

// handleCreateUser creates a member account (always active, role=member) on
// behalf of an admin. The new account can sign in immediately.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminRequired(w, r); !ok {
		return
	}
	var req createUserRequest
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
	hash, err := store.HashPassword(req.Password)
	if err != nil {
		log.Printf("orchestrator: hash admin-created password: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Admin-created accounts are never the seed: always seedAdmin=false.
	user, err := s.users.Create(r.Context(), req.Username, hash, false)
	if err != nil {
		if errors.Is(err, store.ErrUsernameConflict) {
			service.WriteErr(w, http.StatusConflict, "username is already in use")
			return
		}
		log.Printf("orchestrator: create user (admin): %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	service.WriteJSON(w, http.StatusCreated, toUserAdminDTO(user))
}

// handleUpdateUser applies an admin mutation to an account: promote/demote,
// deactivate/reactivate, or reset the password. It runs the last-admin guard
// before any write and audits role/status changes and password resets.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminRequired(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Role == nil && req.Active == nil && req.NewPassword == nil {
		service.WriteErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if req.Role != nil && *req.Role != store.RoleAdmin && *req.Role != store.RoleMember {
		service.WriteErr(w, http.StatusBadRequest, "role must be admin or member")
		return
	}

	target, err := s.users.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			service.WriteErr(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("orchestrator: get target user %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.ensureActiveAdminRule(r.Context(), target, req.Role, req.Active); err != nil {
		if errors.Is(err, errLastAdminRemoval) {
			service.WriteErr(w, http.StatusConflict, errLastAdminRemoval.Error())
			return
		}
		log.Printf("orchestrator: last-admin guard: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	updates := store.UserUpdate{
		Role:   req.Role,
		Active: req.Active,
	}
	if req.NewPassword != nil {
		if msg := validatePassword(*req.NewPassword); msg != "" {
			service.WriteErr(w, http.StatusBadRequest, msg)
			return
		}
		hash, err := store.HashPassword(*req.NewPassword)
		if err != nil {
			log.Printf("orchestrator: hash reset password: %v", err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		updates.PasswordHash = &hash
	}

	updated, err := s.users.Update(r.Context(), id, updates)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			service.WriteErr(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("orchestrator: update user %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if req.Role != nil && *req.Role != target.Role {
		recordAdminAudit(s, r.Context(), "user_role_change", updated, map[string]any{
			"user_id": updated.ID, "username": updated.Username, "role": updated.Role,
		})
	}
	if req.Active != nil && *req.Active != target.Active {
		recordAdminAudit(s, r.Context(), "user_status_change", updated, map[string]any{
			"user_id": updated.ID, "username": updated.Username, "active": updated.Active,
		})
	}
	if req.NewPassword != nil {
		recordAdminAudit(s, r.Context(), "user_password_reset", updated, map[string]any{
			"user_id": updated.ID, "username": updated.Username,
		})
	}

	service.WriteJSON(w, http.StatusOK, toUserAdminDTO(updated))
}

// recordAdminAudit writes a user-management audit with the acting session
// stamped as the Actor (the identity.Current resolution in audit.go handles
// that from the request context the session guard set).
func recordAdminAudit(s *Server, ctx context.Context, action string, u store.User, detail map[string]any) {
	if err := s.recordAudit(ctx, action, "user", detail); err != nil {
		log.Printf("orchestrator: audit %s: %v", action, err)
	}
}

// toUserAdminDTO maps a store user to the admin roster wire shape (no hash, no
// notice flag: it is the roster, not the /me bootstrap).
func toUserAdminDTO(u store.User) UserAdminDto {
	return UserAdminDto{
		ID:        u.ID,
		Username:  u.Username,
		Role:      u.Role,
		Active:    u.Active,
		CreatedAt: u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: u.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
