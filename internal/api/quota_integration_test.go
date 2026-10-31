// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration coverage for storage-quota enforcement on Email/set create (spec
// §10, mailroom#97). The invariant under test: an over-quota compose is an
// EXPLICIT, clean refusal (an overQuota SetError in notCreated) with NOTHING
// stored — no message row, no blob row, no change_log row — never a silent drop.
// It runs through the real client-facing method against a real Postgres with RLS,
// because a check that is bypassable from /jmap protects nothing.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setQuota sets a user's storage cap (app.users.quota_storage_bytes).
func setQuota(t *testing.T, super *pgxpool.Pool, userID string, limit int64) {
	t.Helper()
	if _, err := super.Exec(context.Background(),
		`UPDATE app.users SET quota_storage_bytes = $2 WHERE id = $1`, userID, limit); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

func TestEmailSetCreateRefusesOverQuota(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	// A cap of 1 byte: the fixture already stores a seed blob, so ANY new draft is
	// over quota. (A 0 cap would read as "unlimited" — store.QuotaAdmits — which is
	// exactly the semantics we must NOT trip here.)
	setQuota(t, h.super, a.UserID.String(), 1)

	create := map[string]any{
		"create": map[string]any{
			"c1": map[string]any{
				"from":       []map[string]any{{"email": "me@example.com"}},
				"to":         []map[string]any{{"email": "friend@example.net"}},
				"subject":    "over quota please",
				"bodyValues": map[string]any{"1": map[string]any{"value": "this should not be stored"}},
				"textBody":   []map[string]any{{"partId": "1"}},
			},
		},
	}

	// Baselines: the seed graph already has one (non-draft) message and one blob.
	blobsBefore := countRows(t, h.super,
		`SELECT count(*) FROM app.blobs WHERE user_id = $1`, a.UserID.String())
	changesBefore := countRows(t, h.super,
		`SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.UserID.String())

	res := call(t, h.api.emailSet, a.UserID, create)
	e := setError(t, res, "notCreated", "c1")
	if e["type"] != setOverQuota {
		t.Fatalf("SetError type = %v, want %q — an over-quota create must refuse cleanly", e["type"], setOverQuota)
	}
	if created, _ := res["created"].(map[string]any); len(created) != 0 {
		t.Fatalf("something was created despite being over quota: %#v", created)
	}

	// NOTHING was stored: no draft row, no new blob row, no new change_log row.
	if n := countRows(t, h.super,
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND is_draft`, a.UserID.String()); n != 0 {
		t.Errorf("%d draft(s) written despite an over-quota refusal — the invariant is a silent-drop ban", n)
	}
	if n := countRows(t, h.super,
		`SELECT count(*) FROM app.blobs WHERE user_id = $1`, a.UserID.String()); n != blobsBefore {
		t.Errorf("app.blobs rows went %d -> %d across an over-quota refusal; the row must not be recorded (the bytes are GC'd)", blobsBefore, n)
	}
	if n := countRows(t, h.super,
		`SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.UserID.String()); n != changesBefore {
		t.Errorf("change_log rows went %d -> %d across an over-quota refusal; nothing JMAP-visible changed", changesBefore, n)
	}

	// Raise the cap and the same compose succeeds — proving the refusal was the
	// quota and not something else, and that a create UNDER quota still works.
	setQuota(t, h.super, a.UserID.String(), 100<<20)
	ok := call(t, h.api.emailSet, a.UserID, create)
	created, _ := ok["created"].(map[string]any)
	if len(created) != 1 {
		t.Fatalf("a create under quota must succeed: %#v", ok)
	}
	if n := countRows(t, h.super,
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND is_draft`, a.UserID.String()); n != 1 {
		t.Errorf("draft count = %d after an under-quota create, want 1", n)
	}
}

func countRows(t *testing.T, super *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
