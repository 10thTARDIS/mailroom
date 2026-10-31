// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// RETRY SAFETY FOR SENDS (#62 — offline compose + queued send).
//
// The offline action queue replays until the server agrees, which is safe for an
// `Email/set` patch (applying it twice is a no-op) and NOT safe for a send. The failure is
// ordinary: the client posts the submission, the server creates it and schedules the send,
// and the RESPONSE is lost — a dropped connection, a closed laptop, a proxy timeout. The
// client cannot distinguish that from "the request never arrived", so it must retry.
//
// Without an idempotency key that retry creates a SECOND submission and a second send job,
// and the recipient gets the message twice. These tests are about that, and the assertions
// are ROW COUNTS in the two tables that decide whether mail goes out: app.submissions and
// app.jobs.

import (
	"context"
	"strings"
	"testing"
)

// countRows is a scalar helper for the two tables that decide whether a send happens.
func sendCount(t *testing.T, h *composeHarness, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// draftFor creates a sendable draft and returns its id.
func draftFor(t *testing.T, h *composeHarness, a apiFixture, from, subject string) string {
	t.Helper()
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"create": map[string]any{
			"d": map[string]any{
				"from":       []map[string]any{{"email": from}},
				"to":         []map[string]any{{"email": "friend@example.net"}},
				"subject":    subject,
				"bodyValues": map[string]any{"1": map[string]any{"value": "hi"}},
				"textBody":   []map[string]any{{"partId": "1"}},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["d"].(map[string]any)
	if !ok {
		t.Fatalf("draft not created: %#v", res)
	}
	id, _ := obj["id"].(string)
	if id == "" {
		t.Fatal("draft has no id")
	}
	return id
}

func TestSubmissionReplayWithTheSameKeySendsOnce(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	draft := draftFor(t, h, a, "me@example.com", "retry me")

	const key = "queued-action-0123456789"
	submit := func() map[string]any {
		return call(t, h.api.submissionSet, a.UserID, map[string]any{
			"create": map[string]any{
				"s": map[string]any{"emailId": draft, "idempotencyKey": key},
			},
		})
	}

	first := submit()
	created, _ := first["created"].(map[string]any)
	obj, ok := created["s"].(map[string]any)
	if !ok {
		t.Fatalf("the first submit was refused: %#v", first)
	}
	firstID, _ := obj["id"].(string)

	// THE REPLAY. It must SUCCEED — a retry the client cannot distinguish from a lost
	// response has to be reported as the success it was, or the queue never drains — and
	// it must return THE SAME submission.
	second := submit()
	created2, _ := second["created"].(map[string]any)
	obj2, ok := created2["s"].(map[string]any)
	if !ok {
		t.Fatalf("the replay was refused rather than reported as already done: %#v", second)
	}
	secondID, _ := obj2["id"].(string)
	if secondID != firstID {
		t.Errorf("replay produced a different submission: %s then %s", firstID, secondID)
	}

	// THE ASSERTIONS THAT MATTER: one submission, and one send job.
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()); got != 1 {
		t.Errorf("submissions = %d, want 1 — the message would be sent %d times", got, got)
	}
	if got := sendCount(t, h, `SELECT count(*) FROM app.jobs WHERE user_id=$1 AND kind='submit'`,
		a.UserID.String()); got != 1 {
		t.Errorf("submit jobs = %d, want 1 — each job sends the message once", got)
	}

	// A THIRD replay, because a queue with backoff retries more than twice.
	submit()
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()); got != 1 {
		t.Errorf("after a third replay: submissions = %d, want 1", got)
	}
}

// THE NEGATIVE CONTROL. Without a key the old behaviour must still hold — two submissions
// — or the test above would pass even if the key were ignored and something else were
// deduping.
func TestSubmissionWithoutAKeyIsNotDeduplicated(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	draft := draftFor(t, h, a, "me@example.com", "no key")

	for i := 0; i < 2; i++ {
		res := call(t, h.api.submissionSet, a.UserID, map[string]any{
			"create": map[string]any{"s": map[string]any{"emailId": draft}},
		})
		if _, ok := res["created"].(map[string]any)["s"]; !ok {
			t.Fatalf("submit %d refused: %#v", i, res)
		}
	}
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()); got != 2 {
		t.Errorf("submissions = %d, want 2 — keyless sends are deliberately NOT deduped, "+
			"so the test above is really exercising the key", got)
	}
}

// Two DIFFERENT keys are two different sends, even for the same draft. Otherwise the key
// would be acting as a per-message lock and a deliberate re-send would be impossible.
func TestSubmissionDifferentKeysAreDifferentSends(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	draft := draftFor(t, h, a, "me@example.com", "two keys")

	for _, key := range []string{"first-action-00000000", "second-action-0000000"} {
		res := call(t, h.api.submissionSet, a.UserID, map[string]any{
			"create": map[string]any{
				"s": map[string]any{"emailId": draft, "idempotencyKey": key},
			},
		})
		if _, ok := res["created"].(map[string]any)["s"]; !ok {
			t.Fatalf("submit with key %q refused: %#v", key, res)
		}
	}
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()); got != 2 {
		t.Errorf("submissions = %d, want 2", got)
	}
}

// A key is scoped PER USER. Two clients that happen to generate the same key must not
// suppress each other's mail — which would be one user silently cancelling another's send.
func TestSubmissionKeysAreScopedPerUser(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	seedSendable(t, h.super, b, "other@example.com", "")

	const key = "same-key-both-users0"
	for _, u := range []struct {
		f    apiFixture
		from string
	}{{a, "me@example.com"}, {b, "other@example.com"}} {
		draft := draftFor(t, h, u.f, u.from, "per user")
		res := call(t, h.api.submissionSet, u.f.UserID, map[string]any{
			"create": map[string]any{
				"s": map[string]any{"emailId": draft, "idempotencyKey": key},
			},
		})
		if _, ok := res["created"].(map[string]any)["s"]; !ok {
			t.Fatalf("submit for %s refused: %#v", u.from, res)
		}
	}
	for _, u := range []apiFixture{a, b} {
		if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
			u.UserID.String()); got != 1 {
			t.Errorf("user %s has %d submissions, want 1 — a shared key across users "+
				"must not suppress either send", u.UserID, got)
		}
	}
}

func TestSubmissionRejectsAnUnusableKey(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	draft := draftFor(t, h, a, "me@example.com", "bad key")

	for _, key := range []string{"short", strings.Repeat("x", 201)} {
		res := call(t, h.api.submissionSet, a.UserID, map[string]any{
			"create": map[string]any{
				"s": map[string]any{"emailId": draft, "idempotencyKey": key},
			},
		})
		e, ok := res["notCreated"].(map[string]any)["s"].(map[string]any)
		if !ok {
			t.Fatalf("a %d-character key was accepted: %#v", len(key), res)
		}
		// The property is named so a client can point at what to fix — and, more
		// importantly, a too-short key is REFUSED rather than padded or truncated: a
		// truncated key could collide with another action's and suppress a real send.
		props, _ := e["properties"].([]any)
		if len(props) != 1 || props[0] != "idempotencyKey" {
			t.Errorf("properties = %v, want exactly [idempotencyKey]", props)
		}
	}
	// Nothing was sent by any of that.
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()); got != 0 {
		t.Errorf("submissions = %d, want 0", got)
	}
}
