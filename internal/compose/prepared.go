// SPDX-License-Identifier: AGPL-3.0-only

package compose

// Is this message ALREADY prepared for sending? (#491)
//
// prepareOutbound signs the message and advertises the sender's key, and it does
// that by REWRITING THE STORED BYTES (see the ⚠ note on Submit for why signing
// happens at submit time and not on the wire). Submitting the same draft twice
// therefore hands prepareOutbound its own output.
//
// That is not a hypothetical: a re-send after an Undo Send cancellation is a
// BLESSED PATH — db/schema.sql says so at app.submissions, and the client's
// Outbox makes it one click ("Send now"). Before this file, the second submit
// produced a multipart/signed wrapped in a multipart/signed and TWO Autocrypt
// headers. Both are worse than they look:
//
//   - Nested multipart/signed. The outer signature covers the inner signed
//     entity, so it verifies — and every client shows one signature over a body
//     that is itself an attachment-laden container, with the real signature
//     buried a level down.
//   - Two Autocrypt headers. Autocrypt Level 1 requires a receiver to IGNORE
//     ALL of them (autocrypt.Parse implements exactly that), so the second
//     header does not add a key, it DELETES the one the first header carried.
//     The user's key silently stops propagating on precisely the messages they
//     re-sent.

import (
	"mailroom/internal/autocrypt"
	"mailroom/internal/mime"
	"mailroom/internal/pgpmime"
	"mailroom/internal/store"
)

// preparedReason names WHY outboundAlreadyPrepared answered as it did.
//
// It is returned for tests and diagnostics — the caller needs only the bool —
// and it is not decoration: it is what makes a change to this rule legible.
// Every value below is asserted somewhere in prepared_test.go, so a rule that
// starts answering "yes" for a new reason has to say which one.
type preparedReason string

const (
	// reasonNoSendingAddress — the caller passed no address to compare against.
	// Cannot happen from Submit (it refuses a message with no From before it
	// gets here) and is answered rather than assumed, because the alternative is
	// treating every message as prepared the day a caller forgets.
	reasonNoSendingAddress preparedReason = "no sending address to compare against"
	// reasonFromMismatch — these bytes were not authored for this send: their
	// From is somebody else's (or unparseable). See the note in
	// outboundAlreadyPrepared on what this excludes.
	reasonFromMismatch preparedReason = "the message's From is not the address being sent as"
	// reasonUnprepared — ours, and carries neither marker. The ordinary first
	// submit.
	reasonUnprepared preparedReason = "no outbound preparation present"

	// reasonEncrypted — an RFC 3156 multipart/encrypted. What an ENCRYPTED send's
	// own output looks like (#47, ADR-038), and so the reason a re-send of one
	// must be left alone: encrypting it again would put the ciphertext inside a
	// second ciphertext.
	reasonEncrypted preparedReason = "already an RFC 3156 multipart/encrypted message"
	// reasonSignedAndAdvertised — both markers. What our own output looks like
	// on an instance with a crypto process, and so the reason a re-send actually
	// hits.
	reasonSignedAndAdvertised preparedReason = "already RFC 3156 signed and carrying an Autocrypt header"
	// reasonSigned — signed, no Autocrypt header. A key with no public half
	// bound (prepareOutbound skips the advertisement then), or a draft another
	// client signed with the user's own key and left in Drafts over IMAP.
	reasonSigned preparedReason = "already an RFC 3156 multipart/signed message"
	// reasonAdvertised — an Autocrypt header, no signature. THE COMMON CASE ON
	// AN INSTANCE WITH NO CRYPTO PROCESS: prepareOutbound advertises the key and
	// signs nothing, so this is what its own output looks like there, and
	// requiring a signature would let the re-send add a second Autocrypt header
	// on every instance that cannot sign.
	reasonAdvertised preparedReason = "already carrying an Autocrypt header for this address"
)

// outboundAlreadyPrepared reports whether raw has ALREADY been through
// prepareOutbound for the address being sent as, and must therefore be left
// exactly as it is.
//
// PURE, and deliberately so: it decides from the bytes alone, which is what lets
// the double-signing case be a unit test rather than a database fixture.
//
// # What it checks, and what each check is for
//
//  1. The message's own From must be the address this send resolved to. That is
//     what excludes a message somebody ELSE signed — a received multipart/signed
//     filed into Drafts, or one quoted into a draft — because such a message
//     carries its author's From, not ours. It is nearly redundant from Submit
//     (which derives `from` from these same bytes, through the same parser) and
//     is the load-bearing check for any other caller.
//
//  2. EITHER marker of our own preparation:
//     • an RFC 3156 multipart/signed container, per pgpmime's own splitter —
//     the same code a verifier's side would run, so "signed" here means
//     splittable, not merely "says multipart/signed";
//     • an Autocrypt header, which prepareOutbound adds before signing.
//
// Either is sufficient BECAUSE prepareOutbound emits one or both depending on
// how the instance is wired (crypto process present or not) and on whether the
// bound key has a public half. Requiring both would miss the no-crypto-process
// case entirely, which is the majority of installs today.
//
// # What it does NOT check, and the honest limit
//
// It does not check WHO signed: that means parsing the signature packet's issuer
// and comparing it to the identity's key, which needs the key, a packet parser,
// and therefore not a pure function over bytes. So a received multipart/signed
// whose From has been REWRITTEN to the user's own address — something no client
// does when composing, since composing rebuilds the MIME — would be taken for
// ours and sent on unsigned by us. The trade is deliberate: that costs an
// advertisement and a signature on a message the user did something unusual to
// produce, while the other direction (signing again) breaks key propagation on
// an ordinary Outbox re-send.
//
// # Why one gate rather than two
//
// A finer version could skip the signature and still add the header, or the
// reverse. It is not worth the second decision point: the only case that
// distinguishes them is an instance whose crypto process arrives BETWEEN two
// submits of the same draft, inside the undo window. All-or-nothing keeps the
// rule "these bytes are already what we would send", which is the property that
// is actually true.
func outboundAlreadyPrepared(raw []byte, from string) (bool, preparedReason) {
	sending := store.NormalizeAddress(from)
	if sending == "" {
		return false, reasonNoSendingAddress
	}

	// ParseBytes is total on hostile input (docs/mime-policy.md) and always
	// returns a usable Message, so an unparseable message simply has no From and
	// falls out as a mismatch rather than as a third outcome to reason about.
	parsed, _ := mime.ParseBytes(raw)
	if messageFrom(parsed) != sending {
		return false, reasonFromMismatch
	}

	_, splitErr := pgpmime.SplitSigned(raw)
	signed := splitErr == nil
	// An RFC 3156 multipart/encrypted is our own output of an encrypted send
	// (#47, ADR-038): it was signed before it was encrypted, and the signature is
	// inside the ciphertext where this cannot see it.
	if pgpmime.IsEncrypted(raw) {
		return true, reasonEncrypted
	}

	// EVERY Autocrypt header, not a parsed one. A header that Level 1 says to
	// ignore (two of them, an unknown critical attribute, corrupt base64) still
	// means "an Autocrypt header is present", and adding ours beside it makes the
	// message strictly worse. The addr= attribute is not consulted either: rule
	// 1 has already established that the message's From is the address we are
	// sending as, and Level 1 requires a usable header to match From — so a
	// header for a different addr is one a receiver discards, and a second header
	// would not rescue it.
	advertised := parsed != nil && len(parsed.Header.Values(autocrypt.HeaderName)) > 0

	switch {
	case signed && advertised:
		return true, reasonSignedAndAdvertised
	case signed:
		return true, reasonSigned
	case advertised:
		return true, reasonAdvertised
	default:
		return false, reasonUnprepared
	}
}

// messageFrom returns the normalized From address of a parsed message, or "".
//
// Shared with ParseEnvelope rather than reimplemented: the address this compares
// is exactly the one Submit validated against the user's identities, and two
// derivations of "the From" could differ on a malformed header — where being
// wrong means either refusing a legitimate send or signing twice.
func messageFrom(parsed *mime.Message) string {
	if parsed == nil {
		return ""
	}
	if from := parsed.Header.From(); len(from) > 0 {
		return store.NormalizeAddress(from[0].Address)
	}
	return ""
}
