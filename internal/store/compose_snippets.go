// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Signatures and templates — named, reusable compose text (#56, spec §5).
//
// ONE TYPE FOR BOTH, because they are the same thing wearing two hats: a named
// piece of body text the user wrote once and reuses. A SIGNATURE is appended and
// may belong to an identity; a TEMPLATE seeds a whole message and never does.
// Everything else about them is identical, so two types would mean two JMAP
// objects, two /changes streams and two settings surfaces answering one question.
//
// identities.signature_html IS NOT REPLACED. It holds exactly one signature per
// identity and is read by the JMAP Identity object's htmlSignature, which clients
// already use — so it stays, keeps working, and is the identity's default when this
// table has nothing for it. #56 only adds having SEVERAL and choosing.

// SnippetKind distinguishes the two hats.
type SnippetKind string

const (
	// SnippetSignature is appended to a message and may be bound to an identity —
	// the signature for mail from shop@ is not the one for work@ (spec §5).
	SnippetSignature SnippetKind = "signature"
	// SnippetTemplate seeds a whole body and is never bound to an identity.
	SnippetTemplate SnippetKind = "template"
)

// ComposeSnippet is one signature or template.
type ComposeSnippet struct {
	ID   ulid.ULID
	Kind SnippetKind
	// IdentityID is set only for a signature offered for one specific address.
	// Nil means "any identity" (and is the only legal value for a template).
	IdentityID *ulid.ULID
	Name       string
	BodyHTML   string
	BodyText   string
	IsDefault  bool
}

// ErrSnippetInvalid is returned for a snippet this table will not hold.
var ErrSnippetInvalid = errors.New("store: not a valid compose snippet")

// ErrSnippetNameTaken is returned when the (kind, name) pair is already in use. Its own
// error because it is the one failure a user can FIX — pick another name — so the API has
// to be able to say which of the two it was.
var ErrSnippetNameTaken = errors.New("store: a snippet with that name already exists")

// maxSnippetBody bounds one snippet's body.
//
// A signature or template is a few lines a person typed, not a document. The bound
// exists so that "reusable compose text" cannot become a place to park a megabyte
// that is then prepended to every message the user sends — which would be a
// self-inflicted quota problem discovered at send time, far from its cause.
const maxSnippetBody = 64 * 1024

// maxSnippetName keeps the picker readable: these are menu entries.
const maxSnippetName = 120

func validateSnippet(kind SnippetKind, identityID *ulid.ULID, name, html, text string) (string, error) {
	if kind != SnippetSignature && kind != SnippetTemplate {
		return "", fmt.Errorf("%w: kind %q", ErrSnippetInvalid, kind)
	}
	// A template bound to an identity is a category error the schema also refuses;
	// caught here so the caller gets a clean message instead of a constraint name.
	if kind == SnippetTemplate && identityID != nil {
		return "", fmt.Errorf("%w: a template is not tied to an address", ErrSnippetInvalid)
	}
	n := strings.TrimSpace(name)
	if n == "" {
		return "", fmt.Errorf("%w: a name is required", ErrSnippetInvalid)
	}
	if len(n) > maxSnippetName {
		return "", fmt.Errorf("%w: name is longer than %d characters", ErrSnippetInvalid, maxSnippetName)
	}
	// Both bodies empty is a menu entry that does nothing — refused rather than
	// stored, so a user cannot end up picking a snippet and seeing no change.
	if strings.TrimSpace(html) == "" && strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%w: a signature or template needs a body", ErrSnippetInvalid)
	}
	if len(html) > maxSnippetBody || len(text) > maxSnippetBody {
		return "", fmt.Errorf("%w: body is longer than %d bytes", ErrSnippetInvalid, maxSnippetBody)
	}
	return n, nil
}

// ListComposeSnippets returns every snippet, signatures then templates, by name.
//
// The whole list, always: a compose window needs the signatures for the identity it
// is sending as AND the templates, and both sets are a handful of rows. Filtering
// server-side would be one query per compose open to save nothing.
func (tx *UserTx) ListComposeSnippets(ctx context.Context) ([]ComposeSnippet, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, kind, identity_id, name, body_html, body_text, is_default
		  FROM app.compose_snippets
		 WHERE user_id = $1
		 ORDER BY kind, name`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list compose snippets: %w", err)
	}
	defer rows.Close()

	var out []ComposeSnippet
	for rows.Next() {
		var (
			id, kind, name, html, text string
			identity                   *string
			isDefault                  bool
		)
		if err := rows.Scan(&id, &kind, &identity, &name, &html, &text, &isDefault); err != nil {
			return nil, fmt.Errorf("store: scan compose snippet: %w", err)
		}
		s := ComposeSnippet{
			Kind: SnippetKind(kind), Name: name,
			BodyHTML: html, BodyText: text, IsDefault: isDefault,
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: compose snippet id %q: %w", id, err)
		}
		s.ID = parsed
		if identity != nil {
			iid, err := ulid.Parse(*identity)
			if err != nil {
				return nil, fmt.Errorf("store: compose snippet identity %q: %w", *identity, err)
			}
			s.IdentityID = &iid
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateComposeSnippet stores one and appends its change_log row in the same
// transaction (the load-bearing invariant — a JMAP-visible object).
func (tx *UserTx) CreateComposeSnippet(ctx context.Context, s ComposeSnippet) (ulid.ULID, error) {
	name, err := validateSnippet(s.Kind, s.IdentityID, s.Name, s.BodyHTML, s.BodyText)
	if err != nil {
		return ulid.ULID{}, err
	}
	// Setting a new default must CLEAR the old one, or the partial unique index
	// rejects the insert and the user sees a constraint error for having done a
	// perfectly reasonable thing. "Make this the default" means "and not that one".
	if s.IsDefault {
		if err := tx.clearSnippetDefault(ctx, s.Kind, s.IdentityID); err != nil {
			return ulid.ULID{}, err
		}
	}

	// UNDER A SAVEPOINT. UNIQUE(user_id, kind, name) makes a duplicate name a
	// RECOVERABLE, per-object outcome — but a failed statement aborts the whole
	// surrounding UserTx (SQLSTATE 25P02), so without this one duplicate name takes down
	// every other create in the same ComposeSnippet/set and the request comes back
	// serverFail instead of reporting which snippet was refused.
	//
	// This was a real defect shipped in v1.15.0, found while building #57's saved
	// searches (which have the same shape) and confirmed by probing this path directly.
	// See CreateAccount for the same reasoning written down earlier.
	id := NewULID()
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create compose snippet (savepoint): %w", err)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.compose_snippets
		  (id, user_id, kind, identity_id, name, body_html, body_text, is_default)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id.String(), tx.userID.String(), string(s.Kind), ulidPtrString(s.IdentityID),
		name, s.BodyHTML, s.BodyText, s.IsDefault,
	); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return ulid.ULID{}, fmt.Errorf("%w: %q", ErrSnippetNameTaken, name)
		}
		return ulid.ULID{}, fmt.Errorf("store: create compose snippet: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create compose snippet (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectComposeSnippet, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// UpdateComposeSnippet replaces the mutable fields of one snippet.
//
// A full-object update, not a patch of whatever the caller mentioned: the kind and
// the identity binding decide which pickers offer it, and letting a partial write
// change one without the other produced exactly the class of bug #170 hit (a
// full-object save that wiped a field it never meant to touch). The caller sends
// the whole snippet or nothing.
func (tx *UserTx) UpdateComposeSnippet(ctx context.Context, id ulid.ULID, s ComposeSnippet) (found bool, err error) {
	name, err := validateSnippet(s.Kind, s.IdentityID, s.Name, s.BodyHTML, s.BodyText)
	if err != nil {
		return false, err
	}
	if s.IsDefault {
		if err := tx.clearSnippetDefaultExcept(ctx, s.Kind, s.IdentityID, id); err != nil {
			return false, err
		}
	}

	// Under a savepoint for the same reason the create is: a rename onto a name already in
	// use is recoverable and per-object, and a bare failed UPDATE aborts the surrounding
	// transaction and loses every other write in the same set.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update compose snippet (savepoint): %w", err)
	}
	var updated string
	err = sp.QueryRow(ctx, `
		UPDATE app.compose_snippets
		   SET kind = $3, identity_id = $4, name = $5, body_html = $6, body_text = $7,
		       is_default = $8, updated_at = now()
		 WHERE user_id = $1 AND id = $2
		 RETURNING id`,
		tx.userID.String(), id.String(), string(s.Kind), ulidPtrString(s.IdentityID),
		name, s.BodyHTML, s.BodyText, s.IsDefault,
	).Scan(&updated)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isNoRows(err) {
			return false, nil
		}
		if isUniqueViolation(err) {
			return false, fmt.Errorf("%w: %q", ErrSnippetNameTaken, name)
		}
		return false, fmt.Errorf("store: update compose snippet: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: update compose snippet (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectComposeSnippet, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteComposeSnippet removes one.
func (tx *UserTx) DeleteComposeSnippet(ctx context.Context, id ulid.ULID) (found bool, err error) {
	var deleted string
	err = tx.tx.QueryRow(ctx, `
		DELETE FROM app.compose_snippets WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete compose snippet: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectComposeSnippet, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// clearSnippetDefault drops the default flag from whatever currently holds it for
// this (kind, identity) slot, so setting a new one is a move rather than a clash.
func (tx *UserTx) clearSnippetDefault(ctx context.Context, kind SnippetKind, identityID *ulid.ULID) error {
	return tx.clearSnippetDefaultExcept(ctx, kind, identityID, ulid.ULID{})
}

// clearSnippetDefaultExcept clears the slot's current default (other than keep)
// and appends the ComposeSnippet `updated` row THAT SIBLING IS OWED (#299).
//
// ⚠ MAKING ONE SNIPPET THE DEFAULT MUTATES TWO OBJECTS. The caller appends the
// row for the snippet it is creating or updating; nothing appended one for the
// snippet that silently stopped being the default, so a second client kept
// rendering the old signature as "default" — and, worse, would keep OFFERING it,
// because is_default is what the compose picker preselects. The client that made
// the change sees the right thing and every other client sees the previous state
// forever, which is precisely the split-brain /changes exists to prevent.
//
// RETURNING drives it rather than a count: at most one row can match (the
// partial unique index guarantees one default per slot), but the id is what the
// change row needs and asking the UPDATE for it costs nothing.
func (tx *UserTx) clearSnippetDefaultExcept(
	ctx context.Context, kind SnippetKind, identityID *ulid.ULID, keep ulid.ULID,
) error {
	// The identity comparison has to treat NULL as a value, not as "unknown": the
	// no-identity default is its own slot. `IS NOT DISTINCT FROM` is what makes that
	// read correctly for both cases in one statement.
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.compose_snippets
		   SET is_default = false, updated_at = now()
		 WHERE user_id = $1 AND kind = $2
		   AND identity_id IS NOT DISTINCT FROM $3
		   AND is_default
		   AND id <> $4
		RETURNING id`,
		tx.userID.String(), string(kind), ulidPtrString(identityID), keep.String(),
	)
	if err != nil {
		return fmt.Errorf("store: clear compose snippet default: %w", err)
	}
	var demoted []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan demoted compose snippet: %w", err)
		}
		u, perr := ulid.Parse(s)
		if perr != nil {
			rows.Close()
			return fmt.Errorf("store: demoted compose snippet id %q: %w", s, perr)
		}
		demoted = append(demoted, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: clear compose snippet default: %w", err)
	}
	for _, id := range demoted {
		if _, err := tx.AppendChange(ctx, ObjectComposeSnippet, id, OpUpdated); err != nil {
			return err
		}
	}
	return nil
}

// ulidPtrString renders an optional id for pgx: nil becomes SQL NULL, which the
// nullable identity_id column and the IS NOT DISTINCT FROM comparison both need.
func ulidPtrString(id *ulid.ULID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}
