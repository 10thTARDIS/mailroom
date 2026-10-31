// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"errors"
	"testing"

	"mailroom/internal/store"
)

// A STORE the server acknowledged is recorded on EVERY copy it reached, with
// the delta it sent (#576) — the record is what stops a later MOVE in the same
// action leaving the source row's pre-STORE flags behind for the flag merge.
//
// NEGATIVE CONTROL: delete the RecordStoredFlags call in applyToEveryCopy and this goes red
// at the count.
func TestStoredFlagsAreRecordedPerCopy(t *testing.T) {
	cases := []struct {
		name        string
		op          store.IntentOp
		p           intentPayload
		add, remove []string
	}{
		{"add keyword", store.IntentAddKeyword, intentPayload{Keyword: "$NotJunk"}, []string{"$NotJunk"}, nil},
		{"remove keyword", store.IntentRemoveKeyword, intentPayload{Keyword: "$Junk"}, nil, []string{"$Junk"}},
		{"add flag", store.IntentAddFlag, intentPayload{Flag: `\Seen`}, []string{`\Seen`}, nil},
		{"remove flag", store.IntentRemoveFlag, intentPayload{Flag: `\Flagged`}, nil, []string{`\Flagged`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			a, b := liveTarget(), liveTarget()
			b.IMAPName, b.UID = "Junk", 7
			in := seedIntentTargets(s, tc.op, mustPayload(t, tc.p), a, b)
			if _, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, &fakeWriter{}); err != nil {
				t.Fatal(err)
			}
			if len(s.recorded) != 2 {
				t.Fatalf("recorded %d copies, want 2 (one per live copy): %+v", len(s.recorded), s.recorded)
			}
			for i, want := range []store.ProvenanceTarget{a, b} {
				r := s.recorded[i]
				if r.prov != want.ProvenanceID || !equalStrings(r.add, tc.add) || !equalStrings(r.remove, tc.remove) {
					t.Errorf("record %d = %+v, want prov %s add %v remove %v", i, r, want.ProvenanceID, tc.add, tc.remove)
				}
			}
		})
	}
}

// Only what the server ACKNOWLEDGED is recorded: a copy whose STORE failed
// keeps its old flags on record, because the server still has them — and a
// move or expunge records nothing at all, because it is not a flag change.
func TestStoredFlagsAreNotRecordedForFailuresOrOtherOps(t *testing.T) {
	s := newFakeStore()
	a, b := liveTarget(), liveTarget()
	b.IMAPName, b.UID = "Junk", 7
	in := seedIntentTargets(s, store.IntentRemoveKeyword, mustPayload(t, intentPayload{Keyword: "$Junk"}), a, b)
	w := &fakeWriter{failCoords: map[coord]error{{mailbox: "Junk", uid: 7}: errors.New("NO")}}
	if _, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w); err != nil {
		t.Fatal(err)
	}
	if len(s.recorded) != 1 || s.recorded[0].prov != a.ProvenanceID {
		t.Fatalf("recorded = %+v, want only the copy whose STORE succeeded (%s)", s.recorded, a.ProvenanceID)
	}

	s2 := newFakeStore()
	tg := liveTarget()
	mv := seedIntent(s2, store.IntentMove, mustPayload(t, intentPayload{Mailbox: "Archive"}), &tg)
	if _, err := NewEngine(s2).ApplyPending(context.Background(), mv.AccountID, &fakeWriter{supportsMove: true}); err != nil {
		t.Fatal(err)
	}
	if len(s2.recorded) != 0 {
		t.Errorf("a move recorded flags: %+v", s2.recorded)
	}
}

// A failure to RECORD is a failure of the intent: it goes back to pending
// rather than being marked applied over a stale record, which would leave the
// merge this exists to prevent in place.
//
// NEGATIVE CONTROL: drop the `continue` / failure append after RecordStoredFlags and the
// intent reads applied.
func TestStoredFlagsRecordFailureRetriesTheIntent(t *testing.T) {
	s := newFakeStore()
	s.recordErr = errors.New("db down")
	tg := liveTarget()
	in := seedIntent(s, store.IntentRemoveKeyword, mustPayload(t, intentPayload{Keyword: "$Junk"}), &tg)
	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, &fakeWriter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.intents[in.ID].State; got != store.IntentPending {
		t.Errorf("state = %q, want pending (retry)", got)
	}
	if res.Applied != 0 {
		t.Errorf("Applied = %d, want 0", res.Applied)
	}
}
