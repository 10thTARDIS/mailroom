// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oklog/ulid/v2"
)

// #47 increment 6b: the decrypt trail is per-user AND append-only for the role
// both processes share.
//
// The generic RLS suite (rls_test.go, via appTables) covers visibility. What it
// cannot pin is the property this table exists for: a compromised application —
// which connects as mailroom_app, exactly like the crypto process that writes
// these rows — must not be able to ERASE or REWRITE a decryption, even its own
// user's. That is a grant, and only a test that tries the write can see a grant
// go missing (the generic suite accepts "zero rows affected" for DELETE, which a
// granted DELETE filtered by RLS also produces — so it cannot fail here).
func TestPGPDecryptAuditIsAppendOnlyAndIsolated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := seedUser(t, h.super, "pda-"+NewULID().String())
	b := seedUser(t, h.super, "pdb-"+NewULID().String())
	st := NewFromPool(h.app)
	aID, bID := ulid.MustParse(a.ID), ulid.MustParse(b.ID)
	aMsg := ulid.MustParse(a.MessageID)
	since := time.Now().Add(-time.Hour)

	// The round trip, and the summary the operator reconstructs from it.
	for i, rec := range []PGPDecryptAudit{
		{MessageID: aMsg, Purpose: "body", Decrypted: true, Fingerprint: "aa11"},
		{MessageID: aMsg, Purpose: "attachment", Decrypted: true, Fingerprint: "AA11"},
		{MessageID: NewULID(), Purpose: "body", Decrypted: false},
	} {
		if err := st.UserTx(ctx, aID, func(tx *UserTx) error { return tx.RecordPGPDecrypt(ctx, rec) }); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	var sum PGPDecryptSummary
	if err := st.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		sum, err = tx.PGPDecryptSummary(ctx, since)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The fixture seeds one 'decrypted' row for A's message too, so: 3 decrypted
	// calls over ONE distinct message, and 1 failure.
	if sum.Calls["decrypted"] != 3 || sum.Calls["failed"] != 1 || sum.Messages != 1 {
		t.Errorf("summary = %+v, want 3 decrypted calls over 1 message and 1 failure", sum)
	}

	// A successful decrypt must name its key; the store refuses otherwise, and the
	// CHECK constraint would too.
	err := st.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.RecordPGPDecrypt(ctx, PGPDecryptAudit{MessageID: aMsg, Purpose: "body", Decrypted: true})
	})
	if !errors.Is(err, ErrDecryptAuditInvalid) {
		t.Errorf("a successful decrypt with no key fingerprint: err = %v, want ErrDecryptAuditInvalid", err)
	}

	// B sees none of A's trail.
	if err := st.UserTx(ctx, bID, func(tx *UserTx) error {
		var n int
		if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.pgp_decrypt_audit WHERE user_id = $1`, a.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("B can see %d of A's decrypt rows", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// B cannot write a row in A's name.
	assertSQLState(t, st, bID, "insert as A",
		`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint)
		 VALUES ($1, $2, $3, 'body', 'failed', NULL)`, NewULID().String(), a.ID, a.MessageID)

	// ── THE PROPERTY: append-only for mailroom_app, on the user's OWN rows. ──
	// 42501 specifically: a policy-filtered zero-row UPDATE/DELETE would also
	// "erase nothing" against another user's rows, but against your own rows
	// only a missing grant stops it.
	for _, stmt := range []string{
		`DELETE FROM app.pgp_decrypt_audit WHERE user_id = $1`,
		`UPDATE app.pgp_decrypt_audit SET outcome = 'failed', key_fingerprint = NULL WHERE user_id = $1`,
	} {
		err := st.UserTx(ctx, aID, func(tx *UserTx) error {
			_, err := tx.tx.Exec(ctx, stmt, a.ID)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != sqlstateInsufficientPrivilege {
			t.Errorf("A could run %q on its own decrypt trail (err %v): a compromised application "+
				"could then erase the record of what it decrypted", stmt, err)
		}
	}
	// Control on the assertion above: the rows are still there.
	var n int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.pgp_decrypt_audit WHERE user_id = $1`, a.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("A's trail holds %d rows after the refused erase attempts, want 4", n)
	}

	// No change_log rows from the trail: it is not a JMAP-visible object.
	var before, after int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&before)
	_ = st.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.RecordPGPDecrypt(ctx, PGPDecryptAudit{MessageID: aMsg, Purpose: "body"})
	})
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&after)
	if before != after {
		t.Errorf("RecordPGPDecrypt appended %d change_log rows", after-before)
	}
}
