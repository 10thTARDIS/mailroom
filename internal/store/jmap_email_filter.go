// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Email/query's boolean filters and its newer sort keys (RFC 8620 5.5, RFC 8621
// 4.4). The compiler for them is the SAME one every other Email/query predicate
// goes through (emailQueryWhere): a FilterOperator tree is one more conjunct in
// that WHERE, built from the same standardConds the flat filter uses.

// The bounds a FilterOperator tree must stay inside. Constants, shared by the
// API (which answers invalidArguments) and by the compiler below (which refuses
// rather than trusting its caller): a tree is client input that becomes SQL, and
// both its nesting and its size cost the server something.
const (
	// MaxFilterDepth is the deepest nesting of FilterOperators, counting the root
	// as depth 1.
	MaxFilterDepth = 8
	// MaxFilterNodes is the most nodes (operators and conditions together) one
	// filter may hold.
	MaxFilterNodes = 64
	// MaxThreadKeywordConds is the most thread-keyword conditions (some, none and
	// allInThreadHaveKeyword, all together) one query may carry: each is a
	// per-message subquery over the thread's messages.
	MaxThreadKeywordConds = 4
	// MaxMailboxIDs is the longest inMailbox / inMailboxOtherThan id list one
	// condition may carry.
	MaxMailboxIDs = 256
)

// ConditionCount is how many standard conditions this query carries, which is
// what a FilterOperator leaf costs toward MaxFilterNodes (a leaf with three
// conditions is three, not one). It reads the SAME fields standardConds
// compiles, and the API counts with it too, so the two bounds are identical.
func (q EmailQuery) ConditionCount() int {
	n := 0
	for _, set := range []bool{
		q.InThread != nil, q.InMailbox != nil, q.InMailboxOtherThan != nil, q.InAccount != nil,
		q.HasKeyword != "", q.NotKeyword != "", q.After != nil, q.Before != nil,
		q.FromContains != "", q.SubjectContains != "", q.HasAttachment != nil,
		q.MinSize != nil, q.MaxSize != nil,
		q.SomeInThreadHaveKeyword != "", q.NoneInThreadHaveKeyword != "", q.AllInThreadHaveKeyword != "",
	} {
		if set {
			n++
		}
	}
	return n
}

// ThreadKeywordCount is how many thread-keyword conditions this query carries.
func (q EmailQuery) ThreadKeywordCount() int {
	n := 0
	for _, kw := range []string{q.SomeInThreadHaveKeyword, q.NoneInThreadHaveKeyword, q.AllInThreadHaveKeyword} {
		if kw != "" {
			n++
		}
	}
	return n
}

// MailboxListTooLong reports whether a mailbox id list on this query is past
// MaxMailboxIDs.
func (q EmailQuery) MailboxListTooLong() bool {
	return len(q.InMailboxOtherThan) > MaxMailboxIDs
}

// ThreadKeywordCount is the thread-keyword conditions in the whole tree.
func (n EmailFilterNode) ThreadKeywordCount() int {
	c := n.Leaf.ThreadKeywordCount()
	for _, k := range n.Conditions {
		c += k.ThreadKeywordCount()
	}
	return c
}

// ErrUnsupportedTrashFilter is returned for a Trash-role inMailbox in a FilterOperator
// tree anywhere but a leaf reached from the root through AND alone. The API
// answers unsupportedFilter.
var ErrUnsupportedTrashFilter = errors.New("store: a Trash mailbox condition is supported only in a conjunction")

// ErrFilterTooComplex is returned for a filter tree beyond the bounds above.
var ErrFilterTooComplex = errors.New("store: filter tree exceeds the depth or size bound")

// Filter operators.
const (
	FilterAND = "AND"
	FilterOR  = "OR"
	FilterNOT = "NOT"
)

// EmailFilterNode is one node of a FilterOperator tree. An operator node has an
// Operator and Conditions; a leaf has an empty Operator and carries its
// conditions in Leaf — only the STANDARD fields of EmailQuery are read from it
// (see standardConds), which is what stops a leaf smuggling in anything that
// must not sit under an OR or NOT.
type EmailFilterNode struct {
	Operator   string
	Conditions []EmailFilterNode
	Leaf       EmailQuery
}

// mailboxLeaves lists the inMailbox ids of this tree's leaves, split by where
// they sit: conjunctive are reached from the root through AND operators alone
// (so a matching message MUST be in them); other are under an OR or a NOT. The
// first decide whether the query is a view of Trash (which lifts the default
// "deleted mail is gone"); a Trash mailbox in the second is refused, because
// lifting the exclusion for the whole query would widen every other branch.
func (n EmailFilterNode) mailboxLeaves() (conjunctive, other []ulid.ULID) {
	// conj: reached from the root through AND alone. positive: under an even
	// number of NOTs. Only a POSITIVE leaf can ask for Trash's mail; a negative
	// one ("not in Trash") never makes the query a Trash view, and the default
	// exclusion of deleted mail already agrees with it, so it is neither
	// conjunctive nor refused. A positive leaf reached through an OR is the
	// case with no correct global answer (other).
	var walk func(n EmailFilterNode, conj, positive bool)
	walk = func(n EmailFilterNode, conj, positive bool) {
		if n.Operator == "" {
			if n.Leaf.InMailbox == nil || !positive {
				return
			}
			if conj {
				conjunctive = append(conjunctive, *n.Leaf.InMailbox)
			} else {
				other = append(other, *n.Leaf.InMailbox)
			}
			return
		}
		childPositive := positive
		if n.Operator == FilterNOT {
			childPositive = !positive
		}
		for _, c := range n.Conditions {
			walk(c, conj && n.Operator == FilterAND, childPositive)
		}
	}
	walk(n, true, true)
	return conjunctive, other
}

// filterTreeSQL compiles a FilterOperator tree to ONE parenthesised boolean
// expression over the message alias `m`.
//
// Three-valued logic is closed off at every leaf: a leaf is COALESCE'd to a
// strict true/false, so `NOT (leaf)` is a true complement. Without it a message
// whose thread_id (or any nullable column a condition reads) is NULL would be
// NULL under the leaf and NULL under its NOT, and so appear in neither.
//
// RFC 8620 5.5: AND = all conditions match, OR = at least one, NOT = none of
// them (that is NOT (c1 OR c2 …)). Empty AND is true, empty OR is false.
func (tx *UserTx) filterTreeSQL(ctx context.Context, root EmailFilterNode, restrict []ulid.ULID, bind func(any) string) (string, error) {
	nodes := 0
	var walk func(n EmailFilterNode, depth int) (string, error)
	walk = func(n EmailFilterNode, depth int) (string, error) {
		// A leaf costs one node per condition it carries (at least one).
		if n.Operator == "" {
			nodes += max(1, n.Leaf.ConditionCount())
		} else {
			nodes++
		}
		if depth > MaxFilterDepth || nodes > MaxFilterNodes {
			return "", ErrFilterTooComplex
		}
		if n.Operator == "" {
			conds, err := tx.standardConds(ctx, n.Leaf, restrict, bind)
			if err != nil {
				return "", err
			}
			if len(conds) == 0 {
				return "TRUE", nil
			}
			return "COALESCE((" + strings.Join(conds, " AND ") + "), false)", nil
		}
		parts := make([]string, 0, len(n.Conditions))
		for _, c := range n.Conditions {
			sql, err := walk(c, depth+1)
			if err != nil {
				return "", err
			}
			parts = append(parts, sql)
		}
		switch n.Operator {
		case FilterAND:
			if len(parts) == 0 {
				return "TRUE", nil
			}
			return "(" + strings.Join(parts, " AND ") + ")", nil
		case FilterOR:
			if len(parts) == 0 {
				return "FALSE", nil
			}
			return "(" + strings.Join(parts, " OR ") + ")", nil
		case FilterNOT:
			if len(parts) == 0 {
				return "TRUE", nil
			}
			return "(NOT (" + strings.Join(parts, " OR ") + "))", nil
		}
		return "", fmt.Errorf("store: unknown filter operator %q", n.Operator)
	}
	return walk(root, 1)
}

// likeContains is a case-insensitive CONTAINS pattern for ILIKE: the value with
// LIKE's own metacharacters escaped, wrapped in %.
func likeContains(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(v) + "%"
}

// standardConds compiles the STANDARD (RFC 8621 4.4.1) conditions of q: those a
// FilterOperator leaf may carry, and the ones the query's own flat filter carries
// too. It is the only place they are written, so a condition cannot mean one
// thing at the top of a filter and another inside an OR.
//
// It never reads RestrictAccounts, the trash/junk defaults, the search
// candidates or the Mailroom extension conditions: those are the query's POLICY
// and live in emailQueryWhere, outside any tree.
func (tx *UserTx) standardConds(ctx context.Context, q EmailQuery, restrict []ulid.ULID, bind func(any) string) ([]string, error) {
	var conds []string
	if q.MailboxListTooLong() {
		return nil, ErrFilterTooComplex
	}
	if q.InThread != nil {
		conds = append(conds, "m.thread_id = "+bind(q.InThread.String()))
	}
	if q.InMailbox != nil {
		conds = append(conds, inMailboxSQL(bind, tx.userID, *q.InMailbox))
	}
	if q.InMailboxOtherThan != nil {
		conds = append(conds, inOtherThanSQL(bind, tx.userID, q.InMailboxOtherThan))
	}
	if q.InAccount != nil {
		// "Lives in the account" is EXACTLY liveAccountIDs' definition — a LIVE
		// provenance row for this message in that account — so an index-independent
		// per-account view (spec §7) matches the writeback fan-out set. It reads
		// provenance directly and deliberately ignores moved_mailbox_id: a message
		// the user moved still physically belongs to the account whose folder holds
		// the bytes, which is the account the mutation writes back to.
		//
		// RLS-safe by construction: app.message_provenance is RLS-scoped to the
		// user (and we bind p.user_id = m.user_id = tx.userID), so a foreign or
		// other-user account id matches ZERO rows — fail-closed, with no separate
		// ownership check that could error (500) on a valid request.
		a := bind(q.InAccount.String())
		conds = append(conds, `EXISTS (SELECT 1 FROM app.message_provenance p
			 WHERE p.user_id = m.user_id AND p.message_id = m.id
			   AND p.state = 'live' AND p.account_id = `+a+`)`)
	}
	if q.HasKeyword != "" {
		cond, err := tx.keywordPredicate(ctx, "m", q.HasKeyword, true, bind)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cond)
	}
	if q.NotKeyword != "" {
		cond, err := tx.keywordPredicate(ctx, "m", q.NotKeyword, false, bind)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cond)
	}
	if q.After != nil {
		conds = append(conds, "m.received_at >= "+bind(q.After.UTC()))
	}
	if q.Before != nil {
		conds = append(conds, "m.received_at < "+bind(q.Before.UTC()))
	}
	if q.FromContains != "" {
		// The stored display name and address. from_address is nullable, so it is
		// COALESCE'd: a NULL here would make the leaf NULL, and NOT of it NULL.
		p := bind(likeContains(q.FromContains))
		conds = append(conds, "(m.from_name ILIKE "+p+" OR COALESCE(m.from_address::text, '') ILIKE "+p+")")
	}
	if q.SubjectContains != "" {
		conds = append(conds, "m.subject ILIKE "+bind(likeContains(q.SubjectContains)))
	}
	if q.HasAttachment != nil {
		if *q.HasAttachment {
			conds = append(conds, "m.has_attachments")
		} else {
			conds = append(conds, "NOT m.has_attachments")
		}
	}
	if q.MinSize != nil {
		conds = append(conds, "m.size_bytes >= "+bind(*q.MinSize))
	}
	if q.MaxSize != nil {
		conds = append(conds, "m.size_bytes < "+bind(*q.MaxSize))
	}
	if q.SomeInThreadHaveKeyword != "" {
		c, err := tx.threadKeywordSQL(ctx, q.SomeInThreadHaveKeyword, true, restrict, bind)
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
	}
	if q.NoneInThreadHaveKeyword != "" {
		c, err := tx.threadKeywordSQL(ctx, q.NoneInThreadHaveKeyword, true, restrict, bind)
		if err != nil {
			return nil, err
		}
		conds = append(conds, "NOT "+c)
	}
	if q.AllInThreadHaveKeyword != "" {
		c, err := tx.threadKeywordSQL(ctx, q.AllInThreadHaveKeyword, false, restrict, bind)
		if err != nil {
			return nil, err
		}
		conds = append(conds, "NOT "+c)
	}
	return conds, nil
}

// threadKeywordSQL is "some message of m's thread has (want=true) / lacks
// (want=false) the keyword". The three RFC conditions are this one predicate:
// some = it, none = NOT it, all = NOT (some lacks it).
//
// The members of a thread are its undeleted messages plus m itself (so a Trash
// view of a deleted message still counts the message). A message with no thread
// is a thread of one: the CASE reads m alone, because `tm.thread_id = NULL` is
// never true. Strictly boolean, so NOT is a true complement.
func (tx *UserTx) threadKeywordSQL(ctx context.Context, keyword string, want bool, restrict []ulid.ULID, bind func(any) string) (string, error) {
	own, err := tx.keywordPredicate(ctx, "m", keyword, want, bind)
	if err != nil {
		return "", err
	}
	member, err := tx.keywordPredicate(ctx, "tm", keyword, want, bind)
	if err != nil {
		return "", err
	}
	// The thread's OTHER messages count only if not deleted by the one definition
	// Email/query's default uses (#631): trashed_at set, or effectively in a
	// trash-role mailbox (mailbox moved by another client included).
	folderTrashed := ""
	if trash, err := tx.trashRoleMailboxIDs(ctx); err != nil {
		return "", err
	} else if len(trash) > 0 {
		folderTrashed = " AND NOT " + mailboxMembershipFor("tm", bind(tx.userID.String()), "= ANY("+bind(ulidStrings(trash))+")")
	}
	// restrict is the API key's account restriction (EmailQuery.RestrictAccounts),
	// passed down by emailQueryWhere and NOT read from the leaf, because a tree
	// leaf compiles through here too. A thread can span accounts; "in the thread"
	// for a key means among the thread's messages the key can see, or the
	// keyword of a message in an account it cannot see would be disclosed by the
	// result and its total.
	vis := ""
	if restrict != nil {
		vis = " AND (tm.id = m.id OR " + keyAccountsSQL("tm", bind(ulidStrings(restrict))) + ")"
	}
	return `(CASE WHEN m.thread_id IS NULL THEN ` + own + `
		ELSE EXISTS (SELECT 1 FROM app.messages tm
		   WHERE tm.user_id = m.user_id AND tm.thread_id = m.thread_id
		     AND (tm.id = m.id OR (tm.trashed_at IS NULL` + folderTrashed + `))` + vis + ` AND ` + member + `) END)`, nil
}

// EmailSortKind names an Email/query sort key beyond the historical date ones.
type EmailSortKind string

const (
	SortKeyDate       EmailSortKind = "date" // receivedAt / sentAt / returnedAt, in its place
	SortKeySize       EmailSortKind = "size"
	SortKeyFrom       EmailSortKind = "from"
	SortKeySubject    EmailSortKind = "subject"
	SortKeyHasKeyword EmailSortKind = "hasKeyword"
)

// EmailSortKey is one comparator of an ordered Email/query sort.
type EmailSortKey struct {
	Kind      EmailSortKind
	Keyword   string // hasKeyword only
	Ascending bool
	// Octet selects the i;octet collation (bytes as stored) instead of the
	// default i;ascii-casemap, for the text keys.
	Octet bool
}

// Sort expressions, written once, here. NO index covers them: the query's
// count(*) OVER () reads every row of the user's result anyway, so a sort by
// subject or sender costs a pass over the user's matching messages plus a sort,
// linear in their mail rather than in the page size. The subject functions cap
// their input at 1000 characters (migration 00089), so the per-row work is
// bounded however long a sender-controlled subject is.
const (
	// sortFromExpr: the first From address's display name, else its address,
	// lowercased (RFC 8621 4.4.2). COLLATE "C" so the order is the octet order of
	// the folded value (i;ascii-casemap), independent of the database locale.
	sortFromExpr = `(lower(COALESCE(NULLIF(m.from_name, ''), m.from_address::text, '')) COLLATE "C")`
	// sortFromOctetExpr: the same, not folded (i;octet).
	sortFromOctetExpr = `(COALESCE(NULLIF(m.from_name, ''), m.from_address::text, '') COLLATE "C")`
	// sortSubjectExpr: the base subject (RFC 5256 2.1: reply and forward markers
	// stripped), lowercased. See app.base_subject.
	sortSubjectExpr      = `(app.base_subject(m.subject) COLLATE "C")`
	sortSubjectOctetExpr = `(app.base_subject_raw(m.subject) COLLATE "C")`
)

// sortKeyTerms renders q.SortKeys as ORDER BY terms, resolving what needs the
// database (a keyword atom) first. defaultDir is the direction of the date key
// and the id when the sort names none of its own. dateTerms is the date key's
// expression.
func (tx *UserTx) sortKeyTerms(ctx context.Context, q EmailQuery, bind func(any) string) ([]orderTerm, error) {
	if len(q.SortKeys) == 0 {
		return nil, nil
	}
	dirOf := func(asc bool) string {
		if asc {
			return "ASC"
		}
		return "DESC"
	}
	var out []orderTerm
	for _, k := range q.SortKeys {
		switch k.Kind {
		case SortKeyDate:
			// Placed by emailOrderTerms (it owns the date expression); a marker term.
			out = append(out, orderTerm{expr: "", dir: dirOf(k.Ascending)})
		case SortKeySize:
			out = append(out, orderTerm{expr: "m.size_bytes", dir: dirOf(k.Ascending)})
		case SortKeyFrom:
			e := sortFromExpr
			if k.Octet {
				e = sortFromOctetExpr
			}
			out = append(out, orderTerm{expr: e, dir: dirOf(k.Ascending)})
		case SortKeySubject:
			e := sortSubjectExpr
			if k.Octet {
				e = sortSubjectOctetExpr
			}
			out = append(out, orderTerm{expr: e, dir: dirOf(k.Ascending)})
		case SortKeyHasKeyword:
			pred, err := tx.keywordPredicate(ctx, "m", k.Keyword, true, bind)
			if err != nil {
				return nil, err
			}
			// A boolean key: false before true when ascending, so mail that HAS
			// the keyword sorts first when descending. RFC 8621 4.4.2 does not
			// spell the order out, but its own example sorts flagged mail first
			// with "isAscending": false, which only this reading satisfies.
			out = append(out, orderTerm{expr: "(CASE WHEN " + pred + " THEN 1 ELSE 0 END)", dir: dirOf(k.Ascending)})
		default:
			return nil, fmt.Errorf("store: unknown sort key %q", k.Kind)
		}
	}
	return out, nil
}

// EmailAnchorIndex is the 0-based index of anchor in the result q would return
// (RFC 8620 5.5), or found=false when the anchor is not in it. q's Position and
// Limit are ignored.
//
// It is answered by Postgres, in one statement over the SAME predicate and the
// SAME ordering QueryEmails uses (emailQueryWhere, emailOrderTermsWith), so the
// index it reports is exactly the position QueryEmails would give that row.
// Nothing is paged into memory: the database numbers the matching rows and
// returns the single row asked for. With collapseThreads the numbering is over
// each thread's first message only, so an anchor that is not its thread's
// representative is not in the list and is reported as not found.
func (tx *UserTx) EmailAnchorIndex(ctx context.Context, q EmailQuery, anchor ulid.ULID) (idx int, found bool, err error) {
	args := []any{tx.userID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + fmt.Sprint(len(args))
	}
	from, where, err := tx.emailQueryWhere(ctx, q, bind, nil)
	if err != nil {
		return 0, false, err
	}
	keyTerms, err := tx.sortKeyTerms(ctx, q, bind)
	if err != nil {
		return 0, false, err
	}
	terms := emailOrderTermsWith(q, bind, keyTerms)
	a := bind(anchor.String())
	var sql string
	if q.CollapseThreads {
		body, outerOrder := collapsedRankSQL(from, where, terms)
		sql = `WITH ranked AS (` + body + `)
			SELECT pos FROM (SELECT id, row_number() OVER (ORDER BY ` + outerOrder + `) - 1 AS pos
			                   FROM ranked WHERE rn = 1) t WHERE t.id = ` + a
	} else {
		parts := make([]string, len(terms))
		for i, t := range terms {
			parts[i] = t.render(t.expr)
		}
		sql = `SELECT pos FROM (SELECT m.id AS id, row_number() OVER (ORDER BY ` + strings.Join(parts, ", ") + `) - 1 AS pos
		                         FROM ` + from + ` WHERE ` + where + `) t WHERE t.id = ` + a
	}
	if err := tx.tx.QueryRow(ctx, sql, args...).Scan(&idx); err != nil {
		if isNoRows(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("store: email anchor index: %w", err)
	}
	return idx, true, nil
}
