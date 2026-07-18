// SASL credentials per mailbox auth_kind (architecture doc §1: M365
// client-credentials XOAUTH2 and Gmail OAuth are mandatory; Basic Auth is
// dead at the big providers but stays for generic IMAP/SMTP).
//
// Credential storage: mailboxes.credentials_enc holds an AES-256-GCM
// encrypted JSON blob (internal/secrets, purpose "mailbox-credentials").
// For OAuth mailboxes the blob also caches the current access token +
// expiry; a refresh re-encrypts and persists the blob so every replica
// (and the next process) reuses the token instead of hammering the IDP.
package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wneessen/go-mail"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"golang.org/x/oauth2/google"

	"github.com/BryanBaluyut/slatedesk/internal/secrets"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// m365Scope is the resource-wide scope the client-credentials grant asks
// for; per-mailbox access is restricted Entra-side via application access
// policies.
const m365Scope = "https://outlook.office365.com/.default"

// tokenExpirySkew renews access tokens this long before their stated
// expiry so a token never dies mid-connection-setup.
const tokenExpirySkew = 2 * time.Minute

// Credentials is the decrypted credentials_enc JSON blob. Which fields are
// populated depends on mailboxes.auth_kind:
//
//	basic:        Password
//	oauth_m365:   ClientSecret (client-credentials grant)
//	oauth_google: ClientSecret + RefreshToken (authorization-code flow's
//	              offline refresh token, obtained in the M4 admin wizard)
//
// AccessToken/AccessTokenExpiry are a cache, never user input.
type Credentials struct {
	Password          string    `json:"password,omitempty"`
	ClientSecret      string    `json:"client_secret,omitempty"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	AccessToken       string    `json:"access_token,omitempty"`
	AccessTokenExpiry time.Time `json:"access_token_expiry,omitzero"`
}

// EncryptCredentials seals a credentials blob for storage in
// mailboxes.credentials_enc (used by mailbox provisioning and tests).
func EncryptCredentials(box *secrets.Box, c Credentials) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("email: encode credentials: %w", err)
	}
	enc, err := box.Encrypt(raw)
	if err != nil {
		return "", fmt.Errorf("email: encrypt credentials: %w", err)
	}
	return enc, nil
}

// DecryptCredentials opens a credentials blob sealed by EncryptCredentials
// (used by the mailbox admin handlers to preserve unrotated secrets across
// a PATCH and to scrub secrets out of test-connection error strings).
func DecryptCredentials(box *secrets.Box, enc string) (Credentials, error) {
	raw, err := box.Decrypt(enc)
	if err != nil {
		return Credentials{}, fmt.Errorf("email: decrypt credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, fmt.Errorf("email: decode credentials: %w", err)
	}
	return c, nil
}

// Auth turns a mailbox row into ready-to-use IMAP SASL clients and go-mail
// SMTP options, transparently minting/refreshing OAuth access tokens.
// Safe for concurrent use.
type Auth struct {
	q   *store.Queries
	box *secrets.Box

	// Token endpoints, overridable in tests (httptest servers). Defaults
	// are the real Microsoft/Google endpoints.
	m365TokenURL   func(tenant string) string
	googleTokenURL string

	// mu serializes token refreshes so concurrent IMAP+SMTP setup for the
	// same mailbox does not double-refresh.
	mu sync.Mutex
}

// NewAuth returns an Auth reading/persisting cached tokens via pool and
// decrypting credentials with box.
func NewAuth(pool *pgxpool.Pool, box *secrets.Box) *Auth {
	return &Auth{
		q:   store.New(pool),
		box: box,
		m365TokenURL: func(tenant string) string {
			return "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token"
		},
		googleTokenURL: google.Endpoint.TokenURL,
	}
}

// credentials decrypts the mailbox blob.
func (a *Auth) credentials(mb store.Mailbox) (Credentials, error) {
	raw, err := a.box.Decrypt(mb.CredentialsEnc)
	if err != nil {
		return Credentials{}, fmt.Errorf("email: decrypt credentials for mailbox %s: %w", mb.ID, err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, fmt.Errorf("email: decode credentials for mailbox %s: %w", mb.ID, err)
	}
	return c, nil
}

// accessToken returns a live OAuth access token for the mailbox, from the
// encrypted cache when still valid, otherwise via the auth_kind's token
// flow — and persists the refreshed cache (re-encrypted) so the token
// outlives this process.
func (a *Auth) accessToken(ctx context.Context, mb store.Mailbox) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Re-read the encrypted blob fresh from the DB: mb may be a stale
	// snapshot — the IMAP runner loads its mailbox row once at reconcile,
	// and UpdateMailboxCredentials deliberately leaves updated_at alone,
	// so a token-cache write never restarts the runner. Deciding on the
	// snapshot would re-mint a token on every IMAP reconnect (the
	// persisted cache is never seen) and, worse, write credentials derived
	// from the stale blob back OVER a peer's freshly rotated Google
	// refresh token — a lost update that bricks the mailbox. A mailbox
	// deleted since simply falls back to the snapshot (the write-back then
	// updates zero rows).
	if row, err := a.q.GetMailbox(ctx, mb.ID); err == nil {
		mb.CredentialsEnc = row.CredentialsEnc
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("email: mailbox %s: reload credentials: %w", mb.ID, err)
	}

	creds, err := a.credentials(mb)
	if err != nil {
		return "", err
	}
	if creds.AccessToken != "" && time.Now().Add(tokenExpirySkew).Before(creds.AccessTokenExpiry) {
		return creds.AccessToken, nil
	}

	var tok *oauth2.Token
	switch mb.AuthKind {
	case store.MailboxAuthKindOauthM365:
		if !mb.OauthTenantID.Valid || !mb.OauthClientID.Valid {
			return "", fmt.Errorf("email: mailbox %s: oauth_m365 requires oauth_tenant_id and oauth_client_id", mb.ID)
		}
		cfg := &clientcredentials.Config{
			ClientID:     mb.OauthClientID.String,
			ClientSecret: creds.ClientSecret,
			TokenURL:     a.m365TokenURL(mb.OauthTenantID.String),
			Scopes:       []string{m365Scope},
		}
		tok, err = cfg.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("email: mailbox %s: M365 client-credentials token: %w", mb.ID, err)
		}
	case store.MailboxAuthKindOauthGoogle:
		if !mb.OauthClientID.Valid {
			return "", fmt.Errorf("email: mailbox %s: oauth_google requires oauth_client_id", mb.ID)
		}
		if creds.RefreshToken == "" {
			return "", fmt.Errorf("email: mailbox %s: oauth_google requires a refresh token (re-run the connect wizard)", mb.ID)
		}
		cfg := &oauth2.Config{
			ClientID:     mb.OauthClientID.String,
			ClientSecret: creds.ClientSecret,
			Endpoint:     oauth2.Endpoint{TokenURL: a.googleTokenURL},
		}
		tok, err = cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: creds.RefreshToken}).Token()
		if err != nil {
			return "", fmt.Errorf("email: mailbox %s: Google refresh-token grant: %w", mb.ID, err)
		}
		// Google may rotate the refresh token; keep the newest.
		if tok.RefreshToken != "" {
			creds.RefreshToken = tok.RefreshToken
		}
	default:
		return "", fmt.Errorf("email: mailbox %s: auth kind %q has no OAuth token flow", mb.ID, mb.AuthKind)
	}

	creds.AccessToken = tok.AccessToken
	creds.AccessTokenExpiry = tok.Expiry
	enc, err := EncryptCredentials(a.box, creds)
	if err != nil {
		return "", err
	}
	if err := a.q.UpdateMailboxCredentials(ctx, store.UpdateMailboxCredentialsParams{
		ID: mb.ID, CredentialsEnc: enc,
	}); err != nil {
		return "", fmt.Errorf("email: mailbox %s: persist refreshed token: %w", mb.ID, err)
	}
	return tok.AccessToken, nil
}

// IMAPSASL returns the SASL client for the mailbox's IMAP login.
func (a *Auth) IMAPSASL(ctx context.Context, mb store.Mailbox) (sasl.Client, error) {
	switch mb.AuthKind {
	case store.MailboxAuthKindBasic:
		creds, err := a.credentials(mb)
		if err != nil {
			return nil, err
		}
		return sasl.NewPlainClient("", mb.ImapUsername, creds.Password), nil
	case store.MailboxAuthKindOauthM365, store.MailboxAuthKindOauthGoogle:
		tok, err := a.accessToken(ctx, mb)
		if err != nil {
			return nil, err
		}
		return newXOAUTH2Client(mb.ImapUsername, tok), nil
	default:
		return nil, fmt.Errorf("email: mailbox %s: unknown auth kind %q", mb.ID, mb.AuthKind)
	}
}

// AuthenticateIMAP logs the connected client in. Basic-auth mailboxes use
// SASL PLAIN when the server advertises AUTH=PLAIN and fall back to the
// bare LOGIN command otherwise (GreenMail, some appliance servers);
// OAuth mailboxes always use SASL XOAUTH2.
func (a *Auth) AuthenticateIMAP(ctx context.Context, c *imapclient.Client, mb store.Mailbox) error {
	if mb.AuthKind == store.MailboxAuthKindBasic && !c.Caps().Has(imap.AuthCap(sasl.Plain)) {
		creds, err := a.credentials(mb)
		if err != nil {
			return err
		}
		if err := c.Login(mb.ImapUsername, creds.Password).Wait(); err != nil {
			return fmt.Errorf("email: mailbox %s: IMAP LOGIN: %w", mb.ID, err)
		}
		return nil
	}
	saslClient, err := a.IMAPSASL(ctx, mb)
	if err != nil {
		return err
	}
	if err := c.Authenticate(saslClient); err != nil {
		return fmt.Errorf("email: mailbox %s: IMAP AUTHENTICATE: %w", mb.ID, err)
	}
	return nil
}

// SMTPOptions returns the go-mail client options implementing the
// mailbox's SMTP authentication (TLS posture is handled separately by the
// caller from smtp_tls_mode).
func (a *Auth) SMTPOptions(ctx context.Context, mb store.Mailbox) ([]mail.Option, error) {
	switch mb.AuthKind {
	case store.MailboxAuthKindBasic:
		creds, err := a.credentials(mb)
		if err != nil {
			return nil, err
		}
		authType := mail.SMTPAuthPlain
		if mb.SmtpTlsMode == store.MailTlsModeNone {
			// Dev/test only (GreenMail): PLAIN over cleartext must be
			// requested explicitly or go-mail refuses it.
			authType = mail.SMTPAuthPlainNoEnc
		}
		return []mail.Option{
			mail.WithSMTPAuth(authType),
			mail.WithUsername(mb.SmtpUsername),
			mail.WithPassword(creds.Password),
		}, nil
	case store.MailboxAuthKindOauthM365, store.MailboxAuthKindOauthGoogle:
		tok, err := a.accessToken(ctx, mb)
		if err != nil {
			return nil, err
		}
		return []mail.Option{
			mail.WithSMTPAuth(mail.SMTPAuthXOAUTH2),
			mail.WithUsername(mb.SmtpUsername),
			mail.WithPassword(tok),
		}, nil
	default:
		return nil, fmt.Errorf("email: mailbox %s: unknown auth kind %q", mb.ID, mb.AuthKind)
	}
}

// xoauth2Client is the SASL XOAUTH2 mechanism (Microsoft and Google both
// speak it for IMAP). go-sasl ships OAUTHBEARER but not XOAUTH2, so the
// ~20 lines live here. Initial response per the Google/Microsoft spec:
//
//	user=<user>^Aauth=Bearer <token>^A^A
type xoauth2Client struct {
	username, token string
	failed          bool
}

func newXOAUTH2Client(username, token string) sasl.Client {
	return &xoauth2Client{username: username, token: token}
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	ir := "user=" + c.username + "\x01auth=Bearer " + c.token + "\x01\x01"
	return "XOAUTH2", []byte(ir), nil
}

// Next handles the failure path: the server sends a base64 JSON error as a
// challenge and expects an empty response before failing the command.
func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	if c.failed {
		return nil, fmt.Errorf("email: XOAUTH2 authentication failed: %s", string(challenge))
	}
	c.failed = true
	return []byte{}, nil
}

// smtpTLSOptions maps a mailbox TLS mode onto go-mail client options.
func smtpTLSOptions(mode store.MailTlsMode) []mail.Option {
	switch mode {
	case store.MailTlsModeTls:
		return []mail.Option{mail.WithSSL()}
	case store.MailTlsModeStarttls:
		return []mail.Option{mail.WithTLSPolicy(mail.TLSMandatory)}
	case store.MailTlsModeNone:
		return []mail.Option{mail.WithTLSPolicy(mail.NoTLS)}
	default:
		return []mail.Option{mail.WithTLSPolicy(mail.TLSMandatory)}
	}
}

// canonicalMessageID normalizes a Message-ID header value for storage and
// lookup: surrounding whitespace and one layer of angle brackets removed.
// Returns "" when no id is present.
func canonicalMessageID(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// errNotEmailTicket signals "this ticket has no email origin" internally.
var errNotEmailTicket = errors.New("email: ticket has no email origin")
