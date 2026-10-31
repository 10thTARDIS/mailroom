// SPDX-License-Identifier: AGPL-3.0-only

// Package ml is Mailroom's ML foundation (spec §8, mailroom#35): the model
// registry (snapshot / activate / rollback / reset over the encrypted blob
// store) and a feedback-replay eval harness, built BEFORE any real scorer.
//
// It DEFINES the interfaces the real priority/labeler scorer (mailroom#33) will
// implement — Model, FeatureVector, FeatureExtractor — and ships a trivial
// baseline model so the registry and harness are testable today. #33 swaps in
// the real NB / online-LR model by implementing Model and registering its
// serialized type with RegisterModelType; nothing else in this package changes.
//
// ml_models and feedback_events are OPERATIONAL data (like account_sync_status),
// NOT JMAP-visible objects: no change_log rows, no store ObjectType. The user-
// facing reset/rollback surface arrives later with the Triage/* JMAP methods.
//
// Boundaries (CLAUDE.md): ml may import internal/store, internal/blob and
// internal/crypto, but never internal/api.
package ml

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// FeatureVector is a SPARSE feature map: hashed-feature-name → value. An absent
// key is an implicit 0.0. The convention is presence-as-1.0 — a plain boolean
// feature is stored with value 1.0 — while counts and magnitudes are carried as
// larger values. Keys are opaque strings (typically hashed feature names); the
// extractor (mailroom#33) owns the hashing scheme, and the model only ever sees
// the resulting map, so the two can evolve the feature space independently.
type FeatureVector map[string]float64

// Model is the interface the real scorer (mailroom#33) implements; the registry
// and eval harness are written entirely against it. It is deliberately minimal
// — resist adding methods a scorer does not strictly need.
//
// A Model is stateful and updated online. Score and Update are NOT safe for
// concurrent use on the same instance; a caller that scores from multiple
// goroutines holds its own lock or its own instance (ActiveModel hands back a
// fresh instance per call).
type Model interface {
	// Score returns a probability-like priority in [0,1] for f.
	Score(f FeatureVector) float64
	// Update performs one online-learning step nudging the model toward label
	// (clamped to [0,1]) for f. It mutates the receiver.
	Update(f FeatureVector, label float64)
	// Kind reports the model kind ("priority" | "labeler"), matching the
	// app.ml_models.model_kind the snapshot is stored under.
	Kind() string

	// Snapshots serialize/deserialize the model. MarshalBinary emits a self-
	// describing blob (magic + type tag + format version + payload) so
	// UnmarshalModel can dispatch to the right implementation and a future
	// format is detectable. MarshalBinary and UnmarshalBinary are exact
	// inverses on a single instance.
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
}

// Serialized-model framing. Every model blob begins with these three bytes so
// UnmarshalModel can pick the implementation without trusting the DB, and an
// unknown future format fails loudly instead of being mis-parsed.
const (
	// modelMagic tags a Mailroom model blob. Bumped only if the framing itself
	// (not a specific model's payload format) ever changes.
	modelMagic byte = 0xE1
)

// Model type tags. Each concrete Model implementation owns one byte. The
// baseline reserves 0x01; mailroom#33 registers its own (e.g. 0x02) via
// RegisterModelType. Tags are a stable wire contract — never reuse a retired one.
const typeTagBaseline byte = 0x01

// ModelFactory builds an empty model of a registered implementation for the
// given kind, ready for UnmarshalBinary to populate. The kind flows through from
// the ml_models row so a kind-agnostic implementation (like the baseline) knows
// which kind it is reconstructing.
type ModelFactory func(kind string) Model

// modelFactories maps a type tag to its factory. Populated only at init time
// (RegisterModelType), so it needs no locking at runtime.
var modelFactories = map[byte]ModelFactory{}

// RegisterModelType wires a model type tag to its factory. It is called from
// init() by each implementation (the baseline here, the real scorer in #33).
// Duplicate or nil registrations are a programming error and panic at startup.
func RegisterModelType(typeTag byte, f ModelFactory) {
	if f == nil {
		panic("ml: RegisterModelType: nil factory")
	}
	if _, dup := modelFactories[typeTag]; dup {
		panic(fmt.Sprintf("ml: RegisterModelType: type tag %#x already registered", typeTag))
	}
	modelFactories[typeTag] = f
}

func init() {
	RegisterModelType(typeTagBaseline, func(kind string) Model { return NewBaseline(kind) })
}

// UnmarshalModel reconstructs a Model from a snapshot blob. It peeks the framing
// (magic + type tag) to pick the implementation, builds an empty instance for
// kind, then delegates to the model's own UnmarshalBinary. An unknown magic or
// type tag is an error, never a silent mis-parse — this is what makes a future
// model format detectable at load time.
func UnmarshalModel(kind string, data []byte) (Model, error) {
	if len(data) < 3 {
		return nil, errors.New("ml: model blob too short")
	}
	if data[0] != modelMagic {
		return nil, fmt.Errorf("ml: bad model magic %#x", data[0])
	}
	tag := data[1]
	f, ok := modelFactories[tag]
	if !ok {
		return nil, fmt.Errorf("ml: unknown model type tag %#x", tag)
	}
	m := f(kind)
	if err := m.UnmarshalBinary(data); err != nil {
		return nil, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// baselineModel — the trivial stand-in scorer.
// ---------------------------------------------------------------------------

// baselineFormatV1 is the payload format version for baselineModel. Bumping it
// lets a future baseline change its serialization while old blobs stay readable.
const baselineFormatV1 byte = 1

// baselineLearningRate is the online step size. It is a fixed structural
// constant of the baseline, not serialized state.
const baselineLearningRate = 0.1

// baselineModel is a single-bias logistic regressor: it ignores features
// entirely and predicts sigmoid(bias). Fresh, bias = 0 so it Scores 0.5; each
// Update nudges bias by lr*(label - Score), so over a stream it converges to the
// log-odds of the running mean label. It exists ONLY so the registry and eval
// harness are exercisable now; mailroom#33 replaces it with a real feature-using
// model. Because it implements the full Model contract (including a versioned
// Marshal/Unmarshal round-trip), it is a faithful rehearsal of that contract.
type baselineModel struct {
	kind string
	bias float64 // logit; Score = sigmoid(bias)
	n    int64   // updates applied (diagnostics only)
	lr   float64 // learning rate; not serialized (set from the constant)
}

var _ Model = (*baselineModel)(nil)

// NewBaseline returns a fresh baseline model of the given kind. It Scores 0.5
// until trained. This is the model the registry hands back when a user has no
// active model, so the system always has a scorer.
func NewBaseline(kind string) Model {
	return &baselineModel{kind: kind, lr: baselineLearningRate}
}

func (m *baselineModel) Kind() string { return m.kind }

// Score returns sigmoid(bias), ignoring f (the baseline is feature-blind).
func (m *baselineModel) Score(f FeatureVector) float64 { return sigmoid(m.bias) }

// Update takes one gradient step of single-bias logistic regression toward
// label (clamped to [0,1]).
func (m *baselineModel) Update(f FeatureVector, label float64) {
	label = clamp01(label)
	if m.lr == 0 {
		m.lr = baselineLearningRate
	}
	m.bias += m.lr * (label - sigmoid(m.bias))
	m.n++
}

// MarshalBinary emits: [magic][typeTagBaseline][baselineFormatV1]
// [uint16 kind len][kind][float64 bias][int64 n]. Big-endian throughout.
func (m *baselineModel) MarshalBinary() ([]byte, error) {
	kb := []byte(m.kind)
	if len(kb) > math.MaxUint16 {
		return nil, fmt.Errorf("ml: baseline kind too long (%d bytes)", len(kb))
	}
	buf := make([]byte, 0, 3+2+len(kb)+16)
	buf = append(buf, modelMagic, typeTagBaseline, baselineFormatV1)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(kb)))
	buf = append(buf, kb...)
	buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(m.bias))
	buf = binary.BigEndian.AppendUint64(buf, uint64(m.n))
	return buf, nil
}

// UnmarshalBinary is the exact inverse of MarshalBinary. It validates the
// framing (magic, type tag, format version) and restores kind, bias and n.
func (m *baselineModel) UnmarshalBinary(data []byte) error {
	if len(data) < 3 {
		return errors.New("ml: baseline blob too short")
	}
	if data[0] != modelMagic {
		return fmt.Errorf("ml: baseline: bad magic %#x", data[0])
	}
	if data[1] != typeTagBaseline {
		return fmt.Errorf("ml: baseline: wrong type tag %#x", data[1])
	}
	if data[2] != baselineFormatV1 {
		return fmt.Errorf("ml: baseline: unsupported format version %d", data[2])
	}
	rest := data[3:]
	if len(rest) < 2 {
		return errors.New("ml: baseline blob truncated (kind length)")
	}
	klen := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) < klen+16 {
		return errors.New("ml: baseline blob truncated (body)")
	}
	m.kind = string(rest[:klen])
	rest = rest[klen:]
	m.bias = math.Float64frombits(binary.BigEndian.Uint64(rest))
	rest = rest[8:]
	m.n = int64(binary.BigEndian.Uint64(rest))
	m.lr = baselineLearningRate
	return nil
}

// sigmoid is the numerically-stable logistic function.
func sigmoid(x float64) float64 {
	if x >= 0 {
		z := math.Exp(-x)
		return 1 / (1 + z)
	}
	z := math.Exp(x)
	return z / (1 + z)
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
