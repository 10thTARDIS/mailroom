// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
)

// LEARNED PLACEMENT'S STATUS LINE (LearnedPlacement/getStatus), shown in
// Settings: what learned placement did for the user lately, and which senders
// the user keeps filing by hand that it is NOT learning.
//
// COUNTS are arrival placements only (the ones made on new mail at ingest),
// decided in the last LearningStatusWindowDays. Backfilled placements are on
// old mail nobody revisits and would age into "kept" unseen, so they are left
// out, exactly as the correction gate leaves them out (backfilledMessagesSQL
// is the one definition of which is which).
//
// THE SENDER LIST is the rule suggestion's decision applied to every sender
// the user filed by hand at least RuleSuggestionMinFilings times into one
// area in the window: suggestionVerdict is the same code Area/ruleSuggestion
// runs, so a sender is listed here exactly when the move toast would offer a
// rule (it is not listed when learned placement will act, a rule covers it, or
// the user said "Not now").
//
// BOUNDED. At most LearningStatusMaxCandidates senders are evaluated (most
// filings first), because each evaluation may read one message's bytes. When
// that cap cuts candidates off, SendersTruncated says so: MoreSenders is then
// a lower bound, never read as a total.

const (
	// LearningStatusWindowDays bounds both the counts and the filings.
	LearningStatusWindowDays = RuleSuggestionWindowDays
	// LearningStatusMaxCandidates caps how many senders are evaluated.
	LearningStatusMaxCandidates = 25
	// LearningStatusMaxSenders caps how many are returned.
	LearningStatusMaxSenders = 5
)

// LearningSender is one sender the user keeps filing by hand.
type LearningSender struct {
	Address string
	AreaID  ulid.ULID
	// Reason is why learned placement is not doing it (RuleReason* or a
	// placement.Reason string).
	Reason  string
	Filings int
}

// LearningStatus is LearnedPlacement/getStatus's answer.
type LearningStatus struct {
	Enabled bool
	// Placed: arrival placements decided in the window, whatever became of
	// them. Corrected/Kept/Open are by effective outcome as of now (an open
	// placement past placement.KeptAfter reads as kept); a placement an API key
	// withdrew counts in Placed only.
	Placed, Corrected, Kept, Open int
	Senders                       []LearningSender
	// MoreSenders: listed-worthy senders beyond Senders. A lower bound when
	// SendersTruncated.
	MoreSenders      int
	SendersTruncated bool
}

// LearningStatus computes the status. It writes nothing. When the switch is
// off it returns Enabled=false with nothing else: no counts, no senders.
func (tx *UserTx) LearningStatus(ctx context.Context, senderFor RuleSuggestionSenderFunc) (LearningStatus, error) {
	var out LearningStatus
	on, err := tx.learnedEnabled(ctx)
	if err != nil {
		return out, err
	}
	out.Enabled = on
	if !on {
		return out, nil
	}
	if err := tx.learningCounts(ctx, &out); err != nil {
		return out, err
	}

	type candidate struct {
		areaID, labelID ulid.ULID
		addr            string
		filings         int
	}
	// One grouped query for the candidates. +1 row tells whether the cap cut.
	rows, err := tx.tx.Query(ctx, `
		SELECT a.id, a.label_id, lower(btrim(m.from_address::text)) AS addr, count(*) AS n
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		  JOIN app.messages m ON m.user_id = ml.user_id AND m.id = ml.message_id
		 WHERE ml.user_id = $1 AND ml.applied_by = $2
		   AND m.trashed_at IS NULL AND m.from_address IS NOT NULL
		   AND ml.created_at > now() - make_interval(days => $3)
		 GROUP BY a.id, a.label_id, addr
		HAVING count(*) >= $4
		 ORDER BY n DESC, addr, a.id
		 LIMIT $5`,
		tx.userID.String(), LabelByUser, LearningStatusWindowDays, RuleSuggestionMinFilings,
		LearningStatusMaxCandidates+1)
	if err != nil {
		return out, fmt.Errorf("store: learning status (candidates): %w", err)
	}
	var cands []candidate
	for rows.Next() {
		var aid, lid, addr string
		var n int
		if err := rows.Scan(&aid, &lid, &addr, &n); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: learning status (scan): %w", err)
		}
		a, err1 := ulid.Parse(aid)
		l, err2 := ulid.Parse(lid)
		if err1 != nil || err2 != nil {
			rows.Close()
			return out, fmt.Errorf("store: learning status: bad id %q/%q", aid, lid)
		}
		cands = append(cands, candidate{areaID: a, labelID: l, addr: addr, filings: n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: learning status (candidates): %w", err)
	}
	if len(cands) > LearningStatusMaxCandidates {
		cands = cands[:LearningStatusMaxCandidates]
		out.SendersTruncated = true
	}

	var listed []LearningSender
	for _, c := range cands {
		addrKey, domainKey, ok := placement.KeysFor(c.addr)
		if !ok {
			continue
		}
		reason, suggest, err := tx.suggestionVerdict(ctx, c.areaID, c.labelID, c.addr, addrKey, domainKey, senderFor)
		if err != nil {
			return out, err
		}
		if suggest {
			listed = append(listed, LearningSender{Address: c.addr, AreaID: c.areaID, Reason: reason, Filings: c.filings})
		}
	}
	if len(listed) > LearningStatusMaxSenders {
		out.MoreSenders = len(listed) - LearningStatusMaxSenders
		listed = listed[:LearningStatusMaxSenders]
	}
	out.Senders = listed
	return out, nil
}

// learningCounts fills the arrival-only counts for placements decided in the
// window.
func (tx *UserTx) learningCounts(ctx context.Context, out *LearningStatus) error {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+effectiveOutcomeSQL("$2")+`, count(*)
		  FROM app.learned_placements p
		  LEFT JOIN `+backfilledMessagesSQL("$1")+` bf ON bf.message_id = p.message_id
		 WHERE p.user_id = $1 AND bf.message_id IS NULL
		   AND p.decided_at > now() - make_interval(days => $3)
		 GROUP BY 1`, tx.userID.String(), keptAfterSeconds(), LearningStatusWindowDays)
	if err != nil {
		return fmt.Errorf("store: learning status (counts): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var outcome string
		var n int
		if err := rows.Scan(&outcome, &n); err != nil {
			return fmt.Errorf("store: learning status (scan counts): %w", err)
		}
		out.Placed += n
		switch outcome {
		case "open":
			out.Open += n
		case "kept":
			out.Kept += n
		case "corrected":
			out.Corrected += n
		}
	}
	return rows.Err()
}
