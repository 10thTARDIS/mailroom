// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The change_log invariant on the paths #299 found it missing from.
//
// Every assertion here counts rows PER OBJECT TYPE, never a total. The whole
// class of defect being fixed is "a cascade destroyed four kinds of object and
// one row was appended", and a single count cannot tell that apart from a
// correct one.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// changeRowsSince counts change_log rows appended for one object type since a
// recorded sequence, read as the SUPERUSER so RLS cannot be the reason a missing
// row looks missing.
func changeRowsSince(t *testing.T, ctx context.Context, h *harness, userID string, typ ObjectType, since int64) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = $2 AND seq > $3`,
		userID, string(typ), since).Scan(&n); err != nil {
		t.Fatalf("count %s change rows: %v", typ, err)
	}
	return n
}

func stateOf(t *testing.T, ctx context.Context, s *Store, userID ulid.ULID, typ ObjectType) int64 {
	t.Helper()
	var seq int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		seq, e = tx.ChangeState(ctx, typ)
		return e
	}); err != nil {
		t.Fatalf("read %s state: %v", typ, err)
	}
	return seq
}

// TestDeleteAccountEmitsRowsForWhatTheCascadeDestroys is #299 item 3's
// reproduction, made into an assertion.
//
// The audit's repro: "create an account + 2 mailboxes + an identity,
// DeleteAccount, assert 3 destroyed rows for Mailbox/Identity. Returns 0." The
// existing coverage asserted only the Account row, so a cascade that silently
// destroyed three more JMAP-visible types was green.
func TestDeleteAccountEmitsRowsForWhatTheCascadeDestroys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// The fixture already gives the account one mailbox (INBOX) and one identity.
	// Add a second mailbox, a snippet bound to that identity, and a key bound to
	// it — the two SECOND-HOP casualties (compose_snippets.identity_id CASCADE,
	// user_keys.identity_id SET NULL) that are easiest to miss.
	seedMailbox(t, ctx, h, a, "Archive", `\Archive`)
	snippetID := NewULID().String()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.compose_snippets (id, user_id, kind, identity_id, name, body_text)
		VALUES ($1, $2, 'signature', $3, $4, 'bound signature')`,
		snippetID, a.ID, a.IdentityID, "bound-"+a.Tag); err != nil {
		t.Fatalf("seed bound snippet: %v", err)
	}
	keyID := NewULID().String()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.user_keys (id, user_id, identity_id, kind, fingerprint, public_key)
		VALUES ($1, $2, $3, 'pgp', $4, $5)`,
		keyID, a.ID, a.IdentityID, "fp-bound-"+a.Tag, []byte{0x04}); err != nil {
		t.Fatalf("seed bound key: %v", err)
	}

	// CONTROLS ON THE FIXTURE. Each assertion below is "N rows for N objects", so
	// if any of these sets were empty the corresponding assertion would pass
	// vacuously — the exact shape of failure this issue is about.
	countBy := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	wantMailboxes := countBy(
		`SELECT count(*) FROM app.mailboxes WHERE user_id = $1 AND account_id = $2`, a.ID, a.AccountID)
	wantIdentities := countBy(
		`SELECT count(*) FROM app.identities WHERE user_id = $1 AND account_id = $2`, a.ID, a.AccountID)
	if wantMailboxes < 2 || wantIdentities < 1 {
		t.Fatalf("fixture has %d mailboxes and %d identities on the account; the assertions "+
			"below would not be testing a fan-out", wantMailboxes, wantIdentities)
	}

	before := map[ObjectType]int64{}
	for _, typ := range []ObjectType{ObjectAccount, ObjectMailbox, ObjectIdentity, ObjectComposeSnippet, ObjectKey} {
		before[typ] = stateOf(t, ctx, s, aID, typ)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		found, _, e := tx.DeleteAccount(ctx, mustULID(t, a.AccountID))
		if e != nil {
			return e
		}
		if !found {
			t.Fatal("DeleteAccount: found=false")
		}
		return nil
	}); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	for _, c := range []struct {
		typ  ObjectType
		want int
		why  string
	}{
		{ObjectAccount, 1, "the account itself"},
		{ObjectMailbox, wantMailboxes,
			"every folder the cascade destroyed. Without these a second client polls " +
				"Mailbox/changes, is told nothing changed, and keeps offering the deleted " +
				"account's folders forever"},
		{ObjectIdentity, wantIdentities,
			"every send-as identity the cascade destroyed. A stale one is worse than a stale " +
				"folder: choosing it composes mail that cannot be sent"},
		{ObjectComposeSnippet, 1,
			"the signature bound to a destroyed identity (compose_snippets.identity_id CASCADE)"},
		{ObjectKey, 1,
			"the key whose identity binding was cleared (user_keys.identity_id SET NULL). The " +
				"key SURVIVES — BindKeyToIdentity made that binding JMAP-visible Key state, so " +
				"unbinding owes the same row"},
	} {
		if got := changeRowsSince(t, ctx, h, a.ID, c.typ, before[c.typ]); got != c.want {
			t.Errorf("DeleteAccount appended %d %s change rows, want %d — %s",
				got, c.typ, c.want, c.why)
		}
	}
}

// TestHardDeleteMessageEmitsTheThreadRow is #299 item 3's purge half.
func TestHardDeleteMessageEmitsTheThreadRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// The fixture message has live provenance, which HardDeleteMessage refuses
	// (propagate-first, canonical-last). Drop it, then trash it into the past.
	if _, err := h.super.Exec(ctx,
		`DELETE FROM app.message_provenance WHERE user_id = $1 AND message_id = $2`,
		a.ID, a.MessageID); err != nil {
		t.Fatalf("clear provenance: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		UPDATE app.messages SET trashed_at = now() - interval '40 days',
		                        purge_after = now() - interval '10 days'
		 WHERE user_id = $1 AND id = $2`, a.ID, a.MessageID); err != nil {
		t.Fatalf("trash the fixture message: %v", err)
	}
	var threadID string
	if err := h.super.QueryRow(ctx,
		`SELECT thread_id FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.ID, a.MessageID).Scan(&threadID); err != nil {
		t.Fatalf("read thread id: %v", err)
	}

	beforeEmail := stateOf(t, ctx, s, aID, ObjectEmail)
	beforeThread := stateOf(t, ctx, s, aID, ObjectThread)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, _, ok, e := tx.HardDeleteMessage(ctx, mustULID(t, a.MessageID), time.Now().UTC())
		if e != nil {
			return e
		}
		if !ok {
			t.Fatal("HardDeleteMessage refused an eligible message; nothing below is exercised")
		}
		return nil
	}); err != nil {
		t.Fatalf("hard delete: %v", err)
	}

	if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, beforeEmail); got != 1 {
		t.Errorf("purge appended %d Email rows, want exactly 1 (`destroyed`)", got)
	}
	if got := changeRowsSince(t, ctx, h, a.ID, ObjectThread, beforeThread); got != 1 {
		t.Errorf("purge appended %d Thread rows, want exactly 1. A Thread's JMAP identity IS "+
			"its list of emailIds, so removing a message from one mutates a JMAP-visible "+
			"object; CollapseDuplicateGroup already emits Thread rows for exactly this and "+
			"purge did not, so a client holding the conversation kept rendering a message "+
			"that no longer exists (#299)", got)
	}
	// It must be `updated`, not `destroyed`: purge does not delete app.threads
	// rows, so claiming the Thread was destroyed is a lie the client can check.
	var op string
	if err := h.super.QueryRow(ctx,
		`SELECT op::text FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Thread' AND seq > $2
		  ORDER BY seq LIMIT 1`, a.ID, beforeThread).Scan(&op); err != nil {
		t.Fatalf("read the thread row: %v", err)
	}
	if op != string(OpUpdated) {
		t.Errorf("the Thread row is %q, want %q — the thread row still exists, so anything "+
			"else is a claim a client can falsify", op, OpUpdated)
	}
	var threadStillThere int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.threads WHERE user_id = $1 AND id = $2`,
		a.ID, threadID).Scan(&threadStillThere); err != nil {
		t.Fatal(err)
	}
	if threadStillThere != 1 {
		t.Errorf("purge deleted the thread row (%d left). It must not: that would be a new "+
			"delete, and mail safety says a purge widens nothing", threadStillThere)
	}
}

// TestRescoringAppendsAnEmailRow is #299 item 4. The exception triage.go claimed
// stopped being true when priorityScore/topFeatures/listId/listUnsubscribe were
// added to the JMAP Email object, and `mailroom-admin rescore` was changing them
// on every live message with nothing telling any client.
func TestRescoringAppendsAnEmailRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	// The fixture already carries a triage_scores row for this message, so the
	// first Upsert below is genuinely a RE-score. Confirm rather than assume.
	var scores int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.triage_scores WHERE user_id = $1 AND message_id = $2`,
		a.ID, a.MessageID).Scan(&scores); err != nil {
		t.Fatal(err)
	}
	if scores != 1 {
		t.Fatalf("fixture has %d triage_scores rows; the re-score path is not being exercised", scores)
	}

	t.Run("a changed score appends exactly one row", func(t *testing.T) {
		before := stateOf(t, ctx, s, aID, ObjectEmail)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			return tx.UpsertTriageScore(ctx, msgID, 0.91, "v1", []byte(`[{"f":"x"}]`))
		}); err != nil {
			t.Fatalf("rescore: %v", err)
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 1 {
			t.Errorf("a re-score appended %d Email rows, want 1. Email.priorityScore and "+
				"Email.topFeatures are served on the JMAP object, so clients hold a stale "+
				"priority until something unrelated happens to touch the message (#299)", got)
		}
	})

	t.Run("an identical re-score appends nothing", func(t *testing.T) {
		// Re-running `rescore` produces byte-identical scores for most messages.
		// Telling every client that N Emails changed when none did is its own
		// kind of lie, and it would also make the assertion above meaningless.
		before := stateOf(t, ctx, s, aID, ObjectEmail)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			return tx.UpsertTriageScore(ctx, msgID, 0.91, "v1", []byte(`[{"f":"x"}]`))
		}); err != nil {
			t.Fatalf("rescore: %v", err)
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 0 {
			t.Errorf("an identical re-score appended %d Email rows, want 0", got)
		}
	})

	t.Run("a first score appends nothing (it rides the Email created row)", func(t *testing.T) {
		// Ingest writes the first score in the same transaction as the message, so
		// its values travel on the Email `created` row — and RFC 8620 §5.2 says an
		// object created and updated in one window belongs in `created` alone.
		drafts := seedMailbox(t, ctx, h, a, "Drafts2", `\Drafts`)
		var fresh ulid.ULID
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			fresh, _ = makeDraft(t, ctx, tx, a, drafts, "never scored "+NewULID().String())
			return nil
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		before := stateOf(t, ctx, s, aID, ObjectEmail)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			return tx.UpsertTriageScore(ctx, fresh, 0.4, "v1", nil)
		}); err != nil {
			t.Fatalf("first score: %v", err)
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 0 {
			t.Errorf("the FIRST score of a message appended %d Email rows, want 0 — ingest's "+
				"hot path would pay a redundant row on every message received", got)
		}
	})

	t.Run("changing the bulk signals appends a row", func(t *testing.T) {
		// list_id and list_unsubscribe ARE JMAP-visible (Email.listId,
		// Email.listUnsubscribe under #58's unsubscribe surface), whatever the old
		// comment said.
		before := stateOf(t, ctx, s, aID, ObjectEmail)
		var changed bool
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			changed, e = tx.UpdateMessageSignals(ctx, msgID, "<list.example.com>",
				"<mailto:unsub@example.com>", 3, false)
			return e
		}); err != nil {
			t.Fatalf("update signals: %v", err)
		}
		if !changed {
			t.Fatal("UpdateMessageSignals reported no change; the assertion below is vacuous")
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 1 {
			t.Errorf("backfilling list_id/list_unsubscribe appended %d Email rows, want 1", got)
		}
		// And a repeat is a genuine no-op, in the log as well as in the table.
		before = stateOf(t, ctx, s, aID, ObjectEmail)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			_, e := tx.UpdateMessageSignals(ctx, msgID, "<list.example.com>",
				"<mailto:unsub@example.com>", 3, false)
			return e
		}); err != nil {
			t.Fatalf("update signals (repeat): %v", err)
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 0 {
			t.Errorf("a repeat backfill appended %d Email rows, want 0", got)
		}
	})
}

// TestClearingSuggestionsCountsAsAnEmailChange is #299 item 5's first bullet.
//
// The precondition is v1.34.1's documented "it does not sweep" limit: a rule
// labels a message after a suggestion already exists, the user then accepts that
// same label, the INSERT conflicts and adds nothing, and with no other field in
// the patch `changed` stays false — so ApplyEmailUpdate took its early return
// with the suggestions already deleted and no Email row appended.
func TestClearingSuggestionsCountsAsAnEmailChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	// The fixture's label is ALREADY on the message and a suggestion for it is
	// ALREADY open — exactly the state the limit above produces.
	var labelID string
	if err := h.super.QueryRow(ctx,
		`SELECT label_id FROM app.message_labels WHERE user_id = $1 AND message_id = $2 LIMIT 1`,
		a.ID, a.MessageID).Scan(&labelID); err != nil {
		t.Fatalf("read the fixture label: %v", err)
	}
	var open int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.label_suggestions WHERE user_id = $1 AND message_id = $2`,
		a.ID, a.MessageID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open == 0 {
		t.Fatal("the fixture has no open suggestion; there would be nothing for the accept " +
			"to clear and this test would assert nothing")
	}

	before := stateOf(t, ctx, s, aID, ObjectEmail)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// The ZERO LabelSource means "the user did this by hand" — which is the
		// only actor whose answer closes the question (#169).
		found, _, e := tx.ApplyEmailUpdate(ctx, msgID, EmailUpdate{
			AddLabelIDs: []ulid.ULID{mustULID(t, labelID)},
		})
		if e != nil {
			return e
		}
		if !found {
			t.Fatal("ApplyEmailUpdate: found=false")
		}
		return nil
	}); err != nil {
		t.Fatalf("accept the label: %v", err)
	}

	// CONTROL: the suggestions really were cleared. Without this the row count
	// below could be right for the wrong reason.
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.label_suggestions WHERE user_id = $1 AND message_id = $2`,
		a.ID, a.MessageID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("the accept left %d suggestions open; the precondition never happened", open)
	}
	if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 1 {
		t.Errorf("accepting a label that was ALREADY on the message cleared its suggestions "+
			"and appended %d Email rows, want 1. Suggestions are served as properties of the "+
			"Email, so every OTHER client keeps the answered question in its review queue "+
			"forever (#299)", got)
	}
}

// TestSettingADefaultSnippetEmitsTheSiblingsRow is #299 item 5's third bullet.
// Making one snippet the default silently un-defaults another, and only one of
// the two objects was getting a row.
func TestSettingADefaultSnippetEmitsTheSiblingsRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	var first, second ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		first, e = tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetSignature, Name: "first-" + NewULID().String(),
			BodyText: "one", IsDefault: true,
		})
		return e
	}); err != nil {
		t.Fatalf("create first: %v", err)
	}

	before := stateOf(t, ctx, s, aID, ObjectComposeSnippet)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		second, e = tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetSignature, Name: "second-" + NewULID().String(),
			BodyText: "two", IsDefault: true,
		})
		return e
	}); err != nil {
		t.Fatalf("create second: %v", err)
	}

	// CONTROL: the first really did lose the flag, so there IS a second object
	// whose row we are demanding.
	var stillDefault bool
	if err := h.super.QueryRow(ctx,
		`SELECT is_default FROM app.compose_snippets WHERE user_id = $1 AND id = $2`,
		a.ID, first.String()).Scan(&stillDefault); err != nil {
		t.Fatal(err)
	}
	if stillDefault {
		t.Fatalf("snippet %s is still the default; the swap never happened", first)
	}

	if got := changeRowsSince(t, ctx, h, a.ID, ObjectComposeSnippet, before); got != 2 {
		t.Errorf("making %s the default appended %d ComposeSnippet rows, want 2 (the new "+
			"default AND the sibling that stopped being one). is_default is what the compose "+
			"picker preselects, so a client that missed the demotion keeps OFFERING the old "+
			"signature (#299)", second, got)
	}
}

// TestRepairSentDraftAppendsARowOnTheSecondRun is #311 item 2.
//
// ⚠ IT TAKES TWO RUNS TO SHOW, which is why review missed it. Run 1 clears the
// $draft flag (ClearDraftFlag appends the row) and queues the expunge; if that
// expunge dead-letters, the next sync re-discovers the Drafts copy and restores
// its provenance — documented, intended behaviour. Run 2 finds the message
// stranded again, drops that restored provenance (a `mailboxIds` change), and
// ClearDraftFlag no-ops because is_draft is already false. Net effect of run 2
// before this fix: a JMAP-visible mutation with zero Email rows.
func TestRepairSentDraftAppendsARowOnTheSecondRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := mustULID(t, a.AccountID)
	drafts := seedMailbox(t, ctx, h, a, "Drafts", `\Drafts`)

	// A message in exactly the shape StrandedSentDraftIDs selects: sent, but with
	// live provenance in a Drafts-role mailbox, and (run 2's premise) is_draft
	// already false.
	strand := func(t *testing.T, isDraft bool) ulid.ULID {
		t.Helper()
		var id ulid.ULID
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			id, _ = makeDraft(t, ctx, tx, a, drafts, "stranded "+NewULID().String())
			return nil
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := h.super.Exec(ctx, `
			UPDATE app.messages SET is_draft = $3 WHERE user_id = $1 AND id = $2`,
			a.ID, id.String(), isDraft); err != nil {
			t.Fatalf("set is_draft: %v", err)
		}
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
			        mailbox_id, uid, uidvalidity, state)
			VALUES ($1, $2, $3, $4, $5, $6, 1, 'live')`,
			NewULID().String(), a.ID, id.String(), a.AccountID, drafts.String(),
			testUID()); err != nil {
			t.Fatalf("seed drafts provenance: %v", err)
		}
		return id
	}

	t.Run("run 1: the flag flips and that row covers it", func(t *testing.T) {
		id := strand(t, true)
		before := stateOf(t, ctx, s, aID, ObjectEmail)
		var (
			queued  int
			cleared bool
		)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			queued, cleared, e = tx.RepairSentDraft(ctx, id, acct)
			return e
		}); err != nil {
			t.Fatalf("repair: %v", err)
		}
		if queued == 0 || !cleared {
			t.Fatalf("run 1 queued=%d cleared=%v; the fixture is not the shape the repair "+
				"walks, so run 2 below proves nothing", queued, cleared)
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 1 {
			t.Errorf("run 1 appended %d Email rows, want exactly 1 — two would mean the flag "+
				"row and the provenance row are both being emitted for one mutation", got)
		}
	})

	t.Run("run 2: the flag is already clear and provenance still goes", func(t *testing.T) {
		id := strand(t, false) // the post-run-1, post-resync state
		before := stateOf(t, ctx, s, aID, ObjectEmail)
		var (
			queued  int
			cleared bool
		)
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			queued, cleared, e = tx.RepairSentDraft(ctx, id, acct)
			return e
		}); err != nil {
			t.Fatalf("repair: %v", err)
		}
		if queued == 0 {
			t.Fatal("run 2 queued no expunge, so no provenance was dropped and there is " +
				"nothing for the assertion below to be about")
		}
		if cleared {
			t.Fatal("run 2 cleared the draft flag; the message was seeded with is_draft " +
				"already false, so this is not the second-run case")
		}
		// CONTROL: the provenance really is gone — the mutation happened.
		var live int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.message_provenance
			  WHERE user_id = $1 AND message_id = $2 AND state = 'live'`,
			a.ID, id.String()).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 0 {
			t.Fatalf("%d live provenance rows survive; Email.mailboxIds did not change and "+
				"no row is owed", live)
		}
		if got := changeRowsSince(t, ctx, h, a.ID, ObjectEmail, before); got != 1 {
			t.Errorf("run 2 dropped live Drafts provenance — a mailboxIds change — and "+
				"appended %d Email rows, want 1. With zero, every client keeps the message "+
				"in Drafts and /changes never says otherwise (#311)", got)
		}
	})
}
