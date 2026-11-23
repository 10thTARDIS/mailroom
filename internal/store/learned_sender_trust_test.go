// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"

	"mailroom/internal/placement"
	"mailroom/internal/senderwarn"
)

// trustSender builds a sender the way ingest does, with one From header and the
// given authentication for its domain.
func trustSender(from string, auth placement.SenderAuth) placement.Sender {
	return placement.NewSender(from,
		placement.FromHeaders{Fields: 1, Raw: []string{from}, Parsed: []string{from}},
		func(string) placement.SenderAuth { return auth })
}

// No trusted header on the message at all: the shape of mail from a user's own
// infrastructure. A trusted server is configured, so there is no pass.
var noPass = placement.SenderAuth{TrustConfigured: true}

func TestSenderChecksTrustSkipsOnlyTheDMARCRequirement(t *testing.T) {
	ix := senderwarn.NewIndex(nil, nil)
	s := trustSender("robot@example.org", noPass)
	trusted := map[string]bool{"robot@example.org": true}

	// Control: untrusted, the same message is refused for authentication, so the
	// trusted case below is not "this message always passes".
	if got := senderChecks(s, ix, nil).Refusal; got != placement.Unauthenticated {
		t.Fatalf("untrusted, no pass: refusal %q, want unauthenticated", got)
	}
	c := senderChecks(s, ix, trusted)
	if c.Refusal != "" || !c.Trusted || !c.TrustApplied {
		t.Fatalf("trusted, no pass: %+v, want accepted with trust applied", c)
	}
	// Trust is for that exact address, not its domain or a sibling.
	sib := trustSender("other@example.org", noPass)
	if got := senderChecks(sib, ix, trusted).Refusal; got != placement.Unauthenticated {
		t.Fatalf("a sibling address of a trusted one: refusal %q, want unauthenticated", got)
	}
	// A trusted sender whose mail already passed does not "apply" trust.
	pass := trustSender("robot@example.org", placement.SenderAuth{TrustConfigured: true, DMARCPass: true})
	if c := senderChecks(pass, ix, trusted); c.Refusal != "" || c.TrustApplied {
		t.Fatalf("trusted and passing: %+v, want accepted without trust applied", c)
	}
}

func TestSenderChecksTrustDoesNotWaiveAnyOtherCheck(t *testing.T) {
	trusted := map[string]bool{"robot@example.org": true, "boss@mail.example.net": true}

	// A domain warning still blocks a trusted sender (the control that trust is
	// not "skip everything"). The look-alike warning is switched off in
	// internal/senderwarn (lookalikeEnabled); the punycode one is live and is
	// the same kind of check, a verdict on the domain and not on the message.
	puny := "robot@xn--80ak6aa92e.example"
	ix := senderwarn.NewIndex(nil, nil)
	if ws := ix.Check(senderwarn.Sender{Domain: "xn--80ak6aa92e.example", AuthPassed: true}); len(ws) == 0 {
		t.Fatalf("fixture: expected a punycode warning for the domain")
	}
	s := trustSender(puny, noPass)
	if got := senderChecks(s, ix, map[string]bool{puny: true}).Refusal; got != placement.SenderWarned {
		t.Fatalf("trusted sender at a punycode domain: refusal %q, want warned", got)
	}

	// A self-spoof warning still blocks: the user's own domain, no pass.
	own := senderwarn.NewIndex([]string{"mail.example.net"}, nil)
	spoof := trustSender("boss@mail.example.net", noPass)
	if got := senderChecks(spoof, own, trusted).Refusal; got != placement.SenderWarned {
		t.Fatalf("trusted self-spoof: refusal %q, want warned", got)
	}

	// An ambiguous From still blocks.
	amb := placement.NewSender("robot@example.org",
		placement.FromHeaders{Fields: 2, Raw: []string{"robot@example.org", "x@example.org"}, Parsed: []string{"robot@example.org"}},
		func(string) placement.SenderAuth { return noPass })
	if got := senderChecks(amb, senderwarn.NewIndex(nil, nil), trusted).Refusal; got != placement.AmbiguousFrom {
		t.Fatalf("trusted ambiguous From: refusal %q, want ambiguous", got)
	}

	// A clear authentication FAILURE still blocks: trust waives a missing pass.
	fail := trustSender("robot@example.org", placement.SenderAuth{TrustConfigured: true, AuthFail: true})
	if got := senderChecks(fail, senderwarn.NewIndex(nil, nil), trusted).Refusal; got != placement.Unauthenticated {
		t.Fatalf("trusted with a clear failure: refusal %q, want unauthenticated", got)
	}
}

// SECURITY FIX 1: trust waives the absence of a trusted pass, never the
// presence of a trusted failure.
func TestSenderChecksTrustRefusedOnATrustedFailure(t *testing.T) {
	ix := senderwarn.NewIndex(nil, nil)
	trusted := map[string]bool{"robot@example.org": true}
	// AuthFail false: the shape a forged untrusted pass leaves behind, while the
	// trusted header says dmarc=fail.
	s := trustSender("robot@example.org", placement.SenderAuth{TrustConfigured: true, TrustedFail: true})
	if got := senderChecks(s, ix, trusted).Refusal; got != placement.Unauthenticated {
		t.Fatalf("trusted sender, trusted dmarc=fail, AuthFail false: refusal %q, want unauthenticated", got)
	}
	// Control: without TrustedFail the waiver applies.
	s = trustSender("robot@example.org", placement.SenderAuth{TrustConfigured: true})
	if got := senderChecks(s, ix, trusted).Refusal; got != "" {
		t.Fatalf("control: refusal %q, want none", got)
	}
}

// SECURITY FIX 2: an unaligned affirmative pass must not switch the self-spoof
// warning off while DMARC is being waived.
func TestSenderChecksTrustedOwnDomainStillWarnsWithAnUnalignedPass(t *testing.T) {
	own := senderwarn.NewIndex([]string{"mail.example.net"}, nil)
	trusted := map[string]bool{"boss@mail.example.net": true}
	s := trustSender("boss@mail.example.net", placement.SenderAuth{TrustConfigured: true, AffirmativePass: true, DMARCPass: false})
	c := senderChecks(s, own, trusted)
	if c.Refusal != placement.SenderWarned {
		t.Fatalf("trusted own-domain address with an unaligned pass: refusal %q (warnings %v), want warned", c.Refusal, c.Warnings)
	}
	// Control: a real aligned pass legitimately suppresses it.
	ok := trustSender("boss@mail.example.net", placement.SenderAuth{TrustConfigured: true, AffirmativePass: true, DMARCPass: true})
	if got := senderChecks(ok, own, trusted).Refusal; got != "" {
		t.Fatalf("control with an aligned pass: refusal %q, want none", got)
	}
}

// SECURITY FIX 3: trust is ASCII only and matched exactly.
func TestTrustIsASCIIOnly(t *testing.T) {
	if _, ok := normaliseTrustAddress("ké@example.org"); ok {
		t.Fatal("a non-ASCII address was accepted for trust")
	}
	if _, ok := normaliseTrustAddress("Kevin@example.org"); ok { // KELVIN SIGN
		t.Fatal("a KELVIN SIGN address was accepted for trust")
	}
	if k, ok := normaliseTrustAddress("  Kevin@Example.org "); !ok || k != "kevin@example.org" {
		t.Fatalf("control: %q %v", k, ok)
	}
	ix := senderwarn.NewIndex(nil, nil)
	trusted := map[string]bool{"kevin@example.org": true}
	good := trustSender("Kevin@example.org", noPass)
	if c := senderChecks(good, ix, trusted); !c.Trusted || c.Refusal != "" {
		t.Fatalf("control: ASCII sender with a differing case = %+v, want trusted", c)
	}
	spoof := trustSender("Kevin@example.org", noPass)
	if c := senderChecks(spoof, ix, trusted); c.Trusted || c.Refusal != placement.Unauthenticated {
		t.Fatalf("KELVIN SIGN sender = %+v, want not trusted and unauthenticated", c)
	}
}
