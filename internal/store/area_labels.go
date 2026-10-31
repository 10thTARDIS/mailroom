// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// AREAS ARE LABELS (#442 increment 3): placing a message in an area applies
// the area's OWN label. Increments 1-2 made a message's area DERIVED —
// labels first, else the tab verdict (areas_resolve.go) — so a message that
// lands in an area only because of its TAB VERDICT carries nothing a user can
// see, search (`label:Updates`), or keep when the classifier changes its
// mind. This file is the ONE write path that closes that gap: the ingest
// entry (ApplyAreaLabelForNewMessage) and the report-first admin backfill
// (MaterializeAreaLabel, mailroom-admin materialize-area-labels) both call
// the same unexported applyAreaLabel, exactly the split applyRuleAction
// (rules.go, ingest) and BackfillUpdate (rule_backfill.go, existing mail)
// already draw for a rule's own labels — "never a second write path".
//
// ONLY TAB-DECIDED MESSAGES GET THE LABEL. A message a label already routes
// into an area is already written down — the label IS the reason it landed
// there — so writing the area's own label on top of it would risk flipping a
// future tie: areaJoinSQL/internal/areas.Resolve resolve labels-first, by
// area POSITION, and a message newly carrying two areas' own labels is
// exactly the ambiguity Resolution.Contested exists to name, not something
// this backfill should manufacture.

// applyAreaLabel resolves msgID's area (ResolveAreas, the SQL twin of
// internal/areas.Resolve — areas_resolve.go) and, when it resolved BY TAB,
// inserts the area's own label with applied_by = LabelByDesk. ON CONFLICT
// (user_id, message_id, label_id) DO NOTHING keeps whatever attribution is
// already there — the user's, a rule's — exactly as applyRuleAction's own
// INSERT explains: an existing row's provenance is never overwritten by an
// automation arriving after it.
//
// Not applied when: the message Needs sorting (AreaID nil), it resolved BY
// LABEL (already written down), or the row already existed. None of those is
// an error — applied is simply false, and the two exported wrappers below
// decide what "nothing happened" means for their own change_log obligations.
func (tx *UserTx) applyAreaLabel(ctx context.Context, msgID ulid.ULID) (areaID *ulid.ULID, applied bool, err error) {
	res, err := tx.ResolveAreas(ctx, []ulid.ULID{msgID})
	if err != nil {
		return nil, false, err
	}
	r, ok := res[msgID]
	if !ok || r.AreaID == nil || r.ByLabel {
		return r.AreaID, false, nil
	}

	applied, err = tx.insertAreaOwnLabel(ctx, msgID, *r.AreaID, DeskLabelSource())
	return r.AreaID, applied, err
}

// insertAreaOwnLabel writes area areaID's OWN label onto msgID with the given
// attribution: the one INSERT both the desk (applyAreaLabel, LabelByDesk) and
// learned placement (learned_placement.go, LabelByLearned) place mail
// through, so "placing a message in an area" has one write path whoever
// decided it. ON CONFLICT DO NOTHING keeps an existing row's attribution.
// It appends no change_log row: each caller states its own obligation.
func (tx *UserTx) insertAreaOwnLabel(ctx context.Context, msgID, areaID ulid.ULID, src LabelSource) (applied bool, err error) {
	var ownLabelIDStr string
	err = tx.tx.QueryRow(ctx,
		`SELECT label_id FROM app.areas WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), areaID.String()).Scan(&ownLabelIDStr)
	if err != nil {
		if isNoRows(err) {
			// Unreachable through ResolveAreas, which only ever names an area id
			// drawn from the caller's OWN area_feeds rows — but refusing loudly
			// here beats silently skipping the label if that ever stops being
			// true (CLAUDE.md: the absence of an error proves nothing on its own).
			return false, fmt.Errorf("store: apply area label: area %s resolved but not found", areaID)
		}
		return false, fmt.Errorf("store: apply area label (read own label): %w", err)
	}
	ownLabelID, err := ulid.Parse(ownLabelIDStr)
	if err != nil {
		return false, fmt.Errorf("store: apply area label: parse own label id %q: %w", ownLabelIDStr, err)
	}

	appliedBy, ruleID := src.columns()
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, message_id, label_id) DO NOTHING`,
		tx.userID.String(), msgID.String(), ownLabelID.String(), appliedBy, ruleID)
	if err != nil {
		return false, fmt.Errorf("store: apply area label: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ApplyAreaLabelForNewMessage is the INGEST entry: the identical write, with
// NO change_log row of its own — the ingest transaction appends the Email
// `created` row for this message immediately after (internal/ingest/ingest.go),
// and that row already covers the labelled state, exactly how a rule's
// ingest-time addLabel rides the same row rather than appending a second one
// (applyRuleAction never calls AppendChange either).
//
// ⚠ CALLING THIS OUTSIDE AN INGEST TRANSACTION THAT WILL APPEND THAT ROW WOULD
// BREAK THE LOAD-BEARING INVARIANT (CLAUDE.md): the label would land with
// nothing in change_log to announce it, so /changes would never mention it and
// a client's area counts would go stale until something else happened to
// touch the same message. MaterializeAreaLabel exists for every other caller
// for exactly this reason — do not reach for this one outside ingest.
func (tx *UserTx) ApplyAreaLabelForNewMessage(ctx context.Context, msgID ulid.ULID) (applied bool, err error) {
	_, applied, err = tx.applyAreaLabel(ctx, msgID)
	return applied, err
}

// MaterializeAreaLabel is the BACKFILL entry (mailroom-admin
// materialize-area-labels): the same write, plus an Email `updated` change row
// when — and only when — a row was actually inserted, so every client learns
// the label appeared on mail that was already sitting in a mailbox before this
// increment existed.
//
// ⚠ NO IMAP WRITEBACK IS ENQUEUED, deliberately, even for an own label with
// mirror_to_imap set. The desk's placement is Mailroom's own bookkeeping, not
// an action the user took; a backfill that silently queued hundreds of
// keyword writes to the IMAP server would be exactly the side effect "build
// the report before the fix" (CLAUDE.md) exists to prevent. The label mirrors
// out later if and when the user acts on the message, the same deferral an
// ingest-time rule label already relies on. The admin report says how many
// labels this run affects before --apply ever runs.
func (tx *UserTx) MaterializeAreaLabel(ctx context.Context, msgID ulid.ULID) (areaID *ulid.ULID, applied bool, err error) {
	areaID, applied, err = tx.applyAreaLabel(ctx, msgID)
	if err != nil || !applied {
		return areaID, applied, err
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, msgID, OpUpdated); err != nil {
		return areaID, applied, err
	}
	return areaID, applied, nil
}
