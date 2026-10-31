// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"errors"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Mailroom's OWN query AST (spec §7: "Mailroom owns its query AST — Tantivy
// query syntax never leaks into the API or clients — this is what keeps the
// backend genuinely swappable").
//
// A query is a tree of And/Or/Not over typed Terms, plus DateRange and the two
// degenerate leaves All (matches everything) and None (matches nothing). Every
// backend compiles this tree into its own engine query; nothing else does. The
// tree is a pure value: it can be built by the parser, by the JMAP filter
// translator, or by a test, and it can be compared and printed.

// Field names an index field. The values double as the physical field names in
// the backends, so this list IS the index schema's vocabulary.
type Field string

const (
	// FieldAny is virtual: it expands to the analyzed text fields (TextFields)
	// as an OR. Free text a user types with no operator lands here.
	FieldAny Field = "any"

	// Analyzed (tokenized, stemmed, positional) text fields.
	FieldSubject Field = "subject"
	FieldBody    Field = "body"
	FieldPeople  Field = "people" // display names + addresses of every participant
	FieldFuzzy   Field = "fuzzy"  // n-gram field backing typo tolerance

	// Exact (unanalyzed) fields. Values are matched byte-for-byte after
	// normalizeExact, so index-time and query-time normalization must agree.
	FieldFrom Field = "from"
	FieldTo   Field = "to"

	// FieldFromDomain / FieldToDomain are the DOMAIN views of FieldFrom /
	// FieldTo (#712): for each address, its domain plus every parent-domain
	// suffix that still has two labels (mail.shop.example → mail.shop.example,
	// shop.example; never the bare TLD). An exact term on them is "from this
	// domain or any subdomain" — index-backed, with no wildcard scan. See
	// addressDomains.
	FieldFromDomain Field = "from_domain"
	FieldToDomain   Field = "to_domain"

	FieldLabel   Field = "label"   // label ULID (not the name — see Resolver)
	FieldMailbox Field = "mailbox" // mailbox ULID
	FieldAccount Field = "account" // account ULID
	FieldThread  Field = "thread"  // thread ULID
	FieldFlag    Field = "flag"    // is:/has: atom
	FieldKeyword Field = "keyword" // raw JMAP keyword
	FieldDate    Field = "date"    // calendar bucket token (see daterange.go)
	FieldID      Field = "id"      // message ULID — the index's primary key

	// Schema v3 (#722/#723). See FieldSchemaVersion.
	FieldList     Field = "list"     // List-Id and its parent-domain suffixes (listIDValues)
	FieldSize     Field = "size"     // size-prefix token (see sizerange.go)
	FieldFilename Field = "filename" // ANALYZED attachment file names (name words and extension)

	// FieldDeliveredTo / FieldDeliveredToDomain are the address the message was
	// delivered to (app.messages.addressed_address — the alias it reached the
	// user at) and its domain view, exactly as FieldFrom / FieldFromDomain are
	// for the sender (#723).
	FieldDeliveredTo       Field = "deliveredto"
	FieldDeliveredToDomain Field = "deliveredto_domain"
)

// TextFields is the expansion of FieldAny: the analyzed fields free text is
// searched across, in descending order of how much a match in them means. Both
// backends use this list, so "invoice" means the same thing everywhere.
func TextFields() []Field { return []Field{FieldSubject, FieldPeople, FieldBody} }

// textFieldBoost is the relevance weight of a free-text match per field: a hit
// in the subject or on a participant is worth more than a hit buried in a body.
func textFieldBoost(f Field) float64 {
	switch f {
	case FieldSubject:
		return 3
	case FieldPeople:
		return 2
	default:
		return 1
	}
}

// fuzzyBoost keeps typo-tolerant (n-gram) matches strictly below exact matches,
// so a correctly spelled hit always outranks an approximate one.
const fuzzyBoost = 0.25

// IsExact reports whether f is an unanalyzed field whose values are matched
// byte-for-byte.
func (f Field) IsExact() bool {
	switch f {
	case FieldFrom, FieldTo, FieldFromDomain, FieldToDomain, FieldLabel, FieldMailbox, FieldAccount,
		FieldThread, FieldFlag, FieldKeyword, FieldDate, FieldID,
		FieldList, FieldSize, FieldDeliveredTo, FieldDeliveredToDomain:
		return true
	}
	return false
}

// isCaseSensitiveField reports whether an exact field's values must NOT be
// lowercased: ids are canonical uppercase Crockford base32 (ADR-007), and JMAP
// keywords are case-sensitive tokens (RFC 8621 §4.1.1). Everything else exact —
// addresses, flag atoms, date buckets — is lowercased at both ends.
func isCaseSensitiveField(f Field) bool {
	switch f {
	case FieldLabel, FieldMailbox, FieldAccount, FieldThread, FieldID, FieldKeyword:
		return true
	}
	return false
}

// ErrIndexUpgrading is returned by a backend that is still serving an index
// written under an OLDER schema (it is being rebuilt in the background, see
// Indexer.Reindex) for a query it cannot answer yet — one that needs a field the
// old index does not have. It is an error rather than an empty result on
// purpose: "nothing matched" and "this index predates the field" must not look
// alike (#712).
var ErrIndexUpgrading = errors.New("search: the search index is being upgraded; this kind of search will work when it finishes")

// UsesField reports whether any Term in the tree searches field f.
func UsesField(n Node, f Field) bool {
	switch v := n.(type) {
	case And:
		for _, c := range v.Nodes {
			if UsesField(c, f) {
				return true
			}
		}
	case Or:
		for _, c := range v.Nodes {
			if UsesField(c, f) {
				return true
			}
		}
	case Not:
		return UsesField(v.Node, f)
	case Term:
		return v.Field == f
	}
	return false
}

// FieldSchemaVersion is the index schema version that introduced field f. A
// backend still serving an older index has no values in it (or, for Tantivy, no
// such field at all), so a query that needs it must be refused with
// ErrIndexUpgrading rather than answered as if nothing matched.
func FieldSchemaVersion(f Field) int {
	switch f {
	case FieldFromDomain, FieldToDomain:
		return 2 // #712
	case FieldList, FieldSize, FieldFilename, FieldDeliveredTo, FieldDeliveredToDomain:
		return 3 // #722/#723
	}
	return 1
}

// RequiredSchemaVersion is the oldest index schema that can answer the
// (already Expanded) tree correctly: the newest field any Term uses, and for
// FieldFlag the version that started writing that atom (flagSchemaVersion). An
// old index carries the flag FIELD but not the new atoms, so "has:pdf" on it
// would match nothing rather than fail — the silent failure #712 made this an
// error to avoid.
func RequiredSchemaVersion(n Node) int {
	switch v := n.(type) {
	case And:
		return maxVersion(v.Nodes)
	case Or:
		return maxVersion(v.Nodes)
	case Not:
		return RequiredSchemaVersion(v.Node)
	case Term:
		if v.Field == FieldFlag {
			return flagSchemaVersion(ExactValue(FieldFlag, v.Value))
		}
		return FieldSchemaVersion(v.Field)
	}
	return 1
}

func maxVersion(ns []Node) int {
	out := 1
	for _, c := range ns {
		if r := RequiredSchemaVersion(c); r > out {
			out = r
		}
	}
	return out
}

// Match is how a term's value is compared against a field.
type Match int

const (
	// MatchToken is a single analyzed term: the engine tokenizes and stems the
	// value with the field's analyzer, so case and morphology don't matter.
	MatchToken Match = iota
	// MatchPhrase is an ordered sequence of analyzed terms (positions).
	MatchPhrase
	// MatchPrefix matches any token starting with the value (as-you-type).
	MatchPrefix
	// MatchFuzzy is typo-tolerant matching over FieldFuzzy.
	MatchFuzzy
	// MatchExact matches a raw, unanalyzed field value byte-for-byte.
	MatchExact
)

// Node is one node of the query tree.
type Node interface{ node() }

// And matches documents matching every child. An empty And matches everything.
type And struct{ Nodes []Node }

// Or matches documents matching at least one child, scoring by the best/most
// children matched. An empty Or matches nothing.
type Or struct{ Nodes []Node }

// Not matches documents NOT matching the child.
type Not struct{ Node Node }

// Term is a leaf: compare Value against Field using Match, with Boost applied to
// the score (1 = neutral).
type Term struct {
	Field Field
	Value string
	Match Match
	Boost float64
}

// DateRange matches documents whose received_at falls in [From, To). A zero From
// is open-ended in the past; a zero To is open-ended in the future. It is a node
// of its own (not a Term) because a backend without numeric fields must expand
// it into calendar-bucket terms — see daterange.go.
type DateRange struct{ From, To time.Time }

// All matches every document. It is what a filter-only query ("is:unread" with
// no text) needs as its positive clause.
type All struct{}

// None matches no document. An unresolvable name (`label:DoesNotExist`) compiles
// to None so the query returns nothing — rather than silently dropping the
// clause and returning everything, which is the dangerous failure.
type None struct{}

// SizeRange matches documents whose size in bytes falls in [Min, Max). A zero
// Max is open-ended. Like DateRange it is a node of its own because the engine
// has no numeric fields: Expand turns it into size-prefix tokens (sizerange.go).
type SizeRange struct{ Min, Max int64 }

// StoreTerm is a predicate the INDEX CANNOT ANSWER: it is about state that
// lives only in Postgres and changes with no Email change_log row behind it (an
// area is derived from labels and feeds; a mute is a Thread row; a snooze ends
// with the clock; the line moves with DeskPreferences). Indexing any of them
// would go stale silently, so they are evaluated by the store (store.
// SearchPredicate), never by a backend — see split.go for how a query that mixes
// them with index terms is composed, and Service.Run, which refuses them.
type StoreTerm struct {
	Kind  StoreKind
	Value string // the area NAME for StoreArea; unused otherwise
}

// StoreKind names a StoreTerm predicate.
type StoreKind string

// The store-side predicates (#722 area:, #723 is:snoozed/muted/pinned/important).
const (
	StoreArea      StoreKind = "area"       // resolves to this area (by name)
	StoreSnoozed   StoreKind = "snoozed"    // snoozed right now (the clock decides)
	StoreMuted     StoreKind = "muted"      // its thread is muted
	StorePinned    StoreKind = "pinned"     // pinned
	StoreAboveLine StoreKind = "above_line" // above the user's line on the desk
	// StoreVIP is "from a VIP" (#59). The VIP marker lives on the CONTACT and
	// changes without any Email change_log row, so an indexed copy would go
	// stale the moment the user marks someone — the reason every StoreTerm is
	// a StoreTerm.
	StoreVIP StoreKind = "vip"
)

// IndexSet is a subtree the index evaluates ON ITS OWN, as a set of message ids,
// because it sits under an Or or a Not together with a StoreTerm (split.go). It
// never reaches a backend as such: the caller runs Node and substitutes IDSet.
type IndexSet struct{ Node Node }

// IDSet is a resolved IndexSet: the ids the index returned for it. Capped is
// true when the index hit its hit limit, so the set may be incomplete — a
// bound the caller must report rather than present as a total.
type IDSet struct {
	IDs    []ulid.ULID
	Capped bool
}

func (And) node()       {}
func (Or) node()        {}
func (Not) node()       {}
func (Term) node()      {}
func (DateRange) node() {}
func (All) node()       {}
func (None) node()      {}
func (SizeRange) node() {}
func (StoreTerm) node() {}
func (IndexSet) node()  {}
func (IDSet) node()     {}

// Query is a complete search request against one user's index.
type Query struct {
	// Root is the query tree. A nil Root matches everything.
	Root Node

	// Fuzzy enables typo tolerance for free text (FieldAny terms): each expands
	// to include a low-boost n-gram clause, so a misspelled word still finds the
	// message while correctly spelled hits still rank above it. Field-scoped
	// terms (from:, subject:, …) are never fuzzed — a user who typed an operator
	// meant it.
	Fuzzy bool

	// Limit bounds the hits returned. Zero means DefaultLimit.
	Limit int

	// Highlight requests snippets on each hit (SearchSnippet/get). It costs the
	// engine real work, so it is off unless asked for.
	Highlight bool

	// HighlightFields restricts highlighting to these fields; empty means the
	// analyzed text fields.
	HighlightFields []Field

	// Anywhere is `in:anywhere` (#723): the query asked not to have deleted and
	// junk mail excluded. It is a scope, not a predicate, so it lives here
	// rather than in the tree; the caller that applies the default exclusions
	// reads it.
	Anywhere bool
}

// DefaultLimit is the hit cap when a Query does not set one. It is deliberately
// generous: Email/query hands these ids to Postgres, which applies the remaining
// structured filters, the sort, and the window — so this is the size of the
// candidate set, not the size of a page.
const DefaultLimit = 500

// MaxLimit is the hard ceiling on hits a single search may materialize.
const MaxLimit = 5000

// Expand lowers a Query's tree to the primitives every backend implements
// (And/Or/Not/Term/All/None): FieldAny terms become the boosted OR across the
// text fields, DateRanges become calendar-bucket terms. A backend calls it once,
// at the top of Search — which is what makes two backends mean the same thing by
// the same query.
func Expand(q Query) Node { return expand(q.Root, q.Fuzzy) }

// ExactValue normalizes v for an exact (unanalyzed) field exactly as the index
// normalized it when the document was written. Backends MUST route every exact
// term value through it; it is the whole reason `from:Alice@Example.COM` finds
// mail from alice@example.com in an engine that does not analyze raw fields.
func ExactValue(f Field, v string) string {
	if isCaseSensitiveField(f) {
		return strings.TrimSpace(v)
	}
	return normalizeExact(v)
}

// EffectiveLimit is the clamped hit cap for this query.
func (q Query) EffectiveLimit() int { return q.limit() }

// limit returns the effective, clamped hit cap.
func (q Query) limit() int {
	switch {
	case q.Limit <= 0:
		return DefaultLimit
	case q.Limit > MaxLimit:
		return MaxLimit
	default:
		return q.Limit
	}
}

// --- Builders ---------------------------------------------------------------
// Small constructors so the parser, the JMAP filter translator, and tests all
// build the same shapes.

// TermNode builds a neutral-boost term.
func TermNode(f Field, value string, m Match) Term {
	return Term{Field: f, Value: value, Match: m, Boost: 1}
}

// ExactTerm builds an exact-match term, normalizing the value exactly as the
// index normalized it (the symmetry normalizeExact exists for).
func ExactTerm(f Field, value string) Term {
	if !isCaseSensitiveField(f) {
		value = normalizeExact(value)
	} else {
		value = strings.TrimSpace(value)
	}
	return Term{Field: f, Value: value, Match: MatchExact, Boost: 1}
}

// IDTerm builds an exact term over a ULID-valued field.
func IDTerm(f Field, id ulid.ULID) Term {
	return Term{Field: f, Value: id.String(), Match: MatchExact, Boost: 1}
}

// TextNode expands free text over FieldAny into the OR across the analyzed text
// fields (boosted per field), optionally with a typo-tolerant n-gram clause. It
// is the one place FieldAny is given meaning, so every backend agrees.
//
// The n-gram clause is an extra OR branch at a low boost: an exactly spelled
// query still ranks exact matches first, but a misspelled one still finds the
// document instead of returning nothing.
//
// It is added ONLY for whole-word matches. Not for a phrase, and not for a
// prefix — both are EXPLICIT, precise requests, and n-grams are inherently
// order- and boundary-insensitive. Fuzzing a phrase would make
// `"revenue quarterly"` match "quarterly revenue", quietly destroying the one
// property the user reached for the quotes to get. (This is not hypothetical:
// the Tantivy backend did exactly that until this guard existed.)
func TextNode(value string, m Match, fuzzy bool) Node {
	branches := make([]Node, 0, len(TextFields())+1)
	for _, f := range TextFields() {
		branches = append(branches, Term{Field: f, Value: value, Match: m, Boost: textFieldBoost(f)})
	}
	if fuzzy && m == MatchToken {
		branches = append(branches, Term{Field: FieldFuzzy, Value: value, Match: MatchFuzzy, Boost: fuzzyBoost})
	}
	if len(branches) == 1 {
		return branches[0]
	}
	return Or{Nodes: branches}
}

// expand lowers a query tree to the primitives every backend must implement:
// FieldAny terms become the boosted OR across the text fields (TextNode), and
// DateRanges become the OR over calendar-bucket terms (dateRangeNode). What is
// left is And/Or/Not/Term/All/None — nothing else. Backends call it once before
// compiling, which is what keeps two backends semantically identical.
func expand(n Node, fuzzy bool) Node {
	switch v := n.(type) {
	case nil:
		return All{}
	case And:
		return And{Nodes: expandAll(v.Nodes, fuzzy)}
	case Or:
		return Or{Nodes: expandAll(v.Nodes, fuzzy)}
	case Not:
		return Not{Node: expand(v.Node, fuzzy)}
	case DateRange:
		return dateRangeNode(v)
	case SizeRange:
		return sizeRangeNode(v)
	case Term:
		if v.Field == FieldAny {
			return TextNode(v.Value, v.Match, fuzzy)
		}
		return v
	default:
		return n
	}
}

func expandAll(ns []Node, fuzzy bool) []Node {
	out := make([]Node, 0, len(ns))
	for _, n := range ns {
		out = append(out, expand(n, fuzzy))
	}
	return out
}
