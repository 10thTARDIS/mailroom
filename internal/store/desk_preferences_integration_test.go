// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the desk's two settings surfaces (#442 increment 1,
// migration 00046): default materialisation with a created change row, an
// actual update with exactly one updated row, a no-op with none, and an
// out-of-range value surfacing as a typed error without poisoning the
// transaction.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestDeskAndClientPreferencesMaterialiseAndUpdate(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "prefs-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// The fixture seeds one row of each directly (so the RLS inventory has
	// one); delete both so the store's OWN materialisation path — not the
	// fixture — is what this test actually exercises.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.desk_preferences WHERE user_id = $1`, u.ID); err != nil {
		t.Fatalf("clear fixture desk preferences: %v", err)
	}
	if _, err := h.super.Exec(ctx, `DELETE FROM app.client_preferences WHERE user_id = $1`, u.ID); err != nil {
		t.Fatalf("clear fixture client preferences: %v", err)
	}

	// --- Desk preferences ---
	var deskBefore, deskAfterGet, deskAfterUpdate, deskAfterNoop int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if deskBefore, e = tx.ChangeState(ctx, ObjectDeskPreferences); e != nil {
			return e
		}
		got, e := tx.GetDeskPreferences(ctx)
		if e != nil {
			return e
		}
		if got.LineThreshold != 0.5 || got.DrawerDefaultDays != 30 || got.TrayNagDays != 7 {
			t.Errorf("materialised defaults = %+v, want the schema defaults", got)
		}
		if got.DrawerSweepEnabled {
			t.Error("DrawerSweepEnabled materialised true, want false — the drawer sweep's master " +
				"switch (migration 00048, #443) defaults OFF")
		}
		deskAfterGet, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("get desk preferences: %v", err)
	}
	if deskAfterGet-deskBefore != 1 {
		t.Errorf("materialising desk preferences appended %d change rows, want 1", deskAfterGet-deskBefore)
	}

	newThreshold := float32(0.7)
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{LineThreshold: &newThreshold})
	}); err != nil {
		t.Fatalf("update desk preferences: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		deskAfterUpdate, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if deskAfterUpdate-deskAfterGet != 1 {
		t.Errorf("update appended %d change rows, want 1", deskAfterUpdate-deskAfterGet)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{LineThreshold: &newThreshold})
	}); err != nil {
		t.Fatalf("no-op update desk preferences: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		deskAfterNoop, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if deskAfterNoop != deskAfterUpdate {
		t.Errorf("a no-op patch appended %d change rows, want 0", deskAfterNoop-deskAfterUpdate)
	}

	badThreshold := float32(2.0)
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		e := tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{LineThreshold: &badThreshold})
		if !errors.Is(e, ErrDeskPreferencesInvalid) {
			return fmt.Errorf("out-of-range threshold: %v, want ErrDeskPreferencesInvalid", e)
		}
		// The transaction must still be usable: a real read in the SAME tx.
		_, e = tx.GetDeskPreferences(ctx)
		return e
	}); err != nil {
		t.Fatalf("invalid value recovery: %v", err)
	}

	// DrawerSweepEnabled round-trips the same way: an update flipping it
	// appends exactly one DeskPreferences updated row, and a no-op appends
	// none — the negative control that the master switch is a real,
	// change_log-visible field and not decoration (mailroom#443).
	sweepOn := true
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{DrawerSweepEnabled: &sweepOn})
	}); err != nil {
		t.Fatalf("enable drawer sweep: %v", err)
	}
	var deskAfterSweepOn int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		deskAfterSweepOn, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if deskAfterSweepOn-deskAfterNoop != 1 {
		t.Errorf("enabling the drawer sweep appended %d change rows, want 1", deskAfterSweepOn-deskAfterNoop)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		got, e := tx.GetDeskPreferences(ctx)
		if e != nil {
			return e
		}
		if !got.DrawerSweepEnabled {
			t.Error("DrawerSweepEnabled after enabling = false, want true")
		}
		return nil
	}); err != nil {
		t.Fatalf("read desk preferences: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{DrawerSweepEnabled: &sweepOn})
	}); err != nil {
		t.Fatalf("no-op update drawer sweep: %v", err)
	}
	var deskAfterSweepNoop int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		deskAfterSweepNoop, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if deskAfterSweepNoop != deskAfterSweepOn {
		t.Errorf("a no-op DrawerSweepEnabled patch appended %d change rows, want 0", deskAfterSweepNoop-deskAfterSweepOn)
	}

	// --- Client preferences ---
	var clientBefore, clientAfterGet, clientAfterUpdate, clientAfterNoop int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if clientBefore, e = tx.ChangeState(ctx, ObjectClientPreferences); e != nil {
			return e
		}
		got, e := tx.GetClientPreferences(ctx, ClientWeb)
		if e != nil {
			return e
		}
		if got.Theme != "system" || got.Density != "normal" || got.MessageOpens != "beside" || got.PaneSplitPct != 58 {
			t.Errorf("materialised defaults = %+v, want the schema defaults", got)
		}
		clientAfterGet, e = tx.ChangeState(ctx, ObjectClientPreferences)
		return e
	}); err != nil {
		t.Fatalf("get client preferences: %v", err)
	}
	if clientAfterGet-clientBefore != 1 {
		t.Errorf("materialising client preferences appended %d change rows, want 1", clientAfterGet-clientBefore)
	}

	newTheme := "dark"
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpdateClientPreferences(ctx, ClientWeb, ClientPreferencesPatch{Theme: &newTheme})
	}); err != nil {
		t.Fatalf("update client preferences: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		clientAfterUpdate, e = tx.ChangeState(ctx, ObjectClientPreferences)
		return e
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if clientAfterUpdate-clientAfterGet != 1 {
		t.Errorf("update appended %d change rows, want 1", clientAfterUpdate-clientAfterGet)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpdateClientPreferences(ctx, ClientWeb, ClientPreferencesPatch{Theme: &newTheme})
	}); err != nil {
		t.Fatalf("no-op update client preferences: %v", err)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		clientAfterNoop, e = tx.ChangeState(ctx, ObjectClientPreferences)
		return e
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if clientAfterNoop != clientAfterUpdate {
		t.Errorf("a no-op patch appended %d change rows, want 0", clientAfterNoop-clientAfterUpdate)
	}

	badPct := 5
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		e := tx.UpdateClientPreferences(ctx, ClientWeb, ClientPreferencesPatch{PaneSplitPct: &badPct})
		if !errors.Is(e, ErrClientPreferencesInvalid) {
			return fmt.Errorf("out-of-range pane split: %v, want ErrClientPreferencesInvalid", e)
		}
		_, e = tx.GetClientPreferences(ctx, ClientWeb)
		return e
	}); err != nil {
		t.Fatalf("invalid value recovery: %v", err)
	}
}
