// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Activity/list for the `urn:mailroom:activity` extension (ADR-053, #45): one
// merged, newest-first list of what happened to the caller's mail and access,
// read from the records that already exist (AI-tool actions, decryptions,
// purges, sign-ins and sign-outs, rule and placement bulk actions).
//
// Like SignedInDevice it is OPERATIONAL data read on demand, not a JMAP-synced
// object: it writes nothing, appends no change_log row, and has no /changes.
//
// SESSION ONLY (agent_scopes.go). The list records what API keys and other
// sessions did, including refusals and anomalies; a key must not be able to
// read the record of its own detection. A refusal is audited like any other.
//
// Nothing here is message content: a decryption names a message id, never a
// subject, and the session user agent is described, never returned raw.

// activityLimitDefault and activityLimitMax are store.ActivityDefaultLimit and
// store.ActivityMaxLimit, restated where the API's own description needs them.
const (
	activityLimitDefault = store.ActivityDefaultLimit
	activityLimitMax     = store.ActivityMaxLimit
)

type activityObject struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Exists bool   `json:"exists"`
}

type activityEntry struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Source      string          `json:"source"`
	At          string          `json:"at"`
	Outcome     *string         `json:"outcome"`
	KeyName     *string         `json:"keyName"`
	Method      *string         `json:"method"`
	Reason      *string         `json:"reason"`
	Purpose     *string         `json:"purpose"`
	Fingerprint *string         `json:"fingerprint"`
	Device      *string         `json:"device"`
	Count       *int            `json:"count"`
	Object      *activityObject `json:"object"`
}

func (a *API) activityList(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	if mErr := sessionOnlyGuard(ctx); mErr != nil {
		return nil, mErr
	}
	var args struct {
		AccountID string   `json:"accountId"`
		Sources   []string `json:"sources"`
		Since     *string  `json:"since"`
		Until     *string  `json:"until"`
		Before    *string  `json:"before"`
		Limit     *int     `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	f := store.ActivityFilter{Sources: args.Sources}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > activityLimitMax {
			return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be between 1 and 100"}
		}
		f.Limit = *args.Limit
	}
	parse := func(name string, s *string) (time.Time, *MethodError) {
		if s == nil || *s == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, *s)
		if err != nil {
			return time.Time{}, &MethodError{Type: errInvalidArguments, Description: name + " must be a UTCDate (RFC 3339)"}
		}
		return t.UTC(), nil
	}
	var mErr *MethodError
	if f.Since, mErr = parse("since", args.Since); mErr != nil {
		return nil, mErr
	}
	if f.Until, mErr = parse("until", args.Until); mErr != nil {
		return nil, mErr
	}
	if args.Before != nil && *args.Before != "" {
		c, err := store.ParseActivityCursor(*args.Before)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "before is not a cursor from this list"}
		}
		f.After = &c
	}

	var page store.ActivityPage
	mErr = a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if page, err = tx.ListActivity(ctx, f); err != nil {
			if errors.Is(err, store.ErrActivityArgs) {
				return &MethodError{Type: errInvalidArguments, Description: err.Error()}
			}
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := make([]activityEntry, 0, len(page.Entries))
	for _, e := range page.Entries {
		out := activityEntry{
			ID: e.Kind + ":" + e.ID, Kind: e.Kind, Source: store.ActivitySourceOf(e.Kind), At: jmapDate(e.At),
			Outcome: strOrNull(e.Outcome), KeyName: strOrNull(e.KeyName), Method: strOrNull(e.Method),
			Reason: strOrNull(e.Reason), Purpose: strOrNull(e.Purpose), Fingerprint: strOrNull(e.Fingerprint),
		}
		if e.UserAgent != "" || e.Kind == store.ActivityKindSignIn || e.Kind == store.ActivityKindSignOut {
			d := describeUserAgent(e.UserAgent)
			out.Device = &d
		}
		if e.Count > 0 {
			c := e.Count
			out.Count = &c
		}
		if e.ObjectType != "" {
			out.Object = &activityObject{Type: e.ObjectType, ID: e.ObjectID, Exists: e.ObjectExists}
		}
		list = append(list, out)
	}
	covers := map[string]any{}
	for src, t := range page.Covers {
		covers[src] = jmapDateOrNil(t)
	}
	var next any
	if page.Next != "" {
		next = page.Next
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(), "list": list, "next": next, "limit": page.Limit, "covers": covers,
	}), nil
}

// activityCapability is the urn:mailroom:activity session object: the list's
// bounds, stated by the server rather than assumed by the client.
func activityCapability() map[string]any {
	return map[string]any{
		"sources":      store.ActivitySources,
		"defaultLimit": activityLimitDefault,
		"maxLimit":     activityLimitMax,
	}
}
