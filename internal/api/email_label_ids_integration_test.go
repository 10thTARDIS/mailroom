// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email.labelIds and the `labelIds/<id>` patch (#704, ADR-031): local-only
// labels — every area's own label — are applicable inside Mailroom, and a
// mirrored label applied by id writes back exactly as its keyword would.

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// createAreaAt creates one area at an explicit position and returns its id and
// its own (local-only) label id. One call per area so the positions are the
// test's, not a map's iteration order.
func createAreaAt(t *testing.T, h *apiHarness, userID ulid.ULID, name string, position int) (areaID, labelID string) {
	t.Helper()
	res := call(t, h.api.areaSet, userID, map[string]any{
		"create": map[string]any{"c": map[string]any{"name": name, "position": position}},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["c"].(map[string]any)
	if !ok {
		t.Fatalf("create area %q failed: %#v", name, res)
	}
	areaID, _ = obj["id"].(string)
	labelID, _ = obj["labelId"].(string)
	if areaID == "" || labelID == "" {
		t.Fatalf("create area %q: no id/labelId in %#v", name, obj)
	}
	return areaID, labelID
}

// countEmailChanges counts the Email change_log rows for one message, via the
// super pool (an independent read of what the store appended).
func (h *apiHarness) countEmailChanges(t *testing.T, userID, messageID ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Email' AND object_id=$2`,
		userID.String(), messageID.String()).Scan(&n); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	return n
}

// hasLabelRow reports whether message_labels holds (message, label), via super.
func (h *apiHarness) hasLabelRow(t *testing.T, userID, messageID ulid.ULID, labelID string) bool {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.message_labels WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
		userID.String(), messageID.String(), labelID).Scan(&n); err != nil {
		t.Fatalf("read message_labels: %v", err)
	}
	return n > 0
}

// getOne fetches one Email with the named properties.
func getOne(t *testing.T, h *apiHarness, userID ulid.ULID, id string, props ...string) map[string]any {
	t.Helper()
	res := call(t, h.api.emailGet, userID, map[string]any{
		"ids": []string{id}, "properties": append([]string{"id"}, props...),
	})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get %s: %d results, want 1 (%#v)", id, len(list), res)
	}
	obj, _ := list[0].(map[string]any)
	return obj
}

func idSetKeys(v any) []string {
	m, _ := v.(map[string]any)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEmailLabelIDsApplyLocalAreaLabelsWithoutWriteback(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	msg := a.MessageID

	// Promotions before Updates, as in the default set (positions 2 and 3).
	promoArea, promoLabel := createAreaAt(t, h, a.UserID, "Promo", 2)
	updArea, updLabel := createAreaAt(t, h, a.UserID, "Upd", 3)

	// The premise: an area's own label is LOCAL-ONLY — no atom.
	var atoms int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.label_keyword_map WHERE user_id=$1 AND label_id IN ($2,$3)`,
		a.UserID.String(), promoLabel, updLabel).Scan(&atoms); err != nil {
		t.Fatal(err)
	}
	if atoms != 0 {
		t.Fatalf("area labels carry %d keyword atoms; the test's premise is that they are local-only", atoms)
	}

	// The classifier filed it in Updates.
	mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
		VALUES ($1,$2,$3,'ml')`, a.UserID.String(), msg.String(), updLabel)

	// Email/get: labelIds carries BOTH the seed's mirrored label and the local
	// one; keywords carries only the mirrored atom (unchanged behaviour).
	obj := getOne(t, h, a.UserID, msg.String(), "labelIds", "keywords", "area", "labelProvenance")
	wantIDs := []string{a.LabelID.String(), updLabel}
	sort.Strings(wantIDs)
	if got := idSetKeys(obj["labelIds"]); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("labelIds = %v, want %v", got, wantIDs)
	}
	kws := idSetKeys(obj["keywords"])
	if strings.Join(kws, ",") != a.LabelAtom {
		t.Errorf("keywords = %v, want exactly [%s] — a local label must never surface as a keyword", kws, a.LabelAtom)
	}
	if prov, _ := obj["labelProvenance"].([]any); len(prov) != 1 {
		t.Errorf("labelProvenance = %v, want exactly the mirrored label's entry", obj["labelProvenance"])
	}
	if obj["area"] != updArea {
		t.Fatalf("area before = %v, want Upd %s", obj["area"], updArea)
	}

	changesBefore := h.countEmailChanges(t, a.UserID, msg)
	intentsBefore := h.countRows(t, "writeback_intents", a.UserID, msg)

	// Re-file Upd -> Promo by id.
	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{msg.String(): map[string]any{
			"labelIds/" + promoLabel: true,
			"labelIds/" + updLabel:   nil,
		}},
	})
	if _, ok := res["updated"].(map[string]any)[msg.String()]; !ok {
		t.Fatalf("not updated: %#v", res)
	}
	if !h.hasLabelRow(t, a.UserID, msg, promoLabel) || h.hasLabelRow(t, a.UserID, msg, updLabel) {
		t.Errorf("message_labels after re-file: promo=%v upd=%v, want true,false",
			h.hasLabelRow(t, a.UserID, msg, promoLabel), h.hasLabelRow(t, a.UserID, msg, updLabel))
	}
	if got := h.countEmailChanges(t, a.UserID, msg); got <= changesBefore {
		t.Errorf("Email change_log rows %d -> %d: a label change must append one (the invariant)", changesBefore, got)
	}
	if got := h.countRows(t, "writeback_intents", a.UserID, msg); got != intentsBefore {
		t.Errorf("writeback intents %d -> %d: a LOCAL label must write nothing to the mail server", intentsBefore, got)
	}
	obj = getOne(t, h, a.UserID, msg.String(), "labelIds", "area")
	if got := idSetKeys(obj["labelIds"]); !hasString(got, promoLabel) || hasString(got, updLabel) {
		t.Errorf("labelIds after re-file = %v, want Promo and not Upd", got)
	}
	if obj["area"] != promoArea {
		t.Errorf("area after re-file = %v, want Promo %s", obj["area"], promoArea)
	}

	// And back: the inverse patch restores Updates, still without writeback.
	changesBefore = h.countEmailChanges(t, a.UserID, msg)
	res = call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{msg.String(): map[string]any{
			"labelIds/" + promoLabel: false,
			"labelIds/" + updLabel:   true,
		}},
	})
	if _, ok := res["updated"].(map[string]any)[msg.String()]; !ok {
		t.Fatalf("reverse not updated: %#v", res)
	}
	if got := h.countEmailChanges(t, a.UserID, msg); got <= changesBefore {
		t.Errorf("reverse appended no Email change_log row")
	}
	if got := h.countRows(t, "writeback_intents", a.UserID, msg); got != intentsBefore {
		t.Errorf("writeback intents after reverse = %d, want %d", got, intentsBefore)
	}
	if obj = getOne(t, h, a.UserID, msg.String(), "area"); obj["area"] != updArea {
		t.Errorf("area after reverse = %v, want Upd %s", obj["area"], updArea)
	}

	// labelIds is OPT-IN: a client that did not name it does not receive it.
	if obj = getOne(t, h, a.UserID, msg.String(), "keywords"); obj["labelIds"] != nil {
		t.Errorf("labelIds returned without being requested: %v", obj["labelIds"])
	}
}

func hasString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestEmailLabelIDsRefusesUnknownForeignAndMalformedPatches(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)
	msg := a.MessageID.String()

	refused := func(name string, patch map[string]any, wantProp string) {
		t.Helper()
		before := h.countEmailChanges(t, a.UserID, a.MessageID)
		res := call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{msg: patch}})
		nu, _ := res["notUpdated"].(map[string]any)
		se, ok := nu[msg].(map[string]any)
		if !ok {
			t.Errorf("%s: not refused: %#v", name, res)
			return
		}
		if se["type"] != setInvalidPatch {
			t.Errorf("%s: type = %v, want invalidPatch", name, se["type"])
		}
		props, _ := se["properties"].([]any)
		if len(props) != 1 || props[0] != wantProp {
			t.Errorf("%s: properties = %v, want [%s]", name, props, wantProp)
		}
		if after := h.countEmailChanges(t, a.UserID, a.MessageID); after != before {
			t.Errorf("%s: a refused patch appended %d change rows", name, after-before)
		}
	}

	unknown := store.NewULID().String()
	refused("unknown label", map[string]any{"labelIds/" + unknown: true}, "labelIds/"+unknown)
	// B's label is invisible under A's RLS — refused exactly like a missing one.
	foreign := b.LabelID.String()
	refused("other user's label", map[string]any{"labelIds/" + foreign: true}, "labelIds/"+foreign)
	if h.hasLabelRow(t, a.UserID, a.MessageID, foreign) {
		t.Errorf("B's label landed on A's message")
	}
	refused("malformed id", map[string]any{"labelIds/not-an-id": true}, "labelIds/not-an-id")
	refused("whole-object replacement", map[string]any{"labelIds": map[string]any{}}, "labelIds")
	refused("add and remove the same label",
		map[string]any{"labelIds/" + a.LabelID.String(): true, "keywords/" + a.LabelAtom: nil},
		"labelIds/"+a.LabelID.String())

	// The seed's label is still there: nothing above removed it.
	if !h.hasLabelRow(t, a.UserID, a.MessageID, a.LabelID.String()) {
		t.Errorf("a refused patch removed the seeded label")
	}
}

var labelNameInPayload = regexp.MustCompile(`"label": "[^"]*"`)

// intentShape is a message's writeback intents as (op, payload) with the
// user's own atom replaced, so two users' intents can be compared.
func (h *apiHarness) intentShape(t *testing.T, userID, messageID ulid.ULID, atom string) []string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT op, payload::text FROM app.writeback_intents WHERE user_id=$1 AND message_id=$2 ORDER BY id`,
		userID.String(), messageID.String())
	if err != nil {
		t.Fatalf("read intents: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var op, payload string
		if err := rows.Scan(&op, &payload); err != nil {
			t.Fatal(err)
		}
		// The label's NAME rides along for Gmail writeback (#52); the two
		// users' labels are named differently, so it is normalised like the
		// atom — its presence still has to match.
		payload = labelNameInPayload.ReplaceAllString(payload, `"label": "NAME"`)
		out = append(out, op+" "+strings.ReplaceAll(payload, atom, "ATOM"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEmailLabelIDsMirroredLabelWritesBackLikeTheKeywordPath(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	// Same operation, two paths: A by id, B by keyword atom. Remove, then add.
	set := func(userID ulid.ULID, id string, patch map[string]any) {
		t.Helper()
		res := call(t, h.api.emailSet, userID, map[string]any{"update": map[string]any{id: patch}})
		if _, ok := res["updated"].(map[string]any)[id]; !ok {
			t.Fatalf("not updated: %#v", res)
		}
	}
	set(a.UserID, a.MessageID.String(), map[string]any{"labelIds/" + a.LabelID.String(): nil})
	set(b.UserID, b.MessageID.String(), map[string]any{"keywords/" + b.LabelAtom: nil})
	set(a.UserID, a.MessageID.String(), map[string]any{"labelIds/" + a.LabelID.String(): true})
	set(b.UserID, b.MessageID.String(), map[string]any{"keywords/" + b.LabelAtom: true})

	byID := h.intentShape(t, a.UserID, a.MessageID, a.LabelAtom)
	byKeyword := h.intentShape(t, b.UserID, b.MessageID, b.LabelAtom)
	if len(byKeyword) != 2 {
		t.Fatalf("keyword path enqueued %d intents, want 2 (the control: remove + add): %v", len(byKeyword), byKeyword)
	}
	if strings.Join(byID, "|") != strings.Join(byKeyword, "|") {
		t.Errorf("labelIds path intents = %v, keyword path = %v — a mirrored label must write back the same either way",
			byID, byKeyword)
	}
	for _, s := range byID {
		if !strings.Contains(s, "ATOM") {
			t.Errorf("intent %q does not name the label's atom", s)
		}
	}
}
