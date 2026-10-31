// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/senderwarn"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin sender-report` — #397 increment 0: measure internal/senderwarn
// against a real corpus BEFORE trusting it to warn anyone.
//
// senderwarn's own package comment already states what was measured on a
// real mailbox before it was written (almost every correspondent domain had its
// own distinct skeleton, and no non-ASCII sending domains). This command exists so that
// claim is REPRODUCIBLE against whatever mailbox it is pointed at, rather than
// something only true of the one corpus it was designed against.
//
// ⚠ THE COLLISION COUNT IN §2 IS THE SHIPPABILITY GATE, not a curiosity. If a
// user's own OWN correspondents collide with each other under Skeleton, then
// LookalikeDomain fires on mail from someone they already know — the exact
// false-positive senderAuth.ts and the display-name rule were both retired
// for (see the package comment's "WHAT WAS MEASURED" section). Zero collisions
// on this corpus is not evidence the mechanism is safe everywhere; it is
// evidence it did not already fail on the one corpus that could be checked.
//
// ⚠ PER-KIND COUNTS, NEVER ONE TOTAL (CLAUDE.md's measurement discipline,
// #177/#191/#206). "N messages warned" cannot distinguish a mailbox with no
// lookalikes from a Check() that has stopped firing at all; three kinds means
// three numbers.
//
// READ-ONLY. Like auth-report before it, there is no --apply: what this
// decides — whether the mechanism is worth shipping and at what threshold — is
// a judgement call, not a side effect.
func runSenderReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sender-report", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "list every warning: message id, from address, kind, resembles")
	userFlag := fs.String("user", "", "restrict the report to one user id (default: every active user)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("sender-report: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("sender-report: parse -user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	total := newSenderAgg()
	for _, uid := range users {
		agg, err := senderWarningsForUser(ctx, d, uid, *verbose)
		if err != nil {
			return err
		}
		agg.print(fmt.Sprintf("user %s", uid), *verbose)
		total.merge(agg)
	}
	fmt.Printf("\n\n########## TOTAL across %d user(s) ##########\n", len(users))
	// The warning list (item 6) is per-user detail already shown above; the total
	// section reports counts only, so it is not a second, differently-scoped copy
	// of the same rows.
	total.print("total", false)
	return nil
}

// warnRow is one Warning, kept for --verbose alongside the message it came from
// — a warning is only actionable when it says WHICH message, matching the
// "actionable, not ominous" reasoning behind Warning.Resembles itself.
type warnRow struct {
	messageID string
	from      string
	kind      string
	resembles string
}

// senderAgg is the corpus tally. Every population that could plausibly be zero
// carries the count it needs to say what that zero means, per CLAUDE.md's
// measurement-discipline rules — a bound must be stated, not just a total.
type senderAgg struct {
	scanned      int // messages examined
	usableDomain int // ...of which had a non-empty From domain
	unreadable   int // blob or MIME parse failure — NOT counted as "no warning"

	ownDomains          int
	correspondedDomains int
	// distinctDomains is own ∪ corresponded, deduped by exact string — the
	// population Skeleton is actually asked to separate, and the number
	// distinctSkeletons is compared against.
	distinctDomains   int
	distinctSkeletons int
	// collisionGroups: each element is >=2 domains that reduce to the same
	// skeleton. Non-empty here is the FINDING #2 exists to surface.
	collisionGroups [][]string

	byKind map[string]int // warning kind -> messages carrying it at least once
	fired  int            // messages carrying >=1 warning of ANY kind

	nonASCIIDomain int // From domain has a non-ASCII rune or an xn-- label
	mixedScript    int // senderwarn.MixedScript(domain) is true

	warnings []warnRow // populated only when --verbose
}

func newSenderAgg() *senderAgg {
	return &senderAgg{byKind: map[string]int{}}
}

func (a *senderAgg) merge(b *senderAgg) {
	a.scanned += b.scanned
	a.usableDomain += b.usableDomain
	a.unreadable += b.unreadable
	a.ownDomains += b.ownDomains
	a.correspondedDomains += b.correspondedDomains
	a.distinctDomains += b.distinctDomains
	a.distinctSkeletons += b.distinctSkeletons
	a.collisionGroups = append(a.collisionGroups, b.collisionGroups...)
	a.fired += b.fired
	a.nonASCIIDomain += b.nonASCIIDomain
	a.mixedScript += b.mixedScript
	a.warnings = append(a.warnings, b.warnings...)
	for k, v := range b.byKind {
		a.byKind[k] += v
	}
}

func senderWarningsForUser(ctx context.Context, d *deps, userID ulid.ULID, verbose bool) (*senderAgg, error) {
	agg := newSenderAgg()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return nil, err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("sender-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return nil, fmt.Errorf("sender-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	var domains store.SenderDomains
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		domains, e = tx.SenderDomainsFor(ctx)
		return e
	}); err != nil {
		return nil, fmt.Errorf("sender-report: sender domains: %w", err)
	}
	agg.ownDomains = len(domains.Own)
	agg.correspondedDomains = len(domains.Corresponded)
	agg.distinctDomains, agg.distinctSkeletons, agg.collisionGroups = skeletonCollisions(domains.Own, domains.Corresponded)

	ix := senderwarn.NewIndex(domains.Own, domains.Corresponded)

	const batch = 200
	var after ulid.ULID
	for {
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("sender-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++

			domain := domainOfAddr(ref.FromAddress)
			if domain == "" {
				continue
			}
			agg.usableDomain++

			if isNonASCIIDomain(domain) {
				agg.nonASCIIDomain++
			}
			if senderwarn.MixedScript(domain) {
				agg.mixedScript++
			}

			// ⚠ AuthPassed needs the Authentication-Results header, which lives in
			// the message blob, not on app.messages — the same asymmetry
			// auth-report.go documents. A message this report cannot read
			// contributes to `unreadable` and NOT to the warning counts below, so
			// every warning count is a lower bound over what could be read.
			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				agg.unreadable++
				d.log.Warn("sender-report: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil {
				agg.unreadable++
				d.log.Warn("sender-report: parse failed", "message", ref.ID, "err", err)
				continue
			}

			// THE SHIPPED PARSER (ml.AuthResultsOf), the same one auth-report and
			// the scorer use — a second regex here would measure code written to
			// agree with itself (#169, #220).
			res := ml.AuthResultsOf(msg.Header.Values("Authentication-Results"))
			authPassed := res.Has("dkim", "pass") || res.Has("dmarc", "pass")

			warnings := ix.Check(senderwarn.Sender{
				Domain:          domain,
				LocallyAuthored: ref.LocallyAuthored,
				AuthPassed:      authPassed,
			})
			if len(warnings) > 0 {
				agg.fired++
			}
			for _, w := range warnings {
				agg.byKind[w.Kind]++
				if verbose {
					agg.warnings = append(agg.warnings, warnRow{
						messageID: ref.ID.String(),
						from:      ref.FromAddress,
						kind:      w.Kind,
						resembles: w.Resembles,
					})
				}
			}
		}
	}
	return agg, nil
}

// isNonASCIIDomain reports a sending domain carrying a non-ASCII rune (the
// decoded-Unicode form, rare in a header) or an xn-- label (the wire form IDNA
// actually uses). Both are counted because a message can present either: most
// mail carries the punycode form in From, but a message this report reads
// straight from the address column could in principle hold either.
func isNonASCIIDomain(domain string) bool {
	for _, r := range domain {
		if r > unicode.MaxASCII {
			return true
		}
	}
	for _, label := range strings.Split(domain, ".") {
		if strings.HasPrefix(label, "xn--") {
			return true
		}
	}
	return false
}

// skeletonCollisions measures whether senderwarn.Skeleton actually separates the
// domains an Index is built from. It reproduces the SAME combined set
// senderwarn.NewIndex builds — own domains and corresponded domains, deduped by
// exact string — and groups them by Skeleton. Any group of size > 1 is the
// false-positive source #2 of the report exists to surface: two domains the
// user legitimately knows that render alike.
//
// ⚠ NORMALIZATION IS COPIED, NOT IMPORTED. senderwarn.normalizeDomain is
// unexported by design (a store or report layer must not quietly reinterpret
// what a message said), so this repeats its two steps — lowercase, strip a
// trailing root dot — rather than reaching around the package boundary.
func skeletonCollisions(own, corresponded []string) (distinctDomains, distinctSkeletons int, groups [][]string) {
	seen := map[string]bool{}
	var domains []string
	for _, list := range [][]string{own, corresponded} {
		for _, d := range list {
			d = strings.ToLower(strings.TrimSpace(d))
			d = strings.TrimSuffix(d, ".")
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			domains = append(domains, d)
		}
	}
	bySkel := map[string][]string{}
	for _, d := range domains {
		sk := senderwarn.Skeleton(d)
		bySkel[sk] = append(bySkel[sk], d)
	}
	for _, ds := range bySkel {
		if len(ds) > 1 {
			sort.Strings(ds)
			groups = append(groups, ds)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return strings.Join(groups[i], ",") < strings.Join(groups[j], ",") })
	return len(domains), len(bySkel), groups
}

func (a *senderAgg) print(label string, listWarnings bool) {
	fmt.Printf("\n=== %s: corpus ===\n", label)
	fmt.Printf("  messages examined               %d\n", a.scanned)
	fmt.Printf("  ...with a usable From domain     %d\n", a.usableDomain)
	if a.scanned == 0 {
		fmt.Printf("  — no messages in this corpus; every count below is vacuous (0 of 0).\n")
	} else if a.usableDomain < a.scanned {
		fmt.Printf("  %d message(s) had no usable From domain and were skipped — not a defect,\n",
			a.scanned-a.usableDomain)
		fmt.Printf("    just mail with no address for Check() to judge.\n")
	}
	if a.unreadable > 0 {
		fmt.Printf("  ⚠ unreadable (blob or MIME)      %d — NOT examined, so every warning count\n", a.unreadable)
		fmt.Printf("    below is a LOWER BOUND over what this report could actually read.\n")
	}

	fmt.Printf("\n=== %s: domain sets (senderwarn.Index) ===\n", label)
	fmt.Printf("  own domains                      %d\n", a.ownDomains)
	fmt.Printf("  corresponded-with domains        %d\n", a.correspondedDomains)
	fmt.Printf("  distinct domains (own U corresponded, deduped) %d\n", a.distinctDomains)
	fmt.Printf("  distinct skeletons                %d\n", a.distinctSkeletons)
	switch {
	case a.distinctDomains == 0:
		fmt.Printf("  — no domains at all: the index is empty, so Check() can only ever report\n")
		fmt.Printf("    PunycodeDomain here; LookalikeDomain and SelfSpoof are structurally silent.\n")
	case a.distinctSkeletons < a.distinctDomains:
		fmt.Printf("  ⚠ FINDING: %d domain(s) collide with another domain the user legitimately\n",
			a.distinctDomains-a.distinctSkeletons)
		fmt.Printf("    knows. That is a FALSE-POSITIVE SOURCE and the number that decides\n")
		fmt.Printf("    whether this feature is shippable:\n")
		for _, g := range a.collisionGroups {
			fmt.Printf("      %s\n", strings.Join(g, "  <->  "))
		}
	default:
		fmt.Printf("  — zero collisions: every known domain reduces to its own distinct skeleton.\n")
	}

	fmt.Printf("\n=== %s: firing rate, per kind (never one total) ===\n", label)
	pct := func(n int) float64 {
		if a.scanned == 0 {
			return 0
		}
		return 100 * float64(n) / float64(a.scanned)
	}
	for _, k := range []string{senderwarn.LookalikeDomain, senderwarn.PunycodeDomain, senderwarn.SelfSpoof} {
		fmt.Printf("  %-18s %6d  (%.2f%% of corpus)\n", k, a.byKind[k], pct(a.byKind[k]))
	}
	fmt.Printf("  ANY kind (messages, not warnings) %6d  (%.2f%% of corpus)\n", a.fired, pct(a.fired))

	if a.fired == 0 {
		fmt.Printf("\n=== %s: zero messages fired ===\n", label)
		fmt.Printf("  %d messages examined, 0 produced a warning.\n", a.scanned)
		fmt.Printf("  ⚠ That is consistent BOTH with a clean mailbox AND with a broken mechanism —\n")
		fmt.Printf("  a zero here cannot tell them apart. Its correctness rests on the unit tests'\n")
		fmt.Printf("  synthetic lookalikes (internal/senderwarn), NOT on this corpus: nothing in\n")
		fmt.Printf("  this run exercised the LookalikeDomain or SelfSpoof paths for real.\n")
	}

	fmt.Printf("\n=== %s: the Unicode half ===\n", label)
	fmt.Printf("  From domain non-ASCII or carrying an xn-- label  %d\n", a.nonASCIIDomain)
	fmt.Printf("  senderwarn.MixedScript true                       %d\n", a.mixedScript)
	if a.nonASCIIDomain == 0 && a.mixedScript == 0 {
		fmt.Printf("  both zero — expected on a pure-ASCII corpus, and not itself evidence the\n")
		fmt.Printf("  punycode/homograph paths work; that is what the unit tests are for.\n")
	}

	if listWarnings && len(a.warnings) > 0 {
		fmt.Printf("\n=== %s: every warning (--verbose) ===\n", label)
		for _, w := range a.warnings {
			resembles := w.resembles
			if resembles == "" {
				resembles = "-"
			}
			fmt.Printf("  %s  %-40s %-16s resembles=%s\n", w.messageID, trunc(w.from, 40), w.kind, resembles)
		}
	}
}
