// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// THE STORE IS THE BACKSTOP FOR NAMES: whatever path a label or area name
// arrives by, the saved name carries no control or bidi character, and a name
// with nothing usable left is refused. Creates and renames both.
//
// NEGATIVE CONTROL: with cleanName's SanitizeName removed, every "saved clean"
// assertion fails (run and recorded in the PR).
func TestStoreSanitisesLabelAndAreaNames(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "names-"+NewULID().String())
	userID := mustULID(t, u.ID)
	dirty := "Bills\u001b[2J\u202eevil"
	const clean = "Bills[2Jevil"

	var labelID, areaID string
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		l, err := tx.CreateLabel(ctx, LabelRow{Name: dirty, MirrorToIMAP: false})
		if err != nil {
			return err
		}
		labelID = l.String()
		a, err := tx.CreateArea(ctx, AreaRow{Name: dirty + " area", Position: 90})
		areaID = a.String()
		return err
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	read := func(sql, id string) string {
		var n string
		if err := h.super.QueryRow(ctx, sql, u.ID, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := read(`SELECT name FROM app.labels WHERE user_id = $1 AND id = $2`, labelID); got != clean {
		t.Errorf("label saved as %q, want %q", got, clean)
	}
	if got := read(`SELECT name FROM app.areas WHERE user_id = $1 AND id = $2`, areaID); got != clean+" area" {
		t.Errorf("area saved as %q, want %q", got, clean+" area")
	}

	// Renames.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		l := mustULID(t, labelID)
		if _, err := tx.RenameLabel(ctx, l, "Re\u202ename"); err != nil {
			return err
		}
		n := "Ar\u001bea2"
		_, err := tx.UpdateArea(ctx, mustULID(t, areaID), AreaPatch{Name: &n})
		return err
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := read(`SELECT name FROM app.labels WHERE user_id = $1 AND id = $2`, labelID); got != "Rename" {
		t.Errorf("label renamed to %q, want Rename", got)
	}
	if got := read(`SELECT name FROM app.areas WHERE user_id = $1 AND id = $2`, areaID); got != "Area2" {
		t.Errorf("area renamed to %q, want Area2", got)
	}

	// Nothing usable left: refused, on every path, and nothing is saved.
	only := "\u202e\u0007 "
	for name, fn := range map[string]func(tx *UserTx) error{
		"CreateLabel": func(tx *UserTx) error { _, e := tx.CreateLabel(ctx, LabelRow{Name: only}); return e },
		"CreateArea":  func(tx *UserTx) error { _, e := tx.CreateArea(ctx, AreaRow{Name: only, Position: 91}); return e },
		"RenameLabel": func(tx *UserTx) error { _, e := tx.RenameLabel(ctx, mustULID(t, labelID), only); return e },
		"UpdateArea": func(tx *UserTx) error {
			_, e := tx.UpdateArea(ctx, mustULID(t, areaID), AreaPatch{Name: &only})
			return e
		},
	} {
		err := s.UserTx(ctx, userID, fn)
		if !errors.Is(err, ErrNameInvalid) {
			t.Errorf("%s with an only-unsafe name: %v, want ErrNameInvalid", name, err)
		}
	}
	var n int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.labels WHERE user_id = $1 AND name !~ '^[^[:cntrl:]]+$'`, u.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || strings.Contains(read(`SELECT name FROM app.labels WHERE user_id = $1 AND id = $2`, labelID), "\u202e") {
		t.Errorf("a label with a control character was saved")
	}
}
