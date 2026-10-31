// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/store"
)

// The search surface (spec §7, matrix §5). Three things live here:
//
//   - SetSearch, which installs the search service;
//   - the translation of a JMAP Email/query filter into Mailroom's OWN query AST
//     (searchFilter → search.Query), which is what keeps engine syntax out of the
//     protocol and out of clients;
//   - SearchSnippet/get (RFC 8621 §5): the highlighted fragments explaining why
//     each message on the page matched.
//
// Isolation: every search runs through search.Service with the user id taken from
// the token context (never a request parameter — spec §10), and search.Indexer
// executes it against exactly that user's index directory. There is no cross-user
// index and no query that can name one — the same fail-closed posture RLS gives
// the database.

// capSearch is the capability URN advertising Mailroom's search extension: the
// `query` filter condition (the operator syntax users type) on Email/query.
// SearchSnippet/get itself is standard RFC 8621 and rides on urn:…:jmap:mail.
//
// IT GATES NO METHOD AT ALL (Forgejo #310), and that follows from the sentence
// above rather than contradicting it: the extension is a FilterCondition and a
// vocabulary, not a method, and both of the methods it shows up on — Email/query
// and SearchSnippet/get — are gated by urn:ietf:params:jmap:mail. So declaring
// this URN is a conformance signal (RFC 8620 §3.3 requires it of a client that
// means to send `query`), not an access decision; a client that sends `query`
// without it gets its search, and a client that declares it on a server with no
// index still gets unsupportedFilter rather than a lie. See mayCall in
// capability.go for what the `using` list does and does not do, and
// capabilitiesGatingNoMethod, which pins this URN as one that gates nothing.
const capSearch = "urn:mailroom:search"

// errUnsupportedFilter is the JMAP error for a filter this server cannot honor
// (RFC 8620 §5.5). Answering it is REQUIRED: silently ignoring a filter condition
// returns a SUPERSET of what the client asked for, and the client has no way to
// know its constraint was dropped.
const errUnsupportedFilter = "unsupportedFilter"

// SetSearch installs the search service. Call it during wiring, before Handler()
// is mounted and before anything is served.
//
// The method table IS this package's dependency graph, so a method that needs
// search is registered as a closure that captures it:
//
//   - SearchSnippet/get is added (it exists only when search does);
//   - Email/query is REPLACED with the search-aware variant.
//
// It also records the service on the API itself (the `search` field), the one
// exception to "no field, just a closure": Account/getHealth (mailroom#41) needs
// to read index staleness OUTSIDE any one JMAP method's request, and a nil field
// is exactly the "search not installed" signal it needs to omit that reading
// instead of reporting on an index that does not exist.
//
// Without a search service, Email/query still answers every structured filter and
// REJECTS a text filter with unsupportedFilter rather than silently ignoring it.
// A Mailroom whose index is broken or absent still serves mail — search is a
// disposable cache, not a dependency of the mail store.
func (a *API) SetSearch(svc *search.Service) {
	if svc == nil {
		return
	}
	a.search = svc
	a.methods["Email/query"] = func(ctx context.Context, userID ulid.ULID, raw json.RawMessage) (json.RawMessage, *MethodError) {
		return a.emailQueryWith(ctx, svc, userID, raw)
	}
	a.methods["SearchSnippet/get"] = func(ctx context.Context, userID ulid.ULID, raw json.RawMessage) (json.RawMessage, *MethodError) {
		return a.searchSnippetGet(ctx, svc, userID, raw)
	}
}

// searchCapability advertises the extension's shape to clients (Session resource).
func searchCapability() map[string]any {
	return map[string]any{
		// The parser's own list (search.Operators), so the session can never
		// advertise an operator the parser does not know, or hide one it does.
		"operators":             search.Operators(),
		"maxSnippetsPerRequest": maxSnippets,
		// Email/query's searchLimit bounds (#769).
		"defaultSearchLimit": search.DefaultLimit,
		"maxSearchLimit":     search.MaxLimit,
		"typoTolerance":      true,
		"prefixSearch":       true,
		"phraseSearch":       true,
		// Email/query accepts {"property": "relevance"} (#724): the index's own
		// ranking, best match first, paged by position like any other sort.
		"relevanceSort": true,
	}
}

// --- Email/query text filtering ----------------------------------------------

// searchFilter is the text half of an Email/query FilterCondition: the RFC 8621
// §4.4.1 text conditions, plus Mailroom's `query` extension (the operator syntax:
// `from:alice is:unread invoice`).
type searchFilter struct {
	Text    string `json:"text"`
	From    string `json:"from"`
	To      string `json:"to"`
	Cc      string `json:"cc"`
	Subject string `json:"subject"`
	Body    string `json:"body"`

	// Query is the Mailroom extension (capSearch): the whole operator syntax in
	// one string, exactly what a search box produces.
	Query string `json:"query"`

	// QueryPrefix (capSearch, #724) treats the FINAL bare word of Query as a
	// prefix: search.ParseOptions.Prefix, the as-you-type mode. A search box
	// sends it while the user is still typing, so "invo" already finds
	// "invoice" instead of flickering to nothing. It is a property of the
	// filter rather than of the request so SearchSnippet/get — which is
	// handed the same filter (RFC 8621 §5.1) — highlights what the query
	// actually matched. It is a modifier, not a condition: on its own it
	// filters nothing.
	QueryPrefix bool `json:"queryPrefix"`
}

// empty reports whether no text condition was supplied.
func (f searchFilter) empty() bool {
	return f.Text == "" && f.From == "" && f.To == "" && f.Cc == "" &&
		f.Subject == "" && f.Body == "" && f.Query == ""
}

// validate reports a `query` string containing a clause that can match nothing
// (search.SyntaxError) as invalidArguments, so the client can show the reason
// instead of an empty result list (#712).
func (f searchFilter) validate() *MethodError {
	if f.Query == "" {
		return nil
	}
	if mErr := f.checkLength(); mErr != nil {
		return mErr
	}
	if _, err := search.ParseChecked(f.Query, search.ParseOptions{}); err != nil {
		return &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	return nil
}

// checkLength refuses a `query` longer than search.MaxQueryBytes before anything
// parses it: a query is a search box's worth of text, and the parser's work
// grows with it. (Saved searches are capped lower still, by the store.)
func (f searchFilter) checkLength() *MethodError {
	if len(f.Query) > search.MaxQueryBytes {
		return &MethodError{Type: errInvalidArguments, Description: fmt.Sprintf(
			"the search query is %d bytes; the limit is %d", len(f.Query), search.MaxQueryBytes)}
	}
	return nil
}

// ast translates the filter into Mailroom's query AST. The RFC's text conditions
// AND together (RFC 8621 §4.4.1); the `query` string contributes its own parsed
// tree, and its scope (Query.Anywhere, `in:anywhere`).
//
// Address conditions (from/to/cc) are deliberately matched TWO ways — as an exact
// address AND as a name token — because RFC 8621 defines them as a match against
// the header, which carries both: `from: "Acme Billing"` and
// `from: "billing@example.com"` must each work.
func (f searchFilter) ast(fuzzy bool) search.Query {
	var clauses []search.Node
	anywhere := false

	if f.Query != "" {
		parsed := search.Parse(f.Query, search.ParseOptions{NoFuzzy: !fuzzy, Prefix: f.QueryPrefix})
		clauses = append(clauses, parsed.Root)
		anywhere = parsed.Anywhere
	}
	if f.Text != "" {
		clauses = append(clauses, search.TermNode(search.FieldAny, f.Text, search.MatchToken))
	}
	if f.Subject != "" {
		clauses = append(clauses, search.TermNode(search.FieldSubject, f.Subject, search.MatchToken))
	}
	if f.Body != "" {
		clauses = append(clauses, search.TermNode(search.FieldBody, f.Body, search.MatchToken))
	}
	if f.From != "" {
		clauses = append(clauses, addressClause(search.FieldFrom, f.From))
	}
	if f.To != "" {
		clauses = append(clauses, addressClause(search.FieldTo, f.To))
	}
	if f.Cc != "" {
		// Cc is indexed together with To (the "other participants" set): the
		// schema keeps no Cc column, and the distinction is not one mail search
		// meaningfully turns on.
		clauses = append(clauses, addressClause(search.FieldTo, f.Cc))
	}

	q := search.Query{Fuzzy: fuzzy, Anywhere: anywhere}
	switch len(clauses) {
	case 0:
		q.Root = search.All{}
	case 1:
		q.Root = clauses[0]
	default:
		q.Root = search.And{Nodes: clauses}
	}
	return q
}

// searchPlan is how Email/query answers a filter's text half: what the index
// is asked, what the store evaluates, and which of the default exclusions
// apply.
type searchPlan struct {
	// index is the query the search INDEX runs for the candidate set. Its Root
	// is All when the index has nothing to say (`is:trashed`, `area:Work`,
	// `in:anywhere` alone): Postgres then answers the whole question.
	index search.Query
	// store is the store-side half (search.SplitStore: area:, is:snoozed, …),
	// nil when there is none. Its IndexSets still need the index.
	store search.Node

	// onlyTrashed / onlyJunk: a required is:trashed / is:junk, taken out of the
	// index's query and answered by Postgres (#623, #723).
	onlyTrashed, onlyJunk bool
	// includeDeleted: the query takes deleted mail into its own hands
	// (`in:anywhere`, or trash named as something to include inside an OR),
	// so the default "deleted is gone" predicate is dropped.
	includeDeleted bool
	// excludeJunk: the search box's default — junk is excluded unless the
	// query names it or says in:anywhere.
	excludeJunk bool
}

// plan splits the filter for Email/query.
//
// A required `is:trashed` (or `in:trash`) is taken out of the index's query and
// answered by Postgres instead (store.EmailQuery.OnlyTrashed). The index's
// trashed flag is Mailroom's own trashed_at, so it has never heard of mail
// another client moved to Trash, and filtering the candidates on it would drop
// exactly that mail before Postgres saw it. `is:junk` / `in:junk` get the same
// treatment for the same reason (store.EmailQuery.OnlyJunk, #723).
//
// The junk default applies only to the `query` extension — the search box.
// The RFC 8621 text conditions keep their plain JMAP meaning for any client.
func (f searchFilter) plan(fuzzy bool) searchPlan {
	q := f.ast(fuzzy)
	full := q.Root
	split := search.SplitStore(q.Root)
	p := searchPlan{index: q, store: split.Store}
	p.index.Root = split.Index

	if search.RequiresFlag(p.index.Root, search.FlagTrashed) {
		p.index.Root = search.WithoutRequiredFlag(p.index.Root, search.FlagTrashed)
		p.onlyTrashed = true
	} else if q.Anywhere || search.MentionsFlagPositively(full, search.FlagTrashed) {
		p.includeDeleted = true
	}
	if search.RequiresFlag(p.index.Root, search.FlagJunk) {
		p.index.Root = search.WithoutRequiredFlag(p.index.Root, search.FlagJunk)
		p.onlyJunk = true
	} else if f.Query != "" && !q.Anywhere && !search.MentionsFlagPositively(full, search.FlagJunk) {
		p.excludeJunk = true
	}
	return p
}

// snippetQuery is the query SearchSnippet/get highlights with: the whole text
// tree, minus what the index cannot evaluate (store-side predicates) and minus a
// required trashed/junk flag — the rows being highlighted came from an
// Email/query that answered those in Postgres, and a snippet query that kept
// them would find no text in mail another client trashed or filed as junk.
func (f searchFilter) snippetQuery(fuzzy bool) search.Query {
	q := f.ast(fuzzy)
	q.Root = search.WithoutStore(q.Root)
	q.Root = search.WithoutRequiredFlag(q.Root, search.FlagTrashed)
	q.Root = search.WithoutRequiredFlag(q.Root, search.FlagJunk)
	return q
}

// addressClause matches an address condition against the exact address field, the
// analyzed participant text (which carries display names) and, for a bare domain,
// the domain view.
func addressClause(f search.Field, v string) search.Node {
	branches := []search.Node{
		search.ExactTerm(f, v),
		search.TermNode(search.FieldPeople, v, search.MatchToken),
	}
	// A bare domain ("shop.example") also matches that domain and its
	// subdomains, as `from:shop.example` does in the query syntax (#712).
	if d, ok := search.AddressDomainTerm(f, v); ok {
		branches = append(branches, d)
	}
	return search.Or{Nodes: branches}
}

// searchResult is the text half of a filter, evaluated: the candidate ids for
// store.EmailQuery.RestrictIDs, the store-side tree with its index subtrees
// resolved, and whether any index answer was cut short by its hit limit.
type searchResult struct {
	// candidates: nil means "the index puts no restriction" (no text filter,
	// or nothing the index has to answer); a non-nil EMPTY slice means "the
	// text matched nothing", which must return nothing — the distinction
	// store.EmailQuery.RestrictIDs preserves.
	candidates []ulid.ULID
	// store is the plan's store half with every IndexSet replaced by the
	// IDSet the index returned for it; nil when there is none.
	store search.Node
	// capped is true when an index answer filled its hit limit, so the
	// candidates (or an IDSet) may be incomplete — reported to the client as
	// searchCapped rather than passed off as the total.
	capped bool
}

// searchCandidates runs the index's part of a filter's text half, asking the
// index for at most limit hits (Email/query's searchLimit, #769).
func searchCandidates(ctx context.Context, svc *search.Service, userID ulid.ULID, f searchFilter, limit int) (searchResult, *MethodError) {
	var out searchResult
	if f.empty() {
		return out, nil
	}
	p := f.plan(true)
	if p.store != nil {
		resolved, capped, mErr := resolveIndexSets(ctx, svc, userID, p.store, p.index.Fuzzy, limit)
		if mErr != nil {
			return out, mErr
		}
		out.store, out.capped = resolved, capped
	}
	if _, all := p.index.Root.(search.All); all {
		// Nothing for the index to narrow: `is:trashed`, `area:Work` or
		// `in:anywhere` alone is answered by Postgres in full. Asking the index
		// anyway would only cut the result down to its first N hits.
		return out, nil
	}
	if svc == nil {
		return out, &MethodError{Type: errUnsupportedFilter,
			Description: "this server has no search backend; text filters are unavailable"}
	}
	p.index.Limit = limit
	res, err := svc.Run(ctx, userID, p.index)
	if err != nil {
		return out, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	out.candidates = make([]ulid.ULID, 0, len(res.Hits))
	for _, h := range res.Hits {
		out.candidates = append(out.candidates, h.ID)
	}
	if len(res.Hits) >= p.index.EffectiveLimit() {
		out.capped = true
	}
	return out, nil
}

// resolveIndexSets runs each IndexSet of a store tree against the index and
// replaces it with the IDSet of its hits.
func resolveIndexSets(ctx context.Context, svc *search.Service, userID ulid.ULID, n search.Node, fuzzy bool, limit int) (search.Node, bool, *MethodError) {
	capped := false
	var walk func(n search.Node) (search.Node, *MethodError)
	walk = func(n search.Node) (search.Node, *MethodError) {
		switch v := n.(type) {
		case search.IndexSet:
			if svc == nil {
				return nil, &MethodError{Type: errUnsupportedFilter,
					Description: "this server has no search backend; text filters are unavailable"}
			}
			q := search.Query{Root: v.Node, Fuzzy: fuzzy, Limit: limit}
			res, err := svc.Run(ctx, userID, q)
			if err != nil {
				return nil, &MethodError{Type: errServerFail, Description: err.Error()}
			}
			set := search.IDSet{IDs: make([]ulid.ULID, 0, len(res.Hits))}
			for _, h := range res.Hits {
				set.IDs = append(set.IDs, h.ID)
			}
			if len(res.Hits) >= q.EffectiveLimit() {
				set.Capped, capped = true, true
			}
			return set, nil
		case search.And:
			kids, mErr := walkAll(v.Nodes, walk)
			return search.And{Nodes: kids}, mErr
		case search.Or:
			kids, mErr := walkAll(v.Nodes, walk)
			return search.Or{Nodes: kids}, mErr
		case search.Not:
			inner, mErr := walk(v.Node)
			return search.Not{Node: inner}, mErr
		}
		return n, nil
	}
	out, mErr := walk(n)
	return out, capped, mErr
}

func walkAll(ns []search.Node, walk func(search.Node) (search.Node, *MethodError)) ([]search.Node, *MethodError) {
	out := make([]search.Node, 0, len(ns))
	for _, c := range ns {
		r, mErr := walk(c)
		if mErr != nil {
			return nil, mErr
		}
		out = append(out, r)
	}
	return out, nil
}

// storePredicate compiles a resolved store tree into the store's predicate,
// resolving area NAMES to ids in the caller's transaction. An area name that
// names nothing matches nothing (SearchNone), exactly as an unknown label: does
// — never "no restriction", which would return the whole mailbox.
func storePredicate(ctx context.Context, tx *store.UserTx, n search.Node) (store.SearchPredicate, error) {
	kids := func(ns []search.Node, op store.SearchOp) (store.SearchPredicate, error) {
		p := store.SearchPredicate{Op: op}
		for _, c := range ns {
			k, err := storePredicate(ctx, tx, c)
			if err != nil {
				return store.SearchPredicate{}, err
			}
			p.Kids = append(p.Kids, k)
		}
		return p, nil
	}
	switch v := n.(type) {
	case search.And:
		return kids(v.Nodes, store.SearchAnd)
	case search.Or:
		return kids(v.Nodes, store.SearchOr)
	case search.Not:
		return kids([]search.Node{v.Node}, store.SearchNot)
	case search.All:
		return store.SearchPredicate{Op: store.SearchAll}, nil
	case search.None:
		return store.SearchPredicate{Op: store.SearchNone}, nil
	case search.IDSet:
		return store.SearchPredicate{Op: store.SearchIDs, IDs: v.IDs}, nil
	case search.StoreTerm:
		switch v.Kind {
		case search.StoreArea:
			id, found, err := tx.AreaIDByName(ctx, v.Value)
			if err != nil {
				return store.SearchPredicate{}, err
			}
			if !found {
				return store.SearchPredicate{Op: store.SearchNone}, nil
			}
			return store.SearchPredicate{Op: store.SearchArea, AreaID: id}, nil
		case search.StoreSnoozed:
			return store.SearchPredicate{Op: store.SearchSnoozed}, nil
		case search.StoreMuted:
			return store.SearchPredicate{Op: store.SearchMuted}, nil
		case search.StorePinned:
			return store.SearchPredicate{Op: store.SearchPinned}, nil
		case search.StoreAboveLine:
			return store.SearchPredicate{Op: store.SearchAboveLine}, nil
		case search.StoreVIP:
			return store.SearchPredicate{Op: store.SearchVIP}, nil
		}
		return store.SearchPredicate{}, fmt.Errorf("api: unknown store search predicate %q", v.Kind)
	}
	// SplitStore wraps every other node in an IndexSet, and resolveIndexSets
	// replaced those; anything else here is a bug, and failing beats guessing.
	return store.SearchPredicate{}, fmt.Errorf("api: unexpected %T in the store half of a search", n)
}

// --- SearchSnippet/get -------------------------------------------------------

type searchSnippetGetArgs struct {
	AccountID string          `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	EmailIDs  []string        `json:"emailIds"`
}

// snippetObject is one RFC 8621 §5.1 SearchSnippet. A null subject/preview means
// nothing in that field matched — the RFC's way of saying "render your own".
type snippetObject struct {
	EmailID string  `json:"emailId"`
	Subject *string `json:"subject"`
	Preview *string `json:"preview"`
}

type searchSnippetGetResult struct {
	AccountID string          `json:"accountId"`
	List      []snippetObject `json:"list"`
	NotFound  []string        `json:"notFound"`
}

// maxSnippets bounds one SearchSnippet/get: highlighting re-reads and re-scores
// the stored text of every requested message, so it is priced per PAGE, never per
// mailbox.
const maxSnippets = 100

// searchSnippetGet implements SearchSnippet/get (RFC 8621 §5): for the emails on
// the page a client just fetched, return the fragments that matched, marked up
// with <mark>. The engine's own markup conventions never reach the client —
// search.Snippet.Mark renders the fragments Mailroom's way.
func (a *API) searchSnippetGet(ctx context.Context, svc *search.Service, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args searchSnippetGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if len(args.EmailIDs) > maxSnippets {
		return nil, &MethodError{Type: errRequestTooLarge,
			Description: "too many emailIds for one SearchSnippet/get"}
	}

	f, mErr := parseSnippetFilter(args.Filter)
	if mErr != nil {
		return nil, mErr
	}
	if mErr := f.checkLength(); mErr != nil {
		return nil, mErr
	}
	valid, notFound, requested := parseIDs(args.EmailIDs)

	byID := map[ulid.ULID][]search.Snippet{}
	if len(valid) > 0 && !f.empty() {
		// snippetQuery, not ast: see its comment.
		snips, err := svc.SnippetsFor(ctx, userID, f.snippetQuery(true), valid)
		if err != nil {
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
		byID = snips
	}

	list := make([]snippetObject, 0, len(requested))
	for _, req := range requested {
		id, ok := req.parsed()
		if !ok {
			continue
		}
		obj := snippetObject{EmailID: id.String()}
		for _, s := range byID[id] {
			marked := s.Mark()
			switch s.Field {
			case search.FieldSubject:
				obj.Subject = &marked
			case search.FieldBody:
				obj.Preview = &marked
			}
		}
		list = append(list, obj)
	}
	return mustMarshal(searchSnippetGetResult{
		AccountID: userID.String(),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// parseSnippetFilter reads the text conditions out of a SearchSnippet/get filter,
// which is the SAME filter object the matching Email/query used (RFC 8621 §5.1).
// The structured conditions in it (inMailbox, keywords) do not affect
// highlighting and are ignored here on purpose.
func parseSnippetFilter(raw json.RawMessage) (searchFilter, *MethodError) {
	var f searchFilter
	if len(raw) == 0 || isJSONNull(raw) {
		return f, nil
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, &MethodError{Type: errInvalidArguments, Description: "unsupported filter"}
	}
	return f, nil
}

// jmapTime parses a JMAP UTCDate (RFC 8620 §1.4: "…Z", no fraction), tolerating
// the more precise RFC 3339 forms a client might send rather than rejecting a
// request that is merely more exact than the spec requires.
func jmapTime(s string) (time.Time, bool) {
	if t, err := time.Parse("2006-01-02T15:04:05Z", s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
