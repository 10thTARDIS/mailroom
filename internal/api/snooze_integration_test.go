// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The JMAP surface of snooze (spec §6, mailroom#51): snoozedUntil as the one
// settable property, isSnoozed as the derived boolean, and the isSnoozed filter
// condition that both hides snoozed mail from a mailbox view and IS the Snoozed
// view.
//
// The store suite covers the write path's guarantees (one change_log row, the
// timer committed with the state, idempotent waking). What is tested here is the
// contract a CLIENT sees, including the two ways it can get the request wrong.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// snoozedUntil reads a message's wake time as its owner, through the store's
// RLS-scoped path — for probes that must prove a write did NOT land.
func (h *apiHarness) snoozedUntil(t *testing.T, userID, messageID ulid.ULID) *time.Time {
	t.Helper()
	ctx := context.Background()
	var at *time.Time
	err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		views, err := tx.GetEmails(ctx, []ulid.ULID{messageID})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Fatalf("owner cannot see message %s", messageID)
		}
		at = views[0].SnoozedUntil
		return nil
	})
	if err != nil {
		t.Fatalf("snoozedUntil: %v", err)
	}
	return at
}

func TestEmailSetSnoozeRoundTrip(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	wake := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"snoozedUntil": jmapDate(wake)},
		},
	})
	if _, ok := res["updated"].(map[string]any)[a.MessageID.String()]; !ok {
		t.Fatalf("snooze not in updated: %v", res)
	}

	// Email/get reports both halves of the pair.
	got := call(t, h.api.emailGet, a.UserID, map[string]any{
		"ids":        []string{a.MessageID.String()},
		"properties": []string{"id", "isSnoozed", "snoozedUntil"},
	})
	obj := got["list"].([]any)[0].(map[string]any)
	if obj["isSnoozed"] != true {
		t.Errorf("isSnoozed = %v, want true", obj["isSnoozed"])
	}
	if obj["snoozedUntil"] != jmapDate(wake) {
		t.Errorf("snoozedUntil = %v, want %s", obj["snoozedUntil"], jmapDate(wake))
	}

	// The message is out of the mailbox view and in the Snoozed view — the two
	// halves of "leaves the inbox until it is due".
	hidden := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": a.InboxID.String(), "isSnoozed": false},
	})
	if n := len(hidden["ids"].([]any)); n != 0 {
		t.Errorf("inbox view returned %d ids, want 0 — a snoozed message is still showing in the inbox", n)
	}
	snoozedView := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"isSnoozed": true},
	})
	ids := snoozedView["ids"].([]any)
	if len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Errorf("Snoozed view = %v, want [%s]", ids, a.MessageID)
	}
	// A snooze-filtered query cannot compute a real delta: membership also turns
	// over with the clock, not only with the change log.
	if snoozedView["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v for an isSnoozed filter, want false",
			snoozedView["canCalculateChanges"])
	}

	// null un-snoozes, and the message comes straight back to the inbox view.
	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{a.MessageID.String(): map[string]any{"snoozedUntil": nil}},
	})
	if at := h.snoozedUntil(t, a.UserID, a.MessageID); at != nil {
		t.Errorf("snoozed_until = %v after un-snooze, want nil", at)
	}
	back := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": a.InboxID.String(), "isSnoozed": false},
	})
	if n := len(back["ids"].([]any)); n != 1 {
		t.Errorf("inbox view returned %d ids after un-snooze, want 1", n)
	}
}

// The two ways a client gets this wrong, both answered with a property-level
// error rather than a shrug.
func TestEmailSetSnoozeRejectsBadWakeTimes(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	for name, val := range map[string]any{
		"in the past":     jmapDate(time.Now().Add(-time.Hour)),
		"not a timestamp": "next tuesday-ish",
		"not a string":    42,
	} {
		res := call(t, h.api.emailSet, a.UserID, map[string]any{
			"update": map[string]any{a.MessageID.String(): map[string]any{"snoozedUntil": val}},
		})
		se, ok := res["notUpdated"].(map[string]any)[a.MessageID.String()]
		if !ok {
			t.Errorf("%s: accepted %v, want notUpdated", name, val)
			continue
		}
		if typ := se.(map[string]any)["type"]; typ != setInvalidProps {
			t.Errorf("%s: error type = %v, want %s", name, typ, setInvalidProps)
		}
		if at := h.snoozedUntil(t, a.UserID, a.MessageID); at != nil {
			t.Errorf("%s: a rejected snooze still wrote %v", name, at)
		}
	}
}

// An unknown filter condition must still be refused now that a Mailroom-specific
// one exists — the superset-of-what-was-asked-for failure mode (#20) does not
// get to come back through the new door.
func TestEmailQueryStillRejectsUnknownConditions(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	_, mErr := h.api.emailQuery(context.Background(), a.UserID, mustJSON(t, map[string]any{
		"filter": map[string]any{"isSnooozed": true}, // note the typo a client would make
	}))
	if mErr == nil || mErr.Type != errUnsupportedFilter {
		t.Errorf("misspelled condition: got %+v, want unsupportedFilter", mErr)
	}
}

// Cross-user: A cannot snooze B's mail, and B's message is genuinely untouched.
// Asserting both matters — a write that leaked past RLS while still reporting
// notFound would satisfy only the first half.
func TestEmailSetSnoozeCrossUser(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			b.MessageID.String(): map[string]any{
				"snoozedUntil": jmapDate(time.Now().Add(time.Hour)),
			},
		},
	})
	se, ok := res["notUpdated"].(map[string]any)[b.MessageID.String()]
	if !ok || se.(map[string]any)["type"] != setNotFound {
		t.Errorf("A snoozing B: notUpdated = %v, want notFound", res["notUpdated"])
	}
	if at := h.snoozedUntil(t, b.UserID, b.MessageID); at != nil {
		t.Errorf("B's message was snoozed by A (until %v)", at)
	}
}
