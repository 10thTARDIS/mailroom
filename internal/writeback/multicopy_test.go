// SPDX-License-Identifier: AGPL-3.0-only

package writeback

// Unit coverage for #296: one writeback intent, MANY server copies.
//
// Intents are enqueued per ACCOUNT (store.liveAccountIDs selects DISTINCT
// account_id), but a message can live at several coordinates inside one account
// — re-delivered mail records a provenance row per delivery (#206), and a copy
// can sit in two folders. The engine used to resolve an account to a single copy
// (ORDER BY … LIMIT 1) and stop, while ingest merged the flags of EVERY live
// copy with OR. Adds therefore converged and nothing looked wrong; removals
// reverted on the very next poll, because the untouched copies still carried the
// flag and the merge put it back.
//
// These tests use the fakes in engine_test.go and need no database. The
// store-side proof that the resolver returns every live copy is in
// internal/store/writeback_integration_test.go; the end-to-end proof, real
// Postgres through the real StoreIntents adapter, is in
// writeback_integration_test.go alongside this file.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

// storeCalls returns the recorded UID STORE calls.
func (w *fakeWriter) storeCalls() []writerCall {
	var out []writerCall
	for _, c := range w.calls {
		if c.kind == "store" {
			out = append(out, c)
		}
	}
	return out
}

// callsOfKind returns the recorded calls of one kind.
func (w *fakeWriter) callsOfKind(kind string) []writerCall {
	var out []writerCall
	for _, c := range w.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// uidsOf returns, sorted, the UIDs a set of recorded calls targeted.
func uidsOf(calls []writerCall) []int {
	out := make([]int, len(calls))
	for i, c := range calls {
		out[i] = int(c.uid)
	}
	sort.Ints(out)
	return out
}

// ⚠ THE REGRESSION TEST FOR #296, engine half.
//
// Observed on a real instance before the fix: some messages had
// 2–4 live copies in one account, and several were in exactly the state where marking
// the message unread silently reverted on the next sync pass.
//
// THE SUBTEST AT copies=1 IS THE NEGATIVE CONTROL, and it is not decoration.
// Without it, "2 UID STORE calls" could be passing because two intents were
// claimed rather than because one intent reached two copies. Running the
// identical assertion against a message with ONE copy — and getting 1 — is what
// shows the number tracks copy multiplicity and not intent count.
func TestFlagRemovalReachesEveryLiveCopyInTheAccount(t *testing.T) {
	for _, copies := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("%d live copies", copies), func(t *testing.T) {
			s := newFakeStore()
			targets := make([]store.ProvenanceTarget, copies)
			wantUIDs := make([]int, copies)
			for i := range targets {
				targets[i] = liveTargetAt("INBOX", int64(100+i))
				wantUIDs[i] = 100 + i
			}
			in := seedIntentTargets(s, store.IntentRemoveFlag,
				mustPayload(t, intentPayload{Flag: `\Seen`}), targets...)
			w := &fakeWriter{}

			res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			// Exactly ONE intent, however many copies it reached: Applied counts
			// intents, and conflating the two is how this test could pass for the
			// wrong reason.
			if res.Applied != 1 {
				t.Fatalf("result = %+v, want exactly 1 intent applied", res)
			}
			got := w.storeCalls()
			if len(got) != copies {
				t.Fatalf("UID STORE calls = %d, want %d — one per live copy in the account", len(got), copies)
			}
			if !reflect.DeepEqual(uidsOf(got), wantUIDs) {
				t.Errorf("stored against UIDs %v, want %v", uidsOf(got), wantUIDs)
			}
			for _, c := range got {
				if len(c.remove) != 1 || c.remove[0] != imapsync.FlagSeen {
					t.Errorf("uid %d: remove = %v, want [%q]", c.uid, c.remove, imapsync.FlagSeen)
				}
				if len(c.add) != 0 {
					t.Errorf("uid %d: add = %v, want none — this is a REMOVAL", c.uid, c.add)
				}
			}
		})
	}
}

// One copy failing must not drop its siblings, and must not let the intent
// retire as applied. Both halves matter: stopping at the failure would leave the
// remaining copies with the user's mutation never mirrored, and marking the
// intent applied anyway would make that permanent (#296).
//
// ⚠ IT RUNS THE FAILURE AT EACH POSITION, and that is not thoroughness for its
// own sake — it is what makes the test able to fail. Probing this suite by
// changing the loop's `continue` to a `break` left it GREEN when only the SECOND
// copy failed, because by then the first had already been written and the call
// count was right anyway. Only a failure on the FIRST copy can observe an early
// exit. A test that cannot see the defect it names is documentation.
func TestOneFailingCopyStillReachesTheOthersAndRetriesTheIntent(t *testing.T) {
	for _, failingUID := range []imapsync.UID{100, 101} {
		t.Run(fmt.Sprintf("copy uid %d fails", failingUID), func(t *testing.T) {
			s := newFakeStore()
			in := seedIntentTargets(s, store.IntentRemoveFlag,
				mustPayload(t, intentPayload{Flag: `\Seen`}),
				liveTargetAt("INBOX", 100), liveTargetAt("INBOX", 101))

			w := &fakeWriter{failCoords: map[coord]error{
				{mailbox: "INBOX", uid: failingUID}: errors.New("mailbox locked by another session"),
			}}
			e := NewEngine(s, WithMaxAttempts(3))

			res, err := e.ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if n := len(w.storeCalls()); n != 2 {
				t.Fatalf("UID STORE calls = %d, want 2 — the healthy copy must still be attempted", n)
			}
			if got := uidsOf(w.storeCalls()); got[0] != 100 || got[1] != 101 {
				t.Errorf("stored against UIDs %v, want both 100 and 101", got)
			}
			if res.Applied != 0 || res.Retried != 1 {
				t.Fatalf("result = %+v, want 0 applied / 1 retried — a partly-mirrored delta has not been applied", res)
			}
			if got := s.intents[in.ID].State; got != store.IntentPending {
				t.Fatalf("intent state = %q, want pending (retry)", got)
			}
			// The recorded cause must report the BOUND, so an operator reading a
			// dead_reason can tell a total outage from one stuck copy.
			if last := s.intents[in.ID].LastError; !strings.Contains(last, "1 of 2 live copies") {
				t.Errorf("last_error = %q, want it to say how many of how many copies got through", last)
			}

			// The retry re-applies to BOTH copies, including the one that already
			// succeeded. The flag ops are idempotent, so that is a redundant
			// server-side no-op — the price of not needing per-target durable state.
			w.failCoords = nil
			res, err = e.ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatalf("apply (retry): %v", err)
			}
			if res.Applied != 1 {
				t.Fatalf("retry result = %+v, want the intent applied", res)
			}
			if n := len(w.storeCalls()); n != 4 {
				t.Fatalf("UID STORE calls across both passes = %d, want 4 (2 copies × 2 passes)", n)
			}
		})
	}
}

// An EXPUNGE reaches every live copy, and Result.Expunged counts only the ones
// the SERVER said it removed.
//
// This is the honesty half of #296. A UID EXPUNGE of a UID the server no longer
// holds returns success, so "the command did not error" proves nothing about
// whether mail was destroyed — the exact same-green failure the counter was
// added for in #268. Here the fake server holds one of the message's two copies:
// two expunges are issued, both succeed, and only ONE destruction may be
// claimed.
func TestExpungedCountsOnlyWhatTheServerActuallyRemoved(t *testing.T) {
	s := newFakeStore()
	held := liveTargetAt("INBOX", 100)
	alreadyGone := liveTargetAt("INBOX", 101)
	in := seedIntentTargets(s, store.IntentExpunge, mustPayload(t, intentPayload{}), held, alreadyGone)

	w := &fakeWriter{expungeHolds: map[coord]bool{{mailbox: "INBOX", uid: 100}: true}}

	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("result = %+v, want the intent applied", res)
	}
	if n := len(w.callsOfKind("expunge")); n != 2 {
		t.Fatalf("expunge calls = %d, want 2 — every live copy must be asked about", n)
	}
	if res.Expunged != 1 {
		t.Fatalf("Expunged = %d, want 1: the server removed one message and accepted the other request without removing anything", res.Expunged)
	}
}

// The HEADER-addressed expunge counts the same way. It has stronger evidence
// than the provenance path — a SEARCH found the UID moments ago — but "the
// SEARCH found it" and "the server removed it" are still two different claims,
// and keeping one definition of Expunged across both paths is what stops them
// drifting apart (#296). Applied is still 1: the engine did what it was asked;
// only the destruction tally is withheld.
func TestHeaderExpungeDoesNotCountAUIDTheServerNoLongerHolds(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: "vanished@example.com",
		}),
		IdempotencyKey: "expunge-uncounted", State: store.IntentPending,
	})
	// SEARCH says uid 17 is there; the server holds nothing.
	w := &fakeWriter{
		searchHits:   map[string][]imapsync.UID{"vanished@example.com": {17}},
		expungeHolds: map[coord]bool{},
	}

	res, err := NewEngine(s).ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("result = %+v, want the intent applied", res)
	}
	if n := len(w.callsOfKind("expunge")); n != 1 {
		t.Fatalf("expunge calls = %d, want 1", n)
	}
	if res.Expunged != 0 {
		t.Errorf("Expunged = %d, want 0 — the server accepted the command and removed nothing", res.Expunged)
	}
}

// A copy that fails does not stop the engine from destroying the ones it can,
// and the copies it DID destroy are still counted — Expunged is a claim about
// mail, not about whether the bookkeeping row reached 'applied' (#296).
func TestExpungeCountsDestroyedCopiesEvenWhenTheIntentFails(t *testing.T) {
	s := newFakeStore()
	in := seedIntentTargets(s, store.IntentExpunge, mustPayload(t, intentPayload{}),
		liveTargetAt("INBOX", 100), liveTargetAt("INBOX", 101))

	w := &fakeWriter{failCoords: map[coord]error{
		// The \Deleted STORE on the second copy fails, so its expunge never runs.
		{mailbox: "INBOX", uid: 101}: errors.New("connection reset"),
	}}

	res, err := NewEngine(s, WithMaxAttempts(3)).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 0 || res.Retried != 1 {
		t.Fatalf("result = %+v, want the intent retried", res)
	}
	if res.Expunged != 1 {
		t.Errorf("Expunged = %d, want 1 — the first copy really is gone regardless of the intent's fate", res.Expunged)
	}
}

// A message whose copies are ALL stale is a conflict, and — the part that
// matters — the engine writes to none of them. A stale UID belongs to a
// UIDVALIDITY generation the server discarded and may now name somebody else's
// message, so a loop whose job is to write to every element of the target set
// must never be handed one (#296).
func TestStaleCopiesAreNeverWrittenTo(t *testing.T) {
	s := newFakeStore()
	a := liveTargetAt("INBOX", 100)
	a.State = store.ProvenanceStaleUIDValidity
	b := liveTargetAt("INBOX", 101)
	b.State = store.ProvenanceStaleUIDValidity
	in := seedIntentTargets(s, store.IntentExpunge, mustPayload(t, intentPayload{}), a, b)

	w := &fakeWriter{}
	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Conflicts != 1 || res.DeadLettered != 1 {
		t.Fatalf("result = %+v, want 1 conflict / 1 dead-lettered", res)
	}
	if len(w.calls) != 0 {
		t.Fatalf("the server was touched at a stale coordinate: %v", w.kinds())
	}
	if res.Expunged != 0 {
		t.Errorf("Expunged = %d, want 0 — nothing may be destroyed at a stale coordinate", res.Expunged)
	}
}

// A single stale row mixed in with live ones conflicts the WHOLE intent rather
// than being quietly skipped. The store never returns such a mix — it partitions
// the set — but the engine is what stands between a future store bug and a
// destroyed message, so it re-checks every element instead of trusting the first
// (#296).
func TestAMixedTargetSetFailsClosed(t *testing.T) {
	s := newFakeStore()
	live := liveTargetAt("INBOX", 100)
	stale := liveTargetAt("INBOX", 101)
	stale.State = store.ProvenanceStaleUIDValidity
	in := seedIntentTargets(s, store.IntentExpunge, mustPayload(t, intentPayload{}), live, stale)

	w := &fakeWriter{}
	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Conflicts != 1 || res.DeadLettered != 1 {
		t.Fatalf("result = %+v, want the whole intent treated as a conflict", res)
	}
	if len(w.calls) != 0 {
		t.Fatalf("the server was touched despite a stale coordinate in the set: %v", w.kinds())
	}
}
