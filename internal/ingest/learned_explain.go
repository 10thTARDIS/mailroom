// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// SenderFacts is what a message's headers say about its sender, as learned
// placement reads them, for `mailroom-admin placement-explain` (#840). It is
// the raw material behind the placement.Sender, shown, never decided on.
type SenderFacts struct {
	// StoredFrom is messages.from_address, the value the keys come from.
	StoredFrom string
	// FromFields is the raw count of From header fields (mime FieldCount),
	// FromValues the non-empty ones, Mailboxes the mailboxes parsed from them.
	FromFields int
	FromValues []string
	Mailboxes  []string
	// TrustedIDs are the authserv-ids trusted for THIS message (ml.TrustedAt:
	// a date-bounded entry is judged at the message's trust time).
	TrustedIDs []string
	// AuthHeaders are the Authentication-Results headers, topmost first.
	AuthHeaders []ml.AuthHeaderExplain
}

// LearnedExplanation is one message's arrival decision re-run read-only.
type LearnedExplanation struct {
	Facts SenderFacts
	Trace store.LearnedTrace
}

// ExplainLearnedRaw re-runs the arrival decision for a stored message from its
// raw bytes: the sender is built by LearnedSender, the very function ingest
// calls, and decided by store.ExplainLearnedPlacement, which runs the very
// function the live path runs. tx must be a Store.UserReadTx transaction.
// trustTime is the message's trust time (store.ExplainCandidate.ReceivedAt);
// asOf is zero for "now".
func ExplainLearnedRaw(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, raw []byte, storedFrom string,
	trusted []string, trustTime, asOf time.Time) (LearnedExplanation, error) {
	parsed, err := mime.ParseBytes(raw)
	if err != nil || parsed == nil {
		return LearnedExplanation{}, fmt.Errorf("%w: parse: %v", ErrUnreadableMessage, err)
	}
	sender := LearnedSender(storedFrom, parsed, trusted, trustTime)
	facts := SenderFacts{StoredFrom: storedFrom, FromFields: parsed.Header.FieldCount("From"), FromValues: parsed.Header.Values("From")}
	for _, a := range parsed.Header.From() {
		facts.Mailboxes = append(facts.Mailboxes, a.Address)
	}
	facts.TrustedIDs = ml.TrustedAt(trusted, trustTime)
	facts.AuthHeaders = ml.ExplainAuthHeaders(parsed.Header.Values("Authentication-Results"), facts.TrustedIDs)
	tr, err := tx.ExplainLearnedPlacement(ctx, msgID, sender, asOf)
	if err != nil {
		return LearnedExplanation{}, err
	}
	return LearnedExplanation{Facts: facts, Trace: tr}, nil
}

// ExplainLearned is ExplainLearnedRaw for a message whose bytes are in the blob
// store: they are read through the same blob reader the backfill uses.
func ExplainLearned(ctx context.Context, tx *store.UserTx, blobs blob.BlobStore, userID ulid.ULID, dek []byte,
	trusted []string, c store.ExplainCandidate, asOf time.Time) (LearnedExplanation, error) {
	raw, err := readCandidateBytes(ctx, blobs, userID, dek, c.ContentHash)
	if err != nil {
		return LearnedExplanation{}, err
	}
	return ExplainLearnedRaw(ctx, tx, c.MessageID, raw, c.From, trusted, c.ReceivedAt, asOf)
}
