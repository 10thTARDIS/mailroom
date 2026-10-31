// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Package mailsync's live end-to-end coverage for the flags-only CONDSTORE
// path (#523): a change another IMAP client makes to a message's FLAGS ALONE
// must not re-download the message body (and, downstream, must not re-scan
// it). Proved against a real Dovecot with a recording session wrapper —
// only a real server can confirm what a CONDSTORE FETCH without BODY actually
// returns; an in-memory fake cannot tell you that (the reasoning behind every
// other suite sharing this fixture).
//
// Run via scripts/mailsync-test.sh; without a live Dovecot + PostgreSQL every
// test here skips (newLiveFixture).
package mailsync

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/sync"
)

// fetchLog records every FETCH a recordingSession issues: the item set asked
// for, and the total bytes of every body actually returned.
type fetchLog struct {
	items     []sync.FetchItems
	bodyBytes int64
}

// recordingSession wraps a real sync.SyncSession and records into log — the
// observable #523 exists to make visible, straight off the wire rather than
// through anything the engine itself counts.
type recordingSession struct {
	sync.SyncSession
	log *fetchLog
}

func (r *recordingSession) FetchByUID(ctx context.Context, uids sync.UIDSet, items sync.FetchItems, fn func(sync.Message) error) error {
	r.log.items = append(r.log.items, items)
	return r.SyncSession.FetchByUID(ctx, uids, items, func(m sync.Message) error {
		r.log.bodyBytes += int64(len(m.Body))
		return fn(m)
	})
}

// recordingEngine is a real engine against the real server whose dialed
// session is wrapped to record every FETCH into log.
func (f *liveFixture) recordingEngine(log *fetchLog) *Engine {
	return New(NewStoreAdapter(f.st), ingest.New(f.st, f.blobs),
		func(ctx context.Context, cfg sync.Config) (sync.SyncSession, error) {
			s, err := sync.Dial(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return &recordingSession{SyncSession: s, log: log}, nil
		}, WithReapInterval(time.Nanosecond))
}

// flagged reads is_flagged for a canonical message — what a JMAP client would
// see reflected from the merged live provenance flags.
func (f *liveFixture) flagged(t *testing.T, msg ulid.ULID) bool {
	t.Helper()
	var flagged bool
	if err := f.super.QueryRow(context.Background(),
		`SELECT is_flagged FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&flagged); err != nil {
		t.Fatalf("flagged: %v", err)
	}
	return flagged
}

// ⚠ THE HEADLINE ASSERTION FOR #523, against a REAL server. A message already
// synced, then flagged by ANOTHER IMAP client, must be refreshed with a
// CONDSTORE flag-only FETCH — no body, on the wire, at all — on the pass that
// picks the change up.
//
// PROBE (negative control N1): put BodyFull/Envelope/InternalDate back on the
// flag-pass items in engine.go and this goes red at the no-body assertion.
func TestLiveFlagOnlyChangeFetchesNoBody(t *testing.T) {
	f := newLiveFixture(t)
	uid := f.appendMsg(t, f.inbox, "flags-only-523")

	firstLog := &fetchLog{}
	sum1, err := f.recordingEngine(firstLog).SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(sum1.Errors) != 0 {
		t.Fatalf("first sync errors: %v", sum1.Errors)
	}
	msg, ok := f.messageIDBySubject(t, "flags-only-523")
	if !ok {
		t.Fatal("message was never ingested")
	}
	if f.flagged(t, msg) {
		t.Fatal("precondition: message must start unflagged")
	}

	// Another IMAP client flags it. Nothing about the BODY changes on the
	// server at all.
	if err := f.sess.StoreFlags(context.Background(), f.inbox, uid, []sync.Flag{sync.FlagFlagged}, nil); err != nil {
		t.Fatalf("store \\Flagged: %v", err)
	}

	secondLog := &fetchLog{}
	sum2, err := f.recordingEngine(secondLog).SyncAccount(context.Background(), AccountSync{
		UserID: f.userID, AccountID: f.accountID, Config: f.cfg, DEK: f.dek, DEKID: f.dekID,
	})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(sum2.Errors) != 0 {
		t.Fatalf("second sync errors: %v", sum2.Errors)
	}

	if !f.flagged(t, msg) {
		t.Fatal("the store's message was never flagged — the flags-only pass did not take effect")
	}

	// THE ASSERTION THAT MATTERS: not one FETCH in the second pass asked for a
	// body.
	for _, items := range secondLog.items {
		if items.BodyFull || items.Envelope || items.InternalDate {
			t.Fatalf("the second pass issued a FETCH requesting %+v — "+
				"a flags-only server change must never fetch a body", items)
		}
	}
	if secondLog.bodyBytes != 0 {
		t.Errorf("second pass body bytes off the wire = %d, want 0", secondLog.bodyBytes)
	}
	if sum2.BodiesFetched != 0 || sum2.BodyBytesFetched != 0 {
		t.Errorf("second pass summary = %+v, want BodiesFetched=0 BodyBytesFetched=0", sum2)
	}
	if sum2.MessagesIngested != 0 {
		t.Errorf("second pass MessagesIngested = %d, want 0 (no new message, no re-created one)", sum2.MessagesIngested)
	}
	if sum2.FlagUpdates != 1 {
		t.Errorf("second pass FlagUpdates = %d, want 1", sum2.FlagUpdates)
	}
}
