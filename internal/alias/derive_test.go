// SPDX-License-Identifier: AGPL-3.0-only

package alias

import (
	"strings"
	"testing"

	"mailroom/internal/mime"
)

// The alias-derivation tests ARE the spec §5 acceptance criteria: "default From =
// the exact address the message was delivered to (Delivered-To/X-Original-To/
// envelope → matched To/Cc; header-analysis fallback for forwarded mail)".
//
// The forwarded case is the one that matters most and is easiest to get wrong:
// spec §4 says a Gmail→Migadu forward REWRITES the envelope recipient, so
// trusting Delivered-To there replies from the forwarding mailbox instead of the
// per-site alias the sender actually used — which quietly destroys the whole
// point of the catchall alias scheme.

func parseHeader(t *testing.T, raw string) mime.Header {
	t.Helper()
	msg, err := mime.ParseBytes([]byte(strings.ReplaceAll(raw, "\n", "\r\n")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return msg.Header
}

func TestDeriveFromEnvelopeWins(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: store@example.com
To: undisclosed-recipients:;
Cc: someone-else@example.net
Subject: receipt

body
`)
	cands, forwarded := DeriveFrom(hdr)
	if forwarded {
		t.Fatal("a message with a single Delivered-To and no forwarding header is not forwarded")
	}
	if len(cands) == 0 || cands[0].Address != "store@example.com" {
		t.Fatalf("top candidate = %+v, want store@example.com from the envelope", cands)
	}
	if cands[0].Source != SourceDeliveredTo {
		t.Errorf("source = %q, want %q", cands[0].Source, SourceDeliveredTo)
	}
}

func TestDeriveFromMatchesToWhenNoTraceHeaders(t *testing.T) {
	hdr := parseHeader(t, `To: Shop <shop@example.com>, Someone <other@example.net>
Cc: cc@example.com
Subject: hi

body
`)
	cands, _ := DeriveFrom(hdr)
	got := Addresses(cands)
	want := []string{"shop@example.com", "other@example.net", "cc@example.com"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}

// THE FORWARDED CASE (spec §4). The Gmail→Migadu forward rewrote the envelope,
// so Delivered-To names the forwarding destination (inbox@example.com) and NOT
// the address the sender used (store@example.com). Header analysis must win.
func TestDeriveFromForwardedDemotesEnvelope(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: inbox@example.com
X-Forwarded-To: store@example.com
X-Forwarded-For: store@example.com inbox@example.com
To: store@example.com
Subject: your receipt

body
`)
	cands, forwarded := DeriveFrom(hdr)
	if !forwarded {
		t.Fatal("X-Forwarded-To present: this message must be recognized as forwarded")
	}
	if cands[0].Address != "store@example.com" {
		t.Fatalf("top candidate = %q, want the forwarded-for alias store@example.com, not the forwarding destination", cands[0].Address)
	}
	if cands[0].Source != SourceForwardedTo {
		t.Errorf("source = %q, want %q", cands[0].Source, SourceForwardedTo)
	}
	// The forwarding destination is still a candidate — just a worse one. It has
	// to remain reachable: on an account where only inbox@ is verified, it is the
	// right answer.
	if last := cands[len(cands)-1].Address; last != "inbox@example.com" {
		t.Errorf("the forwarding destination should remain a low-precedence candidate, got %v", Addresses(cands))
	}
}

// A Delivered-To CHAIN (>1 hop) is itself forwarding evidence, even with no
// X-Forwarded-* header: each hop prepends its own, so the topmost value is the
// last delivery and the bottom one is the original.
func TestDeriveFromDeliveredToChain(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: inbox@example.com
Delivered-To: store@example.com
To: store@example.com
Subject: chained

body
`)
	cands, forwarded := DeriveFrom(hdr)
	if !forwarded {
		t.Fatal("a multi-hop Delivered-To chain is forwarding evidence")
	}
	// Header analysis first (To), and within the envelope group the OLDEST
	// Delivered-To (the original delivery) outranks the newest.
	if cands[0].Address != "store@example.com" {
		t.Fatalf("top candidate = %q, want store@example.com", cands[0].Address)
	}
	if got := Addresses(cands); got[len(got)-1] != "inbox@example.com" {
		t.Errorf("candidates = %v, want the forwarding destination last", got)
	}
}

func TestDeriveFromXOriginalTo(t *testing.T) {
	hdr := parseHeader(t, `X-Original-To: newsletter@example.com
To: list@example.net
Subject: news

body
`)
	cands, _ := DeriveFrom(hdr)
	if cands[0].Address != "newsletter@example.com" || cands[0].Source != SourceOriginalTo {
		t.Fatalf("top candidate = %+v, want newsletter@example.com from X-Original-To", cands[0])
	}
}

// Trace headers are not required to be structured address headers; Postfix in
// particular writes "for <you@example.com>" style values.
func TestParseAddressTolerance(t *testing.T) {
	cases := map[string]string{
		"you@example.com":           "you@example.com",
		"<You@Example.COM>":         "you@example.com",
		"Name <you@example.com>":    "you@example.com",
		"for you@example.com;":      "you@example.com",
		"":                          "",
		"not an address":            "",
		"two@example.com x@y@z.com": "two@example.com",
		"  spaced@example.com \t":   "spaced@example.com",
		"undisclosed-recipients:;":  "",
	}
	for in, want := range cases {
		if got := parseAddress(in); got != want {
			t.Errorf("parseAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// A hostile sender controls every header here. Derivation may therefore only
// ever PROPOSE — the caller checks each candidate against the identity store, so
// the worst an attacker can do is reorder addresses the user could already send
// as. This test pins the contract that DeriveFrom performs no authorization.
func TestDeriveFromDoesNotAuthorize(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: victim@other-domain.example
To: you@example.com
Subject: spoof

body
`)
	cands, _ := DeriveFrom(hdr)
	if cands[0].Address != "victim@other-domain.example" {
		t.Fatal("derivation reports what the headers say — it is a proposal, not a decision")
	}
	// ...and store.MatchVerifiedAddress is what refuses it. See
	// TestResolveSendIdentityRejectsUnverified (store, integration).
}

// THE CATCHALL CASE THAT MADE THE ALIAS STORY INERT (mailroom#177).
//
// A catchall provider stamps BOTH headers, and they disagree by design:
// Envelope-To is the address the sender used, Delivered-To is the mailbox the
// catchall collapsed it onto. No test here had ever supplied both, which is how
// Delivered-To's precedence survived — every existing case had exactly one
// envelope header, where the ordering is unobservable.
//
// On a real catch-all mailbox this shape covered nearly all messages, all of
// them resolving to the mailbox address, so `alias` carried no information for
// rules, induction, or a human reading the message.
func TestDeriveFromCatchallPrefersPreRewriteRecipient(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: mailbox@example.net
Envelope-To: sub@example.net
To: sub@example.net
Subject: your membership

body
`)
	cands, forwarded := DeriveFrom(hdr)
	if forwarded {
		t.Fatal("a single Delivered-To with no forwarding header is not forwarded")
	}
	if cands[0].Address != "sub@example.net" {
		t.Fatalf("top candidate = %+v, want sub@example.net — Delivered-To names the\n"+
			"mailbox the catchall routed to, not the address the sender used", cands)
	}
	if cands[0].Source != SourceEnvelopeTo {
		t.Errorf("source = %q, want %q", cands[0].Source, SourceEnvelopeTo)
	}
	// The mailbox address is still PRESENT — it is a legitimate fallback for a
	// user whose alias is not verified. It just must not outrank the alias.
	if last := Addresses(cands)[len(cands)-1]; last != "mailbox@example.net" {
		t.Errorf("candidates = %v, want the mailbox address retained, last", Addresses(cands))
	}
}

// X-Original-To (Postfix's pre-rewrite recipient) beats Delivered-To for the
// same reason Envelope-To does.
func TestDeriveFromOriginalToBeatsDeliveredTo(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: mailbox@example.net
X-Original-To: bank@example.net
Subject: delivery

body
`)
	cands, _ := DeriveFrom(hdr)
	if cands[0].Address != "bank@example.net" || cands[0].Source != SourceOriginalTo {
		t.Fatalf("top candidate = %+v, want bank@example.net from X-Original-To", cands[0])
	}
}

// THE SAFETY PROPERTY THE REORDERING RESTS ON.
//
// Candidates are proposals; store.MatchVerifiedAddress takes the first that
// resolves to a verified identity and skips the rest however high they rank. So
// for a user with ONE fixed identity and no catchall, promoting the pre-rewrite
// headers cannot change the answer: the alias they cannot send as is skipped,
// and they land on the same mailbox address as before.
//
// firstVerified models MatchVerifiedAddress's contract (first candidate in the
// verified set, else none) rather than calling it — this package is a leaf with
// no database by design. The real function is exercised over real identities in
// the store's integration suite.
func TestSingleIdentityUnaffectedByOrdering(t *testing.T) {
	hdr := parseHeader(t, `Delivered-To: mailbox@example.net
Envelope-To: sub@example.net
To: sub@example.net
Subject: your membership

body
`)
	cands, _ := DeriveFrom(hdr)

	firstVerified := func(verified map[string]bool) string {
		for _, c := range cands {
			if verified[c.Address] {
				return c.Address
			}
		}
		return ""
	}

	// No catchall: only the mailbox is a verified send-as address.
	if got := firstVerified(map[string]bool{"mailbox@example.net": true}); got != "mailbox@example.net" {
		t.Errorf("without a catchall the resolved alias = %q, want mailbox@example.net "+
			"(the reordering must be invisible to a single-identity user)", got)
	}
	// With a catchall every address at the domain is verified, and the alias wins.
	catchall := map[string]bool{"mailbox@example.net": true, "sub@example.net": true}
	if got := firstVerified(catchall); got != "sub@example.net" {
		t.Errorf("with a catchall the resolved alias = %q, want sub@example.net", got)
	}
}
