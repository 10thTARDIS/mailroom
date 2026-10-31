// SPDX-License-Identifier: AGPL-3.0-only

// Package sync_test (external test package, matching live_integration_test.go's
// convention) proves two things about Writer:
//
//  1. *sync.Writer structurally satisfies writeback.IMAPWriter — the
//     compile-time assertion below. This is the one place in the whole
//     change that imports both mailroom/internal/sync and
//     mailroom/internal/writeback together; it is safe precisely because
//     it's a test file in a separate (_test) package, so it never becomes
//     part of either package's production import graph (see writer.go's
//     doc comment for why internal/sync itself must not import
//     internal/writeback).
//  2. Writer's delegation logic (narrowing a UID to a one-element UIDSet
//     for Copy/Move/Expunge, discarding the returned UID and zeroing
//     internalDate for Append, deferring to Supports(CapabilityMove) for
//     SupportsMove) is correct, exercised against a fake SyncSession with
//     no network/database involved.
package sync_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	mrsync "mailroom/internal/sync"
	"mailroom/internal/writeback"
)

// Compile-time proof that *mrsync.Writer satisfies writeback.IMAPWriter
// without mrsync ever importing writeback (see writer.go).
var _ writeback.IMAPWriter = (*mrsync.Writer)(nil)

// fakeWriteSession is a minimal SyncSession fake recording write-op calls,
// for testing Writer's delegation in isolation from any real IMAP
// connection.
type fakeWriteSession struct {
	storeFlagsCalls []struct {
		mailbox     string
		uid         mrsync.UID
		add, remove []mrsync.Flag
	}
	copyCalls []struct {
		mailbox string
		uids    mrsync.UIDSet
		dest    string
	}
	moveCalls []struct {
		mailbox string
		uids    mrsync.UIDSet
		dest    string
	}
	expungeCalls []struct {
		mailbox string
		uids    mrsync.UIDSet
	}
	// expungeRemoved is how many messages this fake server reports having
	// removed. Writer must pass it through untouched: it is the only evidence
	// the writeback engine has that mail was actually destroyed (#296).
	expungeRemoved int
	appendCalls    []struct {
		mailbox      string
		flags        []mrsync.Flag
		internalDate time.Time
		body         []byte
	}
	searchCalls []struct {
		mailbox   string
		messageID string
	}
	appendUID  mrsync.UID
	appendErr  error
	searchUIDs []mrsync.UID
	searchErr  error
	supports   map[mrsync.Capability]bool

	uidPresentCalls []struct {
		mailbox string
		uid     mrsync.UID
	}
	uidPresent    bool
	uidPresentErr error
}

func (f *fakeWriteSession) ListMailboxes(ctx context.Context) ([]mrsync.Mailbox, error) {
	return nil, nil
}

func (f *fakeWriteSession) Select(ctx context.Context, mailbox string, opts mrsync.SelectOptions) (*mrsync.MailboxStatus, error) {
	return &mrsync.MailboxStatus{Name: mailbox}, nil
}

func (f *fakeWriteSession) FetchByUID(ctx context.Context, uids mrsync.UIDSet, items mrsync.FetchItems, fn func(mrsync.Message) error) error {
	return nil
}

func (f *fakeWriteSession) LiveUIDs(ctx context.Context, mailbox string) (*mrsync.MailboxStatus, []mrsync.UID, error) {
	return &mrsync.MailboxStatus{Name: mailbox}, nil, nil
}

func (f *fakeWriteSession) StoreFlags(ctx context.Context, mailbox string, uid mrsync.UID, add, remove []mrsync.Flag) error {
	f.storeFlagsCalls = append(f.storeFlagsCalls, struct {
		mailbox     string
		uid         mrsync.UID
		add, remove []mrsync.Flag
	}{mailbox, uid, add, remove})
	return nil
}

func (f *fakeWriteSession) Copy(ctx context.Context, mailbox string, uids mrsync.UIDSet, dest string) error {
	f.copyCalls = append(f.copyCalls, struct {
		mailbox string
		uids    mrsync.UIDSet
		dest    string
	}{mailbox, uids, dest})
	return nil
}

func (f *fakeWriteSession) Move(ctx context.Context, mailbox string, uids mrsync.UIDSet, dest string) error {
	f.moveCalls = append(f.moveCalls, struct {
		mailbox string
		uids    mrsync.UIDSet
		dest    string
	}{mailbox, uids, dest})
	return nil
}

func (f *fakeWriteSession) Expunge(ctx context.Context, mailbox string, uids mrsync.UIDSet) (int, error) {
	f.expungeCalls = append(f.expungeCalls, struct {
		mailbox string
		uids    mrsync.UIDSet
	}{mailbox, uids})
	return f.expungeRemoved, nil
}

func (f *fakeWriteSession) Append(ctx context.Context, mailbox string, flags []mrsync.Flag, internalDate time.Time, body []byte) (mrsync.UID, error) {
	f.appendCalls = append(f.appendCalls, struct {
		mailbox      string
		flags        []mrsync.Flag
		internalDate time.Time
		body         []byte
	}{mailbox, flags, internalDate, body})
	return f.appendUID, f.appendErr
}

func (f *fakeWriteSession) SearchMessageID(ctx context.Context, mailbox string, messageID string) ([]mrsync.UID, error) {
	f.searchCalls = append(f.searchCalls, struct {
		mailbox   string
		messageID string
	}{mailbox, messageID})
	return f.searchUIDs, f.searchErr
}

func (f *fakeWriteSession) UIDPresent(ctx context.Context, mailbox string, uid mrsync.UID) (bool, error) {
	f.uidPresentCalls = append(f.uidPresentCalls, struct {
		mailbox string
		uid     mrsync.UID
	}{mailbox, uid})
	return f.uidPresent, f.uidPresentErr
}

func (f *fakeWriteSession) Supports(c mrsync.Capability) bool {
	return f.supports[c]
}

func (f *fakeWriteSession) Noop(ctx context.Context) error { return nil }

func (f *fakeWriteSession) Notifications() <-chan mrsync.Notification { return nil }

func (f *fakeWriteSession) Idle(ctx context.Context) (mrsync.IdleSession, error) {
	return nil, errors.New("fakeWriteSession: Idle not implemented")
}

func (f *fakeWriteSession) Logout(ctx context.Context) error { return nil }

func (f *fakeWriteSession) Close() error { return nil }

var _ mrsync.SyncSession = (*fakeWriteSession)(nil)

func TestWriterStoreFlagsDelegatesDirectly(t *testing.T) {
	fake := &fakeWriteSession{}
	w := mrsync.NewWriter(fake)

	add := []mrsync.Flag{mrsync.FlagFlagged}
	remove := []mrsync.Flag{mrsync.FlagSeen}
	if err := w.StoreFlags(context.Background(), "INBOX", mrsync.UID(7), add, remove); err != nil {
		t.Fatalf("StoreFlags() error = %v", err)
	}

	if len(fake.storeFlagsCalls) != 1 {
		t.Fatalf("StoreFlags calls = %d, want 1", len(fake.storeFlagsCalls))
	}
	got := fake.storeFlagsCalls[0]
	if got.mailbox != "INBOX" || got.uid != 7 || !reflect.DeepEqual(got.add, add) || !reflect.DeepEqual(got.remove, remove) {
		t.Errorf("StoreFlags call = %+v, want mailbox=INBOX uid=7 add=%v remove=%v", got, add, remove)
	}
}

func TestWriterCopyNarrowsUIDToSet(t *testing.T) {
	fake := &fakeWriteSession{}
	w := mrsync.NewWriter(fake)

	if err := w.Copy(context.Background(), "INBOX", mrsync.UID(9), "Archive"); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}

	if len(fake.copyCalls) != 1 {
		t.Fatalf("Copy calls = %d, want 1", len(fake.copyCalls))
	}
	got := fake.copyCalls[0]
	want := mrsync.UIDSetNum(9)
	if got.mailbox != "INBOX" || got.dest != "Archive" || !reflect.DeepEqual(got.uids, want) {
		t.Errorf("Copy call = %+v, want mailbox=INBOX dest=Archive uids=%v", got, want)
	}
}

func TestWriterMoveNarrowsUIDToSet(t *testing.T) {
	fake := &fakeWriteSession{}
	w := mrsync.NewWriter(fake)

	if err := w.Move(context.Background(), "INBOX", mrsync.UID(3), "Trash"); err != nil {
		t.Fatalf("Move() error = %v", err)
	}

	if len(fake.moveCalls) != 1 {
		t.Fatalf("Move calls = %d, want 1", len(fake.moveCalls))
	}
	got := fake.moveCalls[0]
	want := mrsync.UIDSetNum(3)
	if got.mailbox != "INBOX" || got.dest != "Trash" || !reflect.DeepEqual(got.uids, want) {
		t.Errorf("Move call = %+v, want mailbox=INBOX dest=Trash uids=%v", got, want)
	}
}

func TestWriterExpungeNarrowsUIDToSet(t *testing.T) {
	fake := &fakeWriteSession{expungeRemoved: 1}
	w := mrsync.NewWriter(fake)

	removed, err := w.Expunge(context.Background(), "INBOX", mrsync.UID(11))
	if err != nil {
		t.Fatalf("Expunge() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("Expunge() removed = %d, want the session's 1 passed through", removed)
	}

	if len(fake.expungeCalls) != 1 {
		t.Fatalf("Expunge calls = %d, want 1", len(fake.expungeCalls))
	}
	got := fake.expungeCalls[0]
	want := mrsync.UIDSetNum(11)
	if got.mailbox != "INBOX" || !reflect.DeepEqual(got.uids, want) {
		t.Errorf("Expunge call = %+v, want mailbox=INBOX uids=%v", got, want)
	}
}

// A session that removed NOTHING must surface as 0, not be flattened into
// "no error, so it worked". A UID EXPUNGE of a UID the server no longer holds
// succeeds; if Writer swallowed the count, the writeback engine could never
// tell that apart from a real destruction (#296).
func TestWriterExpungeReportsThatNothingWasRemoved(t *testing.T) {
	fake := &fakeWriteSession{expungeRemoved: 0}
	w := mrsync.NewWriter(fake)

	removed, err := w.Expunge(context.Background(), "INBOX", mrsync.UID(11))
	if err != nil {
		t.Fatalf("Expunge() error = %v — removing nothing is not an error", err)
	}
	if removed != 0 {
		t.Errorf("Expunge() removed = %d, want 0", removed)
	}
}

// The internalDate is zeroed (the server assigns one), but the APPENDUID is
// PASSED BACK, not discarded as it once was: the writeback engine expunges a
// superseded draft moments after appending it, and the UID the server
// volunteered here is the one answer a SEARCH that soon has been seen to miss
// (#268).
func TestWriterAppendZeroesDateAndReturnsTheAppendUID(t *testing.T) {
	fake := &fakeWriteSession{appendUID: 42}
	w := mrsync.NewWriter(fake)

	body := []byte("From: a@example.com\r\n\r\nhi\r\n")
	flags := []mrsync.Flag{mrsync.FlagSeen}
	uid, err := w.Append(context.Background(), "Drafts", flags, body)
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if uid != 42 {
		t.Errorf("Append() uid = %d, want the server's APPENDUID 42 — dropping it is what made the expunge guess", uid)
	}

	if len(fake.appendCalls) != 1 {
		t.Fatalf("Append calls = %d, want 1", len(fake.appendCalls))
	}
	got := fake.appendCalls[0]
	if got.mailbox != "Drafts" || !reflect.DeepEqual(got.flags, flags) || !reflect.DeepEqual(got.body, body) {
		t.Errorf("Append call = %+v, want mailbox=Drafts flags=%v body=%q", got, flags, body)
	}
	if !got.internalDate.IsZero() {
		t.Errorf("Append call internalDate = %v, want zero (Writer.Append doesn't take one)", got.internalDate)
	}
}

func TestWriterAppendPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	fake := &fakeWriteSession{appendErr: wantErr}
	w := mrsync.NewWriter(fake)

	if _, err := w.Append(context.Background(), "Drafts", nil, []byte("x")); !errors.Is(err, wantErr) {
		t.Errorf("Append() error = %v, want %v", err, wantErr)
	}
}

// UIDPresent delegates directly, with no narrowing: SyncSession.UIDPresent
// already takes a single UID, unlike Copy/Move/Expunge (#611).
func TestWriterUIDPresentDelegatesDirectly(t *testing.T) {
	fake := &fakeWriteSession{uidPresent: true}
	w := mrsync.NewWriter(fake)

	present, err := w.UIDPresent(context.Background(), "Drafts", mrsync.UID(37))
	if err != nil {
		t.Fatalf("UIDPresent() error = %v", err)
	}
	if !present {
		t.Error("UIDPresent() = false, want the session's true passed through")
	}
	if len(fake.uidPresentCalls) != 1 {
		t.Fatalf("UIDPresent calls = %d, want 1", len(fake.uidPresentCalls))
	}
	got := fake.uidPresentCalls[0]
	if got.mailbox != "Drafts" || got.uid != 37 {
		t.Errorf("UIDPresent call = %+v, want mailbox=Drafts uid=37", got)
	}
}

func TestWriterUIDPresentPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	fake := &fakeWriteSession{uidPresentErr: wantErr}
	w := mrsync.NewWriter(fake)

	if _, err := w.UIDPresent(context.Background(), "Drafts", mrsync.UID(37)); !errors.Is(err, wantErr) {
		t.Errorf("UIDPresent() error = %v, want %v", err, wantErr)
	}
}

func TestWriterSupportsMoveDelegatesToCapability(t *testing.T) {
	fake := &fakeWriteSession{supports: map[mrsync.Capability]bool{mrsync.CapabilityMove: true}}
	w := mrsync.NewWriter(fake)
	if !w.SupportsMove() {
		t.Error("SupportsMove() = false, want true when CapabilityMove is supported")
	}

	fake2 := &fakeWriteSession{}
	w2 := mrsync.NewWriter(fake2)
	if w2.SupportsMove() {
		t.Error("SupportsMove() = true, want false when CapabilityMove is not supported")
	}
}
