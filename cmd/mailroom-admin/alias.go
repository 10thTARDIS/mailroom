// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/alias"
	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// THE OPERATOR PROVISIONING PATH FOR A CATCHALL (mailroom#177).
//
// A domain_wildcard identity says "every address at this domain is mine", and
// the whole From-verification boundary (spec §5) rests on that being true. The
// JMAP API therefore REFUSES to create one (internal/api/identity.go) — a client
// asserting domain ownership over the wire would make the check meaningless —
// and that refusal is correct and stays. The gap it leaves is that there was no
// path at all, so a catchall user's alias story was inert: every message resolved
// to the one fixed address they had, and their own aliases piled up in the
// address book as though they were correspondents.
//
// This file closes that gap where the authority actually lives: the operator, at
// the host, holding the database credentials and the master key. Provisioning
// here is not a weaker version of the API check — it is the evidence the API
// cannot have.
//
// All three commands are DRY-RUN BY DEFAULT and idempotent under --apply, so an
// operator can look before writing and re-run without compounding anything.

func runProvisionIdentity(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("provision-identity", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		domain    = fs.String("domain", "", "verify a whole domain (catchall): every address at it becomes sendable")
		address   = fs.String("address", "", "verify one exact address")
		accountID = fs.String("account-id", "", "account the identity submits through (default: the user's only account)")
		name      = fs.String("display-name", "", "display name for outgoing mail")
		apply     = fs.Bool("apply", false, "create the identity (default: dry-run)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	dom := strings.ToLower(strings.TrimSpace(*domain))
	addr := store.NormalizeAddress(*address)
	switch {
	case dom == "" && addr == "":
		return errors.New("provision-identity: one of --domain or --address is required")
	case dom != "" && addr != "":
		return errors.New("provision-identity: --domain and --address are mutually exclusive")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	row := store.IdentityRow{Kind: store.IdentityFixed, Address: addr, DisplayName: *name, Enabled: true}
	if dom != "" {
		row = store.IdentityRow{Kind: store.IdentityWildcard, Domain: dom, DisplayName: *name, Enabled: true}
	}

	return d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		acct, err := resolveIdentityAccount(ctx, tx, *accountID)
		if err != nil {
			return err
		}
		row.AccountID = acct

		existing, err := tx.ListIdentities(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("existing identities (%d):\n", len(existing))
		for _, i := range existing {
			fmt.Printf("  %s  %-28s kind=%-15s enabled=%t\n", i.ID, i.Email(), i.Kind, i.Enabled)
		}
		for _, i := range existing {
			if i.Kind == row.Kind &&
				(row.Kind == store.IdentityWildcard && i.Domain == dom ||
					row.Kind == store.IdentityFixed && i.Address == addr) {
				fmt.Printf("\nalready provisioned: %s (%s) — nothing to do\n", i.Email(), i.ID)
				return nil
			}
		}

		if row.Kind == store.IdentityWildcard {
			// Say plainly what is being asserted. An operator who does not in fact
			// control the domain is about to authorize their users to send as every
			// address at it, and no later check will catch that.
			fmt.Printf("\nWILDCARD (catchall) identity for *@%s\n", dom)
			fmt.Printf("  This ATTESTS THAT THE USER OWNS %s. It makes every address at\n", strings.ToUpper(dom))
			fmt.Printf("  that domain a verified send-as address for them, and excludes every\n")
			fmt.Printf("  such address from their address book (they are the user's own aliases,\n")
			fmt.Printf("  not correspondents). Provision it only if that is true.\n")
		} else {
			fmt.Printf("\nFIXED identity for %s\n", addr)
		}
		fmt.Printf("  account: %s\n", acct)

		if !*apply {
			fmt.Printf("\nDRY RUN — nothing written. Re-run with --apply to create it.\n")
			return nil
		}
		id, err := tx.CreateIdentity(ctx, row)
		if err != nil {
			return err
		}
		fmt.Printf("\ncreated identity %s\n", id)
		if row.Kind == store.IdentityWildcard {
			fmt.Printf("Existing mail still records the alias resolved at INGEST time. Run\n")
			fmt.Printf("  mailroom-admin backfill-alias --user-id %s\n", userID)
			fmt.Printf("to see what would change, and repair-contacts to clear own-address contacts.\n")
		}
		return nil
	})
}

// resolveIdentityAccount picks the account an identity submits through: the one
// named, or the user's only account. It refuses to guess between several — an
// identity on the wrong account sends through the wrong SMTP credentials.
func resolveIdentityAccount(ctx context.Context, tx *store.UserTx, want string) (ulid.ULID, error) {
	accts, err := tx.ListAccounts(ctx)
	if err != nil {
		return ulid.ULID{}, err
	}
	if want != "" {
		id, err := ulid.Parse(strings.TrimSpace(want))
		if err != nil {
			return ulid.ULID{}, fmt.Errorf("--account-id %q: not a valid ULID: %w", want, err)
		}
		for _, a := range accts {
			if a.ID == id {
				return id, nil
			}
		}
		return ulid.ULID{}, fmt.Errorf("--account-id %s is not an account of this user", id)
	}
	switch len(accts) {
	case 0:
		return ulid.ULID{}, errors.New("this user has no accounts; add one first (add-account)")
	case 1:
		return accts[0].ID, nil
	default:
		var names []string
		for _, a := range accts {
			names = append(names, fmt.Sprintf("%s (%s)", a.ID, a.Name))
		}
		return ulid.ULID{}, fmt.Errorf("this user has %d accounts; pass --account-id: %s",
			len(accts), strings.Join(names, ", "))
	}
}

// runBackfillAlias re-derives `addressed_address` over already-stored mail.
//
// THE DRY RUN IS THE POINT, and it is a DIAGNOSTIC, not a preview. The alias is
// resolved at ingest against the identities that existed then, so a catchall
// user's whole mailbox records the one fixed address they had. Provisioning the
// wildcard makes EVERY address at the domain verified — which means
// MatchVerifiedAddress now returns the FIRST candidate, and whether that is the
// per-service alias or the mailbox address depends entirely on what the provider
// wrote into the headers.
//
// Those two worlds look identical from outside: in one the backfill revives the
// alias story, in the other it runs clean and changes nothing. So the dry run
// reports the CANDIDATE CHAIN with each candidate's header source, not just the
// verdict — "no rows changed" must never be the first time an operator finds out
// which world they are in.
func runBackfillAlias(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-alias", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the re-derived aliases (default: dry-run)")
		dryRun    = fs.Bool("dry-run", true, "report what would change without writing (overridden by -apply)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
		samples   = fs.Int("samples", 8, "how many per-message candidate chains to print")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	write := *apply || !*dryRun

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

	var identities []store.Identity
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		identities, e = tx.ListIdentities(ctx)
		return e
	}); err != nil {
		return err
	}
	fmt.Printf("identities (%d):\n", len(identities))
	wildcards := 0
	for _, i := range identities {
		if i.Kind == store.IdentityWildcard {
			wildcards++
		}
		fmt.Printf("  %-28s kind=%-15s enabled=%t\n", i.Email(), i.Kind, i.Enabled)
	}
	if wildcards == 0 {
		fmt.Printf("\nNOTE: no domain_wildcard identity exists, so re-derivation can only ever\n")
		fmt.Printf("reproduce what ingest already stored. Provision one first:\n")
		fmt.Printf("  mailroom-admin provision-identity --user-id %s --domain <your-domain>\n\n", userID)
	}

	var (
		scanned, changed, unchanged, noCandidates, errCount int
		bySource                                            = map[string]int{}
		byAlias                                             = map[string]int{}
		printed                                             int
	)
	after := ulid.ULID{}
	for {
		var refs []store.MessageAliasRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.MessageAliasRefsAfter(ctx, after, *batch)
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
				errCount++
				d.log.Warn("backfill-alias: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			parsed, err := mime.Parse(bytes.NewReader(raw))
			if err != nil || parsed == nil {
				errCount++
				d.log.Warn("backfill-alias: parse failed", "message", ref.ID, "err", err)
				continue
			}
			candidates, forwarded := alias.DeriveFrom(parsed.Header)
			if len(candidates) == 0 {
				noCandidates++
				continue
			}

			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				// The SAME decision ingest makes — deliberately the same function,
				// so this can never drift from what the live path would store.
				addressed, identityID, e := ingest.DeriveAddressedIdentity(ctx, tx, candidates)
				if e != nil {
					return e
				}
				var ident ulid.ULID
				if identityID != nil {
					ident = *identityID
				}
				moved := !strings.EqualFold(store.NormalizeAddress(addressed), store.NormalizeAddress(ref.AddressedAddress)) ||
					ident != ref.AddressedIdentityID

				byAlias[store.NormalizeAddress(addressed)]++
				bySource[string(sourceOf(candidates, addressed))]++

				if moved {
					changed++
				} else {
					unchanged++
				}
				// Print chains for the messages that actually tell you something:
				// the ones whose alias moves. If nothing moves, print anyway — that
				// is the case an operator most needs to see the evidence for.
				if printed < *samples && (moved || unchanged <= *samples) {
					printed++
					printCandidateChain(ref, candidates, forwarded, addressed, moved)
				}
				if write && moved {
					if _, e := tx.UpdateAddressedAlias(ctx, ref.ID, addressed, ident); e != nil {
						return e
					}
				}
				return nil
			}); err != nil {
				errCount++
				d.log.Warn("backfill-alias: re-derive failed", "message", ref.ID, "err", err)
			}
		}
	}

	fmt.Printf("\nscanned %d  would-change %d  unchanged %d  no-candidates %d  errors %d\n",
		scanned, changed, unchanged, noCandidates, errCount)

	fmt.Printf("\nWINNING CANDIDATE BY HEADER SOURCE — where the chosen alias came from:\n")
	printCounts(bySource, 10)
	fmt.Printf("\nRESULTING ALIAS DISTRIBUTION (top 25) — this is the signal, or the lack of it:\n")
	printCounts(byAlias, 25)
	if len(byAlias) <= 1 {
		fmt.Printf("\nONE alias across the whole mailbox means the alias carries NO information:\n")
		fmt.Printf("every message resolves the same way, so no rule, induction or human reading\n")
		fmt.Printf("can learn anything from it. Check the source histogram above — if the winner\n")
		fmt.Printf("is an envelope header carrying the mailbox address, the per-service alias is\n")
		fmt.Printf("in the chain but outranked, and the fix is candidate ORDER, not this backfill.\n")
	}

	if !write {
		fmt.Printf("\nDRY RUN — nothing written. Re-run with --apply to persist.\n")
	} else {
		fmt.Printf("\nAPPLIED — %d messages re-pointed (one Email change_log row each).\n", changed)
	}
	return nil
}

// sourceOf reports which header the chosen address came from, so the histogram
// can distinguish "the provider told us the real recipient" from "we fell back
// to a To line".
func sourceOf(candidates []alias.Candidate, chosen string) alias.Source {
	want := store.NormalizeAddress(chosen)
	for _, c := range candidates {
		if store.NormalizeAddress(c.Address) == want {
			return c.Source
		}
	}
	return "none"
}

// printCandidateChain dumps one message's full candidate list in the order the
// resolver sees it. This is the evidence that answers "why did/didn't the alias
// change" without a second run or a debugger.
func printCandidateChain(ref store.MessageAliasRef, candidates []alias.Candidate, forwarded bool, chosen string, moved bool) {
	verdict := "unchanged"
	if moved {
		verdict = "CHANGED"
	}
	fmt.Printf("\n  %s  %s\n", ref.ID, verdict)
	fmt.Printf("    stored : %s\n", orNone(ref.AddressedAddress))
	fmt.Printf("    chosen : %s%s\n", orNone(chosen), map[bool]string{true: "  (forwarded-mail ordering)"}[forwarded])
	fmt.Printf("    chain  :")
	for i, c := range candidates {
		mark := " "
		if store.NormalizeAddress(c.Address) == store.NormalizeAddress(chosen) {
			mark = "*"
		}
		if i > 0 {
			fmt.Printf("            ")
		}
		fmt.Printf(" %s %-38s [%s]\n", mark, c.Address, c.Source)
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// printCounts prints a count map, largest first, capped at n rows.
func printCounts(m map[string]int, n int) {
	type kv struct {
		k string
		v int
	}
	rows := make([]kv, 0, len(m))
	for k, v := range m {
		rows = append(rows, kv{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].v != rows[j].v {
			return rows[i].v > rows[j].v
		}
		return rows[i].k < rows[j].k
	})
	if len(rows) == 0 {
		fmt.Printf("  (none)\n")
		return
	}
	for i, r := range rows {
		if i >= n {
			fmt.Printf("  … and %d more\n", len(rows)-n)
			break
		}
		fmt.Printf("  %6d  %s\n", r.v, orNone(r.k))
	}
}

// runRepairContacts undoes the two ways a missing catchall identity corrupts the
// address book (mailroom#177):
//
//  1. The user's OWN aliases were filed as contacts. upsertContactAddress
//     excludes "me" by asking ResolveSendIdentity, which correctly said no for
//     every per-service alias while no wildcard existed. Ingest self-corrects
//     once the wildcard is provisioned; the rows already written do not.
//
//  2. outbound_count is 0 everywhere. The live path bumps it as mail is
//     ingested, but it shipped in v1.4.0 after this mailbox's Sent mail had
//     already been ingested — so the logic is right and only history is missing.
//     Recipients live only in the blob (there is no recipients table), so this
//     re-reads them, and it SETS rather than increments so re-running converges
//     instead of double-counting.
//
// Until (2) runs, every correspondence-based signal — corr:1, the kind
// classifier's has_corresponded — is dead on the mailbox.
func runRepairContacts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repair-contacts", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the repairs (default: dry-run)")
		dryRun    = fs.Bool("dry-run", true, "report what would change without writing (overridden by -apply)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	write := *apply || !*dryRun

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	// ---- (1) own-address contacts ------------------------------------------
	var own []store.OwnAddressContact
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		own, e = tx.OwnAddressContacts(ctx)
		return e
	}); err != nil {
		return err
	}
	fmt.Printf("OWN ADDRESSES FILED AS CONTACTS: %d\n", len(own))
	for i, c := range own {
		if i >= 25 {
			fmt.Printf("  … and %d more\n", len(own)-25)
			break
		}
		sole := ""
		if c.SoleAddress {
			sole = "  (sole address — contact goes too)"
		}
		fmt.Printf("  %6d sightings  %s%s\n", c.SeenCount, c.Address, sole)
	}
	if len(own) == 0 {
		fmt.Printf("  (none — either no wildcard identity is provisioned yet, or this is already clean)\n")
	}

	removedAddrs, removedContacts := 0, 0
	if write {
		for _, c := range own {
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				gone, e := tx.DeleteOwnAddressContact(ctx, c.AddressID)
				if e != nil {
					return e
				}
				removedAddrs++
				if gone {
					removedContacts++
				}
				return nil
			}); err != nil {
				return fmt.Errorf("remove own-address contact %s: %w", c.Address, err)
			}
		}
		fmt.Printf("  removed %d addresses (%d now-empty contacts destroyed)\n", removedAddrs, removedContacts)
	}

	// ---- (2) outbound_count ------------------------------------------------
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
		scanned, sentSeen, errCount int
		outbound                    = map[string]int{}
	)
	after := ulid.ULID{}
	for {
		var refs []store.MessageAliasRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.MessageAliasRefsAfter(ctx, after, *batch)
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
				errCount++
				d.log.Warn("repair-contacts: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			parsed, err := mime.Parse(bytes.NewReader(raw))
			if err != nil || parsed == nil {
				errCount++
				d.log.Warn("repair-contacts: parse failed", "message", ref.ID, "err", err)
				continue
			}
			from := ""
			if addrs := parsed.Header.From(); len(addrs) > 0 {
				from = addrs[0].Address
			}
			if from == "" {
				continue
			}
			// A message the USER SENT is defined exactly as the contacts layer
			// defines it — From resolves to a verified identity — so the two can
			// never disagree about direction.
			var isSent bool
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				_, e := tx.ResolveSendIdentity(ctx, store.NormalizeAddress(from))
				if e == nil {
					isSent = true
					return nil
				}
				if errors.Is(e, store.ErrFromNotVerified) {
					return nil
				}
				return e
			}); err != nil {
				return err
			}
			if !isSent {
				continue
			}
			sentSeen++
			for _, a := range append(parsed.Header.To(), parsed.Header.Cc()...) {
				norm := store.NormalizeAddress(a.Address)
				if norm == "" || store.AddressDomain(norm) == "" {
					continue
				}
				outbound[norm]++
			}
		}
	}

	fmt.Printf("\nOUTBOUND CORRESPONDENCE: scanned %d messages, %d sent by the user, %d distinct recipients\n",
		scanned, sentSeen, len(outbound))
	printCounts(outbound, 25)
	if errCount > 0 {
		fmt.Printf("  (%d messages could not be read/parsed)\n", errCount)
	}

	bumped := 0
	if write {
		for addr, n := range outbound {
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				ok, e := tx.SetOutboundCount(ctx, addr, n)
				if e != nil {
					return e
				}
				if ok {
					bumped++
				}
				return nil
			}); err != nil {
				return fmt.Errorf("set outbound count for %s: %w", addr, err)
			}
		}
		fmt.Printf("  set outbound_count on %d contact addresses\n", bumped)
		// A recipient the user wrote to who is not in the address book at all has
		// nowhere to record the count. Say so rather than let the numbers quietly
		// not add up.
		if missing := len(outbound) - bumped; missing > 0 {
			fmt.Printf("  %d recipients had no contact_addresses row (never received FROM), so no count was stored\n", missing)
		}
	}

	if !write {
		fmt.Printf("\nDRY RUN — nothing written. Re-run with --apply to persist.\n")
	}
	return nil
}
