// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

// The store refuses an IMAP system flag as a keyword (#49 review, F1), before
// any write, whichever caller built the update: `\Draft` as a keyword becomes
// `UID STORE +FLAGS (\Draft)`, and the next sync reads it back as is_draft.
//
// RED PROBE: remove the KeywordWritable loops in applyEmailUpdate and the
// first half fails — enqueueEmailIntent still refuses, but only after the
// keywords column was written, and Email/set commits what came before an
// object's error; remove the check in enqueueEmailIntent and the second half
// fails with the intent queued.
func TestStoreRefusesSystemFlagKeywords(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	uid, msg, acct := mustULID(t, a.ID), mustULID(t, a.MessageID), mustULID(t, a.AccountID)
	if _, err := h.super.Exec(ctx, `DELETE FROM app.writeback_intents WHERE user_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	intents := func() int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.writeback_intents
			WHERE user_id = $1 AND op IN ('+kw','-kw')`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	keywords := func() []string {
		t.Helper()
		var k []string
		if err := h.super.QueryRow(ctx, `SELECT keywords FROM app.messages WHERE id = $1`, a.MessageID).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}

	before := keywords()
	for _, up := range []EmailUpdate{
		{AddKeywords: []string{`\Draft`}},
		{AddKeywords: []string{"ok", `\Seen`}},
		{RemoveKeywords: []string{`\Deleted`}},
	} {
		// The error is swallowed and the transaction COMMITTED, as Email/set
		// does when it reports one object's failure and carries on with the
		// next: a refusal that came after a write would leave that write behind.
		var applyErr error
		if err := h.appStore().UserTx(ctx, uid, func(tx *UserTx) error {
			_, _, applyErr = tx.ApplyEmailUpdate(ctx, msg, up)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(applyErr, ErrInvalidKeyword) {
			t.Errorf("ApplyEmailUpdate(%+v) = %v, want ErrInvalidKeyword", up, applyErr)
		}
		if n := intents(); n != 0 {
			t.Fatalf("ApplyEmailUpdate(%+v) queued %d keyword intents", up, n)
		}
		if kws := keywords(); !equalSeq(kws, before) {
			t.Fatalf("ApplyEmailUpdate(%+v) wrote keywords %v before refusing", up, kws)
		}
	}

	// The enqueue point itself, for a path that does not come through the
	// AddKeywords check (label atoms, the junk verdict's tokens).
	err := h.appStore().UserTx(ctx, uid, func(tx *UserTx) error {
		return tx.enqueueEmailIntent(ctx, NewULID(), "", msg, []ulid.ULID{acct}, IntentAddKeyword,
			map[string]string{"keyword": `\Draft`})
	})
	if !errors.Is(err, ErrInvalidKeyword) || intents() != 0 {
		t.Fatalf("enqueueEmailIntent(+kw \\Draft) = %v with %d intents, want ErrInvalidKeyword and none", err, intents())
	}

	// Positive control: an ordinary keyword is queued.
	if err := h.appStore().UserTx(ctx, uid, func(tx *UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msg, EmailUpdate{AddKeywords: []string{"MyTag"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := intents(); n != 1 {
		t.Fatalf("an ordinary keyword queued %d intents, want 1: the refusals above prove nothing", n)
	}
}

// A raw keyword never carries a reserved meaning in another casing (#49
// review). RED PROBE: drop the KeywordReserved calls in applyEmailUpdate and
// a "+kw $JUNK" intent is queued.
func TestStoreRefusesReservedKeywordsInAnyCase(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	uid, msg := mustULID(t, a.ID), mustULID(t, a.MessageID)
	if _, err := h.super.Exec(ctx, `DELETE FROM app.writeback_intents WHERE user_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, up := range []EmailUpdate{
		{AddKeywords: []string{"$JUNK"}},
		{AddKeywords: []string{"junk"}},
		{AddKeywords: []string{"$Draft"}},
		{RemoveKeywords: []string{"$NotJunk"}},
	} {
		err := h.appStore().UserTx(ctx, uid, func(tx *UserTx) error {
			_, _, err := tx.ApplyEmailUpdate(ctx, msg, up)
			return err
		})
		if !errors.Is(err, ErrInvalidKeyword) {
			t.Errorf("ApplyEmailUpdate(%v %v) = %v, want ErrInvalidKeyword", up.AddKeywords, up.RemoveKeywords, err)
		}
	}
	var n int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND op IN ('+kw','-kw')`, a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("keyword intents = %d (%v), want none", n, err)
	}
}
