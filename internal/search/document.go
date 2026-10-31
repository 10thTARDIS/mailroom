// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"strings"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mime"
)

// Message is the canonical projection the indexer builds a Document from: the
// app.messages row plus its live mailbox/account membership and label ids. It is
// the store's half of the indexing contract, kept as a plain value so the whole
// indexer is testable with a fake Source and no database.
//
// Everything here comes from Postgres — the system of record. The body and the
// recipient list do NOT: they live only in the encrypted blob (the schema keeps
// no To/Cc columns and no body text), so the indexer decrypts and parses the
// message to get them (see Content).
type Message struct {
	ID          ulid.ULID
	ThreadID    ulid.ULID
	ContentHash []byte

	Subject     string
	FromName    string
	FromAddress string
	Snippet     string

	ReceivedAt time.Time
	SentAt     time.Time
	Size       int64

	HasAttachment bool
	Trashed       bool
	Seen          bool
	Flagged       bool
	Answered      bool
	Draft         bool
	Junk          bool

	Keywords   []string
	LabelIDs   []ulid.ULID
	MailboxIDs []ulid.ULID
	AccountIDs []ulid.ULID

	// Schema v3 (#722/#723), all from Postgres: the List-Id and
	// List-Unsubscribe header values ingest projected onto the row, the
	// address the message was delivered to (addressed_address), and the
	// app.attachments rows — the same part list the client shows, decided by
	// the same mime.AttachmentParts judgement as HasAttachment.
	ListID           string
	ListUnsubscribe  string
	AddressedAddress string
	Attachments      []Attachment
}

// buildDocument projects a canonical message (plus, when available, its raw
// RFC 5322 bytes) into an index Document.
//
// raw may be nil: a blob that is missing, unreadable, or fails its AEAD check is
// NOT a reason to drop the message from the index. The canonical row alone still
// makes the message findable by subject, sender, label, state, and date — a
// degraded but honest index entry — and it will be upgraded the next time the
// message is reindexed. Failing the whole batch instead would let one corrupt
// blob wedge a user's entire search cursor.
func buildDocument(m Message, raw []byte) Document {
	d := Document{
		ID:            m.ID,
		ThreadID:      m.ThreadID,
		AccountIDs:    m.AccountIDs,
		MailboxIDs:    m.MailboxIDs,
		LabelIDs:      m.LabelIDs,
		From:          Address{Name: m.FromName, Address: m.FromAddress},
		Subject:       m.Subject,
		Body:          m.Snippet, // fallback when the blob is unavailable
		Keywords:      m.Keywords,
		ReceivedAt:    m.ReceivedAt,
		SentAt:        m.SentAt,
		HasAttachment: m.HasAttachment,
		Size:          m.Size,
		ListID:        m.ListID,
		Unsubscribe:   strings.TrimSpace(m.ListUnsubscribe) != "",
		DeliveredTo:   m.AddressedAddress,
		Attachments:   m.Attachments,
	}
	d.Flags = Flags(m.Seen, m.Flagged, m.Answered, m.Draft, m.Junk, m.Trashed, m.HasAttachment)

	if len(raw) == 0 {
		return d
	}
	parsed, err := mime.ParseBytes(raw)
	if err != nil || parsed == nil {
		return d
	}

	// Recipients: To ∪ Cc. Bcc is deliberately NOT indexed — on received mail it
	// is a header the sender leaked by accident, and on sent mail surfacing it in
	// search results is a way to disclose a blind copy.
	for _, a := range append(parsed.Header.To(), parsed.Header.Cc()...) {
		if a.Address == "" && a.Name == "" {
			continue
		}
		d.To = append(d.To, Address{Name: a.Name, Address: a.Address})
	}
	// A From on the parsed message beats the projection only when the projection
	// is empty (the row is canonical; this just fills gaps).
	if d.From.Address == "" {
		if from := parsed.Header.From(); len(from) > 0 {
			d.From = Address{Name: from[0].Name, Address: from[0].Address}
		}
	}
	if d.Subject == "" {
		d.Subject = parsed.Header.Subject()
	}

	body := bodyText(parsed)
	if body != "" {
		d.Body = clipUTF8(body, MaxBodyBytes)
	}
	return d
}

// bodyText is the indexable text of a message: every text/plain part, plus every
// text/html part with its markup stripped (spec §7: "body (text + HTML-stripped)").
// Nested message/rfc822 parts contribute their text too — a forwarded message is
// exactly the thing people search for.
//
// Attachment text is NOT extracted here: spec §7 makes it opt-in and size-capped,
// and matrix §5 targets it at v1.x. app.attachments already carries the
// extracted_text_hash column it will use.
func bodyText(m *mime.Message) string {
	var b strings.Builder
	var walk func(msg *mime.Message, depth int)
	walk = func(msg *mime.Message, depth int) {
		if msg == nil || depth > 4 || b.Len() > MaxBodyBytes {
			return
		}
		if t := strings.TrimSpace(msg.Text); t != "" {
			b.WriteString(t)
			b.WriteByte('\n')
		}
		if h := strings.TrimSpace(msg.HTML); h != "" {
			b.WriteString(stripHTML(h))
			b.WriteByte('\n')
		}
		for _, n := range msg.Nested {
			walk(n, depth+1)
		}
	}
	walk(m, 0)
	return strings.TrimSpace(b.String())
}

// stripHTML reduces an HTML body to its text: it drops tags, drops the content
// of <script>/<style>, and decodes the handful of entities that actually occur in
// mail. It is deliberately a lexer, not a parser — an HTML parse tree buys
// nothing for search, and mail HTML is not well-formed anyway.
func stripHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s) / 2)

	inTag := false
	skipTo := "" // when set, we are inside <script>/<style> and skipping to </script>/</style>
	tagStart := 0

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case skipTo != "":
			if c == '<' && strings.HasPrefix(strings.ToLower(s[i:min(i+len(skipTo), len(s))]), skipTo) {
				skipTo = ""
				inTag = true
				tagStart = i
			}
		case c == '<':
			inTag = true
			tagStart = i
		case c == '>' && inTag:
			inTag = false
			tag := strings.ToLower(s[tagStart+1 : i])
			switch {
			case strings.HasPrefix(tag, "script"):
				skipTo = "</script"
			case strings.HasPrefix(tag, "style"):
				skipTo = "</style"
			}
			// Any tag is a token boundary: <b>in</b>voice must not become
			// "invoice", and <br> must not glue two lines together.
			b.WriteByte(' ')
		case !inTag:
			b.WriteByte(c)
		}
	}

	return collapseSpace(htmlUnescaper.Replace(b.String()))
}

// htmlUnescaper decodes the entities that actually appear in mail bodies.
// Numeric entities are left alone: they are rare, and a stray "&#8212;" costs a
// search nothing.
var htmlUnescaper = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
	"&quot;", `"`, "&#39;", "'", "&apos;", "'",
)

// collapseSpace folds every run of whitespace into a single space.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true // leading whitespace is dropped
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(b.String())
}
