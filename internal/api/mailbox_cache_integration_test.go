// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The Mailbox/get ids:null cache (mailbox_cache.go): a repeat call with nothing
// changed is a hit and byte-identical; anything that moves a count or an
// attribute is a miss and shows the new value; users never share an entry; an
// explicit-ids request never touches the cache.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// cacheProbe counts the observer's results.
type cacheProbe struct {
	mu   sync.Mutex
	hit  int
	miss int
}

func (p *cacheProbe) observe(result string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result == mailboxCacheHit {
		p.hit++
	} else {
		p.miss++
	}
}

func (p *cacheProbe) counts() (hit, miss int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hit, p.miss
}

func getAllMailboxes(t *testing.T, h *apiHarness, userID ulid.ULID) (raw string, byID map[string]map[string]any) {
	t.Helper()
	out, mErr := h.api.mailboxGet(context.Background(), userID, mustJSON(t, map[string]any{"ids": nil}))
	if mErr != nil {
		t.Fatalf("Mailbox/get: %+v", mErr)
	}
	var res struct {
		List []map[string]any `json:"list"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID = map[string]map[string]any{}
	for _, m := range res.List {
		byID[m["id"].(string)] = m
	}
	return string(out), byID
}

func TestMailboxGetAllCache(t *testing.T) {
	h := newAPIHarness(t)
	probe := &cacheProbe{}
	h.api = New(h.store, WithMailboxCacheObserver(probe.observe))
	a, b := seedTwo(t, h.super)
	ctx := context.Background()
	inbox, trash := a.InboxID.String(), a.TrashID.String()

	expect := func(step string, wantHit, wantMiss int) {
		t.Helper()
		if hit, miss := probe.counts(); hit != wantHit || miss != wantMiss {
			t.Fatalf("%s: hits=%d misses=%d, want hits=%d misses=%d", step, hit, miss, wantHit, wantMiss)
		}
	}

	// (a) two calls, nothing between: miss then hit, identical bytes.
	first, boxes := getAllMailboxes(t, h, a.UserID)
	expect("first call", 0, 1)
	second, _ := getAllMailboxes(t, h, a.UserID)
	expect("second call", 1, 1)
	if first != second {
		t.Errorf("a cache hit changed the answer:\n first: %s\nsecond: %s", first, second)
	}
	if got := boxes[inbox]["unreadEmails"]; got != float64(1) {
		t.Fatalf("seeded inbox unread = %v, want 1", got)
	}

	// (e) another user never gets this entry, and A's entry survives B's.
	_, bBoxes := getAllMailboxes(t, h, b.UserID)
	expect("other user", 1, 2)
	if _, leaked := bBoxes[inbox]; leaked {
		t.Errorf("user B was served user A's mailbox")
	}
	if _, ok := bBoxes[b.InboxID.String()]; !ok {
		t.Errorf("user B does not see their own inbox")
	}
	getAllMailboxes(t, h, a.UserID)
	expect("A after B", 2, 2)

	// (f) an explicit-ids request bypasses the cache entirely.
	call(t, h.api.mailboxGet, a.UserID, map[string]any{"ids": []string{inbox}})
	expect("explicit ids", 2, 2)

	// (b) mark the message seen: next call is a miss and the unread count fell.
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{
		a.MessageID.String(): map[string]any{"keywords/$seen": true}}})
	_, boxes = getAllMailboxes(t, h, a.UserID)
	expect("after seen", 2, 3)
	if got := boxes[inbox]["unreadEmails"]; got != float64(0) {
		t.Errorf("unread after marking seen = %v, want 0", got)
	}
	if got := boxes[inbox]["totalEmails"]; got != float64(1) {
		t.Errorf("total after marking seen = %v, want 1", got)
	}
	getAllMailboxes(t, h, a.UserID)
	expect("after seen, repeat", 3, 3)

	// (c) move the message to Trash: miss, and the counts moved.
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{
		a.MessageID.String(): map[string]any{"mailboxIds": map[string]any{trash: true}}}})
	_, boxes = getAllMailboxes(t, h, a.UserID)
	expect("after move", 3, 4)
	if in, tr := boxes[inbox]["totalEmails"], boxes[trash]["totalEmails"]; in != float64(0) || tr != float64(1) {
		t.Errorf("after move: inbox total=%v trash total=%v, want 0 and 1", in, tr)
	}

	// (d) rename a mailbox the way a sync does: a Mailbox change only, no Email
	// change, so keying on the Email sequence alone would miss it.
	err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		_, err := tx.UpsertMailbox(ctx, store.MailboxUpsert{
			AccountID: a.AccountID, IMAPName: "Trash", DisplayName: "Bin", SpecialUse: `\Trash`})
		return err
	})
	if err != nil {
		t.Fatalf("rename mailbox: %v", err)
	}
	_, boxes = getAllMailboxes(t, h, a.UserID)
	expect("after rename", 3, 5)
	if got := boxes[trash]["name"]; got != "Bin" {
		t.Errorf("name after rename = %v, want Bin", got)
	}

	// User B was never touched by any of this: still a hit on their own entry.
	getAllMailboxes(t, h, b.UserID)
	expect("B untouched", 4, 5)
}
