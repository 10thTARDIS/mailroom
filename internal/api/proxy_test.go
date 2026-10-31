// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/egress"
)

const trackerURL = "https://tracker.example/img/a.png?uid=RECIPIENT-TOKEN"

func testIDs() (account, message ulid.ULID) {
	return ulid.MustParse("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		ulid.MustParse("01BX5ZZKBKACTAV9WEVGEMMVRZ")
}

// TestProxyOffIsByteIdenticalToSanitizeHTML is the claim the render path depends
// on: turning the feature off changes NOTHING about what every client receives.
// It is asserted rather than assumed because this is the one code path every
// message every user opens goes through.
func TestProxyOffIsByteIdenticalToSanitizeHTML(t *testing.T) {
	for _, in := range []string{
		`<img src="` + trackerURL + `">`,
		`<img src="https://a.example/x.png" width="1" height="1">`,
		`<img srcset="https://a.example/x.png 1x, https://a.example/y.png 2x">`,
		`<p>text</p><img src="//rel.example/z.gif"><a href="https://x.example">link</a>`,
		`<img src="cid:abc"><img src="data:image/png;base64,AAA">`,
		``,
		`<img>`,
	} {
		if got, want := sanitizeHTMLWithProxy(in, nil), SanitizeHTML(in); got != want {
			t.Errorf("proxy-off output differs from SanitizeHTML\n in: %s\ngot: %s\nwant: %s", in, got, want)
		}
	}
}

// TestProxyOnRemovesTheSendersURLEntirely is the structural guarantee. The
// client is not given the real URL alongside a proxy alternative — it is not
// given the real URL at all, so no later client bug can fetch it.
func TestProxyOnRemovesTheSendersURLEntirely(t *testing.T) {
	acct, msg := testIDs()
	out := sanitizeHTMLWithProxy(`<img src="`+trackerURL+`" alt="pic">`, proxyRewriterFor(acct, msg))

	if strings.Contains(out, "tracker.example") {
		t.Errorf("the sender's HOST survived into the client's HTML: %s", out)
	}
	if strings.Contains(out, "RECIPIENT-TOKEN") {
		t.Errorf("the sender's tracking token survived into the client's HTML: %s", out)
	}
	want := proxyPath(acct, msg, ProxyTokenFor(trackerURL))
	if !strings.Contains(out, want) {
		t.Errorf("no proxy reference in the output.\ngot:  %s\nwant it to contain: %s", out, want)
	}
	// It must still be parked under the RESTORABLE name, or the client's
	// existing "show remote content" affordance would have nothing to restore
	// and the images would silently never load.
	if !strings.Contains(out, remoteSrcAttr+`="`) {
		t.Errorf("the proxy reference is not parked under %s, so the client cannot restore it: %s",
			remoteSrcAttr, out)
	}
}

// TestProxyNeverRewritesABeacon is ADR-026 §3d. Proxying a tracking pixel would
// reintroduce precisely the tracking v1.42.0 removed — the sender still learns
// the message was opened, just from a different address.
func TestProxyNeverRewritesABeacon(t *testing.T) {
	acct, msg := testIDs()
	const beacon = "https://beacon.example/open.gif"
	out := sanitizeHTMLWithProxy(
		`<img src="`+beacon+`" width="1" height="1">`, proxyRewriterFor(acct, msg))

	if strings.Contains(out, "/jmap/proxy/") {
		t.Errorf("a beacon was given a proxy reference: %s", out)
	}
	if !strings.Contains(out, pixelSrcAttr) {
		t.Errorf("a beacon lost its un-restorable parking name: %s", out)
	}
	// And the ordinary image beside it in the SAME document is still proxied —
	// otherwise this test would also pass for a proxy that did nothing at all.
	out2 := sanitizeHTMLWithProxy(
		`<img src="`+beacon+`" width="1" height="1"><img src="`+trackerURL+`">`,
		proxyRewriterFor(acct, msg))
	if !strings.Contains(out2, "/jmap/proxy/") {
		t.Fatalf("the ordinary image beside the beacon was not proxied either, so "+
			"this test cannot tell §3d from a disabled proxy: %s", out2)
	}
	// ⚠ The beacon's URL is STILL PRESENT in the output, and that is correct
	// rather than a leak: v1.42.0 parks it under a name the client has no rule
	// for restoring, which is what lets the client count suppressed trackers and
	// say so. What must never happen is it appearing under the RESTORABLE name.
	// An earlier version of this test asserted the URL was absent, which was an
	// assertion about the wrong property — it would have failed the correct
	// implementation.
	if strings.Contains(out2, remoteSrcAttr+`="`+beacon) {
		t.Errorf("the beacon's URL is parked under the restorable name: %s", out2)
	}
	if !strings.Contains(out2, pixelSrcAttr+`="`+beacon) {
		t.Errorf("the beacon's URL left its un-restorable parking name: %s", out2)
	}
}

// TestProxyDropsSrcsetRatherThanParkingIt: parking the original candidates would
// be fail-OPEN — the client restores srcset by name and the browser would fetch
// every candidate straight from the sender, defeating the proxy for exactly the
// images it covers.
func TestProxyDropsSrcsetRatherThanParkingIt(t *testing.T) {
	acct, msg := testIDs()
	in := `<img src="https://a.example/x.png" srcset="https://a.example/x.png 1x, https://a.example/y.png 2x">`
	out := sanitizeHTMLWithProxy(in, proxyRewriterFor(acct, msg))

	if strings.Contains(out, remoteSrcsetAttr) {
		t.Errorf("srcset was parked with the sender's URLs — the client would restore "+
			"them and fetch the sender directly: %s", out)
	}
	if strings.Contains(out, "y.png") {
		t.Errorf("a srcset candidate URL survived: %s", out)
	}
	// src is still proxied, so the image renders — the loss is the responsive
	// variants, not the picture.
	if !strings.Contains(out, "/jmap/proxy/") {
		t.Errorf("src was not proxied, so the image does not render at all: %s", out)
	}
	// The element is still marked blocked, or the client would not say anything
	// was withheld.
	if !strings.Contains(out, blockedAttr) {
		t.Errorf("the element lost its blocked marker: %s", out)
	}
}

// TestProxyTokenIsStableAndURLSpecific: the endpoint resolves a URL by matching
// this token against the message's own parked images, so it must be a function
// of the URL alone and must separate different URLs.
func TestProxyTokenIsStableAndURLSpecific(t *testing.T) {
	first := ProxyTokenFor(trackerURL)
	if second := ProxyTokenFor(trackerURL); first != second {
		t.Fatalf("the token is not stable for one URL: %q then %q", first, second)
	}
	if ProxyTokenFor(trackerURL) != ProxyTokenFor("  "+trackerURL+"  ") {
		t.Error("the token must not depend on surrounding whitespace: the sanitizer " +
			"parks a trimmed URL and the endpoint re-derives from the same bytes")
	}
	if ProxyTokenFor("https://a.example/1.png") == ProxyTokenFor("https://a.example/2.png") {
		t.Error("two different URLs share a token")
	}
	// A URL differing only in its query is a DIFFERENT image: a tracking token
	// is usually the only thing that differs between two recipients' URLs.
	if ProxyTokenFor("https://a.example/x.png?u=1") == ProxyTokenFor("https://a.example/x.png?u=2") {
		t.Error("two URLs differing only in the query share a token")
	}
	if n := len(ProxyTokenFor(trackerURL)); n != 32 {
		t.Errorf("token length = %d, want 32 hex characters", n)
	}
}

// TestProxyContentTypeIsAnAllowlistWithoutSVG. An SVG is a scripting context
// wearing an image's costume — the sanitizer already refuses
// data:image/svg+xml, and relaying one fetched from a stranger would undo that.
func TestProxyContentTypeIsAnAllowlistWithoutSVG(t *testing.T) {
	for _, ok := range []string{
		"image/png", "image/jpeg", "IMAGE/GIF", "image/webp; charset=binary", "image/avif",
	} {
		if got := proxyImageContentType(ok); got == "" {
			t.Errorf("%q was refused, but it is an ordinary image type", ok)
		}
	}
	for _, bad := range []string{
		"image/svg+xml",
		"image/svg+xml; charset=utf-8",
		"text/html",
		"application/octet-stream",
		"text/plain",
		"",
		"application/xhtml+xml",
	} {
		if got := proxyImageContentType(bad); got != "" {
			t.Errorf("%q was accepted as %q — it must be refused", bad, got)
		}
	}
}

// TestProxyRateLimitFires: ADR-026's Consequences require the limit in the
// design, and decision C removed the release it could otherwise have slipped
// into.
func TestProxyRateLimitFires(t *testing.T) {
	p := newImageProxy(ProxyConfig{Enabled: true, PerMinute: 3}, nil)
	user := ulid.MustParse("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	now := time.Now()
	for i := range 3 {
		if !p.allow(user, now) {
			t.Fatalf("request %d was refused before the limit", i+1)
		}
	}
	if p.allow(user, now) {
		t.Error("the 4th request in one window was allowed; the limit does not fire")
	}
	// A different user is unaffected — a shared counter would let one user's
	// mail deny another's.
	if !p.allow(ulid.MustParse("01BX5ZZKBKACTAV9WEVGEMMVRZ"), now) {
		t.Error("a second user was refused because of the first user's usage")
	}
	// The window rolls.
	if !p.allow(user, now.Add(time.Minute+time.Second)) {
		t.Error("the window never rolls, so the limit is permanent rather than per-minute")
	}
}

// TestProxyHandlerRefusesWhenDisabled: the route exists on the mux either way,
// so the handler itself has to refuse.
func TestProxyHandlerRefusesWhenDisabled(t *testing.T) {
	a := New(nil)
	acct, msg := testIDs()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", proxyPath(acct, msg, ProxyTokenFor(trackerURL)), nil)
	a.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("a disabled proxy served %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tracker.example") {
		t.Errorf("the refusal echoed the sender's host: %s", rec.Body.String())
	}
}

// TestProxyTokenShapeIsCheckable replaces a handler-level test that could not
// observe its own subject.
//
// ⚠ THE ORIGINAL WAS VACUOUS AND PROBING PROVED IT. It drove malformed tokens
// through the handler on an API with no store, so every request 404ed at the
// message lookup regardless — deleting the token check entirely left the test
// green. The rule is now a function, and this asserts the function.
func TestProxyTokenShapeIsCheckable(t *testing.T) {
	if !isProxyToken(ProxyTokenFor(trackerURL)) {
		t.Fatal("a token this package generated was rejected by its own shape check")
	}
	for _, bad := range []string{
		"",
		"short",
		strings.Repeat("z", 32),            // right length, not hex
		strings.Repeat("a", 31),            // one short
		strings.Repeat("a", 33),            // one long
		"../../../etc/passwd",              // traversal
		strings.Repeat("A", 32),            // UPPERCASE hex: the handler lowercases first
		"0123456789abcdef0123456789abcde ", // trailing space
		"0123456789abcdef0123456789abcdeG", // near-miss
	} {
		if isProxyToken(bad) {
			t.Errorf("isProxyToken(%q) = true, want false", bad)
		}
	}
}

// ---- Refusal reporting (#674) ----------------------------------------------

type stubFetcher struct {
	resp *http.Response
	err  error
}

func (f stubFetcher) Do(*http.Request) (*http.Response, error) { return f.resp, f.err }

func imgResp(status int, ctype, body string) *http.Response {
	h := http.Header{}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// errBody fails mid-stream the way egress's cappedBody does past MaxBytes.
type errBody struct{ err error }

func (b errBody) Read([]byte) (int, error) { return 0, b.err }
func (b errBody) Close() error             { return nil }

// proxyHarness builds an API whose proxy resolves to trackerURL (or refuses with
// `resolveWhy`), fetches through `client`, and records every outcome and log line.
type proxyHarness struct {
	a        *API
	outcomes []ProxyOutcome
	logs     *bytes.Buffer
	user     ulid.ULID
	msg      ulid.ULID
}

func newProxyHarness(t *testing.T, cfg ProxyConfig, client proxyFetcher, resolveWhy string) *proxyHarness {
	t.Helper()
	h := &proxyHarness{logs: &bytes.Buffer{}}
	h.user, h.msg = testIDs()
	cfg.Enabled = true
	if cfg.CacheBytes == 0 {
		cfg.CacheBytes = -1
	}
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	h.a = New(nil, withImageProxyClient(cfg, client),
		WithProxyObserver(logger, func(o ProxyOutcome) { h.outcomes = append(h.outcomes, o) }))
	h.a.proxy.resolve = func(context.Context, ulid.ULID, ulid.ULID, string) (string, string) {
		if resolveWhy != "" {
			return "", resolveWhy
		}
		return trackerURL, ""
	}
	return h
}

// do serves one request through serveProxy with the path pieces overridable.
func (h *proxyHarness) do(account, message, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/jmap/proxy/x/y/z", nil)
	req.SetPathValue("accountId", account)
	req.SetPathValue("messageId", message)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	h.a.serveProxy(rec, req, h.user)
	return rec
}

func (h *proxyHarness) doOK() *httptest.ResponseRecorder {
	return h.do(h.user.String(), h.msg.String(), ProxyTokenFor(trackerURL))
}

// TestProxyEveryRefusalIsCountedUnderItsOwnReason drives each refusal path and
// requires exactly one outcome, carrying exactly that reason.
func TestProxyEveryRefusalIsCountedUnderItsOwnReason(t *testing.T) {
	timeoutErr := context.DeadlineExceeded
	tests := []struct {
		name       string
		cfg        ProxyConfig
		client     proxyFetcher
		resolveErr string
		drive      func(h *proxyHarness) *httptest.ResponseRecorder
		reason     string
		status     int
		wantCT     string
		wantUp     int
	}{
		{name: "account", client: stubFetcher{}, reason: reasonAccount, status: 404,
			drive: func(h *proxyHarness) *httptest.ResponseRecorder {
				return h.do(h.msg.String(), h.msg.String(), ProxyTokenFor(trackerURL))
			}},
		{name: "bad message", client: stubFetcher{}, reason: reasonBadMessage, status: 404,
			drive: func(h *proxyHarness) *httptest.ResponseRecorder {
				return h.do(h.user.String(), "nope", ProxyTokenFor(trackerURL))
			}},
		{name: "bad token", client: stubFetcher{}, reason: reasonBadToken, status: 404,
			drive: func(h *proxyHarness) *httptest.ResponseRecorder {
				return h.do(h.user.String(), h.msg.String(), "short")
			}},
		{name: "resolve not allowed", client: stubFetcher{}, resolveErr: reasonNotAllowed, reason: reasonNotAllowed, status: 404},
		{name: "resolve token", client: stubFetcher{}, resolveErr: reasonTokenMissing, reason: reasonTokenMissing, status: 404},
		{name: "rate limited", cfg: ProxyConfig{PerMinute: 1}, client: stubFetcher{resp: imgResp(200, "image/png", "x")},
			reason: reasonRateLimited, status: 429,
			drive: func(h *proxyHarness) *httptest.ResponseRecorder {
				if rec := h.doOK(); rec.Code != 200 { // spends the one allowed fetch
					t.Fatalf("priming fetch got %d", rec.Code)
				}
				h.outcomes = nil
				h.logs.Reset()
				return h.doOK()
			}},
		{name: "egress refused", client: stubFetcher{err: fmt.Errorf("dial: %w", egress.ErrBlockedAddress)}, reason: reasonEgressRefused, status: 403},
		{name: "scheme refused", client: stubFetcher{err: fmt.Errorf("x: %w", egress.ErrSchemeNotPermitted)}, reason: reasonSchemeRefused, status: 403},
		{name: "redirects", client: stubFetcher{err: fmt.Errorf("x: %w", egress.ErrTooManyRedirects)}, reason: reasonTooManyRedirect, status: 502},
		{name: "timeout", client: stubFetcher{err: timeoutErr}, reason: reasonUpstreamTimeout, status: 502},
		{name: "upstream error", client: stubFetcher{err: errors.New("connection reset")}, reason: reasonUpstreamError, status: 502},
		{name: "upstream status", client: stubFetcher{resp: imgResp(403, "text/html", "no")}, reason: reasonUpstreamStatus, status: 502, wantUp: 403},
		{name: "content type", client: stubFetcher{resp: imgResp(200, "image/svg+xml; charset=utf-8", "<svg/>")},
			reason: reasonContentType, status: 415, wantCT: "image/svg+xml"},
		{name: "too large", client: stubFetcher{resp: &http.Response{StatusCode: 200,
			Header: http.Header{"Content-Type": {"image/png"}}, Body: errBody{egress.ErrTooLarge}}},
			reason: reasonTooLarge, status: 200},
		{name: "body aborted", client: stubFetcher{resp: &http.Response{StatusCode: 200,
			Header: http.Header{"Content-Type": {"image/png"}}, Body: errBody{io.ErrUnexpectedEOF}}},
			reason: reasonBodyAborted, status: 200},
	}
	seen := map[string]bool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newProxyHarness(t, tc.cfg, tc.client, tc.resolveErr)
			drive := tc.drive
			if drive == nil {
				drive = func(h *proxyHarness) *httptest.ResponseRecorder { return h.doOK() }
			}
			rec := drive(h)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d (the response must not change)", rec.Code, tc.status)
			}
			if len(h.outcomes) != 1 {
				t.Fatalf("got %d outcomes %+v, want exactly 1", len(h.outcomes), h.outcomes)
			}
			o := h.outcomes[0]
			if o.Result != ProxyRefused || o.Reason != tc.reason {
				t.Errorf("outcome = %+v, want refused/%s", o, tc.reason)
			}
			if o.Status != tc.wantUp || o.ContentType != tc.wantCT {
				t.Errorf("outcome detail = status %d ct %q, want %d %q", o.Status, o.ContentType, tc.wantUp, tc.wantCT)
			}
			if n := strings.Count(h.logs.String(), "\n"); n != 1 {
				t.Errorf("%d log lines, want exactly 1: %q", n, h.logs.String())
			}
			if !strings.Contains(h.logs.String(), "reason="+tc.reason) {
				t.Errorf("log line lacks reason=%s: %q", tc.reason, h.logs.String())
			}
			seen[tc.reason] = true
		})
	}
	// Reasons driven elsewhere: disabled and unauthorized (below), cancelled, and
	// the resolve_* ones that need a store. Every other constant must be covered.
	for _, r := range []string{reasonAccount, reasonBadMessage, reasonBadToken, reasonRateLimited,
		reasonEgressRefused, reasonSchemeRefused, reasonTooManyRedirect, reasonUpstreamTimeout,
		reasonUpstreamError, reasonUpstreamStatus, reasonContentType, reasonTooLarge, reasonBodyAborted} {
		if !seen[r] {
			t.Errorf("reason %s has no test", r)
		}
	}
}

func TestProxyDisabledAndUnauthorizedAreCounted(t *testing.T) {
	var got []ProxyOutcome
	sink := WithProxyObserver(slog.New(slog.NewTextHandler(io.Discard, nil)), func(o ProxyOutcome) { got = append(got, o) })
	acct, msg := testIDs()
	req := httptest.NewRequest("GET", proxyPath(acct, msg, ProxyTokenFor(trackerURL)), nil)

	New(nil, sink).ProxyHandler(httptest.NewRecorder(), req)
	// Enabled, but the request carries no session identity.
	rec := httptest.NewRecorder()
	New(nil, withImageProxyClient(ProxyConfig{Enabled: true}, stubFetcher{}), sink).ProxyHandler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
	if len(got) != 2 || got[0].Reason != reasonDisabled || got[1].Reason != reasonUnauthorized {
		t.Errorf("outcomes = %+v, want disabled then unauthorized", got)
	}
}

func TestProxyFailureReasonSeparatesCancelFromTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := fetchFailureReason(ctx, context.Canceled); got != reasonCancelled {
		t.Errorf("cancelled reader = %q, want %q", got, reasonCancelled)
	}
	if got := fetchFailureReason(context.Background(), context.DeadlineExceeded); got != reasonUpstreamTimeout {
		t.Errorf("deadline = %q, want %q", got, reasonUpstreamTimeout)
	}
}

func TestProxyServedAndCachedAreCounted(t *testing.T) {
	h := newProxyHarness(t, ProxyConfig{}, stubFetcher{resp: imgResp(200, "image/png", "png")}, "")
	if rec := h.doOK(); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	h.a.proxy.cacheHit = func(w http.ResponseWriter, _ *http.Request, _ ulid.ULID, _ string) bool { return true }
	h.doOK()
	if len(h.outcomes) != 2 || h.outcomes[0] != (ProxyOutcome{Result: ProxyServed}) ||
		h.outcomes[1] != (ProxyOutcome{Result: ProxyCached}) {
		t.Errorf("outcomes = %+v, want served then cached", h.outcomes)
	}
	if h.logs.Len() != 0 {
		t.Errorf("a delivered image logged %q; only refusals log", h.logs.String())
	}
}

// TestProxyRefusalLogNeverCarriesTheURL: the image URL's path and query carry
// the recipient's tracking identifier, and the message and user ids locate the
// message. None may reach the log, whichever fetch path refused. Since #922 a
// failed FETCH names the sender's host — and only the host — so the line can
// say which sender's links are failing.
func TestProxyRefusalLogNeverCarriesTheURL(t *testing.T) {
	for name, client := range map[string]proxyFetcher{
		// Go's own transport errors embed the URL; so must not reach the log.
		"transport error": stubFetcher{err: &url.Error{Op: "Get", URL: trackerURL, Err: errors.New("boom")}},
		"upstream status": stubFetcher{resp: imgResp(403, "text/html", "no")},
		"guard":           stubFetcher{err: fmt.Errorf("dial tracker.example: %w", egress.ErrBlockedAddress)},
		"content type":    stubFetcher{resp: imgResp(200, "text/html", "<p>")},
		"too large": stubFetcher{resp: &http.Response{StatusCode: 200,
			Header: http.Header{"Content-Type": {"image/png"}}, Body: errBody{egress.ErrTooLarge}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newProxyHarness(t, ProxyConfig{}, client, "")
			h.doOK()
			line := h.logs.String()
			if !strings.Contains(line, "reason=") {
				t.Fatalf("no reason= in %q", line)
			}
			if !strings.Contains(line, "host=tracker.example") {
				t.Errorf("a failed fetch did not name its host: %s", line)
			}
			for _, secret := range []string{"RECIPIENT-TOKEN", "uid=", "/img", "a.png", "https://", h.msg.String(), h.user.String()} {
				if strings.Contains(line, secret) {
					t.Errorf("log line contains %q: %s", secret, line)
				}
			}
		})
	}
}

// TestProxyAuthorizationRefusalsNeverLogAHost: a refusal that answers "may this
// reader have this image" — or one where no fetch was attempted — must not say
// which sender it concerned (#922). Each is driven through the handler; the
// resolve refusals return the URL's host nowhere, so the smuggling case below
// is what proves noteProxy itself refuses to print a host for them.
func TestProxyAuthorizationRefusalsNeverLogAHost(t *testing.T) {
	ok := stubFetcher{resp: imgResp(200, "image/png", "png")}
	cases := map[string]func(t *testing.T) *proxyHarness{
		"not allowed": func(t *testing.T) *proxyHarness {
			h := newProxyHarness(t, ProxyConfig{}, ok, reasonNotAllowed)
			h.doOK()
			return h
		},
		"token": func(t *testing.T) *proxyHarness {
			h := newProxyHarness(t, ProxyConfig{}, ok, reasonTokenMissing)
			h.doOK()
			return h
		},
		"message cap": func(t *testing.T) *proxyHarness {
			h := newProxyHarness(t, ProxyConfig{}, ok, reasonMessageCap)
			h.doOK()
			return h
		},
		"account": func(t *testing.T) *proxyHarness {
			h := newProxyHarness(t, ProxyConfig{}, ok, "")
			h.do(h.msg.String(), h.msg.String(), ProxyTokenFor(trackerURL))
			return h
		},
		"bad token": func(t *testing.T) *proxyHarness {
			h := newProxyHarness(t, ProxyConfig{}, ok, "")
			h.do(h.user.String(), h.msg.String(), "short")
			return h
		},
		"rate limited": func(t *testing.T) *proxyHarness {
			h := newProxyHarness(t, ProxyConfig{PerMinute: 1}, ok, "")
			h.doOK()
			h.logs.Reset()
			h.doOK()
			return h
		},
	}
	for name, drive := range cases {
		t.Run(name, func(t *testing.T) {
			h := drive(t)
			line := h.logs.String()
			if !strings.Contains(line, "image proxy refused") {
				t.Fatalf("no refusal was logged: %q", line)
			}
			if strings.Contains(line, "host=") || strings.Contains(line, "tracker.example") {
				t.Errorf("an authorization refusal named the host: %s", line)
			}
		})
	}

	// A caller that sets Host on a non-fetch refusal still gets no host in the
	// line: the allowlist in noteProxy is the guard, not caller discipline.
	for _, reason := range []string{reasonNotAllowed, reasonMessageMiss, reasonTokenMissing, reasonRateLimited, reasonAccount, reasonUnauthorized, reasonDisabled} {
		h := newProxyHarness(t, ProxyConfig{}, ok, "")
		h.a.noteProxy(context.Background(), ProxyOutcome{Result: ProxyRefused, Reason: reason, Host: "tracker.example"})
		if strings.Contains(h.logs.String(), "tracker.example") {
			t.Errorf("reason %s logged a smuggled host: %s", reason, h.logs.String())
		}
	}
	// CONTROL: the same call with a fetch-failure reason does print it, or the
	// loop above passes for a logger that never prints hosts at all.
	h := newProxyHarness(t, ProxyConfig{}, ok, "")
	h.a.noteProxy(context.Background(), ProxyOutcome{Result: ProxyRefused, Reason: reasonUpstreamStatus, Host: "tracker.example"})
	if !strings.Contains(h.logs.String(), "host=tracker.example") {
		t.Errorf("CONTROL: a fetch failure did not log its host: %s", h.logs.String())
	}
}

// TestLoggableHostIsTheRegistrableDomainOnly: no subdomain labels (a
// wildcard-DNS tracker puts the recipient's token there), no port, no
// userinfo, no path, no query, lowercased; IP literals and hosts the public
// suffix list cannot reduce are named by kind, not by value (#922 review M2).
func TestLoggableHostIsTheRegistrableDomainOnly(t *testing.T) {
	for in, want := range map[string]string{
		"https://CDN.Example:8443/p/a.png?token-hash=SECRET": "cdn.example",
		"https://user:pw@cdn.example/x.png":                  "cdn.example",
		"//cdn.example/x.png":                                "cdn.example",
		"https://u-8f3a91token.img.tracker.example/p.gif":    "tracker.example",
		"https://img.cdn.example.com/a.png":                  "example.com",
		"https://[2001:db8::8f3a:91]/a.png":                  "ip-literal",
		"http://localhost/a.png":                             "unregistrable",
		"not a url at all":                                   "",
	} {
		if got := loggableHost(in); got != want {
			t.Errorf("loggableHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProxyFailureLogNeverCarriesASubdomainToken is the negative control for
// M2 through the handler: a token in a subdomain label reaches neither the log
// line nor the outcome, and the registrable domain does (so the line is not
// simply empty).
func TestProxyFailureLogNeverCarriesASubdomainToken(t *testing.T) {
	const tokenURL = "https://r-SUBTOKEN922.img.tracker.example/a.png"
	h := newProxyHarness(t, ProxyConfig{}, stubFetcher{resp: imgResp(403, "text/html", "no")}, "")
	h.a.proxy.resolve = func(context.Context, ulid.ULID, ulid.ULID, string) (string, string) { return tokenURL, "" }
	h.doOK()
	line := h.logs.String()
	if strings.Contains(line, "SUBTOKEN922") || strings.Contains(line, "img.tracker") {
		t.Errorf("a subdomain token reached the log: %s", line)
	}
	if !strings.Contains(line, "host=tracker.example") {
		t.Errorf("CONTROL: the registrable domain is missing: %s", line)
	}
	for _, o := range h.outcomes {
		if strings.Contains(o.Host, "SUBTOKEN922") {
			t.Errorf("outcome carries the token: %+v", o)
		}
	}
}

// TestPrefetchDefaultPortRule: only the scheme's default port.
func TestPrefetchDefaultPortRule(t *testing.T) {
	for in, want := range map[string]bool{
		"https://cdn.example/a.png":      true,
		"https://cdn.example:443/a.png":  true,
		"http://cdn.example:80/a.png":    true,
		"http://cdn.example/a.png":       true,
		"https://cdn.example:8443/a.png": false,
		"http://cdn.example:443/a.png":   false,
		"https://cdn.example:80/a.png":   false,
		"ftp://cdn.example/a.png":        false,
		"//cdn.example/a.png":            false,
	} {
		if got := prefetchDefaultPort(in); got != want {
			t.Errorf("prefetchDefaultPort(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestBackgroundAdmissionLeavesHalfTheBudgetForTheReader: the prefetch shares
// the reader's per-minute window but stops at half of it (#922), so a burst of
// arriving mail cannot make the message being opened answer 429.
func TestBackgroundAdmissionLeavesHalfTheBudgetForTheReader(t *testing.T) {
	p := newImageProxy(ProxyConfig{PerMinute: 4}, stubFetcher{})
	user, _ := testIDs()
	now := time.Now()
	got := 0
	for i := 0; i < 10; i++ {
		if p.allowBackground(user, now) {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("background admitted %d of a 4-per-minute window, want 2", got)
	}
	// The reader still has the other half.
	for i := 0; i < 2; i++ {
		if !p.allow(user, now) {
			t.Fatalf("the reader's fetch %d was refused inside the half the prefetch must leave", i+1)
		}
	}
	if p.allow(user, now) {
		t.Fatal("the shared window let the total exceed PerMinute")
	}
	if reset := p.windowReset(user, now); !reset.After(now) {
		t.Errorf("windowReset = %v, want a time after now", reset)
	}
}

// TestOrganisationDomainUnderAPrivateSuffix: under a PRIVATE public-suffix
// entry the customer's own label is the eTLD+1 and can be a token, so the line
// names the provider's domain instead (#922 re-review). Hosts are passed bare:
// these are public-suffix fixtures, not URLs this project names.
func TestOrganisationDomainUnderAPrivateSuffix(t *testing.T) {
	for host, want := range map[string]string{
		"r-tok922secret.github.io":           "github.io",
		"deep.r-tok922secret.github.io":      "github.io",
		"r-tok922secret.workers.dev":         "workers.dev",
		"r-tok922secret.herokuapp.com":       "herokuapp.com",
		"r-tok922secret.duckdns.org":         "duckdns.org",
		"img.r-tok922secret.tracker.example": "tracker.example", // default rule: eTLD+1
		"cdn.example.com":                    "example.com",     // ICANN: eTLD+1
	} {
		got := organisationDomain(host)
		if got != want {
			t.Errorf("organisationDomain(%q) = %q, want %q", host, got, want)
		}
		if strings.Contains(got, "tok922secret") {
			t.Errorf("organisationDomain(%q) = %q carries the token label", host, got)
		}
	}
}

// TestLooseIPv4SpellingsAreIPLiterals: the IPv4 forms inet_aton accepts and
// netip rejects are addresses, and must not be logged as if they were names.
func TestLooseIPv4SpellingsAreIPLiterals(t *testing.T) {
	for _, h := range []string{"0x7f.1", "2130706433", "127.1", "0177.0.0.1", "0x7f000001"} {
		if !looseIPv4(h) {
			t.Errorf("looseIPv4(%q) = false, want true", h)
		}
	}
	// CONTROL: names are not addresses.
	for _, h := range []string{"cdn.example", "123.example", "0xbeef.example", "a.b.c.d.e"} {
		if looseIPv4(h) {
			t.Errorf("looseIPv4(%q) = true for a name", h)
		}
	}
	if got := loggableHost("http://2130706433/a.png"); got != "ip-literal" {
		t.Errorf("loggableHost of a decimal IPv4 = %q, want ip-literal", got)
	}
}

// TestPrefetchPortRefusalIsNamedPortRefused: the egress check's error, however
// wrapped by net/http on a redirect hop, is counted as port_refused.
func TestPrefetchPortRefusalIsNamedPortRefused(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://cdn.example:8443/", Err: errPrefetchPort}
	if got := fetchFailureReason(context.Background(), err); got != reasonPortRefused {
		t.Errorf("fetchFailureReason = %q, want %q", got, reasonPortRefused)
	}
}
