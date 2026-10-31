// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The JMAP surface of flagged_at (#444): flaggedAt is a read-only, opt-in
// timestamp property that follows the $flagged keyword — never a property a
// client can patch directly. The store suite (internal/store) covers the
// write path's SQL-level guarantees; this file covers the contract a CLIENT
// sees.

import (
	"testing"
)

func TestEmailGetFlaggedAtFollowsTheFlaggedKeyword(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// Freshly seeded: unflagged, so flaggedAt is null.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "flaggedAt"},
	})
	obj := got["list"].([]any)[0].(map[string]any)
	if obj["flaggedAt"] != nil {
		t.Fatalf("flaggedAt on an unflagged message = %v, want nil", obj["flaggedAt"])
	}

	// The $flagged keyword is the settable half of the pair.
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/$flagged": true},
		},
	})
	if _, ok := res["updated"].(map[string]any)[a.MessageID.String()]; !ok {
		t.Fatalf("$flagged patch not in updated: %v", res)
	}

	got = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "flaggedAt"},
	})
	obj = got["list"].([]any)[0].(map[string]any)
	at, ok := obj["flaggedAt"].(string)
	if !ok || at == "" {
		t.Fatalf("flaggedAt after flagging = %v, want a UTCDate string", obj["flaggedAt"])
	}

	// Unflagging clears it back to null.
	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/$flagged": false},
		},
	})
	got = call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "flaggedAt"},
	})
	obj = got["list"].([]any)[0].(map[string]any)
	if obj["flaggedAt"] != nil {
		t.Errorf("flaggedAt after unflagging = %v, want nil", obj["flaggedAt"])
	}
}

// flaggedAt is SERVER-DERIVED (#444): the server stamps the time so two
// clients cannot disagree about when a message was flagged. A patch naming it
// directly is refused exactly like any other read-only property — through the
// same generic "unsupported Email property in patch" arm that already refuses
// e.g. returnedAt or priorityScore, not a bespoke check.
func TestEmailSetRefusesFlaggedAtInPatch(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"flaggedAt": "2026-01-01T00:00:00Z"},
		},
	})
	se, ok := res["notUpdated"].(map[string]any)[a.MessageID.String()]
	if !ok {
		t.Fatalf("patch naming flaggedAt accepted: %v", res)
	}
	if typ := se.(map[string]any)["type"]; typ != setInvalidProps {
		t.Errorf("error type = %v, want %s", typ, setInvalidProps)
	}

	// And it genuinely did not write anything.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "flaggedAt"},
	})
	obj := got["list"].([]any)[0].(map[string]any)
	if obj["flaggedAt"] != nil {
		t.Errorf("a refused flaggedAt patch still wrote %v", obj["flaggedAt"])
	}
}
