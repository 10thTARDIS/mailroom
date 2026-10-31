// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/clamd"
	"mailroom/internal/compose"
	"mailroom/internal/ml"
	"mailroom/internal/onboarding"
	"mailroom/internal/search"
	"mailroom/internal/store"
)

// Capability URNs Mailroom advertises (spec §13).
//
// EVERY URN here must appear in BOTH the top-level session `capabilities` map
// AND the per-account `accountCapabilities` map (session.go). A client may only
// `using` a capability that is in the TOP-LEVEL set — advertising it per-account
// alone makes the methods unreachable, which is a bug this project has already
// shipped once.
const (
	capCore       = "urn:ietf:params:jmap:core"
	capMail       = "urn:ietf:params:jmap:mail"
	capSubmission = "urn:ietf:params:jmap:submission"
	// capLabels gates the Label/* methods. The Email.labelIds property and
	// its `labelIds/<id>` patch (#704, ADR-031) belong to this extension too,
	// and — like every Email extension property (#310) — are opt-in by name,
	// not withheld by the capability.
	capLabels = "urn:mailroom:labels"
	// capRules is the T0 rules-engine extension (spec §8, mailroom#32): the Rule
	// object and Rule/get, /query, /changes, /set.
	//
	// It gates those METHODS, and a client that omits the URN genuinely cannot
	// reach them (capability.go's mayCall, which fails closed). It does NOT gate
	// the Email properties that also live under this URN — labelProvenance,
	// suggestedLabels, listId. Those are opt-in only in the sense that they are
	// absent from defaultEmailProperties, so they cost nothing until a client
	// names them; a client that names one without declaring this capability still
	// receives it. See mayCall for why the whole mechanism is per-method, and why
	// making it more than that was rejected (Forgejo #310).
	capRules = "urn:mailroom:rules"
	// capCompose covers the Mailroom compose extensions: the derived-alias
	// properties on Email (deliveredTo/identityId — spec §5's catchall story)
	// and the identity/submission extension properties.
	capCompose = "urn:mailroom:compose"
	// capAccounts is the in-UI account-management extension (spec §12): the
	// Account object and Account/get, /query, /changes, /set.
	capAccounts = "urn:mailroom:accounts"
	// capContacts is the auto-derived internal contacts extension (spec §10a,
	// #37): the read-only Contact object and Contact/get, /query, /changes.
	capContacts = "urn:mailroom:contacts"
	// capTriage is the T1 priority-scoring extension (spec §8, #33/#34): the
	// read-only priorityScore / isImportant / explanation properties on the Email
	// object, plus the Triage control methods added in #34 — Triage/get (model
	// state), Triage/set (reset/rollback safety valve) and Triage/correct
	// (one-click importance correction).
	//
	// THE TWO HALVES ARE PROTECTED DIFFERENTLY, and the difference is worth
	// stating because this comment used to imply otherwise (Forgejo #310). The
	// Triage/* METHODS are gated: omit the URN and mayCall answers unknownMethod.
	// The Email PROPERTIES are not, and cannot be — methodFn never sees `using`
	// (request.go), so emailGet builds its property set from the client's
	// `properties` array alone. A client that asks for priorityScore having
	// declared only urn:ietf:params:jmap:mail receives priorityScore. What keeps
	// that acceptable is that the property is one user's own score for one user's
	// own message, reached through RLS under an authenticated id — not the
	// capability list. See mayCall.
	capTriage = "urn:mailroom:triage"
	// capPush is the Web Push extension (spec §12, #42, ADR-016): the
	// WebPushSubscription object and its /get, /set. Its session capability object
	// carries the server's VAPID applicationServerKey — the live value is injected
	// by session.go, since it is instance-specific state, not a constant.
	capPush = "urn:mailroom:push"
	// capOrganize is the local-first organization extension (spec §6, #51): the
	// pin (isPinned/pinnedAt on Email), the thread mute (isMuted on Thread,
	// settable through Thread/set), and the snooze (isSnoozed/snoozedUntil on
	// Email, set by patching snoozedUntil, plus the isSnoozed Email/query filter
	// condition that backs the Snoozed view).
	//
	// It is its OWN capability rather than an addition to urn:ietf:params:jmap:mail
	// because RFC 8621 defines neither property nor a Thread/set method at all.
	// Gating it here keeps the mail capability honestly conformant.
	//
	// ⚠ "CANNOT REACH THEM" IS TRUE OF THE METHOD AND NOT OF THE PROPERTIES, and
	// this comment claimed both until Forgejo #310. Thread/set is gated: a client
	// that omits this URN gets unknownMethod, fail-closed, tested. isPinned /
	// pinnedAt / isSnoozed / snoozedUntil are NOT gated — they are merely absent
	// from defaultEmailProperties, so a client that names one in `properties`
	// receives it whatever it declared. The isSnoozed Email/query filter condition
	// is likewise reachable by anyone who may call Email/query at all. See mayCall
	// in capability.go for the mechanism and for why it was left per-method.
	capOrganize = "urn:mailroom:organize"
	// capRemoteContent is the remembered per-sender "show remote content" decision
	// (#202, spec §11): the RemoteAllow object and RemoteAllow/get, /changes, /set.
	//
	// Its own capability rather than a corner of capMail, because remote-content
	// policy is not mail data — and because a client that does not declare this URN
	// then cannot read or write the allow list at all, which is the right failure:
	// such a client falls back to blocking remote content and asking every time,
	// i.e. to the safe default.
	capRemoteContent = "urn:mailroom:remotecontent"
	// capComposeSnippets is saved signatures and templates (#56, spec §5): the
	// ComposeSnippet object and its /get, /changes, /set. Separate from capCompose,
	// which covers the derived-alias properties on Email and Identity — those are
	// facts about a message, these are stored text the user authored.
	capComposeSnippets = "urn:mailroom:composesnippets"
	// capUnsubscribe is unsubscribe detection (#58, matrix §6): the `unsubscribe`
	// property on Email, which is List-Unsubscribe (RFC 2369) parsed into an https
	// link and a mailto. Read-only, and SURFACING ONLY — nothing here sends an
	// unsubscribe, and RFC 8058 one-click is deliberately not implemented, because a
	// server-side POST to a URL out of a message would be this project's first
	// outbound fetch on behalf of a message (docs/hostile-content.md §2.3).
	//
	// Its own capability rather than a corner of capMail, because RFC 8621 defines no
	// such property and naming it separately keeps the mail capability honestly
	// conformant: a client that speaks RFC 8621 and nothing else is not implicitly
	// claiming to understand a property the RFC never defined.
	//
	// BE PRECISE ABOUT WHAT THIS BUYS: the `using` gate is per-METHOD (capability.go's
	// mayCall), so declaring this URN is what a conformant client is REQUIRED to do,
	// not a mechanism that withholds the property from one that does not. Like every
	// other Mailroom Email property, `unsubscribe` is opt-in only in the sense that it
	// is absent from defaultEmailProperties, so it costs nothing until a client names
	// it. The safety of the value does not rest on the capability at all — it rests on
	// parseListUnsubscribe having refused everything it could not vouch for.
	//
	// This URN in fact gates NO METHOD AT ALL — there is no Unsubscribe/* method,
	// because nothing here sends one — so it is purely a conformance signal. It is
	// listed as such, and that listing is asserted, in capability.go's
	// capabilitiesGatingNoMethod (Forgejo #310).
	capUnsubscribe = "urn:mailroom:unsubscribe"
	// capSenderAuth is what the reading pane can say about a SENDER (#55, #397,
	// matrix §2). Two read-only properties on Email, and they answer different
	// questions:
	//
	//   - `authResults` — the Authentication-Results header (RFC 8601) parsed per
	//     method into what SPF, DKIM and DMARC each said. REPORTING ONLY:
	//     Mailroom verifies nothing itself, it relays what the servers that
	//     handled the message wrote down.
	//   - `senderWarnings` (#397) — whether the sending DOMAIN imitates one the
	//     user knows, or claims to be the user's own.
	//
	// ⚠ THE SECOND IS NOT A REFINEMENT OF THE FIRST, and the URN's name
	// undersells it. Authentication passing says the mail really came from
	// `paypa1.com`; it says nothing about whether that is a domain anyone has
	// heard of. A lookalike domain passes SPF and DKIM perfectly — the attacker
	// configured them. The URN keeps its published spelling because clients key
	// on it and renaming would break them; this comment is where the accuracy
	// lives.
	//
	// Its own capability rather than a corner of capMail, for capUnsubscribe's
	// reason: RFC 8621 defines no such property, so a client that speaks the RFC
	// and nothing else should not be implicitly claiming to understand it.
	//
	// ⚠ WHAT THIS URN DOES NOT DO, stated because the property is security-shaped
	// and that invites the wrong reading: it withholds nothing. The `using` gate
	// is per-METHOD (capability.go's mayCall), so a client that names
	// `authResults` without declaring this URN still receives it — it is merely
	// non-conformant. Nothing about the SAFETY of the value rests here either.
	// That rests on authResultsObject reporting the header's facts and refusing to
	// synthesise a verdict, and on its three-way null / present:false / present:
	// true distinction, which is where the actual correctness lives.
	//
	// For `senderWarnings` the equivalent is that its precision was MEASURED
	// rather than argued: the rule that fires here reduces both domains to what
	// they LOOK like and compares, which collides on none of the domains a
	// real user corresponds with while catching crafted lookalikes of
	// those same domains. Two rules that would also have shipped under this URN
	// were measured and dropped — external-sender marking (nearly all of a catch-all
	// mailbox) and display-name impersonation (wrong every time, because
	// it measures ESP delegation). See internal/senderwarn.
	//
	// Like capUnsubscribe and capSearch this URN gates NO METHOD AT ALL — there is
	// no SenderAuth/* method, because there is nothing to call — so it is purely a
	// conformance signal, listed and asserted as such in capability.go's
	// capabilitiesGatingNoMethod.
	capSenderAuth = "urn:mailroom:senderauth"
	// capSavedSearches is named queries and the sidebar's smart folders (#57): the
	// SavedSearch object and its /get, /changes, /set.
	//
	// Separate from capSearch, which advertises the query SYNTAX and its operators. That
	// is a property of the search engine; this is stored state the user authored. A
	// server with no search index still holds saved searches (they simply fail to run,
	// exactly as typing the same text would), so tying them to capSearch would make a
	// broken index look like missing folders.
	capSavedSearches = "urn:mailroom:savedsearches"
	// capCategoryTabs is the Gmail-style category tabs (#219, spec §8): the
	// CategoryTab object and its /get, /changes, /set, plus the `tab` property on
	// Label that says which tab a label's mail feeds.
	//
	// Separate from capLabels even though the mapping rides on the Label object,
	// because tabs and labels are two ORTHOGONAL axes — tabs are message FUNCTION
	// (decided from public RFC facts), labels are TOPIC (decided from identity and
	// rules). A client that manages labels and knows nothing about tabs is a
	// coherent client, and under capability.go's fail-closed rule it simply cannot
	// reach the tab surface. Folding tabs into capLabels would make "I support
	// labels" mean two unrelated things.
	capCategoryTabs = "urn:mailroom:categorytabs"
	// capAttachmentScan is the attachment malware quarantine (#396, ADR-027): the
	// `attachmentScan` property on Email and the Email/releaseAttachment method.
	//
	// Its own URN rather than a corner of capMail for capSenderAuth's reason —
	// RFC 8621 defines no such property — and for one more that is specific to it:
	// a client declaring this URN is claiming to understand that some parts CANNOT
	// BE DOWNLOADED. A client that does not know that will render a download link
	// which returns 403, and the honest place to record the difference is the
	// capability set.
	//
	// ⚠ WHAT THE URN DOES NOT DO, said plainly because the last security-shaped
	// capability invited the wrong reading: it withholds nothing and it enforces
	// nothing. The `using` gate is per-METHOD, so a client that names
	// `attachmentScan` without declaring this URN still receives it, and one that
	// declares nothing at all is still refused the quarantined bytes. THE
	// ENFORCEMENT IS THE DOWNLOAD GATE IN blob.go, which no capability declaration
	// can reach — that is the whole reason §8 put it there rather than in a
	// property a client may ignore.
	capAttachmentScan = "urn:mailroom:attachmentscan"
	// capDesk is the desk (#442): the Area object and its methods (Area/get,
	// /changes, /set, /getSummary), the DeskPreferences singleton, the
	// per-client-family ClientPreferences, the `feedsDesk` property on
	// Mailbox, the `inArea`/`unsorted` filters on Email/query, and the `area`
	// property on Email.
	//
	// DAY-ONE SETUP RIDES ON THE SAME URN (#446), because it is the desk being
	// set up rather than a separate capability: `Setup/census`,
	// `Setup/preview`, `Setup/apply`, `Setup/get`, `Setup/changes` and
	// `Setup/undo`; the `deskArchived` Email/query filter and the
	// `deskArchivedAt` property on Email ("archived in Mailroom" — off the desk
	// with NOTHING moved on the server, distinct from `drawerArchivedAt`, which
	// is a real move); and `setupCompletedAt` on DeskPreferences, which is NULL
	// for a login that has not been through day-one and is how the client knows
	// to open the wizard rather than the desk.
	//
	// Its own URN, not a corner of capCategoryTabs or capLabels: areas are a
	// THIRD axis — a partition the user ARRANGES, fed BY tabs and labels —
	// exactly the way tabs and labels are already two orthogonal axes rather
	// than one mechanism doing both jobs (capCategoryTabs' own comment). A
	// client that manages tabs or labels without knowing about the desk is a
	// coherent client, and under capability.go's fail-closed rule it simply
	// cannot reach this surface.
	capDesk = "urn:mailroom:desk"
	// capOutOfOffice is the out-of-office reply (#50): OutOfOffice/get, /set
	// and /check, one object per MAIL account, written to the provider over
	// ManageSieve (internal/outofoffice).
	capOutOfOffice = "urn:mailroom:outofoffice"
	// capPGP is #448/#374 increment 1: UserKey/get (the login's OWN keys, public
	// facts only — spec §9, #48) and — riding on capContacts, not gated by
	// this URN — the Contact.keys property and Contact/query's hasKey filter
	// (correspondent keys, #48's discovery side).
	//
	// UserKey/get is a NEW method and therefore genuinely gated: a client that
	// omits this URN cannot reach it (capability.go's mayCall, fail-closed).
	// Contact.keys and hasKey are NOT gated by it — they ride on Contact/get and
	// Contact/query, which capContacts already gates — for the reason every
	// other opt-in property in this file gives: methodFn never sees `using`
	// (capability.go's mayCall doc comment), so a client naming `keys` in
	// Contact/get's properties, or `hasKey` in a filter, receives it whatever it
	// declared. Advertising capPGP is what a conformant client MUST do before
	// using either — it is not what makes them reachable.
	capPGP = "urn:mailroom:pgp"
	// capCalendar is calendar invitations (#725, ADR-033): the read-only
	// `calendarInvite` property on Email (an invitation, cancellation or reply
	// parsed from the message's text/calendar part on the SERVER) and the
	// Email/replyToInvite method that answers one.
	//
	// The property rides on Email/get like every other Mailroom property and is
	// opt-in in the usual sense (absent from defaultEmailProperties). The METHOD
	// is genuinely gated: it sends mail on the user's behalf, so a client must
	// declare this URN to reach it (capability.go's mayCall, fail-closed).
	//
	// Its own URN, not a corner of capMail: RFC 8621 defines neither, and #74
	// (CalDAV, 3.x) will hang the rest of calendar support here.
	capCalendar = "urn:mailroom:calendar"
	// capAPIKeys is agent access (#49, ADR-042): ApiKey/get, /set, /getEvents
	// and PendingAction/get, /set. Its session object states the scope set, the
	// limit defaults and caps, and the approval window.
	//
	// Every one of these methods is session-only EXCEPT PendingAction/get, which
	// an API key may call for its own requests (the scope table, agent_scopes.go).
	capAPIKeys = "urn:mailroom:apikeys"
	// capDevices is the signed-in devices list (#937, ADR-048 section 6):
	// SignedInDevice/get and SignedInDevice/set (destroy only). Both are
	// session-only: an API key is not a session and must never end one.
	capDevices = "urn:mailroom:devices"
)

// maxRequestBytes caps the POST /jmap body (advertised as maxSizeRequestObject).
const maxRequestBytes = 10 << 20 // 10 MiB

// maxUploadBytes caps one blob upload. It IS the session's advertised
// maxSizeUpload (session.go reads this const), because a limit a client is told
// about and a limit the server enforces that disagree is worse than either.
const maxUploadBytes = 50 << 20 // 50 MiB

// API is the client-facing JMAP service (RFC 8620 core + an RFC 8621 subset).
// It reads the authenticated user id from the request context (injected by
// auth.Middleware — the id NEVER comes from a request parameter, spec §10) and
// funnels every data access through internal/store's UserTx, so RLS scopes all
// of it to that user.
type API struct {
	store *store.Store
	// trustedAuthservIDs gates the auth half of the selfSpoof warning (#414).
	// Empty means no Authentication-Results header is believed for that
	// decision — fail-safe: the warning fires rather than being suppressed.
	trustedAuthservIDs []string
	sessionState       string
	methods            map[string]methodFn
	// version is the server version the MCP endpoint reports (WithVersion).
	version string
	// composer authors outgoing mail (drafts + submissions). It is optional:
	// an instance with no blob store still serves every read method, it just
	// cannot compose. Email/set create and EmailSubmission/set say so plainly
	// rather than failing obscurely.
	composer *compose.Composer
	// raw reads message bytes out of the encrypted blob store — the source of
	// truth for every Email property that is not projected into Postgres
	// (bodies, recipient lists). See RawSource.
	raw RawSource
	// blobs is the read/write blob seam the download and upload endpoints need
	// (RawSource can only read whole messages into memory). Nil when no blob
	// store is configured: /jmap/download then finds nothing and /jmap/upload
	// says so.
	blobs BlobStore
	// baseCtx bounds long-lived handlers (the EventSource stream). Cancelling it
	// — main cancels it on SIGINT/SIGTERM — makes every open push connection
	// unwind at once, so graceful shutdown is not held hostage by them.
	baseCtx context.Context
	// eventConns counts open EventSource streams (bounded: see eventsource.go).
	eventConns atomic.Int64
	// limiter throttles requests per authenticated user (spec §10). Nil means
	// limiting is off. New installs the enforced default; WithRateLimit overrides
	// it, and Handler wraps the whole JMAP surface with it — so a 429 is answered
	// for EVERY route, not a list of the ones we remembered to guard.
	limiter *RateLimiter
	// onTiming observes each request's per-method timing (#665). Nil means
	// dispatch takes no timings at all.
	onTiming func(RequestTiming)
	// remoteAsks records one-off "show remote content" clicks so the proxy can
	// honour §3c's second clause. Nil when the proxy is off.
	remoteAsks *remoteRequests
	// proxy serves GET /jmap/proxy and, when enabled, makes bodyValues park a
	// same-origin proxy reference INSTEAD of the sender's URL (ADR-026 §3b).
	// Nil means the feature is off and nothing about rendering changes.
	proxy *imageProxy
	// onProxy and proxyLog observe each proxy request (#674): the sink feeds the
	// counters, the logger takes one INFO line per refusal. Both are optional.
	onProxy  func(ProxyOutcome)
	proxyLog *slog.Logger
	// onProxyCache receives a user's cache size, split by reference, after each
	// eviction pass and after each prefetch run (#922). Nil reports nothing.
	onProxyCache func(userID ulid.ULID, split store.ProxyCacheSplit)
	// accountSecrets seals account credentials server-side for the Account/set
	// surface (urn:mailroom:accounts). Nil when account management is not wired:
	// Account read/metadata paths still work, but a create (or connection change)
	// returns notImplemented rather than storing an unsealed secret.
	accountSecrets AccountSecrets
	// accountDialer validates a new/changed account by dialing its IMAP server.
	// Nil means "use onboarding's production sync.Dial"; tests inject a fake.
	accountDialer onboarding.Dialer
	// accountAllowPrivate widens the account-validation egress guard to permit
	// private/loopback IMAP hosts (#527). False is the safe default.
	accountAllowPrivate bool
	// oauth is the mail-provider OAuth connect flow (#52, ADR-046): the
	// Account/oauthStart and Account/oauthDisconnect methods, the
	// oauthProviders capability field, and Forget on account deletion. Nil
	// when no provider is configured.
	oauth OAuthConnector
	// oauthOps reports providers refusing this server's client registration
	// to admins in Account/getHealth (#52). Nil when no provider is configured.
	oauthOps OAuthOperatorStatus
	// gmailBudget is the Gmail daily download budget Account/getHealth's
	// gmailImport progress estimates against (#52); 0 = the default.
	gmailBudget int64
	// vapidPublicKey is the base64url VAPID applicationServerKey advertised in the
	// urn:mailroom:push session capability (ADR-016). Empty when push is not wired
	// (the capability is still advertised, with an empty key). Set via WithPushKey.
	vapidPublicKey string
	// search is the installed search service, if any (SetSearch). Nil means search
	// is disabled: Email/query still answers structured filters and rejects text
	// ones, and Account/getHealth omits the search-staleness signal rather than
	// reporting on an index that does not exist (search.go, account_health.go).
	search *search.Service
	// registry is the per-user priority-model registry (#35) that backs the Triage
	// controls (Triage/get / Triage/set). Nil when no blob store is wired: the
	// Triage controls then return a clean errTriageUnavailable rather than panicking
	// (Triage/correct is independent of it — it only writes feedback_events). Set
	// via WithTriageRegistry; it is the SAME instance the ingest Scorer uses.
	registry *ml.Registry
	// scanner is the installed malware scanner, if any (SetScanner). Nil means
	// no scanner is configured: Account/getHealth then OMITS the attachmentScanner
	// object entirely rather than reporting freshness for a daemon that does not
	// exist (#395, ADR-027 §5c). ⚠ Nil is not the same as "the scanner is down" —
	// that answers with reachable:false, because a deliberate choice and an
	// outage must not look alike.
	scanner ScannerHealth
	// writebackNudge asks the sync runner to drain this user's queued writeback
	// intents now rather than at the next scheduled pass (#581). Nil means no
	// runner is wired (tests, a read-only deployment): the intents then simply
	// wait for the poll, which is the behaviour every release before this had.
	//
	// ⚠ It must NOT block the response. The installed implementation returns
	// immediately and does the work on its own goroutines under the runner's
	// per-account gate; see syncrunner.NudgeWriteback.
	writebackNudge func(userID ulid.ULID)

	// followUps caches parsed message headers for FollowUp/get (#58):
	// follow_up.go explains why a parse is paid once per message.
	followUps *followUpCache

	// outOfOffice is the out-of-office reply service (#50). Nil means the
	// OutOfOffice methods answer serverFail ("not configured").
	outOfOffice OutOfOfficeService

	// decrypter opens PGP/MIME encrypted mail on open (#47 6b, ADR-037). Nil
	// means encrypted mail is shown as stored, marked encrypted and not
	// decrypted. See decrypt.go.
	decrypter Decrypter
	// onDemandScan scans attachments of decrypted messages at download. Nil
	// means no scanner: those parts are `unscanned`, which does not block.
	onDemandScan OnDemandScanner
	// onDemandObserve counts each on-demand verdict (WithOnDemandObserver).
	onDemandObserve func(verdict string)
	// keyFetcher looks up recipients' keys over WKD for RecipientKey/lookup
	// (#47, ADR-038). Nil means only stored keys are consulted.
	keyFetcher KeyFetcher
	// custody is the crypto process's key verbs for own-key management (#796,
	// user_key_manage.go). Nil means those methods answer
	// keyManagementUnavailable.
	custody KeyCustody
	// wkdDomain is MAILROOM_WKD_DOMAIN (lowercased), the one mail domain this
	// instance publishes keys for. Empty means WKD is off.
	wkdDomain string
	// keyOpLimiter is the per-user budget for generate, import and export
	// (user_key_manage.go). Always set by New.
	keyOpLimiter *RateLimiter
	// refresh is ContactKey/refresh's per-user and per-domain budget and its
	// per-key interval (#818, contact_key_refresh.go); filled on first use.
	refresh refreshLimits

	// alerts pushes API key anomalies and approval requests (#49, ADR-042 §6).
	// Nil means they are audited and not pushed.
	alerts AlertNotifier
	// now is the clock the API key rules read; nil means time.Now (a test seam
	// for the off-hours rule and the approval window).
	now func() time.Time

	// sessionRevoker revokes a signed-out device's refresh token at the identity
	// provider (WithSessionRevoker). Nil means no provider revocation.
	sessionRevoker SessionRevoker
	// sessionLive is the EventSource session re-check (WithSessionLive). Nil
	// means streams are not re-checked.
	sessionLive SessionLive
	// streamRecheck overrides streamRecheckInterval (a test seam).
	streamRecheck time.Duration
}

// ScannerHealth is the slice of the clamd client Account/getHealth needs: what
// the daemon is running, and how old its signatures are.
//
// An interface rather than *clamd.Client so the health surface can be tested
// without a daemon — the states that matter here (unreachable, stale, an unknown
// publication time) are seconds of fake and hours of container.
type ScannerHealth interface {
	Version(ctx context.Context) (clamd.Version, error)
}

// SetScanner installs the malware scanner used for the Account/getHealth
// freshness reading. Call it during wiring, before Handler() is mounted.
//
// ⚠ It is health-only. Scanning at ingest is wired separately (ingest.WithScanner)
// and re-scanning through the job dispatcher; this exists so the panel can say
// how fresh the answers are, which is ADR-027 §5c's requirement that a database
// that has stopped moving becomes a FAULT rather than a silent no-op.
func (a *API) SetScanner(sc ScannerHealth) { a.scanner = sc }

// SetWritebackNudge installs the "drain the writeback queue now" signal (#581).
// Wired in cmd/mailroom once the sync runner exists; nil leaves the pre-#581
// behaviour, where a user's action reaches the provider at the next poll.
func (a *API) SetWritebackNudge(f func(userID ulid.ULID)) { a.writebackNudge = f }

// nudgeWriteback signals the runner, if one is wired. Safe to call on every
// write path: it is a no-op without a runner, and the runner itself skips an
// account with nothing pending (and never dials for one in ingest-only mode).
func (a *API) nudgeWriteback(userID ulid.ULID) {
	if a.writebackNudge != nil {
		a.writebackNudge(userID)
	}
}

// RawSource reads a message's raw RFC 5322 bytes out of the encrypted blob
// store. It is the ONE seam through which the API layer reaches the bytes.
//
// *compose.Composer satisfies it (ReadRaw), and that is deliberately how the
// server wires it: compose ALREADY holds the blob store and the mail-blob DEK
// resolver, and a second key-derivation path for "the read side" is exactly the
// kind of drift that silently decrypts garbage (see cmd/mailroom's keyResolver:
// the DEK is derived once, from syncrunner's constants, and shared).
type RawSource interface {
	ReadRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error)
}

// BlobStore is the full blob seam: RawSource plus the streaming read, the
// upload write, and the per-user existence check the blob endpoints need. It is
// the SAME seam (and therefore the same derived mail-blob DEK) compose uses —
// *compose.Composer satisfies it — because a second key-derivation path for
// "the blob endpoints" would silently decrypt garbage the day the two drift.
type BlobStore interface {
	RawSource
	// OpenRaw streams a message's bytes without buffering them.
	OpenRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) (io.ReadCloser, error)
	// PutRaw stores an uploaded blob for the user and reports its metadata.
	PutRaw(ctx context.Context, userID ulid.ULID, r io.Reader) (compose.StoredBlob, error)
	// HasRaw reports whether THIS USER has a blob with this content hash. The
	// blob store is namespaced per user (a keyed path derivation) and every blob
	// is sealed under that user's DEK, so this is an ownership question, not a
	// content-addressed lookup.
	HasRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) (bool, error)
	// DeleteRaw removes a blob FILE whose app.blobs row the store has already
	// reclaimed under its "nothing references these bytes" guard (#300). It is
	// declared HERE, on the interface, rather than discovered with a type
	// assertion at the call site: a reclaim that silently skipped the file
	// because the blob store did not happen to implement it would turn a leaked
	// row into an unreadable file, which is strictly worse. See
	// blobreclaim.go for the ordering this must be called with.
	DeleteRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) error
}

// Option configures the API.
type Option func(*API)

// WithComposer enables the compose/send surface (Email/set create,
// EmailSubmission/set) and, with it, the blob reads Email/get needs for bodies
// and the blob endpoints (/jmap/download, /jmap/upload) — the Composer is both a
// RawSource and a BlobStore. Without it those methods return notImplemented and
// Email/get serves only the properties Postgres projects.
func WithComposer(c *compose.Composer) Option {
	return func(a *API) {
		a.composer = c
		a.blobs = c
	}
}

// WithTriageRegistry wires the per-user priority-model registry (#35) that backs
// the Triage control methods (Triage/get / Triage/set): read the model's active
// version and lineage, and reset/rollback it. Without it those methods return a
// clean errTriageUnavailable (never a panic); Triage/correct works regardless, as
// it only records feedback_events. The instance passed here is the SAME one the
// ingest Scorer serves from, so the controls and the scorer never diverge on which
// model is live.
func WithTriageRegistry(r *ml.Registry) Option {
	return func(a *API) { a.registry = r }
}

// WithRawSource overrides where Email/get reads message bytes from. It exists
// for tests and for an instance that serves mail without being able to compose
// it; the normal wiring gets one for free from WithComposer.
func WithRawSource(r RawSource) Option {
	return func(a *API) { a.raw = r }
}

// WithBaseContext bounds the lifetime of long-lived handlers (EventSource). The
// server cancels it on shutdown; every open push stream then closes itself
// rather than holding the graceful-shutdown deadline open.
func WithBaseContext(ctx context.Context) Option {
	return func(a *API) { a.baseCtx = ctx }
}

// WithPushKey supplies the base64url VAPID applicationServerKey (the 65-byte
// uncompressed P-256 public point) the session advertises under urn:mailroom:push
// so a browser can subscribe against this server (ADR-016). Without it the push
// capability is still advertised but carries an empty key.
func WithPushKey(publicKeyBase64URL string) Option {
	return func(a *API) { a.vapidPublicKey = publicKeyBase64URL }
}

// WithRateLimit sets the per-user request-rate limit (spec §10). A config that
// disables limiting (non-positive rate or burst) turns the limiter off entirely.
// Without this option New installs the enforced default (DefaultRateLimit*).
func WithRateLimit(cfg RateLimitConfig) Option {
	return func(a *API) { a.limiter = NewRateLimiter(cfg) }
}

// New constructs the JMAP service over the store. The session state is a static
// value for 0.5 (accounts/capabilities do not change during a session); it will
// become dynamic when account management lands.
func New(st *store.Store, opts ...Option) *API {
	a := &API{store: st, sessionState: "0", baseCtx: context.Background(),
		limiter: NewRateLimiter(RateLimitConfig{
			PerSecond: DefaultRateLimitPerSecond, Burst: DefaultRateLimitBurst}),
		keyOpLimiter: newKeyOpLimiter()}
	for _, o := range opts {
		o(a)
	}
	if a.raw == nil && a.composer != nil {
		a.raw = a.composer
	}
	a.followUps = newFollowUpCache(followUpCacheSize)
	if a.baseCtx == nil {
		a.baseCtx = context.Background()
	}
	a.methods = map[string]methodFn{
		"Core/echo":                    echo,
		"Email/get":                    a.emailGet,
		"Email/query":                  a.emailQuery,
		"Email/queryChanges":           a.emailQueryChanges,
		"Email/changes":                a.emailChanges,
		"Email/set":                    a.emailSet,
		"Email/dismissCategory":        a.emailDismissCategory,
		"Email/releaseAttachment":      a.emailReleaseAttachment,
		"Email/replyToInvite":          a.emailReplyToInvite,
		"Mailbox/get":                  a.mailboxGet,
		"Mailbox/query":                a.mailboxQuery,
		"Mailbox/changes":              a.mailboxChanges,
		"Mailbox/set":                  a.mailboxSet,
		"Thread/get":                   a.threadGet,
		"Thread/changes":               a.threadChanges,
		"Thread/set":                   a.threadSet,
		"Label/get":                    a.labelGet,
		"Label/query":                  a.labelQuery,
		"Label/changes":                a.labelChanges,
		"Label/set":                    a.labelSet,
		"ComposeSnippet/get":           a.composeSnippetGet,
		"ComposeSnippet/changes":       a.composeSnippetChanges,
		"ComposeSnippet/set":           a.composeSnippetSet,
		"SavedSearch/get":              a.savedSearchGet,
		"SavedSearch/changes":          a.savedSearchChanges,
		"SavedSearch/set":              a.savedSearchSet,
		"CategoryTab/get":              a.categoryTabGet,
		"CategoryTab/changes":          a.categoryTabChanges,
		"CategoryTab/set":              a.categoryTabSet,
		"RemoteAllow/get":              a.remoteAllowGet,
		"RemoteAllow/changes":          a.remoteAllowChanges,
		"RemoteAllow/set":              a.remoteAllowSet,
		"RemoteContent/request":        a.remoteContentRequest,
		"Rule/get":                     a.ruleGet,
		"Rule/query":                   a.ruleQuery,
		"Rule/changes":                 a.ruleChanges,
		"Rule/set":                     a.ruleSet,
		"Rule/preview":                 a.rulePreview,
		"Rule/induce":                  a.ruleInduce,
		"Rule/tighten":                 a.ruleTighten,
		"Rule/apply":                   a.ruleApply,
		"Triage/get":                   a.triageGet,
		"Triage/set":                   a.triageSet,
		"Triage/correct":               a.triageCorrect,
		"Triage/open":                  a.triageOpen,
		"Account/get":                  a.accountGet,
		"Account/query":                a.accountQuery,
		"Account/changes":              a.accountChanges,
		"Account/set":                  a.accountSet,
		"Account/getHealth":            a.accountGetHealth,
		"Account/oauthStart":           a.accountOAuthStart,
		"Account/oauthDisconnect":      a.accountOAuthDisconnect,
		"Identity/get":                 a.identityGet,
		"Identity/changes":             a.identityChanges,
		"Identity/set":                 a.identitySet,
		"Contact/get":                  a.contactGet,
		"Contact/query":                a.contactQuery,
		"Contact/changes":              a.contactChanges,
		"Contact/set":                  a.contactSet,
		"Contact/merge":                a.contactMerge,
		"Contact/separate":             a.contactSeparate,
		"SenderStats/get":              a.senderStatsGet,
		"FollowUp/get":                 a.followUpGet,
		"FollowUp/set":                 a.followUpSet,
		"SenderStats/emailIds":         a.senderStatsEmailIDs,
		"UserKey/get":                  a.userKeyGet,
		"RecipientKey/lookup":          a.recipientKeyLookup,
		"ContactKey/decide":            a.contactKeyDecide,
		"ContactKey/import":            a.contactKeyImport,
		"ContactKey/get":               a.contactKeyGet,
		"ContactKey/set":               a.contactKeySet,
		"ContactKey/history":           a.contactKeyHistory,
		"ContactKey/refresh":           a.contactKeyRefresh,
		"UserKey/generate":             a.userKeyGenerate,
		"UserKey/import":               a.userKeyImport,
		"UserKey/set":                  a.userKeySet,
		"UserKey/extend":               a.userKeyExtend,
		"UserKey/revoke":               a.userKeyRevoke,
		"UserKey/export":               a.userKeyExport,
		"UserKey/replace":              a.userKeyReplace,
		"EmailSubmission/get":          a.submissionGet,
		"EmailSubmission/query":        a.submissionQuery,
		"EmailSubmission/changes":      a.submissionChanges,
		"EmailSubmission/set":          a.submissionSet,
		"EmailSubmission/getSendQuota": a.submissionSendQuota,
		"WebPushSubscription/get":      a.pushGet,
		"WebPushSubscription/set":      a.pushSet,
		"SignedInDevice/get":           a.signedInDeviceGet,
		"SignedInDevice/set":           a.signedInDeviceSet,
		"Area/get":                     a.areaGet,
		"Area/changes":                 a.areaChanges,
		"Area/set":                     a.areaSet,
		"Area/getSummary":              a.areaGetSummary,
		"Area/getDesk":                 a.areaGetDesk,
		"DeskPreferences/get":          a.deskPreferencesGet,
		"DeskPreferences/changes":      a.deskPreferencesChanges,
		"DeskPreferences/set":          a.deskPreferencesSet,
		"ClientPreferences/get":        a.clientPreferencesGet,
		"ClientPreferences/changes":    a.clientPreferencesChanges,
		"ClientPreferences/set":        a.clientPreferencesSet,
		"Setup/census":                 a.setupCensus,
		"Setup/preview":                a.setupPreview,
		"Setup/apply":                  a.setupApply,
		"Setup/get":                    a.setupGet,
		"Setup/changes":                a.setupChanges,
		"Setup/undo":                   a.setupUndo,
		"OutOfOffice/get":              a.outOfOfficeGet,
		"OutOfOffice/set":              a.outOfOfficeSet,
		"OutOfOffice/check":            a.outOfOfficeCheck,
		"ApiKey/get":                   a.apiKeyGet,
		"ApiKey/set":                   a.apiKeySet,
		"ApiKey/getEvents":             a.apiKeyGetEvents,
		"PendingAction/get":            a.pendingGet,
		"PendingAction/set":            a.pendingSet,
	}
	return a
}

// Handler returns the JMAP HTTP surface. It must be mounted BEHIND
// auth.Middleware (which authenticates and injects the user id): EVERY endpoint
// here requires an authenticated identity and takes the user id from the token
// context only — never from the {accountId} in a path (spec §10). The Session
// resource is served at both /jmap/session and the well-known alias
// /.well-known/jmap (RFC 8620 §2.2).
//
// Every URL the Session resource advertises is routed here, and nothing else is:
// a session that advertises a downloadUrl the server does not serve is a 404 on
// every attachment (which is what this instance did until #85).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jmap/session", a.SessionHandler)
	mux.HandleFunc("GET /.well-known/jmap", a.SessionHandler)
	mux.HandleFunc("POST /jmap", a.RequestHandler)
	mux.HandleFunc("GET /jmap/download/{accountId}/{blobId}/{name}", a.DownloadHandler)
	mux.HandleFunc("GET /jmap/preview/{accountId}/{blobId}/{name}", a.PreviewHandler)
	mux.HandleFunc("GET /jmap/proxy/{accountId}/{messageId}/{token}", a.ProxyHandler)
	mux.HandleFunc("POST /jmap/upload/{accountId}/", a.UploadHandler)
	mux.HandleFunc("GET /jmap/eventsource/", a.EventSourceHandler)
	// MCP tools for API keys (#49, ADR-044): every method, because the
	// transport answers GET and DELETE itself (405 in stateless mode).
	mux.Handle(MCPPath, a.mcpHandler())
	// Per-user rate limiting wraps the whole surface, inside nosniff so even a 429
	// carries the hardening headers. It sits here (behind the auth middleware that
	// mounts this handler) so it reads the authenticated user id, never a param.
	return nosniff(a.limiter.middleware(a.keyPolicyGate(mux)))
}

// nosniff puts X-Content-Type-Options on EVERY response the JMAP surface makes,
// including the ones no handler above wrote: a 405 from the mux, a 404 for
// /jmap/nope, a 400 from Go's own request parsing. writeJSON and writeBlobHeaders
// each set it too — deliberately, so a handler that is mounted somewhere else
// tomorrow keeps its own header — but "nosniff everywhere" has to be a property
// of the surface, not a list of places we remembered (spec §11, #39).
func nosniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// JMAPPaths are the routes Handler serves. cmd/mailroom mounts each of them
// behind the 401-flavored auth middleware (a browser redirect is meaningless to
// a programmatic client), so the list lives here rather than being restated —
// and forgotten — there.
func JMAPPaths() []string {
	return []string{
		"/jmap",
		"/jmap/session",
		"/.well-known/jmap",
		"/jmap/download/",
		"/jmap/preview/",
		"/jmap/proxy/",
		"/jmap/upload/",
		"/jmap/eventsource/",
		MCPPath,
	}
}

// echo implements Core/echo (RFC 8620 §4): the arguments, returned verbatim. It
// is the one method of urn:ietf:params:jmap:core, and it is the cheapest way for
// a client (or an operator with curl) to prove the request envelope, the auth
// path, and the `using` gate all work.
func echo(_ context.Context, _ ulid.ULID, args json.RawMessage) (json.RawMessage, *MethodError) {
	if len(args) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return args, nil
}

// contextUserID pulls the authenticated user id out of the request context.
// Handlers behind auth.Middleware can rely on ok; a missing id means the
// handler was mounted without the middleware (a wiring bug) or the request is
// unauthenticated — either way it is a 401.
func contextUserID(ctx context.Context) (ulid.ULID, bool) {
	return auth.UserIDFromContext(ctx)
}

// resolveAccount validates the accountId argument against the caller's single
// JMAP account (which IS the mailroom user). An omitted accountId defaults to
// it; a mismatched one is accountNotFound (a client must never reach another
// account, spec §10). The account id string equals the user's ULID.
func resolveAccount(userID ulid.ULID, accountID string) *MethodError {
	if accountID == "" || accountID == userID.String() {
		return nil
	}
	return &MethodError{Type: errAccountNotFound}
}

// withUserTx runs fn inside an RLS-scoped UserTx for the caller and maps any
// store error (other than the ones fn already turned into a MethodError) to a
// serverFail. fn returns a *MethodError for JMAP-level failures it recognizes.
func (a *API) withUserTx(ctx context.Context, userID ulid.ULID, fn func(tx *store.UserTx) *MethodError) *MethodError {
	var mErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		mErr = fn(tx)
		if mErr != nil {
			// Roll back the transaction: a JMAP method error should not commit
			// partial work. Returning a sentinel error triggers ROLLBACK.
			return errMethodRollback
		}
		return nil
	})
	if err != nil && err != errMethodRollback {
		return &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mErr
}

// errMethodRollback is an internal sentinel that rolls back a UserTx after a
// method-level error without surfacing as a server failure.
var errMethodRollback = errSentinel("api: method error rollback")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// writeJSON writes v as an application/json response with the given status.
//
// Every JSON response this server emits is HARDENED here, in one place, because
// every one of them can carry attacker-authored bytes: a subject, a display
// name, an error string echoing a hostile blobId. JSON is not a scripting
// context — but only as long as the browser agrees that the response IS JSON,
// which is exactly what nosniff makes non-negotiable. The rest is defence in
// depth for a response that should never be treated as a document at all
// (spec §11, #39).
func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", jsonCSP)
	h.Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonCSP is the policy on a JMAP response body. A JSON document loads nothing,
// runs nothing, and is framed by nobody — so say so, and mean it even in the
// case where a browser is somehow persuaded to render one.
const jsonCSP = "default-src 'none'; frame-ancestors 'none'; sandbox"

// RequestHandler implements POST /jmap: decode the request object, dispatch the
// method calls, and return {methodResponses, sessionState}.
func (a *API) RequestHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextUserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest,
			newRequestError("notRequest", http.StatusBadRequest, err.Error()))
		return
	}
	if len(req.MethodCalls) > maxCallsInRequest {
		writeJSON(w, http.StatusBadRequest,
			newRequestError("limit", http.StatusBadRequest, "too many method calls"))
		return
	}
	// RFC 8620 §3.3: a `using` list naming a capability we do not advertise is a
	// request-level failure. Without this check a client can call a method for a
	// capability the server never offered and get a straight answer — which is
	// exactly how a client ends up depending on semantics we never promised.
	if reqErr := validateUsing(req.Using); reqErr != nil {
		writeJSON(w, http.StatusBadRequest, reqErr)
		return
	}

	resp := a.dispatch(r.Context(), userID, req)
	writeJSON(w, http.StatusOK, resp)
}

// WithImageProxy turns on the image proxy (ADR-026, #55 increment 3). Zero
// fields take the documented defaults, and MaxBytes's default is the one §7
// measured rather than a guess.
//
// ⚠ ENABLING IT CHANGES WHAT Email/get RETURNS. A remote image's parked value
// becomes a same-origin /jmap/proxy path instead of the sender's URL, so the
// sender's URL never reaches any client. That is deliberate and structural: a
// client that cannot see the real URL cannot be made to fetch it.
func WithImageProxy(cfg ProxyConfig) Option {
	return func(a *API) {
		if !cfg.Enabled {
			a.proxy = nil
			a.remoteAsks = nil
			return
		}
		if cfg.MaxBytes == 0 {
			cfg.MaxBytes = defaultProxyMaxBytes
		}
		if cfg.Timeout == 0 {
			cfg.Timeout = defaultProxyTimeout
		}
		if cfg.PerMessage == 0 {
			cfg.PerMessage = defaultProxyPerMessage
		}
		if cfg.PerMinute == 0 {
			cfg.PerMinute = defaultProxyPerMinute
		}
		if cfg.CacheBytes == 0 {
			cfg.CacheBytes = defaultProxyCacheBytes
		}
		if cfg.CacheTTL == 0 {
			cfg.CacheTTL = defaultProxyCacheTTL
		}
		if cfg.PrefetchMaxAge <= 0 {
			cfg.PrefetchMaxAge = DefaultProxyPrefetchMaxAge
		}
		if cfg.ReferencedCacheBytes == 0 {
			cfg.ReferencedCacheBytes = DefaultProxyReferencedCacheBytes
		}
		a.proxy = newImageProxy(cfg, newProxyEgressClient(cfg))
		a.remoteAsks = newRemoteRequests()
	}
}

// withImageProxyClient is the test seam: the same wiring, with the egress client
// replaced so a test needs no open internet.
func withImageProxyClient(cfg ProxyConfig, client proxyFetcher) Option {
	return func(a *API) {
		WithImageProxy(cfg)(a)
		if a.proxy != nil {
			a.proxy.client = client
		}
	}
}

// WithTrustedAuthservIDs names the RFC 8601 authserv-id(s) whose
// Authentication-Results verdicts this deployment believes for a SECURITY
// decision (#414, config.EnvAuthservIDs).
//
// ⚠ IT AFFECTS EXACTLY ONE THING: whether an affirmative DKIM/DMARC `pass` may
// suppress the `selfSpoof` sender warning. The `authResults` property a reader
// sees, and the classifier's auth_fail signal, both keep reading every header —
// they predate the security use, changing them would move classifier features
// across the whole corpus, and neither is a control an attacker gains anything
// by flipping.
//
// ⚠ UNSET MEANS NOTHING IS BELIEVED, which makes selfSpoof fire MORE rather
// than less. There is no value of this option that makes a spoofing warning
// quietly disappear; that asymmetry is the point.
func WithTrustedAuthservIDs(ids []string) Option {
	return func(a *API) { a.trustedAuthservIDs = ids }
}
