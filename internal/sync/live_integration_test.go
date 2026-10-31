// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package sync_test exercises SyncSession/Dial against a real Dovecot IMAP
// server (issue #4, spec §4). It is a separate (external) test package so
// these tests can only reach mailroom/internal/sync through its exported
// API — the same surface every real caller (the sync engine, #10) is
// limited to.
//
// Run via scripts/sync-test.sh, which brings up the Dovecot fixture and
// seeds the mailboxes/messages these tests assert against. Run directly
// (`go test -tags integration ./internal/sync/...`) without that harness,
// every test here skips: the MAILROOM_TEST_IMAP_* env vars are absent.
package sync_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	mrsync "mailroom/internal/sync"
)

// Fixture data seeded by scripts/sync-test.sh — see
// internal/sync/testdata/*.eml. INBOX gets two messages (UID 1, UID 2);
// Sent gets one. Drafts/Trash/Junk/Archive are seeded empty, existing only
// to exercise SPECIAL-USE.
const (
	mailboxInbox   = "INBOX"
	mailboxSent    = "Sent"
	mailboxDrafts  = "Drafts"
	mailboxTrash   = "Trash"
	mailboxJunk    = "Junk"
	mailboxArchive = "Archive"

	inboxUID1Subject = "Hello from Alice"
	inboxUID2Subject = "Second message"
)

// testConfig reads the harness's connection details from the environment
// and skips the test when they are absent, so `go test -tags integration`
// without scripts/sync-test.sh's Dovecot fixture running does not fail.
func testConfig(t *testing.T) mrsync.Config {
	t.Helper()

	host := os.Getenv("MAILROOM_TEST_IMAP_HOST")
	portStr := os.Getenv("MAILROOM_TEST_IMAP_PORT")
	user := os.Getenv("MAILROOM_TEST_IMAP_USER")
	pass := os.Getenv("MAILROOM_TEST_IMAP_PASS")
	if host == "" || portStr == "" || user == "" || pass == "" {
		t.Skip("MAILROOM_TEST_IMAP_HOST/PORT/USER/PASS not set; run via scripts/sync-test.sh")
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("invalid MAILROOM_TEST_IMAP_PORT %q: %v", portStr, err)
	}

	return mrsync.Config{
		Host:     host,
		Port:     port,
		Username: user,
		Password: pass,
		// The harness (scripts/sync-test.sh) runs plaintext IMAP on port
		// 143 with disable_plaintext_auth=no; TLSInsecure is the mode
		// config.go documents as existing specifically for this harness.
		TLS:         mrsync.TLSInsecure,
		DialTimeout: 15 * time.Second,
		// The harness's Dovecot is a container on a private Docker network, which
		// the dial's egress guard refuses by default (#625 §3) — exactly as a
		// deployment whose mail server is on its LAN must set
		// MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS. Without this every live test would
		// fail at Dial with the refusal, which is the guard working.
		AllowPrivateHosts: true,
	}
}

// dial connects, and registers cleanup to Logout+Close.
func dial(t *testing.T, cfg mrsync.Config) mrsync.SyncSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess, err := mrsync.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() {
		logoutCtx, logoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer logoutCancel()
		_ = sess.Logout(logoutCtx)
		_ = sess.Close()
	})
	return sess
}

func TestLiveDialAndAuth(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)
	if sess == nil {
		t.Fatal("Dial() returned nil session with nil error")
	}
}

func TestLiveDialWrongPassword(t *testing.T) {
	cfg := testConfig(t)
	cfg.Password = cfg.Password + "-wrong-" + strconv.Itoa(int(time.Now().UnixNano()))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess, err := mrsync.Dial(ctx, cfg)
	if err == nil {
		_ = sess.Close()
		t.Fatal("Dial() with wrong password: error = nil, want auth error")
	}

	var syncErr *mrsync.Error
	if !errors.As(err, &syncErr) {
		t.Fatalf("Dial() error = %v (%T), want *mrsync.Error", err, err)
	}
	if syncErr.Kind != mrsync.ErrorKindAuth {
		t.Errorf("Dial() error Kind = %v, want %v (error: %v)", syncErr.Kind, mrsync.ErrorKindAuth, err)
	}
}

// TestLiveNoopProbe exercises the connection health probe the sync pool relies
// on (mailroom#103 Stage 3): a NOOP on a live, authenticated connection returns
// nil, and a NOOP on a connection that has been Closed returns a (connection-
// fatal) error — which is exactly the signal that tells the pool to discard the
// session and re-dial rather than run a pass over a dead socket.
func TestLiveNoopProbe(t *testing.T) {
	cfg := testConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess, err := mrsync.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	// A healthy connection: the probe passes, repeatedly, so a warm session can
	// be reused across passes.
	for i := 0; i < 3; i++ {
		if err := sess.Noop(ctx); err != nil {
			t.Fatalf("Noop() #%d on a live connection = %v, want nil", i, err)
		}
	}

	// A dead connection: after Close the probe must fail, so the pool discards it.
	if err := sess.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := sess.Noop(ctx); err == nil {
		t.Fatal("Noop() on a closed connection = nil, want a connection error the pool can act on")
	}
}

func TestLiveListMailboxesSpecialUse(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mailboxes, err := sess.ListMailboxes(ctx)
	if err != nil {
		t.Fatalf("ListMailboxes() error = %v", err)
	}

	byName := make(map[string]mrsync.Mailbox, len(mailboxes))
	for _, mb := range mailboxes {
		byName[mb.Name] = mb
	}

	wantSpecialUse := map[string]mrsync.MailboxAttr{
		mailboxSent:    mrsync.MailboxAttrSent,
		mailboxDrafts:  mrsync.MailboxAttrDrafts,
		mailboxTrash:   mrsync.MailboxAttrTrash,
		mailboxJunk:    mrsync.MailboxAttrJunk,
		mailboxArchive: mrsync.MailboxAttrArchive,
	}
	for name, wantAttr := range wantSpecialUse {
		mb, ok := byName[name]
		if !ok {
			t.Errorf("ListMailboxes(): mailbox %q not returned; got %v", name, mailboxNames(mailboxes))
			continue
		}
		gotAttr, ok := mb.SpecialUse()
		if !ok {
			t.Errorf("mailbox %q: SpecialUse() = (_, false), want (%v, true); attrs = %v", name, wantAttr, mb.Attrs)
			continue
		}
		if gotAttr != wantAttr {
			t.Errorf("mailbox %q: SpecialUse() = %v, want %v", name, gotAttr, wantAttr)
		}
	}

	if _, ok := byName[mailboxInbox]; !ok {
		t.Errorf("ListMailboxes(): INBOX not returned; got %v", mailboxNames(mailboxes))
	}
}

func mailboxNames(mailboxes []mrsync.Mailbox) []string {
	names := make([]string, len(mailboxes))
	for i, mb := range mailboxes {
		names[i] = mb.Name
	}
	return names
}

func TestLiveSelectStatus(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !sess.Supports(mrsync.CapabilityCondStore) {
		t.Fatal("server does not advertise CONDSTORE; harness assumption violated")
	}

	status, err := sess.Select(ctx, mailboxInbox, mrsync.SelectOptions{CondStore: true})
	if err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxInbox, err)
	}

	if status.Name != mailboxInbox {
		t.Errorf("status.Name = %q, want %q", status.Name, mailboxInbox)
	}
	if status.UIDValidity == 0 {
		t.Error("status.UIDValidity = 0, want nonzero")
	}
	if status.NumMessages != 2 {
		t.Errorf("status.NumMessages = %d, want 2 (seeded inbox-1.eml, inbox-2.eml)", status.NumMessages)
	}
	if status.UIDNext < 3 {
		t.Errorf("status.UIDNext = %d, want >= 3 (2 messages already assigned UIDs)", status.UIDNext)
	}
	if status.HighestModSeq == 0 {
		t.Error("status.HighestModSeq = 0 with CondStore requested, want nonzero")
	}
}

func TestLiveFetchByUID(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := sess.Select(ctx, mailboxInbox, mrsync.SelectOptions{}); err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxInbox, err)
	}

	messages, err := collectByUID(ctx, sess, mrsync.UIDSetNum(1, 2), mrsync.FetchItems{
		Flags:    true,
		Envelope: true,
	})
	if err != nil {
		t.Fatalf("FetchByUID() error = %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("FetchByUID() returned %d messages, want 2", len(messages))
	}

	bySubject := make(map[mrsync.UID]string, len(messages))
	for _, msg := range messages {
		if msg.Envelope == nil {
			t.Fatalf("message UID %d: Envelope = nil, want populated (Envelope: true was requested)", msg.UID)
		}
		bySubject[msg.UID] = msg.Envelope.Subject
	}

	if got, want := bySubject[1], inboxUID1Subject; got != want {
		t.Errorf("UID 1 subject = %q, want %q", got, want)
	}
	if got, want := bySubject[2], inboxUID2Subject; got != want {
		t.Errorf("UID 2 subject = %q, want %q", got, want)
	}
}

func TestLiveCapabilities(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	if !sess.Supports(mrsync.CapabilityCondStore) {
		t.Error("Supports(CapabilityCondStore) = false, want true (Dovecot advertises CONDSTORE by default)")
	}
	if !sess.Supports(mrsync.CapabilitySpecialUse) {
		t.Error("Supports(CapabilitySpecialUse) = false, want true (Dovecot advertises SPECIAL-USE by default)")
	}
}

// TestLiveConcurrentSessions is the concurrency property test the pooled
// sync engine (#10) depends on: N independent SyncSessions, each doing its
// own Select+FetchByUID against the same account concurrently, must not
// race (enforced by `go test -race`, which scripts/sync-test.sh always
// passes) and must all observe the same, consistent mailbox contents.
func TestLiveConcurrentSessions(t *testing.T) {
	cfg := testConfig(t)

	const n = 8

	type result struct {
		idx      int
		subjects []string
		err      error
	}
	results := make(chan result, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			sess, err := mrsync.Dial(ctx, cfg)
			if err != nil {
				results <- result{idx: idx, err: fmt.Errorf("dial: %w", err)}
				return
			}
			defer func() { _ = sess.Close() }()

			if _, err := sess.Select(ctx, mailboxInbox, mrsync.SelectOptions{}); err != nil {
				results <- result{idx: idx, err: fmt.Errorf("select: %w", err)}
				return
			}

			messages, err := collectByUID(ctx, sess, mrsync.UIDSetNum(1, 2), mrsync.FetchItems{Envelope: true})
			if err != nil {
				results <- result{idx: idx, err: fmt.Errorf("fetch: %w", err)}
				return
			}

			subjects := make([]string, 0, len(messages))
			for _, msg := range messages {
				if msg.Envelope == nil {
					results <- result{idx: idx, err: fmt.Errorf("UID %d: nil envelope", msg.UID)}
					return
				}
				subjects = append(subjects, msg.Envelope.Subject)
			}
			sort.Strings(subjects)
			results <- result{idx: idx, subjects: subjects}
		}(i)
	}

	wg.Wait()
	close(results)

	var want []string
	failures := 0
	for res := range results {
		if res.err != nil {
			t.Errorf("session %d: %v", res.idx, res.err)
			failures++
			continue
		}
		if want == nil {
			want = res.subjects
			continue
		}
		if !reflect.DeepEqual(res.subjects, want) {
			t.Errorf("session %d: subjects = %v, want %v (inconsistent view across concurrent sessions)", res.idx, res.subjects, want)
		}
	}

	if failures == n {
		t.Fatal("all concurrent sessions failed")
	}
	wantSubjects := []string{inboxUID1Subject, inboxUID2Subject}
	sort.Strings(wantSubjects)
	if want != nil && !reflect.DeepEqual(want, wantSubjects) {
		t.Errorf("concurrent sessions observed subjects = %v, want %v", want, wantSubjects)
	}
}

// --- Write ops (issue #11 follow-up) -------------------------------------
//
// These tests never mutate the shared INBOX/Sent fixture messages the
// read-only tests above assert exact counts/subjects against. Each test
// appends its own scratch message(s) into one of the empty special-use
// mailboxes (Drafts/Trash/Junk/Archive) and only makes UID-specific
// assertions (never a mailbox-wide NumMessages check) against any mailbox
// more than one test writes to, so test order and repetition never matter.

// appendTestMessage builds a minimal RFC 5322 message with a unique
// Message-ID, appends it to mailbox with flags, and returns its assigned
// UID. It fails the test if the server doesn't return one (this harness
// asserts Dovecot's default UIDPLUS support before calling this helper).
func appendTestMessage(t *testing.T, ctx context.Context, sess mrsync.SyncSession, mailbox, subject string, flags []mrsync.Flag) mrsync.UID {
	t.Helper()

	msgID := fmt.Sprintf("<%d@example.com>", time.Now().UnixNano())
	body := fmt.Sprintf(
		"From: Sync Tester <sync-tester@example.com>\r\n"+
			"To: Sync Tester <sync-tester@example.com>\r\n"+
			"Subject: %s\r\n"+
			"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n"+
			"Message-ID: %s\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"Body for %s.\r\n",
		subject, msgID, subject)

	uid, err := sess.Append(ctx, mailbox, flags, time.Time{}, []byte(body))
	if err != nil {
		t.Fatalf("Append(%q) error = %v", mailbox, err)
	}
	if uid == 0 {
		t.Fatal("Append() returned UID 0; harness assumes UIDPLUS support")
	}
	return uid
}

func hasFlag(flags []mrsync.Flag, want mrsync.Flag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func TestLiveAppendMessage(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !sess.Supports(mrsync.CapabilityUIDPlus) {
		t.Fatal("server does not advertise UIDPLUS; harness assumption violated")
	}

	const subject = "Append test message"
	uid := appendTestMessage(t, ctx, sess, mailboxDrafts, subject, []mrsync.Flag{mrsync.FlagSeen})

	// APPEND doesn't require (or change) the selected mailbox; FetchByUID
	// does, so select Drafts explicitly before fetching.
	if _, err := sess.Select(ctx, mailboxDrafts, mrsync.SelectOptions{}); err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxDrafts, err)
	}

	messages, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Envelope: true, Flags: true})
	if err != nil {
		t.Fatalf("FetchByUID() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("FetchByUID() returned %d messages, want 1", len(messages))
	}
	if messages[0].Envelope == nil || messages[0].Envelope.Subject != subject {
		t.Errorf("appended message Envelope = %+v, want Subject %q", messages[0].Envelope, subject)
	}
	if !hasFlag(messages[0].Flags, mrsync.FlagSeen) {
		t.Errorf("appended message Flags = %v, want to contain \\Seen", messages[0].Flags)
	}
}

// TestLiveFetchBodyDoesNotMarkSeen pins the property that made every synced
// message show up read (#129): a body fetch must use BODY.PEEK[], because the
// plain BODY[] form implicitly sets \Seen on the server (RFC 9051 §6.4.5).
//
// This is a live test on purpose. The bug was a single zero-valued struct
// field, invisible to any test that stubs the IMAP client — only a real server
// applying real FETCH semantics can tell BODY[] and BODY.PEEK[] apart. The
// flag re-reads ask for Flags ONLY, so they cannot themselves disturb what
// they read.
//
// It lives down here with the other write ops, not up with the read-only fetch
// tests, because it APPENDS. Sitting above this banner it was the first test in
// the run to write, so it paid Dovecot's cold first-write-to-Drafts cost and
// timed out on a loaded CI runner. The timeout below is correspondingly
// generous: this test asserts a protocol property, and should never be the
// thing that reports how busy the runner is.
func TestLiveFetchBodyDoesNotMarkSeen(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	uid := appendTestMessage(t, ctx, sess, mailboxDrafts, "Peek fetch must not mark seen", nil)

	if _, err := sess.Select(ctx, mailboxDrafts, mrsync.SelectOptions{}); err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxDrafts, err)
	}

	// Guard the premise: the message starts unseen, so a \Seen afterwards can
	// only have come from the body fetch under test.
	before, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Flags: true})
	if err != nil {
		t.Fatalf("FetchByUID(flags, before) error = %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("FetchByUID(flags, before) returned %d messages, want 1", len(before))
	}
	if hasFlag(before[0].Flags, mrsync.FlagSeen) {
		t.Fatalf("appended message Flags = %v, want NO \\Seen (appended with no flags)", before[0].Flags)
	}

	// The fetch the sync engine actually issues for new mail.
	fetched, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{
		Flags: true, Envelope: true, InternalDate: true, BodyFull: true,
	})
	if err != nil {
		t.Fatalf("FetchByUID(body) error = %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("FetchByUID(body) returned %d messages, want 1", len(fetched))
	}
	// Assert the body really came back: a peeked fetch that silently returned
	// nothing would pass the \Seen check for the wrong reason.
	if len(fetched[0].Body) == 0 {
		t.Fatal("FetchByUID(BodyFull) returned an empty Body, want the full message")
	}

	after, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Flags: true})
	if err != nil {
		t.Fatalf("FetchByUID(flags, after) error = %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("FetchByUID(flags, after) returned %d messages, want 1", len(after))
	}
	if hasFlag(after[0].Flags, mrsync.FlagSeen) {
		t.Errorf("after a body fetch, Flags = %v, want NO \\Seen: the fetch used BODY[] instead of BODY.PEEK[] and marked the message read on the server (#129)", after[0].Flags)
	}
}

func TestLiveStoreFlags(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	uid := appendTestMessage(t, ctx, sess, mailboxDrafts, "Store flags test message", nil)

	if err := sess.StoreFlags(ctx, mailboxDrafts, uid, []mrsync.Flag{mrsync.FlagFlagged}, nil); err != nil {
		t.Fatalf("StoreFlags(add \\Flagged) error = %v", err)
	}
	messages, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Flags: true})
	if err != nil {
		t.Fatalf("FetchByUID() error = %v", err)
	}
	if len(messages) != 1 || !hasFlag(messages[0].Flags, mrsync.FlagFlagged) {
		t.Fatalf("after StoreFlags(add \\Flagged): messages = %+v, want 1 message with \\Flagged", messages)
	}

	if err := sess.StoreFlags(ctx, mailboxDrafts, uid, nil, []mrsync.Flag{mrsync.FlagFlagged}); err != nil {
		t.Fatalf("StoreFlags(remove \\Flagged) error = %v", err)
	}
	messages, err = collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Flags: true})
	if err != nil {
		t.Fatalf("FetchByUID() error = %v", err)
	}
	if len(messages) != 1 || hasFlag(messages[0].Flags, mrsync.FlagFlagged) {
		t.Fatalf("after StoreFlags(remove \\Flagged): messages = %+v, want 1 message without \\Flagged", messages)
	}
}

func TestLiveCopyMessage(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const subject = "Copy test message"
	uid := appendTestMessage(t, ctx, sess, mailboxDrafts, subject, nil)

	if err := sess.Copy(ctx, mailboxDrafts, mrsync.UIDSetNum(uid), mailboxArchive); err != nil {
		t.Fatalf("Copy(%q -> %q) error = %v", mailboxDrafts, mailboxArchive, err)
	}

	// Archive is written by this test only, so a mailbox-wide check is safe
	// here (unlike the shared Drafts source mailbox).
	status, err := sess.Select(ctx, mailboxArchive, mrsync.SelectOptions{})
	if err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxArchive, err)
	}
	if status.NumMessages != 1 {
		t.Fatalf("Archive NumMessages = %d, want 1 after Copy", status.NumMessages)
	}
	messages, err := collectByUID(ctx, sess, mrsync.UIDSetRange(1, 0), mrsync.FetchItems{Envelope: true})
	if err != nil {
		t.Fatalf("FetchByUID(Archive) error = %v", err)
	}
	if len(messages) != 1 || messages[0].Envelope == nil || messages[0].Envelope.Subject != subject {
		t.Fatalf("Archive messages = %+v, want 1 message with Subject %q", messages, subject)
	}

	// The source message must still be present in Drafts (Copy, not Move).
	// FetchByUID acts on whatever is currently selected, which the Select
	// above just switched to Archive — reselect Drafts first.
	if _, err := sess.Select(ctx, mailboxDrafts, mrsync.SelectOptions{}); err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxDrafts, err)
	}
	stillThere, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Envelope: true})
	if err != nil {
		t.Fatalf("FetchByUID(Drafts, source UID) error = %v", err)
	}
	if len(stillThere) != 1 {
		t.Errorf("source message in Drafts after Copy: got %d messages, want 1 (Copy must not remove the source)", len(stillThere))
	}
}

func TestLiveMoveMessage(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !sess.Supports(mrsync.CapabilityMove) {
		t.Fatal("server does not advertise MOVE; harness assumption violated")
	}

	const subject = "Move test message"
	uid := appendTestMessage(t, ctx, sess, mailboxDrafts, subject, nil)

	if err := sess.Move(ctx, mailboxDrafts, mrsync.UIDSetNum(uid), mailboxTrash); err != nil {
		t.Fatalf("Move(%q -> %q) error = %v", mailboxDrafts, mailboxTrash, err)
	}

	// Trash is written by this test only.
	status, err := sess.Select(ctx, mailboxTrash, mrsync.SelectOptions{})
	if err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxTrash, err)
	}
	if status.NumMessages != 1 {
		t.Fatalf("Trash NumMessages = %d, want 1 after Move", status.NumMessages)
	}
	messages, err := collectByUID(ctx, sess, mrsync.UIDSetRange(1, 0), mrsync.FetchItems{Envelope: true})
	if err != nil {
		t.Fatalf("FetchByUID(Trash) error = %v", err)
	}
	if len(messages) != 1 || messages[0].Envelope == nil || messages[0].Envelope.Subject != subject {
		t.Fatalf("Trash messages = %+v, want 1 message with Subject %q", messages, subject)
	}

	// The source message must be gone from Drafts (Move, not Copy). The
	// server returns an empty FETCH result for a UID that no longer exists
	// rather than an error. FetchByUID acts on whatever is currently
	// selected, which the Select above just switched to Trash — reselect
	// Drafts first.
	if _, err := sess.Select(ctx, mailboxDrafts, mrsync.SelectOptions{}); err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxDrafts, err)
	}
	gone, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Envelope: true})
	if err != nil {
		t.Fatalf("FetchByUID(Drafts, moved UID) error = %v", err)
	}
	if len(gone) != 0 {
		t.Errorf("source message in Drafts after Move: got %d messages, want 0 (Move must remove the source)", len(gone))
	}
}

func TestLiveExpungeMessage(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !sess.Supports(mrsync.CapabilityUIDPlus) {
		t.Fatal("server does not advertise UIDPLUS; harness assumption violated")
	}

	// Junk is written by this test only.
	uid := appendTestMessage(t, ctx, sess, mailboxJunk, "Expunge test message", nil)

	if err := sess.StoreFlags(ctx, mailboxJunk, uid, []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
		t.Fatalf("StoreFlags(add \\Deleted) error = %v", err)
	}
	removed, err := sess.Expunge(ctx, mailboxJunk, mrsync.UIDSetNum(uid))
	if err != nil {
		t.Fatalf("Expunge() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("Expunge() removed = %d, want 1 — the server must report the message it destroyed", removed)
	}

	status, err := sess.Select(ctx, mailboxJunk, mrsync.SelectOptions{})
	if err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxJunk, err)
	}
	if status.NumMessages != 0 {
		t.Fatalf("Junk NumMessages = %d, want 0 after Expunge", status.NumMessages)
	}

	messages, err := collectByUID(ctx, sess, mrsync.UIDSetNum(uid), mrsync.FetchItems{Envelope: true})
	if err != nil {
		t.Fatalf("FetchByUID(Junk, expunged UID) error = %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expunged message still fetchable: got %d messages, want 0", len(messages))
	}

	// ⚠ THE NEGATIVE CONTROL, and the whole reason Expunge reports a count at
	// all (#296). Ask a REAL server to expunge the UID it has just removed. It
	// answers OK — nothing failed, nothing to fail — and the only thing that
	// separates this from the successful destruction three lines above is the
	// count. Without it, writeback.Result.Expunged reports mail destroyed every
	// time an expunge sails past an empty mailbox, which is exactly the
	// same-green failure #268 was made of.
	//
	// A fake cannot establish this: it returns whatever it was told to. Only a
	// live Dovecot can show that "succeeded" and "removed something" are
	// genuinely different answers.
	again, err := sess.Expunge(ctx, mailboxJunk, mrsync.UIDSetNum(uid))
	if err != nil {
		t.Fatalf("Expunge() of an already-expunged UID error = %v — the server accepts this, it is not a failure", err)
	}
	if again != 0 {
		t.Errorf("Expunge() of an already-expunged UID removed = %d, want 0", again)
	}
}

// UIDPresent exists to answer the one question a UID EXPUNGE's own removal
// count cannot when that count is zero (#611): did the server genuinely have
// nothing to remove, or does it still hold the message? A fake cannot vouch
// for this either — the property under test is a bare UID SEARCH's behaviour
// against a real server across three states a message actually passes
// through: present, \Deleted-but-not-yet-expunged (which still matches a
// SEARCH — only EXPUNGE removes a message from the mailbox), and gone.
//
// It owns Junk for its duration, same as TestLiveExpungeMessage, and cleans
// up after itself by expunging the one message it appends.
func TestLiveUIDPresent(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	uid := appendTestMessage(t, ctx, sess, mailboxJunk, "UIDPresent test message", nil)

	present, err := sess.UIDPresent(ctx, mailboxJunk, uid)
	if err != nil {
		t.Fatalf("UIDPresent() error = %v", err)
	}
	if !present {
		t.Fatal("UIDPresent() = false for a message that was just appended, want true")
	}

	// \Deleted but not yet expunged still matches a UID SEARCH — this is the
	// exact state #611's re-observation must be able to see as "still there",
	// which is what distinguishes it from a fabricated "it must be gone".
	if err := sess.StoreFlags(ctx, mailboxJunk, uid, []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
		t.Fatalf("StoreFlags(add \\Deleted) error = %v", err)
	}
	present, err = sess.UIDPresent(ctx, mailboxJunk, uid)
	if err != nil {
		t.Fatalf("UIDPresent() after STORE \\Deleted error = %v", err)
	}
	if !present {
		t.Fatal("UIDPresent() = false for a \\Deleted-but-unexpunged message, want true")
	}

	removed, err := sess.Expunge(ctx, mailboxJunk, mrsync.UIDSetNum(uid))
	if err != nil {
		t.Fatalf("Expunge() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("Expunge() removed = %d, want 1", removed)
	}
	present, err = sess.UIDPresent(ctx, mailboxJunk, uid)
	if err != nil {
		t.Fatalf("UIDPresent() after Expunge error = %v", err)
	}
	if present {
		t.Fatal("UIDPresent() = true for an expunged UID, want false")
	}
}

// LiveUIDs is the expunge reaper's ONLY source of truth (mailroom#86): the reaper
// deletes a message's provenance — and, when nothing is left, soft-deletes the
// message — purely because this call did not list its UID. So if it lies, mail
// disappears. Every property the reaper leans on is therefore asserted against a
// real Dovecot rather than a fake, because each one is a property of the SERVER
// and the client library, not of our code:
//
//   - the UID set is complete (a message that IS there is listed);
//   - an EXPUNGED UID stops being listed, and its neighbours do not;
//   - an EMPTY mailbox answers "empty" cleanly — no error — which is the single
//     answer that would authorize dropping every provenance row for a folder;
//   - EXISTS corroborates the UID count (the reaper's empty-answer guard);
//   - UIDVALIDITY and UIDNEXT come back on the same SELECT (the generation the
//     diff is keyed by, and the bound that keeps a concurrently-arriving message
//     out of the reap).
//
// It owns Junk for its duration and drains it at both ends, so — per this file's
// discipline — it makes no assumption about what any other test left behind.
func TestLiveLiveUIDsIsTheReapersSourceOfTruth(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)
	ctx := context.Background()

	const box = mailboxJunk
	drain := func() {
		_, uids, err := sess.LiveUIDs(ctx, box)
		if err != nil {
			t.Fatalf("LiveUIDs(%q) error = %v", box, err)
		}
		for _, uid := range uids {
			if err := sess.StoreFlags(ctx, box, uid, []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
				t.Fatalf("StoreFlags() error = %v", err)
			}
			// The removal count is ignored on purpose: this is cleanup, and
			// "there was nothing left to remove" is a fine outcome here.
			if _, err := sess.Expunge(ctx, box, mrsync.UIDSetNum(uid)); err != nil {
				t.Fatalf("Expunge() error = %v", err)
			}
		}
	}
	drain()
	t.Cleanup(drain)

	// THE EMPTY ANSWER. An empty mailbox must report empty with no error, and
	// EXISTS must corroborate it — that corroboration is exactly what lets the
	// reaper tell "the folder is empty" apart from "the answer came back broken".
	// (This is not circular with drain(): drain used LiveUIDs to find what to
	// expunge, but EXISTS is an independent channel — it comes from SELECT. If
	// LiveUIDs had under-reported, EXISTS would still be non-zero here.)
	status, uids, err := sess.LiveUIDs(ctx, box)
	if err != nil {
		t.Fatalf("LiveUIDs() on an empty mailbox error = %v", err)
	}
	if len(uids) != 0 || status.NumMessages != 0 {
		t.Fatalf("empty mailbox reports %d UIDs / EXISTS %d, want 0/0", len(uids), status.NumMessages)
	}
	if status.UIDNext == 0 {
		t.Error("no UIDNEXT on an empty mailbox: the reaper loses its race bound")
	}
	if status.UIDValidity == 0 {
		t.Error("no UIDVALIDITY: the reaper cannot key the diff to a generation")
	}

	// THE COMPLETE ANSWER. Everything in the mailbox is listed, everything listed
	// is below UIDNEXT, and EXISTS agrees with the count.
	var appended []mrsync.UID
	for _, subj := range []string{"live-uids-a", "live-uids-b", "live-uids-c"} {
		appended = append(appended, appendTestMessage(t, ctx, sess, box, subj, nil))
	}
	status, uids, err = sess.LiveUIDs(ctx, box)
	if err != nil {
		t.Fatalf("LiveUIDs() error = %v", err)
	}
	for _, uid := range appended {
		if !containsUID(uids, uid) {
			t.Errorf("LiveUIDs() omitted UID %d, which IS in the mailbox — the reaper would delete it", uid)
		}
		if uid >= status.UIDNext {
			t.Errorf("UID %d >= UIDNEXT %d: the reap bound does not cover a message that exists", uid, status.UIDNext)
		}
	}
	if int(status.NumMessages) != len(uids) {
		t.Errorf("EXISTS = %d but UID SEARCH ALL returned %d UIDs", status.NumMessages, len(uids))
	}

	// THE EXPUNGE. What another IMAP client removes stops being listed — and only
	// that one does.
	gone := appended[1]
	if err := sess.StoreFlags(ctx, box, gone, []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
		t.Fatalf("StoreFlags() error = %v", err)
	}
	removedOne, err := sess.Expunge(ctx, box, mrsync.UIDSetNum(gone))
	if err != nil {
		t.Fatalf("Expunge() error = %v", err)
	}
	if removedOne != 1 {
		t.Errorf("Expunge() removed = %d, want exactly 1 — one message was asked for and three are present", removedOne)
	}
	_, uids, err = sess.LiveUIDs(ctx, box)
	if err != nil {
		t.Fatalf("LiveUIDs() after expunge error = %v", err)
	}
	if containsUID(uids, gone) {
		t.Errorf("expunged UID %d is still reported live — the reaper would never remove it", gone)
	}
	for _, uid := range []mrsync.UID{appended[0], appended[2]} {
		if !containsUID(uids, uid) {
			t.Errorf("UID %d vanished from the live set after expunging a DIFFERENT message", uid)
		}
	}
}

// SearchMessageID is how the writeback engine finds a copy it must remove when
// it has no UID to fire at: a superseded draft version (store.RetireDraft) and —
// since mailroom#146 — the draft copy of a message that has just been sent. Both
// paths end in an EXPUNGE, so if this search returns the wrong UID, or one too
// many, Mailroom deletes mail the user did not ask it to delete.
//
// That makes it exactly the kind of thing a fake cannot vouch for: whether an
// IMAP SEARCH keyed on a HEADER matches the one message we mean is a property of
// the SERVER, not of our code. The same class of assumption, unverified, is what
// let a non-peeking FETCH mark every synced message \Seen (mailroom#129).
//
// It owns Archive for its duration and drains it at both ends, so it makes no
// assumption about what another test left behind.
func TestLiveSearchMessageIDNamesExactlyOneCopy(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The cleanup drain takes its OWN context: t.Cleanup runs after the test
	// function returns, by which point this test's `defer cancel()` has already
	// fired and a shared ctx would be cancelled.
	drain := func(ctx context.Context, when string) {
		t.Helper()
		_, uids, err := sess.LiveUIDs(ctx, mailboxArchive)
		if err != nil {
			t.Fatalf("LiveUIDs(%s) error = %v", when, err)
		}
		if len(uids) == 0 {
			return
		}
		for _, uid := range uids {
			if err := sess.StoreFlags(ctx, mailboxArchive, uid, []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
				t.Fatalf("StoreFlags(drain %s) error = %v", when, err)
			}
		}
		// Cleanup: the removal count is not interesting, only that it did not
		// error.
		if _, err := sess.Expunge(ctx, mailboxArchive, mrsync.UIDSetNum(uids...)); err != nil {
			t.Fatalf("Expunge(drain %s) error = %v", when, err)
		}
	}
	drain(ctx, "before")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		drain(cleanupCtx, "after")
	})

	// Two messages in the same folder, distinct Message-IDs — the situation the
	// search has to be precise about, because the other one is somebody's mail.
	mine := fmt.Sprintf("<sent-draft-%d@example.com>", time.Now().UnixNano())
	other := fmt.Sprintf("<untouched-%d@example.com>", time.Now().UnixNano()+1)
	appendWithID := func(msgID, subject string) mrsync.UID {
		t.Helper()
		body := fmt.Sprintf(
			"From: Sync Tester <sync-tester@example.com>\r\n"+
				"To: Sync Tester <sync-tester@example.com>\r\n"+
				"Subject: %s\r\n"+
				"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n"+
				"Message-ID: %s\r\n"+
				"Content-Type: text/plain; charset=utf-8\r\n"+
				"\r\n"+
				"Body for %s.\r\n", subject, msgID, subject)
		uid, err := sess.Append(ctx, mailboxArchive, []mrsync.Flag{mrsync.FlagDraft}, time.Time{}, []byte(body))
		if err != nil {
			t.Fatalf("Append(%s) error = %v", subject, err)
		}
		return uid
	}
	target := appendWithID(mine, "the draft that was sent")
	bystander := appendWithID(other, "somebody else's draft")

	uids, err := sess.SearchMessageID(ctx, mailboxArchive, mine)
	if err != nil {
		t.Fatalf("SearchMessageID() error = %v", err)
	}
	if len(uids) != 1 || uids[0] != target {
		t.Fatalf("SearchMessageID(%s) = %v, want exactly [%d] — an expunge fires at whatever this returns",
			mine, uids, target)
	}

	// And the expunge it authorizes removes that copy and only that copy.
	if err := sess.StoreFlags(ctx, mailboxArchive, uids[0], []mrsync.Flag{mrsync.FlagDeleted}, nil); err != nil {
		t.Fatalf("StoreFlags(add \\Deleted) error = %v", err)
	}
	removed, err := sess.Expunge(ctx, mailboxArchive, mrsync.UIDSetNum(uids[0]))
	if err != nil {
		t.Fatalf("Expunge() error = %v", err)
	}
	// The bystander is still there, so a count above 1 would mean the expunge
	// widened beyond the UID it was authorized for — the failure this test is
	// really about, now visible as a number rather than only as a survivor check.
	if removed != 1 {
		t.Errorf("Expunge() removed = %d, want exactly 1 (the searched-for copy, and only it)", removed)
	}
	_, live, err := sess.LiveUIDs(ctx, mailboxArchive)
	if err != nil {
		t.Fatalf("LiveUIDs() after expunge error = %v", err)
	}
	if containsUID(live, target) {
		t.Error("the sent message's draft copy survived the expunge — it would still show in Drafts and be re-tagged \\Draft")
	}
	if !containsUID(live, bystander) {
		t.Error("expunging the searched-for copy took a DIFFERENT message with it")
	}

	// A Message-ID nobody has must match nothing, not everything: the engine
	// treats "no hits" as "already gone", and a wildcard answer here would turn
	// that into "delete the folder".
	none, err := sess.SearchMessageID(ctx, mailboxArchive, "<no-such-message@example.com>")
	if err != nil {
		t.Fatalf("SearchMessageID(absent) error = %v", err)
	}
	if len(none) != 0 {
		t.Errorf("searching for an absent Message-ID returned %v, want nothing", none)
	}
}

func containsUID(uids []mrsync.UID, want mrsync.UID) bool {
	for _, u := range uids {
		if u == want {
			return true
		}
	}
	return false
}

// collectByUID drains a streaming fetch into a slice.
//
// SyncSession.FetchByUID streams (#351): the production path ingests each
// message as it arrives so that fetching an entire mailbox — a first sync, or
// any UIDVALIDITY roll — does not hold it all in memory at once. These live
// tests assert on small, known fixtures where a slice is the clearer thing to
// assert against, so they collect deliberately. Nothing in production does.
func collectByUID(ctx context.Context, s mrsync.SyncSession, uids mrsync.UIDSet, items mrsync.FetchItems) ([]mrsync.Message, error) {
	var out []mrsync.Message
	err := s.FetchByUID(ctx, uids, items, func(m mrsync.Message) error {
		out = append(out, m)
		return nil
	})
	return out, err
}

// LIST-STATUS (#620): the numbers the LIST carries inline must be the SAME
// numbers a SELECT of the mailbox reports, or the engine's "unchanged since the
// last poll" comparison compares two different things. Dovecot advertises
// LIST-STATUS, so a nil Status here means the request was never made.
func TestLiveListMailboxesCarriesStatus(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !sess.Supports(mrsync.CapabilityCondStore) {
		t.Fatal("server does not advertise CONDSTORE; harness assumption violated")
	}
	mailboxes, err := sess.ListMailboxes(ctx)
	if err != nil {
		t.Fatalf("ListMailboxes() error = %v", err)
	}
	var listed *mrsync.ListStatus
	for _, mb := range mailboxes {
		if mb.Name == mailboxInbox {
			listed = mb.Status
		}
	}
	if listed == nil {
		t.Fatal("INBOX listed with no Status: LIST-STATUS was not requested or not mapped")
	}

	selected, err := sess.Select(ctx, mailboxInbox, mrsync.SelectOptions{ReadOnly: true, CondStore: true})
	if err != nil {
		t.Fatalf("Select(%q) error = %v", mailboxInbox, err)
	}
	if listed.UIDValidity != selected.UIDValidity || listed.UIDNext != selected.UIDNext ||
		listed.NumMessages != selected.NumMessages || listed.HighestModSeq != selected.HighestModSeq {
		t.Errorf("LIST-STATUS %+v disagrees with SELECT (uidvalidity %d, uidnext %d, messages %d, modseq %d)",
			*listed, selected.UIDValidity, selected.UIDNext, selected.NumMessages, selected.HighestModSeq)
	}
	if listed.HighestModSeq == 0 {
		t.Error("LIST-STATUS HighestModSeq = 0 on a CONDSTORE server: HIGHESTMODSEQ was not requested")
	}
}

// The observation's own status must carry HIGHESTMODSEQ on a FRESH connection
// (#620): it is the baseline a later LIST-STATUS is compared against, and a pass
// whose polls were all skipped sends no CONDSTORE SELECT that would otherwise
// have enabled it.
//
// ⚠ WHAT THIS DOES NOT PROVE. LiveUIDs now EXAMINEs with CONDSTORE for servers
// that only report HIGHESTMODSEQ once CONDSTORE is enabled. Dovecot 2.3 is not
// one of them: with that parameter deleted, this test stays GREEN (probed
// 2026-09-24), because Dovecot reports the modseq on a plain EXAMINE anyway. So
// this pins that the baseline HAS a modseq against this server, not that the
// CONDSTORE parameter is what put it there.
func TestLiveLiveUIDsReportsModSeqOnAFreshSession(t *testing.T) {
	cfg := testConfig(t)
	sess := dial(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	status, _, err := sess.LiveUIDs(ctx, mailboxInbox)
	if err != nil {
		t.Fatalf("LiveUIDs(%q) error = %v", mailboxInbox, err)
	}
	if status.HighestModSeq == 0 {
		t.Error("LiveUIDs status HighestModSeq = 0 on a fresh CONDSTORE session: the EXAMINE did not request CONDSTORE")
	}
}
