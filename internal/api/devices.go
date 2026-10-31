// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// SignedInDevice/get and /set for the `urn:mailroom:devices` extension (#937,
// ADR-048 section 6). A signed-in device is a server-side browser session; the
// list is what Settings > Security shows, and /set with `destroy` is "sign out
// that device".
//
// Like WebPushSubscription it is OPERATIONAL data read on demand, not a
// JMAP-synced object: it appends no change_log row and there is NO
// SignedInDevice/changes. The state string is a digest of the ids listed, so a
// client that wants to know whether the list moved re-fetches it.
//
// Both methods are session-only (agent_scopes.go): an API key is not a session,
// has its own page, and must never end a person's sessions.
//
// The raw user agent is never returned, only the description describeUserAgent
// derives from it.

// reasonSignedOutElsewhere is the revoked_reason for a device signed out from
// another one.
const reasonSignedOutElsewhere = store.SessionRevokedSignedOutElsewhere

// othersToken is the literal in `destroy` meaning every session but this one.
const othersToken = "*others*"

// SessionRevoker is told, AFTER a device sign-out has committed, which sessions
// it ended and each one's SEALED refresh token, so the provider's grant can be
// revoked too (RFC 7009). Only ciphertext crosses this boundary; the
// implementation (cmd/mailroom) opens it. It must not block the response: the
// installed one detaches. Nil means no provider revocation.
type SessionRevoker func(userID, sessionID ulid.ULID, sealedRefresh []byte)

// WithSessionRevoker installs the provider-revocation hook for device sign-outs.
func WithSessionRevoker(f SessionRevoker) Option { return func(a *API) { a.sessionRevoker = f } }

// SessionLive reports whether a session is still usable, for the EventSource
// re-check. It must stamp nothing and never renew.
type SessionLive func(ctx context.Context, sessionID, userID ulid.ULID) (bool, error)

// WithSessionLive installs the stream re-check. Without it a stream is never
// re-checked (dev-auth, tests).
func WithSessionLive(f SessionLive) Option { return func(a *API) { a.sessionLive = f } }

// devicesState is the get response's state: a digest of the listed ids.
func devicesState(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, ",")))
	return "d" + hex.EncodeToString(sum[:6])
}

type deviceObject struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
	LastSeenAt  string `json:"lastSeenAt"`
	LastAddr    string `json:"lastAddr"`
	IsCurrent   bool   `json:"isCurrent"`
}

type deviceGetResult struct {
	AccountID string         `json:"accountId"`
	State     string         `json:"state"`
	List      []deviceObject `json:"list"`
	NotFound  []string       `json:"notFound"`
}

func (a *API) signedInDeviceGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
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
	res := deviceGetResult{AccountID: userID.String(), List: []deviceObject{}, NotFound: []string{}}

	cur, hasSession := auth.SessionFromContext(ctx)
	if !hasSession {
		// Dev-auth has no sessions: an empty list, and every requested id is
		// simply not there.
		if args.IDs != nil {
			res.NotFound = append(res.NotFound, *args.IDs...)
		}
		res.State = devicesState(nil)
		return mustMarshal(res), nil
	}
	if args.IDs != nil {
		// ids other than null are not supported: a list is a handful of rows and
		// the client asks for all of it. Say "not found" rather than guess.
		res.NotFound = append(res.NotFound, *args.IDs...)
		res.State = devicesState(nil)
		return mustMarshal(res), nil
	}

	var rows []store.SessionRow
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		if rows, err = tx.ListLiveSessions(ctx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID.String())
		res.List = append(res.List, deviceObject{
			ID:          r.ID.String(),
			Description: describeUserAgent(r.UserAgent),
			CreatedAt:   jmapDate(r.CreatedAt),
			LastSeenAt:  jmapDate(r.LastSeenAt),
			LastAddr:    r.LastAddr,
			IsCurrent:   r.ID == cur.ID,
		})
	}
	res.State = devicesState(ids)
	return mustMarshal(res), nil
}

type deviceSetResult struct {
	AccountID    string              `json:"accountId"`
	Created      map[string]any      `json:"created"`
	Updated      map[string]any      `json:"updated"`
	Destroyed    []string            `json:"destroyed"`
	NotCreated   map[string]SetError `json:"notCreated"`
	NotUpdated   map[string]SetError `json:"notUpdated"`
	NotDestroyed map[string]SetError `json:"notDestroyed"`
}

// signedInDeviceSet implements destroy and nothing else.
//
// `destroy` takes session ids, and the literal "*others*" for every session but
// the caller's. For each session it ends: the row is revoked
// (signed_out_elsewhere), the push subscriptions registered from it are deleted
// in the same transaction, and, after commit, its refresh token is revoked at the
// provider in the background (SessionRevoker).
//
// DESTROYING THE CURRENT SESSION by its id is allowed and equals signing out
// here: this request was authenticated before it ran, so it succeeds, and the
// NEXT request is a 401. The client then goes to the sign-in page, the same as
// after any session loss. (It does not clear the browser cookie; the 401 does the
// work.)
//
// An unknown, already-revoked or another user's id is notDestroyed/notFound
// (RLS makes the last indistinguishable from the first). Dev-auth has no
// sessions: every id is notFound and "*others*" does nothing.
func (a *API) signedInDeviceSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	res := deviceSetResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}
	for cid := range args.Create {
		res.NotCreated[cid] = SetError{Type: setForbidden,
			Description: "devices are created by signing in, not by this method"}
	}
	for id := range args.Update {
		res.NotUpdated[id] = SetError{Type: setForbidden, Description: "a signed-in device cannot be changed"}
	}

	cur, hasSession := auth.SessionFromContext(ctx)
	if !hasSession {
		for _, id := range args.Destroy {
			if id != othersToken {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
			}
		}
		return mustMarshal(res), nil
	}

	var (
		others   bool
		valid    []ulid.ULID
		sealed   map[ulid.ULID][]byte
		ended    []ulid.ULID
		notFound []string
	)
	for _, id := range args.Destroy {
		if id == othersToken {
			others = true
			continue
		}
		sid, err := ulid.Parse(id)
		if err != nil {
			res.NotDestroyed[id] = SetError{Type: setNotFound}
			continue
		}
		valid = append(valid, sid)
	}

	if len(valid) > 0 || others {
		err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			// Reset per attempt: a transaction body must not accumulate state.
			sealed, ended, notFound = map[ulid.ULID][]byte{}, nil, nil
			if len(valid) > 0 {
				r, err := tx.RevokeSessions(ctx, valid, reasonSignedOutElsewhere)
				if err != nil {
					return err
				}
				ended = append(ended, r.IDs...)
				for id, ct := range r.RefreshCT {
					sealed[id] = ct
				}
				got := make(map[ulid.ULID]bool, len(r.IDs))
				for _, id := range r.IDs {
					got[id] = true
				}
				for _, id := range valid {
					if !got[id] {
						notFound = append(notFound, id.String())
					}
				}
			}
			if others {
				r, err := tx.RevokeSessionsExcept(ctx, cur.ID, reasonSignedOutElsewhere)
				if err != nil {
					return err
				}
				ended = append(ended, r.IDs...)
				for id, ct := range r.RefreshCT {
					sealed[id] = ct
				}
			}
			return nil
		})
		if err != nil {
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
	}
	for _, id := range notFound {
		res.NotDestroyed[id] = SetError{Type: setNotFound}
	}
	seen := map[ulid.ULID]bool{}
	for _, id := range ended {
		if seen[id] {
			continue
		}
		seen[id] = true
		res.Destroyed = append(res.Destroyed, id.String())
		if ct := sealed[id]; len(ct) > 0 && a.sessionRevoker != nil {
			a.sessionRevoker(userID, id, ct)
		}
	}
	return mustMarshal(res), nil
}

// ---- user agent description ------------------------------------------------

// uaRule is one row of a table: the first row whose needle appears in the user
// agent decides. Order is the whole logic, because user agents imitate each
// other (Edge and Opera say Chrome; Chrome says Safari; an iPhone says Mac OS X;
// Android says Linux), so the more specific product is listed first.
type uaRule struct {
	needle string
	name   string
}

var uaBrowsers = []uaRule{
	{"Edg/", "Edge"}, {"EdgA/", "Edge"}, {"EdgiOS/", "Edge"}, {"Edge/", "Edge"},
	{"OPR/", "Opera"}, {"OPiOS/", "Opera"}, {"Opera", "Opera"},
	{"SamsungBrowser/", "Samsung Internet"},
	{"Vivaldi/", "Vivaldi"},
	{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"},
	{"Chrome/", "Chrome"}, {"CriOS/", "Chrome"},
	{"Safari/", "Safari"},
	// An installed web app on iOS drops the Safari/ token but keeps WebKit.
	{"AppleWebKit/", "Safari"},
}

var uaSystems = []uaRule{
	{"iPhone", "iPhone"},
	{"iPad", "iPad"},
	{"Android", "Android"},
	{"Windows", "Windows"},
	{"CrOS", "ChromeOS"},
	{"Macintosh", "macOS"}, {"Mac OS X", "macOS"},
	{"Linux", "Linux"}, {"X11", "Linux"},
}

// unknownBrowser is what a user agent that names no known browser is called.
const unknownBrowser = "Unknown browser"

// describeUserAgent reduces a User-Agent header to "Browser on System" for the
// devices list: "Firefox on Linux", "Safari on iPhone". It is a convenience for
// recognising your own devices, not a fingerprint, and it never echoes the raw
// string back (which is attacker-controlled text and can be long).
func describeUserAgent(ua string) string {
	browser := firstMatch(uaBrowsers, ua)
	if browser == "" {
		return unknownBrowser
	}
	if system := firstMatch(uaSystems, ua); system != "" {
		return browser + " on " + system
	}
	return browser
}

func firstMatch(rules []uaRule, s string) string {
	for _, r := range rules {
		if strings.Contains(s, r.needle) {
			return r.name
		}
	}
	return ""
}

// streamRecheckInterval is how often an open EventSource stream re-checks that
// its session is still usable (ADR-048 section 6).
const streamRecheckInterval = 30 * time.Second
