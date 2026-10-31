// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// StoreAdapter satisfies MailboxStore over a *store.Store, running every call
// inside a Store.UserTx so RLS scopes each statement and the change_log
// invariant is upheld within the writing transaction (CLAUDE.md). It holds no
// per-request state; the user identity is passed to each method.
type StoreAdapter struct {
	store *store.Store
}

// NewStoreAdapter wraps a *store.Store as a MailboxStore.
func NewStoreAdapter(s *store.Store) *StoreAdapter { return &StoreAdapter{store: s} }

var _ MailboxStore = (*StoreAdapter)(nil)

// UpsertMailbox runs UserTx.UpsertMailbox and translates the result.
func (a *StoreAdapter) UpsertMailbox(ctx context.Context, userID ulid.ULID, rec MailboxRecord) (StoredMailbox, error) {
	var out StoredMailbox
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		st, err := tx.UpsertMailbox(ctx, store.MailboxUpsert{
			AccountID:   rec.AccountID,
			IMAPName:    rec.IMAPName,
			DisplayName: rec.DisplayName,
			SpecialUse:  rec.SpecialUse,
		})
		if err != nil {
			return err
		}
		out = StoredMailbox{
			ID:            st.ID,
			Created:       st.Created,
			SyncScope:     st.SyncScope,
			UIDValidity:   st.UIDValidity,
			LastSeenUID:   st.LastSeenUID,
			HighestModSeq: st.HighestModSeq,
			LastReapedAt:  st.LastReapedAt,

			PollUIDNext:        st.PollUIDNext,
			PollMessages:       st.PollMessages,
			Observed:           st.Observed,
			LastObservedFullAt: st.LastObservedFullAt,

			BackfillFloorUID: st.BackfillFloorUID,
			LabelsSweptAt:    st.LabelsSweptAt,
			LabelSweepNext:   st.LabelSweepNext,
		}
		return nil
	})
	return out, err
}

// UpdateMailboxCursors runs UserTx.UpdateMailboxCursors.
func (a *StoreAdapter) UpdateMailboxCursors(ctx context.Context, userID, mailboxID ulid.ULID, c MailboxCursors) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.UpdateMailboxCursors(ctx, mailboxID, store.MailboxCursors{
			UIDValidity:   c.UIDValidity,
			LastSeenUID:   c.LastSeenUID,
			HighestModSeq: c.HighestModSeq,
			UIDNext:       c.UIDNext,
			Messages:      c.Messages,
		})
	})
}

// MarkMailboxProvenanceStale runs UserTx.MarkMailboxProvenanceStale.
func (a *StoreAdapter) MarkMailboxProvenanceStale(ctx context.Context, userID, mailboxID ulid.ULID) (int64, error) {
	var n int64
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		n, err = tx.MarkMailboxProvenanceStale(ctx, mailboxID)
		return err
	})
	return n, err
}

// ReconcileRetiredGeneration runs UserTx.ReconcileRetiredGeneration and advances
// the mailbox's sync cursors in the SAME transaction (#88).
//
// That pairing is the whole retry story. The poll DELIBERATELY does not write the
// cursors for a mailbox whose UIDVALIDITY rolled; they are withheld until the
// retired generation has actually been reconciled. So either the reconcile lands
// — the retired rows are dropped, the membership changes get their change_log
// rows, anything left nowhere is soft-deleted, and the cursors move to the new
// generation — or none of it does, and app.mailboxes.uidvalidity still names the
// OLD generation, which is exactly how the next pass re-detects the roll and tries
// again. A crash, a network failure, or a re-fetch we could not prove complete all
// leave the roll pending rather than silently unfinished.
func (a *StoreAdapter) ReconcileRetiredGeneration(ctx context.Context, userID ulid.ULID, obs store.GenerationObservation, c MailboxCursors) (store.GenerationResult, error) {
	var res store.GenerationResult
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		if res, err = tx.ReconcileRetiredGeneration(ctx, obs); err != nil {
			return err
		}
		return tx.UpdateMailboxCursors(ctx, obs.MailboxID, store.MailboxCursors{
			UIDValidity:   c.UIDValidity,
			LastSeenUID:   c.LastSeenUID,
			HighestModSeq: c.HighestModSeq,
			UIDNext:       c.UIDNext,
			Messages:      c.Messages,
		})
	})
	if err != nil {
		return store.GenerationResult{}, err
	}
	return res, nil
}

// MissingUIDs runs UserTx.MissingUIDs — the #93 gap detector. It is a pure read
// and it is the ONE store call in this adapter that cannot change anything: its
// whole output is a list of UIDs for the engine to go and FETCH. It gets its own
// UserTx (rather than sharing the reap's) precisely because its result is acted on
// by an IMAP round-trip, and holding a database transaction open across a network
// fetch of message bodies is exactly how a pool gets exhausted.
func (a *StoreAdapter) MissingUIDs(ctx context.Context, userID ulid.ULID, obs store.GapObservation) ([]int64, error) {
	var uids []int64
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		uids, err = tx.MissingUIDs(ctx, obs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return uids, nil
}

// ReapMailbox runs UserTx.ReapMailbox and stamps the mailbox's observation cursor
// in the SAME transaction. That pairing is deliberate: the provenance deletes, the
// Email change_log rows they require (the load-bearing invariant), the soft-
// deletes, and the "this mailbox has been reconciled" cursor either all land or
// none of them do. A crash mid-reap therefore leaves the mailbox due again, and the
// reap is idempotent — re-observing the same UID set finds nothing left to drop.
//
// The observation-time baseline (seen, #620) is written by that same stamp, so it
// can only ever describe an observation whose reap actually committed.
func (a *StoreAdapter) ReapMailbox(ctx context.Context, userID ulid.ULID, obs store.ReapObservation, seen *store.ObservedStatus) (store.ReapResult, error) {
	var res store.ReapResult
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		if res, err = tx.ReapMailbox(ctx, obs); err != nil {
			return err
		}
		return tx.StampMailboxReaped(ctx, obs.MailboxID, seen)
	})
	if err != nil {
		return store.ReapResult{}, err
	}
	return res, nil
}

// StampMailboxObserved advances the observation cadence cursor alone, for a
// mailbox that was successfully observed and fully acted on but that no reap ran
// against — see MailboxStore.StampMailboxObserved.
func (a *StoreAdapter) StampMailboxObserved(ctx context.Context, userID, mailboxID ulid.ULID, seen *store.ObservedStatus) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.StampMailboxReaped(ctx, mailboxID, seen)
	})
}

// StampMailboxUnchanged advances the cadence cursor alone for a skipped
// observation — see MailboxStore.StampMailboxUnchanged.
func (a *StoreAdapter) StampMailboxUnchanged(ctx context.Context, userID, mailboxID ulid.ULID) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.StampMailboxUnchanged(ctx, mailboxID)
	})
}

// RetireCanonicalPlacements runs UserTx.RetireCanonicalPlacements.
func (a *StoreAdapter) RetireCanonicalPlacements(ctx context.Context, userID ulid.ULID) (int64, error) {
	var n int64
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		n, err = tx.RetireCanonicalPlacements(ctx)
		return err
	})
	return n, err
}

// ListAccountMailboxes runs UserTx.ListAccountMailboxes.
func (a *StoreAdapter) ListAccountMailboxes(ctx context.Context, userID, accountID ulid.ULID) ([]store.AccountMailbox, error) {
	var out []store.AccountMailbox
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		out, err = tx.ListAccountMailboxes(ctx, accountID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReconcileDeletedMailbox runs UserTx.ReconcileDeletedMailbox in its OWN UserTx,
// so one problematic folder cannot poison another's reconciliation — the same
// per-mailbox isolation ReapMailbox uses. The whole reconciliation (provenance
// drop, placement clearing, the Email change_log rows, the soft-deletes, the
// Mailbox `destroyed` row and the row delete) is one transaction: it all lands or
// none of it does, and a crash leaves the folder to be re-detected next pass.
func (a *StoreAdapter) ReconcileDeletedMailbox(ctx context.Context, userID, mailboxID ulid.ULID) (store.DeletedMailboxResult, error) {
	var res store.DeletedMailboxResult
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		res, err = tx.ReconcileDeletedMailbox(ctx, mailboxID)
		return err
	})
	if err != nil {
		return store.DeletedMailboxResult{}, err
	}
	return res, nil
}

// --- the Gmail model's store surface (#52, ADR-047; GmailStore) ---

var _ GmailStore = (*StoreAdapter)(nil)

// SetBackfillFloor runs UserTx.SetBackfillFloor.
func (a *StoreAdapter) SetBackfillFloor(ctx context.Context, userID, mailboxID ulid.ULID, floor int64) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetBackfillFloor(ctx, mailboxID, floor)
	})
}

// StampLabelSweep runs UserTx.StampLabelSweep.
func (a *StoreAdapter) StampLabelSweep(ctx context.Context, userID, mailboxID ulid.ULID, at time.Time) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.StampLabelSweep(ctx, mailboxID, at)
	})
}

// SetSyncScopeOff runs UserTx.SetMailboxSyncScope(off).
func (a *StoreAdapter) SetSyncScopeOff(ctx context.Context, userID, mailboxID ulid.ULID) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, err := tx.SetMailboxSyncScope(ctx, mailboxID, store.SyncScopeOff)
		return err
	})
}

// LiveRemoteFlags runs UserTx.LiveRemoteFlagsByUID.
func (a *StoreAdapter) LiveRemoteFlags(ctx context.Context, userID, mailboxID ulid.ULID, uidValidity int64, uids []int64) (map[int64]store.RecordedFlags, error) {
	var out map[int64]store.RecordedFlags
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		out, err = tx.LiveRemoteFlagsByUID(ctx, mailboxID, uidValidity, uids)
		return err
	})
	return out, err
}

// MailboxAcceptsKeywords runs UserTx.MailboxAcceptsKeywords (false for a
// mailbox that is not found).
func (a *StoreAdapter) MailboxAcceptsKeywords(ctx context.Context, userID, mailboxID ulid.ULID) (bool, error) {
	var ok bool
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		ok, _, err = tx.MailboxAcceptsKeywords(ctx, mailboxID)
		return err
	})
	return ok, err
}

// SetLabelSweepNext runs UserTx.SetLabelSweepNext.
func (a *StoreAdapter) SetLabelSweepNext(ctx context.Context, userID, mailboxID ulid.ULID, next *int64) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetLabelSweepNext(ctx, mailboxID, next)
	})
}

// DownloadedLast24h runs UserTx.DownloadedLast24h.
func (a *StoreAdapter) DownloadedLast24h(ctx context.Context, userID, accountID ulid.ULID, at time.Time) (int64, error) {
	var n int64
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		n, err = tx.DownloadedLast24h(ctx, accountID, at)
		return err
	})
	return n, err
}

// AddDownloadBytes runs UserTx.AddDownloadBytes.
func (a *StoreAdapter) AddDownloadBytes(ctx context.Context, userID, accountID ulid.ULID, at time.Time, n int64) (int64, error) {
	var total int64
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		total, err = tx.AddDownloadBytes(ctx, accountID, at, n)
		return err
	})
	return total, err
}
