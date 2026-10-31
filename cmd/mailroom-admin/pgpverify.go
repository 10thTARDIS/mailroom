// SPDX-License-Identifier: AGPL-3.0-only

package main

// `pgp-verify` — do the OpenPGP signatures in this mailbox actually check out
// (#47 increment 6a)?
//
// # Why this exists before anything that USES a verdict
//
// `pgp-report` (step 0) detects the multipart/signed STRUCTURE. It cannot say
// whether a signature is valid, and after increment 5 that gap became load
// bearing: signing was shipped on the claim that quoted-printable bodies
// survive a 7-bit hop, and the only evidence for it was unit tests plus a
// structural detection on round-tripped mail. "There is a signature-shaped
// thing here" is not "the signature checks out".
//
// So this is the measurement that closes increment 5, and it is READ-ONLY. It
// writes nothing, stores no verdict, and changes no behaviour — per the
// report-before-the-fix rule. What a verdict should DO (show a badge, gate a
// display, feed triage) is a separate decision that wants this number first.
//
// ⚠ WHAT A `verified` COUNT DOES NOT MEAN. It says the bytes match the
// signature and names the key. It does NOT say that key belongs to whoever the
// From header claims — that is key discovery and trust, which does not exist
// yet. A report that implied otherwise would be telling the operator a forgery
// was fine.

import (
	"context"
	"flag"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpverify"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

func runPGPVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-verify", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
		showEach  = fs.Int("show", 20, "how many individual verdicts to list")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	// The PUBLIC halves only. Verification never needs a private key, so this
	// command never asks the crypto process for anything and never holds key
	// material that matters (ADR-024).
	var keys []pgpverify.PublicKey
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		rows, e := tx.ListUserKeys(ctx)
		if e != nil {
			return e
		}
		for _, k := range rows {
			if len(k.PublicKey) == 0 {
				continue
			}
			keys = append(keys, pgpverify.PublicKey{
				Fingerprint: k.Fingerprint, Binary: k.PublicKey,
			})
		}
		return nil
	}); err != nil {
		return fmt.Errorf("load public keys: %w", err)
	}

	var (
		scanned, blobFailed int
		verdicts            = map[pgpverify.Verdict]int{}
		samples             []string
		badSignatures       []string
	)

	after := ulid.ULID{}
	for {
		var refs []store.MessageBlobRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.MessageBlobRefsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan messages: %w", err)
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			after = ref.ID
			scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// ⚠ Counted on its own line, never as "clean". Not being able to
				// look at a message is not the same as having looked and found
				// nothing (CLAUDE.md).
				blobFailed++
				continue
			}

			res := pgpverify.Verify(raw, keys)
			verdicts[res.Verdict]++
			if res.Verdict == pgpverify.VerdictNotSigned {
				continue
			}
			line := fmt.Sprintf("  %s  %-14s %s", ref.ID, res.Verdict, describeVerdict(res))
			if len(samples) < *showEach {
				samples = append(samples, line)
			}
			if res.Verdict == pgpverify.VerdictBadSignature {
				badSignatures = append(badSignatures, line)
			}
		}
	}

	signed := scanned - verdicts[pgpverify.VerdictNotSigned] - blobFailed

	fmt.Printf("scanned  : %d messages\n", scanned)
	fmt.Printf("unread   : %d blob failures  (NOT counted as unsigned)\n", blobFailed)
	fmt.Printf("keys held: %d public key(s) to check against\n\n", len(keys))

	fmt.Printf("%-20s %s\n", "VERDICT", "MESSAGES")
	for _, v := range []pgpverify.Verdict{
		pgpverify.VerdictVerified,
		pgpverify.VerdictBadSignature,
		pgpverify.VerdictNoKey,
		pgpverify.VerdictMalformed,
		pgpverify.VerdictNotSigned,
	} {
		fmt.Printf("%-20s %8d\n", v, verdicts[v])
	}
	fmt.Printf("\nmessages carrying a PGP signature : %d\n", signed)

	if len(samples) > 0 {
		fmt.Printf("\nsigned messages (%d shown):\n", len(samples))
		sort.Strings(samples)
		for _, s := range samples {
			fmt.Println(s)
		}
	}

	// ⚠ The one verdict that means something is wrong gets its own section, so
	// it cannot be lost in a column of numbers.
	if len(badSignatures) > 0 {
		fmt.Printf("\n⚠ %d BAD SIGNATURE(S) — the content does not match the signature.\n",
			len(badSignatures))
		fmt.Println("  Either the mail was altered after it was signed, or it was never signed")
		fmt.Println("  by the key it names. Both are worth chasing.")
		for _, s := range badSignatures {
			fmt.Println(s)
		}
	}

	fmt.Println()
	switch {
	case signed == 0:
		fmt.Println("No PGP-signed mail in this mailbox, so this report proves nothing about")
		fmt.Println("verification either way — it has not been exercised. Send yourself a signed")
		fmt.Println("message and run it again.")
	case verdicts[pgpverify.VerdictVerified] > 0 && verdicts[pgpverify.VerdictBadSignature] == 0:
		fmt.Println("Signatures check out over the bytes as they arrived — which is the property")
		fmt.Println("increment 5's quoted-printable work exists to protect, measured on real mail")
		fmt.Println("rather than inferred from the structure being present.")
	}
	if verdicts[pgpverify.VerdictNoKey] > 0 {
		fmt.Printf("\n%d signature(s) came from a key this mailbox does not hold. That is EXPECTED\n",
			verdicts[pgpverify.VerdictNoKey])
		fmt.Println("for every correspondent: key discovery (WKD lookup, Autocrypt) does not exist")
		fmt.Println("yet, so only mail signed by your own keys can be checked today.")
	}
	fmt.Println("\nread-only: nothing was written, and no verdict is stored.")
	return nil
}

func describeVerdict(res pgpverify.Result) string {
	switch {
	case res.Verdict == pgpverify.VerdictVerified:
		return "signed by " + res.SignerFingerprint + " (" + res.MicAlg + ")"
	case res.Err != nil:
		return res.Err.Error()
	default:
		return ""
	}
}
