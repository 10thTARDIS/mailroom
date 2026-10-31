// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// This file builds the RFC 5322 bytes Mailroom sends and files. It is the ONLY
// place message bytes are authored, so the headers spec §5 calls out are correct
// by construction:
//
//   - Message-ID is generated UNDER THE SENDING IDENTITY'S DOMAIN. "Mismatched
//     Message-ID domains cost spam-filter points" (spec §5) — a message From
//     store@example.com whose Message-ID says @mailroom.internal looks forged to
//     a filter, so the domain is taken from the From address itself, never from
//     the server's hostname (which is also why nothing here reads os.Hostname:
//     that would leak the operator's infrastructure into every outgoing message,
//     which CLAUDE.md forbids independently).
//   - In-Reply-To is the parent's Message-ID; References is the parent's
//     References chain plus the parent's Message-ID (RFC 5322 §3.6.4), capped so
//     a long thread cannot grow an unbounded header.
//   - Date is always present and always UTC.
//
// The bytes are built once and then immutable: they are the message's identity
// (ADR-001: the content hash of exactly these bytes), so anything that would
// change them — an edit to a draft — produces a NEW message, not a mutation.

// maxReferences caps the References chain. RFC 5322 sets no limit, but a
// hundreds-deep chain bloats every message in a long thread; 20 is well past
// what any threading algorithm needs and keeps the header sane.
const maxReferences = 20

// ErrNoRecipients is returned when a message has no To/Cc/Bcc at all: there is
// nobody to send it to, and a submission with an empty envelope is a bug, not a
// send.
var ErrNoRecipients = errors.New("compose: message has no recipients")

// Address is a compose-time address. Name may be empty.
type Address struct {
	Name  string
	Email string
}

// String renders the address as an RFC 5322 name-addr. The display name is
// written as-is only when it is a plain phrase of atoms; otherwise it is
// quoted (ASCII) or RFC 2047-encoded (non-ASCII).
//
// ⚠ THE NAME IS DATA, NOT SYNTAX (#689). Contact names are harvested from
// decoded headers, so `"Doe, Jane" <r@example.com>` arrives as the name
// `Doe, Jane`. Written back unquoted, that comma split the To field into two
// list members, the parser fell back to a synthetic address with no mailbox,
// and the submit path refused the send with "the message has no recipients" —
// for exactly the recipients picked from the suggestions, because a typed bare
// address has no name. TestDisplayNamesRoundTripThroughTheBytes holds the rule:
// whatever the name, the bytes must parse back to the same mailbox and name.
func (a Address) String() string {
	name := sanitizeHeaderValue(a.Name)
	if name == "" {
		return a.Email
	}
	return encodeDisplayName(name) + " <" + a.Email + ">"
}

// encodeDisplayName renders a display name as an RFC 5322 phrase.
func encodeDisplayName(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			// An encoded-word in a phrase may carry only a restricted set of
			// characters (RFC 2047 §5(3)); the Q encoder leaves specials such as
			// a comma literal, which breaks the phrase all over again. The B
			// encoder carries anything. This is the choice net/mail makes.
			if strings.ContainsAny(name, "\"#$%&'(),.:;<>@[]^`{|}~\\") {
				return mime.BEncoding.Encode("utf-8", name)
			}
			return mime.QEncoding.Encode("utf-8", name)
		}
	}
	if isAtomPhrase(name) {
		return name // byte-identical to what every plain name always produced
	}
	var b strings.Builder
	b.Grow(len(name) + 2)
	b.WriteByte('"')
	for i := 0; i < len(name); i++ {
		if c := name[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(name[i])
	}
	b.WriteByte('"')
	return b.String()
}

// isAtomPhrase reports whether s is one or more RFC 5322 atoms separated by
// single spaces, and so needs no quoting. An encoded-word lookalike (`=?`) is
// quoted too, so a literal name is not decoded into something else.
func isAtomPhrase(s string) bool {
	if s == "" || s[0] == ' ' || s[len(s)-1] == ' ' || strings.Contains(s, "  ") ||
		strings.Contains(s, "=?") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ',
			'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// Draft is the compose input: what the user wrote, plus the threading context
// of the message being replied to (if any). It is deliberately free of any
// server/identity notion beyond the From address — the caller has already
// resolved (and AUTHORIZED) that.
type Draft struct {
	From    Address
	To      []Address
	Cc      []Address
	Bcc     []Address
	Subject string

	// TextBody is the plain-text body; HTMLBody the rich one. With both, the
	// message is multipart/alternative (text first, per RFC 2046 §5.1.4).
	TextBody string
	HTMLBody string

	// InReplyTo is the parent's Message-ID (no angle brackets), and
	// References the parent's References chain (no angle brackets). Both come
	// from the Email being replied to.
	InReplyTo  string
	References []string

	// Date stamps the message. Zero means "now".
	Date time.Time

	// MessageID overrides the generated one. Only set when re-building bytes
	// whose Message-ID must be preserved (a draft being re-saved keeps its id so
	// the thread does not fragment); leave zero for a new message.
	MessageID string

	// Attachments make the message multipart/mixed: the body (whatever it would
	// otherwise have been) becomes its first part, and each attachment follows.
	Attachments []Attachment

	// Calendar, when set, makes the body an iMIP message (RFC 6047 §2.4): a
	// multipart/alternative of TextBody and a text/calendar part carrying
	// Calendar.Data with its METHOD as the content-type parameter. HTMLBody is
	// not used alongside it. The bytes are written as given — internal/ical
	// built them and owns their escaping.
	Calendar *CalendarPart
}

// CalendarPart is the text/calendar body part of an iMIP message.
type CalendarPart struct {
	// Method is the iTIP method (REPLY), repeated as the content-type
	// parameter RFC 6047 §2.4 requires to match the calendar's own METHOD.
	Method string
	Data   []byte
}

// Attachment is one part to attach, with its bytes already in hand. The caller
// is responsible for having AUTHORIZED those bytes (internal/api resolves a JMAP
// blobId to bytes it has proven the authenticated user owns); this package
// cannot check ownership and never pretends to.
type Attachment struct {
	// Name is the filename shown to the recipient. Non-ASCII names are RFC 2231
	// encoded; a name that cannot be encoded is dropped rather than mangled.
	Name string
	// Type is the media type ("application/pdf"). Empty means
	// application/octet-stream.
	Type string
	// Data is the decoded content; it is base64-encoded into the message.
	Data []byte
	// ContentID, when set, makes the part referenceable from HTML as cid:… and
	// disposes it inline (that is the ONLY thing that makes an embedded image
	// render in a recipient's client).
	ContentID string
	// Inline marks a part as inline rather than an attachment (implied by
	// ContentID).
	Inline bool
}

// Built is the result of building a draft: the raw RFC 5322 bytes plus the
// header values the store needs to project (they are re-derivable from the
// bytes, but the builder already knows them).
type Built struct {
	Raw        []byte
	MessageID  string // no angle brackets
	InReplyTo  string
	References []string
	Date       time.Time
	Subject    string
	Preview    string
	// EnvelopeTo is every recipient (To + Cc + Bcc) — the SMTP RCPT TO set.
	EnvelopeTo []string
	// HasAttachments is the projection app.messages.has_attachments wants (it is
	// what puts the paperclip on the list row).
	HasAttachments bool
}

// Build renders a draft to RFC 5322 bytes.
//
// Bcc handling. The STORED bytes keep the Bcc header: a draft the user resumes
// (here or on their phone, via the IMAP Drafts folder) must still know who was
// blind-copied, and the filed Sent copy must still show it — that is what every
// mail client does, and dropping it would silently lose the user's data. The
// TRANSMITTED bytes must not carry it: SendPayload strips the header just
// before SMTP DATA, so the blind recipients are disclosed to nobody. RFC 5322
// §3.6.3 permits either; this split is the only combination that is both
// non-lossy and non-leaky.
func Build(d Draft) (Built, error) {
	if d.From.Email == "" {
		return Built{}, errors.New("compose: message has no From address")
	}
	// ⚠ NO RECIPIENT CHECK HERE, AND THAT IS THE POINT (#601).
	//
	// `Build` builds a DRAFT — it is called from exactly one place,
	// Composer.SaveDraft — and a draft with nobody in the To field is an
	// ordinary, common thing: you write the note at the top of a forward before
	// you decide who needs it. Refusing it here meant the client could not
	// autosave one, so everything typed before a recipient was added was
	// discarded when the composer closed, silently. RFC 5322 agrees: destination
	// fields are optional; only the originator and Date are required.
	//
	// THE RULE HAS NOT BEEN REMOVED, IT HAS MOVED TO WHERE IT IS TRUE. "There is
	// nobody to send it to" is a fact about a SUBMISSION, and the submit path
	// enforces it independently in Composer.PlanSubmit, which refuses an empty
	// envelope with this same ErrNoRecipients. A draft that never becomes a
	// submission was never going to reach anyone.
	//
	// So ErrNoRecipients still exists and is still returned — by the path whose
	// name it describes. `TestPlanSubmitStillRefusesADraftWithNoRecipients` is
	// the control that stops this being a guard deleted rather than relocated.
	for _, a := range concatAddrs(d.To, d.Cc, d.Bcc, []Address{d.From}) {
		if _, err := mail.ParseAddress(a.Email); err != nil {
			return Built{}, fmt.Errorf("compose: %q is not a valid address: %w", a.Email, err)
		}
	}

	when := d.Date
	if when.IsZero() {
		when = time.Now()
	}
	when = when.UTC()

	msgID := d.MessageID
	if msgID == "" {
		var err error
		if msgID, err = NewMessageID(d.From.Email); err != nil {
			return Built{}, err
		}
	}
	refs := buildReferences(d.References, d.InReplyTo)

	var b strings.Builder
	writeHeader(&b, "Date", when.Format(time.RFC1123Z))
	writeHeader(&b, "Message-ID", "<"+msgID+">")
	writeHeader(&b, "From", d.From.String())
	if len(d.To) > 0 {
		writeHeader(&b, "To", addrList(d.To))
	}
	if len(d.Cc) > 0 {
		writeHeader(&b, "Cc", addrList(d.Cc))
	}
	if len(d.Bcc) > 0 {
		writeHeader(&b, "Bcc", addrList(d.Bcc))
	}
	writeHeader(&b, "Subject", encodeSubject(d.Subject))
	if d.InReplyTo != "" {
		writeHeader(&b, "In-Reply-To", "<"+d.InReplyTo+">")
	}
	if len(refs) > 0 {
		writeHeader(&b, "References", bracketList(refs))
	}
	writeHeader(&b, "MIME-Version", "1.0")

	// With attachments the whole message becomes multipart/mixed and the body —
	// text, HTML, or the alternative pair — becomes its FIRST part. Without them
	// nothing changes: the body's headers are the message's headers, which is what
	// every existing test (and every already-composed message) expects.
	if len(d.Attachments) > 0 {
		mixed, err := newBoundary()
		if err != nil {
			return Built{}, err
		}
		writeHeader(&b, "Content-Type", `multipart/mixed; boundary="`+mixed+`"`)
		b.WriteString("\r\n")
		b.WriteString("--" + mixed + "\r\n")
		if err := writeBody(&b, d); err != nil {
			return Built{}, err
		}
		for _, at := range d.Attachments {
			b.WriteString("--" + mixed + "\r\n")
			writeAttachment(&b, at)
		}
		b.WriteString("--" + mixed + "--\r\n")
	} else if err := writeBody(&b, d); err != nil {
		return Built{}, err
	}

	preview := d.TextBody
	if preview == "" {
		preview = d.HTMLBody
	}
	return Built{
		Raw:            []byte(b.String()),
		MessageID:      msgID,
		InReplyTo:      d.InReplyTo,
		References:     refs,
		Date:           when,
		Subject:        d.Subject,
		Preview:        preview,
		EnvelopeTo:     envelopeTo(d),
		HasAttachments: len(d.Attachments) > 0,
	}, nil
}

// writeBody writes the message body as ONE MIME entity: its Content-Type header
// block, a blank line, then the content. It is used both as the top-level body
// (where its headers join the message header block) and as the first part of a
// multipart/mixed — the same bytes either way, which is why attaching a file
// cannot change how the body itself is rendered.
//
// d.HTMLBody is written VERBATIM here. That is safe only because the API trust
// boundary has already run it through SanitizeOutboundHTML before it reached a
// Draft (internal/api/draft.go): this package authors bytes, it does not police
// their content, and it cannot import the sanitizer without a cycle (api imports
// compose). A caller that fills HTMLBody from anything other than that path owns
// sanitizing it first.
func writeBody(b *strings.Builder, d Draft) error {
	switch {
	case d.Calendar != nil:
		method := strings.ToUpper(d.Calendar.Method)
		if method == "" {
			return errors.New("compose: calendar part has no method")
		}
		for _, r := range method {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("compose: calendar method %q", d.Calendar.Method)
			}
		}
		boundary, err := newBoundary()
		if err != nil {
			return err
		}
		writeHeader(b, "Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
		b.WriteString("\r\n")
		writePart(b, boundary, "text/plain; charset=utf-8", d.TextBody)
		writePart(b, boundary, "text/calendar; charset=utf-8; method="+method, string(d.Calendar.Data))
		b.WriteString("--" + boundary + "--\r\n")
	case d.HTMLBody != "" && d.TextBody != "":
		boundary, err := newBoundary()
		if err != nil {
			return err
		}
		writeHeader(b, "Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
		b.WriteString("\r\n")
		writePart(b, boundary, "text/plain; charset=utf-8", d.TextBody)
		writePart(b, boundary, "text/html; charset=utf-8", d.HTMLBody)
		b.WriteString("--" + boundary + "--\r\n")
	case d.HTMLBody != "":
		writeTextPart(b, "text/html; charset=utf-8", d.HTMLBody)
	default:
		writeTextPart(b, "text/plain; charset=utf-8", d.TextBody)
	}
	return nil
}

// bodyLineLimit is RFC 5322 §2.1.1's maximum line length, excluding the CRLF.
// A longer line cannot honestly be declared 7bit, and quoted-printable's soft
// line breaks are how it becomes transmissible without altering the content.
const bodyLineLimit = 998

// writeTextPart writes a text entity: its Content-Type, a transfer encoding
// that SURVIVES A 7-BIT HOP, and the content in that encoding.
//
// ⚠ THIS USED TO WRITE `8bit` UNCONDITIONALLY, WHICH WAS A SIGNATURE BUG WAITING
// TO HAPPEN (#47 increment 5). RFC 3156 §3 requires the signed part to be in an
// encoding that survives a 7-bit hop. With `8bit`, ONE non-ASCII byte — an
// em-dash, an accent, a non-Latin name — is enough for the first MTA without
// 8BITMIME to re-encode the body in transit, and re-encoded bytes do not match
// the signature. ⚠ The failure is INVISIBLE FROM THIS SIDE: we log a successful
// send, and the recipient sees "bad signature" with no way to tell us.
//
// It is also a plain interoperability fix that stands on its own, signing or
// not: `8bit` is an extension an MTA is not obliged to carry, and content that
// needs it has always been at the mercy of the next hop.
//
// `7bit` is still declared when the content genuinely IS 7-bit, so ordinary
// ASCII mail keeps exactly the bytes it had before this change and stays
// readable in the raw. The switch is not cosmetic: it is the difference between
// a claim about the content that happens to be true and one that is checked.
func writeTextPart(b *strings.Builder, contentType, body string) {
	content := normalizeEOL(body)
	writeHeader(b, "Content-Type", contentType)
	if is7BitSafe(content) {
		writeHeader(b, "Content-Transfer-Encoding", "7bit")
		b.WriteString("\r\n")
		b.WriteString(content)
		return
	}
	writeHeader(b, "Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	b.WriteString(quotedPrintable(content))
}

// is7BitSafe reports whether content may legally be declared `7bit`: US-ASCII
// only, no NUL, and no line over RFC 5322's limit.
//
// All three conditions are load-bearing. The first two are what `7bit` means;
// the third is the one that is easy to forget, and a single 999-character line —
// which a pasted URL or a machine-generated paragraph reaches without trying —
// is enough for an MTA to fold the body and break a signature over it.
func is7BitSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 || s[i] == 0 {
			return false
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if len(line) > bodyLineLimit {
			return false
		}
	}
	return true
}

// quotedPrintable encodes content per RFC 2045 §6.7.
//
// The stdlib writer is used rather than a hand-rolled encoder because the parts
// that are easy to get wrong — soft line breaks at 76 columns, and encoding
// TRAILING whitespace so it cannot be stripped in transit — are exactly the
// parts it already gets right. Measured rather than assumed: it emits CRLF for
// both LF and CRLF input, never a bare LF, never a line over 76, and never a
// byte above 127.
func quotedPrintable(s string) string {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	// A bytes.Buffer write cannot fail and Close only flushes, so neither error
	// can be non-nil here; they are discarded rather than dressed up in a
	// returned error the caller would have to pretend to handle.
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return buf.String()
}

// writeAttachment writes one attachment part: base64, with the filename encoded
// per RFC 2231 (mime.FormatMediaType) so a non-ASCII name survives, and with
// every header value passed through sanitizeHeaderValue — an attachment's name
// and type come from a client, and a CRLF in either would otherwise forge
// headers into the message we are about to send.
func writeAttachment(b *strings.Builder, at Attachment) {
	ct := strings.TrimSpace(at.Type)
	if ct == "" {
		ct = "application/octet-stream"
	}
	params := map[string]string{}
	if at.Name != "" {
		params["name"] = at.Name
	}
	if formatted := mime.FormatMediaType(ct, params); formatted != "" {
		writeHeader(b, "Content-Type", formatted)
	} else {
		writeHeader(b, "Content-Type", "application/octet-stream")
	}
	writeHeader(b, "Content-Transfer-Encoding", "base64")

	disposition := "attachment"
	if at.Inline || at.ContentID != "" {
		disposition = "inline"
	}
	dparams := map[string]string{}
	if at.Name != "" {
		dparams["filename"] = at.Name
	}
	if formatted := mime.FormatMediaType(disposition, dparams); formatted != "" {
		writeHeader(b, "Content-Disposition", formatted)
	} else {
		writeHeader(b, "Content-Disposition", disposition)
	}
	if at.ContentID != "" {
		writeHeader(b, "Content-ID", "<"+strings.Trim(at.ContentID, "<>")+">")
	}
	b.WriteString("\r\n")
	writeBase64(b, at.Data)
}

// base64LineLen is the RFC 2045 §6.8 line length for base64 body parts.
const base64LineLen = 76

// writeBase64 writes data as base64 wrapped at 76 columns with CRLF endings.
func writeBase64(b *strings.Builder, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > base64LineLen {
		b.WriteString(enc[:base64LineLen])
		b.WriteString("\r\n")
		enc = enc[base64LineLen:]
	}
	if len(enc) > 0 {
		b.WriteString(enc)
		b.WriteString("\r\n")
	}
}

// SendPayload returns the bytes to hand to SMTP DATA: the stored message with
// its Bcc header removed (see Build). Removing the header — rather than never
// writing it — is what lets the STORED copy stay complete while the TRANSMITTED
// one leaks nothing, and it works for drafts Mailroom did not author (one
// composed on a phone and synced in from the IMAP Drafts folder still carries a
// Bcc header that must not go out on the wire).
//
// It removes the field and any folded continuation lines, and touches nothing
// else: the remaining bytes are byte-for-byte what was stored, so DKIM-relevant
// header order and body bytes are undisturbed.
func SendPayload(raw []byte) []byte {
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	sep := 4
	if headerEnd < 0 {
		if headerEnd = bytes.Index(raw, []byte("\n\n")); headerEnd < 0 {
			return raw // no header/body split: nothing safe to do
		}
		sep = 2
	}
	head, body := raw[:headerEnd+sep], raw[headerEnd+sep:]

	var out bytes.Buffer
	out.Grow(len(raw))
	skipping := false
	for _, line := range splitLinesKeepEnd(head) {
		trimmed := bytes.TrimRight(line, "\r\n")
		isContinuation := len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t')
		if skipping && isContinuation {
			continue // folded continuation of the Bcc field
		}
		skipping = false
		if hasFieldName(trimmed, "bcc") {
			skipping = true
			continue
		}
		out.Write(line)
	}
	out.Write(body)
	return out.Bytes()
}

// hasFieldName reports whether a header line starts the named field
// (case-insensitive, per RFC 5322 §1.2.2).
func hasFieldName(line []byte, name string) bool {
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return false
	}
	return strings.EqualFold(string(bytes.TrimSpace(line[:colon])), name)
}

// splitLinesKeepEnd splits on '\n', keeping the terminator on each line so the
// output can be reassembled byte-for-byte.
func splitLinesKeepEnd(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// NewMessageID generates a Message-ID (without angle brackets) under the domain
// of the SENDING identity (spec §5). It is unpredictable (crypto/rand) so it
// leaks nothing about volume or timing, and it never contains the server's
// hostname (CLAUDE.md: no operator infrastructure in anything that leaves the
// building).
func NewMessageID(fromAddress string) (string, error) {
	at := strings.LastIndex(fromAddress, "@")
	if at <= 0 || at == len(fromAddress)-1 {
		return "", fmt.Errorf("compose: cannot derive a Message-ID domain from %q", fromAddress)
	}
	domain := strings.ToLower(fromAddress[at+1:])

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("compose: generate message id: %w", err)
	}
	local := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))
	return local + "@" + domain, nil
}

// buildReferences assembles the References chain of a reply (RFC 5322 §3.6.4:
// the parent's References, then the parent's Message-ID), de-duplicated and
// capped at maxReferences. Capping drops from the MIDDLE — the root and the most
// recent ancestors are what threading actually uses.
func buildReferences(parentRefs []string, parentID string) []string {
	seen := make(map[string]bool, len(parentRefs)+1)
	out := make([]string, 0, len(parentRefs)+1)
	for _, r := range append(append([]string{}, parentRefs...), parentID) {
		r = strings.Trim(strings.TrimSpace(r), "<>")
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) <= maxReferences {
		return out
	}
	keepHead := 1
	keepTail := maxReferences - keepHead
	capped := make([]string, 0, maxReferences)
	capped = append(capped, out[:keepHead]...)
	capped = append(capped, out[len(out)-keepTail:]...)
	return capped
}

// envelopeTo is the SMTP RCPT TO set: every recipient, including Bcc.
func envelopeTo(d Draft) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range concatAddrs(d.To, d.Cc, d.Bcc) {
		e := strings.ToLower(strings.TrimSpace(a.Email))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

func concatAddrs(lists ...[]Address) []Address {
	var out []Address
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func addrList(addrs []Address) string {
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

func bracketList(ids []string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "<" + id + ">"
	}
	return strings.Join(parts, " ")
}

// encodeSubject RFC 2047-encodes a subject when it is not plain ASCII.
func encodeSubject(s string) string {
	return mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(s))
}

// writeHeader writes one header field, refusing to let a value smuggle in extra
// header lines (CRLF injection: a Subject of "x\r\nBcc: attacker@example.com"
// would otherwise become a real Bcc header).
func writeHeader(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(sanitizeHeaderValue(value))
	b.WriteString("\r\n")
}

// sanitizeHeaderValue strips CR/LF (and NUL) from a header value. Folding is not
// attempted: an over-long line is legal in practice and every MTA accepts it,
// whereas a mis-folded one is not.
func sanitizeHeaderValue(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1
		}
		return r
	}, v)
}

// normalizeEOL converts bare LF to CRLF (RFC 5322 line endings) without
// doubling an existing CRLF, and guarantees the body ends with one.
func normalizeEOL(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	if !strings.HasSuffix(s, "\r\n") {
		s += "\r\n"
	}
	return s
}

func writePart(b *strings.Builder, boundary, contentType, body string) {
	b.WriteString("--" + boundary + "\r\n")
	writeTextPart(b, contentType, body)
}

// newBoundary returns an unguessable MIME boundary.
func newBoundary() (string, error) {
	var buf [18]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("compose: generate boundary: %w", err)
	}
	return "mailroom-" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]), nil
}
