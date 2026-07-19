// Admin CRUD for outbound webhooks (M4), plus a synchronous test-ping and the
// delivery log. The per-endpoint HMAC signing secret is AES-GCM-encrypted at
// write (h.webhookBox, secrets.PurposeWebhookSecret) and returned in plaintext
// exactly once, at creation (WebhookCreated.signing_secret) — every later read
// (list, get-via-update, deliveries) omits it. The actual signed delivery
// happens in internal/webhook (Dispatcher enqueues off the ticket event; the
// River Worker POSTs with retry); this file reuses webhook.Deliver for the
// unrecorded test ping.
//
// Secret-egress rule (contract: "The signing secret is never included"): the
// only store.Webhook -> response conversion is webhookToAPI, which never reads
// SecretEnc; the sole plaintext secret in any response is the one the caller
// just supplied or that was freshly generated, echoed once from CreateWebhook.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/webhook"
)

// webhookTestTimeout bounds a synchronous POST /webhooks/{id}/test ping. It
// also sizes the shared h.webhookClient built in handlers.New.
const webhookTestTimeout = 10 * time.Second

// generatedSecretBytes is the entropy of an auto-generated signing secret
// (rendered as 64 hex chars).
const generatedSecretBytes = 32

// Webhook delivery-log page size bounds.
const (
	defaultWebhookDeliveriesLimit = 50
	maxWebhookDeliveriesLimit     = 200
)

// ListWebhooks returns all endpoints, newest first. Secrets never appear.
func (h *Handlers) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListWebhooks(r.Context())
	if err != nil {
		serverError(w, r, "list webhooks", err)
		return
	}
	out := make([]api.Webhook, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookToAPI(row))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// CreateWebhook registers an endpoint and returns the effective signing
// secret ONCE. A supplied secret is used as-is; otherwise one is generated.
func (h *Handlers) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req api.CreateWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxNameLen {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name must be 1–200 characters")
		return
	}
	endpoint, err := validateWebhookURL(req.Url)
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}
	events, err := validateWebhookEvents(req.Events)
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}

	var secret string
	if req.Secret != nil {
		secret = strings.TrimSpace(*req.Secret)
		if secret == "" {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "secret must not be empty when provided")
			return
		}
	} else {
		secret, err = generateWebhookSecret()
		if err != nil {
			serverError(w, r, "generate webhook secret", err)
			return
		}
	}
	enc, err := h.webhookBox.Encrypt([]byte(secret))
	if err != nil {
		serverError(w, r, "encrypt webhook secret", err)
		return
	}

	active := true
	if req.Active != nil {
		active = *req.Active
	}
	row, err := h.q.CreateWebhook(r.Context(), store.CreateWebhookParams{
		Name:      name,
		Url:       endpoint,
		SecretEnc: enc,
		Events:    events,
		Active:    active,
	})
	if err != nil {
		serverError(w, r, "create webhook", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, api.WebhookCreated{
		Id:            row.ID,
		Name:          row.Name,
		Url:           row.Url,
		Events:        eventsToAPI(row.Events),
		Active:        row.Active,
		SigningSecret: secret,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	})
}

// UpdateWebhook applies a partial update. Supplying `secret` rotates it; the
// new secret is NOT echoed (only creation reveals a secret).
func (h *Handlers) UpdateWebhook(w http.ResponseWriter, r *http.Request, id api.WebhookID) {
	existing, err := h.q.GetWebhook(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such webhook")
			return
		}
		serverError(w, r, "load webhook", err)
		return
	}
	var req api.UpdateWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	params := store.UpdateWebhookParams{
		ID:        id,
		Name:      existing.Name,
		Url:       existing.Url,
		SecretEnc: existing.SecretEnc,
		Events:    existing.Events,
		Active:    existing.Active,
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > maxNameLen {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name must be 1–200 characters")
			return
		}
		params.Name = name
	}
	if req.Url != nil {
		endpoint, err := validateWebhookURL(*req.Url)
		if err != nil {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
			return
		}
		params.Url = endpoint
	}
	if req.Events != nil {
		events, err := validateWebhookEvents(*req.Events)
		if err != nil {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
			return
		}
		params.Events = events
	}
	if req.Active != nil {
		params.Active = *req.Active
	}
	if req.Secret != nil {
		secret := strings.TrimSpace(*req.Secret)
		if secret == "" {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "secret must not be empty when provided")
			return
		}
		enc, err := h.webhookBox.Encrypt([]byte(secret))
		if err != nil {
			serverError(w, r, "encrypt webhook secret", err)
			return
		}
		params.SecretEnc = enc
	}
	updated, err := h.q.UpdateWebhook(r.Context(), params)
	if err != nil {
		serverError(w, r, "update webhook", err)
		return
	}
	writeJSON(w, r, http.StatusOK, webhookToAPI(updated))
}

// DeleteWebhook removes the endpoint and its delivery log (FK cascade).
func (h *Handlers) DeleteWebhook(w http.ResponseWriter, r *http.Request, id api.WebhookID) {
	n, err := h.q.DeleteWebhook(r.Context(), id)
	if err != nil {
		serverError(w, r, "delete webhook", err)
		return
	}
	if n == 0 {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such webhook")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TestWebhook synchronously POSTs a signed `ping` payload and reports the
// outcome. A non-2xx endpoint response is a 200 with ok=false — the ping was
// attempted; the result IS the payload. Not recorded in the delivery log.
func (h *Handlers) TestWebhook(w http.ResponseWriter, r *http.Request, id api.WebhookID) {
	wh, err := h.q.GetWebhook(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such webhook")
			return
		}
		serverError(w, r, "load webhook", err)
		return
	}
	secret, err := h.webhookBox.Decrypt(wh.SecretEnc)
	if err != nil {
		writeJSON(w, r, http.StatusOK, api.WebhookTestResult{
			Ok:     false,
			Detail: "signing secret could not be decrypted",
		})
		return
	}
	body, err := json.Marshal(map[string]any{
		"event":       "ping",
		"webhook_id":  wh.ID.String(),
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		serverError(w, r, "marshal ping payload", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), webhookTestTimeout)
	defer cancel()
	start := time.Now()
	// A synchronous test ping has no durable delivery row, so no dedupe id
	// (0 omits the X-SlateDesk-Delivery header).
	code, deliverErr := webhook.Deliver(ctx, h.webhookClient, wh.Url, 0, secret, body)
	latency := time.Since(start).Milliseconds()

	res := api.WebhookTestResult{LatencyMs: latency}
	if code != 0 {
		c := code
		res.ResponseCode = &c
	}
	if deliverErr == nil {
		res.Ok = true
		res.Detail = fmt.Sprintf("endpoint responded %d", code)
	} else {
		res.Ok = false
		res.Detail = truncateTestDetail(deliverErr.Error())
	}
	writeJSON(w, r, http.StatusOK, res)
}

// ListWebhookDeliveries returns the endpoint's recent delivery attempts,
// newest first — the durable outbox log.
func (h *Handlers) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request, id api.WebhookID, params api.ListWebhookDeliveriesParams) {
	if _, err := h.q.GetWebhook(r.Context(), id); err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such webhook")
			return
		}
		serverError(w, r, "load webhook", err)
		return
	}
	limit := defaultWebhookDeliveriesLimit
	if params.Limit != nil {
		limit = *params.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > maxWebhookDeliveriesLimit {
			limit = maxWebhookDeliveriesLimit
		}
	}
	rows, err := h.q.ListRecentWebhookDeliveries(r.Context(), store.ListRecentWebhookDeliveriesParams{
		WebhookID: id,
		Limit:     int32(limit),
	})
	if err != nil {
		serverError(w, r, "list webhook deliveries", err)
		return
	}
	out := make([]api.WebhookDelivery, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryToAPI(d))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// --- conversions & validation ------------------------------------------------

func webhookToAPI(wh store.Webhook) api.Webhook {
	return api.Webhook{
		Id:        wh.ID,
		Name:      wh.Name,
		Url:       wh.Url,
		Events:    eventsToAPI(wh.Events),
		Active:    wh.Active,
		CreatedAt: wh.CreatedAt,
		UpdatedAt: wh.UpdatedAt,
	}
}

func eventsToAPI(events []string) []api.WebhookEvent {
	out := make([]api.WebhookEvent, 0, len(events))
	for _, e := range events {
		out = append(out, api.WebhookEvent(e))
	}
	return out
}

func deliveryToAPI(d store.WebhookDelivery) api.WebhookDelivery {
	out := api.WebhookDelivery{
		Id:        d.ID,
		WebhookId: d.WebhookID,
		EventType: d.EventType,
		Status:    api.WebhookDeliveryStatus(d.Status),
		Attempts:  int(d.Attempts),
		CreatedAt: d.CreatedAt,
	}
	if len(d.Payload) > 0 {
		_ = json.Unmarshal(d.Payload, &out.Payload)
	}
	if d.TicketID.Valid {
		u := uuid.UUID(d.TicketID.Bytes)
		out.TicketId = &u
	}
	if d.ResponseCode.Valid {
		c := int(d.ResponseCode.Int32)
		out.ResponseCode = &c
	}
	if d.LastError.Valid {
		s := d.LastError.String
		out.LastError = &s
	}
	if d.DeliveredAt.Valid {
		t := d.DeliveredAt.Time
		out.DeliveredAt = &t
	}
	return out
}

func validateWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("url must be an absolute http(s) URL")
	}
	if u.Host == "" {
		return "", errors.New("url must include a host")
	}
	return raw, nil
}

func validateWebhookEvents(in []api.WebhookEvent) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("at least one event is required")
	}
	seen := make(map[api.WebhookEvent]bool, len(in))
	out := make([]string, 0, len(in))
	for _, e := range in {
		switch e {
		case api.WebhookEventTicketCreated, api.WebhookEventTicketUpdated, api.WebhookEventArticleCreated:
		default:
			return nil, fmt.Errorf("unknown event %q", e)
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, string(e))
	}
	return out, nil
}

func generateWebhookSecret() (string, error) {
	raw := make([]byte, generatedSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// truncateTestDetail caps the echoed test-ping error to the same bound as the
// mailbox connectivity tests (maxTestDetailLen, defined in mailboxes.go).
func truncateTestDetail(s string) string {
	if len(s) > maxTestDetailLen {
		return s[:maxTestDetailLen]
	}
	return s
}
