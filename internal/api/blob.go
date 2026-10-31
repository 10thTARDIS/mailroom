// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/clamd"
	"mailroom/internal/compose"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// The blob endpoints (RFC 8620 §6): downloadUrl and uploadUrl. The session has
// advertised both since 0.5; until #85 neither was routed, so every attachment
// link was a 404 and compose could not attach anything.
//
// ── THE SECURITY MODEL ───────────────────────────────────────────────────────
//
// An attachment is attacker-controlled bytes that arrived from a stranger, and
// it is served from the operator's OWN origin, next to their mail. Two things
// therefore have to hold, and both are enforced here rather than assumed:
//
// 1. AUTHORIZATION — a blob is served only to the user who owns it.
//
//   - The caller's identity comes from the auth middleware context. The
//     {accountId} path segment is CHECKED against it and a mismatch is a 404;
//     it is never used to select whose data to read (spec §10).
//   - Ownership is resolved in POSTGRES, inside a UserTx, so RLS scopes the
//     lookup to the caller: `app.messages WHERE content_hash = $1` under
//     `mailroom.user_id`. A blobId is a content hash, and the blob store is
//     content-addressed — so a route that trusted the hash alone would hand any
//     stranger who learned (or guessed) a hash somebody else's attachment.
//     There is no code path here that reads bytes without that check first.
//   - An UPLOADED blob that is not yet attached to any message has no message row
//     to check, so it is authorized against `app.blobs` (store.BlobExists) in the
//     SAME UserTx — also Postgres, also under RLS. The blob store's own per-user
//     namespace would answer correctly too (the path is HMAC(serverKey, userID)
//     and the ciphertext is sealed under that user's DEK with the user id as AEAD
//     associated data, so a cross-user read could not decrypt even if it found the
//     file) — but ownership is a fact about the user's ROWS, and one authority for
//     both cases beats two that must be kept in agreement.
//
// 2. CONTENT SAFETY — the bytes must not be able to run as code on our origin.
//
//   - Content-Disposition: attachment, always, with an RFC 6266/5987-encoded
//     filename. Nothing is ever served for inline navigation.
//   - The Content-Type is CHOSEN BY US from a small allowlist (safeContentType),
//     never echoed from the part's declared type and never from the `type` query
//     parameter. text/html and image/svg+xml — the two that turn a phishing mail
//     into stored XSS on the operator's own origin — are not on it, so they are
//     served as application/octet-stream.
//   - X-Content-Type-Options: nosniff, so the browser cannot recover the
//     dangerous type by sniffing the bytes.
//   - Content-Security-Policy: default-src 'none'; sandbox — a defense in depth
//     for the case where a future change (or a browser quirk) renders a response
//     inline anyway: no script, no fetch, no same-origin privileges.
//   - Cross-Origin-Resource-Policy: same-origin, so another site cannot embed a
//     user's attachment.
//
// 3. QUARANTINE — a part whose scan verdict is `flagged` or `unknown` is not
// served at all, unless the user has released its content (ADR-027 §8, #396).
//
//   - This is the ONE enforcement point for the whole system, and it is here
//     rather than in the client because the client is not the only caller. See
//     quarantineGate for the four paths it covers with one check.
//   - It gates PARTS. The raw message is deliberately not blocked — see
//     serveWholeBlob — and says so in a header instead.
//   - Nothing is ever deleted, and the message stays readable. The scanner is
//     the component most likely to be wrong, so the failure mode chosen here is
//     "hard to reach", never "gone".

// blobRef is a parsed JMAP blobId: the message's content hash, and (optionally)
// the MIME part within it that the id names.
//
// The suffixed form is minted by internal/api/body.go (assignPartIDs):
// "<messageBlobId>.<partId>", e.g. "966f…e6.1.2". It is self-describing — the
// server keeps no table of part blob ids — which is exactly why it must be
// re-derived from the message bytes here (the same walk, the same numbering)
// rather than trusted as an offset into anything.
type blobRef struct {
	hash []byte
	part string // "" for the whole message
	// decrypted marks a part of the DECRYPTED entity of an encrypted message
	// ("<hash>.e.<partPath>", #47, ADR-037). Its bytes are re-derived by
	// decrypting again and scanned on demand; they are never stored.
	decrypted bool
}

// maxPartPathLen bounds the part component of a blobId. A part path is a dotted
// list of 1-based indexes ("2.1.3"); a message deep enough to need more than
// this is past internal/mime's own limits anyway.
const maxPartPathLen = 64

// parseBlobID parses "<64 hex>" or "<64 hex>.<partPath>". It is deliberately
// strict: anything else is not a blobId this server ever minted, and a permissive
// parser here is how a path-traversal or an oversized allocation gets in.
func parseBlobID(s string) (blobRef, error) {
	hashHex, part, hasPart := strings.Cut(s, ".")
	raw, err := hex.DecodeString(hashHex)
	if err != nil || len(raw) != 32 {
		return blobRef{}, errors.New("api: blobId is not a content hash")
	}
	if !hasPart {
		return blobRef{hash: raw}, nil
	}
	decrypted := false
	if rest, ok := strings.CutPrefix(part, decryptedPartPrefix); ok {
		decrypted, part = true, rest
	}
	if part == "" || len(part) > maxPartPathLen {
		return blobRef{}, errors.New("api: blobId has an invalid part")
	}
	for _, seg := range strings.Split(part, ".") {
		if seg == "" {
			return blobRef{}, errors.New("api: blobId has an empty part segment")
		}
		for _, c := range seg {
			if c < '0' || c > '9' {
				return blobRef{}, errors.New("api: blobId part is not a part path")
			}
		}
	}
	return blobRef{hash: raw, part: part, decrypted: decrypted}, nil
}

// DownloadHandler implements GET /jmap/download/{accountId}/{blobId}/{name}
// (RFC 8620 §6.2). See the security model at the top of this file — every rule
// in it is applied here, in order, before a single byte is read.
func (a *API) DownloadHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextUserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The account is the caller's own, ALWAYS. A request naming someone else's
	// account is answered as if the blob simply did not exist: confirming that
	// the account exists would already be a leak.
	if !accountMatches(userID, r.PathValue("accountId")) {
		blobNotFound(w)
		return
	}
	ref, err := parseBlobID(r.PathValue("blobId"))
	if err != nil || a.blobs == nil {
		blobNotFound(w)
		return
	}

	owned, size, isMessage, messageID, err := a.blobOwnedByUser(r.Context(), userID, ref.hash)
	if err != nil {
		http.Error(w, "cannot resolve the blob", http.StatusInternalServerError)
		return
	}
	if !owned {
		blobNotFound(w)
		return
	}

	if ref.part == "" {
		a.serveWholeBlob(w, r, userID, ref.hash, size, isMessage, messageID)
		return
	}
	a.servePart(w, r, userID, ref, messageID)
}

// quarantineGate is THE enforcement point (ADR-027 §8), and it is one point on
// purpose. The first draft of that section had none, and named "preview refuses,
// download requires a release" as though those were the doors. They are not:
//
//   - CID INLINE IMAGES RENDER ON MESSAGE OPEN, fetched through this very route.
//     A flagged image reaches the browser's codecs with ZERO clicks, so "the text
//     is not the threat" is false as stated — an attachment can be part of the
//     rendering.
//   - IMAGE AND TEXT PREVIEWS NEVER TOUCH PreviewHandler. The client renders them
//     from bytes it fetched here; the 415 over there gates PDFs and nothing else.
//   - A THIRD-PARTY JMAP CLIENT IGNORES A PROPERTY entirely.
//   - AND RAW `fetch` IGNORES EVERY CLIENT WE WRITE.
//
// One gate on the part-download route covers all four automatically, which is
// why it is here and not in four places that would have to agree.
//
// ⚠ IT IS KEYED ON THE STORED STATE, NOT ON THE BYTES. No scan happens in a
// request path: the verdict was produced at ingest or by re-scan and is read
// from app.attachments. That is what makes the check cheap enough to sit in
// front of every inline image in a rendering message.
//
// found=false — no projected row for this path — does NOT block. A text body
// part legitimately has no row, and so does an attachment on a message ingested
// before the projection existed. Refusing those would turn a coverage gap into
// a broken mailbox, and the coverage gap is what `attachment-report` measures.
func (a *API) quarantineGate(ctx context.Context, userID, messageID ulid.ULID, partPath string) (blocked bool, state string, err error) {
	if messageID.Compare(ulid.ULID{}) == 0 {
		// An uploaded blob the client has not attached to a message: nothing
		// has ever scanned it and no row describes it.
		return false, "", nil
	}
	var st store.PartScanState
	var found bool
	err = a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var terr error
		st, found, terr = tx.AttachmentScanState(ctx, messageID, partPath)
		return terr
	})
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", nil
	}
	return attachscan.Blocked(st.Status, st.Released), st.Status, nil
}

// blobQuarantined is the refusal. 403 rather than 404: the part exists, the user
// owns it, and pretending otherwise would leave them unable to tell a quarantine
// from a broken link — which is the difference between a warning they can act on
// and a bug report.
//
// ⚠ THE TWO BLOCKING STATES GET THE SAME DOOR AND DIFFERENT WORDS (§5b).
// "flagged" is a claim; "unknown" is the admission that no claim could be made.
// Giving `unknown` a softer response is the whole hole §5b describes: the
// adversary then never has to beat the scanner, only avoid it.
//
// `no-store` because a cached 403 would survive the release that was supposed to
// clear it, and the user would be left clicking a button that appears to do
// nothing.
func blobQuarantined(w http.ResponseWriter, r *http.Request, state, signature string, framable bool) {
	hardenBlobResponse(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Mailroom-Attachment-Scan", state)
	// The detection name, when the caller has one (only a decrypted part does:
	// stored verdicts do not keep it). A header beside the state, so a client
	// can explain the refusal in-app without parsing prose; clamd's own name for
	// the match, sanitised to printable ASCII.
	if sig := sanitizeSignature(signature); sig != "" && state == string(clamd.Flagged) {
		w.Header().Set("X-Mailroom-Attachment-Signature", sig)
	}
	// ⚠ THE WORDING NAMED A CONTROL THAT DID NOT EXIST (#412). It said "release
	// it in the reading pane", and there was no such thing anywhere in the
	// client — so the one instruction the refusal gave was false, and the user's
	// only actual recovery was an admin CLI command they would never find.
	//
	// ⚠ AND THE TWO STATES KEEP DIFFERENT WORDS FOR THE SAME DOOR (§5b).
	// "we checked and it is bad" and "we could not check this" warrant the same
	// friction and a different explanation: collapsing them would either
	// overstate `unknown` — most of which is an encrypted archive or a
	// password-protected document rather than malware — or understate `flagged`.
	detail := "this attachment was flagged by the malware scanner and is being withheld; " +
		"open the message in Mailroom to see why, and release it there if you are sure it is safe"
	if state != string(clamd.Flagged) {
		detail = "this attachment could not be checked by the malware scanner — commonly because it " +
			"is encrypted or password-protected — so it is being withheld; open the message in " +
			"Mailroom to see why, and release it there if you are sure it is safe"
	}
	// ⚠ A BROWSER NAVIGATION GETS A PAGE THAT EXPLAINS ITSELF (#805). The web
	// client asks before it navigates, but "Open link in new tab", "Save link as"
	// and a framed preview are navigations the page cannot intercept, and the
	// JSON body above is a wall of braces to a person. Same status, same
	// headers, still no content; only the body differs. A fetch() (Sec-Fetch-Mode
	// cors/same-origin) keeps the JSON.
	if wantsHTML(r) {
		writeQuarantinePage(w, state, signature, detail, framable)
		return
	}
	writeJSON(w, http.StatusForbidden,
		newRequestError("attachmentQuarantined", http.StatusForbidden, detail))
}

// wantsHTML reports whether the request is a browser navigation (a top-level
// page, a frame): Sec-Fetch-Mode: navigate, which a page script cannot forge
// and a fetch() never carries. A client that sends no Sec-Fetch-* headers (curl,
// an old browser, a JMAP client) gets the JSON.
func wantsHTML(r *http.Request) bool {
	return r != nil && r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// quarantinePage is the whole page. No script, no style, no external resource:
// the response CSP is `default-src 'none'; sandbox`, and the page works under
// it as plain HTML. Every dynamic value goes through html.EscapeString.
const quarantinePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Attachment blocked</title>
</head>
<body>
<h1>Attachment blocked</h1>
<p>%s</p>
%s%s</body>
</html>
`

// writeQuarantinePage answers the 403 for a navigation. The detection name,
// shown only for a flagged part, has been through sanitizeSignature (printable
// ASCII, 128 bytes) and is escaped on top of that, so neither a scanner
// database entry nor anything a message controls can become markup. A preview
// frame (framable) is the same page without the link out: the frame cannot
// load the app, which forbids being framed.
func writeQuarantinePage(w http.ResponseWriter, state, signature, detail string, framable bool) {
	h := w.Header()
	if framable {
		// The preview route's own relaxation (writePreviewHeaders): our own
		// origin may frame it, so the explanation shows where the PDF would have.
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'self'")
		h.Set("X-Frame-Options", "SAMEORIGIN")
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	extra := ""
	if sig := sanitizeSignature(signature); sig != "" && state == string(clamd.Flagged) {
		extra = "<p>Detection: " + html.EscapeString(sig) + "</p>\n"
	}
	back := ""
	if !framable {
		back = "<p><a href=\"/\">Return to Mailroom</a></p>\n"
	}
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, quarantinePage, html.EscapeString(detail), extra, back)
}

// sanitizeSignature keeps a clamd signature name header-safe: printable ASCII
// only, 128 bytes at most. Names come from the signature database, not from
// the mail, but a header value is no place to trust that.
func sanitizeSignature(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			continue
		}
		if b.Len() >= 128 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// blobOwnedByUser is the AUTHORIZATION check. It answers one question — "does
// THIS user have this blob?" — and it answers it in Postgres, under RLS, in a
// UserTx keyed by the authenticated user id (never by anything in the request).
//
// The size it returns is the message's stored byte count, used as Content-Length
// for the streaming whole-message case.
// isMessage distinguishes the two kinds of blob a bare (unsuffixed) blobId can
// name: a canonical MESSAGE, whose bytes really are RFC 5322, and an uploaded
// blob the client has not attached to anything yet, whose bytes are whatever it
// sent. They must not be served with the same Content-Type — see serveWholeBlob.
//
// It also returns the MESSAGE ID, zero for an unattached upload. ⚠ The
// quarantine gate needs it and must not re-derive it: `app.attachments` is keyed
// on (message_id, part_path) while a blobId names a content hash, so a second
// lookup would be a second answer to "which message is this?" — and a gate that
// resolved a different message than the one being served would read the wrong
// verdict without erroring.
func (a *API) blobOwnedByUser(ctx context.Context, userID ulid.ULID, hash []byte) (found bool, size int64, isMessage bool, messageID ulid.ULID, err error) {
	err = a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		id, ok, err := tx.MessageIDByContentHash(ctx, hash)
		if err != nil {
			return err
		}
		if ok {
			ref, err := tx.MessageRef(ctx, id)
			if err != nil {
				return err
			}
			found, size, isMessage, messageID = true, ref.SizeBytes, true, id
			return nil
		}
		// Not a message: an uploaded blob the client has not attached yet. Ask
		// Postgres, in this same RLS-scoped transaction, rather than the blob
		// store. The blob store's per-user namespace would also answer correctly
		// (keyed path derivation + a DEK sealed with the user id as AEAD AAD), but
		// ownership is a fact about the user's rows and it should be the database
		// that says so — one authority, enforced by RLS, for both cases.
		found, err = tx.BlobExists(ctx, hash)
		return err
	})
	if err != nil {
		return false, 0, false, ulid.ULID{}, err
	}
	return found, size, isMessage, messageID, nil
}

// serveWholeMessage streams an unsuffixed blobId: the raw RFC 5322 bytes of the
// message. It is STREAMED (blob.Get pipes the decrypted plaintext) — a 50 MiB
// message never becomes a 50 MiB allocation per request.
func (a *API) serveWholeBlob(w http.ResponseWriter, r *http.Request, userID ulid.ULID, hash []byte, size int64, isMessage bool, messageID ulid.ULID) {
	rc, err := a.blobs.OpenRaw(r.Context(), userID, hash)
	if err != nil {
		blobNotFound(w)
		return
	}
	defer func() { _ = rc.Close() }()

	// ⚠ THE RAW MESSAGE IS NOT GATED, AND THAT IS A DECISION (ADR-027 §11).
	// These bytes are the user's own mail, and a quarantined part sits inside
	// them base64-encoded: inert until something deliberately extracts it, which
	// is a different act from a browser decoding an inline image on open. §12's
	// posture is that refusing to hand someone their own file is user-hostile
	// and the browser-side risk is what gets neutralised. Blocking here would
	// also make a message with one bad attachment un-fetchable, un-forwardable
	// and un-exportable in its entirety, which fails the "the message text stays
	// readable" rule this whole design is built on.
	//
	// What it must not be is SILENT. The header below states the worst state
	// among the message's parts, and the per-message export sidecar carries the
	// same facts durably (§11) — an export that shipped a flagged file's bytes
	// with no verdict is exactly how the silently-waiting attachment gets
	// resurrected on the next instance.
	scanState := a.worstScanState(r.Context(), userID, messageID)

	// message/rfc822 is the honest type for a canonical message, and is not a
	// scripting context. An UNATTACHED UPLOAD is not a message, though — its bytes
	// are whatever the client sent — so calling it message/rfc822 would be the
	// server asserting something false about content it did not author. We do not
	// know its real type (the upload's declared type is not persisted, and we would
	// not trust a client-declared type here anyway), so say exactly that:
	// application/octet-stream. With the attachment disposition and nosniff below,
	// a browser saves either one rather than interpreting it.
	contentType := "application/octet-stream"
	if isMessage {
		contentType = "message/rfc822"
	}
	writeWholeBlobHeaders(w, contentType, downloadName(r, ""), size, isMessage, scanState)
	// A failure mid-copy is an AEAD/hash verification failure (blob.Get verifies
	// as it drains): the response is truncated rather than completed with
	// untrustworthy bytes, which is the right way to fail.
	_, _ = io.Copy(w, rc)
}

// servePart serves one MIME part of a message.
//
// The part is located by re-running the SAME walk Email/get used to mint the id
// (body.go's assignPartIDs), so "the blobId the client was given" and "the part
// the server serves" cannot drift apart. internal/mime has already removed the
// transfer encoding, so what is written is the decoded octets.
//
// This path does buffer: internal/mime parses a whole message at once (it is the
// single MIME authority in this codebase — docs/mime-policy.md — and a second,
// streaming parser here would be a second answer to "which bytes are part 1.2").
// The cost is bounded by the message size and is the same cost Email/get already
// pays to render a body.
func (a *API) servePart(w http.ResponseWriter, r *http.Request, userID ulid.ULID, ref blobRef, messageID ulid.ULID) {
	// A part of a DECRYPTED message has no stored verdict to gate on and no
	// stored bytes to serve: it is re-derived and scanned on demand (#47).
	if ref.decrypted {
		a.serveDecryptedPart(w, r, userID, ref, messageID, false)
		return
	}
	// ⚠ THE GATE RUNS BEFORE THE BYTES ARE READ, not after. Deciding to refuse
	// only once the plaintext is in hand would still have decrypted and parsed
	// attacker-controlled content on every blocked request, and would leave the
	// refusal one early `return` away from being skipped.
	blocked, state, err := a.quarantineGate(r.Context(), userID, messageID, ref.part)
	if err != nil {
		http.Error(w, "cannot resolve the blob", http.StatusInternalServerError)
		return
	}
	if blocked {
		blobQuarantined(w, r, state, "", false)
		return
	}
	// ⚠ THE CONDITIONAL ANSWER COMES AFTER THE GATE, NEVER BEFORE IT (#410). A
	// 304 is permission to reuse the cached bytes, so answering one for a
	// quarantined part would disclose exactly what the 403 above refuses — and
	// putting this check earlier is the obvious "optimisation" that would do it.
	// The saving is the blob read and MIME parse below, which is precisely what
	// makes the ordering tempting.
	etag := partETag(ref)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		notModified(w, etag)
		return
	}
	data, contentType, partName, err := a.blobBytes(r.Context(), userID, ref)
	if err != nil {
		blobNotFound(w)
		return
	}
	writeBlobHeaders(w, safeContentType(contentType), downloadName(r, partName), int64(len(data)), etag)
	_, _ = w.Write(data)
}

// worstScanState reports the most serious state among a message's projected
// parts, for the raw-download header. Empty when nothing is projected or
// everything is clean.
//
// ⚠ RELEASED PARTS DO NOT COUNT. The header exists to warn, and a part the user
// has already looked at and accepted is not news.
func (a *API) worstScanState(ctx context.Context, userID, messageID ulid.ULID) string {
	state, err := a.blockedPartState(ctx, userID, messageID)
	if err != nil {
		// A header is not worth failing a download over; the durable record is
		// the export sidecar.
		return ""
	}
	return state
}

// blockedPartState reports the most serious BLOCKING state among a message's
// projected parts, or "" when none of them is blocked.
//
// ⚠ IT RETURNS AN ERROR AND worstScanState SWALLOWS ONE, and that split is the
// whole reason this is a separate function. The two callers have opposite error
// policies for the same question, exactly like internal/attachscan's two scan
// loops: a raw download degrades to a missing advisory header, while the draft
// gate (#413) must FAIL CLOSED — a database blip there would otherwise let a
// quarantined message be attached to outbound mail, which is the one direction
// ADR-027 §11 says must never be silent.
func (a *API) blockedPartState(ctx context.Context, userID, messageID ulid.ULID) (string, error) {
	if messageID.Compare(ulid.ULID{}) == 0 {
		return "", nil
	}
	var states []store.PartScanState
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		byMsg, err := tx.AttachmentScanStates(ctx, []ulid.ULID{messageID})
		states = byMsg[messageID]
		return err
	}); err != nil {
		return "", err
	}
	worst := ""
	for _, st := range states {
		if !attachscan.Blocked(st.Status, st.Released) {
			continue
		}
		if st.Status == string(clamd.Flagged) {
			return st.Status, nil
		}
		worst = st.Status
	}
	return worst, nil
}

// PreviewHandler implements GET /jmap/preview/{accountId}/{blobId}/{name} — the
// ONE route on this server that serves a stranger's bytes for INLINE rendering
// (Forgejo #131, spec §11's "sandboxed viewer").
//
// docs/hostile-content.md §2.4 used to be able to say there were exactly two
// doors: rendering is sanitized, downloading is inert. This is a third, and it is
// worth being blunt about why it exists rather than pretending it is a variant of
// the second. The alternative to previewing an attachment is DOWNLOADING it and
// opening it in a desktop application — handing a hostile document to a
// full-featured local viewer with the user's own privileges, which is the attack
// path the whole reading pane exists to close. A door that renders under our own
// controls is safer than a door that hands the bytes to Acrobat.
//
// FOUR THINGS MAKE IT NARROWER THAN THE DOWNLOAD DOOR, not wider:
//
//  1. IT SERVES ALMOST NOTHING. inlinePreviewTypes is one entry long. Everything
//     else — including every type the download door is happy to name — is refused
//     with 415 rather than served with a different disposition. The download
//     allowlist says "these are safe to save"; this one says "these are safe to
//     RENDER", and they are not the same question.
//  2. THE OPAQUE ORIGIN IS IN THE RESPONSE, not in the caller's iframe. The CSP
//     `sandbox` directive makes the document's origin opaque no matter who frames
//     it, so the guarantee does not depend on the client remembering an attribute.
//     Verified against Chromium: with `sandbox` a hostile HTML body served here
//     runs no script and is unreachable from the embedder; without it, the
//     embedder can reach into the document.
//  3. IT IS ONLY FRAMEABLE BY US. `frame-ancestors 'self'` + X-Frame-Options
//     SAMEORIGIN. This is the one header that is LOOSER than the download
//     response's (which says 'none'), and it has to be: 'none' means the browser
//     refuses to frame it at all, which is the whole feature.
//  4. THE TYPE IS STILL OURS. safeContentType() first, then the inline allowlist,
//     and `?type=` is ignored exactly as it is on the download door.
//
// A WHOLE blob is never previewable: a bare blobId names raw RFC 5322 (or an
// unattached upload), whose bytes are not a document we have any business
// rendering.
func (a *API) PreviewHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextUserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !accountMatches(userID, r.PathValue("accountId")) {
		blobNotFound(w)
		return
	}
	ref, err := parseBlobID(r.PathValue("blobId"))
	if err != nil || a.blobs == nil {
		blobNotFound(w)
		return
	}
	if ref.part == "" {
		previewUnsupported(w)
		return
	}

	owned, _, _, messageID, err := a.blobOwnedByUser(r.Context(), userID, ref.hash)
	if err != nil {
		http.Error(w, "cannot resolve the blob", http.StatusInternalServerError)
		return
	}
	if !owned {
		blobNotFound(w)
		return
	}

	if ref.decrypted {
		a.serveDecryptedPart(w, r, userID, ref, messageID, true)
		return
	}

	// The quarantine applies here too. ADR-027 §8 calls the download route the
	// single enforcement point because it is the one that covers CID rendering,
	// client-side previews and foreign clients — but this route hands the same
	// stranger's bytes to a browser's PDF engine for INLINE rendering, so
	// leaving it open would be leaving the more dangerous of the two doors
	// unlocked because the other one covers more traffic.
	blocked, state, err := a.quarantineGate(r.Context(), userID, messageID, ref.part)
	if err != nil {
		http.Error(w, "cannot resolve the blob", http.StatusInternalServerError)
		return
	}
	if blocked {
		blobQuarantined(w, r, state, "", true)
		return
	}

	// Same ordering rule as servePart: the gate first, the conditional answer
	// second. A 304 here would let a browser re-render cached bytes inline in
	// its PDF engine — the more dangerous of the two doors.
	etag := partETag(ref)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		notModified(w, etag)
		return
	}
	data, contentType, partName, err := a.blobBytes(r.Context(), userID, ref)
	if err != nil {
		blobNotFound(w)
		return
	}
	// safeContentType FIRST — so what is checked against the inline allowlist is
	// the type WE would serve, never the one the message declared. A part claiming
	// application/pdf whose declared type is really text/html has already become
	// application/octet-stream by here, and octet-stream is not previewable.
	ct := safeContentType(contentType)
	if !inlinePreviewTypes[ct] {
		previewUnsupported(w)
		return
	}
	writePreviewHeaders(w, ct, downloadName(r, partName), int64(len(data)), etag)
	_, _ = w.Write(data)
}

// inlinePreviewTypes is the allowlist of types this server will serve with an
// INLINE disposition. It is deliberately far shorter than downloadableTypes,
// because the two answer different questions: that one is "safe to hand to the
// user's disk", this one is "safe to render in a frame on our own origin".
//
// application/pdf only, for now. A PDF is the case the feature exists for and the
// one browsers render natively in an isolated viewer process. Images and text do
// NOT need to be here — the client renders those from bytes it already fetched
// (a `data:` URL and text nodes respectively), which needs no inline door at all,
// so giving them one would widen this surface for nothing.
//
// NEVER add text/html, application/xhtml+xml, image/svg+xml or the XML family.
// safeContentType already refuses to name them, so an entry here would be dead —
// but it would be dead code sitting one edit away from being a scripting context
// served inline from the operator's own origin, which is the worst outcome this
// file exists to prevent.
var inlinePreviewTypes = map[string]bool{
	"application/pdf": true,
}

// writePreviewHeaders is the download header set with two deliberate differences,
// both required for the response to be renderable at all.
//
//   - `Content-Disposition: inline`, the entire point.
//   - `frame-ancestors 'self'` and `X-Frame-Options: SAMEORIGIN` rather than
//     'none'/DENY. The download response forbids framing outright, and a browser
//     enforces that by refusing the subresource load (verified: Chromium answers
//     net::ERR_BLOCKED_BY_RESPONSE) — so a preview MUST relax it, and relaxes it
//     to exactly our own origin and no further.
//
// Everything else is unchanged, and `sandbox` is the load-bearing one: it makes
// the served document's origin OPAQUE regardless of how it is embedded, so the
// bytes cannot reach our cookies, our storage or our DOM even if a browser
// someday decides to treat them as a document.
func writePreviewHeaders(w http.ResponseWriter, contentType, filename string, size int64, etag string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'self'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", inlineDisposition(filename))
	if size > 0 {
		h.Set("Content-Length", fmt.Sprintf("%d", size))
	}
	h.Set("Cache-Control", revalidateCacheControl)
	h.Set("ETag", etag)
}

// inlineDisposition renders `inline` with the same RFC 6266/5987 filename
// encoding contentDisposition uses, so a hostile filename cannot reach a header
// raw here either. The filename still matters on an inline response: it is what
// the browser's own "save" control in the PDF viewer will offer.
func inlineDisposition(name string) string {
	if name == "" {
		name = "preview"
	}
	ascii := asciiFilename(name)
	return fmt.Sprintf(`inline; filename="%s"; filename*=UTF-8''%s`, ascii, encodeRFC5987(name))
}

// previewUnsupported is the answer for anything this route will not render: a
// clean 415, so a client can say "no preview for this file type" and fall back to
// the download link. Hardened like every other response from these endpoints.
//
// It is deliberately NOT a fallback to serving the bytes with an attachment
// disposition. A caller asked to render something; answering with a download
// would leave a frame pointed at a file the browser then offers to save, which is
// a confusing thing to do to someone who clicked "preview".
func previewUnsupported(w http.ResponseWriter) {
	hardenBlobResponse(w)
	writeJSON(w, http.StatusUnsupportedMediaType,
		newRequestError("previewUnsupported", http.StatusUnsupportedMediaType,
			"this part cannot be previewed; download it instead"))
}

// blobBytes returns the decoded octets a blobRef names, plus the part's declared
// media type and filename. The caller MUST have authorized the blob first
// (blobOwnedByUser) — this function reads bytes, it does not decide who may.
//
// It is shared by the download endpoint and by Email/set create's attachment
// resolution, so "which bytes are part 1.2 of message X" has exactly one answer
// in this server, whichever door the question comes through.
func (a *API) blobBytes(ctx context.Context, userID ulid.ULID, ref blobRef) (data []byte, contentType, name string, err error) {
	if ref.decrypted {
		// ⚠ Decrypted bytes are never read through here: this function feeds
		// Email/set's attachment resolution, and attaching a decrypted part
		// would store its plaintext in a draft blob (ADR-037).
		return nil, "", "", errors.New("api: a part of an encrypted message is not stored and cannot be read here")
	}
	raw, err := a.blobs.ReadRaw(ctx, userID, ref.hash)
	if err != nil {
		return nil, "", "", err
	}
	if ref.part == "" {
		return raw, "message/rfc822", "", nil
	}
	parsed, _ := mime.ParseBytes(raw)
	if parsed == nil || parsed.Structure == nil {
		return nil, "", "", errors.New("api: message has no MIME structure")
	}
	for st, node := range assignPartIDs(parsed.Structure, hex.EncodeToString(ref.hash), "") {
		if node.partID == ref.part {
			return st.Data, st.ContentType, st.Filename, nil
		}
	}
	return nil, "", "", errors.New("api: no such part")
}

// UploadHandler implements POST /jmap/upload/{accountId}/ (RFC 8620 §6.1): the
// body is stored as a blob for the authenticated user and the response names it.
// Without this route a client can compose but never attach.
func (a *API) UploadHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextUserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !accountMatches(userID, r.PathValue("accountId")) {
		writeJSON(w, http.StatusNotFound,
			newRequestError(errAccountNotFound, http.StatusNotFound, "no such account"))
		return
	}
	if a.blobs == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			newRequestError("serverUnavailable", http.StatusServiceUnavailable,
				"this server is not configured to store blobs"))
		return
	}

	// maxSizeUpload, enforced (session.go advertises exactly this const). The
	// reader caps the STREAM, so an oversized upload is refused as it arrives —
	// it is never assembled anywhere.
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	defer func() { _ = r.Body.Close() }()

	stored, err := a.blobs.PutRaw(r.Context(), userID, body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				newRequestError("requestTooLarge", http.StatusRequestEntityTooLarge,
					fmt.Sprintf("an upload may be at most %d bytes", maxUploadBytes)))
			return
		}
		http.Error(w, "cannot store the upload", http.StatusInternalServerError)
		return
	}

	// Record the blob in Postgres, in the caller's own transaction (RLS). This is
	// what makes the upload the USER's blob rather than an anonymous file on
	// disk, and what a later GC of unreferenced blobs will read.
	//
	// The storage quota is checked in the SAME transaction, against usage read
	// under RLS, BEFORE the row is inserted: an over-quota upload records nothing,
	// so the bytes already written to the blob store are just an unreferenced file
	// that GC reclaims, and the client is told plainly nothing was stored (413,
	// not 429 — retrying will not free space). This is the invariant from
	// quota.go: a hit quota is an explicit refusal, never a silent partial write.
	var over *Quota
	if err := a.store.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
		limit, used, err := tx.StorageQuota(r.Context())
		if err != nil {
			return err
		}
		// Delta is the STORED (ciphertext) size, because that is what
		// StorageQuota's usage sum counts (app.blobs.size_stored) — the on-disk
		// footprint the cap actually bounds, not the plaintext length.
		q := Quota{Kind: QuotaStorageBytes, Limit: limit, Used: used}
		if !q.Admits(stored.SizeStored) {
			over = &q
			return nil
		}
		return tx.InsertBlobMeta(r.Context(), blobMeta(stored))
	}); err != nil {
		http.Error(w, "cannot record the upload", http.StatusInternalServerError)
		return
	}
	if over != nil {
		writeOverQuota(w, *over)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"accountId": userID.String(),
		"blobId":    hex.EncodeToString(stored.ContentHash),
		// RFC 8620 §6.1: the type as the client declared it on the request. It is
		// echoed, NOT trusted: the download endpoint derives the type it serves
		// from the message's own MIME structure and its allowlist, never from
		// anything a client said here.
		"type": uploadType(r.Header.Get("Content-Type")),
		"size": stored.SizePlain,
	})
}

// blobMeta converts the blob seam's result into the store's blob metadata row.
func blobMeta(b compose.StoredBlob) store.BlobMeta {
	return store.BlobMeta{
		ContentHash: b.ContentHash,
		SizePlain:   b.SizePlain,
		SizeStored:  b.SizeStored,
		DEKID:       b.DEKID,
		EnvelopeVer: 1,
	}
}

// accountMatches enforces spec §10 at the HTTP layer: the {accountId} in the
// path must be the authenticated user's own account (whose id IS the user's
// ULID). It mirrors resolveAccount, which does the same for method calls.
func accountMatches(userID ulid.ULID, accountID string) bool {
	return accountID == userID.String()
}

// blobNotFound is the ONE negative answer the download endpoint gives: a blob
// that does not exist, a blob belonging to somebody else, an account that is not
// yours, and a malformed blobId are indistinguishable from outside. Anything
// finer-grained is an oracle.
func blobNotFound(w http.ResponseWriter) {
	hardenBlobResponse(w)
	writeJSON(w, http.StatusNotFound,
		newRequestError("notFound", http.StatusNotFound, "no such blob"))
}

// revalidateCacheControl is the caching policy for any response whose CONTENT is
// stable but whose DELIVERY depends on mutable stored state (#410).
//
// ⚠ `immutable` WAS CORRECT UNTIL THE GATE EXISTED, AND THAT IS THE WHOLE LESSON.
// Blobs are content-addressed, so "this blobId always denotes these bytes" was
// true and `immutable, max-age=1y` followed from it. ADR-027 §8's quarantine made
// the RESPONSE a function of `app.attachments.scan_status`, which a re-scan
// rewrites — and nobody revisited the header. `immutable` tells a browser never
// to revalidate, so:
//
//	day 0  the message is opened; every inline CID image is fetched through the
//	       download route with zero clicks and cached for a year
//	day 7  a re-scan flags it, propagateFlag marks every copy
//	after  the gate answers 403 and THE BROWSER NEVER ASKS
//
// That is precisely the zero-click case §8 was written for, re-opened by a
// header. §5c's own argument makes it the EXPECTED case rather than an edge one:
// scan-at-ingest is the moment of minimum signature coverage, so a part that
// arrives clean and is flagged days later is the normal life cycle.
//
// ⚠ `no-cache` DOES NOT MEAN "DO NOT STORE". It means "store it, but revalidate
// before every use" — so the bytes stay in the cache and a conditional request
// with If-None-Match reaches the gate, which answers 403 or 304. That keeps
// almost all of the bandwidth win for inline images, which is why this is not
// the blunt `no-store` the issue also offered: a newsletter can carry a dozen
// images and re-fetching all of them on every message open is a real cost.
const revalidateCacheControl = "private, no-cache, max-age=0, must-revalidate"

// immutableCacheControl is for a response that is a pure function of its blobId
// in EVERY respect — body and headers alike. See writeWholeBlobHeaders for why
// the raw-message download does NOT qualify.
const immutableCacheControl = "private, max-age=31536000, immutable"

// partETag is the validator for one attachment part: its blobId, which is
// `<messageHash>.<partPath>` and names exactly these bytes.
//
// Strong rather than weak, and that is accurate — the entity really is
// byte-identical across requests. What varies is whether we will hand it over,
// which is what the revalidation is for.
func partETag(ref blobRef) string {
	if ref.decrypted {
		return `"` + hex.EncodeToString(ref.hash) + "." + decryptedPartPrefix + ref.part + `"`
	}
	return `"` + hex.EncodeToString(ref.hash) + "." + ref.part + `"`
}

// notModified answers a conditional request.
//
// ⚠ IT IS ONLY EVER REACHED AFTER THE GATE HAS PASSED, and that ordering is the
// entire point of #410's fix. A 304 is permission to reuse the cached bytes, so
// emitting one for a quarantined part would be the same disclosure as serving
// them. servePart checks the gate first and returns 403 before this is called.
func notModified(w http.ResponseWriter, etag string) {
	h := w.Header()
	hardenBlobResponse(w)
	h.Set("ETag", etag)
	h.Set("Cache-Control", revalidateCacheControl)
	w.WriteHeader(http.StatusNotModified)
}

// etagMatches reports whether If-None-Match names this entity. Handles the
// comma-separated list form and `*`; weak comparison is correct for a GET.
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// writeBlobHeaders emits the defensive header set for an attachment PART. See
// the security model at the top of this file for why each one is here.
func writeBlobHeaders(w http.ResponseWriter, contentType, filename string, size int64, etag string) {
	h := w.Header()
	hardenBlobResponse(w)
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", contentDisposition(filename))
	if size > 0 {
		h.Set("Content-Length", fmt.Sprintf("%d", size))
	}
	if etag != "" {
		h.Set("ETag", etag)
	}
	h.Set("Cache-Control", revalidateCacheControl)
}

// writeWholeBlobHeaders is the header set for a bare blobId — a canonical
// message's raw bytes, or an unattached upload.
//
// ⚠ THE BODY IS IMMUTABLE AND THE HEADERS ARE NOT, which is why this is a
// separate function rather than the same one with `immutable`. #410 suggested
// the raw download "genuinely is immutable and can keep the header"; that is
// true of the bytes and false of the response. serveWholeBlob sets
// `X-Mailroom-Attachment-Scan` from worstScanState, which reads the same rows a
// re-scan rewrites — and the code there says the refusal "must not be SILENT".
// A cached-immutable response is silent again, freezing the verdict a browser
// saw on its first fetch.
//
// The bytes are deliberately NOT gated (§11: refusing someone their own mail is
// user-hostile, and a quarantined part inside a .eml is inert until something
// extracts it), so nothing is disclosed here that was not already handed over.
// What is preserved is the honesty of the advisory header — and a raw-message
// download is user-initiated and rare, so revalidating costs approximately
// nothing, unlike the inline images that motivated keeping `no-cache` over
// `no-store` above.
// ⚠ THE SWITCH IS `isMessage`, NOT "does it currently carry a scan state", and
// the difference is a bug I nearly shipped. Keying on a non-empty header would
// cache a message hard TODAY — while nothing is flagged — and that cached
// response then survives the flag that arrives tomorrow, which is the very
// defect this function exists to fix, moved one branch along. A message's scan
// state can always change; an unattached upload has no projected parts and never
// acquires any, so only that case is genuinely immutable.
func writeWholeBlobHeaders(w http.ResponseWriter, contentType, filename string, size int64, isMessage bool, scanState string) {
	h := w.Header()
	hardenBlobResponse(w)
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", contentDisposition(filename))
	if size > 0 {
		h.Set("Content-Length", fmt.Sprintf("%d", size))
	}
	if scanState != "" {
		h.Set("X-Mailroom-Attachment-Scan", scanState)
	}
	if isMessage {
		h.Set("Cache-Control", revalidateCacheControl)
		return
	}
	h.Set("Cache-Control", immutableCacheControl)
}

// hardenBlobResponse sets the headers that must be present on EVERY response
// from these endpoints, including the error ones (a 404 body is JSON, but the
// browser must not be free to reinterpret it either).
func hardenBlobResponse(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
}

// downloadableTypes is the ALLOWLIST of media types we are willing to name in a
// Content-Type header. It is short on purpose, and it is an allowlist rather
// than a blocklist because the failure mode of a missed entry has to be "the
// browser saves the file" and never "the browser executes it".
//
// Conspicuously absent, and never to be added: text/html, application/xhtml+xml,
// image/svg+xml, and the XML family. Each is a scripting context, and serving
// one inline from the operator's own origin turns every phishing email into
// stored XSS against their mailbox.
var downloadableTypes = map[string]bool{
	"text/plain": true,

	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
	"image/avif": true,
	"image/tiff": true,
	"image/heic": true,

	"audio/mpeg": true,
	"audio/mp4":  true,
	"audio/ogg":  true,
	"audio/wav":  true,
	"audio/webm": true,
	"audio/flac": true,
	"audio/aac":  true,

	"video/mp4":       true,
	"video/webm":      true,
	"video/ogg":       true,
	"video/quicktime": true,

	"application/pdf": true,
}

// safeContentType maps a part's DECLARED media type onto the type we will serve
// it as. Anything not on the allowlist becomes application/octet-stream, which a
// browser saves rather than renders.
//
// The `type` query parameter of the download URL (RFC 8620's downloadUrl
// template) is deliberately not consulted: it is client-supplied, it is trivially
// forgeable by whoever wrote the phishing email that contains the link, and a
// server that echoes it into its own Content-Type has handed away the only
// control it had.
func safeContentType(declared string) string {
	t := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if !downloadableTypes[t] {
		return "application/octet-stream"
	}
	if t == "text/plain" {
		// internal/mime charset-decodes text parts to UTF-8, so UTF-8 is what we
		// are actually writing, whatever the message declared.
		return "text/plain; charset=utf-8"
	}
	return t
}

// uploadType sanitizes the Content-Type an upload declared, for echoing back in
// the RFC 8620 §6.1 response. It is metadata, not a decision: nothing downstream
// serves bytes as this type.
func uploadType(declared string) string {
	t := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	slash := strings.IndexByte(t, '/')
	if slash <= 0 || slash == len(t)-1 || len(t) > 128 {
		return "application/octet-stream"
	}
	for _, c := range t {
		if !isMediaTypeChar(c) {
			return "application/octet-stream"
		}
	}
	return t
}

// isMediaTypeChar reports whether c may appear in a media type (RFC 6838 token
// characters plus the '/' separator).
func isMediaTypeChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '/' || c == '.' || c == '+' || c == '-' || c == '_':
		return true
	}
	return false
}

// downloadName picks the filename for the Content-Disposition: the {name} the
// client put in the URL (RFC 8620's downloadUrl template lets it choose), else
// the part's own filename, else a neutral default. Both sources are hostile
// input and are sanitized before they go anywhere near a header.
func downloadName(r *http.Request, partName string) string {
	if n := sanitizeFilename(r.PathValue("name")); n != "" {
		return n
	}
	if n := sanitizeFilename(partName); n != "" {
		return n
	}
	return "attachment"
}

// maxFilenameRunes bounds a filename in a header. Long enough for any real
// attachment; short enough that nobody is writing a kilobyte of header.
const maxFilenameRunes = 100

// sanitizeFilename strips everything a filename must not carry: header-injecting
// CR/LF, control characters, quotes/backslashes (which would escape the quoted
// string), path separators, NUL, and scoping bidi controls. What is left is a
// plain name.
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	n := 0
	for _, c := range s {
		if n >= maxFilenameRunes {
			break
		}
		switch {
		case c < 0x20 || c == 0x7f: // control characters, CR, LF, NUL
			continue
		case c >= 0x202a && c <= 0x202e, c >= 0x2066 && c <= 0x2069:
			// Scoping bidi controls. NOT a header-safety problem — they survive
			// the RFC 5987 encoding below intact and inject nothing. This is a
			// FILE-NAMING one: the name in this header is what the browser writes
			// to disk, and U+202E RIGHT-TO-LEFT OVERRIDE turns the stored name
			// "report<RLO>gnp.exe" into "reportexe.png" in every file manager
			// that draws it — an executable wearing an image's extension, chosen
			// by whoever sent the mail.
			//
			// Dropped rather than substituted, and the ranges are deliberately
			// the same two as web/src/core/hostileText.ts's BIDI_RANGES: the
			// client strips what it RENDERS, this strips what gets SAVED, and a
			// name that disagreed between the two would be its own confusion.
			continue
		case c == '"' || c == '\\' || c == '/' || c == ':':
			b.WriteRune('_')
		default:
			b.WriteRune(c)
		}
		n++
	}
	out := strings.Trim(b.String(), " .")
	if out == ".." || out == "." {
		return ""
	}
	return out
}

// contentDisposition renders an RFC 6266 Content-Disposition with an RFC 5987
// encoded filename, so a name with non-ASCII characters survives without ever
// putting a raw byte into a header.
//
// It is ALWAYS `attachment`. Not "attachment for the dangerous types": a
// disposition that depends on the attacker's declared type is a decision the
// attacker makes.
func contentDisposition(name string) string {
	if name == "" {
		name = "attachment"
	}
	ascii := asciiFilename(name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, encodeRFC5987(name))
}

// asciiFilename is the plain `filename=` fallback for clients that do not
// understand RFC 5987: printable ASCII only, everything else becomes '_'.
func asciiFilename(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(c)
	}
	out := b.String()
	if out == "" {
		return "attachment"
	}
	return out
}

// encodeRFC5987 percent-encodes a filename for `filename*=UTF-8”…` (RFC 5987
// §3.2: only attr-char survives unescaped).
func encodeRFC5987(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range []byte(s) {
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// isAttrChar reports whether a byte is RFC 5987 attr-char.
func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// attachmentBlocked answers "must this blobId be refused as a draft attachment",
// for BOTH forms of blobId (#413).
//
// ⚠ THE TWO FORMS ASK DIFFERENT QUESTIONS AND ONLY ONE WAS BEING ASKED. A
// suffixed blobId names one part, so the question is "is that part blocked" —
// quarantineGate. A BARE blobId names the whole message, so the question is "is
// ANY part of it blocked", which nothing was asking: the bare form skipped the
// gate entirely and the quarantined bytes went out inside a forwarded .eml.
//
// Both paths fail closed: a database error refuses rather than admits, because
// the failure being prevented here is outbound.
func (a *API) attachmentBlocked(ctx context.Context, userID, messageID ulid.ULID, partPath string) (bool, string, error) {
	if partPath != "" {
		return a.quarantineGate(ctx, userID, messageID, partPath)
	}
	state, err := a.blockedPartState(ctx, userID, messageID)
	if err != nil {
		return false, "", err
	}
	return state != "", state, nil
}
