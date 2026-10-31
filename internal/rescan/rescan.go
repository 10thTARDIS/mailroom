// SPDX-License-Identifier: AGPL-3.0-only

// Package rescan re-examines stored attachments as the signature database moves
// (#395, ADR-027 §5c, §13 increment 4).
//
// # Why a verdict expires
//
// `clean` is not a property of a file. It is a statement about what the
// signatures knew at the moment it was made, and the detection gap for genuinely
// new malware is measured in days. So verdicts are re-made on a cadence that is
// tiered by the age of the MAIL — recent mail is where nearly all the value is —
// and every verdict carries the version it was made against.
//
// # ⚠ CHUNKING IS NOT AN OPTIMISATION HERE
//
// The job dispatcher is SEQUENTIAL: one goroutine, one job at a time. A re-scan
// that ran the whole corpus in a single handler call would block `submit`, and
// OUTBOUND MAIL WOULD STOP for as long as it took. Chunks also have to stay well
// under the 15-minute lock lease, which has NO HEARTBEAT — a handler that
// outruns it has its job reclaimed underneath it while still working.
//
// So a pass does a bounded amount of work and reschedules itself. When work
// remains it comes back almost immediately; when it does not, it waits out the
// ordinary interval. That is the loop, and both halves of it matter: the first
// keeps the queue moving, the second keeps it from spinning.
//
// # ⚠ THIS IS BEST-EFFORT, UNLIKE INGEST, AND THE DIFFERENCE IS DELIBERATE
//
// Ingest is fail-closed: a scanner outage refuses the sync pass, because the
// alternative is admitting mail with a verdict nobody produced. Re-scan has no
// such stake. Every attachment it looks at ALREADY has a verdict and is already
// stored; failing to refresh one leaves the last answer standing, which is the
// honest state and not a new risk. Refusing something here would be theatre.
//
// What makes that safe is that the failure is VISIBLE rather than silent:
// `mailroom-admin attachment-report` prints the oldest verdict, the stamped
// versions against the current one, and the database's own age. A re-scan loop
// that has quietly stopped shows up there as verdicts that stop moving.
package rescan

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/clamd"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// JobKind is the durable-job kind this worker registers for.
const JobKind = "attachment_rescan"

// Tunables. All three are bounds on ONE handler call, and they exist because of
// the sequential dispatcher rather than because of the scanner's cost.
const (
	// defaultInterval is how long a pass waits when it found nothing to do.
	// Daily: the tiers themselves are upper bounds on frequency (§5c), so a
	// tighter loop would re-ask a question whose answer cannot have changed.
	defaultInterval = 24 * time.Hour

	// idleAfterChunk is how soon a pass returns when work REMAINS. Short enough
	// that a backlog drains promptly, long enough that the dispatcher gets to
	// run submit and every other job kind in between — which is the entire
	// reason this is chunked at all.
	idleAfterChunk = 30 * time.Second

	// maxMessagesPerChunk bounds the work by count.
	maxMessagesPerChunk = 50

	// chunkBudget bounds it by wall clock, which is the guard that actually
	// holds. ⚠ A count cannot bound the time: attachment sizes vary by three
	// orders of magnitude, and §4a measured 32 ms for a 50 KB file against
	// 366 ms for a 5.4 MB one — on DISTINCT files, because clamd caches by
	// content hash and timing the same file measures the cache. Two minutes is
	// comfortably inside the 15-minute lease with room for a slow blob store.
	chunkBudget = 2 * time.Minute
)

// BlobReader reads a message's raw bytes back out of the blob store.
//
// An interface rather than the concrete composer, so this package does not
// acquire the key hierarchy: the caller already holds one path from a user id to
// their mail-blob DEK, and a second would be the drift that silently reads mail
// with the wrong key.
type BlobReader interface {
	Raw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error)
}

// Scanner is the subset of the clamd client this worker needs.
type Scanner interface {
	Scan(ctx context.Context, data []byte) (clamd.Verdict, error)
	Version(ctx context.Context) (clamd.Version, error)
}

// VersionSink observes every scanner VERSION this worker reads (#641,
// mailroom_scanner_signature_{version,published_timestamp_seconds}). It gets
// the Version itself, not a derived age: an age is true only at the instant it
// is computed, and this read happens as seldom as once a day.
//
// Optional. Nil means nobody is listening — every CLI-side Worker (the drill,
// mailroom-admin's own direct clamd calls) has no Prometheus registry to feed
// and constructs with a nil sink, same as a nil logger falling back to
// slog.Default().
type VersionSink func(version clamd.Version)

// Worker re-scans one user's due attachments per fired job and reschedules.
// It mirrors ml.TrainWorker and purge.Worker: a per-user recurring timer, seeded
// at boot and self-rescheduling from inside the handler.
type Worker struct {
	st      *store.Store
	blobs   BlobReader
	scanner Scanner

	interval    time.Duration
	now         func() time.Time
	log         *slog.Logger
	versionSink VersionSink
}

// New builds a re-scan worker. A non-positive interval falls back to daily; a
// nil logger to slog.Default(); a nil sink means nothing observes the scanner
// VERSION reads this worker makes (see VersionSink).
func New(st *store.Store, blobs BlobReader, scanner Scanner, interval time.Duration, log *slog.Logger, sink VersionSink) *Worker {
	if interval <= 0 {
		interval = defaultInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{st: st, blobs: blobs, scanner: scanner, interval: interval, now: time.Now, log: log, versionSink: sink}
}

// Handle runs one chunk for one user and reschedules the next.
func (w *Worker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		return errors.New("rescan: job has no user")
	}
	userID := *job.UserID

	// ⚠ THE VERSION IS READ ONCE, BEFORE THE CHUNK, AND STAMPED AFTERWARDS —
	// §13's requirement, and it is about self-healing rather than accuracy. If
	// freshclam updates mid-chunk, every row this pass stamps carries the OLDER
	// number, so it stays behind the new current and the next pass picks it up.
	// Reading it per row instead would let a mid-pass update mark rows as
	// current against signatures that were never applied to them.
	version, err := w.scanner.Version(ctx)
	if err != nil {
		// Best-effort by design (see the package comment): a scanner that is
		// down leaves every existing verdict standing, which is the honest
		// state. Reschedule and try later rather than failing the job, so the
		// dispatcher's backoff does not compound with our own interval.
		w.log.Warn("attachment re-scan skipped: the scanner did not answer; existing verdicts stand and are ageing",
			"user_id", userID.String(), "err", err)
		return w.reschedule(ctx, userID, job.ID, w.interval)
	}
	if w.versionSink != nil {
		// Every successful read, not just the first per boot: this is a per-user
		// recurring job, so on a single-scanner deployment the gauges are simply
		// re-set to the same shared clamd's answer each time any user's pass
		// fires, and a database clamd reloads is picked up at the next one.
		w.versionSink(version)
	}
	if version.Stale(w.now()) {
		// ⚠ §5c's FAIL-OPEN HALF, CAUGHT HERE RATHER THAN IGNORED. Every tier
		// is gated on the version CHANGING, so a database that cannot change
		// suspends re-scanning entirely — while every scan still succeeds and
		// every verdict age still advances. Re-scanning against it is not
		// harmful, so the pass continues; what must not happen is it being
		// silent.
		age, known := version.Age(w.now())
		w.log.Warn("SIGNATURE DATABASE IS STALE: re-scan tiers gate on the version changing, so a database "+
			"that cannot change silently suspends re-scanning while every scan still reports success",
			"user_id", userID.String(), "db_version", version.Database,
			"db_age", age.Truncate(time.Minute), "age_known", known)
	}

	done, gaveUp, moved, remaining, scanErr := w.runChunk(ctx, userID, version)

	// ⚠ BOTH NUMBERS, ALWAYS. "50 messages re-scanned" is the same line whether
	// the backlog is now empty or still forty thousand deep, and a chunk loop
	// that has stalled looks exactly like one that has finished.
	//
	// ⚠ AND `gave_up` SEPARATELY FROM `messages` (#409/#415). They used to be
	// one number: a message the pass abandoned was simply not counted, so a
	// chunk that failed on all fifty logged `messages=0 remaining=N` — which is
	// also what a chunk that ran out of time logs. Those are different faults
	// with different fixes, and the operator cannot act on the difference until
	// the line says which one happened.
	w.log.Info("attachment re-scan chunk",
		"user_id", userID.String(), "messages", done, "gave_up", gaveUp, "remaining", remaining,
		"db_version", version.Database, "engine", version.Engine)

	next := w.nextFireIn(remaining, moved)
	if remaining > 0 && !moved {
		w.log.Warn("attachment re-scan made NO progress and work remains; backing off rather than "+
			"retrying immediately, because a pass that changes nothing will not change anything sooner",
			"user_id", userID.String(), "remaining", remaining, "gave_up", gaveUp, "backoff", next)
	}
	if rerr := w.reschedule(ctx, userID, job.ID, next); rerr != nil {
		w.log.Error("attachment re-scan reschedule failed", "user_id", userID.String(), "err", rerr)
		if scanErr == nil {
			return rerr
		}
	}
	return scanErr
}

// nextFireIn decides how soon this worker comes back.
//
// ⚠ `moved` IS THE WHOLE POINT, AND WITHOUT IT THIS IS A LOOP (#415, #409).
// idleAfterChunk exists to keep a BACKLOG DRAINING: work remains, so come back
// promptly. But "work remains" was the only condition, and a pass that changed
// nothing satisfies it forever — so a message the pass could not process
// re-scheduled at thirty seconds, was picked first again, failed again, and the
// job re-read, re-decrypted and re-parsed the same messages every thirty seconds
// indefinitely, starving `submit` on the sequential dispatcher they share. That
// is how one clamd-crashing file became a remote hold on outbound mail.
//
// ⚠ IT IS THE SECOND GUARD, NOT THE FIRST. The predicate itself now terminates
// (store.notYetAttemptedSQL), so a pass that moved nothing means even the
// give-up stamp failed — the database is refusing writes, or shutdown is under
// way. Backing off is right for both, and it is the difference between a system
// that degrades and one that spins.
func (w *Worker) nextFireIn(remaining int, moved bool) time.Duration {
	if remaining > 0 && moved {
		return idleAfterChunk
	}
	return w.interval
}

// runChunk re-scans up to one chunk's worth of messages and reports what it did:
// how many were re-judged, how many it gave up on, whether ANY row's state
// actually moved, and how many remain.
//
// ⚠ `moved` IS NOT `done > 0`, AND THE DIFFERENCE IS THE WHOLE POINT. A chunk
// that gave up on all fifty messages has still made progress if each give-up was
// recorded — those rows have left the queue and the next chunk sees new work.
// What must not be mistaken for progress is a pass in which nothing was written
// at all; that is the state where coming back in thirty seconds is a loop.
func (w *Worker) runChunk(ctx context.Context, userID ulid.ULID, version clamd.Version) (done, gaveUp int, moved bool, remaining int, err error) {
	var targets []store.RescanTarget
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		targets, e = tx.MessagesDueForRescan(ctx, version.Database, maxMessagesPerChunk)
		return e
	}); err != nil {
		return 0, 0, false, 0, err
	}

	deadline := w.now().Add(chunkBudget)
	for _, t := range targets {
		// ⚠ THE WALL-CLOCK GUARD, CHECKED BETWEEN MESSAGES rather than only at
		// the top. The count bound cannot hold the time bound on its own, and
		// overrunning the 15-minute lease means this job is reclaimed while it
		// is still running.
		if w.now().After(deadline) {
			w.log.Info("attachment re-scan chunk stopped on its time budget; the rest is next pass",
				"user_id", userID.String(), "messages", done, "budget", chunkBudget)
			break
		}
		if ctx.Err() != nil {
			break
		}
		stamped, err := w.rescanMessage(ctx, userID, t, version)
		if stamped {
			moved = true
		}
		if err != nil {
			// One unreadable message must not stop the chunk: it would stall
			// the whole loop on a single bad row, and the row keeps its old
			// verdict either way.
			//
			// ⚠ BUT `continue` ALONE IS WHAT MADE IT A LOOP (#415). Skipping
			// without recording anything left the row exactly as due as it was,
			// so the next pass — thirty seconds later — picked the same message
			// first, forever. rescanMessage now stamps the attempt before
			// returning, and `stamped` says whether that actually landed.
			gaveUp++
			w.log.Warn("attachment re-scan gave up on one message; its existing verdict stands and the "+
				"attempt is recorded, so it will not be re-offered until the signature database moves",
				"user_id", userID.String(), "message_id", t.MessageID.String(),
				"attempt_recorded", stamped, "err", err)
			continue
		}
		done++
	}

	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		remaining, e = tx.CountMessagesDueForRescan(ctx, version.Database)
		return e
	}); err != nil {
		return done, gaveUp, moved, 0, err
	}
	return done, gaveUp, moved, remaining, nil
}

// rescanMessage re-reads one message's bytes and re-judges its parts. It reports
// whether the row's attempt was RECORDED — by a verdict write or by a give-up
// stamp — alongside the failure, if any, that stopped it re-judging.
//
// ⚠ EVERY EXIT PATH RECORDS THE ATTEMPT, AND THAT IS THE FIX (#409, #415). A
// pass that produces no verdict used to leave the row byte-for-byte as it found
// it, which meant "still due" — so the same message was picked first on the next
// pass thirty seconds later, forever. Three ways to produce no verdict, all of
// them ordinary on a real mailbox: the blob would not read, the parser now finds
// no attachment parts, or the scanner failed on the bytes. See
// store.notYetAttemptedSQL for why recording the attempt is what makes the queue
// terminate.
//
// ⚠ THE ATTEMPT IS NOT A VERDICT AND MUST NEVER LOOK LIKE ONE. It touches
// attempted_*, never scanned_* or scan_status: the standing verdict keeps its
// real age (ADR-027 §1a), and nothing here downgrades a `clean` part to
// `unknown` because clamd happened to be restarting. Re-scan is best-effort;
// blocking an attachment on a transient failure would be exactly the harm the
// package comment refuses.
//
// ⚠ THIS IS WHERE THE COST IS, AND IT IS NOT THE SCANNER. §5c: attachments are
// not blobs, so every re-scan loads the message blob, decrypts it with the
// user's DEK and re-walks its MIME before a single byte reaches clamd. The ~9 s
// whole-corpus figure in §4a counts clamd time only.
func (w *Worker) rescanMessage(ctx context.Context, userID ulid.ULID, t store.RescanTarget, version clamd.Version) (stamped bool, err error) {
	raw, blobErr := w.blobs.Raw(ctx, userID, t.ContentHash)
	if blobErr != nil {
		return w.recordAttempt(ctx, userID, t.MessageID, version), blobErr
	}
	parsed, _ := mime.ParseBytes(raw) // best-effort, exactly as ingest treats it
	verdicts, scanErr := ScanParts(ctx, w.scanner, parsed)
	if scanErr != nil {
		return w.recordAttempt(ctx, userID, t.MessageID, version), scanErr
	}
	if len(verdicts) == 0 {
		// Not a failure: the message genuinely has no attachment parts to judge
		// now. It still has projected rows — that disagreement is what put it
		// in the queue — so the attempt has to be recorded or it stays there.
		return w.recordAttempt(ctx, userID, t.MessageID, version), nil
	}
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetScanVerdicts(ctx, t.MessageID, verdicts, &store.ScanStamp{
			At: w.now(), DBVersion: version.Database, Engine: version.Engine,
		})
	}); err != nil {
		// The write failed, so nothing was stamped either — the verdict and the
		// attempt move in one statement.
		return false, err
	}
	return true, nil
}

// recordAttempt stamps "a pass visited this row at this signature version" and
// reports whether that landed. A failure here is logged rather than returned:
// the caller is already reporting the failure that brought it here, and a second
// error would replace the interesting one with a database complaint.
func (w *Worker) recordAttempt(ctx context.Context, userID, messageID ulid.ULID, version clamd.Version) bool {
	if err := w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.RecordScanAttempt(ctx, messageID, version.Database, w.now())
	}); err != nil {
		// ⚠ THE ONLY WAY BACK INTO THE 30-SECOND LOOP. Said out loud, because
		// the pass would otherwise look identical to a healthy one and the
		// message would be re-offered immediately and forever. Handle's
		// no-progress back-off is what catches this.
		w.log.Error("attachment re-scan could not record a scan attempt; this message will be offered "+
			"again on the next pass, which is the loop #409 describes",
			"user_id", userID.String(), "message_id", messageID.String(), "err", err)
		return false
	}
	return true
}

// reschedule enqueues the next pass, excluding this still-running job.
//
// ⚠ A DECLINED RESCHEDULE MEANS THE CHAIN JUST STOPPED — the same trap
// internal/purge and internal/ml carry the twin comment for (#351 B3, #357). It
// is logged loudly because nothing else would notice: the queue simply goes
// quiet, and quiet is what success looks like.
func (w *Worker) reschedule(ctx context.Context, userID, currentJobID ulid.ULID, in time.Duration) error {
	return w.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		scheduled, err := tx.EnsureRecurringUserJob(ctx, JobKind, w.now().Add(in), currentJobID)
		if err == nil && !scheduled {
			w.log.Warn("attachment re-scan did not reschedule: another job of this kind is already "+
				"outstanding; if none is actually running, the re-scan chain has stopped",
				"user_id", userID.String(), "kind", JobKind)
		}
		return err
	})
}

// EnsureScheduled seeds the first re-scan pass for a user at boot when none is
// outstanding. Idempotent across restarts; the zero job id excludes nothing.
func EnsureScheduled(ctx context.Context, st *store.Store, userID ulid.ULID, fireAt time.Time) (bool, error) {
	var scheduled bool
	err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		scheduled, e = tx.EnsureRecurringUserJob(ctx, JobKind, fireAt, ulid.ULID{})
		return e
	})
	return scheduled, err
}

// ScanParts produces a verdict for every attachment part of one message, under
// RE-SCAN's error policy.
//
// ⚠ THE RULES ARE SHARED WITH INGEST AND THE POLICY IS NOT, which is the whole
// reason internal/attachscan exists. Every judgement below — what forces
// `unknown` without a scan, what a zero-byte part means, how a clamd verdict
// becomes a row — comes from that package, so the same bytes cannot get one
// answer at ingest and a different one here. What differs is what happens when
// the scanner fails:
//
//	ingest  refuses the pass, because admitting mail with no verdict is the
//	        failure it exists to prevent, and runs a circuit breaker so one file
//	        cannot halt the account.
//	rescan  gives up on the message and returns the error, because the row it
//	        was refreshing ALREADY has a verdict which simply stands. There is
//	        nothing to refuse — but there WAS something to wedge, which is the
//	        correction #415 made: the caller now records the give-up as an
//	        attempt, so the same bytes are not re-offered to the daemon thirty
//	        seconds later. Giving up without recording it is what made a
//	        clamd-crashing file a remote kill switch on the whole account.
func ScanParts(ctx context.Context, sc Scanner, parsed *mime.Message) ([]store.ScanVerdict, error) {
	if parsed == nil {
		return nil, nil
	}
	parts := parsed.AttachmentParts()
	if len(parts) == 0 {
		return nil, nil
	}
	if _, forced := attachscan.ForcedUnknown(parsed); forced {
		return attachscan.Unknown(parts), nil
	}
	enc := attachscan.EncryptedPaths(parsed)
	if attachscan.AllEncrypted(parts, enc) {
		return attachscan.Unknown(parts), nil
	}
	out := make([]store.ScanVerdict, 0, len(parts))
	for _, ap := range parts {
		if enc[ap.Path] {
			// Ciphertext is unknown and never sent (ADR-027 section 6, #405).
			out = append(out, attachscan.Unknown([]mime.AttachmentPart{ap})...)
			continue
		}
		if attachscan.HasNoBytes(ap) {
			out = append(out, attachscan.Unknown([]mime.AttachmentPart{ap})...)
			continue
		}
		v, err := sc.Scan(ctx, ap.Node.Data)
		if err != nil {
			// ⚠ ABANDON THE MESSAGE, DO NOT PART-WRITE IT. Stamping the parts
			// that happened to succeed would advance their version while the
			// rest stayed behind, so the message would look partly fresh and
			// the next pass would re-do it anyway. Leaving all of it alone
			// keeps the message a single unit of work, which is what makes an
			// interrupted chunk self-heal.
			return nil, err
		}
		out = append(out, attachscan.Verdict(ap, v))
	}
	return out, nil
}
