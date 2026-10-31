// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Key history (#815, ADR-040 §11): a removed correspondent key is kept but used
// by nothing, every real change writes exactly one event (and its change row),
// a no-op writes neither, and a re-sighting of a removed key revives the same
// row. Real go-crypto keys throughout.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// histCounts is (events for the key, Contact change rows for the user).
func (f *contactKeyFixture) histCounts(t *testing.T, keyID ulid.ULID) (events, contactRows int) {
	t.Helper()
	ctx := context.Background()
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_key_events WHERE contact_key_id = $1`,
		keyID.String()).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Contact'`,
		f.userID.String()).Scan(&contactRows); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *contactKeyFixture) lastEvent(t *testing.T, keyID ulid.ULID) (kind, source string) {
	t.Helper()
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT kind, source FROM app.contact_key_events WHERE contact_key_id = $1 ORDER BY at DESC, id DESC LIMIT 1`,
		keyID.String()).Scan(&kind, &source); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *contactKeyFixture) tx(t *testing.T, fn func(tx *UserTx) error) {
	t.Helper()
	if err := f.st.UserTx(context.Background(), f.userID, fn); err != nil {
		t.Fatal(err)
	}
}

// TestContactKeyEventsOnePerRealChange walks a key through its life and checks,
// at every step, exactly one event and the documented change row for a real
// change, and none for a no-op.
func TestContactKeyEventsOnePerRealChange(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	const addr = "history@example.com"
	contact := f.seedContact(t, addr)
	now := time.Now().UTC().Truncate(time.Second)
	e := lcGenKey(t, addr, now.Add(-300*lcDay), 310*lcDay)
	old := lcPub(t, e)
	fpr := lcFpr(t, old)
	lcRenew(t, e, now.Add(-time.Hour), 3*365*lcDay)
	renewed := lcPub(t, e)

	upsert := func(pub []byte, src string) UpsertResult {
		t.Helper()
		var r UpsertResult
		f.tx(t, func(tx *UserTx) error {
			var err error
			r, err = tx.UpsertContactKeyDetail(ctx, ContactKeyRow{
				ContactID: contact, Kind: KeyPGP, Fingerprint: fpr, PublicKey: pub, Source: src, Authenticated: true,
			})
			return err
		})
		return r
	}
	type step struct {
		name               string
		do                 func()
		wantEvents, wantCh int
		wantKind           string
	}
	var keyID ulid.ULID
	first := upsert(old, ContactKeyAutocrypt)
	if !first.Created || first.ID == (ulid.ULID{}) {
		t.Fatalf("first sighting = %+v", first)
	}
	keyID = first.ID
	if ev, _ := f.histCounts(t, keyID); ev != 1 {
		t.Fatalf("first sighting wrote %d events, want 1", ev)
	}
	if k, s := f.lastEvent(t, keyID); k != KeyEventFirstSeen || s != ContactKeyAutocrypt {
		t.Errorf("first event = %s/%s", k, s)
	}

	steps := []step{
		{"re-sighting adds nothing", func() { upsert(old, ContactKeyAutocrypt) }, 0, 0, ""},
		{"renewal", func() {
			if r := upsert(renewed, ContactKeyWKD); !r.Merged || r.EventKind != KeyEventRenewed {
				t.Errorf("renewal = %+v", r)
			}
		}, 1, 1, KeyEventRenewed},
		{"renewal again (no-op)", func() { upsert(renewed, ContactKeyWKD) }, 0, 0, ""},
		{"trust", func() {
			f.tx(t, func(tx *UserTx) error { return tx.TrustContactKey(ctx, contact, addr, fpr) })
		}, 1, 1, KeyEventPinned},
		{"trust again (no-op)", func() {
			f.tx(t, func(tx *UserTx) error { return tx.TrustContactKey(ctx, contact, addr, fpr) })
		}, 0, 0, ""},
		{"refuse", func() {
			f.tx(t, func(tx *UserTx) error { return tx.RefuseContactKey(ctx, contact, fpr) })
		}, 1, 1, KeyEventRefused},
		{"refuse again (no-op)", func() {
			f.tx(t, func(tx *UserTx) error { return tx.RefuseContactKey(ctx, contact, fpr) })
		}, 0, 0, ""},
		{"remove", func() {
			f.tx(t, func(tx *UserTx) error {
				r, err := tx.RemoveContactKey(ctx, keyID)
				if err == nil && !r.Found {
					t.Error("remove: not found")
				}
				return err
			})
		}, 1, 1, KeyEventRemoved},
		{"remove again (no-op)", func() {
			f.tx(t, func(tx *UserTx) error {
				r, err := tx.RemoveContactKey(ctx, keyID)
				if err == nil && r.Found {
					t.Error("second remove reported Found")
				}
				return err
			})
		}, 0, 0, ""},
		{"trusting a removed key is refused", func() {
			if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
				return tx.TrustContactKey(ctx, contact, addr, fpr)
			}); err == nil {
				t.Error("trusting a removed key succeeded")
			}
		}, 0, 0, ""},
		{"re-sighting revives the same row", func() {
			r := upsert(renewed, ContactKeyWKD)
			if !r.Revived || r.ID != keyID || r.Created {
				t.Errorf("revival = %+v, want Revived on %s", r, keyID)
			}
		}, 1, 1, KeyEventFirstSeen},
	}
	for _, s := range steps {
		ev0, ch0 := f.histCounts(t, keyID)
		s.do()
		ev, ch := f.histCounts(t, keyID)
		if ev-ev0 != s.wantEvents || ch-ch0 != s.wantCh {
			t.Errorf("%s: %d events, %d Contact rows; want %d/%d", s.name, ev-ev0, ch-ch0, s.wantEvents, s.wantCh)
			continue
		}
		if s.wantKind != "" {
			if k, _ := f.lastEvent(t, keyID); k != s.wantKind {
				t.Errorf("%s: event kind %s, want %s", s.name, k, s.wantKind)
			}
		}
	}

	// The revived row is current again, unrefused, with the sighting's source.
	var keys []ContactKey
	f.tx(t, func(tx *UserTx) error {
		var err error
		keys, err = tx.ContactKeysFor(ctx, contact)
		return err
	})
	if len(keys) != 1 || keys[0].ID != keyID || keys[0].Refused || keys[0].Source != ContactKeyWKD || keys[0].RemovedAt != nil {
		t.Errorf("revived key = %+v", keys)
	}
	// History reads every event, oldest first.
	var hist []ContactKeyEvent
	f.tx(t, func(tx *UserTx) error {
		var err error
		var found bool
		hist, found, err = tx.ContactKeyHistory(ctx, keyID)
		if !found {
			t.Error("history: key not found")
		}
		return err
	})
	want := []string{KeyEventFirstSeen, KeyEventRenewed, KeyEventPinned, KeyEventRefused, KeyEventRemoved, KeyEventFirstSeen}
	if len(hist) != len(want) {
		t.Fatalf("history = %+v, want %v", hist, want)
	}
	for i, k := range want {
		if hist[i].Kind != k {
			t.Errorf("history[%d] = %s, want %s", i, hist[i].Kind, k)
		}
	}
	if hist[1].ExpiresAt == nil || !hist[1].ExpiresAt.After(now.Add(365*lcDay)) {
		t.Errorf("the renewed event records expiry %v, want the renewed date", hist[1].ExpiresAt)
	}
}

// TestRemovedContactKeyIsUsedByNothing: after a removal the row exists (soft
// delete), and every reader that picks keys is blind to it, while the history
// readers still see it.
func TestRemovedContactKeyIsUsedByNothing(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	const addr = "gone@example.com"
	contact := f.seedContact(t, addr)
	now := time.Now().UTC()
	pub := lcPub(t, lcGenKey(t, addr, now.Add(-lcDay), 365*lcDay))
	fpr := lcFpr(t, pub)
	var keyID ulid.ULID
	f.tx(t, func(tx *UserTx) error {
		r, err := tx.UpsertContactKeyDetail(ctx, ContactKeyRow{ContactID: contact, Fingerprint: fpr, PublicKey: pub, Source: ContactKeyWKD})
		keyID = r.ID
		return err
	})
	readers := func() (forOne, forMany, all, count, export, hasKey, byID int) {
		f.tx(t, func(tx *UserTx) error {
			a, err := tx.ContactKeysFor(ctx, contact)
			if err != nil {
				return err
			}
			m, err := tx.ContactKeysForMany(ctx, []ulid.ULID{contact})
			if err != nil {
				return err
			}
			al, err := tx.AllContactKeys(ctx)
			if err != nil {
				return err
			}
			c, err := tx.CountContactKeys(ctx)
			if err != nil {
				return err
			}
			ex, err := tx.ExportContactKeys(ctx)
			if err != nil {
				return err
			}
			yes := true
			ids, _, err := tx.QueryContacts(ctx, ContactQueryFilter{HasKey: &yes}, 10, true)
			if err != nil {
				return err
			}
			_, found, err := tx.ContactKeyByID(ctx, keyID)
			if err != nil {
				return err
			}
			if found {
				byID = 1
			}
			forOne, forMany, all, count, export, hasKey = len(a), len(m[contact]), len(al), c[ContactKeyWKD], len(ex[contact]), len(ids)
			return nil
		})
		return
	}
	// Positive control: every reader sees the held key.
	if a, b, c, d, e, g, h := readers(); a != 1 || b != 1 || c != 1 || d != 1 || e != 1 || g != 1 || h != 1 {
		t.Fatalf("control: readers see %d %d %d %d %d %d %d, want all 1", a, b, c, d, e, g, h)
	}
	f.tx(t, func(tx *UserTx) error { _, err := tx.RemoveContactKey(ctx, keyID); return err })
	if a, b, c, d, e, g, h := readers(); a+b+c+d+e+g+h != 0 {
		t.Errorf("after removal readers see %d %d %d %d %d %d %d, want all 0", a, b, c, d, e, g, h)
	}
	var rows int
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_keys WHERE id = $1 AND removed_at IS NOT NULL`, keyID.String()).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("the removed row is not kept (rows %d, err %v): the removal was a hard delete", rows, err)
	}
	f.tx(t, func(tx *UserTx) error {
		all, err := tx.AllContactKeysIncludingRemoved(ctx)
		if err != nil {
			return err
		}
		many, err := tx.ContactKeysForManyIncludingRemoved(ctx, []ulid.ULID{contact})
		if err != nil {
			return err
		}
		if len(all) != 1 || all[0].RemovedAt == nil || len(many[contact]) != 1 {
			t.Errorf("history readers: all=%+v many=%+v", all, many)
		}
		return nil
	})
}

// TestKeyRetiredAtRecordedNotInvented: retire records retired_at when a live
// key retires; a key retired before 00071 (written directly, the state the
// migration leaves) keeps NULL, which reads as "not recorded", and a later
// retire of it does not invent a date.
func TestKeyRetiredAtRecordedNotInvented(t *testing.T) {
	f := newContactKeyFixture(t)
	ctx := context.Background()
	ins := func(decryptOnly bool) ulid.ULID {
		id := NewULID()
		// Published too, so retiring it again is NOT a no-op (the UPDATE runs to
		// unpublish it), which is the path that could invent a date.
		if _, err := f.h.super.Exec(ctx, `INSERT INTO app.user_keys (id, user_id, kind, fingerprint, public_key, decrypt_only, wkd_published)
		    VALUES ($1, $2, 'pgp', $3, $4, $5, true)`, id.String(), f.userID.String(), "FP"+id.String(), []byte{1}, decryptOnly); err != nil {
			t.Fatal(err)
		}
		return id
	}
	legacy := ins(true) // retired before 00071: no date
	live := ins(false)
	before := time.Now().UTC().Add(-time.Second)
	f.tx(t, func(tx *UserTx) error {
		if _, err := tx.RetireUserKey(ctx, live, false); err != nil {
			return err
		}
		r, err := tx.RetireUserKey(ctx, legacy, false) // already retired; only unpublishes
		if err == nil && !r.Unpublished {
			t.Error("fixture: retiring the legacy key did not run its UPDATE, so the check below proves nothing")
		}
		return err
	})
	f.tx(t, func(tx *UserTx) error {
		l, _, err := tx.GetUserKey(ctx, legacy)
		if err != nil {
			return err
		}
		k, _, err := tx.GetUserKey(ctx, live)
		if err != nil {
			return err
		}
		if l.RetiredAt != nil {
			t.Errorf("a key retired before 00071 got retired_at %v: a date was invented", l.RetiredAt)
		}
		if k.RetiredAt == nil || k.RetiredAt.Before(before) {
			t.Errorf("retire did not record retired_at: %v", k.RetiredAt)
		}
		return nil
	})
}
