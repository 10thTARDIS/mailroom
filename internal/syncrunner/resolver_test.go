// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/onboarding"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

type fakeKeys struct{ master []byte }

func (f fakeKeys) MasterKey(context.Context) ([]byte, error) {
	return append([]byte(nil), f.master...), nil
}

func testMasterKey() []byte {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(i + 7)
	}
	return m
}

// TestResolverPrepare exercises the full runner credential path without a
// database or IMAP: seal (as onboarding does) → derive → open → build the
// AccountSync the sync engine consumes.
func TestResolverPrepare(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()

	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		IMAPPassword: "hunter2",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	acct := store.Account{
		ID:            ulid.Make(),
		IMAPHost:      "imap.example.com",
		IMAPPort:      993,
		CredentialsCT: ct,
	}

	r := NewCredentialResolver(fakeKeys{master})
	req, err := r.Prepare(context.Background(), userID, acct)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if req.UserID != userID || req.AccountID != acct.ID {
		t.Error("prepare did not carry the identities through")
	}
	if req.Config.Host != "imap.example.com" || req.Config.Port != 993 {
		t.Errorf("config endpoint = %s:%d", req.Config.Host, req.Config.Port)
	}
	if req.Config.Username != "you@example.com" || req.Config.Password != "hunter2" {
		t.Errorf("config credentials = %q/%q", req.Config.Username, req.Config.Password)
	}
	if req.Config.TLS != sync.TLSImplicit {
		t.Errorf("config TLS = %v, want implicit", req.Config.TLS)
	}
	if len(req.DEK) != 32 {
		t.Errorf("mail DEK length = %d, want 32", len(req.DEK))
	}
	if req.DEKID != MailBlobSchemeV1 {
		t.Error("mail DEK id should be the scheme sentinel")
	}
	// The resolved config must satisfy the sync layer's own validation.
	if err := req.Config.Validate(); err != nil {
		t.Errorf("resolved config invalid: %v", err)
	}
}

func TestResolverWrongUserFailsToOpen(t *testing.T) {
	master := testMasterKey()
	ct, err := onboarding.SealCredentials(master, ulid.Make(), onboarding.Credentials{IMAPPassword: "x"})
	if err != nil {
		t.Fatal(err)
	}
	r := NewCredentialResolver(fakeKeys{master})
	// A different user id cannot open the sealed credentials.
	if _, err := r.Prepare(context.Background(), ulid.Make(), store.Account{
		ID: ulid.Make(), IMAPHost: "imap.example.com", IMAPPort: 993, CredentialsCT: ct,
	}); err == nil {
		t.Fatal("expected prepare to fail opening credentials for the wrong user")
	}
}

// ---- the per-account SEARCH budget (#638) -----------------------------------

func searchRateAccount(t *testing.T, master []byte, userID ulid.ULID) store.Account {
	t.Helper()
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		IMAPPassword: "hunter2",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return store.Account{ID: ulid.Make(), IMAPHost: "imap.example.com", IMAPPort: 993, CredentialsCT: ct}
}

// ONE BUDGET PER ACCOUNT, NOT PER Prepare. The server's SEARCH ceiling is
// charged per account, and Prepare is called once per sync pass AND again for
// every writeback nudge — both of which dial with the Config it returns. A
// limiter built fresh each time would let the observation pass and the writeback
// drain each burst to the full rate, and the account would be refused exactly as
// it was before the pacing existed (#638).
func TestResolverSharesOneSearchLimiterPerAccount(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	acctA := searchRateAccount(t, master, userID)
	acctB := searchRateAccount(t, master, userID)

	r := NewCredentialResolver(fakeKeys{master}, WithSearchRate(30))

	first, err := r.Prepare(context.Background(), userID, acctA)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	second, err := r.Prepare(context.Background(), userID, acctA)
	if err != nil {
		t.Fatalf("prepare (again): %v", err)
	}
	other, err := r.Prepare(context.Background(), userID, acctB)
	if err != nil {
		t.Fatalf("prepare (other account): %v", err)
	}

	if first.Config.SearchLimiter == nil {
		t.Fatal("SearchLimiter is nil with a configured rate: nothing would be paced")
	}
	if first.Config.SearchLimiter != second.Config.SearchLimiter {
		t.Error("two Prepare calls for one account handed back DIFFERENT limiters; " +
			"the sync pass and the writeback drain would each get the full rate")
	}
	if first.Config.SearchLimiter == other.Config.SearchLimiter {
		t.Error("two different accounts share one limiter; one busy account would " +
			"throttle an unrelated one")
	}
	if err := first.Config.Validate(); err != nil {
		t.Errorf("a config carrying a limiter must still validate: %v", err)
	}
}

// The off switch, and the default. Without WithSearchRate — and with an explicit
// zero — the Config must carry a NIL limiter, which internal/sync documents as
// "no pacing": every existing deployment and test behaves as it did.
func TestResolverSearchLimiterDisabled(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	acct := searchRateAccount(t, master, userID)

	for _, tc := range []struct {
		name string
		opts []ResolverOption
	}{
		{"unset", nil},
		{"explicit zero", []ResolverOption{WithSearchRate(0)}},
		{"negative", []ResolverOption{WithSearchRate(-5)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewCredentialResolver(fakeKeys{master}, tc.opts...)
			req, err := r.Prepare(context.Background(), userID, acct)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if req.Config.SearchLimiter != nil {
				t.Errorf("SearchLimiter = %#v, want nil (no pacing)", req.Config.SearchLimiter)
			}
		})
	}
}

// ---- the account-host egress policy (#625 §3) --------------------------------

// Every Config the resolver builds carries the allow-private switch, because
// every IMAP dial of a stored account (sync pass, pool, IDLE, writeback drain)
// dials with it. Default: guarded.
func TestResolverCarriesAllowPrivateHosts(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	acct := searchRateAccount(t, master, userID)
	for _, tc := range []struct {
		name string
		opts []ResolverOption
		want bool
	}{
		{"default is guarded", nil, false},
		{"explicitly off", []ResolverOption{WithAllowPrivateHosts(false)}, false},
		{"on", []ResolverOption{WithAllowPrivateHosts(true)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := NewCredentialResolver(fakeKeys{master}, tc.opts...).Prepare(context.Background(), userID, acct)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if req.Config.AllowPrivateHosts != tc.want {
				t.Errorf("Config.AllowPrivateHosts = %v, want %v", req.Config.AllowPrivateHosts, tc.want)
			}
		})
	}
}
