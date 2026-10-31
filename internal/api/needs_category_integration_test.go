// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The JMAP surface of the classifier's abstentions (#169): the suggestedLabels
// property and the needsCategory filter that backs the review queue.
//
// What matters here is that a suggestion is NOT a label. It must not appear in
// keywords, must not affect mailbox membership, and must disappear the moment a
// human answers — otherwise the review queue shows work that is already done,
// which is the fastest way to teach someone to ignore it.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func (h *apiHarness) suggest(t *testing.T, f apiFixture, msgID, labelID ulid.ULID, confidence float64) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.label_suggestions (user_id, message_id, label_id, confidence, model_version)
		 VALUES ($1,$2,$3,$4,1)`,
		f.UserID.String(), msgID.String(), labelID.String(), confidence); err != nil {
		t.Fatalf("seed suggestion: %v", err)
	}
}

// suggestWithReasons seeds a suggestion that also carries the WHY payload
// (#176) — the raw []ml.FeatureContribution shape migration 00019 stores.
func (h *apiHarness) suggestWithReasons(t *testing.T, f apiFixture, msgID, labelID ulid.ULID, confidence float64, reasons string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.label_suggestions (user_id, message_id, label_id, confidence, model_version, reasons)
		 VALUES ($1,$2,$3,$4,1,$5)`,
		f.UserID.String(), msgID.String(), labelID.String(), confidence, reasons); err != nil {
		t.Fatalf("seed suggestion with reasons: %v", err)
	}
}

func TestSuggestedLabelsAreNotLabels(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	msg := h.seedMessage(t, a, "Half price everything", "deals@shop.example", time.Minute)
	h.suggest(t, a, msg, promotions, 0.55)

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msg.String()},
		"properties": []string{"id", "keywords", "suggestedLabels"},
	})
	obj := res["list"].([]any)[0].(map[string]any)

	sugg, _ := obj["suggestedLabels"].([]any)
	if len(sugg) != 1 {
		t.Fatalf("suggestedLabels = %v, want one entry", obj["suggestedLabels"])
	}
	entry := sugg[0].(map[string]any)
	if entry["labelId"] != promotions.String() {
		t.Errorf("labelId = %v, want %v", entry["labelId"], promotions)
	}
	if c, _ := entry["confidence"].(float64); c < 0.54 || c > 0.56 {
		t.Errorf("confidence = %v, want ~0.55", entry["confidence"])
	}

	// THE POINT: a suggestion is a question, not a weak label. It must not have
	// leaked into the keyword set, where every other client would read it as a
	// label the user applied.
	kw, _ := obj["keywords"].(map[string]any)
	for k := range kw {
		if k != store.KeywordSeen && k != store.KeywordFlagged && k != "$draft" {
			t.Errorf("a suggestion leaked into keywords as %q", k)
		}
	}
}

// A message with no open question reports null, not an empty array — the same
// contract as the other opt-in extension properties.
func TestSuggestedLabelsNullWhenNoneOpen(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "suggestedLabels"},
	})
	obj := res["list"].([]any)[0].(map[string]any)
	if obj["suggestedLabels"] != nil {
		t.Errorf("suggestedLabels = %v, want null", obj["suggestedLabels"])
	}
}

// queueIDs runs the needs-a-category query and returns the ids as strings.
func queueIDs(t *testing.T, h *apiHarness, f apiFixture) []string {
	t.Helper()
	yes := true
	res := call(t, h.api.emailQuery, f.UserID, map[string]any{
		"filter": map[string]any{"needsCategory": yes},
	})
	out := []string{}
	for _, id := range res["ids"].([]any) {
		out = append(out, id.(string))
	}
	return out
}

func contains(ids []string, want ulid.ULID) bool {
	for _, id := range ids {
		if id == want.String() {
			return true
		}
	}
	return false
}

// THE QUEUE IS "NOTHING HAS CATEGORISED THIS" (#176), not "the classifier
// abstained".
//
// This is the change that makes the surface exist at all. On a fresh
// mailbox there are no model rows, no suggestions, and many unlabelled inbox
// messages. Under the abstention-only reading the queue is empty on every
// mailbox that has never trained a model — which is every mailbox on its first
// day, and the day supervision is worth the most.
func TestNeedsCategoryIsEverythingNothingHasCategorised(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	promotions := h.label(t, a, "Promotions")
	asked := h.seedMessage(t, a, "Maybe promotional", "deals@shop.example", time.Minute)
	h.suggest(t, a, asked, promotions, 0.5)
	untouched := h.seedMessage(t, a, "Nobody has an opinion", "friend@example.com", 2*time.Minute)
	filed := h.seedMessage(t, a, "Already filed", "orders@shop.example", 3*time.Minute)
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, filed, store.EmailUpdate{AddLabelIDs: []ulid.ULID{promotions}})
		return err
	}); err != nil {
		t.Fatalf("apply label: %v", err)
	}

	ids := queueIDs(t, h, a)
	if !contains(ids, asked) {
		t.Errorf("the message with an open question is not in the queue: %v", ids)
	}
	if !contains(ids, untouched) {
		t.Errorf("unlabelled mail is not in the queue — this is the arm that makes the surface non-empty before a model exists: %v", ids)
	}
	if contains(ids, filed) {
		t.Errorf("a message that already carries a label is in the queue: %v", ids)
	}

	// The complement must be exactly that, with no message in both halves and
	// none in neither.
	no := false
	rest := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"needsCategory": no},
	})
	restIDs := []string{}
	for _, id := range rest["ids"].([]any) {
		restIDs = append(restIDs, id.(string))
	}
	if !contains(restIDs, filed) {
		t.Errorf("needsCategory:false did not return the filed message: %v", restIDs)
	}
	for _, id := range restIDs {
		if contains(ids, ulid.MustParse(id)) {
			t.Errorf("message %s is in BOTH halves of the filter", id)
		}
	}
}

// A rule's label is a category, and so is the classifier's. The queue is about
// the ABSENCE of a category, not about who would have supplied it.
func TestNeedsCategoryAcceptsAnyLabelWhoeverAppliedIt(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	promotions := h.label(t, a, "Promotions")
	msg := h.seedMessage(t, a, "Filed by a rule", "deals@shop.example", time.Minute)
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{
			AddLabelIDs: []ulid.ULID{promotions},
			LabelSource: store.LabelSource{Kind: store.LabelByML},
		})
		return err
	}); err != nil {
		t.Fatalf("apply label: %v", err)
	}

	if ids := queueIDs(t, h, a); contains(ids, msg) {
		t.Errorf("a message the classifier categorised is still in the queue: %v", ids)
	}
}

// A message with SEVERAL open questions appears ONCE — and so does one that is
// BOTH unlabelled and suggested, which is every message the classifier has ever
// asked about. EXISTS was chosen over joins precisely so the queue counts
// messages rather than rows.
func TestNeedsCategoryReturnsAMessageOnceRegardlessOfSuggestionCount(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	msg := h.seedMessage(t, a, "Ambiguous", "someone@shop.example", time.Minute)
	for _, name := range []string{"Promotions", "Shopping", "Updates"} {
		h.suggest(t, a, msg, h.label(t, a, name), 0.5)
	}

	yes := true
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"needsCategory": yes},
	})
	ids := res["ids"].([]any)
	seen := 0
	for _, id := range ids {
		if id == msg.String() {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("message with three open questions appeared %d times: %v", seen, ids)
	}
	// The total is the count of MESSAGES; a join would have made it 3 for this
	// message alone.
	if n, _ := res["total"].(float64); int(n) != len(ids) {
		t.Errorf("total = %v but the page has %d ids — the count is counting rows, not messages", res["total"], len(ids))
	}
}

// Answering closes the question. Once the user labels the message themselves,
// every open suggestion on it is stale — including ones for other labels,
// because the answer they gave IS the answer.
func TestUserLabellingClearsOpenQuestions(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	shopping := h.label(t, a, "Shopping")
	promotions := h.label(t, a, "Promotions")
	msg := h.seedMessage(t, a, "Your order", "orders@shop.example", time.Minute)
	h.suggest(t, a, msg, promotions, 0.5)
	h.suggest(t, a, msg, shopping, 0.6)

	// The user files it under Shopping, the way Email/set does.
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{AddLabelIDs: []ulid.ULID{shopping}})
		return err
	}); err != nil {
		t.Fatalf("apply label: %v", err)
	}

	if ids := queueIDs(t, h, a); contains(ids, msg) {
		t.Errorf("message still in the review queue after the user answered: %v", ids)
	}
}

// MOST-CONFIDENT FIRST (#176) — the queue's whole ordering claim, and the one
// that cannot be delegated to the client.
//
// The seed is adversarial on purpose: confidence runs OPPOSITE to arrival, so
// the default received_at order would produce the exact reverse. A client
// re-sorting the page it was handed would look right on this small fixture and
// be wrong the moment the queue is longer than a page — which is the case this
// sort exists for.
func TestNeedsCategoryOrdersByConfidenceNotArrival(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	// seedMessage's age is received_at = now() - age, so the MOST confident is
	// seeded OLDEST. Default receivedAt-desc would therefore return weak, middle,
	// strong — the exact reverse of what is asserted below, which is what makes
	// this a test of the sort rather than of the fixture.
	strong := h.seedMessage(t, a, "Strong", "c@shop.example", 3*time.Minute)
	middle := h.seedMessage(t, a, "Middle", "b@shop.example", 2*time.Minute)
	weak := h.seedMessage(t, a, "Weak", "a@shop.example", time.Minute)
	h.suggest(t, a, weak, promotions, 0.31)
	h.suggest(t, a, middle, promotions, 0.55)
	h.suggest(t, a, strong, promotions, 0.79)

	yes := true
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"needsCategory": yes},
		"sort":   []any{map[string]any{"property": "suggestionConfidence", "isAscending": false}},
	})
	// Positions of the three seeded messages, ignoring the fixture's own mail
	// (unlabelled, so also queued — with no confidence, hence last).
	want := []ulid.ULID{strong, middle, weak}
	ids := res["ids"].([]any)
	at := func(target ulid.ULID) int {
		for i, id := range ids {
			if id == target.String() {
				return i
			}
		}
		return -1
	}
	for i := 1; i < len(want); i++ {
		prev, cur := at(want[i-1]), at(want[i])
		if prev < 0 || cur < 0 {
			t.Fatalf("a seeded message is missing from the queue: %v", ids)
		}
		if prev > cur {
			t.Fatalf("order = %v, want strong→middle→weak (confidence desc, the REVERSE of arrival here)", ids)
		}
	}
}

// A message is ordered by its BEST open question, not by an arbitrary one. The
// ORDER BY uses a correlated max() for the same reason the filter uses EXISTS:
// a message with three questions is still one row in the queue.
func TestSuggestionConfidenceSortUsesTheHighestOpenQuestion(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	shopping := h.label(t, a, "Shopping")
	// `many` has a weak question and a strong one; `one` has a middling one.
	// Judged on its best, `many` leads; on its worst, it would trail.
	many := h.seedMessage(t, a, "Several questions", "a@shop.example", 2*time.Minute)
	one := h.seedMessage(t, a, "One question", "b@shop.example", time.Minute)
	h.suggest(t, a, many, promotions, 0.22)
	h.suggest(t, a, many, shopping, 0.78)
	h.suggest(t, a, one, promotions, 0.50)

	yes := true
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"needsCategory": yes},
		"sort":   []any{map[string]any{"property": "suggestionConfidence", "isAscending": false}},
	})
	ids := res["ids"].([]any)
	if len(ids) == 0 || ids[0] != many.String() {
		t.Errorf("order = %v, want %s first — its BEST question is 0.78, though its worst is 0.22", ids, many)
	}
	// Ordered by max(), so `one` (0.50) trails `many` (0.78) rather than leading
	// it, which is what a min() or an arbitrary pick would have produced.
	if len(ids) < 2 || ids[1] != one.String() {
		t.Errorf("order = %v, want %s second", ids, one)
	}
}

// The sort must not act as a filter. Asked for on an unfiltered query, mail with
// no open question still comes back — last, not missing.
func TestSuggestionConfidenceSortDoesNotHideUnsuggestedMail(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	plain := h.seedMessage(t, a, "No question here", "a@shop.example", time.Minute)
	asked := h.seedMessage(t, a, "A question", "b@shop.example", 2*time.Minute)
	h.suggest(t, a, asked, promotions, 0.5)

	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"sort": []any{map[string]any{"property": "suggestionConfidence", "isAscending": false}},
	})
	ids := res["ids"].([]any)
	var sawPlain bool
	for _, id := range ids {
		if id == plain.String() {
			sawPlain = true
		}
	}
	if !sawPlain {
		t.Fatalf("the sort dropped mail with no suggestions: %v", ids)
	}
	if ids[0] != asked.String() {
		t.Errorf("order = %v, want the suggested message first (NULLS LAST)", ids)
	}
}

// The explainability contract (#176): a suggestion's reasons render in the SAME
// {label, weight, polarity} shape as priorityReasons, through the same function.
// Two "why?" panels, one payload — otherwise they drift into disagreeing about
// what an explanation looks like.
func TestSuggestedLabelsCarryTheirReasons(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	msg := h.seedMessage(t, a, "Half price everything", "deals@shop.example", time.Minute)
	h.suggestWithReasons(t, a, msg, promotions, 0.55,
		`[{"key":"unsub:1","weight":0.9},{"key":"corr:1","weight":-0.4}]`)

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msg.String()},
		"properties": []string{"id", "suggestedLabels"},
	})
	obj := res["list"].([]any)[0].(map[string]any)
	entry := obj["suggestedLabels"].([]any)[0].(map[string]any)

	reasons, _ := entry["reasons"].([]any)
	if len(reasons) != 2 {
		t.Fatalf("reasons = %v, want two entries", entry["reasons"])
	}
	first := reasons[0].(map[string]any)
	// The human phrase is applied at RENDER time from the stored key, exactly as
	// priorityReasons does — storage keeps keys so wording can change freely.
	if first["label"] != "Bulk mail (has unsubscribe)" {
		t.Errorf("label = %v, want the rendered phrase for unsub:1", first["label"])
	}
	if first["polarity"] != "up" {
		t.Errorf("polarity = %v, want \"up\" for a positive weight", first["polarity"])
	}
	if second := reasons[1].(map[string]any); second["polarity"] != "down" {
		t.Errorf("polarity = %v, want \"down\" for a negative weight", second["polarity"])
	}
}

// '[]' IS NOT NULL. A model that explained itself and found nothing above the
// noise floor said something; a suggestion with no reasons column said nothing.
// The client renders different sentences for the two, and the whole reason
// label_suggestions.reasons is nullable — unlike triage_scores.top_features — is
// to keep them apart.
//
// This is the case that was missing while the API rendered both through
// priorityReasons, whose `len(contribs) == 0 → nil` is correct for a NOT NULL
// DEFAULT '[]' column and wrong for this one.
func TestSuggestedLabelsDistinguishEmptyReasonsFromNoRecord(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	explained := h.seedMessage(t, a, "Explained, nothing to say", "a@shop.example", time.Minute)
	unrecorded := h.seedMessage(t, a, "Never explained", "b@shop.example", 2*time.Minute)
	h.suggestWithReasons(t, a, explained, promotions, 0.5, `[]`)
	h.suggest(t, a, unrecorded, promotions, 0.5)

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{explained.String(), unrecorded.String()},
		"properties": []string{"id", "suggestedLabels"},
	})
	for _, item := range res["list"].([]any) {
		obj := item.(map[string]any)
		entry := obj["suggestedLabels"].([]any)[0].(map[string]any)
		reasons, present := entry["reasons"]
		if !present {
			t.Fatalf("reasons key missing: %v", entry)
		}
		switch obj["id"] {
		case explained.String():
			arr, ok := reasons.([]any)
			if !ok || len(arr) != 0 {
				t.Errorf("reasons = %v for the model that explained and found nothing; want an EMPTY ARRAY, not null", reasons)
			}
		case unrecorded.String():
			if reasons != nil {
				t.Errorf("reasons = %v for a suggestion with no record of why; want null", reasons)
			}
		}
	}
}

// "No record of why" is null, NOT an empty array. A suggestion written before
// migration 00019 (or by a model that could not explain itself) must not read as
// a server claim that it looked and found nothing.
func TestSuggestedLabelsReasonsAreNullWhenUnrecorded(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	msg := h.seedMessage(t, a, "Sale", "deals@shop.example", time.Minute)
	h.suggest(t, a, msg, promotions, 0.55) // no reasons column written at all

	res := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msg.String()},
		"properties": []string{"id", "suggestedLabels"},
	})
	obj := res["list"].([]any)[0].(map[string]any)
	entry := obj["suggestedLabels"].([]any)[0].(map[string]any)
	if _, present := entry["reasons"]; !present {
		t.Fatalf("reasons key missing from the suggestion entirely: %v", entry)
	}
	if entry["reasons"] != nil {
		t.Errorf("reasons = %v, want null — the model left no record of why", entry["reasons"])
	}
}

// Dismissing takes the message out of the queue WITHOUT filing it anywhere.
// "This does not need a category" has to be answerable, or the only ways to
// clear such a message are to label it falsely — poisoning the corpus this
// surface exists to grow — or to leave it queued forever.
func TestDismissCategoryClearsTheQueueWithoutLabelling(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	shopping := h.label(t, a, "Shopping")
	msg := h.seedMessage(t, a, "Neither of these", "someone@example.com", time.Minute)
	h.suggest(t, a, msg, promotions, 0.5)
	h.suggest(t, a, msg, shopping, 0.6)

	res := call(t, h.api.emailDismissCategory, a.UserID, map[string]any{
		"ids": []string{msg.String()},
	})
	if ids := res["dismissed"].([]any); len(ids) != 1 || ids[0] != msg.String() {
		t.Fatalf("dismissed = %v, want [%s]", res["dismissed"], msg)
	}

	if ids := queueIDs(t, h, a); contains(ids, msg) {
		t.Errorf("message still queued after dismissal: %v", ids)
	}

	// AND it was not filed. A dismissal that quietly applied the leading
	// suggestion would be the silent misfiling this whole epic exists to avoid.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msg.String()},
		"properties": []string{"id", "keywords", "suggestedLabels"},
	})
	obj := got["list"].([]any)[0].(map[string]any)
	if obj["suggestedLabels"] != nil {
		t.Errorf("suggestedLabels = %v, want null after dismissal", obj["suggestedLabels"])
	}
	for k := range obj["keywords"].(map[string]any) {
		if k != store.KeywordSeen && k != store.KeywordFlagged && k != "$draft" {
			t.Errorf("dismissal applied a label: keyword %q", k)
		}
	}
}

// THE CASE THAT FORCED THE TOMBSTONE: dismissing UNLABELLED mail with no
// suggestions on it at all.
//
// This is most of a first-run backlog. If dismissal only deleted suggestion
// rows, there would be nothing to delete and the message would stay queued on
// the strength of being unlabelled — the button would visibly do nothing, and
// the queue could never be emptied except by labelling every message in it.
func TestDismissCategoryRemovesUnlabelledMailWithNoSuggestions(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	msg := h.seedMessage(t, a, "Nobody has an opinion", "someone@example.com", time.Minute)
	if ids := queueIDs(t, h, a); !contains(ids, msg) {
		t.Fatalf("unlabelled mail is not in the queue to begin with: %v", ids)
	}

	res := call(t, h.api.emailDismissCategory, a.UserID, map[string]any{
		"ids": []string{msg.String()},
	})
	if ids := res["dismissed"].([]any); len(ids) != 1 || ids[0] != msg.String() {
		t.Fatalf("dismissed = %v, want [%s] — nothing was suggested, but something DID change", res["dismissed"], msg)
	}
	if ids := queueIDs(t, h, a); contains(ids, msg) {
		t.Errorf("dismissing unlabelled mail did not remove it from the queue: %v", ids)
	}
}

// Drafts and junk are NEVER queued, whatever the caller asks. Both satisfy the
// "no label, not trashed" arm, and both are actively harmful in a list that
// offers to batch-label whatever is in it: junk means one keystroke files spam
// into a real category — poisoning the corpus this surface exists to grow — and
// a draft means being asked to categorise something still being written.
func TestNeedsCategoryExcludesDraftsAndJunk(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	draft := h.seedMessage(t, a, "Half-written", "me@example.com", time.Minute)
	junk := h.seedMessage(t, a, "You have won", "spam@example.com", 2*time.Minute)
	ordinary := h.seedMessage(t, a, "Ordinary mail", "friend@example.com", 3*time.Minute)
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET is_draft = true WHERE id = $1`, draft.String()); err != nil {
		t.Fatalf("mark draft: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET is_junk = true WHERE id = $1`, junk.String()); err != nil {
		t.Fatalf("mark junk: %v", err)
	}

	ids := queueIDs(t, h, a)
	if contains(ids, draft) {
		t.Errorf("a draft is in the needs-a-category queue: %v", ids)
	}
	if contains(ids, junk) {
		t.Errorf("junk is in the needs-a-category queue — one keystroke would file spam into a real category: %v", ids)
	}
	if !contains(ids, ordinary) {
		t.Fatalf("ordinary unlabelled mail is missing, so this test proves nothing: %v", ids)
	}
}

// Undo puts a dismissed message back. A dismissal is otherwise the only
// permanent, silent verb on this surface, reachable from one keystroke.
func TestUndoDismissCategoryRestoresTheMessage(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	msg := h.seedMessage(t, a, "Dismissed by mistake", "someone@example.com", time.Minute)
	call(t, h.api.emailDismissCategory, a.UserID, map[string]any{"ids": []string{msg.String()}})
	if ids := queueIDs(t, h, a); contains(ids, msg) {
		t.Fatalf("dismissal did not take, so the undo proves nothing: %v", ids)
	}

	res := call(t, h.api.emailDismissCategory, a.UserID, map[string]any{
		"ids":  []string{msg.String()},
		"undo": true,
	})
	if ids := res["dismissed"].([]any); len(ids) != 1 || ids[0] != msg.String() {
		t.Fatalf("undo reported %v, want [%s]", res["dismissed"], msg)
	}
	if ids := queueIDs(t, h, a); !contains(ids, msg) {
		t.Errorf("the message did not come back to the queue: %v", ids)
	}

	// Undoing something that was never dismissed changes nothing and says so.
	again := call(t, h.api.emailDismissCategory, a.UserID, map[string]any{
		"ids":  []string{msg.String()},
		"undo": true,
	})
	if ids := again["dismissed"].([]any); len(ids) != 0 {
		t.Errorf("undo of a live message reported %v, want nothing", ids)
	}
}

// An ascending confidence sort must flip the WHOLE order, not just the date
// tie-break — otherwise isAscending:true returns a list ordered coherently by
// neither key.
func TestSuggestionConfidenceSortHonoursAscending(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	promotions := h.label(t, a, "Promotions")
	strong := h.seedMessage(t, a, "Strong", "c@shop.example", time.Minute)
	weak := h.seedMessage(t, a, "Weak", "a@shop.example", 2*time.Minute)
	h.suggest(t, a, strong, promotions, 0.79)
	h.suggest(t, a, weak, promotions, 0.31)

	yes := true
	res := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"needsCategory": yes},
		"sort":   []any{map[string]any{"property": "suggestionConfidence", "isAscending": true}},
	})
	ids := res["ids"].([]any)
	at := func(target ulid.ULID) int {
		for i, id := range ids {
			if id == target.String() {
				return i
			}
		}
		return -1
	}
	if at(weak) < 0 || at(strong) < 0 {
		t.Fatalf("a seeded message is missing: %v", ids)
	}
	if at(weak) > at(strong) {
		t.Errorf("ascending order = %v, want the WEAKEST first", ids)
	}
}

// Re-dismissing is idempotent and reports NOTHING the second time. A client that
// double-clicks must not be told twice that something changed.
func TestDismissCategoryIsIdempotent(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	msg := h.seedMessage(t, a, "Twice", "someone@example.com", time.Minute)
	call(t, h.api.emailDismissCategory, a.UserID, map[string]any{"ids": []string{msg.String()}})
	again := call(t, h.api.emailDismissCategory, a.UserID, map[string]any{"ids": []string{msg.String()}})
	if ids := again["dismissed"].([]any); len(ids) != 0 {
		t.Errorf("second dismissal reported %v, want nothing — it was already settled", ids)
	}
	if ids := again["notFound"].([]any); len(ids) != 0 {
		t.Errorf("notFound = %v — the message exists, it was just already dismissed", ids)
	}
}

// A dismissal appends an Email/updated change_log row, which the ingest-side
// write deliberately does not. Without it, Email.suggestedLabels changes with
// nothing in the change stream to say so, and every OTHER client keeps rendering
// a question this user already answered.
func TestDismissCategoryIsVisibleInEmailChanges(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	promotions := h.label(t, a, "Promotions")
	msg := h.seedMessage(t, a, "Ambiguous", "someone@example.com", time.Minute)
	h.suggest(t, a, msg, promotions, 0.5)

	// The cursor a synced client would be holding just before it dismisses.
	var before int64
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		var e error
		before, e = tx.ChangeState(ctx, store.ObjectEmail)
		return e
	}); err != nil {
		t.Fatalf("read change state: %v", err)
	}

	call(t, h.api.emailDismissCategory, a.UserID, map[string]any{
		"ids": []string{msg.String()},
	})

	res := call(t, h.api.emailChanges, a.UserID, map[string]any{
		// encodeState, not a bare number: a state string without the API's prefix
		// is "malformed cursor", which answers cannotCalculateChanges and would
		// have made this test pass for a reason unrelated to the dismissal.
		"sinceState": encodeState(before),
	})
	if !strInArr(res["updated"], msg.String()) {
		t.Errorf("Email/changes did not report the dismissal: %v", res)
	}
}

// A label the CLASSIFIER applies must not clear its own outstanding questions —
// that would let the model close its own tickets.
func TestClassifierLabellingDoesNotClearQuestions(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	promotions := h.label(t, a, "Promotions")
	other := h.label(t, a, "Shopping")
	msg := h.seedMessage(t, a, "Sale", "deals@shop.example", time.Minute)
	h.suggest(t, a, msg, other, 0.5)

	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{
			AddLabelIDs: []ulid.ULID{promotions},
			LabelSource: store.LabelSource{Kind: store.LabelByML},
		})
		return err
	}); err != nil {
		t.Fatalf("apply label: %v", err)
	}

	// It carries a label now, so the "unlabelled" arm no longer holds it — it is
	// in the queue purely because its open question is still open, which is
	// exactly what this test is about.
	if ids := queueIDs(t, h, a); !contains(ids, msg) {
		t.Errorf("the classifier closed its own question: queue = %v", ids)
	}
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{msg.String()},
		"properties": []string{"id", "suggestedLabels"},
	})
	obj := got["list"].([]any)[0].(map[string]any)
	if sugg, _ := obj["suggestedLabels"].([]any); len(sugg) != 1 {
		t.Errorf("suggestedLabels = %v, want the still-open question", obj["suggestedLabels"])
	}
}
