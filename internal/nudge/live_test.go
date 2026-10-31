// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// ONE RULE IMPLEMENTATION (#58 phase 2): the report's shipped variants ARE
// the functions the live nudge calls, not restatements of them. If someone
// edits the report's rule table to "match" a changed live rule by copying it,
// this goes red.
func TestShippedRulesAreTheLiveFunctions(t *testing.T) {
	ptr := func(f any) uintptr { return reflect.ValueOf(f).Pointer() }
	found := 0
	for _, r := range inboundRules {
		if r.name == ShippedInboundRule {
			found++
			if ptr(r.pred) != ptr(WaitingOnYou) {
				t.Fatalf("report variant %q is not WaitingOnYou", r.name)
			}
		}
	}
	for _, r := range outboundRules {
		if r.name == ShippedOutboundRule {
			found++
			if ptr(r.pred) != ptr(NoOneAnswered) {
				t.Fatalf("report variant %q is not NoOneAnswered", r.name)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d shipped variants in the report tables, want 2", found)
	}
}

func dueIDs(ns []Nudge) string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.MessageID+":"+string(n.Kind))
	}
	return fmt.Sprint(out)
}

func liveCfg() LiveConfig {
	return LiveConfig{Now: now, Outbound: true, Inbound: true, InboundAfter: 3 * 24 * time.Hour}
}

func TestDueOutbound(t *testing.T) {
	waiting := out("S01", "T1", 10, "alice@example.org")
	answered := out("S02", "T2", 10, "bob@example.org")
	ans := in("R02", "T2", 9, "bob@example.org", []string{"me@example.com"}, nil)
	young := out("S03", "T3", 5, "carol@example.org")
	old := out("S04", "T4", 31, "dave@example.org")
	followed := out("S05", "T5", 12, "erin@example.org")
	followUp := out("S06", "T5", 11, "erin@example.org") // the user's own follow-up: S06 is the one due
	auto := out("S07", "T7", 10, "frank@example.org")
	vacation := in("R07", "T7", 9.9, "frank@example.org", []string{"me@example.com"}, nil)
	vacation.Facts.AutoSubmitted = "auto-replied"
	toList := out("S08", "T8", 10, "noreply@shop.example.com")
	irt := out("S09", "T9", 10, "greg@example.org")
	ansElsewhere := in("R09", "T99", 9, "greg@example.org", []string{"me@example.com"}, nil)
	ansElsewhere.InReplyTo = irt.MessageID // threading missed it; In-Reply-To did not

	// Outbound only: the answers here are themselves inbound mail a person
	// sent the user, which the (opt-in) inbound rule would rightly nudge.
	outOnly := liveCfg()
	outOnly.Inbound = false
	got := Due([]Message{waiting, answered, ans, young, old, followed, followUp, auto, vacation, toList, irt, ansElsewhere},
		me, outOnly, nil)
	// Oldest first: S06 (11d) before S01 and S07 (10d; ties keep id order).
	if want := "[S06:outbound S01:outbound S07:outbound]"; dueIDs(got) != want {
		t.Fatalf("due = %s, want %s", dueIDs(got), want)
	}
	if got[1].DueAt != waiting.At.Add(OutboundAfter) || got[1].Since != waiting.At {
		t.Fatalf("times = %+v", got[1])
	}

	// Context the store read only for conversations or history (a trashed or
	// snoozed sent message, say) is never itself nudged.
	ctx := out("S10", "T10", 10, "ivy@example.org")
	ctx.ContextOnly = true
	if got := Due([]Message{ctx}, me, outOnly, nil); len(got) != 0 {
		t.Fatalf("a context-only message was nudged: %s", dueIDs(got))
	}

	if got := Due([]Message{waiting}, me, liveCfg(), map[string]bool{"S01": true}); len(got) != 0 {
		t.Fatalf("a dismissed nudge came back: %s", dueIDs(got))
	}
	off := liveCfg()
	off.Outbound = false
	if got := Due([]Message{waiting}, me, off, nil); len(got) != 0 {
		t.Fatalf("outbound off still nudged: %s", dueIDs(got))
	}
}

func TestDueInbound(t *testing.T) {
	to := []string{"me@example.com"}
	// A reply from a person, to me, 5 days ago, unanswered: due.
	reply := in("A01", "T1", 5, "ann@example.org", to, nil)
	reply.InReplyTo = "<x@x>"
	// A first message from a stranger: neither kind-personal nor a reply.
	stranger := in("A02", "T2", 5, "bob@example.org", to, nil)
	// From someone the user wrote to BEFORE: kind-personal. Due.
	o0 := out("O00", "T9", 60, "cat@example.org")
	known := in("A03", "T3", 5, "cat@example.org", to, nil)
	// \Answered: not due. Too young: not due. Cc only: not due.
	answered := in("A04", "T4", 5, "dan@example.org", to, nil)
	answered.InReplyTo = "<y@x>"
	answered.Answered = true
	young := in("A05", "T5", 2, "eve@example.org", to, nil)
	young.InReplyTo = "<z@x>"
	cc := in("A06", "T6", 5, "fay@example.org", []string{"other@example.org"}, to)
	cc.InReplyTo = "<w@x>"
	// Two in one conversation: only the newer is due.
	first := in("A07", "T7", 6, "gus@example.org", to, nil)
	first.InReplyTo = "<v@x>"
	second := in("A08", "T7", 5, "gus@example.org", to, nil)
	second.InReplyTo = first.MessageID
	// Replied to: not due.
	repliedTo := in("A09", "T8", 6, "hal@example.org", to, nil)
	repliedTo.InReplyTo = "<u@x>"
	myReply := out("O09", "T8", 5, "hal@example.org")

	// A reply whose subject reads transactional: the kind classifier cannot
	// decide (personal 2.0 against transactional 1.8), so ONLY the is-reply
	// half of the shipped rule picks it up — the case that makes the union
	// more than kind-personal alone.
	invoiceReply := in("A10", "T10", 5, "ivy@example.org", to, nil)
	invoiceReply.InReplyTo = "<t@x>"
	invoiceReply.Facts.Subject = "Re: the invoice for the shed"
	if f := InboundFeatures(&invoiceReply, me, false); f.Personal || !f.IsReply {
		t.Fatalf("fixture premise broken: personal=%v reply=%v", f.Personal, f.IsReply)
	}

	msgs := []Message{reply, stranger, o0, known, answered, young, cc, first, second, repliedTo, myReply, invoiceReply}
	got := Due(msgs, me, liveCfg(), nil)
	if want := "[A01:inbound A03:inbound A08:inbound A10:inbound]"; dueIDs(got) != want {
		t.Fatalf("due = %s, want %s", dueIDs(got), want)
	}
	// A Done on the newer one covers its conversation: the older does not return.
	if got := Due(msgs, me, liveCfg(), map[string]bool{"A08": true}); dueIDs(got) != "[A01:inbound A03:inbound A10:inbound]" {
		t.Fatalf("after Done on A08: %s", dueIDs(got))
	}
	off := liveCfg()
	off.Inbound = false
	if got := Due(msgs, me, off, nil); len(got) != 0 {
		t.Fatalf("inbound off (the default) still nudged: %s", dueIDs(got))
	}
	later := liveCfg()
	later.InboundAfter = 7 * 24 * time.Hour
	if got := Due(msgs, me, later, nil); len(got) != 0 {
		t.Fatalf("a 7-day delay nudged 5-day-old mail: %s", dueIDs(got))
	}
}

// The live rule and the report's shipped variant agree message by message on
// the same corpus: what the report counts as flagged by the shipped inbound
// rule is exactly what Due considers eligible (before timing).
func TestDueAndReportAgreeOnTheShippedInboundRule(t *testing.T) {
	cfg := DefaultConfig(now, 180*24*time.Hour)
	corpus := inboundCorpus()
	rep := Evaluate(corpus, me, cfg)
	var shipped InboundVariant
	for _, v := range rep.InboundVariants {
		if v.Name == ShippedInboundRule {
			shipped = v
		}
	}
	ix := newIndex(corpus, me)
	n := 0
	for _, m := range ix.ms {
		if m.Outbound() || m.Junk || !m.Parsed || m.At.After(now.Add(-cfg.ReplyWithin)) || m.At.Before(cfg.Since) {
			continue
		}
		if WaitingOnYou(InboundFeatures(m, me, ix.correspondedBefore(m))) {
			n++
		}
	}
	if n != shipped.Precision.N || n == 0 {
		t.Fatalf("WaitingOnYou flags %d of the corpus, the report's shipped variant %d", n, shipped.Precision.N)
	}
}
