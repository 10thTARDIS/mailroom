// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// The RFC 8621 §4.1.4 derivation, over REAL message bytes parsed by
// internal/mime (no doubles): the structure a client gets is the structure the
// message actually has.

// alternativeWithAttachment is the shape almost every real message takes:
//
//	multipart/mixed
//	├── multipart/alternative
//	│   ├── text/plain          (part 1.1)
//	│   └── text/html           (part 1.2)
//	└── application/pdf         (part 2, an attachment)
const alternativeWithAttachment = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>, carol@example.com\r\n" +
	"Cc: Dave <dave@example.com>\r\n" +
	"Reply-To: alice+replies@example.com\r\n" +
	"Subject: Quarterly report\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
	"\r\n" +
	"the plain version\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=\"utf-8\"\r\n" +
	"\r\n" +
	"<p>the html version</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--outer--\r\n"

func hashOf(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func TestBuildEmailBodyAlternativeWithAttachment(t *testing.T) {
	raw := []byte(alternativeWithAttachment)
	body := buildEmailBody(raw, hashOf(alternativeWithAttachment), bodyArgs{
		FetchTextBodyValues: true,
		FetchHTMLBodyValues: true,
	})

	// The two representations of the same content go to DIFFERENT lists — that
	// is the whole point of multipart/alternative, and a client that renders
	// HTML must not also print the plain-text twin.
	if len(body.textBody) != 1 || body.textBody[0]["type"] != "text/plain" {
		t.Fatalf("textBody = %#v, want exactly the text/plain part", body.textBody)
	}
	if len(body.htmlBody) != 1 || body.htmlBody[0]["type"] != "text/html" {
		t.Fatalf("htmlBody = %#v, want exactly the text/html part", body.htmlBody)
	}
	if len(body.attachment) != 1 || body.attachment[0]["name"] != "report.pdf" {
		t.Fatalf("attachments = %#v, want the pdf", body.attachment)
	}
	if got := body.attachment[0]["type"]; got != "application/pdf" {
		t.Errorf("attachment type = %v", got)
	}
	// size is the octet count AFTER content-transfer decoding — what a download
	// of that blob would weigh, not the base64 on the wire.
	if got := body.attachment[0]["size"]; got != len("%PDF-1.4\n") {
		t.Errorf("attachment size = %v, want %d (the decoded octets)", got, len("%PDF-1.4\n"))
	}

	// partIds are the MIME path, and bodyValues is keyed by them.
	if body.textBody[0]["partId"] != "1.1" || body.htmlBody[0]["partId"] != "1.2" {
		t.Errorf("partIds = %v / %v, want 1.1 / 1.2",
			body.textBody[0]["partId"], body.htmlBody[0]["partId"])
	}
	text, ok := body.values["1.1"].(map[string]any)
	if !ok {
		t.Fatalf("bodyValues = %#v, want a value for part 1.1", body.values)
	}
	if !strings.Contains(text["value"].(string), "the plain version") {
		t.Errorf("text body value = %q", text["value"])
	}
	if text["isTruncated"] != false || text["isEncodingProblem"] != false {
		t.Errorf("body value flags = %#v", text)
	}
	html, ok := body.values["1.2"].(map[string]any)
	if !ok || !strings.Contains(html["value"].(string), "<p>the html version</p>") {
		t.Fatalf("bodyValues[1.2] = %#v", body.values["1.2"])
	}
	// The attachment is not a text part: it has no body VALUE, only a blob.
	if _, ok := body.values["2"]; ok {
		t.Errorf("bodyValues includes the pdf: %#v", body.values)
	}
	if body.attachment[0]["blobId"] == nil {
		t.Errorf("the attachment has no blobId, so a client cannot fetch it")
	}

	// bodyStructure is the real tree, not a flattened list.
	if body.structure["type"] != "multipart/mixed" {
		t.Fatalf("bodyStructure type = %v", body.structure["type"])
	}
	if body.structure["partId"] != nil || body.structure["blobId"] != nil {
		t.Errorf("a multipart part must have a null partId/blobId (RFC 8621 §4.1.4): %#v", body.structure)
	}
	subs, _ := body.structure["subParts"].([]map[string]any)
	if len(subs) != 2 {
		t.Fatalf("bodyStructure subParts = %#v, want 2", body.structure["subParts"])
	}
	if subs[0]["type"] != "multipart/alternative" {
		t.Errorf("first subPart = %v", subs[0]["type"])
	}
	inner, _ := subs[0]["subParts"].([]map[string]any)
	if len(inner) != 2 || inner[0]["type"] != "text/plain" || inner[1]["type"] != "text/html" {
		t.Errorf("alternative subParts = %#v", subs[0]["subParts"])
	}

	// Reply-all is impossible without these.
	if len(body.to) != 2 || body.to[0]["email"] != "bob@example.com" || body.to[1]["email"] != "carol@example.com" {
		t.Errorf("to = %#v", body.to)
	}
	if len(body.cc) != 1 || body.cc[0]["email"] != "dave@example.com" {
		t.Errorf("cc = %#v", body.cc)
	}
	if len(body.replyTo) != 1 || body.replyTo[0]["email"] != "alice+replies@example.com" {
		t.Errorf("replyTo = %#v", body.replyTo)
	}
	if body.bcc != nil || body.sender != nil {
		t.Errorf("absent headers must be null, not empty: bcc=%#v sender=%#v", body.bcc, body.sender)
	}
}

// A plain single-part message is part "1" and still has a body.
func TestBuildEmailBodySinglePart(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: hi\r\n\r\nhello there\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{FetchAllBodyValues: true})

	if len(body.textBody) != 1 || body.textBody[0]["partId"] != "1" {
		t.Fatalf("textBody = %#v", body.textBody)
	}
	// Outside an alternative, an inline text part is BOTH representations: a
	// client rendering HTML shows it rather than showing nothing.
	if len(body.htmlBody) != 1 || body.htmlBody[0]["partId"] != "1" {
		t.Fatalf("htmlBody = %#v, want the same text part", body.htmlBody)
	}
	v, ok := body.values["1"].(map[string]any)
	if !ok || !strings.Contains(v["value"].(string), "hello there") {
		t.Fatalf("bodyValues = %#v", body.values)
	}
	if len(body.attachment) != 0 {
		t.Errorf("attachments = %#v, want none", body.attachment)
	}
}

// maxBodyValueBytes truncates, reports it, and never splits a rune (RFC 8621 §4.2).
func TestBuildEmailBodyTruncatesOnRuneBoundary(t *testing.T) {
	raw := "From: a@example.com\r\nContent-Type: text/plain; charset=utf-8\r\n\r\naé😀bc\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{
		FetchTextBodyValues: true,
		MaxBodyValueBytes:   4, // lands inside the 4-byte emoji
	})
	v := body.values["1"].(map[string]any)
	if v["isTruncated"] != true {
		t.Errorf("isTruncated = %v, want true", v["isTruncated"])
	}
	if got := v["value"].(string); got != "aé" {
		t.Errorf("value = %q, want %q (cut on a rune boundary)", got, "aé")
	}
}

// A text/plain part with a filename is an ATTACHMENT (notes.txt), not body text
// silently concatenated into the message.
func TestBuildEmailBodyNamedTextPartIsAnAttachment(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nthe body\r\n" +
		"--b\r\nContent-Type: text/plain; name=\"notes.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nnot the body\r\n" +
		"--b--\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{FetchTextBodyValues: true})

	if len(body.textBody) != 1 {
		t.Fatalf("textBody = %#v, want only the unnamed part", body.textBody)
	}
	if len(body.attachment) != 1 || body.attachment[0]["name"] != "notes.txt" {
		t.Fatalf("attachments = %#v, want notes.txt", body.attachment)
	}
	if v := body.values["1"].(map[string]any); !strings.Contains(v["value"].(string), "the body") {
		t.Errorf("body value = %#v", v)
	}
}

// relatedWithInlineImage is a multipart/related carrying a CID-referenced image
// (a signature logo), the shape every inline-image mail takes:
//
//	multipart/related
//	├── multipart/alternative
//	│   ├── text/plain          (part 1.1)
//	│   └── text/html           (part 1.2, references cid:logo@example.com)
//	└── image/png               (part 2, Content-ID: <logo@example.com>)
const relatedWithInlineImage = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: with a logo\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/related; boundary=\"rel\"\r\n" +
	"\r\n" +
	"--rel\r\n" +
	"Content-Type: multipart/alternative; boundary=\"alt\"\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
	"\r\n" +
	"the plain version\r\n" +
	"--alt\r\n" +
	"Content-Type: text/html; charset=\"utf-8\"\r\n" +
	"\r\n" +
	"<p>hi</p><img src=\"cid:logo@example.com\">\r\n" +
	"--alt--\r\n" +
	"--rel\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-ID: <logo@example.com>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--rel--\r\n"

// The client resolves a cid: image by matching the HTML's cid against the
// EmailBodyPart's own `cid`, then fetching its `blobId` (#99). Both come from the
// server's ONE part-id walk, so this proves the map the client relies on is
// actually exposed — with a blobId a download can resolve — and that the inline
// image is NOT mistaken for a remote-content attachment.
func TestBuildEmailBodyExposesCidForInlineImage(t *testing.T) {
	body := buildEmailBody([]byte(relatedWithInlineImage), hashOf(relatedWithInlineImage),
		bodyArgs{FetchHTMLBodyValues: true})

	// The inline image is a body part (CID-referenced), so it rides in htmlBody
	// alongside the text/html part rather than becoming an attachment.
	var img map[string]any
	for _, p := range body.htmlBody {
		if p["type"] == "image/png" {
			img = p
		}
	}
	if img == nil {
		t.Fatalf("htmlBody has no inline image part: %#v", body.htmlBody)
	}
	// cid is the Content-ID with angle brackets stripped (internal/mime) — exactly
	// what the HTML's `cid:logo@example.com` reference normalizes to.
	if img["cid"] != "logo@example.com" {
		t.Errorf("inline image cid = %v, want logo@example.com", img["cid"])
	}
	// blobId is <messageBlobId>.<partId> — the id a client hands the download
	// endpoint. Without it the cid could never be resolved to bytes.
	blobID, _ := img["blobId"].(string)
	if !strings.HasSuffix(blobID, ".2") {
		t.Errorf("inline image blobId = %q, want the message blob suffixed with the part path .2", blobID)
	}
	// The sanitized html body still carries the cid: src for the client to rewrite,
	// and is NOT marked as blocked remote content.
	v, ok := body.values["1.2"].(map[string]any)
	if !ok {
		t.Fatalf("no html body value: %#v", body.values)
	}
	htmlStr, _ := v["value"].(string)
	if !strings.Contains(htmlStr, `src="cid:logo@example.com"`) {
		t.Errorf("html body lost the cid: reference: %q", htmlStr)
	}
	if strings.Contains(htmlStr, blockedAttr) {
		t.Errorf("an inline image was falsely marked as remote content: %q", htmlStr)
	}
}

// format=flowed / delsp (RFC 3676) ride the Content-Type parameters and are
// exposed on the part so a client can un-flow the text before display (#890,
// web/src/core/flowed.ts).
func TestBuildEmailBodyExposesFlowedParameters(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8; format=flowed; delsp=yes\r\n\r\nsoft \r\nwrap\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain\r\n" +
		"--b--\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{FetchTextBodyValues: true})

	if len(body.textBody) != 2 {
		t.Fatalf("textBody = %#v, want both text parts", body.textBody)
	}
	// The flowed part reports both parameters.
	if flowed := body.textBody[0]; flowed["format"] != "flowed" || flowed["delSp"] != true {
		t.Errorf("flowed part = %#v, want format flowed and delSp true", flowed)
	}
	// A part without the parameters reports them as absent (null/false), not by
	// omitting the keys — a client reading the default set can rely on the
	// properties being present.
	if plain := body.textBody[1]; plain["format"] != nil || plain["delSp"] != false {
		t.Errorf("plain part = %#v, want format null and delSp false", plain)
	}
}

// bodyProperties selects which EmailBodyPart properties come back (RFC 8621 §4.2).
func TestBuildEmailBodyHonorsBodyProperties(t *testing.T) {
	body := buildEmailBody([]byte(alternativeWithAttachment), hashOf(alternativeWithAttachment),
		bodyArgs{Properties: []string{"partId", "type"}})
	part := body.textBody[0]
	if _, ok := part["size"]; ok {
		t.Errorf("size was not requested but came back: %#v", part)
	}
	if part["type"] != "text/plain" || part["partId"] != "1.1" {
		t.Errorf("requested properties missing: %#v", part)
	}
}
