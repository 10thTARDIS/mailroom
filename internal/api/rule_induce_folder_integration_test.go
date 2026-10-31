// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Rule/induce OVER A FOLDER (#446 step 3): folder membership is the provider's
// own sorting, and that is evidence of what the user wants in the same way a
// hand-applied label is.
//
// It matters most on the day a mailbox arrives from another client, which is
// exactly when induction over LABELS has nothing to learn from: there are no
// hand-applied Mailroom labels yet. So this path is the only one that can
// propose a rule on day one, and day one is when a rule is worth the most.
//
// The two cases below are the honest pair: a folder that DOES imply a rule, and
// one that does not. The second is not a failure — twelve messages from twelve
// unrelated senders imply nothing, and inventing a rule for them is the
// confident wrongness the whole epic exists to avoid — so the test asserts the
// EMPTY answer explicitly rather than only asserting the positive case.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// folder creates a plain (non-special-use) mailbox for the fixture.
func (h *apiHarness) folder(t *testing.T, f apiFixture, name string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		 VALUES ($1,$2,$3,$4,$4)`,
		id.String(), f.UserID.String(), f.AccountID.String(), name); err != nil {
		t.Fatalf("seed folder %q: %v", name, err)
	}
	return id
}

// inFolder gives a message LIVE provenance in a mailbox — the fact
// RulePreviewCandidate.MailboxIDs reads, and the fact the provider's own sorting
// is recorded as.
func (h *apiHarness) inFolder(t *testing.T, f apiFixture, msg, mailbox ulid.ULID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		 VALUES ($1,$2,$3,$4,$5,$6,1)`,
		store.NewULID().String(), f.UserID.String(), msg.String(), f.AccountID.String(),
		mailbox.String(), testUID()); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
}

func TestRuleInduceOverAFolder(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// Folder A: six messages, three senders, ALL at one domain. A folder somebody
	// has been dragging one relationship's mail into for years.
	folderA := h.folder(t, a, "Folder A")
	for i := 0; i < 6; i++ {
		id := h.seedMessage(t, a, "note", "sender"+string(rune('0'+i%3))+"@one.example",
			time.Duration(i+1)*time.Minute)
		h.inFolder(t, a, id, folderA)
	}
	// Folder D: twelve messages, twelve senders, twelve domains, nothing shared.
	folderD := h.folder(t, a, "Folder D")
	for i := 0; i < 12; i++ {
		id := h.seedMessage(t, a, "misc", "person"+string(rune('a'+i))+"@d"+string(rune('a'+i))+".example",
			time.Duration(20+i)*time.Minute)
		h.inFolder(t, a, id, folderD)
	}
	// Mail OUTSIDE both folders, from a different domain, so a candidate that
	// matched everything would show collateral rather than looking clean by
	// having nothing to catch.
	for i := 0; i < 5; i++ {
		h.seedMessage(t, a, "elsewhere", "someone@other.example", time.Duration(50+i)*time.Minute)
	}

	// ── the folder that DOES imply a rule ─────────────────────────────────────
	res := call(t, h.api.ruleInduce, a.UserID, map[string]any{"mailboxId": folderA.String()})
	if got, _ := res["mailboxId"].(string); got != folderA.String() {
		t.Errorf("mailboxId = %q, want the folder id echoed back (%q)", got, folderA)
	}
	if n, _ := res["examples"].(float64); n != 6 {
		t.Fatalf("examples = %v, want 6 (every message with live provenance in the folder)", res["examples"])
	}
	cands, _ := res["candidates"].([]any)
	if len(cands) == 0 {
		t.Fatal("induced nothing from six messages that share a sender domain; folder membership " +
			"is evidence and this is the case it was added for")
	}
	best, _ := cands[0].(map[string]any)
	if n, _ := best["covers"].(float64); n != 6 {
		t.Errorf("best candidate covers %v of 6 folder messages", best["covers"])
	}
	if n, _ := best["collateral"].(float64); n != 0 {
		t.Errorf("best candidate would also catch %v message(s) outside the folder: %v",
			best["collateral"], best["samples"])
	}
	// THE CONDITION IS ABOUT THE DOMAIN — the property the six actually share.
	// Asserted rather than assumed, because "a candidate came back" would also be
	// true of a rule that matched on something incidental.
	if !conditionMentions(best["conditions"], "fromDomain", "one.example") {
		t.Errorf("the induced condition does not test fromDomain = one.example: %v", best["conditions"])
	}

	// ── the folder that does NOT ──────────────────────────────────────────────
	res = call(t, h.api.ruleInduce, a.UserID, map[string]any{"mailboxId": folderD.String()})
	if n, _ := res["examples"].(float64); n != 12 {
		t.Errorf("examples = %v, want 12 — the count has to be reported even when nothing is "+
			"induced, or an empty answer reads as an empty folder", res["examples"])
	}
	if cands, _ := res["candidates"].([]any); len(cands) != 0 {
		t.Errorf("induced %d candidate(s) from twelve unrelated senders: %v", len(cands), cands)
	}
	if n, _ := res["scanned"].(float64); n == 0 {
		t.Error("scanned = 0: an empty answer must still state the window it was measured over")
	}
}

// EXACTLY ONE of labelId and mailboxId. Both would make "positive" mean two
// different things in one call, and neither leaves nothing to learn from; either
// way the honest answer is invalidArguments rather than a silent precedence rule
// the caller cannot see.
func TestRuleInduceRefusesBothOrNeitherTarget(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	folder := h.folder(t, a, "Some folder")

	cases := []struct {
		name string
		args map[string]any
	}{
		{"neither", map[string]any{}},
		{"both", map[string]any{"labelId": a.LabelID.String(), "mailboxId": folder.String()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, mErr := h.api.ruleInduce(context.Background(), a.UserID, mustJSON(t, tc.args))
			if mErr == nil {
				t.Fatalf("Rule/induce accepted %s target", tc.name)
			}
			if mErr.Type != errInvalidArguments {
				t.Errorf("error type = %q, want %q", mErr.Type, errInvalidArguments)
			}
		})
	}
}

// conditionMentions walks a rendered condition tree looking for one leaf testing
// field against value. The tree is `{field, op, value}` leaves nested under
// `all`/`any`, so a flat check would miss the conjunction the inducer actually
// produces.
func conditionMentions(v any, field, value string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if f, _ := m["field"].(string); f == field {
		if val, _ := m["value"].(string); val == value {
			return true
		}
	}
	for _, key := range []string{"all", "any"} {
		subs, _ := m[key].([]any)
		for _, s := range subs {
			if conditionMentions(s, field, value) {
				return true
			}
		}
	}
	return false
}
