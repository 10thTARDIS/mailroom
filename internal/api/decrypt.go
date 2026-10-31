// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/clamd"
	"mailroom/internal/mime"
	"mailroom/internal/pgpmime"
	"mailroom/internal/store"
)

// PGP/MIME decrypt on open (#47 increment 6b, ADR-037).
//
// ⚠ PLAINTEXT IS NEVER PERSISTED, AND THIS FILE IS WHERE THAT IS TRUE OR NOT.
// The crypto process returns a decrypted MIME entity; it lives in this
// request's memory, is rendered through the SAME body pipeline as ordinary mail
// (buildEmailBody: sanitised HTML, quarantine-aware bodyValues), and is
// dropped. Nothing here writes: no column, no blob, no search document, no
// snippet, no ML feature, no cache. Email/get's snippet (`preview`) is the
// Postgres projection ingest wrote from the CIPHERTEXT message, and stays so.
// TestDecryptOnOpenWritesNothing measures that against the database and the
// blob store rather than asserting it.
//
// Decryption happens in exactly two places, both on a request path:
//
//   - Email/get, and only when a BODY property was asked for (decryptWanted).
//     A list view naming its properties never decrypts — and never leaves an
//     audit row.
//   - a part download or preview inside a decrypted message, which re-derives
//     the part by decrypting again and scans it with clamd ON DEMAND before
//     serving it (serveDecryptedPart).

// Decrypter opens one PGP/MIME encrypted part. In production it is the crypto
// process (cmd/mailroom's adapter over pgpwire); tests substitute a function.
//
// Every failure is one opaque error by design (pgpwire.CodeDecryptFailed).
// Callers must not try to tell failures apart, and must not show detail.
//
// signers are candidate PUBLIC keys (binary OpenPGP) for a signature embedded in
// the OpenPGP message (RFC 3156 section 6.2); nil when no verdict is wanted. They
// never decrypt anything.
type Decrypter interface {
	Decrypt(ctx context.Context, userID, messageID ulid.ULID, purpose string, ciphertext []byte, signers [][]byte) (DecryptResult, error)
}

// DecryptResult is the decrypted entity and the embedded-signature result.
type DecryptResult struct {
	Entity    []byte
	Signature EmbeddedSignature
}

// EmbeddedSignature is the crypto process's verdict on a signature inside the
// OpenPGP message (RFC 3156 section 6.2), in this package's terms. The zero value
// is "not signed".
type EmbeddedSignature struct {
	Signed bool
	// Status is EmbeddedVerified, EmbeddedBad or EmbeddedNoKey; anything else is
	// treated as bad.
	Status string
	// SignerFingerprint: the candidate key that verified (primary fingerprint).
	SignerFingerprint string
	// Issuer: what the signature says made it — a fingerprint, or a 16-digit key
	// id when the signature carries no fingerprint.
	Issuer string
	// KeyExpired (Status bad only, #795/#796): the signature verified and was
	// refused only because the candidate key that made it has expired.
	// ExpiredSignerFingerprint names that key; SignedAt and KeyExpiredAt are
	// the signature's creation time and the key's expiry (either may be nil).
	KeyExpired               bool
	ExpiredSignerFingerprint string
	SignedAt                 *time.Time
	KeyExpiredAt             *time.Time
}

// The embedded-signature statuses (pgpwire.EmbeddedSignatureStatus's values).
const (
	EmbeddedVerified = "verified"
	EmbeddedBad      = "bad"
	EmbeddedNoKey    = "noKey"
)

// OnDemandScanner scans bytes that exist only in this request: the plaintext
// of an attachment inside a decrypted message. *clamd.Client satisfies it.
type OnDemandScanner interface {
	Scan(ctx context.Context, data []byte) (clamd.Verdict, error)
}

// WithDecrypter wires decrypt on open. Nil (the default) means encrypted mail is
// shown as it is stored, marked encrypted and not decrypted.
func WithDecrypter(d Decrypter) Option { return func(a *API) { a.decrypter = d } }

// WithOnDemandScanner wires the scanner used for parts of decrypted messages.
// Nil means no scanner is configured, and those parts are `unscanned` — which,
// as everywhere else (ADR-027 §8), does not block.
func WithOnDemandScanner(s OnDemandScanner) Option { return func(a *API) { a.onDemandScan = s } }

// verdictUnscanned is the on-demand verdict when no scanner is configured.
const verdictUnscanned = "unscanned"

// OnDemandVerdicts lists every label the on-demand observer can receive (clamd's
// three states plus "unscanned"), so the metrics sink can create its series at
// zero at boot.
func OnDemandVerdicts() []string {
	return []string{string(clamd.Clean), string(clamd.Flagged), string(clamd.Unknown), verdictUnscanned}
}

// WithOnDemandObserver installs the sink called once per judged attachment of a
// decrypted message (clean, flagged, unknown, unscanned). Read-only: it cannot
// change a verdict. A nil fn only skips the counter; the log line is always
// written.
func WithOnDemandObserver(fn func(verdict string)) Option {
	return func(a *API) { a.onDemandObserve = fn }
}

// decryptedPartPrefix marks a partId (and the blobId that extends it) as naming
// a part of the DECRYPTED entity rather than of the stored ciphertext message:
// "e.1", "e.2.1". Without it the two trees' paths collide ("2" is both the
// ciphertext part and the decrypted entity's second part), and the stored
// quarantine verdicts for the ciphertext would attach to plaintext parts they
// never described.
const decryptedPartPrefix = "e."

// decryptPropertyWanted reports whether props asks for anything that needs the
// decrypted body. The three extension properties alone do not: a client that
// asks only "is this encrypted?" gets the answer from the stored bytes without
// a decryption, an audit row, or a spent rate-limit token.
func decryptPropertyWanted(props map[string]bool) bool {
	for _, p := range []string{"bodyStructure", "textBody", "htmlBody", "attachments", "bodyValues",
		"pgpSignature"} {
		if props[p] {
			return true
		}
	}
	return false
}

// buildEncryptedBody renders an RFC 3156 multipart/encrypted message: decrypted
// when the client asked for the body and the crypto process can open it, the
// stored structure otherwise. Never fails — a failure is the honest
// `decrypted: false`, with the detail in the operator's log only.
func (a *API) buildEncryptedBody(ctx context.Context, userID, msgID ulid.ULID, ev store.EmailView,
	raw []byte, args bodyArgs, props map[string]bool) *emailBody {

	asStored := func(decrypted *bool) *emailBody {
		b := buildEmailBody(raw, ev.ContentHash, args)
		b.encrypted = true
		b.decrypted = decrypted
		return b
	}
	if !decryptPropertyWanted(props) {
		return asStored(nil) // not attempted: neither true nor false
	}
	// DECRYPTION USES THE USER'S PRIVATE KEY, and that is session-only (#49,
	// ADR-042 §3): an API key gets the message as stored, marked encrypted,
	// not attempted. Not "failed" — nothing went wrong.
	if principalFrom(ctx) != nil {
		return asStored(nil)
	}
	failed := false
	if a.decrypter == nil {
		return asStored(&failed)
	}
	// The keys a signature could be judged against are read BEFORE decrypting,
	// because the crypto process checks an embedded (section 6.2) signature in
	// the same pass and needs the candidates up front. Only when the client asked
	// for the verdict: otherwise nothing is read and no key is sent.
	var keys *sigKeys
	var signers [][]byte
	if props["pgpSignature"] {
		if keys = a.loadSigKeys(ctx, userID, raw); keys != nil {
			signers = keys.embeddedCandidates()
		}
	}
	synth, embedded, ok := a.openEncrypted(ctx, userID, msgID, raw, "body", signers)
	if !ok {
		return asStored(&failed)
	}

	dargs := args
	dargs.partPrefix = decryptedPartPrefix
	// The stored quarantine verdicts describe the CIPHERTEXT parts and say
	// nothing about these; and a decrypted part's verdict exists only at
	// download (serveDecryptedPart). So fetchAllBodyValues withholds the value
	// of every attachment-leaf text part here, exactly as it would withhold an
	// unscanned-and-blocked one: the reader downloads it, and the download is
	// scanned. Body text (not attachment leaves) is unaffected — at ingest it
	// is never scanned either.
	if dargs.gatesBodyValues() {
		dargs.blockedParts = map[string]bool{}
		if parsed, _ := mime.ParseBytes(synth); parsed != nil {
			for _, ap := range parsed.AttachmentParts() {
				dargs.blockedParts[decryptedPartPrefix+ap.Path] = true
			}
		}
	}
	b := buildEmailBody(synth, ev.ContentHash, dargs)
	b.encrypted = true
	ok2 := true
	b.decrypted = &ok2
	if props["pgpSignature"] && keys != nil {
		b.pgpSignature = a.signatureOfDecrypted(ctx, userID, keys, synth, embedded)
	}
	return b
}

// openEncrypted splits, decrypts and rewraps. ok=false for every failure; the
// reason is logged here, never returned. signers are the section 6.2 candidate
// keys (nil: no verdict wanted).
func (a *API) openEncrypted(ctx context.Context, userID, msgID ulid.ULID, raw []byte, purpose string,
	signers [][]byte) (synth []byte, embedded EmbeddedSignature, ok bool) {
	ct, err := pgpmime.SplitEncrypted(raw)
	if err != nil {
		slog.WarnContext(ctx, "encrypted message is not a well-formed RFC 3156 message; showing it as stored",
			"user_id", userID.String(), "message_id", msgID.String(), "err", err)
		return nil, EmbeddedSignature{}, false
	}
	res, err := a.decrypter.Decrypt(ctx, userID, msgID, purpose, ct, signers)
	if err != nil {
		// The crypto process's answer is one opaque code; logging it adds the
		// one distinction that is not cryptographic (rate limit, outage).
		slog.WarnContext(ctx, "could not decrypt a message; showing it as stored",
			"user_id", userID.String(), "message_id", msgID.String(), "purpose", purpose, "err", err)
		return nil, EmbeddedSignature{}, false
	}
	// ⚠ Rewrap copies ONLY the entity's MIME content fields: identity and auth
	// headers hidden inside the ciphertext never reach the header block (see
	// pgpmime.Rewrap). Both Email/get and the part download go through here.
	synth, err = pgpmime.Rewrap(raw, res.Entity)
	if err != nil {
		slog.WarnContext(ctx, "decrypted entity is not a MIME entity; showing the message as stored",
			"user_id", userID.String(), "message_id", msgID.String(), "err", err)
		return nil, EmbeddedSignature{}, false
	}
	return synth, res.Signature, true
}

// signatureOfDecrypted judges the signature of a decrypted message, with the same
// rules as a plaintext signed message. synth is the rewrapped decrypted entity
// (content fields only, as rendered); embedded is the crypto process's verdict on
// a signature inside the encryption. nil when neither is signed.
//
// ⚠ PRECEDENCE (ADR-039 section 6): when the decrypted entity is itself a
// multipart/signed (RFC 3156 section 6.1, signed then encrypted), THAT signature
// is reported and the embedded one (section 6.2) is not. The inner signature
// covers exactly the entity that is rendered, content headers included; an
// embedded one covers the whole plaintext, of which that signed entity is a
// part. One property holds one verdict, and the one about what the reader sees
// is the more specific. Otherwise the embedded verdict is reported. Accepted
// consequence: an inner signature that verifies, wrapped in an embedded one that
// does not, shows the inner verdict (the shown content is exactly what the inner
// signature covers). A malformed inner multipart/signed is `bad`, never a
// fall-through to the embedded verdict.
func (a *API) signatureOfDecrypted(ctx context.Context, userID ulid.ULID, keys *sigKeys, synth []byte, embedded EmbeddedSignature) *pgpSignature {
	if isMultipartSigned(synth) {
		return a.signedPartSignature(ctx, userID, keys, synth)
	}
	if !embedded.Signed {
		return nil
	}
	return a.judgeSignature(ctx, userID, keys, embeddedVerdict(embedded))
}

// serveDecryptedPart serves one part of a decrypted message: decrypt again,
// find the part, scan its plaintext on demand, apply ADR-027's verdict rules,
// and serve or refuse. Nothing is cached — not the plaintext, not the verdict.
//
// ⚠ Cache-Control: no-store on every answer, and no 304: a conditional answer
// would tell a browser to reuse plaintext it may have kept on disk, and this
// server's rule is that decrypted bytes exist only for the request.
func (a *API) serveDecryptedPart(w http.ResponseWriter, r *http.Request, userID ulid.ULID, ref blobRef, messageID ulid.ULID, preview bool) {
	ctx := r.Context()
	if a.decrypter == nil || messageID.Compare(ulid.ULID{}) == 0 {
		blobNotFound(w)
		return
	}
	raw, err := a.blobs.ReadRaw(ctx, userID, ref.hash)
	if err != nil {
		blobNotFound(w)
		return
	}
	synth, _, ok := a.openEncrypted(ctx, userID, messageID, raw, "attachment", nil)
	if !ok {
		decryptUnavailable(w)
		return
	}
	parsed, _ := mime.ParseBytes(synth)
	if parsed == nil || parsed.Structure == nil {
		blobNotFound(w)
		return
	}
	var st *mime.Structure
	for node, path := range mime.PartPaths(parsed.Structure) {
		if path == ref.part && path != "" {
			st = node
		}
	}
	if st == nil {
		blobNotFound(w)
		return
	}

	// ── THE GATE, on the plaintext (ADR-027 §5a, §5b, §6, §8 applied as is). ──
	status, signature := a.decryptedPartVerdict(ctx, userID, messageID, parsed, st, ref.part)
	released := false
	if attachscan.Blocked(status, false) && len(st.Data) > 0 {
		sum := sha256.Sum256(st.Data)
		_ = a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var err error
			released, err = tx.PartReleased(ctx, sum[:])
			return err
		})
	}
	if attachscan.Blocked(status, released) {
		blobQuarantined(w, r, status, signature, preview)
		return
	}

	ct := safeContentType(st.ContentType)
	if preview {
		if !inlinePreviewTypes[ct] {
			previewUnsupported(w)
			return
		}
		writePreviewHeaders(w, ct, downloadName(r, st.Filename), int64(len(st.Data)), partETag(ref))
	} else {
		writeBlobHeaders(w, ct, downloadName(r, st.Filename), int64(len(st.Data)), partETag(ref))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("ETag")
	_, _ = w.Write(st.Data)
}

// decryptedPartVerdict judges one decrypted part by ADR-027's rules, exactly as
// ingest would have judged it had it arrived in plaintext:
//
//   - only ATTACHMENT LEAVES are scanned and gated (mime.IsAttachmentLeaf, the
//     predicate ingest projects with). Body text has no row at ingest and is
//     not gated there; it is not gated here.
//   - a truncated walk forces `unknown` without scanning
//     (attachscan.ForcedUnknown, §5a), and so does a part inside an encrypted
//     SUBTREE of the decrypted entity — a second encryption layer, or a
//     forwarded encrypted message (attachscan.EncryptedPaths, §6, the same
//     subtree-scoped rule ingest applies since #405). Siblings of such a
//     subtree are plaintext and are scanned normally.
//   - zero bytes is `unknown` (attachscan.HasNoBytes).
//   - no scanner configured is `unscanned`, which does not block (§8).
//   - a scanner error is `unknown`, which blocks (§5b): "could not check" and
//     "checked and bad" get the same door.
//
// It also records the verdict (noteOnDemand): one INFO line and one observer
// call per judged attachment, so an on-demand scan is visible without sending
// malware to see it. The second return is clamd's detection name for a flagged
// part, empty otherwise.
func (a *API) decryptedPartVerdict(ctx context.Context, userID, messageID ulid.ULID, parsed *mime.Message, st *mime.Structure, path string) (string, string) {
	if !mime.IsAttachmentLeaf(st.ContentType, st.Disposition) {
		return "", ""
	}
	status, signature := a.judgeDecryptedPart(ctx, userID, parsed, st, path)
	a.noteOnDemand(ctx, messageID, path, status, signature, len(st.Data))
	return status, signature
}

// noteOnDemand records one on-demand verdict. ⚠ NO FILENAME AND NO CONTENT, for
// the reason ingest's logVerdict gives (ADR-027 §10c): the filename is
// attacker-controlled text. message_id plus part_path names the part.
func (a *API) noteOnDemand(ctx context.Context, messageID ulid.ULID, path, status, signature string, size int) {
	attrs := []any{"message_id", messageID.String(), "part_path", path, "verdict", status, "size_bytes", size}
	if status == string(clamd.Flagged) {
		attrs = append(attrs, "signature", signature)
	}
	slog.InfoContext(ctx, "on-demand scan of a decrypted attachment", attrs...)
	if a.onDemandObserve != nil {
		a.onDemandObserve(status)
	}
}

func (a *API) judgeDecryptedPart(ctx context.Context, userID ulid.ULID, parsed *mime.Message, st *mime.Structure, path string) (string, string) {
	if _, forced := attachscan.ForcedUnknown(parsed); forced {
		return string(clamd.Unknown), ""
	}
	if attachscan.EncryptedPaths(parsed)[path] {
		return string(clamd.Unknown), ""
	}
	if attachscan.HasNoBytes(mime.AttachmentPart{Node: st}) {
		return string(clamd.Unknown), ""
	}
	if a.onDemandScan == nil {
		return verdictUnscanned, ""
	}
	v, err := a.onDemandScan.Scan(ctx, st.Data)
	if err != nil {
		slog.WarnContext(ctx, "on-demand scan of a decrypted attachment failed; withholding it",
			"user_id", userID.String(), "err", err)
		return string(clamd.Unknown), ""
	}
	switch v.State {
	case clamd.Clean, clamd.Unknown:
		return string(v.State), ""
	case clamd.Flagged:
		return string(v.State), v.Signature
	}
	return string(clamd.Unknown), ""
}

// decryptUnavailable answers a part request inside an encrypted message that
// cannot be decrypted right now. No detail, by design.
func decryptUnavailable(w http.ResponseWriter) {
	hardenBlobResponse(w)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusUnprocessableEntity,
		newRequestError("notDecryptable", http.StatusUnprocessableEntity,
			"this part is inside an encrypted message that could not be decrypted"))
}
