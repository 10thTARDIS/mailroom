// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE DIFFERENTIAL TEST #442 RESTS ON, the twin of tab_resolve_integration_test
// .go's TestTabResolverAgreesWithTheQueryPredicate: internal/areas.Resolve (Go)
// and areaExprSQL/ResolveAreas (SQL) cannot share code across that boundary, so
// the remedy is a red build on disagreement, not shared code. Also covers
// AreaSummary's live-desk predicate (INBOX or feeds_desk, account-scoped).

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/tabs"
)

func TestAreaResolverAgreesWithTheQueryExpression(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "arr-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// A plan built via CreateArea/UpdateArea, covering every shape #442
	// distinguishes: Alpha is fed by its own label alone (earliest position,
	// so it wins any tie); Beta is ALSO fed by a tab (added via UpdateArea,
	// after creation); Gamma is fed by BOTH an extra label and a tab.
	var alphaID, betaID, gammaID, vipLabelID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		alphaID, e = tx.CreateArea(ctx, AreaRow{Name: "Alpha", Position: 0})
		if e != nil {
			return e
		}
		betaID, e = tx.CreateArea(ctx, AreaRow{Name: "Beta", Position: 1})
		if e != nil {
			return e
		}
		if _, e = tx.UpdateArea(ctx, betaID, AreaPatch{SetFeeds: true, Feeds: []AreaFeed{{Tab: tabs.Updates}}}); e != nil {
			return e
		}
		vipLabelID, e = tx.CreateLabel(ctx, LabelRow{Name: "VIP-" + u.Tag, MirrorToIMAP: false})
		if e != nil {
			return e
		}
		gammaID, e = tx.CreateArea(ctx, AreaRow{Name: "Gamma", Position: 2,
			Feeds: []AreaFeed{{Tab: tabs.Forums}, {LabelID: &vipLabelID}}})
		return e
	}); err != nil {
		t.Fatalf("build plan: %v", err)
	}

	var alphaLabel, betaLabel ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetAreas(ctx, []ulid.ULID{alphaID, betaID})
		if e != nil {
			return e
		}
		for _, v := range views {
			switch v.ID {
			case alphaID:
				alphaLabel = v.LabelID
			case betaID:
				betaLabel = v.LabelID
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read own labels: %v", err)
	}

	var mappedLabelID, deadEndLabelID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		mappedLabelID, e = tx.CreateLabel(ctx, LabelRow{Name: "Newsletter-" + u.Tag, MirrorToIMAP: false, Tab: tabs.Updates})
		if e != nil {
			return e
		}
		deadEndLabelID, e = tx.CreateLabel(ctx, LabelRow{Name: "DeadEnd-" + u.Tag, MirrorToIMAP: false})
		return e
	}); err != nil {
		t.Fatalf("seed extra labels: %v", err)
	}

	seedMsg := func(tag string, classified tabs.Tab) ulid.ULID {
		hash := sha256.Sum256([]byte("arearesolve-" + u.Tag + "-" + tag))
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, u.ID, hash[:], NewULID().String())
		id := NewULID()
		var classifiedArg any
		if classified != "" {
			classifiedArg = string(classified)
		}
		mustExec(t, h, `
			INSERT INTO app.messages (id, user_id, content_hash, subject, received_at,
			    size_bytes, classified_tab, classifier_version)
			VALUES ($1,$2,$3,$4,now(),10,$5::app.category_tab,$6)`,
			id.String(), u.ID, hash[:], tag, classifiedArg, tabs.ClassifierVersion)
		return id
	}
	attachLabel := func(msg, label ulid.ULID) {
		mustExec(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
			u.ID, msg.String(), label.String())
	}

	// 1. Label-fed win.
	labelWin := seedMsg("label-win", "")
	attachLabel(labelWin, alphaLabel)

	// 2. Two labels feeding two areas: earlier position (Alpha) wins the tie.
	tie := seedMsg("tie", "")
	attachLabel(tie, alphaLabel)
	attachLabel(tie, betaLabel)

	// 3. Tab fallback via a MAPPED label — the label itself feeds no area.
	tabMapping := seedMsg("tab-mapping", "")
	attachLabel(tabMapping, mappedLabelID)

	// 4. Tab fallback via the stored classifier verdict — no labels at all.
	tabVerdict := seedMsg("tab-verdict", tabs.Forums)

	// 5. Unsorted: no labels, no verdict.
	unsorted := seedMsg("unsorted", "")

	// 6. Labelled, but the label feeds nothing and there is no verdict either.
	deadEnd := seedMsg("dead-end", "")
	attachLabel(deadEnd, deadEndLabelID)

	ids := []ulid.ULID{labelWin, tie, tabMapping, tabVerdict, unsorted, deadEnd}

	var (
		cfg    areas.Config
		byName map[string]ulid.ULID
		rows   []AreaReportRow
		sqlRes map[ulid.ULID]AreaResolution
	)
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		cfg, byName, e = tx.AreaConfig(ctx)
		if e != nil {
			return e
		}
		rows, e = tx.AreaReportRowsAfter(ctx, ulid.ULID{}, 0)
		if e != nil {
			return e
		}
		sqlRes, e = tx.ResolveAreas(ctx, ids)
		return e
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	byRowID := map[ulid.ULID]AreaReportRow{}
	for _, r := range rows {
		byRowID[r.ID] = r
	}

	for _, id := range ids {
		row, ok := byRowID[id]
		if !ok {
			t.Errorf("message %s missing from AreaReportRowsAfter", id)
			continue
		}
		tab, tabKnown := row.ResolvedTab()
		goRes := cfg.Resolve(areas.Facts{Labels: row.Labels, Tab: tab, TabKnown: tabKnown})
		sql, ok := sqlRes[id]
		if !ok {
			t.Errorf("message %s missing from ResolveAreas", id)
			continue
		}

		if goRes.Reason.Unsorted() {
			if sql.AreaID != nil {
				t.Errorf("message %s: Go says Needs sorting (%+v), SQL says area %s", id, row, *sql.AreaID)
			}
			continue
		}
		wantAreaID, ok := byName[goRes.Area]
		if !ok {
			t.Fatalf("message %s: Go resolved area %q, which AreaConfig does not know", id, goRes.Area)
		}
		if sql.AreaID == nil {
			t.Errorf("message %s: Go says area %q, SQL says Needs sorting", id, goRes.Area)
			continue
		}
		if *sql.AreaID != wantAreaID {
			t.Errorf("message %s: Go resolved area %q (%s), SQL resolved %s", id, goRes.Area, wantAreaID, *sql.AreaID)
		}
		wantByLabel := goRes.Reason == areas.ByLabel
		if sql.ByLabel != wantByLabel {
			t.Errorf("message %s: Go reason = %v (ByLabel=%v), SQL ByLabel = %v", id, goRes.Reason, wantByLabel, sql.ByLabel)
		}
	}

	// AreaPredicate is the form Email/query will use; it must select exactly
	// the messages ResolveAreas places in each area, and exactly the unsorted
	// ones for nil. Otherwise a count and the list under it can disagree.
	for _, areaID := range []*ulid.ULID{&alphaID, &betaID, &gammaID, nil} {
		var want []ulid.ULID
		for _, id := range ids {
			res := sqlRes[id]
			if (areaID == nil && res.AreaID == nil) || (areaID != nil && res.AreaID != nil && *res.AreaID == *areaID) {
				want = append(want, id)
			}
		}
		var got []ulid.ULID
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			args := []any{userID.String(), ulidStrings(ids)}
			bind := func(v any) string {
				args = append(args, v)
				return "$" + strconv.Itoa(len(args))
			}
			pred := AreaPredicate(areaID, bind)
			rows, e := tx.tx.Query(ctx, `SELECT m.id FROM app.messages m WHERE m.user_id = $1 AND m.id = ANY($2) AND `+pred, args...)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if e := rows.Scan(&id); e != nil {
					return e
				}
				got = append(got, mustULID(t, id))
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("AreaPredicate(%v): %v", areaID, err)
		}
		if len(got) != len(want) {
			t.Errorf("AreaPredicate(%v) selected %d messages, ResolveAreas placed %d", areaID, len(got), len(want))
			continue
		}
		wantSet := map[ulid.ULID]bool{}
		for _, id := range want {
			wantSet[id] = true
		}
		for _, id := range got {
			if !wantSet[id] {
				t.Errorf("AreaPredicate(%v) selected %s, which ResolveAreas placed elsewhere", areaID, id)
			}
		}
	}

	// Pin the specific outcomes by name too, so a passing agreement check
	// cannot hide both sides being wrong in the SAME way.
	want := map[ulid.ULID]struct {
		area    ulid.ULID
		unsort  bool
		byLabel bool
	}{
		labelWin:   {area: alphaID, byLabel: true},
		tie:        {area: alphaID, byLabel: true},
		tabMapping: {area: betaID, byLabel: false},
		tabVerdict: {area: gammaID, byLabel: false},
		unsorted:   {unsort: true},
		deadEnd:    {unsort: true},
	}
	for id, w := range want {
		got := sqlRes[id]
		if w.unsort {
			if got.AreaID != nil {
				t.Errorf("message %s: want Needs sorting, got area %s", id, *got.AreaID)
			}
			continue
		}
		if got.AreaID == nil || *got.AreaID != w.area {
			t.Errorf("message %s: area = %v, want %s", id, got.AreaID, w.area)
		}
		if got.ByLabel != w.byLabel {
			t.Errorf("message %s: ByLabel = %v, want %v", id, got.ByLabel, w.byLabel)
		}
	}
}

func TestAreaSummary(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "asum-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var workID, emptyID, ownLabel ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		workID, e = tx.CreateArea(ctx, AreaRow{Name: "Work"})
		if e != nil {
			return e
		}
		emptyID, e = tx.CreateArea(ctx, AreaRow{Name: "Empty"})
		if e != nil {
			return e
		}
		views, e := tx.GetAreas(ctx, []ulid.ULID{workID})
		if e != nil {
			return e
		}
		ownLabel = views[0].LabelID
		return nil
	}); err != nil {
		t.Fatalf("create areas: %v", err)
	}

	flaggedMailbox := NewULID().String()
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, feeds_desk)
		VALUES ($1,$2,$3,'Flagged','Flagged',true)`, flaggedMailbox, u.ID, u.AccountID)
	unflaggedMailbox := NewULID().String()
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, feeds_desk)
		VALUES ($1,$2,$3,'Unflagged','Unflagged',false)`, unflaggedMailbox, u.ID, u.AccountID)

	otherAccount := NewULID().String()
	mustExec(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'other','mail.example.com','smtp.example.com',$3,$4)`,
		otherAccount, u.ID, []byte{0x01}, NewULID().String())
	otherMailbox := NewULID().String()
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		VALUES ($1,$2,$3,'INBOX','Inbox')`, otherMailbox, u.ID, otherAccount)

	seedMsg := func(tag string, score *float32) ulid.ULID {
		hash := sha256.Sum256([]byte("areasummary-" + u.Tag + "-" + tag))
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, u.ID, hash[:], NewULID().String())
		id := NewULID()
		mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,now(),10)`, id.String(), u.ID, hash[:], tag)
		if score != nil {
			mustExec(t, h, `INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
				VALUES ($1,$2,$3,'v0')`, u.ID, id.String(), *score)
		}
		mustExec(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
			u.ID, id.String(), ownLabel.String())
		return id
	}
	provenance := func(msg ulid.ULID, account, mailbox string) {
		uid := testUID()
		mustExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`, NewULID().String(), u.ID, msg.String(), account, mailbox, uid)
	}

	surfaceScore := float32(0.9)
	drawerScore := float32(0.1)

	inInbox := seedMsg("in-inbox", &surfaceScore)
	provenance(inInbox, u.AccountID, u.MailboxID)

	inFlagged := seedMsg("in-flagged", &drawerScore)
	provenance(inFlagged, u.AccountID, flaggedMailbox)

	inUnflagged := seedMsg("in-unflagged", &surfaceScore)
	provenance(inUnflagged, u.AccountID, unflaggedMailbox)

	unscored := seedMsg("unscored", nil)
	provenance(unscored, u.AccountID, u.MailboxID)

	inOtherAccount := seedMsg("other-account", &surfaceScore)
	provenance(inOtherAccount, otherAccount, otherMailbox)

	var summary []AreaCounts
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		summary, e = tx.AreaSummary(ctx, DeskScope{Threshold: 0.5})
		return e
	}); err != nil {
		t.Fatalf("summary: %v", err)
	}

	find := func(id *ulid.ULID) *AreaCounts {
		for i := range summary {
			if id == nil && summary[i].AreaID == nil {
				return &summary[i]
			}
			if id != nil && summary[i].AreaID != nil && *summary[i].AreaID == *id {
				return &summary[i]
			}
		}
		return nil
	}

	work := find(&workID)
	if work == nil {
		t.Fatal("area Work missing from the summary")
	}
	// in-inbox (surface) + other-account (surface, unscoped call sees every
	// account); in-unflagged must be EXCLUDED (its mailbox neither is INBOX nor
	// has feeds_desk).
	if work.Surface != 2 {
		t.Errorf("Work.Surface = %d, want 2 (in-inbox and the other account's message; "+
			"the unflagged-mailbox message must be excluded)", work.Surface)
	}
	if work.Drawer != 1 {
		t.Errorf("Work.Drawer = %d, want 1 (in-flagged, via feeds_desk)", work.Drawer)
	}
	if work.Unscored != 1 {
		t.Errorf("Work.Unscored = %d, want 1 (unscored)", work.Unscored)
	}

	empty := find(&emptyID)
	if empty == nil {
		t.Fatal("area Empty (zero messages) missing from the summary — a bounded/empty result must still report its bound")
	}
	if empty.Surface != 0 || empty.Drawer != 0 || empty.Unscored != 0 {
		t.Errorf("Empty = %+v, want all zero", empty)
	}

	if find(nil) == nil {
		t.Error("the Needs-sorting row is missing from the summary")
	}

	// Account-scoped: the other account's message must drop out.
	acctID := mustULID(t, u.AccountID)
	var scoped []AreaCounts
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		scoped, e = tx.AreaSummary(ctx, DeskScope{AccountID: &acctID, Threshold: 0.5})
		return e
	}); err != nil {
		t.Fatalf("scoped summary: %v", err)
	}
	findIn := func(cs []AreaCounts, id ulid.ULID) *AreaCounts {
		for i := range cs {
			if cs[i].AreaID != nil && *cs[i].AreaID == id {
				return &cs[i]
			}
		}
		return nil
	}
	scopedWork := findIn(scoped, workID)
	if scopedWork == nil {
		t.Fatal("area Work missing from the account-scoped summary")
	}
	if scopedWork.Surface != 1 {
		t.Errorf("account-scoped Work.Surface = %d, want 1 (in-inbox only; "+
			"the other account's message must be excluded)", scopedWork.Surface)
	}
	if fmt.Sprint(scopedWork.Drawer) != fmt.Sprint(work.Drawer) {
		t.Errorf("account scoping changed Drawer (%d -> %d) though both messages are in the seeded account",
			work.Drawer, scopedWork.Drawer)
	}
}
