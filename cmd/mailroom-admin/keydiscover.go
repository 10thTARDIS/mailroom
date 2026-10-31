// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/egress"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// `mailroom-admin key-discover` — look up every contact's address over WKD
// (#48) and report what public keys are actually out there, storing NEW ones
// in app.contact_keys only when asked.
//
// READ-ONLY BY DEFAULT (CLAUDE.md: build the report before the fix, same as
// quarantine.go). Without --apply this performs every lookup and reports what
// it WOULD store; nothing is written. --apply is the opt-in mutation, exactly
// as it is for --release on quarantine.go.
//
// ⚠ EVERY FETCH GOES THROUGH internal/egress, WITHOUT EXCEPTION. A WKD host
// name is derived from a correspondent's email address — chosen by whoever
// wrote to this mailbox, not by us — which is exactly the SSRF shape egress
// was built to guard against (#55). There is no other path to the network in
// this file, and there must never be one.
//
// The report is per-category counts, never one total (#177/#191/#206): a
// mailbox with no PGP correspondents is EXPECTED to print a run of zeros, and
// that is the finding, not evidence the fetcher never ran. "addresses tried"
// is always printed alongside the rest, so a zero has a denominator.
func runKeyDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("key-discover", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user", "", "user id (ULID) — REQUIRED")
		apply     = fs.Bool("apply", false, "store newly discovered keys in app.contact_keys (default: report only)")
		limit     = fs.Int("limit", 200, "maximum number of addresses to look up in one run "+
			"(politeness: a sweep over thousands of contacts must not look like a scan)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// ⚠ --user IS REQUIRED, same reasoning as quarantine.go: this command can
	// mutate app.contact_keys with --apply, and guessing the user or silently
	// sweeping every user's contacts is not an acceptable default for
	// something that reaches out to other people's mail servers on their
	// behalf.
	if strings.TrimSpace(*userIDStr) == "" {
		return errors.New("key-discover: --user is required (ULID)")
	}
	userID, err := ulid.Parse(*userIDStr)
	if err != nil {
		return fmt.Errorf("key-discover: --user %q: not a valid ULID: %w", *userIDStr, err)
	}
	if *limit <= 0 {
		return errors.New("key-discover: --limit must be positive")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	// Step 1: everything the DATABASE already knows, gathered in ONE short
	// transaction that is closed before any network call. A WKD host can be
	// slow, unreachable, or simply hostile; none of that should hold a
	// database transaction — or the RLS session it carries — open.
	var (
		contacts []store.Contact
		// existingKeys maps contact id -> the fingerprints already held for it,
		// so "already held" vs "NEW" is a read-only comparison even when this
		// whole run is a dry run.
		existingKeys = map[ulid.ULID]map[string]bool{}
		beforeCounts map[string]int
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		contacts, e = tx.ListContacts(ctx)
		if e != nil {
			return e
		}
		beforeCounts, e = tx.CountContactKeys(ctx)
		if e != nil {
			return e
		}
		for _, c := range contacts {
			held, e := tx.ContactKeysFor(ctx, c.ID)
			if e != nil {
				return e
			}
			set := make(map[string]bool, len(held))
			for _, k := range held {
				set[k.Fingerprint] = true
			}
			existingKeys[c.ID] = set
		}
		return nil
	}); err != nil {
		return fmt.Errorf("key-discover: read contacts: %w", err)
	}

	fmt.Printf("key-discover: user %s\n", userID)
	if *apply {
		fmt.Println("mode: --apply — NEW keys WILL be stored in app.contact_keys")
	} else {
		fmt.Println("mode: report only — pass --apply to store NEW keys")
	}
	if len(beforeCounts) == 0 {
		fmt.Println("currently held contact keys: none, from any source")
	} else {
		fmt.Println("currently held contact keys, by source:")
		printSortedCounts(beforeCounts)
	}

	// Step 2: the network, through ONE egress.Client for the whole run — the
	// same SSRF guard that fetches attacker-chosen image URLs (#55). https
	// only (the zero-valued Schemes default), a short per-request timeout, and
	// a byte cap matching pgpkeys.MaxKeyBytes as a second, transport-level
	// backstop to internal/wkd's own cap.
	client := egress.New(egress.Policy{
		Timeout:  10 * time.Second,
		MaxBytes: pgpkeys.MaxKeyBytes,
	})
	fetcher := wkd.NewFetcher(client, d.log)

	var (
		contactsExamined int
		addressesTried   int
		keyFound         int
		noKeyPublished   int
		fetchFailed      int
		keyRejected      int
		alreadyHeld      int
		newlyFound       int
		coversAddress    int
		coverUnknown     int
	)

	fmt.Println("\nfetching...")
addrLoop:
	for _, c := range contacts {
		if len(c.Addresses) == 0 {
			continue
		}
		contactsExamined++
		for _, ca := range c.Addresses {
			if addressesTried >= *limit {
				break addrLoop
			}
			if ctx.Err() != nil {
				break addrLoop
			}
			addressesTried++
			addr := ca.Address

			raw, srcURL, ferr := fetcher.Fetch(ctx, addr)
			switch {
			case ferr == nil:
				keyFound++
			case errors.Is(ferr, wkd.ErrNoKey):
				noKeyPublished++
				sleepPolitely(ctx)
				continue
			default:
				fetchFailed++
				fmt.Printf("  fetch failed   %-40s %v\n", addr, ferr)
				sleepPolitely(ctx)
				continue
			}

			key, perr := pgpkeys.Parse(raw)
			if perr != nil {
				keyRejected++
				fmt.Printf("  key rejected   %-40s %v (from %s)\n", addr, perr, srcURL)
				sleepPolitely(ctx)
				continue
			}

			// ⚠ ADVISORY ONLY (pgpkeys.CoversAddress' own doc comment): a real
			// key is allowed to carry no UID for this address at all, so this
			// is reported alongside the outcome, never used to reject it.
			if key.CoversAddress(addr) {
				coversAddress++
			} else {
				coverUnknown++
			}

			if existingKeys[c.ID][key.Fingerprint] {
				alreadyHeld++
				sleepPolitely(ctx)
				continue
			}
			newlyFound++
			fmt.Printf("  NEW            %-40s fingerprint %s (from %s)\n", addr, key.Fingerprint, srcURL)

			if *apply {
				var created bool
				if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
					var e error
					created, e = tx.UpsertContactKey(ctx, store.ContactKeyRow{
						ContactID:   c.ID,
						Kind:        store.KeyPGP,
						Fingerprint: key.Fingerprint,
						PublicKey:   raw,
						Source:      store.ContactKeyWKD,
					})
					return e
				}); err != nil {
					return fmt.Errorf("key-discover: store key for %s: %w", addr, err)
				}
				if created && existingKeys[c.ID] != nil {
					existingKeys[c.ID][key.Fingerprint] = true
				}
			}
			sleepPolitely(ctx)
		}
	}

	fmt.Println("\ncontacts examined         ", contactsExamined)
	fmt.Println("addresses tried           ", addressesTried)
	fmt.Println("  key found               ", keyFound)
	fmt.Println("  no key published        ", noKeyPublished, "  (404 on both paths — the ordinary case)")
	fmt.Println("  fetch failed            ", fetchFailed, "  (DNS, TLS, timeout, refused by the egress guard)")
	fmt.Println("  key rejected            ", keyRejected, "  (unparseable, armored, or carrying private material)")
	fmt.Println("  already held            ", alreadyHeld)
	fmt.Println("  NEW                     ", newlyFound)

	if parsed := coversAddress + coverUnknown; parsed > 0 {
		fmt.Printf("\nof %d parsed key(s): %d carried a UID covering the fetched address, %d did not "+
			"(advisory only — CoversAddress is not a rejection)\n", parsed, coversAddress, coverUnknown)
	}

	switch {
	case addressesTried == 0:
		fmt.Println("\n⚠ ZERO ADDRESSES TRIED — this user has no contacts with a known address " +
			"(or every contact was already past --limit). Every count above is trivially zero because " +
			"nothing was attempted; that is NOT the same finding as \"nobody publishes a key\", so do " +
			"not read it that way.")
	case keyFound == 0:
		fmt.Printf("\n⚠ ZERO KEYS FOUND across %d address(es) actually tried. On a mailbox with few or "+
			"no PGP correspondents this IS the expected result — most people do not publish a WKD key "+
			"— but a zero here cannot by itself distinguish that from a fetcher that never fires. If "+
			"this is unexpected, verify against a domain known to publish one before trusting it.\n",
			addressesTried)
	}

	if !*apply && newlyFound > 0 {
		fmt.Printf("\n%d new key(s) found but NOT stored (report-only run) — rerun with --apply to "+
			"store them.\n", newlyFound)
	}

	return nil
}

// sleepPolitely pauses briefly between WKD requests so a sweep over many
// contacts does not look like a scan to servers on the other end (this
// command's own ⚠, spec'd alongside --limit). It returns early on context
// cancellation rather than blocking a shutdown.
func sleepPolitely(ctx context.Context) {
	t := time.NewTimer(200 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// printSortedCounts prints a map[string]int sorted by key, for output that
// does not reorder itself between runs.
func printSortedCounts(m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-12s %d\n", k, m[k])
	}
}
