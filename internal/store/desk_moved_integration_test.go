// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE DESK HONOURS THE CANONICAL PLACEMENT (#648 follow-up).
//
// Observed in practice: after an archive, moved_mailbox_id = Archive
// and the provider really had moved the message, but its INBOX provenance row
// stayed 'live' until that folder's next hourly live-UID observation — by
// design (reaper.go's RetireCanonicalPlacements is the bridge). deskLiveSQL
// read provenance alone, so the archived message stayed on the desk (onDesk,
// Area/getSummary) for up to an hour while every other view had moved it.
//
// Four messages, one per arm of the new predicate, all in the SAME
// auto-archiving area and old enough to sweep, all below the line except
// restoring (so the summary can tell it from archived), so what differs
// between them is where they are:
//
//	archived   provenance INBOX,   moved -> Archive   NOT on the desk (the bug)
//	restoring  provenance Archive, moved -> INBOX     ON the desk (the control)
//	inbox      provenance INBOX,   no move            ON the desk (unchanged)
//	filed      provenance Stuff,   no move            NOT on the desk (unchanged)

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestDeskHonoursTheCanonicalPlacement(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()
	area := f.createArea(t, "Desk", true, intPtr(30))
	below := score32(0.1)

	opt := msgOpt{daysOld: 31, areaLabel: area.LabelID, score: below}
	archived := f.seedMessage(t, "archived, move not observed yet", opt)
	// restoring scores ABOVE the line, so the summary below can tell it apart
	// from archived: counting the wrong one of the two changes WHICH bucket
	// moves, not only the total.
	restoringOpt := opt
	restoringOpt.score = score32(0.9)
	restoring := f.seedMessage(t, "restored, move not observed yet", restoringOpt)
	inbox := f.seedMessage(t, "plain inbox", opt)
	filedOpt := opt
	filedOpt.inPlainFolder = true
	filed := f.seedMessage(t, "plain folder", filedOpt)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	// archived: the INBOX provenance row stays live (the reaper has not run);
	// only the canonical placement says Archive — exactly what ApplyEmailUpdate
	// leaves after `e`.
	exec(`UPDATE app.messages SET moved_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), archived.String(), f.archive.String())
	// restoring: the bytes are in Archive, the user has put it back in INBOX.
	exec(`UPDATE app.message_provenance SET mailbox_id = $3 WHERE user_id = $1 AND message_id = $2`,
		f.userID.String(), restoring.String(), f.archive.String())
	exec(`UPDATE app.messages SET moved_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), restoring.String(), f.inbox.String())

	query := func(onDesk bool, account *ulid.ULID) map[ulid.ULID]bool {
		t.Helper()
		got := map[ulid.ULID]bool{}
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			ids, _, e := tx.QueryEmails(ctx, EmailQuery{OnDesk: &onDesk, InAccount: account})
			for _, id := range ids {
				got[id] = true
			}
			return e
		}); err != nil {
			t.Fatalf("query onDesk=%v: %v", onDesk, err)
		}
		return got
	}

	want := map[string]struct {
		id     ulid.ULID
		onDesk bool
	}{
		"archived (INBOX provenance live, moved to Archive)": {archived, false},
		"restoring (Archive provenance, moved to INBOX)":     {restoring, true},
		"inbox (INBOX provenance, no move)":                  {inbox, true},
		"filed (plain-folder provenance, no move)":           {filed, false},
	}

	onDesk, offDesk := query(true, nil), query(false, nil)
	for name, w := range want {
		if onDesk[w.id] != w.onDesk {
			t.Errorf("%s: in Email/query onDesk:true = %v, want %v", name, onDesk[w.id], w.onDesk)
		}
		// The NOT(...) side: a NULL anywhere in the predicate would drop the
		// message from BOTH lists. Each message must be in exactly one.
		if offDesk[w.id] == w.onDesk {
			t.Errorf("%s: in Email/query onDesk:false = %v, want %v — each message must be in exactly "+
				"one of onDesk:true and onDesk:false (a NULL predicate would drop it from both)", name, offDesk[w.id], !w.onDesk)
		}
	}

	// Account-scoped: both on-desk messages are the primary account's.
	scoped := query(true, &f.accountID)
	if !scoped[restoring] || !scoped[inbox] || scoped[archived] {
		t.Errorf("account-scoped onDesk: restoring=%v inbox=%v archived=%v, want true/true/false",
			scoped[restoring], scoped[inbox], scoped[archived])
	}
	other := query(true, &f.noArchiveAccountID)
	if other[restoring] || other[inbox] {
		t.Errorf("scoped to the OTHER account, restoring=%v inbox=%v are on its desk — "+
			"a canonical placement in the primary account's INBOX must not count there",
			other[restoring], other[inbox])
	}

	// Area/getSummary counts the same set: restoring (surface) + inbox (drawer).
	var summary []AreaCounts
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		summary, e = tx.AreaSummary(ctx, DeskScope{Threshold: 0.5})
		return e
	}); err != nil {
		t.Fatalf("area summary: %v", err)
	}
	var got *AreaCounts
	for i := range summary {
		if summary[i].AreaID != nil && *summary[i].AreaID == area.ID {
			got = &summary[i]
		}
	}
	if got == nil {
		t.Fatal("the area is missing from the summary")
	}
	if got.Surface != 1 || got.Drawer != 1 || got.Unscored != 0 {
		t.Errorf("area counts = %+v, want Surface 1 (restoring), Drawer 1 (inbox), Unscored 0 — "+
			"the archived message must not be counted", *got)
	}

	// The drawer sweep: the archived message is not a candidate (it has already
	// been moved), while the plain inbox message — same age, area and score —
	// is. The positive half is what proves the negative one was reachable.
	var cands []DrawerCandidate
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		cands, e = tx.DrawerCandidates(ctx, time.Now(), 100)
		return e
	}); err != nil {
		t.Fatalf("drawer candidates: %v", err)
	}
	c := candidateIDs(cands)
	if _, ok := c[archived]; ok {
		t.Error("the archived message is a drawer-sweep candidate; it has already been moved away")
	}
	if _, ok := c[inbox]; !ok {
		t.Errorf("the plain inbox message is NOT a sweep candidate, so the exclusion above proves nothing: %+v", cands)
	}
}
