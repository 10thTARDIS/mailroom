// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/store"
)

// `mailroom-admin quarantine` — the operator surface for the attachment
// quarantine (#396, ADR-027 §8): what the download gate would refuse right
// now, and the two decisions that change it.
//
// READ-ONLY BY DEFAULT. This project's rule is "build the report before the
// fix" (CLAUDE.md, #177/#191/#206): with neither --release nor --revoke this
// command only LISTS every quarantined part and mutates nothing. A release
// is opt-in and one content digest at a time, never a batch default — the
// whole reason it exists is that releasing content is a decision about one
// person's mail, made on purpose.
func runQuarantine(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quarantine", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user", "", "user id (ULID) — REQUIRED")
		release   = fs.String("release", "", "msgID:partPath — release this content (opens EVERY copy the user holds, ADR-027 §8)")
		revoke    = fs.String("revoke", "", "msgID:partPath — revoke the release on this content (re-arms the gate)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan (listing only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// ⚠ --user IS REQUIRED, unlike attachment-report's --user which defaults to
	// "every active user". That command only reads; this one can MUTATE, and a
	// release is a decision about one person's mail — guessing the user, or
	// silently sweeping every user's mailbox, is not an acceptable default for
	// something that opens a door.
	if strings.TrimSpace(*userIDStr) == "" {
		return errors.New("quarantine: --user is required (ULID)")
	}
	userID, err := ulid.Parse(*userIDStr)
	if err != nil {
		return fmt.Errorf("quarantine: --user %q: not a valid ULID: %w", *userIDStr, err)
	}
	if *release != "" && *revoke != "" {
		return errors.New("quarantine: --release and --revoke are mutually exclusive")
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	switch {
	case *release != "":
		return runQuarantineRelease(ctx, d, userID, *release)
	case *revoke != "":
		return runQuarantineRevoke(ctx, d, userID, *revoke)
	default:
		return runQuarantineList(ctx, d, userID, *batch)
	}
}

// parseMsgPart splits "msgID:partPath" on the FIRST colon. A part path is a
// dotted MIME index (internal/mime.PartPaths, e.g. "1.2") and never contains
// a colon itself, so this split is exact rather than a heuristic.
func parseMsgPart(spec string) (ulid.ULID, string, error) {
	i := strings.Index(spec, ":")
	if i < 0 {
		return ulid.ULID{}, "", fmt.Errorf("want msgID:partPath, got %q", spec)
	}
	msgID, err := ulid.Parse(spec[:i])
	if err != nil {
		return ulid.ULID{}, "", fmt.Errorf("%q: not a valid message id: %w", spec[:i], err)
	}
	partPath := spec[i+1:]
	if partPath == "" {
		return ulid.ULID{}, "", fmt.Errorf("%q: empty part path", spec)
	}
	return msgID, partPath, nil
}

func runQuarantineRelease(ctx context.Context, d *deps, userID ulid.ULID, spec string) error {
	msgID, partPath, err := parseMsgPart(spec)
	if err != nil {
		return fmt.Errorf("quarantine --release: %w", err)
	}

	var (
		partHash []byte
		affected []ulid.ULID
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		partHash, affected, e = tx.ReleaseAttachment(ctx, msgID, partPath)
		return e
	}); err != nil {
		return mapAttachmentErr("quarantine --release", msgID, partPath, err)
	}

	fmt.Printf("released content %x (named from %s:%s)\n", partHash, msgID, partPath)
	// ⚠ THE FULL LIST, NOT JUST A COUNT. A release is keyed on CONTENT and
	// reaches every copy the user holds (ADR-027 §8) — the operator named one
	// row and the store may have opened a dozen doors. Printing "12 affected"
	// and stopping there hides exactly the thing the operator needs to audit:
	// which twelve.
	fmt.Printf("affected message(s): %d\n", len(affected))
	for _, id := range affected {
		fmt.Printf("  %s\n", id)
	}
	return nil
}

func runQuarantineRevoke(ctx context.Context, d *deps, userID ulid.ULID, spec string) error {
	msgID, partPath, err := parseMsgPart(spec)
	if err != nil {
		return fmt.Errorf("quarantine --revoke: %w", err)
	}

	var (
		state    store.PartScanState
		affected []ulid.ULID
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		found := false
		var e error
		state, found, e = tx.AttachmentScanState(ctx, msgID, partPath)
		if e != nil {
			return e
		}
		if !found {
			return store.ErrAttachmentNotFound
		}
		if len(state.PartHash) == 0 {
			return store.ErrAttachmentNoContentHash
		}
		affected, e = tx.RevokeAttachmentRelease(ctx, state.PartHash)
		return e
	}); err != nil {
		return mapAttachmentErr("quarantine --revoke", msgID, partPath, err)
	}

	if len(affected) == 0 {
		fmt.Printf("no release existed on content %x (named from %s:%s) — nothing to revoke\n",
			state.PartHash, msgID, partPath)
		return nil
	}
	fmt.Printf("revoked the release on content %x (named from %s:%s)\n", state.PartHash, msgID, partPath)
	// Same reasoning as --release: the revoke re-arms the gate on every copy,
	// not just the one named on the command line.
	fmt.Printf("affected message(s): %d\n", len(affected))
	for _, id := range affected {
		fmt.Printf("  %s\n", id)
	}
	return nil
}

// mapAttachmentErr turns the store's sentinel errors into text an operator
// running this by hand can act on — WHY a part cannot be released or
// revoked, not just that the call failed.
func mapAttachmentErr(op string, msgID ulid.ULID, partPath string, err error) error {
	switch {
	case errors.Is(err, store.ErrAttachmentNotFound):
		return fmt.Errorf("%s: %s:%s: no projected attachment row for that message/part "+
			"(check the ids, or ADR-027 §3's projection may not have backfilled this message yet)",
			op, msgID, partPath)
	case errors.Is(err, store.ErrAttachmentNoContentHash):
		return fmt.Errorf("%s: %s:%s: this part has never been scanned, so it carries no content "+
			"digest to key a release on — there is nothing to release yet", op, msgID, partPath)
	default:
		return fmt.Errorf("%s: %s:%s: %w", op, msgID, partPath, err)
	}
}

// quarantinedPart pairs a message id with the blocked part found on it, so
// the listing can sort deterministically before printing.
type quarantinedPart struct {
	msgID ulid.ULID
	state store.PartScanState
}

// runQuarantineList is the default (no flags but --user): walk the user's
// messages, and for every part attachscan.Blocked calls quarantined right
// now — the SAME function the download gate calls, so this cannot report a
// different policy from the one being enforced — print it.
//
// It reports its bound even when it finds nothing (#155, #176): "0
// quarantined parts" and "0 attachment rows examined" read as the same
// printed number and are opposite findings — the first says the mailbox is
// clean, the second says nothing has been scanned (or projected) at all.
func runQuarantineList(ctx context.Context, d *deps, userID ulid.ULID, batch int) error {
	var (
		scanned  int // attachment rows examined, across every message
		messages int // messages carrying at least one projected row
		byState  = map[string]int{}
		found    []quarantinedPart
	)

	var after ulid.ULID
	for {
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return fmt.Errorf("quarantine: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		ids := make([]ulid.ULID, len(page))
		for i, ref := range page {
			ids[i] = ref.ID
		}
		after = page[len(page)-1].ID

		var states map[ulid.ULID][]store.PartScanState
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			states, e = tx.AttachmentScanStates(ctx, ids)
			return e
		}); err != nil {
			return fmt.Errorf("quarantine: read scan states: %w", err)
		}

		for _, id := range ids {
			// ⚠ A MESSAGE ABSENT FROM THE MAP IS NOT "ZERO ATTACHMENTS" — per
			// AttachmentScanStates' own doc comment, absence means no row was
			// ever projected for it, which is a different fact from "has no
			// attachments" and must not be folded into the same count.
			parts, ok := states[id]
			if !ok {
				continue
			}
			messages++
			for _, p := range parts {
				scanned++
				byState[p.Status]++
				if attachscan.Blocked(p.Status, p.Released) {
					found = append(found, quarantinedPart{msgID: id, state: p})
				}
			}
		}
	}

	if len(found) == 0 {
		fmt.Printf("quarantine: 0 quarantined part(s) found for user %s\n", userID)
		fmt.Printf("  attachment rows examined: %d (across %d message(s) with a projected row)\n",
			scanned, messages)
		if scanned == 0 {
			fmt.Println("  ⚠ ZERO ROWS EXAMINED — this is NOT evidence the mailbox is clean. It")
			fmt.Println("    means either this user has no attachments at all, or the ADR-027 §3")
			fmt.Println("    projection has not backfilled their messages yet. Run attachment-report")
			fmt.Println("    to tell those two apart before reading this as a clean bill of health.")
		}
		printQuarantineStateCounts(byState)
		return nil
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].msgID != found[j].msgID {
			return found[i].msgID.String() < found[j].msgID.String()
		}
		return found[i].state.PartPath < found[j].state.PartPath
	})
	fmt.Printf("quarantine: %d quarantined part(s) for user %s\n\n", len(found), userID)
	fmt.Printf("%-28s %-10s %-10s %-8s %s\n", "MESSAGE", "PART", "STATUS", "RELEASED", "PART HASH")
	for _, q := range found {
		fmt.Printf("%-28s %-10s %-10s %-8t %x\n",
			q.msgID, q.state.PartPath, q.state.Status, q.state.Released, q.state.PartHash)
	}

	fmt.Printf("\nattachment rows examined: %d (across %d message(s) with a projected row)\n", scanned, messages)
	printQuarantineStateCounts(byState)
	return nil
}

// printQuarantineStateCounts is the per-state summary. Never a single total
// (#177, #191, #206): one number cannot distinguish "nothing is quarantined"
// from "nothing has been scanned", and this is the count that tells them
// apart.
func printQuarantineStateCounts(byState map[string]int) {
	if len(byState) == 0 {
		return
	}
	fmt.Println("\nby scan_status (all examined rows, not just the blocked ones):")
	keys := make([]string, 0, len(byState))
	for k := range byState {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-12s %d\n", k, byState[k])
	}
}
