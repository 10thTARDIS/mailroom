// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package syncrunner

// #52, ADR-046 §5: an OAuth account whose grant needs its user to reconnect is
// never dialled — not by a scheduled pass, not by a one-shot pass, not by a
// writeback nudge, and not by the IDLE watcher (which follows the same list) —
// and resumes on its own once the grant is active again. Through the REAL
// StoreLister, because the rule lives in its SQL.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mailsync"
	"mailroom/internal/store"
	mrsync "mailroom/internal/sync"
)

type countingPrepare struct {
	mu    sync.Mutex
	calls map[ulid.ULID]int
}

func (c *countingPrepare) prepare(_ context.Context, userID ulid.ULID, acct store.Account) (mailsync.AccountSync, error) {
	c.mu.Lock()
	c.calls[acct.ID]++
	c.mu.Unlock()
	return mailsync.AccountSync{UserID: userID, AccountID: acct.ID, DEK: make([]byte, 32)}, nil
}

func (c *countingPrepare) n(id ulid.ULID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

type syncedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncedLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncedLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestAwaitingReconnectIsNeverDialledAndResumes(t *testing.T) {
	f := newPushFixture(t) // a user with one password account ("Work")
	ctx := context.Background()
	oauth := ulid.Make()
	f.exec(t, `INSERT INTO app.accounts (id, user_id, name, provider_profile, imap_host, smtp_host, smtp_port,
	                                     auth_kind, credentials_ct, credentials_dek)
		VALUES ($1, $2, 'Outlook', 'microsoft', 'outlook.office365.com', 'smtp.office365.com', 587, 'oauth2', '\x00', $3)`,
		oauth.String(), f.user.String(), ulid.Make().String())
	f.exec(t, `INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, state, state_reason)
		VALUES ($1, $2, 'microsoft', 'oid-1', 'you@example.com', 'needs_reauth', 'invalid_grant')`,
		oauth.String(), f.user.String())

	prep := &countingPrepare{calls: map[ulid.ULID]int{}}
	syncer := &fakeSyncer{}
	var wbMu sync.Mutex
	wb := map[ulid.ULID]int{}
	wbDone := make(chan ulid.ULID, 8)
	logs := &syncedLog{}
	r := New(NewStoreLister(f.st), syncer, prep.prepare,
		WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
		WithWriteback(func(_ context.Context, _ ulid.ULID, acct store.Account, _ mrsync.Config) error {
			wbMu.Lock()
			wb[acct.ID]++
			wbMu.Unlock()
			wbDone <- acct.ID
			return nil
		}))

	syncsFor := func(id ulid.ULID) int {
		syncer.mu.Lock()
		defer syncer.mu.Unlock()
		n := 0
		for _, c := range syncer.calls {
			if c == id {
				n++
			}
		}
		return n
	}

	// The one-shot pass and the scheduled pass.
	if _, err := r.SyncOnce(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	if _, err := r.runScheduledPass(ctx, f.user, 1); err != nil {
		t.Fatal(err)
	}
	if prep.n(oauth) != 0 || syncsFor(oauth) != 0 {
		t.Fatalf("an account waiting for reconnect was prepared %d / synced %d times; want 0 and 0",
			prep.n(oauth), syncsFor(oauth))
	}
	// The control: the password account beside it synced on both passes, so
	// "zero" above is the rule working, not a runner that synced nothing.
	if syncsFor(f.account.ID) != 2 {
		t.Fatalf("the password account synced %d times, want 2", syncsFor(f.account.ID))
	}
	if !strings.Contains(logs.String(), "waiting for their user to reconnect") || !strings.Contains(logs.String(), "Outlook") {
		t.Errorf("the scheduled pass did not name the account it is not dialling:\n%s", logs.String())
	}

	// A writeback nudge drains the password account and never the other.
	before := len(wbDone)
	for range before {
		<-wbDone
	}
	r.NudgeWriteback(ctx, f.user)
	select {
	case id := <-wbDone:
		if id != f.account.ID {
			t.Fatalf("the nudge drained %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the nudge drained nothing at all")
	}
	time.Sleep(100 * time.Millisecond)
	wbMu.Lock()
	if wb[oauth] != 0 {
		t.Errorf("writeback ran %d times for an account waiting for reconnect", wb[oauth])
	}
	wbMu.Unlock()

	// The IDLE watcher's input is the same list.
	listed, err := NewStoreLister(f.st).ListSyncableAccounts(ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range listed {
		if a.ID == oauth {
			t.Fatal("the account waiting for reconnect is in the list IDLE watchers follow")
		}
	}

	// Reconnected: back to active, and the very next pass dials it with
	// nothing else switched on.
	f.exec(t, `UPDATE app.oauth_grants SET state = 'active', state_reason = '' WHERE account_id = $1`, oauth.String())
	if _, err := r.SyncOnce(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	if prep.n(oauth) != 1 || syncsFor(oauth) != 1 {
		t.Fatalf("after reconnect: prepared %d / synced %d, want 1 and 1", prep.n(oauth), syncsFor(oauth))
	}
	// operator_error keeps being dialled (the normal backoff applies).
	f.exec(t, `UPDATE app.oauth_grants SET state = 'operator_error', state_reason = 'invalid_client' WHERE account_id = $1`, oauth.String())
	if _, err := r.SyncOnce(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	if prep.n(oauth) != 2 {
		t.Fatalf("an operator_error account was not dialled: prepared %d", prep.n(oauth))
	}
	// Disconnected (no grant) with sync switched back on: not dialled either.
	f.exec(t, `DELETE FROM app.oauth_grants WHERE account_id = $1`, oauth.String())
	if _, err := r.SyncOnce(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	if prep.n(oauth) != 2 {
		t.Fatalf("a disconnected account was dialled: prepared %d", prep.n(oauth))
	}
}

func TestPushReconnectNeeded(t *testing.T) {
	f := newPushFixture(t)
	phone := f.device(t, "phone", "all")
	vipOnly := f.device(t, "laptop", "vip")
	rec := &recordingSender{sent: map[string]string{}}
	at := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	n := NewStorePushNotifier(f.st, rec, discardLogger()).WithClock(func() time.Time { return at })
	quiet, err := n.PushReconnectNeeded(context.Background(), f.user, f.account.ID, "Work")
	if err != nil || quiet {
		t.Fatalf("quiet=%v err=%v", quiet, err)
	}
	// Every device, whatever its mail mode: this is not about new mail.
	if len(rec.sent) != 2 || !strings.Contains(rec.sent[phone], "Work") || rec.sent[vipOnly] == "" {
		t.Fatalf("sent = %v", rec.sent)
	}
	// Quiet hours: nothing sent, and the caller is told so (it defers).
	f.quiet(t, store.QuietHours{Enabled: true, Start: 14 * 60, End: 18 * 60, Days: 127, Timezone: "UTC"})
	rec2 := &recordingSender{sent: map[string]string{}}
	n2 := NewStorePushNotifier(f.st, rec2, discardLogger()).WithClock(func() time.Time { return at })
	quiet, err = n2.PushReconnectNeeded(context.Background(), f.user, f.account.ID, "Work")
	if err != nil || !quiet || len(rec2.sent) != 0 {
		t.Fatalf("during quiet hours: quiet=%v err=%v sent=%v", quiet, err, rec2.sent)
	}
}
