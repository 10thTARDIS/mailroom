// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"bytes"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func ownTestKey(t *testing.T, created time.Time, lifetime time.Duration, revoke bool) []byte {
	t.Helper()
	e, err := openpgp.NewEntity("Me", "", "me@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if revoke {
		if err := e.RevokeKey(packet.KeyCompromised, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ownKeySubkeyExpired: a primary key that never expires whose ENCRYPTION
// subkey's binding (re-made by go-crypto, as gpg --quick-set-expire would) gave
// it a lifetime that has run out. It can still sign; nobody can encrypt to it.
func ownKeySubkeyExpired(t *testing.T, created time.Time, subLife time.Duration) []byte {
	t.Helper()
	e, err := openpgp.NewEntity("Me", "", "me@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time: func() time.Time { return created },
	})
	if err != nil {
		t.Fatal(err)
	}
	at := created.Add(time.Hour)
	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		cp := *sub.Sig
		cp.CreationTime = at
		l := uint32(subLife / time.Second)
		cp.KeyLifetimeSecs = &l
		if err := cp.SignKey(sub.PublicKey, e.PrivateKey, &packet.Config{Time: func() time.Time { return at }}); err != nil {
			t.Fatal(err)
		}
		sub.Sig = &cp
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestOwnKeyFitness: a lapsed own key is neither advertised over Autocrypt nor
// used to sign (#795); a live one is both; an unreadable one keeps the old
// behaviour rather than silently going quiet.
func TestOwnKeyFitness(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	cases := []struct {
		name            string
		pub             []byte
		advertise, sign bool
	}{
		{"live, never expires", ownTestKey(t, now.Add(-day), 0, false), true, true},
		{"live, expires later", ownTestKey(t, now.Add(-day), 30*day, false), true, true},
		{"expired", ownTestKey(t, now.Add(-40*day), 30*day, false), false, false},
		{"revoked", ownTestKey(t, now.Add(-day), 0, true), false, false},
		// The subkey lapses first: not advertised (nobody can encrypt to it),
		// still signs (the primary is live) — the case the primary alone misses.
		{"encryption subkey expired", ownKeySubkeyExpired(t, now.Add(-40*day), 30*day), false, true},
		{"unreadable", []byte{0x99, 0x01, 0x02, 0x03}, true, true},
	}
	for _, c := range cases {
		a, s := ownKeyFitness(c.pub, now)
		if a != c.advertise || s != c.sign {
			t.Errorf("%s: advertise=%v sign=%v, want %v %v", c.name, a, s, c.advertise, c.sign)
		}
	}
}
