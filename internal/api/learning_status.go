// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// LearnedPlacement/getStatus: what learned placement did lately and which
// senders the user keeps filing by hand that it is not learning. The decision
// is the store's (LearningStatus); this file parses and renders. A Mailroom
// extension under the desk capability, closed to API keys.

func (a *API) learnedPlacementGetStatus(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var res store.LearningStatus
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		res, err = tx.LearningStatus(ctx, a.ruleSuggestionSender(userID))
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	senders := make([]map[string]any, 0, len(res.Senders))
	for _, s := range res.Senders {
		senders = append(senders, map[string]any{
			"address": s.Address, "areaId": s.AreaID.String(), "reason": s.Reason, "filings": s.Filings,
		})
	}
	return mustMarshal(map[string]any{
		"accountId":        userID.String(),
		"enabled":          res.Enabled,
		"placed":           res.Placed,
		"corrected":        res.Corrected,
		"kept":             res.Kept,
		"open":             res.Open,
		"senders":          senders,
		"moreSenders":      res.MoreSenders,
		"sendersTruncated": res.SendersTruncated,
	}), nil
}
