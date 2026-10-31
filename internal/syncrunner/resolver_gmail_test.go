// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mailsync"
	"mailroom/internal/oauthmail"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
)

func gmailOAuthAccount(t *testing.T, master []byte, userID ulid.ULID) store.Account {
	t.Helper()
	ct, err := onboarding.SealCredentials(master, userID, onboarding.Credentials{
		IMAPUsername: "you@example.com",
		OAuth:        &onboarding.OAuthSecret{Provider: "google", RefreshToken: "1//refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.Account{ID: ulid.Make(), Name: "gmail", IMAPHost: "imap.gmail.com", IMAPPort: 993,
		AuthKind: "oauth2", ProviderProfile: "gmail", AuthState: store.OAuthStateActive,
		CredentialsCT: ct, SyncEnabled: true}
}

func liftedHold(string, string) (string, string, bool) { return "", "", false }

// THE GMAIL SYNC HOLD AT THE LAST DOOR (#52, ADR-046 §10, ADR-047 §7). While
// oauthmail.GmailSyncReady is false — as it is in this release — a Gmail
// OAuth account is refused where every dial is prepared, even if its
// sync_enabled is somehow true. With the hold lifted (injected, as 5b and the
// a live test will do for real) it gets the Gmail sync model.
func TestResolverGmailHoldAndModel(t *testing.T) {
	if oauthmail.GmailSyncReady {
		t.Fatal("this release must ship with the Gmail sync hold in place (GmailSyncReady=false); " +
			"lifting it is increment 5b plus a live test")
	}
	master := testMasterKey()
	userID := ulid.Make()
	acct := gmailOAuthAccount(t, master, userID)
	tokens := &fakeTokens{tok: "ya29.access"}

	_, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens)).Prepare(context.Background(), userID, acct)
	if !errors.Is(err, ErrSyncHeld) {
		t.Fatalf("held Gmail account: Prepare = %v, want ErrSyncHeld", err)
	}

	req, err := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens), WithSyncHold(liftedHold)).
		Prepare(context.Background(), userID, acct)
	if err != nil {
		t.Fatalf("lifted hold: %v", err)
	}
	if req.Gmail == nil {
		t.Fatal("a Gmail OAuth account did not get the Gmail sync model")
	}
	if req.Gmail.DailyBudgetBytes != mailsync.DefaultGmailDailyBudget || req.Gmail.LabelSweepInterval != mailsync.DefaultGmailLabelSweepInterval {
		t.Errorf("defaults = %+v", *req.Gmail)
	}
	req, _ = NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens), WithSyncHold(liftedHold),
		WithGmailSync(123, 0)).Prepare(context.Background(), userID, acct)
	if req.Gmail.DailyBudgetBytes != 123 || req.Gmail.LabelSweepInterval != 0 {
		t.Errorf("configured = %+v", *req.Gmail)
	}
	// There is no unbudgeted Gmail: a non-positive budget keeps the default.
	req, _ = NewCredentialResolver(fakeKeys{master}, WithAccessTokens(tokens), WithSyncHold(liftedHold),
		WithGmailSync(0, time.Hour)).Prepare(context.Background(), userID, acct)
	if req.Gmail.DailyBudgetBytes != mailsync.DefaultGmailDailyBudget {
		t.Errorf("a zero budget disabled the budget: %+v", *req.Gmail)
	}

	// A Gmail account added with an app password stays on the generic path.
	pw := acct
	pw.AuthKind = "password"
	pw.CredentialsCT, _ = onboarding.SealCredentials(master, userID, onboarding.Credentials{IMAPUsername: "u", IMAPPassword: "p"})
	req, err = NewCredentialResolver(fakeKeys{master}).Prepare(context.Background(), userID, pw)
	if err != nil || req.Gmail != nil {
		t.Errorf("password Gmail: err=%v gmail=%v", err, req.Gmail)
	}
}

// GmailSyncReady=false → NOTHING SYNCS: a held Gmail account that reaches the
// runner (sync_enabled true, which every door refuses — this is the case
// where something got past them) is never handed to the engine.
func TestRunnerNeverSyncsAHeldGmailAccount(t *testing.T) {
	master := testMasterKey()
	userID := ulid.Make()
	acct := gmailOAuthAccount(t, master, userID)
	syncer := &fakeSyncer{}
	resolver := NewCredentialResolver(fakeKeys{master}, WithAccessTokens(&fakeTokens{tok: "x"}))
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, resolver.Prepare)
	sum, _ := r.SyncOnce(context.Background(), userID)
	if syncer.callCount() != 0 {
		t.Errorf("the engine was called %d time(s) for a held Gmail account", syncer.callCount())
	}
	if sum.Failed != 1 || len(sum.Errors) != 1 || !errors.Is(sum.Errors[0].Err, ErrSyncHeld) {
		t.Errorf("summary = %+v", sum)
	}

	// Control: the same account with the hold lifted IS synced, so the zero
	// above is the hold, not a runner that syncs nothing.
	resolver = NewCredentialResolver(fakeKeys{master}, WithAccessTokens(&fakeTokens{tok: "x"}), WithSyncHold(liftedHold))
	r = New(&fakeLister{accts: []store.Account{acct}}, syncer, resolver.Prepare)
	if _, err := r.SyncOnce(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if syncer.callCount() != 1 {
		t.Errorf("lifted hold: engine called %d time(s), want 1", syncer.callCount())
	}
}
