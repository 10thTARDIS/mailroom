// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Rule/tighten — "this rule has misfiled 4 messages; here is a narrower one"
// (spec §8, mailroom#170).
//
// The other half of #170. Migration 00020 records what a user took back from a
// rule (store/rule_corrections.go); Rule/get reports the count. Neither is worth
// much on its own: telling somebody a rule is wrong without giving them the
// means to fix it just relocates the problem into their lap, and "go and edit
// the conditions yourself" is exactly the manual work the rules engine exists to
// remove.
//
// So this method turns the corrections into a PROPOSAL, using the same inducer
// that writes rules in the first place (#168) — positives are the messages the
// rule still gets right, and every proposal must match NONE of the corrected
// ones. That last clause is the whole method: a narrower rule that still catches
// the mail the user rejected is not a fix, and offering one would be worse than
// offering nothing.
//
// IT WRITES NOTHING, like Rule/preview and Rule/induce. The client saves the
// accepted form with Rule/set, and that save is what retires the corrections
// (updateRule clears them when the conditions change). An automation that
// rewrites its own rules is one nobody can predict, and the rules list would
// stop describing what actually happens.

// tightenMaxProposals caps how many narrower forms come back. Same reasoning as
// induceCandidates: several are genuinely different decisions, but a wall of
// them is one nobody reads.
const tightenMaxProposals = 3

// tightenMaxSamples bounds the example rows per proposal.
const tightenMaxSamples = 8

type ruleTightenArgs struct {
	AccountID string `json:"accountId"`
	RuleID    string `json:"ruleId"`
	Limit     int    `json:"limit"`
}

// ruleScope is what a set of conditions does to the scanned window. The current
// rule and every proposal are reported in the SAME shape, because the only
// useful reading of a proposal is against the thing it would replace — "matches
// 31" means nothing until you know the rule it replaces matches 47.
type ruleScope struct {
	// Matched is everything in the window these conditions match.
	Matched int `json:"matched"`
	// Keeps is matched mail that still carries the label — what the rule is
	// getting right. Drops is the rest of that mail: correctly labelled messages
	// a proposal would stop matching, which is the PRICE of tightening and is
	// reported even when it is zero.
	Keeps int `json:"keeps"`
	Drops int `json:"drops"`
	// Misfires is matched mail the user corrected. The current rule's is the
	// reason this method was called; a proposal's is always zero, because one
	// that still catches a rejected message is not offered at all.
	Misfires int `json:"misfires"`
	// Collateral is matched mail that is neither labelled nor corrected — mail
	// nobody has judged. It is not an error count (an unlabelled message is not a
	// wrong one), it is the blast radius.
	Collateral int `json:"collateral"`
}

type tightenedRule struct {
	Conditions rules.Condition `json:"conditions"`
	ruleScope
	// Fixes is how many corrected messages this form no longer matches. It equals
	// the misfires the scan could see, always — the filter that produces these
	// proposals admits nothing less — and is reported anyway so a client can say
	// "fixes all 4" from the response rather than from an assumption about it.
	Fixes int `json:"fixes"`
	// Conflicts is matched mail the user filed under a DIFFERENT label by hand;
	// the inducer never proposes a candidate with any, so this is always 0 today.
	// Reported because proposals render through the same component as previews,
	// and a silent zero would be a promise rather than a measurement.
	Conflicts      int      `json:"conflicts"`
	ConflictLabels []string `json:"conflictLabels"`
	// Samples are the messages this form would DROP — correctly labelled mail it
	// stops matching. Showing what it still catches would only confirm it covers
	// what it was induced from, which is guaranteed; what a person needs to see
	// before accepting a narrower rule is what they LOSE.
	Samples []rulePreviewSample `json:"samples"`
}

type ruleTightenResult struct {
	AccountID string `json:"accountId"`
	RuleID    string `json:"ruleId"`
	// Corrections is how many messages the user took this rule's label back off,
	// in total. CorrectionsInWindow is how many of those this scan could see —
	// they can differ, and when the second is 0 no proposal can be verified, so
	// the client must say "the mail this rule got wrong is older than the window"
	// rather than "nothing to fix".
	Corrections         int `json:"corrections"`
	CorrectionsInWindow int `json:"correctionsInWindow"`
	// Current is the rule as it stands, measured over the same window.
	Current ruleScope `json:"current"`
	// Scanned/Total/Complete describe the window, exactly as Rule/preview does.
	Scanned  int  `json:"scanned"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
	// Proposals is best-first, and EMPTY IS A REAL ANSWER: mail the user rejected
	// can be indistinguishable from mail they kept on the facts a rule can test,
	// and in that case there is no narrower rule. The honest response is to say so
	// — the client's remaining options are to disable the rule or live with it —
	// rather than to invent a condition that merely looks discriminating.
	Proposals []tightenedRule `json:"proposals"`
}

func (a *API) ruleTighten(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args ruleTightenArgs
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

	var (
		rule        rules.Rule
		corrections []store.RuleCorrection
		correctionN int
		labelled    map[ulid.ULID]bool
		candidates  []store.RulePreviewCandidate
		total       int
		labelNames  = map[string]string{}
	)
	// One transaction for the whole read. The corrections, the labels they name
	// and the messages carrying those labels have to describe the same instant —
	// read across three transactions, a message relabelled in between shows up as
	// both kept and corrected, and the proposal is measured against a mailbox
	// that never existed.
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, e := tx.GetRules(ctx, []ulid.ULID{ruleID})
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if len(views) == 0 {
			return &MethodError{Type: errInvalidArguments, Description: "no such rule"}
		}
		rule = views[0]

		// Bounded by the scan, not by the store's display default: every
		// correction inside the window has to be seen, or a proposal could be
		// declared clean because the message it still catches was truncated away.
		if corrections, e = tx.RuleCorrections(ctx, ruleID, previewMaxScan); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		counts, e := tx.CountRuleCorrections(ctx)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		correctionN = counts[ruleID]

		// THE LABELS COME FROM THE CORRECTIONS, not from the rule's actions.
		//
		// An addLabel action stores a label id, a keyword atom or a name, and
		// resolving all three here would be re-implementing ingest's resolution in
		// a read path where it can only drift. The corrections rows record the
		// label that was actually applied and then taken back, which is both easier
		// to read and MORE correct: a rule that adds two labels and misfiled on one
		// of them should be narrowed with respect to the label that was wrong.
		seen := map[ulid.ULID]bool{}
		var targets []ulid.ULID
		for _, c := range corrections {
			if !seen[c.LabelID] {
				seen[c.LabelID] = true
				targets = append(targets, c.LabelID)
			}
		}
		if labelled, e = tx.MessagesWithLabels(ctx, targets); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}

		limit := args.Limit
		if limit <= 0 {
			limit = previewDefaultScan
		}
		if limit > previewMaxScan {
			limit = previewMaxScan
		}
		if candidates, e = tx.RulePreviewCandidates(ctx, limit); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		labels, e := tx.ListLabels(ctx)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		for _, v := range labels {
			labelNames[v.ID.String()] = v.Name
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	res := ruleTightenResult{
		AccountID:           userID.String(),
		RuleID:              ruleID.String(),
		Corrections:         correctionN,
		Scanned:             len(candidates),
		Total:               total,
		Proposals:           []tightenedRule{},
		CorrectionsInWindow: 0,
	}
	res.Complete = res.Scanned >= res.Total

	// A message the user corrected and then labelled again BY HAND is not
	// evidence against the rule any more — they changed their mind, and the later
	// act is the one that counts. The correction row survives (it still records
	// that the rule was once wrong) but it must not drive a proposal, or the
	// tightened form would be built to exclude mail the user has since said
	// belongs.
	corrected := map[ulid.ULID]bool{}
	for _, c := range corrections {
		if !labelled[c.MessageID] {
			corrected[c.MessageID] = true
		}
	}

	// Bodies are read only if the EXISTING rule tests `to` or a named header. No
	// proposal ever can — the inducer only builds sender, domain, list, alias and
	// unsubscribe conjuncts — so the window is set by what it takes to evaluate
	// the rule being replaced.
	headerNames := rules.HeaderNames([]rules.Rule{rule})
	needBodies := len(headerNames) > 0 || rules.UsesField(rule.Conditions, rules.FieldTo)
	if needBodies && a.raw == nil {
		return nil, &MethodError{Type: errServerFail,
			Description: "this server cannot read message bodies, so a rule testing " +
				"recipients or headers cannot be tightened"}
	}

	rows := make([]tightenRow, 0, len(candidates))
	examples := make([]rules.Example, 0, len(candidates))
	for _, c := range candidates {
		r := tightenRow{
			cand:      c,
			facts:     a.factsFor(ctx, userID, c, needBodies, headerNames),
			labelled:  labelled[c.ID],
			corrected: corrected[c.ID],
		}
		// KEPT is the rule's correct output: mail it matches that still carries
		// the label. It is the thing a narrower form is judged on preserving, and
		// it is deliberately not "everything labelled" — a message the user filed
		// by hand that this rule never matched is none of its business.
		r.kept = r.labelled && rule.Conditions.Matches(r.facts)
		rows = append(rows, r)

		// The inducer is scored over the WHOLE window, not over the rule's own
		// matches, so a proposal's collateral is its real blast radius rather than
		// its blast radius within the scope of the rule it replaces.
		ex := rules.Example{Facts: r.facts, Positive: r.kept}
		for _, lid := range c.UserLabelIDs {
			if isTargetLabel(lid, corrections) {
				// The rule's own label is the SUBJECT of this tightening, not a
				// disagreement with it. Counted as a conflict it would make every
				// proposal collide with the mail it is meant to keep, and the
				// inducer would return nothing at all.
				continue
			}
			if name, ok := labelNames[lid.String()]; ok {
				ex.OtherUserLabels = append(ex.OtherUserLabels, name)
			}
		}
		examples = append(examples, ex)
	}

	res.Current = scopeOf(rule.Conditions, rows)
	res.CorrectionsInWindow = res.Current.Misfires

	// Nothing in the window to fix means nothing can be verified. Proposing a
	// narrower rule here would be proposing it blind: the scan cannot show that
	// the new form excludes mail it never saw.
	if res.CorrectionsInWindow == 0 {
		return mustMarshal(res), nil
	}

	currentKey := conditionJSON(rule.Conditions)
	for _, c := range rules.Induce(examples) {
		if len(res.Proposals) >= tightenMaxProposals {
			break
		}
		// A proposal identical to the rule it replaces is not a proposal.
		if conditionJSON(c.Conditions) == currentKey {
			continue
		}
		scope := scopeOf(c.Conditions, rows)
		// THE FILTER THIS METHOD EXISTS FOR: a narrower rule that still catches
		// mail the user rejected has fixed nothing.
		if scope.Misfires > 0 {
			continue
		}
		// And it has to actually be narrower. The inducer is scored over the whole
		// window and knows nothing about the rule being replaced, so it can return
		// a form that is wider elsewhere — which would trade four wrong messages
		// for forty.
		if scope.Matched > res.Current.Matched {
			continue
		}
		out := tightenedRule{
			Conditions:     c.Conditions,
			ruleScope:      scope,
			Fixes:          res.CorrectionsInWindow,
			Conflicts:      c.Conflicts,
			ConflictLabels: c.ConflictLabels,
			Samples:        []rulePreviewSample{},
		}
		if out.ConflictLabels == nil {
			out.ConflictLabels = []string{}
		}
		for _, r := range rows {
			if len(out.Samples) >= tightenMaxSamples {
				break
			}
			if !r.kept || c.Conditions.Matches(r.facts) {
				continue
			}
			out.Samples = append(out.Samples, rulePreviewSample{
				ID:         r.cand.ID.String(),
				Subject:    r.cand.Subject,
				From:       r.cand.From,
				ReceivedAt: jmapDate(r.cand.ReceivedAt),
			})
		}
		res.Proposals = append(res.Proposals, out)
	}
	return mustMarshal(res), nil
}

// tightenRow is one scanned message with everything a scoring pass needs, so the
// facts are projected once rather than once per proposal (projection can open a
// blob).
type tightenRow struct {
	cand      store.RulePreviewCandidate
	facts     rules.MessageFacts
	labelled  bool
	corrected bool
	kept      bool
}

// scopeOf measures a set of conditions against the scanned window.
//
// The SAME function scores the current rule and every proposal — that symmetry
// is the point. Two numbers produced by two code paths would eventually disagree
// about what "matched" means, and the entire value of this method is that the
// before and after can be read side by side.
func scopeOf(cond rules.Condition, rows []tightenRow) ruleScope {
	var s ruleScope
	for _, r := range rows {
		if !cond.Matches(r.facts) {
			// Correctly labelled mail this form no longer catches. For the current
			// rule this is 0 by construction (kept is defined by its own matches),
			// which is why the field only means something on a proposal.
			if r.kept {
				s.Drops++
			}
			continue
		}
		s.Matched++
		switch {
		case r.labelled:
			s.Keeps++
		case r.corrected:
			s.Misfires++
		default:
			s.Collateral++
		}
	}
	return s
}

// isTargetLabel reports whether a label is one the corrections name — i.e. one
// this tightening is ABOUT. Those must not count as conflicts: the rule applying
// its own label is the situation, not a disagreement with it.
func isTargetLabel(id ulid.ULID, corrections []store.RuleCorrection) bool {
	for _, c := range corrections {
		if c.LabelID == id {
			return true
		}
	}
	return false
}

// conditionJSON renders conditions canonically enough to compare two forms.
// rules has its own canonical key for dedup, but it is unexported and this only
// needs to answer "is this the same rule I already have".
func conditionJSON(c rules.Condition) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}
