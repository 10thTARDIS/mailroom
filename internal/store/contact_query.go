// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// ContactQueryFilter is Contact/query's filter (#448/#374): Text matches the
// same address-or-display-name substring QueryContactsForAutocomplete uses
// (and, since #816, the user's own name for the contact); HasKey, when
// non-nil, additionally requires (true) or forbids (false) at least one
// app.contact_keys row for the contact — "N correspondent keys held" on the
// settings page, and "key held" on a compose recipient chip.
type ContactQueryFilter struct {
	Text   string
	HasKey *bool
	// Address, when set, matches one exact address (case-insensitively — the
	// column is citext): how the sender card finds THE contact behind a From
	// rather than every contact whose address contains it (#59).
	Address string
	// IsVIP / IsBlocked, when non-nil, require (true) or forbid (false) the
	// user's marker — the Settings lists of VIPs and blocked senders (#59).
	IsVIP     *bool
	IsBlocked *bool
	// IsSaved, when non-nil, requires (true) or forbids (false) a saved
	// contact (#816): the Contacts page sends true.
	IsSaved *bool
	// OnlyCoRecipient, when non-nil, requires (true) or forbids (false) that
	// NO address of the contact has mail found: none From it (non-draft) and
	// none sent to it by the user (outbound_count, ingest's count of the
	// user's sent mail to it). The addresses seen only on other people's mail
	// (#816). The sent side is outbound_count rather than the blob walk
	// contact-dates-report does: production measured 53 vs 51, close enough
	// for a filter, and a query cannot read blobs.
	OnlyCoRecipient *bool
}

// Contact/query sorts (#816). The empty sort is the autocomplete order.
const (
	ContactSortDefault    = ""
	ContactSortName       = "name"
	ContactSortLastMailAt = "lastMailAt"
)

// ContactPage is the window of a Contact/query: the sort, the RFC 8620 §5.5
// position (negative counts back from the end) and the limit. LimitSet
// distinguishes limit: 0 from "no limit sent" (#469).
type ContactPage struct {
	Sort      string
	Ascending bool
	Position  int
	Limit     int
	LimitSet  bool
}

// ErrContactSortInvalid: a sort property other than name or lastMailAt.
var ErrContactSortInvalid = errors.New("store: contact sort must be name or lastMailAt")

// QueryContacts is Contact/query in its pre-#816 form (the default sort,
// position 0); see QueryContactsPage.
func (tx *UserTx) QueryContacts(ctx context.Context, f ContactQueryFilter, limit int, limitSet bool) ([]ulid.ULID, int, error) {
	ids, total, _, err := tx.QueryContactsPage(ctx, f, ContactPage{Limit: limit, LimitSet: limitSet})
	return ids, total, err
}

// QueryContactsPage is Contact/query's filter+window+total implementation.
//
// Unlike QueryContactsForAutocomplete (one row per matching ADDRESS, collapsed
// to contacts by the caller), this groups by CONTACT in SQL: the LIMIT and the
// returned total both count distinct contacts, so a contact holding two
// matching addresses can neither crowd two slots out of one page nor count
// twice toward total.
//
// page.LimitSet distinguishes limit: 0 (RFC 8620 §5.5 — "no ids, but tell me
// the total") from "no limit sent" (apply the default autocomplete page),
// exactly as store.EmailQuery.LimitSet does for Email/query — conflating the
// two was #469's bug in that sibling query. total is computed
// UNCONDITIONALLY, in its own COUNT(DISTINCT ...) query against the same
// predicate, so it is correct whatever the window. The returned position is
// the one actually used (a negative one resolved against total, clamped at 0).
//
// SORTS (#816): the default (no sort) is the autocomplete order, unchanged:
// most-sighted first. name is the displayed name (the user's own, else the
// learned one, else the address), case-insensitive. lastMailAt is the latest
// last_seen_at over ALL the contact's addresses, not only the matching ones:
// the same value Contact.lastMailAt shows. Ties break on the id, so paging is
// stable.
//
// A tombstone (merged_into set) has no addresses, so the address join already
// excludes it; the predicate is stated anyway so the rule does not rest on
// that.
func (tx *UserTx) QueryContactsPage(ctx context.Context, f ContactQueryFilter, page ContactPage) ([]ulid.ULID, int, int, error) {
	text := strings.TrimSpace(f.Text)
	pattern := "%" + escapeLike(text) + "%"
	textCond := `( $2::text = '' OR ca.address ILIKE $3 ESCAPE '\' OR c.display_name ILIKE $3 ESCAPE '\'
	               OR c.user_display_name ILIKE $3 ESCAPE '\' )`

	cond := ` AND c.merged_into IS NULL`
	switch {
	case f.HasKey == nil:
		// no constraint
	case *f.HasKey:
		cond += ` AND EXISTS (SELECT 1 FROM app.contact_keys k WHERE k.user_id = ca.user_id AND k.contact_id = ca.contact_id AND k.removed_at IS NULL)`
	default:
		cond += ` AND NOT EXISTS (SELECT 1 FROM app.contact_keys k WHERE k.user_id = ca.user_id AND k.contact_id = ca.contact_id AND k.removed_at IS NULL)`
	}
	// Literal SQL, no binds: each is a fixed predicate chosen by a Go value.
	// $4 is the exact address, bound in both queries (empty = no constraint).
	if f.IsVIP != nil {
		cond += ` AND c.vip = ` + boolLit(*f.IsVIP)
	}
	if f.IsBlocked != nil {
		cond += ` AND c.blocked = ` + boolLit(*f.IsBlocked)
	}
	if f.IsSaved != nil {
		if *f.IsSaved {
			cond += ` AND c.saved_at IS NOT NULL`
		} else {
			cond += ` AND c.saved_at IS NULL`
		}
	}
	if f.OnlyCoRecipient != nil {
		// One probe of messages_user_bulk (user_id, from_address) per address.
		mail := `EXISTS (SELECT 1 FROM app.contact_addresses x
		                  WHERE x.user_id = ca.user_id AND x.contact_id = ca.contact_id
		                    AND (x.outbound_count > 0 OR EXISTS (
		                         SELECT 1 FROM app.messages m
		                          WHERE m.user_id = x.user_id AND m.from_address = x.address AND NOT m.is_draft)))`
		if *f.OnlyCoRecipient {
			cond += ` AND NOT ` + mail
		} else {
			cond += ` AND ` + mail
		}
	}
	cond += ` AND ( $4::text = '' OR ca.contact_id IN (SELECT x.contact_id FROM app.contact_addresses x WHERE x.user_id = $1 AND x.address = $4::citext) )`
	exact := NormalizeAddress(f.Address)

	dir := "DESC"
	if page.Ascending {
		dir = "ASC"
	}
	var order string
	switch page.Sort {
	case ContactSortDefault:
		order = `max(ca.seen_count) DESC, max(ca.last_seen_at) DESC, c.id`
	case ContactSortName:
		order = `lower(COALESCE(NULLIF(c.user_display_name, ''), NULLIF(c.display_name, ''), min(ca.address)::text)) ` + dir + `, c.id ` + dir
	case ContactSortLastMailAt:
		order = `(SELECT max(y.last_seen_at) FROM app.contact_addresses y
		           WHERE y.user_id = c.user_id AND y.contact_id = c.id) ` + dir + `, c.id ` + dir
	default:
		return nil, 0, 0, ErrContactSortInvalid
	}

	var total int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(DISTINCT ca.contact_id)
		  FROM app.contact_addresses ca
		  JOIN app.contacts c ON c.user_id = ca.user_id AND c.id = ca.contact_id
		 WHERE ca.user_id = $1 AND `+textCond+cond,
		tx.userID.String(), text, pattern, exact).Scan(&total); err != nil {
		return nil, 0, 0, fmt.Errorf("store: count contacts for query: %w", err)
	}

	pos := page.Position
	if pos < 0 {
		pos += total
		if pos < 0 {
			pos = 0
		}
	}
	limit := page.Limit
	// limit: 0 is a legitimate request for zero ids plus the total (RFC 8620
	// §5.5) — return now, before running a page query whose LIMIT 0 would just
	// waste a round trip for the same empty result. So is a position past the
	// end.
	if (page.LimitSet && limit == 0) || pos >= total {
		return nil, total, pos, nil
	}
	switch {
	case !page.LimitSet:
		limit = defaultAutocompleteLimit
	case limit > maxAutocompleteLimit:
		limit = maxAutocompleteLimit
	}

	rows, err := tx.tx.Query(ctx, `
		SELECT c.id
		  FROM app.contact_addresses ca
		  JOIN app.contacts c ON c.user_id = ca.user_id AND c.id = ca.contact_id
		 WHERE ca.user_id = $1 AND `+textCond+cond+`
		 GROUP BY c.id
		 ORDER BY `+order+`
		 LIMIT $5 OFFSET $6`,
		tx.userID.String(), text, pattern, exact, limit, pos)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: query contacts: %w", err)
	}
	defer rows.Close()

	var ids []ulid.ULID
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			return nil, 0, 0, fmt.Errorf("store: scan contact id: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("store: parse contact id %q: %w", idStr, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: query contacts: %w", err)
	}
	return ids, total, pos, nil
}

// MaxContactQueryLimit is the largest page Contact/query returns.
const MaxContactQueryLimit = maxAutocompleteLimit
