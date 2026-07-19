// M4 public web form: the single built-in, unauthenticated ticket intake at
// POST /public/tickets (channel=web). It has no form builder — one endpoint,
// defended by three layers:
//
//   - per-IP rate limit (429 when exceeded);
//   - a honeypot field (`_honeypot`) that real users leave empty — a
//     non-empty value is silently accepted (still 202) with NO ticket
//     created, so bots get no signal;
//   - an optional captcha verifier behind CaptchaVerifier, OFF by default
//     (NoopCaptcha). When an operator enables one, a missing/invalid token
//     (header X-Captcha-Token) is a 400.
//
// The requester is auto-created from (or matched to) the submitted email via
// EnsureUserByEmail, which only ever creates a CUSTOMER and returns an
// existing user unchanged — so the response is identical whether or not the
// email already had an account (never leak whether an email existed).
package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// Public-form per-IP rate limit: publicFormBurst immediate submissions, then
// one more every publicFormRefill.
const (
	publicFormBurst  = 5
	publicFormRefill = 12 * time.Second
)

// captchaTokenHeader carries the captcha response token when a verifier is
// enabled (the public-form JSON body has no captcha field by contract).
const captchaTokenHeader = "X-Captcha-Token"

// CaptchaVerifier gates public submissions behind a challenge. The default
// (NoopCaptcha) is disabled; an operator wires a real verifier via config.
type CaptchaVerifier interface {
	// Enabled reports whether verification is active. When false, the public
	// form accepts submissions without a token.
	Enabled() bool
	// Verify checks token (from the X-Captcha-Token header) for a submission
	// from remoteIP. A non-nil error rejects the submission (400).
	Verify(ctx context.Context, token, remoteIP string) error
}

// NoopCaptcha is the default: verification is off; every submission passes.
type NoopCaptcha struct{}

func (NoopCaptcha) Enabled() bool                                { return false }
func (NoopCaptcha) Verify(context.Context, string, string) error { return nil }

// SubmitPublicTicket implements POST /public/tickets (unauthenticated).
func (h *Handlers) SubmitPublicTicket(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !h.publicLimiter.Allow(ip) {
		w.Header().Set("Retry-After", "30")
		problem.Write(w, r, http.StatusTooManyRequests, "Too Many Requests", "too many submissions; retry later")
		return
	}

	var req api.SubmitPublicTicketJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}

	// Honeypot: a real (JS-driven) form leaves this hidden field empty. A
	// non-empty value is a bot — accept silently (202, no ticket) so it gets
	// no signal that it was filtered.
	if req.UnderscoreHoneypot != nil && strings.TrimSpace(*req.UnderscoreHoneypot) != "" {
		writeJSON(w, r, http.StatusAccepted, api.PublicTicketAccepted{})
		return
	}

	// Optional captcha (off by default).
	if h.captcha.Enabled() {
		token := strings.TrimSpace(r.Header.Get(captchaTokenHeader))
		if token == "" {
			badRequest(w, r, "captcha verification is required")
			return
		}
		if err := h.captcha.Verify(r.Context(), token, ip); err != nil {
			badRequest(w, r, "captcha verification failed")
			return
		}
	}

	if strings.TrimSpace(req.Subject) == "" {
		badRequest(w, r, "subject is required")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		badRequest(w, r, "body is required")
		return
	}
	email, err := auth.NormalizeEmail(string(req.Email))
	if err != nil {
		badRequest(w, r, "a valid email is required")
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		name = email
	}

	number, err := h.createPublicTicket(r.Context(), email, name, req.Subject, req.Body)
	if err != nil {
		serverError(w, r, "submit public ticket", err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, api.PublicTicketAccepted{TicketNumber: &number})
}

// createPublicTicket resolves (or creates) the requester and opens a
// web-channel ticket with the body as its first public article.
func (h *Handlers) createPublicTicket(ctx context.Context, email, name, subject, body string) (string, error) {
	// EnsureUserByEmail is a no-op self-upsert for an existing user (its role
	// is never changed) and creates a CUSTOMER on first contact — so an
	// unknown sender can never be auto-created as agent/admin, and the result
	// is identical whether or not the email already existed.
	user, err := h.q.EnsureUserByEmail(ctx, store.EnsureUserByEmailParams{Email: email, Name: name})
	if err != nil {
		return "", err
	}
	created, _, err := h.svc.CreateTicket(ctx, ticket.CreateTicketParams{
		Subject:     subject,
		RequesterID: user.ID,
		Article: ticket.ArticleInput{
			AuthorID:   &user.ID,
			SenderType: store.ArticleSenderCustomer,
			Channel:    store.ArticleChannelWeb,
			BodyText:   body,
		},
	})
	if err != nil {
		return "", err
	}
	return created.Number, nil
}
