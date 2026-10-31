// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for LOCALLY AUTHORED mail (spec §5): drafts
// and the messages a submission sends. Two rules shape all of it.
//
//  1. A locally authored message is a canonical message like any other — its
//     identity is SHA-256(raw bytes) (ADR-001). We never mutate its bytes: an
//     edited draft is a NEW message (new bytes, new hash), and the old version
//     is retired explicitly. That is what makes the draft conflict rule work
//     (see ReplaceDraft).
//  2. It has no message_provenance row until its APPEND round-trips (the server
//     assigns the UID), so it lives in app.messages.local_mailbox_id meanwhile.
//     When the appended copy comes back down through sync, content-hash dedup
//     attaches real provenance to the SAME row — one message, not two.

// ErrMessageNotFound is returned when a message id does not resolve for the
// caller.
var ErrMessageNotFound = errors.New("store: message not found")

// ErrDraftSubmitted is returned when a draft-only operation targets a message
// that has been handed to the send path (mailroom#148). It is not a failure so
// much as a routing answer: the message has a Sent copy and an outbound record,
// so it is soft-deleted like any other message rather than retired.
var ErrDraftSubmitted = errors.New("store: draft has been submitted for sending")

// ErrNotADraft is returned when a draft-only operation targets a message that
// is not a draft. Retiring a non-draft message is never a compose-path action.
var ErrNotADraft = errors.New("store: message is not a draft")

// ErrCannotMakeDraft is an update that would turn a message that is not a
// draft into one (keywords/$draft: true). Refused for every caller (#49
// review): see applyEmailUpdate.
var ErrCannotMakeDraft = errors.New("store: a message that is not a draft cannot be made one")

// LocalMessage is a message Mailroom composed itself, ready to be recorded.
// The caller has already written the bytes to the BlobStore (content-addressed
// by ContentHash) and supplies the blob metadata alongside.
type LocalMessage struct {
	ContentHash []byte
	SizePlain   int64
	SizeStored  int64
	DEKID       ulid.ULID

	Subject     string
	Snippet     string
	FromName    string
	FromAddress string
	SentAt      time.Time
	SizeBytes   int64

	MessageIDHint  string
	InReplyTo      string
	ReferenceIDs   []string
	HasAttachments bool
	// Attachments is the projected part list (#393). ⚠ The caller must derive
	// it from the BUILT BYTES, not from the compose request: a request lists
	// what the user asked to attach, the bytes are what was actually produced,
	// and #377 is what happens when those are allowed to differ.
	Attachments []AttachmentRow

	// MailboxID is the local placement (Drafts for a draft, Sent for a
	// just-sent message).
	MailboxID ulid.ULID
	// AccountID owns that mailbox; it is where the APPEND writeback goes.
	AccountID ulid.ULID

	Draft bool
	Seen  bool
}

// CreateLocalMessage records a locally authored message: blob metadata, the
// canonical message row (placed in MailboxID via local_mailbox_id), its thread
// link, and the Email change_log row — all in this UserTx (the invariant). It
// does NOT enqueue the IMAP APPEND; the caller does that explicitly, because a
// draft and a sent copy append with different flags to different mailboxes.
func (tx *UserTx) CreateLocalMessage(ctx context.Context, m LocalMessage) (ulid.ULID, error) {
	if err := tx.InsertBlobMeta(ctx, BlobMeta{
		ContentHash: m.ContentHash, SizePlain: m.SizePlain, SizeStored: m.SizeStored,
		DEKID: m.DEKID, EnvelopeVer: 1,
	}); err != nil {
		return ulid.ULID{}, err
	}

	// A message with these exact bytes may already exist (the user saved the
	// same draft twice, or a sync already pulled our appended copy down). The
	// content hash IS the identity (ADR-001), so return the existing row rather
	// than violating UNIQUE (user_id, content_hash).
	if existing, ok, err := tx.MessageIDByContentHash(ctx, m.ContentHash); err != nil {
		return ulid.ULID{}, err
	} else if ok {
		return existing, nil
	}

	refs := refUnion(m.ReferenceIDs, m.InReplyTo)
	threadID, found, err := tx.FindThreadByHints(ctx, m.MessageIDHint, refs)
	if err != nil {
		return ulid.ULID{}, err
	}
	sentAt := m.SentAt.UTC()
	if !found {
		if threadID, err = tx.CreateThreadForMessage(ctx, m.Subject, sentAt); err != nil {
			return ulid.ULID{}, err
		}
	}

	id := NewULID()
	mailbox := m.MailboxID
	row := MessageRow{
		ID: id, ContentHash: m.ContentHash, ThreadID: threadID,
		Subject: m.Subject, Snippet: m.Snippet,
		FromName: m.FromName, FromAddress: m.FromAddress,
		SentAt: &sentAt, ReceivedAt: sentAt, SizeBytes: m.SizeBytes,
		HasAttachments: m.HasAttachments,
		MessageIDHint:  m.MessageIDHint, InReplyTo: m.InReplyTo,
		ReferenceIDs: nonNilStrings(m.ReferenceIDs),
		Flags: MessageFlags{
			Draft: m.Draft, Seen: m.Seen, Keywords: []string{},
		},
		// Mail we wrote ourselves was, trivially, "addressed to" the address we
		// sent it from: that is what makes a reply in the thread default back to
		// the same alias (spec §5).
		AddressedAddress: m.FromAddress,
		LocalMailboxID:   &mailbox,
	}
	if err := tx.InsertMessage(ctx, row); err != nil {
		return ulid.ULID{}, err
	}
	// The part projection, in the same transaction as the message (#393).
	if err := tx.ReplaceAttachments(ctx, id, m.Attachments); err != nil {
		return ulid.ULID{}, err
	}
	if found {
		if err := tx.AddMessageToThread(ctx, threadID, sentAt); err != nil {
			return ulid.ULID{}, err
		}
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// MessageBytesProjection carries the app.messages columns DERIVED FROM THE
// MESSAGE BYTES — the ones that stop being true the moment those bytes are
// replaced, and so must be recomputed by the caller and written in the same
// statement (#377).
//
// It is a struct rather than a bare bool so a call site says which projection it
// is setting, and so the next byte-derived column added here becomes a compile
// error at every caller rather than a silently stale row.
type MessageBytesProjection struct {
	// HasAttachments is len(mime.Message.Attachments) > 0 over the NEW bytes.
	// The caller parses them; internal/store does not import internal/mime.
	HasAttachments bool
	// Attachments is the projected part list over the NEW bytes (#393).
	//
	// ⚠ IT MUST BE RE-DERIVED, NOT CARRIED OVER. This path exists because PGP
	// signing rewrites a message at Submit — ADR-001's one exception — and that
	// rewrite ADDS A PART. Reusing the pre-signing list is precisely the defect
	// #377 was: a byte-derived fact left behind when the bytes moved. Rows for
	// parts that no longer exist are pruned by ReplaceAttachments.
	Attachments []AttachmentRow
	// ClearSnippet blanks app.messages.snippet. Set when the new bytes are
	// ENCRYPTED (#47, ADR-038): the snippet is the first characters of the
	// plaintext body, derived when the draft was written, and it would otherwise
	// sit beside the ciphertext in every list view and survive in the database in
	// the clear. An inbound encrypted message has no readable snippet either (it
	// is derived from the ciphertext message at ingest), so blank is the same
	// answer ingest would have given for these bytes.
	ClearSnippet bool
}

// ReplaceMessageBytes swaps an existing message's bytes for a new rendering of
// the SAME message, appending the Email `updated` change_log row in this tx. It
// returns the content hashes whose app.blobs row it reclaimed, which the caller
// must delete the FILES for in THIS transaction.
//
// ⚠ THIS IS A DELIBERATE EXCEPTION TO ADR-001, AND THE ONLY ONE. Everywhere
// else the content hash IS the message's identity, so changing the bytes
// produces a NEW message rather than mutating one — which is why editing a draft
// creates a row and retires the old one. This exists for exactly one case:
// signing an outgoing message at submit time (#47 increment 5), where the
// stored message must BE the signed message.
//
// The alternative — record the signed bytes as a new Email and retire the draft
// — was considered and rejected: the client submitted a
// specific Email id and JMAP's onSuccessUpdateEmail expects that id to survive
// into Sent, so minting a new one would make the message the user sent
// disappear and a stranger appear beside it.
//
// What makes the exception safe is that it is a re-RENDERING, not an edit:
// the Message-ID, the headers and the body content are the same message, and
// the only difference is the MIME wrapper the signature requires. Nothing a
// user wrote changes. A caller using this to alter content would be
// laundering an edit past the identity rule, and that is what this comment
// exists to prevent.
//
// The collision check is not ceremony. app.messages is UNIQUE (user_id,
// content_hash), and a violation would abort the WHOLE surrounding transaction
// (SQLSTATE 25P02) rather than returning a tidy error — the savepoint trap this
// project has already paid for once. Failing early with a legible message keeps
// that out of the submit path.
//
// ⚠ EVERY BYTE-DERIVED PROJECTION MOVES WITH THE BYTES, WHICH IS WHY proj EXISTS
// (#377). This used to update content_hash and size_bytes only, leaving
// app.messages.has_attachments at whatever was computed BEFORE signing — and
// signing adds an application/pgp-signature part, which internal/mime classifies
// as an attachment because it is a non-text leaf. So every signed sent message
// was stored claiming it had no attachments while its own bytes said otherwise.
//
// Found by `mailroom-admin attachment-report` on real mail, and the evidence was
// a natural experiment rather than an inference: a signing key was
// bound on a known date, and of the messages in Sent, those dated on or after
// that day disagreed with their own bytes while the older ones did not.
//
// The generalisation is the reason this is a struct and not a bool: content_hash
// and size_bytes were updated because someone remembered them. Anything else
// derived from the bytes has to be listed here, or it silently goes stale the
// next time this is called.
func (tx *UserTx) ReplaceMessageBytes(ctx context.Context, messageID ulid.ULID, meta BlobMeta, proj MessageBytesProjection) (reclaim [][]byte, err error) {
	existing, ok, err := tx.MessageIDByContentHash(ctx, meta.ContentHash)
	if err != nil {
		return nil, err
	}
	if ok && existing != messageID {
		return nil, fmt.Errorf("store: cannot give message %s bytes already held by message %s",
			messageID, existing)
	}
	// The hash we are about to stop holding, read BEFORE the update. It becomes
	// a tombstone below, and without it a stale IMAP copy of these bytes is
	// indistinguishable from a new message (#260).
	var oldHash []byte
	if err := tx.tx.QueryRow(ctx,
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), messageID.String()).Scan(&oldHash); err != nil {
		return nil, fmt.Errorf("store: read superseded content hash: %w", err)
	}

	if err := tx.InsertBlobMeta(ctx, meta); err != nil {
		return nil, err
	}
	ct, err := tx.tx.Exec(ctx, `
		UPDATE app.messages
		   SET content_hash = $3, size_bytes = $4, has_attachments = $5,
		       snippet = CASE WHEN $6 THEN '' ELSE snippet END
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), messageID.String(), meta.ContentHash, meta.SizePlain,
		proj.HasAttachments, proj.ClearSnippet)
	if err != nil {
		return nil, fmt.Errorf("store: replace message bytes: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// Not found, or not this user's. Both are a caller bug, and silently
		// succeeding would leave the submission pointing at unsigned bytes while
		// every log line said the message had been signed.
		return nil, fmt.Errorf("store: message %s not found", messageID)
	}
	// ⚠ THE PART PROJECTION MOVES WITH THE BYTES (#393), for the same reason
	// has_attachments does (#377): signing adds a part, so the pre-signing rows
	// describe a message that no longer exists. ReplaceAttachments prunes paths
	// that are gone and leaves scan_status untouched on the ones that survive.
	if err := tx.ReplaceAttachments(ctx, messageID, proj.Attachments); err != nil {
		return nil, err
	}
	// ⚠ THE TOMBSTONE, IN THE SAME TRANSACTION AS THE REPLACEMENT (#260).
	//
	// Doing it separately would reopen the race it exists to close: a sync could
	// read the stale IMAP copy in the window between the bytes moving and the
	// tombstone landing, which is precisely the seven-second gap that produced a
	// stray unsigned draft on the first real signed send.
	//
	// ON CONFLICT DO NOTHING because a message can be replaced more than once in
	// principle, and re-recording a hash we already know about is not an error —
	// whereas a unique violation here would abort the whole submit transaction
	// (SQLSTATE 25P02), the savepoint trap this project has paid for before.
	if len(oldHash) > 0 && !bytes.Equal(oldHash, meta.ContentHash) {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO app.superseded_content (user_id, content_hash, message_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (user_id, content_hash) DO NOTHING`,
			tx.userID.String(), oldHash, messageID.String()); err != nil {
			return nil, fmt.Errorf("store: record superseded content: %w", err)
		}

		// RECLAIM THE BYTES WE STOPPED HOLDING (#300). Two of the four orphan
		// app.blobs rows the audit counted live were made here: signing writes the
		// signed blob, points the message at it, and abandoned the unsigned one — the
		// row AND the file — with nothing anywhere that could ever find them again.
		//
		// ⚠ THE SUPERSEDED_CONTENT ROW IS NOT A REFERENT AND MUST NOT BE ONE. It maps
		// hash -> message so ingest can recognise a stale IMAP copy of the old bytes
		// (#260) and so RetireSentDraftCopy can cancel an APPEND that names them
		// (#268). Both want the HASH; neither ever reads the bytes. Treating it as a
		// referent would mean these blobs could never be reclaimed at all.
		//
		// ⚠ WHAT IS A REFERENT is a not-yet-applied writeback intent — see
		// hashesWithLiveWriteback for why that guard is load-bearing rather than
		// decorative on this path specifically. When it holds, we keep the row: the
		// leak we are fixing is cheaper than dead-lettering every signed send.
		held, herr := tx.hashesWithLiveWriteback(ctx, [][]byte{oldHash})
		if herr != nil {
			return nil, herr
		}
		if !held[hex.EncodeToString(oldHash)] {
			if reclaim, err = tx.reclaimUnreferencedBlobs(ctx, [][]byte{oldHash}); err != nil {
				return nil, err
			}
		}
	}

	if _, err := tx.AppendChange(ctx, ObjectEmail, messageID, OpUpdated); err != nil {
		return nil, err
	}
	return reclaim, nil
}

// MessageIDBySupersededContent returns the message whose bytes USED to hash to
// contentHash, if any (#260).
//
// This is a CERTAIN answer, not an inference: the row exists only because this
// server replaced those exact bytes itself and recorded doing so. That is why
// ingest asks it before the dedup-hash question, which is a strong inference
// but still an inference.
func (tx *UserTx) MessageIDBySupersededContent(ctx context.Context, contentHash []byte) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx,
		`SELECT message_id FROM app.superseded_content
		  WHERE user_id = $1 AND content_hash = $2`,
		tx.userID.String(), contentHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: superseded content lookup: %w", err)
	}
	parsed, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: superseded content: bad message id %q: %w", id, err)
	}
	return parsed, true, nil
}

// nonNilStrings normalizes a nil slice to an empty one for a NOT NULL text[].
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// refUnion returns the threading hints of a composed message: its References
// plus its In-Reply-To, de-duplicated and order-preserving, empties dropped.
func refUnion(refs []string, inReplyTo string) []string {
	seen := make(map[string]bool, len(refs)+1)
	out := make([]string, 0, len(refs)+1)
	for _, s := range append(append([]string{}, refs...), inReplyTo) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// RetireDraft removes a superseded draft version: it deletes the canonical row
// (provenance cascades) and appends the Email `destroyed` change_log row, and —
// when the version was already mirrored to the IMAP Drafts folder — enqueues the
// EXPUNGE of exactly that copy. It returns the content hashes whose app.blobs row
// it reclaimed, which the caller must delete the FILES for in THIS transaction.
//
// ⚠ THE RECLAIM IS NEW IN #300 AND THIS PATH LEAKED WITHOUT IT. Every autosave
// retires a revision, so this is the highest-frequency `DELETE FROM app.messages`
// in the system, and it left the app.blobs row of every superseded draft behind
// forever — counted live: four orphan rows, and this path is one of the three
// producers. The row is not inert litter: it carries the dek_id the ciphertext is
// opened with and its size_stored is summed into the user's storage quota, so a
// leaked row bills the user for a draft they replaced.
//
// THE DRAFT CONFLICT RULE (spec §5) lives here. Mailroom expunges only the
// (mailbox, uidvalidity, uid) coordinate of the version it is retiring. If
// another client edited the draft concurrently, its edit is a DIFFERENT message
// (different bytes -> different content hash -> its own row and its own UID), so
// it is never the thing we expunge: it survives as a second draft. The losing
// version is preserved, never destroyed — duplication over data loss. And if our
// own copy has moved or the mailbox's UIDVALIDITY rolled, the writeback engine
// detects the divergence and DEAD-LETTERS the expunge with a surfaced reason
// (spec §4a) instead of blind-firing at whatever now holds that UID.
func (tx *UserTx) RetireDraft(ctx context.Context, id ulid.ULID) (reclaim [][]byte, err error) {
	var (
		isDraft     bool
		contentHash []byte
		messageID   string
	)
	if err := tx.tx.QueryRow(ctx, `
		SELECT is_draft, content_hash, COALESCE(message_id_hint, '')
		  FROM app.messages WHERE user_id = $1 AND id = $2 FOR UPDATE`,
		tx.userID.String(), id.String()).Scan(&isDraft, &contentHash, &messageID); err != nil {
		if isNoRows(err) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("store: retire draft (read): %w", err)
	}
	if !isDraft {
		return nil, ErrNotADraft
	}
	// ONLY A DRAFT THAT LIVES IN A DRAFTS FOLDER IS RETIRED (#49 review).
	// Retiring deletes the row and its blobs and expunges every IMAP copy, so
	// is_draft alone is not enough to authorise it: the flag is a mirror of
	// IMAP's \Draft and could be set on mail anywhere. Mailroom's own drafts are
	// placed in the account's Drafts mailbox (SaveDraft's local_mailbox_id, then
	// the APPENDed provenance), and another client's drafts sync from that same
	// folder, so "is a draft AND is effectively in a Drafts-role mailbox" is
	// exactly the set this operation was designed for. Anything else is not a
	// draft here, and Email/set destroy then moves it to Trash (ADR-009).
	inDrafts, err := tx.inDraftsMailbox(ctx, id)
	if err != nil {
		return nil, err
	}
	if !inDrafts {
		return nil, ErrNotADraft
	}

	// A SUBMITTED message is never retired, however draft-shaped it looks
	// (mailroom#148). Two different disasters are being prevented here:
	//
	//   - the EXPUNGE below fires at every live copy, and a submitted message has
	//     one in SENT. Retiring it would delete the user's record of a message
	//     they actually sent, off the server, to make a Drafts entry go away;
	//   - the DELETE below is refused outright by app.submissions' foreign key,
	//     which is what surfaced this: a raw SQLSTATE 23503 in the client.
	//
	// It is reachable two ways: a message left draft-flagged by the bug in #146,
	// and — still, today — a draft deleted inside its Undo Send window, which has
	// a submission row from the moment the send is scheduled. The caller decides
	// what to do instead; Email/set destroy trashes it (soft-delete, ADR-009).
	submitted, _, err := tx.MessageSubmissionStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	if submitted {
		return nil, ErrDraftSubmitted
	}

	// WHERE THIS VERSION'S BYTES WERE WRITTEN, read BEFORE anything is cancelled
	// or deleted — the append intents are about to be destroyed twice over, by
	// cancelPendingAppend below and then by this message's ON DELETE CASCADE.
	appendTargets, err := tx.draftAppendTargets(ctx, contentHash)
	if err != nil {
		return nil, err
	}

	// ⚠ AN INFLIGHT APPEND HAS BEEN CLAIMED, NOT NECESSARILY PERFORMED (#691).
	// ApplyPending claims its whole batch before applying any of it, and the
	// engine reads an APPEND's bytes from the blob store only when it reaches that
	// intent. Such an APPEND is about to put a copy on the server — which is why
	// it counts as CopyExists below and earns an expunge — so its bytes must still
	// be there when it gets to them. See the reclaim at the end of this function.
	appendInFlight := false
	for _, a := range appendTargets {
		if a.InFlight {
			appendInFlight = true
		}
	}

	// The version may not have reached the server yet (its APPEND is still
	// queued). Dropping that pending append here saves a pointless round trip
	// in which another client would briefly see the stale version appear and
	// then vanish.
	if err := tx.cancelPendingAppend(ctx, contentHash); err != nil {
		return nil, err
	}

	// Expunge every live copy of THIS version, per account it lives in.
	//
	// The intent deliberately carries NO message_id, and instead names the
	// target by (mailbox, Message-ID header). That is not a shortcut, it is
	// forced: app.writeback_intents.message_id is ON DELETE CASCADE, so an
	// intent that referenced this message would be deleted by the very DELETE
	// below — the expunge would silently disappear and the superseded draft
	// would live on the server forever. Naming the message by its Message-ID
	// header also makes the expunge SAFE: the engine SEARCHes for it in the
	// mailbox instead of firing at a UID that a UIDVALIDITY roll may since have
	// reassigned to somebody else's mail.
	targets, err := tx.liveDraftMailboxTargets(ctx, id)
	if err != nil {
		return nil, err
	}

	// ⚠ PROVENANCE IS NOT THE WHOLE ANSWER, AND ASSUMING IT WAS IS #603.
	//
	// The list above is copies SYNC HAS SEEN. A copy whose APPEND has already
	// applied but which no sync pass has observed yet is equally real and equally
	// in need of removing — and that window was measured at 22-41 s against a
	// 15 s autosave interval, so it is the common case rather than a rare race.
	// Missing one leaves the copy on the provider with nothing queued to remove
	// it, and the next sync ingests it as a brand-new draft.
	//
	// Adding a target the provider does not actually hold is the opposite and
	// much cheaper error: the engine SEARCHes by Message-ID, and finding nothing
	// is a transient failure that retries and then dead-letters with a reason the
	// operator can read (writeback.applyHeaderExpunge). Visible and wrong beats
	// silent and wrong. So targets whose append never ran are still excluded —
	// that is what CopyExists is for — but the benefit of the doubt goes to
	// removing.
	seen := map[string]bool{}
	for _, t := range targets {
		seen[t.AccountID.String()+"\x00"+t.IMAPName] = true
	}
	for _, a := range appendTargets {
		if !a.CopyExists || a.IMAPName == "" {
			continue
		}
		if key := a.AccountID.String() + "\x00" + a.IMAPName; !seen[key] {
			seen[key] = true
			targets = append(targets, imapTarget{AccountID: a.AccountID, IMAPName: a.IMAPName})
		}
	}

	for _, t := range targets {
		if messageID == "" {
			// No Message-ID to search for: we cannot name the copy safely, so we
			// refuse to guess. The local version goes; the server copy stays and
			// is reconciled as an ordinary draft (duplication over data loss).
			break
		}
		payload, err := json.Marshal(expungePayload{
			Mailbox: t.IMAPName, MessageIDHeader: messageID,
		})
		if err != nil {
			return nil, fmt.Errorf("store: marshal expunge intent: %w", err)
		}
		if _, err := tx.EnqueueIntent(ctx, IntentRow{
			AccountID:      t.AccountID,
			Op:             IntentExpunge,
			Payload:        payload,
			IdempotencyKey: fmt.Sprintf("expunge:%s:%s:%s", t.AccountID, t.IMAPName, messageID),
			// ⚠ WITHOUT THIS, DELETING A LEFTOVER DRAFT INSIDE MAILROOM DOES
			// NOTHING (#268). Every target here is a copy sync has actually seen,
			// so there IS something on the server to remove — but an expunge for
			// this same (account, mailbox, Message-ID) may already have run and
			// finished, and its idempotency key would swallow this one silently.
			// The user would then watch the message reappear at the next sync,
			// delete it again, and watch it reappear again. See IntentRow.Revive.
			Revive: true,
			Source: IntentByDraft,
		}); err != nil {
			return nil, err
		}
	}

	if _, err := tx.tx.Exec(ctx,
		`DELETE FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()); err != nil {
		return nil, fmt.Errorf("store: retire draft: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpDestroyed); err != nil {
		return nil, err
	}

	// RECLAIM (#300). The row is gone, so nothing references these bytes unless an
	// attachment or a model artifact does — which the shared guard checks. Only the
	// CURRENT hash: a draft can in principle carry superseded hashes too, but the
	// only writer of those is signing at SUBMIT time, and a submitted message was
	// already refused above, so that set is empty on every reachable path. Leaving
	// a hypothetical superseded blob behind is a leak; reclaiming bytes a queued
	// APPEND still names would be data loss. Leak, every time.
	//
	// The pending APPEND of exactly these bytes was cancelled above. An INFLIGHT
	// one is the exception, and this comment used to say the opposite — that it
	// "has reached the server and is being expunged rather than re-read" (#691).
	// It has been CLAIMED; whether the engine has read its body yet is a race.
	// Reclaiming here can lose that race: the APPEND dies on blob.ErrNotFound,
	// nothing reaches the server, and the expunge queued above dead-letters "no
	// copy found" after every retry — the trace in #691. So the bytes stay, and the
	// cost is the one this file already prefers: a blob row nothing references
	// once the APPEND finishes (counted by `mailroom-admin blob-report`). The
	// intent row cannot carry the guard — the DELETE above has cascaded it away —
	// which is why the state was read before anything was deleted.
	if appendInFlight {
		return nil, nil
	}
	reclaim, err = tx.reclaimUnreferencedBlobs(ctx, [][]byte{contentHash})
	if err != nil {
		return nil, err
	}

	// ⚠ NO Thread ROW, and it is a considered omission rather than the oversight
	// #299 found in purge. A draft revision is retired by the same Email/set that
	// created its replacement, and compose mints a FRESH Message-ID per revision
	// (internal/compose/build.go) — so the revision being retired is the only
	// message in its own thread, and that thread is created and abandoned inside
	// one client interaction. Emitting Thread `updated` for a conversation the
	// client has never seen would be noise. If KeepMessageID ever reaches the draft
	// path (#311 item 4), revisions start SHARING a thread and this stops being
	// true — which is one more reason that wiring needs the test #311 asks for.
	return reclaim, nil
}

// RetireSentDraftCopy removes the DRAFT copy of a message that has just been
// sent: it enqueues the EXPUNGE of that copy from each account's Drafts folder,
// drops the local provenance rows that put the message there, and cancels a
// Drafts APPEND that has not reached the server yet. It reports how many copies
// it targeted AND how many local provenance rows it dropped.
//
// ⚠ THE SECOND COUNT EXISTS BECAUSE THE FIRST ONE CANNOT ANSWER "DID JMAP-VISIBLE
// STATE CHANGE?" (#311). Dropping a provenance row changes `Email.mailboxIds` —
// membership is derived from live provenance — and this method deliberately
// appends no change_log row of its own (see the drop block below). That is sound
// for the SEND path, which has just called MarkMessageSent in the same
// transaction, and it is NOT sound for any other caller. Rather than leave the
// next caller to notice, the count is reported so it can be checked; see
// RepairSentDraft, which is the caller that got this wrong.
//
// WITHOUT THIS, A SENT MESSAGE STAYS A DRAFT (mailroom#146). Drafts are mirrored
// to IMAP (spec §5), so a sent message keeps a live copy in Drafts carrying
// \Draft. Two things then go wrong, and both are the system working correctly on
// a premise nobody cleaned up:
//
//   - membership is derived from live provenance, so the message shows in Drafts
//     as well as Sent;
//   - the next sync reconciles flags from the server, and \Draft on that copy
//     overwrites the is_draft = false MarkMessageSent just wrote. Remote-wins for
//     IMAP-representable state is right (spec §6); the remote state was wrong.
//
// IT MUST NOT DEPEND ON HAVING SEEN THE COPY, which is what the first version of
// this got wrong (mailroom#150). It looked for a live provenance row naming a
// Drafts mailbox and did nothing when it found none — but at the moment a send
// completes there usually IS none. The timeline of a real send:
//
//	21:09:33  draft saved      -> Drafts APPEND enqueued
//	21:09:59  send completes   -> nothing to expunge: the APPEND has not run,
//	                              so no copy exists yet and no provenance does
//	21:11:31  writeback runs   -> Drafts APPEND applied. The copy appears.
//	                              Sync then discovers it, and the message is
//	                              back in Drafts, re-tagged \Draft.
//
// So the cleanup addresses the DRAFTS MAILBOX ITSELF, resolved by role, and
// covers both states that copy can be in:
//
//   - QUEUED — its APPEND has not run. The intent is dropped, so the copy is
//     never created. This is the common case for a normal send.
//   - ALREADY THERE — appended, whether or not sync has observed it. An expunge
//     is enqueued naming (mailbox, Message-ID header); the engine SEARCHes the
//     folder when it applies, so it finds the copy without us knowing its UID,
//     and a search that matches nothing is a no-op ("already gone"). That is the
//     same reasoning the Sent-append dedup already uses for the same race.
//
// Any live Drafts provenance we DO know about is dropped too, so the message
// leaves the Drafts view at once rather than at the next sync.
//
// The expunge names its target the way RetireDraft does, for the same reasons:
// by (mailbox, Message-ID header) so it cannot fire at a UID a UIDVALIDITY roll
// reassigned to somebody else's mail, and with NO message_id, which is what
// selects that search path in the engine.
//
// Only DRAFTS-role mailboxes are touched. The Sent copy is never a target, and
// the message row survives — it IS the sent message. With no Message-ID header
// no expunge is queued (we cannot name the copy safely), though a queued APPEND
// is still cancelled: refusing to guess costs us nothing there.
func (tx *UserTx) RetireSentDraftCopy(ctx context.Context, id, accountID ulid.ULID) (queued, provenanceDropped int, err error) {
	var (
		contentHash []byte
		messageID   string
	)
	if err := tx.tx.QueryRow(ctx, `
		SELECT content_hash, COALESCE(message_id_hint, '')
		  FROM app.messages WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()).Scan(&contentHash, &messageID); err != nil {
		if isNoRows(err) {
			return 0, 0, ErrMessageNotFound
		}
		return 0, 0, fmt.Errorf("store: retire sent draft copy (read): %w", err)
	}

	// ⚠ EVERY hash this message has ever had, not just its current one (#268).
	//
	// The queued Drafts APPEND names the bytes it will write, and for a SIGNED
	// send those are not the bytes the message holds now: signing happens at
	// SUBMIT and rewrites the message in place (ReplaceMessageBytes), so by the
	// time the send completes the row's content_hash is the SIGNED one while the
	// APPEND enqueued at draft-save time still names the UNSIGNED one.
	//
	// Matching only the current hash therefore cancelled nothing on exactly the
	// path that matters. That is how the leftover in #268 came to exist at all:
	// the cancel missed, the APPEND ran seven seconds later and wrote an unsigned
	// draft to the server, and the expunge that was supposed to clean it up
	// reported success without removing it. app.superseded_content already
	// records the old hashes for ingest's benefit (#260); this is the second
	// consumer of the same fact.
	hashes, err := tx.heldContentHashes(ctx, id, contentHash)
	if err != nil {
		return 0, 0, err
	}

	// Every Drafts mailbox this message could have a copy in: the sending
	// account's, by ROLE — which needs no copy to exist yet — plus any Drafts
	// mailbox a live provenance row already names, which covers a message that
	// was drafted under a different account.
	type draftTarget struct {
		accountID ulid.ULID
		imapName  string
	}
	seen := map[string]bool{}
	// hasLiveCopy marks the targets where a copy is KNOWN to be on the server
	// already, because sync has seen it and recorded provenance. It decides
	// whether an expunge that finds nothing is a no-op or a failure (#268).
	hasLiveCopy := map[string]bool{}
	targetKey := func(acct ulid.ULID, imapName string) string {
		return acct.String() + "\x00" + imapName
	}
	var targets []draftTarget
	add := func(acct ulid.ULID, imapName string) {
		key := targetKey(acct, imapName)
		if imapName == "" || seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, draftTarget{accountID: acct, imapName: imapName})
	}

	if accountID != (ulid.ULID{}) {
		drafts, found, err := tx.MailboxForRole(ctx, accountID, "drafts")
		if err != nil {
			return 0, 0, err
		}
		if found {
			add(accountID, drafts.IMAPName)
		}
	}

	rows, err := tx.tx.Query(ctx, `
		SELECT p.id, p.account_id, p.mailbox_id, mb.imap_name
		  FROM app.message_provenance p
		  JOIN app.mailboxes mb ON mb.id = p.mailbox_id AND mb.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'`,
		tx.userID.String(), id.String())
	if err != nil {
		return 0, 0, fmt.Errorf("store: retire sent draft copy (locations): %w", err)
	}
	type liveCopy struct {
		provenanceID string
		accountID    ulid.ULID
		mailboxID    ulid.ULID
		imapName     string
	}
	var live []liveCopy
	for rows.Next() {
		var c liveCopy
		var acct, mailbox string
		if err := rows.Scan(&c.provenanceID, &acct, &mailbox, &c.imapName); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan draft copy: %w", err)
		}
		if c.accountID, err = ulid.Parse(acct); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: parse account id %q: %w", acct, err)
		}
		if c.mailboxID, err = ulid.Parse(mailbox); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: parse mailbox id %q: %w", mailbox, err)
		}
		live = append(live, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("store: retire sent draft copy (locations): %w", err)
	}
	rows.Close()

	// The role is resolved through MailboxRole rather than by reading special_use
	// directly, so this agrees with every other role decision in the system —
	// including the ones that derive a role from a RESERVED NAME rather than from
	// SPECIAL-USE (mailroom#117).
	var drop []string
	for _, c := range live {
		role, _, err := tx.MailboxRole(ctx, c.mailboxID)
		if err != nil {
			return 0, 0, err
		}
		if role != "drafts" {
			continue
		}
		add(c.accountID, c.imapName)
		hasLiveCopy[targetKey(c.accountID, c.imapName)] = true
		drop = append(drop, c.provenanceID)
	}
	if len(targets) == 0 {
		return 0, 0, nil
	}

	for _, t := range targets {
		// A Drafts APPEND that has not run yet is dropped, so the copy is never
		// created — the common case, since a send usually beats the writeback pass
		// that would have filed the draft. This is safe with or without a
		// Message-ID: cancelling our own queued write requires no guessing about
		// what is on the server.
		//
		// SCOPED TO THE MAILBOX, unlike cancelPendingAppend: the Sent copy names
		// the SAME content hash (it is the same bytes), so a hash-only match here
		// would cancel the Sent APPEND this very send just queued and the message
		// would never be filed anywhere.
		cancelled, err := tx.cancelPendingAppendIn(ctx, hashes, t.imapName)
		if err != nil {
			return 0, 0, err
		}

		// ⚠ NOTHING WAS CREATED, SO THERE IS NOTHING TO REMOVE (#268).
		//
		// We just deleted the APPEND that would have put a copy in this mailbox,
		// and sync has never seen one there. Queueing an expunge anyway used to be
		// harmless because a search that matched nothing counted as success — but
		// that is precisely the behaviour that let a REAL leftover go unnoticed, so
		// an empty search is now a failure. Keeping this intent would therefore
		// dead-letter a spurious "could not remove the draft" on every ordinary
		// send. The honest thing is not to ask the server to remove a message we
		// stopped ourselves from writing.
		if cancelled && !hasLiveCopy[targetKey(t.accountID, t.imapName)] {
			continue
		}

		if messageID == "" {
			continue
		}
		payload, err := json.Marshal(expungePayload{
			Mailbox: t.imapName, MessageIDHeader: messageID,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("store: marshal expunge intent: %w", err)
		}
		if _, err := tx.EnqueueIntent(ctx, IntentRow{
			AccountID:      t.accountID,
			Op:             IntentExpunge,
			Payload:        payload,
			IdempotencyKey: fmt.Sprintf("expunge:%s:%s:%s", t.accountID, t.imapName, messageID),
			// An expunge that already ran must not silence this one — see
			// IntentRow.Revive (#268).
			Revive: true,
			Source: IntentByDraft,
		}); err != nil {
			return 0, 0, err
		}
		queued++
	}

	// Drop the local locations too, so the message leaves the Drafts view at once
	// instead of at whatever sync pass follows the expunge. If the expunge later
	// dead-letters, the next sync re-discovers that copy and puts the row back —
	// the honest answer, because the message would genuinely still be in Drafts.
	//
	// ONLY when an expunge was actually queued. With no Message-ID we cannot name
	// the copy, so it stays on the server — and hiding a copy we did not remove
	// would be a lie the next sync would have to correct.
	//
	// NO change_log row is appended here: on the SEND path this runs inside the
	// same transaction as MarkMessageSent, whose row already says this Email
	// changed. One mutation, one row.
	//
	// ⚠ THAT REASONING IS THE CALLER'S, NOT THIS METHOD'S (#311). It is true of
	// internal/compose's send and false of `mailroom-admin repair-sent-drafts`,
	// which paired this with ClearDraftFlag — and ClearDraftFlag appends only when
	// the flag was still set, so a SECOND repair run dropped live Drafts provenance
	// (a `mailboxIds` change) with zero Email rows in the transaction. Hence
	// provenanceDropped, and hence RepairSentDraft, which owns that pairing.
	if queued > 0 {
		for _, provenanceID := range drop {
			if _, err := tx.tx.Exec(ctx,
				`DELETE FROM app.message_provenance WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), provenanceID); err != nil {
				return 0, 0, fmt.Errorf("store: drop drafts provenance: %w", err)
			}
			provenanceDropped++
		}
	}
	return queued, provenanceDropped, nil
}

// RepairSentDraft is the whole of one message's `mailroom-admin
// repair-sent-drafts` repair, in one place, so the change_log invariant cannot
// depend on which two methods the CLI happens to call in which order (#311).
//
// It enqueues the Drafts expunge and drops the stale local provenance
// (RetireSentDraftCopy), clears the leftover $draft flag (ClearDraftFlag), and
// guarantees EXACTLY ONE Email `updated` row when either of those changed
// anything.
//
// ⚠ THE BUG THIS REPLACES NEEDED TWO RUNS TO APPEAR, which is why it survived
// review. Run 1 clears the flag (ClearDraftFlag appends the row) and queues an
// expunge; if that expunge dead-letters, the next sync re-discovers the Drafts
// copy and restores its provenance — documented, intended behaviour. Run 2 then
// finds the message stranded again, drops that restored provenance — changing
// `Email.mailboxIds`, because membership is derived from live provenance — and
// ClearDraftFlag no-ops because is_draft is already false. Net effect of run 2:
// a JMAP-visible mutation with no change_log row at all, so every client keeps
// showing the message in Drafts and /changes never says otherwise.
//
// Reported separately (queued, cleared) because the CLI's summary distinguishes
// them and an operator reading "expunges_queued=3 draft_flags_cleared=0" is
// looking at exactly the second-run case above.
func (tx *UserTx) RepairSentDraft(ctx context.Context, messageID, accountID ulid.ULID) (queued int, cleared bool, err error) {
	queued, provenanceDropped, err := tx.RetireSentDraftCopy(ctx, messageID, accountID)
	if err != nil {
		return 0, false, err
	}
	cleared, err = tx.ClearDraftFlag(ctx, messageID)
	if err != nil {
		return 0, false, err
	}
	// ClearDraftFlag already appended one when it flipped the flag; a second row
	// for the same object in the same transaction would be noise, not safety.
	if provenanceDropped > 0 && !cleared {
		if _, err := tx.AppendChange(ctx, ObjectEmail, messageID, OpUpdated); err != nil {
			return 0, false, err
		}
	}
	return queued, cleared, nil
}

// expungePayload is the JSONB payload of an `expunge` writeback intent that
// names its target by header rather than by message row (see RetireDraft). It
// mirrors writeback.intentPayload's field names.
type expungePayload struct {
	Mailbox         string `json:"mailbox"`
	MessageIDHeader string `json:"message_id_header"`
}

// DraftsCopyAwaitingExpunge reports whether a copy of messageIDHeader found in
// mailboxID is one this server has ALREADY decided to remove: a draft-sourced,
// header-addressed EXPUNGE naming that Message-ID in that very mailbox is still
// queued or in flight (#686). Ingest asks it before treating such a copy as mail.
//
// ⚠ THE WINDOW IT CLOSES IS ONE SYNC PASS WIDE, AND IT IS THE COMMON CASE. A
// revision's APPEND reaches the server; the next autosave retires it and queues
// this expunge (RetireDraft); and a sync pass whose writeback phase ran just
// BEFORE the retire then reads Drafts. The copy's bytes match nothing — the row
// was deleted a second earlier — so ingest created a brand-new draft, in a new
// thread, that nothing tied to the expunge. The expunge then removed the server
// copy and left the local row in Drafts until the hourly reaper soft-deleted it.
// RetireSentDraftCopy opens the same window on the send path, where the copy's
// bytes DO match the sent message, so content-hash dedup re-attached it to Drafts
// and merged \Draft back onto a message the user had just sent (#146's symptom).
//
// IT ONLY EVER DEFERS. Nothing is deleted here or because of this; the caller
// simply does not record the copy yet. Its scope is deliberately the expunge's
// own: same account, same mailbox, same Message-ID — so it cannot reach a copy
// the expunge would not itself remove — and only while that expunge is live:
//
//   - 'pending' / 'inflight': it will run (or is running). Recording the copy
//     now would create a row the expunge is about to orphan.
//   - 'applied': the SEARCH has run and removed what it found. A copy seen after
//     that is new information, and is ingested.
//   - 'dead': the provider kept the copy through every retry. It is real mail on
//     the server, and refusing it forever would be a permanent #93 gap that also
//     stops the mailbox from ever being reaped. Duplication over data loss.
//   - 'suppressed': ingest-only mode; nothing will ever run it without an
//     operator's --apply, so the same reasoning as 'dead'.
//
// message_id IS NULL selects the header-addressed form (RetireDraft and
// RetireSentDraftCopy carry no message row; a purge EXPUNGE always does), and
// source = 'draft' names the actor, so no other subsystem's expunge can widen it.
func (tx *UserTx) DraftsCopyAwaitingExpunge(ctx context.Context, mailboxID ulid.ULID, messageIDHeader string) (bool, error) {
	if messageIDHeader == "" {
		return false, nil
	}
	var awaiting bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		    FROM app.writeback_intents wi
		    JOIN app.mailboxes mb
		      ON mb.user_id = wi.user_id AND mb.account_id = wi.account_id
		     AND mb.imap_name = wi.payload->>'mailbox'
		   WHERE wi.user_id = $1 AND mb.id = $2
		     AND wi.op = 'expunge' AND wi.source = 'draft' AND wi.message_id IS NULL
		     AND wi.state IN ('pending', 'inflight')
		     AND wi.payload->>'message_id_header' = $3)`,
		tx.userID.String(), mailboxID.String(), messageIDHeader).Scan(&awaiting); err != nil {
		return false, fmt.Errorf("store: drafts copy awaiting expunge: %w", err)
	}
	return awaiting, nil
}

// imapTarget is one live physical location of a message, in the terms the
// writeback engine addresses the server with.
type imapTarget struct {
	AccountID ulid.ULID
	IMAPName  string
}

// liveDraftMailboxTargets returns every live physical location of a message
// THAT IS IN A DRAFTS MAILBOX. The role filter is the whole point of the
// function and the reason it is not called liveIMAPTargets any more (#522).
//
// ⚠ IT FEEDS AN EXPUNGE THAT BYPASSES TRASH, so its breadth is a blast radius.
// RetireDraft hard-deletes the local row and expunges every target this returns;
// there is no tombstone and no undo. Until this filter existed the query matched
// on `state = 'live'` alone, so it returned EVERY live copy in EVERY mailbox.
//
// That mattered because `is_draft` is not a Mailroom-only property: it is merged
// from the server's \Draft flag at ingest. So any message the provider had
// flagged \Draft — anywhere, including mail imported from a decades-old archive
// that passed through a dozen clients — was destroyable in one step, in every
// folder at once. The #148 guard covers only the SUBMITTED case, which is a
// different hazard.
//
// The restriction follows the preference this file already states elsewhere:
// DUPLICATION OVER DATA LOSS (see the `messageID == ""` break in RetireDraft).
// A provider that advertises no \Drafts leaves a copy behind to be reconciled as
// an ordinary draft, which is the recoverable outcome; expunging on a guess is
// not.
//
// ⚠ CHECKED BEFORE IT WAS CHANGED, on a real instance: in practice NO
// messages had `is_draft` set, and the account had no Drafts mailbox at
// all (INBOX, \Archive, \Junk, \Sent). Positive control: the same three-table
// join without the is_draft filter returned rows, so the zero was the corpus
// and not a broken query. The hazard is entirely LATENT here — which is also why
// the test for it has to construct the situation synthetically.
func (tx *UserTx) liveDraftMailboxTargets(ctx context.Context, messageID ulid.ULID) ([]imapTarget, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT p.account_id, m.imap_name
		  FROM app.message_provenance p
		  JOIN app.mailboxes m ON m.id = p.mailbox_id AND m.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'
		   AND m.special_use = '\Drafts'`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return nil, fmt.Errorf("store: live imap targets: %w", err)
	}
	defer rows.Close()
	var out []imapTarget
	for rows.Next() {
		var t imapTarget
		var acct string
		if err := rows.Scan(&acct, &t.IMAPName); err != nil {
			return nil, fmt.Errorf("store: scan imap target: %w", err)
		}
		if t.AccountID, err = ulid.Parse(acct); err != nil {
			return nil, fmt.Errorf("store: parse account id %q: %w", acct, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// cancelPendingAppend drops any not-yet-applied APPEND of these exact bytes.
// INFLIGHT is deliberately excluded: one already in flight has (or may have)
// reached the server, and the expunge is what cleans that up.
//
// 'suppressed' is included so the predicate means what it says — a not-yet-
// applied APPEND — rather than listing the two states that existed when it was
// written. Deleting such a row is not an exception to "suppressed is terminal":
// it describes an APPEND of bytes the user retracted with their own next
// keystroke, which cannot be performed by anyone ever, so keeping it preserves
// a guaranteed failure rather than evidence of a change worth replaying.
//
// ⚠ ON THIS PATH THAT WIDENING IS BELT-AND-BRACES, NOT THE FIX, and saying so
// matters because #557 named it as the fix. cancelPendingAppend's only caller
// is RetireDraft, which deletes the message row in the same transaction — and
// app.writeback_intents.message_id is ON DELETE CASCADE, so an APPEND here is
// removed by the cascade whatever state it is in. A probe written against this
// path passed green against unfixed code, which is how the real mechanism was
// found: it is hashesWithLiveWriteback (blobreclaim.go), whose own predicate
// omitted 'suppressed' and therefore reclaimed bytes a shadowed APPEND still
// named. See that function's doc.
//
// It is kept anyway because a cancel that is correct only by the grace of a
// foreign key elsewhere is one schema change away from being wrong, and the
// sibling cancelPendingAppendIn — whose caller does NOT delete the message — is
// load-bearing for exactly this reason.
//
// ⚠ THE SHAPE OF THE BUG, which is the part worth carrying forward: three
// separate predicates enumerated intent states, migration 00056 added a fourth,
// and none was revisited. If a fifth is ever added, grep for `state IN (` before
// assuming it is inert.
func (tx *UserTx) cancelPendingAppend(ctx context.Context, contentHash []byte) error {
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'append'
		   AND state IN ('pending', 'suppressed')
		   AND payload->>'content_hash' = $2`,
		tx.userID.String(), hex.EncodeToString(contentHash)); err != nil {
		return fmt.Errorf("store: cancel pending append: %w", err)
	}
	return nil
}

// cancelPendingAppendIn drops a not-yet-applied APPEND into ONE mailbox naming
// any of hashes, and reports whether it removed anything. The boolean is the
// point: it is how the caller knows a copy was never written to the server, and
// therefore that no expunge is owed (#268).
func (tx *UserTx) cancelPendingAppendIn(ctx context.Context, hashes []string, mailbox string) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'append'
		   AND state IN ('pending', 'suppressed')
		   AND payload->>'content_hash' = ANY($2::text[])
		   AND payload->>'mailbox' = $3`,
		tx.userID.String(), hashes, mailbox)
	if err != nil {
		return false, fmt.Errorf("store: cancel pending drafts append: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// heldContentHashes returns every content hash a message has had — the one it
// holds now, plus any it has been given and then replaced (app.superseded_content,
// #260) — as the hex strings a writeback payload names bytes by.
//
// A message only ever has more than one when its bytes were rewritten in place,
// which today means exactly one thing: it was signed at submit time. See the
// call site in RetireSentDraftCopy for why the difference is load-bearing.
func (tx *UserTx) heldContentHashes(ctx context.Context, id ulid.ULID, current []byte) ([]string, error) {
	out := []string{hex.EncodeToString(current)}
	rows, err := tx.tx.Query(ctx, `
		SELECT content_hash FROM app.superseded_content
		 WHERE user_id = $1 AND message_id = $2`,
		tx.userID.String(), id.String())
	if err != nil {
		return nil, fmt.Errorf("store: read superseded content hashes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: scan superseded content hash: %w", err)
		}
		out = append(out, hex.EncodeToString(h))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read superseded content hashes: %w", err)
	}
	return out, nil
}

// MarkMessageSent flips a just-sent draft out of the Drafts state: it clears
// $draft, marks it \Seen (we wrote it), moves its local placement to the Sent
// mailbox, and appends the Email change_log row. The IMAP side is handled by the
// Sent APPEND intent, not by this row.
func (tx *UserTx) MarkMessageSent(ctx context.Context, id, sentMailboxID ulid.ULID) error {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages
		   SET is_draft = false, is_seen = true, local_mailbox_id = $3::app.ulid
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), sentMailboxID.String())
	if err != nil {
		return fmt.Errorf("store: mark message sent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMessageNotFound
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, id, OpUpdated); err != nil {
		return err
	}
	return nil
}

// RawMessageRef is what the send path needs to find a message's bytes and to
// recognize the provider's own copy of it later.
type RawMessageRef struct {
	ContentHash   []byte
	MessageIDHint string
	SizeBytes     int64
	IsDraft       bool
}

// MessageRef reads the blob reference and Message-ID of a message.
func (tx *UserTx) MessageRef(ctx context.Context, id ulid.ULID) (RawMessageRef, error) {
	var r RawMessageRef
	err := tx.tx.QueryRow(ctx, `
		SELECT content_hash, COALESCE(message_id_hint, ''), size_bytes, is_draft
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()).
		Scan(&r.ContentHash, &r.MessageIDHint, &r.SizeBytes, &r.IsDraft)
	if err != nil {
		if isNoRows(err) {
			return RawMessageRef{}, ErrMessageNotFound
		}
		return RawMessageRef{}, fmt.Errorf("store: message ref: %w", err)
	}
	return r, nil
}

// MailboxTarget is an account's mailbox for a JMAP role, with the raw IMAP name
// the writeback engine needs to address it.
type MailboxTarget struct {
	ID        ulid.ULID
	AccountID ulid.ULID
	IMAPName  string
}

// MailboxByID resolves a mailbox the client named to the account that owns it
// and its raw IMAP name — what a move into an explicitly named folder needs in
// order to reach THAT folder (rather than a role-shaped approximation of it).
// found is false when the mailbox does not exist for this user.
func (tx *UserTx) MailboxByID(ctx context.Context, id ulid.ULID) (MailboxTarget, bool, error) {
	var accountID, imapName string
	err := tx.tx.QueryRow(ctx, `
		SELECT account_id, imap_name FROM app.mailboxes
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()).Scan(&accountID, &imapName)
	if err != nil {
		if isNoRows(err) {
			return MailboxTarget{}, false, nil
		}
		return MailboxTarget{}, false, fmt.Errorf("store: mailbox by id: %w", err)
	}
	acct, err := ulid.Parse(accountID)
	if err != nil {
		return MailboxTarget{}, false, fmt.Errorf("store: parse account id %q: %w", accountID, err)
	}
	return MailboxTarget{ID: id, AccountID: acct, IMAPName: imapName}, true, nil
}

// MailboxForRole resolves an account's mailbox with the given JMAP role
// ("drafts", "sent", "trash", "archive", ...), as detected from RFC 6154
// SPECIAL-USE at onboarding. found is false when the account has no such
// mailbox — the caller must surface that (a send with nowhere to file the Sent
// copy is a real, user-visible account-health problem), never invent a name.
func (tx *UserTx) MailboxForRole(ctx context.Context, accountID ulid.ULID, role string) (MailboxTarget, bool, error) {
	// INBOX carries no SPECIAL-USE attribute (it is the inbox by its reserved
	// name, RFC 3501), so it must be included here or a move to role "inbox"
	// (e.g. un-archiving a message) would never resolve a destination.
	rows, err := tx.tx.Query(ctx, `
		SELECT id, imap_name, COALESCE(special_use, '')
		  FROM app.mailboxes
		 WHERE user_id = $1 AND account_id = $2
		   AND (special_use IS NOT NULL OR lower(imap_name) = 'inbox')
		 ORDER BY imap_name`,
		tx.userID.String(), accountID.String())
	if err != nil {
		return MailboxTarget{}, false, fmt.Errorf("store: mailbox for role: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, specialUse string
		if err := rows.Scan(&id, &name, &specialUse); err != nil {
			return MailboxTarget{}, false, fmt.Errorf("store: scan mailbox for role: %w", err)
		}
		if roleForMailbox(name, specialUse) != role {
			continue
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return MailboxTarget{}, false, fmt.Errorf("store: parse mailbox id %q: %w", id, err)
		}
		return MailboxTarget{ID: u, AccountID: accountID, IMAPName: name}, true, nil
	}
	if err := rows.Err(); err != nil {
		return MailboxTarget{}, false, fmt.Errorf("store: mailbox for role: %w", err)
	}
	rows.Close()
	if role == "archive" {
		// A Gmail OAuth account's Archive is All Mail (ADR-047 §2): archiving
		// places the message there and its writeback removes \Inbox (§3).
		return tx.gmailArchiveMailbox(ctx, accountID)
	}
	return MailboxTarget{}, false, nil
}

// appendPayload is the JSONB payload of an `append` writeback intent. It mirrors
// writeback.intentPayload's field names (the engine reads it back).
//
// It deliberately carries NO body bytes (mailroom#89). Before this change, the
// raw message went into this JSONB payload whole, base64-expanded (~1.33x) —
// harmless for a small message, but with attachments (mailroom#85) a 50 MiB
// draft wrote ~67 MiB into app.writeback_intents on every autosave. The bytes
// are already sitting in the blob store by the time this is enqueued (see
// EnqueueAppendIntent's doc), so the payload now names them by ContentHash and
// the writeback engine reads them back from there when it applies the intent.
type appendPayload struct {
	Mailbox string   `json:"mailbox"`
	Flags   []string `json:"flags,omitempty"`
	// MessageIDHeader is the RFC 5322 Message-ID of the bytes being appended.
	// The engine searches the target mailbox for it FIRST and skips the append
	// if the provider already filed its own copy of this very message (spec §4:
	// "the appender first checks whether the provider self-appended, so relayed
	// sends never appear twice"; ADR-001).
	MessageIDHeader string `json:"message_id_header,omitempty"`
	// ContentHash (hex) identifies the bytes: it is both what
	// cancelPendingAppend uses to find and drop a superseded draft's queued
	// APPEND before it ever reaches the server, AND what the writeback engine
	// resolves to the actual bytes via the blob store (mailroom#89).
	ContentHash string `json:"content_hash"`
}

// EnqueueAppendIntent queues an IMAP APPEND of an already-stored blob into an
// account's mailbox (spec §4a). The payload carries only the bytes' CONTENT
// HASH, never the bytes themselves (mailroom#89) — the writeback engine reads
// them back out of the blob store when it applies the intent, from exactly
// where the caller already wrote them. Every caller writes the blob strictly
// BEFORE calling this: compose.Composer.record calls blobs.Put when it creates
// the draft (a draft's own APPEND) or the just-sent message row (SendWorker's
// Sent-copy APPEND names the SAME message's content hash, already stored at
// draft-save time — nothing writes the blob twice). So an intent can never
// name bytes that were never stored.
//
// body is still accepted (existing callers already have the bytes in hand for
// other reasons — e.g. the Sent-append self-dedup probe reads them for SMTP
// transmission) but it is no longer persisted here; the parameter stays so
// this change does not force an unrelated signature churn across every call
// site while it is reviewed.
//
// The idempotency key is derived from the CONTENT HASH and the target mailbox,
// so re-enqueueing the same bytes into the same mailbox — a retried job, a
// replayed send — collapses to exactly one intent, which is the whole point
// of ADR-001's identity being the content hash.
// ⚠ messageID TIES THE INTENT TO THE MESSAGE IT WILL FILE, and that is what
// makes hard delete safe (#418 item 2).
//
// app.writeback_intents.message_id has always existed, with a foreign key and
// ON DELETE CASCADE — this path simply never set it, so an APPEND was an
// orphan. HardDeleteMessage deletes the message row and THEN reclaims the blob,
// so with the column populated the cascade removes the intent first and the
// reclaim is safe. Without it, the intent survived its own message and could
// never complete: its bytes were gone.
//
// ⚠ THE SEMANTICS ARE THE POINT, NOT THE FOREIGN KEY. An APPEND must DIE with
// its message: the user asked for that mail to be destroyed, and filing it onto
// an IMAP server afterwards is the one outcome hard delete exists to prevent.
//
// ⚠ AN EXPUNGE IS THE OPPOSITE AND MUST NOT BE COPIED FROM HERE. It removes a
// copy from the server, so it has to OUTLIVE the local row — cascading it away
// would leave the copy on the provider forever. That is why the draft-retire
// expunges deliberately carry no message_id while purge.go's expunge does: the
// purge path has already established there is no live provenance, so nothing is
// left to remove. Observed on a real instance while fixing this: the
// append and expunge intents had a NULL message_id, while the flag/
// keyword/move intents all set it.
func (tx *UserTx) EnqueueAppendIntent(ctx context.Context, messageID, accountID ulid.ULID, mailbox string, flags []string, body, contentHash []byte, messageIDHeader string) error {
	// body is intentionally unused: see the doc comment above.
	payload, err := json.Marshal(appendPayload{
		Mailbox: mailbox, Flags: flags,
		MessageIDHeader: messageIDHeader,
		ContentHash:     hex.EncodeToString(contentHash),
	})
	if err != nil {
		return fmt.Errorf("store: marshal append intent: %w", err)
	}
	mid := messageID
	key := fmt.Sprintf("append:%s:%x:%s", accountID, contentHash, mailbox)
	if _, err := tx.EnqueueIntent(ctx, IntentRow{
		MessageID:      &mid,
		AccountID:      accountID,
		Op:             IntentAppend,
		Payload:        payload,
		IdempotencyKey: key,
		Source:         IntentByDraft,
	}); err != nil {
		return err
	}
	return nil
}

// ContentHashInMailbox reports whether a message with this exact content hash
// already has a LIVE physical location in the given mailbox — i.e. whether the
// provider self-appended a byte-identical copy that sync has already ingested.
// It is the cheap, store-side half of the Sent-append dedup (the other half is
// the engine's pre-append IMAP search, which also catches a copy sync has not
// seen yet).
func (tx *UserTx) ContentHashInMailbox(ctx context.Context, contentHash []byte, mailboxID ulid.ULID) (bool, error) {
	var n int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM app.message_provenance p
		  JOIN app.messages m ON m.id = p.message_id AND m.user_id = p.user_id
		 WHERE p.user_id = $1 AND p.mailbox_id = $2 AND p.state = 'live'
		   AND m.content_hash = $3`,
		tx.userID.String(), mailboxID.String(), contentHash).Scan(&n); err != nil {
		return false, fmt.Errorf("store: content hash in mailbox: %w", err)
	}
	return n > 0, nil
}

// PendingIntentCount reports how many writeback intents are waiting for an
// account. The writeback runner checks it before dialing IMAP, so an idle
// account costs one cheap query per pass instead of a connection.
func (tx *UserTx) PendingIntentCount(ctx context.Context, accountID ulid.ULID) (int, error) {
	var n int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM app.writeback_intents
		 WHERE user_id = $1 AND account_id = $2 AND state = 'pending'`,
		tx.userID.String(), accountID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: pending intent count: %w", err)
	}
	return n, nil
}

// draftAppendTarget is one mailbox this version's bytes were queued to be
// APPENDED to, and whether that append has (or may have) reached the server.
type draftAppendTarget struct {
	AccountID ulid.ULID
	IMAPName  string
	// CopyExists is true when the APPEND has run, or is in flight and so may
	// have run. False means the copy was never created and none is owed.
	CopyExists bool
	// InFlight is true when the APPEND has been claimed by a writeback pass that
	// has not finished it. Its copy may or may not exist yet, and — the reason
	// this is reported separately (#691) — it may not yet have READ its bytes.
	InFlight bool
}

// draftAppendTargets reads the APPEND intents for these exact bytes and reports,
// per mailbox, whether a copy exists on the server.
//
// ⚠ THIS IS THE FACT `RetireDraft` USED TO GO WITHOUT, AND THE LEAK WAS THE GAP
// (#603). It previously decided what to expunge from `app.message_provenance`
// alone — copies SYNC HAS SEEN. But provenance is written when a sync pass
// observes the UID, not when the APPEND applies, and that lag was MEASURED at
// tens of seconds while autosave supersedes every 15. So a
// draft replaced inside the gap had its APPEND already applied (nothing to
// cancel) and no provenance yet (nothing to expunge), and its copy was left on
// the provider with the last trace of it — the append intent — then deleted by
// the message's own ON DELETE CASCADE. The next sync found an unowned message in
// Drafts and ingested it as a NEW draft. That is the duplicate users saw.
//
// The intent row is the better witness than provenance, and it is already here:
// it names the account and the mailbox, and its STATE says whether the bytes
// were written. `RetireSentDraftCopy` has decided this way since #268
// (`cancelPendingAppendIn` returns "did we stop it in time?" for exactly this
// reason); this is the same rule, reaching the path that did not get it.
func (tx *UserTx) draftAppendTargets(ctx context.Context, contentHash []byte) ([]draftAppendTarget, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT account_id, COALESCE(payload->>'mailbox', ''), state::text
		  FROM app.writeback_intents
		 WHERE user_id = $1 AND op = 'append'
		   AND payload->>'content_hash' = $2`,
		tx.userID.String(), hex.EncodeToString(contentHash))
	if err != nil {
		return nil, fmt.Errorf("store: draft append targets: %w", err)
	}
	defer rows.Close()
	var out []draftAppendTarget
	for rows.Next() {
		var t draftAppendTarget
		var acct, state string
		if err := rows.Scan(&acct, &t.IMAPName, &state); err != nil {
			return nil, fmt.Errorf("store: scan draft append target: %w", err)
		}
		if t.AccountID, err = ulid.Parse(acct); err != nil {
			return nil, fmt.Errorf("store: parse account id %q: %w", acct, err)
		}
		// 'inflight' counts as EXISTING, matching cancelPendingAppend's own rule:
		// one already in flight has, or may have, reached the server, and the
		// expunge is what cleans that up. 'dead' never ran and never will.
		t.CopyExists = state == "inflight" || state == "applied"
		t.InFlight = state == "inflight"
		out = append(out, t)
	}
	return out, rows.Err()
}

// inDraftsMailbox reports whether the message's EFFECTIVE membership (moved,
// local, or live provenance: mailboxMembershipSQL) includes a Drafts-role
// mailbox. The role is roleForMailbox's, the one every other role lookup uses.
func (tx *UserTx) inDraftsMailbox(ctx context.Context, id ulid.ULID) (bool, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT mb.imap_name, COALESCE(mb.special_use, '')
		  FROM app.messages m
		  JOIN app.mailboxes mb ON mb.user_id = m.user_id
		 WHERE m.user_id = $1 AND m.id = $2
		   AND `+mailboxMembershipSQL("$1", "= mb.id"),
		tx.userID.String(), id.String())
	if err != nil {
		return false, fmt.Errorf("store: draft mailbox: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name, specialUse string
		if err := rows.Scan(&name, &specialUse); err != nil {
			return false, fmt.Errorf("store: scan draft mailbox: %w", err)
		}
		if roleForMailbox(name, specialUse) == "drafts" {
			found = true
		}
	}
	return found, rows.Err()
}
