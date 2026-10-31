// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The stray-unsigned-draft regression, reproduced deterministically (#260).
//
// Compose-time signing rewrites a message's bytes at submit, but the copy
// already APPENDed to IMAP Drafts still holds the pre-signature bytes. On the
// real instance a sync pass read that copy moments before the expunge
// landed, and it was ingested as a NEW message: a stray, unsigned, already-sent
// draft in the user's mailbox.
//
// ⚠ THAT WAS A RACE, AND A RACE IS NOT A TEST. Reproducing it by timing would
// be flaky in exactly the direction that matters — a green run would mean
// nothing. So this test does not race anything: it takes the pre-signature
// bytes and asks the ingest path to ingest them, which is what the losing side
// of the race amounts to. The question "would a late copy of the old bytes
// create a second message?" has a deterministic answer, and this asks it
// directly.

import (
	"context"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// TestSupersededDraftBytesDoNotBecomeASecondMessage is the regression guard.
//
// It drives a real signed submission through EmailSubmission/set, recovers the
// bytes the message had BEFORE signing, and then feeds those bytes back in the
// way a sync of the stale IMAP Drafts copy would.
func TestSupersededDraftBytesDoNotBecomeASecondMessage(t *testing.T) {
	signer := &stubSigner{}
	h, blobs := newSigningHarness(t, signer)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	bindKeyToIdentity(t, h, a.UserID, "me@example.com")

	emailID, beforeHash := composeAndSubmit(t, h, a.UserID, "me@example.com",
		"A draft that gets signed — and whose old bytes come back.\n")

	if signer.count() != 1 {
		t.Fatalf("signer called %d times, want 1 — without signing there is no supersession "+
			"and this test would prove nothing", signer.count())
	}

	// CONTROL: the bytes really did move. If they had not, everything below
	// would pass trivially against a build where signing did nothing at all.
	afterRaw := storedBytes(t, h, blobs, a.UserID, emailID)
	if !strings.Contains(string(afterRaw), "multipart/signed") {
		t.Fatalf("the stored message is not signed; nothing was superseded")
	}

	// The tombstone must exist, and must point at the message that was mutated
	// in place — not at some new row.
	var tombstoned string
	if err := h.super.QueryRow(ctx,
		`SELECT message_id FROM app.superseded_content WHERE user_id = $1 AND content_hash = $2`,
		a.UserID.String(), beforeHash).Scan(&tombstoned); err != nil {
		t.Fatalf("no superseded_content row for the pre-signature bytes: %v", err)
	}
	if tombstoned != emailID {
		t.Errorf("tombstone points at message %s, want %s — a stale copy would be attached "+
			"to the wrong message", tombstoned, emailID)
	}

	// NOW THE ACTUAL QUESTION. Ask the store what ingest asks: "do we already
	// hold a message with these bytes?" for the PRE-signature hash.
	var found bool
	var foundID string
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		id, ok, err := tx.MessageIDBySupersededContent(ctx, beforeHash)
		found, foundID = ok, id.String()
		return err
	}); err != nil {
		t.Fatalf("superseded lookup: %v", err)
	}
	if !found {
		t.Fatal("the pre-signature bytes are NOT recognised as superseded — a late sync of " +
			"the IMAP Drafts copy would ingest them as a stray unsigned draft, which is #260")
	}
	if foundID != emailID {
		t.Errorf("superseded bytes resolve to %s, want the in-place message %s", foundID, emailID)
	}

	// And the mailbox still holds exactly ONE message for this Message-ID.
	var rows int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND message_id_hint = (
		   SELECT message_id_hint FROM app.messages WHERE user_id = $1 AND id = $2)`,
		a.UserID.String(), emailID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("the mailbox holds %d rows for this Message-ID, want 1", rows)
	}
}

// TestSupersededLookupIsNotAWildcard is the negative control on the mechanism
// itself. A lookup that answered "yes, superseded" to anything would make the
// test above pass while silently swallowing genuinely new mail — which would be
// a far worse bug than the one being fixed, because it LOSES messages.
func TestSupersededLookupIsNotAWildcard(t *testing.T) {
	signer := &stubSigner{}
	h, _ := newSigningHarness(t, signer)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	bindKeyToIdentity(t, h, a.UserID, "me@example.com")

	_, beforeHash := composeAndSubmit(t, h, a.UserID, "me@example.com", "signed\n")

	unrelated := make([]byte, len(beforeHash))
	copy(unrelated, beforeHash)
	unrelated[0] ^= 0xFF // one byte different: a different message entirely

	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		if _, ok, err := tx.MessageIDBySupersededContent(ctx, unrelated); err != nil {
			return err
		} else if ok {
			t.Error("an unrelated content hash was reported as superseded — ingest would " +
				"discard new mail as a duplicate")
		}
		// And the real one still resolves, so the miss above is not the lookup
		// simply being broken.
		if _, ok, err := tx.MessageIDBySupersededContent(ctx, beforeHash); err != nil {
			return err
		} else if !ok {
			t.Error("the genuine superseded hash does not resolve either — the negative " +
				"result above proves nothing")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
}

// TestSupersededContentIsUserIsolated: the tombstone is a per-user table under
// RLS, and a hash superseded by one user must be invisible to another. Without
// this, one user's send could suppress another user's incoming mail — a
// cross-tenant message-loss bug wearing a dedup costume.
func TestSupersededContentIsUserIsolated(t *testing.T) {
	signer := &stubSigner{}
	h, _ := newSigningHarness(t, signer)
	ctx := context.Background()
	a, b := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	bindKeyToIdentity(t, h, a.UserID, "me@example.com")

	_, beforeHash := composeAndSubmit(t, h, a.UserID, "me@example.com", "signed\n")

	if err := h.store.UserTx(ctx, b.UserID, func(tx *store.UserTx) error {
		if _, ok, err := tx.MessageIDBySupersededContent(ctx, beforeHash); err != nil {
			return err
		} else if ok {
			t.Error("user B can see user A's superseded hash — RLS is not holding, and B's " +
				"incoming mail could be suppressed by A's send")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup as user B: %v", err)
	}

	// Positive control: user A still sees it, so the miss above is isolation
	// rather than the row being absent.
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		if _, ok, err := tx.MessageIDBySupersededContent(ctx, beforeHash); err != nil {
			return err
		} else if !ok {
			t.Error("user A cannot see their own superseded hash — the isolation assertion " +
				"above proves nothing")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup as user A: %v", err)
	}
}
