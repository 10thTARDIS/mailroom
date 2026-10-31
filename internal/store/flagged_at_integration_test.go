// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for flagged_at (#444, migration 00047): the in-tray's
// "waiting since". The claim under test is narrow and load-bearing — the rule
// is enforced in SQL at every write site, not in Go, so these tests exist to
// catch the two columns disagreeing rather than to re-verify is_flagged
// itself:
//
//   - false->true stamps flagged_at to (approximately) now — except at
//     ingest, where a message arriving already flagged is stamped to its
//     received_at instead (the honest lower bound, never now).
//   - true->false clears it back to NULL.
//   - re-asserting the SAME state (flag again while already flagged, or
//     unflag while already unflagged) touches neither column.
//
// Exercised across all three write paths (#444's own list): the JMAP path
// (ApplyEmailUpdate), the inbound-sync merge (UpdateMessageFlagsIfChanged),
// and a brand-new arrival (InsertMessage). The rule at ingest (rules.go's
// ActionFlag) is covered in internal/ingest/rules_integration_test.go instead,
// because it needs the rule engine wired up, not this file's bare fixture.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type flaggedAtFixture struct {
	h      *harness
	st     *Store
	userID ulid.ULID
}

func newFlaggedAtFixture(t *testing.T) *flaggedAtFixture {
	t.Helper()
	h := newHarness(t)
	f := &flaggedAtFixture{h: h, st: h.appStore(), userID: NewULID()}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return f
}

// seedMessage inserts a canonical, unflagged message (plus the blob and
// thread its foreign keys require) and returns its id — mirrors
// snoozeFixture.seedMessage's shape.
func (f *flaggedAtFixture) seedMessage(t *testing.T, subject string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	threadID := NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
	      VALUES ($1, $2, $3, $4, $5, now(), 10)`,
		id.String(), f.userID.String(), hash[:], threadID.String(), subject)
	return id
}

// apply runs one Email/set-shaped update through the audited path.
func (f *flaggedAtFixture) apply(t *testing.T, id ulid.ULID, up EmailUpdate) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, id, up)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("message %s not found by ApplyEmailUpdate", id)
		}
		return nil
	}); err != nil {
		t.Fatalf("apply email update: %v", err)
	}
}

// flaggedAt reads the raw column, as the superuser: the assertions are about
// what is actually stored, not about what a projection reports.
func (f *flaggedAtFixture) flaggedAt(t *testing.T, id ulid.ULID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT flagged_at FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), id.String()).Scan(&at); err != nil {
		t.Fatalf("read flagged_at: %v", err)
	}
	return at
}

func (f *flaggedAtFixture) emailChanges(t *testing.T, id ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.userID.String(), id.String()).Scan(&n); err != nil {
		t.Fatalf("count Email changes: %v", err)
	}
	return n
}

// assertRecent fails unless got is within tolerance of now — "approximately
// now", not "exactly now", because the SQL now() and the test's clock read are
// two different instants.
func assertRecent(t *testing.T, got *time.Time, tolerance time.Duration) {
	t.Helper()
	if got == nil {
		t.Fatal("flagged_at = nil, want ~now")
	}
	if d := time.Since(*got); d < -tolerance || d > tolerance {
		t.Errorf("flagged_at = %v, want within %v of now (off by %v)", got, tolerance, d)
	}
}

// THE HEADLINE (#444): flagged_at follows is_flagged through every transition
// of the JMAP path, and does not churn on a re-assertion of the same state.
func TestFlaggedAtFollowsTheFlag(t *testing.T) {
	f := newFlaggedAtFixture(t)
	msg := f.seedMessage(t, "unflagged to start")

	if got := f.flaggedAt(t, msg); got != nil {
		t.Fatalf("freshly seeded message flagged_at = %v, want nil", got)
	}

	// false -> true: stamped to ~now.
	flag := true
	f.apply(t, msg, EmailUpdate{Flagged: &flag})
	first := f.flaggedAt(t, msg)
	assertRecent(t, first, 10*time.Second)
	changesAfterFirstFlag := f.emailChanges(t, msg)

	// true -> true (re-flag): unchanged — same instant, not bumped, and no new
	// change_log row (the IS DISTINCT FROM guard sees no real change).
	f.apply(t, msg, EmailUpdate{Flagged: &flag})
	if got := f.flaggedAt(t, msg); got == nil || !got.Equal(*first) {
		t.Errorf("re-flagging an already-flagged message: flagged_at = %v, want unchanged %v", got, first)
	}
	if n := f.emailChanges(t, msg); n != changesAfterFirstFlag {
		t.Errorf("re-flagging appended a change_log row (%d -> %d)", changesAfterFirstFlag, n)
	}

	// true -> false: cleared to NULL.
	unflag := false
	f.apply(t, msg, EmailUpdate{Flagged: &unflag})
	if got := f.flaggedAt(t, msg); got != nil {
		t.Errorf("flagged_at after unflag = %v, want nil", got)
	}

	// false -> true again: a NEW, LATER instant — not the first flag's stamp
	// resurrected. A short sleep guarantees the two reads cannot land in the
	// same clock tick.
	time.Sleep(10 * time.Millisecond)
	f.apply(t, msg, EmailUpdate{Flagged: &flag})
	second := f.flaggedAt(t, msg)
	assertRecent(t, second, 10*time.Second)
	if second == nil || !second.After(*first) {
		t.Errorf("second flag's flagged_at = %v, want strictly after the first flag's %v", second, first)
	}
}

// The same three transitions through the inbound-sync merge path
// (UpdateMessageFlagsIfChanged), which folds a server's remote flag state into
// the canonical row outside of any Email/set patch.
func TestFlaggedAtFollowsTheFlagViaSync(t *testing.T) {
	f := newFlaggedAtFixture(t)
	msg := f.seedMessage(t, "synced from the server")
	ctx := context.Background()

	sync := func(flagged bool) (changed bool) {
		t.Helper()
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var err error
			changed, err = tx.UpdateMessageFlagsIfChanged(ctx, msg, MessageFlags{Flagged: flagged, Keywords: []string{}})
			return err
		}); err != nil {
			t.Fatalf("update message flags: %v", err)
		}
		return changed
	}

	if got := f.flaggedAt(t, msg); got != nil {
		t.Fatalf("freshly seeded message flagged_at = %v, want nil", got)
	}

	// false -> true: stamped, and reported as a real change.
	if !sync(true) {
		t.Fatal("sync flagged=true on an unflagged message reported no change")
	}
	first := f.flaggedAt(t, msg)
	assertRecent(t, first, 10*time.Second)

	// true -> true: no-op, both for the guard and for the timestamp.
	if sync(true) {
		t.Error("re-syncing the same flagged=true reported a change")
	}
	if got := f.flaggedAt(t, msg); got == nil || !got.Equal(*first) {
		t.Errorf("re-syncing flagged=true: flagged_at = %v, want unchanged %v", got, first)
	}

	// true -> false: cleared.
	if !sync(false) {
		t.Fatal("sync flagged=false on a flagged message reported no change")
	}
	if got := f.flaggedAt(t, msg); got != nil {
		t.Errorf("flagged_at after sync unflag = %v, want nil", got)
	}

	// false -> true again: a new, later instant.
	time.Sleep(10 * time.Millisecond)
	if !sync(true) {
		t.Fatal("re-flagging via sync after an unflag reported no change")
	}
	second := f.flaggedAt(t, msg)
	assertRecent(t, second, 10*time.Second)
	if second == nil || !second.After(*first) {
		t.Errorf("second sync flag's flagged_at = %v, want strictly after the first's %v", second, first)
	}
}

// A message that ARRIVES already flagged (InsertMessage — initial sync of a
// mailbox starred long ago, or another client's \Flagged beating Mailroom to
// it) gets flagged_at = received_at, never now(): now() would read an old
// starred message as "waiting 0 d" the instant it is ingested, which is worse
// than the honest lower bound (#444, migration 00047's header).
func TestInsertMessageFlaggedGetsReceivedAtAsFlaggedAt(t *testing.T) {
	f := newFlaggedAtFixture(t)
	ctx := context.Background()
	receivedAt := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)

	var msgID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		hash := sha256.Sum256([]byte("insert-flagged-" + f.userID.String()))
		if err := tx.InsertBlobMeta(ctx, BlobMeta{
			ContentHash: hash[:], SizePlain: 10, SizeStored: 10, DEKID: NewULID(), EnvelopeVer: 1,
		}); err != nil {
			return err
		}
		threadID := NewULID()
		if _, err := tx.tx.Exec(ctx, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
			threadID.String(), tx.userID.String(), "arrived already flagged"); err != nil {
			return err
		}
		msgID = NewULID()
		return tx.InsertMessage(ctx, MessageRow{
			ID: msgID, ContentHash: hash[:], ThreadID: threadID,
			Subject: "arrived already flagged", FromAddress: "sender@example.com",
			ReceivedAt: receivedAt, SizeBytes: 10,
			ReferenceIDs: []string{},
			Flags:        MessageFlags{Flagged: true, Keywords: []string{}},
		})
	}); err != nil {
		t.Fatalf("insert flagged message: %v", err)
	}

	got := f.flaggedAt(t, msgID)
	if got == nil || !got.Equal(receivedAt) {
		t.Errorf("flagged_at = %v, want exactly received_at %v (a lower bound, not now)", got, receivedAt)
	}

	// And the ordinary, unflagged case: NULL, not received_at.
	var unflaggedID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		hash := sha256.Sum256([]byte("insert-unflagged-" + f.userID.String()))
		if err := tx.InsertBlobMeta(ctx, BlobMeta{
			ContentHash: hash[:], SizePlain: 10, SizeStored: 10, DEKID: NewULID(), EnvelopeVer: 1,
		}); err != nil {
			return err
		}
		threadID := NewULID()
		if _, err := tx.tx.Exec(ctx, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
			threadID.String(), tx.userID.String(), "arrived unflagged"); err != nil {
			return err
		}
		unflaggedID = NewULID()
		return tx.InsertMessage(ctx, MessageRow{
			ID: unflaggedID, ContentHash: hash[:], ThreadID: threadID,
			Subject: "arrived unflagged", FromAddress: "sender@example.com",
			ReceivedAt: receivedAt, SizeBytes: 10,
			ReferenceIDs: []string{},
			Flags:        MessageFlags{Keywords: []string{}},
		})
	}); err != nil {
		t.Fatalf("insert unflagged message: %v", err)
	}
	if got := f.flaggedAt(t, unflaggedID); got != nil {
		t.Errorf("unflagged arrival flagged_at = %v, want nil", got)
	}
}
