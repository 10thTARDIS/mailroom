// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package writeback

// Ingest-only mode (#521, ADR-028): an account can be synced while Mailroom is
// structurally incapable of writing to it.
//
// THE ASSERTION THAT MATTERS IS NEGATIVE, which is the hard kind to get right.
// "No IMAP write happened" is also what you observe when the test never got far
// enough to write anything — three assertions about a sandbox once passed
// against a page that could not have loaded at all (#131), and a probe harness
// once scored a compile error as the mechanism working. So every negative here
// is paired with a POSITIVE CONTROL: the identical scenario with the mode off,
// asserting the writes DO land. A negative that cannot be made positive is not
// evidence.
//
// The three properties, which are genuinely different and each independently
// breakable:
//
//  1. Nothing is queued in a drainable state (the enqueue guard).
//  2. Nothing drains, and no CONNECTION is even opened (the structural guard —
//     this is the real control, ADR-028 §3).
//  3. Lifting the mode does not retroactively drain the ledger (§4).
//
// Runs against the throwaway Postgres from scripts/integration-test.sh; skips
// when the env is absent. Reuses liveFixture from writeback_integration_test.go.
//
// # Probe record for the instance-flag and APPEND additions (PR #553, S6)
//
//	mechanism deleted                                   | red test, and where
//	----------------------------------------------------|--------------------
//	store/writeback.go EnqueueIntent: `AND NOT           | TestTheInstanceFlagAlone
//	  $9::boolean` (the instance term in the ledger      |   SuppressesAtEnqueue, at
//	  decision)                                          |   "state = pending, want
//	                                                     |   suppressed" x7; ...StopsThe
//	                                                     |   Drain stayed green
//	runner.go DrainAccount: `&& !r.store.Writeback        | TestTheInstanceFlagAlone
//	  Disabled()`                                        |   StopsTheDrain, at the
//	                                                     |   exploder-dial assertion;
//	                                                     |   ...SuppressesAtEnqueue green
//	the `append` case in enqueueEveryMutationClass       | TestIngestOnlySuppressesEvery
//	  (a FIXTURE probe, to show the new coverage runs)   |   MutationClass, at
//	                                                     |   `ledger["append"] = 0, want 1`
//
// # Probe record for the drain-side self-heal (round 5)
//
//	mechanism deleted                                   | red test, and where
//	----------------------------------------------------|--------------------
//	runner.go DrainAccount: the `if pending > 0`         | TestAPendingRowOnAShadowed
//	  SuppressAccountIntents call inside the             |   AccountIsMovedIntoTheLedger,
//	  `!writeback` branch                                |   at "states after a shadowed
//	                                                     |   drain = map[pending:1]";
//	                                                     |   ...OnALiveAccountIsNever
//	                                                     |   MovedIntoTheLedger green

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

// --- helpers -----------------------------------------------------------------

// setWriteback flips the account's per-account switch as the superuser, the
// same thing Account/set does through the API.
func (f *liveFixture) setWriteback(t *testing.T, enabled bool) {
	t.Helper()
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.accounts SET writeback_enabled = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), f.account.String(), enabled); err != nil {
		t.Fatalf("set writeback_enabled=%v: %v", enabled, err)
	}
}

// countByState reports how many of the account's intents are in each state,
// read as the superuser so RLS cannot mask a row.
//
// ⚠ PER STATE, NOT A TOTAL. "3 intents exist" cannot distinguish a ledger from
// a live queue, and the entire difference between this mode working and this
// mode being a silent no-op is which state the rows are in.
func (f *liveFixture) countByState(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.super.Query(context.Background(),
		`SELECT state::text, count(*) FROM app.writeback_intents
		  WHERE user_id = $1 AND account_id = $2 GROUP BY state`,
		f.userID.String(), f.account.String())
	if err != nil {
		t.Fatalf("count by state: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatalf("scan state count: %v", err)
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count by state: %v", err)
	}
	return out
}

// exploderDial is a Dialer that fails the test if it is ever called.
//
// ⚠ THIS IS THE POINT OF THE STRUCTURAL ASSERTION, and it is strictly stronger
// than #521 asked for. The issue wanted "zero IMAP mutations reached the fake
// server". A recording writer that stays empty proves only that nothing was
// written; it cannot tell that apart from a pass that connected, authenticated
// and then found nothing to do. Failing on DIAL proves the capability was never
// obtained at all — which is what ADR-028 §3 actually claims.
func exploderDial(t *testing.T) Dialer {
	t.Helper()
	return func(_ context.Context, _ imapsync.Config) (imapsync.SyncSession, error) {
		t.Errorf("writeback dialed IMAP for an ingest-only account: " +
			"DrainAccount must return before r.dial (ADR-028 §3)")
		return nil, errors.New("dial must not happen")
	}
}

// enqueueEveryMutationClass records one intent of every op the writeback engine
// knows about, which is what #521's acceptance requires be covered.
//
// The ops are the closed set from app.writeback_intents.op: '+flag', '-flag',
// '+kw', '-kw', 'move', 'append', 'expunge'.
//
// ⚠ 'append' WAS MISSING FROM THIS LIST and the ADR's Verification section
// nevertheless claimed draft save and draft destroy were covered. They were
// not: APPEND is the op a draft save produces (compose.record ->
// EnqueueAppendIntent) and the one the Sent copy uses, so the single most
// user-visible write the mode has to withhold was the one class nothing here
// enqueued. It is the last op in the enum and the easiest to leave out, which
// is exactly why "every mutation class" has to be a list somebody checks
// against app.writeback_intents.op rather than a phrase in a doc comment.
func (f *liveFixture) enqueueEveryMutationClass(t *testing.T, keyPrefix string) []ulid.ULID {
	t.Helper()
	msg := f.seedMessage(t, "every mutation class "+keyPrefix)
	f.seedLiveCopy(t, msg, f.inbox, 1)

	cases := []struct {
		op      store.IntentOp
		payload string
	}{
		{store.IntentAddFlag, `{"flag":"\\Seen"}`},
		{store.IntentRemoveFlag, `{"flag":"\\Flagged"}`},
		{store.IntentAddKeyword, `{"keyword":"$label1"}`},
		{store.IntentRemoveKeyword, `{"keyword":"$label2"}`},
		{store.IntentMove, `{"mailbox":"Trash"}`},
		// The real appendPayload shape (store/drafts.go), not a placeholder: a
		// payload the engine could not parse would make a future drain-side
		// assertion fail for a reason that has nothing to do with this mode.
		{store.IntentAppend, `{"mailbox":"Drafts","flags":["\\Draft"],` +
			`"message_id_header":"<append-` + keyPrefix + `@example.com>",` +
			`"content_hash":"00112233445566778899aabbccddeeff"}`},
		{store.IntentExpunge, `{}`},
	}
	ids := make([]ulid.ULID, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, f.enqueue(t, msg, c.op, c.payload, keyPrefix+"-"+string(c.op)))
	}
	return ids
}

// --- 1. the enqueue guard ----------------------------------------------------

// TestIngestOnlySuppressesEveryMutationClass is the acceptance test #521 asks
// for: with the mode on, every mutation class is recorded as a LEDGER entry and
// none of them is drainable.
//
// The negative control is TestWritebackEnabledStillQueuesEveryMutationClass
// below, which runs the identical enqueue and asserts the opposite.
func TestIngestOnlySuppressesEveryMutationClass(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, false)

	ids := f.enqueueEveryMutationClass(t, "shadow")

	// Every one of them must be terminal-suppressed, individually — a count
	// alone would pass if one op silently failed to enqueue at all.
	for _, id := range ids {
		if got := f.intentState(t, id); got != "suppressed" {
			t.Errorf("intent %s: state = %q, want \"suppressed\"", id, got)
		}
	}

	byState := f.countByState(t)
	if n := byState["pending"] + byState["inflight"]; n != 0 {
		t.Errorf("drainable intents = %d, want 0 (states: %v)", n, byState)
	}
	if got, want := byState["suppressed"], len(ids); got != want {
		t.Errorf("suppressed intents = %d, want %d (states: %v)", got, want, byState)
	}

	// The ledger must be readable as a REPORT, per op class — one total cannot
	// distinguish "nothing happened" from "the thing I counted is always empty".
	var byOp map[string]int64
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		var err error
		byOp, err = tx.SuppressedIntentsByOp(context.Background(), f.account)
		return err
	}); err != nil {
		t.Fatalf("SuppressedIntentsByOp: %v", err)
	}
	for _, op := range []string{"+flag", "-flag", "+kw", "-kw", "move", "append", "expunge"} {
		if byOp[op] != 1 {
			t.Errorf("ledger[%q] = %d, want 1 (full ledger: %v)", op, byOp[op], byOp)
		}
	}
}

// TestWritebackEnabledStillQueuesEveryMutationClass is the NEGATIVE CONTROL for
// the test above, and #521 requires it: with the mode off, the identical
// enqueues must all become drainable work.
//
// Without this, every assertion above would still pass if EnqueueIntent had
// simply stopped inserting rows, or if the fixture's ops were malformed.
func TestWritebackEnabledStillQueuesEveryMutationClass(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true) // explicit, not relying on the column default

	ids := f.enqueueEveryMutationClass(t, "live")

	for _, id := range ids {
		if got := f.intentState(t, id); got != "pending" {
			t.Errorf("intent %s: state = %q, want \"pending\"", id, got)
		}
	}
	byState := f.countByState(t)
	if got, want := byState["pending"], len(ids); got != want {
		t.Errorf("pending intents = %d, want %d (states: %v)", got, want, byState)
	}
	if byState["suppressed"] != 0 {
		t.Errorf("suppressed = %d, want 0 with writeback enabled (states: %v)",
			byState["suppressed"], byState)
	}
}

// --- 2. the structural guard (THE control) -----------------------------------

// TestIngestOnlyNeverDials proves ADR-028 §3's actual claim: for a shadowed
// account the runner returns before r.dial, so no IMAP connection is opened and
// no IMAPWriter is ever constructed.
//
// It deliberately seeds the queue with PENDING rows written directly, bypassing
// EnqueueIntent's suppression. That models the one case the enqueue guard
// cannot cover — intents that already existed when the mode was switched on —
// and it means this test fails if the ONLY thing stopping the drain is that
// suppressed rows are not counted as pending.
func TestIngestOnlyNeverDials(t *testing.T) {
	f := newLiveFixture(t)
	msg := f.seedMessage(t, "queued before the switch")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "pre-existing")

	// Queued while live, so it is genuinely pending...
	if got := f.countByState(t)["pending"]; got != 1 {
		t.Fatalf("setup: pending = %d, want 1 — the rest of this test proves nothing", got)
	}
	// ...and only THEN is the account shadowed, without suppressing the backlog.
	f.setWriteback(t, false)

	r := NewRunner(f.st, exploderDial(t), nil, nil)
	res, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{})
	if err != nil {
		t.Fatalf("DrainAccount: %v", err)
	}
	if res.Applied != 0 || res.DeadLettered != 0 || res.Retried != 0 {
		t.Errorf("shadowed drain did work: %+v, want a zero Result", res)
	}
	// The intent reached the LEDGER, not the mailbox: not applied, not dead, not
	// attempted — moved to 'suppressed' by the drain's self-heal (§4).
	//
	// ⚠ THIS ASSERTION USED TO READ `pending == 1, want 1 (untouched)`, and the
	// change is deliberate rather than accommodating. "Untouched" conflated two
	// different goods: that nothing reached the server (still asserted, by the
	// exploder and the zero Result) and that the ROW was not moved — and leaving
	// it pending is what let it drain unreviewed the moment the mode was lifted.
	// A row pending on a shadowed account is the hole, not the baseline.
	if got := f.countByState(t); got["suppressed"] != 1 || got["pending"] != 0 {
		t.Errorf("states after shadowed drain = %v, want {suppressed:1} — the row belongs "+
			"in the ledger, where an explicit --apply is needed to act on it", got)
	}
	var attempts int
	if err := f.super.QueryRow(context.Background(),
		`SELECT max(attempts) FROM app.writeback_intents WHERE user_id = $1 AND account_id = $2`,
		f.userID.String(), f.account.String()).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 — a shadowed account must not burn the retry budget", attempts)
	}
}

// TestWritebackEnabledDoesDial is the NEGATIVE CONTROL for the structural
// guard: the same runner, the same queue, writeback on — the dialer MUST be
// called.
//
// ⚠ Without this, TestIngestOnlyNeverDials would pass just as happily if
// DrainAccount had been broken into returning early for everyone, which is a
// far worse bug than the one it is guarding against and would look identical.
func TestWritebackEnabledDoesDial(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true)
	msg := f.seedMessage(t, "drains normally")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "live-drain")

	dialed := false
	dial := func(_ context.Context, _ imapsync.Config) (imapsync.SyncSession, error) {
		dialed = true
		// Failing the dial is enough: we are asserting that the attempt is made,
		// and a fake SyncSession is a much larger surface to stand up here.
		return nil, errors.New("dial attempted, as required")
	}
	r := NewRunner(f.st, dial, nil, nil)
	if _, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err == nil {
		t.Fatal("DrainAccount succeeded with a failing dialer, want the dial error")
	}
	if !dialed {
		t.Error("writeback did not dial for an account with writeback ENABLED — " +
			"the guard is refusing everyone, not just shadowed accounts")
	}
}

// --- 2a. the INSTANCE flag, on its own ---------------------------------------

// The two tests below cover the half of ADR-028 §2 that nothing else in this
// file touches: `MAILROOM_WRITEBACK_DISABLED` with the account's OWN column
// still TRUE.
//
// Effective writeback is `column AND NOT instanceFlag`, and the two checks that
// implement the mode spell that conjunction out SEPARATELY — `EnqueueIntent`'s
// `AND NOT $9::boolean` and `DrainAccount`'s `acct.WritebackEnabled &&
// !r.store.WritebackDisabled()`. Every other test in this file drives the
// column, so deleting the instance-flag term from EITHER expression left the
// whole suite green. The instance flag is the catch that survives a misclick in
// the settings UI during the cutover window, so each term gets its own test.
//
// ⚠ THEY ARE TWO TESTS, NOT TWO HALVES OF ONE, and that is a probe finding
// rather than taste. Written as one test with a shared fixture, deleting the
// enqueue term left seven stray `pending` rows in the account, which then blew
// up the SECOND half's setup assertion (`pending = 8, want 1`) before it could
// exercise the drain at all. The test was still red, so nothing was hidden —
// but one term's failure was masking the other term's coverage, and a fixture
// whose second act only runs when its first act passes is not two guarantees.

// TestTheInstanceFlagAloneSuppressesAtEnqueue: the ledger term. A CLI or worker
// that loads MAILROOM_WRITEBACK_DISABLED must enqueue 'suppressed' on an
// account whose own column says true — otherwise it queues drainable work on an
// account the server considers shadowed, and it fires at cutover.
func TestTheInstanceFlagAloneSuppressesAtEnqueue(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true) // ⚠ the per-account column stays TRUE throughout
	f.st.SetWritebackDisabled(true)

	ids := f.enqueueEveryMutationClass(t, "instance")
	for _, id := range ids {
		if got := f.intentState(t, id); got != "suppressed" {
			t.Errorf("intent %s: state = %q, want \"suppressed\" — the instance flag did not "+
				"reach EnqueueIntent, so a CLI or worker that loaded it would still queue "+
				"drainable work on an account the server considers shadowed", id, got)
		}
	}
	byState := f.countByState(t)
	if n := byState["pending"] + byState["inflight"]; n != 0 {
		t.Errorf("drainable intents = %d, want 0 (states: %v)", n, byState)
	}
	if got := byState["suppressed"]; got != len(ids) {
		t.Errorf("suppressed = %d, want %d (states: %v)", got, len(ids), byState)
	}

	// NEGATIVE CONTROL: the column is unchanged, so clearing ONLY the instance
	// flag must make the identical enqueue drainable again. Without it, this test
	// passes just as well against a build that suppresses everything.
	f.st.SetWritebackDisabled(false)
	id := f.enqueue(t, f.seedMessage(t, "after the flag is cleared"),
		store.IntentAddFlag, `{"flag":"\\Seen"}`, "instance-cleared")
	if got := f.intentState(t, id); got != "pending" {
		t.Errorf("state with the instance flag CLEARED = %q, want \"pending\" — the suppression "+
			"above was not the flag", got)
	}
}

// TestTheInstanceFlagAloneStopsTheDrain: the structural term, which is THE
// control (ADR-028 §3) — DrainAccount must return before r.dial, so no IMAP
// connection is opened and no writer is constructed.
//
// The queue is seeded the only way that can tell the guard apart from the
// incidental `pending == 0` return: the intent is enqueued while the instance
// flag is CLEAR, so it is genuinely pending, and only then is the flag set.
// That is also the real case — an instance restarted with the flag in its
// compose file, holding a queue from before.
func TestTheInstanceFlagAloneStopsTheDrain(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true) // the per-account column stays TRUE

	msg := f.seedMessage(t, "queued before the instance flag")
	f.seedLiveCopy(t, msg, f.inbox, 2)
	f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "pre-instance-flag")
	if got := f.countByState(t)["pending"]; got != 1 {
		t.Fatalf("setup: pending = %d, want 1 — without a drainable intent DrainAccount would "+
			"return at the pending==0 check and the exploder below would prove nothing", got)
	}

	f.st.SetWritebackDisabled(true)
	r := NewRunner(f.st, exploderDial(t), nil, nil)
	res, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{})
	if err != nil {
		t.Fatalf("DrainAccount: %v", err)
	}
	if res.Applied != 0 || res.DeadLettered != 0 || res.Retried != 0 {
		t.Errorf("instance-flagged drain did work: %+v, want a zero Result", res)
	}
	if got := f.countByState(t); got["suppressed"] != 1 || got["pending"] != 0 {
		t.Errorf("states after the drain = %v, want {suppressed:1} — see the note in "+
			"TestIngestOnlyNeverDials on why this is not 'pending, untouched'", got)
	}

	// NEGATIVE CONTROL: clear the flag and the identical call MUST dial. Without
	// it, the "no dial" above is indistinguishable from a fixture whose queue was
	// never drainable in the first place.
	//
	// ⚠ IT RE-ARMS WITH A FRESH INTENT, AND THAT IS THE WHOLE POINT OF THIS EDIT.
	// This control used to clear the flag and drain the SAME queue, asserting
	// that the shadow-era row now dialed — which means it was asserting that a
	// backlog accumulated under the flag fires on lift with no --apply. That is
	// the hazard ADR-028 §4 exists to forbid and #554 item 7 records, so the
	// control was pinning the defect in place: the drain self-heal that fixes it
	// turned this assertion red, correctly. The fixture is re-armed instead of
	// the rule being relaxed.
	f.st.SetWritebackDisabled(false)
	msg2 := f.seedMessage(t, "queued after the instance flag was cleared")
	f.seedLiveCopy(t, msg2, f.inbox, 3)
	f.enqueue(t, msg2, store.IntentAddFlag, `{"flag":"\\Seen"}`, "post-instance-flag")
	if got := f.countByState(t)["pending"]; got != 1 {
		t.Fatalf("control setup: pending = %d, want 1 — a live account must accept a fresh "+
			"intent as drainable, or the dial assertion below proves nothing", got)
	}

	dialed := false
	r = NewRunner(f.st, func(_ context.Context, _ imapsync.Config) (imapsync.SyncSession, error) {
		dialed = true
		return nil, errors.New("dial attempted, as required")
	}, nil, nil)
	if _, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err == nil {
		t.Fatal("DrainAccount succeeded with a failing dialer, want the dial error")
	}
	if !dialed {
		t.Error("the same account did not dial for a freshly queued intent once the instance " +
			"flag was cleared — the no-dial assertion above was about the fixture, not the flag")
	}
}

// --- 3. suppressed is terminal ------------------------------------------------

// TestSuppressedIsNeverClaimed pins the property the whole ledger rests on: a
// suppressed intent is invisible to the claim query, including to the
// 15-minute stranded-inflight reclaim arm added for #520.
//
// The reclaim arm is the specific hazard. It widened the claim predicate from
// `state='pending'` to also match aged inflight rows; a future widening that
// forgot this mode would drain the ledger silently, on a timer, with no user
// action at all.
func TestSuppressedIsNeverClaimed(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, false)
	msg := f.seedMessage(t, "suppressed stays put")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	id := f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "terminal")

	// Age it well past the lease AND give it a claimed_at, so a predicate that
	// keys on staleness rather than on state would pick it up.
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.writeback_intents
		    SET created_at = now() - interval '3 hours',
		        claimed_at = now() - interval '3 hours'
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), id.String()); err != nil {
		t.Fatalf("age intent: %v", err)
	}

	var claimed int
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		got, err := tx.ClaimPendingIntents(context.Background(), f.account, 100)
		claimed = len(got)
		return err
	}); err != nil {
		t.Fatalf("ClaimPendingIntents: %v", err)
	}
	if claimed != 0 {
		t.Errorf("claimed %d suppressed intent(s), want 0 — the ledger is drainable", claimed)
	}
	if got := f.intentState(t, id); got != "suppressed" {
		t.Errorf("state after a claim attempt = %q, want \"suppressed\"", got)
	}
}

// TestLiftingTheModeDrainsNothing is ADR-028 §4's central promise, and the one
// most likely to be "simplified" away by someone who finds it surprising.
//
// A fortnight of queued moves must not fire because a flag was flipped. Only
// the explicit replay may do that — which the second half asserts, so this is
// not merely proving that nothing works.
func TestLiftingTheModeDrainsNothing(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, false)
	ids := f.enqueueEveryMutationClass(t, "fortnight")
	if got := f.countByState(t)["suppressed"]; got != len(ids) {
		t.Fatalf("setup: suppressed = %d, want %d", got, len(ids))
	}

	// Lift the mode. Nothing else.
	f.setWriteback(t, true)

	byState := f.countByState(t)
	if n := byState["pending"] + byState["inflight"]; n != 0 {
		t.Errorf("lifting the mode made %d intent(s) drainable, want 0 (states: %v)", n, byState)
	}
	if got := byState["suppressed"]; got != len(ids) {
		t.Errorf("suppressed after lift = %d, want %d — the ledger was consumed by the lift", got, len(ids))
	}

	// The explicit replay IS allowed to, and must actually work — otherwise the
	// assertion above is satisfied by a ledger nothing can ever act on.
	var replayed int64
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		var err error
		replayed, err = tx.ReplaySuppressedIntents(context.Background(), f.account)
		return err
	}); err != nil {
		t.Fatalf("ReplaySuppressedIntents: %v", err)
	}
	if replayed != int64(len(ids)) {
		t.Errorf("replayed = %d, want %d", replayed, len(ids))
	}
	if got := f.countByState(t)["pending"]; got != len(ids) {
		t.Errorf("pending after explicit replay = %d, want %d", got, len(ids))
	}
}

// TestSwitchingTheModeOnSuppressesTheBacklog covers the transition edge that an
// enqueue-only guard cannot: intents that were already pending when the mode
// was switched on.
//
// #521 question 2 names this exactly — "an enqueue-only guard means anything
// queued before the mode was switched on fires the instant it is lifted".
func TestSwitchingTheModeOnSuppressesTheBacklog(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true)
	ids := f.enqueueEveryMutationClass(t, "backlog")
	if got := f.countByState(t)["pending"]; got != len(ids) {
		t.Fatalf("setup: pending = %d, want %d", got, len(ids))
	}

	f.setWriteback(t, false)
	var moved int64
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		var err error
		moved, err = tx.SuppressAccountIntents(context.Background(), f.account)
		return err
	}); err != nil {
		t.Fatalf("SuppressAccountIntents: %v", err)
	}
	if moved != int64(len(ids)) {
		t.Errorf("suppressed %d, want %d", moved, len(ids))
	}
	byState := f.countByState(t)
	if n := byState["pending"] + byState["inflight"]; n != 0 {
		t.Errorf("drainable after suppressing the backlog = %d, want 0 (states: %v)", n, byState)
	}
}

// TestRepeatingAMutationAfterLiftUpgradesTheLedgerRow guards a hole the ledger
// design creates: a suppressed row holds the idempotency key, so once the mode
// is lifted an enqueue of that same key would hit ON CONFLICT, and a plain DO
// NOTHING would silently swallow it — the change never reaches IMAP. That is a
// new silent divergence manufactured by the mechanism built to prevent one, so
// the enqueue path upgrades a suppressed row to pending instead.
//
// ⚠ WHAT THIS TEST DOES AND DOES NOT MODEL. It drives EnqueueIntent with a
// FIXED key, which is the arm's contract but is NOT what a user repeating a
// mutation through Email/set produces: ApplyEmailUpdate mints a fresh action id
// per call and folds it into every key, so that path inserts a new row and
// never conflicts. The callers that genuinely reach this arm are the
// deterministic-key ones — the purge expunge (`purge-expunge:<msg>:<acct>`) and
// the draft header-expunge and APPEND in drafts.go.
//
// The test is kept, because the arm is real and must not regress. The claim
// that it protects a *user* repeating a mutation was wrong, and saying so here
// is cheaper than someone re-deriving it from the key construction.
func TestRepeatingAMutationAfterLiftUpgradesTheLedgerRow(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, false)
	msg := f.seedMessage(t, "same key twice")
	f.seedLiveCopy(t, msg, f.inbox, 1)

	const key = "same-logical-mutation"
	id := f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, key)
	if got := f.intentState(t, id); got != "suppressed" {
		t.Fatalf("setup: state = %q, want \"suppressed\"", got)
	}

	f.setWriteback(t, true)
	again := f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, key)

	if again != id {
		t.Errorf("re-enqueue produced a new row %s (was %s) — the idempotency key stopped deduping", again, id)
	}
	if got := f.intentState(t, id); got != "pending" {
		t.Errorf("state after re-requesting the mutation while live = %q, want \"pending\": "+
			"the ledger row swallowed a real user change", got)
	}
}

// TestRepeatingAMutationWhileStillShadowedStaysSuppressed is the counterpart:
// the upgrade arm must key on writeback being LIVE, not merely on the row being
// suppressed, or the ledger would promote itself back into a live queue.
func TestRepeatingAMutationWhileStillShadowedStaysSuppressed(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, false)
	msg := f.seedMessage(t, "still shadowed")
	f.seedLiveCopy(t, msg, f.inbox, 1)

	const key = "repeated-while-shadowed"
	id := f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, key)
	_ = f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, key)

	if got := f.intentState(t, id); got != "suppressed" {
		t.Errorf("state = %q, want \"suppressed\" — re-enqueueing while shadowed made it drainable", got)
	}
}

// --- 4. the related open question #521 asked to settle -------------------------

// TestSyncScopeOffIsStillAWritebackTarget pins the contract ADR-028 §8 records:
// sync_scope='off' means "stop READING this folder", not "stop writing to it".
//
// #521 asked for this to be settled and noted no test covered it. It is
// INTENDED behaviour — sync_scope is a read-cost control, and the reaper
// explicitly reasons about a message moved into an unsynced folder
// (internal/store/reaper.go:613) — so the test pins it rather than changing it.
//
// ⚠ Pinning it matters more than it looks. The obvious "tidy-up" is to make
// sync_scope gate writeback too, on the grounds that writing to a folder you
// never read is odd. That would silently break the reaper's conservative case.
// This test is the tripwire for that refactor.
func TestSyncScopeOffIsStillAWritebackTarget(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true)

	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.mailboxes SET sync_scope = 'off' WHERE user_id = $1 AND id = $2`,
		f.userID.String(), f.inbox.String()); err != nil {
		t.Fatalf("set sync_scope=off: %v", err)
	}

	msg := f.seedMessage(t, "lives in an unsynced folder")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	id := f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "scope-off")

	// It enqueues as real, drainable work...
	if got := f.intentState(t, id); got != "pending" {
		t.Fatalf("intent state = %q, want \"pending\" — sync_scope now gates enqueue", got)
	}

	// ...and the engine resolves it to a live target in that folder and writes.
	w := &fakeWriter{}
	res := f.drain(t, w)
	if res.Applied != 1 {
		t.Errorf("applied = %d, want 1 (result %+v)", res.Applied, res)
	}
	if len(w.calls) != 1 {
		t.Fatalf("writer calls = %d, want 1 — an 'off' mailbox stopped being a writeback target (%+v)",
			len(w.calls), w.calls)
	}
	if w.calls[0].kind != "store" || w.calls[0].mailbox != "INBOX" {
		t.Errorf("writer call = %s on %q, want store on \"INBOX\"", w.calls[0].kind, w.calls[0].mailbox)
	}
}

// --- 4. the drain heals what the switch-on transaction could not see ---------

// TestAPendingRowOnAShadowedAccountIsMovedIntoTheLedger is the regression test
// for a defect a fifth review reproduced: §4's "lifting the mode drains
// nothing" was a property of ONE transactional edge rather than an invariant.
//
// ⚠ THE SHAPE, and why it is not exotic. `Account/set` suppresses the backlog in
// the same transaction as the flag change. But `EnqueueIntent` decides
// pending-vs-suppressed from a subselect on app.accounts at its own statement
// snapshot, and only afterwards does its FK check take KEY SHARE on the account
// row — which conflicts with `UpdateAccount`'s FOR UPDATE. So an enqueue that
// started before the switch committed blocks mid-statement, is invisible to
// `SuppressAccountIntents` in that same transaction (it reports 0 rows), and
// then lands **'pending' on an account that is already shadowed**. The original
// probe carried a pg_stat_activity witness that the interleaving really
// happened, and the row went on to APPLY on the first drain after the mode was
// lifted — no `--apply`, and invisible to `writeback-ledger`, which counts only
// 'suppressed'.
//
// Booting with MAILROOM_WRITEBACK_DISABLED while rows are already pending
// reaches the identical state by a duller route: nothing calls `Account/set`, so
// nothing suppresses them (#554 item 7).
//
// This test does NOT reproduce the race — a lock interleaving is not a thing to
// pin in a regression test, and the ledger row is what matters, not how it got
// there. It seeds the STATE the race produces, by enqueueing while live and then
// shadowing the account directly, which is exactly what bypassing `Account/set`
// does. The precondition assertion below is load-bearing: without it a fixture
// that suppressed at enqueue would make the whole test vacuous.
func TestAPendingRowOnAShadowedAccountIsMovedIntoTheLedger(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true)
	msg := f.seedMessage(t, "queued live, then the account was shadowed")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "pending-then-shadowed")

	// Shadow the account WITHOUT going through Account/set, which is what both
	// the race and the boot case amount to.
	f.setWriteback(t, false)

	if got := f.countByState(t); got["pending"] != 1 {
		t.Fatalf("precondition: states = %v, want one 'pending' row. The scenario "+
			"under test is a pending row on a shadowed account; if the fixture did "+
			"not produce one, this test proves nothing.", got)
	}

	r := NewRunner(f.st, exploderDial(t), nil, nil)
	if _, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err != nil {
		t.Fatalf("DrainAccount: %v", err)
	}

	got := f.countByState(t)
	if got["pending"] != 0 || got["suppressed"] != 1 {
		t.Errorf("states after a shadowed drain = %v, want {suppressed:1}. A row left "+
			"'pending' here drains unreviewed the moment the mode is lifted, and the "+
			"ledger report never shows it because it counts only 'suppressed'.", got)
	}
}

// TestAPendingRowOnALiveAccountIsNeverMovedIntoTheLedger is the control for the
// test above, and it is the one that catches the obvious way to get that test
// passing: suppressing pending rows somewhere that also runs for a LIVE account.
// That would quietly convert every ordinary queued mutation into a ledger entry
// needing a manual --apply — writeback would stop working, silently, for
// everyone.
//
// The dialer fails on purpose. This asserts the row's STATE, not the drain's
// success: a live account's pending row must still be 'pending' after a drain
// attempt, ready for the next pass.
func TestAPendingRowOnALiveAccountIsNeverMovedIntoTheLedger(t *testing.T) {
	f := newLiveFixture(t)
	f.setWriteback(t, true)
	msg := f.seedMessage(t, "queued live, account stays live")
	f.seedLiveCopy(t, msg, f.inbox, 1)
	f.enqueue(t, msg, store.IntentAddFlag, `{"flag":"\\Seen"}`, "pending-stays-pending")

	dial := func(_ context.Context, _ imapsync.Config) (imapsync.SyncSession, error) {
		return nil, errors.New("dial attempted, as required")
	}
	r := NewRunner(f.st, dial, nil, nil)
	if _, err := r.DrainAccount(context.Background(), f.userID, f.account, imapsync.Config{}); err == nil {
		t.Fatal("DrainAccount succeeded with a failing dialer, want the dial error")
	}

	got := f.countByState(t)
	if got["pending"] != 1 || got["suppressed"] != 0 {
		t.Errorf("states after a LIVE drain attempt = %v, want {pending:1}. Suppressing "+
			"here would turn every ordinary queued mutation into a ledger entry that "+
			"needs a manual --apply.", got)
	}
}
