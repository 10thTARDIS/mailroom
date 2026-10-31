// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"mailroom/internal/ical"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// The RFC 8621 §4.1.4 body properties of the JMAP Email object — bodyStructure,
// textBody, htmlBody, attachments, bodyValues — plus the full address headers
// (sender/to/cc/bcc/replyTo).
//
// ALL of these come from ONE place: the stored blob, which is the raw RFC 5322
// bytes and is the only authority on what a message actually says (ADR-001;
// app.messages holds a projection for list views, not the message). So they all
// share one cost — decrypt the blob, parse it once with internal/mime — and one
// gate: they are produced only when the client actually asks for a property that
// needs them (rawProperties below). An Email/get for a LIST view that names its
// properties (id, subject, from, receivedAt, keywords, ...) touches no blob at
// all; a reading-pane Email/get that asks for bodyValues pays for exactly the
// messages it asked about.

// rawProperties are the Email properties that can only be answered from the
// message bytes. Requesting ANY of them makes Email/get read and parse the
// message's blob; requesting NONE of them keeps it a pure Postgres read.
var rawProperties = map[string]bool{
	// RFC 8621 §4.1.2 address headers. Only `from` is projected into Postgres
	// (list views show a sender); the rest live in the bytes.
	"sender": true, "to": true, "cc": true, "bcc": true, "replyTo": true,
	// RFC 8621 §4.1.4 body properties.
	"bodyStructure": true, "textBody": true, "htmlBody": true,
	"attachments": true, "bodyValues": true,
	// authResults (urn:mailroom:senderauth, #55) — the Authentication-Results
	// header, which is a header like any other and therefore lives in the bytes.
	//
	// It is HERE rather than persisted onto app.messages, and that was a measured
	// choice (#55 increment 0). It costs no additional read: the reading pane is
	// the only surface that shows it, the reading pane already asks for body
	// properties, and loadBodies fetches and parses the blob exactly ONCE for all
	// of them. A column would have bought filtering and sorting, which nothing
	// asks for yet, in exchange for state that can drift from the bytes it
	// describes — and increment 0 exists partly to watch for exactly that drift.
	"authResults": true,
	// senderWarnings (urn:mailroom:senderauth, #397) is HERE for the auth half of
	// its judgement: whether DKIM or DMARC affirmatively passed, which only the
	// Authentication-Results header can say. The domain half comes from Postgres.
	// Listing it costs nothing extra in the surface that shows it — the reading
	// pane already asks for body properties, and loadBodies parses once for all
	// of them.
	"senderWarnings": true,
	// calendarInvite (urn:mailroom:calendar, #725) is the invitation a
	// text/calendar part or .ics attachment carries, parsed from the bytes
	// (internal/ical) so no client ever handles a raw .ics. The reading pane
	// already pays for the parse; the calendar search walks the structure
	// loadBodies built and parses only a part that is actually a calendar.
	"calendarInvite": true,
	// encrypted / decrypted / pgpSignature (urn:mailroom:pgp, #47 6b,
	// ADR-037): whether the message is RFC 3156 encrypted is a fact about the
	// bytes. Asking for these alone reads the blob but DECRYPTS NOTHING — only a
	// body property (or pgpSignature) triggers a decryption; see
	// decryptPropertyWanted.
	"encrypted": true, "decrypted": true, "pgpSignature": true,
}

// defaultEmailProperties is the property set Email/get returns when the client
// omits `properties` (RFC 8621 §4.2 replaces RFC 8620's "all" with this exact
// list — note bodyStructure is NOT in it, and bodyValues is present but empty
// unless a fetch*BodyValues argument asked for values).
//
// It is a BODY-READING set: to/cc/textBody/htmlBody/attachments all come from
// the bytes. A client that wants a cheap list MUST name its properties — which
// every real JMAP client does, and which web/src/core/sync.ts does.
var defaultEmailProperties = map[string]bool{
	"id": true, "blobId": true, "threadId": true, "mailboxIds": true,
	"keywords": true, "size": true, "receivedAt": true,
	"messageId": true, "inReplyTo": true, "references": true,
	"sender": true, "from": true, "to": true, "cc": true, "bcc": true,
	"replyTo": true, "subject": true, "sentAt": true, "hasAttachment": true,
	"preview": true, "bodyValues": true, "textBody": true, "htmlBody": true,
	"attachments": true,
	// ⚠ IN THE DEFAULT SET ON PURPOSE (#419). This set is a BODY-READING one, so
	// a client using it is exactly the client that needs to be told when a body
	// could not be read. Leaving it opt-in would mean the clients most likely to
	// hit the case are the ones least likely to ask about it — the silent
	// failure with an extra step.
	"bodyUnavailable": true,
	// Mailroom extensions (urn:mailroom:compose): the derived reply-from alias
	// and the identity it resolved to. Returned by default because a client
	// that does not name properties still needs them to reply correctly.
	"deliveredTo": true, "identityId": true,
}

// defaultBodyProperties is the EmailBodyPart property set returned when the
// client omits `bodyProperties` (RFC 8621 §4.2).
//
// format/delSp are the RFC 3676 flowed-text parameters: a client that wants to
// un-flow a soft-wrapped plain body needs both (web/src/core/flowed.ts). The
// web client omits bodyProperties, so this default set is what it gets.
var defaultBodyProperties = []string{
	"partId", "blobId", "size", "name", "type", "charset",
	"format", "delSp",
	"disposition", "cid", "language", "location",
}

// bodyArgs are the Email/get arguments that shape the body properties
// (RFC 8621 §4.2).
type bodyArgs struct {
	Properties          []string `json:"bodyProperties"`
	FetchTextBodyValues bool     `json:"fetchTextBodyValues"`
	FetchHTMLBodyValues bool     `json:"fetchHTMLBodyValues"`
	FetchAllBodyValues  bool     `json:"fetchAllBodyValues"`
	MaxBodyValueBytes   int      `json:"maxBodyValueBytes"`
	// proxyRewrite, when set, replaces a parked remote image URL with a
	// same-origin proxy reference. UNEXPORTED on purpose: bodyArgs is decoded
	// straight from client-supplied JSON, and a client must not be able to set
	// this.
	proxyRewrite proxyURLFn
	// trustedAuthservIDs are the RFC 8601 authserv-ids whose verdicts may be
	// believed for a SECURITY decision (#414). UNEXPORTED for the same reason as
	// proxyRewrite, and a sharper one: a client that could set this could name
	// the attacker's own id and re-open the hole it exists to close.
	trustedAuthservIDs []string
	// received is the message's received_at (INTERNALDATE), the time a
	// date-bounded authserv-id entry is judged at (#766, ml.TrustedAt). UNEXPORTED
	// and set by loadBodies from the stored row, never from the request.
	received time.Time
	// authSelf is whether the message is the user's own rather than inbound
	// (live in a Sent or Drafts mailbox; never the From address), decided
	// by loadBodies from authSelfFacts (#766). It matters only to the
	// "unauthenticated" notice; see authNoticeObject.
	authSelf bool
	// localDKIM is Mailroom's own DKIM verdict for this message (#776,
	// store.LocalDKIM), nil when none was recorded. UNEXPORTED and set by
	// loadBodies from the stored row, never from the request.
	localDKIM *store.LocalDKIM
	// blockedParts is the set of part paths the quarantine refuses for THIS
	// message (#413). UNEXPORTED for the same reason as proxyRewrite, and more
	// urgently: a client that could set it could clear it.
	blockedParts map[string]bool
	// wantCalendar asks buildEmailBody to look for an invitation (#725). Set by
	// loadBodies from the requested properties; UNEXPORTED like the rest.
	wantCalendar bool
	// partPrefix is prepended to every partId (and therefore blobId) this body
	// mints: decryptedPartPrefix for a decrypted entity, "" otherwise (#47,
	// ADR-037). UNEXPORTED: a client choosing it could make a ciphertext part
	// look like a plaintext one to the download route.
	partPrefix string
}

// gatesBodyValues reports whether these args can reach a part the quarantine
// would refuse, and therefore whether the scan states must be loaded.
//
// ⚠ ONLY fetchAllBodyValues CAN. fetchTextBodyValues and fetchHTMLBodyValues
// walk the BODY lists, and a text part is body content precisely when it is not
// dispositioned as an attachment — which is the same condition
// mime.IsAttachmentLeaf uses to decide what gets projected and scanned. So those
// two reach parts that have no row and could not be blocked. fetchAllBodyValues
// walks the whole structure and does reach dispositioned attachments.
//
// Stated rather than assumed because if that alignment ever breaks, this is the
// line that silently stops covering the gap.
func (b bodyArgs) gatesBodyValues() bool { return b.FetchAllBodyValues }

// emailBody is one message's parsed bytes, rendered as the JMAP body and
// address properties. Built once per message per Email/get.
type emailBody struct {
	structure  map[string]any
	textBody   []map[string]any
	htmlBody   []map[string]any
	attachment []map[string]any
	values     map[string]any

	sender  []map[string]any
	to      []map[string]any
	cc      []map[string]any
	bcc     []map[string]any
	replyTo []map[string]any

	// unreadable marks a message whose stored bytes could not be read at all —
	// a corrupt or tampered blob (#419). Every other field is then zero, and
	// emailObject turns this into the `bodyUnavailable` property.
	//
	// ⚠ IT IS NOT THE SAME AS A nil *emailBody. Nil means "the client did not
	// ask for body properties", so they are absent; this means "the client
	// asked and there is nothing to give". Collapsing the two is exactly how a
	// message would lose its body silently.
	unreadable bool

	// authTrusted is the Authentication-Results headers parsed under RFC 8601
	// §5: only those bearing an authserv-id this deployment names
	// (MAILROOM_AUTHSERV_IDS) survive. It is the ONLY auth input a SECURITY
	// decision may read (#414), and as of #625 that includes the `authResults`
	// Email property shown to the reader, not just the self-spoof warning: a
	// header is attacker-writable unless its authserv-id is checked, so a
	// display fed from the permissive parse is exactly as forgeable as a
	// suppression check fed from it. With MAILROOM_AUTHSERV_IDS unset,
	// TrustedAuthResults keeps nothing, so both the notice and the warning go
	// silent rather than trust a header the sender could have written — silent
	// by design, not a bug to chase.
	//
	// The permissive parse (ml.AuthResultsOf over every header, unfiltered by
	// authserv-id) still exists and is still correct for what it was built
	// for: internal/ingest's cold-start `auth_fail` classifier feature reads
	// it directly from the header bytes via ml.AuthResultsFail, independently
	// of this struct — a forged `pass` only makes that CONSERVATIVE failure
	// signal fire less often, which is noise for a feature and not an exploit
	// (see internal/ml/authresults.go). Nothing in internal/api needs that
	// parse any more, so there is no field for it here.
	authTrusted ml.AuthResults

	// authConfigured is whether the deployment names ANY authserv-id. It is
	// what makes "no trusted ids" SILENT rather than a notice: authTrusted is
	// then empty for every message, and rendering that as `present: false`
	// would make the web client show "unauthenticated" on all remote mail
	// (web/src/core/senderAuth.ts, `!auth.present`). A notice on everything
	// teaches the reader to ignore it, so emailObject sends no authResults at
	// all until an operator names their provider's id (#625).
	authConfigured bool

	// authSelf mirrors bodyArgs.authSelf for emailObject.
	authSelf bool

	// localDKIM is Mailroom's own DKIM verdict (#776, ADR-036), nil when none
	// was recorded. It is read ONLY through LocalDKIMPass and ONLY when
	// authTrusted.Present is false: a provider's verdict always wins.
	localDKIM *store.LocalDKIM

	// fromFields is how many From header FIELDS the message carries, counted
	// raw (mime.Header.FieldCount) — what senderWarnings' multipleFrom reads
	// (#754). Zero for an unreadable message, which raises nothing.
	fromFields int

	// calendar is the invitation the message carries (#725), when the client
	// asked for calendarInvite. Zero — Present false — otherwise.
	calendar ical.Found
	// calendarVerified is whether that calendar is authenticated as coming
	// from the person it speaks for (ical.Verified, the trusted
	// Authentication-Results parse). Only then may it change another
	// message's card.
	calendarVerified bool

	// encrypted is whether the stored message is RFC 3156 multipart/encrypted
	// (#47 6b). decrypted is nil when no decryption was attempted (not
	// encrypted, or no body property asked for), true when the body above was
	// built from the plaintext, and false when it could not be — the honest
	// "could not be decrypted" state, with no detail.
	encrypted bool
	decrypted *bool
	// pgpSignature is the verdict on a PGP/MIME signature, on the message itself
	// or inside its decrypted entity (pgp_signature.go), or nil when unsigned.
	pgpSignature *pgpSignature
}

// bodyNode is one MIME part with the JMAP identity assigned to it: its partId
// (hierarchical, IMAP-style: "1", "2", "2.1") and the blobId that names its
// decoded octets.
type bodyNode struct {
	st     *mime.Structure
	partID string
	blobID string
}

// buildEmailBody parses a message's raw RFC 5322 bytes and renders every
// property that can only come from them. contentHash is the message's blob id
// (the JMAP blobId of the whole message), which part blob ids extend.
//
// It never fails: internal/mime is total on hostile input (docs/mime-policy.md),
// so a corrupt or truncated message yields a degraded — but structurally valid —
// Email object rather than a failed method call for the whole batch.
func buildEmailBody(raw []byte, contentHash []byte, args bodyArgs) *emailBody {
	parsed, _ := mime.ParseBytes(raw)
	if parsed == nil {
		return &emailBody{values: map[string]any{}}
	}

	blobPrefix := hex.EncodeToString(contentHash)
	nodes := assignPartIDs(parsed.Structure, blobPrefix, args.partPrefix)

	var text, html, attach []*bodyNode
	if parsed.Structure != nil {
		// The root is walked as a one-element part list with no enclosing
		// multipart, exactly as RFC 8621's algorithm bootstraps it.
		parseStructure([]*mime.Structure{parsed.Structure}, nodes, "", false, &text, &html, &attach)
	}

	props := args.Properties
	if props == nil {
		props = defaultBodyProperties
	}
	want := make(map[string]bool, len(props))
	for _, p := range props {
		want[p] = true
	}

	// ⚠ THE ENTRIES ARE RESOLVED AT THIS MESSAGE'S RECEIVED TIME, once, here
	// (#766), and everything below reads the resolved ids. A date-bounded entry
	// is trusted only for mail received before its bound.
	trustedNow := ml.TrustedAt(args.trustedAuthservIDs, args.received)

	b := &emailBody{
		structure:  bodyPartObject(nodes[parsed.Structure], nodes, want, true),
		textBody:   bodyPartList(text, nodes, want),
		htmlBody:   bodyPartList(html, nodes, want),
		attachment: bodyPartList(attach, nodes, want),
		values:     bodyValues(parsed.Structure, nodes, text, html, args),
		sender:     addressList(parsed.Header.Sender()),
		to:         addressList(parsed.Header.To()),
		cc:         addressList(parsed.Header.Cc()),
		bcc:        addressList(parsed.Header.Bcc()),
		replyTo:    addressList(parsed.Header.ReplyTo()),
		// ⚠ THE SECURITY DECISION — AND, AS OF #625, THE DISPLAY — READS ONLY
		// THE TRUSTED PARSE. Both the `authResults` Email property (what the
		// reader is shown) and the self-spoof warning (#414) are built from
		// this: a verdict counts only when its authserv-id is one the operator
		// named in MAILROOM_AUTHSERV_IDS. Reading the permissive parse for
		// either one lets a sender cancel a provider's genuine `dmarc=fail`
		// with a forged `Authentication-Results: evil.example; dmarc=pass`
		// header of their own — RFC 8601 §5 only requires a relay to strip
		// headers bearing ITS OWN id, so a header naming any other id passes
		// through untouched. With MAILROOM_AUTHSERV_IDS unset this is empty,
		// so the notice is silent by design rather than trust a forgeable
		// header (#625) — see the doc on the field below.
		authTrusted:    ml.TrustedAuthResults(parsed.Header.Values("Authentication-Results"), trustedNow),
		authConfigured: len(args.trustedAuthservIDs) > 0,
		authSelf:       args.authSelf,
		localDKIM:      args.localDKIM,
		fromFields:     parsed.Header.FieldCount("From"),
	}
	if args.wantCalendar {
		b.calendar = ical.FindInMessage(parsed.Structure)
		b.calendarVerified = ical.Verified(b.calendar.Invite, parsed, trustedNow)
	}
	return b
}

// assignPartIDs walks the MIME tree and gives every node its partId and blobId.
//
// A multipart container has NEITHER (RFC 8621: "partId is null if, and only if,
// the part is of type multipart/*", same for blobId) — it names no octets a
// client could fetch. Leaf ids are the IMAP-style path of the part within the
// message ("1", "2", "2.1"), which makes them stable for a given message and
// meaningful to a human reading a log.
// ⚠ THE PATHS COME FROM internal/mime, NOT FROM A WALK HERE (#393). They used
// to be derived by a private copy of this algorithm. app.attachments.part_path
// now has to name the same part as partId does — a scan verdict recorded
// against a path no blobId resolves to would be attached to nothing — and
// internal/ingest cannot import this package. One implementation, below both.
//
// partPrefix is prepended to every partId, and so to every part blobId:
// decryptedPartPrefix for the parts of a decrypted entity (#47, ADR-037), so
// that they can never be confused with the stored ciphertext's parts.
func assignPartIDs(root *mime.Structure, blobPrefix, partPrefix string) map[*mime.Structure]*bodyNode {
	out := map[*mime.Structure]*bodyNode{}
	paths := mime.PartPaths(root)
	var walk func(n *mime.Structure)
	walk = func(n *mime.Structure) {
		if n == nil {
			return
		}
		node := &bodyNode{st: n}
		// PartPaths returns "" for a multipart container, which is exactly
		// RFC 8621's rule: partId is null iff the part is multipart/*.
		if p := paths[n]; p != "" {
			node.partID = partPrefix + p
			// The part blob id extends the message's blob id, so it is
			// self-describing and needs no server-side table to resolve.
			node.blobID = blobPrefix + "." + node.partID
		}
		out[n] = node
		for _, sub := range n.SubParts {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// isMultipart delegates so there is one definition of "container" (#393).
func isMultipart(contentType string) bool {
	return mime.IsMultipart(contentType)
}

// parseStructure is RFC 8621 §4.1.4's algorithm for deriving textBody, htmlBody
// and attachments from the MIME tree.
//
// Two readings the RFC leaves to the server, both chosen for what a mail client
// actually needs (and both stated plainly because they are judgment calls):
//
//   - a text/plain or text/html part with a FILENAME is an attachment, not body
//     text. The RFC's own note is that "if it has a filename, it's probably an
//     attachment"; a mailer that attaches notes.txt must not have it silently
//     concatenated into the message body.
//   - an image/audio/video part is body content only when it is CID-REFERENCED
//     (it has a Content-ID and is not marked as an attachment) — that is the
//     only way body HTML can display it. An unreferenced image is an attachment,
//     which is what the user calls it.
//
// Everything else follows the RFC: multipart/alternative sends text/plain to
// textBody and text/html to htmlBody (the two representations of the SAME
// content), while any other multipart puts an inline part in BOTH lists — so a
// client rendering HTML still shows the plain-text part of a mixed message
// rather than nothing.
func parseStructure(parts []*mime.Structure, all map[*mime.Structure]*bodyNode,
	multipartType string, inAlternative bool, text, html, attach *[]*bodyNode) {
	for _, st := range parts {
		if st == nil {
			continue
		}
		if isMultipart(st.ContentType) {
			sub := strings.TrimPrefix(st.ContentType, "multipart/")
			parseStructure(st.SubParts, all, sub, inAlternative || sub == "alternative",
				text, html, attach)
			continue
		}
		n := all[st]
		if n == nil {
			continue
		}
		if !isBodyPart(st) {
			*attach = append(*attach, n)
			continue
		}
		switch {
		case multipartType == "alternative":
			// The two representations of the same content: each goes to its own
			// list, and an inline image sitting directly in an alternative is
			// neither representation, so it is an attachment.
			switch st.ContentType {
			case "text/plain":
				*text = append(*text, n)
			case "text/html":
				*html = append(*html, n)
			default:
				*attach = append(*attach, n)
			}
		case inAlternative && st.ContentType == "text/plain":
			// A text part nested deeper inside an alternative (e.g. the HTML
			// half of an alternative wrapped in a multipart/related): it is
			// still only ONE of the two representations, so it must not be
			// pushed into both lists.
			*text = append(*text, n)
		case inAlternative && st.ContentType == "text/html":
			*html = append(*html, n)
		default:
			*text = append(*text, n)
			*html = append(*html, n)
		}
	}
}

// isBodyPart reports whether a leaf part is body content rather than an
// attachment. See parseStructure for the two judgment calls encoded here.
func isBodyPart(st *mime.Structure) bool {
	if st.Disposition == "attachment" {
		return false
	}
	switch {
	case st.ContentType == "text/plain" || st.ContentType == "text/html":
		return st.Filename == ""
	case strings.HasPrefix(st.ContentType, "image/"),
		strings.HasPrefix(st.ContentType, "audio/"),
		strings.HasPrefix(st.ContentType, "video/"):
		return st.ContentID != ""
	default:
		return false
	}
}

// bodyPartList renders a list of parts as JMAP EmailBodyPart objects.
func bodyPartList(nodes []*bodyNode, all map[*mime.Structure]*bodyNode, want map[string]bool) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if obj := bodyPartObject(n, all, want, false); obj != nil {
			out = append(out, obj)
		}
	}
	return out
}

// bodyPartObject renders one EmailBodyPart (RFC 8621 §4.1.4), honoring the
// requested bodyProperties.
//
// subParts is emitted for a multipart node whenever the caller is rendering
// bodyStructure, whether or not it was named in bodyProperties: a bodyStructure
// with its structure removed is not a body structure, and no client asks for one.
func bodyPartObject(n *bodyNode, all map[*mime.Structure]*bodyNode, want map[string]bool, structure bool) map[string]any {
	if n == nil || n.st == nil {
		return nil
	}
	st := n.st
	full := map[string]any{
		"partId":  nilIfEmpty(n.partID),
		"blobId":  nilIfEmpty(n.blobID),
		"size":    st.OctetSize,
		"name":    nilIfEmpty(st.Filename),
		"type":    st.ContentType,
		"charset": nilIfEmpty(st.Params["charset"]),
		// The RFC 3676 flowed-text parameters, for a client to un-flow before
		// display (#890, web/src/core/flowed.ts). An absent parameter reports
		// null/false, not an omitted key — same honesty as language/location.
		// Params keys are lower-case (mime.ParseMediaType); delsp is true only
		// for "yes".
		"format":      nilIfEmpty(strings.ToLower(st.Params["format"])),
		"delSp":       strings.EqualFold(st.Params["delsp"], "yes"),
		"disposition": nilIfEmpty(st.Disposition),
		"cid":         nilIfEmpty(st.ContentID),
		// Language/location are RFC 8621 properties Mailroom does not track
		// (Content-Language / Content-Location are vanishingly rare and nothing
		// reads them). They are reported honestly as absent rather than omitted,
		// so a client that asked for them gets an answer.
		"language": nil,
		"location": nil,
	}
	if isMultipart(st.ContentType) {
		full["size"] = 0
	}

	out := map[string]any{}
	for k, v := range full {
		if want[k] {
			out[k] = v
		}
	}
	if structure {
		if isMultipart(st.ContentType) {
			subs := make([]map[string]any, 0, len(st.SubParts))
			for _, s := range st.SubParts {
				if obj := bodyPartObject(all[s], all, want, true); obj != nil {
					subs = append(subs, obj)
				}
			}
			out["subParts"] = subs
		} else if want["subParts"] {
			out["subParts"] = nil
		}
	}
	return out
}

// bodyValues renders the EmailBodyValue map (RFC 8621 §4.1.4): the decoded text
// of the parts the client asked for, truncated to maxBodyValueBytes.
//
// Only text/* parts ever have a value — an image in textBody is displayed from
// its blob, not from a string — so fetchAllBodyValues means "every text part in
// the structure", not "every part".
//
// THIS IS THE RENDER PATH, and it is where hostile HTML is SANITIZED (spec §11,
// #39, docs/hostile-content.md). Every text/html body value leaves this server
// having been parsed, filtered against an allowlist, and re-serialized
// (SanitizeHTML) — for EVERY client, not just ours: a JMAP client that is not a
// browser has no iframe sandbox to hide behind.
//
// It is sanitized BEFORE truncation, so maxBodyValueBytes counts the bytes the
// client will actually render, and a truncation can only ever cut a safe tag in
// half rather than cut a dangerous one back into existence.
//
// The RAW bytes remain reachable — /jmap/download of the part's blobId — because
// "view source" is a real feature and the raw message is the source of truth.
// That endpoint never renders: it serves the part as application/octet-stream
// with Content-Disposition: attachment and nosniff (blob.go). Rendering is
// sanitized; downloading is inert. Those are the only two doors.
func bodyValues(root *mime.Structure, all map[*mime.Structure]*bodyNode, text, html []*bodyNode, args bodyArgs) map[string]any {
	out := map[string]any{}
	add := func(n *bodyNode) {
		if n == nil || n.st == nil || n.partID == "" {
			return
		}
		if !strings.HasPrefix(n.st.ContentType, "text/") {
			return
		}
		// ⚠ THE QUARANTINE REACHES THIS PATH TOO (#413, ADR-027 §8). A flagged
		// `text/plain` ATTACHMENT is projected, scanned and refused by the
		// download route — and fetchAllBodyValues was handing its content back
		// verbatim as a JSON string. Same bytes, different door.
		//
		// The value is OMITTED rather than replaced with a placeholder: RFC 8621
		// gives EmailBodyValue no field for "withheld", and inventing one here
		// would be a claim no client reads. The reason is available where it
		// belongs, on the attachmentScan property, which names this exact partId
		// in its `quarantined` list.
		if args.blockedParts[n.partID] {
			return
		}
		if _, done := out[n.partID]; done {
			return
		}
		data := string(n.st.Data)
		if n.st.ContentType == "text/html" {
			// ⚠ ONE CALL, TWO BEHAVIOURS, NEVER A THIRD. With no rewriter this is
			// byte-identical to SanitizeHTML; with one, the parked value is a
			// proxy path and the sender's URL never leaves the server. There is
			// deliberately no path that emits BOTH.
			data = sanitizeHTMLWithProxy(data, args.proxyRewrite)
		}
		value, truncated := truncateUTF8(data, args.MaxBodyValueBytes)
		out[n.partID] = map[string]any{
			"value":             value,
			"isEncodingProblem": n.st.EncodingProblem,
			"isTruncated":       truncated,
		}
	}

	if args.FetchTextBodyValues {
		for _, n := range text {
			add(n)
		}
	}
	if args.FetchHTMLBodyValues {
		for _, n := range html {
			add(n)
		}
	}
	if args.FetchAllBodyValues {
		var walk func(st *mime.Structure)
		walk = func(st *mime.Structure) {
			if st == nil {
				return
			}
			if !isMultipart(st.ContentType) {
				add(all[st])
			}
			for _, s := range st.SubParts {
				walk(s)
			}
		}
		walk(root)
	}
	return out
}

// truncateUTF8 cuts s to at most max OCTETS without splitting a rune (RFC 8621:
// "the value MUST NOT be truncated in the middle of a UTF-8 character"). A
// max of 0 (the default) means no limit.
func truncateUTF8(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// addressList renders an RFC 5322 address list as JMAP EmailAddress objects
// (RFC 8621 §4.1.2). An absent header is null, not an empty array — the two
// mean different things to a client composing a reply.
func addressList(in []mime.Address) []map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(in))
	for _, a := range in {
		out = append(out, map[string]any{"name": nilIfEmpty(a.Name), "email": a.Address})
	}
	return out
}
