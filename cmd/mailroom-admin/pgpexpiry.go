// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
)

// The expiry section of `pgp-report` (#795: report first, so the warning
// threshold is chosen on real data). Read-only.
//
// "Usable until" is pgpkeys.Validity.UsableUntil: the EARLIER of the primary
// key's expiry and the encryption subkey's, because a subkey can expire first
// and it is the subkey a correspondent encrypts to. Revoked keys and keys with
// no encryption subkey get their own buckets rather than being read as expired.
//
// Per bucket and per population (own vs held), never one total, and the number
// EXAMINED is printed beside them: "0 keys expiring" means nothing until you
// know whether it was 0 of 0 or 0 of 40. A key whose bytes cannot be read is
// counted as unreadable, not dropped.

// expiryCounts is one population's tally.
type expiryCounts struct {
	examined   int
	unreadable int
	buckets    map[string]int
}

func countExpiry(keys [][]byte, now time.Time) expiryCounts {
	c := expiryCounts{buckets: map[string]int{}}
	for _, k := range keys {
		c.examined++
		v, err := pgpkeys.Assess(k, now)
		if err != nil {
			c.unreadable++
			continue
		}
		c.buckets[pgpkeys.ExpiryBucket(v, now)]++
	}
	return c
}

// reportKeyExpiry prints the expiry section for one user.
func reportKeyExpiry(ctx context.Context, st *store.Store, userID ulid.ULID, now time.Time) error {
	var (
		own     []store.UserKey
		contact []store.ContactKey
	)
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if own, e = tx.ListUserKeys(ctx); e != nil {
			return e
		}
		contact, e = tx.AllContactKeys(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("read keys: %w", err)
	}

	var ownAll, ownLive, held [][]byte
	retired, revokedRows := 0, 0
	for _, k := range own {
		ownAll = append(ownAll, k.PublicKey)
		switch {
		case k.RevokedAt != nil:
			revokedRows++
		case k.DecryptOnly:
			retired++
		default:
			ownLive = append(ownLive, k.PublicKey)
		}
	}
	for _, k := range contact {
		held = append(held, k.PublicKey)
	}
	cols := []expiryCounts{countExpiry(ownAll, now), countExpiry(ownLive, now), countExpiry(held, now)}

	fmt.Printf("\nKEY EXPIRY (#795)  at %s; warning window %d days\n",
		now.UTC().Format(time.RFC3339), int(pgpkeys.ExpiryWarningWindow/(24*time.Hour)))
	fmt.Printf("  usable until = earliest of the primary key's and the encryption subkey's expiry\n\n")
	fmt.Printf("%-20s %10s %10s %10s\n", "BUCKET", "OWN (all)", "OWN (live)", "HELD")
	for _, b := range pgpkeys.ExpiryBuckets {
		fmt.Printf("%-20s %10d %10d %10d\n", b, cols[0].buckets[b], cols[1].buckets[b], cols[2].buckets[b])
	}
	fmt.Printf("%-20s %10d %10d %10d\n", "unreadable", cols[0].unreadable, cols[1].unreadable, cols[2].unreadable)
	fmt.Printf("%-20s %10d %10d %10d\n", "EXAMINED", cols[0].examined, cols[1].examined, cols[2].examined)
	fmt.Printf("\n  OWN (live) excludes %d retired (decrypt-only) and %d revoked key(s).\n", retired, revokedRows)
	fmt.Printf("  HELD is every correspondent key stored, refused ones included (%d).\n", len(contact))
	if cols[0].examined+cols[2].examined == 0 {
		fmt.Printf("  ⚠ No keys at all: this says nothing about expiry, only that none are held.\n")
	}

	if len(own) > 0 {
		fmt.Printf("\n  own keys:\n")
		for _, k := range own {
			fmt.Printf("    %s  fpr=%s  expires=%s\n", k.ID, k.Fingerprint, keyExpiryText(k.PublicKey))
		}
	}
	return nil
}

// keyExpiryText reads a stored binary public key's end of usability, for
// pgp-list. A key that cannot be read says so rather than guessing.
func keyExpiryText(pub []byte) string {
	v, err := pgpkeys.Assess(pub, time.Now())
	if err != nil {
		return "unreadable"
	}
	if v.Revoked {
		return "revoked"
	}
	until, never := v.UsableUntil()
	if never {
		return "never"
	}
	s := until.UTC().Format(time.RFC3339)
	if !time.Now().Before(until) {
		s += "(EXPIRED)"
	}
	return s
}
