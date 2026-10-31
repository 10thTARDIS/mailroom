// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The image prefetch at arrival (#922), end to end through the real store, the
// real sealed blob store and the real sanitizer, with the egress seam
// (proxyFetcher) replaced by a recorder so every outbound request is counted.
//
// The headline is the second half of the first test: after a prefetch, opening
// the message makes ZERO upstream requests. Every refusal test asserts the
// recorder saw nothing (or exactly what the open path would also have asked
// for), because "the prefetch did not cache it" is also what a prefetch that
// never ran would look like.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/egress"
	"mailroom/internal/store"
)

// The recipient's token, in every URL below: it must never reach a log line.
const prefetchSecret = "SECRETTOKEN922"

const (
	heroURL  = "https://cdn.news.example/hero.png?token-time=1&token-hash=" + prefetchSecret
	logoURL  = "https://cdn.news.example/logo.png"
	pixelURL = "https://pixel.news.example/open.gif?u=" + prefetchSecret
)

// recordingFetcher answers by URL and records every request it is asked for.
type recordingFetcher struct {
	mu      sync.Mutex
	calls   []string
	respond func(url string) (*http.Response, error)
}

func (f *recordingFetcher) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.URL.String())
	f.mu.Unlock()
	return f.respond(req.URL.String())
}

func (f *recordingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *recordingFetcher) asked(url string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == url {
			return true
		}
	}
	return false
}

// pngFor is the body served for a URL, distinct per URL so a cache hit can be
// checked byte for byte.
func pngFor(url string) string { return "\x89PNG-bytes-of-" + url }

func servesImages(url string) (*http.Response, error) {
	return imgResp(200, "image/png", pngFor(url)), nil
}

type prefetchHarness struct {
	*composeHarness
	user    apiFixture
	fetcher *recordingFetcher
	// fetcherOverride, when set, replaces fetcher as the egress seam on the
	// next rebuild (fetcher still records).
	fetcherOverride proxyFetcher
	logs            *bytes.Buffer
	outcomes        []ProxyOutcome
}

func newPrefetchHarness(t *testing.T, cfg ProxyConfig, respond func(string) (*http.Response, error)) *prefetchHarness {
	t.Helper()
	ch := newComposeHarness(t)
	h := &prefetchHarness{composeHarness: ch, logs: &bytes.Buffer{},
		fetcher: &recordingFetcher{respond: respond}}
	h.user, _ = seedTwo(t, ch.super)
	if _, err := ch.super.Exec(context.Background(),
		`INSERT INTO app.remote_allows (id, user_id, kind, value) VALUES ($1, $2, 'domain', 'news.example')`,
		store.NewULID().String(), h.user.UserID.String()); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	h.rebuild(cfg)
	return h
}

// rebuild re-creates the API with cfg, keeping the store, the blob store and
// the recorder.
func (h *prefetchHarness) rebuild(cfg ProxyConfig, extra ...Option) {
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	opts := append([]Option{WithComposer(compose.NewComposer(h.blobs, fakeKeys{}, time.Millisecond)),
		withImageProxyClient(cfg, h.seam()),
		WithProxyObserver(logger, func(o ProxyOutcome) { h.outcomes = append(h.outcomes, o) })}, extra...)
	h.api = New(h.store, opts...)
}

func (h *prefetchHarness) seam() proxyFetcher {
	if h.fetcherOverride != nil {
		return h.fetcherOverride
	}
	return h.fetcher
}

func prefetchCfg() ProxyConfig {
	return ProxyConfig{Enabled: true, Prefetch: true, PerMinute: 1000}
}

// seedHTMLMessage stores a real RFC 5322 message with the given HTML body in
// the sealed blob store and points a message row at it.
func (h *prefetchHarness) seedHTMLMessage(t *testing.T, from, html string, received time.Time) ulid.ULID {
	t.Helper()
	return h.seedHTMLMessageWith(t, from, "", html, received)
}

// seedHTMLMessageWith is seedHTMLMessage with extra header lines (each ending
// in CRLF), e.g. an Authentication-Results header.
func (h *prefetchHarness) seedHTMLMessageWith(t *testing.T, from, headers, html string, received time.Time) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	raw := "From: " + from + "\r\nTo: a@example.com\r\nSubject: issue\r\n" + headers +
		"Message-ID: <" + store.NewULID().String() + "@news.example>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + html + "\r\n"
	c := compose.NewComposer(h.blobs, fakeKeys{}, time.Millisecond)
	stored, err := c.PutRaw(ctx, h.user.UserID, strings.NewReader(raw))
	if err != nil {
		t.Fatalf("put raw: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		h.user.UserID.String(), stored.ContentHash, stored.SizePlain, stored.SizeStored, stored.DEKID.String()); err != nil {
		t.Fatalf("seed blob row: %v", err)
	}
	id := store.NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, from_address, received_at, size_bytes)
		VALUES ($1, $2, $3, $4, 'issue', $5, $6, $7)`,
		id.String(), h.user.UserID.String(), stored.ContentHash, h.user.ThreadID.String(),
		addressOf(from), received, len(raw)); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

func addressOf(from string) string {
	if i := strings.IndexByte(from, '<'); i >= 0 {
		return strings.TrimSuffix(from[i+1:], ">")
	}
	return from
}

// open asks the proxy for one image of a message, as the app would.
func (h *prefetchHarness) open(msg ulid.ULID, url string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/jmap/proxy/x/y/z", nil)
	req.SetPathValue("accountId", h.user.UserID.String())
	req.SetPathValue("messageId", msg.String())
	req.SetPathValue("token", ProxyTokenFor(url))
	rec := httptest.NewRecorder()
	h.api.serveProxy(rec, req, h.user.UserID)
	return rec
}

func (h *prefetchHarness) prefetch(t *testing.T, msg ulid.ULID) PrefetchResult {
	t.Helper()
	res, err := h.api.PrefetchMessageImages(context.Background(), h.user.UserID, msg, 0)
	if err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	return res
}

func (h *prefetchHarness) refs(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.proxy_image_refs WHERE user_id = $1 AND message_id = $2`,
		h.user.UserID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("count refs: %v", err)
	}
	return n
}

func (h *prefetchHarness) cached(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.proxy_image_cache WHERE user_id = $1`, h.user.UserID.String()).Scan(&n); err != nil {
		t.Fatalf("count cache: %v", err)
	}
	return n
}

const newsletterHTML = `<html><body>` +
	`<img src="` + heroURL + `" alt="hero">` +
	`<img src="` + logoURL + `" alt="logo">` +
	`<img src="` + logoURL + `" alt="logo again">` +
	`<img src="` + pixelURL + `" width="1" height="1" alt="">` +
	`</body></html>`

// TestPrefetchCachesAnAllowedSendersImagesAndTheOpenIsACacheHit is the
// headline: the prefetch fetches each distinct image once, never the beacon,
// references each from the message — and a later open makes no upstream
// request at all.
func TestPrefetchCachesAnAllowedSendersImagesAndTheOpenIsACacheHit(t *testing.T) {
	h := newPrefetchHarness(t, prefetchCfg(), servesImages)
	h.clearCache(t)
	msg := h.seedHTMLMessage(t, "Letters <letters@news.example>", newsletterHTML, time.Now())

	res := h.prefetch(t, msg)
	if res.Skipped != "" || res.Eligible != 2 || res.Fetched != 2 || res.Refused != 0 {
		t.Fatalf("prefetch = %+v, want 2 eligible, 2 fetched", res)
	}
	if h.fetcher.count() != 2 || !h.fetcher.asked(heroURL) || !h.fetcher.asked(logoURL) {
		t.Fatalf("upstream calls = %v, want exactly the hero and the logo once each", h.fetcher.calls)
	}
	if h.fetcher.asked(pixelURL) {
		t.Fatal("the prefetch fetched a 1x1 beacon — that reintroduces the open-tracking v1.42.0 removed")
	}
	if n := h.refs(t, msg); n != 2 {
		t.Errorf("references = %d, want 2 (one per distinct image)", n)
	}

	before := h.fetcher.count()
	h.outcomes = nil
	for _, u := range []string{heroURL, logoURL} {
		rec := h.open(msg, u)
		if rec.Code != http.StatusOK {
			t.Fatalf("open %s: status %d", u, rec.Code)
		}
		if got := rec.Body.String(); got != pngFor(u) {
			t.Errorf("open served %q, want the prefetched bytes", got)
		}
	}
	if after := h.fetcher.count(); after != before {
		t.Errorf("opening the message made %d upstream request(s) after the prefetch; want 0 — "+
			"the sender learned the open time the prefetch was meant to hide", after-before)
	}
	for _, o := range h.outcomes {
		if o.Result != ProxyCached {
			t.Errorf("open outcome = %+v, want cached", o)
		}
	}

	// Running it again (a retried job) fetches nothing and adds nothing.
	again := h.prefetch(t, msg)
	if again.Fetched != 0 || again.AlreadyCached != 2 || h.fetcher.count() != before {
		t.Errorf("second run = %+v with %d calls, want 2 already cached and no fetch", again, h.fetcher.count()-before)
	}
	if n := h.refs(t, msg); n != 2 {
		t.Errorf("references after a second run = %d, want still 2", n)
	}
}

func (h *prefetchHarness) clearCache(t *testing.T) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`DELETE FROM app.proxy_image_cache WHERE user_id = $1`, h.user.UserID.String()); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
}

// TestPrefetchSkipsWhatTheOpenPathWouldNotFetch: a sender the proxy's rule
// refuses, mail outside the window, another user's message, and every way of
// turning the feature off — each fetches nothing.
func TestPrefetchSkipsWhatTheOpenPathWouldNotFetch(t *testing.T) {
	h := newPrefetchHarness(t, prefetchCfg(), servesImages)
	h.clearCache(t)

	stranger := h.seedHTMLMessage(t, "someone@stranger.example", newsletterHTML, time.Now())
	if res := h.prefetch(t, stranger); res.Skipped != PrefetchSkipNotAllowed {
		t.Errorf("stranger: %+v, want skipped not_allowed", res)
	}
	// The PROXY's rule is fail-closed on a multiple-@ From that the client rule
	// would read as the granted domain (ADR-026 §3c).
	spoof := h.seedHTMLMessage(t, "attacker@evil.example@news.example", newsletterHTML, time.Now())
	if res := h.prefetch(t, spoof); res.Skipped != PrefetchSkipNotAllowed {
		t.Errorf("multiple-@ From: %+v, want skipped not_allowed", res)
	}
	old := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now().Add(-30*24*time.Hour))
	if res := h.prefetch(t, old); res.Skipped != PrefetchSkipOutsideWindow {
		t.Errorf("30-day-old mail: %+v, want skipped outside_window", res)
	}
	// Another user's message id: RLS hides it, so it is simply gone.
	_, b := seedTwo(t, h.super)
	if res := h.prefetch(t, b.MessageID); res.Skipped != PrefetchSkipMessageGone {
		t.Errorf("another user's message: %+v, want skipped message_gone", res)
	}
	if n := h.fetcher.count(); n != 0 {
		t.Fatalf("%d upstream request(s) for messages that must not be prefetched: %v", n, h.fetcher.calls)
	}

	recent := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())
	for name, cfg := range map[string]ProxyConfig{
		"prefetch off":  {Enabled: true, Prefetch: false, PerMinute: 1000},
		"cache off":     {Enabled: true, Prefetch: true, PerMinute: 1000, CacheBytes: -1},
		"proxy off":     {Enabled: false, Prefetch: true},
		"proxy default": {},
	} {
		h.rebuild(cfg)
		if res := h.prefetch(t, recent); res.Skipped != PrefetchSkipDisabled {
			t.Errorf("%s: %+v, want skipped disabled", name, res)
		}
	}
	if n := h.fetcher.count(); n != 0 {
		t.Fatalf("%d upstream request(s) with the feature off", n)
	}

	// CONTROL: the same recent message with the feature on IS prefetched, so
	// every skip above was a decision and not a harness that cannot fetch.
	h.rebuild(prefetchCfg())
	if res := h.prefetch(t, recent); res.Fetched != 2 {
		t.Fatalf("CONTROL: recent allowed mail fetched %d, want 2 (%+v)", res.Fetched, res)
	}
}

// TestPrefetchRefusesExactlyWhatAnOpenRefusesAndLogsTheHost: a guard refusal,
// a non-image and an over-size body are refused by the prefetch for the same
// reason the open path gives, nothing is cached, and the log names each host —
// never the path or the recipient's token.
func TestPrefetchRefusesExactlyWhatAnOpenRefusesAndLogsTheHost(t *testing.T) {
	// Three registrable domains, so each refusal is attributable; a token in a
	// SUBDOMAIN label on the first, which the log must not carry (M2).
	const (
		blockedURL = "https://u-" + prefetchSecret + ".internal.blocked.example/a.png?t=" + prefetchSecret
		htmlURL    = "https://page.html.example/b.png?t=" + prefetchSecret
		bigURL     = "https://big.large.example/c.png?t=" + prefetchSecret
	)
	respond := func(url string) (*http.Response, error) {
		switch url {
		case blockedURL:
			return nil, fmt.Errorf("dial blocked.example: %w", egress.ErrBlockedAddress)
		case htmlURL:
			return imgResp(200, "text/html", "<p>not an image</p>"), nil
		case bigURL:
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}},
				Body: errBody{egress.ErrTooLarge}}, nil
		}
		return servesImages(url)
	}
	h := newPrefetchHarness(t, prefetchCfg(), respond)
	h.clearCache(t)
	html := `<img src="` + blockedURL + `"><img src="` + htmlURL + `"><img src="` + bigURL + `">`
	msg := h.seedHTMLMessage(t, "letters@news.example", html, time.Now())

	res := h.prefetch(t, msg)
	if res.Refused != 3 || res.Fetched != 0 {
		t.Fatalf("prefetch = %+v, want 3 refused, 0 fetched", res)
	}
	if n := h.cached(t); n != 0 {
		t.Errorf("%d image(s) cached from refused fetches", n)
	}
	if n := h.refs(t, msg); n != 0 {
		t.Errorf("%d reference(s) to images that were never cached", n)
	}
	prefetchReasons := map[string]string{}
	for _, o := range h.outcomes {
		if !o.Prefetch || o.Result != ProxyRefused {
			t.Errorf("prefetch outcome %+v, want a refusal marked Prefetch", o)
		}
		prefetchReasons[o.Host] = o.Reason
	}
	want := map[string]string{
		"blocked.example": reasonEgressRefused,
		"html.example":    reasonContentType,
		"large.example":   reasonTooLarge,
	}
	for host, reason := range want {
		if prefetchReasons[host] != reason {
			t.Errorf("prefetch of %s refused as %q, want %q", host, prefetchReasons[host], reason)
		}
	}

	logs := h.logs.String()
	for host := range want {
		if !strings.Contains(logs, "host="+host) {
			t.Errorf("no log line names %s: %s", host, logs)
		}
	}
	for _, secret := range []string{prefetchSecret, "internal.blocked", "page.html", "big.large", "/a.png", "/b.png", "/c.png", "https://", msg.String(), h.user.UserID.String()} {
		if strings.Contains(logs, secret) {
			t.Errorf("a refusal log line carries %q: %s", secret, logs)
		}
	}

	// The open path refuses the same three for the same reasons.
	h.outcomes = nil
	for _, u := range []string{blockedURL, htmlURL, bigURL} {
		h.open(msg, u)
	}
	for _, o := range h.outcomes {
		if o.Prefetch {
			t.Errorf("an open was reported as a prefetch: %+v", o)
		}
		if want[o.Host] != o.Reason {
			t.Errorf("open of %s refused as %q, prefetch said %q", o.Host, o.Reason, want[o.Host])
		}
	}
	if len(h.outcomes) != 3 {
		t.Errorf("open outcomes = %+v, want 3", h.outcomes)
	}
}

// TestPrefetchDefersPastHalfTheRateWindow: the prefetch spends at most half the
// per-minute window, queues a continuation for the rest, and the continuation
// — run through the job handler — finishes the message without re-fetching.
func TestPrefetchDefersPastHalfTheRateWindow(t *testing.T) {
	h := newPrefetchHarness(t, ProxyConfig{Enabled: true, Prefetch: true, PerMinute: 2}, servesImages)
	h.clearCache(t)
	msg := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())

	res := h.prefetch(t, msg)
	if res.Fetched != 1 || res.Deferred != 1 || res.DeferredBy != "rate" {
		t.Fatalf("prefetch = %+v, want 1 fetched and 1 deferred by rate", res)
	}
	var (
		n       int
		payload []byte
	)
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(min(payload::text), '') FROM app.jobs
		  WHERE user_id = $1 AND kind = $2 AND state = 'pending' AND payload->>'message_id' = $3`,
		h.user.UserID.String(), store.ProxyPrefetchJobKind, msg.String()).Scan(&n, &payload); err != nil {
		t.Fatalf("count continuation: %v", err)
	}
	if n != 1 {
		t.Fatalf("continuation jobs = %d, want 1", n)
	}

	// A minute later (the window reset), the continuation finishes the job.
	h.api.proxy.mu.Lock()
	h.api.proxy.perUser = map[ulid.ULID]*proxyWindow{}
	h.api.proxy.mu.Unlock()
	uid := h.user.UserID
	if err := h.api.HandleProxyPrefetch(context.Background(), store.Job{
		ID: store.NewULID(), UserID: &uid, Kind: store.ProxyPrefetchJobKind, Payload: payload,
	}); err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if got := h.fetcher.count(); got != 2 {
		t.Errorf("upstream calls after the continuation = %d, want 2 (each image once)", got)
	}
	if n := h.refs(t, msg); n != 2 {
		t.Errorf("references = %d, want 2", n)
	}
	// A rate deferral keeps its round (M3): waiting for the reader's budget is
	// not progress and must not spend the bounded supply of rounds.
	var p store.ProxyPrefetchPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Round != 0 {
		t.Errorf("rate continuation payload %s: round %d, want 0 (%v)", payload, p.Round, err)
	}
}

// TestPrefetchDefersWhenTheBudgetRunsOut: a run past its time budget hands the
// rest to a continuation one round on — but only after it has made an attempt,
// so every run makes progress.
func TestPrefetchDefersWhenTheBudgetRunsOut(t *testing.T) {
	h := newPrefetchHarness(t, prefetchCfg(), servesImages)
	h.clearCache(t)
	h.api.proxy.prefetchBudget = time.Nanosecond
	msg := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())

	res := h.prefetch(t, msg)
	if res.Fetched != 1 {
		t.Fatalf("prefetch = %+v: a run with a spent budget must still fetch one image before deferring", res)
	}
	if res.Deferred != 1 || res.DeferredBy != "budget" {
		t.Fatalf("prefetch = %+v, want 1 deferred by budget", res)
	}
	var round int
	if err := h.super.QueryRow(context.Background(),
		`SELECT (payload->>'round')::int FROM app.jobs
		  WHERE user_id = $1 AND kind = $2 AND state = 'pending' AND payload->>'message_id' = $3`,
		h.user.UserID.String(), store.ProxyPrefetchJobKind, msg.String()).Scan(&round); err != nil {
		t.Fatalf("read continuation: %v", err)
	}
	if round != 1 {
		t.Errorf("budget continuation round = %d, want 1", round)
	}
}

// TestPrefetchRequiresAnAuthenticatedSenderOutsideJunk: the review gates. A
// message stored as failing authentication, one learned placement's verdict
// does not authenticate, and one sitting in Junk are not prefetched; the same
// sender's authenticated inbox mail is (the control).
func TestPrefetchRequiresAnAuthenticatedSenderOutsideJunk(t *testing.T) {
	h := newPrefetchHarness(t, prefetchCfg(), servesImages)
	h.clearCache(t)
	ctx := context.Background()

	failed := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET auth_fail = true WHERE id = $1`, failed.String()); err != nil {
		t.Fatalf("mark auth_fail: %v", err)
	}
	if res := h.prefetch(t, failed); res.Skipped != PrefetchSkipUnauthenticated {
		t.Errorf("stored auth_fail: %+v, want skipped unauthenticated", res)
	}

	junk := store.NewULID()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		 VALUES ($1, $2, $3, 'Spam', 'Spam', '\Junk')`,
		junk.String(), h.user.UserID.String(), h.user.AccountID.String()); err != nil {
		t.Fatalf("seed junk: %v", err)
	}
	inJunk := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())
	h.place(t, inJunk, junk)
	if res := h.prefetch(t, inJunk); res.Skipped != PrefetchSkipJunk {
		t.Errorf("message in Junk: %+v, want skipped junk", res)
	}

	// With a trusted authserv-id configured, the verdict needs a trusted,
	// aligned DMARC pass: none at all is refused ...
	h.rebuild(prefetchCfg(), WithTrustedAuthservIDs([]string{"mx.trusted.example"}))
	bare := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())
	h.place(t, bare, h.user.InboxID)
	if res := h.prefetch(t, bare); res.Skipped != PrefetchSkipUnauthenticated {
		t.Errorf("no trusted pass: %+v, want skipped unauthenticated", res)
	}
	// ... and so is a pass from an id that is not trusted.
	forged := h.seedHTMLMessageWith(t, "letters@news.example",
		"Authentication-Results: evil.example; dkim=pass; dmarc=pass header.from=news.example\r\n",
		newsletterHTML, time.Now())
	if res := h.prefetch(t, forged); res.Skipped != PrefetchSkipUnauthenticated {
		t.Errorf("untrusted pass: %+v, want skipped unauthenticated", res)
	}
	if n := h.fetcher.count(); n != 0 {
		t.Fatalf("%d upstream request(s) for mail the gates refuse: %v", n, h.fetcher.calls)
	}

	// CONTROL: a trusted, aligned pass in the inbox is prefetched.
	good := h.seedHTMLMessageWith(t, "letters@news.example",
		"Authentication-Results: mx.trusted.example; dkim=pass header.d=news.example; dmarc=pass header.from=news.example\r\n",
		newsletterHTML, time.Now())
	h.place(t, good, h.user.InboxID)
	if res := h.prefetch(t, good); res.Skipped != "" || res.Fetched != 2 {
		t.Fatalf("CONTROL: authenticated inbox mail = %+v, want 2 fetched", res)
	}
}

// place gives a message a live server copy in a mailbox.
func (h *prefetchHarness) place(t *testing.T, msg, mailbox ulid.ULID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		 VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		store.NewULID().String(), h.user.UserID.String(), msg.String(), h.user.AccountID.String(),
		mailbox.String(), testUID()); err != nil {
		t.Fatalf("place message: %v", err)
	}
}

// TestPrefetchStopsAtTheKeptImagesCeilingAndEvictsNothing: below the ceiling
// the prefetch fetches (the control); at it, a run fetches nothing, evicts
// nothing, is counted, and warns once; a negative ceiling means no limit.
func TestPrefetchStopsAtTheKeptImagesCeilingAndEvictsNothing(t *testing.T) {
	cfg := prefetchCfg()
	cfg.ReferencedCacheBytes = 1 << 30
	h := newPrefetchHarness(t, cfg, servesImages)
	h.clearCache(t)

	first := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())
	if res := h.prefetch(t, first); res.Fetched != 2 {
		t.Fatalf("CONTROL below the ceiling: %+v, want 2 fetched", res)
	}
	cachedBefore, refsBefore, callsBefore := h.cached(t), h.refs(t, first), h.fetcher.count()

	cfg.ReferencedCacheBytes = 1 // already exceeded by the two kept images
	h.rebuild(cfg)
	other := `<img src="https://cdn.news.example/issue2.png"><img src="https://cdn.news.example/issue2b.png">`
	second := h.seedHTMLMessage(t, "letters@news.example", other, time.Now())
	h.outcomes = nil
	for i := 0; i < 2; i++ {
		if res := h.prefetch(t, second); res.Skipped != PrefetchSkipCapReached {
			t.Fatalf("at the ceiling (run %d): %+v, want skipped cap_reached", i+1, res)
		}
	}
	if n := h.fetcher.count() - callsBefore; n != 0 {
		t.Errorf("%d upstream request(s) at the ceiling", n)
	}
	if h.cached(t) != cachedBefore || h.refs(t, first) != refsBefore {
		t.Errorf("the ceiling evicted something: cache %d -> %d, refs %d -> %d",
			cachedBefore, h.cached(t), refsBefore, h.refs(t, first))
	}
	capped := 0
	for _, o := range h.outcomes {
		if o.Prefetch && o.Result == ProxyCapReached {
			capped++
		}
	}
	if capped != 2 {
		t.Errorf("cap_reached outcomes = %d, want 2 (one per run): %+v", capped, h.outcomes)
	}
	if n := strings.Count(h.logs.String(), "image prefetch paused"); n != 1 {
		t.Errorf("cap warning written %d times over two runs, want once per hour: %s", n, h.logs.String())
	}

	cfg.ReferencedCacheBytes = -1 // the operator opted into no limit
	h.rebuild(cfg)
	if res := h.prefetch(t, second); res.Fetched != 2 {
		t.Errorf("with no limit: %+v, want 2 fetched", res)
	}
}

// TestPrefetchRefusesANonDefaultPortThatAnOpenFetches: the prefetch stays on
// 443/80; a reader opening the same image is unaffected.
func TestPrefetchRefusesANonDefaultPortThatAnOpenFetches(t *testing.T) {
	h := newPrefetchHarness(t, prefetchCfg(), servesImages)
	h.clearCache(t)
	const (
		oddPort     = "https://cdn.ports.example:8443/x.png?t=" + prefetchSecret
		explicit443 = "https://cdn.ports.example:443/y.png"
	)
	msg := h.seedHTMLMessage(t, "letters@news.example",
		`<img src="`+oddPort+`"><img src="`+explicit443+`">`, time.Now())

	res := h.prefetch(t, msg)
	if res.Refused != 1 || res.Fetched != 1 {
		t.Fatalf("prefetch = %+v, want the :8443 image refused and the :443 one fetched", res)
	}
	if h.fetcher.asked(oddPort) {
		t.Fatal("the prefetch contacted a non-default port")
	}
	var refusal ProxyOutcome
	for _, o := range h.outcomes {
		if o.Result == ProxyRefused {
			refusal = o
		}
	}
	if refusal.Reason != reasonPortRefused || refusal.Host != "ports.example" {
		t.Errorf("refusal = %+v, want port_refused naming ports.example", refusal)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "reason=port_refused") ||
		!strings.Contains(logs, "host=ports.example") || strings.Contains(logs, prefetchSecret) || strings.Contains(logs, "8443") {
		t.Errorf("port refusal log line wrong: %s", logs)
	}

	// The open path does not apply the rule.
	if rec := h.open(msg, oddPort); rec.Code != http.StatusOK {
		t.Errorf("open of the :8443 image: status %d, want 200", rec.Code)
	}
	if !h.fetcher.asked(oddPort) {
		t.Error("the open did not fetch the :8443 image")
	}
}

// TestPrefetchDoesNotCountAnEmptyBodyAsFetched: a 200 with no bytes leaves
// nothing to keep, so it is neither cached, referenced nor counted as fetched
// (#922 review L3).
func TestPrefetchDoesNotCountAnEmptyBodyAsFetched(t *testing.T) {
	h := newPrefetchHarness(t, prefetchCfg(), func(string) (*http.Response, error) {
		return imgResp(200, "image/png", ""), nil
	})
	h.clearCache(t)
	msg := h.seedHTMLMessage(t, "letters@news.example", `<img src="https://cdn.news.example/blank.png">`, time.Now())
	res := h.prefetch(t, msg)
	if res.Fetched != 0 || res.Empty != 1 {
		t.Errorf("prefetch = %+v, want 0 fetched and 1 empty", res)
	}
	if h.cached(t) != 0 || h.refs(t, msg) != 0 {
		t.Errorf("an empty body left %d cache row(s) and %d reference(s)", h.cached(t), h.refs(t, msg))
	}
	if h.fetcher.count() != 1 {
		t.Errorf("upstream calls = %d, want 1 (CONTROL: the fetch happened)", h.fetcher.count())
	}
}

// TestPrefetchRefusesARedirectToANonDefaultPortThatAnOpenFollows: the port
// rule rides on the request (egress.WithRequestCheck) and is applied on every
// hop, so a redirect cannot carry the prefetch to :8443. The double below
// follows a redirect exactly as internal/egress does — consulting the request's
// check for the next hop — and egress's own TestRequestCheckIsAppliedToEveryRedirectHop
// proves the real client does.
func TestPrefetchRefusesARedirectToANonDefaultPortThatAnOpenFollows(t *testing.T) {
	const (
		start  = "https://cdn.redirect.example/start.png?t=" + prefetchSecret
		target = "https://cdn.redirect.example:8443/real.png"
	)
	h := newPrefetchHarness(t, prefetchCfg(), nil)
	h.fetcher = &recordingFetcher{}
	redirecting := &redirectingFetcher{rec: h.fetcher, from: start, to: target}
	h.fetcherOverride = redirecting
	h.rebuild(prefetchCfg())
	h.clearCache(t)
	msg := h.seedHTMLMessage(t, "letters@news.example", `<img src="`+start+`">`, time.Now())

	res := h.prefetch(t, msg)
	if res.Refused != 1 || res.Fetched != 0 {
		t.Fatalf("prefetch = %+v, want the redirected image refused", res)
	}
	if redirecting.followed != 0 {
		t.Fatal("the prefetch followed a redirect to a non-default port")
	}
	if h.cached(t) != 0 || h.refs(t, msg) != 0 {
		t.Errorf("a refused redirect left %d cache row(s), %d reference(s)", h.cached(t), h.refs(t, msg))
	}
	var refusal ProxyOutcome
	for _, o := range h.outcomes {
		if o.Result == ProxyRefused {
			refusal = o
		}
	}
	if refusal.Reason != reasonPortRefused || refusal.Host != "redirect.example" {
		t.Errorf("refusal = %+v, want port_refused naming redirect.example", refusal)
	}

	// CONTROL: an open of the same image follows the redirect.
	if rec := h.open(msg, start); rec.Code != http.StatusOK {
		t.Fatalf("open: status %d, want 200 through the redirect", rec.Code)
	}
	if redirecting.followed != 1 {
		t.Errorf("open followed %d redirect(s), want 1", redirecting.followed)
	}
}

// redirectingFetcher answers `from` with a redirect to `to`, following it the
// way internal/egress does: the next hop is subject to the request's check.
type redirectingFetcher struct {
	rec      *recordingFetcher
	from, to string
	followed int
}

func (f *redirectingFetcher) Do(req *http.Request) (*http.Response, error) {
	f.rec.mu.Lock()
	f.rec.calls = append(f.rec.calls, req.URL.String())
	f.rec.mu.Unlock()
	if req.URL.String() != f.from {
		return imgResp(404, "text/plain", "no"), nil
	}
	next, _ := url.Parse(f.to)
	if err := egress.RequestCheck(req.Context(), next); err != nil {
		return nil, &url.Error{Op: "Get", URL: f.to, Err: err}
	}
	f.followed++
	return imgResp(200, "image/png", pngFor(f.to)), nil
}

// TestPrefetchRateDeferralsAreBounded: rate deferrals keep the round but are
// counted, and past prefetchMaxRateRounds the message is left to open time.
func TestPrefetchRateDeferralsAreBounded(t *testing.T) {
	h := newPrefetchHarness(t, ProxyConfig{Enabled: true, Prefetch: true, PerMinute: 2}, servesImages)
	h.clearCache(t)
	msg := h.seedHTMLMessage(t, "letters@news.example", newsletterHTML, time.Now())
	ctx := context.Background()

	res, err := h.api.prefetchMessageImages(ctx, h.user.UserID, msg, 0, prefetchMaxRateRounds)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != PrefetchSkipRoundsSpent || h.fetcher.count() != 0 {
		t.Fatalf("at the rate-deferral bound: %+v with %d call(s), want rounds_spent and none", res, h.fetcher.count())
	}

	// CONTROL: one below the bound still runs, defers, and the continuation
	// carries the count — so the bound is reached rather than reset.
	res, err = h.api.prefetchMessageImages(ctx, h.user.UserID, msg, 0, prefetchMaxRateRounds-1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 1 || res.DeferredBy != "rate" {
		t.Fatalf("below the bound: %+v, want 1 fetched and a rate deferral", res)
	}
	var round, rateRounds int
	if err := h.super.QueryRow(ctx,
		`SELECT COALESCE((payload->>'round')::int, 0), (payload->>'rate_rounds')::int FROM app.jobs
		  WHERE user_id = $1 AND kind = $2 AND state = 'pending' AND payload->>'message_id' = $3`,
		h.user.UserID.String(), store.ProxyPrefetchJobKind, msg.String()).Scan(&round, &rateRounds); err != nil {
		t.Fatalf("read continuation: %v", err)
	}
	if round != 0 || rateRounds != prefetchMaxRateRounds {
		t.Errorf("continuation round=%d rate_rounds=%d, want 0 and %d", round, rateRounds, prefetchMaxRateRounds)
	}
}
