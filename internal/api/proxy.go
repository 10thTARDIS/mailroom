// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/net/publicsuffix"

	"mailroom/internal/egress"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// The image proxy (ADR-026 §3b–§3d, #55 increment 3 step 3).
//
// # What it is for, stated without inflation
//
// It hides the READER'S BROWSER — User-Agent, Accept-Language, TLS fingerprint —
// and replaces the reader's address with the server's wherever the reader is not
// on the server's network. It does NOT hide WHEN a message was opened; only a
// cache does that, and only from the second open (§4 step 4) — or from the
// first, for an allowed sender's images prefetched at arrival (#922,
// proxy_prefetch.go), which tell the sender the delivery time instead.
//
// It is emphatically NOT what stops open-tracking. v1.42.0 did that, and did it
// more completely than a proxy could: a suppressed beacon is never fetched by
// anyone, whereas a proxied one is still fetched, just by the server.
//
// # Why the endpoint takes a message reference and not a URL
//
// `GET /proxy?url=…` is a general-purpose SSRF gadget: whatever the guard
// refuses today, the endpoint's SHAPE invites every future caller to widen it.
// This takes a message id and a token derived from the URL, and resolves the URL
// out of that message's own stored bytes. There is no way to ask it for an
// address that does not already appear in mail delivered to the asking user, and
// RLS scopes "delivered to the asking user" to the session's identity.
//
// The URL is still attacker-chosen — a spammer picks what is in their mail — so
// internal/egress is still mandatory. What this removes is the gadget.
//
// # The token, and why it is a hash rather than an index
//
// ADR-026 §3b proposed "message id plus the image's index in the server's own
// parked list". A hash of the URL is used instead, and it satisfies the same
// requirement more robustly: an index has to mean the same thing to the
// sanitizer that rendered the body and to this endpoint resolving it later, so
// it silently breaks if part ordering, part selection or the sanitizer's idea of
// "remote" ever changes. A URL hash has no ordering to agree about.
//
// It is NOT a secret and does not need to be: the security is "this URL is
// parked in this message for this user", which is checked, not "this token is
// unguessable".
//
// # Fail-closed, everywhere
//
// A refusal is a broken image. It is never a redirect to the sender's URL and
// never a hint the client could use to fetch directly — decision C forbids
// fail-open, because a silent fallback undoes the feature at exactly the moment
// the user is told it is protecting them.

// proxyURLFn rewrites one parked image URL into the reference the client should
// load instead. See the fail-closed note at its call site in sanitize.go: the
// sanitizer assigns its result unconditionally, so returning "" yields a broken
// image rather than the sender's URL.
type proxyURLFn func(rawURL string) string

// ProxyTokenFor is the reference for one URL within one message.
//
// Truncated to 16 bytes: this is a lookup key inside an already-authenticated,
// already-RLS-scoped message, not a capability. 128 bits is far past any
// collision concern for the handful of images in one message, and the endpoint
// verifies the resolved URL rather than trusting the token to be unique.
func ProxyTokenFor(rawURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURL)))
	return hex.EncodeToString(sum[:16])
}

// proxyPath is the URL the client loads for one image.
func proxyPath(accountID, messageID ulid.ULID, token string) string {
	return "/jmap/proxy/" + accountID.String() + "/" + messageID.String() + "/" + token
}

// proxyRewriterFor returns the rewriter the sanitizer uses while rendering one
// message's bodies.
func proxyRewriterFor(accountID, messageID ulid.ULID) proxyURLFn {
	return func(rawURL string) string {
		return proxyPath(accountID, messageID, ProxyTokenFor(rawURL))
	}
}

// ProxyConfig is the endpoint's policy. The zero value is disabled.
type ProxyConfig struct {
	// Enabled turns the whole feature on, INCLUDING the body rewrite. When it is
	// off nothing changes: bodies park the sender's URL exactly as before.
	Enabled bool
	// MaxBytes bounds one fetched image.
	//
	// The default has evidence behind it (ADR-026 §7): across a sample of
	// real mail images the largest was well under 8 MiB, so
	// 8 MiB is many times the largest thing ever observed. It is a
	// backstop against a hostile sender streaming forever, not a tuning knob.
	MaxBytes int64
	// Timeout bounds one fetch, redirects included.
	Timeout time.Duration
	// PerMessage caps how many distinct images one message may cause to be
	// fetched. ADR-026's Consequences require this in the design rather than a
	// follow-up, and decision C removed the release it could have slipped into.
	PerMessage int
	// PerMinute caps fetches per user per minute across all messages.
	PerMinute int
	// CacheBytes bounds the UNREFERENCED part of the per-user image cache —
	// images cached by an ordinary open that no live message keeps (#922). Zero
	// takes the default; NEGATIVE disables caching entirely, and with it the
	// prefetch, which has nowhere to put what it fetches.
	//
	// ⚠ IT DOES NOT BOUND PREFETCHED IMAGES. An image a live message references
	// (app.proxy_image_refs) is never evicted by this cap or by CacheTTL
	// (decided 2026-10-27): its URL may have expired since, so
	// evicting it loses the picture for good. Their growth is bounded instead by
	// MaxBytes per image, PerMessage per message, the allowed-senders rule and
	// PrefetchMaxAge — and made visible by the cache gauges, since nothing
	// evicts them.
	//
	// ⚠ THE CACHE IS ALSO WHAT HIDES WHEN A MESSAGE WAS OPENED: an open served
	// from it tells the sender nothing. Turning it off gives that up; it does
	// not merely cost bandwidth.
	//
	// The default was 256 MiB, sized from ADR-026 §7's ~65 MiB corpus; it is
	// 2 GiB since #922, so the bound on ordinary opens stays generous now that
	// it is the only bound eviction enforces.
	CacheBytes int64
	// CacheTTL evicts an UNREFERENCED entry that has not been READ for this
	// long. Zero takes the default; negative means no age bound. A referenced
	// entry is exempt until its last reference goes, and then ages from its
	// last_used_at like any other.
	CacheTTL time.Duration
	// Prefetch fetches, at arrival, the images of a message whose sender the
	// proxy's own rule allows, so an image behind a signed, expiring URL is
	// cached while the URL still works (#922, ADR-026 amendment of 2026-10-27).
	// Off in the zero value; cmd/mailroom turns it on from
	// MAILROOM_IMAGE_PROXY_PREFETCH (default on). It has no effect when the
	// proxy or the cache is off.
	Prefetch bool
	// PrefetchMaxAge is the arrival window: a message received longer ago than
	// this is never prefetched, so an import or first sync of years of history
	// does not fetch everything at once. Zero takes the default (7 days).
	PrefetchMaxAge time.Duration
	// ReferencedCacheBytes is the ceiling on one user's REFERENCED cache bytes —
	// images kept because a live message shows them (#922 review H1). It is a
	// ceiling on GROWTH, never an eviction bound: once a user's referenced bytes
	// reach it the prefetch fetches nothing more for them (cap_reached) and
	// nothing already kept is evicted, because a kept image's URL may have
	// expired. Opens are unaffected. Zero takes the default (16 GiB); NEGATIVE
	// means no limit, which the operator may opt into.
	ReferencedCacheBytes int64
}

// Defaults, applied by WithImageProxy to any zero field.
const (
	defaultProxyMaxBytes   = 8 << 20
	defaultProxyTimeout    = 20 * time.Second
	defaultProxyPerMessage = 200
	defaultProxyPerMinute  = 600
	defaultProxyCacheBytes = 2 << 30
	// DefaultProxyReferencedCacheBytes is the referenced-bytes ceiling (#922
	// review H1): 16 GiB per user, far above any whole
	// proxied corpus as ADR-026 §7 measured it.
	DefaultProxyReferencedCacheBytes = 16 << 30
	defaultProxyCacheTTL             = 30 * 24 * time.Hour
	// DefaultProxyPrefetchMaxAge is the prefetch's arrival window (#922). It is
	// NOT a measurement of how long signed image URLs stay valid — live mail is
	// prefetched within seconds of arriving, whatever the window. It exists so
	// a first sync or a catch-up after downtime still prefetches recent mail
	// while a backfill of years of history does not contact every sender in it
	// at once. Exported for internal/config's default.
	DefaultProxyPrefetchMaxAge = 7 * 24 * time.Hour
)

// proxyFetcher is the egress seam, so tests do not need the open internet.
type proxyFetcher interface {
	Do(req *http.Request) (*http.Response, error)
}

// imageProxy holds the endpoint's state.
type imageProxy struct {
	cfg     ProxyConfig
	client  proxyFetcher
	mu      sync.Mutex
	perUser map[ulid.ULID]*proxyWindow

	// Test seams, nil in production. resolve replaces resolveProxyTarget (which
	// needs a store and a blob store) and cacheHit replaces serveProxyCacheHit,
	// so a handler test can drive each refusal path without a database.
	resolve  func(ctx context.Context, userID, messageID ulid.ULID, token string) (string, string)
	cacheHit func(w http.ResponseWriter, r *http.Request, userID ulid.ULID, target string) bool

	// prefetchBudget bounds one prefetch job's wall-clock time (see
	// proxy_prefetch.go); zero takes defaultPrefetchBudget. A field so a test
	// can make the budget run out.
	prefetchBudget time.Duration
	// capWarned is when each user's cap_reached WARN was last written, so it
	// is written at most once an hour per user (guarded by mu).
	capWarned map[ulid.ULID]time.Time
}

// proxyWindow is one user's fixed-window fetch counter.
type proxyWindow struct {
	windowStart time.Time
	count       int
}

func newImageProxy(cfg ProxyConfig, client proxyFetcher) *imageProxy {
	return &imageProxy{cfg: cfg, client: client, perUser: map[ulid.ULID]*proxyWindow{},
		capWarned: map[ulid.ULID]time.Time{}}
}

// allow reports whether this user may make one more fetch now.
func (p *imageProxy) allow(user ulid.ULID, now time.Time) bool {
	return p.admit(user, now, p.cfg.PerMinute)
}

// allowBackground is allow for the prefetch (#922): the SAME per-user window,
// so a prefetch and the reader's opens together never exceed PerMinute — but
// the prefetch is admitted only while the window is below HALF of it. The
// other half is kept for the reader: a burst of arriving mail must not make the
// message they are opening right now answer 429.
func (p *imageProxy) allowBackground(user ulid.ULID, now time.Time) bool {
	limit := p.cfg.PerMinute / 2
	if p.cfg.PerMinute > 0 && limit < 1 {
		limit = 1
	}
	return p.admit(user, now, limit)
}

// admit is the fixed-window counter behind allow and allowBackground: one more
// fetch is admitted while the window holds fewer than limit. PerMinute <= 0
// disables limiting for both.
func (p *imageProxy) admit(user ulid.ULID, now time.Time, limit int) bool {
	if p.cfg.PerMinute <= 0 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.perUser[user]
	if w == nil || now.Sub(w.windowStart) >= time.Minute {
		p.perUser[user] = &proxyWindow{windowStart: now, count: 1}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

// windowReset is when this user's current rate window ends: the earliest a
// deferred prefetch can usefully run again.
func (p *imageProxy) windowReset(user ulid.ULID, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w := p.perUser[user]; w != nil && now.Sub(w.windowStart) < time.Minute {
		return w.windowStart.Add(time.Minute)
	}
	return now
}

// Refusal reasons (#674). Each is the label of exactly one code path below, and
// the label set is closed: they feed mailroom_proxy_refusals_total, so a new one
// is a new time series, not a free-form string.
//
// ⚠ THEY NEVER REACH THE CLIENT. Every response is still the one status and text
// it always was; folding "no such message" and "sender not allowed" into one 404
// is what keeps the endpoint from being an oracle, and these labels exist only in
// the operator's metrics and log.
const (
	reasonDisabled     = "disabled"         // proxy not configured (nil)
	reasonUnauthorized = "unauthorized"     // no session identity on the request
	reasonAccount      = "account_mismatch" // path account is not the session's
	reasonBadMessage   = "bad_message_id"   // path message id is not a ULID
	reasonBadToken     = "bad_token"        // path token fails the shape check

	// Authorization (resolveProxyTarget). All answer 404.
	reasonUnavailable  = "resolve_unavailable" // no raw message source wired
	reasonMessageMiss  = "resolve_message"     // message lookup failed or RLS hid it
	reasonNotAllowed   = "resolve_not_allowed" // sender not allowed, no one-off ask
	reasonReadRaw      = "resolve_read"        // stored message bytes unreadable
	reasonParse        = "resolve_parse"       // stored message did not parse
	reasonMessageCap   = "resolve_message_cap" // more images than PerMessage allows
	reasonTokenMissing = "resolve_token"       // no parked image has this token

	reasonRateLimited = "rate_limited"
	reasonBadRequest  = "bad_request" // http.NewRequest rejected the stored URL

	// Fetch failures.
	reasonEgressRefused   = "egress_refused"     // guard refused the address
	reasonSchemeRefused   = "scheme_refused"     // guard refused the URL scheme
	reasonTooManyRedirect = "too_many_redirects" // redirect chain over the hop limit
	reasonUpstreamTimeout = "upstream_timeout"   // fetch deadline exceeded
	reasonCancelled       = "client_cancelled"   // the reader's request was cancelled
	reasonUpstreamError   = "upstream_error"     // any other network failure
	reasonUpstreamStatus  = "upstream_status"    // sender answered non-2xx
	reasonContentType     = "content_type"       // type not on the allowlist (415)
	reasonTooLarge        = "too_large"          // body passed MaxBytes mid-stream
	reasonBodyAborted     = "body_aborted"       // body copy ended early for another reason
	// reasonPortRefused is the PREFETCH's own egress-style refusal (#922
	// review): a URL naming a port other than its scheme's default. The open
	// path does not apply it — a reader who asked for the image gets what the
	// guard allows — but a fetch nobody asked for yet stays on 443/80.
	reasonPortRefused = "port_refused"
)

// ProxyRefusalReasons lists every reason label above, so a metrics sink can
// export each one at zero from boot. A labelled counter is otherwise absent
// until its first increment, and an absent series reads the same as a proxy
// that was never wired: "no refusals" and "not measured" must look different.
// TestProxyRefusalReasonsListsEveryConstant keeps this list complete.
func ProxyRefusalReasons() []string {
	return []string{
		reasonDisabled, reasonUnauthorized, reasonAccount, reasonBadMessage, reasonBadToken,
		reasonUnavailable, reasonMessageMiss, reasonNotAllowed, reasonReadRaw, reasonParse,
		reasonMessageCap, reasonTokenMissing, reasonRateLimited, reasonBadRequest,
		reasonEgressRefused, reasonSchemeRefused, reasonTooManyRedirect, reasonUpstreamTimeout,
		reasonCancelled, reasonUpstreamError, reasonUpstreamStatus, reasonContentType,
		reasonTooLarge, reasonBodyAborted, reasonPortRefused,
	}
}

// Fetch results for ProxyOutcome.Result.
const (
	ProxyServed  = "served"  // fetched from the sender and relayed
	ProxyCached  = "cached"  // served from the per-user cache, no outbound request
	ProxyRefused = "refused" // anything else; Reason says which
	// Prefetch-only results (#922 review). Neither is logged by noteProxy.
	ProxyCapReached = "cap_reached" // a prefetch run fetched nothing: the referenced-bytes ceiling is reached
	ProxyEmpty      = "empty"       // the sender answered 200 with no bytes: nothing to keep
)

// ProxyOutcome is what one proxy request — or one prefetched image — came to.
// It is made of closed labels and numbers plus, for a FAILED FETCH only, the
// sender's host: no URL path or query, no message id, no user id, no mail
// content, so neither the observer nor the log line can leak what it is never
// given.
type ProxyOutcome struct {
	Result string // ProxyServed, ProxyCached or ProxyRefused
	Reason string // set when Result is ProxyRefused
	// Status is the status the sender's server answered with (upstream_status
	// only); zero otherwise.
	Status int
	// ContentType is the sender-declared media type, parameters stripped and
	// length-capped (content_type only); empty otherwise.
	ContentType string
	// Host is the image URL's host, lowercased and bounded — set only when an
	// outbound fetch was attempted and failed (#922, a design decision).
	// noteProxy logs it only for the reasons in hostLoggedReasons, so an
	// authorization refusal cannot carry it even if a caller set it. It is
	// NEVER a metric label.
	Host string
	// Prefetch marks an outcome of the arrival prefetch rather than of a
	// reader's request (#922). The observer counts those apart, so the
	// request-level refusal rate stays a statement about reads.
	Prefetch bool
}

// hostLoggedReasons are the refusals after which the log line carries the
// sender's HOST (#922, a design decision): the ones where an outbound fetch
// was attempted and failed, so the host is the thing an operator needs to tell
// "one sender's signed URLs expired" from "the proxy is broken".
//
// ⚠ AUTHORIZATION AND REQUEST-SHAPE REFUSALS ARE ABSENT ON PURPOSE. They answer
// "may this reader have this image", and a host in those lines would say which
// sender's mail a refused request was probing. rate_limited is absent too: no
// fetch was attempted. Only the HOST is ever logged, never the path or query —
// those carry the recipient's token.
var hostLoggedReasons = map[string]bool{
	reasonBadRequest:      true,
	reasonEgressRefused:   true,
	reasonSchemeRefused:   true,
	reasonTooManyRedirect: true,
	reasonUpstreamTimeout: true,
	reasonCancelled:       true,
	reasonUpstreamError:   true,
	reasonUpstreamStatus:  true,
	reasonContentType:     true,
	reasonTooLarge:        true,
	reasonBodyAborted:     true,
	reasonPortRefused:     true,
}

// loggableHost is what a failure log line says about the sender: the image
// URL's REGISTRABLE DOMAIN (eTLD+1, by the public suffix list) — never the full
// hostname, never a port, userinfo, path or query. "" when it does not parse.
//
// ⚠ REGISTRABLE DOMAIN, NOT HOSTNAME (#922 review M2, and its re-review). A wildcard-DNS tracker
// can put the recipient's token in a subdomain label
// (`u-8f3a91.img.tracker.example`), and the full hostname would copy it into
// the log just as the path would. The registrable domain names the sender's
// organisation, which is what an operator needs, and cannot carry a token.
//
// Under a PRIVATE-section suffix (github.io, workers.dev, a cloud provider's
// bucket domain) eTLD+1 is a customer's own label — `token.github.io` — so the
// line names the ICANN-registrable domain instead (`github.io`), which for a
// wildcard private rule also drops the wildcard's label (organisationDomain).
//
// An IP literal is logged as "ip-literal": a sender holding an IPv6 /64 could
// encode a token in the address bits. That includes the IPv4 spellings Go's
// resolver accepts and netip does not ("0x7f.1", "2130706433", "127.1").
// A host the list cannot reduce (a bare public suffix, a single label) is
// "unregistrable".
//
// Bounded and reduced to printable ASCII, because it is sender-chosen text.
func loggableHost(rawURL string) string {
	h := strings.TrimSuffix(urlHost(rawURL), ".")
	if h == "" {
		return ""
	}
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil || looseIPv4(h) {
		return "ip-literal"
	}
	h = organisationDomain(h)
	if h == "" {
		return "unregistrable"
	}
	if len(h) > 253 {
		h = h[:253]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x21 || r > 0x7e {
			return '?'
		}
		return r
	}, h)
}

// WithProxyObserver installs the sink for ProxyOutcome and the logger refusals
// are written to (#674). It is read-only: it cannot change a response. A nil fn
// only skips the counters; a nil logger uses slog.Default().
func WithProxyObserver(logger *slog.Logger, fn func(ProxyOutcome)) Option {
	return func(a *API) { a.onProxy, a.proxyLog = fn, logger }
}

// noteProxy records one outcome: it feeds the observer and, for a refusal, writes
// ONE INFO line.
//
// ⚠ THE LINE CARRIES THE REASON, THE TWO ATTRIBUTES ABOVE AND — FOR A FAILED
// FETCH ONLY — THE SENDER'S HOST, NOTHING ELSE. An image URL's path and query
// carry the sender's tracking identifier for this recipient, so they are
// personal data, and so is the message id (it locates the message). The host
// was added by design decision (#922) because a failure that cannot be
// attributed to a sender cannot be acted on; it is logged only for
// hostLoggedReasons. Do not add the URL, the path, the query or any id here;
// TestProxyRefusalLogNeverCarriesTheURL and
// TestProxyAuthorizationRefusalsNeverLogAHost are what notice.
func (a *API) noteProxy(ctx context.Context, o ProxyOutcome) {
	if a.onProxy != nil {
		a.onProxy(o)
	}
	if o.Result != ProxyRefused {
		return
	}
	logger := a.proxyLogger()
	attrs := []any{"reason", o.Reason}
	if o.Status != 0 {
		attrs = append(attrs, "upstream_status", o.Status)
	}
	if o.ContentType != "" {
		attrs = append(attrs, "content_type", o.ContentType)
	}
	if o.Host != "" && hostLoggedReasons[o.Reason] {
		attrs = append(attrs, "host", o.Host)
	}
	if o.Prefetch {
		attrs = append(attrs, "prefetch", true)
	}
	logger.InfoContext(ctx, "image proxy refused", attrs...)
}

// proxyLogger is the logger proxy lines go to: WithProxyObserver's, or the
// default.
func (a *API) proxyLogger() *slog.Logger {
	if a.proxyLog != nil {
		return a.proxyLog
	}
	return slog.Default()
}

// refuse answers with a status and records why. The response is exactly what
// http.Error always wrote here.
func (a *API) refuse(w http.ResponseWriter, r *http.Request, status int, msg string, o ProxyOutcome) {
	o.Result = ProxyRefused
	a.noteProxy(r.Context(), o)
	http.Error(w, msg, status)
}

// ProxyHandler implements GET /jmap/proxy/{accountId}/{messageId}/{token}.
//
// ⚠ EVERY REFUSAL IS A STATUS, NEVER A REDIRECT AND NEVER THE URL. A client that
// learned the sender's URL from a failure could fetch it directly, which is the
// fail-open decision C forbids.
func (a *API) ProxyHandler(w http.ResponseWriter, r *http.Request) {
	// ⚠ nil IS the disabled state, and there is deliberately no second check.
	// WithImageProxy sets a.proxy = nil for a disabled config, so a
	// `!a.proxy.cfg.Enabled` clause here would be unreachable — and probing
	// confirmed it: deleting it left every test green, because no test could
	// construct an API that was configured AND disabled. Dead code in an
	// authorization path reads as defence and provides none.
	if a.proxy == nil {
		a.refuse(w, r, http.StatusNotFound, "image proxy is not enabled", ProxyOutcome{Reason: reasonDisabled})
		return
	}
	userID, ok := contextUserID(r.Context())
	if !ok {
		a.refuse(w, r, http.StatusUnauthorized, "unauthenticated", ProxyOutcome{Reason: reasonUnauthorized})
		return
	}
	a.serveProxy(w, r, userID)
}

// serveProxy is everything after "who is asking", split out so a test can supply
// the identity that only auth.Middleware may put in a context.
func (a *API) serveProxy(w http.ResponseWriter, r *http.Request, userID ulid.ULID) {
	// The account in the path is checked against the SESSION's identity, never
	// trusted as the identity (spec §10).
	if !accountMatches(userID, r.PathValue("accountId")) {
		a.refuse(w, r, http.StatusNotFound, "not found", ProxyOutcome{Reason: reasonAccount})
		return
	}
	messageID, err := ulid.Parse(r.PathValue("messageId"))
	if err != nil {
		a.refuse(w, r, http.StatusNotFound, "not found", ProxyOutcome{Reason: reasonBadMessage})
		return
	}
	token := strings.ToLower(r.PathValue("token"))
	if !isProxyToken(token) {
		a.refuse(w, r, http.StatusNotFound, "not found", ProxyOutcome{Reason: reasonBadToken})
		return
	}

	// ⚠ AUTHORIZATION FIRST, CACHE SECOND. The cache is keyed by URL hash, which
	// is not user-scoped in its own right — resolveProxyTarget is what proves
	// this reader may have this image, and serving a cache hit before asking
	// would turn the cache into a way around the check.
	resolve := a.resolveProxyTarget
	if a.proxy.resolve != nil {
		resolve = a.proxy.resolve
	}
	target, why := resolve(r.Context(), userID, messageID, token)
	if why != "" {
		// One status for every "you may not have this", so the endpoint does not
		// become an oracle distinguishing "no such message" from "sender not
		// allowed" from "no such image". The distinction lives in the reason
		// label only.
		a.refuse(w, r, http.StatusNotFound, "not found", ProxyOutcome{Reason: why})
		return
	}

	// ⚠ A CACHE HIT IS THE WHOLE POINT OF THE CACHE: it is the only case in
	// which opening a message does NOT tell the sender it was opened. It is also
	// checked before the rate limiter, because a hit makes no outbound request
	// and there is nothing to limit — rate-limiting a hit would make re-reading
	// an old thread fail for no benefit.
	cacheHit := a.serveProxyCacheHit
	if a.proxy.cacheHit != nil {
		cacheHit = a.proxy.cacheHit
	}
	if cacheHit(w, r, userID, target) {
		a.noteProxy(r.Context(), ProxyOutcome{Result: ProxyCached})
		return
	}

	if !a.proxy.allow(userID, time.Now()) {
		a.refuse(w, r, http.StatusTooManyRequests, "too many image fetches", ProxyOutcome{Reason: reasonRateLimited})
		return
	}

	resp, ct, fail := a.proxy.openProxyImage(r.Context(), target)
	if fail != nil {
		a.refuse(w, r, fail.status, fail.msg, fail.outcome)
		return
	}
	defer drainAndClose(resp)

	writeProxyImageHeaders(w, ct)

	// ⚠ TEE INTO THE CACHE WHILE SERVING, and let a cache failure be invisible to
	// the reader. The image is already fetched; refusing to serve it because the
	// cache write failed would turn a storage problem into a broken picture. The
	// cost of a failed write is one extra fetch next time, which is the old
	// behaviour, not a new failure.
	//
	// The body is already capped by internal/egress; a read past the cap ends the
	// response mid-image, which the browser renders as a broken image.
	body := io.Reader(resp.Body)
	var buf *bytes.Buffer
	if a.proxy.cacheEnabled() && a.blobs != nil {
		buf = &bytes.Buffer{}
		body = io.TeeReader(resp.Body, buf)
	}
	w.WriteHeader(http.StatusOK)
	if _, cerr := io.Copy(w, body); cerr != nil {
		// The reader went away mid-image. Whatever reached buf is a PARTIAL
		// image, and caching a truncated picture would serve that truncation
		// forever — a silent corruption with no way to notice. Drop it.
		//
		// The 200 and part of the body are already sent, so this is only
		// recorded, never answered: a body past MaxBytes is too_large, anything
		// else (the reader leaving, the sender's connection dropping) is
		// body_aborted.
		a.noteProxy(r.Context(), ProxyOutcome{Result: ProxyRefused, Reason: bodyFailureReason(cerr), Host: loggableHost(target)})
		return
	}
	a.noteProxy(r.Context(), ProxyOutcome{Result: ProxyServed})
	if buf != nil {
		_ = a.storeProxyImage(r.Context(), userID, target, ct, buf.Bytes(), nil)
	}
}

// organisationDomain is the domain a failure line names for host: eTLD+1 when
// the public suffix is ICANN's or the list's default rule, and the
// ICANN-registrable domain of the suffix when the suffix is a PRIVATE-section
// entry. "" when nothing registrable remains.
func organisationDomain(host string) string {
	suffix, icann := publicsuffix.PublicSuffix(host)
	if !icann && strings.Contains(suffix, ".") {
		// A private suffix: walk its labels rightward until what remains is an
		// ICANN suffix (or the default single-label rule), then keep one more
		// label of the HOST — the operator of the private suffix.
		t := suffix
		for {
			i := strings.IndexByte(t, '.')
			if i < 0 {
				break
			}
			t = t[i+1:]
			s2, ic2 := publicsuffix.PublicSuffix(t)
			if s2 == t && (ic2 || !strings.Contains(t, ".")) {
				break
			}
		}
		labels := strings.Split(host, ".")
		keep := strings.Count(t, ".") + 2
		if keep > len(labels) {
			return ""
		}
		return strings.Join(labels[len(labels)-keep:], ".")
	}
	reg, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return reg
}

// looseIPv4 reports a host that is an IPv4 address in one of the spellings
// inet_aton (and so Go's resolver on some platforms) accepts but netip rejects:
// one to four dot-separated numbers, each decimal, 0x-hex or 0-octal.
func looseIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.ParseUint(p, 0, 32); err != nil {
			return false
		}
	}
	return true
}

// proxyFetchFailure is a refusal from openProxyImage: the outcome to record, and
// the status and text the HTTP handler answers with. The prefetch records the
// outcome and has no response to write.
type proxyFetchFailure struct {
	status  int
	msg     string
	outcome ProxyOutcome
}

// openProxyImage makes THE one guarded fetch of an image and checks the parts of
// the answer that are known before the body is read: the transport outcome, the
// sender's status, and the Content-Type allowlist. It returns the response
// (the caller drains and closes it) and the type to serve, or a failure.
//
// ⚠ THE OPEN PATH AND THE PREFETCH BOTH COME THROUGH HERE (#922), and that is
// the whole safety argument for the prefetch: the same egress client (guard,
// schemes, timeout, redirect limit, MaxBytes on the body), the same request,
// the same status rule and the same type allowlist. A prefetch therefore cannot
// fetch or keep anything an open would refuse. A second fetcher "for the
// background" would be the place those drift apart.
//
// Every failure here happens after authorization and is a fetch failure, so its
// outcome carries the sender's host (hostLoggedReasons) — never the URL.
func (p *imageProxy) openProxyImage(ctx context.Context, target string) (*http.Response, string, *proxyFetchFailure) {
	host := loggableHost(target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", &proxyFetchFailure{http.StatusBadGateway, "bad image reference",
			ProxyOutcome{Reason: reasonBadRequest, Host: host}}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// A refusal by the guard and a network failure are both a broken image
		// to the reader, but they are counted and logged apart (#674): a guard
		// that never reports refusing anything is a guard nobody has evidence
		// works. The log line carries the host (#922), never the URL.
		status := http.StatusBadGateway
		if errors.Is(err, egress.ErrBlockedAddress) || errors.Is(err, egress.ErrSchemeNotPermitted) ||
			errors.Is(err, errPrefetchPort) {
			status = http.StatusForbidden
		}
		return nil, "", &proxyFetchFailure{status, "image could not be fetched",
			ProxyOutcome{Reason: fetchFailureReason(ctx, err), Host: host}}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// ⚠ THIS IS AN ORDINARY CASE, NOT AN ANOMALY. ADR-026 §7 measured 9 of
		// 120 sampled fetches returning 403, all on one host whose URLs carry an
		// expiry parameter — a signed URL that has aged out. The reader gets a
		// broken image, which is the correct and visible outcome. It is also the
		// case the prefetch exists to get ahead of (#922).
		drainAndClose(resp)
		return nil, "", &proxyFetchFailure{http.StatusBadGateway, "the sender's server refused the image",
			ProxyOutcome{Reason: reasonUpstreamStatus, Status: resp.StatusCode, Host: host}}
	}
	ct := proxyImageContentType(resp.Header.Get("Content-Type"))
	if ct == "" {
		// Not an image type we will re-serve. Refusing beats relaying an
		// unknown type into an <img>, and beats echoing the sender's own
		// Content-Type, which is the mistake blob.go already refuses to make.
		declared := resp.Header.Get("Content-Type")
		drainAndClose(resp)
		return nil, "", &proxyFetchFailure{http.StatusUnsupportedMediaType, "not an image",
			ProxyOutcome{Reason: reasonContentType, ContentType: loggableMediaType(declared), Host: host}}
	}
	return resp, ct, nil
}

// drainAndClose finishes with a response body.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// bodyFailureReason names why reading an image body ended early: past MaxBytes
// is too_large, anything else body_aborted.
func bodyFailureReason(err error) string {
	if errors.Is(err, egress.ErrTooLarge) {
		return reasonTooLarge
	}
	return reasonBodyAborted
}

// fetchFailureReason names why Do failed. Order matters: the guard's sentinels
// are tested first because they are the evidence the guard works at all, and a
// cancelled reader is tested before a timeout so a navigated-away page is not
// counted as a slow sender.
func fetchFailureReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, errPrefetchPort):
		return reasonPortRefused
	case errors.Is(err, egress.ErrBlockedAddress):
		return reasonEgressRefused
	case errors.Is(err, egress.ErrSchemeNotPermitted):
		return reasonSchemeRefused
	case errors.Is(err, egress.ErrTooManyRedirects):
		return reasonTooManyRedirect
	case errors.Is(ctx.Err(), context.Canceled):
		return reasonCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return reasonUpstreamTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return reasonUpstreamTimeout
	}
	return reasonUpstreamError
}

// loggableMediaType reduces a sender-declared Content-Type to a short, plain
// media type for the log: parameters dropped, lower-cased, length-capped, and
// anything that is not printable ASCII replaced. It is sender-controlled text, so
// it is bounded before it reaches a log line.
func loggableMediaType(declared string) string {
	t := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if t == "" {
		return "(none)"
	}
	if len(t) > 64 {
		t = t[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x21 || r > 0x7e {
			return '?'
		}
		return r
	}, t)
}

// resolveProxyTarget answers "may this user cause a fetch of this image, and
// what is its URL". It is the whole authorisation decision.
//
// It returns the URL, or "" and a refusal reason (one of the resolve_* labels).
// The reason is for the operator's counters and never leaves the server.
func (a *API) resolveProxyTarget(
	ctx context.Context, userID, messageID ulid.ULID, token string,
) (string, string) {
	if a.raw == nil {
		return "", reasonUnavailable
	}
	var (
		ref    store.MessageAuthRef
		allows []store.RemoteAllow
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		// RLS scopes this: another user's message id is simply not found, which
		// is the right answer rather than a check to remember.
		if ref, e = tx.MessageAuthRefByID(ctx, messageID); e != nil {
			return e
		}
		allows, e = tx.ListRemoteAllows(ctx)
		return e
	}); err != nil {
		return "", reasonMessageMiss
	}

	// §3c: fetch only what the client would have loaded anyway, using the
	// PROXY's stricter rule — not store.RemoteAllowsMatch, which deliberately
	// mirrors the client and is the wrong question here.
	// ⚠ TWO WAYS TO BE ALLOWED, AND §3c REQUIRES BOTH. A remembered grant is the
	// first; the second is "the user has asked for THIS message", which is what a
	// one-off "Show remote content" click means. Implementing only the grant left
	// the ordinary one-off path loading nothing at all — the browser asked the
	// server, and the server had no idea the user had clicked. CI caught it; see
	// remote_request.go.
	if !store.ProxyRemoteAllowsMatch(ref.FromAddress, allows) &&
		!a.remoteAsks.asked(userID, messageID, time.Now()) {
		return "", reasonNotAllowed
	}

	raw, err := a.raw.ReadRaw(ctx, userID, ref.ContentHash)
	if err != nil {
		return "", reasonReadRaw
	}
	msg, err := mime.ParseBytes(raw)
	if err != nil {
		return "", reasonParse
	}

	urls, capped := proxyEligibleImages(msg, a.proxy.cfg.PerMessage)
	for _, u := range urls {
		if ProxyTokenFor(u) == token {
			return u, ""
		}
	}
	// The per-message cap counts parked images, so a message carrying thousands
	// cannot be used to make the server fetch thousands of times: an image past
	// the cap is refused as such rather than as missing.
	if capped {
		return "", reasonMessageCap
	}
	return "", reasonTokenMissing
}

// proxyEligibleImages is the list of image URLs the proxy will serve for one
// message, in document order: every remote image the SHIPPED sanitizer parks
// (MeasureHTMLRemoteImages — not a second parser), beacons excluded, cut off at
// perMessage (<= 0: no cap). capped reports that the cut removed something.
//
// ⚠ THE OPEN PATH AND THE PREFETCH BOTH USE IT (#922), so "an image the
// prefetch may fetch" and "an image an open may fetch" are one list by
// construction rather than two loops that agree today.
func proxyEligibleImages(msg *mime.Message, perMessage int) (urls []string, capped bool) {
	seen := 0
	for _, part := range msg.Parts {
		if part.ContentType != "text/html" {
			continue
		}
		_, imgs := MeasureHTMLRemoteImages(string(part.Data))
		for _, img := range imgs {
			// §3d, defence in depth: a beacon never receives a token, so the open
			// path cannot normally reach one — but a guessed token must not be
			// able to reach one either, and the prefetch must never fetch one.
			if IsTrackingPixel(img) {
				continue
			}
			seen++
			if perMessage > 0 && seen > perMessage {
				return urls, true
			}
			urls = append(urls, img.URL)
		}
	}
	return urls, false
}

// ProxyEligibleImages is proxyEligibleImages for mailroom-admin proxy-report's
// prefetch section (#922): the report counts the list the prefetch would walk,
// not a copy of it.
func ProxyEligibleImages(msg *mime.Message, perMessage int) ([]string, bool) {
	return proxyEligibleImages(msg, perMessage)
}

// ProxyURLHash is the cache key for an image URL (proxyURLHash), exported for
// the report's "already cached" count.
func ProxyURLHash(rawURL string) []byte { return proxyURLHash(rawURL) }

// DefaultProxyPerMessage is the per-message image cap the server applies when
// none is configured, exported so the report applies the same one.
const DefaultProxyPerMessage = defaultProxyPerMessage

// proxyImageContentType maps a fetched Content-Type onto one we will re-serve,
// or "" to refuse.
//
// ⚠ IT IS AN ALLOWLIST AND IT DOES NOT INCLUDE SVG. An SVG is a scripting
// context wearing an image's costume; the sanitizer already refuses
// data:image/svg+xml for the same reason. Serving one here from a CSP'd,
// sandboxed, nosniff response would probably be inert, but "probably inert" is
// not a reason to carry attacker-authored markup when the alternative is a
// broken image.
func proxyImageContentType(declared string) string {
	t := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return t
	default:
		return ""
	}
}

// newProxyEgressClient builds the guarded client the endpoint fetches through.
func newProxyEgressClient(cfg ProxyConfig) *egress.Client {
	return egress.New(egress.Policy{
		// ⚠ BOTH SCHEMES — decision A, and confined to this caller. webpush
		// takes the default https-only policy; permitted schemes come from the
		// caller precisely so these two can differ.
		Schemes:  []string{"https", "http"},
		Timeout:  cfg.Timeout,
		MaxBytes: cfg.MaxBytes,
		Exempt:   proxyEgressExempt,
	})
}

// proxyEgressExempt is nil in every normal build: the image proxy refuses
// exactly what internal/egress refuses. It is assigned only by
// proxy_e2e_private.go, which compiles under the e2eprivateimages tag (the
// proxied-images CI lane's image). TestProxyEgressHasNoExemptionInShippedBuild
// pins the untagged value.
var proxyEgressExempt func(netip.Addr) bool

// isProxyToken reports whether s has the shape ProxyTokenFor produces: exactly
// 32 lowercase hex characters.
//
// ⚠ IT IS A SEPARATE FUNCTION SO IT CAN BE TESTED AT ALL. Inline in the handler
// it was unreachable by any unit test: an API with no store refuses every
// request at the message lookup, so deleting the check left the handler tests
// green. Probing found that; the shape rule now has a test that can observe it.
//
// It is a cheap early refusal, not a security boundary — the boundary is "this
// URL is parked in this message for this user", which resolveProxyTarget checks.
func isProxyToken(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// cacheEnabled reports whether images are cached. A negative CacheBytes turns it
// off, which gives up the only protection against the sender learning WHEN a
// message was re-opened — see the field's comment.
func (p *imageProxy) cacheEnabled() bool { return p.cfg.CacheBytes >= 0 }

// proxyURLHash is the cache key: the SHA-256 of the image URL, full width.
//
// ⚠ FULL WIDTH, unlike ProxyTokenFor's truncation, and for a different reason.
// The token is a lookup inside one already-authorized message, where a collision
// costs nothing because the URL is re-derived and compared. This keys a STORE:
// a collision here would serve one image's bytes for another's URL.
func proxyURLHash(rawURL string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURL)))
	return sum[:]
}

// serveProxyCacheHit serves a cached image if there is one, reporting whether it
// did.
//
// ⚠ A CACHE FAILURE IS A MISS, NEVER AN ERROR TO THE READER. Every failure here
// falls through to the ordinary fetch, which is the behaviour without a cache at
// all. The one thing it must never do is serve something it is not sure about.
func (a *API) serveProxyCacheHit(w http.ResponseWriter, r *http.Request, userID ulid.ULID, target string) bool {
	if !a.proxy.cacheEnabled() || a.blobs == nil {
		return false
	}
	var (
		entry store.ProxyCacheEntry
		found bool
	)
	if err := a.store.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
		var e error
		entry, found, e = tx.LookupProxyImage(r.Context(), proxyURLHash(target))
		return e
	}); err != nil || !found {
		return false
	}
	// The stored type is re-checked rather than trusted. An entry written when
	// the allowlist was wider must not keep being served after it narrowed —
	// the cache would otherwise pin an old security decision in place.
	ct := proxyImageContentType(entry.ContentType)
	if ct == "" {
		return false
	}
	rc, err := a.blobs.OpenRaw(r.Context(), userID, entry.ContentHash)
	if err != nil {
		// The row survived but the bytes did not. Fall through and re-fetch:
		// that is exactly the recovery a derived, disposable cache is for.
		return false
	}
	defer func() { _ = rc.Close() }()

	writeProxyImageHeaders(w, ct)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return true
}

// storeProxyImage records a freshly fetched image and evicts down to the bounds.
// refFor, when set, is the message the image is being prefetched for: the
// reference is written in the SAME transaction as the cache row, so the
// eviction pass that follows can never see the new entry unreferenced (#922).
//
// ⚠ THE OPEN PATH IGNORES THE ERROR, deliberately: the reader has already been
// served. A cache write that fails costs one extra fetch next time, which is
// the pre-cache behaviour — turning it into a visible failure would make a
// storage problem look like a broken picture. The prefetch returns it, so the
// job retries.
//
// It runs on the caller's context. A reader who navigates away mid-write leaves
// no row, which is the same "one extra fetch next time".
func (a *API) storeProxyImage(ctx context.Context, userID ulid.ULID, target, contentType string, body []byte, refFor *ulid.ULID) error {
	if len(body) == 0 {
		return nil
	}
	stored, err := a.blobs.PutRaw(ctx, userID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	var split store.ProxyCacheSplit
	err = a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// ⚠ THE CACHE LOCK FIRST, before any statement below locks a row: the
		// eviction at the end takes it too, and taking it only there would
		// deadlock against a prefetch that holds it and wants these rows
		// (LockProxyCache).
		if err := tx.LockProxyCache(ctx); err != nil {
			return err
		}
		// ⚠ Kind MATTERS HERE AND NOWHERE ELSE IN THIS FILE. Without it the row
		// defaults to 'mail' and a stranger's image starts counting against the
		// quota that bounds the user's own mail (migration 00038).
		if err := tx.InsertBlobMeta(ctx, store.BlobMeta{
			ContentHash: stored.ContentHash,
			SizePlain:   stored.SizePlain,
			SizeStored:  stored.SizeStored,
			DEKID:       stored.DEKID,
			EnvelopeVer: 1,
			Kind:        store.BlobKindProxyCache,
		}); err != nil {
			return err
		}
		urlHash := proxyURLHash(target)
		if err := tx.PutProxyImage(ctx, store.ProxyCacheEntry{
			URLHash:     urlHash,
			ContentHash: stored.ContentHash,
			ContentType: contentType,
			SizeBytes:   stored.SizePlain,
		}); err != nil {
			return err
		}
		if refFor != nil {
			if err := tx.AddProxyImageRef(ctx, urlHash, *refFor); err != nil {
				return err
			}
		}
		// Evict in the SAME transaction, and delete the files before it commits.
		// That ordering is the race proof blobreclaim.go spells out: the row
		// delete is invisible to a concurrent writer until commit, so the window
		// in which a re-fetch could re-Put the bytes opens only AFTER the file is
		// gone, never during.
		reclaimed, err := tx.EvictProxyImages(ctx, a.proxy.cfg.CacheBytes, a.proxy.cfg.CacheTTL)
		if err != nil {
			return err
		}
		for _, h := range reclaimed {
			if derr := a.blobs.DeleteRaw(ctx, userID, h); derr != nil {
				// Abort rather than commit a row-less file: a failure here means
				// "retry", never a file that outlives every reference to it.
				return derr
			}
		}
		// The gauges are read here, after the eviction pass that already walked
		// this user's cache, and not on any cache hit.
		if a.onProxyCache != nil {
			split, err = tx.ProxyCacheBreakdown(ctx)
		}
		return err
	})
	if err != nil {
		// A file written before a transaction that failed may now have no
		// app.blobs row. It is deliberately NOT deleted here: blobs are
		// content-addressed, so the same bytes can be another writer's file
		// committed a moment ago, and "no row, so delete" between two
		// statements would race it. Row-less files are left for a sweep with a
		// grace period (follow-up to #922).
		return err
	}
	if a.onProxyCache != nil {
		a.onProxyCache(userID, split)
	}
	return nil
}

// ReportProxyCache reads one user's cache size, split by reference, and hands
// it to the cache observer (#922). cmd/mailroom calls it when it starts a
// user's loops, so the gauges have a real value from boot rather than only
// after the first fetch. A no-op without the proxy or an observer.
func (a *API) ReportProxyCache(ctx context.Context, userID ulid.ULID) error {
	if a.proxy == nil || a.onProxyCache == nil || a.store == nil {
		return nil
	}
	var split store.ProxyCacheSplit
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		split, e = tx.ProxyCacheBreakdown(ctx)
		return e
	}); err != nil {
		return err
	}
	a.onProxyCache(userID, split)
	return nil
}

// WithProxyCacheObserver installs the sink for the per-user cache size split by
// reference (#922). Referenced images are bounded by no eviction, so this is
// what keeps their growth visible.
func WithProxyCacheObserver(fn func(userID ulid.ULID, split store.ProxyCacheSplit)) Option {
	return func(a *API) { a.onProxyCache = fn }
}

// writeProxyImageHeaders is the one place the served image's headers are
// written, so a cache hit and a fresh fetch cannot drift apart in what they
// harden against.
func writeProxyImageHeaders(w http.ResponseWriter, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Referrer-Policy", "no-referrer")
}
