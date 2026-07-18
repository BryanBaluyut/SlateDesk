package handlers_test

// M2 ticket-core HTTP tests: auth matrix over the new endpoints, ticket
// lifecycle (create -> reply flips status -> patch -> audit trail),
// list filters + pagination totals, FTS via the API, tags, dashboard
// counters, attachment upload/download roundtrip, and SSE integration.
//
// The Postgres database is shared by every test in the package, so tests
// that assert list totals scope their queries (by team, unique assignee, or
// unique search tokens) instead of assuming an empty table.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// --- fixtures and helpers ----------------------------------------------------

type seedTicketOpts struct {
	priority store.TicketPriority
	assignee *uuid.UUID
	team     *uuid.UUID
	status   store.TicketStatus // "" = leave open
}

// seedTicket creates a ticket (and first article) through the service, then
// applies optional assignee/status, bypassing the API under test.
func (e *env) seedTicket(requester store.User, subject, body string, opts seedTicketOpts) store.Ticket {
	e.t.Helper()
	ctx := context.Background()
	tk, _, err := e.svc.CreateTicket(ctx, ticket.CreateTicketParams{
		Subject:     subject,
		RequesterID: requester.ID,
		Priority:    opts.priority,
		AssigneeID:  opts.assignee,
		TeamID:      opts.team,
		Article: ticket.ArticleInput{
			AuthorID:   &requester.ID,
			SenderType: store.ArticleSenderAgent,
			Channel:    store.ArticleChannelWeb,
			BodyText:   body,
		},
	})
	if err != nil {
		e.t.Fatalf("seed ticket: %v", err)
	}
	if opts.status != "" && opts.status != store.TicketStatusOpen {
		if tk, err = e.svc.UpdateStatus(ctx, tk.ID, opts.status, nil); err != nil {
			e.t.Fatalf("seed ticket status: %v", err)
		}
	}
	return tk
}

func (e *env) seedTag(name string) store.Tag {
	e.t.Helper()
	tg, err := e.q.CreateTag(context.Background(), store.CreateTagParams{Name: name})
	if err != nil {
		e.t.Fatalf("seed tag: %v", err)
	}
	return tg
}

// upload POSTs one multipart file part named "file".
func (e *env) upload(path string, cookie *http.Cookie, filename, contentType string, content []byte) result {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	pw, err := mw.CreatePart(hdr)
	if err != nil {
		e.t.Fatalf("create multipart part: %v", err)
	}
	if _, err := pw.Write(content); err != nil {
		e.t.Fatalf("write multipart content: %v", err)
	}
	if err := mw.Close(); err != nil {
		e.t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, &buf)
	if err != nil {
		e.t.Fatalf("build upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("upload %s: %v", path, err)
	}
	defer res.Body.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(res.Body); err != nil {
		e.t.Fatalf("read upload response: %v", err)
	}
	return result{status: res.StatusCode, header: res.Header, body: raw.Bytes(), cookies: res.Cookies()}
}

// Response shapes (decoded loosely so the tests document the wire format).

type tagResp struct {
	Id    string  `json:"id"`
	Name  string  `json:"name"`
	Color *string `json:"color"`
}

type attachmentResp struct {
	Id          string `json:"id"`
	ArticleId   string `json:"article_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type articleResp struct {
	Id          string               `json:"id"`
	TicketId    string               `json:"ticket_id"`
	SenderType  string               `json:"sender_type"`
	Channel     string               `json:"channel"`
	IsInternal  bool                 `json:"is_internal"`
	BodyText    string               `json:"body_text"`
	BodyHtml    *string              `json:"body_html"`
	Author      *struct{ Id string } `json:"author"`
	Attachments []attachmentResp     `json:"attachments"`
}

type eventResp struct {
	Id      int64                  `json:"id"`
	Type    string                 `json:"type"`
	ActorId *string                `json:"actor_id"`
	Payload map[string]interface{} `json:"payload"`
}

type ticketResp struct {
	Id        string `json:"id"`
	Number    string `json:"number"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
	Priority  string `json:"priority"`
	Requester struct {
		Id    string `json:"id"`
		Email string `json:"email"`
	} `json:"requester"`
	Assignee *struct {
		Id string `json:"id"`
	} `json:"assignee"`
	TeamId   *string `json:"team_id"`
	ClosedAt *string `json:"closed_at"`

	// Detail-only fields (zero on list/update responses).
	Tags     []tagResp     `json:"tags"`
	Articles []articleResp `json:"articles"`
	Events   []eventResp   `json:"events"`
}

type ticketListResp struct {
	Items []ticketResp `json:"items"`
	Total int64        `json:"total"`
}

func (e *env) listTickets(cookie *http.Cookie, query string) ticketListResp {
	e.t.Helper()
	res := e.do(http.MethodGet, "/api/v1/tickets"+query, cookie, nil)
	if res.status != http.StatusOK {
		e.t.Fatalf("list tickets %q: status %d; body %s", query, res.status, res.body)
	}
	var list ticketListResp
	e.decode(res, &list)
	if list.Items == nil {
		e.t.Fatalf("list %q: items must be [], not null", query)
	}
	return list
}

// --- auth matrix -------------------------------------------------------------

// TestTicketCoreAuthMatrix exercises every M2 endpoint as anon, customer,
// agent, and admin: ticket core is agent+admin, DELETE /tags/{id} is
// admin-only, and rejections are problem+json.
func TestTicketCoreAuthMatrix(t *testing.T) {
	e := newEnv(t)
	adminU := e.seedUser(store.UserRoleAdmin)
	agentU := e.seedUser(store.UserRoleAgent)
	customerU := e.seedUser(store.UserRoleCustomer)

	cookies := map[string]*http.Cookie{
		"anon":     nil,
		"customer": e.mustLogin(customerU.Email, seedPassword),
		"agent":    e.mustLogin(agentU.Email, seedPassword),
		"admin":    e.mustLogin(adminU.Email, seedPassword),
	}
	principals := []string{"anon", "customer", "agent", "admin"}

	tk := e.seedTicket(customerU, uniqueName("matrix-ticket"), "matrix body", seedTicketOpts{})
	doomedTag := e.seedTag(uniqueName("matrix-doomed"))
	patchTag := e.seedTag(uniqueName("matrix-patch"))

	expect := func(anon, customer, agent, admin int) map[string]int {
		return map[string]int{"anon": anon, "customer": customer, "agent": agent, "admin": admin}
	}

	rows := []struct {
		name   string
		method string
		path   string
		body   func() any // evaluated per request so accepted calls stay unique
		want   map[string]int
	}{
		{"list tickets", http.MethodGet, "/api/v1/tickets", nil,
			expect(401, 403, 200, 200)},
		{"create ticket", http.MethodPost, "/api/v1/tickets",
			func() any { return map[string]any{"subject": uniqueName("matrix-create"), "body": "b"} },
			expect(401, 403, 201, 201)},
		{"get ticket", http.MethodGet, "/api/v1/tickets/" + tk.ID.String(), nil,
			expect(401, 403, 200, 200)},
		{"patch ticket", http.MethodPatch, "/api/v1/tickets/" + tk.ID.String(),
			func() any { return map[string]any{"priority": "high"} },
			expect(401, 403, 200, 200)},
		{"set ticket tags", http.MethodPut, "/api/v1/tickets/" + tk.ID.String() + "/tags",
			func() any { return map[string]any{"tag_ids": []string{}} },
			expect(401, 403, 200, 200)},
		{"create article", http.MethodPost, "/api/v1/tickets/" + tk.ID.String() + "/articles",
			func() any { return map[string]any{"body_text": "note", "is_internal": true} },
			expect(401, 403, 201, 201)},
		{"download attachment", http.MethodGet, "/api/v1/attachments/" + uuid.New().String(), nil,
			expect(401, 403, 404, 404)},
		{"list tags", http.MethodGet, "/api/v1/tags", nil,
			expect(401, 403, 200, 200)},
		{"create tag", http.MethodPost, "/api/v1/tags",
			func() any { return map[string]any{"name": uniqueName("matrix-tag")} },
			expect(401, 403, 201, 201)},
		{"patch tag", http.MethodPatch, "/api/v1/tags/" + patchTag.ID.String(),
			func() any { return map[string]any{"color": "#123456"} },
			expect(401, 403, 200, 200)},
		{"delete tag (admin only)", http.MethodDelete, "/api/v1/tags/" + doomedTag.ID.String(), nil,
			expect(401, 403, 403, 204)},
		{"dashboard counters", http.MethodGet, "/api/v1/dashboard/counters", nil,
			expect(401, 403, 200, 200)},
	}

	for _, row := range rows {
		for _, principal := range principals {
			t.Run(row.name+"/"+principal, func(t *testing.T) {
				var body any
				if row.body != nil {
					body = row.body()
				}
				res := e.do(row.method, row.path, cookies[principal], body)
				want := row.want[principal]
				if res.status != want {
					t.Fatalf("%s %s as %s: status %d, want %d; body %s",
						row.method, row.path, principal, res.status, want, res.body)
				}
				if want == 401 || want == 403 {
					if ct := res.header.Get("Content-Type"); ct != "application/problem+json" {
						t.Fatalf("rejection Content-Type = %q, want application/problem+json", ct)
					}
				}
			})
		}
	}

	// Binding failures on ticket-core routes stay behind the auth policy.
	t.Run("binding error still requires auth", func(t *testing.T) {
		if res := e.do(http.MethodGet, "/api/v1/tickets/not-a-uuid", nil, nil); res.status != http.StatusUnauthorized {
			t.Fatalf("anon bad uuid: status %d, want 401", res.status)
		}
		if res := e.do(http.MethodGet, "/api/v1/tickets/not-a-uuid", cookies["customer"], nil); res.status != http.StatusForbidden {
			t.Fatalf("customer bad uuid: status %d, want 403", res.status)
		}
		if res := e.do(http.MethodGet, "/api/v1/tickets/not-a-uuid", cookies["agent"], nil); res.status != http.StatusBadRequest {
			t.Fatalf("agent bad uuid: status %d, want 400", res.status)
		}
	})
}

// --- lifecycle ---------------------------------------------------------------

func TestTicketLifecycle(t *testing.T) {
	e := newEnv(t)
	agentU := e.seedUser(store.UserRoleAgent)
	agent := e.mustLogin(agentU.Email, seedPassword)
	customerU := e.seedUser(store.UserRoleCustomer)
	team := e.seedTeam()

	// Create: number allocated, first article recorded, created event.
	subject := uniqueName("Printer on fire")
	res := e.do(http.MethodPost, "/api/v1/tickets", agent, map[string]any{
		"subject":      subject,
		"body":         "It is genuinely on fire.",
		"priority":     "high",
		"requester_id": customerU.ID.String(),
		"team_id":      team.ID.String(),
	})
	if res.status != http.StatusCreated {
		t.Fatalf("create: status %d; body %s", res.status, res.body)
	}
	var created ticketResp
	e.decode(res, &created)

	numberRe := regexp.MustCompile(`^\d{8}-\d{4,}$`)
	if !numberRe.MatchString(created.Number) {
		t.Fatalf("number %q does not match YYYYMMDD-NNNN", created.Number)
	}
	if !strings.HasPrefix(created.Number, time.Now().UTC().Format("20060102")) {
		t.Fatalf("number %q not prefixed with today's UTC date", created.Number)
	}
	if created.Status != "open" || created.Priority != "high" {
		t.Fatalf("created status/priority = %s/%s", created.Status, created.Priority)
	}
	if created.Requester.Id != customerU.ID.String() {
		t.Fatalf("requester = %s, want %s", created.Requester.Id, customerU.ID)
	}
	if created.TeamId == nil || *created.TeamId != team.ID.String() {
		t.Fatalf("team_id = %v, want %s", created.TeamId, team.ID)
	}
	if created.Assignee != nil || created.ClosedAt != nil {
		t.Fatalf("new ticket must be unassigned and not closed: %s", res.body)
	}
	if len(created.Articles) != 1 || created.Articles[0].SenderType != "agent" ||
		created.Articles[0].Channel != "web" || created.Articles[0].IsInternal {
		t.Fatalf("first article mismatch: %s", res.body)
	}
	if created.Articles[0].Author == nil || created.Articles[0].Author.Id != agentU.ID.String() {
		t.Fatalf("first article author mismatch: %s", res.body)
	}
	if len(created.Events) != 1 || created.Events[0].Type != "created" {
		t.Fatalf("created events mismatch: %s", res.body)
	}
	if len(created.Tags) != 0 {
		t.Fatalf("new ticket must have no tags: %s", res.body)
	}
	id := created.Id

	get := func() ticketResp {
		t.Helper()
		res := e.do(http.MethodGet, "/api/v1/tickets/"+id, agent, nil)
		if res.status != http.StatusOK {
			t.Fatalf("get ticket: status %d; body %s", res.status, res.body)
		}
		var d ticketResp
		e.decode(res, &d)
		return d
	}

	// Public agent reply: sanitized HTML stored, status flips open ->
	// waiting_on_customer, article_added + status_changed audited.
	res = e.do(http.MethodPost, "/api/v1/tickets/"+id+"/articles", agent, map[string]any{
		"body_text":   "Have you tried water?",
		"body_html":   `<script>alert(1)</script><b>Have you tried water?</b>`,
		"is_internal": false,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("reply: status %d; body %s", res.status, res.body)
	}
	var reply articleResp
	e.decode(res, &reply)
	if reply.BodyHtml == nil || strings.Contains(*reply.BodyHtml, "<script>") || !strings.Contains(*reply.BodyHtml, "<b>") {
		t.Fatalf("body_html not sanitized as expected: %v", reply.BodyHtml)
	}

	d := get()
	if d.Status != "waiting_on_customer" {
		t.Fatalf("status after public reply = %s, want waiting_on_customer", d.Status)
	}
	if len(d.Articles) != 2 {
		t.Fatalf("article count = %d, want 2", len(d.Articles))
	}
	wantTypes := []string{"created", "article_added", "status_changed"}
	if len(d.Events) != len(wantTypes) {
		t.Fatalf("events = %v", d.Events)
	}
	for i, want := range wantTypes {
		if d.Events[i].Type != want {
			t.Fatalf("event[%d] = %s, want %s (%v)", i, d.Events[i].Type, want, d.Events)
		}
	}
	if from, to := d.Events[2].Payload["from"], d.Events[2].Payload["to"]; from != "open" || to != "waiting_on_customer" {
		t.Fatalf("status_changed payload = %v", d.Events[2].Payload)
	}

	// Internal note: never changes status.
	res = e.do(http.MethodPost, "/api/v1/tickets/"+id+"/articles", agent, map[string]any{
		"body_text": "internal grumbling", "is_internal": true,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("internal note: status %d; body %s", res.status, res.body)
	}
	if d := get(); d.Status != "waiting_on_customer" {
		t.Fatalf("status after internal note = %s, want waiting_on_customer", d.Status)
	}

	// PATCH: assign, set priority; explicit team_id null clears the team.
	res = e.do(http.MethodPatch, "/api/v1/tickets/"+id, agent, map[string]any{
		"assignee_id": agentU.ID.String(),
		"priority":    "critical",
		"team_id":     nil,
	})
	if res.status != http.StatusOK {
		t.Fatalf("patch: status %d; body %s", res.status, res.body)
	}
	var patched ticketResp
	e.decode(res, &patched)
	if patched.Assignee == nil || patched.Assignee.Id != agentU.ID.String() {
		t.Fatalf("assignee after patch = %v", patched.Assignee)
	}
	if patched.Priority != "critical" || patched.TeamId != nil {
		t.Fatalf("patch result mismatch: %s", res.body)
	}

	// PATCH: explicit assignee_id null clears the assignee.
	res = e.do(http.MethodPatch, "/api/v1/tickets/"+id, agent, map[string]any{"assignee_id": nil})
	if res.status != http.StatusOK {
		t.Fatalf("clear assignee: status %d; body %s", res.status, res.body)
	}
	e.decode(res, &patched)
	if patched.Assignee != nil {
		t.Fatalf("assignee not cleared: %s", res.body)
	}

	// Close stamps closed_at; reopen (status=open) clears it.
	res = e.do(http.MethodPatch, "/api/v1/tickets/"+id, agent, map[string]any{"status": "closed"})
	if res.status != http.StatusOK {
		t.Fatalf("close: status %d; body %s", res.status, res.body)
	}
	e.decode(res, &patched)
	if patched.Status != "closed" || patched.ClosedAt == nil {
		t.Fatalf("close result mismatch: %s", res.body)
	}
	res = e.do(http.MethodPatch, "/api/v1/tickets/"+id, agent, map[string]any{"status": "open"})
	if res.status != http.StatusOK {
		t.Fatalf("reopen: status %d; body %s", res.status, res.body)
	}
	e.decode(res, &patched)
	if patched.Status != "open" || patched.ClosedAt != nil {
		t.Fatalf("reopen result mismatch: %s", res.body)
	}

	// The audit trail recorded every mutation, in order.
	d = get()
	gotTypes := make([]string, 0, len(d.Events))
	for _, ev := range d.Events {
		gotTypes = append(gotTypes, ev.Type)
	}
	wantTrail := []string{
		"created", "article_added", "status_changed", // create + reply
		"article_added",                                        // internal note
		"assignee_changed", "priority_changed", "team_changed", // patch 1 (status first, then priority, assignee, team order — see below)
		"assignee_changed",                 // clear assignee
		"status_changed", "status_changed", // close + reopen
	}
	// Patch 1 applies in handler order: status (absent), priority, assignee, team.
	wantTrail[4], wantTrail[5], wantTrail[6] = "priority_changed", "assignee_changed", "team_changed"
	if strings.Join(gotTypes, ",") != strings.Join(wantTrail, ",") {
		t.Fatalf("audit trail = %v, want %v", gotTypes, wantTrail)
	}

	// An empty patch is a no-op that returns the current ticket.
	res = e.do(http.MethodPatch, "/api/v1/tickets/"+id, agent, map[string]any{})
	if res.status != http.StatusOK {
		t.Fatalf("empty patch: status %d; body %s", res.status, res.body)
	}

	// Validation and not-found behavior.
	t.Run("validation", func(t *testing.T) {
		missing := uuid.New().String()
		bad := []struct {
			name   string
			method string
			path   string
			body   any
			want   int
		}{
			{"create empty subject", http.MethodPost, "/api/v1/tickets", map[string]any{"subject": "  ", "body": "b"}, 400},
			{"create empty body", http.MethodPost, "/api/v1/tickets", map[string]any{"subject": "s", "body": ""}, 400},
			{"create bad priority", http.MethodPost, "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "priority": "urgent"}, 400},
			{"create unknown requester", http.MethodPost, "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "requester_id": missing}, 400},
			{"create unknown team", http.MethodPost, "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "team_id": missing}, 400},
			{"patch bad status", http.MethodPatch, "/api/v1/tickets/" + id, map[string]any{"status": "solved"}, 400},
			{"patch null status", http.MethodPatch, "/api/v1/tickets/" + id, map[string]any{"status": nil}, 400},
			{"patch unknown field", http.MethodPatch, "/api/v1/tickets/" + id, map[string]any{"subject": "nope"}, 400},
			{"patch unknown assignee", http.MethodPatch, "/api/v1/tickets/" + id, map[string]any{"assignee_id": missing}, 400},
			{"patch customer assignee", http.MethodPatch, "/api/v1/tickets/" + id, map[string]any{"assignee_id": customerU.ID.String()}, 400},
			{"patch unknown team", http.MethodPatch, "/api/v1/tickets/" + id, map[string]any{"team_id": missing}, 400},
			{"patch missing ticket", http.MethodPatch, "/api/v1/tickets/" + missing, map[string]any{"priority": "low"}, 404},
			{"get missing ticket", http.MethodGet, "/api/v1/tickets/" + missing, nil, 404},
			{"article empty body_text", http.MethodPost, "/api/v1/tickets/" + id + "/articles", map[string]any{"body_text": " ", "is_internal": false}, 400},
			{"article missing ticket", http.MethodPost, "/api/v1/tickets/" + missing + "/articles", map[string]any{"body_text": "x", "is_internal": false}, 404},
		}
		for _, tt := range bad {
			t.Run(tt.name, func(t *testing.T) {
				res := e.do(tt.method, tt.path, agent, tt.body)
				if res.status != tt.want {
					t.Fatalf("status %d, want %d; body %s", res.status, tt.want, res.body)
				}
			})
		}
	})
}

// --- list filters, views, pagination -----------------------------------------

func TestTicketListFiltersAndPagination(t *testing.T) {
	e := newEnv(t)
	a1 := e.seedUser(store.UserRoleAgent)
	a2 := e.seedUser(store.UserRoleAgent)
	customerU := e.seedUser(store.UserRoleCustomer)
	team := e.seedTeam()
	agent := e.mustLogin(a1.Email, seedPassword)

	teamScope := "?team_id=" + team.ID.String()
	opts := func(mod func(*seedTicketOpts)) seedTicketOpts {
		o := seedTicketOpts{team: &team.ID}
		mod(&o)
		return o
	}

	t1 := e.seedTicket(customerU, uniqueName("list-t1"), "body one", opts(func(o *seedTicketOpts) {
		o.assignee = &a1.ID
		o.priority = store.TicketPriorityLow
	}))
	t2 := e.seedTicket(customerU, uniqueName("list-t2"), "body two", opts(func(o *seedTicketOpts) {
		o.assignee = &a1.ID
		o.priority = store.TicketPriorityHigh
		o.status = store.TicketStatusClosed
	}))
	t3 := e.seedTicket(customerU, uniqueName("list-t3"), "body three", opts(func(o *seedTicketOpts) {
		o.priority = store.TicketPriorityCritical
	}))
	t4 := e.seedTicket(customerU, uniqueName("list-t4"), "body four", opts(func(o *seedTicketOpts) {
		o.assignee = &a2.ID
		o.status = store.TicketStatusOnHold
	}))
	t5 := e.seedTicket(customerU, uniqueName("list-t5"), "body five", opts(func(o *seedTicketOpts) {
		o.priority = store.TicketPriorityHigh
		o.status = store.TicketStatusWaitingOnCustomer
	}))

	ids := func(list ticketListResp) map[string]bool {
		got := make(map[string]bool, len(list.Items))
		for _, item := range list.Items {
			got[item.Id] = true
		}
		return got
	}
	expectIDs := func(name string, list ticketListResp, want ...store.Ticket) {
		t.Helper()
		if int(list.Total) != len(want) || len(list.Items) != len(want) {
			t.Fatalf("%s: total=%d items=%d, want %d", name, list.Total, len(list.Items), len(want))
		}
		got := ids(list)
		for _, w := range want {
			if !got[w.ID.String()] {
				t.Fatalf("%s: missing %s (%s); got %v", name, w.Number, w.ID, got)
			}
		}
	}

	expectIDs("team scope", e.listTickets(agent, teamScope), t1, t2, t3, t4, t5)

	// Pagination: totals are pre-LIMIT; a page past the end still reports
	// the true total.
	page := e.listTickets(agent, teamScope+"&limit=2")
	if len(page.Items) != 2 || page.Total != 5 {
		t.Fatalf("limit=2: items=%d total=%d", len(page.Items), page.Total)
	}
	page = e.listTickets(agent, teamScope+"&limit=2&offset=4")
	if len(page.Items) != 1 || page.Total != 5 {
		t.Fatalf("offset=4: items=%d total=%d", len(page.Items), page.Total)
	}
	page = e.listTickets(agent, teamScope+"&limit=2&offset=50")
	if len(page.Items) != 0 || page.Total != 5 {
		t.Fatalf("offset past end: items=%d total=%d, want 0/5", len(page.Items), page.Total)
	}

	// Ordering: most recent activity first. Touch t1 via a new article.
	if _, err := e.svc.AddArticle(context.Background(), t1.ID, ticket.ArticleInput{
		AuthorID: &a1.ID, SenderType: store.ArticleSenderAgent, Channel: store.ArticleChannelWeb,
		IsInternal: true, BodyText: "bump",
	}); err != nil {
		t.Fatalf("bump t1: %v", err)
	}
	if list := e.listTickets(agent, teamScope); list.Items[0].Id != t1.ID.String() {
		t.Fatalf("expected t1 first after activity; got %s", list.Items[0].Id)
	}

	// Fixed views.
	expectIDs("view=my", e.listTickets(agent, "?view=my"), t1) // a1's non-closed only
	expectIDs("view=my same assignee", e.listTickets(agent, "?view=my&assignee_id="+a1.ID.String()), t1)
	expectIDs("view=my conflicting assignee", e.listTickets(agent, "?view=my&assignee_id="+a2.ID.String()))
	expectIDs("view=unassigned", e.listTickets(agent, "?view=unassigned&team_id="+team.ID.String()), t3, t5)
	expectIDs("view=open", e.listTickets(agent, "?view=open&team_id="+team.ID.String()), t1, t3, t4, t5)
	expectIDs("view=closed", e.listTickets(agent, "?view=closed&team_id="+team.ID.String()), t2)
	expectIDs("view+status contradiction", e.listTickets(agent, "?view=closed&status=open&team_id="+team.ID.String()))

	// Explicit filters.
	expectIDs("status=on_hold", e.listTickets(agent, teamScope+"&status=on_hold"), t4)
	expectIDs("two statuses", e.listTickets(agent, teamScope+"&status=open&status=waiting_on_customer"), t1, t3, t5)
	expectIDs("priority=high", e.listTickets(agent, teamScope+"&priority=high"), t2, t5)
	expectIDs("priority+view", e.listTickets(agent, "?view=open&priority=high&team_id="+team.ID.String()), t5)
	expectIDs("assignee_id", e.listTickets(agent, teamScope+"&assignee_id="+a1.ID.String()), t1, t2)

	// Tag filter.
	tg := e.seedTag(uniqueName("list-tag"))
	if res := e.do(http.MethodPut, "/api/v1/tickets/"+t3.ID.String()+"/tags", agent,
		map[string]any{"tag_ids": []string{tg.ID.String()}}); res.status != http.StatusOK {
		t.Fatalf("tag t3: status %d; body %s", res.status, res.body)
	}
	expectIDs("tag_id", e.listTickets(agent, teamScope+"&tag_id="+tg.ID.String()), t3)

	// Invalid parameters are 400 problems.
	for _, bad := range []string{
		"?view=bogus", "?status=bogus", "?priority=urgent",
		"?limit=0", "?limit=201", "?offset=-1",
	} {
		if res := e.do(http.MethodGet, "/api/v1/tickets"+bad, agent, nil); res.status != http.StatusBadRequest {
			t.Fatalf("list %q: status %d, want 400; body %s", bad, res.status, res.body)
		}
	}
}

// --- full-text search --------------------------------------------------------

func TestTicketSearchAPI(t *testing.T) {
	e := newEnv(t)
	agentU := e.seedUser(store.UserRoleAgent)
	agent := e.mustLogin(agentU.Email, seedPassword)
	customerU := e.seedUser(store.UserRoleCustomer)

	tokenA := fmt.Sprintf("xylograph%d", uniqueCounter.Add(1))
	tokenB := fmt.Sprintf("quokka%d", uniqueCounter.Add(1))

	tk1 := e.seedTicket(customerU, "Broken "+tokenA+" cable", "plain first body", seedTicketOpts{})
	tk2 := e.seedTicket(customerU, "Unrelated subject", "customer reports a "+tokenB+" outage", seedTicketOpts{})

	search := func(q string) ticketListResp {
		t.Helper()
		return e.listTickets(agent, "?q="+q)
	}

	// Subject match and article-body match.
	if list := search(tokenA); list.Total != 1 || list.Items[0].Id != tk1.ID.String() {
		t.Fatalf("q=%s: %+v", tokenA, list)
	}
	if list := search(tokenB); list.Total != 1 || list.Items[0].Id != tk2.ID.String() {
		t.Fatalf("q=%s: %+v", tokenB, list)
	}

	// A reply that mentions tokenB makes tk1 match too (bodies are
	// searched per article and deduplicated per ticket).
	res := e.do(http.MethodPost, "/api/v1/tickets/"+tk1.ID.String()+"/articles", agent, map[string]any{
		"body_text": "might be the same " + tokenB + " outage", "is_internal": false,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("reply: status %d; body %s", res.status, res.body)
	}
	if list := search(tokenB); list.Total != 2 {
		t.Fatalf("q=%s after reply: total %d, want 2", tokenB, list.Total)
	}

	// Search composes with views/filters and pagination totals survive.
	if list := e.listTickets(agent, "?q="+tokenB+"&view=closed"); list.Total != 0 {
		t.Fatalf("q+closed before closing: total %d", list.Total)
	}
	if _, err := e.svc.UpdateStatus(context.Background(), tk2.ID, store.TicketStatusClosed, nil); err != nil {
		t.Fatalf("close tk2: %v", err)
	}
	if list := e.listTickets(agent, "?q="+tokenB+"&view=closed"); list.Total != 1 || list.Items[0].Id != tk2.ID.String() {
		t.Fatalf("q+closed after closing: %+v", list)
	}
	if list := e.listTickets(agent, "?q="+tokenB+"&limit=1"); len(list.Items) != 1 || list.Total != 2 {
		t.Fatalf("q pagination: items=%d total=%d", len(list.Items), list.Total)
	}

	// No hits.
	if list := search("nosuchtokenzzz"); list.Total != 0 || len(list.Items) != 0 {
		t.Fatalf("no-hit search: %+v", list)
	}
}

// --- tags --------------------------------------------------------------------

func TestTagsAPI(t *testing.T) {
	e := newEnv(t)
	adminU := e.seedUser(store.UserRoleAdmin)
	admin := e.mustLogin(adminU.Email, seedPassword)
	agentU := e.seedUser(store.UserRoleAgent)
	agent := e.mustLogin(agentU.Email, seedPassword)
	customerU := e.seedUser(store.UserRoleCustomer)

	name := uniqueName("VIP")
	res := e.do(http.MethodPost, "/api/v1/tags", agent, map[string]any{"name": name, "color": "#ff0000"})
	if res.status != http.StatusCreated {
		t.Fatalf("create tag: status %d; body %s", res.status, res.body)
	}
	var created tagResp
	e.decode(res, &created)
	if created.Name != name || created.Color == nil || *created.Color != "#ff0000" {
		t.Fatalf("created tag mismatch: %s", res.body)
	}

	// citext: names are case-insensitively unique.
	if res := e.do(http.MethodPost, "/api/v1/tags", agent, map[string]any{"name": strings.ToUpper(name)}); res.status != http.StatusConflict {
		t.Fatalf("case-insensitive duplicate: status %d, want 409", res.status)
	}
	if res := e.do(http.MethodPost, "/api/v1/tags", agent, map[string]any{"name": "  "}); res.status != http.StatusBadRequest {
		t.Fatalf("blank name: status %d, want 400", res.status)
	}

	// PATCH: color null clears; name null rejected; rename conflicts 409.
	res = e.do(http.MethodPatch, "/api/v1/tags/"+created.Id, agent, map[string]any{"color": nil})
	if res.status != http.StatusOK {
		t.Fatalf("clear color: status %d; body %s", res.status, res.body)
	}
	var patched tagResp
	e.decode(res, &patched)
	if patched.Color != nil {
		t.Fatalf("color not cleared: %s", res.body)
	}
	if res := e.do(http.MethodPatch, "/api/v1/tags/"+created.Id, agent, map[string]any{"name": nil}); res.status != http.StatusBadRequest {
		t.Fatalf("null name: status %d, want 400", res.status)
	}
	other := e.seedTag(uniqueName("other"))
	if res := e.do(http.MethodPatch, "/api/v1/tags/"+created.Id, agent, map[string]any{"name": strings.ToUpper(other.Name)}); res.status != http.StatusConflict {
		t.Fatalf("rename conflict: status %d, want 409", res.status)
	}
	if res := e.do(http.MethodPatch, "/api/v1/tags/"+uuid.New().String(), agent, map[string]any{"name": uniqueName("x")}); res.status != http.StatusNotFound {
		t.Fatalf("patch missing: status %d, want 404", res.status)
	}

	// List contains both, sorted by name.
	res = e.do(http.MethodGet, "/api/v1/tags", agent, nil)
	if res.status != http.StatusOK {
		t.Fatalf("list tags: status %d", res.status)
	}
	var all []tagResp
	e.decode(res, &all)
	for i := 1; i < len(all); i++ {
		if strings.ToLower(all[i-1].Name) > strings.ToLower(all[i].Name) {
			t.Fatalf("tags not sorted by name: %q before %q", all[i-1].Name, all[i].Name)
		}
	}

	// Ticket tag assignment: replace-all semantics, unknown ids atomic 400.
	tk := e.seedTicket(customerU, uniqueName("tagged"), "body", seedTicketOpts{})
	put := func(ids []string) result {
		return e.do(http.MethodPut, "/api/v1/tickets/"+tk.ID.String()+"/tags", agent, map[string]any{"tag_ids": ids})
	}
	res = put([]string{created.Id, other.ID.String()})
	if res.status != http.StatusOK {
		t.Fatalf("put tags: status %d; body %s", res.status, res.body)
	}
	var tags []tagResp
	e.decode(res, &tags)
	if len(tags) != 2 {
		t.Fatalf("tags after put = %v", tags)
	}

	if res := put([]string{created.Id, uuid.New().String()}); res.status != http.StatusBadRequest {
		t.Fatalf("unknown tag id: status %d, want 400", res.status)
	}
	res = e.do(http.MethodGet, "/api/v1/tickets/"+tk.ID.String(), agent, nil)
	var d ticketResp
	e.decode(res, &d)
	if len(d.Tags) != 2 {
		t.Fatalf("failed PUT must not change tags; got %v", d.Tags)
	}

	if res := put([]string{}); res.status != http.StatusOK {
		t.Fatalf("clear tags: status %d", res.status)
	}
	if res := e.do(http.MethodPut, "/api/v1/tickets/"+tk.ID.String()+"/tags", agent, map[string]any{"tag_ids": nil}); res.status != http.StatusBadRequest {
		t.Fatalf("null tag_ids: status %d, want 400", res.status)
	}
	if res := e.do(http.MethodPut, "/api/v1/tickets/"+uuid.New().String()+"/tags", agent, map[string]any{"tag_ids": []string{}}); res.status != http.StatusNotFound {
		t.Fatalf("tags on missing ticket: status %d, want 404", res.status)
	}

	// DELETE cascades off tickets (and is admin-only, see auth matrix).
	if res := put([]string{created.Id}); res.status != http.StatusOK {
		t.Fatalf("re-tag: status %d", res.status)
	}
	if res := e.do(http.MethodDelete, "/api/v1/tags/"+created.Id, admin, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete tag: status %d", res.status)
	}
	if res := e.do(http.MethodDelete, "/api/v1/tags/"+created.Id, admin, nil); res.status != http.StatusNotFound {
		t.Fatalf("delete deleted tag: status %d, want 404", res.status)
	}
	res = e.do(http.MethodGet, "/api/v1/tickets/"+tk.ID.String(), agent, nil)
	e.decode(res, &d)
	if len(d.Tags) != 0 {
		t.Fatalf("deleted tag still on ticket: %v", d.Tags)
	}
}

// --- dashboard ---------------------------------------------------------------

func TestDashboardCounters(t *testing.T) {
	e := newEnv(t)
	agentU := e.seedUser(store.UserRoleAgent)
	agent := e.mustLogin(agentU.Email, seedPassword)
	customerU := e.seedUser(store.UserRoleCustomer)

	type counters struct {
		Open              int64 `json:"open"`
		Unassigned        int64 `json:"unassigned"`
		WaitingOnCustomer int64 `json:"waiting_on_customer"`
		ClosedToday       int64 `json:"closed_today"`
	}
	get := func() counters {
		t.Helper()
		res := e.do(http.MethodGet, "/api/v1/dashboard/counters", agent, nil)
		if res.status != http.StatusOK {
			t.Fatalf("counters: status %d; body %s", res.status, res.body)
		}
		var c counters
		e.decode(res, &c)
		return c
	}

	// The database is shared across tests, so assert deltas, not absolutes.
	before := get()
	e.seedTicket(customerU, uniqueName("dash-open"), "b", seedTicketOpts{}) // open + unassigned
	e.seedTicket(customerU, uniqueName("dash-waiting"), "b", seedTicketOpts{
		assignee: &agentU.ID, status: store.TicketStatusWaitingOnCustomer,
	})
	e.seedTicket(customerU, uniqueName("dash-closed"), "b", seedTicketOpts{
		assignee: &agentU.ID, status: store.TicketStatusClosed,
	})
	after := get()

	if d := after.Open - before.Open; d != 1 {
		t.Errorf("open delta = %d, want 1", d)
	}
	if d := after.Unassigned - before.Unassigned; d != 1 {
		t.Errorf("unassigned delta = %d, want 1", d)
	}
	if d := after.WaitingOnCustomer - before.WaitingOnCustomer; d != 1 {
		t.Errorf("waiting_on_customer delta = %d, want 1", d)
	}
	if d := after.ClosedToday - before.ClosedToday; d != 1 {
		t.Errorf("closed_today delta = %d, want 1", d)
	}

	// The "Closed today" dashboard tile deep-links GET /tickets with
	// closed_today=true; the list it opens must agree with the counter
	// (same predicate on both sides).
	res := e.do(http.MethodGet, "/api/v1/tickets?view=closed&closed_today=true&limit=200", agent, nil)
	if res.status != http.StatusOK {
		t.Fatalf("closed_today list: status %d; body %s", res.status, res.body)
	}
	var list struct {
		Items []ticketResp `json:"items"`
		Total int64        `json:"total"`
	}
	e.decode(res, &list)
	if list.Total != after.ClosedToday {
		t.Errorf("closed_today list total = %d, counter = %d — tile and queue disagree",
			list.Total, after.ClosedToday)
	}
}

// --- attachments -------------------------------------------------------------

func TestAttachmentRoundtrip(t *testing.T) {
	e := newEnv(t)
	agentU := e.seedUser(store.UserRoleAgent)
	agent := e.mustLogin(agentU.Email, seedPassword)
	customerU := e.seedUser(store.UserRoleCustomer)
	customer := e.mustLogin(customerU.Email, seedPassword)

	tk := e.seedTicket(customerU, uniqueName("attach"), "body", seedTicketOpts{})
	res := e.do(http.MethodGet, "/api/v1/tickets/"+tk.ID.String(), agent, nil)
	var d ticketResp
	e.decode(res, &d)
	articleID := d.Articles[0].Id
	uploadPath := "/api/v1/articles/" + articleID + "/attachments"

	content := []byte("id,severity\n1,on fire\n")
	const contentType = "text/csv; charset=utf-8"

	// Auth on the upload/download surface (not covered by the JSON matrix).
	if res := e.upload(uploadPath, nil, "a.csv", contentType, content); res.status != http.StatusUnauthorized {
		t.Fatalf("anon upload: status %d, want 401", res.status)
	}
	if res := e.upload(uploadPath, customer, "a.csv", contentType, content); res.status != http.StatusForbidden {
		t.Fatalf("customer upload: status %d, want 403", res.status)
	}

	// Path-traversal filenames are reduced to a safe base name.
	res = e.upload(uploadPath, agent, `..\..\evil\rep ort.csv`, contentType, content)
	if res.status != http.StatusCreated {
		t.Fatalf("upload: status %d; body %s", res.status, res.body)
	}
	var att attachmentResp
	e.decode(res, &att)
	if att.Filename != "rep ort.csv" {
		t.Fatalf("filename = %q, want sanitized \"rep ort.csv\"", att.Filename)
	}
	if att.ContentType != contentType {
		t.Fatalf("content_type = %q, want %q", att.ContentType, contentType)
	}
	if att.SizeBytes != int64(len(content)) {
		t.Fatalf("size_bytes = %d, want %d", att.SizeBytes, len(content))
	}
	if att.ArticleId != articleID {
		t.Fatalf("article_id = %s, want %s", att.ArticleId, articleID)
	}

	// The ticket detail carries the attachment on its article.
	res = e.do(http.MethodGet, "/api/v1/tickets/"+tk.ID.String(), agent, nil)
	e.decode(res, &d)
	if len(d.Articles[0].Attachments) != 1 || d.Articles[0].Attachments[0].Id != att.Id {
		t.Fatalf("detail attachments = %+v", d.Articles[0].Attachments)
	}

	// Download: exact bytes, preserved content type, forced download.
	dl := e.do(http.MethodGet, "/api/v1/attachments/"+att.Id, agent, nil)
	if dl.status != http.StatusOK {
		t.Fatalf("download: status %d; body %s", dl.status, dl.body)
	}
	if !bytes.Equal(dl.body, content) {
		t.Fatalf("downloaded bytes differ: %q", dl.body)
	}
	if ct := dl.header.Get("Content-Type"); ct != contentType {
		t.Fatalf("download Content-Type = %q, want %q", ct, contentType)
	}
	cd := dl.header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, `"rep ort.csv"`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if dl.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff header")
	}

	t.Run("anon download unauthorized", func(t *testing.T) {
		if res := e.do(http.MethodGet, "/api/v1/attachments/"+att.Id, nil, nil); res.status != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", res.status)
		}
	})

	t.Run("missing content type defaults to octet-stream", func(t *testing.T) {
		res := e.upload(uploadPath, agent, "raw.bin", "", []byte{0x00, 0x01})
		if res.status != http.StatusCreated {
			t.Fatalf("upload: status %d; body %s", res.status, res.body)
		}
		var a2 attachmentResp
		e.decode(res, &a2)
		if a2.ContentType != "application/octet-stream" {
			t.Fatalf("content_type = %q", a2.ContentType)
		}
	})

	t.Run("unknown article 404", func(t *testing.T) {
		res := e.upload("/api/v1/articles/"+uuid.New().String()+"/attachments", agent, "a.txt", "text/plain", []byte("x"))
		if res.status != http.StatusNotFound {
			t.Fatalf("status %d, want 404; body %s", res.status, res.body)
		}
	})

	t.Run("unknown attachment 404", func(t *testing.T) {
		if res := e.do(http.MethodGet, "/api/v1/attachments/"+uuid.New().String(), agent, nil); res.status != http.StatusNotFound {
			t.Fatalf("status %d, want 404", res.status)
		}
	})

	t.Run("missing file part 400", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("note", "no file here"); err != nil {
			t.Fatalf("write field: %v", err)
		}
		_ = mw.Close()
		req, err := http.NewRequest(http.MethodPost, e.srv.URL+uploadPath, &buf)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.AddCookie(agent)
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", resp.StatusCode)
		}
	})

	t.Run("oversize upload is 413", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), (25<<20)+1) // one byte over the 25 MiB file cap
		res := e.upload(uploadPath, agent, "big.bin", "application/octet-stream", big)
		if res.status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413; body %s", res.status, res.body)
		}
		if ct := res.header.Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("413 Content-Type = %q", ct)
		}
		// Nothing was recorded for the article beyond the earlier uploads.
		atts, err := e.q.ListArticleAttachments(context.Background(), uuid.MustParse(articleID))
		if err != nil {
			t.Fatalf("list attachments: %v", err)
		}
		for _, a := range atts {
			if a.Filename == "big.bin" {
				t.Fatal("oversize upload must not create a row")
			}
		}
	})

	t.Run("exact-limit upload accepted", func(t *testing.T) {
		// The client advertises 25 MiB as acceptable; the multipart envelope
		// must not eat into the file budget (the request cap has slack).
		exact := bytes.Repeat([]byte("b"), 25<<20)
		res := e.upload(uploadPath, agent, "exact.bin", "application/octet-stream", exact)
		if res.status != http.StatusCreated {
			t.Fatalf("status %d, want 201; body %s", res.status, res.body)
		}
		var a attachmentResp
		e.decode(res, &a)
		if a.SizeBytes != int64(len(exact)) {
			t.Fatalf("size_bytes = %d, want %d", a.SizeBytes, len(exact))
		}
	})
}

// --- SSE ---------------------------------------------------------------------

func TestSSEStream(t *testing.T) {
	e := newEnv(t)
	agentU := e.seedUser(store.UserRoleAgent)
	agent := e.mustLogin(agentU.Email, seedPassword)
	customerU := e.seedUser(store.UserRoleCustomer)
	customer := e.mustLogin(customerU.Email, seedPassword)

	// Auth is enforced before the stream starts.
	if res := e.do(http.MethodGet, "/api/v1/events", nil, nil); res.status != http.StatusUnauthorized {
		t.Fatalf("anon events: status %d, want 401", res.status)
	}
	if res := e.do(http.MethodGet, "/api/v1/events", customer, nil); res.status != http.StatusForbidden {
		t.Fatalf("customer events: status %d, want 403", res.status)
	}

	// Open the authenticated stream.
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(agent)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q", ct)
	}

	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// The env's LISTEN connection comes up asynchronously; push pings
	// through Postgres until one is forwarded to the stream, proving the
	// pg_notify -> listener -> hub -> SSE pipe is live end to end.
	readyDeadline := time.After(10 * time.Second)
	pingPayload := `{"type":"test.ping","ticket_id":"` + uuid.New().String() + `"}`
	ready := false
	for !ready {
		if _, err := testPool.Exec(context.Background(),
			"SELECT pg_notify($1, $2)", events.Channel, pingPayload); err != nil {
			t.Fatalf("pg_notify ping: %v", err)
		}
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed while waiting for listener")
			}
			if strings.HasPrefix(line, "data: ") {
				ready = true
			}
		case <-time.After(100 * time.Millisecond):
		case <-readyDeadline:
			t.Fatal("LISTEN connection never became ready")
		}
	}

	// Create a ticket over the API from another goroutine; its
	// ticket.created event must arrive on the stream within 5 seconds.
	createdID := make(chan string, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{
			"subject": "sse ticket " + uuid.NewString(),
			"body":    "sse body",
		})
		req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/tickets", bytes.NewReader(body))
		if err != nil {
			createdID <- ""
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(agent)
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			createdID <- ""
			return
		}
		defer resp.Body.Close()
		var created struct {
			Id string `json:"id"`
		}
		if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&created) != nil {
			createdID <- ""
			return
		}
		createdID <- created.Id
	}()

	seen := map[string]bool{}
	wantID := ""
	timeout := time.After(5 * time.Second)
	for wantID == "" || !seen[wantID] {
		select {
		case id := <-createdID:
			if id == "" {
				t.Fatal("creating the ticket over the API failed")
			}
			wantID = id
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed early")
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Type     string `json:"type"`
				TicketId string `json:"ticket_id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatalf("bad event payload %q: %v", line, err)
			}
			if ev.Type == "ticket.created" {
				seen[ev.TicketId] = true
			}
		case <-timeout:
			t.Fatalf("ticket.created for %q not received within 5s", wantID)
		}
	}
}
