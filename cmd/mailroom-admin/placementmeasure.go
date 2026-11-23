// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/placement"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin placement-measure` (#28, #169): MEASURE learned sorting
// before changing it. READ-ONLY. Counts and your area names only (no addresses,
// senders or subjects). Area names are yours: review them before pasting the
// output anywhere public.
//
// It exists because the next changes to learned placement (act on a single
// filing, learn from "opened and left alone" or "archived unread", key by
// List-Id, recency for mixed senders, accept senders that never pass
// authentication) are each a bet about the user's own mail, and the project's
// costliest mistakes have been features built on a number nobody audited. Each
// section answers one question about a real mailbox; none of them changes what
// learned placement does. `placement-report` keeps describing what learned
// placement has DONE; this describes what it COULD do.
//
// Section 2 reads raw headers (the placement backfill's exact approach: the
// blob store and the user's key) unless --no-headers, in which case it falls
// back to the stored failure flag and says its numbers are then a lower bound.
func runPlacementMeasure(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("placement-measure", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		window    = fs.Int("window-days", store.MeasureWindowDays, "how far back sections 4 and 5 read messages")
		split     = fs.Int("split-days", store.MeasureSplitDays, "days before now that the evidence/outcome split day D falls")
		recent    = fs.Int("recent-days", store.MeasureRecentDays, "the recent window of section 3")
		scanMax   = fs.Int("auth-scan-max", store.MeasureAuthScanMax, "most messages section 2 reads the raw headers of")
		noHeaders = fs.Bool("no-headers", false, "section 2 from the stored failure flag only (a lower bound; reads no message bytes)")
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
	trusted, err := trustedAuthservEntries()
	if err != nil {
		return err
	}
	opts := store.PlacementMeasureOptions{
		Now: time.Now(), WindowDays: *window, SplitDays: *split, RecentDays: *recent, AuthScanMax: *scanMax,
	}
	if !*noHeaders {
		blobs, err := openBlobStore(ctx, d)
		if err != nil {
			return err
		}
		master, err := d.keys.MasterKey(ctx)
		if err != nil {
			return fmt.Errorf("placement-measure: master key: %w", err)
		}
		dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
		crypto.Zero(master)
		if err != nil {
			return fmt.Errorf("placement-measure: derive mail dek: %w", err)
		}
		defer crypto.Zero(dek)
		opts.Senders = ingest.BackfillSenders(blobs, userID, dek, trusted)
	}
	var m store.PlacementMeasure
	if err := d.store.UserReadTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		m, e = tx.PlacementMeasure(ctx, opts)
		return e
	}); err != nil {
		return fmt.Errorf("placement-measure: %w", err)
	}
	printPlacementMeasure(os.Stdout, m, trusted)
	return nil
}

// measureReasonOrder is the vocabulary section 1 prints, in reading order, each
// with what it means. A reason ingest records that is not listed here is
// printed after them rather than dropped.
var measureReasonOrder = []struct{ reason, what string }{
	{"none", "no filings of the sender's own, address or domain"},
	{"thin", "agreeing filings, but fewer than the threshold"},
	{"mixed", "filed into more than one area"},
	{"contradicted", "a learned placement was corrected and nothing newer was filed"},
	{"blocked", "only a freemail or own domain has evidence"},
	{"nokey", "no usable From address"},
	{"ambiguous", "not one mailbox in one From header"},
	{"unauthenticated", "confident, but no trusted DMARC pass for the From domain"},
	{"warned", "confident, but a spoofing or look-alike warning applies"},
	{"gated", "confident, but the area's correction gate has stopped it"},
	{"agrees", "confident, and the desk already put it there"},
	{"already_placed", "a rule's label had already decided the area"},
	{"off", "learned placement is switched off for this login"},
}

// printPlacementMeasure renders the report. Separate from the run so a test can
// read it without a database.
func printPlacementMeasure(w io.Writer, m store.PlacementMeasure, trusted []string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	name := func(id ulid.ULID) string {
		if n, ok := m.AreaNames[id]; ok {
			return safeText(n)
		}
		return id.String()
	}
	areas := func(ids ...map[ulid.ULID]struct{}) []ulid.ULID {
		seen := map[ulid.ULID]bool{}
		var out []ulid.ULID
		for _, set := range ids {
			for id := range set {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return name(out[i]) < name(out[j]) })
		return out
	}
	day := func(t time.Time) string { return t.UTC().Format("2006-01-02") }

	p("=== placement-measure (#28, #169): what learned sorting could do, measured before it is changed ===\n")
	p("REPORT ONLY: counts and your area names only (no addresses, senders or subjects). Area names are yours: review them before pasting the output anywhere public.\nNothing here changes where mail goes.\n")
	p("as of %s · split day D = %s (evidence judged before D, outcomes from D on, so no signal is scored on its own decisions)\n",
		m.Now.UTC().Format("2006-01-02 15:04Z"), day(m.Split))
	p("messages read for sections 4 and 5: created since %s (%d days); a message is judged once it is %d days old\n\n",
		day(m.WindowStart), m.Options.WindowDays, store.MeasureLeftAloneDays)

	// 1.
	t := m.Abstentions
	p("1. WHY ARRIVALS WERE NOT PLACED (last %d days, since %s)\n", t.Days, day(t.Since))
	if t.FirstRecorded.IsZero() {
		p("   nothing recorded yet: this is counted at arrival, going forward, from the release that added it.\n")
	} else {
		p("   recorded since %s: %d of %d days covered", day(t.FirstRecorded), t.DaysCovered, t.Days)
		if t.DaysCovered < t.Days {
			p(" (counts below are a LOWER BOUND on the window)")
		}
		p("\n")
	}
	listed := map[string]bool{}
	for _, r := range measureReasonOrder {
		listed[r.reason] = true
		p("   %-16s %7d   %s\n", r.reason, t.ByReason[r.reason], r.what)
	}
	var extra []string
	for r := range t.ByReason {
		if !listed[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(extra)
	for _, r := range extra {
		p("   %-16s %7d   (not in this report's vocabulary)\n", r, t.ByReason[r])
	}
	p("   %-16s %7d\n\n", "held back, total", t.Total())

	// 2.
	a := m.Auth
	p("2. AUTHENTICATION-ONLY REFUSALS (#169): filed senders refused for authentication and nothing else\n")
	if a.Exact {
		if len(trusted) > 0 {
			p("   exact: each message judged on its own raw headers; needs a DMARC pass from a trusted authserv-id (%s)\n", strings.Join(trusted, ","))
		} else {
			p("   exact: each message judged on its own raw headers; NO trusted authserv-id configured (%s), so the floor is \"did not clearly fail\"\n", config.EnvAuthservIDs)
		}
	} else {
		p("   LOWER BOUND: stored failure flag only (--no-headers); a missing trusted DMARC pass is invisible here\n")
	}
	p("   population: %d message(s) from keys that are confident or thin and that no label already placed; judged %d", a.Population, a.Read)
	if a.Truncated {
		p(" (newest first; CAPPED at --auth-scan-max, so every figure below covers only these)")
	}
	p("; unreadable %d\n", a.Unreadable)
	p("   %-10s %9s %7s %9s %10s %10s %10s %11s %12s\n", "KEY IS", "MESSAGES", "PASSES", "AUTH ONLY", "AUTH+OTHER", "OTHER ONLY", "AUTH KEYS", "RULE-COVERED", "MSGS COVERED")
	for _, r := range []struct {
		n string
		b store.AuthBucket
	}{{"confident", a.Confident}, {"thin", a.Thin}} {
		p("   %-10s %9d %7d %9d %10d %10d %10d %11d  %12d\n", r.n, r.b.Messages, r.b.OK, r.b.AuthOnly, r.b.AuthAlso, r.b.OtherOnly,
			r.b.AuthOnlyKeys, r.b.CoveredKeys, r.b.CoveredMessages)
	}
	p("   (AUTH ONLY: refused for authentication and for nothing else. AUTH KEYS: distinct keys behind them. RULE-COVERED: of those, keys an enabled rule\n" +
		"    already files into the key's area; an UPPER bound, since a rule that merely names the address or domain counts.)\n\n")

	// 3.
	p("3. RECENCY, RE-MEASURED: mixed keys that end in a run of N filings into one area, nothing elsewhere in between\n")
	for _, k := range []struct {
		kind string
		r    placement.Recency
	}{{"address", m.RecencyAddress}, {"domain", m.RecencyDomain}} {
		p("   %-8s keys with filings %d · mixed %d · mixed keys' mail in the last %d days %d\n", k.kind, k.r.Keys, k.r.Mixed, m.RecentDays, k.r.MixedRecent)
		for _, row := range k.r.Rows {
			p("     last %d filings agree: %4d key(s), %6d message(s) in the last %d days\n", row.N, row.Keys, row.Recent, m.RecentDays)
		}
	}
	p("   (filings that share one instant never lengthen a run: a bulk session cannot fake recency. Domain keys leave out freemail and own domains.)\n\n")

	// 4.
	c := m.LeftAloneCover
	p("4. THE \"LEFT ALONE\" SIGNAL (measured, never acted on): the desk placed it, the user opened it, and did not move it within %d days\n", store.MeasureLeftAloneDays)
	p("   events before D: %d desk-placed message(s) old enough to judge, %d of them opened in the window\n", c.DeskPlaced, c.Opened)
	if c.EarliestOpen.IsZero() {
		p("   no `open` event exists at all: the opened signal cannot have fired for any message\n")
	} else {
		p("   the first `open` event on record is %s: nothing opened before then can count\n", day(c.EarliestOpen))
	}
	p("   not observable: %d message(s) the user filed more than %d days after they arrived (the desk's label, which named the original area, was\n"+
		"   deleted by the move), so the events lack the failures that happened late; that can only flatter the signal.\n", c.UnobservableMoved, store.MeasureLeftAloneDays)
	p("   outcomes: %d message(s) arrived since D and old enough to judge; left out because a rule decided them %d, learned placement decided them %d\n",
		c.Outcomes, c.ExcludedRule, c.ExcludedLearned)
	p("   existing evidence before D: %d filing(s) by the user; a key they already cover is not new reach\n", m.UserEvidence)
	for _, v := range []struct {
		title string
		r     placement.LeftAloneResult
	}{{"OPENED and not moved (the signal)", m.LeftAlone}, {"merely NOT MOVED (contrast: \"opened\" dropped)", m.LeftAloneAny}} {
		p("   -- %s: %d event(s)\n", v.title, v.r.Events)
		ids := map[ulid.ULID]struct{}{}
		for id := range v.r.EventsByArea {
			ids[id] = struct{}{}
		}
		for id := range v.r.Keys.Confident {
			ids[id] = struct{}{}
		}
		for id := range v.r.Areas {
			ids[id] = struct{}{}
		}
		p("      %-18s %6s %11s %11s %7s %6s %9s %10s %7s  %s\n", "AREA", "EVENTS", "KEYS ADDR/DOM", "NEW ADDR/DOM", "LATER", "KEPT", "CORRECTED", "PRECISION", "LCB95", "AGAINST THE LIVE GATE")
		for _, id := range areas(ids) {
			k, nk, ap := v.r.Keys.Confident[id], v.r.Keys.New[id], v.r.Areas[id]
			p("      %-18s %6d %6d/%-4d %6d/%-4d %7d %6d %9d %10s %7s  %s\n", name(id), v.r.EventsByArea[id], k.Addresses, k.Domains,
				nk.Addresses, nk.Domains, ap.Messages, ap.Kept, ap.Corrected, placementRatio(ap.Gate.Precision), placementRatio(ap.Gate.LCB), gateVerdict(ap.Gate))
		}
		tot := v.r.Total
		p("      %-18s %6s %11s %11s %7d %6d %9d %10s %7s  %s\n", "all areas", "", "", "", tot.Messages, tot.Kept, tot.Corrected,
			placementRatio(tot.Gate.Precision), placementRatio(tot.Gate.LCB), gateVerdict(tot.Gate))
		p("      later mail already covered by the user's own evidence %d; from keys this signal did not teach %d\n", v.r.Served, v.r.Untaught)
		p("      filed by hand into ANY area: taught keys %d of %d later message(s), untaught keys %d of %d (the base rate: a signal that does not beat it says nothing)\n",
			v.r.MovedAnyTaught, v.r.Total.Messages, v.r.MovedAnyUntaught, v.r.Untaught)
		p("      key verdicts: address %s · domain %s\n", reasonCounts(v.r.Keys.Reasons[placement.KeyAddress]), reasonCounts(v.r.Keys.Reasons[placement.KeyDomain]))
	}
	p("   (PRECISION is kept / (kept + corrected) over later mail from NEW keys; corrected = the user filed it into a DIFFERENT area than the one taught.\n"+
		"    A later move of any age counts, so this is stricter than the live gate's %d days.)\n\n", int(placement.KeptAfter.Hours()/24))

	// 5.
	ar, ac := m.Archived, m.ArchivedCover
	p("5. THE \"ARCHIVED UNREAD\" SIGNAL (measured, never acted on): later mail in %s from senders whose mail the user archives unopened\n", safeText(m.PrimaryName))
	p("   events before D: %d archive-unread event(s) on a message with an area (%d more had no area and were dropped)\n", ar.Events, ac.EventsNoArea)
	p("   outcomes: %d later message(s) in %s from taught senders and %d from senders the signal did not teach\n", sumArchive(ar), safeText(m.PrimaryName), ar.Untaught)
	ids := map[ulid.ULID]struct{}{}
	for id := range ar.Keys.Confident {
		ids[id] = struct{}{}
	}
	for id := range ar.Areas {
		ids[id] = struct{}{}
	}
	p("   %-18s %10s %7s %7s %7s %6s %7s  %-18s  %s\n", "AREA OF ORIGIN", "KEYS ADDR/DOM", "LATER", "UNREAD", "OPENED", "KEPT", "OTHER", "STRICT (prec / LCB95)", "ENGAGED (prec / LCB95)")
	for _, id := range areas(ids) {
		k, r := ar.Keys.Confident[id], ar.Areas[id]
		if r.Messages == 0 {
			// Keys taught, but no later mail to score: nothing measured, so "-".
			r.Strict, r.Engaged = placement.GateStatus{Precision: nan(), LCB: nan()}, placement.GateStatus{Precision: nan(), LCB: nan()}
		}
		p("   %-18s %5d/%-4d %7d %7d %7d %6d %7d  %7s / %-7s     %7s / %-7s\n", name(id), k.Addresses, k.Domains, r.Messages,
			r.ArchivedUnread, r.Opened, r.KeptUnopened, r.ArchivedOther,
			placementRatio(r.Strict.Precision), placementRatio(r.Strict.LCB), placementRatio(r.Engaged.Precision), placementRatio(r.Engaged.LCB))
	}
	p("   key verdicts: address %s · domain %s\n", reasonCounts(ar.Keys.Reasons[placement.KeyAddress]), reasonCounts(ar.Keys.Reasons[placement.KeyDomain]))
	p("   (STRICT: of later mail in %s from these senders, how much was archived unread again, so the rest was opened or kept. ENGAGED: archived unread\n"+
		"    against opened only. UNREAD/OPENED/KEPT/OTHER partition LATER: other = archived or trashed with no sign it was read.)\n\n", safeText(m.PrimaryName))

	// 6.
	l := m.ListID
	p("6. LIST-ID IN THE KEY: the ceiling (a key of address + List-Id)\n")
	p("   sending addresses whose mail carries more than one List-Id: %d\n", m.ListIDAddresses)
	p("   the user's filings read: %d (%d with the List-Id column not filled yet, read as \"none\"; %d of %d messages are filled, so this is a lower bound until the recipients backfill has run)\n",
		l.Filings, l.Unfilled, m.ListIDCensus.Filled, m.ListIDCensus.Total)
	p("   addresses filed into more than one area (mixed): %d, of which carry more than one List-Id: %d\n", l.MixedAddresses, l.MultiList)
	p("     a List-Id key would make the address clean (every list in one area): %d\n", l.Separated)
	p("     would clean some lists and not others: %d\n", l.Partial)
	p("     would not help (every list still mixed): %d\n", l.NotHelped)
	p("   (address + List-Id) keys that would reach %d agreeing filings: %d  <- the ceiling on keys List-Id keying adds\n", placement.MinFilings, l.SubKeysConfident)
	p("\nREPORT ONLY: nothing was written.\n")
}

func nan() float64 { return math.NaN() }

func sumArchive(r placement.ArchiveResult) int {
	n := 0
	for _, a := range r.Areas {
		n += a.Messages
	}
	return n
}

// gateVerdict says what the live correction gate would make of a would-have-been
// precision: nothing while too few outcomes, else whether its lower bound clears
// the floor.
func gateVerdict(g placement.GateStatus) string {
	switch {
	case g.Resolved == 0:
		return "-"
	case !g.Judged:
		return fmt.Sprintf("not judged (%d/%d)", g.Resolved, placement.GateMinResolved)
	case g.Stopped:
		return fmt.Sprintf("would be STOPPED (LCB < %.2f)", placement.GateMinLCB)
	default:
		return fmt.Sprintf("would act (LCB >= %.2f)", placement.GateMinLCB)
	}
}

func reasonCounts(m map[placement.Reason]int) string {
	return fmt.Sprintf("confident %d, thin %d, mixed %d, contradicted %d, blocked %d",
		m[placement.Confident], m[placement.Thin], m[placement.Mixed], m[placement.Contradicted], m[placement.DomainBlocked])
}
