// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for AreaReportRowsAfter (#442 increment 0), the walk
// behind `mailroom-admin area-report`. It proves the two facts the report is
// built on: a message with nothing attached resolves to nothing — not the
// classifier's Primary residue, the same "invisible until something has
// looked at it" cost ADR-022 accepts for ResolveTabs (tab_resolve.go) — and a
// message that IS labelled with a mapped label surfaces that mapping and its
// live INBOX placement, the two facts internal/areas.Resolve needs before it
// ever touches the database-independent package at all.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

func TestAreaReportRowsAfter(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "areareport-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// A label mapped to a tab, so the mapped-tab half of the query has
	// something real to find.
	var labelID ulid.ULID
	labelName := "Promo-" + u.Tag
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		id, e := tx.CreateLabel(ctx, LabelRow{Name: labelName, MirrorToIMAP: false, Tab: tabs.Promotions})
		if e != nil {
			return e
		}
		labelID = id
		return nil
	}); err != nil {
		t.Fatalf("create label: %v", err)
	}

	seedMsg := func(tag byte) string {
		hash := sha256.Sum256([]byte(fmt.Sprintf("areareport-%s-%d", u.Tag, tag)))
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, u.ID, hash[:], NewULID().String())
		id := NewULID().String()
		mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,'m',now(),10)`, id, u.ID, hash[:])
		return id
	}

	// A message with no labels and no provenance at all: neither InInbox, nor
	// any label, nor any resolvable tab.
	bare := seedMsg(1)

	// A message with the mapped label attached, and a LIVE provenance row in
	// the seeded account's INBOX.
	labelled := seedMsg(2)
	mustExec(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
		u.ID, labelled, labelID.String())
	// provenance_live_uq is (mailbox_id, uidvalidity, uid) and the harness's
	// mailbox may already hold rows from other tests in the same run, so the
	// UID is never hard-coded: the first CI run of this test collided on
	// (mailbox, 1, 1) and failed in the seed. It comes from testUID(), not from
	// the clock this comment used to recommend — see #550.
	uid := testUID()
	mustExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		VALUES ($1,$2,$3,$4,$5,$6,1)`,
		NewULID().String(), u.ID, labelled, u.AccountID, u.MailboxID, uid)

	var rows []AreaReportRow
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		rows, e = tx.AreaReportRowsAfter(ctx, ulid.ULID{}, 0)
		return e
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	byID := map[string]AreaReportRow{}
	for _, r := range rows {
		byID[r.ID.String()] = r
	}

	b, ok := byID[bare]
	if !ok {
		t.Fatalf("bare message %s not returned among %d rows", bare, len(rows))
	}
	if b.InInbox {
		t.Error("bare message: InInbox = true, want false (no provenance at all)")
	}
	if len(b.Labels) != 0 {
		t.Errorf("bare message: Labels = %v, want none", b.Labels)
	}
	if b.MappedKnown || b.ClassifiedKnown {
		t.Errorf("bare message: resolved a tab (%+v) with nothing attached", b)
	}
	if _, ok := b.ResolvedTab(); ok {
		t.Error("bare message: ResolvedTab() ok = true, want false")
	}

	l, ok := byID[labelled]
	if !ok {
		t.Fatalf("labelled message %s not returned among %d rows", labelled, len(rows))
	}
	if !l.InInbox {
		t.Error("labelled message: InInbox = false, want true (live provenance in INBOX)")
	}
	if len(l.Labels) != 1 || l.Labels[0] != labelName {
		t.Errorf("labelled message: Labels = %v, want [%q]", l.Labels, labelName)
	}
	if !l.MappedKnown || l.MappedTab != tabs.Promotions {
		t.Errorf("labelled message: MappedTab = %q (known=%v), want %q known", l.MappedTab, l.MappedKnown, tabs.Promotions)
	}
	if tab, ok := l.ResolvedTab(); !ok || tab != tabs.Promotions {
		t.Errorf("labelled message: ResolvedTab() = (%q, %v), want (%q, true)", tab, ok, tabs.Promotions)
	}
}
