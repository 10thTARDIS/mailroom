// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Running EVERY saved rule over the mail you already have (mailroom#193).
//
// #191 gave one rule an Apply button, inside the rule editor. That is the right
// shape for a rule you just wrote and are still deciding about, and the wrong
// shape for the situation induction actually produces: on a real
// instance, dozens of saved rules against hundreds of messages, every one of them induced FROM
// that mail and none of them ever run over it. Closing that by hand is a trip
// through the editor per rule, so it does not get done, and the classification loop stays
// shipped and unexercised — `rule_corrections` empty, which is exactly the
// evidence #170's tighten path and #169 both need.
//
// It is the same operator problem accept-rules has: the only write path for this
// is JMAP, JMAP needs an interactive OIDC session, and someone with shell access
// and a hundred rules in front of them has no way to act on them in bulk. The
// answer is the same too — a front door onto the SAME store functions, never a
// second write path. Every label here goes through ApplyEmailUpdate, which is
// what appends the change_log row and enqueues the IMAP writeback.
//
// REPORT ONLY BY DEFAULT, like accept-rules and backfill-alias. This one earns it
// more than either: it is the largest single write in the tool, and "it ran and
// changed nothing" must not look like "it ran and relabelled six hundred
// messages" from the outside.
//
// THE NUMBER THE REPORT IS FOR is not the total. It is the mail a rule would
// label X that the user filed as Y BY HAND — each one either a second label they
// would want or a rule that is too broad, and the only part of a 700-message
// backfill a person can usefully read before it happens. A bare "would change
// 186" is not reviewable; forty lines of "you called this Shopping, this rule
// says Newsletters" is.

// runApplyRules reports, and optionally performs, a backfill of every enabled
// rule over the existing mailbox.
func runApplyRules(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply-rules", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		limit     = fs.Int("limit", 5000, "how many of the most recent messages to scan")
		only      = fs.String("rule", "", "run only this rule id (default: every enabled rule)")
		apply     = fs.Bool("apply", false, "actually write the change (default: report only)")
		undo      = fs.Bool("undo", false, "REMOVE the labels these rules applied, instead of applying them")
		maxMut    = fs.Int("max-mutations", 1000, "stop after changing this many messages")
		showConf  = fs.Int("show-conflicts", 20, "how many disagreements with your own filing to print (0 for all)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	run := &backfillRun{
		ctx:      ctx,
		userID:   userID,
		limit:    *limit,
		only:     strings.TrimSpace(*only),
		apply:    *apply,
		undo:     *undo,
		maxMut:   *maxMut,
		showConf: *showConf,
	}

	// ONE TRANSACTION for the whole thing, for the reason the JMAP method gives:
	// a half-applied backfill is worse than a slow one, because the operator is
	// left looking at a partial result with no way to tell which half happened.
	// In report mode the transaction is simply never written to.
	if err := d.store.UserTx(ctx, userID, run.do); err != nil {
		return err
	}
	run.report()
	return nil
}

// backfillRun is one invocation: its inputs, and everything the report needs.
//
// Gathered into a struct so the MATCH LOOP IS WRITTEN ONCE and both modes share
// it. A report produced by different code than the apply is a report that can
// disagree with the thing it claims to predict, which for a write this size is
// the whole risk being managed.
type backfillRun struct {
	// ctx is carried on the struct because store.UserTx's callback takes only the
	// transaction, and every store call inside it still needs the cancellation the
	// signal handler installed.
	ctx      context.Context
	userID   ulid.ULID
	limit    int
	only     string
	apply    bool
	undo     bool
	maxMut   int
	showConf int

	scanned  int
	total    int
	labelled int

	rulesSeen int
	outcomes  []backfillOutcome
	changes   []backfillChange

	// mutations counts messages actually changed (or, in report mode, that would
	// be), across every rule — the quantity --max-mutations bounds.
	mutations int
	// upperBound records that at least one rule also flags or marks priority, so
	// the report's "new" column is an upper bound rather than a count. See
	// backfillOutcome.predictable.
	upperBound bool
}

// backfillOutcome is one rule's result.
type backfillOutcome struct {
	rule   rules.Rule
	labels []string
	// skippedActions are the rule's actions a backfill refuses (skipInbox,
	// archive, stop), named so the report can say WHICH part did not run rather
	// than leaving the operator to notice nothing moved.
	skippedActions []string
	// inert is a rule whose every action is one a backfill refuses. Distinct from
	// "matched nothing", which looks identical in the numbers.
	inert bool
	// needBodies is a rule this tool refuses to evaluate at all, because its
	// conditions test recipients or arbitrary headers and neither is on the
	// message row. See the note at the call site: reporting a number for such a
	// rule would be reporting a wrong one.
	needBodies bool
	// predictable is false when the rule also flags or marks priority. Those bits
	// are not on the candidate row, so report mode cannot tell an already-flagged
	// message from a fresh one, and counts it as new.
	predictable bool

	matched   int
	changed   int
	already   int
	corrected int
	deferred  int
	// mispredicted is messages the report expected to change that the database
	// then declined to change. It must be zero; see the state check in oneRule.
	mispredicted int

	// undoable is the messages an --undo would take this rule's labels off.
	undoable int
	undone   int

	// area is set when the rule files into an area (#728): it is a swap there,
	// not an add, exactly as the rule editor's apply does it (#708), and these
	// are the per-outcome counts of store.PlanAreaFiling. Counts per outcome and
	// never one total: "40 moved" and "40 left where you filed them" are
	// different reports.
	area *areaOutcome
}

// areaOutcome is store.PlanAreaFiling's verdicts over the messages a rule
// matched. Move + alreadyThere + the three contested sum to the matched,
// uncorrected mail the plan covered.
type areaOutcome struct {
	move, alreadyThere                               int
	contestedByUser, contestedByRule, contestedOther int
}

func (a *areaOutcome) count(outcome string) {
	switch outcome {
	case store.AreaFilingMove:
		a.move++
	case store.AreaFilingAlreadyThere:
		a.alreadyThere++
	case store.AreaFilingContestedByUser:
		a.contestedByUser++
	case store.AreaFilingContestedByRule:
		a.contestedByRule++
	case store.AreaFilingContestedOther:
		a.contestedOther++
	}
}

// backfillChange is one message a rule would newly label.
//
// hand names the labels the USER applied that this rule is not adding, and is
// empty when there are none — which is what separates "the automation and I
// disagree about this message" from "the automation is filing something I never
// filed", the two halves of the review.
type backfillChange struct {
	msg  store.RulePreviewCandidate
	rule rules.Rule
	adds string
	hand []string
}

func (r *backfillRun) do(tx *store.UserTx) error {
	rs, err := r.rulesToRun(tx)
	if err != nil {
		return err
	}
	r.rulesSeen = len(rs)

	candidates, err := tx.RulePreviewCandidates(r.ctx, r.limit)
	if err != nil {
		return err
	}
	if r.total, err = tx.CountPreviewableMessages(r.ctx); err != nil {
		return err
	}
	labels, err := tx.ListLabels(r.ctx)
	if err != nil {
		return err
	}
	names := map[ulid.ULID]string{}
	for _, l := range labels {
		names[l.ID] = l.Name
	}

	r.scanned = len(candidates)
	ids := make([]ulid.ULID, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
		if len(c.UserLabelIDs) > 0 {
			r.labelled++
		}
	}

	// WHAT IS ALREADY ON THE MAIL — every label, whoever applied it, because that
	// is what decides whether ApplyEmailUpdate does anything.
	//
	// The first version of this asked only for the user's own labels plus the ones
	// THIS rule had applied, and it was wrong in a way that took a run against
	// a real mailbox to see: a message another rule has already labelled came out as
	// "would be newly labelled", the apply then correctly changed nothing, and the
	// next run said exactly the same thing. Fifteen predicted, three changed, and
	// twelve reported forever. Two duplicate rules are enough to produce it, and
	// this mailbox has three pairs.
	//
	// Mutated as the run proceeds, so a rule adding a label an EARLIER rule in the
	// same run already added sees it there. In apply mode the database would say so
	// anyway; carrying it in memory is what makes report mode give the same answer,
	// which is the entire reason the report is worth reading.
	present, err := tx.LabelsOnMessages(r.ctx, ids)
	if err != nil {
		return err
	}
	has := make(map[ulid.ULID]map[ulid.ULID]bool, len(present))
	for mid, labels := range present {
		set := make(map[ulid.ULID]bool, len(labels))
		for _, lid := range labels {
			set[lid] = true
		}
		has[mid] = set
	}

	// DID ANYTHING ACTUALLY HAPPEN? Same trick as the JMAP method: ApplyEmailUpdate
	// short-circuits when a message already carries everything the update asks
	// for, appending no change_log row and returning the state unmoved. So a state
	// that advanced IS the changed signal. Threaded across all rules, because the
	// question is per-write, not per-rule.
	//
	// Kept even though `has` now predicts the same thing, as a CHECK on it: the two
	// must agree, and a run that reports a mispredicted count above zero is telling
	// you the report cannot be trusted. That is worth knowing loudly rather than
	// silently absorbing into "already".
	state, err := tx.ChangeState(r.ctx, store.ObjectEmail)
	if err != nil {
		return err
	}

	for _, rule := range rs {
		out, err := r.oneRule(tx, rule, candidates, ids, names, has, &state)
		if err != nil {
			return err
		}
		r.outcomes = append(r.outcomes, out)
	}
	return nil
}

// rulesToRun resolves --rule, or every enabled rule.
//
// A DISABLED rule is refused even when named explicitly, matching the JMAP
// method: the user switched it off, and running it over their history anyway
// would be the automation arguing with them.
func (r *backfillRun) rulesToRun(tx *store.UserTx) ([]rules.Rule, error) {
	if r.only == "" {
		return tx.ListRules(r.ctx, true)
	}
	id, err := ulid.Parse(r.only)
	if err != nil {
		return nil, fmt.Errorf("--rule must be a rule id (ULID): %w", err)
	}
	rs, err := tx.GetRules(r.ctx, []ulid.ULID{id})
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("no rule %s", r.only)
	}
	if !rs[0].Enabled {
		return nil, fmt.Errorf("rule %s is turned off; turn it on before applying it to existing mail", r.only)
	}
	return rs, nil
}

// oneRule evaluates a single rule over the scanned mail, and applies it when
// asked. The counting and the writing are the same pass on purpose.
func (r *backfillRun) oneRule(
	tx *store.UserTx,
	rule rules.Rule,
	candidates []store.RulePreviewCandidate,
	ids []ulid.ULID,
	names map[ulid.ULID]string,
	has map[ulid.ULID]map[ulid.ULID]bool,
	state *int64,
) (backfillOutcome, error) {
	ctx := r.ctx
	out := backfillOutcome{rule: rule, predictable: true}

	update, skipped, err := tx.BackfillUpdate(ctx, rule)
	if err != nil {
		return out, err
	}
	out.skippedActions = skipped

	// AN UNDO TAKES NONE OF THE PATH BELOW, and that is not a shortcut.
	//
	// Everything from here to the match loop is about deciding what this rule
	// WOULD do to mail it has not touched: its current actions, whether its
	// conditions can be evaluated without message bodies, which of its labels the
	// user has since removed. An undo asks none of that. It reads what the rule
	// ALREADY DID, from the stored attribution.
	//
	// Routing it through the checks above would break it in two specific ways, and
	// both would look like success. A rule whose addLabel action was edited away
	// resolves to an empty update, so `inert` would report "nothing to undo" about
	// labels that are still sitting on the mail. And a rule testing recipients gets
	// refused for want of a blob store — but undoing it needs no facts at all, so
	// the refusal would strand exactly the labels a user is most likely to want
	// removed.
	if r.undo {
		mine, e := tx.LabelsAppliedByRule(ctx, rule.ID, ids)
		if e != nil {
			return out, e
		}
		return r.undoRule(tx, out, ids, mine, names, state)
	}

	if update.DoesNothing() {
		out.inert = true
		return out, nil
	}
	for _, lid := range update.AddLabelIDs {
		if n, ok := names[lid]; ok {
			out.labels = append(out.labels, n)
		}
	}
	if update.Flagged != nil || len(update.AddKeywords) > 0 {
		out.predictable = false
		r.upperBound = true
	}

	// A RULE THIS TOOL WILL NOT GUESS AT. `to` and arbitrary headers are not on
	// the message row — there is no recipients table — so they can only be
	// answered by decrypting and parsing the message, and this tool has no blob
	// store. Evaluated anyway, against a MessageFacts with an empty To, such a
	// rule matches strictly less than it should and the report states a confident
	// number that is simply wrong. Refusing and naming it is the honest failure:
	// the web client can still apply it, because the server there does hold the
	// bytes.
	if len(rules.HeaderNames([]rules.Rule{rule})) > 0 || rules.UsesField(rule.Conditions, rules.FieldTo) {
		out.needBodies = true
		return out, nil
	}

	// Messages the user took this rule's label off (#170). Skipped entirely: a
	// backfill that re-applies a correction has undone the user's own work, and
	// would do it again on every run. Asked for at the size of the scan, because
	// the default limit of 100 would silently let corrections past the edge of it
	// be re-applied.
	corrections, err := tx.RuleCorrections(ctx, rule.ID, len(candidates)+1)
	if err != nil {
		return out, err
	}
	corrected := map[ulid.ULID]bool{}
	for _, c := range corrections {
		corrected[c.MessageID] = true
	}

	// AN AREA-FILING RULE (#728) is a swap, not an add: the other areas' desk and
	// setup labels come off so the target actually wins the labels-first
	// resolution, and mail the user or another rule filed elsewhere is left where
	// it is and reported. This is the plan Rule/preview reports and Rule/apply
	// performs (store.PlanAreaFiling); an add-only backfill left a matched
	// message in its earlier area, which is not what the rule says. Every other
	// rule takes none of this.
	//
	// In report mode the plan reads the database as it stands, so a second area
	// rule over mail a first one would move in the same run is planned against the
	// pre-run state; apply mode plans each rule after the previous one's writes.
	target, isArea, err := tx.AreaFilingTargetFor(ctx, update.AddLabelIDs)
	if err != nil {
		return out, err
	}
	var plan map[ulid.ULID]store.AreaFilingStep
	if isArea {
		var hits []ulid.ULID
		for _, c := range candidates {
			if rule.Conditions.Matches(factsFromCandidate(c)) && !corrected[c.ID] {
				hits = append(hits, c.ID)
			}
		}
		if plan, err = tx.PlanAreaFiling(ctx, target, hits); err != nil {
			return out, err
		}
		out.area = &areaOutcome{}
	}

	for _, c := range candidates {
		if !rule.Conditions.Matches(factsFromCandidate(c)) {
			continue
		}
		out.matched++
		if corrected[c.ID] {
			out.corrected++
			continue
		}

		up := update
		var removed []store.RemovedAreaLabel
		if isArea {
			step, ok := plan[c.ID]
			if !ok {
				continue // gone since the scan
			}
			out.area.count(step.Outcome)
			switch step.Outcome {
			case store.AreaFilingMove, store.AreaFilingAlreadyThere:
				removed = step.Remove
				up.RemoveLabelIDs = append([]ulid.ULID(nil), update.RemoveLabelIDs...)
				for _, rm := range removed {
					up.RemoveLabelIDs = append(up.RemoveLabelIDs, rm.LabelID)
				}
			default:
				// Contested: left where it is. The rule's OTHER actions still run;
				// only the filing is withheld.
				up.AddLabelIDs = nil
				for _, lid := range update.AddLabelIDs {
					if lid != target.LabelID {
						up.AddLabelIDs = append(up.AddLabelIDs, lid)
					}
				}
				if up.DoesNothing() {
					continue
				}
			}
		}

		// Everything this rule adds, already there? Whoever put it there — because
		// the insert underneath is additive with ON CONFLICT DO NOTHING, so a label
		// that is present at all makes this update a no-op. That same clause is what
		// keeps a hand-applied label the user's own: a backfill never rewrites the
		// attribution the inducer treats as evidence.
		fresh := len(removed) > 0 // an area label coming off is a change in itself
		for _, lid := range up.AddLabelIDs {
			if !has[c.ID][lid] {
				fresh = true
				break
			}
		}
		if !fresh && out.predictable {
			out.already++
			continue
		}

		// THE REVIEWABLE PART: every message that would come out of this with a
		// label it does not have, recorded individually.
		//
		// Collected whether or not the write happens, and BEFORE the mutation cap,
		// because the point is to be read before deciding — and a disagreement that
		// a bound deferred to the next run is precisely one someone would want to
		// see before raising the bound.
		//
		// The ones carrying a hand-applied label the rule is not adding get reported
		// separately and first. That is the difference between "would change 15" and
		// a list a person can act on: the rest are mail with no opinion attached to
		// it, and a disagreement with the user's own filing is the only kind that can
		// mean the rule is wrong.
		r.changes = append(r.changes, backfillChange{
			msg:  c,
			rule: rule,
			adds: strings.Join(out.labels, ", "),
			hand: r.handLabels(c, update.AddLabelIDs, names),
		})

		if r.mutations >= r.maxMut {
			out.deferred++
			continue
		}
		if r.apply {
			found, seq, e := tx.ApplyEmailUpdate(ctx, c.ID, up)
			if e != nil {
				return out, e
			}
			if !found {
				continue
			}
			// The database disagreed with the prediction: it saw nothing to change
			// where `has` said there was. Counted as already-done, since that is what
			// happened, and reported separately — see the note in do(). A nonzero
			// count here means the report is lying somewhere, and it should be loud
			// rather than folded quietly into a number that looks fine.
			if seq <= *state {
				out.already++
				out.mispredicted++
				continue
			}
			*state = seq
		}
		// Record it in both modes, so the next rule in this run sees the label this
		// one just added. Without it, two rules adding the same label to the same
		// message both report it as new, and report mode says twice what apply does.
		if has[c.ID] == nil {
			has[c.ID] = map[ulid.ULID]bool{}
		}
		for _, lid := range up.AddLabelIDs {
			has[c.ID][lid] = true
		}
		for _, rm := range removed {
			delete(has[c.ID], rm.LabelID)
		}
		out.changed++
		r.mutations++
	}
	return out, nil
}

// undoRule reverses this rule's backfill, from the stored attribution rather
// than from the rule's current actions — so it stays exact even if the rule was
// edited in between, and can only ever name labels the rule itself applied.
//
// RemovalIsUndo, so this records no corrections: reversing a bulk operation is
// one decision, not several hundred separate disagreements with the rule (#191).
// Without it, an undo would teach the tighten path a lesson the user never gave.
func (r *backfillRun) undoRule(
	tx *store.UserTx,
	out backfillOutcome,
	ids []ulid.ULID,
	mine map[ulid.ULID][]ulid.ULID,
	names map[ulid.ULID]string,
	state *int64,
) (backfillOutcome, error) {
	ctx := r.ctx
	// Named from what is ON THE MAIL, not from the rule's current addLabel actions.
	// For an undo those can differ — an edited rule, a renamed or re-pointed label —
	// and the report has to say which labels are actually coming off.
	seen := map[ulid.ULID]bool{}
	for _, labels := range mine {
		for _, lid := range labels {
			if seen[lid] {
				continue
			}
			seen[lid] = true
			if n, ok := names[lid]; ok {
				out.labels = append(out.labels, n)
			}
		}
	}
	sort.Strings(out.labels)

	for _, id := range ids {
		labels := mine[id]
		if len(labels) == 0 {
			continue
		}
		out.undoable++
		if !r.apply {
			continue
		}
		if r.mutations >= r.maxMut {
			out.deferred++
			continue
		}
		found, seq, err := tx.ApplyEmailUpdate(ctx, id, store.EmailUpdate{
			RemoveLabelIDs: labels,
			RemovalIsUndo:  true,
		})
		if err != nil {
			return out, err
		}
		if !found || seq <= *state {
			continue
		}
		*state = seq
		out.undone++
		r.mutations++
	}
	return out, nil
}

// handLabels names the labels the USER applied to this message that are not
// among the ones the rule is adding.
//
// Only the user's own labels. A label another rule applied is an automation's
// opinion, and surfacing it as a disagreement would have rules arguing with each
// other on the user's behalf — the same reason Rule/preview scores conflicts off
// UserLabelIDs alone.
func (r *backfillRun) handLabels(
	c store.RulePreviewCandidate,
	adding []ulid.ULID,
	names map[ulid.ULID]string,
) []string {
	add := map[ulid.ULID]bool{}
	for _, lid := range adding {
		add[lid] = true
	}
	var out []string
	for _, lid := range c.UserLabelIDs {
		if add[lid] {
			continue
		}
		if n, ok := names[lid]; ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// report prints the whole thing. A bounded scan MUST report its bound (#155),
// and a mode that wrote nothing must say so unmistakably.
func (r *backfillRun) report() {
	complete := r.scanned >= r.total
	fmt.Printf("evidence : %d of %d messages scanned%s\n", r.scanned, r.total,
		map[bool]string{true: "", false: "  (TRUNCATED — raise --limit)"}[complete])
	pct := 0
	if r.scanned > 0 {
		pct = 100 * r.labelled / r.scanned
	}
	fmt.Printf("corpus   : %d of those carry a label you applied by hand (%d%%)\n", r.labelled, pct)

	var runnable, refused, inert int
	for _, o := range r.outcomes {
		switch {
		case o.inert:
			inert++
		case o.needBodies:
			refused++
		default:
			runnable++
		}
	}
	fmt.Printf("rules    : %d selected, %d runnable", r.rulesSeen, runnable)
	if refused > 0 {
		fmt.Printf(", %d need message bodies", refused)
	}
	if inert > 0 {
		fmt.Printf(", %d have nothing a backfill will do", inert)
	}
	fmt.Println()

	verb := "apply"
	if r.undo {
		verb = "undo"
	}
	if r.apply {
		fmt.Printf("mode     : APPLIED (%s)\n\n", verb)
	} else {
		fmt.Printf("mode     : REPORT ONLY (%s) — nothing was changed\n\n", verb)
	}

	if r.undo {
		r.reportUndo()
		return
	}
	r.reportApply()
	r.reportChanges()
	r.reportFooter()
}

func (r *backfillRun) reportApply() {
	rows := make([]backfillOutcome, 0, len(r.outcomes))
	for _, o := range r.outcomes {
		if o.inert || o.needBodies {
			continue
		}
		rows = append(rows, o)
	}
	// Biggest movers first: the operator is deciding whether to let this run, and
	// the rules that would change the most mail are the ones that decision is
	// about.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].changed != rows[j].changed {
			return rows[i].changed > rows[j].changed
		}
		return rows[i].matched > rows[j].matched
	})

	head := "changed"
	if !r.apply {
		head = "new"
	}
	fmt.Printf("%-22s %8s %6s %8s %10s %8s  %s\n",
		"label", "matched", head, "already", "corrected", "deferred", "conditions")
	fmt.Println(strings.Repeat("-", 118))
	var tm, tc, ta, tk, td int
	for _, o := range rows {
		tm += o.matched
		tc += o.changed
		ta += o.already
		tk += o.corrected
		td += o.deferred
		if o.matched == 0 {
			continue
		}
		fmt.Printf("%-22s %8d %6d %8d %10d %8d  %s\n",
			trunc(strings.Join(o.labels, ","), 22),
			o.matched, o.changed, o.already, o.corrected, o.deferred,
			trunc(describe(o.rule.Conditions), 44))
	}
	fmt.Println(strings.Repeat("-", 118))
	fmt.Printf("%-22s %8d %6d %8d %10d %8d\n", "totals", tm, tc, ta, tk, td)

	// THE REPORT CHECKING ITSELF. Every message it expected to change and the
	// database then declined to change means the prediction and the write disagree,
	// which makes the whole report untrustworthy rather than slightly off. It gets
	// its own line, in the imperative, because the number that matters here is zero
	// and a reader must not have to go looking for it.
	mis := 0
	for _, o := range rows {
		mis += o.mispredicted
	}
	if mis > 0 {
		fmt.Printf("\nMISPREDICTED: %d messages this run expected to change were already\n"+
			"unchanged when written. The report above does not match what happened —\n"+
			"treat its counts as unreliable and say so in the issue.\n", mis)
	}

	// A rule that matched nothing at all is worth counting but not worth a row —
	// on an induced rule set most of them are simply about mail that has not
	// arrived since, and eighty blank lines would bury the rows that matter.
	quiet := 0
	for _, o := range rows {
		if o.matched == 0 {
			quiet++
		}
	}
	if quiet > 0 {
		fmt.Printf("\n%d runnable rules matched nothing in the scanned window.\n", quiet)
	}
	if r.upperBound {
		fmt.Printf("\nNOTE: some rules also flag or mark-priority. Those bits are not on the\n" +
			"      message row, so report mode cannot tell an already-flagged message\n" +
			"      from a fresh one and counts it as new — the column is an upper bound.\n")
	}
	// Area-filing rules are a swap, so say what became of the mail the rule
	// matched but did not move: a message filed somewhere else by you, or by
	// another rule, stays there (#728).
	for _, o := range rows {
		if a := o.area; a != nil && o.matched > 0 {
			verb := "would move"
			if r.apply {
				verb = "moved"
			}
			fmt.Printf("\n%-22s area filing: %s %d, already there %d, left where you filed it %d,\n"+
				"%-22s left where another rule filed it %d, left (other routing) %d\n",
				trunc(strings.Join(o.labels, ","), 22), verb, a.move, a.alreadyThere, a.contestedByUser,
				"", a.contestedByRule, a.contestedOther)
		}
	}
	for _, o := range r.outcomes {
		if len(o.skippedActions) > 0 {
			fmt.Printf("\n%-22s will NOT perform: %s  (they describe where mail lands on\n"+
				"%-22s arrival, not what it is)\n",
				trunc(strings.Join(o.labels, ","), 22), strings.Join(o.skippedActions, ", "), "")
		}
		if o.needBodies {
			fmt.Printf("\n%-22s REFUSED: tests recipients or headers, which are not on the\n"+
				"%-22s message row. Apply it from the web client instead.\n",
				trunc(strings.Join(o.labels, ","), 22), "")
		}
	}
}

// reportChanges is the part of the report worth reading. Everything above is
// counting; this is the review.
//
// Split in two, because the two halves carry different weight. A message the
// user filed somewhere else BY HAND is a claim that one of them is wrong, and
// wants reading one at a time. A message with no label on it at all is just mail
// the automation is about to file, and wants only to be visible — the count on
// its own would leave "would change 15" with no way to find out which 15.
func (r *backfillRun) reportChanges() {
	var conflicts, plain []backfillChange
	for _, c := range r.changes {
		if len(c.hand) > 0 {
			conflicts = append(conflicts, c)
		} else {
			plain = append(plain, c)
		}
	}

	if len(conflicts) == 0 {
		fmt.Printf("\nNo rule disagrees with your own filing anywhere in the scanned window.\n")
	} else {
		msgs := map[ulid.ULID]bool{}
		for _, c := range conflicts {
			msgs[c.msg.ID] = true
		}
		fmt.Printf("\nDISAGREEMENTS WITH YOUR OWN FILING — %d, across %d messages.\n",
			len(conflicts), len(msgs))
		fmt.Printf("Each is either a second label you would want, or a rule that is too broad.\n")
		fmt.Printf("These are the ones to read before letting this run.\n\n")
		r.printChanges(conflicts, "--show-conflicts")
	}

	if len(plain) > 0 {
		fmt.Printf("\nNEWLY LABELLED, with nothing of yours to contradict — %d.\n\n", len(plain))
		r.printChanges(plain, "--show-conflicts")
	}
}

func (r *backfillRun) printChanges(cs []backfillChange, flagName string) {
	// Newest first: recent mail is what the operator has any memory of, and so the
	// only part of the list they can actually judge.
	sort.SliceStable(cs, func(i, j int) bool {
		return cs[i].msg.ReceivedAt.After(cs[j].msg.ReceivedAt)
	})
	show := r.showConf
	if show <= 0 || show > len(cs) {
		show = len(cs)
	}
	for _, c := range cs[:show] {
		fmt.Printf("  %s  %-34s %s\n",
			c.msg.ReceivedAt.Format("2006-01-02"), trunc(c.msg.From, 34), trunc(c.msg.Subject, 54))
		// Both ids, both labelled, in full — not truncated. This row is what an
		// operator copies to go look at the message, and the id that resolves to a
		// message must be the one printed as "msg", not the rule that touched it
		// (#558: a reader had only the rule id and it resolved to nothing).
		if len(c.hand) > 0 {
			fmt.Printf("              you filed: %-22s rule adds: %-18s msg %s · rule %s\n",
				trunc(strings.Join(c.hand, ", "), 22), trunc(c.adds, 18), c.msg.ID, c.rule.ID)
			continue
		}
		fmt.Printf("              rule adds: %-18s msg %s · rule %s\n", trunc(c.adds, 18), c.msg.ID, c.rule.ID)
	}
	if show < len(cs) {
		fmt.Printf("\n  (showing %d of %d — raise %s, or 0 for all)\n", show, len(cs), flagName)
	}
}

func (r *backfillRun) reportFooter() {
	if !r.apply {
		fmt.Printf("\nnothing was changed. re-run with --apply to perform it.\n")
	} else {
		fmt.Printf("\n%d messages changed.\n", r.mutations)
	}
	if r.mutations >= r.maxMut {
		fmt.Printf("STOPPED AT THE --max-mutations BOUND of %d. This is a PARTIAL backfill;\n"+
			"re-run to continue, or raise the bound.\n", r.maxMut)
	}
	// NOT a hint to reverse THIS run. --undo reads stored attribution, not a run
	// log — it removes every label these rules have EVER applied, across the whole
	// mailbox, not only the messages just changed above (#558: on the test
	// instance, --undo after a 4-message run changed 1000, the --max-mutations
	// cap). Said in words rather than counted here: the count --undo itself prints
	// (in reportUndo) comes from a query scoped to a preview run's own --limit and
	// --rule selection, so reusing it from here would either mean a second,
	// unrelated database pass in this one, or a number that undercounts the true
	// scope — worse than no number.
	if r.apply {
		fmt.Printf("to reverse it: same command with --undo --apply. That does NOT reverse only\n" +
			"this run — it removes every label these rules have EVER applied, across the\n" +
			"whole mailbox. Run with --undo (no --apply) first to see how many that is.\n" +
			"It takes the area label off but does NOT put back the earlier area's label\n" +
			"an area-filing rule swapped out; re-file those from the desk.\n")
	}
}

func (r *backfillRun) reportUndo() {
	var total, done int
	fmt.Printf("%-22s %10s %8s  %s\n", "label", "undoable", "undone", "conditions")
	fmt.Println(strings.Repeat("-", 100))
	for _, o := range r.outcomes {
		total += o.undoable
		done += o.undone
		if o.undoable == 0 {
			continue
		}
		fmt.Printf("%-22s %10d %8d  %s\n",
			trunc(strings.Join(o.labels, ","), 22), o.undoable, o.undone,
			trunc(describe(o.rule.Conditions), 44))
	}
	fmt.Println(strings.Repeat("-", 100))
	fmt.Printf("%-22s %10d %8d\n", "totals", total, done)
	if !r.apply {
		fmt.Printf("\nnothing was changed. re-run with --undo --apply to remove these labels.\n")
		return
	}
	fmt.Printf("\n%d messages had rule-applied labels removed. No corrections were recorded:\n"+
		"reversing a bulk operation is one decision, not %d disagreements with the rules.\n",
		done, done)
}
