// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// KindPriority is the model_kind the priority scorer trains/serves under (it
// matches app.ml_models.model_kind and the logistic model's Kind()).
const KindPriority = "priority"

// scorerCacheTTL is how long a per-user active model is reused before the Scorer
// re-reads it from the registry. Model artifacts are tiny, but ActiveModel is a
// blob read + decrypt + decode, so doing it on EVERY delivered message would put
// filesystem + crypto work on the hot ingest path for no benefit — the active
// model changes only when PR2's trainer activates a new version, minutes to days
// apart. A short TTL bounds how stale a just-activated model can be (≤ this) while
// keeping steady-state scoring a pure in-memory Score.
const scorerCacheTTL = 60 * time.Second

// Scorer is the SERVE-TIME entry point the ingest path calls to score a freshly
// delivered message. It wraps the model Registry with a small per-user cache so
// scoring a message is, in steady state, a single in-memory Score — no DB, no
// blob read.
//
// Scoring is BEST-EFFORT by contract: ScoreMessage returns an error only on a
// real failure (a corrupt/undecodable active model), and the caller (ingest)
// swallows it — a scoring failure must never block mail delivery.
type Scorer struct {
	reg *Registry

	mu    sync.Mutex
	cache map[ulid.ULID]cachedModel

	// now is time.Now, overridable in tests for TTL behavior.
	now func() time.Time
	ttl time.Duration
}

// cachedModel is one user's active model plus the metadata to expire it. The
// model instance is SHARED across concurrent ScoreMessage calls: logisticModel
// (and baselineModel) Score is read-only and concurrency-safe, so many
// goroutines may Score the same instance at once. The serving path NEVER calls
// Update on a cached instance, so the read-safety holds.
type cachedModel struct {
	model    Model
	version  int
	loadedAt time.Time
}

// NewScorer builds a Scorer over the model registry.
func NewScorer(reg *Registry) *Scorer {
	return &Scorer{
		reg:   reg,
		cache: map[ulid.ULID]cachedModel{},
		now:   time.Now,
		ttl:   scorerCacheTTL,
	}
}

// ScoreMessage hashes facts through the ONE featuresFromFacts, loads (or reuses)
// the user's active priority model, and returns its Score in [0,1], the model
// version string to stamp on the stored score (app.triage_scores.model_version),
// and the top per-feature contributions behind that score as a JSON array (the
// #34 explainability payload for app.triage_scores.top_features). Until PR2
// activates a trained model the registry falls back to the deterministic
// cold-start heuristic (#36) at version 0, so this returns a feature-driven
// priority (direct/known up, list/auth-failing-bulk down) stamped "priority:0" —
// the correct behavior for "no trained model yet", not an error.
//
// (The version and topFeatures are returned alongside the score — a deliberate
// widening of the #33-sketched (score, err) shape, the same pattern the version
// return already established — because both are stored on the triage row and the
// serving path is the only place that knows which model, and therefore which
// contributions, produced the score.)
//
// topFeatures is the json.Marshal of the top-N []FeatureContribution
// (TopContributions). When the active model is not an Explainer (the feature-
// blind baseline) or produced no nonzero contributions, it is the empty array
// []byte("[]") — never JSON null — so the stored column keeps its default shape.
func (s *Scorer) ScoreMessage(ctx context.Context, userID ulid.ULID, facts MessageFacts) (score float64, modelVersion string, topFeatures []byte, err error) {
	model, version, err := s.modelFor(ctx, userID)
	if err != nil {
		return 0, "", nil, err
	}
	fv := featuresFromFacts(facts)
	score = model.Score(fv)
	top := TopContributions(model, fv, topFeaturesN)
	if len(top) == 0 {
		// Not an Explainer, or nothing contributed: the default empty array (never
		// "null", which json.Marshal of a nil slice would produce).
		return score, modelVersionString(version), []byte("[]"), nil
	}
	payload, err := json.Marshal(top)
	if err != nil {
		return 0, "", nil, fmt.Errorf("ml: marshal top features: %w", err)
	}
	return score, modelVersionString(version), payload, nil
}

// topFeaturesN caps how many per-feature contributions are stored per score —
// enough to explain a ranking without bloating the operational row.
const topFeaturesN = 5

// modelVersionString renders the ml_models version as the model_version stamped
// on a triage score. Kind-qualified so a future kind (labeler) is distinguishable
// and version 0 unambiguously denotes the "no trained model yet" fallback (the
// cold-start heuristic for priority, the baseline for other kinds).
func modelVersionString(version int) string {
	return fmt.Sprintf("%s:%d", KindPriority, version)
}

// modelFor returns the user's active model and its version, served from the
// per-user cache when fresh and reloaded from the registry when the entry is
// missing or past its TTL.
//
// The (potentially slow) ActiveModel load runs OUTSIDE the lock: the lock only
// guards the map. Two concurrent misses for the same user may both load — a
// harmless, rare double-read that resolves to whichever store-write lands last;
// the alternative (holding the lock across a blob read) would serialize every
// user's scoring behind one user's disk I/O.
func (s *Scorer) modelFor(ctx context.Context, userID ulid.ULID) (Model, int, error) {
	s.mu.Lock()
	if e, ok := s.cache[userID]; ok && s.now().Sub(e.loadedAt) < s.ttl {
		m, v := e.model, e.version
		s.mu.Unlock()
		return m, v, nil
	}
	s.mu.Unlock()

	model, version, err := s.reg.ActiveModel(ctx, userID, KindPriority)
	if err != nil {
		return nil, 0, err
	}

	s.mu.Lock()
	s.cache[userID] = cachedModel{model: model, version: version, loadedAt: s.now()}
	s.mu.Unlock()
	return model, version, nil
}
