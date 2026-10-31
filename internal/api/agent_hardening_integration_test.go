// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Pre-tag hardening of API keys (#852, #850; ADR-042). Every request goes
// through the real bearer gate, as in agent_integration_test.go.

// countEvents counts a key's audit rows with this outcome[:anomaly] label
// (agentHarness.events' format).
func (h *agentHarness) countEvents(t *testing.T, keyID, label string) int {
	t.Helper()
	n := 0
	for _, e := range h.events(t, keyID) {
		if e == label {
			n++
		}
	}
	return n
}

// countReason counts a key's audit rows refused for reason.
func (h *agentHarness) countReason(t *testing.T, keyID, reason string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.api_key_events WHERE key_id = $1 AND reason = $2`, keyID, reason).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// countAlerts counts pushed alerts matching, after giving the asynchronous
// pushes a moment to land.
func (h *agentHarness) countAlerts(match func(fakeAlert) bool) int {
	time.Sleep(300 * time.Millisecond)
	h.alerts.mu.Lock()
	defer h.alerts.mu.Unlock()
	n := 0
	for _, al := range h.alerts.sent {
		if match(al) {
			n++
		}
	}
	return n
}

// #852 item 1: a key looping past the mass-send or mass-delete threshold is
// refused on EVERY call, but recorded and pushed (urgent, through quiet hours)
// once per key, rule and hour.
//
// RED PROBE: drop the APIKeyAnomalySince checks in admitKeyCall's mass-send
// and mass-delete branches and this records three anomalies of each and
// pushes three alerts of each.
func TestAgentKeyMassAnomaliesReportedOncePerCooldown(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "looper",
		"scopes": []string{"mail:read", "mail:send", "mail:delete"}, "confirmScopes": []string{},
		"acknowledgeSendAndDelete": true, "suspendOnAnomaly": false, "ratePerMinute": 600})
	sends := map[string]any{}
	for i := range MassSendThreshold + 1 {
		sends[fmt.Sprintf("s%d", i)] = map[string]any{"emailId": store.NewULID().String()}
	}
	dels := make([]string, MassDeleteThreshold+1)
	for i := range dels {
		dels[i] = store.NewULID().String()
	}
	const rounds = 3
	for i := range rounds {
		name, res := h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": sends})
		wantForbidden(t, fmt.Sprintf("mass send %d", i+1), name, res)
		name, res = h.keyCall(t, tok, "Email/set", map[string]any{"destroy": dels})
		wantForbidden(t, fmt.Sprintf("mass delete %d", i+1), name, res)
	}
	if got := h.countReason(t, keyID, store.RefusalMassSend); got != rounds {
		t.Errorf("%d mass-send refusals audited, want %d (every call refused and written)", got, rounds)
	}
	if got := h.countReason(t, keyID, store.RefusalMassDelete); got != rounds {
		t.Errorf("%d mass-delete refusals audited, want %d", got, rounds)
	}
	if got := h.countEvents(t, keyID, "anomaly:mass_send"); got != 1 {
		t.Errorf("%d mass_send anomalies for %d calls, want 1 per cooldown", got, rounds)
	}
	if got := h.countEvents(t, keyID, "anomaly:mass_delete"); got != 1 {
		t.Errorf("%d mass_delete anomalies for %d calls, want 1 per cooldown", got, rounds)
	}
	sendAlerts := h.countAlerts(func(al fakeAlert) bool { return al.urgent && strings.Contains(al.body, "send more than") })
	delAlerts := h.countAlerts(func(al fakeAlert) bool { return al.urgent && strings.Contains(al.body, "delete more than") })
	if sendAlerts != 1 || delAlerts != 1 {
		t.Errorf("pushed %d mass-send and %d mass-delete alerts for %d calls each, want 1 and 1", sendAlerts, delAlerts, rounds)
	}
	var suspended bool
	_ = h.super.QueryRow(context.Background(), `SELECT suspended_at IS NOT NULL FROM app.api_keys WHERE id=$1`, keyID).Scan(&suspended)
	if suspended {
		t.Error("a key with suspendOnAnomaly off was suspended")
	}
	// Past the cooldown the rule reports again: age the anomaly rows.
	if _, err := h.super.Exec(context.Background(), `UPDATE app.api_key_events SET at = at - interval '61 minutes'
		WHERE key_id = $1 AND outcome = 'anomaly'`, keyID); err != nil {
		t.Fatal(err)
	}
	name, res := h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": sends})
	wantForbidden(t, "mass send after the cooldown", name, res)
	if got := h.countEvents(t, keyID, "anomaly:mass_send"); got != 2 {
		t.Errorf("%d mass_send anomalies after the cooldown, want 2", got)
	}
}

// pendingCount counts the key's requests in state.
func (h *agentHarness) pendingCount(t *testing.T, keyID, state string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.api_key_pending WHERE key_id = $1 AND state = $2`, keyID, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// #852 item 4a: a request that cannot be held (nothing valid to delete) is
// audited as a refusal for its arguments — with a reason, at refusal cost,
// under the refusal caps — not as an unexplained denied row.
//
// RED PROBE: write the row with tx.InsertAPIKeyEvent as before (no reason) and
// the arguments count is 0.
func TestAgentKeyUnholdableRequestIsAnArgumentsRefusal(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "asks", "scopes": []string{"mail:read", "mail:delete"}})
	bogus := store.NewULID().String()
	name, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{bogus}})
	if name != "Email/set" || setError(t, res, "notDestroyed", bogus)["type"] != setNotFound {
		t.Fatalf("a held delete of a missing message: %s %v, want notFound in the answer", name, res)
	}
	if got := h.countReason(t, keyID, store.RefusalArguments); got != 1 {
		t.Errorf("%d arguments refusals audited, want 1: %v", got, h.events(t, keyID))
	}
	if got := h.pendingCount(t, keyID, store.PendingStatePending); got != 0 {
		t.Errorf("%d requests stored for nothing to delete, want 0", got)
	}
}

// #852 item 4b: an approved run is not held to the key's per-minute rate —
// the key's own refusals since it asked (which spend the rate, not the
// budget) cannot fail what a person approved — but the daily budget still
// applies.
//
// RED PROBE: drop `p.approval == nil &&` from the rate check in admitKeyCall
// and the first approval comes back failed (rateLimit).
func TestAgentKeyApprovalIsNotFailedByTheKeysRate(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	approve := func(pid string) map[string]any {
		res := call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{pid: map[string]any{"state": "approved"}}})
		return asObj(asObj(res["updated"])[pid])
	}
	holdAndFlood := func(tok string) (string, ulid.ULID) {
		t.Helper()
		victim := agentMessage(t, h, a)
		_, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{victim.String()}})
		pid := setError(t, res, "notDestroyed", victim.String())["pendingActionId"].(string)
		// Refusals cost 1 each against the rate: five overspend a rate of 3.
		for range 5 {
			name, res := h.keyCall(t, tok, "ApiKey/get", map[string]any{})
			wantForbidden(t, "a key reading keys", name, res)
		}
		return pid, victim
	}

	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "rate3", "scopes": []string{"mail:read", "mail:delete"},
		"ratePerMinute": 3})
	pid, victim := holdAndFlood(tok)
	if got := approve(pid); got["state"] != store.PendingStateApproved {
		t.Fatalf("approval after the key overspent its rate: %v, want approved", got)
	}
	if _, tr := h.trashed(t, victim); !tr {
		t.Fatal("the approved delete did not run")
	}

	// Positive control: the daily budget is still enforced on an approved run.
	// Only admitted calls count against it, so the person narrows it after
	// the request: two units spent (the hold and a read), a budget of one.
	tok2, id2 := h.newKey(t, a.UserID, map[string]any{"name": "budget", "scopes": []string{"mail:read", "mail:delete"},
		"ratePerMinute": 600, "dailyBudget": 10})
	victim2 := agentMessage(t, h, a)
	_, res := h.keyCall(t, tok2, "Email/set", map[string]any{"destroy": []string{victim2.String()}})
	pid2 := setError(t, res, "notDestroyed", victim2.String())["pendingActionId"].(string)
	if name, res := h.keyCall(t, tok2, "Mailbox/get", map[string]any{}); name != "Mailbox/get" {
		t.Fatalf("a read within budget: %s %v", name, res)
	}
	upd := call(t, h.api.apiKeySet, a.UserID, map[string]any{"update": map[string]any{id2: map[string]any{"dailyBudget": 1}}})
	if !mapHasKey(upd, "updated", id2) {
		t.Fatalf("narrowing the budget: %v", upd)
	}
	got := approve(pid2)
	if got["state"] != store.PendingStateFailed || asObj(got["error"])["type"] != errOverQuota {
		t.Fatalf("approval after the key's budget was narrowed below its spend: %v, want failed with overQuota", got)
	}
	if _, tr := h.trashed(t, victim2); tr {
		t.Fatal("an approval over the daily budget ran anyway")
	}
}

// #852 item 4c: at most MaxPendingPerKey requests wait for a person per key;
// the next is refused (a routine refusal, reason pending_cap) and nothing is
// held, and deciding one makes room.
//
// RED PROBE: remove the outstandingPending check in admitKeyCall and the
// eleventh request is held.
func TestAgentKeyOutstandingRequestsAreCapped(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "eager", "scopes": []string{"mail:read", "mail:delete"},
		"ratePerMinute": 600})
	ask := func() (string, map[string]any, ulid.ULID) {
		victim := agentMessage(t, h, a)
		name, res := h.keyCall(t, tok, "Email/set", map[string]any{"destroy": []string{victim.String()}})
		return name, res, victim
	}
	var first string
	for i := range MaxPendingPerKey {
		name, res, victim := ask()
		e := setError(t, res, "notDestroyed", victim.String())
		if name != "Email/set" || e["type"] != setPendingApproval {
			t.Fatalf("request %d: %s %v, want held", i+1, name, res)
		}
		if i == 0 {
			first = e["pendingActionId"].(string)
		}
	}
	name, res, victim := ask()
	if name != "error" || res["type"] != errOverQuota || !strings.Contains(fmt.Sprint(res["description"]), "waiting") {
		t.Fatalf("request %d: %s %v, want an overQuota refusal naming the waiting requests", MaxPendingPerKey+1, name, res)
	}
	if got := h.pendingCount(t, keyID, store.PendingStatePending); got != MaxPendingPerKey {
		t.Errorf("%d requests waiting, want %d", got, MaxPendingPerKey)
	}
	if got := h.countReason(t, keyID, store.RefusalPendingCap); got != 1 {
		t.Errorf("%d pending_cap refusals audited, want 1", got)
	}
	if _, tr := h.trashed(t, victim); tr {
		t.Fatal("the refused request deleted")
	}
	// Denying one makes room for one more.
	call(t, h.api.pendingSet, a.UserID, map[string]any{"update": map[string]any{first: map[string]any{"state": "denied"}}})
	name, res, victim = ask()
	if name != "Email/set" || setError(t, res, "notDestroyed", victim.String())["type"] != setPendingApproval {
		t.Fatalf("after a denial: %s %v, want held", name, res)
	}
}

// createLabels makes n labels as the user's session (a Label change row each)
// and returns their ids.
func createLabels(t *testing.T, h *agentHarness, f apiFixture, prefix string, n int) []string {
	t.Helper()
	create := map[string]any{}
	for i := range n {
		create[fmt.Sprintf("l%d", i)] = map[string]any{"name": fmt.Sprintf("%s-%03d", prefix, i), "mirrorToImap": false}
	}
	res := call(t, h.api.methods["Label/set"], f.UserID, map[string]any{"create": create})
	created := asObj(res["created"])
	if len(created) != n {
		t.Fatalf("created %d labels of %d: %v", len(created), n, res["notCreated"])
	}
	var ids []string
	for _, v := range created {
		ids = append(ids, asObj(v)["id"].(string))
	}
	return ids
}

// #852 item 2: a key's /changes is bounded — KeyQueryDefaultLimit when it
// sends no maxChanges, never more than KeyQueryMaxLimit — and charged for that
// bound up front; hasMoreChanges and the intermediate newState carry the rest
// (RFC 8620 §5.2), so paging from newState returns every change exactly once.
//
// RED PROBES: drop the key branch in handleChanges and the first page returns
// all 60 changes; drop readCost's /changes case and the maxChanges-100000 call
// on a budget of 5 is admitted (charged 1) instead of refused up front.
func TestAgentKeyChangesAreBounded(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	labelGet := func() string {
		return call(t, h.api.methods["Label/get"], a.UserID, map[string]any{"ids": []string{}})["state"].(string)
	}
	since := labelGet()
	const n = KeyQueryDefaultLimit + 10
	made := createLabels(t, h, a, "chg", n)
	final := labelGet()

	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "syncer", "scopes": []string{"labels:read"}, "ratePerMinute": 600})
	name, res := h.keyCall(t, tok, "Label/changes", map[string]any{"sinceState": since})
	if name != "Label/changes" || len(asArr(res["created"])) != KeyQueryDefaultLimit || res["hasMoreChanges"] != true ||
		res["newState"] == final {
		t.Fatalf("first page with no maxChanges: %s created=%d hasMore=%v newState=%v (final %s), want %d, true, intermediate",
			name, len(asArr(res["created"])), res["hasMoreChanges"], res["newState"], final, KeyQueryDefaultLimit)
	}
	seen := map[string]bool{}
	for _, id := range asArr(res["created"]) {
		seen[id.(string)] = true
	}
	_, res = h.keyCall(t, tok, "Label/changes", map[string]any{"sinceState": res["newState"]})
	if len(asArr(res["created"])) != n-KeyQueryDefaultLimit || res["hasMoreChanges"] != false || res["newState"] != final {
		t.Fatalf("second page: created=%d hasMore=%v newState=%v, want %d, false, %s",
			len(asArr(res["created"])), res["hasMoreChanges"], res["newState"], n-KeyQueryDefaultLimit, final)
	}
	for _, id := range asArr(res["created"]) {
		if seen[id.(string)] {
			t.Errorf("%v returned on both pages", id)
		}
		seen[id.(string)] = true
	}
	for _, id := range made {
		if !seen[id] {
			t.Errorf("label %s never returned by the two pages", id)
		}
	}
	// An explicit smaller maxChanges is honoured.
	_, res = h.keyCall(t, tok, "Label/changes", map[string]any{"sinceState": since, "maxChanges": 7})
	if len(asArr(res["created"])) != 7 || res["hasMoreChanges"] != true {
		t.Fatalf("maxChanges 7: created=%d hasMore=%v", len(asArr(res["created"])), res["hasMoreChanges"])
	}
	// More than the maximum is clamped and paid for before it runs: 10 units
	// on a budget of 5 is refused outright.
	poor, _ := h.newKey(t, a.UserID, map[string]any{"name": "poor", "scopes": []string{"labels:read"},
		"ratePerMinute": 600, "dailyBudget": 5})
	name, res = h.keyCall(t, poor, "Label/changes", map[string]any{"sinceState": since, "maxChanges": 100000})
	if name != "error" || res["type"] != errOverQuota {
		t.Fatalf("maxChanges 100000 on a budget of 5: %s %v, want overQuota up front", name, res)
	}
	// Positive control: the bound is a key's. A session still gets every change.
	all := call(t, h.api.methods["Label/changes"], a.UserID, map[string]any{"sinceState": since})
	if len(asArr(all["created"])) != n || all["hasMoreChanges"] != false {
		t.Fatalf("session Label/changes: created=%d hasMore=%v, want all %d", len(asArr(all["created"])), all["hasMoreChanges"], n)
	}
}

// #850: EVERY /query a key can reach (walked from keyPolicies, so a query
// method opened to keys later is covered without editing this test) runs
// with the key limit: the default when none is sent and the maximum when more
// is asked, each echoed as the server's limit (RFC 8620 §5.5); a limit the
// key may have is honoured and not echoed, and position pages through.
//
// RED PROBE: drop the principalFrom branch in mailboxQuery (or labelQuery,
// contactQuery, submissionQuery) and that method's subtest fails: no server
// limit, and every id on one page.
func TestEveryKeyQueryRunsWithTheKeyLimit(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	// At least two of each object a page can be seen on: mailboxes (Inbox and
	// Trash, from the fixture), emails, labels, contacts.
	agentMessage(t, h, a)
	createLabels(t, h, a, "q", 2)
	for i := range 2 {
		id := store.NewULID()
		if _, err := h.super.Exec(context.Background(), `INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1, $2, '')`,
			id.String(), a.UserID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(context.Background(), `INSERT INTO app.contact_addresses (id, user_id, contact_id, address)
			VALUES ($1, $2, $3, $4)`, store.NewULID().String(), a.UserID.String(), id.String(),
			fmt.Sprintf("q%d-%s@example.net", i, strings.ToLower(id.String()))); err != nil {
			t.Fatal(err)
		}
	}
	paged := map[string]bool{"Email/query": true, "Mailbox/query": true, "Label/query": true, "Contact/query": true}
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "reader", "ratePerMinute": 600, "dailyBudget": 20000,
		"scopes": []string{"mail:read", "labels:read", "contacts:read", "search:read"}})
	huge := 100000
	found := 0
	for name, pol := range keyPolicies {
		if !pol.keyReachable() || !strings.HasSuffix(name, "/query") {
			continue
		}
		found++
		t.Run(name, func(t *testing.T) {
			q := func(args map[string]any) map[string]any {
				t.Helper()
				args["calculateTotal"] = true
				got, res := h.keyCall(t, tok, name, args)
				if got != name {
					t.Fatalf("%s %v: %s %v", name, args, got, res)
				}
				return res
			}
			res := q(map[string]any{})
			if want := keyQueryLimitFor(name, nil); res["limit"] != float64(want) || len(asArr(res["ids"])) > want {
				t.Errorf("no limit sent: limit=%v ids=%d, want server limit %d", res["limit"], len(asArr(res["ids"])), want)
			}
			res = q(map[string]any{"limit": huge})
			if want := keyQueryLimitFor(name, &huge); res["limit"] != float64(want) || want > KeyQueryMaxLimit {
				t.Errorf("limit %d: server limit %v, want %d (at most %d)", huge, res["limit"], want, KeyQueryMaxLimit)
			}
			first := q(map[string]any{"limit": 1})
			if first["limit"] != nil || len(asArr(first["ids"])) > 1 {
				t.Errorf("limit 1: limit=%v ids=%v, want no server limit and at most 1 id", first["limit"], first["ids"])
			}
			total, _ := first["total"].(float64)
			if paged[name] && total < 2 {
				t.Fatalf("total %v: the fixture should give this query at least 2 objects to page", total)
			}
			if total >= 2 {
				if len(asArr(first["ids"])) != 1 {
					t.Errorf("limit 1 of %v: %d ids", total, len(asArr(first["ids"])))
				}
				second := q(map[string]any{"limit": 1, "position": 1})
				if second["position"] != float64(1) || len(asArr(second["ids"])) != 1 ||
					asArr(second["ids"])[0] == asArr(first["ids"])[0] {
					t.Errorf("position 1: position=%v ids=%v (first page %v), want the next id", second["position"], second["ids"], first["ids"])
				}
			}
		})
	}
	if found < 5 {
		t.Fatalf("only %d key-reachable /query methods found: the walk is not looking at keyPolicies", found)
	}
}

// #852 review F1, end to end: a free-text envelope recipient is refused for
// a session and for a key (before anything is held), and a plus-addressed
// one leads the held send's summary as written.
//
// RED PROBE: copy the envelope addresses unchecked in parseSubmitRequest and
// the session's spoofed send is created.
func TestEnvelopeRecipientsMustBeBareAddresses(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	addr := "env-" + a.UserID.String()[20:] + "@example.com"
	seedSendable(t, h.super, a, addr, "")
	spoof := "alice@example.com (+0 more) with 0 attachments: draft 'notes'"
	env := func(rcpt string) map[string]any {
		return map[string]any{"rcptTo": []map[string]any{{"email": rcpt}}}
	}

	// Session.
	sessDraft := createDraftAs(t, h, a, addr, "session send")
	res := call(t, h.api.submissionSet, a.UserID, map[string]any{"create": map[string]any{
		"s": map[string]any{"emailId": sessDraft.String(), "envelope": env(spoof)}}})
	if e := setError(t, res, "notCreated", "s"); e["type"] != setInvalidProps || fmt.Sprint(e["properties"]) != "[envelope/rcptTo/0/email]" {
		t.Fatalf("session send with a free-text recipient: %v, want invalidProperties on envelope/rcptTo/0/email", e)
	}

	// Key in confirm mode: refused before anything is held.
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "envelope", "scopes": []string{"mail:read", "mail:draft", "mail:send"}})
	_, res = h.keyCall(t, tok, "Email/set", draftCreateArgs(addr, "notes"))
	draft := asObj(asObj(res["created"])["d"])["id"].(string)
	_, res = h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": map[string]any{
		"s": map[string]any{"emailId": draft, "envelope": env(spoof)}}})
	if e := setError(t, res, "notCreated", "s"); e["type"] != setInvalidProps {
		t.Fatalf("key send with a free-text recipient: %v, want invalidProperties", e)
	}
	if got := h.pendingCount(t, keyID, store.PendingStatePending); got != 0 {
		t.Fatalf("%d requests held for a free-text recipient, want 0", got)
	}

	// Positive control: a plus-addressed recipient is held and leads the line.
	_, res = h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": map[string]any{
		"s": map[string]any{"emailId": draft, "envelope": env("bob+tag@example.net")}}})
	pid, _ := setError(t, res, "notCreated", "s")["pendingActionId"].(string)
	list := asArr(call(t, h.api.pendingGet, a.UserID, map[string]any{"ids": []string{pid}})["list"])
	if len(list) != 1 || !strings.HasPrefix(asObj(list[0])["summary"].(string), "Send to bob+tag@example.net: draft") {
		t.Fatalf("plus-addressed recipient: %v, want held with a summary that leads with it", list)
	}
}
