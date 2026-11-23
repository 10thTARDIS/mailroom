// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// ---- LearnedSender/get, forget, trust and LearnedSorting/status -------------
//
// "Learned sorting": see what it knows about a sender, make it forget, and tell
// it to believe one exact address without a DMARC pass (#167, #169). The
// decisions are the store's (LearnedSenderViewOf, ForgetSender, SetSenderTrusted,
// LearnedSortingStatus); this file parses and renders.
//
// Mailroom extensions under the desk capability, CLOSED TO API KEYS
// (agent_scopes.go): they read and change how the user's own mail is filed, and
// trust widens what learned sorting believes, so a key must never reach them.
// No change_log row: private per-user settings, not JMAP-visible objects.

const learnedSenderGetMax = 50

func learnedSenderSideJSON(s store.LearnedSenderSide, extra map[string]any) map[string]any {
	m := map[string]any{
		"areaId":  nil,
		"filings": s.Filings,
		"verdict": string(s.Verdict),
		"acting":  s.Acting,
		"reason":  strOrNullPtr(s.Reason),
	}
	if s.AreaID != nil {
		m["areaId"] = s.AreaID.String()
	}
	for k, v := range extra {
		m[k] = v
	}
	m["forgottenAt"] = jmapDateOrNil(s.ForgottenAt)
	return m
}

func strOrNullPtr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func learnedSenderJSON(v store.LearnedSenderView) map[string]any {
	return map[string]any{
		"sender":      v.Sender,
		"address":     learnedSenderSideJSON(v.Address, nil),
		"domain":      learnedSenderSideJSON(v.Domain, map[string]any{"domain": v.DomainName}),
		"decidedBy":   v.DecidedBy,
		"trusted":     v.Trusted,
		"trustUseful": v.TrustUseful,
		"forgottenAt": jmapDateOrNil(v.ForgottenAt),
	}
}

func (a *API) learnedSenderArgs(userID ulid.ULID, rawArgs json.RawMessage, into any, accountID func() string) *MethodError {
	if err := json.Unmarshal(rawArgs, into); err != nil {
		return &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	return resolveAccount(userID, accountID())
}

func senderMethodErr(err error) *MethodError {
	if errors.Is(err, store.ErrForgetInvalid) {
		return &MethodError{Type: errInvalidArguments, Description: "sender is not a usable email address"}
	}
	if errors.Is(err, store.ErrSenderInvalid) {
		return &MethodError{Type: errInvalidArguments, Description: "sender is not a usable email address (trusted senders must be plain ASCII)"}
	}
	return &MethodError{Type: errServerFail, Description: err.Error()}
}

func (a *API) learnedSenderGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string   `json:"accountId"`
		Senders   []string `json:"senders"`
	}
	if mErr := a.learnedSenderArgs(userID, rawArgs, &args, func() string { return args.AccountID }); mErr != nil {
		return nil, mErr
	}
	if len(args.Senders) < 1 || len(args.Senders) > learnedSenderGetMax {
		return nil, &MethodError{Type: errInvalidArguments, Description: "senders must hold 1 to 50 addresses"}
	}
	list := make([]map[string]any, 0, len(args.Senders))
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		seen := map[string]bool{}
		for _, s := range args.Senders {
			s = strings.ToLower(strings.TrimSpace(s))
			if seen[s] {
				continue
			}
			seen[s] = true
			v, err := tx.LearnedSenderViewOf(ctx, s)
			if errors.Is(err, store.ErrSenderInvalid) {
				// One input that cannot be keyed does not fail the batch.
				v, err = store.LearnedSenderEmpty(s), nil
			}
			if err != nil {
				return senderMethodErr(err)
			}
			list = append(list, learnedSenderJSON(v))
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "list": list, "notFound": []string{}}), nil
}

func (a *API) learnedSenderForget(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
		Sender    string `json:"sender"`
		Scope     string `json:"scope"`
	}
	if mErr := a.learnedSenderArgs(userID, rawArgs, &args, func() string { return args.AccountID }); mErr != nil {
		return nil, mErr
	}
	var kind placement.KeyKind
	switch args.Scope {
	case "address":
		kind = placement.KeyAddress
	case "domain":
		kind = placement.KeyDomain
	default:
		return nil, &MethodError{Type: errInvalidArguments, Description: "scope must be \"address\" or \"domain\""}
	}
	var view store.LearnedSenderView
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		addr, dom, ok := placement.KeysFor(args.Sender)
		if !ok {
			return senderMethodErr(store.ErrForgetInvalid)
		}
		key := addr
		if kind == placement.KeyDomain {
			key = dom
		}
		if _, err := tx.ForgetSender(ctx, key.Kind, key.Value); err != nil {
			return senderMethodErr(err)
		}
		if err := tx.AuditLearnedSender(ctx, store.LearnedSenderForget, addr.Value, args.Scope); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		var err error
		if view, err = tx.LearnedSenderViewOf(ctx, addr.Value); err != nil {
			return senderMethodErr(err)
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "forgotten": learnedSenderJSON(view)}), nil
}

func (a *API) learnedSenderTrust(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
		Sender    string `json:"sender"`
		Trusted   *bool  `json:"trusted"`
	}
	if mErr := a.learnedSenderArgs(userID, rawArgs, &args, func() string { return args.AccountID }); mErr != nil {
		return nil, mErr
	}
	if args.Trusted == nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "trusted must be true or false"}
	}
	var view store.LearnedSenderView
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		changed, err := tx.SetSenderTrusted(ctx, args.Sender, *args.Trusted)
		if err != nil {
			return senderMethodErr(err)
		}
		addr := strings.ToLower(strings.TrimSpace(args.Sender))
		if changed { // an idempotent repeat is not a new event
			action := store.LearnedSenderTrust
			if !*args.Trusted {
				action = store.LearnedSenderUntrust
			}
			if err := tx.AuditLearnedSender(ctx, action, addr, "address"); err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		if view, err = tx.LearnedSenderViewOf(ctx, addr); err != nil {
			return senderMethodErr(err)
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "updated": learnedSenderJSON(view)}), nil
}

func (a *API) learnedSortingStatus(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
	}
	if mErr := a.learnedSenderArgs(userID, rawArgs, &args, func() string { return args.AccountID }); mErr != nil {
		return nil, mErr
	}
	var st store.LearnedSortingStatus
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if st, err = tx.LearnedSortingStatus(ctx, time.Now()); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	num := func(f float64) any {
		if math.IsNaN(f) {
			return nil
		}
		return math.Round(f*1000) / 1000
	}
	areas := make([]map[string]any, 0, len(st.Areas))
	for _, ar := range st.Areas {
		areas = append(areas, map[string]any{"areaId": ar.AreaID.String(), "placed": ar.Placed, "corrected": ar.Corrected, "gate": ar.Gate})
	}
	var since any
	if !st.HeldBackSince.IsZero() {
		since = st.HeldBackSince.Format("2006-01-02")
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"enabled":   st.Enabled, "windowDays": store.LearnedSortingWindowDays,
		"placed": st.Placed, "corrected": st.Corrected,
		"precision": num(st.Precision), "lowerBound": num(st.LowerBound),
		"sendersLearned": map[string]int{"address": st.SendersLearnedAddress, "domain": st.SendersLearnedDomain},
		"heldBack":       st.HeldBack, "heldBackSince": since,
		"areas": areas,
	}), nil
}
