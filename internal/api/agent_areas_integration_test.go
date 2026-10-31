// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// AREAS FOR API KEYS, AND KEY MOVES THAT DO NOT TRAIN PLACEMENT (#49), and
// SUSPENSION ON EVERY ANOMALY RULE (#49). Every request goes through the
// real bearer gate, as in agent_integration_test.go. The store half of the
// evidence guarantee (each learning path, with the user as the positive
// control) is internal/ingest/learned_placement_keys_integration_test.go;
// this file proves the API half: that a key's call IS attributed to the key.

// areasOf materialises the user's default areas and returns name -> area.
func (h *agentHarness) areasOf(t *testing.T, user ulid.ULID) map[string]store.AreaView {
	t.Helper()
	out := map[string]store.AreaView{}
	if err := h.store.UserTx(context.Background(), user, func(tx *store.UserTx) error {
		views, err := tx.ListAreas(context.Background())
		for _, v := range views {
			out[v.Name] = v
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Primary", "Updates", "Promotions"} {
		if _, ok := out[n]; !ok {
			t.Fatalf("no %s area in the default plan: %v", n, out)
		}
	}
	return out
}

// filingEvidence is what the learners read as the user's decisions: learned
// placement's filings (the placement report) and the label model's
// hand-applied labels.
func (h *agentHarness) filingEvidence(t *testing.T, user ulid.ULID) (filings, handApplied int) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.UserTx(ctx, user, func(tx *store.UserTx) error {
		rep, err := tx.PlacementReport(ctx)
		if err != nil {
			return err
		}
		hand, err := tx.HandAppliedLabels(ctx)
		if err != nil {
			return err
		}
		filings = rep.Filings
		for _, ls := range hand {
			handApplied += len(ls)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return filings, handApplied
}

func (h *agentHarness) labelBy(t *testing.T, msg, label ulid.ULID) string {
	t.Helper()
	var by string
	_ = h.super.QueryRow(context.Background(),
		`SELECT applied_by FROM app.message_labels WHERE message_id=$1 AND label_id=$2`, msg.String(), label.String()).Scan(&by)
	return by
}

// refilePatch is the client's swap into target (web/src/core/refile.ts), the
// patch move_to_area sends.
func refilePatch(areas map[string]store.AreaView, target string) map[string]any {
	p := map[string]any{"labelIds/" + areas[target].LabelID.String(): true}
	for name, a := range areas {
		if name != target {
			p["labelIds/"+a.LabelID.String()] = nil
		}
	}
	return p
}

// RED PROBE: remove `up.LabelSource = store.AgentLabelSource()` from emailSet's
// update loop and the key's filing is applied_by 'user' and moves both counts.
func TestKeyAreaMoveIsAppliedAuditedAndNotEvidence(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	areas := h.areasOf(t, a.UserID)
	tok, keyID := h.newKey(t, a.UserID, map[string]any{"name": "filer", "scopes": []string{"mail:read", "mail:move"}})

	name, res := h.keyCall(t, tok, "Area/get", map[string]any{"ids": nil})
	if name != "Area/get" || len(asArr(res["list"])) != len(areas) {
		t.Fatalf("a key's Area/get: %s %v", name, res)
	}
	if obj := asObj(asArr(res["list"])[0]); obj["labelId"] == nil || obj["name"] == nil {
		t.Fatalf("Area/get for a key carries no labelId/name: %v", obj)
	}
	name, res = h.keyCall(t, tok, "Area/set", map[string]any{"create": map[string]any{"x": map[string]any{"name": "Agent's"}}})
	wantForbidden(t, "a key's Area/set", name, res)

	filings, hand := h.filingEvidence(t, a.UserID)
	m := agentMessage(t, h, a)
	name, res = h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{m.String(): refilePatch(areas, "Updates")}})
	if name != "Email/set" || !hasKey(asObj(res["updated"]), m.String()) {
		t.Fatalf("a key's area move: %s %v", name, res)
	}
	if by := h.labelBy(t, m, areas["Updates"].LabelID); by != store.LabelByAgent {
		t.Fatalf("the key's filing is applied_by %q, want %q", by, store.LabelByAgent)
	}
	name, res = h.keyCall(t, tok, "Email/get", map[string]any{"ids": []string{m.String()}, "properties": []string{"id", "area"}})
	if l := asArr(res["list"]); name != "Email/get" || len(l) != 1 || asObj(l[0])["area"] != areas["Updates"].ID.String() {
		t.Fatalf("after the key's move the message is not in Updates: %s %v", name, res)
	}
	if n := h.countEvents(t, keyID, store.KeyOutcomeOK); n < 3 {
		t.Errorf("the key's calls were not audited (%d ok rows)", n)
	}
	if f2, h2 := h.filingEvidence(t, a.UserID); f2 != filings || h2 != hand {
		t.Errorf("a key's area move changed the evidence: filings %d -> %d, hand-applied %d -> %d", filings, f2, hand, h2)
	}

	// Positive control: the same move by the user's session is evidence.
	m2 := agentMessage(t, h, a)
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{m2.String(): refilePatch(areas, "Updates")}})
	if by := h.labelBy(t, m2, areas["Updates"].LabelID); by != store.LabelByUser {
		t.Fatalf("positive control: the session's filing is applied_by %q", by)
	}
	if f2, h2 := h.filingEvidence(t, a.UserID); f2 != filings+1 || h2 != hand+1 {
		t.Errorf("positive control: filings %d -> %d, hand-applied %d -> %d, want +1 each", filings, f2, hand, h2)
	}
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

// A key's removal of a learned label through Email/set withdraws the
// placement; the session's is the correction. RED PROBE: as above (the
// key's update attributed to the user makes it a correction).
func TestKeyEmailSetWithdrawsALearnedPlacement(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	areas := h.areasOf(t, a.UserID)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "filer", "scopes": []string{"mail:read", "mail:move"}})
	learned := func() ulid.ULID {
		m := agentMessage(t, h, a)
		for _, q := range []string{
			`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1, $2, $3, 'learned')`,
			`INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
			 VALUES ($1, $2, 'address', 'x@example.com', (SELECT id FROM app.areas WHERE label_id = $3))`,
		} {
			if _, err := h.super.Exec(ctx, q, a.UserID.String(), m.String(), areas["Updates"].LabelID.String()); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}
	outcome := func(m ulid.ULID) (o string) {
		_ = h.super.QueryRow(ctx, `SELECT outcome FROM app.learned_placements WHERE message_id=$1`, m.String()).Scan(&o)
		return o
	}
	byKey, bySession := learned(), learned()
	if name, res := h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{byKey.String(): refilePatch(areas, "Promotions")}}); name != "Email/set" || !hasKey(asObj(res["updated"]), byKey.String()) {
		t.Fatalf("a key's re-file of a learned placement: %s %v", name, res)
	}
	if got := outcome(byKey); got != store.PlacementWithdrawn {
		t.Errorf("after a key's re-file the placement is %q, want %q", got, store.PlacementWithdrawn)
	}
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{bySession.String(): refilePatch(areas, "Promotions")}})
	if got := outcome(bySession); got != store.PlacementCorrected {
		t.Errorf("positive control: after the session's re-file the placement is %q, want %q", got, store.PlacementCorrected)
	}
}

// RED PROBE: drop the UserFilingProtectedError mapping and the call fails as
// a serverFail; drop the store check and the filing is deleted.
func TestKeyCannotUndoTheUsersOwnFiling(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	areas := h.areasOf(t, a.UserID)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "filer", "scopes": []string{"mail:read", "mail:move"}})
	m, other := agentMessage(t, h, a), agentMessage(t, h, a)
	call(t, h.api.emailSet, a.UserID, map[string]any{"update": map[string]any{m.String(): refilePatch(areas, "Promotions")}})
	filings, _ := h.filingEvidence(t, a.UserID)

	name, res := h.keyCall(t, tok, "Email/set", map[string]any{"update": map[string]any{
		m.String(): refilePatch(areas, "Updates"), other.String(): refilePatch(areas, "Updates")}})
	if name != "Email/set" {
		t.Fatalf("Email/set: %s %v", name, res)
	}
	se := asObj(asObj(res["notUpdated"])[m.String()])
	if se["type"] != setForbidden || !slices.Contains(toStrings(se["properties"]), "labelIds/"+areas["Promotions"].LabelID.String()) {
		t.Fatalf("a key re-filing the user's own filing: %v, want forbidden naming labelIds/<Promotions>", res)
	}
	if !hasKey(asObj(res["updated"]), other.String()) {
		t.Errorf("the other message in the same call was not filed: %v", res)
	}
	if by := h.labelBy(t, m, areas["Promotions"].LabelID); by != store.LabelByUser {
		t.Errorf("the user's filing is now %q", by)
	}
	if by := h.labelBy(t, m, areas["Updates"].LabelID); by != "" {
		t.Errorf("the refused update still applied Updates (%q)", by)
	}
	if f2, _ := h.filingEvidence(t, a.UserID); f2 != filings {
		t.Errorf("filings %d -> %d", filings, f2)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range asArr(v) {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// RED PROBE: drop keyVisibleMessages from emailSet's update loop and the
// restricted key files the other account's message.
func TestRestrictedKeyAreaMoveKeepsToItsAccounts(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()
	areas := h.areasOf(t, a.UserID)
	acct2, mbox2 := store.NewULID(), store.NewULID()
	if _, err := h.super.Exec(ctx, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'second','mail.example.com','smtp.example.com',$3,$4)`,
		acct2.String(), a.UserID.String(), []byte{1}, store.NewULID().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		VALUES ($1,$2,$3,'INBOX','Inbox 2','\Inbox')`, mbox2.String(), a.UserID.String(), acct2.String()); err != nil {
		t.Fatal(err)
	}
	f2 := a
	f2.AccountID, f2.InboxID = acct2, mbox2
	theirs := agentMessage(t, h, f2)
	mine := agentMessage(t, h, a)

	one, _ := h.newKey(t, a.UserID, map[string]any{"name": "one", "scopes": []string{"mail:read", "mail:move"},
		"accountIds": []string{a.AccountID.String()}})
	if name, res := h.keyCall(t, one, "Area/get", map[string]any{"ids": nil}); name != "Area/get" || len(asArr(res["list"])) == 0 {
		t.Fatalf("a restricted key's Area/get: %s %v", name, res)
	}
	name, res := h.keyCall(t, one, "Email/set", map[string]any{"update": map[string]any{
		theirs.String(): refilePatch(areas, "Updates"), mine.String(): refilePatch(areas, "Updates")}})
	if name != "Email/set" || asObj(asObj(res["notUpdated"])[theirs.String()])["type"] != setNotFound {
		t.Fatalf("a restricted key filing another account's mail: %s %v, want notFound", name, res)
	}
	if by := h.labelBy(t, theirs, areas["Updates"].LabelID); by != "" {
		t.Errorf("the other account's message was filed anyway (%q)", by)
	}
	if by := h.labelBy(t, mine, areas["Updates"].LabelID); by != store.LabelByAgent {
		t.Errorf("control: its own account's message was not filed (%q)", by)
	}
	// Negative control: an unrestricted key files the same message.
	all, _ := h.newKey(t, a.UserID, map[string]any{"name": "all", "scopes": []string{"mail:read", "mail:move"}})
	if name, res := h.keyCall(t, all, "Email/set", map[string]any{"update": map[string]any{theirs.String(): refilePatch(areas, "Updates")}}); name != "Email/set" || !hasKey(asObj(res["updated"]), theirs.String()) {
		t.Fatalf("control: an unrestricted key's move: %s %v", name, res)
	}
}

// ---- suspension ---------------------------------------------------------------

// keyCalls makes several method calls in ONE request and returns each
// response's name and arguments.
func (h *agentHarness) keyCalls(t *testing.T, token string, calls ...[]any) (names []string, outs []map[string]any) {
	t.Helper()
	body := mustJSON(t, map[string]any{"using": allUsing, "methodCalls": calls})
	r := httptest.NewRequest(http.MethodPost, "/jmap", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.RemoteAddr = "192.0.2.44:4444"
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %q", w.Code, w.Body.String())
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, mr := range resp.MethodResponses {
		var n string
		var o map[string]any
		_ = json.Unmarshal(mr[0], &n)
		_ = json.Unmarshal(mr[1], &o)
		names, outs = append(names, n), append(outs, o)
	}
	return names, outs
}

func (h *agentHarness) suspendedReason(t *testing.T, keyID string) (bool, string) {
	t.Helper()
	var at *string
	var reason *string
	_ = h.super.QueryRow(context.Background(), `SELECT suspended_at::text, suspended_reason FROM app.api_keys WHERE id=$1`, keyID).Scan(&at, &reason)
	if reason == nil {
		return at != nil, ""
	}
	return at != nil, *reason
}

func (h *agentHarness) auditRows(t *testing.T, keyID, action string) []map[string]any {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT actor, detail FROM app.audit_log WHERE object_type='ApiKey' AND object_id=$1 AND action=$2 ORDER BY id`, keyID, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var actor string
		var detail map[string]any
		if err := rows.Scan(&actor, &detail); err != nil {
			t.Fatal(err)
		}
		detail["_actor"] = actor
		out = append(out, detail)
	}
	return out
}

// anomalySuspended reports the "suspended" flag of the key's anomaly rows
// for rule, in order.
func (h *agentHarness) anomalySuspended(t *testing.T, keyID, rule string) []bool {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT COALESCE((summary->>'suspended')::boolean, false) FROM app.api_key_events WHERE key_id=$1 AND anomaly=$2 ORDER BY id`, keyID, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []bool
	for rows.Next() {
		var b bool
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

// tripRule makes the key trip one anomaly rule, returning the refusal of the
// call that tripped it.
type tripRule struct {
	name, anomaly string
	key           func(a apiFixture) map[string]any
	trip          func(t *testing.T, h *agentHarness, a apiFixture, tok string) (string, map[string]any)
}

func anomalyRules() []tripRule {
	return []tripRule{
		{name: "mass delete", anomaly: store.AnomalyMassDelete,
			key: func(apiFixture) map[string]any {
				return map[string]any{"scopes": []string{"mail:read", "mail:delete"}, "confirmScopes": []string{}, "ratePerMinute": 600}
			},
			trip: func(t *testing.T, h *agentHarness, _ apiFixture, tok string) (string, map[string]any) {
				ids := make([]string, MassDeleteThreshold+1)
				for i := range ids {
					ids[i] = store.NewULID().String()
				}
				return h.keyCall(t, tok, "Email/set", map[string]any{"destroy": ids})
			}},
		{name: "mass send", anomaly: store.AnomalyMassSend,
			key: func(apiFixture) map[string]any {
				return map[string]any{"scopes": []string{"mail:read", "mail:send"}, "confirmScopes": []string{}, "ratePerMinute": 600}
			},
			trip: func(t *testing.T, h *agentHarness, _ apiFixture, tok string) (string, map[string]any) {
				sends := map[string]any{}
				for i := range MassSendThreshold + 1 {
					sends[fmt.Sprintf("s%d", i)] = map[string]any{"emailId": store.NewULID().String()}
				}
				return h.keyCall(t, tok, "EmailSubmission/set", map[string]any{"create": sends})
			}},
		{name: "critical refusals", anomaly: store.AnomalyCriticalRefusals,
			key: func(a apiFixture) map[string]any {
				return map[string]any{"scopes": []string{"mail:read"}, "ratePerMinute": 1,
					"accountIds": []string{a.AccountID.String()}}
			},
			trip: func(t *testing.T, h *agentHarness, _ apiFixture, tok string) (name string, res map[string]any) {
				// The cap is CriticalRefusalCapFactor x rate written in full; the
				// next one is past it.
				for range CriticalRefusalCapFactor*1 + 1 {
					name, res = h.keyCall(t, tok, "Email/changes", map[string]any{"sinceState": "s:0"})
				}
				return name, res
			}},
	}
}

// every anomaly rule suspends a key by default, audits it, says so in
// the alert and in the refusal, and the key is refused from then on.
//
// RED PROBES: make suspendForAnomaly return false and every subtest goes red
// on "not suspended"; drop the audit insert in store.SuspendAPIKey and the
// api_key.suspend check goes red; drop the suspended branch of anomalyAlert
// and the alert check goes red.
func TestEveryAnomalyRuleSuspendsByDefault(t *testing.T) {
	h := newAgentHarness(t)
	for _, r := range anomalyRules() {
		t.Run(r.name, func(t *testing.T) {
			a, _ := seedTwo(t, h.super)
			settings := r.key(a)
			settings["name"] = r.name // suspendOnAnomaly NOT given: the default must suspend
			tok, keyID := h.newKey(t, a.UserID, settings)
			name, res := r.trip(t, h, a, tok)
			wantForbidden(t, r.name, name, res)
			if r.anomaly != store.AnomalyCriticalRefusals && !strings.Contains(fmt.Sprint(res["description"]), "suspended") {
				t.Errorf("the refusal does not say the key was suspended: %v", res)
			}
			if s, reason := h.suspendedReason(t, keyID); !s || reason == "" {
				t.Fatalf("the key was not suspended (reason %q)", reason)
			}
			audits := h.auditRows(t, keyID, "api_key.suspend")
			if len(audits) != 1 || audits[0]["rule"] != r.anomaly || audits[0]["_actor"] != "system" {
				t.Errorf("api_key.suspend audit = %v, want one row by system for rule %s", audits, r.anomaly)
			}
			if got := h.anomalySuspended(t, keyID, r.anomaly); !slices.Equal(got, []bool{true}) {
				t.Errorf("anomaly rows' suspended flags = %v, want [true]", got)
			}
			h.alerts.waitFor(t, func(al fakeAlert) bool {
				return al.user == a.UserID && al.urgent && strings.Contains(al.title, "suspended") &&
					strings.Contains(al.body, "reactivate")
			})
			if code, _, _ := h.rawStatus(tok, false); code != http.StatusUnauthorized {
				t.Errorf("a suspended key still authenticates: %d", code)
			}
		})
	}
}

// With suspendOnAnomaly off, every rule only alerts. RED PROBE: drop the
// SuspendOnAnomaly check in suspendForAnomaly and these keys are suspended.
func TestAnomalyWithSuspendOffOnlyAlerts(t *testing.T) {
	h := newAgentHarness(t)
	for _, r := range anomalyRules() {
		t.Run(r.name, func(t *testing.T) {
			a, _ := seedTwo(t, h.super)
			settings := r.key(a)
			settings["name"], settings["suspendOnAnomaly"] = r.name, false
			tok, keyID := h.newKey(t, a.UserID, settings)
			name, res := r.trip(t, h, a, tok)
			wantForbidden(t, r.name, name, res)
			if s, _ := h.suspendedReason(t, keyID); s {
				t.Fatal("a key with suspendOnAnomaly off was suspended")
			}
			if got := h.anomalySuspended(t, keyID, r.anomaly); !slices.Equal(got, []bool{false}) {
				t.Errorf("anomaly rows' suspended flags = %v, want [false]", got)
			}
			h.alerts.waitFor(t, func(al fakeAlert) bool {
				return al.user == a.UserID && al.urgent && !strings.Contains(al.title, "suspended")
			})
			if code, _, _ := h.rawStatus(tok, false); code != http.StatusOK {
				t.Errorf("a key that was only alerted on no longer works: %d", code)
			}
		})
	}
}

// The calls after the one that suspended the key, in the same request, are
// refused as suspended — also a call the table would have refused for its own
// reason before admission, which never reads the key's row (Mailbox/set), and
// one that reaches admission, which does (Core/echo).
//
// RED PROBE: drop the p.suspended check at the top of runKeyCall and the
// Mailbox/set is refused as "not available to API keys" instead (Core/echo
// stays refused as suspended: admission reads the committed suspension).
func TestSuspensionRefusesTheRestOfTheRequest(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	tok, _ := h.newKey(t, a.UserID, map[string]any{"name": "batch", "scopes": []string{"mail:read", "mail:delete"},
		"confirmScopes": []string{}, "ratePerMinute": 600})
	ids := make([]string, MassDeleteThreshold+1)
	for i := range ids {
		ids[i] = store.NewULID().String()
	}
	names, outs := h.keyCalls(t, tok,
		[]any{"Email/set", map[string]any{"destroy": ids}, "0"},
		[]any{"Mailbox/set", map[string]any{"create": map[string]any{}}, "1"},
		[]any{"Core/echo", map[string]any{"hello": true}, "2"})
	if len(names) != 3 {
		t.Fatalf("responses: %v %v", names, outs)
	}
	for i := 1; i < 3; i++ {
		if names[i] != "error" || !strings.Contains(fmt.Sprint(outs[i]["description"]), "suspended") {
			t.Errorf("call %d after the suspending call: %s %v, want refused as suspended", i, names[i], outs[i])
		}
	}
}

// Reactivation is a session's ApiKey/set, audited; a key cannot do it; and a
// key that trips again is suspended, recorded and pushed again.
//
// RED PROBE: drop the api_key.reactivate audit in updateKey and the audit
// check goes red.
func TestReactivationIsSessionOnlyAndAudited(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	r := anomalyRules()[1] // mass send
	settings := r.key(a)
	settings["name"] = "sender"
	tok, keyID := h.newKey(t, a.UserID, settings)
	r.trip(t, h, a, tok)
	if s, _ := h.suspendedReason(t, keyID); !s {
		t.Fatal("fixture: the key was not suspended")
	}

	// Another key, even one with every scope, cannot lift it.
	other, _ := h.newKey(t, a.UserID, map[string]any{"name": "other", "scopes": []string{"mail:read"}})
	name, res := h.keyCall(t, other, "ApiKey/set", map[string]any{"update": map[string]any{keyID: map[string]any{"suspended": false}}})
	wantForbidden(t, "a key reactivating a key", name, res)
	if s, _ := h.suspendedReason(t, keyID); !s {
		t.Fatal("a key reactivated a key")
	}

	res = call(t, h.api.apiKeySet, a.UserID, map[string]any{"update": map[string]any{keyID: map[string]any{"suspended": false}}})
	if !hasKey(asObj(res["updated"]), keyID) {
		t.Fatalf("the session's reactivation: %v", res)
	}
	if code, _, _ := h.rawStatus(tok, false); code != http.StatusOK {
		t.Fatalf("the reactivated key does not authenticate: %d", code)
	}
	audits := h.auditRows(t, keyID, "api_key.reactivate")
	if len(audits) != 1 || audits[0]["_actor"] != "user" || !strings.Contains(fmt.Sprint(audits[0]["suspendedReason"]), "mass send") {
		t.Fatalf("api_key.reactivate audit = %v, want one row by the user naming the suspension it ended", audits)
	}
	// An ordinary update is not a reactivation.
	call(t, h.api.apiKeySet, a.UserID, map[string]any{"update": map[string]any{keyID: map[string]any{"name": "sender2"}}})
	if n := len(h.auditRows(t, keyID, "api_key.reactivate")); n != 1 {
		t.Errorf("%d reactivation rows after a rename, want 1", n)
	}

	// It trips again: suspended, recorded and pushed again (no cooldown).
	r.trip(t, h, a, tok)
	if s, _ := h.suspendedReason(t, keyID); !s {
		t.Fatal("the reactivated key was not suspended again")
	}
	if got := h.anomalySuspended(t, keyID, r.anomaly); !slices.Equal(got, []bool{true, true}) {
		t.Errorf("anomaly rows = %v, want two suspending rows (no cooldown for a suspending key)", got)
	}
	if n := h.countAlerts(func(al fakeAlert) bool { return al.user == a.UserID && strings.Contains(al.title, "suspended") }); n != 2 {
		t.Errorf("%d suspension alerts, want 2", n)
	}
}

// a new key suspends on anomalies unless it says otherwise.
func TestNewKeysSuspendOnAnomalyByDefault(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	_, keyID := h.newKey(t, a.UserID, map[string]any{"name": "plain", "scopes": []string{"mail:read"}})
	res := call(t, h.api.apiKeyGet, a.UserID, map[string]any{"ids": []string{keyID}})
	if l := asArr(res["list"]); len(l) != 1 || asObj(l[0])["suspendOnAnomaly"] != true {
		t.Fatalf("a new key's suspendOnAnomaly: %v, want true", res)
	}
}
