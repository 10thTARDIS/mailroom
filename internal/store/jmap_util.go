// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oklog/ulid/v2"
)

// This file holds small helpers shared by the JMAP store surface
// (jmap_email.go, jmap_mailbox.go, jmap_thread.go).

// ulidStrings renders a ULID slice as the []string of Crockford-base32 forms
// pgx binds to a Postgres text[]/app.ulid[] for `= ANY($n)` predicates.
func ulidStrings(ids []ulid.ULID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// isNoRows reports whether err is pgx's no-rows sentinel.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// uniqueViolationConstraint reports the constraint name of a PostgreSQL
// unique_violation (23505), and whether err was one at all. Postgres' own error
// TEXT is localisable and not meant to be parsed; the constraint name is the
// one stable fact that lets a caller with two overlapping unique indexes (e.g.
// areas.go's CreateArea, which can collide on either the name or the owned
// label) tell them apart and return a distinct sentinel for each, rather than
// one error that could mean either.
func uniqueViolationConstraint(err error) (name string, ok bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return "", false
	}
	return pgErr.ConstraintName, true
}

// isCheckViolation reports whether err is a PostgreSQL check_violation
// (23514) — the SQLSTATE a CHECK constraint fails with, as opposed to a
// unique_violation (23505, see labels.go's isUniqueViolation). Callers that
// let a bounded numeric setting (a threshold, a percentage) reach the CHECK
// use this to turn the raw constraint failure into a typed validation error
// rather than a bare Postgres message.
func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// applyKeywordDelta returns cur with add ∪-ed in and remove removed, preserving
// order and de-duplicating — the canonical messages.keywords[] projection after
// an Email/set keyword patch. Comparison is exact (JMAP keywords are
// case-sensitive tokens).
func applyKeywordDelta(cur, add, remove []string) []string {
	rm := make(map[string]bool, len(remove))
	for _, k := range remove {
		rm[k] = true
	}
	seen := make(map[string]bool, len(cur)+len(add))
	out := make([]string, 0, len(cur)+len(add))
	appendKW := func(k string) {
		if rm[k] || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	for _, k := range cur {
		appendKW(k)
	}
	for _, k := range add {
		appendKW(k)
	}
	return out
}
