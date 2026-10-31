// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	stdsync "sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sync/singleflight"

	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

// Grant is what the Manager needs to refresh one account: the secret half
// (from the sealed envelope) and the queryable half (from app.oauth_grants),
// read together so the generation belongs to the refresh token beside it.
type Grant struct {
	Provider     string
	Address      string
	State        string
	Generation   int64
	RefreshToken xoauth2.Secret
}

// GrantStore persists grants. StoreGrants is the real one (app.oauth_grants +
// the sealed credentials); tests use fakes. Every method is scoped to userID.
type GrantStore interface {
	LoadGrant(ctx context.Context, userID, accountID ulid.ULID) (Grant, error)
	// RotateRefreshToken stores newToken iff the grant is still at
	// seenGeneration (compare-and-swap). won=false means another worker
	// rotated first and nothing was written.
	RotateRefreshToken(ctx context.Context, userID, accountID ulid.ULID, seenGeneration int64, newToken xoauth2.Secret, at time.Time) (won bool, err error)
	// RecordRefresh stamps a refresh that did not rotate.
	RecordRefresh(ctx context.Context, userID, accountID ulid.ULID, at time.Time) error
	// SetState records a grant state transition (a no-op when unchanged).
	SetState(ctx context.Context, userID, accountID ulid.ULID, state, reason string, at time.Time) error
}

// ErrNoGrant: the account's credentials say OAuth but there is no grant row.
// Reconnecting recreates both, so it reads as "reconnect".
var ErrNoGrant = fmt.Errorf("oauthmail: account has no OAuth grant: %w", xoauth2.ErrReauthRequired)

// StateError is returned without any network call when the grant is in a
// state no refresh can leave: needs_reauth.
type StateError struct{ State string }

func (e *StateError) Error() string {
	return fmt.Sprintf("oauthmail: grant is %s; the account must be reconnected", e.State)
}

// Is makes a StateError an xoauth2.ErrReauthRequired.
func (e *StateError) Is(target error) bool { return target == xoauth2.ErrReauthRequired }

const (
	// refreshSkew: an access token is replaced this long before it expires,
	// so a connection never starts with a token about to die mid-command.
	refreshSkew = 5 * time.Minute
	// refreshTimeout bounds one token-endpoint call. It is detached from any
	// single caller's context (see AccessToken).
	refreshTimeout = 30 * time.Second
)

// Manager hands out access tokens per account. One Manager serves the whole
// process — the sync pass, the IDLE watcher, writeback and compose — so they
// share one cache and one in-flight refresh per account.
//
// Concurrency, in two layers (ADR-046 §5):
//
//   - In-process: singleflight per (user, account). N callers needing a token
//     at once cause ONE token-endpoint request, and all N get its result.
//   - Cross-process (the admin CLI, a second replica): compare-and-swap on the
//     grant's generation when a ROTATED refresh token is stored. The loser of
//     the race keeps the access token it was issued (it is valid) and simply
//     does not store its refresh token, so a newer token is never overwritten
//     with an older one. Microsoft does not revoke the previous refresh token
//     on rotation, so losing is never fatal.
type Manager struct {
	providers *Registry
	grants    GrantStore
	http      *http.Client
	log       *slog.Logger
	now       func() time.Time

	flight singleflight.Group

	mu    stdsync.Mutex
	cache map[cacheKey]cachedToken
	// forgets counts Forget calls per account, so a refresh that started
	// before a Forget cannot re-fill the cache after it.
	forgets map[cacheKey]uint64

	// operator holds, per provider, the operator_error this process has seen
	// and not yet seen cleared (OperatorIssues). Guarded by mu.
	operator map[string]*operatorIssue
}

// operatorIssue is one provider's standing refusal of this server.
type operatorIssue struct {
	code     string
	since    time.Time
	last     time.Time
	accounts map[cacheKey]struct{}
}

// OperatorIssue is a provider that is refusing THIS SERVER's client
// registration (operator_error, ADR-046 §5), as this process has observed it.
// It is the operator's notice: users are told only "contact the person who
// runs this server", never to reconnect, and are not pushed (notify.go).
type OperatorIssue struct {
	Provider string
	// Code is the provider's OAuth error code ("invalid_client") or ours
	// ("provider_not_configured"): a short stable token, never provider prose.
	Code string
	// Since is when this process first saw it; LastSeen the latest refusal.
	Since, LastSeen time.Time
	// Accounts is how many distinct accounts have hit it in this process.
	Accounts int
}

type cacheKey struct{ user, account ulid.ULID }

// cachedToken holds the access token as xoauth2.Held, never a bare Secret:
// the field is unexported and lives in Manager's unexported map, and fmt
// renders unexported fields without calling their methods (the rule on
// xoauth2.Secret). The methods below redact wherever they can be called.
type cachedToken struct {
	token  xoauth2.Held
	expiry time.Time
}

func (c cachedToken) String() string {
	return "cachedToken{[redacted], expires " + c.expiry.UTC().Format(time.RFC3339) + "}"
}
func (c cachedToken) GoString() string           { return c.String() }
func (c cachedToken) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }
func (c cachedToken) LogValue() slog.Value       { return slog.StringValue(c.String()) }

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithHTTPClient sets the client used for token-endpoint calls.
func WithHTTPClient(hc *http.Client) ManagerOption { return func(m *Manager) { m.http = hc } }

// WithLogger sets the logger. Nothing logged carries a token.
func WithLogger(l *slog.Logger) ManagerOption { return func(m *Manager) { m.log = l } }

// WithClock sets the clock (tests).
func WithClock(now func() time.Time) ManagerOption { return func(m *Manager) { m.now = now } }

// NewManager builds a Manager over the enabled providers and a grant store.
func NewManager(providers *Registry, grants GrantStore, opts ...ManagerOption) *Manager {
	m := &Manager{
		providers: providers,
		grants:    grants,
		http:      defaultTokenClient,
		log:       slog.Default(),
		now:       time.Now,
		cache:     map[cacheKey]cachedToken{},
		forgets:   map[cacheKey]uint64{},
		operator:  map[string]*operatorIssue{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// AccessToken returns a valid access token for the account, refreshing it if
// the cached one is missing or within refreshSkew of expiry. The error, when
// there is one, matches exactly one of xoauth2.ErrReauthRequired,
// xoauth2.ErrProviderMisconfigured or xoauth2.ErrTokenUnavailable under
// errors.Is, and never contains a token.
//
// The refresh runs detached from ctx (bounded by refreshTimeout): it is shared
// by every concurrent caller, and one caller giving up must not fail the rest.
// Each caller still stops waiting when its own ctx ends.
func (m *Manager) AccessToken(ctx context.Context, userID, accountID ulid.ULID) (xoauth2.Secret, error) {
	key := cacheKey{userID, accountID}
	if tok, ok := m.cached(key); ok {
		return tok, nil
	}
	ch := m.flight.DoChan(userID.String()+"/"+accountID.String(), func() (any, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return m.refresh(rctx, key)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(xoauth2.Secret), nil
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", xoauth2.ErrTokenUnavailable, ctx.Err())
	}
}

// HostsFor returns the fixed mail endpoints of an enabled provider. The sync
// resolver and compose dial OAuth accounts ONLY at these (ADR-046 §1).
func (m *Manager) HostsFor(provider string) (xoauth2.Hosts, bool) {
	p, ok := m.providers.Lookup(provider)
	if !ok {
		return xoauth2.Hosts{}, false
	}
	return p.Hosts(), true
}

// Invalidate drops the cached token for the account if it is still rejected —
// call it when IMAP or SMTP refused a cached token, so the next AccessToken
// refreshes once more before the grant is judged (ADR-046 §5). A token that
// has already been replaced is left alone.
func (m *Manager) Invalidate(userID, accountID ulid.ULID, rejected xoauth2.Secret) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := cacheKey{userID, accountID}
	if c, ok := m.cache[key]; ok && c.token.Get() == rejected {
		delete(m.cache, key)
	}
}

// Forget drops whatever the Manager holds for the account — its cached access
// token — unconditionally. Called when the grant ends (disconnect, account
// deletion: obligation (b) of ADR-046 §9), so no access token outlives its
// grant in this process's memory. A refresh already in flight for the account
// still returns its token to the callers waiting on it, but does NOT put it in
// the cache (the forget epoch moved under it), so the next caller loads the
// grant again and finds none.
func (m *Manager) Forget(userID, accountID ulid.ULID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := cacheKey{userID, accountID}
	delete(m.cache, key)
	m.forgets[key]++
}

// TokenSource returns the account's token source for the protocol layers:
// Token is AccessToken, Reject is Invalidate (ADR-046 §9 obligation (a)).
func (m *Manager) TokenSource(userID, accountID ulid.ULID) xoauth2.TokenSource {
	return accountTokens{m: m, user: userID, account: accountID}
}

// accountTokens binds a Manager to one account. It holds no token.
type accountTokens struct {
	m             *Manager
	user, account ulid.ULID
}

func (a accountTokens) Token(ctx context.Context) (xoauth2.Secret, error) {
	return a.m.AccessToken(ctx, a.user, a.account)
}

func (a accountTokens) Reject(tok xoauth2.Secret) { a.m.Invalidate(a.user, a.account, tok) }

func (m *Manager) cached(key cacheKey) (xoauth2.Secret, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cache[key]
	if !ok || !m.now().Before(c.expiry.Add(-refreshSkew)) {
		return "", false
	}
	return c.token.Get(), true
}

func (m *Manager) refresh(ctx context.Context, key cacheKey) (xoauth2.Secret, error) {
	// A flight that finished just before this one started may have filled
	// the cache; singleflight only collapses calls that overlap.
	if tok, ok := m.cached(key); ok {
		return tok, nil
	}
	m.mu.Lock()
	epoch := m.forgets[key]
	m.mu.Unlock()
	log := m.log.With("user_id", key.user.String(), "account_id", key.account.String())

	g, err := m.grants.LoadGrant(ctx, key.user, key.account)
	if err != nil {
		if errors.Is(err, store.ErrOAuthGrantNotFound) {
			return "", ErrNoGrant
		}
		return "", fmt.Errorf("oauthmail: load grant: %w: %w", xoauth2.ErrTokenUnavailable, err)
	}
	if g.State == store.OAuthStateNeedsReauth {
		// The provider already said the grant is dead. Asking again cannot
		// change that and would only add noise at the provider; the user's
		// reconnect is what returns the grant to active.
		return "", &StateError{State: g.State}
	}
	log = log.With("provider", g.Provider)

	p, ok := m.providers.Lookup(g.Provider)
	if !ok {
		rerr := &RefreshError{Provider: g.Provider, Class: FailureOperator, Code: "provider_not_configured"}
		m.setState(ctx, log, key, store.OAuthStateOperatorError, rerr.Code)
		m.noteOperator(g.Provider, rerr.Code, key)
		log.Error("oauth provider for this grant is not configured on this server",
			"class", rerr.Class.String(), "code", rerr.Code)
		return "", rerr
	}

	resp, err := p.Refresh(ctx, m.http, g.RefreshToken)
	if err != nil {
		var rerr *RefreshError
		if errors.As(err, &rerr) {
			switch rerr.Class {
			case FailureNeedsReauth:
				m.setState(ctx, log, key, store.OAuthStateNeedsReauth, rerr.Code)
				log.Warn("oauth grant needs the user to reconnect", "class", rerr.Class.String(), "code", rerr.Code)
			case FailureOperator:
				m.setState(ctx, log, key, store.OAuthStateOperatorError, rerr.Code)
				m.noteOperator(g.Provider, rerr.Code, key)
				// The operator's problem, said loudly where the operator looks.
				log.Error("oauth provider refused this server's client registration",
					"class", rerr.Class.String(), "code", rerr.Code)
			default:
				log.Info("oauth token refresh failed; will retry", "class", rerr.Class.String(), "code", rerr.Code)
			}
		}
		return "", err
	}

	now := m.now()
	// The provider accepted this server's client: whatever operator_error was
	// standing for it is over.
	m.noteOperator(g.Provider, "", key)
	if !resp.RefreshToken.Empty() && resp.RefreshToken != g.RefreshToken {
		won, err := m.grants.RotateRefreshToken(ctx, key.user, key.account, g.Generation, resp.RefreshToken, now)
		switch {
		case err != nil:
			// The access token is good and the previous refresh token stays
			// valid (Microsoft does not revoke it on rotation), so this is
			// survivable; it is logged so a persistent failure is seen.
			log.Error("could not store the rotated refresh token", "err", err.Error())
		case !won:
			log.Info("another worker rotated this grant first; keeping its refresh token")
		}
	} else if err := m.grants.RecordRefresh(ctx, key.user, key.account, now); err != nil {
		log.Warn("could not record the token refresh", "err", err.Error())
	}
	if g.State == store.OAuthStateOperatorError {
		// The operator fixed the client registration: the grant works again
		// without anyone reconnecting.
		m.setState(ctx, log, key, store.OAuthStateActive, "")
	}

	m.mu.Lock()
	if m.forgets[key] == epoch {
		m.cache[key] = cachedToken{token: xoauth2.Hold(resp.AccessToken), expiry: now.Add(resp.ExpiresIn)}
	}
	m.mu.Unlock()
	return resp.AccessToken, nil
}

// OperatorIssues lists the providers currently refusing this server's client
// registration, oldest first. It is in-process memory, not a table: the
// condition is the server's, not any account's, and it is re-established on
// the next refresh after a restart, because operator_error keeps refreshing on
// the normal backoff (a needs_reauth grant does not). A provider's entry is
// cleared by its next successful refresh.
func (m *Manager) OperatorIssues() []OperatorIssue {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OperatorIssue, 0, len(m.operator))
	for p, oi := range m.operator {
		out = append(out, OperatorIssue{Provider: p, Code: oi.code, Since: oi.since, LastSeen: oi.last, Accounts: len(oi.accounts)})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// noteOperator records (code != "") or clears (code == "") a provider's
// operator_error.
func (m *Manager) noteOperator(provider, code string, key cacheKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if code == "" {
		delete(m.operator, provider)
		return
	}
	now := m.now()
	oi := m.operator[provider]
	if oi == nil || oi.code != code {
		oi = &operatorIssue{code: code, since: now, accounts: map[cacheKey]struct{}{}}
		m.operator[provider] = oi
	}
	oi.last = now
	oi.accounts[key] = struct{}{}
}

func (m *Manager) setState(ctx context.Context, log *slog.Logger, key cacheKey, state, reason string) {
	if err := m.grants.SetState(ctx, key.user, key.account, state, reason, m.now()); err != nil {
		log.Error("could not record the oauth grant state", "state", state, "err", err.Error())
	}
}
