// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package oauthmail

// The one Web Push per transition into needs_reauth (notify.go), over the real
// grant store and the real job table: exactly one notice however many workers
// hear invalid_grant at once, none after a restart, a fresh one for a fresh
// transition, none for a transition the user already undid, a quiet-hours
// deferral that keeps the notice rather than dropping it, and operator_error
// never pushed to the user but reported for the operator.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

type fakePusher struct {
	mu    stdsync.Mutex
	sent  []string // account ids
	quiet bool
}

func (f *fakePusher) PushReconnectNeeded(_ context.Context, _, accountID ulid.ULID, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quiet {
		return true, nil
	}
	f.sent = append(f.sent, accountID.String())
	return false, nil
}

func (f *fakePusher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type notifyHarness struct {
	t      *testing.T
	ctx    context.Context
	super  *pgxpool.Pool
	st     *store.Store
	keys   staticKeys
	user   ulid.ULID
	acct   ulid.ULID
	grants StoreGrants
}

func newNotifyHarness(t *testing.T) *notifyHarness {
	t.Helper()
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
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(5*i + 2)
	}
	h := &notifyHarness{t: t, ctx: ctx, super: super, st: store.NewFromPool(app), keys: staticKeys{master},
		user: store.NewULID(), acct: store.NewULID()}
	h.grants = StoreGrants{Store: h.st, Keys: h.keys}
	ct, err := onboarding.SealCredentials(master, h.user, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		OAuth:        &onboarding.OAuthSecret{Provider: ProviderMicrosoft, RefreshToken: sentRefresh},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		h.user.String(), "sub-"+h.user.String(), h.user.String()+"@example.com")
	h.exec(`INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, smtp_port,
	                                auth_kind, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'outlook', 'microsoft', 'outlook.office365.com', 'smtp.office365.com', 587,
	              'oauth2', $3, $4)`, h.acct.String(), h.user.String(), ct, onboarding.CredentialsSchemeV1.String())
	if err := h.st.UserTx(ctx, h.user, func(tx *store.UserTx) error {
		return tx.CreateOAuthGrant(ctx, store.OAuthGrantRow{AccountID: h.acct, Provider: ProviderMicrosoft,
			Subject: "oid-1", Address: "you@example.com"})
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *notifyHarness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.super.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("exec: %v", err)
	}
}

func (h *notifyHarness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.super.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("count: %v", err)
	}
	return n
}

func (h *notifyHarness) notices() int {
	return h.count(`SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = $2`, h.user.String(), ReauthNotifyJobKind)
}

// runPending runs the handler over every pending notice job, the way the
// dispatcher would, and marks each done.
func (h *notifyHarness) runPending(handler func(context.Context, store.Job) error) int {
	h.t.Helper()
	rows, err := h.super.Query(h.ctx, `SELECT id, payload FROM app.jobs
		WHERE user_id = $1 AND kind = $2 AND state = 'pending' ORDER BY fire_at`, h.user.String(), ReauthNotifyJobKind)
	if err != nil {
		h.t.Fatal(err)
	}
	type row struct {
		id      string
		payload []byte
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.payload); err != nil {
			h.t.Fatal(err)
		}
		todo = append(todo, r)
	}
	rows.Close()
	for _, r := range todo {
		id := ulid.MustParse(r.id)
		u := h.user
		if err := handler(h.ctx, store.Job{ID: id, UserID: &u, Kind: ReauthNotifyJobKind, Payload: r.payload}); err != nil {
			h.t.Fatalf("handler: %v", err)
		}
		h.exec(`UPDATE app.jobs SET state = 'done' WHERE id = $1`, r.id)
	}
	return len(todo)
}

func (h *notifyHarness) setState(state string) {
	h.t.Helper()
	if err := h.st.UserTx(h.ctx, h.user, func(tx *store.UserTx) error {
		_, err := tx.SetOAuthGrantState(h.ctx, h.acct, state, "", "user", time.Now())
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

func TestReauthNoticeExactlyOncePerTransition(t *testing.T) {
	h := newNotifyHarness(t)
	lc := newLeakCheck(t)
	te, srv := newTokenEndpoint(t, oauthError(400, "invalid_grant", "AADSTS70000: revoked"))

	// Two workers — two processes, each with its own Manager and cache —
	// hear invalid_grant at the same moment, several callers each.
	a := newManager(t, lc, srv, h.grants, ProviderMicrosoft, nil)
	b := newManager(t, lc, srv, h.grants, ProviderMicrosoft, nil)
	var wg stdsync.WaitGroup
	var reauth atomic.Int32
	for i := 0; i < 8; i++ {
		m := a
		if i%2 == 1 {
			m = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.AccessToken(h.ctx, h.user, h.acct)
			if errors.Is(lc.err(err), xoauth2.ErrReauthRequired) {
				reauth.Add(1)
			}
		}()
	}
	wg.Wait()
	if reauth.Load() != 8 {
		t.Fatalf("%d of 8 callers were told to reconnect", reauth.Load())
	}
	if n := h.notices(); n != 1 {
		t.Fatalf("one transition queued %d notices, want 1", n)
	}

	// A restart: a brand-new Manager with nothing in memory. It must not even
	// ask the provider, and so cannot make a second transition or notice.
	calls := te.calls.Load()
	c := newManager(t, lc, srv, h.grants, ProviderMicrosoft, nil)
	if _, err := c.AccessToken(h.ctx, h.user, h.acct); !errors.Is(lc.err(err), xoauth2.ErrReauthRequired) {
		t.Fatalf("after restart: %v", err)
	}
	if te.calls.Load() != calls {
		t.Error("a restarted worker asked the provider again about a grant it already refused")
	}
	if n := h.notices(); n != 1 {
		t.Fatalf("after a restart there are %d notices, want 1", n)
	}

	push := &fakePusher{}
	handler := ReauthNotifyHandler(h.st, push, lc.logger(), nil)
	if ran := h.runPending(handler); ran != 1 || push.count() != 1 {
		t.Fatalf("ran %d jobs, pushed %d; want 1 and 1", ran, push.count())
	}
	// Nothing left to run: a second dispatcher sweep sends nothing more.
	if ran := h.runPending(handler); ran != 0 || push.count() != 1 {
		t.Fatalf("second sweep ran %d, pushed total %d", ran, push.count())
	}

	// The user reconnects, and later the grant dies again: a NEW transition,
	// which earns its own single notice.
	h.setState(store.OAuthStateActive)
	time.Sleep(5 * time.Millisecond)
	if _, err := a.AccessToken(h.ctx, h.user, h.acct); !errors.Is(lc.err(err), xoauth2.ErrReauthRequired) {
		t.Fatalf("second death: %v", err)
	}
	if n := h.notices(); n != 2 {
		t.Fatalf("two transitions queued %d notices, want 2", n)
	}
	if ran := h.runPending(handler); ran != 1 || push.count() != 2 {
		t.Fatalf("second transition: ran %d, pushed total %d", ran, push.count())
	}
}

func TestReauthNoticeStaleAndQuiet(t *testing.T) {
	h := newNotifyHarness(t)
	lc := newLeakCheck(t)
	_, srv := newTokenEndpoint(t, oauthError(400, "invalid_grant", "revoked"))
	m := newManager(t, lc, srv, h.grants, ProviderMicrosoft, nil)
	push := &fakePusher{}
	handler := ReauthNotifyHandler(h.st, push, lc.logger(), nil)

	// Reconnected before the notice went out: nothing is sent.
	_, _ = m.AccessToken(h.ctx, h.user, h.acct)
	h.setState(store.OAuthStateActive)
	if ran := h.runPending(handler); ran != 1 || push.count() != 0 {
		t.Fatalf("stale notice: ran %d, pushed %d; want 1 and 0", ran, push.count())
	}

	// Broken again, during quiet hours: the notice is deferred, not dropped.
	time.Sleep(5 * time.Millisecond)
	_, _ = m.AccessToken(h.ctx, h.user, h.acct)
	push.quiet = true
	if ran := h.runPending(handler); ran != 1 || push.count() != 0 {
		t.Fatalf("quiet: ran %d, pushed %d", ran, push.count())
	}
	deferred := h.count(`SELECT count(*) FROM app.jobs WHERE user_id = $1 AND kind = $2 AND state = 'pending'
		AND fire_at > now() + interval '10 minutes'`, h.user.String(), ReauthNotifyJobKind)
	if deferred != 1 {
		t.Fatalf("quiet hours left %d deferred notices, want 1", deferred)
	}
	// Quiet hours end; the deferred job runs and the notice is sent once.
	push.quiet = false
	if ran := h.runPending(handler); ran != 1 || push.count() != 1 {
		t.Fatalf("after quiet: ran %d, pushed %d", ran, push.count())
	}
}

func TestOperatorErrorIsNotPushedToUsers(t *testing.T) {
	h := newNotifyHarness(t)
	lc := newLeakCheck(t)
	var fixed atomic.Bool
	_, srv := newTokenEndpoint(t, func(n int32, f url.Values, w http.ResponseWriter) {
		if fixed.Load() {
			okToken(sentAccess1, "")(n, f, w)
			return
		}
		oauthError(401, "invalid_client", "AADSTS7000222: the client secret has expired")(n, f, w)
	})
	m := newManager(t, lc, srv, h.grants, ProviderMicrosoft, nil)
	if _, err := m.AccessToken(h.ctx, h.user, h.acct); !errors.Is(lc.err(err), xoauth2.ErrProviderMisconfigured) {
		t.Fatalf("err = %v", err)
	}
	if n := h.notices(); n != 0 {
		t.Fatalf("operator_error queued %d user notices, want 0", n)
	}
	var state string
	_ = h.super.QueryRow(h.ctx, `SELECT state FROM app.oauth_grants WHERE account_id = $1`, h.acct.String()).Scan(&state)
	if state != store.OAuthStateOperatorError {
		t.Fatalf("state = %q", state)
	}
	issues := m.OperatorIssues()
	if len(issues) != 1 || issues[0].Provider != ProviderMicrosoft || issues[0].Code != "invalid_client" || issues[0].Accounts != 1 {
		t.Fatalf("operator issues = %+v", issues)
	}
	// The operator fixes the secret: the next refresh succeeds, the grant is
	// active again with nobody reconnecting, and the notice is gone.
	fixed.Store(true)
	if _, err := m.AccessToken(h.ctx, h.user, h.acct); lc.err(err) != nil {
		t.Fatalf("after the fix: %v", err)
	}
	_ = h.super.QueryRow(h.ctx, `SELECT state FROM app.oauth_grants WHERE account_id = $1`, h.acct.String()).Scan(&state)
	if state != store.OAuthStateActive || len(m.OperatorIssues()) != 0 {
		t.Fatalf("after the fix: state %q, issues %+v", state, m.OperatorIssues())
	}
	if n := h.notices(); n != 0 {
		t.Fatalf("recovery queued %d notices", n)
	}
}
