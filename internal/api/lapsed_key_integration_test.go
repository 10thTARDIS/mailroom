// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// #795: a lapsed own key is not advertised. The outgoing Autocrypt header comes
// from the identity's bound key (SigningKeyForIdentity → prepareOutbound); a
// key whose packets say it has EXPIRED, or that has been RETIRED or REVOKED,
// must neither be put in that header nor be asked to sign, and the send still
// goes out. Positive control on the same
// harness: a live real key IS advertised and signs.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func realOwnKey(t *testing.T, address string, created time.Time, lifetime time.Duration) []byte {
	t.Helper()
	e, err := openpgp.NewEntity("Me", "", address, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// bindRealKey is bindKeyToIdentity with real public key bytes.
func bindRealKey(t *testing.T, h *composeHarness, userID ulid.ULID, address string, pub []byte) {
	t.Helper()
	ctx := context.Background()
	var identityID string
	if err := h.super.QueryRow(ctx,
		`SELECT id FROM app.identities WHERE user_id = $1 AND address = $2`,
		userID.String(), address).Scan(&identityID); err != nil {
		t.Fatalf("find identity: %v", err)
	}
	keyID := store.NewULID().String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.user_keys (id, user_id, identity_id, kind, fingerprint, public_key, private_key_ct)
		 VALUES ($1,$2,$3,'pgp',$4,$5,'\xdeadbeef'::bytea)`,
		keyID, userID.String(), identityID, "FPR"+keyID, pub); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`UPDATE app.identities SET default_key_id = $3 WHERE user_id = $1 AND id = $2`,
		userID.String(), identityID, keyID); err != nil {
		t.Fatalf("bind key: %v", err)
	}
}

func TestLapsedOwnKeyIsNotAdvertisedOrUsedToSign(t *testing.T) {
	live := func(t *testing.T) []byte {
		return realOwnKey(t, "me@example.com", time.Now().Add(-time.Hour), 0)
	}
	cases := []struct {
		name      string
		pub       func(t *testing.T) []byte
		mark      string // optional UPDATE run on the bound key ($1 = user id)
		advertise bool
	}{
		{"live key (control)", live, "", true},
		{"expired key", func(t *testing.T) []byte {
			return realOwnKey(t, "me@example.com", time.Now().Add(-40*24*time.Hour), 30*24*time.Hour)
		}, "", false},
		// pgp-retire's and pgp-revoke's column writes (ADR-040), on an
		// otherwise live key: SigningKeyForIdentity must drop both.
		{"retired key", live, `UPDATE app.user_keys SET decrypt_only = true WHERE user_id = $1`, false},
		{"revoked key", live, `UPDATE app.user_keys SET revoked_at = now(), decrypt_only = true WHERE user_id = $1`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			signer := &stubSigner{}
			h, blobs := newSigningHarness(t, signer)
			a, _ := seedTwo(t, h.super)
			seedSendable(t, h.super, a, "me@example.com", "")
			bindRealKey(t, h, a.UserID, "me@example.com", c.pub(t))
			if c.mark != "" {
				if _, err := h.super.Exec(context.Background(), c.mark, a.UserID.String()); err != nil {
					t.Fatalf("mark key: %v", err)
				}
			}

			emailID, _ := composeAndSubmit(t, h, a.UserID, "me@example.com", "hello\n")
			raw := storedBytes(t, h, blobs, a.UserID, emailID)

			hasHeader := bytes.Contains(raw, []byte("Autocrypt:"))
			signed := signer.count() > 0
			if hasHeader != c.advertise || signed != c.advertise {
				t.Errorf("Autocrypt header=%v signed=%v, want both %v", hasHeader, signed, c.advertise)
			}
			// The send itself went through either way.
			if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
				a.UserID.String()); got != 1 {
				t.Errorf("submissions = %d, want 1 — a lapsed key must not block sending", got)
			}
		})
	}
}
