// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/egress"
	"mailroom/internal/store"
)

// pushEndpointPolicy is the policy a push endpoint is validated against, and it
// is deliberately the DEFAULT one: https only. A Web Push endpoint is always
// https, so this must not inherit the image proxy's widening to http (ADR-026
// decision A) — permitted schemes come from the caller precisely so that these
// two callers can differ.
var pushEndpointPolicy = egress.Policy{}

// WebPushSubscription/get|set for the `urn:mailroom:push` extension (spec §12,
// mailroom#42, ADR-016). A subscription is a browser's Web Push registration:
// its endpoint plus the keys the server encrypts to. This is DEVICE-REGISTRATION
// state, read on demand — like Account/getHealth it is NOT a JMAP-synced object,
// so it appends no change_log row and carries no per-type state (CLAUDE.md).
//
// Both methods run through the caller's RLS-scoped UserTx and reject a request
// naming another user's account before any row is read (spec §10): a leak here
// would expose another user's device push endpoints and keys. The cross-user RLS
// probe for both methods is in rls_forcing_test.go + rls_integration_test.go.
//
// The subscription encryption keys (p256dh, auth) are WRITE-ONLY: they are
// accepted by /set and used only by the server-side sender (internal/webpush),
// and are NEVER echoed by /get. /get also omits the raw endpoint, exposing only
// the non-sensitive registration metadata a settings UI needs.

// pushObject renders a stored subscription for WebPushSubscription/get. It
// deliberately surfaces only {id, deviceName, notifyMode, createdAt,
// expiredAt}: never the endpoint, never the keys.
//
// notifyMode (#59) is the device's push mode — "all" | "above_line" | "vip" |
// "above_line_or_vip" — and REPLACES importantOnly, which defaulted to true
// and was never read by anything (#626 §E). importantOnly is no longer
// returned, and an importantOnly in a create is ignored like any other unknown
// key: honouring it now would turn on a filter every existing client sent by
// default without anyone having chosen it.
func pushObject(ps store.PushSubscription, props map[string]bool) map[string]any {
	full := map[string]any{
		"id":         ps.ID.String(),
		"deviceName": ps.DeviceName,
		"notifyMode": ps.NotifyMode,
		"createdAt":  jmapDate(ps.CreatedAt),
		"expiredAt":  jmapDatePtr(ps.ExpiredAt),
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": ps.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// ---- WebPushSubscription/get ----------------------------------------------

type pushGetResult struct {
	AccountID string           `json:"accountId"`
	List      []map[string]any `json:"list"`
	NotFound  []string         `json:"notFound"`
}

func (a *API) pushGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties []string  `json:"properties"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var props map[string]bool
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties))
		for _, p := range args.Properties {
			props[p] = true
		}
	}

	var (
		byID      = make(map[ulid.ULID]store.PushSubscription)
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var subs []store.PushSubscription
		var err error
		if args.IDs == nil {
			// ids:null means "all subscriptions" (RFC 8620 §5.1).
			subs, err = tx.ListPushSubscriptions(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, s := range subs {
				requested = append(requested, requestedID{raw: s.ID.String(), id: s.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			subs, err = tx.GetPushSubscriptions(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		for _, s := range subs {
			byID[s.ID] = s
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			continue
		}
		ps, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, pushObject(ps, props))
	}
	return mustMarshal(pushGetResult{
		AccountID: userID.String(),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// ---- WebPushSubscription/set ----------------------------------------------

// pushSetResult is the /set response. WebPushSubscription carries no per-type
// change state (it is not a change_log object), so — unlike a Foo/set — it emits
// no oldState/newState.
type pushSetResult struct {
	AccountID    string              `json:"accountId"`
	Created      map[string]any      `json:"created"`
	Updated      map[string]any      `json:"updated"`
	Destroyed    []string            `json:"destroyed"`
	NotCreated   map[string]SetError `json:"notCreated"`
	NotUpdated   map[string]SetError `json:"notUpdated"`
	NotDestroyed map[string]SetError `json:"notDestroyed"`
}

// pushCreate is the create payload. keys are required and write-only; deviceName
// and notifyMode are optional. An absent notifyMode keeps a re-registered
// device's mode and gives a new one "all" — every message, the operator's
// default until the user picks (#59; ADR-016's 2026-10-17 amendment records the
// deviation from spec §12's important-only default).
type pushCreate struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	DeviceName string  `json:"deviceName"`
	NotifyMode *string `json:"notifyMode"`
}

func (a *API) pushSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // shared {accountId, ifInState, create, update, destroy}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := pushSetResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}

	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		for cid, raw := range args.Create {
			obj, serr := createPushSubscription(ctx, tx, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			res.Created[cid] = obj
		}

		// notifyMode is the one mutable property (#59): the device's push mode,
		// changed from Settings › Notifications. Everything else about a
		// registration is re-sent by re-subscribing (a create/upsert).
		for id, raw := range args.Update {
			if serr := updatePushSubscription(ctx, tx, id, raw); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		for _, id := range args.Destroy {
			pid, perr := ulid.Parse(id)
			if perr != nil {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, derr := tx.DeletePushSubscription(ctx, pid)
			if derr != nil {
				return derr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
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

// createPushSubscription parses and upserts one create payload, returning the
// created JMAP object (id + non-sensitive metadata; NEVER the keys). Re-creating
// the same endpoint upserts and returns the existing id (store upserts on
// (user_id, endpoint)).
func createPushSubscription(ctx context.Context, tx *store.UserTx, raw json.RawMessage) (map[string]any, *SetError) {
	var c pushCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	if c.Endpoint == "" {
		return nil, &SetError{Type: setInvalidProps,
			Description: "endpoint is required", Properties: []string{"endpoint"}}
	}
	// ⚠ The endpoint is a CLIENT-SUPPLIED URL that the server will later POST to
	// (internal/webpush). Until ADR-026 it was accepted on the strength of being
	// non-empty alone — no scheme, no host, no address check — which made this a
	// blind SSRF: an authenticated user could register their "device" at
	// 169.254.169.254, a loopback admin port or an internal service and have the
	// server POST there on every mail arrival.
	//
	// This refuses the obvious cases at the moment they are offered, so the
	// caller gets a clear error instead of a subscription that silently never
	// works. It is NOT the real defence — a hostname's addresses can change after
	// this runs, which is exactly what DNS rebinding is — so the guard that holds
	// is egress's, at dial time, on the address being dialled.
	if err := pushEndpointPolicy.CheckURLString(c.Endpoint); err != nil {
		return nil, &SetError{Type: setInvalidProps,
			Description: "endpoint is not a permitted URL: " + err.Error(),
			Properties:  []string{"endpoint"}}
	}
	if c.Keys.P256dh == "" || c.Keys.Auth == "" {
		return nil, &SetError{Type: setInvalidProps,
			Description: "keys.p256dh and keys.auth are required", Properties: []string{"keys"}}
	}
	if c.NotifyMode != nil && !store.ValidPushMode(*c.NotifyMode) {
		return nil, invalidPushMode()
	}
	in := store.PushSubscriptionInput{
		Endpoint:   c.Endpoint,
		KeysP256dh: c.Keys.P256dh,
		KeysAuth:   c.Keys.Auth,
		DeviceName: c.DeviceName,
		NotifyMode: c.NotifyMode,
	}
	// The browser session registering this subscription, so signing that device
	// out deletes it (ADR-048 section 6). Dev-auth has no session: nil.
	if ref, ok := auth.SessionFromContext(ctx); ok {
		sid := ref.ID
		in.SessionID = &sid
	}
	ps, err := tx.CreatePushSubscription(ctx, in)
	if err != nil {
		return nil, &SetError{Type: setForbidden, Description: err.Error()}
	}
	return pushObject(ps, nil), nil
}

func invalidPushMode() *SetError {
	return &SetError{Type: setInvalidProps,
		Description: "notifyMode must be one of all, above_line, vip, above_line_or_vip",
		Properties:  []string{"notifyMode"}}
}

// updatePushSubscription applies one /set update. Only notifyMode may change;
// any other key is refused by name rather than ignored, so a client that
// believes it renamed a device is told it did not.
func updatePushSubscription(ctx context.Context, tx *store.UserTx, id string, raw json.RawMessage) *SetError {
	pid, err := ulid.Parse(id)
	if err != nil {
		return &SetError{Type: setNotFound}
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var mode *string
	for key, val := range patch {
		if key != "notifyMode" {
			return &SetError{Type: setInvalidProps,
				Description: "only notifyMode can be updated on a push subscription", Properties: []string{key}}
		}
		var m string
		if err := json.Unmarshal(val, &m); err != nil || !store.ValidPushMode(m) {
			return invalidPushMode()
		}
		mode = &m
	}
	if mode == nil {
		return nil // an empty patch is a successful no-op
	}
	found, err := tx.SetPushSubscriptionMode(ctx, pid, *mode)
	if err != nil {
		if errors.Is(err, store.ErrPushModeInvalid) {
			return invalidPushMode()
		}
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
	if !found {
		return &SetError{Type: setNotFound}
	}
	return nil
}
