// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Re-scoring recent mail with the model now in service (#842).
//
// A revert to the cold start changes nothing a user can see by itself: every
// message keeps the score the collapsed model gave it at ingest, so the inbox
// stays empty of important mail until new mail arrives. `priority-rescore`
// closes that gap, report-first: the dry run says how many messages would
// change band and how many would become important, and --apply writes exactly
// those changes (the same SQL comparison decides "changed" in both modes).
// Idempotent: a second --apply finds nothing to change.

// RescoreResult is one rescore pass, per case.
type RescoreResult struct {
	Since   time.Time
	Model   string // the model_version the pass scored with
	Scanned int
	// Changed is how many stored rows differ from what the active model gives
	// today (score, version or explanation) — the rows --apply writes.
	Changed int
	// Band transitions at PriorityGateThreshold, on stored-precision scores.
	BecomeImportant int
	StopImportant   int
	StayImportant   int
	StayNot         int
	// PriorByModel counts the scanned messages by the model that scored them.
	PriorByModel map[string]int
	Before       [10]int
	After        [10]int
	Missing      int // scored rows whose message facts could not be read
	Applied      bool
	Written      int // rows actually changed by --apply (must equal Changed)
}

// RescorePriority re-scores messages received since `since` that carry a
// stored score, with the user's active model. apply=false writes nothing.
func RescorePriority(ctx context.Context, st *store.Store, reg *Registry, userID ulid.ULID, since time.Time, apply bool) (RescoreResult, error) {
	res := RescoreResult{Since: since, PriorByModel: map[string]int{}, Applied: apply}
	scorer := NewScorer(reg)
	_, version, err := reg.ActiveModel(ctx, userID, KindPriority)
	if err != nil {
		return res, fmt.Errorf("ml: rescore: active model: %w", err)
	}
	res.Model = modelVersionString(version)

	after := ulid.ULID{}
	for {
		var page []store.ScoredMessage
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.ScoredMessagesSince(ctx, since, after, 500)
			return e
		}); err != nil {
			return res, fmt.Errorf("ml: rescore: scan: %w", err)
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].ID

		// Facts are read and scored OUTSIDE the writing transaction, then the
		// whole page is compared (and, under --apply, written) in one UserTx.
		type scored struct {
			msg   store.ScoredMessage
			score float64
			ver   string
			top   []byte
		}
		var batch []scored
		for _, sm := range page {
			var facts MessageFacts
			var found bool
			if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
				row, ok, e := tx.MessageMLFacts(ctx, sm.ID)
				if e != nil || !ok {
					return e
				}
				out, in, e := tx.OutboundCountForAddress(ctx, row.FromAddress)
				if e != nil {
					return e
				}
				facts, found = factsFromRow(row, out, in), true
				return nil
			}); err != nil {
				return res, fmt.Errorf("ml: rescore: facts: %w", err)
			}
			if !found {
				res.Missing++
				continue
			}
			score, ver, top, err := scorer.ScoreMessage(ctx, userID, facts)
			if err != nil {
				return res, fmt.Errorf("ml: rescore: score: %w", err)
			}
			batch = append(batch, scored{msg: sm, score: score, ver: ver, top: top})
		}

		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			for _, b := range batch {
				res.Scanned++
				res.PriorByModel[b.msg.ModelVersion]++
				before, now := b.msg.Score, store.StoredScore(b.score)
				res.Before[bucket(before)]++
				res.After[bucket(now)]++
				wasImp, isImp := before >= PriorityGateThreshold, now >= PriorityGateThreshold
				switch {
				case !wasImp && isImp:
					res.BecomeImportant++
				case wasImp && !isImp:
					res.StopImportant++
				case wasImp:
					res.StayImportant++
				default:
					res.StayNot++
				}
				changed, err := tx.TriageScoreWouldChange(ctx, b.msg.ID, b.score, b.ver, b.top)
				if err != nil {
					return err
				}
				if changed {
					res.Changed++
				}
				if apply {
					wrote, err := tx.RescoreTriage(ctx, b.msg.ID, b.score, b.ver, b.top)
					if err != nil {
						return err
					}
					if wrote {
						res.Written++
					}
				}
			}
			return nil
		}); err != nil {
			return res, fmt.Errorf("ml: rescore: compare/write: %w", err)
		}
		if len(page) < 500 {
			break
		}
	}
	return res, nil
}

func bucket(score float64) int {
	return max(0, min(9, int(score*10)))
}
