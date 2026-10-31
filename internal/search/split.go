// SPDX-License-Identifier: AGPL-3.0-only

package search

// COMPOSING STORE-SIDE PREDICATES WITH THE INDEX (#722 area:, #723 is:snoozed /
// is:muted / is:pinned / is:important).
//
// Those predicates are StoreTerms: the index cannot answer them (see StoreTerm
// for why each one would go stale in it). Email/query already splits every
// search into two halves — the index answers "which messages match this text?"
// as a candidate id set, and Postgres applies the structured filters, the sort,
// the total and the window over it (store.EmailQuery.RestrictIDs). Store-side
// operators join the POSTGRES half, as one more predicate in that same WHERE,
// rather than post-filtering the index's hits. The reasons, in order of weight:
//
//   - TRUTHFUL COUNTS. Postgres computes total and the page over the full
//     predicate, so `area:Work` alone is an ordinary filtered Email/query with
//     an exact total and correct paging — no index involved, no hit cap. A
//     post-filter over the index's capped hit list would report a total of
//     "the area's mail among the first N hits", which is a bound posing as a
//     count.
//   - ONE DEFINITION. The area predicate is store.AreaPredicate's resolver —
//     the very SQL `inArea`, Area/getSummary and the desk use — so a search and
//     the area view cannot disagree about which area a message is in. The same
//     holds for snoozed (Email/query's isSnoozed) and the line (onSurface).
//   - COST. An `area:`-only search over a whole mailbox costs what opening that
//     area's view costs; nothing is shipped through the index at all.
//
// SplitStore does the split. The top-level conjuncts free of store terms are
// the INDEX half (RestrictIDs, exactly as before). Each conjunct that contains a
// store term becomes a STORE tree: And/Or/Not over StoreTerms, in which every
// maximal store-free subtree is an IndexSet — a subtree the index evaluates on
// its own, whose hits the caller substitutes as an IDSet (`m.id = ANY(…)`).
// That is what lets `(area:Work OR from:boss@example.com) -is:snoozed` mean
// exactly what it says. An IDSet carries the index's hit cap; a caller must
// report a capped one as a bound, never as a total.

// ContainsStore reports whether the tree holds anything only the store can
// evaluate (a StoreTerm, or an IndexSet/IDSet produced from one).
func ContainsStore(n Node) bool {
	switch v := n.(type) {
	case StoreTerm, IndexSet, IDSet:
		return true
	case And:
		for _, c := range v.Nodes {
			if ContainsStore(c) {
				return true
			}
		}
	case Or:
		for _, c := range v.Nodes {
			if ContainsStore(c) {
				return true
			}
		}
	case Not:
		return ContainsStore(v.Node)
	}
	return false
}

// StorePlan is a query split for a caller that has the store as well as the
// index (Email/query).
type StorePlan struct {
	// Index is the conjunction of the store-free top-level clauses: All when
	// there are none (the index then has nothing to say).
	Index Node
	// Store is the conjunction of the clauses that need the store, with their
	// store-free subtrees wrapped as IndexSets; nil when there are none.
	Store Node
}

// SplitStore splits a parsed tree into the index half and the store half (see
// the file header). A tree with no StoreTerm comes back whole as Index.
func SplitStore(root Node) StorePlan {
	if root == nil {
		return StorePlan{Index: All{}}
	}
	if !ContainsStore(root) {
		return StorePlan{Index: root}
	}
	var index, store []Node
	for _, c := range flattenAnd(root) {
		if ContainsStore(c) {
			store = append(store, isolateIndex(c))
		} else {
			index = append(index, c)
		}
	}
	plan := StorePlan{Index: andOf(index), Store: andOf(store)}
	if plan.Index == nil {
		plan.Index = All{}
	}
	return plan
}

// flattenAnd returns the conjuncts of n, looking through nested Ands (a
// bracketed group of ANDed clauses is still just more conjuncts).
func flattenAnd(n Node) []Node {
	a, ok := n.(And)
	if !ok {
		return []Node{n}
	}
	var out []Node
	for _, c := range a.Nodes {
		out = append(out, flattenAnd(c)...)
	}
	return out
}

// isolateIndex rewrites a subtree that contains a store term so that its
// store-free parts become IndexSets. Store-free siblings under one And or Or
// are grouped into a single IndexSet, so each costs one index run, not one per
// clause.
func isolateIndex(n Node) Node {
	if !ContainsStore(n) {
		switch n.(type) {
		case All, None:
			return n // nothing for the index to compute
		}
		return IndexSet{Node: n}
	}
	switch v := n.(type) {
	case And:
		return And{Nodes: isolateChildren(v.Nodes, func(ns []Node) Node { return And{Nodes: ns} })}
	case Or:
		return Or{Nodes: isolateChildren(v.Nodes, func(ns []Node) Node { return Or{Nodes: ns} })}
	case Not:
		return Not{Node: isolateIndex(v.Node)}
	}
	return n
}

func isolateChildren(ns []Node, group func([]Node) Node) []Node {
	var free, out []Node
	for _, c := range ns {
		if ContainsStore(c) {
			out = append(out, isolateIndex(c))
		} else {
			free = append(free, c)
		}
	}
	switch len(free) {
	case 0:
	case 1:
		out = append(out, isolateIndex(free[0]))
	default:
		out = append(out, IndexSet{Node: group(free)})
	}
	return out
}

// WithoutStore removes the store-side predicates from a tree, for a caller that
// only needs what the index can say about it — highlighting (SearchSnippet/get).
// A clause that held only store terms disappears; a tree with nothing left is
// All. It is NOT a matching query: `-area:Work` and `area:Work` both reduce to
// All, which is right for "highlight the text terms" and wrong for anything
// else.
func WithoutStore(n Node) Node {
	out, ok := pruneStore(n)
	if !ok {
		return All{}
	}
	return out
}

func pruneStore(n Node) (Node, bool) {
	switch v := n.(type) {
	case StoreTerm, IndexSet, IDSet:
		return nil, false
	case And:
		kept := pruneAll(v.Nodes)
		if len(kept) == 0 {
			return nil, false
		}
		return andOf(kept), true
	case Or:
		kept := pruneAll(v.Nodes)
		switch len(kept) {
		case 0:
			return nil, false
		case 1:
			return kept[0], true
		}
		return Or{Nodes: kept}, true
	case Not:
		inner, ok := pruneStore(v.Node)
		if !ok {
			return nil, false
		}
		return Not{Node: inner}, true
	}
	return n, true
}

func pruneAll(ns []Node) []Node {
	var kept []Node
	for _, c := range ns {
		if p, ok := pruneStore(c); ok {
			kept = append(kept, p)
		}
	}
	return kept
}

// MentionsFlagPositively reports whether the flag atom's term appears anywhere
// in the tree outside a negation — `is:trashed` alone or inside an Or, but not
// `-is:trashed`. Email/query uses it to decide that a query has taken the
// trash/junk question into its own hands (#723): a query that names trash
// somewhere as something to INCLUDE must not have it excluded underneath it.
func MentionsFlagPositively(n Node, atom string) bool {
	switch v := n.(type) {
	case And:
		for _, c := range v.Nodes {
			if MentionsFlagPositively(c, atom) {
				return true
			}
		}
	case Or:
		for _, c := range v.Nodes {
			if MentionsFlagPositively(c, atom) {
				return true
			}
		}
	case Term:
		return v.Field == FieldFlag && v.Value == atom
	}
	return false
}
