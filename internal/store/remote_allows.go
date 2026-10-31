// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Remembering "show remote content", per sender (#202, spec §11).
//
// Remote images are blocked by default and the affordance that loads them was
// deliberately per-message and non-persistent — "I trusted this one newsletter
// once" must not become "this sender may track me forever". That is correct about
// a BLANKET allow. What it left missing was anywhere to record a NARROW one, so
// the only available answers were "ask every single time" and "stop asking", and
// asking every time about the same newsletter is how a safe default turns into
// something a user works around.
//
// TWO THINGS THIS PACKAGE IS RESPONSIBLE FOR, and they are both about keeping the
// grant narrow rather than about loading images:
//
//  1. IT CANNOT EXPRESS A BLANKET ALLOW. kind is 'address' or 'domain' and the
//     column is CHECKed; there is no wildcard value and no kind='all'. A future UI
//     cannot offer "just load everything" because there is nowhere to put it.
//  2. EVERY GRANT IS LISTABLE AND REVOCABLE. ListRemoteAllows is what the settings
//     surface reads, and an allow that could not be shown or removed would be the
//     permanent consent the original comment feared, merely moved into a database.
//
// IT DOES NOT DECIDE WHETHER IMAGES LOAD. internal/api/sanitize.go still parks
// every remote URL unconditionally — a sanitizer that branches on a preference
// stops being the only path — so this is data the CLIENT reads to decide what to
// un-park (docs/hostile-content.md §2.3).

// RemoteAllowKind is the matcher: an exact From address, or a whole sender domain.
type RemoteAllowKind string

const (
	// RemoteAllowAddress matches one exact From address. The narrowest grant: what
	// the user approved is what is allowed.
	RemoteAllowAddress RemoteAllowKind = "address"
	// RemoteAllowDomain matches any sender at a domain. Broader, and it exists
	// because publishers rotate sending subdomains — one sender reaches the
	// mailbox from both mail.vendor.example and email.vendor-alt.example, so an
	// address-only grant would ask three times about one decision.
	RemoteAllowDomain RemoteAllowKind = "domain"
)

// RemoteAllow is one remembered decision.
type RemoteAllow struct {
	ID    ulid.ULID
	Kind  RemoteAllowKind
	Value string
}

// ErrRemoteAllowInvalid is returned for a kind or value this table will not hold.
var ErrRemoteAllowInvalid = errors.New("store: not a valid remote-content allow")

// normalizeRemoteAllow lowercases and trims, and REFUSES anything that is not a
// plain address or domain.
//
// The refusals are the point. A value with a wildcard, a space, or a leading dot
// is either an attempt at a blanket grant or a typo that would silently never
// match; both are better as an error than as a row. `citext` already makes the
// comparison case-insensitive, but normalising on the way in keeps the stored value
// the one the settings list will show, so a user reads back what they granted.
func normalizeRemoteAllow(kind RemoteAllowKind, value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || strings.ContainsAny(v, " \t\r\n*?/\\,;") {
		return "", fmt.Errorf("%w: %q", ErrRemoteAllowInvalid, value)
	}
	switch kind {
	case RemoteAllowAddress:
		// Exactly one @, with something either side. Not full RFC 5322 validation:
		// the value only has to match what the client compares a From against, and
		// anything that cannot be a From address cannot match one.
		at := strings.IndexByte(v, '@')
		if at <= 0 || at != strings.LastIndexByte(v, '@') || at == len(v)-1 {
			return "", fmt.Errorf("%w: address %q", ErrRemoteAllowInvalid, value)
		}
	case RemoteAllowDomain:
		// A domain, not an address and not a suffix pattern. A leading dot would
		// read as ".example.com matches anything under it", which is a wider grant
		// than the UI offers, so it is refused rather than quietly stored.
		if strings.Contains(v, "@") || strings.HasPrefix(v, ".") || strings.HasSuffix(v, ".") ||
			!strings.Contains(v, ".") {
			return "", fmt.Errorf("%w: domain %q", ErrRemoteAllowInvalid, value)
		}
	default:
		return "", fmt.Errorf("%w: kind %q", ErrRemoteAllowInvalid, kind)
	}
	return v, nil
}

// ListRemoteAllows returns every remembered allow, newest first.
//
// Newest first because this is read by the settings list, and the grant a user is
// most likely to want to reconsider is the one they just made.
func (tx *UserTx) ListRemoteAllows(ctx context.Context) ([]RemoteAllow, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, kind, value
		  FROM app.remote_allows
		 WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list remote allows: %w", err)
	}
	defer rows.Close()

	var out []RemoteAllow
	for rows.Next() {
		var (
			id, kind, value string
		)
		if err := rows.Scan(&id, &kind, &value); err != nil {
			return nil, fmt.Errorf("store: scan remote allow: %w", err)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: remote allow id %q: %w", id, err)
		}
		out = append(out, RemoteAllow{ID: parsed, Kind: RemoteAllowKind(kind), Value: value})
	}
	return out, rows.Err()
}

// AddRemoteAllow records one decision and appends its change_log row in the same
// transaction (CLAUDE.md's load-bearing invariant — this is a JMAP-visible object,
// so other devices must hear about it through /changes).
//
// GRANTING THE SAME ALLOW TWICE IS THE SAME ALLOW. A repeat returns the existing
// row and appends NO change row: nothing changed, and a /changes stream that
// reports a change nobody made costs every client a needless refetch. The UNIQUE
// constraint is what makes that safe under concurrency rather than a check-then-act
// race.
func (tx *UserTx) AddRemoteAllow(ctx context.Context, kind RemoteAllowKind, value string) (RemoteAllow, error) {
	v, err := normalizeRemoteAllow(kind, value)
	if err != nil {
		return RemoteAllow{}, err
	}

	id := NewULID()
	var got string
	err = tx.tx.QueryRow(ctx, `
		INSERT INTO app.remote_allows (id, user_id, kind, value)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, kind, value) DO NOTHING
		RETURNING id`,
		id.String(), tx.userID.String(), string(kind), v).Scan(&got)
	if err != nil {
		if isNoRows(err) {
			// Already granted. Return the row that exists, silently.
			existing, findErr := tx.remoteAllowByValue(ctx, kind, v)
			if findErr != nil {
				return RemoteAllow{}, findErr
			}
			return existing, nil
		}
		return RemoteAllow{}, fmt.Errorf("store: add remote allow: %w", err)
	}

	if _, err := tx.AppendChange(ctx, ObjectRemoteAllow, id, OpCreated); err != nil {
		return RemoteAllow{}, err
	}
	return RemoteAllow{ID: id, Kind: kind, Value: v}, nil
}

// RemoveRemoteAllow forgets one decision. Reported as found/not-found rather than
// as an error: revoking something already revoked is the state the caller wanted.
func (tx *UserTx) RemoveRemoteAllow(ctx context.Context, id ulid.ULID) (found bool, err error) {
	var deleted string
	err = tx.tx.QueryRow(ctx, `
		DELETE FROM app.remote_allows WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: remove remote allow: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectRemoteAllow, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// remoteAllowByValue finds an existing grant, for the already-granted path above.
func (tx *UserTx) remoteAllowByValue(ctx context.Context, kind RemoteAllowKind, value string) (RemoteAllow, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.remote_allows
		 WHERE user_id = $1 AND kind = $2 AND value = $3`,
		tx.userID.String(), string(kind), value).Scan(&id)
	if err != nil {
		return RemoteAllow{}, fmt.Errorf("store: find remote allow: %w", err)
	}
	parsed, err := ulid.Parse(id)
	if err != nil {
		return RemoteAllow{}, fmt.Errorf("store: remote allow id %q: %w", id, err)
	}
	return RemoteAllow{ID: parsed, Kind: kind, Value: value}, nil
}

// RemoteAllowsMatch reports whether `from`'s remote content would load WITHOUT
// asking, given this user's remembered grants (#202).
//
// ⚠ THE AUTHORITY FOR THIS PREDICATE IS web/src/core/remoteAllows.ts, NOT THIS
// FUNCTION. Nothing on the server branches on a grant: sanitize.go parks every
// remote URL unconditionally, precisely so a sanitizer never has a preference to
// get wrong (docs/hostile-content.md §2.3). The decision to un-park is the
// client's alone. This exists so a corpus report can answer "how many messages
// would actually fetch" in Go, and it is a SECOND implementation of a rule that
// lives in TypeScript — so it is written to mirror that file line for line, and
// its divergences are pinned by test rather than left to be discovered.
//
// BLAST RADIUS, because it changes how much this duplication matters: if this
// drifts from the client, a REPORT NUMBER is wrong. It cannot cause remote
// content to load, because it is not on any path that loads anything.
//
// ⚠ IT DELIBERATELY DOES NOT USE NormalizeAddress / AddressDomain, and the
// difference is not cosmetic:
//
//   - NormalizeAddress strips surrounding <>; the client's does not.
//   - AddressDomain returns "" when the address contains more than one "@";
//     the client takes the text after the LAST "@" regardless.
//
// Using the store helpers would make this report disagree with the client about
// exactly the malformed addresses a tracker is most likely to use. The question
// being asked is "what would the client do", so the client's rule is the right
// one to copy — see TestRemoteAllowsMatchMirrorsTheClient.
//
// ⚠ INCREMENT 3 MUST REVISIT THIS. A server-side image proxy needs its own
// decision, and for that a stricter, fail-closed reading of a malformed address
// is the safer default. Mirroring the client is right for measuring; it is not
// automatically right for fetching.
func RemoteAllowsMatch(from string, allows []RemoteAllow) bool {
	return MatchingRemoteAllow(from, allows) != nil
}

// MatchingRemoteAllow returns the grant that allows this sender, or nil.
//
// Exact address before domain, matching the client's matchingRemoteAllow: the
// more specific grant is the one a person would recognise as the reason.
func MatchingRemoteAllow(from string, allows []RemoteAllow) *RemoteAllow {
	addr := clientNormalizeAddress(from)
	if addr == "" || len(allows) == 0 {
		return nil
	}
	domain := clientAddressDomain(addr)
	for i := range allows {
		if allows[i].Kind == RemoteAllowAddress && clientNormalizeAddress(allows[i].Value) == addr {
			return &allows[i]
		}
	}
	for i := range allows {
		v := clientNormalizeAddress(allows[i].Value)
		if allows[i].Kind == RemoteAllowDomain && domain != "" && v == domain {
			return &allows[i]
		}
	}
	// An allow carrying an unrecognised kind matches nothing rather than being
	// guessed at, so a future kind cannot come to mean "everything" here.
	return nil
}

// clientNormalizeAddress mirrors normalizeAddress in web/src/core/remoteAllows.ts:
// trim and lowercase, and NOTHING else. It does not strip <> — see the note on
// RemoteAllowsMatch.
func clientNormalizeAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

// clientAddressDomain mirrors addressDomain in web/src/core/remoteAllows.ts: the
// text after the LAST "@", lowercased, or "" when there is none or it is last.
// It does NOT refuse addresses with several "@" the way AddressDomain does.
func clientAddressDomain(address string) string {
	at := strings.LastIndex(address, "@")
	if at < 0 || at == len(address)-1 {
		return ""
	}
	return strings.ToLower(address[at+1:])
}
