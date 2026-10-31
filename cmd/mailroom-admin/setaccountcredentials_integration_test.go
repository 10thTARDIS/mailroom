// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Integration coverage for set-account-credentials (mailroom#571) against a
// real Postgres with db/schema.sql applied (scripts/integration-test.sh),
// reusing accounts_report_integration_test.go's harness and its REAL sealing
// fixture (onboarding.SealCredentials / createSealedAccount) — the established
// pattern this package's admin integration tests use.
//
// The validation dial is a FAKE IMAP session, exactly the seam
// internal/api/account_integration_test.go uses for the same JMAP Account/set
// connection-change path this command reuses ("A fake IMAP dialer stands in
// for the validation dial at the onboarding seam, so the sealing +
// write-only-password ... are genuinely exercised without a live IMAP
// server."). What is proved against REAL Postgres here is everything a fake
// dialer cannot fake: the validate-then-seal ORDER, the change_log invariant
// (CLAUDE.md), and that a rejected credential leaves the stored ciphertext
// byte-for-byte untouched.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/provider"
	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

// fakeCredentialCheckSession is a minimal SyncSession: only ListMailboxes is
// ever called by onboarding.ValidateAccount (internal/onboarding/onboarding.go
// validateIMAP), so everything else is an inert stub.
type fakeCredentialCheckSession struct{ mailboxes []imapsync.Mailbox }

func (s *fakeCredentialCheckSession) ListMailboxes(context.Context) ([]imapsync.Mailbox, error) {
	return s.mailboxes, nil
}
func (s *fakeCredentialCheckSession) Select(context.Context, string, imapsync.SelectOptions) (*imapsync.MailboxStatus, error) {
	return nil, nil
}
func (s *fakeCredentialCheckSession) FetchByUID(context.Context, imapsync.UIDSet, imapsync.FetchItems, func(imapsync.Message) error) error {
	return nil
}
func (s *fakeCredentialCheckSession) LiveUIDs(context.Context, string) (*imapsync.MailboxStatus, []imapsync.UID, error) {
	return nil, nil, nil
}
func (s *fakeCredentialCheckSession) StoreFlags(context.Context, string, imapsync.UID, []imapsync.Flag, []imapsync.Flag) error {
	return nil
}
func (s *fakeCredentialCheckSession) Copy(context.Context, string, imapsync.UIDSet, string) error {
	return nil
}
func (s *fakeCredentialCheckSession) Move(context.Context, string, imapsync.UIDSet, string) error {
	return nil
}
func (s *fakeCredentialCheckSession) Expunge(context.Context, string, imapsync.UIDSet) (int, error) {
	return 0, nil
}
func (s *fakeCredentialCheckSession) SearchMessageID(context.Context, string, string) ([]imapsync.UID, error) {
	return nil, nil
}
func (s *fakeCredentialCheckSession) UIDPresent(context.Context, string, imapsync.UID) (bool, error) {
	return false, nil
}
func (s *fakeCredentialCheckSession) Supports(imapsync.Capability) bool { return false }
func (s *fakeCredentialCheckSession) Noop(context.Context) error        { return nil }
func (s *fakeCredentialCheckSession) Notifications() <-chan imapsync.Notification {
	return nil
}
func (s *fakeCredentialCheckSession) Idle(context.Context) (imapsync.IdleSession, error) {
	return nil, nil
}
func (s *fakeCredentialCheckSession) Append(context.Context, string, []imapsync.Flag, time.Time, []byte) (imapsync.UID, error) {
	return 0, nil
}
func (s *fakeCredentialCheckSession) Logout(context.Context) error { return nil }
func (s *fakeCredentialCheckSession) Close() error                 { return nil }

var _ imapsync.SyncSession = (*fakeCredentialCheckSession)(nil)

// acceptingDialer returns a Dialer that succeeds only for the given
// username/password (an IMAP LOGIN failure otherwise) — a real server's
// behaviour, faked at the onboarding seam.
func acceptingDialer(wantUsername, wantPassword string) onboarding.Dialer {
	return func(_ context.Context, cfg imapsync.Config) (imapsync.SyncSession, error) {
		if cfg.Username != wantUsername || cfg.Password != wantPassword {
			return nil, &imapsync.Error{Kind: imapsync.ErrorKindAuth, Op: "dial",
				Err: errors.New("NO [AUTHENTICATIONFAILED] Authentication failed")}
		}
		return &fakeCredentialCheckSession{mailboxes: make([]imapsync.Mailbox, 2)}, nil
	}
}

// accountUpdatedChangeLogCount counts ObjectAccount 'updated' change_log rows
// for one account — the invariant assertion (CLAUDE.md): every mutation of a
// JMAP-visible object appends exactly one row in the same transaction as the
// mutation.
func accountUpdatedChangeLogCount(t *testing.T, h *accountsReportHarness, userID, accountID string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Account' AND op='updated' AND object_id=$2`,
		userID, accountID).Scan(&n); err != nil {
		t.Fatalf("count Account updated change_log rows: %v", err)
	}
	return n
}

func TestSetAccountCredentialsRotatesValidatesSealsAndAppendsOneChangeLogRow(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	tag := "set-creds-rotate-" + store.NewULID().String()
	userID := seedAccountsReportUser(t, h.super, tag)

	const (
		username    = "you@example.com"
		oldPassword = "S3cret-Marker-original"
		newPassword = "S3cret-Marker-rotated"
	)
	acct := createSealedAccount(t, h.store, userID, "work", username, oldPassword)
	if got := accountUpdatedChangeLogCount(t, h, userID.String(), acct.ID.String()); got != 0 {
		t.Fatalf("baseline updated change_log rows = %d, want 0", got)
	}

	var out bytes.Buffer
	keys := fakeMasterKeyProvider{key: accountsReportMaster}
	dial := acceptingDialer(username, newPassword)
	smtp := &fakeSMTPChecker{wantUsername: username, wantPassword: newPassword}
	err := setAccountCredentials(ctx, &out, h.store, keys, dial, true, userID, acct.ID,
		pendingCredential{Password: newPassword}, setCredentialsOptions{CheckSMTP: smtp.check})
	if err != nil {
		t.Fatalf("set-account-credentials: %v", err)
	}
	if strings.Contains(out.String(), oldPassword) || strings.Contains(out.String(), newPassword) {
		t.Errorf("report printed a password: %q", out.String())
	}
	// Same logins (the common case): both are written, and the SMTP side is
	// checked with the new pair before it is sealed.
	if len(smtp.calls) != 1 || smtp.calls[0].Username != username || smtp.calls[0].Password != newPassword {
		t.Errorf("SMTP AUTH checks = %d, want exactly one with the new shared login", len(smtp.calls))
	}
	if !strings.Contains(out.String(), "will change IMAP and SMTP") {
		t.Errorf("report should say both IMAP and SMTP will change, got %q", out.String())
	}

	var reread store.Account
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		reread, e = tx.GetAccount(ctx, acct.ID)
		return e
	}); err != nil {
		t.Fatalf("re-read account: %v", err)
	}
	got, err := onboarding.OpenCredentials(accountsReportMaster, userID, reread.CredentialsCT)
	if err != nil {
		t.Fatalf("open rotated credentials: %v", err)
	}
	if got.IMAPPassword != newPassword {
		t.Errorf("stored IMAP password = %q, want the rotated one", got.IMAPPassword)
	}
	if got.IMAPUsername != username {
		t.Errorf("stored IMAP username changed to %q, want unchanged %q", got.IMAPUsername, username)
	}
	// --password-file's single-login model (mirrors Account/set): SMTP follows
	// the same rotation.
	if got.SMTPPassword != newPassword || got.SMTPUsername != username {
		t.Errorf("SMTP credentials = %q/%q, want mirrored to the rotated IMAP login", got.SMTPUsername, got.SMTPPassword)
	}

	if got := accountUpdatedChangeLogCount(t, h, userID.String(), acct.ID.String()); got != 1 {
		t.Errorf("updated change_log rows after rotation = %d, want exactly 1", got)
	}

	// A sync succeeds afterwards: proven at the level this command controls —
	// the SAME credentials a sync pass would open now validate against the
	// SAME dialer, which is exactly what a real IMAP LOGIN re-checks.
	if _, err := dial(ctx, imapsync.Config{Username: got.IMAPUsername, Password: got.IMAPPassword}); err != nil {
		t.Errorf("a sync with the rotated credentials should now succeed: %v", err)
	}
}

func TestSetAccountCredentialsRejectsWrongPasswordAndLeavesStoredUnchanged(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	tag := "set-creds-reject-" + store.NewULID().String()
	userID := seedAccountsReportUser(t, h.super, tag)

	const (
		username      = "you@example.com"
		correctPass   = "S3cret-Marker-correct"
		wrongPassword = "S3cret-Marker-wrong"
	)
	acct := createSealedAccount(t, h.store, userID, "work", username, correctPass)

	var ctBefore []byte
	if err := h.super.QueryRow(ctx, `SELECT credentials_ct FROM app.accounts WHERE id=$1`, acct.ID.String()).Scan(&ctBefore); err != nil {
		t.Fatalf("read credentials_ct before: %v", err)
	}

	var out bytes.Buffer
	keys := fakeMasterKeyProvider{key: accountsReportMaster}
	dial := acceptingDialer(username, correctPass)
	err := setAccountCredentials(ctx, &out, h.store, keys, dial, true, userID, acct.ID,
		pendingCredential{Password: wrongPassword}, setCredentialsOptions{CheckSMTP: (&fakeSMTPChecker{}).check})
	if err == nil {
		t.Fatal("expected set-account-credentials to refuse a wrong password")
	}
	if strings.Contains(err.Error(), wrongPassword) || strings.Contains(err.Error(), correctPass) {
		t.Errorf("refusal error leaked a password: %q", err.Error())
	}
	if strings.Contains(out.String(), wrongPassword) || strings.Contains(out.String(), correctPass) {
		t.Errorf("refusal printed a password: %q", out.String())
	}

	var ctAfter []byte
	if err := h.super.QueryRow(ctx, `SELECT credentials_ct FROM app.accounts WHERE id=$1`, acct.ID.String()).Scan(&ctAfter); err != nil {
		t.Fatalf("read credentials_ct after: %v", err)
	}
	if !bytes.Equal(ctBefore, ctAfter) {
		t.Error("a rejected credential must leave the stored ciphertext byte-identical")
	}

	if got := accountUpdatedChangeLogCount(t, h, userID.String(), acct.ID.String()); got != 0 {
		t.Errorf("a rejected credential must append NO change_log row, got %d", got)
	}

	// A sync still works with the OLD password.
	if _, err := dial(ctx, imapsync.Config{Username: username, Password: correctPass}); err != nil {
		t.Errorf("a sync with the untouched original credentials should still succeed: %v", err)
	}
}

func TestSetAccountCredentialsIdenticalCredentialIsANoOp(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	tag := "set-creds-noop-" + store.NewULID().String()
	userID := seedAccountsReportUser(t, h.super, tag)

	const (
		username = "you@example.com"
		password = "S3cret-Marker-unchanged"
	)
	acct := createSealedAccount(t, h.store, userID, "work", username, password)

	var out bytes.Buffer
	keys := fakeMasterKeyProvider{key: accountsReportMaster}
	dial := acceptingDialer(username, password)
	err := setAccountCredentials(ctx, &out, h.store, keys, dial, true, userID, acct.ID,
		pendingCredential{Password: password}, setCredentialsOptions{CheckSMTP: (&fakeSMTPChecker{wantUsername: username, wantPassword: password}).check})
	if err != nil {
		t.Fatalf("set-account-credentials: %v", err)
	}
	if !strings.Contains(out.String(), "already matches") {
		t.Errorf("report should say the credential already matches what is stored, got %q", out.String())
	}
	if strings.Contains(out.String(), password) {
		t.Errorf("report printed the password: %q", out.String())
	}
	if got := accountUpdatedChangeLogCount(t, h, userID.String(), acct.ID.String()); got != 0 {
		t.Errorf("an identical credential must write nothing, got %d updated change_log row(s)", got)
	}
}

// fakeSMTPChecker stands in for the AUTH-only SMTP dial the way acceptingDialer
// stands in for the IMAP one: it accepts only wantUsername/wantPassword, and
// records every call so a test can assert the check ran (or did not).
type fakeSMTPChecker struct {
	wantUsername, wantPassword string
	calls                      []provider.SendRequest
}

func (f *fakeSMTPChecker) check(_ context.Context, req provider.SendRequest) error {
	f.calls = append(f.calls, req)
	if req.Username != f.wantUsername || req.Password != f.wantPassword {
		return errors.New("535 5.7.8 Authentication credentials invalid")
	}
	return nil
}

// sealedSeparateSMTPAccount creates an account whose stored SMTP login differs
// from its IMAP login (#655), by re-sealing the credentials of a
// createSealedAccount row directly (the fixture is not under test).
func sealedSeparateSMTPAccount(t *testing.T, h *accountsReportHarness, userID ulid.ULID, imapUser, imapPass, smtpUser, smtpPass string) store.Account {
	t.Helper()
	acct := createSealedAccount(t, h.store, userID, "work", imapUser, imapPass)
	ct, err := onboarding.SealCredentials(accountsReportMaster, userID, onboarding.Credentials{
		IMAPUsername: imapUser, IMAPPassword: imapPass, SMTPUsername: smtpUser, SMTPPassword: smtpPass,
	})
	if err != nil {
		t.Fatalf("seal separate-SMTP credentials: %v", err)
	}
	if _, err := h.super.Exec(context.Background(), `UPDATE app.accounts SET credentials_ct=$1 WHERE id=$2`, ct, acct.ID.String()); err != nil {
		t.Fatalf("store separate-SMTP credentials: %v", err)
	}
	return acct
}

func openStoredCredentials(t *testing.T, h *accountsReportHarness, userID ulid.ULID, accountID ulid.ULID) onboarding.Credentials {
	t.Helper()
	var ct []byte
	if err := h.super.QueryRow(context.Background(), `SELECT credentials_ct FROM app.accounts WHERE id=$1`, accountID.String()).Scan(&ct); err != nil {
		t.Fatalf("read credentials_ct: %v", err)
	}
	got, err := onboarding.OpenCredentials(accountsReportMaster, userID, ct)
	if err != nil {
		t.Fatalf("open stored credentials: %v", err)
	}
	return got
}

const (
	sepIMAPUser = "you@example.com"
	sepSMTPUser = "send@example.com"
	sepOldIMAP  = "S3cret-Marker-imap-old"
	sepSMTPPass = "S3cret-Marker-smtp-own"
	sepNewPass  = "S3cret-Marker-rotated-new"
)

func TestSetAccountCredentialsPasswordFileKeepsASeparateSMTPLogin(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	userID := seedAccountsReportUser(t, h.super, "set-creds-keep-smtp-"+store.NewULID().String())
	acct := sealedSeparateSMTPAccount(t, h, userID, sepIMAPUser, sepOldIMAP, sepSMTPUser, sepSMTPPass)

	var out bytes.Buffer
	smtp := &fakeSMTPChecker{}
	err := setAccountCredentials(ctx, &out, h.store, fakeMasterKeyProvider{key: accountsReportMaster},
		acceptingDialer(sepIMAPUser, sepNewPass), true, userID, acct.ID,
		pendingCredential{Password: sepNewPass}, setCredentialsOptions{CheckSMTP: smtp.check})
	if err != nil {
		t.Fatalf("set-account-credentials: %v", err)
	}
	got := openStoredCredentials(t, h, userID, acct.ID)
	if got.IMAPPassword != sepNewPass || got.IMAPUsername != sepIMAPUser {
		t.Errorf("IMAP login = %q/<password>, want rotated password under %q", got.IMAPUsername, sepIMAPUser)
	}
	if got.SMTPUsername != sepSMTPUser || got.SMTPPassword != sepSMTPPass {
		t.Errorf("stored SMTP login was replaced (%q): a separate SMTP login must be kept", got.SMTPUsername)
	}
	if !strings.Contains(out.String(), "SMTP login KEPT") || !strings.Contains(out.String(), "--smtp-too") {
		t.Errorf("report should say the SMTP login was kept and name --smtp-too, got %q", out.String())
	}
	if len(smtp.calls) != 0 {
		t.Errorf("a kept SMTP pair is not written, so no SMTP check should run; got %d", len(smtp.calls))
	}
	for _, secret := range []string{sepNewPass, sepOldIMAP, sepSMTPPass} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("report printed a password: %q", out.String())
		}
	}
	if n := accountUpdatedChangeLogCount(t, h, userID.String(), acct.ID.String()); n != 1 {
		t.Errorf("updated change_log rows = %d, want exactly 1", n)
	}
}

func TestSetAccountCredentialsSMTPTooReplacesBoth(t *testing.T) {
	h := newAccountsReportHarness(t)
	ctx := context.Background()
	userID := seedAccountsReportUser(t, h.super, "set-creds-smtp-too-"+store.NewULID().String())
	acct := sealedSeparateSMTPAccount(t, h, userID, sepIMAPUser, sepOldIMAP, sepSMTPUser, sepSMTPPass)

	var out bytes.Buffer
	// The SMTP check must be made against the SMTP login (send@), not the IMAP one.
	smtp := &fakeSMTPChecker{wantUsername: sepSMTPUser, wantPassword: sepNewPass}
	err := setAccountCredentials(ctx, &out, h.store, fakeMasterKeyProvider{key: accountsReportMaster},
		acceptingDialer(sepIMAPUser, sepNewPass), true, userID, acct.ID,
		pendingCredential{Password: sepNewPass}, setCredentialsOptions{SMTPToo: true, CheckSMTP: smtp.check})
	if err != nil {
		t.Fatalf("set-account-credentials --smtp-too: %v", err)
	}
	got := openStoredCredentials(t, h, userID, acct.ID)
	if got.IMAPPassword != sepNewPass || got.SMTPPassword != sepNewPass {
		t.Error("--smtp-too must write the new password to both IMAP and SMTP")
	}
	if got.SMTPUsername != sepSMTPUser {
		t.Errorf("--smtp-too must keep the SMTP USERNAME %q, got %q", sepSMTPUser, got.SMTPUsername)
	}
	if len(smtp.calls) != 1 {
		t.Errorf("SMTP AUTH checks = %d, want 1", len(smtp.calls))
	}
	if !strings.Contains(out.String(), "will change IMAP and SMTP") {
		t.Errorf("report should say both will change, got %q", out.String())
	}
}

func TestSetAccountCredentialsRefusesAndSealsNothingWhenSMTPAuthFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending pendingCredential
		opts    setCredentialsOptions
	}{
		{"password-file with --smtp-too", pendingCredential{Password: sepNewPass}, setCredentialsOptions{SMTPToo: true}},
		{"accounts file with its own smtp password", pendingCredential{
			Username: sepIMAPUser, Password: sepNewPass, SMTPUsername: sepSMTPUser, SMTPPassword: "S3cret-Marker-smtp-typo"}, setCredentialsOptions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccountsReportHarness(t)
			ctx := context.Background()
			userID := seedAccountsReportUser(t, h.super, "set-creds-smtp-reject-"+store.NewULID().String())
			acct := sealedSeparateSMTPAccount(t, h, userID, sepIMAPUser, sepOldIMAP, sepSMTPUser, sepSMTPPass)
			var ctBefore []byte
			if err := h.super.QueryRow(ctx, `SELECT credentials_ct FROM app.accounts WHERE id=$1`, acct.ID.String()).Scan(&ctBefore); err != nil {
				t.Fatalf("read credentials_ct before: %v", err)
			}

			// IMAP accepts the new password; the SMTP server rejects whatever is offered.
			smtp := &fakeSMTPChecker{wantUsername: "nobody", wantPassword: "nothing"}
			tc.opts.CheckSMTP = smtp.check
			var out bytes.Buffer
			err := setAccountCredentials(ctx, &out, h.store, fakeMasterKeyProvider{key: accountsReportMaster},
				acceptingDialer(sepIMAPUser, sepNewPass), true, userID, acct.ID, tc.pending, tc.opts)
			if err == nil {
				t.Fatal("expected a refusal when the SMTP credential does not authenticate")
			}
			if !strings.Contains(err.Error(), "SMTP") || !strings.Contains(err.Error(), "unchanged") {
				t.Errorf("refusal should name SMTP and say nothing changed, got %q", err.Error())
			}
			for _, secret := range []string{sepNewPass, sepSMTPPass, "S3cret-Marker-smtp-typo"} {
				if strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) {
					t.Errorf("output leaked a password: %q / %q", err.Error(), out.String())
				}
			}
			var ctAfter []byte
			if err := h.super.QueryRow(ctx, `SELECT credentials_ct FROM app.accounts WHERE id=$1`, acct.ID.String()).Scan(&ctAfter); err != nil {
				t.Fatalf("read credentials_ct after: %v", err)
			}
			if !bytes.Equal(ctBefore, ctAfter) {
				t.Error("a rejected SMTP credential must leave the stored ciphertext byte-identical")
			}
			if n := accountUpdatedChangeLogCount(t, h, userID.String(), acct.ID.String()); n != 0 {
				t.Errorf("a refused write must append no change_log row, got %d", n)
			}
		})
	}
}
