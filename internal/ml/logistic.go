// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// logisticModel is the real T1 priority scorer (mailroom#33): an ONLINE
// logistic regressor over the sparse feature space featuresFromFacts emits. It
// replaces baselineModel as the trained model the registry stores and hands
// back; until PR2 wires the training loop no user has one active, so
// ActiveModel still falls back to the feature-blind baseline and scores hover
// at ~0.5 — which is correct, not a bug.
//
// Score = sigmoid(bias + Σ weights[k]·f[k]) over the present (value-1.0)
// features. Update is one L2-regularized SGD step of log-loss. It owns model
// TYPE TAG 0x02 (the baseline reserves 0x01), registered in init() so
// UnmarshalModel can reconstruct it from a snapshot blob.
type logisticModel struct {
	// kind is the model kind ("priority"); serialized.
	kind string
	// weights is the sparse feature-name → weight map; serialized.
	weights map[string]float64
	// bias is the logit intercept; serialized.
	bias float64
	// n counts updates applied — diagnostics only; serialized.
	n int64
	// lr (learning rate) and l2 (penalty) are STRUCTURAL constants, not
	// serialized: a snapshot restores weights/bias and re-applies these from the
	// package constants, so tuning them never invalidates a stored artifact.
	lr float64
	l2 float64
}

var _ Model = (*logisticModel)(nil)
var _ Explainer = (*logisticModel)(nil)

// logisticFormatV1 is the payload format version. Bumping it lets a future
// logistic model change its serialization while old blobs stay readable.
const logisticFormatV1 byte = 1

// typeTagLogistic is this implementation's wire tag (the baseline owns 0x01).
// It is a stable contract — never reuse a retired tag.
const typeTagLogistic byte = 0x02

// Structural hyperparameters. They are NOT serialized: a snapshot restores the
// learned state (weights, bias) and these are re-applied from the constants, so
// tuning them changes future training without invalidating stored artifacts —
// exactly how baselineLearningRate is handled.
const (
	// logisticLearningRate is the SGD step size.
	logisticLearningRate = 0.1
	// logisticL2 is the (small) L2 weight-decay coefficient. Kept low: the
	// feature space is high-cardinality and sparse, so heavy regularization
	// would wash out rare-but-informative signals (a specific sender, a subject
	// token) that priority actually turns on.
	logisticL2 = 1e-4
)

func init() {
	RegisterModelType(typeTagLogistic, func(kind string) Model { return NewLogistic(kind) })
}

// NewLogistic returns a fresh, untrained logistic model of the given kind. Its
// weights are empty and bias is 0, so it Scores exactly 0.5 until trained — the
// same neutral prior as the baseline. PR2's training loop constructs one of
// these, replays feedback through Update, and snapshots it via the registry.
func NewLogistic(kind string) Model {
	return &logisticModel{
		kind:    kind,
		weights: map[string]float64{},
		lr:      logisticLearningRate,
		l2:      logisticL2,
	}
}

func (m *logisticModel) Kind() string { return m.kind }

// Score returns sigmoid(bias + Σ weights[k]·f[k]) in [0,1].
//
// Unlike the Model interface's general contract (Score/Update are not
// concurrency-safe), THIS Score mutates nothing — it only reads weights/bias —
// so it is safe to call from multiple goroutines on the same instance. The
// serve-time Scorer relies on that: it caches one instance per user and Scores
// it concurrently without a per-Score lock. Update, by contrast, IS a mutation
// and must not race a Score, so the training path never shares an instance with
// the serving cache.
// SUMMED IN SORTED KEY ORDER, and that is load-bearing rather than tidiness
// (#222). Go randomizes map iteration order and float addition is not
// associative, so ranging the FeatureVector directly makes z depend on which
// order the runtime happened to pick — two Scores of the same model over the
// same features differ by ~1e-12.
//
// That is harmless as a score and NOT harmless as a decision. The promotion
// gate compares a candidate's holdout AUC against the incumbent's
// (`train_labels.go`), and AUC is a function of these sums: two runs over
// identical evidence can land on opposite sides of that comparison when the two
// models are genuinely tied. A promotion would then be decided by map-iteration
// order rather than by anything about the mail. Sorting is what makes a training
// pass reproducible from its evidence — the same property the label-ULID-seeded
// shuffle and the cold restart exist to give it.
//
// The cost is one small slice and a sort of a few dozen keys per Score, which is
// nothing beside the store round trips around it.
func (m *logisticModel) Score(f FeatureVector) float64 {
	z := m.bias
	for _, k := range sortedFeatureKeys(f) {
		z += m.weights[k] * f[k]
	}
	return sigmoid(z)
}

// sortedFeatureKeys returns f's keys in ascending order, so any float
// accumulation over a FeatureVector can be made order-independent. Returns nil
// for an empty vector, which ranges zero times.
func sortedFeatureKeys(f FeatureVector) []string {
	if len(f) == 0 {
		return nil
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Contributions implements Explainer: the signed per-feature breakdown of the
// SAME logit Score sums. For each PRESENT feature k it reports weights[k]·f[k]
// — the exact term Score adds to z — so the returned values sum to Score's logit
// minus bias. Terms that contribute nothing (an unweighted feature, or a
// zero-valued one) are omitted rather than reported as a 0-weight reason.
func (m *logisticModel) Contributions(f FeatureVector) map[string]float64 {
	out := make(map[string]float64, len(f))
	for k, v := range f {
		c := m.weights[k] * v
		if c == 0 {
			continue
		}
		out[k] = c
	}
	return out
}

// Update performs one L2-regularized SGD step of logistic-regression log-loss
// toward label (clamped to [0,1]):
//
//	p        = sigmoid(bias + Σ w·f)          (current prediction)
//	bias    += lr·(label - p)                 (intercept is not regularized)
//	w[k]    += lr·((label - p)·f[k] - l2·w[k]) for each PRESENT feature k
//
// The weight decay is applied lazily, only to the features present in f (the
// standard sparse-SGD approximation): touching absent weights would make each
// step O(|weights|) instead of O(nnz) and, for a presence-1.0 feature space,
// changes nothing a feature ever actually contributes. It mutates the receiver.
//
// THE FEATURE LOOP BELOW IS DELIBERATELY NOT SORTED, unlike Score's (#222).
// Every iteration reads and writes m.weights[k] for its own k and touches no
// accumulator, so the steps are independent and any order produces bit-identical
// weights. The one order-dependent quantity in this method is p, and that is
// Score's sum — fixing it there fixes it here. Sorting a second time would cost
// a sort per training step (millions across a CV run) and buy nothing.
// TestLogisticUpdateIsBitReproducible is the probe that keeps this claim honest.
func (m *logisticModel) Update(f FeatureVector, label float64) {
	label = clamp01(label)
	if m.lr == 0 { // a zero-valued struct (defensive; NewLogistic/Unmarshal set it)
		m.lr = logisticLearningRate
	}
	if m.weights == nil {
		m.weights = map[string]float64{}
	}
	p := m.Score(f)
	grad := label - p // negative log-loss gradient w.r.t. the logit
	m.bias += m.lr * grad
	for k, v := range f {
		w := m.weights[k]
		m.weights[k] = w + m.lr*(grad*v-m.l2*w)
	}
	m.n++
}

// MarshalBinary emits a self-describing blob mirroring baselineModel's framing:
//
//	[magic][typeTagLogistic][logisticFormatV1]
//	[uint16 kind len][kind]
//	[float64 bias][int64 n]
//	[uint32 weight count]( [uint16 key len][key][float64 value] )*
//
// Big-endian throughout; length-prefixed strings/maps. It is the exact inverse
// of UnmarshalBinary. lr/l2 are structural constants, not serialized.
//
// WEIGHTS ARE EMITTED IN SORTED KEY ORDER (#222). Model snapshots are stored as
// CONTENT-ADDRESSED blobs (ADR-004, Registry.snapshot), so a map-ordered payload
// means the same model serializes to a different content hash on every call: the
// blob store cannot recognise two snapshots of one model as the same object, and
// byte identity — the cheapest possible check that a restored model IS the model
// that was saved — is not available to any test. Sorting makes the encoding a
// function of the model rather than of the runtime.
func (m *logisticModel) MarshalBinary() ([]byte, error) {
	kb := []byte(m.kind)
	if len(kb) > math.MaxUint16 {
		return nil, fmt.Errorf("ml: logistic kind too long (%d bytes)", len(kb))
	}
	if len(m.weights) > math.MaxUint32 {
		return nil, fmt.Errorf("ml: logistic has too many weights (%d)", len(m.weights))
	}
	buf := make([]byte, 0, 3+2+len(kb)+16+4+len(m.weights)*(2+8))
	buf = append(buf, modelMagic, typeTagLogistic, logisticFormatV1)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(kb)))
	buf = append(buf, kb...)
	buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(m.bias))
	buf = binary.BigEndian.AppendUint64(buf, uint64(m.n))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(m.weights)))
	keys := make([]string, 0, len(m.weights))
	for k := range m.weights {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m.weights[k]
		kbk := []byte(k)
		if len(kbk) > math.MaxUint16 {
			return nil, fmt.Errorf("ml: logistic weight key too long (%d bytes)", len(kbk))
		}
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(kbk)))
		buf = append(buf, kbk...)
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(v))
	}
	return buf, nil
}

// UnmarshalBinary is the exact inverse of MarshalBinary. It validates the
// framing (magic, type tag, format version), restores kind/bias/n and every
// weight, and re-applies the structural lr/l2 constants.
func (m *logisticModel) UnmarshalBinary(data []byte) error {
	if len(data) < 3 {
		return errors.New("ml: logistic blob too short")
	}
	if data[0] != modelMagic {
		return fmt.Errorf("ml: logistic: bad magic %#x", data[0])
	}
	if data[1] != typeTagLogistic {
		return fmt.Errorf("ml: logistic: wrong type tag %#x", data[1])
	}
	if data[2] != logisticFormatV1 {
		return fmt.Errorf("ml: logistic: unsupported format version %d", data[2])
	}
	rest := data[3:]

	if len(rest) < 2 {
		return errors.New("ml: logistic blob truncated (kind length)")
	}
	klen := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) < klen+16+4 {
		return errors.New("ml: logistic blob truncated (header)")
	}
	m.kind = string(rest[:klen])
	rest = rest[klen:]
	m.bias = math.Float64frombits(binary.BigEndian.Uint64(rest))
	rest = rest[8:]
	m.n = int64(binary.BigEndian.Uint64(rest))
	rest = rest[8:]

	count := int(binary.BigEndian.Uint32(rest))
	rest = rest[4:]
	weights := make(map[string]float64, count)
	for i := 0; i < count; i++ {
		if len(rest) < 2 {
			return errors.New("ml: logistic blob truncated (weight key length)")
		}
		klen := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if len(rest) < klen+8 {
			return errors.New("ml: logistic blob truncated (weight body)")
		}
		key := string(rest[:klen])
		rest = rest[klen:]
		weights[key] = math.Float64frombits(binary.BigEndian.Uint64(rest))
		rest = rest[8:]
	}
	m.weights = weights
	m.lr = logisticLearningRate
	m.l2 = logisticL2
	return nil
}
