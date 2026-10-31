// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// A search for `is:trashed` finds deleted mail (#623).
//
// Before this, the parser accepted `is:trashed` and the index matched the
// trashed messages, and then Email/query's trash predicate removed every one of
// them again, because a search is not scoped to the Trash mailbox. The search
// box returned nothing, with no error.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/search"
	"mailroom/internal/store"
)

// trashedSearchFixture is one user with a message of every kind the trash rule
// has to tell apart, all sharing the word "invoice" so a text search reaches
// each of them.
type trashedSearchFixture struct {
	h   *apiHarness
	f   apiFixture
	arc ulid.ULID

	live             ulid.ULID // in the Inbox, never deleted
	deletedHere      ulid.ULID // deleted in Mailroom (Email/set destroy)
	deletedElsewhere ulid.ULID // another client moved it to Trash: no trashed_at
	vanished         ulid.ULID // gone from the server; the reaper soft-deleted it
	stranded         ulid.ULID // filed in Archive while still trashed (the old move-out-of-Trash defect)
}

func newTrashedSearchFixture(t *testing.T) *trashedSearchFixture {
	t.Helper()
	ctx := context.Background()
	h := newAPIHarness(t)
	ix, err := search.NewIndexer(t.TempDir(), search.MemoryFactory, search.NewStoreSource(h.store), nil)
	if err != nil {
		t.Fatalf("search indexer: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	h.api.SetSearch(search.NewService(ix, search.NewStoreResolver(h.store)))

	f := seedAPIUser(t, h.super, "trashed-"+store.NewULID().String())
	x := &trashedSearchFixture{h: h, f: f}
	x.arc = seedMailbox(t, h.super, f, "Archive", `\Archive`)

	uid := 100
	// seed inserts one message: its own blob row, and a live copy in `live`
	// when that is non-nil. `extra` is a SET clause applied afterwards.
	seed := func(subject string, live *ulid.ULID, extra string, args ...any) ulid.ULID {
		t.Helper()
		id := store.NewULID()
		hash := sha256.Sum256([]byte(id.String()))
		exec := func(sql string, a ...any) {
			t.Helper()
			if _, err := h.super.Exec(ctx, sql, a...); err != nil {
				t.Fatalf("seed %q: %v", subject, err)
			}
		}
		exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		      VALUES ($1,$2,100,132,$3)`, f.UserID.String(), hash[:], store.NewULID().String())
		exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		        from_name, from_address, received_at, size_bytes)
		      VALUES ($1,$2,$3,$4,$5,'a preview','Friend','friend@example.com', now(), 100)`,
			id.String(), f.UserID.String(), hash[:], f.ThreadID.String(), subject)
		if live != nil {
			uid++
			exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			      VALUES ($1,$2,$3,$4,$5,$6,1)`,
				store.NewULID().String(), f.UserID.String(), id.String(),
				f.AccountID.String(), live.String(), uid)
		}
		if extra != "" {
			exec(`UPDATE app.messages SET `+extra+` WHERE user_id = $1 AND id = $2`,
				append([]any{f.UserID.String(), id.String()}, args...)...)
		}
		return id
	}

	inbox, trash := f.InboxID, f.TrashID
	x.live = seed("invoice live", &inbox, "")
	x.deletedHere = seed("invoice deleted here", &inbox, "")
	x.deletedElsewhere = seed("invoice deleted elsewhere", &trash, "")
	x.vanished = seed("invoice vanished", nil,
		"trashed_at = now(), vanished_at = now(), purge_after = now() + interval '30 days'")
	x.stranded = seed("invoice stranded", &trash,
		"trashed_at = now(), purge_after = now() + interval '30 days', moved_mailbox_id = $3", x.arc.String())

	// Deleted the way the web client deletes, so trashed_at and the Trash
	// placement come from the real path rather than from this fixture.
	call(t, h.api.emailSet, f.UserID, map[string]any{"destroy": []string{x.deletedHere.String()}})

	if _, err := ix.Reindex(ctx, f.UserID); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	return x
}

// query runs Email/query through the method table, as dispatch does, and
// returns the ids as a set.
func (x *trashedSearchFixture) query(t *testing.T, q string) map[string]bool {
	t.Helper()
	res, mErr := x.h.api.methods["Email/query"](context.Background(), x.f.UserID,
		mustJSON(t, map[string]any{"filter": map[string]any{"query": q}}))
	if mErr != nil {
		t.Fatalf("Email/query %q: %+v", q, mErr)
	}
	return idSet(t, res)
}

func idSet(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode Email/query: %v", err)
	}
	set := make(map[string]bool, len(out.IDs))
	for _, id := range out.IDs {
		set[id] = true
	}
	return set
}

func TestSearchIsTrashedFindsDeletedMail(t *testing.T) {
	x := newTrashedSearchFixture(t)

	name := map[ulid.ULID]string{
		x.live: "live", x.deletedHere: "deleted in Mailroom", x.deletedElsewhere: "moved to Trash by another client",
		x.vanished: "vanished from the server", x.stranded: "filed elsewhere while trashed",
		x.f.MessageID: "the fixture's own inbox message",
	}
	want := func(t *testing.T, q string, got map[string]bool, in []ulid.ULID, out []ulid.ULID) {
		t.Helper()
		for _, id := range in {
			if !got[id.String()] {
				t.Errorf("%q: missing the message %s", q, name[id])
			}
		}
		for _, id := range out {
			if got[id.String()] {
				t.Errorf("%q: returned the message %s, which it must not", q, name[id])
			}
		}
	}

	deleted := []ulid.ULID{x.deletedHere, x.deletedElsewhere, x.stranded}
	notDeleted := []ulid.ULID{x.live, x.vanished, x.f.MessageID}

	for _, q := range []string{"is:trashed", "invoice is:trashed", "is:deleted", "is:trash invoice"} {
		got := x.query(t, q)
		want(t, q, got, deleted, notDeleted)
		if len(got) != len(deleted) {
			t.Errorf("%q returned %d messages, want exactly the %d deleted ones", q, len(got), len(deleted))
		}
	}

	// A search that does not ask for trash gets no deleted mail — including mail
	// another client moved to Trash (#631). That message is NOT flagged trashed
	// in the index (the flag is trashed_at alone), so the text match returns it
	// as a candidate and only the Postgres predicate can drop it. Through the real
	// index, this is what pins that it does.
	for _, q := range []string{"invoice", "invoice -is:trashed"} {
		want(t, q, x.query(t, q), []ulid.ULID{x.live},
			[]ulid.ULID{x.deletedHere, x.deletedElsewhere, x.vanished, x.stranded})
	}
}

// Postgres decides what is trashed, not the index. A message restored after
// the index last saw it is still flagged trashed THERE, and must not be
// returned; one another client moved to Trash was never flagged at all, and
// must be.
func TestSearchIsTrashedAsksPostgresNotTheIndex(t *testing.T) {
	x := newTrashedSearchFixture(t)
	ctx := context.Background()

	// Restored behind the index's back: no reindex after this.
	if _, err := x.h.super.Exec(ctx, `
		UPDATE app.messages SET trashed_at = NULL, purge_after = NULL, moved_mailbox_id = $3
		 WHERE user_id = $1 AND id = $2`,
		x.f.UserID.String(), x.deletedHere.String(), x.f.InboxID.String()); err != nil {
		t.Fatal(err)
	}
	got := x.query(t, "invoice is:trashed")
	if got[x.deletedHere.String()] {
		t.Error("a restored message was returned by is:trashed because the index still said it was trashed")
	}
	if !got[x.deletedElsewhere.String()] {
		t.Error("mail another client moved to Trash is missing: the index filtered on its own trashed flag, " +
			"which only knows about Mailroom's deletes")
	}
}

// The highlights for a result page are asked for with the SAME filter. If the
// snippet query kept `is:trashed` for the index, mail another client trashed
// would match nothing there and come back with no highlight.
func TestSearchIsTrashedHighlightsMailTrashedElsewhere(t *testing.T) {
	x := newTrashedSearchFixture(t)
	res, mErr := x.h.api.methods["SearchSnippet/get"](context.Background(), x.f.UserID, mustJSON(t, map[string]any{
		"filter":   map[string]any{"query": "invoice is:trashed"},
		"emailIds": []string{x.deletedElsewhere.String()},
	}))
	if mErr != nil {
		t.Fatalf("SearchSnippet/get: %+v", mErr)
	}
	var out searchSnippetGetResult
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.List) != 1 || out.List[0].Subject == nil {
		t.Fatalf("no subject highlight for mail trashed elsewhere: %+v", out.List)
	}
}

// `is:trashed` on its own leaves the index nothing to answer, so it works on a
// server with no search backend. With any text beside it, it still needs one.
func TestSearchIsTrashedAloneNeedsNoIndex(t *testing.T) {
	h := newAPIHarness(t) // no SetSearch: the no-backend Email/query
	f := seedAPIUser(t, h.super, "trashed-noindex-"+store.NewULID().String())
	call(t, h.api.emailSet, f.UserID, map[string]any{"destroy": []string{f.MessageID.String()}})

	raw, mErr := h.api.emailQuery(context.Background(), f.UserID,
		mustJSON(t, map[string]any{"filter": map[string]any{"query": "is:trashed"}}))
	if mErr != nil {
		t.Fatalf("is:trashed without an index: %+v", mErr)
	}
	if got := idSet(t, raw); len(got) != 1 || !got[f.MessageID.String()] {
		t.Fatalf("is:trashed without an index = %v, want the one deleted message", got)
	}

	_, mErr = h.api.emailQuery(context.Background(), f.UserID,
		mustJSON(t, map[string]any{"filter": map[string]any{"query": "invoice is:trashed"}}))
	if mErr == nil || mErr.Type != errUnsupportedFilter {
		t.Fatalf("text beside is:trashed without an index = %+v, want unsupportedFilter", mErr)
	}
}
