// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// DAY-ONE SETUP, END TO END (#446): the census, the preview, the chunked apply,
// the desk agreeing that archived mail is off the desk, and the undo.
//
// ⚠ THE ASSERTION THAT MATTERS MOST IS THE ONE ABOUT WRITEBACK INTENTS. #446's
// whole promise is "nothing moves on the server", and the only way to check a
// promise about the IMAP server from here is to count the intents the run
// enqueued: a labelArchive folder must add NONE, and only the explicit
// bringToDesk folder may add any. Every other assertion in this file could pass
// while that one failed, and the failure would be invisible until somebody's
// provider had four thousand messages moved out from under them.
//
// ⚠ THE DRAWER-SWEEP ASSERTION IS WRITTEN AS A BEFORE/AFTER PAIR ON PURPOSE.
// "DrawerCandidates never lists this message" is trivially true of a message
// that never qualified, and a test that cannot fail is documentation (CLAUDE.md,
// #62/#131). So the fixture makes one message a REAL candidate first — scored
// below the line, old enough, in an auto-archiving area, with an Archive folder
// to move to — asserts that it IS offered, then applies the run and asserts it
// is not. Deleting `desk_archived_at IS NULL` from deskLiveSQL turns the second
// half red; the first half proves the second half is looking at something.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// ---- the fixture -----------------------------------------------------------

// setupFixture is the seeded mailbox the whole file works against.
type setupFixture struct {
	h      *harness
	s      *Store
	u      fixtureUser
	userID ulid.ULID

	inboxID   ulid.ULID
	archiveID ulid.ULID
	folderA   ulid.ULID // 6 messages, ONE sender domain
	folderB   ulid.ULID // 20 messages, 5 senders
	folderC   ulid.ULID // 10 messages, all List-Unsubscribe, 8 senders
	folderD   ulid.ULID // 12 senders + 1 with no From address

	// updatesArea is an area fed by tab:updates, and updatesLabel is its own
	// label. It exists so the INBOX arm of the apply has something to do: an
	// INBOX message is labelled with its OWN RESOLVED AREA's label, and with no
	// tab-fed area at all that path would be exercised by nothing.
	updatesArea  ulid.ULID
	updatesLabel ulid.ULID

	// inboxOlderTabbed is the older INBOX message whose classified_tab is
	// `updates`, so it resolves into updatesArea BY TAB. It is also the drawer
	// candidate the desk-agreement test uses.
	inboxOlderTabbed ulid.ULID
	// inboxOlderPlain are the two older INBOX messages that resolve to NOTHING —
	// Needs sorting. They get NO label and ARE archived, which is the rule worth
	// pinning: day-one does not invent a filing for mail no area claims.
	inboxOlderPlain []ulid.ULID
	// inboxInWindow are the messages inside the window. They must be counted
	// NOWHERE and touched by nothing.
	inboxInWindow []ulid.ULID

	messagesIn map[ulid.ULID][]ulid.ULID // folder -> its seeded messages
}

const (
	setupWindowDays = 30
	// setupOldDays is comfortably outside the window; setupRecentDays comfortably
	// inside it. Neither is near the boundary, so nothing in this file depends on
	// how the cutoff rounds.
	setupOldDays    = 60
	setupRecentDays = 5
)

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	h := newHarness(t)
	f := &setupFixture{h: h, s: h.appStore(), messagesIn: map[ulid.ULID][]ulid.ULID{}}
	f.u = seedUser(t, h.super, "setup-"+NewULID().String())
	f.userID = mustULID(t, f.u.ID)
	f.inboxID = mustULID(t, f.u.MailboxID)

	mailbox := func(imapName, display, specialUse string) ulid.ULID {
		id := NewULID()
		var su any
		if specialUse != "" {
			su = specialUse
		}
		mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			VALUES ($1,$2,$3,$4,$5,$6)`, id.String(), f.u.ID, f.u.AccountID, imapName, display, su)
		return id
	}
	// The Archive folder exists so the drawer sweep has somewhere to move mail
	// to — hasArchiveMailboxSQL is part of the candidate predicate, so without it
	// the before-half of the drawer assertion could not be true and the
	// after-half would prove nothing.
	f.archiveID = mailbox("Archive", "Archive", `\Archive`)
	f.folderA = mailbox("Folder A", "Folder A", "")
	f.folderB = mailbox("Folder B", "Folder B", "")
	f.folderC = mailbox("Folder C", "Folder C", "")
	f.folderD = mailbox("Folder D", "Folder D", "")

	// Folder A: 6 messages, three senders, ALL AT ONE DOMAIN -> oneDomain.
	for i := 0; i < 6; i++ {
		id := f.seedMessage(t, fmt.Sprintf("a%d", i), fmt.Sprintf("sender%d@one.example", i%3), setupOldDays, false, "")
		f.provenance(t, id, f.folderA)
		f.messagesIn[f.folderA] = append(f.messagesIn[f.folderA], id)
	}
	// Folder B: 20 messages, 5 senders at 5 domains, four each -> fewSenders
	// (five senders cover 100%, and five is a handful).
	for i := 0; i < 20; i++ {
		id := f.seedMessage(t, fmt.Sprintf("b%d", i), fmt.Sprintf("sender%d@b%d.example", i%5, i%5), setupOldDays, false, "")
		f.provenance(t, id, f.folderB)
		f.messagesIn[f.folderB] = append(f.messagesIn[f.folderB], id)
	}
	// Folder C: 10 messages, EVERY one with List-Unsubscribe, spread over 8
	// senders so it is not fewSenders -> bulk.
	for i := 0; i < 10; i++ {
		id := f.seedMessage(t, fmt.Sprintf("c%d", i), fmt.Sprintf("list%d@c%d.example", i%8, i%8), setupOldDays, true, "")
		f.provenance(t, id, f.folderC)
		f.messagesIn[f.folderC] = append(f.messagesIn[f.folderC], id)
	}
	// Folder D: 12 messages, 12 senders at 12 domains, no lists, PLUS one with no
	// From address at all -> mixed, Unclassified 1.
	for i := 0; i < 12; i++ {
		id := f.seedMessage(t, fmt.Sprintf("d%d", i), fmt.Sprintf("sender%d@d%d.example", i, i), setupOldDays, false, "")
		f.provenance(t, id, f.folderD)
		f.messagesIn[f.folderD] = append(f.messagesIn[f.folderD], id)
	}
	noFrom := f.seedMessage(t, "d-nofrom", "", setupOldDays, false, "")
	f.provenance(t, noFrom, f.folderD)
	f.messagesIn[f.folderD] = append(f.messagesIn[f.folderD], noFrom)

	// INBOX: the seeded user already has ONE message there, at now() — inside the
	// window. Two more inside, three outside.
	for i := 0; i < 2; i++ {
		id := f.seedMessage(t, fmt.Sprintf("inbox-new%d", i), "friend@inbox.example", setupRecentDays, false, "")
		f.provenance(t, id, f.inboxID)
		f.inboxInWindow = append(f.inboxInWindow, id)
	}
	f.inboxInWindow = append(f.inboxInWindow, mustULID(t, f.u.MessageID))
	f.inboxOlderTabbed = f.seedMessage(t, "inbox-old-tabbed", "news@inbox.example", setupOldDays, false, string(tabs.Updates))
	f.provenance(t, f.inboxOlderTabbed, f.inboxID)
	for i := 0; i < 2; i++ {
		id := f.seedMessage(t, fmt.Sprintf("inbox-old%d", i), "person@inbox.example", setupOldDays, false, "")
		f.provenance(t, id, f.inboxID)
		f.inboxOlderPlain = append(f.inboxOlderPlain, id)
	}

	// The tab-fed area, created DIRECTLY (not through ListAreas, which would
	// materialise the whole default plan and change every count in this file).
	ctx := context.Background()
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		// Position 1, after the area seedUser already seeds at position 0: area
		// order is areaExprSQL's tie-break, and sharing a position would make
		// which area wins a tie depend on ULID order.
		id, e := tx.CreateArea(ctx, AreaRow{Name: "Updates zone", Position: 1,
			Feeds: []AreaFeed{{Tab: tabs.Updates}}})
		if e != nil {
			return e
		}
		f.updatesArea = id
		views, e := tx.GetAreas(ctx, []ulid.ULID{id})
		if e != nil {
			return e
		}
		f.updatesLabel = views[0].LabelID
		return nil
	}); err != nil {
		t.Fatalf("seed updates area: %v", err)
	}
	return f
}

// seedMessage inserts a blob + message aged `ageDays` days, with the given
// sender ("" for none at all — the unclassifiable case), optional
// List-Unsubscribe, and optional stored classifier tab.
func (f *setupFixture) seedMessage(t *testing.T, tag, from string, ageDays int, bulk bool, classifiedTab string) ulid.ULID {
	t.Helper()
	hash := sha256.Sum256([]byte("setup-" + f.u.Tag + "-" + tag))
	mustExec(t, f.h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1,$2,10,20,$3)`, f.u.ID, hash[:], NewULID().String())
	id := NewULID()
	var fromArg, unsub, tabArg any
	if from != "" {
		fromArg = from
	}
	if bulk {
		unsub = "<mailto:unsub@example.com>"
	}
	if classifiedTab != "" {
		tabArg = classifiedTab
	}
	mustExec(t, f.h, `
		INSERT INTO app.messages (id, user_id, content_hash, subject, from_address, received_at,
		    size_bytes, list_unsubscribe, classified_tab, classifier_version)
		VALUES ($1,$2,$3,$4,$5, now() - ($6::int * interval '1 day'), 10, $7, $8::app.category_tab, $9)`,
		id.String(), f.u.ID, hash[:], tag, fromArg, ageDays, unsub, tabArg, tabs.ClassifierVersion)
	return id
}

func (f *setupFixture) provenance(t *testing.T, msg, mailbox ulid.ULID) {
	t.Helper()
	uid := testUID()
	mustExec(t, f.h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		VALUES ($1,$2,$3,$4,$5,$6,1)`,
		NewULID().String(), f.u.ID, msg.String(), f.u.AccountID, mailbox.String(), uid)
}

// scan reads one value as the SUPERUSER (introspection only — every assertion
// about behaviour goes through the app role).
func (f *setupFixture) scan(t *testing.T, dst any, query string, args ...any) {
	t.Helper()
	if err := f.h.super.QueryRow(context.Background(), query, args...).Scan(dst); err != nil {
		t.Fatalf("introspect (%s): %v", query, err)
	}
}

// plan is the mixed plan every test here uses: A labelled into a NEW area, B
// labelled with NO area at all, C left completely alone, D brought to the desk,
// INBOX labelled and archived.
func (f *setupFixture) plan() SetupPlan {
	return SetupPlan{
		WindowDays: setupWindowDays,
		Folders: []SetupFolderChoice{
			{MailboxID: f.folderA, Action: SetupActionLabelArchive, LabelName: "Folder A", AreaName: "Area A"},
			{MailboxID: f.folderB, Action: SetupActionLabelArchive, LabelName: "Folder B"},
			{MailboxID: f.folderC, Action: SetupActionLeave},
			{MailboxID: f.folderD, Action: SetupActionBringToDesk, LabelName: "Folder D"},
			{MailboxID: f.inboxID, Action: SetupActionLabelArchive},
		},
	}
}

// ---- 1. the census ---------------------------------------------------------

func TestMailboxCensus(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()

	var areasBefore int
	f.scan(t, &areasBefore, `SELECT count(*) FROM app.areas WHERE user_id=$1`, f.u.ID)

	var census []FolderCensus
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		census, e = tx.MailboxCensus(ctx, nil, setupWindowDays)
		return e
	}); err != nil {
		t.Fatalf("census: %v", err)
	}

	byID := map[ulid.ULID]FolderCensus{}
	for _, c := range census {
		byID[c.MailboxID] = c
	}
	// "Mapped without asking": the Archive folder is not a row at all.
	if _, present := byID[f.archiveID]; present {
		t.Error("the Archive folder has a census row; special-use folders are mapped without asking")
	}
	// INBOX IS a row, because step 2 applies to it.
	inbox, ok := byID[f.inboxID]
	if !ok {
		t.Fatal("INBOX has no census row, but day-one's step 2 applies to the inbox too")
	}
	if inbox.Role != "inbox" {
		t.Errorf("INBOX role = %q, want %q", inbox.Role, "inbox")
	}
	if inbox.Live != 6 || inbox.InWindow != 3 || inbox.Older != 3 {
		t.Errorf("INBOX live/inWindow/older = %d/%d/%d, want 6/3/3",
			inbox.Live, inbox.InWindow, inbox.Older)
	}

	cases := []struct {
		name          string
		id            ulid.ULID
		wantLive      int
		wantShape     string
		wantUnclassed int
	}{
		{"Folder A", f.folderA, 6, ShapeOneDomain, 0},
		{"Folder B", f.folderB, 20, ShapeFewSenders, 0},
		{"Folder C", f.folderC, 10, ShapeBulk, 0},
		{"Folder D", f.folderD, 13, ShapeMixed, 1},
	}
	for _, tc := range cases {
		c, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s has no census row", tc.name)
			continue
		}
		if c.Live != tc.wantLive {
			t.Errorf("%s live = %d, want %d", tc.name, c.Live, tc.wantLive)
		}
		if c.Older != tc.wantLive {
			t.Errorf("%s older = %d, want %d (every message in it is outside the window)",
				tc.name, c.Older, tc.wantLive)
		}
		if c.Shape != tc.wantShape {
			t.Errorf("%s shape = %q, want %q (senders=%d domains=%d for90=%d bulk=%.2f)",
				tc.name, c.Shape, tc.wantShape, c.DistinctSenders, c.DistinctDomains, c.SendersFor90, c.BulkShare)
		}
		if c.Unclassified != tc.wantUnclassed {
			t.Errorf("%s unclassified = %d, want %d", tc.name, c.Unclassified, tc.wantUnclassed)
		}
	}

	// THE CENSUS MUST NOT HAVE MATERIALISED ANYTHING — it is a report. Compared
	// against the count taken before the call rather than a hardcoded baseline,
	// because the fixture's own area count is not the property under test: what
	// matters is that the number did not MOVE. (Going through ListAreas or
	// GetAreas instead of reading app.areas directly would have materialised the
	// five-area default plan here, and its change rows with it.)
	var areasAfter int
	f.scan(t, &areasAfter, `SELECT count(*) FROM app.areas WHERE user_id=$1`, f.u.ID)
	if areasAfter != areasBefore {
		t.Errorf("a census changed the login's area count from %d to %d; it must write nothing",
			areasBefore, areasAfter)
	}
}

// ---- 2. the preview --------------------------------------------------------

func TestSetupPreview(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()

	var counts SetupCounts
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		counts, e = tx.SetupPreview(ctx, f.plan())
		return e
	}); err != nil {
		t.Fatalf("preview: %v", err)
	}

	byFolder := map[ulid.ULID]SetupFolderCounts{}
	for _, r := range counts.PerFolder {
		byFolder[r.MailboxID] = r
	}
	want := []struct {
		name                           string
		id                             ulid.ULID
		action                         string
		older, labels, archived, moves int
	}{
		{"Folder A", f.folderA, SetupActionLabelArchive, 6, 6, 6, 0},
		{"Folder B", f.folderB, SetupActionLabelArchive, 20, 20, 20, 0},
		{"Folder C", f.folderC, SetupActionLeave, 10, 0, 0, 0},
		{"Folder D", f.folderD, SetupActionBringToDesk, 13, 13, 0, 13},
		// INBOX: three older messages, all archived, but only ONE labelled — the
		// other two resolve to no area at all (Needs sorting), and day-one does
		// not invent a filing for them.
		{"INBOX", f.inboxID, SetupActionLabelArchive, 3, 1, 3, 0},
	}
	for _, w := range want {
		got, ok := byFolder[w.id]
		if !ok {
			t.Errorf("%s missing from the preview", w.name)
			continue
		}
		if got.Action != w.action {
			t.Errorf("%s action = %q, want %q", w.name, got.Action, w.action)
		}
		if got.Older != w.older || got.Labels != w.labels || got.Archived != w.archived || got.Moves != w.moves {
			t.Errorf("%s older/labels/archived/moves = %d/%d/%d/%d, want %d/%d/%d/%d",
				w.name, got.Older, got.Labels, got.Archived, got.Moves,
				w.older, w.labels, w.archived, w.moves)
		}
	}

	if counts.Labels != 40 {
		t.Errorf("total labels = %d, want 40 (6+20+13+1)", counts.Labels)
	}
	if counts.Archived != 29 {
		t.Errorf("total archived = %d, want 29 (6+20+3)", counts.Archived)
	}
	if counts.Moves != 13 {
		t.Errorf("total moves = %d, want 13 (folder D only — nothing else moves on the server)", counts.Moves)
	}
	if counts.Leave != 10 {
		t.Errorf("total leave = %d, want 10 (folder C)", counts.Leave)
	}
	// Unclassified counts the mail the plan WOULD TOUCH: folder D's From-less
	// message. Folder C's mail is left alone and does not inflate it.
	if counts.Unclassified != 1 {
		t.Errorf("unclassified = %d, want 1", counts.Unclassified)
	}
	if counts.EstimatedSeconds <= 0 {
		t.Errorf("estimatedSeconds = %d, want a positive estimate for %d writes",
			counts.EstimatedSeconds, counts.Labels+counts.Archived+counts.Moves)
	}

	// A MESSAGE INSIDE THE WINDOW IS COUNTED NOWHERE. The per-folder Older
	// figures above already exclude them; this is the direct check, against the
	// message ids themselves.
	if got := len(f.inboxInWindow); got != 3 {
		t.Fatalf("fixture has %d in-window INBOX messages, want 3", got)
	}
	total := 0
	for _, r := range counts.PerFolder {
		total += r.Older
	}
	if total != 6+20+10+13+3 {
		t.Errorf("the per-folder Older figures sum to %d, want 52 — the in-window mail must be "+
			"in none of them, and the folders must partition the population", total)
	}
}

// A message with LIVE PROVENANCE IN TWO PLANNED FOLDERS is counted ONCE, under
// the folder that comes first in the plan.
//
// It is its own test rather than a wrinkle in the fixture above because it is a
// property of the COUNTS, and mixing it into the main preview would make every
// expected number in that test depend on it. Gmail exposes labels as IMAP
// folders, so multi-folder mail is the common case rather than the exotic one:
// without the assignment rule, a preview would promise more labels than the
// apply could ever write, and "planned N, wrote fewer" would look like a bug
// in the apply.
func TestSetupPreviewCountsAMessageInTwoPlannedFoldersOnce(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "setup2-"+NewULID().String())
	userID := mustULID(t, u.ID)

	mailbox := func(name string) ulid.ULID {
		id := NewULID()
		mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
			VALUES ($1,$2,$3,$4,$4)`, id.String(), u.ID, u.AccountID, name)
		return id
	}
	first, second := mailbox("First"), mailbox("Second")

	hash := sha256.Sum256([]byte("both-" + u.Tag))
	mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		VALUES ($1,$2,10,20,$3)`, u.ID, hash[:], NewULID().String())
	msg := NewULID()
	mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, from_address, received_at, size_bytes)
		VALUES ($1,$2,$3,'both','x@example.com', now() - interval '60 days', 10)`,
		msg.String(), u.ID, hash[:])
	for _, mb := range []ulid.ULID{first, second} {
		mustExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`,
			NewULID().String(), u.ID, msg.String(), u.AccountID, mb.String(),
			testUID())
	}

	preview := func(order []ulid.ULID) SetupCounts {
		plan := SetupPlan{WindowDays: 30}
		for _, mb := range order {
			plan.Folders = append(plan.Folders, SetupFolderChoice{
				MailboxID: mb, Action: SetupActionLabelArchive, LabelName: "L-" + mb.String()[:6],
			})
		}
		var c SetupCounts
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			var e error
			c, e = tx.SetupPreview(ctx, plan)
			return e
		}); err != nil {
			t.Fatalf("preview: %v", err)
		}
		return c
	}

	c := preview([]ulid.ULID{first, second})
	if c.Labels != 1 || c.Archived != 1 {
		t.Errorf("labels/archived = %d/%d, want 1/1 — one message must be counted once, "+
			"however many planned folders hold it", c.Labels, c.Archived)
	}
	older := map[ulid.ULID]int{}
	for _, r := range c.PerFolder {
		older[r.MailboxID] = r.Older
	}
	if older[first] != 1 || older[second] != 0 {
		t.Errorf("older per folder = first %d, second %d; want 1/0 — the EARLIER plan line wins",
			older[first], older[second])
	}

	// Reverse the plan order and the assignment follows it: the rule is the
	// PLAN's order, not the mailbox ids.
	c = preview([]ulid.ULID{second, first})
	older = map[ulid.ULID]int{}
	for _, r := range c.PerFolder {
		older[r.MailboxID] = r.Older
	}
	if older[second] != 1 || older[first] != 0 {
		t.Errorf("after reversing the plan, older = first %d, second %d; want 0/1", older[first], older[second])
	}
}

// ---- 3. the apply ----------------------------------------------------------

// applyRun creates the run and drains it in SMALL chunks (limit 5) so several
// chunks run: the cursor, the counters and the "one change row per message"
// property all have to hold across chunk boundaries, and a single-chunk apply
// would exercise none of that.
func (f *setupFixture) applyRun(t *testing.T, limit int) (runID ulid.ULID, chunks int, counts SetupCounts) {
	t.Helper()
	ctx := context.Background()
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		if counts, e = tx.SetupPreview(ctx, f.plan()); e != nil {
			return e
		}
		runID, e = tx.CreateSetupRun(ctx, f.plan(), counts)
		return e
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for {
		var p SetupProgress
		if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			p, e = tx.SetupApplyChunk(ctx, runID, limit)
			return e
		}); err != nil {
			t.Fatalf("apply chunk %d: %v", chunks+1, err)
		}
		chunks++
		if p.Done {
			break
		}
		if p.Processed == 0 {
			t.Fatalf("chunk %d made no progress and did not report done", chunks)
		}
		if chunks > 100 {
			t.Fatal("apply did not finish in 100 chunks")
		}
	}
	return runID, chunks, counts
}

func TestSetupApplyToCompletion(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()

	var intentsBefore int
	f.scan(t, &intentsBefore, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.u.ID)

	runID, chunks, counts := f.applyRun(t, 5)
	if chunks < 3 {
		t.Errorf("the apply finished in %d chunk(s) with a limit of 5 over 42 messages; "+
			"the multi-chunk path is not being exercised", chunks)
	}

	// Every folder-A message carries label "Folder A", applied_by 'setup'.
	var labelA ulid.ULID
	var labelAStr string
	f.scan(t, &labelAStr, `SELECT id FROM app.labels WHERE user_id=$1 AND lower(name)='folder a'`, f.u.ID)
	labelA = mustULID(t, labelAStr)
	var labelledA int
	f.scan(t, &labelledA, `SELECT count(*) FROM app.message_labels
		 WHERE user_id=$1 AND label_id=$2 AND applied_by='setup'`, f.u.ID, labelA.String())
	if labelledA != 6 {
		t.Errorf("folder A has %d setup-applied label rows, want 6", labelledA)
	}

	// AREAS ARE LABELS: a message carrying the folder label resolves INTO the
	// area the run created for it. This is the check that day-one's labelling and
	// the desk's own resolver agree.
	var areaA ulid.ULID
	var areaAStr string
	f.scan(t, &areaAStr, `SELECT id FROM app.areas WHERE user_id=$1 AND lower(name)='area a'`, f.u.ID)
	areaA = mustULID(t, areaAStr)
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		res, e := tx.ResolveAreas(ctx, f.messagesIn[f.folderA])
		if e != nil {
			return e
		}
		for _, id := range f.messagesIn[f.folderA] {
			r, ok := res[id]
			if !ok || r.AreaID == nil || *r.AreaID != areaA {
				t.Errorf("message %s resolves to %v, want area A (%s) — the folder's label is the "+
					"area's own label, so a labelled message must resolve into it", id, r.AreaID, areaA)
				break
			}
			if !r.ByLabel {
				t.Errorf("message %s resolved into area A but not BY LABEL: %+v", id, r)
				break
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve areas: %v", err)
	}

	// desk_archived_at is set for the labelArchive folders and the older INBOX
	// mail, and NOT for the bringToDesk folder (it is coming TO the desk) or the
	// left-alone folder or anything inside the window.
	archivedIn := func(ids []ulid.ULID) int {
		var n int
		f.scan(t, &n, `SELECT count(*) FROM app.messages
			 WHERE user_id=$1 AND id = ANY($2) AND desk_archived_at IS NOT NULL`, f.u.ID, ulidStrings(ids))
		return n
	}
	if got := archivedIn(f.messagesIn[f.folderA]); got != 6 {
		t.Errorf("folder A: %d archived in Mailroom, want 6", got)
	}
	if got := archivedIn(f.messagesIn[f.folderB]); got != 20 {
		t.Errorf("folder B: %d archived in Mailroom, want 20", got)
	}
	if got := archivedIn(f.messagesIn[f.folderC]); got != 0 {
		t.Errorf("folder C is `leave`: %d archived, want 0", got)
	}
	if got := archivedIn(f.messagesIn[f.folderD]); got != 0 {
		t.Errorf("folder D is `bringToDesk`: %d archived, want 0 — it is coming TO the desk", got)
	}
	if got := archivedIn(f.inboxInWindow); got != 0 {
		t.Errorf("%d in-window INBOX message(s) archived, want 0 — the window is untouched by design", got)
	}

	// ⚠ THE ASSERTION THAT MATTERS MOST: nothing moved on the server for a
	// labelArchive folder. No moved_mailbox_id, and — the part a column check
	// cannot see — NO WRITEBACK INTENT.
	quiet := append(append([]ulid.ULID{}, f.messagesIn[f.folderA]...), f.messagesIn[f.folderB]...)
	quiet = append(quiet, f.inboxOlderTabbed)
	quiet = append(quiet, f.inboxOlderPlain...)
	var movedQuiet int
	f.scan(t, &movedQuiet, `SELECT count(*) FROM app.messages
		 WHERE user_id=$1 AND id = ANY($2) AND moved_mailbox_id IS NOT NULL`, f.u.ID, ulidStrings(quiet))
	if movedQuiet != 0 {
		t.Errorf("%d labelArchive message(s) have a canonical placement; NOTHING may move on the server", movedQuiet)
	}
	var intentsQuiet int
	f.scan(t, &intentsQuiet, `SELECT count(*) FROM app.writeback_intents
		 WHERE user_id=$1 AND message_id = ANY($2)`, f.u.ID, ulidStrings(quiet))
	if intentsQuiet != 0 {
		t.Errorf("%d writeback intent(s) were enqueued for labelArchive mail. This is THE assertion "+
			"of #446: labelling and archiving in Mailroom must reach the IMAP server in no way at all", intentsQuiet)
	}
	var intentsAfter int
	f.scan(t, &intentsAfter, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.u.ID)
	if intentsAfter-intentsBefore != 13 {
		t.Errorf("the run enqueued %d writeback intent(s), want exactly 13 (folder D's bringToDesk "+
			"moves, one per message and nothing else)", intentsAfter-intentsBefore)
	}

	// The bringToDesk folder DID move: canonical placement INBOX, one intent each.
	var movedD int
	f.scan(t, &movedD, `SELECT count(*) FROM app.messages
		 WHERE user_id=$1 AND id = ANY($2) AND moved_mailbox_id = $3`,
		f.u.ID, ulidStrings(f.messagesIn[f.folderD]), f.inboxID.String())
	if movedD != 13 {
		t.Errorf("folder D: %d message(s) canonically in INBOX, want 13", movedD)
	}
	var intentsD int
	f.scan(t, &intentsD, `SELECT count(*) FROM app.writeback_intents
		 WHERE user_id=$1 AND message_id = ANY($2) AND op='move'`,
		f.u.ID, ulidStrings(f.messagesIn[f.folderD]))
	if intentsD != 13 {
		t.Errorf("folder D: %d move intent(s), want 13 (one per message, exactly as a user's own move)", intentsD)
	}

	// EXACTLY ONE Email `updated` change row per touched message — not two for a
	// message that was both labelled and moved, and not zero for one that was
	// only labelled.
	touched := append(append([]ulid.ULID{}, quiet...), f.messagesIn[f.folderD]...)
	rows, err := f.h.super.Query(ctx, `
		SELECT object_id, count(*) FROM app.change_log
		 WHERE user_id=$1 AND object_type='Email' AND op='updated' AND object_id = ANY($2)
		 GROUP BY object_id`, f.u.ID, ulidStrings(touched))
	if err != nil {
		t.Fatalf("change rows: %v", err)
	}
	seen := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			t.Fatalf("scan change row: %v", err)
		}
		seen[id] = n
	}
	rows.Close()
	for _, id := range touched {
		switch n := seen[id.String()]; n {
		case 1:
		case 0:
			t.Errorf("message %s was written but has NO Email change row — the load-bearing "+
				"invariant (CLAUDE.md) is broken and /changes would never mention it", id)
		default:
			t.Errorf("message %s has %d Email change rows, want exactly 1", id, n)
		}
	}

	// The run's ACTUAL counters equal the preview's PLANNED ones.
	var run SetupRun
	var found bool
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		run, found, e = tx.GetSetupRun(ctx, runID)
		return e
	}); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !found {
		t.Fatal("the run vanished")
	}
	if run.Status != SetupStatusDone {
		t.Errorf("run status = %q, want %q", run.Status, SetupStatusDone)
	}
	if run.LabelsWritten != counts.Labels || run.Archived != counts.Archived || run.Moved != counts.Moves {
		t.Errorf("run wrote %d/%d/%d (labels/archived/moved), the preview promised %d/%d/%d",
			run.LabelsWritten, run.Archived, run.Moved, counts.Labels, counts.Archived, counts.Moves)
	}
	if run.PlannedLabels != counts.Labels || run.PlannedArchived != counts.Archived || run.PlannedMoves != counts.Moves {
		t.Errorf("the run's planned columns %d/%d/%d do not match the preview %d/%d/%d",
			run.PlannedLabels, run.PlannedArchived, run.PlannedMoves,
			counts.Labels, counts.Archived, counts.Moves)
	}
	if run.LeftForLater != counts.Leave || run.Unclassified != counts.Unclassified {
		t.Errorf("run left_for_later/unclassified = %d/%d, want %d/%d",
			run.LeftForLater, run.Unclassified, counts.Leave, counts.Unclassified)
	}
	if run.StartedAt == nil || run.FinishedAt == nil {
		t.Errorf("a finished run must carry startedAt and finishedAt: %+v", run)
	}

	// One journal row per message examined — the cursor, and the undo's evidence.
	var journal int
	f.scan(t, &journal, `SELECT count(*) FROM app.setup_journal WHERE user_id=$1 AND run_id=$2`,
		f.u.ID, runID.String())
	if journal != len(touched) {
		t.Errorf("journal has %d row(s), want %d (one per message the run examined)", journal, len(touched))
	}

	// A CHUNK AFTER Done TOUCHES NOTHING. Idempotence is what makes the job safe
	// to retry and safe to drive twice (the admin command drives chunks inline
	// while the dispatcher also holds a job for the same run).
	var changeBefore, changeAfter, journalAfter int
	f.scan(t, &changeBefore, `SELECT count(*) FROM app.change_log WHERE user_id=$1`, f.u.ID)
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		p, e := tx.SetupApplyChunk(ctx, runID, 5)
		if e != nil {
			return e
		}
		if !p.Done || p.Processed != 0 {
			t.Errorf("a chunk after Done reported %+v, want Done with nothing processed", p)
		}
		return nil
	}); err != nil {
		t.Fatalf("re-run chunk: %v", err)
	}
	f.scan(t, &changeAfter, `SELECT count(*) FROM app.change_log WHERE user_id=$1`, f.u.ID)
	f.scan(t, &journalAfter, `SELECT count(*) FROM app.setup_journal WHERE user_id=$1 AND run_id=$2`,
		f.u.ID, runID.String())
	if changeAfter != changeBefore {
		t.Errorf("a chunk after Done appended %d change row(s), want 0", changeAfter-changeBefore)
	}
	if journalAfter != journal {
		t.Errorf("a chunk after Done wrote %d journal row(s), want 0", journalAfter-journal)
	}
}

// ---- 4. the desk agrees ----------------------------------------------------

// TestSetupArchivedMailIsOffTheDesk is the deskLiveSQL probe's target.
//
// ⚠ IT IS A BEFORE/AFTER PAIR. The `before` half proves the fixture's message
// really is on the desk, really is counted, really is returned by
// `onDesk:true`, and really IS a drawer-sweep candidate — so the `after` half is
// observing an actual change of state rather than a message that never qualified
// for anything. Remove `m.desk_archived_at IS NULL` from deskLiveSQL and the
// after-half of every one of these four goes red.
func TestSetupArchivedMailIsOffTheDesk(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()

	// The tabbed older INBOX message is the subject: give it a below-the-line
	// score so it is a genuine drawer candidate, and let the desk preferences
	// materialise at their defaults (threshold 0.5, 30-day drawer).
	mustExec(t, f.h, `INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
		VALUES ($1,$2,0.1,'v0')`, f.u.ID, f.inboxOlderTabbed.String())

	inAreaSummary := func() int {
		var total int
		if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
			prefs, e := tx.GetDeskPreferences(ctx)
			if e != nil {
				return e
			}
			counts, e := tx.AreaSummary(ctx, DeskScope{Threshold: prefs.LineThreshold})
			if e != nil {
				return e
			}
			for _, c := range counts {
				total += c.Surface + c.Drawer + c.Unscored
			}
			return nil
		}); err != nil {
			t.Fatalf("area summary: %v", err)
		}
		return total
	}
	queryHas := func(q EmailQuery, want ulid.ULID) bool {
		var found bool
		if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
			ids, _, e := tx.QueryEmails(ctx, q)
			if e != nil {
				return e
			}
			for _, id := range ids {
				if id == want {
					found = true
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("query: %v", err)
		}
		return found
	}
	isDrawerCandidate := func() bool {
		var found bool
		if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
			cands, e := tx.DrawerCandidates(ctx, time.Now(), 200)
			if e != nil {
				return e
			}
			for _, c := range cands {
				if c.ID == f.inboxOlderTabbed {
					found = true
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("drawer candidates: %v", err)
		}
		return found
	}
	yes := true
	no := false

	// ── BEFORE: the negative control ──────────────────────────────────────────
	summaryBefore := inAreaSummary()
	if !queryHas(EmailQuery{OnDesk: &yes}, f.inboxOlderTabbed) {
		t.Fatal("the fixture message is not on the desk BEFORE the run; the after-checks below " +
			"would then prove nothing (CLAUDE.md: negative-control every safety assertion)")
	}
	if !isDrawerCandidate() {
		t.Fatal("the fixture message is not a drawer-sweep candidate BEFORE the run; " +
			"the after-check would then be vacuous")
	}
	if queryHas(EmailQuery{DeskArchived: &yes}, f.inboxOlderTabbed) {
		t.Fatal("the fixture message is already marked archived in Mailroom before the run")
	}

	f.applyRun(t, 5)

	// ── AFTER: the property under test ────────────────────────────────────────
	// THE TOTAL MOVES TWO WAYS IN ONE RUN, and both are exact. The three INBOX
	// messages outside the window LEAVE the desk (desk_archived_at — the
	// property under test). Folder D's thirteen ARRIVE: bringToDesk gives them a
	// canonical placement in INBOX, and the desk honours canonical placement the
	// moment it is written, as every other view does (#648 follow-up).
	//
	// ⚠ THIS USED TO ASSERT THAT THE TOTAL DROPPED, which was only true while the
	// desk ignored moved_mailbox_id: folder D's mail then stayed off the desk
	// until the reaper observed the IMAP move, up to an hour later — the very lag
	// that follow-up fixed. Re-armed, not relaxed: without the desk_archived_at
	// clause the total is before + 13 and this still goes red.
	const leftDesk, broughtToDesk = 3, 13
	summaryAfter := inAreaSummary()
	if want := summaryBefore - leftDesk + broughtToDesk; summaryAfter != want {
		t.Errorf("AreaSummary counts %d messages after the run and %d before; want %d — the %d INBOX "+
			"messages outside the window must leave the desk while folder D's %d arrive",
			summaryAfter, summaryBefore, want, leftDesk, broughtToDesk)
	}
	if queryHas(EmailQuery{OnDesk: &yes}, f.inboxOlderTabbed) {
		t.Error("Email/query onDesk:true still returns a day-one archived message")
	}
	if !queryHas(EmailQuery{OnDesk: &no}, f.inboxOlderTabbed) {
		t.Error("Email/query onDesk:false does not return a day-one archived message; it has to be " +
			"reachable somewhere, or the mail is simply gone from the client's point of view")
	}
	if !queryHas(EmailQuery{DeskArchived: &yes}, f.inboxOlderTabbed) {
		t.Error("Email/query deskArchived:true does not return a day-one archived message")
	}
	if queryHas(EmailQuery{DeskArchived: &no}, f.inboxOlderTabbed) {
		t.Error("Email/query deskArchived:false returns a day-one archived message")
	}
	if isDrawerCandidate() {
		t.Error("the drawer sweep still offers a day-one archived message. It would MOVE it on the " +
			"server, which is exactly what day-one promised would not happen — and a month later, " +
			"long after anybody would connect the two")
	}
	// The mark is readable per message, which is how Email.deskArchivedAt renders.
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		at, e := tx.DeskArchivedAt(ctx, []ulid.ULID{f.inboxOlderTabbed})
		if e != nil {
			return e
		}
		if _, ok := at[f.inboxOlderTabbed]; !ok {
			t.Error("DeskArchivedAt does not report the archived message")
		}
		return nil
	}); err != nil {
		t.Fatalf("desk archived at: %v", err)
	}
}

// ---- 5. the undo -----------------------------------------------------------

func TestSetupUndoToCompletion(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()
	runID, _, _ := f.applyRun(t, 5)

	// ONE label the user re-applies by hand after the run. It MUST survive the
	// undo: an undo that took it away would destroy the very filing this whole
	// feature exists to respect, and it is the reason LabelBySetup is its own
	// attribution rather than reusing LabelByUser.
	keeper := f.messagesIn[f.folderA][0]
	var labelAStr string
	f.scan(t, &labelAStr, `SELECT id FROM app.labels WHERE user_id=$1 AND lower(name)='folder a'`, f.u.ID)
	mustExec(t, f.h, `UPDATE app.message_labels SET applied_by='user'
		 WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
		f.u.ID, keeper.String(), labelAStr)

	var intentsBefore int
	f.scan(t, &intentsBefore, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.u.ID)

	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.BeginSetupUndo(ctx, runID)
	}); err != nil {
		t.Fatalf("begin undo: %v", err)
	}
	chunks := 0
	for {
		var p SetupProgress
		if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			p, e = tx.SetupUndoChunk(ctx, runID, 5)
			return e
		}); err != nil {
			t.Fatalf("undo chunk %d: %v", chunks+1, err)
		}
		chunks++
		if p.Done {
			break
		}
		if chunks > 100 {
			t.Fatal("undo did not finish in 100 chunks")
		}
	}
	if chunks < 3 {
		t.Errorf("the undo finished in %d chunk(s) with a limit of 5 over 42 journal rows", chunks)
	}

	// Every setup-applied label is gone...
	var setupLabels int
	f.scan(t, &setupLabels, `SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND applied_by='setup'`, f.u.ID)
	if setupLabels != 0 {
		t.Errorf("%d setup-applied label(s) survive the undo, want 0", setupLabels)
	}
	// ...except the one the user re-applied by hand.
	var keeperRows int
	f.scan(t, &keeperRows, `SELECT count(*) FROM app.message_labels
		 WHERE user_id=$1 AND message_id=$2 AND label_id=$3 AND applied_by='user'`,
		f.u.ID, keeper.String(), labelAStr)
	if keeperRows != 1 {
		t.Errorf("the label the USER re-applied by hand was removed by the undo (rows=%d, want 1). "+
			"That filing is theirs, not the run's", keeperRows)
	}

	// Nothing is archived in Mailroom any more.
	var stillArchived int
	f.scan(t, &stillArchived, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND desk_archived_at IS NOT NULL`, f.u.ID)
	if stillArchived != 0 {
		t.Errorf("%d message(s) are still archived in Mailroom after the undo, want 0", stillArchived)
	}

	// The moved messages are back in their folder, with a fresh intent each: the
	// server-side move is undone the only way it can be — by moving them back.
	var backInD int
	f.scan(t, &backInD, `SELECT count(*) FROM app.messages
		 WHERE user_id=$1 AND id = ANY($2) AND moved_mailbox_id = $3`,
		f.u.ID, ulidStrings(f.messagesIn[f.folderD]), f.folderD.String())
	if backInD != 13 {
		t.Errorf("%d of folder D's messages are canonically back in the folder, want 13", backInD)
	}
	var intentsAfter int
	f.scan(t, &intentsAfter, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1`, f.u.ID)
	if intentsAfter-intentsBefore != 13 {
		t.Errorf("the undo enqueued %d intent(s), want 13 (one move back per moved message)",
			intentsAfter-intentsBefore)
	}

	// The journal is empty and the run says undone.
	var journal int
	f.scan(t, &journal, `SELECT count(*) FROM app.setup_journal WHERE user_id=$1 AND run_id=$2`,
		f.u.ID, runID.String())
	if journal != 0 {
		t.Errorf("%d journal row(s) survive the undo, want 0", journal)
	}
	var run SetupRun
	var found bool
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		run, found, e = tx.GetSetupRun(ctx, runID)
		return e
	}); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !found || run.Status != SetupStatusUndone || run.UndoneAt == nil {
		t.Errorf("run after undo = %+v, want status %q with undoneAt set", run, SetupStatusUndone)
	}
	// Change rows were appended for the reversal too: an undo is a mutation of
	// every message it touches, and a client that never heard about it would show
	// labels that are gone.
	var undoChanges int
	f.scan(t, &undoChanges, `SELECT count(*) FROM app.change_log
		 WHERE user_id=$1 AND object_type='Email' AND op='updated' AND object_id = ANY($2)`,
		f.u.ID, ulidStrings(f.messagesIn[f.folderD]))
	if undoChanges != 2*13 {
		t.Errorf("folder D's messages carry %d Email change rows after apply+undo, want %d "+
			"(one each way)", undoChanges, 2*13)
	}
}

// An undo is REFUSED while the run is still going. The client disables Undo
// until the run is done; this is the server making it a rule, because a
// concurrent undo would race the apply's own chunks over the same journal.
func TestSetupUndoRefusedWhileRunning(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()

	var runID ulid.ULID
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		counts, e := tx.SetupPreview(ctx, f.plan())
		if e != nil {
			return e
		}
		runID, e = tx.CreateSetupRun(ctx, f.plan(), counts)
		return e
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// One chunk, so the run is 'running' rather than 'queued'.
	if err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		_, e := tx.SetupApplyChunk(ctx, runID, 5)
		return e
	}); err != nil {
		t.Fatalf("first chunk: %v", err)
	}

	err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.BeginSetupUndo(ctx, runID)
	})
	if err == nil {
		t.Fatal("BeginSetupUndo accepted a still-running run")
	}
	if !isSetupBusy(err) {
		t.Errorf("BeginSetupUndo on a running run: %v, want ErrSetupRunBusy", err)
	}

	// A second run cannot be created while one is outstanding either.
	err = f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
		counts, e := tx.SetupPreview(ctx, f.plan())
		if e != nil {
			return e
		}
		_, e = tx.CreateSetupRun(ctx, f.plan(), counts)
		return e
	})
	if err == nil {
		t.Fatal("CreateSetupRun created a second run while one was outstanding")
	}
	if !isSetupBusy(err) {
		t.Errorf("CreateSetupRun with a run outstanding: %v, want ErrSetupRunBusy", err)
	}
}

// ---- validation ------------------------------------------------------------

func TestSetupPlanValidation(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		plan SetupPlan
	}{
		{"a window of zero days", SetupPlan{WindowDays: 0}},
		{"a negative window", SetupPlan{WindowDays: -7}},
		{"an unknown mailbox id", SetupPlan{WindowDays: 30, Folders: []SetupFolderChoice{
			{MailboxID: NewULID(), Action: SetupActionLabelArchive}}}},
		{"a SPECIAL-USE mailbox (mapped without asking)", SetupPlan{WindowDays: 30, Folders: []SetupFolderChoice{
			{MailboxID: f.archiveID, Action: SetupActionLabelArchive}}}},
		{"bringToDesk on INBOX itself", SetupPlan{WindowDays: 30, Folders: []SetupFolderChoice{
			{MailboxID: f.inboxID, Action: SetupActionBringToDesk}}}},
		{"an unknown action", SetupPlan{WindowDays: 30, Folders: []SetupFolderChoice{
			{MailboxID: f.folderA, Action: "deleteEverything"}}}},
		{"the same folder twice", SetupPlan{WindowDays: 30, Folders: []SetupFolderChoice{
			{MailboxID: f.folderA, Action: SetupActionLabelArchive},
			{MailboxID: f.folderA, Action: SetupActionLeave}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.s.UserTx(ctx, f.userID, func(tx *UserTx) error {
				_, e := tx.SetupPreview(ctx, tc.plan)
				return e
			})
			if err == nil {
				t.Fatalf("SetupPreview accepted %s", tc.name)
			}
			if !isSetupPlanInvalid(err) {
				t.Errorf("SetupPreview(%s): %v, want ErrSetupPlanInvalid", tc.name, err)
			}
		})
	}
}

// ---- 6. RLS ----------------------------------------------------------------

// A second user can see nothing of the first's run, and — the one that would
// actually destroy mail — cannot drive its undo.
func TestSetupRunsAreUserIsolated(t *testing.T) {
	f := newSetupFixture(t)
	ctx := context.Background()
	runID, _, _ := f.applyRun(t, 20)

	other := seedUser(t, f.h.super, "setup-other-"+NewULID().String())
	otherID := mustULID(t, other.ID)

	if err := f.s.UserTx(ctx, otherID, func(tx *UserTx) error {
		runs, e := tx.ListSetupRuns(ctx)
		if e != nil {
			return e
		}
		// The other user has a run OF THEIR OWN (seedUser seeds one for every
		// fixture user, so the RLS inventory has a row to probe). The claim is
		// therefore "none of these is the first user's", not "there are none" —
		// the weaker form would break the moment a fixture grew a run, which is
		// exactly what happened.
		for _, r := range runs {
			if r.ID == runID {
				t.Errorf("the other user's ListSetupRuns returned the FIRST user's run %s", runID)
			}
		}
		_, found, e := tx.GetSetupRun(ctx, runID)
		if e != nil {
			return e
		}
		if found {
			t.Error("the other user can read the first user's setup run")
		}
		return nil
	}); err != nil {
		t.Fatalf("other user reads: %v", err)
	}

	// The write side. Snapshot what the run produced, try to undo it as the other
	// user, and prove nothing moved.
	var labelsBefore, archivedBefore, journalBefore int
	f.scan(t, &labelsBefore, `SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND applied_by='setup'`, f.u.ID)
	f.scan(t, &archivedBefore, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND desk_archived_at IS NOT NULL`, f.u.ID)
	f.scan(t, &journalBefore, `SELECT count(*) FROM app.setup_journal WHERE user_id=$1`, f.u.ID)
	if labelsBefore == 0 || archivedBefore == 0 || journalBefore == 0 {
		t.Fatalf("the fixture has nothing for a cross-user undo to destroy (labels=%d archived=%d journal=%d); "+
			"the assertions below would be vacuous", labelsBefore, archivedBefore, journalBefore)
	}

	err := f.s.UserTx(ctx, otherID, func(tx *UserTx) error {
		_, e := tx.SetupUndoChunk(ctx, runID, 100)
		return e
	})
	if err == nil {
		t.Error("the other user's SetupUndoChunk on a foreign run succeeded; it must fail closed")
	}
	err = f.s.UserTx(ctx, otherID, func(tx *UserTx) error {
		_, e := tx.SetupApplyChunk(ctx, runID, 100)
		return e
	})
	if err == nil {
		t.Error("the other user's SetupApplyChunk on a foreign run succeeded; it must fail closed")
	}

	var labelsAfter, archivedAfter, journalAfter int
	f.scan(t, &labelsAfter, `SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND applied_by='setup'`, f.u.ID)
	f.scan(t, &archivedAfter, `SELECT count(*) FROM app.messages WHERE user_id=$1 AND desk_archived_at IS NOT NULL`, f.u.ID)
	f.scan(t, &journalAfter, `SELECT count(*) FROM app.setup_journal WHERE user_id=$1`, f.u.ID)
	if labelsAfter != labelsBefore || archivedAfter != archivedBefore || journalAfter != journalBefore {
		t.Errorf("a cross-user undo attempt changed the first user's state: labels %d->%d, "+
			"archived %d->%d, journal %d->%d",
			labelsBefore, labelsAfter, archivedBefore, archivedAfter, journalBefore, journalAfter)
	}
}

// ---- 8. DeskPreferences.setupCompletedAt -----------------------------------

func TestDeskPreferencesSetupCompletedAt(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "setupdone-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// A login materialising its preferences AFTER migration 00050 starts out
	// NULL: it has not been through day-one, so the desk opens to setup.
	var prefs DeskPreferences
	var afterCreate int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if prefs, e = tx.GetDeskPreferences(ctx); e != nil {
			return e
		}
		afterCreate, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("get prefs: %v", err)
	}
	if prefs.SetupCompletedAt != nil {
		t.Errorf("a fresh login's SetupCompletedAt = %v, want nil (it has not been through day-one)",
			prefs.SetupCompletedAt)
	}

	// Setting it appends exactly one change row...
	when := time.Now().UTC().Truncate(time.Second)
	var afterSet int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if e := tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{
			SetSetupCompletedAt: true, SetupCompletedAt: &when}); e != nil {
			return e
		}
		got, e := tx.GetDeskPreferences(ctx)
		if e != nil {
			return e
		}
		if got.SetupCompletedAt == nil || !got.SetupCompletedAt.Equal(when) {
			t.Errorf("SetupCompletedAt = %v, want %v", got.SetupCompletedAt, when)
		}
		afterSet, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if afterSet-afterCreate != 1 {
		t.Errorf("setting setupCompletedAt appended %d change row(s), want 1", afterSet-afterCreate)
	}

	// ...and clearing it back to NULL appends exactly one more. The three-state
	// patch is what makes this expressible at all: with a bare nil pointer,
	// "leave it" and "clear it" would be the same instruction.
	var afterClear int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if e := tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{
			SetSetupCompletedAt: true, SetupCompletedAt: nil}); e != nil {
			return e
		}
		got, e := tx.GetDeskPreferences(ctx)
		if e != nil {
			return e
		}
		if got.SetupCompletedAt != nil {
			t.Errorf("SetupCompletedAt after clearing = %v, want nil", got.SetupCompletedAt)
		}
		afterClear, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if afterClear-afterSet != 1 {
		t.Errorf("clearing setupCompletedAt appended %d change row(s), want 1", afterClear-afterSet)
	}

	// A patch that does NOT mention it leaves it alone and appends nothing.
	var afterNoop int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		nag := 9
		if e := tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{TrayNagDays: &nag}); e != nil {
			return e
		}
		if e := tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{TrayNagDays: &nag}); e != nil {
			return e
		}
		var e error
		afterNoop, e = tx.ChangeState(ctx, ObjectDeskPreferences)
		return e
	}); err != nil {
		t.Fatalf("noop patch: %v", err)
	}
	if afterNoop-afterClear != 1 {
		t.Errorf("one real change plus one re-assertion appended %d change row(s), want 1",
			afterNoop-afterClear)
	}
}

// ---- helpers ---------------------------------------------------------------

func isSetupBusy(err error) bool        { return errors.Is(err, ErrSetupRunBusy) }
func isSetupPlanInvalid(err error) bool { return errors.Is(err, ErrSetupPlanInvalid) }
