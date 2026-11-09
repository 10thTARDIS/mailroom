// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// filterCond is one parsed FilterCondition (RFC 8621 4.4.1).
type filterCond struct {
	// q is the store query with this condition's fields set.
	q store.EmailQuery
	// text is its search-index half (text, body, and the `query`
	// extension); from/to/cc/subject are NOT in it (they are stored-column matches).
	text searchFilter
	// tab is a requested category tab, expanded later by the caller
	// (emailQueryPlan.requestedTab).
	tab tabs.Tab
	// stable is false when any key of the condition is not delta-stable.
	stable bool
}

// parseFilterCondition decodes ONE FilterCondition (a JSON object with no
// `operator`) onto base, which carries the query's own window. It is the single
// place a condition's keys are validated and mapped to the store query, for a
// flat filter and for every leaf of a FilterOperator tree alike.
func parseFilterCondition(rawFilter json.RawMessage, base store.EmailQuery) (filterCond, *MethodError) {
	c := filterCond{q: base, stable: true}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawFilter, &raw); err != nil {
		return c, &MethodError{Type: errInvalidArguments, Description: "filter must be an object"}
	}
	for k := range raw {
		if !emailFilterConditions[k] {
			return c, &MethodError{Type: errUnsupportedFilter, Description: "unsupported filter condition: " + k}
		}
		// A single search-backed or thread/account condition makes the whole
		// filter volatile: its delta cannot be reconstructed from the change log.
		if !deltaStableConditions[k] {
			c.stable = false
		}
	}

	var f emailFilter
	if err := json.Unmarshal(rawFilter, &f); err != nil {
		return c, &MethodError{Type: errInvalidArguments, Description: "unsupported filter"}
	}
	if err := json.Unmarshal(rawFilter, &c.text); err != nil {
		return c, &MethodError{Type: errInvalidArguments, Description: "unsupported filter"}
	}
	// from, to, cc and subject are answered from the stored columns (RFC substring
	// semantics), not by the search index: taking them out of the text half is
	// what makes them so.
	c.text.From, c.text.To, c.text.Cc, c.text.Subject = "", "", "", ""
	// A query clause whose syntax can match nothing (`from:*foo`) is the
	// caller's mistake, not an empty mailbox (#712).
	if mErr := c.text.validate(); mErr != nil {
		return c, mErr
	}
	q := &c.q
	if f.InMailbox != nil {
		id, err := ulid.Parse(*f.InMailbox)
		if err != nil {
			return c, &MethodError{Type: errInvalidArguments, Description: "inMailbox is not a valid id"}
		}
		q.InMailbox = &id
	}
	if len(f.InMailboxOtherThan) > store.MaxMailboxIDs {
		return c, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("inMailboxOtherThan holds more than %d ids", store.MaxMailboxIDs)}
	}
	if f.InMailboxOtherThan != nil {
		other := make([]ulid.ULID, 0, len(f.InMailboxOtherThan))
		for _, raw := range f.InMailboxOtherThan {
			id, err := ulid.Parse(raw)
			if err != nil {
				return c, &MethodError{Type: errInvalidArguments,
					Description: "inMailboxOtherThan holds an id that is not valid"}
			}
			other = append(other, id)
		}
		q.InMailboxOtherThan = other
	}
	if f.InThread != nil {
		id, err := ulid.Parse(*f.InThread)
		if err != nil {
			return c, &MethodError{Type: errInvalidArguments, Description: "inThread is not a valid id"}
		}
		q.InThread = &id
	}
	if f.InAccount != nil {
		id, err := ulid.Parse(*f.InAccount)
		if err != nil {
			return c, &MethodError{Type: errInvalidArguments, Description: "inAccount is not a valid id"}
		}
		q.InAccount = &id
	}
	if f.InTab != nil {
		// Validated here (an unknown tab is a filter error, not a silently
		// empty result) but EXPANDED by the caller, which has the user's
		// enabled set. See emailQueryPlan.requestedTab.
		t, ok := tabs.Parse(*f.InTab)
		if !ok {
			return c, &MethodError{Type: errInvalidArguments, Description: "inTab is not a category tab"}
		}
		c.tab = t
	}
	unsorted := f.Unsorted != nil && *f.Unsorted
	if f.InArea != nil {
		id, err := ulid.Parse(*f.InArea)
		if err != nil {
			return c, &MethodError{Type: errInvalidArguments, Description: "inArea is not an area id"}
		}
		if unsorted {
			return c, &MethodError{Type: errInvalidArguments, Description: "inArea and unsorted are mutually exclusive"}
		}
		// EXISTENCE is checked by the caller (emailQueryWith calling
		// checkAreaExists), which has store access; this parse stays pure.
		// An area that does not exist must be invalidArguments, not a
		// silently empty result — the same reasoning expandTabFilter gives
		// for a disabled tab.
		q.InArea = &id
	} else if unsorted {
		q.Unsorted = true
	}
	if f.Before != "" {
		t, ok := jmapTime(f.Before)
		if !ok {
			return c, &MethodError{Type: errInvalidArguments, Description: "before is not a valid UTCDate"}
		}
		q.Before = &t
	}
	if f.After != "" {
		t, ok := jmapTime(f.After)
		if !ok {
			return c, &MethodError{Type: errInvalidArguments, Description: "after is not a valid UTCDate"}
		}
		q.After = &t
	}
	q.Snoozed = f.IsSnoozed
	q.Important = f.IsImportant
	q.NeedsCategory = f.NeedsCategory
	q.HasKeyword = f.HasKeyword
	q.NotKeyword = f.NotKeyword
	q.OnDesk = f.OnDesk
	q.OnSurface = f.OnSurface
	q.DeskArchived = f.DeskArchived
	q.FromContains = f.From
	q.ToContains, q.CcContains, q.BccContains = f.To, f.Cc, f.Bcc
	if _, ok := raw["header"]; ok {
		// RFC 8621 4.4.1: [name] or [name, text]. Only List-Id is stored (ADR-055);
		// every other name is a condition the server cannot answer, not an empty set.
		if len(f.Header) < 1 || len(f.Header) > 2 {
			return c, &MethodError{Type: errInvalidArguments, Description: "header must hold a name and optionally a text"}
		}
		if !strings.EqualFold(strings.TrimSpace(f.Header[0]), "list-id") {
			return c, &MethodError{Type: errUnsupportedFilter,
				Description: "unsupported filter condition: header is supported for List-Id only"}
		}
		lc := &store.ListIDCondition{}
		if len(f.Header) == 2 {
			lc.Contains = f.Header[1]
		}
		q.ListIDHeader = lc
	}
	q.SubjectContains = f.Subject
	// Substring conditions are bounded (security review of beta 6): ILIKE
	// lowercases the pattern on every row, so a 1 MiB pattern cost ~1 MiB of work
	// per message. Nothing stored is longer than a 254-character address or a
	// 128-character name, so a longer pattern can match nothing anyway.
	for name, v := range map[string]string{"from": f.From, "to": f.To, "cc": f.Cc, "bcc": f.Bcc, "subject": f.Subject} {
		if utf8.RuneCountInString(v) > maxFilterTextRunes {
			return c, &MethodError{Type: errInvalidArguments, Description: name + " is longer than " + strconv.Itoa(maxFilterTextRunes) + " characters"}
		}
	}
	if q.ListIDHeader != nil && utf8.RuneCountInString(q.ListIDHeader.Contains) > maxFilterTextRunes {
		return c, &MethodError{Type: errInvalidArguments, Description: "header text is longer than " + strconv.Itoa(maxFilterTextRunes) + " characters"}
	}
	q.HasAttachment = f.HasAttachment
	for name, p := range map[string]struct {
		in  *int64
		out **int64
	}{"minSize": {f.MinSize, &q.MinSize}, "maxSize": {f.MaxSize, &q.MaxSize}} {
		if p.in != nil && *p.in < 0 {
			return c, &MethodError{Type: errInvalidArguments, Description: name + " must not be negative"}
		}
		*p.out = p.in
	}
	for name, kw := range map[string]string{
		"someInThreadHaveKeyword": f.SomeInThreadHaveKeyword,
		"noneInThreadHaveKeyword": f.NoneInThreadHaveKeyword,
		"allInThreadHaveKeyword":  f.AllInThreadHaveKeyword,
	} {
		if kw != "" && !validJMAPKeyword(kw) {
			return c, &MethodError{Type: errInvalidArguments, Description: name + " is not a valid keyword"}
		}
	}
	q.SomeInThreadHaveKeyword = f.SomeInThreadHaveKeyword
	q.NoneInThreadHaveKeyword = f.NoneInThreadHaveKeyword
	q.AllInThreadHaveKeyword = f.AllInThreadHaveKeyword
	return c, nil
}

// splitRootOnly separates a parsed condition into its STANDARD half (what a
// FilterOperator leaf may carry) and the half that only the query's root can
// apply: the Mailroom extension conditions, whose compilation needs the caller's
// per-request preparation (the tab fold, the area check, the desk line) or is a
// property of the whole query (the area/desk views).
func splitRootOnly(q store.EmailQuery) (std, root store.EmailQuery) {
	root.Snoozed, root.Important, root.NeedsCategory = q.Snoozed, q.Important, q.NeedsCategory
	root.OnDesk, root.OnSurface, root.DeskArchived = q.OnDesk, q.OnSurface, q.DeskArchived
	root.InArea, root.Unsorted = q.InArea, q.Unsorted
	q.Snoozed, q.Important, q.NeedsCategory = nil, nil, nil
	q.OnDesk, q.OnSurface, q.DeskArchived = nil, nil, nil
	q.InArea, q.Unsorted = nil, false
	return q, root
}

// rootAcc collects the root-only conditions (and the search-index half) found
// in the CONJUNCTIVE leaves of a tree, i.e. those reached from the root through
// AND operators only. Each may appear at most once: two leaves each asking for a
// different `text` have no single search to run.
type rootAcc struct {
	q    store.EmailQuery
	text searchFilter
	tab  tabs.Tab
}

// isEmpty reports whether the condition carries nothing only the root can apply.
func rootEmpty(root store.EmailQuery, text searchFilter, tab tabs.Tab) bool {
	return root.Snoozed == nil && root.Important == nil && root.NeedsCategory == nil &&
		root.OnDesk == nil && root.OnSurface == nil && root.DeskArchived == nil &&
		root.InArea == nil && !root.Unsorted && text == (searchFilter{}) && tab == ""
}

// add folds one leaf's root-only half into the accumulator.
func (a *rootAcc) add(root store.EmailQuery, text searchFilter, tab tabs.Tab) *MethodError {
	dup := func(name string, have bool) *MethodError {
		if have {
			return &MethodError{Type: errUnsupportedFilter,
				Description: name + " may appear only once in a filter with operators"}
		}
		return nil
	}
	for _, c := range []struct {
		name string
		new  bool
		old  bool
	}{
		{"isSnoozed", root.Snoozed != nil, a.q.Snoozed != nil},
		{"isImportant", root.Important != nil, a.q.Important != nil},
		{"needsCategory", root.NeedsCategory != nil, a.q.NeedsCategory != nil},
		{"onDesk", root.OnDesk != nil, a.q.OnDesk != nil},
		{"onSurface", root.OnSurface != nil, a.q.OnSurface != nil},
		{"deskArchived", root.DeskArchived != nil, a.q.DeskArchived != nil},
		{"inArea", root.InArea != nil, a.q.InArea != nil},
		{"unsorted", root.Unsorted, a.q.Unsorted},
		{"inTab", tab != "", a.tab != ""},
		{"a text condition", text != (searchFilter{}), a.text != (searchFilter{})},
	} {
		if c.new {
			if mErr := dup(c.name, c.old); mErr != nil {
				return mErr
			}
		}
	}
	if root.Snoozed != nil {
		a.q.Snoozed = root.Snoozed
	}
	if root.Important != nil {
		a.q.Important = root.Important
	}
	if root.NeedsCategory != nil {
		a.q.NeedsCategory = root.NeedsCategory
	}
	if root.OnDesk != nil {
		a.q.OnDesk = root.OnDesk
	}
	if root.OnSurface != nil {
		a.q.OnSurface = root.OnSurface
	}
	if root.DeskArchived != nil {
		a.q.DeskArchived = root.DeskArchived
	}
	if root.InArea != nil {
		a.q.InArea = root.InArea
	}
	if root.Unsorted {
		a.q.Unsorted = true
	}
	if tab != "" {
		a.tab = tab
	}
	if text != (searchFilter{}) {
		a.text = text
	}
	return nil
}

// filterTree parses a FilterOperator tree (RFC 8620 5.5) into the store's
// EmailFilterNode, enforcing the bounds and what may sit under OR and NOT.
//
// WHAT MAY SIT WHERE. Every leaf may carry the standard RFC 8621 conditions
// (inMailbox, hasKeyword, from, subject, sizes, dates, thread keywords, …); the
// store compiles them identically at every depth. The search-index conditions
// (text, body, query) and the Mailroom extensions (inTab, inArea,
// onDesk, …) cannot be compiled inside a boolean expression: the index answers
// "which messages match this text" as a candidate list for the whole query, and
// the extensions need per-request preparation. So they are accepted only in a
// leaf reached from the root through AND operators alone, where "ANDed with the
// rest" is exactly what they mean and they are hoisted to the root; under an OR
// or a NOT they are refused with unsupportedFilter, never approximated. A filter
// with two such leaves for the same condition is refused too.
type filterTree struct {
	nodes          int
	threadKeywords int
	root           rootAcc
	stable         bool
}

func (t *filterTree) parse(raw json.RawMessage, depth int, conjunctive bool) (store.EmailFilterNode, *MethodError) {
	t.nodes++
	if depth > store.MaxFilterDepth {
		return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("the filter is nested more than %d operators deep", store.MaxFilterDepth)}
	}
	if t.nodes > store.MaxFilterNodes {
		return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("the filter has more than %d conditions and operators", store.MaxFilterNodes)}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments, Description: "a filter must be an object"}
	}
	if opRaw, isOp := obj["operator"]; isOp {
		for k := range obj {
			if k != "operator" && k != "conditions" {
				return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
					Description: "a FilterOperator has only operator and conditions, not " + k}
			}
		}
		var op string
		if err := json.Unmarshal(opRaw, &op); err != nil || (op != store.FilterAND && op != store.FilterOR && op != store.FilterNOT) {
			return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
				Description: "operator must be AND, OR or NOT"}
		}
		var kids []json.RawMessage
		if err := json.Unmarshal(obj["conditions"], &kids); err != nil || kids == nil {
			return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
				Description: "conditions must be an array"}
		}
		// Bound the work before descending: a wide array is refused here, not after
		// parsing every child.
		if t.nodes+len(kids) > store.MaxFilterNodes {
			return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
				Description: fmt.Sprintf("the filter has more than %d conditions and operators", store.MaxFilterNodes)}
		}
		node := store.EmailFilterNode{Operator: op, Conditions: make([]store.EmailFilterNode, 0, len(kids))}
		for _, k := range kids {
			child, mErr := t.parse(k, depth+1, conjunctive && op == store.FilterAND)
			if mErr != nil {
				return store.EmailFilterNode{}, mErr
			}
			node.Conditions = append(node.Conditions, child)
		}
		return node, nil
	}

	c, mErr := parseFilterCondition(raw, store.EmailQuery{})
	if mErr != nil {
		return store.EmailFilterNode{}, mErr
	}
	if !c.stable {
		t.stable = false
	}
	std, root := splitRootOnly(c.q)
	// A leaf costs one node per condition it carries (the node itself was counted
	// on entry), the same count the store compiler applies.
	t.nodes += max(1, std.ConditionCount()) - 1
	if t.nodes > store.MaxFilterNodes {
		return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("the filter has more than %d conditions and operators", store.MaxFilterNodes)}
	}
	t.threadKeywords += std.ThreadKeywordCount()
	if t.threadKeywords > store.MaxThreadKeywordConds {
		return store.EmailFilterNode{}, &MethodError{Type: errInvalidArguments,
			Description: fmt.Sprintf("the filter has more than %d thread-keyword conditions", store.MaxThreadKeywordConds)}
	}
	if !rootEmpty(root, c.text, c.tab) {
		if !conjunctive {
			return store.EmailFilterNode{}, &MethodError{Type: errUnsupportedFilter,
				Description: "text and Mailroom conditions cannot be combined under OR or NOT"}
		}
		if mErr := t.root.add(root, c.text, c.tab); mErr != nil {
			return store.EmailFilterNode{}, mErr
		}
	}
	return store.EmailFilterNode{Leaf: std}, nil
}

// maxFilterTextRunes bounds every substring filter condition (from, to, cc, bcc,
// subject, the List-Id header text): longer than anything stored.
const maxFilterTextRunes = 512
