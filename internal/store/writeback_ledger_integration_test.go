// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer tests for the ingest-only ledger (ADR-028 and its amendments):
// the index behind the ledger count, the stale/superseded findings, replay,
// discard, and the enqueue lock.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// TestSuppressedLedgerHasAnIndex pins ADR-028 A6: the partial index must exist
// AND be one the ledger queries can use. A partial index whose predicate drifts
// from the query's text is created, costs writes, and is never chosen, and
// nothing would notice.
//
// The plan is read with sequential scans disabled, because on a table this small
// the planner would pick a scan whether or not an index could serve the query;
// what is being asked is whether the index is ELIGIBLE.
//
// NEGATIVE CONTROL: drop writeback_suppressed (or write its predicate as
// `state::text = 'suppressed'`) and the plan below falls back to the table.
func TestSuppressedLedgerHasAnIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	tx, err := h.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	// The statement SuppressedIntentCount runs, verbatim apart from the count.
	rows, err := tx.Query(ctx, `
		EXPLAIN SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'suppressed'`,
		a.ID, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "writeback_suppressed") {
		t.Fatalf("the ledger count does not use writeback_suppressed:\n%s", strings.Join(plan, "\n"))
	}
}

// ledgerFx is one seeded user with an empty intent table and a message whose
// only provenance row is dated at a time the test chooses.
type ledgerFx struct {
	t    *testing.T
	h    *harness
	a    fixtureUser
	acct ulid.ULID
	uid  ulid.ULID
	msg  ulid.ULID
}

func newLedgerFx(t *testing.T) *ledgerFx {
	t.Helper()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	f := &ledgerFx{t: t, h: h, a: a, acct: mustULID(t, a.AccountID),
		uid: mustULID(t, a.ID), msg: mustULID(t, a.MessageID)}
	f.exec(`DELETE FROM app.writeback_intents WHERE user_id = $1`, a.ID)
	return f
}

func (f *ledgerFx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.h.super.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// dateProvenance sets every provenance row of the seeded message to when.
func (f *ledgerFx) dateProvenance(when time.Time) {
	f.t.Helper()
	f.exec(`UPDATE app.message_provenance SET first_seen_at = $3
	         WHERE user_id = $1 AND message_id = $2`, f.a.ID, f.a.MessageID, when)
}

var ledgerUID int64 = 1000

// addCopy gives the seeded message another provenance row in its account.
func (f *ledgerFx) addCopy(first time.Time, state string) {
	f.t.Helper()
	ledgerUID++
	f.exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
	          mailbox_id, uid, uidvalidity, state, first_seen_at)
	        VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8)`,
		NewULID().String(), f.a.ID, f.a.MessageID, f.a.AccountID, f.a.MailboxID,
		ledgerUID, state, first)
}

// intent inserts one row directly, so created_at, state and source are exactly
// what the test says (EnqueueIntent would stamp now()).
func (f *ledgerFx) intent(op, state, source string, created time.Time, payload string) ulid.ULID {
	f.t.Helper()
	id := NewULID()
	var src any
	if source != "" {
		src = source
	}
	f.exec(`INSERT INTO app.writeback_intents
	          (id, user_id, message_id, account_id, op, payload, idempotency_key, state, source, created_at)
	        VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8::app.intent_state, $9, $10)`,
		id.String(), f.a.ID, f.a.MessageID, f.a.AccountID, op, payload,
		"ledger-"+id.String(), state, src, created)
	return id
}

func (f *ledgerFx) buckets() map[string]SuppressedIntentRow {
	f.t.Helper()
	var rows []SuppressedIntentRow
	if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
		var e error
		rows, e = tx.SuppressedIntentsByOpAndSource(context.Background(), f.acct)
		return e
	}); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]SuppressedIntentRow{}
	for _, r := range rows {
		out[r.Op+"/"+r.Source] = r
	}
	return out
}

func (f *ledgerFx) state(id ulid.ULID) string {
	f.t.Helper()
	var s string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT state::text FROM app.writeback_intents WHERE id = $1`, id.String()).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// TestLedgerFlagsStaleIntents: an intent is stale when its message gained a LIVE
// copy in the account after the intent was created. Each case is built so that
// the other cases are its negative control: the same ledger holds one intent per
// case, and only the right ones may be flagged.
//
// NEGATIVE CONTROL: change `>` to `<` in ledgerStaleSQL, or delete the
// state = 'live' arm, and a named case below goes red.
func TestLedgerFlagsStaleIntents(t *testing.T) {
	f := newLedgerFx(t)
	now := time.Now()
	f.dateProvenance(now.Add(-5 * time.Hour)) // the copy the intents were made against

	// Created after the only copy was first seen.
	f.intent("move", "suppressed", "user", now.Add(-2*time.Hour), `{"mailbox":"Archive"}`)
	f.intent("+kw", "suppressed", "rule", now.Add(-2*time.Hour), `{"keyword":"x"}`)
	// Older than the others, still after the copy: stale once a later copy appears.
	f.intent("-flag", "suppressed", "user", now.Add(-4*time.Hour), `{"flag":"\\Seen"}`)
	// Not judged: an append has no copy by construction, an expunge is re-checked
	// at apply time. A stale copy must not flag them.
	f.intent("append", "suppressed", "draft", now.Add(-4*time.Hour), `{"mailbox":"Drafts","content_hash":"aa"}`)
	f.intent("expunge", "suppressed", "purge", now.Add(-4*time.Hour), `{}`)

	before := f.buckets()
	for k, r := range before {
		if r.Stale != 0 {
			t.Fatalf("%s reports %d stale before any copy appeared — the baseline is wrong, "+
				"so the next assertions would prove nothing", k, r.Stale)
		}
	}

	// A copy that is NOT live (a rolled generation) does not make anything stale.
	f.addCopy(now.Add(-time.Hour), "stale_uidvalidity")
	for k, r := range f.buckets() {
		if r.Stale != 0 {
			t.Fatalf("%s reports %d stale because of a copy that is not live", k, r.Stale)
		}
	}

	// A live copy first seen an hour ago is later than every intent above.
	f.addCopy(now.Add(-time.Hour), "live")
	got := f.buckets()
	for _, key := range []string{"move/user", "+kw/rule", "-flag/user"} {
		if got[key].Stale != 1 {
			t.Errorf("%s stale = %d, want 1 (a live copy appeared after it was created)", key, got[key].Stale)
		}
	}
	for _, key := range []string{"append/draft", "expunge/purge"} {
		if got[key].Stale != 0 {
			t.Errorf("%s stale = %d, want 0 — ops that address no existing copy are not judged", key, got[key].Stale)
		}
		if got[key].Count != 1 {
			t.Errorf("%s count = %d, want 1 — the bucket vanished", key, got[key].Count)
		}
	}
}

// TestLedgerStaleIsRelativeToTheIntent: a copy first seen BEFORE the intent was
// created is the copy the intent was made against, not a change. This is the
// case that would flag every intent in the ledger if the comparison pointed the
// wrong way.
func TestLedgerStaleIsRelativeToTheIntent(t *testing.T) {
	f := newLedgerFx(t)
	now := time.Now()
	f.dateProvenance(now.Add(-5 * time.Hour))
	f.addCopy(now.Add(-4*time.Hour), "live") // a second copy, also OLD
	f.intent("move", "suppressed", "user", now.Add(-time.Hour), `{"mailbox":"Archive"}`)

	got := f.buckets()["move/user"]
	if got.Count != 1 || got.Stale != 0 {
		t.Fatalf("move/user count=%d stale=%d, want 1 and 0 — copies that predate the intent are its baseline", got.Count, got.Stale)
	}
	// Same instant is not later: an ingest-time rule shares the ingesting
	// transaction's now() with the provenance row.
	at := now.Add(-30 * time.Minute)
	f.addCopy(at, "live")
	f.intent("+kw", "suppressed", "rule", at, `{"keyword":"y"}`)
	if got := f.buckets()["+kw/rule"]; got.Stale != 0 {
		t.Fatalf("+kw/rule stale = %d, want 0 — a copy first seen at the intent's own timestamp is not later than it", got.Stale)
	}
}

// TestLedgerFlagsSupersededAppends: only a LATER append of the same message into
// the SAME mailbox, still able to file, supersedes an earlier one.
//
// NEGATIVE CONTROL: drop the mailbox equality, the state filter or the
// created_at comparison from ledgerSupersededSQL and a named case goes red.
func TestLedgerFlagsSupersededAppends(t *testing.T) {
	f := newLedgerFx(t)
	now := time.Now()
	f.dateProvenance(now.Add(-10 * time.Hour))

	// Drafts: v1 then v2, so v1 is superseded and v2 is not.
	v1 := f.intent("append", "suppressed", "draft", now.Add(-3*time.Hour), `{"mailbox":"Drafts","content_hash":"01"}`)
	v2 := f.intent("append", "suppressed", "draft", now.Add(-2*time.Hour), `{"mailbox":"Drafts","content_hash":"02"}`)
	_, _ = v1, v2
	// A different mailbox's append is not superseded by Drafts' later one.
	f.intent("append", "suppressed", "draft", now.Add(-5*time.Hour), `{"mailbox":"Sent","content_hash":"03"}`)

	got := f.buckets()["append/draft"]
	if got.Count != 3 || got.Superseded != 1 {
		t.Fatalf("append/draft count=%d superseded=%d, want 3 and 1: v1 only (v2 is the latest in Drafts, "+
			"and Sent's append has no successor in Sent)", got.Count, got.Superseded)
	}
	// Report which one, not just how many: the count alone cannot tell the
	// right row from the wrong one.
	var superseded []ulid.ULID
	if err := f.h.appStore().UserTx(context.Background(), f.uid, func(tx *UserTx) error {
		rows, err := tx.tx.Query(context.Background(),
			`SELECT w.id FROM app.writeback_intents w WHERE w.user_id = $1 AND `+ledgerSupersededSQL, f.uid.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			superseded = append(superseded, mustULID(t, s))
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 || superseded[0] != v1 {
		t.Fatalf("superseded = %v, want exactly [%s] (the OLDER Drafts append)", superseded, v1)
	}

	// A successor that can no longer file does not retire its predecessor.
	f.exec(`UPDATE app.writeback_intents SET state = 'dead' WHERE id = $1`, v2.String())
	if got := f.buckets()["append/draft"]; got.Superseded != 0 {
		t.Fatalf("superseded = %d with the later append dead, want 0 — a dead successor files nothing", got.Superseded)
	}
}
