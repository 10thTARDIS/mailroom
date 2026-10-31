// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// BLOCK SENDER at ingest (#59): new mail from a blocked contact's address is
// moved to Junk (the default) or Trash through the ORDINARY move — canonical
// placement, Trash's purge clock and an IMAP writeback intent — before rules
// and learned placement ever see it, and it is reported as Result.Blocked so
// the sync engine leaves it out of the push. Every "stays put" test is the
// control for the headline: the move happens for exactly a blocked sender's
// inbox arrival.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// blockFixture adds Junk and Trash mailboxes with their SPECIAL-USE roles to
// the standard fixture (INBOX and Archive are marked by markMailboxRoles).
func blockFixture(t *testing.T) (f *fixture, junk, trash ulid.ULID) {
	t.Helper()
	f = newFixture(t)
	f.markMailboxRoles(t)
	junk, trash = ulid.Make(), ulid.Make()
	for id, m := range map[ulid.ULID][2]string{junk: {"Junk", `\Junk`}, trash: {"Trash", `\Trash`}} {
		if _, err := f.super.Exec(context.Background(),
			`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			 VALUES ($1, $2, $3, $4, $4, $5)`,
			id.String(), f.userID.String(), f.accountID.String(), m[0], m[1]); err != nil {
			t.Fatalf("seed %s: %v", m[0], err)
		}
	}
	return f, junk, trash
}

// block creates the sender's contact (by ingesting one message from them
// into Archive, which a block never touches) and blocks it.
func (f *fixture) block(t *testing.T, from, dest string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	if _, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawFrom(from, "first contact", "seed-"+ulid.Make().String()+"@example.com"), f.prov(f.mailboxB, 9000+int64(len(from)), nil)); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	var cid ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		ids, _, err := tx.QueryContacts(ctx, store.ContactQueryFilter{Address: from}, 0, false)
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			t.Fatalf("contact for %s: %d, want 1", from, len(ids))
		}
		cid = ids[0]
		blocked := true
		_, err = tx.SetContactMarkers(ctx, cid, store.ContactMarkersPatch{Blocked: &blocked, BlockDestination: &dest})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return cid
}

func (f *fixture) ingestFrom(t *testing.T, from, msgID string, mailbox ulid.ULID, uid int64) Result {
	t.Helper()
	res, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		rawFrom(from, "Hello "+msgID, msgID), f.prov(mailbox, uid, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a new canonical message")
	}
	return res
}

// ⚠ THE HEADLINE: blocked → Junk, by the ordinary move, writeback included,
// still unread, one change row, and the rule that would have filed it never ran.
func TestBlockedSenderGoesToJunkOnArrival(t *testing.T) {
	f, junk, _ := blockFixture(t)
	ctx := context.Background()
	const from = "pest@spam.example"

	// A rule that WOULD label this sender's mail. A block decides first and
	// instead, so the label must never appear.
	var labelID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		if labelID, err = tx.CreateLabel(ctx, store.LabelRow{Name: "Pest-" + ulid.Make().String()}); err != nil {
			return err
		}
		_, err = tx.CreateRule(ctx, store.RuleRow{
			Name: "label-pest", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: from},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: labelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.block(t, from, store.BlockToJunk)

	res := f.ingestFrom(t, from, "blocked-1@spam.example", f.mailboxA, 11)

	if !res.Blocked {
		t.Error("Result.Blocked = false — the sync engine would push for a blocked sender")
	}
	if got := f.placement(t, res.MessageID); got != junk.String() {
		t.Fatalf("moved_mailbox_id = %q, want Junk %q", got, junk.String())
	}
	if f.inView(t, f.mailboxA, res.MessageID) {
		t.Error("the blocked message is in the INBOX desk query")
	}
	if !f.inView(t, junk, res.MessageID) {
		t.Error("the blocked message is not findable in Junk")
	}
	// THE ORDINARY MOVE: an IMAP move intent to the Junk role, like a hand move.
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents
		WHERE message_id=$1 AND op='move' AND payload->>'target_role'='junk' AND payload->>'mailbox'='Junk'`,
		res.MessageID.String()); got != 1 {
		t.Errorf("move-to-Junk writeback intents = %d, want 1 (a block writes back like a user move)", got)
	}
	// EXACTLY the move: no keyword intent rides along. A block is not a
	// verdict — it never adds $Junk (spec §6, #737), and a fresh arrival
	// carries no $NotJunk for the move-into-Junk hook to withdraw.
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1`, res.MessageID.String()); got != 1 {
		t.Errorf("writeback intents = %d, want exactly 1 (the move)", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1 AND payload->>'keyword' ILIKE '$junk'`,
		res.MessageID.String()); got != 0 {
		t.Error("a block enqueued a $Junk keyword — the junk verdict is the provider's")
	}
	if got := f.scalar(t, `SELECT is_junk::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 0 {
		t.Error("a block set the provider's junk verdict (is_junk)")
	}
	if got := f.scalar(t, `SELECT is_seen::int FROM app.messages WHERE id=$1`, res.MessageID.String()); got != 0 {
		t.Error("the blocked message was marked read")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		res.MessageID.String(), labelID.String()); got != 0 {
		t.Error("a rule labelled a blocked sender's message — the block must decide before rules")
	}
	// The change_log invariant: one `created` row, no `updated` row of the move's own.
	if got := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1 AND op='created'`,
		res.MessageID.String()); got != 1 {
		t.Errorf("Email created rows = %d, want 1", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1 AND op='updated'`,
		res.MessageID.String()); got != 0 {
		t.Errorf("Email updated rows = %d, want 0 — the move rides the created row", got)
	}
}

// A blocked sender's message that arrives carrying $NotJunk (another client
// rescued an earlier copy) goes to Junk the way any move into Junk does
// (#737): the $NotJunk is withdrawn BEFORE the move, and nothing is added.
func TestBlockedArrivalWithNotJunkWithdrawsItAndAddsNothing(t *testing.T) {
	f, _, _ := blockFixture(t)
	const from = "pest9@spam.example"
	f.block(t, from, store.BlockToJunk)
	res, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		rawFrom(from, "rescued once", "notjunk-1@spam.example"), f.prov(f.mailboxA, 31, []string{"$NotJunk"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1 AND op='-kw' AND payload->>'keyword'='$NotJunk'`,
		res.MessageID.String()); got != 1 {
		t.Errorf("-kw $NotJunk intents = %d, want 1", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1 AND op='+kw'`, res.MessageID.String()); got != 0 {
		t.Errorf("+kw intents = %d, want 0 — a block adds no keyword", got)
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1 AND op='move'`, res.MessageID.String()); got != 1 {
		t.Errorf("move intents = %d, want 1", got)
	}
}

// Trash, when chosen: trashed through the deletion lifecycle (purge clock set).
func TestBlockedSenderToTrashIsTrashed(t *testing.T) {
	f, _, trash := blockFixture(t)
	const from = "worse@spam.example"
	f.block(t, from, store.BlockToTrash)

	res := f.ingestFrom(t, from, "blocked-2@spam.example", f.mailboxA, 12)

	if !res.Blocked {
		t.Error("Result.Blocked = false")
	}
	if got := f.scalar(t, `SELECT (trashed_at IS NOT NULL AND purge_after IS NOT NULL)::int FROM app.messages WHERE id=$1`,
		res.MessageID.String()); got != 1 {
		t.Error("the message is not trashed with a purge clock — Trash must be the real Trash")
	}
	if got := f.placement(t, res.MessageID); got != trash.String() {
		t.Errorf("moved_mailbox_id = %q, want Trash %q", got, trash.String())
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1 AND op='move' AND payload->>'target_role'='trash'`,
		res.MessageID.String()); got != 1 {
		t.Errorf("move-to-Trash intents = %d, want 1", got)
	}
}

// CONTROL: an unblocked sender's message is untouched, and unblocking works.
func TestUnblockedSenderStaysInTheInbox(t *testing.T) {
	f, _, _ := blockFixture(t)
	ctx := context.Background()
	const from = "friend@ok.example"
	cid := f.block(t, from, store.BlockToJunk)
	unblocked := false
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.SetContactMarkers(ctx, cid, store.ContactMarkersPatch{Blocked: &unblocked})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	res := f.ingestFrom(t, from, "ok-1@ok.example", f.mailboxA, 13)
	if res.Blocked {
		t.Error("Result.Blocked = true for an unblocked sender")
	}
	if got := f.placement(t, res.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none", got)
	}
	if !f.inView(t, f.mailboxA, res.MessageID) {
		t.Error("an unblocked sender's message is not in the inbox")
	}
}

// CONTROL: blocking is by EXACT address — a different address at the same
// domain is not blocked.
func TestBlockIsByExactAddress(t *testing.T) {
	f, _, _ := blockFixture(t)
	f.block(t, "news@lists.example", store.BlockToJunk)

	res := f.ingestFrom(t, "billing@lists.example", "exact-1@lists.example", f.mailboxA, 14)
	if res.Blocked || f.placement(t, res.MessageID) != "" {
		t.Error("a different address at the blocked sender's domain was blocked")
	}
}

// A blocked sender's mail that did not arrive in the inbox stays where the
// provider put it — but it is still Blocked, so it never pushes.
func TestBlockedSenderOutsideTheInboxIsNotMovedButNotPushed(t *testing.T) {
	f, _, _ := blockFixture(t)
	const from = "pest2@spam.example"
	f.block(t, from, store.BlockToJunk)

	res := f.ingestFrom(t, from, "elsewhere-1@spam.example", f.mailboxB, 15)
	if got := f.placement(t, res.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none — a non-inbox arrival is left where it is", got)
	}
	if !res.Blocked {
		t.Error("Result.Blocked = false — a blocked sender's mail would notify")
	}
}

// FAIL-SOFT FOR REAL (#741 review): a blocked-sender lookup that FAILS inside
// the ingest transaction must leave the message ingested, in the inbox,
// unblocked — not abort the transaction (25P02) and fail the ingest. The
// failure is made without touching production code: the app role loses
// SELECT on app.contacts.block_destination, the one column only that lookup
// reads on the ingest path, for the length of the test.
func TestBlockedLookupFailureIsFailSoft(t *testing.T) {
	f, _, _ := blockFixture(t)
	ctx := context.Background()
	const from = "someone@ok.example"
	f.block(t, from, store.BlockToJunk) // blocked, so a working lookup WOULD move it
	cols := "id, user_id, display_name, vip, blocked, notes, created_at, blocked_at"
	for _, q := range []string{
		`REVOKE SELECT ON app.contacts FROM mailroom_app`,
		`GRANT SELECT (` + cols + `) ON app.contacts TO mailroom_app`,
	} {
		if _, err := f.super.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE SELECT (` + cols + `) ON app.contacts FROM mailroom_app`,
			`GRANT SELECT ON app.contacts TO mailroom_app`,
		} {
			if _, err := f.super.Exec(context.Background(), q); err != nil {
				t.Errorf("restore grants: %s: %v", q, err)
			}
		}
	})
	// The lookup really does fail now (so the test is about a failure).
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, _, e := tx.BlockedDestination(ctx, from)
		if e == nil {
			t.Fatal("the lookup did not fail — this test would prove nothing")
		}
		return nil
	}); err != nil {
		t.Errorf("a failed lookup poisoned its transaction: %v", err)
	}

	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawFrom(from, "Hello failsoft", "failsoft-1@ok.example"), f.prov(f.mailboxA, 21, nil))
	if err != nil {
		t.Fatalf("ingest failed when the block lookup failed (want fail-soft): %v", err)
	}
	if !res.Created || res.Blocked {
		t.Errorf("Result = %+v, want a new, unblocked message", res)
	}
	if got := f.placement(t, res.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none (treated as not blocked)", got)
	}
	if !f.inView(t, f.mailboxA, res.MessageID) {
		t.Error("the message is not in the inbox")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE object_type='Email' AND object_id=$1 AND op='created'`,
		res.MessageID.String()); got != 1 {
		t.Errorf("Email created rows = %d, want 1 (the ingest committed)", got)
	}
}

// An account with no Junk folder keeps the message in the inbox rather than
// hiding it somewhere the user cannot find it.
func TestBlockedSenderWithoutAJunkFolderStaysVisible(t *testing.T) {
	f := newFixture(t)
	f.markMailboxRoles(t) // INBOX + Archive only
	const from = "pest3@spam.example"
	f.block(t, from, store.BlockToJunk)

	res := f.ingestFrom(t, from, "nojunk-1@spam.example", f.mailboxA, 16)
	if got := f.placement(t, res.MessageID); got != "" {
		t.Errorf("moved_mailbox_id = %q, want none — there is no Junk to move to", got)
	}
	if !f.inView(t, f.mailboxA, res.MessageID) {
		t.Error("the message vanished from the inbox with nowhere to go")
	}
	if got := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE message_id=$1`, res.MessageID.String()); got != 0 {
		t.Errorf("writeback intents = %d, want 0 when nothing moved", got)
	}
}
