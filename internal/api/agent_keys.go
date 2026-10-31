// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// ApiKey/get, ApiKey/set and ApiKey/getEvents (#49, ADR-042 §1). SESSION ONLY:
// the scope table refuses them to every key, and each refuses a key again
// itself, so a key can never create, widen, resume or read keys.
//
// The token appears exactly once: in ApiKey/set's `created` entry. It is never
// stored, logged or returned again.

// Defaults and caps for a key's limits (ADR-042 §5). Mandatory: there is no
// "unlimited". The caps are the store's CHECK bounds.
const (
	DefaultKeyRatePerMinute = 60
	MaxKeyRatePerMinute     = 600
	DefaultKeyDailyBudget   = 500
	MaxKeyDailyBudget       = 20000
	// DefaultSuspendOnAnomaly is a new key's suspendOnAnomaly when the create
	// does not say (#49), and what the session capability advertises.
	DefaultSuspendOnAnomaly = true
	maxKeyNameRunes         = 100
)

func keyObject(k store.APIKey, usage *store.APIKeyUsage) map[string]any {
	var accounts any
	if k.AccountIDs != nil {
		accounts = ulidStringsAPI(k.AccountIDs)
	}
	obj := map[string]any{
		"id":               k.ID.String(),
		"name":             k.Name,
		"tokenHint":        auth.APITokenPrefix + k.TokenHint,
		"scopes":           k.Scopes,
		"accountIds":       accounts,
		"labelIds":         nil,
		"confirmScopes":    k.ConfirmScopes,
		"ratePerMinute":    k.RatePerMin,
		"dailyBudget":      k.DailyBudget,
		"suspendOnAnomaly": k.SuspendOnAnomaly,
		"createdAt":        jmapDate(k.CreatedAt),
		"lastUsedAt":       jmapDatePtr(k.LastUsedAt),
		"revokedAt":        jmapDatePtr(k.RevokedAt),
		"suspendedAt":      jmapDatePtr(k.SuspendedAt),
		"suspendedReason":  nullIfEmpty(k.SuspendedReason),
	}
	if usage != nil {
		obj["usedLastMinute"] = usage.CostLastMinute
		obj["usedLast24h"] = usage.CostLast24h
	}
	return obj
}

func ulidStringsAPI(ids []ulid.ULID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func sessionOnlyGuard(ctx context.Context) *MethodError {
	if principalFrom(ctx) != nil {
		return forbidden(whyKeyManagement)
	}
	return nil
}

func (a *API) apiKeyGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if mErr := sessionOnlyGuard(ctx); mErr != nil {
		return nil, mErr
	}
	var args struct {
		AccountID string    `json:"accountId"`
		IDs       *[]string `json:"ids"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var want map[string]bool
	if args.IDs != nil {
		want = map[string]bool{}
		for _, id := range *args.IDs {
			want[id] = true
		}
	}
	list := []map[string]any{}
	seen := map[string]bool{}
	now := time.Now() // the events' clock; see admitKeyCall
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		keys, err := tx.ListAPIKeys(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, k := range keys {
			if want != nil && !want[k.ID.String()] {
				continue
			}
			u, err := tx.APIKeyUsageAt(ctx, k.ID, now, MassDeleteWindow, MassSendWindow)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			list = append(list, keyObject(k, &u))
			seen[k.ID.String()] = true
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	notFound := []string{}
	if args.IDs != nil {
		for _, id := range *args.IDs {
			if !seen[id] {
				notFound = append(notFound, id)
			}
		}
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "state": "0", "list": list,
		"notFound": notFound}), nil
}

// keySettings is the writable part of a key, as create and update read it.
type keySettings struct {
	Name                     *string          `json:"name"`
	Scopes                   *[]string        `json:"scopes"`
	AccountIDs               *json.RawMessage `json:"accountIds"`
	LabelIDs                 *json.RawMessage `json:"labelIds"`
	ConfirmScopes            *[]string        `json:"confirmScopes"`
	RatePerMinute            *int             `json:"ratePerMinute"`
	DailyBudget              *int             `json:"dailyBudget"`
	SuspendOnAnomaly         *bool            `json:"suspendOnAnomaly"`
	Suspended                *bool            `json:"suspended"`
	AcknowledgeSendAndDelete bool             `json:"acknowledgeSendAndDelete"`
}

var keySettingNames = map[string]bool{"name": true, "scopes": true, "accountIds": true, "labelIds": true,
	"confirmScopes": true, "ratePerMinute": true, "dailyBudget": true, "suspendOnAnomaly": true,
	"suspended": true, "acknowledgeSendAndDelete": true}

func decodeKeySettings(raw json.RawMessage) (keySettings, *SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return keySettings{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	for name := range fields {
		if !keySettingNames[name] {
			return keySettings{}, &SetError{Type: setInvalidProps, Description: "unknown ApiKey property " + name,
				Properties: []string{name}}
		}
	}
	var s keySettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return keySettings{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	return s, nil
}

func invalidKey(prop, desc string) *SetError {
	return &SetError{Type: setInvalidProps, Description: desc, Properties: []string{prop}}
}

// applyKeySettings writes s onto k (a new key's defaults, or the current key),
// validating everything. prev is the key before the change (nil on create):
// granting send AND delete together — newly, by create or by update — needs
// acknowledgeSendAndDelete in the same request (spec §13a), and a destructive
// scope newly granted without confirmScopes starts in confirm mode.
func applyKeySettings(ctx context.Context, tx *store.UserTx, k *store.APIKey, s keySettings, prev *store.APIKey) *SetError {
	if s.Name != nil {
		name := strings.TrimSpace(*s.Name)
		if name == "" || len([]rune(name)) > maxKeyNameRunes || strings.IndexFunc(name, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
		}) >= 0 {
			return invalidKey("name", "a key needs a name of 1 to 100 printable characters")
		}
		k.Name = name
	}
	if s.Scopes != nil {
		seen := map[string]bool{}
		var scopes []string
		for _, sc := range *s.Scopes {
			if !isKnownScope(sc) {
				return invalidKey("scopes", "unknown scope "+sc+"; the scopes are "+strings.Join(allScopes, ", "))
			}
			if !seen[sc] {
				seen[sc] = true
				scopes = append(scopes, sc)
			}
		}
		if len(scopes) == 0 {
			return invalidKey("scopes", "a key needs at least one scope")
		}
		sort.Strings(scopes)
		k.Scopes = scopes
	}
	if k.Name == "" {
		return invalidKey("name", "a key needs a name")
	}
	if len(k.Scopes) == 0 {
		return invalidKey("scopes", "a key needs at least one scope")
	}
	has := func(set []string, sc string) bool { return slices.Contains(set, sc) }
	both := has(k.Scopes, scopeMailSend) && has(k.Scopes, scopeMailDelete)
	hadBoth := prev != nil && has(prev.Scopes, scopeMailSend) && has(prev.Scopes, scopeMailDelete)
	if both && !hadBoth && !s.AcknowledgeSendAndDelete {
		return invalidKey("scopes", "a key that can both send and delete mail needs acknowledgeSendAndDelete: "+
			"an agent with both can mail anyone as you and then remove the trace")
	}

	if s.LabelIDs != nil && strings.TrimSpace(string(*s.LabelIDs)) != "null" {
		var ids []string
		if err := json.Unmarshal(*s.LabelIDs, &ids); err != nil || len(ids) > 0 {
			return invalidKey("labelIds", "restricting a key to labels is not supported yet; leave labelIds null")
		}
	}
	if s.AccountIDs != nil {
		if strings.TrimSpace(string(*s.AccountIDs)) == "null" {
			k.AccountIDs = nil
		} else {
			var raw []string
			if err := json.Unmarshal(*s.AccountIDs, &raw); err != nil {
				return invalidKey("accountIds", "accountIds is null (every account) or a list of account ids")
			}
			if len(raw) == 0 {
				return invalidKey("accountIds", "a key restricted to no account could do nothing; use null for every account")
			}
			accounts, err := tx.ListAccounts(ctx)
			if err != nil {
				return &SetError{Type: errServerFail, Description: err.Error()}
			}
			mine := map[string]ulid.ULID{}
			for _, acct := range accounts {
				mine[acct.ID.String()] = acct.ID
			}
			var ids []ulid.ULID
			for _, r := range raw {
				id, ok := mine[r]
				if !ok {
					return invalidKey("accountIds", "no such account: "+r)
				}
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
			k.AccountIDs = ids
		}
	}

	if s.ConfirmScopes != nil {
		var confirm []string
		for _, sc := range *s.ConfirmScopes {
			if !isDestructiveScope(sc) {
				return invalidKey("confirmScopes", "only mail:send and mail:delete can wait for approval")
			}
			if has(k.Scopes, sc) && !has(confirm, sc) {
				confirm = append(confirm, sc)
			}
		}
		k.ConfirmScopes = confirm
	} else {
		// A destructive scope this request newly grants starts by asking.
		confirm := []string{}
		for _, sc := range destructiveScopes {
			if !has(k.Scopes, sc) {
				continue
			}
			if prev == nil || !has(prev.Scopes, sc) || has(prev.ConfirmScopes, sc) {
				confirm = append(confirm, sc)
			}
		}
		k.ConfirmScopes = confirm
	}
	sort.Strings(k.ConfirmScopes)

	if s.RatePerMinute != nil {
		if *s.RatePerMinute < 1 || *s.RatePerMinute > MaxKeyRatePerMinute {
			return invalidKey("ratePerMinute", "ratePerMinute must be between 1 and 600")
		}
		k.RatePerMin = *s.RatePerMinute
	}
	if s.DailyBudget != nil {
		if *s.DailyBudget < 1 || *s.DailyBudget > MaxKeyDailyBudget {
			return invalidKey("dailyBudget", "dailyBudget must be between 1 and 20000")
		}
		k.DailyBudget = *s.DailyBudget
	}
	if s.SuspendOnAnomaly != nil {
		k.SuspendOnAnomaly = *s.SuspendOnAnomaly
	}
	if s.Suspended != nil {
		switch {
		case *s.Suspended && k.SuspendedAt == nil:
			now := time.Now()
			k.SuspendedAt, k.SuspendedReason = &now, "suspended by you"
		case !*s.Suspended:
			k.SuspendedAt, k.SuspendedReason = nil, ""
		}
	}
	return nil
}

func keyAuditDetail(k store.APIKey) map[string]any {
	return map[string]any{"name": k.Name, "scopes": k.Scopes, "accountIds": ulidStringsAPI(k.AccountIDs),
		"restricted": k.AccountIDs != nil, "confirmScopes": k.ConfirmScopes, "ratePerMinute": k.RatePerMin,
		"dailyBudget": k.DailyBudget, "suspendOnAnomaly": k.SuspendOnAnomaly, "suspended": k.SuspendedAt != nil}
}

func (a *API) apiKeySet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if mErr := sessionOnlyGuard(ctx); mErr != nil {
		return nil, mErr
	}
	var args emailSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	res := newSetResult(userID)
	res.OldState, res.NewState = "0", "0"
	var revoked []ulid.ULID
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		for cid, raw := range args.Create {
			obj, serr, err := a.createKey(ctx, tx, raw)
			if err != nil {
				return err
			}
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			res.Created[cid] = obj
		}
		for id, raw := range args.Update {
			serr, err := updateKey(ctx, tx, id, raw)
			if err != nil {
				return err
			}
			if serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}
		for _, id := range args.Destroy {
			kid, perr := ulid.Parse(id)
			if perr != nil {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, err := tx.RevokeAPIKey(ctx, kid)
			if err != nil {
				return err
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			if err := tx.AuditAPIKeyLifecycle(ctx, "api_key.revoke", kid, map[string]any{}); err != nil {
				return err
			}
			res.Destroyed = append(res.Destroyed, id)
			revoked = append(revoked, kid)
		}
		return nil
	})
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	// After the commit: a revoked key's HTTP bucket is dropped.
	for _, kid := range revoked {
		a.limiter.ForgetKey(userID, kid)
	}
	return mustMarshal(res), nil
}

// MaxActiveKeysPerUser bounds a user's unrevoked keys (#49 review): with a
// per-key HTTP bucket, a key is also a unit of throughput, and the
// aggregate bucket bounds that only if the number of keys is bounded too.
const MaxActiveKeysPerUser = 20

// createKey mints the token, stores its hash and returns the key WITH the
// token — the only place the token ever leaves the server.
func (a *API) createKey(ctx context.Context, tx *store.UserTx, raw json.RawMessage) (map[string]any, *SetError, error) {
	s, serr := decodeKeySettings(raw)
	if serr != nil {
		return nil, serr, nil
	}
	if s.Suspended != nil && *s.Suspended {
		return nil, invalidKey("suspended", "a new key cannot start suspended"), nil
	}
	n, err := tx.LockAndCountUnrevokedAPIKeys(ctx)
	if err != nil {
		return nil, nil, err
	}
	if n >= MaxActiveKeysPerUser {
		return nil, &SetError{Type: setForbidden, Description: fmt.Sprintf(
			"you already have %d active API keys, the most allowed; revoke one you no longer use first", MaxActiveKeysPerUser)}, nil
	}
	k := store.APIKey{RatePerMin: DefaultKeyRatePerMinute, DailyBudget: DefaultKeyDailyBudget, SuspendOnAnomaly: DefaultSuspendOnAnomaly}
	if serr := applyKeySettings(ctx, tx, &k, s, nil); serr != nil {
		return nil, serr, nil
	}
	token, hash, hint, err := auth.NewAPIToken()
	if err != nil {
		return nil, nil, err
	}
	created, err := tx.CreateAPIKey(ctx, store.NewAPIKey{Name: k.Name, TokenHash: hash, TokenHint: hint,
		Scopes: k.Scopes, AccountIDs: k.AccountIDs, ConfirmScopes: k.ConfirmScopes, RatePerMin: k.RatePerMin,
		DailyBudget: k.DailyBudget, SuspendOnAnomaly: k.SuspendOnAnomaly})
	if errors.Is(err, store.ErrAPIKeyNameTaken) {
		return nil, &SetError{Type: "alreadyExists", Description: "you already have a key with this name",
			Properties: []string{"name"}}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := tx.AuditAPIKeyLifecycle(ctx, "api_key.create", created.ID, keyAuditDetail(created)); err != nil {
		return nil, nil, err
	}
	obj := keyObject(created, nil)
	obj["token"] = token
	return obj, nil, nil
}

func updateKey(ctx context.Context, tx *store.UserTx, id string, raw json.RawMessage) (*SetError, error) {
	kid, err := ulid.Parse(id)
	if err != nil {
		return &SetError{Type: setNotFound}, nil
	}
	s, serr := decodeKeySettings(raw)
	if serr != nil {
		return serr, nil
	}
	k, found, err := tx.LockAPIKey(ctx, kid)
	if err != nil {
		return nil, err
	}
	if !found {
		return &SetError{Type: setNotFound}, nil
	}
	if k.RevokedAt != nil {
		return &SetError{Type: setForbidden, Description: "a revoked key cannot be changed; create a new one"}, nil
	}
	prev := k
	prev.Scopes = slices.Clone(k.Scopes)
	prev.ConfirmScopes = slices.Clone(k.ConfirmScopes)
	if s.Scopes == nil && s.ConfirmScopes == nil {
		// Nothing about scopes changes: keep the confirm set exactly as it is.
		cs := k.ConfirmScopes
		s.ConfirmScopes = &cs
	}
	if serr := applyKeySettings(ctx, tx, &k, s, &prev); serr != nil {
		return serr, nil
	}
	if err := tx.UpdateAPIKey(ctx, k); err != nil {
		if errors.Is(err, store.ErrAPIKeyNameTaken) {
			return &SetError{Type: "alreadyExists", Description: "you already have a key with this name",
				Properties: []string{"name"}}, nil
		}
		return nil, err
	}
	if err := tx.AuditAPIKeyLifecycle(ctx, "api_key.update", k.ID, keyAuditDetail(k)); err != nil {
		return nil, err
	}
	// REACTIVATION is its own audit row (#49): the one-click way back for
	// a key an anomaly rule suspended is a person's decision about that
	// suspension, and the audit says which suspension it ended. Session-only,
	// like every ApiKey/set (sessionOnlyGuard).
	if prev.SuspendedAt != nil && k.SuspendedAt == nil {
		return nil, tx.AuditAPIKeyLifecycle(ctx, "api_key.reactivate", k.ID, map[string]any{
			"suspendedAt": prev.SuspendedAt.UTC().Format(time.RFC3339), "suspendedReason": prev.SuspendedReason})
	}
	return nil, nil
}

// apiKeyGetEvents is a key's activity: {keyId?, before?, limit?}, newest first.
func (a *API) apiKeyGetEvents(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if mErr := sessionOnlyGuard(ctx); mErr != nil {
		return nil, mErr
	}
	var args struct {
		AccountID string  `json:"accountId"`
		KeyID     *string `json:"keyId"`
		Before    *string `json:"before"`
		Limit     *int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	limit := 50
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > 200 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be between 1 and 200"}
		}
		limit = *args.Limit
	}
	parse := func(s *string) (*ulid.ULID, *MethodError) {
		if s == nil {
			return nil, nil
		}
		id, err := ulid.Parse(*s)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "not an id: " + *s}
		}
		return &id, nil
	}
	keyID, mErr := parse(args.KeyID)
	if mErr != nil {
		return nil, mErr
	}
	before, mErr := parse(args.Before)
	if mErr != nil {
		return nil, mErr
	}
	var events []store.APIKeyEvent
	var suppressed []store.SuppressedRefusal
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if events, err = tx.ListAPIKeyEvents(ctx, keyID, before, limit); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if suppressed, err = tx.ListSuppressedRefusals(ctx, keyID, limit); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	list := make([]map[string]any, 0, len(events))
	for _, e := range events {
		var pending any
		if e.PendingID != nil {
			pending = e.PendingID.String()
		}
		list = append(list, map[string]any{
			"id": e.ID.String(), "keyId": e.KeyID.String(), "at": jmapDate(e.At), "method": e.Method,
			"outcome": e.Outcome, "cost": e.Cost, "deletes": e.Deletes, "sends": e.Sends,
			"summary": e.Summary, "clientIp": nullIfEmpty(e.ClientIP), "pendingActionId": pending,
			"errorType": nullIfEmpty(e.ErrorType), "anomaly": nullIfEmpty(e.Anomaly),
		})
	}
	// Routine refusals past a key's per-minute cap, counted rather than
	// listed one by one (#49 review): the log is bounded, not lossy.
	counted := make([]map[string]any, 0, len(suppressed))
	for _, s := range suppressed {
		counted = append(counted, map[string]any{"keyId": s.KeyID.String(), "minute": jmapDate(s.Minute),
			"outcome": s.Outcome, "method": s.Method, "reason": s.Reason, "count": s.Count})
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "list": list, "suppressedRefusals": counted}), nil
}

// apiKeysCapability is the urn:mailroom:apikeys session object: what a client
// building a key needs to know, stated by the server rather than assumed.
func apiKeysCapability() map[string]any {
	return map[string]any{
		"scopes":                    allScopes,
		"confirmableScopes":         destructiveScopes,
		"defaultRatePerMinute":      DefaultKeyRatePerMinute,
		"maxRatePerMinute":          MaxKeyRatePerMinute,
		"defaultDailyBudget":        DefaultKeyDailyBudget,
		"maxDailyBudget":            MaxKeyDailyBudget,
		"approvalWindowSeconds":     int(PendingApprovalWindow.Seconds()),
		"labelRestrictionSupported": false,
		"massDeleteThreshold":       MassDeleteThreshold,
		"massDeleteWindowSeconds":   int(MassDeleteWindow.Seconds()),
		"massSendThreshold":         MassSendThreshold,
		"massSendWindowSeconds":     int(MassSendWindow.Seconds()),
		"readUnitObjects":           ReadUnitObjects,
		"defaultSuspendOnAnomaly":   DefaultSuspendOnAnomaly,
	}
}
