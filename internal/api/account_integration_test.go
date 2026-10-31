// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// End-to-end proof of the in-UI account surface (mailroom#98/#45) against real
// Postgres + RLS: Account/set create seals the credentials server-side, the
// password is NEVER returned by /set or /get (write-only), the stored ciphertext
// opens to exactly the supplied secret, and another user cannot read or mutate
// the account. A fake IMAP dialer stands in for the validation dial at the
// onboarding seam, so the sealing + write-only-password + cross-user refusal are
// genuinely exercised without a live IMAP server.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

// e2eMaster is a fixed 32-byte master key for the test sealer. Real deployments
// read it from the key provider; here it only has to be stable so the sealed
// ciphertext opens with the same key.
var e2eMaster = []byte("mailroom-account-e2e-master-key!")

// fakeAccountSecrets seals with the REAL onboarding path (SealCredentials) so the
// test proves the exact server-side derivation, not a stand-in.
type fakeAccountSecrets struct{ master []byte }

func (f fakeAccountSecrets) SealAccountCredentials(_ context.Context, userID ulid.ULID, creds onboarding.Credentials) ([]byte, ulid.ULID, error) {
	ct, err := onboarding.SealCredentials(f.master, userID, creds)
	return ct, onboarding.CredentialsSchemeV1, err
}

// newAccountAPI builds an API over the harness store with the account-management
// seams wired: the real-sealing fake secrets and a fake dialer that accepts any
// account and reports three folders.
func newAccountAPI(t *testing.T, st *store.Store) *API {
	t.Helper()
	dial := func(_ context.Context, _ imapsync.Config) (imapsync.SyncSession, error) {
		return &fakeIMAP{mailboxes: make([]imapsync.Mailbox, 3)}, nil
	}
	return New(st,
		WithAccountSecrets(fakeAccountSecrets{master: e2eMaster}),
		WithAccountDialer(dial),
		// These tests point at fixture hostnames that do not resolve, with the
		// dial itself faked above. Since #527 the validation path resolves and
		// address-checks BEFORE dialling, so without this the fixture fails as
		// "cannot resolve" and never reaches the fake dialer at all.
		//
		// ⚠ This does NOT weaken what these tests cover. The guard's own
		// behaviour is asserted in internal/onboarding/egressguard_test.go,
		// against a fake resolver and a recording dialer; what these tests are
		// about is Account/set's sealing, RLS and change_log semantics, and
		// making them carry the egress policy as well would couple two unrelated
		// things and leave both harder to read.
		WithAllowPrivateAccountHosts(true))
}

func TestAccountSetCreateSealsAndHidesPassword(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)
	api := newAccountAPI(t, h.store)
	ctx := context.Background()

	const password = "s3cret-app-password"
	res := call(t, api.accountSet, a.UserID, map[string]any{
		"create": map[string]any{
			"new": map[string]any{
				"name":     "work",
				"imapHost": "imap.example.com",
				"smtpHost": "smtp.example.com",
				"username": "you@example.com",
				"password": password,
			},
		},
	})

	createdMap, _ := res["created"].(map[string]any)
	obj, ok := createdMap["new"].(map[string]any)
	if !ok {
		t.Fatalf("account not created: %v", res)
	}
	// The created object carries the endpoints but NEVER the secret.
	if _, leaked := obj["password"]; leaked {
		t.Errorf("Account/set create leaked the password: %v", obj)
	}
	if _, leaked := obj["username"]; leaked {
		t.Errorf("Account/set create returned the (write-only) username: %v", obj)
	}
	if obj["imapPort"] != float64(993) || obj["smtpPort"] != float64(465) {
		t.Errorf("default implicit-TLS ports not applied: %v", obj)
	}
	newID, err := ulid.Parse(obj["id"].(string))
	if err != nil {
		t.Fatalf("bad created id %v: %v", obj["id"], err)
	}

	// The stored ciphertext opens (with the same derived key) to EXACTLY the
	// supplied secret — the password was sealed server-side, not dropped.
	var ct []byte
	if err := h.super.QueryRow(ctx, `SELECT credentials_ct FROM app.accounts WHERE id=$1`, newID.String()).Scan(&ct); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	creds, err := onboarding.OpenCredentials(e2eMaster, a.UserID, ct)
	if err != nil {
		t.Fatalf("stored credentials do not open: %v", err)
	}
	if creds.IMAPPassword != password || creds.SMTPPassword != password {
		t.Errorf("sealed password mismatch: %+v", creds)
	}
	if creds.IMAPUsername != "you@example.com" {
		t.Errorf("sealed username mismatch: %+v", creds)
	}

	// A default send-as identity was provisioned for the login address.
	var idn int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.identities WHERE account_id=$1 AND address='you@example.com'`,
		newID.String()).Scan(&idn); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if idn != 1 {
		t.Errorf("default identity count = %d, want 1", idn)
	}

	// Account/get NEVER returns the password (write-only), even when asked.
	got := call(t, api.accountGet, a.UserID, map[string]any{"ids": []string{newID.String()}})
	list := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Account/get own: %d objects, want 1", len(list))
	}
	gotObj := list[0].(map[string]any)
	if _, leaked := gotObj["password"]; leaked {
		t.Errorf("Account/get leaked the password: %v", gotObj)
	}
	if _, leaked := gotObj["username"]; leaked {
		t.Errorf("Account/get returned the write-only username: %v", gotObj)
	}

	// Cross-user: B cannot see A's new account by id, nor destroy it.
	crossGet := call(t, api.accountGet, b.UserID, map[string]any{"ids": []string{newID.String()}})
	if n := len(crossGet["list"].([]any)); n != 0 {
		t.Errorf("B/Account/get A's account leaked %d objects", n)
	}
	crossSet := call(t, api.accountSet, b.UserID, map[string]any{
		"destroy": []string{newID.String()},
	})
	if !mapHasKey(crossSet, "notDestroyed", newID.String()) {
		t.Errorf("B was allowed to destroy A's account: %v", crossSet)
	}
	var stillThere int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.accounts WHERE id=$1`, newID.String()).Scan(&stillThere); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if stillThere != 1 {
		t.Errorf("A's account was destroyed by B: count=%d, want 1", stillThere)
	}
}

// --- minimal SyncSession fake ------------------------------------------------

type fakeIMAP struct {
	mailboxes []imapsync.Mailbox
	listErr   error
}

func (s *fakeIMAP) ListMailboxes(context.Context) ([]imapsync.Mailbox, error) {
	return s.mailboxes, s.listErr
}
func (s *fakeIMAP) Select(context.Context, string, imapsync.SelectOptions) (*imapsync.MailboxStatus, error) {
	return nil, nil
}
func (s *fakeIMAP) FetchByUID(context.Context, imapsync.UIDSet, imapsync.FetchItems, func(imapsync.Message) error) error {
	return nil
}
func (s *fakeIMAP) LiveUIDs(context.Context, string) (*imapsync.MailboxStatus, []imapsync.UID, error) {
	return nil, nil, nil
}
func (s *fakeIMAP) StoreFlags(context.Context, string, imapsync.UID, []imapsync.Flag, []imapsync.Flag) error {
	return nil
}
func (s *fakeIMAP) Copy(context.Context, string, imapsync.UIDSet, string) error   { return nil }
func (s *fakeIMAP) Move(context.Context, string, imapsync.UIDSet, string) error   { return nil }
func (s *fakeIMAP) Expunge(context.Context, string, imapsync.UIDSet) (int, error) { return 0, nil }
func (s *fakeIMAP) SearchMessageID(context.Context, string, string) ([]imapsync.UID, error) {
	return nil, nil
}
func (s *fakeIMAP) UIDPresent(context.Context, string, imapsync.UID) (bool, error) {
	return false, nil
}
func (s *fakeIMAP) Supports(imapsync.Capability) bool                  { return false }
func (s *fakeIMAP) Noop(context.Context) error                         { return nil }
func (s *fakeIMAP) Notifications() <-chan imapsync.Notification        { return nil }
func (s *fakeIMAP) Idle(context.Context) (imapsync.IdleSession, error) { return nil, nil }
func (s *fakeIMAP) Append(context.Context, string, []imapsync.Flag, time.Time, []byte) (imapsync.UID, error) {
	return 0, nil
}
func (s *fakeIMAP) Logout(context.Context) error { return nil }
func (s *fakeIMAP) Close() error                 { return nil }

var _ imapsync.SyncSession = (*fakeIMAP)(nil)
