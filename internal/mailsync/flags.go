// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"fmt"
	"sort"
	"strings"
	gosync "sync"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/sync"
)

// This file holds two things that are both about reading FLAGS correctly:
//
//   - the CONDSTORE-LESS FLAG SWEEP (#52 increment 6, ADR-046 §12). Remote
//     flag changes on messages we already hold are otherwise reconciled ONLY
//     through CONDSTORE (syncMailbox's CHANGEDSINCE pass). A server without it
//     — Outlook/Microsoft 365's IMAP advertises none — never had an existing
//     message's flags re-read at all, so a message read or starred on a phone
//     stayed unread in Mailroom forever.
//   - the engine's correctness COUNTERS (Observer): FETCH responses that came
//     back without the FLAGS they were asked for (#909), flag readings
//     applied by a FETCH command that then failed (#909's report), and new
//     messages the recovery paths ingested (#908's report).

// Observer receives the engine's correctness counters. cmd/mailroom turns
// them into Prometheus counters; the default discards them. Every method must
// return promptly and must not fail: it runs inside a sync pass.
type Observer interface {
	// RecoveredArrival counts NEW messages a recovery path ingested — path
	// "gap" (the #93 gap backfill) or "flag_fallback" (the CONDSTORE pass's
	// body fallback) — by outcome: "notify", "muted" or "blocked". Before
	// #908 none of these reached a push or the blocked count.
	RecoveredArrival(path, outcome string, n int)
	// FlagsAbsent counts messages whose FETCH response lacked the FLAGS item
	// it was asked for (#909) — path "ingest", "refresh" or "sweep". None of
	// them is read as "no flags" any more.
	FlagsAbsent(path string, n int)
	// FlagRefreshAborted counts flag readings applied by a FETCH command that
	// then failed (#909's report): readings that before the fix could have
	// been an empty FLAGS cut short by the failure.
	FlagRefreshAborted(path string, n int)
	// FlagSweep counts the flag sweep's work: messages read, and of those the
	// ones whose server flags differed from what Mailroom last recorded and
	// were therefore applied.
	FlagSweep(read, applied int)
}

type nopObserver struct{}

func (nopObserver) RecoveredArrival(string, string, int) {}
func (nopObserver) FlagsAbsent(string, int)              {}
func (nopObserver) FlagRefreshAborted(string, int)       {}
func (nopObserver) FlagSweep(int, int)                   {}

// WithObserver installs the counters' sink (nil keeps the default, which
// discards them).
func WithObserver(o Observer) Option {
	return func(e *Engine) {
		if o != nil {
			e.obs = o
		}
	}
}

// recordRecovered reports a recovery path's new arrivals by outcome.
func (e *Engine) recordRecovered(path string, b ingestBatch) {
	if n := len(b.notify); n > 0 {
		e.obs.RecoveredArrival(path, "notify", n)
	}
	if b.muted > 0 {
		e.obs.RecoveredArrival(path, "muted", b.muted)
	}
	if b.blocked > 0 {
		e.obs.RecoveredArrival(path, "blocked", b.blocked)
	}
}

// FlagSweepConfig configures the CONDSTORE-less flag sweep.
//
// The sweep runs for a mailbox only when the server advertises NEITHER
// CONDSTORE NOR QRESYNC: with CONDSTORE, the MODSEQ pass already reports every
// flag change, and a sweep would be pure cost. It never runs on the Gmail
// model, which has its own label-and-flag sweep (ADR-047 §4).
type FlagSweepConfig struct {
	// Interval is how often each mailbox's newest Window UIDs are re-read.
	// Zero disables the sweep entirely — the negative control: with it off,
	// a remote \Seen change on a server without CONDSTORE is never observed.
	Interval time.Duration
	// Window is how many UID NUMBERS below the mailbox's cursor the recent
	// re-read covers ([cursor-Window+1, cursor]). UIDs are sparse, so it can
	// cover fewer messages, never more: the cost bound is what matters.
	Window int
	// FullInterval is how often a whole-mailbox re-read starts. Zero disables
	// only the full pass. It walks the mailbox FullChunk UID numbers per pass.
	FullInterval time.Duration
	// FullChunk bounds one pass's share of the full re-read, per mailbox.
	FullChunk int
}

// enabled reports whether the sweep runs at all.
func (c FlagSweepConfig) enabled() bool { return c.Interval > 0 && c.Window > 0 }

// WithFlagSweep configures the CONDSTORE-less flag sweep. The default (the
// zero config) is OFF, so a caller that does not ask for it — the admin CLI,
// every existing test — behaves exactly as before; cmd/mailroom turns it on
// with the MAILROOM_FLAG_SWEEP_* settings.
func WithFlagSweep(c FlagSweepConfig) Option {
	return func(e *Engine) {
		if c.FullChunk <= 0 {
			c.FullChunk = 2000
		}
		e.sweepCfg = c
	}
}

// sweepState is one mailbox's sweep bookkeeping.
//
// ⚠ IN MEMORY, DELIBERATELY. Losing it (a restart) can only make a sweep run
// SOONER — every mailbox reads as never swept — never later and never
// skipped, and that costs one window per mailbox plus a restarted full pass
// (~50 bytes a message). Persisting it would buy nothing a restart could lose.
type sweepState struct {
	recentAt time.Time // last recent-window re-read
	fullNext int64     // next UID of the full pass in progress; 0 = none
	fullDone time.Time // last full pass finished
	validity int64     // the UIDVALIDITY the state belongs to
}

// sweepStates is the engine's per-mailbox sweep bookkeeping.
type sweepStates struct {
	mu gosync.Mutex
	m  map[ulid.ULID]*sweepState
}

func (s *sweepStates) get(id ulid.ULID, validity int64) *sweepState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[ulid.ULID]*sweepState{}
	}
	st := s.m[id]
	if st == nil || st.validity != validity {
		// A new UIDVALIDITY is a new UID space: nothing carried over means
		// anything about it.
		st = &sweepState{validity: validity}
		s.m[id] = st
	}
	return st
}

// sweepFlags runs whatever share of the flag sweep is due for one mailbox,
// whose cursor is lastSeen. It reads FLAGS only, and applies a reading only
// when it DIFFERS from the remote flags Mailroom last recorded for that copy
// (the store's LiveRemoteFlags), through the same RefreshFlags the CONDSTORE
// pass uses.
//
// ⚠ THE COMPARISON IS WHAT MAKES THE SWEEP AS SAFE AS CONDSTORE, not an
// optimisation. RefreshFlags recomputes a message's flags from its copies'
// remote flags, so applying an UNCHANGED server reading would overwrite a
// local edit whose writeback has not reached the server yet (a message marked
// read here a moment ago would turn unread again). The CONDSTORE pass never
// does that, because it only ever sees messages whose MODSEQ moved; comparing
// first gives the sweep the same property: only a real server-side change is
// applied. It also means an unchanged mailbox costs one read query, not one
// write per message.
//
// Readings are applied only after the FETCH command completed (FetchByUID
// returned nil), and a message whose response lacks FLAGS is no reading
// (#909).
func (e *Engine) sweepFlags(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, lastSeen int64, summary *Summary) error {
	cfg := e.sweepCfg
	now := e.now()
	st := e.sweeps.get(mailboxID, int64(status.UIDValidity))

	// Does this mailbox keep user keywords (review M4 of #910)? Only a
	// PERMANENTFLAGS `\*` says so: from this SELECT (an EXAMINE usually lists
	// none) or the recorded probe of an earlier read-write SELECT. Unknown is
	// treated as NO, so keywords Mailroom holds that the server cannot store
	// are never wiped by the server's keyword-less reading.
	keywords := status.PermanentFlagsAllowNew
	if !keywords {
		ok, err := e.store.MailboxAcceptsKeywords(ctx, req.UserID, mailboxID)
		if err != nil {
			return fmt.Errorf("mailsync: flag sweep %q: keyword probe: %w", name, err)
		}
		keywords = ok
	}

	if st.recentAt.IsZero() || now.Sub(st.recentAt) >= cfg.Interval {
		lo := lastSeen - int64(cfg.Window) + 1
		if lo < 1 {
			lo = 1
		}
		if err := e.sweepRange(ctx, req, sess, mailboxID, status, name, lo, lastSeen, keywords, summary); err != nil {
			return err
		}
		st.recentAt = now
		summary.FlagSweeps++
	}

	if cfg.FullInterval <= 0 {
		return nil
	}
	if st.fullNext == 0 && (st.fullDone.IsZero() || now.Sub(st.fullDone) >= cfg.FullInterval) {
		st.fullNext = 1
	}
	if st.fullNext == 0 {
		return nil
	}
	// The full pass covers only what the recent window does not: the window
	// re-reads the top Window UIDs every Interval anyway.
	top := lastSeen - int64(cfg.Window)
	hi := st.fullNext + int64(cfg.FullChunk) - 1
	if hi > top {
		hi = top
	}
	if st.fullNext <= hi {
		if err := e.sweepRange(ctx, req, sess, mailboxID, status, name, st.fullNext, hi, keywords, summary); err != nil {
			return err
		}
	}
	st.fullNext = hi + 1
	if st.fullNext > top {
		st.fullNext, st.fullDone = 0, now
		summary.FlagFullSweeps++
	}
	return nil
}

// sweepRange re-reads FLAGS for UIDs lo..hi and applies what changed.
//
// keywords false (the mailbox does not keep user keywords, or it is not known
// to): only SYSTEM flags (\Seen, \Flagged, …) are compared and applied, and
// the copy's recorded non-system keywords are carried through unchanged —
// the server's silence about a keyword it cannot store is not a removal.
//
// A copy whose message has a flag/keyword writeback still pending or inflight
// is skipped (review L6 of #910): the server has not been told about the local
// edit yet, so its reading is older than Mailroom's. The sweep after the
// writeback drains converges.
func (e *Engine) sweepRange(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, lo, hi int64, keywords bool, summary *Summary) error {
	type reading struct {
		uid   int64
		flags []string
	}
	var (
		readings []reading
		absent   int
	)
	err := sess.FetchByUID(ctx, sync.UIDSetRange(sync.UID(lo), sync.UID(hi)), sync.FetchItems{Flags: true},
		func(m sync.Message) error {
			if m.Flags == nil {
				absent++ // no reading (#909), never "no flags"
				return nil
			}
			readings = append(readings, reading{uid: int64(m.UID), flags: flagStrings(m.Flags)})
			return nil
		})
	if absent > 0 {
		summary.FlagsAbsent += absent
		e.obs.FlagsAbsent("sweep", absent)
	}
	if err != nil {
		// Nothing was applied: every reading waited for the command to finish.
		return fmt.Errorf("mailsync: flag sweep %q: %w", name, err)
	}
	if len(readings) == 0 {
		return nil
	}
	uids := make([]int64, len(readings))
	for i, r := range readings {
		uids[i] = r.uid
	}
	stored, err := e.store.LiveRemoteFlags(ctx, req.UserID, mailboxID, int64(status.UIDValidity), uids)
	if err != nil {
		return fmt.Errorf("mailsync: flag sweep %q: read recorded flags: %w", name, err)
	}
	applied := 0
	for _, r := range readings {
		cur, ok := stored[r.uid]
		if !ok {
			// No live copy recorded at this UID: not the sweep's business. A
			// UID below the cursor with no row is a #93 gap, which the
			// observation re-fetches with its body.
			continue
		}
		if cur.PendingWriteback {
			summary.FlagSweepDeferred++
			continue
		}
		reading := r.flags
		if !keywords {
			reading = withRecordedKeywords(r.flags, cur.Flags)
		}
		if sameFlags(cur.Flags, reading) {
			continue
		}
		res, ferr := e.ingest.RefreshFlags(ctx, req.UserID, ingest.Provenance{
			AccountID: req.AccountID, MailboxID: mailboxID,
			UID: r.uid, UIDValidity: int64(status.UIDValidity), RemoteFlags: reading,
		})
		if ferr != nil {
			return fmt.Errorf("mailsync: flag sweep %q uid %d: %w", name, r.uid, ferr)
		}
		if res.Found {
			applied++
		}
	}
	summary.FlagSweepRead += len(readings)
	summary.FlagSweepApplied += applied
	summary.FlagUpdates += applied
	e.obs.FlagSweep(len(readings), applied)
	return nil
}

// withRecordedKeywords is a server reading's SYSTEM flags (those starting
// with a backslash) plus the recorded copy's non-system keywords — the reading
// to apply for a mailbox that does not keep keywords.
func withRecordedKeywords(server, recorded []string) []string {
	out := make([]string, 0, len(server)+len(recorded))
	for _, f := range server {
		if strings.HasPrefix(f, `\`) {
			out = append(out, f)
		}
	}
	for _, f := range recorded {
		if !strings.HasPrefix(f, `\`) {
			out = append(out, f)
		}
	}
	return out
}

// sameFlags reports whether two flag lists name the same flags, ignoring
// order, case (IMAP flags and keywords are case-insensitive in practice) and
// \Recent, which is per-session state a server may report on one connection
// and not the next — comparing it would make every sweep "change" every
// message.
func sameFlags(a, b []string) bool {
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		seen := map[string]bool{}
		for _, f := range in {
			k := strings.ToLower(strings.TrimSpace(f))
			if k == "" || k == `\recent` || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	x, y := norm(a), norm(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// sweepable reports whether the flag sweep should run against this session:
// configured, and the server has no CONDSTORE and no QRESYNC.
func (e *Engine) sweepable(sess sync.SyncSession, condStore bool) bool {
	return e.sweepCfg.enabled() && !condStore && !sess.Supports(sync.CapabilityQResync)
}
