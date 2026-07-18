package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// toAPITag converts a store tag row (citext name scans as string).
func toAPITag(t store.Tag) api.Tag {
	out := api.Tag{Id: t.ID, Name: t.Name}
	if t.Color.Valid {
		color := t.Color.String
		out.Color = &color
	}
	return out
}

// toAPITags converts a slice, mapping nil to an empty (JSON []) slice.
func toAPITags(tags []store.Tag) []api.Tag {
	out := make([]api.Tag, 0, len(tags))
	for _, t := range tags {
		out = append(out, toAPITag(t))
	}
	return out
}

// ListTags implements GET /tags (agent/admin): all tags sorted by name.
func (h *Handlers) ListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.q.ListTags(r.Context())
	if err != nil {
		serverError(w, r, "list tags", err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPITags(tags))
}

// CreateTag implements POST /tags (agent/admin). Names are
// case-insensitively unique (citext).
func (h *Handlers) CreateTag(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTagJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name is required")
		return
	}
	var color pgtype.Text
	if req.Color != nil {
		color = pgtype.Text{String: *req.Color, Valid: true}
	}

	t, err := h.q.CreateTag(r.Context(), store.CreateTagParams{Name: name, Color: color})
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a tag with this name already exists")
			return
		}
		serverError(w, r, "create tag", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPITag(t))
}

// UpdateTag implements PATCH /tags/{id} (agent/admin). Omitted fields are
// unchanged; explicit `color: null` clears the color; name cannot be null.
func (h *Handlers) UpdateTag(w http.ResponseWriter, r *http.Request, id api.TagID) {
	fields, ok := decodeJSONFields(w, r, "name", "color")
	if !ok {
		return
	}
	var newName *string
	if raw, present := fields["name"]; present {
		var s *string
		if err := json.Unmarshal(raw, &s); err != nil || s == nil || strings.TrimSpace(*s) == "" {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name must be a non-empty string")
			return
		}
		trimmed := strings.TrimSpace(*s)
		newName = &trimmed
	}
	colorSet := false
	var newColor *string
	if raw, present := fields["color"]; present {
		if err := json.Unmarshal(raw, &newColor); err != nil {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "color must be a string or null")
			return
		}
		colorSet = true
	}

	var (
		updated  store.Tag
		notFound bool
	)
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		current, err := q.GetTag(r.Context(), id)
		if err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load tag: %w", err)
		}
		name, color := current.Name, current.Color
		if newName != nil {
			name = *newName
		}
		if colorSet {
			if newColor == nil {
				color = pgtype.Text{}
			} else {
				color = pgtype.Text{String: *newColor, Valid: true}
			}
		}
		updated, err = q.UpdateTag(r.Context(), store.UpdateTagParams{ID: id, Name: name, Color: color})
		if err != nil {
			return fmt.Errorf("update tag: %w", err)
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a tag with this name already exists")
			return
		}
		serverError(w, r, "update tag", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such tag")
		return
	}
	writeJSON(w, r, http.StatusOK, toAPITag(updated))
}

// DeleteTag implements DELETE /tags/{id} (admin only, enforced by
// authPolicy). Hard delete; ticket_tags rows cascade.
func (h *Handlers) DeleteTag(w http.ResponseWriter, r *http.Request, id api.TagID) {
	notFound := false
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		if _, err := q.GetTag(r.Context(), id); err != nil {
			if isNoRows(err) {
				notFound = true
				return nil
			}
			return fmt.Errorf("load tag: %w", err)
		}
		if err := q.DeleteTag(r.Context(), id); err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}
		return nil
	})
	if err != nil {
		serverError(w, r, "delete tag", err)
		return
	}
	if notFound {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such tag")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
