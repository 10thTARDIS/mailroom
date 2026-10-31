// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"fmt"
	"strings"
	"time"

	message "github.com/emersion/go-message"
	emmail "github.com/emersion/go-message/mail"
)

// Header exposes decoded, RFC 2047-tolerant access to a message's headers.
// It never exposes go-message types (CLAUDE.md anti-corruption-layer
// rule): every accessor returns a Mailroom-owned type, and every accessor
// is panic-safe and best-effort per docs/mime-policy.md — malformed input
// degrades to a raw-text fallback, never an error the caller must handle
// and never a crash.
type Header struct {
	h emmail.Header
}

// newHeader wraps a go-message header and eagerly resolves the address
// headers once, so any fallback used can be recorded in the returned
// warnings (the typed accessors below recompute deterministically, so this
// does not change what callers see — it only surfaces *why* a fallback
// happened, once, at parse time).
func newHeader(raw message.Header) (hdr Header, warnings []string) {
	hdr = Header{h: emmail.Header{Header: raw}}
	for _, key := range []string{"From", "To", "Cc", "Bcc", "Reply-To"} {
		if _, warn := safeAddressList(hdr.h, key); warn != "" {
			warnings = append(warnings, warn)
		}
	}
	return hdr, warnings
}

// Get returns the raw value of the named header field (RFC 2047
// encoded-words are NOT decoded), or "" if absent. It is the fallback of
// last resort for headers not covered by the typed accessors below, and
// still applies the unencoded-8-bit-bytes policy (docs/mime-policy.md).
func (hd Header) Get(key string) (v string) {
	defer func() {
		if recover() != nil {
			v = ""
		}
	}()
	return sanitizeText(hd.h.Get(key))
}

// Values returns every value of a repeated header field, in the order the
// message carried them (topmost first — which for a Delivered-To chain means
// the LAST hop first, since each hop prepends its own trace headers). RFC 2047
// encoded-words are NOT decoded. Absent header -> nil.
//
// It exists for the trace headers alias derivation depends on (Delivered-To,
// X-Original-To, X-Forwarded-To — spec §4/§5), which a forwarding chain repeats
// and Get would silently collapse to just one of.
func (hd Header) Values(key string) (vals []string) {
	defer func() {
		if recover() != nil {
			vals = nil
		}
	}()
	raw := hd.h.Values(key)
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s := sanitizeText(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// FieldCount returns how many header fields named key the message carries —
// EVERY one, including empty and unparsable ones, which Values drops after
// sanitising. It is the question an authentication decision must ask before
// believing a parsed From (#725): a message with two From fields has two
// claimed authors, and which one a receiving MTA's DMARC verdict was about
// is not something From() can say. Never panics.
func (hd Header) FieldCount(key string) (n int) {
	defer func() {
		if recover() != nil {
			n = 0
		}
	}()
	return len(hd.h.Values(key))
}

// Subject returns the decoded Subject header. A malformed RFC 2047
// encoded-word falls back to its raw token text; never panics.
func (hd Header) Subject() string {
	return sanitizeText(safeHeaderText(hd.h.Header, "Subject"))
}

// From returns the decoded From address list. Never panics; a malformed
// address list falls back to a single synthetic Address carrying the raw
// header text as its Name.
func (hd Header) From() []Address {
	addrs, _ := safeAddressList(hd.h, "From")
	return addrs
}

// To returns the decoded To address list. See From for fallback behavior.
func (hd Header) To() []Address {
	addrs, _ := safeAddressList(hd.h, "To")
	return addrs
}

// Cc returns the decoded Cc address list. See From for fallback behavior.
func (hd Header) Cc() []Address {
	addrs, _ := safeAddressList(hd.h, "Cc")
	return addrs
}

// Bcc returns the decoded Bcc address list. See From for fallback behavior.
func (hd Header) Bcc() []Address {
	addrs, _ := safeAddressList(hd.h, "Bcc")
	return addrs
}

// ReplyTo returns the decoded Reply-To address list. See From for fallback
// behavior.
func (hd Header) ReplyTo() []Address {
	addrs, _ := safeAddressList(hd.h, "Reply-To")
	return addrs
}

// Sender returns the decoded Sender address list (RFC 5322 §3.6.2: the agent
// that actually submitted the message, when that differs from the author). See
// From for fallback behavior.
func (hd Header) Sender() []Address {
	addrs, _ := safeAddressList(hd.h, "Sender")
	return addrs
}

// Date returns the parsed Date header and true, or the zero time and false
// if the header is absent or unparsable. Mailroom does not fabricate a
// fallback date: a malformed Date genuinely has no usable timestamp.
func (hd Header) Date() (t time.Time, ok bool) {
	defer func() {
		if recover() != nil {
			t, ok = time.Time{}, false
		}
	}()
	v, err := hd.h.Date()
	if err != nil || v.IsZero() {
		return time.Time{}, false
	}
	return v, true
}

// MessageID returns the Message-Id header with angle brackets stripped, or
// "" if absent. A malformed Message-Id falls back to the raw header text
// with any surrounding angle brackets and whitespace trimmed.
func (hd Header) MessageID() (id string) {
	defer func() {
		if recover() != nil {
			id = fallbackMsgID(hd.h.Get("Message-Id"))
		}
	}()
	v, err := hd.h.MessageID()
	if err != nil {
		return fallbackMsgID(hd.h.Get("Message-Id"))
	}
	return sanitizeText(v)
}

// References returns the References header as a list of message
// identifiers (angle brackets stripped). Returns whatever prefix parsed
// successfully if the header is partially malformed; falls back to the raw
// header text as a single element if none of it could be parsed.
func (hd Header) References() []string {
	return safeMsgIDList(hd.h, "References")
}

// InReplyTo returns the In-Reply-To header as a list of message
// identifiers. See References for fallback behavior.
func (hd Header) InReplyTo() []string {
	return safeMsgIDList(hd.h, "In-Reply-To")
}

// safeHeaderText decodes a single header field's RFC 2047 encoded-words,
// falling back to the raw value on any error, and recovering from any
// panic in the underlying decoder.
func safeHeaderText(h message.Header, key string) (text string) {
	defer func() {
		if recover() != nil {
			text = h.Get(key)
		}
	}()
	v, err := h.Text(key)
	if err != nil {
		return h.Get(key)
	}
	return v
}

// safeAddressList decodes an address-list header field, falling back to a
// single synthetic Address wrapping the raw text on any parse error, and
// recovering from any panic in the underlying parser. warn is non-empty
// iff a fallback was used.
func safeAddressList(h emmail.Header, key string) (addrs []Address, warn string) {
	defer func() {
		if r := recover(); r != nil {
			addrs, warn = rawAddressFallback(h.Get(key)), fmt.Sprintf("mime: recovered parsing address list %q: %v", key, r)
		}
	}()
	list, err := h.AddressList(key)
	if err != nil {
		raw := h.Get(key)
		if raw == "" {
			return nil, ""
		}
		return rawAddressFallback(raw), fmt.Sprintf("mime: address list %q fell back to raw text: %v", key, err)
	}
	if len(list) == 0 {
		// AddressList returns (nil, nil) for a missing header; keep that
		// nil-means-absent contract instead of surfacing an allocated
		// empty slice.
		return nil, ""
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		if a == nil {
			continue
		}
		addr := Address{Name: sanitizeText(a.Name), Address: sanitizeText(a.Address)}
		addr.EAI = AddressRequiresEAI(addr.Address)
		out = append(out, addr)
	}
	return out, ""
}

func rawAddressFallback(raw string) []Address {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return []Address{{Name: sanitizeText(raw)}}
}

func fallbackMsgID(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<")
	raw = strings.TrimSuffix(raw, ">")
	return sanitizeText(raw)
}

func safeMsgIDList(h emmail.Header, key string) (ids []string) {
	defer func() {
		if recover() != nil {
			ids = rawMsgIDFallback(h.Get(key))
		}
	}()
	list, err := h.MsgIDList(key)
	if len(list) > 0 {
		out := make([]string, len(list))
		for i, id := range list {
			out[i] = sanitizeText(id)
		}
		return out
	}
	if err != nil {
		return rawMsgIDFallback(h.Get(key))
	}
	return nil
}

func rawMsgIDFallback(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return []string{sanitizeText(raw)}
}
