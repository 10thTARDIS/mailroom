// SPDX-License-Identifier: AGPL-3.0-only

package placement

import (
	"math"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	updates    = ulid.MustParse("01J00000000000000000000001")
	promotions = ulid.MustParse("01J00000000000000000000002")
	t0         = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
)

func ev(addresses int, latest time.Time, filings map[ulid.ULID]int) Evidence {
	return Evidence{Filings: filings, Addresses: addresses, LatestFiling: latest}
}

func TestKeysFor(t *testing.T) {
	a, d, ok := KeysFor("  Alice@Example.COM ")
	if !ok || a != (Key{KeyAddress, "alice@example.com"}) || d != (Key{KeyDomain, "example.com"}) {
		t.Errorf("KeysFor = %v %v %v", a, d, ok)
	}
	for _, bad := range []string{"", "alice", "@example.com", "alice@"} {
		if _, _, ok := KeysFor(bad); ok {
			t.Errorf("KeysFor(%q) ok, want no key", bad)
		}
	}
}

func TestJudge(t *testing.T) {
	cases := []struct {
		name string
		kind KeyKind
		e    Evidence
		want Reason
	}{
		{"none", KeyAddress, Evidence{}, NoEvidence},
		{"one filing acts for an address", KeyAddress, ev(1, t0, map[ulid.ULID]int{updates: 1}), Confident},
		{"one filing is thin for a domain", KeyDomain, ev(2, t0, map[ulid.ULID]int{updates: 1}), Thin},
		{"two agreeing filings act", KeyAddress, ev(1, t0, map[ulid.ULID]int{updates: 2}), Confident},
		{"mixed abstains", KeyAddress, ev(1, t0, map[ulid.ULID]int{updates: 5, promotions: 1}), Mixed},
		{"domain over one address is thin", KeyDomain, ev(1, t0, map[ulid.ULID]int{updates: 9}), Thin},
		{"domain over two addresses acts", KeyDomain, ev(2, t0, map[ulid.ULID]int{updates: 2}), Confident},
	}
	for _, c := range cases {
		if got := Judge(c.kind, c.e).Reason; got != c.want {
			t.Errorf("%s: Judge = %s, want %s", c.name, got, c.want)
		}
	}
}

// The contradiction rule: a correction blocks the key until a filing STRICTLY
// newer than it. The filing made in the same transaction as the correction
// (same timestamp) does not lift it.
func TestJudgeContradiction(t *testing.T) {
	e := ev(1, t0, map[ulid.ULID]int{updates: 3})
	e.LatestCorrection = t0
	if got := Judge(KeyAddress, e).Reason; got != Contradicted {
		t.Errorf("filing at the correction's own instant: %s, want contradicted", got)
	}
	e.LatestFiling = t0.Add(time.Second)
	if got := Judge(KeyAddress, e).Reason; got != Confident {
		t.Errorf("filing after the correction: %s, want confident", got)
	}
	e.LatestCorrection = t0.Add(time.Hour)
	if got := Judge(KeyAddress, e).Reason; got != Contradicted {
		t.Errorf("correction after the newest filing: %s, want contradicted", got)
	}
}

// The address beats the domain: an address with ANY evidence decides, even
// when its own verdict is thin and the domain's is confident.
func TestDecideAddressBeatsDomain(t *testing.T) {
	addr, dom, _ := KeysFor("alice@example.com")
	domainEv := ev(3, t0, map[ulid.ULID]int{updates: 10})

	// An address with ONE filing into Promotions, in a domain confidently filed
	// into Updates: the address decides (a single filing acts for an address,
	// ADR-032 amendment), and it never follows the domain.
	d := Decide(addr, dom, ev(1, t0, map[ulid.ULID]int{promotions: 1}), domainEv, nil)
	if !d.Act || d.Key != addr || d.Area != promotions || d.Basis != BasisSingle {
		t.Errorf("single-filing address in a confident domain = %+v, want Promotions by the address on a single filing", d)
	}
	corrected := ev(1, t0, map[ulid.ULID]int{updates: 2})
	corrected.LatestCorrection = t0.Add(time.Minute)
	d = Decide(addr, dom, corrected, domainEv, nil)
	if d.Act || d.Key != addr || d.Reason != Contradicted {
		t.Errorf("corrected address in a confident domain = %+v, want abstain on the address (contradicted)", d)
	}
	d = Decide(addr, dom, ev(1, t0, map[ulid.ULID]int{promotions: 2}), domainEv, nil)
	if !d.Act || d.Area != promotions || d.Key != addr {
		t.Errorf("confident address = %+v, want Promotions by address", d)
	}
	d = Decide(addr, dom, Evidence{}, domainEv, nil)
	if !d.Act || d.Area != updates || d.Key != dom {
		t.Errorf("no address evidence = %+v, want Updates by domain", d)
	}
	d = Decide(addr, dom, Evidence{}, Evidence{}, nil)
	if d.Act || d.Reason != NoEvidence {
		t.Errorf("no evidence anywhere = %+v, want abstain (none)", d)
	}
}

func TestGate(t *testing.T) {
	cases := []struct {
		kept, corrected int
		judged, stopped bool
	}{
		{0, 0, false, false},
		{10, 9, false, false}, // 19 resolved: not judged, however bad
		{20, 0, true, false},  // LCB 0.839
		{19, 1, true, true},   // LCB 0.764: the first correction at 20 stops
		{37, 3, true, false},  // LCB 0.801
		{36, 4, true, true},
	}
	for _, c := range cases {
		g := Gate(GateCounts{Kept: c.kept, Corrected: c.corrected})
		if g.Judged != c.judged || g.Stopped != c.stopped {
			t.Errorf("Gate(%d kept, %d corrected) = judged %v stopped %v (lcb %.3f), want %v %v",
				c.kept, c.corrected, g.Judged, g.Stopped, g.LCB, c.judged, c.stopped)
		}
	}
	if g := Gate(GateCounts{}); !math.IsNaN(g.LCB) || !math.IsNaN(g.Precision) {
		t.Errorf("empty gate = %+v, want NaN precision and bound (no evidence is not 0)", g)
	}
}

// The domain fallback is blocked for freemail and the user's own domains;
// address-level learning on those domains is untouched.
func TestDecideDomainFallbackBlocked(t *testing.T) {
	domainEv := ev(3, t0, map[ulid.ULID]int{updates: 6})
	gAddr, gDom, _ := KeysFor("new.friend@gmail.com")
	if d := Decide(gAddr, gDom, Evidence{}, domainEv, nil); d.Act || d.Reason != DomainBlocked {
		t.Errorf("new gmail.com sender = %+v, want abstain (blocked)", d)
	}
	if d := Decide(gAddr, gDom, ev(1, t0, map[ulid.ULID]int{updates: 2}), domainEv, nil); !d.Act || d.Key != gAddr {
		t.Errorf("gmail.com address with its own evidence = %+v, want act by address", d)
	}
	if d := Decide(gAddr, gDom, Evidence{}, Evidence{}, nil); d.Reason != NoEvidence {
		t.Errorf("gmail.com with no domain evidence = %+v, want none (nothing was blocked)", d)
	}
	oAddr, oDom, _ := KeysFor("someone@mine.example")
	own := map[string]bool{"mine.example": true}
	if d := Decide(oAddr, oDom, Evidence{}, domainEv, own); d.Act || d.Reason != DomainBlocked {
		t.Errorf("new sender at the user's own domain = %+v, want abstain (blocked)", d)
	}
	if d := Decide(oAddr, oDom, Evidence{}, domainEv, nil); !d.Act {
		t.Errorf("the same domain when it is NOT the user's = %+v, want act (positive control)", d)
	}
}

// NewSender: one identity, checked against the From headers it came from,
// with authentication evaluated for that identity's own domain.
func TestNewSender(t *testing.T) {
	var asked []string
	authFor := func(domain string) SenderAuth {
		asked = append(asked, domain)
		return SenderAuth{TrustConfigured: true, DMARCPass: domain == "bank.example"}
	}
	one := FromHeaders{Fields: 1, Raw: []string{"Bank <alerts@bank.example>"}, Parsed: []string{"alerts@bank.example"}}
	s := NewSender("Alerts@Bank.example", one, authFor)
	a, d, ok := s.Keys()
	if !ok || a.Value != "alerts@bank.example" || d.Value != "bank.example" || s.Ambiguous() || !s.Auth().Authenticated() {
		t.Errorf("single From: %+v", s)
	}
	if len(asked) != 1 || asked[0] != "bank.example" {
		t.Errorf("authFor asked for %v, want exactly the key's own domain", asked)
	}
	for _, c := range []struct {
		name string
		hdr  FromHeaders
	}{
		{"two From headers", FromHeaders{Fields: 2, Raw: []string{"a@attacker.example", "alerts@bank.example"}, Parsed: []string{"alerts@bank.example"}}},
		{"two mailboxes", FromHeaders{Fields: 1, Raw: []string{"alerts@bank.example, x@attacker.example"}, Parsed: []string{"alerts@bank.example", "x@attacker.example"}}},
		{"a group", FromHeaders{Fields: 1, Raw: []string{"Bank: alerts@bank.example;"}, Parsed: []string{"alerts@bank.example"}}},
		{"parse differs from the stored address", FromHeaders{Fields: 1, Raw: []string{"x@attacker.example"}, Parsed: []string{"x@attacker.example"}}},
		{"no From header", FromHeaders{}},
		// #754: an EMPTY From field beside the real one. Values() drops the
		// empty field, so Raw and Parsed both look like exactly one From;
		// only the raw field count says there were two.
		{"an empty From field beside a real one", FromHeaders{Fields: 2, Raw: []string{"Bank <alerts@bank.example>"}, Parsed: []string{"alerts@bank.example"}}},
	} {
		if !NewSender("alerts@bank.example", c.hdr, authFor).Ambiguous() {
			t.Errorf("%s: not ambiguous", c.name)
		}
	}
	for _, raw := range []string{`"Bank: Alerts" <alerts@bank.example>`, "alerts@bank.example (Bank: alerts)", "<alerts@[IPv6:2001:db8::1]>"} {
		if hasGroup(raw) {
			t.Errorf("hasGroup(%q) = true; a ':' in quotes, a comment or angle brackets is not a group", raw)
		}
	}
}

// tl is an Evidence built from a timeline, the way the store builds it.
func tl(addresses int, fs ...TimedFiling) Evidence {
	e := Evidence{Filings: map[ulid.ULID]int{}, Addresses: addresses, Timeline: fs}
	for _, f := range fs {
		e.Filings[f.Area]++
		if f.At.After(e.LatestFiling) {
			e.LatestFiling = f.At
		}
	}
	return e
}

func fil(a ulid.ULID, mins int) TimedFiling {
	return TimedFiling{Area: a, At: t0.Add(time.Duration(mins) * time.Minute)}
}

// A single filing of an address acts and says so; two agreeing are the
// ordinary evidence; a domain never acts on one.
func TestJudgeBasis(t *testing.T) {
	if v := Judge(KeyAddress, tl(1, fil(updates, 0))); v.Reason != Confident || v.Basis != BasisSingle {
		t.Errorf("one filing = %+v, want confident on a single filing", v)
	}
	if v := Judge(KeyAddress, tl(1, fil(updates, 0), fil(updates, 1))); v.Reason != Confident || v.Basis != BasisEvidence {
		t.Errorf("two filings = %+v, want confident on the ordinary evidence", v)
	}
	if v := Judge(KeyDomain, tl(3, fil(updates, 0))); v.Reason != Thin {
		t.Errorf("domain with one filing = %+v, want thin", v)
	}
}

// The stricter undo: a correction of a single-filing placement blocks its key
// until SingleRelearnFilings filings strictly newer than the correction.
func TestJudgeSingleCorrectionNeedsTwoMoreFilings(t *testing.T) {
	corr := t0.Add(10 * time.Minute)
	base := []TimedFiling{fil(updates, 0)}
	mk := func(extra ...TimedFiling) Evidence {
		e := tl(1, append(append([]TimedFiling{}, base...), extra...)...)
		e.LatestCorrection, e.LatestSingleCorrection = corr, corr
		return e
	}
	if v := Judge(KeyAddress, mk()); v.Reason != Contradicted {
		t.Errorf("after the correction, nothing newer: %+v, want contradicted", v)
	}
	// One newer filing lifts the ORDINARY contradiction, but not this one.
	if v := Judge(KeyAddress, mk(fil(updates, 11))); v.Reason != Contradicted {
		t.Errorf("one newer filing after a single-filing correction: %+v, want still contradicted", v)
	}
	// A filing AT the correction's instant (the re-file that is the correction) never counts.
	if v := Judge(KeyAddress, mk(fil(updates, 10), fil(updates, 11))); v.Reason != Contradicted {
		t.Errorf("one newer plus one at the correction's instant: %+v, want contradicted", v)
	}
	if v := Judge(KeyAddress, mk(fil(updates, 11), fil(updates, 12))); v.Reason != Confident {
		t.Errorf("two newer agreeing filings: %+v, want confident", v)
	}
	// Control: the same history after an ORDINARY correction is lifted by one filing.
	e := tl(1, fil(updates, 0), fil(updates, 11))
	e.LatestCorrection = corr
	if v := Judge(KeyAddress, e); v.Reason != Confident {
		t.Errorf("one newer filing after an ordinary correction: %+v, want confident", v)
	}
}

// Recency: a mixed key's newest three filings into one area act, two do not,
// an interleaved filing in another area stops it, and a shared instant never
// counts.
func TestJudgeRecency(t *testing.T) {
	old := []TimedFiling{fil(promotions, 0), fil(promotions, 1)}
	run := func(n int) Evidence {
		fs := append([]TimedFiling{}, old...)
		for i := 0; i < n; i++ {
			fs = append(fs, fil(updates, 10+i))
		}
		return tl(1, fs...)
	}
	if v := Judge(KeyAddress, run(3)); v.Reason != Confident || v.Basis != BasisRecency || *v.Area != updates {
		t.Errorf("a run of 3 = %+v, want confident on recency into Updates", v)
	}
	if v := Judge(KeyAddress, run(2)); v.Reason != Mixed {
		t.Errorf("a run of 2 = %+v, want mixed", v)
	}
	// An interleaved filing elsewhere inside the last three.
	inter := tl(1, fil(promotions, 0), fil(updates, 10), fil(promotions, 11), fil(updates, 12), fil(updates, 13))
	if v := Judge(KeyAddress, inter); v.Reason != Mixed {
		t.Errorf("an interleaved filing = %+v, want mixed", v)
	}
	// Three filings, but the newest instant is shared with the other area, and
	// two of the three share an instant with it: the shared instants never count.
	shared := tl(1, fil(promotions, 0), fil(updates, 10), fil(promotions, 10), fil(updates, 12), fil(updates, 13))
	if v := Judge(KeyAddress, shared); v.Reason != Mixed {
		t.Errorf("a shared instant = %+v, want mixed (the tied filing is not provably newer)", v)
	}
	// A correction still blocks a recency placement by the ordinary rule.
	c := run(3)
	c.LatestCorrection = t0.Add(time.Hour)
	if v := Judge(KeyAddress, c); v.Reason != Contradicted {
		t.Errorf("a corrected recency key = %+v, want contradicted", v)
	}
	// A domain needs MinDomainAddresses addresses behind its run.
	if v := Judge(KeyDomain, run(3)); v.Reason != Mixed {
		t.Errorf("a domain run over one address = %+v, want mixed", v)
	}
	d := run(3)
	d.Addresses = 2
	for i := range d.Timeline { // the run's filings come from two addresses
		d.Timeline[i].Address = []string{"a@d.example", "b@d.example"}[i%2]
	}
	if v := Judge(KeyDomain, d); v.Reason != Confident || v.Basis != BasisRecency {
		t.Errorf("a domain run over two addresses = %+v, want confident on recency", v)
	}
	// Decide reaches it: the user's own and freemail domains stay blocked.
	addr, dom, _ := KeysFor("new@mine.example")
	if dc := Decide(addr, dom, Evidence{}, d, map[string]bool{"mine.example": true}); dc.Act {
		t.Errorf("recency on a blocked own domain acted: %+v", dc)
	}
	if dc := Decide(addr, dom, Evidence{}, d, nil); !dc.Act || dc.Basis != BasisRecency {
		t.Errorf("recency on an ordinary domain = %+v, want act on recency", dc)
	}
}

func filA(a ulid.ULID, mins int, addr string) TimedFiling {
	f := fil(a, mins)
	f.Address = addr
	return f
}

// A domain's recency run needs two distinct addresses WITHIN the run, not just
// across the domain: x2 filed once into Updates and x1 filed three times into
// Promotions is one address's habit, and must not be filed on for newcomers.
func TestJudgeDomainRecencyCountsAddressesWithinTheRun(t *testing.T) {
	one := tl(2, filA(updates, 0, "x2@corp.example"),
		filA(promotions, 10, "x1@corp.example"), filA(promotions, 11, "x1@corp.example"), filA(promotions, 12, "x1@corp.example"))
	one.Addresses = 2 // the whole domain has two addresses
	if v := Judge(KeyDomain, one); v.Reason != Mixed {
		t.Errorf("a run of three by ONE address in a two-address domain = %+v, want mixed", v)
	}
	two := tl(2, filA(updates, 0, "x2@corp.example"),
		filA(promotions, 10, "x1@corp.example"), filA(promotions, 11, "x2@corp.example"), filA(promotions, 12, "x1@corp.example"))
	two.Addresses = 2
	if v := Judge(KeyDomain, two); v.Reason != Confident || v.Basis != BasisRecency {
		t.Errorf("a run of three by two addresses = %+v, want confident on recency", v)
	}
	// The measure is the same rule.
	fs := map[string][]TimedFiling{"corp.example": one.Timeline, "other.example": two.Timeline}
	got := MeasureRecency(fs, map[string]int{}, []int{3}, MinDomainAddresses)
	if got.Rows[0].Keys != 1 {
		t.Errorf("domain recency rows = %+v, want exactly one key (the two-address run)", got.Rows)
	}
}

// A correction of a RECENCY placement is undone as strictly as a single
// filing's: two newer filings, not one.
func TestJudgeRecencyCorrectionNeedsTwoMoreFilings(t *testing.T) {
	corr := t0.Add(30 * time.Minute)
	mk := func(extra ...TimedFiling) Evidence {
		fs := []TimedFiling{fil(promotions, 0), fil(updates, 10), fil(updates, 11), fil(updates, 12)}
		e := tl(1, append(fs, extra...)...)
		e.LatestCorrection, e.LatestSingleCorrection = corr, corr // what the store sets for a recency basis
		return e
	}
	if v := Judge(KeyAddress, mk(fil(updates, 31))); v.Reason != Contradicted {
		t.Errorf("one newer filing after a recency correction: %+v, want still contradicted", v)
	}
	if v := Judge(KeyAddress, mk(fil(updates, 31), fil(updates, 32))); v.Reason != Confident {
		t.Errorf("two newer filings: %+v, want confident", v)
	}
}
