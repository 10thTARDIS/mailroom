// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/gmaillabel"
	"mailroom/internal/ingest"
	"mailroom/internal/sync"
)

// THE GMAIL SYNC MODEL (#52, ADR-047, increment 5a — the read path).
//
// A live Gmail OAuth account is synced differently from every other IMAP
// account, in four ways, all switched on by AccountSync.Gmail and nothing
// else (the generic path is untouched when it is nil):
//
//  1. WHAT IS SELECTED. Only the mailboxes Gmail marks \All (All Mail), \Junk
//     (Spam) and \Trash. Those three sets are disjoint, so each message is
//     downloaded once. Every other folder — INBOX, the user's labels,
//     [Gmail]/Important, Starred, Sent, Drafts — is a LABEL VIEW of All Mail:
//     it is listed and upserted (compose resolves Sent and Drafts by role, and
//     placement needs INBOX's row), and never SELECTed or EXAMINEd. A Gmail
//     with All Mail hidden from IMAP is refused (ErrGmailAllMailHidden), never
//     worked around by syncing the label folders.
//
//  2. WHAT EACH MESSAGE CARRIES. Every fetch adds X-GM-MSGID, X-GM-THRID and
//     X-GM-LABELS (the vendored go-imap patch), and ingest applies the labels
//     in the same transaction as the message (store.ApplyGmailObservation):
//     \Inbox/\Sent/\Draft become the folder placement, user labels become
//     Mailroom labels, and only Gmail's CHANGES are applied.
//
//  3. NEWEST FIRST, UNDER A DAILY BYTE BUDGET. Gmail suspends an account's
//     IMAP everywhere — the person's phone included — when it downloads more
//     than its daily limit (2,500 MB/day for Workspace; the consumer figure is
//     UNVERIFIED and assumed no higher). So:
//       - a mailbox's first sync starts its cursor at UIDNEXT: new mail flows
//         from the first pass, and all of history sits below an explicit
//         BACKFILL FLOOR (app.mailboxes.backfill_floor_uid);
//       - history is then fetched DOWNWARD from the floor, newest first, in
//         chunks whose RFC822.SIZE is read before any body, so a chunk is
//         taken only as far as the budget allows;
//       - every body byte — new mail, history, gap refetches — is metered
//         per account over a rolling 24 hours, on receipt and including what
//         a failed command drained (app.account_sync_status.download_hours),
//         persisted per command so a restart cannot reset it. History may spend
//         at most historyShare of the budget; the rest is kept for new mail.
//       - the #93 gap backfill only looks AT OR ABOVE the floor. Below it is
//         history not yet imported, not mail a FETCH dropped — without the
//         floor, the gap detector would read all of history as gaps and fetch
//         it at once, unpaced, which is the exact thing the budget exists to
//         prevent.
//
//  4. LABEL CHANGES. A label changed in Gmail's own apps arrives two ways:
//     through CONDSTORE (the flag pass asks for X-GM-LABELS too) IF Gmail
//     bumps MODSEQ on a label change — UNVERIFIED, which is why there is also
//     a periodic LABEL SWEEP (UID FETCH floor:lastSeen (FLAGS X-GM-LABELS),
//     about 50–100 bytes a message, on GmailSync.LabelSweepInterval).
//
// Writeback is OFF for Gmail until increment 5b (ADR-028 ingest-only, forced
// in the store: store.EnqueueIntent). Nothing here writes to the server: every
// SELECT is an EXAMINE and every body fetch is BODY.PEEK[].

// ErrGmailAllMailHidden is a Gmail account whose LIST has no \All mailbox:
// the user has hidden All Mail from IMAP. Syncing the label folders instead
// would download every message once per label (ADR-047), so it is refused.
var ErrGmailAllMailHidden = errors.New("mailsync: this Gmail account does not show All Mail to IMAP; " +
	"in Gmail's settings (Labels), turn on \"Show in IMAP\" for All Mail")

// ErrGmailNoExtension is a server synced as Gmail that does not advertise
// X-GM-EXT-1.
var ErrGmailNoExtension = errors.New("mailsync: the server does not advertise X-GM-EXT-1; it is not Gmail")

// errGmailRolled: All Mail, Spam or Trash changed UIDVALIDITY. The generic
// engine re-fetches the whole mailbox and proves the re-fetch complete before
// reconciling (#88); for Gmail that re-fetch would be the whole account at
// once, unbudgeted. So the mailbox is left exactly as it is — nothing retired,
// nothing reaped, cursors untouched — and reported every pass until an
// operator decides. Gmail does not roll All Mail in normal operation.
var errGmailRolled = errors.New("UIDVALIDITY changed on a Gmail mailbox; it is NOT re-fetched " +
	"(that would download the whole account at once, outside the daily budget) and nothing in it " +
	"was retired or reaped — operator action needed")

// historyShare is the part of the daily budget history may spend; the rest is
// kept for new mail and gap refetches, so a long import never starves today's
// mail.
const historyShare = 0.9

// gmailSweepChunk is how many UIDs one label-sweep FETCH names. The sweep
// carries no bodies (FLAGS + X-GM-LABELS, ~100 bytes a message), so it can
// name far more than a body fetch.
var gmailSweepChunk int64 = 5000 // a var only so tests can make it small

// DefaultGmailDailyBudget is the Gmail profile's daily download budget:
// 2,000 MB, below Gmail's documented 2,500 MB/day (ADR-047 §6).
const DefaultGmailDailyBudget int64 = 2000 * 1000 * 1000

// DefaultGmailLabelSweepInterval is how often the label sweep re-reads every
// imported message's labels while it is not known whether CONDSTORE reports
// label changes. At ~100 bytes a message it is ~40 MB a day for 100k messages
// at this interval — inside the 500 MB between the budget and Gmail's limit.
const DefaultGmailLabelSweepInterval = 6 * time.Hour

// GmailSync switches an account onto the Gmail sync model. A nil
// AccountSync.Gmail is every other account, unchanged.
type GmailSync struct {
	// DailyBudgetBytes is the most message-body bytes any 24 hours may
	// download for this account. <= 0 disables the budget (tests only).
	DailyBudgetBytes int64
	// LabelSweepInterval is how often the label sweep runs per mailbox; 0
	// disables it (for when CONDSTORE is known to report label changes).
	LabelSweepInterval time.Duration
}

// GmailStore is the store surface only the Gmail model needs. StoreAdapter
// implements it; a MailboxStore that does not cannot sync Gmail.
type GmailStore interface {
	SetBackfillFloor(ctx context.Context, userID, mailboxID ulid.ULID, floor int64) error
	StampLabelSweep(ctx context.Context, userID, mailboxID ulid.ULID, at time.Time) error
	DownloadedLast24h(ctx context.Context, userID, accountID ulid.ULID, at time.Time) (int64, error)
	AddDownloadBytes(ctx context.Context, userID, accountID ulid.ULID, at time.Time, n int64) (int64, error)
	// SetSyncScopeOff marks a mailbox not synced (with its Mailbox change
	// row, store.SetMailboxSyncScope).
	SetSyncScopeOff(ctx context.Context, userID, mailboxID ulid.ULID) error
	// SetLabelSweepNext records how far an interrupted label sweep got
	// (nil = no sweep in progress).
	SetLabelSweepNext(ctx context.Context, userID, mailboxID ulid.ULID, next *int64) error
}

// gmailLabelView reports whether a listed Gmail mailbox is a label view that
// holds no placement: everything but the three synced mailboxes, INBOX, Sent,
// Drafts and non-selectable containers.
func gmailLabelView(m sync.Mailbox) bool {
	if gmailMailboxSynced(m) || !selectable(m) || strings.EqualFold(m.Name, "INBOX") ||
		m.HasAttr(sync.MailboxAttrSent) || m.HasAttr(sync.MailboxAttrDrafts) {
		return false
	}
	return true
}

// gmailPass is one pass's Gmail state: the store, the budget's meter.
type gmailPass struct {
	cfg   GmailSync
	store GmailStore
	now   time.Time        // the pass's start: the label sweep's cadence
	clock func() time.Time // the meter charges at the time of each charge
	used  int64            // bytes metered in the last 24 hours, as of the last store write
}

// budgeted reports whether a byte budget applies.
func (g *gmailPass) budgeted() bool { return g.cfg.DailyBudgetBytes > 0 }

// remaining is what may still be downloaded today: for history, what is left
// of historyShare of the budget; otherwise what is left of all of it.
func (g *gmailPass) remaining(history bool) int64 {
	if !g.budgeted() {
		return 1 << 62
	}
	limit := g.cfg.DailyBudgetBytes
	if history {
		limit = int64(float64(limit) * historyShare)
	}
	if r := limit - g.used; r > 0 {
		return r
	}
	return 0
}

// spend meters n bytes against today, durably.
func (g *gmailPass) spend(ctx context.Context, req AccountSync, n int64) error {
	if n <= 0 {
		return nil
	}
	// Charged at the time it happened, not at the pass's start (#52 review
	// M2): a pass that runs across an hour boundary charges the hour it is in.
	total, err := g.store.AddDownloadBytes(ctx, req.UserID, req.AccountID, g.clock(), n)
	if err != nil {
		return fmt.Errorf("mailsync: meter download: %w", err)
	}
	g.used = total
	return nil
}

// Preflighter is the optional ingest surface the Gmail path asks before any
// body download: a non-nil error (ingest.ErrQuotaExhausted,
// ingest.ErrScannerUnavailable) pauses the pass. *ingest.Ingestor implements
// it.
type Preflighter interface {
	Preflight(ctx context.Context, userID ulid.ULID) error
}

// gmailMailboxSynced reports whether a Gmail mailbox's bodies are synced:
// \All, \Junk and \Trash, and nothing else.
func gmailMailboxSynced(m sync.Mailbox) bool {
	return m.HasAttr(sync.MailboxAttrAll) || m.HasAttr(sync.MailboxAttrJunk) || m.HasAttr(sync.MailboxAttrTrash)
}

// startGmailPass checks the account really is Gmail with All Mail visible,
// and reads today's meter. Any failure fails the whole pass: syncing a Gmail
// account any other way is the thing ADR-047 exists to prevent.
func (e *Engine) startGmailPass(ctx context.Context, req AccountSync, sess sync.SyncSession, mailboxes []sync.Mailbox) (*gmailPass, error) {
	gs, ok := e.store.(GmailStore)
	if !ok {
		return nil, errors.New("mailsync: this store cannot sync Gmail (no GmailStore)")
	}
	if !sess.Supports(sync.CapabilityGmailExt) {
		return nil, ErrGmailNoExtension
	}
	all := false
	for _, m := range mailboxes {
		if m.HasAttr(sync.MailboxAttrAll) && selectable(m) {
			all = true
		}
	}
	if !all {
		return nil, ErrGmailAllMailHidden
	}
	g := &gmailPass{cfg: *req.Gmail, store: gs, now: e.now(), clock: e.now}
	// THE PAUSES ARE ASKED ABOUT BEFORE A BYTE IS DOWNLOADED (#52 review H2).
	// The generic engine learns about a full quota or a dead scanner from the
	// first ingest that fails — after its body was downloaded, and after
	// FetchByUID's Close drained the rest of the chunk. On Gmail every one of
	// those bytes is budget, and the pass that fails is retried every five
	// minutes; so an ingester that can say so up front is asked here, and a
	// pause stops the pass before it fetches anything.
	if pf, ok := e.ingest.(Preflighter); ok {
		if err := pf.Preflight(ctx, req.UserID); err != nil {
			return nil, err
		}
	}
	used, err := gs.DownloadedLast24h(ctx, req.UserID, req.AccountID, g.now)
	if err != nil {
		return nil, fmt.Errorf("mailsync: read download meter: %w", err)
	}
	g.used = used
	return g, nil
}

// gmailItems are the body-fetch items on the Gmail path.
func gmailItems(condStore bool) sync.FetchItems {
	return sync.FetchItems{Flags: true, Envelope: true, InternalDate: true, BodyFull: true, ModSeq: condStore, GmailExt: true}
}

// withGmail attaches the X-GM-EXT-1 reading to a provenance when the fetch
// asked for it AND the server's response carried X-GM-LABELS.
//
// ⚠ A MISSING X-GM-LABELS IS NO READING, NEVER "NO LABELS" (#52 review H1).
// go-imap hands the callback a message whose labels are nil, with no error,
// when the response was truncated mid-list, when the list was malformed, and
// when Gmail split the labels into a second FETCH response (which goes to the
// unilateral handler); the error, if any, only surfaces at the command's
// Close, after earlier messages have committed. Read as an empty set, any of
// those would remove every label and the INBOX placement, and the next sweep
// would re-add them as fresh filings. So nil leaves prov.Gmail nil: the copy
// is ingested or refreshed exactly like a non-Gmail one, its labels untouched.
func withGmail(prov *ingest.Provenance, items sync.FetchItems, m sync.MessageMeta) {
	if !items.GmailExt || m.GmailLabels == nil {
		return
	}
	prov.Gmail = &ingest.GmailReading{MsgID: m.GmailMsgID, ThrID: m.GmailThrID, Labels: gmaillabel.Sanitize(m.GmailLabels)}
	if m.GmailMsgID != 0 {
		prov.GmMsgID = strconv.FormatUint(m.GmailMsgID, 10)
	}
}

// errBudgetSpent ends a mailbox's work for the day: the budget allows no more
// bodies. It is not a failure — the mailbox is simply continued tomorrow.
var errBudgetSpent = errors.New("mailsync: the daily download budget is spent")

// sized is a UID with its RFC822.SIZE.
type sized struct {
	uid  int64
	size int64
}

// fetchSizes reads RFC822.SIZE (no bodies) for a UID set.
func fetchSizes(ctx context.Context, sess sync.SyncSession, name string, uids sync.UIDSet) ([]sized, error) {
	var out []sized
	err := sess.FetchByUID(ctx, uids, sync.FetchItems{RFC822Size: true}, func(m sync.Message) error {
		out = append(out, sized{uid: int64(m.UID), size: m.RFC822Size})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("mailsync: fetch sizes %q: %w", name, err)
	}
	return out, nil
}

// takeWithin returns the longest prefix of cands whose sizes fit in budget.
// If nothing fits but nothing has been spent today either, the first one is
// taken anyway: a single message larger than the whole allowance would
// otherwise block the mailbox forever, and one oversized message a day is the
// bounded cost of not doing that.
func (g *gmailPass) takeWithin(cands []sized, budget int64) []sized {
	if !g.budgeted() {
		return cands
	}
	var sum int64
	for i, c := range cands {
		if sum+c.size > budget {
			if i == 0 && g.used == 0 {
				return cands[:1]
			}
			return cands[:i]
		}
		sum += c.size
	}
	return cands
}

// fetchBudgeted fetches and ingests the given candidates' bodies, metering
// each command's bytes. It never fetches a UID that is not in cands.
func (e *Engine) fetchBudgeted(ctx context.Context, req AccountSync, sess sync.SyncSession, g *gmailPass, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, cands []sized, items sync.FetchItems) (ingestBatch, error) {
	var total ingestBatch
	uids := make([]int64, len(cands))
	for i, c := range cands {
		uids[i] = c.uid
	}
	size := make(map[int64]int64, len(cands))
	for _, c := range cands {
		size[c.uid] = c.size
	}
	for _, chunk := range uidChunks(uids, e.fetchChunk) {
		b, err := e.fetchAndIngest(ctx, req, sess, mailboxID, status, name, chunk, items)
		total.add(b) // carries notify and blocked since #908
		charge := b.bodyBytes
		if err != nil {
			// A FAILED COMMAND STILL DOWNLOADED (#52 review H2). A callback
			// error makes FetchByUID Close() the command, which drains it:
			// every remaining body in the chunk is read off the wire and
			// thrown away. Those bytes never reached the callback, so they
			// are charged at the RFC822.SIZE already known for them —
			// conservatively, whether or not the server got to send them.
			charge += unseenSize(chunk, b.seen, size)
		}
		if serr := g.spend(ctx, req, charge); serr != nil && err == nil {
			err = serr
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// unseenSize sums the known sizes of the UIDs in set that are not in seen.
func unseenSize(set sync.UIDSet, seen []int64, size map[int64]int64) int64 {
	got := make(map[int64]bool, len(seen))
	for _, u := range seen {
		got[u] = true
	}
	var n int64
	for _, r := range set {
		for u := int64(r.Start); u <= int64(r.Stop); u++ {
			if !got[u] {
				n += size[u]
			}
		}
	}
	return n
}

// syncGmailMailbox is syncMailbox for a Gmail \All, \Junk or \Trash mailbox.
func (e *Engine) syncGmailMailbox(ctx context.Context, req AccountSync, sess sync.SyncSession, g *gmailPass, condStore bool, name string, stored StoredMailbox, summary *Summary) (mailboxSync, int64, error) {
	status, err := sess.Select(ctx, name, sync.SelectOptions{ReadOnly: true, CondStore: condStore})
	if err != nil {
		return mailboxSync{}, 0, fmt.Errorf("mailsync: select %q: %w", name, err)
	}
	if stored.UIDValidity != 0 && stored.UIDValidity != int64(status.UIDValidity) {
		return mailboxSync{}, 0, fmt.Errorf("mailsync: %q: %w", name, errGmailRolled)
	}
	if status.UIDNext == 0 {
		// Newest-first needs to know where "now" is; without UIDNEXT there is no
		// floor to set, and an open-ended 1:* is the unbudgeted walk ADR-047
		// forbids.
		return mailboxSync{}, 0, fmt.Errorf("mailsync: %q: the server reported no UIDNEXT; a Gmail mailbox cannot be imported without one", name)
	}

	lastSeen := stored.LastSeenUID
	floor := int64(1)
	if stored.BackfillFloorUID != nil {
		floor = *stored.BackfillFloorUID
	}
	if stored.UIDValidity == 0 {
		// FIRST SYNC: start at "now". Everything that exists is history, below
		// the floor, and is imported newest-first under the budget. The floor
		// is written BEFORE the cursors: a crash between the two re-runs this
		// with a fresher UIDNEXT, which only moves more mail into history.
		lastSeen = int64(status.UIDNext) - 1
		floor = int64(status.UIDNext)
		if err := g.store.SetBackfillFloor(ctx, req.UserID, stored.ID, floor); err != nil {
			return mailboxSync{}, 0, fmt.Errorf("mailsync: set backfill floor %q: %w", name, err)
		}
	}
	items := gmailItems(condStore)

	// Label/flag changes on imported mail via CONDSTORE — bounded BELOW by the
	// floor as well as above by the cursor: a UID under the floor has no row
	// yet, and asking about it would only produce "missing" UIDs to fetch
	// outside the budget. Missing UIDs are NOT body-fetched here; the gap
	// backfill finds them on the observation cadence, under the budget.
	if condStore && stored.HighestModSeq != 0 && status.HighestModSeq > uint64(stored.HighestModSeq) && lastSeen >= floor {
		refreshed, _, changes, err := e.refreshFlagsCounting(ctx, req, sess, stored.ID, status, name,
			sync.UIDSetRange(sync.UID(floor), sync.UID(lastSeen)),
			sync.FetchItems{Flags: true, ModSeq: true, ChangedSince: uint64(stored.HighestModSeq), GmailExt: true}, "gmail", summary)
		if err != nil {
			return mailboxSync{}, 0, err
		}
		summary.FlagUpdates += refreshed
		summary.GmailLabelChanges += changes
	}

	// New mail: ascending above the cursor, each chunk sized first and taken
	// only as far as the budget allows.
	newLastSeen := lastSeen
	budgetHit := false
	if sync.UID(lastSeen)+1 < status.UIDNext {
		for _, chunk := range newMessageChunks(sync.UID(lastSeen+1), status.UIDNext, e.fetchChunk) {
			cands, err := fetchSizes(ctx, sess, name, chunk)
			if err == nil {
				sort.Slice(cands, func(i, j int) bool { return cands[i].uid < cands[j].uid })
				take := g.takeWithin(cands, g.remaining(false))
				var b ingestBatch
				b, err = e.fetchBudgeted(ctx, req, sess, g, stored.ID, status, name, take, items)
				summary.addBatch(b)
				if b.maxUID > newLastSeen {
					newLastSeen = b.maxUID
				}
				if err == nil && len(take) < len(cands) {
					budgetHit = true
				}
			}
			if err != nil {
				return mailboxSync{}, 0, e.recordPartialPoll(ctx, req, stored.ID, name, status, false, lastSeen, newLastSeen, err)
			}
			if budgetHit {
				break
			}
		}
	}

	cursors := MailboxCursors{
		UIDValidity:   int64(status.UIDValidity),
		LastSeenUID:   newLastSeen,
		HighestModSeq: int64(status.HighestModSeq),
		UIDNext:       int64(status.UIDNext),
		Messages:      int64(status.NumMessages),
	}
	if budgetHit {
		// Part of today's new mail is still on the server: no poll baseline
		// (the recordPartialPoll invariant).
		cursors.UIDNext, cursors.Messages = 0, 0
	}
	if err := e.store.UpdateMailboxCursors(ctx, req.UserID, stored.ID, cursors); err != nil {
		return mailboxSync{}, 0, fmt.Errorf("mailsync: update cursors %q: %w", name, err)
	}
	if budgetHit {
		summary.GmailBudgetSpent = true
		return mailboxSync{uidValidity: int64(status.UIDValidity), lastSeenUID: newLastSeen}, floor, errBudgetSpent
	}

	// History, newest first, below the floor.
	floor, err = e.backfillGmailHistory(ctx, req, sess, g, stored.ID, status, name, floor, items, summary)
	if err != nil && !errors.Is(err, errBudgetSpent) {
		return mailboxSync{}, 0, err
	}
	if errors.Is(err, errBudgetSpent) {
		summary.GmailBudgetSpent = true
	}

	// The label sweep, over what is imported.
	// A sweep that was interrupted is continued from where it stopped,
	// whatever the interval says (#52 review: a persistent failure must not
	// restart a full sweep every pass).
	start := floor
	resume := stored.LabelSweepNext != nil && *stored.LabelSweepNext > floor
	if resume {
		start = *stored.LabelSweepNext
	}
	if g.cfg.LabelSweepInterval > 0 && newLastSeen >= floor &&
		(resume || stored.LabelsSweptAt == nil || g.now.Sub(*stored.LabelsSweptAt) >= g.cfg.LabelSweepInterval) {
		if err := e.sweepGmailLabels(ctx, req, sess, g, stored.ID, status, name, start, newLastSeen, summary); err != nil {
			return mailboxSync{}, 0, err
		}
	}
	return mailboxSync{uidValidity: int64(status.UIDValidity), lastSeenUID: newLastSeen}, floor, nil
}

// backfillGmailHistory walks history downward from floor, newest first, while
// the history share of the budget lasts. It returns the new floor; the floor
// is persisted after every chunk, so whatever was fetched is never fetched
// again. errBudgetSpent means it stopped for the day.
func (e *Engine) backfillGmailHistory(ctx context.Context, req AccountSync, sess sync.SyncSession, g *gmailPass, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, floor int64, items sync.FetchItems, summary *Summary) (int64, error) {
	step := int64(e.fetchChunk)
	for floor > 1 {
		if g.remaining(true) <= 0 {
			return floor, errBudgetSpent
		}
		lo := floor - step
		if lo < 1 {
			lo = 1
		}
		cands, err := fetchSizes(ctx, sess, name, sync.UIDSetRange(sync.UID(lo), sync.UID(floor-1)))
		if err != nil {
			return floor, err
		}
		// Newest first within the chunk, as across chunks.
		sort.Slice(cands, func(i, j int) bool { return cands[i].uid > cands[j].uid })
		take := g.takeWithin(cands, g.remaining(true))
		b, err := e.fetchBudgeted(ctx, req, sess, g, mailboxID, status, name, take, items)
		summary.addBatch(b)
		summary.HistoryBodiesFetched += b.bodies
		if err != nil {
			// Nothing persisted for this chunk: it is re-sized and re-taken next
			// pass (ingest dedups what this one did store).
			return floor, err
		}
		next := lo
		if len(take) < len(cands) {
			// Stopped inside the chunk: everything from the lowest UID taken up
			// to the old floor is fetched; below it is not.
			next = floor
			if len(take) > 0 {
				next = take[len(take)-1].uid
			}
		}
		if next < floor {
			if err := g.store.SetBackfillFloor(ctx, req.UserID, mailboxID, next); err != nil {
				return floor, fmt.Errorf("mailsync: set backfill floor %q: %w", name, err)
			}
			floor = next
		}
		if len(take) < len(cands) {
			return floor, errBudgetSpent
		}
	}
	return floor, nil
}

// sweepGmailLabels re-reads FLAGS and X-GM-LABELS for every imported UID
// (from..lastSeen) and applies what changed. No bodies; a UID with no row is
// left for the gap backfill. Progress is recorded after every chunk
// (app.mailboxes.label_sweep_next_uid), so an interrupted sweep resumes
// rather than starting over.
func (e *Engine) sweepGmailLabels(ctx context.Context, req AccountSync, sess sync.SyncSession, g *gmailPass, mailboxID ulid.ULID, status *sync.MailboxStatus, name string, from, lastSeen int64, summary *Summary) error {
	for lo := from; lo <= lastSeen; lo += gmailSweepChunk {
		hi := lo + gmailSweepChunk - 1
		if hi > lastSeen {
			hi = lastSeen
		}
		_, _, changes, err := e.refreshFlagsCounting(ctx, req, sess, mailboxID, status, name,
			sync.UIDSetRange(sync.UID(lo), sync.UID(hi)), sync.FetchItems{Flags: true, GmailExt: true}, "gmail", summary)
		summary.GmailLabelChanges += changes
		if err != nil {
			return err
		}
		next := hi + 1
		if err := g.store.SetLabelSweepNext(ctx, req.UserID, mailboxID, &next); err != nil {
			return fmt.Errorf("mailsync: record label sweep progress %q: %w", name, err)
		}
	}
	if err := g.store.SetLabelSweepNext(ctx, req.UserID, mailboxID, nil); err != nil {
		return fmt.Errorf("mailsync: finish label sweep %q: %w", name, err)
	}
	if err := g.store.StampLabelSweep(ctx, req.UserID, mailboxID, g.now); err != nil {
		return fmt.Errorf("mailsync: stamp label sweep %q: %w", name, err)
	}
	summary.GmailLabelSweeps++
	return nil
}

// addBatch folds an ingest batch into the summary (the same counts the
// generic new-message pass adds).
func (s *Summary) addBatch(b ingestBatch) {
	s.MessagesIngested += b.created
	s.MessagesMuted += b.muted
	s.MessagesBlocked += b.blocked
	s.NotifyIDs = append(s.NotifyIDs, b.notify...)
	s.FlagUpdates += b.count - b.created
	s.BodiesFetched += b.bodies
	s.BodyBytesFetched += b.bodyBytes
	s.CopiesAwaitingExpunge += b.awaiting
	s.GmailLabelChanges += b.labelChanges
	s.FlagsAbsent += len(b.flagsAbsent)
}

// gapFloorFilter keeps the observed UIDs at or above floor: on a Gmail
// mailbox, a UID below the backfill floor is history not yet imported, never
// a #93 gap.
func gapFloorFilter(uids []int64, floor int64) []int64 {
	if floor <= 1 {
		return uids
	}
	out := make([]int64, 0, len(uids))
	for _, u := range uids {
		if u >= floor {
			out = append(out, u)
		}
	}
	return out
}
