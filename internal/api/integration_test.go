// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the JMAP method surface against a real Postgres with
// db/schema.sql applied (run via scripts/integration-test.sh). They exercise
// Email/Mailbox/Thread reads, the Email/set mutation path (canonical change +
// change_log + writeback intent), the request pipeline with result references,
// and — realizing data-model §5 test 4 — a cross-user negative for every
// endpoint: authenticated as A, requesting B's object ids yields notFound/empty,
// never B's data.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// testUID returns an IMAP UID that no other call in this test process has
// returned. It mirrors internal/store's helper of the same name, which says why
// a UID taken from the millisecond clock collided in provenance_live_uq (#550).
func testUID() int64 { return testUIDs.Add(1) }

var testUIDs = func() *atomic.Int64 {
	var n atomic.Int64
	n.Store(time.Now().UnixMilli() % 1_000_000_000)
	return &n
}()

type apiFixture struct {
	UserID    ulid.ULID
	Subject   string
	AccountID ulid.ULID
	InboxID   ulid.ULID
	TrashID   ulid.ULID
	ThreadID  ulid.ULID
	MessageID ulid.ULID
	LabelID   ulid.ULID
	LabelAtom string
}

// apiHarness mirrors internal/store's harness: super pool for seeding, app pool
// for the Store the API runs on. Skips when the throwaway DB is not configured.
type apiHarness struct {
	super *pgxpool.Pool
	api   *API
	store *store.Store
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)
	st := store.NewFromPool(app)
	return &apiHarness{super: super, api: New(st), store: st}
}

// seedAPIUser inserts a minimal but complete object graph for one user: an
// account, an Inbox and a Trash mailbox, a thread, one message in the Inbox, a
// label with a keyword mapping, and the change machinery (seq/log/floor) so
// state strings and /changes have data.
func seedAPIUser(t *testing.T, super *pgxpool.Pool, tag string) apiFixture {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
	}
	f := apiFixture{
		UserID:    store.NewULID(),
		Subject:   "oidc-" + tag,
		AccountID: store.NewULID(),
		InboxID:   store.NewULID(),
		TrashID:   store.NewULID(),
		ThreadID:  store.NewULID(),
		MessageID: store.NewULID(),
		LabelID:   store.NewULID(),
		LabelAtom: "MailroomLabel-" + tag,
	}
	hash := sha256.Sum256([]byte("blob-" + tag))

	exec(`INSERT INTO app.users (id, oidc_subject, email, display_name)
	      VALUES ($1,$2,$3,$4)`, f.UserID.String(), f.Subject, tag+"@example.com", "User "+tag)
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1,$2,'primary','mail.example.com','smtp.example.com',$3,$4)`,
		f.AccountID.String(), f.UserID.String(), []byte{0x01}, store.NewULID().String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1,$2,$3,'INBOX','Inbox','\Inbox')`,
		f.InboxID.String(), f.UserID.String(), f.AccountID.String())
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1,$2,$3,'Trash','Trash','\Trash')`,
		f.TrashID.String(), f.UserID.String(), f.AccountID.String())
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1,$2,100,132,$3)`, f.UserID.String(), hash[:], store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject, message_count, last_received_at)
	      VALUES ($1,$2,'seed thread',1, now())`, f.ThreadID.String(), f.UserID.String())
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
	        from_name, from_address, received_at, size_bytes, message_id_hint)
	      VALUES ($1,$2,$3,$4,'seed subject','a preview','Friend','friend@example.com', now(), 100, $5)`,
		f.MessageID.String(), f.UserID.String(), hash[:], f.ThreadID.String(), "<seed-"+tag+"@example.com>")
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
	      VALUES ($1,$2,$3,$4,$5,1,1)`,
		store.NewULID().String(), f.UserID.String(), f.MessageID.String(), f.AccountID.String(), f.InboxID.String())
	exec(`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`,
		f.LabelID.String(), f.UserID.String(), "label-"+tag)
	exec(`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom) VALUES ($1,$2,$3)`,
		f.UserID.String(), f.LabelID.String(), f.LabelAtom)
	exec(`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
		f.UserID.String(), f.MessageID.String(), f.LabelID.String())

	// Change machinery: Email/Thread each at seq 1, Mailbox at seq 2.
	exec(`INSERT INTO app.change_seq (user_id, object_type, last_seq)
	      VALUES ($1,'Email',1),($1,'Thread',1),($1,'Mailbox',2)`, f.UserID.String())
	exec(`INSERT INTO app.change_floor (user_id, object_type, floor_seq)
	      VALUES ($1,'Email',0),($1,'Thread',0),($1,'Mailbox',0)`, f.UserID.String())
	exec(`INSERT INTO app.change_log (user_id, object_type, seq, object_id, op)
	      VALUES ($1,'Email',1,$2,'created'),
	             ($1,'Thread',1,$3,'created'),
	             ($1,'Mailbox',1,$4,'created'),
	             ($1,'Mailbox',2,$5,'created')`,
		f.UserID.String(), f.MessageID.String(), f.ThreadID.String(), f.InboxID.String(), f.TrashID.String())

	return f
}

func seedTwo(t *testing.T, super *pgxpool.Pool) (a, b apiFixture) {
	run := store.NewULID().String()
	return seedAPIUser(t, super, "a-"+run), seedAPIUser(t, super, "b-"+run)
}

// call invokes a method handler and fails on a method-level error.
func call(t *testing.T, fn methodFn, userID ulid.ULID, args any) map[string]any {
	t.Helper()
	raw, mErr := fn(context.Background(), userID, mustJSON(t, args))
	if mErr != nil {
		t.Fatalf("method error: %+v", mErr)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func TestUserIDForSubject(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	id, found, err := h.store.UserIDForSubject(ctx, a.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if !found || id != a.UserID {
		t.Fatalf("UserIDForSubject(%q) = %v,%v want %v,true", a.Subject, id, found, a.UserID)
	}
	if _, found, err := h.store.UserIDForSubject(ctx, "no-such-subject"); err != nil || found {
		t.Errorf("unknown subject: found=%v err=%v, want false,nil", found, err)
	}
}

func TestEmailGetAndCrossUser(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	// A gets its own message.
	res := call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{a.MessageID.String()}})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("A/get own: list len %d, want 1", len(list))
	}
	obj := list[0].(map[string]any)
	if obj["id"] != a.MessageID.String() {
		t.Errorf("id = %v", obj["id"])
	}
	if _, ok := obj["mailboxIds"].(map[string]any)[a.InboxID.String()]; !ok {
		t.Errorf("mailboxIds missing inbox: %v", obj["mailboxIds"])
	}
	if _, ok := obj["keywords"].(map[string]any)[a.LabelAtom]; !ok {
		t.Errorf("keywords missing label atom %q: %v", a.LabelAtom, obj["keywords"])
	}

	// A requests B's message id → notFound, never B's data (RLS).
	res = call(t, h.api.emailGet, a.UserID, map[string]any{"ids": []string{b.MessageID.String()}})
	if len(res["list"].([]any)) != 0 {
		t.Errorf("A/get B: leaked %d objects", len(res["list"].([]any)))
	}
	nf := res["notFound"].([]any)
	if len(nf) != 1 || nf[0] != b.MessageID.String() {
		t.Errorf("A/get B: notFound = %v, want [%s]", nf, b.MessageID)
	}
}

func TestEmailQueryCrossUser(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	res := call(t, h.api.emailQuery, a.UserID, map[string]any{})
	ids := res["ids"].([]any)
	if len(ids) != 1 || ids[0] != a.MessageID.String() {
		t.Fatalf("A/query = %v, want [%s]", ids, a.MessageID)
	}

	// Filtering by B's mailbox returns nothing for A (B's mailbox is invisible).
	res = call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inMailbox": b.InboxID.String()},
	})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("A/query inMailbox=B: %d ids, want 0", n)
	}
}

// TestEmailQueryCollapseThreads proves collapseThreads (#210, RFC 8621
// §4.4.4) is actually wired end to end through the JSON args: a second
// message in the fixture's thread makes Email/query return BOTH messages
// uncollapsed (the control — without it, a bug that collapsed everything
// down to one row regardless of thread would look identical to the
// collapsed case), and exactly the newer one collapsed, with `total` equal
// to the collapsed count.
func TestEmailQueryCollapseThreads(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// msg2 lands in a.ThreadID, alongside the seeded a.MessageID, and is
	// received AFTER it — so it sorts first under the default (received-desc)
	// order and is the representative collapseThreads must keep.
	msg2 := insertInboxMessage(t, h.super, a, "collapse", 2)

	uncollapsed := call(t, h.api.emailQuery, a.UserID, map[string]any{"calculateTotal": true})
	ids := uncollapsed["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("control (no collapseThreads): ids = %v, want 2 (both thread messages)", ids)
	}
	if uncollapsed["total"].(float64) != 2 {
		t.Fatalf("control (no collapseThreads): total = %v, want 2", uncollapsed["total"])
	}

	collapsed := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"collapseThreads": true,
		"calculateTotal":  true,
	})
	ids = collapsed["ids"].([]any)
	if len(ids) != 1 || ids[0] != msg2.String() {
		t.Fatalf("collapseThreads: ids = %v, want [%s] (the newer message)", ids, msg2)
	}
	if collapsed["total"].(float64) != 1 {
		t.Errorf("collapseThreads: total = %v, want 1 (the collapsed count)", collapsed["total"])
	}
}

func TestEmailChanges(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// Initial sync (since s:0) reports the seeded message as created.
	res := call(t, h.api.emailChanges, a.UserID, map[string]any{"sinceState": "s:0"})
	created := res["created"].([]any)
	if len(created) != 1 || created[0] != a.MessageID.String() {
		t.Fatalf("created = %v, want [%s]", created, a.MessageID)
	}
	if res["newState"] != "s:1" {
		t.Errorf("newState = %v, want s:1", res["newState"])
	}

	// Up to date (since current) → empty.
	res = call(t, h.api.emailChanges, a.UserID, map[string]any{"sinceState": "s:1"})
	if len(res["created"].([]any)) != 0 || len(res["updated"].([]any)) != 0 {
		t.Errorf("no-op changes not empty: %v", res)
	}

	// An impossible future state → cannotCalculateChanges (method error).
	_, mErr := h.api.emailChanges(context.Background(), a.UserID, mustJSON(t, map[string]any{"sinceState": "s:99"}))
	if mErr == nil || mErr.Type != errCannotCalculateChanges {
		t.Errorf("future state: got %+v, want cannotCalculateChanges", mErr)
	}
}

func TestEmailSetMarkSeenStarLabelTrash(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)
	ctx := context.Background()

	// Mark read + star + remove the seeded label (labels are keywords), all in
	// one update patch. The atom is pre-applied by the seed, so removing it is a
	// real canonical change that also enqueues a writeback delta.
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{
				"keywords/$seen":          true,
				"keywords/$flagged":       true,
				"keywords/" + a.LabelAtom: nil, // remove the label
			},
		},
	})
	if _, ok := res["updated"].(map[string]any)[a.MessageID.String()]; !ok {
		t.Fatalf("message not in updated: %v", res)
	}
	if res["oldState"] == res["newState"] {
		t.Errorf("state did not advance: old=%v new=%v", res["oldState"], res["newState"])
	}

	// Canonical row updated, label removed, and writeback intents enqueued.
	seen, flagged, _ := h.canonicalFlags(t, a.UserID, a.MessageID)
	if !seen || !flagged {
		t.Errorf("flags: seen=%v flagged=%v, want true,true", seen, flagged)
	}
	if labels := h.countRows(t, "message_labels", a.UserID, a.MessageID); labels != 0 {
		t.Errorf("labels = %d, want 0 after removal", labels)
	}
	if intents := h.countRows(t, "writeback_intents", a.UserID, a.MessageID); intents == 0 {
		t.Errorf("no writeback intents enqueued")
	}

	// Destroy = soft trash: message still exists, trashed_at set, Email updated.
	res = call(t, h.api.emailSet, a.UserID, map[string]any{"destroy": []string{a.MessageID.String()}})
	destroyed := res["destroyed"].([]any)
	if len(destroyed) != 1 || destroyed[0] != a.MessageID.String() {
		t.Fatalf("destroyed = %v, want [%s]", destroyed, a.MessageID)
	}
	if _, _, trashed := h.canonicalFlags(t, a.UserID, a.MessageID); !trashed {
		t.Errorf("message not trashed after destroy")
	}

	// Cross-user: A cannot update B's message — notUpdated notFound, B untouched.
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{b.MessageID.String(): map[string]any{"keywords/$seen": true}},
	})
	se, ok := res["notUpdated"].(map[string]any)[b.MessageID.String()]
	if !ok || se.(map[string]any)["type"] != setNotFound {
		t.Errorf("A update B: notUpdated = %v, want notFound", res["notUpdated"])
	}
	if bSeen, _, bTrashed := h.canonicalFlags(t, b.UserID, b.MessageID); bSeen || bTrashed {
		t.Errorf("B's message was mutated by A: seen=%v trashed=%v", bSeen, bTrashed)
	}
	_ = ctx
}

// threadMuted reads a thread's mute state as the given user, for probes that
// must prove a write did NOT land (#51).
func (h *apiHarness) threadMuted(t *testing.T, userID, threadID ulid.ULID) (muted bool) {
	t.Helper()
	ctx := context.Background()
	err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		views, err := tx.GetThreads(ctx, []ulid.ULID{threadID})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Fatalf("owner cannot see thread %s", threadID)
		}
		muted = views[0].Muted
		return nil
	})
	if err != nil {
		t.Fatalf("threadMuted: %v", err)
	}
	return muted
}

// canonicalFlags reads a message's canonical flags as its owner (through the
// store's RLS-scoped path).
func (h *apiHarness) canonicalFlags(t *testing.T, userID, messageID ulid.ULID) (seen, flagged, trashed bool) {
	t.Helper()
	ctx := context.Background()
	err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		views, err := tx.GetEmails(ctx, []ulid.ULID{messageID})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Fatalf("owner cannot see message %s", messageID)
		}
		v := views[0]
		seen = hasKeyword(v.Keywords, store.KeywordSeen)
		flagged = hasKeyword(v.Keywords, store.KeywordFlagged)
		trashed = v.Trashed
		return nil
	})
	if err != nil {
		t.Fatalf("canonicalFlags: %v", err)
	}
	return seen, flagged, trashed
}

// countRows counts rows in a user-scoped table for a message, via the super
// pool (bypasses RLS — an independent check of what the store actually wrote).
func (h *apiHarness) countRows(t *testing.T, table string, userID, messageID ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.`+table+` WHERE user_id=$1 AND message_id=$2`,
		userID.String(), messageID.String()).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func hasKeyword(kws []string, want string) bool {
	for _, k := range kws {
		if k == want {
			return true
		}
	}
	return false
}

func TestMailbox(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	// query lists both of A's mailboxes.
	res := call(t, h.api.mailboxQuery, a.UserID, map[string]any{})
	if n := len(res["ids"].([]any)); n != 2 {
		t.Errorf("A/Mailbox/query = %d ids, want 2", n)
	}

	// get A's inbox → role inbox, total >= 1.
	res = call(t, h.api.mailboxGet, a.UserID, map[string]any{"ids": []string{a.InboxID.String()}})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("A/Mailbox/get inbox: %d, want 1", len(list))
	}
	if list[0].(map[string]any)["role"] != "inbox" {
		t.Errorf("inbox role = %v", list[0].(map[string]any)["role"])
	}

	// Cross-user: A requesting B's mailbox id → notFound.
	res = call(t, h.api.mailboxGet, a.UserID, map[string]any{"ids": []string{b.InboxID.String()}})
	if len(res["list"].([]any)) != 0 {
		t.Errorf("A/Mailbox/get B leaked data")
	}
	if nf := res["notFound"].([]any); len(nf) != 1 {
		t.Errorf("A/Mailbox/get B: notFound = %v", nf)
	}
}

func TestThread(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	res := call(t, h.api.threadGet, a.UserID, map[string]any{"ids": []string{a.ThreadID.String()}})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("A/Thread/get: %d, want 1", len(list))
	}
	emailIDs := list[0].(map[string]any)["emailIds"].([]any)
	if len(emailIDs) != 1 || emailIDs[0] != a.MessageID.String() {
		t.Errorf("emailIds = %v, want [%s]", emailIDs, a.MessageID)
	}

	// Cross-user: A requesting B's thread id → notFound.
	res = call(t, h.api.threadGet, a.UserID, map[string]any{"ids": []string{b.ThreadID.String()}})
	if len(res["list"].([]any)) != 0 {
		t.Errorf("A/Thread/get B leaked data")
	}
}

// insertInboxMessage seeds a second live message in user f's Inbox and advances
// the Email change log by one (a 'created' row at the given seq), mirroring what
// ingest does — so Email/queryChanges has a real created-entry to report.
func insertInboxMessage(t *testing.T, super *pgxpool.Pool, f apiFixture, tag string, seq int) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("insert message %s: %v", tag, err)
		}
	}
	msgID := store.NewULID()
	hash := sha256.Sum256([]byte("blob2-" + tag))
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1,$2,100,132,$3)`, f.UserID.String(), hash[:], store.NewULID().String())
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
	        from_name, from_address, received_at, size_bytes, message_id_hint)
	      VALUES ($1,$2,$3,$4,'second subject','preview2','Friend','friend@example.com', now(), 100, $5)`,
		msgID.String(), f.UserID.String(), hash[:], f.ThreadID.String(), "<second-"+tag+"@example.com>")
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
	      VALUES ($1,$2,$3,$4,$5,2,1)`,
		store.NewULID().String(), f.UserID.String(), msgID.String(), f.AccountID.String(), f.InboxID.String())
	exec(`UPDATE app.change_seq SET last_seq=$2 WHERE user_id=$1 AND object_type='Email'`,
		f.UserID.String(), seq)
	exec(`INSERT INTO app.change_log (user_id, object_type, seq, object_id, op)
	      VALUES ($1,'Email',$2,$3,'created')`, f.UserID.String(), seq, msgID.String())
	return msgID
}

// TestEmailQueryChangesStableDelta proves a stable inMailbox query chains
// query→queryChanges and returns a real delta: a new message shows up in `added`
// at its index, a trashed message shows up in `removed`, and the queryState
// advances in lockstep with the Email change state.
func TestEmailQueryChangesStableDelta(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// A structured inMailbox query is delta-stable: Email/query says so.
	inbox := map[string]any{"filter": map[string]any{"inMailbox": a.InboxID.String()}}
	q := call(t, h.api.emailQuery, a.UserID, inbox)
	if q["canCalculateChanges"] != true {
		t.Fatalf("canCalculateChanges = %v, want true", q["canCalculateChanges"])
	}
	if q["queryState"] != "s:1" {
		t.Fatalf("queryState = %v, want s:1", q["queryState"])
	}

	// A second message arrives in the Inbox (Email state 1 → 2).
	msg2 := insertInboxMessage(t, h.super, a, "run", 2)

	// queryChanges since the captured state: msg2 is `added`, nothing `removed`,
	// and old/new query states bracket the change.
	qc := call(t, h.api.emailQueryChanges, a.UserID, map[string]any{
		"filter":          map[string]any{"inMailbox": a.InboxID.String()},
		"sinceQueryState": "s:1",
	})
	if qc["oldQueryState"] != "s:1" || qc["newQueryState"] != "s:2" {
		t.Fatalf("states = old %v new %v, want s:1/s:2", qc["oldQueryState"], qc["newQueryState"])
	}
	if rm := qc["removed"].([]any); len(rm) != 0 {
		t.Errorf("removed = %v, want empty", rm)
	}
	added := qc["added"].([]any)
	if len(added) != 1 {
		t.Fatalf("added = %v, want 1 entry", added)
	}
	a0 := added[0].(map[string]any)
	if a0["id"] != msg2.String() {
		t.Errorf("added id = %v, want %s", a0["id"], msg2)
	}
	// msg2 is the newest message, so it sorts first (index 0) in received-desc order.
	if a0["index"].(float64) != 0 {
		t.Errorf("added index = %v, want 0", a0["index"])
	}

	// Trash the original message (Email state 2 → 3); it leaves the Inbox result.
	call(t, h.api.emailSet, a.UserID, map[string]any{"destroy": []string{a.MessageID.String()}})
	qc = call(t, h.api.emailQueryChanges, a.UserID, map[string]any{
		"filter":          map[string]any{"inMailbox": a.InboxID.String()},
		"sinceQueryState": "s:2",
		"calculateTotal":  true,
	})
	if qc["newQueryState"] != "s:3" {
		t.Errorf("newQueryState = %v, want s:3", qc["newQueryState"])
	}
	rm := qc["removed"].([]any)
	if len(rm) != 1 || rm[0] != a.MessageID.String() {
		t.Errorf("removed = %v, want [%s]", rm, a.MessageID)
	}
	if len(qc["added"].([]any)) != 0 {
		t.Errorf("added = %v, want empty", qc["added"])
	}
	// calculateTotal: only msg2 remains in the Inbox.
	if qc["total"].(float64) != 1 {
		t.Errorf("total = %v, want 1", qc["total"])
	}
}

// TestEmailQueryChangesVolatileFallback proves a volatile filter (text/search or
// inThread), and a garbled cursor, each yield cannotCalculateChanges — the honest
// fallback that forces a re-query, never a wrong delta.
func TestEmailQueryChangesVolatileFallback(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"text", map[string]any{
			"filter":          map[string]any{"text": "invoice"},
			"sinceQueryState": "s:1"}},
		{"inThread", map[string]any{
			"filter":          map[string]any{"inThread": a.ThreadID.String()},
			"sinceQueryState": "s:1"}},
		{"garbled-cursor", map[string]any{
			"filter":          map[string]any{"inMailbox": a.InboxID.String()},
			"sinceQueryState": "not-a-state"}},
		// collapseThreads (#210, RFC 8621 §4.4.4): even an otherwise
		// delta-stable filter (inMailbox) must fall back, because a new
		// message arriving in a thread swaps WHICH message represents it with
		// no added/removed pair a change-log delta can express.
		{"collapseThreads", map[string]any{
			"filter":          map[string]any{"inMailbox": a.InboxID.String()},
			"collapseThreads": true,
			"sinceQueryState": "s:1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, mErr := h.api.emailQueryChanges(context.Background(), a.UserID, mustJSON(t, c.args))
			if mErr == nil || mErr.Type != errCannotCalculateChanges {
				t.Errorf("%s: got %+v, want cannotCalculateChanges", c.name, mErr)
			}
		})
	}

	// A text filter is volatile whether or not a search backend is installed: the
	// fallback is decided before search is ever consulted, so Email/query also
	// reports canCalculateChanges:false for it.
	q := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter": map[string]any{"inThread": a.ThreadID.String()},
	})
	if q["canCalculateChanges"] != false {
		t.Errorf("inThread canCalculateChanges = %v, want false", q["canCalculateChanges"])
	}

	// Same for collapseThreads: an otherwise delta-stable inMailbox filter must
	// still report canCalculateChanges:false once collapseThreads is set.
	qCollapse := call(t, h.api.emailQuery, a.UserID, map[string]any{
		"filter":          map[string]any{"inMailbox": a.InboxID.String()},
		"collapseThreads": true,
	})
	if qCollapse["canCalculateChanges"] != false {
		t.Errorf("collapseThreads canCalculateChanges = %v, want false", qCollapse["canCalculateChanges"])
	}
}

func TestDispatchWithResultReference(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	req := Request{
		Using: []string{capCore, capMail},
		MethodCalls: []Invocation{
			{Name: "Email/query", Args: mustJSON(t, map[string]any{}), CallID: "q"},
			{Name: "Email/get", Args: mustJSON(t, map[string]any{
				"#ids": map[string]any{"resultOf": "q", "name": "Email/query", "path": "/ids"},
			}), CallID: "g"},
		},
	}
	resp := h.api.dispatch(context.Background(), a.UserID, req)
	if len(resp.MethodResponses) != 2 {
		t.Fatalf("got %d responses", len(resp.MethodResponses))
	}
	get := resp.MethodResponses[1]
	if get.Name != "Email/get" {
		t.Fatalf("second response = %q (%s)", get.Name, get.Args)
	}
	var gr getResult
	if err := json.Unmarshal(get.Args, &gr); err != nil {
		t.Fatal(err)
	}
	if len(gr.List) != 1 || gr.List[0]["id"] != a.MessageID.String() {
		t.Errorf("resolved Email/get returned %v", gr.List)
	}
}
