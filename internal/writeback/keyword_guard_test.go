// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"testing"

	"mailroom/internal/store"
)

// A keyword op never sends an IMAP system flag (#49 review, F1): `+kw \Draft`
// would be `UID STORE +FLAGS (\Draft)`, and the next sync would read the
// message back as a draft. It dead-letters without touching the server.
//
// RED PROBE: remove the KeywordWritable check in validateOp and the system-flag
// cases fail with a STORE sent and the intent applied.
func TestKeywordOpRefusesSystemFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   store.IntentOp
		kw   string
	}{
		{"add draft", store.IntentAddKeyword, `\Draft`},
		{"add seen lowercase", store.IntentAddKeyword, `\seen`},
		{"remove deleted", store.IntentRemoveKeyword, `\Deleted`},
		{"backslash inside", store.IntentAddKeyword, `My\Tag`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			tg := liveTarget()
			in := seedIntent(s, tc.op, mustPayload(t, intentPayload{Keyword: tc.kw}), &tg)
			w := &fakeWriter{}
			res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatal(err)
			}
			if len(w.calls) != 0 {
				t.Errorf("server written for keyword %q: %v", tc.kw, w.kinds())
			}
			if res.DeadLettered != 1 || s.intents[in.ID].State != store.IntentDead || s.intents[in.ID].DeadReason == "" {
				t.Errorf("res = %+v state = %q reason = %q, want dead-lettered with a reason",
					res, s.intents[in.ID].State, s.intents[in.ID].DeadReason)
			}
		})
	}
	// Positive control: an ordinary keyword is written.
	s := newFakeStore()
	tg := liveTarget()
	in := seedIntent(s, store.IntentAddKeyword, mustPayload(t, intentPayload{Keyword: "$NotJunk"}), &tg)
	w := &fakeWriter{}
	if res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w); err != nil || res.Applied != 1 || len(w.calls) != 1 {
		t.Fatalf("an ordinary keyword: res=%+v err=%v calls=%v, want applied with one STORE", res, err, w.kinds())
	}
}
