// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"errors"
	"fmt"
)

// coldStartModel is the DETERMINISTIC heuristic priority scorer that fills the
// no-trained-model gap (spec §8's cold-start line, mailroom#36). Before a user
// has any trained model, the registry used to fall back to the feature-blind
// baseline, which Scores a flat 0.5 for every message — so a brand-new inbox had
// no priority signal at all, and "first impressions decide whether users trust
// triage." This model replaces that flat fallback with a hand-tuned logit over
// the EXISTING featuresFromFacts keys so mail is sensibly prioritized from day
// one: direct-to-you and known/replied-to senders rank UP; mailing-list mail,
// List-Unsubscribe, and auth-failing bulk rank DOWN.
//
// It is a FIXED heuristic, not a learned model:
//
//   - Score is read-only over fixed weights (like logisticModel.Score, and
//     UNLIKE the general Model contract, it mutates nothing, so it is safe to
//     Score concurrently on one instance — the serve-time Scorer caches one
//     instance per user and relies on that);
//   - Update is a NO-OP (there is nothing to learn — the weights are constants);
//   - it is NEVER snapshotted to the blob store (only trained models are
//     persistable, activated artifacts; this only ever fills the gap until #33's
//     trainer activates a real one, at which point ActiveModel returns THAT and
//     this is never consulted). MarshalBinary/UnmarshalBinary are nonetheless a
//     proper STATELESS pair (framing only, no payload) so the full Model contract
//     holds and nothing panics if marshal is ever reached.
//
// It owns model TYPE TAG 0x03 (baseline 0x01, logistic 0x02), registered in
// init() so UnmarshalModel can round-trip the framing.
type coldStartModel struct{}

var _ Model = coldStartModel{}
var _ Explainer = coldStartModel{}

// coldStartDirectKey is the SYNTHETIC contribution key for the direct-to-you
// proxy term (Score's small-recipient-set-and-not-list-mail signal). There is no
// featuresFromFacts feature for it — it is derived from the tocc: bucket and the
// absence of list:1 — so the explanation attributes it to its own key, which
// FeatureLabel renders as "addressed directly to you".
const coldStartDirectKey = "direct:1"

// coldStartFormatV1 is the payload format version (there is no payload; the byte
// still lets a future heuristic reframe while old blobs stay detectable).
const coldStartFormatV1 byte = 1

// typeTagColdStart is this implementation's wire tag (baseline 0x01, logistic
// 0x02). Stable contract — never reuse a retired tag.
const typeTagColdStart byte = 0x03

func init() {
	RegisterModelType(typeTagColdStart, func(string) Model { return newColdStart() })
}

// Cold-start feature weights. These are a HAND-TUNED logit (not learned), read
// off the sparse featuresFromFacts keys and summed into a logit that sigmoid maps
// to [0,1]. They are calibrated around the presentation cutoff ImportantThreshold
// (0.6, logit ≈ +0.405, defined in internal/api): a known sender writing directly
// to you lands clearly ABOVE the line, while list / unsubscribe / auth-failing
// bulk land clearly BELOW it. The bias is mildly negative so a bare, unremarkable
// message sits just under neutral and only affirmative signals push it up.
// RECALIBRATED for mailroom#157 against a sample of real messages, where the previous
// weights scored nearly everything important and ranked a delivery-preview sender
// at the top. Two changes of substance, not just numbers:
//
//   - the big relationship weight now hangs off CORRESPONDENCE (you wrote to
//     them), not off being in the address book (they wrote to you). Under the old
//     scheme a daily newsletter earned the largest UP weight in the model.
//   - bulk mail can no longer collect the direct-address bonus, and
//     List-Unsubscribe alone is now decisive enough to sink a message on its own.
//     It is the most reliable bulk marker there is: transactional mail
//     deliberately omits it, because an unsubscribe must never stop a receipt.
//
// The arithmetic that matters, at the 0.6 cutoff (logit +0.405):
//
//	newsletter (unsub, direct-looking, never written to): -0.2 -1.6 = -1.8 -> 0.14
//	someone you write to, replying:                       -0.2 +1.8 +1.2 +0.9 = 3.7 -> 0.98
//	a stranger writing to you directly:                   -0.2 +0.9 = 0.7 -> 0.67
//
// A stranger addressing you personally still clears the line — that is mail from
// a human that has never reached you before, which is exactly what must not be
// buried. Bulk lands far below it, and no amount of volume moves it up.
const (
	coldStartBias = -0.2 // Score(no signals) = sigmoid(-0.2) ≈ 0.45

	// UP — relationship / direct-address signals.
	coldStartCorresponded = 1.8 // YOU have written to this address (strongest signal)
	coldStartReply        = 1.2 // a reply within a thread you are part of
	coldStartDirect       = 0.9 // addressed to you personally, and NOT bulk
	coldStartFlagged      = 0.5 // the server already flagged it important/junk (mild)

	// DOWN — bulk / list / authentication signals.
	coldStartList     = -1.4 // carries List-Id (mailing-list / bulk mail)
	coldStartUnsub    = -1.6 // carries List-Unsubscribe — on its own, enough to sink it
	coldStartAuthFail = -2.5 // authentication clearly failed ("auth-failing bulk sinks")
	coldStartNoReply  = -1.2 // sent from an address that does not accept replies
)

// coldStartOnewayPenalty maps the oneway:<bucket> categorical — mail received
// from an address the user has NEVER written back to — to a DOWN weight that
// grows with the volume.
//
// This is the old seen: signal used in the direction the evidence actually
// supports (#157). Frequency of contact was treated as intimacy, which is how a
// daily digest became the most important sender in the mailbox; frequency
// WITHOUT reciprocity is the opposite, and it is what catches the bulk senders
// that carry no List-Unsubscribe to give themselves away — delivery-preview
// services, neighbourhood networks, storefront announcements.
//
// The buckets start at onewayMin, so a first contact is never penalised.
var coldStartOnewayPenalty = map[string]float64{
	"6-10":  -0.6,
	"11-25": -0.9,
	"26+":   -1.1,
}

// coldStartSentWeights maps the sent:<bucket> categorical — how many times YOU
// have written to this address — to a monotonic UP weight. sent:0 (never written
// to) contributes nothing, which is the whole point: it is the bucket every
// newsletter in the mailbox falls into.
//
// It replaces the old seen: weights, which counted mail RECEIVED and therefore
// rewarded whoever mailed you most (#157). The weights here are deliberately
// smaller than that scheme's: correspondence is already carried by corr:1, and
// this only distinguishes a one-off reply from an ongoing thread of exchanges.
var coldStartSentWeights = map[string]float64{
	"1":     0.2,
	"2":     0.3,
	"3-5":   0.5,
	"6-10":  0.6,
	"11-25": 0.7,
	"26+":   0.8,
}

// coldStartToccPenalty maps large tocc:<bucket> fan-out to a mild DOWN weight:
// mail addressed to many recipients is less likely to be personally for you.
// Only the large buckets carry a penalty; small ones feed the direct-address
// proxy instead (see Score).
var coldStartToccPenalty = map[string]float64{
	"11-25": -0.4,
	"26+":   -0.6,
}

// newColdStart returns the heuristic cold-start model. It is stateless, so every
// caller can share one value; the constructor exists to mirror NewBaseline /
// NewLogistic and to keep the type unexported.
func newColdStart() coldStartModel { return coldStartModel{} }

// NewColdStart returns the deterministic cold-start heuristic as a Model. It is
// the priority fallback the registry hands back when a user has no trained model.
func NewColdStart() Model { return newColdStart() }

// Kind reports KindPriority: the cold-start heuristic only ever scores priority
// (it is the priority-kind fallback; the baseline still covers other kinds).
func (coldStartModel) Kind() string { return KindPriority }

// Score maps the fixed-weight logit over f into [0,1]. It reads f and mutates
// NOTHING (fixed weights, no receiver state), so — like logisticModel.Score and
// unlike the general Model contract — it is safe to call concurrently on one
// instance, which the serve-time Scorer's per-user cache relies on.
// THE BUCKET LOOPS BELOW RANGE OVER MAPS AND STILL SUM DETERMINISTICALLY, which
// is worth stating because #222 fixed exactly this shape in logisticModel.Score.
// Each bucket family is emitted by featuresFromFacts as EXACTLY ONE key —
// `fv["sent:"+countBucket(…)]`, one `tocc:`, at most one `oneway:` — so at most
// one term per loop is nonzero and every other adds an exact 0.0, which is
// order-independent. The fixed terms around them are in source order.
//
// That reasoning depends on the one-bucket-per-family property, not on anything
// local to this function, so TestColdStartBucketFamiliesAreMutuallyExclusive
// pins it: emit two buckets from one family and this becomes order-dependent
// like the logistic scorer was.
func (coldStartModel) Score(f FeatureVector) float64 {
	z := coldStartBias

	// Relationship / direct-address signals (UP). Multiplying by the feature
	// value honors the presence-as-1.0 convention (absent key -> 0 -> no effect).
	z += coldStartCorresponded * f["corr:1"]
	z += coldStartReply * f["reply:1"]
	z += coldStartFlagged * f["flagged:1"]
	for bucket, w := range coldStartSentWeights {
		z += w * f["sent:"+bucket]
	}
	// Direct-to-you proxy: a small recipient set and NOT BULK BY ANY MARKER.
	//
	// It used to test only for List-Id, which marketing mail rarely carries — so
	// a promotion addressed to you alone collected the whole direct-address bonus
	// (#157). List-Unsubscribe is the marker that actually separates bulk from
	// personal mail, and it is now disqualifying here.
	if f["list:1"] == 0 && f["unsub:1"] == 0 && (f["tocc:0"] != 0 || f["tocc:1"] != 0) {
		z += coldStartDirect
	}

	// Bulk / list / authentication signals (DOWN).
	z += coldStartList * f["list:1"]
	z += coldStartUnsub * f["unsub:1"]
	z += coldStartAuthFail * f["authfail:1"]
	z += coldStartNoReply * f["noreply:1"]
	for bucket, w := range coldStartToccPenalty {
		z += w * f["tocc:"+bucket]
	}
	for bucket, w := range coldStartOnewayPenalty {
		z += w * f["oneway:"+bucket]
	}

	return sigmoid(z)
}

// Contributions implements Explainer: the per-key signed breakdown of the SAME
// fixed-weight logit Score computes. It MIRRORS Score term for term — the
// relationship/direct-address UP signals, the seen: bucket, the direct-to-you
// proxy (attributed to coldStartDirectKey), and the bulk/list/auth-failing DOWN
// signals — so the contributions sum to Score's logit minus coldStartBias. Only
// nonzero terms are emitted, so an absent (implicit-0) feature is never reported
// as a reason.
func (coldStartModel) Contributions(f FeatureVector) map[string]float64 {
	out := map[string]float64{}
	add := func(key string, w float64) {
		if w == 0 {
			return
		}
		out[key] += w
	}

	// Relationship / direct-address signals (UP) — mirror Score exactly.
	add("corr:1", coldStartCorresponded*f["corr:1"])
	add("reply:1", coldStartReply*f["reply:1"])
	add("flagged:1", coldStartFlagged*f["flagged:1"])
	for bucket, w := range coldStartSentWeights {
		add("sent:"+bucket, w*f["sent:"+bucket])
	}
	// Direct-to-you proxy: identical predicate to Score (small recipient set and
	// no bulk marker at all). Attributed to the synthetic direct:1 key.
	if f["list:1"] == 0 && f["unsub:1"] == 0 && (f["tocc:0"] != 0 || f["tocc:1"] != 0) {
		add(coldStartDirectKey, coldStartDirect)
	}

	// Bulk / list / authentication signals (DOWN) — mirror Score exactly.
	add("list:1", coldStartList*f["list:1"])
	add("unsub:1", coldStartUnsub*f["unsub:1"])
	add("authfail:1", coldStartAuthFail*f["authfail:1"])
	add("noreply:1", coldStartNoReply*f["noreply:1"])
	for bucket, w := range coldStartToccPenalty {
		add("tocc:"+bucket, w*f["tocc:"+bucket])
	}
	for bucket, w := range coldStartOnewayPenalty {
		add("oneway:"+bucket, w*f["oneway:"+bucket])
	}

	return out
}

// Update is a NO-OP: the cold-start heuristic is fixed, not learned. Feedback
// trains the real model (#33), which then supersedes this fallback entirely.
func (coldStartModel) Update(FeatureVector, float64) {}

// MarshalBinary emits the framing only — [magic][typeTagColdStart]
// [coldStartFormatV1] with no payload — because the heuristic carries no state.
// It exists so the Model contract holds; the registry never actually snapshots
// this model (it is the un-persisted fallback, not an activated artifact).
func (coldStartModel) MarshalBinary() ([]byte, error) {
	return []byte{modelMagic, typeTagColdStart, coldStartFormatV1}, nil
}

// UnmarshalBinary validates the framing (magic, type tag, format version). There
// is no payload to restore — it is the exact inverse of MarshalBinary.
func (coldStartModel) UnmarshalBinary(data []byte) error {
	if len(data) < 3 {
		return errors.New("ml: coldstart blob too short")
	}
	if data[0] != modelMagic {
		return fmt.Errorf("ml: coldstart: bad magic %#x", data[0])
	}
	if data[1] != typeTagColdStart {
		return fmt.Errorf("ml: coldstart: wrong type tag %#x", data[1])
	}
	if data[2] != coldStartFormatV1 {
		return fmt.Errorf("ml: coldstart: unsupported format version %d", data[2])
	}
	return nil
}
