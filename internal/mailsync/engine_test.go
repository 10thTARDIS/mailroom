// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// ---- fakes -----------------------------------------------------------------

// fakeBox is one mailbox on the fake server: a status plus its messages.
type fakeBox struct {
	status sync.MailboxStatus
	msgs   []sync.Message

	// liveUIDs, when non-nil, is what LiveUIDs reports instead of deriving the
	// set from msgs — the seam for testing an untrustworthy server answer.
	liveUIDs    []sync.UID
	liveUIDsSet bool
	// liveStatus, when non-nil, is the status LiveUIDs returns (defaults to
	// status) — the seam for a UIDVALIDITY that moves between poll and reap.
	liveStatus *sync.MailboxStatus
	liveErr    error

	// failAfterUID, when non-zero, makes the FETCH stream messages normally up to
	// and including that UID and then fail. It is the seam for #351: under the
	// old buffered fetch nothing was ingested when a fetch died, because the
	// error surfaced before the first Ingest call.
	failAfterUID sync.UID

	// dropUID, when non-zero, is silently OMITTED from every FETCH response, with
	// NO error — the #93 fault. UID SEARCH ALL still lists it, because the server
	// still has it: that asymmetry is the entire bug.
	dropUID sync.UID

	// flagsAbsent lists UIDs whose FETCH response carries no FLAGS item even
	// when FLAGS was asked for — #909's split or truncated response, as the
	// callback sees it.
	flagsAbsent map[sync.UID]bool
}

// fakeSession is an in-memory SyncSession. It records selects and fetches so
// tests can assert exactly which UID ranges were requested.
type fakeSession struct {
	caps      map[sync.Capability]bool
	mailboxes []sync.Mailbox
	boxes     map[string]*fakeBox
	listErr   error

	selected  string
	selects   []string
	fetches   []recordedFetch
	liveCalls []string // mailboxes LiveUIDs was asked about, in order
	calls     []string // every op, in order: "fetch:INBOX", "live:INBOX", ...
	loggedIn  bool     // set false on Logout, sanity only
	closed    bool
	noopErr   error // returned by Noop; non-nil simulates a dead connection
}

type recordedFetch struct {
	mailbox string
	uids    sync.UIDSet
	items   sync.FetchItems
}

func (s *fakeSession) ListMailboxes(context.Context) ([]sync.Mailbox, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.mailboxes, nil
}

func (s *fakeSession) Select(_ context.Context, name string, _ sync.SelectOptions) (*sync.MailboxStatus, error) {
	s.selects = append(s.selects, name)
	box, ok := s.boxes[name]
	if !ok {
		return nil, errors.New("fake: no such mailbox: " + name)
	}
	s.selected = name
	st := box.status
	st.Name = name
	return &st, nil
}

func (s *fakeSession) FetchByUID(_ context.Context, uids sync.UIDSet, items sync.FetchItems, fn func(sync.Message) error) error {
	s.fetches = append(s.fetches, recordedFetch{mailbox: s.selected, uids: uids, items: items})
	s.calls = append(s.calls, "fetch:"+s.selected)
	box := s.boxes[s.selected]
	for _, m := range box.msgs {
		if !uidInSet(m.UID, uids) {
			continue
		}
		if items.ChangedSince != 0 && m.ModSeq <= items.ChangedSince {
			continue
		}
		if box.dropUID != 0 && m.UID == box.dropUID {
			continue // silently truncated FETCH: no error, one message just missing
		}
		// A real server answers a FLAGS request with FLAGS (…), possibly
		// empty — non-nil since go-imap patch 2 — unless this message is the
		// #909 fault, whose response carries no FLAGS item at all.
		if items.Flags && m.Flags == nil && !box.flagsAbsent[m.UID] {
			m.Flags = []sync.Flag{}
		}
		if !items.Flags || box.flagsAbsent[m.UID] {
			m.Flags = nil
		}
		if err := fn(m); err != nil {
			return err
		}
		if box.failAfterUID != 0 && m.UID == box.failAfterUID {
			return errors.New("fake: connection died mid-stream")
		}
	}
	return nil
}

// LiveUIDs reports the UID set of the named mailbox, defaulting to exactly the
// UIDs its msgs carry (which is what a real server's UID SEARCH ALL returns).
func (s *fakeSession) LiveUIDs(_ context.Context, name string) (*sync.MailboxStatus, []sync.UID, error) {
	s.liveCalls = append(s.liveCalls, name)
	s.calls = append(s.calls, "live:"+name)
	box, ok := s.boxes[name]
	if !ok {
		return nil, nil, errors.New("fake: no such mailbox: " + name)
	}
	if box.liveErr != nil {
		return nil, nil, box.liveErr
	}
	// The real LiveUIDs EXAMINEs the mailbox, so it becomes the selected one —
	// which the gap backfill's FETCH relies on. Before #620 a Select of the same
	// mailbox always preceded this in the pass and hid the difference; a folder
	// whose poll was skipped has no such Select.
	s.selected = name
	st := box.status
	if box.liveStatus != nil {
		st = *box.liveStatus
	}
	st.Name = name
	if box.liveUIDsSet {
		return &st, box.liveUIDs, nil
	}
	uids := make([]sync.UID, 0, len(box.msgs))
	for _, m := range box.msgs {
		uids = append(uids, m.UID)
	}
	st.NumMessages = uint32(len(uids))
	return &st, uids, nil
}

func (s *fakeSession) SearchMessageID(context.Context, string, string) ([]sync.UID, error) {
	return nil, nil
}
func (s *fakeSession) UIDPresent(context.Context, string, sync.UID) (bool, error) {
	return false, nil
}
func (s *fakeSession) Supports(c sync.Capability) bool { return s.caps[c] }
func (s *fakeSession) Noop(context.Context) error      { return s.noopErr }
func (s *fakeSession) Notifications() <-chan sync.Notification {
	return nil
}
func (s *fakeSession) Idle(context.Context) (sync.IdleSession, error) { return nil, nil }
func (s *fakeSession) Logout(context.Context) error                   { s.loggedIn = false; return nil }
func (s *fakeSession) Close() error                                   { s.closed = true; return nil }

// Write methods: mailsync only reads (EXAMINE); these satisfy the extended
// SyncSession interface and are unused by the sync engine.
func (s *fakeSession) StoreFlags(context.Context, string, sync.UID, []sync.Flag, []sync.Flag) error {
	return nil
}
func (s *fakeSession) Copy(context.Context, string, sync.UIDSet, string) error   { return nil }
func (s *fakeSession) Move(context.Context, string, sync.UIDSet, string) error   { return nil }
func (s *fakeSession) Expunge(context.Context, string, sync.UIDSet) (int, error) { return 0, nil }
func (s *fakeSession) Append(context.Context, string, []sync.Flag, time.Time, []byte) (sync.UID, error) {
	return 0, nil
}

var _ sync.SyncSession = (*fakeSession)(nil)

// uidInSet reports whether uid falls in any range of set (Stop==0 is open).
func uidInSet(uid sync.UID, set sync.UIDSet) bool {
	for _, r := range set {
		if uid < r.Start {
			continue
		}
		if r.Stop == 0 || uid <= r.Stop {
			return true
		}
	}
	return false
}

// storedRow is a persisted mailbox in the fake store.
type storedRow struct {
	id            ulid.ULID
	syncScope     string
	uidValidity   int64
	lastSeenUID   int64
	highestModSeq int64
	lastReapedAt  *time.Time

	// The #620 baselines: the poll-time UIDNEXT/MESSAGES, and the observation-time
	// tuple with when it was recorded. nil = unknown, exactly as a NULL column.
	pollUIDNext  *int64
	pollMessages *int64
	observed     *store.ObservedStatus
	lastFullObs  *time.Time

	// prov is the mailbox's LIVE PROVENANCE: the UIDs we actually hold a location
	// for. It is what the #93 gap detector diffs the server's UID set against, so
	// the fake models it explicitly — a mailbox whose cursor says "polled up to UID
	// 5" and that holds no provenance rows at all is not a state that exists, and a
	// test that pretends otherwise would see phantom gaps.
	prov map[int64]bool
	// provFlags are the remote flags recorded for each live UID (the fake
	// ingester writes them on Ingest and RefreshFlags), which the flag sweep
	// compares a server reading against.
	provFlags map[int64][]string
	// pendingWriteback marks UIDs whose message has a flag writeback still
	// owed; acceptsKeywords is the recorded PERMANENTFLAGS \* probe.
	pendingWriteback map[int64]bool
	acceptsKeywords  bool
}

// fakeStore satisfies MailboxStore in memory, keyed by IMAP name.
type fakeStore struct {
	rows        map[string]*storedRow
	staleCalls  []ulid.ULID
	cursorCalls map[ulid.ULID]MailboxCursors
	reapCalls   []store.ReapObservation
	gapCalls    []store.GapObservation
	stampCalls  []ulid.ULID
	// seenCalls is the observation-time baseline each REAL stamp wrote (#620),
	// keyed by mailbox; unchangedCalls is every skipped observation's re-stamp.
	seenCalls      map[ulid.ULID]*store.ObservedStatus
	unchangedCalls []ulid.ULID
	genCalls       []store.GenerationObservation
	retireCalls    int
	deleteCalls    []ulid.ULID
	upsertErr      error
	cursorErr      error
	reapErr        error
	reapResult     store.ReapResult
	gapErr         error
	genErr         error
	genResult      store.GenerationResult
	retireCount    int64
	listAcctErr    error
	deleteErr      error
	deleteResult   store.DeletedMailboxResult
	liveFlagCalls  int
	scopeOffCalls  []ulid.ULID
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*storedRow{}, cursorCalls: map[ulid.ULID]MailboxCursors{},
		seenCalls: map[ulid.ULID]*store.ObservedStatus{}}
}

// seedRow registers a pre-existing mailbox, together with the live provenance UIDs
// it already holds — see storedRow.prov.
func (f *fakeStore) seedRow(name string, r *storedRow, provUIDs ...int64) *storedRow {
	r.prov = map[int64]bool{}
	for _, u := range provUIDs {
		r.prov[u] = true
	}
	f.rows[name] = r
	return r
}

// rowByID finds a mailbox by its id (the ingester records provenance through it).
func (f *fakeStore) rowByID(id ulid.ULID) *storedRow {
	for _, r := range f.rows {
		if r.id == id {
			return r
		}
	}
	return nil
}

func (f *fakeStore) UpsertMailbox(_ context.Context, _ ulid.ULID, rec MailboxRecord) (StoredMailbox, error) {
	if f.upsertErr != nil {
		return StoredMailbox{}, f.upsertErr
	}
	r, ok := f.rows[rec.IMAPName]
	if !ok {
		r = &storedRow{id: newULID(), syncScope: "full", prov: map[int64]bool{}}
		f.rows[rec.IMAPName] = r
	}
	return StoredMailbox{
		ID:            r.id,
		Created:       !ok,
		SyncScope:     r.syncScope,
		UIDValidity:   r.uidValidity,
		LastSeenUID:   r.lastSeenUID,
		HighestModSeq: r.highestModSeq,
		LastReapedAt:  r.lastReapedAt,

		PollUIDNext:        r.pollUIDNext,
		PollMessages:       r.pollMessages,
		Observed:           r.observed,
		LastObservedFullAt: r.lastFullObs,
	}, nil
}

func (f *fakeStore) UpdateMailboxCursors(_ context.Context, _, mailboxID ulid.ULID, c MailboxCursors) error {
	if f.cursorErr != nil {
		return f.cursorErr
	}
	f.cursorCalls[mailboxID] = c
	return nil
}

func (f *fakeStore) MarkMailboxProvenanceStale(_ context.Context, _, mailboxID ulid.ULID) (int64, error) {
	f.staleCalls = append(f.staleCalls, mailboxID)
	return 1, nil
}

// ReconcileRetiredGeneration mirrors StoreAdapter: the cursors are written by
// THIS call and only by it, so a refused or failed reconcile leaves the mailbox's
// cursors on the old generation — which is the whole retry mechanism.
func (f *fakeStore) ReconcileRetiredGeneration(_ context.Context, _ ulid.ULID, obs store.GenerationObservation, c MailboxCursors) (store.GenerationResult, error) {
	if f.genErr != nil {
		return store.GenerationResult{}, f.genErr
	}
	f.genCalls = append(f.genCalls, obs)
	if f.cursorErr != nil {
		return store.GenerationResult{}, f.cursorErr
	}
	f.cursorCalls[obs.MailboxID] = c
	return f.genResult, nil
}

func (f *fakeStore) ReapMailbox(_ context.Context, _ ulid.ULID, obs store.ReapObservation, seen *store.ObservedStatus) (store.ReapResult, error) {
	if f.reapErr != nil {
		return store.ReapResult{}, f.reapErr
	}
	f.reapCalls = append(f.reapCalls, obs)
	f.stampCalls = append(f.stampCalls, obs.MailboxID) // the real adapter stamps in the reap's tx
	f.seenCalls[obs.MailboxID] = seen
	return f.reapResult, nil
}

// MissingUIDs models store.UserTx.MissingUIDs: the UIDs the SERVER reports, below
// the bound, that we hold no live provenance row for.
func (f *fakeStore) MissingUIDs(_ context.Context, _ ulid.ULID, obs store.GapObservation) ([]int64, error) {
	if f.gapErr != nil {
		return nil, f.gapErr
	}
	f.gapCalls = append(f.gapCalls, obs)
	r := f.rowByID(obs.MailboxID)
	if r == nil {
		return nil, errors.New("fake: no such mailbox")
	}
	var out []int64
	for _, uid := range obs.ObservedUIDs {
		if uid < obs.UIDBound && !r.prov[uid] {
			out = append(out, uid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > obs.Limit {
		out = out[:obs.Limit]
	}
	return out, nil
}

func (f *fakeStore) StampMailboxObserved(_ context.Context, _, mailboxID ulid.ULID, seen *store.ObservedStatus) error {
	f.stampCalls = append(f.stampCalls, mailboxID)
	f.seenCalls[mailboxID] = seen
	return nil
}

func (f *fakeStore) StampMailboxUnchanged(_ context.Context, _, mailboxID ulid.ULID) error {
	f.unchangedCalls = append(f.unchangedCalls, mailboxID)
	return nil
}

func (f *fakeStore) RetireCanonicalPlacements(_ context.Context, _ ulid.ULID) (int64, error) {
	f.retireCalls++
	return f.retireCount, nil
}

// ListAccountMailboxes returns every seeded/upserted mailbox as (id, name), in a
// stable order — the set the folder-deletion reconciler diffs against the LIST.
func (f *fakeStore) ListAccountMailboxes(_ context.Context, _, _ ulid.ULID) ([]store.AccountMailbox, error) {
	if f.listAcctErr != nil {
		return nil, f.listAcctErr
	}
	out := make([]store.AccountMailbox, 0, len(f.rows))
	for name, r := range f.rows {
		out = append(out, store.AccountMailbox{ID: r.id, IMAPName: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IMAPName < out[j].IMAPName })
	return out, nil
}

// ReconcileDeletedMailbox records the folder-deletion call and, mirroring the real
// store, drops the mailbox row so a second pass would not re-detect it.
func (f *fakeStore) ReconcileDeletedMailbox(_ context.Context, _, mailboxID ulid.ULID) (store.DeletedMailboxResult, error) {
	if f.deleteErr != nil {
		return store.DeletedMailboxResult{}, f.deleteErr
	}
	f.deleteCalls = append(f.deleteCalls, mailboxID)
	for name, r := range f.rows {
		if r.id == mailboxID {
			delete(f.rows, name)
			break
		}
	}
	return f.deleteResult, nil
}

// LiveRemoteFlags models store.UserTx.LiveRemoteFlagsByUID: the recorded flags
// of every UID that has live provenance (provFlags, written by the fake
// ingester), absent otherwise.
func (f *fakeStore) LiveRemoteFlags(_ context.Context, _, mailboxID ulid.ULID, _ int64, uids []int64) (map[int64]store.RecordedFlags, error) {
	f.liveFlagCalls++
	r := f.rowByID(mailboxID)
	out := map[int64]store.RecordedFlags{}
	if r == nil {
		return out, nil
	}
	for _, u := range uids {
		if r.prov[u] {
			fl := r.provFlags[u]
			if fl == nil {
				fl = []string{}
			}
			out[u] = store.RecordedFlags{Flags: fl, PendingWriteback: r.pendingWriteback[u]}
		}
	}
	return out, nil
}

// MailboxAcceptsKeywords models the recorded PERMANENTFLAGS probe.
func (f *fakeStore) MailboxAcceptsKeywords(_ context.Context, _, mailboxID ulid.ULID) (bool, error) {
	if r := f.rowByID(mailboxID); r != nil {
		return r.acceptsKeywords, nil
	}
	return false, nil
}

func (f *fakeStore) SetSyncScopeOff(_ context.Context, _, mailboxID ulid.ULID) error {
	if r := f.rowByID(mailboxID); r != nil {
		r.syncScope = "off"
	}
	f.scopeOffCalls = append(f.scopeOffCalls, mailboxID)
	return nil
}

var _ MailboxStore = (*fakeStore)(nil)

// fakeIngester deduplicates by content hash, mirroring the real Ingestor: the
// first sight of a body is Created, repeats are not. When wired to a fakeStore it
// also records the provenance row the real ingest would write, which is what makes
// the gap detector's diff meaningful in the unit tests.
type fakeIngester struct {
	seen  map[[32]byte]bool
	calls []ingest.Provenance
	err   error
	st    *fakeStore

	// refreshCalls records every RefreshFlags call (#523), in order — the
	// flags-only path the CONDSTORE flag-reconcile pass now takes instead of
	// Ingest.
	refreshCalls []ingest.Provenance

	// mutedBodies are bodies the fake reports as muted arrivals (Result.Muted)
	// the first time it creates them — standing in for a reply to a muted
	// conversation, which the real ingestor archives on arrival.
	mutedBodies map[string]bool
	// blockedBodies are bodies the fake reports as arrivals from a blocked
	// sender (Result.Blocked) the first time it creates them (#908).
	blockedBodies map[string]bool
}

func newFakeIngester() *fakeIngester { return &fakeIngester{seen: map[[32]byte]bool{}} }

func (f *fakeIngester) Ingest(_ context.Context, _ ulid.ULID, _ []byte, _ ulid.ULID, raw []byte, prov ingest.Provenance) (ingest.Result, error) {
	if f.err != nil {
		return ingest.Result{}, f.err
	}
	f.calls = append(f.calls, prov)
	if f.st != nil {
		if r := f.st.rowByID(prov.MailboxID); r != nil {
			if r.prov == nil {
				r.prov = map[int64]bool{}
			}
			r.prov[prov.UID] = true
			if r.provFlags == nil {
				r.provFlags = map[int64][]string{}
			}
			r.provFlags[prov.UID] = prov.RemoteFlags
		}
	}
	h := sha256.Sum256(raw)
	created := !f.seen[h]
	f.seen[h] = true
	return ingest.Result{MessageID: newULID(), ContentHash: h, Created: created,
		Muted:   created && f.mutedBodies[string(raw)],
		Blocked: created && !f.mutedBodies[string(raw)] && f.blockedBodies[string(raw)]}, nil
}

// RefreshFlags mirrors store.RefreshLiveProvenanceFlags (#523): Found iff the
// fake store's row already carries live provenance for this (mailbox, uid) —
// the same map fakeStore.MissingUIDs and Ingest read/write, so a UID the
// fallback body-fetch just ingested is immediately found on a later call, and
// a UID with no provenance at all (never seen, or a #93 gap) is not.
func (f *fakeIngester) RefreshFlags(_ context.Context, _ ulid.ULID, prov ingest.Provenance) (ingest.FlagRefresh, error) {
	if f.err != nil {
		return ingest.FlagRefresh{}, f.err
	}
	f.refreshCalls = append(f.refreshCalls, prov)
	if f.st == nil {
		return ingest.FlagRefresh{}, nil
	}
	r := f.st.rowByID(prov.MailboxID)
	if r == nil || !r.prov[prov.UID] {
		return ingest.FlagRefresh{}, nil
	}
	if r.provFlags == nil {
		r.provFlags = map[int64][]string{}
	}
	r.provFlags[prov.UID] = prov.RemoteFlags
	return ingest.FlagRefresh{Found: true, MessageID: newULID(), Changed: true}, nil
}

var _ Ingester = (*fakeIngester)(nil)

// ---- helpers ---------------------------------------------------------------

func newULID() ulid.ULID {
	return ulid.MustNew(ulid.Now(), ulid.DefaultEntropy())
}

func msg(uid sync.UID, modSeq uint64, body string, flags ...sync.Flag) sync.Message {
	return sync.Message{
		MessageMeta: sync.MessageMeta{
			UID: uid, Flags: flags, ModSeq: modSeq,
			InternalDate: time.Unix(int64(uid), 0).UTC(),
		},
		Body: []byte(body),
	}
}

func inbox() sync.Mailbox { return sync.Mailbox{Name: "INBOX", Delimiter: '/'} }

func testReq() AccountSync {
	return AccountSync{
		UserID:    newULID(),
		AccountID: newULID(),
		Config:    sync.Config{Host: "mail.example.com", Port: 993, Username: "u", Password: "p"},
		DEK:       []byte("dek"),
		DEKID:     newULID(),
	}
}

func factoryFor(s *fakeSession) SessionFactory {
	return func(context.Context, sync.Config) (sync.SyncSession, error) { return s, nil }
}

func newEngine(sess *fakeSession, st *fakeStore, ing *fakeIngester) *Engine {
	ing.st = st // ingest writes provenance; the gap detector reads it
	return New(st, ing, factoryFor(sess))
}

func assertCounts(t *testing.T, got Summary, synced, skipped, ingested, flags int) {
	t.Helper()
	if got.MailboxesSynced != synced {
		t.Errorf("MailboxesSynced = %d, want %d", got.MailboxesSynced, synced)
	}
	if got.MailboxesSkipped != skipped {
		t.Errorf("MailboxesSkipped = %d, want %d", got.MailboxesSkipped, skipped)
	}
	if got.MessagesIngested != ingested {
		t.Errorf("MessagesIngested = %d, want %d", got.MessagesIngested, ingested)
	}
	if got.FlagUpdates != flags {
		t.Errorf("FlagUpdates = %d, want %d", got.FlagUpdates, flags)
	}
	if len(got.Errors) != 0 {
		t.Errorf("unexpected errors: %v", got.Errors)
	}
}

// ---- tests -----------------------------------------------------------------

// Fresh sync: an unseen mailbox with three messages fetches the whole
// mailbox (UID 1:*) and ingests all three as new.
func TestSyncAccountFreshAllNew(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, PermanentFlagsAllowNew: true},
				msgs: []sync.Message{
					msg(1, 0, "a", sync.FlagSeen),
					msg(2, 0, "b"),
					msg(3, 0, "c"),
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 3, 0)

	if len(sess.fetches) != 1 {
		t.Fatalf("fetches = %d, want 1", len(sess.fetches))
	}
	if got := sess.fetches[0].uids; len(got) != 1 || got[0].Start != 1 || got[0].Stop != 0 {
		t.Errorf("fresh fetch range = %v, want 1:*", got)
	}
	if !sess.fetches[0].items.BodyFull {
		t.Errorf("fresh fetch must request full body")
	}
	id := st.rows["INBOX"].id
	if c := st.cursorCalls[id]; c.UIDValidity != 10 || c.LastSeenUID != 3 {
		t.Errorf("cursors = %+v, want uidvalidity=10 lastseen=3", c)
	}
	// Provenance carries mailbox + uidvalidity + flags.
	if len(ing.calls) != 3 {
		t.Fatalf("ingest calls = %d, want 3", len(ing.calls))
	}
	if ing.calls[0].MailboxID != id || ing.calls[0].UIDValidity != 10 || ing.calls[0].UID != 1 {
		t.Errorf("provenance[0] = %+v", ing.calls[0])
	}
	if len(ing.calls[0].RemoteFlags) != 1 || ing.calls[0].RemoteFlags[0] != string(sync.FlagSeen) {
		t.Errorf("provenance[0] flags = %v, want [\\Seen]", ing.calls[0].RemoteFlags)
	}
}

// SyncAccountWith runs a pass over a CALLER-OWNED session and must never close
// it — that is the whole basis of syncrunner's connection pool, which reuses one
// connection across passes (mailroom#103 Stage 3). The dial path (SyncAccount),
// by contrast, owns the session it dialed and closes it.
func TestSyncAccountWithDoesNotCloseCallerSession(t *testing.T) {
	newSess := func() *fakeSession {
		return &fakeSession{
			mailboxes: []sync.Mailbox{inbox()},
			boxes: map[string]*fakeBox{
				"INBOX": {
					status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, PermanentFlagsAllowNew: true},
					msgs:   []sync.Message{msg(1, 0, "a"), msg(2, 0, "b")},
				},
			},
		}
	}
	req := testReq()

	// Caller-owned: two passes over the SAME session, never closed and never
	// logged out — the pool keeps it warm for reuse.
	sess := newSess()
	eng := newEngine(sess, newFakeStore(), newFakeIngester())
	for i := 0; i < 2; i++ {
		if _, err := eng.SyncAccountWith(context.Background(), req, sess); err != nil {
			t.Fatalf("SyncAccountWith #%d: %v", i, err)
		}
		if sess.closed {
			t.Fatalf("SyncAccountWith closed the caller's session (pass %d)", i)
		}
	}

	// The dial path owns its session and closes it.
	dialSess := newSess()
	eng2 := newEngine(dialSess, newFakeStore(), newFakeIngester())
	if _, err := eng2.SyncAccount(context.Background(), req); err != nil {
		t.Fatalf("SyncAccount: %v", err)
	}
	if !dialSess.closed {
		t.Error("SyncAccount did not close its dialed session")
	}
}

// Incremental: a mailbox seen up to UID 5 fetches only 6:* and ingests only
// the two genuinely new messages.
func TestSyncAccountIncrementalOnlyNew(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 8},
				msgs: []sync.Message{
					msg(4, 0, "d"), msg(5, 0, "e"), // already seen, must not be fetched
					msg(6, 0, "f"), msg(7, 0, "g"),
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// We already hold UIDs 4 and 5 — the provenance a mailbox polled up to UID 5
	// really has. Without it the gap detector would (rightly) see two gaps.
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 5}, 4, 5)
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 2, 0)

	if len(sess.fetches) != 1 {
		t.Fatalf("fetches = %d, want 1 (no flag pass without CONDSTORE)", len(sess.fetches))
	}
	if got := sess.fetches[0].uids; got[0].Start != 6 || got[0].Stop != 0 {
		t.Errorf("incremental fetch range = %v, want 6:*", got)
	}
	for _, p := range ing.calls {
		if p.UID < 6 {
			t.Errorf("ingested already-seen UID %d", p.UID)
		}
	}
	if c := st.cursorCalls[st.rows["INBOX"].id]; c.LastSeenUID != 7 {
		t.Errorf("lastSeenUID = %d, want 7", c.LastSeenUID)
	}
}

// UIDVALIDITY reset: stored validity differs from the server's, so the
// generation is retired and the mailbox is fully re-fetched from UID 1.
func TestSyncAccountUIDValidityReset(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 99, UIDNext: 3},
				msgs:   []sync.Message{msg(1, 0, "x"), msg(2, 0, "y")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	row := st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 5, highestModSeq: 40}, 1, 2, 3, 4, 5)
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 2, 0)

	if len(st.staleCalls) != 1 || st.staleCalls[0] != row.id {
		t.Fatalf("stale calls = %v, want [%s]", st.staleCalls, row.id)
	}
	if got := sess.fetches[0].uids; got[0].Start != 1 || got[0].Stop != 0 {
		t.Errorf("reset fetch range = %v, want full 1:*", got)
	}
	if c := st.cursorCalls[row.id]; c.UIDValidity != 99 || c.LastSeenUID != 2 {
		t.Errorf("cursors after reset = %+v, want uidvalidity=99 lastseen=2", c)
	}
	// New validity was adopted; provenance reflects it.
	for _, p := range ing.calls {
		if p.UIDValidity != 99 {
			t.Errorf("provenance UIDValidity = %d, want 99", p.UIDValidity)
		}
	}

	// #88: the roll is FINISHED — the retired generation was reconciled, against a
	// corroborating live-UID read bounded by the re-fetch's own UIDNEXT.
	if sum.GenerationsReconciled != 1 {
		t.Fatalf("GenerationsReconciled = %d, want 1 — the retired generation was never reconciled", sum.GenerationsReconciled)
	}
	if len(st.genCalls) != 1 {
		t.Fatalf("generation reconcile calls = %d, want 1", len(st.genCalls))
	}
	obs := st.genCalls[0]
	if obs.MailboxID != row.id || obs.UIDValidity != 99 {
		t.Errorf("observation = %+v, want mailbox %s / uidvalidity 99", obs, row.id)
	}
	if obs.UIDBound != 3 {
		t.Errorf("UIDBound = %d, want 3 (UIDNEXT at the re-fetch's SELECT)", obs.UIDBound)
	}
	if len(obs.ObservedUIDs) != 2 {
		t.Errorf("ObservedUIDs = %v, want the server's live set {1,2}", obs.ObservedUIDs)
	}
	// A rolled mailbox is NOT also reaped: every live row it has was written by
	// the re-fetch that just ran, so there is nothing surplus to find.
	if len(st.reapCalls) != 0 {
		t.Errorf("a rolled mailbox was also reaped: %+v", st.reapCalls)
	}
}

// #88, THE FAILURE THAT DESTROYS MAIL. A UIDVALIDITY roll whose corroborating
// live-UID read FAILS must reconcile nothing, soft-delete nothing, and — the part
// that makes it retryable rather than merely safe — must NOT advance the mailbox's
// cursors, so the next pass still sees the roll.
func TestRollWithFailedCorroborationReconcilesNothing(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:  sync.MailboxStatus{UIDValidity: 99, UIDNext: 3},
				msgs:    []sync.Message{msg(1, 0, "x"), msg(2, 0, "y")},
				liveErr: errors.New("network went away"),
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 5}, 1, 2, 3, 4, 5)

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) == 0 {
		t.Fatal("the failed corroboration was not surfaced")
	}
	if len(st.genCalls) != 0 {
		t.Fatalf("reconciled a retired generation we could not corroborate: %+v", st.genCalls)
	}
	if sum.GenerationsReconciled != 0 || sum.MessagesVanished != 0 {
		t.Errorf("summary claims work we refused to do: %+v", sum)
	}
	if len(st.cursorCalls) != 0 {
		t.Fatalf("CURSORS ADVANCED OVER AN UNFINISHED ROLL: %+v — the next pass will never re-detect it and the orphans stay invisible forever", st.cursorCalls)
	}
}

// A second roll landing between the re-fetch and the corroboration is refused:
// the UID set we would diff against belongs to a generation that has already
// moved on.
func TestGenerationObservationRefusesWhenUIDValidityMovedAgain(t *testing.T) {
	r := rolledMailbox{name: "INBOX", id: newULID(), uidValidity: 99, refetchBound: 3}
	obs, err := generationObservation(r, &sync.MailboxStatus{UIDValidity: 100, UIDNext: 3, NumMessages: 1},
		[]sync.UID{1})
	if err == nil {
		t.Fatalf("reconciled against a generation that moved under us: %+v", obs)
	}
}

// An empty UID set from a mailbox the server's own SELECT says holds messages is
// a broken answer, not a mailbox recreated empty. Acting on it would soft-delete
// the whole folder.
func TestGenerationObservationRefusesEmptyUIDSetWhenServerReportsMessages(t *testing.T) {
	r := rolledMailbox{name: "INBOX", id: newULID(), uidValidity: 99, refetchBound: 3}
	obs, err := generationObservation(r, &sync.MailboxStatus{UIDValidity: 99, UIDNext: 3, NumMessages: 2}, nil)
	if err == nil {
		t.Fatalf("took an empty UID set from a non-empty mailbox at face value: %+v", obs)
	}
}

// No UIDNEXT from the re-fetch's SELECT plus an "empty" mailbox: two zeroes that
// a truncated response produces just as readily as an emptied folder. Refuse —
// this is the one place the code could soft-delete an entire mailbox at once.
func TestGenerationObservationRefusesUncorroboratedEmptyMailbox(t *testing.T) {
	r := rolledMailbox{name: "INBOX", id: newULID(), uidValidity: 99, refetchBound: 0}
	obs, err := generationObservation(r, &sync.MailboxStatus{UIDValidity: 99, UIDNext: 0, NumMessages: 0}, nil)
	if err == nil {
		t.Fatalf("soft-deleted a whole generation on an uncorroborated EXISTS 0: %+v", obs)
	}
}

// A recreated, genuinely empty mailbox IS reconciled — UIDNEXT corroborates it.
// This is the #88 headline scenario, and the reconcile is exactly what turns its
// orphans from invisible into honestly soft-deleted.
func TestGenerationObservationEmptyRecreatedMailboxIsReconciled(t *testing.T) {
	r := rolledMailbox{name: "INBOX", id: newULID(), uidValidity: 99, refetchBound: 1}
	obs, err := generationObservation(r, &sync.MailboxStatus{UIDValidity: 99, UIDNext: 1, NumMessages: 0}, nil)
	if err != nil {
		t.Fatalf("refused a corroborated empty mailbox: %v", err)
	}
	if obs.UIDBound != 1 || len(obs.ObservedUIDs) != 0 {
		t.Errorf("observation = %+v, want bound 1 and an empty UID set", obs)
	}
}

// Without UIDNEXT but with UIDs named, the proof is bounded by the highest UID
// the server named — the strictest reading, so a re-fetch that dropped anything
// the server still lists cannot pass.
func TestGenerationObservationBoundWithoutUIDNext(t *testing.T) {
	r := rolledMailbox{name: "INBOX", id: newULID(), uidValidity: 99, refetchBound: 0}
	obs, err := generationObservation(r, &sync.MailboxStatus{UIDValidity: 99, UIDNext: 0, NumMessages: 2},
		[]sync.UID{4, 7})
	if err != nil {
		t.Fatal(err)
	}
	if obs.UIDBound != 8 {
		t.Errorf("UIDBound = %d, want 8 (max observed UID + 1)", obs.UIDBound)
	}
}

// CONDSTORE flag-only pass: no new UIDs, but HIGHESTMODSEQ advanced, so a
// CHANGEDSINCE fetch over the known range refreshes the one changed message's
// flags (#523: RefreshFlags, not a full re-ingest — the message already has
// live provenance, so no body is fetched at all).
func TestSyncAccountCondStoreFlagPass(t *testing.T) {
	changed := msg(3, 150, "three", sync.FlagSeen, sync.FlagFlagged)
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 6, HighestModSeq: 200},
				msgs: []sync.Message{
					msg(1, 50, "one"), // in range but unchanged (ModSeq <= 100)
					changed,
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// The server holds UIDs 1 and 3; we already hold both (UID 1 is simply unchanged
	// and so is never re-fetched).
	st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 5, highestModSeq: 100}, 1, 3)
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 0, 0, 1)
	if sum.BodiesFetched != 0 || sum.BodyBytesFetched != 0 {
		t.Errorf("BodiesFetched=%d BodyBytesFetched=%d, want 0/0 — every UID here already has live provenance", sum.BodiesFetched, sum.BodyBytesFetched)
	}

	if len(sess.fetches) != 1 {
		t.Fatalf("fetches = %d, want 1 (flag pass only, no new-message pass, no body fallback)", len(sess.fetches))
	}
	f := sess.fetches[0]
	if f.items.ChangedSince != 100 {
		t.Errorf("ChangedSince = %d, want 100", f.items.ChangedSince)
	}
	if f.items.BodyFull || f.items.Envelope || f.items.InternalDate {
		t.Errorf("flag pass requested a body: %+v", f.items)
	}
	if f.uids[0].Start != 1 || f.uids[0].Stop != 5 {
		t.Errorf("flag-pass range = %v, want 1:5 (bounded to known UIDs)", f.uids)
	}
	if len(ing.refreshCalls) != 1 || ing.refreshCalls[0].UID != 3 {
		t.Errorf("refreshCalls = %+v, want only UID 3", ing.refreshCalls)
	}
	if len(ing.calls) != 0 {
		t.Errorf("Ingest called %d times, want 0 (RefreshFlags found live provenance for UID 3)", len(ing.calls))
	}
	if c := st.cursorCalls[st.rows["INBOX"].id]; c.HighestModSeq != 200 || c.LastSeenUID != 5 {
		t.Errorf("cursors = %+v, want highestmodseq=200 lastseen=5", c)
	}
}

// ⚠ THE HEADLINE ASSERTION FOR #523. A flags-only server change — the CONDSTORE
// MODSEQ pass over UIDs we already hold — must fetch NO body, parse nothing,
// and never call Ingest at all: it goes through RefreshFlags exclusively.
//
// PROBE (negative control N1): put BodyFull/Envelope/InternalDate back on the
// flag-pass items and this goes red at the no-body assertion below.
func TestFlagOnlyChangeFetchesNoBody(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, HighestModSeq: 12},
				msgs: []sync.Message{
					msg(1, 10, "one"),
					msg(2, 12, "two", sync.FlagSeen),
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2, highestModSeq: 10}, 1, 2)
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", sum.Errors)
	}

	for _, f := range sess.fetches {
		if f.items.ChangedSince == 0 {
			continue // not the flag pass
		}
		if f.items.BodyFull || f.items.Envelope || f.items.InternalDate {
			t.Fatalf("flags-only fetch requested a body/envelope/internaldate: %+v", f.items)
		}
	}
	if len(ing.refreshCalls) != 1 || ing.refreshCalls[0].UID != 2 {
		t.Fatalf("refreshCalls = %+v, want exactly UID 2", ing.refreshCalls)
	}
	if got := ing.refreshCalls[0].RemoteFlags; len(got) != 1 || got[0] != string(sync.FlagSeen) {
		t.Errorf("refreshed flags = %v, want [\\Seen]", got)
	}
	if len(ing.calls) != 0 {
		t.Errorf("Ingest called %d times, want 0", len(ing.calls))
	}
	if sum.FlagUpdates != 1 || sum.MessagesIngested != 0 || sum.BodiesFetched != 0 || sum.BodyBytesFetched != 0 {
		t.Errorf("summary = %+v, want FlagUpdates=1 MessagesIngested=0 BodiesFetched=0 BodyBytesFetched=0", sum)
	}
}

// A UID the flag pass gets an answer for but that turns out to have NO live
// provenance (reaped out from under the cursor, or a #93 gap) is not silently
// dropped: it falls back to a body fetch, and ONLY that UID is re-fetched with
// a body.
func TestFlagPassFallsBackToBodyOnlyForUIDsWithNoLiveProvenance(t *testing.T) {
	sess := &fakeSession{
		caps:      map[sync.Capability]bool{sync.CapabilityCondStore: true},
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, HighestModSeq: 12},
				msgs: []sync.Message{
					msg(1, 12, "one"),
					msg(3, 12, "three-gap"), // no provenance: a #93 gap case
				},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3, highestModSeq: 10}, 1)
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", sum.Errors)
	}

	if len(ing.refreshCalls) != 2 {
		t.Fatalf("refreshCalls = %+v, want UID 1 and UID 3", ing.refreshCalls)
	}
	var gotRefreshed []int64
	for _, p := range ing.refreshCalls {
		gotRefreshed = append(gotRefreshed, p.UID)
	}
	sort.Slice(gotRefreshed, func(i, j int) bool { return gotRefreshed[i] < gotRefreshed[j] })
	if len(gotRefreshed) != 2 || gotRefreshed[0] != 1 || gotRefreshed[1] != 3 {
		t.Errorf("refreshed UIDs = %v, want [1 3]", gotRefreshed)
	}

	var bodyFetches []recordedFetch
	for _, f := range sess.fetches {
		if f.items.BodyFull {
			bodyFetches = append(bodyFetches, f)
		}
	}
	if len(bodyFetches) != 1 {
		t.Fatalf("body fetches = %d, want exactly 1", len(bodyFetches))
	}
	if got := bodyFetches[0].uids; len(got) != 1 || got[0].Start != 3 || got[0].Stop != 3 {
		t.Errorf("body fetch UID set = %v, want exactly {3}", got)
	}

	if len(ing.calls) != 1 || ing.calls[0].UID != 3 {
		t.Fatalf("Ingest calls = %+v, want exactly one, for UID 3", ing.calls)
	}
	if sum.BodiesFetched != 1 {
		t.Errorf("BodiesFetched = %d, want 1", sum.BodiesFetched)
	}
	wantBytes := int64(len("three-gap"))
	if sum.BodyBytesFetched != wantBytes {
		t.Errorf("BodyBytesFetched = %d, want %d", sum.BodyBytesFetched, wantBytes)
	}
}

// sync_scope='off' short-circuits before Select: the mailbox is skipped, never
// selected, never fetched.
func TestSyncAccountScopeOffSkips(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.rows["INBOX"] = &storedRow{id: newULID(), syncScope: "off"}
	req := testReq()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 0, 1, 0, 0)
	if len(sess.selects) != 0 {
		t.Errorf("selected %v, want no selects for an off mailbox", sess.selects)
	}
	if len(ing.calls) != 0 {
		t.Errorf("ingested %d messages, want 0", len(ing.calls))
	}
}

// A \Noselect container is upserted for its metadata but never selected.
func TestSyncAccountNonSelectableSkipped(t *testing.T) {
	container := sync.Mailbox{Name: "Archive", Delimiter: '/', Attrs: []sync.MailboxAttr{sync.MailboxAttrNoSelect}}
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{container, inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 1, 1, 1, 0)
	for _, name := range sess.selects {
		if name == "Archive" {
			t.Errorf("selected a \\Noselect mailbox")
		}
	}
	if _, ok := st.rows["Archive"]; !ok {
		t.Errorf("non-selectable mailbox was not upserted for its metadata")
	}
}

// Multiple mailboxes plus special-use mapping and display-name derivation.
func TestSyncAccountMultipleMailboxes(t *testing.T) {
	sent := sync.Mailbox{Name: "INBOX/Sent", Delimiter: '/', Attrs: []sync.MailboxAttr{sync.MailboxAttrSent}}
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox(), sent},
		boxes: map[string]*fakeBox{
			"INBOX":      {status: sync.MailboxStatus{UIDValidity: 1, UIDNext: 2}, msgs: []sync.Message{msg(1, 0, "a")}},
			"INBOX/Sent": {status: sync.MailboxStatus{UIDValidity: 2, UIDNext: 2}, msgs: []sync.Message{msg(1, 0, "b")}},
		},
	}
	ing := newFakeIngester()

	// Wrap the store to capture the record passed for the Sent mailbox.
	rec := &recordingStore{fakeStore: newFakeStore()}
	ing.st = rec.fakeStore // ingest writes provenance; the gap detector reads it
	sum, err := New(rec, ing, factoryFor(sess)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, sum, 2, 0, 2, 0)
	if got := rec.records["INBOX/Sent"]; got.SpecialUse != string(sync.MailboxAttrSent) || got.DisplayName != "Sent" {
		t.Errorf("Sent record = %+v, want special_use=\\Sent display=Sent", got)
	}
}

// recordingStore captures the MailboxRecord passed to UpsertMailbox.
type recordingStore struct {
	*fakeStore
	records map[string]MailboxRecord
}

func (r *recordingStore) UpsertMailbox(ctx context.Context, userID ulid.ULID, rec MailboxRecord) (StoredMailbox, error) {
	if r.records == nil {
		r.records = map[string]MailboxRecord{}
	}
	r.records[rec.IMAPName] = rec
	return r.fakeStore.UpsertMailbox(ctx, userID, rec)
}

// ---- error paths -----------------------------------------------------------

func TestSyncAccountDialError(t *testing.T) {
	dialErr := errors.New("boom")
	eng := New(newFakeStore(), newFakeIngester(),
		func(context.Context, sync.Config) (sync.SyncSession, error) { return nil, dialErr })
	_, err := eng.SyncAccount(context.Background(), testReq())
	if !errors.Is(err, dialErr) {
		t.Fatalf("err = %v, want wrapped dial error", err)
	}
}

func TestSyncAccountListError(t *testing.T) {
	sess := &fakeSession{listErr: errors.New("list failed")}
	_, err := newEngine(sess, newFakeStore(), newFakeIngester()).SyncAccount(context.Background(), testReq())
	if err == nil || !errors.Is(err, sess.listErr) {
		t.Fatalf("err = %v, want wrapped list error", err)
	}
}

// An ingest failure is recorded per-mailbox and leaves the cursors unadvanced
// (safe re-fetch next run), without aborting the whole account.
func TestSyncAccountIngestErrorIsNonFatal(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st := newFakeStore()
	ing := newFakeIngester()
	ing.err = errors.New("ingest exploded")

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("SyncAccount returned fatal error: %v", err)
	}
	if sum.MailboxesSynced != 0 || len(sum.Errors) != 1 {
		t.Fatalf("summary = %+v, want 0 synced and 1 error", sum)
	}
	if len(st.cursorCalls) != 0 {
		t.Errorf("cursors advanced despite ingest failure: %v", st.cursorCalls)
	}
}

// A message fetched without a body is a hard error for that mailbox.
func TestSyncAccountMissingBody(t *testing.T) {
	bodyless := sync.Message{MessageMeta: sync.MessageMeta{UID: 1}} // Body nil
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2}, msgs: []sync.Message{bodyless}},
		},
	}
	st := newFakeStore()
	sum, err := newEngine(sess, st, newFakeIngester()).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want 1 (missing body)", sum.Errors)
	}
}

func TestSyncAccountValidation(t *testing.T) {
	eng := New(newFakeStore(), newFakeIngester(), factoryFor(&fakeSession{}))
	cases := map[string]AccountSync{
		"no user":    {AccountID: newULID(), DEK: []byte("k"), DEKID: newULID()},
		"no account": {UserID: newULID(), DEK: []byte("k"), DEKID: newULID()},
		"no dek":     {UserID: newULID(), AccountID: newULID(), DEKID: newULID()},
		"no dek id":  {UserID: newULID(), AccountID: newULID(), DEK: []byte("k")},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := eng.SyncAccount(context.Background(), req); err == nil {
				t.Errorf("expected validation error")
			}
		})
	}
}

// ---- the expunge reaper (#86) ----------------------------------------------
//
// Every test in this section is about one question: under exactly which
// conditions is Mailroom allowed to conclude that a message is GONE? Getting
// that wrong deletes the user's mail, so the cases that must reap NOTHING
// outnumber the ones that reap.

// hoursAgo returns a *time.Time n hours before now, for a last_reaped_at cursor.
func hoursAgo(n int) *time.Time {
	t := time.Now().Add(-time.Duration(n) * time.Hour)
	return &t
}

// reapEngine builds an Engine with the reaper on a 1h cadence and a fixed clock.
func reapEngine(sess *fakeSession, st *fakeStore, ing *fakeIngester) *Engine {
	ing.st = st
	return New(st, ing, factoryFor(sess), WithReapInterval(time.Hour))
}

// A message the server no longer has: its provenance is reaped, bounded by the
// server's own UIDNEXT and carrying exactly the UID set the server reported.
func TestReapDropsProvenanceForVanishedUIDs(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			// The server holds UIDs 1 and 3; UID 2 was expunged elsewhere.
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 2},
				msgs:   []sync.Message{msg(1, 0, "a"), msg(3, 0, "c")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// We hold 1, 2 and 3; the server has since expunged 2.
	st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3, lastReapedAt: hoursAgo(2)}, 1, 2, 3)
	st.reapResult = store.ReapResult{ProvenanceDropped: 1, MembershipChanged: 1, MessagesVanished: 1}
	st.retireCount = 1

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.reapCalls) != 1 {
		t.Fatalf("reap calls = %d, want 1", len(st.reapCalls))
	}
	obs := st.reapCalls[0]
	if obs.MailboxID != st.rows["INBOX"].id {
		t.Errorf("reaped mailbox = %s, want INBOX", obs.MailboxID)
	}
	if obs.UIDValidity != 10 {
		t.Errorf("reap UIDVALIDITY = %d, want 10", obs.UIDValidity)
	}
	if obs.UIDBound != 4 {
		t.Errorf("reap bound = %d, want 4 (the server's UIDNEXT)", obs.UIDBound)
	}
	if len(obs.ObservedUIDs) != 2 || obs.ObservedUIDs[0] != 1 || obs.ObservedUIDs[1] != 3 {
		t.Errorf("observed UIDs = %v, want [1 3]", obs.ObservedUIDs)
	}
	if sum.MailboxesReaped != 1 || sum.ProvenanceReaped != 1 || sum.MessagesVanished != 1 {
		t.Errorf("summary = %+v, want 1 reaped / 1 dropped / 1 vanished", sum)
	}
	if sum.PlacementsRetired != 1 || st.retireCalls != 1 {
		t.Errorf("retirement sweep did not run exactly once: retired=%d calls=%d", sum.PlacementsRetired, st.retireCalls)
	}
}

// The cadence is real: a mailbox reaped ten minutes ago is not asked again on
// the next five-minute poll.
func TestReapNotDueIsSkipped(t *testing.T) {
	recent := time.Now().Add(-10 * time.Minute)
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 1, lastReapedAt: &recent}, 1)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.liveCalls) != 0 {
		t.Errorf("LiveUIDs called %v for a mailbox that is not due", sess.liveCalls)
	}
	if len(st.reapCalls) != 0 || st.retireCalls != 0 || sum.MailboxesReaped != 0 {
		t.Errorf("reaped despite not being due: %+v", sum)
	}
}

// WithReapInterval(0) is the operator's explicit off switch — for the REAPER, and
// for the reaper alone. Not one provenance row may be dropped. But the live-UID
// observation still happens, because the SAME answer is what detects a message our
// own FETCH silently lost (#93), and "never delete a provenance row" is not a
// request to "lose my mail to a truncated response forever".
func TestReapDisabledStillDetectsGaps(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.st = st

	sum, err := New(st, ing, factoryFor(sess), WithReapInterval(0)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.reapCalls) != 0 || sum.MailboxesReaped != 0 || sum.ProvenanceReaped != 0 {
		t.Errorf("THE REAPER RAN WHILE DISABLED: reaps=%d summary=%+v", len(st.reapCalls), sum)
	}
	if len(sess.liveCalls) != 1 {
		t.Fatalf("live-UID reads = %v, want 1 — disabling the reaper must not disable gap detection", sess.liveCalls)
	}
	if len(st.gapCalls) != 1 {
		t.Errorf("gap detection ran %d times, want 1", len(st.gapCalls))
	}
	if sum.MailboxesVerified != 1 {
		t.Errorf("MailboxesVerified = %d, want 1", sum.MailboxesVerified)
	}
	// The cadence cursor still moves: the observation happened and was acted on, so
	// an empty/clean mailbox must not re-issue a UID SEARCH ALL on every poll.
	if len(st.stampCalls) != 1 {
		t.Errorf("observation cursor stamped %d times, want 1", len(st.stampCalls))
	}
}

// THE ONE THAT MATTERS. A mailbox whose POLL failed is not a reap candidate at
// all: we have no trustworthy view of it, so nothing about it may be concluded
// absent. Everything the poll DID read stays exactly where it is.
func TestReapNeverRunsForAMailboxWhosePollFailed(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox(), {Name: "Archive", Delimiter: '/'}},
		boxes: map[string]*fakeBox{
			// INBOX's fetch will blow up on the bodyless message.
			"INBOX":   {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{{MessageMeta: sync.MessageMeta{UID: 1}}}},
			"Archive": {status: sync.MailboxStatus{UIDValidity: 20, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want 1 (the failed INBOX poll)", sum.Errors)
	}
	for _, name := range sess.liveCalls {
		if name == "INBOX" {
			t.Fatal("asked for the live UID set of a mailbox whose poll FAILED — that is how mail gets deleted")
		}
	}
	for _, obs := range st.reapCalls {
		if obs.MailboxID == st.rows["INBOX"].id {
			t.Fatal("reaped a mailbox whose poll failed")
		}
	}
	// The healthy mailbox is still reaped — one bad folder does not stop the rest.
	if sum.MailboxesReaped != 1 {
		t.Errorf("MailboxesReaped = %d, want 1 (Archive)", sum.MailboxesReaped)
	}
}

// A failed LiveUIDs is "we failed to ask", never "the server said it's gone".
func TestReapLiveUIDsFailureReapsNothing(t *testing.T) {
	boom := errors.New("connection reset by peer")
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:  sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1},
				msgs:    []sync.Message{msg(1, 0, "a")},
				liveErr: boom,
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.reapCalls) != 0 {
		t.Fatal("reaped after failing to read the server's UID set")
	}
	if st.retireCalls != 0 {
		t.Error("retired canonical placements after a failed reap")
	}
	if sum.MailboxesReaped != 0 {
		t.Errorf("MailboxesReaped = %d, want 0", sum.MailboxesReaped)
	}
	if len(sum.Errors) != 1 || !errors.Is(sum.Errors[0], boom) {
		t.Fatalf("errors = %v, want the wrapped LiveUIDs failure", sum.Errors)
	}
	// The mailbox still synced — a broken reap degrades the reap, not the sync.
	if sum.MailboxesSynced != 1 {
		t.Errorf("MailboxesSynced = %d, want 1", sum.MailboxesSynced)
	}
}

// An empty UID set from a mailbox the server just said holds messages is a
// BROKEN ANSWER, not an emptied mailbox. Acting on it would delete the folder.
func TestReapRefusesEmptyUIDSetWhenServerReportsMessages(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:      sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				msgs:        []sync.Message{msg(1, 0, "a"), msg(2, 0, "b")},
				liveStatus:  &sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				liveUIDs:    []sync.UID{},
				liveUIDsSet: true,
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.reapCalls) != 0 {
		t.Fatal("acted on an empty UID set from a mailbox the server said holds 2 messages")
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want 1 (refused untrustworthy answer)", sum.Errors)
	}
}

// A genuinely empty mailbox (the server agrees: EXISTS 0) IS reapable — that is
// the whole point of the previous test's guard being conditional.
func TestReapEmptyMailboxIsReaped(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:      sync.MailboxStatus{UIDValidity: 10, UIDNext: 9, NumMessages: 0},
				liveStatus:  &sync.MailboxStatus{UIDValidity: 10, UIDNext: 9, NumMessages: 0},
				liveUIDs:    []sync.UID{},
				liveUIDsSet: true,
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 8})

	if _, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	if len(st.reapCalls) != 1 {
		t.Fatalf("reap calls = %d, want 1", len(st.reapCalls))
	}
	obs := st.reapCalls[0]
	if len(obs.ObservedUIDs) != 0 {
		t.Errorf("observed = %v, want empty", obs.ObservedUIDs)
	}
	if obs.UIDBound != 9 {
		t.Errorf("bound = %d, want 9 (UIDNEXT) — everything below it is provably gone", obs.UIDBound)
	}
}

// UIDVALIDITY moving between the poll and the reap invalidates the whole UID
// space: every UID we hold would look "gone". Refuse.
func TestReapRefusesWhenUIDValidityMoved(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:     sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				msgs:       []sync.Message{msg(1, 0, "a"), msg(2, 0, "b")},
				liveStatus: &sync.MailboxStatus{UIDValidity: 11, UIDNext: 3}, // recreated under us
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2}, 1, 2)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.reapCalls) != 0 {
		t.Fatal("reaped against a UIDVALIDITY generation that is no longer the server's")
	}
	// And it concluded nothing in the OTHER direction either: a UID set from a
	// generation that is not ours cannot be read as "these UIDs are missing" any
	// more than as "these UIDs are gone".
	if len(st.gapCalls) != 0 {
		t.Fatalf("looked for gaps against a stale coordinate space: %+v", st.gapCalls)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want 1", sum.Errors)
	}
}

// No UIDNEXT *and* an empty mailbox is the one answer we must not believe.
//
// Believing it means reaping every provenance row in the folder — the most
// destructive thing the reaper can do — and the two fields that would have to be
// wrong for that to be a mistake are BOTH already zero: no UIDNEXT was parsed,
// and NumMessages is the zero value whether the server said "EXISTS 0" or the
// response was malformed and nothing was parsed at all. A truncated SELECT plus a
// truncated SEARCH is, at that point, indistinguishable from a genuinely emptied
// mailbox — and one of those two readings destroys the user's mail. Refuse.
func TestReapObservationRefusesUncorroboratedEmptyMailbox(t *testing.T) {
	empty := reapTarget{name: "INBOX", id: newULID(), uidValidity: 10}
	obs, err := reapObservation(empty, &sync.MailboxStatus{UIDValidity: 10, UIDNext: 0, NumMessages: 0}, nil)
	if err == nil {
		t.Fatalf("obs = %+v, err = nil; want a refusal — an EXISTS 0 with no UIDNEXT is not enough "+
			"evidence to wipe every provenance row in the folder", obs)
	}
	if obs != nil {
		t.Errorf("obs = %+v, want nil alongside the refusal", obs)
	}
}

// A server that omits UIDNEXT but DOES name UIDs is corroborated: SEARCH ALL is
// complete, so everything at or below the highest UID it named is covered, and
// everything above it is left alone.
func TestReapObservationBoundWithoutUIDNext(t *testing.T) {
	empty := reapTarget{name: "INBOX", id: newULID(), uidValidity: 10}
	obs, err := reapObservation(empty, &sync.MailboxStatus{UIDValidity: 10, UIDNext: 0, NumMessages: 2},
		[]sync.UID{3, 7})
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.UIDBound != 8 {
		t.Fatalf("obs = %+v, want bound 8 (max observed + 1)", obs)
	}
}

// A UID space with nothing in it yet (UIDNEXT 1) can prove nothing absent.
func TestReapObservationEmptyUIDSpaceIsANoOp(t *testing.T) {
	obs, err := reapObservation(
		reapTarget{name: "INBOX", id: newULID(), uidValidity: 10},
		&sync.MailboxStatus{UIDValidity: 10, UIDNext: 1, NumMessages: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil {
		t.Fatalf("obs = %+v, want nil (nothing below UID 1 can exist)", obs)
	}
}

// ORDERING. Every mailbox is POLLED before any mailbox is REAPED. Without that,
// a message another client moved from INBOX to Archive can be reaped out of
// INBOX before Archive has been looked at — and briefly exist nowhere.
func TestReapRunsAfterEveryPoll(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox(), {Name: "Archive", Delimiter: '/'}},
		boxes: map[string]*fakeBox{
			"INBOX":   {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
			"Archive": {status: sync.MailboxStatus{UIDValidity: 20, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "b")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()

	if _, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq()); err != nil {
		t.Fatal(err)
	}
	var lastFetch, firstLive = -1, len(sess.calls)
	for i, c := range sess.calls {
		if len(c) > 6 && c[:6] == "fetch:" {
			lastFetch = i
		}
		if len(c) > 5 && c[:5] == "live:" && i < firstLive {
			firstLive = i
		}
	}
	if lastFetch == -1 || firstLive == len(sess.calls) {
		t.Fatalf("expected both fetches and live-UID reads, got %v", sess.calls)
	}
	if firstLive < lastFetch {
		t.Fatalf("a reap ran before the last poll finished: %v", sess.calls)
	}
}

func TestReapDue(t *testing.T) {
	id := newULID()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-time.Minute)
	if !reapDue(id, nil, now, time.Hour) {
		t.Error("a never-reaped mailbox must be due")
	}
	if !reapDue(id, &old, now, time.Hour) {
		t.Error("a mailbox reaped 2h ago must be due on an hourly cadence")
	}
	if reapDue(id, &recent, now, time.Hour) {
		t.Error("a mailbox reaped a minute ago must not be due")
	}
}

// ---- the cadence jitter (#638) ---------------------------------------------
//
// The per-mailbox cadence does NOT by itself spread a big account's observations:
// every mailbox observed in one pass is stamped within seconds of the others, so
// with a flat interval the whole cohort comes due together again an hour later and
// tries to issue its whole cohort of UID SEARCHes at once. Measured in production:
// 55 of 90 passes had SEARCHes refused, in hourly waves of up to 215 (#638).

// TestReapDueJitterSpreadsACohort is the mechanism: mailboxes stamped at the SAME
// instant must not all come due at the same instant.
func TestReapDueJitterSpreadsACohort(t *testing.T) {
	const interval = time.Hour
	stamped := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// A cohort of mailboxes, all observed in one pass, as a first sync produces.
	ids := make([]ulid.ULID, 200)
	for i := range ids {
		ids[i] = newULID()
	}

	// Walk the hour after the flat due time in one-minute steps and record, for
	// each mailbox, the first step at which it becomes due.
	dueAt := map[ulid.ULID]int{}
	for _, id := range ids {
		for min := 0; min <= 20; min++ {
			if reapDue(id, &stamped, stamped.Add(interval+time.Duration(min)*time.Minute), interval) {
				dueAt[id] = min
				break
			}
		}
	}
	if len(dueAt) != len(ids) {
		t.Fatalf("%d of %d mailboxes were still not due 20 minutes past the interval; "+
			"the jitter must stay within 20%%", len(ids)-len(dueAt), len(ids))
	}

	distinct := map[int]bool{}
	for _, min := range dueAt {
		distinct[min] = true
	}
	// Without jitter every mailbox becomes due in the same step and this is 1.
	if len(distinct) < 10 {
		t.Errorf("a 200-mailbox cohort came due across only %d distinct minutes; "+
			"they are still re-forming as one burst", len(distinct))
	}
}

// TestReapJitterIsStableAndBounded: the offset is a property OF THE MAILBOX, not
// of the moment. Randomness would re-roll every pass and smear a mailbox's
// cadence instead of phasing it; an unbounded offset would silently stretch the
// operator's configured interval.
func TestReapJitterIsStableAndBounded(t *testing.T) {
	const interval = time.Hour
	maxJitter := interval / 5

	for i := 0; i < 500; i++ {
		id := newULID()
		first := reapJitter(id, interval)
		if got := reapJitter(id, interval); got != first {
			t.Fatalf("reapJitter(%s) returned %v then %v — it must be deterministic", id, first, got)
		}
		if first < 0 || first > maxJitter {
			t.Fatalf("reapJitter(%s) = %v, want within [0, %v] (20%% of the interval)", id, first, maxJitter)
		}
	}

	// Two different mailboxes must generally get different offsets — the point.
	a, b := newULID(), newULID()
	if reapJitter(a, interval) == reapJitter(b, interval) {
		t.Errorf("two distinct mailbox ids got the same offset; a 1-in-%d coincidence, or no jitter at all",
			maxJitter/time.Nanosecond)
	}

	// A zero/negative interval has no 20% to take, and must not divide by zero.
	if got := reapJitter(newULID(), 0); got != 0 {
		t.Errorf("reapJitter with a zero interval = %v, want 0", got)
	}
}

// TestReapDueJitterNeverDelaysAnOverdueMailbox is the bound that matters to the
// service level: the jitter only ever ADDS up to 20%, so a mailbox well past its
// interval is due regardless of which offset it drew.
func TestReapDueJitterNeverDelaysAnOverdueMailbox(t *testing.T) {
	const interval = time.Hour
	stamped := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	overdue := stamped.Add(interval + interval/5 + time.Second)

	for i := 0; i < 500; i++ {
		id := newULID()
		if !reapDue(id, &stamped, overdue, interval) {
			t.Fatalf("mailbox %s was not due %v after its last observation (offset %v)",
				id, overdue.Sub(stamped), reapJitter(id, interval))
		}
		if !reapDue(id, nil, stamped, interval) {
			t.Fatalf("mailbox %s has never been observed and must be due whatever its offset", id)
		}
	}
}

// ---- the gap backfill (#93) -------------------------------------------------
//
// The reaper asks "what did we ingest that the server no longer has?". These ask
// the mirror question, which until #93 nothing asked at all: "what does the server
// have that we never ingested?" — because a FETCH that silently drops a middle UID
// returns no error, the last-seen cursor advances past the dropped message anyway,
// and nothing ever asks for it again.

// THE BUG. A poll whose FETCH silently drops a middle UID leaves that message on
// the server and below our cursor, unreachable forever. The next pass must find it
// (the server lists it; we hold no provenance row for it) and re-fetch it.
func TestGapBelowTheCursorIsRefetched(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			// The server holds 1, 2 and 3. We hold 1 and 3: a previous poll's FETCH
			// dropped UID 2 without error, and the cursor advanced to 3 regardless.
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 3},
				msgs:   []sync.Message{msg(1, 0, "a"), msg(2, 0, "b"), msg(3, 0, "c")},
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	row := st.seedRow("INBOX",
		&storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3}, 1, 3)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", sum.Errors)
	}
	if sum.GapsDetected != 1 || sum.GapsBackfilled != 1 {
		t.Fatalf("GapsDetected=%d GapsBackfilled=%d, want 1/1 — the lost message was never found", sum.GapsDetected, sum.GapsBackfilled)
	}
	if sum.MessagesIngested != 1 {
		t.Errorf("MessagesIngested = %d, want 1 (the recovered message)", sum.MessagesIngested)
	}
	// It was fetched by UID, from the right mailbox, in the right generation.
	var got []int64
	for _, c := range ing.calls {
		got = append(got, c.UID)
	}
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("ingested UIDs = %v, want exactly [2]", got)
	}
	if p := ing.calls[0]; p.MailboxID != row.id || p.UIDValidity != 10 {
		t.Errorf("recovered provenance = %+v, want mailbox %s / uidvalidity 10", p, row.id)
	}
	// The gap query was bounded by BOTH the server's UIDNEXT and our own cursor.
	if len(st.gapCalls) != 1 {
		t.Fatalf("gap detection ran %d times, want 1", len(st.gapCalls))
	}
	if b := st.gapCalls[0].UIDBound; b != 4 {
		t.Errorf("gap bound = %d, want 4 (min of UIDNEXT 4 and lastSeen+1 = 4)", b)
	}
	// The cursor is NOT moved by a backfill — every UID in it is below the cursor.
	if c, ok := st.cursorCalls[row.id]; ok && c.LastSeenUID != 3 {
		t.Errorf("the backfill moved the last-seen cursor to %d", c.LastSeenUID)
	}
	// And with the gap closed, the mailbox is verified and reaped as usual.
	if sum.MailboxesVerified != 1 || sum.MailboxesReaped != 1 {
		t.Errorf("summary = %+v, want the mailbox verified and reaped", sum)
	}
}

// FAIL CLOSED. A mailbox we KNOW we are missing a message from is a mailbox we
// have an incomplete view of, and an incomplete view is not a licence to delete
// rows from it. Nothing is reaped, nothing is stamped, and it is due again on the
// very next pass — which is what makes recovery poll-speed rather than reap-speed.
func TestOpenGapBlocksTheReapAndKeepsTheMailboxDue(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:  sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 3},
				msgs:    []sync.Message{msg(1, 0, "a"), msg(2, 0, "b"), msg(3, 0, "c")},
				dropUID: 2, // the FETCH keeps losing it, silently
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3}, 1, 3)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.GapsDetected != 1 {
		t.Fatalf("GapsDetected = %d, want 1", sum.GapsDetected)
	}
	if sum.GapsBackfilled != 0 {
		t.Fatalf("GapsBackfilled = %d, but the fetch drops that UID every time", sum.GapsBackfilled)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want 1 — an un-closeable gap must be surfaced, not swallowed", sum.Errors)
	}
	if len(st.reapCalls) != 0 || sum.MailboxesReaped != 0 {
		t.Fatal("REAPED A MAILBOX WE KNOW WE ARE MISSING A MESSAGE FROM")
	}
	if sum.MailboxesVerified != 0 {
		t.Errorf("MailboxesVerified = %d: a mailbox with an open gap is not verified", sum.MailboxesVerified)
	}
	if len(st.stampCalls) != 0 {
		t.Fatal("stamped the observation cursor over an unclosed gap — the mailbox would not be re-checked for an hour")
	}
}

// A UID at or ABOVE the last-seen cursor is not a gap: the next poll's
// `lastSeen+1:*` range asks for it anyway, so its absence proves nothing and
// re-fetching it here would duplicate work. Only what the cursor has moved PAST is
// unreachable, and only that is #93.
func TestGapDetectionStopsAtTheCursor(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			// UIDNEXT 5: the server holds 1..4. Our poll only ever covered up to 2 (its
			// FETCH range ends there), so 3 and 4 are ordinary un-polled mail.
			"INBOX": {
				status:      sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				msgs:        []sync.Message{msg(1, 0, "a"), msg(2, 0, "b")},
				liveStatus:  &sync.MailboxStatus{UIDValidity: 10, UIDNext: 5, NumMessages: 4},
				liveUIDs:    []sync.UID{1, 2, 3, 4}, // 3 and 4 arrived after the poll
				liveUIDsSet: true,
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2}, 1, 2)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if sum.GapsDetected != 0 {
		t.Fatalf("GapsDetected = %d: mail that simply has not been polled yet is not a gap", sum.GapsDetected)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("errors: %v", sum.Errors)
	}
	if b := st.gapCalls[0].UIDBound; b != 3 {
		t.Errorf("gap bound = %d, want 3 (lastSeen+1, NOT the server's UIDNEXT 5)", b)
	}
	if sum.MailboxesVerified != 1 || sum.MailboxesReaped != 1 {
		t.Errorf("summary = %+v, want verified and reaped", sum)
	}
}

// An untrustworthy answer is not read as "these UIDs are missing" any more than as
// "these UIDs are gone". The gap detector is not even consulted.
func TestGapDetectionRefusesABrokenUIDSet(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:      sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				msgs:        []sync.Message{msg(1, 0, "a"), msg(2, 0, "b")},
				liveStatus:  &sync.MailboxStatus{UIDValidity: 10, UIDNext: 3, NumMessages: 2},
				liveUIDs:    []sync.UID{}, // EXISTS 2, SEARCH says nothing: broken
				liveUIDsSet: true,
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 2}, 1, 2)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.gapCalls) != 0 || len(st.reapCalls) != 0 {
		t.Fatal("acted on a UID set the server's own EXISTS contradicts")
	}
	if sum.GapsDetected != 0 || sum.GapsBackfilled != 0 || len(sum.Errors) != 1 {
		t.Fatalf("summary = %+v, want a refusal and nothing else", sum)
	}
}

// A failed live-UID read backfills nothing, exactly as it reaps nothing: "we
// failed to ask" is not "the server has a message we are missing" either.
func TestGapDetectionFailedLiveUIDsBackfillsNothing(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status:  sync.MailboxStatus{UIDValidity: 10, UIDNext: 4, NumMessages: 3},
				msgs:    []sync.Message{msg(1, 0, "a"), msg(2, 0, "b"), msg(3, 0, "c")},
				liveErr: errors.New("connection reset by peer"),
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3}, 1, 3)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.gapCalls) != 0 || len(ing.calls) != 0 || len(st.stampCalls) != 0 {
		t.Fatal("concluded something about a mailbox we could not ask about")
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want the wrapped LiveUIDs failure", sum.Errors)
	}
}

// gapBound is the pure rule: min(what the observation covers, what the poll
// covered), and nothing at all before the poll has covered anything.
func TestGapBound(t *testing.T) {
	obs := &store.ReapObservation{UIDBound: 100}
	cases := []struct {
		name     string
		obs      *store.ReapObservation
		lastSeen int64
		want     int64
	}{
		{"cursor below the observation", obs, 40, 41},
		{"observation below the cursor", obs, 400, 100},
		{"exactly at the bound", obs, 99, 100},
		{"never polled", obs, 0, 0},
		{"nothing the observation could cover", nil, 40, 0},
	}
	for _, c := range cases {
		if got := gapBound(c.obs, c.lastSeen); got != c.want {
			t.Errorf("%s: gapBound = %d, want %d", c.name, got, c.want)
		}
	}
}

// Consecutive missing UIDs coalesce into ranges, so a truncated chunk costs one
// range rather than one per message.
func TestUIDSetOf(t *testing.T) {
	got := uidSetOf([]int64{2, 3, 4, 9, 11, 12})
	want := sync.UIDSet{{Start: 2, Stop: 4}, {Start: 9, Stop: 9}, {Start: 11, Stop: 12}}
	if len(got) != len(want) {
		t.Fatalf("uidSetOf = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uidSetOf = %v, want %v", got, want)
		}
	}
	if s := uidSetOf(nil); len(s) != 0 {
		t.Errorf("uidSetOf(nil) = %v, want empty", s)
	}
}

func TestDisplayName(t *testing.T) {
	cases := []struct {
		name  string
		delim rune
		want  string
	}{
		{"INBOX", '/', "INBOX"},
		{"INBOX/Work", '/', "Work"},
		{"a.b.c", '.', "c"},
		{"NoDelim", 0, "NoDelim"},
		{"Trailing/", '/', "Trailing/"},
	}
	for _, c := range cases {
		if got := displayName(c.name, c.delim); got != c.want {
			t.Errorf("displayName(%q, %q) = %q, want %q", c.name, c.delim, got, c.want)
		}
	}
}

// ---- folder deletion (#87) --------------------------------------------------
//
// The reaper is a message leaving a folder. These are a whole FOLDER leaving the
// server: a mailbox we hold that a SUCCESSFUL LIST no longer names.

// A folder we hold that the server's LIST no longer reports is reconciled and
// destroyed; the folders still on the server are untouched, and the retirement
// sweep runs because a provenance row moved.
func TestDeletedFolderIsReconciledAndDestroyed(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()}, // the LIST no longer names "Receipts"
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// INBOX is still on the server (and gets polled/upserted). "Receipts" is a
	// folder we hold that the server no longer lists — it was deleted elsewhere.
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 1, lastReapedAt: hoursAgo(2)}, 1)
	gone := st.seedRow("Receipts", &storedRow{id: newULID(), syncScope: "full", uidValidity: 7, lastSeenUID: 5}, 5)
	st.deleteResult = store.DeletedMailboxResult{ProvenanceDropped: 2, MembershipChanged: 2, MessagesVanished: 1}
	st.retireCount = 1

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", sum.Errors)
	}
	if len(st.deleteCalls) != 1 || st.deleteCalls[0] != gone.id {
		t.Fatalf("deleteCalls = %v, want exactly the deleted Receipts folder %s", st.deleteCalls, gone.id)
	}
	if sum.MailboxesDeleted != 1 {
		t.Errorf("MailboxesDeleted = %d, want 1", sum.MailboxesDeleted)
	}
	// Its fallout is folded into the shared reaper counters.
	if sum.ProvenanceReaped < 2 || sum.MessagesVanished < 1 {
		t.Errorf("summary = %+v, want the deleted folder's drops/vanishes counted", sum)
	}
	// The server no longer named it, so it must NOT have been asked about (no reap /
	// live-UID read against a folder that is gone).
	for _, name := range sess.liveCalls {
		if name == "Receipts" {
			t.Error("asked the server about a folder it no longer lists")
		}
	}
	// A provenance row moved, so the placement-retirement sweep ran.
	if st.retireCalls != 1 || sum.PlacementsRetired != 1 {
		t.Errorf("retirement sweep: calls=%d retired=%d, want 1/1", st.retireCalls, sum.PlacementsRetired)
	}
}

// THE ONE THAT MATTERS. A successful LIST that does not even name INBOX is a
// broken/partial answer, not "every folder was deleted". Nothing is reconciled —
// INBOX (which cannot be deleted) is emphatically not torn down — and the refusal
// is surfaced.
func TestDeletedFolderRefusesAListWithoutINBOX(t *testing.T) {
	sess := &fakeSession{
		// A partial LIST: it names Receipts but has dropped INBOX. INBOX cannot be
		// deleted (RFC 9051), so this answer cannot be trusted.
		mailboxes: []sync.Mailbox{{Name: "Receipts", Delimiter: '/'}},
		boxes: map[string]*fakeBox{
			"Receipts": {status: sync.MailboxStatus{UIDValidity: 7, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	// We hold INBOX — which the broken LIST omits. It must survive.
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 3, lastReapedAt: hoursAgo(2)}, 1, 2, 3)

	sum, err := reapEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.deleteCalls) != 0 {
		t.Fatal("TORE DOWN A FOLDER ON THE STRENGTH OF A LIST THAT DROPPED INBOX")
	}
	if sum.MailboxesDeleted != 0 {
		t.Errorf("MailboxesDeleted = %d, want 0", sum.MailboxesDeleted)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("errors = %v, want 1 (the refused untrustworthy LIST)", sum.Errors)
	}
}

// WithReapInterval(0) disables folder-deletion reconciliation too: it deletes
// provenance rows, and that switch is "keep stale membership rather than delete a
// provenance row". A folder the server dropped stays, stale, rather than being
// destroyed.
func TestDeletedFolderNotReconciledWhenReapingDisabled(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1}, msgs: []sync.Message{msg(1, 0, "a")}},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()
	ing.st = st
	st.seedRow("INBOX", &storedRow{id: newULID(), syncScope: "full", uidValidity: 10, lastSeenUID: 1}, 1)
	st.seedRow("Receipts", &storedRow{id: newULID(), syncScope: "full", uidValidity: 7, lastSeenUID: 5}, 5)

	sum, err := New(st, ing, factoryFor(sess), WithReapInterval(0)).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.deleteCalls) != 0 || sum.MailboxesDeleted != 0 {
		t.Fatalf("reconciled a deleted folder while the reaper is OFF: calls=%v summary=%+v", st.deleteCalls, sum)
	}
}

// ⚠ THE ASSERTION THAT DISTINGUISHES STREAMING FROM BUFFERING (#351).
//
// SyncSession.FetchByUID used to return []Message: the whole range was read
// into memory, and only then ingested. Two consequences, and this test pins the
// observable one.
//
// The unobservable one was the reason for the change: a first sync — and every
// UIDVALIDITY roll, which rewinds the cursor to 0 — fetches an entire mailbox
// WITH BODIES, so peak memory was ~2x the mailbox's raw bytes. Spec §18 sizes
// for archives of "tens of GB per user" on 16-32 GB of RAM; those two claims
// could not both hold.
//
// The observable one is here. Because the error surfaced from FetchByUID BEFORE
// the ingest loop began, a fetch that died part-way ingested NOTHING — so a
// mailbox too large to buffer could never make progress, no matter how many
// times it was retried. Streaming ingests each message as it arrives, so the
// work done before a failure is durably kept.
//
// PROBE: buffer the stream inside fetchAndIngest (collect, then ingest) and
// ing.calls drops to 0 here.
func TestAFetchThatDiesPartWayKeepsWhatItAlreadyIngested(t *testing.T) {
	sess := &fakeSession{
		mailboxes: []sync.Mailbox{inbox()},
		boxes: map[string]*fakeBox{
			"INBOX": {
				status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 5, PermanentFlagsAllowNew: true},
				msgs: []sync.Message{
					msg(1, 0, "a"),
					msg(2, 0, "b"),
					msg(3, 0, "c"),
					msg(4, 0, "d"),
				},
				failAfterUID: 2, // 1 and 2 arrive, then the connection dies
			},
		},
	}
	st, ing := newFakeStore(), newFakeIngester()

	sum, err := newEngine(sess, st, ing).SyncAccount(context.Background(), testReq())
	if err != nil {
		t.Fatalf("SyncAccount() error = %v; a single bad mailbox is reported in Summary.Errors, not returned", err)
	}
	if len(sum.Errors) != 1 {
		t.Fatalf("Summary.Errors = %v, want the mid-stream failure recorded against INBOX", sum.Errors)
	}

	// The point: the messages that arrived before the failure were ingested.
	if len(ing.calls) != 2 {
		t.Fatalf("ingest calls = %d, want 2 (UIDs 1 and 2 arrived before the failure); "+
			"0 means the fetch was buffered and the whole range was lost", len(ing.calls))
	}
	if ing.calls[0].UID != 1 || ing.calls[1].UID != 2 {
		t.Errorf("ingested UIDs = %d,%d, want 1,2", ing.calls[0].UID, ing.calls[1].UID)
	}

	// And the cursor is NOT advanced past them: a failed pass must leave the
	// mailbox due, not claim ground it did not finish taking.
	id := st.rows["INBOX"].id
	if c, ok := st.cursorCalls[id]; ok && c.LastSeenUID != 0 {
		t.Errorf("cursor advanced to %d on a failed pass, want it withheld", c.LastSeenUID)
	}
}

// ---- the server refuses a SEARCH (#638) -------------------------------------
//
// Migadu allows 60 UID SEARCHes per rolling minute, per account. An
// account with hundreds of mailboxes walked straight past the refusal into the next
// SEARCH: pass 1 sent hundreds of commands the server had already refused, and
// recorded each one as that mailbox's own error, so hundreds of identical WARNs per
// pass buried anything genuinely broken. The loop must stop instead.

// rateLimitedSearch is exactly what internal/sync hands back for Migadu's plain
// `NO search rate limit exceeded: ... please wait 42s ...` — see
// TestClassifyRateLimited, which pins that the real wire text produces this.
func rateLimitedSearch() error {
	return &sync.Error{
		Kind:       sync.ErrorKindRateLimited,
		Op:         "liveuids",
		Err:        errors.New("imap: NO search rate limit exceeded: 60 searches in 1m0s, please wait 42s before trying again"),
		RetryAfter: 42 * time.Second,
	}
}

// verifyFixture builds n observable mailboxes, all due, with the named one
// failing its LiveUIDs read. It returns the session, the store, the targets in
// order, and the engine.
func verifyFixture(t *testing.T, n int, failAt int, liveErr error) (*fakeSession, *fakeStore, []reapTarget, *Engine) {
	t.Helper()
	sess := &fakeSession{boxes: map[string]*fakeBox{}}
	st, ing := newFakeStore(), newFakeIngester()
	var targets []reapTarget
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Folder%02d", i)
		box := &fakeBox{
			status: sync.MailboxStatus{UIDValidity: 10, UIDNext: 2, NumMessages: 1},
			msgs:   []sync.Message{msg(1, 0, "a")},
		}
		if i == failAt {
			box.liveErr = liveErr
		}
		sess.boxes[name] = box
		sess.mailboxes = append(sess.mailboxes, sync.Mailbox{Name: name})
		id := newULID()
		st.seedRow(name, &storedRow{id: id, syncScope: "full", uidValidity: 10}, 1)
		targets = append(targets, reapTarget{
			name: name, id: id, uidValidity: 10,
			// lastSeenUID 0: the poll claims to have covered nothing, so the gap
			// detector has nothing to prove and the store is not consulted. This
			// test is about WHICH MAILBOXES ARE ASKED, not about gap detection.
			lastSeenUID: 0,
			lastReaped:  nil, // never observed => due, whatever its jitter offset
		})
	}
	return sess, st, targets, reapEngine(sess, st, ing)
}

// THE MECHANISM. A rate-limit refusal on the 3rd of 8 mailboxes must end the
// observation loop for this pass: 3 SEARCHes attempted, 6 mailboxes counted as
// deferred (the refused one plus the 5 never reached), NO per-mailbox errors,
// and nothing stamped or reaped for any of them.
func TestVerifyAccountStopsAskingAfterARateLimitRefusal(t *testing.T) {
	const total, failAt = 8, 2
	sess, st, targets, eng := verifyFixture(t, total, failAt, rateLimitedSearch())

	var summary Summary
	verified := eng.verifyAccount(context.Background(), testReq(), sess, false, targets, &summary)

	// 1. WE STOPPED ASKING. This is the whole point: every SEARCH after the
	// refusal was a command the server had already told us it would not answer.
	if len(sess.liveCalls) != failAt+1 {
		t.Fatalf("LiveUIDs called %d times (%v), want %d — the loop did not stop at the refusal",
			len(sess.liveCalls), sess.liveCalls, failAt+1)
	}

	// 2. THE COUNT IS REPORTED, per #638's "N mailboxes deferred", and it counts
	// the refused mailbox too — it got no answer either.
	if want := total - failAt; summary.MailboxesDeferred != want {
		t.Errorf("MailboxesDeferred = %d, want %d (the refused mailbox plus the %d never reached)",
			summary.MailboxesDeferred, want, total-failAt-1)
	}
	if summary.RateLimitRetryAfter != 42*time.Second {
		t.Errorf("RateLimitRetryAfter = %v, want 42s — the server's own wait was dropped",
			summary.RateLimitRetryAfter)
	}
	// Only the SEARCHes the server ANSWERED are observations; the refused one is
	// deferred, and counting it in both would double-report the same mailbox.
	if summary.MailboxesObserved != failAt {
		t.Errorf("MailboxesObserved = %d, want %d (the answered SEARCHes before the refusal)",
			summary.MailboxesObserved, failAt)
	}

	// 3. NOT AN ERROR. Not for the refused mailbox, not for the ones not reached.
	if len(summary.Errors) != 0 {
		t.Errorf("Summary.Errors = %v, want none: throttling is a paced pass, not a degraded one",
			summary.Errors)
	}

	// 4. FAIL CLOSED, EXACTLY AS BEFORE. Only the mailboxes actually observed are
	// verified, so only they can be stamped or reaped; the rest stay due.
	if len(verified) != failAt {
		t.Fatalf("verified %d mailboxes, want %d (only the ones observed before the refusal)",
			len(verified), failAt)
	}
	eng.reapAccount(context.Background(), testReq(), verified, &summary)
	stamped := map[ulid.ULID]bool{}
	for _, id := range st.stampCalls {
		stamped[id] = true
	}
	for i := failAt; i < total; i++ {
		if stamped[targets[i].id] {
			t.Errorf("mailbox %q was stamped/reaped although it was never observed — "+
				"it would not come due again for a whole interval", targets[i].name)
		}
	}
	if len(stamped) != failAt {
		t.Errorf("%d mailboxes stamped, want %d", len(stamped), failAt)
	}
}

// THE CONTROL. Today's behaviour for every OTHER failure must survive: an
// ordinary error on the 3rd mailbox is recorded against that mailbox and the
// loop CONTINUES to the rest. Without this, "stop on refusal" could quietly
// become "stop on any error" and one broken folder would stall the account.
func TestVerifyAccountContinuesAfterANonRateLimitError(t *testing.T) {
	const total, failAt = 8, 2
	boom := errors.New("connection reset by peer")
	sess, _, targets, eng := verifyFixture(t, total, failAt, boom)

	var summary Summary
	verified := eng.verifyAccount(context.Background(), testReq(), sess, false, targets, &summary)

	if len(sess.liveCalls) != total {
		t.Fatalf("LiveUIDs called %d times (%v), want all %d — an ordinary failure must not stop the pass",
			len(sess.liveCalls), sess.liveCalls, total)
	}
	if len(summary.Errors) != 1 || !errors.Is(summary.Errors[0], boom) {
		t.Fatalf("Summary.Errors = %v, want exactly the one failed mailbox", summary.Errors)
	}
	if summary.Errors[0].Mailbox != targets[failAt].name {
		t.Errorf("the error was recorded against %q, want %q", summary.Errors[0].Mailbox, targets[failAt].name)
	}
	if summary.MailboxesDeferred != 0 {
		t.Errorf("MailboxesDeferred = %d, want 0: nothing was deferred, one mailbox failed",
			summary.MailboxesDeferred)
	}
	if len(verified) != total-1 {
		t.Errorf("verified %d mailboxes, want %d (every one but the failure)", len(verified), total-1)
	}
	// Every answered SEARCH, whatever came of it — and none for the mailbox the
	// server never answered.
	if summary.MailboxesObserved != total-1 {
		t.Errorf("MailboxesObserved = %d, want %d (every answered SEARCH)", summary.MailboxesObserved, total-1)
	}
}

// A mailbox that is NOT DUE was never going to be observed this pass, so it must
// not inflate the deferred count — an operator reading "700 deferred" on an
// account where 12 were due cannot act on the number.
func TestVerifyAccountDeferredCountsOnlyDueMailboxes(t *testing.T) {
	const total, failAt = 8, 2
	sess, _, targets, eng := verifyFixture(t, total, failAt, rateLimitedSearch())

	// Mark the last three as freshly observed: not due, so not deferred either.
	fresh := time.Now()
	for i := total - 3; i < total; i++ {
		stamp := fresh
		targets[i].lastReaped = &stamp
	}

	var summary Summary
	eng.verifyAccount(context.Background(), testReq(), sess, false, targets, &summary)

	if want := total - failAt - 3; summary.MailboxesDeferred != want {
		t.Errorf("MailboxesDeferred = %d, want %d (the refused mailbox plus the due ones after it, "+
			"excluding the 3 that were not due)", summary.MailboxesDeferred, want)
	}
}
