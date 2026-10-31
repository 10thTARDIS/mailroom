// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// app.pgp_root_check (#525): what the crypto process's boot check stands on.
//
// The crypto process shares mailroom_app with the server, so what stops the
// application from rewriting the recorded root is the grant and the policies,
// not the role. These tests make that claim against the executor rather than
// reading it off the schema. scripts/integration-test.sh applies the schema as
// mailroom_owner, so mailroom_app's privileges here are the ones a deployment
// has — the owner-privilege blindness behind #48 and #257 does not apply.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// rootCheckReset empties the table as the superuser — the only role in these
// tests that can, which is itself one of the properties under test — and does
// so again on cleanup.
//
// ⚠ NO ROW MAY OUTLIVE A TEST. There is one row per database, so a leftover
// one silently changes the next test's premise: cmd/mailroom-crypto's boot
// test would begin its "fresh install" case with a root already recorded.
func rootCheckReset(t *testing.T, h *harness) {
	t.Helper()
	wipe := func() error {
		_, err := h.super.Exec(context.Background(), `DELETE FROM app.pgp_root_check`)
		return err
	}
	if err := wipe(); err != nil {
		t.Fatalf("reset app.pgp_root_check: %v", err)
	}
	t.Cleanup(func() {
		if err := wipe(); err != nil {
			t.Errorf("clean up app.pgp_root_check: %v", err)
		}
	})
}

// rootCheckRecorded reads the table as the superuser: an observation that does
// not pass through the code under test.
func rootCheckRecorded(t *testing.T, h *harness) (rows int, value []byte) {
	t.Helper()
	ctx := context.Background()
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.pgp_root_check`).Scan(&rows); err != nil {
		t.Fatalf("count app.pgp_root_check: %v", err)
	}
	if rows == 1 {
		if err := h.super.QueryRow(ctx, `SELECT check_value FROM app.pgp_root_check`).Scan(&value); err != nil {
			t.Fatalf("read app.pgp_root_check: %v", err)
		}
	}
	return rows, value
}

func rootCheckBytes(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func rootCheckEnsure(t *testing.T, s *Store, value []byte) (PGPRootCheck, bool) {
	t.Helper()
	var (
		rec      PGPRootCheck
		inserted bool
	)
	err := s.SystemTx(context.Background(), func(tx *SystemTx) error {
		var err error
		rec, inserted, err = tx.EnsurePGPRootCheck(context.Background(),
			PGPRootCheck{Info: "test-scheme", Value: value})
		return err
	})
	if err != nil {
		t.Fatalf("EnsurePGPRootCheck: %v", err)
	}
	return rec, inserted
}

// rootCheckSystemExec runs one statement as mailroom_app with NO identity — the
// one context in which the table's policies admit its row, so a refusal there
// can only come from something other than the policies.
func rootCheckSystemExec(s *Store, sql string, args ...any) error {
	ctx := context.Background()
	return s.SystemTx(ctx, func(tx *SystemTx) error {
		_, err := tx.tx.Exec(ctx, sql, args...)
		return err
	})
}

// TestEnsurePGPRootCheckRecordsOnceAndNeverReplaces: the first value wins, and
// a later, different candidate gets the recorded one back untouched — which is
// what lets the crypto process refuse it instead of adopting it.
func TestEnsurePGPRootCheckRecordsOnceAndNeverReplaces(t *testing.T) {
	h := newHarness(t)
	rootCheckReset(t, h)
	s := h.appStore()

	first, inserted := rootCheckEnsure(t, s, rootCheckBytes(0x11))
	if !inserted {
		t.Fatal("an empty table did not record the first candidate")
	}
	if !bytes.Equal(first.Value, rootCheckBytes(0x11)) || first.Info != "test-scheme" || first.RecordedAt.IsZero() {
		t.Errorf("first call returned %+v", first)
	}

	second, inserted := rootCheckEnsure(t, s, rootCheckBytes(0x22))
	if inserted {
		t.Fatal("a second candidate was recorded over the first")
	}
	if !bytes.Equal(second.Value, rootCheckBytes(0x11)) {
		t.Errorf("second call returned %x, want the FIRST value back", second.Value)
	}

	if rows, v := rootCheckRecorded(t, h); rows != 1 || !bytes.Equal(v, rootCheckBytes(0x11)) {
		t.Errorf("the table holds %d row(s) with value %x; want exactly the first value", rows, v)
	}
}

// TestPGPRootCheckIsInvisibleAndUnrecordableUnderAUserIdentity: no request path
// can see the check, or pre-empt it on a fresh database by recording a value of
// its own before the crypto process first boots.
//
// ⚠ THE INSERT IS TRIED AGAINST AN EMPTY TABLE, because that is the case that
// matters. Refused against a table that already held its row, the refusal could
// be the singleton key's rather than the policy's.
func TestPGPRootCheckIsInvisibleAndUnrecordableUnderAUserIdentity(t *testing.T) {
	h := newHarness(t)
	rootCheckReset(t, h)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	assertSQLState(t, s, aID, "user-context INSERT into an empty app.pgp_root_check",
		`INSERT INTO app.pgp_root_check (info, check_value) VALUES ('forged', $1)`, rootCheckBytes(0x33))
	if rows, _ := rootCheckRecorded(t, h); rows != 0 {
		t.Fatalf("a user-context INSERT left %d row(s) behind", rows)
	}

	// Positive control: the row now exists, and the identity-less context can
	// record and see it. Without this, "user A sees nothing" below would pass
	// against an empty table.
	rootCheckEnsure(t, s, rootCheckBytes(0x11))
	if rows, _ := rootCheckRecorded(t, h); rows != 1 {
		t.Fatalf("control: the table holds %d row(s), want 1", rows)
	}

	err := s.UserTx(context.Background(), aID, func(tx *UserTx) error {
		var n int
		if err := tx.tx.QueryRow(context.Background(),
			`SELECT count(*) FROM app.pgp_root_check`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("user A sees %d row(s) of app.pgp_root_check, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPGPRootCheckIsInsertOnlyForTheApplicationRole: the application cannot
// overwrite the recorded root, which would wedge the crypto process, or remove
// it, so that the next boot records whatever root it is given. Both take a
// superuser.
func TestPGPRootCheckIsInsertOnlyForTheApplicationRole(t *testing.T) {
	h := newHarness(t)
	rootCheckReset(t, h)
	s := h.appStore()
	rootCheckEnsure(t, s, rootCheckBytes(0x11))

	for _, c := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE app.pgp_root_check SET check_value = $1`, []any{rootCheckBytes(0x22)}},
		{`DELETE FROM app.pgp_root_check`, nil},
		{`TRUNCATE app.pgp_root_check`, nil},
	} {
		err := rootCheckSystemExec(s, c.sql, c.args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != sqlstateInsufficientPrivilege {
			t.Errorf("mailroom_app: %q: got %v, want SQLSTATE %s", c.sql, err, sqlstateInsufficientPrivilege)
		}
	}

	if rows, v := rootCheckRecorded(t, h); rows != 1 || !bytes.Equal(v, rootCheckBytes(0x11)) {
		t.Errorf("after the refused writes the table holds %d row(s) with value %x; want the original",
			rows, v)
	}
}

// TestPGPRootCheckAdmitsOneWellFormedRow: the singleton key and the length
// CHECK, each refused in the context the policies admit, and followed by a
// positive control — so neither passes merely because the table refuses
// everything.
func TestPGPRootCheckAdmitsOneWellFormedRow(t *testing.T) {
	h := newHarness(t)
	rootCheckReset(t, h)
	s := h.appStore()

	const checkViolation = "23514"
	for _, c := range []struct {
		name string
		sql  string
		args []any
	}{
		{"a 31-byte value",
			`INSERT INTO app.pgp_root_check (info, check_value) VALUES ('s', $1)`,
			[]any{bytes.Repeat([]byte{1}, 31)}},
		{"a row that is not the singleton",
			`INSERT INTO app.pgp_root_check (singleton, info, check_value) VALUES (false, 's', $1)`,
			[]any{rootCheckBytes(0x11)}},
	} {
		err := rootCheckSystemExec(s, c.sql, c.args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != checkViolation {
			t.Errorf("%s: got %v, want SQLSTATE %s", c.name, err, checkViolation)
		}
	}

	if _, inserted := rootCheckEnsure(t, s, rootCheckBytes(0x11)); !inserted {
		t.Error("a well-formed 32-byte value was not recorded — the refusals above may be the table " +
			"refusing everything")
	}
}
