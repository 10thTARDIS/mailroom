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
		{"one filing is thin", KeyAddress, ev(1, t0, map[ulid.ULID]int{updates: 1}), Thin},
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

	// The address's one filing AGREES with the domain (it is one of the
	// domain's own), which is the only way the domain can be confident while
	// the address has evidence at all.
	d := Decide(addr, dom, ev(1, t0, map[ulid.ULID]int{updates: 1}), domainEv, nil)
	if d.Act || d.Key != addr || d.Reason != Thin {
		t.Errorf("thin address in a confident domain = %+v, want abstain on the address (thin)", d)
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
