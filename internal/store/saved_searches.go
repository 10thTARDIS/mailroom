// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// SAVED SEARCHES / SMART FOLDERS (#57, matrix §7).
//
// A saved search is a NAMED QUERY. Pinned to the sidebar it is what the matrix calls a
// smart folder; unpinned it is merely remembered. That is one feature and a boolean, not
// two objects — see migration 00024 for why, and for why the query is stored as the
// OPERATOR STRING rather than a parsed filter tree (short version: a saved search must run
// through exactly the same path as typing it, or it could match different mail than the
// same text typed by hand).
//
// This package does NOT validate the query beyond "not blank". Parsing lives in
// internal/search and runs at Email/query time, and duplicating even a little of it here
// would create a second opinion about what a query means — the one thing this design is
// built to avoid. An unparseable query is saved and fails when it runs, which is exactly
// what typing it by hand does.

// SavedSearch is one named query.
type SavedSearch struct {
	ID   ulid.ULID
	Name string
	// Query is the operator syntax, verbatim.
	Query string
	// InSidebar pins it to the nav rail — the "smart folder" presentation.
	InSidebar bool
	// Position orders the sidebar. Ties break by name so the order is total.
	Position int
}

// ErrSavedSearchInvalid is returned for a saved search this table will not hold.
var ErrSavedSearchInvalid = errors.New("store: not a valid saved search")

// ErrSavedSearchNameTaken is returned when the name is already used. It is its own error
// because it is the one failure a user can FIX (pick another name), so the API has to be
// able to say which of the two it was.
var ErrSavedSearchNameTaken = errors.New("store: a saved search with that name already exists")

const (
	maxSavedSearchName  = 120
	maxSavedSearchQuery = 2000
)

func validateSavedSearch(name, query string) (string, string, error) {
	n := strings.TrimSpace(name)
	q := strings.TrimSpace(query)
	if n == "" {
		return "", "", fmt.Errorf("%w: a name is required", ErrSavedSearchInvalid)
	}
	if len(n) > maxSavedSearchName {
		return "", "", fmt.Errorf("%w: name is longer than %d characters",
			ErrSavedSearchInvalid, maxSavedSearchName)
	}
	// A blank query would be a sidebar entry that shows the whole mailbox under a name
	// implying it shows something specific — refused rather than stored.
	if q == "" {
		return "", "", fmt.Errorf("%w: a query is required", ErrSavedSearchInvalid)
	}
	if len(q) > maxSavedSearchQuery {
		return "", "", fmt.Errorf("%w: query is longer than %d characters",
			ErrSavedSearchInvalid, maxSavedSearchQuery)
	}
	return n, q, nil
}

// ListSavedSearches returns every saved search in sidebar order.
//
// The whole list, always: it is a handful of rows, the sidebar needs all the pinned ones
// to render and the search box needs the rest to offer them, so filtering server-side
// would be two queries to save nothing.
func (tx *UserTx) ListSavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, name, query, in_sidebar, position
		  FROM app.saved_searches
		 WHERE user_id = $1
		 ORDER BY position, name`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list saved searches: %w", err)
	}
	defer rows.Close()

	var out []SavedSearch
	for rows.Next() {
		var (
			id string
			s  SavedSearch
		)
		if err := rows.Scan(&id, &s.Name, &s.Query, &s.InSidebar, &s.Position); err != nil {
			return nil, fmt.Errorf("store: scan saved search: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: saved search id %q: %w", id, err)
		}
		s.ID = parsed
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateSavedSearch stores one and appends its change_log row in the same transaction
// (the load-bearing invariant — a JMAP-visible object).
func (tx *UserTx) CreateSavedSearch(ctx context.Context, s SavedSearch) (ulid.ULID, error) {
	name, query, err := validateSavedSearch(s.Name, s.Query)
	if err != nil {
		return ulid.ULID{}, err
	}

	// UNDER A SAVEPOINT, and this is not optional. A UNIQUE(user_id, name) violation is a
	// RECOVERABLE outcome the caller reports in notCreated and continues past — but a
	// failed statement aborts the whole surrounding UserTx (SQLSTATE 25P02), so without
	// the savepoint one duplicate name would take down every other create in the same
	// SavedSearch/set. The per-object error handling above it would be decoration.
	//
	// (Found by a test asserting exactly that: a batch with four bad creates and one good
	// one. The good one came back as serverFail.)
	id := NewULID()
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create saved search (savepoint): %w", err)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.saved_searches (id, user_id, name, query, in_sidebar, position)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id.String(), tx.userID.String(), name, query, s.InSidebar, s.Position,
	); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return ulid.ULID{}, fmt.Errorf("%w: %q", ErrSavedSearchNameTaken, name)
		}
		return ulid.ULID{}, fmt.Errorf("store: create saved search: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create saved search (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSavedSearch, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// UpdateSavedSearch replaces the mutable fields of one.
//
// A FULL-OBJECT update, like ComposeSnippet's and for the same reason: name, query and
// in_sidebar together decide what the thing IS and where it appears, and a partial write
// that changed one without the others is the class of bug #170 hit.
func (tx *UserTx) UpdateSavedSearch(ctx context.Context, id ulid.ULID, s SavedSearch) (found bool, err error) {
	name, query, err := validateSavedSearch(s.Name, s.Query)
	if err != nil {
		return false, err
	}

	// Under a savepoint for the same reason the create is: a rename onto a name already
	// in use is recoverable and per-object, and a bare failed UPDATE would abort the
	// surrounding transaction and lose every other write in the same set.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update saved search (savepoint): %w", err)
	}
	var updated string
	err = sp.QueryRow(ctx, `
		UPDATE app.saved_searches
		   SET name = $3, query = $4, in_sidebar = $5, position = $6, updated_at = now()
		 WHERE user_id = $1 AND id = $2
		 RETURNING id`,
		tx.userID.String(), id.String(), name, query, s.InSidebar, s.Position,
	).Scan(&updated)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isNoRows(err) {
			return false, nil
		}
		if isUniqueViolation(err) {
			return false, fmt.Errorf("%w: %q", ErrSavedSearchNameTaken, name)
		}
		return false, fmt.Errorf("store: update saved search: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: update saved search (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSavedSearch, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteSavedSearch removes one.
func (tx *UserTx) DeleteSavedSearch(ctx context.Context, id ulid.ULID) (found bool, err error) {
	var deleted string
	err = tx.tx.QueryRow(ctx,
		`DELETE FROM app.saved_searches WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete saved search: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectSavedSearch, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}
