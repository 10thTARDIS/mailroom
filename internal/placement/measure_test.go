// SPDX-License-Identifier: AGPL-3.0-only

package placement

import (
	"math"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// Every expectation below is computed by hand from the fixture next to it, and
// the Wilson bounds were computed independently of internal/stats (a short
// script over the textbook formula), so a test cannot agree with the code by
// sharing its mistake.

var mt0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func day(n int) time.Time { return mt0.AddDate(0, 0, n) }

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0006 {
		t.Errorf("%s = %.4f, want %.4f", name, got, want)
	}
}

func TestMeasureRecency(t *testing.T) {
	u, p := ulid.Make(), ulid.Make()
	f := func(a ulid.ULID, d int) TimedFiling { return TimedFiling{Area: a, At: day(d)} }
	filings := map[string][]TimedFiling{
		// newest area U; newest other is P on day 3; U on days 4,5,6 are newer: run 3.
		"k1@a.example": {f(u, 1), f(u, 2), f(p, 3), f(u, 4), f(u, 5), f(u, 6)},
		// run 2.
		"k2@a.example": {f(p, 1), f(u, 2), f(u, 3)},
		// run 5 (the P is day 0).
		"k3@a.example": {f(p, 0), f(u, 1), f(u, 2), f(u, 3), f(u, 4), f(u, 5)},
		// the newest INSTANT holds both areas: no tail area, run 0.
		"k4@a.example": {f(u, 1), f(p, 2), f(u, 3), f(p, 3)},
		// not mixed: three filings, one area.
		"k5@a.example": {f(u, 1), f(u, 2), f(u, 3)},
		// run 1.
		"k6@a.example": {f(u, 1), f(u, 2), f(p, 3)},
		// U on day 2 ties with P on day 2, so only the day-3 U is provably newer: run 1.
		"k7@a.example": {f(p, 2), f(u, 1), f(u, 2), f(u, 3)},
	}
	recent := map[string]int{"k1@a.example": 10, "k2@a.example": 4, "k3@a.example": 7, "k4@a.example": 1,
		"k5@a.example": 50, "k6@a.example": 2, "k7@a.example": 6}
	got := MeasureRecency(filings, recent, RecencyRuns)
	if got.Keys != 7 || got.Mixed != 6 || got.MixedRecent != 30 {
		t.Errorf("keys/mixed/mixedRecent = %d/%d/%d, want 7/6/30 (k5 is not mixed and its 50 are excluded)",
			got.Keys, got.Mixed, got.MixedRecent)
	}
	want := []RecencyRow{{N: 2, Keys: 3, Recent: 21}, {N: 3, Keys: 2, Recent: 17}, {N: 5, Keys: 1, Recent: 7}}
	for i, w := range want {
		if got.Rows[i] != w {
			t.Errorf("run %d = %+v, want %+v", w.N, got.Rows[i], w)
		}
	}
}

// TailRun alone, for the tie cases the aggregate hides.
func TestTailRunTies(t *testing.T) {
	u, p := ulid.Make(), ulid.Make()
	if a, n := TailRun([]TimedFiling{{u, day(1)}, {p, day(1)}}); n != 0 || a != (ulid.ULID{}) {
		t.Errorf("two areas at one instant: area %v run %d, want no tail area", a, n)
	}
	if a, n := TailRun([]TimedFiling{{p, day(1)}, {u, day(2)}, {u, day(3)}}); a != u || n != 2 {
		t.Errorf("run = %v/%d, want U/2", a, n)
	}
	if _, n := TailRun(nil); n != 0 {
		t.Errorf("no filings: run %d", n)
	}
}

func TestEvaluateLeftAlone(t *testing.T) {
	u, p := ulid.Make(), ulid.Make()
	ev := func(from string, a ulid.ULID, n int) []Event {
		var out []Event
		for i := 0; i < n; i++ {
			out = append(out, Event{From: from, Area: a, At: day(i)})
		}
		return out
	}
	var events, filings []Event
	events = append(events, ev("a@shop.example", u, 2)...)    // address a: confident U
	events = append(events, ev("b@news.example", u, 1)...)    // thin
	events = append(events, ev("c@list.example", u, 2)...)    // mixed with the next line
	events = append(events, ev("c@list.example", p, 1)...)    //
	events = append(events, ev("d1@corp.example", u, 1)...)   // thin alone, but two addresses...
	events = append(events, ev("d2@corp.example", u, 1)...)   // ...make corp.example confident U
	events = append(events, ev("f@fresh.example", p, 3)...)   // confident P
	events = append(events, ev("g@known.example", u, 2)...)   // confident U, but the user already filed g
	filings = append(filings, ev("g@known.example", u, 2)...) // the user's own evidence before the split

	filedTo := func(a ulid.ULID) *ulid.ULID { return &a }
	outcomes := []LeftAloneOutcome{
		{From: "a@shop.example"},                      // taught U, kept
		{From: "a@shop.example", FiledTo: filedTo(p)}, // taught U, corrected
		{From: "a@shop.example", FiledTo: filedTo(u)}, // taught U, filed INTO the taught area: kept
		{From: "d3@corp.example"},                     // no evidence of its own: the domain decides, U, kept
		{From: "d3@corp.example", FiledTo: filedTo(p)},
		{From: "d1@corp.example"}, // its own thin evidence outranks the domain: not taught
		{From: "f@fresh.example"},
		{From: "f@fresh.example", FiledTo: filedTo(u)},
		{From: "g@known.example"}, // the user's filings already cover it
		{From: "z@other.example"},
		{From: "z@other.example", FiledTo: filedTo(p)},
		{From: "c@list.example", FiledTo: filedTo(u)}, // mixed: not taught
	}
	r := EvaluateLeftAlone(events, filings, outcomes, nil)

	if r.Events != 13 {
		t.Errorf("events = %d, want 13", r.Events)
	}
	if r.EventsByArea[u] != 9 || r.EventsByArea[p] != 4 {
		t.Errorf("events by area = %v, want Updates 9, Primary 4", r.EventsByArea)
	}
	// Confident keys: addresses a (U), f (P), g (U); domain corp.example (U).
	// New (the user has no filing on the key): a, f, corp.example, not g.
	wantConf := map[ulid.ULID]KeyCounts{u: {Addresses: 2, Domains: 1}, p: {Addresses: 1}}
	wantNew := map[ulid.ULID]KeyCounts{u: {Addresses: 1, Domains: 1}, p: {Addresses: 1}}
	for a, w := range wantConf {
		if r.Keys.Confident[a] != w {
			t.Errorf("confident keys for %v = %+v, want %+v", a, r.Keys.Confident[a], w)
		}
		if r.Keys.New[a] != wantNew[a] {
			t.Errorf("new keys for %v = %+v, want %+v", a, r.Keys.New[a], wantNew[a])
		}
	}
	if got := r.Keys.Reasons[KeyAddress]; got[Confident] != 3 || got[Thin] != 3 || got[Mixed] != 1 {
		t.Errorf("address verdicts = %v, want confident 3, thin 3 (b, d1, d2), mixed 1 (c)", got)
	}
	if got := r.Keys.Reasons[KeyDomain]; got[Confident] != 1 || got[Thin] != 4 || got[Mixed] != 1 {
		t.Errorf("domain verdicts = %v, want confident 1 (corp), thin 4, mixed 1 (list)", got)
	}

	// Outcomes: U kept 3 (a nil, a->U, d3 nil), corrected 2 (a->P, d3->P);
	// P kept 1 (f nil), corrected 1 (f->U).
	if got := r.Areas[u]; got.Kept != 3 || got.Corrected != 2 || got.Messages != 5 || got.Gate.Judged {
		t.Errorf("Updates = %+v, want kept 3, corrected 2, not yet judged", got)
	}
	near(t, "Updates precision", r.Areas[u].Gate.Precision, 0.6)
	near(t, "Updates LCB95", r.Areas[u].Gate.LCB, 0.2307)
	if got := r.Areas[p]; got.Kept != 1 || got.Corrected != 1 {
		t.Errorf("Primary = %+v, want kept 1, corrected 1", got)
	}
	near(t, "Primary LCB95", r.Areas[p].Gate.LCB, 0.0945)
	if r.Total.Kept != 4 || r.Total.Corrected != 3 {
		t.Errorf("total = %+v, want kept 4, corrected 3", r.Total)
	}
	near(t, "total LCB95", r.Total.Gate.LCB, 0.2505)
	if r.Served != 1 || r.Untaught != 4 {
		t.Errorf("served/untaught = %d/%d, want 1/4 (d1, z, z, c)", r.Served, r.Untaught)
	}
	// The base rate: of the untaught, z->P and c->U were filed by hand (2 of 4);
	// of the taught, a->P, a->U, d3->P, f->U (4 of 7).
	if r.MovedAnyTaught != 4 || r.MovedAnyUntaught != 2 {
		t.Errorf("moved any (taught/untaught) = %d/%d, want 4/2", r.MovedAnyTaught, r.MovedAnyUntaught)
	}
}

func TestEvaluateArchivedUnread(t *testing.T) {
	promo, primary := ulid.Make(), ulid.Make()
	ev := func(from string, a ulid.ULID, n int) []Event {
		var out []Event
		for i := 0; i < n; i++ {
			out = append(out, Event{From: from, Area: a, At: day(i)})
		}
		return out
	}
	var events []Event
	events = append(events, ev("x@promo.example", promo, 2)...)  // confident, origin Promotions
	events = append(events, ev("y@news.example", promo, 1)...)   // mixed origin
	events = append(events, ev("y@news.example", primary, 1)...) //
	events = append(events, ev("w@bank.example", primary, 2)...) // confident, origin Primary
	events = append(events, ev("v@one.example", promo, 1)...)    // thin
	outcomes := []ArchiveOutcome{
		{"x@promo.example", ArchivedUnread}, {"x@promo.example", ArchivedUnread},
		{"x@promo.example", Opened}, {"x@promo.example", KeptUnopened}, {"x@promo.example", ArchivedOther},
		{"w@bank.example", Opened},
		{"y@news.example", Opened}, {"v@one.example", ArchivedUnread}, {"q@other.example", Opened},
	}
	r := EvaluateArchivedUnread(events, outcomes, nil)
	if r.Events != 7 || r.Untaught != 3 {
		t.Errorf("events/untaught = %d/%d, want 7/3 (y mixed, v thin, q unknown)", r.Events, r.Untaught)
	}
	if r.Keys.Confident[promo] != (KeyCounts{Addresses: 1}) || r.Keys.Confident[primary] != (KeyCounts{Addresses: 1}) {
		t.Errorf("confident keys = %+v, want one address per origin area", r.Keys.Confident)
	}
	if got := r.Keys.Reasons[KeyAddress]; got[Confident] != 2 || got[Mixed] != 1 || got[Thin] != 1 {
		t.Errorf("address verdicts = %v, want confident 2, mixed 1, thin 1", got)
	}
	pr := r.Areas[promo]
	if pr.Messages != 5 || pr.ArchivedUnread != 2 || pr.Opened != 1 || pr.KeptUnopened != 1 || pr.ArchivedOther != 1 {
		t.Errorf("Promotions = %+v, want 5 messages: 2 archived unread, 1 opened, 1 kept, 1 other", pr)
	}
	near(t, "Promotions strict precision", pr.Strict.Precision, 0.4) // 2 of 5
	near(t, "Promotions strict LCB95", pr.Strict.LCB, 0.1176)
}

func TestMeasureListID(t *testing.T) {
	u, p := ulid.Make(), ulid.Make()
	s := func(v string) *string { return &v }
	f := func(addr string, a ulid.ULID, list *string, n int) []ListFiling {
		var out []ListFiling
		for i := 0; i < n; i++ {
			out = append(out, ListFiling{Address: addr, Area: a, ListID: list})
		}
		return out
	}
	var fs []ListFiling
	fs = append(fs, f("s1@news.example", u, s("a.news.example"), 2)...) // separated: a->U, b->P
	fs = append(fs, f("s1@news.example", p, s("b.news.example"), 2)...)
	fs = append(fs, f("s2@news.example", u, s("a.news.example"), 1)...) // partial: a mixed, b pure
	fs = append(fs, f("s2@news.example", p, s("a.news.example"), 1)...)
	fs = append(fs, f("s2@news.example", p, s("b.news.example"), 2)...)
	fs = append(fs, f("s3@news.example", u, s("a.news.example"), 1)...) // not helped: one list id holds both
	fs = append(fs, f("s3@news.example", p, s("a.news.example"), 1)...)
	fs = append(fs, f("s4@plain.example", u, s(""), 1)...) // mixed, but never carries more than one List-Id
	fs = append(fs, f("s4@plain.example", p, s(""), 1)...)
	fs = append(fs, f("s5@news.example", u, s("a.news.example"), 2)...) // multi-list but filed into one area only
	fs = append(fs, f("s6@news.example", u, s("a.news.example"), 2)...) // separated by an UNFILLED list id
	fs = append(fs, f("s6@news.example", p, nil, 1)...)
	multi := map[string]bool{"s1@news.example": true, "s2@news.example": true, "s3@news.example": true,
		"s5@news.example": true, "s6@news.example": true}
	got := MeasureListID(fs, multi)
	want := ListIDCeiling{Filings: 17, Unfilled: 1, MixedAddresses: 5, MultiList: 4,
		Separated: 2, Partial: 1, NotHelped: 1, SubKeysConfident: 4}
	if got != want {
		t.Errorf("ceiling = %+v\nwant      %+v", got, want)
	}
}
