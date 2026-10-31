// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The Priority Inbox query surface (spec §6/§8, mailroom#154): selecting mail by
// the T1 model's verdict, and the two-bucket ordering the view is built on.
//
// Both are tested against real PostgreSQL because both are SQL — the filter is a
// correlated EXISTS over app.triage_scores and the ordering is a boolean sort key
// in the ORDER BY, and neither is meaningfully exercised by a fake.
//
// The case that matters most here is the UNSCORED message. A model that has not
// spoken must not promote mail, but it must not swallow it either: mail ingested
// before scoring was wired, or whose scoring failed, still has to appear.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

const testImportantThreshold = 0.6

type priorityFixture struct {
	h      *harness
	st     *Store
	userID ulid.ULID
}

func newPriorityFixture(t *testing.T) *priorityFixture {
	t.Helper()
	h := newHarness(t)
	f := &priorityFixture{h: h, st: h.appStore(), userID: NewULID()}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return f
}

// seedScored inserts a message aged by `age` and, when score >= 0, a triage row.
// A negative score means UNSCORED — no row at all, which is the state the filter
// has to handle correctly rather than the state a test would naturally create.
func (f *priorityFixture) seedScored(t *testing.T, subject, age string, score float64) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	sum := sha256.Sum256([]byte(subject + id.String()))
	threadID := NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), sum[:], NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
	      VALUES ($1, $2, $3, $4, $5, now() - $6::interval, 10)`,
		id.String(), f.userID.String(), sum[:], threadID.String(), subject, age)

	if score >= 0 {
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			return tx.UpsertTriageScore(ctx, id, score, "v1", nil)
		}); err != nil {
			t.Fatalf("score message: %v", err)
		}
	}
	return id
}

func (f *priorityFixture) query(t *testing.T, q EmailQuery) []ulid.ULID {
	t.Helper()
	ctx := context.Background()
	q.ImportantThreshold = testImportantThreshold
	var ids []ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		ids, _, err = tx.QueryEmails(ctx, q)
		return err
	}); err != nil {
		t.Fatalf("query emails: %v", err)
	}
	return ids
}

// The filter splits on the threshold, and an UNSCORED message lands on the
// not-important side rather than disappearing from both.
func TestImportantFilterSplitsOnTheThresholdAndKeepsUnscoredMail(t *testing.T) {
	f := newPriorityFixture(t)
	high := f.seedScored(t, "from the boss", "2 hours", 0.91)
	atThreshold := f.seedScored(t, "exactly at the line", "3 hours", testImportantThreshold)
	low := f.seedScored(t, "a newsletter", "4 hours", 0.12)
	unscored := f.seedScored(t, "never scored", "5 hours", -1)

	yes, no := true, false

	important := f.query(t, EmailQuery{Important: &yes})
	got := map[ulid.ULID]bool{}
	for _, id := range important {
		got[id] = true
	}
	if !got[high] || !got[atThreshold] {
		t.Errorf("important = %v, want it to include the 0.91 and the boundary 0.60 message", important)
	}
	if got[low] || got[unscored] {
		t.Error("important included a low-scoring or unscored message")
	}

	rest := f.query(t, EmailQuery{Important: &no})
	got = map[ulid.ULID]bool{}
	for _, id := range rest {
		got[id] = true
	}
	if !got[low] {
		t.Error("the low-scoring message is missing from the not-important half")
	}
	if !got[unscored] {
		t.Error("AN UNSCORED MESSAGE VANISHED: it is not important, but it must still be somewhere — " +
			"mail ingested before scoring was wired would otherwise be invisible in both halves")
	}
	if got[high] {
		t.Error("the not-important half included an important message")
	}
}

// THE VIEW'S ORDER (spec §6): important first, then everything else, each
// newest-first. The grouping has to happen in SQL — a client can only reorder
// the page it already holds, so a bucket boundary computed client-side would be
// wrong the moment the list is longer than one page.
func TestPrioritySortGroupsImportantFirstThenByDate(t *testing.T) {
	f := newPriorityFixture(t)
	// Deliberately interleaved by age: the OLDEST message is important and the
	// NEWEST is not, so a plain date sort produces the opposite order and the
	// test cannot pass by accident.
	oldImportant := f.seedScored(t, "important, last week", "7 days", 0.95)
	newImportant := f.seedScored(t, "important, an hour ago", "1 hour", 0.77)
	oldRest := f.seedScored(t, "noise, three days ago", "3 days", 0.10)
	newRest := f.seedScored(t, "noise, ten minutes ago", "10 minutes", -1)

	got := f.query(t, EmailQuery{SortByPriority: true, SortByEffectiveArrival: true})
	want := []ulid.ULID{newImportant, oldImportant, newRest, oldRest}
	if len(got) != len(want) {
		t.Fatalf("got %d ids, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priority order = %v, want %v (important bucket first, newest-first within each)", got, want)
		}
	}
}

// The bucket sort composes with the snooze exclusion the inbox already applies —
// the Priority view is the inbox, so snoozed mail stays out of it.
func TestPrioritySortStillExcludesSnoozedMail(t *testing.T) {
	f := newPriorityFixture(t)
	visible := f.seedScored(t, "important and here", "1 hour", 0.9)
	away := f.seedScored(t, "important but snoozed", "2 hours", 0.9)

	wake := time.Now().Add(6 * time.Hour).UTC()
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(context.Background(), away, EmailUpdate{SnoozeUntil: &wake})
		return err
	}); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	no := false
	got := f.query(t, EmailQuery{SortByPriority: true, Snoozed: &no})
	if len(got) != 1 || got[0] != visible {
		t.Errorf("priority view = %v, want only the un-snoozed important message %v", got, visible)
	}
}
