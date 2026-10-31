// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/dkimverify"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin auth-report` — what SPF/DKIM/DMARC actually say across this
// mailbox (#55 increment 0).
//
// WHY IT EXISTS, AND WHY IT COMES BEFORE ANY BADGE. #55 wants a sender-
// authentication display. The 2026-08-05 audit called it the cheapest item on
// that list because `Authentication-Results` is already parsed — true, and not
// enough. What is PERSISTED is one conservative boolean, `app.messages.auth_fail`,
// and on a real mailbox that boolean is false for every row.
//
// ⚠ THAT ZERO CANNOT BE READ ON ITS OWN. "This corpus has no authentication
// failures" and "the detector never fires" produce exactly the same number, and
// this project has already shipped a report whose expected output was zero from a
// filter that made zero the only possible answer (#242's "0 reached"). #47's
// increment 0 measured zero across a real corpus and was worth reading only
// because the detector had positive controls first. So does this: see
// TestAuthResultsOfFiresOnEveryMethodAndResult in internal/ml.
//
// ⚠ IT MEASURES THE SHIPPED PARSER. The counts come from ml.AuthResultsOf, the
// same code path `ml.AuthResultsFail` is now defined in terms of and the same one
// ingest uses to fill auth_fail. A second regex in this file would be a
// measurement of code written to agree with the same assumptions (#169, #220).
//
// ⚠ PER METHOD AND PER RESULT KEYWORD, NEVER ONE TOTAL. The stored boolean
// collapses three methods and eight result keywords into one bit, and the whole
// question a badge has to answer is which of them applied. Three populations it
// cannot show at all, each counted separately here:
//
//   - ABSENT — no method token anywhere. Treated as "not a failure" by design,
//     and the case a green "verified" badge would most dangerously misrepresent.
//   - DISAGREEING — a method carrying both a pass and a fail, which is ordinary
//     for multi-signature DKIM and is not a failure.
//   - the soft results (none / neutral / softfail / permerror / temperror),
//     which are neither pass nor fail and must not be folded into either.
//
// READ-ONLY. There is no --apply: it writes nothing and proposes nothing.
func runAuthReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose corpus to measure (required)")
	batch := fs.Int("batch", 500, "messages per page of the keyset walk")
	examples := fs.Int("examples", 3, "show up to this many example senders per unusual outcome")
	authserv := fs.Bool("authserv", false, "instead: which authserv-ids wrote the headers, which are trusted, by received month (v1.90.1)")
	noAR := fs.Bool("no-ar", false, "with --authserv: break down inbound mail with no Authentication-Results against a control group (v1.90.5, #776)")
	dkimOn := fs.Bool("dkim", false, "with --authserv --no-ar: verify DKIM locally on the NOAR mail and a calibration sample of CONTROL (v1.90.6, #776)")
	dkimSample := fs.Int("dkim-sample", 20, "with --dkim: verify every Nth CONTROL message as the calibration sample")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *noAR && !*authserv {
		return fmt.Errorf("auth-report: --no-ar only applies with --authserv")
	}
	if *dkimOn && !*noAR {
		return fmt.Errorf("auth-report: --dkim only applies with --no-ar")
	}
	if *dkimOn && *dkimSample < 1 {
		return fmt.Errorf("auth-report: --dkim-sample must be at least 1")
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
		return fmt.Errorf("auth-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("auth-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	if *authserv {
		trusted, err := trustedAuthservEntries()
		if err != nil {
			return err
		}
		var dk *dkimConfig
		if *dkimOn {
			dk = &dkimConfig{verifier: dkimverify.New(dkimverify.Options{}), sample: *dkimSample}
		}
		av, err := scanCorpusAuthserv(ctx, d, blobs, userID, dek, *batch, trusted, *noAR, *examples, dk)
		if err != nil {
			return err
		}
		av.print()
		return nil
	}

	agg, err := scanCorpusAuth(ctx, d, blobs, userID, dek, *batch, *examples)
	if err != nil {
		return err
	}
	agg.print(*examples)
	return nil
}

// authAggregate is the corpus tally. Every field is a count of a DISTINCT
// outcome; none is derived by subtracting two others, because a population
// obtained by subtraction is one nobody measured.
type authAggregate struct {
	scanned    int
	unreadable int // blob or MIME failure — NEVER counted as clean

	// present/absent partition `scanned - unreadable` exactly.
	present int
	absent  int
	// absentLocal is the subset of `absent` Mailroom itself wrote (a draft or a
	// Sent copy). Locally authored mail never crosses an authenticating hop, so
	// it has no Authentication-Results BY CONSTRUCTION — it is not unverified
	// mail, it is our own. Counted apart because a badge that flags it would be
	// actively wrong, and because the alternative is inferring the split from a
	// handful of example addresses.
	absentLocal  int
	absentRemote int

	// perMethod[method][result] = messages in which that method carried that
	// result at least once. A message can contribute to several results for one
	// method (multi-signature DKIM), so these do not sum to `present`.
	perMethod map[string]map[string]int
	// methodSeen[method] = messages carrying any result for that method.
	methodSeen map[string]int
	// disagree[method] = messages where the method held both a pass and a fail.
	disagree map[string]int
	// failed[method] = messages where the method failed by the parser's rule.
	failed map[string]int

	// clearlyFailed is ml.AuthResultsFail's verdict, recomputed here from the
	// same parse so it can be read AGAINST the per-method rows above rather than
	// on its own.
	clearlyFailed int
	// storedAuthFail is what app.messages.auth_fail actually holds. Kept
	// separate on purpose: a divergence between it and clearlyFailed means the
	// persisted signal and the live parse disagree, which is a finding.
	storedAuthFail int
	// disagreeStored counts messages where the two differ, so that finding has a
	// number rather than being inferred from two totals.
	disagreeStored int

	// exampleAbsent / exampleFailed carry a few real senders, because "17
	// messages have no auth header" is not actionable and "17, and here are
	// three of them" is.
	exampleAbsent []string
	exampleFailed []string
}

func newAuthAggregate() *authAggregate {
	a := &authAggregate{
		perMethod:  map[string]map[string]int{},
		methodSeen: map[string]int{},
		disagree:   map[string]int{},
		failed:     map[string]int{},
	}
	for _, m := range ml.AuthMethods {
		a.perMethod[m] = map[string]int{}
	}
	return a
}

func scanCorpusAuth(
	ctx context.Context,
	d *deps,
	blobs blob.BlobStore,
	userID ulid.ULID,
	dek []byte,
	batch, examples int,
) (*authAggregate, error) {
	agg := newAuthAggregate()
	after := ulid.ULID{}

	for {
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("auth-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++
			if ref.StoredAuthFail {
				agg.storedAuthFail++
			}

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				agg.unreadable++
				d.log.Warn("auth-report: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil {
				agg.unreadable++
				d.log.Warn("auth-report: parse failed", "message", ref.ID, "err", err)
				continue
			}

			// THE SHIPPED PARSER, on the same header ingest reads.
			res := ml.AuthResultsOf(msg.Header.Values("Authentication-Results"))
			agg.add(res, ref, examples)
		}
		if len(page) < batch {
			break
		}
	}
	return agg, nil
}

func (a *authAggregate) add(res ml.AuthResults, ref store.MessageAuthRef, examples int) {
	if !res.Present {
		a.absent++
		if ref.LocallyAuthored {
			a.absentLocal++
		} else {
			a.absentRemote++
		}
		if !ref.LocallyAuthored && len(a.exampleAbsent) < examples && ref.FromAddress != "" {
			a.exampleAbsent = append(a.exampleAbsent, ref.FromAddress)
		}
	} else {
		a.present++
	}
	for _, m := range ml.AuthMethods {
		if len(res.Results[m]) > 0 {
			a.methodSeen[m]++
		}
		for result := range res.Results[m] {
			a.perMethod[m][result]++
		}
		if res.Disagrees(m) {
			a.disagree[m]++
		}
		if res.Failed(m) {
			a.failed[m]++
		}
	}
	if res.ClearlyFailed() {
		a.clearlyFailed++
		if len(a.exampleFailed) < examples && ref.FromAddress != "" {
			a.exampleFailed = append(a.exampleFailed, ref.FromAddress)
		}
	}
	if res.ClearlyFailed() != ref.StoredAuthFail {
		a.disagreeStored++
	}
}

// reportOut is where print writes. A package variable so a test can capture the
// OUTPUT rather than the counters behind it — see authreport_test.go for why
// that distinction earned its own seam.
var reportOut io.Writer = os.Stdout

// rp / rpln write one line of the report. They exist so the printing code reads
// like printing code: writing to an io.Writer returns an error that stdout's
// fmt.Printf does not, and a report has nowhere useful to send a failed write to.
func rp(format string, args ...any) { _, _ = fmt.Fprintf(reportOut, format, args...) }
func rpln(args ...any)              { _, _ = fmt.Fprintln(reportOut, args...) }

func (a *authAggregate) print(examples int) {
	rp("scanned                     : %d messages\n", a.scanned)
	rp("unreadable                  : %d  (blob or MIME failure — NOT counted as clean)\n", a.unreadable)
	rp("with an Authentication-Results method token : %d\n", a.present)
	rp("with NONE at all                            : %d\n", a.absent)
	// ⚠ The split that decides what a badge may say. Locally authored mail has
	// no Authentication-Results by construction — flagging it as unverified
	// would be wrong, not conservative.
	rp("  of which Mailroom itself wrote (drafts, Sent) : %d   ← not unverified mail\n",
		a.absentLocal)
	rp("  of which arrived from outside                 : %d   ← the real ABSENT population\n",
		a.absentRemote)
	rpln()

	readable := a.scanned - a.unreadable
	if readable == 0 {
		rpln("no readable messages — every zero below would be that, and nothing else")
		return
	}
	if a.present == 0 {
		// The bound reported with the zero. A per-method table of zeros over a
		// corpus with no auth headers at all says nothing about the parser.
		rp("⚠ NOT ONE of %d readable messages carried an spf/dkim/dmarc token.\n", readable)
		rpln("  Every per-method count below is therefore zero because there was nothing")
		rpln("  to parse — NOT because the methods passed. A badge built on this would be")
		rpln("  blank on every message in this mailbox.")
		rpln()
	}

	rpln("=== per method, per result keyword (messages, not headers) ===")
	rpln("  a message can hold several results for one method, so rows do not sum")
	rp("  %-8s %8s  %s\n", "METHOD", "SEEN", "RESULTS")
	for _, m := range ml.AuthMethods {
		rp("  %-8s %8d  ", m, a.methodSeen[m])
		if len(a.perMethod[m]) == 0 {
			rpln("(none)")
			continue
		}
		keys := make([]string, 0, len(a.perMethod[m]))
		for k := range a.perMethod[m] {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if a.perMethod[m][keys[i]] != a.perMethod[m][keys[j]] {
				return a.perMethod[m][keys[i]] > a.perMethod[m][keys[j]]
			}
			return keys[i] < keys[j]
		})
		for i, k := range keys {
			if i > 0 {
				rp("  ")
			}
			rp("%s=%d", k, a.perMethod[m][k])
		}
		rpln()
	}
	rpln()

	rpln("=== the populations the stored boolean cannot show ===")
	for _, m := range ml.AuthMethods {
		rp("  %-8s failed (fail and no pass) : %6d    disagreed (both) : %6d\n",
			m, a.failed[m], a.disagree[m])
	}
	rpln()

	rpln("=== the verdict, beside what it was computed from ===")
	rp("  clearly failed (live parse)   : %d\n", a.clearlyFailed)
	rp("  app.messages.auth_fail = true : %d\n", a.storedAuthFail)
	if a.disagreeStored > 0 {
		// Not a statistic. The persisted signal and the live parse read the same
		// header; a difference means one of them is wrong.
		rp("  ⚠ DISAGREEMENTS               : %d — the stored signal and a live parse of\n",
			a.disagreeStored)
		rpln("     the same header differ. One of them is wrong; read those messages by hand.")
	} else {
		rpln("  stored and live agree on every message.")
	}

	if len(a.exampleAbsent) > 0 {
		rpln()
		rp("  example senders with no auth header (up to %d):\n", examples)
		for _, s := range a.exampleAbsent {
			rp("    %s\n", s)
		}
	}
	if len(a.exampleFailed) > 0 {
		rpln()
		rp("  example senders that clearly failed (up to %d):\n", examples)
		for _, s := range a.exampleFailed {
			rp("    %s\n", s)
		}
	}
}
