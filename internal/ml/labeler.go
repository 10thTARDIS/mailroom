// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/oklog/ulid/v2"
)

// The per-label classifier: the long tail of automatic categorisation (spec §8,
// mailroom#169, epic #156).
//
// Rules (#168/#173) cover mail that repeats — four newsletters from one shop
// imply a rule for that shop. They cover NOTHING for a sender seen once. On the
// real mailbox that is most of it: of the messages hand-labelled
// Promotions, most came from senders appearing exactly once. No rule can be
// induced from those, and they are precisely what this model is for.
//
// It is a BINARY model PER LABEL — "does this belong in Promotions?" — not one
// multi-class model over all labels. Two reasons:
//
//   - Labels are not mutually exclusive. A shipping notice can be Shopping AND
//     Updates, and a multi-class model has to pick one.
//   - Labels come and go. Adding one must not invalidate the model for every
//     other, which a single joint model would force.
//
// The registry already supports this: models are keyed by (user, model_kind),
// so each label is its own kind with its own versions and rollback.
//
// ABSTENTION IS THE POINT. A system that abstains 10% of the time is more useful
// than one confidently wrong 3% of the time, because silent misfiling is what
// destroys trust in the whole feature — and the trust is not per-message, it is
// in the idea that the mailbox can be left alone. So this model is allowed to
// say "I do not know", and that answer routes to a human instead of a label.

// KindLabel is the model_kind a label's classifier is stored under. Namespaced
// so it can never collide with KindPriority or any future model kind.
func KindLabel(labelID ulid.ULID) string { return "label:" + labelID.String() }

// LabelIDFromKind reverses KindLabel, reporting false for any other kind.
func LabelIDFromKind(kind string) (ulid.ULID, bool) {
	rest, ok := strings.CutPrefix(kind, "label:")
	if !ok {
		return ulid.ULID{}, false
	}
	id, err := ulid.Parse(rest)
	if err != nil {
		return ulid.ULID{}, false
	}
	return id, true
}

// The abstention band. Below LabelApplyBelow the model is confident the message
// does NOT belong; above LabelApplyAbove it is confident it does; between them
// it declines to answer and the message is surfaced for a human.
//
// THE BAND IS DELIBERATELY WIDE. The costs are not symmetric: a wrong label is
// worse than an unanswered question, because the wrong label is silent — mail
// ends up somewhere the user does not look, and they find out weeks later, or
// never. An abstention costs a glance at a list they chose to open.
//
// These are STARTING points, not tuned constants. Tuning them means measuring
// the confidence distribution over a user's own labelled mail, which needs a
// corpus that exists only after they have labelled for a while (the same reason
// #154's threshold shipped unbuilt). Until then, err toward asking.
const (
	LabelApplyAbove = 0.80
	LabelApplyBelow = 0.20
)

// LabelVerdict is what the classifier says about one message and one label.
type LabelVerdict struct {
	// Confidence is the model's probability the label applies, in [0,1].
	Confidence float64
	// Apply is true only when the model is confident the label BELONGS. It is
	// never true while Abstain is.
	Apply bool
	// Abstain reports that the model declined: the message goes to the
	// needs-a-category surface rather than being filed or silently ignored.
	Abstain bool
	// Reasons are the top feature contributions behind the confidence, already
	// humanised — the same explainability contract the priority score carries
	// (#34). A category you cannot interrogate is one you cannot trust.
	Reasons []FeatureContribution
	// ModelVersion is the model that decided, stamped onto a stored suggestion so
	// a stale one is recognisable after a retrain.
	ModelVersion int
}

// Decide turns a confidence into a verdict.
//
// Note what the three outcomes mean, because two of them look alike from
// outside: a LOW confidence is a real answer ("this does not belong here") and
// results in no label and no question. An ABSTENTION is the absence of an
// answer, and results in a question. Collapsing them would either bury
// uncertainty as a negative or flood the needs-a-category surface with every
// message the model confidently rejected.
func Decide(confidence float64, reasons []FeatureContribution) LabelVerdict {
	v := LabelVerdict{Confidence: confidence, Reasons: reasons}
	switch {
	case confidence >= LabelApplyAbove:
		v.Apply = true
	case confidence <= LabelApplyBelow:
		// Confidently not this label: nothing to do, nothing to ask.
	default:
		v.Abstain = true
	}
	return v
}

// MinLabelTrainSamples is the fewest hand-labelled examples a label needs before
// its classifier is trained at all.
//
// Higher than the rule inducer's two, and for a different reason: a rule is
// INSPECTED before it is accepted, so two examples plus a human reading the
// blast radius is enough. A classifier is not inspected — it generalises from
// whatever it was given — so it needs enough examples to be about the category
// rather than about the handful of messages it happened to see.
//
// Below this the label simply has no model, and every message abstains into the
// needs-a-category surface. That is the correct behaviour, not a degraded one:
// with too little evidence, asking IS the right answer.
const MinLabelTrainSamples = 8

// LabelExample is one training row: a message's features, and whether the user
// put this label on it.
//
// Positives come ONLY from labels the user applied by hand. A label applied by a
// rule or by this model is an automation's own output, and training on it closes
// a loop where the model confirms itself: its mistakes become its evidence, and
// confidence grows while accuracy does not.
type LabelExample struct {
	Features FeatureVector
	Positive bool
	// Group is the sender domain this example came from — the unit the holdout
	// is split on (#169). Never a feature; the model does not see it.
	Group string
}

// labelModelFeatures returns a copy of fv with the sender-IDENTITY features
// removed: the exact From address (`from:`), its domain (`dom:`), and the
// delivery alias the message arrived on (`alias:`). The label model never
// consumes them — not in training, not in evaluation (#169).
//
// WHY EXCLUDE RATHER THAN GROUP. The holdout is grouped by from-domain, which
// keeps `from:`/`dom:` honest — a domain held out whole takes those keys with
// it — but `alias:` crosses that boundary: a per-service delivery alias
// receives one service's mail from SEVERAL mailer domains, so a held-out
// domain still shares its alias with the fit set and the "unseen sender"
// holdout is not unseen at all. The alternative fix, grouping by the connected
// component of (domain, alias) pairs, closes the same leak but fuses groups
// through any widely-shared alias — on a catchall mailbox the primary address
// alone would collapse most of the corpus into one group and make most labels
// unevaluable.
//
// Exclusion is also what the model's charter says. A sender it already knows
// is the rule inducer's job (`alias =` / `fromDomain =` rules); this model
// exists solely for mail from senders it has NOT seen, where identity features
// can contribute nothing but memorisation. Serving needs no change: a model
// trained without these keys simply has no weight for them at score time.
func labelModelFeatures(fv FeatureVector) FeatureVector {
	out := make(FeatureVector, len(fv))
	for k, v := range fv {
		if strings.HasPrefix(k, "from:") || strings.HasPrefix(k, "dom:") || strings.HasPrefix(k, "alias:") {
			continue
		}
		out[k] = v
	}
	return out
}

// TrainLabeler fits a fresh binary model for one label.
//
// COLD RESTART, not warm-start, matching the priority trainer: a pass is then
// reproducible from the examples alone, and cannot compound toward the majority
// class over overlapping windows.
//
// It refuses single-class data. With only positives (or only negatives) a
// classifier learns to answer the same thing every time and reports high
// confidence doing it — the exact shape of a model that looks trained and is
// worthless.
//
// THE EPOCH IS SHUFFLED AND CLASS-BALANCED (#169). The examples arrive
// positives-first with ~20x more negatives, and an unshuffled pass over that
// ends every epoch with hundreds of consecutive negative updates: any feature
// present in BOTH classes is driven net-negative while keys unique to the
// training senders keep their positive gradient — sender memorisation is a
// fixed point of that procedure, which is exactly the defect the grouped
// holdout exists to catch. So each epoch walks the MAJORITY class once in a
// fresh shuffled order, alternating every update with a member of the shuffled
// minority class (cycled). Balancing by ALTERNATION rather than per-example
// gradient weights because Model.Update takes no weight — the interface is
// shared with the priority scorer and not this trainer's to change — and
// cycling the minority is equivalent to inverse-frequency class weights in
// expectation, while also capping same-class runs at one update, which kills
// the end-of-epoch drift outright. A side effect worth naming: the model's
// implied base rate becomes ~even odds rather than the label's raw positive
// rate, so confidences are comparable across labels of very different sizes —
// a precondition for one fixed abstention band serving every label.
//
// The shuffle is seeded from the LABEL ID alone — not time, not global rand —
// so a pass is reproducible from the evidence, the same property the cold
// restart preserves. Identity features are stripped before any update; see
// labelModelFeatures.
func TrainLabeler(labelID ulid.ULID, examples []LabelExample, passes int) (Model, error) {
	if len(examples) < MinLabelTrainSamples {
		return nil, fmt.Errorf("ml: %d examples, need at least %d to train a label model",
			len(examples), MinLabelTrainSamples)
	}
	// Partition by class up front, stripping identity features ONCE rather than
	// per update. The input slice is never reordered: callers (the naive-split
	// diagnostic) depend on its order.
	var pos, neg []FeatureVector
	for _, e := range examples {
		fv := labelModelFeatures(e.Features)
		if e.Positive {
			pos = append(pos, fv)
		} else {
			neg = append(neg, fv)
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return nil, fmt.Errorf("ml: label training data has only one class (%d positive, %d negative)", len(pos), len(neg))
	}
	if passes <= 0 {
		passes = 8
	}

	m := NewLogistic(KindLabel(labelID))
	rng := rand.New(rand.NewPCG(
		binary.BigEndian.Uint64(labelID[:8]),
		binary.BigEndian.Uint64(labelID[8:]),
	))
	big, bigY, small, smallY := neg, 0.0, pos, 1.0
	if len(pos) > len(neg) {
		big, bigY, small, smallY = pos, 1.0, neg, 0.0
	}
	for p := 0; p < passes; p++ {
		rng.Shuffle(len(big), func(i, j int) { big[i], big[j] = big[j], big[i] })
		rng.Shuffle(len(small), func(i, j int) { small[i], small[j] = small[j], small[i] })
		for i, fv := range big {
			m.Update(fv, bigY)
			m.Update(small[i%len(small)], smallY)
		}
	}
	return m, nil
}
