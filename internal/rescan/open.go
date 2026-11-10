// SPDX-License-Identifier: AGPL-3.0-only

package rescan

// Re-scan when opened (ADR-056 Decision A, operator decision Q80).
//
// ADR-027 §5c stops re-scanning mail older than six months, which is safe only
// because the verdict's age is surfaced. This is the other half: when somebody
// OPENS that mail (the reading pane fetches its body) or DOWNLOADS or PREVIEWS one
// of its attachments, the verdicts that are behind the signature database are
// remade. Mail nobody opens costs nothing.
//
// # ⚠ TWO PATHS WITH DIFFERENT RULES, AND WHY
//
// A download or preview scans SYNCHRONOUSLY, before the quarantine gate reads the
// verdict, because the person is about to receive the bytes and a fresh answer is
// worth a short wait. It is bounded hard (SyncBudget, a small number of slots,
// and a breaker) and it FAILS OPEN: on a timeout or a scanner error the request is
// served under the existing verdict exactly as today and a job is enqueued. See
// the ADR for why open, not closed.
//
// A body fetch only ENQUEUES. Nothing about showing text needs the answer, and the
// reading pane must not wait on clamd.
//
// # ⚠ THE JOB IS BOUNDED BECAUSE THE DISPATCHER IS SEQUENTIAL
//
// A re-scan that runs long delays `submit`, and outbound mail stops (the package
// comment on rescan.go). So an open-triggered job is one message, has a budget
// (JobBudget), and its creation is limited per message (Cooldown) and per user
// per minute (MaxQueuedPerMinute) in the INSERT itself.
//
// # ⚠ A TRANSIENT `unknown` NEVER REPLACES A VERDICT THAT WAS SERVED, PART BY PART
//
// clamd reports a per-file failure as `unknown`, not as an error. `unknown` blocks
// (ADR-027 §5b), so writing one over a verdict that did not block (clean, or no
// verdict yet) would lock the person out of a file they could open a second ago
// because the daemon had a bad second. That outcome is "inconclusive": nothing is
// written for that part and the existing verdict stands.
//
// # ⚠ ONE SLOW PART MUST NOT SHIELD THE REST, NOR TRIP THE BREAKER
//
// This path scans only the STALE parts, the requested part first and then the
// smallest first, and gives each part a slice of what is left of the overall
// budget. A part that runs out of its slice keeps its stored verdict, is counted
// as a timeout, and the next part is scanned. Without that, an attacker could put
// a decoy that clamd chews on in front of the real payload and the payload would
// never be refreshed. The process-wide breaker counts SCANNER failures (see
// recordFailure), not slow parts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/clamd"
	"mailroom/internal/mime"
	"mailroom/internal/store"
)

const (
	// SyncBudget bounds a download-time or preview-time re-scan.
	SyncBudget = 10 * time.Second
	// MaxSyncScans is how many synchronous re-scans may run at once. A request
	// that finds them all busy is queued instead of waiting.
	MaxSyncScans = 2
	// BreakerPause is how long the synchronous path is skipped once the breaker
	// has opened, so a dead scanner costs a few requests, not every download.
	BreakerPause = 60 * time.Second
	// BreakerThreshold failures within BreakerWindow open the breaker. A failure
	// is the scanner being unavailable: its VERSION call failing, or a Scan
	// returning an error that is not a deadline. A part that merely ran out of
	// its own time slice is NOT a failure: it is one slow file, and letting one
	// file open a process-wide breaker is how a single crafted attachment would
	// switch the protection off for every user.
	BreakerThreshold = 3
	BreakerWindow    = 60 * time.Second
	// GlobalEnqueueCap bounds open re-scan jobs created across ALL users in any
	// GlobalCapWindow, per process. The per-user bound lives in the INSERT; this
	// one stops many users, each inside their own bound, from flooding the
	// sequential dispatcher together.
	GlobalEnqueueCap = 6
	GlobalCapWindow  = time.Minute
	// JobBudget bounds one queued re-scan (one message).
	JobBudget = 20 * time.Second
	// MaxQueuedPerMinute caps jobs created per user in any 60 seconds.
	MaxQueuedPerMinute = 2
	// Cooldown: at most one job per message in this window.
	Cooldown = 10 * time.Minute
	// MaxMessagesPerOpen is how many messages one body fetch inspects.
	MaxMessagesPerOpen = 10

	versionTTL = 30 * time.Second
	// versionFailTTL: after a failed VERSION call, later calls fail fast for this
	// long without dialling, so a hung scanner adds versionTimeout to one open,
	// not to every open.
	versionFailTTL = 30 * time.Second
	versionTimeout = 2 * time.Second
)

// Open-rescan events, the label values of mailroom_attachment_open_rescan_total.
const (
	EventSync         = "sync"
	EventQueued       = "queued"
	EventJob          = "job"
	EventTimeout      = "timeout"
	EventUnavailable  = "unavailable"
	EventInconclusive = "inconclusive"
	EventBusy         = "busy"
	EventThrottled    = "throttled"
	EventDeduped      = "deduped"
	// EventGlobalCap: the per-process cap across all users refused an enqueue.
	EventGlobalCap = "global_cap"
)

// OpenEvents lists every event, so the metrics sink can create its series at zero.
func OpenEvents() []string {
	return []string{EventSync, EventQueued, EventJob, EventTimeout, EventUnavailable,
		EventInconclusive, EventBusy, EventThrottled, EventDeduped, EventGlobalCap}
}

// OpenRescan re-scans old attachments when they are opened. Safe for concurrent
// use. All methods are no-ops on a nil receiver, so an instance without a scanner
// needs no special case.
type OpenRescan struct {
	st      *store.Store
	blobs   BlobReader
	scanner Scanner
	log     *slog.Logger
	observe func(event string)
	now     func() time.Time

	slots chan struct{}
	// syncBudget is SyncBudget; a field so a test can shorten it.
	syncBudget time.Duration

	mu           sync.Mutex
	ver          clamd.Version
	verAt        time.Time
	verErr       error
	verErrAt     time.Time
	failures     []time.Time
	breakerUntil time.Time
	enqueues     []time.Time
}

// NewOpenRescan builds the open-time re-scanner. observe may be nil.
func NewOpenRescan(st *store.Store, blobs BlobReader, scanner Scanner, log *slog.Logger, observe func(event string)) *OpenRescan {
	if log == nil {
		log = slog.Default()
	}
	if observe == nil {
		observe = func(string) {}
	}
	return &OpenRescan{st: st, blobs: blobs, scanner: scanner, log: log, observe: observe,
		now: time.Now, slots: make(chan struct{}, MaxSyncScans), syncBudget: SyncBudget}
}

// currentVersion is the scanner's signature version, cached for versionTTL and
// bounded by versionTimeout, so a hung scanner cannot slow a body fetch by more
// than that. A FAILURE is cached too (versionFailTTL) and counted toward the
// breaker once, when it actually happened, so a hung scanner does not cost every
// open versionTimeout.
func (o *OpenRescan) currentVersion(ctx context.Context) (clamd.Version, error) {
	o.mu.Lock()
	now := o.now()
	if !o.verAt.IsZero() && now.Sub(o.verAt) < versionTTL {
		v := o.ver
		o.mu.Unlock()
		return v, nil
	}
	if o.verErr != nil && now.Sub(o.verErrAt) < versionFailTTL {
		err := o.verErr
		o.mu.Unlock()
		return clamd.Version{}, err
	}
	o.mu.Unlock()
	vctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	v, err := o.scanner.Version(vctx)
	if err != nil {
		if ctx.Err() == nil { // the scanner's fault, not a caller that went away
			o.mu.Lock()
			o.verErr, o.verErrAt = err, o.now()
			o.recordFailureLocked()
			o.mu.Unlock()
		}
		return clamd.Version{}, err
	}
	o.mu.Lock()
	o.ver, o.verAt = v, o.now()
	o.verErr = nil
	o.mu.Unlock()
	return v, nil
}

func (o *OpenRescan) breakerOpen() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.now().Before(o.breakerUntil)
}

// recordFailure notes that the SCANNER failed (unreachable, or an error that is
// not one part's deadline). BreakerThreshold of them within BreakerWindow open
// the breaker for BreakerPause. At most one is recorded per refresh, however many
// parts it had: one outage is one failure.
func (o *OpenRescan) recordFailure() {
	o.mu.Lock()
	o.recordFailureLocked()
	o.mu.Unlock()
}

func (o *OpenRescan) recordFailureLocked() {
	now := o.now()
	kept := o.failures[:0]
	for _, t := range o.failures {
		if now.Sub(t) < BreakerWindow {
			kept = append(kept, t)
		}
	}
	o.failures = append(kept, now)
	if len(o.failures) >= BreakerThreshold {
		o.breakerUntil = now.Add(BreakerPause)
		o.failures = nil
	}
}

// reserveEnqueue takes one slot of the per-process cap, or reports that the cap
// is reached. releaseEnqueue gives it back when the INSERT created nothing, so the
// cap counts jobs created, not attempts.
func (o *OpenRescan) reserveEnqueue() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	kept := o.enqueues[:0]
	for _, t := range o.enqueues {
		if now.Sub(t) < GlobalCapWindow {
			kept = append(kept, t)
		}
	}
	o.enqueues = kept
	if len(o.enqueues) >= GlobalEnqueueCap {
		return time.Time{}, false
	}
	o.enqueues = append(o.enqueues, now)
	return now, true
}

func (o *OpenRescan) releaseEnqueue(at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.enqueues) - 1; i >= 0; i-- {
		if o.enqueues[i].Equal(at) {
			o.enqueues = append(o.enqueues[:i], o.enqueues[i+1:]...)
			return
		}
	}
}

// WithSyncBudget overrides SyncBudget for this instance and returns it. For
// tests and for tuning; the default is the value the ADR argues for.
func (o *OpenRescan) WithSyncBudget(d time.Duration) *OpenRescan {
	o.syncBudget = d
	return o
}

// BeforeServe is called by the download and preview routes BEFORE the quarantine
// gate reads the stored verdict. It never fails the request: whatever happens, the
// caller proceeds to the gate, which serves or refuses by whatever verdict stands.
// partPath is the requested part ("" for none); it is scanned first.
func (o *OpenRescan) BeforeServe(ctx context.Context, userID, messageID ulid.ULID, partPath string) {
	if o == nil || messageID.Compare(ulid.ULID{}) == 0 {
		return
	}
	// 1. Cheap, scanner-free: is anything old at all? Newer mail, and mail with no
	// attachment bytes, ends here with no clamd call.
	counts, err := o.counts(ctx, userID, []ulid.ULID{messageID}, 0)
	if err != nil {
		o.log.Warn("open re-scan: could not read the message's attachments; serving under the existing verdict",
			"user_id", userID.String(), "message_id", messageID.String(), "err", err)
		return
	}
	if counts[messageID].Old == 0 {
		return
	}
	// 2. Is the verdict behind the current database?
	version, err := o.currentVersion(ctx)
	if err != nil {
		o.observe(EventUnavailable)
		o.log.Warn("open re-scan: the scanner did not report its version; serving under the existing verdict",
			"user_id", userID.String(), "err", err)
		o.enqueue(ctx, userID, messageID)
		return
	}
	counts, err = o.counts(ctx, userID, []ulid.ULID{messageID}, version.Database)
	if err != nil || counts[messageID].Stale == 0 {
		return
	}
	// 3. Synchronously, if the bounds allow; otherwise queue and serve as today.
	if o.breakerOpen() {
		o.observe(EventBusy)
		o.enqueue(ctx, userID, messageID)
		return
	}
	select {
	case o.slots <- struct{}{}:
		defer func() { <-o.slots }()
	default:
		o.observe(EventBusy)
		o.enqueue(ctx, userID, messageID)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, o.syncBudget)
	defer cancel()
	res, err := o.refresh(sctx, userID, messageID, version, partPath, time.Now().Add(o.syncBudget))
	switch {
	case err != nil && ctx.Err() != nil:
		// The caller went away; not the scanner's fault and not worth a job.
	case err != nil:
		if errors.Is(err, context.DeadlineExceeded) {
			o.observe(EventTimeout)
		} else {
			o.observe(EventUnavailable)
		}
		o.log.Warn("open re-scan did not finish; serving under the existing verdict and queueing a re-scan",
			"user_id", userID.String(), "message_id", messageID.String(), "err", err)
		o.enqueue(ctx, userID, messageID)
	default:
		// Nothing to do (another request refreshed it first) falls through all of
		// these. Anything left undone is queued.
		if res.report(o.observe, EventSync) {
			o.enqueue(ctx, userID, messageID)
		}
	}
}

// OnOpen is called after a body fetch for the messages it returned. It enqueues a
// job for each message that has a stale old attachment, within the bounds, and
// returns without waiting for any scan.
func (o *OpenRescan) OnOpen(ctx context.Context, userID ulid.ULID, messageIDs []ulid.ULID) {
	if o == nil || len(messageIDs) == 0 {
		return
	}
	if len(messageIDs) > MaxMessagesPerOpen {
		messageIDs = messageIDs[:MaxMessagesPerOpen]
	}
	counts, err := o.counts(ctx, userID, messageIDs, 0)
	if err != nil {
		o.log.Warn("open re-scan: could not read attachments for a body fetch", "user_id", userID.String(), "err", err)
		return
	}
	var old []ulid.ULID
	for _, id := range messageIDs {
		if counts[id].Old > 0 {
			old = append(old, id)
		}
	}
	if len(old) == 0 {
		return // the common case: no scanner round trip at all
	}
	version, err := o.currentVersion(ctx)
	if err != nil {
		o.observe(EventUnavailable)
		return
	}
	counts, err = o.counts(ctx, userID, old, version.Database)
	if err != nil {
		return
	}
	for _, id := range old {
		if counts[id].Stale > 0 {
			o.enqueue(ctx, userID, id)
		}
	}
}

func (o *OpenRescan) counts(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, version int) (map[ulid.ULID]store.OpenRescanCounts, error) {
	var out map[ulid.ULID]store.OpenRescanCounts
	err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.OpenRescanCountsFor(ctx, ids, version)
		return e
	})
	return out, err
}

// enqueue creates the bounded job and counts what happened.
//
// ⚠ TWO CAPS, AND THIS IS THE SECOND. The per-user bound is in the INSERT. The
// per-process one is here: GlobalEnqueueCap jobs across all users per
// GlobalCapWindow. It is per PROCESS, not per deployment: each instance of the
// service keeps its own window.
func (o *OpenRescan) enqueue(ctx context.Context, userID, messageID ulid.ULID) {
	at, ok := o.reserveEnqueue()
	if !ok {
		o.observe(EventGlobalCap)
		return
	}
	var res store.OpenRescanEnqueue
	err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		res, e = tx.EnqueueOpenRescan(ctx, messageID, MaxQueuedPerMinute, Cooldown)
		return e
	})
	if err != nil {
		o.releaseEnqueue(at)
		o.log.Warn("open re-scan: could not enqueue", "user_id", userID.String(), "err", err)
		return
	}
	if res != store.OpenRescanEnqueued {
		o.releaseEnqueue(at) // the cap counts jobs created, not attempts
	}
	switch res {
	case store.OpenRescanEnqueued:
		o.observe(EventQueued)
	case store.OpenRescanCooledDown:
		o.observe(EventDeduped)
	default:
		o.observe(EventThrottled)
	}
}

// refreshResult is what one refresh did, part by part.
type refreshResult struct {
	// Written is how many verdicts were written.
	Written int
	// Dropped counts transient unknowns that were not written because the part
	// had a prior verdict that did not block.
	Dropped int
	// Timeouts counts stale parts that ran out of their time slice, or of the
	// overall budget, and kept their stored verdict.
	Timeouts int
	// Unavailable is set when the scanner failed in a way that is not one part's
	// slowness. It has already been counted toward the breaker.
	Unavailable bool
}

// report counts what happened (okEvent when anything was written) and says
// whether something was left undone, which a caller on the synchronous path
// answers with a queued job.
func (r refreshResult) report(observe func(string), okEvent string) (incomplete bool) {
	if r.Written > 0 {
		observe(okEvent)
	}
	if r.Dropped > 0 {
		observe(EventInconclusive)
	}
	for i := 0; i < r.Timeouts; i++ {
		observe(EventTimeout)
	}
	if r.Unavailable {
		observe(EventUnavailable)
	}
	return r.Dropped > 0 || r.Timeouts > 0 || r.Unavailable
}

// refresh re-scans one message's STALE parts and writes the verdicts that
// finished, unless a verdict could not support replacing what is stored. An error
// is returned only for what is not the scanner's doing (the blob, the database,
// the caller going away); everything the scanner does is in the result.
func (o *OpenRescan) refresh(ctx context.Context, userID, messageID ulid.ULID, version clamd.Version, first string, deadline time.Time) (refreshResult, error) {
	var (
		target store.RescanTarget
		found  bool
		stale  []string
		prior  []store.PartScanState
	)
	if err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		target, found, e = tx.OpenRescanTarget(ctx, messageID, version.Database)
		if e != nil || !found {
			return e
		}
		if stale, e = tx.OpenRescanStaleParts(ctx, messageID, version.Database); e != nil {
			return e
		}
		byMsg, e := tx.AttachmentScanStates(ctx, []ulid.ULID{messageID})
		prior = byMsg[messageID]
		return e
	}); err != nil {
		return refreshResult{}, err
	}
	if !found || len(stale) == 0 {
		return refreshResult{}, nil
	}
	raw, err := o.blobs.Raw(ctx, userID, target.ContentHash)
	if err != nil {
		return refreshResult{}, fmt.Errorf("read message: %w", err)
	}
	parsed, _ := mime.ParseBytes(raw)
	set := make(map[string]bool, len(stale))
	for _, p := range stale {
		set[p] = true
	}
	scanned, err := o.scanStale(ctx, parsed, set, first, deadline)
	if err != nil {
		return refreshResult{}, err
	}
	res := refreshResult{Timeouts: scanned.timeouts, Unavailable: scanned.unavailable}
	if scanned.unavailable {
		o.recordFailure()
	}
	// ⚠ PER PART, NEVER PER MESSAGE. A transient failure on one attachment must
	// not stop a `flagged` verdict on another from being written: dropping the
	// whole write would leave an infected part served as clean. Only the
	// transient unknowns that would overwrite a verdict that did not block are
	// dropped; every other verdict, flagged ones above all, is written.
	verdicts, dropped := dropTransientRegressions(prior, scanned.verdicts)
	res.Dropped = dropped
	if len(verdicts) == 0 {
		return res, nil
	}
	if err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetScanVerdicts(ctx, messageID, verdicts, &store.ScanStamp{
			At: o.now(), DBVersion: version.Database, Engine: version.Engine,
		})
	}); err != nil {
		return refreshResult{}, err
	}
	res.Written = len(verdicts)
	return res, nil
}

type stalePartScan struct {
	verdicts    []store.ScanVerdict
	timeouts    int
	unavailable bool
}

// scanStale produces verdicts for the stale parts of one message, each part under
// its own time slice, so one slow part cannot consume the budget of the others.
//
// The order is the requested part first, then the smallest first: the part the
// person is about to receive is the one that matters most, and small parts are
// cheap, so more of them finish. A part's slice is the REMAINING overall budget
// divided by the parts still to scan (the requested one, when it is not the only
// one, takes half), so a decoy that eats its whole slice leaves the others a
// share of what remains.
//
// A part that runs out of its slice (or of the overall budget) is counted as a
// timeout and keeps its stored verdict; the loop continues. An error that is not
// a deadline is the scanner failing, not a part being slow: the loop stops (the
// next part would meet the same dead scanner) and the caller counts one breaker
// failure. If the caller's own context was cancelled, the error is returned.
func (o *OpenRescan) scanStale(ctx context.Context, parsed *mime.Message, stale map[string]bool, first string, deadline time.Time) (stalePartScan, error) {
	var out stalePartScan
	if parsed == nil {
		return out, nil
	}
	var parts []mime.AttachmentPart
	for _, ap := range parsed.AttachmentParts() {
		if stale[ap.Path] {
			parts = append(parts, ap)
		}
	}
	if len(parts) == 0 {
		return out, nil
	}
	if _, forced := attachscan.ForcedUnknown(parsed); forced {
		out.verdicts = attachscan.Unknown(parts)
		return out, nil
	}
	enc := attachscan.EncryptedPaths(parsed)
	var todo []mime.AttachmentPart
	for _, ap := range parts {
		switch {
		case enc[ap.Path], attachscan.HasNoBytes(ap):
			// Ciphertext and empty parts are unknown and never sent (ADR-027 §6, #405).
			out.verdicts = append(out.verdicts, attachscan.Unknown([]mime.AttachmentPart{ap})...)
		default:
			todo = append(todo, ap)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool {
		fi, fj := todo[i].Path == first, todo[j].Path == first
		if fi != fj {
			return fi
		}
		return len(todo[i].Node.Data) < len(todo[j].Node.Data)
	})
	for i, ap := range todo {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			out.timeouts += len(todo) - i
			break
		}
		share := remaining / time.Duration(len(todo)-i)
		if i == 0 && ap.Path == first && len(todo) > 1 {
			share = remaining / 2
		}
		sctx, cancel := context.WithTimeout(ctx, share)
		v, err := o.scanner.Scan(sctx, ap.Node.Data)
		slice := sctx.Err()
		cancel()
		if err != nil {
			if perr := ctx.Err(); perr != nil && !errors.Is(perr, context.DeadlineExceeded) {
				return out, perr // the caller went away
			}
			if slice != nil || errors.Is(err, context.DeadlineExceeded) {
				out.timeouts++
				continue
			}
			out.unavailable = true
			break
		}
		out.verdicts = append(out.verdicts, attachscan.Verdict(ap, v))
	}
	return out, nil
}

// dropTransientRegressions removes, from the verdicts to write, each `unknown`
// that is a transient scan failure AND would overwrite a verdict that did not
// block (clean, unscanned, or no row): the stored verdict, with its age, stands
// for that part. A scanner hiccup must never turn a file that was being served
// into a blocked one. It returns the rest and how many were dropped. Policy
// unknowns (encrypted, over the limit, forced) are real answers and are kept, as
// is every flagged verdict. A part already blocked (flagged or unknown) takes the
// new verdict, which cannot make it more blocked.
func dropTransientRegressions(prior []store.PartScanState, verdicts []store.ScanVerdict) ([]store.ScanVerdict, int) {
	was := make(map[string]store.PartScanState, len(prior))
	for _, p := range prior {
		was[p.PartPath] = p
	}
	out := make([]store.ScanVerdict, 0, len(verdicts))
	dropped := 0
	for _, v := range verdicts {
		p := was[v.PartPath]
		if v.Transient && v.Status == string(clamd.Unknown) && !attachscan.Blocked(p.Status, p.Released) {
			dropped++
			continue
		}
		out = append(out, v)
	}
	return out, dropped
}

// Handle runs one queued open re-scan: one message, bounded by JobBudget.
//
// It returns nil for every outcome except a malformed payload. A failure is
// counted and logged, and the per-message cooldown is what stops the next open
// retrying at once; returning an error would add the dispatcher's own retry and
// back-off on top, and a re-scan that cannot finish has no business holding a
// sequential dispatcher.
func (o *OpenRescan) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		return errors.New("open rescan: job has no user")
	}
	var p struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("open rescan: bad payload: %w", err)
	}
	messageID, err := ulid.Parse(p.MessageID)
	if err != nil {
		return fmt.Errorf("open rescan: bad message id: %w", err)
	}
	userID := *job.UserID
	jctx, cancel := context.WithTimeout(ctx, JobBudget)
	defer cancel()
	version, err := o.currentVersion(jctx)
	if err != nil {
		o.observe(EventUnavailable)
		o.log.Warn("open re-scan job skipped: the scanner did not answer; the existing verdict stands",
			"user_id", userID.String(), "err", err)
		return nil
	}
	res, err := o.refresh(jctx, userID, messageID, version, "", time.Now().Add(JobBudget))
	switch {
	case err == nil:
		res.report(o.observe, EventJob)
	case errors.Is(err, context.DeadlineExceeded):
		o.observe(EventTimeout)
		o.log.Warn("open re-scan job ran out of time; the existing verdict stands",
			"user_id", userID.String(), "message_id", messageID.String())
	default:
		o.observe(EventUnavailable)
		o.log.Warn("open re-scan job failed; the existing verdict stands",
			"user_id", userID.String(), "message_id", messageID.String(), "err", err)
	}
	return nil
}
