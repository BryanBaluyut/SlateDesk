package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// List pagination bounds; mirror the OpenAPI parameter constraints.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// escapeLike escapes ILIKE wildcards so a user-supplied q is a literal
// substring match (backslash is the default LIKE escape character).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListUsers implements GET /users (admin).
func (h *Handlers) ListUsers(w http.ResponseWriter, r *http.Request, params api.ListUsersParams) {
	arg := store.ListUsersParams{
		PageLimit:  defaultPageLimit,
		PageOffset: 0,
	}
	if params.Role != nil {
		if !params.Role.Valid() {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid role %q", *params.Role))
			return
		}
		arg.Role = store.NullUserRole{UserRole: store.UserRole(*params.Role), Valid: true}
	}
	if params.Active != nil {
		arg.Active = pgtype.Bool{Bool: *params.Active, Valid: true}
	}
	if params.Q != nil && *params.Q != "" {
		arg.Q = pgtype.Text{String: escapeLike(*params.Q), Valid: true}
	}
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > maxPageLimit {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("limit must be between 1 and %d", maxPageLimit))
			return
		}
		arg.PageLimit = int64(*params.Limit)
	}
	if params.Offset != nil {
		if *params.Offset < 0 {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "offset must be >= 0")
			return
		}
		arg.PageOffset = int64(*params.Offset)
	}

	users, err := h.q.ListUsers(r.Context(), arg)
	if err != nil {
		serverError(w, r, "list users", err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIUsers(users))
}

// CreateUser implements POST /users (admin).
func (h *Handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req api.CreateUserJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	email, err := auth.NormalizeEmail(string(req.Email))
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid email address")
		return
	}
	if !req.Role.Valid() {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid role %q", req.Role))
		return
	}

	var passwordHash pgtype.Text
	if req.Password != nil {
		if err := auth.ValidatePassword(*req.Password); err != nil {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("password must be at least %d characters", auth.MinPasswordLen))
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			serverError(w, r, "hash password", err)
			return
		}
		passwordHash = pgtype.Text{String: hash, Valid: true}
	}

	company := ""
	if req.Company != nil {
		company = *req.Company
	}

	u, err := h.q.CreateUser(r.Context(), store.CreateUserParams{
		Email:        email,
		Name:         req.Name,
		Role:         store.UserRole(req.Role),
		PasswordHash: passwordHash,
		Company:      company,
	})
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a user with this email already exists")
			return
		}
		serverError(w, r, "create user", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIUser(u))
}

// GetUser implements GET /users/{id} (admin).
func (h *Handlers) GetUser(w http.ResponseWriter, r *http.Request, id api.UserID) {
	u, err := h.q.GetUserByID(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such user")
			return
		}
		serverError(w, r, "get user", err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIUser(u))
}

// UpdateUser implements PATCH /users/{id} (admin). Omitted fields are left
// unchanged. Setting password or active=false bumps token_version so every
// outstanding session for the user dies.
func (h *Handlers) UpdateUser(w http.ResponseWriter, r *http.Request, id api.UserID) {
	var req api.UpdateUserJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != nil && !req.Role.Valid() {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid role %q", *req.Role))
		return
	}
	var passwordHash string
	if req.Password != nil {
		if err := auth.ValidatePassword(*req.Password); err != nil {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("password must be at least %d characters", auth.MinPasswordLen))
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			serverError(w, r, "hash password", err)
			return
		}
		passwordHash = hash
	}

	var updated store.User
	notFound := false
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		current, err := q.GetUserByID(r.Context(), id)
		if err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load user: %w", err)
		}

		name, role, company := current.Name, current.Role, current.Company
		if req.Name != nil {
			name = *req.Name
		}
		if req.Role != nil {
			role = store.UserRole(*req.Role)
		}
		if req.Company != nil {
			company = *req.Company
		}
		updated, err = q.UpdateUser(r.Context(), store.UpdateUserParams{
			ID: id, Name: name, Role: role, Company: company,
		})
		if err != nil {
			return fmt.Errorf("update user: %w", err)
		}

		bumpTokenVersion := false
		if req.Password != nil {
			if err := q.SetUserPassword(r.Context(), store.SetUserPasswordParams{
				ID:           id,
				PasswordHash: pgtype.Text{String: passwordHash, Valid: true},
			}); err != nil {
				return fmt.Errorf("set password: %w", err)
			}
			bumpTokenVersion = true
		}
		if req.Active != nil && *req.Active != current.Active {
			if *req.Active {
				if err := q.ActivateUser(r.Context(), id); err != nil {
					return fmt.Errorf("activate user: %w", err)
				}
			} else {
				if err := q.DeactivateUser(r.Context(), id); err != nil {
					return fmt.Errorf("deactivate user: %w", err)
				}
				bumpTokenVersion = true
			}
			updated.Active = *req.Active
		}
		if bumpTokenVersion {
			if _, err := q.BumpUserTokenVersion(r.Context(), id); err != nil {
				return fmt.Errorf("bump token_version: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		serverError(w, r, "update user", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such user")
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIUser(updated))
}

// DeactivateUser implements DELETE /users/{id} (admin): soft delete —
// active=false plus a token_version bump so outstanding sessions die.
func (h *Handlers) DeactivateUser(w http.ResponseWriter, r *http.Request, id api.UserID) {
	notFound := false
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		if _, err := q.GetUserByID(r.Context(), id); err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load user: %w", err)
		}
		if err := q.DeactivateUser(r.Context(), id); err != nil {
			return fmt.Errorf("deactivate user: %w", err)
		}
		if _, err := q.BumpUserTokenVersion(r.Context(), id); err != nil {
			return fmt.Errorf("bump token_version: %w", err)
		}
		return nil
	})
	if err != nil {
		serverError(w, r, "deactivate user", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
