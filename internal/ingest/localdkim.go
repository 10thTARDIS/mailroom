// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"context"
	"strings"
	"time"

	"mailroom/internal/dkimverify"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// LOCAL DKIM AT INGEST (#776, ADR-036).
//
// A message that arrives with NO Authentication-Results header from a trusted
// authserv-id (ml.TrustedAt + ml.TrustedAuthResults) has its DKIM signatures
// verified here, in-process, BEFORE the ingest transaction opens (like the
// malware scan: DNS latency is never held inside a Postgres transaction). The
// verdict is written by ingestNew in the same transaction that creates the
// message (store.SetLocalDKIM), so it rides the message's Email 'created'
// change_log row and appends none of its own.
//
// WHY AT INGEST: the signers that matter use x= expiry, and most
// signatures on the first measurement had expired by the time they
// were checked later. Minutes after receipt they have not.
//
// BOUNDS, all of them reported rather than silent (the metric carries every
// outcome, including the two that store nothing):
//   - each DNS lookup: localDKIMLookupTimeout;
//   - at most localDKIMMaxLookups lookups per message;
//   - the whole check: localDKIMDeadline.
//
// A transient failure (a DNS error or timeout, a spent budget) stores NOTHING:
// "not checked" is the honest state, and there is no retry (no backfill).
//
// A FRESH dkimverify.Verifier PER MESSAGE. Its budget is per Verifier, so a
// fresh one makes "at most 4 lookups per message" structural rather than
// arithmetic. It also means no answer is cached across messages: the Verifier
// caches failures as well as successes for its lifetime, and a shared one would
// turn one transient DNS failure into a permanent "not checked" for a sender.
// The cost is a repeat lookup for a sender whose mail arrives with no trusted
// header in volume (a modest number of messages a month on the measured corpus), which
// is small beside the 3 second bound and not worth a TTL cache's staleness.
const (
	localDKIMLookupTimeout = 3 * time.Second
	localDKIMMaxLookups    = 4
	localDKIMDeadline      = 8 * time.Second
)

// The closed set of metric labels for the check (internal/metrics
// LocalDKIMTotal). The stored results are dkimverify.Status values.
const (
	LocalDKIMSkippedTrusted = "skipped_trusted_header" // a trusted header exists; nothing verified
	LocalDKIMNotChecked     = "not_checked"            // temperror or budget; nothing stored
	LocalDKIMNoSignature    = "no_signature"           // nothing to verify; nothing stored
	LocalDKIMPassUnaligned  = "pass_unaligned"         // stored, never displayed
	// LocalDKIMPassBlocked: an aligned pass that is NOT displayed because a
	// signed header is duplicated (header prepend, RFC 6376 §8.15).
	LocalDKIMPassBlocked = "pass_blocked_duplicate"
	// LocalDKIMPassBlockedUnsigned: an aligned pass that is NOT displayed
	// because a displayed identity header (Reply-To, Cc, ...) is not covered by
	// the signature (header add, RFC 6376 §8.15).
	LocalDKIMPassBlockedUnsigned = "pass_blocked_unsigned"
	LocalDKIMPass                = "pass" // aligned pass, stored and displayed
)

// LocalDKIMResults is every label LocalDKIMTotal can carry, so the server can
// create them at zero at boot (an absent series cannot be told from an unwired
// check).
func LocalDKIMResults() []string {
	return []string{
		LocalDKIMSkippedTrusted, LocalDKIMNotChecked, LocalDKIMNoSignature,
		LocalDKIMPass, LocalDKIMPassUnaligned, LocalDKIMPassBlocked, LocalDKIMPassBlockedUnsigned,
		string(dkimverify.StatusFailBody), string(dkimverify.StatusFailSignature),
		string(dkimverify.StatusKeyNotFound), string(dkimverify.StatusKeyRevoked),
		string(dkimverify.StatusPermError), string(dkimverify.StatusUnsupported),
	}
}

// localDKIM is the Ingestor's local DKIM check; nil disables it.
type localDKIM struct {
	observe func(result string)
	lookup  dkimverify.LookupTXTFunc // nil = the system resolver
}

// WithLocalDKIM enables the local DKIM check at ingest (#776, ADR-036).
// observe, when non-nil, receives one closed label (LocalDKIMResults) per newly
// ingested message that reached the check. Without this option ingest is
// exactly as before: the many non-serving callers (bulk import, the admin and
// drill CLIs, the sync tests) verify nothing and make no DNS lookup.
func WithLocalDKIM(observe func(result string)) Option {
	return func(in *Ingestor) { in.localDKIM = &localDKIM{observe: observe} }
}

// withDKIMLookup replaces the resolver (tests only).
func withDKIMLookup(l dkimverify.LookupTXTFunc) Option {
	return func(in *Ingestor) {
		if in.localDKIM == nil {
			in.localDKIM = &localDKIM{}
		}
		in.localDKIM.lookup = l
	}
}

// localDKIMOutcome is what the check decided for one message: the row to store
// (nil = store nothing) and the metric label.
type localDKIMOutcome struct {
	row   *store.LocalDKIM
	label string
}

// checkLocalDKIM runs the check for one NEW-candidate IMAP message. It never
// returns an error: every failure is "not checked".
//
// received is the provider's INTERNALDATE, the time a date-bounded authserv-id
// entry is judged at (ml.TrustedAt).
func (in *Ingestor) checkLocalDKIM(ctx context.Context, raw []byte, parsed *mime.Message, fromAddress string, received time.Time) localDKIMOutcome {
	if in.localDKIM == nil {
		return localDKIMOutcome{}
	}
	if parsed == nil {
		return localDKIMOutcome{label: LocalDKIMNotChecked}
	}
	// The same trust decision as every other consumer: resolve the entries at
	// this message's received time, then ask whether any header bears one.
	if ml.TrustedAuthResults(parsed.Header.Values("Authentication-Results"),
		ml.TrustedAt(in.trustedAuthservIDs, received)).Present {
		return localDKIMOutcome{label: LocalDKIMSkippedTrusted}
	}
	ctx, cancel := context.WithTimeout(ctx, localDKIMDeadline)
	defer cancel()
	v := dkimverify.New(dkimverify.Options{
		LookupTXT:     in.localDKIM.lookup,
		LookupTimeout: localDKIMLookupTimeout,
		MaxLookups:    localDKIMMaxLookups,
	})
	results := v.Verify(ctx, raw)
	if ctx.Err() != nil {
		// The overall deadline (or a shutdown) cut the check short: whatever the
		// results say, they are not a complete answer.
		return localDKIMOutcome{label: LocalDKIMNotChecked}
	}
	return decideLocalDKIM(results, domainOf(fromAddress), parsed.Header.FieldCount)
}

// failPriority orders non-pass, non-transient results by how much they say
// about the message: when no signature passed, the stored result is the most
// informative one.
var failPriority = []dkimverify.Status{
	dkimverify.StatusFailBody, dkimverify.StatusFailSignature,
	dkimverify.StatusKeyRevoked, dkimverify.StatusKeyNotFound,
	dkimverify.StatusPermError, dkimverify.StatusUnsupported,
}

// decideLocalDKIM turns the per-signature results into the one row stored.
//
//   - a pass aligned with the From domain: stored as pass, aligned;
//   - otherwise any temperror: NOTHING (an undecided signature might have been
//     the aligned pass), not_checked;
//   - otherwise a pass that is not aligned: stored, aligned=false;
//   - no signature at all: nothing to store (no_signature);
//   - otherwise the most informative failure (failPriority).
//
// Only an aligned pass is ever displayed or counted; everything else is stored
// as evidence and measurement (ADR-036).
func decideLocalDKIM(results []dkimverify.Result, fromDomain string, count func(header string) int) localDKIMOutcome {
	var firstPass, firstBlocked *dkimverify.Result
	blockedWhy := ""
	temp := false
	for i := range results {
		r := &results[i]
		switch r.Status {
		case dkimverify.StatusPass:
			if DKIMAligned(fromDomain, r.Domain) {
				if why := displayBlock(r.SignedHeaders, count); why != "" {
					if firstBlocked == nil {
						firstBlocked, blockedWhy = r, why
					}
					continue
				}
				return localDKIMOutcome{
					row:   &store.LocalDKIM{Result: string(dkimverify.StatusPass), Domain: ml.NormalizeAuthDomain(r.Domain), Aligned: true},
					label: LocalDKIMPass,
				}
			}
			if firstPass == nil {
				firstPass = r
			}
		case dkimverify.StatusTempError:
			temp = true
		}
	}
	if temp {
		return localDKIMOutcome{label: LocalDKIMNotChecked}
	}
	if firstBlocked != nil {
		label := LocalDKIMPassUnaligned
		switch {
		case strings.HasPrefix(blockedWhy, "duplicate_header:"):
			label = LocalDKIMPassBlocked
		case strings.HasPrefix(blockedWhy, "unsigned_header:"):
			label = LocalDKIMPassBlockedUnsigned
		}
		return localDKIMOutcome{
			row:   &store.LocalDKIM{Result: string(dkimverify.StatusPass), Domain: ml.NormalizeAuthDomain(firstBlocked.Domain), DisplayBlocked: blockedWhy},
			label: label,
		}
	}
	if firstPass != nil {
		return localDKIMOutcome{
			row:   &store.LocalDKIM{Result: string(dkimverify.StatusPass), Domain: ml.NormalizeAuthDomain(firstPass.Domain)},
			label: LocalDKIMPassUnaligned,
		}
	}
	if len(results) == 0 {
		return localDKIMOutcome{label: LocalDKIMNoSignature}
	}
	for _, want := range failPriority {
		for i := range results {
			if results[i].Status == want {
				return localDKIMOutcome{
					row:   &store.LocalDKIM{Result: string(want), Domain: ml.NormalizeAuthDomain(results[i].Domain)},
					label: string(want),
				}
			}
		}
	}
	return localDKIMOutcome{label: LocalDKIMNotChecked} // a Status this table does not know: store nothing
}

// DKIMAligned reports whether a signature by d= speaks for the From domain:
// d= EQUALS the From domain, or the From domain is a SUBDOMAIN of d=. And d=
// must not itself be a public suffix.
//
// ⚠ THE PUBLIC SUFFIX RULE IS WHAT STOPS `d=co.uk` OR `d=com` SPEAKING FOR
// EVERY DOMAIN BENEATH IT. A suffix cannot be signed for by its registrant (it
// has none): a key published at `selector._domainkey.co.uk` belongs to whoever
// controls that zone, not to the From domain's owner. ml.OrgDomain (the one
// Public Suffix List helper, publicsuffix.EffectiveTLDPlusOne underneath)
// refuses a name that is itself a public suffix (the ICANN and the private
// sections both: `blogspot.com` is a suffix too, so one
// tenant cannot sign for another). It is the Public Suffix List, not the
// last-two-labels approximation the measurement report used.
//
// Both names go through ml.NormalizeAuthDomain (lowercase, no trailing dot,
// punycode), so a U-label and its A-label compare equal.
func DKIMAligned(fromDomain, d string) bool {
	from, d := ml.NormalizeAuthDomain(fromDomain), ml.NormalizeAuthDomain(d)
	if from == "" || d == "" {
		return false
	}
	if _, ok := ml.OrgDomain(d); !ok {
		return false
	}
	return from == d || strings.HasSuffix(from, "."+d)
}

// guardedHeaders are the identity headers a reader acts on. A pass is
// displayable only if EVERY instance of each one present is covered by the
// passing signature's h= (From is handled first: exactly one, signed).
var guardedHeaders = []string{"sender", "reply-to", "to", "cc", "subject", "date", "message-id"}

// displayBlock says why a verified, aligned pass must NOT be displayed, or ""
// when it may be. The rule is "every displayed identity header is signed".
//
// ⚠ THE HEADER-PREPEND AND HEADER-ADD ATTACKS (RFC 6376 §8.15). A DKIM
// signature covers, for each name in h=, the BOTTOM-MOST instance(s) of that
// header, and says nothing about a header it does not name. An attacker who
// holds any genuinely signed message from a domain can therefore
//
//   - PREPEND a second From:/Subject:/Reply-To: above the signed one: the
//     signature still verifies, a reader sees the TOP instance;
//   - ADD a header the signature never covered, e.g. a Reply-To on a message
//     that had none: the signature still verifies and replies go to the
//     attacker.
//
// An unguarded check would label either forgery "Verified by Mailroom". So a
// pass is displayable only when (n = times h= lists the name, m = instances in
// the message, a raw count):
//
//	From:           m == 1 and n >= 1;
//	each guarded h: m == 0 (absent: adding one later makes m=1, n=0, blocked); or
//	                m == 1 and n >= 1 (the one instance is signed); or
//	                m >= 2 and n > m (the header is OVERSIGNED: h= lists it more
//	                times than it is present, so one more instance would fill
//	                the empty slot and break the signature).
//
// Reasons: from_not_signed, duplicate_header:<h> (m >= 2 and not oversigned,
// whether or not it is signed), unsigned_header:<h> (m == 1, n == 0).
//
// count is the raw per-name field count of the message header block.
func displayBlock(signed []string, count func(header string) int) string {
	n := map[string]int{}
	for _, h := range signed {
		n[h]++
	}
	if n["from"] == 0 {
		return "from_not_signed"
	}
	if count("From") != 1 {
		return "duplicate_header:from"
	}
	for _, h := range guardedHeaders {
		m := count(h)
		switch {
		case m == 0:
		case m >= 2 && n[h] <= m:
			return "duplicate_header:" + h
		case m == 1 && n[h] == 0:
			return "unsigned_header:" + h
		}
	}
	return ""
}
