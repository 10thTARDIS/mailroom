// SPDX-License-Identifier: AGPL-3.0-only

// Package xoauth2 is the wire format of the XOAUTH2 SASL mechanism that Gmail
// and Microsoft accept on IMAP and SMTP (ADR-046), and the redacting Secret type
// every OAuth token in Mailroom travels in.
//
// It is a LEAF on purpose. internal/sync (IMAP) and internal/provider (SMTP)
// each wrap their own protocol library behind their own interface (spec §3),
// and both need the same twenty lines of encoding; internal/oauthmail, which
// owns grants and refresh, imports the store and so cannot be imported by
// either. Nothing here does I/O.
//
// The mechanism, as Google documents it and Microsoft copies it:
//
//	initial response = "user=" USER ^A "auth=Bearer " TOKEN ^A ^A
//
// sent as the SASL initial response. On failure the server does not answer NO
// at once: it sends a continuation carrying a base64 JSON object
// ({"status":"401","schemes":"bearer","scope":"..."}) and waits, and the client
// MUST answer with an empty response before the server sends its tagged NO
// (IMAP) or 535 (SMTP). A client that does not answer hangs the connection.
// Microsoft's documented failure skips the challenge and answers a bare
// `NO AUTHENTICATE failed.`; both paths must end as an authentication error.
package xoauth2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const redacted = "[redacted]"

// Secret is an OAuth token (access or refresh) or client secret. When the
// standard library can call its methods — a Secret value, an EXPORTED field
// holding one, a slog attribute — every rendering (%v, %s, %q, %x, %#v, slog,
// JSON, text marshalling) prints "[redacted]". The plaintext is reachable only
// through Reveal, and every call site of Reveal is a place a token crosses a
// boundary on purpose (the wire, the sealed envelope).
//
// ⚠ THE UNEXPORTED-FIELD RULE. fmt (and slog's text handler, which uses fmt)
// renders an UNEXPORTED struct field by reflection without calling its
// methods, so a Secret stored in an unexported field prints in the clear the
// moment anything formats the struct around it with %v/%+v/%#v — however
// deeply nested. Exporting the field does not fix it either, because an outer
// struct may hold the inner one in an unexported field. So a Secret must
// never be stored in an unexported field: hold it in a Held (below), whose
// only field is a pointer, which reflection prints as an address.
//
// ⚠ It cannot be zeroed: it is a Go string, and the runtime may have copied it.
// ADR-046 states this residual risk rather than pretending otherwise.
type Secret string

// String implements fmt.Stringer (covers %v, %s, %q, %x and Print).
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, so %#v redacts too.
func (Secret) GoString() string { return `"` + redacted + `"` }

// Format implements fmt.Formatter, so no verb (not even %d or %T-adjacent
// width tricks) can reach the underlying bytes.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON redacts. Sealing a token uses a plain string field, never a
// Secret, so this cannot corrupt the envelope.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText redacts (covers encoding/xml, text-based loggers, map keys).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Reveal returns the plaintext. Call sites, kept short on purpose:
//
//   - InitialResponse, building the bytes that go on the IMAP/SMTP wire;
//   - internal/oauthmail, POSTing a refresh token to the provider's token
//     endpoint and sealing a rotated one into the account envelope;
//   - redaction itself (Scrub), which must know what to remove.
func (s Secret) Reveal() string { return string(s) }

// Held is how a Secret is stored in an unexported struct field (see the
// unexported-field rule on Secret). Its only field is a pointer, so even fmt's
// method-less reflection over an enclosing struct prints an address, never the
// token; and its own methods redact when they can be called.
type Held struct{ p *Secret }

// Hold wraps s for storage.
func Hold(s Secret) Held { return Held{p: &s} }

// Get returns the held secret ("" when nothing is held).
func (h Held) Get() Secret {
	if h.p == nil {
		return ""
	}
	return *h.p
}

// String implements fmt.Stringer.
func (Held) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Held) GoString() string { return `xoauth2.Held{` + redacted + `}` }

// Format implements fmt.Formatter for every verb.
func (Held) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (Held) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON redacts.
func (Held) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Empty reports whether the secret is unset, without revealing it.
func (s Secret) Empty() bool { return s == "" }

// Hosts are a provider's mail endpoints as its registry entry fixes them
// (ADR-046 §1). An OAuth account is only ever dialled at these, never at
// whatever its account row says.
type Hosts struct {
	IMAPHost        string
	IMAPPort        int
	SMTPHost        string
	SMTPPort        int
	SMTPImplicitTLS bool
}

// ErrHostMismatch: an OAuth account's row names a host other than its
// provider's. Refused rather than silently overridden, so the inconsistency
// is seen.
var ErrHostMismatch = errors.New("xoauth2: the account's mail server is not its OAuth provider's")

// TokenSource issues access tokens for one account and hears back when one is
// refused (ADR-046 §5, obligation (a) of §9).
//
//   - Token returns a currently valid access token. It is called once per
//     authentication attempt, at authentication time, so a long-lived Config
//     never carries a stale token.
//   - Reject reports that a server refused tok. The source drops it if it is
//     still the cached token, so the NEXT Token call refreshes instead of
//     handing the same dead token back. The protocol layers call it on a
//     credential verdict only (an IMAP AUTHENTICATE refusal, an SMTP 535 after
//     XOAUTH2) and then retry exactly once: Reject never starts a loop.
type TokenSource interface {
	Token(ctx context.Context) (Secret, error)
	Reject(tok Secret)
}

// StaticToken is a TokenSource that always returns tok and ignores Reject: a
// token minted for one check (the connect flow's mailbox check) or a test.
func StaticToken(tok Secret) TokenSource { return staticToken{h: Hold(tok)} }

// staticToken holds its token as Held (the unexported-field rule above).
type staticToken struct{ h Held }

func (s staticToken) Token(context.Context) (Secret, error) {
	if s.h.Get().Empty() {
		return "", errors.New("xoauth2: no access token")
	}
	return s.h.Get(), nil
}
func (staticToken) Reject(Secret)                {}
func (staticToken) String() string               { return "xoauth2.StaticToken{" + redacted + "}" }
func (s staticToken) GoString() string           { return s.String() }
func (s staticToken) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }
func (s staticToken) LogValue() slog.Value       { return slog.StringValue(s.String()) }

// InitialResponse is the XOAUTH2 SASL initial response for user and token.
// It refuses a user or token containing the ^A separator, which would let one
// field smuggle the other's syntax.
func InitialResponse(user string, token Secret) ([]byte, error) {
	if user == "" {
		return nil, errors.New("xoauth2: user is required")
	}
	if token.Empty() {
		return nil, errors.New("xoauth2: access token is required")
	}
	if strings.ContainsRune(user, '\x01') || strings.ContainsRune(token.Reveal(), '\x01') {
		return nil, errors.New("xoauth2: user or token contains a control separator")
	}
	return []byte("user=" + user + "\x01auth=Bearer " + token.Reveal() + "\x01\x01"), nil
}

// ChallengeStatus extracts the "status" member of the server's failure
// challenge (already base64-decoded). Only the status is read: the challenge
// also names the scope and schemes, and nothing else from it is ever logged or
// put in an error (ADR-046 §threat model). ok is false when the challenge is
// not the documented JSON shape; the caller still answers it with an empty
// response, because the protocol requires that whatever the server sent.
func ChallengeStatus(challenge []byte) (status string, ok bool) {
	var c struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(challenge, &c); err != nil || c.Status == "" {
		return "", false
	}
	// A status is a short code ("401", "400", "invalid_token"); cap and strip
	// it so a hostile server cannot use it to inject text into our errors.
	s := c.Status
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s), true
}

// Scrub returns s with every non-empty secret in secrets replaced by
// "[redacted]". It is the defence for text that came FROM a server — an OAuth
// error_description, an IMAP/SMTP refusal — and is about to go into an error
// or log line: a server that echoes the token it was sent must not make us
// leak it.
func Scrub(s string, secrets ...Secret) string {
	for _, sec := range secrets {
		if v := sec.Reveal(); v != "" {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

// The three outcomes a token request can have, as sentinels the protocol
// layers can classify without importing internal/oauthmail (which wraps its
// errors so errors.Is matches exactly one of these).
var (
	// ErrReauthRequired: the grant is dead (revoked, expired, password
	// changed, evicted). Only the user can fix it, by connecting again.
	ErrReauthRequired = errors.New("xoauth2: the account must be reconnected")
	// ErrProviderMisconfigured: the provider refused THIS SERVER (client id or
	// secret wrong or expired, provider not configured). Only the operator can
	// fix it; telling the user to reconnect would be a lie.
	ErrProviderMisconfigured = errors.New("xoauth2: the server's sign-in configuration for this provider is not working")
	// ErrTokenUnavailable: a transient failure to obtain a token (network,
	// provider 5xx, timeout). Retry with backoff.
	ErrTokenUnavailable = errors.New("xoauth2: could not obtain an access token right now")
)
