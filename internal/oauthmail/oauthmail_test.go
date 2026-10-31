// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

// Sentinels. Every test in this file runs with a captured logger and checks
// that none of these strings ever appears in a log line or an error string
// (ADR-046 §threat model): the log-redaction negative control.
const (
	sentAccess1     = "ya29.ACCESS-ONE-sentinel-9d1f"
	sentAccess2     = "ya29.ACCESS-TWO-sentinel-0b7e"
	sentRefresh     = "1//REFRESH-ORIGINAL-sentinel-44aa"
	sentRefreshNew  = "1//REFRESH-ROTATED-sentinel-c3e2"
	sentRefreshNew2 = "1//REFRESH-ROTATED-TWO-sentinel-77d0"
	sentClientSec   = "GOCSPX-CLIENT-SECRET-sentinel-5e5e"
)

var allSentinels = []string{sentAccess1, sentAccess2, sentRefresh, sentRefreshNew, sentRefreshNew2, sentClientSec}

// leakCheck captures everything a test logs and every error it collects, and
// fails the test if any sentinel appears in either.
type leakCheck struct {
	mu   stdsync.Mutex
	buf  bytes.Buffer
	errs []string
}

func (l *leakCheck) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *leakCheck) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *leakCheck) err(err error) error {
	if err != nil {
		l.mu.Lock()
		l.errs = append(l.errs, err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
		l.mu.Unlock()
	}
	return err
}

// leaks returns every sentinel found in the captured output.
func (l *leakCheck) leaks() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	hay := l.buf.String() + "\n" + strings.Join(l.errs, "\n")
	var found []string
	for _, s := range allSentinels {
		if strings.Contains(hay, s) {
			found = append(found, s)
		}
	}
	return found
}

func newLeakCheck(t *testing.T) *leakCheck {
	t.Helper()
	l := &leakCheck{}
	t.Cleanup(func() {
		if found := l.leaks(); len(found) > 0 {
			t.Errorf("TOKEN LEAK: %v appeared in logs or errors:\n%s\n%s", found, l.buf.String(), strings.Join(l.errs, "\n"))
		}
	})
	return l
}

// The negative control for the negative control: leakCheck must SEE a
// sentinel that is logged or returned, or every "no leak" above is vacuous.
func TestLeakCheckDetectsALeak(t *testing.T) {
	l := &leakCheck{}
	l.logger().Info("oops", "token", sentAccess1) // a raw string, not a Secret
	if got := l.leaks(); len(got) != 1 || got[0] != sentAccess1 {
		t.Fatalf("leakCheck missed a logged token: %v", got)
	}
	l2 := &leakCheck{}
	_ = l2.err(fmt.Errorf("refresh failed for %s", sentRefresh))
	if got := l2.leaks(); len(got) != 1 {
		t.Fatalf("leakCheck missed a token in an error: %v", got)
	}
	l3 := &leakCheck{}
	l3.logger().Info("fine", "token", xoauth2.Secret(sentAccess1))
	if got := l3.leaks(); len(got) != 0 {
		t.Fatalf("a Secret leaked through slog: %v", got)
	}
}

// --- fake token endpoint -------------------------------------------------------

type tokenEndpoint struct {
	t     *testing.T
	calls atomic.Int32
	// handle answers one request; forms is every form received.
	mu     stdsync.Mutex
	forms  []url.Values
	handle func(n int32, form url.Values, w http.ResponseWriter)
}

func newTokenEndpoint(t *testing.T, handle func(n int32, form url.Values, w http.ResponseWriter)) (*tokenEndpoint, *httptest.Server) {
	te := &tokenEndpoint{t: t, handle: handle}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		te.mu.Lock()
		te.forms = append(te.forms, form)
		te.mu.Unlock()
		n := te.calls.Add(1)
		te.handle(n, form, w)
	}))
	t.Cleanup(srv.Close)
	return te, srv
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func okToken(access, refresh string) func(int32, url.Values, http.ResponseWriter) {
	return func(_ int32, _ url.Values, w http.ResponseWriter) {
		body := map[string]any{"access_token": access, "expires_in": 3599, "token_type": "Bearer",
			"scope": "https://mail.google.com/"}
		if refresh != "" {
			body["refresh_token"] = refresh
		}
		writeJSON(w, http.StatusOK, body)
	}
}

func oauthError(status int, code, desc string) func(int32, url.Values, http.ResponseWriter) {
	return func(_ int32, _ url.Values, w http.ResponseWriter) {
		writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
	}
}

func testProvider(tokenURL string, name string) Provider {
	var p Provider
	if name == ProviderMicrosoft {
		p, _ = Microsoft(Client{ID: "client-id", Secret: sentClientSec}, "common")
	} else {
		p = Google(Client{ID: "client-id", Secret: sentClientSec})
	}
	p.TokenURL = tokenURL
	return p
}

// --- Provider.Refresh ------------------------------------------------------------

func TestRefreshSuccessAndRequestShape(t *testing.T) {
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, okToken(sentAccess1, ""))
	resp, err := testProvider(srv.URL, ProviderGoogle).Refresh(context.Background(), srv.Client(), sentRefresh)
	if lc.err(err) != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if resp.AccessToken.Reveal() != sentAccess1 || !resp.RefreshToken.Empty() {
		t.Errorf("resp = access %v refresh-empty %v", resp.AccessToken.Reveal() == sentAccess1, resp.RefreshToken.Empty())
	}
	if resp.ExpiresIn != 3599*time.Second {
		t.Errorf("ExpiresIn = %v", resp.ExpiresIn)
	}
	f := te.forms[0]
	if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != sentRefresh ||
		f.Get("client_id") != "client-id" || f.Get("client_secret") != sentClientSec {
		t.Errorf("token request form is wrong (keys: %v)", keys(f))
	}
	if f.Has("scope") {
		t.Error("Google refresh sent a scope")
	}

	// Microsoft sends the scope, and returns a rotated refresh token.
	te2, srv2 := newTokenEndpoint(t, okToken(sentAccess2, sentRefreshNew))
	resp2, err := testProvider(srv2.URL, ProviderMicrosoft).Refresh(context.Background(), srv2.Client(), sentRefresh)
	if lc.err(err) != nil {
		t.Fatal(err)
	}
	if resp2.RefreshToken.Reveal() != sentRefreshNew {
		t.Error("rotated refresh token not returned")
	}
	if !strings.Contains(te2.forms[0].Get("scope"), "IMAP.AccessAsUser.All") {
		t.Errorf("Microsoft refresh scope = %q", te2.forms[0].Get("scope"))
	}
}

func keys(v url.Values) []string {
	var out []string
	for k := range v {
		out = append(out, k)
	}
	return out
}

func TestRefreshFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		handle   func(int32, url.Values, http.ResponseWriter)
		class    FailureClass
		code     string
		sentinel error
	}{
		{"invalid_grant", oauthError(400, "invalid_grant", "Token has been expired or revoked."),
			FailureNeedsReauth, "invalid_grant", xoauth2.ErrReauthRequired},
		{"invalid_client", oauthError(401, "invalid_client", "The OAuth client was not found."),
			FailureOperator, "invalid_client", xoauth2.ErrProviderMisconfigured},
		{"unauthorized_client", oauthError(400, "unauthorized_client", "AADSTS7000222: client secret expired"),
			FailureOperator, "unauthorized_client", xoauth2.ErrProviderMisconfigured},
		{"invalid_request", oauthError(400, "invalid_request", "missing parameter"),
			FailureOperator, "invalid_request", xoauth2.ErrProviderMisconfigured},
		{"5xx", func(_ int32, _ url.Values, w http.ResponseWriter) {
			http.Error(w, "upstream", http.StatusServiceUnavailable)
		},
			FailureTransient, "http_503", xoauth2.ErrTokenUnavailable},
		{"5xx with OAuth body", oauthError(500, "server_error", "try later"),
			FailureTransient, "server_error", xoauth2.ErrTokenUnavailable},
		{"429", func(_ int32, _ url.Values, w http.ResponseWriter) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		},
			FailureTransient, "http_429", xoauth2.ErrTokenUnavailable},
		{"temporarily_unavailable", oauthError(400, "temporarily_unavailable", ""),
			FailureTransient, "temporarily_unavailable", xoauth2.ErrTokenUnavailable},
		{"unreadable 4xx (a proxy page)", func(_ int32, _ url.Values, w http.ResponseWriter) {
			http.Error(w, "<html>blocked</html>", http.StatusForbidden)
		}, FailureTransient, "http_403", xoauth2.ErrTokenUnavailable},
		{"200 without a token", func(_ int32, _ url.Values, w http.ResponseWriter) {
			writeJSON(w, 200, map[string]any{"expires_in": 3600})
		}, FailureTransient, "malformed_response", xoauth2.ErrTokenUnavailable},
		{"hostile code", oauthError(400, "Invalid Grant <script>", "x"),
			FailureOperator, "unrecognised_error", xoauth2.ErrProviderMisconfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLeakCheck(t)
			_, srv := newTokenEndpoint(t, tc.handle)
			_, err := testProvider(srv.URL, ProviderGoogle).Refresh(context.Background(), srv.Client(), sentRefresh)
			_ = lc.err(err)
			var rerr *RefreshError
			if !errors.As(err, &rerr) {
				t.Fatalf("err = %v, want *RefreshError", err)
			}
			if rerr.Class != tc.class || rerr.Code != tc.code {
				t.Errorf("class/code = %v/%q, want %v/%q", rerr.Class, rerr.Code, tc.class, tc.code)
			}
			// Exactly one sentinel matches.
			n := 0
			for _, s := range []error{xoauth2.ErrReauthRequired, xoauth2.ErrProviderMisconfigured, xoauth2.ErrTokenUnavailable} {
				if errors.Is(err, s) {
					n++
					if s != tc.sentinel {
						t.Errorf("errors.Is matched %v, want %v", s, tc.sentinel)
					}
				}
			}
			if n != 1 {
				t.Errorf("err matched %d sentinels, want exactly 1", n)
			}
		})
	}
}

// A provider that ECHOES the request's secrets in its error description must
// not make us leak them.
func TestRefreshScrubsEchoedSecrets(t *testing.T) {
	lc := newLeakCheck(t)
	_, srv := newTokenEndpoint(t, func(_ int32, f url.Values, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant",
			"error_description": "bad token " + f.Get("refresh_token") + " for client secret " + f.Get("client_secret")})
	})
	_, err := testProvider(srv.URL, ProviderGoogle).Refresh(context.Background(), srv.Client(), sentRefresh)
	_ = lc.err(err)
	if err == nil || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err = %v, want a scrubbed description", err)
	}
}

func TestRefreshTimeoutIsTransient(t *testing.T) {
	lc := newLeakCheck(t)
	release := make(chan struct{})
	_, srv := newTokenEndpoint(t, func(int32, url.Values, http.ResponseWriter) { <-release })
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := testProvider(srv.URL, ProviderGoogle).Refresh(ctx, srv.Client(), sentRefresh)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrTokenUnavailable) {
		t.Fatalf("err = %v, want transient", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap the deadline", err)
	}
}

// --- Manager over a fake grant store ------------------------------------------------

// fakeGrants is an in-memory GrantStore with the real compare-and-swap rule.
type fakeGrants struct {
	mu        stdsync.Mutex
	g         Grant
	rotations int
	casLosses int
	refreshes int
	states    []string
}

func (f *fakeGrants) LoadGrant(context.Context, ulid.ULID, ulid.ULID) (Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.g, nil
}

func (f *fakeGrants) RotateRefreshToken(_ context.Context, _, _ ulid.ULID, seen int64, tok xoauth2.Secret, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.g.Generation != seen {
		f.casLosses++
		return false, nil
	}
	f.g.Generation++
	f.g.RefreshToken = tok
	f.rotations++
	return true, nil
}

func (f *fakeGrants) RecordRefresh(context.Context, ulid.ULID, ulid.ULID, time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	return nil
}

func (f *fakeGrants) SetState(_ context.Context, _, _ ulid.ULID, state, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.g.State != state {
		f.states = append(f.states, state)
	}
	f.g.State = state
	return nil
}

func (f *fakeGrants) snapshot() fakeGrants {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeGrants{g: f.g, rotations: f.rotations, casLosses: f.casLosses, refreshes: f.refreshes,
		states: append([]string(nil), f.states...)}
}

func newFakeGrants(provider string) *fakeGrants {
	return &fakeGrants{g: Grant{Provider: provider, Address: "you@example.com", State: store.OAuthStateActive,
		Generation: 1, RefreshToken: sentRefresh}}
}

func newManager(t *testing.T, lc *leakCheck, srv *httptest.Server, grants GrantStore, provider string, clock func() time.Time) *Manager {
	t.Helper()
	reg, err := NewRegistry(testProvider(srv.URL, provider))
	if err != nil {
		t.Fatal(err)
	}
	opts := []ManagerOption{WithHTTPClient(srv.Client()), WithLogger(lc.logger())}
	if clock != nil {
		opts = append(opts, WithClock(clock))
	}
	return NewManager(reg, grants, opts...)
}

var (
	userA = ulid.MustParse("01JB0000000000000000000001")
	acctA = ulid.MustParse("01JB00000000000000000000A1")
)

func TestManagerCachesUntilNearExpiry(t *testing.T) {
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		okToken([]string{sentAccess1, sentAccess2}[min(int(n)-1, 1)], "")(n, f, w)
	})
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var mu stdsync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	grants := newFakeGrants(ProviderGoogle)
	m := newManager(t, lc, srv, grants, ProviderGoogle, clock)

	for range 3 {
		tok, err := m.AccessToken(context.Background(), userA, acctA)
		if lc.err(err) != nil || tok.Reveal() != sentAccess1 {
			t.Fatalf("AccessToken = %v", err)
		}
	}
	if te.calls.Load() != 1 {
		t.Fatalf("token endpoint called %d times for three lookups, want 1", te.calls.Load())
	}
	if s := grants.snapshot(); s.refreshes != 1 || s.rotations != 0 {
		t.Errorf("refreshes/rotations = %d/%d, want 1/0 (Google does not rotate)", s.refreshes, s.rotations)
	}
	// 3599s lifetime, 5-minute skew: at +54m it is still served from cache,
	// at +55m it is refreshed.
	mu.Lock()
	now = now.Add(54 * time.Minute)
	mu.Unlock()
	if tok, _ := m.AccessToken(context.Background(), userA, acctA); tok.Reveal() != sentAccess1 || te.calls.Load() != 1 {
		t.Fatalf("refreshed too early (calls=%d)", te.calls.Load())
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	tok, err := m.AccessToken(context.Background(), userA, acctA)
	if lc.err(err) != nil || tok.Reveal() != sentAccess2 || te.calls.Load() != 2 {
		t.Fatalf("not refreshed inside the skew (calls=%d, err=%v)", te.calls.Load(), err)
	}

	// Invalidate drops a rejected token so the next call refreshes again; a
	// stale rejection of a token already replaced is ignored.
	m.Invalidate(userA, acctA, sentAccess1)
	if _, _ = m.AccessToken(context.Background(), userA, acctA); te.calls.Load() != 2 {
		t.Fatalf("Invalidate of an old token caused a refresh")
	}
	m.Invalidate(userA, acctA, sentAccess2)
	if _, _ = m.AccessToken(context.Background(), userA, acctA); te.calls.Load() != 3 {
		t.Fatalf("Invalidate of the current token did not cause a refresh")
	}

	// The cache is per (user, account): another user asking for the same
	// account id is not handed this user's token from the cache.
	other := ulid.MustParse("01JB0000000000000000000002")
	if _, _ = m.AccessToken(context.Background(), other, acctA); te.calls.Load() != 4 {
		t.Errorf("a different user was served from this user's cache")
	}
}

// Two concurrent callers → ONE network refresh, and both get the token.
func TestManagerSingleflight(t *testing.T) {
	lc := newLeakCheck(t)
	gate := make(chan struct{})
	arrived := make(chan struct{}, 8)
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		arrived <- struct{}{}
		<-gate
		okToken(sentAccess1, sentRefreshNew)(n, f, w)
	})
	grants := newFakeGrants(ProviderMicrosoft)
	m := newManager(t, lc, srv, grants, ProviderMicrosoft, nil)

	const callers = 5
	var wg stdsync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := m.AccessToken(context.Background(), userA, acctA)
			results[i], errs[i] = tok.Reveal(), lc.err(err)
		}()
	}
	<-arrived // the first refresh is in flight
	// Give the other callers time to join the flight (or, if singleflight were
	// missing, to reach the endpoint themselves).
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	if n := te.calls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times for %d concurrent callers, want 1", n, callers)
	}
	for i := range callers {
		if errs[i] != nil || results[i] != sentAccess1 {
			t.Errorf("caller %d: token ok=%v err=%v", i, results[i] == sentAccess1, errs[i])
		}
	}
	if s := grants.snapshot(); s.rotations != 1 || s.g.RefreshToken.Reveal() != sentRefreshNew || s.g.Generation != 2 {
		t.Errorf("rotations=%d generation=%d stored-new=%v, want 1/2/true",
			s.rotations, s.g.Generation, s.g.RefreshToken.Reveal() == sentRefreshNew)
	}
}

// Cross-process race: two Managers (two processes) over ONE grant store both
// refresh. The first to store its rotated token wins; the second loses the CAS,
// keeps its own (valid) access token, and does NOT overwrite the winner's
// refresh token with its own.
func TestManagerCASLossKeepsTheWinnersToken(t *testing.T) {
	lc := newLeakCheck(t)
	grants := newFakeGrants(ProviderMicrosoft)
	_, srvA := newTokenEndpoint(t, okToken(sentAccess1, sentRefreshNew))
	mA := newManager(t, lc, srvA, grants, ProviderMicrosoft, nil)
	// B has read the grant (generation 1) and is waiting on ITS token endpoint
	// when A completes a whole refresh-and-rotate; only then does B's answer
	// arrive. B must store nothing: the generation it read is stale.
	_, srvB := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		tok, err := mA.AccessToken(context.Background(), userA, acctA)
		if lc.err(err) != nil || tok.Reveal() != sentAccess1 {
			t.Errorf("A's refresh: %v", err)
		}
		okToken(sentAccess2, sentRefreshNew2)(n, f, w)
	})
	mB := newManager(t, lc, srvB, grants, ProviderMicrosoft, nil)

	tokB, err := mB.AccessToken(context.Background(), userA, acctA)
	if lc.err(err) != nil {
		t.Fatalf("B's refresh failed on a lost CAS: %v", err)
	}
	if tokB.Reveal() != sentAccess2 {
		t.Error("B did not keep the access token it was issued")
	}
	s := grants.snapshot()
	if s.rotations != 1 || s.casLosses != 1 {
		t.Fatalf("rotations/casLosses = %d/%d, want 1/1", s.rotations, s.casLosses)
	}
	if s.g.RefreshToken.Reveal() != sentRefreshNew || s.g.Generation != 2 {
		t.Errorf("stored refresh token is %s at generation %d; the winner's must survive",
			map[bool]string{true: "the winner's", false: "NOT the winner's"}[s.g.RefreshToken.Reveal() == sentRefreshNew], s.g.Generation)
	}
}

func TestManagerInvalidGrantNeedsReauthAndStopsAsking(t *testing.T) {
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, oauthError(400, "invalid_grant", "Token has been expired or revoked."))
	grants := newFakeGrants(ProviderGoogle)
	m := newManager(t, lc, srv, grants, ProviderGoogle, nil)

	_, err := m.AccessToken(context.Background(), userA, acctA)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrReauthRequired", err)
	}
	if s := grants.snapshot(); s.g.State != store.OAuthStateNeedsReauth || len(s.states) != 1 {
		t.Fatalf("state = %q (transitions %v), want one move to needs_reauth", s.g.State, s.states)
	}
	// Asking again makes NO network request: the provider already answered.
	_, err = m.AccessToken(context.Background(), userA, acctA)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrReauthRequired) || te.calls.Load() != 1 {
		t.Fatalf("second call: err=%v calls=%d, want reauth and still 1 call", err, te.calls.Load())
	}
}

func TestManagerInvalidClientIsOperatorErrorAndRecovers(t *testing.T) {
	lc := newLeakCheck(t)
	broken := atomic.Bool{}
	broken.Store(true)
	_, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		if broken.Load() {
			oauthError(401, "invalid_client", "Unauthorized")(n, f, w)
			return
		}
		okToken(sentAccess1, "")(n, f, w)
	})
	grants := newFakeGrants(ProviderGoogle)
	m := newManager(t, lc, srv, grants, ProviderGoogle, nil)

	_, err := m.AccessToken(context.Background(), userA, acctA)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrProviderMisconfigured) || errors.Is(err, xoauth2.ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrProviderMisconfigured and not reauth", err)
	}
	if s := grants.snapshot(); s.g.State != store.OAuthStateOperatorError {
		t.Fatalf("state = %q, want operator_error", s.g.State)
	}
	if !strings.Contains(lc.buf.String(), `"level":"ERROR"`) || !strings.Contains(lc.buf.String(), "invalid_client") {
		t.Errorf("operator error was not logged at ERROR with its code: %s", lc.buf.String())
	}
	// The operator fixes the client: the next refresh succeeds and the grant
	// returns to active with nobody reconnecting.
	broken.Store(false)
	tok, err := m.AccessToken(context.Background(), userA, acctA)
	if lc.err(err) != nil || tok.Reveal() != sentAccess1 {
		t.Fatalf("after the fix: %v", err)
	}
	if s := grants.snapshot(); s.g.State != store.OAuthStateActive {
		t.Fatalf("state = %q, want active again", s.g.State)
	}
}

func TestManagerTransientLeavesStateAlone(t *testing.T) {
	lc := newLeakCheck(t)
	_, srv := newTokenEndpoint(t, func(_ int32, _ url.Values, w http.ResponseWriter) { http.Error(w, "x", http.StatusBadGateway) })
	grants := newFakeGrants(ProviderGoogle)
	m := newManager(t, lc, srv, grants, ProviderGoogle, nil)
	_, err := m.AccessToken(context.Background(), userA, acctA)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrTokenUnavailable) {
		t.Fatalf("err = %v, want transient", err)
	}
	if s := grants.snapshot(); s.g.State != store.OAuthStateActive || len(s.states) != 0 {
		t.Fatalf("a transient failure changed the grant state: %v", s.states)
	}
}

func TestManagerProviderNotConfigured(t *testing.T) {
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, okToken(sentAccess1, ""))
	grants := newFakeGrants(ProviderMicrosoft) // registry below only has google
	m := newManager(t, lc, srv, grants, ProviderGoogle, nil)
	_, err := m.AccessToken(context.Background(), userA, acctA)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrProviderMisconfigured) {
		t.Fatalf("err = %v, want ErrProviderMisconfigured", err)
	}
	if te.calls.Load() != 0 {
		t.Error("a token request went to a provider the grant does not belong to")
	}
	if s := grants.snapshot(); s.g.State != store.OAuthStateOperatorError {
		t.Errorf("state = %q, want operator_error", s.g.State)
	}
}

// A caller that gives up does not fail the shared refresh for the others.
func TestManagerCallerCancellationIsPrivate(t *testing.T) {
	lc := newLeakCheck(t)
	gate := make(chan struct{})
	arrived := make(chan struct{}, 2)
	_, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		arrived <- struct{}{}
		<-gate
		okToken(sentAccess1, "")(n, f, w)
	})
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.AccessToken(ctx, userA, acctA)
		done <- lc.err(err)
	}()
	<-arrived
	other := make(chan error, 1)
	go func() {
		tok, err := m.AccessToken(context.Background(), userA, acctA)
		if err == nil && tok.Reveal() != sentAccess1 {
			err = errors.New("wrong token")
		}
		other <- lc.err(err)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, xoauth2.ErrTokenUnavailable) {
		t.Errorf("cancelled caller: %v", err)
	}
	close(gate)
	if err := <-other; err != nil {
		t.Errorf("the other caller failed because the first gave up: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	ms, err := Microsoft(Client{ID: "id", Secret: "s"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ms.TokenURL, "/common/") {
		t.Errorf("default tenant: %s", ms.TokenURL)
	}
	for _, bad := range []string{"common/../evil", "a b", "x?y=1", "-lead", "évil"} {
		if _, err := Microsoft(Client{ID: "id", Secret: "s"}, bad); err == nil {
			t.Errorf("tenant %q accepted", bad)
		}
	}
	reg, err := NewRegistry(Google(Client{ID: "id"}), ms) // Google has no secret
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Enabled(); len(got) != 1 || got[0] != ProviderMicrosoft {
		t.Errorf("Enabled = %v, want only microsoft", got)
	}
	if _, ok := reg.Lookup(ProviderGoogle); ok {
		t.Error("a provider without a client secret is enabled")
	}
	if _, err := NewRegistry(ms, ms); err == nil {
		t.Error("duplicate provider accepted")
	}
	g := Google(Client{ID: "id", Secret: "s"})
	if g.IMAPHost != "imap.gmail.com" || g.SMTPHost != "smtp.gmail.com" || !g.SMTPImplicitTLS {
		t.Errorf("google endpoints: %+v", g)
	}
	if ms.IMAPHost != "outlook.office365.com" || ms.SMTPPort != 587 || ms.SMTPImplicitTLS {
		t.Errorf("microsoft endpoints: %+v", ms)
	}
	// A Provider printed whole does not show the client secret.
	if s := fmt.Sprintf("%+v", Google(Client{ID: "id", Secret: sentClientSec})); strings.Contains(s, sentClientSec) {
		t.Errorf("client secret rendered: %s", s)
	}
}

func TestFlowVerifierSealing(t *testing.T) {
	master := bytes.Repeat([]byte{7}, 32)
	u1, u2 := ulid.Make(), ulid.Make()
	f1, f2 := ulid.Make(), ulid.Make()
	ct, err := SealFlowVerifier(master, u1, f1, "verifier-abc")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("verifier-abc")) {
		t.Fatal("verifier stored in the clear")
	}
	v, err := OpenFlowVerifier(master, u1, f1, ct)
	if err != nil || v.Reveal() != "verifier-abc" {
		t.Fatalf("open: %v", err)
	}
	if _, err := OpenFlowVerifier(master, u2, f1, ct); err == nil {
		t.Error("another user's key opened the verifier")
	}
	if _, err := OpenFlowVerifier(master, u1, f2, ct); err == nil {
		t.Error("the verifier opened under another flow id")
	}
}
