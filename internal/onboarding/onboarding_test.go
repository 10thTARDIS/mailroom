// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// ---- fakes -----------------------------------------------------------------

type fakeKeys struct{ master []byte }

func (f fakeKeys) MasterKey(context.Context) ([]byte, error) {
	return append([]byte(nil), f.master...), nil
}

type fakeStore struct {
	created   store.AccountRow
	createdID ulid.ULID
	createErr error
	setErr    error
	disabled  map[string]bool // accountID -> sync disabled
}

func (f *fakeStore) CreateAccount(_ context.Context, _ ulid.ULID, row store.AccountRow) (ulid.ULID, error) {
	if f.createErr != nil {
		return ulid.ULID{}, f.createErr
	}
	f.created = row
	f.createdID = ulid.Make()
	return f.createdID, nil
}

func (f *fakeStore) SetAccountSyncEnabled(_ context.Context, _, accountID ulid.ULID, enabled bool) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.disabled == nil {
		f.disabled = map[string]bool{}
	}
	f.disabled[accountID.String()] = !enabled
	return nil
}

// fakeSession is a minimal SyncSession for validation tests.
type fakeSession struct {
	mailboxes []sync.Mailbox
	listErr   error
}

func (s *fakeSession) ListMailboxes(context.Context) ([]sync.Mailbox, error) {
	return s.mailboxes, s.listErr
}
func (s *fakeSession) Select(context.Context, string, sync.SelectOptions) (*sync.MailboxStatus, error) {
	return nil, nil
}
func (s *fakeSession) FetchByUID(context.Context, sync.UIDSet, sync.FetchItems, func(sync.Message) error) error {
	return nil
}
func (s *fakeSession) LiveUIDs(context.Context, string) (*sync.MailboxStatus, []sync.UID, error) {
	return nil, nil, nil
}
func (s *fakeSession) StoreFlags(context.Context, string, sync.UID, []sync.Flag, []sync.Flag) error {
	return nil
}
func (s *fakeSession) Copy(context.Context, string, sync.UIDSet, string) error   { return nil }
func (s *fakeSession) Move(context.Context, string, sync.UIDSet, string) error   { return nil }
func (s *fakeSession) Expunge(context.Context, string, sync.UIDSet) (int, error) { return 0, nil }
func (s *fakeSession) SearchMessageID(context.Context, string, string) ([]sync.UID, error) {
	return nil, nil
}
func (s *fakeSession) UIDPresent(context.Context, string, sync.UID) (bool, error) {
	return false, nil
}
func (s *fakeSession) Supports(sync.Capability) bool                  { return false }
func (s *fakeSession) Noop(context.Context) error                     { return nil }
func (s *fakeSession) Notifications() <-chan sync.Notification        { return nil }
func (s *fakeSession) Idle(context.Context) (sync.IdleSession, error) { return nil, nil }
func (s *fakeSession) Append(context.Context, string, []sync.Flag, time.Time, []byte) (sync.UID, error) {
	return 0, nil
}
func (s *fakeSession) Logout(context.Context) error { return nil }
func (s *fakeSession) Close() error                 { return nil }

var _ sync.SyncSession = (*fakeSession)(nil)

func validSpec() AccountSpec {
	return AccountSpec{
		Name:     "work",
		IMAPHost: "imap.example.com",
		IMAPPort: 993,
		SMTPHost: "smtp.example.com",
		SMTPPort: 465,
		Username: "you@example.com",
		Password: "hunter2",
		TLS:      sync.TLSImplicit,
	}
}

// ---- tests -----------------------------------------------------------------

func TestSpecValidate(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := map[string]func(*AccountSpec){
		"no name":     func(s *AccountSpec) { s.Name = "" },
		"no imaphost": func(s *AccountSpec) { s.IMAPHost = "" },
		"no smtphost": func(s *AccountSpec) { s.SMTPHost = "" },
		"no user":     func(s *AccountSpec) { s.Username = "" },
		"no pass":     func(s *AccountSpec) { s.Password = "" },
		"bad port":    func(s *AccountSpec) { s.IMAPPort = 70000 },
	}
	for name, mutate := range cases {
		s := validSpec()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestAddAccountSealsAndValidates(t *testing.T) {
	master := testMaster()
	userID := ulid.Make()
	st := &fakeStore{}

	dial := func(_ context.Context, cfg sync.Config) (sync.SyncSession, error) {
		if cfg.Username != "you@example.com" || cfg.Password != "hunter2" {
			t.Errorf("validation dial got wrong credentials: %q/%q", cfg.Username, cfg.Password)
		}
		if cfg.Host != "imap.example.com" || cfg.Port != 993 {
			t.Errorf("validation dial got wrong endpoint: %s:%d", cfg.Host, cfg.Port)
		}
		return &fakeSession{mailboxes: make([]sync.Mailbox, 3)}, nil
	}

	res, err := AddAccount(context.Background(), st, fakeKeys{master}, userID, validSpec(), WithDialer(dial), WithResolver(addrs(t, "93.184.216.34")))
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if !res.Validated {
		t.Error("expected Validated=true")
	}
	if res.MailboxesSeen != 3 {
		t.Errorf("MailboxesSeen = %d, want 3", res.MailboxesSeen)
	}
	if res.AccountID != st.createdID {
		t.Error("result account id != created id")
	}

	// The stored ciphertext must open to the supplied credentials, and the
	// SMTP fields must default to the IMAP login.
	got, err := OpenCredentials(master, userID, st.created.CredentialsCT)
	if err != nil {
		t.Fatalf("open stored credentials: %v", err)
	}
	if got.IMAPUsername != "you@example.com" || got.IMAPPassword != "hunter2" {
		t.Errorf("stored IMAP credentials = %+v", got)
	}
	if got.SMTPUsername != "you@example.com" || got.SMTPPassword != "hunter2" {
		t.Errorf("SMTP credentials should default to IMAP login, got %+v", got)
	}
	if st.created.CredentialsDEK != CredentialsSchemeV1 {
		t.Error("credentials DEK should be the scheme sentinel")
	}
	if !st.created.SyncEnabled {
		t.Error("a validated account should be created sync-enabled")
	}
}

// mailroom#435: only a REJECTION latches sync off. Before this, every failure
// did — including a certificate that arrived a few hundred milliseconds after
// the dial — and the latch was permanent, because boot never re-validates an
// account it already has. So the table below is the policy: the two kinds that
// mean "retrying cannot help" disable sync; everything else, including an error
// the classifier could not place at all, leaves it enabled for the sync loop's
// backoff.
func TestAddAccountValidationFailureDisablesSyncOnlyWhenRejected(t *testing.T) {
	cases := []struct {
		name        string
		dialErr     error
		wantEnabled bool
	}{
		{"server rejected the credentials",
			&sync.Error{Kind: sync.ErrorKindAuth, Op: "dial", Err: errors.New("NO [AUTHENTICATIONFAILED] Authentication failed")}, false},
		{"dialer refused the configuration",
			&sync.Error{Kind: sync.ErrorKindConfig, Op: "dial", Err: errors.New("host is required")}, false},
		{"network failure",
			&sync.Error{Kind: sync.ErrorKindTransient, Op: "dial", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, true},
		{"protocol surprise",
			&sync.Error{Kind: sync.ErrorKindProtocol, Op: "dial", Err: errors.New("BAD unexpected")}, true},
		{"unclassified sync error (a TLS verification failure lands here)",
			&sync.Error{Kind: sync.ErrorKindUnknown, Op: "dial", Err: errors.New("tls: failed to verify certificate")}, true},
		{"not a sync error at all",
			errors.New("connection refused"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			dial := func(context.Context, sync.Config) (sync.SyncSession, error) { return nil, tc.dialErr }

			res, err := AddAccount(context.Background(), st, fakeKeys{testMaster()}, ulid.Make(), validSpec(), WithDialer(dial), WithResolver(addrs(t, "93.184.216.34")))
			if err == nil {
				t.Fatal("expected a validation error: the failure is reported even when sync stays enabled")
			}
			if res.AccountID == (ulid.ULID{}) {
				t.Fatal("account should be created before validation, so its id is known")
			}
			if res.Validated {
				t.Error("Validated must be false on failure")
			}
			if res.SyncEnabled != tc.wantEnabled {
				t.Errorf("Result.SyncEnabled = %v, want %v", res.SyncEnabled, tc.wantEnabled)
			}
			if got := st.disabled[res.AccountID.String()]; got != !tc.wantEnabled {
				t.Errorf("store has sync disabled = %v, want %v", got, !tc.wantEnabled)
			}
			// The error text is what boot and the CLI print, so it must say
			// which of the two states the account was left in.
			wantWord := "sync disabled"
			if tc.wantEnabled {
				wantWord = "sync enabled, will be retried"
			}
			if !strings.Contains(err.Error(), wantWord) {
				t.Errorf("error %q does not say %q", err, wantWord)
			}
		})
	}
}

func TestFailureKindNamesTheClassification(t *testing.T) {
	if got := FailureKind(&sync.Error{Kind: sync.ErrorKindAuth}); got != "auth" {
		t.Errorf("FailureKind(auth) = %q", got)
	}
	if got := FailureKind(errors.New("plain")); got != "unclassified" {
		t.Errorf("FailureKind(plain) = %q", got)
	}
}

func TestAddAccountRejectsBadSpec(t *testing.T) {
	st := &fakeStore{}
	_, err := AddAccount(context.Background(), st, fakeKeys{testMaster()}, ulid.Make(), AccountSpec{Name: "x"})
	if err == nil {
		t.Fatal("expected spec validation to reject an incomplete spec")
	}
	if st.created.Name != "" {
		t.Error("nothing should be stored when the spec is invalid")
	}
}
