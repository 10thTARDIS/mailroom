// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Signed-in devices end to end (#937, ADR-048 sections 5 and 6): the REAL
// Authenticator middleware in front of the REAL API over PostgreSQL, with a
// stand-in identity provider that publishes a revocation endpoint.
//
// What is proven here and cannot be proven by the API package's own tests:
//
//  1. DESTROYING A DEVICE ENDS IT ON ITS VERY NEXT REQUEST: a 401 from the
//     middleware, not at its next renewal. The current session included.
//  2. THE PROVIDER IS TOLD. The refresh token that was sealed in the row is
//     opened by the adapter and presented to the provider's revocation endpoint,
//     for a device destroy (detached) and for a logout (synchronous).
//  3. A FAILING PROVIDER BLOCKS NOTHING.
//  4. THE DISPLAY ADDRESS reaches the session row through the trusted-proxy rule,
//     and an untrusted peer cannot choose it.
//  5. THE SWEEP, wired as the daily job does, reports every reason.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// devicesRig is a renewRig whose provider revokes, behind the real middleware
// and API.
type devicesRig struct {
	*renewRig
	authn   *auth.Authenticator
	handler http.Handler
}

func newDevicesRig(t *testing.T, adjust func(*auth.Config)) *devicesRig {
	t.Helper()
	rig := newRenewRig(t, false, nil)
	rig.idp.set(func(p *fakeIDP) { p.withRevocation = true })
	authn := rig.processWith(adjust)

	revoker := newProviderRevoker(context.Background(), newSessionStore(rig.st, rig.keys), quietLogger())
	revoker.set(authn)
	svc := api.New(rig.st, api.WithSessionRevoker(revoker.revoke))
	return &devicesRig{renewRig: rig, authn: authn, handler: authn.Middleware(svc.Handler())}
}

// jmap runs one method call as the session with the given cookie secret and
// returns the HTTP status and the method's response arguments.
func (d *devicesRig) jmap(t *testing.T, secret, method string, args map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:mailroom:devices"},
		"methodCalls": [][]any{{method, args, "c0"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/jmap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: secret})
	rr := httptest.NewRecorder()
	d.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		return rr.Code, nil
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || len(out.MethodResponses) != 1 {
		t.Fatalf("%s: bad envelope %s (%v)", method, rr.Body.String(), err)
	}
	var res map[string]any
	if err := json.Unmarshal(out.MethodResponses[0][1], &res); err != nil {
		t.Fatalf("%s: bad response %s", method, out.MethodResponses[0][1])
	}
	if name := strings.Trim(string(out.MethodResponses[0][0]), `"`); name == "error" {
		t.Fatalf("%s answered an error: %s", method, out.MethodResponses[0][1])
	}
	return rr.Code, res
}

// idFor reads the session id of the cookie secret directly.
func (d *devicesRig) idFor(t *testing.T, secret string) ulid.ULID {
	t.Helper()
	ref, ok, err := d.st.LookupSession(context.Background(), hashSecret(secret))
	if err != nil || !ok {
		t.Fatalf("lookup %q: ok=%v err=%v", secret, ok, err)
	}
	return ref.ID
}

func (d *devicesRig) revokedTokens() []string {
	d.idp.mu.Lock()
	defer d.idp.mu.Unlock()
	return append([]string(nil), d.idp.revoked...)
}

// waitRevoked waits for the detached revocation of token to reach the provider.
func (d *devicesRig) waitRevoked(t *testing.T, token string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range d.revokedTokens() {
			if got == token {
				return true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestDevices_DestroyEndsTheDeviceOnItsNextRequestAndTellsTheProvider(t *testing.T) {
	d := newDevicesRig(t, nil)
	current := d.secret
	otherSecret := d.addSession("rt-other")
	thirdSecret := d.addSession("rt-third")

	// The list, from the current device.
	code, res := d.jmap(t, current, "SignedInDevice/get", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("get: status %d", code)
	}
	list, _ := res["list"].([]any)
	if len(list) != 3 {
		t.Fatalf("list has %d devices, want 3: %v", len(list), res)
	}
	currentCount := 0
	for _, it := range list {
		if it.(map[string]any)["isCurrent"] == true {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Fatalf("%d entries are current, want exactly 1", currentCount)
	}

	// Positive control: every device works before anything is destroyed.
	for name, s := range map[string]string{"current": current, "other": otherSecret, "third": thirdSecret} {
		if code := d.requestWith(d.authn, s); code != http.StatusOK {
			t.Fatalf("%s device: status %d before the destroy", name, code)
		}
	}

	// Destroy "other" by id: its very next request is a 401, the others are fine.
	otherID := d.idFor(t, otherSecret)
	_, res = d.jmap(t, current, "SignedInDevice/set", map[string]any{"destroy": []string{otherID.String()}})
	if got, _ := res["destroyed"].([]any); len(got) != 1 || got[0] != otherID.String() {
		t.Fatalf("destroyed = %v, want [%s]", res["destroyed"], otherID)
	}
	if code := d.requestWith(d.authn, otherSecret); code != http.StatusUnauthorized {
		t.Fatalf("the destroyed device's next request = %d, want 401", code)
	}
	if code := d.requestWith(d.authn, current); code != http.StatusOK {
		t.Fatalf("the current device = %d after destroying another, want 200", code)
	}
	if !d.waitRevoked(t, "rt-other") {
		t.Fatalf("the provider was never asked to revoke the destroyed device's refresh token (saw %v)", d.revokedTokens())
	}
	for _, tok := range d.revokedTokens() {
		if tok == "rt-third" || tok == "rt-start" {
			t.Errorf("the provider was asked to revoke %q, a device that was not destroyed", tok)
		}
	}

	// *others*: the third goes, the current stays.
	_, res = d.jmap(t, current, "SignedInDevice/set", map[string]any{"destroy": []string{"*others*"}})
	if got, _ := res["destroyed"].([]any); len(got) != 1 {
		t.Fatalf("*others* destroyed %v, want just the third device", res["destroyed"])
	}
	if code := d.requestWith(d.authn, thirdSecret); code != http.StatusUnauthorized {
		t.Fatalf("a device signed out by *others* = %d, want 401", code)
	}
	if code := d.requestWith(d.authn, current); code != http.StatusOK {
		t.Fatalf("*others* signed out the current device (%d)", code)
	}
	if !d.waitRevoked(t, "rt-third") {
		t.Fatalf("*others* did not revoke the third device at the provider (saw %v)", d.revokedTokens())
	}

	// Destroying the CURRENT session by id succeeds; the NEXT request is a 401.
	currentID := d.idFor(t, current)
	code, res = d.jmap(t, current, "SignedInDevice/set", map[string]any{"destroy": []string{currentID.String()}})
	if code != http.StatusOK {
		t.Fatalf("destroying the current session: status %d, want the request itself to succeed", code)
	}
	if got, _ := res["destroyed"].([]any); len(got) != 1 || got[0] != currentID.String() {
		t.Fatalf("destroyed = %v, want the current session", res["destroyed"])
	}
	if code := d.requestWith(d.authn, current); code != http.StatusUnauthorized {
		t.Fatalf("the next request after destroying the current session = %d, want 401", code)
	}
	if !d.waitRevoked(t, "rt-start") {
		t.Fatalf("the current session's refresh token was not revoked at the provider (saw %v)", d.revokedTokens())
	}
	if got := d.row().revoked; got == nil || *got != store.SessionRevokedSignedOutElsewhere {
		t.Fatalf("current session revoked_reason = %v, want signed_out_elsewhere", got)
	}
}

func TestDevices_ALoggedOutDeviceTellsTheProviderAndAFailingProviderBlocksNothing(t *testing.T) {
	d := newDevicesRig(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: d.secret})
	rr := httptest.NewRecorder()
	d.authn.LogoutHandler(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("logout: status %d", rr.Code)
	}
	// Synchronous: it is already there, no waiting.
	if got := d.revokedTokens(); len(got) != 1 || got[0] != "rt-start" {
		t.Fatalf("provider revocations after logout = %v, want [rt-start]", got)
	}
	if got := d.row().revoked; got == nil || *got != store.SessionRevokedSignedOut {
		t.Fatalf("revoked_reason = %v, want signed_out", got)
	}
	if code := d.requestWith(d.authn, d.secret); code != http.StatusUnauthorized {
		t.Fatalf("the signed-out cookie = %d, want 401", code)
	}

	// A provider that answers 500: the sign-out still completes.
	second := d.addSession("rt-second")
	d.idp.set(func(p *fakeIDP) { p.revokeStatus = http.StatusInternalServerError })
	req = httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: second})
	rr = httptest.NewRecorder()
	d.authn.LogoutHandler(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("logout against a failing provider: status %d", rr.Code)
	}
	if code := d.requestWith(d.authn, second); code != http.StatusUnauthorized {
		t.Fatalf("a failing revocation endpoint kept the session alive (%d)", code)
	}
}

func TestDevices_TheDisplayAddressFollowsTheTrustedProxyRule(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	touch := func(d *devicesRig, remote, xff string) string {
		t.Helper()
		// Touch writes at most once a minute: age the last use first.
		if _, err := d.h.super.Exec(context.Background(),
			`UPDATE app.sessions SET last_seen_at = now() - interval '1 hour' WHERE user_id = $1`, d.userID.String()); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: d.secret})
		rr := httptest.NewRecorder()
		d.authn.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d", rr.Code)
		}
		var addr string
		if err := d.h.super.QueryRow(context.Background(),
			`SELECT last_addr FROM app.sessions WHERE user_id = $1 ORDER BY created_at LIMIT 1`, d.userID.String()).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		return addr
	}

	behind := newDevicesRig(t, func(c *auth.Config) { c.TrustedProxies = trusted })
	if got := touch(behind, "10.0.0.1:5555", "203.0.113.50"); got != "203.0.113.50" {
		t.Errorf("last_addr behind a trusted proxy = %q, want the forwarded client", got)
	}
	// SPOOFING: an untrusted peer sending the header is recorded as itself.
	if got := touch(behind, "198.51.100.7:5555", "203.0.113.50"); got != "198.51.100.7" {
		t.Errorf("last_addr for an untrusted peer with X-Forwarded-For = %q, want its own address", got)
	}
	// No trusted proxies configured: the header is never believed.
	none := newDevicesRig(t, nil)
	if got := touch(none, "10.0.0.1:5555", "203.0.113.50"); got != "10.0.0.1" {
		t.Errorf("last_addr with no trusted proxies = %q, want the peer", got)
	}
}

// The sweep as the daily job calls it: it logs every reason, and deletes.
func TestSessionSweepJobLogsEveryReason(t *testing.T) {
	d := newDevicesRig(t, nil)
	ctx := context.Background()
	// Prime so the counts are this test's own.
	if _, err := d.st.SweepSessions(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	old := d.addSession("rt-old")
	oldID := d.idFor(t, old)
	if _, err := d.h.super.Exec(ctx, `UPDATE app.sessions SET revoked_at = now() - interval '8 days', revoked_reason = 'refused' WHERE id = $1`, oldID.String()); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := newCaptureLogger(&buf)
	sweepSessionsOnce(ctx, d.st, time.Now(), logger)

	line := buf.String()
	if !strings.Contains(line, "session sweep") {
		t.Fatalf("no sweep line logged: %q", line)
	}
	for _, reason := range store.SweepReasons {
		if !strings.Contains(line, `"`+reason+`"`) {
			t.Errorf("reason %q is missing from the sweep log line: %s", reason, line)
		}
	}
	if !strings.Contains(line, `"refused":1`) {
		t.Errorf("the refused count is not 1: %s", line)
	}
	var exists bool
	if err := d.h.super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app.sessions WHERE id = $1)`, oldID.String()).Scan(&exists); err != nil || exists {
		t.Fatalf("the old revoked session survived the sweep: exists=%v err=%v", exists, err)
	}
}

func hashSecret(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

func newCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}
