// SPDX-License-Identifier: AGPL-3.0-only

package mailsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/gmaillabel"
	"mailroom/internal/ingest"
	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

// THE GMAIL SYNC MODEL, DRIVEN OVER THE WIRE (#52, ADR-047). Every test here
// runs the real engine through the real sync.Dial and the vendored go-imap
// parser against the scripted fake Gmail (internal/sync/gmailtest), so what
// is asserted — which folders were selected, how many body bytes were
// downloaded on which day — is what the server SAW, not the engine's own
// bookkeeping. The store and the ingester are in-memory fakes; the label
// mapping into Mailroom's tables is covered against PostgreSQL by
// internal/store's gmail integration test.

// gmailStore is the fake store plus the Gmail bookkeeping (floor, sweep
// stamp and progress, the rolling meter), persisted across passes like the
// real columns.
type gmailStore struct {
	*fakeStore
	floors    map[ulid.ULID]int64
	swept     map[ulid.ULID]time.Time
	sweepNext map[ulid.ULID]int64
	charges   []charge // every metered charge, in order
	scopeOff  map[ulid.ULID]bool
}

type charge struct {
	at time.Time
	n  int64
}

func newGmailStore() *gmailStore {
	return &gmailStore{fakeStore: newFakeStore(), floors: map[ulid.ULID]int64{},
		swept: map[ulid.ULID]time.Time{}, sweepNext: map[ulid.ULID]int64{}, scopeOff: map[ulid.ULID]bool{}}
}

// window is the metered total in the 24 hours ending at at (whole hours, as
// the real ring buckets them).
func (g *gmailStore) window(at time.Time) int64 {
	var n int64
	now := at.Unix() / 3600
	for _, c := range g.charges {
		if h := c.at.Unix() / 3600; h > now-25 && h <= now { // 25 hourly buckets, as store's meterHours
			n += c.n
		}
	}
	return n
}

// metered is everything ever charged.
func (g *gmailStore) metered() int64 {
	var n int64
	for _, c := range g.charges {
		n += c.n
	}
	return n
}

func (g *gmailStore) UpsertMailbox(ctx context.Context, u ulid.ULID, rec MailboxRecord) (StoredMailbox, error) {
	sm, err := g.fakeStore.UpsertMailbox(ctx, u, rec)
	if f, ok := g.floors[sm.ID]; ok {
		sm.BackfillFloorUID = &f
	}
	if s, ok := g.swept[sm.ID]; ok {
		sm.LabelsSweptAt = &s
	}
	if n, ok := g.sweepNext[sm.ID]; ok {
		sm.LabelSweepNext = &n
	}
	if g.scopeOff[sm.ID] {
		sm.SyncScope = syncScopeOff
	}
	return sm, err
}

func (g *gmailStore) SetSyncScopeOff(_ context.Context, _, id ulid.ULID) error {
	g.scopeOff[id] = true
	return nil
}

func (g *gmailStore) SetLabelSweepNext(_ context.Context, _, id ulid.ULID, next *int64) error {
	if next == nil {
		delete(g.sweepNext, id)
	} else {
		g.sweepNext[id] = *next
	}
	return nil
}

// UpdateMailboxCursors persists the cursors onto the row, as the real store
// does, so the next pass reads them back.
func (g *gmailStore) UpdateMailboxCursors(ctx context.Context, u, id ulid.ULID, c MailboxCursors) error {
	if err := g.fakeStore.UpdateMailboxCursors(ctx, u, id, c); err != nil {
		return err
	}
	r := g.rowByID(id)
	r.uidValidity, r.lastSeenUID, r.highestModSeq = c.UIDValidity, c.LastSeenUID, c.HighestModSeq
	return nil
}

func (g *gmailStore) SetBackfillFloor(_ context.Context, _, id ulid.ULID, floor int64) error {
	g.floors[id] = floor
	return nil
}

func (g *gmailStore) StampLabelSweep(_ context.Context, _, id ulid.ULID, at time.Time) error {
	g.swept[id] = at
	return nil
}

func (g *gmailStore) DownloadedLast24h(_ context.Context, _, _ ulid.ULID, at time.Time) (int64, error) {
	return g.window(at), nil
}

func (g *gmailStore) AddDownloadBytes(_ context.Context, _, _ ulid.ULID, at time.Time, n int64) (int64, error) {
	g.charges = append(g.charges, charge{at: at, n: n})
	return g.window(at), nil
}

var _ GmailStore = (*gmailStore)(nil)

// gmailIngester records every Gmail reading, and models the store's "apply
// only what changed": RefreshFlags reports a Gmail change when the labels
// differ from the last reading of that copy.
type gmailIngester struct {
	*fakeIngester
	order   []int64            // UIDs ingested from All Mail, in order
	labels  map[int64][]string // last labels seen per All Mail UID
	changes map[int64][]string // label sets that arrived as CHANGES via RefreshFlags
	noGmail int                // ingests/refreshes WITHOUT a Gmail reading
	allMail ulid.ULID

	// readings are EVERY Gmail label reading handed over for an All Mail UID,
	// ingest and refresh alike, in order.
	readings map[int64][][]string
	// poison UIDs fail Ingest every time (a message ingest cannot store).
	poison map[int64]bool
	// preflight is what Preflight answers (the quota/scanner pause).
	preflight error
}

func newGmailIngester() *gmailIngester {
	return &gmailIngester{fakeIngester: newFakeIngester(), labels: map[int64][]string{}, changes: map[int64][]string{},
		readings: map[int64][][]string{}, poison: map[int64]bool{}}
}

func (g *gmailIngester) Preflight(context.Context, ulid.ULID) error { return g.preflight }

var _ Preflighter = (*gmailIngester)(nil)

func (g *gmailIngester) Ingest(ctx context.Context, u ulid.ULID, dek []byte, dekID ulid.ULID, raw []byte, prov ingest.Provenance) (ingest.Result, error) {
	if prov.MailboxID == g.allMail && g.poison[prov.UID] {
		return ingest.Result{}, errors.New("poison: ingest cannot store this message")
	}
	if prov.Gmail != nil && prov.MailboxID == g.allMail {
		g.readings[prov.UID] = append(g.readings[prov.UID], prov.Gmail.Labels)
	}
	res, err := g.fakeIngester.Ingest(ctx, u, dek, dekID, raw, prov)
	if prov.Gmail == nil {
		g.noGmail++
	} else if prov.MailboxID == g.allMail {
		g.order = append(g.order, prov.UID)
		g.labels[prov.UID] = prov.Gmail.Labels
	}
	return res, err
}

func (g *gmailIngester) RefreshFlags(ctx context.Context, u ulid.ULID, prov ingest.Provenance) (ingest.FlagRefresh, error) {
	res, err := g.fakeIngester.RefreshFlags(ctx, u, prov)
	if prov.Gmail == nil {
		g.noGmail++
		return res, err
	}
	if prov.MailboxID == g.allMail {
		g.readings[prov.UID] = append(g.readings[prov.UID], prov.Gmail.Labels)
	}
	if res.Found && prov.MailboxID == g.allMail && !gmaillabel.Equal(g.labels[prov.UID], prov.Gmail.Labels) {
		g.labels[prov.UID] = prov.Gmail.Labels
		g.changes[prov.UID] = prov.Gmail.Labels
		res.Gmail.LabelsAdded = 1
		res.Gmail.Changed = true
	}
	return res, err
}

const allMail = "[Gmail]/All Mail"

type gmailRig struct {
	t     *testing.T
	g     *gmailtest.Server
	cfg   sync.Config
	st    *gmailStore
	ing   *gmailIngester
	req   AccountSync
	clock time.Time
	e     *Engine
}

func newGmailRig(t *testing.T, budget int64, sweep time.Duration) *gmailRig {
	r := &gmailRig{t: t, g: gmailtest.New(t), st: newGmailStore(), ing: newGmailIngester(),
		clock: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	r.g.StandardMailboxes()
	r.cfg = r.g.Listen(sync.TLSInsecure)
	r.ing.st = r.st.fakeStore
	r.req = withCfg(testReq(), r.cfg)
	r.req.Gmail = &GmailSync{DailyBudgetBytes: budget, LabelSweepInterval: sweep}
	r.e = New(r.st, r.ing, sync.Dial, WithClock(func() time.Time { return r.clock }), WithFetchChunk(4))
	return r
}

// add appends n All Mail messages of size bytes each, labelled per uid.
func (r *gmailRig) add(n, size int, labels func(i int) []string) {
	for i := 0; i < n; i++ {
		var l []string
		if labels != nil {
			l = labels(i)
		}
		body := fmt.Sprintf("Message-ID: <m%d-%d@example.com>\r\n\r\n", time.Now().UnixNano(), i)
		body += strings.Repeat("x", size-len(body))
		r.g.Append(allMail, &gmailtest.Message{Body: []byte(body), Labels: l, MsgID: uint64(1000 + i), ThrID: 7})
	}
}

// passErr runs one pass and returns what it returned, failing nothing.
func (r *gmailRig) passErr() (Summary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return r.e.SyncAccount(ctx, r.req)
}

func (r *gmailRig) pass() Summary {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sum, err := r.e.SyncAccount(ctx, r.req)
	if err != nil {
		r.t.Fatalf("sync: %v", err)
	}
	for _, e := range sum.Errors {
		r.t.Errorf("mailbox error: %v", e)
	}
	if r.ing.allMail == (ulid.ULID{}) {
		if row, ok := r.st.rows[allMail]; ok {
			r.ing.allMail = row.id
		}
	}
	return sum
}

// ⚠ THE FIRST PASS HAS TO KNOW THE ALL MAIL ID BEFORE IT INGESTS, so the rig
// upserts it ahead of time (the fake store keys rows by name).
func (r *gmailRig) primeAllMail() {
	sm, _ := r.st.UpsertMailbox(context.Background(), r.req.UserID, MailboxRecord{IMAPName: allMail})
	r.ing.allMail = sm.ID
}

func allowedSelect(name string) bool {
	return name == allMail || name == "[Gmail]/Spam" || name == "[Gmail]/Trash"
}

// TestGmailSyncSelectsOnlyAllMailSpamTrash: the label folders — INBOX, the
// user's labels, Important, Starred, Sent, Drafts — are listed and never
// selected, and every message arrives with its X-GM-* reading.
func TestGmailSyncSelectsOnlyAllMailSpamTrash(t *testing.T) {
	r := newGmailRig(t, 0, 0)
	r.add(5, 200, func(i int) []string { return []string{`\Inbox`, "Work"} })
	r.g.Append("[Gmail]/Spam", &gmailtest.Message{Body: []byte("Subject: spam\r\n\r\nx"), MsgID: 9})
	r.g.Append("[Gmail]/Trash", &gmailtest.Message{Body: []byte("Subject: trash\r\n\r\nx"), MsgID: 10})
	r.primeAllMail()

	sum := r.pass()
	for _, sel := range r.g.Selects() {
		if !allowedSelect(sel) {
			t.Errorf("selected %q — a Gmail label view; the Gmail model must never select one", sel)
		}
	}
	if len(r.g.Selects()) == 0 {
		t.Fatal("nothing was selected at all; the assertion above proves nothing")
	}
	// INBOX, Work, Receipts, Été, [Gmail], Drafts, Important, Sent Mail, Starred.
	if sum.GmailFoldersSkipped != 9 {
		t.Errorf("GmailFoldersSkipped = %d, want 9", sum.GmailFoldersSkipped)
	}
	if sum.MessagesIngested != 7 || r.ing.noGmail != 0 {
		t.Errorf("ingested=%d, without a Gmail reading=%d", sum.MessagesIngested, r.ing.noGmail)
	}
	if got := r.ing.labels[1]; !reflect.DeepEqual(got, []string{`\Inbox`, "Work"}) {
		t.Errorf("uid 1 labels = %q", got)
	}
	for _, c := range r.ing.calls {
		if c.Gmail != nil && c.MailboxID == r.ing.allMail && (c.Gmail.MsgID == 0 || c.Gmail.ThrID != 7 || c.GmMsgID == "") {
			t.Errorf("uid %d: msgid=%d thrid=%d gm_msgid=%q", c.UID, c.Gmail.MsgID, c.Gmail.ThrID, c.GmMsgID)
		}
	}
	// Newest first: history is walked downward from the floor, one chunk (4
	// UIDs here) at a time; within a chunk the server answers in its own order.
	if len(r.ing.order) != 5 || !sameSet(r.ing.order[:4], 2, 3, 4, 5) || r.ing.order[4] != 1 {
		t.Errorf("All Mail ingest order %v is not newest-first by chunk", r.ing.order)
	}
	// No body is fetched with the non-peek BODY[] (it would mark mail read).
	for _, f := range r.g.Fetches() {
		if strings.Contains(f, "BODY[]") && !strings.Contains(f, "BODY.PEEK[]") {
			t.Errorf("a non-peek body fetch: %s", f)
		}
	}
}

// TestGmailImportStopsAtBudgetAndResumesNextDay: ten 100-byte messages and a
// 500-byte daily budget. History may spend 90% (450), so each day imports the
// four newest not yet imported, a new day resumes, and new mail is taken
// before history out of the remaining tenth.
func TestGmailImportStopsAtBudgetAndResumesNextDay(t *testing.T) {
	r := newGmailRig(t, 500, 0)
	r.add(10, 100, nil)
	r.primeAllMail()

	served := func() int64 { b, _ := r.g.BodyBytesServed(); return b }

	sum := r.pass()
	if got := served(); got != 400 {
		t.Errorf("day 1: %d body bytes downloaded, want 400 (4 messages under the 450-byte history share)", got)
	}
	if !sum.GmailBudgetSpent || sum.HistoryBodiesFetched != 4 {
		t.Errorf("day 1: budgetSpent=%v history=%d", sum.GmailBudgetSpent, sum.HistoryBodiesFetched)
	}
	if !sameSet(r.ing.order, 7, 8, 9, 10) {
		t.Errorf("day 1 imported %v, want the newest four", r.ing.order)
	}
	floor := r.st.floors[r.ing.allMail]
	if floor != 7 {
		t.Errorf("floor after day 1 = %d, want 7", floor)
	}

	// Same day again: nothing more — the history share is spent.
	r.pass()
	if got := served(); got != 400 {
		t.Errorf("a second pass on day 1 downloaded more: %d bytes total", got)
	}

	// Day 2: new mail arrives; it is fetched first, then history resumes.
	r.clock = r.clock.Add(25 * time.Hour) // the meter frees an hour's bytes 24h after the hour ENDS
	r.add(1, 100, nil)                    // uid 11
	r.pass()
	// New mail (100) comes out of the whole budget first; history then has
	// 450 - 100 = 350 left of its share: three more messages.
	if got := served() - 400; got != 400 {
		t.Errorf("day 2 downloaded %d bytes, want 400 (new mail 100 + history 300)", got)
	}
	if len(r.ing.order) != 8 || r.ing.order[4] != 11 || !sameSet(r.ing.order[5:], 4, 5, 6) {
		t.Errorf("order after day 2 = %v, want new mail (11) first, then history 4-6", r.ing.order)
	}
	if w := r.st.window(r.clock); w > 500 {
		t.Errorf("the 24 hours ending on day 2 metered %d bytes, over the budget", w)
	}

	// Day 3: the rest.
	r.clock = r.clock.Add(25 * time.Hour) // the meter frees an hour's bytes 24h after the hour ENDS
	sum = r.pass()
	if len(r.ing.order) != 11 || !sameSet(r.ing.order[8:], 1, 2, 3) {
		t.Errorf("order after day 3 = %v", r.ing.order)
	}
	if sum.GmailBudgetSpent || r.st.floors[r.ing.allMail] != 1 {
		t.Errorf("day 3: budgetSpent=%v floor=%d, want done at floor 1", sum.GmailBudgetSpent, r.st.floors[r.ing.allMail])
	}
	for _, c := range r.st.charges {
		if w := r.st.window(c.at); w > 500 {
			t.Errorf("the 24 hours ending %s metered %d bytes, over the budget", c.at, w)
		}
	}
}

// TestGmailBackfillFloorBoundsTheGapBackfill: on day 1 the import stops at
// the budget with UIDs 1–6 still on the server and never fetched. The #93
// gap detector must not read them as gaps (they are below the floor): if it
// did, it would fetch them at once, outside the budget.
func TestGmailBackfillFloorBoundsTheGapBackfill(t *testing.T) {
	r := newGmailRig(t, 500, 0)
	r.add(10, 100, nil)
	r.primeAllMail()
	sum := r.pass()
	if b, _ := r.g.BodyBytesServed(); b > 500 {
		t.Errorf("day 1 downloaded %d bytes, over the 500-byte budget — the gap backfill pulled history", b)
	}
	if sum.GapsDetected != 0 {
		t.Errorf("GapsDetected = %d: un-imported history read as gaps", sum.GapsDetected)
	}
	observed := false
	for _, gc := range r.st.gapCalls {
		if gc.MailboxID != r.ing.allMail {
			continue
		}
		observed = true
		for _, u := range gc.ObservedUIDs {
			if u < 7 {
				t.Errorf("the gap detector was handed uid %d, below the floor 7", u)
			}
		}
	}
	if !observed {
		t.Fatal("All Mail was never observed; the floor assertion above proves nothing")
	}

	// A REAL gap (a UID at or above the floor the import never stored) is
	// still found and refetched.
	row := r.st.rows[allMail]
	delete(row.prov, 8)
	row.lastReapedAt = nil
	r.clock = r.clock.Add(2 * time.Hour)
	sum = r.pass()
	if sum.GapsDetected != 1 || sum.GapsBackfilled != 1 {
		t.Errorf("a real gap above the floor: detected=%d backfilled=%d", sum.GapsDetected, sum.GapsBackfilled)
	}
}

// TestGmailLabelChangeByModSeq: a label changed in Gmail bumps MODSEQ (the
// UNVERIFIED behaviour, modelled on), and the CONDSTORE pass carries it.
func TestGmailLabelChangeByModSeq(t *testing.T) {
	r := newGmailRig(t, 0, 0)
	r.add(3, 100, func(int) []string { return []string{`\Inbox`} })
	r.primeAllMail()
	r.pass()
	r.g.SetLabels(allMail, 2, "Receipts")
	sum := r.pass()
	if got := r.ing.changes[2]; !reflect.DeepEqual(got, []string{"Receipts"}) {
		t.Errorf("label change via MODSEQ not applied: %q", got)
	}
	if sum.GmailLabelSweeps != 0 || sum.GmailLabelChanges != 1 {
		t.Errorf("sweeps=%d changes=%d, want 0 and 1", sum.GmailLabelSweeps, sum.GmailLabelChanges)
	}
}

// TestGmailLabelChangeBySweep: when a label change does NOT bump MODSEQ, the
// CONDSTORE pass cannot see it (the negative control), and the sweep does.
func TestGmailLabelChangeBySweep(t *testing.T) {
	r := newGmailRig(t, 0, time.Hour)
	r.g.LabelChangeBumpsModSeq = false
	r.add(3, 100, func(int) []string { return []string{`\Inbox`} })
	r.primeAllMail()
	r.pass() // imports, and sweeps once (never swept before)
	r.g.SetLabels(allMail, 3, "Work", "Été")

	r.clock = r.clock.Add(10 * time.Minute) // sweep not due
	r.pass()
	if _, seen := r.ing.changes[3]; seen {
		t.Fatal("the change was seen without a MODSEQ bump and before the sweep was due — the fixture is not testing the sweep")
	}
	r.clock = r.clock.Add(time.Hour)
	sum := r.pass()
	if got := r.ing.changes[3]; !reflect.DeepEqual(got, []string{"Work", "Été"}) {
		t.Errorf("the sweep did not carry the label change: %q", got)
	}
	if sum.GmailLabelSweeps != 1 {
		t.Errorf("GmailLabelSweeps = %d, want 1", sum.GmailLabelSweeps)
	}
	for _, f := range r.g.Fetches() {
		if strings.Contains(f, "X-GM-LABELS") && strings.Contains(f, "FLAGS") && !strings.Contains(f, "BODY") {
			return // the sweep's FETCH carries no body
		}
	}
	t.Error("no body-less X-GM-LABELS fetch was issued")
}

func TestGmailRefusals(t *testing.T) {
	t.Run("All Mail hidden", func(t *testing.T) {
		g := gmailtest.New(t)
		g.AddMailbox(&gmailtest.Mailbox{Name: "INBOX"})
		g.AddMailbox(&gmailtest.Mailbox{Name: "Work"})
		req := testReq()
		req.Gmail = &GmailSync{}
		_, err := New(newGmailStore(), newGmailIngester(), sync.Dial).SyncAccount(context.Background(), withCfg(req, g.Listen(sync.TLSInsecure)))
		if !errors.Is(err, ErrGmailAllMailHidden) {
			t.Errorf("err = %v", err)
		}
		if len(g.Selects()) != 0 {
			t.Errorf("a refused Gmail selected %v (the label folders must not be the fallback)", g.Selects())
		}
	})
	t.Run("not Gmail", func(t *testing.T) {
		g := gmailtest.New(t)
		g.StandardMailboxes()
		g.GmailExt = false
		req := testReq()
		req.Gmail = &GmailSync{}
		_, err := New(newGmailStore(), newGmailIngester(), sync.Dial).SyncAccount(context.Background(), withCfg(req, g.Listen(sync.TLSInsecure)))
		if !errors.Is(err, ErrGmailNoExtension) || len(g.Selects()) != 0 {
			t.Errorf("err = %v selects = %v", err, g.Selects())
		}
	})
	t.Run("UIDVALIDITY roll retires nothing", func(t *testing.T) {
		r := newGmailRig(t, 0, 0)
		r.add(2, 100, nil)
		r.primeAllMail()
		r.pass()
		r.st.rows[allMail].uidValidity = 99 // the server says 1
		ctx := context.Background()
		sum, err := r.e.SyncAccount(ctx, r.req)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if len(sum.Errors) != 1 || !errors.Is(sum.Errors[0].Err, errGmailRolled) {
			t.Errorf("errors = %v", sum.Errors)
		}
		if len(r.st.staleCalls) != 0 {
			t.Errorf("a Gmail roll retired provenance: %v", r.st.staleCalls)
		}
	})
}

func withCfg(req AccountSync, cfg sync.Config) AccountSync {
	req.Config = cfg
	return req
}

// The non-Gmail path never asks for X-GM items, even from a Gmail server.
func TestGenericSyncNeverAsksForGmailItems(t *testing.T) {
	g := gmailtest.New(t)
	g.AddMailbox(&gmailtest.Mailbox{Name: "INBOX"})
	g.Append("INBOX", &gmailtest.Message{Body: bytes.Repeat([]byte("y"), 50)})
	_, err := New(newGmailStore(), newGmailIngester(), sync.Dial).SyncAccount(context.Background(), withCfg(testReq(), g.Listen(sync.TLSInsecure)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range g.Fetches() {
		if strings.Contains(f, "X-GM") {
			t.Errorf("a generic sync asked for Gmail items: %s", f)
		}
	}
}

func sameSet(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]int64(nil), got...)
	b := append([]int64(nil), want...)
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	return reflect.DeepEqual(a, b)
}
