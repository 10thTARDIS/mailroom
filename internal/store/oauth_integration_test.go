// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The OAuth authorization server's storage (migration 00087, ADR-050), through
// the store methods as mailroom_app and the definers as the token endpoint
// calls them. What is proven:
//
//  1. Client ids are minted by the database. The application role cannot INSERT
//     a client, the table refuses a URL-shaped id from anyone, and a URL never
//     resolves as a registered client.
//  2. oauth_access_for_token answers only for a live access token of a live
//     OAuth grant of a user who can sign in. Each predicate is exercised.
//  3. A code and a refresh token are consumed once, even under concurrency, and
//     a second use is reported (replay / reuse) rather than honoured.
//  4. A user's live authorization requests are capped per user, evicting that
//     user's oldest, and one user's requests never count against another's.
//  5. The sweep deletes what it says, keeps the rest and reports every kind.
//  6. The CIMD cache is unreachable to the application role except through its
//     definers, and the cross-user paths a user could try are closed.

import (
	"context"
	"crypto/sha256"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func oaSum(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// oaGrant makes an OAuth grant for the user as mailroom_app and returns it.
func oaGrant(t *testing.T, s *Store, user ulid.ULID, client string) APIKey {
	t.Helper()
	var k APIKey
	err := s.UserTx(context.Background(), user, func(tx *UserTx) error {
		var err error
		k, err = tx.CreateClientGrant(context.Background(), NewClientGrant{Name: "grant " + client, ClientID: client,
			ClientName: "Test app", ClientKind: OAuthClientCIMD, RedirectHost: "app.example.com",
			Scopes: []string{"mail:read"}, RatePerMin: 60, DailyBudget: 500, SuspendOnAnomaly: true})
		return err
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	return k
}

func oaToken(t *testing.T, s *Store, user ulid.ULID, k APIKey, kind, secret string, family ulid.ULID, ttl time.Duration) {
	t.Helper()
	if err := s.UserTx(context.Background(), user, func(tx *UserTx) error {
		return tx.InsertOAuthTokens(context.Background(), NewOAuthToken{Kind: kind, Hash: oaSum(secret), APIKeyID: k.ID,
			FamilyID: family, Resource: "https://mail.example.com/mcp", Scopes: []string{"mail:read"}, ExpiresAt: time.Now().Add(ttl)})
	}); err != nil {
		t.Fatalf("insert token: %v", err)
	}
}

func TestOAuthClientIDsAreMinted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	au := mustULID(t, a.ID)

	var c OAuthClient
	if err := s.UserTx(ctx, au, func(tx *UserTx) error {
		var err error
		c, err = tx.CreateOAuthClient(ctx, "Mine", []string{"https://app.example.com/cb"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^mc_[0-9a-f]{32}$`).MatchString(c.ID) {
		t.Errorf("minted id = %q", c.ID)
	}
	if got, found, err := s.ResolveOAuthClient(ctx, c.ID); err != nil || !found || got.OwnerID == nil || *got.OwnerID != au {
		t.Errorf("minted client does not resolve to its owner: %+v %v %v", got, found, err)
	}

	// The application role has no INSERT: it cannot choose an id at all.
	err := s.UserTx(ctx, au, func(tx *UserTx) error {
		_, err := tx.tx.Exec(ctx, `INSERT INTO app.oauth_clients (id, kind, user_id, name, redirect_uris)
			VALUES ('mc_00000000000000000000000000000001','manual',$1,'x',ARRAY['https://x.example.com/cb'])`, a.ID)
		return err
	})
	if err == nil {
		t.Error("mailroom_app inserted into app.oauth_clients directly")
	}
	// Whoever writes it, the table refuses an id that is not a minted shape,
	// and a CIMD client_id URL is the clearest case.
	for _, id := range []string{"https://app.example.com/client.json", "mc_zz", "rc_" + "0123456789abcdef0123456789abcdef"} {
		kind := "manual"
		if id[:3] == "rc_" {
			kind = "manual" // a 'registered'-shaped id on a manual client
		}
		if _, err := h.super.Exec(ctx, `INSERT INTO app.oauth_clients (id, kind, user_id, name, redirect_uris)
			VALUES ($1,$2,$3,'squat',ARRAY['https://x.example.com/cb'])`, id, kind, a.ID); err == nil {
			t.Errorf("the table stored the client id %q", id)
		}
	}
	// A URL never resolves, even if a row somehow existed.
	if _, found, _ := s.ResolveOAuthClient(ctx, "https://app.example.com/client.json"); found {
		t.Error("a URL resolved as a registered client")
	}
	// Creation needs an identity.
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		_, err := tx.tx.Exec(ctx, `SELECT app.oauth_client_create('x', ARRAY['https://x.example.com/cb'])`)
		return err
	}); err == nil {
		t.Error("oauth_client_create ran with no user identity")
	}
}

func TestOAuthAccessLookupPredicates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	au := mustULID(t, a.ID)
	k := oaGrant(t, s, au, "https://app.example.com/c.json")
	fam := NewULID()
	resolve := func(secret string) bool {
		_, _, ok, err := s.ResolveOAuthAccessToken(ctx, oaSum(secret))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	oaToken(t, s, au, k, OAuthTokenAccess, "mra_live", fam, time.Hour)
	oaToken(t, s, au, k, OAuthTokenAccess, "mra_expired", fam, -time.Minute)
	oaToken(t, s, au, k, OAuthTokenRefresh, "mrr_refresh", fam, time.Hour)
	oaToken(t, s, au, k, OAuthTokenAccess, "mra_revoked", fam, time.Hour)
	if _, err := h.super.Exec(ctx, `UPDATE app.oauth_tokens SET revoked_at=now() WHERE token_hash=$1`, sumB("mra_revoked")); err != nil {
		t.Fatal(err)
	}
	if !resolve("mra_live") {
		t.Fatal("positive control: a live access token does not resolve")
	}
	u, kid, _, _ := s.ResolveOAuthAccessToken(ctx, oaSum("mra_live"))
	if u != au || kid != k.ID {
		t.Errorf("resolved to %s/%s, want %s/%s", u, kid, au, k.ID)
	}
	for _, c := range []struct{ name, secret string }{{"expired", "mra_expired"}, {"a refresh token", "mrr_refresh"},
		{"revoked", "mra_revoked"}, {"unknown", "mra_nope"}} {
		if resolve(c.secret) {
			t.Errorf("%s token resolved", c.name)
		}
	}
	// The grant's own state.
	for _, c := range []struct{ name, set, undo string }{
		{"suspended grant", "suspended_at=now()", "suspended_at=NULL"},
		{"revoked grant", "revoked_at=now()", "revoked_at=NULL"}} {
		if _, err := h.super.Exec(ctx, `UPDATE app.api_keys SET `+c.set+` WHERE id=$1`, k.ID.String()); err != nil {
			t.Fatal(err)
		}
		if resolve("mra_live") {
			t.Errorf("token of a %s resolved", c.name)
		}
		if _, err := h.super.Exec(ctx, `UPDATE app.api_keys SET `+c.undo+` WHERE id=$1`, k.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	// The user.
	for _, col := range []string{"disabled_at", "deleted_at"} {
		if _, err := h.super.Exec(ctx, `UPDATE app.users SET `+col+`=now() WHERE id=$1`, a.ID); err != nil {
			t.Fatal(err)
		}
		if resolve("mra_live") {
			t.Errorf("token of a user with %s set resolved", col)
		}
		if _, err := h.super.Exec(ctx, `UPDATE app.users SET `+col+`=NULL WHERE id=$1`, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	if !resolve("mra_live") {
		t.Fatal("control after restoring: the token should resolve again")
	}
	// An API key's row never answers the OAuth lookup, nor does an OAuth grant
	// answer the key lookup.
	var keyHash []byte
	_ = h.super.QueryRow(ctx, `SELECT token_hash FROM app.api_keys WHERE user_id=$1 AND kind='key' LIMIT 1`, a.ID).Scan(&keyHash)
	var kh [32]byte
	copy(kh[:], keyHash)
	if _, _, ok, _ := s.ResolveOAuthAccessToken(ctx, kh); ok {
		t.Error("an API key's hash resolved through the OAuth lookup")
	}
	if _, _, ok, _ := s.ResolveAPIKeyToken(ctx, oaSum("mra_live")); ok {
		t.Error("an OAuth access token resolved through the API key lookup")
	}
}

func TestOAuthCodeAndRefreshAreSingleUse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	au := mustULID(t, a.ID)
	k := oaGrant(t, s, au, "https://app.example.com/c.json")

	// Codes: many concurrent redemptions, exactly one wins.
	if err := s.UserTx(ctx, au, func(tx *UserTx) error {
		return tx.InsertOAuthCode(ctx, NewOAuthCode{ID: NewULID(), Hash: oaSum("code-1"), APIKeyID: k.ID,
			ClientID: "c", RedirectURI: "https://app.example.com/cb", CodeChallenge: "ch", Resource: "r", TTL: time.Minute})
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh, replays := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, found, err := s.ConsumeOAuthCode(ctx, oaSum("code-1"))
			if err != nil || !found {
				t.Errorf("consume: %v %v", found, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if c.Replay {
				replays++
			} else {
				fresh++
			}
		}()
	}
	wg.Wait()
	if fresh != 1 || replays != 7 {
		t.Errorf("8 concurrent redemptions: %d fresh, %d replays; want 1 and 7", fresh, replays)
	}
	if _, found, _ := s.ConsumeOAuthCode(ctx, oaSum("no-such-code")); found {
		t.Error("an unknown code was found")
	}

	// Refresh tokens: first use rotates, second is reuse; a revoked one says so.
	fam := NewULID()
	oaToken(t, s, au, k, OAuthTokenRefresh, "mrr_one", fam, time.Hour)
	r1, found, err := s.ConsumeOAuthRefresh(ctx, oaSum("mrr_one"))
	if err != nil || !found || r1.Reuse || r1.Revoked || r1.FamilyID != fam {
		t.Fatalf("first use: %+v %v %v", r1, found, err)
	}
	r2, _, _ := s.ConsumeOAuthRefresh(ctx, oaSum("mrr_one"))
	if !r2.Reuse {
		t.Errorf("second use not reported as reuse: %+v", r2)
	}
	if err := s.UserTx(ctx, au, func(tx *UserTx) error { _, err := tx.RevokeOAuthFamily(ctx, fam); return err }); err != nil {
		t.Fatal(err)
	}
	r3, _, _ := s.ConsumeOAuthRefresh(ctx, oaSum("mrr_one"))
	if !r3.Revoked {
		t.Errorf("a revoked family's token not reported revoked: %+v", r3)
	}
	// An access token is not a refresh token.
	oaToken(t, s, au, k, OAuthTokenAccess, "mra_x", NewULID(), time.Hour)
	if _, found, _ := s.ConsumeOAuthRefresh(ctx, oaSum("mra_x")); found {
		t.Error("an access token was consumed as a refresh token")
	}
}

func TestOAuthRequestsAreCappedPerUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	au, bu := mustULID(t, a.ID), mustULID(t, b.ID)
	mk := func(user ulid.ULID, i int) NewOAuthRequest {
		return NewOAuthRequest{ID: NewULID(), Hash: oaSum("req-" + user.String() + string(rune('A'+i%26)) + time.Now().String()),
			ClientID: "c", ClientName: "n", ClientKind: OAuthClientCIMD, RedirectURI: "https://app.example.com/cb",
			RedirectHost: "app.example.com", CodeChallenge: "ch", Resource: "r", Scopes: []string{"mail:read"}}
	}
	var bHash [32]byte
	if err := s.UserTx(ctx, bu, func(tx *UserTx) error {
		n := mk(bu, 0)
		bHash = n.Hash
		return tx.CreateOAuthRequest(ctx, n)
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxOAuthRequestsPerUser+10; i++ {
		if err := s.UserTx(ctx, au, func(tx *UserTx) error { return tx.CreateOAuthRequest(ctx, mk(au, i)) }); err != nil {
			t.Fatal(err)
		}
	}
	var na, nb int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_requests WHERE user_id=$1 AND expires_at > now() AND client_id='c'`, a.ID).Scan(&na)
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_requests WHERE user_id=$1 AND request_hash=$2`, b.ID, bHash[:]).Scan(&nb)
	if na > MaxOAuthRequestsPerUser {
		t.Errorf("user A holds %d live requests, cap %d", na, MaxOAuthRequestsPerUser)
	}
	if na < MaxOAuthRequestsPerUser {
		t.Errorf("user A holds %d requests, want %d (the newest are kept)", na, MaxOAuthRequestsPerUser)
	}
	if nb != 1 {
		t.Error("user A's requests evicted user B's")
	}
	// B cannot read, decide or even see A's request.
	var aHash []byte
	_ = h.super.QueryRow(ctx, `SELECT request_hash FROM app.oauth_requests WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1`, a.ID).Scan(&aHash)
	var ah [32]byte
	copy(ah[:], aHash)
	_ = s.UserTx(ctx, bu, func(tx *UserTx) error {
		if _, found, _ := tx.GetOAuthRequest(ctx, ah); found {
			t.Error("B read A's authorization request")
		}
		return nil
	})
	// Single-use decision.
	_ = s.UserTx(ctx, au, func(tx *UserTx) error {
		r, found, _ := tx.GetOAuthRequest(ctx, ah)
		if !found {
			t.Fatal("A cannot read its own request")
		}
		if ok, _ := tx.DecideOAuthRequest(ctx, r.ID); !ok {
			t.Error("first decision refused")
		}
		if ok, _ := tx.DecideOAuthRequest(ctx, r.ID); ok {
			t.Error("a request was decided twice")
		}
		return nil
	})
}

func TestOAuthSweepAndCache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	au := mustULID(t, a.ID)
	k := oaGrant(t, s, au, "https://app.example.com/c.json")
	fam := NewULID()
	oaToken(t, s, au, k, OAuthTokenAccess, "old-expired", fam, -30*24*time.Hour)
	oaToken(t, s, au, k, OAuthTokenAccess, "just-expired", fam, -time.Hour)
	oaToken(t, s, au, k, OAuthTokenAccess, "live", fam, time.Hour)

	// Zero first: nothing is past the grace period except "old-expired".
	counts, err := s.SweepOAuth(ctx, time.Now(), OAuthSweepGrace)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range OAuthSweepKinds {
		if _, ok := counts[kind]; !ok {
			t.Errorf("sweep did not report %q", kind)
		}
	}
	if counts["tokens"] < 1 {
		t.Errorf("the sweep deleted %d tokens, want at least the old expired one", counts["tokens"])
	}
	var left int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_tokens WHERE token_hash=ANY($1)`,
		[][]byte{sumB("just-expired"), sumB("live")}).Scan(&left)
	if left != 2 {
		t.Errorf("the sweep kept %d of the 2 tokens inside the grace period", left)
	}
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.oauth_tokens WHERE token_hash=$1`, sumB("old-expired")).Scan(&left)
	if left != 0 {
		t.Error("the sweep kept a token expired 30 days ago")
	}

	// The CIMD cache: definers only.
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		_, err := tx.tx.Exec(ctx, `SELECT count(*) FROM app.oauth_cimd_cache`)
		return err
	}); err == nil {
		t.Error("mailroom_app read app.oauth_cimd_cache directly")
	}
	if stored, err := s.PutCIMDDocument(ctx, "https://app.example.com/c.json", []byte(`{"client_name":"n","redirect_uris":["https://app.example.com/cb"]}`), time.Now().Add(time.Hour)); err != nil || !stored {
		t.Fatalf("put: %v %v", stored, err)
	}
	if doc, _, found, err := s.GetCIMDDocument(ctx, "https://app.example.com/c.json"); err != nil || !found || len(doc) == 0 {
		t.Errorf("get after put: %v %v", found, err)
	}
	_, _ = s.PutCIMDDocument(ctx, "https://app.example.com/stale.json", []byte(`{}`), time.Now().Add(-time.Hour))
	if _, _, found, _ := s.GetCIMDDocument(ctx, "https://app.example.com/stale.json"); found {
		t.Error("an expired cached document was served")
	}
	c2, err := s.SweepOAuth(ctx, time.Now(), OAuthSweepGrace)
	if err != nil || c2["cimd"] < 1 {
		t.Errorf("sweep of the expired cached document: %v %v", c2, err)
	}
}

func sumB(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// A user cannot reach another's family, grant or tokens through the store.
func TestOAuthCrossUserStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	au, bu := mustULID(t, a.ID), mustULID(t, b.ID)
	k := oaGrant(t, s, au, "https://app.example.com/c.json")
	fam := NewULID()
	oaToken(t, s, au, k, OAuthTokenAccess, "mra_a", fam, time.Hour)
	_ = s.UserTx(ctx, bu, func(tx *UserTx) error {
		n, err := tx.RevokeOAuthFamily(ctx, fam)
		if err != nil || n != 0 {
			t.Errorf("B revoked %d of A's tokens (%v)", n, err)
		}
		fams, _ := tx.ListOAuthFamilies(ctx, 100)
		for _, f := range fams {
			if f.ID == fam {
				t.Error("B lists A's family")
			}
		}
		if _, found, _ := tx.GetAPIKey(ctx, k.ID); found {
			t.Error("B reads A's grant")
		}
		return nil
	})
	if _, _, ok, _ := s.ResolveOAuthAccessToken(ctx, oaSum("mra_a")); !ok {
		t.Error("A's token was revoked by B")
	}
}
