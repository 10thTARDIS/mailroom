//go:build integration

package api

import (
	"context"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// #581: Email/set must ASK for the writeback queue to be drained.
//
// The runner's NudgeWriteback is inert without this call — that is the whole
// point of testing it here rather than only in syncrunner: the feature is the
// wiring, and a runner nobody signals behaves exactly like the old code, with a
// user's action sitting in the queue until the next poll (measured: 3m51s at a
// 5-minute interval).
//
// PROBE: delete `a.nudgeWriteback(userID)` from emailSet and
// TestEmailSetAsksForAWritebackDrain fails with "nudges = 0".

type nudgeSpy struct {
	mu    sync.Mutex
	users []ulid.ULID
}

func (s *nudgeSpy) record(userID ulid.ULID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = append(s.users, userID)
}

func (s *nudgeSpy) seen() []ulid.ULID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ulid.ULID(nil), s.users...)
}

func TestEmailSetAsksForAWritebackDrain(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	spy := &nudgeSpy{}
	h.api.SetWritebackNudge(spy.record)

	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/" + store.KeywordSeen: true},
		},
	})

	seen := spy.seen()
	if len(seen) != 1 {
		t.Fatalf("nudges = %d, want 1: the intent this write queued would wait for the next poll", len(seen))
	}
	if seen[0] != a.UserID {
		t.Errorf("nudged user = %s, want %s — a drain for the wrong user leaves these intents queued",
			seen[0], a.UserID)
	}
}

// A nudge that fired for a FAILED write would dial IMAP to discover there is
// nothing to send. The whole method errors here (an unparseable patch), so
// nothing was committed and nothing should be asked for.
func TestEmailSetDoesNotNudgeWhenTheMethodFails(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	spy := &nudgeSpy{}
	h.api.SetWritebackNudge(spy.record)

	if _, mErr := h.api.emailSet(context.Background(), a.UserID, []byte(`{"update": 5}`)); mErr == nil {
		t.Fatal("expected the malformed arguments to be rejected; the rest of this test is vacuous without that")
	}
	if got := len(spy.seen()); got != 0 {
		t.Errorf("nudges = %d, want 0 after a failed Email/set", got)
	}
}

// Without a runner wired — a read-only deployment, or any test that does not
// install one — a write must still succeed. The intents then wait for the poll,
// which is exactly the behaviour every release before #581 had.
func TestEmailSetWithoutANudgeInstalledStillWrites(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/" + store.KeywordFlagged: true},
		},
	})
	if notUpdated, _ := res["notUpdated"].(map[string]any); len(notUpdated) != 0 {
		t.Fatalf("notUpdated = %v, want the write to succeed with no nudge installed", notUpdated)
	}
}
