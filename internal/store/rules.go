// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// This file holds the T0 rules-engine store layer (spec §8, mailroom#32): CRUD
// for app.rules plus the ingest-time application of a rule's actions. A Rule IS
// a JMAP-visible object (changelog.go ObjectRule), so every create/update/
// destroy appends exactly one Rule change_log row in the SAME UserTx as the
// mutation (the load-bearing invariant — CLAUDE.md), mirroring labels.go.
//
// conditions/actions are stored as jsonb and validated through internal/rules
// on the way in: a rule whose regex will not compile, or that names an
// unknown/unsupported action, is rejected HERE (ErrInvalidRule), so the ingest
// evaluator never meets a rule it cannot handle.

// ErrRuleNotFound is returned when a rule id does not resolve for the caller
// (RLS makes another user's id indistinguishable from a missing one).
var ErrRuleNotFound = errors.New("store: rule not found")

// ErrRuleNameTaken is returned when a create/rename collides with the caller's
// UNIQUE (user_id, name) constraint on app.rules.
var ErrRuleNameTaken = errors.New("store: rule name already exists")

// ErrInvalidRule is returned when a rule's conditions/actions fail
// internal/rules validation (a bad regex, an unknown/unsupported action). It
// wraps the underlying rules error so the API can surface a precise message.
var ErrInvalidRule = errors.New("store: invalid rule")

// RuleRow is the input to CreateRule. Priority defaults are the caller's to
// apply (the API defaults an absent priority to 100, matching the schema).
type RuleRow struct {
	Name            string
	Priority        int
	Conditions      rules.Condition
	Actions         []rules.Action
	Enabled         bool
	SystemSuggested bool
}

// RulePatch is a partial update for UpdateRule: a nil pointer leaves the field
// unchanged.
type RulePatch struct {
	Name            *string
	Priority        *int
	Conditions      *rules.Condition
	Actions         *[]rules.Action
	Enabled         *bool
	SystemSuggested *bool
}

// CreateRule validates and inserts a rule, then appends the Rule change_log row
// (the invariant). Returns ErrRuleNameTaken on a name collision and ErrInvalidRule
// on a conditions/actions validation failure. The INSERT runs under a SAVEPOINT
// so a UNIQUE (user_id, name) violation is a recoverable, caller-visible outcome
// that does not abort the surrounding UserTx (SQLSTATE 25P02) — a Rule/set that
// reports one duplicate name in notCreated and keeps its other creates.
func (tx *UserTx) CreateRule(ctx context.Context, row RuleRow) (ulid.ULID, error) {
	if err := rules.Validate(rules.Rule{
		Name: row.Name, Priority: row.Priority,
		Conditions: row.Conditions, Actions: row.Actions,
		Enabled: row.Enabled, SystemSuggested: row.SystemSuggested,
	}); err != nil {
		return ulid.ULID{}, fmt.Errorf("%w: %v", ErrInvalidRule, err)
	}
	condJSON, actJSON, err := marshalRule(row.Conditions, row.Actions)
	if err != nil {
		return ulid.ULID{}, err
	}

	id := NewULID()
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create rule (savepoint): %w", err)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.rules
			(id, user_id, name, priority, conditions, actions, enabled, system_suggested)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8)`,
		id.String(), tx.userID.String(), row.Name, row.Priority,
		string(condJSON), string(actJSON), row.Enabled, row.SystemSuggested); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return ulid.ULID{}, ErrRuleNameTaken
		}
		return ulid.ULID{}, fmt.Errorf("store: create rule: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create rule (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectRule, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// UpdateRule applies a partial update and appends exactly one Rule change_log
// row when something actually changes (a no-op patch is a true no-op). The
// resulting rule is re-validated, so an update cannot introduce a bad regex or
// unsupported action. found is false when the rule does not exist for the caller.
func (tx *UserTx) UpdateRule(ctx context.Context, id ulid.ULID, patch RulePatch) (found bool, err error) {
	var (
		name       string
		priority   int
		condRaw    []byte
		actRaw     []byte
		enabled    bool
		systemSugg bool
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT name, priority, conditions, actions, enabled, system_suggested
		  FROM app.rules
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), id.String()).
		Scan(&name, &priority, &condRaw, &actRaw, &enabled, &systemSugg)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: update rule (read): %w", err)
	}

	conds, err := rules.ParseConditions(condRaw)
	if err != nil {
		return false, fmt.Errorf("store: update rule (parse conditions): %w", err)
	}
	acts, err := rules.ParseActions(actRaw)
	if err != nil {
		return false, fmt.Errorf("store: update rule (parse actions): %w", err)
	}

	nName, nPriority, nEnabled, nSystem := name, priority, enabled, systemSugg
	nConds, nActs := conds, acts
	if patch.Name != nil {
		nName = *patch.Name
	}
	if patch.Priority != nil {
		nPriority = *patch.Priority
	}
	if patch.Conditions != nil {
		nConds = *patch.Conditions
	}
	if patch.Actions != nil {
		nActs = *patch.Actions
	}
	if patch.Enabled != nil {
		nEnabled = *patch.Enabled
	}
	if patch.SystemSuggested != nil {
		nSystem = *patch.SystemSuggested
	}

	if verr := rules.Validate(rules.Rule{
		Name: nName, Priority: nPriority, Conditions: nConds, Actions: nActs,
		Enabled: nEnabled, SystemSuggested: nSystem,
	}); verr != nil {
		return true, fmt.Errorf("%w: %v", ErrInvalidRule, verr)
	}
	condJSON, actJSON, err := marshalRule(nConds, nActs)
	if err != nil {
		return true, err
	}

	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update rule (savepoint): %w", err)
	}
	var touched string
	err = sp.QueryRow(ctx, `
		UPDATE app.rules
		   SET name = $3, priority = $4, conditions = $5::jsonb, actions = $6::jsonb,
		       enabled = $7, system_suggested = $8
		 WHERE user_id = $1 AND id = $2
		   AND (name, priority, conditions, actions, enabled, system_suggested)
		       IS DISTINCT FROM ($3, $4, $5::jsonb, $6::jsonb, $7, $8)
		RETURNING id`,
		tx.userID.String(), id.String(), nName, nPriority,
		string(condJSON), string(actJSON), nEnabled, nSystem).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			// The row exists (we locked it) but nothing changed: a true no-op.
			if cerr := sp.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: update rule (release): %w", cerr)
			}
			return true, nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return true, ErrRuleNameTaken
		}
		return false, fmt.Errorf("store: update rule (write): %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: update rule (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectRule, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteRule destroys a rule and appends the single Rule change_log row. found
// is false when the rule does not exist for the caller.
//
// ⚠ IT DOES NOT FAN OUT PER-EMAIL ROWS FOR THE PROVENANCE IT CLEARS, AND THAT IS
// A DECISION (#299). app.message_labels.applied_by_rule_id is ON DELETE SET NULL,
// so deleting a rule changes `Email.labelProvenance` on every message that rule
// ever labelled — hundreds of messages for a single rule on a real mailbox. No Email
// rows are appended for them, for one reason that is stronger here than it is for
// DeleteLabel's equivalent deferral:
//
//	THE Rule `destroyed` ROW ALREADY CARRIES THE WHOLE ANSWER. Provenance is
//	recorded BY RULE ID, and the client is being told in this very transaction
//	that this rule id no longer exists. Every labelProvenance naming it is
//	therefore reconcilable locally, with no re-query and no ambiguity — unlike a
//	deleted LABEL, where the client must also learn which messages lost it.
//
// What a second client observes: nothing stale it cannot fix from the row it
// receives. The label itself is untouched — it stays on the message, exactly
// where the user put it — and only the "applied by <rule>" attribution goes,
// which is the correct answer once the rule is gone.
//
// The alternative, appending an Email `updated` per affected message, would make
// deleting one rule a multi-hundred-row write inside a JMAP request to tell
// clients something they can already derive. That is the trade being made, so
// the next reader does not have to guess whether it was considered.
func (tx *UserTx) DeleteRule(ctx context.Context, id ulid.ULID) (found bool, err error) {
	var deleted string
	err = tx.tx.QueryRow(ctx, `
		DELETE FROM app.rules WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete rule: %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectRule, id, OpDestroyed); err != nil {
		return false, err
	}
	return true, nil
}

// ListRules returns the caller's rules ordered by priority then name then id —
// the stable total order Rule/query needs. When enabledOnly is true only enabled
// rules are returned.
func (tx *UserTx) ListRules(ctx context.Context, enabledOnly bool) ([]rules.Rule, error) {
	where := "user_id = $1"
	if enabledOnly {
		where += " AND enabled"
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, name, priority, conditions, actions, enabled, system_suggested
		  FROM app.rules
		 WHERE `+where+`
		 ORDER BY priority, name, id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	defer rows.Close()
	return scanRules(rows)
}

// GetRules returns rule projections for the requested ids (RLS-scoped). Ids not
// returned were not found or belong to another user — indistinguishable by
// design. Result order is unspecified; the caller re-associates by id.
func (tx *UserTx) GetRules(ctx context.Context, ids []ulid.ULID) ([]rules.Rule, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, name, priority, conditions, actions, enabled, system_suggested
		  FROM app.rules
		 WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get rules: %w", err)
	}
	defer rows.Close()
	return scanRules(rows)
}

// RulesForEval returns the caller's ENABLED rules ordered by priority (lower
// first) then id — the exact ordered set the ingest evaluator consumes.
func (tx *UserTx) RulesForEval(ctx context.Context) ([]rules.Rule, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, name, priority, conditions, actions, enabled, system_suggested
		  FROM app.rules
		 WHERE user_id = $1 AND enabled
		 ORDER BY priority, id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: rules for eval: %w", err)
	}
	defer rows.Close()
	return scanRules(rows)
}

// scanRules drains a rule result set into rules.Rule values, parsing the
// conditions/actions jsonb.
func scanRules(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]rules.Rule, error) {
	var out []rules.Rule
	for rows.Next() {
		var (
			idStr   string
			r       rules.Rule
			condRaw []byte
			actRaw  []byte
		)
		if err := rows.Scan(&idStr, &r.Name, &r.Priority, &condRaw, &actRaw,
			&r.Enabled, &r.SystemSuggested); err != nil {
			return nil, fmt.Errorf("store: scan rule: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse rule id %q: %w", idStr, err)
		}
		r.ID = id
		if r.Conditions, err = rules.ParseConditions(condRaw); err != nil {
			return nil, err
		}
		if r.Actions, err = rules.ParseActions(actRaw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan rules: %w", err)
	}
	return out, nil
}

func marshalRule(conds rules.Condition, acts []rules.Action) (condJSON, actJSON []byte, err error) {
	condJSON, err = json.Marshal(conds)
	if err != nil {
		return nil, nil, fmt.Errorf("store: marshal rule conditions: %w", err)
	}
	if acts == nil {
		acts = []rules.Action{}
	}
	actJSON, err = json.Marshal(acts)
	if err != nil {
		return nil, nil, fmt.Errorf("store: marshal rule actions: %w", err)
	}
	return condJSON, actJSON, nil
}

// ApplyRuleActions applies a T0 rule's resolved actions to a brand-new message
// during ingest, in the ingesting UserTx. It is FAIL-SOFT and MAIL-SAFE by
// construction: every action runs under its own SAVEPOINT, so a broken action
// (a rule naming a since-deleted label, say) is rolled back to the savepoint and
// SKIPPED, leaving the outer ingest transaction fully usable — a rule can never
// abort the message's ingest or lose mail. It returns the per-action errors for
// logging; the caller ignores them and always finishes ingesting the message.
//
// It is called on a BRAND-NEW message, BEFORE its provenance row and its own
// OpCreated change_log row are written (ingest.go). Two consequences make it both
// safe and simple: (1) there is no live provenance yet, so nothing here can
// enqueue an IMAP writeback intent — the label/keyword/placement changes are
// purely local at ingest time; (2) the actions mutate the new row DIRECTLY and
// append NO change_log of their own, because the message's single OpCreated row
// (appended by ingest immediately after) already reflects the final state — this
// avoids emitting an Email `updated` row that would otherwise carry a HIGHER seq
// than the message's own `created` row. accountID / arrivingMailboxID come from
// the arriving message's provenance and resolve the skip-inbox placement target.
//
// suggestLabel (#242) is the one action that writes OUTSIDE app.messages and its
// label/placement satellites — it appends an app.label_suggestions row instead —
// and both properties above still hold for it. It is inside the same per-action
// SAVEPOINT as everything else, so a suggestion that cannot be written (a
// since-deleted label losing a race, say) is rolled back and skipped like any
// other broken action; and it appends no change_log row of its own because
// Email.suggestedLabels is part of the state the message's OpCreated row already
// reports (PutLabelSuggestion documents that contract in full).
func (tx *UserTx) ApplyRuleActions(ctx context.Context, msgID, accountID, arrivingMailboxID ulid.ULID, actions []rules.AttributedAction) []error {
	var errs []error
	apply := func(act rules.AttributedAction) {
		if err := tx.inSavepoint(ctx, func() error {
			return tx.applyRuleAction(ctx, msgID, accountID, arrivingMailboxID, act)
		}); err != nil {
			errs = append(errs, fmt.Errorf("rule action %q: %w", act.Type, err))
		}
	}

	// ⚠ EVERY suggestLabel RUNS LAST, WHATEVER PRIORITY ITS RULE HAD (#242
	// follow-up). This is a two-pass loop rather than one, and the ordering is
	// load-bearing rather than tidy.
	//
	// PutRuleSuggestion refuses to write a question about a message that already
	// carries a label — that guard is what keeps a suggestion from dragging
	// already-filed mail into the needs-a-category queue. But it can only see
	// labels ALREADY WRITTEN in this transaction, so it is only as good as the
	// order the actions run in.
	//
	// Relying on priority for that was luck dressed as design. `suggest-rules`
	// creates its rules at 200 and ordinary rules default to 100, so induced
	// suggestions did land after their labels — but `--priority` is
	// operator-settable, and a suggestLabel rule written BY HAND through Rule/set
	// takes app.rules.priority's own DEFAULT 100, identical to an addLabel rule.
	// At that point RulesForEval's `ORDER BY priority, id` means CREATION ORDER
	// decides whether the guard sees the label: a hand-written suggestion rule
	// made before the labelling rule fires first, sees an unlabelled message,
	// writes its question, and the label lands a moment later — reproducing the
	// exact defect the guard exists to prevent, on a path nobody would think to
	// test.
	//
	// Partitioning here makes the guarantee structural instead of coincidental: a
	// suggestion is a question about what nothing else decided, so it belongs
	// after every decision in the pass, by construction. Nothing else is
	// reordered, and the relative order WITHIN each pass is preserved, so rules
	// that genuinely depend on priority (a `stop`, two labels racing for the same
	// message) behave exactly as before. `stop` is already resolved upstream in
	// EvaluateAttributed, which trims the action list before it ever gets here.
	for _, a := range actions {
		if a.Type == rules.ActionSuggestLabel {
			continue
		}
		apply(a)
	}
	for _, a := range actions {
		if a.Type != rules.ActionSuggestLabel {
			continue
		}
		apply(a)
	}
	return errs
}

func (tx *UserTx) applyRuleAction(ctx context.Context, msgID, accountID, arrivingMailboxID ulid.ULID, a rules.AttributedAction) error {
	switch a.Type {
	case rules.ActionAddLabel:
		lid, ok, err := tx.ResolveRuleLabel(ctx, a.Label)
		if err != nil {
			return err
		}
		if !ok {
			return nil // unknown label — skip, never fail ingest
		}
		// Direct, additive attach on the new message (a stale label id FK-fails and
		// is rolled back to the savepoint, then skipped).
		//
		// The rule is recorded as the applier (#155). DO NOTHING on conflict keeps
		// whatever attribution is already there, which matters on the re-ingest of
		// a message the user had labelled by hand: their label stays theirs.
		appliedBy, ruleID := RuleLabelSource(a.RuleID).columns()
		_, err = tx.tx.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, message_id, label_id) DO NOTHING`,
			tx.userID.String(), msgID.String(), lid.String(), appliedBy, ruleID)
		return err
	case rules.ActionSuggestLabel:
		// PROPOSES the label; NEVER applies it (#242).
		//
		// ⚠ THE SEPARATION IS STRUCTURAL, NOT A CONVENTION. This arm is its own
		// case, and the ONLY write it can reach is PutRuleSuggestion → an
		// app.label_suggestions row. There is no path from here into
		// app.message_labels: not a shared helper, not a boolean parameter on the
		// addLabel insert, not a fallthrough. So "a suggestion was silently applied
		// as a label" is not a bug that can be introduced by getting a flag wrong —
		// it would take deleting this case and rewriting it as the one above, which
		// is a change a reviewer sees. That matters more here than the duplication
		// costs, because the failure is SILENT in exactly the way #169's own comment
		// describes: a wrongly applied label puts mail somewhere the user does not
		// look, and they find out weeks later or never.
		//
		// Resolution is identical to addLabel's, deliberately: an unknown or
		// since-deleted label is a clean SKIP, not an error, so a rule naming a
		// label the user has removed quietly stops proposing rather than logging a
		// failure on every message that matches it.
		lid, ok, err := tx.ResolveRuleLabel(ctx, a.Label)
		if err != nil {
			return err
		}
		if !ok {
			return nil // unknown label — skip, never fail ingest
		}
		// a.Conditions is the LIVE tree the evaluator just matched (see
		// rules.AttributedAction): the reasons are rendered from what actually
		// fired, so a rule narrowed by Rule/tighten cannot leave stale text behind
		// describing conditions it no longer has.
		//
		// The whole write goes through PutRuleSuggestion so this path and the admin
		// backfill cannot compute different confidences or different phrasing for
		// the same rule — one funnel, per applyrules.go's "front door onto the SAME
		// store functions, never a second write path".
		return tx.PutRuleSuggestion(ctx, msgID, lid, a.Conditions, a.Evidence)
	case rules.ActionMarkPriority:
		// Additive keyword, only if not already present.
		_, err := tx.tx.Exec(ctx, `
			UPDATE app.messages SET keywords = keywords || $3::text[]
			 WHERE user_id = $1 AND id = $2 AND NOT ($4 = ANY(keywords))`,
			tx.userID.String(), msgID.String(), []string{rules.KeywordImportant}, rules.KeywordImportant)
		return err
	case rules.ActionFlag:
		// flagged_at is stamped in the SAME statement (#444): COALESCE rather than
		// a bare now() because this arm only ever runs on a false->true transition
		// (the WHERE already excludes an already-flagged message), so there is
		// nothing to preserve — but writing it as an unconditional now() would be
		// a booby trap for the next person who loosens that WHERE and silently
		// starts re-stamping an already-flagged message's wait time on every
		// ingest of a rule match.
		_, err := tx.tx.Exec(ctx, `
			UPDATE app.messages SET is_flagged = true, flagged_at = COALESCE(flagged_at, now())
			 WHERE user_id = $1 AND id = $2 AND NOT is_flagged`,
			tx.userID.String(), msgID.String())
		return err
	case rules.ActionSkipInbox, rules.ActionArchive:
		_, err := tx.skipInboxForNewMessage(ctx, msgID, accountID, arrivingMailboxID)
		return err
	default:
		// stop is consumed in rules.Evaluate; moveToMailbox/junk are rejected at
		// save; anything else is an unknown action — a safe no-op here.
		return nil
	}
}

// ResolveRuleLabel resolves an addLabel target that may be a label id (ULID), a
// mirrored keyword atom, or a label name — and returns ok=false unless the label
// actually EXISTS for the caller. Confirming existence (rather than trusting a
// parseable ULID) keeps a rule that names a since-deleted label from attempting
// an FK-violating insert; it just becomes a no-op, so the attach never depends on
// savepoint recovery in the common case.
func (tx *UserTx) ResolveRuleLabel(ctx context.Context, s string) (ulid.ULID, bool, error) {
	if id, err := ulid.Parse(s); err == nil {
		var one int
		e := tx.tx.QueryRow(ctx,
			`SELECT 1 FROM app.labels WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), id.String()).Scan(&one)
		if e != nil {
			if isNoRows(e) {
				return ulid.ULID{}, false, nil
			}
			return ulid.ULID{}, false, fmt.Errorf("store: resolve rule label: %w", e)
		}
		return id, true, nil
	}
	if id, ok, err := tx.LabelIDByKeywordAtom(ctx, s); err != nil {
		return ulid.ULID{}, false, err
	} else if ok {
		return id, true, nil
	}
	return tx.LabelIDByName(ctx, s)
}

// skipInboxForNewMessage keeps a freshly-arrived message OUT of the inbox by
// overriding its canonical placement (app.messages.moved_mailbox_id) to the
// account's Archive mailbox. The message is NOT deleted — it stays stored and
// visible in Archive / its account (never dropped). No writeback intent is
// enqueued: this is a LOCAL skip-inbox (the server copy is untouched in this
// tier). No change_log row is appended here — the message's own OpCreated row,
// appended by ingest right after, already reflects the final placement.
//
// It only ever diverts mail that ACTUALLY ARRIVED in the inbox: a message synced
// from another folder (Sent, a label folder) is left where it is. If the account
// has no Archive mailbox to divert to, the message is left in the inbox rather
// than hidden somewhere it cannot be found — correctness over completeness.
//
// It has TWO callers, and they share it so there is one placement mechanism,
// not two: a rule's skipInbox/archive action (applyRuleAction) and a reply
// arriving in a muted conversation (DivertMutedArrival). The first placement
// wins — the UPDATE's `moved_mailbox_id IS NULL` guard makes the second a no-op.
//
// inArchive reports the message's placement AFTER the call: true when it now
// sits in this account's Archive, whether THIS call put it there or an earlier
// one in the same ingest did (a rule's skipInbox, for DivertMutedArrival). The
// rule path ignores it; the mute path needs the state, not the write, because
// "this muted reply is not in the inbox" is what decides its push (mailsync).
func (tx *UserTx) skipInboxForNewMessage(ctx context.Context, msgID, accountID, arrivingMailboxID ulid.ULID) (inArchive bool, err error) {
	if arrivingMailboxID != (ulid.ULID{}) {
		role, _, err := tx.MailboxRole(ctx, arrivingMailboxID)
		if err != nil {
			return false, err
		}
		if role != "inbox" {
			return false, nil
		}
	}
	if accountID == (ulid.ULID{}) {
		return false, nil
	}
	arch, ok, err := tx.MailboxForRole(ctx, accountID, "archive")
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.messages SET moved_mailbox_id = $3
		 WHERE user_id = $1 AND id = $2 AND moved_mailbox_id IS NULL`,
		tx.userID.String(), msgID.String(), arch.ID.String())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	// Already placed by an earlier writer in this ingest. Read what it chose
	// rather than assume it was Archive: today the only earlier writer is a
	// rule's skipInbox (moveToMailbox is refused at save), but a report that
	// is only true because of that is the kind that quietly stops being true.
	var there bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(moved_mailbox_id = $3, false) FROM app.messages
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), msgID.String(), arch.ID.String()).Scan(&there); err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return there, nil
}

// DivertMutedArrival is what makes mute do something (spec §6, #51): a NEW
// message that arrives in the inbox of a conversation the user has muted is
// placed in the account's Archive instead, exactly as a skipInbox rule would
// place it, through the SAME skipInboxForNewMessage — so everything that
// function guarantees holds here too, and is not restated:
//
//   - only mail that ARRIVED IN THE INBOX moves (a Sent copy, a label folder's
//     copy, stay put);
//   - an account with no Archive mailbox keeps the reply in its inbox, rather
//     than hiding it somewhere the user cannot find it;
//   - LOCAL ONLY: no writeback intent, the server copy stays in INBOX, so
//     another mail app still shows it there (decided
//     2026-09-19 — it is Mailroom that was asked to stop surfacing it);
//   - flags and read state are untouched: an unread reply stays unread;
//   - no change_log row of its own: ingest's OpCreated row, appended right
//     after, already reports the final placement (see ApplyRuleActions).
//
// It reports whether the message ended up in Archive as a muted arrival — the
// signal the sync engine subtracts from the new-mail push count. That includes
// a muted reply a skipInbox rule already archived a moment earlier in the same
// ingest: the placement is the rule's, but the user muted the conversation, and
// "a muted reply does not notify you" should not depend on which of the two
// happened to run first.
//
// It runs under its own SAVEPOINT, like every rule action, so a failure (a
// lost connection mid-statement, say) leaves the ingesting transaction usable:
// the caller logs it and the reply ingests normally, in the inbox. Muting can
// cost a notification; it can never cost a message.
//
// threadID is the thread ingest just resolved for the message; a thread that
// does not exist (or belongs to another user — RLS) is simply not muted.
func (tx *UserTx) DivertMutedArrival(ctx context.Context, msgID, threadID, accountID, arrivingMailboxID ulid.ULID) (bool, error) {
	var diverted bool
	err := tx.inSavepoint(ctx, func() error {
		var muted bool
		err := tx.tx.QueryRow(ctx, `
			SELECT muted_at IS NOT NULL FROM app.threads
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), threadID.String()).Scan(&muted)
		if err != nil {
			if isNoRows(err) {
				return nil
			}
			return fmt.Errorf("store: thread mute state: %w", err)
		}
		if !muted {
			return nil
		}
		diverted, err = tx.skipInboxForNewMessage(ctx, msgID, accountID, arrivingMailboxID)
		return err
	})
	if err != nil {
		return false, err
	}
	return diverted, nil
}

// inSavepoint runs fn inside a nested SAVEPOINT: on error it rolls back to the
// savepoint (leaving the surrounding UserTx usable) and returns the error; on
// success it releases the savepoint. It is how ApplyRuleActions makes a broken
// rule action recoverable instead of tx-poisoning.
func (tx *UserTx) inSavepoint(ctx context.Context, fn func() error) error {
	sp, err := tx.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: savepoint: %w", err)
	}
	if ferr := fn(); ferr != nil {
		_ = sp.Rollback(ctx)
		return ferr
	}
	if cerr := sp.Commit(ctx); cerr != nil {
		return fmt.Errorf("store: savepoint (release): %w", cerr)
	}
	return nil
}
