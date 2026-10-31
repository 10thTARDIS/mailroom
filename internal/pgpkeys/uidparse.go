// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

// One parser for user IDs (#822), used by every consumer: the key's address
// list (Parse), the encryption and signature-attribution check
// (namingUserID / NamesAddress) and the display list (UserIDs). They must not
// disagree, so none of them reads go-crypto's packet.UserId.Email: that takes
// the FIRST <...> group, so "Alice <b>Bold</b> <alice@example.org>" yields the
// address "b".
//
// The address is the LAST <...> group (RFC 5322 puts the angle-addr at the end
// of a mailbox), and only if
//   - the user ID ends with that group's ">" (trailing white space aside), so
//     text after the address is ambiguous and yields no address;
//   - the group holds no further "<" or ">";
//   - net/mail accepts it as an address and returns it unchanged (so quoted
//     local parts, comments, display names and route syntax do not slip in).
//
// A user ID that is just a bare, valid address ("alice@example.org") is that
// address. Anything else has NO address: the key then names nothing, which
// fails closed (no encryption to it, no signature attributed through it).
// The parse can only ever return text that is literally inside the user ID.

import (
	"net/mail"
	"sort"
	"strings"
	"unicode"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// parseUserID splits a user ID into the display name and the address; email is
// empty when the user ID carries no unambiguous address. name is the text
// before the address group, trimmed (the whole user ID when email is empty).
func parseUserID(id string) (name, email string) {
	s := strings.TrimSpace(id)
	if strings.HasSuffix(s, ">") {
		if open := strings.LastIndex(s, "<"); open >= 0 {
			if addr, ok := plainAddress(s[open+1 : len(s)-1]); ok {
				return strings.TrimSpace(s[:open]), addr
			}
		}
		return s, ""
	}
	if addr, ok := plainAddress(s); ok {
		return "", addr
	}
	return s, ""
}

// plainAddress accepts only a bare addr-spec that net/mail returns unchanged.
func plainAddress(s string) (string, bool) {
	if s == "" || strings.ContainsAny(s, "<>") {
		return "", false
	}
	for _, r := range s {
		// Control and format characters (bidi overrides, zero-width joiners)
		// have no place in an address and can make it read as another.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	a, err := mail.ParseAddress("<" + s + ">")
	if err != nil || a.Address != s {
		return "", false
	}
	return s, true
}

// userIDAddress is the address of a user ID (empty when none).
func userIDAddress(id string) string {
	_, email := parseUserID(id)
	return email
}

// primaryIdentity picks the primary user ID like go-crypto (revocations, then
// the primary-user-ID flag, then the newest self-signature) but breaks the
// remaining ties by user ID order instead of Go's random map order, so two keys
// with the same user IDs created in the same second never flip (#822).
func primaryIdentity(e *openpgp.Entity) *openpgp.Identity {
	ids := make([]string, 0, len(e.Identities))
	for id := range e.Identities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var best *openpgp.Identity
	for _, id := range ids {
		if cand := e.Identities[id]; cand != nil && preferIdentity(best, cand) {
			best = cand
		}
	}
	return best
}

func preferIdentity(cur, cand *openpgp.Identity) bool {
	if cur == nil {
		return true
	}
	if len(cur.Revocations) != len(cand.Revocations) {
		return len(cur.Revocations) > len(cand.Revocations)
	}
	if cur.SelfSignature == nil {
		return true
	}
	if cand.SelfSignature == nil {
		return false
	}
	curP := cur.SelfSignature.IsPrimaryId != nil && *cur.SelfSignature.IsPrimaryId
	candP := cand.SelfSignature.IsPrimaryId != nil && *cand.SelfSignature.IsPrimaryId
	if curP != candP {
		return candP
	}
	return cand.SelfSignature.CreationTime.After(cur.SelfSignature.CreationTime)
}
