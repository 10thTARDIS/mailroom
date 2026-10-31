// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The OAuth connect surface (#52, ADR-046 increment 3) through the API: the
// two methods over a real oauthmail.Connector and store, the oauthProviders
// capability, Account/set's refusal of connection edits on an OAuth account
// (obligation (e)), Forget on account deletion (obligation (b)), API keys
// refused, and the cross-user probes registered into crossUserProbes.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/oauthmail"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// recordingConnector is the real Connector, recording Forget calls.
type recordingConnector struct {
	*oauthmail.Connector
	mu      stdsync.Mutex
	forgets []ulid.ULID
}

func (r *recordingConnector) Forget(u, a ulid.ULID) {
	r.mu.Lock()
	r.forgets = append(r.forgets, a)
	r.mu.Unlock()
	r.Connector.Forget(u, a)
}

// newTestConnector builds a Connector over st with both providers enabled.
// Nothing here reaches a network: Microsoft has no revocation endpoint and
// the probes never complete a callback.
func newTestConnector(t *testing.T, st *store.Store) *recordingConnector {
	return newTestConnectorWithRevoke(t, st, "")
}

// newTestConnectorWithRevoke points Google's revocation endpoint at revokeURL
// (empty: Google's real one is never reached because no test grant is Google's).
func newTestConnectorWithRevoke(t *testing.T, st *store.Store, revokeURL string) *recordingConnector {
	t.Helper()
	ms, err := oauthmail.Microsoft(oauthmail.Client{ID: "client-id", Secret: "client-secret"}, "common")
	if err != nil {
		t.Fatal(err)
	}
	g := oauthmail.Google(oauthmail.Client{ID: "client-id", Secret: "client-secret"})
	if revokeURL != "" {
		g.RevokeURL = revokeURL
	}
	reg, err := oauthmail.NewRegistry(g, ms)
	if err != nil {
		t.Fatal(err)
	}
	keys := fakeMasterKeys{}
	mgr := oauthmail.NewManager(reg, oauthmail.StoreGrants{Store: st, Keys: keys})
	c, err := oauthmail.NewConnector(oauthmail.ConnectorConfig{Providers: reg, Store: st, Keys: keys,
		Manager: mgr, PublicURL: "https://mail.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return &recordingConnector{Connector: c}
}

// seedOAuthAccountForAPI gives user an OAuth (Microsoft) account with a grant
// and sealed credentials, and returns its id.
func seedOAuthAccountForAPI(t *testing.T, super *pgxpool.Pool, user ulid.ULID, subject string) ulid.ULID {
	t.Helper()
	master, _ := fakeMasterKeys{}.MasterKey(context.Background())
	ct, err := onboarding.SealCredentials(master, user, onboarding.Credentials{IMAPUsername: subject + "@example.com",
		OAuth: &onboarding.OAuthSecret{Provider: oauthmail.ProviderMicrosoft, RefreshToken: "1//refresh-" + subject, Subject: subject}})
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewULID()
	ctx := context.Background()
	if _, err := super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, smtp_port,
	        auth_kind, credentials_ct, credentials_dek) VALUES ($1, $2, $3, 'microsoft', 'outlook.office365.com',
	        'smtp.office365.com', 587, 'oauth2', $4, $5)`,
		id.String(), user.String(), "oauth-"+id.String(), ct, onboarding.CredentialsSchemeV1.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address)
	        VALUES ($1, $2, 'microsoft', $3, $4)`, id.String(), user.String(), subject, subject+"@example.com"); err != nil {
		t.Fatal(err)
	}
	return id
}

func init() {
	crossUserProbes["Account/oauthStart"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		prev := h.api.oauth
		h.api.oauth = newTestConnector(t, h.store)
		defer func() { h.api.oauth = prev }()
		bAcct := seedOAuthAccountForAPI(t, h.super, b.UserID, "b-sub-"+b.UserID.String())
		// (1) Naming B's JMAP account is refused before anything is read.
		if _, mErr := invokeAs(t, h, "Account/oauthStart", a.UserID,
			map[string]any{"accountId": b.UserID.String(), "provider": "microsoft"}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("oauthStart naming B's JMAP account = %+v, want accountNotFound", mErr)
		}
		// (2) Re-authenticating B's mail account is "no such account", and no
		// flow is written for anyone.
		_, mErr := invokeAs(t, h, "Account/oauthStart", a.UserID, map[string]any{"provider": "microsoft", "id": bAcct.String()})
		if mErr == nil || mErr.Type != errInvalidArguments || !strings.Contains(mErr.Description, "no such account") {
			t.Fatalf("oauthStart re-auth of B's account = %+v, want invalidArguments/no such account", mErr)
		}
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.oauth_flows WHERE user_id IN ($1, $2)`, a.UserID.String(), b.UserID.String())
		if n != 0 {
			t.Errorf("a refused cross-user start wrote %d flow rows", n)
		}
		// Positive control: B can start a re-auth of its own account.
		if _, mErr := invokeAs(t, h, "Account/oauthStart", b.UserID, map[string]any{"provider": "microsoft", "id": bAcct.String()}); mErr != nil {
			t.Errorf("control: B re-authenticating its own account: %+v", mErr)
		}
	}
	crossUserProbes["Account/oauthDisconnect"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		prev := h.api.oauth
		h.api.oauth = newTestConnector(t, h.store)
		defer func() { h.api.oauth = prev }()
		bAcct := seedOAuthAccountForAPI(t, h.super, b.UserID, "b2-sub-"+b.UserID.String())
		_, mErr := invokeAs(t, h, "Account/oauthDisconnect", a.UserID, map[string]any{"id": bAcct.String()})
		if mErr == nil || mErr.Type != errInvalidArguments {
			t.Fatalf("A disconnecting B's account = %+v, want invalidArguments", mErr)
		}
		var grants int
		var syncOn bool
		superScan(t, h, &grants, `SELECT count(*) FROM app.oauth_grants WHERE account_id = $1`, bAcct.String())
		superScan(t, h, &syncOn, `SELECT sync_enabled FROM app.accounts WHERE id = $1`, bAcct.String())
		if grants != 1 || !syncOn {
			t.Errorf("A's disconnect reached B's account: grants=%d sync=%v", grants, syncOn)
		}
		// Positive control: B can disconnect it.
		res, mErr := invokeAs(t, h, "Account/oauthDisconnect", b.UserID, map[string]any{"id": bAcct.String()})
		if mErr != nil || res["revocation"] != "unsupported" {
			t.Errorf("control: B disconnecting its own account = %v, %+v", res, mErr)
		}
	}
}

func TestOAuthMethodsThroughJMAP(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "oauthapi-"+store.NewULID().String())
	conn := newTestConnector(t, h.store)
	h.api.oauth = conn
	call := func(method string, args map[string]any) (map[string]any, *MethodError) {
		raw, mErr := h.api.methods[method](context.Background(), f.UserID, mustJSON(t, args))
		if mErr != nil {
			return nil, mErr
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out, nil
	}

	res, mErr := call("Account/oauthStart", map[string]any{"provider": "google"})
	if mErr != nil {
		t.Fatalf("oauthStart: %+v", mErr)
	}
	authURL, _ := res["authorizationUrl"].(string)
	if !strings.HasPrefix(authURL, "https://accounts.google.com/o/oauth2/v2/auth?") || res["expiresAt"] == "" {
		t.Errorf("oauthStart = %v", res)
	}
	if _, mErr := call("Account/oauthStart", map[string]any{"provider": "yahoo"}); mErr == nil || mErr.Type != errOAuthNotImplemented {
		t.Errorf("unknown provider = %+v, want notImplemented", mErr)
	}
	if _, mErr := call("Account/oauthStart", map[string]any{"provider": "google", "id": f.AccountID.String()}); mErr == nil ||
		!strings.Contains(mErr.Description, "password") {
		t.Errorf("re-auth of a password account = %+v", mErr)
	}
	if _, mErr := call("Account/oauthDisconnect", map[string]any{"id": f.AccountID.String()}); mErr == nil ||
		!strings.Contains(mErr.Description, "password") {
		t.Errorf("disconnect of a password account = %+v", mErr)
	}

	// API keys are refused, by the handler as well as by the scope table.
	for _, m := range []string{"Account/oauthStart", "Account/oauthDisconnect"} {
		ctx := withPrincipal(context.Background(), &keyPrincipal{key: store.APIKey{Name: "agent"}})
		if _, mErr := h.api.methods[m](ctx, f.UserID, mustJSON(t, map[string]any{"provider": "google", "id": f.AccountID.String()})); mErr == nil || mErr.Type != errForbidden {
			t.Errorf("%s with an API key = %+v, want forbidden", m, mErr)
		}
		if pol, ok := keyPolicies[m]; !ok || !pol.sessionOnly {
			t.Errorf("%s is not sessionOnly in the scope table", m)
		}
	}

	// Without a connector both answer notImplemented.
	h.api.oauth = nil
	if _, mErr := call("Account/oauthStart", map[string]any{"provider": "google"}); mErr == nil || mErr.Type != errOAuthNotImplemented {
		t.Errorf("oauthStart without OAuth = %+v", mErr)
	}
}

// The capability advertises exactly the configured providers, in both maps.
func TestOAuthProvidersCapability(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "oauthcap-"+store.NewULID().String())
	session := func() map[string]any {
		rec := httptest.NewRecorder()
		auth.DevAuth(http.HandlerFunc(h.api.SessionHandler), f.UserID).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/session", nil))
		var s map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("session: %v (%s)", err, rec.Body.String())
		}
		return s
	}
	providers := func(s map[string]any) (top, acct string) {
		caps := s["capabilities"].(map[string]any)
		b, _ := json.Marshal(caps[capAccounts].(map[string]any)["oauthProviders"])
		for _, a := range s["accounts"].(map[string]any) {
			ac := a.(map[string]any)["accountCapabilities"].(map[string]any)
			bb, _ := json.Marshal(ac[capAccounts].(map[string]any)["oauthProviders"])
			acct = string(bb)
		}
		return string(b), acct
	}
	if top, acct := providers(session()); top != "[]" || acct != "[]" {
		t.Errorf("without OAuth: oauthProviders = %s / %s, want []", top, acct)
	}
	h.api.oauth = newTestConnector(t, h.store)
	if top, acct := providers(session()); top != `["google","microsoft"]` || acct != top {
		t.Errorf("with OAuth: oauthProviders = %s / %s", top, acct)
	}
}

// OBLIGATION (e): connection fields are immutable on an OAuth account, refused
// before anything is sealed or dialled; metadata stays editable. OBLIGATION
// (b): deleting the account forgets its cached token.
func TestAccountSetOnOAuthAccount(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "oauthset-"+store.NewULID().String())
	acct := seedOAuthAccountForAPI(t, h.super, f.UserID, "sub-"+f.UserID.String())
	var ctBefore []byte
	if err := h.super.QueryRow(context.Background(), `SELECT credentials_ct FROM app.accounts WHERE id = $1`, acct.String()).Scan(&ctBefore); err != nil {
		t.Fatal(err)
	}
	dials := 0
	h.api.accountSecrets = sealingSecrets{}
	h.api.accountDialer = func(context.Context, sync.Config) (sync.SyncSession, error) {
		dials++
		return nil, errDialRefusedForTest
	}
	conn := newTestConnector(t, h.store)
	h.api.oauth = conn

	set := func(args map[string]any) map[string]any {
		raw, mErr := h.api.methods["Account/set"](context.Background(), f.UserID, mustJSON(t, args))
		if mErr != nil {
			t.Fatalf("Account/set: %+v", mErr)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	res := set(map[string]any{"update": map[string]any{acct.String(): map[string]any{
		"imapHost": "imap.attacker.test", "smtpHost": "smtp.attacker.test", "username": "x@example.com", "password": "pw",
	}}})
	nu, _ := res["notUpdated"].(map[string]any)
	e, _ := nu[acct.String()].(map[string]any)
	if e == nil || e["type"] != setInvalidProps || !strings.Contains(e["description"].(string), "OAuth") {
		t.Fatalf("connection edit on an OAuth account = %v", res)
	}
	if dials != 0 {
		t.Errorf("the refused edit dialled %d times (it must refuse before validating)", dials)
	}
	var ctAfter []byte
	_ = h.super.QueryRow(context.Background(), `SELECT credentials_ct FROM app.accounts WHERE id = $1`, acct.String()).Scan(&ctAfter)
	if string(ctAfter) != string(ctBefore) {
		t.Error("the refused edit changed the sealed credentials")
	}
	// Metadata is still editable.
	res = set(map[string]any{"update": map[string]any{acct.String(): map[string]any{"name": "Work mail"}}})
	if up, _ := res["updated"].(map[string]any); up == nil || !mapHas(up, acct.String()) {
		t.Errorf("rename of an OAuth account = %v", res)
	}
	// Destroy forgets the account's cached token.
	res = set(map[string]any{"destroy": []string{acct.String()}})
	if d, _ := res["destroyed"].([]any); len(d) != 1 {
		t.Fatalf("destroy = %v", res)
	}
	if len(conn.forgets) != 1 || conn.forgets[0] != acct {
		t.Errorf("Forget calls = %v, want exactly the destroyed account", conn.forgets)
	}
}

func mapHas(m map[string]any, k string) bool { _, ok := m[k]; return ok }

// SECURITY REVIEW M1: deleting an OAuth account revokes its grant at the
// provider (Google here, through a fake revocation endpoint), after the
// delete; deleting a password account revokes nothing; a refused delete (not
// the caller's account) revokes nothing either.
func TestAccountDestroyRevokesOAuthGrant(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "revoke-"+store.NewULID().String())
	other := seedAPIUser(t, h.super, "revoke-other-"+store.NewULID().String())
	var mu stdsync.Mutex
	var revoked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		revoked = append(revoked, r.PostForm.Get("token"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h.api.oauth = newTestConnectorWithRevoke(t, h.store, srv.URL)

	seedGoogle := func(user ulid.ULID, refresh string) ulid.ULID {
		master, _ := fakeMasterKeys{}.MasterKey(context.Background())
		ct, err := onboarding.SealCredentials(master, user, onboarding.Credentials{IMAPUsername: "g@example.com",
			OAuth: &onboarding.OAuthSecret{Provider: oauthmail.ProviderGoogle, RefreshToken: refresh, Subject: "gsub-" + refresh}})
		if err != nil {
			t.Fatal(err)
		}
		id := store.NewULID()
		ctx := context.Background()
		if _, err := h.super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host,
		        smtp_port, auth_kind, credentials_ct, credentials_dek, sync_enabled) VALUES ($1, $2, $3, 'gmail', 'imap.gmail.com',
		        'smtp.gmail.com', 465, 'oauth2', $4, $5, false)`,
			id.String(), user.String(), "g-"+id.String(), ct, onboarding.CredentialsSchemeV1.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(ctx, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address)
		        VALUES ($1, $2, 'google', $3, 'g@example.com')`, id.String(), user.String(), "gsub-"+refresh); err != nil {
			t.Fatal(err)
		}
		return id
	}
	destroy := func(as ulid.ULID, id string) map[string]any {
		raw, mErr := h.api.methods["Account/set"](context.Background(), as, mustJSON(t, map[string]any{"destroy": []string{id}}))
		if mErr != nil {
			t.Fatalf("Account/set: %+v", mErr)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}

	mine := seedGoogle(f.UserID, "1//refresh-mine")
	theirs := seedGoogle(other.UserID, "1//refresh-theirs")

	// Another user's destroy reaches nothing and revokes nothing.
	if res := destroy(f.UserID, theirs.String()); len(asArr(res["destroyed"])) != 0 {
		t.Fatalf("destroyed another user's account: %v", res)
	}
	// A password account (an empty one: the fixture's holds mail, whose blob
	// files this harness has no store to reclaim): deleted, nothing revoked.
	pw := store.NewULID()
	if _, err := h.super.Exec(context.Background(), `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host,
	        credentials_ct, credentials_dek) VALUES ($1, $2, 'pw-empty', 'imap.example.com', 'smtp.example.com', '\x01', $3)`,
		pw.String(), f.UserID.String(), store.NewULID().String()); err != nil {
		t.Fatal(err)
	}
	if res := destroy(f.UserID, pw.String()); len(asArr(res["destroyed"])) != 1 {
		t.Fatalf("password destroy = %v", res)
	}
	mu.Lock()
	n := len(revoked)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("revocations after a password-account and a foreign destroy = %d, want 0", n)
	}
	// The OAuth account: deleted, and its refresh token revoked.
	if res := destroy(f.UserID, mine.String()); len(asArr(res["destroyed"])) != 1 {
		t.Fatalf("oauth destroy = %v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "1//refresh-mine" {
		t.Errorf("revocations = %v, want exactly the destroyed account's refresh token", revoked)
	}
}

// THE GMAIL SYNC HOLD (oauthmail.SyncHold): a held Gmail OAuth account shows
// the reason in Account/getHealth and refuses syncEnabled:true; switching it
// off still works. A Microsoft OAuth account is not held: no reason, and
// syncEnabled:true is accepted.
func TestGmailSyncHold(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "synchold-"+store.NewULID().String())
	gmail := seedOAuthAccountForAPI(t, h.super, f.UserID, "g-"+f.UserID.String())
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.accounts SET provider_profile = 'gmail', sync_enabled = false WHERE id = $1`, gmail.String()); err != nil {
		t.Fatal(err)
	}
	ms := seedOAuthAccountForAPI(t, h.super, f.UserID, "m-"+f.UserID.String())
	if _, err := h.super.Exec(context.Background(), `UPDATE app.accounts SET sync_enabled = false WHERE id = $1`, ms.String()); err != nil {
		t.Fatal(err)
	}
	call := func(method string, args map[string]any) map[string]any {
		raw, mErr := h.api.methods[method](context.Background(), f.UserID, mustJSON(t, args))
		if mErr != nil {
			t.Fatalf("%s: %+v", method, mErr)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	syncOn := func(id ulid.ULID) bool {
		var on bool
		if err := h.super.QueryRow(context.Background(), `SELECT sync_enabled FROM app.accounts WHERE id = $1`, id.String()).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}

	// The reason, in the health panel.
	health := call("Account/getHealth", map[string]any{})
	holds := map[string]any{}
	for _, item := range asArr(health["list"]) {
		obj, _ := item.(map[string]any)
		holds[obj["id"].(string)] = obj["syncHold"]
	}
	gh, _ := holds[gmail.String()].(map[string]any)
	if gh == nil || gh["code"] != oauthmail.SyncHoldGmailPending || gh["message"] != oauthmail.SyncHoldGmailPendingMessage {
		t.Errorf("Gmail syncHold = %v", holds[gmail.String()])
	}
	if holds[ms.String()] != nil || holds[f.AccountID.String()] != nil {
		t.Errorf("syncHold on a Microsoft (%v) or password (%v) account", holds[ms.String()], holds[f.AccountID.String()])
	}

	// Turning a held account on is refused; the row is unchanged.
	res := call("Account/set", map[string]any{"update": map[string]any{gmail.String(): map[string]any{"syncEnabled": true}}})
	nu, _ := res["notUpdated"].(map[string]any)
	e, _ := nu[gmail.String()].(map[string]any)
	if e == nil || e["type"] != setInvalidProps || !strings.Contains(e["description"].(string), oauthmail.SyncHoldGmailPending) {
		t.Fatalf("syncEnabled:true on a held Gmail account = %v", res)
	}
	if syncOn(gmail) {
		t.Fatal("the refused syncEnabled:true switched sync on")
	}
	// Switching it off, and other edits, pass.
	res = call("Account/set", map[string]any{"update": map[string]any{gmail.String(): map[string]any{"syncEnabled": false, "name": "Gmail"}}})
	if up, _ := res["updated"].(map[string]any); !mapHas(up, gmail.String()) {
		t.Errorf("syncEnabled:false on a held account = %v", res)
	}
	// Microsoft is not held.
	res = call("Account/set", map[string]any{"update": map[string]any{ms.String(): map[string]any{"syncEnabled": true}}})
	if up, _ := res["updated"].(map[string]any); !mapHas(up, ms.String()) || !syncOn(ms) {
		t.Errorf("syncEnabled:true on a Microsoft account = %v (sync %v)", res, syncOn(ms))
	}
}

type sealingSecrets struct{}

func (sealingSecrets) SealAccountCredentials(context.Context, ulid.ULID, onboarding.Credentials) ([]byte, ulid.ULID, error) {
	return []byte("sealed"), onboarding.CredentialsSchemeV1, nil
}

var errDialRefusedForTest = &sync.Error{Kind: sync.ErrorKindTransient, Op: "dial", Err: http.ErrServerClosed}
