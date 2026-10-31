// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// SearchBackend is the engine seam (spec §7, ADR-006): "embedded Tantivy behind
// a SearchBackend interface so we can swap the engine later with no client
// changes". Everything above it — the query AST, the parser, the change-log
// indexer, highlighting — is engine-neutral, and every method here speaks only
// in Mailroom's own types. A backend instance indexes exactly ONE user.
//
// Implementations must make Upsert IDEMPOTENT (upsert by document id): the
// indexer delivers changes at-least-once and replays them freely after a crash,
// so re-applying the same document must leave the index in the same state, not
// duplicate it.
type SearchBackend interface {
	// Upsert indexes documents, replacing any document already stored under the
	// same id, and makes them searchable. It must be atomic per call to the
	// extent the engine allows: a partially applied batch is replayed anyway.
	Upsert(ctx context.Context, docs []Document) error

	// Delete removes documents by id. Deleting an id that is not present is a
	// no-op, not an error (the indexer replays destroys).
	Delete(ctx context.Context, ids []ulid.ULID) error

	// Search executes a Mailroom query and returns ranked hits, most relevant
	// first, bounded by q.Limit.
	Search(ctx context.Context, q Query) (Result, error)

	// NumDocs reports the live document count — a cache-health signal, never a
	// source of truth (Postgres is).
	NumDocs(ctx context.Context) (uint64, error)

	// Close flushes and releases the engine's resources (for a CGO backend,
	// native memory). After Close the backend must not be used.
	Close() error
}

// BackendFactory opens (creating if absent) the backend for one user's index
// directory. The directory is created 0700 by the Indexer before this is called;
// a factory must not widen it.
type BackendFactory func(ctx context.Context, dir string) (SearchBackend, error)

// ErrBackendUnavailable is returned by a factory that cannot provide an engine —
// most importantly by internal/search/tantivy when the binary was built without
// CGO or without the `tantivy` build tag. Callers degrade (search off, mail
// still served); they never fail closed into "no server".
var ErrBackendUnavailable = errors.New("search: no search backend is available in this build")

// Address is one participant on a message.
type Address struct {
	Name    string
	Address string
}

// Document is one indexed message: the projection of the canonical message row
// plus the parsed body/participants from its blob. It is the ONLY thing a
// backend is ever handed to index — deliberately a value type with no store,
// blob, or engine types in it.
//
// Body is already extracted text (text/plain plus HTML-stripped text/html) and
// size-capped by the caller (see MaxBodyBytes).
type Document struct {
	ID       ulid.ULID
	ThreadID ulid.ULID

	AccountIDs []ulid.ULID
	MailboxIDs []ulid.ULID
	LabelIDs   []ulid.ULID

	From Address
	To   []Address // To + Cc: the "other participants" set `to:` searches

	Subject string
	Body    string

	Flags    []string // the is:/has: atoms (FlagUnread, FlagStarred, …)
	Keywords []string // raw JMAP keywords that are not labels and not system flags

	ReceivedAt time.Time
	SentAt     time.Time

	HasAttachment bool
	Size          int64

	// Schema v3 (#722/#723).
	ListID      string       // the raw List-Id header value ("" when not list mail)
	Unsubscribe bool         // a List-Unsubscribe header is present
	DeliveredTo string       // the address it was delivered to (addressed_address)
	Attachments []Attachment // attachment parts: names and declared types
}

// The is:/has: atoms a document carries. These are Mailroom's vocabulary, not
// the engine's and not IMAP's: the parser maps user-facing aliases onto them
// (is:flagged → FlagStarred, is:spam → FlagJunk, …) so the index never has to
// know about synonyms.
const (
	FlagRead       = "read"
	FlagUnread     = "unread"
	FlagStarred    = "starred"
	FlagAnswered   = "answered"
	FlagDraft      = "draft"
	FlagJunk       = "junk"
	FlagTrashed    = "trashed"
	FlagAttachment = "attachment"

	// FlagUnsubscribe marks mail carrying a List-Unsubscribe header (#722).
	// Schema v3; the attachment-family atoms (attachtype.go) are too.
	FlagUnsubscribe = "unsubscribe"
)

// flagSchemaVersion is the index schema version that started writing a flag
// atom (see RequiredSchemaVersion).
func flagSchemaVersion(atom string) int {
	if atom == FlagUnsubscribe {
		return 3
	}
	if _, ok := familyAtom(atom); ok {
		return 3
	}
	return 1
}

// MaxBodyBytes caps how much extracted body text is indexed (and stored for
// highlighting) per message. A mail archive contains multi-megabyte text parts
// (mailing-list digests, quoted threads, base64 that decodes to text); indexing
// them whole buys almost no recall and costs index size linearly. 256 KiB of
// text is far past the point where a human is still reading.
const MaxBodyBytes = 256 << 10

// Values returns the normalized index-time values of field f for this document.
// It is the SINGLE source of truth for how a Document maps onto index fields, so
// every backend indexes identically and a query built for one backend means the
// same thing on another.
//
// Normalization matters: FieldFrom/FieldTo/FieldLabel/FieldAccount/FieldMailbox/
// FieldThread/FieldFlag/FieldKeyword/FieldDate/FieldID are EXACT (raw) fields —
// the engine does not analyze them, so the value stored here and the value a
// query looks for must be byte-identical. Addresses are therefore lowercased
// once, here, and Query terms for those fields are lowercased in exactly the
// same way (see normalizeExact).
func (d Document) Values(f Field) []string {
	switch f {
	case FieldID:
		return []string{d.ID.String()}
	case FieldThread:
		if d.ThreadID == (ulid.ULID{}) {
			return nil
		}
		return []string{d.ThreadID.String()}
	case FieldAccount:
		return ulidValues(d.AccountIDs)
	case FieldMailbox:
		return ulidValues(d.MailboxIDs)
	case FieldLabel:
		return ulidValues(d.LabelIDs)
	case FieldFrom:
		return nonEmpty(normalizeExact(d.From.Address))
	case FieldTo:
		out := make([]string, 0, len(d.To))
		for _, a := range d.To {
			if v := normalizeExact(a.Address); v != "" {
				out = append(out, v)
			}
		}
		return dedupe(out)
	case FieldFromDomain:
		return addressDomains(d.From.Address)
	case FieldToDomain:
		var out []string
		for _, a := range d.To {
			out = append(out, addressDomains(a.Address)...)
		}
		return dedupe(out)
	case FieldFlag:
		flags := append(append([]string{}, d.Flags...), attachmentFamilyAtoms(d.Attachments)...)
		if d.Unsubscribe {
			flags = append(flags, FlagUnsubscribe)
		}
		return dedupe(normalizeExactAll(flags))
	case FieldList:
		return listIDValues(d.ListID)
	case FieldSize:
		return sizeTokens(d.Size)
	case FieldFilename:
		var out []string
		for _, a := range d.Attachments {
			if n := strings.TrimSpace(a.Filename); n != "" {
				out = append(out, n)
			}
		}
		return out
	case FieldDeliveredTo:
		return nonEmpty(normalizeExact(d.DeliveredTo))
	case FieldDeliveredToDomain:
		return addressDomains(d.DeliveredTo)
	case FieldKeyword:
		// Keywords are case-sensitive JMAP tokens; they are NOT lowercased.
		return dedupe(d.Keywords)
	case FieldSubject:
		return nonEmpty(d.Subject)
	case FieldBody:
		return nonEmpty(clipUTF8(d.Body, MaxBodyBytes))
	case FieldPeople:
		return nonEmpty(d.peopleText())
	case FieldFuzzy:
		return nonEmpty(d.fuzzyText())
	case FieldDate:
		return dateBuckets(d.ReceivedAt)
	default:
		return nil
	}
}

// addressDomains is the FromDomain/ToDomain index view of one address (#712):
// its domain, then each parent-domain suffix that still has at least two labels.
//
//	alice@mail.shop.example → mail.shop.example, shop.example
//	alice@shop.example      → shop.example
//	alice@localhost         → localhost
//
// The bare TLD is never emitted: `from:com` would otherwise be "all mail", and
// the suffix would cost a posting list holding most of the index for nothing. A
// public-suffix list is deliberately NOT consulted, so `co.uk` is emitted for
// every `*.co.uk` sender — a user who types it gets what they asked for.
//
// An address with no '@' or an empty domain yields nothing (no phantom match).
func addressDomains(addr string) []string {
	a := normalizeExact(addr)
	i := strings.LastIndexByte(a, '@')
	if i < 0 {
		return nil
	}
	dom := strings.Trim(a[i+1:], ".")
	if dom == "" {
		return nil
	}
	out := []string{dom}
	for {
		dot := strings.IndexByte(dom, '.')
		if dot < 0 {
			return out
		}
		dom = dom[dot+1:]
		if !strings.Contains(dom, ".") {
			return out // what is left is the bare TLD
		}
		out = append(out, dom)
	}
}

// listIDValues is the FieldList index view of a List-Id header (RFC 2919): the
// list identifier (the part between angle brackets when there are any, the
// whole value otherwise) lower-cased, then each parent suffix that still has
// two labels, exactly as addressDomains does for a domain:
//
//	"Announcements" <announce.lists.example.org>
//	  -> announce.lists.example.org, lists.example.org, example.org
//
// so `list:announce.lists.example.org` finds that list and `list:example.org`
// every list hosted under example.org. Never the bare TLD.
func listIDValues(raw string) []string {
	id := normalizeListID(raw)
	if id == "" {
		return nil
	}
	if !strings.Contains(id, ".") {
		return []string{id}
	}
	return addressDomains("@" + id)
}

// normalizeListID extracts and normalizes a list identifier, for both the index
// (from the header) and the query (from what the user typed, which may be the
// header value, the bare id, or the list's posting ADDRESS: RFC 2919 ids are
// conventionally that address with '@' replaced by '.', so
// list:announce@lists.example.org means the same list).
func normalizeListID(raw string) string {
	v := strings.TrimSpace(raw)
	if i := strings.LastIndexByte(v, '<'); i >= 0 {
		if j := strings.IndexByte(v[i:], '>'); j > 0 {
			v = v[i+1 : i+j]
		}
	}
	v = strings.Trim(normalizeExact(v), "<>")
	v = strings.TrimPrefix(v, "*.")
	v = strings.ReplaceAll(v, "@", ".")
	return strings.Trim(v, ".")
}

// peopleText is the analyzed participant blob: display names AND addresses of
// everyone on the message, so free text ("alice", "example.com", "Acme
// Billing") finds mail from or to them without the user typing an operator.
func (d Document) peopleText() string {
	var b strings.Builder
	add := func(a Address) {
		if a.Name != "" {
			b.WriteString(a.Name)
			b.WriteByte(' ')
		}
		if a.Address != "" {
			b.WriteString(a.Address)
			b.WriteByte(' ')
		}
	}
	add(d.From)
	for _, a := range d.To {
		add(a)
	}
	return strings.TrimSpace(b.String())
}

// fuzzyText is the text typo tolerance is offered over: subject and
// participants — SHORT, high-value text.
//
// Why not the body: the binding exposes no FuzzyTermQuery (docs/search-spike.md
// §6, the one open ADR-006 follow-up), so typo tolerance is implemented as an
// n-gram field (see the tantivy backend). N-gramming a 25 GB body corpus would
// multiply the index for a feature nobody uses on body prose — mail search
// typos are in names and subjects. This is a deliberate, documented narrowing of
// spec §7's "typo tolerance", not an oversight.
func (d Document) fuzzyText() string {
	s := d.Subject
	if p := d.peopleText(); p != "" {
		s += " " + p
	}
	return strings.TrimSpace(s)
}

// Flags derives the is:/has: atoms from canonical message state. It is exported
// so the store adapter and tests agree on the vocabulary.
func Flags(seen, flagged, answered, draft, junk, trashed, hasAttachment bool) []string {
	var out []string
	if seen {
		out = append(out, FlagRead)
	} else {
		out = append(out, FlagUnread)
	}
	if flagged {
		out = append(out, FlagStarred)
	}
	if answered {
		out = append(out, FlagAnswered)
	}
	if draft {
		out = append(out, FlagDraft)
	}
	if junk {
		out = append(out, FlagJunk)
	}
	if trashed {
		out = append(out, FlagTrashed)
	}
	if hasAttachment {
		out = append(out, FlagAttachment)
	}
	return out
}

// Hit is one search result. Score is engine-relative and only meaningful for
// ordering WITHIN one result set; callers rely on ID (and ReceivedAt, which the
// index stores so a caller can order or window without a second database round
// trip).
type Hit struct {
	ID         ulid.ULID
	Score      float64
	ReceivedAt time.Time
	Snippets   []Snippet
}

// Result is a ranked page of hits. Total is the number of hits RETURNED (bounded
// by Query.Limit), not a corpus-wide count: an engine-side exact total over a
// large archive costs a full scan, and JMAP's `total` is computed by the caller
// against the canonical store anyway.
type Result struct {
	Hits  []Hit
	Total int
}

// Snippet is a highlighted fragment of one field of one hit. Ranges are
// [start,end) BYTE offsets into Text, in ascending order, and each covers a
// term that matched. Rendering (e.g. RFC 8621's <mark> markup) is Mark's job, so
// the engine's markup conventions never reach a client.
type Snippet struct {
	Field  Field
	Text   string
	Ranges [][2]int
}

// Mark renders the snippet as HTML with each matched range wrapped in <mark>,
// escaping everything else — the shape RFC 8621 SearchSnippet/get wants.
func (s Snippet) Mark() string {
	var b strings.Builder
	prev := 0
	for _, r := range s.Ranges {
		lo, hi := r[0], r[1]
		if lo < prev || lo > len(s.Text) || hi > len(s.Text) || hi <= lo {
			continue // defensive: never trust an engine's offsets blindly
		}
		b.WriteString(escapeHTML(s.Text[prev:lo]))
		b.WriteString("<mark>")
		b.WriteString(escapeHTML(s.Text[lo:hi]))
		b.WriteString("</mark>")
		prev = hi
	}
	b.WriteString(escapeHTML(s.Text[prev:]))
	return b.String()
}

// escapeHTML escapes the five characters that matter inside HTML text content.
func escapeHTML(s string) string {
	return htmlEscaper.Replace(s)
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;",
)

// normalizeExact is the canonical form of a value in a raw (unanalyzed) field:
// trimmed and lowercased. Addresses, flag atoms, and any other exact-match token
// pass through here at BOTH index time and query time — that symmetry is what
// makes `from:Alice@Example.COM` find mail from `alice@example.com`. (Ids are
// already canonical uppercase Crockford base32 and are matched verbatim; see
// isCaseSensitiveField.)
func normalizeExact(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizeExactAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if v := normalizeExact(s); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func ulidValues(ids []ulid.ULID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != (ulid.ULID{}) {
			out = append(out, id.String())
		}
	}
	return dedupe(out)
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// clipUTF8 truncates s to at most n bytes without splitting a rune.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

// utf8Start reports whether b is the first byte of a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
