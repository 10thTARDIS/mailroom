// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// The SERVE path for the per-label classifiers (spec §8, #169): what runs on a
// message as it arrives.
//
// It mirrors Scorer, including the cache shape and the reasons for it — the
// registry load is a blob read, and doing one per label per message would put a
// disk round trip on the delivery path for every category the user has.
//
// The one addition is that a MISS is cached too. Most labels will never have a
// model: they have too few examples, or their training never cleared the
// promotion bar. Without caching the absence, every arriving message would
// re-ask the registry for every model that does not exist — the expensive
// version of doing nothing.

// classifierCacheTTL is how long a per-label model (or its absence) is reused.
// Matches the priority scorer's: a model promoted by the daily pass becomes
// visible within minutes, which is soon enough for a decision this reversible.
const classifierCacheTTL = 5 * time.Minute

type cachedLabelModel struct {
	// model is nil when this label has no active model. That is a CACHED FACT,
	// not an empty slot — see the type comment.
	model    Model
	version  int
	loadedAt time.Time
}

// Classifier decides which of a user's labels apply to a message, and which it
// declines to judge.
type Classifier struct {
	reg *Registry

	mu    sync.Mutex
	cache map[string]cachedLabelModel // keyed user|label

	now func() time.Time
	ttl time.Duration
}

// NewClassifier builds a classifier over the model registry.
func NewClassifier(reg *Registry) *Classifier {
	return &Classifier{
		reg:   reg,
		cache: map[string]cachedLabelModel{},
		now:   time.Now,
		ttl:   classifierCacheTTL,
	}
}

// Classify judges one message against each of the given labels.
//
// Labels with no trained model are OMITTED entirely — not returned as
// abstentions. An abstention means "the model looked and is unsure", which is a
// question worth a human's time; "there is no model" is not a judgement at all,
// and queuing it would fill the review surface with every label the user has
// ever created, drowning the real uncertainty in noise.
//
// Errors are per-label and swallowed: this runs on the delivery path, and a
// failure to classify must never affect whether mail arrives.
func (c *Classifier) Classify(
	ctx context.Context,
	userID ulid.ULID,
	labelIDs []ulid.ULID,
	facts MessageFacts,
) map[ulid.ULID]LabelVerdict {
	if len(labelIDs) == 0 {
		return nil
	}
	features := featuresFromFacts(facts)

	out := map[ulid.ULID]LabelVerdict{}
	for _, labelID := range labelIDs {
		model, version, ok := c.modelFor(ctx, userID, labelID)
		if !ok || model == nil {
			continue
		}
		confidence := model.Score(features)
		v := Decide(confidence, TopContributions(model, features, topFeaturesN))
		v.ModelVersion = version
		out[labelID] = v
	}
	return out
}

// ModelVersion reports the active version for a label, or 0 when it has none.
// The suggestion rows stamp it so a stale suggestion is recognisable after a
// retrain.
func (c *Classifier) ModelVersion(ctx context.Context, userID, labelID ulid.ULID) int {
	_, version, ok := c.modelFor(ctx, userID, labelID)
	if !ok {
		return 0
	}
	return version
}

func (c *Classifier) modelFor(ctx context.Context, userID, labelID ulid.ULID) (Model, int, bool) {
	key := userID.String() + "|" + labelID.String()

	c.mu.Lock()
	if e, ok := c.cache[key]; ok && c.now().Sub(e.loadedAt) < c.ttl {
		m, v := e.model, e.version
		c.mu.Unlock()
		return m, v, true
	}
	c.mu.Unlock()

	// Outside the lock, as in Scorer: the lock guards the map, not a blob read.
	model, version, err := c.reg.ActiveModel(ctx, userID, KindLabel(labelID))
	if err != nil {
		// Do NOT cache an error as "no model": a transient registry failure would
		// then suppress a perfectly good classifier for the whole TTL.
		return nil, 0, false
	}

	c.mu.Lock()
	c.cache[key] = cachedLabelModel{model: model, version: version, loadedAt: c.now()}
	c.mu.Unlock()
	return model, version, true
}
