// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Integration coverage for accounts-report (mailroom#538) against a real
// Postgres with db/schema.sql applied (scripts/integration-test.sh). The pure
// core (accounts_report_test.go) proves the comparison logic against fixtures;
// this proves the one thing that can't be faked there — opening a REAL sealed
// credentials row, written by the REAL sealing path (onboarding.SealCredentials,
// through store.CreateAccount), with a real master key. No IMAP dial anywhere.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
)

// accountsReportMaster is a fixed 32-byte master key for this test's sealer —
// stable, not secret, exactly like internal/api/account_integration_test.go's
// e2eMaster. It only has to be the same key used to seal and to open.
var accountsReportMaster = []byte("accounts-report-integration-key!")

// fakeMasterKeyProvider hands back a fixed, already-in-memory master key —
// this test seals its own fixtures directly and has no file secret to read.
type fakeMasterKeyProvider struct{ key []byte }

func (f fakeMasterKeyProvider) MasterKey(context.Context) ([]byte, error) {
	// A copy, matching FileKeyProvider's contract that callers own and may
	// zero the returned buffer — openStoredAccounts defers crypto.Zero(master)
	// on whatever it gets back, and zeroing this test's shared key out from
	// under a later call would break every subsequent test in the package.
	out := make([]byte, len(f.key))
	copy(out, f.key)
	return out, nil
}

// accountsReportHarness mirrors untangleHarness: a superuser pool for fixture
// seeding (bypasses RLS, needed to create the user in the first place) and a
// Store backed by the mailroom_app pool — the exact production configuration
// accounts-report runs through for everything after the user exists.
type accountsReportHarness struct {
	super *pgxpool.Pool
	store *store.Store
}

func newAccountsReportHarness(t *testing.T) *accountsReportHarness {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)
	return &accountsReportHarness{super: super, store: store.NewFromPool(app)}
}

func seedAccountsReportUser(t *testing.T, super *pgxpool.Pool, tag string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email, display_name) VALUES ($1,$2,$3,$4)`,
		id.String(), "oidc-"+tag, tag+"@example.com", "User "+tag); err != nil {
		t.Fatalf("seed user %s: %v", tag, err)
	}
	return id
}

// createSealedAccount seals creds with the REAL onboarding path
// (onboarding.SealCredentials) and creates the account through
// store.CreateAccount (the sanctioned write path, RLS-scoped) — exactly what
// onboarding.AddAccount does internally, minus the IMAP dial.
func createSealedAccount(t *testing.T, st *store.Store, userID ulid.ULID, name, imapUsername, imapPassword string) store.Account {
	t.Helper()
	ctx := context.Background()
	ct, err := onboarding.SealCredentials(accountsReportMaster, userID, onboarding.Credentials{
		IMAPUsername: imapUsername,
		IMAPPassword: imapPassword,
		SMTPUsername: imapUsername,
		SMTPPassword: imapPassword,
	})
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	var accountID ulid.ULID
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		accountID, e = tx.CreateAccount(ctx, store.AccountRow{
			Name:           name,
			IMAPHost:       "imap.example.com",
			IMAPPort:       993,
			SMTPHost:       "smtp.example.com",
			SMTPPort:       465,
			CredentialsCT:  ct,
			CredentialsDEK: onboarding.CredentialsSchemeV1,
			EnvelopeVer:    1,
			SyncEnabled:    true,
		})
		if e != nil {
			return e
		}
		// Mirror reconcileIdentities' username-default rule (cmd/mailroom/
		// main.go): a real onboarded account with no declared identities/
		// wildcard domains ends up with exactly one fixed identity for its
		// username. Without this, declaredIdentities' matching default would
		// (correctly) report it "declared but not present", which is real
		// behavior but not what this test is measuring.
		_, e = tx.CreateIdentity(ctx, store.IdentityRow{
			AccountID: accountID, Kind: store.IdentityFixed,
			Address: imapUsername, Enabled: true,
		})
		return e
	}); err != nil {
		t.Fatalf("create account %q: %v", name, err)
	}
	var acct store.Account
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		acct, e = tx.GetAccount(ctx, accountID)
		return e
	}); err != nil {
		t.Fatalf("get account %q: %v", name, err)
	}
	return acct
}

// TestAccountsReportOpensRealSealedCredentials is the integration half of the
// password comparison: gather+compare against a REAL DB row and a REAL master
// key, both directions — a declared password that matches the sealed one
// reports "matches", and one that does not (a rotation the file made that was
// never applied) reports DIFFERS and counts as drift. This is exactly the
// scenario that motivated #538: the file was edited, the database was not.
func TestAccountsReportOpensRealSealedCredentials(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	tag := "accounts-report-" + store.NewULID().String()
	userID := seedAccountsReportUser(t, h.super, tag)

	const storedPassword = "S3cret-Marker-original"
	acct := createSealedAccount(t, h.store, userID, "work", "you@example.com", storedPassword)

	gather := func() []store.Account {
		var accounts []store.Account
		if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			accounts, e = tx.ListAccounts(ctx)
			return e
		}); err != nil {
			t.Fatalf("list accounts: %v", err)
		}
		return accounts
	}
	identities := func() []store.Identity {
		var out []store.Identity
		if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			out, e = tx.ListIdentities(ctx)
			return e
		}); err != nil {
			t.Fatalf("list identities: %v", err)
		}
		return out
	}

	keys := fakeMasterKeyProvider{key: accountsReportMaster}

	t.Run("declared password matches", func(t *testing.T) {
		stored, err := openStoredAccounts(ctx, keys, userID, gather(), identities())
		if err != nil {
			t.Fatalf("openStoredAccounts: %v", err)
		}
		declared := []config.Account{{
			Name: "work", IMAPHost: "imap.example.com", IMAPPort: 993,
			SMTPHost: "smtp.example.com", SMTPPort: 465,
			Username: "you@example.com", Password: config.Secret(storedPassword),
		}}
		report := compareAccounts(declared, stored)
		notes := notesFor(t, report, "work")
		if !hasFieldNote(notes, "password", "matches") {
			t.Errorf("notes = %v; want password: matches", notes)
		}
		if report.drift() {
			t.Errorf("drift() = true with the matching stored password; driftCount=%d, notes=%v",
				report.driftCount(), notes)
		}
	})

	t.Run("declared password differs (unapplied rotation)", func(t *testing.T) {
		stored, err := openStoredAccounts(ctx, keys, userID, gather(), identities())
		if err != nil {
			t.Fatalf("openStoredAccounts: %v", err)
		}
		declared := []config.Account{{
			Name: "work", IMAPHost: "imap.example.com", IMAPPort: 993,
			SMTPHost: "smtp.example.com", SMTPPort: 465,
			Username: "you@example.com", Password: config.Secret("a-rotated-password-never-applied"),
		}}
		report := compareAccounts(declared, stored)
		notes := notesFor(t, report, "work")
		if !hasFieldNote(notes, "password", "DIFFERS") {
			t.Errorf("notes = %v; want password: DIFFERS", notes)
		}
		if !report.drift() {
			t.Error("drift() = false for an unapplied password rotation; want true")
		}
	})

	_ = acct // acct.ID is available for readers debugging a failure; not asserted directly
}

// TestAccountsReportOpenCredentialsFailureNamesNoBytes is the negative-control
// half of the never-print rule: OpenCredentials refuses a ciphertext sealed
// under a DIFFERENT user id (the AAD binds the user id — internal/crypto), and
// the resulting error must name only the account, never any byte of the
// ciphertext or plaintext.
func TestAccountsReportOpenCredentialsFailureNamesNoBytes(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	tag := "accounts-report-badkey-" + store.NewULID().String()
	userID := seedAccountsReportUser(t, h.super, tag)
	otherTag := "accounts-report-other-" + store.NewULID().String()
	otherUserID := seedAccountsReportUser(t, h.super, otherTag)

	const marker = "S3cret-Marker-should-never-leak"
	// Seal for otherUserID, but store it (by hand, bypassing CreateAccount's
	// caller-supplied ct) under userID's account — the AAD mismatch this
	// produces is exactly what a corrupted/tampered row looks like.
	ct, err := onboarding.SealCredentials(accountsReportMaster, otherUserID, onboarding.Credentials{
		IMAPUsername: "you@example.com", IMAPPassword: marker,
	})
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	var accountID ulid.ULID
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		accountID, e = tx.CreateAccount(ctx, store.AccountRow{
			Name: "mismatched", IMAPHost: "imap.example.com", IMAPPort: 993,
			SMTPHost: "smtp.example.com", SMTPPort: 465,
			CredentialsCT: ct, CredentialsDEK: onboarding.CredentialsSchemeV1,
			EnvelopeVer: 1, SyncEnabled: true,
		})
		return e
	}); err != nil {
		t.Fatalf("create mismatched account: %v", err)
	}
	_ = accountID

	var accounts []store.Account
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		accounts, e = tx.ListAccounts(ctx)
		return e
	}); err != nil {
		t.Fatalf("list accounts: %v", err)
	}

	_, err = openStoredAccounts(ctx, fakeMasterKeyProvider{key: accountsReportMaster}, userID, accounts, nil)
	if err == nil {
		t.Fatal("openStoredAccounts: error = nil, want an AAD-mismatch failure opening the mismatched account")
	}
	if containsSubstr([]string{err.Error()}, marker) {
		t.Fatalf("error leaked the plaintext marker: %v", err)
	}
	if containsSubstr([]string{err.Error()}, string(ct)) {
		t.Fatalf("error leaked the raw ciphertext: %v", err)
	}
}
