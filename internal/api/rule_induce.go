// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Rule/induce — "you labelled two of these Promotions; shall I keep doing it?"
// (spec §8, mailroom#168, epic #156).
//
// The algorithm lives in internal/rules (pure, unit-tested against the Wayfair
// acceptance case). This method is the part that makes it real: it feeds the
// inducer the user's ACTUAL MAIL rather than a fixture, and reports what each
// proposal would have done.
//
// IT CREATES NOTHING. A candidate is a proposal a human accepts, edits or
// dismisses — the client saves it with Rule/set like any other rule. An
// automation that writes its own rules is one the user cannot predict, and the
// rules list would stop describing what actually happens.
//
// EVIDENCE IS THE USER'S OWN LABELLING, never a rule's or the model's output
// (store.RulePreviewCandidate.UserLabelIDs, via the applied_by provenance added
// in migration 00017). Learning from automation output lets a wrong rule confirm
// itself: its labels become training data, inducing a rule that agrees with it,
// forever.
//
// ── OR A FOLDER, WHICH IS THE SAME KIND OF EVIDENCE (#446 step 3) ────────────
//
// `mailboxId` is the alternative to `labelId`: the positive set is the mail with
// LIVE PROVENANCE IN THAT FOLDER, and the rest of the method is unchanged.
//
// FOLDER MEMBERSHIP IS THE PROVIDER'S OWN SORTING, AND THAT IS EVIDENCE OF WHAT
// THE USER WANTS in exactly the way a hand-applied label is. Somebody who has
// been dragging mail into "Receipts" for four years has stated a preference as
// clearly as somebody who clicked a label — more clearly, in fact, because they
// did it repeatedly. And on the day a mailbox arrives from another client it is
// the ONLY evidence available: there are no hand-applied Mailroom labels yet, so
// induction over labels has nothing to learn from on the one day a rule is worth
// the most.
//
// It is NOT a weakening of the rule above. The folder is the USER's filing, not
// an automation's output; what that rule excludes is Mailroom's own guesses
// confirming themselves, and a provider folder is neither. (A folder filled by a
// server-side filter the user wrote is still the user's own rule, expressed
// somewhere else.)

// minInduceExamples is how many hand-labelled messages it takes before asking
// for a rule at all.
//
// ONE is not evidence — every message shares its own sender, subject and list
// with itself, so the "narrowest consistent rule" from a single example is just
// that message described back. Two is the smallest number that can disagree, and
// disagreement is what makes the generalisation mean anything.
//
// It is a floor on the LABEL, not a promise of a proposal: rules are induced per
// CLUSTER of related senders (#173), so a label with twenty examples spread
// across twenty senders clears this bar and still yields nothing — correctly, as
// there is no rule in it. That mail is the long tail the classifier handles.
const minInduceExamples = 2

// induceMaxSamples bounds the examples shown per candidate.
const induceMaxSamples = 8

// induceCandidates caps how many proposals come back — one per cluster of
// related senders, biggest evidence first (#173). A label covering many
// retailers can imply several unrelated rules, and they are separate decisions;
// but a wall of proposals is one nobody reads, so the strongest few are offered
// and the rest wait until those are dealt with.
const induceCandidates = 3

type ruleInduceArgs struct {
	AccountID string `json:"accountId"`
	// LabelID is the label to learn: its hand-applied messages are the positive
	// examples.
	LabelID string `json:"labelId"`
	// MailboxID is the FOLDER to learn (#446 step 3): its live-provenance
	// messages are the positive examples instead. EXACTLY ONE of labelId and
	// mailboxId must be set — both would make "positive" mean two different
	// things in one call, and the honest answer to that is invalidArguments
	// rather than a silent precedence rule the caller cannot see.
	MailboxID string `json:"mailboxId"`
	Limit     int    `json:"limit"`
}

type inducedRule struct {
	Conditions rules.Condition `json:"conditions"`
	// Matched is everything the candidate matches in the scanned window; Covers
	// is how many of the labelled examples that includes, and Collateral the rest
	// — the mail this rule would newly label.
	Matched    int `json:"matched"`
	Covers     int `json:"covers"`
	Collateral int `json:"collateral"`
	// Conflicts is matched mail the user filed under a different label by hand.
	// Induce never proposes a candidate with conflicts, so this is always 0
	// today; it is reported because the client renders proposals and manual
	// previews through the same component, and a silent zero would be a
	// promise rather than a measurement.
	Conflicts      int                 `json:"conflicts"`
	ConflictLabels []string            `json:"conflictLabels"`
	Samples        []rulePreviewSample `json:"samples"`
}

type ruleInduceResult struct {
	AccountID string `json:"accountId"`
	// MailboxID echoes the input when the positives came from a FOLDER, and is
	// absent otherwise. Echoed because a client renders label-induction and
	// folder-induction through the same component and has to be able to tell
	// which answer it is holding.
	MailboxID string `json:"mailboxId,omitempty"`
	// Examples is how many hand-labelled messages were found, and Required how
	// many it takes. Examples < Required is the common answer for a new label,
	// and the client says "label one more" rather than "nothing found".
	Examples int `json:"examples"`
	Required int `json:"required"`
	// Scanned/Total/Complete describe the window the proposals were measured
	// against, exactly as Rule/preview does. A proposal measured over recent mail
	// must not read as measured over everything.
	Scanned  int  `json:"scanned"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
	// Candidates is best-first, or empty when the examples support no rule —
	// which is an honest answer, not a failure. Two messages with nothing in
	// common do not imply a rule, and inventing one would be the confident
	// wrongness this whole epic exists to avoid.
	Candidates []inducedRule `json:"candidates"`
}

func (a *API) ruleInduce(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args ruleInduceArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	wantLabel := strings.TrimSpace(args.LabelID)
	wantMailbox := strings.TrimSpace(args.MailboxID)
	if (wantLabel == "") == (wantMailbox == "") {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "exactly one of labelId and mailboxId must be set"}
	}
	var labelID, mailboxID ulid.ULID
	if wantLabel != "" {
		var err error
		if labelID, err = ulid.Parse(wantLabel); err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "labelId must be a valid id"}
		}
	} else {
		var err error
		if mailboxID, err = ulid.Parse(wantMailbox); err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "mailboxId must be a valid id"}
		}
	}

	limit := args.Limit
	if limit <= 0 {
		limit = previewDefaultScan
	}
	if limit > previewMaxScan {
		limit = previewMaxScan
	}

	var (
		candidates []store.RulePreviewCandidate
		total      int
		labelNames = map[string]string{}
	)
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, limit); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		views, e := tx.ListLabels(ctx)
		if e != nil {
			return &MethodError{Type: errServerFail, Description: e.Error()}
		}
		for _, v := range views {
			labelNames[v.ID.String()] = v.Name
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}

	res := ruleInduceResult{
		AccountID:  userID.String(),
		Required:   minInduceExamples,
		Scanned:    len(candidates),
		Total:      total,
		Candidates: []inducedRule{},
	}
	if wantMailbox != "" {
		res.MailboxID = mailboxID.String()
	}
	res.Complete = res.Scanned >= res.Total

	// Induction reads only stored columns (sender, domain, list, unsubscribe), so
	// no message body is opened — the whole scan is one query's worth of work.
	target := labelID.String()
	examples := make([]rules.Example, 0, len(candidates))
	byIndex := make([]store.RulePreviewCandidate, 0, len(candidates))
	for _, c := range candidates {
		ex := rules.Example{Facts: a.factsFor(ctx, userID, c, false, nil)}
		if wantMailbox != "" {
			// The FOLDER decides positive. A NON-positive message's own
			// hand-applied labels still supply OtherUserLabels — the conflict
			// signal the inducer uses to refuse a candidate that would also catch
			// mail the user filed elsewhere by hand (rules.Induce reads them only
			// for collateral, which is why the positive branch does not bother) —
			// so folder induction is exactly as conservative about collateral as
			// label induction is.
			for _, mid := range c.MailboxIDs {
				if mid == mailboxID {
					ex.Positive = true
					break
				}
			}
			if !ex.Positive {
				for _, lid := range c.UserLabelIDs {
					if name, ok := labelNames[lid.String()]; ok {
						ex.OtherUserLabels = append(ex.OtherUserLabels, name)
					}
				}
			}
		} else {
			for _, lid := range c.UserLabelIDs {
				if lid.String() == target {
					ex.Positive = true
					continue
				}
				if name, ok := labelNames[lid.String()]; ok {
					ex.OtherUserLabels = append(ex.OtherUserLabels, name)
				}
			}
		}
		if ex.Positive {
			res.Examples++
		}
		examples = append(examples, ex)
		byIndex = append(byIndex, c)
	}

	if res.Examples < minInduceExamples {
		return mustMarshal(res), nil
	}

	for i, c := range rules.Induce(examples) {
		if i >= induceCandidates {
			break
		}
		out := inducedRule{
			Conditions:     c.Conditions,
			Matched:        c.Covers + c.Collateral,
			Covers:         c.Covers,
			Collateral:     c.Collateral,
			Conflicts:      c.Conflicts,
			ConflictLabels: c.ConflictLabels,
			Samples:        []rulePreviewSample{},
		}
		if out.ConflictLabels == nil {
			out.ConflictLabels = []string{}
		}
		// Samples come from the COLLATERAL — the mail this rule would newly
		// label. Showing the examples back would only confirm the rule matches
		// what was already labelled, which is guaranteed and tells nobody
		// anything; what a person needs to see is what ELSE it catches.
		for j, ex := range examples {
			if len(out.Samples) >= induceMaxSamples {
				break
			}
			if ex.Positive || !c.Conditions.Matches(ex.Facts) {
				continue
			}
			m := byIndex[j]
			out.Samples = append(out.Samples, rulePreviewSample{
				ID:         m.ID.String(),
				Subject:    m.Subject,
				From:       m.From,
				ReceivedAt: jmapDate(m.ReceivedAt),
			})
		}
		res.Candidates = append(res.Candidates, out)
	}
	return mustMarshal(res), nil
}
