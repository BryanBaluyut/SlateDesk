package handlers_test

import (
	"net/http"
	"testing"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// TestCannedReplyCRUDAuth: agents and admins can manage snippets; customers
// and anonymous callers cannot. The stored body keeps its raw {{...}}
// template (substitution is client-side).
func TestCannedReplyCRUDAuth(t *testing.T) {
	e := newEnv(t)
	agent := e.seedUser(store.UserRoleAgent)
	admin := e.seedUser(store.UserRoleAdmin)
	customer := e.seedUser(store.UserRoleCustomer)
	agentCookie := e.mustLogin(agent.Email, seedPassword)
	adminCookie := e.mustLogin(admin.Email, seedPassword)
	customerCookie := e.mustLogin(customer.Email, seedPassword)

	const rawBody = "Hi {{requester.name}}, ticket {{ticket.number}} is being worked on."

	// Agent creates; the body is stored verbatim (no server-side expansion).
	create := e.do(http.MethodPost, "/api/v1/canned-replies", agentCookie, map[string]any{
		"title": "Working on it",
		"body":  rawBody,
	})
	if create.status != http.StatusCreated {
		t.Fatalf("agent create = %d, want 201; body %s", create.status, create.body)
	}
	var created struct {
		Id   string `json:"id"`
		Body string `json:"body"`
	}
	e.decode(create, &created)
	if created.Body != rawBody {
		t.Fatalf("stored body was expanded/altered: %q", created.Body)
	}

	// Agent and admin can list.
	if res := e.do(http.MethodGet, "/api/v1/canned-replies", agentCookie, nil); res.status != http.StatusOK {
		t.Errorf("agent list = %d, want 200", res.status)
	}
	if res := e.do(http.MethodGet, "/api/v1/canned-replies", adminCookie, nil); res.status != http.StatusOK {
		t.Errorf("admin list = %d, want 200", res.status)
	}

	// Admin can update; omitted fields are unchanged.
	patch := e.do(http.MethodPatch, "/api/v1/canned-replies/"+created.Id, adminCookie, map[string]any{
		"title": "Renamed",
	})
	if patch.status != http.StatusOK {
		t.Fatalf("admin patch = %d; body %s", patch.status, patch.body)
	}
	var patched struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	e.decode(patch, &patched)
	if patched.Title != "Renamed" || patched.Body != rawBody {
		t.Fatalf("patch result = %+v, want title renamed & body unchanged", patched)
	}

	// Customers and anon are refused across the surface.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/canned-replies"},
		{http.MethodPost, "/api/v1/canned-replies"},
		{http.MethodPatch, "/api/v1/canned-replies/" + created.Id},
		{http.MethodDelete, "/api/v1/canned-replies/" + created.Id},
	} {
		if res := e.do(tc.method, tc.path, customerCookie, map[string]any{"title": "x", "body": "y"}); res.status != http.StatusForbidden {
			t.Errorf("customer %s %s = %d, want 403", tc.method, tc.path, res.status)
		}
		if res := e.do(tc.method, tc.path, nil, map[string]any{"title": "x", "body": "y"}); res.status != http.StatusUnauthorized {
			t.Errorf("anon %s %s = %d, want 401", tc.method, tc.path, res.status)
		}
	}

	// Agent deletes; second delete is 404.
	if res := e.do(http.MethodDelete, "/api/v1/canned-replies/"+created.Id, agentCookie, nil); res.status != http.StatusNoContent {
		t.Fatalf("agent delete = %d, want 204", res.status)
	}
	if res := e.do(http.MethodDelete, "/api/v1/canned-replies/"+created.Id, agentCookie, nil); res.status != http.StatusNotFound {
		t.Errorf("re-delete = %d, want 404", res.status)
	}
}

// TestCannedReplyValidation rejects empty title/body.
func TestCannedReplyValidation(t *testing.T) {
	e := newEnv(t)
	agent := e.seedUser(store.UserRoleAgent)
	cookie := e.mustLogin(agent.Email, seedPassword)

	for i, body := range []map[string]any{
		{"title": "", "body": "x"},
		{"title": "   ", "body": "x"},
		{"title": "ok", "body": ""},
		{"title": "ok", "body": "   "},
	} {
		if res := e.do(http.MethodPost, "/api/v1/canned-replies", cookie, body); res.status != http.StatusBadRequest {
			t.Errorf("case %d = %d, want 400; body %s", i, res.status, res.body)
		}
	}
}
