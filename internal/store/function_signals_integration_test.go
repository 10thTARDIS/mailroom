// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the #156 function-signal store surface (migration
// 00027): InsertMessage's six new columns, and the guarded backfill update
// UpdateMessageFunctionSignals. Mirrors resnippet_integration_test.go's shape.
// The load-bearing invariant (CLAUDE.md) does NOT apply here in the usual
// sense — these columns are an envelope projection, not a JMAP-visible Email
// property (see function_signals.go's file header) — so what these tests
// assert instead is that NO change_log row is appended by either write path,
// which is the deliberate, documented behaviour, not an oversight.

import (
	"context"
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// changeLogCount counts every change_log row for one message, inside the
// caller's transaction so it sees uncommitted appends.
func changeLogCount(t *testing.T, tx *UserTx, msgID string) int {
	t.Helper()
	var n int
	if err := tx.tx.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_id = $2`,
		tx.userID.String(), msgID).Scan(&n); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	return n
}

// insertTestMessage inserts a brand-new message (its own blob + thread, so it
// does not collide with the fixture message seedUser already created) via
// InsertMessage, carrying the given function signals, and returns its id.
func insertTestMessage(t *testing.T, tx *UserTx, tag string, sig FunctionSignals) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	hash := sha256.Sum256([]byte("function-signals-blob-" + tag))
	if err := tx.InsertBlobMeta(ctx, BlobMeta{
		ContentHash: hash[:], SizePlain: 10, SizeStored: 10, DEKID: NewULID(), EnvelopeVer: 1,
	}); err != nil {
		t.Fatalf("insert blob meta: %v", err)
	}
	threadID := NewULID()
	if _, err := tx.tx.Exec(ctx, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), tx.userID.String(), "function signals "+tag); err != nil {
		t.Fatalf("insert thread: %v", err)
	}
	msgID := NewULID()
	row := MessageRow{
		ID: msgID, ContentHash: hash[:], ThreadID: threadID,
		Subject: "function signals " + tag, FromAddress: "sender@example.com",
		ReceivedAt: time.Now().UTC(), SizeBytes: 10,
		// NOT NULL DEFAULT '{}' in the schema; InsertMessage does not fill
		// these in for the caller (see mapFlags/nonNil at the ingest call site).
		ReferenceIDs: []string{},
		Flags:        MessageFlags{Keywords: []string{}},
	}
	row.ListUnsubscribePost = sig.ListUnsubscribePost
	row.AutoSubmitted = sig.AutoSubmitted
	row.Precedence = sig.Precedence
	row.FeedbackID = sig.FeedbackID
	row.DKIMDomains = sig.DKIMDomains
	row.ReturnPath = sig.ReturnPath
	if err := tx.InsertMessage(ctx, row); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return msgID
}

// readFunctionSignalColumns reads the raw stored columns back, distinguishing
// a NULL text[] from an absent row by returning ok=false only when the row
// itself does not exist.
func readFunctionSignalColumns(t *testing.T, tx *UserTx, msgID ulid.ULID) (sig FunctionSignals, ok bool) {
	t.Helper()
	var (
		lup, as, prec, fb, rp *string
		dkim                  []string
	)
	err := tx.tx.QueryRow(context.Background(), `
		SELECT list_unsubscribe_post, auto_submitted, precedence, feedback_id, dkim_domains, return_path
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), msgID.String()).Scan(&lup, &as, &prec, &fb, &dkim, &rp)
	if isNoRows(err) {
		return FunctionSignals{}, false
	}
	if err != nil {
		t.Fatalf("read function signal columns: %v", err)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return FunctionSignals{
		ListUnsubscribePost: deref(lup),
		AutoSubmitted:       deref(as),
		Precedence:          deref(prec),
		FeedbackID:          deref(fb),
		DKIMDomains:         dkim,
		ReturnPath:          deref(rp),
	}, true
}

// TestInsertMessageStoresFunctionSignals: a message ingested with all six
// signals present stores them verbatim, and one with none of them present
// stores NULL/NULL text[] (read back as the zero value) — never "" vs NULL
// confusion, and no dkim_domains ends up as '{}' rather than NULL (migration
// 00027's NULL-collapsing decision, exercised end to end).
func TestInsertMessageStoresFunctionSignals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		full := FunctionSignals{
			ListUnsubscribePost: "One-Click",
			AutoSubmitted:       "auto-generated",
			Precedence:          "bulk",
			FeedbackID:          "1:2:3:SES",
			DKIMDomains:         []string{"esp.example", "brand.example"},
			ReturnPath:          "<bounce@example.com>",
		}
		fullID := insertTestMessage(t, tx, "full", full)
		got, ok := readFunctionSignalColumns(t, tx, fullID)
		if !ok {
			t.Fatal("full message not found after insert")
		}
		if !reflect.DeepEqual(got, full) {
			t.Errorf("stored function signals = %+v, want %+v", got, full)
		}

		emptyID := insertTestMessage(t, tx, "empty", FunctionSignals{})
		got, ok = readFunctionSignalColumns(t, tx, emptyID)
		if !ok {
			t.Fatal("empty message not found after insert")
		}
		if !reflect.DeepEqual(got, FunctionSignals{}) {
			t.Errorf("stored function signals for no-headers message = %+v, want zero value", got)
		}

		// dkim_domains specifically: confirm it round-trips as NULL, not '{}', for
		// the no-DKIM message — the migration's NULL-collapsing decision.
		var dkimIsNull bool
		if err := tx.tx.QueryRow(ctx,
			`SELECT dkim_domains IS NULL FROM app.messages WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), emptyID.String()).Scan(&dkimIsNull); err != nil {
			return err
		}
		if !dkimIsNull {
			t.Error("dkim_domains for a no-DKIM message is not NULL (expected NULL, not '{}')")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestInsertMessageAppendsNoChangeLogForFunctionSignals: the function signals
// are operational, not JMAP-visible, so InsertMessage itself must append no
// change_log row on their account — only the caller's own Email `created`
// append (which this test does NOT perform, so a stray row here can only have
// come from InsertMessage).
func TestInsertMessageAppendsNoChangeLogForFunctionSignals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		msgID := insertTestMessage(t, tx, "changelog-check", FunctionSignals{
			ListUnsubscribePost: "One-Click",
			DKIMDomains:         []string{"example.com"},
		})
		if got := changeLogCount(t, tx, msgID.String()); got != 0 {
			t.Errorf("change_log rows for a message InsertMessage alone created = %d, want 0", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUpdateMessageFunctionSignals exercises the backfill's guarded UPDATE:
// a real change writes and reports changed=true; re-running with the SAME
// values is a no-op; and neither write appends a change_log row.
func TestUpdateMessageFunctionSignals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// Start from a message with none of the six signals (as every
		// pre-migration row does).
		msgID := insertTestMessage(t, tx, "backfill", FunctionSignals{})
		before := changeLogCount(t, tx, msgID.String())

		sig := FunctionSignals{
			ListUnsubscribePost: "One-Click",
			AutoSubmitted:       "auto-generated",
			Precedence:          "bulk",
			FeedbackID:          "1:2:3:SES",
			DKIMDomains:         []string{"esp.example"},
			ReturnPath:          "<bounce@example.com>",
		}

		// 1. Real change -> written, changed=true, no change_log row appended.
		changed, err := tx.UpdateMessageFunctionSignals(ctx, msgID, sig)
		if err != nil {
			return err
		}
		if !changed {
			t.Error("first UpdateMessageFunctionSignals: changed = false, want true")
		}
		if got := changeLogCount(t, tx, msgID.String()); got != before {
			t.Errorf("change_log rows after real change = %d, want %d (no new row)", got, before)
		}
		got, ok := readFunctionSignalColumns(t, tx, msgID)
		if !ok {
			t.Fatal("message not found after update")
		}
		if !reflect.DeepEqual(got, sig) {
			t.Errorf("stored function signals after update = %+v, want %+v", got, sig)
		}

		// 2. No-op (identical values) -> changed=false, still no new row.
		changed, err = tx.UpdateMessageFunctionSignals(ctx, msgID, sig)
		if err != nil {
			return err
		}
		if changed {
			t.Error("no-op UpdateMessageFunctionSignals: changed = true, want false")
		}
		if got := changeLogCount(t, tx, msgID.String()); got != before {
			t.Errorf("change_log rows after no-op = %d, want %d (no new row)", got, before)
		}

		// 3. Unknown id -> changed=false, no error, no row.
		other := NewULID()
		changed, err = tx.UpdateMessageFunctionSignals(ctx, other, sig)
		if err != nil {
			return err
		}
		if changed {
			t.Error("unknown-id UpdateMessageFunctionSignals: changed = true, want false")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
