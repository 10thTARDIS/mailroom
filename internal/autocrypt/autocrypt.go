// SPDX-License-Identifier: AGPL-3.0-only

// Package autocrypt reads and writes the Autocrypt Level 1 `Autocrypt:` header
// (https://autocrypt.org/level1.html), which is how correspondents exchange
// OpenPGP public keys in-band without either side doing anything deliberate.
//
// It is deliberately PURE: string and base64 handling only. It does not parse
// OpenPGP packets (internal/pgpkeys does that), touch the database, or talk to
// the crypto process. The key material it returns is PUBLIC, so nothing here
// crosses the boundary ADR-024 draws around private keys.
package autocrypt

import (
	"encoding/base64"
	"errors"
	"strings"
)

// HeaderName is the header this package reads and writes. Autocrypt-Gossip is
// deliberately NOT handled: it carries keys for OTHER recipients of a message,
// which is a wider trust decision than "the sender told me their own key", and
// nothing yet needs it.
const HeaderName = "Autocrypt"

// MaxKeyData bounds the decoded key a header may carry. Autocrypt sets no
// limit, so this is ours: a header is attacker-supplied on every inbound
// message, and an unbounded one would let a stranger decide how much memory a
// message costs to ingest. Matches store's own per-key ceiling.
const MaxKeyData = 256 * 1024

// ErrIgnore reports a header that Autocrypt Level 1 says MUST be ignored — as
// opposed to one that is merely absent. The distinction matters: an ignored
// header means "this sender's key claim is not usable", never "this message is
// bad", so a caller must carry on ingesting the message either way.
var ErrIgnore = errors.New("autocrypt: header must be ignored")

// Peer is one sender's self-asserted key, as read off a header.
//
// ⚠ SELF-ASSERTED IS THE WHOLE POINT AND THE WHOLE LIMIT. Anyone who can send
// mail can put any key here under any address. Autocrypt's security model is
// opportunistic — it beats no key at all, and it is not an identity proof.
// Storing one of these must never upgrade what a message is trusted to do.
type Peer struct {
	// Addr is the `addr` attribute: the address this key belongs to. A caller
	// MUST compare it against the message's From address before storing
	// anything — see MatchesFrom.
	Addr string

	// PreferEncrypt reports `prefer-encrypt=mutual`: the sender says they would
	// rather receive encrypted mail. Any other value, including absence, is
	// "nopreference" per the spec, which is the safe reading.
	PreferEncrypt bool

	// KeyData is the decoded BINARY OpenPGP public key ("Transferable Public
	// Key"). Not armored — Autocrypt base64s the binary form, which is also
	// exactly what store.UserKey and WKD want, so nothing re-encodes it.
	KeyData []byte
}

// MatchesFrom reports whether this peer's claimed address is the message's From
// address, compared case-insensitively on the whole address.
//
// ⚠ CALL THIS BEFORE STORING. Without it a sender could attach a key claiming
// to be someone else's address, and a later encrypt-to-contact would use the
// attacker's key for that third party. Level 1 requires the match for exactly
// this reason.
func (p Peer) MatchesFrom(fromAddr string) bool {
	return p.Addr != "" && strings.EqualFold(strings.TrimSpace(p.Addr), strings.TrimSpace(fromAddr))
}

// Parse reads the Autocrypt header(s) of one message. values is EVERY
// `Autocrypt:` header the message carries, in order.
//
// It returns ErrIgnore for the cases Level 1 says are not usable, and a nil
// error with ok=false when the message simply has no Autocrypt header. Callers
// treat both as "no key here" — the split exists so a report can tell "nobody
// sends Autocrypt" from "senders do and we reject them", which are very
// different findings.
func Parse(values []string) (Peer, bool, error) {
	if len(values) == 0 {
		return Peer{}, false, nil
	}
	// ⚠ MORE THAN ONE HEADER MEANS IGNORE ALL OF THEM, per Level 1. Picking the
	// first would let a sender append a second header that some clients honour
	// and others don't, so two parties end up with different keys for the same
	// person — the disagreement is the attack, not either key.
	if len(values) > 1 {
		return Peer{}, false, ErrIgnore
	}

	var (
		p       Peer
		rawKey  string
		sawAddr bool
		sawKey  bool
	)
	for _, attr := range strings.Split(values[0], ";") {
		attr = strings.TrimSpace(attr)
		if attr == "" {
			continue
		}
		name, value, found := strings.Cut(attr, "=")
		if !found {
			return Peer{}, false, ErrIgnore
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)

		switch name {
		case "addr":
			p.Addr, sawAddr = value, true
		case "keydata":
			rawKey, sawKey = value, true
		case "prefer-encrypt":
			// Only "mutual" is defined. Every other value — including a
			// misspelling — is nopreference rather than an error, which is what
			// the spec says and is also the direction that fails safe.
			p.PreferEncrypt = strings.EqualFold(value, "mutual")
		default:
			// ⚠ AN UNKNOWN ATTRIBUTE NOT STARTING WITH "_" IS CRITICAL: Level 1
			// says the whole header must be ignored. That is what makes it safe
			// for a future version to add a meaning-changing attribute — an old
			// parser refuses rather than silently reading a header whose terms
			// it does not understand. Attributes starting with "_" are
			// non-critical and skipped.
			if !strings.HasPrefix(name, "_") {
				return Peer{}, false, ErrIgnore
			}
		}
	}
	if !sawAddr || !sawKey || p.Addr == "" || rawKey == "" {
		return Peer{}, false, ErrIgnore
	}

	// Folding inserts CRLF + leading whitespace inside the base64 run, and some
	// senders also space it out, so all whitespace comes out before decoding.
	rawKey = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, rawKey)

	// Bound the DECODED size before allocating it. base64 is 4:3, so this is
	// the check that actually stops a large header.
	if base64.StdEncoding.DecodedLen(len(rawKey)) > MaxKeyData {
		return Peer{}, false, ErrIgnore
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return Peer{}, false, ErrIgnore
	}
	if len(key) == 0 || len(key) > MaxKeyData {
		return Peer{}, false, ErrIgnore
	}
	p.KeyData = key
	return p, true, nil
}

// foldAt is where Header wraps. RFC 5322 allows 998 octets per line; 72 keeps
// the header readable in a raw message and well inside every implementation's
// tolerance.
const foldAt = 72

// Header builds the value of an outbound `Autocrypt:` header for addr,
// advertising keydata (BINARY OpenPGP public key — store.UserKey.PublicKey is
// already in this form, so callers pass it straight through).
//
// preferMutual sets `prefer-encrypt=mutual`.
//
// ⚠ DO NOT SET preferMutual UNTIL DECRYPTION EXISTS. `mutual` tells every
// Autocrypt-aware correspondent to encrypt to us BY DEFAULT. Mailroom cannot
// decrypt yet (#47, v1.65.0), and an encrypted message today is close to inert:
// the body is unreadable and every attachment is force-quarantined as `unknown`
// (#405). Advertising mutual before then would actively degrade the mailbox on
// the strength of a header we chose to send. Publishing the key WITHOUT the
// preference is the useful half — peers learn the key and can encrypt when they
// deliberately choose to — and flipping this to true is the one-line change
// that belongs with the decrypt work.
func Header(addr string, keydata []byte, preferMutual bool) string {
	var b strings.Builder
	b.WriteString("addr=")
	b.WriteString(addr)
	b.WriteString("; ")
	if preferMutual {
		b.WriteString("prefer-encrypt=mutual; ")
	}
	b.WriteString("keydata=")

	// The header value is folded with CRLF + a single space, the continuation
	// form net/mail and every MTA understands. `used` tracks the current line's
	// length including the "Autocrypt: " the caller will prepend.
	enc := base64.StdEncoding.EncodeToString(keydata)
	used := len(HeaderName) + 2 + b.Len()
	for i := 0; i < len(enc); {
		room := foldAt - used
		if room < 8 {
			b.WriteString("\r\n ")
			used = 1
			room = foldAt - used
		}
		if room > len(enc)-i {
			room = len(enc) - i
		}
		b.WriteString(enc[i : i+room])
		used += room
		i += room
	}
	return b.String()
}
