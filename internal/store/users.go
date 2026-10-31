// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// ErrLoginEmailTaken is returned when a login's email claim collides with
// app.users.email (citext NOT NULL UNIQUE, constraint users_email_key) for a
// DIFFERENT subject than the one logging in — app.upsert_login's raw 23505,
// mapped here rather than left as an opaque constraint failure. Two shapes
// reach it (Forgejo #574):
//
//   - the INSERT branch: a subject logging in for the first time whose email
//     claim already belongs to an existing row — most commonly a
//     misconfigured MAILROOM_OPERATOR_SUBJECT, which provisioned the operator
//     at boot under the wrong subject, so ProvisionUser's row already holds
//     the operator's real email and the operator's genuine OIDC login (a
//     different subject, same email) collides with it;
//   - the UPDATE branch: an EXISTING, enabled user's email claim changed
//     (upsert_login refreshes email on every login) to a value some OTHER
//     user's row already holds.
//
// Either way, no row is left half-written: Postgres rejects the statement
// outright, so the caller (cmd/mailroom's provisioningResolver) must refuse
// the login rather than silently creating a second, empty user or leaving the
// existing row untouched with no explanation.
var ErrLoginEmailTaken = errors.New("email already belongs to a different user")

// This file holds the user-identity surface the API layer needs at the edges of
// the RLS world:
//
//   - ProvisionUser get-or-creates the configured operator at BOOT, before any
//     identity exists. mailroom_app cannot INSERT into app.users at all (the
//     users_self policy is SELECT-only), so it goes through the narrow SECURITY
//     DEFINER app.provision_user.
//   - UserIDForSubject resolves an OIDC subject to a mailroom user id at LOGIN,
//     before any identity is set. RLS blocks a lookup-by-subject as mailroom_app
//     (no identity ⇒ no rows), so it goes through the narrow SECURITY DEFINER
//     app.user_id_for_subject — the exact same pattern as app.dequeue_due_jobs
//     (data-model §5): a NOLOGIN owner-run function with a tight signature that
//     exposes one id, never mail content.
//   - Profile reads the authenticated user's own row (RLS users_self policy) to
//     populate the JMAP Session object.

// UserProfile is the subset of app.users the Session resource reports.
type UserProfile struct {
	ID          ulid.ULID
	Email       string
	DisplayName string
	Role        string
}

// ProvisionUser get-or-creates the user identified by subject and returns its
// id, inside a SystemTx because boot has no user identity yet. It is the
// BOOT-time provisioning path: the server calls it for the dev-auth operator
// (which needs a fixed identity to inject) and for the optional declared-accounts
// operator (MAILROOM_OPERATOR_SUBJECT). Under OIDC, users are provisioned at
// LOGIN instead, via ResolveLogin — app-binding is the access gate (mailroom#98).
// Unlike ResolveLogin, ProvisionUser never mutates an existing row (no email
// rewrite, no role change, no un-disabling): it is strictly get-or-create.
//
// The candidate id is minted app-side (ADR-007) and offered to
// app.provision_user, which inserts it only if the subject is new; an existing
// subject returns its pre-existing id and the candidate is discarded. Calling
// this on every boot is therefore idempotent — same subject, same id, exactly
// one row — and it never mutates an existing row (a changed email or a disabled
// account is not silently rewritten/revived here).
func (s *Store) ProvisionUser(ctx context.Context, subject, email string) (ulid.ULID, error) {
	if subject == "" {
		return ulid.ULID{}, errors.New("store: provision user: subject is required")
	}
	if email == "" {
		// app.users.email is NOT NULL UNIQUE; an empty string would both read
		// badly in the JMAP session and collide across operators. Fail closed.
		return ulid.ULID{}, errors.New("store: provision user: email is required")
	}

	candidate := NewULID()
	var id ulid.ULID
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		var raw *string
		if err := tx.tx.QueryRow(ctx,
			`SELECT app.provision_user($1::app.ulid, $2::text, $3::text)`,
			candidate.String(), subject, email,
		).Scan(&raw); err != nil {
			return fmt.Errorf("store: provision user: %w", err)
		}
		if raw == nil {
			// The function always returns an id on success; NULL means an RLS
			// policy silently swallowed the INSERT. Never hand back a zero id.
			return errors.New("store: provision user: no id returned (definer blocked by RLS?)")
		}
		u, err := ulid.Parse(*raw)
		if err != nil {
			return fmt.Errorf("store: provision user: parse user id %q: %w", *raw, err)
		}
		id = u
		return nil
	})
	if err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// ResolveLogin is the login-time get-or-create for the OIDC multi-user model
// (Forgejo #98): app-binding is the access gate, so ANY subject that completes
// the OIDC flow for this application is provisioned on first login and has its
// role re-evaluated on EVERY login. It is the provisioning counterpart to the
// boot-time ProvisionUser — the two share the same SECURITY DEFINER pattern over
// app.users (mailroom_app has SELECT-only on that table, scoped to its own row),
// here app.upsert_login, run inside a SystemTx because the login path has no user
// identity yet.
//
// It:
//   - inserts the user when the subject is new (role, email, display_name taken
//     from the VERIFIED token — never from anything request-supplied), and
//   - on an existing, enabled user, refreshes email/display_name and re-applies
//     the role, so a user added to (or removed from) the admin group gains (or
//     loses) admin on their next login.
//
// ok is false — and no row is mutated — when the subject resolves to a DISABLED
// or soft-DELETED user: such a user must not be able to log back in, exactly as
// UserIDForSubject refuses them, so the caller (the provisioning resolver) fails
// the login closed. subject and email are both required; email is NOT NULL UNIQUE
// on app.users, so a login that carried no email claim cannot provision and is
// rejected here rather than inventing a placeholder identity.
//
// admin selects the role: true → 'admin', false → 'user'. The decision itself
// (which group means admin) belongs to the caller; this layer only persists the
// verdict.
//
// created reports whether THIS call inserted a new row, as opposed to matching
// (and possibly refreshing) an existing one: the definer only ever returns the
// offered candidate id on the insert path (an existing subject's UPDATE returns
// its own, pre-existing id, and a lost race falls back to the row that won it),
// so "created" is exactly "the returned id equals the candidate". The caller
// uses it to warn, once per new user, when auto-provisioning may have just
// split what should have been one operator into two (Forgejo #574).
//
// err wraps ErrLoginEmailTaken — rather than a bare Postgres constraint
// failure — when the email claim collides with a DIFFERENT existing user (see
// that sentinel's doc for the two shapes this can happen in); every other
// failure keeps its own error as before.
func (s *Store) ResolveLogin(ctx context.Context, subject, email, displayName string, admin bool) (id ulid.ULID, created, ok bool, err error) {
	if subject == "" {
		return ulid.ULID{}, false, false, errors.New("store: resolve login: subject is required")
	}
	if email == "" {
		// app.users.email is NOT NULL UNIQUE; provisioning a blank email would
		// both read wrong in the JMAP session and collide across users. Fail closed.
		return ulid.ULID{}, false, false, errors.New("store: resolve login: email is required")
	}
	role := "user"
	if admin {
		role = "admin"
	}

	candidate := NewULID()
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var raw *string
		if e := tx.tx.QueryRow(ctx,
			`SELECT app.upsert_login($1::app.ulid, $2::text, $3::text, $4::text, $5::text)`,
			candidate.String(), subject, email, displayName, role,
		).Scan(&raw); e != nil {
			if cname, cok := uniqueViolationConstraint(e); cok && cname == "users_email_key" {
				return fmt.Errorf("store: resolve login: %w", ErrLoginEmailTaken)
			}
			return fmt.Errorf("store: resolve login: %w", e)
		}
		if raw == nil {
			// NULL is the definer's "this subject is disabled or deleted" signal —
			// deny, do not fabricate an id.
			return nil
		}
		u, pe := ulid.Parse(*raw)
		if pe != nil {
			return fmt.Errorf("store: resolve login: parse user id %q: %w", *raw, pe)
		}
		id, ok = u, true
		created = u == candidate
		return nil
	})
	if err != nil {
		return ulid.ULID{}, false, false, err
	}
	return id, created, ok, nil
}

// ListActiveUserIDs returns the id of every provisioned, enabled (non-disabled,
// non-deleted) user. It drives the per-user fan-out of the background loops (sync,
// indexer, Trash purge seeding): mailroom_app cannot enumerate app.users under RLS
// (users_self scopes it to one row, and a SystemTx has no identity at all), so the
// list comes through the narrow SECURITY DEFINER app.active_user_ids — the same
// owner-pinned pattern as UserIDForSubject, exposing user IDS only, never mail.
func (s *Store) ListActiveUserIDs(ctx context.Context) ([]ulid.ULID, error) {
	var ids []ulid.ULID
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		rows, e := tx.tx.Query(ctx, `SELECT app.active_user_ids()`)
		if e != nil {
			return fmt.Errorf("store: list active user ids: %w", e)
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if e := rows.Scan(&raw); e != nil {
				return fmt.Errorf("store: list active user ids: scan: %w", e)
			}
			u, pe := ulid.Parse(raw)
			if pe != nil {
				return fmt.Errorf("store: list active user ids: parse %q: %w", raw, pe)
			}
			ids = append(ids, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// UserIDForSubject resolves an OIDC subject claim to a mailroom user id via the
// SECURITY DEFINER app.user_id_for_subject, run inside a SystemTx (no identity —
// the login path has no user context yet). found is false when no enabled,
// non-deleted user matches the subject; the caller (auth.ResolveUser) then
// aborts the login rather than provisioning — provisioning is a boot-time
// operator decision (ProvisionUser), never a consequence of someone logging in.
// Fail closed: an unknown subject never yields a zero-value id that could be
// mistaken for a valid user.
func (s *Store) UserIDForSubject(ctx context.Context, subject string) (ulid.ULID, bool, error) {
	var (
		id    ulid.ULID
		found bool
	)
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		var raw *string
		if err := tx.tx.QueryRow(ctx,
			`SELECT app.user_id_for_subject($1)`, subject,
		).Scan(&raw); err != nil {
			return fmt.Errorf("store: user id for subject: %w", err)
		}
		if raw == nil {
			return nil
		}
		u, err := ulid.Parse(*raw)
		if err != nil {
			return fmt.Errorf("store: parse user id %q: %w", *raw, err)
		}
		id, found = u, true
		return nil
	})
	if err != nil {
		return ulid.ULID{}, false, err
	}
	return id, found, nil
}

// Profile returns the authenticated user's own row. RLS (users_self) scopes the
// read to the transaction identity, so this can only ever return the caller's
// own profile. A missing row (a token for a since-deleted user) is a not-found.
func (tx *UserTx) Profile(ctx context.Context) (UserProfile, bool, error) {
	var p UserProfile
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id, email, display_name, role
		  FROM app.users
		 WHERE id = $1`,
		tx.userID.String(),
	).Scan(&id, &p.Email, &p.DisplayName, &p.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserProfile{}, false, nil
	}
	if err != nil {
		return UserProfile{}, false, fmt.Errorf("store: read profile: %w", err)
	}
	if p.ID, err = ulid.Parse(id); err != nil {
		return UserProfile{}, false, fmt.Errorf("store: parse user id %q: %w", id, err)
	}
	return p, true, nil
}
