// SPDX-License-Identifier: AGPL-3.0-only

package kind

import "testing"

// isVERP's same-org check is an EARLY RETURN, so a domain wrongly read as
// same-org silently suppresses the signal rather than producing a false one.
// That makes it the quiet kind of bug: nothing looks wrong, a signal just never
// fires (#175).

func TestVERPRequiresALabelBoundaryNotAStringSuffix(t *testing.T) {
	// The bug: strings.HasSuffix("notexample.com", "example.com") is true, so an
	// unrelated organisation whose domain merely ENDS with the From domain read
	// as the sender's own infrastructure.
	if !isVERP("<bounces-0000000000001234abcd@notexample.com>", "example.com") {
		t.Error("a Return-Path at an UNRELATED domain that merely ends with the From domain was treated as same-org")
	}
}

func TestVERPIgnoresTheSendersOwnInfrastructure(t *testing.T) {
	// A real subdomain of the From domain is the sender's own bounce handling,
	// not third-party infrastructure, and must NOT read as VERP.
	for _, rp := range []string{
		"<bounces-0000000000001234abcd@example.com>",
		"<bounces-0000000000001234abcd@bounce.example.com>",
	} {
		if isVERP(rp, "example.com") {
			t.Errorf("%s is the sender's own domain or a subdomain of it; it must not read as VERP", rp)
		}
	}
}

func TestVERPNeedsAnOpaqueLocalPart(t *testing.T) {
	// A third-party domain alone is not VERP — plenty of legitimate mail is
	// relayed. The opaque, per-recipient local part is the actual signal.
	if isVERP("<hello@sendgrid.net>", "example.com") {
		t.Error("a short, readable local part at a third-party domain read as VERP")
	}
	if !isVERP("<bounce-deadbeef0123456@sendgrid.net>", "example.com") {
		t.Error("a hex-run local part at a third-party domain did not read as VERP")
	}
}

func TestSameOrSubdomain(t *testing.T) {
	cases := []struct {
		domain, parent string
		want           bool
	}{
		{"example.com", "example.com", true},
		{"bounce.example.com", "example.com", true},
		{"a.b.example.com", "example.com", true},
		{"notexample.com", "example.com", false}, // the bug
		{"example.com.evil.test", "example.com", false},
		{"example.co", "example.com", false},
	}
	for _, c := range cases {
		if got := sameOrSubdomain(c.domain, c.parent); got != c.want {
			t.Errorf("sameOrSubdomain(%q, %q) = %v, want %v", c.domain, c.parent, got, c.want)
		}
	}
}

// The lexicon gaps that mattered once SubjectClasses became a classifier
// feature: Amazon says "Ordered", carriers say "Out for delivery" (#175, #169).
func TestTransactionalLexiconCatchesTheCommonSurfaceForms(t *testing.T) {
	has := func(cs []SubjectClass, want SubjectClass) bool {
		for _, c := range cs {
			if c == want {
				return true
			}
		}
		return false
	}
	for _, subject := range []string{
		"Ordered: Alphasmart Neo",
		"OUT FOR DELIVERY: VGC- Alphasmart Neo",
		"Your deliveries for today",
		"Your order has shipped",
	} {
		if !has(SubjectClasses(subject), SubjectTransactional) {
			t.Errorf("SubjectClasses(%q) did not read as transactional", subject)
		}
	}
}
