// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// ADR-049 area-index: report-only changes nothing; --apply closes every gap
// (including rows the triggers already filled), marks the user ready only at
// zero disagreements, and a second run is a no-op.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

func TestAreaIndexReportApplyAndIdempotence(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := "areaindex-" + store.NewULID().String()
	userID := seedUntangleUser(t, h.super, tag)

	var labelFed, tabFed ulid.ULID
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		l, e := tx.CreateLabel(ctx, store.LabelRow{Name: "VIP-" + tag})
		if e != nil {
			return e
		}
		if labelFed, e = tx.CreateArea(ctx, store.AreaRow{Name: "People-" + tag, Feeds: []store.AreaFeed{{LabelID: &l}}}); e != nil {
			return e
		}
		tabFed, e = tx.CreateArea(ctx, store.AreaRow{Name: "News-" + tag, Feeds: []store.AreaFeed{{Tab: tabs.Updates}}})
		return e
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	_ = labelFed
	_ = tabFed

	// Mail: some classified into the tab-fed area, some labelled into the label-fed
	// one, some neither. Inserting them goes through the triggers, so the column
	// starts CORRECT; the fixture then damages it the three ways a real column can
	// be wrong: NULL where it should be set, set where it should be NULL, and the
	// wrong area. The rest stay trigger-populated, which --apply must leave alone.
	var ids []ulid.ULID
	for i := 0; i < 30; i++ {
		id := seedUntangleMessage(t, h.super, userID, "m")
		ids = append(ids, id)
		if i%3 == 0 {
			if _, err := h.super.Exec(ctx, `UPDATE app.messages SET classified_tab='updates' WHERE id=$1`, id.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
	var lbl string
	if err := h.super.QueryRow(ctx, `SELECT label_id FROM app.area_feeds WHERE user_id=$1 AND label_id IS NOT NULL
		AND area_id=$2`, userID.String(), labelFed.String()).Scan(&lbl); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[1:6] {
		if _, err := h.super.Exec(ctx, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
			userID.String(), id.String(), lbl); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, q, a...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE app.messages SET area_id = NULL WHERE user_id=$1 AND id = ANY($2)`, userID.String(), idStrings(ids[0:8]))
	exec(`UPDATE app.messages SET area_id = $3 WHERE user_id=$1 AND id = ANY($2)`, userID.String(), idStrings(ids[20:24]), tabFed.String())
	exec(`UPDATE app.messages SET area_id = $3 WHERE user_id=$1 AND id = ANY($2)`, userID.String(), idStrings(ids[1:3]), tabFed.String())

	snap := func() string {
		var s string
		if err := h.super.QueryRow(ctx, `SELECT string_agg(id||':'||COALESCE(area_id,'')||':'||xmin::text, ',' ORDER BY id)
			FROM app.messages WHERE user_id=$1`, userID.String()).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ready := func() bool {
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.area_index_state WHERE user_id=$1 AND ready_at IS NOT NULL`,
			userID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	run := func(apply bool) string {
		var buf bytes.Buffer
		if err := areaIndex(ctx, &buf, h.store, []ulid.ULID{userID}, apply, 7); err != nil {
			t.Fatalf("area-index apply=%v: %v\n%s", apply, err, buf.String())
		}
		return buf.String()
	}

	before := snap()
	rep := run(false)
	if snap() != before {
		t.Fatal("the report-only run wrote to messages")
	}
	if ready() {
		t.Fatal("the report-only run marked the user ready")
	}
	if strings.Contains(rep, "rows --apply would write): 0") {
		t.Fatalf("the damaged fixture reported nothing to do (the report cannot see the damage):\n%s", rep)
	}
	t.Logf("report:\n%s", rep)

	out := run(true)
	if !strings.Contains(out, "marked ready: 0 disagreements") || !ready() {
		t.Fatalf("--apply did not reach zero disagreements and mark ready:\n%s", out)
	}
	if strings.Contains(out, "wrote 0\n") {
		t.Fatalf("--apply wrote nothing on a damaged column:\n%s", out)
	}
	t.Logf("apply:\n%s", out)

	afterApply := snap()
	var firstReady string
	if err := h.super.QueryRow(ctx, `SELECT ready_at::text FROM app.area_index_state WHERE user_id=$1`, userID.String()).Scan(&firstReady); err != nil {
		t.Fatal(err)
	}
	second := run(true)
	if !strings.Contains(second, "wrote 0\n") {
		t.Fatalf("a second --apply wrote rows:\n%s", second)
	}
	if snap() != afterApply {
		t.Fatal("a second --apply changed messages")
	}
	var secondReady string
	if err := h.super.QueryRow(ctx, `SELECT ready_at::text FROM app.area_index_state WHERE user_id=$1`, userID.String()).Scan(&secondReady); err != nil {
		t.Fatal(err)
	}
	if firstReady != secondReady {
		t.Fatalf("ready_at moved on a second run: %s -> %s", firstReady, secondReady)
	}
	// And the standing report on a ready user lists nothing.
	if rep := run(false); !strings.Contains(rep, "rows --apply would write): 0") {
		t.Fatalf("standing report on a backfilled user shows disagreements:\n%s", rep)
	}
}

func idStrings(ids []ulid.ULID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
