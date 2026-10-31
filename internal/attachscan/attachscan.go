// SPDX-License-Identifier: AGPL-3.0-only

// Package attachscan holds the rules for judging one attachment part, shared by
// every path that produces a verdict (#394, #395, ADR-027 §5a, §6).
//
// # Why a package for these few small functions
//
// There are two callers — ingest, which judges a message as it arrives, and
// rescan, which re-judges stored ones as the signatures move — and they must
// reach the SAME answer about the same bytes. If they disagree, an attachment's
// verdict depends on which path touched it last, and "clean" or "unknown"
// becomes a fact about timing rather than about the file.
//
// ⚠ WHAT IS SHARED IS THE JUDGEMENT, NOT THE CONTROL FLOW, and the split is
// deliberate. The two callers have genuinely different error policies and must
// keep them:
//
//   - ingest is FAIL-CLOSED. A scanner outage refuses the sync pass, because the
//     alternative is admitting mail carrying a verdict nobody produced. It also
//     runs a circuit breaker, because a file that CRASHES clamd would otherwise
//     halt the account forever at a stranger's choosing (§4b).
//   - rescan is BEST-EFFORT. Everything it looks at already has a verdict and is
//     already stored, so failing to refresh one leaves the last answer standing.
//     Refusing anything there would be theatre.
//
// Folding those together would mean one of them getting the other's behaviour.
// So this package exports the rules that must not differ and nothing else — the
// same reasoning as internal/attachproj, where the mapping lived in three copies
// and the judgement in one.
//
// Increment 5 (#396) added two more rules of the same kind: the CONTENT IDENTITY
// a quarantine acts on (PartHash), which must be identical across the two
// scanning paths or "every copy" silently becomes "some copies"; and the gate
// rule itself (Blocked), which the download route, the JMAP property and the
// report all have to agree on.
package attachscan

import (
	"crypto/sha256"

	"mailroom/internal/clamd"
	"mailroom/internal/mime"
	"mailroom/internal/pgpmail"
	"mailroom/internal/store"
)

// ForcedUnknown reports the message-level condition under which no scan result
// could mean anything, so none should be attempted. (The per-subtree encrypted
// condition is EncryptedPaths.)
//
// ⚠ A SCAN HERE WOULD SUCCEED AND BE WRONG, which is worse than one that fails.
// clamd produces no error; it produces a confident "stream: OK" over bytes that
// are not the whole payload.
func ForcedUnknown(m *mime.Message) (reason string, forced bool) {
	if m == nil {
		return "", false
	}
	// ADR-027 §5a. internal/mime stops walking at 64 MiB / MaxDepth / MaxParts
	// and sets Truncated. The parts visible after that are a PREFIX of the
	// message, so "clean" would be a claim about the portion we happened to
	// reach — while the whole payload stays downloadable as raw .eml.
	if m.Truncated {
		return "the MIME walk hit a parser limit, so the parts seen are a prefix of the message", true
	}
	// Encryption (ADR-027 §6) is NOT decided here: it is a property of a
	// SUBTREE, not of the message, so see EncryptedPaths.
	return "", false
}

// EncryptedPaths returns the part paths of this message's own attachment parts
// that sit inside (or are) encrypted content (ADR-027 §6, scoped by #405).
//
// WKD publishes the operator's key, so ANY stranger can encrypt to this
// mailbox. An attachment inside a PGP/MIME or S/MIME encrypted node is an
// opaque octet-stream: clamd scans ciphertext, finds nothing, and the row would
// record `clean` on a file never examined in plaintext, an attacker-selected
// path the design would then affirmatively endorse. Those parts are `unknown`
// and are never sent to the scanner.
//
// ⚠ SCOPE IS THE ENCRYPTED SUBTREE, NOT THE MESSAGE. Siblings of an encrypted
// node, and the parts of the message itself when only a forwarded
// (message/rfc822) message is encrypted, are plaintext and are scanned
// normally. The one exception is deliberate: the message/rfc822 part that IS a
// forwarded encrypted message is itself ciphertext to the scanner, so that one
// leaf is covered (and only that leaf).
//
// Opaque-signed S/MIME (smime-type=signed-data) is not encrypted and is not
// here (pgpmail.EncryptedNode). Inline PGP (PGPInlineMessage) is deliberately
// NOT here either: its ciphertext sits in a text body rather than an
// attachment part, so no attachment bytes are being blessed.
func EncryptedPaths(m *mime.Message) map[string]bool {
	if m == nil || m.Structure == nil {
		return nil
	}
	paths := mime.PartPaths(m.Structure)
	out := map[string]bool{}

	// A forwarded message/rfc822 leaf is itself an attachment of THIS message
	// whose bytes contain whatever the forwarded message contains. If that
	// message is encrypted, the leaf is ciphertext to the scanner and is unknown;
	// that is the only way the forwarded message reaches the outer message's
	// verdicts, and it reaches no sibling. Nested messages are in document
	// order, one per rfc822 leaf; if the counts disagree (a parser limit was
	// hit) every rfc822 leaf is judged against all of them, the conservative way.
	var rfc822 []*mime.Structure
	var collect func(n *mime.Structure)
	collect = func(n *mime.Structure) {
		if n == nil {
			return
		}
		if ct := n.ContentType; ct == "message/rfc822" || ct == "message/global" {
			rfc822 = append(rfc822, n)
		}
		for _, sub := range n.SubParts {
			collect(sub)
		}
	}
	collect(m.Structure)
	nestedEnc := func(i int) bool {
		enc := func(nm *mime.Message) bool {
			sg := pgpmail.DetectWithNested(nm)
			return sg.PGPMIMEEncrypted || sg.SMIMEEncrypted
		}
		if len(rfc822) == len(m.Nested) {
			return enc(m.Nested[i])
		}
		for _, nm := range m.Nested {
			if enc(nm) {
				return true
			}
		}
		return false
	}
	forwardedEncrypted := map[*mime.Structure]bool{}
	for i, n := range rfc822 {
		if nestedEnc(i) {
			forwardedEncrypted[n] = true
		}
	}

	var walk func(n *mime.Structure, inside bool)
	walk = func(n *mime.Structure, inside bool) {
		if n == nil {
			return
		}
		inside = inside || pgpmail.EncryptedNode(n) || forwardedEncrypted[n]
		if !mime.IsMultipart(n.ContentType) {
			if inside && mime.IsAttachmentLeaf(n.ContentType, n.Disposition) {
				out[paths[n]] = true
			}
			return
		}
		for _, sub := range n.SubParts {
			walk(sub, inside)
		}
	}
	walk(m.Structure, false)
	return out
}

// AllEncrypted reports whether there is at least one part and every one is in
// the encrypted set, so a caller can skip the scanner (and its health probes)
// entirely.
func AllEncrypted(parts []mime.AttachmentPart, enc map[string]bool) bool {
	if len(parts) == 0 {
		return false
	}
	for _, ap := range parts {
		if !enc[ap.Path] {
			return false
		}
	}
	return true
}

// HasNoBytes reports a part that decoded to nothing.
//
// ⚠ ZERO BYTES CANNOT BE CLEAN. clamd answers "stream: OK" to an empty INSTREAM,
// so sending nothing produces a confident clean verdict about a part nothing
// read. internal/mime reaches this state without any hostile input: walkLeaf
// records a body read error as a WARNING and leaves the leaf's Data as whatever
// it got, which can be nothing — and unlike the size limit it does NOT set
// Truncated, so ForcedUnknown never sees it.
//
// ⚠ It does NOT catch a PARTIAL read — some bytes, but not all — because the
// parser keeps that failure in msg.Warnings and the Structure carries no
// per-part read status to key on. Named rather than papered over.
func HasNoBytes(ap mime.AttachmentPart) bool {
	return ap.Node == nil || len(ap.Node.Data) == 0
}

// Unknown records `unknown` for every part.
//
// Signature stays empty: it is clamd's own name for what it matched, and nothing
// matched. The human-readable reason goes to the caller's log rather than the
// row — app.attachments has no column for it.
func Unknown(parts []mime.AttachmentPart) []store.ScanVerdict {
	out := make([]store.ScanVerdict, 0, len(parts))
	for _, ap := range parts {
		out = append(out, store.ScanVerdict{
			PartPath: ap.Path,
			Status:   string(clamd.Unknown),
			PartHash: PartHash(ap),
		})
	}
	return out
}

// Verdict maps one part's scan result to the row it becomes.
func Verdict(ap mime.AttachmentPart, v clamd.Verdict) store.ScanVerdict {
	return store.ScanVerdict{
		PartPath:  ap.Path,
		Status:    string(v.State),
		Signature: v.Signature,
		PartHash:  PartHash(ap),
	}
}

// PartHash is the CONTENT IDENTITY of one part: SHA-256 over its decoded bytes.
//
// ⚠ IT IS HERE FOR THE REASON THE REST OF THIS PACKAGE IS. Quarantine and
// release act on content, not on a row (ADR-027 §8) — the same attachment sits
// on N different messages, and a decision that reached only one of them is the
// writeback-hit-1-of-N-copies bug. If ingest and re-scan computed this
// differently, two copies of identical bytes would land under different
// identities and the "all of them" guarantee would quietly become "some of
// them". Both callers already hold ap.Node.Data at this point, so it costs no
// additional read on either path.
//
// nil for a part with no bytes, which HasNoBytes has already turned into an
// `unknown`: hashing nothing yields the digest of the empty string, and that is
// a single identity every empty part in the mailbox would share — one release
// against it would open the gate on all of them.
func PartHash(ap mime.AttachmentPart) []byte {
	if HasNoBytes(ap) {
		return nil
	}
	sum := sha256.Sum256(ap.Node.Data)
	return sum[:]
}

// Blocked reports whether the download gate must refuse this part (ADR-027 §8).
//
// ⚠ `unknown` BLOCKS EXACTLY AS `flagged` DOES (§5b). Different wording, same
// friction: "we could not check this" and "we checked and it is bad" warrant
// the same door. Anything less and the adversary never has to beat the scanner,
// only avoid it — pad past the size limit, nest deeply, or send an encrypted
// zip with the password in the body, which is commodity technique.
//
// ⚠ `unscanned` DOES NOT BLOCK. It means no scanner is configured or none has
// reached this row yet; blocking it would break every attachment on an instance
// whose operator never asked for a scanner, and it is also the resting state of
// the user's own outbound mail. internal/store/quarantine.go carries the full
// argument, including why inbound mail cannot rest there while a scanner is
// configured.
//
// released is the user's override, and it opens the door whatever the verdict
// says — the verdict is deliberately not erased by the decision.
func Blocked(status string, released bool) bool {
	if released {
		return false
	}
	return status == string(clamd.Flagged) || status == string(clamd.Unknown)
}
