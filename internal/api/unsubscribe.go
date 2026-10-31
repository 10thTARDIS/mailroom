// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"mime"
	"net/url"
	"strings"
)

// UNSUBSCRIBE DETECTION (#58, matrix §6) — parsing List-Unsubscribe (RFC 2369).
//
// This is SURFACING ONLY. Mailroom never sends the unsubscribe itself, and in
// particular does not implement RFC 8058 one-click: a server-side POST to a URL taken
// out of a message would be Mailroom's FIRST outbound fetch on behalf of a message,
// which docs/hostile-content.md §2.3 says it makes none of, and it would be an SSRF
// surface pointed at whatever an untrusted sender wrote. #58 scopes this to offering
// the affordance, and that boundary is deliberate rather than incidental.
//
// PARSING HAPPENS HERE, ON THE SERVER, and the client receives structured, validated
// pieces. That is the anti-corruption-layer position (spec §3): List-Unsubscribe is a
// structured protocol field written by strangers, and asking the browser to re-derive a
// scheme, decode MIME words and split a mailto's query would be asking it to make the
// same security decision again, in a second language, with a different set of bugs.
//
// THE SHAPE OF REAL MAIL, measured over a real mailbox's messages that carry the header —
// the RFC is not what arrives:
//
//	many carry both a mailto: and an https: entry
//	many carry https: only
//	few  carry mailto: only
//	many carry more than one entry
//	a few carry a BARE URL with no angle brackets at all (RFC 2369 requires <…>)
//	a few arrive as undecoded RFC 2047 encoded-words (=?us-ascii?Q?=3Chttps…)
//	none use plain http://
//	none use any other scheme
//
// So the parser is lenient about the two deviations that actually occur (a strict
// reading would silently drop them) and strict about schemes, where
// leniency is what would hurt.
//
// PLAIN http:// IS REFUSED. In practice no message used it, so the cost is measured and
// zero, while an unsubscribe over cleartext leaks the subscription to the network and is
// trivially rewritten in flight into something else. Where a message offers only an
// http:// link it is reported as having no unsubscribe link — the mailto, if there is
// one, is still offered.

// maxUnsubscribeHeader bounds the header before parsing. The longest real one measured
// is 3920 bytes; this is room for that and pathological input both, and it exists so a
// hostile header cannot make the parser the expensive part of Email/get.
const maxUnsubscribeHeader = 8 << 10

// unsubscribeMailto is a mailto: unsubscribe, split into the parts a compose needs.
//
// The SUBJECT is not decoration: senders key the unsubscribe on it (the measured mailbox
// has subjects like `unsubscribe%3A-BnxdbaHBY…` and whole JWTs), so dropping it produces
// a message that arrives and does nothing.
type unsubscribeMailto struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// unsubscribeTargets is what a message offers. Both halves can be present; a message
// with neither has no unsubscribe and the Email property is null.
type unsubscribeTargets struct {
	// URL is an https: link for the user's own browser to open. Never fetched here.
	URL string `json:"url"`
	// Email is a mailto: unsubscribe, sent through the user's own identity by the
	// ordinary submission path — no new outbound door.
	Email *unsubscribeMailto `json:"email"`
}

// parseListUnsubscribe turns a raw List-Unsubscribe header into what a client can offer,
// or nil when the header offers nothing usable.
//
// Never returns an error: a header this cannot understand means "no unsubscribe
// affordance", which is the same outcome as a message that carries no header at all.
// There is nothing a client could do with a parse error, and a message must never fail
// to load because a stranger wrote a malformed header.
func parseListUnsubscribe(raw string) *unsubscribeTargets {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if len(raw) > maxUnsubscribeHeader {
		raw = raw[:maxUnsubscribeHeader]
	}
	raw = decodeEncodedWords(raw)

	var out unsubscribeTargets
	for _, entry := range unsubscribeEntries(raw) {
		lower := strings.ToLower(entry)
		switch {
		case strings.HasPrefix(lower, "mailto:"):
			if out.Email == nil {
				out.Email = parseUnsubscribeMailto(entry)
			}
		case strings.HasPrefix(lower, "https://"):
			// FIRST https entry wins. Senders list the human-facing preference
			// centre first and machine endpoints after it; picking the first is
			// both what other clients do and the one likeliest to explain itself
			// to a person.
			if out.URL == "" && validUnsubscribeURL(entry) {
				out.URL = entry
			}
		}
		// Every other scheme — http, javascript, data, ftp, anything — is ignored.
		// An allowlist, so a scheme nobody has thought of yet is refused by default.
	}
	if out.URL == "" && out.Email == nil {
		return nil
	}
	return &out
}

// unsubscribeEntries splits the header into candidate targets.
//
// RFC 2369 requires each to be angle-bracketed and comma-separated. Five messages in the
// measured mailbox carry a bare URL instead, so when there is no bracket at all the whole
// value is treated as one candidate — a strict parser drops those, and a user reading one
// of them would see an unsubscribe link in the message body that Mailroom claimed not to
// have.
func unsubscribeEntries(raw string) []string {
	if !strings.Contains(raw, "<") {
		// A bare value. Split on whitespace and commas anyway: it costs nothing and
		// catches "url1, url2" written without brackets.
		fields := strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
		return fields
	}
	var out []string
	rest := raw
	for {
		open := strings.Index(rest, "<")
		if open < 0 {
			break
		}
		rest = rest[open+1:]
		close := strings.Index(rest, ">")
		if close < 0 {
			// An unterminated bracket: take what is left and stop. Better than
			// discarding a truncated-but-usable URL.
			if v := strings.TrimSpace(rest); v != "" {
				out = append(out, v)
			}
			break
		}
		if v := strings.TrimSpace(rest[:close]); v != "" {
			out = append(out, v)
		}
		rest = rest[close+1:]
	}
	return out
}

// decodeEncodedWords MIME-decodes a header that arrived as RFC 2047 encoded-words.
//
// Encoded-words are NOT legal in a structured field like List-Unsubscribe, and three
// messages in the measured mailbox use them anyway — arriving as
// `=?us-ascii?Q?=3Chttps=3A=2F=2F…`, i.e. entirely unusable as stored. Decoding is done
// HERE rather than at ingest deliberately: doing it at ingest would fix new mail and
// leave every message already in the database broken until someone ran a backfill, and a
// read-time decode needs neither a migration nor a backfill to work on all of it.
//
// A value with no encoded-word is returned untouched, and a decode failure returns the
// original — a header we cannot decode is no worse off than before we tried.
func decodeEncodedWords(raw string) string {
	if !strings.Contains(raw, "=?") {
		return raw
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil || strings.TrimSpace(decoded) == "" {
		return raw
	}
	return decoded
}

// validUnsubscribeURL is the last gate before a URL is handed to a client that will put
// it in an href. The scheme is already known to be https; this rejects the shapes that
// are https-prefixed but not a usable absolute URL.
func validUnsubscribeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	// Scheme re-checked against the PARSED value rather than the string prefix: the
	// prefix test says what the header looked like, this says what a browser will
	// actually resolve, and only the second one matters.
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return false
	}
	// A userinfo section (https://evil.example@real.example/) is the classic way to
	// make a URL read as one host and resolve to another. An unsubscribe link has no
	// legitimate use for credentials, so refuse rather than display something
	// misleading.
	return u.User == nil
}

// parseUnsubscribeMailto splits a mailto: into recipient, subject and body.
//
// Returns nil unless there is a recipient — a mailto with no address is a button that
// cannot do anything, which is worse than no button.
func parseUnsubscribeMailto(raw string) *unsubscribeMailto {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	// mailto:'s recipient is in Opaque (mailto: is not a //host-style URL). A leading
	// "//" form is malformed but survivable, so fall back to Path.
	to := strings.TrimSpace(u.Opaque)
	if to == "" {
		to = strings.TrimSpace(strings.TrimPrefix(u.Path, "//"))
	}
	// Some senders put the recipient list before the query without escaping; take the
	// first address and drop the rest rather than mailing several strangers.
	if i := strings.IndexAny(to, ",;"); i >= 0 {
		to = strings.TrimSpace(to[:i])
	}
	if to == "" || !strings.Contains(to, "@") {
		return nil
	}
	q := u.Query()
	return &unsubscribeMailto{
		To:      to,
		Subject: strings.TrimSpace(q.Get("subject")),
		Body:    strings.TrimSpace(q.Get("body")),
	}
}
