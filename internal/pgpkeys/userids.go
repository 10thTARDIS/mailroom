// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// The user IDs a key carries, as a client may show them (#812).
//
// ⚠ ONLY USER IDS WITH A VERIFYING SELF-SIGNATURE COUNT, read through the same
// parser Merge uses (readKeyBlock), which verifies every self-signature against
// the primary key and drops what does not verify. A user ID packet anyone can
// append to a key, with no signature or a forged one, is not the key holder's
// claim and is never listed. go-crypto then reads the normalised key for the
// two facts it decides: revocation and which user ID is primary.
//
// ⚠ THE VALUES ARE UNTRUSTED STRINGS. A correspondent's key is attacker-made,
// and so is an imported own key. Every name and email is cleaned before it
// leaves this package (cleanUserIDField): control characters and Unicode format
// characters (bidi overrides, zero-width joiners) are removed, the result is
// trimmed and capped at MaxUserIDFieldRunes, and at most MaxUserIDs are listed.
// Clients must still render them as text, never as markup.

import (
	"bytes"
	"strings"
	"time"
	"unicode"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// UserID is one user ID of a key.
type UserID struct {
	// Name is the display-name part ("Alice Example"); for a user ID that is
	// not in "Name <email>" form, the whole cleaned user ID.
	Name string
	// Email is the address part, as written in the key (cleaned, not
	// lowercased). Empty when the user ID carries none.
	Email string
	// Revoked: the key holder revoked this user ID.
	Revoked bool
	// Primary: go-crypto's choice of primary user ID (the primary-user-ID flag,
	// then the newest self-signature; a revoked one loses).
	Primary bool
}

const (
	// MaxUserIDs bounds how many user IDs are listed for one key.
	MaxUserIDs = 32
	// MaxUserIDFieldRunes bounds one name or email, in runes.
	MaxUserIDFieldRunes = 256
)

// UserIDs lists binary's self-signed user IDs at now: the primary first, then
// the rest in the order the key carries them.
func UserIDs(binary []byte, now time.Time) ([]UserID, error) {
	kb, err := readKeyBlock(binary)
	if err != nil {
		return nil, err
	}
	normalised, err := kb.serialize()
	if err != nil {
		return nil, err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(normalised))
	if err != nil || len(entities) == 0 {
		return nil, ErrMalformed
	}
	e := entities[0]
	primary := primaryIdentity(e)

	var first, rest []UserID
	for _, u := range kb.uids {
		if len(u.sigs.order) == 0 {
			continue // no verifying self-signature: not the holder's claim
		}
		ident := e.Identities[u.uid.Id]
		if ident == nil {
			continue
		}
		out := UserID{Revoked: ident.Revoked(now), Primary: ident == primary}
		name, email := parseUserID(u.uid.Id)
		if name == "" && email == "" {
			name = u.uid.Id
		}
		out.Name, out.Email = cleanUserIDField(name), cleanUserIDField(email)
		if out.Primary {
			first = append(first, out)
		} else {
			rest = append(rest, out)
		}
	}
	all := append(first, rest...)
	if len(all) > MaxUserIDs {
		all = all[:MaxUserIDs]
	}
	return all, nil
}

// cleanUserIDField removes control and format characters (the latter include
// bidi overrides such as U+202E, which can make "moc.elpmaxe@" read as
// "example.com"), trims, and caps the length in runes.
func cleanUserIDField(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			continue
		}
		if n == MaxUserIDFieldRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
