// SPDX-License-Identifier: AGPL-3.0-only

package rules

import (
	"sort"
	"strings"
)

// Rule induction: turn "you labelled these two messages Promotions" into a rule
// that keeps doing it — without the failure that makes such systems
// untrustworthy (spec §8, mailroom#156).
//
// THE PROBLEM, in one case. Label two Wayfair advertisements as Promotions. The
// obvious induction is `fromDomain = wayfair.example`, and that rule eats the
// order receipts. Any implementation that does not avoid this is not worth
// shipping, so it is the acceptance test.
//
// WHY THE OBVIOUS FIX IS NOT ENOUGH. "Reject candidates that collide with mail
// the user labelled differently" only works if the receipt is already labelled.
// On a fresh mailbox it is not labelled at all — so a domain rule collides with
// nothing, matches strictly more mail, and wins on coverage. The receipt gets
// swept up and the system looks confident while being wrong, which is precisely
// the outcome that destroys trust.
//
// WHAT ACTUALLY WORKS is to notice that the examples SHARE a property the
// receipts do not: marketing mail carries List-Unsubscribe (and usually
// List-Id), receipts do not, because senders keep those streams separate so an
// unsubscribe cannot kill an order confirmation. So induction here is:
//
//  1. Build the MAXIMALLY SPECIFIC conjunction of everything the positives share
//     (classic find-S). For the Wayfair ads that is: sender, domain, has-list,
//     has-unsubscribe, list id.
//  2. Walk from specific toward general, dropping one conjunct at a time.
//  3. STOP AT THE KNEE — where dropping a conjunct causes collateral to jump.
//     Dropping "has List-Unsubscribe" from a Wayfair rule admits every receipt,
//     every shipping notice and every password reset from that domain, so the
//     jump is large and the conjunct is discriminative. Dropping "sender is
//     deals@" admits other marketing senders at the same domain, which is a
//     small jump and usually what the user meant.
//
// Nothing is applied automatically: Induce RANKS candidates and reports what
// each would have matched, and a human accepts one (#156 requires the blast
// radius before acceptance). The default is the safest reading of the evidence,
// not a guess presented as a conclusion.

// Example is one message the inducer learns from: the facts a condition can
// test, plus what the USER has said about it.
type Example struct {
	Facts MessageFacts
	// Positive means the user applied the target label to this message.
	Positive bool
	// OtherUserLabels are labels the USER applied that are NOT the target. A
	// candidate matching one of these is a hard conflict: the user has already
	// said this message belongs somewhere else.
	OtherUserLabels []string
}

// Candidate is one induced rule, scored against the user's real mail.
type Candidate struct {
	Conditions Condition
	// Covers is how many of the labelled examples it matches; Positives is how
	// many there were. Covers < Positives means it does not explain the evidence.
	Covers    int
	Positives int
	// Collateral is how many NON-positive messages it also matches — the blast
	// radius beyond what was labelled.
	Collateral int
	// Conflicts are matched messages carrying a different user-applied label.
	// These are shown before acceptance ("3 of these you labelled Shopping").
	Conflicts int
	// ConflictLabels are the distinct other labels involved, for that sentence.
	ConflictLabels []string
	// Discriminative marks a candidate that keeps a conjunct whose removal would
	// sharply widen the match — the reason to prefer it over a simpler one.
	Discriminative bool
}

// kneeFactor is how much collateral growth marks a conjunct as discriminative.
// Dropping it must roughly TRIPLE what the rule catches before the more general
// form is treated as a different rule rather than a tidier one.
//
// Tuned to the failure it exists to prevent rather than to a dataset: a
// retailer's transactional mail (receipts, shipping, password resets, account
// notices) typically outnumbers any one marketing stream by a wide margin, so
// the promo-vs-everything jump clears 3x comfortably, while "this sender" ->
// "any sender at this domain" usually does not.
const kneeFactor = 3

// minCollateralJump keeps the knee test meaningful on a small mailbox: below
// this, a jump from 1 to 4 is noise, not evidence.
const minCollateralJump = 5

// Induce proposes rules that explain the labelled examples, best first.
//
// It returns only candidates that cover EVERY positive — a rule that does not
// explain the evidence is not an induction of it — and never returns one that
// collides with a message the user labelled differently. An empty result means
// the examples share nothing usable, which is an honest answer: two messages
// with nothing in common do not imply a rule.
func Induce(examples []Example) []Candidate {
	// ONE RULE PER CLUSTER, not one per label (#173).
	//
	// The first version asked what EVERY labelled message had in common. That is
	// right for a sender-shaped label ("these two Wayfair ads") and useless for a
	// CATEGORY, which is how people actually use a label called Promotions: on
	// real mail, a whole category's messages from many different senders shared
	// nothing at all — not the domain, not even List-Unsubscribe — so
	// induction proposed nothing for their largest label.
	//
	// Asking "what do all 22 have in common" is a question whose honest answer is
	// "nothing". The right question is asked per group of mail that plausibly
	// forms one rule: the four newsletters from one yarn shop imply a rule; the
	// fourteen one-off senders imply nothing, and are the long tail the per-label
	// classifier exists for (#169).
	var clusters map[string][]MessageFacts
	var order []string
	for _, e := range examples {
		if !e.Positive {
			continue
		}
		for _, key := range clusterKeys(e.Facts) {
			if clusters == nil {
				clusters = map[string][]MessageFacts{}
			}
			if _, seen := clusters[key]; !seen {
				order = append(order, key)
			}
			clusters[key] = append(clusters[key], e.Facts)
		}
	}

	var out []Candidate
	seenRule := map[string]bool{}
	for _, key := range order {
		group := clusters[key]
		// A single message is not evidence of a rule, whatever else is labelled
		// alongside it.
		if len(group) < MinClusterExamples {
			continue
		}
		c, ok := induceOne(group, examples)
		if !ok {
			continue
		}
		// A message now belongs to more than one cluster, so two clusters can
		// arrive at the SAME rule (a retailer whose whole domain reaches one
		// alias). Offer it once.
		if k := conditionKey(c.Conditions); seenRule[k] {
			continue
		} else {
			seenRule[k] = true
		}
		out = append(out, c)
	}
	// Biggest evidence first: the group the user labelled most is the one they
	// most clearly meant. On a TIE, the tighter rule wins — equal evidence with a
	// smaller blast radius is strictly the better proposal, and without this the
	// order would come down to which cluster key happened to be visited first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Positives != out[j].Positives {
			return out[i].Positives > out[j].Positives
		}
		return out[i].Collateral < out[j].Collateral
	})
	return out
}

// conditionKey renders a condition tree canonically, for dedup.
func conditionKey(c Condition) string {
	var b strings.Builder
	var walk func(Condition)
	walk = func(c Condition) {
		switch {
		case len(c.All) > 0:
			b.WriteString("all(")
			for _, s := range c.All {
				walk(s)
				b.WriteByte(',')
			}
			b.WriteByte(')')
		case len(c.Any) > 0:
			b.WriteString("any(")
			for _, s := range c.Any {
				walk(s)
				b.WriteByte(',')
			}
			b.WriteByte(')')
		default:
			b.WriteString(c.Field + "|" + string(c.Op) + "|" + strings.ToLower(c.Value) + ";")
		}
	}
	walk(c)
	return b.String()
}

// MinClusterExamples is how many messages from one sender/domain/list it takes
// before a rule is worth proposing for them.
const MinClusterExamples = 2

// clusterKeys groups labelled messages that plausibly belong to one rule. A
// message can belong to SEVERAL groups, because there is more than one way for
// mail to "plausibly be one rule" and they do not nest.
//
// The sender grouping leads: a retailer sends marketing from deals@ and news@ at
// the same domain, and those are one rule, not two. The list is next, for mail
// whose sending domain varies but whose List-Id does not. The bare sender is the
// fallback for mail with no usable domain.
//
// THE ALIAS IS A SEPARATE GROUPING, NOT A FALLBACK (mailroom#177). Under a
// catchall the whole point is that mail reaching one alias comes from WHATEVER
// senders that service uses: on a real mailbox, a big shop arrives from
// its own domain, from marketplace sellers and from shipping partners. Grouped by
// domain that is three clusters of one, all below MinClusterExamples, so nothing
// is proposed and the alias conjunct in sharedConjuncts never even gets asked —
// teaching the conjunct without teaching the clustering would have fixed only
// the cases where a domain rule already worked.
//
// Emitting both keys means the alias competes as a hypothesis rather than
// replacing the sender one; whichever explains the evidence with less collateral
// is what ranking prefers, and identical rules from two clusters are deduped.
func clusterKeys(f MessageFacts) []string {
	var out []string
	switch {
	case strings.TrimSpace(f.FromDomain) != "":
		out = append(out, "dom:"+strings.ToLower(strings.TrimSpace(f.FromDomain)))
	case strings.TrimSpace(f.ListID) != "":
		out = append(out, "list:"+strings.ToLower(strings.TrimSpace(f.ListID)))
	case strings.TrimSpace(f.From) != "":
		out = append(out, "from:"+strings.ToLower(strings.TrimSpace(f.From)))
	}
	if a := strings.ToLower(strings.TrimSpace(f.Alias)); a != "" {
		out = append(out, "alias:"+a)
	}
	return out
}

// induceOne runs the narrowest-consistent-rule machinery for ONE cluster.
//
// The hypothesis is built from the cluster, but SCORED against every example:
// collateral and conflicts are about the whole mailbox, not the group. A rule
// that looks tight within its cluster and sweeps half the inbox is exactly what
// the blast radius exists to expose.
func induceOne(group []MessageFacts, examples []Example) (Candidate, bool) {
	ladder := generalizationLadder(sharedConjuncts(group))
	if len(ladder) == 0 {
		return Candidate{}, false
	}

	scored := make([]Candidate, 0, len(ladder))
	for _, conds := range ladder {
		c := score(conds, examples, len(group))
		// Must explain THIS cluster's evidence, and must not contradict the user.
		covered := 0
		for i := range group {
			if conds.Matches(group[i]) {
				covered++
			}
		}
		c.Covers = covered
		if covered < len(group) || c.Conflicts > 0 {
			continue
		}
		scored = append(scored, c)
	}
	if len(scored) == 0 {
		return Candidate{}, false
	}

	// STOP AT THE KNEE. The ladder runs specific -> general, so collateral only
	// grows along it; the right rule is the last rung before it grows sharply.
	//
	// This is a WALK, not a sort. Ranking safe candidates by breadth would pick
	// the broadest one, and "broadest" means "labels the most mail the user never
	// labelled" — the wrong default for a feature whose failure mode is confident
	// misfiling. Walking stops at the evidence instead.
	best := 0
	for i := 0; i+1 < len(scored); i++ {
		next, cur := scored[i+1].Collateral, scored[i].Collateral
		if next-cur >= minCollateralJump && next >= max(cur*kneeFactor, minCollateralJump) {
			scored[i].Discriminative = true
			best = i
			break
		}
		best = i + 1
	}
	return scored[best], true
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// sharedConjuncts is the maximally specific hypothesis: every leaf condition
// that holds for EVERY positive. Order is most-discriminating-last so the
// generalization ladder drops the narrowest facts first.
func sharedConjuncts(positives []MessageFacts) []Condition {
	first := positives[0]
	all := func(pred func(MessageFacts) bool) bool {
		for _, f := range positives {
			if !pred(f) {
				return false
			}
		}
		return true
	}

	var out []Condition

	// Narrowest first: the exact sender.
	if first.From != "" && all(func(f MessageFacts) bool {
		return strings.EqualFold(f.From, first.From)
	}) {
		out = append(out, Condition{Field: FieldFrom, Op: OpEquals, Value: first.From})
	}
	// The exact list, when they are all on the same one.
	if first.ListID != "" && all(func(f MessageFacts) bool {
		return strings.EqualFold(f.ListID, first.ListID)
	}) {
		out = append(out, Condition{Field: FieldListID, Op: OpEquals, Value: first.ListID})
	}
	// The domain.
	if first.FromDomain != "" && all(func(f MessageFacts) bool {
		return strings.EqualFold(f.FromDomain, first.FromDomain)
	}) {
		out = append(out, Condition{Field: FieldFromDomain, Op: OpEquals, Value: first.FromDomain})
	}
	// THE ALIAS — the category the user stated themselves (mailroom#177).
	//
	// A catchall user hands out a per-service address (sub@, bank@, shop@)
	// and thereby categorises their mail AT THE MOMENT THEY SIGN UP. No inference
	// can beat that, because it is not an inference: where it applies it is the
	// user's own declaration, needing no learning, no heuristics and no header
	// archaeology.
	//
	// It sits LATE — after the sender and the domain — so generalization drops
	// those first and the alias survives to become the rule. That ordering is the
	// point: placed early it would be discarded before it could ever be the
	// answer, which is effectively where it was before (invisible to the inducer
	// entirely). On real mail the alias is the narrowest conjunct
	// consistent with a whole label: every message filed as Shopping arrived
	// at shop@, and more of the subscription mail shares sub@ than shares any
	// sending domain.
	//
	// It sits BEFORE the bulk marker, though, and that is not arbitrary either.
	// Under a catchall a retailer's promos and its receipts arrive at the SAME
	// alias, so an alias-only rule would merge exactly the two things the
	// milestone's acceptance test says must stay apart. Keeping ListUnsub last
	// means the most general rung the ladder can reach is `alias AND unsubscribe`,
	// which still separates them.
	if first.Alias != "" && all(func(f MessageFacts) bool {
		return strings.EqualFold(f.Alias, first.Alias)
	}) {
		out = append(out, Condition{Field: FieldAlias, Op: OpEquals, Value: first.Alias})
	}
	// The bulk marker, LAST so it survives generalization longest. This is the
	// conjunct that separates a retailer's marketing from its receipts, and the
	// whole acceptance test turns on it outliving "which sender" and "which list".
	if all(func(f MessageFacts) bool { return f.ListUnsubscribe != "" }) {
		out = append(out, Condition{Field: FieldListUnsub, Op: OpExists})
	}
	return out
}

// generalizationLadder yields hypotheses from most specific to most general, by
// dropping leading (narrowest) conjuncts one at a time.
//
// A rung consisting ONLY of presence tests is dropped. `List-Unsubscribe exists`
// on its own does not say "mail like the examples" — it says "all bulk mail",
// which is a blanket rather than an induction from the evidence, and on a real
// mailbox it would relabel every newsletter the user gets. If the examples share
// nothing but "is bulk", the honest output is no candidate at all, exactly as it
// is for examples that share nothing.
func generalizationLadder(conjuncts []Condition) []Condition {
	var out []Condition
	for i := 0; i < len(conjuncts); i++ {
		rest := conjuncts[i:]
		if !identifying(rest) {
			continue
		}
		if len(rest) == 1 {
			out = append(out, rest[0])
			continue
		}
		clone := make([]Condition, len(rest))
		copy(clone, rest)
		out = append(out, Condition{All: clone})
	}
	return out
}

// identifying reports whether the conjunction pins something specific — a
// sender, a domain, a list — rather than only asserting a property every bulk
// message has.
func identifying(conjuncts []Condition) bool {
	for _, c := range conjuncts {
		if c.Op != OpExists && strings.TrimSpace(c.Value) != "" {
			return true
		}
	}
	return false
}

// score evaluates one hypothesis against every example.
func score(conds Condition, examples []Example, positives int) Candidate {
	c := Candidate{Conditions: conds, Positives: positives}
	seenLabel := map[string]bool{}
	for _, e := range examples {
		if !conds.Matches(e.Facts) {
			continue
		}
		if e.Positive {
			c.Covers++
			continue
		}
		c.Collateral++
		if len(e.OtherUserLabels) > 0 {
			c.Conflicts++
			for _, l := range e.OtherUserLabels {
				if !seenLabel[l] {
					seenLabel[l] = true
					c.ConflictLabels = append(c.ConflictLabels, l)
				}
			}
		}
	}
	sort.Strings(c.ConflictLabels)
	return c
}
