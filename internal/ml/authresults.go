// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"strings"
)

// This file is the message-authentication signal feeding the cold-start scorer
// (spec §8, mailroom#36): a small, conservative parser over the RFC 8601
// Authentication-Results header that decides whether a message's authentication
// CLEARLY failed. It is the source of the `authfail:1` feature that lets
// "auth-failing bulk sinks" (spec §8's cold-start line).
//
// It lives here (the ml feature layer) rather than in ingest so that the ONE
// definition of "auth failed" is shared: ingest fills MessageFacts.AuthFail from
// the parsed header on the serve path via AuthResultsFail. (The replay path
// cannot see this header — it lives in the message blob, not on app.messages —
// so it leaves AuthFail=false; the same documented facts-source asymmetry as
// List-Id / List-Unsubscribe / To+Cc-count. See MessageFacts.)

// Method results are read by the resinfo tokenizer in authresinfo.go: one
// tokenizer for every caller (#714 replaced the regex that used to live here,
// see that file for why).

// AuthResultsFail reports whether a message's authentication CLEARLY failed,
// given all of its Authentication-Results header values (RFC 8601). It is
// deliberately CONSERVATIVE — it returns true only on an unambiguous failure —
// because a legitimate personal mail server may add no auth headers at all, and
// penalizing mail that simply lacks authentication would sink real correspondence.
//
// The rule (spec §8's "auth-failing bulk sinks"):
//
//   - DMARC=fail is a failure (DMARC is the aligned, policy-level verdict), OR
//   - SPF=fail AND DKIM=fail together (neither path authenticated the sender).
//
// A method is treated as failing only when it has a `fail` result AND no `pass`
// result across all headers — so a message with one passing DKIM signature and
// one failing one is NOT counted as DKIM-failed (multi-signature is normal). The
// ABSENCE of a header, or a result of none / neutral / softfail / permerror /
// temperror / policy, is never a failure: only an explicit `fail` counts.
func AuthResultsFail(values []string) bool {
	return AuthResultsOf(values).ClearlyFailed()
}

// AuthMethods are the three methods this parser models, in the order a report
// should present them.
var AuthMethods = []string{"spf", "dkim", "dmarc"}

// AuthResults is every (method, result) pair found across a message's
// Authentication-Results headers, with the result keywords LOWERCASED and
// de-duplicated. A method may hold several results at once: multi-signature DKIM
// is ordinary, and one passing plus one failing signature is a real and common
// shape rather than a contradiction.
//
// ⚠ IT EXISTS SO THE CORPUS REPORT MEASURES THE SHIPPED PARSER (#55 increment 0).
// The alternative — a second regex in cmd/ that re-reads the same header — would
// be a measurement of code written to agree with the same assumptions, which is
// how this project spent a month judging a classifier on numbers from a broken
// evaluation (#169, #220). AuthResultsFail is now defined in terms of this, so
// the report and the scorer's signal cannot disagree about what a header says.
type AuthResults struct {
	// Present is false when no method token appeared at all — the population
	// AuthResultsFail deliberately treats as "not a failure" and which a badge
	// most risks misrepresenting as "verified".
	Present bool
	// Results maps method -> set of result keywords seen for it, for the
	// three methods modelled (AuthMethods).
	Results map[string]map[string]bool
	// Clauses is every result clause, every method, in header order, each
	// with its OWN properties (#714): what DMARCPassFor reads.
	Clauses []ResultClause
}

// AuthResultsOf parses all of a message's Authentication-Results header values.
func AuthResultsOf(values []string) AuthResults {
	a := AuthResults{Results: map[string]map[string]bool{}}
	for _, v := range values {
		for _, c := range parseResinfo(v) {
			a.Clauses = append(a.Clauses, c)
			if c.Method != "spf" && c.Method != "dkim" && c.Method != "dmarc" {
				continue
			}
			if c.Result == "" {
				continue
			}
			if a.Results[c.Method] == nil {
				a.Results[c.Method] = map[string]bool{}
			}
			a.Results[c.Method][c.Result] = true
			a.Present = true
		}
	}
	return a
}

// Has reports whether method carried result anywhere across the headers.
func (a AuthResults) Has(method, result string) bool { return a.Results[method][result] }

// Failed reports whether a METHOD failed by this parser's rule: it has a `fail`
// result and no `pass` result anywhere. One passing DKIM signature alongside a
// failing one is therefore not a DKIM failure.
func (a AuthResults) Failed(method string) bool {
	return a.Has(method, "fail") && !a.Has(method, "pass")
}

// Disagrees reports whether a method carried BOTH a pass and a fail — normal for
// multi-signature DKIM, and invisible in the stored boolean. Counted separately
// because "the methods disagreed" and "the method failed" are different things
// to show a reader.
func (a AuthResults) Disagrees(method string) bool {
	return a.Has(method, "fail") && a.Has(method, "pass")
}

// ClearlyFailed is the rule documented on AuthResultsFail, and is its whole
// implementation. Kept as a method so the report can show the verdict beside the
// per-method results it was computed from, rather than beside a second opinion.
func (a AuthResults) ClearlyFailed() bool {
	return a.Failed("dmarc") || (a.Failed("spf") && a.Failed("dkim"))
}

// AffirmativelyPassed reports DKIM or DMARC saying `pass`: the notion the
// selfSpoof sender warning is suppressed by (internal/api/sender_warnings.go).
// Moved here from internal/api (#714) so learned placement asks the warning
// the SAME question the reading pane does, rather than a copy of it.
//
// ⚠ SPF IS DELIBERATELY NOT ENOUGH ON ITS OWN. SPF authenticates the ENVELOPE
// sender, not the From header a reader sees, so a message can pass SPF for the
// attacker's own bounce domain while the From says something else entirely.
// DKIM and DMARC are the two that say something about the visible From.
//
// ⚠ Meaningful for a security decision only on a TrustedAuthResults parse.
func (a AuthResults) AffirmativelyPassed() bool {
	return a.Has("dkim", "pass") || a.Has("dmarc", "pass")
}

// ── RFC 8601 §5: whose verdict is this? (#414) ──────────────────────────────
//
// ⚠ EVERYTHING ABOVE TRUSTS ANY Authentication-Results HEADER, AND THAT IS
// CORRECT FOR WHAT IT WAS BUILT FOR AND WRONG FOR WHAT v1.61.0 ADDED. This
// parser's original job is a CONSERVATIVE FAILURE signal for the cold-start
// scorer: it answers "did authentication clearly fail", and a forged header
// saying `pass` makes that answer *less* likely to fire, which for a
// classification feature is noise rather than an exploit.
//
// #397 made a SECURITY control depend on the same parser. `selfSpoof` — a
// stranger claiming one of your own domains — is suppressed when DKIM or DMARC
// affirmatively passed. So the attacker writes
//
//	Authentication-Results: x.example; dkim=pass
//
// into their own message. RFC 8601 §5 requires an MTA to strip only headers
// bearing ITS OWN authserv-id, so a header claiming any other id travels through
// an ordinary MX untouched. The real MX appends its genuine verdict; this parser
// sees a `pass` somewhere and reports one. **The most serious of the three
// warnings is opt-out, for the attacker, at the cost of one header line.**
//
// ⚠ THE FIX IS NOT "TRUST THE LAST HEADER". Header order is not a trust
// boundary: a sender can append trailing headers and intermediate hops
// legitimately prepend. The only thing RFC 8601 offers is the authserv-id, so a
// verdict counts only when it is stamped by an identity the operator names.

// TrustedAuthResults parses only those Authentication-Results values whose
// authserv-id is in `trusted`, for use in a SECURITY decision.
//
// ⚠ AN EMPTY `trusted` YIELDS AN EMPTY RESULT, DELIBERATELY. If the operator has
// not said whose verdict to believe, there is no way to tell a genuine pass from
// one the sender wrote, and the honest answer is that nothing has been
// authenticated. That direction is FAIL-SAFE for the caller this exists for:
// `selfSpoof` then fires on own-domain mail it would otherwise have suppressed,
// so a misconfiguration produces a warning rather than silence.
//
// Measured before choosing that default, on a real corpus: the few
// messages from the user's own domain are mostly locally authored (where
// selfSpoof cannot fire at all), and the rest are exactly the messages
// selfSpoof already fires on. So NONE of them was being suppressed by an auth
// pass, and distrusting unverified headers costs zero additional warnings here.
//
// ⚠ A SECOND CALLER JOINED IN #625: the `authResults` Email property shown to
// the reader was, until then, built from the permissive parse above and could
// be silenced by the same forged-pass trick that motivated this function for
// selfSpoof. It now reads this instead, so an empty `trusted` makes that
// notice read as "no authentication information" rather than trust a header
// the sender could have written — the mirror image of selfSpoof's fail-safe
// (there, empty `trusted` means MORE warnings; here, it means NONE), and both
// are the honest answer for a caller that cannot tell a real verdict from a
// forged one.
//
// ⚠ ONLY THE TOPMOST TRUSTED ID SPEAKS (#766). Of the trusted headers, only
// those bearing the SAME authserv-id as the topmost trusted one are kept: the
// receiving provider may split its verdicts over several headers with its own
// id, but a header with a DIFFERENT trusted id below it is not the provider's
// word on this delivery. That matters as soon as two ids are trusted at once,
// which date-bounded trust makes routine: mail received while a provider ran
// both its old id (trusted until a date) and its new one carries the new id on
// top, and a sender could write a header with the OLD id below it — §5 strips
// only headers bearing the server's CURRENT id, so it would travel untouched
// and add its `pass` to the genuine verdict. TopmostTrustedAuthResults is
// stricter still (one header) for the decisions that act on a pass.
func TrustedAuthResults(values []string, trusted []string) AuthResults {
	if len(trusted) == 0 {
		return AuthResults{Results: map[string]map[string]bool{}}
	}
	ok := trustedSet(trusted)
	top := ""
	kept := make([]string, 0, len(values))
	for _, v := range values {
		id := authservIDOf(v)
		if !ok[id] {
			continue
		}
		if top == "" {
			top = id
		}
		if id == top {
			kept = append(kept, v)
		}
	}
	return AuthResultsOf(kept)
}

// authservIDOf extracts the authserv-id: RFC 8601 §2.2 puts it first in the
// header value, before the first ';', optionally followed by a version number.
//
// ⚠ IT DOES NOT VALIDATE THE REST OF THE HEADER. This is a trust decision about
// WHO is speaking, made before anything they said is read — deliberately the
// same shape as the quarantine gate running before the bytes are fetched.
//
// ⚠ EXACT, AND STRICT ABOUT WHAT MAY FOLLOW (#714). Comments are removed
// first. After the id, only an RFC 8601 version (digits) may follow; any
// other token ("mx.provider.example evil", "mx.provider.example x;") makes
// the header nobody's, so it can never be read as the provider's. Matching is
// whole-string, case-insensitive: no prefix, suffix or substring match.
func authservIDOf(v string) string {
	head := splitUnquoted(stripComments(v), ';')[0]
	f := strings.Fields(head)
	switch {
	case len(f) == 0:
		return ""
	case len(f) == 1:
		return normalizeAuthservID(f[0])
	case len(f) == 2 && allDigits(f[1]):
		return normalizeAuthservID(f[0])
	default:
		return ""
	}
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// normalizeAuthservID lowercases and trims, so configuration and header text
// compare the same way a domain name would.
// AuthservIDOf is authservIDOf, exported for `mailroom-admin auth-report
// --authserv` (v1.90.1): the report must name the authserv-ids exactly as
// TrustedAuthResults and TopmostTrustedAuthResults read them, not by a second
// parse that could disagree. "" means the header's authserv-id is unreadable.
func AuthservIDOf(headerValue string) string { return authservIDOf(headerValue) }

// IsTrustedAuthservID reports whether id (as AuthservIDOf returns it) is one
// of trusted, under the same normalisation the trusted parses use.
func IsTrustedAuthservID(id string, trusted []string) bool {
	return id != "" && trustedSet(trusted)[normalizeAuthservID(id)]
}

func normalizeAuthservID(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
