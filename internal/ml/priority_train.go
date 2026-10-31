// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// The priority model's corpus, fit and cross-validation (#842). The gate that
// consumes them is priority_gate.go; the daily pass is train.go.
//
// Four decisions here answer the audit in #842, and each is a change from the
// pass v1..v9 came out of:
//
//  1. ONE EXAMPLE PER MESSAGE, positive if ANY engagement signal touched it. The
//     old pass used one sample per EVENT, so a message read and then replied to
//     appeared as both a negative (mark_read_no_open) and a positive (reply) —
//     the model was asked to fit a contradiction, and the majority won.
//  2. CLASS BALANCE. The old pass ran plain SGD over thousands of negatives and a handful of
//     positives; the bias walked to the majority and every score sank below
//     0.25. The fit interleaves the minority class against the majority, as
//     TrainLabeler does.
//  3. FEATURES AS OF ARRIVAL. Two features were the label in disguise when
//     replayed today: flagged:1 is set ON the messages the user flagged (the
//     `flag` positive), and corr:/sent: count the very reply that made a message
//     positive. Serve-time scoring happens at ingest, before either is true, so
//     the corpus rebuilds both as they stood when the message arrived.
//  4. SENDER IDENTITY IS NOT A FEATURE (labelModelFeatures strips from:, dom:,
//     alias:). The gate cross-validates over sender domains, where identity
//     weights carry nothing to a held-out sender — except alias:, which crosses
//     domains and would leak (labeler.go). A weight the gate cannot measure is
//     a weight it cannot vouch for; the relationship itself is still carried by
//     corr:/sent:, which generalise.

// priorityTrainPasses is how many balanced passes the fit makes over the
// majority class.
const priorityTrainPasses = 8

// PriorityExample is one labelled message.
type PriorityExample struct {
	MessageID ulid.ULID
	Positive  bool
	// Sources are the actions (or "reply-derived") that labelled it, for the
	// per-source report. A message can carry several.
	Sources []string
	// Group is the sender domain — the unit the cross-validation holds out.
	Group string
	// AsOf are the features as they stood when the message arrived (the honest
	// view, used to fit and to judge); Now are the features as a re-score today
	// would compute them (reported alongside, so the leak is visible).
	AsOf FeatureVector
	Now  FeatureVector
	// At is when the example was labelled (the earliest labelling event).
	At time.Time
}

// PriorityCorpus is the labelled set plus the counts a report needs.
type PriorityCorpus struct {
	Examples []PriorityExample
	// Skipped counts messages with a label that could not be extracted
	// (purged, or not visible).
	Skipped int
	// OwnSkipped counts labelled messages the user sent themselves. Priority
	// scores inbound mail; a trashed copy of your own reply says nothing about
	// what you want to read.
	OwnSkipped int
	// Conflicted counts messages that carried both a positive and a negative
	// signal; they are labelled positive (engagement wins).
	Conflicted int
	// FlagMasked / CorrMasked count examples whose as-of-arrival features
	// differ from today's: flagged:1 removed because the user flagged it, and
	// the correspondence count reduced by replies sent after it arrived.
	FlagMasked int
	CorrMasked int
	// CorrOnlyFromReplies counts positives whose sender would have had NO
	// correspondence at arrival — corr:1 today exists only because of replies
	// made since.
	CorrOnlyFromReplies int
}

// corpusOptions shape loadPriorityCorpus.
type corpusOptions struct {
	since, until time.Time
	// extraReplies are originals known to have been replied to but possibly not
	// recorded as events (the report's derived replies), keyed to the reply's
	// time. Nil in the trainer, which learns only from recorded events.
	extraReplies map[ulid.ULID]time.Time
}

// loadPriorityCorpus builds per-message examples from the user's feedback in
// [since, until) under the CURRENT label map (mark_read_no_open unmapped).
func (t *Trainer) loadPriorityCorpus(ctx context.Context, userID ulid.ULID, opt corpusOptions) (PriorityCorpus, error) {
	var events []store.FeedbackEvent
	if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		events, e = tx.ListFeedbackEvents(ctx, opt.since, opt.until)
		return e
	}); err != nil {
		return PriorityCorpus{}, fmt.Errorf("ml: priority corpus: list feedback: %w", err)
	}

	type agg struct {
		pos, neg bool
		sources  []string
		at       time.Time
		flagged  bool
	}
	byMsg := map[ulid.ULID]*agg{}
	get := func(id ulid.ULID) *agg {
		a := byMsg[id]
		if a == nil {
			a = &agg{}
			byMsg[id] = a
		}
		return a
	}
	// replyTimes: per message, when it was replied to — feeds the as-of
	// correspondence correction for its sender.
	replyTimes := map[ulid.ULID]time.Time{}
	for _, e := range events {
		if e.MessageID == nil || *e.MessageID == (ulid.ULID{}) {
			continue
		}
		id := *e.MessageID
		if e.Action == store.FeedbackActionReply {
			if old, ok := replyTimes[id]; !ok || e.CreatedAt.Before(old) {
				replyTimes[id] = e.CreatedAt
			}
		}
		label, ok := LabelForAction(e.Action)
		if !ok {
			continue
		}
		a := get(id)
		if a.at.IsZero() || e.CreatedAt.Before(a.at) {
			a.at = e.CreatedAt
		}
		a.sources = appendUnique(a.sources, e.Action)
		if e.Action == "flag" {
			a.flagged = true
		}
		if label >= 0.5 {
			a.pos = true
		} else {
			a.neg = true
		}
	}
	for id, at := range opt.extraReplies {
		if old, ok := replyTimes[id]; !ok || at.Before(old) {
			replyTimes[id] = at
		}
		a := get(id)
		if a.at.IsZero() || at.Before(a.at) {
			a.at = at
		}
		a.sources = appendUnique(a.sources, "reply-derived")
		a.pos = true
	}

	ids := make([]ulid.ULID, 0, len(byMsg))
	for id := range byMsg {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })

	// Facts for every labelled message and every replied-to message (the
	// latter for the per-sender reply timeline), read in one pass.
	type row struct {
		facts MessageFacts
		from  string
		recv  time.Time
		own   bool
	}
	rows := map[ulid.ULID]row{}
	need := map[ulid.ULID]bool{}
	for _, id := range ids {
		need[id] = true
	}
	for id := range replyTimes {
		need[id] = true
	}
	needIDs := make([]ulid.ULID, 0, len(need))
	for id := range need {
		needIDs = append(needIDs, id)
	}
	sort.Slice(needIDs, func(i, j int) bool { return needIDs[i].Compare(needIDs[j]) < 0 })
	const page = 200
	for start := 0; start < len(needIDs); start += page {
		end := min(start+page, len(needIDs))
		if err := t.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			ownCache := map[string]bool{}
			for _, id := range needIDs[start:end] {
				r, found, err := tx.MessageMLFacts(ctx, id)
				if err != nil {
					return err
				}
				if !found {
					continue
				}
				out, in, err := tx.OutboundCountForAddress(ctx, r.FromAddress)
				if err != nil {
					return err
				}
				from := store.NormalizeAddress(r.FromAddress)
				own, cached := ownCache[from]
				if !cached {
					if own, err = tx.IsOwnAddress(ctx, from); err != nil {
						return err
					}
					ownCache[from] = own
				}
				rows[id] = row{facts: factsFromRow(r, out, in), from: from, recv: r.ReceivedAt, own: own}
			}
			return nil
		}); err != nil {
			return PriorityCorpus{}, fmt.Errorf("ml: priority corpus: extract: %w", err)
		}
	}

	// Per sender, the times the user replied to that sender's mail.
	repliesTo := map[string][]time.Time{}
	for id, at := range replyTimes {
		if r, ok := rows[id]; ok && r.from != "" {
			repliesTo[r.from] = append(repliesTo[r.from], at)
		}
	}

	var c PriorityCorpus
	for _, id := range ids {
		a := byMsg[id]
		r, ok := rows[id]
		if !ok {
			c.Skipped++
			continue
		}
		if r.own {
			c.OwnSkipped++
			continue
		}
		if a.pos && a.neg {
			c.Conflicted++
		}
		asOf := r.facts
		if a.flagged && asOf.RemoteFlagged {
			asOf.RemoteFlagged = false
			c.FlagMasked++
		}
		later := 0
		for _, at := range repliesTo[r.from] {
			if !at.Before(r.recv) {
				later++
			}
		}
		if later > 0 && asOf.OutboundCount > 0 {
			asOf.OutboundCount = max(0, asOf.OutboundCount-later)
			asOf.HasCorresponded = asOf.OutboundCount > 0
			c.CorrMasked++
			if a.pos && !asOf.HasCorresponded {
				c.CorrOnlyFromReplies++
			}
		}
		c.Examples = append(c.Examples, PriorityExample{
			MessageID: id,
			Positive:  a.pos,
			Sources:   a.sources,
			Group:     r.facts.SenderDomain,
			AsOf:      featuresFromFacts(asOf),
			Now:       featuresFromFacts(r.facts),
			At:        a.at,
		})
	}
	return c, nil
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// fitPriority trains a fresh priority model on examples (as-of features,
// identity stripped), interleaving the minority class against the majority so
// neither dominates the gradient. Deterministic: the shuffle is seeded from
// seed, and logisticModel.Score sums in sorted key order (#222).
func fitPriority(examples []PriorityExample, seed ulid.ULID) (Model, error) {
	var pos, neg []FeatureVector
	for _, e := range examples {
		fv := labelModelFeatures(e.AsOf)
		if e.Positive {
			pos = append(pos, fv)
		} else {
			neg = append(neg, fv)
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return nil, fmt.Errorf("ml: priority fit needs both classes (%d positive, %d negative)", len(pos), len(neg))
	}
	m := NewLogistic(KindPriority)
	rng := rand.New(rand.NewPCG(binary.BigEndian.Uint64(seed[:8]), binary.BigEndian.Uint64(seed[8:])))
	big, bigY, small, smallY := neg, 0.0, pos, 1.0
	if len(pos) > len(neg) {
		big, bigY, small, smallY = pos, 1.0, neg, 0.0
	}
	for p := 0; p < priorityTrainPasses; p++ {
		rng.Shuffle(len(big), func(i, j int) { big[i], big[j] = big[j], big[i] })
		rng.Shuffle(len(small), func(i, j int) { small[i], small[j] = small[j], small[i] })
		for i, fv := range big {
			m.Update(fv, bigY)
			m.Update(small[i%len(small)], smallY)
		}
	}
	return m, nil
}

// scorePriority scores a model over examples' as-of features with identity
// stripped — the same view fitPriority trained on. The cold start reads none of
// the stripped keys, so stripping changes nothing for it.
func scorePriority(m Model, examples []PriorityExample, now bool) []float64 {
	out := make([]float64, len(examples))
	for i, e := range examples {
		fv := e.AsOf
		if now {
			fv = e.Now
		}
		out[i] = m.Score(labelModelFeatures(fv))
	}
	return out
}

// priorityCV is the pooled out-of-fold result of sender-grouped k-fold
// cross-validation.
type priorityCV struct {
	// Cand[i] is examples[i]'s score from the fold model that never saw its
	// sender domain; Cold[i] is the cold start's score of the same example.
	Cand, Cold      []float64
	Positive        []bool
	PositiveSenders int
	UsableFolds     int
}

// crossValidatePriority partitions sender domains into folds (labelCVFolds:
// every domain in exactly one holdout, positives and negative-only domains
// round-robined separately), fits on k-1 and scores the held-out fold.
// An example in a fold that cannot be fitted (single-class training side) is
// scored 0 — "the procedure produced no model", which can only lose recall —
// and the fold is not counted as usable.
func crossValidatePriority(examples []PriorityExample, folds int, seed ulid.ULID) priorityCV {
	cv := priorityCV{
		Cand:     make([]float64, len(examples)),
		Cold:     scorePriority(NewColdStart(), examples, false),
		Positive: make([]bool, len(examples)),
	}
	lex := make([]LabelExample, len(examples))
	for i, e := range examples {
		lex[i] = LabelExample{Positive: e.Positive, Group: e.Group}
		cv.Positive[i] = e.Positive
	}
	keys := groupKeys(lex)
	foldOf, posGroups := labelCVFolds(lex, keys, folds)
	cv.PositiveSenders = posGroups
	for f := 0; f < folds; f++ {
		var fit, hold []PriorityExample
		var holdIdx []int
		for i, e := range examples {
			if foldOf[keys[i]] == f {
				hold = append(hold, e)
				holdIdx = append(holdIdx, i)
			} else {
				fit = append(fit, e)
			}
		}
		if len(hold) == 0 {
			continue
		}
		m, err := fitPriority(fit, seed)
		if err != nil {
			continue // scores stay 0 for this fold
		}
		var hp, hn bool
		for _, e := range hold {
			if e.Positive {
				hp = true
			} else {
				hn = true
			}
		}
		if hp && hn {
			cv.UsableFolds++
		}
		for j, s := range scorePriority(m, hold, false) {
			cv.Cand[holdIdx[j]] = s
		}
	}
	return cv
}

// EvaluatePriorityGate runs the full judgement over a corpus: grouped CV, then
// the gate. Exposed for the report, which prints the verdict the daily pass
// would reach today — the same function, so the two cannot disagree (#155).
func EvaluatePriorityGate(c PriorityCorpus, seed ulid.ULID) PriorityGate {
	cv := crossValidatePriority(c.Examples, priorityGateFolds, seed)
	return priorityGate(cv.Cand, cv.Cold, cv.Positive, cv.PositiveSenders, priorityGateFolds, cv.UsableFolds)
}
