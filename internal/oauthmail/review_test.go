// SPDX-License-Identifier: AGPL-3.0-only

package oauthmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailroom/internal/xoauth2"
)

// Tests for the fixes from the security review of #894.

// L2: OIDC's "come back and sign in" codes mean the user must act, not the
// operator.
func TestRefreshInteractionCodesNeedReauth(t *testing.T) {
	for _, code := range []string{"interaction_required", "login_required", "consent_required"} {
		t.Run(code, func(t *testing.T) {
			lc := newLeakCheck(t)
			_, srv := newTokenEndpoint(t, oauthError(400, code, "AADSTS50076: step-up required"))
			_, err := testProvider(srv.URL, ProviderMicrosoft).Refresh(context.Background(), srv.Client(), sentRefresh)
			_ = lc.err(err)
			if !errors.Is(err, xoauth2.ErrReauthRequired) {
				t.Fatalf("%s = %v, want ErrReauthRequired", code, err)
			}
		})
	}
}

// L1: a redirecting token endpoint is never followed — the next hop would get
// the refresh token and the client secret — and the answer is transient. Both
// a caller-supplied client and the private default.
func TestRefreshNeverFollowsRedirects(t *testing.T) {
	lc := newLeakCheck(t)
	elsewhere, other := newTokenEndpoint(t, okToken(sentAccess1, ""))
	_, srv := newTokenEndpoint(t, func(_ int32, _ url.Values, w http.ResponseWriter) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	for _, hc := range []*http.Client{srv.Client(), nil} {
		_, err := testProvider(srv.URL, ProviderGoogle).Refresh(context.Background(), hc, sentRefresh)
		_ = lc.err(err)
		var rerr *RefreshError
		if !errors.As(err, &rerr) || rerr.Class != FailureTransient || rerr.Code != "http_3xx" {
			t.Fatalf("redirect = %v, want transient http_3xx", err)
		}
	}
	if n := elsewhere.calls.Load(); n != 0 {
		t.Fatalf("the redirect target received %d token requests", n)
	}
}

// L4: a stated lifetime is clamped to [30s, 24h], overflow included.
func TestParseExpiresInClamps(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`3599`, 3599 * time.Second},
		{`"3600"`, time.Hour},
		{`1000000000000`, 24 * time.Hour},
		{`1e30`, 24 * time.Hour},
		{`9223372036854775807`, 24 * time.Hour},
		{`-5`, 30 * time.Second},
		{`-1e30`, 30 * time.Second},
		{`0`, 30 * time.Second},
		{`5`, 30 * time.Second},
		{`"soon"`, defaultAccessLifetime},
		{``, defaultAccessLifetime},
	} {
		if got := parseExpiresIn(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("parseExpiresIn(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// L4 end to end: a provider claiming 10^12 seconds neither overflows the
// cache expiry into the past nor keeps the token cached beyond 24h.
func TestHugeExpiresInIsCachedAtMostADay(t *testing.T) {
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, func(_ int32, _ url.Values, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"access_token": sentAccess1, "expires_in": 1000000000000})
	})
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, func() time.Time { return clock })
	for range 2 {
		if _, err := m.AccessToken(context.Background(), userA, acctA); lc.err(err) != nil {
			t.Fatal(err)
		}
	}
	if te.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", te.calls.Load())
	}
	clock = now.Add(24 * time.Hour)
	if _, err := m.AccessToken(context.Background(), userA, acctA); lc.err(err) != nil {
		t.Fatal(err)
	}
	if te.calls.Load() != 2 {
		t.Fatalf("a token claiming 10^12 s was still cached after 24h (calls=%d)", te.calls.Load())
	}
}

// MEDIUM-1: the Manager keeps access tokens in an unexported map of
// unexported structs, which fmt and slog render by reflection WITHOUT calling
// any method. Formatting the Manager, a cachedToken, or a struct nesting one
// in an unexported field must still show no token.
func TestCachedTokensNeverRender(t *testing.T) {
	lc := newLeakCheck(t)
	_, srv := newTokenEndpoint(t, okToken(sentAccess1, ""))
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, nil)
	if _, err := m.AccessToken(context.Background(), userA, acctA); lc.err(err) != nil {
		t.Fatal(err)
	}
	// A snapshot of the Manager's cache in the same shape (an unexported map
	// of unexported structs, in an unexported field). Formatting the live
	// Manager itself would race with singleflight's own bookkeeping.
	type managerLike struct{ cache map[cacheKey]cachedToken }
	m.mu.Lock()
	ct := m.cache[cacheKey{userA, acctA}]
	snap := managerLike{cache: map[cacheKey]cachedToken{{userA, acctA}: ct}}
	m.mu.Unlock()
	if ct.token.Get().Reveal() != sentAccess1 {
		t.Fatal("control: the token is not in the cache, so this test would see nothing")
	}
	type outer struct{ inner cachedToken }
	var outs []string
	for _, v := range []any{ct, &ct, outer{ct}, snap, &snap} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			outs = append(outs, fmt.Sprintf(verb, v))
		}
	}
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		slog.New(h).Info("x", "ct", ct, "outer", outer{ct}, "m", snap)
	}
	outs = append(outs, buf.String())
	for _, o := range outs {
		if strings.Contains(o, sentAccess1) {
			t.Errorf("an access token rendered: %s", o)
		}
	}
}

func TestManagerHostsFor(t *testing.T) {
	lc := newLeakCheck(t)
	_, srv := newTokenEndpoint(t, okToken(sentAccess1, ""))
	m := newManager(t, lc, srv, newFakeGrants(ProviderGoogle), ProviderGoogle, nil)
	h, ok := m.HostsFor(ProviderGoogle)
	if !ok || h.IMAPHost != "imap.gmail.com" || h.SMTPHost != "smtp.gmail.com" || h.SMTPPort != 465 || !h.SMTPImplicitTLS {
		t.Errorf("HostsFor(google) = %+v, %v", h, ok)
	}
	if _, ok := m.HostsFor(ProviderMicrosoft); ok {
		t.Error("an unconfigured provider has hosts")
	}
}
