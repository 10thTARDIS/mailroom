// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// syncScopeOff is the mailbox sync_scope value that disables syncing (spec §4).
const syncScopeOff = "off"

// DefaultReapInterval is how often a mailbox's provenance is reconciled against
// the server's live UID set when no interval is configured (spec §4, #86, #93).
//
// It is deliberately an order of magnitude slower than the incremental poll. The
// poll is cheap and incremental (a UID range above the last-seen UID, plus a
// MODSEQ-filtered flag fetch); the observation asks for a whole mailbox's UID set
// every time. That is affordable — UIDs are 4 bytes, so even a 100k-message folder
// answers in a few hundred kilobytes — but it is not free, and "a message another
// client deleted disappears within the hour" is the right service level for it.
// New mail still arrives at the poll interval; only VANISHING is on this clock.
//
// It is also the DETECTION latency for a gap (#93): a message our own fetch
// silently dropped is noticed within one interval. Once it IS noticed, recovery is
// at POLL speed, not reap speed — an open gap deliberately leaves the mailbox due,
// so it is retried on every pass until the message is actually ingested.
const DefaultReapInterval = time.Hour

// maxGapBackfill bounds how many missing UIDs (#93) one pass re-fetches from a
// single mailbox. A gap is meant to be a handful of messages a truncated response
// dropped; a mailbox that turns up hundreds of them is anomalous, and re-fetching
// them in bounded batches keeps one pathological folder from issuing a single
// enormous FETCH of message bodies. A full batch means "there is more to do", so
// the mailbox is left due and the next pass continues draining it.
const maxGapBackfill = 500

// DefaultFetchChunk is the most UIDs one body FETCH command may ask for (#656).
//
// Every FETCH that downloads message bodies is split into commands of at most
// this many UIDs. The new-message pass used to ask for `lastSeen+1:*` as ONE
// command, which is fine for a poll's handful of arrivals and fatal for a
// backlog: after a very large number of messages were moved into INBOX on the provider, that one
// command was huge, died mid-stream twice, and the provider then answered
// `NO [UNAVAILABLE]` to it on every pass while a fresh session fetching 50 bodies
// worked at once. The folder stopped syncing, and the mail moved into it stayed
// soft-deleted until its fetch succeeded. The failure was the SIZE OF ONE
// COMMAND, so the fix bounds the command.
//
// The bound is on UIDs, not bytes, because that is what a command can name.
// Streaming is unchanged: each message is still ingested as it arrives
// (fetchAndIngest), so peak memory stays one message whatever N is.
const DefaultFetchChunk = 500

// Engine synchronizes a single IMAP account into the canonical store. It is
// safe for concurrent use across distinct accounts; a single account should be
// synced by one Engine call at a time (the store cursors serialize correctness
// but concurrent selects of the same mailbox waste work).
type Engine struct {
	dial   SessionFactory
	store  MailboxStore
	ingest Ingester

	reapInterval time.Duration
	fetchChunk   int // UIDs per body FETCH command; see DefaultFetchChunk
	now          func() time.Time

	obs      Observer        // correctness counters (flags.go); never nil
	sweepCfg FlagSweepConfig // the CONDSTORE-less flag sweep (flags.go); zero = off
	sweeps   sweepStates
}

// Option configures an Engine.
type Option func(*Engine)

// WithReapInterval sets how often each mailbox's provenance is reconciled against
// the server's live UID set (default DefaultReapInterval).
//
// Zero DISABLES THE REAPER — an explicit opt-out for an operator who would rather
// keep stale folder membership than let anything delete a provenance row; a
// negative value is treated the same way.
//
// It disables ONLY THAT. The live-UID observation still runs, on the default
// cadence, because the SAME answer also drives the gap backfill (#93), and the two
// conclusions drawn from it are opposites: the reap REMOVES provenance for UIDs
// the server no longer has, while the backfill ADDS provenance for UIDs the server
// DOES have and our own fetch silently dropped. An operator switching off "let
// nothing delete a provenance row" is not asking for "let a truncated FETCH lose
// my mail forever", and this switch is not allowed to mean that. There is
// deliberately no way to turn gap detection off: it can only ever add mail.
//
// The cadence is per MAILBOX and persisted (app.mailboxes.last_reaped_at), not
// per process: it survives restarts, and a big account's mailboxes come due at
// different times rather than all reconciling in one pass.
//
// ⚠ THAT LAST CLAUSE WAS NOT TRUE UNTIL #638, and the correction is worth
// keeping. A per-mailbox cadence does not by itself spread anything: every
// mailbox observed in one pass is stamped within seconds of the others, so a
// flat interval brings the whole cohort due together again, and a FIRST sync
// has every mailbox due at once with nothing to spread it. What makes the claim
// true is reapJitter — a deterministic per-mailbox offset of up to 20% of the
// interval, added to the due time.
func WithReapInterval(d time.Duration) Option {
	return func(e *Engine) { e.reapInterval = d }
}

// observeInterval is the cadence for asking the server for a mailbox's whole live
// UID set — the single question that drives both the reap and the gap backfill.
// It follows the reap interval, and falls back to the default when the reaper is
// disabled, because the backfill still needs the answer (see WithReapInterval).
func (e *Engine) observeInterval() time.Duration {
	if e.reapInterval > 0 {
		return e.reapInterval
	}
	return DefaultReapInterval
}

// reaping reports whether this engine is allowed to draw the DESTRUCTIVE
// conclusion from an observation (WithReapInterval(0) turns it off).
func (e *Engine) reaping() bool { return e.reapInterval > 0 }

// WithClock replaces the engine's clock (tests).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithFetchChunk sets how many UIDs one body FETCH command may name (default
// DefaultFetchChunk). A value below 1 keeps the default. It exists so tests can
// drive the chunk walk with a handful of messages; production has no reason to
// change it.
func WithFetchChunk(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.fetchChunk = n
		}
	}
}

// New constructs an Engine. dial defaults to sync.Dial when nil.
func New(mailboxes MailboxStore, ingester Ingester, dial SessionFactory, opts ...Option) *Engine {
	if dial == nil {
		dial = sync.Dial
	}
	e := &Engine{
		dial:         dial,
		store:        mailboxes,
		ingest:       ingester,
		reapInterval: DefaultReapInterval,
		fetchChunk:   DefaultFetchChunk,
		now:          time.Now,
		obs:          nopObserver{},
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// AccountSync is one account's sync request. Credentials are already decrypted
// into Config and the mail-blob DEK is already unwrapped (DEK/DEKID) — key
// management is the caller's responsibility, exactly as for ingest.Ingest.
type AccountSync struct {
	UserID    ulid.ULID
	AccountID ulid.ULID
	Config    sync.Config
	DEK       []byte
	DEKID     ulid.ULID

	// Gmail switches the account onto the Gmail sync model (#52, ADR-047;
	// gmail.go). Nil for every other account, which then syncs exactly as
	// before.
	Gmail *GmailSync

	// ProviderProfile is the account's app.accounts.provider_profile. It
	// selects the profile's stated folder-name map (a folder listed without
	// SPECIAL-USE gets its role from the name) and its non-mail folders
	// (provider.FolderSpecialUse / provider.NonMailFolder; ADR-046 §12).
	// Empty or "generic" changes nothing.
	ProviderProfile string
}

// mailboxSpecialUse is a listed mailbox's SPECIAL-USE attribute, or — when
// the server sent none — the one the account's profile states for its name.
func mailboxSpecialUse(req AccountSync, mb sync.Mailbox) string {
	if su := specialUse(mb); su != "" {
		return su
	}
	return provider.FolderSpecialUse(req.ProviderProfile, mb.Name, mb.Delimiter)
}

func (r AccountSync) validate() error {
	switch {
	case r.UserID == (ulid.ULID{}):
		return errors.New("mailsync: user id is required")
	case r.AccountID == (ulid.ULID{}):
		return errors.New("mailsync: account id is required")
	case len(r.DEK) == 0:
		return errors.New("mailsync: mail DEK is required")
	case r.DEKID == (ulid.ULID{}):
		return errors.New("mailsync: mail DEK id is required")
	default:
		return nil
	}
}

// Summary reports the outcome of a SyncAccount call. Per-mailbox failures are
// collected in Errors and are non-fatal — sync continues with the next
// mailbox, and a failed mailbox's cursors are left unadvanced so the next run
// safely re-fetches it (ingest is idempotent).
type Summary struct {
	MailboxesSynced  int
	MailboxesSkipped int // sync_scope='off' or non-selectable
	MessagesIngested int // new canonical messages created
	FlagUpdates      int // re-ingested messages that deduped (flag/location merges)

	// MessagesMuted is the part of MessagesIngested that arrived in a muted
	// conversation's inbox and was placed in Archive instead (ingest.Result.Muted,
	// spec §6). It is a SUBSET, never additional: every muted arrival is also a
	// new canonical message. syncrunner pushes for MessagesIngested minus this, so
	// a muted conversation never wakes the user's devices. Counted at all three
	// fetchAndIngest call sites, like MessagesIngested itself — a muted reply
	// recovered by the gap backfill is just as muted as one from the poll.
	MessagesMuted int

	// MessagesBlocked is the part of MessagesIngested that came from a BLOCKED
	// sender and was moved to Junk or Trash on arrival (ingest.Result.Blocked,
	// #59). A subset like MessagesMuted, and disjoint from it (a blocked
	// arrival never reaches the mute check). syncrunner pushes for neither.
	MessagesBlocked int

	// NotifyIDs are the new canonical messages this pass that the user may be
	// notified about — MessagesIngested minus the muted and the blocked ones,
	// one id each, in ingest order. The push sender counts them by category
	// (store.PushCounts) for the triage-aware push modes (#59). Held for one
	// pass only; a first sync of 25k messages is 25k ids (~400 KB).
	NotifyIDs []ulid.ULID

	// BodiesFetched and BodyBytesFetched count every message BODY this pass
	// actually downloaded — across the new-message pass, the flag-pass's
	// body fallback, and the gap backfill (#93) — the three fetchAndIngest
	// call sites. They exist because a flags-only server change used to cost a
	// full body download (and, downstream, a full malware scan) and nothing
	// measured it (#523): FlagUpdates alone cannot distinguish a cheap
	// flags-only refresh from an expensive re-ingest, and this is that
	// measurement. The CONDSTORE flag-reconcile pass itself contributes ZERO
	// to either — that is the point of #523 — so a healthy pass with no new
	// mail and no gaps reports 0/0 here even while FlagUpdates is nonzero.
	BodiesFetched    int   // message bodies downloaded this pass
	BodyBytesFetched int64 // their total size in bytes

	// --- the expunge reaper (#86) ---
	MailboxesReaped   int // mailboxes whose live UID set was reconciled this pass
	ProvenanceReaped  int // physical locations the server no longer has, dropped
	MessagesVanished  int // messages left in NO location, soft-deleted (ADR-009)
	PlacementsRetired int // canonical placements (moved_mailbox_id) the reaper made redundant

	// --- folder deletion (#87) ---
	// MailboxesDeleted counts folders the server's LIST no longer reports and that
	// this pass therefore reconciled and destroyed (a Mailbox `destroyed` row, plus
	// the membership fallout). Its dropped provenance is counted in ProvenanceReaped
	// and its soft-deletes in MessagesVanished — both are the same reconciliation the
	// reaper does (reapMessage), applied to a whole folder at once.
	MailboxesDeleted int

	// --- the UIDVALIDITY roll (#88) ---
	// GenerationsReconciled counts mailboxes whose UIDVALIDITY rolled this pass
	// AND whose retired generation was successfully reconciled afterwards. A
	// mailbox that rolled but could not be reconciled (a failed or unprovable
	// re-fetch) is NOT counted here — it is an entry in Errors, its cursors are
	// left on the old generation, and it is retried next pass.
	GenerationsReconciled int
	// ProvenanceRetired is the number of retired-generation provenance rows
	// dropped. MessagesVanished (above) is shared with the reaper: both paths
	// soft-delete a message left in no location at all, through the same code.
	ProvenanceRetired int

	// --- the gap backfill (#93) ---
	// MailboxesVerified counts mailboxes this pass PROVED complete: every UID the
	// server reports below our own last-seen cursor has a live provenance row. A
	// mailbox with an open gap is not counted, is not reaped, and stays due.
	MailboxesVerified int
	// GapsDetected is the number of UIDs the server holds, below our cursor, that
	// we had never ingested — each one a message a silently-truncated FETCH lost
	// and that nothing would ever have asked for again. It should be zero. That it
	// is not is the only thing that makes the loss recoverable.
	GapsDetected int
	// GapsBackfilled is how many of those this pass re-fetched and ingested. A
	// shortfall is not fatal (a UID can be expunged between the SEARCH and the
	// FETCH) but it does leave the mailbox unverified, unreaped and due.
	GapsBackfilled int
	// CopiesAwaitingExpunge counts fetched copies that were NOT recorded because a
	// queued draft EXPUNGE is about to remove them from that very folder (#686,
	// ingest.Result.AwaitingExpunge). Not an error and not ingested: a superseded
	// draft revision's copy read in the seconds between its retirement and the
	// writeback pass that removes it. A mailbox observed while holding one is NOT
	// verified (so not reaped and not stamped) and is due again next pass, the same
	// fail-closed state as any incomplete view — but without a WARN, because the
	// cause is known and is ours.
	CopiesAwaitingExpunge int

	// --- the server's SEARCH rate limit (#638) ---
	// MailboxesDeferred counts the due mailboxes this pass did NOT observe
	// because the server refused a UID SEARCH for rate limiting: the one that
	// was refused, plus every due mailbox after it that the pass therefore
	// never asked about. It is NOT an error count — none of these mailboxes
	// appears in Errors, deliberately. Before #638 each refusal was recorded as
	// that mailbox's own failure, so one throttled account produced 300–600
	// identical WARNs per pass and a genuinely broken mailbox was invisible
	// among them.
	//
	// A deferred mailbox is in exactly the state a mailbox whose observation
	// failed has always been in: not verified, not reaped, cursor not stamped,
	// due again next pass. Nothing about the fail-closed guarantees changes —
	// what changes is that we stop sending commands the server has already
	// refused.
	MailboxesDeferred int
	// RateLimitRetryAfter is the wait the server named in that refusal
	// ("please wait 42s"), or zero when it named none. Zero is NOT "retry
	// immediately"; it means the server gave no number.
	RateLimitRetryAfter time.Duration
	// MailboxesObserved counts the mailboxes whose live UID set the server
	// ANSWERED this pass — exactly one UID SEARCH each, so over an hour it is the
	// account's SEARCH rate, the number #638's pacing budget is spent on. A
	// refused SEARCH is in MailboxesDeferred instead, and a mailbox whose answer
	// then failed a trust gate is still counted here: the question was asked.
	//
	// ⚠ IT IS NOT MailboxesReaped, and reading that one as this is how a
	// production measurement undercounted observations by half:
	// an observation with nothing to prove is stamped but never reaped, so
	// MailboxesReaped only counts the mailboxes that went through ReapMailbox.
	MailboxesObserved int

	// --- skipping what did not change (#620) ---
	// MailboxesUnchanged counts the folders phase 1 did NOT SELECT, because
	// LIST-STATUS reported the same UIDVALIDITY, UIDNEXT, MESSAGES and
	// HIGHESTMODSEQ as the last poll's own SELECT and the poll had nothing
	// outstanding (see pollUnchanged). It is a SUBSET of MailboxesSynced, never
	// additional: a folder the LIST proved current is in exactly the good
	// standing a folder whose SELECT found nothing is, so `mailboxes` on the log
	// line keeps meaning "folders in good standing this pass" and does not
	// shrink the way it does when folders fail (#277).
	MailboxesUnchanged int
	// ObservationsSkipped counts the DUE folders whose live-UID observation
	// (EXAMINE + UID SEARCH) phase 2 did not issue, because LIST-STATUS matched
	// what the last REAL observation saw (see observationUnchanged). Only the
	// cadence cursor was re-stamped: nothing was verified, backfilled or reaped
	// on the strength of a skip. It is NOT in MailboxesObserved, which stays
	// "SEARCHes the server answered".
	ObservationsSkipped int

	// --- the Gmail sync model (#52, ADR-047; zero off Gmail) ---
	// GmailFoldersSkipped counts the label folders (INBOX, user labels,
	// Important, Starred, Sent, Drafts) listed and NOT selected. A subset of
	// MailboxesSkipped.
	GmailFoldersSkipped int
	// HistoryBodiesFetched is the part of BodiesFetched that was newest-first
	// history below a backfill floor.
	HistoryBodiesFetched int
	// GmailBudgetSpent is true when the download budget (a rolling 24 hours)
	// stopped work this pass; the rest continues as the window frees up.
	GmailBudgetSpent bool
	// GmailLabelChanges counts what Gmail label readings changed: labels
	// added or removed and folder placements moved.
	GmailLabelChanges int
	// GmailLabelSweeps counts mailboxes whose labels the sweep re-read.
	GmailLabelSweeps int

	// --- reading FLAGS correctly (#909) and without CONDSTORE (ADR-046 §12) ---
	// FlagsAbsent counts FETCH responses that lacked the FLAGS item they were
	// asked for. A flag refresh or sweep skips such a message (no reading, never
	// "no flags"); a new message is ingested and its flags re-read. It should
	// be zero; a server that keeps producing it is splitting or truncating
	// its responses.
	FlagsAbsent int
	// FlagRefreshAborted counts flag readings a refresh applied in a FETCH
	// command that then failed. Each was a genuine reading (FLAGS present);
	// the count is #909's report — before the fix, any of them could have
	// been an empty FLAGS cut short.
	FlagRefreshAborted int
	// FlagSweeps counts mailboxes whose newest UIDs the CONDSTORE-less flag
	// sweep re-read this pass, FlagFullSweeps the whole-mailbox passes that
	// finished. FlagSweepRead is the messages it read, FlagSweepApplied the
	// ones whose server flags differed from what Mailroom had recorded and
	// were applied (also in FlagUpdates). All zero on a CONDSTORE server.
	FlagSweeps       int
	FlagFullSweeps   int
	FlagSweepRead    int
	FlagSweepApplied int
	// FlagSweepDeferred counts copies the sweep left alone because a local
	// flag edit for them is still on its way to the server (pending writeback).
	FlagSweepDeferred int

	// Duration is the wall time of SyncAccountWith for this account — the
	// figure #620 exists to bring down.
	Duration time.Duration

	Errors []MailboxError
}

// MailboxError is a non-fatal failure syncing or reaping one mailbox. An empty
// Mailbox is an account-level failure (the per-user retirement sweep).
type MailboxError struct {
	Mailbox string
	Err     error
}

func (e MailboxError) Error() string {
	if e.Mailbox == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("mailbox %q: %v", e.Mailbox, e.Err)
}
func (e MailboxError) Unwrap() error { return e.Err }

func (s *Summary) addError(mailbox string, err error) {
	s.Errors = append(s.Errors, MailboxError{Mailbox: mailbox, Err: err})
}

// reapTarget is a mailbox that this pass POLLED SUCCESSFULLY, and is therefore a
// candidate for the live-UID observation that drives both the reap (#86) and the
// gap backfill (#93). Only successfully-polled mailboxes get in here: that is the
// structural half of "a failed sync never reaps anything".
type reapTarget struct {
	name        string
	id          ulid.ULID
	uidValidity int64 // the generation this pass actually reconciled against
	// lastSeenUID is the cursor this pass LEFT BEHIND — the top of the UID space
	// the poll claims to have covered. It is the bound the gap detector measures
	// against: a UID at or above it will be re-requested by the next poll's
	// `lastSeen+1:*` range, so it is not lost; a UID BELOW it never will be, so a
	// missing one is the #93 bug. See store.GapObservation.UIDBound.
	lastSeenUID int64
	lastReaped  *time.Time

	// listed is what this pass's LIST-STATUS reported for the mailbox (nil =
	// unknown); observed and lastFullObs are the OBSERVATION-TIME baseline and
	// when it was recorded. Together they decide whether a due observation may
	// be skipped (#620, observationUnchanged).
	listed      *sync.ListStatus
	observed    *store.ObservedStatus
	lastFullObs *time.Time

	// gapFloor is a Gmail mailbox's backfill floor (#52): a gap is a UID at
	// or above it, never one below (that is history not imported yet). 0 or 1
	// = no floor. gmail is the pass's Gmail state, nil off Gmail; with it, a
	// gap refetch is metered and refused once the budget is spent.
	gapFloor int64
	gmail    *gmailPass
}

// verifiedMailbox is a mailbox whose live-UID observation was TRUSTWORTHY and
// whose provenance this pass then proved complete (no gaps, or gaps that were
// re-fetched and ingested). Only these are handed to the reaper.
//
// That gate is the fail-closed rule #93 adds: a mailbox we know we are MISSING a
// message from is a mailbox we have an incomplete picture of, and an incomplete
// picture is not a licence to delete rows from it. It is also cheap — the gap
// closes on the next pass and the mailbox reaps then.
type verifiedMailbox struct {
	target reapTarget
	// obs is the reap's observation, retained from the verification so the pass
	// asks the server for the mailbox's UID set exactly ONCE. Nil means the answer
	// was fine but there was nothing it could prove absent (an untouched UID
	// space): trustworthy, so the cadence cursor is stamped, but nothing to reap.
	obs *store.ReapObservation
	// seen is the observation-time baseline this observation establishes (#620),
	// written with the stamp. Nil when the observation's proof did not reach the
	// top of the UID space it saw (observedBaseline) — then no baseline is
	// recorded and the next due observation runs for real.
	seen *store.ObservedStatus
}

// rolledMailbox is a mailbox whose UIDVALIDITY changed in this pass: its old
// generation has been retired to 'stale_uidvalidity' and it has been fully
// re-fetched under the new one — but the retired generation has NOT yet been
// reconciled, so its cursors are deliberately still unwritten (see syncMailbox).
type rolledMailbox struct {
	name string
	id   ulid.ULID

	// uidValidity is the NEW generation the re-fetch ran under.
	uidValidity int64

	// refetchBound is UIDNEXT as reported by the SELECT that drove the re-fetch:
	// the exclusive top of the UID space the re-fetch actually covered. It is
	// what the completeness proof is bounded by, so that mail delivered AFTER the
	// re-fetch (which necessarily lands at or above it) cannot be mistaken for a
	// message the re-fetch failed to bring back. Zero when the server sent no
	// UIDNEXT at all.
	refetchBound int64

	// cursors are the mailbox's post-re-fetch sync cursors, withheld from the
	// database until the reconcile lands so that "the roll finished" and "the
	// cursors name the new generation" are one commit — and a roll that could not
	// be finished is still pending on the next pass rather than lost.
	cursors MailboxCursors
}

// mailboxSync is what one successfully polled mailbox hands to the later phases.
type mailboxSync struct {
	uidValidity int64
	// lastSeenUID is the cursor the poll just wrote: the top of the UID space it
	// claims to have covered, and therefore the bound the gap detector uses (#93).
	lastSeenUID int64
	// roll is non-nil iff this mailbox's UIDVALIDITY changed in this pass, in
	// which case its cursors have NOT been written and its retired generation is
	// awaiting reconciliation.
	roll *rolledMailbox
}

// SyncAccount dials the account, discovers its mailboxes, and reconciles each
// one incrementally, ingesting new and flag-changed messages. A dial or
// mailbox-list failure is fatal (returned as the error). Per-mailbox failures
// are recorded in the returned Summary and do not abort the run.
//
// It runs in phases, and the order is load-bearing:
//
//  1. POLL every mailbox (new messages + CONDSTORE flag changes), ingesting as
//     it goes — except a folder whose LIST-STATUS proves the poll would find
//     nothing (#620, pollUnchanged): it is upserted and stays a phase-2
//     target, but is not SELECTed.
//  2. VERIFY every due mailbox against the server's live UID set, and BACKFILL
//     the gaps: UIDs the server holds, below our own cursor, that we never
//     ingested because a FETCH silently dropped them (#93). A due folder whose
//     LIST-STATUS matches what its last REAL observation saw — not its last
//     poll — is not asked again: only its cadence is re-stamped, it is neither
//     verified nor reaped, and a real observation is forced at least every
//     24 hours (#620, observationUnchanged).
//  3. RECONCILE the RETIRED GENERATION of every mailbox whose UIDVALIDITY rolled
//     in phase 1, once its re-fetch can be proved complete (#88).
//  4. REAP the verified mailboxes, diffing each one's provenance against the live
//     UID set phase 2 already read (#86).
//  5. RETIRE the canonical placements that 3 or 4 have made redundant.
//
// EVERY ADDITIVE PHASE COMES BEFORE EVERY DESTRUCTIVE ONE, across the whole
// account. That is not tidiness, it is the safety property.
//
// A message another client MOVED between two folders is, for an instant, in
// neither: gone from the source, and in a destination we have not looked at yet.
// Polling everything first means the destination's provenance already exists by
// the time the source is reaped or its retired generation reconciled, so the move
// reads as a move rather than as a disappearance.
//
// Phase 2 extends that to the case where the destination's fetch was TRUNCATED:
// the message is on the server in the destination, we hold no row for it there,
// and its source row is about to be reaped. Backfilling every mailbox before
// reaping any of them puts the row back before anything can conclude the message
// is nowhere — so it is never soft-deleted at all, rather than being soft-deleted
// and retracted a moment later. (Neither ordering is a complete answer — the move
// can still land inside the pass — which is why BOTH soft-delete paths stay
// retractable; see store.UserTx.RetractVanished.)
func (e *Engine) SyncAccount(ctx context.Context, req AccountSync) (Summary, error) {
	var summary Summary
	if err := req.validate(); err != nil {
		return summary, err
	}

	sess, err := e.dial(ctx, req.Config)
	if err != nil {
		return summary, fmt.Errorf("mailsync: dial account %s: %w", req.AccountID, err)
	}
	defer func() {
		_ = sess.Logout(ctx) // best-effort: politeness, not correctness
		_ = sess.Close()
	}()

	return e.SyncAccountWith(ctx, req, sess)
}

// SyncAccountWith runs the exact same pass as SyncAccount over a CALLER-SUPPLIED
// session, and does NOT dial, log out, or close it — the caller owns the
// connection's whole lifetime. It is the entry point for syncrunner's warm
// per-account sync pool (mailroom#103 Stage 3), which reuses one connection
// across passes instead of dialing TCP+TLS+LOGIN every time; SyncAccount is just
// the dial-and-close wrapper around it for non-pooled callers (mailroom-admin
// sync-once, and the pool-disabled runner path).
//
// The session must be authenticated and idle (no command in flight). Reuse is
// safe with NO mailbox reset because the pass is read-only and every phase
// SELECTs/EXAMINEs the mailbox it operates on unconditionally (syncMailbox's
// Select, LiveUIDs's EXAMINE), so nothing here depends on the session's prior
// selected state. All the fail-closed reaper/backfill/INBOX-guard ordering below
// is identical whether the session was freshly dialed or reused.
//
// ⚠ "EVERY PHASE SELECTs/EXAMINEs UNCONDITIONALLY" now has two exceptions
// (#620), and neither weakens the reuse argument: a folder skipped in phase 1
// or phase 2 is not SELECTed at all, so it issues no command that could
// depend on the selected mailbox either.
//
// Summary.Duration is this call's wall time.
func (e *Engine) SyncAccountWith(ctx context.Context, req AccountSync, sess sync.SyncSession) (Summary, error) {
	start := time.Now()
	summary, err := e.syncAccountWith(ctx, req, sess)
	summary.Duration = time.Since(start)
	return summary, err
}

// syncAccountWith is SyncAccountWith's body; the wrapper only times it.
func (e *Engine) syncAccountWith(ctx context.Context, req AccountSync, sess sync.SyncSession) (Summary, error) {
	var summary Summary
	if err := req.validate(); err != nil {
		return summary, err
	}

	mailboxes, err := sess.ListMailboxes(ctx)
	if err != nil {
		return summary, fmt.Errorf("mailsync: list mailboxes: %w", err)
	}

	condStore := sess.Supports(sync.CapabilityCondStore)

	var gm *gmailPass
	if req.Gmail != nil {
		if gm, err = e.startGmailPass(ctx, req, sess, mailboxes); err != nil {
			summary.addError("", err)
			return summary, err
		}
		// EVERY ROW BEFORE ANY MESSAGE. A Gmail message's folder placement
		// names the INBOX, Sent or Drafts row (store.ApplyGmailObservation),
		// and LIST order puts [Gmail]/Sent Mail AFTER [Gmail]/All Mail — so on
		// a first pass, All Mail's messages would be ingested before the Sent
		// row existed, and a sent message would land in All Mail. A failure
		// here is recorded by the loop below, which upserts again.
		for _, mb := range mailboxes {
			stored, uerr := e.store.UpsertMailbox(ctx, req.UserID, MailboxRecord{
				AccountID: req.AccountID, IMAPName: mb.Name,
				DisplayName: displayName(mb.Name, mb.Delimiter), SpecialUse: mailboxSpecialUse(req, mb),
			})
			// A label view other than INBOX/Sent/Drafts is marked NOT SYNCED
			// (sync_scope='off', ADR-047 §1), so clients render it as the
			// frozen, never-fetched folder it is rather than as an empty live
			// one. INBOX, Sent and Drafts stay 'full': they are where Gmail's
			// \Inbox, \Sent and \Draft labels place messages.
			if uerr == nil && stored.SyncScope != syncScopeOff && gmailLabelView(mb) {
				if serr := gm.store.SetSyncScopeOff(ctx, req.UserID, stored.ID); serr != nil {
					summary.addError(mb.Name, fmt.Errorf("mailsync: mark Gmail label view not synced: %w", serr))
				}
			}
		}
	}

	// --- Phase 1: poll -------------------------------------------------------
	var (
		targets []reapTarget
		rolled  []rolledMailbox
	)
	for _, mb := range mailboxes {
		stored, err := e.store.UpsertMailbox(ctx, req.UserID, MailboxRecord{
			AccountID:   req.AccountID,
			IMAPName:    mb.Name,
			DisplayName: displayName(mb.Name, mb.Delimiter),
			SpecialUse:  mailboxSpecialUse(req, mb),
		})
		if err != nil {
			summary.addError(mb.Name, fmt.Errorf("mailsync: upsert mailbox: %w", err))
			continue
		}
		if gm != nil && !gmailMailboxSynced(mb) {
			// A Gmail LABEL VIEW (ADR-047 §1): listed, upserted for role
			// mapping, never selected.
			summary.MailboxesSkipped++
			summary.GmailFoldersSkipped++
			continue
		}
		// A profile's NON-MAIL folder candidate (Exchange's Calendar, Contacts,
		// …; ADR-046 §12). Decided ONCE, on the pass that CREATED the row — a
		// folder the user later switches back on is never switched off again
		// (review M-H2 of #910) — and never on the name alone (review M3): a
		// localised mailbox, or a user's own mail folder called "Notes", must
		// not stop syncing. It needs evidence too: the server marks it
		// \Noselect (here), or its first SELECT shows it empty (syncMailbox).
		nonMail := stored.Created && stored.SyncScope != syncScopeOff &&
			provider.NonMailFolder(req.ProviderProfile, mb.Name, mb.Delimiter)
		if nonMail && !selectable(mb) {
			if serr := e.store.SetSyncScopeOff(ctx, req.UserID, stored.ID); serr != nil {
				summary.addError(mb.Name, fmt.Errorf("mailsync: mark non-mail folder not synced: %w", serr))
			}
		}
		if stored.SyncScope == syncScopeOff || !selectable(mb) {
			summary.MailboxesSkipped++
			continue
		}
		if gm != nil {
			// The Gmail model never skips a poll on LIST-STATUS: the history
			// backfill and the label sweep run on the poll, whatever changed.
			out, floor, err := e.syncGmailMailbox(ctx, req, sess, gm, condStore, mb.Name, stored, &summary)
			if errors.Is(err, errBudgetSpent) {
				// Not a failure, and not a phase-2 target: part of the mailbox's
				// new mail is still owed, so it is not verified or reaped today.
				summary.MailboxesSynced++
				continue
			}
			if err != nil {
				if errors.Is(err, ingest.ErrQuotaExhausted) || errors.Is(err, ingest.ErrScannerUnavailable) {
					summary.addError(mb.Name, err)
					return summary, err
				}
				summary.addError(mb.Name, err)
				continue
			}
			summary.MailboxesSynced++
			targets = append(targets, reapTarget{
				name: mb.Name, id: stored.ID, uidValidity: out.uidValidity,
				lastSeenUID: out.lastSeenUID, lastReaped: stored.LastReapedAt,
				listed: mb.Status, observed: stored.Observed, lastFullObs: stored.LastObservedFullAt,
				gapFloor: floor, gmail: gm,
			})
			continue
		}
		if pollUnchanged(condStore, mb.Status, stored) {
			// NOTHING TO POLL (#620). The SELECT would report exactly the status
			// the last poll reconciled against, so its flag pass would have no
			// MODSEQ to fetch above and its new-message pass no UID to fetch —
			// the whole poll would be a round trip that changes nothing. The
			// folder was still upserted above (which is also how a NEW folder is
			// discovered), and it is still a phase-2 target on the stored
			// cursors, which are exactly what the SELECT would have rewritten.
			summary.MailboxesSynced++
			summary.MailboxesUnchanged++
			targets = append(targets, reapTarget{
				name: mb.Name, id: stored.ID, uidValidity: stored.UIDValidity,
				lastSeenUID: stored.LastSeenUID, lastReaped: stored.LastReapedAt,
				listed: mb.Status, observed: stored.Observed, lastFullObs: stored.LastObservedFullAt,
			})
			continue
		}
		out, err := e.syncMailbox(ctx, req, sess, condStore, mb.Name, stored, nonMail, &summary)
		if errors.Is(err, errNonMailFolder) {
			// Empty on its first SELECT and a profile non-mail name: set not
			// synced (by syncMailbox); not a reap target.
			summary.MailboxesSkipped++
			continue
		}
		if err != nil {
			if errors.Is(err, ingest.ErrQuotaExhausted) || errors.Is(err, ingest.ErrScannerUnavailable) {
				// THE TWO ACCOUNT-WIDE PAUSES. Neither is a per-mailbox hiccup to
				// isolate and continue past: the provider still holds mail we have
				// refused, for a reason every later mailbox would hit identically.
				//
				//   ErrQuotaExhausted     — no room for it (spec §10).
				//   ErrScannerUnavailable — no scanner to judge it (ADR-027 §4c).
				//
				// STOP the whole pass and return NON-NIL so SyncAccount reports
				// failure: the runner records account health from err.Error() (a nil
				// return would record SUCCESS and hide the pause), while OTHER
				// accounts keep syncing — per-account isolation lives in the runner.
				//
				// ⚠ THE RETURN IS DOING TWO JOBS, AND THE SECOND IS THE ONE THAT IS
				// EASY TO MISS. The refused message was not ingested, and syncMailbox
				// withheld every cursor that could carry the pass past it (at most it
				// recorded the completed chunks BELOW it, with no poll baseline — see
				// recordPartialPoll) — that makes the refusal a RETRY. Returning HERE additionally skips phases 2–5 for
				// the whole account, and phase 4 is the expunge reaper: without this
				// return, THIS mailbox's failure would be isolated and the account's
				// OTHER mailboxes would go on to be polled, reaped, and have folders
				// reconciled. "We could not scan the INBOX" would become "mail was
				// deleted from Archive". A refusal that withheld cursors and forgot
				// the reaper would be worse than having no scanner at all (ADR-027
				// §4c), which is what mailsync/scanner_test.go's negative control
				// exists to prove.
				//
				// The mail is re-fetched on the next pass once the user frees space,
				// the operator raises the limit, or the scanner answers again.
				//
				// ⚠ The two sentinels are DISTINCT on purpose even though the
				// handling is identical: account health is built from err.Error(),
				// and reporting a scanner outage as "over the storage quota" would
				// send the operator to look at disk usage while a container is down.
				summary.addError(mb.Name, err)
				return summary, err
			}
			// The poll failed, so this mailbox is NOT a reap candidate: we have no
			// trustworthy view of it, and "we could not read the mailbox" must never
			// become "the mailbox is empty".
			summary.addError(mb.Name, err)
			continue
		}
		summary.MailboxesSynced++
		if out.roll != nil {
			// A rolled mailbox is unfinished business, not a reap candidate: its
			// cursors are still unwritten and its retired generation is still on the
			// table. Phase 2 owns it. (It also needs no reap — every live row it has
			// was created by the re-fetch that just ran, so there is nothing surplus
			// for the reaper to find.)
			rolled = append(rolled, *out.roll)
			continue
		}
		targets = append(targets, reapTarget{
			name: mb.Name, id: stored.ID, uidValidity: out.uidValidity,
			lastSeenUID: out.lastSeenUID, lastReaped: stored.LastReapedAt,
			listed: mb.Status, observed: stored.Observed, lastFullObs: stored.LastObservedFullAt,
		})
	}

	// --- Phase 2: verify the poll, and backfill what it dropped (#93) ---------
	// ADDITIVE ONLY, and therefore ahead of everything below it.
	verified := e.verifyAccount(ctx, req, sess, condStore, targets, &summary)

	// --- Phase 3: finish the UIDVALIDITY rolls --------------------------------
	e.reconcileRolled(ctx, req, sess, rolled, &summary)

	// --- Phase 4: reap -------------------------------------------------------
	e.reapAccount(ctx, req, verified, &summary)

	// --- Phase 4b: reconcile folders the server no longer lists (#87) ---------
	// DESTRUCTIVE, so it too is behind every additive phase. It uses the same LIST
	// phase 1 already succeeded on — a folder we hold that a SUCCESSFUL LIST no
	// longer names is a deleted folder — and the same poll-then-destroy ordering
	// that protects a MOVE protects it here: a message moved out of a doomed folder
	// into a surviving one has had its new home polled before this runs, so it reads
	// as a move, not a disappearance.
	e.reconcileDeletedMailboxes(ctx, req, mailboxes, &summary)

	// --- Phase 5: retire redundant canonical placements ------------------------
	// It runs only after something has actually removed a provenance row, because
	// that is the only thing that can make the retirement guard hold (the source
	// folder's row has to be gone first) — and the reap, the generation reconcile
	// AND a folder deletion all do exactly that. It is per-user, not per-mailbox,
	// and membership-neutral by construction (store.UserTx.RetireCanonicalPlacements).
	if summary.MailboxesReaped > 0 || summary.GenerationsReconciled > 0 || summary.MailboxesDeleted > 0 {
		n, err := e.store.RetireCanonicalPlacements(ctx, req.UserID)
		if err != nil {
			summary.addError("", fmt.Errorf("mailsync: retire canonical placements: %w", err))
		} else {
			summary.PlacementsRetired += int(n)
		}
	}

	return summary, nil
}

// syncMailbox selects one mailbox and reconciles it against its stored cursors.
// On any error it returns without advancing the cursors past what it finished,
// so the rest of the mailbox is re-fetched next run (a new-message pass that
// fails part-way records only its completed chunks: recordPartialPoll). It
// returns the UIDVALIDITY generation it reconciled against — the coordinate
// space the reaper must diff in, and no other — and,
// when that generation is NEW (a UIDVALIDITY roll), the rolledMailbox that phase
// 2 has to finish.
func (e *Engine) syncMailbox(ctx context.Context, req AccountSync, sess sync.SyncSession, condStore bool, name string, stored StoredMailbox, nonMail bool, summary *Summary) (mailboxSync, error) {
	status, err := sess.Select(ctx, name, sync.SelectOptions{ReadOnly: true, CondStore: condStore})
	if err != nil {
		return mailboxSync{}, fmt.Errorf("mailsync: select %q: %w", name, err)
	}
	if nonMail && status.NumMessages == 0 {
		// The second piece of evidence for a profile non-mail folder (see the
		// caller): first sighting AND empty. A folder with anything in it
		// stays synced, whatever its name.
		if err := e.store.SetSyncScopeOff(ctx, req.UserID, stored.ID); err != nil {
			return mailboxSync{}, fmt.Errorf("mailsync: mark non-mail folder %q not synced: %w", name, err)
		}
		return mailboxSync{}, errNonMailFolder
	}

	// Effective cursors: a UIDVALIDITY reset rewinds them and retires the old
	// provenance generation, forcing a full re-fetch under the new validity.
	lastSeen := stored.LastSeenUID
	baseModSeq := stored.HighestModSeq
	rolled := stored.UIDValidity != 0 && stored.UIDValidity != int64(status.UIDValidity)
	if rolled {
		if _, err := e.store.MarkMailboxProvenanceStale(ctx, req.UserID, stored.ID); err != nil {
			return mailboxSync{}, fmt.Errorf("mailsync: retire stale generation %q: %w", name, err)
		}
		lastSeen = 0
		baseModSeq = 0
	}

	// TWO DIFFERENT FETCHES, and the split is the whole point of #523. The
	// new-message pass below can meet a UID this account has never ingested at
	// all, so it needs the full message (flags, envelope, internal date, body).
	// The CONDSTORE flag-reconcile pass runs ONLY over UIDs we already believe
	// we hold a live copy of — its range is bounded to 1:lastSeen — so the only
	// thing a MODSEQ bump there can mean is a flag or keyword change on the
	// server. It therefore asks for exactly that (no envelope, no internal
	// date, no body) and hands each result to RefreshFlags, which touches only
	// remote_flags and never opens the blob store or the malware scanner. A UID
	// that turns out to have no live provenance after all (reaped out from
	// under the cursor, or a #93 gap) is not fetched here a second time with a
	// body guess — it is collected and re-fetched, with a body, exactly once,
	// by the fallback below.
	newItems := sync.FetchItems{Flags: true, Envelope: true, InternalDate: true, BodyFull: true}

	// Flag-change reconcile (CONDSTORE), bounded to already-known UIDs so it
	// never overlaps the new-message pass. Skipped when nothing could have
	// changed: no CONDSTORE, no prior MODSEQ baseline, MODSEQ not advanced, or
	// no known messages yet.
	// cursorStatus is the status the cursors are written from. It is status,
	// except that a CONDSTORE pass that met a reply WITHOUT FLAGS (#909) keeps
	// the old HIGHESTMODSEQ, so the next pass's CHANGEDSINCE asks about that
	// message again instead of stepping past it (review L7 of #910).
	cursorStatus := status
	absentBefore := summary.FlagsAbsent
	if condStore && baseModSeq != 0 && status.HighestModSeq > uint64(baseModSeq) && lastSeen > 0 {
		items := sync.FetchItems{Flags: true, ModSeq: true, ChangedSince: uint64(baseModSeq)}
		refreshed, missing, err := e.refreshFlags(ctx, req, sess, stored.ID, status,
			name, sync.UIDSetRange(1, sync.UID(lastSeen)), items, summary)
		if err != nil {
			return mailboxSync{}, err
		}
		summary.FlagUpdates += refreshed
		if len(missing) > 0 {
			// THE BODY FALLBACK, for these UIDs only. sort first: uidSetOf expects
			// an ascending list so it can coalesce consecutive runs into ranges.
			sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
			fb, err := e.fetchAndIngestUIDs(ctx, req, sess, stored.ID, status, name, missing,
				sync.FetchItems{Flags: true, Envelope: true, InternalDate: true, BodyFull: true, ModSeq: true})
			if err != nil {
				// Nothing to record here: every UID in this list is below the
				// cursor already, and the unadvanced MODSEQ baseline lists them
				// again next pass. It resumes by itself — the ones this call did
				// ingest now have provenance, so RefreshFlags finds them and only
				// the rest fall back to a body fetch again.
				return mailboxSync{}, err
			}
			summary.MessagesIngested += fb.created
			summary.MessagesMuted += fb.muted
			summary.MessagesBlocked += fb.blocked
			summary.NotifyIDs = append(summary.NotifyIDs, fb.notify...)
			summary.FlagUpdates += fb.count - fb.created
			summary.BodiesFetched += fb.bodies
			summary.BodyBytesFetched += fb.bodyBytes
			summary.CopiesAwaitingExpunge += fb.awaiting
			summary.FlagsAbsent += len(fb.flagsAbsent)
			e.recordRecovered("flag_fallback", fb)
		}
	}

	// New-message pass: UIDs strictly above the last-seen UID. Skipped when
	// UIDNEXT proves there is nothing new (avoids a pointless open-ended fetch).
	// After a roll this is the FULL re-fetch — lastSeen was rewound to 0, so the
	// range is 1:* — and it is what re-attaches a live provenance row, under the
	// new generation, to every message still in the mailbox.
	//
	// It is issued as a WALK of bounded commands (#656), never as one command
	// over the whole range: see DefaultFetchChunk for the incident. newLastSeen
	// only ever advances past a chunk that COMPLETED, which is what lets a pass
	// that fails part-way record how far it got (recordPartialPoll).
	if summary.FlagsAbsent > absentBefore {
		held := *status
		held.HighestModSeq = uint64(baseModSeq)
		cursorStatus = &held
	}

	newLastSeen := lastSeen
	if status.UIDNext == 0 || sync.UID(lastSeen)+1 < status.UIDNext {
		items := newItems
		items.ModSeq = condStore
		for _, chunk := range newMessageChunks(sync.UID(lastSeen+1), status.UIDNext, e.fetchChunk) {
			b, err := e.fetchAndIngest(ctx, req, sess, stored.ID, status, name, chunk, items)
			// Counted before the error check: whatever a failed chunk did ingest
			// before it died is durably stored, and the summary says so.
			summary.MessagesIngested += b.created
			summary.MessagesMuted += b.muted
			summary.MessagesBlocked += b.blocked
			summary.NotifyIDs = append(summary.NotifyIDs, b.notify...)
			summary.FlagUpdates += b.count - b.created
			summary.BodiesFetched += b.bodies
			summary.BodyBytesFetched += b.bodyBytes
			summary.CopiesAwaitingExpunge += b.awaiting
			summary.FlagsAbsent += len(b.flagsAbsent)
			if err != nil {
				return mailboxSync{}, e.recordPartialPoll(ctx, req, stored.ID, name, cursorStatus, rolled, lastSeen, newLastSeen, err)
			}
			if b.maxUID > newLastSeen {
				newLastSeen = b.maxUID
			}
		}
	}

	// THE CONDSTORE-LESS FLAG SWEEP (ADR-046 §12, flags.go). Without CONDSTORE
	// the flag pass above never runs, and nothing else re-reads an existing
	// message's flags: a message read on a phone would stay unread here. Over
	// UIDs we held before this pass (at most the old cursor), FLAGS only,
	// budgeted, and only a reading that differs from what we recorded is
	// applied.
	//
	// AFTER the new-message walk and NON-FATAL (review M5 of #910): a server
	// that refuses or breaks the sweep's FETCH must never stand between the
	// user and new mail. Its failure is recorded and the mailbox carries on.
	if !rolled && lastSeen > 0 && e.sweepable(sess, condStore) {
		if err := e.sweepFlags(ctx, req, sess, stored.ID, status, name, lastSeen, summary); err != nil {
			summary.addError(name, err)
		}
	}

	cursors := MailboxCursors{
		UIDValidity:   int64(status.UIDValidity),
		LastSeenUID:   newLastSeen,
		HighestModSeq: int64(cursorStatus.HighestModSeq),
		// The rest of the poll-time baseline (#620), from THIS SELECT: the status
		// this poll reconciled against, which the next LIST-STATUS is compared to.
		UIDNext:  int64(status.UIDNext),
		Messages: int64(status.NumMessages),
	}

	// THE ROLL IS NOT DONE YET (#88). The old generation has been retired to
	// 'stale_uidvalidity' and the mailbox re-fetched under the new one, but
	// nothing has yet decided what became of the messages the re-fetch did NOT
	// bring back — and each of those is, right now, in no folder at all, with no
	// change_log row to its name. That decision needs a proof the re-fetch was
	// complete, which needs another question put to the server, which is phase 2's
	// job.
	//
	// So the cursors are WITHHELD. app.mailboxes.uidvalidity keeps naming the OLD
	// generation until the reconcile commits it alongside the reconciliation
	// itself (StoreAdapter.ReconcileRetiredGeneration). That is what makes the roll
	// a durable, retryable unit of work: if we crash here, or the corroboration
	// fails, or the answer cannot be trusted, the next pass sees the same
	// UIDVALIDITY mismatch, re-runs the (idempotent) re-fetch, and tries again —
	// rather than recording "we are on the new generation now" over a roll that was
	// never finished, which would strand the orphans invisibly forever.
	if rolled {
		return mailboxSync{
			uidValidity: int64(status.UIDValidity),
			lastSeenUID: newLastSeen,
			roll: &rolledMailbox{
				name:         name,
				id:           stored.ID,
				uidValidity:  int64(status.UIDValidity),
				refetchBound: int64(status.UIDNext),
				cursors:      cursors,
			},
		}, nil
	}

	if err := e.store.UpdateMailboxCursors(ctx, req.UserID, stored.ID, cursors); err != nil {
		return mailboxSync{}, fmt.Errorf("mailsync: update cursors %q: %w", name, err)
	}
	return mailboxSync{uidValidity: int64(status.UIDValidity), lastSeenUID: newLastSeen}, nil
}

// recordPartialPoll is the new-message pass's failure path (#656). It records
// how far the pass got — the highest UID ingested by a chunk that COMPLETED — so
// the next pass resumes there instead of re-downloading everything before it,
// and returns cause (annotated if the record itself failed). The mailbox's
// failure then flows exactly as before: syncMailbox returns an error, so it is
// not a phase-2 target, is not observed, verified or reaped this pass.
//
// ⚠ IT RECORDS NO POLL BASELINE, AND THAT IS THE INVARIANT. The poll-time
// UIDNEXT/MESSAGES (#620) mean "this folder's poll has fetched everything below
// UIDNEXT"; pollUnchanged skips a folder whose LIST-STATUS still matches them.
// A partly fetched folder must never claim that, or the rest of its backlog is
// stranded invisibly behind a skip. So the write carries UIDNext 0, which the
// store turns into NULL (poll_uidnext = NULLIF($7, 0)) — "no baseline" — and
// every "unknown" in pollUnchanged means "poll it". This does not depend on
// pollOwesNothing (whose cursor-below-UIDNEXT test would also refuse the skip
// today); it removes the baseline outright rather than trusting a second guard.
//
// The rest of the write is what the success path would write for the UIDs
// actually covered, from the same SELECT:
//
//   - UIDVALIDITY: this SELECT's. It equals the stored one, or the stored one
//     was 0 (first sync) — a roll never reaches here, see below.
//   - HIGHESTMODSEQ: this SELECT's. Every UID at or below the new cursor has
//     flags current as of it: those below the old cursor were just reconciled
//     by the CHANGEDSINCE pass (which runs, and succeeded, before this one — the
//     same assumption the success path makes when it writes this modseq),
//     and those in the completed chunks were fetched, flags included, after
//     the SELECT. Keeping the OLD modseq instead would be worse, not safer: on
//     a first sync it is 0, which disables the next pass's flag reconcile, and
//     that pass would then write a newer modseq over flag changes it never read.
//
// Nothing is written when no chunk completed (the cursor has not moved), or when
// the mailbox ROLLED: writing this SELECT's UIDVALIDITY would declare the roll
// finished, which only ReconcileRetiredGeneration may do (#88). A rolled mailbox
// retries its whole re-fetch, which ingest's content-hash dedup makes safe.
func (e *Engine) recordPartialPoll(ctx context.Context, req AccountSync, mailboxID ulid.ULID, name string, status *sync.MailboxStatus, rolled bool, from, reached int64, cause error) error {
	if rolled || reached <= from {
		return cause
	}
	partial := MailboxCursors{
		UIDValidity:   int64(status.UIDValidity),
		LastSeenUID:   reached,
		HighestModSeq: int64(status.HighestModSeq),
		UIDNext:       0, // NO POLL BASELINE — see above
		Messages:      0,
	}
	if err := e.store.UpdateMailboxCursors(ctx, req.UserID, mailboxID, partial); err != nil {
		return fmt.Errorf("%w (and recording partial progress to uid %d in %q failed: %v)", cause, reached, name, err)
	}
	return cause
}

// ---------------------------------------------------------------------------
// Phase 2: the gap backfill (mailroom#93)
// ---------------------------------------------------------------------------
//
// THE BUG. syncMailbox fetches `lastSeen+1:*` and then advances lastSeen to the
// highest UID it INGESTED. A FETCH that silently drops a UID in the middle of that
// range returns no error — an ordinary truncated response does exactly this — so
// the cursor sails past the dropped UID anyway. The next poll asks for
// `cursor+1:*`. The dropped UID is now below the cursor, and NOTHING will ever ask
// for it again: not the poll, whose range starts above it, and not the reaper,
// which only ever DROPS provenance rows and is blind to a message that has none.
// The message sits on the server forever, and Mailroom never learns it exists.
//
// It cannot be detected from the FETCH itself. A hole in a FETCH response is
// completely normal — it is what an expunged UID looks like — so "we asked for
// 5:* and got 5,7,8" is indistinguishable from "6 was deleted". The only thing
// that can tell those apart is a second, INDEPENDENT question to the server, which
// is precisely the proof #88 already built for the UIDVALIDITY roll:
//
//	EVERY UID THE SERVER REPORTS IN THIS MAILBOX, BELOW THE BOUND WE CLAIM TO
//	HAVE COVERED, HAS A LIVE PROVENANCE ROW.
//
// The reaper is already asking that question every reap pass (LiveUIDs — UID
// SEARCH ALL), and throwing half the answer away. This phase reads the other half.

// verifyAccount asks the server for the live UID set of every DUE mailbox and
// proves our provenance complete against it, re-fetching whatever the poll lost.
// It returns the mailboxes whose observation was trustworthy AND whose provenance
// it managed to prove complete — the only ones the reaper is then allowed to touch.
//
// It runs for every due mailbox whether or not the reaper is enabled: the answer
// drives two opposite conclusions and only one of them is destructive
// (WithReapInterval).
//
// A due mailbox whose LIST-STATUS matches its last REAL observation is not asked
// at all (#620, observationUnchanged): its cadence is re-stamped and it is NOT
// returned as verified, so a skip can never lead to a reap.
func (e *Engine) verifyAccount(ctx context.Context, req AccountSync, sess sync.SyncSession, condStore bool, targets []reapTarget, summary *Summary) []verifiedMailbox {
	now := e.now()
	interval := e.observeInterval()
	verified := make([]verifiedMailbox, 0, len(targets))
	for i, t := range targets {
		if !reapDue(t.id, t.lastReaped, now, interval) {
			continue
		}
		if observationUnchanged(condStore, t, now) {
			// NOTHING THIS OBSERVATION COULD FIND (#620). The server's UID set is
			// the one the last real observation proved complete, so the SEARCH
			// would return it again. Re-stamp the cadence and move on — and do
			// nothing else: the mailbox is not "verified" and not reaped, because
			// nothing destructive may key off a question we did not ask.
			if err := e.store.StampMailboxUnchanged(ctx, req.UserID, t.id); err != nil {
				summary.addError(t.name, fmt.Errorf("mailsync: stamp unchanged %q: %w", t.name, err))
				continue
			}
			summary.ObservationsSkipped++
			continue
		}
		v, err := e.verifyMailbox(ctx, req, sess, condStore, t, summary)
		if err != nil {
			// ⚠ THE SERVER SAID "STOP ASKING" — SO STOP ASKING (#638). Migadu
			// allows 60 UID SEARCHes per rolling minute, per account. An account
			// with hundreds of mailboxes walked straight past the refusal into the next SEARCH, and
			// sent hundreds of commands per pass that had already been refused;
			// over hours of use, thousands of them. Continuing cannot
			// help: the budget is spent for the whole window, so every remaining
			// mailbox would be refused too.
			//
			// THE FAIL-CLOSED GUARANTEES ARE EXACTLY AS THEY WERE. A deferred
			// mailbox is in the identical state to one whose observation failed:
			// it is NOT verified (so it never reaches the reaper), NOT reaped, its
			// cadence cursor is NOT stamped, and it is due again next pass. This
			// changes what we SEND, never what we CONCLUDE.
			//
			// It is not recorded in Errors — for this mailbox or for the ones
			// below it. A throttled pass is a paced pass, not a degraded one, and
			// burying the count in 600 identical per-mailbox WARNs is what made a
			// genuinely broken mailbox invisible.
			if errors.Is(err, errAwaitingExpunge) || errors.Is(err, errBudgetSpent) {
				// Unverified and due again, exactly like a failed observation —
				// but the cause is ours and already known, so it is not a WARN.
				continue
			}
			if sync.IsRateLimited(err) {
				summary.MailboxesDeferred += countDue(targets[i:], now, interval)
				summary.RateLimitRetryAfter = sync.RetryAfterOf(err)
				break
			}
			// Either we could not ask, or the answer could not be trusted, or the
			// backfill could not close a gap it found. In all three we have an
			// INCOMPLETE picture of this mailbox — so it is not verified, it is not
			// reaped, its cadence cursor is not stamped, and it is due again next pass.
			summary.addError(t.name, err)
			continue
		}
		verified = append(verified, v)
	}
	return verified
}

// countDue counts how many of targets are due for observation at now — which,
// called with the slice starting at the refused mailbox, is exactly the number
// of mailboxes a pass that stops there leaves unobserved. The refused mailbox
// itself is included: verifyAccount only reaches a target after reapDue said
// yes for it, so it counts here too, and it is genuinely one of the mailboxes
// that got no answer.
//
// Kept separate and pure so "deferred" has a definition an operator can act on:
// a mailbox that was NOT due this pass was never going to be observed, and
// counting it would inflate the figure into noise.
func countDue(targets []reapTarget, now time.Time, interval time.Duration) int {
	n := 0
	for _, t := range targets {
		if reapDue(t.id, t.lastReaped, now, interval) {
			n++
		}
	}
	return n
}

// verifyMailbox reads one mailbox's live UID set, proves our provenance complete
// against it, and re-fetches whatever is missing.
//
// The FETCH is issued immediately after LiveUIDs and for that reason only: LiveUIDs
// EXAMINEs the mailbox, so it is the session's selected mailbox, and FetchByUID
// operates on the selected mailbox (RFC 9051). Nothing may be inserted between
// them.
func (e *Engine) verifyMailbox(ctx context.Context, req AccountSync, sess sync.SyncSession, condStore bool, t reapTarget, summary *Summary) (verifiedMailbox, error) {
	status, uids, err := sess.LiveUIDs(ctx, t.name)
	if err != nil {
		// WE FAILED TO ASK. Nothing is concluded — not that a message is gone, and
		// not that one is missing either. The mailbox is simply due again next pass.
		return verifiedMailbox{}, fmt.Errorf("mailsync: live uids %q: %w", t.name, err)
	}
	// Counted HERE, on the answer and before any trust gate, because it measures
	// what we asked the server, not what we concluded (see MailboxesObserved).
	summary.MailboxesObserved++

	// The SAME trust gates the reaper applies, for the same reasons and from the
	// same answer: a UID set from another generation is not about this mailbox, and
	// an empty UID set from a mailbox the server just said holds messages is a
	// broken answer. An untrustworthy answer must never be read as "these UIDs are
	// missing" any more than as "these UIDs are gone".
	obs, err := reapObservation(t, status, uids)
	if err != nil {
		return verifiedMailbox{}, err
	}
	seen := observedBaseline(status, uids, t.lastSeenUID)

	bound := gapBound(obs, t.lastSeenUID)
	if bound <= 1 {
		// The poll has covered nothing (a mailbox with no cursor yet, or an untouched
		// UID space), so no UID can be below the cursor and no gap can exist. The
		// answer was fine; there was just nothing to prove.
		summary.MailboxesVerified++
		return verifiedMailbox{target: t, obs: obs, seen: seen}, nil
	}

	gaps, err := e.store.MissingUIDs(ctx, req.UserID, store.GapObservation{
		MailboxID:    t.id,
		UIDValidity:  t.uidValidity,
		ObservedUIDs: gapFloorFilter(obs.ObservedUIDs, t.gapFloor),
		UIDBound:     bound,
		Limit:        maxGapBackfill,
	})
	if err != nil {
		return verifiedMailbox{}, fmt.Errorf("mailsync: detect gaps %q: %w", t.name, err)
	}
	if len(gaps) == 0 {
		// THE PROOF HELD: every UID the server reports below our cursor has a live
		// location. The poll lost nothing.
		summary.MailboxesVerified++
		return verifiedMailbox{target: t, obs: obs, seen: seen}, nil
	}

	// A GAP. The server holds this UID; we hold no location for it; our cursor is
	// already past it. This says NOTHING about anything being deleted — it is
	// evidence that OUR FETCH was incomplete, and the only correct response is to
	// go and get the message. Re-fetching a UID we turn out to have anyway is a
	// harmless no-op (ingest dedups on content hash, ADR D1).
	summary.GapsDetected += len(gaps)
	var (
		b         ingestBatch
		ferr      error
		gapsShort bool // Gmail: the budget did not cover every gap
	)
	if t.gmail != nil {
		// Gmail: the refetch carries the labels and is SIZED BEFORE IT IS
		// FETCHED, exactly like new mail (#52 review M1): only as many gaps
		// as the budget covers are downloaded, every byte is metered, and
		// what is left waits for budget. A mailbox with a gap left over stays
		// unverified, unreaped and due, like any incomplete view.
		var cands []sized
		if cands, ferr = fetchSizes(ctx, sess, t.name, uidSetOf(gaps)); ferr == nil {
			take := t.gmail.takeWithin(cands, t.gmail.remaining(false))
			gapsShort = len(take) < len(cands)
			b, ferr = e.fetchBudgeted(ctx, req, sess, t.gmail, t.id, status, t.name, take, gmailItems(condStore))
		}
		summary.GmailLabelChanges += b.labelChanges
	} else {
		b, ferr = e.fetchAndIngestUIDs(ctx, req, sess, t.id, status, t.name, gaps,
			sync.FetchItems{Flags: true, Envelope: true, InternalDate: true, BodyFull: true, ModSeq: condStore})
	}
	summary.MessagesIngested += b.created
	summary.MessagesMuted += b.muted
	summary.MessagesBlocked += b.blocked
	summary.NotifyIDs = append(summary.NotifyIDs, b.notify...)
	summary.FlagUpdates += b.count - b.created
	summary.GapsBackfilled += b.count
	summary.BodiesFetched += b.bodies
	summary.BodyBytesFetched += b.bodyBytes
	summary.CopiesAwaitingExpunge += b.awaiting
	summary.FlagsAbsent += len(b.flagsAbsent)
	e.recordRecovered("gap", b)
	if ferr != nil {
		return verifiedMailbox{}, fmt.Errorf("mailsync: backfill %q: %w", t.name, ferr)
	}
	if gapsShort {
		return verifiedMailbox{}, fmt.Errorf("mailsync: %q: %d gap(s), only %d refetched within the download budget: %w",
			t.name, len(gaps), b.count, errBudgetSpent)
	}

	// The cursor is NOT advanced by any of this: every UID here is below it by
	// construction. What the backfill changes is provenance, and nothing else.

	if b.count+b.awaiting < len(gaps) || len(gaps) == maxGapBackfill {
		// Either the FETCH did not return everything we asked for — which is benign
		// if the UID was expunged between the SEARCH and the FETCH, and is the #93
		// fault itself if it was not — or the batch was full and there is more. Both
		// mean the same thing here: we still do not hold everything the server says
		// it has. So the mailbox is NOT verified, is NOT reaped (an incomplete view of
		// a mailbox is no licence to delete rows from it), and is NOT stamped — which
		// leaves it due on the very next pass rather than in an hour. That is what
		// makes recovery poll-speed once a gap is known, and what makes a persistently
		// lossy server loudly, repeatedly visible instead of quietly lossy.
		return verifiedMailbox{}, fmt.Errorf(
			"mailsync: %q: the server holds %d UID(s) below our own last-seen cursor that we never ingested, "+
				"and re-fetching them brought back %d; our view of this mailbox is INCOMPLETE, so nothing is "+
				"reaped from it and it is re-checked next pass",
			t.name, len(gaps), b.count)
	}

	if b.awaiting > 0 {
		// EVERY GAP IS ACCOUNTED FOR, BUT NOT EVERY ONE IS HELD (#686). The ones we
		// did not record are copies a queued draft EXPUNGE is about to remove, so
		// this is not the #93 fault above and is not reported as one — but our
		// provenance still does not match the server's UID set, so the mailbox is
		// NOT verified: not reaped, not stamped, due again next pass. By then the
		// expunge has run (the gap is gone) or has given up (and the copy is
		// ingested, because a dead expunge no longer defers it).
		return verifiedMailbox{}, errAwaitingExpunge
	}

	summary.MailboxesVerified++
	return verifiedMailbox{target: t, obs: obs, seen: seen}, nil
}

// errNonMailFolder is syncMailbox's "this was a profile non-mail folder, now
// set not synced" (ADR-046 §12): not a failure.
var errNonMailFolder = errors.New("mailsync: non-mail folder set not synced")

// errAwaitingExpunge is verifyMailbox's "complete except for copies a queued
// draft expunge is about to remove" (#686). verifyAccount treats it as an
// unverified mailbox that is due again, and records no error for it.
var errAwaitingExpunge = errors.New("mailsync: mailbox holds copies a queued draft expunge is about to remove")

// gapBound is the exclusive top of the UID space a gap can live in:
// min(what the server's observation covers, what our poll claims to have covered).
//
// The second half is the one that defines a gap. A UID at or above the last-seen
// cursor is not lost — the next poll's `lastSeen+1:*` range asks for it — so its
// absence from our provenance proves nothing and re-fetching it would duplicate
// work the poll is about to do anyway. A UID BELOW the cursor is one the poll will
// never ask for again. Only those are #93.
func gapBound(obs *store.ReapObservation, lastSeenUID int64) int64 {
	if obs == nil || lastSeenUID <= 0 {
		return 0
	}
	if lastSeenUID+1 < obs.UIDBound {
		return lastSeenUID + 1
	}
	return obs.UIDBound
}

// ---------------------------------------------------------------------------
// Skipping what did not change (mailroom#620)
// ---------------------------------------------------------------------------
//
// An account with hundreds of folders SELECTed every folder every pass and UID SEARCHed most of
// them an hour, overwhelmingly folders in which nothing had happened. LIST-STATUS
// hands us each folder's (UIDVALIDITY, UIDNEXT, MESSAGES, HIGHESTMODSEQ) in the
// one LIST the pass already sends. Two skips are decided from it, against TWO
// DIFFERENT baselines, and confusing them is how mail gets lost:
//
//   - the POLL is skipped when the tuple equals what the last POLL's SELECT saw;
//   - the OBSERVATION is skipped when it equals what the last OBSERVATION saw.
//
// Both predicates fail safe: every "unknown" — no CONDSTORE, no status from the
// server, a baseline never recorded — means "do the work", which is exactly the
// behaviour before #620.

// maxObservationSkipAge bounds how long a folder that keeps reading as unchanged
// may go without a REAL observation. It is a backstop, not the mechanism: it
// catches anything the tuple comparison cannot see (a server whose STATUS is
// wrong, provenance lost on our side without the server changing), at the cost
// of one SEARCH per folder per day.
const maxObservationSkipAge = 24 * time.Hour

// pollUnchanged reports whether phase 1 may skip SELECTing a folder: whether the
// SELECT would provably lead to no FETCH at all.
//
// The four-number comparison is #620's design. The final test — the poll OWES
// NOTHING (pollOwesNothing) — is an addition to that design: it is what makes
// "no FETCH at all" true rather than merely likely.
func pollUnchanged(condStore bool, listed *sync.ListStatus, stored StoredMailbox) bool {
	if !condStore || listed == nil || listed.HighestModSeq == 0 {
		return false
	}
	if stored.PollUIDNext == nil || stored.PollMessages == nil || stored.UIDValidity == 0 {
		return false // no poll-time baseline yet (first pass after migration 00058)
	}
	if int64(listed.UIDValidity) != stored.UIDValidity ||
		int64(listed.UIDNext) != *stored.PollUIDNext ||
		int64(listed.NumMessages) != *stored.PollMessages ||
		int64(listed.HighestModSeq) != stored.HighestModSeq {
		return false
	}
	return pollOwesNothing(listed, stored)
}

// pollOwesNothing reports whether the server can hold a message in the range the
// poll's new-message pass would fetch, [lastSeen+1, UIDNEXT). If it can, the poll
// is NOT skipped, however unchanged the tuple is.
//
// That range can be non-empty under an UNCHANGED tuple: a FETCH that silently
// dropped the TOP of its range (#93) leaves the cursor below the dropped UID, and
// today's poll re-asks for it on the next pass — the one recovery that happens at
// poll speed. The observation cannot stand in for it, because gapBound leaves
// every UID at or above lastSeen+1 to the poll: a skipped poll there would lose
// the message until the folder next changed.
//
// The range holds no message when any one of these is true:
//
//   - it is empty (lastSeen+1 >= UIDNEXT);
//   - the folder holds no message at all (MESSAGES 0) — the folder emptied before
//     our first sync, whose cursor stays at 0 below a large UIDNEXT forever;
//   - the listed tuple equals the last observation's baseline. That baseline is
//     only recorded when every UID the observation saw was at or below the cursor
//     (observedBaseline), and an equal tuple means the server's UID set has not
//     changed since (UIDNEXT: nothing added; MESSAGES: so nothing removed). This
//     is the folder whose newest message was expunged before we fetched it.
//
// Only a folder that is none of these — a real message above the cursor, or one
// not yet observed since — pays today's price: a SELECT and a FETCH every pass.
func pollOwesNothing(listed *sync.ListStatus, stored StoredMailbox) bool {
	switch {
	case stored.LastSeenUID+1 >= *stored.PollUIDNext:
		return true
	case listed.NumMessages == 0:
		return true
	default:
		return stored.Observed != nil && sameStatus(listed, *stored.Observed)
	}
}

// sameStatus reports whether a listed status equals a recorded baseline in all
// four numbers.
func sameStatus(l *sync.ListStatus, o store.ObservedStatus) bool {
	return int64(l.UIDValidity) == o.UIDValidity &&
		int64(l.UIDNext) == o.UIDNext &&
		int64(l.NumMessages) == o.Messages &&
		int64(l.HighestModSeq) == o.HighestModSeq
}

// observationUnchanged reports whether phase 2 may skip a DUE folder's live-UID
// observation and just re-stamp its cadence.
//
// ⚠ THE BASELINE IS THE LAST OBSERVATION, NOT THE LAST POLL. A poll whose FETCH
// silently lost a message still advances the poll cursors (that is #93), and the
// observation is the only thing that finds the loss. Comparing LIST-STATUS
// against the poll would say "unchanged since the poll" about precisely the
// folder that must be observed. Against the observation it says what the skip
// needs: the server's UID set is the one we last PROVED we hold in full
// (observedBaseline only records a baseline when the proof covered it all).
//
// And never for longer than maxObservationSkipAge since the last REAL one.
func observationUnchanged(condStore bool, t reapTarget, now time.Time) bool {
	if !condStore || t.listed == nil || t.listed.HighestModSeq == 0 {
		return false
	}
	o := t.observed
	if o == nil || t.lastFullObs == nil || !now.Before(t.lastFullObs.Add(maxObservationSkipAge)) {
		return false
	}
	return o.UIDValidity == t.uidValidity && sameStatus(t.listed, *o)
}

// observedBaseline is the observation-time tuple a trustworthy observation
// records (#620), or nil when it must record none.
//
// The observation proves our provenance complete only BELOW gapBound — below
// min(UIDNEXT, lastSeen+1). A UID at or above lastSeen+1 is left to the poll. If
// the observation saw any such UID (mail that arrived between this pass's poll
// and its observation), then "the server still looks like this" does NOT mean
// "we hold everything the server holds": the poll that fetches that UID next
// pass may silently drop it and move its cursor past it, without changing the
// tuple. So a baseline is recorded only when every UID the server reported is at
// or below the cursor (the proof covered all of them); otherwise nil, and the
// next due observation runs for real. This condition is an addition to #620's
// posted design.
//
// An empty UID set is covered trivially: the empty folder with a large UIDNEXT
// gets a baseline, which is what lets both of its skips fire.
func observedBaseline(status *sync.MailboxStatus, uids []sync.UID, lastSeenUID int64) *store.ObservedStatus {
	if status == nil || status.UIDNext == 0 {
		return nil // no UIDNEXT: the tuple could never match a LIST-STATUS anyway
	}
	if int64(maxUID(uids)) > lastSeenUID {
		return nil
	}
	return &store.ObservedStatus{
		UIDValidity:   int64(status.UIDValidity),
		UIDNext:       int64(status.UIDNext),
		Messages:      int64(status.NumMessages),
		HighestModSeq: int64(status.HighestModSeq),
	}
}

// newMessageChunks splits the new-message range [from, *) into FETCH commands
// that each name at most n UIDs (#656), in ascending order.
//
// When the SELECT reported UIDNEXT, [from, uidNext-1] is walked in CLOSED ranges
// of n, and the FINAL chunk is left OPEN-ENDED (`start:*`) — so mail that
// arrives while the pass runs is still caught exactly as the single `from:*`
// fetch caught it. Its start is within n of UIDNEXT, so it names at most n UIDs
// that existed at the SELECT.
//
// Without UIDNEXT there is nothing to walk towards, and the result is today's
// single open-ended command. A range that is already empty (from >= uidNext)
// also yields one open-ended command rather than none; the caller never asks
// for one (it skips the pass when UIDNEXT proves nothing is new).
func newMessageChunks(from, uidNext sync.UID, n int) []sync.UIDSet {
	if uidNext == 0 || n <= 0 {
		return []sync.UIDSet{sync.UIDSetRange(from, 0)}
	}
	step := sync.UID(n)
	var out []sync.UIDSet
	start := from
	// More than n UIDs remain in [start, uidNext): emit a closed chunk. The
	// start < uidNext test guards the unsigned subtraction.
	for start < uidNext && uidNext-start > step {
		out = append(out, sync.UIDSetRange(start, start+step-1))
		start += step
	}
	return append(out, sync.UIDSetRange(start, 0))
}

// uidChunks splits an explicit list of UIDs into sets of at most n UIDs each
// (#656), ascending, each coalesced by uidSetOf. The list is sorted on a copy,
// so the caller's order does not matter.
func uidChunks(uids []int64, n int) []sync.UIDSet {
	if n <= 0 {
		n = DefaultFetchChunk
	}
	sorted := append([]int64(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := make([]sync.UIDSet, 0, (len(sorted)+n-1)/n)
	for i := 0; i < len(sorted); i += n {
		end := i + n
		if end > len(sorted) {
			end = len(sorted)
		}
		out = append(out, uidSetOf(sorted[i:end]))
	}
	return out
}

// uidSetOf turns an ASCENDING list of UIDs into a UIDSet, coalescing consecutive
// runs into ranges so a contiguous chunk a truncated response dropped costs one
// range rather than one per message.
func uidSetOf(uids []int64) sync.UIDSet {
	set := make(sync.UIDSet, 0, len(uids))
	for i := 0; i < len(uids); {
		j := i
		for j+1 < len(uids) && uids[j+1] == uids[j]+1 {
			j++
		}
		set = append(set, sync.UIDRange{Start: sync.UID(uids[i]), Stop: sync.UID(uids[j])})
		i = j + 1
	}
	return set
}

// reconcileRolled finishes every UIDVALIDITY roll this pass detected (#88).
//
// It runs AFTER every mailbox has been polled, for the same reason the reaper
// does: a message that another client moved out of the rolled mailbox and into
// another one must be found in its new home before we are allowed to conclude it
// is in no home at all.
//
// A roll it cannot finish is recorded as a per-mailbox error and left pending —
// cursors unadvanced, retired rows still retired, nothing soft-deleted — so the
// next pass tries again. Nothing here is on the reaper's cadence, and nothing
// here is disabled by WithReapInterval(0): that switch trades "a folder keeps
// showing mail that has been deleted elsewhere" for "nothing ever deletes a
// provenance row", which is a coherent choice about STALE membership. It has no
// bearing on a roll, where the alternative to reconciling is not stale membership
// but a message that is in no folder at all and that nobody was ever told about.
func (e *Engine) reconcileRolled(ctx context.Context, req AccountSync, sess sync.SyncSession, rolled []rolledMailbox, summary *Summary) {
	for _, r := range rolled {
		if err := e.reconcileGeneration(ctx, req, sess, r, summary); err != nil {
			summary.addError(r.name, err)
		}
	}
}

// reconcileGeneration corroborates one rolled mailbox's re-fetch against the
// server and, if the proof holds, reconciles the retired generation.
func (e *Engine) reconcileGeneration(ctx context.Context, req AccountSync, sess sync.SyncSession, r rolledMailbox, summary *Summary) error {
	status, uids, err := sess.LiveUIDs(ctx, r.name)
	if err != nil {
		// WE FAILED TO ASK. The re-fetch may have been perfect or may have been
		// truncated; we cannot tell, so we conclude nothing, delete nothing, and
		// leave the cursors on the old generation. The roll is still pending.
		return fmt.Errorf("mailsync: live uids %q (post-roll corroboration): %w", r.name, err)
	}

	obs, err := generationObservation(r, status, uids)
	if err != nil {
		return err
	}

	res, err := e.store.ReconcileRetiredGeneration(ctx, req.UserID, *obs, r.cursors)
	if err != nil {
		return fmt.Errorf("mailsync: reconcile retired generation %q: %w", r.name, err)
	}
	summary.GenerationsReconciled++
	summary.ProvenanceRetired += res.ProvenanceDropped
	summary.MessagesVanished += res.MessagesVanished
	return nil
}

// generationObservation turns one post-roll LiveUIDs answer into a
// store.GenerationObservation, or REFUSES to. It is where every "do not destroy
// the user's mail" rule that can be decided from the server's answer alone is
// enforced for the roll path, kept pure so each rule is directly testable — the
// exact counterpart of reapObservation.
func generationObservation(r rolledMailbox, status *sync.MailboxStatus, uids []sync.UID) (*store.GenerationObservation, error) {
	if status == nil {
		return nil, fmt.Errorf("mailsync: reconcile %q: server returned no mailbox status", r.name)
	}

	// UIDVALIDITY, AGAIN. The mailbox rolled once; it can roll twice. If the
	// generation moved between the re-fetch and this corroboration, then the live
	// rows the re-fetch just wrote are themselves in a dead coordinate space and
	// this UID set says nothing about them. Refuse — and because the cursors are
	// still on the ORIGINAL generation, the next pass simply sees a roll again and
	// re-fetches against whatever the mailbox is now.
	if int64(status.UIDValidity) != r.uidValidity {
		return nil, fmt.Errorf(
			"mailsync: reconcile %q: UIDVALIDITY moved again (re-fetched %d, now %d); refusing to reconcile a retired generation against a coordinate space that has already moved on",
			r.name, r.uidValidity, status.UIDValidity)
	}

	// THE EMPTY-ANSWER GUARD (reapObservation's, for the same reason). An empty
	// UID set from a mailbox whose own SELECT just said it holds messages is a
	// broken answer, not an emptied mailbox — and here, acting on it would soft-
	// delete every message the folder ever had.
	if len(uids) == 0 && status.NumMessages > 0 {
		return nil, fmt.Errorf(
			"mailsync: reconcile %q: server reported %d message(s) but returned an empty UID set; refusing to treat that as a mailbox recreated empty",
			r.name, status.NumMessages)
	}

	// THE BOUND THE COMPLETENESS PROOF IS MEASURED AGAINST — see
	// rolledMailbox.refetchBound and store.GenerationObservation.UIDBound.
	bound := r.refetchBound
	switch {
	case bound > 0:
		// The normal path: the re-fetch's SELECT carried UIDNEXT, as RFC 9051 says
		// it SHOULD and as every server we care about does.
	case status.NumMessages == 0:
		// No UIDNEXT from the re-fetch's SELECT, and the mailbox now reports empty.
		// Taken at face value that says "the mailbox was recreated with nothing in
		// it, soft-delete everything it used to hold" — the single most destructive
		// thing this code can do — on the strength of two zeroes that are exactly
		// what a truncated or unparsed response also produces. Refuse, as the reaper
		// refuses the same shape. The cost is that on a (non-conformant) server that
		// omits UIDNEXT, a mailbox recreated empty stays unreconciled and its old
		// messages stay invisible — the #88 bug, unfixed, for that server. The cost
		// of the other choice is destroying mail that is still there. They are not
		// comparable, and the error is surfaced rather than swallowed.
		return nil, fmt.Errorf(
			"mailsync: reconcile %q: the re-fetch SELECT carried no UIDNEXT and the server now reports an empty mailbox; "+
				"refusing to conclude an entire retired generation is gone on an uncorroborated EXISTS 0", r.name)
	default:
		// No UIDNEXT, but the server did name UIDs. Bound the proof by the highest
		// one it named: everything at or below it must then be accounted for, which
		// is the STRICTEST reading available and so the safe one — a re-fetch that
		// dropped anything the server still lists fails the proof.
		bound = int64(maxUID(uids)) + 1
	}

	return &store.GenerationObservation{
		MailboxID:    r.id,
		UIDValidity:  r.uidValidity,
		ObservedUIDs: uidInts(uids),
		UIDBound:     bound,
	}, nil
}

// reapAccount runs the EXPUNGE REAPER over the mailboxes phase 2 VERIFIED, using
// the live UID set phase 2 already read — the pass asks the server for a mailbox's
// UID set exactly once, and draws both conclusions from it.
//
// This is the answer to the correctness hole CONDSTORE leaves: the incremental
// poll can see new messages and changed flags, but nothing in it can ever see a
// message LEAVE. (QRESYNC's VANISHED would, but go-imap v2 beta.8 cannot drive a
// QRESYNC exchange at all — internal/sync/NOTES.md.) So we ask the server for the
// truth: the whole live UID set of the mailbox, diffed against what we hold.
//
// It reaps NOTHING unless the server answered. Every gate is fail-closed:
//
//   - a mailbox whose POLL failed never reaches here (it is not a target);
//   - a mailbox whose UIDVALIDITY ROLLED this pass never reaches here either: it
//     is phase 3's, and every live row it holds was written by the re-fetch that
//     just ran, so there is nothing surplus for a reap to find;
//   - a dial or LIST failure means SyncAccount returned before this ran at all;
//   - a failed LiveUIDs (network error, timeout, auth loss, protocol error) fails
//     the VERIFICATION, so the mailbox never becomes a reap candidate — "we failed
//     to ask" is not "the server said it's gone";
//   - an answer that cannot be trusted (wrong generation, or an empty UID set from
//     a mailbox the server just said holds messages) is REFUSED there, not acted
//     on here (reapObservation);
//   - a mailbox with an OPEN GAP — one we know the server holds a message we do
//     not — is not verified either, because an incomplete view of a mailbox is not
//     a licence to delete rows from it (#93);
//   - the cadence cursor is stamped only inside the transaction that actually
//     reaped, so a refusal or failure simply leaves the mailbox due next pass.
//
// When the reaper is DISABLED (WithReapInterval(0)) the verification still ran, so
// the cadence cursor still has to move; it is stamped on its own. The observation
// happened and was acted on — just not destructively.
func (e *Engine) reapAccount(ctx context.Context, req AccountSync, verified []verifiedMailbox, summary *Summary) {
	for _, v := range verified {
		// obs == nil: the answer was trustworthy but could prove nothing absent (an
		// untouched UID space). Nothing to reap — but the mailbox WAS observed, so its
		// cadence cursor moves; otherwise an empty folder would re-issue a UID SEARCH
		// ALL on every single poll, forever.
		if !e.reaping() || v.obs == nil {
			if err := e.store.StampMailboxObserved(ctx, req.UserID, v.target.id, v.seen); err != nil {
				summary.addError(v.target.name, fmt.Errorf("mailsync: stamp observed %q: %w", v.target.name, err))
			}
			continue
		}
		res, err := e.store.ReapMailbox(ctx, req.UserID, *v.obs, v.seen)
		if err != nil {
			summary.addError(v.target.name, fmt.Errorf("mailsync: reap %q: %w", v.target.name, err))
			continue
		}
		summary.MailboxesReaped++
		summary.ProvenanceReaped += res.ProvenanceDropped
		summary.MessagesVanished += res.MessagesVanished
	}
}

// reconcileDeletedMailboxes reconciles every folder we hold that the server's
// LIST no longer reports (#87): its messages lose that membership, a Mailbox
// `destroyed` and the Email `updated` rows are emitted, and the folder is torn
// down. It is the folder-level counterpart of the expunge reaper.
//
// It is DESTRUCTIVE, so it obeys the same two safety rules the reaper does.
//
// FAIL CLOSED on the evidence. Only a SUCCESSFUL LIST is evidence a folder was
// deleted — a LIST error is already fatal (SyncAccount returned before this ran).
// And a successful LIST is trusted only when it names INBOX: INBOX is the one
// mailbox RFC 9051 guarantees always exists and cannot be deleted, so a LIST that
// omits it is a broken or partial answer, not "every folder was deleted", and this
// refuses to conclude ANY deletion from it. That guard is the folder-level twin of
// the reaper's "empty UID set from a non-empty mailbox is a broken answer": in both
// cases the destructive reading of a malformed response is refused outright, and
// the residual (a genuinely partial LIST that still names INBOX) is self-healing —
// a folder wrongly reconciled reappears next pass, is re-ingested, and its
// soft-deletes are retracted (store.UpsertProvenance -> RetractVanished), exactly
// as a wrongly-reaped message is.
//
// DISABLED BY WithReapInterval(0), like the reaper and for the same reason: it
// deletes provenance rows, and that switch is the operator's "keep stale folder
// membership rather than let anything delete a provenance row". A deleted folder
// left showing its old mail IS stale membership — the reaper's category, not the
// UIDVALIDITY roll's (which is never disabled because its alternative is a message
// in no folder at all that no client was told about). It needs no cadence: the
// evidence is the LIST we already hold, so it is free to run every pass.
func (e *Engine) reconcileDeletedMailboxes(ctx context.Context, req AccountSync, listed []sync.Mailbox, summary *Summary) {
	if !e.reaping() {
		return
	}

	present := make(map[string]struct{}, len(listed))
	haveInbox := false
	for _, mb := range listed {
		present[mb.Name] = struct{}{}
		if strings.EqualFold(mb.Name, "INBOX") {
			haveInbox = true
		}
	}
	if !haveInbox {
		// A successful LIST that does not even name INBOX is not trustworthy. Refuse
		// to treat anything as deleted from it — "we got a broken answer" is not "the
		// user deleted their folders".
		summary.addError("", errors.New(
			"mailsync: LIST returned no INBOX; refusing to treat any folder as deleted on an untrustworthy mailbox list"))
		return
	}

	stored, err := e.store.ListAccountMailboxes(ctx, req.UserID, req.AccountID)
	if err != nil {
		summary.addError("", fmt.Errorf("mailsync: list stored mailboxes: %w", err))
		return
	}
	for _, sm := range stored {
		if _, ok := present[sm.IMAPName]; ok {
			continue // still on the server
		}
		res, err := e.store.ReconcileDeletedMailbox(ctx, req.UserID, sm.ID)
		if err != nil {
			summary.addError(sm.IMAPName, fmt.Errorf("mailsync: reconcile deleted mailbox: %w", err))
			continue
		}
		summary.MailboxesDeleted++
		summary.ProvenanceReaped += res.ProvenanceDropped
		summary.MessagesVanished += res.MessagesVanished
	}
}

// reapDue reports whether a mailbox's observation cadence has elapsed. A mailbox
// that has never been observed is always due.
//
// The due time is interval PLUS a per-mailbox offset derived from id (see
// reapJitter), so two mailboxes observed in the same pass do not come due in the
// same pass an hour later.
func reapDue(id ulid.ULID, last *time.Time, now time.Time, interval time.Duration) bool {
	return last == nil || !now.Before(last.Add(interval+reapJitter(id, interval)))
}

// reapJitter is a mailbox's fixed offset into its observation cadence: a
// deterministic value in [0, interval/5] — up to 20% — derived from its id.
//
// ⚠ COHORTS RE-FORM WITHOUT IT (#638). Every mailbox observed in one pass is
// stamped within seconds of every other, so with a flat interval they all come
// due again in the same pass, an hour later, and that pass tries to issue the
// whole cohort's UID SEARCHes at once. Measured in production: seven hours after
// the first-sync backlog cleared, 55 of 90 passes still had SEARCHes refused, in
// hourly waves of up to 215. Hashing the id spreads a cohort's next due times
// across a fifth of the interval, permanently, and the spread is inherited by
// whatever pass each mailbox lands in next.
//
// DETERMINISTIC, not random, and derived from data we already hold: the same
// mailbox gets the same offset on every process, forever, with no stored state,
// no migration, and no new column. Randomness would re-roll each pass and
// smear a mailbox's cadence instead of phasing it.
//
// It only ever DELAYS, and by at most 20%, so the cadence stays inside its
// service level and an overdue mailbox stays overdue.
func reapJitter(id ulid.ULID, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(id[:]) // hash.Hash.Write never returns an error
	return time.Duration(h.Sum64() % uint64(interval/5+1))
}

// reapObservation turns one LiveUIDs answer into a store.ReapObservation, or
// REFUSES to. It is where every "do not delete the user's mail" rule that can be
// decided from the server's answer alone is enforced, kept pure so each rule is
// directly testable.
//
// It is the trust gate for BOTH conclusions drawn from that one answer — the reap
// (#86) and the gap backfill (#93) — and deliberately so. An answer that cannot be
// trusted cannot be read as "these UIDs are gone" and cannot be read as "these UIDs
// are missing" either: a UID set from the wrong generation is not about this
// mailbox at all, and an empty UID set from a mailbox the server just said holds
// messages is not a statement about anything. One refusal, both directions.
//
// A nil observation with a nil error means "the answer was fine, but there is
// nothing it could prove absent" (an untouched, empty UID space). An error means
// the answer cannot be trusted and the mailbox must be left exactly as it is.
func reapObservation(t reapTarget, status *sync.MailboxStatus, uids []sync.UID) (*store.ReapObservation, error) {
	if status == nil {
		return nil, fmt.Errorf("mailsync: reap %q: server returned no mailbox status", t.name)
	}

	// UIDVALIDITY. A UID is only meaningful inside the generation that issued it.
	// If the generation moved between this pass's poll and this reap, our
	// provenance rows are keyed by a coordinate space that no longer exists and
	// the diff would be pure nonsense — every UID would look "gone". Refuse; the
	// next pass's poll will see the reset, retire the generation properly
	// (MarkMailboxProvenanceStale), and re-fetch.
	if int64(status.UIDValidity) != t.uidValidity {
		return nil, fmt.Errorf(
			"mailsync: reap %q: UIDVALIDITY moved under us (synced %d, now %d); refusing to reap against a stale coordinate space",
			t.name, t.uidValidity, status.UIDValidity)
	}

	// THE EMPTY-ANSWER GUARD. An empty UID set is a legitimate answer only from a
	// mailbox that is actually empty. If the server's own SELECT just said the
	// mailbox holds messages and the SEARCH then returned none, the answer is
	// broken (a truncated response, a library type-confusion, a server bug) — and
	// acting on it would delete every message in the folder. Refuse. If the
	// mailbox really was emptied, the next pass sees EXISTS 0 and reaps it then.
	if len(uids) == 0 && status.NumMessages > 0 {
		return nil, fmt.Errorf(
			"mailsync: reap %q: server reported %d message(s) but returned an empty UID set; refusing to treat that as an emptied mailbox",
			t.name, status.NumMessages)
	}

	// THE RACE BOUND. UIDNEXT is the exclusive top of the UID space the server
	// just answered about: anything at or above it did not exist when it answered.
	// Bounding the reap by it is what makes a message ingested concurrently with
	// (or just after) the search un-reapable — it lives above the bound, outside
	// the observation, so its absence from the UID set proves nothing about it.
	bound := int64(status.UIDNext)
	switch {
	case bound > 0:
		// The normal path: RFC 9051 says SELECT SHOULD carry UIDNEXT, and every
		// server we care about does.
	case status.NumMessages == 0:
		// No UIDNEXT, and EXISTS says the mailbox is empty. Taking that at face
		// value would mean reaping EVERY provenance row in the folder — the single
		// most destructive thing this code can do — on the strength of one
		// unsupported zero.
		//
		// Both of the fields that would have to be wrong for that to be a mistake
		// are already zero here: no UIDNEXT was parsed, and NumMessages is the
		// zero value whether the server said "EXISTS 0" or the response was
		// malformed and nothing was parsed at all. A truncated SELECT plus a
		// truncated SEARCH is indistinguishable, at this point, from a genuinely
		// emptied mailbox — and one of those two readings destroys the folder.
		//
		// So refuse. The cost is that on a (non-conformant) server that omits
		// UIDNEXT, a mailbox the user really did empty never reaps and its
		// provenance lingers — stale rows, which is a visible annoyance. The
		// alternative cost is deleting mail that still exists. Those are not
		// comparable, and this is the "fail closed" the rest of the reaper already
		// commits to.
		return nil, fmt.Errorf(
			"mailsync: reap %q: server reported an empty mailbox but sent no UIDNEXT; "+
				"refusing to reap the whole folder on an uncorroborated EXISTS 0", t.name)
	default:
		// No UIDNEXT and a non-empty mailbox: fall back to the highest UID the
		// server actually named. SEARCH ALL is complete, so everything at or below
		// it is covered; everything above it is left alone. Conservative and
		// converging.
		bound = int64(maxUID(uids)) + 1
	}

	obs := &store.ReapObservation{
		MailboxID:    t.id,
		UIDValidity:  t.uidValidity,
		ObservedUIDs: uidInts(uids),
		UIDBound:     bound,
	}
	if obs.UIDBound <= 1 {
		return nil, nil // UIDs start at 1: nothing below the bound can exist
	}
	return obs, nil
}

// maxUID returns the highest UID in uids, or 0 when empty.
func maxUID(uids []sync.UID) sync.UID {
	var max sync.UID
	for _, u := range uids {
		if u > max {
			max = u
		}
	}
	return max
}

// uidInts converts fetched UIDs to the int64 form the store's bigint[] anti-join
// expects. It returns a non-nil empty slice for an empty set, so "the server said
// the mailbox is empty" reaches the store as an empty array rather than a NULL.
func uidInts(uids []sync.UID) []int64 {
	out := make([]int64, len(uids))
	for i, u := range uids {
		out[i] = int64(u)
	}
	return out
}

// ingestBatch summarizes one fetch+ingest pass over a UID set.
type ingestBatch struct {
	count   int // messages ingested
	created int // new canonical messages (Result.Created)
	muted   int // of those, arrivals a mute diverted (Result.Muted)
	blocked int // of those, arrivals from a blocked sender (Result.Blocked)
	// notify are the created messages that were neither muted nor blocked —
	// the ones a push may be about (Summary.NotifyIDs).
	notify []ulid.ULID
	maxUID int64 // highest UID ingested, 0 if none
	// bodies and bodyBytes count the message bodies this batch actually
	// downloaded, and their total size (#523) — every fetchAndIngest call
	// requests BodyFull, so this is exactly len(ingestBatch messages).
	bodies    int
	bodyBytes int64
	// awaiting counts fetched copies ingest deliberately did NOT record because
	// a queued draft EXPUNGE is about to remove them (#686). They are not in
	// count: nothing was ingested for them.
	awaiting int
	// labelChanges counts what Gmail label readings changed (#52): labels
	// added or removed, plus folder placements moved. Zero off Gmail.
	labelChanges int
	// seen are the UIDs whose bodies reached the callback (#52 review H2):
	// on a failed command, the Gmail path charges the known size of every
	// requested UID NOT in here, which the drain downloaded unseen.
	seen []int64
	// flagsAbsent are the UIDs whose response carried a body but no FLAGS
	// (#909); fetchAndIngest re-reads their flags after the command.
	flagsAbsent []int64
}

// fetchAndIngest fetches the requested UIDs and ingests each message, attaching
// provenance (account/mailbox/uid/uidvalidity/flags/internaldate). It fails on
// the first ingest error so the caller leaves the cursors unadvanced.
func (e *Engine) fetchAndIngest(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, uids sync.UIDSet, items sync.FetchItems) (ingestBatch, error) {
	var b ingestBatch
	// Each message is ingested AS IT ARRIVES rather than after the whole range
	// has been read (#351). The range here was once an entire mailbox — a first
	// sync, or any UIDVALIDITY roll, rewinds the cursor to 0 — so buffering it
	// first put ~2x the mailbox's raw bytes in memory before a single row was
	// written. Ingesting inside the stream also means a fetch that dies part-way
	// leaves the messages it did reach durably stored, instead of nothing. Since
	// #656 every caller also bounds the range to a chunk of UIDs, which bounds
	// the COMMAND; streaming is still what bounds memory.
	err := sess.FetchByUID(ctx, uids, items, func(m sync.Message) error {
		if m.Body == nil {
			return fmt.Errorf("mailsync: fetch %q uid %d: server returned no body", name, m.UID)
		}
		// COUNTED ON RECEIPT, BEFORE INGEST (#52 review H2). The bytes were
		// downloaded whether or not ingest then accepts them, and the Gmail
		// download budget meters exactly this count — a body whose ingest
		// failed is still a body Gmail charged for.
		b.bodies++
		b.bodyBytes += int64(len(m.Body))
		b.seen = append(b.seen, int64(m.UID))
		if items.Flags && m.Flags == nil {
			// NO FLAGS IN THE RESPONSE (#909). The body is real, so the message
			// is ingested — holding back mail over a missing flag would be the
			// worse harm — and its flags are re-read on their own once this
			// command has finished (rereadAbsentFlags).
			b.flagsAbsent = append(b.flagsAbsent, int64(m.UID))
		}
		prov := ingest.Provenance{
			AccountID:    req.AccountID,
			MailboxID:    mailboxID,
			UID:          int64(m.UID),
			UIDValidity:  int64(status.UIDValidity),
			RemoteFlags:  flagStrings(m.Flags),
			InternalDate: m.InternalDate,
		}
		withGmail(&prov, items, m.MessageMeta)
		res, err := e.ingest.Ingest(ctx, req.UserID, req.DEK, req.DEKID, m.Body, prov)
		if err != nil {
			return fmt.Errorf("mailsync: ingest %q uid %d: %w", name, m.UID, err)
		}
		// The cursor still moves past it (maxUID below): the poll will not ask
		// for it again, and the observation finds it as a gap if it outlives its
		// expunge — which is the dead-letter case, where it is then ingested.
		if int64(m.UID) > b.maxUID {
			b.maxUID = int64(m.UID)
		}
		if res.AwaitingExpunge {
			b.awaiting++
			return nil
		}
		b.labelChanges += res.Gmail.LabelsAdded + res.Gmail.LabelsRemoved
		if res.Gmail.FolderChanged {
			b.labelChanges++
		}
		b.count++
		if res.Created {
			b.created++
			switch {
			case res.Muted:
				b.muted++
			case res.Blocked:
				b.blocked++
			default:
				b.notify = append(b.notify, res.MessageID)
			}
		}
		return nil
	})
	if len(b.flagsAbsent) > 0 {
		e.obs.FlagsAbsent("ingest", len(b.flagsAbsent))
	}
	if err != nil {
		// A callback error is already wrapped and named; only a transport-level
		// failure needs the fetch context added.
		if strings.HasPrefix(err.Error(), "mailsync: ") {
			return b, err
		}
		return b, fmt.Errorf("mailsync: fetch %q: %w", name, err)
	}
	if len(b.flagsAbsent) > 0 {
		if rerr := e.rereadAbsentFlags(ctx, req, sess, mailboxID, status, name, b.flagsAbsent); rerr != nil {
			return b, rerr
		}
	}
	return b, nil
}

// rereadAbsentFlags asks again, FLAGS only, for messages a body FETCH just
// ingested without them (#909), and applies each reading that comes back
// with FLAGS. One that comes back without them again is left as ingested —
// with no flags — and is counted (mailroom_fetch_flags_absent_total
// {path="ingest_reread"}). On a server WITHOUT CONDSTORE the flag sweep reads
// it again within its window. On a CONDSTORE server NOTHING re-reads it until
// its MODSEQ next moves (a new message is above the CONDSTORE pass's range,
// so holding HIGHESTMODSEQ back would not help): it stays unread-looking
// until then. That is the bounded cost of delivering the mail rather than
// withholding it, and the counter says how often it happens. On Gmail the re-read carries no labels (FLAGS only, a few bytes, nothing
// against the download budget); the labels came with the body or the Gmail
// sweep reads them.
func (e *Engine) rereadAbsentFlags(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, uids []int64) error {
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	var scratch Summary
	_, _, _, err := e.refreshFlagsCounting(ctx, req, sess, mailboxID, status, name, uidSetOf(uids),
		sync.FetchItems{Flags: true}, "ingest_reread", &scratch)
	return err
}

// add folds another batch's counts into b — EVERY outcome, including the
// notify ids and the blocked count (#908). Those two were once left out, so a
// new message recovered by the #93 gap backfill or by the CONDSTORE body
// fallback (both reach here through fetchAndIngestUIDs) never sent a push and
// never counted as blocked. TestRecoveredArrivalsCarryNotifyAndBlocked is the
// guard.
func (b *ingestBatch) add(o ingestBatch) {
	b.count += o.count
	b.created += o.created
	b.muted += o.muted
	b.blocked += o.blocked
	b.notify = append(b.notify, o.notify...)
	b.flagsAbsent = append(b.flagsAbsent, o.flagsAbsent...)
	b.bodies += o.bodies
	b.bodyBytes += o.bodyBytes
	b.awaiting += o.awaiting
	b.labelChanges += o.labelChanges
	if o.maxUID > b.maxUID {
		b.maxUID = o.maxUID
	}
}

// fetchAndIngestUIDs is fetchAndIngest over an explicit UID list, issued as
// commands of at most e.fetchChunk UIDs each (#656). It stops at the first
// failing command and returns what the earlier ones ingested alongside the
// error — every message is ingested as it arrives, so that work is kept.
func (e *Engine) fetchAndIngestUIDs(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, uids []int64, items sync.FetchItems) (ingestBatch, error) {
	var total ingestBatch
	for _, chunk := range uidChunks(uids, e.fetchChunk) {
		b, err := e.fetchAndIngest(ctx, req, sess, mailboxID, status, name, chunk, items)
		total.add(b)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// refreshFlags runs the CONDSTORE flag-reconcile FETCH over UIDs the mailbox
// poll already believes it holds, and hands each result to
// ingest.Ingester.RefreshFlags instead of Ingest (#523) — so a change no
// client made to anything but flags never opens the blob store, the MIME
// parser, or the malware scanner. It reports how many it refreshed and, for
// any UID RefreshFlags could not find a live provenance row for (reaped out
// from under the cursor, or a #93 gap the mailbox has not verified yet), the
// UID so the caller can fall back to a body fetch for exactly those.
//
// Errors are wrapped/named exactly like fetchAndIngest's, for the same reason:
// a callback error is already named, and only a transport-level failure needs
// the fetch context added.
func (e *Engine) refreshFlags(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, uids sync.UIDSet, items sync.FetchItems, summary *Summary) (refreshed int, missing []int64, err error) {
	refreshed, missing, _, err = e.refreshFlagsCounting(ctx, req, sess, mailboxID, status, name, uids, items, "refresh", summary)
	return refreshed, missing, err
}

// refreshFlagsCounting is refreshFlags plus the count of what Gmail label
// readings changed (#52; zero unless items.GmailExt). path names the caller
// for the counters ("refresh" for the generic CONDSTORE pass, "gmail" for the
// Gmail model's CONDSTORE pass and label sweep).
//
// ⚠ A MESSAGE WHOSE RESPONSE LACKS FLAGS IS NO READING (#909), and is skipped
// — not refreshed, not "missing", not body-fetched. go-imap hands each
// message on with err=nil and whatever items were parsed before a truncated
// response (EOF mid-message) ended, and a FLAGS item the server sent in a
// SECOND FETCH response for the same message goes to the unilateral handler,
// never to this message's buffer; the command's error, if any, surfaces only
// at Close(). Reading a missing FLAGS as an empty one applied `flags=[]` —
// cleared \Seen, \Flagged and every keyword — before that error was seen.
// sync.MessageMeta.Flags is now nil for "not in the response" and non-nil
// (possibly empty) for "the server said FLAGS (…)" (the vendored go-imap's
// patch 2), the same absent-versus-empty rule #52 review H1 set for
// X-GM-LABELS. A FLAGS item that WAS parsed is a genuine reading even if a
// later item in the same command fails, so per-message commits stay.
func (e *Engine) refreshFlagsCounting(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, uids sync.UIDSet, items sync.FetchItems, path string, summary *Summary) (refreshed int, missing []int64, labelChanges int, err error) {
	applied, absent := 0, 0
	defer func() {
		if absent > 0 {
			summary.FlagsAbsent += absent
			e.obs.FlagsAbsent(path, absent)
		}
		if err != nil && applied > 0 {
			// #909's report: readings this command applied before it failed.
			summary.FlagRefreshAborted += applied
			e.obs.FlagRefreshAborted(path, applied)
		}
	}()
	err = sess.FetchByUID(ctx, uids, items, func(m sync.Message) error {
		if items.Flags && m.Flags == nil {
			absent++
			return nil
		}
		prov := ingest.Provenance{
			AccountID:   req.AccountID,
			MailboxID:   mailboxID,
			UID:         int64(m.UID),
			UIDValidity: int64(status.UIDValidity),
			RemoteFlags: flagStrings(m.Flags),
		}
		withGmail(&prov, items, m.MessageMeta)
		res, ferr := e.ingest.RefreshFlags(ctx, req.UserID, prov)
		if ferr != nil {
			return fmt.Errorf("mailsync: refresh flags %q uid %d: %w", name, m.UID, ferr)
		}
		if !res.Found {
			missing = append(missing, int64(m.UID))
			return nil
		}
		applied++
		labelChanges += res.Gmail.LabelsAdded + res.Gmail.LabelsRemoved
		if res.Gmail.FolderChanged {
			labelChanges++
		}
		refreshed++
		return nil
	})
	if err != nil {
		// A callback error is already wrapped and named; only a transport-level
		// failure needs the fetch context added.
		if strings.HasPrefix(err.Error(), "mailsync: ") {
			return refreshed, missing, labelChanges, err
		}
		return refreshed, missing, labelChanges, fmt.Errorf("mailsync: fetch %q: %w", name, err)
	}
	return refreshed, missing, labelChanges, nil
}

// displayName derives a human-facing name from a raw IMAP mailbox name: the
// leaf segment after the hierarchy delimiter (e.g. "INBOX/Work" -> "Work").
// A zero delimiter (server sent NIL) or a trailing delimiter yields the raw
// name unchanged.
func displayName(imapName string, delim rune) string {
	if delim == 0 {
		return imapName
	}
	if i := strings.LastIndex(imapName, string(delim)); i >= 0 && i+len(string(delim)) < len(imapName) {
		return imapName[i+len(string(delim)):]
	}
	return imapName
}

// specialUse returns the mailbox's RFC 6154 role attribute as a string, or ""
// if it has none.
func specialUse(m sync.Mailbox) string {
	if a, ok := m.SpecialUse(); ok {
		return string(a)
	}
	return ""
}

// selectable reports whether a mailbox can be SELECTed (not a container-only or
// nonexistent node).
func selectable(m sync.Mailbox) bool {
	return !m.HasAttr(sync.MailboxAttrNoSelect) && !m.HasAttr(sync.MailboxAttrNonExistent)
}

// flagStrings converts fetched IMAP flags to the string form ingest expects.
// nil (no FLAGS in the response) stays nil, so "absent" is never turned into
// "empty" here (#909); an empty reading stays empty and non-nil.
func flagStrings(flags []sync.Flag) []string {
	if flags == nil {
		return nil
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}
