// SPDX-License-Identifier: AGPL-3.0-only

// Package rules is the deterministic T0 filtering tier (spec §8): user-authored
// (and system-suggested) rules of conditions → actions, evaluated at ingest.
//
// It is the foundation the ML tiers layer on, so it is kept DETERMINISTIC,
// TRANSPARENT and SAFE: a rule can add labels, flag/mark a message, or keep it
// out of the inbox, but it can NEVER drop or lose mail. The package is pure —
// no store, no IMAP, no DOM — so the whole evaluator is unit-testable against a
// small MessageFacts projection the ingest path fills in.
//
// The condition/action Go structs (de)serialize directly to the `conditions`
// and `actions` jsonb columns of app.rules; the store validates them through
// this package before persisting (a rule with an uncompilable regex or an
// unknown/unsupported action is rejected at the store/api boundary, never at
// ingest), and treats a bad regex as NON-MATCHING at eval time rather than
// panicking.
package rules

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Condition field selectors (spec §8, T0). Each names one projected fact.
const (
	FieldFrom       = "from"            // the From address
	FieldFromDomain = "fromDomain"      // the domain part of the From address
	FieldTo         = "to"              // any To/Cc recipient address
	FieldSubject    = "subject"         // the Subject
	FieldAlias      = "alias"           // the derived addressed-identity address
	FieldListID     = "listId"          // the List-Id header value
	FieldListUnsub  = "listUnsubscribe" // the List-Unsubscribe header value
	FieldHeader     = "header"          // an arbitrary named header (requires Header)
)

// Condition operators.
const (
	OpEquals   = "equals"   // case-insensitive full-string equality
	OpContains = "contains" // case-insensitive substring
	OpMatches  = "matches"  // Go (RE2) regexp match on the field
	OpExists   = "exists"   // the field is present / non-empty
)

// Action types. addLabel / suggestLabel / markPriority / flag / skipInbox|archive
// are the SAFE, composable set implemented at ingest; stop halts evaluation after
// the current rule (Sieve-style). moveToMailbox and junk are DEFINED but DEFERRED
// — they are rejected at rule-save time (see ValidateActions), because applying
// them safely needs placement/IMAP-writeback work this tier does not do yet, and
// correctness / no-mail-loss beats completeness.
const (
	ActionAddLabel = "addLabel"
	// ActionSuggestLabel PROPOSES a label instead of applying one (#242).
	//
	// It exists because the suggestion surface — app.label_suggestions,
	// Email.suggestedLabels, the needsCategory filter — is shipped, wired, tested
	// and produces ZERO rows: its only writer is classifyMessage, which returns
	// immediately unless a per-label model has been promoted, and #169's measured
	// verdict is that none should be (AUC 0.62–0.72 ranking coexisting with
	// 0.20–0.60 precision at the threshold that decides what a user sees). The
	// mechanism that demonstrably works on real mail is the RULE
	// INDUCER: induced candidates at zero collateral. #242 therefore sources
	// suggestions from induction rather than from the classifier.
	//
	// ⚠ INDUCTION CANNOT RUN AT INGEST. Induce is a corpus-wide batch operation —
	// it clusters the user's whole labelled history and scores every candidate
	// against every example — so it is nowhere near a per-message cost. The split
	// this action embodies is: induce in BATCH (a CLI proposes candidates, a human
	// accepts), then MATCH per message, which applyRules already does for free.
	// An accepted candidate is persisted as an ORDINARY app.rules row carrying
	// this action, so it gets rule editing, rule priority, Rule/tighten and rule
	// provenance without a second engine.
	//
	// It is deliberately a SEPARATE action type rather than a flag on addLabel:
	// the two are executed by different switch cases writing to different tables
	// (store.applyRuleAction), so "a suggestion was silently applied as a label"
	// is not a bug that can be introduced by getting a boolean wrong.
	ActionSuggestLabel  = "suggestLabel"
	ActionMarkPriority  = "markPriority"
	ActionFlag          = "flag"
	ActionSkipInbox     = "skipInbox"
	ActionArchive       = "archive" // synonym for skipInbox (out of inbox, still stored)
	ActionStop          = "stop"
	ActionMoveToMailbox = "moveToMailbox" // deferred (rejected at save)
	ActionJunk          = "junk"          // deferred (rejected at save)
)

// KeywordImportant is the additive keyword markPriority sets on a message (a
// `$important`-style JMAP keyword). It is purely additive and never removes
// anything, so it can never lose state.
const KeywordImportant = "$important"

// maxRegexLen bounds a rule's regex pattern length. Go's regexp is RE2 (linear
// time, no catastrophic backtracking), so the real risk is not ReDoS but a
// pathologically large pattern; a length cap plus the compile-check at save
// time keeps rules cheap and total.
const maxRegexLen = 512

// Condition is one node of the boolean condition tree. It is EITHER a boolean
// group (exactly one of All/Any set, nestable) OR a leaf (Field set). A node
// that is neither matches nothing — a malformed rule does nothing rather than
// acting on all mail (safe default).
type Condition struct {
	All []Condition `json:"all,omitempty"`
	Any []Condition `json:"any,omitempty"`

	Field  string `json:"field,omitempty"`
	Op     string `json:"op,omitempty"`
	Value  string `json:"value,omitempty"`
	Header string `json:"header,omitempty"` // header name when Field == FieldHeader
}

// Action is one thing a matched rule does. Type is required; Label carries the
// target for addLabel and suggestLabel (a label id or a keyword atom); Mailbox is
// reserved for the deferred moveToMailbox.
type Action struct {
	Type    string `json:"type"`
	Label   string `json:"label,omitempty"`
	Mailbox string `json:"mailbox,omitempty"`
	// Evidence is suggestLabel ONLY: how many of the user's OWN filings backed
	// this candidate when it was induced (Candidate.Positives). It is stamped once,
	// at rule creation, because it describes the moment of induction and not the
	// rule's present reach — re-deriving it at ingest would mean re-running Induce
	// per message, which is the batch/match split this action exists to avoid.
	//
	// It feeds exactly two things, both of them PRESENTATION: EvidenceConfidence,
	// which turns it into the ordering rank app.label_suggestions.confidence needs
	// (see suggest.go — emphatically not a probability), and the "backed by N
	// messages you filed" sentence on the queue row. Nothing branches on it, so a
	// stale or absent count degrades an ORDER BY and a phrase, never a decision.
	// That is why it is not validated: rejecting a rule over an advisory integer
	// would be a rule the user cannot save, for no safety gained.
	Evidence int `json:"evidence,omitempty"`
}

// Rule is a stored T0 rule. It is the shared type the store returns and the
// evaluator consumes; ID is unset for a not-yet-persisted rule.
type Rule struct {
	ID              ulid.ULID
	Name            string
	Priority        int
	Conditions      Condition
	Actions         []Action
	Enabled         bool
	SystemSuggested bool
}

// MessageFacts is the DOM-free projection the ingest path fills from a parsed
// message. Every field is the caller's responsibility; the evaluator only reads
// it. Addresses/domains/alias are matched case-insensitively.
type MessageFacts struct {
	From            string            // From address
	FromDomain      string            // domain of the From address (lowercased)
	To              []string          // To + Cc recipient addresses
	Subject         string            // Subject
	Alias           string            // the derived addressed-identity address
	ListID          string            // List-Id header value ("" if absent)
	ListUnsubscribe string            // List-Unsubscribe header value ("" if absent)
	Headers         map[string]string // arbitrary headers, keyed by LOWERCASED name
}

// AttributedAction is one action together with the rule that produced it
// (#155). Evaluate's flat []Action cannot say WHICH rule contributed a given
// action, and that is exactly what a user needs when a label surprises them —
// with several labelling rules, "a rule did this" is not an answer.
type AttributedAction struct {
	Action
	RuleID ulid.ULID
	// Conditions is the matched rule's LIVE condition tree, carried alongside the
	// action so an executor can say WHY the rule fired (#242). suggestLabel needs
	// it: a suggestion's stored reasons are DescribeCondition of this tree, and
	// without it the store would have to re-read app.rules by RuleID per action —
	// a query per matched action, for data the evaluator was already holding.
	//
	// ⚠ THE TREE, NOT PRE-RENDERED TEXT, AND THAT IS THE POINT. The alternative
	// was to bake the phrases into the Action at rule-creation time (the inducer
	// knows them then) and store the strings. That drifts the first time a user
	// runs Rule/tighten: the narrowing rewrites app.rules.conditions, the baked
	// text does not move with it, and every subsequent suggestion cites a
	// condition the rule no longer has — a stored explanation that is a claim
	// about code, and false. Rendering from the tree the evaluator ACTUALLY
	// MATCHED makes the reasons unable to describe anything but what fired.
	//
	// It is a copy of the rule's field, so mutating it here cannot affect the
	// caller's rules; the Condition tree is read-only to every consumer anyway.
	Conditions Condition
}

// Evaluate returns the actions to apply to a message, given the user's ENABLED
// rules already ordered by priority (lower first). Semantics are ALL-APPLY, like
// Sieve: every matching rule contributes its actions, in priority order, unless
// a matched rule carries a `stop` action — that rule's other actions still
// apply, then evaluation halts. The returned slice preserves that order; a
// `stop` action is consumed here and never returned. It is a pure function.
func Evaluate(rs []Rule, f MessageFacts) []Action {
	att := EvaluateAttributed(rs, f)
	if len(att) == 0 {
		return nil
	}
	out := make([]Action, 0, len(att))
	for _, a := range att {
		out = append(out, a.Action)
	}
	return out
}

// EvaluateAttributed is Evaluate, keeping each action's originating rule. The
// semantics are identical — Evaluate is defined in terms of this — so there is
// one evaluation order and one `stop` implementation, not two that can drift.
func EvaluateAttributed(rs []Rule, f MessageFacts) []AttributedAction {
	var out []AttributedAction
	for _, r := range rs {
		if !r.Enabled {
			continue
		}
		if !r.Conditions.match(f) {
			continue
		}
		stop := false
		for _, a := range r.Actions {
			if a.Type == ActionStop {
				stop = true
				continue
			}
			out = append(out, AttributedAction{Action: a, RuleID: r.ID, Conditions: r.Conditions})
		}
		if stop {
			break
		}
	}
	return out
}

// Matches reports whether this condition tree matches the facts. It is the
// exported form of the matcher, for callers that need to ask "would this match?"
// without applying anything — the blast-radius preview (#155). Evaluate cannot
// answer that: a rule whose only action is `stop` matches but contributes
// nothing, so an empty action list does not mean "did not match".
func (c Condition) Matches(f MessageFacts) bool { return c.match(f) }

// match reports whether the condition tree matches the facts.
func (c Condition) match(f MessageFacts) bool {
	switch {
	case len(c.All) > 0:
		for _, sub := range c.All {
			if !sub.match(f) {
				return false
			}
		}
		return true
	case len(c.Any) > 0:
		for _, sub := range c.Any {
			if sub.match(f) {
				return true
			}
		}
		return false
	default:
		return c.matchLeaf(f)
	}
}

func (c Condition) matchLeaf(f MessageFacts) bool {
	switch c.Field {
	case FieldFrom:
		return matchStr(c, f.From)
	case FieldFromDomain:
		return matchStr(c, f.FromDomain)
	case FieldSubject:
		return matchStr(c, f.Subject)
	case FieldAlias:
		return matchStr(c, f.Alias)
	case FieldListID:
		return matchStr(c, f.ListID)
	case FieldListUnsub:
		return matchStr(c, f.ListUnsubscribe)
	case FieldHeader:
		return matchStr(c, f.Headers[strings.ToLower(c.Header)])
	case FieldTo:
		return matchList(c, f.To)
	default:
		return false
	}
}

// matchStr applies the leaf operator to a single string field. A bad regex is
// treated as NON-MATCHING (never a panic, never a match), so a rule that somehow
// carried an uncompilable pattern past validation can only ever fail to match.
func matchStr(c Condition, v string) bool {
	switch c.Op {
	case OpExists:
		return strings.TrimSpace(v) != ""
	case OpEquals:
		return strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(c.Value))
	case OpContains:
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.Value))
	case OpMatches:
		re, err := regexp.Compile(c.Value)
		if err != nil {
			return false
		}
		return re.MatchString(v)
	default:
		return false
	}
}

// matchList applies the leaf operator across a list field (recipients): exists
// is true when any non-empty value is present; every other operator matches if
// ANY element matches.
func matchList(c Condition, vs []string) bool {
	if c.Op == OpExists {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
		return false
	}
	for _, v := range vs {
		if matchStr(c, v) {
			return true
		}
	}
	return false
}

// HeaderNames returns the distinct arbitrary-header names referenced by header
// leaves across the rules, so the ingest path can populate MessageFacts.Headers
// with exactly the headers the loaded rules need (and no more). List-Id and
// List-Unsubscribe have their own dedicated fields and are not returned here.
func HeaderNames(rs []Rule) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(c Condition)
	walk = func(c Condition) {
		for _, s := range c.All {
			walk(s)
		}
		for _, s := range c.Any {
			walk(s)
		}
		if c.Field == FieldHeader && c.Header != "" {
			k := strings.ToLower(c.Header)
			if !seen[k] {
				seen[k] = true
				out = append(out, c.Header)
			}
		}
	}
	for _, r := range rs {
		walk(r.Conditions)
	}
	return out
}

// UsesField reports whether the tree tests the named field anywhere.
//
// It exists next to HeaderNames because callers need the same thing of both:
// which facts must be gathered before this rule can be evaluated HONESTLY. A
// rule testing `to` needs the message body parsed; evaluated against a
// MessageFacts with an empty To, it silently matches less than it should, and
// the caller reports a confident count that is simply wrong. Every caller that
// asks HeaderNames what to load has to ask this too, so the two live together
// rather than one of them being re-derived per call site (mailroom#193).
func UsesField(c Condition, field string) bool {
	if c.Field == field {
		return true
	}
	for _, sub := range c.All {
		if UsesField(sub, field) {
			return true
		}
	}
	for _, sub := range c.Any {
		if UsesField(sub, field) {
			return true
		}
	}
	return false
}

// Validate checks a whole rule: its condition tree and its action list. It is
// the single gate the store applies before persisting (and the API surfaces the
// message as a clean SetError).
func Validate(r Rule) error {
	if err := ValidateConditions(r.Conditions); err != nil {
		return err
	}
	return ValidateActions(r.Actions)
}

// ValidateConditions rejects a malformed condition tree: a node that is both a
// group and a leaf, both all AND any, an unknown field/op, a header leaf with no
// header name, a value-taking op with no value, and — the load-bearing check —
// an uncompilable or over-long regex (rejected HERE, at save time, so ingest
// never meets one).
func ValidateConditions(c Condition) error {
	branch := len(c.All) > 0 || len(c.Any) > 0
	leaf := c.Field != ""
	if branch && leaf {
		return fmt.Errorf("condition cannot be both a boolean group and a leaf")
	}
	if branch {
		if len(c.All) > 0 && len(c.Any) > 0 {
			return fmt.Errorf("condition cannot set both all and any")
		}
		subs := c.All
		if len(c.Any) > 0 {
			subs = c.Any
		}
		for _, s := range subs {
			if err := ValidateConditions(s); err != nil {
				return err
			}
		}
		return nil
	}
	if !leaf {
		return fmt.Errorf("condition must be a leaf (field) or a boolean group (all/any)")
	}
	if !validField(c.Field) {
		return fmt.Errorf("unknown condition field %q", c.Field)
	}
	if c.Field == FieldHeader && strings.TrimSpace(c.Header) == "" {
		return fmt.Errorf("header condition requires a header name")
	}
	switch c.Op {
	case OpExists:
	case OpEquals, OpContains:
		if c.Value == "" {
			return fmt.Errorf("%s condition requires a value", c.Op)
		}
	case OpMatches:
		if c.Value == "" {
			return fmt.Errorf("matches condition requires a pattern")
		}
		if len(c.Value) > maxRegexLen {
			return fmt.Errorf("regex pattern too long (max %d bytes)", maxRegexLen)
		}
		if _, err := regexp.Compile(c.Value); err != nil {
			return fmt.Errorf("invalid regex %q: %v", c.Value, err)
		}
	default:
		return fmt.Errorf("unknown condition op %q", c.Op)
	}
	return nil
}

// ValidateActions rejects an empty action list, an unknown action type, an
// addLabel or suggestLabel with no label, and the deferred moveToMailbox / junk
// actions (which are recognized but not yet supported and must not be silently
// stored).
func ValidateActions(as []Action) error {
	if len(as) == 0 {
		return fmt.Errorf("a rule must have at least one action")
	}
	for _, a := range as {
		switch a.Type {
		case ActionAddLabel:
			if strings.TrimSpace(a.Label) == "" {
				return fmt.Errorf("addLabel action requires a label")
			}
		// suggestLabel carries the SAME requirement as addLabel — a label to name
		// — but keeps its own arm rather than sharing addLabel's. Two reasons, and
		// neither is style. The error text has to name the action the user actually
		// wrote, or a Rule/set SetError tells them to fix an addLabel they do not
		// have. And more importantly, EVERY switch over action types in this
		// codebase now lists the two separately, so adding a third rule-driven
		// labelling action cannot accidentally inherit addLabel's write path by
		// falling into a shared arm (see store.applyRuleAction, where the same
		// separation is what makes "a suggestion cannot be applied as a label" a
		// property of the code rather than a promise in a comment).
		//
		// Evidence is deliberately unvalidated: see Action.Evidence.
		case ActionSuggestLabel:
			if strings.TrimSpace(a.Label) == "" {
				return fmt.Errorf("suggestLabel action requires a label")
			}
		case ActionMarkPriority, ActionFlag, ActionSkipInbox, ActionArchive, ActionStop:
			// safe, parameterless actions
		case ActionMoveToMailbox, ActionJunk:
			return fmt.Errorf("action %q is not supported yet", a.Type)
		case "":
			return fmt.Errorf("action requires a type")
		default:
			return fmt.Errorf("unknown action %q", a.Type)
		}
	}
	return nil
}

func validField(field string) bool {
	switch field {
	case FieldFrom, FieldFromDomain, FieldTo, FieldSubject, FieldAlias,
		FieldListID, FieldListUnsub, FieldHeader:
		return true
	default:
		return false
	}
}

// ParseConditions unmarshals the `conditions` jsonb into a Condition tree. The
// store uses it (with Validate) to reject a rule that cannot round-trip.
func ParseConditions(b []byte) (Condition, error) {
	var c Condition
	if err := json.Unmarshal(b, &c); err != nil {
		return Condition{}, fmt.Errorf("rules: parse conditions: %w", err)
	}
	return c, nil
}

// ParseActions unmarshals the `actions` jsonb into an action list.
func ParseActions(b []byte) ([]Action, error) {
	var a []Action
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("rules: parse actions: %w", err)
	}
	return a, nil
}
