// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package mailsync's live end-to-end coverage for the expunge reaper (#86).
//
// This suite deliberately uses NO doubles. It runs the real Engine against a
// real Dovecot IMAP server and a real PostgreSQL with RLS enforced, through the
// real ingest path and a real (filesystem) blob store, and asserts on what a JMAP
// client would actually read (store.GetEmails / QueryEmails). The reaper is the
// one thing in this project that can DESTROY user data on the strength of an
// inference about a remote server, and an in-memory fake cannot tell you whether
// that inference survives contact with a real IMAP server — which UIDs Dovecot
// actually reports, what UID SEARCH ALL returns for an emptied folder, what
// UIDNEXT it hands back. So it is tested against the real thing.
//
// Run via scripts/mailsync-test.sh, which brings up both servers. Without them,
// every test here skips.
package mailsync

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// ---- harness ----------------------------------------------------------------

// The mailboxes scripts/mailsync-test.sh creates on the Dovecot fixture. The
// suite runs serially (-p 1) against ONE IMAP account and empties both folders
// before each test, while every test gets a FRESH Postgres user — so IMAP state
// is reset per test and store state is isolated per test, without needing to
// create folders (which Mailroom itself never does, and which SyncSession
// therefore deliberately cannot).
const (
	liveInbox   = "INBOX"
	liveArchive = "Archive"
)

type liveFixture struct {
	super  *pgxpool.Pool
	st     *store.Store
	blobs  blob.BlobStore
	engine *Engine
	cfg    sync.Config
	sess   sync.SyncSession // an independent session: the OTHER IMAP client

	userID    ulid.ULID
	accountID ulid.ULID
	dek       []byte
	dekID     ulid.ULID

	inbox   string
	archive string
}

func envOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; run via scripts/mailsync-test.sh", name)
	}
	return v
}

func newLiveFixture(t *testing.T, opts ...Option) *liveFixture {
	t.Helper()
	ctx := context.Background()

	superURL := envOrSkip(t, "MAILROOM_TEST_SUPER_URL")
	appURL := envOrSkip(t, "MAILROOM_TEST_APP_URL")
	host := envOrSkip(t, "MAILROOM_TEST_IMAP_HOST")
	portStr := envOrSkip(t, "MAILROOM_TEST_IMAP_PORT")
	user := envOrSkip(t, "MAILROOM_TEST_IMAP_USER")
	pass := envOrSkip(t, "MAILROOM_TEST_IMAP_PASS")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad MAILROOM_TEST_IMAP_PORT %q: %v", portStr, err)
	}

	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	f := &liveFixture{
		super: super, st: st, blobs: mustBlobs(t),
		cfg: sync.Config{
			Host: host, Port: port, Username: user, Password: pass, TLS: sync.TLSInsecure,
			// Dovecot is on a private Docker network, which the dial's egress
			// guard refuses by default (#625 §3); this is the harness's
			// MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS.
			AllowPrivateHosts: true,
		},
		userID: ulid.Make(), accountID: ulid.Make(),
		dek: randomKey(t), dekID: ulid.Make(),
		inbox: liveInbox, archive: liveArchive,
	}
	f.engine = New(NewStoreAdapter(st), ingest.New(st, f.blobs), nil, opts...)

	// A dedicated session, separate from the engine's, standing in for the OTHER
	// IMAP client (the user's phone) that expunges and moves things.
	sess, err := sync.Dial(ctx, f.cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	f.sess = sess

	f.seed(t)
	f.emptyMailboxes(t)
	return f
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := crand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func (f *liveFixture) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		f.accountID.String(), f.userID.String(), ulid.Make().String())
}

// emptyMailboxes leaves both test folders empty on the SERVER, so each test
// starts from a known state. UIDs keep climbing across tests — which is exactly
// what a real mailbox does, and a useful thing for the reaper to survive.
func (f *liveFixture) emptyMailboxes(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, mb := range []string{f.inbox, f.archive} {
		_, uids, err := f.sess.LiveUIDs(ctx, mb)
		if err != nil {
			t.Fatalf("empty %q: %v (is the mailbox missing? run via scripts/mailsync-test.sh)", mb, err)
		}
		for _, uid := range uids {
			f.expungeOnServer(t, mb, uid)
		}
	}
}

// appendMsg puts a message on the SERVER and returns its UID.
func (f *liveFixture) appendMsg(t *testing.T, mailbox, subject string) sync.UID {
	t.Helper()
	body := []byte("From: sender@example.com\r\n" +
		"To: " + f.userID.String() + "@example.com\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + subject + "@example.com>\r\n" +
		"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n" +
		"\r\n" +
		"body of " + subject + "\r\n")
	uid, err := f.sess.Append(context.Background(), mailbox, nil, time.Time{}, body)
	if err != nil {
		t.Fatalf("append %q to %q: %v", subject, mailbox, err)
	}
	if uid == 0 {
		t.Fatalf("append %q: server returned no UID (UIDPLUS expected from the harness)", subject)
	}
	return uid
}

// expungeOnServer deletes a message on the server, as another IMAP client would.
func (f *liveFixture) expungeOnServer(t *testing.T, mailbox string, uid sync.UID) {
	t.Helper()
	ctx := context.Background()
	if err := f.sess.StoreFlags(ctx, mailbox, uid, []sync.Flag{sync.FlagDeleted}, nil); err != nil {
		t.Fatalf("store \\Deleted: %v", err)
	}
	removed, err := f.sess.Expunge(ctx, mailbox, sync.UIDSetNum(uid))
	if err != nil {
		t.Fatalf("expunge: %v", err)
	}
	// ⚠ THIS IS THE LIVE NEGATIVE CONTROL FOR SyncSession.Expunge'S COUNT (#296).
	//
	// The count exists so writeback.Result.Expunged can tell "I destroyed the
	// message" apart from "there was nothing there" — a UID EXPUNGE of a UID the
	// server no longer holds succeeds. It is read out of the untagged EXPUNGE
	// responses go-imap hands to the pending ExpungeCommand, and no unit test can
	// prove a real server sends them where the library looks: a fake returns
	// whatever it was told to. Here a message demonstrably existed (the fixture
	// just appended it) and is demonstrably gone (the assertions downstream of
	// this helper depend on it), so 1 is the only honest answer. If go-imap ever
	// routes those responses to the unilateral handler instead, this goes red
	// against a real Dovecot rather than silently reporting 0 destructions in
	// production.
	if removed != 1 {
		t.Fatalf("expunge removed = %d, want 1 — the server must report the message it just destroyed", removed)
	}
}

func (f *liveFixture) syncOnce(t *testing.T) Summary {
	t.Helper()
	sum, err := f.engine.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	return sum
}

// mailboxID reads the store's id for an IMAP mailbox name.
func (f *liveFixture) mailboxID(t *testing.T, imapName string) ulid.ULID {
	t.Helper()
	var id string
	if err := f.super.QueryRow(context.Background(),
		`SELECT id FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), imapName).Scan(&id); err != nil {
		t.Fatalf("mailbox %q: %v", imapName, err)
	}
	return ulid.MustParse(id)
}

// messageIDBySubject finds a canonical message by subject.
func (f *liveFixture) messageIDBySubject(t *testing.T, subject string) (ulid.ULID, bool) {
	t.Helper()
	var id string
	err := f.super.QueryRow(context.Background(),
		`SELECT id FROM app.messages WHERE user_id = $1 AND subject = $2`,
		f.userID.String(), subject).Scan(&id)
	if err != nil {
		return ulid.ULID{}, false
	}
	return ulid.MustParse(id), true
}

// mailboxIDs is what a JMAP client would read for this message: the real
// GetEmails derivation, not a hand-rolled provenance query.
func (f *liveFixture) mailboxIDs(t *testing.T, msg ulid.ULID) []string {
	t.Helper()
	var out []string
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		views, err := tx.GetEmails(context.Background(), []ulid.ULID{msg})
		if err != nil {
			return err
		}
		for _, v := range views {
			for _, mb := range v.MailboxIDs {
				out = append(out, mb.String())
			}
		}
		sort.Strings(out)
		return nil
	}); err != nil {
		t.Fatalf("get email: %v", err)
	}
	return out
}

// inMailbox is Email/query inMailbox — the other place membership is derived, and
// the one that must never disagree with GetEmails.
func (f *liveFixture) inMailbox(t *testing.T, mailbox ulid.ULID) []string {
	t.Helper()
	var out []string
	if err := f.st.UserTx(context.Background(), f.userID, func(tx *store.UserTx) error {
		ids, _, err := tx.QueryEmails(context.Background(), store.EmailQuery{InMailbox: &mailbox, Limit: 100})
		if err != nil {
			return err
		}
		for _, id := range ids {
			out = append(out, id.String())
		}
		sort.Strings(out)
		return nil
	}); err != nil {
		t.Fatalf("query emails: %v", err)
	}
	return out
}

func (f *liveFixture) trashed(t *testing.T, msg ulid.ULID) bool {
	t.Helper()
	var trashed bool
	if err := f.super.QueryRow(context.Background(),
		`SELECT trashed_at IS NOT NULL FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&trashed); err != nil {
		t.Fatalf("trashed: %v", err)
	}
	return trashed
}

func (f *liveFixture) liveProvenanceCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.message_provenance WHERE user_id = $1 AND state = 'live'`,
		f.userID.String()).Scan(&n); err != nil {
		t.Fatalf("provenance count: %v", err)
	}
	return n
}

// ---- tests ------------------------------------------------------------------

// THE BUG THIS ISSUE EXISTS FOR. A message deleted in another IMAP client must
// actually leave its Mailroom folder — from mailboxIds, from inMailbox queries,
// and from the change stream that drives the search index.
func TestLiveExpungedMessageLeavesTheMailroomFolder(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond)) // always due
	f.appendMsg(t, f.inbox, "keep-me")
	killUID := f.appendMsg(t, f.inbox, "delete-me-on-my-phone")

	f.syncOnce(t)
	inboxID := f.mailboxID(t, f.inbox)
	keep, ok := f.messageIDBySubject(t, "keep-me")
	if !ok {
		t.Fatal("keep-me was never ingested")
	}
	kill, ok := f.messageIDBySubject(t, "delete-me-on-my-phone")
	if !ok {
		t.Fatal("delete-me-on-my-phone was never ingested")
	}
	if got := f.mailboxIDs(t, kill); len(got) != 1 || got[0] != inboxID.String() {
		t.Fatalf("precondition: message is not in the Mailroom inbox: %v", got)
	}

	// The user deletes it on their phone.
	f.expungeOnServer(t, f.inbox, killUID)

	sum := f.syncOnce(t)
	if sum.MailboxesReaped == 0 {
		t.Fatalf("nothing was reaped: %+v (errors: %v)", sum, sum.Errors)
	}
	if sum.ProvenanceReaped != 1 {
		t.Errorf("ProvenanceReaped = %d, want 1", sum.ProvenanceReaped)
	}
	if sum.MessagesVanished != 1 {
		t.Errorf("MessagesVanished = %d, want 1", sum.MessagesVanished)
	}

	if got := f.mailboxIDs(t, kill); len(got) != 0 {
		t.Errorf("the expunged message is STILL in %v — the whole point of #86", got)
	}
	if !f.trashed(t, kill) {
		t.Error("the expunged message was not soft-deleted (ADR-009)")
	}
	// The one that is still on the server is untouched.
	if got := f.mailboxIDs(t, keep); len(got) != 1 || got[0] != inboxID.String() {
		t.Errorf("a message the server still has was reaped: %v", got)
	}
	if f.trashed(t, keep) {
		t.Error("soft-deleted a message that is sitting in the server's INBOX")
	}
	// Email/query agrees with Email/get. If these two ever disagree, the client's
	// message list and its folder counts disagree.
	got := f.inMailbox(t, inboxID)
	if len(got) != 1 || got[0] != keep.String() {
		t.Errorf("inMailbox = %v, want only the surviving message", got)
	}
	// And the change_log carried it — this is what the search indexer consumes.
	var changes int
	if err := f.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2 AND op = 'updated'`,
		f.userID.String(), kill.String()).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes == 0 {
		t.Error("no Email change_log row for the membership change — /changes and the index will never learn")
	}
}

// A message MOVED on the server (INBOX -> Archive by another client) ends up in
// Archive and ONLY Archive, and is emphatically not deleted.
func TestLiveMovedMessageFollowsTheServer(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	uid := f.appendMsg(t, f.inbox, "archive-me-on-my-phone")
	f.syncOnce(t)

	inboxID, archiveID := f.mailboxID(t, f.inbox), f.mailboxID(t, f.archive)
	msg, ok := f.messageIDBySubject(t, "archive-me-on-my-phone")
	if !ok {
		t.Fatal("message was never ingested")
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != inboxID.String() {
		t.Fatalf("precondition: %v", got)
	}

	// The phone moves it. UID MOVE: gone from INBOX, new UID in Archive.
	if err := f.sess.Move(context.Background(), f.inbox, sync.UIDSetNum(uid), f.archive); err != nil {
		t.Fatalf("move: %v", err)
	}

	sum := f.syncOnce(t)
	if len(sum.Errors) != 0 {
		t.Fatalf("sync errors: %v", sum.Errors)
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != archiveID.String() {
		t.Errorf("mailboxIds = %v, want exactly {Archive}", got)
	}
	if f.trashed(t, msg) {
		t.Fatal("SOFT-DELETED A MESSAGE THAT WAS MERELY MOVED — the poll-then-reap ordering is broken")
	}
	if got := f.inMailbox(t, inboxID); len(got) != 0 {
		t.Errorf("still in the inbox: %v", got)
	}
	if got := f.inMailbox(t, archiveID); len(got) != 1 || got[0] != msg.String() {
		t.Errorf("not in Archive: %v", got)
	}
}

// A FAILED SYNC REAPS NOTHING. The IMAP server is real and healthy; the sync
// itself fails (bad credentials). Not one provenance row may move.
func TestLiveFailedSyncReapsNothing(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	uid := f.appendMsg(t, f.inbox, "must-survive-a-failed-sync")
	f.syncOnce(t)

	msg, ok := f.messageIDBySubject(t, "must-survive-a-failed-sync")
	if !ok {
		t.Fatal("message was never ingested")
	}
	inboxID := f.mailboxID(t, f.inbox)
	before := f.liveProvenanceCount(t)

	// Now delete it on the server — so a reaper that DID run would remove it —
	// and then break the account's credentials so the sync cannot happen at all.
	f.expungeOnServer(t, f.inbox, uid)

	broken := f.cfg
	broken.Password = "definitely-not-the-password"
	if _, err := f.engine.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: broken, DEK: f.dek, DEKID: f.dekID,
	}); err == nil {
		t.Fatal("a sync with bad credentials succeeded; the test proves nothing")
	}

	if got := f.liveProvenanceCount(t); got != before {
		t.Fatalf("live provenance went from %d to %d ACROSS A FAILED SYNC — a sync failure must never be read as an expunge", before, got)
	}
	if f.trashed(t, msg) {
		t.Fatal("SOFT-DELETED MAIL BECAUSE THE SYNC FAILED")
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != inboxID.String() {
		t.Fatalf("membership changed across a failed sync: %v", got)
	}

	// And when the sync DOES work, the reap happens — proving the test above was
	// blocked by the failure and not by the message still being there.
	f.syncOnce(t)
	if got := f.mailboxIDs(t, msg); len(got) != 0 {
		t.Errorf("after a successful sync the expunged message is still in %v", got)
	}
}

// A mailbox whose LIVE-UID READ fails reaps nothing, while its siblings still
// reap. The sync itself is entirely real and successful — only the reaper's
// question to the server fails, which is the failure mode a flaky network
// actually produces.
func TestLiveFailedLiveUIDsReadReapsNothing(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	inboxUID := f.appendMsg(t, f.inbox, "inbox-survivor")
	archiveUID := f.appendMsg(t, f.archive, "archive-casualty")
	f.syncOnce(t)

	inboxMsg, ok := f.messageIDBySubject(t, "inbox-survivor")
	if !ok {
		t.Fatal("inbox message was never ingested")
	}
	archiveMsg, ok := f.messageIDBySubject(t, "archive-casualty")
	if !ok {
		t.Fatal("archive message was never ingested")
	}

	// Both are deleted on the server, so a working reaper would remove BOTH.
	f.expungeOnServer(t, f.inbox, inboxUID)
	f.expungeOnServer(t, f.archive, archiveUID)

	// Break only the LiveUIDs call, and only for the inbox: the poll, the fetch,
	// the ingest and the Archive reap all run against the real Dovecot.
	broken := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs), func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
		s, err := sync.Dial(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return &brokenLiveUIDs{SyncSession: s, mailbox: f.inbox}, nil
	}, WithReapInterval(time.Nanosecond))

	sum, err := broken.SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	if len(sum.Errors) == 0 {
		t.Fatal("the broken LiveUIDs read was not surfaced as an error")
	}

	// The mailbox we could not ask about is untouched...
	if got := f.mailboxIDs(t, inboxMsg); len(got) != 1 {
		t.Fatalf("reaped a mailbox whose live-UID read FAILED: membership now %v", got)
	}
	if f.trashed(t, inboxMsg) {
		t.Fatal("SOFT-DELETED MAIL BECAUSE WE COULD NOT ASK THE SERVER ABOUT IT")
	}
	// ...and the one we could ask about was reaped correctly.
	if got := f.mailboxIDs(t, archiveMsg); len(got) != 0 {
		t.Errorf("the healthy mailbox did not reap: %v", got)
	}
}

func mustBlobs(t *testing.T) blob.BlobStore {
	t.Helper()
	b, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return b
}

// brokenLiveUIDs is a real IMAP session with exactly one fault injected: the
// reaper's question about one mailbox fails. Everything else — LIST, SELECT,
// FETCH — is the genuine server.
type brokenLiveUIDs struct {
	sync.SyncSession
	mailbox string
}

func (b *brokenLiveUIDs) LiveUIDs(ctx context.Context, mailbox string) (*sync.MailboxStatus, []sync.UID, error) {
	if mailbox == b.mailbox {
		return nil, nil, fmt.Errorf("injected fault: cannot read the live UID set of %q", mailbox)
	}
	return b.SyncSession.LiveUIDs(ctx, mailbox)
}

// THE #84 -> #86 HANDOFF, end to end. The user archives a message IN MAILROOM;
// writeback is not wired into this test, so the move is simulated on the server
// exactly as the writeback engine would perform it (UID MOVE). The canonical
// placement holds membership steady throughout, and once the reaper has removed
// the stale source row the placement retires itself — with the message never once
// appearing back in the inbox.
func TestLiveCanonicalPlacementRetiresAfterTheReap(t *testing.T) {
	f := newLiveFixture(t, WithReapInterval(time.Nanosecond))
	uid := f.appendMsg(t, f.inbox, "archive-me-in-mailroom")
	f.syncOnce(t)

	ctx := context.Background()
	inboxID, archiveID := f.mailboxID(t, f.inbox), f.mailboxID(t, f.archive)
	msg, ok := f.messageIDBySubject(t, "archive-me-in-mailroom")
	if !ok {
		t.Fatal("message was never ingested")
	}

	// The user hits Archive. Email/set sets moved_mailbox_id and enqueues the
	// writeback intent; provenance still says INBOX.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, msg, store.EmailUpdate{MoveToRole: "archive", MoveToMailboxID: &archiveID})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("message not found for Email/set")
		}
		return nil
	}); err != nil {
		t.Fatalf("Email/set archive: %v", err)
	}
	if got := f.mailboxIDs(t, msg); len(got) != 1 || got[0] != archiveID.String() {
		t.Fatalf("right after the archive, mailboxIds = %v, want {Archive}", got)
	}

	// Writeback performs the move on the server.
	if err := f.sess.Move(ctx, f.inbox, sync.UIDSetNum(uid), f.archive); err != nil {
		t.Fatalf("writeback move: %v", err)
	}

	// Sync + reap. THE ASSERTION THAT MATTERS: at no point does the message read
	// as being in the inbox again.
	for pass := 0; pass < 2; pass++ {
		f.syncOnce(t)
		got := f.mailboxIDs(t, msg)
		if len(got) != 1 || got[0] != archiveID.String() {
			t.Fatalf("pass %d: mailboxIds = %v, want a stable {Archive} — the message FLICKERED", pass, got)
		}
		if in := f.inMailbox(t, inboxID); len(in) != 0 {
			t.Fatalf("pass %d: the archived message came back to the inbox: %v", pass, in)
		}
		if f.trashed(t, msg) {
			t.Fatalf("pass %d: the archived message was soft-deleted", pass)
		}
	}

	// And the override has now retired itself: membership is derived from
	// provenance alone, so a LATER move by another client can be followed.
	var moved string
	if err := f.super.QueryRow(ctx,
		`SELECT COALESCE(moved_mailbox_id, '') FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != "" {
		t.Errorf("moved_mailbox_id = %s: the override never retired, so the message is pinned to Archive forever", moved)
	}
}
