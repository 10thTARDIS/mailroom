// SPDX-License-Identifier: AGPL-3.0-only

package store

// The two judgements the ingest-only ledger makes about a suppressed row before
// it is replayed (ADR-028 amendments A2 and A4). They are SQL predicates over an
// app.writeback_intents row aliased `w`, defined ONCE here and spliced into both
// the report and the replay, so the report can never describe a set that --apply
// does not act on.
//
// ⚠ ONE PREDICATE FOR ONE QUESTION. Three review rounds of ADR-028 were spent on
// two predicates that disagreed about the same fact (the purge deferral, the
// ledger state, the physical copy). A report that counts "stale" with one
// expression and an apply that skips "stale" with another is that defect again,
// and the report would be the one to lie.

// ledgerStaleOps are the ops that address an EXISTING copy of a message, which
// is what makes "the copy's surroundings changed" meaningful. An append has no
// copy by construction (its message is local until the APPEND round-trips), and
// a purge expunge is re-checked against the copy's physical place by the engine
// at apply time (#555), so neither is judged here.
const ledgerStaleOps = `('move', '+flag', '-flag', '+kw', '-kw')`

// ledgerStaleSQL: the message gained a LIVE copy in the intent's account AFTER
// the intent was created.
//
// ⚠ WHY THIS IS THE SIGNAL. Replay is a delta replay, not a state reconcile
// (ADR-028 §7a): a week-old `move` is applied to wherever the copy is NOW, so
// filing done from another client in the meantime would be silently reversed.
// The trace such filing leaves is a new provenance row (the copy appearing in its
// new folder, or in a rolled UIDVALIDITY generation) whose first_seen_at is later
// than the intent. It is strictly `>`: an intent enqueued in the same transaction
// that ingested its message (an ingest-time rule) shares that transaction's now()
// and is not stale.
//
// What it cannot see, stated here because the report states it too: a copy that
// only changed flags, and a message another client moved into a folder where
// Mailroom already held a copy. Both errors land on the safe side.
const ledgerStaleSQL = `(w.op IN ` + ledgerStaleOps + `
	AND w.message_id IS NOT NULL
	AND EXISTS (
		SELECT 1 FROM app.message_provenance p
		 WHERE p.user_id = w.user_id AND p.message_id = w.message_id
		   AND p.account_id = w.account_id AND p.state = 'live'
		   AND p.first_seen_at > w.created_at))`

// ledgerSupersededSQL: a LATER append of the same message into the same mailbox
// exists, so this one files bytes the draft no longer has.
//
// The later row must itself still be capable of filing (suppressed, pending,
// inflight or applied); a dead or discarded successor carries nothing, so it does
// not retire its predecessor. "Later" is (created_at, id), the order
// ClaimPendingIntents applies in, so the verdict does not depend on which of the
// two the planner meets first.
const ledgerSupersededSQL = `(w.op = 'append'
	AND w.message_id IS NOT NULL
	AND EXISTS (
		SELECT 1 FROM app.writeback_intents n
		 WHERE n.user_id = w.user_id AND n.account_id = w.account_id
		   AND n.message_id = w.message_id AND n.op = 'append'
		   AND n.id <> w.id
		   AND n.payload->>'mailbox' IS NOT DISTINCT FROM w.payload->>'mailbox'
		   AND n.state IN ('suppressed', 'pending', 'inflight', 'applied')
		   AND (n.created_at, n.id COLLATE "C") > (w.created_at, w.id COLLATE "C")))`
