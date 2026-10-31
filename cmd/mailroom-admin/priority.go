// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"mailroom/internal/ml"
)

// The priority-model verbs (#842): a read-only report, the reply backfill and
// the re-score. All three are report-first; the two that write do so only under
// --apply. Output is COUNTS ONLY — no addresses, subjects or ids — so a report
// can be pasted into an issue as it stands.

// openPriorityTrainer builds the registry-backed trainer the three verbs share.
func openPriorityTrainer(ctx context.Context) (*deps, *ml.Trainer, *ml.Registry, error) {
	d, err := openDeps(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		d.close()
		return nil, nil, nil, err
	}
	reg := ml.NewRegistry(d.store, blobs, d.keys)
	return d, ml.NewTrainer(d.store, reg), reg, nil
}

// runPriorityReport prints how the user's engaged mail ranks under the cold
// start and the trained model(s), the signal census behind it, and the verdict
// the promotion gate would reach today. Writes nothing.
func runPriorityReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("priority-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		model     = fs.Int("model", 0, "also score this stored priority version (default: the cold start and the active model only)")
		sinceDays = fs.Int("since", 0, "only feedback from the last N days (default 0: all time)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	d, trainer, _, err := openPriorityTrainer(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var since time.Time
	if *sinceDays > 0 {
		since = time.Now().UTC().Add(-time.Duration(*sinceDays) * 24 * time.Hour)
	}
	r, err := trainer.BuildPriorityReport(ctx, userID, since, *model)
	if err != nil {
		return err
	}
	printPriorityReport(r)
	return nil
}

func printPriorityReport(r ml.PriorityReport) {
	window := "all time"
	if !r.Since.IsZero() {
		window = "since " + r.Since.Format("2006-01-02")
	}
	fmt.Printf("priority-report (read-only), feedback %s, threshold %.2f\n\n", window, ml.PriorityGateThreshold)

	fmt.Println("feedback events by action (as recorded):")
	printPriorityCounts(r.SignalCounts)

	c := r.Replies
	fmt.Printf("\nreplies from any client (derived from the user's own sent mail):\n")
	fmt.Printf("  own messages with threading headers  %6d (verifiably sent: in Sent, or sent through Mailroom)\n", c.OwnThreaded)
	fmt.Printf("  From the user but NOT verifiably sent %5d (excluded: a From header is sender-controlled)\n", c.UnverifiedOwn)
	printPriorityCounts(c.ByResolution)
	fmt.Printf("  distinct inbound originals replied to %5d\n", c.Originals)
	fmt.Printf("    with a recorded reply event        %6d\n", c.Recorded)
	fmt.Printf("    replied to but NEVER recorded      %6d\n", c.Unrecorded)
	fmt.Printf("  sent by an AI tool key, not counted  %6d (a key's replies teach nothing)\n", c.KeySent)
	fmt.Printf("  sent through Mailroom (submissions)  %6d; naming a stored original %d; of those recorded %d\n",
		c.ViaMailroom, c.ViaMailroomResolved, c.ViaMailroomRecorded)

	fmt.Printf("\nlabelled inbound messages (one per message; engagement wins a conflict):\n")
	fmt.Printf("  positives %d, negatives %d\n", r.Positives, r.Negatives)
	fmt.Println("  by labelling source (a message can carry several):")
	printPriorityCounts(r.BySource)
	fmt.Printf("  conflicted (positive and negative signals) %d\n", r.Corpus.Conflicted)
	fmt.Printf("  skipped: not readable %d, the user's own mail %d\n", r.Corpus.Skipped, r.Corpus.OwnSkipped)
	fmt.Printf("  leak control, features rebuilt as of arrival: flagged:1 removed %d, correspondence reduced %d "+
		"(positives with NO correspondence at arrival: %d)\n", r.Corpus.FlagMasked, r.Corpus.CorrMasked, r.Corpus.CorrOnlyFromReplies)
	fmt.Printf("  mark_read_no_open (retired): %d messages carry one; %d of them are engaged positives, %d labelled negatives\n",
		r.MarkReadTotal, r.MarkReadOnPos, r.MarkReadOnNeg)

	fmt.Printf("\nmodels over those messages (active version: %d; 0 = cold start):\n", r.ActiveVersion)
	for _, m := range r.Models {
		fmt.Printf("\n  %s", m.Name)
		if !m.CreatedAt.IsZero() {
			fmt.Printf("  (snapshot %s)", m.CreatedAt.UTC().Format("2006-01-02 15:04"))
		}
		fmt.Println()
		printEval("as of arrival (honest)", m.AsOf)
		printEval("today's features      ", m.Now)
		if m.OutOfSample != nil {
			fmt.Printf("    in-sample examples (labelled before the snapshot) %d\n", m.InSample)
			printEval("out of sample only    ", *m.OutOfSample)
		}
		if t := m.TrainedOn; t != nil {
			fmt.Printf("    what it was fitted on (pre-#842 pass, reconstructed; label-level upper bound):\n")
			fmt.Printf("      window events %d, labelled samples %d (mark_read_no_open %d)\n", t.WindowEvents, t.Samples, t.MarkReadNoOpen)
			fmt.Printf("      train split: %d positives / %d negatives; holdout: %d / %d\n",
				t.TrainPositives, t.TrainNegatives, t.HoldPositives, t.HoldNegatives)
			if t.HoldoutDegenerate {
				fmt.Printf("      holdout lacked a class: AUC was 0.5 for both sides by convention, and 0.5 >= 0.5 promoted it\n")
			}
		}
		printHist(m.AsOf)
	}

	g := r.Gate
	fmt.Printf("\npromotion gate, as the daily pass would run it now (last 90 days, recorded events only):\n")
	if r.GateNote != "" {
		fmt.Printf("  %s\n", r.GateNote)
	}
	fmt.Printf("  evidence: positives %d (need %d), negatives %d, positive sender domains %d (need %d), usable folds %d/%d\n",
		g.Positives, g.MinPositives, g.Negatives, g.PositiveSenders, g.MinSenders, g.UsableFolds, g.Folds)
	fmt.Printf("  candidate (out of fold): recall %d/%d (LB %s), precision %d/%d (LB %s), AUC %s\n",
		g.Cand.AppliedPositives, g.Cand.Positives, f3(g.CandRecallLCB), g.Cand.AppliedPositives, g.Cand.Applied, f3(g.CandPrecisionLCB), f3(g.CandAUC))
	fmt.Printf("  cold start:              recall %d/%d (LB %s), precision %d/%d (LB %s), AUC %s\n",
		g.Cold.AppliedPositives, g.Cold.Positives, f3(g.ColdRecallLCB), g.Cold.AppliedPositives, g.Cold.Applied, f3(g.ColdPrecisionLCB), f3(g.ColdAUC))
	fmt.Printf("  disagreements on engaged mail: candidate %d, cold start %d, paired LB %s\n", g.Wins, g.Losses, f3(g.PairedLCB))
	verdict := "REFUSE"
	if g.Pass {
		verdict = "PASS"
	}
	fmt.Printf("  verdict: %s — %s\n", verdict, g.Reason)

	fmt.Printf("\nserved scores, mail received in the last 30 days, by model:\n")
	for _, s := range r.Stored {
		fmt.Printf("  %-14s messages %6d  >=0.6 %5d  >=0.4 %5d  max %.3f  last scored %s\n",
			s.ModelVersion, s.Messages, s.Important, s.AtLeast04, s.MaxScore, s.LastScored.UTC().Format("2006-01-02"))
	}
	if len(r.Decisions) > 0 {
		fmt.Printf("\nrecent gate decisions:\n")
		for _, dcs := range r.Decisions {
			fmt.Printf("  %s  %s\n", dcs.At.UTC().Format("2006-01-02 15:04"), dcs.Action)
		}
	}
}

func printEval(label string, s ml.EvalStats) {
	fmt.Printf("    %s  n=%d pos=%d  precision %d/%d=%s (LB %s)  recall %d/%d=%s (LB %s)  AUC %s  max %.3f\n",
		label, s.N, s.Positives, s.AppliedPositives, s.Applied, f3(s.Precision), f3(s.PrecisionLCB),
		s.AppliedPositives, s.Positives, f3(s.Recall), f3(s.RecallLCB), f3(s.AUC), s.Max)
}

func printHist(s ml.EvalStats) {
	fmt.Printf("    score distribution (as of arrival): bucket  all  engaged\n")
	for i := range s.Hist {
		fmt.Printf("      %.1f-%.1f  %6d  %6d\n", float64(i)/10, float64(i+1)/10, s.Hist[i], s.HistPositive[i])
	}
}

func printPriorityCounts(m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Println("  (none)")
	}
	for _, k := range keys {
		fmt.Printf("  %-36s %6d\n", k, m[k])
	}
}

// f3 prints a proportion, or n/a for NaN (no evidence — not zero).
func f3(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", v)
}

// runPriorityReplyBackfill records the `reply` signal for replies the user made
// from ANY client, derived from their own sent mail. Report-first: without
// --apply it prints the census and writes nothing. Idempotent: once per
// original, so a rerun writes zero rows.
func runPriorityReplyBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("priority-reply-backfill", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the missing reply events (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	d, trainer, _, err := openPriorityTrainer(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	c, err := trainer.CensusReplies(ctx, userID)
	if err != nil {
		return err
	}
	mode := "report only (no writes)"
	if *apply {
		mode = "apply"
	}
	fmt.Printf("priority-reply-backfill [%s]\n", mode)
	fmt.Printf("  own messages with threading headers %d (verifiably sent); From the user but not verifiably sent, excluded %d\n",
		c.OwnThreaded, c.UnverifiedOwn)
	printPriorityCounts(c.ByResolution)
	fmt.Printf("  distinct originals replied to %d: recorded %d, would record %d\n", c.Originals, c.Recorded, c.Unrecorded)
	fmt.Printf("  sent by an AI tool key, not counted %d\n", c.KeySent)
	if !*apply {
		return nil
	}
	n, err := trainer.BackfillReplies(ctx, userID, c)
	if err != nil {
		return err
	}
	fmt.Printf("  wrote %d reply events\n", n)
	if n != c.Unrecorded {
		fmt.Printf("  NOTE: wrote %d, the report predicted %d — something recorded replies concurrently\n", n, c.Unrecorded)
	}
	return nil
}

// runPriorityTrain runs the daily priority pass NOW, so a release can apply
// the gate before re-scoring instead of waiting up to a day for the scheduled
// pass. Report-first: without --apply it prints the verdict the pass would
// reach and what it would do (promote, refuse, or revert the active model) and
// writes nothing; --apply runs TrainAndMaybePromote itself, which also writes
// the audit row.
func runPriorityTrain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("priority-train", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "run the pass: promote, or revert to the cold start (default: report only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	d, trainer, reg, err := openPriorityTrainer(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	if !*apply {
		r, err := trainer.BuildPriorityReport(ctx, userID, time.Time{}, 0)
		if err != nil {
			return err
		}
		g := r.Gate
		action := "keep the cold start"
		switch {
		case r.GateNote == "" && g.Pass:
			action = "promote a new version"
		case r.ActiveVersion > 0:
			action = fmt.Sprintf("REVERT priority:%d to the cold start", r.ActiveVersion)
		}
		fmt.Printf("priority-train [report only]: active priority:%d\n", r.ActiveVersion)
		if r.GateNote != "" {
			fmt.Printf("  %s\n", r.GateNote)
		}
		fmt.Printf("  gate: pass=%v — %s\n  the pass would: %s\n", g.Pass && r.GateNote == "", g.Reason, action)
		return nil
	}
	res, err := trainer.TrainAndMaybePromote(ctx, userID)
	if err != nil {
		return err
	}
	active, err := reg.ActiveVersion(ctx, userID, ml.KindPriority)
	if err != nil {
		return err
	}
	fmt.Printf("priority-train [apply]: samples %d (positives %d, negatives %d) trained=%v promoted=%v reverted=%v\n",
		res.Samples, res.Positives, res.Negatives, res.Trained, res.Promoted, res.Reverted)
	fmt.Printf("  %s\n  active now: priority:%d\n", res.Reason, active)
	return nil
}

// runPriorityRescore re-scores recent mail with the model in service.
// Report-first: the dry run prints how many messages would change band and how
// many would become important; --apply writes exactly the rows the dry run
// counted as changed (one Email change row each). Idempotent.
func runPriorityRescore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("priority-rescore", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		sinceDays = fs.Int("since", 30, "re-score mail received in the last N days")
		apply     = fs.Bool("apply", false, "write the new scores (default: dry run)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *sinceDays <= 0 {
		return fmt.Errorf("--since must be a positive number of days")
	}
	d, _, reg, err := openPriorityTrainer(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	since := time.Now().UTC().Add(-time.Duration(*sinceDays) * 24 * time.Hour)
	res, err := ml.RescorePriority(ctx, d.store, reg, userID, since, *apply)
	if err != nil {
		return err
	}
	mode := "dry run (no writes)"
	if *apply {
		mode = "apply"
	}
	fmt.Printf("priority-rescore [%s]: mail received since %s, scored with %s\n", mode, since.Format("2006-01-02"), res.Model)
	fmt.Printf("  scanned %d (unreadable %d); rows that differ %d\n", res.Scanned, res.Missing, res.Changed)
	fmt.Printf("  at %.2f: become important %d, stop being important %d, stay important %d, stay not %d\n",
		ml.PriorityGateThreshold, res.BecomeImportant, res.StopImportant, res.StayImportant, res.StayNot)
	fmt.Println("  previously scored by:")
	printPriorityCounts(res.PriorByModel)
	fmt.Println("  distribution: bucket  before  after")
	for i := range res.Before {
		fmt.Printf("    %.1f-%.1f  %6d  %6d  %s\n", float64(i)/10, float64(i+1)/10, res.Before[i], res.After[i],
			strings.Repeat("#", res.After[i]/10))
	}
	if *apply {
		fmt.Printf("  wrote %d rows\n", res.Written)
		if res.Written != res.Changed {
			return fmt.Errorf("wrote %d rows but the comparison counted %d changed — investigate before trusting this run", res.Written, res.Changed)
		}
	}
	return nil
}
