// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// runInduce reports what rule induction WOULD propose for each hand-applied
// label, over real mail (#177, #168).
//
// Rule/induce is reachable only over JMAP, which makes "did this change help?"
// a question you can currently answer only by clicking through the web client
// one label at a time. That is not a basis for changing the hypothesis space —
// and changing it by intuition is exactly how the kind layer got worse twice
// (#175). This prints the same numbers the JMAP method returns (covers,
// collateral, conflicts, discriminative) for every label at once, so a conjunct
// can be added and the before/after compared on real evidence.
//
// It is READ-ONLY: it proposes, exactly as Rule/induce does, and writes nothing.
func runInduce(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("induce", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		limit     = fs.Int("limit", 5000, "how many messages to scan as evidence")
		top       = fs.Int("top", 3, "how many candidates to print per label")
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
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if candidates, e = tx.RulePreviewCandidates(ctx, *limit); e != nil {
			return e
		}
		if total, e = tx.CountPreviewableMessages(ctx); e != nil {
			return e
		}
		labels, e = tx.ListLabels(ctx)
		return e
	}); err != nil {
		return err
	}

	// A bounded scan MUST report its bound, or a lower bound reads as a total
	// (the #155 lesson).
	fmt.Printf("evidence: %d of %d messages scanned%s\n\n",
		len(candidates), total,
		map[bool]string{true: "", false: "  (TRUNCATED — raise --limit)"}[len(candidates) >= total])

	names := map[string]string{}
	for _, l := range labels {
		names[l.ID.String()] = l.Name
	}

	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	for _, l := range labels {
		target := l.ID.String()
		examples := make([]rules.Example, 0, len(candidates))
		positives := 0
		aliasHist := map[string]int{}
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
				aliasHist[orNone(c.Alias)]++
			}
			examples = append(examples, ex)
		}

		fmt.Printf("%-14s %d hand-labelled\n", l.Name, positives)
		if positives == 0 {
			fmt.Printf("  (no evidence)\n\n")
			continue
		}
		// The alias spread across the positives is the fact that decides whether
		// an alias conjunct can even apply: one alias = a usable conjunct, many =
		// the label spans services and the alias is correctly dropped.
		fmt.Printf("  aliases  : %s\n", histLine(aliasHist))

		induced := rules.Induce(examples)
		if len(induced) == 0 {
			fmt.Printf("  candidates: (none — nothing consistent explains the examples)\n\n")
			continue
		}
		for i, c := range induced {
			if i >= *top {
				break
			}
			mark := " "
			if c.Discriminative {
				mark = "*"
			}
			fmt.Printf("  %s covers %d/%d  collateral %-4d conflicts %-3d %s\n",
				mark, c.Covers, c.Positives, c.Collateral, c.Conflicts, describe(c.Conditions))
		}
		fmt.Println()
	}
	return nil
}

// factsFromCandidate projects a stored candidate into rule facts. It mirrors
// api.factsFor with needBodies=false — induction reads only stored columns, so
// no blob is opened.
func factsFromCandidate(c store.RulePreviewCandidate) rules.MessageFacts {
	return rules.MessageFacts{
		From:            c.From,
		FromDomain:      store.AddressDomain(store.NormalizeAddress(c.From)),
		Subject:         c.Subject,
		Alias:           c.Alias,
		ListID:          c.ListID,
		ListUnsubscribe: c.ListUnsub,
		Headers:         map[string]string{},
	}
}

// describe renders a condition tree compactly enough to scan a column of them.
func describe(c rules.Condition) string {
	if len(c.All) > 0 {
		return "(" + joinConds(c.All, " AND ") + ")"
	}
	if len(c.Any) > 0 {
		return "(" + joinConds(c.Any, " OR ") + ")"
	}
	if c.Op == rules.OpExists {
		return c.Field + " exists"
	}
	return fmt.Sprintf("%s %s %q", c.Field, c.Op, c.Value)
}

func joinConds(cs []rules.Condition, sep string) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, describe(c))
	}
	return strings.Join(parts, sep)
}

// histLine renders a small count map on one line, largest first.
func histLine(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	rows := make([]kv, 0, len(m))
	for k, v := range m {
		rows = append(rows, kv{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].v != rows[j].v {
			return rows[i].v > rows[j].v
		}
		return rows[i].k < rows[j].k
	})
	parts := make([]string, 0, len(rows))
	for i, r := range rows {
		if i >= 6 {
			parts = append(parts, fmt.Sprintf("… %d more", len(rows)-6))
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", r.k, r.v))
	}
	return strings.Join(parts, "  ")
}
