// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// ---- fakes -----------------------------------------------------------------

type fakeLister struct {
	accts       []store.Account
	err         error
	disabled    []store.Account // mailroom#435
	disabledErr error
}

func (f *fakeLister) ListSyncableAccounts(context.Context, ulid.ULID) ([]store.Account, error) {
	return f.accts, f.err
}

func (f *fakeLister) ListDisabledAccounts(context.Context, ulid.ULID) ([]store.Account, error) {
	return f.disabled, f.disabledErr
}

type fakeSyncer struct {
	mu      sync.Mutex
	calls   []ulid.ULID
	failFor map[string]error
	summary mailsync.Summary
}

func (f *fakeSyncer) SyncAccount(_ context.Context, req mailsync.AccountSync) (mailsync.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.AccountID)
	if err := f.failFor[req.AccountID.String()]; err != nil {
		return mailsync.Summary{}, err
	}
	return f.summary, nil
}

func (f *fakeSyncer) SyncAccountWith(ctx context.Context, req mailsync.AccountSync, _ mrsync.SyncSession) (mailsync.Summary, error) {
	return f.SyncAccount(ctx, req)
}

func (f *fakeSyncer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// idPrepare maps an account to a minimal AccountSync (no real credentials).
func idPrepare(_ context.Context, userID ulid.ULID, acct store.Account) (mailsync.AccountSync, error) {
	return mailsync.AccountSync{UserID: userID, AccountID: acct.ID, DEK: make([]byte, 32)}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func accounts(names ...string) []store.Account {
	out := make([]store.Account, len(names))
	for i, n := range names {
		out[i] = store.Account{ID: ulid.Make(), Name: n}
	}
	return out
}

type fakePushNotifier struct {
	mu    sync.Mutex
	calls []int // newMessages per call
	err   error
}

func (f *fakePushNotifier) NotifyNewMail(_ context.Context, _ ulid.ULID, _ store.Account, newMessages int, _ []ulid.ULID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, newMessages)
	return f.err
}

func (f *fakePushNotifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---- tests -----------------------------------------------------------------

// mailroom#435: a sync-disabled account must be NAMED on every pass, not folded
// into a cheerful accounts=0 at INFO. The negative control is the load-bearing
// half: a user with no disabled accounts must not get the warning, or every
// healthy instance would log a spurious WARN per pass and the signal would be
// worthless. Both the one-shot pass (SyncOnce) and the production scheduled
// pass are exercised, because the fix is only real on the path the loop runs.
func TestDisabledAccountsAreNamedEveryPass(t *testing.T) {
	type outcome struct {
		log  string
		sum  PassSummary
		sunk []int
	}
	run := func(lister *fakeLister, scheduled bool) outcome {
		var buf bytes.Buffer
		var sunk []int
		r := New(lister, &fakeSyncer{}, idPrepare,
			WithLogger(slog.New(slog.NewTextHandler(&buf, nil))),
			WithDisabledAccountsSink(func(_ ulid.ULID, n int) { sunk = append(sunk, n) }))
		var (
			sum PassSummary
			err error
		)
		if scheduled {
			sum, err = r.runScheduledPass(context.Background(), ulid.Make(), 0)
		} else {
			sum, err = r.SyncOnce(context.Background(), ulid.Make())
		}
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
		return outcome{buf.String(), sum, sunk}
	}

	for _, scheduled := range []bool{false, true} {
		name := "sync-once"
		if scheduled {
			name = "scheduled"
		}
		t.Run(name, func(t *testing.T) {
			// --- one enabled, one disabled: the disabled one is named ---
			dis := accounts("dormant")
			got := run(&fakeLister{accts: accounts("live"), disabled: dis}, scheduled)
			if got.sum.Accounts != 1 || got.sum.Disabled != 1 {
				t.Errorf("summary accounts=%d disabled=%d, want 1/1", got.sum.Accounts, got.sum.Disabled)
			}
			for _, want := range []string{
				"level=WARN",       // it is a warning, not INFO
				"sync disabled",    // the line exists at all
				"dormant",          // WHICH account
				dis[0].ID.String(), // by id too, which enable-account needs
				"disabled=1",       // the count is on the summary line as well
				"enable-account",   // and the way out is named
			} {
				if !strings.Contains(got.log, want) {
					t.Errorf("log is missing %q\n--- log ---\n%s", want, got.log)
				}
			}
			if len(got.sunk) != 1 || got.sunk[0] != 1 {
				t.Errorf("sink saw %v, want [1]", got.sunk)
			}

			// --- NEGATIVE CONTROL: nothing disabled, nothing warned ---
			got = run(&fakeLister{accts: accounts("live")}, scheduled)
			if got.sum.Disabled != 0 {
				t.Errorf("summary disabled=%d, want 0", got.sum.Disabled)
			}
			if strings.Contains(got.log, "level=WARN") {
				t.Errorf("a user with no disabled accounts must not be warned\n--- log ---\n%s", got.log)
			}
			if !strings.Contains(got.log, "disabled=0") {
				t.Errorf("the summary line must still carry the count\n--- log ---\n%s", got.log)
			}
			if len(got.sunk) != 1 || got.sunk[0] != 0 {
				t.Errorf("sink saw %v, want [0] so a stale gauge clears", got.sunk)
			}

			// --- a failed listing is an ERROR, not a zero ---
			got = run(&fakeLister{accts: accounts("live"), disabledErr: errors.New("db away")}, scheduled)
			if !strings.Contains(got.log, "level=ERROR") || !strings.Contains(got.log, "count is unknown") {
				t.Errorf("a failed disabled-listing must be logged as its own error\n--- log ---\n%s", got.log)
			}
			if len(got.sunk) != 0 {
				t.Errorf("sink saw %v, want nothing: an unknown count must not be reported as 0", got.sunk)
			}
		})
	}
}

func TestPushNotifierFiresOnNewMail(t *testing.T) {
	accts := accounts("a", "b")
	syncer := &fakeSyncer{summary: mailsync.Summary{MessagesIngested: 2}}
	push := &fakePushNotifier{}
	r := New(&fakeLister{accts: accts}, syncer, idPrepare,
		WithLogger(discardLogger()), WithPushNotifier(push))

	if _, err := r.SyncOnce(context.Background(), ulid.Make()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if push.callCount() != 2 {
		t.Errorf("push fired %d times, want 2 (one per account with new mail)", push.callCount())
	}
	for _, n := range push.calls {
		if n != 2 {
			t.Errorf("push newMessages = %d, want 2", n)
		}
	}
}

func TestPushNotifierSkippedWithoutNewMail(t *testing.T) {
	accts := accounts("a", "b")
	syncer := &fakeSyncer{summary: mailsync.Summary{MessagesIngested: 0, FlagUpdates: 5}}
	push := &fakePushNotifier{}
	r := New(&fakeLister{accts: accts}, syncer, idPrepare,
		WithLogger(discardLogger()), WithPushNotifier(push))

	if _, err := r.SyncOnce(context.Background(), ulid.Make()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if push.callCount() != 0 {
		t.Errorf("push fired %d times on a pass with no new mail, want 0", push.callCount())
	}
}

// A reply that arrived in a muted conversation was archived on arrival and must
// not notify (spec §6). A pass with one muted reply and one ordinary message
// pushes for ONE; a pass whose only new mail was a muted reply pushes NOTHING —
// not a push with a count of zero, no push at all.
func TestPushLeavesOutMutedArrivals(t *testing.T) {
	cases := []struct {
		name      string
		summary   mailsync.Summary
		wantCalls []int
	}{
		{"one muted reply and one ordinary message", mailsync.Summary{MessagesIngested: 2, MessagesMuted: 1}, []int{1}},
		{"only a muted reply", mailsync.Summary{MessagesIngested: 1, MessagesMuted: 1}, nil},
		// #59: a blocked sender's arrival is left out the same way.
		{"one blocked arrival and one ordinary message", mailsync.Summary{MessagesIngested: 2, MessagesBlocked: 1}, []int{1}},
		{"only a blocked arrival", mailsync.Summary{MessagesIngested: 1, MessagesBlocked: 1}, nil},
		{"one muted, one blocked, one ordinary", mailsync.Summary{MessagesIngested: 3, MessagesMuted: 1, MessagesBlocked: 1}, []int{1}},
		{"nothing muted (control)", mailsync.Summary{MessagesIngested: 2}, []int{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			push := &fakePushNotifier{}
			r := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{summary: tc.summary}, idPrepare,
				WithLogger(discardLogger()), WithPushNotifier(push))

			sum, err := r.SyncOnce(context.Background(), ulid.Make())
			if err != nil {
				t.Fatalf("SyncOnce: %v", err)
			}
			push.mu.Lock()
			got := append([]int(nil), push.calls...)
			push.mu.Unlock()
			if len(got) != len(tc.wantCalls) {
				t.Fatalf("push calls = %v, want %v", got, tc.wantCalls)
			}
			for i := range got {
				if got[i] != tc.wantCalls[i] {
					t.Errorf("push newMessages = %v, want %v", got, tc.wantCalls)
				}
			}
			// The muted arrivals are still INGESTED — they are new mail, filed —
			// and the pass summary says so; only the notification leaves them out.
			if sum.MessagesIngested != tc.summary.MessagesIngested || sum.MessagesMuted != tc.summary.MessagesMuted {
				t.Errorf("pass summary ingested=%d muted=%d, want %d/%d",
					sum.MessagesIngested, sum.MessagesMuted, tc.summary.MessagesIngested, tc.summary.MessagesMuted)
			}
		})
	}
}

func TestPushNotifierFailSoft(t *testing.T) {
	accts := accounts("a", "b")
	syncer := &fakeSyncer{summary: mailsync.Summary{MessagesIngested: 1}}
	push := &fakePushNotifier{err: errors.New("push service unreachable")}
	r := New(&fakeLister{accts: accts}, syncer, idPrepare,
		WithLogger(discardLogger()), WithPushNotifier(push))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("a push failure must not fail the pass: %v", err)
	}
	if sum.Synced != 2 || sum.Failed != 0 {
		t.Errorf("push failure leaked into the sync outcome: %+v", sum)
	}
}

func TestSyncOnceSyncsAllAccounts(t *testing.T) {
	accts := accounts("a", "b", "c")
	syncer := &fakeSyncer{summary: mailsync.Summary{MessagesIngested: 2, FlagUpdates: 1, MailboxesSynced: 1}}
	r := New(&fakeLister{accts: accts}, syncer, idPrepare, WithLogger(discardLogger()))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if sum.Accounts != 3 || sum.Synced != 3 || sum.Failed != 0 {
		t.Errorf("summary counts = %+v", sum)
	}
	if sum.MessagesIngested != 6 || sum.FlagUpdates != 3 || sum.MailboxesSynced != 3 {
		t.Errorf("aggregation wrong: %+v", sum)
	}
	if syncer.callCount() != 3 {
		t.Errorf("syncer called %d times, want 3", syncer.callCount())
	}
}

func TestSyncOnceErrorIsolation(t *testing.T) {
	accts := accounts("a", "bad", "c")
	bad := accts[1].ID
	syncer := &fakeSyncer{failFor: map[string]error{bad.String(): errors.New("boom")}}
	r := New(&fakeLister{accts: accts}, syncer, idPrepare, WithLogger(discardLogger()))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("a single failing account must not fail the pass: %v", err)
	}
	if sum.Synced != 2 || sum.Failed != 1 {
		t.Errorf("want 2 synced / 1 failed, got %+v", sum)
	}
	if len(sum.Errors) != 1 || sum.Errors[0].AccountID != bad {
		t.Errorf("expected the failure to be recorded for %s, got %+v", bad, sum.Errors)
	}
	if syncer.callCount() != 3 {
		t.Errorf("all accounts should be attempted despite one failure, got %d", syncer.callCount())
	}
}

func TestSyncOncePrepareErrorIsolation(t *testing.T) {
	accts := accounts("a", "bad", "c")
	bad := accts[1].ID
	prepare := func(_ context.Context, userID ulid.ULID, acct store.Account) (mailsync.AccountSync, error) {
		if acct.ID == bad {
			return mailsync.AccountSync{}, errors.New("cannot open credentials")
		}
		return mailsync.AccountSync{UserID: userID, AccountID: acct.ID}, nil
	}
	syncer := &fakeSyncer{}
	r := New(&fakeLister{accts: accts}, syncer, prepare, WithLogger(discardLogger()))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if sum.Synced != 2 || sum.Failed != 1 {
		t.Errorf("want 2 synced / 1 failed, got %+v", sum)
	}
	if syncer.callCount() != 2 {
		t.Errorf("a prepare failure must not reach the syncer: syncer calls = %d, want 2", syncer.callCount())
	}
}

func TestSyncOnceListerErrorFailsPass(t *testing.T) {
	r := New(&fakeLister{err: errors.New("database down")}, &fakeSyncer{}, idPrepare, WithLogger(discardLogger()))
	if _, err := r.SyncOnce(context.Background(), ulid.Make()); err == nil {
		t.Fatal("a list failure should fail the pass")
	}
}

func TestSyncOnceProgressCallback(t *testing.T) {
	accts := accounts("a", "b")
	var mu sync.Mutex
	var events []ProgressEvent
	progress := func(ev ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	r := New(&fakeLister{accts: accts}, &fakeSyncer{}, idPrepare,
		WithLogger(discardLogger()), WithProgress(progress))
	if _, err := r.SyncOnce(context.Background(), ulid.Make()); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 progress events, got %d", len(events))
	}
}

// signalSyncer reports every SyncAccount call on a channel, so a test can prove
// the catch-up pass ran without waiting for the ticker.
type signalSyncer struct{ ch chan ulid.ULID }

func (s *signalSyncer) SyncAccount(_ context.Context, req mailsync.AccountSync) (mailsync.Summary, error) {
	s.ch <- req.AccountID
	return mailsync.Summary{}, nil
}

func (s *signalSyncer) SyncAccountWith(ctx context.Context, req mailsync.AccountSync, _ mrsync.SyncSession) (mailsync.Summary, error) {
	return s.SyncAccount(ctx, req)
}

func TestRunCatchUpOnStartThenStop(t *testing.T) {
	acct := store.Account{ID: ulid.Make(), Name: "a"}
	synced := make(chan ulid.ULID, 4)
	syncer := &signalSyncer{ch: synced}
	// A long interval means only the immediate catch-up pass can fire within
	// the test window.
	r := New(&fakeLister{accts: []store.Account{acct}}, syncer, idPrepare,
		WithInterval(time.Hour), WithLogger(discardLogger()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, ulid.Make()) }()

	select {
	case got := <-synced:
		if got != acct.ID {
			t.Errorf("catch-up synced %s, want %s", got, acct.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("catch-up pass did not run immediately on start")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// barrierSyncer blocks each SyncAccount until `target` of them are in flight at
// once, which can only happen if they run in parallel. It records the peak
// concurrency the bounded pool actually reached and the total number of accounts
// it saw.
type barrierSyncer struct {
	target  int
	mu      sync.Mutex
	active  int
	peak    int
	total   int
	gate    chan struct{}
	summary mailsync.Summary
}

func newBarrierSyncer(target int, summary mailsync.Summary) *barrierSyncer {
	return &barrierSyncer{target: target, gate: make(chan struct{}), summary: summary}
}

func (b *barrierSyncer) SyncAccount(ctx context.Context, _ mailsync.AccountSync) (mailsync.Summary, error) {
	b.mu.Lock()
	b.active++
	b.total++
	if b.active > b.peak {
		b.peak = b.active
	}
	if b.active >= b.target {
		select {
		case <-b.gate: // already open
		default:
			close(b.gate)
		}
	}
	b.mu.Unlock()

	// Proceed only once `target` callers are concurrently here (proof of
	// parallelism), or the context is cancelled (so a bug can't hang the suite).
	select {
	case <-b.gate:
	case <-ctx.Done():
	}

	b.mu.Lock()
	b.active--
	b.mu.Unlock()
	return b.summary, nil
}

func (b *barrierSyncer) SyncAccountWith(ctx context.Context, req mailsync.AccountSync, _ mrsync.SyncSession) (mailsync.Summary, error) {
	return b.SyncAccount(ctx, req)
}

// A single user's accounts sync CONCURRENTLY, bounded to WithConcurrency, and the
// out-of-order summary aggregation stays correct. Run under -race, this also
// proves the summary writes are properly synchronized.
func TestRunPassRunsAccountsConcurrentlyBounded(t *testing.T) {
	accts := accounts("a", "b", "c", "d")
	syncer := newBarrierSyncer(2, mailsync.Summary{MessagesIngested: 1})
	r := New(&fakeLister{accts: accts}, syncer, idPrepare,
		WithLogger(discardLogger()), WithConcurrency(2))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sum, err := r.SyncOnce(ctx, ulid.Make())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if sum.Accounts != 4 || sum.Synced != 4 {
		t.Errorf("summary = %+v, want 4 accounts / 4 synced", sum)
	}
	if sum.MessagesIngested != 4 {
		t.Errorf("aggregation lost updates: MessagesIngested = %d, want 4", sum.MessagesIngested)
	}

	syncer.mu.Lock()
	peak, total := syncer.peak, syncer.total
	syncer.mu.Unlock()
	if total != 4 {
		t.Errorf("syncer saw %d accounts, want 4", total)
	}
	if peak < 2 {
		t.Errorf("accounts did not run concurrently: peak in-flight = %d, want >= 2", peak)
	}
	if peak > 2 {
		t.Errorf("concurrency bound exceeded: peak in-flight = %d, want <= 2 (WithConcurrency(2))", peak)
	}
}

func TestRunRejectsZeroUser(t *testing.T) {
	r := New(&fakeLister{}, &fakeSyncer{}, idPrepare, WithLogger(discardLogger()))
	if err := r.Run(context.Background(), ulid.ULID{}); err == nil {
		t.Fatal("Run should reject a zero user id")
	}
}

// TestMailboxErrorCausesAreLogged pins #277: a per-mailbox failure must leave the
// MAILBOX NAME and the CAUSE in the log, not just a count.
//
// The bug this guards was not a crash. `mailbox_errors: 2` appeared once on the
// real instance, on a line marked INFO, and there was no way to learn which two
// mailboxes failed or why — the only consumer of summary.Errors took len() of it
// and dropped the values. A count with no cause cannot distinguish a mailbox that
// was briefly busy from one that has failed every pass for a week.
//
// ⚠ The negative-control half is the load-bearing half. A logging test passes
// trivially if it asserts on strings that were going to appear anyway, so the
// clean-pass case below asserts the failure line is ABSENT and the level stays
// INFO. Without it, this test would still pass if the new code logged
// unconditionally.
func TestMailboxErrorCausesAreLogged(t *testing.T) {
	logs := func(sum mailsync.Summary) string {
		var buf bytes.Buffer
		r := New(&fakeLister{accts: accounts("migadu")}, &fakeSyncer{summary: sum}, idPrepare,
			WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
		if _, err := r.SyncOnce(context.Background(), ulid.Make()); err != nil {
			t.Fatalf("SyncOnce: %v", err)
		}
		return buf.String()
	}

	// --- a degraded pass: every cause must be recoverable from the log ---
	out := logs(mailsync.Summary{
		MailboxesSynced: 4,
		Errors: []mailsync.MailboxError{
			{Mailbox: "Archive/2019", Err: errors.New("SELECT failed: mailbox is not selectable")},
			{Mailbox: "", Err: errors.New("retirement sweep failed")}, // account-level
		},
	})
	for _, want := range []string{
		"mailbox sync failed",       // the line exists at all
		"Archive/2019",              // WHICH mailbox
		"mailbox is not selectable", // WHY
		"(account-level)",           // an empty Mailbox is named, not blank
		"retirement sweep failed",   // the account-level cause too
		"mailbox_errors=2",          // the count is kept, not replaced
	} {
		if !strings.Contains(out, want) {
			t.Errorf("degraded pass: log is missing %q\n--- log ---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("a pass with mailbox errors must not be filed under INFO\n--- log ---\n%s", out)
	}

	// --- NEGATIVE CONTROL: a clean pass must say none of it ---
	clean := logs(mailsync.Summary{MailboxesSynced: 4})
	if strings.Contains(clean, "mailbox sync failed") {
		t.Errorf("clean pass logged a mailbox failure\n--- log ---\n%s", clean)
	}
	if strings.Contains(clean, "level=WARN") {
		t.Errorf("clean pass was logged at WARN\n--- log ---\n%s", clean)
	}
	if !strings.Contains(clean, "mailbox_errors=0") {
		t.Errorf("clean pass lost the mailbox_errors tally\n--- log ---\n%s", clean)
	}
}

// ---- deferred mailboxes are not failures (#638) -----------------------------

// A pass whose mailboxes were DEFERRED because the provider refused a UID SEARCH
// is a paced pass, not a failed one. The count must reach the operator (both
// aggregate lines plus one line naming the server's wait), and it must NOT be
// mistaken for a failure anywhere: the account still counts as synced, nothing
// lands in Errors, and the per-account line stays at INFO.
func TestPassSummaryCarriesDeferredMailboxes(t *testing.T) {
	var buf bytes.Buffer
	summary := mailsync.Summary{
		MailboxesSynced:     12,
		MailboxesDeferred:   7,
		RateLimitRetryAfter: 42 * time.Second,
	}
	r := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{summary: summary}, idPrepare,
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	if sum.MailboxesDeferred != 7 {
		t.Errorf("PassSummary.MailboxesDeferred = %d, want 7", sum.MailboxesDeferred)
	}
	// NOT a failure: this is the reporting change #638 is about.
	if sum.Synced != 1 || sum.Failed != 0 || len(sum.Errors) != 0 {
		t.Errorf("a throttled pass was scored as a failure: synced=%d failed=%d errors=%v",
			sum.Synced, sum.Failed, sum.Errors)
	}

	log := buf.String()
	for _, want := range []string{
		`mailboxes_deferred=7`,                // on the per-account and pass lines
		`mailboxes deferred to the next pass`, // the one line per pass
		`server_retry_after=42s`,              // ...naming the server's own wait
	} {
		if !strings.Contains(log, want) {
			t.Errorf("sync log does not contain %q; log:\n%s", want, log)
		}
	}
	// One line, not one per mailbox — the whole point.
	if got := strings.Count(log, "mailboxes deferred to the next pass"); got != 1 {
		t.Errorf("the deferral was logged %d times, want exactly 1", got)
	}

	// CONTROL: no deferral, no line.
	buf.Reset()
	r2 := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{summary: mailsync.Summary{MailboxesSynced: 12}},
		idPrepare, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if _, err := r2.SyncOnce(context.Background(), ulid.Make()); err != nil {
		t.Fatalf("SyncOnce (control): %v", err)
	}
	if strings.Contains(buf.String(), "mailboxes deferred to the next pass") {
		t.Errorf("a pass with nothing deferred logged the deferral line; log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "mailboxes_deferred=0") {
		t.Errorf("the field is missing from a clean pass's lines; log:\n%s", buf.String())
	}
}

// The account's SEARCH rate has to be readable from the log alone. Production's
// first measurement read it off mailboxes_reaped and undercounted by half, and
// the true figure needed a query against app.mailboxes. So the observed count
// must reach the pass summary AND both lines — the per-account one for "this
// account", the pass one for a grep over a day.
func TestPassSummaryCarriesObservedMailboxes(t *testing.T) {
	var buf bytes.Buffer
	summary := mailsync.Summary{MailboxesSynced: 300, MailboxesObserved: 12, MailboxesReaped: 5}
	r := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{summary: summary}, idPrepare,
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if sum.MailboxesObserved != 12 {
		t.Errorf("PassSummary.MailboxesObserved = %d, want 12", sum.MailboxesObserved)
	}
	if got := strings.Count(buf.String(), "mailboxes_observed=12"); got != 2 {
		t.Errorf("mailboxes_observed=12 appears %d times, want 2 (per-account and pass lines); log:\n%s",
			got, buf.String())
	}
}

// #620's figures have to be readable from the log alone, exactly like the
// observed count above: the skip counts on BOTH lines (per-account for "this
// account", pass-complete for a grep over a day) and the engine's wall time on
// the per-account line, since the whole point is to bring that number down.
func TestPassSummaryCarriesUnchangedSkips(t *testing.T) {
	var buf bytes.Buffer
	summary := mailsync.Summary{
		MailboxesSynced: 300, MailboxesUnchanged: 250, ObservationsSkipped: 9,
		MailboxesObserved: 12, Duration: 4321 * time.Millisecond,
	}
	r := New(&fakeLister{accts: accounts("a")}, &fakeSyncer{summary: summary}, idPrepare,
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

	sum, err := r.SyncOnce(context.Background(), ulid.Make())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if sum.MailboxesUnchanged != 250 || sum.ObservationsSkipped != 9 {
		t.Errorf("PassSummary unchanged/skipped = %d/%d, want 250/9", sum.MailboxesUnchanged, sum.ObservationsSkipped)
	}
	for _, field := range []string{"mailboxes_unchanged=250", "observations_skipped=9"} {
		if got := strings.Count(buf.String(), field); got != 2 {
			t.Errorf("%s appears %d times, want 2 (per-account and pass lines); log:\n%s", field, got, buf.String())
		}
	}
	if got := strings.Count(buf.String(), "duration_ms=4321"); got != 1 {
		t.Errorf("duration_ms=4321 appears %d times, want 1 (the per-account line); log:\n%s", got, buf.String())
	}
	// A skipped observation is not an answered SEARCH: the observed count is
	// passed through untouched, not inflated by the skips.
	if !strings.Contains(buf.String(), "mailboxes_observed=12") {
		t.Errorf("mailboxes_observed changed meaning; log:\n%s", buf.String())
	}
}
