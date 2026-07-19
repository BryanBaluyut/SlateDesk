// M4 customer portal: authenticated self-service for customer-role users.
//
// HARD ISOLATION (a security boundary, not a filter). Every query is scoped
// to the calling customer's own tickets (requester_id = caller). A ticket the
// caller does not own returns 404, NOT 403, so the endpoint cannot be used to
// probe which ticket ids exist (anti-enumeration). Article listings exclude
// internal notes unconditionally. The portal never exposes staff identities,
// assignees, or team routing. requireCustomer (authPolicy) additionally
// refuses agent/admin principals — including API keys — with 403; agents use
// the workspace.
package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// portalMaxTickets caps a portal ticket list. A customer with more than this
// many tickets sees the most recently updated ones (the portal has no pager).
const portalMaxTickets = 200

// toPortalTicket projects a ticket row to the portal shape, dropping assignee
// and team (never exposed to customers).
func toPortalTicket(t store.Ticket) api.PortalTicket {
	out := api.PortalTicket{
		Id:        t.ID,
		Number:    t.Number,
		Subject:   t.Subject,
		Status:    api.TicketStatus(t.Status),
		Priority:  api.TicketPriority(t.Priority),
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
	if t.ClosedAt.Valid {
		ts := t.ClosedAt.Time
		out.ClosedAt = &ts
	}
	return out
}

// toPortalArticle projects a public article to the portal shape, reducing the
// author to a sender_type so staff identities are never disclosed. Attachments
// are intentionally omitted: the portal has no customer-scoped attachment
// download route yet, so surfacing filenames the client cannot fetch would be
// dead metadata.
func toPortalArticle(a store.Article) api.PortalArticle {
	out := api.PortalArticle{
		Id:         a.ID,
		TicketId:   a.TicketID,
		SenderType: api.ArticleSenderType(a.SenderType),
		BodyText:   a.BodyText,
		CreatedAt:  a.CreatedAt,
	}
	if a.BodyHtml.Valid {
		html := a.BodyHtml.String
		out.BodyHtml = &html
	}
	return out
}

// portalTicket loads a ticket and enforces ownership. found is false — mapped
// by callers to 404, never 403 — when the ticket does not exist OR is not
// owned by caller, so the two are indistinguishable to the client.
func (h *Handlers) portalTicket(ctx context.Context, id, callerID uuid.UUID) (store.Ticket, bool, error) {
	row, err := h.q.GetTicket(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return store.Ticket{}, false, nil
		}
		return store.Ticket{}, false, err
	}
	if row.Ticket.RequesterID != callerID {
		return store.Ticket{}, false, nil
	}
	return row.Ticket, true, nil
}

// portalTicketDetail loads the ticket plus its PUBLIC thread (internal notes
// excluded).
func (h *Handlers) portalTicketDetail(ctx context.Context, t store.Ticket) (api.PortalTicketDetail, error) {
	base := toPortalTicket(t)
	articleRows, err := h.q.ListArticlesByTicket(ctx, t.ID)
	if err != nil {
		return api.PortalTicketDetail{}, err
	}

	articles := make([]api.PortalArticle, 0, len(articleRows))
	for _, ar := range articleRows {
		if ar.Article.IsInternal {
			continue // internal notes are NEVER shown to customers
		}
		articles = append(articles, toPortalArticle(ar.Article))
	}

	return api.PortalTicketDetail{
		Id:        base.Id,
		Number:    base.Number,
		Subject:   base.Subject,
		Status:    base.Status,
		Priority:  base.Priority,
		CreatedAt: base.CreatedAt,
		UpdatedAt: base.UpdatedAt,
		ClosedAt:  base.ClosedAt,
		Articles:  articles,
	}, nil
}

// ListPortalTickets implements GET /portal/tickets (customer): the caller's
// own tickets, newest first.
func (h *Handlers) ListPortalTickets(w http.ResponseWriter, r *http.Request) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired credentials")
		return
	}
	rows, err := h.q.ListTickets(r.Context(), store.ListTicketsParams{
		RequesterID: pgtype.UUID{Bytes: caller.ID, Valid: true},
		PageLimit:   portalMaxTickets,
	})
	if err != nil {
		serverError(w, r, "list portal tickets", err)
		return
	}
	out := make([]api.PortalTicket, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPortalTicket(row.Ticket))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// CreatePortalTicket implements POST /portal/tickets (customer): opens a
// ticket requested by the caller; the body becomes the first public article.
func (h *Handlers) CreatePortalTicket(w http.ResponseWriter, r *http.Request) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired credentials")
		return
	}
	var req api.CreatePortalTicketJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Subject) == "" {
		badRequest(w, r, "subject is required")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		badRequest(w, r, "body is required")
		return
	}

	created, _, err := h.svc.CreateTicket(r.Context(), ticket.CreateTicketParams{
		Subject:     req.Subject,
		RequesterID: caller.ID,
		ActorID:     &caller.ID,
		Article: ticket.ArticleInput{
			AuthorID:   &caller.ID,
			SenderType: store.ArticleSenderCustomer,
			Channel:    store.ArticleChannelWeb,
			BodyText:   req.Body,
		},
	})
	if err != nil {
		writeTicketServiceError(w, r, "create portal ticket", err)
		return
	}
	detail, err := h.portalTicketDetail(r.Context(), created)
	if err != nil {
		serverError(w, r, "load created portal ticket", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, detail)
}

// GetPortalTicket implements GET /portal/tickets/{id} (customer): the ticket
// with its public thread. A ticket the caller does not own is 404, not 403.
func (h *Handlers) GetPortalTicket(w http.ResponseWriter, r *http.Request, id api.PortalTicketID) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired credentials")
		return
	}
	t, found, err := h.portalTicket(r.Context(), id, caller.ID)
	if err != nil {
		serverError(w, r, "load portal ticket", err)
		return
	}
	if !found {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such ticket")
		return
	}
	detail, err := h.portalTicketDetail(r.Context(), t)
	if err != nil {
		serverError(w, r, "load portal ticket detail", err)
		return
	}
	writeJSON(w, r, http.StatusOK, detail)
}

// ReplyPortalTicket implements POST /portal/tickets/{id}/reply (customer):
// appends a public customer reply. The ticket service's status matrix flips a
// waiting_on_customer ticket back to open. Not-owned is 404, not 403.
func (h *Handlers) ReplyPortalTicket(w http.ResponseWriter, r *http.Request, id api.PortalTicketID) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired credentials")
		return
	}
	// Ownership check BEFORE reading the body, so a non-owner learns nothing
	// beyond 404 regardless of what they submit.
	t, found, err := h.portalTicket(r.Context(), id, caller.ID)
	if err != nil {
		serverError(w, r, "load portal ticket", err)
		return
	}
	if !found {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such ticket")
		return
	}
	var req api.ReplyPortalTicketJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		badRequest(w, r, "body is required")
		return
	}

	article, err := h.svc.AddArticle(r.Context(), t.ID, ticket.ArticleInput{
		AuthorID:   &caller.ID,
		SenderType: store.ArticleSenderCustomer,
		Channel:    store.ArticleChannelWeb,
		IsInternal: false,
		BodyText:   req.Body,
	})
	if err != nil {
		writeTicketServiceError(w, r, "reply to portal ticket", err)
		return
	}
	// Mirror the email channel (architecture doc §3, "reopen on customer reply
	// to closed ticket"): the add-article status matrix leaves a CLOSED ticket
	// closed, so a portal reply would otherwise be buried with the ticket never
	// resurfacing in staff views. Reopen it explicitly. A waiting_on_customer
	// ticket is already flipped to open by AddArticle.
	if t.Status == store.TicketStatusClosed {
		if _, err := h.svc.UpdateStatus(r.Context(), t.ID, store.TicketStatusOpen, &caller.ID); err != nil {
			serverError(w, r, "reopen portal ticket on reply", err)
			return
		}
	}
	writeJSON(w, r, http.StatusCreated, toPortalArticle(article))
}
