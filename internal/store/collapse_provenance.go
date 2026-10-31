// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// WHAT PAST COLLAPSES DID TO LABEL PROVENANCE (#859).
//
// Before #859, CollapseDuplicateGroup copied a loser's labels onto the keeper
// WITHOUT applied_by, so each such row fell to the column default, 'user', and
// was stamped created_at = now(). The loser itself was then deleted, and with it
// the only record of who really applied the label. This file reads what evidence
// survived, and repairs only what that evidence settles.
//
// THE EVIDENCE, AND HOW FAR EACH KIND REACHES:
//
//  1. RULE-ID CONTRADICTION. applied_by = 'user' with applied_by_rule_id set.
//     Every writer of app.message_labels except the pre-#859 collapse goes
//     through LabelSource.columns, which writes a rule id ONLY beside 'rule'
//     (label_source.go), and the old collapse copied the rule id while dropping
//     applied_by. So this pair can only be a rule's row that a collapse turned
//     into a 'user' one: the original attribution is known exactly, and it is
//     the one kind this file repairs. It does not depend on change_log history,
//     so it reaches back to the first collapse ever run — EXCEPT where the rule
//     has since been deleted: the FK is ON DELETE SET NULL, the id is gone, and
//     the row is indistinguishable from a real filing.
//
//  2. A COLLAPSE TRANSACTION IN change_log. A collapse appends, in one
//     transaction, Email `destroyed` for each loser and Email `updated` for the
//     keeper; change_log.created_at and the unioned label's created_at are both
//     now(), i.e. the SAME transaction timestamp. A keeper label row whose
//     created_at equals such a transaction's timestamp was written by it. That
//     says the row came from a loser — not who applied it on the loser, which
//     may well have been the user. So these rows are COUNTED, never repaired.
//     It is an upper bound: any other transaction that both labelled a message
//     and hard-deleted a different one would match too. And it reaches only as
//     far back as change_log does — compaction (changelog_compact.go) deletes
//     history older than the retention window — so a collapse before the oldest
//     retained Email row is invisible to it. Collapses after #859 never match:
//     they carry the loser's own created_at.
//
//  NOT EVIDENCE, AND WHY: before #882 setup_journal and learned_placements
//  referenced the loser ON DELETE CASCADE only, so the record that the setup or
//  learned placement touched that copy was deleted with it (collapses since #882
//  re-point both to the keeper, but older ones are unrecoverable). feedback_events was re-pointed but
//  says nothing about labels. There is no collapse journal.

// CollapseProvenanceRow is one label row the repair would re-attribute.
type CollapseProvenanceRow struct {
	MessageID string
	LabelID   string
	RuleID    string
	CreatedAt time.Time
	// InCollapseTx: evidence 2 also places this row in a collapse transaction
	// still visible in change_log. Not required for the repair (evidence 1 is
	// sufficient on its own); shown so the operator can see both agree.
	InCollapseTx bool
}

// CollapseProvenanceReport is what the read-only report found. Every count is
// per evidence kind and per applied_by bucket, never one total: "nothing to
// repair" and "nothing visible" must not print the same number.
type CollapseProvenanceReport struct {
	// HORIZON of evidence 2.
	EmailFloorSeq      int64      // app.change_floor for Email; 0 = never compacted
	OldestEmailChange  *time.Time // oldest retained Email change_log row; nil = none
	CollapseTxs        int        // distinct collapse transactions still visible
	FirstCollapseTx    *time.Time
	LastCollapseTx     *time.Time
	CollapseKeepers    int // distinct keepers across those transactions
	CollapseLabelsByBy map[string]int
	// Evidence 1, per applied_by. Only the 'user' bucket is a collapse
	// mis-attribution (and repairable); any other non-'rule' bucket with a rule
	// id is an anomaly with no known cause, reported and left alone.
	RuleIDByAppliedBy map[string]int
	// The 'user' + rule-id rows themselves: what --apply would change.
	Repairable []CollapseProvenanceRow
	// The BOUND. 'user' rows with no rule id: how many there are in total, and
	// how many predate the oldest retained Email change row (where evidence 2
	// cannot look at all). A past collapse's mis-attribution of a non-rule label
	// hides somewhere inside these, and nothing left in the database can say
	// which.
	UserRowsNoRule              int
	UserRowsNoRuleBeforeHorizon int
}

// collapseTxCTE finds the collapse transactions still in change_log: an Email
// `updated` row (the keeper) sharing its exact created_at with an Email
// `destroyed` row for a DIFFERENT message that no longer exists (a loser).
const collapseTxCTE = `
	WITH destroyed AS (
	  SELECT DISTINCT d.created_at, d.object_id
	    FROM app.change_log d
	   WHERE d.user_id = $1 AND d.object_type = 'Email' AND d.op = 'destroyed'
	     AND NOT EXISTS (SELECT 1 FROM app.messages m
	                      WHERE m.user_id = $1 AND m.id = d.object_id)
	),
	ctx AS (
	  SELECT DISTINCT u.created_at, u.object_id AS keeper_id
	    FROM app.change_log u
	    JOIN destroyed d ON d.created_at = u.created_at AND d.object_id <> u.object_id
	   WHERE u.user_id = $1 AND u.object_type = 'Email' AND u.op = 'updated'
	)`

// CollapseProvenanceReport reads the evidence. READ-ONLY.
func (tx *UserTx) CollapseProvenanceReport(ctx context.Context) (CollapseProvenanceReport, error) {
	r := CollapseProvenanceReport{
		CollapseLabelsByBy: map[string]int{},
		RuleIDByAppliedBy:  map[string]int{},
	}
	uid := tx.userID.String()

	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT floor_seq FROM app.change_floor
		                  WHERE user_id = $1 AND object_type = 'Email'), 0),
		       (SELECT min(created_at) FROM app.change_log
		         WHERE user_id = $1 AND object_type = 'Email')`,
		uid).Scan(&r.EmailFloorSeq, &r.OldestEmailChange); err != nil {
		return r, fmt.Errorf("store: collapse provenance (horizon): %w", err)
	}

	if err := tx.tx.QueryRow(ctx, collapseTxCTE+`
		SELECT count(DISTINCT created_at), min(created_at), max(created_at), count(DISTINCT keeper_id)
		  FROM ctx`, uid).Scan(&r.CollapseTxs, &r.FirstCollapseTx, &r.LastCollapseTx, &r.CollapseKeepers); err != nil {
		return r, fmt.Errorf("store: collapse provenance (transactions): %w", err)
	}

	byBy := func(sql, what string, into map[string]int) error {
		rows, err := tx.tx.Query(ctx, sql, uid)
		if err != nil {
			return fmt.Errorf("store: collapse provenance (%s): %w", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			var by string
			var n int
			if err := rows.Scan(&by, &n); err != nil {
				return fmt.Errorf("store: collapse provenance (%s): %w", what, err)
			}
			into[by] = n
		}
		return rows.Err()
	}
	if err := byBy(collapseTxCTE+`
		SELECT ml.applied_by, count(*)
		  FROM app.message_labels ml
		  JOIN ctx ON ctx.keeper_id = ml.message_id AND ctx.created_at = ml.created_at
		 WHERE ml.user_id = $1
		 GROUP BY ml.applied_by`, "collapse-written labels", r.CollapseLabelsByBy); err != nil {
		return r, err
	}
	if err := byBy(`
		SELECT applied_by, count(*) FROM app.message_labels
		 WHERE user_id = $1 AND applied_by_rule_id IS NOT NULL AND applied_by <> 'rule'
		 GROUP BY applied_by`, "rule-id contradictions", r.RuleIDByAppliedBy); err != nil {
		return r, err
	}

	rows, err := tx.tx.Query(ctx, collapseTxCTE+`
		SELECT ml.message_id, ml.label_id, ml.applied_by_rule_id, ml.created_at,
		       EXISTS (SELECT 1 FROM ctx WHERE ctx.keeper_id = ml.message_id
		                                   AND ctx.created_at = ml.created_at)
		  FROM app.message_labels ml
		 WHERE ml.user_id = $1 AND ml.applied_by = 'user' AND ml.applied_by_rule_id IS NOT NULL
		 ORDER BY ml.message_id, ml.label_id`, uid)
	if err != nil {
		return r, fmt.Errorf("store: collapse provenance (repairable): %w", err)
	}
	for rows.Next() {
		var row CollapseProvenanceRow
		if err := rows.Scan(&row.MessageID, &row.LabelID, &row.RuleID, &row.CreatedAt, &row.InCollapseTx); err != nil {
			rows.Close()
			return r, fmt.Errorf("store: collapse provenance (repairable): %w", err)
		}
		r.Repairable = append(r.Repairable, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, fmt.Errorf("store: collapse provenance (repairable): %w", err)
	}

	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE $2::timestamptz IS NULL OR created_at < $2::timestamptz)
		  FROM app.message_labels
		 WHERE user_id = $1 AND applied_by = 'user' AND applied_by_rule_id IS NULL`,
		uid, r.OldestEmailChange).Scan(&r.UserRowsNoRule, &r.UserRowsNoRuleBeforeHorizon); err != nil {
		return r, fmt.Errorf("store: collapse provenance (bound): %w", err)
	}
	return r, nil
}

// RepairCollapseRuleProvenance re-attributes every 'user' row carrying a rule id
// to that rule — evidence 1 above, the only kind that settles the original
// attribution. One Email `updated` change row per touched message, in this
// transaction: applied_by is JMAP-visible ("why is this label here?"), so every
// synced client must re-read it. A rerun finds nothing to do, because the rows
// it would select no longer say 'user'.
//
// It leaves created_at alone: the collapse overwrote the original time, and no
// surviving record holds it.
func (tx *UserTx) RepairCollapseRuleProvenance(ctx context.Context) (labels int, messages int, err error) {
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.message_labels SET applied_by = $2
		 WHERE user_id = $1 AND applied_by = $3 AND applied_by_rule_id IS NOT NULL
		RETURNING message_id`,
		tx.userID.String(), LabelByRule, LabelByUser)
	if err != nil {
		return 0, 0, fmt.Errorf("store: repair collapse provenance: %w", err)
	}
	seen := map[string]bool{}
	var ids []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: repair collapse provenance (scan): %w", err)
		}
		labels++
		if seen[s] {
			continue
		}
		seen[s] = true
		id, err := ulid.Parse(s)
		if err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: repair collapse provenance: message id %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: repair collapse provenance: %w", err)
	}
	if err := tx.AppendChanges(ctx, ObjectEmail, ids, OpUpdated); err != nil {
		return 0, 0, err
	}
	return labels, len(ids), nil
}
