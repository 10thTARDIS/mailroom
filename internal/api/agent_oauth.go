// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/oauthserver"
	"mailroom/internal/store"
)

// OAUTH GRANTS AND CLIENTS (ADR-050). A grant is an API key row of kind
// 'oauth', written by the SAME validation and audit ApiKey/set uses
// (applyKeySettings, keyAuditDetail): the consent screen is the key form, so
// there is no second enforcement path.

// OAuthPolicy is the key policy the consent screen presents, from the same
// constants ApiKey/set enforces.
func OAuthPolicy() oauthserver.Policy {
	return oauthserver.Policy{Scopes: allScopes, Destructive: destructiveScopes,
		DefaultRate: DefaultKeyRatePerMinute, MaxRate: MaxKeyRatePerMinute,
		DefaultBudget: DefaultKeyDailyBudget, MaxBudget: MaxKeyDailyBudget,
		DefaultSuspend: DefaultSuspendOnAnomaly,
		// The minimal useful set advertised in discovery and asked for by a
		// request that names no scope (ADR-050 section 2).
		DefaultScopes: []string{scopeMailRead, scopeSearchRead}}
}

// ApproveOAuthGrant creates or updates the grant for the signed-in user inside
// tx. refusal is a user-facing reason (nothing was written); err is a fault.
func ApproveOAuthGrant(ctx context.Context, tx *store.UserTx, in oauthserver.GrantApproval) (store.APIKey, string, error) {
	scopes, confirm := in.Scopes, in.ConfirmScopes
	s := keySettings{Scopes: &scopes, ConfirmScopes: &confirm, RatePerMinute: &in.RatePerMinute,
		DailyBudget: &in.DailyBudget, SuspendOnAnomaly: &in.SuspendOnAnomaly,
		AcknowledgeSendAndDelete: in.AcknowledgeSendAndDelete}
	var accounts json.RawMessage = []byte("null")
	if in.AccountIDs != nil {
		b, _ := json.Marshal(in.AccountIDs)
		accounts = b
	}
	s.AccountIDs = &accounts

	k, found, err := tx.LockOAuthGrantForClient(ctx, in.ClientID)
	if err != nil {
		return store.APIKey{}, "", err
	}
	if found {
		prev := k
		prev.Scopes = append([]string(nil), k.Scopes...)
		prev.ConfirmScopes = append([]string(nil), k.ConfirmScopes...)
		if serr := applyKeySettings(ctx, tx, &k, s, &prev); serr != nil {
			return store.APIKey{}, serr.Description, nil
		}
		if err := tx.UpdateAPIKey(ctx, k); err != nil {
			return store.APIKey{}, "", err
		}
		if err := tx.UpdateOAuthGrantClient(ctx, k.ID, in.ClientName, in.ClientKind, in.RedirectHost); err != nil {
			return store.APIKey{}, "", err
		}
		d := keyAuditDetail(k)
		d["kind"], d["client"] = "oauth", oauthAuditClient(in)
		return k, "", tx.AuditAPIKeyLifecycle(ctx, "api_key.update", k.ID, d)
	}

	n, err := tx.LockAndCountUnrevokedAPIKeys(ctx)
	if err != nil {
		return store.APIKey{}, "", err
	}
	if n >= MaxActiveKeysPerUser {
		return store.APIKey{}, fmt.Sprintf("You already have %d active API keys and connected apps, the most allowed. "+
			"Revoke one you no longer use first.", MaxActiveKeysPerUser), nil
	}
	names, err := tx.APIKeyNames(ctx)
	if err != nil {
		return store.APIKey{}, "", err
	}
	name := uniqueKeyName(oauthKeyName(in.ClientName), names)
	s.Name = &name
	nk := store.APIKey{RatePerMin: DefaultKeyRatePerMinute, DailyBudget: DefaultKeyDailyBudget, SuspendOnAnomaly: DefaultSuspendOnAnomaly}
	if serr := applyKeySettings(ctx, tx, &nk, s, nil); serr != nil {
		return store.APIKey{}, serr.Description, nil
	}
	created, err := tx.CreateClientGrant(ctx, store.NewClientGrant{Name: nk.Name, ClientID: in.ClientID,
		ClientName: in.ClientName, ClientKind: in.ClientKind, RedirectHost: in.RedirectHost, Scopes: nk.Scopes,
		AccountIDs: nk.AccountIDs, ConfirmScopes: nk.ConfirmScopes, RatePerMin: nk.RatePerMin,
		DailyBudget: nk.DailyBudget, SuspendOnAnomaly: nk.SuspendOnAnomaly})
	if err != nil {
		if errors.Is(err, store.ErrOAuthClientGrantExists) || errors.Is(err, store.ErrAPIKeyNameTaken) {
			return store.APIKey{}, "This app was just connected in another window. Start again from the app.", nil
		}
		return store.APIKey{}, "", err
	}
	d := keyAuditDetail(created)
	d["kind"], d["client"] = "oauth", oauthAuditClient(in)
	return created, "", tx.AuditAPIKeyLifecycle(ctx, "api_key.create", created.ID, d)
}

func oauthAuditClient(in oauthserver.GrantApproval) map[string]any {
	return map[string]any{"id": in.ClientID, "name": in.ClientName, "kind": in.ClientKind, "redirectHost": in.RedirectHost}
}

// oauthKeyName is the grant's default key name: the client's name, made safe.
func oauthKeyName(client string) string {
	n := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, client))
	if r := []rune(n); len(r) > maxKeyNameRunes-6 {
		n = string(r[:maxKeyNameRunes-6])
	}
	if n == "" {
		n = "Connected app"
	}
	return n
}

// uniqueKeyName appends " (2)", " (3)" ... until the name is free.
func uniqueKeyName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s (%d)", base, i); !taken[n] {
			return n
		}
	}
}

// AlertOAuthRefreshReuse pushes the urgent alert for a reused refresh token.
func (a *API) AlertOAuthRefreshReuse(userID ulid.ULID, k store.APIKey) {
	a.sendAlert(userID, *anomalyAlert(k, "had a sign-in reused",
		"A refresh token that had already been used was presented again, so every sign-in from that connection was ended. "+
			"The app stays connected and can ask you to approve it again.", false))
}

// ---- OAuthClient/get, OAuthClient/set, OAuthFamily/set ---------------------

const maxManualClients = 20

func oauthClientObject(c store.OAuthClient) map[string]any {
	return map[string]any{"id": c.ID, "name": c.Name, "redirectUris": c.RedirectURIs, "createdAt": jmapDate(c.CreatedAt)}
}

func (a *API) oauthClientGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if mErr := sessionOnlyGuard(ctx); mErr != nil {
		return nil, mErr
	}
	var args struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	list := []map[string]any{}
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		cs, err := tx.ListOAuthClients(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, c := range cs {
			list = append(list, oauthClientObject(c))
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{"accountId": userID.String(), "state": "0", "list": list, "notFound": []string{}}), nil
}

func (a *API) oauthClientSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		for cid, raw := range args.Create {
			var in struct {
				Name         string   `json:"name"`
				RedirectURIs []string `json:"redirectUris"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				res.NotCreated[cid] = SetError{Type: setInvalidProps, Description: err.Error()}
				continue
			}
			name := oauthKeyName(in.Name)
			if strings.TrimSpace(in.Name) == "" {
				res.NotCreated[cid] = *invalidKey("name", "a client needs a name")
				continue
			}
			if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
				res.NotCreated[cid] = *invalidKey("redirectUris", "give between 1 and 10 redirect URIs")
				continue
			}
			bad := ""
			for _, u := range in.RedirectURIs {
				if oauthserver.ValidateRedirectURI(u) != nil {
					bad = u
					break
				}
			}
			if bad != "" {
				res.NotCreated[cid] = *invalidKey("redirectUris", "redirect URIs must be https, or http to localhost, 127.0.0.1 or [::1]: "+bad)
				continue
			}
			n, err := tx.CountOAuthClients(ctx)
			if err != nil {
				return err
			}
			if n >= maxManualClients {
				res.NotCreated[cid] = SetError{Type: setForbidden, Description: fmt.Sprintf("you already have %d registered apps, the most allowed", maxManualClients)}
				continue
			}
			c, err := tx.CreateOAuthClient(ctx, name, in.RedirectURIs)
			if err != nil {
				return err
			}
			res.Created[cid] = oauthClientObject(c)
		}
		for id := range args.Update {
			res.NotUpdated[id] = SetError{Type: setForbidden, Description: "a registered app cannot be edited; delete it and register it again"}
		}
		for _, id := range args.Destroy {
			if err := tx.DeleteOAuthClient(ctx, id); err != nil {
				if errors.Is(err, store.ErrOAuthClientNotFound) {
					res.NotDestroyed[id] = SetError{Type: setNotFound}
					continue
				}
				return err
			}
			res.Destroyed = append(res.Destroyed, id)
		}
		return nil
	})
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(res), nil
}

// oauthFamilySet revokes one token family of a grant (destroy only): one
// device that connected, without ending the grant. It takes effect on the next
// request, because every access token is checked against its row each time.
func (a *API) oauthFamilySet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
	for id := range args.Create {
		res.NotCreated[id] = SetError{Type: setForbidden, Description: "families are made by authorizing"}
	}
	for id := range args.Update {
		res.NotUpdated[id] = SetError{Type: setForbidden, Description: "families are only revoked"}
	}
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		fams, err := tx.ListOAuthFamilies(ctx, 1000)
		if err != nil {
			return err
		}
		for _, id := range args.Destroy {
			fid, perr := ulid.Parse(id)
			var owner *ulid.ULID
			for _, f := range fams {
				if perr == nil && f.ID == fid {
					k := f.APIKeyID
					owner = &k
				}
			}
			if owner == nil {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			n, err := tx.RevokeOAuthFamily(ctx, fid)
			if err != nil {
				return err
			}
			if err := tx.AuditAPIKeyLifecycle(ctx, "api_key.family_revoke", *owner,
				map[string]any{"familyId": id, "tokensRevoked": n}); err != nil {
				return err
			}
			res.Destroyed = append(res.Destroyed, id)
		}
		return nil
	})
	if err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(res), nil
}
