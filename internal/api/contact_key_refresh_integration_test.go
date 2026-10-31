// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// ContactKey/refresh (#818) against a WKD stub and against the real egress-
// guarded fetcher: every outcome, its change rows and events, the decision path
// for a different key, the limits, and the guard refusing a private host.

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/egress"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

const rfDay = 24 * time.Hour

// expiringPeer is a real key for addr made `age` ago and expiring `left` from
// now, and the same key renewed (new self-signatures, three years).
func expiringPeer(t *testing.T, addr string, age, left time.Duration) (old, renewed []byte, e *openpgp.Entity) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-age)
	e, err := openpgp.NewEntity("Peer", "", addr, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time: func() time.Time { return created }, KeyLifetimeSecs: uint32((age + left) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	// NewEntity binds the subkey with no expiry, so the key's usable-until is
	// the primary's, and renewing the user ID self-signature renews the key.
	old = pubOf(t, e)
	at := now.Add(-time.Minute)
	l := uint32((age + 3*365*rfDay) / time.Second)
	cfg := &packet.Config{Time: func() time.Time { return at }}
	for name, ident := range e.Identities {
		cp := *ident.SelfSignature
		cp.Notations = nil
		cp.CreationTime, cp.KeyLifetimeSecs, cp.SigLifetimeSecs = at, &l, nil
		if err := cp.SignUserId(name, e.PrimaryKey, e.PrivateKey, cfg); err != nil {
			t.Fatal(err)
		}
		ident.SelfSignature = &cp
		ident.Signatures = append(ident.Signatures, &cp)
	}
	renewed = pubOf(t, e)
	return old, renewed, e
}

func (h *encHarness) refresh(t *testing.T, id string) (map[string]any, *MethodError) {
	t.Helper()
	raw, mErr := h.api.contactKeyRefresh(context.Background(), h.a.UserID, mustJSON(t, map[string]any{"id": id}))
	if mErr != nil {
		return nil, mErr
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func (h *encHarness) eventCount(t *testing.T, keyID string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.contact_key_events WHERE contact_key_id = $1`, keyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// unclaim lets a test refresh the same key again at once.
func (h *encHarness) unclaim(t *testing.T, keyID string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), `UPDATE app.contact_keys SET checked_at = NULL WHERE id = $1`, keyID); err != nil {
		t.Fatal(err)
	}
}

func TestContactKeyRefreshOutcomes(t *testing.T) {
	h := newEncHarness(t)
	const addr = "carol@example.com"
	old, renewed, _ := expiringPeer(t, addr, 300*rfDay, 10*rfDay)
	fpr := h.seedKey(t, addr, old, store.ContactKeyWKD, false, time.Now())
	id, k := h.contactKeyID(t, fpr, false)
	if k["expiringSoon"] != true {
		t.Fatalf("fixture: the held key is not expiring soon: %v", k)
	}

	step := func(name string, serve fetchResult, want string, wantRows, wantEvents int) map[string]any {
		t.Helper()
		h.unclaim(t, id)
		h.fetcher.results[addr] = serve
		r0, e0 := h.contactChanges(t, addr), h.eventCount(t, id)
		out, mErr := h.refresh(t, id)
		if mErr != nil {
			t.Fatalf("%s: %+v", name, mErr)
		}
		if out["result"] != want {
			t.Errorf("%s: result %v, want %s (%v)", name, out["result"], want, out)
		}
		if r, e := h.contactChanges(t, addr)-r0, h.eventCount(t, id)-e0; r != wantRows || e != wantEvents {
			t.Errorf("%s: %d Contact rows, %d events; want %d/%d", name, r, e, wantRows, wantEvents)
		}
		return out
	}

	step("not found", fetchResult{err: wkd.ErrNoKey}, refreshNotFound, 0, 0)
	step("unreachable", fetchResult{err: errors.New("dial tcp: i/o timeout")}, refreshUnreachable, 0, 0)
	step("unchanged", fetchResult{key: old}, refreshUnchanged, 0, 0)
	out := step("renewed", fetchResult{key: renewed}, refreshRenewed, 1, 1)
	if nk := out["key"].(map[string]any); nk["expiringSoon"] != false || nk["fingerprint"] != fpr {
		t.Errorf("after the renewal the key is %v", nk)
	}
	step("renewal again", fetchResult{key: renewed}, refreshUnchanged, 0, 0)
	// A renewal is the same key: no keyChanged anywhere.
	if r := h.lookupOne(t, addr, false); r["keyChanged"] != false || r["usable"] != true {
		t.Errorf("lookup after the renewal = %v", r)
	}
}

func TestContactKeyRefreshDifferentKeyGoesToTheDecision(t *testing.T) {
	h := newEncHarness(t)
	const addr = "dave@example.com"
	old, _, _ := expiringPeer(t, addr, 300*rfDay, 10*rfDay)
	fpr := h.seedKey(t, addr, old, store.ContactKeyWKD, false, time.Now().Add(-time.Hour))
	id, _ := h.contactKeyID(t, fpr, false)
	other := newPeerKey(t, addr, nil)
	h.fetcher.results[addr] = fetchResult{key: pubOf(t, other)}

	r0 := h.contactChanges(t, addr)
	out, mErr := h.refresh(t, id)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if out["result"] != refreshDifferentKey || out["newFingerprint"] != hexOf(other) || out["newKeyId"] == nil {
		t.Fatalf("refresh = %v, want differentKey naming the new key", out)
	}
	if r := h.contactChanges(t, addr) - r0; r != 1 {
		t.Errorf("a stored different key wrote %d Contact rows, want 1", r)
	}
	// Never a silent replacement: the address now reports keyChanged, pinned
	// to the held key, and an encrypted send is refused until the decision.
	r := h.lookupOne(t, addr, false)
	if r["keyChanged"] != true || r["pinnedFingerprint"] != fpr || r["usable"] != false {
		t.Errorf("lookup after a different key = %v, want keyChanged pinned to %s", r, fpr)
	}
	if pin, ok := h.pin(t, addr); !ok || pin != fpr {
		t.Errorf("pin = %q %v, want the held key %s", pin, ok, fpr)
	}
}

func TestContactKeyRefreshLimits(t *testing.T) {
	h := newEncHarness(t)
	const addr = "erin@example.com"
	old, _, _ := expiringPeer(t, addr, 300*rfDay, 10*rfDay)
	fpr := h.seedKey(t, addr, old, store.ContactKeyWKD, false, time.Now())
	id, _ := h.contactKeyID(t, fpr, false)
	h.fetcher.results[addr] = fetchResult{key: old}
	calls := func() int { h.fetcher.mu.Lock(); defer h.fetcher.mu.Unlock(); return len(h.fetcher.calls) }

	if _, mErr := h.refresh(t, id); mErr != nil {
		t.Fatal(mErr)
	}
	// Per key: a second refresh inside the interval is refused before the network.
	c0 := calls()
	if _, mErr := h.refresh(t, id); mErr == nil || mErr.Type != errRateLimited {
		t.Errorf("second refresh at once: %+v, want rateLimited", mErr)
	}
	if calls() != c0 {
		t.Error("the refused refresh still looked the key up")
	}
	// Per user and per domain: tiny budgets, the interval out of the way.
	lim := h.api.refreshLimit()
	lim.minInterval = time.Nanosecond
	lim.user = NewRateLimiter(RateLimitConfig{PerSecond: 1e-9, Burst: 1})
	h.unclaim(t, id)
	if _, mErr := h.refresh(t, id); mErr != nil {
		t.Fatalf("first under the user budget: %+v", mErr)
	}
	c0 = calls()
	h.unclaim(t, id)
	if _, mErr := h.refresh(t, id); mErr == nil || mErr.Type != errRateLimited {
		t.Errorf("over the user budget: %+v, want rateLimited", mErr)
	}
	lim.user = NewRateLimiter(RateLimitConfig{PerSecond: 1, Burst: 100})
	lim.domain = NewRateLimiter(RateLimitConfig{PerSecond: 1e-9, Burst: 1})
	h.unclaim(t, id)
	if _, mErr := h.refresh(t, id); mErr != nil {
		t.Fatalf("first under the domain budget: %+v", mErr)
	}
	h.unclaim(t, id)
	if _, mErr := h.refresh(t, id); mErr == nil || mErr.Type != errRateLimited {
		t.Errorf("over the domain budget: %+v, want rateLimited", mErr)
	}
	if calls() != c0+1 {
		t.Errorf("refused refreshes reached the network: %d calls, want %d", calls(), c0+1)
	}
}

// errFetcher wraps a real fetcher and keeps every error it returned.
type errFetcher struct {
	inner KeyFetcher
	mu    sync.Mutex
	errs  []error
}

func (f *errFetcher) Fetch(ctx context.Context, addr string) ([]byte, string, error) {
	b, u, err := f.inner.Fetch(ctx, addr)
	f.mu.Lock()
	f.errs = append(f.errs, err)
	f.mu.Unlock()
	return b, u, err
}

// TestContactKeyRefreshEgressGuardRefusesAPrivateHost: through the PRODUCTION
// fetcher (internal/wkd over internal/egress), a key whose address is at a
// loopback host is reported unreachable, and the reason is the guard. The
// negative control exempts loopback in the policy and sees a different error
// (the connection itself), so the assertion can tell the guard from a dead port.
func TestContactKeyRefreshEgressGuardRefusesAPrivateHost(t *testing.T) {
	const addr = "frank@127.0.0.1"
	run := func(t *testing.T, policy egress.Policy) (map[string]any, []error) {
		h := newEncHarness(t)
		old, _, _ := expiringPeer(t, addr, 300*rfDay, 10*rfDay)
		fpr := h.seedKey(t, addr, old, store.ContactKeyWKD, false, time.Now())
		id, _ := h.contactKeyID(t, fpr, false)
		f := &errFetcher{inner: wkd.NewFetcher(egress.New(policy), nil)}
		h.api.keyFetcher = f
		out, mErr := h.refresh(t, id)
		if mErr != nil {
			t.Fatal(mErr)
		}
		return out, f.errs
	}
	out, errs := run(t, egress.Policy{Timeout: 3 * time.Second})
	if out["result"] != refreshUnreachable {
		t.Errorf("result = %v, want unreachable", out["result"])
	}
	if len(errs) != 1 || !errors.Is(errs[0], egress.ErrBlockedAddress) {
		t.Errorf("fetch errors = %v, want the egress guard's ErrBlockedAddress", errs)
	}
	// Control: with loopback exempted the guard admits it, and the failure is
	// something else, so the check above is observing the guard.
	_, errs = run(t, egress.Policy{Timeout: 3 * time.Second, Exempt: func(a netip.Addr) bool { return a.IsLoopback() }})
	if len(errs) != 1 || errs[0] == nil || errors.Is(errs[0], egress.ErrBlockedAddress) {
		t.Errorf("control: fetch errors with loopback exempted = %v, want a non-guard failure", errs)
	}
}
