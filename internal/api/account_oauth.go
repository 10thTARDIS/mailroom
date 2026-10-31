// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/oauthmail"
	"mailroom/internal/store"
)

// Connecting a mail account by signing in at its provider (#52, ADR-046 §3,
// increment 3): Account/oauthStart and Account/oauthDisconnect, both
// urn:mailroom:accounts extensions.
//
// Both are SESSION-ONLY (the scope table refuses them to API keys, and the
// handlers refuse a key again, as PendingAction/set does): connecting a
// mailbox grants this server full control of it, and is a person's decision.
// Both are JMAP POSTs, so the CSRF guard in front of /jmap applies (a
// cross-site request is refused before it reaches them).
//
// oauthStart does not create anything visible: it records a pending flow and
// returns the provider's authorization URL. The account appears (with its
// change_log row) when the provider sends the browser back to
// /oauth/callback/{provider} and that succeeds.

// OAuthConnector is the connect flow as the API sees it (oauthmail.Connector).
type OAuthConnector interface {
	Providers() []string
	Start(ctx context.Context, userID ulid.ULID, provider string, reauth *ulid.ULID) (oauthmail.StartResult, error)
	Disconnect(ctx context.Context, userID, accountID ulid.ULID) (oauthmail.DisconnectResult, error)
	Forget(userID, accountID ulid.ULID)
	// PrepareRevoke reads what deleting the account must revoke at the
	// provider, before the delete; the returned function (nil: nothing to
	// revoke) runs after the delete commits and never fails it.
	PrepareRevoke(ctx context.Context, userID, accountID ulid.ULID) func(context.Context)
}

// WithOAuthConnect enables the OAuth connect methods and advertises the
// enabled providers in the accounts capability. Without it oauthProviders is
// empty and both methods answer notImplemented.
func WithOAuthConnect(c OAuthConnector) Option {
	return func(a *API) { a.oauth = c }
}

// errOAuthNotImplemented is the method-level answer when no provider (or not
// the named one) is configured: the client offers no Connect button then, so
// a call is a stale client or a probe.
const errOAuthNotImplemented = "notImplemented"

// oauthProviders is the capability's list: never nil, so it serialises as [].
func (a *API) oauthProviders() []string {
	if a.oauth == nil {
		return []string{}
	}
	if p := a.oauth.Providers(); p != nil {
		return p
	}
	return []string{}
}

type oauthStartArgs struct {
	AccountID string `json:"accountId"`
	Provider  string `json:"provider"`
	// ID names an existing OAuth Account to RE-authenticate (or reconnect
	// after a disconnect); absent for a new account.
	ID *string `json:"id"`
}

type oauthStartResult struct {
	AccountID        string `json:"accountId"`
	Provider         string `json:"provider"`
	AuthorizationURL string `json:"authorizationUrl"`
	ExpiresAt        string `json:"expiresAt"`
}

func (a *API) accountOAuthStart(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if principalFrom(ctx) != nil {
		return nil, forbidden("connecting a mailbox is for a person, never an API key")
	}
	var args oauthStartArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if a.oauth == nil {
		return nil, &MethodError{Type: errOAuthNotImplemented, Description: "no sign-in provider is configured on this server"}
	}
	var target *ulid.ULID
	if args.ID != nil {
		id, err := ulid.Parse(*args.ID)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "id is not an Account id"}
		}
		target = &id
	}
	res, err := a.oauth.Start(ctx, userID, args.Provider, target)
	if err != nil {
		return nil, oauthMethodError(err)
	}
	return mustMarshal(oauthStartResult{
		AccountID:        userID.String(),
		Provider:         args.Provider,
		AuthorizationURL: res.AuthorizeURL,
		ExpiresAt:        res.ExpiresAt.UTC().Format(time.RFC3339),
	}), nil
}

type oauthDisconnectArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
}

type oauthDisconnectResult struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	// Revocation: "revoked", "failed", "unsupported" or "not_configured"
	// (oauthmail.DisconnectResult). Anything but "revoked" means the grant
	// may still exist at the provider and the user should remove the app's
	// access there.
	Revocation string `json:"revocation"`
}

func (a *API) accountOAuthDisconnect(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if principalFrom(ctx) != nil {
		return nil, forbidden("disconnecting a mailbox is for a person, never an API key")
	}
	var args oauthDisconnectArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	id, err := ulid.Parse(args.ID)
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "id is not an Account id"}
	}
	if a.oauth == nil {
		return nil, &MethodError{Type: errOAuthNotImplemented, Description: "no sign-in provider is configured on this server"}
	}
	res, err := a.oauth.Disconnect(ctx, userID, id)
	if err != nil {
		return nil, oauthMethodError(err)
	}
	return mustMarshal(oauthDisconnectResult{
		AccountID: userID.String(), ID: id.String(), Provider: res.Provider, Revocation: res.Revocation,
	}), nil
}

// oauthMethodError maps the connector's refusals to method errors whose
// descriptions a person can act on. Anything unexpected is serverFail with a
// generic text: a connector error may name internals.
func oauthMethodError(err error) *MethodError {
	switch {
	case errors.Is(err, oauthmail.ErrProviderNotEnabled):
		return &MethodError{Type: errOAuthNotImplemented, Description: "that provider is not configured on this server"}
	case errors.Is(err, store.ErrAccountNotFound):
		return &MethodError{Type: errInvalidArguments, Description: "no such account"}
	case errors.Is(err, oauthmail.ErrNotOAuthAccount):
		return &MethodError{Type: errInvalidArguments, Description: "that account signs in with a password, not with a provider"}
	case errors.Is(err, oauthmail.ErrWrongProvider):
		return &MethodError{Type: errInvalidArguments, Description: "that account belongs to another provider"}
	case errors.Is(err, oauthmail.ErrNoGrant):
		return &MethodError{Type: errInvalidArguments, Description: "that account is already disconnected"}
	}
	slog.Error("oauth connect method failed", "err", err.Error())
	return &MethodError{Type: errServerFail, Description: "could not complete the request"}
}
