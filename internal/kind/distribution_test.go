// SPDX-License-Identifier: AGPL-3.0-only

package kind

import (
	"fmt"
	"sort"
	"testing"
)

// syntheticCorpus is a small, hand-built stand-in for mail of each shape, written
// with the facts a message carries at INGEST (no HasCorresponded: a rule's kind is
// judged from the projected headers alone, the same facts the category tabs use).
// All addresses are synthetic.
//
// It exists to answer one question before a rule can be bound to a kind: does
// Classify spread over the kinds, or is it one value for nearly everything (which
// would make a "only for promotional mail" rule either always or never true)?
func syntheticCorpus() map[string][]Facts {
	mk := func(i int, f Facts) Facts {
		f.FromAddress = fmt.Sprintf(f.FromAddress, i)
		return f
	}
	out := map[string][]Facts{}
	add := func(name string, n int, f Facts) {
		for i := 0; i < n; i++ {
			out[name] = append(out[name], mk(i, f))
		}
	}
	add("marketing via ESP", 5, Facts{
		FromAddress: "deals%d@shop.example", FromDomain: "shop.example",
		Subject:         "40% off everything: last chance",
		ListUnsubscribe: "<https://shop.example/unsub>", ListUnsubscribePost: "List-Unsubscribe=One-Click",
		DKIMDomains: []string{"shop.example", "mail.klaviyomail.com"},
		ReturnPath:  "<bounce-8f2a91c4de77@mail.klaviyomail.com>", RecipientCount: 1,
	})
	add("order receipt", 5, Facts{
		FromAddress: "orders%d@shop.example", FromDomain: "shop.example",
		Subject: "Your order #55012 has shipped", AutoSubmitted: "auto-generated",
		DKIMDomains: []string{"shop.example"}, ReturnPath: "<orders@shop.example>", RecipientCount: 1,
	})
	add("mailing list", 5, Facts{
		FromAddress: "member%d@lists.example.org", FromDomain: "lists.example.org",
		Subject: "Re: the patch series", ListID: "<dev.lists.example.org>",
		ListUnsubscribe: "<mailto:dev-unsubscribe@lists.example.org>", IsReply: true, RecipientCount: 1,
	})
	add("bounce or cron", 5, Facts{
		FromAddress: "mailer-daemon%d@mail.example.net", FromDomain: "mail.example.net",
		Subject: "Delivery Status Notification (Failure)", AutoSubmitted: "auto-replied",
		Precedence: "bulk", RecipientCount: 1,
	})
	add("service notification", 5, Facts{
		FromAddress: "notify%d@app.example", FromDomain: "app.example",
		Subject: "Someone commented on your document", AutoSubmitted: "auto-generated",
		FeedbackID: "1:app:notifications", DKIMDomains: []string{"app.example"}, RecipientCount: 1,
	})
	add("reply from a person", 5, Facts{
		FromAddress: "sam%d@example.com", FromDomain: "example.com",
		Subject: "Re: lunch tomorrow?", IsReply: true, RecipientCount: 1,
	})
	add("bare first message", 5, Facts{
		FromAddress: "someone%d@example.net", FromDomain: "example.net",
		Subject: "hello", RecipientCount: 1,
	})
	return out
}

// TestKindDistributionOverSyntheticMail is the measurement that comes before the
// rule-kind binding (CLAUDE.md: report per-case counts, never one total). It
// asserts the spread is real, not a particular mapping, and logs the table so the
// numbers are in the test output.
func TestKindDistributionOverSyntheticMail(t *testing.T) {
	corpus := syntheticCorpus()
	total := map[Kind]int{}
	var names []string
	for n := range corpus {
		names = append(names, n)
	}
	sort.Strings(names)
	n := 0
	for _, name := range names {
		per := map[Kind]int{}
		for _, f := range corpus[name] {
			k := Classify(f).Kind
			per[k]++
			total[k]++
			n++
		}
		t.Logf("%-22s %v", name, per)
	}
	t.Logf("TOTAL %d messages: %v", n, total)

	distinct := 0
	for _, c := range total {
		if c > 0 {
			distinct++
		}
	}
	if distinct < 4 {
		t.Fatalf("Classify returned only %d distinct kinds over %d synthetic messages (%v): a rule bound to a kind would be all-or-nothing", distinct, n, total)
	}
	for k, c := range total {
		if c == n {
			t.Fatalf("every message classified %q", k)
		}
		if !k.Valid() {
			t.Fatalf("Classify returned %q, which is not in Kinds()", k)
		}
	}
}
