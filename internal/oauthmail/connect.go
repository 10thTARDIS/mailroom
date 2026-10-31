// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/xoauth2"
)

// THE CONNECT FLOW (ADR-046 §3, increment 3 of #52).
//
//  1. Account/oauthStart (a session-only, CSRF-guarded JMAP method) calls
//     Connector.Start, which writes a single-use app.oauth_flows row bound to
//     the signed-in user — the OAuth state kept only as its SHA-256, the PKCE
//     verifier sealed, a nonce, ten minutes to live — and returns the
//     provider's authorization URL (code flow, PKCE S256).
//  2. The provider sends the browser back to GET /oauth/callback/{provider},
//     behind the session gate. Connector.Callback consumes the flow (a DELETE
//     … RETURNING under the user's RLS scope, so a replay, a stale state or
//     ANOTHER user's state all find nothing), checks the flow was started for
//     this provider (mix-up defence, plus RFC 9207 `iss` when present),
//     exchanges the code with the verifier, checks the ID token and the
//     granted scopes, proves the mailbox with an XOAUTH2 IMAP sign-in, and
//     then creates the account and its grant — or, for a re-authentication,
//     updates the existing grant, which must belong to the SAME provider
//     account — in one transaction.
//  3. The browser is redirected to a FIXED in-app location carrying a result
//     code (never a token, never a code, never a destination taken from the
//     request).
//
// Disconnect revokes at the provider where it can (Google), deletes the grant
// and the sealed refresh token, forgets the cached access token and stops
// sync, and keeps the account and its mail (ADR-046 §6).

// CallbackPathPrefix is where the provider returns the browser; the provider
// name follows. The route is registered by cmd/mailroom as
// "GET /oauth/callback/{provider}".
const CallbackPathPrefix = "/oauth/callback/"

// flowTTL is how long a started connect may take. Long enough for Google's
// unverified-app screen and a consent page; short enough that a leaked state
// is worthless soon.
const flowTTL = 10 * time.Minute

// Errors Start and Disconnect return for the API layer to map.
var (
	// ErrProviderNotEnabled: the operator has not configured that provider.
	ErrProviderNotEnabled = errors.New("oauthmail: that provider is not configured on this server")
	// ErrNotOAuthAccount: the account signs in with a password.
	ErrNotOAuthAccount = errors.New("oauthmail: that account does not sign in with a provider")
	// ErrWrongProvider: a re-authentication named a provider other than the
	// account's.
	ErrWrongProvider = errors.New("oauthmail: that account belongs to another provider")
)

// Result codes the callback redirects with. They are the whole vocabulary a
// client sees; each says what happened without carrying anything secret.
const (
	ResultConnected          = "connected"
	ResultReconnected        = "reconnected"
	ResultAlreadyConnected   = "already_connected"
	ResultDenied             = "denied"
	ResultProviderError      = "provider_error"
	ResultInvalidRequest     = "invalid_request"
	ResultInvalidState       = "invalid_state"
	ResultExpired            = "expired"
	ResultProviderMismatch   = "provider_mismatch"
	ResultExchangeFailed     = "exchange_failed"
	ResultIdentityInvalid    = "identity_invalid"
	ResultMissingScope       = "missing_scope"
	ResultNoRefreshToken     = "no_refresh_token"
	ResultMailboxRefused     = "mailbox_refused"
	ResultMailboxUnreachable = "mailbox_unreachable"
	ResultWrongAccount       = "wrong_account"
	ResultNotConfigured      = "not_configured"
	ResultServerError        = "server_error"
)

// resultLocation is where every callback ends: the app root (the client's
// router lives in the fragment), with the result in the query. Fixed; no
// part of it comes from the request but the provider's NAME, which is one of
// the registry's.
func resultLocation(r callbackResult) string {
	q := url.Values{"oauth": {r.code}}
	if r.provider != "" {
		q.Set("provider", r.provider)
	}
	if r.account != (ulid.ULID{}) {
		q.Set("account", r.account.String())
	}
	return "/?" + q.Encode() + "#/settings"
}

// ResultLocation is the fixed in-app location for a result code, for callers
// that answer the callback route without a Connector (no provider configured).
func ResultLocation(code, provider string) string {
	return resultLocation(callbackResult{code: code, provider: provider})
}

// MailboxCheck proves an address and access token work for IMAP at the
// provider: the final arbiter of the identity the ID token claims (ADR-046
// §3). The default dials the provider's fixed IMAP host with XOAUTH2.
type MailboxCheck func(ctx context.Context, p Provider, address string, token xoauth2.Secret) error

// ConnectorConfig wires a Connector.
type ConnectorConfig struct {
	Providers *Registry
	Store     *store.Store
	Keys      crypto.KeyProvider
	// Manager is the process's token Manager: Forget is called on it when a
	// grant ends or is replaced. Required.
	Manager *Manager
	// PublicURL is the origin the browser reaches this server at
	// (MAILROOM_PUBLIC_URL, or the OIDC redirect URL's origin). The callback
	// URLs registered at each provider are built from it.
	PublicURL string
	// HTTPClient is used for the token and revocation endpoints (nil: a
	// private client; redirects are never followed either way).
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Clock (tests).
	Clock func() time.Time
	// CheckMailbox overrides the IMAP proof (tests).
	CheckMailbox MailboxCheck
	// CheckSMTP, when set, probes a Microsoft account's SMTP AUTH after
	// connect and reconnect and records whether it is receive-only
	// (ADR-046 §12). nil runs no probe; cmd/mailroom sets SMTPAuthCheck.
	CheckSMTP SMTPCheck
	// AllowPrivateHosts is MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS, applied to
	// the default mailbox check's dial like every other account dial.
	AllowPrivateHosts bool
}

// Connector runs the connect, re-authentication and disconnect flows.
type Connector struct {
	providers *Registry
	store     *store.Store
	keys      crypto.KeyProvider
	manager   *Manager
	base      string
	http      *http.Client
	log       *slog.Logger
	now       func() time.Time
	check     MailboxCheck
	smtpCheck SMTPCheck
}

// NewConnector validates cfg and builds a Connector.
func NewConnector(cfg ConnectorConfig) (*Connector, error) {
	base, err := PublicOrigin(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if cfg.Store == nil || cfg.Keys == nil || cfg.Manager == nil {
		return nil, errors.New("oauthmail: connector needs a store, a key provider and a token manager")
	}
	c := &Connector{
		providers: cfg.Providers,
		store:     cfg.Store,
		keys:      cfg.Keys,
		manager:   cfg.Manager,
		base:      base,
		http:      cfg.HTTPClient,
		log:       cfg.Logger,
		now:       cfg.Clock,
		check:     cfg.CheckMailbox,
		smtpCheck: cfg.CheckSMTP,
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.check == nil {
		c.check = imapMailboxCheck(cfg.AllowPrivateHosts)
	}
	return c, nil
}

// PublicOrigin reduces raw to its scheme://host[:port] origin, refusing
// anything that is not an absolute http(s) URL. https is required except for
// a loopback host (a developer's machine).
func PublicOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", fmt.Errorf("oauthmail: public URL %q is not an absolute http(s) URL", raw)
	}
	if u.Scheme == "http" {
		h := u.Hostname()
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return "", fmt.Errorf("oauthmail: public URL %q must be https (providers send the authorization code to it)", raw)
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

// RedirectURI is the exact callback URL to register at provider.
func (c *Connector) RedirectURI(provider string) string {
	return c.base + CallbackPathPrefix + provider
}

// Providers lists the enabled providers, sorted (the capability's
// oauthProviders).
func (c *Connector) Providers() []string { return c.providers.Enabled() }

// Forget drops the account's cached access token (ADR-046 §9 obligation (b)).
func (c *Connector) Forget(userID, accountID ulid.ULID) { c.manager.Forget(userID, accountID) }

// StartResult is what Account/oauthStart returns.
type StartResult struct {
	AuthorizeURL string
	ExpiresAt    time.Time
}

// Start begins a connect for userID with provider — a new account, or a
// re-authentication of reauth (one of the user's OAuth accounts of the same
// provider). It records the pending flow and returns where to send the
// browser. Errors: ErrProviderNotEnabled, store.ErrAccountNotFound,
// ErrNotOAuthAccount, ErrWrongProvider.
func (c *Connector) Start(ctx context.Context, userID ulid.ULID, provider string, reauth *ulid.ULID) (StartResult, error) {
	p, ok := c.providers.Lookup(provider)
	if !ok {
		return StartResult{}, ErrProviderNotEnabled
	}
	state, err := randomToken(32)
	if err != nil {
		return StartResult{}, err
	}
	verifier, err := randomToken(32) // 43 characters: RFC 7636's minimum, 256 bits
	if err != nil {
		return StartResult{}, err
	}
	nonce, err := randomToken(16)
	if err != nil {
		return StartResult{}, err
	}
	master, err := c.keys.MasterKey(ctx)
	if err != nil {
		return StartResult{}, fmt.Errorf("oauthmail: master key: %w", err)
	}
	defer crypto.Zero(master)

	flowID := store.NewULID()
	sealed, err := SealFlowVerifier(master, userID, flowID, xoauth2.Secret(verifier))
	if err != nil {
		return StartResult{}, err
	}
	now := c.now()
	expires := now.Add(flowTTL)
	hint := ""
	err = c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// Opportunistic cleanup: whoever starts a flow clears their own stale
		// ones (the periodic sweep covers users who never come back).
		if _, err := tx.DeleteExpiredOAuthFlows(ctx, now); err != nil {
			return err
		}
		if reauth != nil {
			addr, err := c.reauthTarget(ctx, tx, master, userID, *reauth, p.Name)
			if err != nil {
				return err
			}
			hint = addr
		}
		return tx.CreateOAuthFlow(ctx, store.OAuthFlowRow{
			ID: flowID, StateHash: sha256.Sum256([]byte(state)), Provider: p.Name,
			AccountID: reauth, VerifierCT: sealed, Nonce: nonce, ExpiresAt: expires,
		})
	})
	if err != nil {
		return StartResult{}, err
	}

	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.Client.ID},
		"redirect_uri":          {c.RedirectURI(p.Name)},
		"scope":                 {strings.Join(p.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	switch p.Name {
	case ProviderGoogle:
		// offline + consent: Google issues a refresh token only on a consent
		// it has just shown (ADR-046 §3).
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	case ProviderMicrosoft:
		// Explicitly the query: never form_post, which would be a cross-site
		// POST the CSRF guard refuses and that carries no session cookie.
		q.Set("response_mode", "query")
	}
	if hint != "" {
		q.Set("login_hint", hint)
	}
	return StartResult{AuthorizeURL: p.AuthURL + "?" + q.Encode(), ExpiresAt: expires}, nil
}

// reauthTarget checks that accountID is one of the user's OAuth accounts of
// provider and returns its address (the login hint).
func (c *Connector) reauthTarget(ctx context.Context, tx *store.UserTx, master []byte, userID, accountID ulid.ULID, provider string) (string, error) {
	acct, err := tx.GetAccount(ctx, accountID)
	if err != nil {
		return "", err // ErrAccountNotFound (wrapped) for anything not the caller's
	}
	if acct.AuthKind != "oauth2" {
		return "", ErrNotOAuthAccount
	}
	if g, err := tx.OAuthGrant(ctx, accountID); err == nil {
		if g.Provider != provider {
			return "", ErrWrongProvider
		}
		return g.Address, nil
	} else if !errors.Is(err, store.ErrOAuthGrantNotFound) {
		return "", err
	}
	// Disconnected: no grant, so the sealed secret says whose it was.
	creds, err := onboarding.OpenCredentials(master, userID, acct.CredentialsCT)
	if err != nil {
		return "", fmt.Errorf("oauthmail: account %s: %w", accountID, err)
	}
	if creds.OAuth == nil {
		return "", ErrNotOAuthAccount
	}
	if creds.OAuth.Provider != provider {
		return "", ErrWrongProvider
	}
	return creds.IMAPUsername, nil
}

// callbackResult is one callback's outcome.
type callbackResult struct {
	code     string
	provider string
	account  ulid.ULID
	// why is for the log line only: a check's name or an error class, never
	// anything secret and never provider prose.
	why string
}

// Callback serves GET /oauth/callback/{provider}. userID is the session's
// (the route sits behind the session gate); ok false means no session.
func (c *Connector) Callback(w http.ResponseWriter, r *http.Request, userID ulid.ULID, ok bool) {
	// Nothing about this response may be cached, and the URL that brought the
	// browser here (it carries the code and state) must not leak onward as a
	// Referer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	name := r.PathValue("provider")
	var res callbackResult
	if !ok {
		res = callbackResult{code: ResultInvalidRequest, why: "no session"}
	} else {
		res = c.complete(r.Context(), userID, name, r.URL.Query())
	}
	if res.provider == "" {
		if _, known := c.providers.Lookup(name); known {
			res.provider = name
		}
	}
	level := slog.LevelInfo
	if res.code == ResultServerError {
		level = slog.LevelError
	}
	attrs := []any{"result", res.code, "provider", res.provider, "user_id", userID.String()}
	if res.account != (ulid.ULID{}) {
		attrs = append(attrs, "account_id", res.account.String())
	}
	if res.why != "" {
		attrs = append(attrs, "detail", res.why)
	}
	c.log.Log(r.Context(), level, "oauth connect callback", attrs...)
	http.Redirect(w, r, resultLocation(res), http.StatusSeeOther)
}

// complete runs the callback's checks and writes, in the order ADR-046 §3
// gives. Every early return is a refusal that has written nothing but the
// flow's consumption.
func (c *Connector) complete(ctx context.Context, userID ulid.ULID, name string, q url.Values) callbackResult {
	p, ok := c.providers.Lookup(name)
	if !ok {
		return callbackResult{code: ResultNotConfigured, why: "provider not enabled"}
	}
	res := func(code, why string) callbackResult { return callbackResult{code: code, provider: p.Name, why: why} }

	state := q.Get("state")
	if state == "" || len(state) > 512 {
		return res(ResultInvalidRequest, "no state")
	}

	// Consume FIRST, whatever else the response says: a state is good for
	// exactly one callback, including a refused or an erroneous one. The
	// DELETE runs under the session user's RLS scope, so another user's state,
	// a replay and an unknown state are the same outcome: not found.
	var flow store.OAuthFlow
	var expired bool
	if err := c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		f, err := tx.ConsumeOAuthFlow(ctx, sha256.Sum256([]byte(state)), c.now())
		if errors.Is(err, store.ErrOAuthFlowExpired) {
			expired = true // commit the deletion: an expired state stays dead
			return nil
		}
		flow = f
		return err
	}); err != nil {
		if errors.Is(err, store.ErrOAuthFlowNotFound) {
			return res(ResultInvalidState, "no pending flow for this state and user")
		}
		return res(ResultServerError, "consume flow: "+err.Error())
	}
	if expired {
		return res(ResultExpired, "flow expired")
	}
	if flow.Provider != p.Name {
		// Mix-up: a flow started for one provider completing at another's
		// callback. Consumed above, so it cannot be retried at the right one.
		return res(ResultProviderMismatch, "flow was started for "+flow.Provider)
	}
	if iss := q.Get("iss"); iss != "" && !p.authorizationIssuer(iss) {
		return res(ResultProviderMismatch, "authorization response iss is not this provider")
	}
	if e := q.Get("error"); e != "" {
		code := oauthCode(e)
		if code == "access_denied" {
			return res(ResultDenied, "user declined consent")
		}
		return res(ResultProviderError, "provider returned "+code)
	}
	code := q.Get("code")
	if code == "" || len(code) > 4096 {
		return res(ResultInvalidRequest, "no code")
	}

	master, err := c.keys.MasterKey(ctx)
	if err != nil {
		return res(ResultServerError, "master key")
	}
	defer crypto.Zero(master)
	verifier, err := OpenFlowVerifier(master, userID, flow.ID, flow.VerifierCT)
	if err != nil {
		return res(ResultServerError, "open flow verifier")
	}

	tok, err := p.Exchange(ctx, c.http, xoauth2.Secret(code), verifier, c.RedirectURI(p.Name))
	if err != nil {
		var rerr *RefreshError
		if errors.As(err, &rerr) {
			return res(ResultExchangeFailed, "token endpoint: "+rerr.Class.String()+" "+rerr.Code)
		}
		return res(ResultExchangeFailed, "token endpoint")
	}
	id, err := p.verifyIDToken(tok.IDToken, flow.Nonce, c.now())
	if err != nil {
		return res(ResultIdentityInvalid, err.Error())
	}
	if missing := p.missingScopes(tok.Scopes); len(missing) > 0 {
		return res(ResultMissingScope, "not granted: "+strings.Join(missing, " "))
	}
	if tok.RefreshToken.Empty() {
		return res(ResultNoRefreshToken, "token response carried no refresh token")
	}
	if err := c.check(ctx, p, id.Address, tok.AccessToken); err != nil {
		var se *sync.Error
		if errors.As(err, &se) && se.Kind == sync.ErrorKindAuth {
			return res(ResultMailboxRefused, "IMAP refused the new token")
		}
		return res(ResultMailboxUnreachable, "IMAP check failed: "+xoauth2.Scrub(err.Error(), tok.AccessToken, tok.RefreshToken))
	}

	granted := tok.Scopes
	if len(granted) == 0 {
		granted = p.Scopes // RFC 6749 §5.1: omitted means as requested
	}
	var out callbackResult
	if flow.AccountID != nil {
		out = c.reauthenticate(ctx, master, userID, p, *flow.AccountID, id, granted, tok.RefreshToken)
	} else {
		out = c.connectNew(ctx, master, userID, p, id, granted, tok.RefreshToken)
	}
	if out.code == ResultConnected || out.code == ResultReconnected {
		// Receive-only or not (ADR-046 §12): after the account exists, so a
		// 5.7.139 tenant still connects — it just cannot send.
		c.checkSending(ctx, userID, p, out.account, id.Address, tok.AccessToken)
	}
	return out
}

// connectNew creates the account, its identity and its grant in one
// transaction.
func (c *Connector) connectNew(ctx context.Context, master []byte, userID ulid.ULID, p Provider, id Identity, scopes []string, refresh xoauth2.Secret) callbackResult {
	res := func(code, why string, acct ulid.ULID) callbackResult {
		return callbackResult{code: code, provider: p.Name, why: why, account: acct}
	}
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: id.Address,
		OAuth:        &onboarding.OAuthSecret{Provider: p.Name, RefreshToken: refresh.Reveal(), Subject: id.Subject},
	})
	if err != nil {
		return res(ResultServerError, "seal credentials", ulid.ULID{})
	}
	gen, err := randomGeneration()
	if err != nil {
		return res(ResultServerError, "random generation", ulid.ULID{})
	}
	var (
		created   ulid.ULID
		duplicate ulid.ULID
	)
	err = c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if g, err := tx.OAuthGrantBySubject(ctx, p.Name, id.Subject); err == nil {
			duplicate = g.AccountID
			return nil
		} else if !errors.Is(err, store.ErrOAuthGrantNotFound) {
			return err
		}
		// Sync starts OFF while the Gmail sync hold stands (synchold.go):
		// connected and ready, but not downloading every label folder with
		// no budget.
		_, _, held := SyncHold("oauth2", p.Profile)
		row := store.AccountRow{
			ProviderProfile: p.Profile,
			IMAPHost:        p.IMAPHost, IMAPPort: p.IMAPPort,
			SMTPHost: p.SMTPHost, SMTPPort: p.SMTPPort,
			AuthKind:       "oauth2",
			CredentialsCT:  ct,
			CredentialsDEK: onboarding.CredentialsSchemeV1,
			SyncEnabled:    !held,
			// The profile's stated daily send cap (Microsoft: 100, ADR-046
			// §12); 0 keeps the schema default.
			OutboundDailyQuota: provider.OutboundDailyQuota(p.Profile),
		}
		if p.Name == ProviderGoogle {
			// ADR-047: a Gmail account is ingest-only until Mailroom's Gmail
			// writeback (label-aware, no EXPUNGE outside Trash and Spam)
			// exists. Today's generic writeback would act on Gmail's label
			// folders as if they were folders.
			off := false
			row.WritebackEnabled = &off
		}
		acct, err := createNamedAccount(ctx, tx, row, accountNames(id.Address, p.Name))
		if err != nil {
			return err
		}
		if _, err := tx.CreateIdentity(ctx, store.IdentityRow{
			AccountID: acct, Kind: store.IdentityFixed, Address: id.Address, Enabled: true,
		}); err != nil && !errors.Is(err, store.ErrIdentityExists) {
			return err
		}
		if err := tx.CreateOAuthGrant(ctx, store.OAuthGrantRow{
			AccountID: acct, Provider: p.Name, Subject: id.Subject, Address: id.Address,
			Scopes: scopes, Generation: gen,
		}); err != nil {
			return err
		}
		created = acct
		return tx.AuditOAuth(ctx, "oauth.connect", acct, map[string]any{"provider": p.Name, "scopes": scopes})
	})
	switch {
	case errors.Is(err, store.ErrOAuthGrantExists):
		// A concurrent connect of the same provider account won the UNIQUE
		// (user, provider, subject); this transaction rolled back whole.
		return res(ResultAlreadyConnected, "connected concurrently", ulid.ULID{})
	case err != nil:
		return res(ResultServerError, "create account: "+err.Error(), ulid.ULID{})
	case duplicate != (ulid.ULID{}):
		return res(ResultAlreadyConnected, "that provider account is already connected", duplicate)
	}
	return res(ResultConnected, "", created)
}

// reauthenticate updates an existing grant (obligation (d)) or, for a
// disconnected account, re-attaches one — in both cases only for the SAME
// provider account the Mailroom account belonged to.
func (c *Connector) reauthenticate(ctx context.Context, master []byte, userID ulid.ULID, p Provider, accountID ulid.ULID, id Identity, scopes []string, refresh xoauth2.Secret) callbackResult {
	res := func(code, why string) callbackResult {
		return callbackResult{code: code, provider: p.Name, why: why, account: accountID}
	}
	newCreds := onboarding.Credentials{
		IMAPUsername: id.Address,
		OAuth:        &onboarding.OAuthSecret{Provider: p.Name, RefreshToken: refresh.Reveal(), Subject: id.Subject},
	}
	ct, err := onboarding.SealCredentials(master, userID, newCreds)
	if err != nil {
		return res(ResultServerError, "seal credentials")
	}
	var outcome string
	err = c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		acct, err := tx.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if acct.AuthKind != "oauth2" {
			return ErrNotOAuthAccount
		}
		err = tx.UpdateOAuthGrant(ctx, store.OAuthGrantUpdate{
			AccountID: accountID, Provider: p.Name, Subject: id.Subject, Address: id.Address,
			Scopes: scopes, CredentialsCT: ct,
		}, c.now())
		if !errors.Is(err, store.ErrOAuthGrantNotFound) {
			outcome = "updated"
			return err
		}
		// No grant: the account was disconnected. Its sealed secret still
		// names the provider account it belonged to (OAuthSecret.Subject).
		old, err := onboarding.OpenCredentials(master, userID, acct.CredentialsCT)
		if err != nil {
			return err
		}
		if old.OAuth == nil || old.OAuth.Provider != p.Name || old.OAuth.Subject == "" || old.OAuth.Subject != id.Subject {
			return store.ErrOAuthSubjectMismatch
		}
		gen, err := randomGeneration()
		if err != nil {
			return err
		}
		outcome = "reattached"
		_, _, held := SyncHold(acct.AuthKind, acct.ProviderProfile)
		return tx.ReconnectOAuthAccount(ctx, store.OAuthGrantRow{
			AccountID: accountID, Provider: p.Name, Subject: id.Subject, Address: id.Address,
			Scopes: scopes, Generation: gen,
		}, ct, !held)
	})
	switch {
	case errors.Is(err, store.ErrOAuthSubjectMismatch):
		return res(ResultWrongAccount, "signed in as a different provider account")
	case errors.Is(err, store.ErrOAuthGrantExists):
		// The provider account is already connected as ANOTHER of this user's
		// accounts (UNIQUE (user, provider, subject)).
		return res(ResultAlreadyConnected, "that provider account is connected as another account")
	case errors.Is(err, store.ErrAccountNotFound), errors.Is(err, ErrNotOAuthAccount):
		return res(ResultInvalidState, "re-authentication target is gone or not an OAuth account")
	case err != nil:
		return res(ResultServerError, "re-authenticate: "+err.Error())
	}
	// The cached access token (if any) belongs to the replaced refresh token;
	// drop it so the next connection uses the new grant.
	c.manager.Forget(userID, accountID)
	return res(ResultReconnected, outcome)
}

// createNamedAccount inserts row under the first free name in names.
func createNamedAccount(ctx context.Context, tx *store.UserTx, row store.AccountRow, names []string) (ulid.ULID, error) {
	for _, n := range names {
		row.Name = n
		id, err := tx.CreateAccount(ctx, row)
		if errors.Is(err, store.ErrAccountNameTaken) {
			continue
		}
		return id, err
	}
	return ulid.ULID{}, store.ErrAccountNameTaken
}

// accountNames are the names a new account tries, in order: the address,
// then the address with the provider's product name, then numbered.
func accountNames(address, provider string) []string {
	label := "Gmail"
	if provider == ProviderMicrosoft {
		label = "Outlook"
	}
	out := []string{address, address + " (" + label + ")"}
	for i := 2; i <= 5; i++ {
		out = append(out, fmt.Sprintf("%s (%s %d)", address, label, i))
	}
	return out
}

// DisconnectResult is what Account/oauthDisconnect reports.
type DisconnectResult struct {
	Provider string
	// Revocation is "revoked" (the provider confirmed), "failed" (it could
	// not be reached or refused; the user should remove access at the
	// provider), "unsupported" (Microsoft: no programmatic revocation for
	// delegated tokens) or "not_configured" (the provider is no longer
	// configured here, so nothing could be sent).
	Revocation string
}

// Disconnect ends an OAuth account's grant and keeps the account and its mail
// (ADR-046 §6): best-effort revocation at the provider, then — in one
// transaction — the grant and the sealed refresh token are deleted and sync
// is switched off; then the cached access token is forgotten. Errors:
// store.ErrAccountNotFound, ErrNotOAuthAccount, ErrNoGrant (already
// disconnected).
func (c *Connector) Disconnect(ctx context.Context, userID, accountID ulid.ULID) (DisconnectResult, error) {
	var (
		g    store.OAuthGrant
		ct   []byte
		kind string
	)
	err := c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		acct, err := tx.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		kind = acct.AuthKind
		if kind != "oauth2" {
			return nil
		}
		g, ct, err = tx.OAuthGrantWithSecret(ctx, accountID)
		return err
	})
	switch {
	case err == nil && kind != "oauth2":
		return DisconnectResult{}, ErrNotOAuthAccount
	case errors.Is(err, store.ErrOAuthGrantNotFound):
		return DisconnectResult{}, ErrNoGrant
	case err != nil:
		return DisconnectResult{}, err
	}
	master, err := c.keys.MasterKey(ctx)
	if err != nil {
		return DisconnectResult{}, fmt.Errorf("oauthmail: master key: %w", err)
	}
	defer crypto.Zero(master)
	creds, err := onboarding.OpenCredentials(master, userID, ct)
	if err != nil {
		return DisconnectResult{}, fmt.Errorf("oauthmail: account %s: %w", accountID, err)
	}
	var refresh xoauth2.Secret
	if creds.OAuth != nil {
		refresh = xoauth2.Secret(creds.OAuth.RefreshToken)
	}

	out := DisconnectResult{Provider: g.Provider, Revocation: "unsupported"}
	log := c.log.With("user_id", userID.String(), "account_id", accountID.String(), "provider", g.Provider)
	if p, enabled := c.providers.Lookup(g.Provider); !enabled {
		out.Revocation = "not_configured"
	} else if p.RevokeURL != "" && !refresh.Empty() {
		if err := p.Revoke(ctx, c.http, refresh); err != nil {
			out.Revocation = "failed"
			log.Warn("could not revoke the grant at the provider; the user should remove access there",
				"err", xoauth2.Scrub(err.Error(), refresh, p.Client.Secret))
		} else {
			out.Revocation = "revoked"
		}
	}

	// The sealed secret keeps the provider and the SUBJECT (a reconnect must
	// be the same provider account) and loses the refresh token.
	kept := onboarding.Credentials{IMAPUsername: creds.IMAPUsername,
		OAuth: &onboarding.OAuthSecret{Provider: g.Provider, Subject: g.Subject}}
	newCT, err := onboarding.SealCredentials(master, userID, kept)
	if err != nil {
		return DisconnectResult{}, err
	}
	if err := c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, err := tx.DisconnectOAuthGrant(ctx, accountID, newCT, out.Revocation)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrOAuthGrantNotFound) {
			return DisconnectResult{}, ErrNoGrant
		}
		return DisconnectResult{}, err
	}
	c.manager.Forget(userID, accountID)
	log.Info("oauth account disconnected; its mail is kept", "revocation", out.Revocation)
	return out, nil
}

// PrepareRevoke readies the best-effort provider revocation an account
// DELETION owes (security review M1 of #897): deleting an OAuth account must
// not leave its grant live at the provider, where a restored database backup
// would re-arm it. It reads the grant and the sealed refresh token NOW, while
// they still exist (the delete cascades them away), and returns a function
// that revokes — to be called only once the delete has committed, so an
// aborted delete revokes nothing. It returns nil when there is nothing to
// revoke: a password account, no grant, a provider without a revocation
// endpoint, a provider no longer configured. Nothing here can fail or block
// the delete; outcomes are logged with ids only.
func (c *Connector) PrepareRevoke(ctx context.Context, userID, accountID ulid.ULID) func(context.Context) {
	var (
		g  store.OAuthGrant
		ct []byte
	)
	if err := c.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		g, ct, err = tx.OAuthGrantWithSecret(ctx, accountID)
		return err
	}); err != nil {
		return nil // not an OAuth account with a grant (or not the caller's)
	}
	p, enabled := c.providers.Lookup(g.Provider)
	if !enabled || p.RevokeURL == "" {
		return nil
	}
	master, err := c.keys.MasterKey(ctx)
	if err != nil {
		return nil
	}
	defer crypto.Zero(master)
	creds, err := onboarding.OpenCredentials(master, userID, ct)
	if err != nil || creds.OAuth == nil || creds.OAuth.RefreshToken == "" {
		return nil
	}
	refresh := xoauth2.Hold(xoauth2.Secret(creds.OAuth.RefreshToken))
	return func(ctx context.Context) {
		log := c.log.With("user_id", userID.String(), "account_id", accountID.String(), "provider", g.Provider)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := p.Revoke(ctx, c.http, refresh.Get()); err != nil {
			log.Warn("deleted an OAuth account but could not revoke its grant at the provider; "+
				"the user should remove access there", "err", xoauth2.Scrub(err.Error(), refresh.Get(), p.Client.Secret))
			return
		}
		log.Info("deleted an OAuth account and revoked its grant at the provider")
	}
}

// SweepExpiredFlows deletes expired pending flows for each user, each in that
// user's own transaction (the flows are RLS-scoped; there is no cross-user
// door), and returns how many it removed.
func (c *Connector) SweepExpiredFlows(ctx context.Context, users []ulid.ULID) (int64, error) {
	var total int64
	now := c.now()
	for _, u := range users {
		if err := c.store.UserTx(ctx, u, func(tx *store.UserTx) error {
			n, err := tx.DeleteExpiredOAuthFlows(ctx, now)
			total += n
			return err
		}); err != nil {
			return total, err
		}
	}
	return total, nil
}

// RunFlowSweep runs SweepExpiredFlows over listUsers() every interval until
// ctx ends — the same ticker shape as the API key approval sweep.
func (c *Connector) RunFlowSweep(ctx context.Context, interval time.Duration, listUsers func(context.Context) ([]ulid.ULID, error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			users, err := listUsers(ctx)
			if err != nil {
				if ctx.Err() == nil {
					c.log.Warn("oauth flow sweep: could not list users", "err", err)
				}
				continue
			}
			if n, err := c.SweepExpiredFlows(ctx, users); err != nil && ctx.Err() == nil {
				c.log.Warn("oauth flow sweep failed", "err", err)
			} else if n > 0 {
				c.log.Info("removed expired oauth connect flows", "flows", n)
			}
		}
	}
}

// authorizationIssuer checks RFC 9207's `iss` on the authorization response.
func (p Provider) authorizationIssuer(iss string) bool {
	switch p.Name {
	case ProviderGoogle:
		return iss == "https://accounts.google.com"
	case ProviderMicrosoft:
		return strings.HasPrefix(iss, "https://login.microsoftonline.com/")
	}
	return false
}

// missingScopes returns the required scopes absent from granted. An empty
// granted list means the provider did not say, which RFC 6749 §5.1 defines as
// "as requested". Compared case-insensitively (Microsoft's scopes are).
func (p Provider) missingScopes(granted []string) []string {
	if len(granted) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, s := range granted {
		have[strings.ToLower(s)] = true
	}
	var missing []string
	for _, s := range p.RequiredScopes {
		if !have[strings.ToLower(s)] {
			missing = append(missing, s)
		}
	}
	return missing
}

// Exchange redeems an authorization code with its PKCE verifier at p's token
// endpoint (the same request path, scrubbing and classification as Refresh).
func (p Provider) Exchange(ctx context.Context, hc *http.Client, code, verifier xoauth2.Secret, redirectURI string) (TokenResponse, error) {
	if code.Empty() || verifier.Empty() {
		return TokenResponse{}, &RefreshError{Provider: p.Name, Class: FailureOperator, Code: "invalid_request"}
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code.Reveal()},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.Client.ID},
		"client_secret": {p.Client.Secret.Reveal()},
		"code_verifier": {verifier.Reveal()},
	}
	return p.tokenCall(ctx, hc, form, code, verifier)
}

// Revoke asks the provider to revoke token (Google's revocation endpoint,
// RFC 7009 shape). A token the provider already considers invalid counts as
// revoked. Errors never contain the token.
func (p Provider) Revoke(ctx context.Context, hc *http.Client, token xoauth2.Secret) error {
	if p.RevokeURL == "" {
		return errors.New("oauthmail: provider has no revocation endpoint")
	}
	hc = noRedirects(hc)
	form := url.Values{"token": {token.Reveal()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.RevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("oauthmail: bad revocation URL")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return scrubbed(err, token, p.Client.Secret)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = decodeLimited(resp, &body)
	if resp.StatusCode == http.StatusBadRequest && body.Error == "invalid_token" {
		return nil // already dead: the goal is met
	}
	return fmt.Errorf("oauthmail: revocation answered HTTP %d %s", resp.StatusCode, oauthCode(body.Error))
}

// imapMailboxCheck is the default MailboxCheck: an XOAUTH2 sign-in at the
// provider's fixed IMAP host, then LOGOUT.
func imapMailboxCheck(allowPrivate bool) MailboxCheck {
	return func(ctx context.Context, p Provider, address string, token xoauth2.Secret) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		sess, err := sync.Dial(ctx, sync.Config{
			Host: p.IMAPHost, Port: p.IMAPPort, TLS: sync.TLSImplicit,
			Auth:              sync.XOAuth2Auth(address, xoauth2.StaticToken(token)),
			AllowPrivateHosts: allowPrivate,
		})
		if err != nil {
			return err
		}
		_ = sess.Logout(ctx)
		return sess.Close()
	}
}

// randomToken is n random bytes, base64url without padding.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauthmail: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomGeneration is a grant's starting rotation generation: random in
// [2^32, 2^62), so a recreated grant never shares a generation with the grant
// it replaced (obligation (c)), while staying far from overflow.
func randomGeneration() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("oauthmail: random: %w", err)
	}
	v := binary.BigEndian.Uint64(b[:]) >> 2 // [0, 2^62)
	if v < 1<<32 {
		v += 1 << 32
	}
	return int64(v), nil
}

// decodeLimited reads a small JSON body (an error answer) into v.
func decodeLimited(resp *http.Response, v any) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
