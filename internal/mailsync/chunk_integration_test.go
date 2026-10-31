// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package mailsync

// #656 against a REAL server and a REAL database: a backlog larger than the
// chunk size is fetched as a walk of bounded commands, every message arrives,
// and a pass that dies part-way resumes where it stopped — with the resume
// position and the ABSENT poll baseline both round-tripped through the store
// adapter's real SQL (poll_uidnext = NULLIF($7, 0)). Run via
// scripts/mailsync-test.sh; without Dovecot + PostgreSQL it skips.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	gosync "sync"
	"testing"

	"mailroom/internal/ingest"
	"mailroom/internal/sync"
)

const liveChunk = 7

// chunkRecorder is a real session that records every SELECT and every body
// FETCH, and can kill the failOn-th body FETCH after it has streamed failAfter
// messages — a connection dying mid-command, as the provider's did.
type chunkRecorder struct {
	sync.SyncSession
	mu        gosync.Mutex
	selects   map[string]int
	bodies    []sync.UIDSet
	failOn    int // 1-based body FETCH to kill; 0 = never
	failAfter int
}

func (s *chunkRecorder) Select(ctx context.Context, name string, opts sync.SelectOptions) (*sync.MailboxStatus, error) {
	s.mu.Lock()
	s.selects[name]++
	s.mu.Unlock()
	return s.SyncSession.Select(ctx, name, opts)
}

var errInjectedMidChunk = errors.New("injected: connection died mid-FETCH")

func (s *chunkRecorder) FetchByUID(ctx context.Context, uids sync.UIDSet, items sync.FetchItems, fn func(sync.Message) error) error {
	if !items.BodyFull {
		return s.SyncSession.FetchByUID(ctx, uids, items, fn)
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, uids)
	kill := s.failOn != 0 && len(s.bodies) == s.failOn
	s.mu.Unlock()
	if !kill {
		return s.SyncSession.FetchByUID(ctx, uids, items, fn)
	}
	n := 0
	return s.SyncSession.FetchByUID(ctx, uids, items, func(m sync.Message) error {
		if n == s.failAfter {
			return errInjectedMidChunk
		}
		n++
		return fn(m)
	})
}

// chunkPass runs one pass with a fresh recorder and a WithFetchChunk(7) engine.
func (f *liveFixture) chunkPass(t *testing.T, failOn, failAfter int) (Summary, *chunkRecorder) {
	t.Helper()
	rec := &chunkRecorder{selects: map[string]int{}, failOn: failOn, failAfter: failAfter}
	e := New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			rec.SyncSession = s
			return rec, nil
		}, WithFetchChunk(liveChunk))
	return f.run(t, e), rec
}

// storedPollUIDNext is the #620 poll baseline's UIDNEXT; nil is NULL (none).
func (f *liveFixture) storedPollUIDNext(t *testing.T, imapName string) *int64 {
	t.Helper()
	var v *int64
	if err := f.super.QueryRow(context.Background(),
		`SELECT poll_uidnext FROM app.mailboxes WHERE user_id = $1 AND imap_name = $2`,
		f.userID.String(), imapName).Scan(&v); err != nil {
		t.Fatalf("stored poll_uidnext: %v", err)
	}
	return v
}

// backlog syncs one seed message (so the cursor sits at a known UID), then
// appends n more on the server. It returns the seed UID; the backlog is
// seed+1 .. seed+n.
func (f *liveFixture) backlog(t *testing.T, n int) sync.UID {
	t.Helper()
	seed := f.appendMsg(t, f.inbox, "chunk-seed")
	if sum, _ := f.chunkPass(t, 0, 0); len(sum.Errors) != 0 {
		t.Fatalf("seed pass errors: %v", sum.Errors)
	}
	if got := f.storedLastSeenUID(t, f.inbox); got != int64(seed) {
		t.Fatalf("seed pass cursor = %d, want %d", got, seed)
	}
	for i := 1; i <= n; i++ {
		if uid := f.appendMsg(t, f.inbox, fmt.Sprintf("chunk-%02d", i)); uid != seed+sync.UID(i) {
			t.Fatalf("append %d got UID %d, want %d (the expected ranges assume contiguous UIDs)", i, uid, seed+sync.UID(i))
		}
	}
	return seed
}

func (f *liveFixture) ingestedBacklog(t *testing.T, n int) []int {
	t.Helper()
	var have []int
	for i := 1; i <= n; i++ {
		if _, ok := f.messageIDBySubject(t, fmt.Sprintf("chunk-%02d", i)); ok {
			have = append(have, i)
		}
	}
	return have
}

func TestLiveBacklogIsFetchedInBoundedChunks(t *testing.T) {
	f := newLiveFixture(t)
	seed := f.backlog(t, 20)

	sum, rec := f.chunkPass(t, 0, 0)
	if len(sum.Errors) != 0 {
		t.Fatalf("errors: %v", sum.Errors)
	}
	// 20 new UIDs at 7 per command: two closed chunks, then the open tail.
	want := []sync.UIDSet{
		sync.UIDSetRange(seed+1, seed+7),
		sync.UIDSetRange(seed+8, seed+14),
		sync.UIDSetRange(seed+15, 0),
	}
	if !reflect.DeepEqual(rec.bodies, want) {
		t.Fatalf("body FETCH commands = %v, want %v", rec.bodies, want)
	}
	if have := f.ingestedBacklog(t, 20); len(have) != 20 {
		t.Fatalf("ingested %d of 20 backlog messages: %v", len(have), have)
	}
	if got := f.storedLastSeenUID(t, f.inbox); got != int64(seed)+20 {
		t.Errorf("cursor = %d, want %d", got, int64(seed)+20)
	}
}

func TestLivePartialBacklogResumesAndIsNotSkipped(t *testing.T) {
	f := newLiveFixture(t)
	seed := f.backlog(t, 20)

	// Pass A: the SECOND command dies after streaming two messages.
	sum, rec := f.chunkPass(t, 2, 2)
	if len(sum.Errors) != 1 || !errors.Is(sum.Errors[0].Err, errInjectedMidChunk) {
		t.Fatalf("pass A errors = %v, want only the injected mid-chunk failure", sum.Errors)
	}
	if n := len(rec.bodies); n != 2 {
		t.Fatalf("pass A issued %d body FETCHes (%v), want 2 — it must stop at the failure", n, rec.bodies)
	}
	if have := f.ingestedBacklog(t, 20); len(have) != 9 {
		t.Fatalf("pass A kept %v, want backlog 1..9 (chunk 1 whole, 2 of chunk 2)", have)
	}
	if got := f.storedLastSeenUID(t, f.inbox); got != int64(seed)+7 {
		t.Errorf("pass A cursor = %d, want %d (end of the last COMPLETED chunk)", got, int64(seed)+7)
	}
	if p := f.storedPollUIDNext(t, f.inbox); p != nil {
		t.Errorf("pass A left poll_uidnext = %d, want NULL — a half-fetched folder must never look polled", *p)
	}

	// Pass B: the server behaves. Nothing changed on it since pass A, so only
	// the missing baseline stands between this folder and a #620 skip.
	sum, rec = f.chunkPass(t, 0, 0)
	if len(sum.Errors) != 0 {
		t.Fatalf("pass B errors: %v", sum.Errors)
	}
	if rec.selects[f.inbox] != 1 {
		t.Fatalf("pass B selected INBOX %d times, want 1 — the rest of the backlog is stranded", rec.selects[f.inbox])
	}
	want := []sync.UIDSet{sync.UIDSetRange(seed+8, seed+14), sync.UIDSetRange(seed+15, 0)}
	if !reflect.DeepEqual(rec.bodies, want) {
		t.Fatalf("pass B body FETCH commands = %v, want a resume after the completed chunk: %v", rec.bodies, want)
	}
	if have := f.ingestedBacklog(t, 20); len(have) != 20 {
		t.Fatalf("after pass B ingested %d of 20: %v", len(have), have)
	}
	if got := f.storedLastSeenUID(t, f.inbox); got != int64(seed)+20 {
		t.Errorf("pass B cursor = %d, want %d", got, int64(seed)+20)
	}
	if p := f.storedPollUIDNext(t, f.inbox); p == nil || *p != int64(seed)+21 {
		t.Errorf("pass B poll_uidnext = %v, want %d — a complete poll records its baseline", p, int64(seed)+21)
	}

	// Pass C (positive control): complete and unchanged, so it IS skipped.
	// Without this, pass B's SELECT could be a fixture that never skips.
	sum, rec = f.chunkPass(t, 0, 0)
	if len(sum.Errors) != 0 {
		t.Fatalf("pass C errors: %v", sum.Errors)
	}
	if rec.selects[f.inbox] != 0 || sum.MailboxesUnchanged == 0 {
		t.Errorf("pass C selected INBOX %d times (unchanged=%d), want the fully polled folder skipped",
			rec.selects[f.inbox], sum.MailboxesUnchanged)
	}
}
