// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Key lifecycle in the crypto process (#795, ADR-040): the 2-year default,
// extend, revoke. Driven through the real daemon on a real socket, like the
// boundary tests. Negative controls run on the way in are recorded in the PR.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/pgpwire"
)

const lcDay = 24 * time.Hour

func intp(n int) *int { return &n }

func assessStored(t *testing.T, h *harness, keyID string, at time.Time) pgpkeys.Validity {
	t.Helper()
	rec, err := h.vault.Get(context.Background(), keyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	v, err := pgpkeys.Assess(rec.PublicKey, at)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return v
}

func secondEq(a, b time.Time) bool { return a.Truncate(time.Second).Equal(b.Truncate(time.Second)) }

// TestGenerateDefaultsToTwoYears: primary AND encryption subkey expire two years
// after creation, unless the caller says otherwise.
func TestGenerateDefaultsToTwoYears(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "two@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := gen.Key.CreatedAt.Add(pgpwire.DefaultKeyLifetimeDays * lcDay)
	v := assessStored(t, h, gen.Key.KeyID, time.Now())
	if v.PrimaryExpires == nil || !secondEq(*v.PrimaryExpires, want) {
		t.Errorf("primary key expires %v, want %v (two years from creation)", v.PrimaryExpires, want)
	}
	if v.EncryptionExpires == nil || !secondEq(*v.EncryptionExpires, want) {
		t.Errorf("encryption subkey expires %v, want %v", v.EncryptionExpires, want)
	}
	if gen.Key.UsableUntil == nil || !secondEq(*gen.Key.UsableUntil, want) {
		t.Errorf("response usable_until = %v, want %v", gen.Key.UsableUntil, want)
	}

	never, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "never@example.com", ExpiresDays: intp(0)})
	if err != nil {
		t.Fatalf("Generate never: %v", err)
	}
	if never.Key.UsableUntil != nil {
		t.Errorf("expires_days 0 still set an expiry: %v", never.Key.UsableUntil)
	}
	if _, nev := assessStored(t, h, never.Key.KeyID, time.Now()).UsableUntil(); !nev {
		t.Error("expires_days 0 stored a key that expires")
	}

	for _, bad := range []int{-1, pgpwire.MaxKeyLifetimeDays + 1} {
		_, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "bad@example.com", ExpiresDays: intp(bad)})
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
			t.Errorf("expires_days %d: err = %v, want invalid_request", bad, err)
		}
	}
}

// TestExtendRenewsWithoutChangingTheKey: a key about to expire is extended; it
// keeps its fingerprint, the stored public key says the new date, the SEALED
// copy says it too (the daemon can still sign after the old date), and a
// correspondent merging the new public key into the old one gets the extension.
func TestExtendRenewsWithoutChangingTheKey(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "ext@example.com", ExpiresDays: intp(10)})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	oldRec, _ := h.vault.Get(ctx, gen.Key.KeyID)
	oldPub := append([]byte(nil), oldRec.PublicKey...)

	before := time.Now()
	ext, err := h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: 365})
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if ext.Key.Fingerprint != gen.Key.Fingerprint {
		t.Fatalf("fingerprint changed: %s -> %s", gen.Key.Fingerprint, ext.Key.Fingerprint)
	}
	if ext.Key.UsableUntil == nil || ext.Key.UsableUntil.Before(before.Add(364*lcDay)) {
		t.Fatalf("usable_until after extend = %v, want about a year out", ext.Key.UsableUntil)
	}

	// After the OLD expiry, the stored copy still works.
	later := time.Now().Add(30 * lcDay)
	if v := assessStored(t, h, gen.Key.KeyID, later); !v.Usable(later) {
		t.Error("the stored public key still stops at the old expiry")
	}
	h.svc.now = func() time.Time { return later }
	if _, err := h.client.Sign(ctx, pgpwire.SignRequest{UserID: user, KeyID: gen.Key.KeyID, Data: []byte("after the old date\r\n")}); err != nil {
		t.Errorf("signing after the old expiry fails — the sealed copy was not renewed: %v", err)
	}
	h.svc.now = time.Now

	// The production producer feeding the merger: a correspondent holding the
	// old copy merges the extended one.
	newRec, _ := h.vault.Get(ctx, gen.Key.KeyID)
	merged, changed, err := pgpkeys.Merge(oldPub, newRec.PublicKey)
	if err != nil || !changed {
		t.Fatalf("Merge(old, extended): changed=%v err=%v", changed, err)
	}
	if v, _ := pgpkeys.Assess(merged, later); !v.Usable(later) {
		t.Error("a correspondent merging the extended key does not get the extension")
	}
}

func TestExtendSetsAnExpiryOnANeverExpiringKey(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "nev@example.com", ExpiresDays: intp(0)})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: 30})
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if ext.Key.UsableUntil == nil {
		t.Fatal("extending a never-expiring key left it never-expiring")
	}
	if d := time.Until(*ext.Key.UsableUntil); d < 29*lcDay || d > 31*lcDay {
		t.Errorf("usable_until %v is not 30 days out", ext.Key.UsableUntil)
	}
}

func TestExtendRefusesBadDaysAndARevokedKey(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "r@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []int{0, -5, pgpwire.MaxKeyLifetimeDays + 1} {
		_, err := h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: d})
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
			t.Errorf("days %d: err = %v, want invalid_request", d, err)
		}
	}
	if _, err := h.client.Revoke(ctx, pgpwire.RevokeRequest{
		UserID: user, KeyID: gen.Key.KeyID, Reason: pgpwire.RevokeRetired, Confirm: fingerprintSuffix(gen.Key.Fingerprint),
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, err = h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: 30})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("extending a revoked key: err = %v, want invalid_request naming revocation", err)
	}

	// The packet-level refusal stands on its own, for a key whose RECORD does not
	// say revoked but whose packets do.
	h2 := startDaemon(t)
	gen2, err := h2.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "r2@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ent, err := h2.svc.unseal(func() Record { r, _ := h2.vault.Get(ctx, gen2.Key.KeyID); return r }())
	if err != nil {
		t.Fatal(err)
	}
	if err := ent.RevokeKey(0, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := extendEntity(ent, time.Now().Add(lcDay), time.Now()); err != errKeyRevoked {
		t.Errorf("extendEntity on a revoked entity: err = %v, want errKeyRevoked", err)
	}
}

func TestRevokeProducesARealRevocationAndKeepsDecrypting(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "rev@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Mail encrypted to the key BEFORE revocation.
	ct := encryptTo(t, pubRing(t, gen.Key.PublicKey), []byte(secretEntity), nil)

	// A wrong confirmation is refused and changes nothing.
	_, err = h.client.Revoke(ctx, pgpwire.RevokeRequest{UserID: user, KeyID: gen.Key.KeyID, Reason: pgpwire.RevokeCompromised, Confirm: "00000000"})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
		t.Fatalf("wrong confirm: err = %v, want invalid_request", err)
	}
	if rec, _ := h.vault.Get(ctx, gen.Key.KeyID); rec.RevokedAt != nil {
		t.Fatal("a refused revoke marked the key revoked")
	}
	// An unknown reason is refused.
	_, err = h.client.Revoke(ctx, pgpwire.RevokeRequest{UserID: user, KeyID: gen.Key.KeyID, Reason: "bored", Confirm: fingerprintSuffix(gen.Key.Fingerprint)})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
		t.Errorf("unknown reason: err = %v", err)
	}

	res, err := h.client.Revoke(ctx, pgpwire.RevokeRequest{
		UserID: user, KeyID: gen.Key.KeyID, Reason: pgpwire.RevokeCompromised,
		Confirm: strings.ToLower(fingerprintSuffix(gen.Key.Fingerprint)),
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if res.Key.Fingerprint != gen.Key.Fingerprint {
		t.Error("revocation changed the fingerprint")
	}
	rec, _ := h.vault.Get(ctx, gen.Key.KeyID)
	if rec.RevokedAt == nil || !rec.DecryptOnly {
		t.Errorf("record after revoke: revoked=%v decrypt_only=%v", rec.RevokedAt, rec.DecryptOnly)
	}
	// The stored AND returned public key carry a genuine revocation, read by
	// go-crypto itself.
	for name, pub := range map[string][]byte{"stored": rec.PublicKey, "returned": dearmor(t, res.Key.PublicKey)} {
		ring, err := openpgp.ReadKeyRing(bytes.NewReader(pub))
		if err != nil || len(ring) != 1 || !ring[0].Revoked(time.Now()) {
			t.Errorf("%s public key does not read as revoked (err=%v)", name, err)
		}
	}
	// It no longer signs...
	_, err = h.client.Sign(ctx, pgpwire.SignRequest{UserID: user, KeyID: gen.Key.KeyID, Data: []byte("x\r\n")})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
		t.Errorf("a revoked key signed: err = %v", err)
	}
	// ...and still decrypts what was sent to it.
	dr, err := h.client.Decrypt(ctx, pgpwire.DecryptRequest{
		UserID: user, MessageID: ulid.Make().String(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	})
	if err != nil || string(dr.Entity) != secretEntity {
		t.Errorf("a revoked key no longer decrypts old mail: %v", err)
	}
	// Twice is refused.
	_, err = h.client.Revoke(ctx, pgpwire.RevokeRequest{UserID: user, KeyID: gen.Key.KeyID, Reason: pgpwire.RevokeRetired, Confirm: fingerprintSuffix(gen.Key.Fingerprint)})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
		t.Errorf("second revoke: err = %v", err)
	}
}

// TestLifecycleConfusedDeputyFailsClosed: B may not extend or revoke A's key,
// even naming it exactly. Positive control: A can.
func TestLifecycleConfusedDeputyFailsClosed(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	alice, bob := newUserID(), newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: alice, Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := h.vault.Get(ctx, gen.Key.KeyID)

	_, err = h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: bob, KeyID: gen.Key.KeyID, Days: 30})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeDenied {
		t.Errorf("bob extending alice's key: err = %v, want denied", err)
	}
	_, err = h.client.Revoke(ctx, pgpwire.RevokeRequest{UserID: bob, KeyID: gen.Key.KeyID,
		Reason: pgpwire.RevokeCompromised, Confirm: fingerprintSuffix(gen.Key.Fingerprint)})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeDenied {
		t.Errorf("bob revoking alice's key: err = %v, want denied", err)
	}
	after, _ := h.vault.Get(ctx, gen.Key.KeyID)
	if !bytes.Equal(before.PublicKey, after.PublicKey) || after.RevokedAt != nil {
		t.Error("a refused confused-deputy request changed the key")
	}
	if _, err := h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: alice, KeyID: gen.Key.KeyID, Days: 30}); err != nil {
		t.Errorf("CONTROL: alice cannot extend her own key: %v", err)
	}
}

// TestLifecycleVerbsDoNotLeakIntoLogsOrErrors: extend and revoke unseal a
// private key. Nothing of it may reach the log, an error, or a response — on
// success or on refusal.
func TestLifecycleVerbsDoNotLeakIntoLogsOrErrors(t *testing.T) {
	h, logged := startDaemonLogging(t)
	ctx := context.Background()
	user := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "leak@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	priv := h.unsealPrivate(t, gen.Key.KeyID)
	det := newLeakDetector(t, priv, gen.Key.PublicKey)

	var corpus bytes.Buffer
	note := func(v any, err error) {
		if err != nil {
			corpus.WriteString(err.Error())
		}
		corpus.Write(mustJSON(t, v))
	}
	note(h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: 0}))
	note(h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: 90}))
	note(h.client.Revoke(ctx, pgpwire.RevokeRequest{UserID: user, KeyID: gen.Key.KeyID, Reason: pgpwire.RevokeSuperseded, Confirm: "FFFFFFFF"}))
	note(h.client.Revoke(ctx, pgpwire.RevokeRequest{UserID: user, KeyID: gen.Key.KeyID, Reason: pgpwire.RevokeSuperseded, Confirm: fingerprintSuffix(gen.Key.Fingerprint)}))
	note(h.client.Extend(ctx, pgpwire.ExtendRequest{UserID: user, KeyID: gen.Key.KeyID, Days: 90}))

	// The key was re-sealed twice; the CURRENT private serialization differs
	// from the original (new signatures), so check against it too.
	priv2 := h.unsealPrivate(t, gen.Key.KeyID)
	rec, _ := h.vault.Get(ctx, gen.Key.KeyID)
	det2 := newLeakDetector(t, priv2, mustArmorPublic(t, rec.PublicKey))

	if logged.Len() == 0 || corpus.Len() == 0 {
		t.Fatal("nothing logged or returned — this test would pass vacuously")
	}
	for name, d := range map[string]*leakDetector{"original": det, "re-sealed": det2} {
		if leak := d.findLeak(logged.Bytes()); leak != "" {
			t.Errorf("%s private key reached the log: %s", name, leak)
		}
		if leak := d.findLeak(corpus.Bytes()); leak != "" {
			t.Errorf("%s private key reached a response or error: %s", name, leak)
		}
	}
}

func mustArmorPublic(t *testing.T, pub []byte) string {
	t.Helper()
	s, err := ArmorPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
