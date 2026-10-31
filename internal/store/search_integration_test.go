// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the search store surface (mailroom#20, spec §7): the
// IndexMessage projection the indexer consumes, the reindex walk, the
// name→id lookups a search query needs, and the candidate-id restriction the
// text-filtered Email/query applies. Runs against the throwaway Postgres
// provisioned by scripts/integration-test.sh; skips when the env is absent
// (harness_test.go).
//
// The load-bearing property proved here is ISOLATION: the indexer reads through
// UserTx, so RLS scopes it — one user's indexer can never see another user's
// messages, labels, or accounts, and therefore cannot index them.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// mustExec runs a seeding statement as the superuser (fixtures only; every
// assertion below still runs through the mailroom_app role, so RLS applies).
func mustExec(t *testing.T, h *harness, sql string, args ...any) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// mustTime parses an RFC 3339 timestamp for a test bound.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestIndexMessagesProjection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-proj")
	st := h.appStore()

	userID := ulid.MustParse(u.ID)
	msgID := ulid.MustParse(u.MessageID)

	var msgs []IndexMessage
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		var err error
		msgs, err = tx.IndexMessages(ctx, []ulid.ULID{msgID})
		return err
	}); err != nil {
		t.Fatalf("IndexMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	m := msgs[0]
	if m.ID != msgID {
		t.Errorf("id: got %s, want %s", m.ID, msgID)
	}
	if m.Subject != "seed message" {
		t.Errorf("subject: got %q", m.Subject)
	}
	if m.FromAddress != "friend@example.com" {
		t.Errorf("from: got %q", m.FromAddress)
	}
	if len(m.ContentHash) != 32 {
		t.Errorf("content hash: got %d bytes, want 32 (the indexer needs it to read the blob)", len(m.ContentHash))
	}
	if m.ReceivedAt.IsZero() {
		t.Error("received_at is zero — date filters would be unindexable")
	}
	// The projection must carry the label / mailbox / account membership the
	// label:, mailbox and account: filters are built on.
	if len(m.LabelIDs) != 1 {
		t.Errorf("labels: got %d, want 1", len(m.LabelIDs))
	}
	if len(m.MailboxIDs) != 1 {
		t.Errorf("mailboxes: got %d, want 1", len(m.MailboxIDs))
	}
	if len(m.AccountIDs) != 1 {
		t.Errorf("accounts: got %d, want 1", len(m.AccountIDs))
	}
	if m.AccountIDs[0].String() != u.AccountID {
		t.Errorf("account: got %s, want %s", m.AccountIDs[0], u.AccountID)
	}
}

// A DRAFT — locally authored, not yet APPENDed to the server, so it has NO
// message_provenance row at all — must still get a mailbox/account facet in
// the index, derived from local_mailbox_id (the same precedence
// effectiveMembershipSQL uses for the JMAP read path). Before mailroom#90 the
// provenance projection INNER JOINed app.message_provenance, so a draft got
// neither facet and was invisible to mailbox- and account-scoped search (only
// free-text search, which does not go through provenance, could find it).
func TestIndexMessagesFindsDraftByLocalMailbox(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-draft")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	draftsMailboxID := NewULID().String()
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	                VALUES ($1, $2, $3, 'Drafts', 'Drafts')`,
		draftsMailboxID, u.ID, u.AccountID)

	hash := make([]byte, 32)
	hash[0] = 0xDD
	mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	                VALUES ($1, $2, 10, 20, $3)`, u.ID, hash, NewULID().String())
	draftID := NewULID()
	mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at,
	                size_bytes, is_draft, local_mailbox_id)
	                VALUES ($1, $2, $3, 'unsent draft', now(), 10, true, $4)`,
		draftID.String(), u.ID, hash, draftsMailboxID)

	var msgs []IndexMessage
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		var err error
		msgs, err = tx.IndexMessages(ctx, []ulid.ULID{draftID})
		return err
	}); err != nil {
		t.Fatalf("IndexMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if len(m.MailboxIDs) != 1 || m.MailboxIDs[0].String() != draftsMailboxID {
		t.Errorf("mailbox ids = %v, want exactly [%s] (its local_mailbox_id) — a mailbox-scoped search would miss this draft", m.MailboxIDs, draftsMailboxID)
	}
	if len(m.AccountIDs) != 1 || m.AccountIDs[0].String() != u.AccountID {
		t.Errorf("account ids = %v, want exactly [%s] — an account:-scoped search would miss this draft", m.AccountIDs, u.AccountID)
	}
}

// THE isolation test (data-model §5): the indexer runs through UserTx, so one
// user's index build can never read another's rows. A cross-user id must come
// back as "not found", indistinguishable from a message that does not exist.
func TestIndexMessagesAreUserIsolated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	alice := seedUser(t, h.super, "search-iso-a")
	mallory := seedUser(t, h.super, "search-iso-b")
	st := h.appStore()

	aliceMsg := ulid.MustParse(alice.MessageID)
	malloryID := ulid.MustParse(mallory.ID)

	// Mallory's indexer asks for Alice's message by id, explicitly.
	var msgs []IndexMessage
	if err := st.UserTx(ctx, malloryID, func(tx *UserTx) error {
		var err error
		msgs, err = tx.IndexMessages(ctx, []ulid.ULID{aliceMsg})
		return err
	}); err != nil {
		t.Fatalf("IndexMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("ISOLATION BREACH: mallory's indexer read %d of alice's messages", len(msgs))
	}

	// And the reindex WALK only ever yields her own.
	var ids []ulid.ULID
	if err := st.UserTx(ctx, malloryID, func(tx *UserTx) error {
		var err error
		ids, err = tx.MessageIDsAfter(ctx, ulid.ULID{}, 1000)
		return err
	}); err != nil {
		t.Fatalf("MessageIDsAfter: %v", err)
	}
	for _, id := range ids {
		if id == aliceMsg {
			t.Fatal("ISOLATION BREACH: alice's message appeared in mallory's reindex walk")
		}
	}
	if len(ids) != 1 || ids[0].String() != mallory.MessageID {
		t.Fatalf("mallory's walk returned %d ids, want exactly her own", len(ids))
	}

	// Fail closed: no identity, no rows (and therefore no index).
	if err := st.UserTx(ctx, ulid.ULID{}, func(tx *UserTx) error { return nil }); err != ErrNoIdentity {
		t.Fatalf("a zero user id must be rejected, got %v", err)
	}
}

// The reindex walk is a keyset scan: ascending id order, exclusive low bound, so
// a rebuild can page through an archive without an offset or a held snapshot.
func TestMessageIDsAfterWalksInOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-walk")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	// Add two more messages so the walk has something to page through.
	extra := []ulid.ULID{NewULID(), NewULID()}
	for i, id := range extra {
		hash := make([]byte, 32)
		hash[0] = byte(i + 1)
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		                VALUES ($1, $2, 10, 20, $3)`, u.ID, hash, NewULID().String())
		mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
		                VALUES ($1, $2, $3, 'walk', now(), 10)`, id.String(), u.ID, hash)
	}

	var all []ulid.ULID
	var after ulid.ULID
	for {
		var page []ulid.ULID
		if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
			var err error
			page, err = tx.MessageIDsAfter(ctx, after, 2) // page size 2
			return err
		}); err != nil {
			t.Fatalf("walk: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, id := range page {
			if len(all) > 0 && id.Compare(all[len(all)-1]) <= 0 {
				t.Fatalf("walk is not strictly ascending: %s after %s", id, all[len(all)-1])
			}
			all = append(all, id)
		}
		after = page[len(page)-1]
	}
	if len(all) != 3 {
		t.Fatalf("walk yielded %d messages, want 3 (paging lost or duplicated rows)", len(all))
	}
}

// label:Work and account:personal are typed as names; these are the lookups that
// turn them into the ids the index actually stores.
func TestNameLookups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-names")
	other := seedUser(t, h.super, "search-names-other")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		// Case-insensitive by design: users do not type their labels back
		// verbatim.
		id, found, err := tx.LabelIDByName(ctx, "LABEL-SEARCH-NAMES")
		if err != nil {
			return err
		}
		if !found || id == (ulid.ULID{}) {
			t.Error("label lookup failed (case-insensitively)")
		}

		acct, found, err := tx.AccountIDByName(ctx, "primary")
		if err != nil {
			return err
		}
		if !found || acct.String() != u.AccountID {
			t.Errorf("account lookup: got %s found=%v, want %s", acct, found, u.AccountID)
		}

		// An unknown name is not found — which the search service turns into
		// "match nothing", never "no constraint".
		if _, found, err := tx.LabelIDByName(ctx, "does-not-exist"); err != nil {
			return err
		} else if found {
			t.Error("an unknown label name must not resolve")
		}

		// A name belonging to ANOTHER user does not resolve either: RLS, not a
		// WHERE clause we could forget.
		if _, found, err := tx.LabelIDByName(ctx, "label-search-names-other"); err != nil {
			return err
		} else if found {
			t.Error("ISOLATION BREACH: another user's label name resolved")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookups: %v", err)
	}
	_ = other
}

// The candidate-id restriction: the index says WHICH messages match the text, and
// Postgres still applies the structured filters, the sort, the total, and the
// window. An empty candidate set must return nothing — not everything.
func TestQueryEmailsRestrictIDs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-restrict")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)
	msgID := ulid.MustParse(u.MessageID)

	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		// nil = no text filter: everything the structured filter allows.
		ids, total, err := tx.QueryEmails(ctx, EmailQuery{})
		if err != nil {
			return err
		}
		if total != 1 || len(ids) != 1 {
			t.Fatalf("unrestricted: got %d ids / total %d, want 1", len(ids), total)
		}

		// A candidate set containing the message: it survives.
		ids, total, err = tx.QueryEmails(ctx, EmailQuery{RestrictIDs: []ulid.ULID{msgID}})
		if err != nil {
			return err
		}
		if total != 1 || len(ids) != 1 || ids[0] != msgID {
			t.Fatalf("restricted to itself: got %d ids / total %d", len(ids), total)
		}

		// A candidate set NOT containing it: filtered out.
		ids, total, err = tx.QueryEmails(ctx, EmailQuery{RestrictIDs: []ulid.ULID{NewULID()}})
		if err != nil {
			return err
		}
		if total != 0 || len(ids) != 0 {
			t.Fatalf("restricted to another id: got %d ids / total %d, want 0", len(ids), total)
		}

		// THE dangerous case: an EMPTY (but non-nil) candidate set means "the
		// search matched nothing" and must return nothing. If this ever returns
		// the whole mailbox, a text search that found no results would show the
		// user everything.
		ids, total, err = tx.QueryEmails(ctx, EmailQuery{RestrictIDs: []ulid.ULID{}})
		if err != nil {
			return err
		}
		if total != 0 || len(ids) != 0 {
			t.Fatalf("EMPTY candidate set returned %d ids / total %d — a no-match search would show everything", len(ids), total)
		}
		return nil
	}); err != nil {
		t.Fatalf("QueryEmails: %v", err)
	}
}

// IndexStalenessSeconds turns the sequence lag into the wall-clock age of the
// oldest un-indexed Email change — spec §7's "search may be N minutes stale"
// (mailroom#41).
func TestIndexStalenessSeconds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-staleness")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	// seedUser already wrote change_log seq 1 and 2 (both ~now()). Add a third,
	// pending change 10 minutes in the past — the oldest un-indexed row once the
	// index has only caught up to seq 2.
	mustExec(t, h, `INSERT INTO app.change_log (user_id, object_type, seq, object_id, op, created_at)
	                VALUES ($1, 'Email', 3, $2, 'updated', now() - interval '10 minutes')`,
		u.ID, u.MessageID)

	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		// Caught up (sinceSeq == head): no rows past it, so staleness is 0.
		secs, err := tx.IndexStalenessSeconds(ctx, 3)
		if err != nil {
			return err
		}
		if secs != 0 {
			t.Errorf("caught up: staleness = %v, want 0", secs)
		}

		// Even further ahead than head: still 0, never negative.
		secs, err = tx.IndexStalenessSeconds(ctx, 100)
		if err != nil {
			return err
		}
		if secs != 0 {
			t.Errorf("ahead of head: staleness = %v, want 0", secs)
		}

		// Behind: the index has only processed seq 2, so seq 3 (10 minutes old)
		// is the oldest pending change.
		secs, err = tx.IndexStalenessSeconds(ctx, 2)
		if err != nil {
			return err
		}
		if secs < 590 || secs > 610 {
			t.Errorf("behind by one change: staleness = %v, want ~600 (10 minutes)", secs)
		}
		return nil
	}); err != nil {
		t.Fatalf("IndexStalenessSeconds: %v", err)
	}
}

// before/after are enforced in Postgres (exactly), not in the day-granular index.
func TestQueryEmailsDateBounds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := seedUser(t, h.super, "search-dates")
	st := h.appStore()
	userID := ulid.MustParse(u.ID)

	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		// The seed message was received now(); a window strictly in the past
		// excludes it, and one that contains "now" includes it.
		past := mustTime(t, "2000-01-01T00:00:00Z")
		soon := mustTime(t, "2999-01-01T00:00:00Z")

		ids, _, err := tx.QueryEmails(ctx, EmailQuery{Before: &past})
		if err != nil {
			return err
		}
		if len(ids) != 0 {
			t.Errorf("before a past date: got %d ids, want 0", len(ids))
		}

		ids, _, err = tx.QueryEmails(ctx, EmailQuery{After: &past, Before: &soon})
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			t.Errorf("within the window: got %d ids, want 1", len(ids))
		}

		ids, _, err = tx.QueryEmails(ctx, EmailQuery{After: &soon})
		if err != nil {
			return err
		}
		if len(ids) != 0 {
			t.Errorf("after a future date: got %d ids, want 0", len(ids))
		}
		return nil
	}); err != nil {
		t.Fatalf("QueryEmails: %v", err)
	}
}
