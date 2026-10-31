// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// "The user has asked for THIS message" (ADR-026 §3c), which is the half of the
// proxy's authorisation rule that a remembered grant does not cover.
//
// # Why this has to exist at all
//
// Remote content has two ways to be allowed, and #202 made them deliberately
// different. A GRANT is remembered, per sender or per domain, listed in
// settings and revocable. A ONE-OFF click is not remembered by design — "I
// trusted this one newsletter once" must not become "this sender may track me
// forever" — so the client keeps it as component state and records nothing.
//
// ⚠ THAT DESIGN BROKE THE MOMENT THE PROXY BECAME THE FETCHER, and CI is what
// caught it. Before the proxy, a one-off click needed no server involvement: the
// client un-parked the URL and the BROWSER fetched it. With the proxy on, the
// browser asks the SERVER, and the server had no idea the user had clicked — so
// it refused, and the most ordinary path in the whole feature ("show me this
// one message's images") silently loaded nothing.
//
// # Why it is in memory and not a table
//
// Because "a one-off allow records nothing" is a promise to the user, not an
// implementation detail. A row in Postgres would be a durable record of which
// messages someone chose to look at properly — closer to a grant than to the
// non-decision it is meant to be. This is a short-lived, bounded, in-process
// note that the user clicked, and it is deliberately lost on restart: the cost
// of that is one more click, and the alternative is a log of reading habits.
//
// It also means a multi-process deployment would need this rethought, which is
// recorded here rather than discovered later.

// remoteRequestTTL is how long a click keeps the proxy willing to fetch for one
// message. Long enough to load a message's images and re-read it for a while;
// short enough that it is a moment rather than a grant.
const remoteRequestTTL = 30 * time.Minute

// maxRemoteRequestsPerUser bounds the map so a client looping over messages
// cannot grow it without limit. Oldest entries are dropped first.
const maxRemoteRequestsPerUser = 512

// remoteRequests records, per user, which messages the user has asked to see
// remote content for. Safe for concurrent use.
type remoteRequests struct {
	mu     sync.Mutex
	byUser map[ulid.ULID]map[ulid.ULID]time.Time
}

func newRemoteRequests() *remoteRequests {
	return &remoteRequests{byUser: map[ulid.ULID]map[ulid.ULID]time.Time{}}
}

// note records that this user asked for this message's remote content.
func (r *remoteRequests) note(user, message ulid.ULID, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.byUser[user]
	if m == nil {
		m = map[ulid.ULID]time.Time{}
		r.byUser[user] = m
	}
	m[message] = now.Add(remoteRequestTTL)
	if len(m) <= maxRemoteRequestsPerUser {
		return
	}
	// Over the bound: drop the entry expiring soonest. Dropping the oldest is
	// the fail-closed direction — the user re-clicks — and it keeps this from
	// becoming a memory leak driven by a client's request pattern.
	var oldest ulid.ULID
	var oldestAt time.Time
	for id, exp := range m {
		if oldestAt.IsZero() || exp.Before(oldestAt) {
			oldest, oldestAt = id, exp
		}
	}
	delete(m, oldest)
}

// asked reports whether an unexpired request exists, and sweeps expired entries
// for this user while it is holding the lock.
//
// NIL-RECEIVER SAFE, and that is the fail-closed direction: a nil store means
// the proxy is off, so nobody has asked for anything.
func (r *remoteRequests) asked(user, message ulid.ULID, now time.Time) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.byUser[user]
	if m == nil {
		return false
	}
	for id, exp := range m {
		if now.After(exp) {
			delete(m, id)
		}
	}
	if len(m) == 0 {
		delete(r.byUser, user)
		return false
	}
	exp, ok := m[message]
	return ok && !now.After(exp)
}

// remoteContentRequest implements RemoteContent/request: "I clicked Show remote
// content on this message."
//
// ⚠ IT VERIFIES THE MESSAGE IS THE CALLER'S BEFORE RECORDING ANYTHING, through
// an RLS-scoped read. Skipping that would let one user note a request against
// another user's message id — which, combined with the proxy, is a way to ask
// the server to fetch from a message they cannot see. Another user's id is
// simply not found, which is the same answer as an id that never existed.
//
// It grants nothing durable and appends no change_log row: it is not a
// JMAP-visible object, it is a moment.
func (a *API) remoteContentRequest(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	messageID, err := ulid.Parse(args.MessageID)
	if err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "messageId is not a valid id"}
	}
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, e := tx.MessageAuthRefByID(ctx, messageID)
		return e
	}); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "no such message"}
	}
	if a.remoteAsks == nil {
		// The proxy is off, so there is nothing for a request to unlock. Say so
		// rather than pretending to have recorded something.
		return mustMarshal(map[string]any{
			"accountId": userID.String(), "requested": false,
		}), nil
	}
	a.remoteAsks.note(userID, messageID, time.Now())
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"requested": true,
		"expiresIn": int(remoteRequestTTL.Seconds()),
	}), nil
}
