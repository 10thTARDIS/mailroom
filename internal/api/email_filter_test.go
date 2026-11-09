// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/store"
)

func parseFilter(t *testing.T, filter string, sort string) (emailQueryPlan, *MethodError) {
	t.Helper()
	args := emailQueryArgs{Filter: json.RawMessage(filter)}
	if sort != "" {
		if err := json.Unmarshal([]byte(sort), &args.Sort); err != nil {
			t.Fatal(err)
		}
	}
	return parseEmailQuery(args)
}

func nested(depth int) string {
	s := `{"inThread":"0000000000000000000000000A"}`
	for i := 1; i < depth; i++ {
		s = `{"operator":"AND","conditions":[` + s + `]}`
	}
	return s
}

func TestFilterOperatorBounds(t *testing.T) {
	// The leaf is at depth `depth`: exactly the bound parses, one more does not.
	if _, mErr := parseFilter(t, nested(store.MaxFilterDepth), ""); mErr != nil {
		t.Fatalf("a tree at the depth bound was refused: %v", mErr)
	}
	if _, mErr := parseFilter(t, nested(store.MaxFilterDepth+1), ""); mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("a tree past the depth bound: %v, want invalidArguments", mErr)
	}
	leaves := func(n int) string {
		return `{"operator":"OR","conditions":[` + strings.TrimSuffix(strings.Repeat(`{"hasKeyword":"$seen"},`, n), ",") + `]}`
	}
	if _, mErr := parseFilter(t, leaves(store.MaxFilterNodes-1), ""); mErr != nil {
		t.Fatalf("a tree at the size bound was refused: %v", mErr)
	}
	if _, mErr := parseFilter(t, leaves(store.MaxFilterNodes), ""); mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("a tree past the size bound: %v, want invalidArguments", mErr)
	}
}

func TestFilterOperatorShapeErrors(t *testing.T) {
	for name, c := range map[string]struct{ filter, want string }{
		"unknown operator":            {`{"operator":"XOR","conditions":[]}`, errInvalidArguments},
		"no conditions":               {`{"operator":"AND"}`, errInvalidArguments},
		"conditions not array":        {`{"operator":"AND","conditions":{}}`, errInvalidArguments},
		"stray key":                   {`{"operator":"AND","conditions":[],"inMailbox":"x"}`, errInvalidArguments},
		"unknown inside":              {`{"operator":"AND","conditions":[{"bogus":1}]}`, errUnsupportedFilter},
		"other header is unsupported": {`{"operator":"OR","conditions":[{"header":["X-Mailer"]}]}`, errUnsupportedFilter},
		"header needs a name":         {`{"operator":"OR","conditions":[{"header":[]}]}`, errInvalidArguments},
		"header takes two strings":    {`{"operator":"OR","conditions":[{"header":["List-Id","a","b"]}]}`, errInvalidArguments},
		"text under OR":               {`{"operator":"OR","conditions":[{"text":"x"},{"hasKeyword":"$seen"}]}`, errUnsupportedFilter},
		"text under NOT":              {`{"operator":"NOT","conditions":[{"body":"x"}]}`, errUnsupportedFilter},
		"text under AND under OR": {`{"operator":"OR","conditions":[{"operator":"AND","conditions":[{"body":"x"}]}]}`,
			errUnsupportedFilter},
		"extension under NOT": {`{"operator":"NOT","conditions":[{"onDesk":true}]}`, errUnsupportedFilter},
		"two texts":           {`{"operator":"AND","conditions":[{"text":"a"},{"text":"b"}]}`, errUnsupportedFilter},
	} {
		if _, mErr := parseFilter(t, c.filter, ""); mErr == nil || mErr.Type != c.want {
			t.Errorf("%s: %v, want %s", name, mErr, c.want)
		}
	}
	// Text in conjunctive position is hoisted and works.
	p, mErr := parseFilter(t, `{"operator":"AND","conditions":[{"text":"invoice"},{"operator":"NOT","conditions":[{"hasKeyword":"$seen"}]}]}`, "")
	if mErr != nil || p.text.Text != "invoice" || p.query.Tree == nil {
		t.Fatalf("AND with text: %+v %v", p, mErr)
	}
	if p.stable {
		t.Error("a tree with a text condition must not be delta-stable")
	}
}

// Delta stability of a tree: stable only if EVERY leaf is, wherever it sits.
// NOT does not turn an unstable leaf stable (its complement changes exactly when
// the leaf does), and does not make a stable one unstable: a message enters or
// leaves NOT(c) exactly when it enters or leaves c, and for a stable c that
// always appends that message's own Email change row.
func TestFilterOperatorStability(t *testing.T) {
	mb := `"inMailbox":"0000000000000000000000000A"`
	for name, c := range map[string]struct {
		filter string
		stable bool
	}{
		"flat stable":                        {`{` + mb + `}`, true},
		"AND stable":                         {`{"operator":"AND","conditions":[{` + mb + `},{"hasKeyword":"$seen"}]}`, true},
		"NOT stable":                         {`{"operator":"NOT","conditions":[{` + mb + `}]}`, true},
		"OR NOT nested stable":               {`{"operator":"OR","conditions":[{"operator":"NOT","conditions":[{` + mb + `}]},{"minSize":5}]}`, true},
		"from/subject are columns":           {`{"operator":"NOT","conditions":[{"from":"a"},{"subject":"b"}]}`, true},
		"thread keyword unstable":            {`{"someInThreadHaveKeyword":"$seen"}`, false},
		"NOT of unstable":                    {`{"operator":"NOT","conditions":[{"noneInThreadHaveKeyword":"$seen"}]}`, false},
		"unstable deep in OR":                {`{"operator":"OR","conditions":[{` + mb + `},{"operator":"AND","conditions":[{"allInThreadHaveKeyword":"$seen"}]}]}`, false},
		"recipients and List-Id are columns": {`{"operator":"NOT","conditions":[{"to":"a"},{"cc":"b"},{"bcc":"c"},{"header":["list-id","d"]}]}`, true},
		"text still goes to the index":       {`{"operator":"AND","conditions":[{"text":"a"},{` + mb + `}]}`, false},
		"inAccount":                          {`{"operator":"NOT","conditions":[{"inAccount":"0000000000000000000000000A"}]}`, false},
	} {
		p, mErr := parseFilter(t, c.filter, "")
		if mErr != nil {
			t.Errorf("%s: %v", name, mErr)
			continue
		}
		if p.stable != c.stable {
			t.Errorf("%s: stable = %v, want %v", name, p.stable, c.stable)
		}
	}
}

func TestEmailQuerySortParsing(t *testing.T) {
	for _, prop := range []string{"size", "from", "to", "subject"} {
		p, mErr := parseFilter(t, "", fmt.Sprintf(`[{"property":%q,"isAscending":true}]`, prop))
		if mErr != nil || len(p.query.SortKeys) != 1 || !p.query.SortKeys[0].Ascending || !p.stable {
			t.Errorf("%s: %+v %v", prop, p.query.SortKeys, mErr)
		}
	}
	if _, mErr := parseFilter(t, "", `[{"property":"hasKeyword"}]`); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("hasKeyword without a keyword: %v", mErr)
	}
	if _, mErr := parseFilter(t, "", `[{"property":"from","collation":"i;nope"}]`); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("unknown collation: %v", mErr)
	}
	// `to` sorts on the stored key (ADR-055), with both collations.
	if p, mErr := parseFilter(t, "", `[{"property":"to","collation":"i;octet"}]`); mErr != nil ||
		len(p.query.SortKeys) != 1 || p.query.SortKeys[0].Kind != store.SortKeyTo || !p.query.SortKeys[0].Octet {
		t.Errorf("sort by to (octet): %+v %v", p.query.SortKeys, mErr)
	}
	// A new key does not steal the date key's direction: [from asc, receivedAt desc]
	p, _ := parseFilter(t, "", `[{"property":"from","isAscending":true},{"property":"receivedAt","isAscending":false}]`)
	if p.query.SortAscending || len(p.query.SortKeys) != 2 || p.query.SortKeys[1].Kind != store.SortKeyDate {
		t.Errorf("mixed sort: %+v asc=%v", p.query.SortKeys, p.query.SortAscending)
	}
	// Legacy sorts keep the historical path (no SortKeys).
	if p, _ := parseFilter(t, "", `[{"property":"receivedAt","isAscending":true}]`); p.query.SortKeys != nil || !p.query.SortAscending {
		t.Errorf("legacy sort: %+v", p.query)
	}
}

func TestMailboxFilterParsing(t *testing.T) {
	inbox := store.MailboxView{Name: "Inbox", Role: "inbox"}
	plain := store.MailboxView{Name: "Receipts 2026"}
	keep, mErr := parseMailboxFilter(json.RawMessage(`{"role":"inbox"}`))
	if mErr != nil || !keep(inbox) || keep(plain) {
		t.Errorf("role: %v", mErr)
	}
	keep, _ = parseMailboxFilter(json.RawMessage(`{"role":null}`))
	if keep(inbox) || !keep(plain) {
		t.Error("role null")
	}
	keep, _ = parseMailboxFilter(json.RawMessage(`{"name":"RECEIPT","hasAnyRole":false,"isSubscribed":true,"parentId":null}`))
	if !keep(plain) || keep(inbox) {
		t.Error("combined")
	}
	keep, _ = parseMailboxFilter(json.RawMessage(`{"parentId":"0000000000000000000000000A"}`))
	if keep(plain) {
		t.Error("parentId of a real id must match nothing (every mailbox is top level)")
	}
	if _, mErr := parseMailboxFilter(json.RawMessage(`{"sortOrder":1}`)); mErr == nil || mErr.Type != errUnsupportedFilter {
		t.Errorf("unknown condition: %v", mErr)
	}
	if _, mErr := parseMailboxFilter(json.RawMessage(`{"operator":"AND","conditions":[]}`)); mErr == nil || mErr.Type != errUnsupportedFilter {
		t.Errorf("operator on Mailbox/query: %v", mErr)
	}
}

func TestFilterLeafConditionsCountTowardTheNodeBound(t *testing.T) {
	leaf := `{"hasAttachment":true,"minSize":0,"maxSize":100}` // three conditions
	or := func(n int) string {
		return `{"operator":"OR","conditions":[` + strings.TrimSuffix(strings.Repeat(leaf+",", n), ",") + `]}`
	}
	// 21 x 3 + the operator = 64: at the bound.
	if _, mErr := parseFilter(t, or(21), ""); mErr != nil {
		t.Fatalf("a tree at the bound: %v", mErr)
	}
	// 22 objects, 67 conditions.
	if _, mErr := parseFilter(t, or(22), ""); mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("a tree past the bound by conditions: %v, want invalidArguments", mErr)
	}
}

func TestFilterThreadKeywordConditionsAreCapped(t *testing.T) {
	three := `{"someInThreadHaveKeyword":"a","noneInThreadHaveKeyword":"b","allInThreadHaveKeyword":"c"}`
	one := `{"someInThreadHaveKeyword":"a"}`
	if _, mErr := parseFilter(t, `{"operator":"OR","conditions":[`+three+`,`+one+`]}`, ""); mErr != nil {
		t.Fatalf("at the cap: %v", mErr)
	}
	if _, mErr := parseFilter(t, `{"operator":"OR","conditions":[`+three+`,`+one+`,`+one+`]}`, ""); mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("past the cap: %v, want invalidArguments", mErr)
	}
}

func TestFilterMailboxListsAreCapped(t *testing.T) {
	ids := func(n int) string {
		return `["` + strings.TrimSuffix(strings.Repeat(`0000000000000000000000000A","`, n), `,"`) + `]`
	}
	for _, shape := range []string{`{"inMailboxOtherThan":%s}`, `{"operator":"OR","conditions":[{"inMailboxOtherThan":%s}]}`} {
		ok := strings.Replace(shape, "%s", ids(store.MaxMailboxIDs), 1)
		if _, mErr := parseFilter(t, ok, ""); mErr != nil {
			t.Fatalf("a list at the cap (%s): %v", shape, mErr)
		}
		over := strings.Replace(shape, "%s", ids(store.MaxMailboxIDs+1), 1)
		if _, mErr := parseFilter(t, over, ""); mErr == nil || mErr.Type != errInvalidArguments {
			t.Fatalf("a list past the cap (%s): %v, want invalidArguments", shape, mErr)
		}
	}
}

func TestEmailQueryWindowOffsetsAreBounded(t *testing.T) {
	for name, args := range map[string]emailQueryArgs{
		"anchorOffset high": {AnchorOffset: maxWindowOffset + 1},
		"anchorOffset low":  {AnchorOffset: -maxWindowOffset - 1},
		"position high":     {Position: maxWindowOffset + 1},
		"position low":      {Position: -maxWindowOffset - 1},
	} {
		if _, mErr := parseEmailQuery(args); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("%s: %v, want invalidArguments", name, mErr)
		}
	}
	for name, args := range map[string]emailQueryArgs{
		"anchorOffset at the bound": {AnchorOffset: -maxWindowOffset},
		"position at the bound":     {Position: maxWindowOffset},
	} {
		if _, mErr := parseEmailQuery(args); mErr != nil {
			t.Errorf("%s: %v", name, mErr)
		}
	}
}
