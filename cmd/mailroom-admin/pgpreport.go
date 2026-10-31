// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/pgpmail"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `pgp-report` — how much of this mailbox is signed or encrypted at all (#47,
// step 0). Read-only; there is no --apply because there is nothing to apply.
//
// WHY THIS RUNS BEFORE ANY CRYPTO IS WRITTEN. #47's plan sequences a
// measurement first: whether the first useful capability is inbound
// verification/decryption or outbound signing and key publication (#48) depends
// on whether anyone actually sends this mailbox encrypted mail. It also feeds
// the S/MIME go/no-go (spec §18 item 6), which is why S/MIME is counted
// separately throughout rather than folded into an encryption total.
//
// ⚠ THE EXPECTED ANSWER IS ZERO, and that is precisely why the DETECTOR is
// tested rather than trusted. A report whose expected output is zero cannot
// distinguish "no encrypted mail" from "the detector never fires" — this
// project shipped exactly that mistake once, on the day this was written
// (`suggest-report` printed "0 reached" from a filter that made zero the only
// possible answer). So `internal/pgpmail` carries positive controls for every
// signal, each probed into failure, plus a negative control that ordinary mail
// detects nothing. A zero here therefore CONFIRMS the operator's expectation
// instead of merely agreeing with it.
func runPGPReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
		showPeers = fs.Int("show-peers", 15, "how many encryption-capable correspondents to list")
		keysOnly  = fs.Bool("keys-only", false, "print only the key-expiry section (#795); skip the message scan")
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

	if *keysOnly {
		return reportKeyExpiry(ctx, d.store, userID, time.Now())
	}

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

	var (
		scanned, parseFailed, blobFailed int
		withAny, withPGP, withSMIME      int
	)
	// Per SIGNAL, never one total (CLAUDE.md). "Some encryption somewhere" and
	// "one correspondent publishes an Autocrypt key" call for entirely different
	// next steps, and a combined count cannot tell them apart.
	count := map[string]int{}
	// Correspondents whose mail shows they could receive encrypted replies —
	// the population #48 (WKD/Autocrypt) would actually serve.
	peers := map[string]int{}

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
				blobFailed++
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil || msg == nil {
				parseFailed++
				continue
			}

			s := pgpmail.DetectWithNested(msg)
			if !s.Any() {
				continue
			}
			withAny++
			if s.AnyPGP() {
				withPGP++
			}
			if s.AnySMIME() {
				withSMIME++
			}
			for name, on := range map[string]bool{
				"pgpmime_signed":    s.PGPMIMESigned,
				"pgpmime_encrypted": s.PGPMIMEEncrypted,
				"inline_signed":     s.PGPInlineSigned,
				"inline_message":    s.PGPInlineMessage,
				"public_key":        s.PGPPublicKey,
				"autocrypt":         s.Autocrypt,
				"autocrypt_gossip":  s.AutocryptGossip,
				"smime_signed":      s.SMIMESigned,
				"smime_encrypted":   s.SMIMEEncrypted,
			} {
				if on {
					count[name]++
				}
			}
			if s.Autocrypt || s.PGPPublicKey || s.PGPMIMESigned || s.PGPInlineSigned {
				if from := msg.Header.Get("From"); from != "" {
					peers[from]++
				}
			}
		}
	}

	// ⚠ FAILURES ARE REPORTED SEPARATELY, not folded into "no apparatus". A
	// message we could not read or parse is not evidence of absence, and
	// silently counting it as clean is how a scan that read nothing reports a
	// clean mailbox.
	fmt.Printf("scanned  : %d messages\n", scanned)
	fmt.Printf("unread   : %d blob failures, %d parse failures  (NOT counted as clean)\n",
		blobFailed, parseFailed)
	fmt.Printf("examined : %d\n\n", scanned-blobFailed-parseFailed)

	fmt.Printf("%-20s %8s\n", "SIGNAL", "MESSAGES")
	for _, name := range []string{
		"pgpmime_signed", "pgpmime_encrypted", "inline_signed", "inline_message",
		"public_key", "autocrypt", "autocrypt_gossip", "smime_signed", "smime_encrypted",
	} {
		fmt.Printf("%-20s %8d\n", name, count[name])
	}

	fmt.Printf("\nmessages with ANY apparatus : %d\n", withAny)
	fmt.Printf("  of which PGP-family       : %d\n", withPGP)
	fmt.Printf("  of which S/MIME           : %d\n", withSMIME)

	if len(peers) > 0 {
		fmt.Printf("\nencryption-capable correspondents (%d), most mail first:\n", len(peers))
		type peer struct {
			addr string
			n    int
		}
		list := make([]peer, 0, len(peers))
		for a, n := range peers {
			list = append(list, peer{a, n})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].n != list[j].n {
				return list[i].n > list[j].n
			}
			return list[i].addr < list[j].addr
		})
		shown := *showPeers
		if shown > len(list) {
			shown = len(list)
		}
		for _, p := range list[:shown] {
			fmt.Printf("  %-52s %4d\n", truncate(p.addr, 52), p.n)
		}
		if len(list) > shown {
			fmt.Printf("  … and %d more (raise --show-peers)\n", len(list)-shown)
		}
	}

	// WHAT THE NUMBER MEANS FOR THE PLAN, stated here rather than left for a
	// reader to infer — the whole purpose of running this before building.
	fmt.Printf("\n")
	switch {
	case withAny == 0 && scanned-blobFailed-parseFailed == 0:
		fmt.Printf("⚠ NOTHING WAS EXAMINED — every message failed to read or parse. This is not\n")
		fmt.Printf("  a finding about encryption; it is a broken scan. Check the blob store and\n")
		fmt.Printf("  the mail DEK before reading anything else above.\n")
	case withAny == 0:
		fmt.Printf("No signing or encryption apparatus anywhere in this mailbox.\n\n")
		fmt.Printf("  This is a real zero, not an unexercised detector: internal/pgpmail carries\n")
		fmt.Printf("  a positive control for every signal above, each probed into failure, plus a\n")
		fmt.Printf("  negative control that ordinary mail detects nothing.\n\n")
		fmt.Printf("  For #47 it says the first useful capability is OUTBOUND — signing and key\n")
		fmt.Printf("  publication (#48) — not inbound verification or decryption, because there\n")
		fmt.Printf("  is nothing inbound to verify or decrypt. Increments 4 and 5 should swap.\n")
		fmt.Printf("  For the S/MIME go/no-go it says DEFER: not one correspondent uses it.\n")
	case withSMIME == 0:
		fmt.Printf("PGP apparatus is present and S/MIME is entirely absent — the go/no-go for\n")
		fmt.Printf("S/MIME (spec §18 item 6) has its answer from this mailbox: defer.\n")
	default:
		fmt.Printf("Both PGP and S/MIME appear. The S/MIME go/no-go cannot be settled from this\n")
		fmt.Printf("mailbox alone — %d message(s) would be unreadable without it.\n", withSMIME)
	}
	return reportKeyExpiry(ctx, d.store, userID, time.Now())
}
