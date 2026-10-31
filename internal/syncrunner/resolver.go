// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/mailsync"
	"mailroom/internal/oauthmail"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/xoauth2"
)

// MailBlobInfoV1 is the HKDF domain-separation label for the per-user mail-blob
// data-encryption key. The 0.5 scheme derives this key storage-free from the
// master key (salt=userID), mirroring the credentials key; the wrapped
// random-KEK/DEK hierarchy is 1.0 hardening.
const MailBlobInfoV1 = "mailroom-mail-blob-v1"

// MailBlobSchemeV1 is the sentinel DEKID recorded for blobs written under the
// derived mail-blob key: the scheme derives, it does not store a key.
var MailBlobSchemeV1 = onboarding.SchemeULID("mail-blob-v1")

// CredentialResolver turns a stored account into a ready-to-sync AccountSync:
// it re-derives the per-user credentials key, opens the sealed secret, builds
// the sync.Config, and derives the mail-blob DEK. It satisfies the runner's
// Prepare via its Prepare method.
//
// TLS assumption: app.accounts carries no TLS-mode column in 0.5, so the
// resolver dials implicit TLS (993), the expected mode for real accounts.
type CredentialResolver struct {
	keys crypto.KeyProvider

	// searchRate is the per-account UID SEARCH ceiling in searches per minute,
	// 0 meaning unpaced (#638). See WithSearchRate.
	searchRate int

	// limiterMu guards limiters, the per-account SEARCH-budget registry.
	//
	// ⚠ ONE LIMITER PER ACCOUNT IS THE WHOLE POINT, so this map must outlive
	// any single Prepare call. The server's budget is per ACCOUNT, not per
	// connection: the observation pass and the writeback drain open different
	// sessions against the same account and spend the same 60/min, so a limiter
	// created fresh per Prepare would let the two halves each burst to the
	// ceiling and the account would still be refused. Prepare is called once per
	// pass per account AND once per writeback nudge, and every one of those
	// calls must hand back the same Limiter value.
	limiterMu stdsync.Mutex
	limiters  map[ulid.ULID]sync.Limiter

	// allowPrivateHosts is copied onto every Config this resolver builds, so
	// every IMAP dial of a stored account (sync pass, pool, IDLE, writeback
	// drain) is held to the egress rule account validation applied. See
	// WithAllowPrivateHosts.
	allowPrivateHosts bool

	// tokens issues access tokens for OAuth accounts (ADR-046). Nil means no
	// provider is configured; an OAuth account then fails Prepare with a cause
	// naming that, and is never dialled with LOGIN. See WithAccessTokens.
	tokens AccessTokens

	// gmail is the Gmail sync model's configuration, handed to every Gmail
	// OAuth account's AccountSync (#52, ADR-047). See WithGmailSync.
	gmail mailsync.GmailSync

	// syncHold decides whether an account's sync is held off by the server
	// (oauthmail.SyncHold; ADR-046 §10). See WithSyncHold.
	syncHold func(authKind, providerProfile string) (code, message string, held bool)
}

// AccessTokens issues a current OAuth access token for an account
// (internal/oauthmail.Manager implements it). It is declared here, by its
// consumer, so this package does not depend on the grant store.
//
// The token is asked for at AUTHENTICATION time, once per connection, through
// the source Prepare puts in sync.Config.Auth — never at Prepare time, so a
// Config held for a long pass or an IDLE watcher never carries a stale token.
// A refused token goes back through the source's Reject, so the one retry
// refreshes (ADR-046 §9 obligation (a)).
type AccessTokens interface {
	// TokenSource is the account's token source: Token issues, Reject drops
	// a refused token.
	TokenSource(userID, accountID ulid.ULID) xoauth2.TokenSource
	// HostsFor returns an enabled provider's fixed endpoints (ADR-046 §1).
	HostsFor(provider string) (xoauth2.Hosts, bool)
}

// PinOAuthHost checks an OAuth account's stored host and port against its
// provider's registry entry and returns the registry's. A row that names
// anything else is refused with xoauth2.ErrHostMismatch — never dialled, and
// never silently corrected, so the inconsistency surfaces — and logged with
// ids only. port 0 in the row means "the default" and matches. Shared by the
// sync resolver (IMAP) and compose (SMTP).
func PinOAuthHost(userID, accountID ulid.ULID, provider, kind, rowHost string, rowPort int, wantHost string, wantPort int) error {
	if rowHost == wantHost && (rowPort == 0 || rowPort == wantPort) {
		return nil
	}
	slog.Warn("oauth account's stored mail server differs from its provider's; refusing to connect",
		"user_id", userID.String(), "account_id", accountID.String(), "provider", provider, "protocol", kind)
	return fmt.Errorf("account %s (%s %s): %w", accountID, provider, kind, xoauth2.ErrHostMismatch)
}

// ErrOAuthNotConfigured is Prepare's refusal of an OAuth account when the
// resolver has no token source.
var ErrOAuthNotConfigured = errors.New("syncrunner: this account signs in with OAuth, which is not configured on this server")

// ErrAwaitingReconnect is Prepare's refusal of an account whose OAuth grant
// needs its user to reconnect (#52, ADR-046 §5). It matches
// xoauth2.ErrReauthRequired under errors.Is. The sync lists already leave such
// an account out (store.ListSyncableAccounts); this is the same rule at the
// one place every dial of a stored account is prepared — the admin CLI's
// sync-once included — so nothing opens a connection only to fail it.
var ErrAwaitingReconnect = fmt.Errorf("syncrunner: the account must be reconnected before it can sync: %w", xoauth2.ErrReauthRequired)

// ErrSyncHeld is Prepare's refusal of an account whose sync the server holds
// off (oauthmail.SyncHold: a Gmail OAuth account while GmailSyncReady is
// false). The doors that switch sync on already refuse; this is the same rule
// where every dial of a stored account is prepared, so an account that is
// somehow enabled anyway is still never synced. Fail closed.
var ErrSyncHeld = errors.New("syncrunner: this account's sync is held off by the server")

// NoSyncHold is a WithSyncHold check that holds nothing, for READ-ONLY
// callers that dial a held account without ever ingesting from it (the
// Gmail census, which exists to be run before the first import).
func NoSyncHold(string, string) (string, string, bool) { return "", "", false }

// NewCredentialResolver builds a resolver over a master-key provider. Without
// WithSearchRate it paces nothing, which is the pre-#638 behaviour.
func NewCredentialResolver(keys crypto.KeyProvider, opts ...ResolverOption) *CredentialResolver {
	r := &CredentialResolver{
		keys: keys, limiters: map[ulid.ULID]sync.Limiter{},
		gmail: mailsync.GmailSync{
			DailyBudgetBytes:   mailsync.DefaultGmailDailyBudget,
			LabelSweepInterval: mailsync.DefaultGmailLabelSweepInterval,
		},
		syncHold: oauthmail.SyncHold,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ResolverOption configures a CredentialResolver.
type ResolverOption func(*CredentialResolver)

// WithSearchRate paces every UID SEARCH an account issues to at most perMinute
// per minute, shared across every session built for that account (#638).
// perMinute <= 0 disables pacing entirely.
func WithSearchRate(perMinute int) ResolverOption {
	return func(r *CredentialResolver) { r.searchRate = perMinute }
}

// WithAllowPrivateHosts sets sync.Config.AllowPrivateHosts on every Config the
// resolver builds: MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527, #625 §3).
// Without this option the account's IMAP host must resolve to public
// addresses on EVERY dial, which is the safe default and the one a resolver
// that was never told otherwise gets.
func WithAllowPrivateHosts(allow bool) ResolverOption {
	return func(r *CredentialResolver) { r.allowPrivateHosts = allow }
}

// WithAccessTokens gives the resolver a token source for OAuth accounts
// (ADR-046). Without it, OAuth accounts fail Prepare with ErrOAuthNotConfigured.
func WithAccessTokens(t AccessTokens) ResolverOption {
	return func(r *CredentialResolver) { r.tokens = t }
}

// WithGmailSync sets the Gmail sync model's daily download budget (bytes; a
// non-positive value keeps the default — there is no "unbudgeted" Gmail) and
// label-sweep interval (0 disables the sweep; negative keeps the default) for
// every Gmail OAuth account (#52, ADR-047 §4, §6).
func WithGmailSync(dailyBudgetBytes int64, labelSweep time.Duration) ResolverOption {
	return func(r *CredentialResolver) {
		if dailyBudgetBytes > 0 {
			r.gmail.DailyBudgetBytes = dailyBudgetBytes
		}
		if labelSweep >= 0 {
			r.gmail.LabelSweepInterval = labelSweep
		}
	}
}

// WithSyncHold replaces the sync-hold check (default oauthmail.SyncHold).
// Tests inject the Gmail hold lifted; the census passes NoSyncHold.
func WithSyncHold(f func(authKind, providerProfile string) (code, message string, held bool)) ResolverOption {
	return func(r *CredentialResolver) {
		if f != nil {
			r.syncHold = f
		}
	}
}

// isGmailOAuth reports whether an account syncs on the Gmail model: a Gmail
// OAuth account. ADR-047 governs only those; a Gmail account added with an
// app password stays on the generic path (ADR-047, Consequences).
func isGmailOAuth(acct store.Account) bool {
	return acct.AuthKind == "oauth2" && acct.ProviderProfile == "gmail"
}

// searchLimiterFor returns the account's shared SEARCH limiter, creating it on
// first use. It returns nil — meaning "no pacing" — when the rate is disabled;
// sync.Config.SearchLimiter documents nil as the off switch.
//
// ⚠ Returned through the two-value map read, never the one-value form: a miss
// on a map of interface values yields a nil interface that is indistinguishable
// from a stored one, which is exactly how a "shared" limiter quietly becomes no
// limiter at all.
func (r *CredentialResolver) searchLimiterFor(accountID ulid.ULID) sync.Limiter {
	if r.searchRate <= 0 {
		return nil
	}
	r.limiterMu.Lock()
	defer r.limiterMu.Unlock()
	if r.limiters == nil {
		r.limiters = map[ulid.ULID]sync.Limiter{}
	}
	if l, ok := r.limiters[accountID]; ok {
		return l
	}
	// Burst 1: strict spacing. A burst of B on top of perMinute would put
	// B+perMinute searches into one 60-second window, which is precisely the
	// window Migadu counts — the pacing would then be refused for the same
	// reason the unpaced code was.
	l := sync.NewSearchLimiter(r.searchRate, 1)
	r.limiters[accountID] = l
	return l
}

// Prepare implements the runner's Prepare seam.
func (r *CredentialResolver) Prepare(ctx context.Context, userID ulid.ULID, acct store.Account) (mailsync.AccountSync, error) {
	if acct.AwaitingReconnect() {
		return mailsync.AccountSync{}, fmt.Errorf("account %s: %w", acct.ID, ErrAwaitingReconnect)
	}
	if code, _, held := r.syncHold(acct.AuthKind, acct.ProviderProfile); held {
		return mailsync.AccountSync{}, fmt.Errorf("account %s (%s): %w", acct.ID, code, ErrSyncHeld)
	}
	master, err := r.keys.MasterKey(ctx)
	if err != nil {
		return mailsync.AccountSync{}, fmt.Errorf("syncrunner: master key: %w", err)
	}
	defer crypto.Zero(master)

	creds, err := onboarding.OpenCredentials(master, userID, acct.CredentialsCT)
	if err != nil {
		return mailsync.AccountSync{}, fmt.Errorf("syncrunner: account %s: %w", acct.ID, err)
	}

	dek, err := crypto.DeriveKey(master, userID[:], MailBlobInfoV1)
	if err != nil {
		return mailsync.AccountSync{}, fmt.Errorf("syncrunner: derive mail dek: %w", err)
	}

	port := acct.IMAPPort
	if port == 0 {
		port = 993
	}
	cfg := sync.Config{
		Host: acct.IMAPHost,
		Port: port,
		TLS:  sync.TLSImplicit,
		// Shared, not per-Config: this same Config value is what the
		// writeback drain dials with (Runner.drainOnly and
		// Runner.syncAccount both pass req.Config straight to
		// writeback.Runner.DrainAccount), so observation and writeback
		// draw on one budget — #638's requirement.
		SearchLimiter: r.searchLimiterFor(acct.ID),
		// The account's host was address-checked when it was validated;
		// that answer says nothing about what it resolves to NOW. sync.Dial
		// re-checks on every connection and dials the checked address
		// (#625 §3), under this same switch.
		AllowPrivateHosts: r.allowPrivateHosts,
	}
	if creds.OAuth != nil {
		// An OAuth account (ADR-046): XOAUTH2 as the mailbox address, with the
		// token fetched when each connection authenticates.
		if r.tokens == nil {
			crypto.Zero(dek)
			return mailsync.AccountSync{}, fmt.Errorf("account %s: %w", acct.ID, ErrOAuthNotConfigured)
		}
		// The host is the PROVIDER's, never the row's (ADR-046 §1): a token
		// is only ever sent to the provider that issued it.
		hosts, ok := r.tokens.HostsFor(creds.OAuth.Provider)
		if !ok {
			crypto.Zero(dek)
			return mailsync.AccountSync{}, fmt.Errorf("account %s: provider %q: %w", acct.ID, creds.OAuth.Provider, ErrOAuthNotConfigured)
		}
		if err := PinOAuthHost(userID, acct.ID, creds.OAuth.Provider, "imap", acct.IMAPHost, acct.IMAPPort,
			hosts.IMAPHost, hosts.IMAPPort); err != nil {
			crypto.Zero(dek)
			return mailsync.AccountSync{}, err
		}
		cfg.Host, cfg.Port = hosts.IMAPHost, hosts.IMAPPort
		cfg.Auth = sync.XOAuth2Auth(creds.IMAPUsername, r.tokens.TokenSource(userID, acct.ID))
	} else {
		// A password account: LOGIN, exactly as before ADR-046.
		cfg.Username = creds.IMAPUsername
		cfg.Password = creds.IMAPPassword
	}
	req := mailsync.AccountSync{
		UserID:    userID,
		AccountID: acct.ID,
		Config:    cfg,
		DEK:       dek,
		DEKID:     MailBlobSchemeV1,
		// The profile's folder-name map and non-mail folders (ADR-046 §12).
		ProviderProfile: acct.ProviderProfile,
	}
	if isGmailOAuth(acct) {
		g := r.gmail
		req.Gmail = &g
	}
	return req, nil
}
