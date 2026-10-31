// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// deploy/rebind-operator-subject.sql is the one database step of the recovery
// deploy/README.md §4 prescribes for a wrong MAILROOM_OPERATOR_SUBJECT (#574).
// These tests run THAT FILE — read from disk, not a copy of its SQL — so the
// documented procedure is the tested one, and they pin the two claims the
// README's ordering rests on: correcting the variable without moving the user
// makes boot fail, and so does moving the user without correcting the variable.
// Boot is ProvisionUser, the call provisionOperator makes; a login is
// ResolveLogin, the call provisioningResolver makes.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// runRebindScript executes deploy/rebind-operator-subject.sql as the superuser,
// the way psql does: :'old' and :'new' become quoted literals, and the file is
// sent as one simple-protocol batch, so a RAISE stops it mid-transaction just
// as ON_ERROR_STOP does. The ROLLBACK afterwards is what psql's exit does to
// the aborted transaction.
func runRebindScript(t *testing.T, h *harness, oldSubject, newSubject string) error {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/rebind-operator-subject.sql")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(raw)
	for _, v := range []string{":'old'", ":'new'"} {
		// A renamed variable would otherwise reach the server as a syntax error
		// and read as a refusal the script never made.
		if !strings.Contains(script, v) {
			t.Fatalf("script no longer uses %s; update this harness", v)
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script = strings.ReplaceAll(script, ":'old'", quote(oldSubject))
	script = strings.ReplaceAll(script, ":'new'", quote(newSubject))

	ctx := context.Background()
	conn, err := h.super.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	_, runErr := conn.Conn().PgConn().Exec(ctx, script).ReadAll()
	if runErr != nil {
		if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("rollback after refusal: %v", err)
		}
	}
	return runErr
}

// userBySubject reads the row the script moves or retires, as the superuser.
func userBySubject(t *testing.T, h *harness, subject string) (id, email string, disabled, found bool) {
	t.Helper()
	err := h.super.QueryRow(context.Background(),
		`SELECT id, email::text, disabled_at IS NOT NULL FROM app.users WHERE oidc_subject = $1`,
		subject).Scan(&id, &email, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, false
	}
	if err != nil {
		t.Fatalf("read user %q: %v", subject, err)
	}
	return id, email, disabled, true
}

// isEmailCollision reports whether err is the users_email_key violation that
// boot (ProvisionUser) raises on a colliding email.
func isEmailCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_key"
}

// The lockout shape: same email in the config and the token, so the operator's
// real login is refused and no second user exists.
func TestRebindOperatorSubjectLockout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	wrong, actual := "wrong-"+uniq, "real-"+uniq
	email := uniq + "@example.com"

	op, err := s.ProvisionUser(ctx, wrong, email)
	if err != nil {
		t.Fatalf("boot provision under the wrong subject: %v", err)
	}
	if _, _, _, err := s.ResolveLogin(ctx, actual, email, "Op", false); !errors.Is(err, ErrLoginEmailTaken) {
		t.Fatalf("premise: the real login should be refused with ErrLoginEmailTaken, got %v", err)
	}

	// The obvious fix — correct the variable and restart — does not boot.
	if _, err := s.ProvisionUser(ctx, actual, email); !isEmailCollision(err) {
		t.Fatalf("boot with the corrected subject BEFORE the move: want a users_email_key collision, got %v", err)
	}
	if _, _, _, found := userBySubject(t, h, actual); found {
		t.Fatal("the failed boot left a user under the real subject")
	}

	if err := runRebindScript(t, h, wrong, actual); err != nil {
		t.Fatalf("rebind script: %v", err)
	}

	// Next boot, with the corrected variable: the SAME user, so everything it
	// owns came along.
	booted, err := s.ProvisionUser(ctx, actual, email)
	if err != nil {
		t.Fatalf("boot with the corrected subject after the move: %v", err)
	}
	if booted != op {
		t.Fatalf("boot resolved user %s, want the moved operator %s", booted, op)
	}
	got, created, ok, err := s.ResolveLogin(ctx, actual, email, "Op", false)
	if err != nil || !ok {
		t.Fatalf("the operator's real login after the move: ok=%v err=%v", ok, err)
	}
	if got != op || created {
		t.Fatalf("login resolved %s (created=%v), want the existing operator %s", got, created, op)
	}

	// And the other half of the README's ordering: a boot that still carries
	// the OLD value after the move collides and refuses to start.
	if _, err := s.ProvisionUser(ctx, wrong, email); !isEmailCollision(err) {
		t.Fatalf("boot with the OLD subject after the move: want a users_email_key collision, got %v", err)
	}
}

// The split shape: the provider's email differs from the configured one, so the
// real login created a second, empty user. The script retires it and moves the
// operator into its subject — and its email, which the next login refreshes to.
func TestRebindOperatorSubjectSplit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	wrong, actual := "wrong-"+uniq, "real-"+uniq
	configEmail, tokenEmail := "cfg-"+uniq+"@example.com", "idp-"+uniq+"@example.com"

	op, err := s.ProvisionUser(ctx, wrong, configEmail)
	if err != nil {
		t.Fatalf("boot provision: %v", err)
	}
	squat, created, ok, err := s.ResolveLogin(ctx, actual, tokenEmail, "Op", false)
	if err != nil || !ok || !created || squat == op {
		t.Fatalf("premise: the real login should create a second user: id=%s created=%v ok=%v err=%v", squat, created, ok, err)
	}

	if err := runRebindScript(t, h, wrong, actual); err != nil {
		t.Fatalf("rebind script: %v", err)
	}

	id, email, disabled, found := userBySubject(t, h, "retired-574:"+squat.String())
	if !found || id != squat.String() || !disabled {
		t.Fatalf("the empty user was not retired: found=%v id=%s disabled=%v", found, id, disabled)
	}
	if email == tokenEmail {
		t.Fatal("the retired user kept the provider's email; the operator's next login would collide on it")
	}

	booted, err := s.ProvisionUser(ctx, actual, configEmail)
	if err != nil || booted != op {
		t.Fatalf("boot after the move: id=%s err=%v, want %s", booted, err, op)
	}
	// This login takes upsert_login's UPDATE branch and refreshes the email to
	// the provider's — which works only because the retired user gave it up.
	got, created, ok, err := s.ResolveLogin(ctx, actual, tokenEmail, "Op", false)
	if err != nil || !ok || got != op || created {
		t.Fatalf("login after the move: id=%s created=%v ok=%v err=%v, want the operator %s", got, created, ok, err, op)
	}
}

// Every refusal changes nothing.
func TestRebindOperatorSubjectRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	uniq := NewULID().String()
	wrong, actual := "wrong-"+uniq, "real-"+uniq

	op, err := s.ProvisionUser(ctx, wrong, "cfg-"+uniq+"@example.com")
	if err != nil {
		t.Fatalf("boot provision: %v", err)
	}
	squat, _, _, err := s.ResolveLogin(ctx, actual, "idp-"+uniq+"@example.com", "Op", false)
	if err != nil {
		t.Fatalf("second user: %v", err)
	}
	// The second user is NOT empty: it owns an account.
	mustExec(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'real','mail.example.com','smtp.example.com',$3,$4)`,
		NewULID().String(), squat.String(), []byte{0x01}, NewULID().String())

	for _, tc := range []struct {
		name, oldSub, newSub, wantMsg string
	}{
		{"unknown old subject", "nobody-" + uniq, actual, "no user has subject"},
		{"old equals new", wrong, wrong, "must both be set, and differ"},
		{"empty new", wrong, "", "must both be set, and differ"},
		{"non-empty user under new", wrong, actual, "refusing to retire it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runRebindScript(t, h, tc.oldSub, tc.newSub)
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("want a refusal containing %q, got %v", tc.wantMsg, err)
			}
			if id, _, disabled, found := userBySubject(t, h, wrong); !found || id != op.String() || disabled {
				t.Fatalf("the operator moved or changed: found=%v id=%s disabled=%v", found, id, disabled)
			}
			if id, _, disabled, found := userBySubject(t, h, actual); !found || id != squat.String() || disabled {
				t.Fatalf("the second user moved or was retired: found=%v id=%s disabled=%v", found, id, disabled)
			}
		})
	}
}
