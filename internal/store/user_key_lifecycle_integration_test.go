// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The user's own keys after creation, against the real store under RLS (#795,
// ADR-040): retire, extend, revoke, and what WKD serves afterwards. Key helpers
// (lcGenKey, lcPub, lcFpr, lcRenew) live in contact_key_merge_integration_test.go.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"
)

// lcInsertKey stores pub as one of the user's keys, bound to identityID, and
// returns its id.
func lcInsertKey(t *testing.T, s *Store, userID, identityID ulid.ULID, pub []byte, bind bool) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if _, err := tx.InsertUserKey(ctx, UserKey{
			ID: id, UserID: userID, Kind: KeyPGP, Fingerprint: lcFpr(t, pub),
			PublicKey: pub, PrivateKeyCT: []byte{0x5E, 0xA1},
		}); err != nil {
			return err
		}
		if !bind {
			// Point identity_id at the identity WITHOUT making it the default:
			// what an older, rotated-out key looks like.
			_, err := tx.tx.Exec(ctx, `UPDATE app.user_keys SET identity_id = $3 WHERE user_id = $1 AND id = $2`,
				userID.String(), id.String(), identityID.String())
			return err
		}
		_, err := tx.BindKeyToIdentity(ctx, id, identityID)
		return err
	}); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	return id
}

func lcPublish(t *testing.T, h *harness, s *Store, userID, identityID, keyID ulid.ULID) (domain, hash string) {
	t.Helper()
	ctx := context.Background()
	domain, hash = publishedAs(t, h, identityID.String())
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		ok, err := tx.PublishKey(ctx, keyID, hash)
		if err == nil && !ok {
			t.Fatal("publish: key not found")
		}
		return err
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return domain, hash
}

func TestRetireRefusesTheBoundDefaultUnlessUnbinding(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	userID, identityID := ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID)

	keyID := lcInsertKey(t, s, userID, identityID, lcPub(t, lcGenKey(t, "x@example.com", time.Now(), 0)), true)
	domain, hash := lcPublish(t, h, s, userID, identityID, keyID)
	keysBefore, idsBefore := countChanges(t, h, a.ID, "Key"), countChanges(t, h, a.ID, "Identity")

	err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, err := tx.RetireUserKey(ctx, keyID, false)
		return err
	})
	if !errors.Is(err, ErrKeyIsDefault) {
		t.Fatalf("retiring the bound default without --unbind: err = %v, want ErrKeyIsDefault", err)
	}
	if countChanges(t, h, a.ID, "Key") != keysBefore || countChanges(t, h, a.ID, "Identity") != idsBefore {
		t.Error("a refused retire wrote change_log rows")
	}

	var res RetireResult
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		res, e = tx.RetireUserKey(ctx, keyID, true)
		return e
	}); err != nil {
		t.Fatalf("retire --unbind: %v", err)
	}
	if !res.Found || !res.Changed || !res.Unpublished || len(res.Unbound) != 1 || res.Unbound[0] != identityID {
		t.Errorf("retire result = %+v", res)
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		k, _, err := tx.GetUserKey(ctx, keyID)
		if err != nil {
			return err
		}
		if !k.DecryptOnly || k.WKDPublished {
			t.Errorf("after retire: decrypt_only=%v wkd_published=%v", k.DecryptOnly, k.WKDPublished)
		}
		if _, found, err := tx.SigningKeyForIdentity(ctx, identityID); err != nil || found {
			t.Errorf("the identity still has a signing key after retire --unbind (found=%v err=%v)", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LookupWKDKey(ctx, domain, hash); err != nil || found {
		t.Errorf("a retired key is still served over WKD (found=%v err=%v)", found, err)
	}
	if got := countChanges(t, h, a.ID, "Key") - keysBefore; got != 1 {
		t.Errorf("Key change rows = %d, want 1", got)
	}
	if got := countChanges(t, h, a.ID, "Identity") - idsBefore; got != 1 {
		t.Errorf("Identity change rows = %d, want 1 (default cleared and WKD hash cleared are one identity)", got)
	}

	// Retiring again is a no-op and writes nothing.
	keysAfter := countChanges(t, h, a.ID, "Key")
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		res, e = tx.RetireUserKey(ctx, keyID, false)
		return e
	}); err != nil {
		t.Fatalf("second retire: %v", err)
	}
	if res.Changed || countChanges(t, h, a.ID, "Key") != keysAfter {
		t.Errorf("a second retire changed something: %+v", res)
	}
}

// TestRetiringTheOldKeyKeepsTheNewOnePublished is the rotation case: publish
// the new key, then retire the old. The identity's WKD hash must survive, or
// the new key silently leaves WKD with the old one.
func TestRetiringTheOldKeyKeepsTheNewOnePublished(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	userID, identityID := ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID)

	oldKey := lcInsertKey(t, s, userID, identityID, lcPub(t, lcGenKey(t, "x@example.com", time.Now().Add(-lcDay), 0)), true)
	lcPublish(t, h, s, userID, identityID, oldKey)
	newPub := lcPub(t, lcGenKey(t, "x@example.com", time.Now(), 0))
	newKey := lcInsertKey(t, s, userID, identityID, newPub, true) // rebinding moves the default
	domain, hash := lcPublish(t, h, s, userID, identityID, newKey)

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, err := tx.RetireUserKey(ctx, oldKey, false)
		return err
	}); err != nil {
		t.Fatalf("retire old: %v", err)
	}
	got, found, err := s.LookupWKDKey(ctx, domain, hash)
	if err != nil || !found {
		t.Fatalf("after retiring the OLD key the NEW one is no longer served (found=%v err=%v)", found, err)
	}
	if !bytes.Equal(got.PublicKey, newPub) {
		t.Error("WKD serves something other than exactly the new key")
	}

	// UnpublishKey follows the same rule.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, err := tx.UnpublishKey(ctx, oldKey)
		return err
	}); err != nil {
		t.Fatalf("unpublish old: %v", err)
	}
	if _, found, _ := s.LookupWKDKey(ctx, domain, hash); !found {
		t.Error("unpublishing the OLD key took the NEW one out of WKD")
	}
}

func TestWKDDoesNotServeAnExpiredKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	userID, identityID := ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID)

	expired := lcPub(t, lcGenKey(t, "x@example.com", time.Now().Add(-40*lcDay), 30*lcDay))
	keyID := lcInsertKey(t, s, userID, identityID, expired, true)
	domain, hash := lcPublish(t, h, s, userID, identityID, keyID)
	if _, found, err := s.LookupWKDKey(ctx, domain, hash); err != nil || found {
		t.Errorf("an expired key is served over WKD (found=%v err=%v)", found, err)
	}

	// Positive control on the same address: a live key next to it IS served.
	live := lcPub(t, lcGenKey(t, "x@example.com", time.Now(), 0))
	liveID := lcInsertKey(t, s, userID, identityID, live, true)
	lcPublish(t, h, s, userID, identityID, liveID)
	got, found, err := s.LookupWKDKey(ctx, domain, hash)
	if err != nil || !found || !bytes.Equal(got.PublicKey, live) {
		t.Errorf("the live key is not served exactly (found=%v err=%v)", found, err)
	}
}

func TestRevokedKeyIsServedAsARevocationAfterTheCurrentKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	userID, identityID := ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID)

	e := lcGenKey(t, "x@example.com", time.Now().Add(-lcDay), 0)
	pub := lcPub(t, e)
	keyID := lcInsertKey(t, s, userID, identityID, pub, true)
	domain, hash := lcPublish(t, h, s, userID, identityID, keyID)

	if err := e.RevokeKey(packet.KeyCompromised, "", nil); err != nil {
		t.Fatal(err)
	}
	revoked := lcPub(t, e)
	keysBefore, idsBefore := countChanges(t, h, a.ID, "Key"), countChanges(t, h, a.ID, "Identity")
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, err := tx.RevokeUserKey(ctx, keyID, KeyMaterial{
			Fingerprint: lcFpr(t, pub), PublicKey: revoked, PrivateKeyCT: []byte{0x5E, 0xA2},
		}, time.Now())
		if err == nil && !found {
			t.Fatal("revoke: not found")
		}
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if countChanges(t, h, a.ID, "Key")-keysBefore != 1 || countChanges(t, h, a.ID, "Identity")-idsBefore != 1 {
		t.Error("revoking the bound default must write one Key and one Identity change row")
	}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		k, _, err := tx.GetUserKey(ctx, keyID)
		if err != nil {
			return err
		}
		if k.RevokedAt == nil || !k.DecryptOnly || !bytes.Equal(k.PublicKey, revoked) {
			t.Errorf("after revoke: revoked_at=%v decrypt_only=%v stored-revoked-bytes=%v",
				k.RevokedAt, k.DecryptOnly, bytes.Equal(k.PublicKey, revoked))
		}
		// A second revoke finds no unrevoked key.
		found, err := tx.RevokeUserKey(ctx, keyID, KeyMaterial{
			Fingerprint: lcFpr(t, pub), PublicKey: revoked, PrivateKeyCT: []byte{1},
		}, time.Now())
		if err != nil || found {
			t.Errorf("revoking twice: found=%v err=%v", found, err)
		}
		// And a revoked key cannot be "extended".
		found, err = tx.ReplaceUserKeyMaterial(ctx, keyID, KeyMaterial{
			Fingerprint: lcFpr(t, pub), PublicKey: pub, PrivateKeyCT: []byte{1},
		})
		if err != nil || found {
			t.Errorf("replacing a revoked key's material: found=%v err=%v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.LookupWKDKey(ctx, domain, hash)
	if err != nil || !found || !got.RevocationsOnly || !bytes.Equal(got.PublicKey, revoked) {
		t.Fatalf("the revoked key is not served as a revocation (found=%v err=%v revOnly=%v)",
			found, err, got.RevocationsOnly)
	}

	// Publish a replacement: the replacement comes first, the revocation after.
	live := lcPub(t, lcGenKey(t, "x@example.com", time.Now(), 0))
	liveID := lcInsertKey(t, s, userID, identityID, live, true)
	lcPublish(t, h, s, userID, identityID, liveID)
	got, found, err = s.LookupWKDKey(ctx, domain, hash)
	if err != nil || !found || got.RevocationsOnly {
		t.Fatalf("lookup with a replacement: found=%v err=%v revOnly=%v", found, err, got.RevocationsOnly)
	}
	want := append(append([]byte{}, live...), revoked...)
	if !bytes.Equal(got.PublicKey, want) {
		t.Error("WKD does not serve exactly the live key followed by the revocation")
	}
	ring, err := openpgp.ReadKeyRing(bytes.NewReader(got.PublicKey))
	if err != nil || len(ring) != 2 {
		t.Errorf("the served blob is not two readable keys (n=%d err=%v)", len(ring), err)
	}
}

func TestReplaceUserKeyMaterialRefusesAnotherFingerprint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	userID, identityID := ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID)

	e := lcGenKey(t, "x@example.com", time.Now().Add(-lcDay), 30*lcDay)
	pub := lcPub(t, e)
	keyID := lcInsertKey(t, s, userID, identityID, pub, true)
	lcRenew(t, e, time.Now(), 400*lcDay)
	renewed := lcPub(t, e)
	other := lcPub(t, lcGenKey(t, "x@example.com", time.Now(), 0))
	before := countChanges(t, h, a.ID, "Key")

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, err := tx.ReplaceUserKeyMaterial(ctx, keyID, KeyMaterial{
			Fingerprint: lcFpr(t, other), PublicKey: other, PrivateKeyCT: []byte{1},
		})
		if err != nil || found {
			t.Errorf("a different fingerprint was accepted (found=%v err=%v)", found, err)
		}
		// The right fingerprint NAMED, a different key HANDED OVER: refused on
		// the bytes, not only on the row.
		found, err = tx.ReplaceUserKeyMaterial(ctx, keyID, KeyMaterial{
			Fingerprint: lcFpr(t, pub), PublicKey: other, PrivateKeyCT: []byte{1},
		})
		if !errors.Is(err, ErrKeyInvalid) || found {
			t.Errorf("a different key under the right fingerprint was accepted (found=%v err=%v)", found, err)
		}
		found, err = tx.ReplaceUserKeyMaterial(ctx, keyID, KeyMaterial{
			Fingerprint: lcFpr(t, pub), PublicKey: renewed, PrivateKeyCT: []byte{2},
		})
		if err != nil || !found {
			t.Fatalf("the renewal was not stored (found=%v err=%v)", found, err)
		}
		k, _, err := tx.GetUserKey(ctx, keyID)
		if err != nil {
			return err
		}
		if !bytes.Equal(k.PublicKey, renewed) || !bytes.Equal(k.PrivateKeyCT, []byte{2}) {
			t.Error("the stored key material is not the renewal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := countChanges(t, h, a.ID, "Key") - before; got != 1 {
		t.Errorf("Key change rows for one extend (and one refused write) = %d, want 1", got)
	}
}
