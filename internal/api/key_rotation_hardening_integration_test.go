// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The security-review fixes to ADR-054 (#31), against a real database:
//   - only the PINNED key certifies (a held or refused key never does) and at most
//     one certification per certifier and user ID is stored;
//   - rule 8, the notice when the key that vouched for the pin is later revoked.
//
// PROBES (each red, then reverted): restoring the old certifier query (every
// non-removed key of the contact, refused included) turns the held-but-unpinned
// and refused tests red; making the merge keep every certification turns the
// 300-certification test red; making PinVoucherFor return nothing turns every
// rule 8 test red.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

func (h *encHarness) rotContactID(t *testing.T) ulid.ULID {
	t.Helper()
	var cid string
	if err := h.super.QueryRow(context.Background(), `SELECT contact_id FROM app.contact_addresses WHERE user_id=$1 AND address=$2`,
		h.a.UserID.String(), rotAddr).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	return ulid.MustParse(cid)
}

func (h *encHarness) upsertPub(t *testing.T, pub []byte) store.UpsertResult {
	t.Helper()
	ctx := context.Background()
	parsed, err := pgpkeys.Parse(pub)
	if err != nil {
		t.Fatal(err)
	}
	cid := h.rotContactID(t)
	var res store.UpsertResult
	if err := h.api.store.UserTx(ctx, h.a.UserID, func(tx *store.UserTx) error {
		var err error
		res, err = tx.UpsertContactKeyDetail(ctx, store.ContactKeyRow{
			ContactID: cid, Kind: store.KeyPGP, Fingerprint: parsed.Fingerprint, PublicKey: pub, Source: store.ContactKeyWKD,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *encHarness) storedKey(t *testing.T, fpr string) []byte {
	t.Helper()
	var b []byte
	if err := h.super.QueryRow(context.Background(), `SELECT public_key FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), fpr).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// certCount counts third-party certifications (0x10-0x13) in a stored key.
func certCount(t *testing.T, pub []byte) int {
	t.Helper()
	n := 0
	var primary *packet.PublicKey
	r := packet.NewReader(bytes.NewReader(pub))
	for {
		p, err := r.Next()
		if err != nil {
			return n
		}
		switch pk := p.(type) {
		case *packet.PublicKey:
			if !pk.IsSubkey && primary == nil {
				primary = pk
			}
		case *packet.Signature:
			if pk.SigType >= packet.SigTypeGenericCert && pk.SigType <= packet.SigTypePositiveCert && !pk.CheckKeyIdOrFingerprint(primary) {
				n++
			}
		}
	}
}

func certifyMany(t *testing.T, subject, signer *openpgp.Entity, count int, base time.Time) {
	t.Helper()
	for i := 0; i < count; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		if err := subject.SignIdentity("Peer <"+rotAddr+">", signer, &packet.Config{Time: func() time.Time { return at }}); err != nil {
			t.Fatal(err)
		}
	}
}

// H is the honest key. X is a stranger's key that holds for the contact (holding is
// free: it arrived in an Autocrypt header). X certifies H 300 times and H is
// re-sighted with them. They must not be stored, whether X is merely held or
// refused; and the honest key must still take a renewal afterwards.
func TestAStrangersHeldKeyCannotBloatAnotherKeysStoredCopy(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "held but unpinned"
		if refused {
			name = "refused"
		}
		t.Run(name, func(t *testing.T) {
			h := newEncHarness(t)
			now := time.Now().UTC().Truncate(time.Second)
			honest := newPeerKey(t, rotAddr, rotCfg(now.Add(-50*24*time.Hour), 365*24*time.Hour))
			attacker := newPeerKey(t, rotAddr, rotCfg(now.Add(-40*24*time.Hour), 365*24*time.Hour))
			honestFpr := h.seedKey(t, rotAddr, pubOf(t, honest), store.ContactKeyWKD, false, now.Add(-48*time.Hour))
			xFpr := h.seedKey(t, rotAddr, pubOf(t, attacker), store.ContactKeyAutocrypt, false, now.Add(-24*time.Hour))
			// The user's pin is the honest key; with a refused attacker it is the attacker's
			// pin that is the hard case (a pin on a refused key), so pin it there.
			if refused {
				h.setRefused(t, xFpr)
				if err := h.setPin(rotAddr, xFpr); err != nil {
					t.Fatal(err)
				}
			} else if err := h.setPin(rotAddr, honestFpr); err != nil {
				t.Fatal(err)
			}
			certifyMany(t, honest, attacker, 300, now.Add(-time.Hour))
			h.upsertPub(t, pubOf(t, honest))
			stored := h.storedKey(t, honestFpr)
			if n := certCount(t, stored); n != 0 {
				t.Fatalf("%d certifications by a key that is not a pinned, unrefused certifier were stored", n)
			}
			if len(stored) > 4096 {
				t.Errorf("stored copy is %d bytes", len(stored))
			}
			// An honest renewal after the attempt still merges.
			later := now.Add(-time.Minute)
			for n, ident := range honest.Identities {
				cp := *ident.SelfSignature
				cp.CreationTime = later
				l := uint32((900 * 24 * time.Hour) / time.Second)
				cp.KeyLifetimeSecs = &l
				if err := cp.SignUserId(n, honest.PrimaryKey, honest.PrivateKey, &packet.Config{Time: func() time.Time { return later }}); err != nil {
					t.Fatal(err)
				}
				ident.SelfSignature = &cp
				ident.Signatures = append(ident.Signatures, &cp)
			}
			if res := h.upsertPub(t, pubOf(t, honest)); !res.Merged {
				t.Errorf("the renewal was not merged after a bloat attempt: %+v", res)
			}
		})
	}
}

// CONTROL and the bound: the PINNED key as certifier has one certification stored,
// the newest, out of 300 sent.
func TestAPinnedCertifierKeepsExactlyOneCertification(t *testing.T) {
	h := newEncHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	p := newPeerKey(t, rotAddr, rotCfg(now.Add(-50*24*time.Hour), 365*24*time.Hour))
	n := newPeerKey(t, rotAddr, rotCfg(now.Add(-3*24*time.Hour), 365*24*time.Hour))
	pFpr := h.seedKey(t, rotAddr, pubOf(t, p), store.ContactKeyWKD, false, now.Add(-48*time.Hour))
	nFpr := h.seedKey(t, rotAddr, pubOf(t, n), store.ContactKeyWKD, false, now.Add(-24*time.Hour))
	if err := h.setPin(rotAddr, pFpr); err != nil {
		t.Fatal(err)
	}
	certifyMany(t, n, p, 300, now.Add(-time.Hour))
	h.upsertPub(t, pubOf(t, n))
	stored := h.storedKey(t, nFpr)
	if got := certCount(t, stored); got != 1 {
		t.Fatalf("%d certifications stored, want exactly 1", got)
	}
	if len(stored) > 4096 {
		t.Errorf("stored copy is %d bytes: not bounded", len(stored))
	}
	if !pgpkeys.CertifiedBy(stored, pubOf(t, p), rotAddr) {
		t.Error("the kept certification does not verify under the pinned key")
	}
}

// ---- rule 8 ---------------------------------------------------------------------------

func revokeAndMerge(t *testing.T, h *encHarness, p *openpgp.Entity, reason packet.ReasonForRevocation) {
	t.Helper()
	if err := p.RevokeKey(reason, "test", &packet.Config{Time: func() time.Time { return time.Now().Add(-time.Minute) }}); err != nil {
		t.Fatal(err)
	}
	h.upsertPub(t, pubOf(t, p))
}

func TestPinVouchedByRevokedKeyIsReportedAtAPIAndLookup(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason packet.ReasonForRevocation
		compro bool
	}{{"compromised", packet.KeyCompromised, true}, {"superseded", packet.KeySuperseded, false}} {
		t.Run(c.name, func(t *testing.T) {
			h := newEncHarness(t)
			k := h.seedRotation(t, defaultRot())
			h.decide(t, rotAddr, k.nFpr, "trust") // the pin moves to N, source vouched:P
			if got := h.pinnedEventSource(t, k.nFpr); got != "vouched:"+k.pFpr {
				t.Fatalf("fixture: pinned event source = %q", got)
			}
			// BEFORE the revocation: no notice anywhere.
			before := h.sigFrom(t, k.n)
			wantState(t, before, "verified")
			if before["pinVouchedByRevoked"] != nil {
				t.Errorf("notice before any revocation: %v", before)
			}
			if _, a := h.lookupRow(t)["pinVouchedByRevoked"]; a {
				t.Error("lookup carries the notice before any revocation")
			}

			revokeAndMerge(t, h, k.p, c.reason) // P's revocation is merged in

			got := h.sigFrom(t, k.n)
			wantState(t, got, "verified")
			if got["pinVouchedByRevoked"] != true {
				t.Fatalf("no notice after the voucher was revoked: %v", got)
			}
			if gc, _ := got["pinVouchedByCompromised"].(bool); gc != c.compro {
				t.Errorf("pinVouchedByCompromised = %v, want %v", gc, c.compro)
			}
			row := h.lookupRow(t)
			if row["usable"] != true || row["pinVouchedByRevoked"] != true {
				t.Fatalf("lookup = %v", row)
			}
			if gc, _ := row["pinVouchedByCompromised"].(bool); gc != c.compro {
				t.Errorf("lookup pinVouchedByCompromised = %v, want %v", gc, c.compro)
			}
			// Nothing changed the pin.
			if pin, _ := h.pin(t, rotAddr); pin != k.nFpr {
				t.Errorf("the pin moved to %s", pin)
			}
		})
	}
}

// CONTROL: a pin the user decided WITHOUT a vouch shows no warning though the
// previous key is revoked as well.
func TestAPinWithoutAVouchShowsNoRevokedVoucherNotice(t *testing.T) {
	h := newEncHarness(t)
	r := defaultRot()
	r.noCert = true
	k := h.seedRotation(t, r)
	h.decide(t, rotAddr, k.nFpr, "trust")
	if got := h.pinnedEventSource(t, k.nFpr); got != store.KeyEventSourceUser {
		t.Fatalf("fixture: source = %q", got)
	}
	revokeAndMerge(t, h, k.p, packet.KeyCompromised)
	got := h.sigFrom(t, k.n)
	wantState(t, got, "verified")
	if got["pinVouchedByRevoked"] != nil || got["pinVouchedByCompromised"] != nil {
		t.Errorf("notice without a vouch: %v", got)
	}
	row := h.lookupRow(t)
	if _, a := row["pinVouchedByRevoked"]; a {
		t.Errorf("lookup carries a notice without a vouch: %v", row)
	}
}

// Migration 00096's CHECK admits exactly "vouched:" plus an uppercase hex
// fingerprint, not any text after the prefix.
func TestVouchedEventSourceCheckIsExact(t *testing.T) {
	h := newEncHarness(t)
	k := h.seedRotation(t, defaultRot())
	var keyID string
	if err := h.super.QueryRow(context.Background(), `SELECT id FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), k.nFpr).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	insert := func(source string) error {
		_, err := h.super.Exec(context.Background(), `INSERT INTO app.contact_key_events
		    (id, user_id, contact_key_id, kind, source) VALUES ($1,$2,$3,'pinned',$4)`,
			store.NewULID().String(), h.a.UserID.String(), keyID, source)
		return err
	}
	if err := insert("vouched:" + k.pFpr); err != nil { // CONTROL: the form the code writes
		t.Fatalf("a real voucher was refused: %v", err)
	}
	for _, bad := range []string{"vouched:", "vouched:free text", "vouched:" + k.pFpr + "x", "vouched:" + k.pFpr[:39], "vouched:abcdef0123456789abcdef0123456789abcdef01"} {
		if err := insert(bad); err == nil {
			t.Errorf("source %q was accepted", bad)
		}
	}
}
