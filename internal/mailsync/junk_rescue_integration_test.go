// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// #576 end to end, against a real Dovecot: "Not junk" on a provider that tags
// the verdict with a $Junk KEYWORD must still be "not junk" after the next sync.
//
// The sequence is the issue's: a message sits in Junk carrying $Junk (what a
// Thunderbird-style filter leaves behind); the user rescues it; writeback
// drains; the account syncs. Until #576 the MOVE carried $Junk into INBOX and
// the sync re-derived is_junk = true — and, independently, the source copy's
// provenance row still remembered $Junk after the move, so the flag merge put
// it back even when the server copy was clean. Both halves are exercised here,
// because the assertion is on the state AFTER THE SYNC, not after Email/set.
//
// It uses the real store (RLS on), the real ingest path, the real writeback
// engine over its own IMAP session (as writeback.Runner dials one), and the real
// mailsync engine. Run via scripts/mailsync-test.sh, which creates the Junk
// folder with its \Junk SPECIAL-USE.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/writeback"
)

const liveJunk = "Junk"

// isJunk reads the canonical verdict column — what areas, the desk and search
// all filter on.
func (f *liveFixture) isJunk(t *testing.T, msg ulid.ULID) bool {
	t.Helper()
	var j bool
	if err := f.super.QueryRow(context.Background(),
		`SELECT is_junk FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&j); err != nil {
		t.Fatalf("is_junk: %v", err)
	}
	return j
}

// emailChangeRows counts the Email change_log rows for one message.
func (f *liveFixture) emailChangeRows(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("change rows: %v", err)
	}
	return n
}

// serverFlags reports every copy of the message by Message-ID subject in a
// mailbox and its flags, asked on a FRESHLY DIALED session (a long-lived one may
// not yet have been told about another session's EXPUNGE — see
// draft_expunge_integration_test.go's onServer).
func (f *liveFixture) serverFlags(t *testing.T, mailbox string) [][]sync.Flag {
	t.Helper()
	ctx := context.Background()
	s, err := sync.Dial(ctx, f.cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()
	_, uids, err := s.LiveUIDs(ctx, mailbox)
	if err != nil {
		t.Fatalf("live uids %s: %v", mailbox, err)
	}
	var out [][]sync.Flag
	if len(uids) == 0 {
		return out
	}
	if err := s.FetchByUID(ctx, sync.UIDSetNum(uids...), sync.FetchItems{Flags: true}, func(m sync.Message) error {
		out = append(out, m.Flags)
		return nil
	}); err != nil {
		t.Fatalf("fetch flags %s: %v", mailbox, err)
	}
	return out
}

func hasFlag(fl []sync.Flag, want string) bool {
	for _, f := range fl {
		if strings.EqualFold(string(f), want) {
			return true
		}
	}
	return false
}

// THE ISSUE'S VERIFICATION PLAN (#576), in the shipping configuration AND with
// the reaper due on every pass. The shipping one is the harder case: on the
// default hourly cadence the Junk folder's copy is NOT reaped by the sync that
// ingests the rescued INBOX copy, so its provenance row is still live — and
// still remembering whatever flags it had — when the flag merge runs.
//
// NEGATIVE CONTROLS (both must go red, each alone):
//   - drop the enqueueJunkVerdictIntents call in store.ApplyEmailUpdate: the
//     MOVE carries $Junk into INBOX and the post-sync is_junk reads true;
//   - drop the RecordStoredFlags call in writeback.applyToEveryCopy: the server
//     copy is clean, but the unreaped Junk row still says $Junk and the merge
//     re-junks the message on the shipping cadence.
func TestLiveNotJunkSurvivesTheNextSync(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"shipping reap cadence", nil},
		{"reap every pass", []Option{WithReapInterval(time.Nanosecond)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newLiveFixture(t, tc.opts...)
			_, uids, err := f.sess.LiveUIDs(ctx, liveJunk)
			if err != nil {
				t.Fatalf("Junk: %v (is it missing? scripts/mailsync-test.sh creates it)", err)
			}
			for _, uid := range uids {
				f.expungeOnServer(t, liveJunk, uid)
			}

			// A message the provider's filter put in Junk AND tagged $Junk.
			subject := "rescue-me-576-" + strings.ReplaceAll(tc.name, " ", "-")
			body := []byte("From: sender@example.com\r\n" +
				"To: " + f.userID.String() + "@example.com\r\n" +
				"Subject: " + subject + "\r\n" +
				"Message-ID: <" + subject + "@example.com>\r\n" +
				"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n" +
				"\r\n" +
				"a real message the filter got wrong\r\n")
			if _, err := f.sess.Append(ctx, liveJunk, []sync.Flag{"$Junk"}, time.Time{}, body); err != nil {
				t.Fatalf("append to Junk: %v", err)
			}
			f.syncOnce(t)
			msg, ok := f.messageIDBySubject(t, subject)
			if !ok {
				t.Fatal("the Junk message was never ingested")
			}
			junkID, inboxID := f.mailboxID(t, liveJunk), f.mailboxID(t, f.inbox)

			// PREMISES, each of which would make the run vacuous if false.
			var role string
			var accepts bool
			if err := f.super.QueryRow(ctx,
				`SELECT COALESCE(special_use, ''), accepts_keywords FROM app.mailboxes WHERE id = $1`,
				junkID.String()).Scan(&role, &accepts); err != nil {
				t.Fatal(err)
			}
			if role != `\Junk` {
				t.Fatalf("premise: Junk special_use=%q, want \\Junk", role)
			}
			// accepts_keywords is NOT a premise, and is reported rather than
			// required: the sync pass opens folders with EXAMINE, and a server
			// answers EXAMINE with an empty PERMANENTFLAGS (RFC 9051 §6.3.3), so
			// the recorded probe reads false even on Dovecot. The $NotJunk add is
			// gated on it; the $Junk REMOVAL — the half #576 turns on — is not.
			t.Logf("Junk accepts_keywords=%v (so +$NotJunk expected=%v)", accepts, accepts)
			if !f.isJunk(t, msg) {
				t.Fatal("premise: a $Junk-tagged message must ingest as is_junk = true")
			}
			if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != junkID.String() {
				t.Fatalf("premise: mailboxIds = %v, want {Junk}", got)
			}

			// THE RESCUE, exactly as the API builds it for keywords/$junk: false
			// with no destination named (internal/api/email.go parseEmailPatch).
			notJunk := false
			before := f.emailChangeRows(t, msg)
			if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
				found, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{Junk: &notJunk, MoveToRole: "inbox"})
				if err == nil && !found {
					t.Fatal("message not found for Email/set")
				}
				return err
			}); err != nil {
				t.Fatalf("Email/set not-junk: %v", err)
			}
			if got := f.emailChangeRows(t, msg) - before; got != 1 {
				t.Errorf("the rescue appended %d Email change rows, want exactly 1", got)
			}

			// Writeback drains over its own session.
			ws, err := sync.Dial(ctx, f.cfg)
			if err != nil {
				t.Fatalf("dial writeback session: %v", err)
			}
			defer func() { _ = ws.Close() }()
			eng := writeback.NewEngine(writeback.NewStoreIntents(f.st, f.userID))
			res, err := eng.ApplyPending(ctx, f.accountID, sync.NewWriter(ws))
			if err != nil {
				t.Fatalf("writeback: %v", err)
			}
			wantApplied := 2 // -$Junk, move
			if accepts {
				wantApplied = 3 // and +$NotJunk
			}
			// Errorf, not Fatalf: with the keyword intents gone this count is the
			// first thing to go red, and the run must still reach the post-sync
			// assertion below, which is the one the issue names.
			if res.Applied != wantApplied || res.DeadLettered != 0 {
				t.Errorf("writeback = %+v, want %d applied and none dead", res, wantApplied)
			}

			// The SERVER: the copy left Junk, and arrived without $Junk and with
			// $NotJunk. A bounded read — every copy in both folders.
			if left := f.serverFlags(t, liveJunk); len(left) != 0 {
				t.Errorf("Junk still holds %d copies after the move", len(left))
			}
			inInbox := f.serverFlags(t, f.inbox)
			if len(inInbox) != 1 {
				t.Fatalf("INBOX holds %d copies, want exactly 1", len(inInbox))
			}
			if hasFlag(inInbox[0], "$Junk") || hasFlag(inInbox[0], "$NotJunk") != accepts {
				t.Errorf("INBOX copy flags = %v, want no $Junk, and $NotJunk iff accepts_keywords (%v)", inInbox[0], accepts)
			}

			// THE ASSERTION THAT MATTERS: after the sync, not after the Email/set.
			f.syncOnce(t)
			if f.isJunk(t, msg) {
				t.Fatal("after the sync is_junk is TRUE again: the rescue lasted one sync cycle (#576)")
			}
			if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != inboxID.String() {
				t.Errorf("after the sync mailboxIds = %v, want {INBOX}", got)
			}
			if in := f.inMailbox(t, inboxID); len(in) != 1 || in[0] != msg.String() {
				t.Errorf("after the sync the inbox lists %v, want just the rescued message", in)
			}

			// And it STAYS rescued: a further pass changes nothing a client can
			// see, so it appends no change row.
			settled := f.emailChangeRows(t, msg)
			f.syncOnce(t)
			if f.isJunk(t, msg) {
				t.Fatal("a second sync re-junked the message")
			}
			if got := f.emailChangeRows(t, msg) - settled; got != 0 {
				t.Errorf("an idle sync appended %d Email change rows, want 0", got)
			}
		})
	}
}
