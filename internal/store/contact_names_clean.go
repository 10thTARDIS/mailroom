// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"unicode"

	"github.com/oklog/ulid/v2"
)

// The surface of `mailroom-admin contact-names-clean` (#831). Learned display
// names used to be stored raw; ingest now stores them through
// CleanLearnedContactName, and this repairs the rows written before.
//
// CHANGE ROWS: yes. Contact.name is a property, so a cleaned name is a visible
// change: one Contact `updated` row per contact the batch changed, in the same
// savepoint as the writes. A row whose name moved since it was read is skipped
// and counted as stale, never forced.

// ContactNameKinds says which kinds of hostile character a name contains.
type ContactNameKinds struct {
	// Bidi: explicit directional formatting characters.
	Bidi bool
	// Control: other C0 and C1 control characters (line breaks included).
	Control bool
	// Format: zero-width and other format characters that are not bidi.
	Format bool
}

// Any reports whether the name has a character of any kind.
func (k ContactNameKinds) Any() bool { return k.Bidi || k.Control || k.Format }

// ClassifyContactName reports which kinds of character CleanLearnedContactName
// would remove from s. Surrounding space is not a kind: only characters
// that are invisible or that change how the text reads.
func ClassifyContactName(s string) ContactNameKinds {
	var k ContactNameKinds
	for _, r := range s {
		switch {
		case isBidiControl(r):
			k.Bidi = true
		case unicode.IsControl(r):
			k.Control = true
		case isInvisibleFormat(r):
			k.Format = true
		}
	}
	return k
}

// ContactName is one contact's stored display name.
type ContactName struct {
	ID   ulid.ULID
	Name string
}

// ContactNameFix is one name to rewrite: the stored value it was computed
// from, and what to store instead.
type ContactNameFix struct {
	ID       ulid.ULID
	Old, New string
}

// ContactDisplayNames returns the caller's contacts that have a learned
// display name, in id order.
func (tx *UserTx) ContactDisplayNames(ctx context.Context) ([]ContactName, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, display_name FROM app.contacts
		 WHERE user_id = $1 AND display_name <> '' ORDER BY id`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: contact display names: %w", err)
	}
	defer rows.Close()
	var out []ContactName
	for rows.Next() {
		var (
			c  ContactName
			id string
		)
		if err := rows.Scan(&id, &c.Name); err != nil {
			return nil, fmt.Errorf("store: scan contact display name: %w", err)
		}
		if c.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", id, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyContactNameFixes writes one batch of cleaned names and one Contact
// `updated` row per contact actually changed, in one savepoint. It returns how
// many were changed and how many were skipped as stale.
func (tx *UserTx) ApplyContactNameFixes(ctx context.Context, fixes []ContactNameFix) (changed, stale int, err error) {
	err = tx.inSavepoint(ctx, func() error {
		var ids []ulid.ULID
		for _, f := range fixes {
			tag, e := tx.tx.Exec(ctx, `
				UPDATE app.contacts SET display_name = $4
				 WHERE user_id = $1 AND id = $2 AND display_name = $3`,
				tx.userID.String(), f.ID.String(), f.Old, f.New)
			if e != nil {
				return fmt.Errorf("store: clean contact name: %w", e)
			}
			if tag.RowsAffected() == 0 {
				stale++
				continue
			}
			ids = append(ids, f.ID)
		}
		changed = len(ids)
		return tx.AppendChanges(ctx, ObjectContact, ids, OpUpdated)
	})
	if err != nil {
		return 0, 0, err
	}
	return changed, stale, nil
}
