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
// # ⚠ A FRESH `unknown` NEVER REPLACES A STORED `clean`
//
// clamd reports a per-file failure as `unknown`, not as an error. `unknown` blocks
// (ADR-027 §5b), so writing one over a `clean` would lock the person out of
// ordinary old mail because the daemon had a bad second. That outcome is
// "inconclusive": nothing is written and the existing verdict stands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

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
	// BreakerPause is how long the synchronous path is skipped after it failed,
	// so a hung scanner costs one request SyncBudget, not every download.
	BreakerPause = 60 * time.Second
	// JobBudget bounds one queued re-scan (one message).
	JobBudget = 20 * time.Second
	// MaxQueuedPerMinute caps jobs created per user in any 60 seconds.
	MaxQueuedPerMinute = 2
	// Cooldown: at most one job per message in this window.
	Cooldown = 10 * time.Minute
	// MaxMessagesPerOpen is how many messages one body fetch inspects.
	MaxMessagesPerOpen = 10

	versionTTL     = 30 * time.Second
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
)

// OpenEvents lists every event, so the metrics sink can create its series at zero.
func OpenEvents() []string {
	return []string{EventSync, EventQueued, EventJob, EventTimeout, EventUnavailable,
		EventInconclusive, EventBusy, EventThrottled, EventDeduped}
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

	mu           sync.Mutex
	ver          clamd.Version
	verAt        time.Time
	breakerUntil time.Time
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
		now: time.Now, slots: make(chan struct{}, MaxSyncScans)}
}

// currentVersion is the scanner's signature version, cached for versionTTL and
// bounded by versionTimeout, so a hung scanner cannot slow a body fetch by more
// than that.
func (o *OpenRescan) currentVersion(ctx context.Context) (clamd.Version, error) {
	o.mu.Lock()
	if !o.verAt.IsZero() && o.now().Sub(o.verAt) < versionTTL {
		v := o.ver
		o.mu.Unlock()
		return v, nil
	}
	o.mu.Unlock()
	vctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	v, err := o.scanner.Version(vctx)
	if err != nil {
		return clamd.Version{}, err
	}
	o.mu.Lock()
	o.ver, o.verAt = v, o.now()
	o.mu.Unlock()
	return v, nil
}

func (o *OpenRescan) breakerOpen() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.now().Before(o.breakerUntil)
}

func (o *OpenRescan) tripBreaker() {
	o.mu.Lock()
	o.breakerUntil = o.now().Add(BreakerPause)
	o.mu.Unlock()
}

// BeforeServe is called by the download and preview routes BEFORE the quarantine
// gate reads the stored verdict. It never fails the request: whatever happens, the
// caller proceeds to the gate, which serves or refuses by whatever verdict stands.
func (o *OpenRescan) BeforeServe(ctx context.Context, userID, messageID ulid.ULID) {
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
	sctx, cancel := context.WithTimeout(ctx, SyncBudget)
	defer cancel()
	outcome, err := o.refresh(sctx, userID, messageID, version)
	switch {
	case err == nil && outcome == outcomeRefreshed:
		o.observe(EventSync)
	case err == nil && outcome == outcomeInconclusive:
		o.observe(EventInconclusive)
		o.enqueue(ctx, userID, messageID)
	case err == nil:
		// Nothing to do: another request refreshed it first.
	case ctx.Err() != nil:
		// The caller went away; not the scanner's fault and not worth a job.
	default:
		o.tripBreaker()
		if errors.Is(err, context.DeadlineExceeded) {
			o.observe(EventTimeout)
		} else {
			o.observe(EventUnavailable)
		}
		o.log.Warn("open re-scan did not finish; serving under the existing verdict and queueing a re-scan",
			"user_id", userID.String(), "message_id", messageID.String(), "err", err)
		o.enqueue(ctx, userID, messageID)
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
func (o *OpenRescan) enqueue(ctx context.Context, userID, messageID ulid.ULID) {
	var res store.OpenRescanEnqueue
	err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		res, e = tx.EnqueueOpenRescan(ctx, messageID, MaxQueuedPerMinute, Cooldown)
		return e
	})
	if err != nil {
		o.log.Warn("open re-scan: could not enqueue", "user_id", userID.String(), "err", err)
		return
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

type openOutcome int

const (
	outcomeNothing openOutcome = iota
	outcomeRefreshed
	outcomeInconclusive
)

// refresh re-scans one message's parts and writes the verdicts, unless the answer
// could not support replacing what is stored.
func (o *OpenRescan) refresh(ctx context.Context, userID, messageID ulid.ULID, version clamd.Version) (openOutcome, error) {
	var (
		target store.RescanTarget
		found  bool
		prior  []store.PartScanState
	)
	if err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		target, found, e = tx.OpenRescanTarget(ctx, messageID, version.Database)
		if e != nil || !found {
			return e
		}
		byMsg, e := tx.AttachmentScanStates(ctx, []ulid.ULID{messageID})
		prior = byMsg[messageID]
		return e
	}); err != nil {
		return outcomeNothing, err
	}
	if !found {
		return outcomeNothing, nil
	}
	raw, err := o.blobs.Raw(ctx, userID, target.ContentHash)
	if err != nil {
		return outcomeNothing, fmt.Errorf("read message: %w", err)
	}
	parsed, _ := mime.ParseBytes(raw)
	verdicts, err := ScanParts(ctx, o.scanner, parsed)
	if err != nil {
		return outcomeNothing, err
	}
	if len(verdicts) == 0 {
		return outcomeNothing, nil
	}
	if regressesCleanToUnknown(prior, verdicts) {
		return outcomeInconclusive, nil
	}
	if err := o.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetScanVerdicts(ctx, messageID, verdicts, &store.ScanStamp{
			At: o.now(), DBVersion: version.Database, Engine: version.Engine,
		})
	}); err != nil {
		return outcomeNothing, err
	}
	return outcomeRefreshed, nil
}

// regressesCleanToUnknown reports whether writing verdicts would turn a stored
// `clean` into `unknown`. See the package comment: that is a transient scanner
// failure, and `unknown` blocks.
func regressesCleanToUnknown(prior []store.PartScanState, verdicts []store.ScanVerdict) bool {
	was := make(map[string]string, len(prior))
	for _, p := range prior {
		was[p.PartPath] = p.Status
	}
	for _, v := range verdicts {
		if was[v.PartPath] == string(clamd.Clean) && v.Status == string(clamd.Unknown) {
			return true
		}
	}
	return false
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
	outcome, err := o.refresh(jctx, userID, messageID, version)
	switch {
	case err == nil && outcome == outcomeRefreshed:
		o.observe(EventJob)
	case err == nil && outcome == outcomeInconclusive:
		o.observe(EventInconclusive)
	case err == nil:
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
