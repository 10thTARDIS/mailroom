// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the account store surface against a real database:
// CreateAccount / ListSyncableAccounts round-trip (including the encrypted
// credentials bytes), GetAccount, and SetAccountSyncEnabled toggling
// visibility in BOTH listings (ListSyncableAccounts / ListDisabledAccounts,
// mailroom#435). Seeds a user via the superuser pool (bypassing RLS), then does
// every mutation/read through the mailroom_app store — the production path.
// Skips when the integration database is not configured.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestCreateAndListSyncableAccounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	userID := NewULID()
	uniq := userID.String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		uniq, "sub-"+uniq, uniq+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	ct := []byte{0x01, 0x02, 0x03, 0x04}
	dek := NewULID()

	var accountID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		id, err := tx.CreateAccount(ctx, AccountRow{
			Name:            "work",
			ProviderProfile: "migadu",
			IMAPHost:        "imap.example.com",
			IMAPPort:        993,
			SMTPHost:        "smtp.example.com",
			SMTPPort:        465,
			CredentialsCT:   ct,
			CredentialsDEK:  dek,
			SyncEnabled:     true,
		})
		accountID = id
		return err
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// Round-trip through ListSyncableAccounts + GetAccount.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		accts, err := tx.ListSyncableAccounts(ctx)
		if err != nil {
			return err
		}
		if len(accts) != 1 {
			t.Fatalf("ListSyncableAccounts len = %d, want 1", len(accts))
		}
		a := accts[0]
		if a.ID != accountID {
			t.Errorf("account id = %s, want %s", a.ID, accountID)
		}
		if a.Name != "work" || a.ProviderProfile != "migadu" {
			t.Errorf("name/profile = %q/%q", a.Name, a.ProviderProfile)
		}
		if a.IMAPHost != "imap.example.com" || a.IMAPPort != 993 {
			t.Errorf("imap endpoint = %s:%d", a.IMAPHost, a.IMAPPort)
		}
		if a.SMTPHost != "smtp.example.com" || a.SMTPPort != 465 {
			t.Errorf("smtp endpoint = %s:%d", a.SMTPHost, a.SMTPPort)
		}
		if !bytes.Equal(a.CredentialsCT, ct) {
			t.Errorf("credentials ciphertext round-trip mismatch: got %x", a.CredentialsCT)
		}
		if a.CredentialsDEK != dek {
			t.Errorf("credentials dek = %s, want %s", a.CredentialsDEK, dek)
		}
		if !a.SyncEnabled {
			t.Error("account should be sync-enabled")
		}
		// Negative control for the disabled listing (mailroom#435): an enabled
		// account must not appear in it, or the runner's WARN would fire for
		// every healthy account.
		dis, err := tx.ListDisabledAccounts(ctx)
		if err != nil {
			return err
		}
		if len(dis) != 0 {
			t.Errorf("ListDisabledAccounts len = %d for an enabled account, want 0", len(dis))
		}

		got, err := tx.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if got.ID != accountID || !bytes.Equal(got.CredentialsCT, ct) {
			t.Error("GetAccount round-trip mismatch")
		}
		return nil
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Disabling removes it from the syncable set but not from GetAccount.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if err := tx.SetAccountSyncEnabled(ctx, accountID, false); err != nil {
			return err
		}
		accts, err := tx.ListSyncableAccounts(ctx)
		if err != nil {
			return err
		}
		if len(accts) != 0 {
			t.Errorf("disabled account still listed as syncable (len=%d)", len(accts))
		}
		// ...and it appears in the disabled listing, which is what makes it
		// visible to the operator at all (mailroom#435).
		dis, err := tx.ListDisabledAccounts(ctx)
		if err != nil {
			return err
		}
		if len(dis) != 1 || dis[0].ID != accountID {
			t.Errorf("ListDisabledAccounts = %d account(s), want exactly the disabled one", len(dis))
		}
		got, err := tx.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if got.SyncEnabled {
			t.Error("account should now be sync-disabled")
		}
		return nil
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	userID := NewULID()
	uniq := userID.String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		uniq, "sub-"+uniq, uniq+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, err := tx.GetAccount(ctx, NewULID())
		if !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("GetAccount(missing) = %v, want ErrAccountNotFound", err)
		}
		if err := tx.SetAccountSyncEnabled(ctx, NewULID(), false); !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("SetAccountSyncEnabled(missing) = %v, want ErrAccountNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// --- in-UI account management (mailroom#98): Account is now JMAP-visible -------
//
// These prove CreateAccount/UpdateAccount/DeleteAccount append the change_log
// row in the same tx, that a duplicate name is a savepoint-isolated
// ErrAccountNameTaken (the surrounding tx stays usable), and that destroy
// cascades the account's surface AND removes its exclusively-owned mail.

func mustParseULID(t *testing.T, s string) ulid.ULID {
	t.Helper()
	id, err := ulid.Parse(s)
	if err != nil {
		t.Fatalf("parse ulid %q: %v", s, err)
	}
	return id
}

func sampleAccountRow(name string) AccountRow {
	return AccountRow{
		Name:           name,
		IMAPHost:       "imap.example.com",
		SMTPHost:       "smtp.example.com",
		CredentialsCT:  []byte{0x01, 0x02},
		CredentialsDEK: NewULID(),
		SyncEnabled:    true,
	}
}

func accountState(t *testing.T, h *harness, userID string) int64 {
	t.Helper()
	var seq int64
	if err := h.super.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT last_seq FROM app.change_seq WHERE user_id=$1 AND object_type='Account'),0)`,
		userID).Scan(&seq); err != nil {
		t.Fatalf("read Account state: %v", err)
	}
	return seq
}

func TestCreateAccountChangeLogAndNameCollision(t *testing.T) {
	h := newHarness(t)
	u := seedUser(t, h.super, "acct-create-"+NewULID().String())
	uid := mustParseULID(t, u.ID)
	st := h.appStore()
	ctx := context.Background()

	var id1, id2 ulid.ULID
	err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		var e error
		id1, e = tx.CreateAccount(ctx, sampleAccountRow("work"))
		if e != nil {
			return e
		}
		// A duplicate name is a recoverable ErrAccountNameTaken, and the SAVEPOINT
		// keeps the surrounding tx usable — a second, distinct create still works.
		if _, e := tx.CreateAccount(ctx, sampleAccountRow("work")); !errors.Is(e, ErrAccountNameTaken) {
			t.Fatalf("duplicate name: err = %v, want ErrAccountNameTaken", e)
		}
		id2, e = tx.CreateAccount(ctx, sampleAccountRow("home"))
		return e
	})
	if err != nil {
		t.Fatalf("create accounts: %v", err)
	}

	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Account' AND op='created'
		   AND object_id = ANY($2)`,
		u.ID, []string{id1.String(), id2.String()}).Scan(&n); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	if n != 2 {
		t.Errorf("Account created change_log rows = %d, want 2", n)
	}
}

func TestUpdateAccountRenameAndNoOp(t *testing.T) {
	h := newHarness(t)
	u := seedUser(t, h.super, "acct-update-"+NewULID().String())
	uid := mustParseULID(t, u.ID)
	st := h.appStore()
	ctx := context.Background()

	var id ulid.ULID
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		var e error
		id, e = tx.CreateAccount(ctx, sampleAccountRow("work"))
		return e
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	seqAfterCreate := accountState(t, h, u.ID)

	newName := "work-renamed"
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		found, e := tx.UpdateAccount(ctx, id, AccountPatch{Name: &newName})
		if e != nil {
			return e
		}
		if !found {
			t.Fatal("rename: found=false")
		}
		return nil
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	var gotName string
	if err := h.super.QueryRow(ctx, `SELECT name FROM app.accounts WHERE id=$1`, id.String()).Scan(&gotName); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if gotName != newName {
		t.Errorf("name = %q, want %q", gotName, newName)
	}
	seqAfterRename := accountState(t, h, u.ID)
	if seqAfterRename <= seqAfterCreate {
		t.Errorf("rename did not advance Account state: %d <= %d", seqAfterRename, seqAfterCreate)
	}

	// A no-op patch (same name) touches nothing and appends NO change_log row.
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		found, e := tx.UpdateAccount(ctx, id, AccountPatch{Name: &newName})
		if e != nil {
			return e
		}
		if !found {
			t.Fatal("no-op update: found=false (row exists)")
		}
		return nil
	}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if s := accountState(t, h, u.ID); s != seqAfterRename {
		t.Errorf("no-op update advanced Account state: %d != %d", s, seqAfterRename)
	}
}

func TestDeleteAccountCascadesAndRemovesOwnMail(t *testing.T) {
	h := newHarness(t)
	u := seedUser(t, h.super, "acct-delete-"+NewULID().String())
	uid := mustParseULID(t, u.ID)
	st := h.appStore()
	ctx := context.Background()

	acctID := mustParseULID(t, u.AccountID)
	// The seeded message is provenanced only in u.AccountID, so destroying the
	// account must remove it (no ghost) along with its mailboxes/provenance.
	var found bool
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		var e error
		found, _, e = tx.DeleteAccount(ctx, acctID)
		return e
	}); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if !found {
		t.Fatal("delete account: found=false")
	}

	assertGone := func(table, col, val string) {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.`+table+` WHERE `+col+`=$1`, val).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("app.%s still has %d rows for %s=%s after account delete", table, n, col, val)
		}
	}
	assertGone("accounts", "id", u.AccountID)
	assertGone("mailboxes", "account_id", u.AccountID)
	assertGone("message_provenance", "account_id", u.AccountID)
	assertGone("messages", "id", u.MessageID) // the exclusively-owned message is gone

	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Account' AND op='destroyed' AND object_id=$2`,
		u.ID, u.AccountID).Scan(&n); err != nil {
		t.Fatalf("count destroyed change_log: %v", err)
	}
	if n != 1 {
		t.Errorf("Account destroyed change_log rows = %d, want 1", n)
	}
}
