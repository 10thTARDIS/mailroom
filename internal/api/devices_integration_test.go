// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Signed-in devices against a real Postgres (#937, ADR-048 section 6):
// SignedInDevice/get and /set, the push subscription's session binding, and the
// EventSource session re-check.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// makeSession inserts a live session for user with the given user agent, address
// and sealed refresh token, and returns the context a request authenticated with
// it would carry.
func makeSession(t *testing.T, h *apiHarness, user ulid.ULID, ua, addr string, sealed []byte) (ulid.ULID, context.Context) {
	t.Helper()
	id := store.NewULID()
	hash := [32]byte{}
	copy(hash[:], []byte(id.String()))
	if err := h.store.UserTx(context.Background(), user, func(tx *store.UserTx) error {
		return tx.CreateSession(context.Background(), store.NewSession{
			ID: id, TokenHash: hash, RefreshCT: sealed, UserAgent: ua, Addr: addr,
		})
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	ref := auth.SessionRef{ID: id, UserID: user, LastCheckedAt: time.Now()}
	return id, auth.ContextWithSessionForTest(context.Background(), ref)
}

func callCtx(t *testing.T, ctx context.Context, fn methodFn, user ulid.ULID, args any) map[string]any {
	t.Helper()
	raw, mErr := fn(ctx, user, mustJSON(t, args))
	if mErr != nil {
		t.Fatalf("method error: %+v", mErr)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

func sessionRevokedReason(t *testing.T, h *apiHarness, id ulid.ULID) string {
	t.Helper()
	var reason *string
	if err := h.super.QueryRow(context.Background(),
		`SELECT revoked_reason FROM app.sessions WHERE id = $1`, id.String()).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

const (
	uaFirefoxLinux = "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0"
	uaSafariIPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
)

func TestSignedInDeviceGet(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "dev-get-a-"+store.NewULID().String())
	b := seedAPIUser(t, h.super, "dev-get-b-"+store.NewULID().String())

	s1, ctx1 := makeSession(t, h, a.UserID, uaFirefoxLinux, "192.0.2.5", nil)
	s2, _ := makeSession(t, h, a.UserID, uaSafariIPhone, "198.51.100.9", nil)
	// Make s2 the most recently used.
	if _, err := h.super.Exec(context.Background(), `UPDATE app.sessions SET last_seen_at = now() + interval '1 hour' WHERE id = $1`, s2.String()); err != nil {
		t.Fatal(err)
	}
	sGone, _ := makeSession(t, h, a.UserID, "Mozilla/5.0 Firefox/1", "192.0.2.77", nil)
	if err := h.store.UserTx(context.Background(), a.UserID, func(tx *store.UserTx) error {
		_, err := tx.RevokeSession(context.Background(), sGone, store.SessionRevokedSignedOut)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sB, _ := makeSession(t, h, b.UserID, uaFirefoxLinux, "203.0.113.200", nil)

	res := callCtx(t, ctx1, h.api.signedInDeviceGet, a.UserID, map[string]any{})
	list := asArr(res["list"])
	byID := map[string]map[string]any{}
	for _, it := range list {
		m := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID[s1.String()] == nil || byID[s2.String()] == nil {
		t.Fatalf("A's live sessions missing from %v", res["list"])
	}
	if byID[sGone.String()] != nil {
		t.Error("a revoked session is listed")
	}
	if byID[sB.String()] != nil {
		t.Error("B's session is listed for A")
	}
	if got := byID[s1.String()]["description"]; got != "Firefox on Linux" {
		t.Errorf("description = %v, want Firefox on Linux", got)
	}
	if got := byID[s2.String()]["description"]; got != "Safari on iPhone" {
		t.Errorf("description = %v, want Safari on iPhone", got)
	}
	if byID[s1.String()]["isCurrent"] != true || byID[s2.String()]["isCurrent"] != false {
		t.Errorf("isCurrent: s1=%v s2=%v, want true/false", byID[s1.String()]["isCurrent"], byID[s2.String()]["isCurrent"])
	}
	if byID[s1.String()]["lastAddr"] != "192.0.2.5" {
		t.Errorf("lastAddr = %v", byID[s1.String()]["lastAddr"])
	}
	for _, f := range []string{"createdAt", "lastSeenAt"} {
		if s, _ := byID[s1.String()][f].(string); !strings.HasSuffix(s, "Z") {
			t.Errorf("%s = %v, want a UTCDate", f, byID[s1.String()][f])
		}
	}
	if first := list[0].(map[string]any)["id"]; first != s2.String() {
		t.Errorf("first entry is %v, want the most recently used (%s)", first, s2)
	}
	// The raw user agent never leaves the server.
	raw, _ := json.Marshal(res)
	for _, secret := range []string{"Mozilla", "X11", "rv:131", "AppleWebKit"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the response contains raw user-agent text %q:\n%s", secret, raw)
		}
	}
	if res["state"] == "" || res["state"] == nil {
		t.Error("no state")
	}

	// ids other than null: not found, never a row.
	res = callCtx(t, ctx1, h.api.signedInDeviceGet, a.UserID, map[string]any{"ids": []string{s1.String()}})
	if len(asArr(res["list"])) != 0 || !strInArr(res["notFound"], s1.String()) {
		t.Errorf("get with ids = %v, want notFound", res)
	}
}

func TestSignedInDeviceSetDestroy(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "dev-set-a-"+store.NewULID().String())
	b := seedAPIUser(t, h.super, "dev-set-b-"+store.NewULID().String())

	var mu sync.Mutex
	revoked := map[ulid.ULID][]byte{}
	h.api.sessionRevoker = func(user, session ulid.ULID, sealed []byte) {
		mu.Lock()
		defer mu.Unlock()
		if user != a.UserID {
			t.Errorf("revoker told about user %s, want A", user)
		}
		revoked[session] = sealed
	}

	cur, ctx := makeSession(t, h, a.UserID, uaFirefoxLinux, "192.0.2.5", []byte("sealed-current"))
	other1, _ := makeSession(t, h, a.UserID, uaSafariIPhone, "192.0.2.6", []byte("sealed-other1"))
	other2, _ := makeSession(t, h, a.UserID, uaSafariIPhone, "192.0.2.7", nil) // no refresh token
	bs, _ := makeSession(t, h, b.UserID, uaFirefoxLinux, "203.0.113.1", []byte("sealed-b"))

	// Push subscriptions: one per device, plus one with no session.
	push := func(user ulid.ULID, endpoint string, session *ulid.ULID) ulid.ULID {
		var id ulid.ULID
		if err := h.store.UserTx(context.Background(), user, func(tx *store.UserTx) error {
			ps, err := tx.CreatePushSubscription(context.Background(), store.PushSubscriptionInput{
				Endpoint: endpoint, KeysP256dh: "p", KeysAuth: "a", SessionID: session,
			})
			id = ps.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pOther1 := push(a.UserID, "https://push.example.com/o1", &other1)
	pOther2 := push(a.UserID, "https://push.example.com/o2", &other2)
	pNone := push(a.UserID, "https://push.example.com/none", nil)
	pB := push(b.UserID, "https://push.example.com/b", &bs)
	pushExists := func(id ulid.ULID) bool {
		var n int
		_ = h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.push_subscriptions WHERE id = $1`, id.String()).Scan(&n)
		return n == 1
	}

	// Destroy other1, an unknown id, B's session, and a malformed id.
	res := callCtx(t, ctx, h.api.signedInDeviceSet, a.UserID, map[string]any{
		"destroy": []string{other1.String(), store.NewULID().String(), bs.String(), "not-an-id"},
	})
	destroyed := asArr(res["destroyed"])
	if len(destroyed) != 1 || destroyed[0] != other1.String() {
		t.Fatalf("destroyed = %v, want only %s", destroyed, other1)
	}
	if !mapHasKey(res, "notDestroyed", bs.String()) || !mapHasKey(res, "notDestroyed", "not-an-id") {
		t.Errorf("notDestroyed = %v, want B's session and the malformed id", res["notDestroyed"])
	}
	if nd := res["notDestroyed"].(map[string]any)[bs.String()].(map[string]any); nd["type"] != "notFound" {
		t.Errorf("B's session: %v, want notFound", nd)
	}
	if got := sessionRevokedReason(t, h, other1); got != store.SessionRevokedSignedOutElsewhere {
		t.Errorf("other1 revoked_reason = %q, want signed_out_elsewhere", got)
	}
	if sessionRevokedReason(t, h, bs) != "" || sessionRevokedReason(t, h, cur) != "" || sessionRevokedReason(t, h, other2) != "" {
		t.Error("a session that was not destroyed was revoked")
	}
	if pushExists(pOther1) || !pushExists(pOther2) || !pushExists(pNone) || !pushExists(pB) {
		t.Errorf("push subscriptions: other1=%v other2=%v none=%v B=%v; want only other1's deleted",
			pushExists(pOther1), pushExists(pOther2), pushExists(pNone), pushExists(pB))
	}
	mu.Lock()
	if string(revoked[other1]) != "sealed-other1" || len(revoked) != 1 {
		t.Errorf("provider revocation hook calls = %v, want exactly other1 with its sealed token", revoked)
	}
	mu.Unlock()

	// Destroying an already-revoked id is notFound, not success.
	res = callCtx(t, ctx, h.api.signedInDeviceSet, a.UserID, map[string]any{"destroy": []string{other1.String()}})
	if len(asArr(res["destroyed"])) != 0 || !mapHasKey(res, "notDestroyed", other1.String()) {
		t.Errorf("destroying a revoked session again = %v", res)
	}

	// *others*: every session but the current one.
	third, _ := makeSession(t, h, a.UserID, uaSafariIPhone, "192.0.2.8", []byte("sealed-third"))
	res = callCtx(t, ctx, h.api.signedInDeviceSet, a.UserID, map[string]any{"destroy": []string{"*others*"}})
	got := map[string]bool{}
	for _, id := range asArr(res["destroyed"]) {
		got[id.(string)] = true
	}
	if got[cur.String()] {
		t.Fatal("*others* destroyed the current session")
	}
	if !got[other2.String()] || !got[third.String()] {
		t.Errorf("*others* destroyed %v, want other2 and the third session", got)
	}
	if sessionRevokedReason(t, h, cur) != "" {
		t.Error("the current session was revoked by *others*")
	}
	if sessionRevokedReason(t, h, bs) != "" {
		t.Error("*others* revoked B's session")
	}
	if pushExists(pOther2) {
		t.Error("*others* left a revoked session's push subscription")
	}
	if !pushExists(pNone) || !pushExists(pB) {
		t.Error("*others* deleted a subscription that belongs to no revoked session")
	}
	mu.Lock()
	if string(revoked[third]) != "sealed-third" {
		t.Errorf("the revoker was not told about the third session: %v", revoked)
	}
	if _, ok := revoked[other2]; ok {
		t.Error("the revoker was called for a session that has no refresh token")
	}
	mu.Unlock()

	// Destroying the CURRENT session by its id is allowed: it equals signing out.
	res = callCtx(t, ctx, h.api.signedInDeviceSet, a.UserID, map[string]any{"destroy": []string{cur.String()}})
	if d := asArr(res["destroyed"]); len(d) != 1 || d[0] != cur.String() {
		t.Fatalf("destroying the current session = %v", res)
	}
	if got := sessionRevokedReason(t, h, cur); got != store.SessionRevokedSignedOutElsewhere {
		t.Errorf("current session revoked_reason = %q", got)
	}
}

// WebPushSubscription/set fills session_id from the request's session; a
// request with none (dev-auth) stores NULL.
func TestPushCreateRecordsTheSession(t *testing.T) {
	h := newAPIHarness(t)
	a := seedAPIUser(t, h.super, "dev-push-"+store.NewULID().String())
	sess, ctx := makeSession(t, h, a.UserID, uaFirefoxLinux, "192.0.2.5", nil)

	create := func(ctx context.Context, endpoint string) string {
		res := callCtx(t, ctx, h.api.pushSet, a.UserID, map[string]any{"create": map[string]any{"c": map[string]any{
			"endpoint": endpoint, "keys": map[string]any{"p256dh": "p", "auth": "a"},
		}}})
		return res["created"].(map[string]any)["c"].(map[string]any)["id"].(string)
	}
	sessionOf := func(id string) *string {
		var s *string
		if err := h.super.QueryRow(context.Background(), `SELECT session_id FROM app.push_subscriptions WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	withSession := create(ctx, "https://push.example.com/with-session")
	if s := sessionOf(withSession); s == nil || *s != sess.String() {
		t.Fatalf("session_id = %v, want %s", s, sess)
	}
	without := create(context.Background(), "https://push.example.com/without-session")
	if s := sessionOf(without); s != nil {
		t.Fatalf("a session-less registration stored session_id %s", *s)
	}
	// Re-registering the first endpoint WITHOUT a session moves it to NULL (the
	// upsert overwrites), and with a session moves it back.
	if id := create(context.Background(), "https://push.example.com/with-session"); id != withSession {
		t.Fatalf("re-registration made a new row")
	}
	if s := sessionOf(withSession); s != nil {
		t.Fatalf("re-registration kept session_id %s", *s)
	}
	create(ctx, "https://push.example.com/with-session")
	if s := sessionOf(withSession); s == nil || *s != sess.String() {
		t.Fatalf("session_id after re-registration with a session = %v", s)
	}
}

// ---- the EventSource session re-check ---------------------------------------

type streamRig struct {
	srv  *httptest.Server
	live atomic.Bool
	fail atomic.Bool
}

// newStreamRig serves the API's event stream for a request that carries a
// session (injected the way Middleware does), with a fake liveness check and a
// short re-check interval.
func newStreamRig(t *testing.T, h *apiHarness, user, session ulid.ULID, interval time.Duration) *streamRig {
	t.Helper()
	r := &streamRig{}
	r.live.Store(true)
	h.api.streamRecheck = interval
	h.api.sessionLive = func(_ context.Context, sid, uid ulid.ULID) (bool, error) {
		if sid != session || uid != user {
			t.Errorf("re-check asked about session %s user %s, want %s %s", sid, uid, session, user)
		}
		if r.fail.Load() {
			return false, errors.New("database unavailable")
		}
		return r.live.Load(), nil
	}
	handler := h.api.Handler()
	inject := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ref := auth.SessionRef{ID: session, UserID: user}
		handler.ServeHTTP(w, req.WithContext(auth.ContextWithSessionForTest(req.Context(), ref)))
	})
	r.srv = httptest.NewServer(auth.DevAuth(inject, user))
	t.Cleanup(r.srv.Close)
	return r
}

// open connects and returns a reader over the stream plus a channel closed when
// the server ends it.
func (r *streamRig) open(t *testing.T) (*bufio.Reader, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+"/jmap/eventsource/?types=Email&ping=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	if ev, ok := readEvent(t, br); !ok || ev.Name != "state" {
		t.Fatalf("first event = %+v", ev)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	return br, ended
}

func TestEventSourceEndsWhenTheSessionIsGone(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	r := newStreamRig(t, h.apiHarness, a.UserID, store.NewULID(), 100*time.Millisecond)
	_, ended := r.open(t)

	// While the session is live the stream stays open across several re-checks
	// (the positive control: a stream that always ends proves nothing).
	select {
	case <-ended:
		t.Fatal("the stream ended although the session is live")
	case <-time.After(600 * time.Millisecond):
	}

	r.live.Store(false)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end within a few re-check intervals of the session being revoked")
	}
}

func TestEventSourceKeepsTheStreamWhenTheCheckErrors(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	r := newStreamRig(t, h.apiHarness, a.UserID, store.NewULID(), 100*time.Millisecond)
	r.fail.Store(true)
	r.live.Store(false) // would end the stream if the error were read as "gone"
	_, ended := r.open(t)

	select {
	case <-ended:
		t.Fatal("a store error ended the stream; a database blip must not drop everyone's push channel")
	case <-time.After(1200 * time.Millisecond):
	}
	// The error clears while the session is still revoked: now it ends.
	r.fail.Store(false)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end once the check could answer")
	}
}
