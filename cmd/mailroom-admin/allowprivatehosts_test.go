// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// #624 §2: onboarding.WithAllowPrivateHosts was passed by the server
// (cmd/mailroom) and the JMAP Account/set path, but never by mailroom-admin —
// so MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS had no effect on add-account or on
// enable-account's re-validation, and a LAN account the operator explicitly
// allowed in was refused by this CLI regardless. Both tests drive the option
// with no network: the egress guard runs before any dial (#527), so a private
// literal-IP host is refused deterministically and reaching the dialer at all
// is the signal that the option got through.

// fakeAccountStore satisfies onboarding.AccountStore without a database —
// these tests are about whether the egress option reaches onboarding, not
// about persistence.
type fakeAccountStore struct{}

func (fakeAccountStore) CreateAccount(context.Context, ulid.ULID, store.AccountRow) (ulid.ULID, error) {
	return ulid.Make(), nil
}

func (fakeAccountStore) SetAccountSyncEnabled(context.Context, ulid.ULID, ulid.ULID, bool) error {
	return nil
}

// fakeKeyProvider hands back a fixed in-memory master key — these tests seal
// their own fixtures and never read a file secret.
type fakeKeyProvider struct{ key []byte }

func (f fakeKeyProvider) MasterKey(context.Context) ([]byte, error) {
	out := make([]byte, len(f.key))
	copy(out, f.key)
	return out, nil
}

var allowPrivateHostsTestMaster = []byte("mailroom-admin-allow-private-key")

// recordingIMAPDialer reports whether the IMAP dial was ever attempted,
// without touching a real socket — the same shape as
// internal/onboarding/egressguard_test.go's recordingDialer (unexported there,
// so it cannot be reused from this package), because #527's load-bearing
// assertion is that a refused host is never dialled, not merely that an error
// comes back.
type recordingIMAPDialer struct{ called bool }

func (d *recordingIMAPDialer) dial(context.Context, sync.Config) (sync.SyncSession, error) {
	d.called = true
	return nil, errors.New("recordingIMAPDialer: reached (this is the observation, not a failure)")
}

// privateAccountSpec is an account whose IMAP and SMTP hosts are both on a
// private network, as RFC1918 literals — literal addresses skip the resolver
// entirely (egress.checkedAddrs), so these tests need no fake DNS and no
// network to be deterministic.
func privateAccountSpec() onboarding.AccountSpec {
	return onboarding.AccountSpec{
		Name: "lan", IMAPHost: "10.0.0.5", SMTPHost: "10.0.0.6",
		Username: "u", Password: "p", TLS: onboarding.TLSImplicit,
	}
}

// TestAddAccountHonorsAllowPrivateAccountHosts drives the EXACT seam
// runAddAccount calls (addAccountOnboardingOptions), not a hand-built option
// list — so a probe that deletes the WithAllowPrivateHosts line from that
// seam turns this test red at the same place it would break add-account
// itself, rather than the test quietly re-deriving the right answer beside a
// broken call site.
func TestAddAccountHonorsAllowPrivateAccountHosts(t *testing.T) {
	userID := ulid.Make()
	keys := fakeKeyProvider{key: allowPrivateHostsTestMaster}

	t.Run("refused by default", func(t *testing.T) {
		// log is left nil: onboarding.WithLogger treats a nil logger as "keep
		// the default" (onboarding.go), so this exercises exactly what
		// runAddAccount passes without needing a real *slog.Logger here.
		d := &deps{allowPrivateAccountHosts: false}
		rec := &recordingIMAPDialer{}
		opts := append(addAccountOnboardingOptions(d), onboarding.WithDialer(rec.dial))
		_, err := onboarding.AddAccount(context.Background(), fakeAccountStore{}, keys, userID, privateAccountSpec(), opts...)
		if rec.called {
			t.Error("add-account dialled a LAN account with allowPrivateAccountHosts=false")
		}
		if !errors.Is(err, onboarding.ErrHostNotPermitted) {
			t.Errorf("add-account with allowPrivateAccountHosts=false did not refuse a LAN account: %v", err)
		}
	})

	t.Run("permitted when configured", func(t *testing.T) {
		d := &deps{allowPrivateAccountHosts: true}
		rec := &recordingIMAPDialer{}
		opts := append(addAccountOnboardingOptions(d), onboarding.WithDialer(rec.dial))
		_, err := onboarding.AddAccount(context.Background(), fakeAccountStore{}, keys, userID, privateAccountSpec(), opts...)
		if !rec.called {
			t.Fatal("add-account with allowPrivateAccountHosts=true never reached the dialer for a LAN account — " +
				"the option did not reach onboarding")
		}
		if errors.Is(err, onboarding.ErrHostNotPermitted) {
			t.Errorf("still refused as policy despite allowPrivateAccountHosts=true: %v", err)
		}
	})
}

// TestEnableAccountRevalidationHonorsAllowPrivateAccountHosts pins the second
// call site: enable-account's re-validation (validateStoredAccount) must
// refuse a stored LAN account by default, exactly like add-account, and must
// permit one when the operator's config says to.
//
// ⚠ THE "permitted" CASE IS THE LOAD-BEARING HALF. validateStoredAccount hands
// onboarding.ValidateAccount a nil dial override (no WithDialer seam exists on
// this path), so it always uses the production sync.Dial once the policy gate
// passes — a REAL dial is unavoidable here, unlike the add-account test above.
// The context is bounded to a few milliseconds so that dial fails FAST (by
// cancellation, exactly as Dial's own doc promises — sync/session.go) rather
// than hanging on an unroutable RFC1918 address; the only thing asserted is
// that the error is NOT ErrHostNotPermitted, which is true precisely when the
// policy gate let the attempt through to a dial at all. Without this half,
// "refuses by default" is satisfied by a validateStoredAccount that never
// wires the option at all — false=false either way — and the test would prove
// nothing about the true branch this fix exists for (#624 §2).
func TestEnableAccountRevalidationHonorsAllowPrivateAccountHosts(t *testing.T) {
	userID := ulid.Make()
	ct, err := onboarding.SealCredentials(allowPrivateHostsTestMaster, userID, onboarding.Credentials{
		IMAPUsername: "u", IMAPPassword: "p", SMTPUsername: "u", SMTPPassword: "p",
	})
	if err != nil {
		t.Fatalf("seal fixture credentials: %v", err)
	}
	acct := store.Account{
		ID: userID, Name: "lan",
		IMAPHost: "10.0.0.5", IMAPPort: 993,
		SMTPHost: "10.0.0.6", SMTPPort: 465,
		CredentialsCT: ct,
	}
	keys := fakeKeyProvider{key: allowPrivateHostsTestMaster}

	t.Run("refused by default", func(t *testing.T) {
		_, err := validateStoredAccount(context.Background(), keys, userID, acct, false)
		if !errors.Is(err, onboarding.ErrHostNotPermitted) {
			t.Errorf("enable-account re-validation with allowPrivateAccountHosts=false did not refuse a stored LAN account: %v", err)
		}
	})

	t.Run("permitted when configured", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := validateStoredAccount(ctx, keys, userID, acct, true)
		if errors.Is(err, onboarding.ErrHostNotPermitted) {
			t.Errorf("enable-account re-validation still refused as POLICY with allowPrivateAccountHosts=true: %v — "+
				"the option did not reach onboarding.ValidateAccount", err)
		}
	})
}
