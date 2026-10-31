// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ml

// priority-report and the reply backfill (#842): a reply sent from another
// client is counted as an engaged message the feedback never recorded, the
// backfill records it exactly once, and the report then shows it recorded.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedIdentity gives the user an identity on a new account and returns the
// account id and that account's \Sent and INBOX mailbox ids.
func (f *trainFixture) seedIdentity(t *testing.T, address string) (acct, sent, inbox string) {
	t.Helper()
	ctx := context.Background()
	acct = ulid.Make().String()
	if _, err := f.super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host,
	        credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', $3, $4)`,
		acct, f.userID.String(), []byte{0x01}, ulid.Make().String()); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := f.super.Exec(ctx, `INSERT INTO app.identities (id, user_id, account_id, kind, address)
	      VALUES ($1, $2, $3, 'fixed', $4)`, ulid.Make().String(), f.userID.String(), acct, address); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	sent, inbox = ulid.Make().String(), ulid.Make().String()
	if _, err := f.super.Exec(ctx, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1, $2, $3, 'Sent', 'Sent', '\Sent'), ($4, $2, $3, 'INBOX', 'Inbox', NULL)`,
		sent, f.userID.String(), acct, inbox); err != nil {
		t.Fatalf("seed mailboxes: %v", err)
	}
	return acct, sent, inbox
}

func (f *trainFixture) fileAt(t *testing.T, acct, mailbox string, msg ulid.ULID, uid int) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(), `INSERT INTO app.message_provenance
	        (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity) VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		ulid.Make().String(), f.userID.String(), msg.String(), acct, mailbox, uid); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
}

func (f *trainFixture) seedMessage(t *testing.T, from, hint, inReplyTo string, at time.Time) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := ulid.Make()
	hash := sha256.Sum256([]byte("rep-" + id.String()))
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1, $2, 10, 20, $3)`,
		f.userID.String(), hash[:], ulid.Make().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := f.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address, received_at, size_bytes,
		        message_id_hint, in_reply_to)
		 VALUES ($1, $2, $3, 'msg', $4, $5, 10, NULLIF($6, ''), NULLIF($7, ''))`,
		id.String(), f.userID.String(), hash[:], from, at, hint, inReplyTo); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

// The two features that were the label in disguise are rebuilt as of arrival:
// flagged:1 on a message the user flagged (the `flag` positive), and the
// correspondence that exists only because the user replied to it since. The
// Now view keeps both, so the test is also the control: the as-of view differs
// from today's on exactly those keys.
func TestCorpusRebuildsLeakedFeaturesAsOfArrival(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	at := f.now.Add(-30 * time.Hour)

	flagged := f.seedMessage(t, "flagger@one.test", "", "", at)
	if _, err := f.super.Exec(ctx, `UPDATE app.messages SET is_flagged = true WHERE id = $1`, flagged.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.super.Exec(ctx, `INSERT INTO app.feedback_events (id, user_id, message_id, action, created_at)
		VALUES ($1, $2, $3, 'flag', $4)`, ulid.Make().String(), f.userID.String(), flagged.String(), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	replied := f.seedMessage(t, "pal@two.test", "", "", at)
	contact := ulid.Make().String()
	if _, err := f.super.Exec(ctx, `INSERT INTO app.contacts (id, user_id) VALUES ($1, $2)`, contact, f.userID.String()); err != nil {
		t.Fatal(err)
	}
	// One outbound message to pal — the reply itself.
	if _, err := f.super.Exec(ctx, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address, outbound_count)
		VALUES ($1, $2, $3, 'pal@two.test', 1)`, ulid.Make().String(), f.userID.String(), contact); err != nil {
		t.Fatal(err)
	}
	if _, err := f.super.Exec(ctx, `INSERT INTO app.feedback_events (id, user_id, message_id, action, created_at)
		VALUES ($1, $2, $3, 'reply', $4)`, ulid.Make().String(), f.userID.String(), replied.String(), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	c, err := f.trainer.loadPriorityCorpus(ctx, f.userID, corpusOptions{since: time.Time{}, until: farFuture})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[ulid.ULID]PriorityExample{}
	for _, e := range c.Examples {
		byID[e.MessageID] = e
	}
	fe, re := byID[flagged], byID[replied]
	if fe.Now["flagged:1"] == 0 || fe.AsOf["flagged:1"] != 0 {
		t.Errorf("flagged message: now=%v asOf=%v, want flagged:1 only today", fe.Now["flagged:1"], fe.AsOf["flagged:1"])
	}
	if re.Now["corr:1"] == 0 || re.AsOf["corr:1"] != 0 {
		t.Errorf("replied message: now corr=%v asOf corr=%v, want correspondence only today", re.Now["corr:1"], re.AsOf["corr:1"])
	}
	if c.FlagMasked != 1 || c.CorrMasked != 1 || c.CorrOnlyFromReplies != 1 {
		t.Errorf("mask counts = flag %d corr %d corr-only %d, want 1/1/1", c.FlagMasked, c.CorrMasked, c.CorrOnlyFromReplies)
	}
}

func TestReportCountsUnrecordedRepliesAndBackfillRecordsThemOnce(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	acct, sent, inbox := f.seedIdentity(t, "me@self.test")
	at := f.now.Add(-30 * time.Hour)
	orig := f.seedMessage(t, "friend@other.test", "orig-1@other.test", "", at)
	reply := f.seedMessage(t, "me@self.test", "reply-1@self.test", "orig-1@other.test", at.Add(time.Hour))
	f.fileAt(t, acct, sent, reply, 1)
	// A message nobody replied to — except a SPOOF in the INBOX, From the
	// user's address, naming it. The census and backfill must skip the spoof.
	news := f.seedMessage(t, "news@bulk.test", "news-1@bulk.test", "", at)
	spoof := f.seedMessage(t, "me@self.test", "spoof-1@attacker.test", "news-1@bulk.test", at.Add(time.Hour))
	f.fileAt(t, acct, inbox, spoof, 1)

	r, err := f.trainer.BuildPriorityReport(ctx, f.userID, time.Time{}, 0)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if r.Replies.OwnThreaded != 1 || r.Replies.Originals != 1 || r.Replies.Unrecorded != 1 || r.Replies.Recorded != 0 {
		t.Fatalf("census before backfill = %+v, want 1 own, 1 original, 1 unrecorded", r.Replies)
	}
	if r.Replies.UnverifiedOwn != 1 {
		t.Errorf("unverified own = %d, want 1 (the INBOX spoof)", r.Replies.UnverifiedOwn)
	}
	if r.BySource["reply-derived"] != 1 || r.Positives != 1 {
		t.Errorf("the derived reply is not a positive in the report: by source %v, positives %d", r.BySource, r.Positives)
	}

	c, err := f.trainer.CensusReplies(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.trainer.BackfillReplies(ctx, f.userID, c)
	if err != nil || n != 1 {
		t.Fatalf("backfill wrote %d (err %v), want 1", n, err)
	}
	n, err = f.trainer.BackfillReplies(ctx, f.userID, c)
	if err != nil || n != 0 {
		t.Fatalf("second backfill wrote %d (err %v), want 0", n, err)
	}
	var rows int
	var created time.Time
	if err := f.super.QueryRow(ctx,
		`SELECT count(*), max(created_at) FROM app.feedback_events WHERE message_id = $1 AND action = 'reply'`,
		orig.String()).Scan(&rows, &created); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("reply rows on the original = %d, want 1", rows)
	}
	// Dated at the reply, not at the backfill, so it sits where it happened in
	// the training window.
	if !created.Equal(at.Add(time.Hour)) {
		t.Errorf("backfilled reply dated %v, want the reply's time %v", created, at.Add(time.Hour))
	}

	var onNews int
	if err := f.super.QueryRow(ctx, `SELECT count(*) FROM app.feedback_events WHERE message_id = $1`, news.String()).Scan(&onNews); err != nil {
		t.Fatal(err)
	}
	if onNews != 0 {
		t.Fatalf("the backfill recorded %d events from the INBOX spoof", onNews)
	}

	r, err = f.trainer.BuildPriorityReport(ctx, f.userID, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Replies.Recorded != 1 || r.Replies.Unrecorded != 0 {
		t.Errorf("census after backfill = %+v, want 1 recorded, 0 unrecorded", r.Replies)
	}
}

// #867: a reply an AI-tool key sent is neither "Unrecorded" forever nor
// backfilled; it is counted on its own line. NEGATIVE CONTROL: the user's own
// reply, in the same census, is still an unrecorded original that the backfill
// records.
func TestReportCountsKeySentRepliesSeparately(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()
	acct, sent, _ := f.seedIdentity(t, "me@self.test")
	at := f.now.Add(-30 * time.Hour)
	origUser := f.seedMessage(t, "friend@other.test", "orig-u@other.test", "", at)
	replyUser := f.seedMessage(t, "me@self.test", "reply-u@self.test", "orig-u@other.test", at.Add(time.Hour))
	f.fileAt(t, acct, sent, replyUser, 1)
	origKey := f.seedMessage(t, "friend@other.test", "orig-k@other.test", "", at)
	replyKey := f.seedMessage(t, "me@self.test", "reply-k@self.test", "orig-k@other.test", at.Add(time.Hour))
	f.fileAt(t, acct, sent, replyKey, 2)

	keyID := ulid.Make().String()
	hash := sha256.Sum256([]byte("key-" + keyID))
	if _, err := f.super.Exec(ctx, `INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes)
	      VALUES ($1, $2, 'ai tool', $3, ARRAY['mail:read'])`, keyID, f.userID.String(), hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.super.Exec(ctx, `INSERT INTO app.api_key_drafts (user_id, message_id, key_id) VALUES ($1, $2, $3)`,
		f.userID.String(), replyKey.String(), keyID); err != nil {
		t.Fatal(err)
	}

	c, err := f.trainer.CensusReplies(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if c.KeySent != 1 || c.Originals != 1 || c.Unrecorded != 1 {
		t.Fatalf("census = key-sent %d, originals %d, unrecorded %d; want 1/1/1", c.KeySent, c.Originals, c.Unrecorded)
	}
	n, err := f.trainer.BackfillReplies(ctx, f.userID, c)
	if err != nil || n != 1 {
		t.Fatalf("backfill wrote %d (err %v), want 1 (the user's reply only)", n, err)
	}
	for orig, want := range map[ulid.ULID]int{origUser: 1, origKey: 0} {
		var rows int
		if err := f.super.QueryRow(ctx,
			`SELECT count(*) FROM app.feedback_events WHERE message_id = $1 AND action = 'reply'`, orig.String()).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != want {
			t.Errorf("reply rows on %s = %d, want %d", orig, rows, want)
		}
	}
}
