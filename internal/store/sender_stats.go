// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// THE CLEAN-UP ASSISTANT'S RANKING (#53): who sends the user the most mail,
// and whose mail the user leaves alone.
//
// ONE PASS, ONE GROUP-BY. Every statistic is an aggregate over the caller's
// messages in the window, grouped by the chosen key, so the cost is one scan of
// the user's rows (or of the window's, through messages_user_received) and a
// hash aggregate — never a query per sender. Measured on a seeded 25k-message
// mailbox in sender_stats_integration_test.go, which logs the plan and the time.
//
// WHAT COUNTS. A message counts when it is the user's, NOT deleted and NOT
// junk by EXACTLY the rules a search applies by default (Email/query's trash
// exclusion — trashed_at, or in a Trash folder — and junkSQL — the provider's
// verdict, or in a Junk folder), NOT a draft, has a From address, and is not
// FROM THE USER: an address of one of their fixed identities, or any address
// under a domain_wildcard identity's domain (the same "me" the send path uses
// — store.ResolveSendIdentity). Sharing the search's definition is what makes
// a row's count the count its action query finds (#155's lesson: a number
// beside a list must be the list's): a first version that left folder-only
// junk in counted one message the `from:` search did not return.
//
// "NEGLECT", PRECISELY. A message is NEGLECTED when either
//
//   - it is UNREAD now (is_seen false — the merged \Seen of every copy, so a
//     message read in another client is read here too), wherever it sits:
//     still in the inbox ("left unread") or already archived ("archived
//     unread"); or
//   - Mailroom recorded an `archive_unread` feedback event for it — the user
//     archived it from Mailroom before reading it (api/feedback.go), even if
//     something marked it read afterwards (another client's "mark read on
//     move", a bulk mark-read).
//
// The neglect SHARE is neglected / total for the sender. Ranking by neglect
// orders by the neglected COUNT first and the share second, so one ignored
// message from a stranger does not outrank two hundred from a newsletter.
//
// THE THREE GROUPINGS, because newsletters rotate sender addresses:
//
//   - address: the exact From address (citext, lower-cased for the key);
//   - domain:  the From address's domain, exactly (no subdomain folding — subdomains
//     are separate rows, and the client's actions use SenderStats/emailIds,
//     which matches this exact key, so the count a confirm shows is the row's);
//   - list:    the List-Id, normalised as search.normalizeListID does (the id
//     inside the last <…>, lower-cased, '@' as '.', outer dots trimmed), so a
//     row's key is exactly what `list:<key>` searches for. Mail with no List-Id
//     is not in this grouping at all.
//
// RLS AND SCOPING: every relation is read through the caller's UserTx, and
// every subquery is bound to $1, the caller's own id — another user's mail,
// identities and feedback can neither count nor appear.

// SenderGroup is the key a SenderStats query groups by.
type SenderGroup string

// The three groupings.
const (
	SenderByAddress SenderGroup = "address"
	SenderByDomain  SenderGroup = "domain"
	SenderByList    SenderGroup = "list"
)

// SenderSort is the ranking.
type SenderSort string

// The two rankings.
const (
	SenderSortVolume  SenderSort = "volume"
	SenderSortNeglect SenderSort = "neglect"
)

// ErrSenderStatsInvalid is a group or sort outside the closed sets.
var ErrSenderStatsInvalid = errors.New("store: invalid sender stats query")

// MaxSenderStatsPage bounds one page of senders.
const MaxSenderStatsPage = 200

// maxGroupSenders bounds the addresses listed per group (a domain or a list
// that has used more is reported with AddressCount, not enumerated).
const maxGroupSenders = 20

// SenderStatsQuery is one ranking request.
type SenderStatsQuery struct {
	Group SenderGroup
	Sort  SenderSort
	// Since, when non-nil, keeps only mail received at or after it.
	Since    *time.Time
	Position int
	Limit    int
}

// SenderAddress is one address inside a group, with its contact's markers.
// ContactID is nil for an address no contact holds (it should not happen —
// ingest creates one for every sender — but a row must not claim one).
type SenderAddress struct {
	Address   string
	ContactID *ulid.ULID
	// Name is the contact's display name ("" when none is known).
	Name    string
	VIP     bool
	Blocked bool
	// Correspondent is true when the user has WRITTEN to this address —
	// contact_addresses.outbound_count > 0, the correspondence signal ingest
	// keeps from the user's own sent mail and replies (#157). A clean-up
	// block of a whole domain or list must not sweep these up by default.
	Correspondent bool
}

// SenderStat is one ranked sender (or domain, or list).
type SenderStat struct {
	Key          string
	DisplayName  string
	Total        int
	Unread       int
	Neglected    int
	LastReceived time.Time
	// AddressCount is how many distinct From addresses the group holds;
	// Addresses lists at most maxGroupSenders of them, newest first.
	AddressCount int
	Addresses    []SenderAddress
	// UnsubscribeMessageID is the newest message in the group that carries a
	// List-Unsubscribe header — what the client loads to offer the existing
	// unsubscribe affordance. Nil when none does.
	UnsubscribeMessageID *ulid.ULID
	// Actionable reports whether Key can be searched for as EXACTLY this row
	// (SenderKeyActionable). A row that is not is still counted and shown,
	// but no action may be offered on it.
	Actionable bool
}

// SenderKeyActionable reports whether a ranking key, in its grouping, can be
// handed to the search as exactly that row:
//
//   - address: a well-formed local@domain by the store's own rule
//     (AddressDomain: exactly one '@', a non-empty local part and domain).
//     A From with no local part — "example.com", "@example.com",
//     "*.example.com", ".example.com" — would be read by `from:` as a DOMAIN
//     (#712: domain and subdomains), so archiving "one address" would move a
//     whole domain's mail. Such a key is never actionable.
//   - domain: a dotted host name with no '@', no wildcard and no empty label —
//     one exact domain key (a subdomain is a separate row, not part of this one).
//   - list: a non-empty List-Id with no characters the query syntax splits on.
//
// In every grouping, whitespace, quotes and brackets are refused: the query
// syntax would split or group on them.
func SenderKeyActionable(g SenderGroup, key string) bool {
	if key == "" || strings.ContainsAny(key, " \t\r\n\"()") {
		return false
	}
	switch g {
	case SenderByAddress:
		// The search package's own reading of `from:<key>` is checked by the
		// API layer too (search.FromExactAddress — the store cannot import
		// search); here: exactly one '@', a local part, and a domain whose
		// every label is non-empty (no leading, trailing or doubled dot).
		d := AddressDomain(key)
		if d == "" || strings.HasPrefix(key, "*") || NormalizeAddress(key) != key {
			return false
		}
		for _, l := range strings.Split(d, ".") {
			if l == "" {
				return false
			}
		}
		return true
	case SenderByDomain:
		if strings.ContainsAny(key, "@*") {
			return false
		}
		labels := strings.Split(key, ".")
		if len(labels) < 2 {
			return false
		}
		for _, l := range labels {
			if l == "" {
				return false
			}
		}
		return true
	case SenderByList:
		// A List-Id key never holds '@' (normalised to '.') or '*' (trimmed);
		// one that does did not come from the ranking.
		return !strings.ContainsAny(key, "@*")
	}
	return false
}

// MaxSenderIDsPage caps one page of SenderMessageIDs — the sender-wide move's
// own cap (#708), so "Move the rest" pages through the same way.
const MaxSenderIDsPage = 1000

// SenderIDsQuery names exactly one ranking row: its grouping, its key and the
// window the ranking was computed over.
type SenderIDsQuery struct {
	Group    SenderGroup
	Key      string
	Since    *time.Time
	Position int
	Limit    int
}

// SenderMessageIDs enumerates the messages ONE ranking row counted — the same
// `mine` (senderMineCTE) filtered to that row's key — newest first, capped at
// MaxSenderIDsPage with an offset. total is how many the row holds; it equals
// the row's Total for the same window, by construction (and by test).
//
// This is what every clean-up action moves (#53 security review): rebuilding
// a `from:`/`list:` search from a key could reach mail the row never showed
// (list: matches sub-lists; from: reads a domain-shaped key as a domain).
// RLS-scoped like every read here; another user's key matches nothing.
func (tx *UserTx) SenderMessageIDs(ctx context.Context, q SenderIDsQuery) (ids []ulid.ULID, total int, err error) {
	key, err := senderKeySQL(q.Group)
	if err != nil {
		return nil, 0, err
	}
	if !SenderKeyActionable(q.Group, q.Key) {
		return nil, 0, ErrSenderStatsInvalid
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxSenderIDsPage {
		limit = MaxSenderIDsPage
	}
	pos := q.Position
	if pos < 0 {
		pos = 0
	}
	var since any
	if q.Since != nil {
		since = q.Since.UTC()
	}
	mine, wild, err := tx.myAddressesAndDomains(ctx)
	if err != nil {
		return nil, 0, err
	}
	excl, err := tx.senderStatsExclusions(ctx)
	if err != nil {
		return nil, 0, err
	}
	args := []any{tx.userID.String(), since, limit, pos, mine, wild}
	args = append(args, excl.args...)
	args = append(args, q.Key)
	keyPH := fmt.Sprintf("$%d", len(args))
	rows, err := tx.tx.Query(ctx, senderMineCTE(key, excl.sql)+`
		SELECT id::text, count(*) OVER () FROM mine
		 WHERE k = `+keyPH+`
		 ORDER BY received_at DESC, id
		 LIMIT $3 OFFSET $4`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: sender message ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s, &total); err != nil {
			return nil, 0, fmt.Errorf("store: scan sender message id: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, 0, fmt.Errorf("store: parse message id %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: sender message ids: %w", err)
	}
	if len(ids) == 0 && pos > 0 {
		// Past the end: learn the total from the first id.
		_, total, err = tx.SenderMessageIDs(ctx, SenderIDsQuery{Group: q.Group, Key: q.Key, Since: q.Since, Limit: 1})
		if err != nil {
			return nil, 0, err
		}
	}
	return ids, total, nil
}

// senderKeySQL returns the grouping expression over `m`.
func senderKeySQL(g SenderGroup) (string, error) {
	switch g {
	case SenderByAddress:
		return `lower(m.from_address::text)`, nil
	case SenderByDomain:
		return `NULLIF(lower(split_part(m.from_address::text, '@', 2)), '')`, nil
	case SenderByList:
		// search.normalizeListID, in SQL: the id inside the LAST <…> when there
		// is one, else the raw value; lower-cased; '@' -> '.'; '<', '>' and
		// outer dots trimmed. An empty result is no list.
		return `NULLIF(btrim(replace(lower(btrim(COALESCE(substring(m.list_id from '<([^<>]*)>[^<]*$'), m.list_id))), '@', '.'), '.<>* '), '')`, nil
	}
	return "", ErrSenderStatsInvalid
}

// SenderStats ranks the caller's senders. total is the number of groups in the
// window (the whole ranking, not the page).
func (tx *UserTx) SenderStats(ctx context.Context, q SenderStatsQuery) (stats []SenderStat, total int, err error) {
	key, err := senderKeySQL(q.Group)
	if err != nil {
		return nil, 0, err
	}
	var order string
	switch q.Sort {
	case SenderSortVolume, "":
		order = `g.total DESC, g.neglected DESC, g.last DESC, g.k`
	case SenderSortNeglect:
		order = `g.neglected DESC, (g.neglected::float8 / g.total) DESC, g.total DESC, g.k`
	default:
		return nil, 0, ErrSenderStatsInvalid
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxSenderStatsPage {
		limit = 50
	}
	pos := q.Position
	if pos < 0 {
		pos = 0
	}
	var since any
	if q.Since != nil {
		since = q.Since.UTC()
	}

	// "Me", read once and passed as two small arrays: comparing every message
	// against the identities table row by row (citext casts and all) was the
	// largest single cost in the measured plan.
	mine, wild, err := tx.myAddressesAndDomains(ctx)
	if err != nil {
		return nil, 0, err
	}
	excl, err := tx.senderStatsExclusions(ctx)
	if err != nil {
		return nil, 0, err
	}
	stats, total, err = tx.senderStatsPage(ctx, key, order, since, limit, pos, mine, wild, excl)
	if err != nil {
		return nil, 0, err
	}
	if len(stats) == 0 && pos > 0 {
		// Past the end of the ranking: an empty page carries no row to hold
		// the total, so ask for the first row only to learn it.
		_, total, err = tx.senderStatsPage(ctx, key, order, since, 1, 0, mine, wild, excl)
		if err != nil {
			return nil, 0, err
		}
	}
	for i := range stats {
		stats[i].Actionable = SenderKeyActionable(q.Group, stats[i].Key)
	}
	return stats, total, nil
}

// senderStatsPage runs the one ranking statement (see the file header).
//
// THE SHAPE: `mine` is the counted mail, read once (Postgres materialises a
// CTE referenced twice); `g` aggregates it by key; `page` ranks the groups and
// cuts the window, carrying the full group count; `addr` / `ranked` / `named`
// list up to maxGroupSenders addresses for the PAGE's keys only, with their
// contacts' markers — one more pass over `mine`, not one per row.
func (tx *UserTx) senderStatsPage(ctx context.Context, key, order string, since any, limit, pos int, mine, wild []string, excl senderExclusions) ([]SenderStat, int, error) {
	args := []any{tx.userID.String(), since, limit, pos, mine, wild}
	args = append(args, excl.args...)
	rows, err := tx.tx.Query(ctx, senderStatsSQL(key, order, excl.sql), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: sender stats: %w", err)
	}
	defer rows.Close()
	var (
		stats []SenderStat
		total int
	)
	for rows.Next() {
		var (
			s       SenderStat
			unsubID *string
			senders []byte
		)
		if err := rows.Scan(&s.Key, &s.Total, &s.Unread, &s.Neglected, &s.LastReceived, &s.AddressCount,
			&s.DisplayName, &unsubID, &total, &senders); err != nil {
			return nil, 0, fmt.Errorf("store: scan sender stats: %w", err)
		}
		if unsubID != nil {
			id, err := ulid.Parse(*unsubID)
			if err != nil {
				return nil, 0, fmt.Errorf("store: parse message id %q: %w", *unsubID, err)
			}
			s.UnsubscribeMessageID = &id
		}
		var raw []struct {
			A string  `json:"a"`
			C *string `json:"c"`
			N string  `json:"n"`
			V bool    `json:"v"`
			B bool    `json:"b"`
			O bool    `json:"o"`
		}
		if err := json.Unmarshal(senders, &raw); err != nil {
			return nil, 0, fmt.Errorf("store: decode group senders: %w", err)
		}
		for _, r := range raw {
			sa := SenderAddress{Address: r.A, Name: r.N, VIP: r.V, Blocked: r.B, Correspondent: r.O}
			if r.C != nil {
				id, err := ulid.Parse(*r.C)
				if err != nil {
					return nil, 0, fmt.Errorf("store: parse contact id %q: %w", *r.C, err)
				}
				sa.ContactID = &id
			}
			s.Addresses = append(s.Addresses, sa)
		}
		s.LastReceived = s.LastReceived.UTC()
		stats = append(stats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: sender stats: %w", err)
	}
	return stats, total, nil
}

// senderExclusions is the trash-and-junk predicate SenderStats shares with a
// search, compiled with its own binds starting at $7.
type senderExclusions struct {
	sql  string
	args []any
}

// senderStatsExclusions builds "NOT deleted AND NOT junk" exactly as
// Email/query does by default (jmap_email.go's trash exclusion, junkSQL).
func (tx *UserTx) senderStatsExclusions(ctx context.Context) (senderExclusions, error) {
	var e senderExclusions
	bind := func(v any) string {
		e.args = append(e.args, v)
		return fmt.Sprintf("$%d", 6+len(e.args))
	}
	conds := []string{"m.trashed_at IS NULL"}
	trash, err := tx.trashRoleMailboxIDs(ctx)
	if err != nil {
		return e, err
	}
	if len(trash) > 0 {
		conds = append(conds, "NOT "+inAnyMailboxSQL(bind, tx.userID, trash))
	}
	junk, err := tx.junkSQL(ctx, bind)
	if err != nil {
		return e, err
	}
	conds = append(conds, "NOT "+junk)
	e.sql = strings.Join(conds, " AND ")
	return e, nil
}

// myAddressesAndDomains returns the caller's own fixed identity addresses and
// wildcard identity domains, lower-cased — "me", as SenderStats excludes it.
func (tx *UserTx) myAddressesAndDomains(ctx context.Context) (addrs, domains []string, err error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT kind::text, lower(COALESCE(address, domain)::text) FROM app.identities
		 WHERE user_id = $1 AND COALESCE(address, domain) IS NOT NULL`, tx.userID.String())
	if err != nil {
		return nil, nil, fmt.Errorf("store: sender stats identities: %w", err)
	}
	defer rows.Close()
	addrs, domains = []string{}, []string{}
	for rows.Next() {
		var kind, v string
		if err := rows.Scan(&kind, &v); err != nil {
			return nil, nil, fmt.Errorf("store: scan identity: %w", err)
		}
		if kind == "domain_wildcard" {
			domains = append(domains, v)
		} else {
			addrs = append(addrs, v)
		}
	}
	return addrs, domains, rows.Err()
}

// senderStatsSQL is the ranking statement for one grouping key and order
// (both chosen from closed sets by SenderStats, never from input). Binds: $1
// the caller's user id, $2 the window start or NULL, $3 the page size, $4 the
// offset, $5 the user's own addresses and $6 their wildcard domains (both
// lower-cased; myAddressesAndDomains); exclusions binds from $7
// (senderStatsExclusions). A function so the measurement test can
// EXPLAIN exactly this text.
//
// MEASURED SHAPE (sender_stats_integration_test.go): `g` must stay a HASH
// aggregate. An ordered aggregate (array_agg ... ORDER BY) or a
// count(DISTINCT) there forces a sort of every counted message by key — it
// was 60% of the first version's time — so the newest display name rides a
// max() over a sortable timestamp prefix, the unsubscribe message is the
// newest INGESTED one carrying the header (max(id): ULIDs order by creation),
// and the per-key address count comes from the page-only pass.
func senderStatsSQL(key, order, exclusions string) string {
	return senderMineCTE(key, exclusions) + senderStatsTail(order)
}

// senderMineCTE is "the counted mail" — `WITH au AS (…), mine AS (…)` —
// shared VERBATIM by the ranking (senderStatsSQL) and the id enumeration
// (SenderMessageIDs), so the messages an action moves are exactly the ones its
// row counted: the same grouping key, window, exclusions and "not me". Binds
// as senderStatsSQL documents ($1, $2, $5, $6, exclusions from $7).
func senderMineCTE(key, exclusions string) string {
	return `
		WITH au AS (
			SELECT DISTINCT f.message_id FROM app.feedback_events f
			 WHERE f.user_id = $1 AND f.action = 'archive_unread' AND f.message_id IS NOT NULL
		), mine AS (
			SELECT m.id, m.received_at, m.is_seen, m.from_name,
			       (m.list_unsubscribe IS NOT NULL) AS unsub,
			       lower(m.from_address::text) AS addr,
			       ` + key + ` AS k,
			       (NOT m.is_seen OR au.message_id IS NOT NULL) AS neglected
			  FROM app.messages m
			  LEFT JOIN au ON au.message_id = m.id
			 WHERE m.user_id = $1
			   AND ` + exclusions + ` AND NOT m.is_draft
			   AND m.from_address IS NOT NULL
			   AND ($2::timestamptz IS NULL OR m.received_at >= $2::timestamptz)
			   AND NOT (lower(m.from_address::text) = ANY($5::text[]))
			   AND NOT (lower(split_part(m.from_address::text, '@', 2)) = ANY($6::text[]))
		)`
}

// senderStatsTail is the ranking after `mine`: the aggregate, the page and
// the page's addresses.
func senderStatsTail(order string) string {
	return `, g AS (
			SELECT k,
			       count(*) AS total,
			       count(*) FILTER (WHERE NOT is_seen) AS unread,
			       count(*) FILTER (WHERE neglected) AS neglected,
			       max(received_at) AS last,
			       max(id::text COLLATE "C") FILTER (WHERE unsub) AS unsub_id,
			       max((to_char(received_at AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS') || from_name) COLLATE "C")
			         FILTER (WHERE from_name <> '') AS stamped_name
			  FROM mine
			 WHERE k IS NOT NULL
			 GROUP BY k
		), page AS (
			SELECT g.*, count(*) OVER () AS groups, row_number() OVER (ORDER BY ` + order + `) AS rnk
			  FROM g
			 ORDER BY rnk
			 LIMIT $3 OFFSET $4
		), addr AS (
			SELECT k, addr AS a, max(received_at) AS last
			  FROM mine
			 WHERE k IN (SELECT k FROM page)
			 GROUP BY k, addr
		), cnt AS (
			SELECT k, count(*) AS addrs FROM addr GROUP BY k
		), ranked AS (
			SELECT addr.*, row_number() OVER (PARTITION BY k ORDER BY last DESC, a) AS rn FROM addr
		), named AS (
			SELECT r.k,
			       json_agg(json_build_object('a', r.a, 'c', c.id, 'n', COALESCE(c.display_name, ''),
			                                  'v', COALESCE(c.vip, false),
			                                  'b', COALESCE(c.blocked, false),
			                                  'o', COALESCE(ca.outbound_count, 0) > 0)
			                ORDER BY r.last DESC, r.a) AS senders
			  FROM ranked r
			  LEFT JOIN app.contact_addresses ca ON ca.user_id = $1 AND ca.address = r.a::citext
			  LEFT JOIN app.contacts c ON c.user_id = $1 AND c.id = ca.contact_id
			 WHERE r.rn <= ` + fmt.Sprint(maxGroupSenders) + `
			 GROUP BY r.k
		)
		SELECT p.k, p.total, p.unread, p.neglected, p.last, COALESCE(cnt.addrs, 0),
		       COALESCE(substr(p.stamped_name, 21), ''), p.unsub_id, p.groups,
		       COALESCE(n.senders, '[]'::json)
		  FROM page p
		  LEFT JOIN cnt ON cnt.k = p.k
		  LEFT JOIN named n ON n.k = p.k
		 ORDER BY p.rnk`
}
