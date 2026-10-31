// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// A contact's first_seen_at / last_seen_at are stamped from the message's
// received_at, not from the moment of ingest (#829). Each test would fail if the
// store went back to now(): the fixture dates are years in the past.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func sightingDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// ingestSighting inserts a real message row (so the report's true dates see it)
// and records its correspondents with the same received_at, in one UserTx as
// ingest does.
func ingestSighting(t *testing.T, s *Store, userID ulid.ULID, from string, at time.Time, draft bool) {
	t.Helper()
	ctx := context.Background()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		hash := sha256.Sum256([]byte(NewULID().String()))
		if err := tx.InsertBlobMeta(ctx, BlobMeta{
			ContentHash: hash[:], SizePlain: 10, SizeStored: 10, DEKID: NewULID(), EnvelopeVer: 1,
		}); err != nil {
			return err
		}
		threadID := NewULID()
		if _, err := tx.tx.Exec(ctx, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
			threadID.String(), tx.userID.String(), "sighting"); err != nil {
			return err
		}
		if err := tx.InsertMessage(ctx, MessageRow{
			ID: NewULID(), ContentHash: hash[:], ThreadID: threadID,
			Subject: "sighting", FromAddress: from, ReceivedAt: at, SizeBytes: 10,
			ReferenceIDs: []string{},
			Flags:        MessageFlags{Draft: draft, Keywords: []string{}},
		}); err != nil {
			return err
		}
		return tx.UpsertContactsFromMessage(ctx, ContactSourceMessage{
			FromAddress: from, ReceivedAt: at, Draft: draft,
		})
	}); err != nil {
		t.Fatalf("ingest sighting: %v", err)
	}
}

func storedSeen(t *testing.T, h *harness, userID, addr string) (first, last time.Time, n int) {
	t.Helper()
	if err := h.super.QueryRow(context.Background(), `
		SELECT first_seen_at, last_seen_at, seen_count FROM app.contact_addresses
		 WHERE user_id = $1 AND address = $2`, userID, addr).Scan(&first, &last, &n); err != nil {
		t.Fatalf("read sighting dates: %v", err)
	}
	return first.UTC(), last.UTC(), n
}

func wantTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// A newer message then an older one: first_seen moves earlier, last_seen does
// not regress. Also a new contact from an old message gets that message's date.
func TestContactSightingOutOfOrder(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	addr := NormalizeAddress("ooo-" + NewULID().String() + "@ext.com")

	newer, older := sightingDay(2023, 6, 1), sightingDay(2021, 3, 1)

	ingestSighting(t, s, aID, addr, newer, false)
	first, last, _ := storedSeen(t, h, a.ID, addr)
	// Brand-new contact from an old message: that message's date, not now.
	wantTime(t, "new contact first_seen_at", first, newer)
	wantTime(t, "new contact last_seen_at", last, newer)

	ingestSighting(t, s, aID, addr, older, false)
	first, last, n := storedSeen(t, h, a.ID, addr)
	wantTime(t, "first_seen_at after older message", first, older)
	wantTime(t, "last_seen_at after older message", last, newer)
	if n != 2 {
		t.Errorf("seen_count = %d, want 2", n)
	}
}

// A bulk import of old mail, in arbitrary order, leaves last_seen at the newest
// message and first_seen at the oldest. The stored dates agree with the report's
// true dates (ContactMailDates) within a day.
func TestContactSightingBulkImportAgreesWithReport(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	addr := NormalizeAddress("bulk-" + NewULID().String() + "@ext.com")

	for _, d := range []time.Time{
		sightingDay(2019, 5, 5), sightingDay(2022, 1, 9), sightingDay(2018, 2, 3),
		sightingDay(2020, 12, 31), sightingDay(2022, 8, 8), sightingDay(2019, 1, 1),
	} {
		ingestSighting(t, s, aID, addr, d, false)
	}
	first, last, _ := storedSeen(t, h, a.ID, addr)
	wantTime(t, "first_seen_at", first, sightingDay(2018, 2, 3))
	wantTime(t, "last_seen_at", last, sightingDay(2022, 8, 8))

	var rows []ContactMailDate
	if err := s.UserTx(context.Background(), aID, func(tx *UserTx) (err error) {
		rows, err = tx.ContactMailDates(context.Background())
		return err
	}); err != nil {
		t.Fatalf("ContactMailDates: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Address != addr {
			continue
		}
		found = true
		if r.FromFirst == nil || r.FromLast == nil || r.FromCount != 6 {
			t.Fatalf("report saw %d messages, first=%v last=%v; want 6 with both dates", r.FromCount, r.FromFirst, r.FromLast)
		}
		if d := r.FirstSeenAt.Sub(*r.FromFirst).Abs(); d > 24*time.Hour {
			t.Errorf("stored first vs true first differ by %v", d)
		}
		if d := r.LastSeenAt.Sub(*r.FromLast).Abs(); d > 24*time.Hour {
			t.Errorf("stored last vs true last differ by %v", d)
		}
	}
	if !found {
		t.Fatal("report returned no row for the address")
	}
}

// Drafts are excluded from the report and the backfill, so a draft must not move
// an existing address's dates; it still counts as a sighting.
func TestContactSightingDraftDoesNotMoveDates(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	addr := NormalizeAddress("draft-" + NewULID().String() + "@ext.com")

	d1 := sightingDay(2021, 4, 4)
	ingestSighting(t, s, aID, addr, d1, false)
	ingestSighting(t, s, aID, addr, sightingDay(2015, 1, 1), true)
	ingestSighting(t, s, aID, addr, sightingDay(2030, 1, 1), true)
	first, last, n := storedSeen(t, h, a.ID, addr)
	wantTime(t, "first_seen_at", first, d1)
	wantTime(t, "last_seen_at", last, d1)
	if n != 3 {
		t.Errorf("seen_count = %d, want 3 (drafts still count)", n)
	}
}

// A sighting with no message behind it (a key import) keeps the old behaviour:
// stamped now.
func TestContactSightingWithoutMessageIsNow(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	addr := NormalizeAddress("nomsg-" + NewULID().String() + "@ext.com")

	mustUpsert(t, s, aID, ContactSourceMessage{FromAddress: addr})
	first, last, _ := storedSeen(t, h, a.ID, addr)
	if time.Since(first) > time.Hour || time.Since(last) > time.Hour {
		t.Errorf("no-message sighting stamped %v / %v, want about now", first, last)
	}
}
