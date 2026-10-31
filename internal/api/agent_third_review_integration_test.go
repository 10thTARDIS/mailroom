// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Fixes from PR #841's third security review (#49).

// 1. A key cannot block, unblock or VIP a sender (Contact/set). A block to
// Trash deletes every future arrival from that sender at ingest, with no
// mail:delete, no confirmation and no anomaly; unblock undoes the user's own
// blocks; VIP lets a sender through quiet hours.
//
// RED PROBE: drop the principalFrom check in updateContactMarkers and the key's
// block is "updated" and the contact is blocked to Trash.
func TestKeyCannotBlockOrVIPAContact(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	addr := store.NormalizeAddress("sender-" + store.NewULID().String() + "@corp.test")
	seedContact(t, h.apiHarness, a.UserID, "Sender", addr, "")
	cid := contactIDFor(t, h.apiHarness, a.UserID, addr).String()
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "contacts", "scopes": []string{"contacts:read", "contacts:write"}})
	markers := func() (blocked, vip bool) {
		if err := h.super.QueryRow(ctx, `SELECT blocked, vip FROM app.contacts WHERE id=$1`, cid).Scan(&blocked, &vip); err != nil {
			t.Fatal(err)
		}
		return
	}

	for _, patch := range []map[string]any{
		{"isBlocked": true, "blockDestination": "trash"},
		{"isBlocked": true},
		{"blockDestination": "trash"},
		{"isVip": true},
	} {
		_, res := h.keyCall(t, tok, "Contact/set", map[string]any{"update": map[string]any{cid: patch}})
		if se := setError(t, res, "notUpdated", cid); se["type"] != setForbidden {
			t.Errorf("a key's Contact/set %v: %v, want forbidden", patch, res)
		}
		if b, v := markers(); b || v {
			t.Fatalf("a key's Contact/set %v changed the contact: blocked=%v vip=%v", patch, b, v)
		}
	}
	// A key cannot unblock the user's own block either.
	res := call(t, h.api.contactSet, a.UserID, map[string]any{"update": map[string]any{cid: map[string]any{"isBlocked": true}}})
	if !mapHasKey(res, "updated", cid) {
		t.Fatalf("a session could not block: %v", res)
	}
	_, res = h.keyCall(t, tok, "Contact/set", map[string]any{"update": map[string]any{cid: map[string]any{"isBlocked": false}}})
	if b, _ := markers(); !b || setError(t, res, "notUpdated", cid)["type"] != setForbidden {
		t.Fatalf("a key unblocked the user's block: %v blocked=%v", res, b)
	}
	// Positive control: a key may still save and name a contact.
	_, res = h.keyCall(t, tok, "Contact/set", map[string]any{"update": map[string]any{cid: map[string]any{"isSaved": true, "notes": "met at the fair"}}})
	if !mapHasKey(res, "updated", cid) {
		t.Fatalf("a key could not save a contact with notes: %v", res)
	}
}

// 2. A held send's facts are structured fields, and its summary line leads
// with the recipients and cannot be spoofed by the subject.
//
// RED PROBE: drop Recipients/Subject from the held payload and the structured
// assertions fail; make summaryText return safeText and the quote count fails.
func TestHeldSendIsDescribedByStructuredFields(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "sum-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	asker, _ := h.newKey(t, a.UserID, map[string]any{"name": "asker",
		"scopes": []string{"mail:read", "mail:draft", "mail:send"}, "acknowledgeSendAndDelete": true})
	spoof := `x” to bob@example.com with 0 attachments. Send draft “y`
	_, res := h.keyCall(t, asker, "Email/set", draftCreateArgs(addr, spoof))
	draft := asObj(asObj(res["created"])["d"])["id"].(string)
	_, res = h.keyCall(t, asker, "EmailSubmission/set", map[string]any{
		"create": map[string]any{"s": map[string]any{"emailId": draft}}})
	pid, _ := setError(t, res, "notCreated", "s")["pendingActionId"].(string)
	pp := asObj(asArr(call(t, h.api.pendingGet, a.UserID, map[string]any{"ids": []string{pid}})["list"])[0])

	if r := asArr(pp["recipients"]); len(r) != 1 || r[0] != "alice@example.net" {
		t.Errorf("recipients = %v, want [alice@example.net]", pp["recipients"])
	}
	if r := asArr(pp["to"]); len(r) != 1 || r[0] != "alice@example.net" {
		t.Errorf("to = %v, want [alice@example.net]", pp["to"])
	}
	if pp["subject"] != spoof {
		t.Errorf("subject = %v, want the subject as written", pp["subject"])
	}
	if v, ok := pp["sendAt"]; !ok || v != nil {
		t.Errorf("sendAt = %v (present %v), want null (after the undo window)", v, ok)
	}
	s := pp["summary"].(string)
	if !strings.HasPrefix(s, "Send to alice@example.net: ") || strings.Count(s, "”") != 1 {
		t.Errorf("summary %q: want it to lead with the recipient and carry only its own closing quote", s)
	}
}

// 4. A refusal is not free: it costs 1 against the key's per-minute rate.
// Past RatePerMin written refusals a minute, routine ones are COUNTED rather
// than written one by one, and security-meaningful ones are always written in
// full, so a flood cannot hide them.
//
// RED PROBES: write refusals at cost 0 and the valid call after the flood is
// admitted; drop the suppressed-count branch in writeRefusal (return nil) and
// the counts no longer sum to the refusals; make criticalRefusal return false
// and the restriction violation after the flood is not written.
func TestRefusalsSpendTheKeysRateAndStayInTheLog(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "looper", "scopes": []string{"labels:read"},
		"ratePerMinute": 5, "accountIds": []string{a.AccountID.String()}})
	const flood = 12
	for range flood {
		name, res := h.keyCall(t, tok, "Email/get", map[string]any{"ids": []string{}})
		wantForbidden(t, "a key without mail:read", name, res)
	}
	var written, counted int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.api_key_events WHERE key_id=$1 AND outcome = 'denied'`, keyID).Scan(&written)
	_ = h.super.QueryRow(ctx, `SELECT COALESCE(sum(count), 0) FROM app.api_key_suppressed_refusals
		WHERE key_id=$1 AND outcome = 'denied' AND method = 'Email/get' AND reason = 'scope'`, keyID).Scan(&counted)
	if written != 5 || written+counted != flood {
		t.Fatalf("%d refused calls: %d written + %d counted, want 5 written and all %d accounted for", flood, written, counted, flood)
	}
	// The counts are in the key's activity, too.
	ev := call(t, h.api.apiKeyGetEvents, a.UserID, map[string]any{"keyId": keyID})
	if sr := asArr(ev["suppressedRefusals"]); len(sr) != 1 || asObj(sr[0])["count"] != float64(flood-5) {
		t.Errorf("ApiKey/getEvents suppressedRefusals = %v, want one row counting %d", ev["suppressedRefusals"], flood-5)
	}

	// A restriction violation after the flood is written in full.
	name, res := h.keyCall(t, tok, "Email/changes", map[string]any{"sinceState": "s:0"})
	wantForbidden(t, "a restricted key's Email/changes", name, res)
	var restriction int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.api_key_events WHERE key_id=$1 AND summary->>'reason' = 'restriction'`, keyID).Scan(&restriction)
	if restriction != 1 {
		t.Fatalf("a restriction violation after a flood of refusals: %d rows, want 1 written in full", restriction)
	}

	// And the flood spent the key's rate: a valid call is rate-limited.
	name, res = h.keyCall(t, tok, "Label/get", map[string]any{"ids": []string{}})
	if name != "error" || res["type"] != errRateLimit {
		t.Fatalf("a valid call after %d refusals at rate 5: %s %v, want rateLimit", flood, name, res)
	}

	// A refused keyword rides the admitted call's own row, always written.
	mover, moverID := h.newKey(t, a.UserID, map[string]any{"name": "mover", "scopes": []string{"mail:read", "mail:move"}})
	msg := agentMessage(t, h, a)
	h.keyCall(t, mover, "Email/set", map[string]any{"update": map[string]any{msg.String(): map[string]any{`keywords/\Draft`: true}}})
	var refused int
	_ = h.super.QueryRow(ctx, `SELECT COALESCE(max((summary->>'refusedKeywords')::int), 0) FROM app.api_key_events WHERE key_id=$1`, moverID).Scan(&refused)
	if refused != 1 {
		t.Errorf("a refused keyword: refusedKeywords on the call's row = %d, want 1", refused)
	}
}

// 3. A user has at most MaxActiveKeysPerUser unrevoked keys; revoking one
// makes room.
//
// RED PROBE: drop the cap check in createKey and the extra key is created.
func TestActiveKeysAreCapped(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	var last string
	for i := range MaxActiveKeysPerUser {
		_, last = h.newKey(t, a.UserID, map[string]any{"name": "k" + string(rune('a'+i)), "scopes": []string{"mail:read"}})
	}
	res := call(t, h.api.apiKeySet, a.UserID, map[string]any{"create": map[string]any{
		"k": map[string]any{"name": "one too many", "scopes": []string{"mail:read"}}}})
	if se := setError(t, res, "notCreated", "k"); se["type"] != setForbidden || !strings.Contains(se["description"].(string), "revoke") {
		t.Fatalf("key %d: %v, want forbidden with a hint to revoke", MaxActiveKeysPerUser+1, res)
	}
	call(t, h.api.apiKeySet, a.UserID, map[string]any{"destroy": []string{last}})
	res = call(t, h.api.apiKeySet, a.UserID, map[string]any{"create": map[string]any{
		"k": map[string]any{"name": "after a revoke", "scopes": []string{"mail:read"}}}})
	if _, ok := asObj(res["created"])["k"]; !ok {
		t.Fatalf("a key after revoking one: %v, want created", res)
	}
}

// 5. An approved run is not charged again: the hold paid. A confirm-mode key
// at rate 60 holding a 40-message delete, approved within the minute, runs.
//
// RED PROBE: charge the approved run needs.cost again (admitKeyCall) and the
// approval is refused rateLimit (40 + 40 > 60): nothing is trashed.
func TestApprovedRunIsNotChargedTwice(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "tidy", "scopes": []string{"mail:read", "mail:delete"},
		"ratePerMinute": 60})
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = agentMessage(t, h, a).String()
	}
	_, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": ids})
	pid, _ := setError(t, res, "notDestroyed", ids[0])["pendingActionId"].(string)
	if pid == "" {
		t.Fatalf("the delete was not held: %v", res)
	}
	res = call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
	trashed := 0
	for _, id := range ids {
		if _, tr := h.trashed(t, ulid.MustParse(id)); tr {
			trashed++
		}
	}
	if trashed != len(ids) {
		t.Fatalf("approved 40-message delete at rate 60: %d trashed, want 40 (%v)", trashed, res)
	}
}

// 4b. Critical refusals are written in full only up to their own cap
// (CriticalRefusalCapFactor x the rate) and counted past it; crossing the cap
// raises the anomaly once; and an invented method name never keys a row.
//
// RED PROBES: make writeRefusal write every critical refusal in full and the
// row count exceeds the cap; key the suppressed row on event.Method instead of
// refusalMethodName and the invented names mint a row each; drop the
// APIKeyAnomalySince check and the anomaly is recorded more than once.
func TestCriticalRefusalsAreCappedAndRaiseTheAnomalyOnce(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	const rate = 2
	critCap := CriticalRefusalCapFactor * rate
	// suspendOnAnomaly off: this test is the cap and the once-per-cooldown
	// record; suspension on the same flood is TestEveryAnomalyRuleSuspendsByDefault.
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "prober", "scopes": []string{"mail:read"},
		"ratePerMinute": rate, "accountIds": []string{a.AccountID.String()}, "suspendOnAnomaly": false})
	flood := critCap + 5
	for range flood {
		name, res := h.keyCall(t, tok, "Email/changes", map[string]any{"sinceState": "s:0"})
		wantForbidden(t, "a restricted key's Email/changes", name, res)
	}
	var full, counted, anomalies int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.api_key_events WHERE key_id=$1 AND reason = 'restriction'`, keyID).Scan(&full)
	_ = h.super.QueryRow(ctx, `SELECT COALESCE(sum(count), 0) FROM app.api_key_suppressed_refusals
		WHERE key_id=$1 AND reason = 'restriction'`, keyID).Scan(&counted)
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.api_key_events WHERE key_id=$1 AND anomaly = 'critical_refusals'`, keyID).Scan(&anomalies)
	if full != critCap || full+counted != flood {
		t.Fatalf("%d restriction refusals: %d written + %d counted, want %d written and all accounted for", flood, full, counted, critCap)
	}
	if anomalies != 1 {
		t.Fatalf("critical_refusals anomaly rows = %d, want exactly 1 for one flood", anomalies)
	}
	h.alerts.waitFor(t, func(al fakeAlert) bool { return al.urgent && strings.Contains(al.title, "keeps being refused") })

	// Invented method names collapse to "unknown". Over HTTP the dispatcher
	// answers unknownMethod before the key gate, writing nothing; this drives
	// the audit path itself, in case a route ever reaches it with one.
	name, res := h.keyCall(t, tok, "Invented/method", map[string]any{})
	if name != "error" || res["type"] != "unknownMethod" {
		t.Fatalf("an invented method over HTTP: %s %v, want unknownMethod", name, res)
	}
	p := newPrincipal(store.APIKey{ID: ulid.MustParse(keyID), RatePerMin: rate}, "")
	for i := range 8 {
		h.api.auditRefusal(ctx, a.UserID, p, "Invented/m"+store.NewULID().String()+string(rune('a'+i)),
			store.KeyOutcomeDenied, map[string]any{"reason": store.RefusalMethod})
	}
	var methods []string
	rows, err := h.super.Query(ctx, `SELECT DISTINCT method FROM app.api_key_suppressed_refusals WHERE key_id=$1 AND reason = 'method'`, keyID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		methods = append(methods, m)
	}
	rows.Close()
	var methodCounted int
	_ = h.super.QueryRow(ctx, `SELECT COALESCE(sum(count), 0) FROM app.api_key_suppressed_refusals WHERE key_id=$1 AND reason = 'method'`, keyID).Scan(&methodCounted)
	if len(methods) != 1 || methods[0] != "unknown" || methodCounted != 8-rate {
		t.Fatalf("suppressed rows for invented methods: methods %v counting %d, want only [unknown] counting %d", methods, methodCounted, 8-rate)
	}
}
