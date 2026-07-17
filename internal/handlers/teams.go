package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// ListTeams implements GET /teams (admin).
func (h *Handlers) ListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := h.q.ListTeams(r.Context())
	if err != nil {
		serverError(w, r, "list teams", err)
		return
	}
	out := make([]api.Team, 0, len(teams))
	for _, t := range teams {
		out = append(out, toAPITeam(t))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// CreateTeam implements POST /teams (admin).
func (h *Handlers) CreateTeam(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTeamJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name is required")
		return
	}
	description := ""
	if req.Description != nil {
		description = *req.Description
	}

	t, err := h.q.CreateTeam(r.Context(), store.CreateTeamParams{
		Name:        req.Name,
		Description: description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a team with this name already exists")
			return
		}
		serverError(w, r, "create team", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPITeam(t))
}

// GetTeam implements GET /teams/{id} (admin): the team with its members.
func (h *Handlers) GetTeam(w http.ResponseWriter, r *http.Request, id api.TeamID) {
	t, err := h.q.GetTeamByID(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such team")
			return
		}
		serverError(w, r, "get team", err)
		return
	}
	members, err := h.q.ListTeamMembers(r.Context(), id)
	if err != nil {
		serverError(w, r, "list team members", err)
		return
	}
	writeJSON(w, r, http.StatusOK, api.TeamWithMembers{
		Id:          t.ID,
		Name:        t.Name,
		Description: t.Description,
		Members:     toAPIUsers(members),
	})
}

// UpdateTeam implements PATCH /teams/{id} (admin). Omitted fields are left
// unchanged.
func (h *Handlers) UpdateTeam(w http.ResponseWriter, r *http.Request, id api.TeamID) {
	var req api.UpdateTeamJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil && *req.Name == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name must not be empty")
		return
	}

	var updated store.Team
	notFound := false
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		current, err := q.GetTeamByID(r.Context(), id)
		if err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load team: %w", err)
		}
		name, description := current.Name, current.Description
		if req.Name != nil {
			name = *req.Name
		}
		if req.Description != nil {
			description = *req.Description
		}
		updated, err = q.UpdateTeam(r.Context(), store.UpdateTeamParams{
			ID: id, Name: name, Description: description,
		})
		if err != nil {
			return fmt.Errorf("update team: %w", err)
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a team with this name already exists")
			return
		}
		serverError(w, r, "update team", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such team")
		return
	}
	writeJSON(w, r, http.StatusOK, toAPITeam(updated))
}

// DeleteTeam implements DELETE /teams/{id} (admin). Hard delete; membership
// rows cascade.
func (h *Handlers) DeleteTeam(w http.ResponseWriter, r *http.Request, id api.TeamID) {
	notFound := false
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		if _, err := q.GetTeamByID(r.Context(), id); err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load team: %w", err)
		}
		if err := q.DeleteTeam(r.Context(), id); err != nil {
			return fmt.Errorf("delete team: %w", err)
		}
		return nil
	})
	if err != nil {
		serverError(w, r, "delete team", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such team")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetTeamMembers implements PUT /teams/{id}/members (admin): replaces the
// full membership list atomically. Unknown user ids fail the whole request
// with 400; nothing is applied partially.
func (h *Handlers) SetTeamMembers(w http.ResponseWriter, r *http.Request, id api.TeamID) {
	var req api.SetTeamMembersJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UserIds == nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "user_ids is required (may be empty)")
		return
	}

	var (
		team     store.Team
		members  []store.User
		notFound bool
		unknown  []string
	)
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		var err error
		team, err = q.GetTeamByID(r.Context(), id)
		if err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load team: %w", err)
		}

		seen := make(map[uuid.UUID]bool, len(req.UserIds))
		for _, uid := range req.UserIds {
			if seen[uid] {
				continue
			}
			seen[uid] = true
			if _, err := q.GetUserByID(r.Context(), uid); err != nil {
				if isNoRows(err) {
					unknown = append(unknown, uid.String())
					continue
				}
				return fmt.Errorf("check user %s: %w", uid, err)
			}
		}
		if len(unknown) > 0 {
			// Abort the transaction; the caller maps this sentinel to 400.
			return errUnknownUsers
		}

		if err := q.RemoveAllTeamMembers(r.Context(), id); err != nil {
			return fmt.Errorf("clear members: %w", err)
		}
		for uid := range seen {
			if err := q.AddTeamMember(r.Context(), store.AddTeamMemberParams{
				TeamID: id, UserID: uid,
			}); err != nil {
				return fmt.Errorf("add member %s: %w", uid, err)
			}
		}
		members, err = q.ListTeamMembers(r.Context(), id)
		if err != nil {
			return fmt.Errorf("list members: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errUnknownUsers) {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request",
				"unknown user ids: "+strings.Join(unknown, ", "))
			return
		}
		serverError(w, r, "set team members", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such team")
		return
	}
	writeJSON(w, r, http.StatusOK, api.TeamWithMembers{
		Id:          team.ID,
		Name:        team.Name,
		Description: team.Description,
		Members:     toAPIUsers(members),
	})
}

// errUnknownUsers aborts the SetTeamMembers transaction when the request
// references user ids that do not exist.
var errUnknownUsers = errors.New("handlers: unknown user ids")
