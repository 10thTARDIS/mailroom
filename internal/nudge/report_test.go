// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"fmt"
	"math"
	"testing"
	"time"

	"mailroom/internal/kind"
)

var (
	now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me  = NewMe([]string{"me@example.com"}, nil)
)

// ago is a time d days (fractional allowed) before now.
func ago(d float64) time.Time { return now.Add(-time.Duration(d * float64(24*time.Hour))) }

// in builds a parsed inbound message.
func in(id, thread string, daysAgo float64, from string, to, cc []string) Message {
	return Message{
		ID: id, Thread: thread, At: ago(daysAgo), From: from, MessageID: "<" + id + "@x>",
		Parsed: true, To: to, Cc: cc,
		Facts: kind.Facts{FromAddress: from, FromDomain: domainOf(from), Subject: "s"},
	}
}

// out builds a parsed message the user sent (Sent folder + identity).
func out(id, thread string, daysAgo float64, to ...string) Message {
	return Message{
		ID: id, Thread: thread, At: ago(daysAgo), Date: ago(daysAgo), From: "me@example.com", MessageID: "<" + id + "@x>",
		InSentFolder: true, FromIdentity: true, Parsed: true, To: to,
		Facts: kind.Facts{FromAddress: "me@example.com", FromDomain: "example.com"},
	}
}

func domainOf(a string) string {
	for i := len(a) - 1; i >= 0; i-- {
		if a[i] == '@' {
			return a[i+1:]
		}
	}
	return ""
}

func variant(t *testing.T, rep Report, name string) InboundVariant {
	t.Helper()
	for _, v := range rep.InboundVariants {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no inbound variant %q", name)
	return InboundVariant{}
}

func outVariant(t *testing.T, rep Report, name string) OutboundVariant {
	t.Helper()
	for _, v := range rep.OutboundVariants {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no outbound variant %q", name)
	return OutboundVariant{}
}

func eqRate(t *testing.T, what string, got Rate, hits, n int) {
	t.Helper()
	if got.Hits != hits || got.N != n {
		t.Errorf("%s = %d/%d, want %d/%d", what, got.Hits, got.N, hits, n)
	}
}

// inboundCorpus is a mailbox whose inbound ground truth is known exactly:
//
//	A1  alice, To me, asks, corresponded before (O0)  — replied after 1d
//	A2  bob,   To me                                  — never replied; superseded by A12
//	A12 bob,   To me, same thread, reply to A2        — never replied
//	A3  carol, Cc me only                             — replied after 5d
//	A4  list (List-Id), To me                         — never replied
//	A5  no-reply sender, To me                        — never replied
//	A6  dave,  To me, \Answered, no reply message     — replied (untimed)
//	A10 frank, To me; reply O3 lands in ANOTHER thread, naming A10 in In-Reply-To — replied after 1d
//	A7  censored (10d old); A8 junk; A9 before the window
func inboundCorpus() []Message {
	me := []string{"me@example.com"}
	a1 := in("A01", "T1", 60, "alice@example.org", me, nil)
	a1.Question = true
	a2 := in("A02", "T2", 50, "bob@example.org", me, nil)
	a12 := in("A12", "T2", 49.5, "bob@example.org", me, nil)
	a12.InReplyTo = a2.MessageID
	a3 := in("A03", "T3", 40, "carol@example.org", []string{"someone@example.org"}, me)
	a4 := in("A04", "T4", 45, "news@list.example.org", me, nil)
	a4.Facts.ListID = "<news.list.example.org>"
	a5 := in("A05", "T5", 30, "noreply@shop.example.com", me, nil)
	a6 := in("A06", "T6", 20, "dave@example.org", me, nil)
	a6.Answered = true
	a10 := in("A10", "T7", 25, "frank@example.org", me, nil)
	a7 := in("A07", "T10", 10, "erin@example.org", me, nil)
	a8 := in("A08", "T11", 30, "spam@example.net", me, nil)
	a8.Junk = true
	a9 := in("A09", "T12", 200, "alice@example.org", me, nil)

	o0 := out("O00", "T9", 100, "alice@example.org")
	o1 := out("O01", "T1", 59, "alice@example.org")
	o2 := out("O02", "T3", 35, "carol@example.org")
	o3 := out("O03", "T8", 24, "frank@example.org")
	o3.InReplyTo = a10.MessageID
	return []Message{a1, a2, a12, a3, a4, a5, a6, a10, a7, a8, a9, o0, o1, o2, o3}
}

func TestEvaluateInboundExact(t *testing.T) {
	cfg := DefaultConfig(now, 180*24*time.Hour)
	rep := Evaluate(inboundCorpus(), me, cfg)

	if rep.InboundInWindow != 10 || rep.InboundCensored != 1 || rep.InboundJunk != 1 || rep.InboundUnparsed != 0 || rep.InboundEvaluable != 8 {
		t.Fatalf("population: window %d censored %d junk %d unparsed %d evaluable %d; want 10/1/1/0/8",
			rep.InboundInWindow, rep.InboundCensored, rep.InboundJunk, rep.InboundUnparsed, rep.InboundEvaluable)
	}
	if want := 166.0 / 7; math.Abs(rep.InboundWeeks-want) > 1e-9 {
		t.Fatalf("weeks = %v, want %v", rep.InboundWeeks, want)
	}
	if rep.Audit.RepliesByInReplyToOnly != 1 {
		t.Errorf("replies by In-Reply-To only = %d, want 1 (A10)", rep.Audit.RepliesByInReplyToOnly)
	}
	if rep.Audit.AnsweredInbound != 1 || rep.Audit.AnsweredNoReply != 1 {
		t.Errorf("answered audit %+v", rep.Audit)
	}

	all := variant(t, rep, "all")
	eqRate(t, "all precision", all.Precision, 4, 8)
	eqRate(t, "all recall", all.Recall, 4, 4)
	if all.TimedReplies != 3 || all.AnsweredOnly != 1 {
		t.Errorf("all: timed %d answered-only %d, want 3/1", all.TimedReplies, all.AnsweredOnly)
	}

	person := variant(t, rep, "person")
	eqRate(t, "person precision", person.Precision, 4, 6) // A1 A2 A12 A3 A6 A10
	eqRate(t, "person recall", person.Recall, 4, 4)
	// person at 3d: A3 (replied on day 5) fires late-replied; A2 superseded by
	// A12; A12 and A6 fire. At 7d A3 was replied in time.
	n3, n7 := person.Nudges[0], person.Nudges[1]
	if n3.AfterDays != 3 || n3.Fired != 3 || n3.Superseded != 1 || n3.LateReplied != 1 || n3.AnsweredUntimed != 1 || n3.NoReply != 1 {
		t.Errorf("person nudge@3d = %+v", n3)
	}
	if n7.Fired != 2 || n7.Superseded != 1 || n7.LateReplied != 0 {
		t.Errorf("person nudge@7d = %+v", n7)
	}
	if want := 3 / (166.0 / 7); math.Abs(n3.PerWeek-want) > 1e-9 {
		t.Errorf("per week = %v, want %v", n3.PerWeek, want)
	}

	toMe := variant(t, rep, "person+to-me")
	eqRate(t, "to-me precision", toMe.Precision, 3, 5) // A1 A2 A12 A6 A10
	eqRate(t, "to-me recall", toMe.Recall, 3, 4)       // misses A3 (Cc only)

	corr := variant(t, rep, "person+to-me+corresponded")
	eqRate(t, "corresponded precision", corr.Precision, 1, 1) // A1 only: O0 wrote to alice first

	asks := variant(t, rep, "person+to-me+asks")
	eqRate(t, "asks precision", asks.Precision, 1, 1)

	kp := variant(t, rep, "person+to-me+kind-personal")
	eqRate(t, "kind-personal precision", kp.Precision, 1, 2) // A1 (corresponded) and A12 (a reply)

	shipped := variant(t, rep, ShippedInboundRule)
	eqRate(t, "shipped (kind-personal|is-reply) precision", shipped.Precision, 1, 2) // A1, A12
	eqRate(t, "shipped recall", shipped.Recall, 1, 4)

	// Per signal: me-in-to on all evaluable inbound.
	for _, s := range rep.SignalsAll {
		if s.Name == "me-in-to" {
			eqRate(t, "me-in-to WITH", s.With, 3, 7)
			eqRate(t, "me-in-to WITHOUT", s.Without, 1, 1)
		}
	}
	for _, v := range rep.InboundVariants {
		if len(v.Examples) != 0 {
			t.Fatalf("examples kept with Examples=0: %v", v.Examples)
		}
	}
}

// The correspondence feature must be AS OF the message: a reply the user
// sends AFTER a message must not make that message's sender "corresponded".
// contact_addresses.outbound_count would — it is today's total, and the
// reply under measurement bumps it (#220's fixed-point trap).
func TestEvaluateCorrespondenceIsAsOf(t *testing.T) {
	cfg := DefaultConfig(now, 180*24*time.Hour)
	msgs := []Message{
		in("B01", "T1", 60, "zed@example.org", []string{"me@example.com"}, nil),
		out("O01", "T1", 59, "zed@example.org"),
	}
	rep := Evaluate(msgs, me, cfg)
	if v := variant(t, rep, "person+to-me+corresponded"); v.Precision.N != 0 {
		t.Fatalf("a later reply leaked into corresponded-before: %+v", v.Precision)
	}
}

// outboundCorpus: sent mail with known answers (see the expectations).
func outboundCorpus() []Message {
	s1 := out("S01", "T1", 40, "alice@example.org")
	a1 := in("R01", "T1", 38, "alice@example.org", []string{"me@example.com"}, nil)
	a1.InReplyTo = s1.MessageID
	s2 := out("S02", "T2", 40, "bob@example.org")
	a2 := in("R02", "T2", 35, "bob@example.org", []string{"me@example.com"}, nil)
	s3 := out("S03", "T3", 40, "carol@example.org")
	s4 := out("S04", "T4", 40, "dave@example.org")
	auto := in("R04", "T4", 39, "dave@example.org", []string{"me@example.com"}, nil)
	auto.Facts.AutoSubmitted = "auto-replied"
	s5 := out("S05", "T5", 40, "noreply@shop.example.com")
	l1 := in("L01", "T6", 41, "list@lists.example.org", []string{"list@lists.example.org"}, nil)
	l1.Facts.ListID = "<l.lists.example.org>"
	s6 := out("S06", "T6", 40, "list@lists.example.org")
	s6.InReplyTo = l1.MessageID
	s7 := out("S07", "T7", 40, "me@example.com")
	s8 := out("S08", "T8", 20, "erin@example.org")
	s9 := out("S09", "T8", 18, "erin@example.org")
	s9.InReplyTo = s8.MessageID
	s10 := out("S10", "T9", 2, "frank@example.org")
	s11 := out("S11", "T1", 37, "alice@example.org")
	s11.InReplyTo = a1.MessageID
	s11.Question = true
	// S12 continues greg's thread WITHOUT an In-Reply-To (a client that drops
	// it): only thread order can say it did not start the conversation.
	g1 := in("G01", "T10", 45, "greg@example.org", []string{"me@example.com"}, nil)
	s12 := out("S12", "T10", 44, "greg@example.org")
	// S13 replies to a list message that threading left in no thread: only
	// the In-Reply-To parent can say it is list traffic.
	l2 := in("L02", "", 41, "list2@lists.example.org", []string{"list2@lists.example.org"}, nil)
	l2.Facts.ListID = "<l2.lists.example.org>"
	s13 := out("S13", "T11", 40, "list2@lists.example.org")
	s13.InReplyTo = l2.MessageID
	s13.Date = ago(400) // imported: arrival is not when it was written
	s7.Date = time.Time{}
	return []Message{s1, a1, s2, a2, s3, s4, auto, s5, l1, s6, s7, s8, s9, s10, s11, g1, s12, l2, s13}
}

func TestEvaluateOutboundExact(t *testing.T) {
	cfg := DefaultConfig(now, 180*24*time.Hour)
	cfg.Examples = 10
	rep := Evaluate(outboundCorpus(), me, cfg)

	if rep.OutboundInWindow != 13 || rep.OutboundAutoOnly != 1 {
		t.Fatalf("outbound in window %d auto-only %d, want 13/1", rep.OutboundInWindow, rep.OutboundAutoOnly)
	}
	if a := rep.Audit; a.OutboundClockSkewOver1d != 1 || a.OutboundNoDate != 1 {
		t.Errorf("clock audit: skew %d no-date %d, want 1/1", a.OutboundClockSkewOver1d, a.OutboundNoDate)
	}
	if a := rep.Audit; a.OutboundThreadedWithInbound != 6 || a.OutboundWithInReplyTo != 4 || a.ParentSameThread != 3 || a.ParentOtherThread != 1 {
		t.Errorf("audit %+v", a)
	}

	if v := outVariant(t, rep, "all"); v.InWindow != 13 {
		t.Errorf("all in window %d", v.InWindow)
	}
	tp := outVariant(t, rep, "to-people")
	if tp.InWindow != 9 {
		t.Fatalf("to-people in window %d, want 9 (S5 no-reply, S6 and S13 list, S7 self excluded)", tp.InWindow)
	}
	want := []struct{ days, hits, n, fired, sup, lateObs, lateAns int }{
		{3, 1, 8, 6, 1, 5, 1},
		{7, 2, 8, 5, 1, 4, 0},
		{14, 2, 8, 5, 1, 4, 0},
	}
	for i, w := range want {
		od := tp.ByDays[i]
		if od.Days != w.days || od.Answered.Hits != w.hits || od.Answered.N != w.n || od.Fired != w.fired ||
			od.Superseded != w.sup || od.LateObserved != w.lateObs || od.LateAnswered != w.lateAns {
			t.Errorf("to-people @%dd = %+v, want %+v", w.days, od, w)
		}
	}
	if want := 6 / (177.0 / 7); math.Abs(tp.ByDays[0].PerWeek-want) > 1e-9 {
		t.Errorf("per week %v want %v", tp.ByDays[0].PerWeek, want)
	}
	if v := outVariant(t, rep, "to-people+started"); v.InWindow != 6 {
		t.Errorf("started in window %d, want 6", v.InWindow)
	}
	if v := outVariant(t, rep, "to-people+continued"); v.InWindow != 3 {
		t.Errorf("continued in window %d, want 3 (S9, S11, S12)", v.InWindow)
	}
	if v := outVariant(t, rep, "to-people+asks"); v.InWindow != 1 {
		t.Errorf("asks in window %d, want 1", v.InWindow)
	}
	got := fmt.Sprint(tp.Examples["nudged at 7d"])
	if got != "[S03 S04 S09 S11 S12]" {
		t.Errorf("nudged-at-7d examples = %s", got)
	}
	if got := fmt.Sprint(tp.Examples["answered within 7d"]); got != "[S01 S02]" {
		t.Errorf("answered examples = %s", got)
	}
}

func TestRateNaN(t *testing.T) {
	if !math.IsNaN(Rate{}.P()) || !math.IsNaN(Rate{}.LB()) {
		t.Fatal("an empty rate must be NaN, not 0")
	}
	if lb := (Rate{Hits: 5, N: 5}).LB(); lb > 0.6 || lb < 0.5 {
		t.Fatalf("5/5 LB = %v; Wilson is ~0.57", lb)
	}
}
