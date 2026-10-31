// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Where a private key came from (#54, migration 00039).
//
// The property under test is narrow and the whole point of the column: the
// three states must stay THREE. A generated key is the only copy and destroying
// it is irreversible; an imported key exists elsewhere; an unrecorded one is
// unknown, and unknown is the cautious case, not the safe one. Any code that
// collapses unrecorded into imported has silently decided a delete is harmless
// on no evidence.

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestKeyOriginRoundTripsAsThreeDistinctStates.
//
// ⚠ THE UNRECORDED CASE IS THE ONE WORTH THE TEST. The other two are ordinary
// round-trips; this one asserts that a SQL NULL survives the scan as "" rather
// than becoming one of the two real values, which is exactly what a non-pointer
// scan target would have done silently.
func TestKeyOriginRoundTripsAsThreeDistinctStates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	cases := []struct {
		name string
		want KeyOrigin
	}{
		{"generated", KeyGenerated},
		{"imported", KeyImported},
		{"unrecorded", KeyOriginUnrecorded},
	}

	ids := make(map[string]ulid.ULID, len(cases))
	for _, c := range cases {
		id := NewULID()
		ids[c.name] = id
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			_, e := tx.InsertUserKey(ctx, UserKey{
				ID:           id,
				UserID:       aID,
				Kind:         KeyPGP,
				Fingerprint:  "ORIGIN" + strings.ToUpper(c.name) + id.String(),
				PublicKey:    []byte{0x98, 0x33, 0x04}, // binary packets, not armor
				PrivateKeyCT: []byte("sealed-" + c.name),
				Origin:       c.want,
			})
			return e
		}); err != nil {
			t.Fatalf("insert %s: %v", c.name, err)
		}
	}

	for _, c := range cases {
		var got UserKey
		var found bool
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			got, found, e = tx.GetUserKey(ctx, ids[c.name])
			return e
		}); err != nil {
			t.Fatalf("get %s: %v", c.name, err)
		}
		if !found {
			t.Fatalf("%s: key not found", c.name)
		}
		if got.Origin != c.want {
			t.Errorf("%s: origin round-tripped as %q, want %q", c.name, got.Origin, c.want)
		}
	}

	// The database's own view: the unrecorded row must be SQL NULL, not the
	// empty string. An empty string would satisfy the Go assertion above while
	// violating the CHECK's intent and reading as a fourth state in SQL.
	var isNull bool
	if err := h.super.QueryRow(ctx,
		`SELECT origin IS NULL FROM app.user_keys WHERE id = $1`,
		ids["unrecorded"].String()).Scan(&isNull); err != nil {
		t.Fatalf("null check: %v", err)
	}
	if !isNull {
		t.Error("an unrecorded origin was stored as something other than NULL")
	}
}

// TestKeyOriginRejectsAnythingButTheTwoKnownValues proves the CHECK constraint
// is doing work — a typo becomes a loud failure rather than a fourth silent
// category that every reader would then mis-handle.
func TestKeyOriginRejectsAnythingButTheTwoKnownValues(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	_, err := h.super.Exec(ctx, `
		INSERT INTO app.user_keys (id, user_id, kind, fingerprint, public_key, origin)
		VALUES ($1, $2, 'pgp', $3, '\x983304'::bytea, 'genrated')`,
		NewULID().String(), a.ID, "TYPO"+NewULID().String())
	if err == nil {
		t.Fatal("a misspelled origin was accepted; the CHECK constraint is not " +
			"constraining, and every reader's switch would fall through to its " +
			"default — which is the unknown case, silently")
	}
	if !strings.Contains(err.Error(), "user_keys_origin_known") {
		t.Errorf("rejected, but not by the named constraint: %v", err)
	}
}

// TestListUserKeysReportsOriginPerKey is what the delete-user report reads, and
// it must not need a second query to get it.
func TestListUserKeysReportsOriginPerKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	gen := NewULID()
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, e := tx.InsertUserKey(ctx, UserKey{
			ID: gen, UserID: aID, Kind: KeyPGP,
			Fingerprint:  "LISTGEN" + gen.String(),
			PublicKey:    []byte{0x98, 0x33, 0x04},
			PrivateKeyCT: []byte("sealed"),
			Origin:       KeyGenerated,
		})
		return e
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var keys []UserKey
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		keys, e = tx.ListUserKeys(ctx)
		return e
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	var seen bool
	for _, k := range keys {
		if k.ID.Compare(gen) == 0 {
			seen = true
			if k.Origin != KeyGenerated {
				t.Errorf("listed origin = %q, want %q", k.Origin, KeyGenerated)
			}
		}
	}
	if !seen {
		t.Fatal("the generated key did not appear in ListUserKeys, so this test " +
			"asserted nothing about the list path")
	}
}
