// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The operator syntax users type, parsed into Mailroom's AST (spec §7, matrix
// §5: "Field operators (from/to/subject/label/has/is/account/date) — Mailroom-
// owned query AST").
//
//	invoice overdue            free text, ALL terms required (AND)
//	"quarterly revenue"        phrase (ordered, position-aware)
//	overd*                     prefix / as-you-type
//	from:alice@example.com     exact sender address
//	from:shop.example          sender at that domain OR any subdomain (#712);
//	                           from:*.shop.example and from:@shop.example are the same
//	to:bob@example.com         exact recipient (To or Cc); to:/cc: take a domain too
//	alias:me@example.com       the address it was DELIVERED to (#723); deliveredto:
//	                           is the same, and both take a domain like from: does
//	shop.example               a bare dotted word is a PHRASE, never its parts (#712)
//	subject:invoice            analyzed term, subject only
//	body:refund                analyzed term, body only
//	label:Work                 label BY NAME (resolved to its id — see Resolver)
//	label:"Client Work"        …quoted when it has spaces
//	account:personal           account BY NAME
//	area:Work                  the AREA it resolves to on the desk, by name (#722) —
//	                           answered by the store, not the index (StoreTerm)
//	list:lists.example.org     the List-Id, or any list under that domain (#722)
//	filename:report.pdf        an attachment's file name or extension (#722)
//	larger:5M  smaller:100K    size in bytes; K, M, G are binary (#722)
//	is:unread is:starred       state (read/unread/starred/answered/draft/junk/trashed)
//	is:snoozed is:muted        desk state, answered by the store (#723); also
//	is:pinned is:important     is:above-line (= is:important: above the desk's line)
//	is:vip                     from a contact the user marked VIP (#59), by the store
//	has:attachment             has:* is a synonym family of is:*
//	has:pdf has:image          attachment type families (attachtype.go, #723), and
//	has:unsubscribe            a List-Unsubscribe header (#722)
//	in:trash in:junk           deleted / junk mail (#723)
//	in:anywhere                do not exclude deleted and junk mail (#723)
//	after:2026-01-01           received on/after that UTC day
//	before:2026-07-01          received strictly before that UTC day
//	on:2026-07-12              received on that UTC day
//	date:2026-07               received in that month (or year: date:2026)
//	newer_than:7d              received on/after the UTC day 7 days ago (#722);
//	older_than:1y              received before the UTC day a year ago; units
//	                           d, w, m (calendar months), y
//	-is:read  -label:Noise     any clause negates with a leading '-'
//	a OR b                     either (#722); OR is UPPER CASE ONLY and binds
//	                           looser than the implicit AND: `a b OR c` is (a b) OR c
//	(a OR b) -(c d)            brackets group, and '-' negates a group
//
// Anything else with a colon (`foo:bar`, a URL, a Message-ID) is NOT an operator
// and is parsed as free text — a search box must never turn a paste into an
// empty result set. The same rule makes brackets forgiving: one that closes
// nothing is dropped, one never closed runs to the end of the query, and a '('
// or ')' inside a word (a URL, `Foo_(bar)`) is just part of the word.
//
// Names (label:, account:, area:) stay NAMES in the AST. Resolving them to ids
// needs the database, which the parser deliberately does not have; Service.
// Resolve does it for label/account (and turns an unknown name into None —
// matches nothing — rather than dropping the clause), and Email/query does it
// for area the same way.

// MaxGroupDepth bounds bracket nesting. The parser recurses once per open
// group, and a Go stack overflow is a fatal crash of the whole server, so a
// query of ten thousand '(' must not be able to drive it: past this depth a '('
// is an ordinary character of the word it starts (and its ')' with it), which
// is the same forgiving treatment any other unbalanced bracket gets. Every
// other walk of the tree (Expand, resolve, SplitStore, the backends) recurses
// on the tree's depth too, so this bounds them all.
const MaxGroupDepth = 32

// MaxQueryBytes bounds a query string. Callers that take user text refuse a
// longer one (ErrQueryTooLong) rather than parse it.
const MaxQueryBytes = 4 << 10

// ErrQueryTooLong is returned for a query string over MaxQueryBytes.
var ErrQueryTooLong = fmt.Errorf("search: the query is longer than %d bytes", MaxQueryBytes)

// ParseOptions tunes parsing. The zero value is the search-box default: typo
// tolerance on, no as-you-type.
type ParseOptions struct {
	// NoFuzzy disables typo tolerance for this query.
	NoFuzzy bool

	// Prefix treats the FINAL bare word as a prefix (as-you-type). A query the
	// user is still typing ends in a partial word; treating it as a whole word
	// makes the result list flicker to empty on every keystroke.
	Prefix bool

	// Now is the instant relative dates (newer_than:, older_than:) count back
	// from. Zero means time.Now(); tests pin it.
	Now time.Time
}

// Parse turns a user query string into a Query. It never fails: unparseable
// fragments degrade to free text, and a clause that can match nothing (see
// ParseChecked) becomes None, which matches nothing. An empty (or
// whitespace-only) query yields a match-everything Query, which callers combine
// with their own filters.
func Parse(q string, opts ParseOptions) Query {
	out, _ := parse(q, opts)
	return out
}

// ParseChecked is Parse plus validation: it returns a *SyntaxError for the first
// clause whose syntax can match nothing (`from:*foo`, `from:@`, `older_than:3x`,
// `larger:big`) — so the API can say so, instead of returning an empty list that
// reads as "no such mail". The Query returned alongside the error is the same one
// Parse would return.
func ParseChecked(q string, opts ParseOptions) (Query, error) {
	return parse(q, opts)
}

// parser is a recursive-descent parser over tokenize's items:
//
//	expr  := conj ( OR conj )*
//	conj  := unary*                 (implicit AND)
//	unary := word | ['-'] '(' expr [')']
//
// An empty alternative (`a OR`, `OR b`, `()`) is dropped, so a query being
// typed or pasted with a dangling OR still means what its words say.
type parser struct {
	items    []item
	pos      int
	opts     ParseOptions
	now      time.Time
	firstErr error
	anywhere bool
	depth    int // open groups being parsed (bounded by MaxGroupDepth)
}

func parse(q string, opts ParseOptions) (Query, error) {
	p := &parser{items: tokenize(q), opts: opts, now: opts.Now}
	if p.now.IsZero() {
		p.now = time.Now()
	}
	root := p.expr(false)
	if root == nil {
		root = All{}
	}
	return Query{Root: root, Fuzzy: !opts.NoFuzzy, Anywhere: p.anywhere}, p.firstErr
}

func (p *parser) expr(inGroup bool) Node {
	var alts []Node
	for {
		if n := andOf(p.conj(inGroup)); n != nil {
			alts = append(alts, n)
		}
		if p.pos < len(p.items) && p.items[p.pos].kind == itemOr {
			p.pos++
			continue
		}
		break
	}
	switch len(alts) {
	case 0:
		return nil
	case 1:
		return alts[0]
	}
	return Or{Nodes: alts}
}

// conj reads clauses until an OR, the closing bracket of the group it is in, or
// the end of the query.
func (p *parser) conj(inGroup bool) []Node {
	var out []Node
	for p.pos < len(p.items) {
		it := p.items[p.pos]
		switch it.kind {
		case itemOr:
			return out
		case itemClose:
			if inGroup {
				return out
			}
			p.pos++ // unreachable: tokenize drops a bracket that closes nothing
		case itemOpen:
			p.pos++
			if p.depth >= MaxGroupDepth {
				// Unreachable through tokenize, which stops opening groups at
				// this depth; kept so the parser's recursion is bounded by its
				// own state whatever it is handed.
				continue
			}
			p.depth++
			inner := p.expr(true)
			p.depth--
			if p.pos < len(p.items) && p.items[p.pos].kind == itemClose {
				p.pos++
			} // else: never closed — the group runs to the end of the query
			if inner == nil {
				continue // "()" or "-()" says nothing
			}
			if it.negated {
				inner = Not{Node: inner}
			}
			out = append(out, inner)
		case itemWord:
			last := p.pos == len(p.items)-1
			p.pos++
			n, err := parseToken(it.tok, p.opts, last, p.now)
			if err != nil {
				if p.firstErr == nil {
					p.firstErr = err
				}
				n = None{}
			}
			if n == nil {
				continue
			}
			if _, ok := n.(anywhereMarker); ok {
				// A scope, not a predicate (Query.Anywhere). Negating it
				// would mean "the default", which is what leaving it out
				// already says.
				if !it.tok.negated {
					p.anywhere = true
				}
				continue
			}
			if it.tok.negated {
				n = Not{Node: n}
			}
			out = append(out, n)
		}
	}
	return out
}

// andOf is the implicit AND of clauses: nil for none, the clause itself for one.
func andOf(clauses []Node) Node {
	switch len(clauses) {
	case 0:
		return nil
	case 1:
		return clauses[0]
	}
	return And{Nodes: clauses}
}

// anywhereMarker is what parseToken returns for `in:anywhere`; conj turns it
// into Query.Anywhere. It never appears in a Query's tree.
type anywhereMarker struct{}

func (anywhereMarker) node() {}

// parseToken maps one token to a node, or nil to drop it (an empty value). A
// non-nil error is a clause that can match nothing; Parse turns it into None.
func parseToken(t token, opts ParseOptions, last bool, now time.Time) (Node, error) {
	if t.field == "" {
		if t.value == "" {
			return nil, nil
		}
		switch {
		case t.quoted:
			return TermNode(FieldAny, t.value, MatchPhrase), nil
		case strings.HasSuffix(t.value, "*"):
			v := strings.TrimSuffix(t.value, "*")
			if v == "" {
				return nil, nil
			}
			return TermNode(FieldAny, v, MatchPrefix), nil
		case opts.Prefix && last && !t.negated:
			return TermNode(FieldAny, t.value, MatchPrefix), nil
		case looksLikeEmail(t.value):
			// A bare address is ONE thing too (#728). As a token query it was
			// fuzzed: "alice@example.com" also matched alice@example.org and
			// alices@example.com. It is routed to the address fields as an exact
			// match (from, to/cc, delivered-to) rather than searched as a phrase
			// like a bare domain: a phrase goes through the text analyzer, whose
			// stemming still folds alices@ into alice@, so it would remain a
			// near-miss match. Mentions of the address in a body are not found by
			// a bare address; quote it ("alice@example.com") for that.
			v := t.value
			return Or{Nodes: []Node{
				ExactTerm(FieldFrom, v), ExactTerm(FieldTo, v), ExactTerm(FieldDeliveredTo, v),
			}}, nil
		case looksLikeDomain(t.value):
			// A bare hostname is ONE thing. As a token query it decayed into its
			// parts (#712): the analyzer splits "shop.example" into "shop" and
			// "example", every part is required only somewhere in the field, and
			// the typo-tolerance n-grams of ".com" matched ~90% of a mailbox.
			// A phrase keeps the parts adjacent and in order, and is never fuzzed.
			return TermNode(FieldAny, t.value, MatchPhrase), nil
		default:
			return TermNode(FieldAny, t.value, MatchToken), nil
		}
	}

	switch t.field {
	case "from":
		return addressNode("from", FieldFrom, FieldFromDomain, t.value)
	case "to", "cc", "recipient":
		return addressNode(t.field, FieldTo, FieldToDomain, t.value)
	case "alias", "deliveredto":
		// The address the message reached the user at — the same domain and
		// subdomain semantics as from: (#712) for a value without '@'.
		return addressNode(t.field, FieldDeliveredTo, FieldDeliveredToDomain, t.value)
	case "subject":
		return textFieldTerm(FieldSubject, t), nil
	case "body":
		return textFieldTerm(FieldBody, t), nil
	case "filename":
		return filenameNode(t), nil
	case "label":
		// Value is a NAME; Service.Resolve rewrites it to an id term.
		return Term{Field: FieldLabel, Value: t.value, Match: MatchExact, Boost: 1}, nil
	case "account":
		return Term{Field: FieldAccount, Value: t.value, Match: MatchExact, Boost: 1}, nil
	case "area":
		// Value is a NAME, resolved by the store (Email/query); an empty one
		// (mid-keystroke) matches nothing, as an empty label: does.
		if v := strings.TrimSpace(t.value); v != "" {
			return StoreTerm{Kind: StoreArea, Value: v}, nil
		}
		return None{}, nil
	case "list":
		return ExactTerm(FieldList, normalizeListID(t.value)), nil
	case "larger", "smaller":
		return sizeNode(t.field, t.value)
	case "is", "has":
		if atom, ok := flagAtom(t.value); ok {
			return ExactTerm(FieldFlag, atom), nil
		}
		if kind, ok := storeStateKind(t.value); ok {
			return StoreTerm{Kind: kind}, nil
		}
		// An unknown is:/has: value is a user error, not a wildcard: match
		// nothing rather than silently ignoring the constraint.
		return None{}, nil
	case "in":
		return inNode(t.value), nil
	case "keyword":
		return Term{Field: FieldKeyword, Value: t.value, Match: MatchExact, Boost: 1}, nil
	case "after", "since":
		if d, ok := parseDay(t.value); ok {
			return DateRange{From: d}, nil
		}
	case "before", "until":
		if d, ok := parseDay(t.value); ok {
			return DateRange{To: d}, nil
		}
	case "on":
		if d, ok := parseDay(t.value); ok {
			return DateRange{From: d, To: d.AddDate(0, 0, 1)}, nil
		}
	case "date":
		if from, to, ok := parsePeriod(t.value); ok {
			return DateRange{From: from, To: to}, nil
		}
	case "newer_than", "older_than":
		return relativeDateNode(t.field, t.value, now)
	}

	// Not an operator we know (or an unparseable date): fall back to free text
	// over the whole original word, so a pasted URL or Message-ID still searches.
	return TermNode(FieldAny, t.raw, MatchToken), nil
}

// textFieldTerm builds a subject:/body: term, honoring quoting (phrase) and a
// trailing '*' (prefix).
func textFieldTerm(f Field, t token) Node {
	switch {
	case t.quoted:
		return TermNode(f, t.value, MatchPhrase)
	case strings.HasSuffix(t.value, "*"):
		v := strings.TrimSuffix(t.value, "*")
		if v == "" {
			return nil
		}
		return TermNode(f, v, MatchPrefix)
	default:
		return TermNode(f, t.value, MatchToken)
	}
}

// filenameNode builds filename:. The field is ANALYZED, so `filename:pdf` finds
// the extension and `filename:invoice` a word of the name. A dotted value
// (`filename:report.pdf`) is a phrase — "report" right before "pdf" — for the
// reason a bare domain is (#712): as separate tokens, a message with
// report.docx and photo.pdf would match. A value that is only an extension
// (".pdf") is one token, not a phrase.
func filenameNode(t token) Node {
	v := strings.TrimSpace(t.value)
	if !t.quoted && !strings.HasSuffix(v, "*") && strings.Contains(strings.Trim(v, "."), ".") {
		return TermNode(FieldFilename, v, MatchPhrase)
	}
	if !t.quoted {
		t.value = strings.TrimLeft(v, ".")
	}
	return textFieldTerm(FieldFilename, t)
}

// flagAtom maps the user-facing is:/has: vocabulary (and its synonyms) onto the
// atoms the index actually carries. Keeping the synonym table here — in the AST
// layer — is exactly the point of owning the query language: the index stores one
// canonical atom per state and never learns about aliases.
func flagAtom(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "unread", "new":
		return FlagUnread, true
	case "read", "seen":
		return FlagRead, true
	case "starred", "flagged", "star":
		return FlagStarred, true
	case "answered", "replied":
		return FlagAnswered, true
	case "draft":
		return FlagDraft, true
	case "junk", "spam":
		return FlagJunk, true
	case "trashed", "trash", "deleted":
		return FlagTrashed, true
	case "attachment", "attachments", "file":
		return FlagAttachment, true
	case "unsubscribe":
		return FlagUnsubscribe, true
	}
	return familyAtom(v)
}

// storeStateKind maps the is:/has: values the INDEX cannot answer (StoreTerm)
// to their predicate. is:important and is:above-line are one predicate: above
// the user's line on the desk, the threshold the desk itself draws it at.
func storeStateKind(v string) (StoreKind, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "snoozed":
		return StoreSnoozed, true
	case "muted":
		return StoreMuted, true
	case "pinned":
		return StorePinned, true
	case "important", "above-line", "aboveline", "above_line":
		return StoreAboveLine, true
	case "vip":
		return StoreVIP, true
	}
	return "", false
}

// inNode compiles in:. `in:trash` and `in:junk` are the same clauses as
// is:trashed and is:junk (Email/query answers both from Postgres, so mail
// another client put in the Trash or Junk folder counts); `in:anywhere` is a
// scope, returned as a marker. An unknown value matches nothing, like an
// unknown is: value.
func inNode(v string) Node {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "anywhere":
		return anywhereMarker{}
	case "trash", "trashed", "bin":
		return ExactTerm(FieldFlag, FlagTrashed)
	case "junk", "spam":
		return ExactTerm(FieldFlag, FlagJunk)
	}
	return None{}
}

// relativeDateNode compiles newer_than:/older_than: (#722, Gmail-compatible).
// The cutoff is the UTC DAY that lies N units before now — calendar-aware for
// months and years, so a month back from 31 March is 28 (or 29) February, not 3
// March — and the clause is day-granular like every date clause (daterange.go):
//
//	newer_than:N<unit>   received on or after the cutoff day
//	older_than:N<unit>   received before the cutoff day
//
// The two are exact complements for the same value: together they cover every
// message once.
func relativeDateNode(op, raw string, now time.Time) (Node, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return None{}, nil
	}
	fail := func() (Node, error) {
		return nil, &SyntaxError{Operator: op, Value: raw,
			Reason: "use a whole number and a unit: d (days), w (weeks), m (months) or y (years), e.g. 7d, 2w, 6m, 1y"}
	}
	digits, unit := v[:len(v)-1], v[len(v)-1]
	if digits == "" || len(digits) > 4 || strings.Trim(digits, "0123456789") != "" {
		return fail()
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return fail()
	}
	u := now.UTC()
	y, m, d := u.Date()
	var cutoff time.Time
	switch unit {
	case 'd':
		cutoff = time.Date(y, m, d-n, 0, 0, 0, 0, time.UTC)
	case 'w':
		cutoff = time.Date(y, m, d-7*n, 0, 0, 0, 0, time.UTC)
	case 'm':
		months := y*12 + int(m-1) - n
		cutoff = clampedDate(months/12, time.Month(months%12+1), d)
	case 'y':
		cutoff = clampedDate(y-n, m, d)
	default:
		return fail()
	}
	if op == "newer_than" {
		return DateRange{From: cutoff}, nil
	}
	return DateRange{To: cutoff}, nil
}

// clampedDate is year-month-day at UTC midnight with the day clamped to the
// month's last day (Go's time.Date would roll 30 February into March).
func clampedDate(y int, m time.Month, d int) time.Time {
	if last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day(); d > last {
		d = last
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// parseDay parses YYYY-MM-DD (and the shorter YYYY-MM / YYYY) as a UTC midnight.
func parseDay(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parsePeriod parses date:<period> — a whole day, month, or year — into its
// half-open [from, to) range.
func parsePeriod(v string) (from, to time.Time, ok bool) {
	v = strings.TrimSpace(v)
	switch {
	case len(v) == len("2006-01-02"):
		if t, err := time.ParseInLocation("2006-01-02", v, time.UTC); err == nil {
			return t, t.AddDate(0, 0, 1), true
		}
	case len(v) == len("2006-01"):
		if t, err := time.ParseInLocation("2006-01", v, time.UTC); err == nil {
			return t, t.AddDate(0, 1, 0), true
		}
	case len(v) == len("2006"):
		if t, err := time.ParseInLocation("2006", v, time.UTC); err == nil {
			return t, t.AddDate(1, 0, 0), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// --- Tokenizer ---------------------------------------------------------------

// token is one lexed query word: an optional field, its value, and whether the
// value was quoted (a phrase) or negated with a leading '-'.
type token struct {
	field   string // "" for free text
	value   string
	raw     string // the token as typed, minus the '-' (the free-text fallback)
	quoted  bool
	negated bool
}

// itemKind is what one lexed item is: a word, a bracket, or the OR keyword.
type itemKind int

const (
	itemWord itemKind = iota
	itemOpen
	itemClose
	itemOr
)

// item is one lexed item. negated is set on an itemOpen written "-(".
type item struct {
	kind    itemKind
	tok     token
	negated bool
}

// tokenize splits a query on whitespace, honoring double quotes (which may
// follow an operator: label:"Client Work"), brackets, and the OR keyword. An
// unterminated quote runs to the end of the string rather than being an error —
// a user mid-type has one open quote most of the time.
//
// Brackets are recognized only where they can mean grouping: a '(' that starts a
// word (optionally after '-'), and a ')' that closes an open group. A ')' that
// closes nothing is dropped when it stands alone and kept as part of the word
// otherwise, and a '(' inside a word makes its matching ')' part of the word too
// — so `https://example.com/Foo_(bar)` stays one searchable word.
//
// OR is the operator only as a bare, unquoted, upper-case word: "or" and "OR"
// in quotes are words to search for.
func tokenize(q string) []item {
	var (
		out       []item
		cur       strings.Builder
		field     string
		inQ       bool
		neg       bool
		had       bool // cur holds something (possibly an empty quoted string)
		wasQ      bool
		depth     int // open groups
		wordParen int // '(' written into the current word, still unmatched
	)
	flush := func() {
		if !had && cur.Len() == 0 {
			field, neg, wasQ, wordParen = "", false, false, 0
			return
		}
		val := cur.String()
		raw := val
		if field != "" {
			raw = field + ":" + val
		}
		if field == "" && !wasQ && !neg && val == "OR" {
			out = append(out, item{kind: itemOr})
		} else {
			out = append(out, item{kind: itemWord,
				tok: token{field: field, value: val, raw: raw, quoted: wasQ, negated: neg}})
		}
		cur.Reset()
		field, neg, had, wasQ, wordParen = "", false, false, false, 0
	}

	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '"':
			inQ = !inQ
			had = true
			wasQ = true
		case !inQ && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			flush()
		case !inQ && c == '-' && cur.Len() == 0 && field == "" && !neg:
			neg = true
		case !inQ && c == '(' && cur.Len() == 0 && field == "" && !had && depth < MaxGroupDepth:
			out = append(out, item{kind: itemOpen, negated: neg})
			neg = false
			depth++
		case !inQ && c == ')' && wordParen == 0 && depth > 0:
			flush()
			out = append(out, item{kind: itemClose})
			depth--
		case !inQ && c == ')' && wordParen == 0 && cur.Len() == 0 && !had && field == "":
			// A bracket closing nothing, on its own: dropped. As a word it would
			// be a term with no searchable text, which matches nothing.
		case !inQ && c == ':' && field == "" && cur.Len() > 0 && !wasQ && isOperator(cur.String()):
			field = strings.ToLower(cur.String())
			cur.Reset()
		default:
			if !inQ && c == '(' {
				wordParen++
			} else if !inQ && c == ')' && wordParen > 0 {
				wordParen--
			}
			cur.WriteByte(c)
			had = true
		}
	}
	flush()
	return out
}

// operatorNames is every operator the parser knows: the single list isOperator
// tests against and Operators exports (the session capability and the web
// client's vocabulary test read it).
var operatorNames = []string{
	"from", "to", "cc", "recipient", "alias", "deliveredto",
	"subject", "body", "filename",
	"label", "account", "area", "list",
	"is", "has", "in", "keyword",
	"after", "since", "before", "until", "on", "date", "newer_than", "older_than",
	"larger", "smaller",
}

// Operators returns the operator names the parser accepts (without the colon).
func Operators() []string { return append([]string(nil), operatorNames...) }

// isOperator reports whether a bare word before a ':' is one of Mailroom's
// operators. Everything else keeps its colon and stays free text — this is what
// makes `https://example.com/x` and `<abc@example.com>` searchable strings rather
// than a bogus `https:` field query.
func isOperator(w string) bool {
	w = strings.ToLower(w)
	for _, op := range operatorNames {
		if w == op {
			return true
		}
	}
	return false
}

// StateValueKnown reports whether `op:value` (op is "is", "has" or "in") names
// something the parser understands — a flag atom, an attachment family, a
// store-side state, or an in: scope — rather than falling back to matching
// nothing. It exists for the vocabulary test: a suggestion list must not offer
// a value this returns false for (#623).
func StateValueKnown(op, value string) bool {
	switch op {
	case "is", "has":
		if _, ok := flagAtom(value); ok {
			return true
		}
		_, ok := storeStateKind(value)
		return ok
	case "in":
		_, none := inNode(value).(None)
		return !none
	}
	return false
}
