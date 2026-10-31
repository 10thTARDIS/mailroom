// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for RefreshLiveProvenanceFlags (#523): the flags-only
// projection of provenance the CONDSTORE flag-reconcile pass uses instead of a
// full re-ingest. It must never INSERT and never change STATE — a live row's
// remote_flags moves and nothing else; anything else (no row, a
// 'stale_uidvalidity' row) reports found=false and touches nothing, leaving
// the caller to fall back to the full body path.
//
// Reuses reapFixture (reaper_integration_test.go): the mailboxes/provenance
// shape is identical, and RLS coverage needs the same "another user" fixture.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// remoteFlagsAt reads remote_flags for a provenance row directly, bypassing
// RLS — ground truth for assertions.
func (f *reapFixture) remoteFlagsAt(t *testing.T, mailbox ulid.ULID, uid, uidValidity int64) []string {
	t.Helper()
	var flags []string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT remote_flags FROM app.message_provenance
		  WHERE mailbox_id = $1 AND uidvalidity = $2 AND uid = $3`,
		mailbox.String(), uidValidity, uid).Scan(&flags); err != nil {
		t.Fatalf("remote flags: %v", err)
	}
	return flags
}

// stateAt reads a provenance row's state directly, bypassing RLS.
func (f *reapFixture) stateAt(t *testing.T, mailbox ulid.ULID, uid, uidValidity int64) string {
	t.Helper()
	var state string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT state FROM app.message_provenance
		  WHERE mailbox_id = $1 AND uidvalidity = $2 AND uid = $3`,
		mailbox.String(), uidValidity, uid).Scan(&state); err != nil {
		t.Fatalf("state: %v", err)
	}
	return state
}

func (f *reapFixture) refreshFlags(t *testing.T, userID, mailbox ulid.ULID, uid, uidValidity int64, flags []string) (ulid.ULID, bool) {
	t.Helper()
	var (
		id    ulid.ULID
		found bool
	)
	if err := f.st.UserTx(context.Background(), userID, func(tx *UserTx) error {
		var err error
		id, found, err = tx.RefreshLiveProvenanceFlags(context.Background(), mailbox, uidValidity, uid, flags)
		return err
	}); err != nil {
		t.Fatalf("refresh live provenance flags: %v", err)
	}
	return id, found
}

// (a) No row at all: never seen, or reaped. found=false, nothing written.
func TestRefreshLiveProvenanceFlagsNoRowFound(t *testing.T) {
	f := newReapFixture(t)

	if _, found := f.refreshFlags(t, f.userID, f.inbox, 1, 42, []string{`\Seen`}); found {
		t.Fatal("found a row that was never seeded")
	}
}

// (b) A 'stale_uidvalidity' row: retired by a UIDVALIDITY roll, invisible to
// every client. found=false — reviving it needs the full body path
// (UpsertProvenance) — and the row itself must not be touched.
func TestRefreshLiveProvenanceFlagsStaleRowNotFoundOrModified(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "retired generation")
	f.seedProvenance(t, msg, f.inbox, 3, 42, "stale_uidvalidity")
	before := f.remoteFlagsAt(t, f.inbox, 3, 42)

	if _, found := f.refreshFlags(t, f.userID, f.inbox, 3, 42, []string{`\Seen`, `\Flagged`}); found {
		t.Fatal("a stale_uidvalidity row was reported found — the caller would skip the body fallback that revives it")
	}
	if got := f.remoteFlagsAt(t, f.inbox, 3, 42); len(got) != len(before) {
		t.Errorf("stale row's remote_flags was modified: before=%v after=%v", before, got)
	}
	if got := f.stateAt(t, f.inbox, 3, 42); got != "stale_uidvalidity" {
		t.Errorf("stale row's state changed to %q", got)
	}
}

// (c) THE HEADLINE. A live row: found=true, remote_flags updates, state and
// everything else about the row is untouched, and the right message id comes
// back.
func TestRefreshLiveProvenanceFlagsUpdatesTheLiveRow(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "live message")
	f.seedProvenance(t, msg, f.inbox, 5, 42, "live")

	gotID, found := f.refreshFlags(t, f.userID, f.inbox, 5, 42, []string{`\Seen`, `\Flagged`})
	if !found {
		t.Fatal("live row not found")
	}
	if gotID != msg {
		t.Errorf("message id = %s, want %s", gotID, msg)
	}
	if got := f.remoteFlagsAt(t, f.inbox, 5, 42); len(got) != 2 {
		t.Fatalf("remote_flags = %v, want 2 flags", got)
	}
	if got := f.stateAt(t, f.inbox, 5, 42); got != "live" {
		t.Errorf("state changed to %q, want unchanged 'live'", got)
	}
}

// RLS. Another user's identity cannot see — and therefore cannot refresh — my
// provenance row, even naming the exact same coordinate.
func TestRefreshLiveProvenanceFlagsIsUserScoped(t *testing.T) {
	f := newReapFixture(t)
	msg := f.seedMessage(t, "mine")
	f.seedProvenance(t, msg, f.inbox, 1, 42, "live")

	if _, found := f.refreshFlags(t, f.otherID, f.inbox, 1, 42, []string{`\Seen`}); found {
		t.Fatal("another user's tx found and could refresh my provenance row")
	}
	if got := f.remoteFlagsAt(t, f.inbox, 1, 42); len(got) != 0 {
		t.Errorf("another user's refresh modified my row: %v", got)
	}
}
