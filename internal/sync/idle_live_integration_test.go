// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Live IMAP IDLE test (mailroom#103 Stage 2). It exercises the one property the
// IDLE watcher leans on that is a property of the SERVER and the client library
// rather than of our code: that an IMAP IDLE session on a selected mailbox
// receives an untagged EXISTS when ANOTHER connection appends mail to that
// mailbox. The watcher treats that EXISTS purely as a hint and reconciles via
// SyncAccount, so this test proves the hint actually arrives — not that any
// particular count is delivered (Notifications are explicitly lossy).
//
// Runs under scripts/sync-test.sh (the same Dovecot 2.3.21 fixture the rest of
// internal/sync's live tests use; it advertises IDLE by default). Without the
// MAILROOM_TEST_IMAP_* env vars it skips, like every test in this package.
package sync_test

import (
	"context"
	"testing"
	"time"

	mrsync "mailroom/internal/sync"
)

// TestLiveIdleExistsNotification opens IDLE on Junk from one session, appends a
// message to Junk from a SECOND session, and asserts an EXISTS notification
// arrives on the idling session within a generous window. It owns Junk for its
// duration and drains it at both ends, so it makes no assumption about what any
// other test left behind (matching this package's discipline).
func TestLiveIdleExistsNotification(t *testing.T) {
	cfg := testConfig(t)

	// The idling session and a separate appender session — never the same
	// connection: a session in IDLE can service no other command.
	idler := dial(t, cfg)
	appender := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if !idler.Supports(mrsync.CapabilityIdle) {
		t.Skip("server does not advertise IDLE; skipping the live IDLE test")
	}

	const box = mailboxJunk

	// Drain Junk to a clean baseline via the appender (the idler is about to be
	// tied up in IDLE), and again on cleanup so the mailbox is left empty. Each
	// call opens its OWN short context: the cleanup runs after the test's ctx has
	// been cancelled, so it must not depend on it.
	drain := func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dcancel()
		_, uids, err := appender.LiveUIDs(dctx, box)
		if err != nil {
			t.Fatalf("LiveUIDs(%q) error = %v", box, err)
		}
		for _, uid := range uids {
			if err := appender.StoreFlags(dctx, box, uid, []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
				t.Fatalf("StoreFlags() error = %v", err)
			}
			// The removal count is deliberately ignored here: this is cleanup,
			// draining whatever the previous run left behind, so "there was
			// nothing to remove" is a perfectly good outcome.
			if _, err := appender.Expunge(dctx, box, mrsync.UIDSetNum(uid)); err != nil {
				t.Fatalf("Expunge() error = %v", err)
			}
		}
	}
	drain()
	t.Cleanup(drain)

	// Select Junk on the idler so it receives that mailbox's unilateral updates.
	if _, err := idler.Select(ctx, box, mrsync.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("Select(%q) error = %v", box, err)
	}

	idle, err := idler.Idle(ctx)
	if err != nil {
		t.Fatalf("Idle() error = %v", err)
	}
	// Ensure IDLE is always ended, even if an assertion below fails.
	defer func() {
		_ = idle.Stop()
		_ = idle.Wait()
	}()

	// From the second connection, append a message to the mailbox the idler is
	// watching. This is the event a real new-mail delivery produces.
	appendTestMessage(t, ctx, appender, box, "idle-exists-notification", nil)

	// Wait for an EXISTS. Draining any non-EXISTS hints (flag/expunge) that may
	// arrive first. The window is deliberately generous: Dovecot's default
	// mailbox_idle_check_interval is 30s, so a worst-case notification can lag
	// that far even though a same-server cross-connection APPEND is usually
	// near-instant.
	notifs := idler.Notifications()
	deadline := time.After(45 * time.Second)
	for {
		select {
		case n := <-notifs:
			if n.Kind == mrsync.NotificationExists {
				return // success: the IDLE hint the watcher relies on arrived
			}
			// A non-EXISTS hint: keep waiting for the EXISTS.
		case <-deadline:
			t.Fatal("no EXISTS notification arrived on the IDLE session within the window")
		}
	}
}
