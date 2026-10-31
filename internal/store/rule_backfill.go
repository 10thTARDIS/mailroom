// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// Running a rule over mail that already arrived (spec §8, mailroom#191).
//
// Rules are evaluated in exactly one place — ingest, on a message as it lands.
// That leaves a saved rule doing nothing at all to the mailbox it was written
// about: Rule/preview says "this would match 47 of your messages", and then none
// of the 47 are touched. The backfill closes that gap.
//
// THE APPLICATION ITSELF IS NOT HERE, deliberately. It goes through
// ApplyEmailUpdate, the same path Email/set uses, because that is what appends
// the change_log row and enqueues the IMAP writeback. ApplyRuleActions — the
// ingest one — does neither, and correctly so: it runs before the message's own
// `created` row exists and before it has any live provenance. Reusing it on
// existing mail would silently violate the load-bearing invariant and leave
// labels that never reach the server.
//
// What lives here is the part that is not the application: deciding WHICH of a
// rule's actions a backfill is allowed to perform, and reading back WHICH labels
// a given rule put on a given message so an apply can be undone precisely rather
// than by guessing from the rule's actions.
//
// The first of those started life behind the JMAP method and moved here when a
// second caller appeared (`mailroom-admin apply-rules`, mailroom#193). It had to
// move rather than be copied: it is the rule that decides what a backfill will
// NOT do to several hundred messages, and two copies of that would eventually
// disagree about it — at which point one of the two front doors is quietly
// unsafe and nothing says which.

// BackfillUpdate turns a rule's actions into the ONE Email update a backfill
// performs, and names the actions it refuses.
//
// Refusing is the point. skipInbox and archive are statements about where mail
// lands WHEN IT ARRIVES; performed against history they mean "take several
// hundred messages out of the inbox you have already triaged", which is a
// different act with a different risk profile and is not what "apply this rule"
// sounds like. `stop` is control flow between rules and means nothing to one
// rule on its own. Both are reported rather than silently dropped, so a caller
// can say which part of the rule did not run.
//
// The returned update carries the rule's attribution, so every label it applies
// is traceable to its cause and — because only the user's own labels count as
// induction evidence — cannot be mistaken later for ground truth the user
// supplied.
func (tx *UserTx) BackfillUpdate(ctx context.Context, rule rules.Rule) (EmailUpdate, []string, error) {
	up := EmailUpdate{LabelSource: RuleLabelSource(rule.ID)}
	skipped := map[string]bool{}
	for _, act := range rule.Actions {
		switch act.Type {
		case rules.ActionAddLabel:
			// Resolved through the store, exactly as ingest does: the target may be
			// a label id, a keyword atom or a name, and a target that no longer
			// resolves is a no-op rather than an error — the same rule would simply
			// have labelled nothing at ingest either.
			lid, ok, err := tx.ResolveRuleLabel(ctx, act.Label)
			if err != nil {
				return EmailUpdate{}, nil, err
			}
			if ok {
				up.AddLabelIDs = append(up.AddLabelIDs, lid)
			}
		case rules.ActionFlag:
			t := true
			up.Flagged = &t
		case rules.ActionMarkPriority:
			up.AddKeywords = append(up.AddKeywords, rules.KeywordImportant)
		case rules.ActionSkipInbox, rules.ActionArchive, rules.ActionStop:
			skipped[act.Type] = true
		}
	}
	names := make([]string, 0, len(skipped))
	for k := range skipped {
		names = append(names, k)
	}
	sort.Strings(names)
	return up, names, nil
}

// DoesNothing reports that a backfill built from this update would not change
// any message, because every action the rule asks for is one a backfill refuses.
//
// Worth naming rather than open-coding at each call site: "the rule has no
// backfillable action" and "the rule matched nothing" produce the same visible
// zero, and a caller that cannot tell them apart scans the whole mailbox to
// report a confident nothing.
func (up EmailUpdate) DoesNothing() bool {
	return len(up.AddLabelIDs) == 0 && up.Flagged == nil && len(up.AddKeywords) == 0
}

// LabelsAppliedByRule reports, per message, the labels that RULE applied and
// that are still there.
//
// Keyed by message so an undo can be exact. Reading the attribution back is
// safer than re-resolving the rule's `addLabel` actions: the rule may have been
// edited since it ran, its label may have been renamed or re-pointed, and in
// either case the actions no longer describe what is actually on the message.
// The rows do.
//
// It can only ever name labels this rule applied, which is what makes the undo
// safe by construction — a hand-applied label is invisible to it, so no undo can
// remove something the user put there themselves.
func (tx *UserTx) LabelsAppliedByRule(
	ctx context.Context,
	ruleID ulid.ULID,
	msgIDs []ulid.ULID,
) (map[ulid.ULID][]ulid.ULID, error) {
	out := map[ulid.ULID][]ulid.ULID{}
	if len(msgIDs) == 0 {
		return out, nil
	}
	ids := make([]string, len(msgIDs))
	for i, id := range msgIDs {
		ids[i] = id.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, label_id
		  FROM app.message_labels
		 WHERE user_id = $1
		   AND message_id = ANY($2)
		   AND applied_by = $3
		   AND applied_by_rule_id = $4`,
		tx.userID.String(), ids, LabelByRule, ruleID.String())
	if err != nil {
		return nil, fmt.Errorf("store: labels applied by rule: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var mid, lid string
		if err := rows.Scan(&mid, &lid); err != nil {
			return nil, fmt.Errorf("store: scan rule-applied label: %w", err)
		}
		m, err := ulid.Parse(mid)
		if err != nil {
			return nil, fmt.Errorf("store: rule-applied message id %q: %w", mid, err)
		}
		l, err := ulid.Parse(lid)
		if err != nil {
			return nil, fmt.Errorf("store: rule-applied label id %q: %w", lid, err)
		}
		out[m] = append(out[m], l)
	}
	return out, rows.Err()
}

// LabelsOnMessages reports every label currently on each message, WHOEVER put it
// there.
//
// It answers exactly one question: will ApplyEmailUpdate change this message, or
// short-circuit? Because the label insert is additive with ON CONFLICT DO
// NOTHING, a label already present from ANY source makes the update a no-op —
// and a caller predicting the effect of a backfill has to know that.
//
// IT IS NOT EVIDENCE, and must never be used as such. RulePreviewCandidate
// deliberately carries only the labels the USER applied, because the inducer
// treats those as ground truth and feeding it rule output would let a rule that
// is already wrong confirm itself. This is the opposite question — "what is on
// the message" rather than "what did the person decide" — and the two must not be
// confused. It exists because a predictor built from UserLabelIDs alone reports
// mail that another rule has already labelled as about to be newly labelled,
// which never converges: the count stays the same on every run, and the report
// disagrees with the apply it claims to predict (mailroom#193).
func (tx *UserTx) LabelsOnMessages(
	ctx context.Context,
	msgIDs []ulid.ULID,
) (map[ulid.ULID][]ulid.ULID, error) {
	out := map[ulid.ULID][]ulid.ULID{}
	if len(msgIDs) == 0 {
		return out, nil
	}
	ids := make([]string, len(msgIDs))
	for i, id := range msgIDs {
		ids[i] = id.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, label_id
		  FROM app.message_labels
		 WHERE user_id = $1
		   AND message_id = ANY($2)`,
		tx.userID.String(), ids)
	if err != nil {
		return nil, fmt.Errorf("store: labels on messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var mid, lid string
		if err := rows.Scan(&mid, &lid); err != nil {
			return nil, fmt.Errorf("store: scan message label: %w", err)
		}
		m, err := ulid.Parse(mid)
		if err != nil {
			return nil, fmt.Errorf("store: message id %q: %w", mid, err)
		}
		l, err := ulid.Parse(lid)
		if err != nil {
			return nil, fmt.Errorf("store: label id %q: %w", lid, err)
		}
		out[m] = append(out[m], l)
	}
	return out, rows.Err()
}
