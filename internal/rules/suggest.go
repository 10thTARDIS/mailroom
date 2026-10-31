// SPDX-License-Identifier: AGPL-3.0-only

package rules

import "strings"

// Rule-sourced label suggestions (#242): the two pure functions that turn a
// matched rule into the two things app.label_suggestions stores about it — a
// sort key, and a human-readable account of why it fired.
//
// They live in internal/rules rather than in the store because they are pure
// projections of a Condition tree and an integer, with no database in them, and
// because internal/store CANNOT import internal/ml (internal/ml imports
// internal/store — see store.PutRuleSuggestion). Keeping the rendering here also
// means the ingest path and the admin backfill get the same phrases from the same
// code, which is the whole reason store.PutRuleSuggestion exists as one funnel.

// EvidenceConfidence maps "how many of the user's own filings back this
// candidate" onto app.label_suggestions.confidence.
//
// ⚠ THE RESULT IS AN ORDERING RANK. IT IS NOT A PROBABILITY. Nothing here
// estimates the chance that the label is correct, nothing is calibrated against
// anything, and no measurement stands behind the numbers it returns. 4 filings
// yielding 0.8 does NOT mean "80% likely to be right"; it means only "sorts above
// a candidate backed by 2". Read as a percentage it is a fabricated statistic,
// which is why the wire carries a `source` field alongside it (store.LabelSuggestion.Source):
// the client can see that this row came from a rule and must render the evidence
// COUNT, never this number.
//
// WHY THE COLUMN NEEDS A VALUE AT ALL, since it is not a probability. The
// needs-a-category surface orders on
//
//	(SELECT max(ls.confidence) FROM app.label_suggestions ls
//	  WHERE ls.user_id = m.user_id AND ls.message_id = m.id) DESC NULLS LAST
//
// (internal/store/jmap_email.go:788). confidence is NOT NULL, so a rule-sourced
// row must put SOMETHING there. Storing a constant — 1.0, or 0.5 — would make
// that max() identical for every suggested message, collapsing the whole sort to
// its date tie-break: the queue would silently become "newest first" while still
// being labelled most-confident-first, for every user whose suggestions come from
// rules. That is a surface asserting an order it does not have, which is worse
// than an admittedly crude one.
//
// THE SHAPE, and why this one: 1 - 1/(1+positives). It is strictly increasing in
// positives (more of the user's own evidence sorts higher), it is bounded in
// [0, 1) so it can never collide with a model's certainty at exactly 1.0, and its
// steps shrink — 1→0.5, 2→0.667, 4→0.8, 9→0.9 — so the gap between 2 filings and
// 4 matters far more than the gap between 20 and 40, which is the right shape for
// evidence counts. It is chosen for those ordering properties alone. No fit, no
// dataset, no claim.
//
// positives < 1 returns 0: there is no evidence, so the candidate sorts last
// among suggestions while still being a suggestion. It is reachable — a
// hand-written suggestLabel rule carries no Evidence at all (see Action.Evidence,
// which is deliberately unvalidated) — and 0 is the honest rank for "nothing
// backs this but the rule itself".
func EvidenceConfidence(positives int) float64 {
	if positives < 1 {
		return 0
	}
	return 1 - 1/(1+float64(positives))
}

// DescribeCondition renders a condition tree as human-readable phrases, ONE PER
// CONJUNCT — the sentences a person reads on a needs-a-category queue row when
// they ask why a label is being suggested (#242, #176's explainability contract).
//
// One string per conjunct rather than one joined sentence, because the consumer
// stores them as separate []ml.FeatureContribution entries and the surface renders
// them as a list. An AND of three tests is three reasons, and a user scanning them
// is checking each against the message in front of them.
//
// An `any` group is a DISJUNCTION and therefore ONE conjunct: it collapses to a
// single "x or y" phrase, with a nested `all` inside it parenthesised, so the
// boolean structure survives into the text instead of being flattened into a lie
// about what the rule tests.
//
// PHRASING IS PLAIN AND LOWERCASE-LED ("alias is sub@example.com", "has a
// List-Unsubscribe header", "subject contains sale") because these read as items
// in a list under a heading, not as sentences. It deliberately does NOT match
// ml.FeatureLabel's capitalised style: those are model features and these are
// literal rule conditions the user can verify, and the difference in voice is
// worth keeping visible.
//
// It renders what the tree SAYS, never what it would match — no facts are
// consulted and it is a pure function of the condition. An unrecognised field or
// operator falls back to naming itself rather than being dropped, for the reason
// ml.FeatureLabel falls back to its key: a conjunct that silently disappears from
// an explanation makes the rule look narrower than it is, and this project has
// paid repeatedly for code that ran and quietly did nothing. A node that is
// neither a group nor a leaf yields NO phrase, which is correct rather than
// lossy — such a node matches nothing (Condition.match's safe default), so no
// suggestion can ever have been produced by one.
func DescribeCondition(c Condition) []string {
	switch {
	case len(c.All) > 0:
		var out []string
		for _, sub := range c.All {
			out = append(out, DescribeCondition(sub)...)
		}
		return out
	case len(c.Any) > 0:
		var parts []string
		for _, sub := range c.Any {
			ps := DescribeCondition(sub)
			switch len(ps) {
			case 0:
				// An unrenderable branch of an `any` is skipped rather than joined
				// as an empty string, which would render as "x or  or y".
				continue
			case 1:
				parts = append(parts, ps[0])
			default:
				// A nested `all` inside an `any`: parenthesised so "(a and b) or c"
				// cannot be misread as "a and (b or c)".
				parts = append(parts, "("+strings.Join(ps, " and ")+")")
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return []string{strings.Join(parts, " or ")}
	default:
		if p := describeLeaf(c); p != "" {
			return []string{p}
		}
		return nil
	}
}

// describeLeaf renders one leaf condition. It returns "" only for the empty node
// (no field at all), which matches nothing and therefore describes nothing.
func describeLeaf(c Condition) string {
	if strings.TrimSpace(c.Field) == "" {
		return ""
	}
	subject, presence, _ := fieldPhrase(c)
	if c.Op == OpExists {
		// Presence reads as "has …", and the three header-backed fields name their
		// header, because "has a List-Unsubscribe header" is something a user can go
		// and check in the message source while "has a list unsubscribe" is not.
		return "has " + presence
	}
	return subject + " " + opPhrase(c.Op) + " " + c.Value
}

// fieldPhrase returns how a field is named as the SUBJECT of a comparison
// ("subject contains sale"), and as the object of a PRESENCE test ("has a
// List-Unsubscribe header"). The two differ enough — definite vs indefinite, and
// header fields naming the header itself — that deriving one from the other
// produced worse English than listing both.
//
// The presence article is always "a", including before names that would
// idiomatically take "an" ("has a X-Mailer header"). Getting that right for
// arbitrary user-supplied header names needs a pronunciation table; a slightly
// wrong article costs a reader nothing, and a table would rot.
//
// An unknown field names itself. Validation rejects one at save time, so ingest
// cannot meet it — but this function is exported through DescribeCondition and
// the store applies it to rows it did not write, so the fallback is here rather
// than assumed away.
//
// `known` reports whether the field had a real arm, and exists ONLY so a test can
// see the difference. Nothing in production branches on it: the fallback renders
// either way. Without it, "a new Field constant was added and nobody gave it a
// phrase" is invisible — the queue would read "listArchive is x" forever, working
// and plausible and wrong, and no assertion about the OUTPUT can catch it because
// two of the eight fields (subject, alias) legitimately render as their own key,
// so "the phrase equals the field name" is not a fallback detector.
func fieldPhrase(c Condition) (subject, presence string, known bool) {
	switch c.Field {
	case FieldFrom:
		return "sender", "a sender address", true
	case FieldFromDomain:
		return "sender domain", "a sender domain", true
	case FieldTo:
		// "a recipient", singular and indefinite, because matchList matches if ANY
		// To/Cc entry does — "recipients is bob@" would claim the wrong thing.
		return "a recipient", "a recipient", true
	case FieldSubject:
		return "subject", "a subject", true
	case FieldAlias:
		// The alias is the user's own declaration of what a message is (see
		// induce.go): under a catchall they categorised it at signup. It is named
		// plainly for that reason — "alias is sub@example.com" is the whole
		// explanation, and dressing it up as "addressed identity" would obscure it.
		return "alias", "an alias", true
	case FieldListID:
		return "the List-Id header", "a List-Id header", true
	case FieldListUnsub:
		return "the List-Unsubscribe header", "a List-Unsubscribe header", true
	case FieldHeader:
		name := strings.TrimSpace(c.Header)
		if name == "" {
			// Validation requires a name; a row that somehow lacks one still gets a
			// phrase that says so rather than "the  header".
			return "an unnamed header", "an unnamed header", true
		}
		return "the " + name + " header", "a " + name + " header", true
	default:
		return c.Field, "a " + c.Field, false
	}
}

// opPhrase is the verb for a value-taking operator. An unknown operator renders
// as itself: matchStr treats it as non-matching, so it cannot have fired, and
// printing it is more useful than pretending the leaf was not there.
func opPhrase(op string) string {
	switch op {
	case OpEquals:
		return "is"
	case OpContains:
		return "contains"
	case OpMatches:
		return "matches"
	default:
		return op
	}
}
