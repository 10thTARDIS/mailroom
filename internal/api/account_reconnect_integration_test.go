// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Health and UX for OAuth accounts (#52, ADR-046 §5, increment 4) through the
// API: the read-only auth properties on Account/get, the auth section of
// Account/getHealth, the Account change row each grant transition owes (and
// the one a refresh does not), EmailSubmission/set's refusal on an account
// waiting for its user to reconnect, and the admin-only operator notices.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/oauthmail"
	"mailroom/internal/store"
)

func (h *apiHarness) invoke(t *testing.T, method string, user ulid.ULID, args map[string]any) map[string]any {
	t.Helper()
	raw, mErr := h.api.methods[method](context.Background(), user, mustJSON(t, args))
	if mErr != nil {
		t.Fatalf("%s: %+v", method, mErr)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func accountByID(t *testing.T, res map[string]any, id ulid.ULID) map[string]any {
	t.Helper()
	for _, a := range res["list"].([]any) {
		m := a.(map[string]any)
		if m["id"] == id.String() {
			return m
		}
	}
	t.Fatalf("account %s not in %v", id, res["list"])
	return nil
}

func TestAccountAuthStateGetHealthAndChangeRows(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	f := seedAPIUser(t, h.super, "authstate-"+store.NewULID().String())
	oauth := seedOAuthAccountForAPI(t, h.super, f.UserID, "sub-"+f.UserID.String())

	get := func() map[string]any { return h.invoke(t, "Account/get", f.UserID, map[string]any{}) }
	health := func(id ulid.ULID) map[string]any {
		res := h.invoke(t, "Account/getHealth", f.UserID, map[string]any{})
		return accountByID(t, res, id)["auth"].(map[string]any)
	}

	res := get()
	pw, oa := accountByID(t, res, f.AccountID), accountByID(t, res, oauth)
	if pw["authKind"] != "password" || pw["authState"] != nil || pw["authProvider"] != nil || pw["authAddress"] != nil {
		t.Errorf("password account auth props = %v %v %v %v", pw["authKind"], pw["authState"], pw["authProvider"], pw["authAddress"])
	}
	if oa["authKind"] != "oauth2" || oa["authState"] != "active" || oa["authProvider"] != "microsoft" ||
		oa["authAddress"] != "sub-"+f.UserID.String()+"@example.com" {
		t.Errorf("oauth account auth props = %v %v %v %v", oa["authKind"], oa["authState"], oa["authProvider"], oa["authAddress"])
	}
	if hp := health(f.AccountID); hp["kind"] != "password" || hp["state"] != nil {
		t.Errorf("password health auth = %v", hp)
	}
	if ha := health(oauth); ha["kind"] != "oauth2" || ha["state"] != "active" || ha["reason"] != nil ||
		ha["provider"] != "microsoft" || ha["lastRefreshAt"] != nil || ha["stateChangedAt"] == nil {
		t.Errorf("active oauth health auth = %v", ha)
	}

	// A TRANSITION appends the Account change row, and Account/changes shows it.
	since := res["state"].(string)
	if err := h.store.UserTx(ctx, f.UserID, func(tx *store.UserTx) error {
		_, err := tx.SetOAuthGrantState(ctx, oauth, store.OAuthStateNeedsReauth, "invalid_grant", "system", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ch := h.invoke(t, "Account/changes", f.UserID, map[string]any{"sinceState": since})
	if up := ch["updated"].([]any); len(up) != 1 || up[0] != oauth.String() {
		t.Fatalf("Account/changes after the transition = %v, want exactly the OAuth account updated", ch)
	}
	res = get()
	if accountByID(t, res, oauth)["authState"] != "needs_reauth" {
		t.Errorf("authState after the transition = %v", accountByID(t, res, oauth)["authState"])
	}
	if ha := health(oauth); ha["state"] != "needs_reauth" || ha["reason"] != "invalid_grant" {
		t.Errorf("needs_reauth health auth = %v", ha)
	}

	// A refresh stamp is not visible on Account/get and appends NO row
	// (ADR-046 §4); getHealth, a computed read, shows it.
	since = res["state"].(string)
	if err := h.store.UserTx(ctx, f.UserID, func(tx *store.UserTx) error {
		return tx.RecordOAuthRefresh(ctx, oauth, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	ch = h.invoke(t, "Account/changes", f.UserID, map[string]any{"sinceState": since})
	if up := ch["updated"].([]any); len(up) != 0 {
		t.Errorf("a refresh stamp appended an Account change: %v", ch)
	}
	if ha := health(oauth); ha["lastRefreshAt"] == nil {
		t.Errorf("getHealth does not report the refresh: %v", ha)
	}

	// Disconnected: no grant, the provider still named from the profile.
	if err := h.store.UserTx(ctx, f.UserID, func(tx *store.UserTx) error {
		_, err := tx.DisconnectOAuthGrant(ctx, oauth, []byte("resealed"), "unsupported")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ch = h.invoke(t, "Account/changes", f.UserID, map[string]any{"sinceState": since})
	if up := ch["updated"].([]any); len(up) != 1 {
		t.Errorf("disconnect: Account/changes = %v", ch)
	}
	oa = accountByID(t, get(), oauth)
	if oa["authState"] != "disconnected" || oa["authProvider"] != "microsoft" || oa["authAddress"] != nil {
		t.Errorf("disconnected auth props = %v %v %v", oa["authState"], oa["authProvider"], oa["authAddress"])
	}
	if ha := health(oauth); ha["state"] != "disconnected" || ha["provider"] != "microsoft" {
		t.Errorf("disconnected health auth = %v", ha)
	}

	// The auth properties are read-only.
	set := h.invoke(t, "Account/set", f.UserID, map[string]any{"update": map[string]any{
		oauth.String(): map[string]any{"authState": "active"}}})
	nu, _ := set["notUpdated"].(map[string]any)
	if e, _ := nu[oauth.String()].(map[string]any); e == nil || e["type"] != setInvalidProps {
		t.Errorf("Account/set authState = %v, want invalidProperties", set)
	}
}

type fakeOperatorStatus struct{ issues []oauthmail.OperatorIssue }

func (f fakeOperatorStatus) OperatorIssues() []oauthmail.OperatorIssue { return f.issues }

func TestOperatorNoticesAreForAdminsOnly(t *testing.T) {
	h := newAPIHarness(t)
	f := seedAPIUser(t, h.super, "opnotice-"+store.NewULID().String())
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	h.api.oauthOps = fakeOperatorStatus{issues: []oauthmail.OperatorIssue{
		{Provider: "microsoft", Code: "invalid_client", Since: at, LastSeen: at.Add(time.Hour), Accounts: 3}}}

	res := h.invoke(t, "Account/getHealth", f.UserID, map[string]any{})
	if _, ok := res["operatorNotices"]; ok {
		t.Fatalf("a non-admin was shown operator notices: %v", res["operatorNotices"])
	}
	if _, err := h.super.Exec(context.Background(), `UPDATE app.users SET role = 'admin' WHERE id = $1`, f.UserID.String()); err != nil {
		t.Fatal(err)
	}
	res = h.invoke(t, "Account/getHealth", f.UserID, map[string]any{})
	notices, ok := res["operatorNotices"].([]any)
	if !ok || len(notices) != 1 {
		t.Fatalf("admin operatorNotices = %v", res["operatorNotices"])
	}
	n := notices[0].(map[string]any)
	if n["provider"] != "microsoft" || n["code"] != "invalid_client" || n["accounts"] != float64(3) || n["since"] != "2026-10-27T09:00:00Z" {
		t.Errorf("notice = %v", n)
	}
	// No issue standing: an admin sees an empty list, not an absent one.
	h.api.oauthOps = fakeOperatorStatus{}
	res = h.invoke(t, "Account/getHealth", f.UserID, map[string]any{})
	if notices, ok := res["operatorNotices"].([]any); !ok || len(notices) != 0 {
		t.Errorf("admin with nothing standing: operatorNotices = %v", res["operatorNotices"])
	}
}

func TestSubmissionRefusedWhileAwaitingReconnect(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	ctx := context.Background()
	if _, err := h.super.Exec(ctx, `UPDATE app.accounts SET auth_kind = 'oauth2', provider_profile = 'microsoft'
		WHERE id = $1`, a.AccountID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, state)
		VALUES ($1, $2, 'microsoft', 'oid-1', 'me@example.com', 'needs_reauth')`, a.AccountID.String(), a.UserID.String()); err != nil {
		t.Fatal(err)
	}
	emailID := composeDraft(t, h, a.UserID, "me@example.com", "waiting-send")
	res := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	if e := setError(t, res, "notCreated", "s1"); e["type"] != setAccountNeedsReconnect {
		t.Fatalf("SetError = %v, want %q", e, setAccountNeedsReconnect)
	}
	if got := submissionRowStates(t, h, a.UserID, emailID); len(got) != 0 {
		t.Errorf("a refused send left submission rows %v", got)
	}
	// The control: reconnected, the identical send is accepted.
	if _, err := h.super.Exec(ctx, `UPDATE app.oauth_grants SET state = 'active' WHERE account_id = $1`, a.AccountID.String()); err != nil {
		t.Fatal(err)
	}
	res = call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	if !mapHasKey(res, "created", "s1") {
		t.Fatalf("after reconnect the send was refused: %v", res)
	}
}

// Review of #903: a DISCONNECTED OAuth account (no grant) refuses a new send
// like needs_reauth; an operator_error account ACCEPTS it (the server's broken
// sign-in is not the user's to fix — the send worker holds it); and a held
// submission says so on EmailSubmission/get (mailroomHeld).
func TestSubmissionOnDisconnectedAndOperatorErrorAccounts(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	ctx := context.Background()
	if _, err := h.super.Exec(ctx, `UPDATE app.accounts SET auth_kind = 'oauth2', provider_profile = 'microsoft'
		WHERE id = $1`, a.AccountID.String()); err != nil {
		t.Fatal(err)
	}
	submit := func(emailID string) map[string]any {
		return call(t, h.api.submissionSet, a.UserID, map[string]any{
			"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
		})
	}

	// No grant at all: disconnected.
	e1 := composeDraft(t, h, a.UserID, "me@example.com", "disconnected-send")
	if e := setError(t, submit(e1), "notCreated", "s1"); e["type"] != setAccountNeedsReconnect {
		t.Fatalf("disconnected: SetError = %v, want %q", e, setAccountNeedsReconnect)
	}
	if got := submissionRowStates(t, h, a.UserID, e1); len(got) != 0 {
		t.Errorf("a refused send left rows %v", got)
	}

	// operator_error: accepted.
	if _, err := h.super.Exec(ctx, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, state)
		VALUES ($1, $2, 'microsoft', 'oid-1', 'me@example.com', 'operator_error')`, a.AccountID.String(), a.UserID.String()); err != nil {
		t.Fatal(err)
	}
	e2 := composeDraft(t, h, a.UserID, "me@example.com", "operator-send")
	res := submit(e2)
	if !mapHasKey(res, "created", "s1") {
		t.Fatalf("operator_error refused a send: %v", res)
	}
	subID := res["created"].(map[string]any)["s1"].(map[string]any)["id"].(string)

	// Held by the worker (simulated: the reason only the worker writes):
	// EmailSubmission/get says why it is waiting.
	held := func() any {
		got := call(t, h.api.submissionGet, a.UserID, map[string]any{"ids": []string{subID}})
		return got["list"].([]any)[0].(map[string]any)["mailroomHeld"]
	}
	if v := held(); v != nil {
		t.Fatalf("a submission not held reports mailroomHeld=%v", v)
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.submissions SET smtp_response = $2 WHERE id = $1`,
		subID, store.HeldProviderReason("microsoft")); err != nil {
		t.Fatal(err)
	}
	if v := held(); v != store.HeldKindProvider {
		t.Fatalf("mailroomHeld = %v, want %q", v, store.HeldKindProvider)
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.submissions SET smtp_response = $2 WHERE id = $1`,
		subID, store.HeldReconnectReason); err != nil {
		t.Fatal(err)
	}
	if v := held(); v != store.HeldKindReconnect {
		t.Fatalf("mailroomHeld = %v, want %q", v, store.HeldKindReconnect)
	}
}
