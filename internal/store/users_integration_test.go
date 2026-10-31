// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the boot-time operator surface against a real
// database: ProvisionUser is a get-or-create through the narrow SECURITY DEFINER
// app.provision_user, run as mailroom_app with NO identity set (the boot
// situation). The load-bearing property is IDEMPOTENCE — the server calls it on
// every boot, so a second call with the same subject must return the SAME id and
// leave exactly one row behind. Skips when the integration database is not
// configured.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestProvisionUserIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	// Unique per run so repeated runs against the same database never collide.
	uniq := NewULID().String()
	subject := "sub-" + uniq
	email := uniq + "@example.com"

	first, err := s.ProvisionUser(ctx, subject, email)
	if err != nil {
		t.Fatalf("ProvisionUser (first): %v", err)
	}
	if first == (ulid.ULID{}) {
		t.Fatal("ProvisionUser returned the zero id")
	}

	// Second boot: same subject, a fresh candidate ULID is minted internally and
	// must be discarded in favor of the id already on file.
	second, err := s.ProvisionUser(ctx, subject, email)
	if err != nil {
		t.Fatalf("ProvisionUser (second): %v", err)
	}
	if second != first {
		t.Fatalf("ProvisionUser is not idempotent: second id %s != first id %s", second, first)
	}

	// ...and exactly one row exists. Counted as the superuser: mailroom_app
	// cannot see app.users rows at all without an identity, which is precisely
	// why provisioning needs the definer.
	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.users WHERE oidc_subject = $1`, subject).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Fatalf("app.users rows for subject = %d, want exactly 1", n)
	}

	// The provisioned id is the one login resolves, so the operator that boot
	// created is the operator that can actually log in.
	resolved, found, err := s.UserIDForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("UserIDForSubject: %v", err)
	}
	if !found || resolved != first {
		t.Fatalf("UserIDForSubject = (%s, %t), want (%s, true)", resolved, found, first)
	}

	// The row is usable as an RLS identity immediately (a UserTx can read the
	// profile it just created).
	if err := s.UserTx(ctx, first, func(tx *UserTx) error {
		p, ok, perr := tx.Profile(ctx)
		if perr != nil {
			return perr
		}
		if !ok {
			t.Fatal("Profile: not found for the freshly provisioned operator")
		}
		if p.ID != first {
			t.Fatalf("Profile.ID = %s, want %s", p.ID, first)
		}
		if p.Email != email {
			t.Fatalf("Profile.Email = %q, want %q", p.Email, email)
		}
		return nil
	}); err != nil {
		t.Fatalf("UserTx/Profile: %v", err)
	}
}

// A different subject is a different user: provisioning must not collapse two
// operators onto one row (and the second insert must genuinely happen).
func TestProvisionUserDistinctSubjects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	a := NewULID().String()
	b := NewULID().String()

	idA, err := s.ProvisionUser(ctx, "sub-"+a, a+"@example.com")
	if err != nil {
		t.Fatalf("ProvisionUser A: %v", err)
	}
	idB, err := s.ProvisionUser(ctx, "sub-"+b, b+"@example.com")
	if err != nil {
		t.Fatalf("ProvisionUser B: %v", err)
	}
	if idA == idB {
		t.Fatalf("distinct subjects provisioned to the same id %s", idA)
	}
}

// Fail closed on incomplete input: app.users.email is NOT NULL UNIQUE, so an
// empty email must be rejected before it can reach the database (where it would
// otherwise succeed once and then collide forever after).
func TestProvisionUserRejectsIncompleteInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	if _, err := s.ProvisionUser(ctx, "", "operator@example.com"); err == nil {
		t.Error("ProvisionUser with an empty subject: error = nil, want an error")
	}
	if _, err := s.ProvisionUser(ctx, "sub-"+NewULID().String(), ""); err == nil {
		t.Error("ProvisionUser with an empty email: error = nil, want an error")
	}
}

// ResolveLogin is the OIDC multi-user login path (mailroom#98): it provisions a
// brand-new subject on first login, and on every login refreshes email/display
// name and RE-EVALUATES the role from group membership — so an admin promoted or
// demoted in the identity provider gains or loses admin on their next sign-in.
func TestResolveLoginProvisionsAndReevaluatesRole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	subject := "sub-" + uniq
	email := uniq + "@example.com"

	// First login as an ordinary user.
	id, created, ok, err := s.ResolveLogin(ctx, subject, email, "Ada", false)
	if err != nil || !ok {
		t.Fatalf("ResolveLogin (first) = (%s, %t, %t, %v), want a provisioned user", id, created, ok, err)
	}
	if id == (ulid.ULID{}) {
		t.Fatal("ResolveLogin returned the zero id")
	}
	if !created {
		t.Error("ResolveLogin (first login for a new subject): created = false, want true")
	}
	assertProfile(t, s, id, email, "Ada", "user")

	// Second login: same subject (idempotent id), now a member of the admin group,
	// with a changed email and display name — all must take effect.
	email2 := uniq + "-new@example.com"
	id2, created2, ok, err := s.ResolveLogin(ctx, subject, email2, "Ada L.", true)
	if err != nil || !ok {
		t.Fatalf("ResolveLogin (admin) = (%s, %t, %t, %v)", id2, created2, ok, err)
	}
	if id2 != id {
		t.Fatalf("ResolveLogin minted a new id on re-login: %s != %s", id2, id)
	}
	if created2 {
		t.Error("ResolveLogin (re-login of an existing subject): created = true, want false")
	}
	assertProfile(t, s, id, email2, "Ada L.", "admin")

	// Third login: removed from the admin group — role reverts to user.
	if _, _, ok, err := s.ResolveLogin(ctx, subject, email2, "Ada L.", false); err != nil || !ok {
		t.Fatalf("ResolveLogin (demote) ok=%t err=%v", ok, err)
	}
	assertProfile(t, s, id, email2, "Ada L.", "user")
}

// A disabled or soft-deleted user is authenticated by the provider but must not be
// able to log back in: ResolveLogin returns ok=false and mutates nothing.
func TestResolveLoginDeniesDisabledUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	subject := "sub-" + uniq
	id, _, ok, err := s.ResolveLogin(ctx, subject, uniq+"@example.com", "", false)
	if err != nil || !ok {
		t.Fatalf("ResolveLogin (seed) ok=%t err=%v", ok, err)
	}

	// Disable the user out of band (the admin plane).
	if _, err := h.super.Exec(ctx,
		`UPDATE app.users SET disabled_at = now() WHERE id = $1`, id.String()); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if _, _, ok, err := s.ResolveLogin(ctx, subject, uniq+"@example.com", "", false); err != nil {
		t.Fatalf("ResolveLogin (disabled) unexpected error: %v", err)
	} else if ok {
		t.Fatal("ResolveLogin admitted a disabled user; want ok=false (fail closed)")
	}
}

func TestResolveLoginRejectsIncompleteInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	if _, _, _, err := s.ResolveLogin(ctx, "", "a@example.com", "", false); err == nil {
		t.Error("ResolveLogin with empty subject: want an error")
	}
	if _, _, _, err := s.ResolveLogin(ctx, "sub-"+NewULID().String(), "", "", false); err == nil {
		t.Error("ResolveLogin with empty email: want an error")
	}
}

// --- ErrLoginEmailTaken (Forgejo #574) ---------------------------------------
//
// provisionOperator (boot, cmd/mailroom) and ResolveLogin (login) can disagree
// about which subject the operator's email belongs to — most commonly a
// MAILROOM_OPERATOR_SUBJECT that names the wrong sub. app.users.email is
// citext NOT NULL UNIQUE (constraint users_email_key); these pin BOTH shapes
// that constraint can be hit from upsert_login (the INSERT branch and the
// UPDATE branch) and confirm the failure leaves no partial row behind.

// The INSERT shape: a brand-new subject's email claim already belongs to a
// DIFFERENT existing user. Must refuse with ErrLoginEmailTaken and — the part
// a bare unique_violation could not prove — leave no second row for the new
// subject at all.
func TestResolveLoginRejectsEmailTakenByAnotherUserOnInsert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	email := uniq + "@example.com"
	subjA := "sub-a-" + uniq
	subjB := "sub-b-" + uniq

	if _, _, ok, err := s.ResolveLogin(ctx, subjA, email, "", false); err != nil || !ok {
		t.Fatalf("ResolveLogin (seed subject A) ok=%t err=%v", ok, err)
	}

	_, _, ok, err := s.ResolveLogin(ctx, subjB, email, "", false)
	if !errors.Is(err, ErrLoginEmailTaken) {
		t.Fatalf("ResolveLogin (colliding subject B) err = %v, want ErrLoginEmailTaken", err)
	}
	if ok {
		t.Error("ResolveLogin (colliding subject B): ok = true, want false")
	}

	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.users WHERE oidc_subject = $1`, subjB).Scan(&n); err != nil {
		t.Fatalf("count users for subject B: %v", err)
	}
	if n != 0 {
		t.Fatalf("app.users rows for the refused subject = %d, want 0 (no partial row)", n)
	}
}

// The split branch, pinned as the DELIBERATE counterpart to the collision
// above: two subjects with two DIFFERENT emails are two legitimate, distinct
// users — ResolveLogin must not refuse this, and must actually create the
// second row (created=true), not silently reuse the first.
func TestResolveLoginAllowsDistinctSubjectsAndEmails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	subjA := "sub-a-" + uniq
	subjB := "sub-b-" + uniq
	emailA := uniq + "-a@example.com"
	emailB := uniq + "-b@example.com"

	idA, _, ok, err := s.ResolveLogin(ctx, subjA, emailA, "", false)
	if err != nil || !ok {
		t.Fatalf("ResolveLogin A: ok=%t err=%v", ok, err)
	}
	idB, createdB, ok, err := s.ResolveLogin(ctx, subjB, emailB, "", false)
	if err != nil || !ok {
		t.Fatalf("ResolveLogin B: ok=%t err=%v", ok, err)
	}
	if !createdB {
		t.Error("ResolveLogin B (new subject, distinct email): created = false, want true")
	}
	if idA == idB {
		t.Fatalf("distinct subjects and emails resolved to the same id %s", idA)
	}

	var n int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.users WHERE id = ANY($1)`,
		[]string{idA.String(), idB.String()}).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 2 {
		t.Fatalf("app.users rows for A and B = %d, want 2 (a genuine second user)", n)
	}
}

// The UPDATE shape: an EXISTING, enabled user's refreshed email claim collides
// with a DIFFERENT existing user's email — not the INSERT path above, and
// upsert_login's own UPDATE statement is what raises the 23505 here.
func TestResolveLoginRejectsEmailTakenByAnotherUserOnUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	subjX := "sub-x-" + uniq
	subjY := "sub-y-" + uniq
	emailX := uniq + "-x@example.com"
	emailY := uniq + "-y@example.com"

	idX, _, ok, err := s.ResolveLogin(ctx, subjX, emailX, "", false)
	if err != nil || !ok {
		t.Fatalf("ResolveLogin (seed X) ok=%t err=%v", ok, err)
	}
	if _, _, ok, err := s.ResolveLogin(ctx, subjY, emailY, "", false); err != nil || !ok {
		t.Fatalf("ResolveLogin (seed Y) ok=%t err=%v", ok, err)
	}

	// X logs back in with an email claim that now collides with Y's.
	_, _, ok, err = s.ResolveLogin(ctx, subjX, emailY, "", false)
	if !errors.Is(err, ErrLoginEmailTaken) {
		t.Fatalf("ResolveLogin (X's email now collides with Y) err = %v, want ErrLoginEmailTaken", err)
	}
	if ok {
		t.Error("ResolveLogin (X's email now collides with Y): ok = true, want false")
	}

	// X's row must be untouched — upsert_login's UPDATE is one statement and the
	// constraint failure aborts it wholesale, so the OLD email must still be on
	// file rather than half-applied.
	assertProfile(t, s, idX, emailX, "", "user")
}

// ListActiveUserIDs enumerates enabled users for the per-user fan-out, and must
// exclude a disabled one.
func TestListActiveUserIDsExcludesDisabled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	a := NewULID().String()
	b := NewULID().String()
	idA, _, _, err := s.ResolveLogin(ctx, "sub-"+a, a+"@example.com", "", false)
	if err != nil {
		t.Fatalf("ResolveLogin A: %v", err)
	}
	idB, _, _, err := s.ResolveLogin(ctx, "sub-"+b, b+"@example.com", "", false)
	if err != nil {
		t.Fatalf("ResolveLogin B: %v", err)
	}

	if _, err := h.super.Exec(ctx,
		`UPDATE app.users SET disabled_at = now() WHERE id = $1`, idB.String()); err != nil {
		t.Fatalf("disable B: %v", err)
	}

	ids, err := s.ListActiveUserIDs(ctx)
	if err != nil {
		t.Fatalf("ListActiveUserIDs: %v", err)
	}
	set := map[ulid.ULID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if !set[idA] {
		t.Errorf("enabled user %s missing from active set", idA)
	}
	if set[idB] {
		t.Errorf("disabled user %s present in active set", idB)
	}
}

// assertProfile checks the app-visible profile (RLS-scoped) matches expectations.
func assertProfile(t *testing.T, s *Store, id ulid.ULID, email, display, role string) {
	t.Helper()
	if err := s.UserTx(context.Background(), id, func(tx *UserTx) error {
		p, ok, err := tx.Profile(context.Background())
		if err != nil {
			return err
		}
		if !ok {
			t.Fatalf("Profile: not found for %s", id)
		}
		if p.Email != email {
			t.Errorf("Profile.Email = %q, want %q", p.Email, email)
		}
		if p.DisplayName != display {
			t.Errorf("Profile.DisplayName = %q, want %q", p.DisplayName, display)
		}
		if p.Role != role {
			t.Errorf("Profile.Role = %q, want %q", p.Role, role)
		}
		return nil
	}); err != nil {
		t.Fatalf("UserTx/Profile: %v", err)
	}
}
