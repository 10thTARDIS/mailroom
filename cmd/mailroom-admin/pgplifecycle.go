// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

// `pgp-retire` / `pgp-extend` / `pgp-revoke`: what happens to one of the user's
// own keys after it is made (#795, ADR-040).
//
//   - retire is a STORE write, like pgp-publish: it flips decrypt_only and
//     wkd_published and touches no key material, so it does not need the crypto
//     process. The key keeps decrypting forever (ADR-037).
//   - extend and revoke produce NEW key material — re-signed self-signatures, a
//     revocation signature — which needs the private half, so they go through
//     the crypto process like pgp-generate.
//
// Retire vs revoke, in one line each: retire says "I have stopped using this
// key" to Mailroom; revoke says "nobody should use this key" to the world, and
// cannot be taken back.

// usableUntilText renders KeyInfo.UsableUntil for the operator.
func usableUntilText(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// runPGPRetire makes a key decrypt-only and withdraws it from WKD (#795).
func runPGPRetire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-retire", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	keyIDStr := fs.String("key-id", "", "key id (ULID) to retire, from pgp-list")
	unbind := fs.Bool("unbind", false,
		"also clear it as an identity's default signing key (otherwise a bound default is refused)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	keyID, err := parseULIDFlag("key-id", *keyIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var res store.RetireResult
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		res, e = tx.RetireUserKey(ctx, keyID, *unbind)
		return e
	}); err != nil {
		if errors.Is(err, store.ErrKeyIsDefault) {
			return fmt.Errorf("pgp-retire: %s", strings.TrimPrefix(err.Error(), "store: "))
		}
		return fmt.Errorf("pgp-retire: %w", err)
	}
	if !res.Found {
		return fmt.Errorf("pgp-retire: no key %s for user %s (pgp-list shows this user's keys)", keyID, userID)
	}
	if !res.Changed {
		fmt.Printf("key %s was already retired, unpublished and unbound; nothing changed\n", keyID)
		return nil
	}
	fmt.Printf("key %s retired: decrypt-only from now on — it will never sign or be published again,\n", keyID)
	fmt.Printf("  and it keeps decrypting mail already encrypted to it.\n")
	if res.Unpublished {
		fmt.Printf("  withdrawn from WKD.\n")
	}
	for _, id := range res.Unbound {
		fmt.Printf("  no longer the default key of identity %s — its mail now goes out unsigned until\n", id)
		fmt.Printf("  another key is bound (pgp-bind).\n")
	}
	fmt.Printf("  retiring does not tell correspondents anything; if the key must not be used by\n")
	fmt.Printf("  ANYONE (it was compromised), revoke it instead: pgp-revoke.\n")
	return nil
}

// runPGPExtend re-signs a key with a later expiry (#795).
func runPGPExtend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-extend", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	keyIDStr := fs.String("key-id", "", "key id (ULID) to extend, from pgp-list")
	days := fs.Int("days", 0, fmt.Sprintf("new expiry, in days from now (1..%d)", pgpwire.MaxKeyLifetimeDays))
	socket := addSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	keyID, err := parseULIDFlag("key-id", *keyIDStr)
	if err != nil {
		return err
	}
	if *days <= 0 || *days > pgpwire.MaxKeyLifetimeDays {
		return fmt.Errorf("pgp-extend: --days must be between 1 and %d", pgpwire.MaxKeyLifetimeDays)
	}

	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	client, err := cryptoClient(callCtx, pgpSocketPath(*socket))
	if err != nil {
		return err
	}
	resp, err := client.Extend(callCtx, pgpwire.ExtendRequest{UserID: userID.String(), KeyID: keyID.String(), Days: *days})
	if err != nil {
		return wireCallError("pgp-extend", err)
	}
	fmt.Printf("key %s extended\n", resp.Key.KeyID)
	fmt.Printf("  fingerprint: %s (unchanged — correspondents will not see a new key)\n", resp.Key.Fingerprint)
	fmt.Printf("  usable until: %s\n", usableUntilText(resp.Key.UsableUntil))
	fmt.Printf("  correspondents pick the renewal up from your next Autocrypt header or a WKD\n")
	fmt.Printf("  refresh; until they do, their copy still ends on the old date.\n")
	return nil
}

// runPGPRevoke revokes a key (#795). Irreversible.
func runPGPRevoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-revoke", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	keyIDStr := fs.String("key-id", "", "key id (ULID) to revoke, from pgp-list")
	reason := fs.String("reason", "", "compromised | superseded | retired")
	confirm := fs.String("confirm", "", "the last 8 hex digits of the key's fingerprint (pgp-list shows it)")
	printPublic := fs.Bool("print-public", false, "print the revoked public key (armored) to hand to correspondents")
	socket := addSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	keyID, err := parseULIDFlag("key-id", *keyIDStr)
	if err != nil {
		return err
	}
	switch *reason {
	case pgpwire.RevokeCompromised, pgpwire.RevokeSuperseded, pgpwire.RevokeRetired:
	default:
		return fmt.Errorf("pgp-revoke: --reason must be %s, %s or %s",
			pgpwire.RevokeCompromised, pgpwire.RevokeSuperseded, pgpwire.RevokeRetired)
	}
	if len(strings.TrimSpace(*confirm)) != 8 {
		return errors.New("pgp-revoke: --confirm must be the last 8 hex digits of the key's fingerprint; " +
			"revocation cannot be undone, so the key is named twice")
	}

	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	client, err := cryptoClient(callCtx, pgpSocketPath(*socket))
	if err != nil {
		return err
	}
	resp, err := client.Revoke(callCtx, pgpwire.RevokeRequest{
		UserID: userID.String(), KeyID: keyID.String(), Reason: *reason, Confirm: *confirm,
	})
	if err != nil {
		return wireCallError("pgp-revoke", err)
	}
	fmt.Printf("key %s REVOKED (%s) at %s\n", resp.Key.KeyID, *reason, resp.RevokedAt.UTC().Format(time.RFC3339))
	fmt.Printf("  fingerprint: %s\n", resp.Key.Fingerprint)
	fmt.Printf("  it will never sign again; it still decrypts mail already encrypted to it.\n")
	fmt.Printf("  if it was published in WKD, WKD now serves the revocation (after any current\n")
	fmt.Printf("  key at that address), so correspondents who refresh learn of it. Others only\n")
	fmt.Printf("  learn if they are sent the revoked key (--print-public prints it).\n")
	if *printPublic {
		fmt.Println()
		fmt.Println(resp.Key.PublicKey)
	}
	return nil
}
