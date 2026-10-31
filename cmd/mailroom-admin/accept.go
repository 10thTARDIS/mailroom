// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Operator-side acceptance of induced rules, and label removal (#156).
//
// Both exist because the ONLY write path for rules and labels is JMAP, and JMAP
// is reachable only with an interactive OIDC session — so an operator with shell
// access and a hundred proposals in front of them has no way to act on them in
// bulk. The alternative anyone reaches for is `psql`, and that is precisely the
// thing that must not happen: app.rules and app.labels are JMAP-visible objects,
// so every mutation owes a change_log row in the same transaction (CLAUDE.md's
// load-bearing invariant). A hand-written INSERT produces a rule the server has
// and no client ever hears about.
//
// These go through store.CreateRule / store.DeleteLabel, which append that row —
// the same functions Rule/set and Label/set call. Nothing here is a second write
// path; it is the same one with a different front door.
//
// DRY RUN BY DEFAULT, like backfill-alias, and for the same reason: the report
// IS the deliverable. "It ran and changed nothing" and "it ran and changed
// everything" must not look alike from the outside.

// runAcceptRules creates rules from induction's proposals, bounded by how much
// mail each one would newly touch.
//
// COLLATERAL IS THE ACCEPTANCE CRITERION, and the default of 0 is deliberately
// the strictest one available: accept only rules that, over the scanned window,
// match nothing the user did not already file under that label themselves. That
// is as close to "this rule only formalises what you already did" as the data
// can express.
//
// It is worth being precise about what collateral means now, because it changed.
// While a mailbox is mostly unlabelled, collateral is a BLAST RADIUS — mail the
// rule would newly label, most of which is probably correct and simply never got
// filed — and scoring it as error is meaningless (#175). On a mailbox that has
// been worked through, unlabelled mail is genuinely unclassified, and collateral
// approaches a real error count. So the threshold means different things at
// different corpus densities, and the report prints the density alongside it
// rather than letting a bare number imply a confidence it has not earned.
func runAcceptRules(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("accept-rules", flag.ContinueOnError)
	var (
		userIDStr     = fs.String("user-id", "", "operator user id (ULID)")
		limit         = fs.Int("limit", 5000, "how many messages to scan as evidence")
		maxCollateral = fs.Int("max-collateral", 0, "accept a proposal only if it would newly touch at most this many messages")
		minCovers     = fs.Int("min-covers", 2, "accept a proposal only if it explains at least this many of your own labellings")
		apply         = fs.Bool("apply", false, "actually create the rules (default: report only)")
		priority      = fs.Int("priority", 100, "priority for created rules")
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

	var (
		candidates []store.RulePreviewCandidate
		total      int
		labels     []store.LabelView
		labelled   int
		saved      []savedRule
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, *limit); e != nil {
			return e
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return e
		}
		if labels, e = tx.ListLabels(ctx); e != nil {
			return e
		}
		saved, e = savedRules(ctx, tx)
		return e
	}); err != nil {
		return err
	}
	for _, c := range candidates {
		if len(c.UserLabelIDs) > 0 {
			labelled++
		}
	}

	// The bound, and the density that decides how much the collateral figure is
	// worth. A bounded scan MUST report its bound (#155).
	complete := len(candidates) >= total
	fmt.Printf("evidence : %d of %d messages scanned%s\n",
		len(candidates), total,
		map[bool]string{true: "", false: "  (TRUNCATED — raise --limit)"}[complete])
	pct := 0
	if len(candidates) > 0 {
		pct = 100 * labelled / len(candidates)
	}
	fmt.Printf("corpus   : %d of those carry a label (%d%%)\n", labelled, pct)
	if pct < 50 {
		fmt.Printf("           NOTE: on a mostly-unlabelled mailbox, collateral is a blast\n")
		fmt.Printf("           radius rather than an error count — most of what a rule would\n")
		fmt.Printf("           newly touch is probably correct and simply never got filed.\n")
	}
	fmt.Printf("accepting: collateral <= %d, covers >= %d\n\n", *maxCollateral, *minCovers)

	reportDuplicateRules(saved)

	names := map[string]string{}
	for _, l := range labels {
		names[l.ID.String()] = l.Name
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

	var accepted []proposal

	for _, l := range labels {
		target := l.ID.String()
		examples := make([]rules.Example, 0, len(candidates))
		positives := 0
		for _, c := range candidates {
			ex := rules.Example{Facts: factsFromCandidate(c)}
			for _, lid := range c.UserLabelIDs {
				if lid.String() == target {
					ex.Positive = true
					continue
				}
				if n, ok := names[lid.String()]; ok {
					ex.OtherUserLabels = append(ex.OtherUserLabels, n)
				}
			}
			if ex.Positive {
				positives++
			}
			examples = append(examples, ex)
		}
		if positives == 0 {
			continue
		}
		for _, c := range rules.Induce(examples) {
			// Conflicts are never acceptable at any threshold: a conflict is mail
			// the user filed under a DIFFERENT label by hand, so the rule would
			// contradict them rather than merely surprise them. Induce does not
			// currently propose one, and this does not rely on that staying true.
			if c.Conflicts > 0 || c.Collateral > *maxCollateral || c.Covers < *minCovers {
				continue
			}
			accepted = append(accepted, proposal{l, c.Conditions, c.Covers, c.Collateral})
		}
	}

	// DROP ANY PROPOSAL STRICTLY NARROWER THAN ONE WE ARE ALREADY TAKING.
	//
	// Induce walks from the most specific hypothesis outward, so for one cluster
	// it can offer both `alias = openai@` (covers 3) and `alias = openai@ AND
	// listUnsubscribe` (covers 2) — and at collateral 0 both clear the bar.
	// Creating both adds a rule that can never fire on anything the broader one
	// misses: strictly more conditions, strictly fewer matches, same label.
	//
	// Keeping it would not mislabel anything; it would just make the rules list
	// longer than the number of decisions in it, which is its own kind of harm —
	// a list nobody can read is a list nobody audits.
	//
	// Broadest first, then skip anything whose conjuncts are a superset of one
	// already kept. Conjunct-set containment rather than tree equality, because
	// that is exactly the "same rule plus another restriction" relationship.
	sort.SliceStable(accepted, func(i, j int) bool { return accepted[i].covers > accepted[j].covers })
	kept := accepted[:0]
	for _, p := range accepted {
		redundant := false
		for _, k := range kept {
			if k.label.ID == p.label.ID && isSuperset(conjuncts(p.conditions), conjuncts(k.conditions)) {
				redundant = true
				break
			}
		}
		if !redundant {
			kept = append(kept, p)
		}
	}
	accepted = kept

	// NOW DROP ANYTHING A RULE THE USER ALREADY HAS WOULD ALREADY CATCH — the same
	// containment test, extended over the rules that exist (mailroom#195).
	//
	// This used to be guarded by the rule NAME: CreateRule returns
	// ErrRuleNameTaken and ruleName is deterministic, so a second run of this tool
	// skips everything the first run made. That works, and it stops working the
	// moment a rule arrives from anywhere else. A real instance ended up with
	// duplicate pairs, each one rule accepted in the rules editor and one created
	// here — the client names a rule `Shop (suggested)`, this names it
	// `Shop: alias equals "shop@…" AND listUnsubscribe exists`, the names do
	// not collide, and the guard waved it through.
	//
	// A NAME IS ONLY A PROXY FOR IDENTITY WHILE ONE GENERATOR OWNS THE NAMING.
	// Two front doors, two conventions, and the proxy fails silently. So the test
	// is on what a rule DOES — its label and its conjuncts — which is the same
	// thing that made the in-run check above correct.
	// EVERY covering rule is collected, not just the first, because more than one
	// IS THE DUPLICATE DETECTOR. If two saved rules both cover a proposal for the
	// same label, the user has two rules doing one job — which is the condition
	// this whole change is about, and the tool may as well name it while it is
	// standing right on top of it.
	superseded := make([]supersededProposal, 0, len(accepted))
	fresh := accepted[:0]
	for _, p := range accepted {
		pc := conjuncts(p.conditions)
		var by []savedRule
		for i := range saved {
			if saved[i].labels[p.label.ID] && isSuperset(pc, saved[i].conjuncts) {
				by = append(by, saved[i])
			}
		}
		if len(by) == 0 {
			fresh = append(fresh, p)
			continue
		}
		superseded = append(superseded, supersededProposal{p, by})
	}
	accepted = fresh

	sort.SliceStable(accepted, func(i, j int) bool {
		if accepted[i].label.Name != accepted[j].label.Name {
			return accepted[i].label.Name < accepted[j].label.Name
		}
		return accepted[i].covers > accepted[j].covers
	})

	if len(superseded) > 0 {
		fmt.Printf("already covered by rules you have — %d proposal(s) not offered:\n", len(superseded))
		for _, s := range superseded {
			// Plain %s, not %q. describe() already quotes the values inside a
			// condition, and wrapping the whole name in %q escapes those into \" —
			// unreadable across thirty lines of report.
			fmt.Printf("  %-14s %s\n                 already handled by: %s\n",
				s.proposal.label.Name, describe(s.proposal.conditions), s.by[0].name)
			// A rule the user TURNED OFF is called out, because the consequence
			// differs: creating an enabled copy of it would resurrect automation
			// they deliberately switched off, which is worse than a duplicate and
			// would look like the tool ignoring them.
			for _, r := range s.by {
				if !r.enabled {
					fmt.Printf("                 NOTE: %s is TURNED OFF — not re-created as an enabled copy\n", r.name)
				}
			}
		}
		fmt.Println()
	}

	if len(accepted) == 0 {
		// "Nothing cleared the bar" and "everything that cleared the bar you
		// already have" are different facts, and collapsing them would make a
		// healthy second run read like a failed first one.
		if len(superseded) > 0 {
			fmt.Println("every proposal that met the bar is already covered by a rule you have; nothing to do")
		} else {
			fmt.Println("no proposal meets the bar; nothing to do")
		}
		return nil
	}

	for _, p := range accepted {
		fmt.Printf("  %-14s covers %-4d collateral %-3d %s\n",
			p.label.Name, p.covers, p.collateral, describe(p.conditions))
	}
	fmt.Println()

	if !*apply {
		fmt.Printf("DRY RUN — would create %d rules. Re-run with --apply.\n", len(accepted))
		return nil
	}

	created, skipped := 0, 0
	for _, p := range accepted {
		name := ruleName(p.label.Name, p.conditions)
		err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			_, e := tx.CreateRule(ctx, store.RuleRow{
				Name:       name,
				Priority:   *priority,
				Conditions: p.conditions,
				Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: p.label.ID.String()}},
				Enabled:    true,
				// Recorded as system-suggested because that is what it is: this
				// rule was derived from the user's labelling, not written by them.
				// A rules list that cannot tell the two apart is one where nobody
				// can answer "did I ask for this?".
				SystemSuggested: true,
			})
			return e
		})
		switch {
		case err == nil:
			created++
			fmt.Printf("  created  %s\n", name)
		case errors.Is(err, store.ErrRuleNameTaken):
			// Almost always a re-run. Reported rather than swallowed: "already
			// there" and "just made it" are different facts.
			skipped++
			fmt.Printf("  exists   %s\n", name)
		default:
			return fmt.Errorf("creating %q: %w", name, err)
		}
	}
	fmt.Printf("\ncreated %d, already present %d\n", created, skipped)
	return nil
}

// proposal is one induced rule that cleared the acceptance bar: the label it
// would apply, the conditions it would apply it under, and the two numbers the
// bar is set against.
type proposal struct {
	label      store.LabelView
	conditions rules.Condition
	covers     int
	collateral int
}

// savedRule is a rule the user already has, reduced to the two things that
// decide whether a new proposal would be redundant against it: WHICH LABEL it
// applies and WHICH TESTS it ANDs together.
//
// Deliberately not the rule itself. Comparing whole rules would drag in the
// name, the priority and the other actions, none of which bear on "can this
// proposal ever fire on something that rule misses" — and the name in particular
// is what made the old check wrong.
type savedRule struct {
	name      string
	enabled   bool
	labels    map[ulid.ULID]bool
	conjuncts map[string]bool
}

// supersededProposal pairs a dropped proposal with the rules that already cover
// it, so the report can say WHICH rule rather than only that something did.
//
// A slice rather than one rule: more than one cover means the user has duplicate
// rules, which is worth telling them about, and is exactly the condition
// mailroom#195 exists to stop this tool from creating.
type supersededProposal struct {
	proposal proposal
	by       []savedRule
}

// reportDuplicateRules names rules the user already has that do the same job.
//
// A STANDING PROPERTY OF THE RULES LIST, so it is checked against the list
// itself rather than as a byproduct of this run's proposals. The first attempt
// did the latter — reporting a duplicate whenever two saved rules both covered
// one proposal — and it found one of the operator's three pairs, because the
// other two sit under mail that no longer produces a proposal at the current
// bar. A detector whose coverage depends on what induction happens to offer
// today reports "no duplicates" for a list full of them.
//
// The relation is the same one used everywhere else here: same label, and one
// rule's conjuncts contained in the other's. Equal sets are true duplicates;
// strict containment means the narrower one can never fire on anything the
// broader one misses, so the broader one is the keeper. Both are worth naming,
// and neither is deleted — which rule a user wants to keep is theirs to say, and
// this tool's whole posture is that it does not remove things.
func reportDuplicateRules(saved []savedRule) {
	// THE TWO KINDS ARE NOT INTERCHANGEABLE, and the advice differs. Identical
	// rules are symmetric: either may go. Strict containment is not — the narrower
	// one is the redundant one, and deleting the BROADER one would stop matching
	// mail that is currently getting labelled. Telling someone to "delete either"
	// in that case is advice that loses coverage.
	type pair struct {
		broader, narrower savedRule
		identical         bool
	}
	var pairs []pair
	for i := range saved {
		for j := i + 1; j < len(saved); j++ {
			a, b := saved[i], saved[j]
			if !sharesLabel(a, b) {
				continue
			}
			aInB, bInA := isSuperset(b.conjuncts, a.conjuncts), isSuperset(a.conjuncts, b.conjuncts)
			switch {
			case aInB && bInA:
				pairs = append(pairs, pair{a, b, true})
			case bInA:
				pairs = append(pairs, pair{b, a, false})
			case aInB:
				pairs = append(pairs, pair{a, b, false})
			}
		}
	}
	if len(pairs) == 0 {
		return
	}
	fmt.Printf("DUPLICATE RULES YOU ALREADY HAVE — %d pair(s). Harmless to your mail (the label\n"+
		"lands once either way), but the redundant rule can never fire on anything the other\n"+
		"misses, and it makes the rules list longer than the number of decisions in it.\n"+
		"Delete the redundant one in the rules editor:\n", len(pairs))
	for _, p := range pairs {
		if p.identical {
			fmt.Printf("  identical, delete either:\n    %s\n    %s\n", p.broader.name, p.narrower.name)
			continue
		}
		fmt.Printf("  keep      : %s\n  redundant : %s  (narrower — matches a subset)\n",
			p.broader.name, p.narrower.name)
	}
	fmt.Println()
}

// sharesLabel reports whether two rules apply any label in common. Rules that
// label different things are not duplicates however alike their conditions look.
func sharesLabel(a, b savedRule) bool {
	for lid := range a.labels {
		if b.labels[lid] {
			return true
		}
	}
	return false
}

// savedRules loads every rule the user has — INCLUDING DISABLED ONES — reduced to
// the comparable form.
//
// Disabled rules count. A proposal duplicating a rule the user switched off must
// not be created as an enabled copy: that is the automation overruling an
// explicit decision, and it would arrive looking like a brand-new suggestion.
//
// Labels are resolved through the store rather than string-compared, because a
// rule's addLabel target may be a label id, a keyword atom or a name, and two
// rules can name the same label three different ways. A target that no longer
// resolves is dropped — such a rule labels nothing, so it cannot cover anything.
func savedRules(ctx context.Context, tx *store.UserTx) ([]savedRule, error) {
	existing, err := tx.ListRules(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]savedRule, 0, len(existing))
	for _, r := range existing {
		sr := savedRule{
			name:      r.Name,
			enabled:   r.Enabled,
			labels:    map[ulid.ULID]bool{},
			conjuncts: conjuncts(r.Conditions),
		}
		for _, act := range r.Actions {
			if act.Type != rules.ActionAddLabel {
				continue
			}
			lid, ok, err := tx.ResolveRuleLabel(ctx, act.Label)
			if err != nil {
				return nil, err
			}
			if ok {
				sr.labels[lid] = true
			}
		}
		// A rule that applies no label cannot make a labelling proposal redundant,
		// whatever else it does.
		if len(sr.labels) > 0 {
			out = append(out, sr)
		}
	}
	return out, nil
}

// conjuncts flattens a condition into the set of leaf tests it ANDs together.
// A leaf is a one-element set. An `any` (OR) tree is NOT decomposable this way,
// so it is treated as one opaque test — containment between two ORs is not the
// "same rule plus a restriction" relationship this is looking for, and pretending
// otherwise would drop a rule that genuinely matches different mail.
func conjuncts(c rules.Condition) map[string]bool {
	out := map[string]bool{}
	if len(c.All) > 0 {
		for _, sub := range c.All {
			for k := range conjuncts(sub) {
				out[k] = true
			}
		}
		return out
	}
	out[describe(c)] = true
	return out
}

// isSuperset reports whether a contains every element of b — i.e. a is b plus
// zero or more extra restrictions, and therefore matches no more than b does.
func isSuperset(a, b map[string]bool) bool {
	if len(a) < len(b) {
		return false
	}
	for k := range b {
		if !a[k] {
			return false
		}
	}
	return true
}

// ruleName builds a stable, unique-ish name from the label and what the rule
// keys on. It has to be BOTH readable in the rules list and distinct per
// proposal — a label with three rules cannot have them all called "Shop
// (suggested)", because the name is UNIQUE per user and the second create would
// simply fail.
func ruleName(label string, c rules.Condition) string {
	desc := strings.TrimSuffix(strings.TrimPrefix(describe(c), "("), ")")
	name := label + ": " + desc
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// runDeleteLabel removes a label by name.
//
// Its dry run reports HOW MANY MESSAGES CARRY IT, because that is the number
// that decides whether this is housekeeping or data loss. store.DeleteLabel
// detaches the label from every carrying message FIRST, through
// ApplyEmailUpdate — one Email change row per detached message (RemovalIsUndo,
// so this does not manufacture a rule correction) plus a writeback intent that
// strips the mirrored keyword from the IMAP server — and only then deletes the
// label row itself, appending a Label/destroyed change row. There is still no
// undo: afterwards the change_log shows N Email removals and one Label
// destroy, not a record of which messages the label used to be on.
func runDeleteLabel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delete-label", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		name      = fs.String("name", "", "label name to delete (exact, case-sensitive)")
		apply     = fs.Bool("apply", false, "actually delete it (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--name is required")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		labels, err := tx.ListLabels(ctx)
		if err != nil {
			return err
		}
		var found *store.LabelView
		for i := range labels {
			if labels[i].Name == *name {
				found = &labels[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("no label named %q (names are case-sensitive)", *name)
		}

		attached, err := tx.CountLabelUsage(ctx, found.ID)
		if err != nil {
			return err
		}

		fmt.Printf("label    : %s (%s)\n", found.Name, found.ID)
		fmt.Printf("messages : %d carry it\n", attached)
		if !*apply {
			fmt.Printf("\nDRY RUN — would delete it%s. Re-run with --apply.\n",
				map[bool]string{true: "", false: " and detach it from those messages"}[attached == 0])
			return nil
		}
		ok, err := tx.DeleteLabel(ctx, found.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("label vanished between read and delete")
		}
		fmt.Printf("\ndeleted.\n")
		return nil
	})
}
