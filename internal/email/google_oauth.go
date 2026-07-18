// Google connect flow (architecture doc §1: Gmail OAuth is mandatory —
// authorization-code XOAUTH2 with an offline refresh token).
//
// The HTTP layer owns the browser round-trip (state minting/validation,
// redirects); this file owns the OAuth mechanics: building the
// authorization URL and exchanging the callback code, persisting the
// refresh token into the mailbox's encrypted credentials blob. Live
// verification against real Google requires a tenant; the token plumbing
// is unit-tested against local token servers via the test hook below.
package email

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// googleMailScope is the full-mail scope Gmail's IMAP/SMTP XOAUTH2 accepts.
const googleMailScope = "https://mail.google.com/"

// GoogleAuthCodeURL builds the Google authorization URL for an
// oauth_google mailbox: authorization-code flow with access_type=offline
// plus prompt=consent, so the exchange always yields a refresh token
// (Google omits it on silent re-consent otherwise). state must be the
// signed single-use token minted by the caller; redirectURI must be
// registered on the Google OAuth client.
func (a *Auth) GoogleAuthCodeURL(mb store.Mailbox, redirectURI, state string) (string, error) {
	if mb.AuthKind != store.MailboxAuthKindOauthGoogle {
		return "", fmt.Errorf("email: mailbox %s: auth kind %q has no Google connect flow", mb.ID, mb.AuthKind)
	}
	if !mb.OauthClientID.Valid || mb.OauthClientID.String == "" {
		return "", fmt.Errorf("email: mailbox %s: oauth_google requires oauth_client_id", mb.ID)
	}
	cfg := &oauth2.Config{
		ClientID:    mb.OauthClientID.String,
		Endpoint:    google.Endpoint,
		RedirectURL: redirectURI,
		Scopes:      []string{googleMailScope},
	}
	return cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	), nil
}

// GoogleExchange exchanges the authorization code from the connect
// callback and persists the offline refresh token (and the first access
// token, as cache) into the mailbox's encrypted credentials. redirectURI
// must equal the one the authorization URL was built with.
func (a *Auth) GoogleExchange(ctx context.Context, mb store.Mailbox, code, redirectURI string) error {
	if mb.AuthKind != store.MailboxAuthKindOauthGoogle {
		return fmt.Errorf("email: mailbox %s: auth kind %q has no Google connect flow", mb.ID, mb.AuthKind)
	}
	if !mb.OauthClientID.Valid || mb.OauthClientID.String == "" {
		return fmt.Errorf("email: mailbox %s: oauth_google requires oauth_client_id", mb.ID)
	}

	// Serialize against token refreshes so a concurrent supervisor refresh
	// cannot clobber the freshly stored refresh token.
	a.mu.Lock()
	defer a.mu.Unlock()

	creds, err := a.credentials(mb)
	if err != nil {
		return err
	}
	cfg := &oauth2.Config{
		ClientID:     mb.OauthClientID.String,
		ClientSecret: creds.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  google.Endpoint.AuthURL,
			TokenURL: a.googleTokenURL,
		},
		RedirectURL: redirectURI,
		Scopes:      []string{googleMailScope},
	}
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("email: mailbox %s: Google code exchange: %w", mb.ID, err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("email: mailbox %s: Google returned no refresh token; re-run the connect flow", mb.ID)
	}

	creds.RefreshToken = tok.RefreshToken
	creds.AccessToken = tok.AccessToken
	creds.AccessTokenExpiry = tok.Expiry
	enc, err := EncryptCredentials(a.box, creds)
	if err != nil {
		return err
	}
	if err := a.q.UpdateMailboxCredentials(ctx, store.UpdateMailboxCredentialsParams{
		ID: mb.ID, CredentialsEnc: enc,
	}); err != nil {
		return fmt.Errorf("email: mailbox %s: persist refresh token: %w", mb.ID, err)
	}
	return nil
}

// SetGoogleTokenURLForTest points the Google token endpoint at a local
// test server. Test hook only — the real endpoint cannot be exercised in
// CI (no Google tenant); production wiring never calls this.
func (a *Auth) SetGoogleTokenURLForTest(u string) { a.googleTokenURL = u }
