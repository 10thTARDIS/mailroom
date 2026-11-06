// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// plaintext-report against a real database and blob store: the walk reads the
// right user's messages through RLS, classifies what is in the blobs, reports the
// --limit bound only when it cut the scan short, and writes nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

var ptTestMaster = []byte("mailroom-admin-plaintext-key!!!!")

func ptSeedMessage(t *testing.T, ctx context.Context, d *deps, super *pgxpool.Pool, userID ulid.ULID, dek, raw []byte) {
	t.Helper()
	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	h, err := blobs.Put(ctx, userID, dek, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	id := store.NewULID()
	if _, err := super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1,$2,$3,$3,$4) ON CONFLICT DO NOTHING`,
		userID.String(), h[:], len(raw), store.NewULID().String()); err != nil {
		t.Fatalf("seed blob row: %v", err)
	}
	if _, err := super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_name, from_address,
		    received_at, size_bytes, message_id_hint)
		 VALUES ($1,$2,$3,'s','Friend','friend@example.com', now(), $4, $5)`,
		id.String(), userID.String(), h[:], len(raw), "<"+id.String()+"@example.com>"); err != nil {
		t.Fatalf("seed message: %v", err)
	}
}

func TestPlaintextReportWalksOneUsersMailAndReportsItsBound(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	t.Setenv("MAILROOM_BLOB_ROOT", t.TempDir())
	d := &deps{log: quietDeleteUserLogger(), keys: fakeKeyProvider{key: ptTestMaster}, store: h.store, close: func() {}}

	userID := seedUntangleUser(t, h.super, "ptreport-"+store.NewULID().String())
	other := seedUntangleUser(t, h.super, "ptreport-other-"+store.NewULID().String())
	dekFor := func(u ulid.ULID) []byte {
		k, err := crypto.DeriveKey(ptTestMaster, u[:], syncrunner.MailBlobInfoV1)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	wrapped := ptWrap(ptProse, 72)
	for _, raw := range [][]byte{
		ptMsg("text/plain; format=flowed", wrapped),
		ptMsg("text/plain", wrapped+"\r\n\r\n"+wrapped),
		ptMsg("text/plain", "Thanks, see you at 3.\r\n\r\nAlice\r\n"),
		ptMsg("text/html", "<p>hi</p>"),
	} {
		ptSeedMessage(t, ctx, d, h.super, userID, dekFor(userID), raw)
	}
	// Another user's wrapped message: RLS must keep it out of this user's count.
	ptSeedMessage(t, ctx, d, h.super, other, dekFor(other), ptMsg("text/plain", wrapped))

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	scan := func(limit int) *ptAgg {
		a := newPtAgg()
		if err := ptScanUser(ctx, d, blobs, userID, dekFor(userID), limit, a); err != nil {
			t.Fatalf("scan(limit=%d): %v", limit, err)
		}
		return a
	}

	all := scan(0)
	if all.scanned != 4 || all.unreadable != 0 || all.notPlain != 1 || all.plain != 3 ||
		all.flowed != 1 || all.looksWrap != 1 || all.wouldChange != 1 || all.limited {
		t.Errorf("full scan = %+v, want 4 walked / 1 html / 3 plain / 1 flowed / 1 wrapped, not limited", all)
	}
	if got := strings.Count(wrapped, "\r\n") * 2; all.joins != got {
		t.Errorf("joins = %d, want %d", all.joins, got)
	}

	cut := scan(2)
	if cut.scanned != 2 || !cut.limited {
		t.Errorf("--limit 2 = scanned %d limited %v, want 2 and limited", cut.scanned, cut.limited)
	}
	exact := scan(4)
	if exact.scanned != 4 || exact.limited {
		t.Errorf("--limit 4 (exactly the mailbox) = scanned %d limited %v, want 4 and NOT limited", exact.scanned, exact.limited)
	}

	// And it wrote nothing: the user's message count is unchanged.
	var n int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.messages WHERE user_id = $1`, userID.String()).Scan(&n); err != nil || n != 4 {
		t.Errorf("messages after the report = %d (%v), want 4", n, err)
	}
}
