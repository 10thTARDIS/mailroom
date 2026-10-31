// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/store"
)

// senderKeyActionable is the store's rule (store.SenderKeyActionable) AND, for
// an address, the search parser's own: `from:<key>` must compile to an exact
// address term equal to the key. Defence in depth — no clean-up action builds
// a query from a key any more (SenderStats/emailIds enumerates the row's own
// messages) — but a key the parser would read as a domain is still never
// offered as one address.
func senderKeyActionable(g store.SenderGroup, key string) bool {
	if !store.SenderKeyActionable(g, key) {
		return false
	}
	if g == store.SenderByAddress {
		exact, ok := search.FromExactAddress(key)
		return ok && exact == key
	}
	return true
}

// SenderStats/get (#53) — the clean-up assistant's ranking: the caller's
// senders (or their domains, or their mailing lists) by volume or by neglect,
// in a window. Computed on demand, like Account/getHealth: a ranking is not a
// synced object, so it has no state and appends no change_log row. Every
// action the clean-up view offers (unsubscribe, archive, move, block) goes
// through the existing methods — this method only reads.
//
// "Neglect" is defined in store/sender_stats.go, once.
//
// Args: groupBy "address" (default) | "domain" | "list"; sort "volume"
// (default) | "neglect"; sinceDays a positive whole number of days, or null
// for all time; position and limit page the ranking (limit at most
// store.MaxSenderStatsPage). total is the number of groups in the window.

type senderStatsArgs struct {
	AccountID string `json:"accountId"`
	GroupBy   string `json:"groupBy"`
	Sort      string `json:"sort"`
	SinceDays *int   `json:"sinceDays"`
	Position  int    `json:"position"`
	Limit     *int   `json:"limit"`
}

type senderStatsResult struct {
	AccountID string `json:"accountId"`
	GroupBy   string `json:"groupBy"`
	Sort      string `json:"sort"`
	SinceDays *int   `json:"sinceDays"`
	Position  int    `json:"position"`
	Total     int    `json:"total"`
	// Since is the window's start the ranking used (UTCDate), or null for all
	// time. A client passes it back to SenderStats/emailIds so an action
	// enumerates the very window its row was counted in.
	Since any              `json:"since"`
	List  []map[string]any `json:"list"`
}

func senderStatObject(s store.SenderStat) map[string]any {
	addrs := make([]map[string]any, 0, len(s.Addresses))
	for _, a := range s.Addresses {
		var cid any
		if a.ContactID != nil {
			cid = a.ContactID.String()
		}
		addrs = append(addrs, map[string]any{
			"address":   a.Address,
			"contactId": cid,
			"name":      a.Name,
			"isVip":     a.VIP,
			"isBlocked": a.Blocked,
			// correspondent (#53 review): the user has written to this address.
			"correspondent": a.Correspondent,
		})
	}
	var unsub any
	if s.UnsubscribeMessageID != nil {
		unsub = s.UnsubscribeMessageID.String()
	}
	share := 0.0
	if s.Total > 0 {
		// Rounded to 3 places so the wire carries a share, not float noise.
		share = math.Round(float64(s.Neglected)/float64(s.Total)*1000) / 1000
	}
	return map[string]any{
		"key":                s.Key,
		"displayName":        s.DisplayName,
		"total":              s.Total,
		"unread":             s.Unread,
		"neglected":          s.Neglected,
		"neglectShare":       share,
		"lastReceivedAt":     jmapDate(s.LastReceived),
		"addressCount":       s.AddressCount,
		"addresses":          addrs,
		"unsubscribeEmailId": unsub,
		// actionable: the key searches for exactly this row
		// (store.SenderKeyActionable). The client offers no action otherwise.
		"actionable": s.Actionable,
	}
}

func parseSenderGroup(s string) (store.SenderGroup, *MethodError) {
	switch s {
	case "", "address":
		return store.SenderByAddress, nil
	case "domain":
		return store.SenderByDomain, nil
	case "list":
		return store.SenderByList, nil
	}
	return "", &MethodError{Type: errInvalidArguments, Description: "groupBy must be address, domain or list"}
}

// ---- SenderStats/emailIds -------------------------------------------------

// SenderStats/emailIds enumerates the messages ONE ranking row counted
// (store.SenderMessageIDs: the ranking's own predicate, filtered to the key),
// newest first, at most 1,000 per call with a position — the ids every
// clean-up action moves. Args: groupBy, key, since (the ranking's `since`, a
// UTCDate, or null for all time — passed back so both use one window),
// position, limit. A key that is not actionable is invalidArguments.
func (a *API) senderStatsEmailIDs(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string  `json:"accountId"`
		GroupBy   string  `json:"groupBy"`
		Key       string  `json:"key"`
		Since     *string `json:"since"`
		Position  int     `json:"position"`
		Limit     *int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	g, mErr := parseSenderGroup(args.GroupBy)
	if mErr != nil {
		return nil, mErr
	}
	if !senderKeyActionable(g, args.Key) {
		return nil, &MethodError{Type: errInvalidArguments, Description: "key does not name exactly one " + string(g)}
	}
	q := store.SenderIDsQuery{Group: g, Key: args.Key, Position: args.Position}
	if args.Since != nil {
		at, ok := jmapTime(*args.Since)
		if !ok {
			return nil, &MethodError{Type: errInvalidArguments, Description: "since must be a UTCDate or null"}
		}
		q.Since = &at
	}
	if args.Position < 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "position must be a non-negative integer"}
	}
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > store.MaxSenderIDsPage {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be between 1 and 1000"}
		}
		q.Limit = *args.Limit
	}
	var (
		ids   []ulid.ULID
		total int
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		ids, total, e = tx.SenderMessageIDs(ctx, q)
		return e
	}); err != nil {
		if errors.Is(err, store.ErrSenderStatsInvalid) {
			return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
		}
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"position":  args.Position,
		"total":     total,
		"ids":       out,
	}), nil
}

func (a *API) senderStatsGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args senderStatsArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	q := store.SenderStatsQuery{Group: store.SenderByAddress, Sort: store.SenderSortVolume, Position: args.Position}
	g, mErr := parseSenderGroup(args.GroupBy)
	if mErr != nil {
		return nil, mErr
	}
	q.Group = g
	switch args.Sort {
	case "", "volume":
	case "neglect":
		q.Sort = store.SenderSortNeglect
	default:
		return nil, &MethodError{Type: errInvalidArguments, Description: "sort must be volume or neglect"}
	}
	if args.SinceDays != nil {
		if *args.SinceDays <= 0 || *args.SinceDays > 36500 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "sinceDays must be a positive number of days, or null for all time"}
		}
		since := time.Now().UTC().Add(-time.Duration(*args.SinceDays) * 24 * time.Hour)
		q.Since = &since
	}
	if args.Position < 0 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "position must be a non-negative integer"}
	}
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > store.MaxSenderStatsPage {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be between 1 and 200"}
		}
		q.Limit = *args.Limit
	}

	var (
		stats []store.SenderStat
		total int
	)
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		stats, total, e = tx.SenderStats(ctx, q)
		return e
	}); err != nil {
		if errors.Is(err, store.ErrSenderStatsInvalid) {
			return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
		}
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	list := make([]map[string]any, 0, len(stats))
	for _, s := range stats {
		s.Actionable = s.Actionable && senderKeyActionable(q.Group, s.Key)
		list = append(list, senderStatObject(s))
	}
	var since any
	if q.Since != nil {
		since = jmapDate(*q.Since)
	}
	return mustMarshal(senderStatsResult{
		AccountID: userID.String(),
		GroupBy:   string(q.Group),
		Sort:      string(q.Sort),
		SinceDays: args.SinceDays,
		Position:  args.Position,
		Total:     total,
		Since:     since,
		List:      list,
	}), nil
}
