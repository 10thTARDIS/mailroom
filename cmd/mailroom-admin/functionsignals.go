// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin backfill-function-signals` — populate the #156 function
// signals for mail already held.
//
// Migration 00027 added six columns projecting internal/kind.Facts' byte-only
// fields onto app.messages — List-Unsubscribe-Post, Auto-Submitted,
// Precedence, Feedback-ID, the DKIM d= domains, and Return-Path — with the
// identical shape migration 00016 already established for
// List-Id/List-Unsubscribe/the recipient count/the auth verdict (#157). Ingest
// writes them for every NEW message from here on (ingest.FunctionSignalsFromHeader);
// this backfills the mail that arrived before they existed, the job rescore's
// first half does for the 00016 four.
//
// It calls the EXACT SAME extraction ingest uses (ingest.FunctionSignalsFromHeader),
// so this command cannot quietly derive a different answer than live delivery
// would have — the mistake the alias backfill's own comment warns about for
// DeriveAddressedIdentity.
//
// DRY RUN BY DEFAULT, like every other backfill here. THE REPORT IS PER SIGNAL,
// not one total: a single count says nothing about whether the six behave
// alike over this mailbox, and #157's own history (a hand-set weight making a
// daily newsletter the closest correspondent) is the standing reminder that an
// aggregate hides exactly the shape that matters. For each signal it prints how
// many of the scanned messages carry that header at all — re-derived from the
// stored BYTES, not the column, since the column does not exist yet for these
// rows — which is also the number that answers whether a signal is worth
// storing at all on THIS mailbox.
func runBackfillFunctionSignals(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-function-signals", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the backfilled signals (default: dry-run, report only)")
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

	stats := newFunctionSignalStats()

	var scanned, changed, unchanged, errCount int
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
				errCount++
				d.log.Warn("backfill-function-signals: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			parsed, err := mime.ParseBytes(raw)
			if err != nil || parsed == nil {
				errCount++
				d.log.Warn("backfill-function-signals: parse failed", "message", ref.ID, "err", err)
				continue
			}

			sig := ingest.FunctionSignalsFromHeader(parsed.Header)
			stats.record(sig)

			if !write {
				continue
			}
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				didChange, e := tx.UpdateMessageFunctionSignals(ctx, ref.ID, sig)
				if e != nil {
					return e
				}
				if didChange {
					changed++
				} else {
					unchanged++
				}
				return nil
			}); err != nil {
				errCount++
				d.log.Warn("backfill-function-signals: write failed", "message", ref.ID, "err", err)
			}
		}
		if len(refs) < *batch {
			break
		}
	}

	mode := "DRY RUN (nothing written)"
	if write {
		mode = "APPLIED"
	}
	fmt.Printf("=== backfill-function-signals: %s ===\n", mode)
	fmt.Printf("  messages scanned                    : %d\n", scanned)
	if write {
		fmt.Printf("  rows changed                         : %d\n", changed)
		fmt.Printf("  rows unchanged (already backfilled)  : %d\n", unchanged)
	}
	fmt.Printf("  errors (blob unreadable/unparsable)  : %d\n\n", errCount)

	stats.print(scanned)

	if !write {
		fmt.Println("\nRe-run with -apply to write the signals.")
	}
	return nil
}

// functionSignalStats accumulates, per #156 signal, how many of the scanned
// messages actually carried it — the number that answers both "what would
// this backfill change" and "is this signal worth storing on THIS mailbox".
type functionSignalStats struct {
	listUnsubscribePost int
	autoSubmitted       int
	precedence          int
	feedbackID          int
	dkim                int
	dkimMultiDomain     int
	dkimDomainTotal     int
	returnPath          int
}

func newFunctionSignalStats() *functionSignalStats { return &functionSignalStats{} }

func (s *functionSignalStats) record(sig store.FunctionSignals) {
	if strings.TrimSpace(sig.ListUnsubscribePost) != "" {
		s.listUnsubscribePost++
	}
	if strings.TrimSpace(sig.AutoSubmitted) != "" {
		s.autoSubmitted++
	}
	if strings.TrimSpace(sig.Precedence) != "" {
		s.precedence++
	}
	if strings.TrimSpace(sig.FeedbackID) != "" {
		s.feedbackID++
	}
	if strings.TrimSpace(sig.ReturnPath) != "" {
		s.returnPath++
	}
	if n := len(sig.DKIMDomains); n > 0 {
		s.dkim++
		s.dkimDomainTotal += n
		if n > 1 {
			s.dkimMultiDomain++
		}
	}
}

func (s *functionSignalStats) print(scanned int) {
	pct := func(n int) float64 {
		if scanned == 0 {
			return 0
		}
		return 100 * float64(n) / float64(scanned)
	}
	fmt.Println("PER-SIGNAL HEADER PRESENCE (re-derived from the stored bytes, not the column):")
	rows := []struct {
		name string
		n    int
	}{
		{"List-Unsubscribe-Post", s.listUnsubscribePost},
		{"Auto-Submitted", s.autoSubmitted},
		{"Precedence", s.precedence},
		{"Feedback-ID", s.feedbackID},
		{"DKIM-Signature (d=)", s.dkim},
		{"Return-Path", s.returnPath},
	}
	for _, r := range rows {
		fmt.Printf("  %-24s %6d  (%5.1f%%)\n", r.name, r.n, pct(r.n))
	}
	if s.dkim > 0 {
		fmt.Printf("\n  DKIM: %d messages sign with more than one d= domain; %.2f domains/message where present\n",
			s.dkimMultiDomain, float64(s.dkimDomainTotal)/float64(s.dkim))
	}
}
