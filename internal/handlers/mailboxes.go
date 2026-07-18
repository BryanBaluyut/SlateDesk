// M3 mailbox admin surface: mailbox CRUD with credential encryption at
// write, live IMAP/SMTP connectivity tests, the Google OAuth connect flow,
// and the external-URL instance setting. Everything here is admin-only
// (authPolicy default) — see api/openapi.yaml.
//
// Credential egress rule (contract: "Mailbox responses never contain
// credentials"): the ONLY conversion from a store.Mailbox to a response
// body is toAPIMailbox, which never reads CredentialsEnc; error paths write
// problem details built from validation messages or scrubbed engine errors
// (scrubSecrets), so no secret material can reach a response on any path.
//
// scrubSecrets removes CREDENTIAL material only — the connectivity tests
// deliberately echo non-credential dial/protocol error text for arbitrary
// admin-supplied hosts, which permits SSRF-style internal probing by
// admins. That is inside the trust boundary by design: see the package doc
// of internal/email/diagnostics.go.
package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/email"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/settings"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// mailboxTestTimeout bounds the live test-fetch / test-send dials. The
// whole test (dial, TLS, auth incl. OAuth token minting, SELECT/SEND) must
// finish inside it.
const mailboxTestTimeout = 10 * time.Second

// maxTestDetailLen caps the detail string echoed from a failed
// connectivity test.
const maxTestDetailLen = 1000

// googleOauthStateTTL is how long a minted Google connect state token
// stays valid (one browser consent round-trip).
const googleOauthStateTTL = 10 * time.Minute

// googleOauthCallbackPath is the redirect_uri path under the external URL
// (must match the route in api/openapi.yaml).
const googleOauthCallbackPath = "/api/v1/mailboxes/oauth/google/callback"

// oauthStateMACPrefix domain-separates the state HMAC from session-cookie
// HMACs computed under the same instance secret: a session token can never
// verify as OAuth state, and vice versa.
const oauthStateMACPrefix = "slatedesk-google-oauth-state:"

// connectedRedirect is where the callback sends the admin's browser.
const connectedRedirect = "/settings/mailboxes?connected=1"

// ---------------------------------------------------------------------------
// Conversion (the single credential-egress choke point)

// toAPIMailbox converts a mailbox row for a response. CredentialsEnc is
// deliberately never read here — this is the only store.Mailbox -> API
// conversion, so no response can carry credential material.
func toAPIMailbox(mb store.Mailbox) api.Mailbox {
	out := api.Mailbox{
		Id:              mb.ID,
		Name:            mb.Name,
		EmailAddress:    openapi_types.Email(mb.EmailAddress),
		Active:          mb.Active,
		AuthKind:        api.MailboxAuthKind(mb.AuthKind),
		ImapHost:        mb.ImapHost,
		ImapPort:        int(mb.ImapPort),
		ImapTlsMode:     api.MailTLSMode(mb.ImapTlsMode),
		ImapUsername:    mb.ImapUsername,
		SmtpHost:        mb.SmtpHost,
		SmtpPort:        int(mb.SmtpPort),
		SmtpTlsMode:     api.MailTLSMode(mb.SmtpTlsMode),
		SmtpUsername:    mb.SmtpUsername,
		FromDisplayName: mb.FromDisplayName,
		Signature:       mb.Signature,
		AutoAckEnabled:  mb.AutoAckEnabled,
		CreatedAt:       mb.CreatedAt,
		UpdatedAt:       mb.UpdatedAt,
	}
	if mb.OauthTenantID.Valid {
		v := mb.OauthTenantID.String
		out.OauthTenantId = &v
	}
	if mb.OauthClientID.Valid {
		v := mb.OauthClientID.String
		out.OauthClientId = &v
	}
	if mb.LastPollAt.Valid {
		t := mb.LastPollAt.Time
		out.LastPollAt = &t
	}
	if mb.LastError.Valid {
		v := mb.LastError.String
		out.LastError = &v
	}
	if mb.LastErrorAt.Valid {
		t := mb.LastErrorAt.Time
		out.LastErrorAt = &t
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared validation

func badRequest(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, r, http.StatusBadRequest, "Bad Request", detail)
}

// validPort reports whether p is a usable TCP port.
func validPort(p int) bool { return p >= 1 && p <= 65535 }

// requireText trims and validates a required text field; empty -> 400.
func requireText(w http.ResponseWriter, r *http.Request, field, value string) (string, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		badRequest(w, r, field+" is required")
		return "", false
	}
	return v, true
}

// validateMailboxEmail normalizes the mailbox address (bare addr-spec,
// lowercased).
func validateMailboxEmail(w http.ResponseWriter, r *http.Request, value string) (string, bool) {
	normalized, err := auth.NormalizeEmail(value)
	if err != nil {
		badRequest(w, r, "invalid email_address")
		return "", false
	}
	return normalized, true
}

// validateTLSMode checks an api.MailTLSMode enum value.
func validateTLSMode(w http.ResponseWriter, r *http.Request, field string, m api.MailTLSMode) (store.MailTlsMode, bool) {
	if !m.Valid() {
		badRequest(w, r, fmt.Sprintf("invalid %s %q (want tls, starttls, or none)", field, m))
		return "", false
	}
	return store.MailTlsMode(m), true
}

// ---------------------------------------------------------------------------
// CRUD

// ListMailboxes implements GET /mailboxes (admin).
func (h *Handlers) ListMailboxes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListMailboxes(r.Context())
	if err != nil {
		serverError(w, r, "list mailboxes", err)
		return
	}
	out := make([]api.Mailbox, 0, len(rows))
	for _, mb := range rows {
		out = append(out, toAPIMailbox(mb))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// GetMailbox implements GET /mailboxes/{id} (admin).
func (h *Handlers) GetMailbox(w http.ResponseWriter, r *http.Request, id api.MailboxID) {
	mb, err := h.q.GetMailbox(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "load mailbox", err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIMailbox(mb))
}

// CreateMailbox implements POST /mailboxes (admin). The request body is
// discriminated on auth_kind; each kind's write-only credential fields are
// validated, encrypted (AES-GCM via the secrets box), and stored — they
// never appear in the response.
func (h *Handlers) CreateMailbox(w http.ResponseWriter, r *http.Request) {
	var body api.CreateMailboxJSONRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	raw, err := body.MarshalJSON() // the stored union bytes
	if err != nil {
		serverError(w, r, "reread mailbox payload", err)
		return
	}
	kind, err := body.Discriminator()
	if err != nil {
		badRequest(w, r, "invalid JSON body: "+err.Error())
		return
	}

	params := store.CreateMailboxParams{}
	var creds email.Credentials

	// strictDecode re-decodes the raw payload into the variant struct with
	// unknown fields rejected (the union wrapper accepts anything).
	strictDecode := func(dst any) bool {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			badRequest(w, r, "invalid JSON body: "+err.Error())
			return false
		}
		return true
	}

	var cfg api.MailboxConfig
	switch api.MailboxAuthKind(kind) {
	case api.MailboxAuthKindBasic:
		var req api.CreateMailboxBasicRequest
		if !strictDecode(&req) {
			return
		}
		if req.Password == nil || *req.Password == "" {
			badRequest(w, r, "password is required for auth_kind basic")
			return
		}
		creds.Password = *req.Password
		params.AuthKind = store.MailboxAuthKindBasic
		cfg = basicToConfig(req)
	case api.MailboxAuthKindOauthM365:
		var req api.CreateMailboxOauthM365Request
		if !strictDecode(&req) {
			return
		}
		tenantID, clientID, ok := requireOauthIDs(w, r, req.TenantId, req.ClientId, true)
		if !ok {
			return
		}
		if req.ClientSecret == nil || *req.ClientSecret == "" {
			badRequest(w, r, "client_secret is required for auth_kind oauth_m365")
			return
		}
		creds.ClientSecret = *req.ClientSecret
		params.AuthKind = store.MailboxAuthKindOauthM365
		params.OauthTenantID = pgtype.Text{String: tenantID, Valid: true}
		params.OauthClientID = pgtype.Text{String: clientID, Valid: true}
		cfg = m365ToConfig(req)
	case api.MailboxAuthKindOauthGoogle:
		var req api.CreateMailboxOauthGoogleRequest
		if !strictDecode(&req) {
			return
		}
		_, clientID, ok := requireOauthIDs(w, r, nil, req.ClientId, false)
		if !ok {
			return
		}
		if req.ClientSecret == nil || *req.ClientSecret == "" {
			badRequest(w, r, "client_secret is required for auth_kind oauth_google")
			return
		}
		creds.ClientSecret = *req.ClientSecret
		params.AuthKind = store.MailboxAuthKindOauthGoogle
		params.OauthClientID = pgtype.Text{String: clientID, Valid: true}
		cfg = googleToConfig(req)
	default:
		badRequest(w, r, fmt.Sprintf("invalid auth_kind %q (want basic, oauth_m365, or oauth_google)", kind))
		return
	}

	if !applyConfigToCreate(w, r, cfg, &params) {
		return
	}

	enc, err := email.EncryptCredentials(h.box, creds)
	if err != nil {
		serverError(w, r, "encrypt mailbox credentials", err)
		return
	}
	params.CredentialsEnc = enc

	mb, err := h.q.CreateMailbox(r.Context(), params)
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a mailbox with this email address already exists")
			return
		}
		serverError(w, r, "create mailbox", err)
		return
	}
	h.kickMailboxes()
	writeJSON(w, r, http.StatusCreated, toAPIMailbox(mb))
}

// basicToConfig, m365ToConfig, and googleToConfig collapse the generated
// per-kind create variants onto the shared config shape. The variants
// repeat MailboxConfig's fields structurally (allOf), so this is a plain
// field copy.
func basicToConfig(r api.CreateMailboxBasicRequest) api.MailboxConfig {
	return api.MailboxConfig{
		Name: r.Name, EmailAddress: r.EmailAddress, Active: r.Active,
		ImapHost: r.ImapHost, ImapPort: r.ImapPort, ImapTlsMode: r.ImapTlsMode, ImapUsername: r.ImapUsername,
		SmtpHost: r.SmtpHost, SmtpPort: r.SmtpPort, SmtpTlsMode: r.SmtpTlsMode, SmtpUsername: r.SmtpUsername,
		FromDisplayName: r.FromDisplayName, Signature: r.Signature, AutoAckEnabled: r.AutoAckEnabled,
	}
}

func m365ToConfig(r api.CreateMailboxOauthM365Request) api.MailboxConfig {
	return api.MailboxConfig{
		Name: r.Name, EmailAddress: r.EmailAddress, Active: r.Active,
		ImapHost: r.ImapHost, ImapPort: r.ImapPort, ImapTlsMode: r.ImapTlsMode, ImapUsername: r.ImapUsername,
		SmtpHost: r.SmtpHost, SmtpPort: r.SmtpPort, SmtpTlsMode: r.SmtpTlsMode, SmtpUsername: r.SmtpUsername,
		FromDisplayName: r.FromDisplayName, Signature: r.Signature, AutoAckEnabled: r.AutoAckEnabled,
	}
}

func googleToConfig(r api.CreateMailboxOauthGoogleRequest) api.MailboxConfig {
	return api.MailboxConfig{
		Name: r.Name, EmailAddress: r.EmailAddress, Active: r.Active,
		ImapHost: r.ImapHost, ImapPort: r.ImapPort, ImapTlsMode: r.ImapTlsMode, ImapUsername: r.ImapUsername,
		SmtpHost: r.SmtpHost, SmtpPort: r.SmtpPort, SmtpTlsMode: r.SmtpTlsMode, SmtpUsername: r.SmtpUsername,
		FromDisplayName: r.FromDisplayName, Signature: r.Signature, AutoAckEnabled: r.AutoAckEnabled,
	}
}

// requireOauthIDs validates the tenant/client id inputs of the OAuth
// create variants (tenant only when wantTenant).
func requireOauthIDs(w http.ResponseWriter, r *http.Request, tenantID, clientID *string, wantTenant bool) (tenant, client string, ok bool) {
	if wantTenant {
		if tenantID == nil {
			badRequest(w, r, "tenant_id is required for auth_kind oauth_m365")
			return "", "", false
		}
		if tenant, ok = requireText(w, r, "tenant_id", *tenantID); !ok {
			return "", "", false
		}
	}
	if clientID == nil {
		badRequest(w, r, "client_id is required for this auth_kind")
		return "", "", false
	}
	if client, ok = requireText(w, r, "client_id", *clientID); !ok {
		return "", "", false
	}
	return tenant, client, true
}

// applyConfigToCreate validates the shared config fields and writes them
// into params. Defaults per the contract: active=true, imap_tls_mode=tls,
// smtp_tls_mode=starttls, from_display_name='', signature='',
// auto_ack_enabled=true.
func applyConfigToCreate(w http.ResponseWriter, r *http.Request, cfg api.MailboxConfig, params *store.CreateMailboxParams) bool {
	var ok bool
	if params.Name, ok = requireText(w, r, "name", cfg.Name); !ok {
		return false
	}
	if params.EmailAddress, ok = validateMailboxEmail(w, r, string(cfg.EmailAddress)); !ok {
		return false
	}
	if params.ImapHost, ok = requireText(w, r, "imap_host", cfg.ImapHost); !ok {
		return false
	}
	if params.ImapUsername, ok = requireText(w, r, "imap_username", cfg.ImapUsername); !ok {
		return false
	}
	if params.SmtpHost, ok = requireText(w, r, "smtp_host", cfg.SmtpHost); !ok {
		return false
	}
	if params.SmtpUsername, ok = requireText(w, r, "smtp_username", cfg.SmtpUsername); !ok {
		return false
	}
	if !validPort(cfg.ImapPort) {
		badRequest(w, r, "imap_port must be between 1 and 65535")
		return false
	}
	params.ImapPort = int32(cfg.ImapPort)
	if !validPort(cfg.SmtpPort) {
		badRequest(w, r, "smtp_port must be between 1 and 65535")
		return false
	}
	params.SmtpPort = int32(cfg.SmtpPort)

	params.ImapTlsMode = store.MailTlsModeTls
	if cfg.ImapTlsMode != nil {
		if params.ImapTlsMode, ok = validateTLSMode(w, r, "imap_tls_mode", *cfg.ImapTlsMode); !ok {
			return false
		}
	}
	params.SmtpTlsMode = store.MailTlsModeStarttls
	if cfg.SmtpTlsMode != nil {
		if params.SmtpTlsMode, ok = validateTLSMode(w, r, "smtp_tls_mode", *cfg.SmtpTlsMode); !ok {
			return false
		}
	}

	params.Active = cfg.Active == nil || *cfg.Active
	params.AutoAckEnabled = cfg.AutoAckEnabled == nil || *cfg.AutoAckEnabled
	if cfg.FromDisplayName != nil {
		params.FromDisplayName = strings.TrimSpace(*cfg.FromDisplayName)
	}
	if cfg.Signature != nil {
		params.Signature = *cfg.Signature
	}
	return true
}

// UpdateMailbox implements PATCH /mailboxes/{id} (admin): partial config
// update plus write-only credential rotation. Rotating any credential
// drops the cached OAuth access token; rotating an oauth_google client
// (or switching kinds) invalidates the stored refresh token. Changing
// auth_kind requires the new kind's credential fields in the same request.
func (h *Handlers) UpdateMailbox(w http.ResponseWriter, r *http.Request, id api.MailboxID) {
	var req api.UpdateMailboxJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}

	mb, err := h.q.GetMailbox(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "load mailbox", err)
		return
	}

	params := store.UpdateMailboxParams{
		ID:              mb.ID,
		Name:            mb.Name,
		EmailAddress:    mb.EmailAddress,
		Active:          mb.Active,
		AuthKind:        mb.AuthKind,
		ImapHost:        mb.ImapHost,
		ImapPort:        mb.ImapPort,
		ImapTlsMode:     mb.ImapTlsMode,
		ImapUsername:    mb.ImapUsername,
		SmtpHost:        mb.SmtpHost,
		SmtpPort:        mb.SmtpPort,
		SmtpTlsMode:     mb.SmtpTlsMode,
		SmtpUsername:    mb.SmtpUsername,
		CredentialsEnc:  mb.CredentialsEnc,
		OauthTenantID:   mb.OauthTenantID,
		OauthClientID:   mb.OauthClientID,
		FromDisplayName: mb.FromDisplayName,
		Signature:       mb.Signature,
		AutoAckEnabled:  mb.AutoAckEnabled,
	}

	var ok bool
	if req.Name != nil {
		if params.Name, ok = requireText(w, r, "name", *req.Name); !ok {
			return
		}
	}
	if req.EmailAddress != nil {
		if params.EmailAddress, ok = validateMailboxEmail(w, r, string(*req.EmailAddress)); !ok {
			return
		}
	}
	if req.Active != nil {
		params.Active = *req.Active
	}
	if req.ImapHost != nil {
		if params.ImapHost, ok = requireText(w, r, "imap_host", *req.ImapHost); !ok {
			return
		}
	}
	if req.ImapPort != nil {
		if !validPort(*req.ImapPort) {
			badRequest(w, r, "imap_port must be between 1 and 65535")
			return
		}
		params.ImapPort = int32(*req.ImapPort)
	}
	if req.ImapTlsMode != nil {
		if params.ImapTlsMode, ok = validateTLSMode(w, r, "imap_tls_mode", *req.ImapTlsMode); !ok {
			return
		}
	}
	if req.ImapUsername != nil {
		if params.ImapUsername, ok = requireText(w, r, "imap_username", *req.ImapUsername); !ok {
			return
		}
	}
	if req.SmtpHost != nil {
		if params.SmtpHost, ok = requireText(w, r, "smtp_host", *req.SmtpHost); !ok {
			return
		}
	}
	if req.SmtpPort != nil {
		if !validPort(*req.SmtpPort) {
			badRequest(w, r, "smtp_port must be between 1 and 65535")
			return
		}
		params.SmtpPort = int32(*req.SmtpPort)
	}
	if req.SmtpTlsMode != nil {
		if params.SmtpTlsMode, ok = validateTLSMode(w, r, "smtp_tls_mode", *req.SmtpTlsMode); !ok {
			return
		}
	}
	if req.SmtpUsername != nil {
		if params.SmtpUsername, ok = requireText(w, r, "smtp_username", *req.SmtpUsername); !ok {
			return
		}
	}
	if req.FromDisplayName != nil {
		params.FromDisplayName = strings.TrimSpace(*req.FromDisplayName)
	}
	if req.Signature != nil {
		params.Signature = *req.Signature
	}
	if req.AutoAckEnabled != nil {
		params.AutoAckEnabled = *req.AutoAckEnabled
	}

	// Resolve the final auth kind, then validate credential-field
	// applicability against it and apply the rotation rules.
	finalKind := mb.AuthKind
	kindChanged := false
	if req.AuthKind != nil {
		if !req.AuthKind.Valid() {
			badRequest(w, r, fmt.Sprintf("invalid auth_kind %q (want basic, oauth_m365, or oauth_google)", *req.AuthKind))
			return
		}
		finalKind = store.MailboxAuthKind(*req.AuthKind)
		kindChanged = finalKind != mb.AuthKind
	}
	params.AuthKind = finalKind

	// Reject credential fields that do not apply to the final kind — a
	// silently ignored secret is worse than a 400.
	if req.Password != nil && finalKind != store.MailboxAuthKindBasic {
		badRequest(w, r, "password only applies to auth_kind basic")
		return
	}
	if req.TenantId != nil && finalKind != store.MailboxAuthKindOauthM365 {
		badRequest(w, r, "tenant_id only applies to auth_kind oauth_m365")
		return
	}
	if (req.ClientId != nil || req.ClientSecret != nil) && finalKind == store.MailboxAuthKindBasic {
		badRequest(w, r, "client_id / client_secret only apply to the OAuth auth kinds")
		return
	}
	if req.Password != nil && *req.Password == "" {
		badRequest(w, r, "password must not be empty")
		return
	}
	if req.ClientSecret != nil && *req.ClientSecret == "" {
		badRequest(w, r, "client_secret must not be empty")
		return
	}

	creds, err := email.DecryptCredentials(h.box, mb.CredentialsEnc)
	if err != nil {
		serverError(w, r, "decrypt mailbox credentials", err)
		return
	}

	if kindChanged {
		// A kind switch starts from empty credentials: nothing from the
		// old kind (password, refresh token, cached access token) may
		// survive into the new one.
		creds = email.Credentials{}
		switch finalKind {
		case store.MailboxAuthKindBasic:
			if req.Password == nil {
				badRequest(w, r, "changing auth_kind to basic requires password")
				return
			}
			params.OauthTenantID = pgtype.Text{}
			params.OauthClientID = pgtype.Text{}
		case store.MailboxAuthKindOauthM365:
			if req.TenantId == nil || req.ClientId == nil || req.ClientSecret == nil {
				badRequest(w, r, "changing auth_kind to oauth_m365 requires tenant_id, client_id, and client_secret")
				return
			}
			params.OauthTenantID = pgtype.Text{}
			params.OauthClientID = pgtype.Text{}
		case store.MailboxAuthKindOauthGoogle:
			if req.ClientId == nil || req.ClientSecret == nil {
				badRequest(w, r, "changing auth_kind to oauth_google requires client_id and client_secret")
				return
			}
			params.OauthTenantID = pgtype.Text{}
			params.OauthClientID = pgtype.Text{}
		}
	}

	rotated := false
	if req.Password != nil {
		creds.Password = *req.Password
		rotated = true
	}
	if req.TenantId != nil {
		tenant, ok := requireText(w, r, "tenant_id", *req.TenantId)
		if !ok {
			return
		}
		params.OauthTenantID = pgtype.Text{String: tenant, Valid: true}
		rotated = true
	}
	if req.ClientId != nil {
		client, ok := requireText(w, r, "client_id", *req.ClientId)
		if !ok {
			return
		}
		params.OauthClientID = pgtype.Text{String: client, Valid: true}
		rotated = true
	}
	if req.ClientSecret != nil {
		creds.ClientSecret = *req.ClientSecret
		rotated = true
	}

	if rotated {
		// Rotating any credential drops the cached OAuth access token.
		creds.AccessToken = ""
		creds.AccessTokenExpiry = time.Time{}
		// Rotating an oauth_google client invalidates the stored refresh
		// token (it was granted to the old client) — the connect flow
		// must be run again.
		if finalKind == store.MailboxAuthKindOauthGoogle && (req.ClientId != nil || req.ClientSecret != nil) {
			creds.RefreshToken = ""
		}
	}

	if rotated || kindChanged {
		enc, err := email.EncryptCredentials(h.box, creds)
		if err != nil {
			serverError(w, r, "encrypt mailbox credentials", err)
			return
		}
		params.CredentialsEnc = enc
	}

	updated, err := h.q.UpdateMailbox(r.Context(), params)
	if err != nil {
		if isUniqueViolation(err) {
			problem.Write(w, r, http.StatusConflict, "Conflict", "a mailbox with this email address already exists")
			return
		}
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "update mailbox", err)
		return
	}
	h.kickMailboxes()
	writeJSON(w, r, http.StatusOK, toAPIMailbox(updated))
}

// DeleteMailbox implements DELETE /mailboxes/{id} (admin). Hard delete;
// threading state survives (email_message_ids.mailbox_id is ON DELETE SET
// NULL) and the supervisor drops the runner + lease on the next (kicked)
// reconcile.
func (h *Handlers) DeleteMailbox(w http.ResponseWriter, r *http.Request, id api.MailboxID) {
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		if _, err := q.GetMailbox(r.Context(), id); err != nil {
			return err
		}
		return q.DeleteMailbox(r.Context(), id)
	})
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "delete mailbox", err)
		return
	}
	h.kickMailboxes()
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Live connectivity tests

// scrubSecrets removes credential material from a detail string: every
// non-empty secret currently stored for the mailbox is replaced. The
// engine's errors do not embed credentials by construction; this is
// defense in depth for server/library error text we do not control.
func scrubSecrets(detail string, creds email.Credentials) string {
	for _, secret := range []string{creds.Password, creds.ClientSecret, creds.RefreshToken, creds.AccessToken} {
		if secret == "" {
			continue
		}
		detail = strings.ReplaceAll(detail, secret, "[redacted]")
	}
	if len(detail) > maxTestDetailLen {
		detail = detail[:maxTestDetailLen] + "…"
	}
	return detail
}

// testCredentials decrypts the mailbox blob for scrubbing. Best-effort: on
// decrypt failure the zero value scrubs nothing, and the test itself will
// surface the (credential-free) decrypt error.
func (h *Handlers) testCredentials(mb store.Mailbox) email.Credentials {
	creds, err := email.DecryptCredentials(h.box, mb.CredentialsEnc)
	if err != nil {
		return email.Credentials{}
	}
	return creds
}

// TestMailboxFetch implements POST /mailboxes/{id}/test-fetch (admin): a
// live IMAP dial + auth + SELECT INBOX with the stored credentials. A
// failed test is 200 {ok:false} — the test ran; the outcome is the payload.
func (h *Handlers) TestMailboxFetch(w http.ResponseWriter, r *http.Request, id api.MailboxID) {
	if h.engine == nil {
		serverError(w, r, "test mailbox fetch", errors.New("email engine not configured"))
		return
	}
	mb, err := h.q.GetMailbox(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "load mailbox", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mailboxTestTimeout)
	defer cancel()
	start := time.Now()
	testErr := h.engine.TestFetch(ctx, mb)
	latency := time.Since(start).Milliseconds()

	res := api.MailboxTestResult{Ok: testErr == nil, LatencyMs: latency}
	if testErr != nil {
		res.Detail = scrubSecrets(testErr.Error(), h.testCredentials(mb))
	} else {
		res.Detail = fmt.Sprintf("IMAP connection OK: authenticated as %s and selected INBOX", mb.ImapUsername)
	}
	writeJSON(w, r, http.StatusOK, res)
}

// TestMailboxSend implements POST /mailboxes/{id}/test-send (admin): a
// live SMTP dial + auth sending a short loop-guarded test message. Same
// 200 {ok:false} semantics as test-fetch.
func (h *Handlers) TestMailboxSend(w http.ResponseWriter, r *http.Request, id api.MailboxID) {
	if h.engine == nil {
		serverError(w, r, "test mailbox send", errors.New("email engine not configured"))
		return
	}
	var req api.TestMailboxSendJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	to, err := auth.NormalizeEmail(string(req.To))
	if err != nil {
		badRequest(w, r, "invalid to address")
		return
	}
	mb, err := h.q.GetMailbox(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "load mailbox", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mailboxTestTimeout)
	defer cancel()
	start := time.Now()
	testErr := h.engine.TestSend(ctx, mb, to)
	latency := time.Since(start).Milliseconds()

	res := api.MailboxTestResult{Ok: testErr == nil, LatencyMs: latency}
	if testErr != nil {
		res.Detail = scrubSecrets(testErr.Error(), h.testCredentials(mb))
	} else {
		res.Detail = fmt.Sprintf("SMTP send OK: test message delivered to %s", to)
	}
	writeJSON(w, r, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// Google OAuth connect flow

// googleOauthState is the payload of the signed state parameter: bound to
// one mailbox, one initiating admin, and one single-use nonce.
type googleOauthState struct {
	MailboxID uuid.UUID `json:"mid"`
	UserID    uuid.UUID `json:"aid"`
	Nonce     string    `json:"n"`
	ExpiresAt int64     `json:"exp"` // unix seconds
}

// googleOauthNonceKey is the settings key persisting the outstanding nonce
// for one mailbox's connect flow (single-use: consumed by the callback;
// starting a new flow supersedes the previous one).
func googleOauthNonceKey(mailboxID uuid.UUID) string {
	return "google_oauth_nonce:" + mailboxID.String()
}

// signOauthState serializes and MACs a state token under the instance
// secret, domain-separated from session cookies.
func (h *Handlers) signOauthState(s googleOauthState) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode oauth state: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + h.oauthStateMAC(encoded), nil
}

// verifyOauthState validates signature and expiry. Failures carry no
// detail by design.
func (h *Handlers) verifyOauthState(token string) (googleOauthState, bool) {
	encoded, mac, ok := strings.Cut(token, ".")
	if !ok {
		return googleOauthState{}, false
	}
	if !hmac.Equal([]byte(h.oauthStateMAC(encoded)), []byte(mac)) {
		return googleOauthState{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return googleOauthState{}, false
	}
	var s googleOauthState
	if err := json.Unmarshal(payload, &s); err != nil {
		return googleOauthState{}, false
	}
	if s.Nonce == "" || time.Now().Unix() >= s.ExpiresAt {
		return googleOauthState{}, false
	}
	return s, true
}

func (h *Handlers) oauthStateMAC(encodedPayload string) string {
	mac := hmac.New(sha256.New, h.secret)
	mac.Write([]byte(oauthStateMACPrefix + encodedPayload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// externalURL reads the external-url setting ('' when unset).
func (h *Handlers) externalURL(ctx context.Context) (string, error) {
	var s string
	if err := h.settings.Get(ctx, settings.KeyExternalURL, &s); err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return s, nil
}

// StartMailboxGoogleOauth implements POST /mailboxes/{id}/oauth/google/start
// (admin): mints the signed single-use state, persists its nonce, and
// returns the Google authorization URL (offline access + consent prompt,
// so the exchange yields a refresh token).
func (h *Handlers) StartMailboxGoogleOauth(w http.ResponseWriter, r *http.Request, id api.MailboxID) {
	if h.engine == nil {
		serverError(w, r, "start google oauth", errors.New("email engine not configured"))
		return
	}
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	mb, err := h.q.GetMailbox(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "load mailbox", err)
		return
	}
	if mb.AuthKind != store.MailboxAuthKindOauthGoogle {
		problem.Write(w, r, http.StatusConflict, "Conflict",
			"the Google connect flow only applies to oauth_google mailboxes")
		return
	}
	base, err := h.externalURL(r.Context())
	if err != nil {
		serverError(w, r, "load external url", err)
		return
	}
	if base == "" {
		problem.Write(w, r, http.StatusConflict, "Conflict",
			"the instance external URL is not configured (PUT /settings/external-url first)")
		return
	}

	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		serverError(w, r, "generate oauth nonce", err)
		return
	}
	st := googleOauthState{
		MailboxID: mb.ID,
		UserID:    caller.ID,
		Nonce:     hex.EncodeToString(nonceBytes[:]),
		ExpiresAt: time.Now().Add(googleOauthStateTTL).Unix(),
	}
	if err := h.settings.Set(r.Context(), googleOauthNonceKey(mb.ID), st.Nonce); err != nil {
		serverError(w, r, "persist oauth nonce", err)
		return
	}
	state, err := h.signOauthState(st)
	if err != nil {
		serverError(w, r, "sign oauth state", err)
		return
	}

	authURL, err := h.engine.Auth().GoogleAuthCodeURL(mb, base+googleOauthCallbackPath, state)
	if err != nil {
		// Config-shaped failure (e.g. missing client id); the message
		// carries no secrets by construction.
		problem.Write(w, r, http.StatusConflict, "Conflict", err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, api.GoogleOauthStart{AuthorizationUrl: authURL})
}

// MailboxGoogleOauthCallback implements GET /mailboxes/oauth/google/callback
// — the browser redirect target of the connect flow (admin-only like the
// rest of the surface: the SameSite=Lax session cookie rides on the
// top-level navigation). Validates the signed single-use state, exchanges
// the code, persists the refresh token into the mailbox's encrypted
// credentials, and bounces the browser back to the mailbox screen. Any
// validation or exchange failure is a 400 problem; nothing is persisted.
func (h *Handlers) MailboxGoogleOauthCallback(w http.ResponseWriter, r *http.Request, params api.MailboxGoogleOauthCallbackParams) {
	if h.engine == nil {
		serverError(w, r, "google oauth callback", errors.New("email engine not configured"))
		return
	}
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}

	st, ok := h.verifyOauthState(params.State)
	if !ok {
		badRequest(w, r, "invalid or expired state")
		return
	}
	if st.UserID != caller.ID {
		// The state is bound to the admin session that started the flow.
		badRequest(w, r, "state was issued to a different session")
		return
	}
	mb, err := h.q.GetMailbox(r.Context(), st.MailboxID)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such mailbox")
			return
		}
		serverError(w, r, "load mailbox", err)
		return
	}
	if mb.AuthKind != store.MailboxAuthKindOauthGoogle {
		badRequest(w, r, "mailbox is no longer an oauth_google mailbox")
		return
	}

	// Consume the nonce: a state token is valid exactly once, and only if
	// it is the most recently minted one for this mailbox.
	var storedNonce string
	if err := h.settings.Get(r.Context(), googleOauthNonceKey(mb.ID), &storedNonce); err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			badRequest(w, r, "state already used or superseded")
			return
		}
		serverError(w, r, "load oauth nonce", err)
		return
	}
	if !hmac.Equal([]byte(storedNonce), []byte(st.Nonce)) {
		badRequest(w, r, "state already used or superseded")
		return
	}
	if err := h.settings.Delete(r.Context(), googleOauthNonceKey(mb.ID)); err != nil {
		serverError(w, r, "consume oauth nonce", err)
		return
	}

	if params.Error != nil && *params.Error != "" {
		badRequest(w, r, "google authorization failed: "+*params.Error)
		return
	}
	if params.Code == nil || *params.Code == "" {
		badRequest(w, r, "missing authorization code")
		return
	}
	base, err := h.externalURL(r.Context())
	if err != nil {
		serverError(w, r, "load external url", err)
		return
	}
	if base == "" {
		badRequest(w, r, "the instance external URL is not configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mailboxTestTimeout)
	defer cancel()
	if err := h.engine.Auth().GoogleExchange(ctx, mb, *params.Code, base+googleOauthCallbackPath); err != nil {
		badRequest(w, r, scrubSecrets(err.Error(), h.testCredentials(mb)))
		return
	}

	// The refresh token landed via a credentials-only write that leaves
	// updated_at alone; bump it so a supervisor holding a stale row
	// restarts this mailbox's runner with the fresh credentials.
	if _, err := h.q.TouchMailbox(r.Context(), mb.ID); err != nil {
		serverError(w, r, "touch mailbox after oauth connect", err)
		return
	}
	h.kickMailboxes()

	http.Redirect(w, r, connectedRedirect, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Instance settings

// GetExternalUrl implements GET /settings/external-url (admin).
func (h *Handlers) GetExternalUrl(w http.ResponseWriter, r *http.Request) {
	base, err := h.externalURL(r.Context())
	if err != nil {
		serverError(w, r, "load external url", err)
		return
	}
	writeJSON(w, r, http.StatusOK, api.ExternalUrlSetting{ExternalUrl: base})
}

// SetExternalUrl implements PUT /settings/external-url (admin): an
// absolute http(s) URL with no path, query, fragment, or userinfo; a
// trailing slash is stripped. Idempotent.
func (h *Handlers) SetExternalUrl(w http.ResponseWriter, r *http.Request) {
	var req api.SetExternalUrlJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	normalized, ok := normalizeExternalURL(strings.TrimSpace(req.ExternalUrl))
	if !ok {
		badRequest(w, r, "external_url must be an absolute http(s) URL with no path, query, or fragment (e.g. https://desk.example.com)")
		return
	}
	if err := h.settings.Set(r.Context(), settings.KeyExternalURL, normalized); err != nil {
		serverError(w, r, "store external url", err)
		return
	}
	writeJSON(w, r, http.StatusOK, api.ExternalUrlSetting{ExternalUrl: normalized})
}

// normalizeExternalURL validates and canonicalizes the external base URL.
func normalizeExternalURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.Host == "" || u.User != nil {
		return "", false
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}
