// SPDX-License-Identifier: AGPL-3.0-only

// Package alias derives which of the user's addresses a message was delivered to
// — i.e. which address a reply should come FROM (spec §5, with the
// forwarded-mail caveat from §4).
//
// It is a LEAF package on purpose: it depends only on internal/mime, so the
// ingest path (which runs the derivation on every message that arrives) does not
// drag in the send path's dependencies, and so the derivation stays a pure,
// exhaustively testable function over headers with no database and no network.
package alias

import (
	"net/mail"
	"strings"

	"mailroom/internal/mime"
)

// This file implements THE ALIAS DERIVATION (spec §5, with the forwarded-mail
// caveat from §4). It answers one question: which of the user's addresses was
// this message actually delivered to — i.e. which address should a reply come
// FROM?
//
// It matters because the primary domain is a CATCHALL (spec §5): arbitrary
// per-site addresses all land in one mailbox and there is no finite alias list
// to enumerate, so the identity is DERIVED FROM THE RECEIVED MESSAGE, not looked
// up in a configured list. Whatever address a site used IS the reply-from
// address.
//
// Precedence (spec §5: "Delivered-To / X-Original-To / envelope -> matched
// To/Cc; header-analysis fallback for forwarded mail"):
//
//	1. Envelope trace headers — X-Original-To, Envelope-To, then Delivered-To.
//	   These are what the receiving MDA stamped with the actual RCPT TO, so they
//	   are the truth about where the message landed... UNLESS the message was
//	   forwarded (2). On the ORDER WITHIN THIS GROUP, see envelopeCandidates: the
//	   pre-rewrite headers beat Delivered-To, because under a catchall
//	   Delivered-To names the MAILBOX rather than the address the sender used.
//
//	2. FORWARDED MAIL (spec §4's explicit caveat: "the current Gmail->Migadu
//	   forward rewrites the envelope recipient, so alias derivation for forwarded
//	   mail must fall back to header analysis rather than the envelope"). When a
//	   forwarder rewrote the envelope, Delivered-To names the FORWARDING
//	   DESTINATION (the mailbox the forward dumps into), not the address the
//	   sender used — so trusting it would reply from the wrong alias and defeat
//	   the whole per-site alias scheme. When forwarding evidence is present, the
//	   envelope trace headers are DEMOTED below header analysis:
//	   X-Forwarded-To, then the original To/Cc.
//
//	3. To/Cc — the addresses the sender actually typed. The last resort before
//	   giving up, and the only signal that survives a forwarder that strips
//	   everything else.
//
// Every candidate is a CANDIDATE ONLY. The caller (store.MatchVerifiedAddress)
// takes the first one that resolves to a verified identity of THIS user; an
// address we cannot send as is never chosen, no matter how high it ranks. That
// keeps the derivation (a heuristic over attacker-supplied headers) strictly
// separated from the authorization (a database fact) — a hostile sender can put
// anything in Delivered-To and still only ever move us between addresses we
// were already allowed to send as.

// Source labels why a candidate address was proposed. It is carried for
// diagnosability (and for the compose UI's "we picked this because…").
type Source string

const (
	// SourceDeliveredTo is the Delivered-To trace header (the MDA's RCPT TO).
	SourceDeliveredTo Source = "delivered-to"
	// SourceOriginalTo is X-Original-To (Postfix's pre-rewrite recipient).
	SourceOriginalTo Source = "x-original-to"
	// SourceForwardedTo is X-Forwarded-To (the address a forwarder relayed for).
	SourceForwardedTo Source = "x-forwarded-to"
	// SourceTo is an address from the To header.
	SourceTo Source = "to"
	// SourceCc is an address from the Cc header.
	SourceCc Source = "cc"
	// SourceEnvelopeTo is X-Envelope-To / Envelope-To (some MDAs stamp this).
	SourceEnvelopeTo Source = "envelope-to"
)

// Candidate is one proposed reply-from address with the header it came from.
type Candidate struct {
	Address string
	Source  Source
}

// forwardHeaders are the headers whose mere presence means "a forwarder handled
// this message, so the envelope recipient has probably been rewritten" (spec §4).
var forwardHeaders = []string{
	"X-Forwarded-To",
	"X-Forwarded-For",
	"X-Forwarded-Message-Id",
	"Resent-To",
}

// DeriveFrom returns the ordered reply-from candidates for a received message,
// highest precedence first, de-duplicated. The caller picks the first candidate
// that is a verified identity of the user (store.MatchVerifiedAddress); if none
// is, it falls back to the account's default identity.
//
// forwarded reports whether forwarding evidence demoted the envelope headers —
// exposed so the caller can log/surface WHY an alias was chosen.
func DeriveFrom(hdr mime.Header) (candidates []Candidate, forwarded bool) {
	envelope := envelopeCandidates(hdr)
	analysis := headerAnalysisCandidates(hdr)
	forwarded = isForwarded(hdr)

	var ordered []Candidate
	if forwarded {
		// The forwarder rewrote the envelope: header analysis outranks it (§4).
		ordered = append(ordered, analysis...)
		ordered = append(ordered, envelope...)
	} else {
		ordered = append(ordered, envelope...)
		ordered = append(ordered, analysis...)
	}
	return dedupe(ordered), forwarded
}

// isForwarded reports whether the message carries evidence that a forwarder
// handled it — in which case the envelope recipient is not trustworthy as an
// alias signal (spec §4's Gmail->Migadu caveat).
//
// Two signals:
//   - an explicit forwarding header (X-Forwarded-To and friends);
//   - a Delivered-To CHAIN with more than one hop: each hop prepends its own
//     Delivered-To, so >1 means the message was re-delivered at least once and
//     the topmost (most recent) value is the forwarding destination.
func isForwarded(hdr mime.Header) bool {
	for _, h := range forwardHeaders {
		if len(hdr.Values(h)) > 0 {
			return true
		}
	}
	return len(hdr.Values("Delivered-To")) > 1
}

// envelopeCandidates returns the trace-header candidates, PRE-REWRITE HEADERS
// FIRST.
//
// All three name "the recipient", but not at the same point in delivery, and
// under a catchall they routinely disagree:
//
//	X-Original-To  Postfix's record of the envelope recipient BEFORE alias and
//	               virtual rewriting — i.e. the address the sender actually used.
//	Envelope-To    the same idea from Exim and friends.
//	Delivered-To   stamped by the final MDA: the mailbox the message ended up in,
//	               AFTER the catchall collapsed every alias onto it.
//
// So for a catchall user — the case this package exists to serve (spec §5) —
// Delivered-To is the one header that has had the answer erased from it. Real
// mail from a Migadu catchall carries both, and they differ on
// every message:
//
//	Delivered-To: mailroomtest@example.net     <- the mailbox
//	Envelope-To:  sub@example.net          <- the alias the sender used
//
// Ranking Delivered-To first therefore resolved nearly all messages to the same
// address and made the entire alias story inert (mailroom#177). Preferring the
// pre-rewrite headers is the same principle the Delivered-To chain below already
// applies — prefer the ORIGINAL recipient over the final destination — extended
// from "within one header" to "between headers", which is where a catchall
// actually loses the information.
//
// This reordering CANNOT change the outcome for a user without a catchall:
// candidates are only candidates, and MatchVerifiedAddress takes the first that
// resolves to a verified identity. An alias the user cannot send as is skipped
// however high it ranks, so a single-fixed-identity user still lands on exactly
// the address they landed on before (TestSingleIdentityUnaffectedByOrdering).
//
// Delivered-To is itself read OLDEST-FIRST (the bottom of the chain). Each hop
// PREPENDS its own Delivered-To, so the last value in the header list is the
// original delivery — the address the sender actually reached — while the first
// is the final forwarding destination.
func envelopeCandidates(hdr mime.Header) []Candidate {
	var out []Candidate
	for _, v := range hdr.Values("X-Original-To") {
		if a := parseAddress(v); a != "" {
			out = append(out, Candidate{Address: a, Source: SourceOriginalTo})
		}
	}
	for _, key := range []string{"X-Envelope-To", "Envelope-To"} {
		for _, v := range hdr.Values(key) {
			if a := parseAddress(v); a != "" {
				out = append(out, Candidate{Address: a, Source: SourceEnvelopeTo})
			}
		}
	}
	deliveredTo := hdr.Values("Delivered-To")
	for i := len(deliveredTo) - 1; i >= 0; i-- {
		if a := parseAddress(deliveredTo[i]); a != "" {
			out = append(out, Candidate{Address: a, Source: SourceDeliveredTo})
		}
	}
	return out
}

// headerAnalysisCandidates returns the candidates that survive a forwarder that
// rewrote the envelope: the forwarder's own record of what it relayed for, and
// then the addresses the sender actually typed (spec §4).
func headerAnalysisCandidates(hdr mime.Header) []Candidate {
	var out []Candidate
	for _, v := range hdr.Values("X-Forwarded-To") {
		if a := parseAddress(v); a != "" {
			out = append(out, Candidate{Address: a, Source: SourceForwardedTo})
		}
	}
	for _, a := range hdr.To() {
		if addr := normalize(a.Address); addr != "" {
			out = append(out, Candidate{Address: addr, Source: SourceTo})
		}
	}
	for _, a := range hdr.Cc() {
		if addr := normalize(a.Address); addr != "" {
			out = append(out, Candidate{Address: addr, Source: SourceCc})
		}
	}
	return out
}

// parseAddress extracts a bare address from a trace header value, which may be
// a plain address ("you@example.com"), an angle-addr ("<you@example.com>"), or
// a full name-addr. Trace headers are not required to be structured address
// headers, so this is deliberately forgiving.
func parseAddress(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if a, err := mail.ParseAddress(v); err == nil {
		return normalize(a.Address)
	}
	// Not a structured address: take the last whitespace-separated token that
	// looks like an address (e.g. "for you@example.com; Tue, 1 Jul 2025 …").
	v = strings.TrimSuffix(v, ";")
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == ';' || r == '<' || r == '>'
	})
	for i := len(fields) - 1; i >= 0; i-- {
		if a := normalize(fields[i]); a != "" {
			return a
		}
	}
	return ""
}

// normalize lowercases and validates a bare address (exactly one "@", non-empty
// local part and domain). It returns "" for anything that is not one.
func normalize(addr string) string {
	a := strings.ToLower(strings.TrimSpace(addr))
	a = strings.Trim(a, "<>")
	a = strings.TrimSpace(a)
	if strings.Count(a, "@") != 1 {
		return ""
	}
	at := strings.Index(a, "@")
	if at <= 0 || at == len(a)-1 {
		return ""
	}
	if strings.ContainsAny(a, " \t\"\\") {
		return ""
	}
	return a
}

// dedupe removes repeated addresses, keeping the first (highest-precedence)
// occurrence of each.
func dedupe(in []Candidate) []Candidate {
	seen := make(map[string]bool, len(in))
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if seen[c.Address] {
			continue
		}
		seen[c.Address] = true
		out = append(out, c)
	}
	return out
}

// Addresses projects candidates to their bare addresses, in order — the form
// store.MatchVerifiedAddress consumes.
func Addresses(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Address
	}
	return out
}
