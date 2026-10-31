// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/authserv"
	"mailroom/internal/pgdsn"
)

// Default values used when the corresponding environment variable is unset.
const (
	DefaultListenAddr    = ":8080"
	DefaultMasterKeyFile = "/run/secrets/mailroom_master_key"
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "json"
	DefaultSyncInterval  = 5 * time.Minute
	// DefaultSyncMaxBackoff caps the per-account exponential backoff the sync loop
	// applies to a repeatedly-failing account (mailroom#103). Backoff doubles from
	// the account's effective interval on each consecutive failure and is clamped
	// here, so a broken account is retried at most this often.
	DefaultSyncMaxBackoff = 30 * time.Minute
	// DefaultIMAPIdle enables the IMAP IDLE watcher by default (mailroom#103
	// Stage 2). IDLE makes new mail sync within seconds instead of waiting for
	// the poll tick, and it is PURELY ADDITIVE — a server without IDLE support
	// auto-falls-back to polling, and disabling it only loses the speedup, never
	// mail — so on-by-default is the safe, desired behavior.
	DefaultIMAPIdle = true
	// DefaultLocalDKIM enables the local DKIM check at ingest by default (#776).
	// It acts only on mail with no trusted Authentication-Results, it can only
	// add a "Verified by Mailroom" mark (a failure is never displayed), and its
	// cost is bounded per message, so on-by-default is the safe, desired
	// behavior.
	DefaultLocalDKIM = true
	// DefaultImageProxy enables the image proxy by default (ADR-026 decision C,
	// #55 increment 3). ⚠ ON BY DEFAULT IS THE DECISION: §3c constrains the
	// proxy to fetch only what the client would otherwise have fetched itself,
	// so turning it on does not add egress, it MOVES an existing fetch from the
	// reader's browser to the server. A privacy feature nobody enables protects
	// nobody.
	DefaultImageProxy = true
	// DefaultImageProxyPrefetch fetches allowed senders' images when mail
	// ARRIVES (#922), so an image behind a signed, expiring URL is cached while
	// the URL still works. It acts only where the proxy itself would fetch at
	// open (the proxy's own remote-content rule), never on a beacon, and only
	// when the proxy and its cache are on.
	DefaultImageProxyPrefetch = true
	// DefaultImageProxyPrefetchMaxAge is the prefetch's arrival window: mail
	// received longer ago than this is never prefetched, so a first sync of
	// years of history does not contact every sender in it at once.
	DefaultImageProxyPrefetchMaxAge = 7 * 24 * time.Hour
	// DefaultIMAPIdleTimeout is how often an IDLE command is re-issued to keep
	// the connection alive (RFC 2177: a server may drop an IDLE after ~30 min,
	// so re-issue safely before that).
	DefaultIMAPIdleTimeout = 29 * time.Minute
	// DefaultIMAPPool enables the per-account warm sync connection pool by default
	// (mailroom#103 Stage 3). The pool keeps ONE authenticated sync connection per
	// account and reuses it across passes instead of dialing TCP+TLS+LOGIN every
	// time — worthwhile now that IDLE triggers frequent syncs. It is SAFE on by
	// default: a warm connection is NOOP-probed before every reuse, so a server
	// that drops idle connections simply makes the probe fail and the pool
	// re-dials — it never skips or corrupts a sync.
	DefaultIMAPPool = true
	// DefaultIMAPPoolIdleTimeout evicts a warm pooled connection that has sat
	// unused longer than this, so the pool never hands a pass a soon-stale one
	// (it re-dials instead of paying a doomed probe). A few multiples of the base
	// sync interval: long enough that back-to-back and IDLE-triggered passes reuse
	// the connection, short enough that it is not held near a server's idle cutoff.
	DefaultIMAPPoolIdleTimeout = 5 * time.Minute
	DefaultBlobRoot            = "/var/lib/mailroom/blobs"
	DefaultWebRoot             = "/srv/web"
	// DefaultUndoSendWindow is the hold before a send actually goes out — long
	// enough to catch the "wait, no" reflex, short enough that Send still feels
	// like Send.
	DefaultUndoSendWindow = 10 * time.Second
	// DefaultSearchRoot is where the per-user search indexes live (spec §7). It
	// is a DISPOSABLE CACHE: never back it up, and losing it costs only a
	// reindex. It must sit on an at-rest-encrypted volume — the index holds the
	// plaintext of every message, including mail that arrived encrypted
	// (spec §7/§11, ADR-006).
	DefaultSearchRoot = "/var/lib/mailroom/search"
	// DefaultIndexInterval is how often the indexer drains the change log when
	// idle. Seconds, not minutes: spec §15's "new mail visible within seconds"
	// includes being findable.
	DefaultIndexInterval = 5 * time.Second
	// DefaultIMAPSearchRate is how many UID SEARCHes one account may issue per
	// minute (#638).
	//
	// ⚠ 30, NOT 60, AND THAT IS NOT CAUTION FOR ITS OWN SAKE. Migadu's measured
	// ceiling is 60 searches per rolling minute and it is charged PER ACCOUNT,
	// not per connection, so Mailroom is not the only spender: the writeback
	// drain issues UID SEARCHes of its own (SearchMessageID/UIDPresent), and so
	// do the user's phone and webmail. Pacing to the ceiling would leave those
	// nothing and get them refused instead of us. Half the ceiling is the
	// headroom that lets writeback be switched on at all.
	//
	// The cost is bounded: an account observes each mailbox hourly, so 750
	// mailboxes average ~13 searches/minute — well under 30. The rate only
	// binds during a first sync or a re-formed cohort, exactly where the
	// refusals were.
	DefaultIMAPSearchRate = 30

	// DefaultGmailDailyDownloadMB is a Gmail account's daily download budget
	// in MB (10^6 bytes), below Gmail's documented 2,500 MB/day (#52,
	// ADR-047 §6). DefaultGmailLabelSweepInterval is how often the Gmail
	// label sweep re-reads every imported message's labels.
	DefaultGmailDailyDownloadMB    = 2000
	DefaultGmailLabelSweepInterval = 6 * time.Hour
	// The CONDSTORE-less flag sweep (#52 increment 6, ADR-046 §12): on a
	// server without CONDSTORE (Outlook/Microsoft 365, some others) nothing
	// else ever re-reads an existing message's flags, so a message read or
	// starred on a phone stays unread here forever. Every
	// DefaultFlagSweepInterval the newest DefaultFlagSweepWindow UIDs of each
	// mailbox are re-read (FLAGS only, ~50 bytes a message), and every
	// DefaultFlagSweepFullInterval the whole mailbox is, DefaultFlagSweepFullChunk
	// UIDs per mailbox per pass.
	DefaultFlagSweepInterval     = 5 * time.Minute
	DefaultFlagSweepWindow       = 500
	DefaultFlagSweepFullInterval = 6 * time.Hour
	DefaultFlagSweepFullChunk    = 2000
	// DefaultReapInterval is how often each mailbox's provenance is reconciled
	// against the server's live UID set — how quickly a message deleted or moved
	// away in ANOTHER IMAP client leaves its Mailroom folder (spec §4, #86).
	//
	// Deliberately far slower than the sync interval. The incremental poll is
	// cheap (a UID range above the last-seen UID); the reap asks for a whole
	// mailbox's UID set. That is affordable — UIDs are 4 bytes, so even a
	// 100k-message folder is a few hundred kilobytes — but it is not free, and
	// hourly is the right service level for "the message I deleted on my phone is
	// gone here too". New mail still arrives on the sync interval.
	DefaultReapInterval = time.Hour
	// DefaultTrashRetention is how long a trashed message dwells in Trash before
	// the purge job hard-deletes it (spec §4 / ADR-009: 30 days, configurable).
	// It is the window in which a Restore is still possible.
	DefaultTrashRetention = 30 * 24 * time.Hour
	// DefaultPurgeInterval is how often the purge job sweeps a user's Trash for
	// messages past their purge_after and hard-deletes them (ADR-009). Like the
	// reaper it runs far slower than the incremental poll: the sweep is a cheap
	// indexed scan (messages_user_trash), but hard deletion is destructive and
	// unattended, so an hourly cadence is the right service level — a message is
	// purged within an hour of its 30-day window closing, not the instant it does.
	DefaultPurgeInterval = time.Hour
	// DefaultChangeLogRetention is how far back change_log history is kept
	// before the weekly compaction job (internal/changelog, mailroom#526,
	// ADR-003) deletes it and raises change_floor to match (data-model §3.3).
	// A client that has not synced in longer than this gets
	// cannotCalculateChanges exactly once and refetches — correct, bounded,
	// and cheap; this is the knob that trades "how long a truly offline
	// client can come back for free" against "how much history the log
	// carries forever".
	DefaultChangeLogRetention = 90 * 24 * time.Hour
	// MinChangeLogRetention is the floor Load() enforces on
	// MAILROOM_CHANGE_LOG_RETENTION. A typo that collapsed retention to
	// minutes would force EVERY client — not just one that has genuinely
	// been offline — through cannotCalculateChanges on its very next sync,
	// every week, forever; refusing to load below a week makes that
	// mistake a config error instead of a standing correctness surprise.
	MinChangeLogRetention = 7 * 24 * time.Hour
	// DefaultJobRetention is how long a finished ('done') job row is kept
	// before the daily job sweep deletes it (migration 00084). Nothing reads a
	// done row except someone investigating, so a month is plenty.
	DefaultJobRetention = 30 * 24 * time.Hour
	// DefaultDeadJobRetention is the same for a dead-lettered job, kept three
	// times as long so a failure stays inspectable after it is noticed.
	DefaultDeadJobRetention = 90 * 24 * time.Hour
	// MinJobRetention is the floor Load() enforces on both job windows: a typo
	// that collapsed one to minutes would delete the evidence of a job that
	// failed this morning.
	MinJobRetention = 24 * time.Hour

	// DefaultRateLimitPerSecond / DefaultRateLimitBurst are the enforced per-user
	// API rate-limit defaults (spec §10: enforced in v1.0, admin UI may lag).
	// They mirror internal/api's own defaults; carrying them here lets an operator
	// tune the limit without a rebuild. A non-positive value disables limiting.
	DefaultRateLimitPerSecond = 50.0
	DefaultRateLimitBurst     = 100

	// DefaultWKDRateLimitPerSecond / DefaultWKDRateLimitBurst bound the
	// ANONYMOUS key surface (#264). They mirror internal/keypub's own defaults
	// for the same reason the pair above mirrors internal/api's: carrying them
	// here lets an operator tune without a rebuild, and config must not import
	// a service package to read a number.
	//
	// ⚠ Both copies must move together. keypub has a test asserting a
	// DEFAULT-constructed service throttles, so a drift that turned limiting
	// off would fail there — but a drift that merely changed the NUMBER would
	// not, and this comment is the only thing pointing at the other copy.
	DefaultWKDRateLimitPerSecond = 5.0
	DefaultWKDRateLimitBurst     = 20
)

// MinSessionKeyLen is the minimum accepted length, in bytes, of the session
// signing key when OIDC auth is configured. It matches auth.MinSigningKeyLen;
// the value is duplicated here rather than imported so internal/config keeps
// no dependency on internal/auth.
const MinSessionKeyLen = 32

// DevAuthAckPhrase is the exact value EnvDevAuthAck must carry for the dev-auth
// escape hatch to be honored. It spells out the consequence rather than saying
// "yes", so that a config carrying it cannot be read as anything else.
const DevAuthAckPhrase = "this-instance-has-no-authentication"

// Environment variable names. Exported so callers/tests can reference them
// without repeating string literals.
const (
	EnvDatabaseURL = "MAILROOM_DATABASE_URL"
	// EnvDatabasePasswordFile names a file (a Docker secret, in the shipped
	// deployment) whose trimmed contents are injected into EnvDatabaseURL's
	// (or its _FILE's) userinfo as the password, via internal/pgdsn (#537).
	// It is NOT itself a "_FILE" indirection of another variable — it always
	// names a path directly — because the value it carries is never anything
	// but a file: a password never belongs in a plain environment variable.
	//
	// Optional: a URL that already carries its own password (or needs none,
	// e.g. peer/trust auth) works unchanged when this is unset. Setting it
	// AND embedding a password in the URL is a configuration error — see
	// internal/pgdsn.WithPasswordFile.
	EnvDatabasePasswordFile = "MAILROOM_DATABASE_PASSWORD_FILE"
	EnvListenAddr           = "MAILROOM_LISTEN_ADDR"
	EnvMasterKeyFile        = "MAILROOM_MASTER_KEY_FILE"
	EnvLogLevel             = "MAILROOM_LOG_LEVEL"
	EnvLogFormat            = "MAILROOM_LOG_FORMAT"

	// OIDC (all five required together for the OIDC auth path).
	EnvOIDCIssuer       = "MAILROOM_OIDC_ISSUER"
	EnvOIDCClientID     = "MAILROOM_OIDC_CLIENT_ID"
	EnvOIDCClientSecret = "MAILROOM_OIDC_CLIENT_SECRET" // honors _FILE
	EnvOIDCRedirectURL  = "MAILROOM_OIDC_REDIRECT_URL"
	EnvSessionKey       = "MAILROOM_SESSION_KEY" // honors _FILE

	// Mail-provider OAuth (#52, ADR-046): the operator's own OAuth client at
	// each provider. A provider is offered (Connect with Google / Microsoft)
	// only when BOTH its client id and secret are set; one without the other
	// is a startup error. All honor _FILE.
	EnvOAuthGoogleClientID        = "MAILROOM_OAUTH_GOOGLE_CLIENT_ID"
	EnvOAuthGoogleClientSecret    = "MAILROOM_OAUTH_GOOGLE_CLIENT_SECRET"
	EnvOAuthMicrosoftClientID     = "MAILROOM_OAUTH_MICROSOFT_CLIENT_ID"
	EnvOAuthMicrosoftClientSecret = "MAILROOM_OAUTH_MICROSOFT_CLIENT_SECRET"
	// EnvOAuthMicrosoftTenant narrows the Microsoft authority: "common"
	// (default: personal accounts and any organization), "consumers",
	// "organizations", a tenant id or a verified domain.
	EnvOAuthMicrosoftTenant = "MAILROOM_OAUTH_MICROSOFT_TENANT"
	// EnvPublicURL is the origin browsers reach this server at, from which
	// the OAuth callback URLs are built. Optional: it defaults to the origin
	// of MAILROOM_OIDC_REDIRECT_URL, and is required for OAuth under dev-auth.
	EnvPublicURL = "MAILROOM_PUBLIC_URL"

	// Operator: a subject get-or-created at boot. Under OIDC this is now just an
	// OPTIONAL declared-accounts owner (the accounts file is reconciled onto it) —
	// it is NOT an allowlist: any user who completes OIDC login is auto-provisioned
	// (mailroom#98). Under dev-auth the dev-auth subject IS the fixed identity.
	EnvOperatorSubject = "MAILROOM_OPERATOR_SUBJECT"
	EnvOperatorEmail   = "MAILROOM_OPERATOR_EMAIL"

	// Dev-auth escape hatch — used ONLY when OIDC is not configured.
	EnvDevAuthSubject = "MAILROOM_DEV_AUTH_SUBJECT"
	EnvDevAuthEmail   = "MAILROOM_DEV_AUTH_EMAIL"

	// EnvDevAuthAck must equal DevAuthAckPhrase before dev-auth is honored.
	//
	// ⚠ WHY AN ACKNOWLEDGEMENT AND NOT JUST THE SUBJECT (#351). Dev-auth used to
	// be selected by the ABSENCE of OIDC plus a non-empty subject — the weakest
	// possible gate on the one setting that turns authentication off completely.
	// An absence cannot express intent, and it silently did the opposite of what
	// the operator was told: deploy/.env.example shipped the subject UNCOMMENTED,
	// directly beneath a comment reading "Leave both commented to force the OIDC
	// path", so `cp .env.example .env && docker compose up -d` with no edits
	// produced a reachable mailbox with no authentication at all.
	//
	// A phrase the operator has to type is not security — anyone editing the env
	// can type it. It is a statement of intent, which is exactly what was missing:
	// after this, an unauthenticated instance is something someone chose.
	EnvDevAuthAck = "MAILROOM_DEV_AUTH_I_UNDERSTAND"

	// Sync + web.
	EnvSyncInterval = "MAILROOM_SYNC_INTERVAL"
	// EnvSyncMaxBackoff caps the per-account failure backoff (mailroom#103). Must
	// be positive; defaults to DefaultSyncMaxBackoff.
	EnvSyncMaxBackoff = "MAILROOM_SYNC_MAX_BACKOFF"
	// EnvIMAPIdle toggles the IMAP IDLE watcher (mailroom#103 Stage 2). Default
	// ON; set to a false value (0/false/no/off) to make sync poll-only. A server
	// without IDLE support falls back to polling regardless.
	EnvIMAPIdle = "MAILROOM_IMAP_IDLE"
	// EnvLocalDKIM toggles Mailroom's own DKIM check at ingest (#776, ADR-036).
	// Default ON; set it to a false value (0/false/no/off) to disable the check
	// entirely: no signature is verified, no DNS lookup is made, and no verdict
	// is stored. Mail that arrives without a trusted Authentication-Results
	// header then reads "unauthenticated", exactly as before. When on, mail that
	// carries no such header has its DKIM signatures verified in-process before
	// it is stored, with each DNS lookup bounded to 3 seconds, at most 4 lookups
	// and 8 seconds per message; a DNS failure stores nothing ("not checked").
	// Only a pass whose signing domain is aligned with the From domain is ever
	// shown, as "Verified by Mailroom".
	EnvLocalDKIM = "MAILROOM_LOCAL_DKIM"
	// EnvIMAPIdleTimeout sets the IDLE re-issue period (RFC 2177). Must be
	// positive; defaults to DefaultIMAPIdleTimeout.
	EnvIMAPIdleTimeout = "MAILROOM_IMAP_IDLE_TIMEOUT"
	// EnvImageProxy toggles the image proxy (ADR-026, #55 increment 3). Default
	// ON; set it to a negative value to disable. Disabling restores the previous
	// behaviour exactly: bodies park the sender's URL and the browser fetches it.
	EnvImageProxy = "MAILROOM_IMAGE_PROXY"
	// EnvImageProxyPrefetch toggles fetching allowed senders' images on arrival
	// (#922, ADR-026 amendment of 2026-10-27). Default ON when the proxy is on;
	// it has no effect with the proxy off.
	EnvImageProxyPrefetch = "MAILROOM_IMAGE_PROXY_PREFETCH"
	// EnvImageProxyPrefetchMaxAge is the prefetch's arrival window, a positive
	// Go duration; defaults to DefaultImageProxyPrefetchMaxAge (168h).
	EnvImageProxyPrefetchMaxAge = "MAILROOM_IMAGE_PROXY_PREFETCH_MAX_AGE"
	// EnvImageProxyPrefetchMaxBytes is the per-user ceiling, in bytes, on
	// images kept because a message shows them (#922 review H1). When it is
	// reached the prefetch stops fetching for that user; nothing kept is ever
	// evicted by it. Empty or 0 takes the built-in 16 GiB; a negative value
	// means no limit.
	EnvImageProxyPrefetchMaxBytes = "MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES"
	// EnvIMAPPool toggles the warm per-account sync connection pool (mailroom#103
	// Stage 3). Default ON; set to a false value (0/false/no/off) to dial a fresh
	// connection for every pass. Reuse is NOOP-probe-gated, so leaving it on is
	// safe even against a server that resets idle connections.
	EnvIMAPPool = "MAILROOM_IMAP_POOL"
	// EnvIMAPPoolIdleTimeout sets how long a warm pooled connection may sit unused
	// before the pool evicts it (mailroom#103 Stage 3). Must be positive; defaults
	// to DefaultIMAPPoolIdleTimeout.
	EnvIMAPPoolIdleTimeout = "MAILROOM_IMAP_POOL_IDLE_TIMEOUT"
	// EnvIMAPSearchRate caps how many UID SEARCHes a single account may issue
	// per minute (#638). "0" disables pacing; see Config.IMAPSearchRate.
	EnvIMAPSearchRate = "MAILROOM_IMAP_SEARCH_RATE"
	// EnvGmailDailyDownloadMB sets a Gmail account's daily download budget in
	// MB (#52, ADR-047 §6). Positive; see Config.GmailDailyDownloadMB.
	EnvGmailDailyDownloadMB = "MAILROOM_GMAIL_DAILY_DOWNLOAD_MB"
	// EnvGmailLabelSweepInterval sets how often the Gmail label sweep runs
	// (#52, ADR-047 §4). "0" disables it.
	EnvGmailLabelSweepInterval = "MAILROOM_GMAIL_LABEL_SWEEP_INTERVAL"
	// EnvFlagSweepInterval sets how often the CONDSTORE-less flag sweep
	// re-reads each mailbox's newest UIDs (ADR-046 §12). "0" disables the
	// sweep entirely — recent and full.
	EnvFlagSweepInterval = "MAILROOM_FLAG_SWEEP_INTERVAL"
	// EnvFlagSweepWindow sets how many of the newest UIDs that re-read covers.
	EnvFlagSweepWindow = "MAILROOM_FLAG_SWEEP_WINDOW"
	// EnvFlagSweepFullInterval sets how often the slower whole-mailbox flag
	// sweep starts. "0" disables only the full sweep.
	EnvFlagSweepFullInterval = "MAILROOM_FLAG_SWEEP_FULL_INTERVAL"
	// EnvReapInterval sets the expunge-reaper cadence (#86). "0" disables the
	// reaper entirely; see Config.ReapInterval for what that costs.
	EnvReapInterval = "MAILROOM_REAP_INTERVAL"
	// EnvTrashRetention sets the Trash dwell time before hard deletion (ADR-009,
	// 30-day default). Must be positive — a zero/negative window would purge on
	// trash, which is not a delete lifecycle, it is data loss.
	EnvTrashRetention = "MAILROOM_TRASH_RETENTION"
	// EnvPurgeInterval sets the purge-sweep cadence (ADR-009). Must be positive;
	// unlike the reaper there is no "disable" value — Trash that is never purged
	// is the bug this job exists to fix.
	EnvPurgeInterval = "MAILROOM_PURGE_INTERVAL"
	// EnvChangeLogRetention sets how far back change_log history is kept
	// before the weekly compaction job deletes it (mailroom#526, ADR-003).
	// Must be a Go duration of at least MinChangeLogRetention (7 days) —
	// see DefaultChangeLogRetention for why a shorter value is refused
	// rather than merely discouraged.
	EnvChangeLogRetention = "MAILROOM_CHANGE_LOG_RETENTION"
	// EnvJobRetention / EnvDeadJobRetention set how long finished and
	// dead-lettered job rows are kept before the daily job sweep deletes them.
	// Go durations of at least MinJobRetention (24h).
	EnvJobRetention     = "MAILROOM_JOB_RETENTION"
	EnvDeadJobRetention = "MAILROOM_DEAD_JOB_RETENTION"

	EnvBlobRoot = "MAILROOM_BLOB_ROOT"
	EnvWebRoot  = "MAILROOM_WEB_ROOT"
	// EnvWKDDomain is the mail domain whose keys this instance publishes over
	// WKD (#48, spec §9). UNSET DISABLES PUBLICATION, which is the right
	// default: the direct method carries no domain in the request path, so a
	// server that guessed would answer for domains it has no business
	// answering for.
	EnvWKDDomain = "MAILROOM_WKD_DOMAIN"

	// EnvPGPSocket is the crypto process's Unix socket (ADR-024). UNSET
	// DISABLES OUTGOING SIGNING, which is the right default and is not a
	// degraded mode: mail goes out unsigned exactly as it did before #47, and
	// the server is otherwise unaffected.
	//
	// ⚠ Once it IS set, a user with a bound signing key cannot send while the
	// crypto process is down. That is ADR-024's no-fallback rule reaching the
	// send path deliberately — signing in the app would be the boundary
	// deleting itself under load — and it fails at the moment of sending rather
	// than producing mail with a signature nobody can verify.
	EnvPGPSocket = "MAILROOM_PGP_SOCKET"

	// EnvClamdSocket is the malware scanner's Unix socket (ADR-027, #394). UNSET
	// DISABLES SCANNING, and that is the honest default: app.attachments rows
	// then keep their 'unscanned' state, which the §5d aggregate reports as its
	// own answer rather than folding into "clean".
	//
	// ⚠ Once it IS set, ingest is fail-closed. A daemon that does not answer
	// pauses the whole account's sync — mail is left on the provider, cursors
	// are withheld, and the expunge reaper is skipped — until it answers again.
	// That is deliberate (ADR-027 §4c): the alternative is a badge that says
	// "checked" on mail nothing checked. A per-file failure never does this; see
	// internal/ingest/scan.go for the taxonomy that separates the two.
	EnvClamdSocket = "MAILROOM_CLAMD_SOCKET"
	// EnvClamdMaxBytes overrides the largest attachment sent to the scanner. It
	// MUST match the daemon's own StreamMaxLength.
	//
	// ⚠ MEASURED, NOT DOCUMENTED: the official clamav/clamav image reports
	// StreamMaxLength = 104857600 (100 MB), not the 25 MB in upstream's
	// documentation. The default here is the measured one. Mismatch is safe in
	// both directions but wasteful in one — see internal/clamd.MaxStreamBytes.
	EnvClamdMaxBytes = "MAILROOM_CLAMD_MAX_BYTES"

	// EnvAuthservIDs names the Authentication-Results authserv-id(s) this
	// deployment trusts, comma-separated (#414, RFC 8601 §5). It gates ONE
	// thing: whether an affirmative DKIM/DMARC `pass` is allowed to suppress the
	// `selfSpoof` sender warning.
	//
	// ⚠ AN Authentication-Results HEADER IS ATTACKER-WRITABLE UNLESS ITS
	// authserv-id IS CHECKED. §5 requires an MTA to strip only headers bearing
	// its OWN id, so `Authentication-Results: x.example; dkim=pass` written by
	// the sender travels through an ordinary MX untouched. Without this setting,
	// a stranger spoofing one of your own domains switches off the warning about
	// it by adding one line.
	//
	// ⚠ THE ID BELONGS TO THE AUTHENTICATING HOP, WHICH IS USUALLY NOT MAILROOM.
	// Mailroom reads mail over IMAP from a provider that already did SPF/DKIM/
	// DMARC, so the id to trust is the PROVIDER'S — whatever appears at the start
	// of the header value on mail you already have. Read it off a real message
	// rather than guessing.
	//
	// ⚠ UNSET IS FAIL-SAFE, NOT FAIL-OPEN, and the direction was chosen on a
	// measurement rather than a preference. With no trusted id, no verdict is
	// believed for the security decision, so selfSpoof fires on own-domain mail
	// it might otherwise have suppressed — noise, never silence. On the corpus
	// this was measured against that costs nothing at all: of 13 own-domain
	// messages, 7 are locally authored (where the warning cannot fire) and the
	// other 6 already fire, so none was being suppressed by an auth pass.
	//
	// Setting it WRONG has the same shape: every header is discarded and the
	// warning fires more often. There is no configuration of this value that
	// makes a spoofing warning quietly disappear.
	//
	// DATE-BOUNDED ENTRIES (#766). An entry may be `id<YYYY-MM-DD`, for example
	// `mx.provider.example,old.provider.example<2026-07-01`: the id is believed
	// only for mail whose INTERNALDATE (received_at, set by the provider, never
	// the sender's Date header) is strictly before 00:00 UTC on that date. It
	// is for a provider that changed the id it stamps: RFC 8601 §5 has an MTA
	// strip only headers bearing its CURRENT id, so after the change a forged
	// header bearing the old id is no longer stripped, and trusting the old id
	// forever would re-open the hole for new mail. A malformed bound is a
	// startup error. An entry that reaches a trust check without being resolved
	// by ml.TrustedAt matches nothing, and mail imported from an archive
	// (whose received_at may be the sender's Date header) is never judged
	// against a bound.
	//
	// WHAT THE ID DOES FOR THE NOTICE. It also decides the reading-pane
	// sender-authentication notice (`authResults`): with no id configured there
	// is no notice; with ids configured, a trusted header shows its verdicts and
	// anything else reads "unauthenticated" for inbound mail (not locally
	// authored, not in Sent or Drafts). An UNTRUSTED header changes nothing
	// about that, and neither does the From address. A quiet-on-untrusted rule
	// was built and withdrawn (#766, ADR-035): the header is attacker-writable.
	// So the sentence above holds for the notice too: no configuration of this
	// value makes a warning quietly disappear.
	EnvAuthservIDs = "MAILROOM_AUTHSERV_IDS"

	// EnvTrustedProxies lists the reverse proxies (IPs or CIDRs, comma-separated)
	// whose X-Forwarded-For is believed when the signed-in devices list records
	// the address a session last connected from (#937, ADR-048 section 6). Empty
	// trusts none. See Config.TrustedProxies.
	EnvTrustedProxies = "MAILROOM_TRUSTED_PROXIES"

	// Search (spec §7). The index root is a disposable cache — never backed up,
	// always rebuildable — and MUST sit on an at-rest-encrypted volume.
	EnvSearchRoot     = "MAILROOM_SEARCH_ROOT"
	EnvIndexInterval  = "MAILROOM_INDEX_INTERVAL"
	EnvSearchDisabled = "MAILROOM_SEARCH_DISABLED"

	// EnvWritebackDisabled is the instance-level half of ingest-only mode
	// (ADR-028 §2, #521). See Config.WritebackDisabled for what it does.
	EnvWritebackDisabled = "MAILROOM_WRITEBACK_DISABLED"

	// EnvAllowPrivateAccountHosts widens the egress guard on every account
	// connection (validation, IMAP sync, SMTP submission) to permit
	// private/loopback hosts (#527, #625 §3). See Config.AllowPrivateAccountHosts.
	EnvAllowPrivateAccountHosts = "MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS"

	// Send. The undo-send hold window is how long a submission sits cancelable
	// before the durable job fires (spec §5: Undo Send and Send Later are the
	// same mechanism).
	EnvUndoSendWindow = "MAILROOM_UNDO_SEND_WINDOW"

	// Declarative account configuration (a JSON file — see accounts.go).
	EnvAccountsFile = "MAILROOM_ACCOUNTS_FILE"

	// Quotas & rate limits (spec §10). Per-user quota LIMITS live on app.users
	// (quota_storage_bytes/…) — they are per-user columns, not global config. What
	// is global, and therefore lives here, is the per-user API rate limit.
	// "0" for either value disables rate limiting (an explicit operator opt-out).
	EnvRateLimitPerSecond = "MAILROOM_RATE_LIMIT_RPS"
	EnvRateLimitBurst     = "MAILROOM_RATE_LIMIT_BURST"

	// The ANONYMOUS key surface has its own budget (#264), separate from the
	// per-user one above because it is a different problem: those requests
	// carry no identity to charge, so the limit is global and bounds database
	// WORK rather than apportioning it fairly. See internal/keypub/ratelimit.go
	// for why per-IP would be the wrong answer here.
	//
	// ⚠ Unset means the DEFAULT (on), not zero. A limiter that switches itself
	// off when nobody configures it is one that ships inert.
	EnvWKDRateLimitPerSecond = "MAILROOM_WKD_RATE_LIMIT_RPS"
	EnvWKDRateLimitBurst     = "MAILROOM_WKD_RATE_LIMIT_BURST"
)

// fileSuffix is appended to a sensitive variable's name to form its
// Docker-secrets indirection: FOO_FILE points at a file holding FOO's
// value, taking precedence over FOO itself.
const fileSuffix = "_FILE"

// Config holds Mailroom's runtime configuration (spec §16). It carries only
// plain settings and file paths — internal/config never reads or
// interprets secret contents beyond the database DSN (which the store
// package requires as a connection string, and which since #537 may itself
// be assembled from a separate password file — see EnvDatabasePasswordFile).
type Config struct {
	// DatabaseURL is the Postgres DSN used to connect as mailroom_app
	// (CLAUDE.md database-access rules). Required. Sourced from
	// MAILROOM_DATABASE_URL, or from the file named by
	// MAILROOM_DATABASE_URL_FILE if set. If EnvDatabasePasswordFile is also
	// set, its contents are injected into the URL as the password (#537,
	// internal/pgdsn) — the URL itself must then carry no password of its
	// own.
	DatabaseURL string

	// ListenAddr is the HTTP listen address for health/readiness/metrics
	// (and, later, the JMAP API). Defaults to ":8080".
	ListenAddr string

	// MasterKeyFile is the path to the envelope-encryption master key
	// (spec §11). Only the path is carried here; internal/crypto owns
	// reading and interpreting the file's contents. Defaults to
	// "/run/secrets/mailroom_master_key".
	MasterKeyFile string

	// LogLevel is one of "debug", "info", "warn", "error". Defaults to
	// "info".
	LogLevel string

	// LogFormat is "json" or "text". Defaults to "json".
	LogFormat string

	// --- Authentication ---------------------------------------------------
	// The server refuses to start unless exactly one auth path is usable
	// (see Load's either/or validation): EITHER OIDC fully configured, OR
	// DevAuthSubject set. It never comes up with no way to authenticate.

	// OIDCIssuer/OIDCClientID/OIDCClientSecret/OIDCRedirectURL are the OIDC
	// Authorization-Code confidential-client settings (internal/auth). All
	// four, plus SessionKey, must be set together to select the OIDC path.
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string

	// OAuth* are the mail-provider OAuth clients (#52, ADR-046). Empty pairs
	// mean the provider is not offered. OAuthMicrosoftTenant defaults to
	// "common". PublicURL is MAILROOM_PUBLIC_URL as given (may be empty);
	// OAuthPublicURL resolves the default.
	OAuthGoogleClientID        string
	OAuthGoogleClientSecret    string
	OAuthMicrosoftClientID     string
	OAuthMicrosoftClientSecret string
	OAuthMicrosoftTenant       string
	PublicURL                  string

	// SessionKey is the HMAC key that signs session/login-state cookies
	// (auth.Config.SigningKey). Required with OIDC, and at least
	// MinSessionKeyLen bytes. Sourced from MAILROOM_SESSION_KEY or its _FILE.
	SessionKey []byte

	// OperatorSubject/OperatorEmail identify an OPTIONAL boot operator that is
	// get-or-created at boot (store.ProvisionUser) and owns the declared accounts
	// file. Under OIDC it is NO LONGER an allowlist: every user who completes the
	// login flow is auto-provisioned from their token and driven by the per-user
	// fan-out (mailroom#98). It survives only as the declared-accounts owner and,
	// under dev-auth, as the fixed identity DevAuthSubject/DevAuthEmail inject.
	OperatorSubject string
	OperatorEmail   string

	// DevAuthSubject/DevAuthEmail enable the INSECURE dev-auth escape hatch
	// (auth.DevAuth): a middleware that injects this operator's id into every
	// request with NO authentication. Honored ONLY when OIDC is not
	// configured; it exists so a test instance is exercisable without a full
	// OIDC provider. Never set it on an internet-exposed instance.
	DevAuthSubject string
	// DevAuthAck must equal DevAuthAckPhrase or dev-auth is refused at load.
	DevAuthAck   string
	DevAuthEmail string

	// --- Sync + web -------------------------------------------------------

	// SyncInterval is the per-account BASE re-sync period for the background sync
	// loop (syncrunner, mailroom#103). Each account is scheduled independently
	// around this value — raised to any per-provider minimum-interval floor, and
	// pushed out by exponential backoff while the account is failing — rather than
	// every account syncing in a single global lockstep. Defaults to
	// DefaultSyncInterval.
	SyncInterval time.Duration

	// SyncMaxBackoff caps the per-account exponential backoff applied to a
	// repeatedly-failing account (mailroom#103). Defaults to DefaultSyncMaxBackoff;
	// must be positive.
	SyncMaxBackoff time.Duration

	// IMAPIdle enables the IMAP IDLE watcher (mailroom#103 Stage 2). Default
	// true; MAILROOM_IMAP_IDLE=0 makes sync poll-only. IDLE is additive — it
	// triggers the SAME sync pass sooner, and a server lacking IDLE support
	// auto-falls-back to polling — so it is safe on by default.
	IMAPIdle bool

	// LocalDKIM enables Mailroom's own DKIM check at ingest (#776, ADR-036).
	// Default true; MAILROOM_LOCAL_DKIM=off disables it entirely.
	LocalDKIM bool

	// IMAPIdleTimeout is the IDLE re-issue period (RFC 2177). Defaults to
	// DefaultIMAPIdleTimeout; must be positive.
	IMAPIdleTimeout time.Duration

	// IMAPPool enables the warm per-account sync connection pool (mailroom#103
	// Stage 3). Default true; MAILROOM_IMAP_POOL=0 dials a fresh connection per
	// pass. Reuse is NOOP-probe-gated, so it is safe on by default even against a
	// server that resets idle connections.
	IMAPPool bool
	// ImageProxy serves remote images through the server instead of letting the
	// reader's browser fetch them (ADR-026). Default on.
	ImageProxy bool
	// ImageProxyPrefetch fetches allowed senders' images on arrival (#922).
	// Default on; meaningful only with ImageProxy.
	ImageProxyPrefetch bool
	// ImageProxyPrefetchMaxAge is the prefetch's arrival window. Defaults to
	// DefaultImageProxyPrefetchMaxAge; must be positive.
	ImageProxyPrefetchMaxAge time.Duration
	// ImageProxyPrefetchMaxBytes is the referenced-bytes ceiling per user. 0
	// takes the API's default (16 GiB); negative means no limit.
	ImageProxyPrefetchMaxBytes int64

	// IMAPPoolIdleTimeout evicts a warm pooled connection unused longer than it
	// (mailroom#103 Stage 3). Defaults to DefaultIMAPPoolIdleTimeout; must be
	// positive.
	IMAPPoolIdleTimeout time.Duration

	// IMAPSearchRate caps how many UID SEARCHes one account issues per minute,
	// across every session opened for it — the observation pass and the
	// writeback drain share the budget because the server does (#638).
	//
	// Defaults to DefaultIMAPSearchRate (30/min). ZERO IS LEGAL and disables
	// pacing completely, which is the pre-#638 behaviour: correct against a
	// server with no SEARCH ceiling, and the reason it is an opt-out rather
	// than a hard floor. Negative is rejected.
	IMAPSearchRate int

	// GmailDailyDownloadMB is the most message-body MB (10^6 bytes) a Gmail
	// account may download in any 24 hours (#52, ADR-047 §6). Gmail suspends an
	// account's IMAP everywhere — the person's phone included — past its own
	// daily limit, so there is no "off": it must be positive. Defaults to
	// DefaultGmailDailyDownloadMB.
	GmailDailyDownloadMB int
	// GmailLabelSweepInterval is how often the Gmail label sweep re-reads
	// imported messages' labels (ADR-047 §4). Zero disables it — right only
	// once CONDSTORE is known to report Gmail label changes.
	GmailLabelSweepInterval time.Duration

	// FlagSweepInterval, FlagSweepWindow and FlagSweepFullInterval configure
	// the CONDSTORE-less flag sweep (mailsync.WithFlagSweep, ADR-046 §12). It
	// runs only for a server that advertises neither CONDSTORE nor QRESYNC.
	// FlagSweepInterval zero disables it; FlagSweepFullInterval zero disables
	// only the whole-mailbox pass. FlagSweepWindow must be positive.
	FlagSweepInterval     time.Duration
	FlagSweepWindow       int
	FlagSweepFullInterval time.Duration

	// ReapInterval is how often each mailbox is reconciled against the server's
	// live UID set — the expunge reaper (mailsync.WithReapInterval, #86). It is
	// what makes a message deleted or moved away in another IMAP client (phone,
	// webmail, Thunderbird) actually leave its Mailroom folder; without it,
	// provenance only ever grows and the message stays forever.
	//
	// Defaults to DefaultReapInterval (hourly). ZERO IS LEGAL and disables the
	// reaper completely — an explicit opt-out, at the price of folder membership
	// that can only ever gain messages, never lose them. Negative is rejected.
	ReapInterval time.Duration

	// TrashRetention is how long a trashed message dwells in Trash before the
	// purge job hard-deletes it (ADR-009). Defaults to DefaultTrashRetention
	// (30 days). Must be positive: a non-positive window would make "trash" mean
	// "immediately destroy", which is the opposite of the retractable soft-delete
	// the deletion lifecycle promises.
	TrashRetention time.Duration

	// PurgeInterval is how often the purge job sweeps each user's Trash for
	// messages past purge_after and hard-deletes them (ADR-009). Defaults to
	// DefaultPurgeInterval (hourly). Must be positive — there is no "off": Trash
	// that is never purged grows without bound and never reclaims its blobs.
	PurgeInterval time.Duration

	// ChangeLogRetention is how far back change_log history is kept before the
	// weekly compaction job deletes it (mailroom#526, ADR-003, data-model
	// §3.3). Defaults to DefaultChangeLogRetention (90 days); must be at least
	// MinChangeLogRetention (7 days) — there is no "off", by design: an
	// ever-growing change_log is exactly what this job exists to prevent.
	ChangeLogRetention time.Duration

	// JobRetention and DeadJobRetention are how long finished and dead-lettered
	// job rows are kept before the daily job sweep deletes them. Default
	// DefaultJobRetention (30 days) and DefaultDeadJobRetention (90 days); each
	// at least MinJobRetention.
	JobRetention     time.Duration
	DeadJobRetention time.Duration

	// UndoSendWindow is how long a submission is held (and cancelable) before
	// the send job fires. Defaults to DefaultUndoSendWindow; zero is legal and
	// means "send immediately, no undo".
	UndoSendWindow time.Duration

	// BlobRoot is the filesystem root of the encrypted mail-blob store
	// (blob.NewFSBlobStore). Defaults to DefaultBlobRoot; must be writable.
	BlobRoot string

	// WKDDomain is the mail domain published over WKD. Empty means WKD is off:
	// the routes still exist and answer 404, so the gate tests do not become
	// conditional on configuration.
	//
	// ⚠ Setting this is only half the job, and the other half is DNS the
	// operator owns. Clients MUST try the `openpgpkey.<domain>` advanced method
	// FIRST, so a subdomain that exists but does not serve keys correctly
	// breaks every lookup — including ones the direct method would have
	// answered. Absent or fully correct; there is no working middle.
	WKDDomain string

	// PGPSocket is the crypto process's Unix socket. Empty means outgoing mail
	// is not signed — the ordinary case, and not an error.
	PGPSocket string

	// ClamdSocket is the malware scanner's Unix socket. Empty means attachments
	// are not scanned; see EnvClamdSocket for what setting it commits to.
	ClamdSocket string
	// ClamdMaxBytes is the largest attachment sent to the scanner, defaulting to
	// clamd.MaxStreamBytes. Zero means the default.
	ClamdMaxBytes int64

	// WebRoot is the directory of the built web client, served at "/" when it
	// exists (best-effort; absence is fine). Defaults to DefaultWebRoot.
	WebRoot string

	// --- Search (spec §7) --------------------------------------------------

	// SearchRoot is the filesystem root of the per-user search indexes
	// (internal/search). Defaults to DefaultSearchRoot; must be writable, and
	// the server creates it and every per-user directory under it 0700.
	//
	// Operational contract, stated because getting it wrong is expensive in
	// both directions: this directory is a CACHE. Do not back it up (it is
	// rebuildable from Postgres at any time, and a restored-but-stale index is
	// worse than no index). DO put it on an at-rest-encrypted volume: it holds
	// the plaintext of every message, including mail that arrived encrypted,
	// and Tantivy has no encryption of its own (spec §7/§11, ADR-006).
	SearchRoot string

	// IndexInterval is how often the indexer drains the change log when idle.
	// Defaults to DefaultIndexInterval.
	IndexInterval time.Duration

	// SearchDisabled turns the indexer and the search API off entirely
	// (MAILROOM_SEARCH_DISABLED=1). The server still serves mail; Email/query
	// then rejects text filters with unsupportedFilter rather than pretending.
	// It exists for the operator who wants no plaintext index on disk at all.
	SearchDisabled bool

	// WritebackDisabled is the INSTANCE-level half of ingest-only mode
	// (ADR-028 §2, MAILROOM_WRITEBACK_DISABLED=1). It ANDs with the per-account
	// `writeback_enabled` column (store.Account.WritebackEnabled): either one
	// being off is enough to suppress writeback for that account, so a
	// misclick that flips the per-account flag back on in the settings UI
	// cannot, by itself, resurrect a real IMAP write. It exists so the safety
	// catch for a cutover window lives in the deployment's environment — versioned
	// alongside the compose file, not in user data — rather than depending on
	// nobody clicking a checkbox. While it is set, Account/set refuses to turn
	// the per-account flag back on.
	WritebackDisabled bool

	// AllowPrivateAccountHosts permits an account's IMAP and SMTP hosts to
	// resolve to a private, loopback or link-local address (#527, #625 §3,
	// MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1). Default false.
	//
	// ⚠ IT WIDENS THE ACCOUNT CONNECTIONS ONLY — but all of them: the
	// validation check and dial (onboarding), every IMAP dial of a stored
	// account (sync, pool, IDLE, writeback — via syncrunner.CredentialResolver
	// onto sync.Config.AllowPrivateHosts), and the SMTP submission dial
	// (provider.SMTPSender). Each of those dials resolves the host once, checks
	// every address, and connects to the checked literal, so a host that
	// passed validation cannot rebind to a private address later. The image
	// proxy and web push keep the strict policy regardless, because the thing
	// being guarded there is a URL that arrived in somebody's mail, not an
	// endpoint the operator typed.
	//
	// Off by default because the default should be the safe one: without the
	// guard, an authenticated caller can use Account/set as a port-scan oracle
	// against everything the container can reach. On for the deployment whose
	// mail server genuinely lives on the LAN — a real configuration this
	// project should not make impossible, and the refusal message names this
	// variable so the way out is discoverable from the error.
	AllowPrivateAccountHosts bool

	// TrustedProxies are the reverse proxies whose X-Forwarded-For is believed
	// when recording the address a session last connected from
	// (MAILROOM_TRUSTED_PROXIES: comma-separated IPs or CIDRs). Empty, the
	// default, trusts none: every session then shows the connecting address,
	// which behind a proxy is the proxy's. DISPLAY DATA ONLY (the signed-in
	// devices list); nothing security-relevant reads it, and the API key rate
	// limiter keeps using the connection's own address.
	TrustedProxies []netip.Prefix

	// AccountsFile is the path to the JSON file declaring the operator's
	// IMAP/SMTP accounts (LoadAccountsFile / AccountsDoc). Optional: unset, or
	// a path that does not exist, simply means "no declared accounts" — the
	// server still boots and serves mail it has already synced, and accounts
	// can be added out of band with mailroom-admin.
	//
	// Only the PATH lives in Config. The file's contents carry passwords, and
	// this package's contract is that a Config holds plain settings and file
	// paths — never secret material — so the accounts (and their passwords)
	// are read on demand by LoadAccountsFile into a value the caller owns and
	// can scrub, not into a long-lived Config field.
	AccountsFile string

	// --- Quotas & rate limits (spec §10) -----------------------------------

	// RateLimitPerSecond / RateLimitBurst are the per-user API rate limit — the
	// sustained request rate and the burst allowance. A non-positive value in
	// either disables rate limiting (the API then constructs no limiter). Default
	// DefaultRateLimit*. Per-user quota LIMITS (storage/sync/LLM) are NOT here:
	// they are per-user columns on app.users, read by the store per request.
	RateLimitPerSecond float64
	RateLimitBurst     int

	// WKDRateLimitPerSecond / WKDRateLimitBurst bound the ANONYMOUS key surface
	// (#264): WKD lookups and the key page, which carry no identity to charge.
	// The limit is therefore global — it bounds database work rather than
	// apportioning it between callers. A non-positive value in either disables
	// it, which is an explicit opt-out; the default is ON.
	WKDRateLimitPerSecond float64
	WKDRateLimitBurst     int
}

// OIDCConfigured reports whether the full OIDC auth path is available: all
// four OIDC settings plus a long-enough session key.
func (c *Config) OIDCConfigured() bool {
	return c.OIDCIssuer != "" && c.OIDCClientID != "" && c.OIDCClientSecret != "" &&
		c.OIDCRedirectURL != "" && len(c.SessionKey) >= MinSessionKeyLen
}

// DevAuthConfigured reports whether the dev-auth escape hatch is selected: it
// is honored only when OIDC is NOT configured and a dev-auth subject is set.
func (c *Config) DevAuthConfigured() bool {
	return !c.OIDCConfigured() && c.DevAuthSubject != ""
}

// Load builds a Config from the environment, applying the Docker-secrets
// _FILE indirection to sensitive values and validating required fields.
// It returns a single error listing everything that's missing or invalid.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:    envOrDefault(EnvListenAddr, DefaultListenAddr),
		MasterKeyFile: envOrDefault(EnvMasterKeyFile, DefaultMasterKeyFile),
		LogLevel:      strings.ToLower(envOrDefault(EnvLogLevel, DefaultLogLevel)),
		LogFormat:     strings.ToLower(envOrDefault(EnvLogFormat, DefaultLogFormat)),

		OIDCIssuer:               strings.TrimSpace(os.Getenv(EnvOIDCIssuer)),
		OIDCClientID:             strings.TrimSpace(os.Getenv(EnvOIDCClientID)),
		OIDCRedirectURL:          strings.TrimSpace(os.Getenv(EnvOIDCRedirectURL)),
		OperatorSubject:          strings.TrimSpace(os.Getenv(EnvOperatorSubject)),
		OperatorEmail:            strings.TrimSpace(os.Getenv(EnvOperatorEmail)),
		DevAuthSubject:           strings.TrimSpace(os.Getenv(EnvDevAuthSubject)),
		DevAuthAck:               strings.TrimSpace(os.Getenv(EnvDevAuthAck)),
		DevAuthEmail:             strings.TrimSpace(os.Getenv(EnvDevAuthEmail)),
		BlobRoot:                 envOrDefault(EnvBlobRoot, DefaultBlobRoot),
		WebRoot:                  envOrDefault(EnvWebRoot, DefaultWebRoot),
		WKDDomain:                strings.ToLower(strings.TrimSpace(os.Getenv(EnvWKDDomain))),
		PGPSocket:                strings.TrimSpace(os.Getenv(EnvPGPSocket)),
		ClamdSocket:              strings.TrimSpace(os.Getenv(EnvClamdSocket)),
		ClamdMaxBytes:            envInt64(EnvClamdMaxBytes),
		SearchRoot:               envOrDefault(EnvSearchRoot, DefaultSearchRoot),
		SearchDisabled:           envBool(EnvSearchDisabled),
		WritebackDisabled:        envBool(EnvWritebackDisabled),
		AllowPrivateAccountHosts: envBool(EnvAllowPrivateAccountHosts),
		AccountsFile:             strings.TrimSpace(os.Getenv(EnvAccountsFile)),
		SyncInterval:             DefaultSyncInterval,
		SyncMaxBackoff:           DefaultSyncMaxBackoff,
		IMAPIdle:                 envBoolDefault(EnvIMAPIdle, DefaultIMAPIdle),
		LocalDKIM:                envBoolDefault(EnvLocalDKIM, DefaultLocalDKIM),
		IMAPIdleTimeout:          DefaultIMAPIdleTimeout,
		IMAPPool:                 envBoolDefault(EnvIMAPPool, DefaultIMAPPool),
		ImageProxy:               envBoolDefault(EnvImageProxy, DefaultImageProxy),
		ImageProxyPrefetch:       envBoolDefault(EnvImageProxyPrefetch, DefaultImageProxyPrefetch),
		ImageProxyPrefetchMaxAge: DefaultImageProxyPrefetchMaxAge,
		IMAPPoolIdleTimeout:      DefaultIMAPPoolIdleTimeout,
		IMAPSearchRate:           DefaultIMAPSearchRate,
		GmailDailyDownloadMB:     DefaultGmailDailyDownloadMB,
		GmailLabelSweepInterval:  DefaultGmailLabelSweepInterval,
		FlagSweepInterval:        DefaultFlagSweepInterval,
		FlagSweepWindow:          DefaultFlagSweepWindow,
		FlagSweepFullInterval:    DefaultFlagSweepFullInterval,
		ReapInterval:             DefaultReapInterval,
		TrashRetention:           DefaultTrashRetention,
		PurgeInterval:            DefaultPurgeInterval,
		ChangeLogRetention:       DefaultChangeLogRetention,
		JobRetention:             DefaultJobRetention,
		DeadJobRetention:         DefaultDeadJobRetention,
		IndexInterval:            DefaultIndexInterval,
		UndoSendWindow:           DefaultUndoSendWindow,

		RateLimitPerSecond: DefaultRateLimitPerSecond,
		RateLimitBurst:     DefaultRateLimitBurst,

		WKDRateLimitPerSecond: DefaultWKDRateLimitPerSecond,
		WKDRateLimitBurst:     DefaultWKDRateLimitBurst,
	}

	var problems []string

	// A boolean toggle whose value we cannot read is a configuration error, not
	// a `false` (#556).
	//
	// ⚠ This is a safety property because of ONE of these variables:
	// MAILROOM_WRITEBACK_DISABLED. ADR-028 §2 makes it the second of the two
	// locks that keep an ingest-only instance from writing to somebody's real
	// mailbox, and an operator sets it precisely because they do not trust the
	// first. MEASURED before the fix: `enabled`, `disabled`, `ture` and `y` all
	// booted with the lock OFF and said nothing — the WARN in cmd/mailroom
	// fires only when the flag parses TRUE, and the web client's banner reads
	// the EFFECTIVE flag, so the interface correctly reported "live", which
	// looks like the UI working rather than like the lock having evaporated.
	//
	// For an ordinary feature toggle, defaulting off is fine. For a switch
	// whose whole purpose is "do not write to this person's real mailbox", the
	// safe reading of an unparseable value is "I do not know what you meant"
	// and the safe action is to stop.
	for _, name := range boolToggles {
		if _, _, ok := parseBool(name); !ok {
			problems = append(problems, fmt.Sprintf(
				"%s must be one of 1|true|yes|on or 0|false|no|off (case-insensitive, surrounding space ignored), got %q",
				name, os.Getenv(name)))
		}
	}

	// A malformed authserv-id bound is a startup error, never a silent drop
	// (#766): dropping it would quietly narrow whose verdicts are believed.
	if err := ValidateAuthservIDs(); err != nil {
		problems = append(problems, err.Error())
	}

	// A malformed trusted-proxy entry is a startup error, never a silent drop:
	// dropping one would quietly report the proxy's address for every device.
	trusted, err := ParseTrustedProxies(os.Getenv(EnvTrustedProxies))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.TrustedProxies = trusted

	dbURL, err := resolveSecret(EnvDatabaseURL)
	if err != nil {
		problems = append(problems, err.Error())
	}
	// Inject the password from EnvDatabasePasswordFile, if set (#537). Only
	// attempted when a URL was actually resolved above — with no URL at all,
	// "MAILROOM_DATABASE_URL (or _FILE) is required" below is the one error
	// that matters, not a confusing second complaint about a URL that does
	// not exist yet.
	if dbURL != "" {
		if withPW, perr := pgdsn.WithPasswordFile(dbURL, strings.TrimSpace(os.Getenv(EnvDatabasePasswordFile))); perr != nil {
			problems = append(problems, perr.Error())
		} else {
			dbURL = withPW
		}
	}
	cfg.DatabaseURL = dbURL

	// Secrets that honor the _FILE indirection.
	secret, err := resolveSecret(EnvOIDCClientSecret)
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.OIDCClientSecret = secret

	sessKey, err := resolveSecret(EnvSessionKey)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if sessKey != "" {
		cfg.SessionKey = []byte(sessKey)
	}

	// Mail-provider OAuth clients (#52). Every value honors _FILE; a half
	// configured pair is refused rather than silently leaving the provider
	// off, the same rule as a partial OIDC block.
	for _, f := range []struct {
		env string
		dst *string
	}{
		{EnvOAuthGoogleClientID, &cfg.OAuthGoogleClientID},
		{EnvOAuthGoogleClientSecret, &cfg.OAuthGoogleClientSecret},
		{EnvOAuthMicrosoftClientID, &cfg.OAuthMicrosoftClientID},
		{EnvOAuthMicrosoftClientSecret, &cfg.OAuthMicrosoftClientSecret},
	} {
		v, err := resolveSecret(f.env)
		if err != nil {
			problems = append(problems, err.Error())
		}
		*f.dst = v
	}
	if (cfg.OAuthGoogleClientID == "") != (cfg.OAuthGoogleClientSecret == "") {
		problems = append(problems, fmt.Sprintf("Google OAuth is partially configured: set both %s and %s (or neither)",
			EnvOAuthGoogleClientID, EnvOAuthGoogleClientSecret))
	}
	if (cfg.OAuthMicrosoftClientID == "") != (cfg.OAuthMicrosoftClientSecret == "") {
		problems = append(problems, fmt.Sprintf("Microsoft OAuth is partially configured: set both %s and %s (or neither)",
			EnvOAuthMicrosoftClientID, EnvOAuthMicrosoftClientSecret))
	}
	cfg.OAuthMicrosoftTenant = envOrDefault(EnvOAuthMicrosoftTenant, "common")
	cfg.PublicURL = strings.TrimSpace(os.Getenv(EnvPublicURL))

	if raw := strings.TrimSpace(os.Getenv(EnvSyncInterval)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 5m), got %q", EnvSyncInterval, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive, got %q", EnvSyncInterval, raw))
		default:
			cfg.SyncInterval = d
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvSyncMaxBackoff)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 30m), got %q", EnvSyncMaxBackoff, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive, got %q", EnvSyncMaxBackoff, raw))
		default:
			cfg.SyncMaxBackoff = d
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvIMAPIdleTimeout)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 29m), got %q", EnvIMAPIdleTimeout, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive, got %q", EnvIMAPIdleTimeout, raw))
		default:
			cfg.IMAPIdleTimeout = d
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvIMAPPoolIdleTimeout)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 5m), got %q", EnvIMAPPoolIdleTimeout, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive, got %q", EnvIMAPPoolIdleTimeout, raw))
		default:
			cfg.IMAPPoolIdleTimeout = d
		}
	}

	// Zero is accepted here too, and for the same shape of reason as the reaper:
	// it is the operator's explicit "this server has no SEARCH ceiling, do not
	// pace me". Negative is refused — it would read as "disabled" through a typo
	// on the one knob whose accidental OFF state produces the #638 refusals.
	if raw := strings.TrimSpace(os.Getenv(EnvGmailDailyDownloadMB)); raw != "" {
		v, perr := strconv.Atoi(raw)
		if perr != nil || v <= 0 {
			problems = append(problems, fmt.Sprintf("%s must be a positive integer (MB per 24 hours), got %q", EnvGmailDailyDownloadMB, raw))
		} else {
			cfg.GmailDailyDownloadMB = v
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvImageProxyPrefetchMaxBytes)); raw != "" {
		v, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("%s must be a whole number of bytes (0 or empty = the 16 GiB default, negative = no limit), got %q", EnvImageProxyPrefetchMaxBytes, raw))
		} else {
			cfg.ImageProxyPrefetchMaxBytes = v
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvImageProxyPrefetchMaxAge)); raw != "" {
		d, perr := time.ParseDuration(raw)
		if perr != nil || d <= 0 {
			problems = append(problems, fmt.Sprintf("%s must be a positive duration (e.g. 168h; turn the prefetch off with %s instead), got %q", EnvImageProxyPrefetchMaxAge, EnvImageProxyPrefetch, raw))
		} else {
			cfg.ImageProxyPrefetchMaxAge = d
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvGmailLabelSweepInterval)); raw != "" {
		d, perr := time.ParseDuration(raw)
		if perr != nil || d < 0 {
			problems = append(problems, fmt.Sprintf("%s must be a non-negative duration (e.g. 6h; 0 disables the sweep), got %q", EnvGmailLabelSweepInterval, raw))
		} else {
			cfg.GmailLabelSweepInterval = d
		}
	}
	for _, knob := range []struct {
		env string
		dst *time.Duration
	}{{EnvFlagSweepInterval, &cfg.FlagSweepInterval}, {EnvFlagSweepFullInterval, &cfg.FlagSweepFullInterval}} {
		if raw := strings.TrimSpace(os.Getenv(knob.env)); raw != "" {
			d, perr := time.ParseDuration(raw)
			if perr != nil || d < 0 {
				problems = append(problems, fmt.Sprintf("%s must be a non-negative duration (0 disables it), got %q", knob.env, raw))
			} else {
				*knob.dst = d
			}
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvFlagSweepWindow)); raw != "" {
		v, perr := strconv.Atoi(raw)
		if perr != nil || v <= 0 {
			problems = append(problems, fmt.Sprintf("%s must be a positive integer (UIDs), got %q", EnvFlagSweepWindow, raw))
		} else {
			cfg.FlagSweepWindow = v
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvIMAPSearchRate)); raw != "" {
		v, perr := strconv.Atoi(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be an integer (UID SEARCHes per minute; 0 disables pacing), got %q", EnvIMAPSearchRate, raw))
		case v < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative (0 disables search pacing), got %q", EnvIMAPSearchRate, raw))
		default:
			cfg.IMAPSearchRate = v
		}
	}

	// Zero is accepted here (unlike SyncInterval): it is the operator's explicit
	// "never reap" switch. Negative is not — it would silently disable the reaper
	// through a typo, and this is the one knob whose accidental OFF state leaves
	// deleted mail visible forever.
	if raw := strings.TrimSpace(os.Getenv(EnvReapInterval)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 1h), got %q", EnvReapInterval, raw))
		case d < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative (0 disables the expunge reaper), got %q", EnvReapInterval, raw))
		default:
			cfg.ReapInterval = d
		}
	}

	// Trash retention and the purge cadence both demand a POSITIVE duration.
	// Unlike the reaper, neither has a meaningful "off": a non-positive retention
	// would purge on trash (data loss disguised as a delete), and a non-positive
	// sweep interval would either never run or spin. A typo turns the feature
	// into a hazard, so it is a hard config error rather than a silent default.
	if raw := strings.TrimSpace(os.Getenv(EnvTrashRetention)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 720h), got %q", EnvTrashRetention, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive (it is the Trash dwell time before hard deletion), got %q", EnvTrashRetention, raw))
		default:
			cfg.TrashRetention = d
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvPurgeInterval)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 1h), got %q", EnvPurgeInterval, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive (Trash must be swept), got %q", EnvPurgeInterval, raw))
		default:
			cfg.PurgeInterval = d
		}
	}

	// Change-log retention demands a duration of at least MinChangeLogRetention
	// (7 days) — see DefaultChangeLogRetention for why a shorter value is a
	// hard config error rather than merely a smaller number: it would force
	// every client, not just one that has genuinely been offline a while,
	// through cannotCalculateChanges on a regular basis.
	if raw := strings.TrimSpace(os.Getenv(EnvChangeLogRetention)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 2160h), got %q", EnvChangeLogRetention, raw))
		case d < MinChangeLogRetention:
			problems = append(problems, fmt.Sprintf("%s must be at least %s (a shorter window would force every client to resync on a regular basis), got %q",
				EnvChangeLogRetention, MinChangeLogRetention, raw))
		default:
			cfg.ChangeLogRetention = d
		}
	}

	for _, w := range []struct {
		env string
		dst *time.Duration
	}{{EnvJobRetention, &cfg.JobRetention}, {EnvDeadJobRetention, &cfg.DeadJobRetention}} {
		raw := strings.TrimSpace(os.Getenv(w.env))
		if raw == "" {
			continue
		}
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 720h), got %q", w.env, raw))
		case d < MinJobRetention:
			problems = append(problems, fmt.Sprintf("%s must be at least %s, got %q", w.env, MinJobRetention, raw))
		default:
			*w.dst = d
		}
	}

	// Rate limits. A negative value is a config error (a typo must not silently
	// invert the limit); zero is the explicit "disable rate limiting" opt-out and
	// is accepted. Enforcement of the resulting limit lives in internal/api.
	if raw := strings.TrimSpace(os.Getenv(EnvRateLimitPerSecond)); raw != "" {
		v, perr := strconv.ParseFloat(raw, 64)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a number (requests/second; 0 disables), got %q", EnvRateLimitPerSecond, raw))
		case v < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative (0 disables rate limiting), got %q", EnvRateLimitPerSecond, raw))
		default:
			cfg.RateLimitPerSecond = v
		}
	}

	// The anonymous key surface's own budget, same rules as above. Unset leaves
	// the default in place, which is ON — see EnvWKDRateLimitPerSecond.
	if raw := strings.TrimSpace(os.Getenv(EnvWKDRateLimitPerSecond)); raw != "" {
		v, perr := strconv.ParseFloat(raw, 64)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a number (requests/second; 0 disables), got %q", EnvWKDRateLimitPerSecond, raw))
		case v < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative (0 disables rate limiting), got %q", EnvWKDRateLimitPerSecond, raw))
		default:
			cfg.WKDRateLimitPerSecond = v
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvWKDRateLimitBurst)); raw != "" {
		v, perr := strconv.Atoi(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be an integer (burst allowance; 0 disables), got %q", EnvWKDRateLimitBurst, raw))
		case v < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative (0 disables rate limiting), got %q", EnvWKDRateLimitBurst, raw))
		default:
			cfg.WKDRateLimitBurst = v
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvRateLimitBurst)); raw != "" {
		v, perr := strconv.Atoi(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be an integer (burst allowance; 0 disables), got %q", EnvRateLimitBurst, raw))
		case v < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative (0 disables rate limiting), got %q", EnvRateLimitBurst, raw))
		default:
			cfg.RateLimitBurst = v
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvUndoSendWindow)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 10s), got %q", EnvUndoSendWindow, raw))
		case d < 0:
			problems = append(problems, fmt.Sprintf("%s must not be negative, got %q", EnvUndoSendWindow, raw))
		default:
			cfg.UndoSendWindow = d
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvIndexInterval)); raw != "" {
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			problems = append(problems, fmt.Sprintf("%s must be a Go duration (e.g. 5s), got %q", EnvIndexInterval, raw))
		case d <= 0:
			problems = append(problems, fmt.Sprintf("%s must be positive, got %q", EnvIndexInterval, raw))
		default:
			cfg.IndexInterval = d
		}
	}

	if !cfg.SearchDisabled && cfg.SearchRoot == "" {
		problems = append(problems, fmt.Sprintf("%s must not be empty (or set %s=1)", EnvSearchRoot, EnvSearchDisabled))
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, fmt.Sprintf("%s (or %s%s) is required", EnvDatabaseURL, EnvDatabaseURL, fileSuffix))
	}

	switch cfg.LogFormat {
	case "json", "text":
	default:
		problems = append(problems, fmt.Sprintf("%s must be \"json\" or \"text\", got %q", EnvLogFormat, cfg.LogFormat))
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("%s must be one of debug|info|warn|error, got %q", EnvLogLevel, cfg.LogLevel))
	}

	if cfg.ListenAddr == "" {
		problems = append(problems, fmt.Sprintf("%s must not be empty", EnvListenAddr))
	}

	if cfg.MasterKeyFile == "" {
		problems = append(problems, fmt.Sprintf("%s must not be empty", EnvMasterKeyFile))
	}

	problems = append(problems, cfg.validateAuth()...)

	if len(problems) > 0 {
		return nil, fmt.Errorf("config: invalid configuration: %s", strings.Join(problems, "; "))
	}

	return cfg, nil
}

// validateAuth enforces the either/or auth contract: the server must have
// exactly one usable auth path, so it never boots with no way to authenticate.
// EITHER OIDC is fully configured, OR the dev-auth subject is set. A partially
// configured OIDC block (some fields set, others missing) is rejected rather
// than silently ignored, so a typo can't cause a fully-open dev-auth fallback.
func (c *Config) validateAuth() []string {
	oidcFields := []struct {
		env, val string
	}{
		{EnvOIDCIssuer, c.OIDCIssuer},
		{EnvOIDCClientID, c.OIDCClientID},
		{EnvOIDCClientSecret, c.OIDCClientSecret},
		{EnvOIDCRedirectURL, c.OIDCRedirectURL},
	}
	oidcAny := len(c.SessionKey) > 0
	oidcAllStrings := true
	var missing []string
	for _, f := range oidcFields {
		if f.val == "" {
			oidcAllStrings = false
			missing = append(missing, f.env)
		} else {
			oidcAny = true
		}
	}

	switch {
	case c.OIDCConfigured():
		// Fully configured OIDC — the primary, secure path. OK.
		return nil
	case oidcAny:
		// OIDC was partially configured; name what's missing rather than
		// falling through to dev-auth or "no auth path".
		var probs []string
		if len(missing) > 0 {
			probs = append(probs, fmt.Sprintf("OIDC is partially configured; missing %s", strings.Join(missing, ", ")))
		}
		if oidcAllStrings && len(c.SessionKey) < MinSessionKeyLen {
			probs = append(probs, fmt.Sprintf("%s (or %s%s) must be at least %d bytes when OIDC is configured, got %d",
				EnvSessionKey, EnvSessionKey, fileSuffix, MinSessionKeyLen, len(c.SessionKey)))
		}
		if len(probs) == 0 {
			// Defensive: shouldn't happen, but never return empty here.
			probs = append(probs, "OIDC is partially configured")
		}
		return probs
	case c.DevAuthSubject != "":
		// Dev-auth escape hatch selected. It requires an explicit
		// acknowledgement (#351): being *unable* to configure OIDC is not the
		// same as intending to run with no authentication at all, and the
		// absence-based gate this replaces let a stock .env.example produce an
		// unauthenticated instance. The server still logs a loud warning.
		if c.DevAuthAck != DevAuthAckPhrase {
			return []string{fmt.Sprintf(
				"%s is set, which DISABLES AUTHENTICATION COMPLETELY — every request that reaches the port is %q, with no login. "+
					"If that is genuinely what you want, set %s=%s. "+
					"If it is not, unset %s and %s and configure the OIDC block (%s, %s, %s, %s, %s) instead.",
				EnvDevAuthSubject, c.DevAuthSubject,
				EnvDevAuthAck, DevAuthAckPhrase,
				EnvDevAuthSubject, EnvDevAuthEmail,
				EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecret, EnvOIDCRedirectURL, EnvSessionKey)}
		}
		return nil
	default:
		return []string{fmt.Sprintf(
			"no auth path configured: set the OIDC block (%s, %s, %s, %s, %s) or the dev-auth escape hatch (%s)",
			EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecret, EnvOIDCRedirectURL, EnvSessionKey, EnvDevAuthSubject)}
	}
}

// resolveSecret reads the value for name, honoring the Docker-secrets
// indirection: if name+"_FILE" is set, its contents (trimmed) win over
// name itself. An empty, unset name (and unset name+"_FILE") resolves to
// "" without error — callers decide whether that's a validation failure.
func resolveSecret(name string) (string, error) {
	if filePath := os.Getenv(name + fileSuffix); filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("config: read %s%s (%s): %w", name, fileSuffix, filePath, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

// envOrDefault returns the trimmed value of the named environment variable,
// or def if it is unset or empty.
func envOrDefault(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// envInt64 reads a positive byte/count override, returning 0 when unset or
// unparsable. Zero means "use the compiled default" everywhere it is read, so a
// typo degrades to the default rather than to nothing — the same
// fail-toward-the-intended-behaviour rule as envBool.
func envInt64(name string) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parseBool reads a boolean toggle's raw value and is the single source of
// truth for which spellings are accepted, in both directions.
//
// The three results are deliberately separate:
//   - set is false for an unset OR EMPTY value. deploy/compose.yml writes
//     every one of these as `${VAR:-}`, so an empty string is the normal way
//     an unset toggle arrives and must never be an error.
//   - ok is false for a spelling it does not recognise. That is a
//     configuration ERROR rather than a side to pick — Load refuses to boot on
//     it (#556), which is why envBool's own "unrecognised => false" no longer
//     decides anything.
func parseBool(name string) (val, set, ok bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return false, false, true
	case "1", "true", "yes", "on":
		return true, true, true
	case "0", "false", "no", "off":
		return false, true, true
	default:
		return false, true, false
	}
}

// boolToggles is every MAILROOM_* boolean Load reads. Load rejects an
// unrecognised spelling for each one; see the loop that walks this.
//
// EnvWritebackDisabled leads because it is the reason this is a safety
// property and not tidiness: it is the ONE toggle whose false is the
// PERMISSIVE direction. EnvSearchDisabled and EnvAllowPrivateAccountHosts both
// fail closed at false (search stays on; the egress guard stays on), and the
// envBoolDefault toggles default ON, so a typo there costs a feature
// rather than a guard. Nothing outside this package parses a boolean from the
// environment, so this list is the whole surface.
var boolToggles = []string{
	EnvWritebackDisabled,
	EnvSearchDisabled,
	EnvAllowPrivateAccountHosts,
	EnvIMAPIdle,
	EnvIMAPPool,
	EnvImageProxy,
	EnvImageProxyPrefetch,
	EnvLocalDKIM,
}

// envBool reads a boolean toggle that defaults to false. An unrecognised value
// reads as false here, but Load refuses to boot on one, so that branch is a
// formality rather than a decision (#556).
func envBool(name string) bool {
	v, _, _ := parseBool(name)
	return v
}

// envBoolDefault reads a boolean toggle that has a non-false default. Only an
// explicit affirmative or negative value moves it; an unset value keeps def.
// It exists for a DEFAULT-ON feature (MAILROOM_IMAP_IDLE), where envBool's
// "unset => false" would invert the intended default.
func envBoolDefault(name string, def bool) bool {
	v, set, ok := parseBool(name)
	if !set || !ok {
		return def
	}
	return v
}

// TrustedAuthservIDs reads EnvAuthservIDs as a comma-separated, lowercased
// list of ENTRIES. Moved here from cmd/mailroom (#714) because a second process now needs
// the same answer: mailroom-admin's live sync ingests mail that learned
// placement may file, and placement-report states which rule applied. One
// reader, so the server and the CLI cannot disagree about whose
// Authentication-Results are believed.
//
// ⚠ EMPTY IS A MEANINGFUL VALUE, NOT A MISSING ONE (#414). It says "believe no
// Authentication-Results header for a security decision".
//
// ⚠ THE RESULT IS ENTRIES, NOT IDS (#766). An entry may be `id<YYYY-MM-DD`
// (trusted only for mail received before that date). Do not compare them to a
// header's authserv-id: resolve them for one message with ml.TrustedAt first.
// A raw bounded entry that is not resolved matches nothing (fail closed).
func TrustedAuthservIDs() []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(EnvAuthservIDs), ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidateAuthservIDs refuses a malformed EnvAuthservIDs entry (#766): a bad
// date, an empty id, or more than one '<'. Called by Load (the server) and by
// every mailroom-admin command that reads the setting. A silent drop would
// narrow the trusted set without telling the operator.
func ValidateAuthservIDs() error {
	var problems []string
	for _, part := range strings.Split(os.Getenv(EnvAuthservIDs), ",") {
		if _, _, err := authserv.Parse(part); err != nil {
			problems = append(problems, fmt.Sprintf("%q: %v", strings.TrimSpace(part), err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s has malformed entries (want `id` or `id<YYYY-MM-DD`): %s",
			EnvAuthservIDs, strings.Join(problems, "; "))
	}
	return nil
}

// ParseTrustedProxies parses MAILROOM_TRUSTED_PROXIES: comma-separated IP
// addresses or CIDR prefixes, surrounding space ignored, blank entries skipped
// (so an empty value, which Compose passes for an unset variable, is "none"). A
// bare address is a single-host prefix. Any malformed entry is an error naming
// it.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var (
		out      []netip.Prefix
		problems []string
	)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if ip, err := netip.ParseAddr(part); err == nil && ip.Zone() == "" {
			ip = ip.Unmap()
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		problems = append(problems, fmt.Sprintf("%q", part))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s has malformed entries (want an IP address or a CIDR such as 10.0.0.0/8): %s",
			EnvTrustedProxies, strings.Join(problems, "; "))
	}
	return out, nil
}
