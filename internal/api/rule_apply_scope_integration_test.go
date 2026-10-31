// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// #720: Rule/apply and Rule/preview scanned only the newest 500 messages, so on
// a real mailbox a saved rule never reached its older matches. A rule answerable
// from row facts now scans the whole previewable mailbox; one that must open
// message bytes keeps the (much smaller) blob bound and says so.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedFiller adds n unremarkable previewable messages, all NEWER than anything
// seedMessage places a day ago, with one statement for blobs and one for rows.
// They are from a sender no test rule matches.
func (h *apiHarness) seedFiller(t *testing.T, f apiFixture, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		SELECT $1, sha256(convert_to('filler-' || g::text || $4::text, 'UTF8')), 100, 132, $2
		  FROM generate_series(1, $3) g`,
		f.UserID.String(), ulid.Make().String(), n, f.UserID.String()); err != nil {
		t.Fatalf("seed filler blobs: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, snippet,
		    from_name, from_address, received_at, size_bytes, message_id_hint)
		SELECT $5::text || upper(lpad(to_hex(g), 14, '0')), $1,
		       sha256(convert_to('filler-' || g::text || $4::text, 'UTF8')), $2,
		       'filler ' || g::text, '', ' ', 'noise@filler.example',
		       now() - make_interval(secs => g), 100, '<filler-' || g::text || '@example.com>'
		  FROM generate_series(1, $3) g`,
		f.UserID.String(), f.ThreadID.String(), n, f.UserID.String(), ulid.Make().String()[:12]); err != nil {
		t.Fatalf("seed filler messages: %v", err)
	}
}

const scopeFiller = 650 // comfortably more than the old 500 default

// THE DEFECT: the one matching message is older than the newest 500.
func TestRuleApplyReachesMailOlderThanTheNewest500(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	const sender = "old-news@shop.example"
	label := h.label(t, a, "Old news")
	conds := map[string]any{"field": "from", "op": "equals", "value": sender}
	actions := []any{map[string]any{"type": "addLabel", "label": label.String()}}
	ruleID := h.createRule(t, a, "old news", conds, actions)

	old := h.seedMessage(t, a, "years ago", sender, 24*time.Hour)
	h.seedFiller(t, a, scopeFiller)

	pre := call(t, h.api.rulePreview, a.UserID, map[string]any{"conditions": conds})
	if num(pre, "matched") != 1 {
		t.Fatalf("preview matched = %v, want 1 (scanned %v of %v)", pre["matched"], pre["scanned"], pre["total"])
	}
	if c, _ := pre["complete"].(bool); !c {
		t.Errorf("preview complete = false (scanned %v of %v), want true", pre["scanned"], pre["total"])
	}

	res := h.applyRule(t, a, ruleID)
	if num(res, "changed") != 1 || num(res, "matched") != 1 {
		t.Fatalf("apply changed=%v matched=%v scanned=%v total=%v, want 1/1",
			res["changed"], res["matched"], res["scanned"], res["total"])
	}
	if c, _ := res["complete"].(bool); !c {
		t.Errorf("apply complete = false (scanned %v of %v), want true", res["scanned"], res["total"])
	}
	if num(res, "scanned") != num(res, "total") || num(res, "total") < scopeFiller {
		t.Errorf("scanned=%v total=%v, want the whole mailbox", res["scanned"], res["total"])
	}
	if by := h.labelledBy(t, old, label); by == "" {
		t.Error("the old message was not labelled")
	}

	// A caller-supplied limit still lowers the scan, and then reports itself bounded.
	lim := call(t, h.api.rulePreview, a.UserID, map[string]any{"conditions": conds, "limit": 100})
	if num(lim, "scanned") != 100 || num(lim, "matched") != 0 {
		t.Errorf("limit 100: scanned=%v matched=%v, want 100/0", lim["scanned"], lim["matched"])
	}
	if c, _ := lim["complete"].(bool); c {
		t.Error("limit 100: complete = true on a bounded scan")
	}
}

// The same defect through an area-filing rule: the old message is on the desk
// in Primary, so the filing is a swap, not an add.
func TestRuleApplyAreaFilingReachesMailOlderThanTheNewest500(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	areas := h.defaultAreas(t, a)
	primary, updates := areas["Primary"], areas["Updates"]

	const sender = "old-news@shop.example"
	conds := map[string]any{"field": "from", "op": "equals", "value": sender}
	actions := []any{map[string]any{"type": "addLabel", "label": updates.LabelID.String()}}
	ruleID := h.createRule(t, a, "old news is Updates", conds, actions)

	old := h.seedMessage(t, a, "years ago", sender, 24*time.Hour)
	h.live(t, a, old)
	h.applyLabel(t, a, old, primary.LabelID, "desk")
	h.applyLabel(t, a, old, updates.LabelID, "user")
	h.seedFiller(t, a, scopeFiller)
	if got := h.areaOf(t, a, old); got != primary.ID.String() {
		t.Fatalf("premise: old message resolves to %q, want Primary", got)
	}

	pre := call(t, h.api.rulePreview, a.UserID, map[string]any{
		"conditions": conds, "targetLabelId": updates.LabelID.String(),
	})
	if num(pre, "matched") != 1 || areaFilingCounts(t, pre) != "1/0/0/0/0" {
		t.Fatalf("preview matched=%v areaFiling=%v, want 1 and a single move", pre["matched"], pre["areaFiling"])
	}

	res := h.applyRule(t, a, ruleID)
	if num(res, "changed") != 1 {
		t.Fatalf("apply changed = %v, want 1", res["changed"])
	}
	if c, _ := res["complete"].(bool); !c {
		t.Errorf("apply complete = false (scanned %v of %v)", res["scanned"], res["total"])
	}
	if h.hasLabelRow(t, a.UserID, old, primary.LabelID.String()) {
		t.Error("the desk's Primary label is still on the old message")
	}
	if got := h.areaOf(t, a, old); got != updates.ID.String() {
		t.Errorf("old message resolves to %q, want Updates %s", got, updates.ID)
	}
}

// unreadableRaw lets a body-reading rule run without any message bytes: every
// message is evaluated on the facts the row has, the documented best-effort
// path. The test is about the SCAN BOUND, not the headers.
type unreadableRaw struct{}

func (unreadableRaw) ReadRaw(context.Context, ulid.ULID, []byte) ([]byte, error) {
	return nil, errors.New("no bytes in this test")
}

// A rule that must open message bytes keeps the blob bound, in preview AND in
// apply (apply used to take the row bound for these too).
func TestBodyRuleKeepsTheBlobScanBound(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	h.api.raw = unreadableRaw{}

	conds := map[string]any{"field": "header", "header": "X-Test", "op": "exists"}
	label := h.label(t, a, "Headered")
	ruleID := h.createRule(t, a, "headered", conds,
		[]any{map[string]any{"type": "addLabel", "label": label.String()}})
	h.seedFiller(t, a, previewMaxScanBlob+250)

	pre := call(t, h.api.rulePreview, a.UserID, map[string]any{"conditions": conds})
	if num(pre, "scanned") != previewDefaultScanBlob {
		t.Errorf("preview scanned = %v, want %d", pre["scanned"], previewDefaultScanBlob)
	}
	if c, _ := pre["complete"].(bool); c {
		t.Error("preview complete = true on a mailbox larger than the blob bound")
	}
	// A larger requested limit is clamped to the blob maximum, not the row one.
	big := call(t, h.api.rulePreview, a.UserID, map[string]any{"conditions": conds, "limit": 50000})
	if num(big, "scanned") != previewMaxScanBlob {
		t.Errorf("preview limit 50000 scanned = %v, want %d", big["scanned"], previewMaxScanBlob)
	}

	res := h.applyRule(t, a, ruleID)
	if num(res, "scanned") != previewDefaultScanBlob {
		t.Errorf("apply scanned = %v, want %d", res["scanned"], previewDefaultScanBlob)
	}
	if c, _ := res["complete"].(bool); c {
		t.Error("apply complete = true on a mailbox larger than the blob bound")
	}
}
