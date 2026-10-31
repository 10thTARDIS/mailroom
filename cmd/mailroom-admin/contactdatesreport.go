// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin contact-dates-report --user-id U [--sample N]` — how wrong are
// the stored contact sighting dates as "first/last mail" dates? (#816, report
// first.)
//
// WHY IT EXISTS. contact_addresses.first_seen_at/last_seen_at are set to now() at
// insert and bumped to now() at ingest: they say when Mailroom INGESTED mail,
// not when it was sent. After a bulk mailbox import every contact looks "seen in
// the last 90 days". The contacts page needs real dates, and a "recently in
// touch" sort on the stored column would sort nothing. Before any backfill, this
// measures the gap per signal.
//
// THE TRUE DATES come from the user's messages by received_at (see
// store/contact_dates_report.go for why that clock):
//
//	from   earliest/latest message From the address — ONE aggregate query;
//	sent   earliest/latest message the user SENT to it (To/Cc/Bcc). Recipients
//	       are not columns, so this reads the blob of each message whose From
//	       resolves to one of the user's identities (the rule ingest uses for
//	       outbound_count) and parses To/Cc/Bcc with the real MIME parser.
//
// The stored dates cover both directions (a recipient is a contact too), so the
// comparison uses the earliest and latest of the two. The per-direction counts
// are printed so a single direction's absence is visible.
//
// BOUNDS, stated in the output: messages examined, how many were the user's
// own, how many blobs were unreadable (their recipients are NOT in the sent
// side, so an address reachable only through them reads as "no mail found" and
// the count says how many such blobs there were). Drafts are excluded; trashed
// messages are included, as ingest counted them.
//
// PRIVACY: this prints addresses (the sample, and the examples of addresses
// with no mail), which is an operator report on the operator's own instance. Nothing
// is logged elsewhere. READ-ONLY: no writes, no --apply.
func runContactDatesReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("contact-dates-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose contacts to measure (required)")
	sample := fs.Int("sample", 10, "addresses to list with stored vs true dates")
	batch := fs.Int("batch", 500, "messages per page of the sent-mail walk")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	read, done, err := openMailReader(ctx, d, userID, "contact-dates-report")
	if err != nil {
		return err
	}
	defer done()
	rep, err := collectContactDates(ctx, d.store, userID, read, *batch, time.Now().UTC(), *sample)
	if err != nil {
		return err
	}
	rep.print()
	return nil
}

// openMailReader is the decrypting blob reader both contact-dates verbs use.
// done zeroes the derived key.
func openMailReader(ctx context.Context, d *deps, userID ulid.ULID, verb string) (rawReader, func(), error) {
	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: master key: %w", verb, err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: derive mail dek: %w", verb, err)
	}
	read := func(ctx context.Context, hash []byte) ([]byte, error) {
		return readMailBlob(ctx, blobs, userID, dek, hash)
	}
	return read, func() { crypto.Zero(dek) }, nil
}

// span is the earliest and latest message time seen for one address.
type span struct {
	first, last time.Time
	n           int
}

func (s *span) add(t time.Time) {
	if s.n == 0 || t.Before(s.first) {
		s.first = t
	}
	if s.n == 0 || t.After(s.last) {
		s.last = t
	}
	s.n++
}

// sentStats bounds the sent side.
type sentStats struct {
	examined   int // non-draft messages walked
	own        int // ...whose From resolved to one of the user's identities
	unreadable int // own messages whose blob could not be read or parsed
}

// collectSent walks the user's messages and returns, per normalised recipient
// address, the span of messages the user sent to it (To, Cc and Bcc; one count
// per message however many fields name it).
func collectSent(ctx context.Context, st *store.Store, userID ulid.ULID, read rawReader, batch int) (map[string]*span, sentStats, error) {
	sent := map[string]*span{}
	var stats sentStats
	own := map[string]bool{} // normalised From -> resolves to the user
	after := ulid.ULID{}
	for {
		var page []store.ContactSentRef
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			if page, e = tx.ContactSentRefsAfter(ctx, after, batch); e != nil {
				return e
			}
			for _, r := range page {
				from := store.NormalizeAddress(r.FromAddress)
				if _, seen := own[from]; seen {
					continue
				}
				_, e := tx.ResolveSendIdentity(ctx, from)
				switch {
				case e == nil:
					own[from] = true
				case errors.Is(e, store.ErrFromNotVerified):
					own[from] = false
				default:
					return e
				}
			}
			return nil
		}); err != nil {
			return nil, stats, fmt.Errorf("contact-dates-report: walk messages: %w", err)
		}
		for _, r := range page {
			after = r.ID
			stats.examined++
			if !own[store.NormalizeAddress(r.FromAddress)] {
				continue
			}
			stats.own++
			raw, err := read(ctx, r.ContentHash)
			if err != nil {
				stats.unreadable++
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil || msg == nil {
				stats.unreadable++
				continue
			}
			for _, a := range recipientAddresses(msg.Header) {
				s := sent[a]
				if s == nil {
					s = &span{}
					sent[a] = s
				}
				s.add(r.ReceivedAt)
			}
		}
		if len(page) < batch {
			break
		}
	}
	return sent, stats, nil
}

// recipientAddresses is the distinct normalised To/Cc/Bcc addresses of a header.
func recipientAddresses(h mime.Header) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]mime.Address{h.To(), h.Cc(), h.Bcc()} {
		for _, a := range group {
			n := store.NormalizeAddress(a.Address)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func collectContactDates(ctx context.Context, st *store.Store, userID ulid.ULID, read rawReader, batch int, now time.Time, sample int) (*contactDatesReport, error) {
	in, err := gatherContactDates(ctx, st, userID, read, batch)
	if err != nil {
		return nil, err
	}
	rep := buildContactDatesReport(in.rows, in.sent, now, sample)
	rep.sent = in.stats
	return rep, nil
}

// contactDatesInput is what both contact-dates verbs measure from: the stored
// dates with the From-side aggregate, and the sent-side spans.
type contactDatesInput struct {
	rows  []store.ContactMailDate
	sent  map[string]*span
	stats sentStats
}

func gatherContactDates(ctx context.Context, st *store.Store, userID ulid.ULID, read rawReader, batch int) (contactDatesInput, error) {
	var in contactDatesInput
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		in.rows, e = tx.ContactMailDates(ctx)
		return e
	}); err != nil {
		return in, fmt.Errorf("contact dates: %w", err)
	}
	var err error
	in.sent, in.stats, err = collectSent(ctx, st, userID, read, batch)
	return in, err
}

// trueDates is one address's true first and last mail: the earliest and
// latest of the From side and the sent side. ok is false when no mail was
// found in either direction. THE one definition the report and the backfill
// share, so the backfill writes exactly what the report measured.
func trueDates(r store.ContactMailDate, sent map[string]*span) (first, last time.Time, hasFrom, hasSent, ok bool) {
	s := sent[store.NormalizeAddress(r.Address)]
	hasFrom = r.FromCount > 0 && r.FromFirst != nil && r.FromLast != nil
	hasSent = s != nil && s.n > 0
	extend := func(f, l time.Time) {
		if first.IsZero() || f.Before(first) {
			first = f
		}
		if last.IsZero() || l.After(last) {
			last = l
		}
	}
	if hasFrom {
		extend(*r.FromFirst, *r.FromLast)
	}
	if hasSent {
		extend(s.first, s.last)
	}
	return first, last, hasFrom, hasSent, hasFrom || hasSent
}

// Delta buckets: |stored - true| under a day is "within 1d" whichever way it
// points; beyond that the direction matters (stored LATER than the true date
// means ingest pushed it forward, EARLIER means the stored date predates every
// message found).
const (
	bucketWithinDay = iota
	bucketDay30     // 1 d to under 30 d
	bucketDay365    // 30 d to under 365 d
	bucketOverYear  // 365 d and more
	numBuckets
)

var bucketNames = [numBuckets]string{"<1 d", "1-30 d", "30-365 d", ">1 y"}

const day = 24 * time.Hour

// bucketDelta classifies stored - true. later is true when stored is after
// true; it is meaningless (false) in bucketWithinDay.
func bucketDelta(d time.Duration) (b int, later bool) {
	later = d > 0
	if d < 0 {
		d = -d
	}
	switch {
	case d < day:
		return bucketWithinDay, false
	case d < 30*day:
		return bucketDay30, later
	case d < 365*day:
		return bucketDay365, later
	}
	return bucketOverYear, later
}

// Age buckets of a "last mail" time, relative to now.
const (
	ageFuture = iota
	age30
	age90
	age365
	ageOver
	numAges
)

var ageNames = [numAges]string{"in the future", "<=30 d", "<=90 d", "<=1 y", ">1 y"}

func ageBucket(now, t time.Time) int {
	a := now.Sub(t)
	switch {
	case a < 0:
		return ageFuture
	case a <= 30*day:
		return age30
	case a <= 90*day:
		return age90
	case a <= 365*day:
		return age365
	}
	return ageOver
}

// directionCounts is one signal's buckets: within a day, then later/earlier
// per band.
type directionCounts struct {
	within         int
	later, earlier [numBuckets]int
}

func (c *directionCounts) add(stored, truth time.Time) {
	b, later := bucketDelta(stored.Sub(truth))
	switch {
	case b == bucketWithinDay:
		c.within++
	case later:
		c.later[b]++
	default:
		c.earlier[b]++
	}
}

// sampleRow is one listed address.
type sampleRow struct {
	address                 string
	storedFirst, storedLast time.Time
	trueFirst, trueLast     time.Time
}

type contactDatesReport struct {
	now                time.Time
	examined           int
	withMail           int
	withoutMail        int
	withFrom, withSent int
	withBoth           int
	noMailExamples     []string
	first, last        directionCounts
	trueAge, storedAge [numAges]int
	sample             []sampleRow
	sampleN            int
	sent               sentStats
}

// buildContactDatesReport is the pure core: rows (from the store aggregate) and
// the sent-side spans in, counts out. rows are expected sorted by address.
func buildContactDatesReport(rows []store.ContactMailDate, sent map[string]*span, now time.Time, sampleN int) *contactDatesReport {
	rep := &contactDatesReport{now: now, sampleN: sampleN}
	var withMail []sampleRow
	for _, r := range rows {
		rep.examined++
		trueFirst, trueLast, hasFrom, hasSent, _ := trueDates(r, sent)
		if hasFrom {
			rep.withFrom++
		}
		if hasSent {
			rep.withSent++
		}
		if hasFrom && hasSent {
			rep.withBoth++
		}
		if !hasFrom && !hasSent {
			rep.withoutMail++
			if len(rep.noMailExamples) < sampleN {
				rep.noMailExamples = append(rep.noMailExamples, r.Address)
			}
			continue
		}
		rep.withMail++
		sr := sampleRow{address: r.Address, storedFirst: r.FirstSeenAt, storedLast: r.LastSeenAt,
			trueFirst: trueFirst, trueLast: trueLast}
		rep.first.add(sr.storedFirst, sr.trueFirst)
		rep.last.add(sr.storedLast, sr.trueLast)
		rep.trueAge[ageBucket(now, sr.trueLast)]++
		rep.storedAge[ageBucket(now, sr.storedLast)]++
		withMail = append(withMail, sr)
	}
	rep.sample = pickSample(withMail, sampleN)
	return rep
}

// pickSample takes n rows evenly spaced through the (address-sorted) input, so
// the sample spans the whole list rather than its first letter.
func pickSample(rows []sampleRow, n int) []sampleRow {
	if n <= 0 || len(rows) == 0 {
		return nil
	}
	if n >= len(rows) {
		return rows
	}
	out := make([]sampleRow, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, rows[i*len(rows)/n])
	}
	return out
}

func (r *contactDatesReport) print() { r.fprint(os.Stdout) }

func (r *contactDatesReport) fprint(w io.Writer) {
	_, _ = fmt.Fprintf(w, "contact-dates-report (read-only); now = %s\n", r.now.Format(time.RFC3339))
	_, _ = fmt.Fprintln(w, "true dates: messages.received_at; stored: contact_addresses.first_seen_at / last_seen_at")

	_, _ = fmt.Fprintln(w, "\n== Totals ==")
	_, _ = fmt.Fprintf(w, "addresses examined:             %d\n", r.examined)
	_, _ = fmt.Fprintf(w, "  with mail found:              %d\n", r.withMail)
	_, _ = fmt.Fprintf(w, "    from the address:           %d\n", r.withFrom)
	_, _ = fmt.Fprintf(w, "    sent to the address:        %d\n", r.withSent)
	_, _ = fmt.Fprintf(w, "    both directions:            %d\n", r.withBoth)
	_, _ = fmt.Fprintf(w, "  WITHOUT any mail found:       %d\n", r.withoutMail)
	if len(r.noMailExamples) > 0 {
		_, _ = fmt.Fprintf(w, "  examples without mail (up to %d):\n", r.sampleN)
		for _, a := range r.noMailExamples {
			_, _ = fmt.Fprintf(w, "    %s\n", a)
		}
	}
	_, _ = fmt.Fprintf(w, "bounds: %d messages walked for the sent side, %d from the user's own identities, %d of those unreadable\n",
		r.sent.examined, r.sent.own, r.sent.unreadable)
	if r.sent.unreadable > 0 {
		_, _ = fmt.Fprintln(w, "  (recipients of unreadable messages are missing from the sent side)")
	}
	_, _ = fmt.Fprintln(w, "  drafts excluded; trashed messages included; all buckets below cover the addresses with mail only")

	_, _ = fmt.Fprintf(w, "\n== Stored vs true, per signal (%d addresses with mail) ==\n", r.withMail)
	printDirection(w, "first_seen_at vs earliest mail", &r.first)
	printDirection(w, "last_seen_at vs latest mail", &r.last)

	_, _ = fmt.Fprintln(w, "\n== Age of the latest mail (what a \"recently in touch\" sort sees) ==")
	_, _ = fmt.Fprintf(w, "%-16s %10s %10s\n", "", "true", "stored")
	for i, n := range ageNames {
		if i == ageFuture && r.trueAge[i] == 0 && r.storedAge[i] == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "%-16s %10d %10d\n", n, r.trueAge[i], r.storedAge[i])
	}

	_, _ = fmt.Fprintf(w, "\n== Sample (%d of %d addresses with mail, evenly spaced by address) ==\n", len(r.sample), r.withMail)
	for _, s := range r.sample {
		_, _ = fmt.Fprintf(w, "%s\n  stored first %s  true first %s\n  stored last  %s  true last  %s\n",
			s.address, fmtDate(s.storedFirst), fmtDate(s.trueFirst), fmtDate(s.storedLast), fmtDate(s.trueLast))
	}
}

func fmtDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

func printDirection(w io.Writer, title string, c *directionCounts) {
	_, _ = fmt.Fprintf(w, "\n%s\n", title)
	_, _ = fmt.Fprintf(w, "  within 1 d:                 %d\n", c.within)
	for i := bucketDay30; i < numBuckets; i++ {
		_, _ = fmt.Fprintf(w, "  stored later,   %-9s %d\n", bucketNames[i]+":", c.later[i])
	}
	for i := bucketDay30; i < numBuckets; i++ {
		_, _ = fmt.Fprintf(w, "  stored earlier, %-9s %d\n", bucketNames[i]+":", c.earlier[i])
	}
}
