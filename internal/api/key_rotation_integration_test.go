// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Key rotation (ADR-054, #31) against a real database: a contact's new key that
// the key we PINNED vouches for gets a gentler prompt, and NOTHING moves the pin
// without the user. Every key and certification is made by go-crypto.
//
// PROBES (run on the way in, each red then reverted; the commit message records
// them): making a vouch move the pin (TestAVouchNeverMovesAPin goes red); deleting
// the revocation refusal in pgpkeys.CheckVouch (the revoked cases go red); the
// first-sighting bound (the backdated case goes red); the VerifyUserIdSignature
// call and the issuer match (the merge tests in pgpkeys go red).

import (
	"context"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

const rotAddr = "alice@example.com"

// rot describes one rotation; defaults make an honest, vouched one.
type rot struct {
	now       time.Time
	pCreated  time.Time
	pLife     time.Duration // 0 = never expires
	nCreated  time.Time
	nLife     time.Duration
	nAddr     string // the address N's user ID names
	certAt    time.Time
	firstSeen time.Time // N's first_seen event
	pSeen     time.Time // P's first_seen event

	noCert      bool
	certUID     string // "" = N's own user ID
	extraUID    func(t *testing.T, n *openpgp.Entity)
	reviseP     func(t *testing.T, p *openpgp.Entity) // after certifying
	pinOther    bool                                  // pin a third key Q instead of P
	nRefused    bool
	nSource     string
	nFetchedAgo time.Duration
}

func defaultRot() rot {
	now := time.Now().UTC().Truncate(time.Second)
	return rot{
		now:         now,
		pCreated:    now.Add(-100 * 24 * time.Hour),
		pLife:       365 * 24 * time.Hour,
		nCreated:    now.Add(-3 * 24 * time.Hour),
		nLife:       730 * 24 * time.Hour,
		nAddr:       rotAddr,
		certAt:      now.Add(-47 * time.Hour),
		firstSeen:   now.Add(-time.Hour),
		pSeen:       now.Add(-90 * 24 * time.Hour),
		nSource:     store.ContactKeyWKD,
		nFetchedAgo: 0,
	}
}

func rotCfg(created time.Time, life time.Duration) *packet.Config {
	return &packet.Config{Time: func() time.Time { return created }, KeyLifetimeSecs: uint32(life / time.Second)}
}

type rotKeys struct {
	p, n         *openpgp.Entity
	pFpr, nFpr   string
	qFpr         string // the pinned key when pinOther
	pubP, pubN   []byte
	pinnedBefore string
}

// firstSeenAt records a first_seen event at `at` for the held key fpr (the real
// paths write it at now(); a test that needs another clock writes the row).
func (h *encHarness) firstSeenAt(t *testing.T, fpr string, at time.Time) {
	t.Helper()
	var id string
	if err := h.super.QueryRow(context.Background(),
		`SELECT id FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`, h.a.UserID.String(), fpr).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(context.Background(), `INSERT INTO app.contact_key_events
	    (id, user_id, contact_key_id, kind, source, at) VALUES ($1,$2,$3,'first_seen','wkd',$4)`,
		store.NewULID().String(), h.a.UserID.String(), id, at); err != nil {
		t.Fatal(err)
	}
}

func (h *encHarness) setRefused(t *testing.T, fpr string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.contact_keys SET refused=true WHERE user_id=$1 AND fingerprint=$2`, h.a.UserID.String(), fpr); err != nil {
		t.Fatal(err)
	}
}

// seedRotation builds the world r describes and pins P (or a third key).
func (h *encHarness) seedRotation(t *testing.T, r rot) rotKeys {
	t.Helper()
	var k rotKeys
	k.p = newPeerKey(t, rotAddr, rotCfg(r.pCreated, r.pLife))
	k.n = newPeerKey(t, r.nAddr, rotCfg(r.nCreated, r.nLife))
	if r.extraUID != nil {
		r.extraUID(t, k.n)
	}
	uid := r.certUID
	if uid == "" {
		uid = "Peer <" + r.nAddr + ">"
	}
	if !r.noCert {
		if err := k.n.SignIdentity(uid, k.p, &packet.Config{Time: func() time.Time { return r.certAt }}); err != nil {
			t.Fatalf("SignIdentity: %v", err)
		}
	}
	if r.reviseP != nil {
		r.reviseP(t, k.p)
	}
	k.pubP, k.pubN = pubOf(t, k.p), pubOf(t, k.n)
	k.pFpr = h.seedKey(t, rotAddr, k.pubP, store.ContactKeyWKD, false, r.now.Add(-48*time.Hour))
	h.firstSeenAt(t, k.pFpr, r.pSeen)
	k.nFpr = h.seedKey(t, rotAddr, k.pubN, r.nSource, false, r.now.Add(-r.nFetchedAgo))
	if !r.firstSeen.IsZero() {
		h.firstSeenAt(t, k.nFpr, r.firstSeen)
	}
	if r.nRefused {
		h.setRefused(t, k.nFpr)
	}
	pin := k.pFpr
	if r.pinOther {
		q := newPeerKey(t, rotAddr, nil)
		k.qFpr = h.seedKey(t, rotAddr, pubOf(t, q), store.ContactKeyWKD, false, r.now.Add(-72*time.Hour))
		pin = k.qFpr
	}
	if err := h.setPin(rotAddr, pin); err != nil {
		t.Fatal(err)
	}
	k.pinnedBefore = pin
	return k
}

func revokeP(reason packet.ReasonForRevocation, at time.Time) func(*testing.T, *openpgp.Entity) {
	return func(t *testing.T, p *openpgp.Entity) {
		if err := p.RevokeKey(reason, "test", &packet.Config{Time: func() time.Time { return at }}); err != nil {
			t.Fatal(err)
		}
	}
}

// sigFrom is the pgpSignature of a multipart/signed message from alice by n.
func (h *encHarness) sigFrom(t *testing.T, n *openpgp.Entity) map[string]any {
	return h.sigOf(t, signedFromAlice(t, n))
}

// wantKeyChanged checks the state and the vouch fields of a keyChanged result.
func wantKeyChanged(t *testing.T, got map[string]any, vouchedBy string, compromised bool) {
	t.Helper()
	wantState(t, got, "keyChanged")
	gv, _ := got["vouchedBy"].(string)
	if gv != vouchedBy {
		t.Errorf("vouchedBy = %q, want %q (%v)", gv, vouchedBy, got)
	}
	gc, _ := got["previousKeyCompromised"].(bool)
	if gc != compromised {
		t.Errorf("previousKeyCompromised = %v, want %v (%v)", gc, compromised, got)
	}
}

// ---------------------------------------------------------------------------

func TestVouchedRotationIsKeyChangedWithVouchedBy(t *testing.T) {
	h := newEncHarness(t)
	k := h.seedRotation(t, defaultRot())
	got := h.sigFrom(t, k.n)
	wantKeyChanged(t, got, k.pFpr, false)
	if got["previousFingerprint"] != k.pFpr || got["fingerprint"] != k.nFpr {
		t.Errorf("fingerprints = %v", got)
	}
	// CONTROL: the pinned key itself is simply verified, with no vouch fields.
	if v := h.sigFrom(t, k.p); v["state"] != "verified" || v["vouchedBy"] != nil {
		t.Errorf("pinned key: %v", v)
	}
}

// Every refusal in ADR-054 section 3, each against the vouched control above.
func TestVouchRefusalsGiveTheOrdinaryPrompt(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name        string
		mut         func(*rot)
		compromised bool
		state       string // default keyChanged
	}{
		{"P held and verified once but not the pin", func(r *rot) { r.pinOther = true }, false, ""},
		{"certification over another address", func(r *rot) {
			r.extraUID = func(t *testing.T, n *openpgp.Entity) {
				if err := n.AddUserId("Other", "", "b@example.com", &packet.Config{Time: func() time.Time { return r.nCreated }}); err != nil {
					t.Fatal(err)
				}
			}
			r.certUID = "Other <b@example.com>"
		}, false, ""},
		{"user ID with no address", func(r *rot) {
			r.extraUID = func(t *testing.T, n *openpgp.Entity) {
				if err := n.AddUserId("only a name", "", "", &packet.Config{Time: func() time.Time { return r.nCreated }}); err != nil {
					t.Fatal(err)
				}
			}
			r.certUID = "only a name"
		}, false, ""},
		{"no certification at all", func(r *rot) { r.noCert = true }, false, ""},
		{"P revoked (superseded) after the certification", func(r *rot) { r.reviseP = revokeP(packet.KeySuperseded, r.now.Add(-time.Hour)) }, false, ""},
		{"P revoked (superseded) before the certification", func(r *rot) { r.reviseP = revokeP(packet.KeySuperseded, r.certAt.Add(-time.Hour)) }, false, ""},
		{"P revoked as compromised", func(r *rot) { r.reviseP = revokeP(packet.KeyCompromised, r.now.Add(-time.Hour)) }, true, ""},
		{"dated before N existed", func(r *rot) { r.certAt = r.nCreated.Add(-time.Hour) }, false, ""},
		{"dated two days in the future", func(r *rot) {
			r.certAt = r.now.Add(48 * time.Hour)
			r.nCreated = r.firstSeen.Add(-day)
		}, false, ""},
		{"N user-refused", func(r *rot) { r.nRefused = true }, false, ""},
		{"N first sighting unknown", func(r *rot) { r.firstSeen = time.Time{} }, false, ""},
		{"N does not name the address", func(r *rot) { r.nAddr = "b@example.com" }, false, "unknownKey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newEncHarness(t)
			r := defaultRot()
			c.mut(&r)
			k := h.seedRotation(t, r)
			got := h.sigFrom(t, k.n)
			if c.state == "unknownKey" {
				wantState(t, got, "unknownKey")
				if got["vouchedBy"] != nil {
					t.Errorf("an unknownKey carries vouchedBy: %v", got)
				}
				return
			}
			wantKeyChanged(t, got, "", c.compromised)
			if pin, _ := h.pin(t, rotAddr); pin != k.pinnedBefore {
				t.Errorf("the pin moved to %s", pin)
			}
		})
	}
}

// The backdating bound (rule 5), through the real signature path.
func TestVouchBackdatedCertification(t *testing.T) {
	day := 24 * time.Hour
	build := func(t *testing.T, seenAfterExpiry time.Duration) map[string]any {
		h := newEncHarness(t)
		r := defaultRot()
		pExpiry := r.now.Add(-365 * day) // P expired a year ago
		r.pCreated, r.pLife = pExpiry.Add(-400*day), 400*day
		r.pSeen = r.pCreated.Add(day)
		r.certAt = pExpiry.Add(-day) // dated the day before it expired
		r.nCreated = r.certAt.Add(-time.Hour)
		r.nLife = 0
		r.firstSeen = r.now // N first seen today
		if seenAfterExpiry != 0 {
			r.firstSeen = pExpiry.Add(seenAfterExpiry)
		}
		k := h.seedRotation(t, r)
		return h.sigFrom(t, k.n)
	}
	got := build(t, 0)
	wantKeyChanged(t, got, "", false) // refused: first seen a year after P expired
	// POSITIVE CONTROL: the same certification, N first seen 20 days after expiry.
	got = build(t, 20*day)
	if v, _ := got["vouchedBy"].(string); v == "" {
		t.Errorf("seen 20 days after the expiry: not vouched: %v", got)
	}
}

func TestVouchClockSkewBound(t *testing.T) {
	h := newEncHarness(t)
	r := defaultRot()
	r.firstSeen = r.now.Add(-2 * time.Hour)
	r.nCreated = r.firstSeen.Add(-24 * time.Hour)
	r.certAt = r.firstSeen.Add(time.Hour) // one hour after first sighting
	k := h.seedRotation(t, r)
	wantKeyChanged(t, h.sigFrom(t, k.n), k.pFpr, false)

	// Rule 6 refuses only a FUTURE date: nine days after first sighting, already
	// past, is an honest late certification and vouches.
	h2 := newEncHarness(t)
	r2 := defaultRot()
	r2.firstSeen = r2.now.Add(-10 * 24 * time.Hour)
	r2.nCreated = r2.firstSeen.Add(-24 * time.Hour)
	r2.certAt = r2.firstSeen.Add(9 * 24 * time.Hour)
	k2 := h2.seedRotation(t, r2)
	wantKeyChanged(t, h2.sigFrom(t, k2.n), k2.pFpr, false)
}

// The second ADR-039 shape: a signature inside the encryption goes through the
// same judge.
func TestVouchedRotationEmbeddedShape(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	k := h.seedRotation(t, defaultRot())
	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, k.n, embeddedEntity)))
	wantKeyChanged(t, got, k.pFpr, false)

	h2 := newEncHarness(t)
	me2 := h2.withCryptoDecrypter(t)
	r := defaultRot()
	r.reviseP = revokeP(packet.KeyCompromised, r.now.Add(-time.Hour))
	k2 := h2.seedRotation(t, r)
	wantKeyChanged(t, h2.sigOf(t, fromAliceEncrypted(combined(t, me2, k2.n, embeddedEntity))), "", true)
}

// ---- lookup, compose, decide, and the one guarantee ---------------------------

func (h *encHarness) lookupRow(t *testing.T) map[string]any {
	t.Helper()
	lk := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{rotAddr}, "wkd": false})
	return lk["list"].([]any)[0].(map[string]any)
}

func TestRecipientLookupCarriesTheVouch(t *testing.T) {
	h := newEncHarness(t)
	k := h.seedRotation(t, defaultRot())
	row := h.lookupRow(t)
	if row["keyChanged"] != true || row["usable"] != false || row["vouchedBy"] != k.pFpr || row["fingerprint"] != k.nFpr {
		t.Errorf("lookup = %v", row)
	}
	if _, present := row["previousKeyCompromised"]; present {
		t.Errorf("previousKeyCompromised present on a vouched rotation: %v", row)
	}

	// Revoked as compromised: no vouch, the warning instead.
	h2 := newEncHarness(t)
	r := defaultRot()
	r.reviseP = revokeP(packet.KeyCompromised, r.now.Add(-time.Hour))
	h2.seedRotation(t, r)
	row = h2.lookupRow(t)
	if row["keyChanged"] != true || row["vouchedBy"] != nil || row["previousKeyCompromised"] != true {
		t.Errorf("compromised lookup = %v", row)
	}

	// An ordinary change (no certification): the fields are ABSENT, as today.
	h3 := newEncHarness(t)
	r = defaultRot()
	r.noCert = true
	h3.seedRotation(t, r)
	row = h3.lookupRow(t)
	if _, a := row["vouchedBy"]; a {
		t.Errorf("vouchedBy present without a vouch: %v", row)
	}
	if _, a := row["previousKeyCompromised"]; a {
		t.Errorf("previousKeyCompromised present without a revocation: %v", row)
	}
}

func TestCannotEncryptCarriesTheVouch(t *testing.T) {
	h := newEncHarness(t)
	k := h.seedRotation(t, defaultRot())
	id := h.draft(t, []string{rotAddr}, nil)
	before := h.hash(t, id)
	errObj := h.submit(t, id, true)
	h.refused(t, id, errObj, before)
	changes, _ := errObj["keyChanges"].([]any)
	if len(changes) != 1 {
		t.Fatalf("keyChanges = %v", errObj["keyChanges"])
	}
	c := changes[0].(map[string]any)
	if c["address"] != rotAddr || c["vouchedBy"] != k.pFpr || c["pinnedFingerprint"] != k.pFpr || c["fingerprint"] != k.nFpr {
		t.Errorf("keyChanges[0] = %v", c)
	}
	// Still refused: a vouch does not let the send through.
	if pin, _ := h.pin(t, rotAddr); pin != k.pFpr {
		t.Errorf("the pin moved to %s", pin)
	}
}

// ⚠ THE GUARANTEE (operator decision, ADR-054 section 4): nothing a vouch does
// moves the pin. Every read path that computes a vouch runs, then the pin and the
// event log are checked. PROBE: make vouchFor pin the key it vouched for and this
// goes red.
func TestAVouchNeverMovesAPin(t *testing.T) {
	h := newEncHarness(t)
	k := h.seedRotation(t, defaultRot())

	wantKeyChanged(t, h.sigFrom(t, k.n), k.pFpr, false) // the vouch is computed...
	if row := h.lookupRow(t); row["vouchedBy"] != k.pFpr {
		t.Fatalf("lookup did not vouch: %v", row)
	}
	id := h.draft(t, []string{rotAddr}, nil)
	h.refused(t, id, h.submit(t, id, true), h.hash(t, id))

	// ...and the pin is where the user left it, with no pinned event.
	if pin, _ := h.pin(t, rotAddr); pin != k.pFpr {
		t.Fatalf("a vouch moved the pin: %s -> %s", k.pFpr, pin)
	}
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_key_events WHERE user_id=$1 AND kind='pinned'`, h.a.UserID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d pinned events were written without the user deciding", n)
	}
	// The key N stays what it was: not trusted, not refused.
	var src string
	var refused bool
	if err := h.super.QueryRow(context.Background(),
		`SELECT source, refused FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`, h.a.UserID.String(), k.nFpr).Scan(&src, &refused); err != nil {
		t.Fatal(err)
	}
	if src != store.ContactKeyWKD || refused {
		t.Errorf("new key changed without a decision: source=%s refused=%v", src, refused)
	}

	// CONTROL, so this test can fail: the user deciding DOES move it.
	h.decide(t, rotAddr, k.nFpr, "trust")
	if pin, _ := h.pin(t, rotAddr); pin != k.nFpr {
		t.Errorf("control: deciding trust left the pin at %s", pin)
	}
}

func (h *encHarness) pinnedEventSource(t *testing.T, fpr string) string {
	t.Helper()
	var src string
	if err := h.super.QueryRow(context.Background(), `
		SELECT e.source FROM app.contact_key_events e JOIN app.contact_keys k ON k.id = e.contact_key_id
		 WHERE k.user_id=$1 AND k.fingerprint=$2 AND e.kind='pinned' ORDER BY e.at DESC LIMIT 1`,
		h.a.UserID.String(), fpr).Scan(&src); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestDecideTrustRecordsTheVoucher(t *testing.T) {
	h := newEncHarness(t)
	k := h.seedRotation(t, defaultRot())
	h.decide(t, rotAddr, k.nFpr, "trust")
	if got := h.pinnedEventSource(t, k.nFpr); got != "vouched:"+k.pFpr {
		t.Errorf("pinned event source = %q, want vouched:%s", got, k.pFpr)
	}

	// Not vouched: the ordinary source.
	h2 := newEncHarness(t)
	r := defaultRot()
	r.noCert = true
	k2 := h2.seedRotation(t, r)
	h2.decide(t, rotAddr, k2.nFpr, "trust")
	if got := h2.pinnedEventSource(t, k2.nFpr); got != store.KeyEventSourceUser {
		t.Errorf("pinned event source = %q, want user", got)
	}
}

// The store path end to end: the new key arrives with its certification (the
// INSERT path), is re-sighted unchanged (the merge path) and keeps it, and the
// first_seen event the real path writes is what the vouch is judged against.
func TestInsertPathKeepsAHeldKeysCertificationAndVouches(t *testing.T) {
	h := newEncHarness(t)
	ctx := context.Background()
	r := defaultRot()
	p := newPeerKey(t, rotAddr, rotCfg(r.pCreated, r.pLife))
	n := newPeerKey(t, rotAddr, rotCfg(r.nCreated, r.nLife))
	if err := n.SignIdentity("Peer <"+rotAddr+">", p, &packet.Config{Time: func() time.Time { return r.certAt }}); err != nil {
		t.Fatal(err)
	}
	pubP, pubN := pubOf(t, p), pubOf(t, n)
	pFpr := h.seedKey(t, rotAddr, pubP, store.ContactKeyWKD, false, r.now.Add(-48*time.Hour))
	h.firstSeenAt(t, pFpr, r.pSeen)
	if err := h.setPin(rotAddr, pFpr); err != nil {
		t.Fatal(err)
	}
	var cid string
	if err := h.super.QueryRow(ctx, `SELECT contact_id FROM app.contact_addresses WHERE user_id=$1 AND address=$2`,
		h.a.UserID.String(), rotAddr).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	contactID := ulid.MustParse(cid)
	nParsed, _ := pgpkeys.Parse(pubN)

	upsert := func() store.UpsertResult {
		var res store.UpsertResult
		if err := h.api.store.UserTx(ctx, h.a.UserID, func(tx *store.UserTx) error {
			var err error
			res, err = tx.UpsertContactKeyDetail(ctx, store.ContactKeyRow{
				ContactID: contactID, Kind: store.KeyPGP, Fingerprint: nParsed.Fingerprint, PublicKey: pubN,
				Source: store.ContactKeyWKD,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := upsert(); !res.Created {
		t.Fatalf("first sighting: %+v", res)
	}
	if res := upsert(); res.Merged {
		t.Errorf("an identical re-sighting changed the stored key (the certification was stripped?): %+v", res)
	}
	var stored []byte
	if err := h.super.QueryRow(ctx, `SELECT public_key FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), nParsed.Fingerprint).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !pgpkeys.CertifiedBy(stored, pubP, rotAddr) {
		t.Fatal("the stored copy lost the certification")
	}
	// A renewal of N arrives (a real merge, which rewrites the stored key): the
	// certification is still there afterwards.
	renewed := n
	later := r.now.Add(-time.Hour)
	for name, ident := range renewed.Identities {
		cp := *ident.SelfSignature
		cp.CreationTime = later
		l := uint32((900 * 24 * time.Hour) / time.Second)
		cp.KeyLifetimeSecs = &l
		if err := cp.SignUserId(name, renewed.PrimaryKey, renewed.PrivateKey, &packet.Config{Time: func() time.Time { return later }}); err != nil {
			t.Fatal(err)
		}
		ident.SelfSignature = &cp
		ident.Signatures = append(ident.Signatures, &cp)
	}
	pubN = pubOf(t, renewed)
	if res := upsert(); !res.Merged {
		t.Fatalf("a renewal was not merged: %+v", res)
	}
	if err := h.super.QueryRow(ctx, `SELECT public_key FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), nParsed.Fingerprint).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !pgpkeys.CertifiedBy(stored, pubP, rotAddr) {
		t.Fatal("a merge that rewrote the stored key dropped the certification")
	}

	// The vouch, judged against the first_seen the real path wrote (just now).
	got := h.sigOf(t, signedFromAlice(t, n))
	wantKeyChanged(t, got, pFpr, false)

	// CONTROL: with P removed from the contact, the next rewrite loses the
	// certification (P is no longer a held certifier).
	if _, err := h.super.Exec(ctx, `UPDATE app.contact_keys SET removed_at = now() WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), pFpr); err != nil {
		t.Fatal(err)
	}
	later2 := r.now
	for name, ident := range renewed.Identities {
		cp := *ident.SelfSignature
		cp.CreationTime = later2
		l := uint32((1000 * 24 * time.Hour) / time.Second)
		cp.KeyLifetimeSecs = &l
		if err := cp.SignUserId(name, renewed.PrimaryKey, renewed.PrivateKey, &packet.Config{Time: func() time.Time { return later2 }}); err != nil {
			t.Fatal(err)
		}
		ident.SelfSignature = &cp
		ident.Signatures = append(ident.Signatures, &cp)
	}
	pubN = pubOf(t, renewed)
	upsert()
	if err := h.super.QueryRow(ctx, `SELECT public_key FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), nParsed.Fingerprint).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if pgpkeys.CertifiedBy(stored, pubP, rotAddr) {
		t.Error("control: with the certifier removed the certification should not survive a rewrite")
	}
}
