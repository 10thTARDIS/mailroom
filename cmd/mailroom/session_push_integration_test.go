// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// A push subscription must not outlive the session that registered it (ADR-048
// section 5): a subscription left behind keeps receiving pushes whose text comes
// from mail. These drive the real middleware, logout handler and adapter over
// PostgreSQL, one test per way a session ends that goes through RevokeSession:
// logout, a renewal the provider refuses, and an expiry past the outage allowance.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

type pushRig struct {
	*renewRig
	secondSecret string
	pCurrent     ulid.ULID // subscription of the rig's session
	pSecond      ulid.ULID // subscription of another session
	pNone        ulid.ULID // subscription with no session
}

func newPushRig(t *testing.T) *pushRig {
	t.Helper()
	rig := newRenewRig(t, false, nil)
	r := &pushRig{renewRig: rig, secondSecret: rig.addSession("rt-second")}
	sid := func(secret string) ulid.ULID {
		ref, ok, err := rig.st.LookupSession(context.Background(), hashSecret(secret))
		if err != nil || !ok {
			t.Fatalf("lookup: ok=%v err=%v", ok, err)
		}
		return ref.ID
	}
	push := func(endpoint string, session *ulid.ULID) ulid.ULID {
		var id ulid.ULID
		if err := rig.st.UserTx(context.Background(), rig.userID, func(tx *store.UserTx) error {
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
	cur, second := sid(rig.secret), sid(r.secondSecret)
	r.pCurrent = push("https://push.example.com/current", &cur)
	r.pSecond = push("https://push.example.com/second", &second)
	r.pNone = push("https://push.example.com/none", nil)
	return r
}

func (r *pushRig) exists(id ulid.ULID) bool {
	var n int
	_ = r.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.push_subscriptions WHERE id = $1`, id.String()).Scan(&n)
	return n == 1
}

// onlyCurrentGone asserts exactly the rig session's subscription was deleted.
func (r *pushRig) onlyCurrentGone(t *testing.T, how string) {
	t.Helper()
	if r.exists(r.pCurrent) {
		t.Errorf("%s: the ended session's push subscription survived", how)
	}
	if !r.exists(r.pSecond) {
		t.Errorf("%s: another session's push subscription was deleted", how)
	}
	if !r.exists(r.pNone) {
		t.Errorf("%s: the NULL-session push subscription was deleted", how)
	}
}

func TestPushDeletedOnLogout(t *testing.T) {
	r := newPushRig(t)
	authn := r.process()
	if !r.exists(r.pCurrent) || !r.exists(r.pSecond) || !r.exists(r.pNone) {
		t.Fatal("the seeded subscriptions are missing (positive control)")
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.DefaultSessionCookieName, Value: r.secret})
	rr := httptest.NewRecorder()
	authn.LogoutHandler(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("logout: status %d", rr.Code)
	}
	r.onlyCurrentGone(t, "logout")
}

func TestPushDeletedOnRenewalRefusal(t *testing.T) {
	r := newPushRig(t)
	// The provider no longer knows the token the session holds: invalid_grant.
	r.idp.mu.Lock()
	r.idp.valid = map[string]bool{}
	r.idp.mu.Unlock()
	r.age(time.Hour)
	if code := r.request(r.process()); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if row := r.row(); row.revoked == nil || *row.revoked != "refused" {
		t.Fatalf("revoked_reason = %v, want refused", row.revoked)
	}
	r.onlyCurrentGone(t, "renewal refusal")
}

func TestPushDeletedOnExpiry(t *testing.T) {
	r := newPushRig(t)
	// Identity cannot be re-checked (UserInfo down, no groups in the ID token), so
	// the renewal is transient and never confirms; 13 hours unconfirmed is past the
	// 12 hour allowance.
	r.idp.set(func(p *fakeIDP) { p.idTokenNoGroups = true; p.userinfoFail = true })
	r.age(13 * time.Hour)
	if code := r.request(r.process()); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if row := r.row(); row.revoked == nil || *row.revoked != "expired" {
		t.Fatalf("revoked_reason = %v, want expired", row.revoked)
	}
	r.onlyCurrentGone(t, "expiry")
}
