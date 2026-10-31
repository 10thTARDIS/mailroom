// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// The read-only priority report (#842): what engagement the mailbox actually
// holds, how the cold start and the trained model rank it at the line the user
// sees, and what the trained model was really fitted on. Nothing here writes.
//
// It was built BEFORE the gate, on CLAUDE.md's rule that a measurement is a
// claim about the pipeline that produced it: every number the old trainer
// promoted on came from a pipeline nobody had audited, and the audit is what
// this report prints.

// ReplyCensus counts the user's own threaded messages by how their inbound
// original resolved, per case, and how many resolved originals already carry a
// recorded `reply` event.
type ReplyCensus struct {
	OwnThreaded int
	// UnverifiedOwn counts threaded messages whose From names the user but that
	// were never filed as sent (no Sent provenance, no submission): excluded,
	// because a From header is sender-controlled.
	UnverifiedOwn int
	ByResolution  map[string]int
	// Originals is the number of DISTINCT inbound messages replied to.
	Originals  int
	Recorded   int // originals that already have a reply event
	Unrecorded int // originals the user replied to with no reply event: the missing positives
	// Sends made THROUGH MAILROOM (a 'sent' submission), resolved by the same
	// rule the EmailSubmission/set seam uses, whatever the row's draft flag:
	// how many there are, how many name a stored inbound original, and how many
	// of those originals carry a reply event. A gap between the last two is the
	// submission seam failing, as opposed to replies made in other clients.
	ViaMailroom         int
	ViaMailroomResolved int
	ViaMailroomRecorded int
	// KeySent counts sent messages written or submitted by an AI-tool API key
	// (#867): not counted as replies, excluded from the backfill, so they
	// are not "Unrecorded" forever.
	KeySent int
	// originals maps each resolved original to its earliest reply's time, and
	// pairs lists (sent, original) for the backfill.
	originals map[ulid.ULID]time.Time
	pairs     [][2]ulid.ULID
}

// replyPage is how many own messages one resolution transaction handles.
const replyPage = 200

// CensusReplies resolves every own threaded message to its inbound original.
// Read-only.
func (t *Trainer) CensusReplies(ctx context.Context, userID ulid.ULID) (ReplyCensus, error) {
	c := ReplyCensus{ByResolution: map[string]int{}, originals: map[ulid.ULID]time.Time{}}
	var own []ulid.ULID
	keySeen := map[ulid.ULID]bool{}
	recorded := map[ulid.ULID]bool{}
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if own, c.UnverifiedOwn, e = tx.OwnThreadedMessages(ctx); e != nil {
			return e
		}
		events, e := tx.ListFeedbackEvents(ctx, time.Time{}, farFuture)
		if e != nil {
			return e
		}
		for _, ev := range events {
			if ev.Action == store.FeedbackActionReply && ev.MessageID != nil {
				recorded[*ev.MessageID] = true
			}
		}
		return nil
	}); err != nil {
		return c, fmt.Errorf("ml: reply census: %w", err)
	}
	c.OwnThreaded = len(own)
	for start := 0; start < len(own); start += replyPage {
		end := min(start+replyPage, len(own))
		if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			for _, sent := range own[start:end] {
				orig, res, err := tx.ReplyTarget(ctx, sent)
				if err != nil {
					return err
				}
				c.ByResolution[res.String()]++
				if res != store.ReplyResolved {
					continue
				}
				if byKey, err := tx.SentByAPIKey(ctx, sent); err != nil {
					return err
				} else if byKey {
					c.KeySent++
					keySeen[sent] = true
					continue
				}
				at, _, err := tx.MessageReceivedAt(ctx, sent)
				if err != nil {
					return err
				}
				if old, ok := c.originals[orig]; !ok || at.Before(old) {
					c.originals[orig] = at
				}
				c.pairs = append(c.pairs, [2]ulid.ULID{sent, orig})
			}
			return nil
		}); err != nil {
			return c, fmt.Errorf("ml: reply census: resolve: %w", err)
		}
	}
	var submitted []ulid.ULID
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		submitted, e = tx.SentSubmissionMessages(ctx)
		return e
	}); err != nil {
		return c, fmt.Errorf("ml: reply census: submissions: %w", err)
	}
	seenSent := map[ulid.ULID]bool{}
	for _, p := range c.pairs {
		seenSent[p[0]] = true
	}
	c.ViaMailroom = len(submitted)
	for start := 0; start < len(submitted); start += replyPage {
		end := min(start+replyPage, len(submitted))
		if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			for _, sent := range submitted[start:end] {
				orig, ok, err := tx.OriginalMessageForSend(ctx, sent)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				// Key-sent messages are not the seam's population (#867).
				if keySeen[sent] {
					continue
				}
				if byKey, err := tx.SentByAPIKey(ctx, sent); err != nil {
					return err
				} else if byKey {
					c.KeySent++
					keySeen[sent] = true
					continue
				}
				c.ViaMailroomResolved++
				if recorded[orig] {
					c.ViaMailroomRecorded++
				}
				if seenSent[sent] {
					continue // already counted through the own-mail census
				}
				at, _, err := tx.MessageReceivedAt(ctx, sent)
				if err != nil {
					return err
				}
				if old, ok := c.originals[orig]; !ok || at.Before(old) {
					c.originals[orig] = at
				}
				c.pairs = append(c.pairs, [2]ulid.ULID{sent, orig})
			}
			return nil
		}); err != nil {
			return c, fmt.Errorf("ml: reply census: resolve submissions: %w", err)
		}
	}

	c.Originals = len(c.originals)
	for orig := range c.originals {
		if recorded[orig] {
			c.Recorded++
		} else {
			c.Unrecorded++
		}
	}
	return c, nil
}

// farFuture bounds "all time" queries over ListFeedbackEvents' half-open window.
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// BackfillReplies records the reply signal for every resolved (sent, original)
// pair in the census, once per original. It returns how many rows it inserted;
// a rerun inserts none. Callers run CensusReplies first and print it, and call
// this only under --apply.
func (t *Trainer) BackfillReplies(ctx context.Context, userID ulid.ULID, c ReplyCensus) (int, error) {
	inserted := 0
	for start := 0; start < len(c.pairs); start += replyPage {
		end := min(start+replyPage, len(c.pairs))
		if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			for _, p := range c.pairs[start:end] {
				ok, err := tx.RecordReplyFeedback(ctx, p[1], p[0], "backfill")
				if err != nil {
					return err
				}
				if ok {
					inserted++
				}
			}
			return nil
		}); err != nil {
			return inserted, fmt.Errorf("ml: reply backfill: %w", err)
		}
	}
	return inserted, nil
}

// EvalStats is one model's decision stats over a set of labelled examples at
// PriorityGateThreshold, with Wilson 95% lower bounds.
type EvalStats struct {
	N, Positives              int
	Applied, AppliedPositives int
	Precision, PrecisionLCB   float64
	Recall, RecallLCB         float64
	AUC                       float64
	// Hist / HistPositive bucket the scores in tenths (0.0-0.1 … 0.9-1.0).
	Hist         [10]int
	HistPositive [10]int
	Max          float64
}

func evalStats(scores []float64, positive []bool) EvalStats {
	labels := make([]float64, len(positive))
	for i, p := range positive {
		if p {
			labels[i] = 1
		}
	}
	d := bandStats(scores, labels)
	s := EvalStats{
		N: d.Scored, Positives: d.Positives, Applied: d.Applied, AppliedPositives: d.AppliedPositives,
		Precision: d.Precision, Recall: d.Coverage,
		PrecisionLCB: wilsonLowerBound(d.AppliedPositives, d.Applied, priorityGateZ),
		RecallLCB:    wilsonLowerBound(d.AppliedPositives, d.Positives, priorityGateZ),
		AUC:          math.NaN(),
	}
	var hasPos, hasNeg bool
	for i, sc := range scores {
		b := int(sc * 10)
		b = max(0, min(9, b))
		s.Hist[b]++
		if positive[i] {
			s.HistPositive[b]++
			hasPos = true
		} else {
			hasNeg = true
		}
		s.Max = math.Max(s.Max, sc)
	}
	if hasPos && hasNeg {
		s.AUC = auc(scores, labels)
	}
	return s
}

// LegacyTraining reconstructs what a stored version was fitted on under the
// PRE-#842 pass: the 90-day window of feedback EVENTS before its snapshot, the
// legacy label map (mark_read_no_open a negative), and the 80/20 time split.
// Counts are at the label level — an upper bound on what was fitted, since the
// old pass also skipped events whose message could not be extracted.
type LegacyTraining struct {
	WindowEvents   int
	Samples        int
	TrainPositives int
	TrainNegatives int
	HoldPositives  int
	HoldNegatives  int
	MarkReadNoOpen int
	// HoldoutDegenerate is true when the holdout lacked a class, in which case
	// the old AUC comparison scored 0.5 for BOTH sides and "0.5 >= 0.5"
	// promoted the candidate without measuring anything.
	HoldoutDegenerate bool
}

// ModelReport is one model scored over the report's examples.
type ModelReport struct {
	Name      string
	Version   int
	CreatedAt time.Time
	// AsOf scores the features as they stood at arrival (the honest view, the
	// one the gate uses). Now scores today's features, which is what a re-score
	// would compute and where the leaked flag/correspondence features show.
	AsOf EvalStats
	Now  EvalStats
	// OutOfSample: trained versions only — the examples labelled AFTER the
	// snapshot, which it cannot have trained on. Nil for the cold start.
	OutOfSample *EvalStats
	InSample    int // examples labelled before the snapshot (possibly trained on)
	TrainedOn   *LegacyTraining
}

// PriorityReport is the full read-only report.
type PriorityReport struct {
	Since         time.Time // zero = all time
	SignalCounts  map[string]int
	Replies       ReplyCensus
	Corpus        PriorityCorpus
	Positives     int
	Negatives     int
	BySource      map[string]int // examples per labelling source (a message can count under several)
	MarkReadTotal int            // distinct messages with a mark_read_no_open event
	MarkReadOnPos int            // ...of which are engaged (positive) messages
	MarkReadOnNeg int            // ...of which are labelled negative
	ActiveVersion int
	Models        []ModelReport
	// Gate is the verdict the daily pass would reach today, on its own window
	// and its own evidence (recorded events only, no derived replies).
	Gate      PriorityGate
	GateNote  string
	Decisions []store.PriorityGateDecision
	Stored    []store.TriageScoreSummary
}

// BuildPriorityReport assembles the report. extraVersion (> 0) scores that
// stored version too, alongside the cold start and the active model. Read-only.
func (t *Trainer) BuildPriorityReport(ctx context.Context, userID ulid.ULID, since time.Time, extraVersion int) (PriorityReport, error) {
	r := PriorityReport{Since: since, BySource: map[string]int{}}
	now := t.now().UTC()

	var events []store.FeedbackEvent
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if r.SignalCounts, e = tx.FeedbackActionCounts(ctx, since); e != nil {
			return e
		}
		if events, e = tx.ListFeedbackEvents(ctx, since, farFuture); e != nil {
			return e
		}
		if r.Decisions, e = tx.PriorityGateDecisions(ctx, 10); e != nil {
			return e
		}
		r.Stored, e = tx.TriageScoreSummaryByModel(ctx, now.Add(-30*24*time.Hour))
		return e
	}); err != nil {
		return r, fmt.Errorf("ml: priority report: %w", err)
	}

	census, err := t.CensusReplies(ctx, userID)
	if err != nil {
		return r, err
	}
	r.Replies = census
	extra := map[ulid.ULID]time.Time{}
	for id, at := range census.originals {
		if !at.Before(since) {
			extra[id] = at
		}
	}
	corpus, err := t.loadPriorityCorpus(ctx, userID, corpusOptions{since: since, until: farFuture, extraReplies: extra})
	if err != nil {
		return r, err
	}
	r.Corpus = corpus
	posByID := map[ulid.ULID]bool{}
	for _, e := range corpus.Examples {
		posByID[e.MessageID] = e.Positive
		if e.Positive {
			r.Positives++
		} else {
			r.Negatives++
		}
		for _, src := range e.Sources {
			r.BySource[src]++
		}
	}
	markRead := map[ulid.ULID]bool{}
	for _, ev := range events {
		if ev.Action == legacyMarkReadNoOpen && ev.MessageID != nil {
			markRead[*ev.MessageID] = true
		}
	}
	r.MarkReadTotal = len(markRead)
	for id := range markRead {
		if p, ok := posByID[id]; ok {
			if p {
				r.MarkReadOnPos++
			} else {
				r.MarkReadOnNeg++
			}
		}
	}

	positive := make([]bool, len(corpus.Examples))
	for i, e := range corpus.Examples {
		positive[i] = e.Positive
	}
	scoreModel := func(name string, version int, m Model, created time.Time) ModelReport {
		mr := ModelReport{Name: name, Version: version, CreatedAt: created,
			AsOf: evalStats(scorePriority(m, corpus.Examples, false), positive),
			Now:  evalStats(scorePriority(m, corpus.Examples, true), positive),
		}
		if version > 0 {
			var oos []PriorityExample
			var oosPos []bool
			for _, e := range corpus.Examples {
				if e.At.After(created) {
					oos = append(oos, e)
					oosPos = append(oosPos, e.Positive)
				} else {
					mr.InSample++
				}
			}
			s := evalStats(scorePriority(m, oos, false), oosPos)
			mr.OutOfSample = &s
			lt := legacyTraining(events, created)
			mr.TrainedOn = &lt
		}
		return mr
	}

	r.Models = append(r.Models, scoreModel("cold start", 0, NewColdStart(), time.Time{}))
	if r.ActiveVersion, err = t.reg.ActiveVersion(ctx, userID, KindPriority); err != nil {
		return r, err
	}
	versions := []int{}
	if r.ActiveVersion > 0 {
		versions = append(versions, r.ActiveVersion)
	}
	if extraVersion > 0 && extraVersion != r.ActiveVersion {
		versions = append(versions, extraVersion)
	}
	for _, v := range versions {
		m, created, err := t.reg.LoadVersion(ctx, userID, KindPriority, v)
		if err != nil {
			return r, fmt.Errorf("ml: priority report: load v%d: %w", v, err)
		}
		name := fmt.Sprintf("priority:%d", v)
		if v == r.ActiveVersion {
			name += " (active)"
		}
		r.Models = append(r.Models, scoreModel(name, v, m, created))
	}

	// The gate, exactly as the daily pass would run it now.
	gateCorpus, err := t.loadPriorityCorpus(ctx, userID, corpusOptions{since: now.Add(-trainWindow), until: now})
	if err != nil {
		return r, err
	}
	var gp, gn int
	for _, e := range gateCorpus.Examples {
		if e.Positive {
			gp++
		} else {
			gn++
		}
	}
	r.Gate = EvaluatePriorityGate(gateCorpus, userID)
	if len(gateCorpus.Examples) < MinTrainSamples || gp == 0 || gn == 0 {
		r.GateNote = fmt.Sprintf("the daily pass would not fit at all (samples=%d, positives=%d, negatives=%d; need >=%d with both classes)",
			len(gateCorpus.Examples), gp, gn, MinTrainSamples)
	}
	return r, nil
}

// legacyTraining reconstructs the pre-#842 training set for a snapshot taken
// at created (see LegacyTraining).
func legacyTraining(events []store.FeedbackEvent, created time.Time) LegacyTraining {
	var lt LegacyTraining
	since := created.Add(-trainWindow)
	type sample struct{ pos bool }
	var samples []sample
	ordered := append([]store.FeedbackEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID.Compare(ordered[j].ID) < 0
	})
	for _, e := range ordered {
		if e.CreatedAt.Before(since) || !e.CreatedAt.Before(created) {
			continue
		}
		lt.WindowEvents++
		label, ok := LegacyLabelForAction(e.Action)
		if !ok || e.MessageID == nil || *e.MessageID == (ulid.ULID{}) {
			continue
		}
		if e.Action == legacyMarkReadNoOpen {
			lt.MarkReadNoOpen++
		}
		samples = append(samples, sample{pos: label >= 0.5})
	}
	lt.Samples = len(samples)
	if lt.Samples == 0 {
		lt.HoldoutDegenerate = true
		return lt
	}
	split := int(float64(len(samples)) * 0.8)
	split = max(1, min(split, len(samples)-1))
	for i, s := range samples {
		switch {
		case i < split && s.pos:
			lt.TrainPositives++
		case i < split:
			lt.TrainNegatives++
		case s.pos:
			lt.HoldPositives++
		default:
			lt.HoldNegatives++
		}
	}
	lt.HoldoutDegenerate = lt.HoldPositives == 0 || lt.HoldNegatives == 0
	return lt
}
