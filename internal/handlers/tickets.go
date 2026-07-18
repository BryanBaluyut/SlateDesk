package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// nonClosedStatuses is the status set behind the `my`, `unassigned`, and
// `open` fixed views.
var nonClosedStatuses = []string{
	string(store.TicketStatusOpen),
	string(store.TicketStatusWaitingOnCustomer),
	string(store.TicketStatusOnHold),
}

// writeTicketServiceError maps ticket.Service errors onto problem responses:
// ErrValidation -> 400 (the message is caller-input feedback, safe to
// expose), ErrNotFound -> 404, anything else -> logged 500.
func writeTicketServiceError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, ticket.ErrValidation):
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
	case errors.Is(err, ticket.ErrNotFound):
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such ticket")
	default:
		serverError(w, r, what, err)
	}
}

// toAPITicket assembles the API ticket from the store row plus the joined
// requester/assignee display columns.
func toAPITicket(t store.Ticket, requesterName, requesterEmail, assigneeName string, assigneeEmail any) api.Ticket {
	out := api.Ticket{
		Id:       t.ID,
		Number:   t.Number,
		Subject:  t.Subject,
		Status:   api.TicketStatus(t.Status),
		Priority: api.TicketPriority(t.Priority),
		Requester: api.UserSummary{
			Id:    t.RequesterID,
			Name:  requesterName,
			Email: openapi_types.Email(requesterEmail),
		},
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
	if t.AssigneeID.Valid {
		id := uuid.UUID(t.AssigneeID.Bytes)
		out.Assignee = &api.UserSummary{
			Id:    id,
			Name:  assigneeName,
			Email: openapi_types.Email(asString(assigneeEmail)),
		}
	}
	if t.TeamID.Valid {
		id := uuid.UUID(t.TeamID.Bytes)
		out.TeamId = &id
	}
	if t.ClosedAt.Valid {
		ts := t.ClosedAt.Time
		out.ClosedAt = &ts
	}
	return out
}

// toAPITicketEvent converts an audit row; the jsonb payload becomes a map.
func toAPITicketEvent(ev store.TicketEvent) (api.TicketEvent, error) {
	payload := map[string]interface{}{}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return api.TicketEvent{}, fmt.Errorf("decode event %d payload: %w", ev.ID, err)
		}
	}
	out := api.TicketEvent{
		Id:        ev.ID,
		TicketId:  ev.TicketID,
		Type:      ev.Type,
		Payload:   payload,
		CreatedAt: ev.CreatedAt,
	}
	if ev.ActorID.Valid {
		id := uuid.UUID(ev.ActorID.Bytes)
		out.ActorId = &id
	}
	return out, nil
}

// ticketDetail loads the full TicketDetail (ticket + tags + articles with
// attachments + events). found is false when the ticket does not exist.
func (h *Handlers) ticketDetail(ctx context.Context, id uuid.UUID) (api.TicketDetail, bool, error) {
	row, err := h.q.GetTicket(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return api.TicketDetail{}, false, nil
		}
		return api.TicketDetail{}, false, fmt.Errorf("load ticket: %w", err)
	}
	base := toAPITicket(row.Ticket, row.RequesterName, row.RequesterEmail, row.AssigneeName, row.AssigneeEmail)

	tags, err := h.q.ListTagsForTicket(ctx, id)
	if err != nil {
		return api.TicketDetail{}, false, fmt.Errorf("load ticket tags: %w", err)
	}
	articleRows, err := h.q.ListArticlesByTicket(ctx, id)
	if err != nil {
		return api.TicketDetail{}, false, fmt.Errorf("load articles: %w", err)
	}
	attachments, err := h.q.ListAttachmentsForTicket(ctx, id)
	if err != nil {
		return api.TicketDetail{}, false, fmt.Errorf("load attachments: %w", err)
	}
	eventRows, err := h.q.ListTicketEvents(ctx, id)
	if err != nil {
		return api.TicketDetail{}, false, fmt.Errorf("load events: %w", err)
	}

	attachmentsByArticle := make(map[uuid.UUID][]api.Attachment, len(articleRows))
	for _, att := range attachments {
		attachmentsByArticle[att.ArticleID] = append(attachmentsByArticle[att.ArticleID], toAPIAttachment(att))
	}

	articles := make([]api.Article, 0, len(articleRows))
	for _, ar := range articleRows {
		atts := attachmentsByArticle[ar.Article.ID]
		if atts == nil {
			atts = []api.Attachment{}
		}
		articles = append(articles, toAPIArticle(ar.Article, ar.AuthorName, asString(ar.AuthorEmail), atts))
	}

	events := make([]api.TicketEvent, 0, len(eventRows))
	for _, ev := range eventRows {
		apiEv, err := toAPITicketEvent(ev)
		if err != nil {
			return api.TicketDetail{}, false, err
		}
		events = append(events, apiEv)
	}

	return api.TicketDetail{
		Id:        base.Id,
		Number:    base.Number,
		Subject:   base.Subject,
		Status:    base.Status,
		Priority:  base.Priority,
		Requester: base.Requester,
		Assignee:  base.Assignee,
		TeamId:    base.TeamId,
		CreatedAt: base.CreatedAt,
		UpdatedAt: base.UpdatedAt,
		ClosedAt:  base.ClosedAt,
		Tags:      toAPITags(tags),
		Articles:  articles,
		Events:    events,
	}, true, nil
}

// ListTickets implements GET /tickets (agent/admin): fixed views, filters,
// FTS, and pagination with a pre-LIMIT total.
func (h *Handlers) ListTickets(w http.ResponseWriter, r *http.Request, params api.ListTicketsParams) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}

	arg := store.ListTicketsParams{PageLimit: defaultPageLimit}
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

	emptyPage := func() {
		writeJSON(w, r, http.StatusOK, api.TicketList{Items: []api.Ticket{}, Total: 0})
	}

	// View resolution: the view fixes a status set (and, for my/unassigned,
	// an assignee constraint); every explicit filter then ANDs with it.
	var viewStatuses []string // nil = all statuses
	if params.View != nil {
		switch *params.View {
		case api.TicketViewMy:
			viewStatuses = nonClosedStatuses
			if params.AssigneeId != nil && *params.AssigneeId != caller.ID {
				// "my" pins the assignee to the caller; a conflicting
				// explicit assignee filter can never match.
				emptyPage()
				return
			}
			arg.AssigneeID = pgtype.UUID{Bytes: caller.ID, Valid: true}
		case api.TicketViewUnassigned:
			viewStatuses = nonClosedStatuses
			arg.Unassigned = pgtype.Bool{Bool: true, Valid: true}
		case api.TicketViewOpen:
			viewStatuses = nonClosedStatuses
		case api.TicketViewClosed:
			viewStatuses = []string{string(store.TicketStatusClosed)}
		default:
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid view %q", *params.View))
			return
		}
	}

	statuses := viewStatuses
	if params.Status != nil && len(*params.Status) > 0 {
		requested := make([]string, 0, len(*params.Status))
		for _, s := range *params.Status {
			if !s.Valid() {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid status %q", s))
				return
			}
			requested = append(requested, string(s))
		}
		if viewStatuses == nil {
			statuses = requested
		} else {
			statuses = intersect(viewStatuses, requested)
			if len(statuses) == 0 {
				emptyPage()
				return
			}
		}
	}
	arg.Statuses = statuses

	if params.Priority != nil && len(*params.Priority) > 0 {
		priorities := make([]string, 0, len(*params.Priority))
		for _, p := range *params.Priority {
			if !p.Valid() {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid priority %q", p))
				return
			}
			priorities = append(priorities, string(p))
		}
		arg.Priorities = priorities
	}

	if params.AssigneeId != nil && !arg.AssigneeID.Valid {
		arg.AssigneeID = pgtype.UUID{Bytes: *params.AssigneeId, Valid: true}
	}
	if params.TeamId != nil {
		arg.TeamID = pgtype.UUID{Bytes: *params.TeamId, Valid: true}
	}
	if params.TagId != nil {
		arg.TagID = pgtype.UUID{Bytes: *params.TagId, Valid: true}
	}
	if params.ClosedToday != nil && *params.ClosedToday {
		arg.ClosedToday = pgtype.Bool{Bool: true, Valid: true}
	}

	query := ""
	if params.Q != nil {
		query = strings.TrimSpace(*params.Q)
	}

	// fetch runs one page; with FTS when q is present. Both queries carry
	// the pre-LIMIT total via a window function.
	fetch := func(offset, limit int64) ([]api.Ticket, int64, error) {
		var (
			items []api.Ticket
			total int64
		)
		if query != "" {
			rows, err := h.q.SearchTickets(r.Context(), store.SearchTicketsParams{
				Statuses:    arg.Statuses,
				Priorities:  arg.Priorities,
				AssigneeID:  arg.AssigneeID,
				Unassigned:  arg.Unassigned,
				TeamID:      arg.TeamID,
				RequesterID: arg.RequesterID,
				TagID:       arg.TagID,
				ClosedToday: arg.ClosedToday,
				PageOffset:  offset,
				PageLimit:   limit,
				Query:       query,
			})
			if err != nil {
				return nil, 0, err
			}
			items = make([]api.Ticket, 0, len(rows))
			for _, row := range rows {
				items = append(items, toAPITicket(row.Ticket, row.RequesterName, row.RequesterEmail, row.AssigneeName, row.AssigneeEmail))
				total = row.TotalCount
			}
			return items, total, nil
		}
		rows, err := h.q.ListTickets(r.Context(), store.ListTicketsParams{
			Statuses:    arg.Statuses,
			Priorities:  arg.Priorities,
			AssigneeID:  arg.AssigneeID,
			Unassigned:  arg.Unassigned,
			TeamID:      arg.TeamID,
			RequesterID: arg.RequesterID,
			TagID:       arg.TagID,
			ClosedToday: arg.ClosedToday,
			PageOffset:  offset,
			PageLimit:   limit,
		})
		if err != nil {
			return nil, 0, err
		}
		items = make([]api.Ticket, 0, len(rows))
		for _, row := range rows {
			items = append(items, toAPITicket(row.Ticket, row.RequesterName, row.RequesterEmail, row.AssigneeName, row.AssigneeEmail))
			total = row.TotalCount
		}
		return items, total, nil
	}

	items, total, err := fetch(arg.PageOffset, arg.PageLimit)
	if err != nil {
		serverError(w, r, "list tickets", err)
		return
	}
	if len(items) == 0 && arg.PageOffset > 0 {
		// Page past the end: the window-function total came back empty, so
		// probe once from offset 0 for the real match count.
		if _, total, err = fetch(0, 1); err != nil {
			serverError(w, r, "count tickets", err)
			return
		}
	}
	writeJSON(w, r, http.StatusOK, api.TicketList{Items: items, Total: total})
}

// intersect returns the members of a that are also in b, preserving a's
// order.
func intersect(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if inB[s] {
			out = append(out, s)
		}
	}
	return out
}

// CreateTicket implements POST /tickets (agent/admin): ticket + first
// article + audit event + stream event in one service transaction.
func (h *Handlers) CreateTicket(w http.ResponseWriter, r *http.Request) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	var req api.CreateTicketJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Subject) == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "subject is required")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "body is required")
		return
	}
	priority := store.TicketPriority("")
	if req.Priority != nil {
		if !req.Priority.Valid() {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid priority %q", *req.Priority))
			return
		}
		priority = store.TicketPriority(*req.Priority)
	}

	requesterID := caller.ID
	if req.RequesterId != nil {
		if _, err := h.q.GetUserByID(r.Context(), *req.RequesterId); err != nil {
			if isNoRows(err) {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", "unknown requester_id")
				return
			}
			serverError(w, r, "check requester", err)
			return
		}
		requesterID = *req.RequesterId
	}
	var teamID *uuid.UUID
	if req.TeamId != nil {
		if _, err := h.q.GetTeamByID(r.Context(), *req.TeamId); err != nil {
			if isNoRows(err) {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", "unknown team_id")
				return
			}
			serverError(w, r, "check team", err)
			return
		}
		teamID = req.TeamId
	}

	created, _, err := h.svc.CreateTicket(r.Context(), ticket.CreateTicketParams{
		Subject:     req.Subject,
		RequesterID: requesterID,
		Priority:    priority,
		TeamID:      teamID,
		ActorID:     &caller.ID,
		Article: ticket.ArticleInput{
			AuthorID:   &caller.ID,
			SenderType: store.ArticleSenderAgent,
			Channel:    store.ArticleChannelWeb,
			BodyText:   req.Body,
		},
	})
	if err != nil {
		writeTicketServiceError(w, r, "create ticket", err)
		return
	}

	detail, found, err := h.ticketDetail(r.Context(), created.ID)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("ticket %s vanished after create", created.ID)
		}
		serverError(w, r, "load created ticket", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, detail)
}

// GetTicket implements GET /tickets/{id} (agent/admin).
func (h *Handlers) GetTicket(w http.ResponseWriter, r *http.Request, id api.TicketID) {
	detail, found, err := h.ticketDetail(r.Context(), id)
	if err != nil {
		serverError(w, r, "get ticket", err)
		return
	}
	if !found {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such ticket")
		return
	}
	writeJSON(w, r, http.StatusOK, detail)
}

// optionalUUID is a tri-state PATCH field: absent, explicit null (clear), or
// a uuid (set).
type optionalUUID struct {
	set bool
	id  *uuid.UUID
}

// parseOptionalUUID reads a tri-state uuid field out of the raw PATCH body.
func parseOptionalUUID(fields map[string]json.RawMessage, name string) (optionalUUID, error) {
	raw, present := fields[name]
	if !present {
		return optionalUUID{}, nil
	}
	var id *uuid.UUID
	if err := json.Unmarshal(raw, &id); err != nil {
		return optionalUUID{}, fmt.Errorf("%s must be a uuid or null", name)
	}
	return optionalUUID{set: true, id: id}, nil
}

// UpdateTicket implements PATCH /tickets/{id} (agent/admin). Omitted fields
// are unchanged; explicit null for assignee_id / team_id clears the field.
func (h *Handlers) UpdateTicket(w http.ResponseWriter, r *http.Request, id api.TicketID) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	fields, ok := decodeJSONFields(w, r, "status", "priority", "assignee_id", "team_id")
	if !ok {
		return
	}

	var status *store.TicketStatus
	if raw, present := fields["status"]; present {
		var s *string
		if err := json.Unmarshal(raw, &s); err != nil || s == nil || !api.TicketStatus(*s).Valid() {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid status")
			return
		}
		v := store.TicketStatus(*s)
		status = &v
	}
	var priority *store.TicketPriority
	if raw, present := fields["priority"]; present {
		var p *string
		if err := json.Unmarshal(raw, &p); err != nil || p == nil || !api.TicketPriority(*p).Valid() {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid priority")
			return
		}
		v := store.TicketPriority(*p)
		priority = &v
	}
	assignee, err := parseOptionalUUID(fields, "assignee_id")
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}
	team, err := parseOptionalUUID(fields, "team_id")
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}

	// Referential pre-checks so a bad id is a clean 400, not a partially
	// applied patch. Assignees must be able to work the ticket.
	if assignee.set && assignee.id != nil {
		u, err := h.q.GetUserByID(r.Context(), *assignee.id)
		if err != nil {
			if isNoRows(err) {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", "unknown assignee_id")
				return
			}
			serverError(w, r, "check assignee", err)
			return
		}
		if u.Role != store.UserRoleAgent && u.Role != store.UserRoleAdmin {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "assignee must be an agent or admin")
			return
		}
	}
	if team.set && team.id != nil {
		if _, err := h.q.GetTeamByID(r.Context(), *team.id); err != nil {
			if isNoRows(err) {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", "unknown team_id")
				return
			}
			serverError(w, r, "check team", err)
			return
		}
	}

	// Apply the whole patch through ONE service transaction: every changed
	// field is audited, at most one ticket.updated notification fires, and
	// a failure rolls the entire patch back — a PATCH is never
	// half-applied. The service 404s on a missing ticket, so an empty
	// patch on a missing id still 404s.
	if _, err := h.svc.UpdateFields(r.Context(), id, ticket.UpdateFieldsParams{
		Status:      status,
		Priority:    priority,
		SetAssignee: assignee.set,
		AssigneeID:  assignee.id,
		SetTeam:     team.set,
		TeamID:      team.id,
	}, &caller.ID); err != nil {
		writeTicketServiceError(w, r, "update ticket", err)
		return
	}

	row, err := h.q.GetTicket(r.Context(), id)
	if err != nil {
		serverError(w, r, "reload ticket", err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPITicket(row.Ticket, row.RequesterName, row.RequesterEmail, row.AssigneeName, row.AssigneeEmail))
}

// SetTicketTags implements PUT /tickets/{id}/tags (agent/admin): full
// replacement, all-or-nothing.
func (h *Handlers) SetTicketTags(w http.ResponseWriter, r *http.Request, id api.TicketID) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	var req api.SetTicketTagsJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TagIds == nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "tag_ids is required (may be empty)")
		return
	}

	if err := h.svc.SetTags(r.Context(), id, req.TagIds, &caller.ID); err != nil {
		writeTicketServiceError(w, r, "set ticket tags", err)
		return
	}
	tags, err := h.q.ListTagsForTicket(r.Context(), id)
	if err != nil {
		serverError(w, r, "list ticket tags", err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPITags(tags))
}
