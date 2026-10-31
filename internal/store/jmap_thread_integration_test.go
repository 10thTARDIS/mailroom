// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Thread membership under soft-delete (mailroom#84 follow-up).
//
// Email/query hides trashed messages, because Email/set already reported them to
// the client as `destroyed` (ADR-009). Thread/get has to agree: a reading pane
// renders a thread's emailIds, so a trashed message left in that list comes back
// on screen right after the list view was told it was gone.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// seedThreadMessage inserts one message (and the blob its FK requires) into a
// thread, optionally trashed.
func seedThreadMessage(t *testing.T, h *harness, userID string, threadID, msgID ulid.ULID, subject string, trashed bool) {
	t.Helper()
	hash := make([]byte, 32)
	copy(hash, msgID[:])
	mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	                VALUES ($1, $2, 10, 20, $3)`, userID, hash, NewULID().String())
	trash := "NULL"
	if trashed {
		trash = "now()"
	}
	mustExec(t, h, `INSERT INTO app.messages
	                  (id, user_id, thread_id, content_hash, subject, received_at, size_bytes, trashed_at)
	                VALUES ($1, $2, $3, $4, $5, now(), 10, `+trash+`)`,
		msgID.String(), userID, threadID.String(), hash, subject)
}

func TestGetThreadsExcludesTrashedMessages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "thread-trash")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	threadID := NewULID()
	mustExec(t, h, `INSERT INTO app.threads (id, user_id) VALUES ($1, $2)`, threadID.String(), u.ID)

	live := NewULID()
	trashed := NewULID()
	seedThreadMessage(t, h, u.ID, threadID, live, "still here", false)
	seedThreadMessage(t, h, u.ID, threadID, trashed, "deleted", true)

	var views []ThreadView
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		var err error
		views, err = tx.GetThreads(ctx, []ulid.ULID{threadID})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if len(views) != 1 {
		t.Fatalf("GetThreads returned %d threads, want 1", len(views))
	}
	got := views[0].EmailIDs
	if len(got) != 1 || got[0] != live {
		t.Errorf("thread emailIds = %v, want exactly the untrashed message %v "+
			"(a trashed message the client was told is destroyed came back through Thread/get)", got, live)
	}
}

// Every message trashed ⇒ the thread is gone as far as the client is concerned.
// Reporting an existing-but-empty Thread would leave a reading pane rendering
// nothing, and an RFC 8621 Thread is defined by its emailIds.
func TestGetThreadsOmitsFullyTrashedThread(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "thread-alltrash")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	threadID := NewULID()
	mustExec(t, h, `INSERT INTO app.threads (id, user_id) VALUES ($1, $2)`, threadID.String(), u.ID)
	seedThreadMessage(t, h, u.ID, threadID, NewULID(), "gone", true)

	var views []ThreadView
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		var err error
		views, err = tx.GetThreads(ctx, []ulid.ULID{threadID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Errorf("GetThreads returned %d threads, want 0 — a thread whose every message is trashed "+
			"must land in notFound, not come back empty", len(views))
	}
}
