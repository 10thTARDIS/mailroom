// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Activity/list (ADR-053, #45) against a real Postgres: the merged list is the
// caller's alone for EVERY source, bounded, never carries content, and is refused
// to API keys (with a session as the positive control).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mailroom/internal/store"
)

// activityIDs are the distinctive values seedActivityRows writes for one user.
type activityIDs struct {
	keyID, keyName, purged, batch, rule, ua string
}

// activityKindsAll is every kind the list can return.
var activityKindsAll = []string{"ai_tool", "ai_key", "decryption", "purge", "sign_in", "sign_out", "placement", "rule_apply"}

// seedActivityRows writes one record for every Activity source, each carrying a
// value no other user's rows share, so a leak is visible in the JSON by name.
func seedActivityRows(t *testing.T, super *pgxpool.Pool, u apiFixture, tag string) activityIDs {
	t.Helper()
	ctx := context.Background()
	ids := activityIDs{keyID: store.NewULID().String(), keyName: "tool-" + tag, purged: store.NewULID().String(),
		batch: store.NewULID().String(), rule: store.NewULID().String(), ua: "Mozilla/5.0 Firefox/130.0 secret-" + tag}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed activity: %v\n%s", err, sql)
		}
	}
	th := sha256.Sum256([]byte("act-" + ids.keyID))
	exec(`INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes) VALUES ($1,$2,$3,$4,ARRAY['mail:read'])`,
		ids.keyID, u.UserID.String(), ids.keyName, th[:])
	exec(`INSERT INTO app.api_key_events (id, user_id, key_id, method, outcome, reason) VALUES ($1,$2,$3,'Email/set','denied','scope')`,
		store.NewULID().String(), u.UserID.String(), ids.keyID)
	for _, a := range []struct{ action, typ, obj string }{
		{"api_key.create", "ApiKey", ids.keyID},
		{store.PurgeAction, "Email", ids.purged},
		// The placement entry reads the batch's SUMMARY row (ADR-053 amendment).
		{"placement.backfill.move", "PlacementBackfillSummary", ids.batch},
		{store.RuleApplyAction, store.RuleApplyObjectType, ids.rule},
	} {
		exec(`INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail) VALUES ($1,'user',$2,$3,$4,'{"changed":4}')`,
			u.UserID.String(), a.action, a.typ, a.obj)
	}
	exec(`INSERT INTO app.rules (id, user_id, name, conditions, actions) VALUES ($1,$2,$3,'{}','[]')`,
		ids.rule, u.UserID.String(), "rule-"+tag)
	exec(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint) VALUES ($1,$2,$3,'body','decrypted','AABBCCDD11')`,
		store.NewULID().String(), u.UserID.String(), u.MessageID.String())
	sid := store.NewULID().String()
	sh := sha256.Sum256([]byte("act-sess-" + sid))
	exec(`INSERT INTO app.sessions (id, user_id, token_hash, user_agent, revoked_at, revoked_reason) VALUES ($1,$2,$3,$4,now(),'signed_out')`,
		sid, u.UserID.String(), sh[:], ids.ua)
	return ids
}

func activityKinds(res map[string]any) map[string]int {
	m := map[string]int{}
	for _, e := range asArr(res["list"]) {
		if em, ok := e.(map[string]any); ok {
			k, _ := em["kind"].(string)
			m[k]++
		}
	}
	return m
}

// The cross-user probe (data-model s5 test 4): A reads Activity/list and sees
// nothing of B's, for each of the sources, with A's own as the positive control.
func probeActivityList(t *testing.T, h *rlsHarness, a, b rlsUser) {
	ai := seedActivityRows(t, h.super, a.apiFixture, "a"+store.NewULID().String())
	bi := seedActivityRows(t, h.super, b.apiFixture, "b"+store.NewULID().String())
	res, mErr := invokeAs(t, h, "Activity/list", a.UserID, map[string]any{"limit": 100})
	if mErr != nil {
		t.Fatalf("Activity/list: %+v", mErr)
	}
	blob, _ := json.Marshal(res)
	for what, leak := range map[string]string{
		"key id": bi.keyID, "key name": bi.keyName, "purged message": bi.purged, "placement batch": bi.batch,
		"rule": bi.rule, "session user agent": "secret-" + strings.TrimPrefix(bi.keyName, "tool-"),
		"B's message id": b.MessageID.String(),
	} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("A's activity carries B's %s (%q): %s", what, leak, blob)
		}
	}
	got := activityKinds(res)
	for _, k := range activityKindsAll {
		if got[k] == 0 {
			t.Errorf("A's own %s entry is missing: the probe proves nothing without it (%v)", k, got)
		}
	}
	if !strings.Contains(string(blob), ai.keyName) {
		t.Errorf("A's own key name is missing from A's list: %s", blob)
	}
	// Each source asked for alone, too: a branch that lost its predicate is named.
	for _, src := range []string{"ai_tool", "decryption", "purge", "session", "bulk"} {
		r, mErr := invokeAs(t, h, "Activity/list", a.UserID, map[string]any{"sources": []string{src}})
		if mErr != nil {
			t.Fatalf("Activity/list %s: %+v", src, mErr)
		}
		blob, _ := json.Marshal(r)
		for _, leak := range []string{bi.keyID, bi.keyName, bi.purged, bi.batch, bi.rule, b.MessageID.String()} {
			if strings.Contains(string(blob), leak) {
				t.Errorf("source %s: A's list carries B's %q", src, leak)
			}
		}
	}
	// Naming B's JMAP account is refused before any row is read.
	if _, mErr := h.api.activityList(context.Background(), a.UserID,
		mustJSON(t, map[string]any{"accountId": b.UserID.String()})); mErr == nil || mErr.Type != errAccountNotFound {
		t.Errorf("list naming B's account: got %+v, want accountNotFound", mErr)
	}
}

func TestActivityListShapePrivacyAndBounds(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ids := seedActivityRows(t, h.super, a, "shape")
	res := call(t, h.api.activityList, a.UserID, map[string]any{})
	blob, _ := json.Marshal(res)

	if res["limit"].(float64) != 50 || res["next"] != nil {
		t.Errorf("limit/next = %v/%v, want 50/null for a short list", res["limit"], res["next"])
	}
	// Privacy: the raw user agent never leaves, only the description; the decrypt
	// entry names the message and its key but carries no subject.
	if strings.Contains(string(blob), "secret-shape") || strings.Contains(string(blob), "Mozilla") {
		t.Errorf("the raw user agent is in the list: %s", blob)
	}
	if !strings.Contains(string(blob), "Firefox") {
		t.Errorf("the described device is missing: %s", blob)
	}
	if strings.Contains(string(blob), "seed message") || strings.Contains(string(blob), "subject") {
		t.Errorf("a message subject is in the list: %s", blob)
	}
	// covers names the oldest record per selected source.
	covers, _ := res["covers"].(map[string]any)
	for _, src := range []string{"ai_tool", "decryption", "purge", "session", "bulk"} {
		if covers[src] == nil {
			t.Errorf("covers[%s] is null though the source holds records: %v", src, covers)
		}
	}
	// Links: a live message, key and rule link; the purged message does not.
	links := map[string]bool{}
	for _, e := range asArr(res["list"]) {
		em := e.(map[string]any)
		if o, ok := em["object"].(map[string]any); ok {
			links[em["kind"].(string)+":"+o["id"].(string)] = o["exists"].(bool)
		}
	}
	if !links["decryption:"+a.MessageID.String()] || !links["ai_tool:"+ids.keyID] || !links["rule_apply:"+ids.rule] {
		t.Errorf("a live object has no link: %v", links)
	}
	if exists, ok := links["purge:"+ids.purged]; !ok || exists {
		t.Errorf("a purged message links or its entry is missing: %v", links)
	}

	// Bounds and arguments.
	for name, args := range map[string]map[string]any{
		"limit 0": {"limit": 0}, "limit 101": {"limit": 101}, "unknown source": {"sources": []string{"x"}},
		"bad since": {"since": "yesterday"}, "bad cursor": {"before": "nope"},
		"since after until": {"since": "2030-01-02T00:00:00Z", "until": "2030-01-01T00:00:00Z"},
	} {
		if _, mErr := h.api.activityList(context.Background(), a.UserID, mustJSON(t, args)); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("%s: got %+v, want invalidArguments", name, mErr)
		}
	}
	// Paging through the API: limit 3 hands back a cursor that continues.
	p1 := call(t, h.api.activityList, a.UserID, map[string]any{"limit": 3})
	next, _ := p1["next"].(string)
	if len(asArr(p1["list"])) != 3 || next == "" {
		t.Fatalf("page 1 = %d entries, next %q", len(asArr(p1["list"])), next)
	}
	p2 := call(t, h.api.activityList, a.UserID, map[string]any{"limit": 3, "before": next})
	first := asArr(p1["list"])[0].(map[string]any)["id"]
	for _, e := range asArr(p2["list"]) {
		if e.(map[string]any)["id"] == first {
			t.Errorf("page 2 repeats page 1's %v", first)
		}
	}
	// A date filter narrows: nothing is older than the far past.
	old := call(t, h.api.activityList, a.UserID, map[string]any{"until": "2001-01-01T00:00:00Z"})
	if len(asArr(old["list"])) != 0 {
		t.Errorf("until 2001 lists %d entries", len(asArr(old["list"])))
	}
}

// API keys cannot read the list; a session can.
func TestActivityListRefusedToKeys(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	seedActivityRows(t, h.super, a, "keys")
	token, _ := h.newKey(t, a.UserID, map[string]any{"name": "reader", "scopes": []string{"mail:read"}})

	name, res := h.keyCall(t, token, "Activity/list", map[string]any{})
	wantForbidden(t, "Activity/list for a key", name, res)
	if !strings.Contains(res["description"].(string), "not available to API keys") {
		t.Errorf("the refusal does not say why: %v", res)
	}
	// Positive control: the same call as a session succeeds and lists entries.
	got := call(t, h.api.activityList, a.UserID, map[string]any{})
	if len(asArr(got["list"])) == 0 {
		t.Error("the session's own list is empty: the refusal above proves nothing without it")
	}
	// The refusal is audited as a session_only refusal.
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.api_key_events WHERE user_id = $1 AND method = 'Activity/list' AND outcome = 'denied' AND reason = 'session_only'`,
		a.UserID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("session_only refusals recorded = %d (%v), want 1", n, err)
	}
}

// A rule bulk apply, and its undo, leave one entry each, in the same transaction.
func TestRuleApplyAppearsInActivity(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	promotions := h.label(t, a, "Promotions")
	conds, actions := domainRule(promotions)
	ruleID := h.createRule(t, a, "activity-promos", conds, actions)
	m1 := h.seedBulk(t, a, "40% off", "deals@wayfair.example", "wayfair.example", time.Minute)
	h.seedBulk(t, a, "Flash sale", "deals@wayfair.example", "wayfair.example", 2*time.Minute)

	if res := h.applyRule(t, a, ruleID); res["changed"].(float64) != 2 {
		t.Fatalf("changed = %v", res["changed"])
	}
	// A second run changes nothing and records nothing.
	h.applyRule(t, a, ruleID)
	list := call(t, h.api.activityList, a.UserID, map[string]any{"sources": []string{"bulk"}})
	es := asArr(list["list"])
	if len(es) != 1 {
		t.Fatalf("after an apply and a no-op re-run: %d bulk entries, want 1", len(es))
	}
	e := es[0].(map[string]any)
	if e["outcome"] != store.RuleApplyAction || e["count"].(float64) != 2 || e["keyName"] != "activity-promos" {
		t.Errorf("apply entry = %v", e)
	}
	call(t, h.api.ruleApply, a.UserID, map[string]any{"ruleId": ruleID.String(), "undo": true, "messageIds": []string{m1.String()}})
	list = call(t, h.api.activityList, a.UserID, map[string]any{"sources": []string{"bulk"}})
	if es := asArr(list["list"]); len(es) != 2 || es[0].(map[string]any)["outcome"] != store.RuleApplyUndoAction {
		t.Errorf("after the undo: %v", es)
	}
}
