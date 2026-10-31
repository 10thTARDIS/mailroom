// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// SessionFactory opens an authenticated SyncSession for cfg. The production
// value is sync.Dial; tests inject a factory returning a fake session. It is a
// seam, not an interface, because there is exactly one operation.
type SessionFactory func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error)

// Ingester canonicalizes one raw fetched message. It is the single-method
// projection of *ingest.Ingestor the sync loop needs, so the loop can be
// exercised with a fake that records calls instead of touching the blob store
// and database.
type Ingester interface {
	Ingest(ctx context.Context, userID ulid.ULID, dek []byte, dekID ulid.ULID, raw []byte, prov ingest.Provenance) (ingest.Result, error)
	// RefreshFlags is the flags-only projection of the same ingestor (#523): it
	// applies a server flag observation to a message ALREADY living at prov's
	// (mailbox, uidvalidity, uid), with no body, parse, scan, or blob access.
	// The CONDSTORE flag-reconcile pass uses this instead of Ingest so a change
	// no client made to anything but flags does not re-download and re-scan
	// the whole message.
	RefreshFlags(ctx context.Context, userID ulid.ULID, prov ingest.Provenance) (ingest.FlagRefresh, error)
}

// MailboxRecord is the server-discovered metadata for one mailbox, passed to
// MailboxStore.UpsertMailbox.
type MailboxRecord struct {
	AccountID   ulid.ULID
	IMAPName    string
	DisplayName string
	SpecialUse  string // RFC 6154 attribute (e.g. `\Sent`), or "" if none
}

// StoredMailbox is a mailbox's persisted state after an upsert: its id, the
// preserved sync scope, and the cursors the engine reconciles against.
type StoredMailbox struct {
	ID ulid.ULID
	// Created: this upsert inserted the row — the folder's first sighting.
	Created       bool
	SyncScope     string
	UIDValidity   int64
	LastSeenUID   int64
	HighestModSeq int64
	// LastReapedAt is when the expunge reaper last reconciled this mailbox
	// against the server's live UID set; nil = never. It drives the reaper's
	// cadence (see Engine's reap pass).
	LastReapedAt *time.Time

	// PollUIDNext and PollMessages are UIDNEXT and MESSAGES from the last
	// poll's own SELECT; nil = unknown. With UIDValidity and HighestModSeq they
	// are the POLL-TIME baseline phase 1 compares LIST-STATUS against (#620).
	PollUIDNext  *int64
	PollMessages *int64

	// Observed is the status the last REAL live-UID observation saw, nil when
	// unknown, and LastObservedFullAt is when it ran. They are the
	// OBSERVATION-TIME baseline phase 2 compares LIST-STATUS against — never the
	// poll-time one above (see observationUnchanged).
	Observed           *store.ObservedStatus
	LastObservedFullAt *time.Time

	// BackfillFloorUID and LabelsSweptAt are the Gmail read path's
	// bookkeeping (#52, ADR-047): the newest-first import's floor (nil = no
	// floor) and when the X-GM-LABELS sweep last ran (nil = never). See
	// gmail.go.
	BackfillFloorUID *int64
	LabelsSweptAt    *time.Time
	// LabelSweepNext is where an interrupted label sweep resumes; nil = no
	// sweep in progress.
	LabelSweepNext *int64
}

// MailboxCursors is the sync-derived state persisted after reconciling a
// mailbox. None of it is JMAP-visible, so the store writes it without a
// change_log row.
type MailboxCursors struct {
	UIDValidity   int64
	LastSeenUID   int64
	HighestModSeq int64
	// No AcceptsKeywords: an EXAMINE cannot observe it (#740); see
	// store.UserTx.RecordKeywordProbes.
	// UIDNext and Messages come from the same SELECT as the fields above: the
	// rest of the poll-time baseline (#620).
	UIDNext  int64
	Messages int64
}

// MailboxStore is the store surface the sync loop needs. It is satisfied by
// StoreAdapter, a thin wrapper that runs each call inside a Store.UserTx (the
// only legitimate path to the database — CLAUDE.md). Every method takes the
// user identity explicitly, mirroring ingest.Ingestor.Ingest.
type MailboxStore interface {
	// UpsertMailbox records a discovered mailbox and returns its persisted
	// state, including the sync cursors to reconcile against.
	UpsertMailbox(ctx context.Context, userID ulid.ULID, rec MailboxRecord) (StoredMailbox, error)
	// UpdateMailboxCursors advances a mailbox's sync cursors after a reconcile.
	UpdateMailboxCursors(ctx context.Context, userID, mailboxID ulid.ULID, c MailboxCursors) error
	// MarkMailboxProvenanceStale retires a mailbox's live provenance generation
	// after a UIDVALIDITY reset, returning the number of rows retired.
	MarkMailboxProvenanceStale(ctx context.Context, userID, mailboxID ulid.ULID) (int64, error)

	// ReconcileRetiredGeneration finishes a UIDVALIDITY roll (#88), once the
	// mailbox has been re-fetched under its new generation: it proves the
	// re-fetch complete against obs, drops the retired provenance rows, appends
	// the Email change_log rows the resulting membership changes require, and
	// soft-deletes any message the roll left in no location at all — then
	// advances the mailbox's cursors, ALL in one transaction.
	//
	// The cursors are passed in here, rather than written by the poll, precisely
	// so that "the roll is finished" and "the cursors say we are on the new
	// generation" are the same commit. A reconcile that cannot be justified
	// leaves the cursors on the OLD generation, so the next pass re-detects the
	// roll and retries the whole thing rather than forgetting it forever.
	ReconcileRetiredGeneration(ctx context.Context, userID ulid.ULID, obs store.GenerationObservation, c MailboxCursors) (store.GenerationResult, error)

	// MissingUIDs asks the OTHER question about the same live UID set the reaper
	// diffs (#93): which UIDs does the SERVER hold, below the bound our poll
	// claims to have covered, that we hold no live provenance row for? Each one
	// is a message a silently-truncated FETCH dropped and that the poll's
	// last-seen cursor has since moved past — so nothing will ever ask for it
	// again. The answer is a list of UIDs to re-fetch. It is read-only: it can
	// never conclude anything is absent, and nothing it returns is fed to
	// ReapMailbox.
	MissingUIDs(ctx context.Context, userID ulid.ULID, obs store.GapObservation) ([]int64, error)

	// ReapMailbox reconciles one mailbox's provenance against the live UID set
	// the server reported (store.ReapMailbox): it drops the physical locations
	// the server no longer has, appends the Email change_log rows the resulting
	// membership changes require, and soft-deletes anything left in no location
	// at all — all in one transaction. It also stamps the mailbox's observation
	// cursor, so ONLY a reap that actually completed advances the cadence — and,
	// in the same transaction, records seen as the observation-time baseline
	// (#620; nil = this observation established none).
	ReapMailbox(ctx context.Context, userID ulid.ULID, obs store.ReapObservation, seen *store.ObservedStatus) (store.ReapResult, error)

	// StampMailboxObserved advances the mailbox's observation cadence cursor
	// (app.mailboxes.last_reaped_at) on its own — the path taken when the live
	// UID set was read and fully acted on but no reap ran: either the reaper is
	// disabled (WithReapInterval(0), which switches off only the DESTRUCTIVE half
	// of the observation, never the gap backfill) or the observation could not
	// prove anything absent. When a reap DOES run, ReapMailbox stamps it in the
	// same transaction instead, so the reap and its cursor land together. Like
	// ReapMailbox it records seen and the real-observation time (#620).
	StampMailboxObserved(ctx context.Context, userID, mailboxID ulid.ULID, seen *store.ObservedStatus) error

	// StampMailboxUnchanged advances ONLY the cadence cursor, for a due mailbox
	// whose observation was SKIPPED because LIST-STATUS showed nothing changed
	// since the last real one (#620). It never touches the observation-time
	// baseline or its timestamp — that is what bounds skipping to 24 hours.
	StampMailboxUnchanged(ctx context.Context, userID, mailboxID ulid.ULID) error

	// RetireCanonicalPlacements clears app.messages.moved_mailbox_id for every
	// message whose live provenance now proves the override redundant, returning
	// how many it retired. See store.UserTx.RetireCanonicalPlacements for why it
	// cannot make a message flicker back into its old folder.
	RetireCanonicalPlacements(ctx context.Context, userID ulid.ULID) (int64, error)

	// ListAccountMailboxes returns every mailbox Mailroom holds for the account —
	// the set the folder-deletion reconciler (#87) diffs against the server's live
	// LIST. A name we hold that a SUCCESSFUL LIST no longer reports is evidence the
	// folder was deleted on the server.
	ListAccountMailboxes(ctx context.Context, userID, accountID ulid.ULID) ([]store.AccountMailbox, error)

	// ReconcileDeletedMailbox reconciles a folder the server no longer lists (#87)
	// and destroys it, in one transaction: it drops the folder's provenance, clears
	// every canonical/local placement that names it (so the mailbox row's ON DELETE
	// SET NULL cannot silently undo a move — the #84 landmine), appends the Email
	// change_log rows the resulting membership changes require, soft-deletes any
	// message left in no location at all, appends the Mailbox `destroyed` row, and
	// deletes the mailbox row. It is only ever called for a folder a TRUSTWORTHY LIST
	// no longer names — the fail-closed gate lives in the engine.
	ReconcileDeletedMailbox(ctx context.Context, userID, mailboxID ulid.ULID) (store.DeletedMailboxResult, error)

	// LiveRemoteFlags returns, for each of uids that has a LIVE provenance row
	// in the mailbox at uidValidity, the remote flags last recorded for it and
	// whether a flag writeback for its message is still owed — what the
	// CONDSTORE-less flag sweep compares a server reading against (ADR-046
	// §12). Read-only. A UID with no live row is absent from the map.
	LiveRemoteFlags(ctx context.Context, userID, mailboxID ulid.ULID, uidValidity int64, uids []int64) (map[int64]store.RecordedFlags, error)

	// MailboxAcceptsKeywords reports the recorded PERMANENTFLAGS `\*` probe
	// for a mailbox (app.mailboxes.accepts_keywords; false = no or unknown).
	MailboxAcceptsKeywords(ctx context.Context, userID, mailboxID ulid.ULID) (bool, error)

	// SetSyncScopeOff marks a mailbox not synced, with its Mailbox change row
	// (store.UserTx.SetMailboxSyncScope). The Microsoft profile uses it for a
	// non-mail folder (Calendar, Contacts, …) the first time it is listed.
	SetSyncScopeOff(ctx context.Context, userID, mailboxID ulid.ULID) error
}
