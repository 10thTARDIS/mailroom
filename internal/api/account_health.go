// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/oauthmail"
	"mailroom/internal/store"
)

// Account/getHealth is the read-only account-health panel surface (spec §12,
// mailroom#43). For each of the caller's mail accounts it reports:
//
//   - syncStatus  — the sync runner's per-account telemetry (last success/attempt,
//     the lag since the last success, the most recent error, and a running
//     consecutive-failure count), read from app.account_sync_status;
//   - deliveryFailures — recent failed/bounced deliveries (AccountDeliveryFailures);
//   - deadLetters — writeback intents that gave up, with their reasons (ListDeadIntents);
//   - sendQuota — today's outbound spend vs. the provider cap (OutboundUsage);
//   - writeback — ADR-028's ingest-only mode: the EFFECTIVE enabled state (the
//     account's column ANDed with the instance-wide switch) and the suppressed-
//     intent ledger (SuppressedIntentCount) — everything Mailroom deliberately
//     did not write back while the account was shadowed.
//
// It also reports one envelope-level (not per-account) signal, when a search
// service is installed:
//
//   - searchStalenessSeconds — how many seconds behind the change log the user's
//     search index is (spec §7's "search may be N minutes stale", mailroom#41):
//     the age of the oldest un-indexed Email change (IndexStalenessSeconds), read
//     from the cursor search.Service.Stats reports. It is per-user (one index per
//     user, not one per mail account) and nil when this server has no search
//     backend installed.
//
// Like EmailSubmission/getSendQuota this is a COMPUTED read that mutates nothing
// and appends no change_log row: account health is OPERATIONAL telemetry, not a
// JMAP-synced object, so it is fetched on demand rather than through /changes
// (CLAUDE.md). It reads through the caller's RLS-scoped UserTx and rejects a
// request naming another user's JMAP account before any row is read (spec §10),
// so it can only ever report the caller's own accounts.
//
// It is gated on urn:mailroom:accounts (capability.go), the same capability the
// Account object and its /get, /query, /changes, /set live under.

const (
	// healthMaxRecentFailures bounds the recent delivery-failure list per account
	// (the panel shows a short tail; count carries the fuller total).
	healthMaxRecentFailures = 10
	// healthMaxDeadReasons bounds the distinct dead-letter reasons per account.
	healthMaxDeadReasons = 10
	// scannerHealthTimeout caps the live VERSION call this endpoint makes. A
	// health panel that hangs because a sidecar is slow has become the outage it
	// was built to report.
	scannerHealthTimeout = 2 * time.Second
)

// healthSyncStatus is one account's sync telemetry. The timestamps and lagSeconds
// are pointers so "never attempted / never succeeded / no error" serialises as
// JSON null, distinct from a zero value.
type healthSyncStatus struct {
	LastSuccessAt       *string `json:"lastSuccessAt"`
	LastAttemptAt       *string `json:"lastAttemptAt"`
	LagSeconds          *int64  `json:"lagSeconds"`
	LastError           *string `json:"lastError"`
	LastErrorAt         *string `json:"lastErrorAt"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
}

// healthDeliveryFailure is one recent failed delivery, flattened for the panel.
type healthDeliveryFailure struct {
	Recipient  string `json:"recipient"`
	StatusCode string `json:"statusCode"`
	Diagnostic string `json:"diagnostic"`
	At         string `json:"at"`
}

type healthDeliveryFailures struct {
	Count  int                     `json:"count"`
	Recent []healthDeliveryFailure `json:"recent"`
}

type healthDeadLetters struct {
	Count   int      `json:"count"`
	Reasons []string `json:"reasons"`
}

type healthSendQuota struct {
	Sent  int `json:"sent"`
	Limit int `json:"limit"`
}

// healthWriteback is ADR-028's per-account ingest-only ledger: how a shadow run
// is legible without a SQL prompt.
//
// Enabled is the EFFECTIVE value — the account's own writeback_enabled column
// ANDed with !Store.WritebackDisabled() — never the column alone, because the
// instance-level flag can shadow an account whose own column still says true
// (ADR-028 §2), and reporting the column verbatim would tell the operator a
// backlog is draining when the instance flag has it parked.
//
// SuppressedIntents is the ingest-only LEDGER (ADR-028 §4): the count of
// writeback intents Mailroom deliberately did NOT apply because the account was
// shadowed when they were enqueued (or suppressed when the mode was switched
// on). It is unlike `dead` intents, which are FAILURES the writeback engine gave
// up on — a suppressed intent is not an error, it is exactly the mode working as
// designed, and it is the reviewable artifact the go-live decision rests on.
//
// ⚠ NEVER conflate the two counts. Adding suppressedIntents to a deadLetters
// total would report a healthy shadow run — every mutation withheld exactly as
// intended — as if it were thousands of writeback failures.
//
// Unlike attachmentScanner (omitempty, because a server can have none
// configured), Writeback is ALWAYS present: every account has an effective
// writeback state whether or not ingest-only mode is in use, so there is no
// "not configured" case to distinguish.
type healthWriteback struct {
	Enabled           bool  `json:"enabled"`
	SuppressedIntents int64 `json:"suppressedIntents"`
}

// accountHealth is one account's full health entry.
type accountHealth struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name"`
	SyncStatus       healthSyncStatus       `json:"syncStatus"`
	DeliveryFailures healthDeliveryFailures `json:"deliveryFailures"`
	DeadLetters      healthDeadLetters      `json:"deadLetters"`
	SendQuota        healthSendQuota        `json:"sendQuota"`
	// Writeback is genuinely per-account (unlike searchStalenessSeconds and
	// attachmentScanner below, which are per-USER and live on the envelope):
	// two accounts on the same user can be in different modes.
	Writeback healthWriteback `json:"writeback"`
	// SyncHold says why this account's sync is held OFF by the server and
	// cannot be switched on yet (#52: a connected Gmail account until the
	// Gmail sync model lands, oauthmail.SyncHold). Computed from the account
	// and a code switch, never stored, so like the rest of this panel it owes
	// no change_log row. Omitted when there is no hold.
	SyncHold *healthSyncHold `json:"syncHold,omitempty"`
	// Auth is how the account signs in and whether that is working (#52,
	// ADR-046 §5). Always present. Never a token, never provider prose: reason
	// is a short stable code.
	Auth healthAuth `json:"auth"`
	// GmailImport is a Gmail OAuth account's import progress (#52, ADR-047
	// §6): computed from the mailbox cursors, the stored messages and the
	// daily meter, never stored, so it owes no change_log row. Omitted for
	// every other account.
	GmailImport *healthGmailImport `json:"gmailImport,omitempty"`
	// SendBlocked says the account is RECEIVE-ONLY: its provider refuses to
	// take mail from it as a policy (ADR-046 §12; code "smtp_auth_disabled"
	// is Microsoft's 5.7.139), since when, and the text to show. Operational
	// state (app.account_sync_status), so no change_log row. Omitted when
	// the account can send.
	SendBlocked *healthSendBlocked `json:"sendBlocked,omitempty"`
}

// healthSendBlocked is a receive-only account's explanation.
type healthSendBlocked struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Since   string `json:"since"`
}

// sendBlockedOf projects a health row's receive-only state, or nil.
func sendBlockedOf(h store.AccountHealth) *healthSendBlocked {
	if h.SendBlockedCode == "" {
		return nil
	}
	sb := &healthSendBlocked{Code: h.SendBlockedCode, Message: store.SendBlockedReason(h.SendBlockedCode)}
	if h.SendBlockedAt != nil {
		sb.Since = jmapDate(*h.SendBlockedAt)
	}
	return sb
}

// healthGmailImport is the newest-first Gmail import's progress.
//
// messagesOnServer is the sum of All Mail, Spam and Trash's message counts at
// their last poll (null before the first). remainingEstimateBytes and etaDays
// are ESTIMATES — the remaining count times the average size imported so far,
// over the history share of the daily budget — and are null until there is an
// average to estimate from. To stop an import, switch the account's sync off
// (Account/set syncEnabled:false): it resumes where it stopped.
type healthGmailImport struct {
	MessagesOnServer       *int64 `json:"messagesOnServer"`
	MessagesImported       int64  `json:"messagesImported"`
	BytesImported          int64  `json:"bytesImported"`
	HistoryComplete        bool   `json:"historyComplete"`
	BytesDownloadedLast24h int64  `json:"bytesDownloadedLast24h"`
	DailyBudgetBytes       int64  `json:"dailyBudgetBytes"`
	RemainingEstimateBytes *int64 `json:"remainingEstimateBytes"`
	EtaDays                *int   `json:"etaDays"`
}

// WithGmailDailyBudget sets the budget Account/getHealth's Gmail import
// progress estimates against (the server's MAILROOM_GMAIL_DAILY_DOWNLOAD_MB).
func WithGmailDailyBudget(bytes int64) Option {
	return func(a *API) { a.gmailBudget = bytes }
}

// defaultGmailDailyBudget is mailsync.DefaultGmailDailyBudget (2,000 MB),
// repeated so the API does not import the sync engine for one number.
const defaultGmailDailyBudget int64 = 2000 * 1000 * 1000

// writebackHeld reports the Gmail writeback hold (oauthmail.WritebackHold):
// the effective writeback state of a held account is off, whatever its column.
func writebackHeld(ac store.Account) bool {
	_, _, held := oauthmail.WritebackHold(ac.AuthKind, ac.ProviderProfile)
	return held
}

// gmailImportOf turns the store's progress into the health section.
func gmailImportOf(p store.GmailImportProgress, budget int64) *healthGmailImport {
	h := &healthGmailImport{
		MessagesOnServer: p.MessagesOnServer, MessagesImported: p.MessagesImported,
		BytesImported: p.BytesImported, HistoryComplete: p.HistoryComplete,
		BytesDownloadedLast24h: p.BytesLast24h, DailyBudgetBytes: budget,
	}
	if p.HistoryComplete {
		zero, z := int64(0), 0
		h.RemainingEstimateBytes, h.EtaDays = &zero, &z
		return h
	}
	if p.MessagesOnServer == nil || p.MessagesImported == 0 || budget <= 0 {
		return h
	}
	left := *p.MessagesOnServer - p.MessagesImported
	if left < 0 {
		left = 0
	}
	est := left * (p.BytesImported / p.MessagesImported)
	share := budget * 9 / 10 // history's share (mailsync historyShare)
	days := int((est + share - 1) / share)
	h.RemainingEstimateBytes, h.EtaDays = &est, &days
	return h
}

// healthAuth is one account's sign-in health. For a password account only
// kind is set. For an OAuth account: provider; state ("active",
// "needs_reauth", "operator_error", or "disconnected" when it has no grant);
// reason, the stable code behind a non-active state (an OAuth error code such
// as "invalid_grant", already reduced to [a-z0-9_] when it was recorded) or
// null; when the state last changed; and when a token was last obtained. The
// refresh time is a COMPUTED read here, like the rest of this panel, which is
// why a refresh owes no change row (ADR-046 §4) — Account/get does not carry
// it.
type healthAuth struct {
	Kind           string  `json:"kind"`
	Provider       *string `json:"provider"`
	State          *string `json:"state"`
	Reason         *string `json:"reason"`
	StateChangedAt *string `json:"stateChangedAt"`
	LastRefreshAt  *string `json:"lastRefreshAt"`
}

// healthOperatorNotice is one provider refusing this server's client
// registration (operator_error): an instance-wide condition only the operator
// can fix, reported to admins only (OAuthOperatorStatus).
type healthOperatorNotice struct {
	Provider string `json:"provider"`
	// Code is the provider's OAuth error code or ours, never provider prose.
	Code       string `json:"code"`
	Since      string `json:"since"`
	LastSeenAt string `json:"lastSeenAt"`
	// Accounts is how many accounts, of every user, have hit it since the
	// server started — an aggregate, never which users.
	Accounts int `json:"accounts"`
}

// OAuthOperatorStatus reports the providers refusing this server's client
// registration (oauthmail.Manager implements it).
type OAuthOperatorStatus interface {
	OperatorIssues() []oauthmail.OperatorIssue
}

// WithOAuthOperatorStatus lets Account/getHealth show admins the providers
// refusing this server (#52). Without it, nothing operator-side is reported.
func WithOAuthOperatorStatus(s OAuthOperatorStatus) Option {
	return func(a *API) { a.oauthOps = s }
}

// authHealthOf builds an account's auth section from its row and its grant
// (nil when it has none).
func authHealthOf(ac store.Account, g *store.OAuthGrant) healthAuth {
	h := healthAuth{Kind: ac.AuthKind}
	if ac.AuthKind != "oauth2" {
		return h
	}
	if g == nil {
		st := authStateDisconnected
		h.State = &st
		if p := oauthmail.ProviderForProfile(ac.ProviderProfile); p != "" {
			h.Provider = &p
		}
		return h
	}
	provider, state := g.Provider, g.State
	h.Provider, h.State = &provider, &state
	if g.State != store.OAuthStateActive {
		h.Reason = strOrNull(g.StateReason)
	}
	changed := jmapDate(g.StateChangedAt)
	h.StateChangedAt = &changed
	h.LastRefreshAt = jmapDatePtr(g.LastRefreshAt)
	return h
}

// healthSyncHold is a stable machine code plus the text a client can show.
type healthSyncHold struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// accountHealthResult is the Account/getHealth response. accountId echoes the
// caller's single JMAP account (which IS the mailroom user); list carries one
// entry per requested (or, when ids is null, every) mail account.
//
// searchStalenessSeconds is NOT per-account: the search index is per-USER (one
// index, one cursor, for every mail account the user has), so it lives on the
// envelope rather than duplicated onto each accountHealth entry. It is a
// pointer so "search is not installed on this server" serialises as JSON null
// (omitted, not a lying zero) rather than claiming an up-to-date index that
// does not exist (mailroom#41).
type accountHealthResult struct {
	AccountID              string          `json:"accountId"`
	List                   []accountHealth `json:"list"`
	SearchStalenessSeconds *float64        `json:"searchStalenessSeconds,omitempty"`
	// AttachmentScanner is per-USER for the same reason searchStaleness is: one
	// scanner and one corpus, not one per mail account. Omitted entirely when no
	// scanner is configured (#395, ADR-027 §5c/§5d/§9).
	AttachmentScanner *healthScanner `json:"attachmentScanner,omitempty"`
	// OperatorNotices is INSTANCE-level and ADMIN-ONLY (#52): the providers
	// refusing this server's client registration, an empty list when none is.
	// Omitted for anyone who is not an admin, and when no provider is
	// configured. This is the operator's notice for operator_error: users are
	// never pushed for it, and Mailroom has no other channel to the operator
	// besides the ERROR line the token Manager logs.
	OperatorNotices *[]healthOperatorNotice `json:"operatorNotices,omitempty"`
}

// healthScanner is the malware scanner's freshness, and the aggregate that makes
// "is my mailbox clean?" answerable (ADR-027 §5c, §5d, §9).
//
// ⚠ IT IS A COMPUTED READ, DELIBERATELY NOT A SYNCED PROPERTY. §9: if verdict age
// and database version synced through change_log, every re-scan in the 7-day
// tier would append change rows and force every client to refetch, several times
// a day, for pure noise. State TRANSITIONS sync; freshness is fetched on demand,
// the searchStalenessSeconds pattern.
type healthScanner struct {
	// Reachable distinguishes "the scanner did not answer" from "no scanner is
	// configured" — the latter omits this object entirely. ⚠ Collapsing the two
	// would report a DOWN scanner exactly like one that was never asked for.
	Reachable bool `json:"reachable"`
	// EngineVersion and DatabaseVersion are what the daemon is running; zero
	// when it did not answer.
	EngineVersion   string `json:"engineVersion,omitempty"`
	DatabaseVersion int    `json:"databaseVersion,omitempty"`
	// DatabaseAgeSeconds is how long ago the loaded signatures were published,
	// nil when the daemon did not say. ⚠ nil is NOT zero: "we cannot tell how
	// old this is" and "it is brand new" are opposite answers, and collapsing
	// them rebuilds §5c's fail-open bug in the UI.
	DatabaseAgeSeconds *float64 `json:"databaseAgeSeconds"`
	// Stale is the fault §5c requires: re-scan tiers gate on the database
	// version CHANGING, so a database that cannot change silently suspends
	// re-scanning while every scan still reports success. An unknown age counts
	// as stale — fail-closed.
	Stale bool `json:"stale"`
	// OldestVerdictAgeSeconds is the age of the least-recently-refreshed verdict
	// still standing, nil when nothing has been scanned.
	OldestVerdictAgeSeconds *float64 `json:"oldestVerdictAgeSeconds"`
	// ByState is §5d's aggregate, PER STATE and never a total: "0 flagged" and
	// "0 flagged because nothing has been scanned" are the same number and
	// opposite answers, so an `unscanned` count degrades the reading rather than
	// vanishing into it.
	ByState map[string]int `json:"byState"`
}

// lagSeconds is the whole seconds elapsed since an account last synced
// successfully, or nil if it never has. It never returns a negative value (a
// clock skew that puts the last success slightly in the future clamps to 0). It
// is pure so it unit-tests without a database.
func lagSeconds(now time.Time, lastSuccess *time.Time) *int64 {
	if lastSuccess == nil {
		return nil
	}
	secs := int64(now.Sub(*lastSuccess) / time.Second)
	if secs < 0 {
		secs = 0
	}
	return &secs
}

// jmapDatePtr renders a nullable timestamp as a nullable JMAP UTCDate.
func jmapDatePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := jmapDate(*t)
	return &s
}

// strOrNull renders "" as JSON null (no recorded error), else the string.
func strOrNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// syncStatusOf projects a store health row (or its zero value, for a
// never-synced account) into the wire shape.
func syncStatusOf(now time.Time, h store.AccountHealth) healthSyncStatus {
	return healthSyncStatus{
		LastSuccessAt:       jmapDatePtr(h.LastSuccessAt),
		LastAttemptAt:       jmapDatePtr(h.LastAttemptAt),
		LagSeconds:          lagSeconds(now, h.LastSuccessAt),
		LastError:           strOrNull(h.LastError),
		LastErrorAt:         jmapDatePtr(h.LastErrorAt),
		ConsecutiveFailures: h.ConsecutiveFailures,
	}
}

func (a *API) accountGetHealth(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string    `json:"accountId"`
		IDs       *[]string `json:"ids"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	// Reject a request naming another user's account BEFORE opening a tx: identity
	// comes from the auth context, never this parameter (spec §10).
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	now := time.Now()
	var list []accountHealth
	var searchStaleness *float64
	var scannerHealth *healthScanner
	var operatorNotices *[]healthOperatorNotice
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var (
			accts []store.Account
			err   error
		)
		if args.IDs == nil {
			// ids:null means "all accounts" (RFC 8620 §5.1).
			accts, err = tx.ListAccounts(ctx)
		} else {
			valid, _, _ := parseIDs(*args.IDs)
			accts, err = tx.GetAccounts(ctx, valid)
		}
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}

		ids := make([]ulid.ULID, len(accts))
		for i, ac := range accts {
			ids[i] = ac.ID
		}
		healthByID, err := tx.AccountHealth(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		grants, err := tx.OAuthGrantsFor(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}

		list = make([]accountHealth, 0, len(accts))
		for _, ac := range accts {
			entry := accountHealth{
				ID:         ac.ID.String(),
				Name:       ac.Name,
				SyncStatus: syncStatusOf(now, healthByID[ac.ID]),
				// syncStatusOf deliberately ignores it: the send side has its
				// own section.
				SendBlocked: sendBlockedOf(healthByID[ac.ID]),
			}
			if code, msg, held := oauthmail.SyncHold(ac.AuthKind, ac.ProviderProfile); held {
				entry.SyncHold = &healthSyncHold{Code: code, Message: msg}
			}
			if ac.AuthKind == "oauth2" && ac.ProviderProfile == "gmail" {
				p, err := tx.GmailImportProgress(ctx, ac.ID, now)
				if err != nil {
					return &MethodError{Type: errServerFail, Description: err.Error()}
				}
				budget := a.gmailBudget
				if budget <= 0 {
					budget = defaultGmailDailyBudget
				}
				entry.GmailImport = gmailImportOf(p, budget)
			}
			var grant *store.OAuthGrant
			if g, ok := grants[ac.ID]; ok {
				grant = &g
			}
			entry.Auth = authHealthOf(ac, grant)

			// Recent delivery failures: fetch the (default-capped) tail for an
			// accurate count, then surface only the first few in the list.
			fails, err := tx.AccountDeliveryFailures(ctx, ac.ID, 0)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			df := healthDeliveryFailures{Count: len(fails), Recent: []healthDeliveryFailure{}}
			for i, f := range fails {
				if i >= healthMaxRecentFailures {
					break
				}
				df.Recent = append(df.Recent, healthDeliveryFailure{
					Recipient:  f.Recipient,
					StatusCode: f.Status,
					Diagnostic: f.Diagnostic,
					At:         jmapDate(f.CreatedAt),
				})
			}
			entry.DeliveryFailures = df

			// Dead-lettered writeback intents: count them and surface the distinct
			// reasons (deduped, capped) so the user sees WHY mirror-backs gave up.
			dead, err := tx.ListDeadIntents(ctx, ac.ID)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			dl := healthDeadLetters{Count: len(dead), Reasons: []string{}}
			seen := make(map[string]bool, len(dead))
			for _, in := range dead {
				if in.DeadReason == "" || seen[in.DeadReason] {
					continue
				}
				seen[in.DeadReason] = true
				dl.Reasons = append(dl.Reasons, in.DeadReason)
				if len(dl.Reasons) >= healthMaxDeadReasons {
					break
				}
			}
			entry.DeadLetters = dl

			// Today's outbound spend vs. the provider cap.
			sent, limit, err := tx.OutboundUsage(ctx, ac.ID)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			entry.SendQuota = healthSendQuota{Sent: sent, Limit: limit}

			// ADR-028: the ingest-only mode and its ledger. Enabled is the EFFECTIVE
			// value (the column ANDed with the instance-wide switch), not the column
			// alone — see healthWriteback's doc comment for why the distinction
			// matters to a reader of this panel.
			suppressed, err := tx.SuppressedIntentCount(ctx, ac.ID)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			entry.Writeback = healthWriteback{
				Enabled:           ac.WritebackEnabled && !a.store.WritebackDisabled() && !writebackHeld(ac),
				SuppressedIntents: suppressed,
			}

			list = append(list, entry)
		}

		// Search staleness (mailroom#41, spec §7): per-USER, not per-account — the
		// index is one per user, not one per mail account — so it is computed once
		// here rather than inside the accounts loop above. Search is optional
		// (SetSearch); when it is not installed the field stays nil and is omitted
		// from the response entirely, rather than claiming an index that does not
		// exist is up to date. A Stats or staleness read failure is likewise
		// swallowed into "omit the field" — this is a nice-to-have health signal,
		// not something that should fail the whole health response.
		if a.search != nil {
			if stats, err := a.search.Stats(ctx, userID); err == nil {
				if secs, err := tx.IndexStalenessSeconds(ctx, stats.Cursor); err == nil {
					searchStaleness = &secs
				}
			}
		}
		// Attachment-scanner freshness (#395, ADR-027 §5c/§5d), per-USER for the
		// same reason as search above. Fail-soft in the same way too: a health
		// panel must not fail because a sidecar is slow.
		scannerHealth = a.scannerHealth(ctx)
		addScannerCorpus(ctx, tx, scannerHealth)
		if a.oauthOps != nil {
			admin, err := tx.SelfIsAdmin(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			if admin {
				notices := []healthOperatorNotice{}
				for _, oi := range a.oauthOps.OperatorIssues() {
					notices = append(notices, healthOperatorNotice{
						Provider: oi.Provider, Code: oi.Code, Since: jmapDate(oi.Since),
						LastSeenAt: jmapDate(oi.LastSeen), Accounts: oi.Accounts,
					})
				}
				operatorNotices = &notices
			}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(accountHealthResult{
		AccountID:              userID.String(),
		List:                   list,
		SearchStalenessSeconds: searchStaleness,
		AttachmentScanner:      scannerHealth,
		OperatorNotices:        operatorNotices,
	}), nil
}

// scannerHealth reads the scanner's freshness and the verdict aggregate.
//
// ⚠ NIL MEANS "NO SCANNER CONFIGURED", and that is the only thing it means. A
// configured scanner that does not answer comes back with Reachable false and
// Stale true — because a scanner nobody can reach is not refreshing anything,
// and reporting it identically to "you never asked for one" would hide the
// difference between a deliberate choice and an outage.
//
// ⚠ IT IS BOUNDED IN TIME. This runs inside a user-facing request, and the
// daemon lives behind a unix socket that can stall. The client applies its own
// ping timeout; this caps it again so a slow sidecar degrades the panel's
// scanner box rather than the whole health response.
//
// ⚠ IT TAKES NO TRANSACTION ON PURPOSE. The freshness half is derived entirely
// from what the daemon says, and keeping it free of the database is what lets
// the states that matter — unreachable, stale, an unknown publication time — be
// unit-tested without standing one up. The corpus aggregate is filled in by the
// caller, which has the transaction.
func (a *API) scannerHealth(ctx context.Context) *healthScanner {
	if a.scanner == nil {
		return nil
	}
	out := &healthScanner{ByState: map[string]int{}}

	vctx, cancel := context.WithTimeout(ctx, scannerHealthTimeout)
	defer cancel()
	if v, err := a.scanner.Version(vctx); err == nil {
		out.Reachable = true
		out.EngineVersion, out.DatabaseVersion = v.Engine, v.Database
		if age, ok := v.Age(time.Now()); ok {
			secs := age.Seconds()
			out.DatabaseAgeSeconds = &secs
		}
		out.Stale = v.Stale(time.Now())
	} else {
		// Unreachable is a fault, not an absence.
		out.Stale = true
	}

	return out
}

// addScannerCorpus fills in §5d's aggregate from the database.
//
// ⚠ IT RUNS WHETHER OR NOT THE DAEMON ANSWERED. What has been scanned so far is
// a fact about the corpus, not about the scanner's current health — and it is
// exactly what a reader needs when the scanner is down, because it says how much
// of the mailbox has a verdict at all.
func addScannerCorpus(ctx context.Context, tx *store.UserTx, h *healthScanner) {
	if h == nil {
		return
	}
	if counts, err := tx.CountAttachmentsByScanStatus(ctx); err == nil {
		h.ByState = counts
	}
	if cov, err := tx.AttachmentVerdictCoverageFor(ctx); err == nil && cov.OldestVerdict != nil {
		secs := time.Since(*cov.OldestVerdict).Seconds()
		h.OldestVerdictAgeSeconds = &secs
	}
}
