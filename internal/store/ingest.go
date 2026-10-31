// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// BlobExists reports whether the CALLER has a blob with this content hash
// (app.blobs, under RLS).
//
// It is the authorization check the JMAP download and attach paths need for a
// blob that is not yet attached to any message (a fresh upload). The blob store
// is content-addressed, so ownership must be a question about the user's ROWS —
// never about the hash, which is guessable in principle and shared by
// construction whenever two users hold identical bytes.
func (tx *UserTx) BlobExists(ctx context.Context, contentHash []byte) (bool, error) {
	var one int
	err := tx.tx.QueryRow(ctx,
		`SELECT 1 FROM app.blobs WHERE user_id = $1 AND content_hash = $2`,
		tx.userID.String(), contentHash).Scan(&one)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: blob exists: %w", err)
	}
	return true, nil
}

// This file holds the store surface for canonical ingest (spec §4, ADR D1):
// content-hash message identity, provenance (where a message physically
// lives), flag merge, and incremental threading. Every method runs on a
// UserTx, so RLS scopes each statement and the change_log invariant
// (CLAUDE.md, ADR-003) is upheld within the ingesting transaction.

// nullStr maps "" to a SQL NULL for nullable text/citext columns.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullBytes maps an empty slice to a SQL NULL for nullable bytea columns.
//
// The distinction matters for dedup_hash: NULL means "not computed, does not
// participate", while a zero-length bytea would be a HASH VALUE that every
// not-yet-backfilled row shared — turning the whole un-backfilled table into one
// giant duplicate group.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullStrSlice maps a nil/empty string slice to a SQL NULL for a nullable
// text[] column (dkim_domains) — the array counterpart of nullStr above, and
// for the same reason: "not extracted" and "extracted, nothing found" are
// deliberately not distinguished (migration 00027).
func nullStrSlice(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}

// nullInt maps a zero int to a SQL NULL, for classifier_version — where 0 means
// "no classifier ran" and must not be stored as a real version number that a
// later re-classify would then treat as merely stale.
func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// MessageFlags is the canonical (merged) flag set of a message.
type MessageFlags struct {
	Seen     bool
	Flagged  bool
	Answered bool
	Draft    bool
	Junk     bool
	Keywords []string
}

// BlobMeta is the app.blobs row for a stored blob; the bytes live in the
// BlobStore, content-addressed by ContentHash (SHA-256 of the plaintext).
type BlobMeta struct {
	ContentHash []byte
	SizePlain   int64
	SizeStored  int64
	DEKID       ulid.ULID
	// Kind is the blob class (BlobKindMail, BlobKindProxyCache). Empty means
	// mail — every writer that predates ADR-026 leaves it empty and means mail.
	Kind        string
	EnvelopeVer int
}

// Blob classes (migration 00038, ADR-026). BlobKindMail is the user's own bytes
// and counts against their storage quota; BlobKindProxyCache is a third-party
// image the image proxy fetched on their behalf, which does not.
const (
	BlobKindMail       = "mail"
	BlobKindProxyCache = "proxy_cache"
)

// InsertBlobMeta records blob metadata. Blobs are content-addressed and
// immutable, so a repeat is a no-op — EXCEPT for one promotion.
//
// ⚠ A 'mail' INSERT PROMOTES AN EXISTING 'proxy_cache' ROW, and never the
// reverse. Blobs are addressed by the hash of their PLAINTEXT, so if a cached
// image's bytes ever equalled a message's or an attachment's, ONE row would
// serve both. Whichever writer arrived first would have set `kind` — and if that
// was the cache, the user's own mail would sit under 'proxy_cache' and be
// silently EXCLUDED FROM THEIR STORAGE QUOTA, which is the one thing this column
// exists to get right.
//
// The collision is vanishingly unlikely: a PNG is not an RFC 5322 message. But
// "it has never happened" is exactly the reasoning #300 found had been
// protecting a reclaim clause that could not fire, so the promotion is written
// down rather than relied upon not to be needed. The reverse — a cache insert
// demoting a mail row — must never happen, and the WHERE is what forbids it.
func (tx *UserTx) InsertBlobMeta(ctx context.Context, b BlobMeta) error {
	kind := b.Kind
	if kind == "" {
		kind = BlobKindMail
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id, envelope_ver, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, content_hash) DO UPDATE
		   SET kind = 'mail'
		 WHERE app.blobs.kind <> 'mail' AND EXCLUDED.kind = 'mail'`,
		tx.userID.String(), b.ContentHash, b.SizePlain, b.SizeStored, b.DEKID.String(), b.EnvelopeVer, kind,
	); err != nil {
		return fmt.Errorf("store: insert blob meta: %w", err)
	}
	return nil
}

// MessageRow is a canonical message projection inserted at ingest. The
// authoritative bytes are the blob; these columns feed list views and search.
type MessageRow struct {
	ID             ulid.ULID
	ContentHash    []byte
	ThreadID       ulid.ULID
	Subject        string
	Snippet        string
	FromName       string
	FromAddress    string
	SentAt         *time.Time
	ReceivedAt     time.Time
	SizeBytes      int64
	HasAttachments bool
	MessageIDHint  string
	InReplyTo      string
	ReferenceIDs   []string
	Flags          MessageFlags

	// AddressedAddress is the alias this message was DELIVERED TO — the exact
	// address a reply should come From (spec §5). AddressedIdentityID is the
	// verified identity it resolved to, or nil when the derived address matches
	// no identity we know of yet.
	AddressedAddress    string
	AddressedIdentityID *ulid.ULID

	// LocalMailboxID places a message Mailroom authored itself (a draft, or a
	// just-composed send) that is not on the IMAP server yet, so it has no
	// provenance row to derive membership from. Nil for anything ingested from
	// a server. See db/schema.sql app.messages.local_mailbox_id.
	LocalMailboxID *ulid.ULID

	// The bulk/priority signals read off the headers at ingest and persisted
	// (migration 00016, #157), so the training path sees what the serve path saw.
	// Empty/0/false for a message with no such headers, which is the honest
	// reading — not "unknown".
	ListID          string
	ListUnsubscribe string
	RecipientCount  int
	AuthFail        bool

	// The rest of kind.Facts (migration 00027, #156): see function_signals.go
	// for the shape and the NULL-semantics decision. Populated by
	// ingest.FunctionSignalsFromHeader — the SAME extraction the
	// backfill-function-signals command calls, so the two write paths cannot
	// drift apart. Flat fields, matching the #157 block above rather than a
	// nested struct, even though they are always set together as a
	// store.FunctionSignals at the call site.
	ListUnsubscribePost string
	AutoSubmitted       string
	Precedence          string
	FeedbackID          string
	DKIMDomains         []string
	ReturnPath          string

	// ClassifiedTab is the category-tab classifier's verdict for this message and
	// ClassifierVersion the rule-list version that produced it (#228, ADR-022,
	// migration 00029). "" means unclassified.
	//
	// ⚠ NOT the message's tab. The tab is COALESCE(mapping_tab, ClassifiedTab),
	// with the mapping half read live at query time — so re-pointing a label moves
	// its mail on the next read with no backfill (ADR-021). What is stored is only
	// the classifier's opinion, which changes on release cadence rather than user
	// cadence.
	ClassifiedTab     string
	ClassifierVersion int

	// DedupHash detects a RE-DELIVERY of this message (#206, ADR-018): the hash of
	// its identity-bearing headers plus its body (ingest.DedupHash), which unlike
	// ContentHash is unchanged by the trace metadata a mail path stamps on each
	// delivery. Nil is legal and means "does not participate in re-delivery dedup",
	// the behaviour of every message before migration 00023.
	//
	// It is NOT identity. Two rows CAN share it — that is the condition it exists to
	// find — whereas two rows sharing ContentHash are the same bytes.
	DedupHash []byte
}

// MessageIDByDedupHash returns the id of the user's OLDEST message with the given
// dedup hash, if one exists — a RE-DELIVERY of a message already held (#206).
//
// Oldest, deliberately: ULIDs sort by creation time, so `ORDER BY id` picks the copy
// that arrived first. That is the one whose id other rows already reference and whose
// received_at is the true arrival, so attaching the new delivery's provenance to it
// keeps the message's history in the right order.
//
// Callers MUST try MessageIDByContentHash first. Identical bytes are certainly the
// same message; an equal dedup hash is a strong inference, and checking the certain
// thing first keeps the inference from ever overriding it.
//
// A nil or empty hash matches nothing rather than matching every un-backfilled row —
// the index is partial on `dedup_hash IS NOT NULL` for the same reason.
func (tx *UserTx) MessageIDByDedupHash(ctx context.Context, dedupHash []byte) (ulid.ULID, bool, error) {
	if len(dedupHash) == 0 {
		return ulid.ULID{}, false, nil
	}
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.messages
		 WHERE user_id = $1 AND dedup_hash = $2
		 ORDER BY id
		 LIMIT 1`,
		tx.userID.String(), dedupHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: message by dedup hash: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse message id %q: %w", id, err)
	}
	return u, true, nil
}

// MessageIDByContentHash returns the id of the user's message with the given
// content hash, if one exists — the dedup identity (ADR D1).
func (tx *UserTx) MessageIDByContentHash(ctx context.Context, contentHash []byte) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx,
		`SELECT id FROM app.messages WHERE user_id = $1 AND content_hash = $2`,
		tx.userID.String(), contentHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: message by content hash: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse message id %q: %w", id, err)
	}
	return u, true, nil
}

// InsertMessage inserts a new canonical message. The caller must have set
// ThreadID and appended the Email change_log row in the same transaction.
func (tx *UserTx) InsertMessage(ctx context.Context, m MessageRow) error {
	var identity, localMailbox any
	if m.AddressedIdentityID != nil {
		identity = m.AddressedIdentityID.String()
	}
	if m.LocalMailboxID != nil {
		localMailbox = m.LocalMailboxID.String()
	}
	// A message that ARRIVES flagged (initial sync of a mailbox the user starred
	// long ago, or a message another client flagged before Mailroom ever saw it)
	// gets flagged_at = received_at — the same lower bound migration 00047's
	// backfill uses, and for the same reason: now() would be actively wrong here,
	// reading an old starred message as "waiting 0 d" the instant it is ingested.
	// NULL for anything not flagged on arrival, matching the false state exactly
	// (#444).
	var flaggedAt any
	if m.Flags.Flagged {
		flaggedAt = m.ReceivedAt
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.messages (
			id, user_id, content_hash, thread_id,
			subject, snippet, from_name, from_address, sent_at, received_at,
			size_bytes, has_attachments,
			message_id_hint, in_reply_to, references_ids,
			is_seen, is_flagged, is_answered, is_draft, is_junk, keywords,
			addressed_identity_id, addressed_address, local_mailbox_id,
			list_id, list_unsubscribe, recipient_count, auth_fail, dedup_hash,
			list_unsubscribe_post, auto_submitted, precedence, feedback_id, dkim_domains, return_path,
			classified_tab, classifier_version, flagged_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,
		        $22::app.ulid,$23,$24::app.ulid,$25,$26,$27,$28,$29,
		        $30,$31,$32,$33,$34::text[],$35,
		        NULLIF($36,'')::app.category_tab,$37,$38)`,
		m.ID.String(), tx.userID.String(), m.ContentHash, m.ThreadID.String(),
		m.Subject, m.Snippet, m.FromName, nullStr(m.FromAddress), m.SentAt, m.ReceivedAt,
		m.SizeBytes, m.HasAttachments,
		nullStr(m.MessageIDHint), nullStr(m.InReplyTo), m.ReferenceIDs,
		m.Flags.Seen, m.Flags.Flagged, m.Flags.Answered, m.Flags.Draft, m.Flags.Junk, m.Flags.Keywords,
		identity, nullStr(NormalizeAddress(m.AddressedAddress)), localMailbox,
		nullStr(m.ListID), nullStr(m.ListUnsubscribe), m.RecipientCount, m.AuthFail,
		nullBytes(m.DedupHash),
		nullStr(m.ListUnsubscribePost), nullStr(m.AutoSubmitted), nullStr(m.Precedence),
		nullStr(m.FeedbackID), nullStrSlice(m.DKIMDomains), nullStr(m.ReturnPath),
		m.ClassifiedTab, nullInt(m.ClassifierVersion), flaggedAt,
	); err != nil {
		return fmt.Errorf("store: insert message: %w", err)
	}
	return nil
}

// UpdateMessageFlagsIfChanged writes merged canonical flags only when they
// differ from what is stored, returning whether a change occurred (so the
// caller appends an Email change only on a real update).
//
// flagged_at rides along in SQL, not Go, so it can never disagree with
// is_flagged (#444): it becomes now() the instant is_flagged goes false->true
// on an inbound sync, NULL the instant it goes true->false, and is left alone
// otherwise. The IS DISTINCT FROM guard already limits the whole statement to
// real changes, so an unchanged flag never re-evaluates the CASE at all.
func (tx *UserTx) UpdateMessageFlagsIfChanged(ctx context.Context, messageID ulid.ULID, f MessageFlags) (bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET is_seen = $3, is_flagged = $4, is_answered = $5, is_draft = $6, is_junk = $7, keywords = $8,
		       flagged_at = CASE WHEN $4 AND NOT is_flagged THEN now()
		                         WHEN NOT $4 THEN NULL
		                         ELSE flagged_at END
		 WHERE user_id = $1 AND id = $2
		   AND (is_seen, is_flagged, is_answered, is_draft, is_junk, keywords)
		       IS DISTINCT FROM ($3, $4, $5, $6, $7, $8)
		RETURNING id`,
		tx.userID.String(), messageID.String(),
		f.Seen, f.Flagged, f.Answered, f.Draft, f.Junk, f.Keywords).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update message flags: %w", err)
	}
	return true, nil
}

// ProvenanceRow records where a canonical message physically lives (spec §4).
type ProvenanceRow struct {
	MessageID   ulid.ULID
	AccountID   ulid.ULID
	MailboxID   ulid.ULID
	UID         int64
	UIDValidity int64
	GmMsgID     string
	RemoteFlags []string
}

// UpsertProvenance records a physical location for a message and reports whether
// that location is NOW LIVE AND WAS NOT BEFORE — i.e. whether this call gave the
// message a live physical location it did not previously have.
//
// That one boolean deliberately covers two paths, because they mean the same
// thing to the caller: the message's JMAP mailbox membership may have just
// changed (so an Email change_log row is owed — the load-bearing invariant), and
// any reaper soft-delete standing against it has just been PROVED WRONG by the
// server (so it must be retracted — RetractVanished).
//
//   - INSERT — the server handed us a (mailbox, uidvalidity, uid) coordinate we
//     had never seen. A brand-new location.
//   - REVIVE — the row existed but had been retired to 'stale_uidvalidity' by
//     MarkMailboxProvenanceStale. Membership derivation (jmap_email.go
//     effectiveMembershipSQL) and the flag merge (LiveProvenanceFlags) both
//     filter state='live', so a stale row is invisible to every client: as far
//     as JMAP is concerned that location did not exist. The SERVER has just
//     served this exact coordinate again, so it does exist, and the message
//     re-enters the folder. Before this reported true, that re-entry happened
//     with NO change_log row and no retraction — the message silently reappeared
//     and the search index never heard about it. It is reachable whenever a
//     UIDVALIDITY roll is still awaiting reconciliation (reaper.go
//     ReconcileRetiredGeneration) and the generation reverts to a value we have
//     seen before — a mailbox restored from backup, which is precisely when a
//     mailbox most needs to come back rather than stay half-invisible.
//
// A repeat sight of an already-live row (the ordinary poll) refreshes the remote
// flags in place and reports false: nothing about the LOCATION changed, and a
// change_log row would be a lie. Flag changes are detected separately, by
// UpdateMessageFlagsIfChanged.
//
// The `existing` CTE reads the pre-upsert state, because a data-modifying WITH
// cannot see its own effects and RETURNING sees only the NEW row — the same
// snapshot trick UpsertMailbox uses for its change detection.
func (tx *UserTx) UpsertProvenance(ctx context.Context, p ProvenanceRow) (bool, error) {
	var nowLive bool
	err := tx.tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT state FROM app.message_provenance
			 WHERE user_id = $2 AND mailbox_id = $5 AND uidvalidity = $7 AND uid = $6
		),
		upsert AS (
			INSERT INTO app.message_provenance
				(id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, gm_msgid, remote_flags)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (mailbox_id, uidvalidity, uid) DO UPDATE
				SET remote_flags = EXCLUDED.remote_flags,
				    message_id   = EXCLUDED.message_id,
				    state        = 'live'
			RETURNING (xmax = 0) AS created
		)
		SELECT u.created
		    OR COALESCE((SELECT e.state FROM existing e) <> 'live', false)
		  FROM upsert u`,
		NewULID().String(), tx.userID.String(), p.MessageID.String(), p.AccountID.String(),
		p.MailboxID.String(), p.UID, p.UIDValidity, nullStr(p.GmMsgID), p.RemoteFlags).Scan(&nowLive)
	if err != nil {
		return false, fmt.Errorf("store: upsert provenance: %w", err)
	}
	return nowLive, nil
}

// RefreshLiveProvenanceFlags sets remote_flags on the LIVE provenance row at
// (mailbox, uidvalidity, uid) and returns the message it belongs to. found is
// false when no live row exists there (never seen, reaped, or only a
// stale_uidvalidity row) — the caller must then fetch the body and ingest,
// which is what creates, revives, or retracts. It never inserts and never
// changes state (#523): it is the flags-only counterpart of UpsertProvenance,
// used when a CONDSTORE flag-change fetch has nothing else to report.
func (tx *UserTx) RefreshLiveProvenanceFlags(ctx context.Context, mailboxID ulid.ULID, uidValidity, uid int64, remoteFlags []string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.message_provenance
		   SET remote_flags = $5
		 WHERE user_id = $1 AND mailbox_id = $2 AND uidvalidity = $3 AND uid = $4 AND state = 'live'
		RETURNING message_id`,
		tx.userID.String(), mailboxID.String(), uidValidity, uid, remoteFlags).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: refresh live provenance flags: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse message id %q: %w", id, err)
	}
	return u, true, nil
}

// RecordedFlags is one live copy's recorded remote flags, as the flag sweep
// compares a server reading against them.
type RecordedFlags struct {
	Flags []string
	// PendingWriteback: the copy's message has a flag or keyword writeback
	// intent still pending or inflight — a local edit the server has not been
	// told about yet (review L6 of #910). The sweep leaves such a copy alone
	// rather than undoing the edit with the server's older state.
	PendingWriteback bool
}

// LiveRemoteFlagsByUID returns, for each of uids that has a LIVE provenance
// row at (mailboxID, uidValidity, uid), its recorded remote_flags and whether
// its message has a flag/keyword writeback still owed. It is the
// CONDSTORE-less flag sweep's comparison baseline (ADR-046 §12): read-only,
// one query per swept range, served by the unique (mailbox_id, uidvalidity,
// uid) index and, for the intent check, the partial writeback_pending index.
// A UID with no live row is simply not in the map.
func (tx *UserTx) LiveRemoteFlagsByUID(ctx context.Context, mailboxID ulid.ULID, uidValidity int64, uids []int64) (map[int64]RecordedFlags, error) {
	out := make(map[int64]RecordedFlags, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT p.uid, p.remote_flags,
		       EXISTS (SELECT 1 FROM app.writeback_intents w
		                WHERE w.user_id = p.user_id AND w.account_id = p.account_id
		                  AND w.message_id = p.message_id
		                  AND w.state IN ('pending', 'inflight')
		                  AND w.op IN ('+flag', '-flag', '+kw', '-kw'))
		  FROM app.message_provenance p
		 WHERE p.user_id = $1 AND p.mailbox_id = $2 AND p.uidvalidity = $3
		   AND p.uid = ANY($4) AND p.state = 'live'`,
		tx.userID.String(), mailboxID.String(), uidValidity, uids)
	if err != nil {
		return nil, fmt.Errorf("store: live remote flags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			uid int64
			r   RecordedFlags
		)
		if err := rows.Scan(&uid, &r.Flags, &r.PendingWriteback); err != nil {
			return nil, fmt.Errorf("store: scan live remote flags: %w", err)
		}
		if r.Flags == nil {
			r.Flags = []string{}
		}
		out[uid] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: live remote flags: %w", err)
	}
	return out, nil
}

// LiveProvenanceFlags returns the remote flag sets of every live physical
// location of a message, for computing the merged canonical flags.
func (tx *UserTx) LiveProvenanceFlags(ctx context.Context, messageID ulid.ULID) ([][]string, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT remote_flags FROM app.message_provenance
		 WHERE user_id = $1 AND message_id = $2 AND state = 'live'`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return nil, fmt.Errorf("store: live provenance flags: %w", err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var flags []string
		if err := rows.Scan(&flags); err != nil {
			return nil, fmt.Errorf("store: scan provenance flags: %w", err)
		}
		out = append(out, flags)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: live provenance flags: %w", err)
	}
	return out, nil
}

// FindThreadByHints finds an existing thread to attach a message to by
// matching Message-IDs in three directions: this message references an
// existing one, an existing one references this message, or an existing one
// IS this message (same Message-ID). It returns the first such thread; thread
// merging across multiple matches is deferred. hintRefs are the new message's
// References ∪ In-Reply-To; selfID is its own Message-ID (may be empty).
//
// ⚠ THE SAME-MESSAGE-ID ARM IS LOAD-BEARING AND WAS MISSING (#210). RFC 5322
// §3.6.4 makes a Message-ID globally unique, so two messages bearing one are
// the same message and belong in one conversation. Without this arm a message
// carrying no References — which is most bulk mail — started a fresh thread
// even when a copy of it was already threaded. Measured on a real
// mailbox before the fix: duplicate Message-ID groups were split across
// separate threads, every one of them a single message re-delivered through
// more than one catchall alias (same subject, same sender, same day, differing
// only in headers outside #206's dedup allowlist).
//
// ⚠ It cannot be used to DISPLACE anything, and that is structural rather than
// checked: this function only chooses a thread for the message being ingested.
// It never moves, re-parents or removes an existing message, so a sender who
// forges someone else's Message-ID can join a thread — which they could
// already do by naming that id in References, the reason the old comment's
// spoofing concern was not load-bearing — and can do nothing else.
func (tx *UserTx) FindThreadByHints(ctx context.Context, selfID string, hintRefs []string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT thread_id FROM app.messages
		 WHERE user_id = $1 AND thread_id IS NOT NULL
		   AND ( ($2 <> '' AND message_id_hint = ANY($3))
		      OR ($4 <> '' AND $4 = ANY(references_ids))
		      OR ($4 <> '' AND in_reply_to = $4)
		      OR ($4 <> '' AND message_id_hint = $4) )
		 ORDER BY received_at
		 LIMIT 1`,
		tx.userID.String(), joinFlag(hintRefs), hintRefs, selfID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: find thread by hints: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse thread id %q: %w", id, err)
	}
	return u, true, nil
}

// joinFlag returns "x" when refs is non-empty, "" otherwise — a cheap guard so
// the ANY($3) branch is only taken when there are references to match.
func joinFlag(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return "x"
}

// CreateThreadForMessage creates a new thread seeded with a single message's
// rollups and appends its Thread change_log row (the invariant). The caller
// inserts the message with the returned thread id in the same transaction.
func (tx *UserTx) CreateThreadForMessage(ctx context.Context, subject string, receivedAt time.Time) (ulid.ULID, error) {
	id := NewULID()
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.threads (id, user_id, subject, message_count, last_received_at)
		VALUES ($1, $2, $3, 1, $4)`,
		id.String(), tx.userID.String(), subject, receivedAt); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create thread: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectThread, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// AddMessageToThread bumps a thread's rollups for a newly attached message and
// appends its Thread change_log row.
func (tx *UserTx) AddMessageToThread(ctx context.Context, threadID ulid.ULID, receivedAt time.Time) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.threads
		   SET message_count = message_count + 1,
		       last_received_at = GREATEST(last_received_at, $3)
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), threadID.String(), receivedAt); err != nil {
		return fmt.Errorf("store: add message to thread: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectThread, threadID, OpUpdated); err != nil {
		return err
	}
	return nil
}
