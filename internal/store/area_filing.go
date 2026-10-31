// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// FILING EXISTING MAIL INTO AN AREA WITH A RULE (#708 item 3).
//
// A rule whose addLabel target is an area's OWN label means "file this mail in
// that area". For NEW mail that already works: ingest runs the rules before
// the desk writes its own placement (ingest.go, applyRules then
// applyAreaLabelAtIngest), and the desk only labels mail that resolved by TAB,
// so the rule's label is the one that decides.
//
// For EXISTING mail it silently did nothing. Most of it already carries a desk
// (or day-one setup) placement label, and the resolver is labels-first by
// area position (areas_resolve.go), so adding Updates' label to a message that
// carries Primary's (position 0) leaves it in Primary. The backfill reported
// "changed 40" and nothing on the desk moved.
//
// So an area-filing backfill is a SWAP, the same one the client's Re-file
// makes (web/src/core/refile.ts), with one difference that is the point of
// this file: a backfill acts on the user's BEHALF, not at their hand, so it
// may only take off labels an AUTOMATION put there.
//
//   - applied_by desk/setup, on another area's own label: removed. That is the
//     desk's (or day-one's) bookkeeping about where the message sits, and the
//     rule the user just asked for is a better answer to the same question.
//   - applied_by user: never removed. The user filed it somewhere else
//     themselves, and their own filing wins over a rule. The message is left
//     alone and REPORTED (contestedByUser).
//   - applied_by rule: never removed. Another rule disagrees, and one rule
//     silently undoing another is how rules end up fighting on the user's
//     behalf. Left alone and reported (contestedByRule).
//   - anything else that routes the message to another area — a non-own feed
//     label (a day-one folder label feeding an area is a TOPIC, the same reason
//     refile.ts keeps those), or an ml/agent row — is left alone and reported
//     (contestedOther).
//
// A contested message is left WHERE IT IS: the target label is not added
// either. Adding it would move mail away from a later-positioned area the user
// chose by hand, and "the user's own filing wins" has to hold whichever side of
// the target the other area sorts on.
//
// ⚠ NOT A SECOND WRITE PATH. Nothing here writes. The plan becomes ordinary
// EmailUpdate.RemoveLabelIDs on the ONE update the backfill already performs
// through ApplyEmailUpdate, so the change_log row, the (absent, because the
// area labels are local-only — ADR-031) writeback and the #170 correction
// logic are exactly the existing ones. Removing a desk or setup label records
// no rule_correction because recordRuleCorrection only ever fires for a row
// whose applied_by was 'rule', and this plan never removes one.

// The outcomes of filing one message into an area. Reported per outcome, never
// as one total: "40 matched" cannot tell "40 moved" from "40 you had already
// filed elsewhere", and those call for opposite reactions.
const (
	AreaFilingMove            = "move"
	AreaFilingAlreadyThere    = "alreadyThere"
	AreaFilingContestedByUser = "contestedByUser"
	AreaFilingContestedByRule = "contestedByRule"
	AreaFilingContestedOther  = "contestedOther"
)

// AreaFilingTarget is the area a rule files into, and that area's own label.
type AreaFilingTarget struct {
	AreaID  ulid.ULID
	LabelID ulid.ULID
}

// RemovedAreaLabel is one automation-applied area label the plan takes off a
// message, WITH the provenance it had — which is what lets an undo put it back
// exactly (desk stays desk, setup stays setup, so day-one's own undo, which
// deletes only rows still applied_by = 'setup', keeps working afterwards).
type RemovedAreaLabel struct {
	LabelID   ulid.ULID
	AppliedBy string
}

// AreaFilingStep is the plan for one message.
type AreaFilingStep struct {
	Outcome string
	// Remove is set for move and alreadyThere only: the other areas' own labels
	// on this message that an automation applied.
	Remove []RemovedAreaLabel
}

// AreaFilingTargetFor reports whether exactly one of labelIDs is an area's own
// label that feeds that area — the definition of "this rule files into an
// area". Two area labels in one rule have no single answer, so the rule is
// treated as an ordinary labelling rule rather than guessed at.
func (tx *UserTx) AreaFilingTargetFor(ctx context.Context, labelIDs []ulid.ULID) (AreaFilingTarget, bool, error) {
	if len(labelIDs) == 0 {
		return AreaFilingTarget{}, false, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT a.id, a.label_id
		  FROM app.areas a
		  JOIN app.area_feeds f
		    ON f.user_id = a.user_id AND f.area_id = a.id AND f.label_id = a.label_id
		 WHERE a.user_id = $1 AND a.label_id = ANY($2)`,
		tx.userID.String(), ulidStrings(labelIDs))
	if err != nil {
		return AreaFilingTarget{}, false, fmt.Errorf("store: area filing target: %w", err)
	}
	defer rows.Close()
	var found []AreaFilingTarget
	for rows.Next() {
		var aid, lid string
		if err := rows.Scan(&aid, &lid); err != nil {
			return AreaFilingTarget{}, false, fmt.Errorf("store: scan area filing target: %w", err)
		}
		a, err := ulid.Parse(aid)
		if err != nil {
			return AreaFilingTarget{}, false, fmt.Errorf("store: area id %q: %w", aid, err)
		}
		l, err := ulid.Parse(lid)
		if err != nil {
			return AreaFilingTarget{}, false, fmt.Errorf("store: label id %q: %w", lid, err)
		}
		found = append(found, AreaFilingTarget{AreaID: a, LabelID: l})
	}
	if err := rows.Err(); err != nil {
		return AreaFilingTarget{}, false, fmt.Errorf("store: area filing target: %w", err)
	}
	if len(found) != 1 {
		return AreaFilingTarget{}, false, nil
	}
	return found[0], true, nil
}

// PlanAreaFiling decides, per message, what filing it into target means — see
// the file comment for the rules. Read-only: Rule/preview reports it and
// Rule/apply performs it, from the SAME function, so the preview cannot
// disagree with the apply it predicts (#193's lesson).
//
// Every id in msgIDs that exists gets a step. "Already there" is the message's
// CURRENT resolved area (ResolveAreas, the one resolver Email/query and the
// desk use) being the target; anything else uncontested is a move, because
// once every other area's label is gone the target's own label wins the
// labels-first resolution outright.
func (tx *UserTx) PlanAreaFiling(
	ctx context.Context,
	target AreaFilingTarget,
	msgIDs []ulid.ULID,
) (map[ulid.ULID]AreaFilingStep, error) {
	out := make(map[ulid.ULID]AreaFilingStep, len(msgIDs))
	if len(msgIDs) == 0 {
		return out, nil
	}
	before, err := tx.ResolveAreas(ctx, msgIDs)
	if err != nil {
		return nil, err
	}

	// Every label on these messages that is SOME area's own label or feeds SOME
	// area, with who applied it. A label can be an own label of at most one area
	// (areas UNIQUE (user_id, label_id)) and feed at most one (area_feeds_label_once),
	// so this is at most one row per (message, label).
	rows, err := tx.tx.Query(ctx, `
		SELECT ml.message_id, ml.label_id, ml.applied_by, own.id, f.area_id
		  FROM app.message_labels ml
		  LEFT JOIN app.areas own
		         ON own.user_id = ml.user_id AND own.label_id = ml.label_id
		  LEFT JOIN app.area_feeds f
		         ON f.user_id = ml.user_id AND f.label_id = ml.label_id
		 WHERE ml.user_id = $1 AND ml.message_id = ANY($2)
		   AND (own.id IS NOT NULL OR f.area_id IS NOT NULL)`,
		tx.userID.String(), ulidStrings(msgIDs))
	if err != nil {
		return nil, fmt.Errorf("store: plan area filing: %w", err)
	}
	defer rows.Close()

	type blockers struct{ user, rule, other bool }
	remove := map[ulid.ULID][]RemovedAreaLabel{}
	blocked := map[ulid.ULID]*blockers{}
	targetArea := target.AreaID.String()
	for rows.Next() {
		var (
			mid, lid, by string
			ownArea      *string
			feedArea     *string
		)
		if err := rows.Scan(&mid, &lid, &by, &ownArea, &feedArea); err != nil {
			return nil, fmt.Errorf("store: scan area filing label: %w", err)
		}
		otherOwn := ownArea != nil && *ownArea != targetArea
		otherFeed := feedArea != nil && *feedArea != targetArea
		if !otherOwn && !otherFeed {
			continue // the target's own label, or one feeding the target
		}
		m, err := ulid.Parse(mid)
		if err != nil {
			return nil, fmt.Errorf("store: area filing message id %q: %w", mid, err)
		}
		l, err := ulid.Parse(lid)
		if err != nil {
			return nil, fmt.Errorf("store: area filing label id %q: %w", lid, err)
		}
		// Removable: another area's OWN label (not a topic label that happens to
		// feed one), routing nowhere but that area, put there by an automation.
		if otherOwn && (feedArea == nil || *feedArea == *ownArea) &&
			(by == LabelByDesk || by == LabelBySetup) {
			remove[m] = append(remove[m], RemovedAreaLabel{LabelID: l, AppliedBy: by})
			continue
		}
		b := blocked[m]
		if b == nil {
			b = &blockers{}
			blocked[m] = b
		}
		switch by {
		case LabelByUser:
			b.user = true
		case LabelByRule:
			b.rule = true
		default:
			b.other = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: plan area filing: %w", err)
	}

	for _, id := range msgIDs {
		res, exists := before[id]
		if !exists {
			continue // no such message (or not this user's): no step, as ApplyEmailUpdate would find nothing
		}
		// The user's own filing outranks everything, then another rule's: the
		// report names the strongest reason the message stayed put.
		if b := blocked[id]; b != nil {
			switch {
			case b.user:
				out[id] = AreaFilingStep{Outcome: AreaFilingContestedByUser}
			case b.rule:
				out[id] = AreaFilingStep{Outcome: AreaFilingContestedByRule}
			default:
				out[id] = AreaFilingStep{Outcome: AreaFilingContestedOther}
			}
			continue
		}
		step := AreaFilingStep{Outcome: AreaFilingMove, Remove: remove[id]}
		if res.AreaID != nil && *res.AreaID == target.AreaID {
			step.Outcome = AreaFilingAlreadyThere
		}
		out[id] = step
	}
	return out, nil
}

// AreaLabelRestorable reports which of labelIDs are SOME area's own label —
// the only labels an area-filing undo may put back (Rule/apply undo). The undo
// receives the removed labels from the client (the server keeps no memory of
// an operation, the same reason Rule/apply's undo takes explicit message ids),
// so this is the check that keeps it from re-adding an arbitrary label under a
// desk attribution.
func (tx *UserTx) AreaLabelRestorable(ctx context.Context, labelIDs []ulid.ULID) (map[ulid.ULID]bool, error) {
	out := map[ulid.ULID]bool{}
	if len(labelIDs) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx,
		`SELECT label_id FROM app.areas WHERE user_id = $1 AND label_id = ANY($2)`,
		tx.userID.String(), ulidStrings(labelIDs))
	if err != nil {
		return nil, fmt.Errorf("store: area label restorable: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: scan area label: %w", err)
		}
		l, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: area label id %q: %w", s, err)
		}
		out[l] = true
	}
	return out, rows.Err()
}
