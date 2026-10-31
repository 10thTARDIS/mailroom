// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func legacyEvents(actions []string, start time.Time) []store.FeedbackEvent {
	out := make([]store.FeedbackEvent, len(actions))
	for i, a := range actions {
		m := ulid.Make()
		out[i] = store.FeedbackEvent{ID: ulid.Make(), MessageID: &m, Action: a, CreatedAt: start.Add(time.Duration(i) * time.Minute)}
	}
	return out
}

// The reconstruction flags what promoted v1: a holdout (newest 20% of events)
// with no positive, where AUC is 0.5 on both sides by convention. Control: the
// same corpus with one positive moved into the tail is not degenerate.
func TestLegacyTrainingFlagsADegenerateHoldout(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	actions := make([]string, 100)
	for i := range actions {
		actions[i] = legacyMarkReadNoOpen
		if i%3 == 0 {
			actions[i] = "archive_unread"
		}
	}
	actions[5] = "flag" // the only positive, early: in the train split
	created := start.Add(200 * time.Minute)

	lt := legacyTraining(legacyEvents(actions, start), created)
	if lt.Samples != 100 || lt.TrainPositives != 1 || lt.HoldPositives != 0 || !lt.HoldoutDegenerate {
		t.Fatalf("reconstruction = %+v, want 100 samples, 1 train positive, degenerate holdout", lt)
	}
	if lt.MarkReadNoOpen != 65 {
		t.Errorf("mark_read_no_open = %d, want 65", lt.MarkReadNoOpen)
	}

	actions[95] = "flag"
	if lt := legacyTraining(legacyEvents(actions, start), created); lt.HoldoutDegenerate {
		t.Fatalf("control: a holdout holding a positive was flagged degenerate: %+v", lt)
	}
}

// Events at or after the snapshot are not what it trained on.
func TestLegacyTrainingStopsAtTheSnapshot(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ev := legacyEvents([]string{"flag", "trash", "flag", "trash"}, start)
	lt := legacyTraining(ev, start.Add(2*time.Minute))
	if lt.WindowEvents != 2 {
		t.Errorf("window events = %d, want the 2 before the snapshot", lt.WindowEvents)
	}
}
