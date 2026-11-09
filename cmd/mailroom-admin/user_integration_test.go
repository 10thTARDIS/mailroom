// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// `user disable` / `user enable` through userCore, against a real database: what
// each prints, that running either twice is safe, that the last enabled user is
// refused (non-zero exit, nothing changed), and that an unknown user is an error.
func TestUserDisableEnableCommand(t *testing.T) {
	h := newDeleteUserHarness(t)
	ctx := context.Background()
	tag := strings.ToLower(store.NewULID().String())
	a := seedDeleteUserFixture(t, h.super, "ucmd-a-"+tag)
	seedDeleteUserFixture(t, h.super, "ucmd-b-"+tag)

	var out bytes.Buffer
	prev := reportOut
	reportOut = &out
	t.Cleanup(func() { reportOut = prev })
	run := func(sub, ref string) (string, error) {
		out.Reset()
		err := userCore(ctx, h.store, sub, ref)
		return out.String(), err
	}
	disabled := func() bool {
		var d bool
		if err := h.super.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM app.users WHERE id = $1`, a.String()).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	got, err := run("disable", a.String())
	if err != nil || !strings.Contains(got, "disabled ucmd-a-") || !disabled() {
		t.Fatalf("disable: %q err=%v disabled=%v", got, err, disabled())
	}
	got, err = run("disable", "ucmd-a-"+tag+"@example.com")
	if err != nil || !strings.Contains(got, "was already disabled; nothing changed") {
		t.Fatalf("second disable (by email): %q err=%v", got, err)
	}
	got, err = run("enable", a.String())
	if err != nil || !strings.Contains(got, "enabled ucmd-a-") || disabled() {
		t.Fatalf("enable: %q err=%v disabled=%v", got, err, disabled())
	}
	got, err = run("enable", a.String())
	if err != nil || !strings.Contains(got, "was already enabled; nothing changed") {
		t.Fatalf("second enable: %q err=%v", got, err)
	}

	if _, err := run("disable", "nobody-"+tag+"@example.com"); err == nil || !strings.Contains(err.Error(), "no such user") {
		t.Fatalf("unknown user: err=%v", err)
	}
	if _, err := run("frobnicate", a.String()); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}

	// The last enabled user: make A the only one, then try.
	rows, err := h.super.Query(ctx, `SELECT id FROM app.users WHERE id <> $1 AND disabled_at IS NULL AND deleted_at IS NULL`, a.String())
	if err != nil {
		t.Fatal(err)
	}
	var others []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		others = append(others, id)
	}
	rows.Close()
	if _, err := h.super.Exec(ctx, `UPDATE app.users SET disabled_at = now() WHERE id = ANY($1)`, others); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `UPDATE app.users SET disabled_at = NULL WHERE id = ANY($1)`, others)
	})
	got, err = run("disable", a.String())
	if err == nil || !strings.Contains(err.Error(), "last enabled user") || !strings.Contains(got, "refused") {
		t.Fatalf("last enabled user: out=%q err=%v", got, err)
	}
	if disabled() {
		t.Fatal("the last enabled user was disabled")
	}
}
