// Agent + admin CRUD for canned replies (M4): reusable composer snippets.
//
// Variable substitution decision (documented per M4 scope): the stored body
// keeps its RAW template. {{ticket.number}}, {{requester.name}} and
// {{requester.email}} are substituted CLIENT-SIDE at composer-insert time,
// where the ticket and requester are already loaded — the server neither
// stores nor returns an expanded body. This keeps the endpoint a plain CRUD
// surface (no ticket context needed to read a snippet) and avoids a second
// "render for ticket X" code path. See frontend/src/lib/canned.ts.
package handlers

import (
	"net/http"
	"strings"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// ListCannedReplies returns all snippets, sorted by title.
func (h *Handlers) ListCannedReplies(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListCannedReplies(r.Context())
	if err != nil {
		serverError(w, r, "list canned replies", err)
		return
	}
	out := make([]api.CannedReply, 0, len(rows))
	for _, c := range rows {
		out = append(out, cannedReplyToAPI(c))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// CreateCannedReply stores a snippet with its raw template body.
func (h *Handlers) CreateCannedReply(w http.ResponseWriter, r *http.Request) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	var req api.CreateCannedReplyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > maxNameLen {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "title must be 1–200 characters")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "body is required")
		return
	}
	row, err := h.q.CreateCannedReply(r.Context(), store.CreateCannedReplyParams{
		Title:     title,
		Body:      req.Body,
		CreatedBy: caller.ID,
	})
	if err != nil {
		serverError(w, r, "create canned reply", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, cannedReplyToAPI(row))
}

// UpdateCannedReply applies a partial update; omitted fields are unchanged.
func (h *Handlers) UpdateCannedReply(w http.ResponseWriter, r *http.Request, id api.CannedReplyID) {
	existing, err := h.q.GetCannedReply(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such canned reply")
			return
		}
		serverError(w, r, "load canned reply", err)
		return
	}
	var req api.UpdateCannedReplyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	title := existing.Title
	body := existing.Body
	if req.Title != nil {
		title = strings.TrimSpace(*req.Title)
		if title == "" || len(title) > maxNameLen {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "title must be 1–200 characters")
			return
		}
	}
	if req.Body != nil {
		if strings.TrimSpace(*req.Body) == "" {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "body must not be empty")
			return
		}
		body = *req.Body
	}
	row, err := h.q.UpdateCannedReply(r.Context(), store.UpdateCannedReplyParams{
		ID:    id,
		Title: title,
		Body:  body,
	})
	if err != nil {
		serverError(w, r, "update canned reply", err)
		return
	}
	writeJSON(w, r, http.StatusOK, cannedReplyToAPI(row))
}

// DeleteCannedReply permanently removes a snippet.
func (h *Handlers) DeleteCannedReply(w http.ResponseWriter, r *http.Request, id api.CannedReplyID) {
	n, err := h.q.DeleteCannedReply(r.Context(), id)
	if err != nil {
		serverError(w, r, "delete canned reply", err)
		return
	}
	if n == 0 {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such canned reply")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func cannedReplyToAPI(c store.CannedReply) api.CannedReply {
	return api.CannedReply{
		Id:        c.ID,
		Title:     c.Title,
		Body:      c.Body,
		CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}
