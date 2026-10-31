// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Service is the query-time facade internal/api talks to. It is the only search
// type the protocol layer knows: parse the user's query, resolve the names in it
// to ids, run it against that user's index, and hand back hits (and, on request,
// highlighted snippets).
//
// It exists so that no caller ever holds a SearchBackend, an Indexer, or — least
// of all — an engine type. Swapping Tantivy for Bleve changes nothing above this
// line.
type Service struct {
	ix       *Indexer
	resolver Resolver
	timing   func(TimingKind, time.Duration)
}

// TimingKind says which of the service's two index reads a timing is for. It
// is the ONLY label mailroom_search_duration_seconds carries (#724): a closed
// set of two, never anything from the query, so the series count is fixed.
type TimingKind string

const (
	// TimingSearch is a search proper: Email/query's candidate set, an
	// IndexSet of a store-side query, or Service.Search.
	TimingSearch TimingKind = "search"
	// TimingSnippet is SearchSnippet/get's highlighting read over one page.
	TimingSnippet TimingKind = "snippet"
)

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithSearchTiming installs an observer that receives how long each index
// search took (#724) — the time spent in the engine, not the name resolution
// before it. cmd/mailroom feeds it to mailroom_search_duration_seconds, which
// is what the web client's live-search debounce is tuned from.
//
// It is called for failed searches too: a search that took 3 s and then failed
// still cost the user 3 s, and leaving it out would flatter the histogram.
func WithSearchTiming(f func(TimingKind, time.Duration)) ServiceOption {
	return func(s *Service) { s.timing = f }
}

// NewService binds an indexer and a name resolver into the API-facing service.
func NewService(ix *Indexer, r Resolver, opts ...ServiceOption) *Service {
	s := &Service{ix: ix, resolver: r}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Indexer exposes the underlying indexer, for the operator surfaces that drive it
// (the sync loop, reindex-user, index health).
func (s *Service) Indexer() *Indexer { return s.ix }

// SearchOptions tunes one search.
type SearchOptions struct {
	Limit          int
	Highlight      bool
	Prefix         bool // treat the last word as as-you-type
	NoFuzzy        bool // turn typo tolerance off
	IncludeTrashed bool // by default trashed mail is excluded (see Search)
}

// Search parses a Mailroom query string, resolves its names, and runs it.
//
// Trashed mail is EXCLUDED unless the query says otherwise (`is:trashed`, or
// IncludeTrashed). Deleted-but-not-purged mail lives in Trash for 30 days
// (ADR-009), and a search box that surfaces it by default is a search box that
// keeps showing people the thing they just deleted.
func (s *Service) Search(ctx context.Context, userID ulid.ULID, q string, opts SearchOptions) (Result, error) {
	if len(q) > MaxQueryBytes {
		return Result{}, ErrQueryTooLong
	}
	parsed := Parse(q, ParseOptions{NoFuzzy: opts.NoFuzzy, Prefix: opts.Prefix})
	parsed.Limit = opts.Limit
	parsed.Highlight = opts.Highlight
	if !opts.IncludeTrashed && !parsed.Anywhere && !mentionsFlag(parsed.Root, FlagTrashed) {
		parsed.Root = And{Nodes: []Node{parsed.Root, Not{Node: ExactTerm(FieldFlag, FlagTrashed)}}}
	}
	return s.Run(ctx, userID, parsed)
}

// Run resolves and executes an already-built Query — the entry point the JMAP
// filter translator uses, since it builds the AST directly rather than from a
// string.
func (s *Service) Run(ctx context.Context, userID ulid.ULID, q Query) (Result, error) {
	return s.run(ctx, userID, q, TimingSearch)
}

func (s *Service) run(ctx context.Context, userID ulid.ULID, q Query, kind TimingKind) (Result, error) {
	if userID == (ulid.ULID{}) {
		return Result{}, fmt.Errorf("search: a user identity is required")
	}
	if ContainsStore(q.Root) {
		// A backend would compile a StoreTerm as nothing at all — the clause
		// silently dropped, the query widened. Only a caller with the store can
		// evaluate one (Email/query, via SplitStore).
		return Result{}, ErrStorePredicate
	}
	root, err := s.resolve(ctx, userID, q.Root)
	if err != nil {
		return Result{}, err
	}
	q.Root = root
	if s.timing == nil {
		return s.ix.Search(ctx, userID, q)
	}
	start := time.Now()
	res, err := s.ix.Search(ctx, userID, q)
	s.timing(kind, time.Since(start))
	return res, err
}

// ErrStorePredicate is returned for a query that holds a predicate only the
// store can answer (area:, is:snoozed, … — see StoreTerm) when it is run against
// the index alone.
var ErrStorePredicate = errors.New("search: this query uses operators (area:, is:snoozed, is:muted, is:pinned, is:important, is:vip) that only a mail query can answer")

// Snippets runs a query STRING and returns the highlighted fragments for a
// specific set of message ids — RFC 8621's SearchSnippet/get, which asks "why did
// these messages match?" about the page the client is rendering.
func (s *Service) Snippets(ctx context.Context, userID ulid.ULID, q string, ids []ulid.ULID, opts SearchOptions) (map[ulid.ULID][]Snippet, error) {
	if len(q) > MaxQueryBytes {
		return nil, ErrQueryTooLong
	}
	parsed := Parse(q, ParseOptions{NoFuzzy: opts.NoFuzzy, Prefix: opts.Prefix})
	return s.SnippetsFor(ctx, userID, parsed, ids)
}

// SnippetsFor is Snippets over an already-built Query — what the JMAP layer uses,
// since it translates a FilterCondition into the AST directly.
//
// The query is constrained to exactly the ids asked about: the client already has
// its page and only wants the highlights for it, so there is no reason to score
// the rest of the archive.
func (s *Service) SnippetsFor(ctx context.Context, userID ulid.ULID, q Query, ids []ulid.ULID) (map[ulid.ULID][]Snippet, error) {
	if len(ids) == 0 {
		return map[ulid.ULID][]Snippet{}, nil
	}
	q.Highlight = true
	q.Limit = len(ids)

	idNodes := make([]Node, 0, len(ids))
	for _, id := range ids {
		idNodes = append(idNodes, IDTerm(FieldID, id))
	}
	// Highlighting is about the TEXT terms; a store-side predicate (area:, …)
	// has nothing to highlight and no backend can evaluate it, so it is
	// dropped here rather than refused.
	q.Root = And{Nodes: []Node{WithoutStore(q.Root), Or{Nodes: idNodes}}}

	res, err := s.run(ctx, userID, q, TimingSnippet)
	if err != nil {
		return nil, err
	}
	out := make(map[ulid.ULID][]Snippet, len(res.Hits))
	for _, h := range res.Hits {
		out[h.ID] = h.Snippets
	}
	return out, nil
}

// Reindex rebuilds a user's index from Postgres — the first-class operation
// (spec §7). Exposed here so an operator surface (CLI, admin endpoint) never has
// to reach past the service.
func (s *Service) Reindex(ctx context.Context, userID ulid.ULID) (Progress, error) {
	return s.ix.Reindex(ctx, userID)
}

// Stats reports index health (cursor, lag, document count).
func (s *Service) Stats(ctx context.Context, userID ulid.ULID) (Stats, error) {
	return s.ix.Stats(ctx, userID)
}

// resolve rewrites the name-valued terms the parser produced (label:, account:)
// into id terms. A name that does not resolve becomes None — the clause matches
// NOTHING. Dropping it instead would quietly widen the query (`label:Typo` would
// return the user's whole mailbox), which is the failure mode a search box must
// never have.
func (s *Service) resolve(ctx context.Context, userID ulid.ULID, n Node) (Node, error) {
	switch v := n.(type) {
	case nil:
		return All{}, nil
	case And:
		nodes, err := s.resolveAll(ctx, userID, v.Nodes)
		return And{Nodes: nodes}, err
	case Or:
		nodes, err := s.resolveAll(ctx, userID, v.Nodes)
		return Or{Nodes: nodes}, err
	case Not:
		inner, err := s.resolve(ctx, userID, v.Node)
		return Not{Node: inner}, err
	case Term:
		return s.resolveTerm(ctx, userID, v)
	default:
		return n, nil
	}
}

func (s *Service) resolveAll(ctx context.Context, userID ulid.ULID, ns []Node) ([]Node, error) {
	out := make([]Node, 0, len(ns))
	for _, n := range ns {
		r, err := s.resolve(ctx, userID, n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) resolveTerm(ctx context.Context, userID ulid.ULID, t Term) (Node, error) {
	switch t.Field {
	case FieldLabel, FieldAccount, FieldMailbox, FieldThread, FieldID:
		// Already an id? Then it came from the JMAP filter translator (or a
		// previous resolve) and needs no lookup.
		if _, err := ulid.Parse(t.Value); err == nil {
			return t, nil
		}
	default:
		return t, nil
	}

	var (
		id    ulid.ULID
		found bool
		err   error
	)
	switch t.Field {
	case FieldLabel:
		id, found, err = s.resolver.LabelIDByName(ctx, userID, t.Value)
	case FieldAccount:
		id, found, err = s.resolver.AccountIDByName(ctx, userID, t.Value)
	default:
		// A mailbox/thread/message id that is not a ULID cannot name anything.
		return None{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("search: resolve %s:%s: %w", t.Field, t.Value, err)
	}
	if !found {
		return None{}, nil
	}
	return IDTerm(t.Field, id), nil
}

// mentionsFlag reports whether the tree already constrains a flag atom, so the
// default "exclude trash" clause is not bolted onto a query that deliberately
// asked for trash.
func mentionsFlag(n Node, atom string) bool {
	switch v := n.(type) {
	case And:
		for _, c := range v.Nodes {
			if mentionsFlag(c, atom) {
				return true
			}
		}
	case Or:
		for _, c := range v.Nodes {
			if mentionsFlag(c, atom) {
				return true
			}
		}
	case Not:
		return mentionsFlag(v.Node, atom)
	case Term:
		return v.Field == FieldFlag && v.Value == atom
	}
	return false
}

// RequiresFlag reports whether every document the tree can match must carry the
// flag atom: the tree IS that flag's term, or an And with a child that requires
// it. A negated or OR'd mention does not count.
//
// It is stricter than mentionsFlag on purpose. Email/query uses it to decide
// that a search asked for trashed mail (#623); it then takes the flag out of the
// index's query (WithoutRequiredFlag) and restricts to deleted mail in Postgres
// instead. That swap is only sound when the query cannot also match something
// untrashed.
func RequiresFlag(n Node, atom string) bool {
	switch v := n.(type) {
	case And:
		for _, c := range v.Nodes {
			if RequiresFlag(c, atom) {
				return true
			}
		}
	case Term:
		return v.Field == FieldFlag && v.Value == atom
	}
	return false
}

// WithoutRequiredFlag returns the tree with every occurrence of the flag atom
// that RequiresFlag counts removed, for a caller that enforces the flag itself
// and must not have the index filter on it too. What is left is simplified: an
// And that loses all its children matches everything (All), and an And left with
// one child is that child.
func WithoutRequiredFlag(n Node, atom string) Node {
	switch v := n.(type) {
	case Term:
		if v.Field == FieldFlag && v.Value == atom {
			return All{}
		}
	case And:
		kept := make([]Node, 0, len(v.Nodes))
		for _, c := range v.Nodes {
			c = WithoutRequiredFlag(c, atom)
			if _, all := c.(All); all {
				continue
			}
			kept = append(kept, c)
		}
		switch len(kept) {
		case 0:
			return All{}
		case 1:
			return kept[0]
		}
		return And{Nodes: kept}
	}
	return n
}
