// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"

	"mailroom/internal/store"

	"github.com/oklog/ulid/v2"
)

// StoreIntents adapts *store.Store to the engine's IntentStore interface for a
// single user. Each method opens its own short UserTx, so the engine never
// holds a database transaction open across a network IMAP round-trip: claim in
// one transaction, apply via IMAP with no transaction held, then mark the
// outcome in another. RLS scopes every statement to userID (fail closed).
type StoreIntents struct {
	store  *store.Store
	userID ulid.ULID
}

var _ IntentStore = (*StoreIntents)(nil)

// NewStoreIntents builds an IntentStore bound to one user's identity.
func NewStoreIntents(s *store.Store, userID ulid.ULID) *StoreIntents {
	return &StoreIntents{store: s, userID: userID}
}

// ClaimPendingIntents claims a batch of pending intents in one transaction.
func (s *StoreIntents) ClaimPendingIntents(ctx context.Context, accountID ulid.ULID, limit int) ([]store.Intent, error) {
	var out []store.Intent
	err := s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		var err error
		out, err = tx.ClaimPendingIntents(ctx, accountID, limit)
		return err
	})
	return out, err
}

// MessageProvenanceTargetsForAccount reads EVERY live copy of a message in an
// account — one message can have several, and reaching only one of them is what
// made removals revert on the next sync (#296).
func (s *StoreIntents) MessageProvenanceTargetsForAccount(ctx context.Context, messageID, accountID ulid.ULID) ([]store.ProvenanceTarget, error) {
	var targets []store.ProvenanceTarget
	err := s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		var err error
		targets, err = tx.MessageProvenanceTargetsForAccount(ctx, messageID, accountID)
		return err
	})
	return targets, err
}

// MarkIntentApplied transitions one intent inflight->applied and reports
// whether a row moved.
func (s *StoreIntents) MarkIntentApplied(ctx context.Context, id ulid.ULID) (bool, error) {
	var moved bool
	err := s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		var err error
		moved, err = tx.MarkIntentApplied(ctx, id)
		return err
	})
	return moved, err
}

// MarkIntentFailed retries or dead-letters one intent and reports whether a
// row moved.
func (s *StoreIntents) MarkIntentFailed(ctx context.Context, id ulid.ULID, errMsg, deadReason string) (bool, error) {
	var moved bool
	err := s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		var err error
		moved, err = tx.MarkIntentFailed(ctx, id, errMsg, deadReason)
		return err
	})
	return moved, err
}

// PurgeExpungeStillEligible re-runs the purge sweep's own eligibility predicate
// at apply time (#555), in its own short transaction like every other method
// here.
func (s *StoreIntents) PurgeExpungeStillEligible(ctx context.Context, messageID, accountID ulid.ULID) (bool, store.ExpungeSkip, error) {
	var (
		eligible bool
		skip     store.ExpungeSkip
	)
	err := s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		var err error
		eligible, skip, err = tx.PurgeExpungeStillEligible(ctx, messageID, accountID)
		return err
	})
	return eligible, skip, err
}

// WithdrawIntent drops a claimed intent that must not be applied after all.
func (s *StoreIntents) WithdrawIntent(ctx context.Context, id ulid.ULID) error {
	return s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		return tx.WithdrawIntent(ctx, id)
	})
}

// RecordGmailLabels moves one live Gmail copy's gm_labels baseline by a
// confirmed label delta (#52, ADR-047 §3).
func (s *StoreIntents) RecordGmailLabels(ctx context.Context, provenanceID ulid.ULID, add, remove []string) error {
	return s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		_, err := tx.RecordGmailLabelDelta(ctx, provenanceID, add, remove)
		return err
	})
}

// RecordStoredFlags records an acknowledged STORE on one live provenance row
// (#576).
func (s *StoreIntents) RecordStoredFlags(ctx context.Context, provenanceID ulid.ULID, add, remove []string) error {
	return s.store.UserTx(ctx, s.userID, func(tx *store.UserTx) error {
		_, err := tx.ApplyStoredFlagDelta(ctx, provenanceID, add, remove)
		return err
	})
}
