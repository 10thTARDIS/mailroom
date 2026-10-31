// SPDX-License-Identifier: AGPL-3.0-only

package store

import "github.com/oklog/ulid/v2"

// Who applied a label (app.message_labels.applied_by), spec §8 / #155.
//
// The column has carried this vocabulary since the baseline schema and nothing
// ever wrote to it — every row said 'user' by default, including the ones a
// rule created. That made the one question a surprising label provokes ("why is
// this here?") answerable only with a falsehood. These constants are the
// vocabulary; LabelSource is how a caller states which one it is.
const (
	LabelByUser = "user"
	LabelByRule = "rule"
	LabelByML   = "ml"
	// LabelByAgent: an API KEY applied this label (#49, ADR-042 §3a): an
	// agent acting on the user's behalf through Email/set or the MCP tools.
	//
	// NOT LabelByUser, and that is the whole point of it: every learner reads
	// applied_by = 'user' as "the user decided this" — learned placement's
	// filings (learned_placement.go's placementFilings), the label model's
	// positive and negative evidence (label_model.go), rule induction's
	// UserLabelIDs (rule_preview.go) — so a key's move counted as the user's
	// would let an agent train the mailbox to sort the way the agent sorts.
	// Decision on #49: a key may file mail into areas, and its
	// moves never become the user's filing evidence.
	//
	// The same EmailUpdate carries it as the ACTOR of the whole update
	// (LabelSource.Kind), so ApplyEmailUpdate also skips the evidence a
	// REMOVAL or an archive would record for a person: no rule correction,
	// no learned-placement correction (a key taking a learned label off
	// withdraws the placement instead: PlacementWithdrawn), and no "kept on
	// archive". And a key may not take off a filing the user made by hand
	// (ErrUserFilingProtected): deleting the user's own evidence is training
	// too, by subtraction.
	LabelByAgent = "agent"
	// LabelByDesk: the desk applied this label because the message's TAB VERDICT
	// placed it in one of the operator's areas (#442) — "areas are labels":
	// placing a message in an area IS applying the area's own label. NOT
	// LabelByRule: there is no rule to point at, only a resolved tab. NOT
	// LabelByML: the label classifier never applies a label at all (#169) —
	// label_model.go treats 'ml' as this very model's own output, and the desk's
	// placement is not that. NOT LabelByUser: it would poison the label model's
	// positive evidence (label_model.go's HandAppliedLabels, rule_preview.go's
	// UserLabelIDs both read applied_by = 'user' as "the user decided this") and
	// claim a filing the user never made.
	LabelByDesk = "desk"
	// LabelBySetup: DAY-ONE SETUP applied this label, because the message was
	// sitting in a folder of that name when the mailbox was first brought onto
	// the desk (#446 step 2 — "mail already there"). One bulk, operator-approved
	// act, previewed as counts before it ran and reversible as a whole
	// (SetupUndoChunk), which is why it needs an attribution of its own:
	//
	// NOT LabelByUser: it would poison the label model's positive evidence
	// (label_model.go's HandAppliedLabels and rule_preview.go's UserLabelIDs
	// both read applied_by = 'user' as "the user decided THIS message"), and
	// four thousand rows arriving at once would swamp every real hand-filing in
	// the mailbox. The user approved a PLAN, not four thousand filings.
	//
	// NOT LabelByRule: there is no rule to point at, and a rule's labels are
	// correctable evidence (ApplyEmailUpdate records a rule_correction when one
	// is removed) — undoing a bulk import is not thousands of disagreements
	// with a rule, which is the same distinction EmailUpdate.RemovalIsUndo
	// already draws.
	//
	// NOT LabelByDesk: that means "the message's TAB VERDICT placed it in an
	// area" (area_labels.go). Day-one's evidence is the FOLDER the provider's
	// own sorting put it in, which is a different fact with a different owner,
	// and undo has to be able to tell the two apart: it deletes a label ONLY
	// while it is still applied_by = 'setup'.
	LabelBySetup = "setup"
	// LabelByLearned: LEARNED PLACEMENT filed this new message into the area
	// that owns this label, at ingest, because the user's own filings on its
	// sender all went there (#714, ADR-032, learned_placement.go).
	//
	// NOT LabelByUser, and this is the one that matters most: learned
	// placement's evidence IS applied_by = 'user' rows, so a learned row
	// counted as a filing would make every placement confirm itself, the #169
	// failure exactly (a model trained on its own output confirms its own
	// mistakes). NOT LabelByDesk: the desk means "the tab verdict put it
	// here", and removing a learned label is a CORRECTION that must be
	// recorded (ApplyEmailUpdate), which removing a desk label is not. NOT
	// LabelByRule: there is no rule to point at or to narrow.
	LabelByLearned = "learned"
)

// LabelSource attributes a label application. The ZERO VALUE means the user did
// it by hand — the right default, because the JMAP path is a person acting
// through a client, and an unattributed automation is the failure mode this
// type exists to prevent.
type LabelSource struct {
	// Kind is one of the LabelBy* constants. Empty means LabelByUser.
	Kind string
	// Rule is the rule that applied the label, set when Kind is LabelByRule.
	// Nil otherwise — an ML- or agent-applied label has no rule to point at.
	Rule *ulid.ULID
}

// RuleLabelSource attributes a label to the rule that applied it.
func RuleLabelSource(ruleID ulid.ULID) LabelSource {
	return LabelSource{Kind: LabelByRule, Rule: &ruleID}
}

// SetupLabelSource attributes a label to day-one setup — the folder a message
// was sitting in when the mailbox was first brought onto the desk (#446),
// documented on LabelBySetup above.
func SetupLabelSource() LabelSource {
	return LabelSource{Kind: LabelBySetup}
}

// DeskLabelSource attributes a label to the desk itself — the machinery that
// places a message into an area because its tab verdict decided it (#442),
// documented on LabelByDesk above.
func DeskLabelSource() LabelSource {
	return LabelSource{Kind: LabelByDesk}
}

// LearnedLabelSource attributes a label to learned placement (#714),
// documented on LabelByLearned above.
func LearnedLabelSource() LabelSource {
	return LabelSource{Kind: LabelByLearned}
}

// AgentLabelSource attributes an update to an API key (#49), documented
// on LabelByAgent above.
func AgentLabelSource() LabelSource {
	return LabelSource{Kind: LabelByAgent}
}

// byAgent reports whether the update this source attributes was made by an
// API key: the one actor whose changes are never the user's evidence.
func (s LabelSource) byAgent() bool { return s.Kind == LabelByAgent }

// columns renders the source as the two column values to insert. The rule id is
// returned as *string because pgx maps a nil pointer to SQL NULL, and the FK
// (ON DELETE SET NULL) requires NULL rather than an empty string for "no rule".
//
// A rule id is only ever written alongside Kind == LabelByRule: attributing a
// row to a rule while claiming the user applied it would make applied_by and
// applied_by_rule_id contradict each other, and a reader has no way to tell
// which one lied.
func (s LabelSource) columns() (appliedBy string, ruleID *string) {
	kind := s.Kind
	if kind == "" {
		kind = LabelByUser
	}
	if kind == LabelByRule && s.Rule != nil {
		id := s.Rule.String()
		return kind, &id
	}
	return kind, nil
}
