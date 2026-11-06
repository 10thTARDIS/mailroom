// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/hardwrap"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin plaintext-report [--user-id U] [--limit N]` (#35): REPORT
// FIRST, for a display change nobody has decided to make.
//
// Plain-text mail is shown with every line break kept, so mail hard-wrapped at
// about 72 columns wraps a second time on a narrow screen. format=flowed mail is
// already rejoined (web/src/core/flowed.ts). The open question is mail that is
// hard-wrapped and NOT marked: a rule could rejoin it when it looks wrapped. This
// command measures, on the real corpus, how big that population is and how many
// messages the rule would touch, so the decision is made on numbers.
//
// (Opening the blob store may create its directory, like every other report
// command here; the database access is read-only.)
//
// It changes nothing, and neither does anything it calls. Every database read is a
// read-only transaction (UserReadTx, rolled back); the blobs are only read. It
// prints COUNTS, the thresholds in force, the --limit bound, and a few HASHED
// message ids for spot checks. It never prints message text, subjects or
// addresses.
//
// Per-case counts, never one total (#177, #191): "0 would change" must be
// distinguishable from "everything was refused for the same reason", so every
// refusal reason has its own line.
//
// ⚠ THE RULE IS internal/hardwrap.Analyze, NOT A COPY OF IT. If a display change
// is ever built, it uses the same function, so the report and the display cannot
// disagree about what "looks wrapped" means.
func runPlaintextReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plaintext-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "one user (ULID); default every active user")
		limit     = fs.Int("limit", 0, "scan at most N messages per user, oldest first (default 0: every message)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must not be negative")
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var users []ulid.ULID
	if *userIDStr != "" {
		u, err := parseUserID(*userIDStr)
		if err != nil {
			return err
		}
		users = []ulid.ULID{u}
	} else if users, err = d.store.ListActiveUserIDs(ctx); err != nil {
		return err
	}

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("plaintext-report: master key: %w", err)
	}
	defer crypto.Zero(master)

	say(os.Stdout, "plaintext-report: %d user(s)\n", len(users))
	total := newPtAgg()
	for _, u := range users {
		dek, err := crypto.DeriveKey(master, u[:], syncrunner.MailBlobInfoV1)
		if err != nil {
			return fmt.Errorf("plaintext-report: derive mail dek: %w", err)
		}
		agg := newPtAgg()
		err = ptScanUser(ctx, d, blobs, u, dek, *limit, agg)
		crypto.Zero(dek)
		if err != nil {
			return fmt.Errorf("plaintext-report: user %s: %w", u, err)
		}
		say(os.Stdout, "\nuser %s\n", u)
		agg.print(os.Stdout, *limit)
		total.merge(agg)
	}
	if len(users) > 1 {
		say(os.Stdout, "\nall users\n")
		total.print(os.Stdout, *limit)
	}
	say(os.Stdout, "\nREPORT ONLY - nothing was written and no display behaviour changed.\n")
	return nil
}

// ptScanUser walks a user's messages oldest first, in pages, each page a
// read-only transaction, and feeds the bytes of each to agg. limit > 0 stops the
// walk after that many messages and records whether any were left.
func ptScanUser(ctx context.Context, d *deps, blobs blob.BlobStore, userID ulid.ULID, dek []byte, limit int, agg *ptAgg) error {
	const batch = 200
	var after ulid.ULID
	for {
		n := batch
		if limit > 0 && limit-agg.scanned < n {
			n = limit - agg.scanned
		}
		if n <= 0 {
			// At the limit: is anything left? One more row answers it.
			var more []store.MessageAuthRef
			if err := d.store.UserReadTx(ctx, userID, func(tx *store.UserTx) error {
				var e error
				more, e = tx.MessageAuthRefsAfter(ctx, after, 1)
				return e
			}); err != nil {
				return err
			}
			agg.limited = len(more) > 0
			return nil
		}
		var page []store.MessageAuthRef
		if err := d.store.UserReadTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, n)
			return e
		}); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, ref := range page {
			after = ref.ID
			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// Counted, never folded into "not plain": a message that could not
				// be read was not measured.
				agg.scanned++
				agg.unreadable++
				d.log.Warn("plaintext-report: read blob failed", "message", ptHashID(ref.ID), "err", err)
				continue
			}
			agg.add(ref.ID, raw)
		}
	}
}

// ptAgg is one scan's counters. Every count is a number of MESSAGES.
type ptAgg struct {
	scanned    int // messages walked
	unreadable int // blob unreadable or unparsable: not measured, not counted as anything else
	notPlain   int // shown as HTML (a text/html part exists), or no text body at all
	plain      int // shown as text/plain: the denominator for everything below

	flowed int // plain, with a format=flowed text/plain part: already rejoined by the client

	// The rest are among plain, not flowed.
	unmarked    int
	looksWrap   int // the rule judged them hard-wrapped
	wouldChange int // looksWrap with at least one line break to join
	joins       int // line breaks that would be replaced by a space
	reasons     map[hardwrap.Reason]int

	limited bool // the scan stopped at --limit with messages still unread
	samples []string
}

const ptMaxSamples = 5

func newPtAgg() *ptAgg { return &ptAgg{reasons: map[hardwrap.Reason]int{}} }

func (a *ptAgg) merge(b *ptAgg) {
	a.scanned += b.scanned
	a.unreadable += b.unreadable
	a.notPlain += b.notPlain
	a.plain += b.plain
	a.flowed += b.flowed
	a.unmarked += b.unmarked
	a.looksWrap += b.looksWrap
	a.wouldChange += b.wouldChange
	a.joins += b.joins
	a.limited = a.limited || b.limited
	for k, v := range b.reasons {
		a.reasons[k] += v
	}
	for _, s := range b.samples {
		if len(a.samples) < ptMaxSamples {
			a.samples = append(a.samples, s)
		}
	}
}

// ptHashID is a short stable hash of a message id: enough to find the message
// again from the operator's side (hash the candidates) and useless to anyone else.
func ptHashID(id ulid.ULID) string {
	sum := sha256.Sum256([]byte("plaintext-report:" + id.String()))
	return hex.EncodeToString(sum[:6])
}

// ptFlowedMarked reports whether any text/plain BODY part of the message carries
// format=flowed (the same marker the JMAP body parts expose to the client).
func ptFlowedMarked(st *mime.Structure) bool {
	if st == nil {
		return false
	}
	if len(st.SubParts) > 0 {
		for _, sp := range st.SubParts {
			if ptFlowedMarked(sp) {
				return true
			}
		}
		return false
	}
	return st.ContentType == "text/plain" && st.Disposition != "attachment" &&
		strings.EqualFold(st.Params["format"], "flowed")
}

// add classifies one message's raw bytes into the counters.
func (a *ptAgg) add(id ulid.ULID, raw []byte) {
	a.scanned++
	msg, err := mime.ParseBytes(raw)
	if err != nil || msg == nil {
		a.unreadable++
		return
	}
	// "Shown as plain text" is the client's own rule (app/ui/bodyChoice.ts): the
	// HTML path is taken only when a text/html part exists.
	if msg.HTML != "" || strings.TrimSpace(msg.Text) == "" {
		a.notPlain++
		return
	}
	a.plain++
	if ptFlowedMarked(msg.Structure) {
		a.flowed++
		return
	}
	a.unmarked++
	res := hardwrap.Analyze(msg.Text)
	if !res.Wrapped {
		a.reasons[res.Reason]++
		return
	}
	a.looksWrap++
	if res.Joins > 0 {
		a.wouldChange++
		a.joins += res.Joins
		if len(a.samples) < ptMaxSamples {
			a.samples = append(a.samples, ptHashID(id))
		}
	}
}

func ptPct(n, of int) string {
	if of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}

func (a *ptAgg) print(w io.Writer, limit int) {
	say(w, "  messages walked:            %d\n", a.scanned)
	say(w, "  unreadable or unparsable:   %d  (not measured)\n", a.unreadable)
	say(w, "  shown as HTML / no text:    %d\n", a.notPlain)
	say(w, "  shown as text/plain:        %d\n", a.plain)
	say(w, "    format=flowed:            %d  (%s of plain; already rejoined)\n", a.flowed, ptPct(a.flowed, a.plain))
	say(w, "    no flowed marker:         %d  (%s of plain)\n", a.unmarked, ptPct(a.unmarked, a.plain))
	say(w, "      look hard-wrapped:      %d  (%s of plain, %s of unmarked)\n", a.looksWrap, ptPct(a.looksWrap, a.plain), ptPct(a.looksWrap, a.unmarked))
	say(w, "      would change:           %d  (%s of plain), joining %d line break(s)\n", a.wouldChange, ptPct(a.wouldChange, a.plain), a.joins)
	say(w, "      judged NOT wrapped:     %d, by reason:\n", a.unmarked-a.looksWrap)
	for _, r := range hardwrap.Reasons {
		say(w, "        %-26s %d\n", string(r), a.reasons[r])
	}
	say(w, "  rule: >= %d body lines; >= %.0f%% of paragraph lines (all but each paragraph's last, at least %d of them) between %d and %d characters; no list markers; no code-like indentation (>= 2 spaces or a tab); quoted lines and the signature set aside\n",
		hardwrap.MinBodyLines, 100*hardwrap.WrapFraction, hardwrap.MinCandidates, hardwrap.WrapMinCols, hardwrap.WrapMaxCols)
	if len(a.samples) > 0 {
		say(w, "  sample of would-change messages (hashed ids): %s\n", strings.Join(a.samples, " "))
	}
	switch {
	case limit > 0 && a.limited:
		say(w, "  BOUND: stopped at --limit %d per user (oldest first) with messages still unread; every count above is a LOWER BOUND for the whole mailbox.\n", limit)
	case limit > 0:
		say(w, "  --limit %d was not reached; every message of the user was walked.\n", limit)
	}
	if a.unreadable > 0 {
		say(w, "  BOUND: %d message(s) could not be read; the counts are lower bounds by that many.\n", a.unreadable)
	} else if !a.limited {
		say(w, "  the counts cover every message of the user.\n")
	}
}
