// SPDX-License-Identifier: AGPL-3.0-only

package ml

import "testing"

// Only the topmost TRUSTED header is marked as the one read, an untrusted
// header above it is shown but not trusted, and the answer agrees with
// TopmostTrustedAuthResults (#840).
func TestExplainAuthHeadersAgreesWithTheTrustedParse(t *testing.T) {
	values := []string{
		"evil.example; dmarc=pass header.from=bank.example",
		"mx.trusted.example; dmarc=pass header.from=mail.bank.example",
		"mx.trusted.example; dmarc=pass header.from=bank.example",
	}
	hs := ExplainAuthHeaders(values, []string{"mx.trusted.example"})
	if len(hs) != 3 {
		t.Fatalf("headers = %d, want 3", len(hs))
	}
	if hs[0].Trusted || hs[0].Topmost {
		t.Errorf("untrusted header = %+v, want ignored", hs[0])
	}
	if !hs[1].Topmost || hs[2].Topmost || !hs[2].Trusted {
		t.Errorf("topmost flags = %v %v, want only the first trusted header", hs[1].Topmost, hs[2].Topmost)
	}
	if len(hs[1].Clauses) != 1 || hs[1].Clauses[0].Props["header.from"][0] != "mail.bank.example" {
		t.Errorf("clauses = %+v, want the dmarc clause with its header.from", hs[1].Clauses)
	}
	top := TopmostTrustedAuthResults(values, []string{"mx.trusted.example"})
	if !top.DMARCPassFor("mail.bank.example", DMARCStrict) || top.DMARCPassFor("bank.example", DMARCRelaxed) {
		t.Fatal("fixture: the decision reads the topmost trusted header only")
	}
}
