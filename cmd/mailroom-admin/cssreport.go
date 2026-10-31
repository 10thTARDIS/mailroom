// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin css-report` — what the CSS property allowlist actually drops
// on this mailbox (#305).
//
// WHY IT EXISTS. v1.34.4 replaced the CSS regex with a property allowlist
// (#297). An allowlist is safe by construction however short it is, so the
// SECURITY half needed no corpus evidence and got a Chromium before/after
// instead. The RENDERING half is a different question that was never asked: the
// list's contents were written from what HTML mail is generally understood to
// need, not from what this mailbox actually contains. A list that is too narrow
// does not fail — it silently renders newsletters wrong.
//
// ⚠ IT MEASURES THE SHIPPED FILTER, NOT A COPY OF ONE. The counts come from
// api.MeasureHTMLCSS, which runs the real sanitizer with the real HTML walk and
// returns the sanitized output alongside the numbers; a test in internal/api
// asserts that output is byte-identical to SanitizeHTML. Had this command
// re-extracted the CSS and re-applied its own allowlist check, it would be
// measuring a second implementation written by the same hand to agree with the
// same conclusion — which is how this project spent a month judging a classifier
// on numbers from a broken evaluation (#169, #220).
//
// IT IS READ-ONLY AND HAS NO --apply. There is nothing to apply: it writes
// nothing and proposes nothing. Deciding whether the allowlist moves is a
// separate, explicit judgement made after reading this — deliberately in that
// order, because a list edited first and measured second is a list the
// measurement was fitted to.
//
// ⚠ PER-PROPERTY COUNTS, AND TWO INDEPENDENT COLUMNS. "N declarations dropped"
// cannot distinguish one newsletter template received two hundred times from two
// hundred senders each losing a property, and those call for opposite responses.
// DECLS and MESSAGES are therefore never summed into each other, and the table
// leads with MESSAGES because that is the number that predicts what the operator
// will actually notice.
func runCSSReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("css-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose corpus to measure (required)")
	top := fs.Int("top", 25, "show this many rows per table (0 = all)")
	batch := fs.Int("batch", 500, "messages per page of the keyset walk")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("css-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("css-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	agg, err := scanCorpusCSS(ctx, d, blobs, userID, dek, *batch)
	if err != nil {
		return err
	}
	agg.print(*top)
	return nil
}

// cssAggregate accumulates one corpus's measurements.
//
// Every map is keyed by property (or at-rule name) and paired with a
// messages-affected map, because the two are independent facts and merging them
// is the failure this report exists to avoid.
type cssAggregate struct {
	// --- what was examined, and what was NOT ---------------------------------
	scanned    int // messages walked
	unreadable int // blob read or MIME parse failed — NEVER counted as clean
	withHTML   int // messages having at least one text/html part
	htmlParts  int // text/html parts measured
	// nestedSkipped counts messages carrying an embedded message/rfc822 whose
	// own HTML this report does NOT measure. A bounded result must report its
	// bound (#155): without this line the report would read as "the whole
	// corpus" when a forwarded .eml's stylesheet was never looked at.
	nestedSkipped int

	propDrop  map[string]int
	propMsgs  map[string]map[ulid.ULID]bool
	valDrop   map[string]int
	valMsgs   map[string]map[ulid.ULID]bool
	kept      map[string]int
	malformed int

	atNotAllowed map[string]int
	atMsgs       map[string]map[ulid.ULID]bool
	atKept       map[string]int
	atPrelude    map[string]int
	atEmptyInner map[string]int

	ruleNoBlock      int
	rulePreludeEmpty int
	rulePreludeToken map[string]int
	ruleNoDeclsLeft  int

	styleElements      int
	styleEmptied       int
	styleMarkupGuard   int
	styleAttrs         int
	styleAttrsEmptied  int
	msgsLostAStyleElem map[ulid.ULID]bool

	emptiedByAllowlist int
	emptiedBySelector  int
	emptiedByBoth      int
	emptiedByNeither   int
}

func newCSSAggregate() *cssAggregate {
	return &cssAggregate{
		propDrop:           map[string]int{},
		propMsgs:           map[string]map[ulid.ULID]bool{},
		valDrop:            map[string]int{},
		valMsgs:            map[string]map[ulid.ULID]bool{},
		kept:               map[string]int{},
		atNotAllowed:       map[string]int{},
		atMsgs:             map[string]map[ulid.ULID]bool{},
		atKept:             map[string]int{},
		atPrelude:          map[string]int{},
		atEmptyInner:       map[string]int{},
		rulePreludeToken:   map[string]int{},
		msgsLostAStyleElem: map[ulid.ULID]bool{},
	}
}

func addMsg(m map[string]map[ulid.ULID]bool, key string, id ulid.ULID) {
	if m[key] == nil {
		m[key] = map[ulid.ULID]bool{}
	}
	m[key][id] = true
}

// add folds one message's measurement in. msgID is what makes the
// messages-affected column real rather than a second copy of the declaration
// count.
func (a *cssAggregate) add(msgID ulid.ULID, m api.CSSMeasurement) {
	for p, n := range m.PropNotAllowed {
		a.propDrop[p] += n
		addMsg(a.propMsgs, p, msgID)
	}
	for p, n := range m.ValueRejected {
		a.valDrop[p] += n
		addMsg(a.valMsgs, p, msgID)
	}
	for p, n := range m.DeclKept {
		a.kept[p] += n
	}
	a.malformed += m.DeclMalformed

	for n, c := range m.AtRuleNotAllowed {
		a.atNotAllowed[n] += c
		addMsg(a.atMsgs, n, msgID)
	}
	for n, c := range m.AtRuleKept {
		a.atKept[n] += c
	}
	for n, c := range m.AtRulePreludeBad {
		a.atPrelude[n] += c
	}
	for n, c := range m.AtRuleEmptyInner {
		a.atEmptyInner[n] += c
	}

	a.ruleNoBlock += m.RuleNoBlock
	a.rulePreludeEmpty += m.RulePreludeEmpty
	a.ruleNoDeclsLeft += m.RuleNoDeclsLeft
	for k, c := range m.RulePreludeToken {
		a.rulePreludeToken[k] += c
	}

	a.styleElements += m.StyleElements
	a.styleEmptied += m.StyleElementsEmptied
	a.styleMarkupGuard += m.StyleElementsMarkupGuard
	a.styleAttrs += m.StyleAttrs
	a.styleAttrsEmptied += m.StyleAttrsEmptied
	a.emptiedByAllowlist += m.EmptiedByAllowlist
	a.emptiedBySelector += m.EmptiedBySelector
	a.emptiedByBoth += m.EmptiedByBoth
	a.emptiedByNeither += m.EmptiedByNeither
	if m.StyleElementsEmptied > 0 {
		a.msgsLostAStyleElem[msgID] = true
	}
}

// scanCorpusCSS walks the user's messages page by page and measures every
// text/html part.
//
// ⚠ EVERY text/html PART IS MEASURED, which is a SUPERSET of what any one client
// fetches (a client asks for htmlBody and gets the alternative the RFC 8621
// algorithm picks). That is the deliberate direction to err in: the question is
// "does the allowlist have a hole", and a report that quietly measured a subset
// would understate a real one. It is printed rather than left implicit.
func scanCorpusCSS(
	ctx context.Context,
	d *deps,
	blobs blob.BlobStore,
	userID ulid.ULID,
	dek []byte,
	batch int,
) (*cssAggregate, error) {
	agg := newCSSAggregate()
	after := ulid.ULID{}

	for {
		var page []store.MessageBlobRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageBlobRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("css-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// NOT silently skipped. A body we could not read is a message this
				// report did not measure, and a report that dropped it would call
				// the corpus clean on evidence it never saw.
				agg.unreadable++
				d.log.Warn("css-report: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil {
				agg.unreadable++
				d.log.Warn("css-report: parse failed", "message", ref.ID, "err", err)
				continue
			}
			if len(msg.Nested) > 0 {
				agg.nestedSkipped++
			}

			sawHTML := false
			for _, p := range msg.Parts {
				if p.ContentType != "text/html" {
					continue
				}
				sawHTML = true
				agg.htmlParts++
				_, m := api.MeasureHTMLCSS(string(p.Data))
				agg.add(ref.ID, m)
			}
			if sawHTML {
				agg.withHTML++
			}
		}
		if len(page) < batch {
			break
		}
	}
	return agg, nil
}

func (a *cssAggregate) print(top int) {
	fmt.Printf("scanned                     : %d messages\n", a.scanned)
	fmt.Printf("unreadable                  : %d  (blob or MIME failure — NOT counted as clean)\n", a.unreadable)
	fmt.Printf("with a text/html part       : %d\n", a.withHTML)
	fmt.Printf("text/html parts measured    : %d\n", a.htmlParts)
	fmt.Printf("nested messages NOT walked  : %d  (a forwarded .eml's own CSS is outside this report)\n",
		a.nestedSkipped)
	fmt.Println()

	if a.htmlParts == 0 {
		// A zero with its denominator beside it. "No properties dropped" over
		// zero parts is not a clean bill of health, and printing it as one is
		// the exact mistake this report's shape exists to prevent (#176, #177).
		fmt.Println("no text/html parts were measured — every table below would be empty for that")
		fmt.Println("reason alone, not because the allowlist drops nothing")
		return
	}

	fmt.Println("=== <style> elements and style=\"\" attributes ===")
	fmt.Printf("  style elements seen                 : %d\n", a.styleElements)
	fmt.Printf("    dropped (nothing survived)        : %d, across %d message(s)\n",
		a.styleEmptied, len(a.msgsLostAStyleElem))
	// The question #305 asks. Attributed PER ELEMENT, not read off corpus
	// totals: "4402 rules lost to the allowlist and 3648 to selectors" cannot
	// say which of the two emptied any particular stylesheet, and they have
	// different fixes.
	fmt.Printf("      attributed to the property allowlist : %d\n", a.emptiedByAllowlist)
	fmt.Printf("      attributed to the selector grammar   : %d\n", a.emptiedBySelector)
	fmt.Printf("      needed BOTH fixes to survive         : %d\n", a.emptiedByBoth)
	fmt.Printf("      neither (empty or at-rule-only)      : %d\n", a.emptiedByNeither)
	fmt.Printf("  style=\"\" attributes seen            : %d\n", a.styleAttrs)
	fmt.Printf("    dropped (nothing survived)        : %d\n", a.styleAttrsEmptied)
	fmt.Printf("  dropped by the markup guard         : %d", a.styleMarkupGuard)
	if a.styleMarkupGuard > 0 {
		// writeStyleElement's own comment calls this unreachable. A hit is a
		// claim about the escaper being false, not a statistic about mail.
		fmt.Print("   ⚠ THIS SHOULD BE UNREACHABLE — read those messages by hand")
	}
	fmt.Println()
	fmt.Println()

	fmt.Println("=== why rules were dropped ===")
	fmt.Printf("  selector fine, EVERY declaration dropped : %d   (the property allowlist)\n", a.ruleNoDeclsLeft)
	fmt.Printf("  selector token refused                   : %d   (the selector grammar)\n",
		sumInts(a.rulePreludeToken))
	fmt.Printf("  selector empty or unbalanced             : %d\n", a.rulePreludeEmpty)
	fmt.Printf("  no block at all (a bare declaration)     : %d   (browsers drop these too)\n", a.ruleNoBlock)
	if len(a.rulePreludeToken) > 0 {
		fmt.Println("  refused selector tokens:")
		for _, c := range sortedCounts(a.rulePreludeToken) {
			fmt.Printf("    %-40s %6d\n", c.k, c.v)
		}
	}
	fmt.Println()

	fmt.Println("=== properties NOT on the allowlist ===")
	fmt.Println("  a hole in the list looks like a row here with a high MESSAGES count")
	printDropTable(a.propDrop, a.propMsgs, nil, top)

	fmt.Println("=== allowed properties whose VALUE was refused ===")
	fmt.Println("  a different problem with a different fix: the property is on the list and the")
	fmt.Println("  value rule is what rejected it. KEPT is the same property's surviving count —")
	fmt.Println("  a property rejected 20 times and kept 2000 is not the same finding as one")
	fmt.Println("  rejected 20 times and never kept at all.")
	printDropTable(a.valDrop, a.valMsgs, a.kept, top)

	fmt.Println("=== at-rules refused ===")
	printDropTable(a.atNotAllowed, a.atMsgs, a.atKept, top)

	fmt.Printf("malformed declarations (never well-formed): %d\n", a.malformed)
	fmt.Println("  not evidence about the allowlist — counted so it cannot be folded into")
	fmt.Println("  something that is.")
}

func sumInts(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

// printDropTable renders one drop table, sorted by MESSAGES affected via the
// shipped comparator so two runs over the same corpus print the same order.
func printDropTable(drops map[string]int, msgs map[string]map[ulid.ULID]bool, kept map[string]int, top int) {
	if len(drops) == 0 {
		fmt.Println("  (none)")
		fmt.Println()
		return
	}
	rows := make([]api.CSSDrop, 0, len(drops))
	for name, n := range drops {
		rows = append(rows, api.CSSDrop{Name: name, Declarations: n, Messages: len(msgs[name])})
	}
	api.SortCSSDrops(rows)

	shown := rows
	if top > 0 && len(shown) > top {
		shown = shown[:top]
	}
	if kept == nil {
		fmt.Printf("  %-42s %8s %9s\n", "PROPERTY", "DECLS", "MESSAGES")
		for _, r := range shown {
			fmt.Printf("  %-42s %8d %9d\n", r.Name, r.Declarations, r.Messages)
		}
	} else {
		fmt.Printf("  %-42s %8s %9s %8s\n", "NAME", "DROPPED", "MESSAGES", "KEPT")
		for _, r := range shown {
			fmt.Printf("  %-42s %8d %9d %8d\n", r.Name, r.Declarations, r.Messages, kept[r.Name])
		}
	}
	// A truncated table says so, with the number it hid. A top-N that silently
	// cuts reads as the whole population (#155).
	if len(shown) < len(rows) {
		fmt.Printf("  … %d more not shown (--top 0 for all)\n", len(rows)-len(shown))
	}
	fmt.Println()
}
