// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// #766: a message imported from an ARCHIVE has a received_at that is the
// archive's guess or, with none, the sender's own Date header. Judging a
// date-bounded authserv-id entry against it would let a forger back-date their
// message into the bound. IngestArchive therefore hands the trust check the zero
// time; a live message with the same bytes and a provider INTERNALDATE inside
// the bound is the control.
func calInviteMessage(uid string) []byte {
	return []byte("From: Carol <carol@example.org>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Invite " + uid + "\r\n" +
		"Message-ID: <" + uid + "@example.org>\r\n" +
		// Written by the sender: a forged header bearing the OLD id, and a Date
		// far inside the bound.
		"Authentication-Results: old.provider.example; dmarc=pass header.from=example.org\r\n" +
		"Date: Mon, 12 Jan 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/calendar; method=REQUEST; charset=utf-8\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//y//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nSEQUENCE:1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20261021T090000Z\r\nDTEND:20261021T100000Z\r\nSUMMARY:Review\r\n" +
		"ORGANIZER:mailto:carol@example.org\r\nATTENDEE:mailto:bob@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n")
}

func TestArchiveIngestNeverTrustsABoundedIDOnItsDateHeader(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The bound is in 2099, so any real received time is inside it.
	ing := New(f.st, f.ing.blobs, WithTrustedAuthservIDs([]string{"old.provider.example<2099-01-01"}))

	live, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, calInviteMessage("live-1"), f.prov(f.mailboxA, 801, nil))
	if err != nil {
		t.Fatalf("ingest live: %v", err)
	}
	// CONTROL: a provider-dated message inside the bound IS verified, so the
	// zero below is the archive path's doing and not a fixture that never verifies.
	if n := f.scalar(t, `SELECT count(*) FROM app.calendar_invites WHERE message_id=$1 AND verified`, live.MessageID.String()); n != 1 {
		t.Fatalf("CONTROL FAILED: live message inside the bound verified=%d, want 1", n)
	}

	// Archive row: ReceivedAt unset, so received_at falls back to the Date header.
	arc, err := ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, calInviteMessage("arc-1"), Archive{MailboxID: f.mailboxB})
	if err != nil {
		t.Fatalf("ingest archive: %v", err)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.calendar_invites WHERE message_id=$1`, arc.MessageID.String()); n != 1 {
		t.Fatalf("archive invite not indexed (%d), so the assertion below proves nothing", n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.calendar_invites WHERE message_id=$1 AND verified`, arc.MessageID.String()); n != 0 {
		t.Errorf("an archive-imported message was verified on a bounded id (%d) judged by a sender-controlled time", n)
	}
	// Same, with an archive delivery date that is inside the bound.
	arc2, err := ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, calInviteMessage("arc-2"),
		Archive{MailboxID: f.mailboxB, ReceivedAt: time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("ingest archive 2: %v", err)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.calendar_invites WHERE message_id=$1 AND verified`, arc2.MessageID.String()); n != 0 {
		t.Errorf("an archive row with an archive date was verified on a bounded id (%d)", n)
	}
}

// The archive-first case: the same bytes are imported from an archive (forged
// bounded-id header, Date inside the bound) and THEN seen over IMAP, which adds
// a provenance row. The row keeps local_mailbox_id and its archive-era
// received_at, so a provenance row alone would wrongly mark it provider-dated.
// It must stay untrusted for a bounded entry.
func TestArchiveFirstThenSyncedMessageIsNotProviderDated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ing := New(f.st, f.ing.blobs, WithTrustedAuthservIDs([]string{"old.provider.example<2099-01-01"}))
	raw := calInviteMessage("arc-first-1")

	arc, err := ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, raw,
		Archive{MailboxID: f.mailboxB, ReceivedAt: time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)})
	if err != nil || !arc.Created {
		t.Fatalf("archive ingest: %+v %v", arc, err)
	}
	// The identical bytes arrive over IMAP: a dedup hit that attaches provenance.
	live, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 811, nil))
	if err != nil {
		t.Fatalf("ingest over IMAP: %v", err)
	}
	if live.MessageID != arc.MessageID {
		t.Fatalf("setup: the IMAP copy did not dedup onto the archive row (%s vs %s)", live.MessageID, arc.MessageID)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.message_provenance WHERE message_id=$1`, arc.MessageID.String()); n == 0 {
		t.Fatal("setup: no provenance row attached, so this proves nothing")
	}
	// CONTROL: a message that only ever came over IMAP is provider-dated.
	pure, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, calInviteMessage("pure-imap-1"), f.prov(f.mailboxA, 812, nil))
	if err != nil {
		t.Fatalf("ingest pure: %v", err)
	}
	var archiveFirst, pureIMAP bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		m, e := tx.ProviderDated(ctx, []ulid.ULID{arc.MessageID, pure.MessageID})
		archiveFirst, pureIMAP = m[arc.MessageID], m[pure.MessageID]
		return e
	}); err != nil {
		t.Fatalf("ProviderDated: %v", err)
	}
	if !pureIMAP {
		t.Fatal("CONTROL FAILED: an IMAP-only message is not provider-dated")
	}
	if archiveFirst {
		t.Error("an archive-first message later synced over IMAP is provider-dated; its archive-era received_at would be judged against a bounded entry")
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.calendar_invites WHERE message_id=$1 AND verified`, arc.MessageID.String()); n != 0 {
		t.Errorf("the archive-first invite is verified (%d)", n)
	}
}
