package ticket

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// createUser seeds a user of the given role with a unique email.
func createUser(t *testing.T, role store.UserRole) store.User {
	t.Helper()
	u, err := store.New(testPool).CreateUser(context.Background(), store.CreateUserParams{
		Email: fmt.Sprintf("%s-%s@test.local", role, uuid.NewString()[:8]),
		Name:  "Test " + string(role),
		Role:  role,
	})
	if err != nil {
		t.Fatalf("seed %s user: %v", role, err)
	}
	return u
}

// newTestService returns a Service with a fixed clock (stable ticket-number
// days regardless of wall time).
func newTestService(day time.Time) *Service {
	s := NewService(testPool)
	s.now = func() time.Time { return day }
	return s
}

func customerArticle(author uuid.UUID, body string) ArticleInput {
	return ArticleInput{
		AuthorID:   &author,
		SenderType: store.ArticleSenderCustomer,
		Channel:    store.ArticleChannelWeb,
		BodyText:   body,
	}
}

// randomDay returns a random counter day far in the future, so repeated
// runs (including -count=N in one process) never share a counter row.
func randomDay() time.Time {
	return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, rand.IntN(3_000_000))
}

func TestTicketNumberingConcurrent(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	day := randomDay()

	const n = 100
	results := make([]int32, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = q.NextTicketNumber(ctx, day)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
	}
	slices.Sort(results)
	for i, got := range results {
		if got != int32(i+1) {
			t.Fatalf("after sorting, allocation %d = %d; want %d (not unique+sequential: %v)", i, got, i+1, results)
		}
	}

	// Day rollover: a different day starts back at 1.
	next, err := q.NextTicketNumber(ctx, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("rollover allocation: %v", err)
	}
	if next != 1 {
		t.Fatalf("rollover counter = %d; want 1", next)
	}
}

func TestCreateTicketNumberFormat(t *testing.T) {
	ctx := context.Background()
	day := randomDay().Add(13 * time.Hour) // mid-day: CreateTicket must truncate
	svc := newTestService(day)
	requester := createUser(t, store.UserRoleCustomer)

	for i := 1; i <= 2; i++ {
		tk, article, err := svc.CreateTicket(ctx, CreateTicketParams{
			Subject:     fmt.Sprintf("Number format %d", i),
			RequesterID: requester.ID,
			Article:     customerArticle(requester.ID, "body"),
		})
		if err != nil {
			t.Fatalf("create ticket %d: %v", i, err)
		}
		want := fmt.Sprintf("%s-%04d", day.UTC().Truncate(24*time.Hour).Format("20060102"), i)
		if tk.Number != want {
			t.Errorf("ticket %d number = %q; want %q", i, tk.Number, want)
		}
		if tk.Status != store.TicketStatusOpen || tk.Priority != store.TicketPriorityMedium {
			t.Errorf("ticket defaults = %s/%s; want open/medium", tk.Status, tk.Priority)
		}
		if article.TicketID != tk.ID {
			t.Errorf("first article ticket_id = %s; want %s", article.TicketID, tk.ID)
		}
	}
}

func TestNextStatusOnArticleMatrix(t *testing.T) {
	const (
		open    = store.TicketStatusOpen
		waiting = store.TicketStatusWaitingOnCustomer
		onHold  = store.TicketStatusOnHold
		closed  = store.TicketStatusClosed
	)
	tests := []struct {
		current    store.TicketStatus
		sender     store.ArticleSender
		isInternal bool
		want       store.TicketStatus
	}{
		// Agent public replies.
		{open, store.ArticleSenderAgent, false, waiting},
		{waiting, store.ArticleSenderAgent, false, waiting},
		{onHold, store.ArticleSenderAgent, false, onHold},
		{closed, store.ArticleSenderAgent, false, closed},
		// Customer articles.
		{open, store.ArticleSenderCustomer, false, open},
		{waiting, store.ArticleSenderCustomer, false, open},
		{onHold, store.ArticleSenderCustomer, false, onHold},
		{closed, store.ArticleSenderCustomer, false, closed}, // closed stays closed
		// Internal notes never flip.
		{open, store.ArticleSenderAgent, true, open},
		{waiting, store.ArticleSenderAgent, true, waiting},
		{onHold, store.ArticleSenderAgent, true, onHold},
		{closed, store.ArticleSenderAgent, true, closed},
		// System articles never flip.
		{open, store.ArticleSenderSystem, false, open},
		{waiting, store.ArticleSenderSystem, false, waiting},
		{onHold, store.ArticleSenderSystem, false, onHold},
		{closed, store.ArticleSenderSystem, false, closed},
	}
	for _, tc := range tests {
		got := nextStatusOnArticle(tc.current, tc.sender, tc.isInternal)
		if got != tc.want {
			t.Errorf("nextStatusOnArticle(%s, %s, internal=%v) = %s; want %s",
				tc.current, tc.sender, tc.isInternal, got, tc.want)
		}
	}
}

// TestAddArticleStatusFlow drives the matrix through the real service and
// database, including the closed-stays-closed rule and explicit reopen.
func TestAddArticleStatusFlow(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testPool)
	q := store.New(testPool)
	requester := createUser(t, store.UserRoleCustomer)
	agent := createUser(t, store.UserRoleAgent)

	tk, _, err := svc.CreateTicket(ctx, CreateTicketParams{
		Subject:     "Status flow",
		RequesterID: requester.ID,
		Article:     customerArticle(requester.ID, "it is broken"),
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	status := func() store.TicketStatus {
		t.Helper()
		row, err := q.GetTicket(ctx, tk.ID)
		if err != nil {
			t.Fatalf("get ticket: %v", err)
		}
		return row.Ticket.Status
	}

	// Agent public reply: open -> waiting_on_customer.
	if _, err := svc.AddArticle(ctx, tk.ID, ArticleInput{
		AuthorID: &agent.ID, SenderType: store.ArticleSenderAgent,
		Channel: store.ArticleChannelWeb, BodyText: "try turning it off and on",
	}); err != nil {
		t.Fatalf("agent reply: %v", err)
	}
	if got := status(); got != store.TicketStatusWaitingOnCustomer {
		t.Fatalf("after agent reply status = %s; want waiting_on_customer", got)
	}

	// Agent internal note: no change.
	if _, err := svc.AddArticle(ctx, tk.ID, ArticleInput{
		AuthorID: &agent.ID, SenderType: store.ArticleSenderAgent,
		Channel: store.ArticleChannelWeb, IsInternal: true, BodyText: "suspect PEBKAC",
	}); err != nil {
		t.Fatalf("internal note: %v", err)
	}
	if got := status(); got != store.TicketStatusWaitingOnCustomer {
		t.Fatalf("after internal note status = %s; want waiting_on_customer", got)
	}

	// Customer reply: waiting_on_customer -> open.
	if _, err := svc.AddArticle(ctx, tk.ID, customerArticle(requester.ID, "did not help")); err != nil {
		t.Fatalf("customer reply: %v", err)
	}
	if got := status(); got != store.TicketStatusOpen {
		t.Fatalf("after customer reply status = %s; want open", got)
	}

	// Close, then confirm a customer article does NOT reopen (M2 rule).
	closedTk, err := svc.UpdateStatus(ctx, tk.ID, store.TicketStatusClosed, &agent.ID)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closedTk.ClosedAt.Valid {
		t.Fatal("closed ticket has no closed_at")
	}
	if _, err := svc.AddArticle(ctx, tk.ID, customerArticle(requester.ID, "hello? still broken")); err != nil {
		t.Fatalf("customer article on closed: %v", err)
	}
	if got := status(); got != store.TicketStatusClosed {
		t.Fatalf("after customer article on closed ticket status = %s; want closed", got)
	}

	// Explicit reopen clears closed_at.
	reopened, err := svc.UpdateStatus(ctx, tk.ID, store.TicketStatusOpen, &agent.ID)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Status != store.TicketStatusOpen || reopened.ClosedAt.Valid {
		t.Fatalf("reopened = %s closed_at.Valid=%v; want open with closed_at cleared", reopened.Status, reopened.ClosedAt.Valid)
	}

	// The audit trail recorded every step.
	evs, err := q.ListTicketEvents(ctx, tk.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	want := []string{
		EventCreated,
		EventArticleAdded, EventStatusChanged, // agent reply + flip
		EventArticleAdded,                     // internal note
		EventArticleAdded, EventStatusChanged, // customer reply + flip
		EventStatusChanged, // close
		EventArticleAdded,  // customer article on closed (no flip)
		EventStatusChanged, // reopen
	}
	if !slices.Equal(types, want) {
		t.Fatalf("event trail = %v; want %v", types, want)
	}
}

func TestAddArticleUnknownTicket(t *testing.T) {
	svc := NewService(testPool)
	_, err := svc.AddArticle(context.Background(), uuid.New(), customerArticle(uuid.New(), "hi"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

func TestBodyHTMLSanitized(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testPool)
	requester := createUser(t, store.UserRoleCustomer)

	in := customerArticle(requester.ID, "xss attempt")
	in.BodyHTML = `<p>hi</p><script>alert("pwn")</script><a href="javascript:evil()">x</a>`
	_, article, err := svc.CreateTicket(ctx, CreateTicketParams{
		Subject:     "Sanitize me",
		RequesterID: requester.ID,
		Article:     in,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !article.BodyHtml.Valid {
		t.Fatal("body_html unexpectedly NULL")
	}
	html := article.BodyHtml.String
	if want := "<p>hi</p>"; !strings.Contains(html, want) {
		t.Errorf("sanitized html %q lost benign markup %q", html, want)
	}
	for _, banned := range []string{"<script", "javascript:"} {
		if strings.Contains(html, banned) {
			t.Errorf("sanitized html %q still contains %q", html, banned)
		}
	}
}

// TestSearchTickets: FTS must find tickets by subject AND by article body.
func TestSearchTickets(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testPool)
	q := store.New(testPool)
	requester := createUser(t, store.UserRoleCustomer)

	bySubject, _, err := svc.CreateTicket(ctx, CreateTicketParams{
		Subject:     "Xerographic printer melting down",
		RequesterID: requester.ID,
		Article:     customerArticle(requester.ID, "please help quickly"),
	})
	if err != nil {
		t.Fatalf("create ticket 1: %v", err)
	}
	byBody, _, err := svc.CreateTicket(ctx, CreateTicketParams{
		Subject:     "VPN trouble",
		RequesterID: requester.ID,
		Article:     customerArticle(requester.ID, "the flux capacitor exploded during standup"),
	})
	if err != nil {
		t.Fatalf("create ticket 2: %v", err)
	}

	search := func(query string) []uuid.UUID {
		t.Helper()
		rows, err := q.SearchTickets(ctx, store.SearchTicketsParams{
			Query:     query,
			Statuses:  []string{},
			PageLimit: 10,
		})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.Ticket.ID
		}
		return ids
	}

	if got := search("xerographic"); !slices.Contains(got, bySubject.ID) {
		t.Errorf("search by subject: %v does not contain %s", got, bySubject.ID)
	}
	if got := search("flux capacitor"); !slices.Contains(got, byBody.ID) {
		t.Errorf("search by article body: %v does not contain %s", got, byBody.ID)
	}
	if got := search("xerographic"); slices.Contains(got, byBody.ID) {
		t.Errorf("search %q unexpectedly matched unrelated ticket", "xerographic")
	}
	if got := search("zzzzunfindable"); len(got) != 0 {
		t.Errorf("nonsense query returned %v; want none", got)
	}
}

// TestNotifyReachesHub proves the whole realtime chain: service pg_notify
// (in-transaction) -> LISTEN connection -> Hub subscriber.
func TestNotifyReachesHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := events.NewHub()
	listener := events.NewListener(testDatabaseURL, hub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = listener.Run(ctx)
	}()
	defer func() { cancel(); <-done }()

	sub, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	svc := NewService(testPool)
	requester := createUser(t, store.UserRoleCustomer)

	// The LISTEN connection comes up asynchronously; keep creating tickets
	// until a notification lands (or time out).
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, _, err := svc.CreateTicket(ctx, CreateTicketParams{
			Subject:     "Realtime probe",
			RequesterID: requester.ID,
			Article:     customerArticle(requester.ID, "ping"),
		}); err != nil {
			t.Fatalf("create ticket: %v", err)
		}
		select {
		case payload := <-sub:
			// The listener publishes a {"type":"resync"} hint whenever it
			// (re)establishes LISTEN; skip those and wait for the real event.
			if strings.Contains(string(payload), `"resync"`) {
				continue
			}
			if !strings.Contains(string(payload), NotifyTicketCreated) {
				t.Fatalf("payload %q does not mention %q", payload, NotifyTicketCreated)
			}
			return
		case <-deadline:
			t.Fatal("no notification reached the hub within 10s")
		case <-tick.C:
		}
	}
}
