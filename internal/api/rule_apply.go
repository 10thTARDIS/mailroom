// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Rule/apply — run a saved rule over the mail you already have (spec §8,
// mailroom#191).
//
// Rules are evaluated in exactly one place, ingest, on a message as it arrives.
// So Rule/preview says "this would match 47 of your messages", the user saves
// the rule, and none of the 47 are touched — the blast radius is a measurement
// of mail the rule will never meet. On a real instance that meant many
// saved rules against very few rule-applied labels: the automation was written and
// doing almost nothing, because the mailbox is historical.
//
// It is the missing step in a loop the rest of the milestone already built —
// propose (#168), preview (#155), save, APPLY, correct (#170), narrow (#170) —
// and it matters most for exactly the rules that came from induction, whose
// entire premise is that they explain mail you already have.
//
// FOUR THINGS MAKE IT SAFE TO RUN, and each is a way this could have gone wrong:
//
//  1. It goes through ApplyEmailUpdate, NOT the ingest applier. store's
//     ApplyRuleActions appends no change_log row and enqueues no writeback,
//     which is right for a brand-new message and catastrophic for an existing
//     one — the labels would be invisible to /changes, to the search indexer and
//     to IMAP.
//  2. ADDITIVE ACTIONS ONLY. addLabel/markPriority/flag say what mail IS;
//     skipInbox and archive say where it LANDS ON ARRIVAL, and applying those to
//     history would move mail the user has already triaged.
//  3. It never re-applies a label the user has CORRECTED (#170). Without this,
//     the first thing a backfill would do is silently undo the user's own
//     disagreements — the exact opposite of what the corrections exist for.
//  4. It never downgrades a hand-applied label, and running it twice changes
//     nothing the second time.
//
// AND ONE REMOVAL, for one kind of rule (#708): a rule that files into an
// area removes the OTHER areas' desk/setup placement labels, or the
// labels-first resolver would leave the mail where the desk put it. Never a
// user's or another rule's label — those messages are left alone and counted
// (store.PlanAreaFiling, rule_area_filing.go).

// applyMaxMutations bounds how much mail one call will change.
//
// Separate from the SCAN bound: reading five thousand messages is cheap and
// reversible, writing to five thousand is neither. A backfill is one of the very
// few operations here that touches a large amount of mail on a single click, so
// it stops at a number a person can still review and undo, and says so.
const applyMaxMutations = 1000

type ruleApplyArgs struct {
	AccountID string `json:"accountId"`
	RuleID    string `json:"ruleId"`
	// Limit lowers the scan bound; omitted or <= 0 takes ruleScanLimit's default
	// (the whole mailbox for a row-facts rule, the blob bound for a body rule).
	Limit int `json:"limit"`
	// Undo removes the labels THIS RULE applied to the named messages, and
	// records no corrections against it — reversing a bulk operation is not a
	// hundred separate disagreements with the rule (see EmailUpdate.RemovalIsUndo).
	//
	// It takes explicit ids rather than "whatever the last apply did": the server
	// keeps no memory of an operation, and inferring one from a timestamp would
	// sooner or later undo a label that ingest happened to apply in the same
	// second. The ids come straight back from the apply that made them.
	Undo       bool     `json:"undo"`
	MessageIDs []string `json:"messageIds"`
	// RestoreAreaLabels is an area-filing apply's removedAreaLabels, handed
	// back on undo (#708): the desk/setup labels the apply took off, keyed by
	// message, which the undo puts back with the provenance they had. Only
	// messages also named in messageIds are restored, and only labels that are
	// some area's own label (store.AreaLabelRestorable) with a desk or setup
	// attribution — anything else in it is ignored rather than trusted.
	RestoreAreaLabels map[string][]ruleApplyRemovedLabel `json:"restoreAreaLabels"`
}

// ruleApplyRemovedLabel is one automation-applied area label an area-filing
// apply removed, with the attribution it had.
type ruleApplyRemovedLabel struct {
	LabelID   string `json:"labelId"`
	AppliedBy string `json:"appliedBy"`
}

type ruleApplyResult struct {
	AccountID string `json:"accountId"`
	RuleID    string `json:"ruleId"`
	// Matched is how many scanned messages the rule's conditions match.
	Matched int `json:"matched"`
	// Changed is how many messages this call actually altered. It is the number
	// worth showing: Matched includes mail that already carried everything the
	// rule applies, and reporting that as "labelled 47" would claim work that did
	// not happen.
	Changed int `json:"changed"`
	// AlreadyDone is matched mail the rule would have changed nothing about —
	// usually because a previous apply already did it, or the user had filed it
	// there by hand. Reported rather than folded into Matched so a second run
	// reads as "nothing left to do" instead of "matched 47, changed 0, no idea
	// why".
	AlreadyDone int `json:"alreadyDone"`
	// SkippedCorrected is matched mail carrying a label this rule applied and the
	// user REMOVED (#170). Never re-applied, and reported loudly: it is the
	// number that says "your corrections were respected", and a silent zero would
	// be indistinguishable from not having looked.
	SkippedCorrected int `json:"skippedCorrected"`
	// SkippedActions names the rule's actions that a backfill will not perform,
	// so a client can say WHICH part of the rule did not run rather than leaving
	// the user to notice that nothing moved.
	SkippedActions []string `json:"skippedActions"`
	// MessageIDs are the messages this call changed — the argument to an undo.
	MessageIDs []string `json:"messageIds"`
	// Truncated reports that more mail matched than one call will change, and
	// Remaining how much is left. The client must offer to continue rather than
	// present a partial backfill as a finished one.
	Truncated bool `json:"truncated"`
	Remaining int  `json:"remaining"`

	Scanned  int  `json:"scanned"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`

	// Undone is how many messages an undo took labels off.
	Undone int `json:"undone"`

	// AreaFiling is set when the rule's addLabel target is an area's own label
	// (#708): the per-outcome counts of filing the processed mail into that
	// area. Null for every other rule, which behaves exactly as before.
	AreaFiling *ruleAreaFiling `json:"areaFiling"`
	// RemovedAreaLabels names, per changed message, the desk/setup area labels
	// this apply took off — the argument an undo needs (restoreAreaLabels) to
	// put them back exactly. Empty for a rule that is not area filing.
	RemovedAreaLabels map[string][]ruleApplyRemovedLabel `json:"removedAreaLabels"`
}

func (a *API) ruleApply(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args ruleApplyArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	ruleID, err := ulid.Parse(args.RuleID)
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "ruleId must be a valid id"}
	}
	if args.Undo {
		return a.ruleApplyUndo(ctx, userID, ruleID, args.MessageIDs, args.RestoreAreaLabels)
	}

	res := ruleApplyResult{
		AccountID:         userID.String(),
		RuleID:            ruleID.String(),
		SkippedActions:    []string{},
		MessageIDs:        []string{},
		RemovedAreaLabels: map[string][]ruleApplyRemovedLabel{},
	}

	// ONE TRANSACTION for the whole backfill. A half-applied one is worse than a
	// slow one: the user would be looking at a partial result with no way to tell
	// which half happened, and the undo they were offered would only reach the
	// part that landed.
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, e := tx.GetRules(ctx, []ulid.ULID{ruleID})
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if len(views) == 0 {
			return &MethodError{Type: errInvalidArguments, Description: "no such rule"}
		}
		rule := views[0]
		// A DISABLED rule is one the user switched off. Running it over their
		// history anyway would be the automation arguing with them, and it is the
		// same rule either way — so this refuses rather than quietly obeying.
		if !rule.Enabled {
			return &MethodError{Type: errInvalidArguments,
				Description: "this rule is turned off; turn it on before applying it to existing mail"}
		}

		update, skipped, e := tx.BackfillUpdate(ctx, rule)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		res.SkippedActions = skipped
		// A rule whose every action is one a backfill will not perform has nothing
		// to do. Saying so beats scanning the mailbox to report a confident zero.
		if update.DoesNothing() {
			return nil
		}

		// Messages the user took this rule's label off. Skipped entirely — a
		// backfill that re-applies a correction has undone the user's own work,
		// and would do it again on every run.
		corrections, e := tx.RuleCorrections(ctx, ruleID, previewMaxScanRows)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		corrected := map[ulid.ULID]bool{}
		for _, c := range corrections {
			corrected[c.MessageID] = true
		}

		headerNames := rules.HeaderNames([]rules.Rule{rule})
		needBodies := len(headerNames) > 0 || rules.UsesField(rule.Conditions, rules.FieldTo)
		if needBodies && a.raw == nil {
			return &MethodError{Type: errServerFail,
				Description: "this server cannot read message bodies, so a rule testing " +
					"recipients or headers cannot be applied to existing mail"}
		}

		// The bound depends on whether the rule must open message bytes (#720):
		// a row-facts rule scans the whole mailbox, a body rule the blob bound.
		limit := ruleScanLimit(args.Limit, needBodies)
		candidates, e := tx.RulePreviewCandidates(ctx, limit)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if res.Total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		res.Scanned = len(candidates)
		res.Complete = res.Scanned >= res.Total

		// DID ANYTHING ACTUALLY HAPPEN? ApplyEmailUpdate short-circuits when a
		// message already carries everything the update asks for: it appends no
		// change_log row and returns the state unmoved. So a state that advanced
		// IS the "changed" signal, and no extra query is needed to tell "labelled
		// 47" from "47 already had it" — which is the difference between a second
		// run reading as "nothing left to do" and as unexplained silence.
		prev, e := tx.ChangeState(ctx, store.ObjectEmail)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}

		var hits []ulid.ULID
		for _, c := range candidates {
			facts := a.factsFor(ctx, userID, c, needBodies, headerNames)
			if !rule.Conditions.Matches(facts) {
				continue
			}
			res.Matched++
			if corrected[c.ID] {
				res.SkippedCorrected++
				continue
			}
			hits = append(hits, c.ID)
		}

		// AN AREA-FILING RULE (#708) is a swap, not an add: the other areas'
		// desk/setup labels come off so the target actually wins the
		// labels-first resolution, and mail the user (or another rule) filed
		// elsewhere is left where it is and reported. The plan is the same one
		// Rule/preview reports (store.PlanAreaFiling), so the two cannot
		// disagree. Every other rule takes none of this and behaves as before.
		target, isArea, e := tx.AreaFilingTargetFor(ctx, update.AddLabelIDs)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		var plan map[ulid.ULID]store.AreaFilingStep
		if isArea {
			if plan, e = tx.PlanAreaFiling(ctx, target, hits); e != nil {
				return &MethodError{Type: errServerFail, Description: e.Error()}
			}
			res.AreaFiling = newRuleAreaFiling(target)
		}

		for _, id := range hits {
			if res.Changed >= applyMaxMutations {
				res.Truncated = true
				res.Remaining++
				continue
			}
			up := update
			var removed []store.RemovedAreaLabel
			if isArea {
				step, ok := plan[id]
				if !ok {
					continue // gone since the scan; ApplyEmailUpdate would find nothing either
				}
				res.AreaFiling.count(step.Outcome)
				switch step.Outcome {
				case store.AreaFilingMove, store.AreaFilingAlreadyThere:
					removed = step.Remove
					up.RemoveLabelIDs = make([]ulid.ULID, 0, len(removed))
					for _, r := range removed {
						up.RemoveLabelIDs = append(up.RemoveLabelIDs, r.LabelID)
					}
				default:
					// Contested: left where it is. The rule's OTHER actions still
					// run; only the filing is withheld.
					up.AddLabelIDs = withoutLabel(update.AddLabelIDs, target.LabelID)
					if up.DoesNothing() {
						continue
					}
				}
			}
			// The ON CONFLICT DO NOTHING underneath keeps a hand-applied label the
			// user's own: a backfill must never rewrite the attribution the
			// inducer treats as evidence, or a rule would launder its own output
			// into ground truth.
			found, state, e := tx.ApplyEmailUpdate(ctx, id, up)
			if e != nil {
				return &MethodError{Type: errServerFail, Description: e.Error()}
			}
			if !found {
				continue
			}
			if state <= prev {
				res.AlreadyDone++
				continue
			}
			prev = state
			res.Changed++
			res.MessageIDs = append(res.MessageIDs, id.String())
			if len(removed) > 0 {
				out := make([]ruleApplyRemovedLabel, 0, len(removed))
				for _, r := range removed {
					out = append(out, ruleApplyRemovedLabel{LabelID: r.LabelID.String(), AppliedBy: r.AppliedBy})
				}
				res.RemovedAreaLabels[id.String()] = out
			}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}

// ruleApplyUndo reverses a backfill: it removes the labels THIS RULE put on the
// named messages, and records no corrections for doing so.
//
// The labels come from the stored attribution rather than from the rule's
// current actions, so an undo stays exact even if the rule was edited in
// between — and it can only ever name labels the rule applied, which is what
// makes it incapable of removing something the user filed by hand.
func (a *API) ruleApplyUndo(
	ctx context.Context,
	userID, ruleID ulid.ULID,
	rawIDs []string,
	rawRestore map[string][]ruleApplyRemovedLabel,
) (json.RawMessage, *MethodError) {
	ids, _, _ := parseIDs(rawIDs)
	res := ruleApplyResult{
		AccountID:         userID.String(),
		RuleID:            ruleID.String(),
		SkippedActions:    []string{},
		MessageIDs:        []string{},
		RemovedAreaLabels: map[string][]ruleApplyRemovedLabel{},
	}
	if len(ids) == 0 {
		return mustMarshal(res), nil
	}

	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		byMessage, e := tx.LabelsAppliedByRule(ctx, ruleID, ids)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		restore, e := areaLabelsToRestore(ctx, tx, ids, rawRestore)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		for _, id := range ids {
			labels := byMessage[id]
			back := restore[id]
			if len(labels) == 0 && len(back) == 0 {
				continue
			}
			up := store.EmailUpdate{
				RemoveLabelIDs: labels,
				RemovalIsUndo:  true,
			}
			// The area labels an area-filing apply removed go back in the SAME
			// update — one change_log row — each with its own provenance
			// (AddLabelSources), so a restored desk label is the desk's again and
			// not the user's hand-filing.
			if len(back) > 0 {
				up.AddLabelSources = map[ulid.ULID]store.LabelSource{}
				for _, r := range back {
					up.AddLabelIDs = append(up.AddLabelIDs, r.LabelID)
					up.AddLabelSources[r.LabelID] = store.LabelSource{Kind: r.AppliedBy}
				}
			}
			found, _, e := tx.ApplyEmailUpdate(ctx, id, up)
			if e != nil {
				return &MethodError{Type: errServerFail, Description: e.Error()}
			}
			if !found {
				continue
			}
			res.Undone++
			res.MessageIDs = append(res.MessageIDs, id.String())
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}
