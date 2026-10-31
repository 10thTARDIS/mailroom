// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package oauthmail

// The Manager over the REAL grant store (StoreGrants): sealed refresh token in
// app.accounts.credentials_ct, grant row in app.oauth_grants, a fake token
// endpoint. Proves end to end that a rotation re-seals the new token under the
// compare-and-swap with no change_log row, that invalid_grant moves the grant
// to needs_reauth with exactly one Account change row and one audit row, and
// that no token reaches a log line or an error on the way.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

type staticKeys struct{ master []byte }

func (k staticKeys) MasterKey(context.Context) ([]byte, error) {
	return append([]byte(nil), k.master...), nil
}

func TestStoreGrantsEndToEnd(t *testing.T) {
	superURL, appURL := os.Getenv("MAILROOM_TEST_SUPER_URL"), os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	lc := newLeakCheck(t)
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(3*i + 1)
	}
	keys := staticKeys{master}
	userID, acct := store.NewULID(), store.NewULID()
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		OAuth:        &onboarding.OAuthSecret{Provider: ProviderMicrosoft, RefreshToken: sentRefresh},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		userID.String(), "sub-"+userID.String(), userID.String()+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, smtp_port,
	                                auth_kind, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'outlook', 'microsoft', 'outlook.office365.com', 'smtp.office365.com', 587,
	              'oauth2', $3, $4)`, acct.String(), userID.String(), ct, onboarding.CredentialsSchemeV1.String())
	st := store.NewFromPool(app)
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.CreateOAuthGrant(ctx, store.OAuthGrantRow{AccountID: acct, Provider: ProviderMicrosoft,
			Subject: "oid-1", Address: "you@example.com"})
	}); err != nil {
		t.Fatal(err)
	}
	changes := func() int {
		var n int
		_ = super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Account' AND object_id = $2`,
			userID.String(), acct.String()).Scan(&n)
		return n
	}

	var revoked atomic.Bool
	te, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		if revoked.Load() {
			oauthError(400, "invalid_grant", "AADSTS70000: the grant has been revoked")(n, f, w)
			return
		}
		okToken(sentAccess1, sentRefreshNew)(n, f, w)
	})
	grants := StoreGrants{Store: st, Keys: keys}
	m := newManager(t, lc, srv, grants, ProviderMicrosoft, nil)

	before := changes()
	tok, err := m.AccessToken(ctx, userID, acct)
	if lc.err(err) != nil || tok.Reveal() != sentAccess1 {
		t.Fatalf("AccessToken: %v", err)
	}
	if got := te.forms[0].Get("refresh_token"); got != sentRefresh {
		t.Error("the sealed refresh token was not the one sent")
	}
	g, err := grants.LoadGrant(ctx, userID, acct)
	if lc.err(err) != nil {
		t.Fatal(err)
	}
	if g.Generation != 2 || g.RefreshToken.Reveal() != sentRefreshNew {
		t.Fatalf("after rotation: generation %d, new token stored %v", g.Generation, g.RefreshToken.Reveal() == sentRefreshNew)
	}
	if got := changes() - before; got != 0 {
		t.Errorf("a rotation appended %d Account change rows, want 0", got)
	}
	// A stale CAS through the real store loses and writes nothing.
	won, err := grants.RotateRefreshToken(ctx, userID, acct, 1, sentRefreshNew2, time.Now())
	if lc.err(err) != nil || won {
		t.Fatalf("stale rotation won=%v err=%v", won, err)
	}
	if g, _ := grants.LoadGrant(ctx, userID, acct); g.RefreshToken.Reveal() != sentRefreshNew {
		t.Fatal("a stale rotation overwrote the newer refresh token")
	}

	// Revocation: the next refresh says invalid_grant.
	revoked.Store(true)
	m.Invalidate(userID, acct, tok)
	_, err = m.AccessToken(ctx, userID, acct)
	_ = lc.err(err)
	if !errors.Is(err, xoauth2.ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrReauthRequired", err)
	}
	var state, reason string
	var audits int
	_ = super.QueryRow(ctx, `SELECT state, state_reason FROM app.oauth_grants WHERE account_id = $1`, acct.String()).Scan(&state, &reason)
	_ = super.QueryRow(ctx, `SELECT count(*) FROM app.audit_log WHERE user_id = $1 AND action = 'oauth.state_changed'`, userID.String()).Scan(&audits)
	if state != store.OAuthStateNeedsReauth || reason != "invalid_grant" || audits != 1 {
		t.Errorf("after revocation: state=%q reason=%q audits=%d", state, reason, audits)
	}
	if got := changes() - before; got != 1 {
		t.Errorf("the needs_reauth transition appended %d change rows in total, want 1", got)
	}
	// And nobody else can load this grant: a different user id finds nothing.
	if _, err := grants.LoadGrant(ctx, ulid.Make(), acct); !errors.Is(err, store.ErrOAuthGrantNotFound) {
		t.Errorf("another identity loaded the grant: %v", err)
	}
}
