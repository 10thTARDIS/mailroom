// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ml

// Integration coverage for the replay-side feature path (mailroom#33): a
// StoreExtractor re-reads a seeded message row + the sender's contact stats and
// runs the ONE featuresFromFacts, so the exact keys serve-time would emit come
// out of a stored row. Runs against the throwaway Postgres from
// scripts/integration-test.sh; skips when the env is absent.

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func TestStoreExtractorBuildsFeaturesFromRow(t *testing.T) {
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()

	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	userID := ulid.Make()
	msgID := ulid.Make()
	contactID := ulid.Make()
	hash := sha256.Sum256([]byte("extractor-" + userID.String()))
	sender := "alice@ext.com"

	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1,$2,$3)`,
		userID.String(), "sub-"+userID.String(), userID.String()+"@example.com")
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1,$2,100,132,$3)`, userID.String(), hash[:], ulid.Make().String())
	exec(`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
	        received_at, size_bytes, has_attachments, is_flagged, in_reply_to, addressed_address)
	      VALUES ($1,$2,$3,'Re: Q3 budget',$4,
	              '2026-01-02T09:30:00Z',100,true,true,'<parent@x.test>','me+tag@example.com')`,
		msgID.String(), userID.String(), hash[:], sender)
	// A sender the user has WRITTEN TO 4 times -> corr:1 and sent:3-5.
	//
	// outbound_count, not seen_count (#157): being mailed at is not a
	// relationship, and a fixture that seeded only seen_count would be asserting
	// the very confusion that put newsletters at the top of the priority inbox.
	exec(`INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1,$2,'Alice')`,
		contactID.String(), userID.String())
	exec(`INSERT INTO app.contact_addresses (id, user_id, contact_id, address, seen_count, outbound_count)
	      VALUES ($1,$2,$3,$4,4,4)`,
		ulid.Make().String(), userID.String(), contactID.String(), sender)

	fx := NewStoreExtractor(st, userID)
	got, err := fx.Extract(ctx, msgID)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	wantPresent := []string{
		"from:alice@ext.com", "dom:ext.com", "corr:1", "sent:3-5",
		"reply:1", "hour:9", "attach:1", "alias:me+tag@example.com", "flagged:1",
		"tocc:0", // recipients are not on the row, so replay sees zero
	}
	for _, k := range wantPresent {
		if got[k] != 1.0 {
			t.Errorf("missing feature %q (got keys %v)", k, sortedKeys(got))
		}
	}
	// List-Id / List-Unsubscribe are not projected onto the row, so the replay
	// path cannot see them — they must be ABSENT, not guessed.
	for _, k := range []string{"list:1", "unsub:1"} {
		if _, ok := got[k]; ok {
			t.Errorf("feature %q present, but the row cannot carry list headers", k)
		}
	}
	// The subject tokenizes.
	for _, k := range []string{"subtok:re", "subtok:q3", "subtok:budget"} {
		if got[k] != 1.0 {
			t.Errorf("missing subject token %q", k)
		}
	}

	// A missing message is an error the eval harness treats as skip.
	if _, err := fx.Extract(ctx, ulid.Make()); err == nil {
		t.Error("Extract of a nonexistent message should error")
	}
}
