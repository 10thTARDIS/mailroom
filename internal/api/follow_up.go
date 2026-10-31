// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/nudge"
	"mailroom/internal/store"
)

// FollowUp/get and FollowUp/set (#58 phase 2): follow-up nudges.
//
//   - "No one answered your mail from <day>" (kind "outbound"), on by default.
//   - "You haven't replied to this" (kind "inbound"), opt-in.
//
// COMPUTED ON READ, like SenderStats/get and Account/getHealth. A nudge is a
// judgement about a conversation's state now, so there is no stored nudge to
// go stale and no job to fall behind: the store reads the candidates in three
// bounded queries (store/follow_up.go states the bounds) and nudge.Due — the
// SAME rule functions `mailroom-admin nudge-report` measured — decides.
//
// THE ONLY COSTLY STEP IS PARSING, and it is paid once per message. The rules
// read To/Cc and header facts from the message bytes. Bytes never change for a
// message id, so each parse is kept in a bounded in-process cache
// (followUpCacheSize entries); a steady-state read decrypts only mail that
// arrived since the last one. Entries are keyed by user AND message, and are
// only ever looked up with ids the caller's RLS-scoped read just returned.
//
// FollowUp/set's one operation is `dismiss`: the user's Done. It appends an
// Email `updated` change row (store.DismissNudge), so every client sees the
// message change and re-asks.

// followUpCacheSize bounds the parse cache. One entry is a few hundred bytes
// (addresses and header facts, never a body), so this is a few MB at most.
const followUpCacheSize = 20000

type parsedEntry struct {
	key string
	msg nudge.Message // only the parsed fields are meaningful
	ok  bool
}

// followUpCache is a small LRU of parsed message content.
type followUpCache struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List
	limit int
}

func newFollowUpCache(limit int) *followUpCache {
	return &followUpCache{items: map[string]*list.Element{}, order: list.New(), limit: limit}
}

func (c *followUpCache) get(key string) (parsedEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(parsedEntry), true
	}
	return parsedEntry{}, false
}

func (c *followUpCache) put(e parsedEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[e.key]; ok {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.items[e.key] = c.order.PushFront(e)
	for c.order.Len() > c.limit {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(parsedEntry).key)
	}
}

// followUpTimeNow is the clock; a test replaces it.
var followUpTimeNow = func() time.Time { return time.Now().UTC() }

func (a *API) followUpGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if a.raw == nil {
		return nil, &MethodError{Type: errServerFail, Description: "FollowUp/get: no message source configured"}
	}
	now := followUpTimeNow()

	var (
		prefs store.DeskPreferences
		set   store.FollowUpSet
		ids   store.NudgeIdentities
		disp  map[ulid.ULID]followUpDisplay
	)
	read := func(tx *store.UserTx) error {
		var e error
		if prefs, e = tx.GetDeskPreferences(ctx); e != nil {
			return e
		}
		if !prefs.FollowUp.Outbound && !prefs.FollowUp.Inbound {
			return nil
		}
		if ids, e = tx.NudgeIdentities(ctx); e != nil {
			return e
		}
		inboundAfter := time.Duration(prefs.FollowUp.InboundDays) * 24 * time.Hour
		minAge := nudge.OutboundAfter
		if prefs.FollowUp.Inbound && inboundAfter < minAge {
			minAge = inboundAfter
		}
		set, e = tx.FollowUpMessages(ctx, store.FollowUpQuery{Now: now, MinAge: minAge, Inbound: prefs.FollowUp.Inbound})
		return e
	}
	if err := a.store.UserTx(ctx, userID, read); err != nil {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}

	me := nudge.NewMe(ids.Addresses, ids.Domains)
	msgs := make([]nudge.Message, 0, len(set.Messages))
	unreadable := 0
	cache := a.followUps
	for _, r := range set.Messages {
		m := nudge.Message{
			ID: r.ID.String(), At: r.ReceivedAt, From: r.FromAddress,
			InSentFolder: r.InSentMailbox, ViaSubmission: r.ViaSubmission, FromIdentity: me.Is(r.FromAddress),
			MessageID: r.MessageIDHint, InReplyTo: r.InReplyTo, Answered: r.IsAnswered, Junk: r.IsJunk,
			ContextOnly: !set.Candidates[r.ID],
		}
		if r.ThreadID != (ulid.ULID{}) {
			m.Thread = r.ThreadID.String()
		}
		key := userID.String() + "/" + m.ID
		e, hit := cache.get(key)
		if !hit {
			e = parsedEntry{key: key}
			if raw, err := a.raw.ReadRaw(ctx, userID, r.ContentHash); err == nil {
				if _, perr := nudge.ParseContent(raw, &e.msg); perr == nil {
					e.ok = true
				}
			}
			cache.put(e)
		}
		if e.ok {
			m.Parsed = true
			m.To, m.Cc, m.Bcc, m.Facts = e.msg.To, e.msg.Cc, e.msg.Bcc, e.msg.Facts
			m.Question, m.Request = e.msg.Question, e.msg.Request
			if m.InReplyTo == "" {
				m.InReplyTo = e.msg.InReplyTo
			}
		} else {
			unreadable++
		}
		msgs = append(msgs, m)
	}
	dismissed := map[string]bool{}
	for id := range set.Dismissed {
		dismissed[id.String()] = true
	}
	due := nudge.Due(msgs, me, nudge.LiveConfig{
		Now: now, Outbound: prefs.FollowUp.Outbound, Inbound: prefs.FollowUp.Inbound,
		InboundAfter: time.Duration(prefs.FollowUp.InboundDays) * 24 * time.Hour,
	}, dismissed)

	if len(due) > 0 {
		dueIDs := make([]ulid.ULID, 0, len(due))
		for _, n := range due {
			dueIDs = append(dueIDs, ulid.MustParse(n.MessageID))
		}
		if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			disp, e = followUpDisplayFor(ctx, tx, dueIDs)
			return e
		}); err != nil {
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
	}

	byID := map[string]*nudge.Message{}
	for i := range msgs {
		byID[msgs[i].ID] = &msgs[i]
	}
	list := make([]map[string]any, 0, len(due))
	for _, n := range due {
		id := ulid.MustParse(n.MessageID)
		d, ok := disp[id]
		if !ok {
			continue // vanished between the two reads
		}
		var thread any
		if m := byID[n.MessageID]; m != nil && m.Thread != "" {
			thread = m.Thread
		}
		// who: for "no one answered", the first person it went to; for
		// "you haven't replied", who sent it.
		who := map[string]any{"name": d.fromName, "email": d.fromAddress}
		if n.Kind == nudge.KindOutbound {
			who = map[string]any{"name": "", "email": firstOther(byID[n.MessageID], me)}
		}
		list = append(list, map[string]any{
			"emailId":  n.MessageID,
			"threadId": thread,
			"kind":     string(n.Kind),
			"since":    jmapDate(n.Since),
			"dueAt":    jmapDate(n.DueAt),
			"subject":  d.subject,
			"who":      who,
		})
	}
	truncated := set.Truncated
	if truncated == nil {
		truncated = []string{}
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"list":      list,
		"settings":  followUpObject(prefs.FollowUp),
		// The bounds this answer was read under, so a client (and a person
		// debugging one) can tell "nothing due" from "nothing could be read".
		"truncated":  truncated,
		"unreadable": unreadable,
	}), nil
}

func firstOther(m *nudge.Message, me nudge.Me) string {
	if m == nil {
		return ""
	}
	for _, l := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, a := range l {
			if !me.Is(a) {
				return a
			}
		}
	}
	return ""
}

type followUpDisplay struct {
	subject, fromName, fromAddress string
}

func followUpDisplayFor(ctx context.Context, tx *store.UserTx, ids []ulid.ULID) (map[ulid.ULID]followUpDisplay, error) {
	rows, err := tx.FollowUpDisplay(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[ulid.ULID]followUpDisplay, len(rows))
	for _, r := range rows {
		out[r.ID] = followUpDisplay{subject: r.Subject, fromName: r.FromName, fromAddress: r.FromAddress}
	}
	return out, nil
}

// FollowUp/set: {accountId, dismiss: {emailId: kind}} — the user's Done.
// Result: {dismissed: [ids], notDismissed: {id: SetError}}. A message that is
// not the caller's is notFound, exactly as one that never existed.
func (a *API) followUpSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID string            `json:"accountId"`
		Dismiss   map[string]string `json:"dismiss"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if len(args.Dismiss) > 100 {
		return nil, &MethodError{Type: errInvalidArguments, Description: "dismiss at most 100 at a time"}
	}
	dismissed := []string{}
	notDismissed := map[string]*SetError{}
	for idStr, kind := range args.Dismiss {
		id, err := ulid.Parse(idStr)
		if err != nil {
			notDismissed[idStr] = &SetError{Type: setNotFound, Description: "no such email"}
			continue
		}
		kind = strings.TrimSpace(kind)
		var found bool
		if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			found, _, e = tx.DismissNudge(ctx, id, kind)
			return e
		}); err != nil {
			if errors.Is(err, store.ErrNudgeKind) {
				notDismissed[idStr] = &SetError{Type: setInvalidProps, Description: "kind must be outbound or inbound"}
				continue
			}
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if !found {
			notDismissed[idStr] = &SetError{Type: setNotFound, Description: "no such email"}
			continue
		}
		dismissed = append(dismissed, idStr)
	}
	var nd any
	if len(notDismissed) > 0 {
		nd = notDismissed
	}
	return mustMarshal(map[string]any{
		"accountId":    userID.String(),
		"dismissed":    dismissed,
		"notDismissed": nd,
	}), nil
}
