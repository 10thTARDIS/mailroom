// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// The API's feedback tokens MUST be exactly the actions ml/eval.go's
// LabelForAction maps — an unmapped token is silently dropped by the training/
// eval harness, so a drift here would quietly lose the signal. This locks the two
// together with the expected label SIGN for each.
func TestFeedbackTokensMatchLabelMap(t *testing.T) {
	cases := []struct {
		token        string
		wantPositive bool
	}{
		{feedbackFlag, true},
		{feedbackArchiveUnread, false},
		{feedbackTrash, false},
		{feedbackReply, true},
		// #842: the explicit open, and the store's own copies of the tokens it
		// writes for replies derived from any client and for Triage/open.
		{feedbackOpen, true},
		{store.FeedbackActionReply, true},
		{store.FeedbackActionOpen, true},
		// #34 Triage/correct tokens.
		{feedbackMarkImportant, true},
		{feedbackMarkNotImportant, false},
	}
	for _, c := range cases {
		label, ok := ml.LabelForAction(c.token)
		if !ok {
			t.Errorf("token %q is not in ml.LabelForAction — the training harness would drop it", c.token)
			continue
		}
		if (label >= 0.5) != c.wantPositive {
			t.Errorf("token %q label = %v, want positive=%v", c.token, label, c.wantPositive)
		}
	}
}

// feedbackTokenForEmailUpdate classifies a patch into one signal with a fixed
// precedence. These cases never take the move branch, so a nil tx is safe (the
// move branch is exercised by the store/api integration suites).
func TestFeedbackTokenForEmailUpdate(t *testing.T) {
	ctx := context.Background()
	msgID := ulid.Make()
	tt := []struct {
		name   string
		up     store.EmailUpdate
		want   string
		wantOK bool
	}{
		{"trash", store.EmailUpdate{Trash: true}, feedbackTrash, true},
		{"flag", store.EmailUpdate{Flagged: bp(true)}, feedbackFlag, true},
		// #842: a mark-read is no longer a negative. The web client marks read
		// on OPEN, so this case is "the user opened it"; the open signal is
		// Triage/open, after the dwell.
		{"mark-read emits nothing", store.EmailUpdate{Seen: bp(true)}, "", false},
		{"unflag emits nothing", store.EmailUpdate{Flagged: bp(false)}, "", false},
		{"mark-unread emits nothing", store.EmailUpdate{Seen: bp(false)}, "", false},
		{"restore emits nothing", store.EmailUpdate{Restore: true}, "", false},
		{"empty emits nothing", store.EmailUpdate{}, "", false},
		// Precedence: trash wins over a co-set flag/seen.
		{"trash precedence", store.EmailUpdate{Trash: true, Seen: bp(true), Flagged: bp(true)}, feedbackTrash, true},
	}
	for _, c := range tt {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := feedbackTokenForEmailUpdate(ctx, nil, msgID, c.up)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != c.wantOK || got != c.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func bp(b bool) *bool { return &b }

// The retired mark_read_no_open token must not be a label any more: a stale
// historical row of it is skipped by the trainer, never read as "did not
// matter" (#842).
func TestMarkReadNoOpenIsNotALabel(t *testing.T) {
	if _, ok := ml.LabelForAction("mark_read_no_open"); ok {
		t.Fatal("mark_read_no_open is still a training label")
	}
	if l, ok := ml.LegacyLabelForAction("mark_read_no_open"); !ok || l != 0 {
		t.Fatalf("the legacy map (report-only) must still read it as 0, got %v %v", l, ok)
	}
}

// The gate measures recall at the line the user sees. If the presentation
// threshold moves and the gate's does not, the gate measures the wrong harm.
func TestGateThresholdIsThePresentationThreshold(t *testing.T) {
	if ml.PriorityGateThreshold != ImportantThreshold {
		t.Fatalf("ml.PriorityGateThreshold = %v, api.ImportantThreshold = %v", ml.PriorityGateThreshold, ImportantThreshold)
	}
}
