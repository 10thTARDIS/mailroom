// SPDX-License-Identifier: AGPL-3.0-only

package store

// Verdict freshness: the re-scan tiers and the census over them
// (#395, ADR-027 §5c, §13 increment 4).
//
// # One definition, because two would drift
//
// The tier boundaries are needed in two places — the report that says what is
// due, and the job that acts on it — and a report that disagrees with the job it
// audits is worse than no report. So they are declared once here, and the SQL
// below is GENERATED from that declaration rather than written alongside it.
// Same reasoning as internal/attachproj: the judgement lives once, and #350 is
// an open issue about exactly this pattern in this repo.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// RescanTier is one band of ADR-027 §5c's cadence table.
type RescanTier struct {
	// Name is the stable key used in reports and logs.
	Name string
	// MaxAge is the upper bound on the MAIL's age for this tier. Zero means
	// unbounded (the last tier).
	MaxAge time.Duration
	// Rescan reports whether this tier is re-scanned at all.
	Rescan bool
	// MinInterval is how long a verdict in this tier stays good. Zero with
	// Rescan true means "every signature-database change", which is the
	// strictest setting and has no time component.
	MinInterval time.Duration
	// Why is the one-line justification printed beside the count, so a reader
	// of the report does not have to hold the ADR open.
	Why string
}

// RescanTiers is ADR-027 §5c's cadence table, in order, most recent first.
//
// ⚠ THESE BOUNDARIES ARE JUDGEMENT AND HAVE NEVER BEEN VALIDATED. The ADR says
// so itself — "the thinnest-supported decision in this document… the boundaries
// derive from no measurement of this corpus's age distribution, and nothing in
// the design would reveal them being wrong" — and promises a revisit "once
// verdict coverage has run".
//
// ⚠ COVERAGE HAS NOW RUN, AND THE REVISIT STILL CANNOT HAPPEN. Measured on the
// instance that motivated this, 2026-09-01: the oldest attachment-bearing
// message was seven weeks old, so `> 6 months` held ZERO messages and zero
// attachments. The boundary is not merely unvalidated, it is unreachable there —
// and a mailbox of several years would be dominated by that same tier, which is
// the opposite shape. Nothing measured so far justifies moving a boundary.
//
// What that argues for is not a better guess but OBSERVABILITY:
// AttachmentVerdictCoverage prints the population of every tier, so the day the
// last one stops being empty is something seen rather than assumed. Do not treat
// these numbers as derived; they are a starting point with a counter attached.
var RescanTiers = []RescanTier{
	{
		Name: "<= 7 days", MaxAge: 7 * 24 * time.Hour, Rescan: true, MinInterval: 0,
		Why: "the detection gap is short, so recent mail is where nearly all the value is",
	},
	{
		Name: "<= 1 month", MaxAge: 30 * 24 * time.Hour, Rescan: true, MinInterval: 24 * time.Hour,
		Why: "daily — and close to decorative, since the database moves most days",
	},
	{
		Name: "1-6 months", MaxAge: 180 * 24 * time.Hour, Rescan: true, MinInterval: 7 * 24 * time.Hour,
		Why: "weekly",
	},
	{
		Name: "> 6 months", MaxAge: 0, Rescan: false,
		Why: "STOP — safe ONLY because the verdict's age is surfaced (ADR-027 §1a)",
	},
}

// tierCaseSQL builds the CASE expression that buckets a message by age, from
// RescanTiers, so the query cannot disagree with the table above.
func tierCaseSQL(column string) string {
	var b strings.Builder
	b.WriteString("CASE\n")
	for _, t := range RescanTiers {
		if t.MaxAge == 0 {
			continue // the unbounded tier is the ELSE
		}
		fmt.Fprintf(&b, "  WHEN %s > now() - interval '%d seconds' THEN '%s'\n",
			column, int64(t.MaxAge/time.Second), t.Name)
	}
	fmt.Fprintf(&b, "  ELSE '%s'\nEND", RescanTiers[len(RescanTiers)-1].Name)
	return b.String()
}

// TierCount is one tier's population.
type TierCount struct {
	Tier        string
	Messages    int
	Attachments int
}

// AttachmentVerdictCoverage is the freshness census (ADR-027 §13 increment 1's
// deferred half, delivered by increment 4).
//
// ⚠ IT IS THE SINGLE AUDIT THAT CATCHES EVERY SILENT FAILURE IN THIS DESIGN:
// freshclam rot, a re-scan chunk that never runs, a version-stamp race. Every
// field is per-population rather than a total, for the reason this project keeps
// relearning (#177, #191, #206): one number cannot distinguish "nothing to do"
// from "the thing I measured is always empty".
type AttachmentVerdictCoverage struct {
	// Tiers is the population of each §5c band.
	Tiers []TierCount
	// NeverScanned is the number of attachments with no stamped version at all.
	//
	// ⚠ THIS IS THE NUMBER THE SPECIFIED PREDICATE WOULD HAVE SKIPPED. ADR-027
	// §13 and #395 both give the re-scan predicate as
	// `WHERE scanned_db_version < current`, which matches NOTHING for these rows
	// because `NULL < n` is NULL rather than true. Reported separately so a
	// backlog that is not being cleared is visible as a number, not as silence.
	NeverScanned int
	// StampedVersions counts attachments by the signature version they were
	// scanned against — the "stamped vs current" comparison §13 asks for.
	StampedVersions map[int]int
	// OldestVerdict is the earliest scanned_at still standing, or nil when
	// nothing has been scanned.
	OldestVerdict *time.Time
}

// AttachmentVerdictCoverageFor reads the freshness census for one user.
func (tx *UserTx) AttachmentVerdictCoverageFor(ctx context.Context) (AttachmentVerdictCoverage, error) {
	out := AttachmentVerdictCoverage{StampedVersions: map[int]int{}}

	// Per tier: messages that carry attachments, and how many attachments.
	// Bucketed on the MESSAGE's received_at, because the tier is about how old
	// the mail is, not how old the verdict is.
	rows, err := tx.tx.Query(ctx, `
		SELECT `+tierCaseSQL("m.received_at")+` AS tier,
		       count(DISTINCT m.id) AS messages,
		       count(a.id)          AS attachments
		  FROM app.messages m
		  JOIN app.attachments a ON a.message_id = m.id AND a.user_id = m.user_id
		 WHERE m.user_id = $1
		 GROUP BY 1`, tx.userID.String())
	if err != nil {
		return out, fmt.Errorf("store: verdict coverage tiers: %w", err)
	}
	seen := map[string]TierCount{}
	for rows.Next() {
		var t TierCount
		if err := rows.Scan(&t.Tier, &t.Messages, &t.Attachments); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: scan tier row: %w", err)
		}
		seen[t.Tier] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: tier rows: %w", err)
	}
	// ⚠ EVERY TIER IS EMITTED, INCLUDING THE EMPTY ONES. A GROUP BY returns no
	// row for a band with no mail in it, and a tier that vanishes from the
	// report reads as "not applicable" rather than "zero" — which is exactly
	// how the `> 6 months` band would have disappeared on the instance that
	// motivated this, taking its own unvalidated-ness with it.
	for _, def := range RescanTiers {
		if t, ok := seen[def.Name]; ok {
			out.Tiers = append(out.Tiers, t)
		} else {
			out.Tiers = append(out.Tiers, TierCount{Tier: def.Name})
		}
	}

	// Stamped versions, never-scanned, and the oldest verdict still standing.
	vrows, err := tx.tx.Query(ctx, `
		SELECT scanned_db_version, count(*), min(scanned_at)
		  FROM app.attachments
		 WHERE user_id = $1
		 GROUP BY scanned_db_version`, tx.userID.String())
	if err != nil {
		return out, fmt.Errorf("store: verdict coverage versions: %w", err)
	}
	defer vrows.Close()
	for vrows.Next() {
		var ver *int
		var n int
		var oldest *time.Time
		if err := vrows.Scan(&ver, &n, &oldest); err != nil {
			return out, fmt.Errorf("store: scan version row: %w", err)
		}
		if ver == nil {
			out.NeverScanned = n
			continue
		}
		out.StampedVersions[*ver] = n
		if oldest != nil && (out.OldestVerdict == nil || oldest.Before(*out.OldestVerdict)) {
			t := oldest.UTC()
			out.OldestVerdict = &t
		}
	}
	return out, vrows.Err()
}

// RescanTarget is one message whose attachments are due for a re-scan.
//
// It is a MESSAGE, not an attachment, and that is the cost model rather than a
// convenience: ADR-027 §5c notes that "cost is not the clamd call" — attachments
// are not blobs (§3), so re-scanning any part of a message means loading the
// message blob, decrypting it with the user's DEK and re-walking its MIME. Doing
// that once per message and scanning every due part inside it is the difference
// between one decrypt and five.
type RescanTarget struct {
	MessageID   ulid.ULID
	ContentHash []byte
}

// dueClauseSQL builds the "this attachment's tier says it is due" predicate from
// RescanTiers, so the job and the census cannot disagree about what due means.
//
// ⚠ THE BANDS IN RescanTiers OVERLAP AS WRITTEN and must not overlap here. "≤ 7
// days" and "≤ 1 month" both describe a three-day-old message; tierCaseSQL
// resolves that by ordering its WHENs, but a WHERE clause has no first-match
// rule. Each band therefore gets an explicit lower bound taken from the previous
// tier's MaxAge, or the two would double-count and a message could be "due" on
// the strictest tier's schedule forever.
func dueClauseSQL(ageColumn, scannedColumn string) string {
	var parts []string
	var prev time.Duration
	for _, t := range RescanTiers {
		if !t.Rescan || t.MaxAge == 0 {
			prev = t.MaxAge
			continue
		}
		var b strings.Builder
		b.WriteString("(")
		fmt.Fprintf(&b, "%s > now() - interval '%d seconds'", ageColumn, int64(t.MaxAge/time.Second))
		if prev > 0 {
			fmt.Fprintf(&b, " AND %s <= now() - interval '%d seconds'", ageColumn, int64(prev/time.Second))
		}
		// ⚠ MinInterval == 0 means "on every signature-database change", which
		// has no time component at all: the version predicate alone governs, and
		// adding a time gate here would make the strictest tier the slowest.
		if t.MinInterval > 0 {
			fmt.Fprintf(&b, " AND (%s IS NULL OR %s < now() - interval '%d seconds')",
				scannedColumn, scannedColumn, int64(t.MinInterval/time.Second))
		}
		b.WriteString(")")
		parts = append(parts, b.String())
		prev = t.MaxAge
	}
	if len(parts) == 0 {
		return "false" // no tier re-scans; a syntactically valid "nothing is due"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// rescanStalenessSQL is the "this row's verdict needs remaking" half of the
// re-scan predicate, expecting $1 = user id and $2 = the current signature
// version.
//
// ⚠ IT IS A CONSTANT BECAUSE THE SELECT AND THE COUNT MUST NOT DRIFT. The job
// selects a chunk with one query and reports "remaining" with the other, and
// this project's own rule is that a chunked pass has to be able to say what it
// did NOT do — a loop that processes 50 and says "50 done" is indistinguishable
// from one that stalled. If the two predicates disagreed, "remaining" would be a
// number about a different population, which is worse than no number at all.
const rescanStalenessSQL = `(a.scanned_db_version IS NULL
		        OR a.scanned_db_version < $2)`

// notYetAttemptedSQL is the predicate's LEADING FACTOR: we have not already
// tried this row at this signature version. $2 is the current version.
//
// ⚠ IT IS WHAT MAKES THE QUEUE TERMINATE, AND WITHOUT IT THE JOB EATS ITSELF
// (#409). Every other clause here describes a row that NEEDS something; none of
// them could describe a row that has already been given everything a pass can
// give. Three populations could therefore never leave:
//
//   - a part that decoded to zero bytes. attachscan.PartHash returns nil for it
//     BY DESIGN — hashing nothing yields one identity every empty part in the
//     mailbox would share, and a single release against it would open the gate
//     on all of them — so part_hash stays NULL on every pass, and the digest arm
//     below deliberately bypasses the tier gate.
//   - a row whose message blob would not read (runChunk logs and continues).
//   - a row whose message the parser now finds no attachment parts in.
//
// ⚠ AND THE COST WAS NOT THE WASTED WORK. MessagesDueForRescan is
// `ORDER BY m.id LIMIT 50` over ingest-ordered ULIDs, so once fifty stuck
// messages precede a genuinely due one, THE SAME FIFTY RUN FOREVER and nothing
// else is ever re-scanned — including the `<= 7 days` tier, which is the entire
// value of re-scanning — while the job logs `messages=50 remaining=N` every
// thirty seconds, which reads as progress. Old mail is full of empty
// `ATT00001.txt` artefacts, so fifty of them on a real mailbox is ordinary.
//
// The termination argument is now structural rather than hopeful: a pass over a
// row either writes a verdict (which stamps this column, via writeVerdicts) or
// gives up (which stamps it, via RecordScanAttempt). Either way the row leaves.
// THE DUE SET STRICTLY SHRINKS WITHIN A SIGNATURE VERSION, and a version change
// is what legitimately re-opens it.
//
// ⚠ IT IS ALSO #415'S CIRCUIT BREAKER, and a better one than ingest's. A file
// that CRASHES clamd is now retried at most once per signature-database change
// instead of every thirty seconds, and the memory is a column rather than a
// process-lifetime map — so unlike ingest's breaker (ADR-027 §4b), a restart
// does not forget it.
const notYetAttemptedSQL = `(a.attempted_db_version IS NULL
		        OR a.attempted_db_version < $2)`

// missingDigestSQL is the "this row has no content identity and could have one"
// arm.
//
// ⚠ `size_bytes > 0` IS THE DEFINITION, NOT A WORKAROUND. A part that decoded to
// zero bytes has no content identity to compute — that is exactly what
// attachscan.HasNoBytes says and why PartHash returns nil for it — so it is
// permanently outside this arm rather than being asked for something impossible.
//
// ⚠ THE TWO SIDES OF THAT EQUIVALENCE ARE WRITTEN IN DIFFERENT PACKAGES, so it
// is pinned by a test rather than left as a reading of two files:
// mime.walkLeaf sets `OctetSize: len(data)` from the same read whose emptiness
// HasNoBytes tests, and attachproj.Rows copies OctetSize into size_bytes. If
// that ever stops being true, size_bytes stops meaning "HasNoBytes" and this
// clause silently readmits the fixed point — see attachproj's
// TestSizeBytesIsZeroExactlyWhenThePartHasNoBytes.
const missingDigestSQL = `(a.part_hash IS NULL AND a.size_bytes > 0)`

// rescanDueSQL is the whole "this message needs a pass" predicate: not already
// attempted at this signature version, AND either a stale verdict whose TIER has
// come due or — regardless of tier — an attachment with no content digest.
//
// ⚠ THE MISSING DIGEST DELIBERATELY BYPASSES THE TIER GATE, and the first
// version of this did not. Written as `staleness AND tierDue` with the digest
// folded into `staleness`, a row with no digest still had to wait for its own
// tier — up to a week for mail 1–6 months old. MEASURED ON THE DEPLOYED
// INSTANCE, which is the only reason it was caught: after migration 00044 the
// first pass filled in 41 of 268 digests and stopped, because only the
// ≤ 7-day tier was due.
//
//	tier          attachments   with digest after one pass
//	<= 7 days              41   41
//	<= 1 month            117    0
//	1-6 months            110    0
//
// That is a quarantined attachment on older mail being blocked and NOT
// RELEASABLE for up to a week, since a release is keyed on the digest. The
// commit that introduced the arm claimed it "costs one extra full sweep after
// the migration"; that was a claim about code that did the opposite, and
// standing it up is what said so.
//
// ⚠ A MISSING DIGEST IS NOT A FRESHNESS QUESTION. The tiers exist to bound how
// often a VERDICT is remade — an answer that ages. A digest is a fact about
// bytes that never changes and is simply absent, so rationing it by age is
// answering the wrong question.
//
// ⚠ AND THE COMMENT THAT STOOD HERE WAS FALSE, WHICH IS HOW #409 HAPPENED. It
// said: "It really is one sweep: every row a pass touches leaves with a digest,
// so this arm matches nothing afterwards." Not for a part that decoded to zero
// bytes — that row leaves every pass exactly as it arrived, and the arm matched
// it forever. The claim is now true because notYetAttemptedSQL makes it true,
// rather than because a part was assumed to always have bytes.
// unresolvedVerdictSQL is the third arm: an `unknown` verdict is re-tried
// whatever the tier says (#412).
//
// ⚠ THE STOP TIER'S ARGUMENT IS ABOUT `clean`, AND IT WAS APPLIED TO BOTH.
// §5c stops re-scanning mail over six months old because a `clean` verdict ages
// — the detection gap is short, so re-asking about old mail buys little, and the
// verdict's age is surfaced so the staleness is visible. Every word of that is
// about an answer that was actually produced.
//
// `unknown` is not an ageing answer. It means NOTHING LOOKED — an encrypted
// archive, a password-protected PDF, a transient scan failure, a parser limit —
// and §5b gives it the same enforcement as `flagged`, so it BLOCKS. Combining
// those two facts on mail over six months old produced a permanent quarantine on
// ordinary mail: a payslip that failed to scan during a first sync was never
// re-judged for the life of the mailbox, and the only recovery was a CLI command.
//
// ⚠ THAT IS THE INTERACTION THAT MADE THE OTHER ROUTES MATTER (#412). A transient
// failure on recent mail self-heals within a day; the same failure on a 2019
// message was forever. The first sync of a large old mailbox is precisely when
// clamd is under the heaviest load it will ever see, so it is also when transient
// failures are likeliest — and every message in it is scanned exactly once.
//
// ⚠ IT IS BOUNDED BY THE ATTEMPT GATE, NOT BY THE TIER. A part that can never
// resolve — a genuinely encrypted zip — is re-tried at most once per
// signature-database change rather than continuously, because notYetAttemptedSQL
// still applies. That is the price of not permanently withholding ordinary mail,
// and it is a price this arm makes visible rather than one it hides: the
// population is `attachment-report`'s `unknown` count.
//
// ⚠ AND IT CARRIES `size_bytes > 0` FOR THE SAME REASON missingDigestSQL DOES —
// which #409'S OWN TEST CAUGHT when this arm was first written without it. A
// zero-byte part is recorded `unknown` by attachscan, so the bare status
// predicate dragged every empty `ATT00001.txt` back into the queue once per
// signature update, forever. That is not the case this arm is for: "we could not
// check this" is a statement about a file we might yet check, and there is
// nothing in an empty part that a future signature database could resolve. The
// two clauses ask the same question — could a later pass change this answer —
// and they must agree.
const unresolvedVerdictSQL = `(a.scan_status = 'unknown' AND a.size_bytes > 0)`

func rescanDueSQL() string {
	return "(" + notYetAttemptedSQL + " AND ((" + rescanStalenessSQL + " AND " +
		dueClauseSQL("m.received_at", "a.scanned_at") + ") OR " + missingDigestSQL +
		" OR " + unresolvedVerdictSQL + "))"
}

// MessagesDueForRescan returns messages holding at least one attachment whose
// verdict is behind the given signature version and whose tier says it is due,
// oldest message first, bounded by limit.
//
// ⚠ THE VERSION PREDICATE IS NOT THE ONE ADR-027 §13 SPECIFIES, AND THAT IS THE
// WHOLE POINT. The ADR and #395 both give it as `scanned_db_version < current`,
// which matches NOTHING for a never-scanned row because `NULL < n` is NULL
// rather than true. Measured against the live database: it would have skipped
// all 255 unscanned attachments — the entire backlog this exists to clear —
// while the job logged a successful pass. The IS NULL arm is load-bearing.
//
// ⚠ A MISSING part_hash IS ALSO "DUE", AND WITHOUT IT THE QUARANTINE HAS A HOLE
// (#396). The digest is written by the same pass that writes a verdict, so every
// row that existed before migration 00044 has a verdict and no digest — and a
// release is keyed on the digest, so those rows CAN be blocked and CANNOT be
// released. The version predicate alone does not reach them: they are stamped at
// the current version, so they are not behind anything. That is a gate with no
// key. See rescanDueSQL, which is where that arm lives and why it deliberately
// sits OUTSIDE the tier gate rather than inside the staleness clause.
//
// ⚠ ORDERED BY THE MESSAGE ID, WHICH IS A ULID AND THEREFORE TIME-ORDERED, so
// chunks walk a stable sequence rather than re-shuffling between passes. It does
// NOT need a cursor: stamping a row removes it from the predicate, so the next
// chunk's LIMIT lands on genuinely new work. That is also what makes an
// interrupted pass self-heal — nothing has to remember where it stopped.
func (tx *UserTx) MessagesDueForRescan(ctx context.Context, currentVersion, limit int) ([]RescanTarget, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT m.id, m.content_hash
		  FROM app.messages m
		  JOIN app.attachments a ON a.message_id = m.id AND a.user_id = m.user_id
		 WHERE m.user_id = $1
		   AND `+rescanDueSQL()+`
		 ORDER BY m.id
		 LIMIT $3`, tx.userID.String(), currentVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("store: messages due for rescan: %w", err)
	}
	defer rows.Close()
	var out []RescanTarget
	for rows.Next() {
		var id string
		var hash []byte
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, fmt.Errorf("store: scan rescan target: %w", err)
		}
		u, perr := ulid.Parse(id)
		if perr != nil {
			return nil, fmt.Errorf("store: parse rescan message id %q: %w", id, perr)
		}
		out = append(out, RescanTarget{MessageID: u, ContentHash: hash})
	}
	return out, rows.Err()
}

// CountMessagesDueForRescan is the same population as a number, for the report
// and for the job's own "how much is left" log line.
//
// ⚠ It exists so a chunked pass can say what it did NOT do. A job that processes
// 50 messages and says "50 done" is indistinguishable from one that has finished
// and one that has 40,000 left — and the second is the state where a chunk loop
// that stalled looks exactly like a chunk loop that succeeded.
func (tx *UserTx) CountMessagesDueForRescan(ctx context.Context, currentVersion int) (int, error) {
	var n int
	err := tx.tx.QueryRow(ctx, `
		SELECT count(DISTINCT m.id)
		  FROM app.messages m
		  JOIN app.attachments a ON a.message_id = m.id AND a.user_id = m.user_id
		 WHERE m.user_id = $1
		   AND `+rescanDueSQL(),
		tx.userID.String(), currentVersion).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count messages due for rescan: %w", err)
	}
	return n, nil
}

// RescanBacklog is what the re-scan queue actually holds, BY THE JOB'S OWN
// PREDICATE rather than by a second computation over the same columns.
//
// ⚠ THE SECOND COMPUTATION IS WHY #409 WAS INVISIBLE. `attachment-report`
// printed "due for re-scan: %d behind + %d never scanned" derived from stamped
// versions alone — it never learned about the digest arm — so the report could
// say `due for re-scan: 0` while the job had work forever. v1.61.1 extracted
// rescanStalenessSQL as a shared constant precisely so the SELECT and the COUNT
// could not drift, and MISSED THE THIRD CONSUMER: the check that exists to make
// a stalled backlog legible could not see the stall. So the report now asks the
// predicate rather than re-deriving it.
type RescanBacklog struct {
	// DueMessages is CountMessagesDueForRescan — the number the job acts on.
	DueMessages int
	// NoDigest is attachments with no content identity at all.
	NoDigest int
	// NoDigestPossible is those of NoDigest that decoded to zero bytes, which
	// can never acquire one. ⚠ Reported apart from NoDigest because they are
	// opposite findings wearing the same number: "the backfill has not reached
	// these yet" versus "these are done, and done means empty".
	NoDigestPossible int
	// AttemptedAtCurrent is attachments a pass has already visited at the
	// current signature version. ⚠ It is the denominator that says the queue is
	// MOVING rather than merely small — with DueMessages at 0, this separates
	// "everything is caught up" from "nothing was ever looked at".
	AttemptedAtCurrent int
}

// RescanBacklogFor reads the backlog census for one user against the current
// signature version.
func (tx *UserTx) RescanBacklogFor(ctx context.Context, currentVersion int) (RescanBacklog, error) {
	var out RescanBacklog
	n, err := tx.CountMessagesDueForRescan(ctx, currentVersion)
	if err != nil {
		return out, err
	}
	out.DueMessages = n
	err = tx.tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE part_hash IS NULL),
		       count(*) FILTER (WHERE part_hash IS NULL AND size_bytes = 0),
		       count(*) FILTER (WHERE attempted_db_version = $2)
		  FROM app.attachments
		 WHERE user_id = $1`,
		tx.userID.String(), currentVersion).
		Scan(&out.NoDigest, &out.NoDigestPossible, &out.AttemptedAtCurrent)
	if err != nil {
		return out, fmt.Errorf("store: rescan backlog: %w", err)
	}
	return out, nil
}
