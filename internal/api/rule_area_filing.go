// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Filing existing mail into an area with a rule (#708 item 3) — the API half.
// The decisions live in store.PlanAreaFiling; this file is the wire shape both
// Rule/preview (what WOULD happen) and Rule/apply (what DID) report it in.

// ruleAreaFiling is the per-outcome report of filing mail into an area.
//
// Counts per outcome, never one total (CLAUDE.md, "report per-signal counts"):
// a single number cannot tell "40 will move" from "40 you already filed
// somewhere else", and the second is the one that should stop a person.
//
// On Rule/preview the counts cover every matched message in the scanned window
// and sum to `matched`. On Rule/apply they cover the matched mail the call
// actually processed: corrected mail (skippedCorrected) and mail past the
// mutation cap (remaining) are not in them.
type ruleAreaFiling struct {
	AreaID  string `json:"areaId"`
	LabelID string `json:"labelId"`
	// Move: not in the area now, and nothing but automation placement in the
	// way — on apply, moved.
	Move int `json:"move"`
	// AlreadyThere: already resolves to the area.
	AlreadyThere int `json:"alreadyThere"`
	// ContestedByUser: the user filed it in another area themselves. Left
	// there: their own filing wins over a rule.
	ContestedByUser int `json:"contestedByUser"`
	// ContestedByRule: another rule filed it in another area. Left there and
	// reported, rather than one rule silently undoing another.
	ContestedByRule int `json:"contestedByRule"`
	// ContestedOther: something else routes it to another area — a topic label
	// that feeds one, or a classifier/agent row. Left there.
	ContestedOther int `json:"contestedOther"`
}

func newRuleAreaFiling(t store.AreaFilingTarget) *ruleAreaFiling {
	return &ruleAreaFiling{AreaID: t.AreaID.String(), LabelID: t.LabelID.String()}
}

func (f *ruleAreaFiling) count(outcome string) {
	switch outcome {
	case store.AreaFilingMove:
		f.Move++
	case store.AreaFilingAlreadyThere:
		f.AlreadyThere++
	case store.AreaFilingContestedByUser:
		f.ContestedByUser++
	case store.AreaFilingContestedByRule:
		f.ContestedByRule++
	case store.AreaFilingContestedOther:
		f.ContestedOther++
	}
}

// withoutLabel is ids minus drop, as a NEW slice — the caller's update is
// reused across messages, so its backing array must never be written through.
func withoutLabel(ids []ulid.ULID, drop ulid.ULID) []ulid.ULID {
	out := make([]ulid.ULID, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// areaLabelsToRestore validates an undo's restoreAreaLabels against what an
// area-filing apply can have removed: a message the undo names, a label that is
// some area's own label, and a desk or setup attribution. Everything else is
// dropped, not trusted — the list comes back from the client, and without
// these checks an undo would be a way to put any label on any message under an
// automation's name.
func areaLabelsToRestore(
	ctx context.Context,
	tx *store.UserTx,
	ids []ulid.ULID,
	raw map[string][]ruleApplyRemovedLabel,
) (map[ulid.ULID][]store.RemovedAreaLabel, error) {
	out := map[ulid.ULID][]store.RemovedAreaLabel{}
	if len(raw) == 0 {
		return out, nil
	}
	named := make(map[ulid.ULID]bool, len(ids))
	for _, id := range ids {
		named[id] = true
	}
	var all []ulid.ULID
	for mid, list := range raw {
		m, err := ulid.Parse(mid)
		if err != nil || !named[m] {
			continue
		}
		for _, r := range list {
			if r.AppliedBy != store.LabelByDesk && r.AppliedBy != store.LabelBySetup {
				continue
			}
			l, err := ulid.Parse(r.LabelID)
			if err != nil {
				continue
			}
			out[m] = append(out[m], store.RemovedAreaLabel{LabelID: l, AppliedBy: r.AppliedBy})
			all = append(all, l)
		}
	}
	ok, err := tx.AreaLabelRestorable(ctx, all)
	if err != nil {
		return nil, err
	}
	for m, list := range out {
		kept := list[:0]
		for _, r := range list {
			if ok[r.LabelID] {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(out, m)
		} else {
			out[m] = kept
		}
	}
	return out, nil
}
