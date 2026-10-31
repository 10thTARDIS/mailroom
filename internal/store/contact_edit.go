// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

// THE USER'S EDITS TO A CONTACT (#59 markers; #816 saved, name and notes;
// ADR-041). One method, SetContactMarkers, so every edit has the same change
// row discipline: exactly one Contact `updated` row when anything visible
// changed, none for a no-op, in one savepoint with the write.

// Limits on the user's own text (ADR-041 §2). The schema CHECKs the same
// numbers (migration 00072), so a path that skipped this file still could not
// store more.
const (
	MaxContactUserName = 200
	MaxContactNotes    = 4000
)

// Errors for text the user may not store. The API reports them as
// invalidProperties naming the field.
var (
	ErrContactUserNameTooLong = fmt.Errorf("store: a contact name is at most %d characters", MaxContactUserName)
	ErrContactNotesTooLong    = fmt.Errorf("store: contact notes are at most %d characters", MaxContactNotes)
	// ErrContactSaveConflict: one patch both marks a contact VIP (which saves
	// it) and un-saves it.
	ErrContactSaveConflict = errors.New("store: a contact cannot be marked VIP and removed from contacts in one change")
)

// ContactMarkersPatch is a partial update of a contact's markers. A nil
// pointer leaves that field unchanged.
type ContactMarkersPatch struct {
	VIP              *bool
	Blocked          *bool
	BlockDestination *string
	// Saved: true saves (keeps an existing saved_at), false un-saves. Un-saving
	// deletes nothing: keys, history, VIP and block stay (#816).
	Saved *bool
	// UserNameSet: UserName is part of the patch. A nil UserName (or one that
	// is empty once cleaned) clears the user's name for the contact.
	UserNameSet bool
	UserName    *string
	// Notes, when non-nil, replaces the notes ("" clears them).
	Notes *string
}

// isBidiControl reports the explicit directional formatting characters
// (embeddings, overrides and isolates). They are format characters, not
// controls, so unicode.IsControl misses them, and in a name shown next to an
// address they can reorder what the reader sees.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

// CleanContactUserName is what a user's name for a contact is stored as:
// every control and directional formatting character removed (a line break
// in a name included), surrounding space trimmed. "" means "no name": the
// caller clears the field. Over MaxContactUserName characters (counted after
// cleaning, as the CHECK counts them) is an error, never a silent cut.
func CleanContactUserName(s string) (string, error) {
	s = cleanContactName(s, false)
	if utf8.RuneCountInString(s) > MaxContactUserName {
		return "", ErrContactUserNameTooLong
	}
	return s, nil
}

// CleanLearnedContactName is what a display name learned from a sender's From
// header is stored as (#831): the same cleaning as CleanContactUserName, plus
// every other invisible format character (zero-width space, joiner and
// non-joiner, word joiner, byte-order mark, soft hyphen), because a name that
// arrives in mail is hostile text and every client, the JMAP API and the MCP
// server included, reads the stored string. "" means "no name". A name over
// MaxContactUserName characters is cut at that length rather than refused:
// there is no author to tell, and the ingest must not fail on it.
func CleanLearnedContactName(s string) string {
	s = cleanContactName(s, true)
	if utf8.RuneCountInString(s) > MaxContactUserName {
		s = strings.TrimSpace(string([]rune(s)[:MaxContactUserName]))
	}
	return s
}

// isInvisibleFormat is every character a renderer draws as nothing: Unicode
// format (Cf) characters plus the rest of Default_Ignorable_Code_Point — the
// Hangul fillers (U+3164 and friends), variation selectors, the combining
// grapheme joiner (#853 review: a learned name of U+3164s is blank on screen
// and still "a name"). DerivedCoreProperties' Default_Ignorable is
// Other_Default_Ignorable_Code_Point + Cf + Variation_Selector, minus a few Cf
// exceptions this removes anyway.
func isInvisibleFormat(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r)
}

// cleanContactName is the one name sanitiser: control and directional
// formatting characters removed, surrounding space trimmed, and with
// dropFormat every other Unicode format (Cf) character too.
func cleanContactName(s string, dropFormat bool) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || isBidiControl(r) || (dropFormat && isInvisibleFormat(r)) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// CleanContactNotes is what a contact's notes are stored as: line breaks
// normalised to \n, tabs and newlines kept, every other control and
// directional formatting character removed, surrounding space trimmed. Over
// MaxContactNotes characters is an error.
func CleanContactNotes(s string) (string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) || isBidiControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxContactNotes {
		return "", ErrContactNotesTooLong
	}
	return s, nil
}

// contactEditable is the user-editable state of one contact, as locked.
type contactEditable struct {
	vip, blocked bool
	dest         string
	blockedAt    *time.Time
	savedAt      *time.Time
	userName     *string
	notes        string
}

// SetContactMarkers applies patch to contact id and appends exactly one
// Contact `updated` change_log row when anything actually changed (none for a
// no-op), in the same savepoint as the write — so a caller that recovers from
// an error can never commit the write without its change row, or the reverse.
//
// blocked_at is stamped when a contact becomes blocked and cleared when it is
// unblocked; re-blocking an already blocked contact (to change where its mail
// goes, say) keeps the original stamp. isVip:true also SAVES the contact
// (#816); blocking does not. A merged-away contact (a tombstone) is not found.
func (tx *UserTx) SetContactMarkers(ctx context.Context, id ulid.ULID, patch ContactMarkersPatch) (changed bool, err error) {
	if patch.BlockDestination != nil {
		d := *patch.BlockDestination
		if d != BlockToJunk && d != BlockToTrash {
			return false, ErrBlockDestinationInvalid
		}
	}
	if patch.VIP != nil && *patch.VIP && patch.Saved != nil && !*patch.Saved {
		return false, ErrContactSaveConflict
	}
	var userName *string
	if patch.UserNameSet && patch.UserName != nil {
		clean, cerr := CleanContactUserName(*patch.UserName)
		if cerr != nil {
			return false, cerr
		}
		if clean != "" {
			userName = &clean
		}
	}
	var notes *string
	if patch.Notes != nil {
		clean, cerr := CleanContactNotes(*patch.Notes)
		if cerr != nil {
			return false, cerr
		}
		notes = &clean
	}

	err = tx.inSavepoint(ctx, func() error {
		var cur contactEditable
		e := tx.tx.QueryRow(ctx, `
			SELECT vip, blocked, block_destination, blocked_at, saved_at, user_display_name, notes
			  FROM app.contacts
			 WHERE user_id = $1 AND id = $2 AND merged_into IS NULL
			 FOR UPDATE`,
			tx.userID.String(), id.String()).Scan(&cur.vip, &cur.blocked, &cur.dest, &cur.blockedAt,
			&cur.savedAt, &cur.userName, &cur.notes)
		if isNoRows(e) {
			return ErrContactNotFound
		}
		if e != nil {
			return fmt.Errorf("store: set contact markers (read): %w", e)
		}

		next := cur
		if patch.VIP != nil {
			next.vip = *patch.VIP
		}
		if patch.Blocked != nil {
			next.blocked = *patch.Blocked
		}
		if patch.BlockDestination != nil {
			next.dest = *patch.BlockDestination
		}
		if patch.UserNameSet {
			next.userName = userName
		}
		if notes != nil {
			next.notes = *notes
		}
		saveNow := (patch.Saved != nil && *patch.Saved) || (patch.VIP != nil && *patch.VIP)
		unsave := patch.Saved != nil && !*patch.Saved

		visible := next.vip != cur.vip || next.blocked != cur.blocked || next.dest != cur.dest ||
			!sameStringPtr(next.userName, cur.userName) || next.notes != cur.notes ||
			(saveNow && cur.savedAt == nil) || (unsave && cur.savedAt != nil)
		if !visible {
			return nil // a true no-op: nothing changed, so no change row
		}
		if _, e := tx.tx.Exec(ctx, `
			UPDATE app.contacts c
			   SET vip               = $3,
			       blocked           = $4,
			       block_destination = $5,
			       blocked_at        = CASE WHEN NOT $4 THEN NULL ELSE COALESCE(c.blocked_at, now()) END,
			       saved_at          = CASE WHEN $6 THEN NULL
			                                WHEN $7 THEN COALESCE(c.saved_at, now())
			                                ELSE c.saved_at END,
			       user_display_name = $8,
			       notes             = $9
			 WHERE c.user_id = $1 AND c.id = $2`,
			tx.userID.String(), id.String(), next.vip, next.blocked, next.dest,
			unsave, saveNow, next.userName, next.notes); e != nil {
			return fmt.Errorf("store: set contact markers: %w", e)
		}
		changed = true
		_, e = tx.AppendChange(ctx, ObjectContact, id, OpUpdated)
		return e
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
