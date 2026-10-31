// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Which writer wins when both speak about the same (message, label) — #242.
//
// app.label_suggestions had exactly ONE writer from its creation until #242:
// the per-label classifier, through ingest's classifyMessage. An unconditional
// upsert was therefore correct by default, because nothing else could ever
// collide with it. #242 adds the rule inducer as a second writer on the same
// primary key, and at ingest the collision is ORDERED — applyRules runs before
// classifyMessage (internal/ingest/ingest.go) — so "last write wins" silently
// resolves to "the model always wins".
//
// That is the wrong way round, and the reasons are measured rather than
// aesthetic. The rule side has zero collateral across 27 candidates and 66 rules
// (#168/#173); the model side sits at 0.20–0.60 precision at the apply threshold,
// which is why #169 declines to promote one at all (#220). The rule carries an
// explanation the user can read and a condition they can correct through
// Rule/tighten; the model carries feature weights and can only be retrained.
//
// ⚠ THIS SUITE EXISTS BECAUSE THE BUG IS CURRENTLY UNREACHABLE. No model is
// promoted on any real instance, so nothing today can produce the losing
// interleaving — which means no other test, and no amount of running the product,
// would notice if the precedence were wrong or if it silently regressed later
// when #169's data floor is finally met. The collision is constructed here
// directly, at the store, for exactly that reason.
//
// All four orderings are asserted, not just the one that motivated the change: a
// test that only pinned "model cannot clobber rule" would pass just as happily
// against an upsert that had stopped updating ANYTHING, which would break both
// sides refreshing their own answer.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// ruleCond is the condition the rule-sourced suggestions in this file are
// derived from. Its rendering is what proves a rule row survived: the reasons
// payload is the visible difference between the two writers.
var ruleCond = rules.Condition{All: []rules.Condition{
	{Field: rules.FieldAlias, Op: rules.OpEquals, Value: "sub@example.com"},
}}

func TestModelSuggestionCannotOverwriteARuleSuggestion(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "prec-"+NewULID().String())
	userID := mustULID(t, u.ID)
	msgID := mustULID(t, u.MessageID)
	labelID := seededSuggestionLabel(t, h, u)
	unfile(t, h, u)

	// The fixture already seeded a MODEL row for this (message, label) — see
	// seedUser. Starting from it is deliberate: the rule must be able to take over
	// from a model row, which is the backfill's real situation.
	writeRule := func() {
		t.Helper()
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			return tx.PutRuleSuggestion(ctx, msgID, labelID, ruleCond, 4)
		}); err != nil {
			t.Fatalf("put rule suggestion: %v", err)
		}
	}
	writeModel := func(confidence float64) {
		t.Helper()
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			return tx.PutLabelSuggestion(ctx, LabelSuggestion{
				MessageID:    msgID,
				LabelID:      labelID,
				Confidence:   confidence,
				ModelVersion: 7,
				Reasons:      []byte(`[{"key":"unsub:1","weight":1.5}]`),
				Source:       SuggestionSourceModel,
			})
		}); err != nil {
			t.Fatalf("put model suggestion: %v", err)
		}
	}
	read := func() LabelSuggestion {
		t.Helper()
		var got LabelSuggestion
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			byMsg, e := tx.SuggestionsForMessages(ctx, []ulid.ULID{msgID})
			if e != nil {
				return e
			}
			for _, s := range byMsg[msgID] {
				if s.LabelID == labelID {
					got = s
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.LabelID != labelID {
			t.Fatalf("no suggestion row for the seeded label; the upsert wrote nothing at all")
		}
		return got
	}

	// A RULE TAKES OVER FROM A MODEL. The backfill reaching mail the classifier had
	// already spoken for must replace it, not be dropped.
	writeRule()
	afterRule := read()
	if afterRule.Source != SuggestionSourceRule {
		t.Fatalf("after the rule wrote, source = %q, want %q — a rule could not take over a model row",
			afterRule.Source, SuggestionSourceRule)
	}
	wantConfidence := rules.EvidenceConfidence(4)
	if afterRule.Confidence != wantConfidence {
		t.Errorf("after the rule wrote, confidence = %v, want %v (the evidence rank)",
			afterRule.Confidence, wantConfidence)
	}

	// THE LOAD-BEARING ONE. A model row arriving over a rule row must change
	// nothing — not the source, not the rank, not the explanation.
	writeModel(0.97)
	afterModel := read()
	if afterModel.Source != SuggestionSourceRule {
		t.Errorf("source = %q, want %q — the model overwrote the rule's suggestion",
			afterModel.Source, SuggestionSourceRule)
	}
	if afterModel.Confidence != wantConfidence {
		t.Errorf("confidence = %v, want %v — the model replaced the rule's evidence rank "+
			"with its own probability, which the client would then render as a percentage",
			afterModel.Confidence, wantConfidence)
	}
	if afterModel.ModelVersion != 0 {
		t.Errorf("model_version = %d, want 0 — a model stamped its version onto a rule-sourced row",
			afterModel.ModelVersion)
	}
	// The explanation is the part a user actually reads, so it gets its own
	// assertion rather than being assumed to travel with the source column.
	if string(afterModel.Reasons) != string(afterRule.Reasons) {
		t.Errorf("reasons changed under the model write:\n got %s\nwant %s",
			afterModel.Reasons, afterRule.Reasons)
	}
}

func TestSuggestionRefreshWorksWithinEachSource(t *testing.T) {
	// NEGATIVE CONTROL for the test above. Blocking model-over-rule must not have
	// been implemented by blocking updates generally — an upsert that had quietly
	// stopped updating anything would satisfy every assertion in
	// TestModelSuggestionCannotOverwriteARuleSuggestion while breaking both writers'
	// ability to revise their own answer. Each source refreshing itself is asserted
	// here so that failure mode cannot hide.
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "refr-"+NewULID().String())
	userID := mustULID(t, u.ID)
	msgID := mustULID(t, u.MessageID)
	labelID := seededSuggestionLabel(t, h, u)
	unfile(t, h, u)

	read := func() LabelSuggestion {
		t.Helper()
		var got LabelSuggestion
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			byMsg, e := tx.SuggestionsForMessages(ctx, []ulid.ULID{msgID})
			if e != nil {
				return e
			}
			for _, s := range byMsg[msgID] {
				if s.LabelID == labelID {
					got = s
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		return got
	}

	// MODEL OVER MODEL. The fixture's seeded row is a model row at 0.5.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.PutLabelSuggestion(ctx, LabelSuggestion{
			MessageID: msgID, LabelID: labelID, Confidence: 0.81, ModelVersion: 9,
			Source: SuggestionSourceModel,
		})
	}); err != nil {
		t.Fatalf("model refresh: %v", err)
	}
	if got := read(); got.Confidence != 0.81 {
		t.Errorf("model-over-model confidence = %v, want 0.81 — a model can no longer revise its own answer",
			got.Confidence)
	}

	// RULE OVER RULE. Two different evidence counts, so the rank must actually move.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.PutRuleSuggestion(ctx, msgID, labelID, ruleCond, 2)
	}); err != nil {
		t.Fatalf("first rule write: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.PutRuleSuggestion(ctx, msgID, labelID, ruleCond, 9)
	}); err != nil {
		t.Fatalf("rule refresh: %v", err)
	}
	want := rules.EvidenceConfidence(9)
	if got := read(); got.Confidence != want {
		t.Errorf("rule-over-rule confidence = %v, want %v — a re-run of the backfill with more "+
			"evidence can no longer revise its own rank", got.Confidence, want)
	}
}

// unfile removes the label seedUser attaches to its fixture message, so
// PutRuleSuggestion will actually write.
//
// ⚠ REQUIRED SINCE THE #242 FOLLOW-UP, and worth reading before assuming it is
// fixture noise. PutRuleSuggestion now writes NOTHING for a message that already
// carries any label, because a suggestion row satisfies one arm of the
// needs-a-category predicate and would drag already-categorised mail back into
// the review queue (a real share of them on a real mailbox — see
// rule_suggestion_filed_integration_test.go, which is where that behaviour is
// asserted). seedUser files its message under a label, so without this every
// PutRuleSuggestion below would be a silent no-op and this suite would be
// testing precedence between one writer and nobody.
//
// THIS FILE IS ABOUT WHICH WRITER WINS, which is a different question from
// whether either should speak at all. Unfiling the message here keeps the two
// separate rather than tangling the guard into every precedence assertion; that
// the suite went red the moment the guard landed is the evidence these calls
// were load-bearing.
func unfile(t *testing.T, h *harness, u fixtureUser) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`DELETE FROM app.message_labels WHERE user_id = $1 AND message_id = $2`,
		u.ID, u.MessageID); err != nil {
		t.Fatalf("unfile the fixture message: %v", err)
	}
}

// seededSuggestionLabel returns the label seedUser attached its
// app.label_suggestions row to. Read from the database rather than added to
// fixtureUser: every other suite is indifferent to it, and widening the shared
// fixture for one test is how fixtures grow until nobody can tell what a given
// field is for.
func seededSuggestionLabel(t *testing.T, h *harness, u fixtureUser) ulid.ULID {
	t.Helper()
	var id string
	if err := h.super.QueryRow(context.Background(),
		`SELECT label_id FROM app.label_suggestions WHERE user_id = $1`, u.ID,
	).Scan(&id); err != nil {
		t.Fatalf("read seeded suggestion label: %v", err)
	}
	return mustULID(t, id)
}
