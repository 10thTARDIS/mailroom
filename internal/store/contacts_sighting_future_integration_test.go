// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// A future received time (a forged date that reached received_at) is clamped to
// now: GREATEST would otherwise pin last_seen_at there forever. An untrusted
// date moves nothing on an existing address and is now() on a new one.
func TestContactSightingFutureAndUntrustedDates(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	future := time.Now().UTC().AddDate(5, 0, 0)

	addr := NormalizeAddress("fut-" + NewULID().String() + "@ext.com")
	d1 := sightingDay(2021, 4, 4)
	ingestSighting(t, s, aID, addr, d1, false)
	ingestSighting(t, s, aID, addr, future, false)
	_, last, _ := storedSeen(t, h, a.ID, addr)
	if last.After(time.Now().Add(time.Minute)) {
		t.Errorf("future received_at pinned last_seen_at to %v", last)
	}

	nw := NormalizeAddress("futnew-" + NewULID().String() + "@ext.com")
	ingestSighting(t, s, aID, nw, future, false)
	f1, l1, _ := storedSeen(t, h, a.ID, nw)
	if f1.After(time.Now().Add(time.Minute)) || l1.After(time.Now().Add(time.Minute)) {
		t.Errorf("new contact from a future date stamped %v / %v", f1, l1)
	}

	un := NormalizeAddress("untr-" + NewULID().String() + "@ext.com")
	ingestSighting(t, s, aID, un, d1, false)
	mustUpsert(t, s, aID, ContactSourceMessage{FromAddress: un, ReceivedAt: sightingDay(2010, 1, 1), DateUntrusted: true})
	uf, ul, _ := storedSeen(t, h, a.ID, un)
	wantTime(t, "untrusted first_seen_at", uf, d1)
	wantTime(t, "untrusted last_seen_at", ul, d1)
	un2 := NormalizeAddress("untr2-" + NewULID().String() + "@ext.com")
	mustUpsert(t, s, aID, ContactSourceMessage{FromAddress: un2, ReceivedAt: sightingDay(2010, 1, 1), DateUntrusted: true})
	if f2, _, _ := storedSeen(t, h, a.ID, un2); time.Since(f2) > time.Hour {
		t.Errorf("new contact from an untrusted date stamped %v, want about now", f2)
	}
}

// The report (and so the backfill, which plans from it) ignores messages dated
// more than a day ahead.
func TestContactDatesReportIgnoresFutureMessages(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	addr := NormalizeAddress("rfut-" + NewULID().String() + "@ext.com")
	d1 := sightingDay(2021, 4, 4)
	ingestSighting(t, s, aID, addr, d1, false)
	ctx := context.Background()
	// A forged future-dated message row (the sighting is clamped; the row is not).
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		hash := sha256.Sum256([]byte(NewULID().String()))
		if err := tx.InsertBlobMeta(ctx, BlobMeta{ContentHash: hash[:], SizePlain: 10, SizeStored: 10, DEKID: NewULID(), EnvelopeVer: 1}); err != nil {
			return err
		}
		tid := NewULID()
		if _, err := tx.tx.Exec(ctx, `INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, 'x')`, tid.String(), tx.userID.String()); err != nil {
			return err
		}
		return tx.InsertMessage(ctx, MessageRow{ID: NewULID(), ContentHash: hash[:], ThreadID: tid, Subject: "x",
			FromAddress: addr, ReceivedAt: time.Now().UTC().AddDate(5, 0, 0), SizeBytes: 10,
			ReferenceIDs: []string{}, Flags: MessageFlags{Keywords: []string{}}})
	}); err != nil {
		t.Fatalf("insert future message: %v", err)
	}
	var rows []ContactMailDate
	var refs []ContactSentRef
	if err := s.UserTx(ctx, aID, func(tx *UserTx) (err error) {
		if rows, err = tx.ContactMailDates(ctx); err != nil {
			return err
		}
		refs, err = tx.ContactSentRefsAfter(ctx, ulid.ULID{}, 1000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Address != addr {
			continue
		}
		found = true
		if r.FromCount != 1 || r.FromLast == nil || !r.FromLast.Equal(d1) {
			t.Errorf("report counted the future message: n=%d last=%v", r.FromCount, r.FromLast)
		}
	}
	if !found {
		t.Fatal("no report row for the address")
	}
	for _, r := range refs {
		if r.FromAddress == addr && r.ReceivedAt.After(time.Now().Add(48*time.Hour)) {
			t.Errorf("sent-ref walk returned a future message %v", r.ReceivedAt)
		}
	}
}
