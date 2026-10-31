// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

import (
	"context"
	"testing"
	"time"
)

// #829: an archive row with no received time of its own falls back to the
// sender's Date header for received_at. That time is attacker-controlled, so it
// must not stamp a contact's mail dates: a Date in 2100 would otherwise pin
// last_seen_at there for good (GREATEST never comes back down).
func dateMsg(from, id, date string) []byte {
	return []byte("From: Eve <" + from + ">\r\nTo: bob@example.com\r\nSubject: " + id + "\r\n" +
		"Message-ID: <" + id + "@example.org>\r\nDate: " + date + "\r\n\r\nbody " + id + "\r\n")
}

func (f *fixture) seenDates(t *testing.T, addr string) (first, last time.Time) {
	t.Helper()
	if err := f.super.QueryRow(context.Background(), `
		SELECT first_seen_at, last_seen_at FROM app.contact_addresses
		 WHERE user_id = $1 AND address = $2`, f.userID.String(), addr).Scan(&first, &last); err != nil {
		t.Fatalf("read contact dates for %s: %v", addr, err)
	}
	return first.UTC(), last.UTC()
}

func TestArchiveDateHeaderFallbackDoesNotStampContactDates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const future = "Fri, 01 Jan 2100 00:00:00 +0000"
	trusted := time.Date(2020, 3, 3, 0, 0, 0, 0, time.UTC)

	// CONTROL: an archive WITH its own received time stamps the contact with it.
	if _, err := f.ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, dateMsg("ctl829@ext.example.org", "ctl", future),
		Archive{MailboxID: f.mailboxB, ReceivedAt: trusted}); err != nil {
		t.Fatalf("control ingest: %v", err)
	}
	if first, last := f.seenDates(t, "ctl829@ext.example.org"); !first.Equal(trusted) || !last.Equal(trusted) {
		t.Fatalf("CONTROL FAILED: dates %v / %v, want %v", first, last, trusted)
	}

	// A NEW contact from a future-dated, header-only message: now, not 2100.
	if _, err := f.ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, dateMsg("new829@ext.example.org", "n1", future),
		Archive{MailboxID: f.mailboxB}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	first, last := f.seenDates(t, "new829@ext.example.org")
	if time.Since(first) > time.Hour || time.Since(last) > time.Hour || last.After(time.Now().Add(time.Minute)) {
		t.Errorf("new contact stamped %v / %v from a Date header, want about now", first, last)
	}

	// An EXISTING contact keeps its dates when the only evidence is a header.
	if _, err := f.ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, dateMsg("ctl829@ext.example.org", "c2", future),
		Archive{MailboxID: f.mailboxB}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if first, last := f.seenDates(t, "ctl829@ext.example.org"); !first.Equal(trusted) || !last.Equal(trusted) {
		t.Errorf("existing contact moved to %v / %v by a header-only date, want %v", first, last, trusted)
	}

	// A future archive received time is clamped to now.
	if _, err := f.ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, dateMsg("ctl829@ext.example.org", "c3", future),
		Archive{MailboxID: f.mailboxB, ReceivedAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, last := f.seenDates(t, "ctl829@ext.example.org"); last.After(time.Now().Add(time.Minute)) {
		t.Errorf("a future received time pinned last_seen_at to %v", last)
	}
}
