// SPDX-License-Identifier: AGPL-3.0-only

// Package pgpmail detects end-to-end encryption and signing apparatus in a
// parsed message: OpenPGP (RFC 3156 PGP/MIME and the older inline armor),
// Autocrypt (Level 1), and S/MIME (RFC 8551).
//
// IT DETECTS ONLY. Nothing here verifies a signature, decrypts anything, or
// touches a private key — that is the crypto process's job (#47, spec §9) and
// deliberately not this package's. Detection is separable, needs no key
// material, and is wanted in three places: the prevalence report below, the
// ingest path once decryption exists, and Autocrypt peer-key discovery (#48).
//
// # Why this exists before any crypto does
//
// #47's plan sequences a read-only measurement first: how much of this mailbox
// is signed or encrypted at all? The operator's answer, given in advance, is
// "none" — and that is exactly what makes the DETECTOR rather than the count
// the deliverable.
//
// A report whose expected output is zero cannot distinguish "no PGP mail" from
// "the detector never fires". That failure has already been paid for once in
// this project on the same day this package was written: `suggest-report`
// printed "0 of 361" from a filter that made the answer zero by construction.
// So the tests here are POSITIVE controls over real message structures, and the
// report is a thin wrapper over a mechanism proven to fire. A zero from the
// live mailbox then CONFIRMS the expectation rather than establishing anything.
package pgpmail

import (
	"bytes"
	"strings"

	"mailroom/internal/mime"
)

// Signals is what one message reveals about signing and encryption. Every field
// is independent: a message can be inline-signed AND carry an Autocrypt header,
// and PGP and S/MIME can both appear in one mailbox from different senders.
type Signals struct {
	// PGPMIMESigned is RFC 3156 §5: multipart/signed with
	// protocol="application/pgp-signature".
	PGPMIMESigned bool
	// PGPMIMEEncrypted is RFC 3156 §4: multipart/encrypted with
	// protocol="application/pgp-encrypted".
	PGPMIMEEncrypted bool
	// PGPInlineSigned is the pre-MIME cleartext framework (RFC 4880 §7):
	// "-----BEGIN PGP SIGNED MESSAGE-----" in a text body.
	PGPInlineSigned bool
	// PGPInlineMessage is inline armored ciphertext: "-----BEGIN PGP MESSAGE-----".
	PGPInlineMessage bool
	// PGPPublicKey is a key delivered in the message — application/pgp-keys, or
	// an armored public key block in a text body.
	PGPPublicKey bool
	// Autocrypt is an Autocrypt: header (Level 1). Its presence means the
	// SENDER's client would accept encrypted replies, which is the signal that
	// matters for #48 and is independent of whether this message is encrypted.
	Autocrypt bool
	// AutocryptGossip is Autocrypt-Gossip:, which carries peer keys for the
	// other recipients of a group message.
	AutocryptGossip bool
	// SMIMESigned is application/pkcs7-signature (RFC 8551), including the
	// legacy x-pkcs7 spelling.
	SMIMESigned bool
	// SMIMEEncrypted is any application/pkcs7-mime, whatever its smime-type
	// label says (see EncryptedNode).
	SMIMEEncrypted bool
}

// Any reports whether the message showed any signing or encryption apparatus at
// all. It is what makes "this mailbox has none" a statement rather than an
// absence of statements.
func (s Signals) Any() bool {
	return s.PGPMIMESigned || s.PGPMIMEEncrypted || s.PGPInlineSigned ||
		s.PGPInlineMessage || s.PGPPublicKey || s.Autocrypt ||
		s.AutocryptGossip || s.SMIMESigned || s.SMIMEEncrypted
}

// AnyPGP and AnySMIME split Any by scheme, because the S/MIME go/no-go (#47,
// spec §18 item 6) turns on the second number and it must not be buried in a
// combined total.
func (s Signals) AnyPGP() bool {
	return s.PGPMIMESigned || s.PGPMIMEEncrypted || s.PGPInlineSigned ||
		s.PGPInlineMessage || s.PGPPublicKey || s.Autocrypt || s.AutocryptGossip
}

func (s Signals) AnySMIME() bool { return s.SMIMESigned || s.SMIMEEncrypted }

// Armor prefixes, per RFC 4880 §6.2. Matched against decoded text bodies.
var (
	armorSignedMessage = []byte("-----BEGIN PGP SIGNED MESSAGE-----")
	armorMessage       = []byte("-----BEGIN PGP MESSAGE-----")
	armorPublicKey     = []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")
)

// Detect walks a parsed message and reports what it carries.
//
// IT REPORTS THIS MESSAGE'S OWN MIME TREE AND NOTHING NESTED (#405). An
// encrypted message forwarded as message/rfc822 is a different message with its
// own tree; merging its signals into the parent made the forwarded ciphertext
// speak for the parent's unrelated attachments. Callers that want "is there
// encrypted mail anywhere in here" (the prevalence report) use DetectWithNested.
//
// It reads the MIME TREE (Message.Structure) rather than the flattened Parts
// view, because two of these signals are properties of a CONTAINER — RFC 3156
// puts the protocol on the multipart/signed or multipart/encrypted node, and
// the flattened view discards exactly that. A detector built on Parts alone
// would silently miss every correctly-formed PGP/MIME message, which is the
// only kind that matters.
//
// A nil message, or one that failed to parse, yields the zero Signals: nothing
// detected. That is honest — we did not observe apparatus — and callers that
// need to distinguish "parsed and found nothing" from "could not parse" must
// track parse failures themselves; the report below does.
func Detect(msg *mime.Message) Signals {
	var s Signals
	if msg == nil {
		return s
	}

	// Headers first: Autocrypt is a header-only signal and appears on messages
	// that are otherwise entirely ordinary.
	if strings.TrimSpace(msg.Header.Get("Autocrypt")) != "" {
		s.Autocrypt = true
	}
	if strings.TrimSpace(msg.Header.Get("Autocrypt-Gossip")) != "" {
		s.AutocryptGossip = true
	}

	if msg.Structure != nil {
		walkNode(msg.Structure, &s)
	}
	return s
}

// DetectWithNested is Detect plus every embedded message/rfc822 tree, merged.
// It answers "does this mailbox hold encryption apparatus anywhere" (a
// forwarded encrypted message is still encrypted mail in this mailbox), and
// must NOT be used to decide anything about the outer message's own parts.
func DetectWithNested(msg *mime.Message) Signals {
	s := Detect(msg)
	if msg == nil {
		return s
	}
	for _, n := range msg.Nested {
		s.merge(DetectWithNested(n))
	}
	return s
}

// EncryptedNode reports whether this one node is encrypted content: a
// multipart/encrypted with the PGP protocol, a bare application/pgp-encrypted
// part, or ANY application/pkcs7-mime / x-pkcs7-mime part.
//
// pkcs7-mime is deliberately NOT split by its smime-type parameter (#405). That
// label is written by the sender: an attacker labels enveloped-data as
// "signed-data" and gets ciphertext scanned and recorded clean. Telling the two
// apart safely means parsing the CMS ContentInfo OID, which is not worth doing
// while S/MIME is deferred (#69) and the measured S/MIME population is zero.
// Opaque-signed S/MIME therefore stays held as unknown, knowingly.
func EncryptedNode(n *mime.Structure) bool {
	if n == nil {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(n.ContentType))
	switch ct {
	case "multipart/encrypted":
		return strings.ToLower(strings.TrimSpace(n.Params["protocol"])) == "application/pgp-encrypted"
	case "application/pgp-encrypted":
		return true
	case "application/pkcs7-mime", "application/x-pkcs7-mime":
		return true
	}
	return false
}

// walkNode inspects one node and recurses. Split out so the container-level
// protocol checks read plainly.
func walkNode(n *mime.Structure, s *Signals) {
	if n == nil {
		return
	}
	ct := strings.ToLower(strings.TrimSpace(n.ContentType))
	proto := strings.ToLower(strings.TrimSpace(n.Params["protocol"]))

	switch ct {
	case "multipart/signed":
		// RFC 3156 §5. The protocol parameter is what distinguishes a PGP
		// signature from an S/MIME one on an otherwise identical container.
		switch proto {
		case "application/pgp-signature":
			s.PGPMIMESigned = true
		case "application/pkcs7-signature", "application/x-pkcs7-signature":
			s.SMIMESigned = true
		}
	case "multipart/encrypted":
		if proto == "application/pgp-encrypted" {
			s.PGPMIMEEncrypted = true
		}
	}

	// Leaf media types. Checked independently of the container, because
	// real-world mail is not always well formed and a bare pgp-signature part
	// is still evidence of a signing correspondent.
	switch ct {
	case "application/pgp-signature":
		s.PGPMIMESigned = true
	case "application/pgp-encrypted":
		s.PGPMIMEEncrypted = true
	case "application/pgp-keys":
		s.PGPPublicKey = true
	case "application/pkcs7-signature", "application/x-pkcs7-signature":
		s.SMIMESigned = true
	case "application/pkcs7-mime", "application/x-pkcs7-mime":
		s.SMIMEEncrypted = true
	}

	// Inline armor, in text bodies only. Data is nil for containers.
	if len(n.Data) > 0 && strings.HasPrefix(ct, "text/") {
		if bytes.Contains(n.Data, armorSignedMessage) {
			s.PGPInlineSigned = true
		}
		if bytes.Contains(n.Data, armorMessage) {
			s.PGPInlineMessage = true
		}
		if bytes.Contains(n.Data, armorPublicKey) {
			s.PGPPublicKey = true
		}
	}

	for _, c := range n.SubParts {
		walkNode(c, s)
	}
}

// merge ORs another message's signals into these (used for nested messages).
func (s *Signals) merge(o Signals) {
	s.PGPMIMESigned = s.PGPMIMESigned || o.PGPMIMESigned
	s.PGPMIMEEncrypted = s.PGPMIMEEncrypted || o.PGPMIMEEncrypted
	s.PGPInlineSigned = s.PGPInlineSigned || o.PGPInlineSigned
	s.PGPInlineMessage = s.PGPInlineMessage || o.PGPInlineMessage
	s.PGPPublicKey = s.PGPPublicKey || o.PGPPublicKey
	s.Autocrypt = s.Autocrypt || o.Autocrypt
	s.AutocryptGossip = s.AutocryptGossip || o.AutocryptGossip
	s.SMIMESigned = s.SMIMESigned || o.SMIMESigned
	s.SMIMEEncrypted = s.SMIMEEncrypted || o.SMIMEEncrypted
}
