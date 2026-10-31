// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the drawer auto-archive sweep (#443, migration
// 00048): DrawerCandidates' exclusions, the login-default/own-period
// inheritance, ArchiveByDrawer's move-record-and-nothing-else, and
// DrawerReport's per-signal counts.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// drawerFixture is a hand-built user (not seedUser/fixtureUser: the drawer
// sweep needs precise control over provenance mailboxes, ages and scores
// that the shared fixture does not expose) with two accounts — one carrying
// an Archive mailbox, one deliberately without — and three mailboxes on the
// first: INBOX, Archive, and a plain folder that does NOT feed the desk.
type drawerFixture struct {
	h      *harness
	st     *Store
	userID ulid.ULID

	accountID   ulid.ULID
	inbox       ulid.ULID
	archive     ulid.ULID
	plainFolder ulid.ULID

	noArchiveAccountID ulid.ULID
	noArchiveInbox     ulid.ULID

	nextUID int64 // provenance_live_uq is unique per (mailbox, uidvalidity, uid)
}

func newDrawerFixture(t *testing.T) *drawerFixture {
	t.Helper()
	h := newHarness(t)
	f := &drawerFixture{
		h: h, st: h.appStore(),
		userID:      NewULID(),
		accountID:   NewULID(),
		inbox:       NewULID(),
		archive:     NewULID(),
		plainFolder: NewULID(),

		noArchiveAccountID: NewULID(),
		noArchiveInbox:     NewULID(),
	}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com")
	for _, a := range []struct {
		id   ulid.ULID
		name string
	}{
		{f.accountID, "primary"}, {f.noArchiveAccountID, "no-archive"},
	} {
		exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		      VALUES ($1, $2, $3, 'mail.example.com', 'smtp.example.com', '\x00', $4)`,
			a.id.String(), f.userID.String(), a.name, NewULID().String())
	}
	for _, m := range []struct {
		id        ulid.ULID
		account   ulid.ULID
		name      string
		role      string
		feedsDesk bool
	}{
		{f.inbox, f.accountID, "INBOX", "", false},
		{f.archive, f.accountID, "Archive", `\Archive`, false},
		{f.plainFolder, f.accountID, "Stuff", "", false},
		{f.noArchiveInbox, f.noArchiveAccountID, "INBOX", "", false},
	} {
		exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use, feeds_desk)
		      VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''), $6)`,
			m.id.String(), f.userID.String(), m.account.String(), m.name, m.role, m.feedsDesk)
	}
	return f
}

// createArea creates an area (own label minted) with the given auto-archive
// settings and returns its view.
func (f *drawerFixture) createArea(t *testing.T, name string, autoArchive bool, days *int) AreaView {
	t.Helper()
	ctx := context.Background()
	var id ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		aa := autoArchive
		var e error
		id, e = tx.CreateArea(ctx, AreaRow{Name: name, AutoArchive: &aa, AutoArchiveDays: days})
		return e
	}); err != nil {
		t.Fatalf("create area %s: %v", name, err)
	}
	var views []AreaView
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		views, e = tx.GetAreas(ctx, []ulid.ULID{id})
		return e
	}); err != nil {
		t.Fatalf("get area %s: %v", name, err)
	}
	if len(views) != 1 {
		t.Fatalf("get area %s: got %d views, want 1", name, len(views))
	}
	return views[0]
}

// msgOpt is one seeded message's shape. The zero value is a message that
// qualifies for nothing (unlabelled, unscored) — every field below is an
// override toward the "true candidate" shape a given test builds up from.
type msgOpt struct {
	daysOld        int
	areaLabel      ulid.ULID // message_labels row resolving it into an area
	score          *float32
	flagged        bool
	pinned         bool
	snoozed        bool
	movedElsewhere bool       // moved_mailbox_id set (UPDATE after insert, provenance untouched)
	inPlainFolder  bool       // provenance in the plain folder instead of INBOX
	account        ulid.ULID  // zero = f.accountID
	alreadySwept   *ulid.ULID // area id to record in app.drawer_archives, if any
}

func score32(f float32) *float32 { return &f }

// seedMessage inserts one message shaped by opt and returns its id.
func (f *drawerFixture) seedMessage(t *testing.T, subject string, opt msgOpt) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	threadID := NewULID()
	receivedAt := time.Now().UTC().AddDate(0, 0, -opt.daysOld)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message %s: %v", subject, err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
	      VALUES ($1, $2, $3, $4, $5, $6, 10)`,
		id.String(), f.userID.String(), hash[:], threadID.String(), subject, receivedAt)

	account := opt.account
	if account == (ulid.ULID{}) {
		account = f.accountID
	}
	mailbox := f.inbox
	switch {
	case account == f.noArchiveAccountID:
		mailbox = f.noArchiveInbox
	case opt.inPlainFolder:
		mailbox = f.plainFolder
	}
	f.nextUID++
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
	      VALUES ($1, $2, $3, $4, $5, $6, 1, 'live')`,
		NewULID().String(), f.userID.String(), id.String(), account.String(), mailbox.String(), f.nextUID)

	if opt.areaLabel != (ulid.ULID{}) {
		exec(`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
		      VALUES ($1, $2, $3, 'user')`,
			f.userID.String(), id.String(), opt.areaLabel.String())
	}

	if opt.score != nil {
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			return tx.UpsertTriageScore(ctx, id, float64(*opt.score), "test-model", nil)
		}); err != nil {
			t.Fatalf("seed score for %s: %v", subject, err)
		}
	}

	var up EmailUpdate
	touched := false
	if opt.flagged {
		v := true
		up.Flagged = &v
		touched = true
	}
	if opt.pinned {
		v := true
		up.Pinned = &v
		touched = true
	}
	if opt.snoozed {
		at := time.Now().Add(24 * time.Hour)
		up.SnoozeUntil = &at
		touched = true
	}
	if touched {
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			_, _, e := tx.ApplyEmailUpdate(ctx, id, up)
			return e
		}); err != nil {
			t.Fatalf("seed flags for %s: %v", subject, err)
		}
	}

	if opt.movedElsewhere {
		exec(`UPDATE app.messages SET moved_mailbox_id = $2 WHERE user_id = $1 AND id = $3`,
			f.userID.String(), f.plainFolder.String(), id.String())
	}

	if opt.alreadySwept != nil {
		exec(`INSERT INTO app.drawer_archives (user_id, message_id, area_id, area_name, period_days)
		      VALUES ($1, $2, $3, 'pre-swept', 30)`,
			f.userID.String(), id.String(), opt.alreadySwept.String())
	}
	return id
}

func (f *drawerFixture) emailChanges(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("email changes: %v", err)
	}
	return n
}

func (f *drawerFixture) moveIntentCount(t *testing.T, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents WHERE user_id = $1 AND message_id = $2 AND op = 'move'`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("move intent count: %v", err)
	}
	return n
}

func (f *drawerFixture) isSeen(t *testing.T, msg ulid.ULID) bool {
	t.Helper()
	var seen bool
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT is_seen FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&seen); err != nil {
		t.Fatalf("is_seen: %v", err)
	}
	return seen
}

func (f *drawerFixture) movedMailbox(t *testing.T, msg ulid.ULID) string {
	t.Helper()
	var m string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT COALESCE(moved_mailbox_id, '') FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&m); err != nil {
		t.Fatalf("moved_mailbox_id: %v", err)
	}
	return m
}

func (f *drawerFixture) messageExists(t *testing.T, msg ulid.ULID) bool {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&n); err != nil {
		t.Fatalf("message exists: %v", err)
	}
	return n == 1
}

// candidateIDs is a convenience for asserting on the SET of candidate ids
// DrawerCandidates returned, independent of order.
func candidateIDs(cs []DrawerCandidate) map[ulid.ULID]DrawerCandidate {
	out := make(map[ulid.ULID]DrawerCandidate, len(cs))
	for _, c := range cs {
		out[c.ID] = c
	}
	return out
}

// TestDrawerCandidatesRespectEveryExclusion pins every predicate
// DrawerCandidates applies, one seeded message per exclusion, against the
// SAME true-candidate shape.
func TestDrawerCandidatesRespectEveryExclusion(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()

	area := f.createArea(t, "Sweeps", true, intPtr(30))
	offArea := f.createArea(t, "Off", false, intPtr(30))

	below := score32(0.1) // desk_preferences.line_threshold defaults to 0.5
	above := score32(0.9)

	trueCandidate := f.seedMessage(t, "true candidate", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below,
	})
	flagged := f.seedMessage(t, "flagged", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, flagged: true,
	})
	pinned := f.seedMessage(t, "pinned", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, pinned: true,
	})
	snoozed := f.seedMessage(t, "snoozed", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, snoozed: true,
	})
	aboveLine := f.seedMessage(t, "above the line", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: above,
	})
	unscored := f.seedMessage(t, "unscored", msgOpt{
		daysOld: 31, areaLabel: area.LabelID,
	})
	tooYoung := f.seedMessage(t, "too young", msgOpt{
		daysOld: 1, areaLabel: area.LabelID, score: below,
	})
	alreadySwept := f.seedMessage(t, "already swept", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, alreadySwept: &area.ID,
	})
	movedAway := f.seedMessage(t, "moved elsewhere", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, movedElsewhere: true,
	})
	notOnDesk := f.seedMessage(t, "plain folder", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, inPlainFolder: true,
	})
	areaOff := f.seedMessage(t, "area off", msgOpt{
		daysOld: 31, areaLabel: offArea.LabelID, score: below,
	})

	var candidates []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		candidates, e = tx.DrawerCandidates(ctx, time.Now(), 100)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates: %v", err)
	}

	got := candidateIDs(candidates)
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want exactly 1 — %+v", len(got), candidates)
	}
	if _, ok := got[trueCandidate]; !ok {
		t.Errorf("the true candidate was excluded: %+v", candidates)
	}
	for name, id := range map[string]ulid.ULID{
		"flagged": flagged, "pinned": pinned, "snoozed": snoozed,
		"above the line": aboveLine, "unscored": unscored, "too young": tooYoung,
		"already swept": alreadySwept, "moved elsewhere": movedAway,
		"not on desk": notOnDesk, "area off": areaOff,
	} {
		if _, ok := got[id]; ok {
			t.Errorf("%s was NOT excluded, and it should have been: %+v", name, candidates)
		}
	}
}

// TestDrawerPeriodInheritsTheLoginDefault pins the NULL-means-inherit
// semantics: an area with no own period follows desk_preferences'
// drawer_default_days, moves when that default changes, and an own period
// overrides it regardless of the default.
func TestDrawerPeriodInheritsTheLoginDefault(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()

	inherits := f.createArea(t, "Inherits", true, nil)
	below := score32(0.1)

	msg := f.seedMessage(t, "inherits default", msgOpt{
		daysOld: 31, areaLabel: inherits.LabelID, score: below,
	})

	assertCandidateFor := func(want bool, why string) {
		t.Helper()
		var cs []DrawerCandidate
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			cs, e = tx.DrawerCandidates(ctx, time.Now(), 100)
			return e
		}); err != nil {
			t.Fatalf("drawer candidates: %v", err)
		}
		_, is := candidateIDs(cs)[msg]
		if is != want {
			t.Errorf("%s: candidate = %v, want %v", why, is, want)
		}
	}

	// 30-day default (schema default), message is 31 days old: a candidate.
	assertCandidateFor(true, "31 days old against the 30-day default")

	// Raise the default past the message's age: no longer a candidate.
	longer := 40
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{DrawerDefaultDays: &longer})
	}); err != nil {
		t.Fatalf("raise default: %v", err)
	}
	assertCandidateFor(false, "31 days old against a raised 40-day default")

	// An area with its OWN period overrides the (now 40-day) default.
	own := f.createArea(t, "OwnPeriod", true, intPtr(25))
	ownMsg := f.seedMessage(t, "own period overrides", msgOpt{
		daysOld: 26, areaLabel: own.LabelID, score: below,
	})
	var cs []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		cs, e = tx.DrawerCandidates(ctx, time.Now(), 100)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates: %v", err)
	}
	if _, ok := candidateIDs(cs)[ownMsg]; !ok {
		t.Errorf("a 26-day-old message in a 25-day-period area was not a candidate despite a 40-day default: %+v", cs)
	}
}

// TestArchiveByDrawerMovesAndRecordsAndNothingElse pins ArchiveByDrawer's
// whole contract: the move, the ONE change row, the ONE writeback intent,
// the audit row, is_seen untouched, idempotency, and the no-archive-folder
// refusal.
func TestArchiveByDrawerMovesAndRecordsAndNothingElse(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()

	area := f.createArea(t, "Sweeps", true, intPtr(30))
	below := score32(0.1)
	now := time.Now()

	msg := f.seedMessage(t, "archive me", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below})
	if f.isSeen(t, msg) {
		t.Fatal("fixture bug: message seeded as already seen, this test needs it unread")
	}
	before := f.emailChanges(t, msg)

	var candidates []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		candidates, e = tx.DrawerCandidates(ctx, now, 10)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates: %v", err)
	}
	cs := candidateIDs(candidates)
	c, ok := cs[msg]
	if !ok {
		t.Fatalf("seeded message was not a candidate: %+v", candidates)
	}

	var archived bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		archived, e = tx.ArchiveByDrawer(ctx, c, now)
		return e
	}); err != nil {
		t.Fatalf("archive by drawer: %v", err)
	}
	if !archived {
		t.Fatal("ArchiveByDrawer reported false for a message that qualified")
	}

	if got := f.movedMailbox(t, msg); got != f.archive.String() {
		t.Errorf("moved_mailbox_id = %q, want the Archive mailbox %q", got, f.archive.String())
	}
	if got := f.emailChanges(t, msg) - before; got != 1 {
		t.Errorf("Email change rows appended = %d, want exactly 1", got)
	}
	if got := f.moveIntentCount(t, msg); got != 1 {
		t.Errorf("move writeback intents = %d, want exactly 1 (one live account)", got)
	}
	if f.isSeen(t, msg) {
		t.Error("ArchiveByDrawer marked the message read; it must never touch is_seen")
	}
	if !f.messageExists(t, msg) {
		t.Error("ArchiveByDrawer deleted the message row; it must never delete anything")
	}

	var areaID, areaName string
	var periodDays int
	if err := f.h.super.QueryRow(ctx,
		`SELECT area_id, area_name, period_days FROM app.drawer_archives WHERE user_id = $1 AND message_id = $2`,
		f.userID.String(), msg.String()).Scan(&areaID, &areaName, &periodDays); err != nil {
		t.Fatalf("drawer_archives row: %v", err)
	}
	if areaID != area.ID.String() || areaName != "Sweeps" || periodDays != 30 {
		t.Errorf("drawer_archives row = (area_id=%s, area_name=%s, period_days=%d), want (%s, Sweeps, 30)",
			areaID, areaName, periodDays, area.ID.String())
	}

	// A second call: not a candidate any more (moved_mailbox_id is set, and
	// it is already in app.drawer_archives either way), so nothing changes.
	beforeSecond := f.emailChanges(t, msg)
	var archivedAgain bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		archivedAgain, e = tx.ArchiveByDrawer(ctx, c, now)
		return e
	}); err != nil {
		t.Fatalf("second archive by drawer: %v", err)
	}
	if archivedAgain {
		t.Error("a second ArchiveByDrawer call reported archived=true")
	}
	if got := f.emailChanges(t, msg) - beforeSecond; got != 0 {
		t.Errorf("second call appended %d Email change rows, want 0", got)
	}

	// Moving it back to INBOX does NOT make it a candidate again — the
	// app.drawer_archives row is the durable "swept once" guard.
	if _, err := f.h.super.Exec(ctx,
		`UPDATE app.messages SET moved_mailbox_id = NULL WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()); err != nil {
		t.Fatalf("move back to inbox: %v", err)
	}
	var after []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		after, e = tx.DrawerCandidates(ctx, now, 100)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates after restore: %v", err)
	}
	if _, ok := candidateIDs(after)[msg]; ok {
		t.Error("a message restored to INBOX after being swept became a candidate again")
	}

	// An account with no Archive folder: not archived.
	noArchiveMsg := f.seedMessage(t, "no archive account", msgOpt{
		daysOld: 31, areaLabel: area.LabelID, score: below, account: f.noArchiveAccountID,
	})
	var naCandidates []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		naCandidates, e = tx.DrawerCandidates(ctx, now, 100)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates (no archive account): %v", err)
	}
	if _, ok := candidateIDs(naCandidates)[noArchiveMsg]; ok {
		t.Fatalf("a message whose only account has no Archive mailbox was offered as a DrawerCandidate: %+v — "+
			"the report files it under no-archive-folder, so the sweep must not count it either, and a batch "+
			"of such messages must not starve the archivable ones behind them", naCandidates)
	}
	// ArchiveByDrawer holds the same line on its own, for a candidate handed
	// to it directly (the re-check is the last defence, not the only one).
	naC := DrawerCandidate{ID: noArchiveMsg, AreaID: area.ID, AreaName: area.Name, PeriodDays: 30}
	var naArchived bool
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		naArchived, e = tx.ArchiveByDrawer(ctx, naC, now)
		return e
	}); err != nil {
		t.Fatalf("archive by drawer (no archive account): %v", err)
	}
	if naArchived {
		t.Error("ArchiveByDrawer archived a message whose only account has no Archive mailbox")
	}

	// And it shows up in the report's own no-archive-folder signal.
	var report DrawerReportRows
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		report, e = tx.DrawerReport(ctx, now)
		return e
	}); err != nil {
		t.Fatalf("drawer report: %v", err)
	}
	for _, a := range report.Areas {
		if a.AreaID != nil && *a.AreaID == area.ID {
			if a.Excluded.NoArchiveFolder < 1 {
				t.Errorf("area %s NoArchiveFolder = %d, want at least 1", a.AreaName, a.Excluded.NoArchiveFolder)
			}
		}
	}
}

// TestDrawerReportCountsPerSignal seeds one message per DrawerReport
// exclusion signal (the report's OWN seven, distinct from DrawerCandidates'
// ten — no "too young" or "area off" here, since those never reach the
// "old enough, on the desk, in this area" population the report counts) and
// checks each shows up exactly once, alongside the one true candidate.
func TestDrawerReportCountsPerSignal(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()

	area := f.createArea(t, "Reported", true, intPtr(30))
	below := score32(0.1)
	above := score32(0.9)

	trueCandidate := f.seedMessage(t, "true candidate", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below})
	_ = f.seedMessage(t, "flagged", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below, flagged: true})
	_ = f.seedMessage(t, "pinned", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below, pinned: true})
	_ = f.seedMessage(t, "snoozed", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below, snoozed: true})
	_ = f.seedMessage(t, "above the line", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: above})
	_ = f.seedMessage(t, "unscored", msgOpt{daysOld: 31, areaLabel: area.LabelID})
	_ = f.seedMessage(t, "already swept", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below, alreadySwept: &area.ID})
	_ = f.seedMessage(t, "no archive account", msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below, account: f.noArchiveAccountID})

	var report DrawerReportRows
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		report, e = tx.DrawerReport(ctx, time.Now())
		return e
	}); err != nil {
		t.Fatalf("drawer report: %v", err)
	}

	var row *DrawerAreaReport
	for i := range report.Areas {
		if report.Areas[i].AreaID != nil && *report.Areas[i].AreaID == area.ID {
			row = &report.Areas[i]
		}
	}
	if row == nil {
		t.Fatalf("area %s not found in report: %+v", area.ID, report.Areas)
	}

	if row.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1 (%s)", row.Candidates, trueCandidate)
	}
	for name, got := range map[string]int{
		"Flagged": row.Excluded.Flagged, "Pinned": row.Excluded.Pinned,
		"Snoozed": row.Excluded.Snoozed, "AboveLine": row.Excluded.AboveLine,
		"Unscored": row.Excluded.Unscored, "AlreadySwept": row.Excluded.AlreadySwept,
		"NoArchiveFolder": row.Excluded.NoArchiveFolder,
	} {
		if got != 1 {
			t.Errorf("%s = %d, want 1: %+v", name, got, row.Excluded)
		}
	}
}

func intPtr(n int) *int { return &n }
