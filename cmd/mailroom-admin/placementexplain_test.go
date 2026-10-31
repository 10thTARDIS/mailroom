// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/ml"
	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// The printer shows the whole trace in order and ends with the outcome and the
// first check that refused, including header.from against the key domain.
func TestPrintPlacementExplainUnauthenticated(t *testing.T) {
	updates := ulid.Make()
	names := map[ulid.ULID]string{updates: "Updates"}
	addr := placement.Key{Kind: placement.KeyAddress, Value: "alerts@mail.bank.example"}
	dom := placement.Key{Kind: placement.KeyDomain, Value: "mail.bank.example"}
	ev := placement.Evidence{Filings: map[ulid.ULID]int{updates: 2}, Addresses: 1}
	tr := store.LearnedTrace{
		Enabled: true, KeysOK: true, Address: addr, Domain: dom,
		Decided: true,
		Evidence: store.LearnedEvidence{Address: ev, Domain: placement.Evidence{},
			AddressVerdict: placement.Judge(placement.KeyAddress, ev)},
		Decision: placement.Decision{Act: true, Key: addr, Area: updates, Reason: placement.Confident},
		Checks: &store.SenderChecks{
			Auth:    placement.SenderAuth{TrustConfigured: true},
			Refusal: placement.Unauthenticated,
		},
		Result:       store.LearnedPlacementResult{Reason: placement.Unauthenticated, Key: addr, AreaID: &updates},
		FirstRefusal: "sender check: unauthenticated",
	}
	ex := ingest.LearnedExplanation{
		Facts: ingest.SenderFacts{
			StoredFrom: addr.Value, FromFields: 1, FromValues: []string{addr.Value}, Mailboxes: []string{addr.Value},
			TrustedIDs: []string{"mx.trusted.example"},
			AuthHeaders: ml.ExplainAuthHeaders(
				[]string{"mx.trusted.example; dmarc=pass header.from=attacker.example"}, []string{"mx.trusted.example"}),
		},
		Trace: tr,
	}
	var buf bytes.Buffer
	printPlacementExplain(&buf, []explainedMessage{{
		Cand: store.ExplainCandidate{CreatedAt: time.Unix(1_700_000_000, 0)}, Exp: ex,
	}}, names)
	out := buf.String()

	want := []string{
		"evidence: as of NOW",
		"From header fields: 1", "mailboxes parsed: 1",
		`parsed address: "alerts@mail.bank.example" · domain: "mail.bank.example"`,
		`address key: "alerts@mail.bank.example"`,
		"Updates 2",
		`act: address key "alerts@mail.bank.example" -> area Updates`,
		`trusted authserv-ids for this message: "mx.trusted.example"`,
		"TRUSTED, topmost: the one header read",
		`dmarc="pass" · header.from="attacker.example" matches the key domain: no (key domain "mail.bank.example")`,
		"first sender-check refusal: unauthenticated",
		"outcome: unauthenticated",
		"first check that refused: sender check: unauthenticated",
	}
	pos := 0
	for _, w := range want {
		i := strings.Index(out[pos:], w)
		if i < 0 {
			t.Fatalf("output lacks %q (in order) after offset %d:\n%s", w, pos, out)
		}
		pos += i
	}
}

// Mail-derived values reach the operator's terminal only quoted: a crafted
// From, authserv-id, header.from or clause method carrying ESC [2J, BEL and a
// bidi override must not appear as raw bytes (the security review of #840).
func TestPrintPlacementExplainEscapesMailControlledValues(t *testing.T) {
	const evil = "\x1b[2J\a\u202e"
	updates := ulid.Make()
	names := map[ulid.ULID]string{updates: "Updates"}
	addr := placement.Key{Kind: placement.KeyAddress, Value: "a" + evil + "@x" + evil + ".example"}
	dom := placement.Key{Kind: placement.KeyDomain, Value: "x" + evil + ".example"}
	ev := placement.Evidence{Filings: map[ulid.ULID]int{updates: 2}, Addresses: 1}
	authHeader := "mx" + evil + ".example; dmarc" + evil + "=pass" + evil + " header.from=" + evil + "bank.example"
	ex := ingest.LearnedExplanation{
		Facts: ingest.SenderFacts{
			StoredFrom: addr.Value, FromFields: 1, FromValues: []string{addr.Value}, Mailboxes: []string{addr.Value},
			TrustedIDs:  []string{"mx" + evil + ".example"},
			AuthHeaders: ml.ExplainAuthHeaders([]string{authHeader}, []string{"mx" + evil + ".example"}),
		},
		Trace: store.LearnedTrace{
			Enabled: true, KeysOK: true, Address: addr, Domain: dom, Decided: true,
			Evidence: store.LearnedEvidence{Address: ev, AddressVerdict: placement.Judge(placement.KeyAddress, ev)},
			Decision: placement.Decision{Act: true, Key: addr, Area: updates, Reason: placement.Confident},
			Checks:   &store.SenderChecks{Auth: placement.SenderAuth{TrustConfigured: true}, Refusal: placement.Unauthenticated},
			Result:   store.LearnedPlacementResult{Reason: placement.Unauthenticated, Key: addr, AreaID: &updates},
		},
	}
	var buf bytes.Buffer
	printPlacementExplain(&buf, []explainedMessage{{Exp: ex}}, names)
	out := buf.String()
	// Negative control: the fixture really carries the bytes into the printer
	// (the header parses to a clause whose header.from holds them), so a
	// printer that skipped mq would emit them.
	if !strings.Contains(authHeader, "\x1b") || len(ex.Facts.AuthHeaders[0].Clauses) == 0 {
		t.Fatalf("fixture: the hostile header must parse to clauses: %+v", ex.Facts.AuthHeaders)
	}
	for _, bad := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains the raw byte sequence %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `\x1b`) || !strings.Contains(out, `\u202e`) {
		t.Errorf("the escapes should appear visibly quoted:\n%s", out)
	}
}

// A subdomain sender authenticated by a pass for its organisational domain
// (#846): both the decision's line and the clause's line say it was an
// org-domain match, not an exact one, so an operator reading the output can
// tell the relaxed rule acted.
//
// NEGATIVE CONTROL: with matchLabel returning "yes (exact match)" for
// DMARCOrgDomainMatch, or the clause line comparing the normalised names for
// equality as it did before #846, this goes red.
func TestPrintPlacementExplainOrgDomainMatch(t *testing.T) {
	updates := ulid.Make()
	names := map[ulid.ULID]string{updates: "Updates"}
	addr := placement.Key{Kind: placement.KeyAddress, Value: "alerts@news.bank.example"}
	dom := placement.Key{Kind: placement.KeyDomain, Value: "news.bank.example"}
	ev := placement.Evidence{Filings: map[ulid.ULID]int{updates: 2}, Addresses: 1}
	headers := []string{
		"mx.trusted.example; dkim=pass header.d=bank.example; dmarc=pass (policy=reject) header.from=bank.example",
		"mx.trusted.example; dmarc=pass header.from=news.bank.example",
	}
	ex := ingest.LearnedExplanation{
		Facts: ingest.SenderFacts{
			StoredFrom: addr.Value, FromFields: 1, FromValues: []string{addr.Value}, Mailboxes: []string{addr.Value},
			TrustedIDs:  []string{"mx.trusted.example"},
			AuthHeaders: ml.ExplainAuthHeaders(headers[:1], []string{"mx.trusted.example"}),
		},
		Trace: store.LearnedTrace{
			Enabled: true, KeysOK: true, Address: addr, Domain: dom, Decided: true,
			Evidence: store.LearnedEvidence{Address: ev, AddressVerdict: placement.Judge(placement.KeyAddress, ev)},
			Decision: placement.Decision{Act: true, Key: addr, Area: updates, Reason: placement.Confident},
			Checks: &store.SenderChecks{
				Auth:          placement.SenderAuth{TrustConfigured: true, DMARCPass: true, DMARCMatch: string(ml.DMARCOrgDomainMatch)},
				Authenticated: true,
			},
			Result: store.LearnedPlacementResult{Placed: true, Key: addr, AreaID: &updates},
		},
	}
	var buf bytes.Buffer
	printPlacementExplain(&buf, []explainedMessage{{Exp: ex}}, names)
	out := buf.String()
	for _, w := range []string{
		"trusted DMARC pass bound to the key domain: yes (org-domain match)",
		`header.from="bank.example" matches the key domain: yes (org-domain match: header.from is the key domain's organisational domain) (key domain "news.bank.example")`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "exact match") {
		t.Errorf("an org-domain match was printed as exact:\n%s", out)
	}

	// Positive control for the wording: an exact pass says so.
	ex.Facts.AuthHeaders = ml.ExplainAuthHeaders(headers[1:], []string{"mx.trusted.example"})
	ex.Trace.Checks.Auth.DMARCMatch = string(ml.DMARCExactMatch)
	buf.Reset()
	printPlacementExplain(&buf, []explainedMessage{{Exp: ex}}, names)
	for _, w := range []string{
		"trusted DMARC pass bound to the key domain: yes (exact match)",
		`header.from="news.bank.example" matches the key domain: yes (exact match)`,
	} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("exact case lacks %q:\n%s", w, buf.String())
		}
	}
}
