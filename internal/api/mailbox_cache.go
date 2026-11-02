// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"container/list"
	"sync"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// mailboxCacheUsers bounds the all-mailboxes cache: one entry per user, so
// this is the number of users whose latest answer is kept. An entry is the
// user's mailbox list (a few dozen small structs), so the bound is memory-safe
// by a wide margin.
const mailboxCacheUsers = 512

// Results of a Mailbox/get ids:null lookup, for the counter's label.
const (
	mailboxCacheHit  = "hit"
	mailboxCacheMiss = "miss"
)

// MailboxCacheResults lists every label the cache observer can receive, so the
// metrics sink can create its series at zero at boot.
func MailboxCacheResults() []string { return []string{mailboxCacheHit, mailboxCacheMiss} }

// WithMailboxCacheObserver installs the sink called once per Mailbox/get
// ids:null lookup with "hit" or "miss". Optional; nil observes nothing.
func WithMailboxCacheObserver(fn func(result string)) Option {
	return func(a *API) { a.onMailboxCache = fn }
}

// mailboxCache keeps, per user, the latest full Mailbox/get ids:null answer
// (every mailbox with its total/unread counts), keyed by the user's Email and
// Mailbox change sequences.
//
// WHY THE KEY IS THE PAIR OF SEQUENCES AND NOT A TIMER. Counts derive from
// message flags, placement and provenance; names and roles from the mailbox
// rows. Every mutation of any of those appends an Email or a Mailbox change row
// in the same transaction (the load-bearing invariant). The caller reads both
// sequences first, inside the same transaction that would recount, so an entry
// stored under (email, mailbox) is what a recount at that pair would return, or
// something newer that can never be served again (the sequences only grow, and
// a commit newer than the read has already moved them). Several server
// processes each hold their own cache and stay correct for the same reason.
//
// It holds the latest entry only (no history). Entries are never mutated after
// storing, and get/put copy, so a caller cannot alter a cached answer.
type mailboxCache struct {
	mu    sync.Mutex
	limit int
	items map[ulid.ULID]*list.Element
	order *list.List // front = most recently used
}

type mailboxCacheEntry struct {
	userID     ulid.ULID
	emailSeq   int64
	mailboxSeq int64
	ids        []ulid.ULID         // every mailbox id, in ListMailboxIDs order
	views      []store.MailboxView // the counted views for ids (unordered)
}

func newMailboxCache(limit int) *mailboxCache {
	return &mailboxCache{limit: limit, items: map[ulid.ULID]*list.Element{}, order: list.New()}
}

// get returns a copy of the user's entry when it was stored at exactly this
// pair of sequences.
func (c *mailboxCache) get(userID ulid.ULID, emailSeq, mailboxSeq int64) (ids []ulid.ULID, views []store.MailboxView, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, found := c.items[userID]
	if !found {
		return nil, nil, false
	}
	e := el.Value.(*mailboxCacheEntry)
	if e.emailSeq != emailSeq || e.mailboxSeq != mailboxSeq {
		return nil, nil, false
	}
	c.order.MoveToFront(el)
	return append([]ulid.ULID(nil), e.ids...), append([]store.MailboxView(nil), e.views...), true
}

// put replaces the user's entry (latest only) and evicts the least recently
// used user beyond the bound.
func (c *mailboxCache) put(userID ulid.ULID, emailSeq, mailboxSeq int64, ids []ulid.ULID, views []store.MailboxView) {
	e := &mailboxCacheEntry{
		userID: userID, emailSeq: emailSeq, mailboxSeq: mailboxSeq,
		ids:   append([]ulid.ULID(nil), ids...),
		views: append([]store.MailboxView(nil), views...),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, found := c.items[userID]; found {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.items[userID] = c.order.PushFront(e)
	for c.order.Len() > c.limit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*mailboxCacheEntry).userID)
	}
}

func (a *API) noteMailboxCache(result string) {
	if a.onMailboxCache != nil {
		a.onMailboxCache(result)
	}
}
