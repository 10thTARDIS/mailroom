// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/mime"
	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// ---- Area/ruleSuggestion and Area/dismissRuleSuggestion ---------------------
//
// "Always put mail from X in <area>?": after the user files mail from one
// sender into an area by hand, the client asks whether to offer a rule. The
// decision is the store's (RuleSuggestion): this file only parses, resolves
// the one thing the store cannot (the sender of the address's latest message,
// from its bytes) and renders.
//
// Both are Mailroom extensions under the desk capability, closed to API keys:
// they read and write the user's own desk arrangement, like Area/set.

type areaRuleSuggestionArgs struct {
	AccountID string `json:"accountId"`
	AreaID    string `json:"areaId"`
	Address   string `json:"address"`
}

func (a *API) parseRuleSuggestionArgs(userID ulid.ULID, rawArgs json.RawMessage) (areaRuleSuggestionArgs, ulid.ULID, *MethodError) {
	var args areaRuleSuggestionArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return args, ulid.ULID{}, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return args, ulid.ULID{}, mErr
	}
	areaID, err := ulid.Parse(args.AreaID)
	if err != nil {
		return args, ulid.ULID{}, &MethodError{Type: errInvalidArguments, Description: "areaId is not a valid id"}
	}
	if args.Address == "" {
		return args, ulid.ULID{}, &MethodError{Type: errInvalidArguments, Description: "address is required"}
	}
	return args, areaID, nil
}

// ruleSuggestionSender builds the placement.Sender for a stored message the way
// ingest does (ingest.LearnedSender), from its bytes in the blob store.
func (a *API) ruleSuggestionSender(userID ulid.ULID) store.RuleSuggestionSenderFunc {
	return func(ctx context.Context, c store.ExplainCandidate) (placement.Sender, error) {
		if a.raw == nil {
			return placement.Sender{}, fmt.Errorf("api: no raw message source")
		}
		raw, err := a.raw.ReadRaw(ctx, userID, c.ContentHash)
		if err != nil {
			return placement.Sender{}, err
		}
		parsed, err := mime.ParseBytes(raw)
		if err != nil || parsed == nil {
			return placement.Sender{}, fmt.Errorf("api: message unparseable: %v", err)
		}
		return ingest.LearnedSender(c.From, parsed, a.trustedAuthservIDs, c.ReceivedAt), nil
	}
}

func (a *API) areaRuleSuggestion(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	args, areaID, mErr := a.parseRuleSuggestionArgs(userID, rawArgs)
	if mErr != nil {
		return nil, mErr
	}
	var res store.RuleSuggestion
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		res, err = tx.RuleSuggestion(ctx, areaID, args.Address, a.ruleSuggestionSender(userID))
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"suggest":   res.Suggest,
		"reason":    res.Reason,
		"address":   res.Address,
		"areaId":    res.AreaID.String(),
		"filings":   res.Filings,
		"earlier":   res.Earlier,
	}), nil
}

func (a *API) areaDismissRuleSuggestion(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	args, areaID, mErr := a.parseRuleSuggestionArgs(userID, rawArgs)
	if mErr != nil {
		return nil, mErr
	}
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		found, err := tx.DismissRuleSuggestion(ctx, areaID, args.Address)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			return &MethodError{Type: errInvalidArguments, Description: "no such area, or the address is not usable"}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{"accountId": userID.String()}), nil
}
