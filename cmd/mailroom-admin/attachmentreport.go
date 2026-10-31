// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/clamd"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin attachment-report` — increment 0 of Forgejo #333 (optional
// ClamAV attachment scanning): measure the corpus BEFORE standing anything up.
//
// Report-only by default. What it decides — whether a scanner is worth building
// here and what shape it should take — is a decision, not a mutation.
//
// It grew ONE mutation, behind --apply, and only because it found something to
// repair: the has_attachments drift on signed sent mail (#377). Even then it
// rewrites nothing except rows whose stored flag disagrees with their own bytes,
// and only to what those bytes say. Everything else here stays read-only.
//
// ⚠ THE TABLE THAT LOOKS LIKE THE ANSWER WAS EMPTY FOR THE WHOLE LIFE OF THE
// PROJECT, and this report exists because of it. db/schema.sql carried
// app.attachments — filename, content_type, size_bytes, content_hash and a
// scan_status column — since the baseline commit, and it is the obvious thing
// for this report to query. It would have returned zero for every row: nothing
// wrote it, not ingest, not compose, not the importer. Measured at increment 0
// rather than assumed: no rows against a good share of messages carrying
// has_attachments.
//
// #393 made it a written table and #395 gave its verdicts a timestamp, so the
// projection sections below now read real rows. The paragraph stays because the
// reason this command reads the MESSAGE BYTES rather than the table has not
// changed: the bytes are the authority, and the disagreement between them and
// the projection is the number worth printing.
//
// ⚠ AN EARLIER VERSION OF THIS COMMENT SAID THAT GUARD IS "STRUCTURALLY INCAPABLE
// OF FIRING". THAT WAS WRONG, and the error was propagated to #333, #378 and the
// first draft of ADR-027 before being checked against the code. There is ONE
// guard; it runs on every draft retire, purge, collapse and account delete; and
// it is integration-tested in both directions. What is true is narrower: its
// ATTACHMENTS CLAUSE matches nothing while the table is empty, and goes
// production-live the moment the projection populates content_hash (ADR-027 §3).
//
// Measured on a live instance rather than inferred: app.attachments held no
// rows while a good share of messages carried has_attachments. The population
// is real; only the projection is missing.
//
// So this report READS THE MESSAGE BYTES, and its counts are per-signal for the
// usual reason (#177, #191, #206): one total cannot distinguish "there is
// nothing to scan" from "the thing I measured is always empty", and this corpus
// contains a table that is exactly the second.
//
// ⚠ IT USES THE SYSTEM'S OWN DEFINITION OF "ATTACHMENT", NOT ITS OWN.
// mime.Message.Attachments is what ingest counts to set has_attachments
// (internal/ingest: `p.HasAttachments = len(m.Attachments) > 0`) and what JMAP
// exposes. A report that invented a stricter or looser rule would answer a
// question nobody else in the system asks, and its numbers could not be
// compared with anything.
func runAttachmentReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attachment-report", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "list every attachment (type, size, filename), not just the counts")
	userFlag := fs.String("user", "", "restrict the report to one user id (default: every active user)")
	// ⚠ OPT-IN, AND NOTHING ELSE THIS COMMAND DOES MUTATES (#177). The report is
	// the deliverable; repairing is a separate, explicit decision. --apply ONLY
	// rewrites app.messages.has_attachments where the stored flag disagrees with
	// the message's own bytes, and only to what those bytes say.
	apply := fs.Bool("apply", false,
		"repair has_attachments where it disagrees with the message bytes (default: report only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("attachment-report: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("attachment-report: parse -user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	// ⚠ READ ONCE, BEFORE THE WALK, because the per-user backlog count needs it.
	// Reading it per user would also let the number move mid-report, so two users'
	// figures could be against different signature databases and their sum would
	// be against neither.
	current := currentScannerVersion(ctx, d.log)

	total := newAttachAgg()
	for _, uid := range users {
		agg, err := attachmentsForUser(ctx, d, uid, *verbose, *apply, current)
		if err != nil {
			return err
		}
		total.merge(agg)
	}
	total.print(len(users), current)
	return nil
}

// currentScannerVersion asks the configured daemon what it is running, for the
// stamped-vs-current comparison (#395, ADR-027 §13).
//
// ⚠ IT RETURNS THE ZERO VALUE RATHER THAN FAILING, and the report prints that
// difference rather than smoothing it over. "No scanner configured" and "the
// scanner is unreachable" both leave the comparison without a right-hand side,
// and a report that quietly rendered either as "everything is current" would be
// asserting freshness it never checked — the exact fail-open §5c is about.
func currentScannerVersion(ctx context.Context, log *slog.Logger) clamd.Version {
	sock := strings.TrimSpace(os.Getenv(config.EnvClamdSocket))
	if sock == "" {
		return clamd.Version{}
	}
	c := &clamd.Client{Network: "unix", Address: sock}
	v, err := c.Version(ctx)
	if err != nil {
		log.Warn("attachment-report: the scanner did not answer; stamped-vs-current will be reported as uncomputable",
			"clamd_socket", sock, "err", err)
		return clamd.Version{}
	}
	return v
}

// clamdStreamMaxLength is the largest attachment clamd will accept over
// INSTREAM. Anything over it is REFUSED rather than scanned — and a scanner that
// silently skips the biggest files while reporting nothing is the worst
// available outcome, so it is counted separately from the start (#333).
//
// ⚠ 100 MB, MEASURED, NOT 25 MB AS DOCUMENTED. This constant said 25 MB — which
// is upstream's DOCUMENTED default and is wrong about a deployed daemon: the
// official clamav/clamav image reports StreamMaxLength = 104857600 (probed
// 2026-09-01, #394). ADR-027 says 25 MB for the same reason and is corrected in
// the same commit. With the old number this column counted files clamd scans
// perfectly well as "over the limit", which is precisely the kind of number
// nobody audits because it looks plausible.
//
// It must stay equal to internal/clamd.MaxStreamBytes and to
// deploy/clamd.conf's StreamMaxLength; the report is only as true as that
// agreement.
const clamdStreamMaxLength = 100 * 1024 * 1024

type attachAgg struct {
	scanned    int // messages whose bytes were examined
	unreadable int // blob unreadable or unparsable — NOT counted as "no attachments"
	truncated  int // mime parsing hit a defensive limit; the tail was never seen
	nested     int // messages carrying embedded message/rfc822

	withAttachments int // messages the parser says carry >= 1 attachment
	flagSaysYes     int // messages whose stored has_attachments is true

	// ⚠ PER MESSAGE, NOT TWO TOTALS. A totals comparison cannot tell 4 newly
	// seen from 6 newly seen and 2 lost, and those are different bugs.
	parserOnlyLocal  int // parser sees attachments, flag says no — locally authored
	parserOnlyRemote int // ...same, but received mail
	flagOnly         int // flag says yes, parser sees none
	disagreeIDs      []string
	repaired         int // rows --apply actually rewrote (0 without it)
	attachments      int // attachment parts in total

	// Verdict freshness (#395, ADR-027 §5c). Per tier and per stamped version,
	// never a total: "0 stale" and "0 stale because nothing was ever scanned"
	// are the same number and opposite answers.
	tiers           map[string]store.TierCount
	neverScanned    int
	stampedVersions map[int]int
	// The backlog as the JOB sees it, not as a second computation over the same
	// columns. ⚠ THE SECOND COMPUTATION IS WHY #409 WAS INVISIBLE: this report
	// derived "due for re-scan" from stamped versions alone and never learned
	// about the digest arm, so it could print 0 while the job had work forever.
	dueMessages        int
	noDigest           int
	noDigestPossible   int
	attemptedAtCurrent int
	oldestVerdict      *time.Time

	byDeclared map[string]int
	bySniffed  map[string]int
	mismatch   map[string]int // "declared -> sniffed", only where they disagree

	sizeBuckets map[string]int
	overClamd   int
	largest     int

	zipShaped   int // sniffs as application/zip
	zipIsOffice int // ...of which are declared as an OOXML office type

	// The app.attachments projection (ADR-027 §3, increment 1). COUNTED, not
	// assumed empty: "0 rows" and "0 rows because nothing writes this table" are
	// the same number and different findings.
	attRows int
	// namedTextNotProjected is #413.3's population: text/* leaves JMAP calls
	// attachments and the projection does not, so nothing ever scans them.
	namedTextNotProjected int
	attWithContentHash    int
	attMessagesWithRows   int
	attByState            map[string]int

	// The quarantine (#396, ADR-027 §8): releases the user has made, and the
	// rows those cover.
	releases       int
	releasedRows   int
	quarantinedNow int

	// CONTENT DUPLICATION, computed from real digests over the decoded bytes
	// (#396). ⚠ THE NUMBER THIS REPLACES WAS AN UPPER BOUND AND SAID SO: before
	// there was a digest anywhere, the only available proxy was
	// (filename, size, content_type) equality, which put most rows in a
	// group spanning more than one message — while eight `signature.asc` at
	// exactly 307 bytes proved the proxy loose, being eight DIFFERENT PGP
	// signatures. This is the measurement that bound was standing in for, and
	// it is the population a content-keyed quarantine acts on.
	//
	// Costs nothing extra: the loop below has already decoded every part.
	// Memory is O(distinct attachment contents), which on a large archive is a
	// real cost and is stated rather than discovered.
	digestRows map[string]int
	digestMsgs map[string]int
}

func newAttachAgg() *attachAgg {
	return &attachAgg{
		byDeclared:      map[string]int{},
		bySniffed:       map[string]int{},
		mismatch:        map[string]int{},
		sizeBuckets:     map[string]int{},
		attByState:      map[string]int{},
		digestRows:      map[string]int{},
		digestMsgs:      map[string]int{},
		tiers:           map[string]store.TierCount{},
		stampedVersions: map[int]int{},
	}
}

func (a *attachAgg) merge(b *attachAgg) {
	a.scanned += b.scanned
	a.unreadable += b.unreadable
	a.truncated += b.truncated
	a.nested += b.nested
	a.withAttachments += b.withAttachments
	a.flagSaysYes += b.flagSaysYes
	a.parserOnlyLocal += b.parserOnlyLocal
	a.parserOnlyRemote += b.parserOnlyRemote
	a.flagOnly += b.flagOnly
	a.repaired += b.repaired
	a.disagreeIDs = append(a.disagreeIDs, b.disagreeIDs...)
	a.attachments += b.attachments
	a.overClamd += b.overClamd
	a.zipShaped += b.zipShaped
	a.zipIsOffice += b.zipIsOffice
	a.attRows += b.attRows
	a.namedTextNotProjected += b.namedTextNotProjected
	a.attWithContentHash += b.attWithContentHash
	a.attMessagesWithRows += b.attMessagesWithRows
	a.releases += b.releases
	a.releasedRows += b.releasedRows
	a.quarantinedNow += b.quarantinedNow
	if b.largest > a.largest {
		a.largest = b.largest
	}
	for _, m := range []struct{ dst, src map[string]int }{
		{a.byDeclared, b.byDeclared}, {a.bySniffed, b.bySniffed},
		{a.mismatch, b.mismatch}, {a.sizeBuckets, b.sizeBuckets},
		{a.attByState, b.attByState},
		{a.digestRows, b.digestRows}, {a.digestMsgs, b.digestMsgs},
	} {
		for k, v := range m.src {
			m.dst[k] += v
		}
	}
	// Freshness merges across users the same way: summed per tier and per
	// version, with the OLDEST verdict winning, because the question the number
	// answers is "how stale is the stalest thing here".
	for k, v := range b.tiers {
		t := a.tiers[k]
		t.Tier = k
		t.Messages += v.Messages
		t.Attachments += v.Attachments
		a.tiers[k] = t
	}
	for k, v := range b.stampedVersions {
		a.stampedVersions[k] += v
	}
	a.neverScanned += b.neverScanned
	a.dueMessages += b.dueMessages
	a.noDigest += b.noDigest
	a.noDigestPossible += b.noDigestPossible
	a.attemptedAtCurrent += b.attemptedAtCurrent
	if b.oldestVerdict != nil && (a.oldestVerdict == nil || b.oldestVerdict.Before(*a.oldestVerdict)) {
		a.oldestVerdict = b.oldestVerdict
	}
}

func attachmentsForUser(ctx context.Context, d *deps, userID ulid.ULID, verbose, apply bool, current clamd.Version) (*attachAgg, error) {
	agg := newAttachAgg()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return nil, err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("attachment-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return nil, fmt.Errorf("attachment-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	// The STORED flag, read once, as a cross-check against what the parser sees
	// now. They are set by the same code path (ingest counts mime.Attachments),
	// so they should agree exactly — a disagreement means the parser's
	// classification has drifted since those messages were ingested, which is
	// worth knowing before anything is built on either number.
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		agg.flagSaysYes, e = tx.CountMessagesWithAttachments(ctx)
		if e != nil {
			return e
		}
		cov, e := tx.AttachmentCoverage(ctx)
		if e != nil {
			return e
		}
		agg.attRows, agg.attWithContentHash = cov.Rows, cov.WithContentHash
		agg.attMessagesWithRows = cov.MessagesWithRows
		for k, v := range cov.ByScanStatus {
			agg.attByState[k] += v
		}
		// Verdict freshness (#395), in the same transaction so the tier counts
		// and the status counts describe one consistent snapshot.
		fresh, e := tx.AttachmentVerdictCoverageFor(ctx)
		if e != nil {
			return e
		}
		for _, t := range fresh.Tiers {
			agg.tiers[t.Tier] = t
		}
		for k, v := range fresh.StampedVersions {
			agg.stampedVersions[k] += v
		}
		agg.neverScanned += fresh.NeverScanned
		agg.oldestVerdict = fresh.OldestVerdict
		// THE BACKLOG, BY THE JOB'S OWN PREDICATE (#409). Only computable
		// against a live signature version, hence the guard: asking
		// "what is behind version 0" would produce a confident number about
		// nothing.
		if current.Database > 0 {
			bl, e := tx.RescanBacklogFor(ctx, current.Database)
			if e != nil {
				return e
			}
			agg.dueMessages += bl.DueMessages
			agg.noDigest += bl.NoDigest
			agg.noDigestPossible += bl.NoDigestPossible
			agg.attemptedAtCurrent += bl.AttemptedAtCurrent
		}
		// The quarantine (#396). Two release numbers, never one: a single
		// decision about content can open the gate on a dozen rows, and the gap
		// between them is the whole reason the release is keyed on content.
		agg.releases, agg.releasedRows, e = tx.CountAttachmentReleases(ctx)
		if e != nil {
			return e
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("attachment-report: count has_attachments: %w", err)
	}

	const batch = 200
	var after ulid.ULID
	for {
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("attachment-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// NOT silently skipped. A message whose bytes could not be read
				// is one this report did not measure, and letting it fall into
				// "no attachments" would understate exactly the population a
				// scanner exists for.
				agg.unreadable++
				d.log.Warn("attachment-report: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil {
				agg.unreadable++
				d.log.Warn("attachment-report: parse failed", "message", ref.ID, "err", err)
				continue
			}
			if msg.Truncated {
				// Parsing stopped at a defensive limit, so parts past it were
				// never seen. Those are attachments this report cannot count and
				// a scanner would have the same problem.
				agg.truncated++
			}
			if len(msg.Nested) > 0 {
				agg.nested++
			}
			// The per-message cross-check. Direction matters: the parser seeing
			// MORE than ingest recorded and seeing LESS are different faults,
			// and locally-authored mail is the population where a drift is most
			// expected (compose sets the flag from the REQUEST's attachment
			// list, before signing adds parts to the built bytes).
			parserSees := len(msg.Attachments) > 0
			if parserSees != ref.HasAttachments {
				if len(agg.disagreeIDs) < 20 {
					agg.disagreeIDs = append(agg.disagreeIDs, ref.ID.String())
				}
				switch {
				case parserSees && ref.LocallyAuthored:
					agg.parserOnlyLocal++
				case parserSees:
					agg.parserOnlyRemote++
				default:
					agg.flagOnly++
				}

				if apply {
					// The BYTES are the authority. This writes what the message
					// actually contains, never a guess about how it got wrong —
					// so it repairs both directions, and a message whose flag is
					// already right is not touched (the store guard makes that a
					// no-op that appends nothing).
					var changed bool
					if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
						var e error
						changed, e = tx.SetMessageHasAttachments(ctx, ref.ID, parserSees)
						return e
					}); err != nil {
						return nil, fmt.Errorf("attachment-report: repair %s: %w", ref.ID, err)
					}
					if changed {
						agg.repaired++
					}
				}
			}

			if !parserSees {
				continue
			}
			agg.withAttachments++

			// One digest per DISTINCT content within this message, so a message
			// carrying the same logo twice counts two rows and one message. The
			// distinction is the census: rows say how much a quarantine would
			// touch, messages say whether the content is genuinely shared.
			seenHere := map[string]bool{}

			// ⚠ THE PARTS THE TWO DEFINITIONS DISAGREE ABOUT (#413.3). JMAP calls a
			// text part with a FILENAME an attachment (api/body.go isBodyPart);
			// mime.IsAttachmentLeaf projects a text part only when the
			// DISPOSITION says attachment. So `text/html; inline; filename=x.html`
			// is listed to the client as an attachment, is downloadable, and is
			// never projected, never scanned and never gated — the no-row =>
			// not-blocked rule lets it through. #396 reconciled the two
			// definitions in one direction and did not check the other.
			//
			// Counted rather than assumed, because the fix (widening
			// IsAttachmentLeaf) changes what has_attachments counts, and #377
			// showed that number is load-bearing elsewhere.
			for _, p := range msg.Parts {
				if !strings.HasPrefix(p.ContentType, "text/") ||
					p.Disposition == "attachment" || p.Filename == "" {
					continue
				}
				agg.namedTextNotProjected++
			}

			for _, at := range msg.Attachments {
				agg.attachments++
				if len(at.Data) > 0 {
					// ⚠ Deliberately the same digest internal/attachscan.PartHash
					// stores: SHA-256 over the DECODED bytes. A census computed a
					// different way would be measuring a different population
					// from the one the quarantine acts on.
					sum := sha256.Sum256(at.Data)
					h := hex.EncodeToString(sum[:])
					agg.digestRows[h]++
					if !seenHere[h] {
						seenHere[h] = true
						agg.digestMsgs[h]++
					}
				}
				declared := normalizeMediaType(at.ContentType)
				agg.byDeclared[declared]++

				sniffed := sniffType(at.Data)
				agg.bySniffed[sniffed]++
				if declared != sniffed {
					agg.mismatch[declared+" -> "+sniffed]++
				}

				agg.sizeBuckets[sizeBucket(at.Size)]++
				if at.Size > agg.largest {
					agg.largest = at.Size
				}
				if at.Size > clamdStreamMaxLength {
					agg.overClamd++
				}
				if sniffed == "application/zip" {
					agg.zipShaped++
					if isOOXML(declared) {
						agg.zipIsOffice++
					}
				}
				if verbose {
					fmt.Printf("  %-40s %-32s %-32s %9d\n",
						attachName(at.Filename, 40), declared, sniffed, at.Size)
				}
			}
		}
	}
	return agg, nil
}

// sniffType is http.DetectContentType over the leading bytes — the same
// signature table a browser uses. It is a weak identifier by design (it knows a
// few dozen magic numbers), which is the point: the question here is not "what
// is this file really" but "does what it CLAIMS to be survive even a cursory
// look".
func sniffType(data []byte) string {
	if len(data) == 0 {
		return "(empty)"
	}
	n := len(data)
	if n > 512 {
		n = 512
	}
	return normalizeMediaType(http.DetectContentType(data[:n]))
}

func normalizeMediaType(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if t == "" {
		return "(none declared)"
	}
	return t
}

// isOOXML reports whether a declared type is one of the Office formats that ARE
// zip containers. Without this the archive count is actively misleading: every
// .docx and .xlsx sniffs as application/zip, so "N archives" would read as N
// suspicious nested containers when most of them are ordinary documents.
func isOOXML(declared string) bool {
	return strings.HasPrefix(declared, "application/vnd.openxmlformats-officedocument.") ||
		strings.HasPrefix(declared, "application/vnd.oasis.opendocument.")
}

func sizeBucket(n int) string {
	switch {
	case n < 64*1024:
		return "1. < 64 KB"
	case n < 1024*1024:
		return "2. 64 KB – 1 MB"
	case n < 5*1024*1024:
		return "3. 1 – 5 MB"
	case n <= clamdStreamMaxLength:
		return "4. 5 – 100 MB"
	default:
		return "5. > 100 MB (clamd refuses the stream)"
	}
}

func attachName(s string, n int) string {
	if s == "" {
		return "(no filename)"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (a *attachAgg) print(users int, current clamd.Version) {
	fmt.Printf("\n=== corpus ===\n")
	fmt.Printf("  users examined                 %d\n", users)
	fmt.Printf("  messages examined              %d\n", a.scanned)
	fmt.Printf("  messages with attachments      %d  (parser)\n", a.withAttachments)
	fmt.Printf("  messages with has_attachments  %d  (stored at ingest)\n", a.flagSaysYes)
	dis := a.parserOnlyLocal + a.parserOnlyRemote + a.flagOnly
	if dis > 0 {
		fmt.Printf("  ⚠ THEY DISAGREE on %d message(s), BY DIRECTION:\n", dis)
		fmt.Printf("      parser sees attachments, flag says no — LOCALLY AUTHORED  %d\n", a.parserOnlyLocal)
		fmt.Printf("      parser sees attachments, flag says no — received mail     %d\n", a.parserOnlyRemote)
		fmt.Printf("      flag says yes, parser sees none                           %d\n", a.flagOnly)
		fmt.Printf("    Both numbers come from len(mime.Attachments), so they should agree.\n")
		if a.parserOnlyLocal == dis {
			fmt.Printf("    ALL of them are locally authored, which points at compose rather than\n")
			fmt.Printf("    at parser drift: compose sets has_attachments from the REQUEST's\n")
			fmt.Printf("    attachment list, while the parser reads the BUILT bytes — and PGP\n")
			fmt.Printf("    signing adds a part to those bytes after the flag is decided.\n")
		}
		if len(a.disagreeIDs) > 0 {
			fmt.Printf("    message ids: %s\n", strings.Join(a.disagreeIDs, " "))
		}
	} else {
		fmt.Printf("  — the two agree on every message, in both directions.\n")
	}
	fmt.Printf("  attachment parts               %d\n", a.attachments)

	a.printProjection(current)

	fmt.Printf("\n=== what this report could NOT see ===\n")
	fmt.Printf("  unreadable (blob or parse)     %d\n", a.unreadable)
	fmt.Printf("  truncated by a parse limit     %d  (parts past the limit were never seen)\n", a.truncated)
	fmt.Printf("  carrying embedded messages     %d  (attachments nested inside message/rfc822)\n", a.nested)
	// All THREE have to be zero for the counts above to be a total, and nested is
	// not merely informational: Message.Attachments deliberately excludes parts
	// belonging to an embedded message/rfc822 (internal/mime/message.go), so an
	// attachment inside a forwarded mail is invisible to len(msg.Attachments)
	// here — and, for the same reason, to the has_attachments ingest stores.
	// ⚠ THE PARTS THE TWO DEFINITIONS OF "ATTACHMENT" DISAGREE ABOUT (#413).
	// A `text/*` leaf with a FILENAME but no `attachment` disposition is listed
	// to a JMAP client as an attachment and is downloadable — and is NOT
	// projected, so nothing scans it and the gate has no row to refuse on.
	// Printed whether zero or not: zero here is the finding that the corpus
	// cannot exercise the gap, which is different from the gap not existing.
	fmt.Printf("  text parts named but not projected %d  (JMAP calls these attachments; nothing scans them)\n",
		a.namedTextNotProjected)
	if a.namedTextNotProjected > 0 {
		fmt.Printf("    ⚠ FINDING: those parts are downloadable and UNSCANNED. mime.IsAttachmentLeaf\n")
		fmt.Printf("      projects a text part only when its disposition says `attachment`, while\n")
		fmt.Printf("      api/body.go calls any named text part an attachment. Widening the first\n")
		fmt.Printf("      closes the gap and CHANGES has_attachments, which #377 showed is\n")
		fmt.Printf("      load-bearing — so it is a decision, not a patch.\n")
	}

	if a.unreadable == 0 && a.truncated == 0 && a.nested == 0 {
		fmt.Printf("  — all three zero: the counts above are over the WHOLE corpus, not a subset.\n")
	} else {
		fmt.Printf("  ⚠ NOT ZERO, so every count above is a LOWER BOUND.\n")
		if a.nested > 0 {
			fmt.Printf("    In particular, attachments carried INSIDE those %d embedded message(s)\n", a.nested)
			fmt.Printf("    are counted nowhere above: the parser records them on the nested\n")
			fmt.Printf("    message, not on this one.\n")
		}
	}

	if a.attachments == 0 {
		fmt.Printf("\n=== no attachments found ===\n")
		fmt.Printf("  %d messages examined.\n", a.scanned)
		// Zero is a statement about the CORPUS only when the report could see all
		// of it. Otherwise it is a statement about this report's reach, which is a
		// weaker claim and must not be printed as the same one.
		if a.unreadable == 0 && a.truncated == 0 && a.nested == 0 {
			fmt.Printf("  That is a finding, not an empty result: nothing was unreadable,\n")
			fmt.Printf("  truncated or nested, so a scanner would have nothing to do here.\n")
		} else {
			fmt.Printf("  ⚠ That is NOT a finding: %d unreadable, %d truncated and %d carrying an\n", a.unreadable, a.truncated, a.nested)
			fmt.Printf("  embedded message were not fully examined. Zero here means \"none found\n")
			fmt.Printf("  where this report could look\", which is a weaker claim.\n")
		}
		return
	}

	printAttachCounts("declared type (what the message claims)", a.byDeclared)
	printAttachCounts("sniffed type (what the leading bytes look like)", a.bySniffed)

	fmt.Printf("\n=== declared vs sniffed: where they disagree ===\n")
	if len(a.mismatch) == 0 {
		fmt.Printf("  none — every attachment's declared type survived a look at its bytes.\n")
	} else {
		printAttachCounts("", a.mismatch)
		fmt.Printf("  ⚠ Most disagreements are benign: http.DetectContentType knows only a few\n")
		fmt.Printf("    dozen signatures, so anything it cannot place reads as octet-stream.\n")
		fmt.Printf("    The rows worth reading are the ones where BOTH sides are specific.\n")
	}

	printAttachCounts("size", a.sizeBuckets)
	fmt.Printf("  largest attachment             %d bytes\n", a.largest)
	// ⚠ THE THRESHOLD IS RENDERED FROM THE CONSTANT, NEVER RESTATED (#421). This
	// line said "25 MB" for a release after the constant beside it was corrected
	// to 100 MB — a label wrong by 4x on an AUDIT SURFACE, whose whole value is
	// that its statements can be trusted. The count was always computed from the
	// constant, so nothing was ever miscounted; a reader asking whether a 40 MB
	// attachment would be refused simply got a confident wrong answer from the
	// label and a silent right one from the number.
	fmt.Printf("  over clamd's %d MB limit       %d  ", clamdStreamMaxLength/(1024*1024), a.overClamd)
	if a.overClamd == 0 {
		fmt.Printf("(nothing would be refused unscanned)\n")
	} else {
		fmt.Printf("⚠ these would be REFUSED by a default clamd, not scanned\n")
	}

	fmt.Printf("\n=== zip-shaped (where a scanner's nesting limits bite) ===\n")
	fmt.Printf("  sniffs as application/zip       %d\n", a.zipShaped)
	fmt.Printf("  ...declared as an Office format %d\n", a.zipIsOffice)
	fmt.Printf("  ...genuine archives             %d\n", a.zipShaped-a.zipIsOffice)
	fmt.Printf("  ⚠ .docx/.xlsx/.odt ARE zip containers. Counting \"archives\" by sniffing\n")
	fmt.Printf("    alone would report ordinary documents as nested containers.\n")

	fmt.Printf("\n=== what a scanner would and would not see ===\n")
	fmt.Printf("  ⚠ NOTHING IS CURRENTLY REFUSED ON TYPE, so the answer is \"all of it\".\n")
	fmt.Printf("  #333 assumed a population already blocked as dangerous types, which a\n")
	fmt.Printf("  scanner would never see. That population is EMPTY: internal/api/blob.go's\n")
	fmt.Printf("  safeContentType does not refuse anything — it DOWNGRADES an unrecognised\n")
	fmt.Printf("  type to application/octet-stream and serves it with Content-Disposition:\n")
	fmt.Printf("  attachment and nosniff. The download path returns no 4xx on type at all;\n")
	fmt.Printf("  only the PREVIEW path refuses (415 previewUnsupported).\n")
	fmt.Printf("  Spec §11 says \"dangerous types blocked\"; the code NEUTRALISES rather than\n")
	fmt.Printf("  blocks. Both are defensible, but they are not the same claim, and #333's\n")
	fmt.Printf("  scoping question turns on which one is true.\n")
}

// printProjection reports the app.attachments projection — ADR-027 increment 1,
// deliberately built BEFORE increment 2 writes the table.
//
// ⚠ THE EMPTY CASE IS THE WHOLE REASON THIS EXISTS. An all-zero census here is a
// FINDING ("nothing has ever written this table"), not an empty result, and the
// two are indistinguishable from a row count alone. What separates them is
// comparing coverage against the messages that DO carry attachments — so that
// comparison is printed even when, especially when, the count is zero (#155, #176).
func (a *attachAgg) printProjection(current clamd.Version) {
	fmt.Printf("\n=== app.attachments: the projection a scan verdict would live in ===\n")
	fmt.Printf("  rows                           %d\n", a.attRows)
	fmt.Printf("  ...with content_hash set       %d  (bytes extracted to a blob)\n", a.attWithContentHash)
	fmt.Printf("  messages having >= 1 row       %d\n", a.attMessagesWithRows)
	fmt.Printf("  messages with has_attachments  %d  (what the line above should equal)\n", a.flagSaysYes)

	switch {
	case a.attRows == 0 && a.flagSaysYes == 0:
		fmt.Printf("  — both zero. This corpus carries no attachments at all, so an empty\n")
		fmt.Printf("    projection is the correct state rather than a missing one.\n")
	case a.attRows == 0:
		// The state every deployment is in until increment 2 lands.
		fmt.Printf("  ⚠ THE PROJECTION DOES NOT EXIST. %d message(s) carry attachments and NONE\n", a.flagSaysYes)
		fmt.Printf("    of them have a row here, because no production code writes this table\n")
		fmt.Printf("    (ADR-027 §3). Read this as \"not built yet\", NOT as \"nothing to scan\".\n")
		fmt.Printf("    Everything a verdict needs — per-attachment identity, and the\n")
		fmt.Printf("    content_hash a scanner keys its cache on — is missing until it is.\n")
	case a.attMessagesWithRows < a.flagSaysYes:
		fmt.Printf("  ⚠ PARTIAL: %d message(s) with attachments have no row here. A verdict\n", a.flagSaysYes-a.attMessagesWithRows)
		fmt.Printf("    cannot be stored for those, and a scanner keyed off this table would\n")
		fmt.Printf("    skip them SILENTLY — the failure this report exists to make loud.\n")
	case a.attMessagesWithRows > a.flagSaysYes:
		fmt.Printf("  ⚠ MORE messages have rows (%d) than carry has_attachments (%d).\n", a.attMessagesWithRows, a.flagSaysYes)
		fmt.Printf("    Check the has_attachments cross-check above before trusting either.\n")
	default:
		fmt.Printf("  — every message carrying attachments has at least one row.\n")
	}

	if len(a.attByState) > 0 {
		printAttachCounts("scan_status", a.attByState)
		// ⚠ THIS LIST GREW IN v1.59.0 AND THE OLD ONE WOULD HAVE LIED. It read
		// 'unscanned'|'clean'|'flagged' and called anything else "a finding" —
		// so the moment the scanner shipped, every legitimate `unknown` (an
		// encrypted archive, a truncated walk, a file over clamd's limit) would
		// have been reported as a defect. A report that flags normal operation
		// is a report nobody reads.
		var unexpected []string
		for st := range a.attByState {
			switch st {
			// ⚠ 'released' IS STILL ACCEPTED HERE AND IS NO LONGER WRITTEN.
			// Migration 00044 moved a release out of this column and into
			// app.attachment_releases, keyed on content. No code ever wrote the
			// value, so no row can carry it — but listing it as "unexpected"
			// would turn a hypothetical legacy row into a FINDING that tells an
			// operator nothing they can act on, and this report's job is to
			// distinguish faults from normal operation.
			case "unscanned", "clean", "flagged", "unknown", "released":
			default:
				unexpected = append(unexpected, st)
			}
		}
		sort.Strings(unexpected)
		if len(unexpected) > 0 {
			fmt.Printf("  ⚠ FINDING: state(s) outside ADR-027 §5's five: %s\n", strings.Join(unexpected, ", "))
			fmt.Printf("    scan_status is a bare text column, not a constrained enum, so a\n")
			fmt.Printf("    typo in a writer lands here rather than being rejected.\n")
		}
		// ⚠ 'unknown' is NOT a fault, and saying so is the point: ADR-027 §5b
		// gives it the same enforcement as 'flagged' with a different
		// explanation. What it does mean is a file nothing looked inside, so
		// the count is worth watching rather than worth alarming about.
		if n := a.attByState["unknown"]; n > 0 {
			fmt.Printf("  %d attachment(s) are 'unknown' — attempted or deliberately skipped, and\n", n)
			fmt.Printf("    NOT a fault: encrypted archives, truncated walks and over-limit files\n")
			fmt.Printf("    all land here by design (ADR-027 §5a/§5b/§6).\n")
		}
		// ⚠ 'unscanned' DEGRADES the answer rather than vanishing into it
		// (§5d): "0 flagged" and "0 flagged because nothing was scanned" are
		// the same number and opposite answers.
		if n := a.attByState["unscanned"]; n > 0 {
			fmt.Printf("  %d attachment(s) are still 'unscanned' — no scanner ran on them. Any\n", n)
			fmt.Printf("    'is this mailbox clean?' claim is bounded by that number, not by the\n")
			fmt.Printf("    flagged count. Coverage of already-stored mail is ADR-027 increment 4.\n")
		}
	}

	printVerdictFreshness(a, current)
	printQuarantine(a)
}

// printQuarantine is the enforcement half (#396, ADR-027 §8): what the download
// gate would refuse right now, what the user has released, and the CONTENT
// DUPLICATION that makes a release act on more than the row it was made from.
//
// ⚠ THE DUPLICATION CENSUS IS THE MEASUREMENT THAT JUSTIFIED THE DESIGN, and it
// replaces a bound rather than an estimate. §8 says a quarantine "must hit all
// of them — the same attachment exists as N rows sharing a content_hash", but
// app.attachments.content_hash is a BLOB REFERENCE and is NULL on every row
// (measured: none had it set), so the join key it names did not exist. Before there was
// a digest, the closest available proxy — (filename, size, content_type)
// equality — put most rows in a multi-message group, and was provably
// loose: eight `signature.asc` at 307 bytes are eight different signatures. The
// numbers below are the real ones.
func printQuarantine(a *attachAgg) {
	fmt.Printf("\n=== quarantine (ADR-027 §8) ===\n")

	// What the gate refuses today. ⚠ Computed with attachscan.Blocked, the same
	// function the download route calls, so this cannot report a different
	// policy from the one being enforced.
	blocked := 0
	for _, st := range []string{"flagged", "unknown"} {
		blocked += a.attByState[st]
	}
	fmt.Printf("  parts the download gate would refuse: %d (flagged %d + unknown %d)\n",
		blocked, a.attByState["flagged"], a.attByState["unknown"])
	fmt.Printf("    ⚠ MINUS ANY RELEASED CONTENT below. 'unknown' blocks exactly as\n")
	fmt.Printf("    'flagged' does (§5b): otherwise the adversary never has to beat the\n")
	fmt.Printf("    scanner, only avoid it. 'unscanned' does NOT block.\n")
	fmt.Printf("  releases the user has made: %d, covering %d attachment row(s)\n",
		a.releases, a.releasedRows)
	if a.releases > 0 && a.releasedRows > a.releases {
		fmt.Printf("    (one decision about content reached more rows than it was made from —\n")
		fmt.Printf("     which is the behaviour §8 requires, visible rather than asserted)\n")
	}

	// The content census.
	var (
		distinct   = len(a.digestRows)
		sharedRows int
		sharedSets int
		largest    int
	)
	for h, msgs := range a.digestMsgs {
		if msgs > 1 {
			sharedSets++
			sharedRows += a.digestRows[h]
		}
		if a.digestRows[h] > largest {
			largest = a.digestRows[h]
		}
	}
	fmt.Printf("  content duplication, by SHA-256 over the decoded part bytes:\n")
	fmt.Printf("    %d attachment part(s) carry %d distinct content(s)\n", a.attachments, distinct)
	fmt.Printf("    %d part(s) in %d content group(s) that span MORE THAN ONE message\n",
		sharedRows, sharedSets)
	fmt.Printf("    largest single content group: %d part(s)\n", largest)
	switch {
	case a.attachments == 0:
		fmt.Printf("    ⚠ NOTHING WAS MEASURED — there are no attachment parts at all, so this\n")
		fmt.Printf("      section says nothing about whether the content key works.\n")
	case sharedSets == 0:
		fmt.Printf("    ⚠ NO CONTENT IS SHARED on this corpus, so the 'act on every copy' rule\n")
		fmt.Printf("      is UNEXERCISED here and its correctness rests on the tests, not on\n")
		fmt.Printf("      this instance. That is a fact about this mailbox today.\n")
	default:
		fmt.Printf("    a release or a quarantine on one of those groups moves every row in it,\n")
		fmt.Printf("    which is the ADR-027 §8 requirement this census exists to make visible.\n")
	}
}

// printVerdictFreshness is ADR-027 §13's deferred half: per re-scan tier, oldest
// verdict, and stamped-vs-current signature version (#395).
//
// ⚠ IT IS THE SINGLE AUDIT THAT CATCHES EVERY SILENT FAILURE IN THIS DESIGN —
// freshclam rot, a re-scan chunk that never runs, a version-stamp race. Until
// migration 00043 the columns behind it did not exist and this printed a
// "NOT YET REPORTABLE" note instead, which was the honest thing to print and is
// what this replaces.
//
// current is the daemon's live version, or the zero value when no scanner is
// configured — a distinction the output has to keep, because "everything is
// stamped at the current version" and "we have no idea what current is" look
// identical once collapsed.
func printVerdictFreshness(a *attachAgg, current clamd.Version) {
	fmt.Printf("\n=== verdict freshness (ADR-027 §5c) ===\n")

	fmt.Printf("  re-scan tiers, by the age of the MAIL:\n")
	for _, def := range store.RescanTiers {
		t := a.tiers[def.Name]
		cadence := "STOP"
		if def.Rescan {
			switch def.MinInterval {
			case 0:
				cadence = "every DB change"
			case 24 * time.Hour:
				cadence = "daily"
			case 7 * 24 * time.Hour:
				cadence = "weekly"
			default:
				cadence = "every " + def.MinInterval.String()
			}
		}
		fmt.Printf("    %-12s %5d messages %5d attachments   %-16s %s\n",
			def.Name, t.Messages, t.Attachments, cadence, def.Why)
	}
	// ⚠ AN EMPTY TIER IS A FINDING ABOUT THE TIER, NOT ABOUT THE MAILBOX. The
	// boundaries are judgement the ADR itself calls its thinnest-supported
	// decision, and it promised a revisit "once verdict coverage has run". A
	// band with nothing in it cannot participate in that revisit, so say so
	// rather than let a row of zeroes read as agreement.
	//
	// ⚠ BUT ONLY WHEN THERE IS DATA TO BE ABSENT FROM. On a corpus with no
	// attachments at all, every tier is empty for a reason that says nothing
	// about the boundaries, and printing four warnings there is how a reader
	// learns to skip them — which costs the one that matters later.
	var total int
	for _, def := range store.RescanTiers {
		total += a.tiers[def.Name].Attachments
	}
	if total > 0 {
		for _, def := range store.RescanTiers {
			if a.tiers[def.Name].Attachments == 0 {
				fmt.Printf("    ⚠ '%s' is EMPTY, so its boundary is untested against this corpus.\n", def.Name)
			}
		}
	}

	fmt.Printf("\n  verdict staleness:\n")
	if a.oldestVerdict == nil {
		fmt.Printf("    oldest verdict: NONE — nothing has ever been scanned.\n")
	} else {
		fmt.Printf("    oldest verdict: %s (%s ago)\n",
			a.oldestVerdict.Format("2006-01-02 15:04"),
			time.Since(*a.oldestVerdict).Truncate(time.Hour))
	}
	// ⚠ THE NUMBER THE SPECIFIED PREDICATE WOULD HAVE SKIPPED. ADR-027 §13 gives
	// the re-scan predicate as `scanned_db_version < current`, which matches
	// NOTHING for these rows because `NULL < n` is NULL. Printed on its own line
	// so a backlog that is never being cleared is a number rather than silence.
	fmt.Printf("    never scanned:  %d attachment(s) with no stamped version at all\n", a.neverScanned)

	if current.Database == 0 {
		fmt.Printf("\n  ⚠ NO SCANNER CONFIGURED (%s unset), so stamped-vs-current cannot be\n", config.EnvClamdSocket)
		fmt.Printf("    computed. This is NOT the same as 'everything is up to date' — the\n")
		fmt.Printf("    comparison simply has no right-hand side.\n")
		return
	}

	fmt.Printf("\n  signature database, live from the daemon:\n")
	fmt.Printf("    engine %s, database %d, published %s\n",
		current.Engine, current.Database, current.Published.Format("2006-01-02 15:04"))
	if age, ok := current.Age(time.Now()); ok {
		fmt.Printf("    database age: %s\n", age.Truncate(time.Minute))
	}
	// ⚠ §5c's FAIL-OPEN HALF. Every tier is gated on "only if the database
	// moved", so a freshclam that quietly stops updating suspends the entire
	// re-scan system with NO signal: clamd keeps answering, scans keep
	// succeeding, and the verdict ages shown above keep advancing because a scan
	// really did run — against a frozen database. Age is the only thing that
	// separates "has not moved" from "cannot move".
	if current.Stale(time.Now()) {
		fmt.Printf("    ⚠ FINDING: the signature database is STALE (over %s old, or its age is\n", clamd.MaxDatabaseAge)
		fmt.Printf("      unknown). Re-scan tiers gate on the version CHANGING, so a database\n")
		fmt.Printf("      that cannot change silently suspends re-scanning while every scan\n")
		fmt.Printf("      still reports success. Check the freshclam container.\n")
	}

	fmt.Printf("\n  stamped versions vs current (%d):\n", current.Database)
	vers := make([]int, 0, len(a.stampedVersions))
	for v := range a.stampedVersions {
		vers = append(vers, v)
	}
	sort.Ints(vers)
	if len(vers) == 0 {
		fmt.Printf("    (none — nothing carries a stamped version yet)\n")
	}
	behind := 0
	for _, v := range vers {
		marker := ""
		if v < current.Database {
			marker = "  <- behind, due for re-scan"
			behind += a.stampedVersions[v]
		}
		fmt.Printf("    %8d  %5d attachment(s)%s\n", v, a.stampedVersions[v], marker)
	}
	fmt.Printf("    attachments behind the current version: %d (+ %d never scanned)\n",
		behind, a.neverScanned)

	// ⚠ THE JOB'S OWN PREDICATE, NOT A SECOND COMPUTATION OVER THE SAME COLUMNS.
	// This line used to read "due for re-scan: N behind + M never scanned",
	// derived from stamped versions alone — it never learned about the digest
	// arm — so it could print `due for re-scan: 0` while the job had work
	// forever, which is exactly the state #409 describes and exactly the state
	// this section exists to make visible. v1.61.1 extracted a shared constant
	// so the SELECT and the COUNT could not drift, and missed this third
	// consumer: the check that exists to make a stalled backlog legible could
	// not see the stall. It now asks CountMessagesDueForRescan.
	fmt.Printf("\n  the re-scan queue, by the job's own predicate:\n")
	fmt.Printf("    due now:        %d message(s)\n", a.dueMessages)
	fmt.Printf("    already visited at version %d: %d attachment(s)\n",
		current.Database, a.attemptedAtCurrent)
	// ⚠ TWO POPULATIONS, NEVER ONE NUMBER. "no digest yet" and "no digest is
	// possible" are opposite findings: the first is a backfill that has not
	// arrived, the second is a part that decoded to zero bytes and is DONE.
	// Collapsing them is how a permanent fixed point looks like pending work —
	// and a permanently-due row at the head of an `ORDER BY m.id LIMIT 50`
	// starves everything behind it.
	fmt.Printf("    without a content digest: %d attachment(s), of which %d have no bytes\n",
		a.noDigest, a.noDigestPossible)

	// ⚠ NO "THE CHAIN HAS STOPPED" FINDING HERE, DELIBERATELY, AND THE REASON IS
	// worth keeping. The obvious one — "nothing is due and nothing has been
	// visited, so the job must be dead" — FIRES ON EVERY HEALTHY INSTANCE the
	// day this ships: attempted_* is NULL on every pre-existing row, and a
	// corpus already stamped at the current version is correctly not due, so
	// nothing visits it. It would have been a confident, wrong FINDING printed
	// on a working system. A false finding is not a harmless one: it is how a
	// reader learns to skip the warnings, which costs the one that matters
	// later. The two numbers above say the same thing without asserting a cause.
	if stuck := a.noDigest - a.noDigestPossible; stuck > 0 {
		fmt.Printf("    ⚠ %d attachment(s) could have a content digest and do not.\n", stuck)
		fmt.Printf("      A release is keyed on the digest, so those parts can be BLOCKED and\n")
		fmt.Printf("      CANNOT be released until one pass reads their bytes successfully.\n")
		if a.dueMessages == 0 {
			fmt.Printf("      None are due right now, so they are waiting on the next signature\n")
			fmt.Printf("      database change rather than on the job. If this number does not fall\n")
			fmt.Printf("      after one, their blobs are not readable — check the re-scan warnings.\n")
		}
	}
}

func printAttachCounts(title string, m map[string]int) {
	if title != "" {
		fmt.Printf("\n=== %s ===\n", title)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		fmt.Printf("  %-52s %d\n", k, m[k])
	}
}
