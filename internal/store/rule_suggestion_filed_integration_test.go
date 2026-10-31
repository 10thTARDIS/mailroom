// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The guard that keeps a QUESTION off mail that already has an ANSWER — the
// #242 follow-up.
//
// A label suggestion is the system asking "which category is this?". Asking it
// about mail that is already filed does not merely waste a row: it UN-FILES the
// message in the user's view, because the needs-a-category predicate
// (EmailQuery.NeedsCategory, jmap_email.go) admits a message on either of two
// arms —
//
//	EXISTS (label_suggestions …)  OR  NOT EXISTS (message_labels …)
//
// — so a suggestion written on top of a rule-applied label satisfies the FIRST
// one. MEASURED before the guard existed: of the suggestions
// `suggest-rules --backfill` would have written on a real mailbox, a fair share
// landed on mail a rule had already filed, and the review queue would have
// grown. A review surface that GROWS by asking about mail it has already
// answered is worse than one that stays empty.
//
// Both halves are asserted here, and the second is what makes the first mean
// anything: a PutRuleSuggestion that had stopped writing altogether would
// satisfy every "wrote nothing" assertion in this file.

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// filedFixture is one seeded user plus a message-and-label factory. Message
// identity is (user_id, content_hash) with an FK onto app.blobs, so every
// message needs its own hash and its own blob row — the same shape
// label_model_integration_test.go's newMsg uses.
type filedFixture struct {
	h      *harness
	u      fixtureUser
	userID ulid.ULID
	// suggestTarget is the label the rule-sourced suggestion is FOR. It is
	// deliberately NOT the label anything below gets filed under: the guard's
	// claim is "this message already has a category", not "this message already
	// has THIS category", and a fixture that used one label for both could not
	// tell those apart.
	suggestTarget ulid.ULID
	// filedUnder is the label the filed messages carry.
	filedUnder ulid.ULID
}

func newFiledFixture(t *testing.T, tag string) *filedFixture {
	t.Helper()
	h := newHarness(t)
	u := seedUser(t, h.super, tag+"-"+NewULID().String())
	f := &filedFixture{h: h, u: u, userID: mustULID(t, u.ID)}
	f.suggestTarget = f.newLabel(t, "target")
	f.filedUnder = f.newLabel(t, "filed")
	return f
}

func (f *filedFixture) newLabel(t *testing.T, name string) ulid.ULID {
	t.Helper()
	id := NewULID().String()
	mustExec(t, f.h, `INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, $3)`,
		id, f.u.ID, name+"-"+f.u.Tag)
	return mustULID(t, id)
}

// newMessage inserts an untrashed message with no labels. appliedBy, when
// non-empty, files it under filedUnder with that provenance — 'user', 'rule',
// 'ml' or 'agent'.
func (f *filedFixture) newMessage(t *testing.T, subject, appliedBy string) ulid.ULID {
	t.Helper()
	id := NewULID().String()
	hash := sha256.Sum256([]byte("filed-msg-" + id))
	mustExec(t, f.h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	                  VALUES ($1, $2, 10, 42, $3)`, f.u.ID, hash[:], NewULID().String())
	mustExec(t, f.h, `INSERT INTO app.messages (id, user_id, content_hash, subject,
	                         from_address, received_at, size_bytes)
	                  VALUES ($1, $2, $3, $4, 'deals@shop.example', now(), 10)`,
		id, f.u.ID, hash[:], subject)
	if appliedBy != "" {
		mustExec(t, f.h, `INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
		                  VALUES ($1, $2, $3, $4)`, f.u.ID, id, f.filedUnder.String(), appliedBy)
	}
	return mustULID(t, id)
}

func (f *filedFixture) suggestionCount(t *testing.T, msgID ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.label_suggestions WHERE user_id = $1 AND message_id = $2`,
		f.u.ID, msgID.String()).Scan(&n); err != nil {
		t.Fatalf("count suggestions: %v", err)
	}
	return n
}

func (f *filedFixture) changeLogCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1`, f.u.ID).Scan(&n); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	return n
}

// TestPutRuleSuggestionSkipsAFiledMessage is the guard itself: ANY label, from
// ANY writer, means no suggestion row.
//
// "Already carries a label" means any label, whoever applied it, because that is
// exactly what the needs-a-category predicate asks — it carries no applied_by
// test. It deliberately does NOT mean "the user filed it by hand": that is a
// different question with a different right answer elsewhere (induction reads
// only hand-applied labels as EVIDENCE), and reusing that definition here is
// precisely the conflation that produced the 36. So every provenance the column
// allows is exercised, not just the two that motivated the fix.
//
// THE LAST ROW IS THE POSITIVE CONTROL and it is not optional. Without a case
// that WRITES, this whole table would pass against a PutRuleSuggestion that had
// been broken into a no-op, or against a fixture whose label id resolved to
// nothing.
func TestPutRuleSuggestionSkipsAFiledMessage(t *testing.T) {
	f := newFiledFixture(t, "filedguard")
	s := f.h.appStore()
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		appliedBy string // "" = no label at all
		wantRows  int
		why       string
	}{
		{"hand-filed", LabelByUser, 0,
			"the user already categorised this by hand; a suggestion re-opens a question they answered"},
		{"rule-filed", LabelByRule, 0,
			"⚠ THE 36. A rule filed this at ingest, so it is categorised — but it contributes no " +
				"UserLabelIDs, which is how the original `no hand label` filter counted it as a target"},
		{"model-filed", LabelByML, 0,
			"applied_by is not part of the needs-a-category predicate, so it must not be part of this guard either"},
		{"agent-filed", LabelByAgent, 0,
			"same reasoning; the column has four values and the guard must answer for all of them"},
		{"unfiled", "", 1,
			"POSITIVE CONTROL: an uncategorised message is the entire target population, and if this " +
				"is 0 then every 'wrote nothing' row above is vacuous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgID := f.newMessage(t, "subject-"+tc.name, tc.appliedBy)
			before := f.changeLogCount(t)

			if err := s.UserTx(ctx, f.userID, func(tx *UserTx) error {
				return tx.PutRuleSuggestion(ctx, msgID, f.suggestTarget, ruleCond, 4)
			}); err != nil {
				// A SILENT SKIP, NOT AN ERROR — asserted, because the callers are bulk
				// and fail-soft. An error here would make ingest log a rule failure on
				// every delivery of already-filed mail, and would abort the admin
				// backfill on the first ordinary, expected case.
				t.Fatalf("PutRuleSuggestion returned an error for a %s message: %v — "+
					"the skip must be silent (%s)", tc.name, err, tc.why)
			}

			if got := f.suggestionCount(t, msgID); got != tc.wantRows {
				t.Errorf("label_suggestions rows = %d, want %d — %s", got, tc.wantRows, tc.why)
			}
			// PutLabelSuggestion, and therefore this, appends NO change_log row: a
			// suggestion is not a JMAP-visible mutation of the Email. Pinned on the
			// writing case too, which is the only one where a stray append is possible.
			if after := f.changeLogCount(t); after != before {
				t.Errorf("change_log rows went %d → %d; a suggestion must append none", before, after)
			}
		})
	}
}

// TestPutRuleSuggestionWritesWhatItPromisesWhenUnfiled reads the surviving row's
// columns back. The table above proves a row EXISTS for unfiled mail; this proves
// the guard did not corrupt what it contains — a pre-check that accidentally
// changed the payload would pass every count assertion.
func TestPutRuleSuggestionWritesWhatItPromisesWhenUnfiled(t *testing.T) {
	f := newFiledFixture(t, "filedpayload")
	s := f.h.appStore()
	ctx := context.Background()
	msgID := f.newMessage(t, "unfiled payload", "")

	if err := s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.PutRuleSuggestion(ctx, msgID, f.suggestTarget, ruleCond, 4)
	}); err != nil {
		t.Fatalf("PutRuleSuggestion: %v", err)
	}

	var (
		source       string
		confidence   float64
		modelVersion int
	)
	if err := f.h.super.QueryRow(ctx,
		`SELECT source, confidence, model_version FROM app.label_suggestions
		  WHERE user_id = $1 AND message_id = $2 AND label_id = $3`,
		f.u.ID, msgID.String(), f.suggestTarget.String()).Scan(&source, &confidence, &modelVersion); err != nil {
		t.Fatalf("read back the suggestion (did the guard swallow the write?): %v", err)
	}
	if source != SuggestionSourceRule {
		t.Errorf("source = %q, want %q", source, SuggestionSourceRule)
	}
	if want := rules.EvidenceConfidence(4); confidence != want {
		t.Errorf("confidence = %v, want %v (the evidence rank, NOT a probability)", confidence, want)
	}
	if modelVersion != 0 {
		t.Errorf("model_version = %d, want 0 — no model is involved in this path", modelVersion)
	}
}

// TestRulePreviewCandidateSeparatesEvidenceFromFiled pins the store-side fact the
// whole fix is built on, at its source.
//
// attachLabelFacts used to carry `applied_by = 'user'` in its WHERE clause, so a
// message filed ONLY by a rule did not come back at all and every caller asking
// "is this filed?" through UserLabelIDs got "no". Moving that test out of the
// query and into the loop is the entire mechanical change behind the #242
// follow-up, and these are the two questions it has to keep apart:
//
//	UserLabelIDs  answers "is this EVIDENCE?"  — whose opinion is it
//	HasAnyLabel   answers "is this FILED?"     — is it categorised at all
//
// It also pins the IMPLICATION `len(UserLabelIDs) > 0 ⟹ HasAnyLabel`, which the
// unit tests in cmd/mailroom-admin build their fixtures on. If the store could
// ever return a hand-labelled candidate with HasAnyLabel false, scanBackfill
// would write a suggestion onto mail the user filed by hand and its unit test
// would still be green — the fixture would simply be describing a shape the
// store no longer produces.
func TestRulePreviewCandidateSeparatesEvidenceFromFiled(t *testing.T) {
	f := newFiledFixture(t, "previewfacts")
	s := f.h.appStore()
	ctx := context.Background()

	hand := f.newMessage(t, "hand filed", LabelByUser)
	rule := f.newMessage(t, "rule filed", LabelByRule)
	model := f.newMessage(t, "model filed", LabelByML)
	unfiled := f.newMessage(t, "unfiled", "")

	var cands []RulePreviewCandidate
	if err := s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		cands, e = tx.RulePreviewCandidates(ctx, 500)
		return e
	}); err != nil {
		t.Fatalf("RulePreviewCandidates: %v", err)
	}
	at := map[ulid.ULID]RulePreviewCandidate{}
	for _, c := range cands {
		at[c.ID] = c
	}

	for _, tc := range []struct {
		name        string
		id          ulid.ULID
		wantAny     bool
		wantUserIDs int
	}{
		{"hand-filed", hand, true, 1},
		// ⚠ THE ROW THE FIX EXISTS FOR: filed (so not a suggestion target) and yet
		// no evidence (so not training data). Before the fix this came back looking
		// identical to the unfiled row below.
		{"rule-filed", rule, true, 0},
		{"model-filed", model, true, 0},
		{"unfiled", unfiled, false, 0},
	} {
		c, ok := at[tc.id]
		if !ok {
			t.Errorf("%s message %s is missing from the candidate scan entirely", tc.name, tc.id)
			continue
		}
		if c.HasAnyLabel != tc.wantAny {
			t.Errorf("%s: HasAnyLabel = %v, want %v — this is the flag the suggestion target "+
				"population is chosen by", tc.name, c.HasAnyLabel, tc.wantAny)
		}
		if len(c.UserLabelIDs) != tc.wantUserIDs {
			t.Errorf("%s: %d user label(s) %v, want %d — only the user's own labels are evidence; "+
				"an automation's opinion fed back to the inducer induces a rule that agrees with it, forever",
				tc.name, len(c.UserLabelIDs), c.UserLabelIDs, tc.wantUserIDs)
		}
	}
	if got := at[hand].UserLabelIDs; len(got) == 1 && got[0] != f.filedUnder {
		t.Errorf("hand-filed candidate's user label = %s, want %s", got[0], f.filedUnder)
	}

	// THE IMPLICATION, over every candidate the scan returned (which includes
	// seedUser's own labelled fixture message, not just the four above).
	for _, c := range cands {
		if len(c.UserLabelIDs) > 0 && !c.HasAnyLabel {
			t.Errorf("candidate %s has %d user label(s) but HasAnyLabel is false — the two fields "+
				"have gone out of step, and every fixture built on that implication is now describing "+
				"a shape the store does not produce", c.ID, len(c.UserLabelIDs))
		}
	}
}
