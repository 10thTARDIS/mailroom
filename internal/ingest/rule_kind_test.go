// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

func kindBoundRule(name, kind string) rules.Rule {
	return rules.Rule{
		ID: ulid.Make(), Name: name, Enabled: true, Kind: kind,
		Conditions: rules.Condition{Field: rules.FieldFromDomain, Op: rules.OpEquals, Value: "shop.example"},
		Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: name}},
	}
}

// A promotional message from the shop (marketing infrastructure + one-click
// unsubscribe) and a receipt from the same shop differ only in kind.
var (
	promoProj = projection{
		FromAddress: "deals@shop.example", Subject: "40% off everything: last chance",
		ListUnsubscribe: "<https://shop.example/unsub>", ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains: []string{"shop.example", "mail.klaviyomail.com"},
		ReturnPath:  "<bounce-8f2a91c4de77@mail.klaviyomail.com>",
	}
	receiptProj = projection{
		FromAddress: "orders@shop.example", Subject: "Your order #55012 has shipped",
		AutoSubmitted: "auto-generated", DKIMDomains: []string{"shop.example"},
		ReturnPath: "<orders@shop.example>",
	}
)

func labels(as []rules.AttributedAction) map[string]bool {
	out := map[string]bool{}
	for _, a := range as {
		out[a.Label] = true
	}
	return out
}

// A rule bound to a kind applies to that kind at ingest and not to another kind
// from the same sender; an unbound rule applies to both.
func TestIngestAppliesAKindBoundRuleOnlyToItsKind(t *testing.T) {
	rs := []rules.Rule{
		kindBoundRule("promo-only", "promotional"),
		kindBoundRule("receipt-only", "transactional"),
		kindBoundRule("any", ""),
	}
	got := labels(evaluateRules(rs, promoProj, nil, ""))
	if !got["promo-only"] || got["receipt-only"] || !got["any"] {
		t.Errorf("promotional message: labels %v, want promo-only and any only", got)
	}
	got = labels(evaluateRules(rs, receiptProj, nil, ""))
	if got["promo-only"] || !got["receipt-only"] || !got["any"] {
		t.Errorf("receipt: labels %v, want receipt-only and any only", got)
	}
}

// kind.Classify runs ONCE per message however many kind-bound rules there are,
// and not at all when no enabled rule is bound.
func TestIngestClassifiesOncePerMessageNotOncePerRule(t *testing.T) {
	bound := []rules.Rule{
		kindBoundRule("a", "promotional"), kindBoundRule("b", "promotional"),
		kindBoundRule("c", "transactional"), kindBoundRule("d", "list"),
	}
	before := classifyCount.Load()
	evaluateRules(bound, promoProj, nil, "")
	if n := classifyCount.Load() - before; n != 1 {
		t.Errorf("4 kind-bound rules caused %d classifications for one message, want 1", n)
	}

	before = classifyCount.Load()
	evaluateRules([]rules.Rule{kindBoundRule("x", ""), kindBoundRule("y", "")}, promoProj, nil, "")
	if n := classifyCount.Load() - before; n != 0 {
		t.Errorf("unbound rules caused %d classifications, want 0", n)
	}
}
