// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpkeys"
)

func expiryTestKey(t *testing.T, created time.Time, lifetime time.Duration) []byte {
	t.Helper()
	e, err := openpgp.NewEntity("R", "", "r@example.com", &packet.Config{
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

// TestCountExpiry: every key lands in exactly one bucket or in unreadable, and
// examined is the bound the buckets add up to — so a zero bucket can be read
// against it.
func TestCountExpiry(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	keys := [][]byte{
		expiryTestKey(t, now.Add(-day), 0),         // never
		expiryTestKey(t, now.Add(-40*day), 30*day), // expired
		expiryTestKey(t, now.Add(-day), 10*day),    // <30d
		expiryTestKey(t, now.Add(-day), 60*day),    // <90d
		expiryTestKey(t, now.Add(-day), 2*365*day), // later
		{0x99, 0x01, 0x02},                         // unreadable
	}
	c := countExpiry(keys, now)
	want := map[string]int{
		pgpkeys.BucketNever: 1, pgpkeys.BucketExpired: 1, pgpkeys.BucketUnder30: 1,
		pgpkeys.BucketUnder90: 1, pgpkeys.BucketLater: 1,
	}
	for b, n := range want {
		if c.buckets[b] != n {
			t.Errorf("bucket %s = %d, want %d", b, c.buckets[b], n)
		}
	}
	if c.unreadable != 1 || c.examined != len(keys) {
		t.Errorf("unreadable=%d examined=%d, want 1 and %d", c.unreadable, c.examined, len(keys))
	}
	sum := c.unreadable
	for _, n := range c.buckets {
		sum += n
	}
	if sum != c.examined {
		t.Errorf("buckets + unreadable = %d, examined = %d: a key was dropped or double-counted", sum, c.examined)
	}
}
