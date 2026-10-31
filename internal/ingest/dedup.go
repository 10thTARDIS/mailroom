// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"net/mail"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// RE-DELIVERY DEDUP (#206, ADR-018 amending ADR-001).
//
// ADR-001 makes a message's IDENTITY SHA-256 of its raw RFC 5322 bytes, and states the
// consequence plainly: "a transport that mutates bytes creates a second message, which is
// correct." That is right for identity — the blob is content-addressed by it and the
// messages row is the FK hub of the schema — and it is why nothing here touches it.
//
// But it means the one case dedup most obviously exists for is the case it cannot see.
// When a mail path delivers the same message twice, each delivery is stamped with its own
// trace metadata, so the bytes differ, so it is two messages. Real inboxes showed
// some mail three and four times.
//
// WHY THIS IS AN ALLOWLIST AND NOT A DENYLIST — the measurement decided it. Running
// `mailroom-admin diff-duplicates` over a real mailbox: every duplicate group had
// BYTE-IDENTICAL BODIES. What differed was `Received:` in nearly all of
// them, and in the rest a further nine fields from two providers between them —
// X-Gm-Message-State, X-Google-Dkim-Signature, X-Forwarded-Encrypted, X-Migadu-Queue-Id,
// X-Migadu-Spam-Score, X-Migadu-Flow, X-Migadu-Reason, X-Report-Abuse, and a re-signed
// Dkim-Signature.
//
// Subtracting a denylist of those would work today and rot tomorrow: every provider has
// its own private headers, and this evidence comes from a single mailbox behind exactly
// two providers. Hashing an ALLOWLIST of the fields that identify a message, plus the
// body, needs no entry per provider and cannot silently stop working.
//
// WHY ADR-001's HAZARD DOES NOT RETURN. Its argument against Message-ID dedup is that a
// mailing-list copy with a rewritten footer and the direct copy share a Message-ID and
// would silently merge. That copy has a DIFFERENT BODY, so a different DedupHash, so it
// stays its own message. The same holds for malicious reuse: to collide, an attacker must
// reproduce the body, Date, From, To, Cc and Subject — at which point it is the same
// message, and treating it as one is the correct answer rather than the dangerous one.
//
// To and Cc are in the allowlist DELIBERATELY, as the conservative choice: one message
// genuinely addressed to two of a catchall's aliases stays two rows rather than being
// merged into one. Under-merging is recoverable by a person; over-merging silently loses
// mail.

// dedupOf is DedupHash as a slice, for handing to the store.
func dedupOf(raw []byte) []byte {
	h := DedupHash(raw)
	return h[:]
}

// findExisting answers "do we already hold this message?" for every ingest path.
//
// THREE QUESTIONS, ASKED IN ORDER OF CERTAINTY:
//
//  1. Identical BYTES (content_hash, ADR-001's identity). Certain: the same bytes are
//     the same message, and this is the check that has always existed.
//  2. Bytes we USED to hold (superseded_content, #260). Also certain, and certain for
//     a different reason: the row exists only because this server replaced those exact
//     bytes itself and recorded doing so.
//  3. Identical MESSAGE, re-delivered (dedup_hash, #206). An inference — a strong one,
//     borne out on real mail, but an inference.
//
// The order is load-bearing. Asking the certain questions first means the inference can
// never override them, and it also means the common case — a poll re-fetching mail we
// already have, byte for byte — is answered by the cheaper, unique index.
//
// ⚠ QUESTION 2 EXISTS BECAUSE COMPOSE-TIME SIGNING BROKE QUESTION 1's REACH (#47
// increment 5). Signing rewrites a message's bytes at submit, but the copy already
// APPENDed to IMAP Drafts still holds the pre-signature bytes — so a sync that reads
// Drafts before the expunge lands used to find bytes matching nothing and ingest a
// stray, unsigned, already-sent draft. Observed on a real send rather than predicted:
// sync beat the expunge by seven seconds, and a second send lost the race the other way
// and was clean. The dedup hash cannot cover this either, because it hashes the BODY and
// a signed body is a multipart/signed wrapper — a genuinely different body.
//
// A hit on any of them returns the EXISTING message, and the caller records this delivery
// as another provenance row rather than inserting a second message. That is the whole
// fix: nothing is discarded, the new delivery is still recorded as having happened, and
// the mailbox stops showing one message four times.
func (in *Ingestor) findExisting(
	ctx context.Context, tx *store.UserTx, sum [32]byte, raw []byte,
) (ulid.ULID, bool, error) {
	id, ok, err := tx.MessageIDByContentHash(ctx, sum[:])
	if err != nil || ok {
		return id, ok, err
	}
	id, ok, err = tx.MessageIDBySupersededContent(ctx, sum[:])
	if err != nil || ok {
		return id, ok, err
	}
	return tx.MessageIDByDedupHash(ctx, dedupOf(raw))
}

// dedupFields are the headers that identify a message, in a fixed order.
//
// FIXED ORDER MATTERS: the hash is over a canonical rendering, so the same message must
// produce the same bytes regardless of the order a transport happened to emit its headers
// in. Iterating a map here would make the hash depend on Go's map iteration order and the
// whole mechanism would fail intermittently — the worst possible failure for a dedup key.
var dedupFields = []string{
	"Message-Id",
	"Date",
	"From",
	"To",
	"Cc",
	"Subject",
}

// DedupHash identifies a message for RE-DELIVERY detection: the identity-bearing headers
// plus the body, canonically rendered.
//
// It is NOT a message's identity (that stays ADR-001's content hash) and must never be
// used as one: two messages CAN legitimately share a DedupHash — that is the entire point
// — whereas two messages sharing a content_hash are the same bytes.
//
// Never fails. A message that will not parse hashes its raw bytes instead, which means an
// unparseable message dedupes exactly as it did before this existed: on its bytes. That is
// the safe direction — the fallback under-merges rather than over-merges.
func DedupHash(raw []byte) [32]byte {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return sha256.Sum256(raw)
	}

	h := sha256.New()
	for _, field := range dedupFields {
		// The field NAME goes in as well as its value. Without it, a message whose
		// Subject is "X" and empty To would hash the same as one with empty Subject
		// and To "X" — adjacent empty fields make the concatenation ambiguous.
		h.Write([]byte(field))
		h.Write([]byte{0})
		// Every value of a repeated field, in the order received, so a message with
		// two From lines cannot collide with one that has a single combined one.
		for _, v := range m.Header[field] {
			h.Write([]byte(normalizeHeaderValue(v)))
			h.Write([]byte{0})
		}
		h.Write([]byte{'\n'})
	}

	// The body, with line endings normalised. A transport that rewrites CRLF to LF (or
	// back) is doing precisely the "mutates bytes" thing ADR-001 describes, and it must
	// not defeat this the way it defeats the content hash.
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(m.Body); err != nil {
		return sha256.Sum256(raw)
	}
	h.Write(normalizeNewlines(body.Bytes()))

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// normalizeHeaderValue folds the whitespace a transport is free to rewrite.
//
// RFC 5322 lets a header be folded across lines at any whitespace, and relaying can
// refold it, so `Subject: a  b` and a version wrapped after `a` are the same header. Runs
// of whitespace collapse to one space and the ends are trimmed. Nothing else is touched —
// case is preserved, because `From: Alice` and `From: alice` are genuinely different
// display names.
func normalizeHeaderValue(v string) string {
	var sb strings.Builder
	sb.Grow(len(v))
	space := false
	for _, r := range v {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			space = true
			continue
		}
		if space && sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		space = false
		sb.WriteRune(r)
	}
	return sb.String()
}

// normalizeNewlines renders any mix of CRLF/CR/LF as LF, leaving all other bytes alone.
// Streamed line by line rather than with a replace-all so a large body is not copied
// twice.
func normalizeNewlines(b []byte) []byte {
	if !bytes.ContainsAny(b, "\r") {
		return b
	}
	out := make([]byte, 0, len(b))
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), len(b)+1)
	first := true
	for sc.Scan() {
		if !first {
			out = append(out, '\n')
		}
		first = false
		out = append(out, bytes.TrimSuffix(sc.Bytes(), []byte("\r"))...)
	}
	if sc.Err() != nil {
		// A body the scanner cannot walk falls back to the blunt replacement rather
		// than to nothing: a truncated hash would be a wrong hash.
		return bytes.ReplaceAll(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
	}
	// A trailing newline is information (some transports add one), so it is preserved.
	if len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		out = append(out, '\n')
	}
	return out
}
