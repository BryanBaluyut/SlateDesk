// Package ticket is the transactional M2 ticket service. Every write —
// create, article, status/priority/assignee/team/tags — happens in ONE
// database transaction that also records a ticket_events audit row and
// fires pg_notify on the events channel, so a notification can never
// outlive a rolled-back write and an audit row can never be missing for a
// committed one.
package ticket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microcosm-cc/bluemonday"

	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// ErrNotFound is returned when the referenced ticket (or tag) does not exist.
var ErrNotFound = errors.New("ticket: not found")

// ErrValidation wraps all caller-input errors (empty subject, bad enum
// value, unknown tag id, ...). Handlers map it to a 4xx problem response.
var ErrValidation = errors.New("ticket: invalid input")

// Notification types published via pg_notify (payload: {"type": ...,
// "ticket_id": ...}). SSE clients treat them as cache-invalidation hints.
const (
	NotifyTicketCreated  = "ticket.created"
	NotifyTicketUpdated  = "ticket.updated"
	NotifyArticleCreated = "article.created"
)

// ticket_events audit row types.
const (
	EventCreated         = "created"
	EventArticleAdded    = "article_added"
	EventStatusChanged   = "status_changed"
	EventPriorityChanged = "priority_changed"
	EventAssigneeChanged = "assignee_changed"
	EventTeamChanged     = "team_changed"
	EventTagsChanged     = "tags_changed"
)

// Service implements ticket-core writes on top of the sqlc store.
type Service struct {
	pool *pgxpool.Pool
	html *bluemonday.Policy

	// now is swappable in tests (ticket-number day rollover).
	now func() time.Time
}

// NewService returns a Service using pool. HTML bodies are sanitized with
// bluemonday's UGC policy on every write path.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		pool: pool,
		html: bluemonday.UGCPolicy(),
		now:  time.Now,
	}
}

// nextStatusOnArticle is the add-article status matrix.
//
// Rules (M2 spec):
//   - Internal notes and system articles never change status.
//   - A public AGENT reply on an OPEN ticket flips it to
//     waiting_on_customer ("ball is in the customer's court").
//   - A CUSTOMER article (web portal, or an API article submitted on the
//     customer's behalf with sender_type=customer) on a
//     WAITING_ON_CUSTOMER ticket flips it back to open.
//   - CLOSED stays closed: reopening is an explicit UpdateStatus call
//     (M3 adds the customer-reply-reopens-ticket behavior on the email
//     ingest path, where it will make that explicit call).
//   - ON_HOLD is pinned until an explicit status change.
//
// Full matrix (rows = current status, columns = article kind):
//
//	                    | agent public         | customer public | internal/system
//	open                | waiting_on_customer  | open            | open
//	waiting_on_customer | waiting_on_customer  | open            | waiting_on_customer
//	on_hold             | on_hold              | on_hold         | on_hold
//	closed              | closed               | closed          | closed
func nextStatusOnArticle(current store.TicketStatus, sender store.ArticleSender, isInternal bool) store.TicketStatus {
	if isInternal || sender == store.ArticleSenderSystem {
		return current
	}
	switch {
	case current == store.TicketStatusOpen && sender == store.ArticleSenderAgent:
		return store.TicketStatusWaitingOnCustomer
	case current == store.TicketStatusWaitingOnCustomer && sender == store.ArticleSenderCustomer:
		return store.TicketStatusOpen
	default:
		return current
	}
}

// ArticleInput describes a new article (the create-ticket first article or
// a later reply/note). BodyHTML, when non-empty, is sanitized with
// bluemonday before storage; BodyText is required and feeds FTS.
type ArticleInput struct {
	AuthorID   *uuid.UUID // nil for external/system authors
	SenderType store.ArticleSender
	Channel    store.ArticleChannel
	IsInternal bool
	BodyText   string
	BodyHTML   string
}

// CreateTicketParams is the input to CreateTicket. Priority defaults to
// medium; the ticket always starts open with the given first article.
type CreateTicketParams struct {
	Subject     string
	RequesterID uuid.UUID
	Priority    store.TicketPriority // "" = medium
	AssigneeID  *uuid.UUID
	TeamID      *uuid.UUID
	ActorID     *uuid.UUID // audit actor; nil = system/self-service
	Article     ArticleInput
}

// CreateTicket allocates the day's next ticket number, then creates the
// ticket, its first article, the "created" audit event, and the
// ticket.created notification — all in one transaction. The per-day
// counter UPSERT serializes concurrent creates (any replica count), so
// numbers are unique; a rolled-back create can at worst leave a gap.
func (s *Service) CreateTicket(ctx context.Context, p CreateTicketParams) (store.Ticket, store.Article, error) {
	var (
		ticket  store.Ticket
		article store.Article
	)

	p.Subject = strings.TrimSpace(p.Subject)
	if p.Subject == "" {
		return ticket, article, fmt.Errorf("%w: subject required", ErrValidation)
	}
	if p.Priority == "" {
		p.Priority = store.TicketPriorityMedium
	}
	if err := validPriority(p.Priority); err != nil {
		return ticket, article, err
	}
	if err := validateArticleInput(p.Article); err != nil {
		return ticket, article, err
	}

	err := s.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		// Ticket numbers use the UTC day so all replicas agree on the
		// counter row regardless of local timezones.
		day := s.now().UTC().Truncate(24 * time.Hour)
		seq, err := q.NextTicketNumber(ctx, day)
		if err != nil {
			return fmt.Errorf("allocate number: %w", err)
		}
		number := fmt.Sprintf("%s-%04d", day.Format("20060102"), seq)

		ticket, err = q.CreateTicket(ctx, store.CreateTicketParams{
			Number:      number,
			Subject:     p.Subject,
			Status:      store.TicketStatusOpen,
			Priority:    p.Priority,
			RequesterID: p.RequesterID,
			AssigneeID:  pgUUID(p.AssigneeID),
			TeamID:      pgUUID(p.TeamID),
		})
		if err != nil {
			return fmt.Errorf("insert ticket: %w", err)
		}

		article, err = q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   ticket.ID,
			AuthorID:   pgUUID(p.Article.AuthorID),
			SenderType: p.Article.SenderType,
			Channel:    p.Article.Channel,
			IsInternal: p.Article.IsInternal,
			BodyText:   p.Article.BodyText,
			BodyHtml:   s.sanitizedHTML(p.Article.BodyHTML),
		})
		if err != nil {
			return fmt.Errorf("insert first article: %w", err)
		}

		if err := s.addEvent(ctx, q, ticket.ID, p.ActorID, EventCreated, map[string]any{
			"number":  ticket.Number,
			"subject": ticket.Subject,
		}); err != nil {
			return err
		}
		return s.notify(ctx, tx, NotifyTicketCreated, ticket.ID)
	})
	if err != nil {
		return store.Ticket{}, store.Article{}, fmt.Errorf("ticket: create: %w", err)
	}
	return ticket, article, nil
}

// AddArticle appends an article to an existing ticket and applies the
// status matrix (see nextStatusOnArticle). Everything — article, optional
// status change, audit events, notifications — commits atomically. The
// ticket row is locked FOR UPDATE so concurrent articles serialize their
// status decisions.
func (s *Service) AddArticle(ctx context.Context, ticketID uuid.UUID, in ArticleInput) (store.Article, error) {
	if err := validateArticleInput(in); err != nil {
		return store.Article{}, err
	}

	var article store.Article
	err := s.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		t, err := q.GetTicketForUpdate(ctx, ticketID)
		if err != nil {
			return lookupErr("ticket", err)
		}

		article, err = q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   t.ID,
			AuthorID:   pgUUID(in.AuthorID),
			SenderType: in.SenderType,
			Channel:    in.Channel,
			IsInternal: in.IsInternal,
			BodyText:   in.BodyText,
			BodyHtml:   s.sanitizedHTML(in.BodyHTML),
		})
		if err != nil {
			return fmt.Errorf("insert article: %w", err)
		}

		if err := s.addEvent(ctx, q, t.ID, in.AuthorID, EventArticleAdded, map[string]any{
			"article_id":  article.ID,
			"sender_type": in.SenderType,
			"is_internal": in.IsInternal,
		}); err != nil {
			return err
		}

		if next := nextStatusOnArticle(t.Status, in.SenderType, in.IsInternal); next != t.Status {
			if _, err := q.UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{ID: t.ID, Status: next}); err != nil {
				return fmt.Errorf("flip status: %w", err)
			}
			if err := s.addEvent(ctx, q, t.ID, in.AuthorID, EventStatusChanged, map[string]any{
				"from": t.Status, "to": next,
			}); err != nil {
				return err
			}
			if err := s.notify(ctx, tx, NotifyTicketUpdated, t.ID); err != nil {
				return err
			}
		} else if _, err := q.TouchTicket(ctx, t.ID); err != nil {
			return fmt.Errorf("touch ticket: %w", err)
		}

		return s.notify(ctx, tx, NotifyArticleCreated, t.ID)
	})
	if err != nil {
		return store.Article{}, fmt.Errorf("ticket: add article: %w", err)
	}
	return article, nil
}

// UpdateFieldsParams is a partial ticket update. A nil Status/Priority
// leaves the field unchanged; SetAssignee/SetTeam distinguish "clear the
// field" (true with a nil id) from "leave unchanged" (false).
type UpdateFieldsParams struct {
	Status      *store.TicketStatus
	Priority    *store.TicketPriority
	SetAssignee bool
	AssigneeID  *uuid.UUID
	SetTeam     bool
	TeamID      *uuid.UUID
}

// UpdateFields applies any combination of status / priority / assignee /
// team changes in ONE transaction: each changed field gets its own audit
// event, and at most one ticket.updated notification fires. A PATCH can
// therefore never be half-applied — a mid-sequence failure rolls the whole
// update back. Unchanged fields are no-ops; the returned ticket is the row
// after the update (the current row when nothing changed).
func (s *Service) UpdateFields(ctx context.Context, ticketID uuid.UUID, p UpdateFieldsParams, actorID *uuid.UUID) (store.Ticket, error) {
	if p.Status != nil {
		if err := validStatus(*p.Status); err != nil {
			return store.Ticket{}, err
		}
	}
	if p.Priority != nil {
		if err := validPriority(*p.Priority); err != nil {
			return store.Ticket{}, err
		}
	}

	var ticket store.Ticket
	err := s.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		t, err := q.GetTicketForUpdate(ctx, ticketID)
		if err != nil {
			return lookupErr("ticket", err)
		}
		ticket = t
		changed := false

		if p.Status != nil && t.Status != *p.Status {
			ticket, err = q.UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{ID: t.ID, Status: *p.Status})
			if err != nil {
				return fmt.Errorf("update status: %w", err)
			}
			if err := s.addEvent(ctx, q, t.ID, actorID, EventStatusChanged, map[string]any{
				"from": t.Status, "to": *p.Status,
			}); err != nil {
				return err
			}
			changed = true
		}
		if p.Priority != nil && t.Priority != *p.Priority {
			ticket, err = q.UpdateTicketPriority(ctx, store.UpdateTicketPriorityParams{ID: t.ID, Priority: *p.Priority})
			if err != nil {
				return fmt.Errorf("update priority: %w", err)
			}
			if err := s.addEvent(ctx, q, t.ID, actorID, EventPriorityChanged, map[string]any{
				"from": t.Priority, "to": *p.Priority,
			}); err != nil {
				return err
			}
			changed = true
		}
		if next := pgUUID(p.AssigneeID); p.SetAssignee && t.AssigneeID != next {
			ticket, err = q.UpdateTicketAssignee(ctx, store.UpdateTicketAssigneeParams{ID: t.ID, AssigneeID: next})
			if err != nil {
				return fmt.Errorf("update assignee: %w", err)
			}
			if err := s.addEvent(ctx, q, t.ID, actorID, EventAssigneeChanged, map[string]any{
				"from": uuidOrNil(t.AssigneeID), "to": p.AssigneeID,
			}); err != nil {
				return err
			}
			changed = true
		}
		if next := pgUUID(p.TeamID); p.SetTeam && t.TeamID != next {
			ticket, err = q.UpdateTicketTeam(ctx, store.UpdateTicketTeamParams{ID: t.ID, TeamID: next})
			if err != nil {
				return fmt.Errorf("update team: %w", err)
			}
			if err := s.addEvent(ctx, q, t.ID, actorID, EventTeamChanged, map[string]any{
				"from": uuidOrNil(t.TeamID), "to": p.TeamID,
			}); err != nil {
				return err
			}
			changed = true
		}

		if !changed {
			return nil
		}
		return s.notify(ctx, tx, NotifyTicketUpdated, t.ID)
	})
	if err != nil {
		return store.Ticket{}, fmt.Errorf("ticket: update fields: %w", err)
	}
	return ticket, nil
}

// UpdateStatus sets the status explicitly (this is also the reopen path
// for closed tickets). No-op if the status is unchanged.
func (s *Service) UpdateStatus(ctx context.Context, ticketID uuid.UUID, status store.TicketStatus, actorID *uuid.UUID) (store.Ticket, error) {
	return s.UpdateFields(ctx, ticketID, UpdateFieldsParams{Status: &status}, actorID)
}

// UpdatePriority sets the priority. No-op if unchanged.
func (s *Service) UpdatePriority(ctx context.Context, ticketID uuid.UUID, priority store.TicketPriority, actorID *uuid.UUID) (store.Ticket, error) {
	return s.UpdateFields(ctx, ticketID, UpdateFieldsParams{Priority: &priority}, actorID)
}

// Assign sets (or, with nil, clears) the assignee. No-op if unchanged.
func (s *Service) Assign(ctx context.Context, ticketID uuid.UUID, assigneeID *uuid.UUID, actorID *uuid.UUID) (store.Ticket, error) {
	return s.UpdateFields(ctx, ticketID, UpdateFieldsParams{SetAssignee: true, AssigneeID: assigneeID}, actorID)
}

// SetTeam sets (or, with nil, clears) the owning team. No-op if unchanged.
func (s *Service) SetTeam(ctx context.Context, ticketID uuid.UUID, teamID *uuid.UUID, actorID *uuid.UUID) (store.Ticket, error) {
	return s.UpdateFields(ctx, ticketID, UpdateFieldsParams{SetTeam: true, TeamID: teamID}, actorID)
}

// SetTags replaces the ticket's tag set (delete + insert) after checking
// every tag id exists. Emits one tags_changed event.
func (s *Service) SetTags(ctx context.Context, ticketID uuid.UUID, tagIDs []uuid.UUID, actorID *uuid.UUID) error {
	tagIDs = dedupe(tagIDs)
	err := s.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		t, err := q.GetTicketForUpdate(ctx, ticketID)
		if err != nil {
			return lookupErr("ticket", err)
		}
		if len(tagIDs) > 0 {
			n, err := q.CountTagsByIDs(ctx, tagIDs)
			if err != nil {
				return fmt.Errorf("check tag ids: %w", err)
			}
			if n != int64(len(tagIDs)) {
				return fmt.Errorf("%w: unknown tag id", ErrValidation)
			}
		}
		if err := q.ClearTicketTags(ctx, t.ID); err != nil {
			return fmt.Errorf("clear tags: %w", err)
		}
		if len(tagIDs) > 0 {
			if err := q.AddTicketTags(ctx, store.AddTicketTagsParams{TicketID: t.ID, TagIds: tagIDs}); err != nil {
				return fmt.Errorf("add tags: %w", err)
			}
		}
		if _, err := q.TouchTicket(ctx, t.ID); err != nil {
			return fmt.Errorf("touch ticket: %w", err)
		}
		if err := s.addEvent(ctx, q, t.ID, actorID, EventTagsChanged, map[string]any{
			"tag_ids": tagIDs,
		}); err != nil {
			return err
		}
		return s.notify(ctx, tx, NotifyTicketUpdated, t.ID)
	})
	if err != nil {
		return fmt.Errorf("ticket: set tags: %w", err)
	}
	return nil
}

// --- internals ---------------------------------------------------------------

// inTx runs fn inside one transaction, committing on nil.
func (s *Service) inTx(ctx context.Context, fn func(q *store.Queries, tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(store.New(tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// addEvent inserts a ticket_events audit row.
func (s *Service) addEvent(ctx context.Context, q *store.Queries, ticketID uuid.UUID, actorID *uuid.UUID, typ string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	if _, err := q.CreateTicketEvent(ctx, store.CreateTicketEventParams{
		TicketID: ticketID,
		ActorID:  pgUUID(actorID),
		Type:     typ,
		Payload:  raw,
	}); err != nil {
		return fmt.Errorf("insert %s event: %w", typ, err)
	}
	return nil
}

// notify fires pg_notify on the shared events channel INSIDE the current
// transaction: Postgres delivers notifications only on commit, which is
// exactly the semantics we want (no phantom events from rollbacks).
func (s *Service) notify(ctx context.Context, tx pgx.Tx, typ string, ticketID uuid.UUID) error {
	payload, err := json.Marshal(map[string]string{
		"type":      typ,
		"ticket_id": ticketID.String(),
	})
	if err != nil {
		return fmt.Errorf("marshal %s notification: %w", typ, err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", events.Channel, string(payload)); err != nil {
		return fmt.Errorf("pg_notify %s: %w", typ, err)
	}
	return nil
}

// sanitizedHTML runs bluemonday's UGC policy over html; empty input maps
// to SQL NULL.
func (s *Service) sanitizedHTML(html string) pgtype.Text {
	if strings.TrimSpace(html) == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s.html.Sanitize(html), Valid: true}
}

func validateArticleInput(in ArticleInput) error {
	switch in.SenderType {
	case store.ArticleSenderCustomer, store.ArticleSenderAgent, store.ArticleSenderSystem:
	default:
		return fmt.Errorf("%w: bad sender_type %q", ErrValidation, in.SenderType)
	}
	switch in.Channel {
	case store.ArticleChannelEmail, store.ArticleChannelApi, store.ArticleChannelWeb:
	default:
		return fmt.Errorf("%w: bad channel %q", ErrValidation, in.Channel)
	}
	if strings.TrimSpace(in.BodyText) == "" {
		return fmt.Errorf("%w: body_text required", ErrValidation)
	}
	return nil
}

func validStatus(s store.TicketStatus) error {
	switch s {
	case store.TicketStatusOpen, store.TicketStatusWaitingOnCustomer, store.TicketStatusOnHold, store.TicketStatusClosed:
		return nil
	default:
		return fmt.Errorf("%w: bad status %q", ErrValidation, s)
	}
}

func validPriority(p store.TicketPriority) error {
	switch p {
	case store.TicketPriorityLow, store.TicketPriorityMedium, store.TicketPriorityHigh, store.TicketPriorityCritical:
		return nil
	default:
		return fmt.Errorf("%w: bad priority %q", ErrValidation, p)
	}
}

// lookupErr maps pgx.ErrNoRows onto ErrNotFound for FOR UPDATE lookups.
func lookupErr(what string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return fmt.Errorf("load %s: %w", what, err)
}

// pgUUID converts an optional uuid to its pgtype representation.
func pgUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// uuidOrNil renders a pgtype.UUID for JSON payloads (nil when NULL).
func uuidOrNil(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	u := uuid.UUID(id.Bytes)
	return &u
}

// dedupe removes duplicate uuids, preserving order.
func dedupe(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
