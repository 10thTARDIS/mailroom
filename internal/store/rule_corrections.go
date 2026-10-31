// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// Corrections: what a user took back from a rule (spec §8, mailroom#170).
//
// Rules apply labels automatically at ingest, and until migration 00020 there
// was no way to disagree durably. Removing a label deleted the row and its
// attribution together, so the rule kept firing and nothing recorded that a
// human had said otherwise. This is the record that makes an automation
// correctable rather than something to fight.
//
// IT IS EVIDENCE, NEVER AN ACTION. Nothing in this file disables, edits or
// narrows a rule. A rule is explicit state the user wrote or accepted, and
// silently changing it would make the rules list stop describing what actually
// happens — #170's own constraint, and the same principle that keeps Rule/induce
// a proposal rather than a write.

// RuleCorrection is one message a rule labelled and the user un-labelled.
type RuleCorrection struct {
	MessageID ulid.ULID
	LabelID   ulid.ULID
}

// recordRuleCorrection notes that the user removed a label a rule had applied.
//
// Called from ApplyEmailUpdate's removal path, inside the SAME transaction as
// the removal — the correction and the thing it describes must land together or
// not at all. It appends NO change_log row of its own: this is operational
// evidence about a rule, like a triage score is about a message, and the Rule
// object's own state has not changed. The Rule/updated row that tells clients a
// rule's correction count moved is appended by the caller once per affected
// rule (see CountRuleCorrections' doc for why the count is read, not stored).
//
// ON CONFLICT refreshes the timestamp rather than inserting again: one row per
// (rule, message, label) is the grain, because a user who removes the same
// label twice has been wronged about ONE message twice. Counting events would
// measure how stubborn a rule is rather than how wrong it is.
func (tx *UserTx) recordRuleCorrection(ctx context.Context, ruleID, msgID, labelID ulid.ULID) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO app.rule_corrections (user_id, rule_id, message_id, label_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, rule_id, message_id, label_id)
		DO UPDATE SET corrected_at = now()`,
		tx.userID.String(), ruleID.String(), msgID.String(), labelID.String())
	if err != nil {
		return fmt.Errorf("store: record rule correction: %w", err)
	}
	return nil
}

// CountRuleCorrections returns how many messages each rule has misfiled, keyed
// by rule id. Rules with none are absent rather than zero.
//
// READ rather than stored as a counter on app.rules, so it cannot drift from the
// rows it counts — a denormalised count would need maintaining on every
// correction, every rule delete, every message purge and every label delete, and
// the three cascades already handle all of those correctly for the rows
// themselves.
func (tx *UserTx) CountRuleCorrections(ctx context.Context) (map[ulid.ULID]int, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT rule_id, count(DISTINCT message_id)
		  FROM app.rule_corrections
		 WHERE user_id = $1
		 GROUP BY rule_id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: count rule corrections: %w", err)
	}
	defer rows.Close()

	out := map[ulid.ULID]int{}
	for rows.Next() {
		var raw string
		var n int
		if err := rows.Scan(&raw, &n); err != nil {
			return nil, fmt.Errorf("store: scan rule correction count: %w", err)
		}
		id, err := ulid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("store: rule correction rule id %q: %w", raw, err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// RuleCorrections lists the messages a given rule misfiled, newest first.
//
// The MESSAGE IDS are the point, not the count. To offer a narrower rule you
// have to know WHICH messages were wrong, so a proposal can be checked against
// them — a bare count says a rule is bad while giving nobody the means to fix
// it. This is what feeds the tightening proposal (#168's inducer over the
// rule's matches minus its corrections).
func (tx *UserTx) RuleCorrections(ctx context.Context, ruleID ulid.ULID, limit int) ([]RuleCorrection, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, label_id
		  FROM app.rule_corrections
		 WHERE user_id = $1 AND rule_id = $2
		 ORDER BY corrected_at DESC, message_id DESC
		 LIMIT $3`,
		tx.userID.String(), ruleID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: rule corrections: %w", err)
	}
	defer rows.Close()

	var out []RuleCorrection
	for rows.Next() {
		var mid, lid string
		if err := rows.Scan(&mid, &lid); err != nil {
			return nil, fmt.Errorf("store: scan rule correction: %w", err)
		}
		m, err := ulid.Parse(mid)
		if err != nil {
			return nil, fmt.Errorf("store: rule correction message id %q: %w", mid, err)
		}
		l, err := ulid.Parse(lid)
		if err != nil {
			return nil, fmt.Errorf("store: rule correction label id %q: %w", lid, err)
		}
		out = append(out, RuleCorrection{MessageID: m, LabelID: l})
	}
	return out, rows.Err()
}

// MessagesWithLabels reports which messages currently carry any of the given
// labels, whoever applied them.
//
// "Currently" is the operative word, and the reason provenance is NOT filtered
// here as it is everywhere else evidence is gathered (see
// RulePreviewCandidate.UserLabelIDs). Those callers ask what the USER believes,
// and must ignore automation output or a wrong rule confirms itself. This one
// asks what a rule is DOING right now, so a proposal to narrow it can say how
// much of its present output it would keep.
//
// A label a rule applied and the user has not taken back is weak assent — but it
// is the only evidence there is about that message, and counting it as a
// negative would propose narrowing away nearly everything the rule does. The
// self-confirmation trap does not apply, because nothing here can WIDEN a rule's
// scope: this evidence is only ever used to decide what a narrower form keeps.
func (tx *UserTx) MessagesWithLabels(ctx context.Context, labelIDs []ulid.ULID) (map[ulid.ULID]bool, error) {
	out := map[ulid.ULID]bool{}
	if len(labelIDs) == 0 {
		return out, nil
	}
	ids := make([]string, len(labelIDs))
	for i, id := range labelIDs {
		ids[i] = id.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT message_id
		  FROM app.message_labels
		 WHERE user_id = $1 AND label_id = ANY($2)`,
		tx.userID.String(), ids)
	if err != nil {
		return nil, fmt.Errorf("store: messages with labels: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("store: scan labelled message: %w", err)
		}
		id, err := ulid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("store: labelled message id %q: %w", raw, err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ClearRuleCorrections forgets a rule's corrections. Called when the user acts
// on them — narrowing the rule, or dismissing the suggestion — because the
// evidence describes the rule AS IT WAS, and carrying it across an edit would
// keep accusing the new rule of the old one's mistakes.
func (tx *UserTx) ClearRuleCorrections(ctx context.Context, ruleID ulid.ULID) (int, error) {
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.rule_corrections WHERE user_id = $1 AND rule_id = $2`,
		tx.userID.String(), ruleID.String())
	if err != nil {
		return 0, fmt.Errorf("store: clear rule corrections: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
