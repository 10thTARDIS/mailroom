// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/egress"
	"mailroom/internal/ingest"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// The image prefetch at arrival (#922, ADR-026 amendment of 2026-10-27).
//
// # The problem it solves
//
// The proxy fetched only when a message was opened. A sender whose image URLs
// are signed and EXPIRING (measured on a real instance: one host's `token-time` /
// `token-hash` URLs answer 403 once they age out, ADR-026 §7) therefore showed
// broken pictures in any message opened late — while newer mail from the same
// sender loaded fine. Nothing at open time can fix an expired signature; the
// only fix is to fetch while it is valid.
//
// # What it does
//
// Ingest queues one job per newly stored message whose sender passes the
// PROXY's own rule (store.EnqueueProxyPrefetchIfAllowed). The job re-reads the
// message under the user's identity, re-checks the rule, takes the same image
// list an open would (proxyEligibleImages: what the shipped sanitizer parks,
// beacons excluded, the per-message cap applied), and fetches each image through
// the same openProxyImage + storeProxyImage an open uses. Each cached image gets
// a reference to the message, and a referenced image is not evicted until the
// last referencing message is destroyed. A later open is an ordinary cache hit.
//
// # What it deliberately does not do
//
//   - fetch for a one-off "show remote content" click: that is the reader's
//     action at open time and stays an open-time fetch;
//   - fetch a beacon, ever (§3d);
//   - fetch for mail older than PrefetchMaxAge, so a first sync or an import of
//     years of history does not contact every sender in it at once;
//   - act on a From alone: it requires the sender authenticated by learned
//     placement's verdict (ingest.LearnedSender) and not stored as failing,
//     declines mail in Junk, and stays on the default ports (#922 review);
//   - grow without limit: at ReferencedCacheBytes of kept images a user's
//     prefetch stops (cap_reached) — and evicts nothing;
//   - hold the job dispatcher: one run is time-boxed (prefetchBudget) and
//     admitted only into half of the reader's per-minute budget; anything left
//     is a continuation job, so scheduled sends never queue behind a slow
//     sender.
//
// # The privacy trade, stated once
//
// A fetch at arrival tells the sender WHEN THE MAIL WAS DELIVERED, which their
// own mail server learns anyway, and stops telling them when it was OPENED —
// before, the cache hid open time only from the second open onward. ADR-026's
// amendment records it.

// Prefetch bounds that are not configuration: they protect the shared job
// dispatcher, not the senders (PerMinute and PerMessage do that).
const (
	// defaultPrefetchBudget is the wall-clock time one prefetch run may spend
	// before it hands the rest to a continuation. The dispatcher runs jobs one
	// at a time, so this is how long a scheduled send can wait behind a
	// prefetch — plus at most one fetch's Timeout, since a fetch in flight is
	// not cut off (a cut fetch would be logged as a sender timeout it was not).
	defaultPrefetchBudget = 10 * time.Second
	// prefetchMaxRounds bounds the continuations of one message. A message
	// still unfinished after this many runs is left to open-time fetching.
	prefetchMaxRounds = 20
	// prefetchMaxRateRounds bounds the rate deferrals of one message (#922
	// re-review). They do not spend rounds — waiting is not progress — but a
	// message must not be re-queued forever behind a window other mail keeps
	// filling. At one deferral per window this is about an hour of waiting.
	prefetchMaxRateRounds = 60
)

// errPrefetchPort is the prefetch's refusal of a non-default port, raised by
// the per-request check internal/egress applies to the first URL and to every
// redirect hop (egress.WithRequestCheck). fetchFailureReason maps it to
// port_refused.
var errPrefetchPort = errors.New("api: prefetch refuses a non-default port")

// prefetchPortCheck is that check.
func prefetchPortCheck(u *url.URL) error {
	if !prefetchDefaultPort(u.String()) {
		return errPrefetchPort
	}
	return nil
}

// Why a whole message was not prefetched. A closed set: they are counted, never
// free text.
const (
	PrefetchSkipDisabled      = "disabled"       // proxy, prefetch or cache off, or no blob store
	PrefetchSkipMessageGone   = "message_gone"   // destroyed before the job ran, or RLS hid it
	PrefetchSkipNotAllowed    = "not_allowed"    // the proxy's own rule refuses the sender (now)
	PrefetchSkipOutsideWindow = "outside_window" // received before PrefetchMaxAge
	PrefetchSkipUnreadable    = "unreadable"     // stored bytes unreadable or unparseable
	PrefetchSkipRoundsSpent   = "rounds_spent"   // prefetchMaxRounds continuations, or prefetchMaxRateRounds rate deferrals, already ran
	// The #922 review gates. Each is a reason the PREFETCH declines; an open of
	// the same message is unaffected.
	PrefetchSkipCapReached      = "cap_reached"     // the user's referenced bytes are at ReferencedCacheBytes
	PrefetchSkipUnauthenticated = "unauthenticated" // stored auth_fail, or not authenticated by learned placement's verdict
	PrefetchSkipJunk            = "junk"            // the message sits in a junk-role mailbox
)

// PrefetchResult is what one prefetch run did, per image and per message, so a
// run that changed nothing can say why (CLAUDE.md: never one total).
type PrefetchResult struct {
	// Skipped is set when the whole message was skipped (a PrefetchSkip* label).
	Skipped string
	// Eligible is how many distinct images the open path would serve for this
	// message (beacons excluded, after the per-message cap).
	Eligible int
	// Beacons are never in Eligible: proxyEligibleImages drops them before
	// anything is counted (proxy-report counts them, as a population excluded).
	Capped        bool // the per-message cap cut images off
	AlreadyCached int  // cached before this run; only the reference was added
	Fetched       int  // fetched and cached by this run
	Refused       int  // the fetch failed or was refused (logged with the domain)
	Empty         int  // the sender answered 200 with no bytes; nothing kept, not counted as fetched
	// Deferred is how many images were left for a continuation, and DeferredBy
	// says why ("budget" or "rate").
	Deferred   int
	DeferredBy string
}

// ProxyPrefetchEnabled reports whether arrival prefetch is in effect: the proxy
// is on, prefetch is on, and there is a cache to put images in.
func (a *API) ProxyPrefetchEnabled() bool {
	return a.proxy != nil && a.proxy.cfg.Prefetch && a.proxy.cacheEnabled()
}

// ProxyPrefetchMaxAge is the arrival window ingest applies before queueing a
// job; zero when prefetch is off.
func (a *API) ProxyPrefetchMaxAge() time.Duration {
	if !a.ProxyPrefetchEnabled() {
		return 0
	}
	return a.proxy.cfg.PrefetchMaxAge
}

// HandleProxyPrefetch is the jobs.Handler for store.ProxyPrefetchJobKind.
//
// A skipped message completes the job: nothing about it will change on retry.
// An error is returned only for a failure worth retrying — the store or the
// blob store — and the images already cached are skipped on the retry.
func (a *API) HandleProxyPrefetch(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		return errors.New("api: proxy prefetch job has no user")
	}
	var p store.ProxyPrefetchPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("api: proxy prefetch payload: %w", err)
	}
	msgID, err := ulid.Parse(p.MessageID)
	if err != nil {
		return fmt.Errorf("api: proxy prefetch message id: %w", err)
	}
	res, err := a.prefetchMessageImages(ctx, *job.UserID, msgID, p.Round, p.RateRounds)
	// Counts only: no URL, host or message id (the dispatcher's own line already
	// names the job).
	a.proxyLogger().DebugContext(ctx, "image prefetch run",
		"skipped", res.Skipped, "eligible", res.Eligible, "capped", res.Capped,
		"already_cached", res.AlreadyCached, "fetched", res.Fetched, "refused", res.Refused,
		"deferred", res.Deferred, "deferred_by", res.DeferredBy, "round", p.Round)
	return err
}

// PrefetchMessageImages prefetches one message's images (see the file comment).
// round is the continuation count of the job running it.
func (a *API) PrefetchMessageImages(ctx context.Context, userID, messageID ulid.ULID, round int) (PrefetchResult, error) {
	return a.prefetchMessageImages(ctx, userID, messageID, round, 0)
}

// prefetchMessageImages is PrefetchMessageImages with the rate-deferral count.
func (a *API) prefetchMessageImages(ctx context.Context, userID, messageID ulid.ULID, round, rateRounds int) (PrefetchResult, error) {
	var res PrefetchResult
	if !a.ProxyPrefetchEnabled() || a.blobs == nil || a.raw == nil || a.store == nil {
		res.Skipped = PrefetchSkipDisabled
		return res, nil
	}
	if round >= prefetchMaxRounds || rateRounds >= prefetchMaxRateRounds {
		res.Skipped = PrefetchSkipRoundsSpent
		return res, nil
	}
	start := time.Now()

	var (
		ref    store.MessageAuthRef
		allows []store.RemoteAllow
		junk   bool
		split  store.ProxyCacheSplit
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if ref, e = tx.MessageAuthRefByID(ctx, messageID); e != nil {
			return e
		}
		if allows, e = tx.ListRemoteAllows(ctx); e != nil {
			return e
		}
		if junk, e = tx.MessageInJunk(ctx, messageID); e != nil {
			return e
		}
		split, e = tx.ProxyCacheBreakdown(ctx)
		return e
	}); err != nil {
		if errors.Is(err, store.ErrMessageNotFound) {
			res.Skipped = PrefetchSkipMessageGone
			return res, nil
		}
		return res, err
	}
	// §3c, re-checked now: a grant withdrawn since arrival withdraws the fetch.
	// The PROXY's rule — never the client's — and never a one-off ask.
	if !store.ProxyRemoteAllowsMatch(ref.FromAddress, allows) {
		res.Skipped = PrefetchSkipNotAllowed
		return res, nil
	}
	if ref.ReceivedAt.Before(start.Add(-a.proxy.cfg.PrefetchMaxAge)) {
		res.Skipped = PrefetchSkipOutsideWindow
		return res, nil
	}
	// Junk is evidence the From may be forged; the prefetch acts on the From
	// before anyone has looked, so it declines. Re-checked here because the
	// message may have been moved to Junk after it was queued.
	if junk {
		res.Skipped = PrefetchSkipJunk
		return res, nil
	}
	// The referenced-bytes ceiling (#922 review H1): a ceiling on growth, never
	// an eviction. Nothing already kept is touched.
	if limit := a.proxy.cfg.ReferencedCacheBytes; limit > 0 && split.ReferencedBytes >= limit {
		res.Skipped = PrefetchSkipCapReached
		a.noteProxy(ctx, ProxyOutcome{Result: ProxyCapReached, Prefetch: true})
		a.warnCapReached(ctx, userID, split.ReferencedBytes, limit)
		return res, nil
	}

	raw, err := a.raw.ReadRaw(ctx, userID, ref.ContentHash)
	if err != nil {
		res.Skipped = PrefetchSkipUnreadable
		return res, nil
	}
	msg, err := mime.ParseBytes(raw)
	if err != nil {
		res.Skipped = PrefetchSkipUnreadable
		return res, nil
	}
	// AUTHENTICATION (#922 review M1). A grant names a sender by From, and From
	// is forgeable; an open is the reader acting on what they see, but a
	// prefetch is the server acting on the From alone. So it requires what
	// learned placement requires before acting on a From — THE SAME verdict
	// (ingest.LearnedSender: one From mailbox in one header, matching the
	// stored address, Authenticated() for its domain), not a second reading of
	// Authentication-Results — and refuses a message ingest stored as failing.
	if !prefetchSenderAuthenticated(ref, msg, a.trustedAuthservIDs) {
		res.Skipped = PrefetchSkipUnauthenticated
		return res, nil
	}

	urls, capped := proxyEligibleImages(msg, a.proxy.cfg.PerMessage)
	res.Capped = capped
	urls = distinctByCacheKey(urls)
	res.Eligible = len(urls)

	budget := a.proxy.prefetchBudget
	if budget <= 0 {
		budget = defaultPrefetchBudget
	}
	for i, target := range urls {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		urlHash := proxyURLHash(target)

		// Already cached (by an open, by another message, or by an earlier run
		// of this job): reference it and move on — no fetch. The check and the
		// reference are one transaction under the cache lock, so an eviction
		// cannot remove the entry in between (L1). An entry stored under a type
		// the allowlist no longer accepts is not a hit, exactly as
		// serveProxyCacheHit treats it; it is re-fetched and replaced.
		var hit bool
		if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			hit, e = tx.RefProxyImageIfCached(ctx, urlHash, messageID,
				func(ct string) bool { return proxyImageContentType(ct) != "" })
			return e
		}); err != nil {
			return res, err
		}
		if hit {
			res.AlreadyCached++
			a.noteProxy(ctx, ProxyOutcome{Result: ProxyCached, Prefetch: true})
			continue
		}

		// The prefetch's own port rule, before any budget is spent on it: a
		// fetch nobody has asked for yet stays on the scheme's default port.
		if !prefetchDefaultPort(target) {
			a.noteProxy(ctx, ProxyOutcome{Result: ProxyRefused, Reason: reasonPortRefused,
				Host: loggableHost(target), Prefetch: true})
			res.Refused++
			continue
		}

		// The budget is checked only once this run has made an attempt, so
		// every run makes progress: a run that deferred before its first fetch
		// would hand the same list to its successor forever.
		now := time.Now()
		if res.Fetched+res.Refused+res.Empty > 0 && now.Sub(start) >= budget {
			return a.deferPrefetch(ctx, userID, messageID, round+1, rateRounds, now, false, len(urls)-i, "budget", res)
		}
		// A rate deferral keeps its round: waiting for the reader's budget is
		// not progress, and must not spend the bounded supply of rounds (M3).
		if !a.proxy.allowBackground(userID, now) {
			return a.deferPrefetch(ctx, userID, messageID, round, rateRounds+1, a.proxy.windowReset(userID, now), true, len(urls)-i, "rate", res)
		}

		// The default-port rule rides on the request, so internal/egress
		// applies it to every redirect hop too — a check of the first URL alone
		// would be bypassed by one redirect.
		resp, ct, fail := a.proxy.openProxyImage(egress.WithRequestCheck(ctx, prefetchPortCheck), target)
		if fail != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			fail.outcome.Result = ProxyRefused
			fail.outcome.Prefetch = true
			a.noteProxy(ctx, fail.outcome)
			res.Refused++
			continue
		}
		// The body is capped by internal/egress exactly as on the open path; a
		// read past MaxBytes fails here and nothing is stored.
		body, rerr := io.ReadAll(resp.Body)
		drainAndClose(resp)
		if rerr != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			a.noteProxy(ctx, ProxyOutcome{Result: ProxyRefused, Reason: bodyFailureReason(rerr),
				Host: loggableHost(target), Prefetch: true})
			res.Refused++
			continue
		}
		// An empty 200 is served as-is on the open path and never cached; here
		// there is nothing to keep, so it is not counted as fetched (L3).
		if len(body) == 0 {
			a.noteProxy(ctx, ProxyOutcome{Result: ProxyEmpty, Prefetch: true})
			res.Empty++
			continue
		}
		if err := a.storeProxyImage(ctx, userID, target, ct, body, &messageID); err != nil {
			return res, err
		}
		a.noteProxy(ctx, ProxyOutcome{Result: ProxyServed, Prefetch: true})
		res.Fetched++
	}
	_ = a.ReportProxyCache(ctx, userID)
	return res, nil
}

// prefetchSenderAuthenticated is the prefetch's authentication gate: not stored
// as failing (messages.auth_fail), and authenticated by learned placement's own
// verdict for the stored From address. Without MAILROOM_AUTHSERV_IDS that
// verdict's floor is "did not clearly fail", exactly as for learned placement;
// with them, a trusted, aligned (relaxed) DMARC pass.
func prefetchSenderAuthenticated(ref store.MessageAuthRef, msg *mime.Message, trusted []string) bool {
	if ref.StoredAuthFail {
		return false
	}
	sender := ingest.LearnedSender(ref.FromAddress, msg, trusted, ref.ReceivedAt)
	if _, _, ok := sender.Keys(); !ok || sender.Ambiguous() {
		return false
	}
	return sender.Auth().Authenticated()
}

// prefetchDefaultPort reports whether an image URL uses its scheme's default
// port: none written, 443 for https, 80 for http. Anything else — another
// port, another scheme, an unparseable URL — is refused by the prefetch; the
// open path does not apply this rule.
func prefetchDefaultPort(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Port() == "" || u.Port() == "443"
	case "http":
		return u.Port() == "" || u.Port() == "80"
	}
	return false
}

// capWarnEvery bounds the cap_reached WARN to one line per user per hour.
const capWarnEvery = time.Hour

// warnCapReached writes the cap_reached WARN at most once per user per hour.
// Counts only: no URL, domain or message id.
func (a *API) warnCapReached(ctx context.Context, userID ulid.ULID, have, limit int64) {
	p := a.proxy
	now := time.Now()
	p.mu.Lock()
	last, seen := p.capWarned[userID]
	if seen && now.Sub(last) < capWarnEvery {
		p.mu.Unlock()
		return
	}
	p.capWarned[userID] = now
	p.mu.Unlock()
	a.proxyLogger().WarnContext(ctx,
		"image prefetch paused for a user: images kept for their messages have reached the limit; "+
			"nothing kept is removed, new mail's images load when opened",
		"user_id", userID.String(), "referenced_bytes", have, "limit_bytes", limit,
		"env", "MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES")
}

// deferPrefetch queues the continuation for the images this run did not reach
// and completes the current run. The continuation starts from the top of the
// list; what this run cached is a cheap hit for it.
func (a *API) deferPrefetch(ctx context.Context, userID, messageID ulid.ULID, nextRound, nextRateRounds int, at time.Time,
	rateDeferred bool, left int, why string, res PrefetchResult) (PrefetchResult, error) {
	res.Deferred, res.DeferredBy = left, why
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, e := tx.EnqueueProxyPrefetch(ctx, messageID, at, nextRound, nextRateRounds, rateDeferred)
		return e
	}); err != nil {
		return res, err
	}
	_ = a.ReportProxyCache(ctx, userID)
	return res, nil
}

// distinctByCacheKey drops repeats of one image within a message — a logo used
// twice is one fetch and one reference.
func distinctByCacheKey(urls []string) []string {
	seen := make(map[string]bool, len(urls))
	out := urls[:0:0]
	for _, u := range urls {
		k := string(proxyURLHash(u))
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	return out
}
